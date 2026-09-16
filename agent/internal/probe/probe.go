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

	"mocenter/agent/internal/ip"
	"mocenter/agent/internal/protocol"
)

// Collector 探针采集器。
type Collector struct {
	AgentID string
	host    string
	osType  string
	arch    string
	// 静态信息（首次采集后缓存）
	osVersion string
	cpuModel  string
	cpuMHz    float64
	uuid      string
	staticGot bool
	// 上次采样用于计算速率
	lastCPU  cpuTimes
	lastTime time.Time
	start    time.Time
	// 公网出口 IP 探测器
	ipDetector *ip.Detector
}

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// NewCollector 创建采集器。
func NewCollector(agentID string) *Collector {
	return NewCollectorWithProxy(agentID, "")
}

// NewCollectorWithProxy 创建采集器，proxy 为可选的国内加速代理前缀。
func NewCollectorWithProxy(agentID, proxy string) *Collector {
	return &Collector{
		AgentID:    agentID,
		host:       hostname(),
		osType:     runtime.GOOS,
		arch:       runtime.GOARCH,
		start:      time.Now(),
		ipDetector: ip.NewDetector(proxy),
	}
}

// PublicIPs 返回公网出口 IP（IPv4/IPv6）。
func (c *Collector) PublicIPs() ip.Result { return c.ipDetector.Resolve() }

// PublicIPv4 返回公网 IPv4。
func (c *Collector) PublicIPv4() string { return c.ipDetector.Resolve().IPv4 }

// Host 返回主机名。
func (c *Collector) Host() string { return c.host }

// OSType 返回操作系统类型。
func (c *Collector) OSType() string { return c.osType }

// Arch 返回 CPU 架构。
func (c *Collector) Arch() string { return c.arch }

// UUID 返回硬件唯一标识（首次调用后缓存）。获取失败返回空字符串。
func (c *Collector) UUID() string {
	if c.uuid == "" {
		c.uuid = HardwareUUID()
	}
	return c.uuid
}

// Sample 采集一次探针数据。
func (c *Collector) Sample() *protocol.ProbeDataReq {
	now := time.Now()
	ips := c.PublicIPs()
	c.ensureStatic()
	memTotal, memUsed := memDetails()
	diskTotal, diskUsed := diskDetails()
	p := &protocol.ProbeDataReq{
		AgentID:       c.AgentID,
		UUID:          c.UUID(),
		Timestamp:     now.Unix(),
		OSType:        c.osType,
		Arch:          c.arch,
		KernelVersion: kernelVersion(),
		Uptime:        int64(now.Sub(c.start).Seconds()),
		DiskUsage:     diskUsage(),
		PublicIP:      ips.IPv4,
		PublicIPv6:    ips.IPv6,
		// 扩展指标
		OSVersion:      c.osVersion,
		CPUModel:       c.cpuModel,
		CPUMHz:         c.cpuMHz,
		MemTotalBytes:  memTotal,
		MemUsedBytes:   memUsed,
		DiskTotalBytes: diskTotal,
		DiskUsedBytes:  diskUsed,
	}
	// 系统负载（1/5/15 分钟平均）。平台不提供时保持 nil，
	// 前端据此显示「—」而不是把 0 当成"空闲"（0 本身是合法负载）。
	if l1, l5, l15, ok := loadAvg(); ok {
		p.Load1, p.Load5, p.Load15 = &l1, &l5, &l15
	}
	// 内存使用率统一由明细计算（Linux /proc 与 Windows wmic 均适用）
	if memTotal > 0 {
		p.MemUsage = float64(memUsed) / float64(memTotal) * 100
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
		// 网卡自系统启动以来的累计收发字节
		p.NetInBytes, p.NetOutBytes = netTraffic()
	} else {
		p.CPUUsage = windowsCPU()
		p.NetInBytes, p.NetOutBytes = windowsNetTraffic()
	}
	c.lastTime = now
	return p
}

// OSVersion 返回操作系统版本描述（如 "Rocky Linux 8.10"/"Windows 10 Pro 22H2"）。
func (c *Collector) OSVersion() string {
	c.ensureStatic()
	return c.osVersion
}

// ensureStatic 首次调用时采集静态信息并缓存。
func (c *Collector) ensureStatic() {
	if c.staticGot {
		return
	}
	c.staticGot = true
	c.osVersion, c.cpuModel, c.cpuMHz = staticSysInfo()
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

func diskUsage() float64 {
	return diskUsageImpl()
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
