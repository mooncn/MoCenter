//go:build windows

package command

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"mocenter/agent/internal/protocol"
)

// TestEchoForPipe 覆盖管道模式下的代做回显规则。
//
// 背景（回归防护）：Windows 以普通管道接 shell，cmd.exe 只会在读到**整行后**
// 才把输入行打印一遍，敲字时完全不可见。修复办法是 Agent 实时补一份回显，
// 并用 cmd /Q 关掉 cmd 自己那次滞后回显。本测试钉住这份回显的规则。
func TestEchoForPipe(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"空输入", "", ""},
		{"普通字符", "dir", "dir"},
		{"回车(CR)", "dir\r", "dir\r\n"},
		{"回车(CRLF)", "dir\r\n", "dir\r\n"}, // 不得变成两个换行
		{"单独换行", "a\nb", "a\r\nb"},
		{"退格 DEL", "abc\x7f", "abc\b \b"},
		{"退格 BS", "abc\x08", "abc\b \b"},
		{"方向键等转义序列不回显", "\x1b[A", ""},
		{"Ctrl-C 不代做回显", "\x03", ""},
		{"中文字节原样透传", "中文", "中文"},
	}
	for _, c := range cases {
		if got := echoForPipe([]byte(c.in)); got != c.want {
			t.Errorf("%s: echoForPipe(%q)=%q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestBuildCmdLocalEcho 钉住「哪个 shell 需要 Agent 代做回显」这一契约。
//
// 实测结论：
//   - cmd.exe 管道模式不实时回显 -> localEcho=true（并用 /Q 关掉滞后回显）
//   - powershell 管道模式逐字符实时回显 -> localEcho=false（代劳会显示两遍）
func TestBuildCmdLocalEcho(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 下的管道回显行为有意义")
	}

	cmdName, cmdArgs, cmdEcho := buildCmd(&protocol.CommandReq{Interactive: true, ShellType: "cmd"})
	if cmdName != "cmd.exe" || !cmdEcho {
		t.Errorf("cmd 交互式: name=%q localEcho=%v, want cmd.exe/true", cmdName, cmdEcho)
	}
	if !containsArg(cmdArgs, "/Q") {
		t.Errorf("cmd 交互式参数应包含 /Q（关掉 cmd 自身滞后回显），实际 %v", cmdArgs)
	}
	if !containsArg(cmdArgs, "/K") {
		t.Errorf("cmd 交互式参数应保留 /K（常驻交互），实际 %v", cmdArgs)
	}

	psName, _, psEcho := buildCmd(&protocol.CommandReq{Interactive: true, ShellType: "powershell"})
	if psName != "powershell.exe" || psEcho {
		t.Errorf("powershell 交互式: name=%q localEcho=%v, want powershell.exe/false", psName, psEcho)
	}

	// 非交互式不需要回显
	if _, _, e := buildCmd(&protocol.CommandReq{ShellType: "cmd", Command: "echo hi"}); e {
		t.Error("非交互式命令不应要求本地回显")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestInteractiveLocalEcho 端到端验证：走真实 StartInteractive，
// 敲键过程中就应收到回显（这正是「敲进去看不见」的 bug）。
func TestInteractiveLocalEcho(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅验证 Windows 管道模式")
	}

	var mu sync.Mutex
	var got strings.Builder
	sess, err := StartInteractive(&protocol.CommandReq{
		CommandID:   "t-echo",
		Interactive: true,
		ShellType:   "cmd",
		Rows:        24,
		Cols:        80,
	}, func(o *protocol.InteractiveOutput) {
		mu.Lock()
		defer mu.Unlock()
		if o.Type == "stdout" && o.Data != "" {
			got.WriteString(o.Data)
		}
	})
	if err != nil {
		t.Fatalf("StartInteractive: %v", err)
	}
	defer sess.Close()

	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return got.String()
	}
	waitFor := func(sub string, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if strings.Contains(snapshot(), sub) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	// 等 shell 给出首个提示符再开始敲，避免与启动横幅竞争
	if !waitFor(">", 15*time.Second) {
		t.Skip("未能等到 cmd 提示符（环境不支持交互式 cmd），跳过")
	}

	// 逐字符敲入；修复前此处收不到任何回显
	for _, ch := range "echo" {
		if err := sess.Write([]byte(string(ch))); err != nil {
			t.Fatalf("Write(%q): %v", ch, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !waitFor("echo", 5*time.Second) {
		t.Errorf("敲键过程中未收到回显（本地回显失效）；实际收到 %q", snapshot())
	}
}
