package filetransfer

import (
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

// TestPushTaskCountCap 任务数上限：超限的新分发必须被拒绝并给出原因（L-1）。
func TestPushTaskCountCap(t *testing.T) {
	m := NewPushManager()
	orig := maxActivePushTasks
	maxActivePushTasks = 2
	defer func() { maxActivePushTasks = orig }()

	for i := 0; i < 2; i++ {
		if _, _, err := m.UploadChunk(chunk("c" + string(rune('a'+i)))); err != nil {
			t.Fatalf("第 %d 个任务应被接纳: %v", i, err)
		}
	}
	if _, _, err := m.UploadChunk(chunk("z")); err == nil {
		t.Fatal("超过任务数上限时必须拒绝")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("拒绝原因要能被界面读懂: %v", err)
	}
}

// TestPushBytesCapAndRelease 单任务字节上限：越限时任务被摘除且暂存额度归还，
// 后续正常分发不会因为"额度没释放"而被永久挡住。
func TestPushBytesCapAndRelease(t *testing.T) {
	m := NewPushManager()
	origTask := maxPushTaskBytes
	maxPushTaskBytes = 16
	defer func() { maxPushTaskBytes = origTask }()

	// 先收 10 字节（在限内）
	if _, _, err := m.UploadChunk(withData("a1", 16, 10, 0)); err != nil {
		t.Fatalf("首个分片应被接纳: %v", err)
	}
	if _, _, err := m.UploadChunk(withData("a1", 16, 12, 1)); err == nil {
		t.Fatal("超过单任务上限的分片必须被拒绝")
	}
	if m.Task("a1") != nil {
		t.Fatal("被拒绝的任务应已摘除，不该继续占内存")
	}
	if m.stored != 0 {
		t.Fatalf("暂存额度未归还: %d", m.stored)
	}
	// 额度归还后新任务仍可建立
	if _, _, err := m.UploadChunk(withData("a2", 16, 10, 0)); err != nil {
		t.Fatalf("额度未正确归还，后续分发被挡: %v", err)
	}
}

// TestPushDeclaredSizeCap 声明大小本身就超上限时，建任务那一刻就拒（不留半截条目）。
func TestPushDeclaredSizeCap(t *testing.T) {
	m := NewPushManager()
	origTask := maxPushTaskBytes
	maxPushTaskBytes = 16
	defer func() { maxPushTaskBytes = origTask }()

	if _, _, err := m.UploadChunk(withData("big", 1024, 8, 0)); err == nil {
		t.Fatal("声明大小超过单任务上限时必须拒绝")
	}
	if m.Task("big") != nil {
		t.Fatal("被拒绝的分发不应留下任务条目")
	}
}

// TestPushCleanupStale 闲置任务由 TTL 回收：目标主机离线时半截分片不应永驻内存。
func TestPushCleanupStale(t *testing.T) {
	m := NewPushManager()
	origTTL := pushTaskTTL
	pushTaskTTL = time.Millisecond
	defer func() { pushTaskTTL = origTTL }()

	if _, _, err := m.UploadChunk(withData("slow", 64, 10, 0)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if n := m.CleanupStale(); n != 1 {
		t.Fatalf("应回收 1 个闲置任务，实际 %d", n)
	}
	if m.stored != 0 {
		t.Fatalf("回收后暂存额度应归零，实际 %d", m.stored)
	}
}

// TestPushRemoveReleasesStored 正常完成后的 Remove 也要归还额度（否则闸门会被
// 一次次成功的分发吃满，最后挡住的是正常请求）。
func TestPushRemoveReleasesStored(t *testing.T) {
	m := NewPushManager()
	if _, _, err := m.UploadChunk(withData("ok", 64, 10, 0)); err != nil {
		t.Fatal(err)
	}
	m.Remove("ok")
	if m.stored != 0 {
		t.Fatalf("Remove 后暂存额度未归还: %d", m.stored)
	}
}

func chunk(id string) *protocol.FilePushReq {
	return &protocol.FilePushReq{CommandID: id, AgentID: "a1", FilePath: "/tmp/x", FileSize: 1024}
}

// withData 构造一个"声明总长 fileSize、序号 seq、本次带 size 字节"的分发分片。
func withData(id string, fileSize, size, seq int) *protocol.FilePushReq {
	r := chunk(id)
	r.FileSize = int64(fileSize)
	r.ChunkSeq = seq
	r.ChunkData = []byte(strings.Repeat("x", size))
	return r
}
