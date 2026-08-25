//go:build windows

// PTY 助手（Windows）：当前版本退化，使用普通管道（上游 ConPTY 预留）。
package command

import (
	"os"
	"os/exec"
)

// startPTY Windows 退化：通过 cmd.StdinPipe 桥接，不创建 PTY。
func startPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	return nil, nil
}

func resizePTY(f *os.File, rows, cols int) error {
	return nil
}
