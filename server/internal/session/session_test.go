package session

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestUpsertAndGet(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	conn := NewConn(c1)
	conn.AgentID = "a1"
	m.Upsert("a1", conn)

	if m.Get("a1") == nil {
		t.Fatal("Get should return connection")
	}
	if len(m.All()) != 1 {
		t.Fatal("All should contain 1 conn")
	}
}

func TestTickOffline(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	conn := NewConn(c1)
	conn.AgentID = "a1"
	conn.LastSeen = time.Now().Add(-time.Minute)
	m.Upsert("a1", conn)

	off := m.Tick(30 * time.Second)
	found := false
	for _, id := range off {
		if id == "a1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a1 to be marked offline")
	}
	if m.Get("a1") != nil {
		t.Fatal("offline agent should be removed")
	}
}

func TestRemove(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	conn := NewConn(c1)
	conn.AgentID = "a1"
	m.Upsert("a1", conn)
	m.Remove("a1")
	if m.Get("a1") != nil {
		t.Fatal("agent should be removed")
	}
}

// TestUpsertRejectsHijack 安全回归：同名 AgentID 的活跃连接不得被顶替。
// 旧实现会 Close 旧连接并替换 —— 攻击者只要知道一个 AgentID 就能抢走该主机的身份。
func TestUpsertRejectsHijack(t *testing.T) {
	m := NewManager()
	real1, _ := net.Pipe()
	realConn := NewConn(real1)
	realConn.AgentID = "victim"
	realConn.RemoteAddr = "10.0.0.1:1111"
	realConn.LastSeen = time.Now() // 活跃
	m.Upsert("victim", realConn)

	fake1, _ := net.Pipe()
	fake := NewConn(fake1)
	fake.AgentID = "victim"
	fake.RemoteAddr = "6.6.6.6:6666"

	old, accepted := m.Upsert("victim", fake)
	if accepted {
		t.Fatal("活跃连接存在时必须拒绝新的同名注册（否则可被冒名顶替）")
	}
	if old != realConn {
		t.Fatal("返回的应是原连接")
	}
	if m.Get("victim") != realConn {
		t.Fatal("原连接必须仍是该 AgentID 的会话")
	}
	if m.Get("victim") == fake {
		t.Fatal("伪造连接绝不能成为会话")
	}
}

// TestUpsertReplacesStale 旧连接超时未心跳时允许替换（避免重启的 Agent 被僵尸连接挡住）。
func TestUpsertReplacesStale(t *testing.T) {
	m := NewManager()
	m.SetStaleAfter(50 * time.Millisecond)

	c1, _ := net.Pipe()
	old := NewConn(c1)
	old.AgentID = "a1"
	old.LastSeen = time.Now().Add(-time.Second) // 早已超时
	m.Upsert("a1", old)

	c2, _ := net.Pipe()
	fresh := NewConn(c2)
	fresh.AgentID = "a1"
	got, accepted := m.Upsert("a1", fresh)
	if !accepted {
		t.Fatal("旧连接已超时，应允许替换")
	}
	if got != old {
		t.Fatal("应返回被替换的旧连接")
	}
	if m.Get("a1") != fresh {
		t.Fatal("新连接应成为会话")
	}
}

// TestUpsertSameConnIdempotent 同一个连接重复注册应放行（心跳/重复上报场景）。
func TestUpsertSameConnIdempotent(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	conn := NewConn(c1)
	conn.AgentID = "a1"
	m.Upsert("a1", conn)
	if _, accepted := m.Upsert("a1", conn); !accepted {
		t.Fatal("同一连接重复注册不应被拒绝")
	}
}

// TestSetStaleAfterInvalid 非法阈值回落到默认值。
func TestSetStaleAfterInvalid(t *testing.T) {
	m := NewManager()
	m.SetStaleAfter(0)
	m.mu.RLock()
	got := m.staleAfter
	m.mu.RUnlock()
	if got != DefaultStaleAfter {
		t.Fatalf("阈值应为默认值 %v，实际 %v", DefaultStaleAfter, got)
	}
}

// stubConn 只实现本包测试用到的几个方法：记录是否设过写超时、是否被关闭。
// 未实现的方法走内嵌的 nil net.Conn，一旦被调用即 panic —— 这正好把
// "写路径还依赖别的什么" 暴露出来。
type stubConn struct {
	net.Conn
	writeErr    error
	hadDeadline bool
	closed      bool
}

func (s *stubConn) SetWriteDeadline(time.Time) error { s.hadDeadline = true; return nil }
func (s *stubConn) Close() error                     { s.closed = true; return nil }
func (s *stubConn) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if !s.hadDeadline {
		return 0, errNoWriteDeadline
	}
	return len(p), nil
}

var errNoWriteDeadline = errors.New("写之前没有设写超时")

// TestWriteSetsDeadlineThenDropsDeadConn 向 Agent 的每条写都要有超时，且写失败即断开。
//
// 回归用例：没有写超时的时候，一条"只收不发"的连接会让 Write 永久持有 c.mu ——
// 调用方既有 HTTP 处理协程也有端口映射的转发泵，最坏情况整条隧道连同该主机的
// 后续下发一起挂死。写失败必须关连接：一帧 JSON 可能只写出去一半，留在流里的
// 残帧会让 Agent 的按行解析从此一直错位。
func TestWriteSetsDeadlineThenDropsDeadConn(t *testing.T) {
	live := &stubConn{}
	if err := NewConn(live).Write("test", map[string]string{"ok": "1"}); err != nil {
		t.Fatalf("正常写入不应失败: %v", err)
	}
	if !live.hadDeadline {
		t.Fatal("Write 必须设置写超时")
	}

	dead := &stubConn{writeErr: io.ErrClosedPipe}
	if err := NewConn(dead).Write("test", map[string]string{"ok": "1"}); err == nil {
		t.Fatal("写失败应把错误返回给调用方")
	}
	if !dead.closed {
		t.Fatal("写失败后必须关闭连接，否则残帧会污染后续所有报文")
	}
}

// TestWriteOnMissingConn 没有底层连接时报错，不能 panic（调用方要据此走下线流程）。
func TestWriteOnMissingConn(t *testing.T) {
	c := NewConn(nil)
	c.SetAgentID("a1")
	if err := c.Write("test", nil); err == nil {
		t.Fatal("NetConn 为空时写入应返回错误")
	}
}