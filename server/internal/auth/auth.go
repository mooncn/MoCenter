// Package auth 提供 API/WebSocket 的 Token 校验、IP 白名单与速率限制。
package auth

import (
	"net"
	"strings"
	"sync"
	"time"
)

// Auth 鉴权器。
//
// 只有一种凭证：登录成功后颁发、并绑定用户名的 Token（见 RegisterToken）。
// 早前的「全局 API Key」已移除 —— 它是一把对所有接口都有效的万能口令，
// 会绕过账号体系，且审计里只能记成 "api-key"，与「用户名+密码(+OTP)」的单一登录
// 模型冲突。Agent 侧从不使用它（见 Auth 的说明与 handleAgentConn）。
type Auth struct {
	tokens    map[string]string // 有效 Token -> 登录用户名（审计归属）
	whitelist []string          // IP 白名单（CIDR 或精确 IP）
	mu        sync.Mutex
	// 速率限制：按 IP 记录请求时间戳
	rate    map[string][]time.Time
	rateMax int // 窗口内最大请求数
	rateWin time.Duration
}

// NewAuth 创建鉴权器。
func NewAuth(whitelist []string) *Auth {
	a := &Auth{
		tokens:    make(map[string]string),
		whitelist: whitelist,
		rate:      make(map[string][]time.Time),
		rateMax:   120,
		rateWin:   time.Minute,
	}
	return a
}

// RegisterToken 注册一个有效 Token（登录成功后颁发），并绑定其归属用户名。
func (a *Auth) RegisterToken(token, username string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = username
}

// RevokeToken 吊销 Token。
func (a *Auth) RevokeToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

// ValidateToken 校验 Token：只认登录成功后颁发的、登记在册的 Token。
func (a *Auth) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tokens[token]
	return ok
}

// OperatorOf 返回该 Token 归属的操作者名称，用于审计留痕。
// 账号登录返回用户名；未知 Token 返回 "unknown"。
func (a *Auth) OperatorOf(token string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name, ok := a.tokens[token]; ok && name != "" {
		return name
	}
	return "unknown"
}

// Operator 从 Authorization 请求头解析操作者名称。
func (a *Auth) Operator(authorization string) string {
	return a.OperatorOf(strings.TrimPrefix(authorization, "Bearer "))
}

// IsAllowedIP 判断源 IP 是否在白名单中。白名单为空表示不限制。
func (a *Auth) IsAllowedIP(remoteAddr string) bool {
	if len(a.whitelist) == 0 {
		return true
	}
	ipStr := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ipStr = host
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, w := range a.whitelist {
		if strings.Contains(w, "/") {
			_, cidr, err := net.ParseCIDR(w)
			if err == nil && cidr.Contains(ip) {
				return true
			}
		} else if w == ipStr {
			return true
		}
	}
	return false
}

// AllowRequest 速率限制：每个 IP 在窗口内限制请求数，超出返回 false。
func (a *Auth) AllowRequest(remoteAddr string) bool {
	ipStr := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ipStr = host
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
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
