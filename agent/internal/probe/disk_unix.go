//go:build linux || darwin

// 磁盘使用率采集（Linux/macOS）：基于 syscall.Statfs 计算根分区使用率。
package probe

import "syscall"

func diskUsageImpl() float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total <= 0 {
		return 0
	}
	used := total - free
	return float64(used) / float64(total) * 100
}
