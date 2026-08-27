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
	if len(a) != 12 || a == b {
		t.Fatalf("unexpected random passwords: %q %q", a, b)
	}
}