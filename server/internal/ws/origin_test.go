package ws

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// SameOrigin 与 WSToken 是"跨站 WebSocket 劫持"这一类里唯二由服务端说了算的点，
// 所以两边都按"能不能被一个别的站点的页面用"来出题，而不是按实现分支出题。

func TestSameOriginAcceptsSameHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Host = "mc.example.com:8080"
	r.Header.Set("Origin", "http://mc.example.com:8080")
	if !SameOrigin(r) {
		t.Fatal("同源应放行，否则终端与远程桌面全废")
	}
}

func TestSameOriginRejectsOtherSite(t *testing.T) {
	for _, origin := range []string{
		"http://evil.example",           // 明显的跨站
		"http://mc.example.com:8081",    // 只差端口也不算同源
		"http://mc.example.com.evil.cn", // 后缀伪装
		"null",                          // 沙箱 iframe / file: 页面
	} {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Host = "mc.example.com:8080"
		r.Header.Set("Origin", origin)
		if SameOrigin(r) {
			t.Fatalf("Origin=%q 不该被放行", origin)
		}
	}
}

// 主机名大小写与首尾空白不该改变判定：Origin 头是浏览器写的，比对不该因它而松。
func TestSameOriginNormalizesCaseAndSpace(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Host = "MC.example.com:8080"
	r.Header.Set("Origin", " http://mc.EXAMPLE.com:8080")
	if !SameOrigin(r) {
		t.Fatal("大小写/空白差异应判为同源")
	}
}

// 反代/TLS 终止后的常见形态：页面在 https://host（无端口）下打开，
// 服务端看到的 Host 也没有端口。补默认端口这条必须判等，否则线上全断。
func TestSameOriginHandlesDefaultPortAndScheme(t *testing.T) {
	cases := []struct{ origin, host string }{
		{"https://mc.example.com", "mc.example.com"},
		{"https://mc.example.com:443", "mc.example.com"},
		{"http://192.168.1.9", "192.168.1.9:80"},
		{"ws://mc.example.com:8080", "mc.example.com:8080"},
		{"http://[::1]:8080", "[::1]:8080"}, // IPv6 字面量的冒号不能被当成分隔符
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Host = c.host
		r.Header.Set("Origin", c.origin)
		if !SameOrigin(r) {
			t.Fatalf("Origin=%q 对 Host=%q 应判同源", c.origin, c.host)
		}
	}
}

// 没有 Origin 头 = 不是浏览器发起的握手（curl、探活脚本）。
// 拒掉它们等于把非浏览器客户端一起拍死，而跨站劫持必然带 Origin。
func TestSameOriginAllowsMissingHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("Origin", "")
	if !SameOrigin(r) {
		t.Fatal("缺 Origin 应放行")
	}
}

// Token 只能从子协议头取：query 传参已摘除（会进访问日志、浏览器历史与截图）。
func TestWSTokenIgnoresQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws?token=secret-in-url", nil)
	if got := WSToken(r); got != "" {
		t.Fatalf("query 里的 token 不该再被接受，实际 %q", got)
	}
	r.Header.Set("Sec-WebSocket-Protocol", "mocenter,abc123")
	if got := WSToken(r); got != "abc123" {
		t.Fatalf("子协议里的 token = %q，期望 abc123", got)
	}
	// 只有固定子协议名 = 没带凭证，而不是"拿固定名当 token"
	r2 := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r2.Header.Set("Sec-WebSocket-Protocol", WSSubprotocolName)
	if got := WSToken(r2); got != "" {
		t.Fatalf("无 token 时应为空，实际 %q", got)
	}
}

// upgrader 接的是 SameOrigin 而不是"允许任意来源" —— 这条曾经被显式放宽过。
// 单独断言一次，是为了让"改回 return true"必须经过一个失败的测试。
func TestHubUpgraderChecksOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Host = "mc.example.com:8080"
	r.Header.Set("Origin", "http://evil.example")
	if upgrader.CheckOrigin(r) {
		t.Fatal("/ws 的 upgrader 没有在判同源")
	}
}
