package auth

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
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
)

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
	now   func() time.Time // 测试可注入
}

type failState struct {
	count       int
	lastFail    time.Time
	nextAllowed time.Time
}

// NewLoginGuard 创建登录守卫。
func NewLoginGuard() *LoginGuard {
	return &LoginGuard{
		perIP: make(map[string][]time.Time),
		fails: make(map[string]*failState),
		now:   time.Now,
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
	if st := g.fails[strings.ToLower(strings.TrimSpace(username))]; st != nil {
		if now.Before(st.nextAllowed) {
			return false, st.nextAllowed.Sub(now)
		}
	}

	g.perIP[ip] = append(g.perIP[ip], now)
	return true, 0
}

// Fail 记录一次登录失败。
func (g *LoginGuard) Fail(username string) {
	key := strings.ToLower(strings.TrimSpace(username))
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
	key := strings.ToLower(strings.TrimSpace(username))
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
	for k, st := range g.fails {
		if now.Sub(st.lastFail) > loginFailTTL {
			delete(g.fails, k)
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
