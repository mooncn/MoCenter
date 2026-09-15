package session

import (
	"net"
	"testing"
	"time"
)

// TestReplaceByUUID 回归：同一台机器（同 UUID）改了 agent_id 重连时，
// 旧连接必须被顶替，会话池里只留一条 —— 否则前端会看到两台机器。
func TestReplaceByUUID(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	old := NewConn(c1)
	old.AgentID = "old-id"
	old.UUID = "HW-UUID"
	m.Upsert("old-id", old)

	c2, _ := net.Pipe()
	fresh := NewConn(c2)
	fresh.AgentID = "new-id"
	fresh.UUID = "HW-UUID"

	replaced := m.ReplaceByUUID("HW-UUID", fresh)
	if len(replaced) != 1 || replaced[0] != "old-id" {
		t.Fatalf("应顶替旧连接 old-id，实得 %v", replaced)
	}
	m.Upsert("new-id", fresh)
	if got := len(m.All()); got != 1 {
		t.Fatalf("同 UUID 应只保留一条会话，实得 %d", got)
	}
	if m.Get("new-id") == nil {
		t.Fatal("新会话应存在")
	}
}

// TestReplaceByUUIDKeepsOtherDevices 反向验证：UUID 不同不得互相顶替。
func TestReplaceByUUIDKeepsOtherDevices(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	a := NewConn(c1)
	a.AgentID = "a"
	a.UUID = "UUID-A"
	m.Upsert("a", a)

	c2, _ := net.Pipe()
	b := NewConn(c2)
	b.AgentID = "b"
	b.UUID = "UUID-B"

	if replaced := m.ReplaceByUUID("UUID-B", b); len(replaced) != 0 {
		t.Fatalf("不同 UUID 不应互相顶替，实得 %v", replaced)
	}
	m.Upsert("b", b)
	if got := len(m.All()); got != 2 {
		t.Fatalf("应保留两台机器，实得 %d", got)
	}
}

// TestRemoveIfCurrent 回归：旧连接收尾时不得误删新连接。
func TestRemoveIfCurrent(t *testing.T) {
	m := NewManager()
	c1, _ := net.Pipe()
	old := NewConn(c1)
	old.AgentID = "same"
	old.UUID = "U"
	m.Upsert("same", old)

	c2, _ := net.Pipe()
	fresh := NewConn(c2)
	fresh.AgentID = "same"
	fresh.UUID = "U"
	// 模拟真实时序：ReplaceByUUID 顶掉旧连接，随后新连接注册占用该 agent_id
	m.ReplaceByUUID("U", fresh)
	if _, ok := m.Upsert("same", fresh); !ok {
		t.Fatal("旧连接已被顶替，新连接应能注册成功")
	}

	if m.RemoveIfCurrent("same", old) {
		t.Fatal("旧连接不应删除新连接")
	}
	if m.Get("same") != fresh {
		t.Fatal("新连接应仍在会话池中")
	}
	if !m.RemoveIfCurrent("same", fresh) {
		t.Fatal("当前连接应可被移除")
	}
	if m.Get("same") != nil {
		t.Fatal("移除后不应存在于会话池")
	}
	_ = time.Now()
}

// TestConnAccessorsAreConcurrencySafe 回归：LastSeen / UUID / AgentID 会被
// 读循环、心跳清扫、离线判定从不同 goroutine 并发读写，必须全部经由带锁的
// 访问器，否则构成数据竞争（race 下可能读到半写的 time.Time 而误判离线）。
func TestConnAccessorsAreConcurrencySafe(t *testing.T) {
	c := NewConn(nil)
	c.SetAgentID("a1")
	c.SetUUID("UUID-1")
	c.SetHeartbeatInterval(30)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			c.SetLastSeen(time.Now())
			c.SetHeartbeatInterval(30 + i%5)
			c.SetOnline(i%2 == 0)
		}
	}()
	for i := 0; i < 500; i++ {
		_ = c.GetLastSeen()
		_ = c.GetUUID()
		_ = c.GetAgentID()
		_ = c.IsOnline()
		_ = c.heartbeatTimeout()
	}
	<-done

	if got := c.GetAgentID(); got != "a1" {
		t.Fatalf("agent_id 应可读，实得 %q", got)
	}
	if got := c.GetUUID(); got != "UUID-1" {
		t.Fatalf("uuid 应可读，实得 %q", got)
	}
}

// TestTickUsesLastSeenAccessor Tick 与写连接并发时不应触发竞争，
// 且仍按心跳超时正确判定离线。
func TestTickUsesLastSeenAccessor(t *testing.T) {
	m := NewManager()
	m.SetStaleAfter(time.Millisecond)
	c := NewConn(nil)
	c.SetAgentID("a1")
	c.SetLastSeen(time.Now().Add(-time.Hour))
	m.conns["a1"] = c

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			c.SetLastSeen(time.Now().Add(-time.Hour))
		}
	}()
	offline := m.Tick(time.Millisecond)
	<-done

	if len(offline) != 1 || offline[0] != "a1" {
		t.Fatalf("超时连接应被判离线，实得 %v", offline)
	}
}
