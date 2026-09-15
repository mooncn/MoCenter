package auth

import (
	"testing"
	"time"
)

// TestTokenExpires Token 必须有过期时间，且过期后立即失效。
func TestTokenExpires(t *testing.T) {
	a := NewAuth(nil)
	a.SetTokenTTL(50 * time.Millisecond)
	a.RegisterToken("tok", "admin")
	if !a.ValidateToken("tok") {
		t.Fatal("刚颁发的 Token 应有效")
	}
	time.Sleep(80 * time.Millisecond)
	if a.ValidateToken("tok") {
		t.Fatal("超过 TTL 的 Token 必须失效（此前永不过期）")
	}
	if op := a.OperatorOf("tok"); op != "unknown" {
		t.Fatalf("过期 Token 的操作者应为 unknown，实际 %q", op)
	}
}

// TestTokenSlidingRenewal 活跃会话在 TTL 内反复校验不会被踢出。
func TestTokenSlidingRenewal(t *testing.T) {
	a := NewAuth(nil)
	a.SetTokenTTL(60 * time.Millisecond)
	a.RegisterToken("tok", "admin")
	for i := 0; i < 5; i++ {
		time.Sleep(25 * time.Millisecond)
		if !a.ValidateToken("tok") {
			t.Fatalf("第 %d 次校验失败：滑动续期未生效，活跃会话被误踢", i+1)
		}
	}
}

// TestTokenTTLDefault 默认 TTL 必须是有限值。
func TestTokenTTLDefault(t *testing.T) {
	a := NewAuth(nil)
	if a.TokenTTL() != DefaultTokenTTL {
		t.Fatalf("默认 TTL 应为 %v，实际 %v", DefaultTokenTTL, a.TokenTTL())
	}
	if DefaultTokenTTL <= 0 || DefaultTokenTTL > 7*24*time.Hour {
		t.Fatalf("默认 TTL 取值不合理: %v", DefaultTokenTTL)
	}
	// 非法值回落到默认
	a.SetTokenTTL(0)
	if a.TokenTTL() != DefaultTokenTTL {
		t.Fatal("TTL<=0 应回落到默认值")
	}
	a.SetTokenTTL(-time.Hour)
	if a.TokenTTL() != DefaultTokenTTL {
		t.Fatal("负 TTL 应回落到默认值")
	}
}

// TestRevokeUserTokens 改密码/改 OTP 时应能吊销该用户的其它会话。
func TestRevokeUserTokens(t *testing.T) {
	a := NewAuth(nil)
	a.RegisterToken("t1", "admin")
	a.RegisterToken("t2", "admin")
	a.RegisterToken("t3", "bob")

	n := a.RevokeUserTokens("admin", "t1")
	if n != 1 {
		t.Fatalf("应吊销 1 个，实际 %d", n)
	}
	if !a.ValidateToken("t1") {
		t.Fatal("keep 指定的 Token 应保留（当前会话不被踢出）")
	}
	if a.ValidateToken("t2") {
		t.Fatal("同用户其它 Token 应被吊销")
	}
	if !a.ValidateToken("t3") {
		t.Fatal("其它用户的 Token 不应受影响")
	}

	// keep 为空 → 全部吊销
	a.RevokeUserTokens("admin", "")
	if a.ValidateToken("t1") {
		t.Fatal("keep 为空时应吊销该用户全部 Token")
	}
}

// TestRevokeTokenWorks 单 Token 吊销（登出）应生效。
func TestRevokeTokenWorks(t *testing.T) {
	a := NewAuth(nil)
	a.RegisterToken("t", "admin")
	if !a.ValidateToken("t") {
		t.Fatal("应有效")
	}
	a.RevokeToken("t")
	if a.ValidateToken("t") {
		t.Fatal("登出后 Token 必须立即失效")
	}
}

// TestRegisterTokenEmptyIgnored 空 Token 不应被登记（否则空串会变成万能凭证）。
func TestRegisterTokenEmptyIgnored(t *testing.T) {
	a := NewAuth(nil)
	a.RegisterToken("", "admin")
	if a.ValidateToken("") {
		t.Fatal("空 Token 必须始终被拒绝")
	}
}

// TestIPAllowedSharedHelper 复用型 IP 匹配（端口映射也用它）。
func TestIPAllowedSharedHelper(t *testing.T) {
	cases := []struct {
		list []string
		addr string
		want bool
	}{
		{nil, "1.2.3.4:5", true},
		{[]string{}, "1.2.3.4:5", true},
		{[]string{"10.0.0.0/8"}, "10.9.9.9:1", true},
		{[]string{"10.0.0.0/8"}, "11.0.0.1:1", false},
		{[]string{"1.2.3.4"}, "1.2.3.4:1", true},
		{[]string{"1.2.3.4"}, "1.2.3.5:1", false},
		{[]string{" 10.0.0.0/8 ", " 1.1.1.1 "}, "1.1.1.1:9", true}, // 容忍空格
		{[]string{"bad-cidr"}, "1.2.3.4:1", false},
		{[]string{"10.0.0.0/8"}, "not-an-ip", false},
	}
	for _, c := range cases {
		if got := IPAllowed(c.list, c.addr); got != c.want {
			t.Errorf("IPAllowed(%v, %q) = %v, want %v", c.list, c.addr, got, c.want)
		}
	}
}

// TestClientIP 去端口。
func TestClientIP(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::1]:9000", "::1"},
		{"1.2.3.4", "1.2.3.4"},
	} {
		if got := ClientIP(c.in); got != c.want {
			t.Errorf("ClientIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- LoginGuard ----

func newTestGuard(now *time.Time) *LoginGuard {
	g := NewLoginGuard()
	g.now = func() time.Time { return *now }
	return g
}

// TestLoginGuardIPRateLimit 单 IP 尝试次数受限。
func TestLoginGuardIPRateLimit(t *testing.T) {
	now := time.Unix(0, 0)
	g := newTestGuard(&now)
	allowed := 0
	for i := 0; i < loginIPMax+5; i++ {
		if ok, _ := g.Allow("1.2.3.4:1000", "admin"); ok {
			allowed++
		}
	}
	if allowed != loginIPMax {
		t.Fatalf("单 IP 应只放行 %d 次，实际 %d", loginIPMax, allowed)
	}
	// 窗口滑过后恢复
	now = now.Add(loginIPWin + time.Second)
	if ok, _ := g.Allow("1.2.3.4:1000", "admin"); !ok {
		t.Fatal("窗口滑过后应恢复放行")
	}
	// 不同 IP 不受影响
	if ok, _ := g.Allow("5.6.7.8:1000", "admin"); !ok {
		t.Fatal("其它 IP 不应被牵连")
	}
}

// TestLoginGuardBackoffAfterFailures 连续失败后需要等待，且等待随失败次数增长。
func TestLoginGuardBackoffAfterFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newTestGuard(&now)

	// 未达阈值前不放宽也不阻塞
	for i := 0; i < loginMaxFails; i++ {
		if ok, _ := g.Allow("1.2.3.4:1", "admin"); !ok {
			t.Fatalf("第 %d 次不应被阻塞", i+1)
		}
		g.Fail("admin")
	}

	ok, wait := g.Allow("1.2.3.4:1", "admin")
	if ok {
		t.Fatal("超过失败阈值后应要求等待")
	}
	if wait <= 0 {
		t.Fatalf("应返回正的等待时长，实际 %v", wait)
	}

	// 等够之后可以再试
	now = now.Add(wait + time.Millisecond)
	if ok, _ := g.Allow("1.2.3.4:1", "admin"); !ok {
		t.Fatal("等待结束后应放行")
	}

	// 继续失败 → 等待变长
	prev := wait
	g.Fail("admin")
	_, wait2 := g.Allow("1.2.3.4:1", "admin")
	if wait2 <= prev {
		t.Fatalf("退避应随失败次数增长: %v -> %v", prev, wait2)
	}
	// 但不超过上限（避免变成永久锁定 / 账号 DoS）
	now = now.Add(wait2 + time.Millisecond)
	for i := 0; i < 20; i++ {
		g.Fail("admin")
	}
	now = now.Add(time.Hour)
	g.Allow("1.2.3.4:1", "admin")
	g.Fail("admin")
	_, capped := g.Allow("1.2.3.4:1", "admin")
	if capped > loginMaxDelay {
		t.Fatalf("退避上限应为 %v，实际 %v", loginMaxDelay, capped)
	}
}

// TestLoginGuardSuccessResets 登录成功后失败计数清零。
func TestLoginGuardSuccessResets(t *testing.T) {
	now := time.Unix(2000, 0)
	g := newTestGuard(&now)
	for i := 0; i < loginMaxFails+3; i++ {
		g.Allow("1.2.3.4:1", "admin")
		g.Fail("admin")
	}
	if ok, _ := g.Allow("1.2.3.4:1", "admin"); ok {
		t.Fatal("应处于退避中")
	}
	g.Success("admin")
	if ok, _ := g.Allow("1.2.3.4:1", "admin"); !ok {
		t.Fatal("成功后应立即放行")
	}
}

// TestLoginGuardUsernameCaseInsensitive 用户名大小写不同不应绕过退避。
func TestLoginGuardUsernameCaseInsensitive(t *testing.T) {
	now := time.Unix(3000, 0)
	g := newTestGuard(&now)
	for i := 0; i < loginMaxFails+1; i++ {
		g.Allow("1.2.3.4:1", "Admin")
		g.Fail("Admin")
	}
	if ok, _ := g.Allow("1.2.3.4:1", "aDmIn"); ok {
		t.Fatal("大小写变体不应绕过按用户名的退避")
	}
}

// TestLoginGuardPrunesState 状态表会被清理，避免内存无界增长。
func TestLoginGuardPrunesState(t *testing.T) {
	now := time.Unix(4000, 0)
	g := newTestGuard(&now)
	for i := 0; i < 100; i++ {
		g.Allow("10.0.0."+itoa(i%255)+":1", "u")
		g.Fail("u")
	}
	now = now.Add(2 * time.Hour)
	// 触发清理
	g.Allow("1.2.3.4:1", "someone")
	g.mu.Lock()
	nIP, nFail := len(g.perIP), len(g.fails)
	g.mu.Unlock()
	if nIP > 2 {
		t.Errorf("过期 IP 记录未清理: %d", nIP)
	}
	if nFail > 1 {
		t.Errorf("过期失败记录未清理: %d", nFail)
	}
}

// TestRetryAfter 头值取整且至少 1 秒。
func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "1"}, {100 * time.Millisecond, "1"}, {time.Second, "1"},
		{1500 * time.Millisecond, "2"}, {3 * time.Second, "3"},
	} {
		if got := RetryAfter(c.d); got != c.want {
			t.Errorf("RetryAfter(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
