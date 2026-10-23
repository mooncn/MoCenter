package client

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mocenter/agent/internal/protocol"
)

// fsFixture 建一个临时目录，返回其绝对路径。
func fsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fsReq(op, path, dest string, isDir bool) protocol.FsOpReq {
	return protocol.FsOpReq{RequestID: "r1", Op: op, Path: path, Dest: dest, IsDir: isDir}
}

// TestFsOpList 整机列目录：目录优先、给出父目录、条目大小正确。
func TestFsOpList(t *testing.T) {
	dir := fsFixture(t)
	resp := runFsOp(fsReq("list", dir, "", false), "a1")
	if !resp.OK {
		t.Fatalf("列目录失败: %s", resp.Message)
	}
	if len(resp.Entries) != 2 || !resp.Entries[0].Dir || resp.Entries[0].Name != "sub" {
		t.Fatalf("目录应排在最前: %+v", resp.Entries)
	}
	if resp.Parent != filepath.Dir(dir) {
		t.Fatalf("父目录不正确: %q", resp.Parent)
	}
}

// TestFsOpListPaging 列目录分页：total 正确、页之间不重不漏、越界返回空页。
func TestFsOpListPaging(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 25; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	page := func(offset, limit int) protocol.FsOpResp {
		return runFsOp(protocol.FsOpReq{Op: "list", Path: dir, Offset: offset, Limit: limit}, "a1")
	}
	p1 := page(0, 10)
	if !p1.OK || p1.Total != 25 || len(p1.Entries) != 10 {
		t.Fatalf("第 1 页不正确: ok=%v total=%d n=%d", p1.OK, p1.Total, len(p1.Entries))
	}
	p2 := page(10, 10)
	p3 := page(20, 10)
	if len(p2.Entries) != 10 || len(p3.Entries) != 5 {
		t.Fatalf("第 2/3 页条目数不对: %d %d", len(p2.Entries), len(p3.Entries))
	}
	seen := map[string]bool{}
	for _, e := range append(append(p1.Entries, p2.Entries...), p3.Entries...) {
		if seen[e.Name] {
			t.Errorf("分页重复: %s", e.Name)
		}
		seen[e.Name] = true
	}
	if len(seen) != 25 {
		t.Errorf("三页合计应 25 项，实际 %d", len(seen))
	}
	if over := page(100, 10); !over.OK || len(over.Entries) != 0 || over.Total != 25 {
		t.Errorf("越界页应为空且仍报 total: %+v", over)
	}
	// Limit=0 表示不分页（兼容不带分页参数的旧服务端）
	if def := runFsOp(fsReq("list", dir, "", false), "a1"); len(def.Entries) != 25 {
		t.Errorf("Limit=0 应返回整份列表，实际 %d", len(def.Entries))
	}
}

// TestFsOpCreateCopyMoveDelete 新建 → 复制 → 移动 → 删除 全链路。
func TestFsOpCreateCopyMoveDelete(t *testing.T) {
	dir := fsFixture(t)
	newFile := filepath.Join(dir, "new.txt")
	if resp := runFsOp(fsReq("create", newFile, "", false), "a1"); !resp.OK {
		t.Fatalf("新建文件失败: %s", resp.Message)
	}
	if st, err := os.Stat(newFile); err != nil || st.Size() != 0 {
		t.Fatalf("新建文件应为空文件: %v", err)
	}

	copyDir := filepath.Join(dir, "copy")
	if resp := runFsOp(fsReq("create", copyDir, "", true), "a1"); !resp.OK {
		t.Fatalf("新建目录失败: %s", resp.Message)
	}
	// 复制到已存在的目录 → 放在该目录下同名
	if resp := runFsOp(fsReq("copy", filepath.Join(dir, "a.txt"), copyDir, false), "a1"); !resp.OK {
		t.Fatalf("复制失败: %s", resp.Message)
	}
	got, err := os.ReadFile(filepath.Join(copyDir, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("复制结果不正确: %q, %v", got, err)
	}

	// 移动（重命名）
	moved := filepath.Join(dir, "moved.txt")
	if resp := runFsOp(fsReq("move", newFile, moved, false), "a1"); !resp.OK {
		t.Fatalf("移动失败: %s", resp.Message)
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("移动后源文件应不存在: %v", err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("移动后目标应存在: %v", err)
	}

	// 删除目录（递归）
	if resp := runFsOp(fsReq("delete", copyDir, "", false), "a1"); !resp.OK {
		t.Fatalf("删除失败: %s", resp.Message)
	}
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("删除后目录应不存在: %v", err)
	}
}

// TestFsOpRejectRootDelete 拒绝删除文件系统根（误点毁机）。
func TestFsOpRejectRootDelete(t *testing.T) {
	root := "/"
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(t.TempDir()) + `\`
	}
	if resp := runFsOp(fsReq("delete", root, "", false), "a1"); resp.OK {
		t.Fatalf("不应允许删除文件系统根 %q", root)
	}
}

// TestFsOpRejectRelative 相对路径一律拒绝（含义取决于 Agent 的工作目录）。
func TestFsOpRejectRelative(t *testing.T) {
	for _, op := range []string{"list", "create", "delete"} {
		if resp := runFsOp(fsReq(op, "relative/path", "", false), "a1"); resp.OK {
			t.Errorf("%s 不应接受相对路径", op)
		}
	}
}

// TestFsOpRejectCopyIntoItself 目录不能复制/移动到自身内部。
func TestFsOpRejectCopyIntoItself(t *testing.T) {
	dir := fsFixture(t)
	inside := filepath.Join(dir, "sub", "nested")
	if resp := runFsOp(fsReq("copy", dir, inside, false), "a1"); resp.OK {
		t.Fatal("不应允许把目录复制到自身内部")
	}
	if resp := runFsOp(fsReq("move", dir, inside, false), "a1"); resp.OK {
		t.Fatal("不应允许把目录移动到自身内部")
	}
}

// TestFsOpRename 重命名：同目录改名成功；目标已存在或跨目录一律拒绝。
func TestFsOpRename(t *testing.T) {
	dir := fsFixture(t)
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	if resp := runFsOp(fsReq("rename", src, dst, false), "a1"); !resp.OK {
		t.Fatalf("重命名失败: %s", resp.Message)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("重命名后新名字应存在: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("重命名后旧名字应不存在: %v", err)
	}

	// 目标已存在：不覆盖
	occupado := filepath.Join(dir, "c.txt")
	if err := os.WriteFile(occupado, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp := runFsOp(fsReq("rename", dst, occupado, false), "a1"); resp.OK {
		t.Fatal("目标已存在时不应覆盖")
	}
	// 跨目录：应由"移动"负责
	if resp := runFsOp(fsReq("rename", dst, filepath.Join(dir, "sub", "b.txt"), false), "a1"); resp.OK {
		t.Fatal("不应允许跨目录重命名")
	}
	// 新名字与原名字相同
	if resp := runFsOp(fsReq("rename", dst, dst, false), "a1"); resp.OK {
		t.Fatal("同名重命名应被拒绝")
	}
}

// TestFsOpDisk 分区信息：至少一个卷，容量自洽；list(WithDisk) 也要带上。
func TestFsOpDisk(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("该平台不枚举分区")
	}
	resp := runFsOp(fsReq("disk", "", "", false), "a1")
	if !resp.OK || len(resp.Volumes) == 0 {
		t.Fatalf("读取磁盘信息失败: %s", resp.Message)
	}
	for _, v := range resp.Volumes {
		if v.Path == "" || v.Total <= 0 || v.Free < 0 || v.Free > v.Total {
			t.Errorf("卷信息不合理: %+v", v)
		}
	}
	req := fsReq("list", fsFixture(t), "", false)
	req.WithDisk = true
	if lr := runFsOp(req, "a1"); lr.OK && len(lr.Volumes) == 0 {
		t.Error("list(WithDisk) 应返回分区信息")
	}
}

// TestFsOpRoots 根列表：Unix 为 "/"，Windows 至少包含系统盘。
func TestFsOpRoots(t *testing.T) {
	resp := runFsOp(fsReq("roots", "", "", false), "a1")
	if !resp.OK || len(resp.Entries) == 0 {
		t.Fatalf("读取根失败: %s", resp.Message)
	}
	for _, e := range resp.Entries {
		if !strings.HasSuffix(e.Path, string(filepath.Separator)) && e.Path != "/" {
			t.Errorf("根路径应以分隔符结尾: %q", e.Path)
		}
	}
	if runtime.GOOS != "windows" && resp.Entries[0].Path != "/" {
		t.Fatalf("Unix 根应为 /，实际 %q", resp.Entries[0].Path)
	}
}
