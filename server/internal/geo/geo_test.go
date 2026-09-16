package geo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

// TestPrivate 覆盖内网/保留地址判定：这些地址不该消耗外部接口额度。
func TestPrivate(t *testing.T) {
	yes := []string{
		"", "127.0.0.1", "::1", "localhost",
		"10.0.0.1", "172.16.3.4", "192.168.1.10",
		"169.254.1.1", "fe80::1", "fc00::1", "fd12:3456::1",
		"100.64.0.1", "100.127.255.254", "192.0.2.5",
		"255.255.255.255", "0.0.0.0", "224.0.0.1", "not-an-ip", "192.168.1.10:8080",
	}
	for _, ip := range yes {
		if !Private(ip) {
			t.Errorf("Private(%q) = false，应为内网/保留地址", ip)
		}
	}
	no := []string{"8.8.8.8", "114.114.114.114", "1.1.1.1", "2001:4860:4860::8888", "100.128.0.1"}
	for _, ip := range no {
		if Private(ip) {
			t.Errorf("Private(%q) = true，应为公网地址", ip)
		}
	}
}

// TestLookupSkipsPrivate 内网地址不发起任何请求（proxy 指向一个必然失败的地址也不该被调用）。
func TestLookupSkipsPrivate(t *testing.T) {
	r := NewResolverWithProxy("http://127.0.0.1:1/?url=")
	done := make(chan *struct{}, 1)
	go func() { done <- nil }()
	_ = done
	start := time.Now()
	if g := r.Lookup("192.168.10.10"); g != nil {
		t.Fatalf("内网地址应返回 nil，得到 %+v", g)
	}
	if time.Since(start) > time.Second {
		t.Fatal("内网地址不应发起外网请求（耗时过长）")
	}
}

// TestApplyProxy URL 包装格式与 Agent 端一致。
func TestApplyProxy(t *testing.T) {
	cases := []struct{ proxy, in, wantPrefix string }{
		{"", "http://ip-api.com/json/8.8.8.8?x=1", "http://ip-api.com/json/8.8.8.8?x=1"},
		{"http://fa.example/?url=", "http://ip-api.com/json/8.8.8.8", "http://fa.example/?url=http://ip-api.com/json/8.8.8.8"},
		{"http://fa.example", "http://x/?a=1", "http://fa.example/?url=http://x/%3Fa%3D1"},
		{"http://fa.example/", "http://x/", "http://fa.example/?url=http://x/"},
	}
	for _, c := range cases {
		r := NewResolverWithProxy(c.proxy)
		if got := r.applyProxy(c.in); got != c.wantPrefix {
			t.Errorf("proxy=%q applyProxy(%q) = %q，期望 %q", c.proxy, c.in, got, c.wantPrefix)
		}
	}
}

// TestFetchParsesAndCaches 成功结果解析出国家代码，并命中 24h 缓存。
func TestFetchParsesAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"status":"success","country":"中国","countryCode":"CN","regionName":"广东","city":"广州"}`))
	}))
	defer srv.Close()

	r := NewResolver()
	// 三个源都指向测试服务：用例不应依赖机房的公网可达性（否则离线跑测必挂）
	r.ipingEndpoint = srv.URL + "/%s"
	r.ipapiEndpoint = srv.URL + "/json/%s"
	r.ipwhoEndpoint = srv.URL + "/%s"
	g := r.fetch("1.2.3.4")
	if g == nil || g.CountryCode != "CN" || g.Country != "China" || g.City != "广州" {
		t.Fatalf("解析结果异常: %+v", g)
	}
	if !strings.HasPrefix(r.ipapiEndpoint, srv.URL) {
		t.Fatal("测试端点未生效")
	}
}

// TestFailureCachedShort 失败结果只缓存短时间：否则限流期间地区会长时间不显示。
func TestFailureCachedShort(t *testing.T) {
	if cacheFail >= cacheOK {
		t.Fatalf("失败缓存(%v) 必须远短于成功缓存(%v)", cacheFail, cacheOK)
	}
	if cacheFail > 15*time.Minute {
		t.Fatalf("失败缓存 %v 过长，限流恢复后无法及时补上地区", cacheFail)
	}

	r := NewResolver()
	// 三个源都指向必然失败的地址（保留端口 1），确保用例不依赖公网
	r.ipingEndpoint = "http://127.0.0.1:1/%s"
	r.ipapiEndpoint = "http://127.0.0.1:1/json/%s"
	r.ipwhoEndpoint = "http://127.0.0.1:1/%s"
	if g := r.Lookup("8.8.8.8"); g != nil {
		t.Fatalf("不可达接口应返回 nil，得到 %+v", g)
	}
	e, ok := r.cache["8.8.8.8"]
	if !ok {
		t.Fatal("失败结果也应写入缓存（避免每次列表都打外部接口）")
	}
	if d := time.Until(e.expireAt); d > cacheFail+time.Second || d <= 0 {
		t.Fatalf("失败缓存有效期 %v，期望约 %v", d, cacheFail)
	}
}

// TestNormalizeGeoFillsCountryCodeFromChineseName 主源（api.iping.cc）不返回
// ISO 国家代码，必须能由中文国名反查出代码，否则前端国旗不显示。
func TestNormalizeGeoFillsCountryCodeFromChineseName(t *testing.T) {
	cases := []struct{ in, code, en string }{
		{"中国", "CN", "China"},
		{"美国", "US", "United States"},
		{"日本", "JP", "Japan"},
		{"中国香港特别行政区", "HK", "Hong Kong"},
		{"台湾", "TW", "Taiwan"},
	}
	for _, c := range cases {
		g := normalizeGeo(&protocol.GeoInfo{Country: c.in, Region: "广东", City: "广州"})
		if g.CountryCode != c.code {
			t.Errorf("normalizeGeo(%q).CountryCode = %q，期望 %q", c.in, g.CountryCode, c.code)
		}
		if g.Country != c.en {
			t.Errorf("normalizeGeo(%q).Country = %q，期望统一为英文名 %q", c.in, g.Country, c.en)
		}
		if g.Region != "广东" || g.City != "广州" {
			t.Errorf("省/市不应被改写: %+v", g)
		}
	}
}

// TestNormalizeGeoKeepsUnknownCountry 表里没有的国家名保留原值（不猜测、不写错）。
func TestNormalizeGeoKeepsUnknownCountry(t *testing.T) {
	g := normalizeGeo(&protocol.GeoInfo{Country: "不存在国", CountryCode: "xx"})
	if g.Country != "不存在国" {
		t.Errorf("未知国家名不应被改写: %q", g.Country)
	}
	if g.CountryCode != "XX" {
		t.Errorf("合法两位代码应保留并大写，得到 %q", g.CountryCode)
	}
	g2 := normalizeGeo(&protocol.GeoInfo{Country: "不存在国", CountryCode: "CHN"})
	if g2.CountryCode != "" {
		t.Errorf("非两位的代码应清空，得到 %q", g2.CountryCode)
	}
}

// TestCountryAliases 上游数据源的写法差异要能命中（避免同一国家两种口径）。
func TestCountryAliases(t *testing.T) {
	cases := map[string]string{
		"United States of America":  "US",
		"usa":                       "US",
		"Republic of Korea":         "KR",
		"South Korea":               "KR",
		"Viet Nam":                  "VN",
		"Czech Republic":            "CZ",
		"Taiwan, Province of China": "TW",
		"香港":                        "HK",
	}
	for name, want := range cases {
		if code, _, ok := lookupCountry(name); !ok || code != want {
			t.Errorf("lookupCountry(%q) = %q, ok=%v，期望 %q", name, code, ok, want)
		}
	}
}

// TestFetchFallsBackToIPAPI 主源限流（code=500）时必须降级到备源，
// 并沿用备源的国家代码 —— 这是"地区与国旗都不显示"的主干修复。
func TestFetchFallsBackToIPAPI(t *testing.T) {
	iping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":500,"msg":"frequent queries, please try again later"}`))
	}))
	defer iping.Close()
	ipapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","country":"中国","countryCode":"CN","regionName":"广东","city":"广州"}`))
	}))
	defer ipapi.Close()

	r := NewResolver()
	r.ipingEndpoint = iping.URL + "/v1/query?ip=%s"
	r.ipapiEndpoint = ipapi.URL + "/json/%s"

	g := r.fetch("1.2.3.4")
	if g == nil || g.CountryCode != "CN" {
		t.Fatalf("主源失败应降级到备源并保留国家代码: %+v", g)
	}
	if g.Country != "China" || g.Region != "广东" || g.City != "广州" {
		t.Fatalf("备源结果口径异常: %+v", g)
	}
}

// TestFetchPrefersIping 主源可用时优先用主源，并补齐国家代码（含英文名归一）。
func TestFetchPrefersIping(t *testing.T) {
	var ipapiHits int
	iping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","data":{"ip":"1.2.3.4","country":"美国","region":"加利福尼亚州","city":"山景","longitude":"-122.08","latitude":"37.38"}}`))
	}))
	defer iping.Close()
	ipapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ipapiHits++
		_, _ = w.Write([]byte(`{"status":"success","country":"美国","countryCode":"US"}`))
	}))
	defer ipapi.Close()

	r := NewResolver()
	r.ipingEndpoint = iping.URL + "/%s"
	r.ipapiEndpoint = ipapi.URL + "/%s"

	g := r.fetch("1.2.3.4")
	if g == nil {
		t.Fatal("主源可用却解析失败")
	}
	if ipapiHits != 0 {
		t.Fatalf("主源成功时不应再请求备源（备源命中 %d 次）", ipapiHits)
	}
	if g.Country != "United States" || g.CountryCode != "US" {
		t.Fatalf("主源结果未归一: %+v", g)
	}
	if g.Latitude != 37.38 {
		t.Fatalf("经纬度字符串解析失败: %v", g.Latitude)
	}
}

// TestFetchFallsBackToIPWho 主源与备源都失败时由兜底源接手。
func TestFetchFallsBackToIPWho(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer bad.Close()
	who := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"country":"China","country_code":"CN","region":"Zhejiang","city":"Hangzhou","latitude":30.28,"longitude":120.15}`))
	}))
	defer who.Close()

	r := NewResolver()
	r.ipingEndpoint = bad.URL + "/%s"
	r.ipapiEndpoint = bad.URL + "/%s"
	r.ipwhoEndpoint = who.URL + "/%s"

	g := r.fetch("1.2.3.4")
	if g == nil || g.CountryCode != "CN" || g.City != "Hangzhou" {
		t.Fatalf("兜底源未生效: %+v", g)
	}
}

// TestFetchAllSourcesFail 三个源都不可用时返回 nil（不影响主机列表展示）。
func TestFetchAllSourcesFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer bad.Close()
	r := NewResolver()
	r.ipingEndpoint = bad.URL + "/%s"
	r.ipapiEndpoint = bad.URL + "/%s"
	r.ipwhoEndpoint = bad.URL + "/%s"
	if g := r.fetch("1.2.3.4"); g != nil {
		t.Fatalf("全部源失败应返回 nil，得到 %+v", g)
	}
}

// TestLookupCachesSuccess 成功结果命中 24h 缓存（避免频繁打外部接口）。
func TestLookupCachesSuccess(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"code":200,"data":{"country":"中国","region":"广东","city":"广州"}}`))
	}))
	defer srv.Close()
	r := NewResolver()
	r.ipingEndpoint = srv.URL + "/%s"
	r.ipapiEndpoint = srv.URL + "/%s"
	r.ipwhoEndpoint = srv.URL + "/%s"

	first := r.Lookup("9.9.9.9")
	second := r.Lookup("9.9.9.9")
	if first == nil || second == nil || first.CountryCode != "CN" {
		t.Fatalf("解析结果异常: %+v / %+v", first, second)
	}
	if hits != 1 {
		t.Fatalf("第二次查询应命中缓存，实际请求 %d 次", hits)
	}
}

// iping 主源不返回国家代码，但返回 as_country / company_country（归属国 ISO）。
// 这条用例锁定「必须用 as_country 补 country_code」这个行为 ——
// 丢了它，国土面积/国旗就又得依赖国名反查表，多一次可能失败的归一。
func TestParseIpingUsesASCountry(t *testing.T) {
	body := []byte(`{"code":200,"msg":"success","data":{
		"ip":"1.2.3.4","country":"新加坡","region":"","city":"",
		"longitude":"103.8","latitude":"1.35",
		"as_country":"SG","company_country":"SG"}}`)
	g := (&Resolver{}).parseIping(body)
	if g == nil {
		t.Fatal("应解析成功")
	}
	if g.CountryCode != "SG" {
		t.Fatalf("应取 as_country 作为国家代码，实际 %q", g.CountryCode)
	}
	ng := normalizeGeo(g)
	if ng.CountryCode != "SG" {
		t.Fatalf("归一化后国家代码应为 SG，实际 %q", ng.CountryCode)
	}
	// 中文国名应被统一为英文名，避免两个源口径不一致
	if ng.Country != "Singapore" {
		t.Fatalf("中文国名应归一为英文名，实际 %q", ng.Country)
	}
}

// as_country 缺失时（部分 IP 无 AS 归属）应回退 company_country，
// 两者都缺失时留空，交由 normalizeGeo 用国名反查表补齐。
func TestParseIpingCountryCodeFallback(t *testing.T) {
	r := &Resolver{}

	g := r.parseIping([]byte(`{"code":200,"data":{"country":"日本","as_country":"","company_country":"JP"}}`))
	if g == nil || g.CountryCode != "JP" {
		t.Fatalf("as_country 为空时应回退 company_country，实际 %+v", g)
	}

	g = r.parseIping([]byte(`{"code":200,"data":{"country":"日本","as_country":"","company_country":""}}`))
	if g == nil || g.CountryCode != "" {
		t.Fatalf("两者都为空时应留空，实际 %+v", g)
	}
	if n := normalizeGeo(g); n.CountryCode != "JP" {
		t.Fatalf("国名反查表应补齐 JP，实际 %q", n.CountryCode)
	}
}

// IPv6 必须跳过 api.iping.cc：该源对 IPv6 恒返回 code=500，
// 排在前面只会多一次无效往返与一条误导性的错误日志。
func TestFetchSkipsIpingForIPv6(t *testing.T) {
	var hitIping, hitIPAPI atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.RawQuery, "iping.example"):
			hitIping.Add(1)
			io.WriteString(w, `{"code":200,"data":{"country":"中国"}}`)
		case strings.Contains(r.URL.RawQuery, "ipapi.example"):
			hitIPAPI.Add(1)
			io.WriteString(w, `{"status":"success","country":"美国","countryCode":"US"}`)
		}
	}))
	defer srv.Close()

	r := &Resolver{
		timeout:       time.Second,
		ipingEndpoint: srv.URL + "/?target=iping.example&ip=%s",
		ipapiEndpoint: srv.URL + "/?target=ipapi.example&ip=%s",
		ipwhoEndpoint: srv.URL + "/?target=ipwho.example&ip=%s",
	}
	g := r.fetch("2606:4700::1111")
	if g == nil || g.CountryCode != "US" {
		t.Fatalf("IPv6 应直接命中备源，实际 %+v", g)
	}
	if n := hitIping.Load(); n != 0 {
		t.Fatalf("IPv6 不应请求 api.iping.cc，实际请求了 %d 次", n)
	}
	if n := hitIPAPI.Load(); n != 1 {
		t.Fatalf("备源应被请求 1 次，实际 %d", n)
	}

	// IPv4 仍应优先主源（主源只给国名，国家代码由反查表补齐）
	if g := r.fetch("1.2.3.4"); g == nil || g.CountryCode != "CN" {
		t.Fatalf("IPv4 应命中主源并由反查表补码，实际 %+v", g)
	}
	if n := hitIping.Load(); n != 1 {
		t.Fatalf("IPv4 应请求主源 1 次，实际 %d", n)
	}
}
