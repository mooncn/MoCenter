package session

import (
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