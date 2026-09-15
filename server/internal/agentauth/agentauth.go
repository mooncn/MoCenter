// Package agentauth 实现 Agent 长连接的应用层注册鉴权。
//
// 背景：:9000 通道此前不做任何应用层校验 —— 能连通该端口的主机就能注册为 Agent，
// 并且（旧实现）同名 AgentID 会顶掉真实连接，等于任何人只要知道一个 AgentID 就能
// 接管该主机的控制通道（看到下发命令、接收推送文件与 VNC 隧道数据）。
//
// 方案：预共享密钥 + HMAC 挑战响应（密钥本身不上线传输）。
//
//	proof = HMAC-SHA256(key, agentID + "|" + nonce + "|" + unixTs)
//
// 服务端校验 proof、时间戳新鲜度（±60s）与 nonce 未被重放。
// 这样：
//   - 不知道密钥的一方无法伪造 proof（不像「把密钥发上去」那样可被嗅探复用）；
//   - 重放整条注册报文也会被 nonce 缓存与时间窗拦住；
//   - 与传输层是否加密无关，明文链路上依然成立。
package agentauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// KeyFileName 密钥在数据目录下的文件名。
	KeyFileName = "agent.key"
	// KeyBytes 密钥长度（字节）。
	KeyBytes = 32
	// MaxClockSkew 允许的时间偏差。
	MaxClockSkew = 60 * time.Second
	// nonceTTL nonce 去重窗口，需 >= 2*MaxClockSkew。
	nonceTTL = 5 * time.Minute
	// nonceCacheLimit nonce 缓存上限，防御伪造 nonce 造成的内存放大。
	nonceCacheLimit = 20000
)

// Proof 计算注册证明。agentID/nonce 为空或 ts<=0 时返回空串（调用方据此拒绝）。
func Proof(key, agentID, nonce string, ts int64) string {
	key = strings.TrimSpace(key)
	if key == "" || agentID == "" || nonce == "" || ts <= 0 {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "%s|%s|%d", agentID, nonce, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// NewNonce 生成一个随机 nonce（32 字节 hex）。
func NewNonce() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 熵源故障：退化为时间熵（仍不可预测到可被利用的程度，且服务端有重放缓存）
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// LoadOrCreateKey 从文件读取注册密钥；不存在则生成并写入（0600）。
// created 为 true 表示本次新建，调用方可据此提示运维。
func LoadOrCreateKey(path string) (key string, created bool, err error) {
	if path == "" {
		return "", false, fmt.Errorf("密钥路径为空")
	}
	if b, rerr := os.ReadFile(path); rerr == nil {
		k := strings.TrimSpace(string(b))
		if k != "" {
			return k, false, nil
		}
	} else if !os.IsNotExist(rerr) {
		return "", false, rerr
	}
	buf := make([]byte, KeyBytes)
	if _, rerr := rand.Read(buf); rerr != nil {
		return "", false, fmt.Errorf("生成密钥失败: %w", rerr)
	}
	key = hex.EncodeToString(buf)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", false, err
	}
	if err = os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// Verifier 校验 Agent 注册证明（含时间窗与重放保护）。
type Verifier struct {
	key string

	mu     sync.Mutex
	nonces map[string]time.Time
	now    func() time.Time
}

// NewVerifier 创建校验器。key 为空表示不校验（调用方应打印警告）。
func NewVerifier(key string) *Verifier {
	return &Verifier{
		key:    strings.TrimSpace(key),
		nonces: make(map[string]time.Time),
		now:    time.Now,
	}
}

// Enabled 是否启用了校验。
func (v *Verifier) Enabled() bool { return v != nil && v.key != "" }

// KeyFingerprint 返回密钥指纹（前 8 位 SHA-256），用于日志比对而不泄露密钥。
func (v *Verifier) KeyFingerprint() string {
	if v == nil || v.key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v.key))
	return hex.EncodeToString(sum[:])[:8]
}

// Verify 校验一次注册。返回 nil 表示通过。
func (v *Verifier) Verify(agentID, nonce, proof string, ts int64) error {
	if !v.Enabled() {
		return nil
	}
	if agentID == "" {
		return fmt.Errorf("缺少 agent_id")
	}
	if nonce == "" || proof == "" || ts <= 0 {
		return fmt.Errorf("缺少注册凭证（agent-key 未配置？）")
	}
	now := v.now()
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return fmt.Errorf("注册报文时间戳偏差过大（%v，请检查两端时钟）", skew.Truncate(time.Second))
	}
	want := Proof(v.key, agentID, nonce, ts)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(proof))) != 1 {
		return fmt.Errorf("注册凭证校验失败")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked(now)
	if _, dup := v.nonces[nonce]; dup {
		return fmt.Errorf("注册报文被重放（nonce 已使用）")
	}
	v.nonces[nonce] = now.Add(nonceTTL)
	return nil
}

// pruneLocked 清理过期 nonce。
func (v *Verifier) pruneLocked(now time.Time) {
	for n, exp := range v.nonces {
		if now.After(exp) {
			delete(v.nonces, n)
		}
	}
	if len(v.nonces) > nonceCacheLimit {
		v.nonces = make(map[string]time.Time)
	}
}
