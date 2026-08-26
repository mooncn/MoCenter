//go:build windows

// Windows VNC 一键安装/启动：
//   1. 优先 winget / choco 安装 TightVNC；
//   2. 否则通过 curl 下载 TightVNC MSI 静默安装（支持国内加速代理前缀）。
package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tightvncMSI 官方默认下载地址（可从 VncConfig.DownloadBase 覆盖）。
const defaultTightVNCURL = "https://www.tightvnc.com/download/2.8.85/tightvnc-2.8.85-gpl-setup-64bit.msi"

// vncInstallWindows 安装并启动 TightVNC。
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
	// 仍未安装：通过 curl 下载 MSI 静默安装（含国内加速代理）
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

	// 启动 TightVNC 服务并允许无密码本地查看（best-effort）
	_ = exec.Command("cmd.exe", "/c", "sc", "config", "tvnserver", "start=", "auto").Run()
	_ = exec.Command("cmd.exe", "/c", "tvnserver", "-start").Run()
	openVNCFirewall(port)
	if res.Message == "" {
		res.Message = "TightVNC 已安装并尝试启动（端口 " + itoa(port) + "）。若需免密访问请另行配置。"
	}
	if portOpen(port) {
		res.OK = true
		res.Message = "VNC(TightVNC) 已就绪，端口 " + itoa(port) + "。"
	}
	return res
}

// installTightVNCByCurl 通过 curl 下载 TightVNC MSI 并静默安装。
// 返回安装结果消息与是否成功。
func installTightVNCByCurl(cfg VncConfig) (string, bool) {
	url := cfg.DownloadBase
	if url == "" {
		url = defaultTightVNCURL
	}
	if cfg.Proxy != "" {
		url = proxyURL(cfg.Proxy, url)
	}
	// 判断 32/64 位，动态选择安装包
	if strings.Contains(url, "64bit") {
		// 默认 64bit 安装包；32 位系统走 32bit 包
		if is32BitWindows() {
			url = strings.ReplaceAll(url, "64bit", "32bit")
		}
	}
	tmp := filepath.Join(os.TempDir(), "tightvnc-setup.msi")
	// 下载（curl 自带于 Win10+）
	dl := exec.Command("curl.exe", "-fSL", "-o", tmp, url)
	if out, err := dl.CombinedOutput(); err != nil {
		return fmt.Sprintf("TightVNC 自动安装失败：curl 下载失败（%v）。可手动下载安装：%s\n%s", err, url, strings.TrimSpace(string(out))), false
	}
	// 静默安装（关闭新用户桌面，允许本地服务）
	inst := exec.Command("msiexec.exe", "/i", tmp, "/qn", "/norestart",
		"ADDLOCAL=Server", "VALUE_OF_USE_PASSWORDS=1")
	if out, err := inst.CombinedOutput(); err != nil {
		return fmt.Sprintf("TightVNC 自动安装失败：msiexec 执行错误（%v）。\n%s", err, strings.TrimSpace(string(out))), false
	}
	return "已通过 curl 下载并安装 TightVNC。", true
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

// openVNCFirewall 放行 VNC 端口（best-effort，需管理员）。
func openVNCFirewall(port int) {
	_ = exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=MoCenter VNC", "dir=in", "action=allow", "protocol=TCP", "localport="+itoa(port)).Run()
}
