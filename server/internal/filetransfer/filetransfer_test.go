package filetransfer

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mocenter/server/internal/protocol"
)

func md5hex(b []byte) string {
	return fmt.Sprintf("%x", md5.Sum(b))
}

// TestUploadAssembly 验证分片上传全流程：ACK、MD5 校验失败 NACK、断点续传去重、合并落盘。
func TestUploadAssembly(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// 两个分片：64KB + 5000B
	first := bytes.Repeat([]byte("x"), 64*1024)
	second := bytes.Repeat([]byte("y"), 5000)
	total := int64(len(first) + len(second))

	// 第一分片：应 ACK
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/opt/app.bin", FileSize: total,
		ChunkSeq: 0, ChunkData: first, ChunkMD5: md5hex(first),
	})
	if !ack.Ack {
		t.Fatal("first chunk should ACK")
	}

	// MD5 校验失败：应 NACK 且不写入
	bad := &protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/opt/app.bin", FileSize: total,
		ChunkSeq: 1, ChunkData: []byte("corrupted"), ChunkMD5: "deadbeef",
	}
	if ack := m.HandleUploadChunk(bad); ack.Ack {
		t.Fatal("corrupted chunk should NACK")
	}

	// 断点续传：重发已接收的 seq0，应直接 ACK 且不导致重复计数
	if ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/opt/app.bin", FileSize: total,
		ChunkSeq: 0, ChunkData: first, ChunkMD5: md5hex(first),
	}); !ack.Ack {
		t.Fatal("duplicate chunk should ACK (resume)")
	}

	// 第二分片：正确写入，触发合并且落盘
	if ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/opt/app.bin", FileSize: total,
		ChunkSeq: 1, ChunkData: second, ChunkMD5: md5hex(second),
	}); !ack.Ack {
		t.Fatal("second chunk should ACK")
	}

	fp := filepath.Join(dir, "a1_app.bin")
	st, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("assembled file missing: %v", err)
	}
	if st.Size() != total {
		t.Fatalf("assembled size=%d, want %d", st.Size(), total)
	}
}

// TestUploadFileSizeZero 文件大小为 0（空文件）不应产生合并动作。
func TestUploadFileSizeZero(t *testing.T) {
	m := NewManager(t.TempDir())
	_ = m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/tmp/empty", FileSize: 0,
		ChunkSeq: 0, ChunkData: nil, ChunkMD5: "",
	})
	// 不应 panic，文件大小 0 不落盘
}