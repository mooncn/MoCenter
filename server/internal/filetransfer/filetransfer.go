// Package filetransfer 实现服务端文件传输管理。
//
// 支持文件上传与下载的分片收发、MD5 校验、ACK/NACK 确认与断点续传。
// - 上传：Agent 分片回传 -> 服务端按分片落盘并做 MD5 校验，回 ACK/NACK。
// - 下载：Web 请求 -> 服务端向 Agent 下发 FileDownloadReq -> Agent 回传分片。
package filetransfer

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"mocenter/server/internal/protocol"
)

// DefaultChunkSize 默认分片大小（与服务端/Agent 协议一致）。
const DefaultChunkSize = 64 * 1024

// Manager 文件传输管理器。
type Manager struct {
	mu     sync.RWMutex
	// uploads 记录进行中的上传任务（含断点续传的已完成分片）
	uploads map[string]*uploadTask
	// baseDir 服务端接收目录
	baseDir string
}

type uploadTask struct {
	agentID    string
	filePath   string
	localPath  string
	fileSize   int64
	received   int64
	chunks     map[int][]byte // 已收到的分片（用于乱序/断点续传）
	maxSeq     int
	done       bool
}

// NewManager 创建文件传输管理器。
func NewManager(baseDir string) *Manager {
	_ = os.MkdirAll(baseDir, 0755)
	return &Manager{
		uploads: make(map[string]*uploadTask),
		baseDir: baseDir,
	}
}

// taskKey 生成上传任务键。
func taskKey(agentID, filePath string) string {
	return agentID + ":" + filePath
}

// localPath 返回服务端本地存储路径。
func (m *Manager) localPath(agentID, filePath string) string {
	safe := filepath.Base(filepath.Clean(filePath))
	return filepath.Join(m.baseDir, agentID+"_"+safe)
}

// HandleUploadChunk 处理 Agent 上传的分片，返回 ACK/NACK。
func (m *Manager) HandleUploadChunk(req *protocol.FileUploadReq) *protocol.ChunkAck {
	key := taskKey(req.AgentID, req.FilePath)
	ack := &protocol.ChunkAck{FilePath: req.FilePath, ChunkSeq: req.ChunkSeq, Ack: false}

	if req.FileSize < 0 {
		// 文件打开失败通知
		log.Printf("[filetransfer] Agent %s 上传文件 %s 失败", req.AgentID, req.FilePath)
		return ack
	}

	m.mu.Lock()
	t, ok := m.uploads[key]
	if !ok {
		t = &uploadTask{
			agentID:   req.AgentID,
			filePath:  req.FilePath,
			localPath: m.localPath(req.AgentID, req.FilePath),
			fileSize:  req.FileSize,
			chunks:    make(map[int][]byte),
		}
		m.uploads[key] = t
	}
	// MD5 校验
	if len(req.ChunkData) > 0 {
		sum := fmt.Sprintf("%x", md5.Sum(req.ChunkData))
		if req.ChunkMD5 != "" && sum != req.ChunkMD5 {
			m.mu.Unlock()
			return ack // NACK
		}
		// 断点续传：若该分片已接收则跳过
		if _, exists := t.chunks[req.ChunkSeq]; exists {
			m.mu.Unlock()
			ack.Ack = true
			return ack
		}
		t.chunks[req.ChunkSeq] = append([]byte(nil), req.ChunkData...)
		t.received += int64(len(req.ChunkData))
		if req.ChunkSeq > t.maxSeq {
			t.maxSeq = req.ChunkSeq
		}
	}
	ack.Ack = true
	m.mu.Unlock()

	// 全部接收完成则合并落盘
	m.tryAssemble(t)
	return ack
}

// tryAssemble 当所有分片到齐后按序合并写盘。
func (m *Manager) tryAssemble(t *uploadTask) {
	m.mu.Lock()
	if t.done || t.fileSize == 0 {
		m.mu.Unlock()
		return
	}
	// 判断是否齐了：按序累加长度 == fileSize
	var total int64
	for i := 0; i <= t.maxSeq; i++ {
		if _, ok := t.chunks[i]; !ok {
			m.mu.Unlock()
			return // 尚有缺失分片
		}
		total += int64(len(t.chunks[i]))
	}
	if total != t.fileSize {
		m.mu.Unlock()
		return
	}
	t.done = true
	chunks := make([][]byte, 0, t.maxSeq+1)
	for i := 0; i <= t.maxSeq; i++ {
		chunks = append(chunks, t.chunks[i])
	}
	delete(m.uploads, taskKey(t.agentID, t.filePath))
	m.mu.Unlock()

	// 合并写盘
	f, err := os.OpenFile(t.localPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[filetransfer] 打开 %s 失败: %v", t.localPath, err)
		return
	}
	defer f.Close()
	for _, c := range chunks {
		if _, err := f.Write(c); err != nil {
			log.Printf("[filetransfer] 写入 %s 失败: %v", t.localPath, err)
			return
		}
	}
	log.Printf("[filetransfer] 文件上传完成: %s -> %s (%d bytes)", t.filePath, t.localPath, t.fileSize)
}

// HandleFileDownload 处理 Agent 下载回传的分片（复用上传逻辑）。
func (m *Manager) HandleFileDownload(req *protocol.FileUploadReq) {
	// 与上传共用分片落盘逻辑
	_ = m.HandleUploadChunk(req)
}

// ReadFileChunks 读取本地文件分片（用于服务端向 Web 提供下载）。
func (m *Manager) ReadFileChunks(agentID, filePath string, startSeq int, onChunk func(seq int, data []byte)) (int64, error) {
	path := m.localPath(agentID, filePath)
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, _ := f.Stat()
	buf := make([]byte, DefaultChunkSize)
	seq := 0
	for {
		if seq < startSeq {
			// 跳过已读分片（断点续传），按块跳过
			skip := int64(DefaultChunkSize)
			if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}
			seq++
			continue
		}
		n, err := f.Read(buf)
		if n > 0 {
			onChunk(seq, buf[:n])
			seq++
		}
		if err != nil {
			break
		}
	}
	return st.Size(), nil
}
