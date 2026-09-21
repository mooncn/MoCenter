//go:build !windows

// 升级后自重启（Unix）：就地替换进程镜像。
package upgrade

import (
	"os"
	"syscall"
)

// platformRelaunch 用新二进制替换当前进程镜像（PID 不变）。
//
// 不用"fork 子进程 + 老进程退出"：那样会在 unit 之外留下一份进程，systemd 按
// Restart=always 重启出的第二份又被单实例检测挡掉，结果是"Agent 在跑但服务一直
// 起不来"；exec 原地替换则 MainPID 始终是同一个，监督关系与 stdin/journal 都不中断。
func platformRelaunch(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
