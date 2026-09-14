package portmap

import (
	"testing"
	"time"
)

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