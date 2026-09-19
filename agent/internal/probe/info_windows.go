//go:build windows

// Windows 静态系统信息与内存/磁盘/网络明细采集（通过 wmic）。
// 注意：wmic 写入管道的输出使用 OEM 代码页（中文系统为 GBK/CP936），
// 与控制台 chcp 无关，必须按 GBK 解码后才能得到正确 UTF-8 文本。
package probe

import (
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// wmicOut 执行 wmic 并将输出解码为 UTF-8。
// wmic 管道输出使用系统 ANSI 代码页：勾选"Beta: UTF-8"的系统输出 UTF-8，
// 中文系统默认输出 GBK；先验证 UTF-8，再回退 GBK 解码。
func wmicOut(args ...string) string {
	out, err := exec.Command("wmic", args...).Output()
	if err != nil {
		return ""
	}
	if !utf8.Valid(out) {
		if dec, derr := simplifiedchinese.GBK.NewDecoder().Bytes(out); derr == nil {
			return string(dec)
		}
	}
	return string(out)
}

// wmicValue 取 wmic 单列输出的首个数据行（跳过表头与空行）。
func wmicValue(args ...string) string {
	out := wmicOut(args...)
	for i, l := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if i == 0 { // 表头
			continue
		}
		if v := strings.TrimSpace(l); v != "" {
			return v
		}
	}
	return ""
}

// powershellValue 通过 PowerShell 执行表达式并返回去空白后的结果。
// 用于 wmic 不可用/被禁用（Win11 已默认移除 wmic）时的回退路径。
func powershellValue(script string) string {
	for _, exe := range []string{"powershell", "pwsh"} {
		cmd := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", script)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff")); v != "" {
			return v
		}
	}
	return ""
}

// wmicFields 取 wmic 多列输出的首个数据行字段（跳过表头与空行）。
func wmicFields(args ...string) []string {
	out := wmicOut(args...)
	for i, l := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if i == 0 {
			continue
		}
		if f := strings.Fields(l); len(f) > 0 {
			return f
		}
	}
	return nil
}

// HardwareUUID 返回硬件 UUID。
// 优先 wmic（csproduct get UUID）；wmic 缺失（Win11 默认已移除）或被禁用时，
// 回退到 PowerShell CIM 查询 (Get-CimInstance Win32_ComputerSystemProduct).UUID。
// 全部失败返回空字符串，调用方应回退到 hostname。
func HardwareUUID() string {
	if v := wmicValue("csproduct", "get", "UUID"); v != "" {
		// wmic 在部分虚拟机上返回全 0 占位符，视为无效
		if !isAllZeroUUID(v) {
			return v
		}
	}
	if v := powershellValue("(Get-CimInstance Win32_ComputerSystemProduct).UUID"); v != "" {
		if !isAllZeroUUID(v) {
			return v
		}
	}
	return ""
}

// isAllZeroUUID 判断形如 00000000-0000-0000-0000-000000000000 的占位 UUID。
func isAllZeroUUID(s string) bool {
	t := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), " ", "")
	if t == "" {
		return true
	}
	for _, r := range t {
		if r != '0' {
			return false
		}
	}
	return true
}

// staticSysInfo 返回 Windows 版本描述、CPU 型号与主频。
// 版本号优先取注册表 DisplayVersion（22H2 等），无则用 wmic Version。
func staticSysInfo() (osVer, cpuModel string, cpuMHz float64) {
	// 如 "Microsoft Windows 10 专业版" → "Windows 10 专业版"
	caption := strings.TrimPrefix(strings.TrimSpace(wmicValue("os", "get", "Caption")), "Microsoft ")
	if caption == "" {
		caption = "Windows"
	}
	ver := ""
	if b, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "/v", "DisplayVersion").Output(); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "REG_SZ"); i > 0 {
				dv := strings.TrimSpace(l[i+len("REG_SZ"):])
				if dv != "" {
					ver = dv
					break
				}
			}
		}
	}
	if ver == "" {
		ver = wmicValue("os", "get", "Version")
	}
	osVer = caption
	if ver != "" {
		osVer += " " + ver
	}
	// CPU 型号与主频分列查询，避免 wmic 按列名字典序输出导致的错位
	cpuModel = strings.Join(strings.Fields(wmicValue("cpu", "get", "Name")), " ")
	if s := wmicValue("cpu", "get", "MaxClockSpeed"); s != "" {
		cpuMHz, _ = strconv.ParseFloat(s, 64)
	}
	return osVer, cpuModel, cpuMHz
}

// memDetails 返回物理内存总量与已用量（字节）。
func memDetails() (total, used int64) {
	// 列名字典序：FreePhysicalMemory 在前，TotalVisibleMemorySize 在后（单位 kB）
	r := wmicFields("os", "get", "FreePhysicalMemory,TotalVisibleMemorySize")
	if len(r) < 2 {
		return 0, 0
	}
	freeKB, _ := strconv.ParseFloat(r[0], 64)
	totKB, _ := strconv.ParseFloat(r[1], 64)
	total = int64(totKB * 1024)
	used = int64((totKB - freeKB) * 1024)
	return total, used
}

// diskDetails 返回系统盘（首个固定磁盘）总量与已用量（字节）。
func diskDetails() (total, used int64) {
	// 列名字典序：FreeSpace 在前，Size 在后
	r := wmicFields("logicaldisk", "where", "DriveType=3", "get", "FreeSpace,Size")
	if len(r) < 2 {
		return 0, 0
	}
	freeB, _ := strconv.ParseInt(r[0], 10, 64)
	sizeB, _ := strconv.ParseInt(r[1], 10, 64)
	return sizeB, sizeB - freeB
}

// windowsNetTraffic 返回网卡累计收/发字节（RAW 性能计数器为开机累计值）。
func windowsNetTraffic() (recv, sent int64) {
	out := wmicOut("path", "Win32_PerfRawData_Tcpip_NetworkInterface",
		"get", "BytesReceivedPersec,BytesSentPersec")
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || !isDigits(f[0]) || !isDigits(f[1]) {
			continue
		}
		r, err1 := strconv.ParseInt(f[0], 10, 64)
		s, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 == nil && err2 == nil {
			recv += r
			sent += s
		}
	}
	return recv, sent
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// loadAvg 返回处理器队列长度的近似负载（Windows 没有 Unix 意义上的平均负载）。
//
// 口径说明：Win32_PerfFormattedData_PerfOS_System 的 ProcessorQueueLength 是
// "当前就绪队列长度"，不是 1/5/15 分钟滑动平均 —— 但它是 Windows 上最接近
// "负载"的指标，且 > 逻辑核数即表示 CPU 排队。因此三个值都填同一个数，
// 前端只展示 Load1，不会造成误导。查询不可用时返回 ok=false。
func loadAvg() (l1, l5, l15 float64, ok bool) {
	v := wmicValue("path", "Win32_PerfFormattedData_PerfOS_System", "get", "ProcessorQueueLength")
	if v == "" || !isDigits(strings.TrimSpace(v)) {
		return 0, 0, 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, 0, 0, false
	}
	return n, n, n, true
}
