package command

import (
	"runtime"
	"strings"
	"testing"

	"mocenter/agent/internal/protocol"
)

// TestCappedBufferTruncates 输出超限时只保留前 limit 字节并附带截断提示，
// 同时仍向 os/exec 报告全部写成功 —— 否则命令会因短写收到 EPIPE。
func TestCappedBufferTruncates(t *testing.T) {
	w := &cappedBuffer{limit: 1024}
	payload := strings.Repeat("x", 4096)
	n, err := w.Write([]byte(payload))
	if err != nil || n != len(payload) {
		t.Fatalf("超限写入应回报全部字节: n=%d err=%v", n, err)
	}
	got := w.String()
	if !strings.HasPrefix(got, payload[:1024]) {
		t.Fatal("应保留前 1024 字节原内容")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatalf("应带截断提示，实得 %q", got[len(got)-40:])
	}
	// 已满后继续写：不增长、不报错
	if n, err := w.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("满额后写入仍应成功: n=%d err=%v", n, err)
	}
	if len(w.buf.String()) != 1024 {
		t.Fatalf("缓冲不应继续增长，实得 %d", w.buf.Len())
	}
}

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
