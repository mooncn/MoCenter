package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 二进制魔数校验：执行/安装前确认拿到的是真实可执行文件。
// 拦住两类情况：URL 已失效却下回一个 HTML 错误页；内容被中间人替换。
func TestExecutableMagic(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	pe := write("a.exe", append([]byte("MZ"), make([]byte, 64)...))
	elf := write("b.bin", append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...))
	html := write("c.html", []byte("<!DOCTYPE html><html>404 Not Found</html>"))
	short := write("d.bin", []byte("M"))
	empty := write("e.bin", nil)

	if !isPEExecutable(pe) {
		t.Error("PE 文件应被识别为 PE")
	}
	for _, p := range []string{elf, html, short, empty} {
		if isPEExecutable(p) {
			t.Errorf("非 PE 文件被误判为 PE: %s", p)
		}
	}
	if !isELFExecutable(elf) {
		t.Error("ELF 文件应被识别为 ELF")
	}
	for _, p := range []string{pe, html, empty} {
		if isELFExecutable(p) {
			t.Errorf("非 ELF 文件被误判为 ELF: %s", p)
		}
	}
	if isPEExecutable("") || isELFExecutable("") {
		t.Error("空路径应为 false")
	}
	if isPEExecutable(filepath.Join(dir, "not-exist")) {
		t.Error("不存在的文件应为 false")
	}
}

// 回归保护：服务模式的 systemd 单元必须包含能"跟着会话走"的关键参数。
//
//   - 少了 -auth guess：登录界面的 Xauthority 与用户会话不是同一个文件，
//     写死路径会随发行版/显示管理器变化而挂不上 :0，也就看不到登录界面。
//   - 少了 -loop：注销回到登录界面（X 服务重启）后 x11vnc 会直接退出。
//   - 少了 -localhost：会把 5900 暴露到所有网卡，绕过了 Agent 隧道的隔离。
func TestX11VNCServiceUnit(t *testing.T) {
	unit := buildX11VNCServiceUnit(5900, "/usr/bin/x11vnc")
	mustHave := []string{
		"ExecStart=/usr/bin/x11vnc",
		"-display :0",
		"-auth guess",
		"-rfbport 5900",
		"-rfbauth " + x11vncServicePassFile,
		"-localhost",
		"-forever",
		"-loop",
		"User=root",
		"Restart=always",
		"WantedBy=multi-user.target",
	}
	for _, want := range mustHave {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd 单元缺少 %q\n--- 实际内容 ---\n%s", want, unit)
		}
	}
}

// 两个模式的 x11vnc 命令行都必须带 "mocenter" 标记：
// 切换模式时靠 pkill -f 'x11vnc.*mocenter' 精确清理自己的实例，
// 标记丢失会导致旧实例继续占用端口，新模式"报成功但连不上"。
func TestX11VNCLogPathCarriesMarker(t *testing.T) {
	if !strings.Contains(x11vncServiceLog, "mocenter") {
		t.Errorf("x11vncServiceLog 必须含 mocenter 标记: %q", x11vncServiceLog)
	}
	if !strings.Contains(buildX11VNCServiceUnit(5900, "/usr/bin/x11vnc"), "-o "+x11vncServiceLog) {
		t.Errorf("服务单元未把日志指向带标记的路径，pkill 清理会失效")
	}
	// 交互模式脚本同样要带标记
	script := buildVncStartScript(5900, "/tmp/pw", "/root/.mocenter/vnc.log")
	if strings.Count(script, "-o \"$LOG\"") < 2 {
		t.Errorf("交互模式脚本的两个分支都应通过 -o 写入带标记的日志路径:\n%s", script)
	}
}

// x11vnc 的托管包名只表示「运维自备二进制」的约定文件名，不对应任何公网下载地址。
//
// 背景：LibVNC/x11vnc 官方**只发布源码**，GitHub release 里没有任何预编译产物。
// 代码里曾经写死的 releases/download/<ver>/x11vnc-<ver>-<arch> 是 404 死链，
// 已整体移除 —— 与其从第三方站点拉二进制（与防劫持目标相悖），
// 不如只认发行版包管理器与服务端托管的自备二进制。
func TestX11VNCPkgName(t *testing.T) {
	name := x11vncPkgName()
	if !strings.HasPrefix(name, "x11vnc-linux-") {
		t.Errorf("托管包名应以 x11vnc-linux- 开头: %q", name)
	}
	if strings.Contains(name, "http") || strings.Contains(name, "/") {
		t.Errorf("托管包名不应包含 URL 或路径分隔符: %q", name)
	}
	switch strings.TrimPrefix(name, "x11vnc-linux-") {
	case "amd64", "arm64", "arm", "386":
	default:
		t.Errorf("架构后缀不符合预期（应为 amd64/arm64/arm/386）: %q", name)
	}
}
