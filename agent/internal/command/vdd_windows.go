//go:build windows

// 虚拟显示驱动(Virtual Display Driver) 安装与配置。
//
// 背景：无头（无物理显示器）Windows 虚拟机无法被 VNC 捕获到任何画面。
// 通过在系统中安装开源虚拟显示驱动 VirtualDrivers/Virtual-Display-Driver，
// 创建一块虚拟显示器，VNC 在交互式用户会话中即可正常捕获桌面画面。
//
// 本项目选用 VirtualDrivers/Virtual-Display-Driver 的原因：
//   - 开源、可商用、无授权风险；
//   - 它实现的 IDD（Indirect Display Driver）在系统里表现为一块真正的显示器，
//     DXGI Desktop Duplication 能正常抓取到桌面内容（无头 VM 里物理显示器缺席时才需要它）。
//
// 安装流程：
//  1. 检测系统是否有活动显示器（无头检测）；
//  2. 若无显示器，取得 VDD 驱动包（服务端托管优先，公网下载需显式解锁）并校验内置摘要，
//     再安装驱动（pnputil /add-driver）；
//  3. 通过 PowerShell 创建/激活虚拟显示器；
//  4. 返回结果供 VNC 启动流程使用。
package command

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// vddReleaseURL VirtualDrivers/Virtual-Display-Driver 的 GitHub Release 直下地址。
// 包名（vddZipName）与内置摘要（installPins）定义在 vncinstall_common.go；
// 地址可用 VncConfig.DownloadBase 或 https 的 VncConfig.Proxy 覆盖（需显式解锁公网取包）。
const (
	vddReleaseBaseURL = "https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/25.7.23/"
	vddReleaseURL     = vddReleaseBaseURL + vddZipName
)

// hasActiveDisplay 检测系统是否已有活动显示器。
// 通过多种方式综合判断：
//  1. 检查是否有物理/虚拟监视器设备连接到桌面；
//  2. 排除仅存在显示适配器但无实际监视器的情况（如无头 VM）。
//
// 若判定为无头系统，后续流程会安装虚拟显示驱动。
func hasActiveDisplay() bool {
	// 方式1：通过 PowerShell 查询已连接的监视器设备（非适配器）
	// Win32_DesktopMonitor 只列出真正连接的监视器，不包括显示适配器
	psScript := `
$monitors = Get-CimInstance -ClassName Win32_DesktopMonitor -ErrorAction SilentlyContinue |
    Where-Object { $_.Status -eq 'OK' -and $_.ScreenWidth -gt 0 }
if ($monitors) { $monitors.Count } else { 0 }
`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript).Output()
	if err == nil {
		n := strings.TrimSpace(string(out))
		if n != "" && n != "0" {
			log.Printf("[vdd] 检测到 %s 个活动监视器设备", n)
			return true
		}
	}

	// 方式2：通过 QueryDisplayConfig 检查是否有活动的显示路径
	// 这比 EnumDisplayDevices 更准确，能区分"有适配器"和"有监视器"
	psScript2 := `
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public class DisplayConfig {
    [DllImport("user32.dll")]
    public static extern int GetSystemMetrics(int nIndex);
    // SM_CMONITORS = 80 (number of display monitors on desktop)
    public const int SM_CMONITORS = 80;
}
'@
[DisplayConfig]::GetSystemMetrics([DisplayConfig]::SM_CMONITORS)
`
	out2, err := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript2).Output()
	if err == nil {
		n := strings.TrimSpace(string(out2))
		if n != "" && n != "0" {
			log.Printf("[vdd] GetSystemMetrics(SM_CMONITORS)=%s，判定有活动显示器", n)
			return true
		}
	}

	// 方式3：检查注册表中是否有活动的显示配置
	out3, err := exec.Command("reg.exe", "query", `HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers\Configuration`, "/s").Output()
	if err == nil {
		content := string(out3)
		// 检查是否有 Scaling 值（表示有活动的显示配置）
		if strings.Contains(content, "Scaling") && !strings.Contains(content, "0x0") {
			log.Printf("[vdd] 注册表中检测到活动显示配置")
			return true
		}
	}

	log.Printf("[vdd] 未检测到活动显示器，判定为无头系统")
	return false
}

// installVirtualDisplayDriver 取得并安装 VirtualDrivers/Virtual-Display-Driver。
// 返回安装是否成功。若已安装则直接返回 true。
//
// 取包策略与 UltraVNC 完全一致：服务端托管优先、公网下载需本机显式解锁，
// 且在解压 / 交给 pnputil 之前必须先通过内置摘要校验 —— 解压出来的是**内核模式驱动**。
func installVirtualDisplayDriver(cfg VncConfig) bool {
	// 先检查是否已安装 Virtual Display Driver
	if isVDDInstalled() {
		log.Printf("[vdd] Virtual Display Driver 已安装，跳过下载")
		return true
	}

	extractDir := filepath.Join(os.TempDir(), "vdd-driver")
	// 先清空上一轮的解压结果：否则本轮一个包都没取到时，findDriverINF 仍会命中
	// 旧目录里的 INF，于是把一份**没经过本轮校验**的驱动交给 pnputil 加载。
	_ = os.RemoveAll(extractDir)

	zipPath, err := fetchVDDPackage(cfg)
	if err != nil {
		log.Printf("[vdd] 获取 VDD 驱动包失败: %v", err)
		return false
	}
	defer os.Remove(zipPath)

	if err := os.MkdirAll(extractDir, 0700); err != nil {
		log.Printf("[vdd] 创建解压目录失败: %v", err)
		return false
	}
	if err := extractZip(zipPath, extractDir); err != nil {
		log.Printf("[vdd] 解压 VDD 驱动失败: %v", err)
		return false
	}

	// 在解压目录中查找 .inf 驱动文件
	infFile := findDriverINF(extractDir)
	if infFile == "" {
		log.Printf("[vdd] 未找到 VDD 驱动 INF 文件")
		return false
	}

	// 使用 pnputil 安装驱动
	// pnputil /add-driver <inf> /install
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	installCmd := exec.CommandContext(ctx, "pnputil.exe", "/add-driver", infFile, "/install")
	out, err := installCmd.CombinedOutput()
	if err != nil {
		log.Printf("[vdd] pnputil 安装驱动失败: %v, 输出: %s", err, string(out))
		return false
	}
	log.Printf("[vdd] pnputil 安装驱动成功: %s", strings.TrimSpace(string(out)))

	// 等待驱动安装生效
	time.Sleep(3 * time.Second)

	// 验证驱动安装成功
	if !isVDDInstalled() {
		log.Printf("[vdd] 驱动已执行 pnputil 安装但未检测到 VDD 设备")
		// 可能需要重启才能生效，返回 false 让调用方决定
		return false
	}
	return true
}

// fetchVDDPackage 取到一份「已通过内置摘要校验」的 VDD 驱动包，返回其落盘路径。
// 调用方负责在使用后删除该文件。
func fetchVDDPackage(cfg VncConfig) (string, error) {
	want := pinnedDigest(vddZipName)
	if want == "" {
		return "", fmt.Errorf("缺少 %s 的内置钉扎摘要，已拒绝加载（请同步 installPins 后重新发布 Agent）", vddZipName)
	}
	// 1) 服务端托管：走 Agent↔服务端已 mutual-TLS 的长连接
	if cfg.FetchPackage != nil {
		path, err := cfg.FetchPackage(vddZipName)
		if err != nil {
			log.Printf("[vdd] 服务端未托管 %s（%v）", vddZipName, err)
		} else {
			if verr := verifyDigest(path, want, vddZipName); verr != nil {
				_ = os.Remove(path)
				return "", verr
			}
			return path, nil
		}
	}
	// 2) 公网取包需本机显式解锁
	if !cfg.AllowPublicDownload {
		return "", fmt.Errorf("服务端未托管 %s%s。准备托管包：bash scripts/fetch-packages.sh <服务端 packages 目录> win",
			vddZipName, publicDownloadHint(cfg))
	}
	var candidates []string
	if cfg.DownloadBase != "" && strings.Contains(cfg.DownloadBase, "VirtualDisplayDriver") {
		candidates = append(candidates, cfg.DownloadBase)
	}
	if p := httpsProxyOrEmpty(cfg.Proxy, "VDD 驱动包下载"); p != "" {
		candidates = append(candidates, proxyURL(p, vddReleaseURL))
	}
	candidates = append(candidates, vddReleaseURL)
	seen := map[string]bool{}
	var urls []string
	for _, u := range candidates {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	var lastErr error
	for _, u := range urls {
		tmp := filepath.Join(os.TempDir(), vddZipName)
		log.Printf("[vdd] 尝试下载 Virtual Display Driver: %s", u)
		if err := downloadPlain(u, tmp); err != nil {
			lastErr = err
			_ = os.Remove(tmp)
			continue
		}
		if fi, e := os.Stat(tmp); e != nil || fi.Size() < 1024 {
			lastErr = fmt.Errorf("下载文件无效或过小")
			_ = os.Remove(tmp)
			continue
		}
		// 摘要不符时继续试下一个源（某源失效不该阻断），但绝不解压未通过的包
		if err := verifyDigest(tmp, want, vddZipName); err != nil {
			lastErr = err
			_ = os.Remove(tmp)
			continue
		}
		return tmp, nil
	}
	return "", fmt.Errorf("未能取到与内置钉扎一致的 VDD 驱动包: %v", lastErr)
}

// isVDDInstalled 检查 Virtual Display Driver 是否已安装。
// 通过 PnP 设备枚举判断。
func isVDDInstalled() bool {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		`(Get-PnpDevice -Class Display | Where-Object {$_.FriendlyName -match 'Virtual|VDD' -and $_.Status -eq 'OK'} | Measure-Object).Count`).Output()
	if err == nil {
		n := strings.TrimSpace(string(out))
		if n != "" && n != "0" {
			return true
		}
	}
	// 另一种方式：检查注册表中是否有 VDD 设备
	out2, err := exec.Command("reg.exe", "query", `HKLM\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`, "/f", "Virtual", "/s").Output()
	if err == nil && strings.Contains(strings.ToLower(string(out2)), "virtual") {
		return true
	}
	return false
}

// createVirtualDisplay 创建/激活一块虚拟显示器。
// 驱动安装后，虚拟显示器通常会自动出现在系统中。
// 此函数主要做验证和必要的设备激活。
func createVirtualDisplay() (bool, string) {
	// 先检查是否已有虚拟显示器
	if virtualDisplayCount() > 0 {
		log.Printf("[vdd] 已存在虚拟显示器，跳过创建")
		return true, "虚拟显示器已就绪"
	}

	// 扫描硬件更改，触发虚拟显示器自动创建
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "pnputil.exe", "/scan-devices").Run()
	time.Sleep(2 * time.Second)

	// 再次检查
	if virtualDisplayCount() > 0 {
		return true, "虚拟显示器已创建"
	}

	// 尝试通过 PowerShell 启用显示设备
	psScript := `
$devs = Get-PnpDevice -Class Display -Status Error -ErrorAction SilentlyContinue
foreach ($d in $devs) {
    Enable-PnpDevice -InstanceId $d.InstanceId -Confirm:$false -ErrorAction SilentlyContinue
}
Start-Sleep -Seconds 1
pnputil /scan-devices
Start-Sleep -Seconds 2
(Get-PnpDevice -Class Display -Status OK | Measure-Object).Count
`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript).Output()
	if err != nil {
		return false, "启用虚拟显示设备失败: " + err.Error()
	}
	n := strings.TrimSpace(string(out))
	if n != "" && n != "0" {
		log.Printf("[vdd] 虚拟显示设备已激活，设备数: %s", n)
		return true, "虚拟显示器已激活"
	}

	// 如果驱动已装但无显示设备，尝试从 INF 安装显示输出
	// 这是 VirtualDrivers 驱动特有的步骤
	psScript2 := `
$vddRoot = "C:\Windows\System32\drivers"
$devConPath = Join-Path $env:TEMP "devcon.exe"
# 如果存在 devcon 工具则使用它创建设备
if (Test-Path $devConPath) {
    & $devConPath findall *VIDD*
}
# 直接检查 VDD 相关设备
Get-PnpDevice -Class Display | Format-List FriendlyName, InstanceId, Status
`
	out2, _ := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript2).Output()
	log.Printf("[vdd] 显示设备枚举: %s", strings.TrimSpace(string(out2)))

	return false, "虚拟显示驱动已安装但未检测到活动显示输出"
}

// virtualDisplayCount 统计当前虚拟显示器数量。
func virtualDisplayCount() int {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		`(Get-PnpDevice -Class Display | Where-Object {$_.FriendlyName -match 'Virtual|VDD' -and $_.Status -eq 'OK'} | Measure-Object).Count`).Output()
	if err == nil {
		n := strings.TrimSpace(string(out))
		if n != "" {
			var cnt int
			if _, err := fmt.Sscanf(n, "%d", &cnt); err == nil {
				return cnt
			}
		}
	}
	return 0
}

// ensureVirtualDisplay 确保系统有可用的显示设备。
// 若无头（无物理显示器），则尝试安装虚拟显示驱动并创建虚拟显示器。
// 返回结果消息和是否成功。
func ensureVirtualDisplay(cfg VncConfig) (bool, string) {
	// 1. 检查是否已有活动显示器（物理或已配置的虚拟）
	if hasActiveDisplay() {
		log.Printf("[vdd] 系统已有活动显示器，无需安装虚拟显示驱动")
		return true, "系统已有活动显示器"
	}

	// 2. 尝试安装虚拟显示驱动
	log.Printf("[vdd] 检测到无头系统，开始安装虚拟显示驱动...")
	if !installVirtualDisplayDriver(cfg) {
		msg := "无头系统且虚拟显示驱动安装失败。请连接物理显示器或在 VM 设置中添加虚拟显示器后重试。"
		log.Printf("[vdd] %s", msg)
		return false, msg
	}

	// 3. 创建/激活虚拟显示器
	ok, msg := createVirtualDisplay()
	if !ok {
		log.Printf("[vdd] 创建虚拟显示器失败: %s", msg)
		return false, "虚拟显示驱动已安装但无法创建虚拟显示器: " + msg
	}
	log.Printf("[vdd] 虚拟显示器就绪: %s", msg)
	return true, "已创建虚拟显示器，VNC 可正常捕获画面"
}

// findDriverINF 在解压目录中查找 .inf 驱动文件。
func findDriverINF(dir string) string {
	if dir == "" {
		return ""
	}
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".inf") {
			// 优先选择主驱动 INF（排除无关的系统 INF）
			name := strings.ToLower(info.Name())
			if strings.Contains(name, "virtual") || strings.Contains(name, "vdd") ||
				strings.Contains(name, "idd") || strings.Contains(name, "display") {
				found = path
				return filepath.SkipAll
			}
			if found == "" {
				found = path
			}
		}
		return nil
	})
	return found
}

// extractZip 使用 PowerShell 的 Expand-Archive 解压 ZIP 文件。
func extractZip(zipPath, dest string) error {
	psCmd := "Expand-Archive -Path " + psQuote(zipPath) + " -DestinationPath " + psQuote(dest) + " -Force"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psCmd).Run()
}
