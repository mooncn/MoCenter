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

// deployExe 返回本引擎（计划任务实际调起的可执行文件）的绝对路径。
// 取不到时按安装目录推导 —— 与 taskAction 的旧行为保持一致。
func deployExe(exe string) string {
	self, err := os.Executable()
	if err != nil || self == "" {
		return filepath.Join(filepath.Dir(exe), "mocenter-deploy.exe")
	}
	return self
}

// taskAction 生成 schtasks /tr 的命令行（整串需再包一层引号）。
func taskAction(role, exe, confPath string) string {
	parts := []string{
		quoteTaskArg(deployExe(exe)), "run",
		"-role", role,
		"-exe", quoteTaskArg(exe),
		"-config", quoteTaskArg(confPath),
	}
	return `"` + strings.Join(parts, " ") + `"`
}

// checkSystemTaskPaths 校验「将以 SYSTEM 身份被重跑」的那几个路径的可写性。
//
// 为什么这是硬门槛而不是提示：保活任务用 /RU SYSTEM /RL HIGHEST 注册，且跑的是
// 用户在安装向导里自选目录下的二进制。只要那个二进制、本引擎、或它读取的配置
// 能被普通用户改写，任意本地用户改一次文件就能在下一个巡检周期（≤5 分钟）拿到
// SYSTEM —— 配置可写同样致命，Agent 以 SYSTEM 跑时会照配置里的 server 地址回连
// 并执行远端下发的命令。
//
// 查不到 ACL 也按不放行处理：判不出来就不该把 SYSTEM 交出去。
func checkSystemTaskPaths(paths ...string) error {
	var unsafe []string
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		risk, err := nonAdminWritable(p)
		if err != nil {
			return fmt.Errorf("无法判定 %s 的访问权限，拒绝注册 SYSTEM 计划任务: %w", p, err)
		}
		if risk != "" {
			unsafe = append(unsafe, risk)
		}
	}
	if len(unsafe) > 0 {
		return fmt.Errorf("以下路径可被非管理员改写，注册 SYSTEM 保活等于本机提权通道，已拒绝注册：\n    - %s",
			strings.Join(unsafe, "\n    - "))
	}
	return nil
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

	// 注册前先过可写性闸门（见 checkSystemTaskPaths）。
	// 不通过时连旧任务也不动：卸载由调用方决定，这里只负责"不新开提权面"。
	if err := checkSystemTaskPaths(exe, deployExe(exe), confPath); err != nil {
		return err
	}
	_ = uninstallKeepAlive(role) // 幂等：清同名任务

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
//
// 拉起前重做一次可写性判定：注册任务那一刻 ACL 是紧的，但之后可能被别的软件放宽
// （例如有人把安装目录挪到公共可写位置、或在同级目录装了共享配置的工具）。
// 判定不通过就宁可不保活：漏一次自启是可恢复故障，被劫持是提权事故。
func runKeepAlive(role, exe, confPath string) error {
	if serviceRunning(serviceNameOf(role)) {
		return nil // 服务在跑，交给 SCM
	}
	if fi, err := os.Stat(exe); err != nil || fi.IsDir() {
		return fmt.Errorf("未找到可执行文件 %s", exe)
	}
	if err := checkSystemTaskPaths(exe, confPath); err != nil {
		return err
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
