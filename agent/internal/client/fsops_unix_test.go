//go:build !windows

package client

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"mocenter/agent/internal/protocol"
)

// TestFsOpChmod Unix 权限修改：八进制解析、非法值拒绝。
func TestFsOpChmod(t *testing.T) {
	dir := fsFixture(t)
	p := filepath.Join(dir, "a.txt")

	resp := runFsOp(protocol.FsOpReq{Op: "chmod", Path: p, Mode: "600"}, "a1")
	if !resp.OK {
		t.Fatalf("chmod 600 失败: %s", resp.Message)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600，实际 %v (%v)", st.Mode().Perm(), err)
	}

	// 带前导 0 与前缀 0o 都接受
	for _, m := range []string{"0644", "0o644"} {
		if resp := runFsOp(protocol.FsOpReq{Op: "chmod", Path: p, Mode: m}, "a1"); !resp.OK {
			t.Errorf("chmod %s 应接受: %s", m, resp.Message)
		}
	}
	for _, bad := range []string{"", "999", "abc", "17777"} {
		if resp := runFsOp(protocol.FsOpReq{Op: "chmod", Path: p, Mode: bad}, "a1"); resp.OK {
			t.Errorf("非法权限 %q 应被拒绝", bad)
		}
	}
}

// TestFsOpChown 属主/组修改：数字与原值均可，未知用户拒绝。
func TestFsOpChown(t *testing.T) {
	dir := fsFixture(t)
	p := filepath.Join(dir, "a.txt")
	uid, gid := os.Getuid(), os.Getgid()

	resp := runFsOp(protocol.FsOpReq{Op: "chown", Path: p, Owner: strconv.Itoa(uid), Group: strconv.Itoa(gid)}, "a1")
	if !resp.OK {
		t.Fatalf("chown 当前 uid/gid 失败: %s", resp.Message)
	}

	// 留空表示"不改"，两个都空则拒绝
	if resp := runFsOp(protocol.FsOpReq{Op: "chown", Path: p}, "a1"); resp.OK {
		t.Error("所有者与分组都为空时应拒绝")
	}
	if resp := runFsOp(protocol.FsOpReq{Op: "chown", Path: p, Owner: "no-such-user-xyz"}, "a1"); resp.OK {
		t.Error("不存在的用户应拒绝")
	}
	if resp := runFsOp(protocol.FsOpReq{Op: "chown", Path: p, Group: "no-such-group-xyz"}, "a1"); resp.OK {
		t.Error("不存在的分组应拒绝")
	}
}

// TestFsOpListMeta 列目录要带上权限与属主（右击菜单的权限对话框依赖它）。
func TestFsOpListMeta(t *testing.T) {
	dir := fsFixture(t)
	resp := runFsOp(fsReq("list", dir, "", false), "a1")
	if !resp.OK {
		t.Fatalf("列目录失败: %s", resp.Message)
	}
	if resp.Platform != "linux" && resp.Platform != "darwin" {
		t.Errorf("应返回平台信息，实际 %q", resp.Platform)
	}
	var found bool
	for _, e := range resp.Entries {
		if e.Name != "a.txt" {
			continue
		}
		found = true
		if e.Mode == 0 {
			t.Errorf("条目应带权限位: %+v", e)
		}
		if e.UID != os.Getuid() || e.Owner == "" {
			t.Errorf("条目应带属主信息: %+v", e)
		}
	}
	if !found {
		t.Fatal("没找到 a.txt")
	}
}
