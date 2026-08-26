//go:build windows

// Windows VNC 一键安装/启动：优先 winget/choco 安装 TightVNC，兜底 PowerShell 下载 MSI。
package command

import (
	"os"
	"os/exec"
)

func vncInstallWindows(port int) VncInstallResult {
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
	if !installed {
		res.Message = "未能通过 winget/choco 自动安装 TightVNC，请以管理员运行安装包或使用：‘一键安装’仅在目标机器具备包管理工具/联网时有效。"
		return res
	}

	// 启动 TightVNC 服务并允许无密码本地查看（best-effort）
	_ = exec.Command("cmd.exe", "/c", "sc", "config", "tvnserver", "start=", "auto").Run()
	_ = exec.Command("cmd.exe", "/c", "tvnserver", "-start").Run()
	openVNCFirewall(port)
	res.Message = "TightVNC 已安装并尝试启动（端口 " + itoa(port) + "）。若需免密访问请另行配置。"
	if portOpen(port) {
		res.OK = true
		res.Message = "VNC(TightVNC) 已就绪，端口 " + itoa(port) + "。"
	}
	return res
}

// probeWinVNC 探测是否已安装 TightVNC/TigerVNC。
func probeWinVNC() bool {
	for _, bin := range []string{"tvnserver.exe", "winvnc.exe", "vncserver.exe"} {
		p, err := exec.LookPath(bin)
		if err == nil && p != "" {
			return true
		}
		if _, err := os.Stat(`C:\\Program Files\\TightVNC\\tvnserver.exe`); err == nil {
			return true
		}
		if _, err := os.Stat(`C:\\Program Files (x86)\\TightVNC\\tvnserver.exe`); err == nil {
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

// openVNCFirewall 放行 VNC 端口（best-effort，需管理员）。
func openVNCFirewall(port int) {
	_ = exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=MoCenter VNC", "dir=in", "action=allow", "protocol=TCP", "localport="+itoa(port)).Run()
}