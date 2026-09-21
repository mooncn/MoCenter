package portmap

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// freePort 取一个当前空闲的 TCP 端口用于起真实监听。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestTrackAndCleanup(t *testing.T) {
	m := NewManager(nil, nil)
	m.idleTimeout = time.Second

	m.TrackConn(&Track{TunnelID: "t1", PublicPort: 8080, AgentID: "a1", LastActiveAt: time.Now().Add(-time.Hour)})
	m.TrackConn(&Track{TunnelID: "t2", PublicPort: 8081, AgentID: "a1", LastActiveAt: time.Now()})

	m.UpdateTraffic("t2", 100, 50)
	if tr := m.GetTrack("t2"); tr == nil || tr.BytesSent != 100 || tr.BytesReceived != 50 {
		t.Fatal("traffic stats not updated")
	}

	exp := m.CleanupExpired(10)
	if len(exp) != 1 || exp[0].TunnelID != "t1" {
		t.Fatalf("expected only t1 expired, got %d", len(exp))
	}
	if m.GetTrack("t1") != nil {
		t.Fatal("t1 should be removed")
	}
	if m.GetTrack("t2") == nil {
		t.Fatal("t2 active should remain")
	}
}

func TestRuleValidation(t *testing.T) {
	m := NewManager(nil, nil)
	err := m.CreateRule(&Rule{PublicPort: 0, LocalPort: 80, AgentID: "a1"})
	if err == nil {
		t.Fatal("invalid public port should error")
	}
	err = m.CreateRule(&Rule{PublicPort: 20000, LocalPort: 0, AgentID: "a1"})
	if err == nil {
		t.Fatal("invalid local port should error")
	}
}

func TestByAgentIndex(t *testing.T) {
	m := NewManager(nil, nil)
	m.TrackConn(&Track{TunnelID: "t1", AgentID: "a1"})
	m.TrackConn(&Track{TunnelID: "t2", AgentID: "a1"})
	m.TrackConn(&Track{TunnelID: "t3", AgentID: "a2"})

	m.CloseTrack("t1")
	// t1 关闭后，a1 的二级索引应移除 t1
	m.mu.RLock()
	_, ok := m.byAgent["a1"]["t1"]
	m.mu.RUnlock()
	if ok {
		t.Fatal("closed tunnel t1 should be removed from agent index")
	}
	if m.GetTrack("t2") == nil {
		t.Fatal("t2 should remain")
	}
}

// TestCreateRuleOverwriteTakesEffect 覆盖同端口规则必须真正换上新的监听与白名单。
//
// 回归用例（M-7）：旧实现先写 m.rules 再 net.Listen，端口还被自己的旧监听占着，
// 于是 Listen 必然失败、旧 accept 循环继续握着**旧** Rule 对象放行 —— 界面上
// 白名单已经改小，实际放进来的还是全网连接，属于"看着收紧了其实没生效"。
func TestCreateRuleOverwriteTakesEffect(t *testing.T) {
	m := NewManager(nil, nil)
	port := freePort(t)

	if err := m.CreateRule(&Rule{PublicPort: port, LocalPort: 80, AgentID: "a1", SourceIP: "0.0.0.0/0"}); err != nil {
		t.Fatalf("首次建规则失败: %v", err)
	}
	if err := m.CreateRule(&Rule{PublicPort: port, LocalPort: 81, AgentID: "a1", SourceIP: "127.0.0.1"}); err != nil {
		t.Fatalf("覆盖同端口规则不应失败: %v", err)
	}
	defer m.DeleteRule(port)

	if got := m.GetRule(port); got == nil || got.LocalPort != 81 || got.SourceIP != "127.0.0.1" {
		t.Fatalf("规则未按新值生效: %+v", got)
	}
	m.mu.RLock()
	ln := m.listeners[port]
	m.mu.RUnlock()
	if ln == nil {
		t.Fatal("覆盖后该端口应仍在监听")
	}
	// 新监听确实可接受连接（旧监听已关闭，否则会报地址占用）
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("新监听不可连接: %v", err)
	}
	_ = conn.Close()
}

// TestCreateRuleFailureKeepsPreviousRule 新的监听起不来时，配置不能已经被改掉。
//
// 否则 SQLite/界面上是"新规则"，在跑的是"旧转发"，两边永远对不上，
// 而运维只会相信自己看到的那一份。
func TestCreateRuleFailureKeepsPreviousRule(t *testing.T) {
	m := NewManager(nil, nil)
	// 65536 超出端口号范围，ServeRule 的 net.Listen 必然失败（跨平台一致）
	const badPort = 65536
	if err := m.CreateRule(&Rule{PublicPort: badPort, LocalPort: 80, AgentID: "a1"}); err == nil {
		t.Skipf("本平台竟能绑定端口 %d，无法构造监听失败", badPort)
	}
	if got := m.GetRule(badPort); got != nil {
		t.Fatalf("监听失败时不应提交规则，实得 %+v", got)
	}
	m.mu.RLock()
	ln := m.listeners[badPort]
	m.mu.RUnlock()
	if ln != nil {
		t.Fatal("监听失败时不应登记监听句柄")
	}

	// 已有规则在表时改坏了：旧规则必须还在（不能"改了个失败的配置，把在跑的也弄没"）
	prev := &Rule{PublicPort: badPort, LocalPort: 80, AgentID: "a1", Enabled: true}
	m.mu.Lock()
	m.rules[badPort] = prev
	m.mu.Unlock()
	if err := m.CreateRule(&Rule{PublicPort: badPort, LocalPort: 99, AgentID: "a2"}); err == nil {
		t.Fatal("监听失败应返回错误")
	}
	if got := m.GetRule(badPort); got != prev {
		t.Fatalf("失败的修改应保留原规则，实得 %+v", got)
	}
}

// TestTunnelDeliveryRequiresOwningAgent 隧道回传只认发起它的那台 Agent（M-6）。
//
// tunnel_id 由服务端生成，但报文里的 tunnel_id 是 Agent 可写字段：不校验归属时，
// 一台被拿下的主机可以把任意数据投进别的主机的公网转发流里。
func TestTunnelDeliveryRequiresOwningAgent(t *testing.T) {
	m := NewManager(nil, nil)
	pub, peer := net.Pipe()
	defer pub.Close()
	defer peer.Close()

	m.mu.Lock()
	m.pub["t-1"] = &publicTrack{pconn: pub, publicPort: 8080, agentID: "a1"}
	m.mu.Unlock()

	reads := make(chan []byte, 4)
	go func() {
		buf := make([]byte, 32)
		for {
			n, err := peer.Read(buf)
			if n > 0 {
				reads <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	// 越权回传：一个字节都不应落到公网连接上
	m.DeliverTunnelData("t-1", "evil", []byte("injected"))
	// 归属正确：数据照常转发
	m.DeliverTunnelData("t-1", "a1", []byte("ok"))

	select {
	case got := <-reads:
		if string(got) != "ok" {
			t.Fatalf("公网连接实收 %q，应为 %q", got, "ok")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("归属正确的回传没有转发到公网连接")
	}
	select {
	case got := <-reads:
		t.Fatalf("越权回传不应写出字节，实收 %q", got)
	default:
	}

	m.mu.RLock()
	_, alive := m.pub["t-1"]
	m.mu.RUnlock()
	if !alive {
		t.Fatal("越权回传不应顺手关掉别人的公网连接")
	}
}

// TestNotifyTunnelClosedRequiresOwningAgent 关闭通知同样要认归属。
// 否则猜到隧道 ID 就能掐断别人的转发。
func TestNotifyTunnelClosedRequiresOwningAgent(t *testing.T) {
	m := NewManager(nil, nil)
	pub, peer := net.Pipe()
	defer peer.Close()
	m.mu.Lock()
	m.pub["t-1"] = &publicTrack{pconn: pub, publicPort: 8080, agentID: "a1"}
	m.mu.Unlock()

	m.NotifyTunnelClosed("t-1", "evil", "bye")
	m.mu.RLock()
	_, alive := m.pub["t-1"]
	m.mu.RUnlock()
	if !alive {
		t.Fatal("别台主机关闭通知不应生效")
	}

	m.NotifyTunnelClosed("t-1", "a1", "bye")
	m.mu.RLock()
	_, alive = m.pub["t-1"]
	m.mu.RUnlock()
	if alive {
		t.Fatal("归属正确的关闭通知应移除隧道")
	}
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("关闭后对端应已断开")
	}
}