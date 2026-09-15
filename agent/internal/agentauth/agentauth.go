// Package agentauth 计算发往服务端的注册证明。
//
// 与 server/internal/agentauth 保持同一算法（两个模块相互独立，刻意不共享代码，
// 与 protocol 定义的做法一致）：
//
//	proof = HMAC-SHA256(key, agentID + "|" + nonce + "|" + unixTs)
//
// 密钥不上线传输，因此即使链路是明文，嗅探者也拿不到密钥、无法伪造证明。
package agentauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Proof 计算注册证明；key 为空时返回空串（表示不携带凭证）。
func Proof(key, agentID, nonce string, ts int64) string {
	key = strings.TrimSpace(key)
	if key == "" || agentID == "" || nonce == "" || ts <= 0 {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "%s|%s|%d", agentID, nonce, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// NewNonce 生成随机 nonce。
func NewNonce() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
