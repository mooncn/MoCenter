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

// VncInstall 安装并按需启动 VNC 服务，返回结果。port<=0 时默认 5900。
func VncInstall(port int) VncInstallResult {
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
		return vncInstallWindows(port)
	case "linux":
		return vncInstallLinux(port)
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