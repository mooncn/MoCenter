package filetransfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

// chunk 构造一个 Agent 回传分片。
func dlChunk(agentID, path string, size int64, seq int, data []byte) *protocol.FileUploadReq {
	return &protocol.FileUploadReq{
		AgentID: agentID, FilePath: path, FileSize: size, ChunkSeq: seq, ChunkData: data,
	}
}

// TestExpectDownloadCompletes 下载等待：首片带大小 → 收齐后落盘并唤醒等待方。
func TestExpectDownloadCompletes(t *testing.T) {
	m := NewManager(t.TempDir())
	agent, path := "a1", "/data/report.txt"

	h, err := m.ExpectDownload(agent, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(h.LocalPath), "report.txt") {
		t.Fatalf("落盘文件名应保留原文件名: %s", h.LocalPath)
	}
	// 模拟 Agent 回传两片（与 sendFileChunks 的报文一致）。
	m.HandleUploadChunk(dlChunk(agent, path, 10, 0, []byte("hello")))
	m.HandleUploadChunk(dlChunk(agent, path, 10, 1, []byte("world")))

	if err := h.Wait(2 * time.Second); err != nil {
		t.Fatalf("下载应完成: %v", err)
	}
	got, err := os.ReadFile(h.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "helloworld" {
		t.Fatalf("落盘内容不正确: %q", got)
	}
	if _, ok := m.uploads[taskKey(agent, path)]; ok {
		t.Fatal("完成后任务应从表中摘除")
	}
}

// TestExpectDownloadEmptyFile 空文件也必须完成（Agent 会回一帧 FileSize=0）。
func TestExpectDownloadEmptyFile(t *testing.T) {
	m := NewManager(t.TempDir())
	h, err := m.ExpectDownload("a1", "/data/empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	m.HandleUploadChunk(dlChunk("a1", "/data/empty.txt", 0, 0, nil))
	if err := h.Wait(2 * time.Second); err != nil {
		t.Fatalf("空文件下载应完成: %v", err)
	}
	st, err := os.Stat(h.LocalPath)
	if err != nil || st.Size() != 0 {
		t.Fatalf("应落一个空文件: err=%v", err)
	}
}

// TestExpectDownloadRejectsOversize 单次下载上限：首片就超过 64MiB 时立刻失败，
// 不能让 HTTP 请求干等到超时。
func TestExpectDownloadRejectsOversize(t *testing.T) {
	m := NewManager(t.TempDir())
	h, err := m.ExpectDownload("a1", "/data/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	m.HandleUploadChunk(dlChunk("a1", "/data/big.bin", maxUploadTaskBytes+1, 0, []byte("x")))
	errc := make(chan error, 1)
	go func() { errc <- h.Wait(2 * time.Second) }()
	select {
	case e := <-errc:
		if e == nil || !strings.Contains(e.Error(), "上限") {
			t.Fatalf("应返回超限错误，实际 %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超限后等待方不应挂起")
	}
}

// TestExpectDownloadAgentFailure Agent 明确回报读不到文件时，等待方立刻失败。
func TestExpectDownloadAgentFailure(t *testing.T) {
	m := NewManager(t.TempDir())
	h, err := m.ExpectDownload("a1", "/data/denied.txt")
	if err != nil {
		t.Fatal(err)
	}
	m.HandleUploadChunk(&protocol.FileUploadReq{AgentID: "a1", FilePath: "/data/denied.txt", FileSize: -1})
	if err := h.Wait(2 * time.Second); err == nil {
		t.Fatal("Agent 失败通知后不应报告成功")
	}
}

// TestCancelDownload 超时取消：等待方立刻收到错误，残留任务被回收。
func TestCancelDownload(t *testing.T) {
	m := NewManager(t.TempDir())
	path := "/data/stuck.bin"
	h, err := m.ExpectDownload("a1", path)
	if err != nil {
		t.Fatal(err)
	}
	m.HandleUploadChunk(dlChunk("a1", path, 100, 0, []byte("partial")))
	m.CancelDownload("a1", path)
	if err := h.Wait(2 * time.Second); err == nil {
		t.Fatal("取消后不应报告成功")
	}
	if _, ok := m.uploads[taskKey("a1", path)]; ok {
		t.Fatal("取消后任务应被摘除")
	}
	if _, err := os.Stat(h.LocalPath); !os.IsNotExist(err) {
		t.Fatalf("取消后不应留下半截文件: %v", err)
	}
}
