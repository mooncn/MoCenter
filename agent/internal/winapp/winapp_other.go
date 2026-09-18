//go:build !windows

// 非 Windows 平台：Linux/macOS 由 systemd/launchd 管理进程形态，
// 但**单实例保护同样必要** —— 定时任务保活、手工重复启动都会拉起重复实例。
// 实现见 singleton_unix.go（flock，随进程退出自动释放，无陈旧锁问题）。
package winapp

import "errors"

// IsWindowsService 非 Windows 平台恒为 false。
func IsWindowsService() bool { return false }

// RunAsService 非 Windows 平台不支持，调用方不应走到这里。
func RunAsService(string, func(), func()) error {
	return errors.New("仅 Windows 支持以系统服务方式运行")
}
