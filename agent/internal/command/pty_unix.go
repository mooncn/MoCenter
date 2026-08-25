//go:build linux || darwin

// PTY 助手（Linux/macOS）：基于 creack/pty 创建真实伪终端。
package command

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startPTY 以 PTY 方式启动命令。
func startPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, err
	}
	return f, nil
}

func resizePTY(f *os.File, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	return pty.Setsize(f, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}
