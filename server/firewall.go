package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ensureFirewallPort 尝试在当前服务端操作系统上放通一个入站 TCP 端口。
//
// 返回值只用于提示：监听切换是主操作，防火墙可能由云安全组/上游设备管理，
// 自动放通失败不应把已经成功的监听切换回滚。已存在放通规则时直接跳过。
func ensureFirewallPort(port int, role string) string {
	if port < 1 || port > 65535 {
		return "端口无效，跳过防火墙配置"
	}
	switch runtime.GOOS {
	case "windows":
		return ensureWindowsFirewallPort(port, role)
	case "linux":
		return ensureLinuxFirewallPort(port)
	default:
		return "当前操作系统不支持自动放通防火墙，请手工配置"
	}
}

func firewallRuleName(role string, port int) string {
	return fmt.Sprintf("MoCenter %s TCP %d", role, port)
}

// ensureWindowsFirewallPort 优先用 PowerShell 按"端口"查询已有入站放行规则；
// 老系统没有 NetSecurity 模块时退回 netsh，并按固定规则名查重。
func ensureWindowsFirewallPort(port int, role string) string {
	ruleName := strings.ReplaceAll(firewallRuleName(role, port), "'", "''")
	script := fmt.Sprintf(`$p = %d
$name = '%s'
$existing = Get-NetFirewallPortFilter -LocalPort $p -ErrorAction SilentlyContinue |
  Get-NetFirewallRule -ErrorAction SilentlyContinue |
  Where-Object { $_.Direction -eq 'Inbound' -and $_.Action -eq 'Allow' -and $_.Enabled -ne 'False' }
if ($existing) { 'exists'; exit 0 }
try {
  New-NetFirewallRule -DisplayName $name -Direction Inbound -Protocol TCP -LocalPort $p -Action Allow -Profile Any -ErrorAction Stop | Out-Null
  'added'
} catch {
  'error:' + $_.Exception.Message
}`, port, ruleName)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script).CombinedOutput()
	if err == nil {
		text := strings.TrimSpace(string(out))
		switch {
		case strings.Contains(text, "exists"):
			return "防火墙已存在放通规则（Windows），跳过"
		case strings.Contains(text, "added"):
			return fmt.Sprintf("Windows 防火墙已放通 TCP %d", port)
		}
	}

	nameArg := "name=" + firewallRuleName(role, port)
	if exec.Command("netsh", "advfirewall", "firewall", "show", "rule", nameArg).Run() == nil {
		return "防火墙已存在放通规则（Windows），跳过"
	}
	out, err = exec.Command("netsh", "advfirewall", "firewall", "add", "rule", nameArg,
		"dir=in", "action=allow", "protocol=TCP", fmt.Sprintf("localport=%d", port)).CombinedOutput()
	if err != nil {
		return "Windows 防火墙放通失败: " + commandError(out, err)
	}
	return fmt.Sprintf("Windows 防火墙已放通 TCP %d（netsh）", port)
}

// ensureLinuxFirewallPort 只动 firewalld / ufw 这两套常见管理器；
// 都没有运行时不做猜测（直接改 iptables 可能与发行版规则链冲突）。
func ensureLinuxFirewallPort(port int) string {
	spec := fmt.Sprintf("%d/tcp", port)
	if path, err := exec.LookPath("firewall-cmd"); err == nil {
		if exec.Command(path, "--state").Run() == nil {
			if exec.Command(path, "--query-port", spec).Run() == nil {
				return "防火墙已存在放通规则（firewalld），跳过"
			}
			if out, err := exec.Command(path, "--permanent", "--add-port", spec).CombinedOutput(); err != nil {
				return "firewalld 放通失败: " + commandError(out, err)
			}
			if out, err := exec.Command(path, "--reload").CombinedOutput(); err != nil {
				return "firewalld 已写入规则但 reload 失败: " + commandError(out, err)
			}
			return fmt.Sprintf("firewalld 已放通 TCP %d", port)
		}
	}
	if path, err := exec.LookPath("ufw"); err == nil {
		out, err := exec.Command(path, "status").CombinedOutput()
		if err == nil && strings.Contains(string(out), "Status: active") {
			if ufwAllowsPort(string(out), port) {
				return "防火墙已存在放通规则（ufw），跳过"
			}
			if out, err := exec.Command(path, "allow", spec).CombinedOutput(); err != nil {
				return "ufw 放通失败: " + commandError(out, err)
			}
			return fmt.Sprintf("ufw 已放通 TCP %d", port)
		}
	}
	return "未检测到运行中的 firewalld/ufw，跳过防火墙配置（使用云安全组或其它防火墙时请手工放通）"
}

// ufwAllowsPort 从 `ufw status` 输出里判断端口是否已经有 ALLOW 规则。
func ufwAllowsPort(status string, port int) bool {
	needle := fmt.Sprintf("%d/tcp", port)
	for _, line := range strings.Split(status, "\n") {
		if strings.Contains(line, needle) && strings.Contains(strings.ToUpper(line), "ALLOW") {
			return true
		}
	}
	return false
}

func commandError(out []byte, err error) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return err.Error()
	}
	if i := strings.IndexByte(text, '\n'); i > 0 {
		text = text[:i]
	}
	return text
}
