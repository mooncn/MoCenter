package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/auth"
)

func resetHandoffs(t *testing.T) {
	t.Helper()
	sessionHandoffs = &handoffStore{items: make(map[string]handoffEntry), now: time.Now}
}

func handoffRequest(t *testing.T, h http.HandlerFunc, body, bearer, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session/handoff", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSessionHandoffFlow 交接码：同 IP 单次兑换、新 Token 有效、不能重复使用。
func TestSessionHandoffFlow(t *testing.T) {
	resetHandoffs(t)
	authz := auth.NewAuth(nil)
	authz.RegisterToken("old-token", "admin")
	create := sessionHandoffCreateHandler(authz)
	consume := sessionHandoffConsumeHandler(authz)

	rec := doJSON(t, create, http.MethodPost, "", "old-token")
	resp := decodeResp(t, rec.Body.Bytes())
	code, _ := resp["code"].(string)
	if ok, _ := resp["ok"].(bool); !ok || code == "" {
		t.Fatalf("应生成交接码: %s", rec.Body.String())
	}

	rec = doJSON(t, consume, http.MethodPost, `{"code":"`+code+`"}`, "")
	resp = decodeResp(t, rec.Body.Bytes())
	newToken, _ := resp["token"].(string)
	if ok, _ := resp["ok"].(bool); !ok || newToken == "" {
		t.Fatalf("交接码应能兑换新 Token: %s", rec.Body.String())
	}
	if !authz.ValidateToken(newToken) {
		t.Fatal("兑换出的新 Token 应有效")
	}

	rec = doJSON(t, consume, http.MethodPost, `{"code":"`+code+`"}`, "")
	resp = decodeResp(t, rec.Body.Bytes())
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("交接码只能使用一次")
	}
}

// TestSessionHandoffBoundToIP 不同来源 IP 不能兑换；记录保留给原 IP 重试。
func TestSessionHandoffBoundToIP(t *testing.T) {
	resetHandoffs(t)
	authz := auth.NewAuth(nil)
	authz.RegisterToken("old-token", "admin")
	create := sessionHandoffCreateHandler(authz)
	consume := sessionHandoffConsumeHandler(authz)

	rec := handoffRequest(t, create, "", "old-token", "192.0.2.10:1111")
	code, _ := decodeResp(t, rec.Body.Bytes())["code"].(string)
	if code == "" {
		t.Fatalf("应生成交接码: %s", rec.Body.String())
	}

	rec = handoffRequest(t, consume, `{"code":"`+code+`"}`, "", "192.0.2.99:2222")
	if ok, _ := decodeResp(t, rec.Body.Bytes())["ok"].(bool); ok {
		t.Fatal("不同 IP 不应能兑换交接码")
	}
	rec = handoffRequest(t, consume, `{"code":"`+code+`"}`, "", "192.0.2.10:3333")
	if ok, _ := decodeResp(t, rec.Body.Bytes())["ok"].(bool); !ok {
		t.Fatal("原 IP 仍应能兑换（错误 IP 不应消耗记录）")
	}
}

// TestSessionHandoffRequiresLiveToken 原 Token 被吊销后，交接码随之失效。
func TestSessionHandoffRequiresLiveToken(t *testing.T) {
	resetHandoffs(t)
	authz := auth.NewAuth(nil)
	authz.RegisterToken("old-token", "admin")
	create := sessionHandoffCreateHandler(authz)
	consume := sessionHandoffConsumeHandler(authz)

	rec := doJSON(t, create, http.MethodPost, "", "old-token")
	code, _ := decodeResp(t, rec.Body.Bytes())["code"].(string)
	authz.RevokeToken("old-token")

	rec = doJSON(t, consume, http.MethodPost, `{"code":"`+code+`"}`, "")
	if ok, _ := decodeResp(t, rec.Body.Bytes())["ok"].(bool); ok {
		t.Fatal("原 Token 已吊销时交接码不应再生效")
	}
}
