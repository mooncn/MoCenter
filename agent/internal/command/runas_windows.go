//go:build windows

package command

import (
	"fmt"
	"os/exec"
)

// applyRunAs 当前平台（Windows）暂不支持跨用户降权执行，返回明确错误。
// 如需支持，可改用 CreateProcessWithTokenW 或交互式服务账号切换。
func applyRunAs(cmd *exec.Cmd, username string) error {
	return fmt.Errorf("当前平台不支持降权执行")
}
