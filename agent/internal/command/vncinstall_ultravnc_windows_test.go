//go:build windows

package command

import (
	"os"
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

// 公网取包默认关闭：服务端未托管时必须**明确失败并给出办法**，而不是悄悄改用第三方源。
//
// 这条用例只走「取不到包 → 拒装」的分支，不会真的下载或安装任何东西。
func TestUltraVNCRefusesPublicDownloadByDefault(t *testing.T) {
	msg, ok := installUltraVNC(VncConfig{})
	if ok {
		t.Fatal("默认配置下不应成功安装（公网取包未解锁）")
	}
	if !strings.Contains(msg, uvncSetupX64Name) && !strings.Contains(msg, uvncSetupX86Name) {
		t.Errorf("失败消息应点明缺哪个包: %q", msg)
	}
	if !strings.Contains(msg, "fetch-packages.sh") {
		t.Errorf("失败消息应给出托管包的准备方式: %q", msg)
	}
	if !strings.Contains(msg, "allow_public_package_download") {
		t.Errorf("失败消息应说明解锁开关: %q", msg)
	}
}

// 执行前的钉扎闸：托管/下载来的包只要摘要不符，就必须拒装且不执行。
// 这里给一个内容不是 PE 的小文件，摘要必然不符 —— 断言的是「拒绝 + 删文件」，不会安装任何东西。
func TestUltraVNCRejectsUnpinnedPackage(t *testing.T) {
	dir := t.TempDir()
	setup := filepath.Join(dir, uvncSetupX64Name)
	if err := os.WriteFile(setup, []byte("not the pinned installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok := installUltraVNCFromLocal(setup, pinnedDigest(uvncSetupX64Name))
	if ok {
		t.Fatal("摘要不符必须拒装")
	}
	if !strings.Contains(msg, "installPins") {
		t.Errorf("消息应指明钉扎表位置: %q", msg)
	}
	if _, err := os.Stat(setup); !os.IsNotExist(err) {
		t.Error("被拒的包应就地删除，不留在校验失败的路径上")
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

// 回归：ini 里不是本次下发的口令时，校验必须失败。
//
// 这直接对应「旧实例退出时用旧口令回写 ini」这个间歇性故障：修复前没有任何读回校验，
// 于是一路报成功，而前端拿着新口令去连必然认证失败，且看不出原因。
func TestVerifyUltraVNCIniDetectsClobber(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "ultravnc.ini")

	// 模拟被旧实例回写覆盖：文件里是上一次的旧口令
	const oldPw = "OLDOLDOL"
	if err := os.WriteFile(ini, []byte(ultraVNCIniContent(5900, oldPw)), 0600); err != nil {
		t.Fatal(err)
	}

	// 本次下发的口令与文件不一致 → 必须报错
	if err := verifyUltraVNCIni(dir, "NEWNEWNE"); err == nil {
		t.Fatal("ini 中是旧口令，verifyUltraVNCIni 却通过了（回写覆盖会被漏掉）")
	}
	// 与文件一致 → 必须通过（否则会把正常情况也判成失败）
	if err := verifyUltraVNCIni(dir, oldPw); err != nil {
		t.Fatalf("ini 与口令一致时校验应通过，实际: %v", err)
	}
}

// 回归：ini 缺失或不可读时必须报错，不能静默通过。
func TestVerifyUltraVNCIniMissingFile(t *testing.T) {
	if err := verifyUltraVNCIni(t.TempDir(), "ABCDEFGH"); err == nil {
		t.Fatal("ini 不存在时校验应失败")
	}
}

// passwd2 不能被当成 passwd 解析：两者同值时最容易掩盖问题。
func TestIniPasswordValueSkipsPasswd2(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "ultravnc.ini")
	content := "[admin]\r\nAuthRequired=1\r\n\r\n[UltraVNC]\r\n" +
		"passwd=AAAABBBBCCCCDDDD01\r\npasswd2=111122223333444455\r\n"
	if err := os.WriteFile(ini, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if got := iniPasswordValue(ini); got != "AAAABBBBCCCCDDDD01" {
		t.Fatalf("应取 passwd 的值，实际取到 %q", got)
	}
}

// tasklist CSV 解析：既要取到 PID，又不能在「无匹配」的本地化提示行上误判。
// 环境清理是否彻底全靠这个判断，误判会让"旧实例仍在跑"被当成"已清空"。
func TestParseTasklistCSV(t *testing.T) {
	// 单条匹配（真实格式：进程名,PID,会话名,会话#,内存）
	if got := parseTasklistCSV("\"winvnc.exe\",\"4321\",\"Console\",\"1\",\"12,345 K\"\r\n"); len(got) != 1 || got[0] != "4321" {
		t.Fatalf("应解析出 PID 4321，实际 %v", got)
	}
	// 多条
	got := parseTasklistCSV("\"winvnc.exe\",\"1\",\"Console\",\"1\",\"1 K\"\r\n" +
		"\"winvnc.exe\",\"2\",\"Console\",\"1\",\"1 K\"\r\n")
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个 PID，实际 %v", got)
	}
	// 无匹配：tasklist 打印一行本地化提示，中英文各验一次
	for _, msg := range []string{
		"信息: 没有运行的任务匹配指定标准。\r\n",
		"INFO: No tasks are running which match the specified criteria.\r\n",
	} {
		if got := parseTasklistCSV(msg); len(got) != 0 {
			t.Fatalf("无匹配提示不应被解析成 PID，实际 %v", got)
		}
	}
	// 空输出
	if got := parseTasklistCSV(""); len(got) != 0 {
		t.Fatalf("空输出应为空，实际 %v", got)
	}
}

// 校验和不符时必须报错，绝不能把 HEX 段解出来当口令返回。
//
// UltraVNC 用 GetPrivateProfileStruct 读取 passwd，校验和对不上就判数据损坏并**丢弃**，
// 此时若照常返回解出的口令，等于给出一个必然连不上的错误口令 —— 比报错更难排查。
func TestDecodeUltraVNCPasswordChecksum(t *testing.T) {
	good := privateProfileStructValue(vncObfuscatePassword("abcdefgh"))
	pw, err := decodeUltraVNCPassword(good)
	if err != nil {
		t.Fatalf("正确编码应能解出: %v", err)
	}
	if pw != "abcdefgh" {
		t.Fatalf("解出 %q, want %q", pw, "abcdefgh")
	}

	// 篡改末位校验和（避开恰好相同的情况）
	bad := good[:len(good)-1] + "0"
	if bad == good {
		bad = good[:len(good)-1] + "1"
	}
	if _, err := decodeUltraVNCPassword(bad); err == nil {
		t.Fatal("校验和不符时必须报错（否则会返回一个连不上的错误口令）")
	}

	// 非 HEX 与长度异常
	for _, s := range []string{"ZZZZ", "ABC", "", "GGGGGGGGGGGGGGGGGG"} {
		if _, err := decodeUltraVNCPassword(s); err == nil {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}
