//go:build !windows

package winapp

import (
	"encoding/json"
	"fmt"
	"mocenter/server/internal/logx"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 本文件为 Linux / macOS / *BSD 提供与 Windows 命名互斥体等价的能力：
// 「同一份配置只允许一个实例」。
//
// 为什么必须有（而不是"交给 systemd 管就行"）：
//   - 用 cron / 定时任务保活时，脚本"看到进程不在就拉一个"；
//   - 有人在机器上手工再执行一次二进制（排查问题时最常见的动作）；
//   - 服务单元报失败但进程其实还活着（supervisor / nohup / daemon 模式混用）。
//
// 这些情况会同时跑起多个 Agent：它们共用同一配置（同一 creds、同一 AgentID），
// 一起连服务端互相顶号，隧道 / VNC / 端口映射的会话状态错乱；
// 定时任务每 5 分钟拉一个，几天下来就是几十个进程。
//
// 实现取舍：**flock(2) 而不是 PID 文件比对**。
//   - flock 由内核在进程退出时自动释放（含被 SIGKILL、机器重启），
//     不存在"进程已死而锁还在"的陈旧锁；PID 文件方案要额外处理 PID 复用与残留文件，
//     而两种误判都很危险（宁可不锁 / 误杀），见下面"绝不杀进程"的说明。
//   - flock 在多进程同时抢锁时由内核串行化，不存在"两个进程都读到空文件、
//     都认为自己该持锁"的竞态。
//
// 锁文件里写入持有者信息（PID / 可执行文件 / 配置 / 启动时间）**仅用于日志排查**，
// 不参与任何判断：判断只认 flock 本身。

const (
	// lockFileSuffix 锁文件后缀。
	lockFileSuffix = ".lock"
	// staleLockAge 锁文件"看起来陈旧"的时长阈值。
	// 持锁期间时间戳会被周期刷新（见 touchLoop），因此长时间未刷新的锁文件
	// 可判定是**上次异常退出（例如断电）留下的** —— 此时 flock 已由内核释放，
	// 我们已拿到锁，只是文件内容过期，重写即可。仅用于日志提示。
	staleLockAge = 10 * time.Minute
	// touchInterval 锁文件时间戳刷新周期。
	touchInterval = time.Minute
)

// lockOwner 锁文件内容。仅供人排查问题，不参与持锁判断。
type lockOwner struct {
	PID       int    `json:"pid"`
	Exe       string `json:"exe"`
	Config    string `json:"config"`
	StartedAt string `json:"started_at"`
	Hostname  string `json:"hostname,omitempty"`
	BootID    string `json:"boot_id,omitempty"`
}

// AcquireSingleton 以 flock 独占锁保证「同一 key 在本机只允许一个实例」。
//
// configPath 为配置文件路径：锁文件与之同目录（见 lockDir 的说明），
// 因此"谁能用这份配置"与"谁能建锁"天然一致，不会出现多用户机器上
// root 抢占锁目录导致普通用户永远降级放行的漏洞。
//
// 返回 (release, ok)：ok 为 false 表示已有实例在运行，调用方应**直接退出**；
// release 用于提前释放（正常路径由 defer 调用，且可重复调用）。
//
// key 沿用 Windows 侧格式（MoCenter-<role>-<配置路径哈希>，见 winapp.go 的
// SingletonKey）：只要两边 key 相同，语义就完全一致 ——
// 不同配置互不影响，同一配置严格互斥。
func AcquireSingleton(key, configPath string) (func(), bool) {
	path := lockFilePath(key, configPath)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		// 打不开锁文件（只读文件系统、目录属主不对、SELinux 等）。
		// 此时**不阻塞启动** —— 宁可多跑一个实例，也不要让 Agent 完全起不来。
		// 但必须留下明确告警：这类"保护静默失效"正是重复实例最难排查的形态。
		logx.Warnf("[单实例] 无法打开锁文件 %s: %v —— 本次启动**未启用**单实例保护。"+
			"请检查该路径是否可写（可用 -config 指向可写目录，如 /var/lib/mocenter）", path, err)
		return func() {}, true
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// 拿不到锁 = 已有实例在跑。**绝不杀进程**：
		// 判断本身可能出错（容器 PID 命名空间、其它用户、PID 复用），
		// 而误杀一个正在跑远控会话的 Agent 后果严重。这里只退出并打印持有者信息，
		// 由运维决定怎么处理。
		owner := readLockOwner(path)
		_ = f.Close()
		if owner != nil {
			logx.Infof("[单实例] 已有实例正在运行: PID %d%s（可执行文件 %s，配置 %s，启动于 %s）",
				owner.PID, lockOwnerNote(owner), owner.Exe, owner.Config, owner.StartedAt)
			logx.Infof("[单实例] 本进程不会去终止对方。若确认系统里确实没有实例在运行"+
				"（例如上次断电留下的陈迹），可删除锁文件后重试: %s", path)
		} else {
			logx.Infof("[单实例] 已有实例正在运行（未能读取锁文件 %s 的持有者信息）", path)
		}
		return nil, false
	}

	writeLockOwner(f, configPath, path)
	stop := make(chan struct{})
	go touchLoop(f, path, stop)

	released := false
	release := func() {
		// 幂等：`defer release()` 与调用方显式释放可能同时发生，
		// 重复 close(chan) / 重复解锁会 panic。
		if released {
			return
		}
		released = true
		close(stop)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, true
}

// touchLoop 周期刷新锁文件时间戳。
//
// 目的只有一个：让"超过 staleLockAge 未被刷新"成为**异常退出**的可靠信号，
// 从而让日志能提示"这是上次断电留下的陈迹"。
// 顺带在文件被外部误删时重建 —— 否则后续 flock 落在已被 unlink 的 inode 上，
// 表现就是"锁莫名其妙失效"（又出现重复实例）。
func touchLoop(f *os.File, path string, stop <-chan struct{}) {
	t := time.NewTicker(touchInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			if _, err := os.Stat(path); err != nil {
				if nf, oerr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600); oerr == nil {
					_ = nf.Close()
				}
				continue
			}
			_ = os.Chtimes(path, now, now)
		}
	}
}

// writeLockOwner 写入持有者信息（尽力而为：写失败不影响持锁）。
func writeLockOwner(f *os.File, configPath, path string) {
	host, _ := os.Hostname()
	exe, _ := os.Executable()
	o := lockOwner{
		PID:       os.Getpid(),
		Exe:       exe,
		Config:    configPath,
		StartedAt: time.Now().Format(time.RFC3339),
		Hostname:  host,
		BootID:    bootID(),
	}
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	if _, err := f.Seek(0, 0); err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return
	}
	_ = f.Sync()
	_ = os.Chtimes(path, time.Now(), time.Now())
}

// readLockOwner 读取锁文件里的持有者信息；读不到/解析失败返回 nil。
func readLockOwner(path string) *lockOwner {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// 陈旧锁提示：文件长时间未被刷新，说明上次是异常退出（断电等）。
	// 只在日志里说明，不影响持锁判断。
	if fi, serr := os.Stat(path); serr == nil {
		if age := time.Since(fi.ModTime()); age > staleLockAge {
			logx.Infof("[单实例] 锁文件 %s 已 %s 未被刷新，可能是**上次异常退出**（如断电）"+
				"遗留；若本机确实没有实例在运行，可删除该文件后重试", path, age.Truncate(time.Second))
		}
	}
	var o lockOwner
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &o); err != nil {
		return nil
	}
	if o.PID <= 0 {
		return nil
	}
	return &o
}

// lockOwnerNote 补充"该 PID 来自上一次开机/当前不存在"这类**事实**描述。
//
// 刻意不给"可以放心删除锁/可以杀掉它"这类结论：PID 复用是真实存在的，
// 错误结论会让运维杀掉无关进程。本实现绝不杀进程。
func lockOwnerNote(o *lockOwner) string {
	var notes []string
	if cur, was := bootID(), o.BootID; cur != "" && was != "" && cur != was {
		notes = append(notes, "该 PID 来自上一次开机，属异常退出遗留，不可按编号判断进程")
	} else if !processAlive(o.PID) {
		notes = append(notes, "该 PID 当前不存在")
	}
	if len(notes) == 0 {
		return ""
	}
	return "（" + strings.Join(notes, "；") + "）"
}

// processAlive 判断本机是否存在该 PID（signal 0，不发送任何信号）。
// 仅用于日志，且需注意 PID 可能已被复用。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// lockFilePath 由 key 派生锁文件路径（与配置文件同目录，见 lockDir）。
func lockFilePath(key, configPath string) string {
	return filepath.Join(lockDir(configPath), ".mocenter-"+sanitizeKey(key)+lockFileSuffix)
}

// lockDir 锁文件目录：**与配置文件同目录**（不做任何"退路"）。
//
// 两条设计约束，缺一不可：
//
//  1. 锁文件必须与配置文件同目录 —— 否则"同一份配置只允许一个实例"不成立。
//     反例：早先试过退回 os.TempDir()，结果是
//     **root 以 /etc/mocenter/x.json 跑服务、普通用户以同一路径跑 cron** 时，
//     两边各自在 /tmp 与配置目录各建一把锁，谁也拦不住谁 —— 日志看着"锁已取得"，
//     实则多实例照跑。这类"看起来生效、实际失效"比完全不锁更危险。
//
//  2. 该目录由**使用这份配置的账号**共同拥有（安装态 /etc/mocenter 由 root 属主、
//     组可写；或直接让 Agent 以 root 运行 —— systemd 单元默认就是 root）。
//     多用户混跑同一个配置文件本身就是配置错误：明文配置里含 agent_key，
//     普通用户能读即视为凭据泄露。此时应当各用各的配置。
//
// 因此这里**不做降级**：目录不可写就如实进入 AcquireSingleton 的"未启用保护"
// 告警分支，让问题暴露出来，而不是悄悄换一个位置把保护变成摆设。
func lockDir(configPath string) string {
	dir := filepath.Dir(absPath(configPath))
	// 配置目录通常已存在；MkdirAll 只兜住"配置还没生成"的首启场景。
	if err := os.MkdirAll(dir, 0700); err != nil {
		return dir // 让后续 OpenFile 如实失败并告警
	}
	return dir
}

// absPath 归一化路径（失败时退回原值），保证锁文件位置与当前工作目录无关。
func absPath(p string) string {
	if p == "" {
		return "."
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// sanitizeKey 把 key 转成安全文件名（只保留字母数字与 -_.）。
// 同时杜绝路径穿越（key 里若混入 ../ 也不会逃出锁目录）。
func sanitizeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), ".")
	if s == "" {
		return "mocenter-default"
	}
	return s
}

// DescribeSingleton 返回单实例锁的可读描述，供启动日志打印。
func DescribeSingleton(key, configPath string) string {
	return fmt.Sprintf("锁文件 %s（key %s）", lockFilePath(key, configPath), key)
}

// bootID 读取内核启动标识（Linux）。取不到返回空串（macOS/BSD 无此文件）。
// 用途：区分"上电残留的锁文件"与"本机现有进程"，仅作日志提示。
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
