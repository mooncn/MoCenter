//go:build windows

package command

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestStartConPTY 冒烟验证：用 ConPTY 启动 cmd，写入命令并读取带伪控制台回显的输出。
// 需要真实 Windows 环境（本环境的沙箱禁止创建子进程），默认跳过。
func TestStartConPTY(t *testing.T) {
	if os.Getenv("MOCENTER_CONPTY_TEST") == "" {
		t.Skip("需要真实 Windows 环境：设置 MOCENTER_CONPTY_TEST=1 运行 ConPTY 冒烟")
	}
	name := "cmd.exe"
	stdin, stdout, resize, cleanup, err := startConPTY(name, nil, 24, 80)
	if err != nil {
		t.Fatalf("startConPTY failed: %v", err)
	}
	defer cleanup()

	if err := resize(24, 80); err != nil {
		t.Fatalf("resize failed: %v", err)
	}

	if _, err := stdin.Write([]byte("echo conpty-ok\r")); err != nil {
		t.Fatalf("write to conpty failed: %v", err)
	}

	// 后台读取输出；ConPTY 无 EOF，用定时器触发 cleanup 解除阻塞
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	tmp := make([]byte, 512)
	go func() {
		for {
			n, rerr := stdout.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if rerr != nil {
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		cleanup()
		<-done
	}

	got := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("conpty-ok")) {
		t.Fatalf("output missing expected echo, got: %q", got)
	}
}