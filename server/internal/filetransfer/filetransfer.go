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
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// DefaultChunkSize 默认分片大小（与服务端/Agent 协议一致）。
const DefaultChunkSize = 64 * 1024

// 上传任务的资源闸门（M-8）。
//
// 分片在服务端是**全量驻内存**的（chunks map），要等所有分片到齐才落盘。
// 加固前没有任何上限：一条被拿下的 Agent 只要
//   - 不断换 FilePath 开新任务 → 任务条目无界增长；
//   - 只开任务不传完 → 半截分片永不回收；
//   - 单任务猛灌 → 内存直接打满；
//
// 三条路都能把服务端吃死。这里给条目数、单任务字节、总在途字节和闲置时间
// 各上一道闸。阈值按"单人自用的运维用量"取：正常一次上传是几十 MB 的日志或
// 安装包，离上限很远。
const (
	// maxActiveUploadTasks 同时在途的上传任务数上限。
	maxActiveUploadTasks = 64
	// maxUploadTaskBytes 单个上传任务可暂存的最大字节数。
	maxUploadTaskBytes = 256 << 20 // 256 MiB
	// maxStoredBytes 全部在途任务暂存字节的上限（内存总量的闸门）。
	maxStoredBytes = 512 << 20 // 512 MiB
	// uploadTaskTTL 任务闲置超过这个时间即视为中断，回收其分片。
	// 取 30 分钟：断点续传的重试间隔量级在秒到分钟，再长就不是"慢"而是"死了"。
	uploadTaskTTL = 30 * time.Minute
)

// Manager 文件传输管理器。
type Manager struct {
	mu     sync.RWMutex
	// uploads 记录进行中的上传任务（含断点续传的已完成分片）
	uploads map[string]*uploadTask
	// stored 全部在途任务当前暂存的字节数（受 mu 保护）
	stored int64
	// lastRejectLog 上一次因超限拒绝的留痕时间：超限请求可以高频重放，
	// 不限频的话"挡滥用"的闸门本身会变成刷日志的入口。
	lastRejectLog time.Time
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
	lastActive time.Time // 最近一次收到分片的时刻，用于 TTL 回收
}

// NewManager 创建文件传输管理器。
//
// 目录权限 0700：这里面是 Agent 上传/下传的文件内容，不应让同机其他用户读到。
func NewManager(baseDir string) *Manager {
	_ = os.MkdirAll(baseDir, 0700)
	return &Manager{
		uploads: make(map[string]*uploadTask),
		baseDir: baseDir,
	}
}

// taskKey 生成上传任务键。
func taskKey(agentID, filePath string) string {
	return agentID + ":" + filePath
}

// sanitizeComponent 把任意字符串收敛成一个安全的**单层文件名片段**。
//
// 为什么必须做：落盘路径由 agentID 与 filePath 拼成，而 agentID 来自
// TCP 报文（Agent 自报），历史上未做任何净化 —— 实测 agentID 取
// "../../etc/cron.d/mocenter" 可让落盘路径逃出 baseDir，
// 由于服务端通常以 root/LocalSystem 运行，等同于任意文件写入。
//
// 策略：白名单字符 [A-Za-z0-9._-]，其余一律替换为 '_'。
// 用「替换」而不是「拒绝」，是为了不打断中文主机名等既有用法
// （代价是文件名可能被改写，但 FilePath 原值仍保留在任务里用于展示）。
func sanitizeComponent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	// 纯点（"." / ".." / "..."）会被文件系统当成目录语义，前面补一个下划线
	if strings.Trim(out, ".") == "" {
		out = "_" + out
	}
	return out
}

// within 断言 p 位于 root 之内（兜底防线：即使净化逻辑将来被改坏也不会越界）。
func within(root, p string) bool {
	rootAbs, err1 := filepath.Abs(root)
	pAbs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// localPath 返回服务端本地存储路径；路径不安全时返回 ""。
func (m *Manager) localPath(agentID, filePath string) string {
	safeID := sanitizeComponent(agentID)
	safeFile := sanitizeComponent(filepath.Base(filepath.Clean(filePath)))
	p := filepath.Join(m.baseDir, safeID+"_"+safeFile)
	if !within(m.baseDir, p) {
		return ""
	}
	return p
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
		if len(m.uploads) >= maxActiveUploadTasks {
			if m.shouldLogReject() {
				log.Printf("[filetransfer] 拒绝 %s 的新任务 %s：在途上传任务已达上限 %d（可能有主机在异常刷上传）",
					req.AgentID, req.FilePath, maxActiveUploadTasks)
			}
			m.mu.Unlock()
			return ack // NACK
		}
		if req.FileSize > maxUploadTaskBytes {
			if m.shouldLogReject() {
				log.Printf("[filetransfer] 拒绝 %s 的上传 %s：声明大小 %d 超过单任务上限 %d",
					req.AgentID, req.FilePath, req.FileSize, maxUploadTaskBytes)
			}
			m.mu.Unlock()
			return ack // NACK
		}
		lp := m.localPath(req.AgentID, req.FilePath)
		if lp == "" {
			m.mu.Unlock()
			log.Printf("[filetransfer] 拒绝上传：AgentID/FilePath 非法 agent=%q file=%q", req.AgentID, req.FilePath)
			return ack // NACK
		}
		t = &uploadTask{
			agentID:    req.AgentID,
			filePath:   req.FilePath,
			localPath:  lp,
			fileSize:   req.FileSize,
			chunks:     make(map[int][]byte),
			lastActive: time.Now(),
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
		n := int64(len(req.ChunkData))
		// 分片是全量驻内存的（要等齐了才落盘），所以"还能再收多少"必须在这里判：
		// 单任务上限挡住一路灌到底，总量上限挡住多条任务合力吃满内存。
		if t.received+n > maxUploadTaskBytes || m.stored+n > maxStoredBytes {
			if m.shouldLogReject() {
				log.Printf("[filetransfer] 拒绝 %s 的续传分片 %s#%d：本任务已存 %d、全部在途已存 %d，超过上限（%d/%d）",
					req.AgentID, req.FilePath, req.ChunkSeq, t.received, m.stored, maxUploadTaskBytes, maxStoredBytes)
			}
			m.mu.Unlock()
			return ack // NACK
		}
		t.chunks[req.ChunkSeq] = append([]byte(nil), req.ChunkData...)
		t.received += n
		m.stored += n
		t.lastActive = time.Now()
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

// shouldLogReject 报告此刻是否该留痕一条超限拒绝（每分钟最多一条）。
// 超限请求是可以被高频重放的，不限频就等于"挡滥用的闸门"自己成了刷日志的入口。
// 调用方须持有 m.mu。
func (m *Manager) shouldLogReject() bool {
	now := time.Now()
	if now.Sub(m.lastRejectLog) < time.Minute {
		return false
	}
	m.lastRejectLog = now
	return true
}

// CleanupStale 回收闲置超过 uploadTaskTTL 的在途任务，返回回收条数。
//
// 断点续传允许"传一半待会儿再传"，但不允许"永远占着"：Agent 中途放弃、被卸载
// 或被拿下的主机乱开任务之后，那些半截分片必须有人收 —— 加固前任务表只会单调
// 增长，且分片全在内存里。
func (m *Manager) CleanupStale() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-uploadTaskTTL)
	var n int
	for key, t := range m.uploads {
		if t.done || !t.lastActive.After(cutoff) {
			delete(m.uploads, key)
			m.stored -= t.received
			n++
		}
	}
	if n > 0 {
		log.Printf("[filetransfer] 回收 %d 个超过 %v 未完成的上传任务", n, uploadTaskTTL)
	}
	return n
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
	// 分片即将交给磁盘，暂存额度同步释放 —— 不释放的话闸门会被
	// 一次次成功的上传吃满，最后反而把正常请求挡在门外。
	m.stored -= total
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
	if path == "" {
		return 0, fmt.Errorf("非法的文件标识")
	}
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
