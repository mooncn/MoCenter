package winapp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRotatingWriterSplitsAtLimit 单份日志超过上限即切到 .1，且只保留一份旧文件。
//
// 旧实现只在启动时截断，开 info 级别长时间运行会把磁盘写满；
// 这里用极小的上限把轮转行为固定下来。
func TestRotatingWriterSplitsAtLimit(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 64
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	w, err := openRotating(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.f.Close() }() // 轮转后 w.f 指向新文件，必须延迟取值

	if _, err := w.Write(bytes.Repeat([]byte("a"), 50)); err != nil {
		t.Fatal(err)
	}
	// 再写 50 字节：50+50 > 64，应先轮转再写
	if _, err := w.Write(bytes.Repeat([]byte("b"), 50)); err != nil {
		t.Fatal(err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != 50 || cur[0] != 'b' {
		t.Fatalf("当前日志应只含第二次写入的 50 字节，实际 %d 字节", len(cur))
	}
	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("轮转后应存在 .1 文件: %v", err)
	}
	if len(prev) != 50 || prev[0] != 'a' {
		t.Fatalf(".1 应含第一次写入的 50 字节，实际 %d 字节", len(prev))
	}
}

// TestOpenRotatingRotatesOversizedFile 启动时文件已超限：挪成 .1，新文件从 0 开始。
func TestOpenRotatingRotatesOversizedFile(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 16
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 32), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := openRotating(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.f.Close() }() // 轮转后 w.f 指向新文件，必须延迟取值

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("新文件应为空，实际 %d 字节", fi.Size())
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != 32 {
		t.Fatalf(".1 应保留旧的 32 字节，实际 err=%v", err)
	}
}
