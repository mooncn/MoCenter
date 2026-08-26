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