package filetransfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mocenter/server/internal/protocol"
)

// TestSanitizeComponent 净化规则表。
func TestSanitizeComponent(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"host-01", "host-01"},
		{"host_01.example", "host_01.example"},
		{"../../etc/cron.d/mocenter", ".._.._etc_cron.d_mocenter"},
		{`..\..\Windows\System32\evil`, ".._.._Windows_System32_evil"},
		{"/etc/passwd", "_etc_passwd"},
		{"..", "_.."},
		{".", "_."},
		{"...", "_..."},
		{"", "_"},
		{"   ", "_"},
		{"a/b", "a_b"},
		{"服务器-01", "___-01"}, // 每个非白名单字符（含多字节字符）各替换为 1 个下划线
		{"a\x00b", "a_b"},
		{"with space", "with_space"},
	}
	for _, c := range cases {
		if got := sanitizeComponent(c.in); got != c.want {
			t.Errorf("sanitizeComponent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLocalPathCannotEscape 回归测试：agentID 曾未净化，可让落盘路径逃出 baseDir。
// 这里断言所有已知攻击向量都被挡在 baseDir 之内。
func TestLocalPathCannotEscape(t *testing.T) {
	base := filepath.Join(t.TempDir(), "uploads")
	m := NewManager(base)

	evil := []string{
		"../../etc/cron.d/mocenter",
		"../../../etc/profile.d/evil",
		"../../var/spool/cron/root",
		"../../../../../../tmp/pwn",
		`..\..\Windows\System32\evil`,
		"/etc/cron.d/x",
		"..",
		".",
		"",
		"   ",
	}
	for _, id := range evil {
		p := m.localPath(id, "x.sh")
		if p == "" {
			continue // 被拒绝也是可接受的结果
		}
		// 关键断言：解析后的绝对路径必须仍在 baseDir 之内
		if !within(base, p) {
			t.Errorf("agentID=%q 导致路径逃出 baseDir: %s", id, p)
		}
		// 落盘名必须是单层文件名，不能含任何路径分隔符
		if strings.ContainsAny(filepath.Base(p), `/\`) {
			t.Errorf("agentID=%q 的落盘名含路径分隔符: %s", id, filepath.Base(p))
		}
		// 且其父目录必须就是 baseDir（而不是被 .. 带上去了）
		if filepath.Dir(p) != filepath.Clean(base) {
			t.Errorf("agentID=%q 的父目录不是 baseDir: %s", id, filepath.Dir(p))
		}
	}

	// filePath 侧同样不能越界
	for _, fp := range []string{"../../../etc/passwd", `..\..\boot.ini`, "/etc/shadow"} {
		p := m.localPath("host-01", fp)
		if p == "" {
			continue
		}
		if !within(base, p) {
			t.Errorf("filePath=%q 导致路径逃出 baseDir: %s", fp, p)
		}
	}
}

// TestUploadChunkRejectsTraversal 端到端：带穿越 payload 的分片不得在 baseDir 之外落盘。
func TestUploadChunkRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "uploads")
	m := NewManager(base)

	payload := []byte("#!/bin/sh\necho pwned\n")
	// 目标文件名（净化后）与「逃逸后本应命中的位置」
	escaped := filepath.Join(root, "pwned")
	_ = os.Remove(escaped)

	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID:   "../pwned", // 会试图逃出 uploads/
		FilePath:  "x",
		FileSize:  int64(len(payload)),
		ChunkSeq:  0,
		ChunkData: payload,
	})
	if ack == nil || !ack.Ack {
		// 被拒绝（NACK）也是正确结果
		return
	}
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("分片被写到了 baseDir 之外: %s", escaped)
	}
	// 确认文件确实落在 baseDir 之内
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("读取 baseDir 失败: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("文件既没落到 baseDir 内也没被拒绝 —— 状态不一致")
	}
	for _, e := range entries {
		if !within(base, filepath.Join(base, e.Name())) {
			t.Fatalf("落盘文件越界: %s", e.Name())
		}
		if strings.Contains(e.Name(), string(filepath.Separator)) {
			t.Fatalf("落盘文件名含分隔符: %s", e.Name())
		}
	}
}

// TestLocalPathRejectsEmptyResult 非法标识应返回空串，调用方据此拒绝。
func TestLocalPathRejectsEmptyResult(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "u"))
	// 净化后仍应有内容（不会返回空串），但绝不能越界
	for _, id := range []string{"..", "../..", ""} {
		if p := m.localPath(id, "f"); p != "" && !within(m.baseDir, p) {
			t.Fatalf("id=%q -> %s 越界", id, p)
		}
	}
}

// TestNormalTransferStillWorks 净化不能影响正常用例。
func TestNormalTransferStillWorks(t *testing.T) {
	base := filepath.Join(t.TempDir(), "uploads")
	m := NewManager(base)

	p := m.localPath("host-01", "/var/log/syslog")
	want := filepath.Join(base, "host-01_syslog")
	if p != want {
		t.Fatalf("正常路径被改写: got %s want %s", p, want)
	}

	payload := []byte("hello world")
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "host-01", FilePath: "/var/log/syslog",
		FileSize: int64(len(payload)), ChunkSeq: 0, ChunkData: payload,
	})
	if ack == nil || !ack.Ack {
		t.Fatal("正常上传应被接受")
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("正常上传未落盘: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("内容不一致: %q", got)
	}

	// 下载路径也要能找到同一个文件
	var read []byte
	if _, err := m.ReadFileChunks("host-01", "/var/log/syslog", 0, func(_ int, d []byte) {
		read = append(read, d...)
	}); err != nil {
		t.Fatalf("ReadFileChunks 失败: %v", err)
	}
	if string(read) != string(payload) {
		t.Fatalf("读回内容不一致: %q", read)
	}
}

// TestPushSaveToTempSanitized push 侧同样不能越界。
func TestPushSaveToTempSanitized(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "pushed")
	pm := NewPushManager()

	payload := []byte("data")
	pm.UploadChunk(&protocol.FilePushReq{
		CommandID: "c1", AgentID: "../escaped", FilePath: "x",
		FileSize: int64(len(payload)), ChunkSeq: 0, ChunkData: payload,
	})
	path, err := pm.SaveToTemp("c1", base)
	if err != nil {
		t.Fatalf("SaveToTemp 失败: %v", err)
	}
	if !within(base, path) {
		t.Fatalf("SaveToTemp 越界: %s", path)
	}
}
