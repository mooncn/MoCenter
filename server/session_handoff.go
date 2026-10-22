package main

import (
	"mocenter/server/internal/logx"
	"net/http"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/auth"
)

// 会话交接：修改 Web 监听端口后，浏览器会进入一个新的 origin（端口不同），
// localStorage 里的 Token 不会带过去。为了让「保存后直接跳到新地址」不用重新登录，
// 旧地址先申请一个一次性交接码，新地址用它换一枚新 Token。
//
// 安全约束：
//   - 高熵（128 位）、单次使用、60 秒过期；
//   - 绑定申请时的客户端 IP；
//   - 兑换时原 Token 必须仍然有效（改密码/吊销后交接码随之失效）；
//   - 兑换接口免登录，但走同一套 IP 白名单与限流。
const (
	handoffTTL      = 60 * time.Second
	handoffMaxItems = 256
)

type handoffEntry struct {
	username  string
	ip        string
	token     string
	expiresAt time.Time
}

type handoffStore struct {
	mu    sync.Mutex
	items map[string]handoffEntry
	now   func() time.Time
}

var sessionHandoffs = &handoffStore{items: make(map[string]handoffEntry), now: time.Now}

func (s *handoffStore) create(username, ip, token string) (string, error) {
	code, err := randHex(16)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if len(s.items) >= handoffMaxItems {
		// 只可能被已登录者灌满；清空比无限增长安全，用户重试即可。
		s.items = make(map[string]handoffEntry)
	}
	s.items[code] = handoffEntry{username: username, ip: ip, token: token, expiresAt: now.Add(handoffTTL)}
	return code, nil
}

// consume 兑换交接码。IP 不匹配时保留记录（避免他人误用导致合法用户失效），
// 原 Token 失效时则直接丢弃。
func (s *handoffStore) consume(code, ip string, tokenValid func(string) bool) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	e, ok := s.items[code]
	if !ok {
		return "", false
	}
	if e.ip != ip {
		return "", false
	}
	if tokenValid != nil && !tokenValid(e.token) {
		delete(s.items, code)
		return "", false
	}
	delete(s.items, code)
	return e.username, true
}

func (s *handoffStore) pruneLocked(now time.Time) {
	for code, e := range s.items {
		if now.After(e.expiresAt) {
			delete(s.items, code)
		}
	}
}

// sessionHandoffCreateHandler 为当前会话生成一次性交接码（需登录）。
func sessionHandoffCreateHandler(authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		username := authz.OperatorOf(token)
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		code, err := sessionHandoffs.create(username, auth.LoginIPOf(r.RemoteAddr), token)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "生成会话交接码失败"})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok":         true,
			"code":       code,
			"expires_in": int(handoffTTL / time.Second),
		})
	}
}

// sessionHandoffConsumeHandler 在新地址兑换交接码并颁发新 Token。
// 免登录（新 origin 还没有 Token），但必须通过 IP 白名单/限流。
func sessionHandoffConsumeHandler(authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guardGate(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if !decodeJSON(w, r, maxCredBody, &body) {
			return
		}
		code := strings.TrimSpace(body.Code)
		if code == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "缺少会话交接码"})
			return
		}
		ip := auth.LoginIPOf(r.RemoteAddr)
		username, ok := sessionHandoffs.consume(code, ip, authz.ValidateToken)
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "交接码无效或已过期，请重新登录"})
			return
		}
		newToken, err := randHex(32)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "生成会话失败"})
			return
		}
		authz.RegisterToken(newToken, username)
		logx.Infof("[auth] 会话交接成功（用户 %s，来自 %s）", username, ip)
		writeJSON(w, map[string]interface{}{"ok": true, "token": newToken})
	}
}
