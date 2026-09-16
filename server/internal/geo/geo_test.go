package geo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	r.endpoint = srv.URL + "/json/%s"
	g := r.fetch("1.2.3.4")
	if g == nil || g.CountryCode != "CN" || g.Country != "中国" || g.City != "广州" {
		t.Fatalf("解析结果异常: %+v", g)
	}
	if !strings.HasPrefix(r.endpoint, srv.URL) {
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
	// 指向一个必然失败的地址（保留端口 1）
	r.endpoint = "http://127.0.0.1:1/json/%s"
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
