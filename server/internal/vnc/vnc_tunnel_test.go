package vnc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/session"
)

// newRelayViewer 造一条"浏览器侧"WebSocket，服务端 handler 把收到的每一帧
// 转进 returned channel —— 这样测试能分辨"帧真的写进了查看器"还是被丢弃。
func newRelayViewer(t *testing.T) (*websocket.Conn, chan []byte) {
	t.Helper()

	frames := make(chan []byte, 8)
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		close(ready)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			frames <- data
		}
	}))

	client, _, err := websocket.DefaultDialer.DialContext(context.Background(),
		"ws"+srv.URL[len("http"):]+"/vnc-ws", nil)
	if err != nil {
		srv.Close()
		t.Fatalf("拨号测试 WebSocket 失败: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		_ = client.Close()
		srv.Close()
		t.Fatal("测试服务端未完成升级")
	}
	t.Cleanup(func() {
		_ = client.Close()
		srv.Close()
	})
	return client, frames
}

// addTestViewer 在隧道表里登记一条属于 ownerAgent 的查看器隧道。
func addTestViewer(t *testing.T, m *Mgr, conn *websocket.Conn, ownerAgent, sess string) string {
	t.Helper()
	if !m.acquireViewer(sess) {
		t.Fatalf("为测试会话 %s 取名额失败", sess)
	}
	tunnelID := m.nextTunnel("v")
	m.mu.Lock()
	m.viewer[tunnelID] = &viewerConn{ws: conn, agentID: ownerAgent, session: sess, tunnelID: tunnelID}
	m.mu.Unlock()
	t.Cleanup(func() { m.closeViewer(tunnelID) })
	return tunnelID
}

// viewerAlive 报告隧道是否仍在表里。
func viewerAlive(m *Mgr, tunnelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewer[tunnelID] != nil
}

// TestTunnelDataRequiresOwningAgent 只有发起隧道的 Agent 能往桌面里写帧。
//
// 回归用例（M-6）：隧道 ID 由服务端生成，但报文里的 tunnel_id 是 Agent 可写字段。
// 不比对连接身份时，一台被拿下的主机可以把字节投进别的主机的 RFB 流 ——
// 表现为在别人的远程桌面里"凭空出现键鼠"。
func TestTunnelDataRequiresOwningAgent(t *testing.T) {
	m := NewMgr(session.NewManager())
	conn, frames := newRelayViewer(t)
	tunnelID := addTestViewer(t, m, conn, "a1", "sess-A")

	m.DeliverTunnelData(tunnelID, "evil", []byte("injected"))
	select {
	case got := <-frames:
		t.Fatalf("越权回传不应写出帧，实收 %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if !viewerAlive(m, tunnelID) {
		t.Fatal("越权回传不应关掉别人的桌面")
	}

	m.DeliverTunnelData(tunnelID, "a1", []byte("frame"))
	select {
	case got := <-frames:
		if string(got) != "frame" {
			t.Fatalf("归属正确的帧应原样送达，实收 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("归属正确的帧没有送达查看器")
	}
}

// TestOpenRespRequiresOwningAgent 隧道建立响应同样要认发起方。
//
// 不校验的话，任意一条连接都能对别人的隧道回"建立失败"，直接把别人的桌面掐断；
// 或回"就绪"把浏览器缓冲的键鼠数据导流到自己那边。
func TestOpenRespRequiresOwningAgent(t *testing.T) {
	m := NewMgr(session.NewManager())
	conn, frames := newRelayViewer(t)
	tunnelID := addTestViewer(t, m, conn, "a1", "sess-A")

	m.DeliverOpenResp(tunnelID, "evil", false, "目标不可达")
	select {
	case got := <-frames:
		t.Fatalf("越权响应不应向查看器播报错误，实收 %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if !viewerAlive(m, tunnelID) {
		t.Fatal("越权的失败响应不应关闭别人的桌面")
	}

	m.DeliverOpenResp(tunnelID, "a1", false, "目标不可达")
	if viewerAlive(m, tunnelID) {
		t.Fatal("归属正确的失败响应应关闭该桌面")
	}
}

// TestProbeRespRequiresOwningAgent 探测结果只接受被探测那台主机的回执。
// 否则任一台在线主机都能替别家伪造"VNC 可连/不可连"，把面板与后续动作带偏。
func TestProbeRespRequiresOwningAgent(t *testing.T) {
	m := NewMgr(session.NewManager())
	ch := make(chan probeResult, 2)
	m.mu.Lock()
	m.probeWait["vp-test"] = &probeWaiter{agentID: "a1", ch: ch}
	m.mu.Unlock()

	m.DeliverOpenResp("vp-test", "evil", true, "ok")
	select {
	case r := <-ch:
		t.Fatalf("越权探测回执不应送达，实得 %+v", r)
	case <-time.After(200 * time.Millisecond):
	}

	m.DeliverOpenResp("vp-test", "a1", true, "ok")
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatalf("归属正确的探测回执应被采纳，实得 %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("归属正确的探测回执没有送达")
	}
}

// TestUnknownTunnelIgnored 没人发起过的隧道 ID 一律忽略，且不影响同名在册隧道。
func TestUnknownTunnelIgnored(t *testing.T) {
	m := NewMgr(session.NewManager())
	m.DeliverTunnelData("v-nope", "a1", []byte("x"))
	m.DeliverOpenResp("v-nope", "a1", true, "")
	m.NotifyTunnelClosed("v-nope", "a1")
}

// TestNotifyTunnelClosedRequiresOwningAgent 关闭通知要认归属：猜到 ID 就能掐断别人的桌面。
func TestNotifyTunnelClosedRequiresOwningAgent(t *testing.T) {
	m := NewMgr(session.NewManager())
	conn, _ := newRelayViewer(t)
	tunnelID := addTestViewer(t, m, conn, "a1", "sess-A")

	m.NotifyTunnelClosed(tunnelID, "evil")
	if !viewerAlive(m, tunnelID) {
		t.Fatal("别台主机的关闭通知不应生效")
	}
	m.NotifyTunnelClosed(tunnelID, "a1")
	if viewerAlive(m, tunnelID) {
		t.Fatal("归属正确的关闭通知应移除隧道")
	}
}
