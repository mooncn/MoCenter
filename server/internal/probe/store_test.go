package probe

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mocenter/server/internal/geo"
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
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Hostname: "host-a", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a1", Timestamp: time.Now().Unix(), CPUUsage: 5})
	// 同一台机器改名为 a2 并补齐 UUID 后重新注册（真实时序：注册先于探针）。
	//
	// 改名同时补齐 UUID 时只能靠**主机名**识别旧记录，因此这里模拟真实环境
	// （主机名一致）—— 主机名与 agent_id 都对不上时不做认领，见
	// TestNoBlindHostnameAdoption：宁可多留一条旧记录，也不能把别的机器合并进来。
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a2", UUID: "UUID-X", Hostname: "host-a", Online: true})
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

// TestProbeCannotRewriteDeviceUUID 安全回归：探针报文里的 uuid 不可信。
//
// 报文里的 agent_id 已由连接身份校验，但 uuid 仍是 Agent 自报字段。
// 若据此把设备迁移到别的 UUID，已鉴权的 Agent 就能把指标写进他人设备记录、
// 或凭空造出一台"新设备"（非预期资产）。这里断言自报值只用于**首次补齐**。
func TestProbeCannotRewriteDeviceUUID(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "UUID-OWN", Hostname: "h1", Online: true})

	// 连接到 a1，却申报了别人的 UUID
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "a1", UUID: "UUID-VICTIM", Timestamp: 1, CPUUsage: 42})

	if got := len(s.All()); got != 1 {
		t.Fatalf("自报 UUID 不应造出新设备，实得 %d 台", got)
	}
	if a := s.GetByUUID("UUID-VICTIM"); a != nil {
		t.Fatalf("不应存在 UUID-VICTIM 的设备，实得 %+v", a)
	}
	if a := s.Get("a1"); a == nil || a.UUID != "UUID-OWN" {
		t.Fatalf("设备 UUID 不应被报文改写，实得 %+v", a)
	}
	if a := s.Get("a1"); a.CPUUsage != 42 {
		t.Fatalf("指标仍应按连接身份落到自己的设备，实得 %v", a.CPUUsage)
	}
}

// TestProbeBackfillsUUIDOnlyWhenAbsent 旧版 Agent：设备还没有 UUID 时才用自报值补齐。
func TestProbeBackfillsUUIDOnlyWhenAbsent(t *testing.T) {
	s := NewStore(nil)
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "legacy", UUID: "UUID-L", Timestamp: 1, CPUUsage: 1})
	if len(s.All()) != 1 {
		t.Fatalf("应只有一台设备，实得 %d", len(s.All()))
	}
	if a := s.Get("legacy"); a == nil || a.UUID != "UUID-L" {
		t.Fatalf("无 UUID 设备应被补齐，实得 %+v", a)
	}
}

// TestResolveUUIDIsFirstBound 注册路径的设备身份同样按 TOFU 裁决（M-4）。
//
// 探针侧早已"只在缺失时补齐"，注册侧却照单全收自报 uuid —— 于是任一条通过
// 注册鉴权的连接都能：① 用别人的 UUID 顶掉那台机器的在线会话，② 把别人的
// 设备记录（历史、备注）并进自己上报的信息里。这里断言绑定只发生一次。
func TestResolveUUIDIsFirstBound(t *testing.T) {
	s := NewStore(nil)

	// 首次上报：采纳并钉死
	trusted, conflict := s.ResolveUUID("a1", "uuid-own")
	if conflict || trusted != "UUID-OWN" {
		t.Fatalf("首次声明应被采纳（归一化大写），实得 trusted=%q conflict=%v", trusted, conflict)
	}
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: trusted, Hostname: "h1", Online: true})

	// 之后申报别人的 UUID：作废
	if trusted, conflict = s.ResolveUUID("a1", "UUID-VICTIM"); !conflict || trusted != "UUID-OWN" {
		t.Fatalf("冲突声明应被拒并回落到已绑定值，实得 trusted=%q conflict=%v", trusted, conflict)
	}
	// 大小写/空白差异仍是同一台机器，不该报冲突
	if trusted, conflict = s.ResolveUUID("a1", " uuid-own\n"); conflict || trusted != "UUID-OWN" {
		t.Fatalf("归一化后相同的声明不应判为冲突，实得 trusted=%q conflict=%v", trusted, conflict)
	}
	// 没带 UUID 的旧版 Agent：沿用绑定值
	if trusted, conflict = s.ResolveUUID("a1", ""); conflict || trusted != "UUID-OWN" {
		t.Fatalf("未上报 UUID 应沿用已绑定值，实得 trusted=%q conflict=%v", trusted, conflict)
	}
	// 未知 agent_id 首次上线：采纳
	if trusted, conflict = s.ResolveUUID("a2", "UUID-B"); conflict || trusted != "UUID-B" {
		t.Fatalf("新 agent_id 的首次声明应被采纳，实得 trusted=%q conflict=%v", trusted, conflict)
	}
}

// TestNoBlindHostnameAdoption 回归：不能因"全库只有一条无 UUID 记录"就把它
// 认领给任意首次上报 UUID 的机器 —— 多机环境会因此合并不同主机的历史/备注/端口映射。
func TestNoBlindHostnameAdoption(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "legacy-1", Hostname: "host-legacy", Online: true})

	// 另一台机器（主机名不同）首次上报 UUID
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "other", UUID: "UUID-OTHER", Hostname: "host-other", Online: true})

	if got := len(s.All()); got != 2 {
		t.Fatalf("主机名不同不应被误合并，实得 %d 台", got)
	}
	if a := s.Get("legacy-1"); a == nil || a.UUID != "" {
		t.Fatalf("旧记录不应被改写，实得 %+v", a)
	}
}

// TestAdoptByAgentIDWhenUUIDAppears 主机名缺失时，按 agent_id 认领同一条旧记录。
func TestAdoptByAgentIDWhenUUIDAppears(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "n1", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "n1", Timestamp: 1, CPUUsage: 7})

	// 同一 agent_id 补上 UUID 重新注册
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "n1", UUID: "UUID-N1", Online: true})

	if got := len(s.All()); got != 1 {
		t.Fatalf("同 agent_id 补齐 UUID 后应只有一条记录，实得 %d", got)
	}
	if a := s.Get("n1"); a == nil || a.UUID != "UUID-N1" {
		t.Fatalf("应迁移到 UUID 主键并保留指标，实得 %+v", a)
	}
	if h := len(s.History("n1", time.Unix(0, 0))); h != 1 {
		t.Fatalf("历史不应因迁移丢失，实得 %d 条", h)
	}
}

// TestRenameAbortsWhenRemarkMigrationFails 回归：备注迁移失败时不应改名。
//
// 备注按 agent_id 存储。若先改名再迁备注，迁移失败就会留下"新 ID 有记录、
// 备注仍在旧 ID"的状态 —— 用户看到的正是备注莫名消失，且无人察觉。
func TestRenameAbortsWhenRemarkMigrationFails(t *testing.T) {
	s := NewStore(nil)
	s.SetRemarkRenameFn(func(oldID, newID string) error {
		return errors.New("disk full")
	})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "old", UUID: "UUID-R", Hostname: "h", Online: true})
	s.UpdateProbe(&protocol.ProbeDataReq{AgentID: "old", Timestamp: 1, CPUUsage: 5})

	// 同一设备改名为 new：备注迁移失败，改名应被放弃
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "new", UUID: "UUID-R", Hostname: "h", Online: true})

	if got := len(s.All()); got != 1 {
		t.Fatalf("失败时仍应只有一台设备，实得 %d", got)
	}
	if a := s.Get("old"); a == nil || a.AgentID != "old" {
		t.Fatalf("改名失败后应保持旧 agent_id 可用，实得 %+v", a)
	}
	if a := s.Get("new"); a != nil && a.AgentID == "new" {
		t.Fatalf("改名失败后不应把记录改成 new，实得 %+v", a)
	}
	// 设备指标与历史不能丢
	if a := s.Get("old"); a == nil || a.CPUUsage != 5 {
		t.Fatalf("指标不应丢失，实得 %+v", a)
	}
	if h := len(s.History("old", time.Unix(0, 0))); h != 1 {
		t.Fatalf("历史不应丢失，实得 %d 条", h)
	}
}

// TestMarkOfflineIsIdempotent 回归：重复标离线不应刷新离线时刻。
//
// 连接超时清扫（Tick）与断开回调会先后调用 MarkOffline。若每次都刷新
// LastOfflineAt，"离线时长"会不断从 0 重算，历史离线时间线被抹掉。
func TestMarkOfflineIsIdempotent(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", UUID: "UUID-I", Online: true})

	s.MarkOffline("a1")
	first := s.Get("a1").LastOfflineAt
	time.Sleep(1100 * time.Millisecond)
	s.MarkOffline("a1")

	if got := s.Get("a1").LastOfflineAt; got != first {
		t.Fatalf("重复标离线不应改写离线时刻（首次 %d，再次 %d）", first, got)
	}
	if s.Get("a1").Online {
		t.Fatal("设备应处于离线状态")
	}
}

// TestMarkOfflineByAliasUsesCurrentAgentID 回归：按旧别名标离线时，
// 设备记录的 agent_id 应保持为当前值（落库要用它，否则离线状态写不进库）。
func TestMarkOfflineByAliasUsesCurrentAgentID(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "old", UUID: "UUID-M", Online: true})
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "new", UUID: "UUID-M", Online: true})

	s.MarkOffline("old")
	a := s.Get("new")
	if a == nil {
		t.Fatal("改名后应能按新 agent_id 查到设备")
	}
	if a.AgentID != "new" {
		t.Fatalf("设备记录应保留当前 agent_id，实得 %q", a.AgentID)
	}
	if a.Online {
		t.Fatal("按旧别名标离线，同一设备应变离线")
	}
}

// TestProbeNetDeltaAndRates 验证增量与速率的派生规则：
// 首点未知、正常点按差值/时间、计数器回绕视为未知（而不是画出一个负值）。
func TestProbeNetDeltaAndRates(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "n1", UUID: "UUID-N1", Online: true})
	base := time.Now().Unix()

	// 首点：没有前驱，增量未知
	first := &protocol.ProbeDataReq{AgentID: "n1", UUID: "UUID-N1", Timestamp: base, NetInBytes: 1000, NetOutBytes: 2000}
	s.UpdateProbe(first)
	if first.NetDeltaKnown {
		t.Fatal("首点增量应为未知")
	}

	// 第二点：+2000 字节 / 10 秒 = 200 B/s
	p2 := &protocol.ProbeDataReq{AgentID: "n1", UUID: "UUID-N1", Timestamp: base + 10, NetInBytes: 3000, NetOutBytes: 6000}
	s.UpdateProbe(p2)
	if !p2.NetDeltaKnown {
		t.Fatal("正常采样点增量应可信")
	}
	if p2.NetInDeltaBytes != 2000 || p2.NetOutDeltaBytes != 4000 {
		t.Fatalf("增量计算错误: in=%d out=%d", p2.NetInDeltaBytes, p2.NetOutDeltaBytes)
	}
	if a := s.Get("n1"); a.NetRateIn != 200 || a.NetRateOut != 400 {
		t.Fatalf("速率计算错误: in=%d out=%d", a.NetRateIn, a.NetRateOut)
	}

	// 第三点：下行计数器回绕（变小），上行继续增长 -> 下行增量未知而非负数
	p3 := &protocol.ProbeDataReq{AgentID: "n1", UUID: "UUID-N1", Timestamp: base + 20, NetInBytes: 10, NetOutBytes: 7000}
	s.UpdateProbe(p3)
	if p3.NetInDeltaBytes != 0 {
		t.Fatalf("回绕点下行增量应为 0，实得 %d", p3.NetInDeltaBytes)
	}
	if p3.NetOutDeltaBytes != 1000 {
		t.Fatalf("回绕点上行增量应为 1000，实得 %d", p3.NetOutDeltaBytes)
	}
}

// TestNetTotalsSince 验证"窗口内累计流量"口径：只累加已知增量，
// 且可以按时间窗裁剪。
func TestNetTotalsSince(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "n2", UUID: "UUID-N2", Online: true})

	// 固定基准时间：用 time.Now() 会在"构造采样"与"断言"之间产生毫秒级漂移，
	// 使窗口边界的断言偶发失败（测试必须与运行时刻无关）。
	const base = int64(1_700_000_000)

	// 每 10 秒一次，累计下行 0 -> 1000 -> 3000 -> 6000（相邻增量 1000/2000/3000）
	for i, in := range []int64{0, 1000, 3000, 6000} {
		s.UpdateProbe(&protocol.ProbeDataReq{
			AgentID: "n2", UUID: "UUID-N2", Timestamp: base + int64(i*10),
			NetInBytes: in, NetOutBytes: in * 2,
		})
	}
	// 全窗口：首点无增量（计数器起点未知），其余 3 点增量求和 = 6000
	totalIn, totalOut, ok := s.NetTotalsSince("n2", time.Unix(base, 0))
	if !ok {
		t.Fatal("窗口内应存在可信增量点")
	}
	if totalIn != 6000 || totalOut != 12000 {
		t.Fatalf("窗口累计错误: in=%d out=%d", totalIn, totalOut)
	}

	// 只取后半段（第三点起）：窗口内只含最后两段增量 2000+3000=5000
	totalIn, _, ok = s.NetTotalsSince("n2", time.Unix(base+20, 0))
	if !ok || totalIn != 5000 {
		t.Fatalf("裁剪后的窗口累计错误: in=%d ok=%v", totalIn, ok)
	}

	// 未知主机：known=false（前端据此显示 —，而不是 0B）
	if _, _, ok := s.NetTotalsSince("nope", time.Unix(base, 0)); ok {
		t.Fatal("未知主机不应返回可信流量")
	}
}

// TestProbeNetDeltaRejectsAbsurdCounterJump 验证计数器异常跳变不污染趋势与累计。
//
// 典型场景：从旧库恢复的设备 LastSeen 有值、但累计字节过去未持久化（为 0），
// 首个采样会把"自启动以来的全部流量"当成这几秒的增量。若照单全收，
// 公开页会出现一个戳穿天花板的假尖峰，并被窗口累计永久计入。
func TestProbeNetDeltaRejectsAbsurdCounterJump(t *testing.T) {
	s := NewStore(nil)
	// LastSeen 有值（模拟从库恢复），NetInBytes 为 0（旧版未持久化累计值）
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "lj", UUID: "UUID-LJ", Online: true, LastSeen: 1000})

	p := &protocol.ProbeDataReq{
		AgentID: "lj", UUID: "UUID-LJ", Timestamp: 1010,
		NetInBytes: 5_000_000_000_000, NetOutBytes: 5_000_000_000_000, // 5TB/10s
	}
	s.UpdateProbe(p)
	if p.NetDeltaKnown {
		t.Fatalf("异常跳变不应标记为可信增量: %+v", p)
	}
	if in, out, known := s.NetTotalsSince("lj", time.Unix(0, 0)); known && (in != 0 || out != 0) {
		t.Fatalf("异常跳变不应计入窗口累计: in=%d out=%d known=%v", in, out, known)
	}

	// 正常量级的增量仍然可用（对照组，避免把守卫写死成"永远不可信"）。
	// 注意基准累计值已被上面的异常点推进到 5TB：这里在同一基准上给出正常增量。
	p2 := &protocol.ProbeDataReq{AgentID: "lj", UUID: "UUID-LJ", Timestamp: 1020, NetInBytes: 5_000_000_001_000, NetOutBytes: 5_000_000_001_000}
	s.UpdateProbe(p2)
	if !p2.NetDeltaKnown || p2.NetInDeltaBytes != 1000 {
		t.Fatalf("正常增量应被采纳: known=%v delta=%d", p2.NetDeltaKnown, p2.NetInDeltaBytes)
	}
}

// TestAllDoesNotBlockOnGeoLookup 回归：All() 每次探针推送（5–10 秒一次）都会
// 被调用，绝不能被外部地理定位服务拖住。
//
// 早期实现是同步请求外部接口，首次上线的主机会让整条 WS 推送卡住数秒 ——
// 前端表现为首屏长时间空白、偶尔才成功（移动端高版本浏览器上尤其明显）。
func TestAllDoesNotBlockOnGeoLookup(t *testing.T) {
	s := NewStore(nil)
	// 指向必然连接失败的地址：若实现真去同步请求，本用例会明显变慢
	r := geo.NewResolver()
	r.SetTimeout(300 * time.Millisecond)
	r.SetEndpoints("http://127.0.0.1:1/%s", "http://127.0.0.1:1/%s", "http://127.0.0.1:1/%s")
	s.SetGeoResolver(r)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true, PublicIP: "8.8.4.4"})

	done := make(chan struct{})
	go func() {
		s.All()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("All() 被地理定位解析阻塞：必须只读缓存 + 后台补齐")
	}
	s.WaitBackground()
}

// TestAllKicksOffAsyncGeoLookup 未命中缓存时应在后台补齐并回调通知
// （前端据此收到"补推的那一帧"，地区/国旗才会显示出来）。
func TestAllKicksOffAsyncGeoLookup(t *testing.T) {
	s := NewStore(nil)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"code":200,"data":{"country":"美国","as_country":"US","region":"加州","city":"洛杉矶"}}`))
	}))
	defer srv.Close()

	r := geo.NewResolver()
	r.SetEndpoints(srv.URL+"/%s", srv.URL+"/%s", srv.URL+"/%s")
	s.SetGeoResolver(r)
	notified := make(chan struct{}, 8)
	s.SetGeoNotify(func() { notified <- struct{}{} })
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true, PublicIP: "8.8.4.4"})

	// 首次：返回 nil（未缓存），但应触发后台解析
	if got := s.All()[0].Geo; got != nil {
		t.Fatalf("首次调用不应有地理位置（未缓存，且不得同步请求），实得 %+v", got)
	}
	select {
	case <-notified:
	case <-time.After(3 * time.Second):
		t.Fatal("解析完成后必须回调，否则前端拿不到地区/国旗")
	}
	s.WaitBackground()
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("应只解析一次，实得 %d 次", n)
	}
	// 第二次：应直接读缓存
	got := s.All()[0].Geo
	if got == nil || got.CountryCode != "US" {
		t.Fatalf("缓存生效后应返回地理位置，实得 %+v", got)
	}
	s.All()
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("缓存命中后不应重复解析，实得 %d 次", n)
	}
}

// TestAllCachedNeverTriggersGeoOutbound 匿名读路径不得驱动外呼 GeoIP（M-13）。
//
// 原来公开页的列表/详情也走 All()，于是"访客量"与"免费地理接口的额度"之间
// 一个闸门都没有：一组无节流的公开页请求就能把 -proxy-ip 的配额打空，
// 顺带在服务端堆协程。缓存由后台仪表盘那条 All() 路径负责填，两处读同一份。
func TestAllCachedNeverTriggersGeoOutbound(t *testing.T) {
	s := NewStore(nil)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"code":200,"data":{"country":"美国","as_country":"US"}}`))
	}))
	defer srv.Close()

	r := geo.NewResolver()
	r.SetEndpoints(srv.URL+"/%s", srv.URL+"/%s", srv.URL+"/%s")
	s.SetGeoResolver(r)
	notified := make(chan struct{}, 8)
	s.SetGeoNotify(func() { notified <- struct{}{} })
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true, PublicIP: "8.8.4.4"})

	for i := 0; i < 20; i++ {
		if got := s.AllCached()[0].Geo; got != nil {
			t.Fatalf("AllCached 不得读取到未准备好的外呼结果，实得 %+v", got)
		}
	}
	// 稍等一拍：若实现走的是 All()，外呼此刻早已发生
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("AllCached 触发了 %d 次外呼解析：匿名访客可以消耗出网额度", n)
	}

	// 后台路径暖好缓存后，匿名读应当看到同一份数据（分桶不该造成口径差异）。
	// 这里等回调而不是 WaitBackground：解析协程由 geo 内部再起一个，不属于 store.wg。
	s.All()
	select {
	case <-notified:
	case <-time.After(3 * time.Second):
		t.Fatal("后台解析未回调，测试无法继续")
	}
	cached := s.AllCached()[0].Geo
	if cached == nil || cached.CountryCode != "US" {
		t.Fatalf("缓存就绪后 AllCached 应读到地理位置，实得 %+v", cached)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("外呼应只发生一次，实得 %d 次", n)
	}
}

// TestAllSkipsGeoWhenNoResolver 未注入解析器时不应 panic（部署时可能只跑命令行）。
func TestAllSkipsGeoWhenNoResolver(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "a1", Online: true, PublicIP: "8.8.4.4"})
	got := s.All()
	if len(got) != 1 || got[0].Geo != nil {
		t.Fatalf("无解析器时应返回主机但不带地理位置，实得 %+v", got)
	}
	s.WaitBackground()
}
