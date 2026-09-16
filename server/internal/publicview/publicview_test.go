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
	node := BuildNode(sampleAgent(), cfg)
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
	node := BuildNode(sampleAgent(), cfg)
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

// node_id 必须稳定（同一 agent_id 多次派生一致）且与 agent_id 单向。
func TestNodeIDStableAndOpaque(t *testing.T) {
	a := NodeID("host-1")
	if a != NodeID("host-1") {
		t.Fatal("node_id 不稳定")
	}
	if a == NodeID("host-2") {
		t.Fatal("不同主机不应得到相同 node_id")
	}
	if strings.Contains(a, "host-1") {
		t.Fatal("node_id 不应包含原始 agent_id")
	}
	if !strings.HasPrefix(a, "n-") || len(a) != 10 {
		t.Fatalf("node_id 格式不符合约定: %q", a)
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
	nodes := BuildNodes(agents, cfg)
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
	s := BuildSummary(BuildNodes(agents, cfg), 0)
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
	node := BuildNode(a, cfg)
	if node.Remark != "" {
		t.Fatalf("备注为空且 hostname 关闭时不应输出名称，实际 %q", node.Remark)
	}

	cfg.Fields["hostname"] = true
	node = BuildNode(a, cfg)
	if node.Remark != a.Hostname {
		t.Fatalf("备注为空应降级用主机名，实际 %q", node.Remark)
	}
}
