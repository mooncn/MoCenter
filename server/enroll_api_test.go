package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/auth"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
)

// 签发令牌的接口本身就是一个"给出身份"的入口：未鉴权者拿到令牌等于拿到那台主机的身份，
// 已鉴权者的审计里带出令牌明文等于把身份写进日志。这两条是这里唯一要证的性质。

func newEnrollAPIFixture(t *testing.T) (*agentauth.EnrollStore, *auth.Auth, *route.Router) {
	t.Helper()
	store, err := agentauth.NewEnrollStore(filepath.Join(t.TempDir(), agentauth.EnrollStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	authz := auth.NewAuth(nil)
	authz.RegisterToken("test-token", "admin")
	return store, authz, route.NewRouter(session.NewManager(), nil)
}

func doJSON(t *testing.T, h http.HandlerFunc, method, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/enroll/tokens", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEnrollAPIRequiresAuth 未登录（或口令错误 Token）不得签发、列举、吊销任何东西。
func TestEnrollAPIRequiresAuth(t *testing.T) {
	store, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared")
	verifier.SetEnrollStore(store)
	issue := enrollTokensHandler(store, verifier, authz, router)
	revoke := enrollRevokeHandler(store, authz, router)
	reset := enrollBindingResetHandler(store, authz, router)

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"签发需登录", issue},
		{"吊销需登录", revoke},
		{"清除绑定需登录", reset},
	} {
		for _, bearer := range []string{"", "wrong"} {
			rec := doJSON(t, tc.h, http.MethodPost, `{"agent_id":"host-01","id":"x"}`, bearer)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s：无有效 Token（%q）时应 401，实际 %d", tc.name, bearer, rec.Code)
			}
		}
	}
	if n := len(store.ListTokens()); n != 0 {
		t.Fatalf("被拒的请求不应落库，实际 %d 条", n)
	}
	rec := doJSON(t, issue, http.MethodGet, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录不得读取令牌清单，实际 %d", rec.Code)
	}
}

// TestEnrollAPITokenShownOnceAndNeverAudited 令牌明文只在签发响应里出现一次：
// 列表接口与审计日志都不能再带出它。
func TestEnrollAPITokenShownOnceAndNeverAudited(t *testing.T) {
	store, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("")
	verifier.SetEnrollStore(store)
	issue := enrollTokensHandler(store, verifier, authz, router)
	revoke := enrollRevokeHandler(store, authz, router)

	rec := doJSON(t, issue, http.MethodPost, `{"agent_id":"host-01","label":"rack-2","ttl_seconds":3600}`, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("签发应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.OK || created.Token == "" {
		t.Fatalf("签发响应异常: %s", rec.Body.String())
	}

	listRec := doJSON(t, issue, http.MethodGet, "", "test-token")
	if strings.Contains(listRec.Body.String(), created.Token) {
		t.Fatal("令牌列表回显了明文，控制台一次泄露即等于该机身份泄露")
	}
	// 吊销（按 id）后不可再用
	if r := doJSON(t, revoke, http.MethodPost, `{"id":"`+created.ID+`"}`, "test-token"); r.Code != http.StatusOK {
		t.Fatalf("吊销响应异常: %d %s", r.Code, r.Body.String())
	}
	if n := len(store.ListTokens()); n != 0 {
		t.Fatalf("吊销后仍残留 %d 条", n)
	}

	var auditDump []byte
	for _, l := range router.Audits() {
		if l.Category == "op" {
			b, _ := json.Marshal(l)
			auditDump = append(auditDump, b...)
		}
	}
	if len(auditDump) == 0 {
		t.Fatal("令牌签发/吊销必须留操作审计")
	}
	if strings.Contains(string(auditDump), created.Token) {
		t.Fatal("审计日志里出现了令牌明文")
	}
}

// TestEnrollAPIRejectsBadAgentID agent_id 会进证书 CN、日志与审计，必须先校验再落库。
func TestEnrollAPIRejectsBadAgentID(t *testing.T) {
	store, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared")
	verifier.SetEnrollStore(store)
	issue := enrollTokensHandler(store, verifier, authz, router)
	for _, body := range []string{
		`{"agent_id":""}`,
		`{"agent_id":"host 01"}`,
		`{"agent_id":"` + strings.Repeat("a", 200) + `"}`,
		`{"agent_id":"host\ttab"}`,
	} {
		rec := doJSON(t, issue, http.MethodPost, body, "test-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("非法 agent_id 应以业务错误返回，实际 %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("非法 agent_id %s 被接受", body)
		}
	}
	if len(store.ListTokens()) != 0 {
		t.Fatal("非法请求不应留下令牌")
	}
}

// TestEnrollAPISharedKeyFlagVisible 迁移期控制台必须能看到"共享密钥还开着"，
// 否则运维无从判断能不能加 -disable-shared-agent-key。
func TestEnrollAPISharedKeyFlagVisible(t *testing.T) {
	store, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared")
	verifier.SetEnrollStore(store)
	issue := enrollTokensHandler(store, verifier, authz, router)

	rec := doJSON(t, issue, http.MethodGet, "", "test-token")
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["shared_key_enabled"] != true {
		t.Fatalf("未关闭时应报告共享密钥可用: %+v", resp)
	}
	verifier.DisableSharedKey()
	rec = doJSON(t, issue, http.MethodGet, "", "test-token")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["shared_key_enabled"] != false {
		t.Fatalf("关闭后应报告不可用: %+v", resp)
	}
}

// TestEnrollAPIStoreUnavailable 存储不可用（磁盘故障等）时必须明确报错，
// 而不是静默返回一个"空令牌表"让运维误判成已迁移完。
func TestEnrollAPIStoreUnavailable(t *testing.T) {
	_, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared")
	rec := doJSON(t, enrollTokensHandler(nil, verifier, authz, router), http.MethodGet, "", "test-token")
	if strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("存储不可用却报告成功: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "不可用") {
		t.Fatalf("应给出可读原因: %s", rec.Body.String())
	}
}

// --- M-1：控制台不再被动下发注册密钥明文，明文只随一次显式轮换出现 ---

// TestServerInfoWithholdsSharedKeyPlaintext /api/server-info 是页面加载时顺手拉的接口，
// 把部署级根凭证放在这里，等于让每一次已登录访问（以及任何 XSS/扩展/代理日志）都带走一份。
func TestServerInfoWithholdsSharedKeyPlaintext(t *testing.T) {
	dataDir := t.TempDir()
	const plaintext = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(filepath.Join(dataDir, agentauth.KeyFileName), []byte(plaintext+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, authz, _ := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier(plaintext)

	rec := doJSON(t, serverInfoHandler(authz, nil, verifier, ":9000", dataDir, false), http.MethodGet, "", "test-token")
	body := rec.Body.String()
	if strings.Contains(body, plaintext) {
		t.Fatalf("server-info 仍下发密钥明文: %s", body)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["agent_key"]; ok {
		t.Fatalf("响应里不该再有 agent_key 字段: %+v", resp)
	}
	// 指纹与路径要在：运维得能确认"两台机器配的是同一把"、并知道去哪找它。
	if resp["agent_key_fingerprint"] != verifier.KeyFingerprint() || resp["agent_key_fingerprint"] == "" {
		t.Fatalf("应下发密钥指纹: %+v", resp)
	}
	if resp["agent_key_path"] != filepath.Join(dataDir, agentauth.KeyFileName) {
		t.Fatalf("应下发密钥文件路径: %+v", resp)
	}
}

// TestSharedKeyRotateNeedsAuthAndConfirm 轮换必须登录、且必须显式确认。
func TestSharedKeyRotateNeedsAuthAndConfirm(t *testing.T) {
	_, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared-key-before-rotate")
	keyPath := filepath.Join(t.TempDir(), agentauth.KeyFileName)
	h := sharedKeyRotateHandler(verifier, authz, router, keyPath, false)

	if rec := doJSON(t, h, http.MethodPost, `{"confirm":true}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录轮换应 401，实际 %d", rec.Code)
	}
	rec := doJSON(t, h, http.MethodPost, `{}`, "test-token")
	if strings.Contains(rec.Body.String(), `"agent_key"`) {
		t.Fatalf("未确认就给出了明文: %s", rec.Body.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("未确认时不该写密钥文件")
	}
	if rec := doJSON(t, h, http.MethodGet, "", "test-token"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("轮换是写操作，GET 应 405，实际 %d", rec.Code)
	}
}

// TestSharedKeyRotateTakesEffectAndKeepsPlaintextOutOfAudit 轮换要真的换掉可用密钥，
// 并且除了那一次响应之外，任何地方（这里查审计）都不该再出现明文。
func TestSharedKeyRotateTakesEffectAndKeepsPlaintextOutOfAudit(t *testing.T) {
	const oldKey = "old-shared-key-0000000000000000000000"
	_, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier(oldKey)
	keyPath := filepath.Join(t.TempDir(), agentauth.KeyFileName)
	// keyFromArgs=true：这条形态下重启会用启动值盖掉文件，必须告警
	h := sharedKeyRotateHandler(verifier, authz, router, keyPath, true)

	rec := doJSON(t, h, http.MethodPost, `{"confirm":true}`, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("轮换应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("含明文的响应必须禁止缓存，实际 %q", rec.Header().Get("Cache-Control"))
	}
	var resp struct {
		OK          bool     `json:"ok"`
		AgentKey    string   `json:"agent_key"`
		Fingerprint string   `json:"fingerprint"`
		Warnings    []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.AgentKey == "" || resp.AgentKey == oldKey {
		t.Fatalf("响应未给出一把新密钥: %s", rec.Body.String())
	}
	if resp.Fingerprint == "" || resp.Fingerprint != verifier.KeyFingerprint() {
		t.Fatalf("应回传新指纹（与运行时一致）: resp=%q runtime=%q", resp.Fingerprint, verifier.KeyFingerprint())
	}
	if joined := strings.Join(resp.Warnings, "|"); !strings.Contains(joined, "盖掉") {
		t.Fatalf("启动参数给密钥时应告警重启会退回旧值: %v", resp.Warnings)
	}
	// 落盘的就是这把新密钥（一键部署与下次启动都读它）
	onDisk, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读密钥文件失败: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != resp.AgentKey {
		t.Fatal("密钥文件与响应里的明文不一致")
	}
	// 旧密钥立即不能再注册，新密钥可以
	ts := time.Now().Unix()
	if _, err := verifier.Verify("host-01", "nonce-old", agentauth.Proof(oldKey, "host-01", "nonce-old", ts), ts); err == nil {
		t.Fatal("旧密钥轮换后仍能注册，等于没换")
	}
	if cred, err := verifier.Verify("host-01", "nonce-new", agentauth.Proof(resp.AgentKey, "host-01", "nonce-new", ts), ts); err != nil || cred != agentauth.CredSharedKey {
		t.Fatalf("新密钥应能注册，实际 cred=%q err=%v", cred, err)
	}

	var dump []byte
	for _, l := range router.Audits() {
		if l.Category == "op" {
			b, _ := json.Marshal(l)
			dump = append(dump, b...)
		}
	}
	if len(dump) == 0 {
		t.Fatal("轮换必须留操作审计")
	}
	s := string(dump)
	if strings.Contains(s, resp.AgentKey) || strings.Contains(s, oldKey) {
		t.Fatal("审计里出现了密钥明文")
	}
	if !strings.Contains(s, "轮换") {
		t.Fatalf("审计摘要应说明做了什么: %s", s)
	}
}

// TestSharedKeyRotateRefusedWhenDisabled 已关闭共享密钥这条路径时不该"顺手再打开"：
// 换一把没人能用的密钥只会让文件状态与运行时不一致。
func TestSharedKeyRotateRefusedWhenDisabled(t *testing.T) {
	_, authz, router := newEnrollAPIFixture(t)
	verifier := agentauth.NewVerifier("shared-key-x")
	verifier.DisableSharedKey()
	keyPath := filepath.Join(t.TempDir(), agentauth.KeyFileName)
	h := sharedKeyRotateHandler(verifier, authz, router, keyPath, false)

	rec := doJSON(t, h, http.MethodPost, `{"confirm":true}`, "test-token")
	if strings.Contains(rec.Body.String(), `"agent_key"`) {
		t.Fatalf("路径已关闭却仍给明文: %s", rec.Body.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("路径已关闭时不该写密钥文件")
	}
	for _, l := range router.Audits() {
		if l.Category == "op" {
			t.Fatalf("未发生轮换却写了审计: %+v", l)
		}
	}
}
