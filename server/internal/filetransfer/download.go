package filetransfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errDownloadCancelled 下载被取消/超时，等待方据此立刻返回而不是继续挂起。
var errDownloadCancelled = errors.New("下载已取消或超时")

// downloadWaiter 一次"等待 Agent 回传文件落盘"的等待位。
//
// 独立于 uploadTask 存在：任务在收齐/失败/超时后会从 map 摘除，
// 而等待方（HTTP 请求）必须且只能收到一次结果。
type downloadWaiter struct {
	ch   chan error
	once sync.Once
}

func (w *downloadWaiter) notify(err error) {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.ch <- err
		close(w.ch)
	})
}

// DownloadHandle 下载等待句柄。
type DownloadHandle struct {
	ch        chan error
	LocalPath string // 落盘位置（完成后由调用方负责读取并删除）
}

// Wait 等待 Agent 回传完成；超时返回错误（任务会被 CancelDownload 回收）。
func (h *DownloadHandle) Wait(timeout time.Duration) error {
	select {
	case err := <-h.ch:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("等待文件回传超时（%s）", timeout)
	}
}

// ExpectDownload 预先登记一次下载，返回等待句柄。
//
// 必须在向 Agent 发出 FileDownloadReq **之前**调用：文件很小、链路很快时，
// 第一片可能在登记之前就到达，那样服务端会因为"没有任务"而把它当成一次
// 普通上传，等待方则永远等不到完成信号。
//
// 落盘路径与普通上传分开（dl_<时间戳>_<agent>_<文件名>）：
// 两者同名时不会互相覆盖，下载完成后由调用方删除临时文件即可。
func (m *Manager) ExpectDownload(agentID, filePath string) (*DownloadHandle, error) {
	key := taskKey(agentID, filePath)
	local := filepath.Join(m.baseDir,
		fmt.Sprintf("dl_%d_%s_%s", time.Now().UnixNano(), sanitizeComponent(agentID), sanitizeComponent(filepath.Base(filepath.Clean(filePath)))))
	if !within(m.baseDir, local) {
		return nil, fmt.Errorf("非法的下载落盘路径")
	}
	w := &downloadWaiter{ch: make(chan error, 1)}

	m.mu.Lock()
	if old, ok := m.uploads[key]; ok {
		// 同路径的上一次下载/上传残留：直接丢弃，避免新旧分片混在一起。
		delete(m.uploads, key)
		m.stored -= old.received
		if old.localPath != "" {
			_ = os.Remove(old.localPath)
		}
	}
	m.uploads[key] = &uploadTask{
		agentID:    agentID,
		filePath:   filePath,
		localPath:  local,
		chunks:     make(map[int][]byte),
		lastActive: time.Now(),
		expectSize: true,
		wait:       w,
	}
	m.downloadWaiters[key] = w
	m.mu.Unlock()
	return &DownloadHandle{ch: w.ch, LocalPath: local}, nil
}

// CancelDownload 取消等待并回收半截任务与落盘文件（超时/客户端断开时调用）。
func (m *Manager) CancelDownload(agentID, filePath string) {
	key := taskKey(agentID, filePath)
	m.mu.Lock()
	t := m.uploads[key]
	delete(m.uploads, key)
	if t != nil {
		m.stored -= t.received
	}
	w := m.downloadWaiters[key]
	delete(m.downloadWaiters, key)
	m.mu.Unlock()

	w.notify(errDownloadCancelled)
	if t != nil && t.localPath != "" {
		_ = os.Remove(t.localPath)
	}
}

// failDownload 当 Agent 明确回报"读不到这个文件"时，让等待方立刻拿到失败原因。
// 调用方不持锁。
func (m *Manager) failDownload(key string, err error) {
	m.mu.Lock()
	t := m.uploads[key]
	delete(m.uploads, key)
	if t != nil {
		m.stored -= t.received
	}
	w := m.downloadWaiters[key]
	delete(m.downloadWaiters, key)
	m.mu.Unlock()

	if w == nil {
		return
	}
	w.notify(err)
	if t != nil && t.localPath != "" {
		_ = os.Remove(t.localPath)
	}
}

// cleanupWaiterForLocked 在回收任务时取出等待位（调用方须持有 m.mu）。
func (m *Manager) cleanupWaiterForLocked(key string) *downloadWaiter {
	w := m.downloadWaiters[key]
	delete(m.downloadWaiters, key)
	return w
}
