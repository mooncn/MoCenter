package auth

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// TestLoginGuardUsernameKeyCapped 用户名作为 map 键必须限长，避免用超长键占驻内存。
func TestLoginGuardUsernameKeyCapped(t *testing.T) {
	long := strings.Repeat("x", loginUserKeyMax*40)
	key := loginUserKey("  " + strings.ToUpper(long) + "  ")
	if utf8.RuneCountInString(key) > loginUserKeyMax {
		t.Fatalf("键应截断到 %d 个字符，实际 %d", loginUserKeyMax, utf8.RuneCountInString(key))
	}
	// 前缀相同的两个超长用户名共用一条记录：键集合有界，退避依然生效
	g := newTestGuard(&time.Time{})
	for i := 0; i < loginMaxFails; i++ {
		g.Fail(long + "a")
	}
	if ok, _ := g.Allow("1.2.3.4:1", long+"b"); ok {
		t.Fatal("超长用户名同样应受按用户名退避的约束")
	}
	// 多字节输入截断后必须是合法 UTF-8（截在半个 rune 上会让键变得不可比对）
	if !utf8.ValidString(loginUserKey(strings.Repeat("中", loginUserKeyMax*2))) {
		t.Fatal("截断结果破坏了 UTF-8 编码")
	}
}

// TestLoginGuardFailsMapCapped 失败计数表需有容量上限，且回收不牺牲仍在生效的退避。
func TestLoginGuardFailsMapCapped(t *testing.T) {
	now := time.Unix(5000, 0)
	g := newTestGuard(&now)
	for i := 0; i < loginMaxFails; i++ {
		g.Fail("locked") // 进入退避，nextAllowed 在当前时钟之后
	}
	for i := 0; i < loginFailMapLimit+10; i++ {
		g.Fail("u" + itoa(i)) // 未达阈值、当前不拦任何人
	}
	if ok, _ := g.Allow("1.2.3.4:1", "locked"); ok {
		t.Fatal("仍在退避中的用户名必须继续被拦")
	}
	g.mu.Lock()
	n := len(g.fails)
	_, kept := g.fails[loginUserKey("locked")]
	g.mu.Unlock()
	if n > loginFailMapLimit {
		t.Fatalf("失败表应封顶在 %d，实际 %d", loginFailMapLimit, n)
	}
	if !kept {
		t.Fatal("回收时不得丢弃仍在生效的退避记录")
	}
}

// ---- M-12：匿名端点的独立限流桶 ----

// TestPublicRouteBucket 匿名桶按 (route, ip) 计数：单桶封顶，换路由/换 IP 互不影响。
func TestPublicRouteBucket(t *testing.T) {
	now := time.Unix(6000, 0)
	g := newTestGuard(&now)

	allowed := 0
	var wait time.Duration
	for i := 0; i < publicRouteMax+5; i++ {
		if ok, w := g.AllowPublicRoute("1.2.3.4:1000", "nodes"); ok {
			allowed++
		} else {
			wait = w
		}
	}
	if allowed != publicRouteMax {
		t.Fatalf("单 (route, ip) 应只放行 %d 次，实际 %d", publicRouteMax, allowed)
	}
	if wait <= 0 || wait > publicRouteWin {
		t.Fatalf("拒绝时的等待时长应落在 (0, %v]，实际 %v", publicRouteWin, wait)
	}
	// 另一条路由自成一桶：打满 /nodes 不该把 /state 一起锁死
	if ok, _ := g.AllowPublicRoute("1.2.3.4:1000", "state"); !ok {
		t.Fatal("不同路由不应共用额度（一处刷满会误伤整站公开页）")
	}
	// 其它访客不受牵连
	if ok, _ := g.AllowPublicRoute("5.6.7.8:1000", "nodes"); !ok {
		t.Fatal("其它 IP 不应被牵连")
	}
	// 窗口滑过后恢复
	now = now.Add(publicRouteWin + time.Second)
	if ok, _ := g.AllowPublicRoute("1.2.3.4:1000", "nodes"); !ok {
		t.Fatal("窗口滑过后应恢复放行")
	}
}

// TestPublicRouteBudgetSeparateFromLogin 匿名流量与登录额度必须互不吃亏（M-12 的正题）。
//
// 共用一个桶时，多开几个公开页标签（或反代后共享同一出口 IP 的整站访客）就能
// 把管理员的登录尝试次数耗光 —— 那是把 DoS 做成了功能。
func TestPublicRouteBudgetSeparateFromLogin(t *testing.T) {
	now := time.Unix(7000, 0)
	g := newTestGuard(&now)

	for i := 0; i < publicRouteMax*3; i++ {
		g.AllowPublicRoute("1.2.3.4:1", "nodes")
	}
	if ok, _ := g.AllowPublicRoute("1.2.3.4:1", "nodes"); ok {
		t.Fatal("前置条件失效：匿名桶未打满")
	}
	for i := 0; i < loginIPMax; i++ {
		if ok, _ := g.Allow("1.2.3.4:1", "admin"); !ok {
			t.Fatalf("第 %d 次登录被匿名访客挤掉：匿名流量吃了登录额度", i+1)
		}
	}

	// 反向同理：登录洪泛不得让访客看不到状态页
	g2 := newTestGuard(&now)
	for i := 0; i < loginIPMax+10; i++ {
		g2.Allow("1.2.3.4:1", "admin")
	}
	if ok, _ := g2.AllowPublicRoute("1.2.3.4:1", "state"); !ok {
		t.Fatal("登录尝试耗尽了匿名端点额度")
	}
}

// TestPublicRouteStateBounded 匿名桶既会回收过期项，也有容量兜底。
func TestPublicRouteStateBounded(t *testing.T) {
	now := time.Unix(8000, 0)
	g := newTestGuard(&now)
	// 未过期但数量超限：整表作废，宁可丢计数精度也不放任内存无界增长
	g.mu.Lock()
	for i := 0; i < publicRouteMapLimit+16; i++ {
		g.perPublic["nodes|"+itoa(i)] = []time.Time{now}
	}
	g.mu.Unlock()
	if ok, _ := g.AllowPublicRoute("1.2.3.4:1", "nodes"); !ok {
		t.Fatal("回收后应可正常放行")
	}
	g.mu.Lock()
	n := len(g.perPublic)
	g.mu.Unlock()
	if n > publicRouteMapLimit {
		t.Fatalf("匿名桶应封顶在 %d，实际 %d", publicRouteMapLimit, n)
	}

	// 全部过期时逐条回收，而不是只靠容量兜底
	now = now.Add(2 * time.Hour)
	g.AllowPublicRoute("9.9.9.9:1", "state")
	g.mu.Lock()
	n = len(g.perPublic)
	g.mu.Unlock()
	if n > 1 {
		t.Fatalf("过期的匿名记录未清理: %d", n)
	}
}

// TestAuthRateMapCapped 速率表要封顶，且回收触发点不能依赖"有 Token 校验成功"。
//
// 原实现只在 `ValidateToken` 成功时清理 —— 而"拿假 Token 猛刷"这条路径恰恰
// 永远不会成功，既能无限增表又永远不触发回收（M-12）。
func TestAuthRateMapCapped(t *testing.T) {
	a := NewAuth(nil)
	for i := 0; i < rateMapLimit+64; i++ {
		a.AllowRequest("faked-ip-" + itoa(i) + ":1")
	}
	a.mu.Lock()
	n := len(a.rate)
	a.mu.Unlock()
	if n > rateMapLimit {
		t.Fatalf("速率表应封顶在 %d，实际 %d", rateMapLimit, n)
	}
}

// TestAuthRatePruneRunsOnEntry 过期 Token 由请求入口回收，与登录是否成功无关。
func TestAuthRatePruneRunsOnEntry(t *testing.T) {
	a := NewAuth(nil)
	a.SetTokenTTL(10 * time.Millisecond)
	a.RegisterToken("stale", "admin")
	time.Sleep(30 * time.Millisecond)

	// 让"上次回收"退回到一个窗口之前，模拟只靠入口驱动的常态
	a.mu.Lock()
	a.lastRatePrune = time.Time{}
	a.mu.Unlock()
	a.AllowRequest("1.2.3.4:1")

	a.mu.Lock()
	_, still := a.tokens["stale"]
	a.mu.Unlock()
	if still {
		t.Fatal("AllowRequest 未回收过期 Token：假 Token 洪泛路径下回收永远不会发生")
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
