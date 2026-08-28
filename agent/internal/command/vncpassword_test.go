package command

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 校验 VNC 口令混淆与 TigerVNC 权威测试向量一致：
// obfuscate("Secure!") == D7A514D8C556AADE
func TestVncObfuscateKnownVector(t *testing.T) {
	got := vncObfuscatePassword("Secure!")
	want := "D7A514D8C556AADE"
	if strings.ToUpper(hex.EncodeToString(got)) != want {
		t.Fatalf("obfuscate(Secure!) = %X, want %s", got, want)
	}
}

func TestGenerateRandomPassword(t *testing.T) {
	a := generateRandomPassword()
	b := generateRandomPassword()
	if len(a) != 8 || a == b {
		t.Fatalf("unexpected random passwords: %q %q", a, b)
	}
}

// 超长口令必须截断为前 8 字节再混淆（VNC 协议上限）。
func TestVncObfuscateTruncates(t *testing.T) {
	short := vncObfuscatePassword("12345678")
	long := vncObfuscatePassword("12345678xyz")
	if string(short) != string(long) {
		t.Fatal("passwords longer than 8 bytes must be truncated to 8")
	}
}