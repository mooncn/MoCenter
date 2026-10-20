package agentauth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
)

// legacyFingerprintLen 历史版本只保存/展示 SHA-256 的前 16 位（64 bit）。
// 新指纹一律输出完整 64 位；比较时仍兼容这段短指纹，避免老配置一夜失效。
const legacyFingerprintLen = 16

// certFingerprint 返回证书公钥指纹（完整 SHA-256，64 位 hex）。
//
// 刻意用 **公钥（SPKI）** 而不是整证书或 CT 日志：这样服务端证书换签到新 IP/域名、
// 换有效期、重新签发，只要 CA 不变，指纹就不变，Agent 端的 pin 无需更新。
//
// 此前只保留前 16 位 hex（64 bit）：作为长期 pin 熵不足，且人工比对时容易只核对
// 前缀。完整摘要不增加任何使用成本，却把碰撞/暴力匹配的难度拉回 256 bit。
func certFingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// normFingerprint 归一化人工填写的指纹（允许冒号/空格分隔、大小写混写）。
func normFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fingerprintMatches 比较预期指纹与实际指纹（忽略分隔符与大小写）。
//
// 完整指纹必须完全一致；历史 16 位短指纹按前缀匹配（当时服务端只输出前 16 位）。
func fingerprintMatches(want, got string) bool {
	w, g := normFingerprint(want), normFingerprint(got)
	if w == "" || g == "" {
		return false
	}
	if w == g {
		return true
	}
	return len(w) == legacyFingerprintLen && len(g) > legacyFingerprintLen && strings.HasPrefix(g, w)
}

// MatchFingerprint 比较两个指纹（忽略分隔符与大小写）。任一为空视为不匹配。
func MatchFingerprint(a, b string) bool { return fingerprintMatches(a, b) }

// DisplayFingerprint 把指纹格式化成 4 位一组，便于人工比对。
func DisplayFingerprint(fp string) string {
	fp = normFingerprint(fp)
	if len(fp) < 8 {
		return fp
	}
	parts := make([]string, 0, (len(fp)+3)/4)
	for i := 0; i+4 <= len(fp); i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	if rem := len(fp) % 4; rem > 0 {
		parts = append(parts, fp[len(fp)-rem:])
	}
	return strings.Join(parts, " ")
}

// FingerprintError 生成可读的指纹不一致错误。
func FingerprintError(expected, got string) error {
	return fmt.Errorf("服务端身份指纹不一致：期望 %s，实际 %s",
		DisplayFingerprint(expected), DisplayFingerprint(got))
}
