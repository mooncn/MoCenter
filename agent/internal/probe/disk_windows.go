//go:build windows

// 磁盘使用率采集（Windows）：调用 PowerShell 获取磁盘使用率。
package probe

import (
	"os/exec"
	"strconv"
	"strings"
)

func diskUsageImpl() float64 {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		"Get-CimInstance Win32_LogicalDisk -Filter \"DriveType=3\" | ForEach-Object { if($_.Size -gt 0){ [math]::Round(($_.Size-$_.FreeSpace)/$_.Size*100) } }").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return v
		}
	}
	return 0
}
