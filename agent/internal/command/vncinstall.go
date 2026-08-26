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
	OK      bool
	Message string
	Port    int
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
	// 目标端口已在监听，认为 VNC 已就绪
	if portOpen(port) {
		res.OK = true
		res.Message = fmt.Sprintf("VNC 已在端口 %d 上监听，可直接连接。", port)
		return res
	}
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

func itoa(n int) string { return strconv.Itoa(n) }