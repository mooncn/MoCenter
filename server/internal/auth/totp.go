// Package auth 提供 API 鉴权、登录、TOTP 多因子校验。
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

// validateTOTP 校验 RFC 6238 的一次性验证码（HMAC-SHA1，6 位，30 秒步长）。
// secret 为 base32 编码（不带填充的常见形式，兼容 Google Authenticator）。
// window 表示允许向前/向后偏移的步长数量（容忍时钟偏差）。
func validateTOTP(secret, code string, window int) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false
	}
	counter := time.Now().Unix() / 30
	for w := -window; w <= window; w++ {
		if totpAt(key, counter+int64(w)) == code {
			return true
		}
	}
	return false
}

// totpAt 计算某一时间步长的 HOTP 值（RFC 4226）并格式化为 6 位数字。
func totpAt(key []byte, counter int64) string {
	buf := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		buf[i] = byte(counter & 0xff)
		counter >>= 8
	}
	h := hmac.New(sha1.New, key)
	_, _ = h.Write(buf)
	sum := h.Sum(nil)
	offset := int(sum[len(sum)-1] & 0x0f)
	bin := (int(sum[offset]&0x7f)<<24 |
		int(sum[offset+1])<<16 |
		int(sum[offset+2])<<8 |
		int(sum[offset+3]))
	return fmt.Sprintf("%06d", bin%1000000)
}