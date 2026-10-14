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

// 反混淆必须与混淆严格互逆，并与权威测试向量一致：
// obfuscate("Secure!") == D7A514D8C556AADE，故反混淆必须还原出 "Secure!"。
//
// 这个逆运算支撑"读取当前 VNC 口令"功能：没有它，口令一旦与服务端缓存脱钩
// （服务端重启、换人操作、手工装的 VNC）就只能靠重装去重置，
// 而那会打断正在使用的远程会话。
func TestVncDeobfuscateKnownVector(t *testing.T) {
	raw, err := hex.DecodeString("D7A514D8C556AADE")
	if err != nil {
		t.Fatal(err)
	}
	got, err := vncDeobfuscatePassword(raw)
	if err != nil {
		t.Fatalf("反混淆失败: %v", err)
	}
	if got != "Secure!" {
		t.Fatalf("deobfuscate = %q, want %q", got, "Secure!")
	}
}

// 往返：任意 8 字符口令混淆后再反混淆必须回到原值。
func TestVncPasswordRoundTrip(t *testing.T) {
	for _, pw := range []string{"abcdefgh", "ABCDEFGH", "aB3kM9zQ", "12345678", "zzzzzzzz"} {
		got, err := vncDeobfuscatePassword(vncObfuscatePassword(pw))
		if err != nil {
			t.Fatalf("%q 往返失败: %v", pw, err)
		}
		if got != pw {
			t.Errorf("往返后 = %q, want %q", got, pw)
		}
	}
}

// 长度不对必须报错，而不是返回一个看似可用的短口令。
func TestVncDeobfuscateRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 4, 7, 9, 16} {
		if _, err := vncDeobfuscatePassword(make([]byte, n)); err == nil {
			t.Errorf("%d 字节的输入应被拒绝", n)
		}
	}
}
