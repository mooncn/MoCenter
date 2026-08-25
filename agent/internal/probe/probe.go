// Package probe 采集主机运行状态探针数据（CPU/内存/磁盘/网络/系统信息）。
// Linux 直接读取 /proc，Windows 调用系统命令，保持零外部依赖。
package probe

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mocenter/agent/internal/protocol"
)

// Collector 探针采集器。
type Collector struct {
	AgentID string
	host    string
	osType  string
	arch    string
	// 上次采样用于计算速率
	lastCPU    cpuTimes
	lastNetIn  int64
	lastNetOut int64
	lastTime   time.Time
	start      time.Time
}

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// NewCollector 创建采集器。
func NewCollector(agentID string) *Collector {
	return &Collector{
		AgentID: agentID,
		host:    hostname(),
		osType:  runtime.GOOS,
		arch:    runtime.GOARCH,
		start:   time.Now(),
	}
}

// Host 返回主机名。
func (c *Collector) Host() string { return c.host }

// OSType 返回操作系统类型。
func (c *Collector) OSType() string { return c.osType }

// Arch 返回 CPU 架构。
func (c *Collector) Arch() string { return c.arch }

// Sample 采集一次探针数据。
func (c *Collector) Sample() *protocol.ProbeDataReq {
	now := time.Now()
	p := &protocol.ProbeDataReq{
		AgentID:       c.AgentID,
		Timestamp:     now.Unix(),
		OSType:        c.osType,
		Arch:          c.arch,
		KernelVersion: kernelVersion(),
		Uptime:        int64(now.Sub(c.start).Seconds()),
		MemUsage:      memUsage(),
		DiskUsage:     diskUsage(),
	}
	if c.osType == "linux" {
		cpu := readCPUTimes()
		if c.lastCPU.user > 0 {
			delta := cpuTotalDelta(cpu, c.lastCPU)
			if delta > 0 {
				p.CPUUsage = float64(cpuBusyDelta(cpu, c.lastCPU)) / float64(delta) * 100
			}
		}
		c.lastCPU = cpu
		in, out := netTraffic()
		if c.lastTime.IsZero() {
			p.NetInBytes = 0
			p.NetOutBytes = 0
		} else {
			p.NetInBytes = in - c.lastNetIn
			p.NetOutBytes = out - c.lastNetOut
		}
		c.lastNetIn, c.lastNetOut = in, out
	} else {
		p.CPUUsage = windowsCPU()
	}
	c.lastTime = now
	return p
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func kernelVersion() string {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/version")
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	out, _ := exec.Command("uname", "-a").Output()
	return strings.TrimSpace(string(out))
}

func memUsage() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	parse := func(k string) float64 {
		for _, l := range lines {
			if strings.HasPrefix(l, k) {
				f := strings.Fields(l)
				if len(f) >= 2 {
					v, _ := strconv.ParseFloat(f[1], 64)
					return v
				}
			}
		}
		return 0
	}
	total := parse("MemTotal:")
	free := parse("MemFree:")
	avail := parse("MemAvailable:")
	used := total - free
	if avail > 0 {
		used = total - avail
	}
	if total <= 0 {
		return 0
	}
	return used / total * 100
}

func diskUsage() float64 {
	return diskUsageProc()
}

// cpuTimes 结构用于存储 /proc/stat 数值
func readCPUTimes() cpuTimes {
	var t cpuTimes
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return t
	}
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "cpu ") {
			line = l
			break
		}
	}
	f := strings.Fields(line)
	if len(f) >= 8 {
		t.user, _ = strconv.ParseUint(f[1], 10, 64)
		t.nice, _ = strconv.ParseUint(f[2], 10, 64)
		t.system, _ = strconv.ParseUint(f[3], 10, 64)
		t.idle, _ = strconv.ParseUint(f[4], 10, 64)
		t.iowait, _ = strconv.ParseUint(f[5], 10, 64)
		t.irq, _ = strconv.ParseUint(f[6], 10, 64)
		t.softirq, _ = strconv.ParseUint(f[7], 10, 64)
	}
	return t
}

func cpuTotalDelta(cur, prev cpuTimes) uint64 {
	return (cur.user - prev.user) + (cur.nice - prev.nice) +
		(cur.system - prev.system) + (cur.idle - prev.idle) +
		(cur.iowait - prev.iowait) + (cur.irq - prev.irq) +
		(cur.softirq - prev.softirq)
}

func cpuBusyDelta(cur, prev cpuTimes) uint64 {
	return cpuTotalDelta(cur, prev) - (cur.idle - prev.idle)
}

func netTraffic() (int64, int64) {
	var rx, tx int64
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n")[2:] {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		f := strings.Fields(parts[1])
		if len(f) >= 9 {
			r, _ := strconv.ParseInt(f[0], 10, 64)
			t, _ := strconv.ParseInt(f[8], 10, 64)
			rx += r
			tx += t
		}
	}
	return rx, tx
}

func windowsCPU() float64 {
	out, err := exec.Command("wmic", "cpu", "get", "LoadPercentage").Output()
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if v, err := strconv.ParseFloat(l, 64); err == nil {
			return v
		}
	}
	return 0
}

func diskUsageProc() float64 {
	return 0 // 简化：跨平台磁盘使用率在 agent 启动时通过 du 命令探测
}
