package agentauth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
)

// certFingerprint 返回证书公钥指纹（SHA-256 前 16 位 hex）。
//
// 刻意用 **公钥（SPKI）** 而不是整证书或 CT 日志：这样服务端证书换签到新 IP/域名、
// 换有效期、重新签发，只要 CA 不变，指纹就不变，Agent 端的 pin 无需更新。
func certFingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])[:16]
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

// MatchFingerprint 比较两个指纹（忽略分隔符与大小写）。任一为空视为不匹配。
func MatchFingerprint(a, b string) bool {
	na, nb := normFingerprint(a), normFingerprint(b)
	return na != "" && na == nb
}

// DisplayFingerprint 把指纹格式化成 4 位一组，便于人工比对。
func DisplayFingerprint(fp string) string {
	fp = normFingerprint(fp)
	if len(fp) < 8 {
		return fp
	}
	parts := make([]string, 0, 4)
	for i := 0; i+4 <= len(fp) && len(parts) < 4; i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	return strings.Join(parts, " ")
}

// FingerprintError 生成可读的指纹不一致错误。
func FingerprintError(expected, got string) error {
	return fmt.Errorf("服务端身份指纹不一致：期望 %s，实际 %s",
		DisplayFingerprint(expected), DisplayFingerprint(got))
}
