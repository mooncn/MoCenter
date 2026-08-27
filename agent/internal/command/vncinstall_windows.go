//go:build windows

// Windows VNC 一键安装/启动：
//   1. 优先 winget / choco 安装 TightVNC；
//   2. 否则通过 curl 下载 TightVNC MSI 静默安装（支持国内加速代理前缀）。
package command

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// tightvncMSI 官方默认下载地址（可从 VncConfig.DownloadBase 覆盖）。
const defaultTightVNCURL = "https://www.tightvnc.com/download/2.8.88/tightvnc-2.8.88-gpl-setup-64bit.msi"

// defaultProxy 国内加速代理前缀，直连失败时自动回退（用户可经 VncConfig 覆盖）。
const defaultProxy = "http://fa.mooncn.win/?url="

// vncInstallWindows 安装并启动 TightVNC，并写入随机口令。
func vncInstallWindows(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port}
	installed := probeWinVNC()

	if !installed {
		// 依次尝试 winget / choco
		if tryInstall("winget", []string{"install", "--id", "TightVNC.TightVNC", "--accept-package-agreements", "--accept-source-agreements", "--silent"}) {
			installed = true
		} else if tryInstall("choco", []string{"install", "tightvnc", "-y"}) {
			installed = true
		}
	}
	// 仍未安装：直连/代理下载官方 MSI 并静默安装（兼容无 winget/choco 的环境）
	if !installed {
		if msg, ok := installTightVNCByCurl(cfg); ok {
			installed = true
			res.Message = msg
		} else {
			res.Message = msg
		}
	}
	if !installed {
		return res
	}

	// 生成随机口令并写入注册表（DES 混淆），强制回环，禁止免密
	res.Password = generateRandomPassword()
	configureAndStartWindowsVNC(port, res.Password)
	if res.Message == "" {
		res.Message = "TightVNC 已安装并启动（端口 " + itoa(port) + "）。口令请使用本次下发值。"
	}
	if portOpen(port) {
		res.OK = true
		res.Message = "VNC(TightVNC) 已就绪，端口 " + itoa(port) + "。"
	}
	return res
}

// vncRegistryKeys 需要写入的全部注册表位置（服务模式读 HKLM，用户会话读 HKCU/WOW6432）。
var vncRegistryKeys = []string{
	`HKLM\Software\TightVNC\Server`,
	`HKLM\Software\WOW6432Node\TightVNC\Server`,
	`HKCU\Software\TightVNC\Server`,
}

// configureAndStartWindowsVNC 写入 TightVNC 配置并启动：
//   1) 全部设置写入所有 hive（含 AllowLoopback 允许回环隧道回连）；
//   2) 使用指定口令（禁止免密）；
//   3) 启动顺序 windows：先用服务，未监听则以独立 GUI 进程（Start-Process）常驻启动，
//      避免服务/进程随 agent 命令上下文被收束导致 5900 瞬断。
func configureAndStartWindowsVNC(port int, password string) {
	dword := []struct{ name, val string }{
		// Authentication=1 启用口令认证；否则即使写了 ControlPassword 也被视为“未配置”
		{"Authentication", "1"},
		// LoopbackOnly=1 仅允许回环，配合 Agent 隧道；AllowLoopback=1 允许回环客户端连接
		{"LoopbackOnly", "1"}, {"AllowLoopback", "1"},
		{"AcceptKeyEvents", "1"}, {"AcceptPointerEvents", "1"}, {"ViewOnly", "0"},
	}
	hexpw := ""
	if password != "" {
		hexpw = hex.EncodeToString(vncObfuscatePassword(password))
	}
	for _, key := range vncRegistryKeys {
		for _, kv := range dword {
			_ = exec.Command("reg.exe", "add", key, "/v", kv.name, "/t", "REG_DWORD", "/d", kv.val, "/f").Run()
		}
		if hexpw != "" {
			// 同时写 Password / ControlPassword / PasswordViewOnly，兼容新旧版本取值
			for _, v := range []string{"Password", "ControlPassword", "PasswordViewOnly"} {
				_ = exec.Command("reg.exe", "add", key, "/v", v, "/t", "REG_BINARY", "/d", hexpw, "/f").Run()
			}
		}
	}

	exe := findTvnserverExe()

	// 1) 尝试以服务方式启动
	_ = exec.Command("sc.exe", "config", "tvnserver", "start=", "auto").Run()
	_ = exec.Command("sc.exe", "stop", "tvnserver").Run()
	_ = exec.Command("sc.exe", "start", "tvnserver").Run()

	// 2) 若服务方式未保持监听，则以独立 GUI 进程常驻启动（Start-Process 脱离 agent 上下文）
	if !waitPortOpen(port, 4*time.Second) && exe != "" {
		_ = exec.Command("taskkill.exe", "/IM", "tvnserver.exe", "/F").Run()
		ps := fmt.Sprintf("Start-Process -FilePath '%s'", strings.ReplaceAll(exe, "'", "''"))
		_ = exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps).Run()
	}
	_ = waitPortOpen(port, 6*time.Second)
}

// findTvnserverExe 定位已安装的 tvnserver 可执行文件。
func findTvnserverExe() string {
	for _, p := range []string{
		`C:\Program Files\TightVNC\tvnserver.exe`,
		`C:\Program Files (x86)\TightVNC\tvnserver.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if bin, err := exec.Command("where.exe", "tvnserver.exe").Output(); err == nil {
		if p := strings.TrimSpace(string(bin)); p != "" {
			return p
		}
	}
	return ""
}

// waitPortOpen 轮询等待端口监听，成功返回 true。
func waitPortOpen(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portOpen(port) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// installTightVNCByCurl 下载 TightVNC MSI 并静默安装。
// 仅需要一次成功下载；多个下载源用于提高可达性，安装只执行一次。
func installTightVNCByCurl(cfg VncConfig) (string, bool) {
	base := cfg.DownloadBase
	if base == "" {
		base = defaultTightVNCURL
	}
	if is32BitWindows() {
		base = strings.ReplaceAll(base, "64bit", "32bit")
	}

	// 候选下载地址：显式配置优先，否则直连+代理双通道依次尝试
	var candidates []string
	if cfg.Proxy != "" {
		candidates = append(candidates, proxyURL(cfg.Proxy, base))
	}
	if strings.HasPrefix(base, "http") {
		candidates = append(candidates, base)
	}
	if cfg.Proxy == "" {
		candidates = append(candidates, proxyURL(defaultProxy, base))
	}
	seen := map[string]bool{}
	var urls []string
	for _, u := range candidates {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	// 1) 成功下载到第一个可用的源；除兜底源外均做速率检测，兜底减速不中断
	var msi string
	var lastErr error
	for i, u := range urls {
		tmp := filepath.Join(os.TempDir(), "tightvnc-setup.msi")
		log.Printf("[vnc-install] 尝试下载 %s", u)
		var err error
		if i == len(urls)-1 {
			// 兜底源（fa.mooncn.win）：不再按速率判断，只限总超时
			err = downloadPlain(u, tmp)
		} else {
			err = downloadFile(u, tmp) // 含速率判断，过慢切换下一源
		}
		if err == nil {
			msi = tmp
			break
		}
		lastErr = fmt.Errorf("下载失败: %w", err)
		_ = os.Remove(tmp)
	}
	if msi == "" {
		log.Printf("[vnc-install] 下载全部失败: %v", lastErr)
		return fmt.Sprintf("TightVNC 自动安装失败：%v。可手动下载安装：%s", lastErr, base), false
	}
	log.Printf("[vnc-install] 下载成功，开始静默安装")

	// 2) 静默安装一次（默认装 Server+Viewer）。用 /l*v 记录日志便于排查。
	msilog := filepath.Join(os.TempDir(), "tightvnc-install.log")
	_ = os.Remove(msilog)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	inst := exec.CommandContext(ctx, "msiexec.exe", "/i", msi, "/qn", "/norestart", "/l*v", msilog)
	out, instErr := inst.CombinedOutput()
	cancel()
	_ = os.Remove(msi)
	log.Printf("[vnc-install] msiexec 结束 err=%v", instErr)
	if instErr == nil {
		return "已下载并安装 TightVNC。", true
	}
	reason := strings.TrimSpace(string(out))
	if reason == "" {
		if log, e := os.ReadFile(msilog); e == nil {
			reason = lastLogLine(string(log))
		}
	}
	if isElevationError(instErr) {
		lastErr = fmt.Errorf("msiexec 失败(%v)：请确认以管理员身份运行 Agent 后再试。%s", instErr, reason)
	} else {
		lastErr = fmt.Errorf("msiexec 执行失败: %v %s", instErr, reason)
	}
	return fmt.Sprintf("TightVNC 自动安装失败：%v。可手动下载安装：%s", lastErr, base), false
}

// isElevationError 判断 msiexec 退出码是否指向权限问题（1603 常见于未提权）。
func isElevationError(err error) bool {
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1603 {
		return true
	}
	return false
}

// lastLogLine 返回 MSI 日志最后几行以定位失败原因。
func lastLogLine(log string) string {
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return strings.Join(lines, "\n")
}

// downloadFile 下载文件到目标路径（PowerShell Invoke-WebRequest），
// 并入慢速检测：速率低于阈值即返回错误，调用方切换备选加速源。
func downloadFile(url, out string) error {
	return downloadSlowAware(out, minVNCBps, func(ctx context.Context) error {
		ps := fmt.Sprintf("Invoke-WebRequest -Uri '%s' -OutFile '%s' -UseBasicParsing", url, out)
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run()
	})
}

// downloadPlain 下载文件到目标路径，不带速率检测（用于兜底源 fa.mooncn.win，
// 兜底不因减速而中断），仅受总超时约束。
func downloadPlain(url, out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ps := fmt.Sprintf("Invoke-WebRequest -Uri '%s' -OutFile '%s' -UseBasicParsing", url, out)
	if err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run(); err != nil {
		return err
	}
	if fi, e := os.Stat(out); e == nil && fi.Size() > 0 {
		return nil
	}
	return fmt.Errorf("下载不完整（文件缺失或空）")
}

func is32BitWindows() bool {
	out, err := exec.Command("wmic", "os", "get", "OSArchitecture").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "32")
}

// probeWinVNC 探测是否已安装 TightVNC/TigerVNC。
func probeWinVNC() bool {
	for _, bin := range []string{"tvnserver.exe", "winvnc.exe", "vncserver.exe"} {
		p, err := exec.LookPath(bin)
		if err == nil && p != "" {
			return true
		}
		if _, err := os.Stat(`C:\Program Files\TightVNC\tvnserver.exe`); err == nil {
			return true
		}
		if _, err := os.Stat(`C:\Program Files (x86)\TightVNC\tvnserver.exe`); err == nil {
			return true
		}
	}
	return false
}

// tryInstall 尝试以命令行工具安装，成功返回 true。
func tryInstall(bin string, args []string) bool {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	_ = out
	return err == nil
}
