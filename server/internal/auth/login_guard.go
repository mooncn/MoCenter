package auth

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 登录入口的限流与失败退避默认参数。
const (
	loginIPMax      = 20               // 单 IP 每分钟最多尝试次数
	loginIPWin      = time.Minute      //
	loginMaxFails   = 5                // 连续失败次数阈值，超过后开始退避
	loginBaseDelay  = 2 * time.Second  // 退避基数
	loginMaxDelay   = 30 * time.Second // 退避上限（不做永久锁定，避免被用来把管理员锁在门外）
	loginFailTTL    = 30 * time.Minute // 失败计数保留时长，超时自动清零
	loginIPMapLimit = 4096             // perIP 表上限，防御大量伪造源 IP 造成的内存放大
	// fails 表上限。perIP 只约束 1 分钟窗口，失败计数却要驻留 loginFailTTL，
	// 换用户名即可跨窗口累积，因此需要单独封顶。
	loginFailMapLimit = 4096
	// 用户名键的最大字符数。它来自请求体、不受账号表约束，不限长就等于让匿名者用
	// 一个小请求换走一条驻留 30 分钟的大键。真实用户名远短于此。
	loginUserKeyMax = 64

	// 匿名端点的频率闸门（M-12）。与登录刻意分开的两条理由：
	//   - 语义不同：登录要防"猜口令"，公开页要防"刷接口"。共用一个桶，
	//     就等于多开几个公开页标签（或反代后整站访客共享的那一个 IP）能直接
	//     消耗掉管理员的登录额度 —— 这是把 DoS 做成了功能。
	//   - 合法量级不同：公开详情页每 15s 轮询，三个标签页就是 12 次/分，
	//     给登录用的 20/分/IP 套在它身上必然自我锁死。
	publicRouteMax      = 30 // 单 IP 单个公开路由每分钟上限
	publicRouteWin      = time.Minute
	publicRouteMapLimit = 4096 // (route, ip) 键数上限，挡住伪造源 IP 的内存放大
)

// loginUserKey 归一化失败退避的 map 键（小写 + 去空白 + 限长）。
//
// 限长是截断而非拒绝：超长键只会共用同一条退避记录，退避依旧生效，
// 而合法用户名不受影响（也不会因此被锁在门外）。
func loginUserKey(username string) string {
	key := strings.ToLower(strings.TrimSpace(username))
	if utf8.RuneCountInString(key) <= loginUserKeyMax {
		return key
	}
	return string([]rune(key)[:loginUserKeyMax])
}

// LoginGuard 登录入口的访问控制：按 IP 限流 + 按用户名失败退避。
//
// 为什么需要：/api/login 是唯一无法要求 Token 的入口，此前完全没有限流与锁定，
// 配合弱口令可以无限次在线尝试。
//
// 设计取舍：**不做永久锁定**。永久锁定会被攻击者用来把管理员锁在门外（账号 DoS），
// 因此只做「失败越多、下一次尝试等待越久」的指数退避，上限 30 秒 —— 足以把
// 在线爆破压到不可行，同时管理员最多等半分钟就能重试。
type LoginGuard struct {
	mu    sync.Mutex
	perIP map[string][]time.Time
	fails map[string]*failState
	// perPublic 匿名端点的 (route, ip) 滑窗，与登录的 perIP 完全分开计数。
	perPublic map[string][]time.Time
	now       func() time.Time // 测试可注入
}

type failState struct {
	count       int
	lastFail    time.Time
	nextAllowed time.Time
}

// NewLoginGuard 创建登录守卫。
func NewLoginGuard() *LoginGuard {
	return &LoginGuard{
		perIP:     make(map[string][]time.Time),
		fails:     make(map[string]*failState),
		perPublic: make(map[string][]time.Time),
		now:       time.Now,
	}
}

// Allow 判断本次登录尝试是否放行；不放行时返回建议的等待时长。
func (g *LoginGuard) Allow(remoteAddr, username string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	ip := ClientIP(remoteAddr)
	g.pruneLocked(now)

	// 1) 按 IP 限流
	if ts := g.perIP[ip]; len(ts) >= loginIPMax {
		// 最早一条滑出窗口的时间就是可重试时间
		wait := ts[0].Add(loginIPWin).Sub(now)
		if wait < 0 {
			wait = 0
		}
		return false, wait
	}

	// 2) 按用户名退避
	if st := g.fails[loginUserKey(username)]; st != nil {
		if now.Before(st.nextAllowed) {
			return false, st.nextAllowed.Sub(now)
		}
	}

	g.perIP[ip] = append(g.perIP[ip], now)
	return true, 0
}

// AllowPublicRoute 判断某个匿名端点对该来源 IP 是否放行。
//
// 与 Allow 的差别是本体的设计意图（不是"另一种调用方式"）：
//   - 键是 (route, ip)，因此「详情页轮询」不吃「列表页」的额度，也不吃登录的额度；
//   - 不查失败退避表 —— 匿名端点没有"用户名"可供退避，硬套只会往 fails 里
//     塞进永远无人消费的条目。
func (g *LoginGuard) AllowPublicRoute(remoteAddr, route string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	key := route + "|" + ClientIP(remoteAddr)
	g.pruneLocked(now)

	ts := g.perPublic[key]
	if len(ts) >= publicRouteMax {
		wait := ts[0].Add(publicRouteWin).Sub(now)
		if wait < 0 {
			wait = 0
		}
		return false, wait
	}
	g.perPublic[key] = append(ts, now)
	return true, 0
}

// Fail 记录一次登录失败。
func (g *LoginGuard) Fail(username string) {
	key := loginUserKey(username)
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	st := g.fails[key]
	if st == nil {
		st = &failState{}
		g.fails[key] = st
	}
	st.count++
	st.lastFail = now
	if st.count >= loginMaxFails {
		// 第 maxFails 次失败之后，下一次尝试需要等待 2^(超出次数) 秒，封顶 loginMaxDelay。
		delay := loginBaseDelay
		for i := 0; i < st.count-loginMaxFails; i++ {
			delay *= 2
			if delay >= loginMaxDelay {
				break
			}
		}
		if delay > loginMaxDelay {
			delay = loginMaxDelay
		}
		st.nextAllowed = now.Add(delay)
	}
}

// Success 登录成功后清零该用户名的失败计数。
func (g *LoginGuard) Success(username string) {
	key := loginUserKey(username)
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, key)
}

// pruneLocked 清理过期记录，避免 map 无界增长。
func (g *LoginGuard) pruneLocked(now time.Time) {
	cutoff := now.Add(-loginIPWin)
	for ip, ts := range g.perIP {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(g.perIP, ip)
			continue
		}
		g.perIP[ip] = kept
	}
	// perIP 过大时直接丢弃最旧的一半（极端情况下牺牲精度换内存可控）
	if len(g.perIP) > loginIPMapLimit {
		g.perIP = make(map[string][]time.Time)
	}
	// 匿名桶与登录桶同样会随伪造源 IP 单调增长，回收口径保持一致。
	rcutoff := now.Add(-publicRouteWin)
	for k, ts := range g.perPublic {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(rcutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(g.perPublic, k)
			continue
		}
		g.perPublic[k] = kept
	}
	if len(g.perPublic) > publicRouteMapLimit {
		g.perPublic = make(map[string][]time.Time)
	}
	for k, st := range g.fails {
		if now.Sub(st.lastFail) > loginFailTTL {
			delete(g.fails, k)
		}
	}
	// TTL 清理只看"多久没失败"，海量不同用户名轮流尝试即可绕过它，所以再叠一层容量兜底。
	// 超限时丢弃退避已到期的记录（它们当前不拦任何人），仍在生效的退避保留。
	if len(g.fails) > loginFailMapLimit {
		for k, st := range g.fails {
			if !now.Before(st.nextAllowed) {
				delete(g.fails, k)
			}
		}
	}
}

// LoginIPOf 从 RemoteAddr 提取源 IP（供审计与日志使用）。
func LoginIPOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return remoteAddr
}

// RetryAfter 生成 Retry-After 头值（秒，向上取整，至少 1）。
func RetryAfter(d time.Duration) string {
	sec := int(d / time.Second)
	if d%time.Second != 0 {
		sec++
	}
	if sec < 1 {
		sec = 1
	}
	return strconv.Itoa(sec)
}
