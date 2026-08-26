package command

import (
	"runtime"
	"testing"

	"mocenter/agent/internal/protocol"
)

func TestBuildCmd(t *testing.T) {
	wantDefault := "/bin/bash"
	if runtime.GOOS == "windows" {
		wantDefault = "cmd.exe"
	}
	cases := []struct {
		shell string
		cmd   string
		want  string
	}{
		{"bash", "ls", "/bin/bash"},
		{"sh", "ls", "/bin/sh"},
		{"cmd", "dir", "cmd.exe"},
		{"powershell", "Get-Date", "powershell.exe"},
		{"", "ls", wantDefault}, // 默认按目标平台推断
	}
	for _, c := range cases {
		name, _ := buildCmd(&protocol.CommandReq{Command: c.cmd, ShellType: c.shell})
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