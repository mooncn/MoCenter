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

// TestAgentIDChangeKeepsSingleDevice 回归：同一台机器改了 agent_id（配置文件变更）
// 不应产生第二条记录，历史采样与 UUID 都要保留。
func TestAgentIDChangeKeepsSingleDevice(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "old-id", UUID: "HW-UUID-1", Hostname: "h1", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "old-id", UUID: "HW-UUID-1", Timestamp: time.Now().Unix(), CPUUsage: 10})

	// 同一台机器换了 agent_id 重新上线
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "new-id", UUID: "HW-UUID-1", Hostname: "h1", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "new-id", UUID: "HW-UUID-1", Timestamp: time.Now().Unix() + 1, CPUUsage: 20})

	if got := len(s.All()); got != 1 {
		t.Fatalf("同一 UUID 应只有一条记录，实得 %d 条", got)
	}
	a := s.Get("new-id")
	if a == nil {
		t.Fatal("按新 agent_id 应能查到该设备")
	}
	if a.UUID != "HW-UUID-1" {
		t.Fatalf("UUID 应保留，实得 %q", a.UUID)
	}
	if a.CPUUsage != 20 {
		t.Fatalf("指标应更新为最新值，实得 %v", a.CPUUsage)
	}
	// 旧 agent_id 仍作为别名可查（前端/审计不至于查不到）
	if s.Get("old-id") == nil {
		t.Fatal("旧 agent_id 应仍能解析到同一台设备")
	}
	// 历史不应因改名而丢弃
	if h := len(s.History("new-id", time.Unix(0, 0))); h != 2 {
		t.Fatalf("改名后历史应保留 2 条，实得 %d", h)
	}
}

// TestUUIDCaseInsensitive 回归：UUID 大小写/空白差异不应被判成两台机器。
func TestUUIDCaseInsensitive(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "abcd-ef01", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: " ABCD-EF01\n", Online: true})
	if got := len(s.All()); got != 1 {
		t.Fatalf("UUID 归一化后应只有一条记录，实得 %d", got)
	}
	if got := s.Get("a1").UUID; got != "ABCD-EF01" {
		t.Fatalf("UUID 应统一为大写无空白，实得 %q", got)
	}
}

// TestDifferentUUIDKeepsSeparateDevices 反向验证：UUID 不同仍是两台机器。
func TestDifferentUUIDKeepsSeparateDevices(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "UUID-A", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", UUID: "UUID-B", Online: true})
	if got := len(s.All()); got != 2 {
		t.Fatalf("不同 UUID 应为两台机器，实得 %d", got)
	}
}

// TestProbeUUIDBackfillAndRename 验证旧版 Agent 先以 agent_id 上报、随后补齐 UUID
// 时，设备记录被迁移到 UUID 主键下而不是新增一条。
func TestProbeUUIDBackfillAndRename(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a1", Timestamp: time.Now().Unix(), CPUUsage: 5})
	// 同一台机器改名并补齐 UUID 后重新注册（真实时序：注册先于探针）
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", UUID: "UUID-X", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a2", UUID: "UUID-X", Timestamp: time.Now().Unix() + 1, CPUUsage: 6})
	if got := len(s.All()); got != 1 {
		t.Fatalf("补齐 UUID 后应仍只有一条记录，实得 %d", got)
	}
	if s.GetByUUID("UUID-X") == nil {
		t.Fatal("应能按 UUID 查到设备")
	}
}

// TestMarkOfflineByOldAlias 验证按旧 agent_id 标记离线会落到同一台设备上。
func TestMarkOfflineByOldAlias(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "old", UUID: "UUID-Z", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "new", UUID: "UUID-Z", Online: true})
	s.MarkOffline("old")
	if s.Get("new").Online {
		t.Fatal("按旧 agent_id 标离线，同一设备也应变离线")
	}
}
