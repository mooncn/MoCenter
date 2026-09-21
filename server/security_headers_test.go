package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// securityHeaders 是 XSS 与凭据外流的最后一道服务端防线，所以按"攻击者能不能
// 用它"来出题，而不是按"头有没有写"来出题：CSP 只要允许内联脚本，页脚那类
// HTML 注入绕过净化后照样能执行。

func doRequest(t *testing.T, path string, secure bool) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if secure {
		r.TLS = &tls.ConnectionState{}
	}
	rec := httptest.NewRecorder()
	securityHeaders(next).ServeHTTP(rec, r)
	return rec
}

func TestCSPBlocksInjectedScripts(t *testing.T) {
	csp := doRequest(t, "/api/hosts", false).Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("响应没有 CSP，M-18 的兜底等于没上")
	}
	for _, directive := range []string{"script-src", "object-src", "base-uri", "frame-ancestors"} {
		line := ""
		for _, d := range strings.Split(csp, ";") {
			if strings.HasPrefix(strings.TrimSpace(d), directive) {
				line = strings.TrimSpace(d)
			}
		}
		if line == "" {
			t.Fatalf("CSP 缺少 %s：\n%s", directive, csp)
		}
		if strings.Contains(line, "unsafe-inline") || strings.Contains(line, "unsafe-eval") {
			t.Fatalf("%s 放开到 unsafe-*，注入即可执行：%s", directive, line)
		}
		if directive == "script-src" && strings.Contains(line, "*") {
			t.Fatalf("script-src 含通配符：%s", line)
		}
	}
	// 国旗精灵与终端样式是本仓实测出来的例外，去掉它们就是把控制台弄黑屏。
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Fatal("style-src 未允许内联样式：index.html 的首屏底色会被拦掉")
	}
	if !strings.Contains(csp, "img-src 'self' data:") {
		t.Fatal("img-src 未允许 data:：canvas 画出的国旗会全变方框")
	}
	if !strings.Contains(csp, "ws:") || !strings.Contains(csp, "wss:") {
		t.Fatal("connect-src 未允许 ws:/wss:：终端与远程桌面会被策略拦死")
	}
}

func TestAPINeverCacheable(t *testing.T) {
	// 登录响应带 Token、审计响应带命令结果、备份响应带文件：存一份就多一处泄露点。
	for _, path := range []string{"/api/login", "/api/audits", "/api/backups"} {
		if got := doRequest(t, path, false).Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s 的 Cache-Control = %q，期望 no-store", path, got)
		}
	}
	// 静态资源是内容寻址的压缩产物，禁缓存只会把首屏变慢。
	if got := doRequest(t, "/assets/index-abc123.js", false).Header().Get("Cache-Control"); got != "" {
		t.Fatalf("静态资源不该被加缓存头，实得 %q", got)
	}
}

func TestHSTSOnlyOverTLS(t *testing.T) {
	if got := doRequest(t, "/", true).Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("HTTPS 响应应带 HSTS，防止后续降级")
	}
	// 反代终止 TLS 时本服务看到的是明文请求；此时下发 HSTS 会让浏览器把
	// 同主机的明文站点一起锁死。
	if got := doRequest(t, "/", false).Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("明文响应不该带 HSTS，实得 %q", got)
	}
}

// tlsConn 只为满足 r.TLS != nil 这个判据，不参与任何真实协商。
type tlsConn struct{}

func (tlsConn) VerifyHostname(string) error { return nil }
