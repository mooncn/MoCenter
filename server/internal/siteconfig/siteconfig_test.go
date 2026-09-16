package siteconfig

import (
	"os"
	"path/filepath"
	"testing"
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
