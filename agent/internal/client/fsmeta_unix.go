//go:build !windows

package client

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

// Unix 专有的权限 / 属主操作。Windows 版见 fsmeta_windows.go（一律返回"不支持"）。

// fsEntryMeta 取出权限位与属主/属组。DirEntry.Info 对符号链接返回链接本身的信息。
func fsEntryMeta(info os.FileInfo) (mode uint32, uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint32(st.Mode) & 0o7777, int(st.Uid), int(st.Gid), true
}

// 用户名/组名解析带缓存：列一个目录时同一 UID 会出现很多次，
// 逐条查 /etc/passwd 在几千项目录里很浪费。
var (
	fsNameMu     sync.Mutex
	fsUserCache  = map[int]string{}
	fsGroupCache = map[int]string{}
)

func fsUserName(uid int) string {
	fsNameMu.Lock()
	name, ok := fsUserCache[uid]
	fsNameMu.Unlock()
	if ok {
		return name
	}
	name = ""
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		name = u.Username
	}
	fsNameMu.Lock()
	if len(fsUserCache) > 1024 {
		fsUserCache = map[int]string{}
	}
	fsUserCache[uid] = name
	fsNameMu.Unlock()
	return name
}

func fsGroupName(gid int) string {
	fsNameMu.Lock()
	name, ok := fsGroupCache[gid]
	fsNameMu.Unlock()
	if ok {
		return name
	}
	name = ""
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
		name = g.Name
	}
	fsNameMu.Lock()
	if len(fsGroupCache) > 1024 {
		fsGroupCache = map[int]string{}
	}
	fsGroupCache[gid] = name
	fsNameMu.Unlock()
	return name
}

// applyFsChmod 按八进制字符串改权限；解析规则见 fsmeta.go。
func applyFsChmod(path, modeText string) error {
	m, err := parseFsMode(modeText)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, m); err != nil {
		return fmt.Errorf("修改权限失败: %v", err)
	}
	return nil
}

// applyFsChown 改属主/属组（用户名或数字 ID；留空表示不变）。
// os.Chown 跟随符号链接，与 chown 命令的默认行为一致。
func applyFsChown(path, ownerText, groupText string) error {
	uid, gid, err := resolveFsOwner(ownerText, groupText)
	if err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("修改所有者失败: %v", err)
	}
	return nil
}
