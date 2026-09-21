package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/ws"
)

// wsRequest 造一条浏览器形态的 WS 握手请求：凭证只能放子协议头。
func wsRequest(remoteAddr, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = remoteAddr
	r.Header.Set("Sec-WebSocket-Protocol", ws.WSSubprotocolName+","+token)
	return r
}

// TestGuardWSAllowsTokenInSubprotocol 白名单内的 IP + 子协议里的有效 Token 才放行。
func TestGuardWSAllowsTokenInSubprotocol(t *testing.T) {
	authz := auth.NewAuth([]string{"10.0.0.0/8"})
	authz.RegisterToken("tok-1", "ops")

	rec := httptest.NewRecorder()
	if !guardWS(rec, wsRequest("10.1.2.3:5210", "tok-1"), authz) {
		t.Fatalf("合法握手应放行，实得 HTTP %d", rec.Code)
	}
}

// TestGuardWSRejectsIPOutsideWhitelist 白名单外的来源即使持有有效 Token 也要拒。
//
// 回归用例：/ws 与 /vnc-ws 曾绕过 guard，运维用 allow_ips 把控制台关进内网后，
// 这两个入口仍对全网敞开 —— 而它们恰是最强的两个（操作员推送通道、内网 VNC 代理）。
func TestGuardWSRejectsIPOutsideWhitelist(t *testing.T) {
	authz := auth.NewAuth([]string{"10.0.0.0/8"})
	authz.RegisterToken("tok-1", "ops")

	rec := httptest.NewRecorder()
	if guardWS(rec, wsRequest("203.0.113.9:5210", "tok-1"), authz) {
		t.Fatal("白名单外的来源不应被放行")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("应回 403，实得 %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestGuardWSRejectsBadToken Token 从子协议头取出后仍需校验，猜不得。
func TestGuardWSRejectsBadToken(t *testing.T) {
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok-1", "ops")

	rec := httptest.NewRecorder()
	if guardWS(rec, wsRequest("203.0.113.9:5210", "guessed"), authz) {
		t.Fatal("无效 Token 不应被放行")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应回 401，实得 %d", rec.Code)
	}
}

// TestGuardWSRateLimits 握手吃每 IP 限流：闸门若不计数，就能无限速试 Token。
func TestGuardWSRateLimits(t *testing.T) {
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok-1", "ops")

	blockedAt := -1
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		if !guardWS(rec, wsRequest("203.0.113.9:5210", "tok-1"), authz) {
			blockedAt = i
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("第 %d 次请求被拒但非限流，状态码 %d", i+1, rec.Code)
			}
			break
		}
	}
	if blockedAt < 20 {
		// -1 表示 200 次都没被限流；过小则是"窗口远未打满就误伤"。
		// 上限具体数值由 auth 内部决定，不在此钉死。
		t.Fatalf("限流应在窗口接近打满后才生效（-1 表示从未生效），实得第 %d 次被拒", blockedAt+1)
	}
	// 被限流的来源不该牵连别的 IP
	rec := httptest.NewRecorder()
	if !guardWS(rec, wsRequest("198.51.100.7:5210", "tok-1"), authz) {
		t.Fatal("其他 IP 的握手不应被这一个来源的流量拖累")
	}
}

// TestGuardWSIgnoresAuthorizationHeader 只认子协议头：避免有人以为带上
// Authorization 就能握手，从而在前端留下半套鉴权路径。
func TestGuardWSIgnoresAuthorizationHeader(t *testing.T) {
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok-1", "ops")

	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "203.0.113.9:5210"
	r.Header.Set("Authorization", "Bearer tok-1")
	rec := httptest.NewRecorder()
	if guardWS(rec, r, authz) {
		t.Fatal("裸 Authorization 头不是 WS 握手的凭证来源")
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("应给出未授权提示，实得 %q", rec.Body.String())
	}
}
