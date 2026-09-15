//go:build !windows

// 非 Windows 平台的桩实现：Linux/macOS 由 systemd/launchd 管理进程，
// 既没有 SCM，也不需要应用层单实例保护（服务管理器已保证）。
package winapp

import "errors"

// IsWindowsService 非 Windows 平台恒为 false。
func IsWindowsService() bool { return false }

// RunAsService 非 Windows 平台不支持，调用方不应走到这里。
func RunAsService(string, func(), func()) error {
	return errors.New("仅 Windows 支持以系统服务方式运行")
}

// AcquireSingleton 非 Windows 平台不启用单实例保护。
func AcquireSingleton(string) (func(), bool) { return func() {}, true }
