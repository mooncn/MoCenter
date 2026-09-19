//go:build !windows

package main

import "errors"

// 非 Windows 平台的桩实现：Linux 的保活由 systemd timer / cron 负责
// （见 scripts/deploy/install-linux.sh 与 mocenter-keepalive.sh）。
func installKeepAlive(role, exe, confPath string) error {
	return errors.New("保活计划任务仅 Windows 支持；Linux 请用 scripts/deploy/install-linux.sh（systemd timer / cron）")
}

func uninstallKeepAlive(role string) error { return nil }

func taskExists(name string) bool { return false }

func runKeepAlive(role, exe, confPath string) error {
	return errors.New("保活计划任务仅 Windows 支持")
}

func serviceRunning(name string) bool { return false }
