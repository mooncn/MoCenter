//go:build windows

package client

import (
	"errors"
	"os"
)

// Windows 没有 Unix 权限位与 UID/GID 概念：列表里不返回这些字段，
// 界面对应菜单项也会按平台隐藏；直接调用则给出明确错误而不是静默成功。
func fsEntryMeta(os.FileInfo) (mode uint32, uid, gid int, ok bool) {
	return 0, 0, 0, false
}

func fsUserName(int) string  { return "" }
func fsGroupName(int) string { return "" }

func applyFsChmod(string, string) error {
	return errors.New("当前平台（Windows）不支持修改 Unix 权限")
}

func applyFsChown(string, string, string) error {
	return errors.New("当前平台（Windows）不支持修改所有者/分组")
}
