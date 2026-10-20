// Package share 实现"邀请远程管理"的能力链接：把一台主机的部分远程能力
// （终端 / 桌面 / 端口转发）在一段有限时间内授权给未登录的访客。
//
// 设计取舍：
//   - 系统本身是单用户、没有 RBAC，因此分享不是"给某个账号授权"，而是一枚
//     有作用域、有寿命、可撤销的 bearer 链接；
//   - 作用域必须同时钉住**主机**与**功能**：只分享桌面不应顺带得到 shell，
//     分享 A 机的链接也不应能操作 B 机；
//   - 链接明文只在创建响应里出现一次；落盘只保存 SHA-256 摘要，数据目录泄露
//     不等于历史分享链接全部可用；
//   - 会话键（用于把终端输出定向回访客的 WebSocket）是 token 的不可逆派生值，
//     一并持久化，撤销时才能主动断开已建立的连接。
package share

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/ws"
)

const (
	// StoreFileName 分享记录在数据目录下的文件名。
	StoreFileName = "shares.json"

	// FeatureTerminal 远程终端（交互 shell）。
	FeatureTerminal = "terminal"
	// FeatureVNC 远程桌面（VNC 隧道）。
	FeatureVNC = "vnc"
	// FeaturePortmap 端口转发（该主机的映射规则）。
	FeaturePortmap = "portmap"

	// DefaultTTL 未指定时的分享有效期。
	DefaultTTL = 2 * time.Hour
	// MinTTL / MaxTTL 允许的有效期区间。
	MinTTL = 5 * time.Minute
	MaxTTL = 7 * 24 * time.Hour

	defaultVNCPort = 5900
	// maxShares 活跃分享总量上限，防止创建接口被用来无限堆记录。
	maxShares = 512
	// retainAfterEnd 过期/撤销记录的保留时间：留一段时间便于解释"链接为什么失效"，
	// 之后自动清掉，避免文件无限增长。
	retainAfterEnd = 7 * 24 * time.Hour
	// touchInterval LastUsedAt 的落盘节流：访客每次请求都写文件没有意义。
	touchInterval = 60
)

// Share 一条分享记录。TokenHash 是链接 token 的 SHA-256，不是明文。
type Share struct {
	ID        string   `json:"id"`
	AgentID   string   `json:"agent_id"`
	Label     string   `json:"label,omitempty"`
	Features  []string `json:"features"`
	VNCPort   int      `json:"vnc_port,omitempty"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt int64    `json:"expires_at"`
	RevokedAt int64    `json:"revoked_at,omitempty"`
	// LastUsedAt 最近一次有效访问时间，供控制台判断链接是否真的被用过。
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	TokenHash  string `json:"token_hash"`
	// SessionKey 由 token 派生（见 ws.SessionKey），撤销时用它断开在线会话。
	SessionKey string `json:"session_key"`
}

// View 控制台可展示的分享信息（绝不含 token 或可重放的凭据）。
type View struct {
	ID         string   `json:"id"`
	AgentID    string   `json:"agent_id"`
	Label      string   `json:"label,omitempty"`
	Features   []string `json:"features"`
	VNCPort    int      `json:"vnc_port,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	ExpiresAt  int64    `json:"expires_at"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`
	Expired    bool     `json:"expired"`
	Revoked    bool     `json:"revoked"`
	Active     bool     `json:"active"`
}

func (s *Share) active(now int64) bool {
	return s.RevokedAt == 0 && now < s.ExpiresAt
}

func (s *Share) view(now int64) View {
	return View{
		ID:         s.ID,
		AgentID:    s.AgentID,
		Label:      s.Label,
		Features:   append([]string{}, s.Features...),
		VNCPort:    s.VNCPort,
		CreatedAt:  s.CreatedAt,
		ExpiresAt:  s.ExpiresAt,
		LastUsedAt: s.LastUsedAt,
		Expired:    s.RevokedAt == 0 && now >= s.ExpiresAt,
		Revoked:    s.RevokedAt != 0,
		Active:     s.active(now),
	}
}

// Store 分享记录存储（进程内单例 + JSON 落盘，0600）。
type Store struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	shares []*Share
}

// NewStore 创建存储并尽力加载既有记录。文件不存在 = 首次启动。
func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("分享存储路径为空")
	}
	s := &Store{path: path, now: time.Now}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var rows []*Share
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	for _, sh := range rows {
		if sh == nil || sh.ID == "" || sh.AgentID == "" || sh.TokenHash == "" || sh.SessionKey == "" || sh.ExpiresAt == 0 {
			continue
		}
		if _, err := NormalizeFeatures(sh.Features); err != nil {
			continue
		}
		if sh.VNCPort <= 0 || sh.VNCPort > 65535 {
			sh.VNCPort = defaultVNCPort
		}
		s.shares = append(s.shares, sh)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	buf, err := json.MarshalIndent(s.shares, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create 创建一条分享，返回链接 token 明文（只此一次）与记录副本。
func (s *Store) Create(agentID, label string, features []string, vncPort int, ttl time.Duration) (string, *Share, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return "", nil, fmt.Errorf("未指定目标主机")
	}
	normalized, err := NormalizeFeatures(features)
	if err != nil {
		return "", nil, err
	}
	hasVNC := false
	for _, f := range normalized {
		if f == FeatureVNC {
			hasVNC = true
		}
	}
	if hasVNC {
		if vncPort <= 0 {
			vncPort = defaultVNCPort
		}
		if vncPort < 1 || vncPort > 65535 {
			return "", nil, fmt.Errorf("VNC 端口应在 1-65535 之间")
		}
	} else {
		vncPort = 0
	}
	switch {
	case ttl <= 0:
		ttl = DefaultTTL
	case ttl < MinTTL:
		ttl = MinTTL
	case ttl > MaxTTL:
		ttl = MaxTTL
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", nil, err
	}
	id, err := randomHex(6)
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	nowUnix := now.Unix()
	s.pruneLocked(nowUnix)
	active := 0
	for _, sh := range s.shares {
		if sh.active(nowUnix) {
			active++
		}
	}
	if active >= maxShares {
		return "", nil, fmt.Errorf("活跃分享已达上限 %d，请先撤销不再使用的链接", maxShares)
	}
	sh := &Share{
		ID:         id,
		AgentID:    agentID,
		Label:      strings.TrimSpace(label),
		Features:   normalized,
		VNCPort:    vncPort,
		CreatedAt:  nowUnix,
		ExpiresAt:  nowUnix + int64(ttl/time.Second),
		TokenHash:  hashToken(secret),
		SessionKey: ws.SessionKey(secret),
	}
	s.shares = append(s.shares, sh)
	if err := s.saveLocked(); err != nil {
		s.shares = s.shares[:len(s.shares)-1]
		return "", nil, fmt.Errorf("分享已生成但保存失败: %w", err)
	}
	cp := *sh
	cp.Features = append([]string{}, sh.Features...)
	return secret, &cp, nil
}

// Validate 校验链接并返回记录副本。agentID / feature 非空时必须匹配。
func (s *Store) Validate(token, agentID, feature string) (*Share, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("缺少分享凭据")
	}
	want := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	for _, sh := range s.shares {
		if subtle.ConstantTimeCompare([]byte(sh.TokenHash), []byte(want)) != 1 {
			continue
		}
		if sh.RevokedAt != 0 {
			return nil, fmt.Errorf("分享链接已被撤销")
		}
		if now >= sh.ExpiresAt {
			return nil, fmt.Errorf("分享链接已过期")
		}
		if agentID != "" && sh.AgentID != agentID {
			return nil, fmt.Errorf("分享链接不适用于该主机")
		}
		if feature != "" && !hasFeature(sh.Features, feature) {
			return nil, fmt.Errorf("分享链接未授予该功能")
		}
		if now-sh.LastUsedAt >= touchInterval {
			sh.LastUsedAt = now
			_ = s.saveLocked()
		}
		cp := *sh
		cp.Features = append([]string{}, sh.Features...)
		return &cp, nil
	}
	return nil, fmt.Errorf("分享链接无效")
}

// ListActive 返回某台主机当前有效的分享；agentID 为空时返回全部。
func (s *Store) ListActive(agentID string) []View {
	agentID = strings.TrimSpace(agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	s.pruneLocked(now)
	out := make([]View, 0, len(s.shares))
	for _, sh := range s.shares {
		if !sh.active(now) {
			continue
		}
		if agentID != "" && sh.AgentID != agentID {
			continue
		}
		out = append(out, sh.view(now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Get 返回指定 id 的记录副本（吊销/审计时用于取出主机与会话键）。
func (s *Store) Get(id string) *Share {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sh := range s.shares {
		if sh.ID == id {
			cp := *sh
			cp.Features = append([]string{}, sh.Features...)
			return &cp
		}
	}
	return nil
}

// Active 判断指定分享当前是否仍有效（未撤销且未过期）。
// 已建立的访客连接在每条上行消息上复查它，避免"链接过期了，终端还能继续输入"。
func (s *Store) Active(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	for _, sh := range s.shares {
		if sh.ID == id {
			return sh.active(now)
		}
	}
	return false
}

// Revoke 撤销一条分享，返回是否存在。
func (s *Store) Revoke(id string) *Share {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sh := range s.shares {
		if sh.ID != id {
			continue
		}
		cp := *sh
		cp.Features = append([]string{}, sh.Features...)
		if sh.RevokedAt == 0 {
			sh.RevokedAt = s.now().Unix()
			_ = s.saveLocked()
		}
		return &cp
	}
	return nil
}

// RevokeAgent 撤销某台主机的全部有效分享，返回被撤销记录的会话键。
// 删除主机时调用：主机都不在册了，指向它的邀请链接不应继续可用。
func (s *Store) RevokeAgent(agentID string) (sessionKeys []string, count int) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	changed := false
	for _, sh := range s.shares {
		if !sh.active(now) || sh.AgentID != agentID {
			continue
		}
		sh.RevokedAt = now
		sessionKeys = append(sessionKeys, sh.SessionKey)
		count++
		changed = true
	}
	if changed {
		_ = s.saveLocked()
	}
	return sessionKeys, count
}

// pruneLocked 清掉结束超过保留期的记录。调用方持锁。
func (s *Store) pruneLocked(now int64) {
	retain := int64(retainAfterEnd / time.Second)
	kept := s.shares[:0]
	for _, sh := range s.shares {
		if sh.RevokedAt != 0 {
			if now-sh.RevokedAt > retain {
				continue
			}
		} else if now-sh.ExpiresAt > retain {
			continue
		}
		kept = append(kept, sh)
	}
	s.shares = kept
}

// NormalizeFeatures 校验并去重功能列表，同时固定输出顺序。
func NormalizeFeatures(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, raw := range in {
		f := strings.ToLower(strings.TrimSpace(raw))
		switch f {
		case FeatureTerminal, FeatureVNC, FeaturePortmap:
			seen[f] = true
		case "":
		default:
			return nil, fmt.Errorf("未知的分享功能: %s", raw)
		}
	}
	out := make([]string, 0, 3)
	for _, f := range []string{FeatureTerminal, FeatureVNC, FeaturePortmap} {
		if seen[f] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少选择一个分享功能")
	}
	return out, nil
}

// HasFeature 判断一条记录是否授予了指定功能。
func HasFeature(sh *Share, feature string) bool {
	return sh != nil && hasFeature(sh.Features, feature)
}

func hasFeature(list []string, feature string) bool {
	for _, f := range list {
		if f == feature {
			return true
		}
	}
	return false
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
