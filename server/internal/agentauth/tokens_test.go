package agentauth

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore 建一个临时令牌存储。
func newTestStore(t *testing.T) *EnrollStore {
	t.Helper()
	s, err := NewEnrollStore(filepath.Join(t.TempDir(), EnrollStoreFileName))
	if err != nil {
		t.Fatalf("创建令牌存储失败: %v", err)
	}
	return s
}

// enrollProof 按 Agent 侧的算法算出注册证明。
func enrollProof(secret, agentID, nonce string, ts int64) string {
	return Proof(secret, agentID, nonce, ts)
}

// TestTokenIsSingleUse 一次性令牌：用第二次必须被拒（哪怕报文完全相同）。
func TestTokenIsSingleUse(t *testing.T) {
	store := newTestStore(t)
	secret, _, err := store.CreateToken("host-01", "test", time.Hour)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	v := NewVerifier("")
	v.SetEnrollStore(store)
	nonce := NewNonce()
	ts := time.Now().Unix()

	if src, bound, err := v.VerifyEx("host-01", nonce, enrollProof(secret, "host-01", nonce, ts), ts); err != nil || src != CredToken || !bound {
		t.Fatalf("首次使用应通过且报告为已绑定令牌，得到 src=%q bound=%v err=%v", src, bound, err)
	}
	nonce2 := NewNonce()
	if _, err := v.Verify("host-01", nonce2, enrollProof(secret, "host-01", nonce2, ts), ts); err == nil {
		t.Fatal("同一枚令牌第二次使用必须被拒（否则一次性形同虚设）")
	}
}

// TestTokenBoundToAgentID 令牌只对绑定的那台主机有效：换 agent_id 即失效。
//
// 这正是 C-1 的核心收口点：旧实现里一把共享密钥可以注册任意 agent_id。
func TestTokenBoundToAgentID(t *testing.T) {
	store := newTestStore(t)
	secret, _, err := store.CreateToken("host-01", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("")
	v.SetEnrollStore(store)
	nonce := NewNonce()
	ts := time.Now().Unix()
	// 拿 host-01 的令牌去换 host-02 的身份
	if _, err := v.Verify("host-02", nonce, enrollProof(secret, "host-02", nonce, ts), ts); err == nil {
		t.Fatal("用 A 机的令牌注册 B 机必须被拒")
	}
}

// TestUnboundTokenAcceptsAnyAgentOnce 不绑定令牌（签发时留空 agent_id）：
// 任何主机都能用它换取一次证书，身份以它上报的 agent_id 为准；用掉即焚。
func TestUnboundTokenAcceptsAnyAgentOnce(t *testing.T) {
	store := newTestStore(t)
	secret, token, err := store.CreateToken("", "any-host", time.Hour)
	if err != nil {
		t.Fatalf("签发不绑定令牌失败: %v", err)
	}
	v := NewVerifier("")
	v.SetEnrollStore(store)
	ts := time.Now().Unix()

	// 客户端未指定 agent_id 时会以机器 UUID 上报，这里直接模拟该形态
	const uuid = "BF674D56-7E0D-70DB-A9FF-103DE56368D8"
	nonce := NewNonce()
	if src, bound, err := v.VerifyEx(uuid, nonce, Proof(secret, uuid, nonce, ts), ts); err != nil || src != CredToken || bound {
		t.Fatalf("不绑定令牌应接受任意 agent_id 且 tokenBound=false，得到 src=%q bound=%v err=%v", src, bound, err)
	}

	// 同一枚令牌不能被第二台主机再用
	nonce2 := NewNonce()
	const other = "host-02"
	if _, err := v.Verify(other, nonce2, Proof(secret, other, nonce2, ts), ts); err == nil {
		t.Fatal("不绑定令牌也只能用一次")
	}

	// 使用后应把实际上报的身份回填到令牌记录，便于列表显示与后续清理。
	got := store.GetToken(token.ID)
	if got == nil {
		t.Fatal("令牌记录不应消失")
	}
	if got.AgentID != uuid {
		t.Fatalf("不绑定令牌使用后应回填实际 agent_id，期望 %q，实际 %q", uuid, got.AgentID)
	}
	if got.UsedAt == 0 {
		t.Fatal("使用后 used_at 应更新")
	}
	for _, rec := range store.ListTokens() {
		if rec["id"] == token.ID && rec["unbound"] != false {
			t.Fatalf("回填身份后列表不应再标记 unbound: %+v", rec)
		}
	}
}

// TestUnboundTokenBackfillKeepsTokenBoundSemantics 回归：账本回填不能改变
// "签发时是否绑定"的语义，否则未绑定令牌会被 CheckOrBind 当成显式授权，
// 从而可以接管一台已有主机（换掉它的公钥）。
func TestUnboundTokenBackfillKeepsTokenBoundSemantics(t *testing.T) {
	store := newTestStore(t)
	secret, token, err := store.CreateToken("", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("")
	v.SetEnrollStore(store)
	ts := time.Now().Unix()
	nonce := NewNonce()
	if _, bound, err := v.VerifyEx("host-new", nonce, Proof(secret, "host-new", nonce, ts), ts); err != nil || bound {
		t.Fatalf("首次使用应成功且 tokenBound=false，得到 bound=%v err=%v", bound, err)
	}
	if got := store.GetToken(token.ID); got == nil || got.AgentID != "host-new" {
		t.Fatalf("身份应回填到记录: %+v", got)
	}

	// 已有主机换公钥：未绑定令牌（即使已回填身份）也不能授权。
	if err := store.CheckOrBind("host-existing", "old-spki", CredSharedKey, false); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckOrBind("host-existing", "new-spki", CredToken, false); err == nil {
		t.Fatal("未绑定令牌不得接管已有主机")
	}
}

// TestUnboundTokenProofBindsPresentedID 不绑定令牌不意味着证明可以跨身份复用：
// HMAC 的输入含本次请求的 agent_id，拿 A 的证明冒充 B 必须失配。
func TestUnboundTokenProofBindsPresentedID(t *testing.T) {
	store := newTestStore(t)
	secret, _, err := store.CreateToken("", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier("")
	v.SetEnrollStore(store)
	ts := time.Now().Unix()
	nonce := NewNonce()
	proofA := Proof(secret, "host-a", nonce, ts)
	if _, err := v.Verify("host-b", nonce, proofA, ts); err == nil {
		t.Fatal("以 A 的证明冒充 B 必须被拒（证明绑定本次上报的 agent_id）")
	}
	// 失败不应消耗令牌：正确的身份与证明仍可用
	nonce2 := NewNonce()
	if src, err := v.Verify("host-a", nonce2, Proof(secret, "host-a", nonce2, ts), ts); err != nil || src != CredToken {
		t.Fatalf("失配请求不应消耗令牌，得到 src=%q err=%v", src, err)
	}
}

// TestUnboundTokenPersistsAcrossReload 空 agent_id 也是合法记录，
// 重载时不能被当成"缺字段"丢弃（否则重启后令牌凭空消失）。
func TestUnboundTokenPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), EnrollStoreFileName)
	s1, err := NewEnrollStore(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s1.CreateToken("", "reload", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewEnrollStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s2.ListTokens()); n != 1 {
		t.Fatalf("不绑定令牌未持久化，重载后剩 %d 条", n)
	}
	v := NewVerifier("")
	v.SetEnrollStore(s2)
	nonce := NewNonce()
	ts := time.Now().Unix()
	if _, err := v.Verify("host-01", nonce, Proof(secret, "host-01", nonce, ts), ts); err != nil {
		t.Fatalf("重载后不绑定令牌应仍可用: %v", err)
	}
}

// TestTokenExpiry 过期令牌不再可用，且列表里标记为已过期。
//
// 只推进令牌存储的时钟：校验器的时间窗仍按真实时间判定，这样"被拒"的原因
// 一定是过期而不是时钟偏差。
func TestTokenExpiry(t *testing.T) {
	store := newTestStore(t)
	v := NewVerifier("")
	v.SetEnrollStore(store)

	live, _, err := store.CreateToken("host-01", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	nonce := NewNonce()
	ts := time.Now().Unix()
	if _, err := v.Verify("host-01", nonce, Proof(live, "host-01", nonce, ts), ts); err != nil {
		t.Fatalf("有效期内应可用: %v", err)
	}

	dead, _, err := store.CreateToken("host-02", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	ts2 := time.Now().Unix()
	nonce2 := NewNonce()
	if _, err := v.Verify("host-02", nonce2, Proof(dead, "host-02", nonce2, ts2), ts2); err == nil {
		t.Fatal("过期令牌必须被拒")
	}
	for _, rec := range store.ListTokens() {
		if rec["agent_id"] == "host-02" && rec["expired"] != true {
			t.Fatalf("过期令牌应在列表里标记 expired，得到 %+v", rec)
		}
	}
}

// TestSharedKeyPathStillWorksUntilDisabled 双轨过渡：迁移期共享密钥仍可换取证书
// （否则现网未换令牌的主机直接被锁在门外），关闭后只剩令牌路径。
func TestSharedKeyPathStillWorksUntilDisabled(t *testing.T) {
	const shared = "deploy-wide-key"
	store := newTestStore(t)
	v := NewVerifier(shared)
	v.SetEnrollStore(store)

	nonce := NewNonce()
	ts := time.Now().Unix()
	src, err := v.Verify("legacy-host", nonce, Proof(shared, "legacy-host", nonce, ts), ts)
	if err != nil || src != CredSharedKey {
		t.Fatalf("迁移期共享密钥应可用，得到 src=%q err=%v", src, err)
	}
	if !v.SharedKeyEnabled() || !v.Enabled() {
		t.Fatal("未关闭时两条状态位都应反映共享密钥可用")
	}

	v.DisableSharedKey()
	if v.SharedKeyEnabled() {
		t.Fatal("关闭后 SharedKeyEnabled 必须为 false")
	}
	if !v.Enabled() {
		t.Fatal("令牌存储还在，Enabled 必须仍为 true")
	}
	// 关闭后共享密钥不再构成身份（这条路径整体失效，而不是"降级可用"）
	nonce2 := NewNonce()
	if _, err := v.Verify("legacy-host", nonce2, Proof(shared, "legacy-host", nonce2, ts), ts); err == nil {
		t.Fatal("关闭共享密钥后，用它注册必须被拒")
	}

	secret, _, err := store.CreateToken("host-09", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	nonce3 := NewNonce()
	ts3 := time.Now().Unix()
	src, err = v.Verify("host-09", nonce3, Proof(secret, "host-09", nonce3, ts3), ts3)
	if err != nil || src != CredToken {
		t.Fatalf("令牌路径应独立可用，得到 src=%q err=%v", src, err)
	}
}

// TestRotateSharedKeyTakesEffectImmediately 轮换必须同时改文件与运行时状态。
//
// 只要有一处没跟上，表现就是"控制台说换好了"而服务端仍认旧密钥（或反过来）——
// 这类不一致排查起来比直接的拒绝服务更费时间。
func TestRotateSharedKeyTakesEffectImmediately(t *testing.T) {
	const old = "legacy-deploy-key"
	path := filepath.Join(t.TempDir(), KeyFileName)
	v := NewVerifier(old)
	oldFP := v.KeyFingerprint()

	newKey, err := v.RotateSharedKey(path)
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if newKey == "" || newKey == old {
		t.Fatalf("轮换结果异常: %q", newKey)
	}
	if v.SharedKey() != newKey {
		t.Fatal("运行时仍持旧密钥：一键部署会写出连不上的配置")
	}
	if v.KeyFingerprint() == oldFP {
		t.Fatal("指纹没变，说明密钥没真的换")
	}
	onDisk, _, rerr := LoadOrCreateKey(path)
	if rerr != nil || onDisk != newKey {
		t.Fatalf("文件内容与运行时不一致: %q %v", onDisk, rerr)
	}
	// 长度与熵：轮换出来的密钥必须与首次生成同规格，否则客户端兼容会被悄悄打破
	if len(newKey) != KeyBytes*2 || strings.ContainsAny(newKey, " \n\t") {
		t.Fatalf("密钥形态异常（应为 %d 位 hex）: %q", KeyBytes*2, newKey)
	}

	ts := time.Now().Unix()
	n1 := NewNonce()
	if _, err := v.Verify("host-10", n1, Proof(old, "host-10", n1, ts), ts); err == nil {
		t.Fatal("旧密钥轮换后仍可通过校验")
	}
	n2 := NewNonce()
	if src, err := v.Verify("host-10", n2, Proof(newKey, "host-10", n2, ts), ts); err != nil || src != CredSharedKey {
		t.Fatalf("新密钥应可用，src=%q err=%v", src, err)
	}
}

// TestRotateSharedKeyRefusedWhenDisabled 关闭这条路径后轮换必须拒绝：
// 换一把"没人能用"的密钥只会让文件与运行时状态分叉，而不是修好什么。
func TestRotateSharedKeyRefusedWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	v := NewVerifier("some-key")
	v.DisableSharedKey()
	if _, err := v.RotateSharedKey(path); err == nil {
		t.Fatal("已关闭的路径不应被轮换「顺手打开」")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("拒绝轮换时不应写文件")
	}
	if v.SharedKey() != "" {
		t.Fatal("已关闭路径下不该给出明文")
	}
	if _, err := NewVerifier("k").RotateSharedKey(""); err == nil {
		t.Fatal("空路径应被拒绝")
	}
}

// TestRotateSharedKeySafeUnderConcurrentVerify 轮换与校验并发跑：
// 任何锁外读 v.key 的写法都会被 -race 抓出来，而它在单线程测试里永远"通过"。
func TestRotateSharedKeySafeUnderConcurrentVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	v := NewVerifier("first-key")
	if _, err := v.RotateSharedKey(path); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ts := time.Now().Unix()
				n := NewNonce()
				// 不关心校验结果（密钥可能刚被换掉），只关心会不会读到半个状态
				_, _ = v.Verify("host-x", n, Proof(v.SharedKey(), "host-x", n, ts), ts)
				_ = v.KeyFingerprint()
				_ = v.SharedKeyEnabled()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := v.RotateSharedKey(path); err != nil {
				t.Errorf("第 %d 次轮换失败: %v", i, err)
				break
			}
		}
		close(stop)
	}()
	wg.Wait()
}

// TestNoCredentialPathMeansDisabled 既无共享密钥又无令牌存储时，Enabled 必须为 false：
// "是否鉴权"由调用方按这个开关决定，Verify 自身不能充当闸门。
func TestNoCredentialPathMeansDisabled(t *testing.T) {
	v := NewVerifier("")
	if v.Enabled() {
		t.Fatal("未配置任何凭据路径时 Enabled 应为 false")
	}
	if v.Tokens() != nil {
		t.Fatal("未接入存储时 Tokens 应为 nil")
	}
}

// TestVerifyPrefersToken 同一台机器两种凭据都能过时，来源记为令牌（迁移盘点才准）。
func TestVerifyPrefersToken(t *testing.T) {
	store := newTestStore(t)
	v := NewVerifier("shared")
	v.SetEnrollStore(store)
	secret, _, err := store.CreateToken("host-01", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	nonce := NewNonce()
	ts := time.Now().Unix()
	src, err := v.Verify("host-01", nonce, Proof(secret, "host-01", nonce, ts), ts)
	if err != nil || src != CredToken {
		t.Fatalf("应记为令牌来源，得到 src=%q err=%v", src, err)
	}
}

// TestReplayStillBlocked 重构校验流程后，nonce 重放保护不能丢。
func TestReplayStillBlocked(t *testing.T) {
	v := NewVerifier("k")
	nonce := NewNonce()
	ts := time.Now().Unix()
	proof := Proof("k", "h", nonce, ts)
	if _, err := v.Verify("h", nonce, proof, ts); err != nil {
		t.Fatalf("首次应通过: %v", err)
	}
	if _, err := v.Verify("h", nonce, proof, ts); err == nil || !strings.Contains(err.Error(), "重放") {
		t.Fatalf("重放必须被拒且原因明确，实际: %v", err)
	}
}

// TestClockSkewRejected 时间窗校验对两条路径都生效。
func TestClockSkewRejected(t *testing.T) {
	store := newTestStore(t)
	v := NewVerifier("k")
	v.SetEnrollStore(store)
	secret, _, err := store.CreateToken("host-01", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	nonce := NewNonce()
	old := time.Now().Add(-10 * time.Minute).Unix()
	if _, err := v.Verify("host-01", nonce, Proof(secret, "host-01", nonce, old), old); err == nil {
		t.Fatal("超窗令牌证明必须被拒")
	}
	if _, err := v.Verify("host-01", NewNonce(), Proof("k", "host-01", nonce, old), old); err == nil {
		t.Fatal("超窗共享密钥证明必须被拒")
	}
}

// TestStorePersistsAcrossReload 重启后令牌状态必须保持：已用过的不能复活。
func TestStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), EnrollStoreFileName)
	s1, err := NewEnrollStore(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s1.CreateToken("host-01", "rack-2", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.CheckOrBind("host-01", "aabbcc", CredToken, true); err != nil {
		t.Fatal(err)
	}
	s2, err := NewEnrollStore(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if len(s2.Bindings()) != 1 || s2.Bindings()[0].SPKIHash != "aabbcc" {
		t.Fatalf("绑定未持久化: %+v", s2.Bindings())
	}
	v := NewVerifier("")
	v.SetEnrollStore(s2)
	nonce := NewNonce()
	ts := time.Now().Unix()
	if _, err := v.Verify("host-01", nonce, Proof(secret, "host-01", nonce, ts), ts); err != nil {
		t.Fatalf("重载后未使用的令牌应仍可用: %v", err)
	}
	nonce2 := NewNonce()
	if _, err := v.Verify("host-01", nonce2, Proof(secret, "host-01", nonce2, ts), ts); err == nil {
		t.Fatal("重载后一次性令牌仍可复用")
	}
}

// TestTokenListNeverLeaksSecret 列表接口不得带出令牌明文。
func TestTokenListNeverLeaksSecret(t *testing.T) {
	store := newTestStore(t)
	secret, _, err := store.CreateToken("host-01", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range store.ListTokens() {
		for _, val := range v {
			if s, ok := val.(string); ok && strings.Contains(s, secret) {
				t.Fatal("令牌列表里出现了明文")
			}
		}
	}
	// 明文只在创建时返回一次
	if secret == "" || len(secret) < 32 {
		t.Fatalf("令牌长度异常: %q", secret)
	}
}

// TestTokenCaps 单台主机与总量的令牌数上限（防止签发接口变成内存放大器）。
func TestTokenCaps(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < maxTokensPerAgent; i++ {
		if _, _, err := store.CreateToken("host-01", "", 0); err != nil {
			t.Fatalf("第 %d 枚不应触顶: %v", i+1, err)
		}
	}
	if _, _, err := store.CreateToken("host-01", "", 0); err == nil {
		t.Fatal("单台超过上限应被拒")
	}
	// 换一台主机仍可继续（额度按主机计）
	if _, _, err := store.CreateToken("host-02", "", 0); err != nil {
		t.Fatalf("其它主机不应被牵连: %v", err)
	}
	// 吊销一台的未使用令牌后额度释放
	if n := store.RevokeAgentTokens("host-01"); n != maxTokensPerAgent {
		t.Fatalf("应吊销 %d 枚，实际 %d", maxTokensPerAgent, n)
	}
	if _, _, err := store.CreateToken("host-01", "", 0); err != nil {
		t.Fatalf("吊销后应可再签: %v", err)
	}
}

// TestCheckOrBindTOFU TOFU：换公钥只有令牌授权才放行。
func TestCheckOrBindTOFU(t *testing.T) {
	store := newTestStore(t)
	if err := store.CheckOrBind("host-01", "key-A", CredSharedKey, false); err != nil {
		t.Fatalf("首次绑定应成功: %v", err)
	}
	if err := store.CheckOrBind("host-01", "key-A", CredSharedKey, false); err != nil {
		t.Fatalf("同一公钥重复换取应放行: %v", err)
	}
	// 共享密钥泄露后拿同一 ID 换新生成对 → 拒
	if err := store.CheckOrBind("host-01", "key-B", CredSharedKey, false); err == nil {
		t.Fatal("仅凭共享密钥不得换发新公钥")
	} else if !strings.Contains(err.Error(), "一次性注册令牌") {
		t.Fatalf("错误信息应给出恢复路径，实际: %v", err)
	}
	// 运维为该主机签了新令牌 = 明确授权 → 放行并更新绑定
	if err := store.CheckOrBind("host-01", "key-B", CredToken, true); err != nil {
		t.Fatalf("令牌授权的重绑应成功: %v", err)
	}
	b := store.Bindings()
	if len(b) != 1 || b[0].SPKIHash != "key-B" || b[0].Rebinds != 1 {
		t.Fatalf("重绑后状态异常: %+v", b)
	}
	if !store.ResetBinding("host-01") {
		t.Fatal("清除绑定应成功")
	}
	if len(store.Bindings()) != 0 {
		t.Fatal("绑定未清除")
	}
}

// TestUnboundTokenCannotRebindExistingHost 回归：未绑定令牌（任意主机可用一次）
// 只能用于首次接入，不能把已有主机的证书公钥换掉 —— 否则令牌泄露即等于
// 任意离线主机的身份可被接管。绑定令牌才是"这台主机换机/重装"的授权。
func TestUnboundTokenCannotRebindExistingHost(t *testing.T) {
	store := newTestStore(t)
	// 首次绑定：未绑定令牌允许（新机器还没有绑定记录）
	if err := store.CheckOrBind("brand-new", "key-new", CredToken, false); err != nil {
		t.Fatalf("未绑定令牌用于首次接入应放行: %v", err)
	}
	// 已有绑定 → 换公钥：未绑定令牌必须被拒
	if err := store.CheckOrBind("host-01", "key-A", CredToken, true); err != nil {
		t.Fatalf("绑定令牌首次绑定应成功: %v", err)
	}
	if err := store.CheckOrBind("host-01", "key-B", CredToken, false); err == nil {
		t.Fatal("未绑定令牌不得重绑已有主机")
	} else if !strings.Contains(err.Error(), "绑定到该 agent_id") {
		t.Fatalf("错误信息应给出恢复路径，实际: %v", err)
	}
	// 原绑定保持不变
	b := store.Bindings()
	if len(b) != 2 {
		t.Fatalf("绑定数量异常: %+v", b)
	}
	for _, item := range b {
		if item.AgentID == "host-01" && item.SPKIHash != "key-A" {
			t.Fatalf("被拒的重绑不应修改原绑定: %+v", item)
		}
	}
}

// TestSpkiHashOfPEM 公钥指纹要能从签发的证书 PEM 中取到，且不同密钥对不同值。
//
// TOFU 完全依赖这个指纹：同一 agent_id 换密钥必须换成不同的指纹，
// 否则"冒名换证"会和正常重签长得一样，闸就形同虚设。
func TestSpkiHashOfPEM(t *testing.T) {
	id, err := NewIdentity("Acme")
	if err != nil {
		t.Fatal(err)
	}
	csrA, _ := csrPEMFor(t)
	csrB, _ := csrPEMFor(t)
	pemA, err := id.AgentCertPEM("host-01", csrA)
	if err != nil {
		t.Fatal(err)
	}
	pemB, err := id.AgentCertPEM("host-01", csrB)
	if err != nil {
		t.Fatal(err)
	}
	hA, hB := SpkiHashOfPEM(pemA), SpkiHashOfPEM(pemB)
	if hA == "" || hA == hB {
		t.Fatalf("两张不同密钥的证书指纹应不同且非空: %q vs %q", hA, hB)
	}
	// 同一份 CSR 重签（证书到期轮换）指纹必须稳定
	pemA2, err := id.AgentCertPEM("host-01", csrA)
	if err != nil {
		t.Fatal(err)
	}
	if SpkiHashOfPEM(pemA2) != hA {
		t.Fatal("同一公钥重签后指纹变化，会把正常轮换误判为冒名")
	}
	if SpkiHashOfPEM("not pem") != "" {
		t.Fatal("非法 PEM 应返回空串（调用方据此跳过绑定检查）")
	}
}
