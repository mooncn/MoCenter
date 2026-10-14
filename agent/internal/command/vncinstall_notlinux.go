//go:build !linux

package command

// vncInstallLinux 在非 Linux 平台上不可用（由 linux 版本实现）。
func vncInstallLinux(port int, cfg VncConfig) VncInstallResult {
	return VncInstallResult{Port: port, Message: "当前平台非 Linux，请使用其他方式安装 VNC。"}
}
