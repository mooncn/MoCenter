//go:build windows

// UltraVNC 服务模式安装/配置/启动。
//
// 为什么用 UltraVNC 而不是 TightVNC：Windows 在锁屏/未登录时会把活动桌面切到
// WinSta0\Winlogon（安全桌面），DXGI Desktop Duplication 与普通桌面抓取都会失效
// （AcquireNextFrame 返回 DXGI_ERROR_ACCESS_LOST），表现为整屏黑。
// 要抓到安全桌面，必须由一个以 SYSTEM 身份运行、并能切换桌面的服务来完成。
//
// UltraVNC 的服务（uvnc_service）正是这个双进程模型：
//   - 服务以 LocalSystem 常驻，用 WTS API 跟踪会话变化；
//   - 通过 WTSQueryUserToken / 劫持 winlogon.exe 进程令牌（DuplicateTokenEx）拿到目标会话令牌；
//   - 以 lpDesktop = "Winsta0\\Winlogon" 启动工作进程，从而能抓取登录/锁屏界面。
//     （见 UltraVNC 源码 UltraVNCService.cpp：service_name="uvnc_service"、
//     pad() 把 binary path 注册为 `"winvnc.exe" -service`、StartupInfo.lpDesktop = L"Winsta0\\Winlogon"）
//
// TightVNC 的 service mode 在锁屏/登录界面有大量黑屏反馈，故 Windows 侧统一改用 UltraVNC：
// 服务模式走 uvnc_service，交互会话模式走 UltraVNC 应用模式（见 vncinstall_windows.go）。
package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// uvncSetupBaseURL UltraVNC 官方下载地址前缀
	// （Inno Setup 安装包，来源 winget-pkgs 清单 uvncbvba.UltraVNC）。
	uvncSetupBaseURL = "https://uvnc.eu/download/1800/"
	// 安装包文件名常量（uvncSetupX64Name / uvncSetupX86Name）定义在 vncinstall_common.go，
	// 与服务端托管包名、钉扎表的键共用同一份定义。
	// 官方下载地址由前缀 + 文件名派生，保证与托管包名不会写歪
	uvncSetupX64URL = uvncSetupBaseURL + uvncSetupX64Name
	uvncSetupX86URL = uvncSetupBaseURL + uvncSetupX86Name
	// uvncServiceName UltraVNC 服务名（源码内固定为 "uvnc_service"）。
	uvncServiceName = "uvnc_service"
)

// uvncSetupName 返回当前系统架构对应的安装包文件名。
func uvncSetupName() string {
	if is32BitWindows() {
		return uvncSetupX86Name
	}
	return uvncSetupX64Name
}

// installAndStartUltraVNCService 以「服务模式」安装并启动 UltraVNC。
// 该模式下即使目标机处于锁屏/未登录状态，VNC 仍能抓到画面。
func installAndStartUltraVNCService(port int, cfg VncConfig, res *VncInstallResult) bool {
	// 1) 确保 UltraVNC 已安装，并定位 winvnc.exe
	exe := findWinvncExe()
	if exe == "" {
		msg, ok := installUltraVNC(cfg)
		if !ok {
			res.Message = msg
			return false
		}
		exe = findWinvncExe()
	}
	if exe == "" {
		res.Message = "UltraVNC 安装后未找到 winvnc.exe，无法配置服务模式"
		return false
	}

	// 2) 生成随机口令
	res.Password = generateRandomPassword()

	// 2.1) 无头检测：服务模式同样需要「有活动显示输出」才能抓到画面。
	// 若为无头（无物理显示器）且未装虚拟显示器，则尝试安装 VDD。
	vddMsg := ""
	if vddOK, msg := ensureVirtualDisplay(cfg); !vddOK {
		log.Printf("[vnc-install] UltraVNC 服务模式警告: %s", msg)
		vddMsg = msg
	}

	// 3) 让配置真正生效：清环境 → 写配置 → 读回校验。
	//    顺序不可颠倒：winvnc.exe 退出时会把内存中的设置回写 ultravnc.ini，
	//    先写配置会被旧实例的退出回写覆盖成旧口令 —— 这正是「前端拿到新口令却
	//    认证失败」且时有时无的来源。
	if err := prepareUltraVNCConfig(filepath.Dir(exe), port, res.Password); err != nil {
		res.Message = err.Error()
		return false
	}

	// 4) 注册并启动 uvnc_service
	if err := registerUltraVNCService(exe); err != nil {
		res.Message = "注册 UltraVNC 服务失败: " + err.Error()
		return false
	}
	// 重启服务以加载新写入的配置：先停并等待完全停止，再启动
	// （重复点击「一键安装」时，未等停稳就启动会报 service is already running）
	_ = exec.Command("sc.exe", "stop", uvncServiceName).Run()
	waitServiceStopped(uvncServiceName, 10*time.Second)
	if out, err := exec.Command("sc.exe", "start", uvncServiceName).CombinedOutput(); err != nil {
		res.Message = "启动 UltraVNC 服务失败: " + err.Error() + " " + strings.TrimSpace(string(out))
		return false
	}
	// 服务必须真正进入 RUNNING，否则后续「端口已监听」可能来自残留实例而非本服务
	if !waitServiceRunning(uvncServiceName, 10*time.Second) {
		res.Message = "UltraVNC 服务未进入运行状态（" + serviceState(uvncServiceName) + "），请检查配置文件与运行权限"
		return false
	}

	// 6) 等待端口就绪，并确认监听者确实是本次启动的实例
	if !waitPortOpen(port, 20*time.Second) {
		res.Message = "UltraVNC 服务已启动但端口未监听，请检查服务状态（sc query " + uvncServiceName + "）与配置文件"
		return false
	}
	if err := confirmPortIsOurs(port); err != nil {
		res.Message = "UltraVNC 服务已启动但端口归属异常: " + err.Error()
		return false
	}
	res.Message = "UltraVNC 服务模式已就绪（端口 " + itoa(port) + "）。锁屏/登录界面亦可查看。口令请使用本次下发值。"
	if vddMsg != "" {
		res.Message += " " + vddMsg
	}
	return true
}

// installAndStartUltraVNCApp 以「应用模式」在登录用户的交互会话中启动 UltraVNC。
//
// 与服务模式共用同一套 ultravnc.ini 与口令编码，唯一区别是不注册 uvnc_service：
// 适用于不需要（或不允许）注册 SYSTEM 服务的场景，保持最小权限。
// 代价是锁屏/未登录时看不到画面（安全桌面抓取依赖服务）。
func installAndStartUltraVNCApp(port int, cfg VncConfig, res *VncInstallResult) bool {
	// 1) 确保 UltraVNC 已安装
	exe := findWinvncExe()
	if exe == "" {
		msg, ok := installUltraVNC(cfg)
		if !ok {
			res.Message = msg
			return false
		}
		exe = findWinvncExe()
	}
	if exe == "" {
		res.Message = "UltraVNC 安装后未找到 winvnc.exe，无法以应用模式启动"
		return false
	}

	// 2) 生成随机口令
	res.Password = generateRandomPassword()

	// 3) 无头检测（与其它模式一致：无活动显示输出必然黑屏）
	vddMsg := ""
	if vddOK, msg := ensureVirtualDisplay(cfg); !vddOK {
		log.Printf("[vnc-install] UltraVNC 应用模式警告: %s", msg)
		vddMsg = msg
	}

	// 4) 让配置真正生效：清环境 → 写配置 → 读回校验。
	//    应用模式与服务模式共用同一份 ultravnc.ini，同样受"旧实例退出回写"影响，
	//    顺序理由见 prepareUltraVNCConfig。
	if err := prepareUltraVNCConfig(filepath.Dir(exe), port, res.Password); err != nil {
		res.Message = err.Error()
		return false
	}

	// 5) 优先直接启动（Agent 若在交互会话中则立即可见）；
	//    未就绪则通过计划任务在交互会话中启动，解决 Session 0 隔离。
	startWinvncDirect(exe)
	if !waitPortOpen(port, 5*time.Second) {
		// 直启实例未监听（多为 Agent 在 Session 0）：结束它并**等确认退出**，
		// 再由计划任务拉进交互会话。只发 taskkill 就往下走会让残留实例占着端口，
		// 使后续的"端口已监听"判断失真。
		killWinvncProcesses()
		_ = waitNoWinvnc(10 * time.Second)
		startInUserSession(exe)
	}
	if !waitPortOpen(port, 10*time.Second) {
		res.Message = "UltraVNC 应用模式启动后端口未监听（可能需管理员权限，或交互会话未就绪）"
		return false
	}
	if err := confirmPortIsOurs(port); err != nil {
		res.Message = "UltraVNC 应用模式端口归属异常: " + err.Error()
		return false
	}

	// 6) 登录自启任务：系统重启后自动在交互会话中拉起
	ensureVncLogonTask(exe)

	res.Message = "UltraVNC 应用模式已就绪（端口 " + itoa(port) + "）。锁屏/登录界面不可见，需要时请改用服务模式。口令请使用本次下发值。"
	if vddMsg != "" {
		res.Message += " " + vddMsg
	}
	return true
}

// startWinvncDirect 以独立进程方式启动 winvnc.exe（继承 Agent 当前会话）。
func startWinvncDirect(exe string) {
	psCmd := "Start-Process -FilePath " + psQuote(exe) + " -WindowStyle Hidden"
	_ = exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", psCmd).Run()
}

// stopUltraVNCService 停止 uvnc_service 并改为手动启动，避免它与应用模式实例争抢端口。
// 切回服务模式时 registerUltraVNCService 会把启动类型改回 auto，因此是幂等可逆的。
// 注意：本函数只处理服务本身；残留的 winvnc.exe 统一由 killWinvncProcesses 负责。
func stopUltraVNCService() {
	if !serviceExists(uvncServiceName) {
		return
	}
	_ = exec.Command("sc.exe", "stop", uvncServiceName).Run()
	waitServiceStopped(uvncServiceName, 10*time.Second)
	_ = exec.Command("sc.exe", "config", uvncServiceName, "start=", "demand").Run()
}

// ultraVNCDirCandidates UltraVNC 可能的安装目录（winget 清单 DefaultInstallLocation 为
// %ProgramFiles%\uvnc bvba\UltraVNC）。
func ultraVNCDirCandidates() []string {
	var dirs []string
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		dirs = append(dirs,
			filepath.Join(base, "uvnc bvba", "UltraVNC"),
			filepath.Join(base, "UltraVNC"),
		)
	}
	return dirs
}

// findWinvncExe 定位 UltraVNC 的 winvnc.exe。
func findWinvncExe() string {
	for _, d := range ultraVNCDirCandidates() {
		p := filepath.Join(d, "winvnc.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if bin, err := exec.Command("where.exe", "winvnc.exe").Output(); err == nil {
		if p := strings.TrimSpace(strings.Split(string(bin), "\n")[0]); p != "" {
			return p
		}
	}
	return ""
}

// installUltraVNC 取并安装 UltraVNC 安装包。
//
// 取包顺序即信任顺序：
//  1. 服务端托管包（默认路径）：走 Agent↔服务端已 mutual-TLS 的长连接，源与版本都由运维掌握；
//  2. 公网取包（winget / choco / 厂商直连 / https 加速代理）：仅当运维在本机显式解锁。
//
// 无论哪条，执行前都按 installPins 的 SHA-256 校验；校验不通过**不会回退到下一条**——
// 静默改用别的源会把「内容被替换」伪装成「安装方式不同」，正是要防的事。
func installUltraVNC(cfg VncConfig) (string, bool) {
	name := uvncSetupName()
	want := pinnedDigest(name)
	if want == "" {
		// 钉扎缺失通常是包名常量改了而忘了同步摘要：宁可拒装，也不执行未钉扎的安装器。
		return "缺少 " + name + " 的内置钉扎摘要，已拒绝安装（请同步 installPins 后重新发布 Agent）", false
	}
	// 1) 服务端托管
	if cfg.FetchPackage != nil {
		path, err := cfg.FetchPackage(name)
		if err != nil {
			log.Printf("[vnc-install] 服务端未托管安装包 %s（%v）", name, err)
		} else {
			msg, ok := installUltraVNCFromLocal(path, want)
			if ok {
				msg = "已通过服务端下发的安装包完成安装。"
			}
			return msg, ok
		}
	}
	// 2) 公网取包（需显式解锁）
	if !cfg.AllowPublicDownload {
		return "服务端未托管安装包 " + name + publicDownloadHint(cfg) +
			"。准备托管包：bash scripts/fetch-packages.sh <服务端 packages 目录> win", false
	}
	// 2a) winget（Windows 10 1809+ 自带 App Installer）/ Chocolatey：
	//     二者各自校验清单摘要，但选源与选版本都不由我们决定，故同样只在解锁后可用。
	if tryInstall("winget", []string{"install", "--id", "uvncbvba.UltraVNC", "-e", "--silent",
		"--accept-package-agreements", "--accept-source-agreements"}) {
		if findWinvncExe() != "" {
			return "已通过 winget 安装 UltraVNC。", true
		}
	}
	// Chocolatey
	if tryInstall("choco", []string{"install", "ultravnc", "-y"}) {
		if findWinvncExe() != "" {
			return "已通过 choco 安装 UltraVNC。", true
		}
	}
	// 2b) 直连/https 代理下载官方安装包并静默安装
	return installUltraVNCByDownload(cfg, want)
}

// installUltraVNCByDownload 下载官方安装包（Inno Setup）并静默安装，下载结果先按 want 校验摘要。
func installUltraVNCByDownload(cfg VncConfig, want string) (string, bool) {
	base := uvncSetupX64URL
	if is32BitWindows() {
		base = uvncSetupX86URL
	}

	var candidates []string
	if cfg.DownloadBase != "" && strings.Contains(strings.ToLower(cfg.DownloadBase), "ultravnc") {
		candidates = append(candidates, cfg.DownloadBase)
	}
	if p := httpsProxyOrEmpty(cfg.Proxy, "UltraVNC 安装包下载"); p != "" {
		candidates = append(candidates, proxyURL(p, base))
	}
	candidates = append(candidates, base)
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
		// 落盘名用规范包名：出错时看目录就知道是哪个包，也避免多架构共用一个含糊的临时名。
		tmp := filepath.Join(os.TempDir(), uvncSetupName())
		log.Printf("[vnc-install] 尝试下载 UltraVNC %s", u)
		var err error
		if i == len(urls)-1 {
			err = downloadPlain(u, tmp)
		} else {
			err = downloadFile(u, tmp)
		}
		if err == nil {
			// 摘要不符时换下一个源继续试：某个源失效（下回错误页/半截文件）不该阻断安装，
			// 但无论如何都不会执行不符的包。
			if verr := verifyDigest(tmp, want, uvncSetupName()); verr != nil {
				err = verr
			} else {
				setup = tmp
				break
			}
		}
		lastErr = err
		_ = os.Remove(tmp)
	}
	if setup == "" {
		return fmt.Sprintf("UltraVNC 下载失败：%v。可手动安装：%s", lastErr, base), false
	}
	msg, ok := installUltraVNCFromLocal(setup, want)
	if !ok {
		return msg, false
	}
	return "已下载并安装 UltraVNC。", true
}

// installUltraVNCFromLocal 用本地已就绪的 UltraVNC 安装包静默安装
// （Inno Setup 参数），安装结束后删除安装包。
// 服务端下发的包与公网下载的包共用本函数，保证「执行前校验 + 安装参数」一致。
func installUltraVNCFromLocal(setup, want string) (string, bool) {
	// 执行前的两道闸：魔数挡掉「下回一个 HTML 错误页」这类静默失败，摘要挡掉内容被替换。
	if err := verifyDigest(setup, want, filepath.Base(setup)); err != nil {
		_ = os.Remove(setup)
		return err.Error(), false
	}
	if !isPEExecutable(setup) {
		_ = os.Remove(setup)
		return "安装包不是有效的 Windows 可执行文件（可能下载到了错误页面或被篡改），已拒绝执行", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, setup, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-").CombinedOutput()
	_ = os.Remove(setup)
	log.Printf("[vnc-install] UltraVNC 安装结束 err=%v", err)
	if err != nil {
		return fmt.Sprintf("UltraVNC 静默安装失败：%v %s", err, strings.TrimSpace(string(out))), false
	}
	time.Sleep(3 * time.Second)
	if findWinvncExe() == "" {
		return "UltraVNC 安装程序已执行，但未找到 winvnc.exe（可能需重启或权限不足）", false
	}
	return "UltraVNC 安装完成。", true
}

// ultraVNCIniPaths 返回需要写入 ultravnc.ini 的路径。
// 服务模式（标准模式）下 UltraVNC 读取 %ProgramData%\UltraVNC\ultravnc.ini；
// 同时写入安装目录以兼容便携模式。
func ultraVNCIniPaths(installDir string) []string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	paths := []string{filepath.Join(pd, "UltraVNC", "ultravnc.ini")}
	if installDir != "" {
		paths = append(paths, filepath.Join(installDir, "ultravnc.ini"))
	}
	return paths
}

// writeUltraVNCIni 写入 UltraVNC 配置：端口、仅回环、启用口令认证、禁用 MS Logon 与 DSM 插件。
func writeUltraVNCIni(installDir string, port int, password string) error {
	content := ultraVNCIniContent(port, password)
	var lastErr error
	wrote := false
	for _, p := range ultraVNCIniPaths(installDir) {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			lastErr = err
			continue
		}
		// 0600 只是 Unix 语义；Windows 上必须显式收紧 DACL，否则继承 %ProgramData%
		// 默认的 BUILTIN\Users 读权限 —— 文件里是可用公开固定密钥还原的 VNC 口令。
		// 收紧失败不阻断安装（UltraVNC 仍需工作），但必须留在日志与回执里。
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			lastErr = err
			continue
		}
		if err := restrictPathToPrivileged(p); err != nil {
			log.Printf("[vnc-install] 警告: %s 的 VNC 口令仍可被本机其他用户读取: %v", p, err)
		}
		log.Printf("[vnc-install] 已写入 UltraVNC 配置: %s", p)
		wrote = true
	}
	if !wrote {
		if lastErr == nil {
			lastErr = fmt.Errorf("无可写路径")
		}
		return lastErr
	}
	return nil
}

// ultraVNCIniContent 生成 ultravnc.ini 内容。
//
// 字段说明（section/key 均取自 UltraVNC 源码 SettingsManager.cpp）：
//   - [admin] PortNumber：VNC 监听端口；
//   - [admin] HTTPConnect=0：关闭内置 HTTP（Java 查看器）端口，避免额外暴露；
//   - [admin] AutoPortSelect=0：固定端口，不自动递增；
//   - [admin] LoopbackOnly=1 + AllowLoopback=1：仅监听回环，配合 Agent 隧道；
//   - [admin] AuthRequired=1：必须口令认证；
//   - [admin] MSLogonRequired=0 / NewMSLogon=0：使用 VNC 口令认证（noVNC 不支持 MS Logon）；
//   - [admin] UseDSMPlugin=0：不启用加密插件（noVNC 不支持 UltraVNC DSM 插件）；
//   - [admin] ForceCursorShape=1：强制发送光标形状更新（RichCursor/XCursor）。
//     必须显式开启：UltraVNC 在客户端未使用其私有 PointerPos 扩展时，默认会关闭
//     光标形状更新（见 vncclient.cpp：`if (!m_use_PointerPos) { if (!m_ForceCursorShape)
//     { EnableXRichCursor(FALSE); ... } }`）。noVNC 是标准 RFB 客户端、不发 PointerPos，
//     于是默认配置下远端光标不会下发 —— 表现为画面正常但看不到鼠标。
//   - [UltraVNC] passwd / passwd2：DES 混淆口令，编码为 WritePrivateProfileStruct 格式。
func ultraVNCIniContent(port int, password string) string {
	pw := privateProfileStructValue(vncObfuscatePassword(password))
	return "[admin]\r\n" +
		"PortNumber=" + itoa(port) + "\r\n" +
		"HTTPConnect=0\r\n" +
		"AutoPortSelect=0\r\n" +
		"LoopbackOnly=1\r\n" +
		"AllowLoopback=1\r\n" +
		"AuthRequired=1\r\n" +
		"MSLogonRequired=0\r\n" +
		"NewMSLogon=0\r\n" +
		"UseDSMPlugin=0\r\n" +
		"ForceCursorShape=1\r\n" +
		"FileTransferEnabled=0\r\n" +
		"\r\n" +
		"[UltraVNC]\r\n" +
		"passwd=" + pw + "\r\n" +
		"passwd2=" + pw + "\r\n"
}

// stopConflictingVNC 清理可能抢占 VNC 端口的其它实例：
// 旧版本曾用 TightVNC/TigerVNC 部署，从旧版本升级上来的机器可能仍残留
// tvnserver 服务（Session 0，会抓到空桌面）或 tvnserver/vncserver 进程，
// 以及交互模式的登录自启任务。这里一并停用/结束，避免与 UltraVNC 争抢同一端口。
func stopConflictingVNC() {
	if serviceExists("tvnserver") {
		_ = exec.Command("sc.exe", "stop", "tvnserver").Run()
		_ = exec.Command("sc.exe", "config", "tvnserver", "start=", "disabled").Run()
	}
	_ = exec.Command("taskkill.exe", "/IM", "tvnserver.exe", "/F").Run()
	_ = exec.Command("taskkill.exe", "/IM", "vncserver.exe", "/F").Run()
	_ = exec.Command("cmd.exe", "/c", `schtasks /delete /tn "MoCenter-VNC-Logon" /f`).Run()
	time.Sleep(1 * time.Second)
}

// registerUltraVNCService 注册（或修正）uvnc_service。
// 与 UltraVNC 自身一致：binary path 为 `"<winvnc.exe>" -service`，
// 启动类型 AUTO_START，LocalSystem 账户。
func registerUltraVNCService(exe string) error {
	binPath := `"` + exe + `" -service`
	if serviceExists(uvncServiceName) {
		// 已存在：修正 binPath 与启动类型（幂等）
		out, err := exec.Command("sc.exe", "config", uvncServiceName, "start=", "auto", "binPath=", binPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	out, err := exec.Command("sc.exe", "create", uvncServiceName,
		"start=", "auto",
		"DisplayName=", uvncServiceName,
		"binPath=", binPath,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[vnc-install] 已注册服务 %s: %s", uvncServiceName, binPath)
	return nil
}

// waitServiceStopped 轮询等待服务进入 STOPPED 状态（服务不存在或查询失败视为已停止）。
func waitServiceStopped(name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("sc.exe", "query", name).Output()
		if err != nil || strings.Contains(string(out), "STOPPED") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitServiceRunning 轮询等待服务进入 RUNNING 状态。
func waitServiceRunning(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("sc.exe", "query", name).Output()
		if err == nil && strings.Contains(string(out), "RUNNING") {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// serviceState 返回服务 STATE 行的文本，用于错误信息中定位问题。
func serviceState(name string) string {
	out, err := exec.Command("sc.exe", "query", name).Output()
	if err != nil {
		return "查询失败: " + err.Error()
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "STATE") {
			return strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
		}
	}
	return "未知"
}

// killWinvncProcesses 结束所有 winvnc.exe 进程：应用模式实例、以及可能残留的
// 服务工作进程。UltraVNC 的应用模式进程不在服务管理器管辖范围内，切换模式时
// 必须显式结束，否则会一直占用端口导致另一种模式绑定失败。
//
// 注意：taskkill /F 只是「请求结束」，返回时进程往往仍然存活。调用方必须再用
// waitNoWinvnc 确认真的退出了 —— 不能靠 sleep 猜（见 prepUltraVNCEnv）。
func killWinvncProcesses() {
	_ = exec.Command("taskkill.exe", "/IM", "winvnc.exe", "/F").Run()
}

// winvncPIDs 返回当前存活的 winvnc.exe 进程 PID 列表。
func winvncPIDs() []string {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq winvnc.exe",
		"/NH", "/FO", "CSV").Output()
	if err != nil {
		return nil
	}
	return parseTasklistCSV(string(out))
}

// parseTasklistCSV 从 tasklist /NH /FO CSV 的输出里取第二列（PID）。
//
// 单独抽出来是为了可测：解析是这段逻辑里唯一容易出错、又不需要真实进程的部分。
// 无匹配时 tasklist 会打印一行本地化提示（不以引号开头），据此跳过 ——
// 不能按文本匹配，否则换系统语言就失效。
func parseTasklistCSV(out string) []string {
	var pids []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		fields := strings.Split(line, `","`)
		if len(fields) < 2 {
			continue
		}
		if pid := strings.Trim(fields[1], `" `); pid != "" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitNoWinvnc 轮询等待所有 winvnc.exe 退出，返回是否已清空。
func waitNoWinvnc(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(winvncPIDs()) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// prepareUltraVNCConfig 让配置真正生效：清环境 → 写配置 → 读回校验。
//
// 这三步必须按此顺序、且放在同一个函数里 —— 顺序本身就是正确性的一部分
// （winvnc.exe 退出会回写 ultravnc.ini）。分散到各调用方，早晚会被改错顺序，
// 而后果是间歇性的认证失败，极难排查。
func prepareUltraVNCConfig(installDir string, port int, password string) error {
	if err := prepUltraVNCEnv(); err != nil {
		return fmt.Errorf("清理既有 VNC 环境失败: %w", err)
	}
	if err := writeUltraVNCIni(installDir, port, password); err != nil {
		return fmt.Errorf("写入 UltraVNC 配置失败: %w", err)
	}
	if err := verifyUltraVNCIni(installDir, password); err != nil {
		return fmt.Errorf("UltraVNC 配置写入后校验失败: %w", err)
	}
	return nil
}

// prepUltraVNCEnv 在写配置之前把环境清干净：停冲突 VNC、停服务、结束所有 winvnc 实例，
// 并**等到确认清空**才返回。
//
// 顺序是关键：UltraVNC 的 winvnc.exe 在退出时会把内存中的设置**回写** ultravnc.ini。
// 若先写配置再杀旧实例，旧实例退出时会用旧口令覆盖刚写入的内容 —— 表现为
// 「口令写入成功、前端拿到新口令，但认证始终失败」，且是否发生取决于时序，
// 因此是间歇性的。正确顺序：清干净 → 写配置 → 启动。
func prepUltraVNCEnv() error {
	stopConflictingVNC()
	stopUltraVNCService()
	killWinvncProcesses()
	if !waitNoWinvnc(15 * time.Second) {
		return fmt.Errorf("winvnc.exe 未能全部退出（仍有 PID: %s）；旧实例会继续占用端口并用旧口令应答",
			strings.Join(winvncPIDs(), ","))
	}
	return nil
}

// iniPasswordValue 读回 ini 中 [UltraVNC] 段的 passwd 值；读不到返回空串。
// 注意 "passwd2=" 不会误匹配：这里要求 "passwd" 之后紧跟 '='。
func iniPasswordValue(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(strings.ToLower(line), "passwd=") {
			return strings.TrimSpace(line[len("passwd="):])
		}
	}
	return ""
}

// verifyUltraVNCIni 校验 UltraVNC 实际读取的那份 ini 里确实是我们本次下发的口令。
//
// 为什么必须读回：WriteFile 返回 nil 只证明"写这个路径成功了"，不能证明"生效了"。
// 旧实例回写、并发写入、权限导致的静默失败，都会让文件内容不是我们写的那份 ——
// 而前端已经拿着新口令去连了，结果就是认证失败却查不出原因。
func verifyUltraVNCIni(installDir, password string) error {
	want := privateProfileStructValue(vncObfuscatePassword(password))
	// UltraVNC 默认读 exe 同目录的 ini（安装器模式下另写一份到 ProgramData）。
	// 以 exe 目录这份为准校验。
	primary := filepath.Join(installDir, "ultravnc.ini")
	got := iniPasswordValue(primary)
	switch {
	case got == want:
		return nil
	case got == "":
		return fmt.Errorf("%s 中读不到 passwd（文件缺失或不可读）", primary)
	default:
		return fmt.Errorf("%s 中的 passwd 与本次下发的口令不一致"+
			"（典型原因是旧实例退出时回写了旧配置）", primary)
	}
}

// confirmPortIsOurs 确认端口上的监听者确实是本次启动的 winvnc。
//
// 调用前必须已用 prepUltraVNCEnv 证明过"启动前没有任何 winvnc"，
// 因此此刻只要端口开着就应当有 winvnc 进程；没有就说明端口被别的程序占着，
// 本次启动其实没成功 —— 这正是旧实现"端口已监听即判定成功"漏掉的情况。
func confirmPortIsOurs(port int) error {
	pids := winvncPIDs()
	if len(pids) == 0 {
		return fmt.Errorf("端口 %d 已被监听，但不存在 winvnc.exe 进程，端口被其它程序占用", port)
	}
	return nil
}
