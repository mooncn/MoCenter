// 本文件实现 per-agent 的一次性注册令牌，以及 agent_id → 证书公钥的 TOFU 绑定。
//
// ## 为什么需要它（对应评估项 C-1）
//
// 部署级共享注册密钥（<data>/agent.key）是"HMAC 证明"的唯一信任根，而注册报文里的
// agent_id 完全由申请方自选 —— 于是**任何一台被攻破的机器**都能申请一张
// CN = 受害者 agent_id 的合法客户端证书，从此被服务端当成那台主机。
// "一把密钥 = 全网身份"必须收敛为"一把令牌 = 一台主机、一次机会"：
//
//   - 令牌默认与 agent_id 绑定：拿 A 机的令牌换不到 B 机的证书；
//   - 令牌可以显式"不绑定"（签发时 agent_id 留空）：任何主机可用一次，身份由该机决定
//     （客户端未指定 agent_id 时回退为机器 UUID）。便利的代价是它本质为一次性 bearer
//     令牌 —— 谁先拿到谁就能用，控制台必须把这一点写清楚；
//   - 一次性：换取时即作废（用后即焚），泄露面随时间收敛；
//   - 带有效期：没在窗口内使用的令牌自动失效，不积累长期凭据；
//   - TOFU：同一 agent_id 第二次以**不同公钥**换取证书时，除非出示该主机的新令牌，
//     否则拒绝并审计告警 —— 这正是"共享密钥泄露后冒名"的最后一道闸。
//
// ## 为什么令牌明文落盘（与登录口令的存储方式不同）
//
// HMAC 证明要求服务端用令牌本身计算期望值（Proof(token, agent_id|nonce|ts)），
// 只存摘要就无法校验。这与既有的 <data>/agent.key 一致（同样明文、同样 0600）：
// 文件可读权限本身就是信任边界。相比共享密钥，令牌的差别在**作用域与寿命** ——
// 泄露一台只丢一台的身份，且用过即废。
package agentauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// EnrollStoreFileName 令牌与 TOFU 绑定的持久化文件名（数据目录下）。
	EnrollStoreFileName = "enroll.json"
	// TokenDefaultTTL 令牌默认有效期。
	TokenDefaultTTL = 24 * time.Hour
	// TokenMinTTL / TokenMaxTTL 允许的有效期区间，防止误配成"永不过期"或"秒级失效"。
	TokenMinTTL = time.Minute
	TokenMaxTTL = 30 * 24 * time.Hour
	// maxTokensPerAgent 单台主机同时可持有的未使用令牌上限。
	maxTokensPerAgent = 8
	// maxTokens 记录总量上限，防止创建接口被用来无限堆键。
	maxTokens = 2048
)

// 一次注册/换取所凭的凭证类型。
const (
	// CredToken per-agent 一次性注册令牌（推荐）
	CredToken = "token"
	// CredSharedKey 部署级共享注册密钥（旧方式，双轨过渡期保留）
	CredSharedKey = "shared-key"
	// CredAnon 服务端显式开启 -allow-unauthenticated-agents 时的无凭据接入。
	// 它由**运维开关**决定，不由"忘配密钥"产生 —— 见 Verifier.Verify 的注释（H-N1）。
	CredAnon = "anonymous"
)

// EnrollToken 一条注册令牌记录。
type EnrollToken struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Label     string `json:"label,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	UsedAt    int64  `json:"used_at,omitempty"`
	// Source 本次签发凭的是什么（token / shared-key），便于迁移期盘点。
	Source string `json:"source,omitempty"`

	// secret 令牌明文（HMAC 密钥）。刻意不设 json 标签：任何整体序列化
	// EnrollToken 的路径都带不出明文，只有 saveLocked 会显式写入 secrets 表。
	secret string
}

func (t *EnrollToken) expiredAt(now int64) bool { return now >= t.ExpiresAt }

// usable 当前时刻能否用它做校验（未使用且未过期）。
func (t *EnrollToken) usable(now int64) bool { return t.UsedAt == 0 && !t.expiredAt(now) }

// Public 返回可下发给控制台的视图（不含明文）。令牌只在创建那一刻回显一次。
func (t *EnrollToken) Public(now int64) map[string]interface{} {
	return map[string]interface{}{
		"id":         t.ID,
		"agent_id":   t.AgentID,
		"label":      t.Label,
		"created_at": t.CreatedAt,
		"expires_at": t.ExpiresAt,
		"used_at":    t.UsedAt,
		"source":     t.Source,
		"expired":    t.UsedAt == 0 && t.expiredAt(now),
		// Unbound 为 true 表示签发时没有绑定 agent_id：任何主机可用一次。
		"unbound": t.AgentID == "",
	}
}

// CertBinding 一个 agent_id 当前绑定的客户端证书公钥指纹（TOFU 记录）。
type CertBinding struct {
	AgentID   string `json:"agent_id"`
	SPKIHash  string `json:"spki_hash"`
	IssuedAt  int64  `json:"issued_at"`
	ReboundAt int64  `json:"rebound_at,omitempty"`
	// Rebinds 经令牌重新绑定（换机/重装）的次数，便于审计发现异常重签。
	Rebinds int `json:"rebinds,omitempty"`
}

// EnrollStore 注册令牌与证书绑定的存储。服务端有单实例锁，因此进程内单例即可；
// 每次变更即时落盘（0600，先写临时文件再 rename）。
type EnrollStore struct {
	mu       sync.Mutex
	path     string
	now      func() time.Time
	tokens   []*EnrollToken
	bindings map[string]*CertBinding
}

// persistedToken 落盘形态（与 EnrollToken 分离，防字段漂移）。
type persistedToken struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Label     string `json:"label,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	UsedAt    int64  `json:"used_at,omitempty"`
	Source    string `json:"source,omitempty"`
}

type persistedState struct {
	Tokens   []persistedToken        `json:"tokens"`
	Secrets  map[string]string       `json:"secrets"`
	Bindings map[string]*CertBinding `json:"bindings"`
}

// NewEnrollStore 创建存储并尽力加载既有内容。文件不存在 = 首次启动，正常。
func NewEnrollStore(path string) (*EnrollStore, error) {
	if path == "" {
		return nil, fmt.Errorf("令牌存储路径为空")
	}
	s := &EnrollStore{
		path:     path,
		now:      time.Now,
		bindings: make(map[string]*CertBinding),
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	for _, pt := range st.Tokens {
		if pt.ID == "" || st.Secrets[pt.ID] == "" {
			continue // 缺 id/secret 的记录无法校验也无法使用，丢弃；agent_id 允许为空（不绑定令牌）
		}
		s.tokens = append(s.tokens, &EnrollToken{
			ID: pt.ID, AgentID: pt.AgentID, Label: pt.Label,
			CreatedAt: pt.CreatedAt, ExpiresAt: pt.ExpiresAt,
			UsedAt: pt.UsedAt, Source: pt.Source,
			secret: st.Secrets[pt.ID],
		})
	}
	for id, bd := range st.Bindings {
		if bd != nil && bd.AgentID != "" && bd.SPKIHash != "" {
			s.bindings[id] = bd
		}
	}
	return s, nil
}

// saveLocked 落盘，调用方须持锁。
func (s *EnrollStore) saveLocked() error {
	st := persistedState{
		Secrets:  make(map[string]string, len(s.tokens)),
		Bindings: make(map[string]*CertBinding, len(s.bindings)),
	}
	for _, t := range s.tokens {
		st.Tokens = append(st.Tokens, persistedToken{
			ID: t.ID, AgentID: t.AgentID, Label: t.Label,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
			UsedAt: t.UsedAt, Source: t.Source,
		})
		st.Secrets[t.ID] = t.secret
	}
	for id, b := range s.bindings {
		st.Bindings[id] = b
	}
	buf, err := json.MarshalIndent(&st, "", "  ")
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

// CreateToken 生成一次性注册令牌。
// agentID 为空表示"不绑定"：任何主机都可用它换取一次证书，身份由客户端决定
// （客户端未指定 agent_id 时默认机器 UUID）。返回的明文只在这一次给出，
// 控制台与列表接口都不会再出现它。
func (s *EnrollStore) CreateToken(agentID, label string, ttl time.Duration) (string, *EnrollToken, error) {
	agentID = strings.TrimSpace(agentID)
	switch {
	case ttl <= 0:
		ttl = TokenDefaultTTL
	case ttl < TokenMinTTL:
		ttl = TokenMinTTL
	case ttl > TokenMaxTTL:
		ttl = TokenMaxTTL
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
	now := s.now().Unix()
	s.pruneLocked(now)
	pending := 0
	for _, t := range s.tokens {
		if t.AgentID == agentID && t.usable(now) {
			pending++
		}
	}
	// 上限按"未使用"计：用过的记录要留着做迁移盘点与审计，不能占额度。
	unused := 0
	for _, t := range s.tokens {
		if t.UsedAt == 0 {
			unused++
		}
	}
	// 未绑定令牌共用一个桶：它们不对应任何一台主机，按同一签发目标限制数量。
	if unused >= maxTokens || pending >= maxTokensPerAgent {
		return "", nil, fmt.Errorf("未使用令牌已达上限（同一签发目标 %d / 总量 %d），请先吊销", maxTokensPerAgent, maxTokens)
	}
	tok := &EnrollToken{
		ID: id, AgentID: agentID, Label: strings.TrimSpace(label),
		CreatedAt: now, ExpiresAt: now + int64(ttl/time.Second), secret: secret,
	}
	s.tokens = append(s.tokens, tok)
	if err := s.saveLocked(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		return "", nil, fmt.Errorf("令牌已生成但保存失败: %w", err)
	}
	return secret, tok, nil
}

// ListTokens 返回令牌视图（创建时间倒序），不含明文。
func (s *EnrollStore) ListTokens() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	out := make([]map[string]interface{}, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, t.Public(now))
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["created_at"].(int64)
		b, _ := out[j]["created_at"].(int64)
		return a > b
	})
	return out
}

// RevokeToken 按 id 吊销一条令牌，返回是否存在并被移除。
func (s *EnrollStore) RevokeToken(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID != id {
			continue
		}
		before := s.tokens
		s.tokens = append(append([]*EnrollToken{}, before[:i]...), before[i+1:]...)
		if err := s.saveLocked(); err != nil {
			s.tokens = before
			return false
		}
		return true
	}
	return false
}

// RevokeAgentTokens 移除某台主机的全部未使用令牌（"这台机不再允许换取"）。
// 已使用的记录保留在账上，作为"该机曾换取过"的证据。
func (s *EnrollStore) RevokeAgentTokens(agentID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]*EnrollToken, 0, len(s.tokens))
	n := 0
	for _, t := range s.tokens {
		if t.UsedAt == 0 && t.AgentID == agentID {
			n++
			continue
		}
		kept = append(kept, t)
	}
	if n == 0 {
		return 0
	}
	s.tokens = kept
	_ = s.saveLocked()
	return n
}

// matchConsume 找出与本次证明匹配的未使用令牌，并当场作废它。
//
// 绑定令牌只在 t.AgentID == agentID 时参与；不绑定令牌（t.AgentID == ""）对任何
// agent_id 都参与，证明值用**本次请求的 agent_id** 计算 —— Agent 侧本来就是这么算的，
// 因此客户端无需知道令牌是否绑定。一台主机最多 8 条候选，逐个算 HMAC 的代价可忽略；
// 比对用常数时间，避免把匹配位置泄露成时序信号。作废与落盘在持锁内一次完成 ——
// "一次性"必须是硬保证，即使写文件失败也只影响重启后的状态，不影响本次进程内的单次使用。
func (s *EnrollStore) matchConsume(agentID, nonce, proof string, ts int64) *EnrollToken {
	if agentID == "" || proof == "" {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(proof))
	now := s.now().Unix()
	for _, t := range s.tokens {
		if (t.AgentID != "" && t.AgentID != agentID) || !t.usable(now) {
			continue
		}
		got := Proof(t.secret, agentID, nonce, ts)
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			t.UsedAt = now
			t.Source = CredToken
			_ = s.saveLocked()
			return t
		}
	}
	return nil
}

// Bindings 返回当前的 TOFU 绑定（供控制台展示与审计）。
func (s *EnrollStore) Bindings() []CertBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CertBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// CheckOrBind 校验并更新 agent_id 的证书公钥绑定（TOFU）。
//
//   - 无历史记录：记下本次公钥（首次信任）；
//   - 公钥一致：同一台机器重新换取（证书到期轮换等），放行；
//   - 公钥变了：只有本次出示了**该主机的新令牌**才放行并更新绑定；否则视为
//     "拿泄露的共享密钥冒名换机"，拒绝。运维的恢复路径很明确：给该机生成一个新令牌。
func (s *EnrollStore) CheckOrBind(agentID, spkiHash, source string) error {
	if agentID == "" || spkiHash == "" {
		return fmt.Errorf("缺少 agent_id 或证书公钥指纹")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.bindings[agentID]
	if cur == nil {
		s.bindings[agentID] = &CertBinding{AgentID: agentID, SPKIHash: spkiHash, IssuedAt: s.now().Unix()}
		return s.saveLocked()
	}
	if subtle.ConstantTimeCompare([]byte(cur.SPKIHash), []byte(spkiHash)) == 1 {
		return nil
	}
	if source != CredToken {
		old := cur.SPKIHash
		if len(old) > 16 {
			old = old[:16]
		}
		return fmt.Errorf("该 agent_id 已绑定另一张证书的公钥（首次绑定 %s，指纹 %s…）："+
			"仅凭共享注册密钥不允许换发。若确为本机重装/换盘，请为该主机生成一次性注册令牌后重试",
			time.Unix(cur.IssuedAt, 0).Format("2006-01-02 15:04:05"), old)
	}
	cur.SPKIHash = spkiHash
	cur.ReboundAt = s.now().Unix()
	cur.Rebinds++
	return s.saveLocked()
}

// ResetBinding 清除某台主机的 TOFU 绑定（运维显式授权重签）。
func (s *EnrollStore) ResetBinding(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bindings[agentID]; !ok {
		return false
	}
	delete(s.bindings, agentID)
	_ = s.saveLocked()
	return true
}

// pruneLocked 丢弃"过期且从未使用"的记录（用过的留着做审计）。调用方持锁。
func (s *EnrollStore) pruneLocked(now int64) {
	kept := s.tokens[:0]
	for _, t := range s.tokens {
		if t.UsedAt == 0 && t.expiredAt(now) {
			continue
		}
		kept = append(kept, t)
	}
	s.tokens = kept
}

// PendingTokens 返回当前可用（未使用且未过期）的令牌数，用于启动时的迁移提示。
func (s *EnrollStore) PendingTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	n := 0
	for _, t := range s.tokens {
		if t.usable(now) {
			n++
		}
	}
	return n
}

// SpkiHashOfCert 计算证书公钥（SPKI）的 SHA-256 指纹，用于 TOFU 比对。
// PEM 解析失败返回空串（调用方据此跳过绑定检查，不影响签发本身）。
func SpkiHashOfPEM(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return ""
	}
	return SpkiHashOfX509(block.Bytes)
}

// SpkiHashOfX509 计算 DER 证书公钥的 SHA-256 指纹。
func SpkiHashOfX509(der []byte) string {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// randomHex 生成 n 字节随机值的 hex 串。
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
