//go:build !windows

// 升级后自重启（Unix）：setsid 分离为新会话，避免随旧进程退出。
package upgrade

import (
	"os"
	"os/exec"
	"syscall"
)

// platformSpawnSelf 构造以分离会话方式重新启动自身的命令。
func platformSpawnSelf(exe string) *exec.Cmd {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
