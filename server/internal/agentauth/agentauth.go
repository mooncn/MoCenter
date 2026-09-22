// Package agentauth 实现 Agent 长连接的应用层注册鉴权。
//
// 背景：:9000 通道此前不做任何应用层校验 —— 能连通该端口的主机就能注册为 Agent，
// 并且（旧实现）同名 AgentID 会顶掉真实连接，等于任何人只要知道一个 AgentID 就能
// 接管该主机的控制通道（看到下发命令、接收推送文件与 VNC 隧道数据）。
//
// 方案：注册凭据 + HMAC 挑战响应（凭据本身不上线传输）。
//
//	proof = HMAC-SHA256(key, agentID + "|" + nonce + "|" + unixTs)
//
// 服务端校验 proof、时间戳新鲜度（±60s）与 nonce 未被重放。
// 这样：
//   - 不知道密钥的一方无法伪造 proof（不像「把密钥发上去」那样可被嗅探复用）；
//   - 重放整条注册报文也会被 nonce 缓存与时间窗拦住；
//   - 与传输层是否加密无关，明文链路上依然成立。
//
// 这里的 key 有两种（见 Verifier）：**per-agent 一次性注册令牌**（tokens.go，推荐 ——
// 一把令牌只对应一台主机、只用一次），或**部署级共享注册密钥**（agent.key，旧方式 ——
// 一把密钥可注册任意主机，泄露一台即全网身份泄露，仅作双轨过渡期兼容保留）。
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
//
// 两条凭据路径并存（双轨过渡）：
//
//	a) per-agent 一次性令牌（tokens，推荐）：证明某台主机的这一次注册动作，用后即废；
//	b) 部署级共享注册密钥（key，旧方式）：一把密钥可注册任意 agent_id，
//	   泄露一台即等于全网身份泄露，保留一个版本用于迁移，可显式关闭。
//
// 校验顺序是先令牌后共享密钥：新装 Agent 走令牌就能通过，不会被"共享密钥还开着"
// 掩盖成旧路径，运维因此能在日志里看清还有多少主机停留在旧方式上。
type Verifier struct {
	key    string
	legacy bool // 是否仍接受共享密钥（关闭后只认令牌）

	tokens *EnrollStore

	mu     sync.Mutex
	nonces map[string]time.Time
	now    func() time.Time
}

// NewVerifier 创建校验器。key 为空表示没有共享密钥这条路径（调用方应打印警告）。
func NewVerifier(key string) *Verifier {
	key = strings.TrimSpace(key)
	return &Verifier{
		key:    key,
		legacy: key != "",
		nonces: make(map[string]time.Time),
		now:    time.Now,
	}
}

// SetEnrollStore 接入 per-agent 一次性令牌存储。
func (v *Verifier) SetEnrollStore(s *EnrollStore) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tokens = s
}

// DisableSharedKey 关闭"部署级共享注册密钥"这条路径，只认一次性令牌。
//
// 这是双轨过渡的终点开关：现网 Agent 全部换成令牌（或已持证书）后再关，
// 否则未迁移的主机将无法重新注册。
func (v *Verifier) DisableSharedKey() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.legacy = false
}

// SharedKeyEnabled 共享密钥这条路径是否仍然可用。
func (v *Verifier) SharedKeyEnabled() bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.legacy && v.key != ""
}

// SharedKey 返回当前共享注册密钥明文（路径关闭时为空串）。
//
// 它是**明文秘密**，只允许用在"运维显式动作"的出口 —— 目前唯一的使用者是一键部署，
// 它要把密钥写进目标机的 Agent 配置。控制台读接口一律只能给 KeyFingerprint，
// 给明文就等于让任何一次已登录页面的加载顺带把根凭证带走（M-1）。
func (v *Verifier) SharedKey() string {
	if v == nil {
		return ""
	}
	key, ok := v.sharedKeySnapshot()
	if !ok {
		return ""
	}
	return key
}

// RotateSharedKey 生成一把新的部署级共享注册密钥，写入 path（0600）并立即生效。
//
// 为什么需要它：控制台不再回显当前密钥明文（M-1），"要给一台遗留机器配密钥"于是
// 只剩两条路 —— 登服务端读文件，或者换一把新的。后者做成一个显式动作，顺带让
// 疑似泄露的旧密钥对新注册立即失效。
//
// 它**不会**让已签发的客户端证书作废（那是 CA 的事，须轮换 ca.key），
// 也不影响已用一次性令牌接入的主机 —— 它们早就不依赖这把密钥了。
func (v *Verifier) RotateSharedKey(path string) (string, error) {
	if v == nil {
		return "", fmt.Errorf("校验器未初始化")
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("密钥路径为空")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.legacy {
		// 这条路径已被 -disable-shared-agent-key 显式关掉：换新密钥不会生效，
		// 只会让文件与运行时状态不一致，所以拒绝而不是"顺手再打开"。
		return "", fmt.Errorf("共享注册密钥已关闭，轮换无意义")
	}
	buf := make([]byte, KeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成密钥失败: %w", err)
	}
	key := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	// 先写临时文件再改名：留下半把密钥比没有密钥更难排查
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	v.key = key
	return key, nil
}

// Tokens 返回已接入的令牌存储（未接入为 nil）。
func (v *Verifier) Tokens() *EnrollStore {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.tokens
}

// Enabled 是否启用了凭证校验（任一条路径可用即为启用）。
func (v *Verifier) Enabled() bool {
	if v == nil {
		return false
	}
	return v.SharedKeyEnabled() || v.Tokens() != nil
}

// KeyFingerprint 返回密钥指纹（前 8 位 SHA-256），用于日志比对而不泄露密钥。
func (v *Verifier) KeyFingerprint() string {
	if v == nil {
		return ""
	}
	key, _ := v.sharedKeySnapshot()
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

// sharedKeySnapshot 在锁内取回当前共享密钥及其可用状态。
//
// RotateSharedKey 会改写 v.key，因此**任何**读它的地方都必须走这里 ——
// 否则锁外读到的一半旧一半新的密钥，会把校验变成随机失败。
func (v *Verifier) sharedKeySnapshot() (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.key, v.legacy && v.key != ""
}

// Verify 校验一次注册，返回本次所凭的凭证类型（CredToken / CredSharedKey）。
//
// 未通过时返回错误；错误串刻意保持模糊（不区分"令牌不存在"与"证明不对"），
// 免得把这条通道变成探测某个 agent_id 是否配过令牌的 oracle。
func (v *Verifier) Verify(agentID, nonce, proof string, ts int64) (string, error) {
	// 未启用任何凭据路径时**必须报错**，不能返回 ("", nil)。
	//
	// 这里曾经是 `return "", nil` —— 把"没配凭据"当成"校验通过"，调用方
	// enrollAgentCert 在 -allow-unauthenticated-agents 下又不再自己判 Enabled，
	// 两处语义一叠加就成了"无凭据者可自选 agent_id 换到合法 CA 证书"（H-N1）。
	// 静默通过这种语义没有任何调用方需要：要放行匿名注册的地方应当**显式**放行
	// （见 main.go 的 CredAnon），而不是靠"忘了配密钥"这条路径顺出来。
	if !v.Enabled() {
		return "", fmt.Errorf("服务端未配置注册凭据（一次性注册令牌或共享注册密钥）")
	}
	if agentID == "" {
		return "", fmt.Errorf("缺少 agent_id")
	}
	if nonce == "" || proof == "" || ts <= 0 {
		return "", fmt.Errorf("缺少注册凭证（agent-key / 注册令牌未配置？）")
	}
	now := v.now()
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return "", fmt.Errorf("注册报文时间戳偏差过大（%v，请检查两端时钟）", skew.Truncate(time.Second))
	}

	if store := v.Tokens(); store != nil {
		if tok := store.matchConsume(agentID, nonce, proof, ts); tok != nil {
			if err := v.claimNonce(nonce, now); err != nil {
				// 令牌已在 matchConsume 里作废：重放者最多把这台机器的机会用掉一次，
				// 运维重新生成即可，不存在"重复注册成功"。
				return "", err
			}
			return CredToken, nil
		}
	}
	if key, ok := v.sharedKeySnapshot(); ok {
		want := Proof(key, agentID, nonce, ts)
		if want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(proof))) == 1 {
			if err := v.claimNonce(nonce, now); err != nil {
				return "", err
			}
			return CredSharedKey, nil
		}
	}
	return "", fmt.Errorf("注册凭证校验失败")
}

// claimNonce 记录本次使用的 nonce；若已被使用则判定为重放。
func (v *Verifier) claimNonce(nonce string, now time.Time) error {
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
