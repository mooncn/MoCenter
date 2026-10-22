// Package filetransfer 文件分发管理器：Web 上传文件分发到 Agent。
//
// 流程：Web 将文件分片上传到服务端 -> 服务端按分片缓存并做 MD5 校验 ->
// 全部到齐后由服务端向目标 Agent 依次推送分片 -> Agent 落盘并回 ACK。
// 服务端作为中转与缓冲，可应对 Agent 短暂离线场景。
package filetransfer

import (
	"crypto/md5"
	"fmt"
	"mocenter/server/internal/logx"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// 分发任务的资源闸门（L-1）。
//
// 上传侧（Manager）在 M-8 就有了四道闸，分发侧一直缺：任务按客户端自报的
// command_id 建条目、分片全驻内存，只有"推给 Agent 成功之后"才有人删。
// 于是任一有效会话（包括一枚泄漏的 Token）都能
//   - 不断换 command_id 开新任务 → 条目无界；
//   - 开完不传完 → 半截分片永不回收；
//   - 单任务猛灌 → 内存直接打满。
//
// 阈值与上传侧同一口径：分片收齐前全量驻内存，单任务 64 MiB、总量 128 MiB。
// 典型载荷（7–15 MiB 的 Agent 包、几十 MB 的日志）离上限很远，但足够
// 把「多台机器同时分发」的内存峰值压住。
//
// 用 var 只为测试能便宜地触发这些分支（真灌 64 MiB 进单测不合理）；
// 运行期没有任何入口能改它们。
var (
	maxActivePushTasks = 64
	maxPushTaskBytes   = int64(64 << 20)
	maxPushStoredBytes = int64(128 << 20)
	pushTaskTTL        = 30 * time.Minute
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
	// lastActive 最近一次收到分片的时刻，用于 TTL 回收（与上传任务同语义）。
	lastActive time.Time
}

// PushManager 管理 Web -> Server -> Agent 的文件分发。
type PushManager struct {
	mu    sync.Mutex
	tasks map[string]*pushTask // key = commandID
	// stored 全部在途任务当前暂存的字节数（受 mu 保护）
	stored int64
	// lastRejectLog 超限拒绝的留痕节流：超限请求可以被高频重放，
	// 不限频的话"挡滥用"的闸门自己就成了刷日志的入口。
	lastRejectLog time.Time
}

// NewPushManager 创建文件分发管理器。
func NewPushManager() *PushManager {
	return &PushManager{tasks: make(map[string]*pushTask)}
}

// UploadChunk 接收 Web 上传的分片并缓存，返回是否已收齐及目标信息。
//
// err 非空表示本次请求已被拒绝（超限）：任务已从表中摘除、暂存额度一并释放，
// 调用方据此把"失败 + 原因"如实报给 Web，而不是让它静默停在半截。
// 返回 (nil, false, nil) 表示"分发被取消/失败通知"，与旧口径一致。
func (m *PushManager) UploadChunk(req *protocol.FilePushReq) (*pushTask, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[req.CommandID]
	if !ok {
		if req.FileSize < 0 {
			return nil, false, nil // 分发失败
		}
		if len(m.tasks) >= maxActivePushTasks {
			m.logRejectLocked(fmt.Sprintf("拒绝 %s 的新分发：在途分发任务已达上限 %d（可能有会话在异常刷分发）",
				req.AgentID, maxActivePushTasks))
			return nil, false, fmt.Errorf("在途分发任务已达上限 %d，请稍后重试", maxActivePushTasks)
		}
		if req.FileSize > maxPushTaskBytes {
			m.logRejectLocked(fmt.Sprintf("拒绝 %s 的分发 %s：声明大小 %d 超过单任务上限 %d",
				req.AgentID, req.FilePath, req.FileSize, maxPushTaskBytes))
			return nil, false, fmt.Errorf("单次分发不得超过 %d 字节", maxPushTaskBytes)
		}
		t = &pushTask{
			CommandID:  req.CommandID,
			AgentID:    req.AgentID,
			FilePath:   req.FilePath,
			FileSize:   req.FileSize,
			Chunks:     make(map[int][]byte),
			lastActive: time.Now(),
		}
		m.tasks[req.CommandID] = t
	}

	if req.FileSize < 0 {
		// 取消分发
		m.deleteLocked(t)
		return nil, false, nil
	}
	t.lastActive = time.Now()

	if len(req.ChunkData) > 0 {
		sum := fmt.Sprintf("%x", md5.Sum(req.ChunkData))
		if req.ChunkMD5 != "" && sum != req.ChunkMD5 {
			return t, false, nil // NACK
		}
		if _, exists := t.Chunks[req.ChunkSeq]; !exists {
			// 分片是全量驻内存的（要等齐了才推给 Agent），所以"还能再收多少"必须在这里判：
			// 单任务上限挡住一路灌到底，总量上限挡住多条任务合力吃满内存。
			n := int64(len(req.ChunkData))
			if t.total+n > maxPushTaskBytes || m.stored+n > maxPushStoredBytes {
				m.logRejectLocked(fmt.Sprintf("拒绝 %s 的续传分片 %s#%d：本任务已存 %d、全部在途已存 %d，超过上限（%d/%d）",
					req.AgentID, req.FilePath, req.ChunkSeq, t.total, m.stored, maxPushTaskBytes, maxPushStoredBytes))
				m.deleteLocked(t)
				return nil, false, fmt.Errorf("分发内容超过容量上限（单任务 %d / 全部在途 %d 字节），已中止本次分发",
					maxPushTaskBytes, maxPushStoredBytes)
			}
			t.Chunks[req.ChunkSeq] = append([]byte(nil), req.ChunkData...)
			t.total += n
			m.stored += n
			if req.ChunkSeq > t.maxSeq {
				t.maxSeq = req.ChunkSeq
			}
		}
	}

	// 判断是否收齐
	if t.FileSize > 0 && t.total >= t.FileSize {
		t.done = true
		return t, true, nil
	}
	return t, req.Done, nil
}

// logRejectLocked 超限拒绝的留痕：每分钟最多一条（调用方须持有 m.mu）。
func (m *PushManager) logRejectLocked(reason string) {
	now := time.Now()
	if !m.lastRejectLog.IsZero() && now.Sub(m.lastRejectLog) < time.Minute {
		return
	}
	m.lastRejectLog = now
	logx.Infof("[filetransfer] %s", reason)
}

// deleteLocked 从表中摘除任务并释放其暂存额度（调用方须持有 m.mu）。
func (m *PushManager) deleteLocked(t *pushTask) {
	if t == nil {
		return
	}
	if _, ok := m.tasks[t.CommandID]; !ok {
		return
	}
	delete(m.tasks, t.CommandID)
	m.stored -= t.total
}

// CleanupStale 回收闲置超过 pushTaskTTL 的分发任务，返回回收条数。
//
// 分发允许"传一半待会儿再传"，但不允许"永远占着"：目标主机一直离线、
// 或操作员中途关掉页面时，那些半截分片必须有人收（加固前这张表只会单调增长）。
func (m *PushManager) CleanupStale() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-pushTaskTTL)
	var n int
	for id, t := range m.tasks {
		if t.done || !t.lastActive.After(cutoff) {
			delete(m.tasks, id)
			m.stored -= t.total
			n++
		}
	}
	if n > 0 {
		logx.Infof("[filetransfer] 回收 %d 个超过 %v 未完成的分发任务", n, pushTaskTTL)
	}
	return n
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
	if t, ok := m.tasks[commandID]; ok {
		delete(m.tasks, commandID)
		m.stored -= t.total
	}
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
	logx.Infof("[filetransfer] 文件分发 %s 已缓存到 %s", t.CommandID, path)
	return path, nil
}
