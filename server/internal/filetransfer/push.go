// Package filetransfer 文件分发管理器：Web 上传文件分发到 Agent。
//
// 流程：Web 将文件分片上传到服务端 -> 服务端按分片缓存并做 MD5 校验 ->
// 全部到齐后由服务端向目标 Agent 依次推送分片 -> Agent 落盘并回 ACK。
// 服务端作为中转与缓冲，可应对 Agent 短暂离线场景。
package filetransfer

import (
	"crypto/md5"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"mocenter/server/internal/protocol"
)

// pushTask 一次文件分发任务。
type pushTask struct {
	CommandID string
	AgentID   string
	FilePath  string // Agent 端目标路径
	FileSize  int64
	Chunks    map[int][]byte // 分片缓存
	maxSeq    int
	total     int64 // 已缓存字节数
	done      bool
}

// PushManager 管理 Web -> Server -> Agent 的文件分发。
type PushManager struct {
	mu    sync.Mutex
	tasks map[string]*pushTask // key = commandID
}

// NewPushManager 创建文件分发管理器。
func NewPushManager() *PushManager {
	return &PushManager{tasks: make(map[string]*pushTask)}
}

// UploadChunk 接收 Web 上传的分片并缓存，返回是否已收齐及目标信息。
// 返回 (complete bool, task *pushTask, ack bool)。
func (m *PushManager) UploadChunk(req *protocol.FilePushReq) (*pushTask, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[req.CommandID]
	if !ok {
		if req.FileSize < 0 {
			return nil, false // 分发失败
		}
		t = &pushTask{
			CommandID: req.CommandID,
			AgentID:   req.AgentID,
			FilePath:  req.FilePath,
			FileSize:  req.FileSize,
			Chunks:    make(map[int][]byte),
		}
		m.tasks[req.CommandID] = t
	}

	if req.FileSize < 0 {
		// 取消分发
		delete(m.tasks, req.CommandID)
		return nil, false
	}

	if len(req.ChunkData) > 0 {
		sum := fmt.Sprintf("%x", md5.Sum(req.ChunkData))
		if req.ChunkMD5 != "" && sum != req.ChunkMD5 {
			return t, false // NACK
		}
		if _, exists := t.Chunks[req.ChunkSeq]; !exists {
			t.Chunks[req.ChunkSeq] = append([]byte(nil), req.ChunkData...)
			t.total += int64(len(req.ChunkData))
			if req.ChunkSeq > t.maxSeq {
				t.maxSeq = req.ChunkSeq
			}
		}
	}

	// 判断是否收齐
	if t.FileSize > 0 && t.total >= t.FileSize {
		t.done = true
		return t, true
	}
	return t, req.Done
}

// Task 返回任务。
func (m *PushManager) Task(commandID string) *pushTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[commandID]
}

// Remove 移除任务。
func (m *PushManager) Remove(commandID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, commandID)
}

// TotalBytes 返回已缓存字节数。
func (t *pushTask) TotalBytes() int64 {
	if t == nil {
		return 0
	}
	return t.total
}

// Size 返回文件总大小。
func (t *pushTask) Size() int64 {
	if t == nil {
		return 0
	}
	return t.FileSize
}

// OrderedChunks 返回按序排列的分片数据（不含最后收尾标记）。
func (t *pushTask) OrderedChunks() [][]byte {
	out := make([][]byte, 0, t.maxSeq+1)
	for i := 0; i <= t.maxSeq; i++ {
		if c, ok := t.Chunks[i]; ok {
			out = append(out, c)
		}
	}
	return out
}

// SaveToTemp 将缓存的分片合并落盘到服务端临时目录（供审计/留存）。
func (m *PushManager) SaveToTemp(commandID string, baseDir string) (string, error) {
	t := m.Task(commandID)
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	_ = os.MkdirAll(baseDir, 0700)
	// 与 filetransfer.localPath 同一套净化逻辑：AgentID 来自报文，不能直接拼进路径
	safeID := sanitizeComponent(t.AgentID)
	safe := sanitizeComponent(filepath.Base(filepath.Clean(t.FilePath)))
	path := filepath.Join(baseDir, safeID+"_"+safe)
	if !within(baseDir, path) {
		return "", fmt.Errorf("非法的落盘路径")
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, chunk := range t.OrderedChunks() {
		if _, err := f.Write(chunk); err != nil {
			return "", err
		}
	}
	log.Printf("[filetransfer] 文件分发 %s 已缓存到 %s", t.CommandID, path)
	return path, nil
}
