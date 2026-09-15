package command

import (
	"runtime"
	"testing"

	"mocenter/agent/internal/protocol"
)

func TestBuildCmd(t *testing.T) {
	wantDefault := "/bin/bash"
	wantPowershell := "pwsh"
	if runtime.GOOS == "windows" {
		wantDefault = "cmd.exe"
		wantPowershell = "powershell.exe"
	}
	cases := []struct {
		shell string
		cmd   string
		want  string
	}{
		{"bash", "ls", "/bin/bash"},
		{"sh", "ls", "/bin/sh"},
		{"cmd", "dir", "cmd.exe"},
		{"powershell", "Get-Date", wantPowershell},
		{"", "ls", wantDefault}, // 默认按目标平台推断
	}
	for _, c := range cases {
		name, _, _ := buildCmd(&protocol.CommandReq{Command: c.cmd, ShellType: c.shell})
		if name != c.want {
			t.Errorf("buildCmd(%q) name=%q, want %q", c.shell, name, c.want)
		}
	}
}

func TestExecuteBasic(t *testing.T) {
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "cmd"
	}
	res := Execute(&protocol.CommandReq{Command: "echo mocenter-ok", ShellType: shell})
	if res.Status != 0 {
		t.Fatalf("echo should succeed, status=%d stderr=%q", res.Status, res.Stderr)
	}
}

func TestNormalizePTYOutput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"line1\nline2\n", "line1\r\nline2\r\n"},
		{"already\r\ncrlf", "already\r\ncrlf"},
		{"no newline", "no newline"},
		{"mixed\r\nthen\nend", "mixed\r\nthen\r\nend"},
	}
	for _, c := range cases {
		if got := normalizePTYOutput(c.in); got != c.want {
			t.Errorf("normalizePTYOutput(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeInput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dir\r", "dir\r\n"},   // xterm 回车只发 CR
		{"dir\r\n", "dir\r\n"}, // 已是 CRLF 不应变成双 CR
		{"a\nb", "a\r\nb"},     // 粘贴的多行文本
		{"abc\x7f", "abc\b"},   // 退格
		{"\x03", "\x03"},       // Ctrl+C 原样透传
	}
	for _, c := range cases {
		if got := string(normalizeInput([]byte(c.in))); got != c.want {
			t.Errorf("normalizeInput(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecuteTimeout(t *testing.T) {
	shell := "sh"
	var cmd string
	if runtime.GOOS == "windows" {
		shell = "cmd"
		cmd = "ping -n 6 127.0.0.1"
	} else {
		cmd = "sleep 5"
	}
	res := Execute(&protocol.CommandReq{Command: cmd, ShellType: shell, Timeout: 1})
	if res.Status != 2 {
		t.Fatalf("timeout command should return status=2, got %d", res.Status)
	}
}
