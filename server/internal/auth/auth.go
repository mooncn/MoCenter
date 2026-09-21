// Package auth 提供 API/WebSocket 的 Token 校验、IP 白名单与速率限制。
package auth

import (
	"net"
	"strings"
	"sync"
	"time"
)

// DefaultTokenTTL 会话 Token 默认有效期。
//
// 之前 Token 永不过期且 RevokeToken 在运行期从不被调用，等于「一次登录永久有效」：
// 一旦 Token 被窃取（XSS、代理访问日志、截图），受害者改密码也救不回来。
const DefaultTokenTTL = 12 * time.Hour

// tokenEntry 一条会话记录。
type tokenEntry struct {
	username  string
	expiresAt time.Time
}

// Auth 鉴权器。
//
// 只有一种凭证：登录成功后颁发、并绑定用户名的 Token（见 RegisterToken）。
type Auth struct {
	tokens    map[string]tokenEntry
	whitelist []string // IP 白名单（CIDR 或精确 IP）
	mu        sync.Mutex
	// 速率限制：按 IP 记录请求时间戳
	rate     map[string][]time.Time
	rateMax  int // 窗口内最大请求数
	rateWin  time.Duration
	tokenTTL time.Duration
	// lastRatePrune 上次全表回收速率记录的时刻。
	//
	// 回收必须有个**不依赖"有人成功登录"**的触发点：`AllowRequest` 是每个请求都会
	// 走的入口，而 Token 洪泛恰恰会让 `ValidateToken` 一直失败（M-12）。
	lastRatePrune time.Time
}

// rateMapLimit 速率表（按 IP 键）的容量上限：挡住"伪造源 IP 无限增表"。
// 超限时先清过期项，仍超限就整表作废 —— 与 LoginGuard 同一口径：
// 计数精度可以让步，内存不可以无界。
const rateMapLimit = 4096

// NewAuth 创建鉴权器。
func NewAuth(whitelist []string) *Auth {
	return &Auth{
		tokens:    make(map[string]tokenEntry),
		whitelist: whitelist,
		rate:      make(map[string][]time.Time),
		rateMax:   120,
		rateWin:   time.Minute,
		tokenTTL:  DefaultTokenTTL,
	}
}

// SetTokenTTL 覆盖 Token 有效期（<=0 表示用默认值）。
func (a *Auth) SetTokenTTL(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d <= 0 {
		d = DefaultTokenTTL
	}
	a.tokenTTL = d
}

// TokenTTL 返回当前 Token 有效期。
func (a *Auth) TokenTTL() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tokenTTL <= 0 {
		return DefaultTokenTTL
	}
	return a.tokenTTL
}

// RegisterToken 注册一个有效 Token（登录成功后颁发），并绑定其归属用户名。
func (a *Auth) RegisterToken(token, username string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = tokenEntry{
		username:  username,
		expiresAt: time.Now().Add(a.ttlLocked()),
	}
}

// RevokeToken 吊销 Token。
func (a *Auth) RevokeToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

// RevokeUserTokens 吊销某用户的全部 Token；keep 非空时保留该 Token
// （用于「改密码后踢掉其它会话，但当前会话继续可用」）。
// 返回被吊销的数量。
func (a *Auth) RevokeUserTokens(username, keep string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for tok, e := range a.tokens {
		if e.username == username && tok != keep {
			delete(a.tokens, tok)
			n++
		}
	}
	return n
}

// ValidateToken 校验 Token：只认登录成功后颁发的、未过期且登记在册的 Token。
// 校验通过会滑动续期（活跃会话不会被强制踢出）。
func (a *Auth) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.tokens[token]
	if !ok {
		return false
	}
	now := time.Now()
	if now.After(e.expiresAt) {
		delete(a.tokens, token)
		return false
	}
	e.expiresAt = now.Add(a.ttlLocked())
	a.tokens[token] = e
	a.pruneLocked(now)
	return true
}

// OperatorOf 返回该 Token 归属的操作者名称，用于审计留痕。
// 账号登录返回用户名；未知或已过期 Token 返回 "unknown"。
func (a *Auth) OperatorOf(token string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.tokens[token]
	if !ok || e.username == "" || time.Now().After(e.expiresAt) {
		return "unknown"
	}
	return e.username
}

// Operator 从 Authorization 请求头解析操作者名称。
func (a *Auth) Operator(authorization string) string {
	return a.OperatorOf(strings.TrimPrefix(authorization, "Bearer "))
}

// ttlLocked 在持锁状态下取 TTL。
func (a *Auth) ttlLocked() time.Duration {
	if a.tokenTTL <= 0 {
		return DefaultTokenTTL
	}
	return a.tokenTTL
}

// pruneLocked 回收过期 Token 与空限流记录，避免 map 无界增长。
func (a *Auth) pruneLocked(now time.Time) {
	for tok, e := range a.tokens {
		if now.After(e.expiresAt) {
			delete(a.tokens, tok)
		}
	}
	cutoff := now.Add(-a.rateWin)
	for ip, ts := range a.rate {
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(a.rate, ip)
			continue
		}
		a.rate[ip] = kept
	}
}

// ClientIP 从 RemoteAddr 取出 IP 部分（去掉端口）。
func ClientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// IPAllowed 判断源地址是否命中白名单（CIDR 或精确 IP）。
// list 为空表示不限制。
//
// 导出它是为了让端口映射等模块复用同一套匹配逻辑，避免两处实现语义漂移。
func IPAllowed(list []string, remoteAddr string) bool {
	return addrAllowed(list, parseAddrIP(remoteAddr))
}

// AddrAllowed 同 IPAllowed，但直接接受 net.Addr（如 net.Conn.RemoteAddr()）。
func AddrAllowed(list []string, addr net.Addr) bool {
	if addr == nil {
		return false
	}
	var ip net.IP
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip = a.IP
	case *net.UDPAddr:
		ip = a.IP
	case *net.IPAddr:
		ip = a.IP
	default:
		ip = parseAddrIP(addr.String())
	}
	return addrAllowed(list, ip)
}

func parseAddrIP(remoteAddr string) net.IP {
	ipStr := ClientIP(remoteAddr)
	return net.ParseIP(ipStr)
}

func addrAllowed(list []string, ip net.IP) bool {
	if len(list) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	for _, w := range list {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if strings.Contains(w, "/") {
			if _, cidr, err := net.ParseCIDR(w); err == nil && cidr.Contains(ip) {
				return true
			}
		} else if pip := net.ParseIP(w); pip != nil && pip.Equal(ip) {
			return true
		}
	}
	return false
}

// IsAllowedIP 判断源 IP 是否在白名单中。白名单为空表示不限制。
func (a *Auth) IsAllowedIP(remoteAddr string) bool {
	return IPAllowed(a.whitelist, remoteAddr)
}

// AllowRequest 速率限制：每个 IP 在窗口内限制请求数，超出返回 false。
//
// 这里也是速率表的回收点：以前只有 `ValidateToken` 成功才会 `pruneLocked`，
// 于是"拿假 Token 猛刷"这条路径既能无限增表、又永远不会触发回收。
func (a *Auth) AllowRequest(remoteAddr string) bool {
	ipStr := ClientIP(remoteAddr)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.pruneRateLocked(now)
	cutoff := now.Add(-a.rateWin)
	ts := a.rate[ipStr]
	filtered := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) >= a.rateMax {
		a.rate[ipStr] = filtered
		return false
	}
	filtered = append(filtered, now)
	a.rate[ipStr] = filtered
	return true
}

// pruneRateLocked 回收过期的速率记录（顺带回收过期 Token），至多每个窗口周期一次；
// 表仍超限时整表作废。
func (a *Auth) pruneRateLocked(now time.Time) {
	if a.lastRatePrune.IsZero() || now.Sub(a.lastRatePrune) >= a.rateWin {
		a.lastRatePrune = now
		a.pruneLocked(now)
	}
	// 长度检查每次请求都做：它是 O(1)，而全表回收至多每个窗口一次 ——
	// 两次回收之间仍要有人挡住无限增表，否则上限只是"一分钟内可以涨多少"。
	if len(a.rate) > rateMapLimit {
		a.rate = make(map[string][]time.Time)
	}
}
