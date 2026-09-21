// VNC 远程桌面：一键安装/启动 VNC 服务端的辅助实现。
//
// Windows 侧统一使用 UltraVNC（应用模式 / 服务模式，见 vncinstall_windows.go
// 与 vncinstall_ultravnc_windows.go）；Linux 尝试 apt/yum/dnf 安装
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
	Mode     string // 实际生效的模式：interactive / service
}

// VNC 安装模式（与服务端 protocol 常量保持一致）。
const (
	// VncModeInteractive 交互会话模式：VNC 运行在登录用户的交互会话中，
	// 仅在「已登录且未锁屏」时可见桌面。
	VncModeInteractive = "interactive"
	// VncModeService 服务模式：VNC 以系统服务运行，可抓取 Winlogon 安全桌面，
	// 锁屏/未登录状态下仍可见画面（Windows 下为 UltraVNC）。
	VncModeService = "service"
)

// VncConfig VNC 安装/启动配置。
type VncConfig struct {
	// Proxy 国内加速代理前缀（**只接受 https://**，如 https://mirror.example.com/?url= ），
	// 为空或不合规则不使用。
	Proxy string
	// DownloadBase 下载地址前缀（如官方或镜像），为空使用默认地址。
	DownloadBase string
	// Mode 安装模式：VncModeInteractive（默认）/ VncModeService。
	Mode string
	// FetchPackage 从服务端拉取托管的安装包，返回本地落盘路径（由 client 注入）。
	// 走 Agent 与服务端既有的长连接，避免公网下载慢或被劫持；这是**默认**取包方式。
	FetchPackage func(name string) (string, error)
	// AllowPublicDownload 运维显式解锁公网取包（winget / choco / 厂商直连 / 加速代理）。
	// 默认关闭：这些通道由不得我们选源与版本；执行前虽仍比对内置钉扎摘要，
	// 但"上游换版本 → 拒装并报错"才是可信默认，需要连通性时显式打开而不是默认兜底。
	AllowPublicDownload bool
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
