// Package command 实现 Agent 端的远程命令执行。
// 支持 bash/sh/cmd/powershell、超时控制、工作目录、环境变量覆盖、
// 交互式 PTY 模式（Linux 使用 pty，Windows 使用 ConPTY/降级管道）。
package command

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"mocenter/agent/internal/protocol"
)

// Result 命令执行结果。
type Result struct {
	Status   int    // 0=成功 1=失败 2=超时
	ExitCode int
	Stdout   string
	Stderr   string
	Duration int64
}

// buildCmd 根据 shell_type 构造命令行参数。
func buildCmd(req *protocol.CommandReq) (name string, args []string) {
	shell := req.ShellType
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "cmd"
		} else {
			shell = "bash"
		}
	}
	switch shell {
	case "bash":
		return "/bin/bash", []string{"-c", req.Command}
	case "sh":
		return "/bin/sh", []string{"-c", req.Command}
	case "powershell":
		if runtime.GOOS == "windows" {
			return "powershell.exe", []string{"-NoProfile", "-Command", req.Command}
		}
		return "pwsh", []string{"-NoProfile", "-Command", req.Command}
	case "cmd":
		return "cmd.exe", []string{"/c", req.Command}
	default:
		if runtime.GOOS == "windows" {
			return "cmd.exe", []string{"/c", req.Command}
		}
		return "/bin/bash", []string{"-c", req.Command}
	}
}

// Execute 执行一条非交互式命令。
func Execute(req *protocol.CommandReq) *Result {
	start := time.Now()
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

	name, args := buildCmd(req)
	cmd := exec.CommandContext(ctx, name, args...)
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

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start).Milliseconds(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Status = 2 // 超时
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
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


