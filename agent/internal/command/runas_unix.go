//go:build !windows

package command

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// applyRunAs 使命令以指定用户身份运行（Linux/macOS 通过 setuid-like Credential）。
// 支持用户名或数字 UID。
func applyRunAs(cmd *exec.Cmd, username string) error {
	if username == "" {
		return nil
	}
	var u *user.User
	var err error
	if uid, cerr := strconv.Atoi(username); cerr == nil {
		u, err = user.LookupId(strconv.Itoa(uid))
	} else {
		u, err = user.Lookup(username)
	}
	if err != nil {
		return fmt.Errorf("unknown user %q", username)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	return nil
}