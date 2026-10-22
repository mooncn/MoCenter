// Package publicview 把内部主机信息**脱敏**成可对外展示的视图。
//
// 设计原则（对应 Issue #5 的"只展示非敏感信息"）：
//
//   - 输出结构体只包含允许公开的字段，且每个字段都带 `omitempty`：
//     未开启的字段在结构体层面就是零值，序列化后**根本不存在**，
//     而不是靠前端"不渲染"。前端隐藏属于 UI，接口层面必须没有数据。
//   - agent_id / uuid / remote_addr(内网地址) / 网卡累计流量 一律不出现在
//     任何输出结构中（没有对应字段 = 无法因配置误开而泄露）。
//   - 每台主机额外派生一个**部署内稳定**的节点编号 node_id（n-xxxxxxxxxxxxxxxx），
//     供公开页路由与前端 key 使用。它由**部署私钥**（`<data>/node.key`，安装期随机生成）
//     做 HMAC-SHA256 后截 8 字节得到：反向无法推导 agent_id，同一 agent_id 在不同部署
//     里也得到不同编号 —— 因此公开页既不需要暴露 agent_id，外人也无法拿主机名候选
//     离线字典反查"哪些主机在册"（详见 NodeKeys）。
//
// 本包不做配置判断（是否可见由调用方 siteconfig 决定），只负责"按开关裁剪"，
// 这样脱敏逻辑可被单元测试单独覆盖。
package publicview

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/siteconfig"
)

// Rate / usage 精度常量（公开页统一保留 1 位小数，避免浮点尾巴）。
const (
	// trendCap 趋势序列可携带的最大采样点数（与公开接口的 maxPoints 对齐）。
	trendCap = 200
	// byteRateMax 单点速率上限 400Gbit/s（=50GB/s）：超出视为计数器异常
	// （旧库恢复导致自启动累计被当成单次增量、溢出、时间戳跳变），
	// 直接丢弃该点而不是画出一根戳穿天花板的假尖峰。
	// 与 probe.rateLimitBytesPerSec 同口径：服务端写入时已挡一次，这里再挡一次，
	// 让 publicview（可单测的脱敏层）对内存回退路径的老数据同样免疫。
	byteRateMax = float64(50_000_000_000)
)

// Node 公开页的单台主机视图。
type Node struct {
	// NodeID 公开节点编号（n- + 16 位 hex），由部署私钥派生，非 agent_id、外部不可反推
	NodeID string `json:"node_id"`
	// Remark 备注名（运维填写，通常是最适合对外展示的友好名称）
	Remark string `json:"remark,omitempty"`
	// Hostname 主机名（敏感：可能含内网命名规范，默认关闭）
	Hostname string `json:"hostname,omitempty"`
	// Online 在线状态。
	//
	// 用 *bool 而不是 bool：bool + omitempty 会把**离线**（false）当成"零值"
	// 一并从 JSON 里抹掉，前端拿到的是 undefined —— 于是"离线"既无法与
	// "该字段未开启"区分，状态点还会整个不渲染（离线机器看起来像没接入）。
	//
	// 三态语义：
	//   nil   = 「在线状态」字段未开启（响应里不出现 online 键）；
	//   &true = 在线；
	//   &false= 离线（显式出现 "online": false）。
	//
	// 注意：这里必须保留 omitempty —— 它让 nil 时不输出，正好满足"未勾选的
	// 字段在结构体层面即零值、序列化后从 JSON 消失"的安全约定。
	Online *bool `json:"online,omitempty"`
	// LastSeen 最后在线时间（Unix 秒）
	LastSeen int64 `json:"last_seen,omitempty"`
	// Uptime 运行时长（秒）
	Uptime int64 `json:"uptime,omitempty"`

	// OSType 操作系统类型（Linux / Windows）
	OSType string `json:"os_type,omitempty"`
	// OSVersion 系统版本（敏感：漏洞指纹，默认关闭）
	OSVersion string `json:"os_version,omitempty"`
	// Arch 架构
	Arch string `json:"arch,omitempty"`
	// KernelVersion 内核版本（敏感，默认关闭）
	KernelVersion string `json:"kernel_version,omitempty"`

	// Geo 地区（国家/省份/城市，非 lat/lon —— 精确经纬度可定位机房）
	Geo *Geo `json:"geo,omitempty"`
	// PublicIP / PublicIPv6 公网出口 IP（敏感，默认关闭）
	PublicIP   string `json:"public_ip,omitempty"`
	PublicIPv6 string `json:"public_ipv6,omitempty"`

	// NetRateIn / NetRateOut 实时下行 / 上行速率（字节/秒），由服务端按相邻探针差值计算
	NetRateIn  int64 `json:"net_rate_in,omitempty"`
	NetRateOut int64 `json:"net_rate_out,omitempty"`
	// NetTotalIn / NetTotalOut 区间累计流量（字节）。
	// 口径由 siteconfig.TrafficMode 决定：boot=自设备启动以来（网卡累计），
	// window=历史留存窗口内（各采样点增量的和）。
	NetTotalIn  int64 `json:"net_total_in,omitempty"`
	NetTotalOut int64 `json:"net_total_out,omitempty"`

	// CPUUsage CPU 利用率（%）
	CPUUsage float64 `json:"cpu_usage,omitempty"`
	// CPUModel CPU 型号与主频
	CPUModel string `json:"cpu_model,omitempty"`
	// MemUsage 内存利用率（%）
	MemUsage float64 `json:"mem_usage,omitempty"`
	// MemUsedBytes / MemTotalBytes 内存已用与总量（字节）
	MemUsedBytes  int64 `json:"mem_used_bytes,omitempty"`
	MemTotalBytes int64 `json:"mem_total_bytes,omitempty"`
	// DiskUsage 磁盘利用率（%）
	DiskUsage float64 `json:"disk_usage,omitempty"`
	// DiskUsedBytes / DiskTotalBytes 磁盘已用与总量（字节）
	DiskUsedBytes  int64 `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes int64 `json:"disk_total_bytes,omitempty"`

	// Load1 / Load5 / Load15 系统平均负载。
	// 用指针 + omitempty：LoadKnown=false（平台不提供，如 Windows 未取到队列长度）
	// 时字段整体从 JSON 消失，前端显示「—」，而不是把 0 当成"完全空闲"。
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
}

// Geo 地区信息（刻意不含经纬度）。
type Geo struct {
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
}

// Sample 公开页的历史采样点（趋势图用）。
//
// 三个序列都用指针 + omitempty：未开启的字段在 JSON 中**整个消失**，
// 前端据此判断"该序列不可展示"，而不是拿到一串 0 画出一条假的水平线。
type Sample struct {
	TS        int64    `json:"ts"`
	CPUUsage  *float64 `json:"cpu_usage,omitempty"`
	MemUsage  *float64 `json:"mem_usage,omitempty"`
	DiskUsage *float64 `json:"disk_usage,omitempty"`
	// NetInBps / NetOutBps 下行 / 上行速率（字节/秒）。
	// 服务端从相邻采样的**累计字节**差值求得：既避免前端拿到累计量级（可能被
	// 用于估算主机承载的业务规模），也让 "流量最小的节点" 这类横向对比成立。
	NetInBps  *float64 `json:"net_in_bps,omitempty"`
	NetOutBps *float64 `json:"net_out_bps,omitempty"`
}

// Summary 站点级汇总指标（不含任何主机标识）。
type Summary struct {
	Total     int     `json:"total"`
	Online    int     `json:"online"`
	Offline   int     `json:"offline"`
	AvgCPU    float64 `json:"avg_cpu"`
	AvgMem    float64 `json:"avg_mem"`
	AvgDisk   float64 `json:"avg_disk"`
	TotalMem  int64   `json:"total_mem_bytes"`
	UsedMem   int64   `json:"used_mem_bytes"`
	TotalDisk int64   `json:"total_disk_bytes"`
	UsedDisk  int64   `json:"used_disk_bytes"`
	// 站点级流量（仅统计可见节点）：速率是当前值，累计是口径内总和
	NetRateIn   int64 `json:"net_rate_in,omitempty"`
	NetRateOut  int64 `json:"net_rate_out,omitempty"`
	NetTotalIn  int64 `json:"net_total_in,omitempty"`
	NetTotalOut int64 `json:"net_total_out,omitempty"`
	UpdatedAt   int64 `json:"updated_at"`
	ServerTime  int64 `json:"server_time"`
}

// NodeKeys 派生公开节点编号（node_id）的**部署私有密钥**。
//
// 为什么必须是密钥而不是固定盐（H-8）：node_id 是公开页上唯一对外可见的主机标识，
// 一旦它是"人人可算"的函数（例如固定盐 + SHA-256），匿名访客就能拿一批候选主机名
// （`web-01`、`db-prod`，或从某台开了 hostname 的兄弟节点学来的命名规律）离线算出
// 各自的 node_id，再用 `/api/public/samples?node=…` 的 404/200 当判定器逐条命中 ——
// 即使 `hostname` 开关关着，也等于把"哪些内网主机在册"整本念给外人听。
// 换成部署私钥后，同一个 agent_id 在不同部署里得到完全不同的编号，离线字典失去前提。
type NodeKeys struct {
	secret []byte
}

// NodeKeyFileName 部署私钥在数据目录下的文件名（与 <data>/agent.key 同一套习惯）。
const NodeKeyFileName = "node.key"

// minNodeKeyBytes 密钥长度下限（256 bit）。
//
// 取 32 字节是因为 HMAC 的强度不会超过密钥本身；短于这个值的密钥既可穷举，
// 也会让"部署私钥"退化回"公开常量"。
const minNodeKeyBytes = 32

// nodeIDBytes 编号携带的摘要字节数（64 bit）。
//
// 旧实现只取 4 字节（32 bit）：按生日界约 6.5 万个候选就能撞上同一个编号，
// 而公开页允许匿名反复查询。8 字节把碰撞与暴力成本推到实际不可达，
// 同时编号仍短到可以印在卡片上。
const nodeIDBytes = 8

// NewNodeKeys 校验并持有部署私钥。
//
// 这里**只拒绝、不补生成**：密钥必须由启动方（读数据目录）提供，
// 否则每次进程重启就会换一把，公开页的节点编号跟着全变，而没人会预期这件事。
func NewNodeKeys(secret []byte) (*NodeKeys, error) {
	if len(secret) < minNodeKeyBytes {
		return nil, fmt.Errorf("节点编号密钥不足 %d 字节（当前 %d），拒绝以可预测方式派生公开编号", minNodeKeyBytes, len(secret))
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &NodeKeys{secret: cp}, nil
}

// LoadNodeKeys 从文件读取部署私钥；不存在则生成一把随机密钥并写入（0600）。
//
// 文件格式与 <data>/agent.key 一致（十六进制文本 + 换行），便于运维用同一套习惯轮换。
// 读到内容但长度不够时**报错而不是重新生成**：那通常意味着文件被截断（磁盘满/
// 半途复制），静默换钥匙会让全部公开编号凭空改变，看起来像"页面坏了"。
func LoadNodeKeys(path string) (keys *NodeKeys, created bool, err error) {
	if path == "" {
		return nil, false, errors.New("节点编号密钥路径为空")
	}
	if data, rerr := os.ReadFile(path); rerr == nil {
		hexStr := strings.TrimSpace(string(data))
		if hexStr == "" {
			return nil, false, fmt.Errorf("节点编号密钥文件 %s 为空", path)
		}
		secret, derr := hex.DecodeString(hexStr)
		if derr != nil {
			return nil, false, fmt.Errorf("解析节点编号密钥 %s 失败: %w", path, derr)
		}
		k, kerr := NewNodeKeys(secret)
		if kerr != nil {
			return nil, false, fmt.Errorf("%v（如需重新生成，请先删除 %s）", kerr, path)
		}
		return k, false, nil
	} else if !os.IsNotExist(rerr) {
		return nil, false, rerr
	}
	secret := make([]byte, minNodeKeyBytes)
	if _, rerr := rand.Read(secret); rerr != nil {
		return nil, false, fmt.Errorf("生成节点编号密钥失败: %w", rerr)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0700); mkErr != nil {
		return nil, false, mkErr
	}
	// 先写临时文件再 rename：避免并发启动或掉电时留下半把密钥
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, []byte(hex.EncodeToString(secret)+"\n"), 0600); werr != nil {
		return nil, false, werr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		os.Remove(tmp)
		return nil, false, rerr
	}
	k, kerr := NewNodeKeys(secret)
	return k, true, kerr
}

// ID 派生某台主机的公开节点编号。
//
// nil 接收者是一次接线错误（说明调用方忘了把密钥传下来），这里 panic 而不是
// 退回旧的"固定盐派生"—— 静默退回等于把刚修掉的洞留在原地。
func (k *NodeKeys) ID(agentID string) string {
	if k == nil || len(k.secret) < minNodeKeyBytes {
		panic("publicview: 未初始化节点编号密钥（应在服务端启动时从数据目录载入）")
	}
	mac := hmac.New(sha256.New, k.secret)
	mac.Write([]byte(agentID))
	return "n-" + hex.EncodeToString(mac.Sum(nil)[:nodeIDBytes])
}

// Match 判断某个编号是否属于该 agent（反查，常数时间比较）。
func (k *NodeKeys) Match(agentID, nodeID string) bool {
	if k == nil {
		panic("publicview: 未初始化节点编号密钥（应在服务端启动时从数据目录载入）")
	}
	return subtle.ConstantTimeCompare([]byte(k.ID(agentID)), []byte(nodeID)) == 1
}

// MaskPublicIP 对公网 IP 做部分掩码（保留前两段），即使误开启也不至于精确到主机。
// IPv6 保留前 4 组。
func MaskPublicIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) <= 4 {
			return ip
		}
		return strings.Join(parts[:4], ":") + "::***"
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return parts[0] + "." + parts[1] + ".*.*"
}

// IsOnline 返回是否在线。
//
// nil（「在线状态」字段未开启）一律按**离线**处理：这样汇总统计
// （在线/离线台数）在任何配置下都有定义，不会因为字段开关而产生
// "既不在线也不离线"的第三类计数。
func (n *Node) IsOnline() bool {
	return n != nil && n.Online != nil && *n.Online
}

// OnlineState 返回在线状态的展示三元组，供前端/汇总复用：
// known=false 表示「在线状态」字段未开启（前端据此不渲染状态点）。
// （前端目前直接用 JSON 里有没有 online 键判断，此方法供 Go 侧写测试更直观。）
func (n *Node) OnlineState() (online, known bool) {
	if n == nil || n.Online == nil {
		return false, false
	}
	return *n.Online, true
}

// maskIPSwitch 掩码开关：true 时对公网 IP 做部分掩码。
// 由 BuildNode 的调用方（server）按部署策略注入，默认开启掩码。
var maskIPSwitch = true

// SetMaskPublicIP 设置是否掩码公网 IP（默认 true）。
func SetMaskPublicIP(on bool) { maskIPSwitch = on }

// BuildNode 按配置裁剪出单台主机的公开视图。
//
// cfg 决定字段可见性；调用方须已用 cfg.HostAllowed 判定该主机是否可公开。
// keys 用于派生 node_id（部署私钥，见 NodeKeys）。
func BuildNode(a *protocol.AgentInfo, cfg *siteconfig.Config, keys *NodeKeys) *Node {
	if a == nil {
		return nil
	}
	n := &Node{NodeID: keys.ID(a.AgentID)}
	on := func(key string) bool { return cfg.FieldVisible(key) }

	if on("remark") {
		n.Remark = a.Remark
		// 备注为空时降级用主机名，避免公开页出现一堆"未命名节点"
		if n.Remark == "" && on("hostname") {
			n.Remark = a.Hostname
		}
	}
	if on("hostname") {
		n.Hostname = a.Hostname
	}
	if on("online") {
		online := a.Online
		n.Online = &online
	}
	if on("last_seen") {
		n.LastSeen = a.LastSeen
	}
	if on("uptime") {
		n.Uptime = a.Uptime
	}

	if on("os_type") {
		n.OSType = a.OSType
	}
	if on("os_version") {
		n.OSVersion = a.OSVersion
	}
	if on("arch") {
		n.Arch = a.Arch
	}
	if on("kernel_version") {
		n.KernelVersion = a.KernelVersion
	}

	if on("geo") && a.Geo != nil {
		n.Geo = &Geo{
			Country:     a.Geo.Country,
			CountryCode: a.Geo.CountryCode,
			Region:      a.Geo.Region,
			City:        a.Geo.City,
		}
	}
	if on("public_ip") {
		n.PublicIP = a.PublicIP
		if maskIPSwitch {
			n.PublicIP = MaskPublicIP(a.PublicIP)
		}
	}
	if on("public_ipv6") {
		n.PublicIPv6 = a.PublicIPv6
		if maskIPSwitch {
			n.PublicIPv6 = MaskPublicIP(a.PublicIPv6)
		}
	}

	if on("network") {
		// 负值（计数器回绕/差分异常）不下发，避免前端画出反向尖刺
		n.NetRateIn = nonNegative(a.NetRateIn)
		n.NetRateOut = nonNegative(a.NetRateOut)
	}

	if on("cpu_usage") {
		n.CPUUsage = round1(a.CPUUsage)
	}
	if on("cpu_model") {
		n.CPUModel = cpuModelText(a)
	}
	if on("mem_usage") {
		n.MemUsage = round1(a.MemUsage)
	}
	// 容量与百分比分开授权：只想知道"用了多少"的站点可以关掉百分比，
	// 反之亦然。两项都关时 JSON 里不会出现任何内存容量字段。
	if on("mem_size") {
		n.MemUsedBytes = a.MemUsedBytes
		n.MemTotalBytes = a.MemTotalBytes
	}
	if on("disk_usage") {
		n.DiskUsage = round1(a.DiskUsage)
	}
	if on("disk_size") {
		n.DiskUsedBytes = a.DiskUsedBytes
		n.DiskTotalBytes = a.DiskTotalBytes
	}
	if on("load") && a.Load1 != nil {
		// 负载保留 2 位小数：1 位会把 0.15 与 0.24 抹成同一个值，
		// 而负载本身量级就小，精度比百分比指标更重要。
		// a.Load1 非 nil 才下发 —— 0 是合法的"空闲"负载，必须如实展示。
		l1 := round2(*a.Load1)
		n.Load1 = &l1
		if a.Load5 != nil {
			l5 := round2(*a.Load5)
			n.Load5 = &l5
		}
		if a.Load15 != nil {
			l15 := round2(*a.Load15)
			n.Load15 = &l15
		}
	}
	return n
}

// cpuModelText 拼出 CPU 型号文本（含主频），无型号时返回空串。
func cpuModelText(a *protocol.AgentInfo) string {
	if a.CPUModel == "" {
		return ""
	}
	if a.CPUMHz > 0 {
		return a.CPUModel + " @ " + trimFloat(a.CPUMHz/1000) + "GHz"
	}
	return a.CPUModel
}

// AttachTraffic 把流量口径写入节点视图（速率即时可得，累计需查历史）。
//
// 为什么不放在 BuildNode 里：单节点裁剪是纯函数（无 IO，可单测脱敏），
// 而累计流量要读探针历史。这里由调用方显式注入，避免脱敏逻辑与存储耦合。
func AttachTraffic(n *Node, a *protocol.AgentInfo, cfg *siteconfig.Config) {
	if n == nil || a == nil || cfg == nil || !cfg.FieldVisible("network") {
		return
	}
	if cfg.TrafficModeOrDefault() == siteconfig.TrafficModeWindow {
		// 由调用方另行查历史填充（store 内存回退与 SQL 结果口径不同，交给调用方）
		return
	}
	n.NetTotalIn = nonNegative(a.NetInBytes)
	n.NetTotalOut = nonNegative(a.NetOutBytes)
}

// BuildNodes 批量裁剪并按「在线优先、备注/主机名」排序，保证公开页顺序稳定
// （服务端 map 遍历顺序随机，不排序会导致每次推送卡片乱跳）。
func BuildNodes(agents []*protocol.AgentInfo, cfg *siteconfig.Config, keys *NodeKeys) []*Node {
	out := make([]*Node, 0, len(agents))
	for _, a := range agents {
		if a == nil || !cfg.HostAllowed(a.AgentID) {
			continue
		}
		if n := BuildNode(a, cfg, keys); n != nil {
			out = append(out, n)
		}
	}
	// 在线优先排序：IsOnline() 把"字段未开启"（nil）与"离线"一致对待，
	// 因此未开启在线状态时本排序退化为按名称排，行为与之前一致。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsOnline() != out[j].IsOnline() {
			return out[i].IsOnline()
		}
		return displayName(out[i]) < displayName(out[j])
	})
	return out
}

// displayName 排序用的显示名。
func displayName(n *Node) string {
	if n.Remark != "" {
		return n.Remark
	}
	if n.Hostname != "" {
		return n.Hostname
	}
	return n.NodeID
}

// BuildSummary 汇总统计（对传入的已裁剪节点集计算，避免把不可见主机算进去）。
func BuildSummary(nodes []*Node, updatedAt int64) *Summary {
	s := &Summary{Total: len(nodes), UpdatedAt: updatedAt, ServerTime: time.Now().Unix()}
	if len(nodes) == 0 {
		return s
	}
	for _, n := range nodes {
		if n.IsOnline() {
			s.Online++
		} else {
			s.Offline++
		}
		s.AvgCPU += n.CPUUsage
		s.AvgMem += n.MemUsage
		s.AvgDisk += n.DiskUsage
		s.TotalMem += n.MemTotalBytes
		s.UsedMem += n.MemUsedBytes
		s.TotalDisk += n.DiskTotalBytes
		s.UsedDisk += n.DiskUsedBytes
		s.NetRateIn += nonNegative(n.NetRateIn)
		s.NetRateOut += nonNegative(n.NetRateOut)
		s.NetTotalIn += nonNegative(n.NetTotalIn)
		s.NetTotalOut += nonNegative(n.NetTotalOut)
	}
	cnt := float64(len(nodes))
	s.AvgCPU = round1(s.AvgCPU / cnt)
	s.AvgMem = round1(s.AvgMem / cnt)
	s.AvgDisk = round1(s.AvgDisk / cnt)
	return s
}

// BuildSamples 把历史采样裁剪为公开趋势点。
//
// 只有开启对应字段才带上该序列；三个字段都没开时返回空切片
// （调用方据此判断"该主机无可公开的历史数据"）。
func BuildSamples(history []*protocol.ProbeDataReq, cfg *siteconfig.Config) []Sample {
	if len(history) == 0 {
		return []Sample{}
	}
	cpu := cfg.FieldVisible("cpu_usage")
	mem := cfg.FieldVisible("mem_usage")
	disk := cfg.FieldVisible("disk_usage")
	net := cfg.FieldVisible("network")
	out := make([]Sample, 0, len(history))
	var rates *[2][]float64
	if net {
		rates = sampleNetRates(history)
	}
	for i, p := range history {
		if p == nil {
			continue
		}
		s := Sample{TS: p.Timestamp}
		if cpu {
			v := round1(p.CPUUsage)
			s.CPUUsage = &v
		}
		if mem {
			v := round1(p.MemUsage)
			s.MemUsage = &v
		}
		if disk {
			v := round1(p.DiskUsage)
			s.DiskUsage = &v
		}
		if net && rates != nil {
			if v := rates[0][i]; v >= 0 {
				s.NetInBps = &v
			}
			if v := rates[1][i]; v >= 0 {
				s.NetOutBps = &v
			}
		}
		out = append(out, s)
	}
	return out
}

// sampleNetRates 由**累计字节**序列求各点的速率（字节/秒）。
//
// 为什么要在服务端算：Agent 上报的是网卡自启动以来的累计收发字节，
// 直接下发等于把"这台机器总共跑了多少流量"公开出去；而取相邻点差值/时间差
// 得到的瞬时速率既满足趋势图需求，也天然过滤了累计量级信息。
//
// 首点没有前驱（无法求差），以及时间戳重复/倒退、计数器回绕的点，
// 统一返回 -1 表示"该点无速率"，前端跳过而不是连成一条假的水平线。
func sampleNetRates(history []*protocol.ProbeDataReq) *[2][]float64 {
	in := make([]float64, len(history))
	out := make([]float64, len(history))
	for i := range history {
		in[i], out[i] = -1, -1
	}
	for i := 1; i < len(history); i++ {
		prev, cur := history[i-1], history[i]
		if prev == nil || cur == nil || cur.Timestamp <= prev.Timestamp {
			continue
		}
		dt := float64(cur.Timestamp - prev.Timestamp)
		in[i] = rateBetween(prev.NetInBytes, cur.NetInBytes, dt)
		out[i] = rateBetween(prev.NetOutBytes, cur.NetOutBytes, dt)
	}
	return &[2][]float64{in, out}
}

// rateBetween 计算两点之间的字节速率；计数器回绕/跳变/异常量级时返回 -1。
func rateBetween(prev, cur int64, dt float64) float64 {
	if dt <= 0 || cur < prev || prev < 0 || cur < 0 {
		return -1
	}
	rate := float64(cur-prev) / dt
	if rate > byteRateMax {
		return -1
	}
	return round1(rate)
}

// nonNegative 负值归零（计数器回绕、差分异常等）。
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// round2 保留 2 位小数（负载量级小，需要更高精度）。
func round2(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return float64(int64(v*100+0.5)) / 100
}

// round1 保留 1 位小数，避免公开页出现浮点尾巴。
func round1(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return float64(int64(v*10+0.5)) / 10
}

// trimFloat 去掉数字多余的 0（2.40 -> 2.4，3.00 -> 3）。
func trimFloat(v float64) string {
	s := strings.TrimRight(strings.TrimRight(toString2(v), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s
}

// toString2 两位小数格式化（不引入 strconv 之外的行为差异）。
func toString2(v float64) string {
	i := int64(v*100 + 0.5)
	whole := i / 100
	frac := i % 100
	ws := intToString(whole)
	if frac == 0 {
		return ws + ".00"
	}
	fs := intToString(frac)
	if frac < 10 {
		fs = "0" + fs
	}
	return ws + "." + fs
}

func intToString(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
