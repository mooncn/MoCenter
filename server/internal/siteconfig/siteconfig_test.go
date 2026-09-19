package siteconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 默认配置必须关闭总开关：升级安装不能因为引入本功能而对外暴露主机信息。
func TestDefaultDisabled(t *testing.T) {
	c := Default()
	if c.Enabled {
		t.Fatal("默认配置必须关闭公开展示")
	}
	if Default().clone() == nil {
		t.Fatal("clone 不应返回 nil")
	}
	for _, key := range []string{"cpu_usage", "mem_usage", "disk_usage", "geo", "remark"} {
		if !c.FieldVisible(key) {
			t.Errorf("字段 %s 应默认可见（非敏感）", key)
		}
	}
	// 敏感字段必须默认关闭
	for _, key := range []string{"hostname", "public_ip", "public_ipv6", "os_version", "kernel_version"} {
		if c.FieldVisible(key) {
			t.Errorf("敏感字段 %s 不应默认可见", key)
		}
	}
}

// 配置持久化：保存后重新 Load 必须完全一致，且 fields 被重建为完整表。
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "public-site.json")
	st := NewStore(path)
	if err := st.Load(); err != nil {
		t.Fatalf("Load 空文件不应报错: %v", err)
	}
	cfg := Default()
	cfg.Enabled = true
	cfg.CatalogEnabled = false
	cfg.HostIDs = []string{" host-a ", "", "host-b"}
	cfg.Fields["hostname"] = true
	cfg.Fields["not-a-field"] = true // 未知 key 应被丢弃
	if err := st.Set(cfg); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("配置文件未落盘: %v", err)
	}

	re := NewStore(path)
	if err := re.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	got := re.Get()
	if !got.Enabled || got.CatalogEnabled {
		t.Fatalf("开关未正确恢复: enabled=%v catalog=%v", got.Enabled, got.CatalogEnabled)
	}
	if len(got.HostIDs) != 2 || got.HostIDs[0] != "host-a" || got.HostIDs[1] != "host-b" {
		t.Fatalf("HostIDs 未归一化: %#v", got.HostIDs)
	}
	if !got.FieldVisible("hostname") {
		t.Fatal("hostname 开关未恢复")
	}
	if _, ok := got.Fields["not-a-field"]; ok {
		t.Fatal("未知字段 key 不应被保留")
	}
	if len(got.Fields) != len(FieldCatalog) {
		t.Fatalf("fields 应被重建为完整目录，实际 %d 项", len(got.Fields))
	}
}

// 主机可见性：catalog 模式 / 白名单模式 / 关闭状态。
func TestHostAllowed(t *testing.T) {
	c := Default()
	if c.HostAllowed("any") {
		t.Fatal("总开关关闭时不应有任何主机可见")
	}

	c.Enabled = true
	c.CatalogEnabled = true
	if !c.HostAllowed("any") {
		t.Fatal("目录模式应允许全部主机")
	}

	c.CatalogEnabled = false
	c.HostIDs = []string{"host-a"}
	if !c.HostAllowed("host-a") {
		t.Fatal("白名单内的主机应可见")
	}
	if c.HostAllowed("host-b") {
		t.Fatal("白名单外的主机不应可见")
	}

	// 主页不展示主机时，即便总开关打开也不允许
	c.HostsEnabled = false
	if c.HostAllowed("host-a") {
		t.Fatal("hosts_enabled 关闭时不应有主机可见")
	}
}

// 配置副本隔离：Get 返回的对象被外部修改不能污染内部状态。
func TestGetReturnsCopy(t *testing.T) {
	st := NewStore("")
	cfg := st.Get()
	cfg.Enabled = true
	cfg.Fields["hostname"] = true
	cfg.HostIDs = append(cfg.HostIDs, "x")

	if st.Get().Enabled {
		t.Fatal("Get 返回的副本修改影响了内部状态")
	}
	if st.Get().FieldVisible("hostname") {
		t.Fatal("fields map 未深拷贝")
	}
	if len(st.Get().HostIDs) != 0 {
		t.Fatal("HostIDs 未深拷贝")
	}
}

// 变更回调：配置保存后必须触发，供服务端清理/重算缓存。
func TestOnChange(t *testing.T) {
	st := NewStore("")
	var got *Config
	st.SetOnChange(func(c *Config) { got = c })
	cfg := Default()
	cfg.Enabled = true
	if err := st.Set(cfg); err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Enabled {
		t.Fatal("变更回调未收到新配置")
	}
}

// 网络流量字段默认可见（README 明确为站点核心指标），且能持久化往返。
func TestNetworkFieldAndTrafficMode(t *testing.T) {
	c := Default()
	if !c.FieldVisible("network") {
		t.Fatal("network 应默认可见")
	}
	if c.TrafficModeOrDefault() != TrafficModeBoot {
		t.Fatalf("默认流量口径应为 boot，实际 %q", c.TrafficModeOrDefault())
	}

	// 非法口径回退 boot（有值且无需查库，避免配置写错导致公开页流量整列空白）
	for _, bad := range []string{"", "  ", "BOOT", "sinceboot", "全部"} {
		if got := NormalizeTrafficMode(bad); got != TrafficModeBoot {
			t.Errorf("NormalizeTrafficMode(%q) = %q，应回退 boot", bad, got)
		}
	}
	if got := NormalizeTrafficMode("window"); got != TrafficModeWindow {
		t.Errorf("window 未被识别: %q", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "public-site.json")
	st := NewStore(path)
	cfg := Default()
	cfg.Enabled = true
	cfg.TrafficMode = TrafficModeWindow
	if err := st.Set(cfg); err != nil {
		t.Fatal(err)
	}
	re := NewStore(path)
	if err := re.Load(); err != nil {
		t.Fatal(err)
	}
	if re.Get().TrafficMode != TrafficModeWindow {
		t.Fatalf("流量口径未持久化: %q", re.Get().TrafficMode)
	}
}

// TestLoadFieldInCatalog 负载是性能分组的可配置字段，且默认可见。
func TestLoadFieldInCatalog(t *testing.T) {
	f, ok := FieldDef("load")
	if !ok {
		t.Fatal("字段目录应包含 load")
	}
	if f.Group != "性能" || !f.Default || f.Sensitive {
		t.Fatalf("负载字段定义异常: %+v", f)
	}
	if !DefaultFields()["load"] {
		t.Fatal("默认配置应包含负载")
	}
	// 默认配置经 normalize 后必须显式带上该键（前端据此渲染开关）
	cfg := Default()
	if _, ok := cfg.Fields["load"]; !ok {
		t.Fatal("默认配置缺少 load 键")
	}
}

// 站点品牌文案（标题 / 页脚）默认值与回退规则。
func TestSiteBrandingDefaults(t *testing.T) {
	c := Default()
	if c.SiteTitle != DefaultSiteTitle {
		t.Fatalf("默认站点标题应为 %q，实际 %q", DefaultSiteTitle, c.SiteTitle)
	}
	if c.SiteFooter != DefaultSiteFooter {
		t.Fatalf("默认页脚应为 %q，实际 %q", DefaultSiteFooter, c.SiteFooter)
	}
	// 默认页脚使用 {year} 占位符，前端按当前年份替换
	if !strings.Contains(DefaultSiteFooter, FooterYearToken) {
		t.Fatalf("默认页脚应含年份占位符 %q", FooterYearToken)
	}
	if c.SiteTitleOrDefault() != DefaultSiteTitle || c.SiteFooterOrDefault() != DefaultSiteFooter {
		t.Fatal("空配置时 OrDefault 应回退默认值")
	}
}

// 空值与纯空白都必须回退默认值（否则页脚会渲染成空白区块 / 标签页无标题）。
func TestSiteBrandingFallback(t *testing.T) {
	c := Default()
	c.SiteTitle = "   "
	c.SiteFooter = "\n\t "
	c.normalize()
	if c.SiteTitleOrDefault() != DefaultSiteTitle {
		t.Errorf("空白标题应回退默认，实际 %q", c.SiteTitleOrDefault())
	}
	if c.SiteFooterOrDefault() != DefaultSiteFooter {
		t.Errorf("空白页脚应回退默认，实际 %q", c.SiteFooterOrDefault())
	}
	var nilCfg *Config
	if nilCfg.SiteTitleOrDefault() != DefaultSiteTitle || nilCfg.SiteFooterOrDefault() != DefaultSiteFooter {
		t.Error("nil 配置也应回退默认值（避免公开页 panic 路径）")
	}
}

// 自定义品牌文案必须能持久化往返，且页脚 HTML 原样保留。
func TestSiteBrandingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "public-site.json"))
	cfg := Default()
	cfg.Enabled = true
	cfg.SiteTitle = "内部节点状态"
	cfg.SiteFooter = `<a href="https://example.com">©2025 MOONCN</a>`
	if err := st.Set(cfg); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	got := NewStore(filepath.Join(dir, "public-site.json"))
	if err := got.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	g := got.Get()
	if g.SiteTitle != cfg.SiteTitle {
		t.Errorf("标题未往返：want %q got %q", cfg.SiteTitle, g.SiteTitle)
	}
	if g.SiteFooter != cfg.SiteFooter {
		t.Errorf("页脚 HTML 应原样保留：want %q got %q", cfg.SiteFooter, g.SiteFooter)
	}
}

// 超长品牌文案在 normalize 阶段被截断，且不会把多字节字符切成乱码。
func TestSiteBrandingLengthCap(t *testing.T) {
	c := Default()
	c.SiteTitle = strings.Repeat("中", maxSiteTitleLen+50)
	c.SiteFooter = strings.Repeat("a", maxSiteFooterLen+100)
	c.normalize()
	if n := len([]rune(c.SiteTitle)); n != maxSiteTitleLen {
		t.Errorf("标题应被截断到 %d 字符，实际 %d", maxSiteTitleLen, n)
	}
	if len(c.SiteFooter) != maxSiteFooterLen {
		t.Errorf("页脚应被截断到 %d 字节，实际 %d", maxSiteFooterLen, len(c.SiteFooter))
	}
	// 截断后仍是合法 UTF-8（否则前端显示为乱码方块）
	if !utf8.ValidString(c.SiteTitle) {
		t.Error("标题截断后应是合法 UTF-8")
	}
}

// 品牌文案变更也要触发 onChange（公开页 WS 订阅者据此下发新标题/页脚）。
func TestSiteBrandingOnChange(t *testing.T) {
	st := NewStore("")
	got := make(chan string, 1)
	st.SetOnChange(func(c *Config) { got <- c.SiteFooter })
	cfg := Default()
	cfg.SiteFooter = "自定义页脚"
	if err := st.Set(cfg); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	select {
	case v := <-got:
		if v != "自定义页脚" {
			t.Errorf("回调收到 %q，want 自定义页脚", v)
		}
	default:
		t.Fatal("变更回调未被触发")
	}
}
