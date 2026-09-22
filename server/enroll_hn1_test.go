package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
)

// 本文件是 H-N1 的回归闸门。
//
// 漏洞形态：服务端**没配任何注册凭据** + 开了 -allow-unauthenticated-agents 时，
// 一张自选 agent_id 的 CSR + 完全空的凭据就能换到本套 CA 签发的合法客户端证书。
// 两道判断互相抵消：enrollAgentCert 写成 `if !Enabled() && !allowAnon {拒}`，
// 而 Verifier.Verify 对"未启用"返回 ("", nil) —— 后者是根因（"沉默通过"）。
//
// 这些用例断言的是**性质**，不是实现细节：
//   · 没配凭据时，无论 allowAnon 是什么，都不许签发；
//   · 开了 allowAnon 也必须带 nonce/TS（不能"随便发个包就给证书"）；
//   · 带了错凭据时不许降级成匿名接入（否则"凭据配错"会伪装成"匿名接入"）；
//   · 正常令牌路径不受影响（修复不能把功能一起关掉）。

func hn1Fixture(t *testing.T) (*agentauth.Identity, *session.Manager, *route.Router) {
	t.Helper()
	id, err := agentauth.NewIdentity("Acme")
	if err != nil {
		t.Fatalf("准备服务端身份失败: %v", err)
	}
	sess := session.NewManager()
	return id, sess, route.NewRouter(sess, nil)
}

func hn1Enroll(t *testing.T, id *agentauth.Identity, v *agentauth.Verifier,
	sess *session.Manager, r *route.Router, req protocol.EnrollReq, allowAnon bool) protocol.EnrollResp {
	t.Helper()
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	return enrollAgentCert(conn, req, id, v, sess, r, allowAnon)
}

// TestHN1_NoCredentialPathRefusesWithoutFlag 没配凭据 + 没开开关 = 一律拒绝（默认拒绝）。
func TestHN1_NoCredentialPathRefusesWithoutFlag(t *testing.T) {
	id, sess, r := hn1Fixture(t)
	v := agentauth.NewVerifier("") // 无共享密钥、无令牌存储 → Enabled()==false
	req := protocol.EnrollReq{
		AgentID: "victim-host", // 自选身份
		Nonce:   agentauth.NewNonce(),
		TS:      time.Now().Unix(),
		CSR:     testCSR(t),
		// Proof 故意留空 —— 即使 nonce/TS 齐备，没有凭据也必须有开关才放行
	}
	resp := hn1Enroll(t, id, v, sess, r, req, false)
	if resp.OK {
		t.Fatalf("未开开关时无凭据仍签发证书：%s", resp.Message)
	}
	if resp.CertPEM != "" {
		t.Fatal("未开开关时仍返回了证书 PEM")
	}
	if !strings.Contains(resp.Message, "未配置注册凭据") {
		t.Errorf("拒绝理由应指出是服务端没有配置凭据，实际 %q", resp.Message)
	}
}

// TestHN1_AllowAnonIsTheOnlyGate 匿名接入**只由运维开关决定**，不由"是否配了凭据"决定。
//
// 这正是 H-N1 的修复口径：旧版把两件事混在一起（`!Enabled() && !allowAnon` 才拒），
// 又让 Verifier.Verify 对"未启用"返回 ("", nil)，于是"没配凭据"悄悄变成了"校验通过"。
// 现在两者解耦：没配凭据且没开开关 → 拒；开了开关（无论有没有配凭据）→ 按显式匿名放行，
// 但仍要求 nonce/TS，并写审计留痕。
func TestHN1_AllowAnonIsTheOnlyGate(t *testing.T) {
	for _, withStore := range []bool{false, true} {
		id, sess, r := hn1Fixture(t)
		v := agentauth.NewVerifier("")
		if withStore {
			store, err := agentauth.NewEnrollStore(t.TempDir() + "/enroll.json")
			if err != nil {
				t.Fatal(err)
			}
			v.SetEnrollStore(store)
		}
		resp := hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{
			// Proof 留空 = 明确"没带凭据"，这才是匿名路径（带 nonce/TS 是防重放底线）
			AgentID: "host-01", Nonce: agentauth.NewNonce(), TS: time.Now().Unix(), CSR: testCSR(t),
		}, true)
		if !resp.OK {
			t.Fatalf("withStore=%v 时显式匿名接入被拒（开关本身失效）：%s", withStore, resp.Message)
		}
		if resp.CertPEM == "" {
			t.Fatalf("withStore=%v 放行但没给出证书", withStore)
		}
	}
}

// TestHN1_AnonymousNeedsTimestamp 开了开关也不接受没有时间戳的空壳报文。
func TestHN1_AnonymousNeedsTimestamp(t *testing.T) {
	id, sess, r := hn1Fixture(t)
	v := agentauth.NewVerifier("")
	resp := hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{
		AgentID: "host-01", Nonce: agentauth.NewNonce(), TS: 0, CSR: testCSR(t),
	}, true)
	if resp.OK {
		t.Fatal("匿名接入未携带时间戳仍签发证书")
	}
}

// TestHN1_VerifierVerifyNeverSilentlyPasses Verify 层不允许"沉默通过"。
func TestHN1_VerifierVerifyNeverSilentlyPasses(t *testing.T) {
	v := agentauth.NewVerifier("")
	if _, err := v.Verify("any-id", "", "", 0); err == nil {
		t.Fatal(`Verify 对"未配置凭据"返回了 nil error —— 这正是 H-N1 的根因`)
	}
	if _, err := v.Verify("any-id", agentauth.NewNonce(), "deadbeef", time.Now().Unix()); err == nil {
		t.Fatal("未配置凭据时任意 proof 都不该通过")
	}
}

// TestHN1_AnonymousPathStillNeedsNonce 开了 allowAnon 也不接受"空壳"注册报文。
func TestHN1_AnonymousPathStillNeedsNonce(t *testing.T) {
	id, sess, r := hn1Fixture(t)
	store, err := agentauth.NewEnrollStore(t.TempDir() + "/enroll.json")
	if err != nil {
		t.Fatal(err)
	}
	v := agentauth.NewVerifier("")
	v.SetEnrollStore(store) // 已启用（令牌路径可用）

	// 无 nonce / 无 TS：即便开了 allowAnon 也拒
	resp := hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{AgentID: "host-01", CSR: testCSR(t)}, true)
	if resp.OK {
		t.Fatalf("匿名接入未携带 nonce/TS 仍签发证书：%s", resp.Message)
	}

	// 带齐 nonce/TS 且**明确不带凭据**（Proof 为空）：按显式匿名放行
	// —— 这是 allowAnon 的正当语义，修 H-N1 不能把它一起关掉。
	resp = hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{
		AgentID: "host-01", Nonce: agentauth.NewNonce(), TS: time.Now().Unix(), CSR: testCSR(t),
	}, true)
	if !resp.OK {
		t.Fatalf("显式匿名接入应可换取证书（否则修 H-N1 修掉了这个开关本身）：%s", resp.Message)
	}
	if resp.CertPEM == "" {
		t.Fatal("放行但没给出证书")
	}
}

// TestHN1_WrongCredentialNotDowngradedToAnonymous 带错凭据不许静默降级成匿名。
func TestHN1_WrongCredentialNotDowngradedToAnonymous(t *testing.T) {
	id, sess, r := hn1Fixture(t)
	store, err := agentauth.NewEnrollStore(t.TempDir() + "/enroll.json")
	if err != nil {
		t.Fatal(err)
	}
	v := agentauth.NewVerifier("")
	v.SetEnrollStore(store)

	nonce := agentauth.NewNonce()
	ts := time.Now().Unix()
	resp := hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{
		AgentID: "host-01", Nonce: nonce, TS: ts,
		Proof: agentauth.Proof("完全不对的令牌", "host-01", nonce, ts),
		CSR:   testCSR(t),
	}, true)
	if resp.OK {
		t.Fatal("错误凭据在 allowAnon 下降级成了匿名接入（会把配置错误伪装成正常）")
	}
}

// TestHN1_TokenPathStillWorks 修复不能把正当路径一起关掉。
func TestHN1_TokenPathStillWorks(t *testing.T) {
	id, sess, r := hn1Fixture(t)
	store, err := agentauth.NewEnrollStore(t.TempDir() + "/enroll.json")
	if err != nil {
		t.Fatal(err)
	}
	v := agentauth.NewVerifier("")
	v.SetEnrollStore(store)
	secret, _, err := store.CreateToken("host-01", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nonce := agentauth.NewNonce()
	ts := time.Now().Unix()
	resp := hn1Enroll(t, id, v, sess, r, protocol.EnrollReq{
		AgentID: "host-01", Nonce: nonce, TS: ts,
		Proof: agentauth.Proof(secret, "host-01", nonce, ts),
		CSR:   testCSR(t),
	}, false)
	if !resp.OK {
		t.Fatalf("一次性令牌路径应仍然可用：%s", resp.Message)
	}
}

// TestHN1_RefusalAlwaysLeavesAuditTrail 每一次"拒发证书"都必须留 auth 类审计。
//
// 这类事件的意义正好是"有人拿到了凭据/想拿到身份" —— 只进日志没人会看。
func TestHN1_RefusalAlwaysLeavesAuditTrail(t *testing.T) {
	cases := []struct {
		name      string
		req       protocol.EnrollReq
		allowAnon bool
	}{
		{"没配凭据 + 没开开关", protocol.EnrollReq{AgentID: "victim", Nonce: agentauth.NewNonce(), TS: time.Now().Unix()}, false},
		{"匿名但缺 nonce/TS", protocol.EnrollReq{AgentID: "victim"}, true},
		{"带了错凭据（不被降级）", protocol.EnrollReq{AgentID: "victim", Nonce: agentauth.NewNonce(), TS: time.Now().Unix(), Proof: "bogus"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, sess, r := hn1Fixture(t)
			v := agentauth.NewVerifier("")
			c.req.CSR = testCSR(t)
			resp := hn1Enroll(t, id, v, sess, r, c.req, c.allowAnon)
			if resp.OK {
				t.Fatalf("该场景应被拒，实际签发了证书：%s", resp.Message)
			}
			found := false
			for _, a := range r.Audits() {
				if a.Category == "auth" && a.AgentID == "victim" {
					found = true
				}
			}
			if !found {
				t.Error("被拒的签发没有留下 auth 类审计")
			}
		})
	}
}
