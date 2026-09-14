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

// 校验 Win32 WritePrivateProfileStruct 的编码格式。
// UltraVNC 读取 ultravnc.ini 的 passwd 时用 GetPrivateProfileStruct，
// 该 API 会重算校验和并比对，不一致即丢弃（表现为口令不生效）。
// 测试向量取自公开的兼容实现文档：24 字节结构体 → 48 位 HEX + 2 位校验和。
func TestPrivateProfileStructValue(t *testing.T) {
	data := []byte{
		0x01, 0x00, 0x00, 0x00,
		0x80, 0x07, 0x00, 0x00,
		0x38, 0x04, 0x00, 0x00,
		0x64, 0x00, 0x00, 0x00,
		0xC8, 0x00, 0x00, 0x00,
		0x20, 0x00, 0x00, 0x00,
	}
	const want = "01000000800700003804000064000000C80000002000000010"
	if got := privateProfileStructValue(data); got != want {
		t.Fatalf("privateProfileStructValue = %s, want %s", got, want)
	}
}

// 口令编码：8 字节混淆口令 → 16 位 HEX + 2 位校验和 = 18 个字符。
func TestPrivateProfileStructPasswordEncoding(t *testing.T) {
	enc := vncObfuscatePassword("Secure!")
	v := privateProfileStructValue(enc)
	if len(v) != 18 {
		t.Fatalf("编码长度 = %d, want 18 (%s)", len(v), v)
	}
	if strings.ToUpper(hex.EncodeToString(enc)) != v[:16] {
		t.Fatalf("前 16 位应为混淆口令 HEX，got %s", v[:16])
	}
	sum := 0
	for _, b := range enc {
		sum += int(b)
	}
	if want := strings.ToUpper(hex.EncodeToString([]byte{byte(sum)})); v[16:] != want {
		t.Fatalf("校验和 = %s, want %s", v[16:], want)
	}
}
