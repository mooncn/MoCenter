//go:build !windows

package command

// vncInstallWindows 在非 Windows 平台上不可用（由 windows 版本实现）。
func vncInstallWindows(port int) VncInstallResult {
	return VncInstallResult{Port: port, Message: "当前平台非 Windows，请使用 Linux/其他方式安装 VNC。"}
}