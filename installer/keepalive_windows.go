//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// 保活（系统服务不可用时的兜底通道）。
//
// 场景：容器、精简版系统、组策略禁止注册服务、或服务被反复查杀的环境。
// 此时靠两个计划任务保证 Agent/Server 既能开机自启、又能在被干掉后自动回来：
//  1. **开机自启**：<角色>Boot，ONSTART（不可用时回退 ONLOGON）；
//  2. **看门狗**：<角色>KeepAlive，每 5 分钟巡检一次，进程不在就重新拉起 ——
//     等价于 systemd 的 Restart=always。
//
// 只用系统自带的 schtasks.exe：不引入额外依赖，也无需保存用户口令
// （任务以 SYSTEM 主体运行）。
//
// 任务命令行统一走 `mocenter-deploy.exe run` 包一层：schtasks /tr 直接启动
// 控制台程序会在用户会话里闪出黑窗口，经由本引擎创建 CREATE_NO_WINDOW
// 子进程即可完全隐藏。
const (
	keepAliveTaskSuffix = "KeepAlive" // <角色>KeepAlive：每 5 分钟巡检
	bootTaskSuffix      = "Boot"      // <角色>Boot：开机/登录自启
	keepAliveInterval   = "5"         // 巡检间隔（分钟）
)

// keepAliveTasks 返回某角色的两个任务名。
func keepAliveTasks(role string) (watchdog, boot string) {
	base := serviceNameOf(role)
	return base + keepAliveTaskSuffix, base + bootTaskSuffix
}

// taskAction 生成 schtasks /tr 的命令行（整串需再包一层引号）。
func taskAction(role, exe, confPath string) string {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = filepath.Join(filepath.Dir(exe), "mocenter-deploy.exe")
	}
	parts := []string{
		quoteTaskArg(self), "run",
		"-role", role,
		"-exe", quoteTaskArg(exe),
		"-config", quoteTaskArg(confPath),
	}
	return `"` + strings.Join(parts, " ") + `"`
}

// quoteTaskArg 给含空格的路径加引号（schtasks 会把整串 /tr 再解析一次，
// 因此内层引号需要转义成 \"）。
func quoteTaskArg(s string) string {
	if !strings.ContainsAny(s, " \t") {
		return s
	}
	return `\"` + s + `\"`
}

// installKeepAlive 注册开机自启 + 看门狗两个计划任务；任一失败都只告警，
// 不影响安装结果（安装是否成功以二进制/配置/服务为准）。
func installKeepAlive(role, exe, confPath string) error {
	watchdog, boot := keepAliveTasks(role)
	_ = uninstallKeepAlive(role) // 幂等：先清同名任务

	action := taskAction(role, exe, confPath)

	// 开机自启：ONSTART 在部分受限环境不可用，降级为「登录即启动」。
	// /rl HIGHEST 需要管理员安装（安装器已要求提权）。
	if err := schtasks("/Create", "/TN", boot, "/TR", action,
		"/SC", "ONSTART", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F"); err != nil {
		if err2 := schtasks("/Create", "/TN", boot, "/TR", action,
			"/SC", "ONLOGON", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F"); err2 != nil {
			return fmt.Errorf("注册开机自启任务失败: %v（回退 ONLOGON 亦失败: %v）", err, err2)
		}
	}
	if err := schtasks("/Create", "/TN", watchdog, "/TR", action,
		"/SC", "MINUTE", "/MO", keepAliveInterval, "/RU", "SYSTEM", "/RL", "HIGHEST", "/F"); err != nil {
		return fmt.Errorf("注册保活巡检任务失败: %v", err)
	}
	return nil
}

// uninstallKeepAlive 删除两个保活任务（不存在时静默返回）。
func uninstallKeepAlive(role string) error {
	watchdog, boot := keepAliveTasks(role)
	var firstErr error
	for _, t := range []string{watchdog, boot} {
		if err := schtasks("/Delete", "/TN", t, "/F"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// queryTask 查询计划任务是否存在（不需要管理员权限）。
// 用 taskExists 的语义：schtasks /Query 成功即存在。
func taskExists(name string) bool {
	return schtasks("/Query", "/TN", name) == nil
}

// schtasks 调用系统计划任务命令。
func schtasks(args ...string) error {
	cmd := exec.Command("schtasks.exe", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("schtasks %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

// runKeepAlive 看门狗入口（由计划任务每 5 分钟调用一次）。
//
// 行为：目标服务可用时什么都不做（服务自身的失败恢复已足够）；
// 服务不可用或未注册时，用同一份配置拉起进程。不重复拉起 ——
// Agent/Server 自带单实例保护，已有实例时新进程会立即自行退出。
func runKeepAlive(role, exe, confPath string) error {
	if serviceRunning(serviceNameOf(role)) {
		return nil // 服务在跑，交给 SCM
	}
	if fi, err := os.Stat(exe); err != nil || fi.IsDir() {
		return fmt.Errorf("未找到可执行文件 %s", exe)
	}
	cmd := exec.Command(exe, "-config", confPath)
	// 与计划任务一致：不创建控制台窗口，避免被拉起时闪黑框
	cmd.SysProcAttr = hiddenWindowAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("拉起 %s 失败: %v", exe, err)
	}
	// 不 Wait：拉起后立即退出，进程交由系统接管
	return nil
}

// serviceRunning 判断服务是否处于 Running 状态。
func serviceRunning(name string) bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return false
	}
	defer s.Close()
	st, err := s.Query()
	return err == nil && st.State == svc.Running
}
