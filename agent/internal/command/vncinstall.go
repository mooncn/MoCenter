// VNC 远程桌面：一键安装/启动 VNC 服务端的辅助实现。
//
// Windows 优先尝试 winget/choco 安装 TightVNC；Linux 尝试 apt/yum/dnf 安装
// x11vnc 并启动（无密码，绑定 rfbport）。安装结果以消息形式回传 Web 端。
package command

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"time"
)

// VncInstallResult VNC 安装/启动结果。
type VncInstallResult struct {
	OK       bool
	Message  string
	Port     int
	Password string // 生成的 VNC 口令（供 Web 端本次会话连接使用）
}

// VncConfig VNC 安装/启动配置。
type VncConfig struct {
	// Proxy 国内加速代理前缀（如 http://fa.mooncn.win/?url= ），为空则不使用。
	Proxy string
	// DownloadBase 下载地址前缀（如官方或镜像），为空使用默认地址。
	DownloadBase string
}

// VncInstall 安装并按需启动 VNC 服务，返回结果。port<=0 时默认 5900。
func VncInstall(port int) VncInstallResult {
	return VncInstallCfg(port, VncConfig{})
}

// VncInstallCfg 带配置安装 VNC，支持国内加速代理与自定义下载地址。
func VncInstallCfg(port int, cfg VncConfig) VncInstallResult {
	if port <= 0 {
		port = 5900
	}
	res := VncInstallResult{Port: port}
	// 注意：不在此处因“端口已监听”短路返回——即使 VNC 已在运行，仍会重新生成
	// 并回写口令，从而始终能给前端提供有效的本次口令（实现口令兜底与轮换）。
	switch runtime.GOOS {
	case "windows":
		return vncInstallWindows(port, cfg)
	case "linux":
		return vncInstallLinux(port, cfg)
	default:
		res.Message = "当前平台暂不支持一键安装，请手动安装 VNC 服务端。"
		return res
	}
}

// portOpen 探测本机端口是否已监听。
func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err == nil {
		_ = c.Close()
		return true
	}
	return false
}

// waitPortOpen 轮询等待端口监听，成功返回 true（各平台共用）。
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

func itoa(n int) string { return strconv.Itoa(n) }