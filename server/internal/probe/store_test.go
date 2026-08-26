package probe

import (
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

func TestStoreLifecycle(t *testing.T) {
	s := NewStore()
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Hostname: "h", Online: true})
	p := &protocol.ProbeDataReq{AgentID: "a1", CPUUsage: 50, MemUsage: 30, Timestamp: time.Now().Unix()}
	s.UpdateProbe(p)

	a := s.Get("a1")
	if a.CPUUsage != 50 || a.MemUsage != 30 {
		t.Fatalf("probe not updated: cpu=%v mem=%v", a.CPUUsage, a.MemUsage)
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
	s := NewStore()
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", Online: false})
	if len(s.All()) != 2 {
		t.Fatalf("All should return 2 agents, got %d", len(s.All()))
	}
}