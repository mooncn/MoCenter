//go:build windows

package command

import (
	"path/filepath"
	"strings"
	"testing"
)

// 服务端托管的包名必须与官方下载地址的文件名一致：
// Agent 从服务端取包时按包名查找，名称写歪会 404 并静默回退到公网下载（不易察觉）。
func TestUltraVNCPackageNameMatchesURL(t *testing.T) {
	cases := []struct{ name, url string }{
		{uvncSetupX64Name, uvncSetupX64URL},
		{uvncSetupX86Name, uvncSetupX86URL},
	}
	for _, c := range cases {
		if got := filepath.Base(c.url); got != c.name {
			t.Errorf("托管包名 %q 与下载地址 %q 的文件名(%q)不一致", c.name, c.url, got)
		}
	}
	if !strings.HasSuffix(uvncSetupX64Name, "x64_Setup.exe") {
		t.Errorf("x64 包名不符合预期: %q", uvncSetupX64Name)
	}
	if !strings.HasSuffix(uvncSetupX86Name, "x86_Setup.exe") {
		t.Errorf("x86 包名不符合预期: %q", uvncSetupX86Name)
	}
}

// 回归保护：ultravnc.ini 必须显式开启 ForceCursorShape。
//
// 背景：UltraVNC 在客户端未使用其私有 PointerPos 扩展时会默认关闭光标形状更新
// （vncclient.cpp: `if (!m_use_PointerPos) { if (!m_ForceCursorShape) { EnableXRichCursor(FALSE); ... } }`）。
// noVNC 是标准 RFB 客户端、不发 PointerPos，因此若不写 ForceCursorShape=1，
// 远端光标不会下发，表现为「画面正常但看不到鼠标」。
func TestUltraVNCIniForcesCursorShape(t *testing.T) {
	ini := ultraVNCIniContent(5900, "abcdefgh")
	mustHave := []string{
		"ForceCursorShape=1",
		"PortNumber=5900",
		"LoopbackOnly=1",
		"AllowLoopback=1",
		"AuthRequired=1",
		"MSLogonRequired=0",
		"NewMSLogon=0",
		"UseDSMPlugin=0",
	}
	for _, want := range mustHave {
		if !strings.Contains(ini, want) {
			t.Errorf("ultravnc.ini 缺少 %q\n--- 实际内容 ---\n%s", want, ini)
		}
	}
}

// 口令必须以 WritePrivateProfileStruct 格式写入：
// 16 位 HEX（8 字节 DES 混淆口令）+ 2 位 HEX 校验和 = 18 个字符。
// 只写 16 位会被 GetPrivateProfileStruct 判为损坏而丢弃，导致认证始终失败。
func TestUltraVNCIniPasswordFormat(t *testing.T) {
	ini := ultraVNCIniContent(5900, "abcdefgh")
	const prefix = "passwd="
	for _, line := range strings.Split(ini, "\r\n") {
		if !strings.HasPrefix(line, prefix) || strings.HasPrefix(line, "passwd2=") {
			continue
		}
		val := strings.TrimPrefix(line, prefix)
		if len(val) != 18 {
			t.Fatalf("passwd 长度 = %d, want 18 (%q)", len(val), val)
		}
		if val != strings.ToUpper(val) {
			t.Fatalf("passwd 应为大写 HEX: %q", val)
		}
		return
	}
	t.Fatalf("ultravnc.ini 中未找到 passwd 行:\n%s", ini)
}
