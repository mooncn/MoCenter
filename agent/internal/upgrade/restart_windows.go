//go:build windows

// 升级后自重启（Windows）。
// 以「分离 + 无窗口」方式重新拉起新版本，并脱离父进程组与控制台，
// 避免父进程 os.Exit 时因继承控制台/进程组连带杀掉新进程。
package upgrade

import (
	"os"
	"os/exec"
	"syscall"
)

// DETACHED_PROCESS/CREATE_NEW_PROCESS_GROUP/CREATE_NO_WINDOW
const (
	detachedProcess      = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow       = 0x08000000
)

// platformSpawnSelf 构造重新启动自身的命令（Windows 子进程脱离父进程上下文）。
func platformSpawnSelf(exe string) *exec.Cmd {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
	}
	return cmd
}