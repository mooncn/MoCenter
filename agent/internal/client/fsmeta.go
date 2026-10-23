package client

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// 权限/属主参数的解析与校验放在这里（不带平台标签），便于在任意平台单测；
// 真正落盘的系统调用在 fsmeta_unix.go / fsmeta_windows.go。

// parseFsMode 解析八进制权限串：接受 755 / 0644 / 0o644，上限 07777。
func parseFsMode(text string) (os.FileMode, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, errors.New("权限为空")
	}
	m, err := strconv.ParseUint(strings.TrimPrefix(text, "0o"), 8, 32)
	if err != nil || m > 0o7777 {
		return 0, errors.New("权限格式不正确（八进制，如 755 或 0644）")
	}
	return os.FileMode(m), nil
}

// resolveFsOwner 把"用户名/组名或数字 ID"解析成 uid/gid；-1 表示该项不变。
func resolveFsOwner(ownerText, groupText string) (int, int, error) {
	ownerText = strings.TrimSpace(ownerText)
	groupText = strings.TrimSpace(groupText)
	if ownerText == "" && groupText == "" {
		return -1, -1, errors.New("请填写所有者或分组")
	}
	uid, gid := -1, -1
	if ownerText != "" {
		if n, err := strconv.Atoi(ownerText); err == nil {
			uid = n
		} else if u, err := user.Lookup(ownerText); err == nil {
			uid, _ = strconv.Atoi(u.Uid)
		} else {
			return -1, -1, fmt.Errorf("用户 %q 不存在", ownerText)
		}
	}
	if groupText != "" {
		if n, err := strconv.Atoi(groupText); err == nil {
			gid = n
		} else if g, err := user.LookupGroup(groupText); err == nil {
			gid, _ = strconv.Atoi(g.Gid)
		} else {
			return -1, -1, fmt.Errorf("分组 %q 不存在", groupText)
		}
	}
	if uid == -1 && gid == -1 {
		return -1, -1, errors.New("请填写所有者或分组")
	}
	return uid, gid, nil
}
