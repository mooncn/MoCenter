// Package command 实现 Agent 端的远程命令执行。
// 支持 bash/sh/cmd/powershell、超时控制、工作目录、环境变量覆盖、
// 交互式 PTY 模式（Linux 使用 pty，Windows 使用 ConPTY/降级管道）。
package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"mocenter/agent/internal/protocol"
)

// tracked 记录运行中的非交互命令，commandID -> 结束回调，用于「断开终端连接」。
var (
	trackMu sync.Mutex
	tracked = map[string]func(){}
)

// Result 命令执行结果。
type Result struct {
	Status   int // 0=成功 1=失败 2=超时
	ExitCode int
	Stdout   string
	Stderr   string
	Duration int64
}

// 单条命令每个输出流的保留上限。
//
// 命令输出会**整包**回传服务端并写入审计；不设上限时一条 `cat 大日志`
// 就能让 Agent 与服务端同时占用数百 MB（服务端每连接扫描缓冲上限 16 MiB，
// 超限还会直接把 Agent 踢下线）。上限取小一些还有一层考虑：JSON 编码会把
// 控制字符转义成 \u00XX，最坏情况下体积翻 6 倍，1 MiB 输出仍在扫描缓冲内。
const (
	maxStdoutCapture = 1 << 20
	maxStderrCapture = 256 << 10
)

// cappedBuffer 是带容量上限的 io.Writer。
//
// 超限后仍返回 len(p), nil：os/exec 把短写当成写失败并可能让命令收到 EPIPE，
// 而这里要的是「结果被截断」，不是「命令被打断」。
type cappedBuffer struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedBuffer) String() string {
	if !w.truncated {
		return w.buf.String()
	}
	return w.buf.String() + fmt.Sprintf("\n\n[输出超过 %d KiB，已截断]", w.limit>>10)
}

// buildCmd 根据 shell_type 构造命令行参数。
// 交互模式（req.Interactive）下返回登录式 shell（不带 -c），
// 使会话可连续执行多条命令，而非“一命令一进程一退出”。
//
// 第三个返回值 localEcho 表示「这个 shell 在管道模式下不会自己回显用户输入，
// 需要 Agent 代做实时回显」。实测（见 echo 相关探针）：
//   - cmd.exe：管道模式下只在读到**整行后**才把输入行打印一遍 —— 敲字时不可见、
//     回车后才出现；故需 Agent 补实时回显，并用 /Q 关掉 cmd 自己那次滞后回显。
//   - powershell.exe：从管道读入时**逐字符即时回显**，Agent 不能代劳，否则每个
//     字符都会显示两遍。
func buildCmd(req *protocol.CommandReq) (name string, args []string, localEcho bool) {
	shell := req.ShellType
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "cmd"
		} else {
			shell = "bash"
		}
	}
	if req.Interactive {
		switch shell {
		case "bash":
			return "/bin/bash", []string{"-i"}, false
		case "sh":
			return "/bin/sh", []string{"-i"}, false
		case "powershell":
			if runtime.GOOS == "windows" {
				return "powershell.exe", []string{"-NoProfile"}, false
			}
			return "pwsh", []string{"-NoProfile"}, false
		case "cmd":
			// /Q 关掉 cmd 自己的「行回显」：管道模式下 cmd 要等读到整行才把输入行
			// 打印一遍（表现为「敲的时候看不见、回车后才显示」）。回显改由 Agent 实时
			// 补做（见 InteractiveSession.localEcho），先关掉 cmd 的以免重复两遍。
			// /K 先切 UTF-8 代码页：cmd 默认按系统 GBK 输出，透传到按 UTF-8
			// 解码的 Web 终端会乱码；随后保持交互 shell 常驻。
			return "cmd.exe", []string{"/Q", "/K", "chcp 65001>nul"}, true
		default:
			if runtime.GOOS == "windows" {
				return "cmd.exe", []string{"/Q"}, true
			}
			return "/bin/bash", []string{"-i"}, false
		}
	}
	switch shell {
	case "bash":
		return "/bin/bash", []string{"-c", req.Command}, false
	case "sh":
		return "/bin/sh", []string{"-c", req.Command}, false
	case "powershell":
		if runtime.GOOS == "windows" {
			return "powershell.exe", []string{"-NoProfile", "-Command", req.Command}, false
		}
		return "pwsh", []string{"-NoProfile", "-Command", req.Command}, false
	case "cmd":
		return "cmd.exe", []string{"/c", winUtf8Prefix() + req.Command}, false
	default:
		if runtime.GOOS == "windows" {
			return "cmd.exe", []string{"/c", winUtf8Prefix() + req.Command}, false
		}
		return "/bin/bash", []string{"-c", req.Command}, false
	}
}

// winUtf8Prefix 返回强制 cmd 使用 UTF-8 字符集的前缀，用于消除中文乱码。
func winUtf8Prefix() string {
	if runtime.GOOS == "windows" {
		return "chcp 65001>nul& "
	}
	return ""
}

// Execute 执行一条非交互式命令。
func Execute(req *protocol.CommandReq) *Result {
	ctx := context.Background()
	var cancel context.CancelFunc
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	// 交互式命令默认无超时，由用户手动中断
	if !req.Interactive {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	return runCmd(req, ctx, nil)
}

// runCmd 以 ctx 构建并运行命令进程，返回结果。cmdOut 非空时立即写入创建的
// *exec.Cmd（便于外部按需结束进程树）；ctx 取消会一并结束相关进程树。
func runCmd(req *protocol.CommandReq, ctx context.Context, cmdOut **exec.Cmd) *Result {
	start := time.Now()

	name, args, _ := buildCmd(req)
	cmd := exec.CommandContext(ctx, name, args...)
	if cmdOut != nil {
		*cmdOut = cmd
	}
	// 降权执行：以指定用户身份运行（非 Windows）
	if req.User != "" {
		if err := applyRunAs(cmd, req.User); err != nil {
			return &Result{Status: 1, Stderr: "降权执行失败: " + err.Error(), Duration: time.Since(start).Milliseconds()}
		}
	}

	// 工作目录
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	// 环境变量覆盖/追加
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdout := &cappedBuffer{limit: maxStdoutCapture}
	stderr := &cappedBuffer{limit: maxStderrCapture}
	// cmd 已通过 chcp 65001 强制 UTF-8，其他 shell 默认 UTF-8，直接透传原始字节
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	res := &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start).Milliseconds(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Status = 2 // 超时
		killProc(cmd)
	} else if ctx.Err() == context.Canceled {
		res.Status = 1 // 被外部中断（断开连接）
		if cmd.Process != nil {
			res.ExitCode = -1
		}
	} else if err != nil {
		res.Status = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		}
	} else {
		res.Status = 0
	}
	return res
}

// RunAsync 异步执行非交互命令。执行期间按 command_id 登记 kill 回调，
// 便于「断开终端连接」时结束相关进程；结束后自动注销并回调 onDone。
func RunAsync(req *protocol.CommandReq, onDone func(*Result)) {
	ctx, cancel := context.WithCancel(context.Background())
	if req.Timeout > 0 {
		// 将超时与取消合并到同一个 cancel，避免返回时误取消进程
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
	}
	var cmd *exec.Cmd

	trackMu.Lock()
	tracked[req.CommandID] = func() {
		// 先结束进程树，再取消上下文，避免子进程被孤立
		if cmd != nil {
			killProc(cmd)
		}
		cancel()
	}
	trackMu.Unlock()

	go func() {
		defer cancel()
		res := runCmd(req, ctx, &cmd)
		trackMu.Lock()
		delete(tracked, req.CommandID)
		trackMu.Unlock()
		if onDone != nil {
			onDone(res)
		}
	}()
}

// Kill 结束指定 command_id 的命令进程。返回是否找到并下发结束。
func Kill(commandID string) bool {
	if commandID == "" {
		return false
	}
	trackMu.Lock()
	fn, ok := tracked[commandID]
	trackMu.Unlock()
	if ok && fn != nil {
		fn()
	}
	return ok
}

// KillAll 结束所有登记中的命令进程，返回结束数量。
func KillAll() int {
	trackMu.Lock()
	fns := make([]func(), 0, len(tracked))
	for _, f := range tracked {
		fns = append(fns, f)
	}
	trackMu.Unlock()
	for _, f := range fns {
		f()
	}
	return len(fns)
}

// killProc 终止进程（Windows 连同进程树，其他平台直接 Kill）。
func killProc(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/PID", fmt.Sprintf("%d", cmd.Process.Pid), "/T", "/F").Run()
	} else {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Process.Release()
}
