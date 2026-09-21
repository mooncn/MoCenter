package filetransfer

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// startTask 开一个"在途"任务：声明大小比实发分片大，故不会触发合并、分片留在内存里。
func startTask(t *testing.T, m *Manager, agentID, filePath string) {
	t.Helper()
	chunk := []byte("0123456789")
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: agentID, FilePath: filePath, FileSize: int64(len(chunk) * 10),
		ChunkSeq: 0, ChunkData: chunk, ChunkMD5: md5hex(chunk),
	})
	if !ack.Ack {
		t.Fatalf("建立任务 %s 失败", filePath)
	}
}

// TestUploadTaskCountCap 在途任务数达上限后，新 FilePath 的任务必须被拒。
//
// 守护 M-8 的"条目数"闸门：加固前只要不断换 FilePath 就能让任务表无界增长。
func TestUploadTaskCountCap(t *testing.T) {
	m := NewManager(t.TempDir())
	for i := 0; i < maxActiveUploadTasks; i++ {
		startTask(t, m, "a1", fmt.Sprintf("/upload/f-%02d.bin", i))
	}
	if len(m.uploads) != maxActiveUploadTasks {
		t.Fatalf("前置条件不成立：在途任务 %d，期望 %d", len(m.uploads), maxActiveUploadTasks)
	}

	chunk := []byte("zz")
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/upload/extra.bin", FileSize: 100,
		ChunkSeq: 0, ChunkData: chunk, ChunkMD5: md5hex(chunk),
	})
	if ack.Ack {
		t.Fatal("任务数已达上限，新任务必须 NACK")
	}
	if len(m.uploads) != maxActiveUploadTasks {
		t.Fatalf("被拒的新任务不得占位，实际 %d", len(m.uploads))
	}

	// 已存在的任务不受影响：续传仍应放行，闸门只挡"新开任务"。
	startTask(t, m, "a1", "/upload/f-00.bin")
}

// TestUploadDeclaredSizeCap 声明大小超单任务上限时必须直接拒绝，不驻留任何分片。
func TestUploadDeclaredSizeCap(t *testing.T) {
	m := NewManager(t.TempDir())
	chunk := []byte("hello")
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/upload/huge.bin", FileSize: maxUploadTaskBytes + 1,
		ChunkSeq: 0, ChunkData: chunk, ChunkMD5: md5hex(chunk),
	})
	if ack.Ack {
		t.Fatal("声明大小超过单任务上限，必须 NACK")
	}
	if len(m.uploads) != 0 {
		t.Fatalf("被拒任务不得入表，实际 %d 条", len(m.uploads))
	}
	m.mu.RLock()
	stored := m.stored
	m.mu.RUnlock()
	if stored != 0 {
		t.Fatalf("被拒任务不得占用暂存额度，实际 %d", stored)
	}
}

// TestUploadAggregateStoredCap 总在途字节触顶时，即使单任务没超也要 NACK。
//
// 单任务上限挡不住"多条任务合力吃满内存"，总量闸门就是为这一条而存在。
func TestUploadAggregateStoredCap(t *testing.T) {
	m := NewManager(t.TempDir())
	startTask(t, m, "a1", "/upload/one.bin")

	// 模拟其它任务已把总量额度吃到只剩 1 字节。
	m.mu.Lock()
	m.stored = maxStoredBytes - 1
	m.mu.Unlock()

	chunk := []byte("abcdefghij")
	ack := m.HandleUploadChunk(&protocol.FileUploadReq{
		AgentID: "a1", FilePath: "/upload/one.bin", FileSize: 1000,
		ChunkSeq: 1, ChunkData: chunk, ChunkMD5: md5hex(chunk),
	})
	if ack.Ack {
		t.Fatal("总在途字节已达上限，续传分片必须 NACK")
	}

	m.mu.RLock()
	t1 := m.uploads[taskKey("a1", "/upload/one.bin")]
	stored := m.stored
	chunks := len(t1.chunks)
	m.mu.RUnlock()
	if stored != maxStoredBytes-1 {
		t.Fatalf("被拒分片不得增加暂存额度，实际 %d", stored)
	}
	if chunks != 1 {
		t.Fatalf("被拒分片不得写入任务，实际 %d 片", chunks)
	}
}

// TestCleanupStaleReclaimsIdleTasks 闲置超 TTL 的任务要被回收，且暂存额度同步释放。
func TestCleanupStaleReclaimsIdleTasks(t *testing.T) {
	m := NewManager(t.TempDir())
	startTask(t, m, "a1", "/upload/stale.bin")
	startTask(t, m, "a1", "/upload/fresh.bin")

	m.mu.Lock()
	stored := m.stored
	m.uploads[taskKey("a1", "/upload/stale.bin")].lastActive = time.Now().Add(-uploadTaskTTL - time.Minute)
	m.mu.Unlock()
	if stored == 0 {
		t.Fatal("前置条件不成立：任务未暂存任何字节")
	}

	if n := m.CleanupStale(); n != 1 {
		t.Fatalf("应回收 1 个闲置任务，实际 %d", n)
	}
	m.mu.RLock()
	_, staleThere := m.uploads[taskKey("a1", "/upload/stale.bin")]
	fresh := m.uploads[taskKey("a1", "/upload/fresh.bin")]
	storedAfter := m.stored
	m.mu.RUnlock()
	if staleThere {
		t.Fatal("闲置任务未被回收")
	}
	if fresh == nil {
		t.Fatal("仍在活跃期内的任务不得被回收")
	}
	if storedAfter != fresh.received {
		t.Fatalf("回收后暂存额度应只保留活跃任务的 %d 字节，实际 %d", fresh.received, storedAfter)
	}
}

// TestStoredReleasedAfterAssembly 上传成功落盘后暂存额度必须归零。
//
// 否则闸门会被一次次正常上传吃满，最后把运维用量挡在门外。
func TestStoredReleasedAfterAssembly(t *testing.T) {
	m := NewManager(t.TempDir())
	first := bytes.Repeat([]byte("x"), 64*1024)
	second := bytes.Repeat([]byte("y"), 5000)
	total := int64(len(first) + len(second))

	for _, c := range []struct {
		seq  int
		data []byte
	}{{0, first}, {1, second}} {
		ack := m.HandleUploadChunk(&protocol.FileUploadReq{
			AgentID: "a1", FilePath: "/opt/app.bin", FileSize: total,
			ChunkSeq: c.seq, ChunkData: c.data, ChunkMD5: md5hex(c.data),
		})
		if !ack.Ack {
			t.Fatalf("分片 %d 应 ACK", c.seq)
		}
	}

	m.mu.RLock()
	n := len(m.uploads)
	stored := m.stored
	m.mu.RUnlock()
	if n != 0 {
		t.Fatalf("已完成的任务应移出任务表，实际 %d 条", n)
	}
	if stored != 0 {
		t.Fatalf("落盘后暂存额度应归零，实际 %d", stored)
	}
}