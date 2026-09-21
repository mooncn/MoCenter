package publicview

import (
	"encoding/json"
	"strings"
	"testing"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/siteconfig"
)

// sampleAgent 构造一台"信息齐全"的主机，用于验证脱敏是否彻底。
func sampleAgent() *protocol.AgentInfo {
	return &protocol.AgentInfo{
		AgentID:        "agent-secret-001",
		UUID:           "4C4C4544-0037-3010-8046-B7C04F4A3332",
		Hostname:       "prod-db-01.internal",
		OSType:         "linux",
		Arch:           "amd64",
		KernelVersion:  "5.15.0-91-generic",
		OSVersion:      "Ubuntu 22.04.3 LTS",
		Online:         true,
		LastSeen:       1700000000,
		Uptime:         86400,
		CPUUsage:       37.46,
		CPUModel:       "Intel Xeon Gold 6248R",
		CPUMHz:         3000,
		MemUsage:       61.24,
		MemTotalBytes:  64 << 30,
		MemUsedBytes:   39 << 30,
		DiskUsage:      72.5,
		DiskTotalBytes: 512 << 30,
		DiskUsedBytes:  371 << 30,
		NetInBytes:     12345,
		NetOutBytes:    6789,
		NetRateIn:      1024,
		NetRateOut:     2048,
		RemoteAddr:     "10.1.2.3:54321",
		PublicIP:       "203.0.113.45",
		PublicIPv6:     "2001:db8:85a3:1234:5678:8a2e:370:7334",
		Geo: &protocol.GeoInfo{
			Country: "中国", CountryCode: "CN", Region: "江苏", City: "南京",
			Latitude: 32.06, Longitude: 118.79,
		},
		Remark: "核心交易库",
	}
}

// 核心断言：公开视图的 JSON 里绝不能出现 agent_id / uuid / 内网地址 / 经纬度。
func TestBuildNodeNeverLeaksIdentifiers(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	// 把所有字段都打开（最坏情况），验证即使全开也不泄露敏感标识
	for _, f := range siteconfig.FieldCatalog {
		cfg.Fields[f.Key] = true
	}
	node := BuildNode(sampleAgent(), cfg, testKeys)
	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	for _, forbidden := range []string{
		"agent-secret-001",        // agent_id（命令下发句柄）
		"4C4C4544-0037-3010-8046", // 硬件 UUID（机器指纹）
		"10.1.2.3",                // 内网地址
		"32.06", "118.79",         // 经纬度（可定位机房）
		"12345", "6789", // 累计流量
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("公开视图泄露了敏感字段 %q: %s", forbidden, body)
		}
	}
	// 节点编号必须是派生值，不是 agent_id
	if node.NodeID == "" || strings.HasPrefix(node.NodeID, "agent") {
		t.Fatalf("node_id 未正确派生: %q", node.NodeID)
	}
	// 公网 IP 默认掩码
	if node.PublicIP != "203.0.*.*" {
		t.Errorf("公网 IP 应被掩码，实际 %q", node.PublicIP)
	}
}

// 关闭的字段在 JSON 中必须"不存在"，而不是空值。
func TestDisabledFieldsAbsentFromJSON(t *testing.T) {
	cfg := siteconfig.Default() // 默认已关闭 hostname / os_version / public_ip
	cfg.Enabled = true
	node := BuildNode(sampleAgent(), cfg, testKeys)
	raw, _ := json.Marshal(node)
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hostname", "os_version", "kernel_version", "public_ip", "public_ipv6"} {
		if _, ok := m[key]; ok {
			t.Errorf("关闭字段 %s 不应出现在 JSON 中", key)
		}
	}
	for _, key := range []string{"node_id", "remark", "cpu_usage", "mem_usage", "disk_usage", "geo"} {
		if _, ok := m[key]; !ok {
			t.Errorf("默认可见字段 %s 应出现在 JSON 中", key)
		}
	}
}

// node_id 必须稳定（同一 agent_id + 同一部署密钥多次派生一致）且与 agent_id 单向。
func TestNodeIDStableAndOpaque(t *testing.T) {
	a := testKeys.ID("host-1")
	if a != testKeys.ID("host-1") {
		t.Fatal("node_id 不稳定")
	}
	if a == testKeys.ID("host-2") {
		t.Fatal("不同主机不应得到相同 node_id")
	}
	if strings.Contains(a, "host-1") {
		t.Fatal("node_id 不应包含原始 agent_id")
	}
	// n- + 16 位 hex（8 字节摘要）：长度掉回 4 字节就等于把暴力成本降到可枚举
	if !strings.HasPrefix(a, "n-") || len(a) != 2+nodeIDBytes*2 {
		t.Fatalf("node_id 格式不符合约定: %q（期望 n- 前缀 + %d 位 hex）", a, nodeIDBytes*2)
	}
}

// BuildNodes 只保留被允许的主机，并按在线优先排序。
func TestBuildNodesFiltersAndSorts(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.CatalogEnabled = false
	cfg.HostIDs = []string{"b", "c"}

	agents := []*protocol.AgentInfo{
		{AgentID: "a", Remark: "A"},
		{AgentID: "b", Remark: "B", Online: false},
		{AgentID: "c", Remark: "C", Online: true},
	}
	nodes := BuildNodes(agents, cfg, testKeys)
	if len(nodes) != 2 {
		t.Fatalf("应只输出白名单内的 2 台主机，实际 %d", len(nodes))
	}
	if nodes[0].Remark != "C" {
		t.Fatalf("在线主机应排在前面，实际首项 %q", nodes[0].Remark)
	}
}

// 汇总口径必须只统计可见节点，避免"卡片列表只见 2 台、统计却按 10 台算"。
func TestBuildSummaryCountsVisibleOnly(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.CatalogEnabled = false
	cfg.HostIDs = []string{"c"}
	agents := []*protocol.AgentInfo{
		{AgentID: "a", CPUUsage: 100, Online: true},
		{AgentID: "b", CPUUsage: 100, Online: true},
		{AgentID: "c", CPUUsage: 40, Online: true, MemTotalBytes: 100, MemUsedBytes: 50},
	}
	s := BuildSummary(BuildNodes(agents, cfg, testKeys), 0)
	if s.Total != 1 || s.Online != 1 || s.AvgCPU != 40 {
		t.Fatalf("汇总未按可见节点计算: %+v", s)
	}
	if s.TotalMem != 100 || s.UsedMem != 50 {
		t.Fatalf("汇总内存口径错误: %+v", s)
	}
}

// 历史采样只带开启的序列。
func TestBuildSamplesRespectsFields(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Fields = map[string]bool{"cpu_usage": true}
	history := []*protocol.ProbeDataReq{
		{Timestamp: 1, CPUUsage: 10.55, MemUsage: 20, DiskUsage: 30},
		{Timestamp: 2, CPUUsage: 11, MemUsage: 21, DiskUsage: 31},
	}
	samples := BuildSamples(history, cfg)
	if len(samples) != 2 {
		t.Fatalf("采样点数错误: %d", len(samples))
	}
	raw, _ := json.Marshal(samples[0])
	if strings.Contains(string(raw), "mem_usage") || strings.Contains(string(raw), "disk_usage") {
		t.Fatalf("未开启的序列不应输出: %s", raw)
	}
	if samples[0].CPUUsage == nil || *samples[0].CPUUsage != 10.6 { // 保留 1 位小数（四舍五入）
		t.Fatalf("CPU 值未按 1 位小数归一: %v", samples[0].CPUUsage)
	}
	if samples[0].MemUsage != nil || samples[0].DiskUsage != nil {
		t.Fatal("未开启的序列应为 nil（JSON 中不存在）")
	}
}

// 公网 IP 掩码：IPv4 保留前两段，IPv6 保留前四组。
func TestMaskPublicIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.45":                          "203.0.*.*",
		"2001:db8:85a3:1234:5678:8a2e:370:7334": "2001:db8:85a3:1234::***",
		"":                                      "",
		"not-an-ip":                             "",
	}
	for in, want := range cases {
		if got := MaskPublicIP(in); got != want {
			t.Errorf("MaskPublicIP(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// 关闭 hostname 时备注为空应降级为 node_id，避免公开页出现无标题卡片。
func TestRemarkFallback(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	a := sampleAgent()
	a.Remark = ""
	node := BuildNode(a, cfg, testKeys)
	if node.Remark != "" {
		t.Fatalf("备注为空且 hostname 关闭时不应输出名称，实际 %q", node.Remark)
	}

	cfg.Fields["hostname"] = true
	node = BuildNode(a, cfg, testKeys)
	if node.Remark != a.Hostname {
		t.Fatalf("备注为空应降级用主机名，实际 %q", node.Remark)
	}
}

// 网络流量：开启时输出速率，未开启时速率与累计量都不出现在 JSON 中。
func TestNetworkFieldGate(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true

	node := BuildNode(sampleAgent(), cfg, testKeys)
	if node.NetRateIn != 1024 || node.NetRateOut != 2048 {
		t.Fatalf("默认应公开网络速率，实际 in=%d out=%d", node.NetRateIn, node.NetRateOut)
	}
	raw, _ := json.Marshal(node)
	if strings.Contains(string(raw), "net_in_bytes") {
		t.Fatalf("网卡累计原始计数器不应出现在公开视图: %s", raw)
	}

	// 关闭 network：速率与累计全部消失
	cfg.Fields["network"] = false
	node = BuildNode(sampleAgent(), cfg, testKeys)
	raw, _ = json.Marshal(node)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	for _, key := range []string{"net_rate_in", "net_rate_out", "net_total_in", "net_total_out"} {
		if _, ok := m[key]; ok {
			t.Errorf("network 关闭时 %s 不应出现在 JSON 中", key)
		}
	}
}

// 累计流量口径：boot 取网卡累计，window 交由调用方（此函数不填）。
func TestAttachTrafficModes(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	a := sampleAgent()
	node := BuildNode(a, cfg, testKeys)

	AttachTraffic(node, a, cfg)
	if node.NetTotalIn != a.NetInBytes || node.NetTotalOut != a.NetOutBytes {
		t.Fatalf("boot 口径应取网卡累计: %d/%d", node.NetTotalIn, node.NetTotalOut)
	}

	cfg.TrafficMode = siteconfig.TrafficModeWindow
	node = BuildNode(a, cfg, testKeys)
	AttachTraffic(node, a, cfg)
	if node.NetTotalIn != 0 || node.NetTotalOut != 0 {
		t.Fatal("window 口径不应由 AttachTraffic 填写（需查历史）")
	}
}

// 趋势流量：由相邻采样累计值差求得速率，首点与回绕点不输出。
func TestBuildSamplesNetRates(t *testing.T) {
	cfg := siteconfig.Default()
	history := []*protocol.ProbeDataReq{
		{Timestamp: 1000, NetInBytes: 1000, NetOutBytes: 500},
		{Timestamp: 1010, NetInBytes: 3000, NetOutBytes: 2500}, // +2000/10s = 200B/s, +2000/10s = 200B/s
		{Timestamp: 1020, NetInBytes: 10, NetOutBytes: 2600},   // 下行计数器回绕 -> 该点下行不输出
	}
	samples := BuildSamples(history, cfg)
	if len(samples) != 3 {
		t.Fatalf("采样点数错误: %d", len(samples))
	}
	if samples[0].NetInBps != nil || samples[0].NetOutBps != nil {
		t.Fatal("首点没有前驱，不应输出速率")
	}
	if samples[1].NetInBps == nil || *samples[1].NetInBps != 200 {
		t.Fatalf("下行速率计算错误: %v", samples[1].NetInBps)
	}
	if samples[1].NetOutBps == nil || *samples[1].NetOutBps != 200 {
		t.Fatalf("上行速率计算错误: %v", samples[1].NetOutBps)
	}
	if samples[2].NetInBps != nil {
		t.Fatalf("计数器回绕点不应输出速率: %v", samples[2].NetInBps)
	}

	// 关闭 network：流量序列整体消失
	cfg.Fields["network"] = false
	samples = BuildSamples(history, cfg)
	raw, _ := json.Marshal(samples)
	if strings.Contains(string(raw), "net_in_bps") || strings.Contains(string(raw), "net_out_bps") {
		t.Fatalf("network 关闭时不应输出流量序列: %s", raw)
	}
}

// 时间戳重复/倒退的点必须被跳过（否则除零或把历史间隔当作瞬时速度）。
func TestBuildSamplesNetRatesSkipsBadTimestamps(t *testing.T) {
	cfg := siteconfig.Default()
	history := []*protocol.ProbeDataReq{
		{Timestamp: 2000, NetInBytes: 100},
		{Timestamp: 2000, NetInBytes: 5000}, // 同一时间戳
		{Timestamp: 1990, NetInBytes: 9000}, // 时间倒退
	}
	samples := BuildSamples(history, cfg)
	for i, s := range samples[1:] {
		if s.NetInBps != nil {
			t.Fatalf("第 %d 个异常时间戳点不应输出速率: %v", i+1, *s.NetInBps)
		}
	}
}

// 汇总流量：只统计可见节点，且负值不计入。
func TestBuildSummaryTraffic(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.CatalogEnabled = false
	cfg.HostIDs = []string{"c"}
	agents := []*protocol.AgentInfo{
		{AgentID: "a", NetRateIn: 999, NetRateOut: 999},
		{AgentID: "c", NetRateIn: 100, NetRateOut: 200},
	}
	nodes := BuildNodes(agents, cfg, testKeys)
	if len(nodes) != 1 {
		t.Fatalf("可见节点数错误: %d", len(nodes))
	}
	nodes[0].NetTotalIn = 1000
	nodes[0].NetTotalOut = 2000
	s := BuildSummary(nodes, 0)
	if s.NetRateIn != 100 || s.NetRateOut != 200 {
		t.Fatalf("速率汇总错误: %+v", s)
	}
	if s.NetTotalIn != 1000 || s.NetTotalOut != 2000 {
		t.Fatalf("累计汇总错误: %+v", s)
	}
}

// 异常速率的点不下发：趋势图宁可不连线，也不要戳穿天花板的假尖峰。
func TestBuildSamplesFiltersAbsurdRates(t *testing.T) {
	cfg := siteconfig.Default()
	history := []*protocol.ProbeDataReq{
		{Timestamp: 1000, NetInBytes: 0},
		// 10 秒内涨了 5TB -> 500GB/s，超过 50GB/s 上限，应被丢弃
		{Timestamp: 1010, NetInBytes: 5_000_000_000_000},
	}
	s := BuildSamples(history, cfg)
	if s[1].NetInBps != nil {
		t.Fatalf("异常速率不应下发: %v", *s[1].NetInBps)
	}
}

// TestLoadFieldGate 负载字段：开关关闭时不出现在 JSON；平台不提供（LoadKnown=false）
// 时也不出现 —— 后者是为了让前端显示「—」而不是把 0 画成"完全空闲"。
func TestLoadFieldGate(t *testing.T) {
	a := sampleAgent()
	a.Load1, a.Load5, a.Load15 = f64(0.42), f64(0.31), f64(0.25)

	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.Fields["load"] = false
	if raw, _ := json.Marshal(BuildNode(a, cfg, testKeys)); strings.Contains(string(raw), "load1") {
		t.Fatalf("负载开关关闭时不应出现 load1: %s", raw)
	}

	cfg.Fields["load"] = true
	node := BuildNode(a, cfg, testKeys)
	if node.Load1 == nil || *node.Load1 != 0.42 || node.Load5 == nil || *node.Load5 != 0.31 {
		t.Fatalf("负载应被下发且保留两位小数: %+v", node)
	}
	raw, _ := json.Marshal(node)
	if !strings.Contains(string(raw), `"load1":0.42`) {
		t.Fatalf("负载精度应为 2 位小数: %s", raw)
	}

	// 平台不提供负载：字段整体消失（而不是 0）
	a.Load1, a.Load5, a.Load15 = nil, nil, nil
	node2 := BuildNode(a, cfg, testKeys)
	raw2, _ := json.Marshal(node2)
	if strings.Contains(string(raw2), `"load1"`) {
		t.Fatalf("LoadKnown=false 时不应下发负载: %s", raw2)
	}
}

// TestLoadRounded 负载量级小，必须保留 2 位小数（1 位会把 0.15/0.24 抹平）。
func TestLoadRounded(t *testing.T) {
	a := sampleAgent()
	a.Load1, a.Load5, a.Load15 = f64(0.154), f64(1.005), f64(12.999)
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.Fields["load"] = true
	n := BuildNode(a, cfg, testKeys)
	if *n.Load1 != 0.15 || *n.Load5 != 1 || *n.Load15 != 13 {
		t.Fatalf("负载取整异常: %v / %v / %v", *n.Load1, *n.Load5, *n.Load15)
	}
}

// TestLoadZeroIsValidValue 负载 0 是"空闲机器"的合法值，必须如实下发，
// 不能因为 omitempty/零值判断被当成"没有数据"丢掉（那会让前端显示「—」）。
func TestLoadZeroIsValidValue(t *testing.T) {
	a := sampleAgent()
	a.Load1, a.Load5, a.Load15 = f64(0), f64(0), f64(0)
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.Fields["load"] = true
	node := BuildNode(a, cfg, testKeys)
	if node.Load1 == nil || *node.Load1 != 0 {
		t.Fatalf("负载 0 应被下发: %+v", node.Load1)
	}
	raw, _ := json.Marshal(node)
	if !strings.Contains(string(raw), `"load1":0`) {
		t.Fatalf("负载 0 应出现在 JSON 中: %s", raw)
	}
}

func f64(v float64) *float64 { return &v }

// 回归：离线状态必须**显式**出现在 JSON 里（"online": false）。
//
// 历史 Bug：Online 曾是 `bool` + `omitempty`，离线（false）是零值，被
// omitempty 从 JSON 中抹掉 —— 前端拿到的 online 是 undefined，于是
//
//	· 卡片右上角状态点整个不渲染（离线机器看着像"没接入"）；
//	· 列表视图状态列显示 "-"；节点详情不显示在线/离线徽标。
//
// 修复后三态语义明确：nil=字段未开启 / true=在线 / false=离线。
func TestOfflineStateSerializedExplicitly(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	a := sampleAgent()
	a.Online = false

	node := BuildNode(a, cfg, testKeys)
	if node.Online == nil {
		t.Fatal("「在线状态」开启时 Online 不应为 nil")
	}
	raw, _ := json.Marshal(node)
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m["online"]
	if !ok {
		t.Fatal("离线节点必须在 JSON 中显式携带 online: false（否则前端无法区分「离线」与「字段未开启」）")
	}
	if v != false {
		t.Fatalf("离线节点 online 应为 false，实际 %v", v)
	}
	if node.IsOnline() {
		t.Fatal("IsOnline 对离线节点应返回 false")
	}
}

// 关闭「在线状态」字段时 online 键必须消失（与其它字段一样的数据不下发）。
func TestOnlineFieldGate(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	cfg.Fields = map[string]bool{}
	node := BuildNode(sampleAgent(), cfg, testKeys)
	if node.Online != nil {
		t.Fatal("关闭「在线状态」后 Online 应为 nil")
	}
	raw, _ := json.Marshal(node)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["online"]; ok {
		t.Error("关闭「在线状态」后 JSON 中不应出现 online 键")
	}
	// 字段未开启时按离线归类，保证汇总的在线+离线=总数恒成立
	if node.IsOnline() {
		t.Error("字段未开启时 IsOnline 应返回 false")
	}
}

// 汇总的在线/离线计数必须与节点状态一致（含离线→在线切换）。
func TestSummaryOnlineOfflineCounts(t *testing.T) {
	cfg := siteconfig.Default()
	cfg.Enabled = true
	agents := []*protocol.AgentInfo{
		{AgentID: "a", Online: true},
		{AgentID: "b", Online: false},
		{AgentID: "c", Online: false},
	}
	s := BuildSummary(BuildNodes(agents, cfg, testKeys), 0)
	if s.Total != 3 || s.Online != 1 || s.Offline != 2 {
		t.Fatalf("在线/离线统计错误: total=%d online=%d offline=%d", s.Total, s.Online, s.Offline)
	}
	if s.Online+s.Offline != s.Total {
		t.Fatalf("在线+离线应等于总数: %d+%d != %d", s.Online, s.Offline, s.Total)
	}
}
