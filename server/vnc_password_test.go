package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
	"mocenter/server/internal/vnc"
)

// vncSecret 测试用的 VNC 口令。断言"别的地方不能出现它"时，
// 用一个可辨识的字符串比逐个字段断言更抗漏改。
const vncSecret = "S3cr3t-VNC-Pw-xz"

func newVNCAPIFixture(t *testing.T) (*vnc.Mgr, *auth.Auth, *route.Router) {
	t.Helper()
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok-vnc", "ops")
	router := route.NewRouter(session.NewManager(), nil)
	t.Cleanup(func() { _ = router.Close() })
	return vnc.NewMgr(session.NewManager()), authz, router
}

func vncBearerRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer tok-vnc")
	return r
}

// TestVNCStatusHasNoPassword 状态接口是前端每几秒轮询的，口令一旦进它的响应体，
// 就等于长期驻留在浏览器内存、扩展与任何能读到响应的位置。
func TestVNCStatusHasNoPassword(t *testing.T) {
	mgr, authz, _ := newVNCAPIFixture(t)
	mgr.NotifyInstall("host-1", &protocol.VncInstallResp{
		OK: true, Message: "已启动", Port: 5901, Password: vncSecret, Mode: "start",
	})

	rec := httptest.NewRecorder()
	vncStatusHandler(mgr, authz).ServeHTTP(rec, vncBearerRequest(t, http.MethodGet, "/api/vnc/status?agent=host-1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, vncSecret) {
		t.Fatalf("轮询状态接口不得带出口令：%s", body)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if _, ok := payload["password"]; ok {
		t.Fatal("响应里不应存在 password 字段（前端只需知道有口令可用）")
	}
	if payload["has_password"] != true {
		t.Fatalf("has_password 应为 true，实际 %v", payload["has_password"])
	}
	if payload["port"] != float64(5901) {
		t.Fatalf("端口应照常下发，实际 %v", payload["port"])
	}
}

// TestVNCPasswordHandlerDeliversAndAudits 口令只经这一个按需端点交付，且必须留痕。
// 审计里存的是"取到了没有"，不是口令本身 —— 否则审计库成了第二份明文凭据存储。
func TestVNCPasswordHandlerDeliversAndAudits(t *testing.T) {
	mgr, authz, router := newVNCAPIFixture(t)
	mgr.NotifyInstall("host-1", &protocol.VncInstallResp{OK: true, Password: vncSecret, Port: 5900, Mode: "start"})

	rec := httptest.NewRecorder()
	vncPasswordHandler(mgr, authz, router).ServeHTTP(rec,
		vncBearerRequest(t, http.MethodPost, "/api/vnc/password", `{"agent_id":"host-1"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		OK       bool   `json:"ok"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if !payload.OK || payload.Password != vncSecret {
		t.Fatalf("应按需交付口令，实际 %+v", payload)
	}

	logs := router.Audits()
	if len(logs) == 0 {
		t.Fatal("取口令必须留下审计")
	}
	last := logs[len(logs)-1]
	if last.Operator != "ops" || last.AgentID != "host-1" || !strings.Contains(last.Command, "VNC") {
		t.Fatalf("审计的取证要素不完整：%+v", last)
	}
	raw, _ := json.Marshal(logs)
	if strings.Contains(string(raw), vncSecret) {
		t.Fatal("审计记录中出现了口令明文")
	}
}

// TestVNCPasswordHandlerUnknownHost 没有口令可交付时不能返回空串加 ok:true，
// 前端会把它当"口令是空"填进客户端；同时这次失败也要留痕。
func TestVNCPasswordHandlerUnknownHost(t *testing.T) {
	mgr, authz, router := newVNCAPIFixture(t)
	rec := httptest.NewRecorder()
	vncPasswordHandler(mgr, authz, router).ServeHTTP(rec,
		vncBearerRequest(t, http.MethodPost, "/api/vnc/password", `{"agent_id":"ghost"}`))
	var payload struct {
		OK       bool   `json:"ok"`
		Password string `json:"password"`
		Msg      string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if payload.OK || payload.Password != "" || payload.Msg == "" {
		t.Fatalf("无口令时应明确失败并给出下一步，实际 %+v", payload)
	}
	logs := router.Audits()
	if len(logs) == 0 {
		t.Fatal("未取到口令同样要留痕（否则探测哪些主机有口令不留痕迹）")
	}
	if logs[len(logs)-1].Result == vncSecret || logs[len(logs)-1].Result == "" {
		t.Fatalf("审计结果字段应记录失败原因，实际 %q", logs[len(logs)-1].Result)
	}
}

// TestVNCPasswordHandlerRejectsNonPOST 口令交付必须是 POST：
// GET 会被浏览器历史、代理与访问日志整段记下来。
func TestVNCPasswordHandlerRejectsNonPOST(t *testing.T) {
	mgr, authz, _ := newVNCAPIFixture(t)
	mgr.NotifyInstall("host-1", &protocol.VncInstallResp{OK: true, Password: vncSecret})
	rec := httptest.NewRecorder()
	vncPasswordHandler(mgr, authz, nil).ServeHTTP(rec,
		vncBearerRequest(t, http.MethodGet, "/api/vnc/password?agent=host-1", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 取口令应返回 405，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), vncSecret) {
		t.Fatal("405 响应体里也不得出现口令")
	}
}

// TestVNCPasswordHandlerNeedsAuth 未登录者拿不到口令。
func TestVNCPasswordHandlerNeedsAuth(t *testing.T) {
	mgr, authz, _ := newVNCAPIFixture(t)
	mgr.NotifyInstall("host-1", &protocol.VncInstallResp{OK: true, Password: vncSecret})
	rec := httptest.NewRecorder()
	vncPasswordHandler(mgr, authz, nil).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/api/vnc/password", strings.NewReader(`{"agent_id":"host-1"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证应 401，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), vncSecret) {
		t.Fatal("被拒绝的请求不得带回口令")
	}
}
