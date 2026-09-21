package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
)

// 本文件覆盖 C-1 的签发端把关顺序。测试断言的都是"漏掉就等于没改"的性质：
// 一次性令牌只能用一次、令牌只对绑定的 agent_id 有效、活动会话不许被抢身份、
// 共享密钥泄露后换公钥必须被 TOFU 拦住 —— 这些如果回归，现网表现为"冒名接管主机"
// 而不是报错，因此不能只依赖 agentauth 包内的单元测试。

// enrollFixture 是一套最小可用的签发环境（真实 CA + 真实令牌存储 + 真实会话池）。
type enrollFixture struct {
	identity  *agentauth.Identity
	verifier  *agentauth.Verifier
	store     *agentauth.EnrollStore
	sess      *session.Manager
	router    *route.Router
	allowAnon bool
}

func newEnrollFixture(t *testing.T, sharedKey string) *enrollFixture {
	t.Helper()
	id, err := agentauth.NewIdentity("Acme")
	if err != nil {
		t.Fatalf("准备服务端身份失败: %v", err)
	}
	store, err := agentauth.NewEnrollStore(filepath.Join(t.TempDir(), agentauth.EnrollStoreFileName))
	if err != nil {
		t.Fatalf("准备令牌存储失败: %v", err)
	}
	v := agentauth.NewVerifier(sharedKey)
	v.SetEnrollStore(store)
	sess := session.NewManager()
	return &enrollFixture{identity: id, verifier: v, store: store, sess: sess, router: route.NewRouter(sess, nil)}
}

// testCSR 生成一份 Agent 侧形态的证书请求（私钥留在"本机"，只上交 CSR）。
func testCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// enroll 模拟一次"用凭据换证书"，返回服务端响应。secret 为空表示不带任何凭据。
func (f *enrollFixture) enroll(t *testing.T, agentID, secret string) protocol.EnrollResp {
	t.Helper()
	nonce := agentauth.NewNonce()
	ts := time.Now().Unix()
	req := protocol.EnrollReq{AgentID: agentID, Nonce: nonce, TS: ts, CSR: testCSR(t)}
	if secret != "" {
		req.Proof = agentauth.Proof(secret, agentID, nonce, ts)
	}
	// enrollAgentCert 只用连接做日志署名与拒绝回写，管道连接足够
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	return enrollAgentCert(conn, req, f.identity, f.verifier, f.sess, f.router, f.allowAnon)
}

// authAudits 返回被拒签发写下的 auth 类审计条目。
func (f *enrollFixture) authAudits() []*route.AuditLog {
	var out []*route.AuditLog
	for _, l := range f.router.Audits() {
		if l.Category == "auth" {
			out = append(out, l)
		}
	}
	return out
}

// TestEnrollTokenIsSingleUseAcrossIssuance 令牌换过一次证书后，同一台机器再拿它换第二张必须失败。
func TestEnrollTokenIsSingleUseAcrossIssuance(t *testing.T) {
	f := newEnrollFixture(t, "")
	secret, _, err := f.store.CreateToken("host-01", "rack-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.enroll(t, "host-01", secret); !resp.OK {
		t.Fatalf("首次换取应成功: %s", resp.Message)
	}
	if resp := f.enroll(t, "host-01", secret); resp.OK {
		t.Fatal("同一枚一次性令牌换到第二张证书，说明「一次性」没落地")
	}
}

// TestEnrollTokenCannotMintAnotherHostsIdentity 拿 A 机的令牌申请 B 机身份必须被拒。
func TestEnrollTokenCannotMintAnotherHostsIdentity(t *testing.T) {
	f := newEnrollFixture(t, "")
	secret, _, err := f.store.CreateToken("host-01", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.enroll(t, "victim-host", secret); resp.OK {
		t.Fatal("用他人令牌铸出了别的 agent_id 的证书")
	}
	if len(f.store.Bindings()) != 0 {
		t.Fatal("被拒的申请不应留下 TOFU 绑定")
	}
}

// TestEnrollRefusesBusyAgentID 已有活动会话的 agent_id 不得再被发证：
// 否则攻破一台机器后，可以用它的凭据抢一台正在服务的主机身份。
func TestEnrollRefusesBusyAgentID(t *testing.T) {
	f := newEnrollFixture(t, "shared")
	peer, other := net.Pipe()
	defer peer.Close()
	defer other.Close()
	if _, ok := f.sess.Upsert("host-01", session.NewConn(peer)); !ok {
		t.Fatal("前置条件失败：会话未建立")
	}
	resp := f.enroll(t, "host-01", "shared")
	if resp.OK {
		t.Fatal("该 agent_id 正在线，仍被发了新证书")
	}
	if !strings.Contains(resp.Message, "活动会话") {
		t.Fatalf("拒绝原因应说明是活动会话冲突，实际: %s", resp.Message)
	}
	if len(f.authAudits()) == 0 {
		t.Fatal("抢身份未遂必须留下审计（这类事件只有进审计库才会被人看到）")
	}
}

// TestEnrollTOFUBlocksSharedKeySwap 共享密钥泄露者拿同一 agent_id 换新公钥 → 拒；
// 运维为该主机签发一次性令牌（明确授权）后 → 放行并更新绑定。
func TestEnrollTOFUBlocksSharedKeySwap(t *testing.T) {
	f := newEnrollFixture(t, "shared")
	if resp := f.enroll(t, "host-01", "shared"); !resp.OK {
		t.Fatalf("首次接入应成功: %s", resp.Message)
	}
	resp := f.enroll(t, "host-01", "shared")
	if resp.OK {
		t.Fatal("仅凭共享密钥就能换发新公钥，TOFU 形同虚设")
	}
	if !strings.Contains(resp.Message, "一次性注册令牌") {
		t.Fatalf("拒绝信息应给出恢复路径，实际: %s", resp.Message)
	}
	if len(f.authAudits()) == 0 {
		t.Fatal("TOFU 不匹配必须写审计")
	}
	// 绑定不能被失败的重绑改写
	before := f.store.Bindings()

	secret, _, err := f.store.CreateToken("host-01", "重装授权", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.enroll(t, "host-01", secret); !resp.OK {
		t.Fatalf("令牌授权的重绑应成功: %s", resp.Message)
	}
	after := f.store.Bindings()
	if len(after) != 1 || after[0].Rebinds != 1 || after[0].SPKIHash == before[0].SPKIHash {
		t.Fatalf("重绑后绑定状态异常: %+v（原 %+v）", after, before)
	}
}

// TestEnrollRejectsGarbageInputs agent_id 非法与无凭据都要在签发动作之前被拒。
func TestEnrollRejectsGarbageInputs(t *testing.T) {
	f := newEnrollFixture(t, "shared")
	for _, tc := range []struct {
		name, agentID, secret string
	}{
		{"空 agent_id", "", "shared"},
		{"带空格的 agent_id", "host 01", "shared"},
		{"无凭据", "host-01", ""},
		{"凭据不对", "host-01", "wrong-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := f.enroll(t, tc.agentID, tc.secret); resp.OK {
				t.Fatalf("%s 的情况下仍签发了证书", tc.name)
			}
		})
	}
	if n := len(f.store.Bindings()); n != 0 {
		t.Fatalf("全部被拒后不应有绑定记录，得到 %d 条", n)
	}
}

// TestEnrollWithDisabledSharedKey 迁移终点：关掉共享密钥后，只有一台一份的令牌能接入。
func TestEnrollWithDisabledSharedKey(t *testing.T) {
	f := newEnrollFixture(t, "shared")
	f.verifier.DisableSharedKey()
	if resp := f.enroll(t, "host-01", "shared"); resp.OK {
		t.Fatal("关闭共享密钥后它仍可换取证书")
	}
	secret, _, err := f.store.CreateToken("host-01", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.enroll(t, "host-01", secret); !resp.OK {
		t.Fatalf("令牌路径应独立可用: %s", resp.Message)
	}
}

// TestEnrollWithNoCredentialPathRefuses 服务端没配任何注册凭据时不得发证。
//
// 这条不是可有可无的收尾：证书身份会让后续连接**跳过** HMAC 校验，
// 所以"没配凭据也照样签发"等于把上线阶段的默认拒绝整个绕开。
func TestEnrollWithNoCredentialPathRefuses(t *testing.T) {
	f := newEnrollFixture(t, "")
	f.verifier = agentauth.NewVerifier("") // 无共享密钥、无令牌存储
	if resp := f.enroll(t, "host-01", "shared"); resp.OK {
		t.Fatal("无任何注册凭据时仍签发了证书，谁都能拿到合法身份")
	}
	// 显式开启 -allow-unauthenticated-agents 时才放行（保持既有语义）
	f.allowAnon = true
	if resp := f.enroll(t, "host-01", ""); !resp.OK {
		t.Fatalf("显式允许无凭证接入时应可换取证书: %s", resp.Message)
	}
}

// TestVerifyOnlineRegistration 覆盖上线阶段的鉴权分流（与签发共用同一套凭据）。
func TestVerifyOnlineRegistration(t *testing.T) {
	store, err := agentauth.NewEnrollStore(filepath.Join(t.TempDir(), agentauth.EnrollStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := store.CreateToken("host-01", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := agentauth.NewVerifier("")
	v.SetEnrollStore(store)

	nonce := agentauth.NewNonce()
	ts := time.Now().Unix()
	okInfo := protocol.AgentInfo{AgentID: "host-01", AuthNonce: nonce, AuthTime: ts,
		AuthProof: agentauth.Proof(secret, "host-01", nonce, ts)}
	if err := verifyOnlineRegistration(v, okInfo, false); err != nil {
		t.Fatalf("有效令牌应放行上线: %v", err)
	}
	// 令牌已消耗，重启后的第二次上线必须靠证书身份（这里没有证书 → 拒）
	nonce2 := agentauth.NewNonce()
	replay := protocol.AgentInfo{AgentID: "host-01", AuthNonce: nonce2, AuthTime: ts,
		AuthProof: agentauth.Proof(secret, "host-01", nonce2, ts)}
	if err := verifyOnlineRegistration(v, replay, false); err == nil {
		t.Fatal("复用已消耗令牌仍能上线")
	}

	// 一条凭据路径都没配：默认拒绝，只有显式 -allow-anon-agents 才放行
	bare := agentauth.NewVerifier("")
	if err := verifyOnlineRegistration(bare, protocol.AgentInfo{AgentID: "x"}, false); err == nil {
		t.Fatal("未配置任何注册凭据时不得默认放行（忘了配密钥不能等于谁都能注册）")
	}
	if err := verifyOnlineRegistration(bare, protocol.AgentInfo{AgentID: "x"}, true); err != nil {
		t.Fatalf("显式允许匿名注册时才应放行: %v", err)
	}
	// nil verifier（测试/极端配置）不得 panic
	if err := verifyOnlineRegistration(nil, protocol.AgentInfo{AgentID: "x"}, false); err == nil {
		t.Fatal("nil verifier 在无凭证配置下应拒绝")
	}
}
