//go:build windows

// Windows VNC 一键安装/启动。
//
// Windows 侧统一使用 UltraVNC（实现见 vncinstall_ultravnc_windows.go），按模式分流：
//   - 服务模式（VncModeService）：注册 uvnc_service，以 SYSTEM 权限跟随会话切换，
//     可抓取 Winlogon 安全桌面，锁屏/登录界面也能看到画面；
//   - 交互会话模式（默认）：以应用模式在登录用户会话中运行，不注册服务（最小权限），
//     锁屏/未登录时不可见。
//
// 历史实现 TightVNC / TigerVNC 已在两种模式均真机验证通过后移除，
// 避免同时维护三个 VNC 产品与两套口令编码。
package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// vncInstallWindows 安装并启动 VNC，并写入随机口令。
//
// 按 cfg.Mode 分流：
//   - VncModeService：UltraVNC 服务模式，支持锁屏/登录界面抓屏；
//     失败时降级为交互会话模式（并明确提示锁屏不可见）。
//   - 其它（默认）：UltraVNC 应用模式（交互会话，不注册服务）。
func vncInstallWindows(port int, cfg VncConfig) VncInstallResult {
	if cfg.Mode == VncModeService {
		res := VncInstallResult{Port: port, Mode: VncModeService}
		if installAndStartUltraVNCService(port, cfg, &res) && portOpen(port) {
			res.OK = true
			return res
		}
		// 服务模式失败：保留失败原因，降级为交互会话模式
		failMsg := res.Message
		if failMsg == "" {
			failMsg = "服务模式未能就绪"
		}
		log.Printf("[vnc-install] 服务模式失败，降级为交互会话模式: %s", failMsg)
		fallback := installInteractiveWindows(port, cfg)
		if fallback.OK {
			fallback.Message = "服务模式失败（" + failMsg + "），已降级为交互会话模式（锁屏/登录界面不可见）：" + fallback.Message
			return fallback
		}
		return VncInstallResult{
			Port: port, Mode: VncModeService,
			Message: "服务模式失败：" + failMsg + "；交互模式亦失败：" + fallback.Message,
		}
	}
	return installInteractiveWindows(port, cfg)
}

// installInteractiveWindows 交互会话模式：以 UltraVNC 应用模式在登录用户的交互会话中
// 运行 VNC，不注册 SYSTEM 服务（最小权限）。锁屏/未登录时不可见桌面。
func installInteractiveWindows(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port, Mode: VncModeInteractive}
	if installAndStartUltraVNCApp(port, cfg, &res) && portOpen(port) {
		res.OK = true
	}
	return res
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

// ensureVncLogonTask 创建（或更新）持久化的「用户登录后自动启动 VNC」计划任务。
// 该任务在用户登录交互式会话时自动运行 VNC server，确保系统重启后
// VNC 仍能正常工作（捕获交互式桌面），且不会落入 Session 0。
func ensureVncLogonTask(exe string) {
	taskName := "MoCenter-VNC-Logon"
	quotedExe := "\"" + strings.ReplaceAll(exe, "\"", "\"\"") + "\""

	// 先删除旧任务（避免重复）
	_ = exec.Command("cmd.exe", "/c", fmt.Sprintf("schtasks /delete /tn \"%s\" /f", taskName)).Run()

	// 创建用户登录时触发的交互式任务
	// /it 确保在交互式用户会话中运行（可捕获桌面）
	cmdStr := fmt.Sprintf(
		"schtasks /create /tn \"%s\" /tr %s /sc onlogon /it /f",
		taskName, quotedExe,
	)
	if err := exec.Command("cmd.exe", "/c", cmdStr).Run(); err != nil {
		log.Printf("[vnc-install] 创建登录自启任务失败: %v", err)
		return
	}
	log.Printf("[vnc-install] 已创建登录自启任务 %s", taskName)
}

// downloadFile 下载文件到目标路径（PowerShell Invoke-WebRequest）。
func downloadFile(url, out string) error {
	return downloadSlowAware(out, minVNCBps, func(ctx context.Context) error {
		ps := "Invoke-WebRequest -Uri " + psQuote(url) + " -OutFile " + psQuote(out) + " -UseBasicParsing"
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run()
	})
}

// downloadPlain 下载文件到目标路径，不带速率检测。
func downloadPlain(url, out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ps := "Invoke-WebRequest -Uri " + psQuote(url) + " -OutFile " + psQuote(out) + " -UseBasicParsing"
	if err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps).Run(); err != nil {
		return err
	}
	if fi, e := os.Stat(out); e == nil && fi.Size() > 0 {
		return nil
	}
	return fmt.Errorf("下载不完整（文件缺失或空）")
}

// is32BitWindows 判断当前系统是否为 32 位（用于选择对应架构的安装包）。
func is32BitWindows() bool {
	out, err := exec.Command("wmic", "os", "get", "OSArchitecture").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "32")
}

// tryInstall 尝试以命令行工具安装，成功返回 true。
func tryInstall(bin string, args []string) bool {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	_ = out
	return err == nil
}
