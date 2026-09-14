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
// TestHistoryDownsampledMemory 验证内存回退路径下按 maxPoints 抽稀，
// 避免长时间跨度返回全量采样点。
func TestHistoryDownsampledMemory(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true})
	base := time.Now().Unix()
	for i := 0; i < 500; i++ {
		s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a1", Timestamp: base + int64(i), CPUUsage: float64(i)})
	}

	full := s.HistoryDownsampled("a1", time.Unix(base-10, 0), 500)
	if len(full) != 500 {
		t.Fatalf("expected 500 points when maxPoints>=total, got %d", len(full))
	}

	small := s.HistoryDownsampled("a1", time.Unix(base-10, 0), 50)
	if len(small) != 50 {
		t.Fatalf("expected 50 downsampled points, got %d", len(small))
	}
	// 抽稀结果应保持时间升序
	for i := 1; i < len(small); i++ {
		if small[i].Timestamp < small[i-1].Timestamp {
			t.Fatalf("downsampled history not time-ascending at %d", i)
		}
	}
	// 首点应接近起点
	if small[0].Timestamp < base {
		t.Fatalf("first point %d should be >= base %d", small[0].Timestamp, base)
	}

	// 空 agentID 返回空切片而非 nil panic
	if got := s.HistoryDownsampled("", time.Unix(base-10, 0), 50); len(got) != 0 {
		t.Fatalf("expected empty for empty agentID, got %d", len(got))
	}
}
