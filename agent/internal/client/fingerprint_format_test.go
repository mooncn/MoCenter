package client

import (
	"strings"
	"testing"
)

// 指纹比较：完整 64 位必须全等；历史 16 位短指纹按前缀兼容，更短的值一律不认。
func TestFingerprintMatchesFullAndLegacy(t *testing.T) {
	full := strings.Repeat("ab", 32)
	if len(full) != 64 {
		t.Fatalf("测试前置：完整指纹应为 64 位，实得 %d", len(full))
	}
	if !fingerprintMatches(strings.ToUpper(full), full) {
		t.Fatal("大小写不同应视为同一指纹")
	}
	if !fingerprintMatches(full[:legacyFingerprintLen], full) {
		t.Fatal("历史 16 位短指纹应按前缀匹配")
	}
	if fingerprintMatches(full[:8], full) {
		t.Fatal("8 位短指纹熵不足，不应再被接受")
	}
	if fingerprintMatches(full[:63], full) {
		t.Fatal("长度不符的指纹不应匹配")
	}
	if fingerprintMatches("", full) || fingerprintMatches(full, "") {
		t.Fatal("空指纹不应匹配")
	}
}

func TestValidateServerFingerprint(t *testing.T) {
	full := strings.Repeat("ab", 32)
	ok := []string{
		"",
		full,
		full[:legacyFingerprintLen],
		strings.ToUpper(full),
		full[:8] + ":" + full[8:16] + " " + full[16:],
	}
	for _, fp := range ok {
		if err := ValidateServerFingerprint(fp); err != nil {
			t.Fatalf("合法指纹 %q 被拒绝: %v", fp, err)
		}
	}
	bad := []string{
		full[:8],
		full[:32],
		"zzzzzzzzzzzzzzzz",
		"not-a-fingerprint",
	}
	for _, fp := range bad {
		if err := ValidateServerFingerprint(fp); err == nil {
			t.Fatalf("非法指纹 %q 应被拒绝", fp)
		}
	}
}
