package probe

import (
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

func TestStoreLifecycle(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "UUID-1", Hostname: "h", Online: true})
	p := &protocol.ProbeDataReq{AgentID: "a1", UUID: "UUID-1", CPUUsage: 50, MemUsage: 30, Timestamp: time.Now().Unix()}
	s.UpdateProbe(p)

	a := s.Get("a1")
	if a.CPUUsage != 50 || a.MemUsage != 30 {
		t.Fatalf("probe not updated: cpu=%v mem=%v", a.CPUUsage, a.MemUsage)
	}
	if a.UUID != "UUID-1" {
		t.Fatalf("uuid not stored: %q", a.UUID)
	}
	if len(s.History("a1", time.Now().Add(-time.Hour))) != 1 {
		t.Fatal("history should contain 1 sample")
	}

	s.MarkOffline("a1")
	if s.Get("a1").Online {
		t.Fatal("a1 should be offline after MarkOffline")
	}
	if s.Get("a1").LastOfflineAt == 0 {
		t.Fatal("offline timestamp should be set")
	}
}

func TestStoreAll(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", Online: false})
	if len(s.All()) != 2 {
		t.Fatalf("All should return 2 agents, got %d", len(s.All()))
	}
}

// TestUUIDPreservedOnReconnect 验证重连未上报 UUID 时沿用历史值，
// 且探针上报可补齐 UUID。
func TestUUIDPreservedOnReconnect(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "HW-UUID", Online: true})
	// 重连时未携带 UUID
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true})
	if got := s.Get("a1").UUID; got != "HW-UUID" {
		t.Fatalf("重连后 UUID 应保留，实得 %q", got)
	}
	// 新主机通过探针补齐 UUID
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a2", UUID: "HW-UUID-2", Timestamp: time.Now().Unix()})
	if got := s.Get("a2").UUID; got != "HW-UUID-2" {
		t.Fatalf("探针应补齐 UUID，实得 %q", got)
	}
}