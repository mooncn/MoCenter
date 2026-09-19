//go:build linux || darwin

// Linux/macOS 静态系统信息与内存/磁盘明细采集（零外部依赖，直接读 /proc 等）。
package probe

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// HardwareUUID 返回硬件 UUID。
// 优先读 /sys/class/dmi/id/product_uuid（需要 root 权限），
// 失败时回退执行 dmidecode -s system-uuid。
// 全部失败返回空字符串，调用方应回退到 hostname。
func HardwareUUID() string {
	if b, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	out, err := exec.Command("dmidecode", "-s", "system-uuid").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// staticSysInfo 返回操作系统版本描述、CPU 型号与主频。
// staticSysInfo 返回操作系统版本描述、CPU 型号与主频。
func staticSysInfo() (osVer, cpuModel string, cpuMHz float64) {
	// 系统版本：/etc/os-release 的 PRETTY_NAME（如 "Rocky Linux 8.10 (Green Obsidian)"）
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				osVer = strings.Trim(strings.TrimSpace(v), `"`)
				break
			}
		}
	}
	if osVer == "" {
		osVer = "Linux"
	}
	// CPU 型号与主频：/proc/cpuinfo
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if cpuModel == "" {
				if v, ok := strings.CutPrefix(l, "model name"); ok {
					cpuModel = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":"))
					continue
				}
			}
			if cpuMHz == 0 {
				if v, ok := strings.CutPrefix(l, "cpu MHz"); ok {
					f, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":")), 64)
					cpuMHz = f
				}
			}
		}
	}
	return osVer, cpuModel, cpuMHz
}

// memDetails 返回内存总量与已用量（字节）。已用 = Total - Available。
func memDetails() (total, used int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var t, avail, free float64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64) // 单位 kB
		switch {
		case strings.HasPrefix(l, "MemTotal:"):
			t = v
		case strings.HasPrefix(l, "MemAvailable:"):
			avail = v
		case strings.HasPrefix(l, "MemFree:"):
			free = v
		}
	}
	if avail <= 0 {
		avail = free
	}
	total = int64(t * 1024)
	used = int64((t - avail) * 1024)
	return total, used
}

// diskDetails 返回根分区总量与已用量（字节）。
func diskDetails() (total, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, 0
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	used = total - int64(st.Bavail)*int64(st.Bsize)
	return total, used
}

// windowsNetTraffic Unix 平台占位（不会被执行）。
func windowsNetTraffic() (recv, sent int64) { return 0, 0 }

// loadAvg 返回系统 1/5/15 分钟平均负载。
//
// Linux 直接读 /proc/loadavg（第一个字段起为三个数值，Win 无关）；
// macOS/BSD 没有 /proc，用 sysctl -n vm.loadavg 兜底（输出形如 "{ 1.2 1.1 0.9 }"）。
// 两者都拿不到时返回 ok=false —— 调用方据此标记"本平台不提供负载"。
func loadAvg() (l1, l5, l15 float64, ok bool) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			a, e1 := strconv.ParseFloat(f[0], 64)
			b2, e2 := strconv.ParseFloat(f[1], 64)
			c, e3 := strconv.ParseFloat(f[2], 64)
			if e1 == nil && e2 == nil && e3 == nil {
				return a, b2, c, true
			}
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	a, e1 := strconv.ParseFloat(f[0], 64)
	b2, e2 := strconv.ParseFloat(f[1], 64)
	c, e3 := strconv.ParseFloat(f[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return a, b2, c, true
}
