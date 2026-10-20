package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestParseProxyCIDRs 可信代理清单的解析口径：裸 IP 与 CIDR 都要认，垃圾必须报错。
func TestParseProxyCIDRs(t *testing.T) {
	if got, err := ParseProxyCIDRs(""); err != nil || got != nil {
		t.Fatalf("空串应表示不启用，得 %v / %v", got, err)
	}
	got, err := ParseProxyCIDRs("127.0.0.1, 10.0.0.0/8, ::1")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d", len(got))
	}
	if _, err := ParseProxyCIDRs("not-an-ip"); err == nil {
		t.Fatal("非法条目必须报错，不能让运维以为已经放行")
	}
}

// TestTrustProxyClients 只有 TCP 对端可信时才采信 XFF，且按"从右往左第一个非可信跳"取值。
func TestTrustProxyClients(t *testing.T) {
	proxies, err := ParseProxyCIDRs("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	seen := ""
	h := TrustProxyClients(proxies, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ClientIP(r.RemoteAddr)
	}))

	// ① 代理之后：取到真实客户端
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.RemoteAddr = "10.0.0.9:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7" {
		t.Fatalf("可信代理后应按 XFF 还原，实际 %q", seen)
	}

	// ② 非代理来源：XFF 一律不采信（否则任何人都能伪造来源过白名单）
	req = httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.RemoteAddr = "198.51.100.5:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "198.51.100.5" {
		t.Fatalf("不可信对端必须按 TCP 对端判定，实际 %q", seen)
	}

	// ③ XFF 里有垃圾：维持代理 IP，不把伪造内容当身份
	req = httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.RemoteAddr = "10.0.0.9:443"
	req.Header.Set("X-Forwarded-For", "evil\", 10.0.0.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "10.0.0.9" {
		t.Fatalf("XFF 含非法值时应维持对端地址，实际 %q", seen)
	}

	// ④ 未配置可信代理：与修复前完全一致
	next := TrustProxyClients(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ClientIP(r.RemoteAddr)
	}))
	req = httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.RemoteAddr = "10.0.0.9:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	next.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "10.0.0.9" {
		t.Fatalf("默认不得采信 XFF，实际 %q", seen)
	}
}
