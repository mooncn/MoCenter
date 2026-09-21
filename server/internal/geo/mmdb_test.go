package geo

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

// buildTestMMDB 生成一个最小的 GeoLite2-Country 测试库。
//
// 为什么在测试里**生成**库而不是提交一个二进制 fixture：
//   - 二进制文件进仓库会随版本膨胀，且无法 code review；
//   - 生成式 fixture 让"库结构"这件事在测试里显式可见，
//     一旦上游结构变化（字段名改了），这里会立刻失败而不是静默不命中。
func buildTestMMDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-country.mmdb")

	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "GeoLite2-Country",
		RecordSize:   24,
	})
	if err != nil {
		t.Fatalf("创建测试库失败: %v", err)
	}
	_, network, err := net.ParseCIDR("1.2.3.0/24")
	if err != nil {
		t.Fatalf("解析 CIDR 失败: %v", err)
	}
	rec := mmdbtype.Map{
		"country": mmdbtype.Map{
			"iso_code": mmdbtype.String("CN"),
			"names": mmdbtype.Map{
				"en":    mmdbtype.String("China"),
				"zh-CN": mmdbtype.String("中国"),
			},
		},
		"subdivisions": mmdbtype.Slice{
			mmdbtype.Map{
				"names": mmdbtype.Map{
					"en":    mmdbtype.String("Guangdong"),
					"zh-CN": mmdbtype.String("广东"),
				},
			},
		},
		"city": mmdbtype.Map{
			"names": mmdbtype.Map{
				"en":    mmdbtype.String("Guangzhou"),
				"zh-CN": mmdbtype.String("广州"),
			},
		},
		"location": mmdbtype.Map{
			"latitude":  mmdbtype.Float64(23.12),
			"longitude": mmdbtype.Float64(113.25),
		},
	}
	if err := w.Insert(network, rec); err != nil {
		t.Fatalf("写入测试记录失败: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	if _, err := w.WriteTo(f); err != nil {
		f.Close()
		t.Fatalf("写出测试库失败: %v", err)
	}
	f.Close()
	return path
}

// TestMMDBLookup 离线库命中时应给出国家代码/省/市，且不消耗任何网络请求。
func TestMMDBLookup(t *testing.T) {
	path := buildTestMMDB(t)
	p, err := NewMMDBProvider(path)
	if err != nil {
		t.Fatalf("加载测试库失败: %v", err)
	}
	defer p.Close()
	if !p.Loaded() {
		t.Fatal("库应处于已加载状态")
	}
	g := p.Lookup("1.2.3.4")
	if g == nil {
		t.Fatal("应命中 1.2.3.4")
	}
	if g.CountryCode != "CN" {
		t.Fatalf("国家代码应为 CN，实际 %q", g.CountryCode)
	}
	// 国家名统一为英文：normalizeGeo 会把中文国名规范成英文，
	// 否则同一批节点会因为数据源口径不同（"中国" vs "China"）
	// 在页面上看起来像两个国家。中文的省/市则保持原样。
	if g.Country != "China" {
		t.Fatalf("国家名应被规范为英文 China，实际 %q", g.Country)
	}
	if g.Region != "广东" || g.City != "广州" {
		t.Fatalf("省/市不正确: region=%q city=%q", g.Region, g.City)
	}
}

// TestMMDBNotFound 未命中的地址返回 nil（交由下一个源），而不是返回空结构体。
//
// 这条很关键：返回空结构体会让上层认为"解析成功但国家为空"，
// 从而**终止降级**，外部源再也没机会补上 —— 表现为地区永久为空。
func TestMMDBNotFound(t *testing.T) {
	path := buildTestMMDB(t)
	p, err := NewMMDBProvider(path)
	if err != nil {
		t.Fatalf("加载测试库失败: %v", err)
	}
	defer p.Close()
	if g := p.Lookup("8.8.8.8"); g != nil {
		t.Fatalf("未命中的地址应返回 nil，实际 %+v", g)
	}
	if g := p.Lookup("not-an-ip"); g != nil {
		t.Fatalf("非法地址应返回 nil，实际 %+v", g)
	}
}

// TestMMDBOptional 未配置/路径不存在时**不报错**，只是不启用。
//
// 离线库是可选增强：缺了它必须优雅降级到外部 HTTP 源，
// 而不是让服务端启动失败（那会让"忘了放库文件"直接变成一次故障）。
func TestMMDBOptional(t *testing.T) {
	p, err := NewMMDBProvider("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if p.Loaded() {
		t.Fatal("空路径不应处于已加载状态")
	}
	if g := p.Lookup("1.1.1.1"); g != nil {
		t.Fatal("未加载时查询应返回 nil")
	}

	// 路径不存在：返回错误（供启动日志提示），但对象可用且不影响查询
	p2, err := NewMMDBProvider(filepath.Join(t.TempDir(), "nope.mmdb"))
	if err == nil {
		t.Fatal("不存在的文件应返回错误，便于运维从日志发现配置问题")
	}
	if p2.Loaded() {
		t.Fatal("加载失败时不应标记为已加载")
	}
	if g := p2.Lookup("1.1.1.1"); g != nil {
		t.Fatal("加载失败时查询应返回 nil")
	}
}

// TestResolverMMDBFirst 配了离线库时，解析器应**优先**走本地库，
// 且不发起任何外部 HTTP 请求。
func TestResolverMMDBFirst(t *testing.T) {
	path := buildTestMMDB(t)
	r := NewResolver()
	// 把外部源全部指向一个必然失败的地址：若离线库未优先生效，
	// 就会走到外部源并返回 nil（而不是命中本地库的结果）。
	r.ipingEndpoint = "http://127.0.0.1:1/ip=%s"
	r.ipapiEndpoint = "http://127.0.0.1:1/ip=%s"
	r.ipwhoEndpoint = "http://127.0.0.1:1/%s"
	if err := r.SetMMDB(path); err != nil {
		t.Fatalf("挂载离线库失败: %v", err)
	}
	t.Cleanup(r.mmdb.Close)
	if !r.MMDBLoaded() {
		t.Fatal("解析器应报告离线库已加载")
	}
	g := r.Lookup("1.2.3.4")
	if g == nil || g.CountryCode != "CN" {
		t.Fatalf("应命中离线库返回 CN，实际 %+v", g)
	}
}

// TestResolverMMDBFallback 离线库未命中时，仍应降级到外部源。
//
// 这是"离线库存量不全"（如只有 Country 库、或数据未覆盖该段）时的关键行为：
// 不能因为配了库就让未命中的节点永久没有地区。
func TestResolverMMDBFallback(t *testing.T) {
	path := buildTestMMDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","country":"Japan","countryCode":"JP","regionName":"Tokyo","city":"Tokyo","lat":35.6,"lon":139.7,"query":"9.9.9.9"}`))
	}))
	defer upstream.Close()
	r := NewResolver()
	r.endpoint = upstream.URL + "/%s"
	r.ipingEndpoint = "http://127.0.0.1:1/ip=%s"
	r.ipwhoEndpoint = "http://127.0.0.1:1/%s"
	if err := r.SetMMDB(path); err != nil {
		t.Fatalf("挂载离线库失败: %v", err)
	}
	t.Cleanup(r.mmdb.Close)
	// 9.9.9.9 不在测试库中
	g := r.Lookup("9.9.9.9")
	if g == nil || g.CountryCode != "JP" {
		t.Fatalf("离线库未命中时应降级到外部源并返回 JP，实际 %+v", g)
	}
}

// TestMMDBReaderConcurrent 离线库查询必须并发安全（多台 Agent 同时上线）。
// 用 -race 跑这个用例才有意义（CI 里已开启）。
func TestMMDBReaderConcurrent(t *testing.T) {
	path := buildTestMMDB(t)
	p, err := NewMMDBProvider(path)
	if err != nil {
		t.Fatalf("加载测试库失败: %v", err)
	}
	defer p.Close()
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = p.Lookup("1.2.3.4")
				_ = p.Lookup("8.8.8.8")
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// 保底：确保 mmdbwriter 确实可用（导入被用上，且生成物能被 Open 读回）。
func TestTestFixtureReadable(t *testing.T) {
	path := buildTestMMDB(t)
	r, err := maxminddb.Open(path)
	if err != nil {
		t.Fatalf("生成的测试库无法打开: %v", err)
	}
	defer r.Close()
	if r.Metadata.DatabaseType != "GeoLite2-Country" {
		t.Fatalf("库类型不正确: %s", r.Metadata.DatabaseType)
	}
}
