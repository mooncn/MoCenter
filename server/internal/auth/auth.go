// Package auth 提供 API/WebSocket 的 Token 校验、IP 白名单与速率限制。
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"sync"
	"time"
)

// Auth 鉴权器。
type Auth struct {
	tokens  map[string]bool // 有效 Token 集合
	apiKey  string          // 全局 API Key
	whitelist []string      // IP 白名单（CIDR 或精确 IP）
	mu      sync.Mutex
	// 速率限制：按 IP 记录请求时间戳
	rate     map[string][]time.Time
	rateMax  int           // 窗口内最大请求数
	rateWin  time.Duration
}

// NewAuth 创建鉴权器。
func NewAuth(apiKey string, whitelist []string) *Auth {
	a := &Auth{
		tokens:    make(map[string]bool),
		apiKey:    apiKey,
		whitelist: whitelist,
		rate:      make(map[string][]time.Time),
		rateMax:   120,
		rateWin:   time.Minute,
	}
	return a
}

// RegisterToken 注册一个有效 Token（登录成功后颁发）。
func (a *Auth) RegisterToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = true
}

// RevokeToken 吊销 Token。
func (a *Auth) RevokeToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

// ValidateToken 校验 Token。
func (a *Auth) ValidateToken(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens[token] || token == a.apiKey
}

// HashToken 计算 Token 摘要。
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
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
