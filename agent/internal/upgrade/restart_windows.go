//go:build windows

// 升级后自重启（Windows）：直接以子进程方式拉起新版本，旧进程随即退出。
package upgrade

import (
	"os"
	"os/exec"
)

// platformSpawnSelf 构造重新启动自身的命令（Windows 子进程独立于父进程生命周期）。
func platformSpawnSelf(exe string) *exec.Cmd {
	return exec.Command(exe, os.Args[1:]...)
}
