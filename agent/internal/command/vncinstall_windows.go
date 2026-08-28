//go:build windows

// Windows VNC 一键安装/启动：
//   1. 优先 winget / choco 安装 TightVNC；
//   2. 否则通过 curl 下载 TightVNC MSI 静默安装（支持国内加速代理前缀）；
//   3. 若 TightVNC 不可用，回退尝试 TigerVNC。
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

// tigervncWinURL TigerVNC Windows 安装包（备选方案）。
const defaultTigerVNCWinURL = "https://github.com/TigerVNC/tigervnc/releases/download/v1.13.1/tigervnc-1.13.1.x86_64.exe"

// defaultProxy 国内加速代理前缀，直连失败时自动回退（用户可经 VncConfig 覆盖）。
const defaultProxy = "http://fa.mooncn.win/?url="

// vncInstallWindows 安装并启动 VNC，并写入随机口令。
// 优先使用 TightVNC；若 TightVNC 安装/启动失败则回退 TigerVNC。
func vncInstallWindows(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port}

	// 尝试安装并配置 TightVNC
	if installAndStartTightVNC(port, cfg, &res) {
		if portOpen(port) {
			res.OK = true
			res.Message = "VNC(TightVNC) 已就绪，端口 " + itoa(port) + "。"
			return res
		}
	}

	// TightVNC 不可用或启动失败，回退 TigerVNC
	if installAndStartTigerVNC(port, cfg, &res) {
		if portOpen(port) {
			res.OK = true
			res.Message = "VNC(TigerVNC) 已就绪，端口 " + itoa(port) + "。"
			return res
		}
	}

	// 都失败时返回最后的错误信息
	if res.Message == "" {
		res.Message = "VNC 安装/启动失败：TightVNC 和 TigerVNC 均未能正常运行。请检查权限或手动安装。"
	}
	return res
}

// installAndStartTightVNC 安装并启动 TightVNC，成功且端口就绪返回 true。
func installAndStartTightVNC(port int, cfg VncConfig, res *VncInstallResult) bool {
	installed := probeTightVNC()

	if !installed {
		// 依次尝试 winget / choco
		if tryInstall("winget", []string{"install", "--id", "TightVNC.TightVNC", "--accept-package-agreements", "--accept-source-agreements", "--silent"}) {
			installed = true
		} else if tryInstall("choco", []string{"install", "tightvnc", "-y"}) {
			installed = true
		}
	}
	// 仍未安装：直连/代理下载官方 MSI 并静默安装
	if !installed {
		if msg, ok := installTightVNCByCurl(cfg); ok {
			installed = true
			res.Message = msg
		} else {
			res.Message = msg
			return false
		}
	}
	if !installed {
		res.Message = "TightVNC 安装失败：未检测到已安装的 TightVNC。"
		return false
	}

	// 生成随机口令并写入注册表（DES 混淆），强制回环，禁止免密
	res.Password = generateRandomPassword()
	configureAndStartTightVNC(port, res.Password)
	if res.Message == "" {
		res.Message = "TightVNC 已安装并启动（端口 " + itoa(port) + "）。口令请使用本次下发值。"
	}
	return portOpen(port)
}

// vncRegistryKeys 需要写入的全部注册表位置（服务模式读 HKLM，用户会话读 HKCU/WOW6432）。
var vncRegistryKeys = []string{
	`HKLM\Software\TightVNC\Server`,
	`HKLM\Software\WOW6432Node\TightVNC\Server`,
	`HKCU\Software\TightVNC\Server`,
}

// configureAndStartTightVNC 写入 TightVNC 配置并启动：
//   1) 全部设置写入所有 hive（含 AllowLoopback 允许回环隧道回连）；
//   2) 使用指定口令（禁止免密）；
//   3) 设置监听端口（PortNumber）；
//   4) 启动顺序：优先在交互式用户会话中启动（避免 Session 0 隔离导致黑屏），
//      先尝试独立进程启动 → 计划任务用户会话 → 服务模式兜底。
func configureAndStartTightVNC(port int, password string) {
	dword := []struct{ name, val string }{
		// Authentication=1 启用口令认证；同时写 ControlPassword
		{"Authentication", "1"},
		// 端口号：确保 TightVNC 监听指定端口
		{"PortNumber", itoa(port)},
		// LoopbackOnly=1 仅允许回环，配合 Agent 隧道
		{"LoopbackOnly", "1"}, {"AllowLoopback", "1"},
		{"AcceptKeyEvents", "1"}, {"AcceptPointerEvents", "1"}, {"ViewOnly", "0"},
	}
	hexpw := ""
	if password != "" {
		hexpw = hex.EncodeToString(vncObfuscatePassword(password))
	}
	regOK := false
	for _, key := range vncRegistryKeys {
		keyOK := true
		for _, kv := range dword {
			if err := exec.Command("reg.exe", "add", key, "/v", kv.name, "/t", "REG_DWORD", "/d", kv.val, "/f").Run(); err != nil {
				log.Printf("[vnc-install] reg add %s %s failed: %v", key, kv.name, err)
				keyOK = false
			}
		}
		if hexpw != "" {
			for _, v := range []string{"Password", "ControlPassword", "PasswordViewOnly"} {
				if err := exec.Command("reg.exe", "add", key, "/v", v, "/t", "REG_BINARY", "/d", hexpw, "/f").Run(); err != nil {
					log.Printf("[vnc-install] reg add %s %s failed: %v", key, v, err)
					keyOK = false
				}
			}
		}
		if keyOK {
			regOK = true
		}
	}
	if !regOK {
		log.Printf("[vnc-install] 警告: 所有注册表写入均失败，TightVNC 可能无法使用指定口令")
	}

	exe := findTvnserverExe()
	if exe == "" {
		log.Printf("[vnc-install] 未找到 tvnserver.exe，跳过启动")
		return
	}

	// 先停止 TightVNC 服务并杀掉所有已运行的 tvnserver 实例，
	// 确保使用最新配置在交互式用户会话中重新启动
	if serviceExists("tvnserver") {
		_ = exec.Command("sc.exe", "stop", "tvnserver").Run()
	}
	_ = exec.Command("taskkill.exe", "/IM", "tvnserver.exe", "/F").Run()
	time.Sleep(1 * time.Second)

	// 1) 优先以独立进程启动：若 Agent 运行在交互式用户会话中，直接启动即可捕获桌面。
	//    若 Agent 作为服务运行（Session 0），独立进程也会落在 Session 0，仍需走第2步。
	startTvnserverDirect(exe)

	// 2) 若未监听，通过计划任务在交互式用户会话中启动，解决 Session 0 黑屏问题
	if !waitPortOpen(port, 5*time.Second) {
		_ = exec.Command("taskkill.exe", "/IM", "tvnserver.exe", "/F").Run()
		startInUserSession(exe)
	}

	// 3) 兜底：使用服务方式启动（此时可能受 Session 0 限制，但至少端口可达）
	if !waitPortOpen(port, 5*time.Second) && serviceExists("tvnserver") {
		_ = exec.Command("sc.exe", "stop", "tvnserver").Run()
		time.Sleep(2 * time.Second)
		_ = exec.Command("sc.exe", "config", "tvnserver", "start=", "auto").Run()
		_ = exec.Command("sc.exe", "start", "tvnserver").Run()
	}

	_ = waitPortOpen(port, 8*time.Second)
}

// startTvnserverDirect 以独立进程方式启动 TightVNC（继承 Agent 当前会话）。
func startTvnserverDirect(exe string) {
	psCmd := fmt.Sprintf("Start-Process -FilePath '%s'", strings.ReplaceAll(exe, "'", "''"))
	_ = exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", psCmd).Run()
}

// serviceExists 检查 Windows 服务是否存在。
func serviceExists(name string) bool {
	out, err := exec.Command("sc.exe", "query", name).Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "running") ||
		strings.Contains(strings.ToLower(string(out)), "stopped")
}

// startInUserSession 通过 Windows 计划任务在当前登录用户的交互会话中启动程序。
// 这解决了 agent 以服务方式运行时无法访问用户桌面（Session 0 隔离）的问题。
// 关键点：schtasks /it 让任务在登录用户的交互会话中运行，从而能捕获桌面内容。
func startInUserSession(exe string) {
	taskName := "MoCenter-VNC-Starter"
	// 在 /tr 参数中对可执行文件路径加引号，以支持带空格路径（如 C:\Program Files\...）
	quotedExe := "\"" + strings.ReplaceAll(exe, "\"", "\"\"") + "\""
	cmdStr := fmt.Sprintf(
		"schtasks /create /tn \"%s\" /tr %s /sc once /st 00:00 /it /f && schtasks /run /tn \"%s\" && timeout /t 5 /nobreak >nul && schtasks /delete /tn \"%s\" /f",
		taskName, quotedExe, taskName, taskName,
	)
	if err := exec.Command("cmd.exe", "/c", cmdStr).Run(); err != nil {
		log.Printf("[vnc-install] schtasks 用户会话启动失败: %v", err)
	}
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

// installTightVNCByCurl 下载 TightVNC MSI 并静默安装。
func installTightVNCByCurl(cfg VncConfig) (string, bool) {
	base := cfg.DownloadBase
	if base == "" {
		base = defaultTightVNCURL
	}
	if is32BitWindows() {
		base = strings.ReplaceAll(base, "64bit", "32bit")
	}

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

	var msi string
	var lastErr error
	for i, u := range urls {
		tmp := filepath.Join(os.TempDir(), "tightvnc-setup.msi")
		log.Printf("[vnc-install] 尝试下载 %s", u)
		var err error
		if i == len(urls)-1 {
			err = downloadPlain(u, tmp)
		} else {
			err = downloadFile(u, tmp)
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

	msilog := filepath.Join(os.TempDir(), "tightvnc-install.log")
	_ = os.Remove(msilog)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		if logData, e := os.ReadFile(msilog); e == nil {
			reason = lastLogLine(string(logData))
		}
	}
	if isElevationError(instErr) {
		lastErr = fmt.Errorf("msiexec 失败(%v)：请确认以管理员身份运行 Agent 后再试。%s", instErr, reason)
	} else {
		lastErr = fmt.Errorf("msiexec 执行失败: %v %s", instErr, reason)
	}
	return fmt.Sprintf("TightVNC 自动安装失败：%v。可手动下载安装：%s", lastErr, base), false
}

// isElevationError 判断 msiexec 退出码是否指向权限问题。
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

// downloadFile 下载文件到目标路径（PowerShell Invoke-WebRequest）。
func downloadFile(url, out string) error {
	return downloadSlowAware(out, minVNCBps, func(ctx context.Context) error {
		ps := fmt.Sprintf("Invoke-WebRequest -Uri '%s' -OutFile '%s' -UseBasicParsing", url, out)
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run()
	})
}

// downloadPlain 下载文件到目标路径，不带速率检测。
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

// probeTightVNC 探测是否已安装 TightVNC。
func probeTightVNC() bool {
	for _, p := range []string{
		`C:\Program Files\TightVNC\tvnserver.exe`,
		`C:\Program Files (x86)\TightVNC\tvnserver.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if bin, err := exec.Command("where.exe", "tvnserver.exe").Output(); err == nil && strings.TrimSpace(string(bin)) != "" {
		return true
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

// ===================== TigerVNC Fallback =====================

// installAndStartTigerVNC 安装并启动 TigerVNC，成功且端口就绪返回 true。
func installAndStartTigerVNC(port int, cfg VncConfig, res *VncInstallResult) bool {
	installed := probeTigerVNC()

	if !installed {
		// 尝试 winget / choco
		if tryInstall("winget", []string{"install", "--id", "TigerVNC.TigerVNC", "--accept-package-agreements", "--accept-source-agreements", "--silent"}) {
			installed = true
		} else if tryInstall("choco", []string{"install", "tigervnc", "-y"}) {
			installed = true
		}
	}
	// 仍未安装：下载安装包
	if !installed {
		if msg, ok := installTigerVNCByCurl(cfg); ok {
			installed = true
			res.Message = msg
		} else {
			if res.Message == "" {
				res.Message = msg
			} else {
				res.Message += "；TigerVNC 回退失败：" + msg
			}
			return false
		}
	}
	if !installed {
		if res.Message == "" {
			res.Message = "TigerVNC 安装失败。"
		} else {
			res.Message += "；TigerVNC 未安装。"
		}
		return false
	}

	// 生成口令并写入配置
	res.Password = generateRandomPassword()
	configureAndStartTigerVNC(port, res.Password)
	return portOpen(port)
}

// probeTigerVNC 探测是否已安装 TigerVNC。
func probeTigerVNC() bool {
	for _, p := range []string{
		`C:\Program Files\TigerVNC\vncserver.exe`,
		`C:\Program Files (x86)\TigerVNC\vncserver.exe`,
		`C:\Program Files\TigerVNC\vncserver-x11.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if bin, err := exec.Command("where.exe", "vncserver.exe").Output(); err == nil && strings.TrimSpace(string(bin)) != "" {
		return true
	}
	return false
}

// configureAndStartTigerVNC 配置并启动 TigerVNC。
// 优先在当前会话直接启动；若端口未就绪则通过计划任务在用户交互会话启动。
func configureAndStartTigerVNC(port int, password string) {
	// 写入 TigerVNC 口令（格式与 TightVNC 相同：DES 混淆的 8 字节）
	passFile := filepath.Join(os.TempDir(), "mocenter_vnc_passwd")
	if err := writeVncPasswdFile(passFile, password); err != nil {
		log.Printf("[vnc-install] TigerVNC 口令文件写入失败: %v", err)
		return
	}

	exe := findTigerVNCExe()
	if exe == "" {
		return
	}

	// 先杀掉已有 vncserver 进程
	_ = exec.Command("taskkill.exe", "/IM", "vncserver.exe", "/F").Run()
	time.Sleep(1 * time.Second)

	// 1) 直接启动 TigerVNC server（-rfbport 指定端口，-rfbauth 指定口令文件）
	psCmd := fmt.Sprintf(
		"Start-Process -FilePath '%s' -ArgumentList '-rfbport', '%d', '-rfbauth', '%s' -WindowStyle Hidden",
		strings.ReplaceAll(exe, "'", "''"), port, strings.ReplaceAll(passFile, "'", "''"),
	)
	_ = exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", psCmd).Run()

	// 2) 未就绪则通过计划任务在用户交互会话启动（避免 Session 0 黑屏）
	if !waitPortOpen(port, 5*time.Second) {
		_ = exec.Command("taskkill.exe", "/IM", "vncserver.exe", "/F").Run()
		taskName := "MoCenter-TigerVNC-Starter"
		// 在 /tr 参数中对可执行文件路径加引号，支持带空格路径
		quotedExe := "\"" + strings.ReplaceAll(exe, "\"", "\"\"") + "\""
		cmdStr := fmt.Sprintf(
			"schtasks /create /tn \"%s\" /tr %s -rfbport %d -rfbauth \"%s\" /sc once /st 00:00 /it /f && schtasks /run /tn \"%s\" && timeout /t 5 /nobreak >nul && schtasks /delete /tn \"%s\" /f",
			taskName, quotedExe, port, strings.ReplaceAll(passFile, "\"", "\\\""), taskName, taskName,
		)
		_ = exec.Command("cmd.exe", "/c", cmdStr).Run()
	}

	_ = waitPortOpen(port, 8*time.Second)
}

// findTigerVNCExe 定位已安装的 TigerVNC 可执行文件。
func findTigerVNCExe() string {
	for _, p := range []string{
		`C:\Program Files\TigerVNC\vncserver.exe`,
		`C:\Program Files (x86)\TigerVNC\vncserver.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if bin, err := exec.Command("where.exe", "vncserver.exe").Output(); err == nil {
		if p := strings.TrimSpace(string(bin)); p != "" {
			return p
		}
	}
	return ""
}

// installTigerVNCByCurl 下载 TigerVNC 安装包并静默安装。
func installTigerVNCByCurl(cfg VncConfig) (string, bool) {
	base := defaultTigerVNCWinURL

	var candidates []string
	if cfg.Proxy != "" {
		candidates = append(candidates, proxyURL(cfg.Proxy, base))
	}
	candidates = append(candidates, base)
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

	var setup string
	var lastErr error
	for i, u := range urls {
		tmp := filepath.Join(os.TempDir(), "tigervnc-setup.exe")
		log.Printf("[vnc-install] 尝试下载 TigerVNC %s", u)
		var err error
		if i == len(urls)-1 {
			err = downloadPlain(u, tmp)
		} else {
			err = downloadFile(u, tmp)
		}
		if err == nil {
			setup = tmp
			break
		}
		lastErr = fmt.Errorf("下载失败: %w", err)
		_ = os.Remove(tmp)
	}
	if setup == "" {
		return fmt.Sprintf("TigerVNC 下载失败：%v", lastErr), false
	}

	// TigerVNC 的 NSIS 安装包支持 /S 静默安装
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	inst := exec.CommandContext(ctx, setup, "/S", "/verysilent", "/suppressmsgboxes")
	out, instErr := inst.CombinedOutput()
	cancel()
	_ = os.Remove(setup)
	log.Printf("[vnc-install] TigerVNC 安装结束 err=%v out=%s", instErr, strings.TrimSpace(string(out)))
	if instErr == nil {
		return "已下载并安装 TigerVNC。", true
	}
	return fmt.Sprintf("TigerVNC 安装失败：%v", instErr), false
}
