// Package siteconfig 管理「公开站点展示配置」。
//
// 背景（对应 Issue #5）：
//
//	前端首页此前是登录页 —— 未登录访客看不到任何信息，运维想给同事/甲方看
//	"有哪些机器、负载如何"只能逐个截图。本包提供一套**白名单式**的展示配置：
//
//	  - public_enabled：总开关，关闭后公开页一律不可见（默认关闭）；
//	  - hosts_enabled：主页是否展示主机条目；
//	  - catalog_enabled：是否允许展示整个「主机目录」（即所有主机卡片/列表）；
//	  - host_ids：主机白名单，留空 = 全部主机，非空 = 只展示列出的主机；
//	  - fields：逐字段开关（CPU/内存/磁盘/地区…）。
//
// 安全基线（这是本包的核心约束，任何改动都不能放松）：
//
//  1. 只输出 `internal/publicview` 构造的**脱敏视图**结构体，绝不把
//     protocol.AgentInfo 直接序列化出去 —— 后者含 agent_id、uuid、
//     remote_addr(内网 IP) 等可被用于横向渗透的字段。
//  2. 未在 fields 中显式开启的字段，在结构体层面就是 nil / 零值
//     （omitempty 后从 JSON 中消失），而不是"前端不渲染"。
//     前端隐藏只是 UI，接口层面必须没有数据。
//  3. 默认全关闭：升级安装不会因为本功能把内网主机信息对外暴露。
package siteconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Field 一个可单独开关的展示字段。
//
// Default 表示该字段在「总开关刚打开、用户尚未逐项配置」时的默认可见性。
// 只有明确属于「非敏感」范畴的字段才允许 Default=true。
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Group       string `json:"group"`
	Default     bool   `json:"default"`
	Sensitive   bool   `json:"sensitive"`   // 敏感字段：可显式开启，但前端需提示风险
	Description string `json:"description"` // 字段口径说明
}

// FieldCatalog 全部可配置字段（顺序即前端展示顺序）。
//
// agent_id / uuid / remote_addr / net_* 不在目录中：它们或被用作对外标识
// （agent_id 是命令下发的句柄），或直接暴露内网拓扑与机器指纹，属于固有
// 不公开字段，不提供开关。
var FieldCatalog = []Field{
	{Key: "remark", Label: "备注名", Group: "主机", Default: true, Description: "运维填写的主机备注"},
	{Key: "hostname", Label: "主机名", Group: "主机", Default: false, Sensitive: true, Description: "可能包含内网命名规范/业务信息"},
	{Key: "node_id", Label: "节点编号", Group: "主机", Default: true, Description: "随机节点标识（n-xxxxxxxx），与 agent_id 无对应关系"},
	{Key: "online", Label: "在线状态", Group: "主机", Default: true, Description: "在线 / 离线"},
	{Key: "last_seen", Label: "最后在线时间", Group: "主机", Default: true, Description: "最近一次心跳时间"},
	{Key: "uptime", Label: "运行时长", Group: "主机", Default: true, Description: "系统已运行时长"},

	{Key: "os_type", Label: "操作系统类型", Group: "系统", Default: true, Description: "Linux / Windows"},
	{Key: "os_version", Label: "系统版本", Group: "系统", Default: false, Sensitive: true, Description: "如 Ubuntu 22.04（可被用于匹配已知漏洞）"},
	{Key: "arch", Label: "架构", Group: "系统", Default: true, Description: "x86_64 / arm64"},
	{Key: "kernel_version", Label: "内核版本", Group: "系统", Default: false, Sensitive: true, Description: "精确内核版本属于漏洞指纹"},

	{Key: "geo", Label: "地区", Group: "网络", Default: true, Description: "国家 / 省份 / 城市"},
	{Key: "network", Label: "网络流量", Group: "网络", Default: true, Description: "实时上下行速率与自启动累计流量（README 中的站点核心指标）"},
	{Key: "public_ip", Label: "公网 IP", Group: "网络", Default: false, Sensitive: true, Description: "公网出口 IP（暴露资产归属）"},
	{Key: "public_ipv6", Label: "公网 IPv6", Group: "网络", Default: false, Sensitive: true, Description: "公网 IPv6 出口"},

	{Key: "cpu_usage", Label: "CPU 利用率", Group: "性能", Default: true, Description: "当前 CPU 使用率"},
	{Key: "cpu_model", Label: "CPU 型号", Group: "性能", Default: true, Description: "CPU 型号与主频"},
	{Key: "mem_usage", Label: "内存利用率", Group: "性能", Default: true, Description: "已用/总内存与百分比"},
	{Key: "disk_usage", Label: "磁盘利用率", Group: "性能", Default: true, Description: "已用/总磁盘与百分比"},
}

// catalogByKey 字段索引（Key -> Field）。
var catalogByKey = func() map[string]Field {
	m := make(map[string]Field, len(FieldCatalog))
	for _, f := range FieldCatalog {
		m[f.Key] = f
	}
	return m
}()

// IsKnownField 判断字段 key 是否在目录中。
func IsKnownField(key string) bool {
	_, ok := catalogByKey[key]
	return ok
}

// FieldDef 返回字段定义。
func FieldDef(key string) (Field, bool) {
	f, ok := catalogByKey[key]
	return f, ok
}

// DefaultFields 返回默认可见字段集合（目录中 Default=true 的字段）。
func DefaultFields() map[string]bool {
	m := make(map[string]bool, len(FieldCatalog))
	for _, f := range FieldCatalog {
		if f.Default {
			m[f.Key] = true
		}
	}
	return m
}

// 流量统计口径。
const (
	// TrafficModeBoot 自设备启动以来的累计流量（网卡计数器，来自当前探针）。
	TrafficModeBoot = "boot"
	// TrafficModeWindow 历史留存窗口内的累计流量（各采样点增量之和）。
	TrafficModeWindow = "window"
)

// NormalizeTrafficMode 归一化流量口径，非法值回退 boot（有值且无需查库）。
func NormalizeTrafficMode(v string) string {
	if trimSpace(v) == TrafficModeWindow {
		return TrafficModeWindow
	}
	return TrafficModeBoot
}

// Config 公开站点展示配置。
type Config struct {
	// Enabled 总开关（**默认关闭**）：关闭时公开页 404，接口返回 enabled=false。
	Enabled bool `json:"public_enabled"`
	// HostsEnabled 主页是否展示主机条目（关闭后只剩站点级汇总指标）。
	HostsEnabled bool `json:"hosts_enabled"`
	// CatalogEnabled 是否展示整个主机目录。关闭 + HostIDs 非空 = 只公开挑选出来的主机。
	CatalogEnabled bool `json:"catalog_enabled"`
	// HostIDs 公开主机白名单（agent_id）。仅在 CatalogEnabled=false 且非空时生效。
	HostIDs []string `json:"host_ids"`
	// Fields 逐字段开关。未出现的 key 视为**关闭**（只在默认配置里预置 Default 字段）。
	Fields map[string]bool `json:"fields"`
	// Title / Description 公开页标题与说明（便于区分"对内部/对外"两种场景）。
	Title       string `json:"title"`
	Description string `json:"description"`

	// TrafficMode 流量统计口径：boot（自设备启动） / window（历史留存窗口内）。
	// 直连连接类主机（HTTPS/流媒体/代理）长期累计流量很大，对外开放时更适合用
	// window 口径，避免暴露总承载量。
	TrafficMode string `json:"traffic_mode"`
}

// TrafficModeOrDefault 返回生效的流量口径（空值按 boot 处理）。
func (c *Config) TrafficModeOrDefault() string {
	if c == nil {
		return TrafficModeBoot
	}
	return NormalizeTrafficMode(c.TrafficMode)
}

// Default 返回默认配置：总开关关闭，字段取目录默认值。
func Default() *Config {
	return &Config{
		Enabled:        false,
		HostsEnabled:   true,
		CatalogEnabled: true,
		HostIDs:        []string{},
		Fields:         DefaultFields(),
		Title:          "MoCenter 节点状态",
		Description:    "本页面由 MoCenter 公开站点提供，仅展示非敏感运行指标。",
		TrafficMode:    TrafficModeBoot,
	}
}

// normalize 修正非法/越界取值，保证内存中的配置始终是"可用且安全"的。
//
// 关键点：fields 会被**重建**为「目录中全部 key 的显式布尔值」，
// 这样未知 key（手工编辑配置文件塞进来的）不会留在配置里，
// 前端拿到的也是一份完整、可遍历的字段表。
func (c *Config) normalize() {
	if c == nil {
		return
	}
	if c.Fields == nil {
		c.Fields = map[string]bool{}
	}
	fields := make(map[string]bool, len(FieldCatalog))
	for _, f := range FieldCatalog {
		fields[f.Key] = c.Fields[f.Key]
	}
	c.Fields = fields

	ids := make([]string, 0, len(c.HostIDs))
	for _, id := range c.HostIDs {
		if id = trimSpace(id); id != "" && len(id) <= 128 {
			ids = append(ids, id)
		}
	}
	c.HostIDs = ids
	c.TrafficMode = NormalizeTrafficMode(c.TrafficMode)
}

// FieldVisible 判断字段是否对公开页可见。
func (c *Config) FieldVisible(key string) bool {
	if c == nil || c.Fields == nil {
		return false
	}
	return c.Fields[key]
}

// HostAllowed 判断某主机是否允许出现在公开页。
//
// 规则：总开关关闭 -> 一律不允许；catalog 打开 -> 全部允许；
// 否则只允许 HostIDs 里列出的主机（白名单模式，默认拒绝）。
func (c *Config) HostAllowed(agentID string) bool {
	if c == nil || !c.Enabled || !c.HostsEnabled {
		return false
	}
	if c.CatalogEnabled {
		return true
	}
	for _, id := range c.HostIDs {
		if id == agentID {
			return true
		}
	}
	return false
}

// Store 配置存储（JSON 落盘，读写加锁）。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
	// onChange 变更回调（由 server 注入，用于把新配置广播给公开页 WS 订阅者）
	onChange func(*Config)
}

// NewStore 创建配置存储。path 为空时仅内存生效（测试用）。
func NewStore(path string) *Store {
	return &Store{path: path, cfg: Default()}
}

// SetOnChange 注册配置变更回调。
func (s *Store) SetOnChange(fn func(*Config)) { s.onChange = fn }

// Load 从磁盘加载配置；文件不存在或损坏时保持默认值（**默认关闭**，安全侧兜底）。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg := Default()
	if err := json.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("解析公开站点配置 %s 失败: %w", s.path, err)
	}
	cfg.normalize()
	s.cfg = cfg
	return nil
}

// Get 返回配置副本（调用方拿到的对象不会被内部修改影响）。
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

// Set 全量更新配置并落盘。
func (s *Store) Set(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("配置不能为空")
	}
	cfg.normalize()
	s.mu.Lock()
	s.cfg = cfg.clone()
	path := s.path
	onChange := s.onChange
	s.mu.Unlock()

	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		// 0600：配置本身不含秘密，但属于服务运行文件，收紧无副作用
		if err := os.WriteFile(path, b, 0600); err != nil {
			return err
		}
	}
	if onChange != nil {
		onChange(cfg.clone())
	}
	return nil
}

// clone 深拷贝配置（含 map/slice），避免调用方与存储共享可变状态。
func (c *Config) clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.HostIDs = append([]string{}, c.HostIDs...)
	if c.Fields != nil {
		out.Fields = make(map[string]bool, len(c.Fields))
		for k, v := range c.Fields {
			out.Fields[k] = v
		}
	}
	return &out
}

// trimSpace 内联 strings.TrimSpace，避免为一处调用引入依赖心智。
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
