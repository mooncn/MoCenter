// 文件管理：按会话浏览并维护 Agent 主机上的整机文件。
//
// 设计取舍：
//   - **整机访问**，不再限制在 -file-roots 沙箱内；默认根为 /（Windows 是盘符列表）。
//     代价由审计与"操作者即管理员"承担，不靠路径闸门。
//   - 列出 / 新建 / 复制 / 移动 / 删除 / 上传 / 下载全部走 Agent 的文件系统调用，
//     而不是拼 shell 命令：省掉跨平台的引号与注入面，能力等价。
//   - **会话制**：无操作 20 分钟自动断开，没有累计时长上限；显式断开立即失效。
//     进行中的传输（busy>0）不会被空闲回收。
//   - 上传/下载都是**流式**：上传边读 HTTP 正文边推给 Agent（不整份驻内存），
//     下载由 Agent 分片回传、服务端落临时文件后随响应删除。
package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/filetransfer"
	"mocenter/server/internal/logx"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

const (
	// fileSessionIdle 无操作多久自动断开。只有空闲这一个时间闸门：
	// 整机文件管理是长任务（翻目录、传大文件），按累计时长强制断开只会把人从
	// 中途踢出去，因此不设会话总上限。
	fileSessionIdle = 20 * time.Minute
	// maxFileSessions 全局并发会话上限（单人自用，留足排障余量即可）。
	maxFileSessions = 8
	// maxFileSessionsPerAgent 同一台主机最多允许两个会话：一个浏览、一个传输互不影响。
	maxFileSessionsPerAgent = 2
	// maxFileUploadBytes 单文件上传上限，与文件分发/接收侧 64MiB 口径一致。
	maxFileUploadBytes = 64 << 20
	// maxFileDownloadBytes 单文件下载上限（filetransfer 侧同口径再挡一次）。
	maxFileDownloadBytes = 64 << 20
	fileListTimeout      = 8 * time.Second
	// fsOpTimeout 复制/移动/删除要等 Agent 完成磁盘操作；大目录可能耗时较久。
	fsOpTimeout         = 60 * time.Second
	fileDownloadTimeout = 60 * time.Second
	maxFilePathLen      = 4096
)

var (
	errFileSessionNotFound = errors.New("文件会话不存在或已断开，请重新连接")
	errFileSessionExpired  = errors.New("文件会话已超时，请重新连接")
)

// fileSession 一次"打开某台主机的文件管理"。
type fileSession struct {
	ID         string
	AgentID    string
	Operator   string
	Roots      []string
	Root       string
	LastActive time.Time
	busy       int
}

// fileSessionView 返回给处理函数的只读快照。
type fileSessionView struct {
	ID         string
	AgentID    string
	Operator   string
	Roots      []string
	Root       string
	LastActive time.Time
}

type fileSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*fileSession
}

func newFileSessionManager() *fileSessionManager {
	return &fileSessionManager{sessions: make(map[string]*fileSession)}
}

func fileRandomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("f%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// create 新建会话。调用方已完成"向 Agent 取根目录"的步骤。
func (m *fileSessionManager) create(agentID, operator string, roots []string) (*fileSessionView, error) {
	if len(roots) == 0 {
		return nil, errors.New("该主机没有可访问的文件根目录")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	perAgent := 0
	for id, s := range m.sessions {
		if now.Sub(s.LastActive) > fileSessionIdle {
			delete(m.sessions, id)
			continue
		}
		if s.AgentID == agentID {
			perAgent++
		}
	}
	if len(m.sessions) >= maxFileSessions {
		return nil, fmt.Errorf("同时打开的文件会话已达上限（%d 个），请先关闭不用的会话", maxFileSessions)
	}
	if perAgent >= maxFileSessionsPerAgent {
		return nil, fmt.Errorf("该主机已有 %d 个文件会话，请先关闭", maxFileSessionsPerAgent)
	}
	s := &fileSession{
		ID: fileRandomID(), AgentID: agentID, Operator: operator,
		Roots: append([]string(nil), roots...), Root: roots[0],
		LastActive: now,
	}
	m.sessions[s.ID] = s
	return s.view(), nil
}

func (s *fileSession) view() *fileSessionView {
	return &fileSessionView{
		ID: s.ID, AgentID: s.AgentID, Operator: s.Operator,
		Roots: append([]string(nil), s.Roots...), Root: s.Root,
		LastActive: s.LastActive,
	}
}

// acquire 校验并刷新会话；op=true 时计入进行中操作（空闲回收会跳过它）。
// operator 必须与会话创建者一致：即便将来引入多账号，也不能跨操作员接管会话。
func (m *fileSessionManager) acquire(id, operator string, op, touch bool) (*fileSessionView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.Operator != operator {
		return nil, errFileSessionNotFound
	}
	now := time.Now()
	if now.Sub(s.LastActive) > fileSessionIdle {
		delete(m.sessions, id)
		return nil, errFileSessionExpired
	}
	if touch {
		s.LastActive = now
	}
	if op {
		s.busy++
	}
	return s.view(), nil
}

// release 结束一次进行中操作。
func (m *fileSessionManager) release(id string) {
	m.mu.Lock()
	if s := m.sessions[id]; s != nil && s.busy > 0 {
		s.busy--
		s.LastActive = time.Now()
	}
	m.mu.Unlock()
}

func (m *fileSessionManager) close(id, operator string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.Operator != operator {
		return false
	}
	delete(m.sessions, id)
	return true
}

// sweep 回收空闲会话，返回回收数量。进行中的传输（busy>0）不回收，
// 一轮传输结束后算一次活动，计时重新开始。
func (m *fileSessionManager) sweep(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if now.Sub(s.LastActive) > fileSessionIdle && s.busy == 0 {
			delete(m.sessions, id)
			n++
		}
	}
	return n
}

func fileSessionSweeper(m *fileSessionManager) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for now := range t.C {
		if n := m.sweep(now); n > 0 {
			logx.Infof("[files] 回收 %d 个空闲的文件会话", n)
		}
	}
}

// fsOpRegistry 收集 Agent 对 FsOpReq 的响应。
type fsOpRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan protocol.FsOpResp
}

var fsOpReqs = &fsOpRegistry{waiters: make(map[string]chan protocol.FsOpResp)}

func (r *fsOpRegistry) begin(id string) chan protocol.FsOpResp {
	ch := make(chan protocol.FsOpResp, 1)
	r.mu.Lock()
	r.waiters[id] = ch
	r.mu.Unlock()
	return ch
}

func (r *fsOpRegistry) end(id string, ch chan protocol.FsOpResp) {
	r.mu.Lock()
	if r.waiters[id] == ch {
		delete(r.waiters, id)
	}
	r.mu.Unlock()
}

func (r *fsOpRegistry) deliver(resp protocol.FsOpResp) {
	r.mu.Lock()
	ch := r.waiters[resp.RequestID]
	r.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// requestFsOp 向 Agent 发起一次文件操作并等待结果。
func requestFsOp(conn *session.Conn, req protocol.FsOpReq, timeout time.Duration) (protocol.FsOpResp, error) {
	if req.RequestID == "" {
		req.RequestID = fileRandomID()
	}
	ch := fsOpReqs.begin(req.RequestID)
	defer fsOpReqs.end(req.RequestID, ch)
	if err := conn.Write(protocol.TypeFsOpReq, req); err != nil {
		return protocol.FsOpResp{}, fmt.Errorf("下发文件操作失败: %w", err)
	}
	select {
	case resp := <-ch:
		if !resp.OK {
			msg := strings.TrimSpace(resp.Message)
			if msg == "" {
				msg = "Agent 拒绝了该操作"
			}
			return resp, errors.New(msg)
		}
		return resp, nil
	case <-time.After(timeout):
		return protocol.FsOpResp{}, fmt.Errorf("Agent 未在时限内返回结果（%s）", timeout)
	}
}

// fsAbsolutePath 服务端侧路径校验：整机模式只要求绝对路径。
func fsAbsolutePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("路径为空")
	}
	if len(p) > maxFilePathLen || strings.ContainsRune(p, 0) {
		return "", errors.New("路径不合法")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("必须是绝对路径")
	}
	return filepath.Clean(p), nil
}

// fsListPath 列目录允许 "/"（Windows 表示盘符列表，Unix 表示根目录）。
func fsListPath(p, fallback string) (string, error) {
	if strings.TrimSpace(p) == "" {
		p = fallback
	}
	if strings.TrimSpace(p) == "/" {
		return "/", nil
	}
	return fsAbsolutePath(p)
}

// fileSessionHandler GET(状态) / POST(新建) /api/files/session
func fileSessionHandler(m *fileSessionManager, sess *session.Manager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodGet:
			id := strings.TrimSpace(r.URL.Query().Get("session"))
			// 状态查询不刷新活跃时间：否则页面每 30 秒轮询一次，
			// "无操作 20 分钟自动断开"将永远不会触发。
			view, err := m.acquire(id, operator, false, false)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{
				"ok": true, "session_id": view.ID, "agent_id": view.AgentID,
				"roots": view.Roots, "root": view.Root,
				"idle_seconds": int(fileSessionIdle.Seconds()),
				"idle_left":    int(time.Until(view.LastActive.Add(fileSessionIdle)).Seconds()),
			})
		case http.MethodPost:
			var body struct {
				AgentID string `json:"agent_id"`
			}
			if !decodeJSON(w, r, maxJSONBody, &body) {
				return
			}
			agentID := strings.TrimSpace(body.AgentID)
			if agentID == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "请选择主机"})
				return
			}
			conn := sess.Get(agentID)
			if conn == nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机不在线，无法打开文件管理"})
				return
			}
			rootsResp, err := requestFsOp(conn, protocol.FsOpReq{Op: "roots"}, fileListTimeout)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "读取文件系统根失败：" + err.Error()})
				return
			}
			roots := make([]string, 0, len(rootsResp.Entries))
			for _, e := range rootsResp.Entries {
				if e.Dir {
					roots = append(roots, e.Path)
				}
			}
			view, err := m.create(agentID, operator, roots)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			if audit != nil {
				audit(r, fmt.Sprintf("打开文件管理：%s（整机访问，默认 %s）", agentID, view.Root))
			}
			logx.Infof("[files] 打开文件会话 %s：agent=%s root=%s", view.ID, agentID, view.Root)
			writeJSON(w, map[string]interface{}{
				"ok": true, "session_id": view.ID, "agent_id": view.AgentID,
				"roots": view.Roots, "root": view.Root,
				"idle_seconds": int(fileSessionIdle.Seconds()),
				"msg":          "文件管理会话已建立",
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// fileSessionCloseHandler POST /api/files/session/close —— 主动断开。
func fileSessionCloseHandler(m *fileSessionManager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Session string `json:"session"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		ok := m.close(strings.TrimSpace(body.Session), operator)
		if ok && audit != nil {
			audit(r, "关闭文件管理会话 "+body.Session)
		}
		writeJSON(w, map[string]interface{}{"ok": ok})
	}
}

// fileListHandler GET /api/files/list?session=&path=
func fileListHandler(m *fileSessionManager, sess *session.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		id := strings.TrimSpace(r.URL.Query().Get("session"))
		view, err := m.acquire(id, operator, true, true)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		defer m.release(id)
		path, err := fsListPath(strings.TrimSpace(r.URL.Query().Get("path")), view.Root)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		conn := sess.Get(view.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机已离线，文件会话结束"})
			return
		}
		resp, err := requestFsOp(conn, protocol.FsOpReq{Op: "list", Path: path, WithDisk: true}, fileListTimeout)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "path": resp.Path, "parent": resp.Parent,
			"entries": resp.Entries, "truncated": resp.Truncated,
			"roots": view.Roots, "volumes": resp.Volumes,
		})
	}
}

// fileOpHandler POST /api/files/op —— create / copy / move / delete。
//
// 请求：{"session":"...","op":"create|copy|move|delete","path":"...","dest":"...","is_dir":false}
// 路径为整机绝对路径；文件系统根的删除由 Agent 侧拒绝。
func fileOpHandler(m *fileSessionManager, sess *session.Manager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Session string `json:"session"`
			Op      string `json:"op"`
			Path    string `json:"path"`
			Dest    string `json:"dest"`
			IsDir   bool   `json:"is_dir"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		op := strings.ToLower(strings.TrimSpace(body.Op))
		switch op {
		case "create", "rename", "copy", "move", "delete":
		default:
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "不支持的操作"})
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		view, err := m.acquire(strings.TrimSpace(body.Session), operator, true, true)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		defer m.release(view.ID)

		path, err := fsAbsolutePath(body.Path)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "路径不合法：" + err.Error()})
			return
		}
		req := protocol.FsOpReq{Op: op, Path: path, IsDir: body.IsDir}
		if op == "copy" || op == "move" || op == "rename" {
			dest, derr := fsAbsolutePath(body.Dest)
			if derr != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "目标路径不合法：" + derr.Error()})
				return
			}
			req.Dest = dest
		}
		conn := sess.Get(view.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机已离线，文件会话结束"})
			return
		}
		resp, err := requestFsOp(conn, req, fsOpTimeout)
		if err != nil {
			logx.Warnf("[files] %s 失败 agent=%s path=%s dest=%s: %v", op, view.AgentID, path, req.Dest, err)
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		if audit != nil {
			switch op {
			case "create":
				kind := "文件"
				if body.IsDir {
					kind = "目录"
				}
				audit(r, fmt.Sprintf("新建%s：%s（%s）", kind, path, view.AgentID))
			case "copy":
				audit(r, fmt.Sprintf("复制：%s → %s（%s）", path, req.Dest, view.AgentID))
			case "move":
				audit(r, fmt.Sprintf("移动：%s → %s（%s）", path, req.Dest, view.AgentID))
			case "rename":
				audit(r, fmt.Sprintf("重命名：%s → %s（%s）", path, req.Dest, view.AgentID))
			case "delete":
				audit(r, fmt.Sprintf("删除：%s（%s）", path, view.AgentID))
			}
		}
		logx.Infof("[files] %s 成功 agent=%s path=%s dest=%s", op, view.AgentID, path, req.Dest)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": resp.Message, "path": resp.Path})
	}
}

// fileUploadHandler POST /api/files/upload?session=&dir=&name=（请求体即文件内容）
//
// 用原始正文而不是 multipart：无需把整份文件先解析进内存/临时文件，
// 边读边推给 Agent，64MiB 上限在读取过程中硬性截断。
func fileUploadHandler(m *fileSessionManager, sess *session.Manager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		q := r.URL.Query()
		id := strings.TrimSpace(q.Get("session"))
		view, err := m.acquire(id, operator, true, true)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		defer m.release(id)

		dir, err := fsAbsolutePath(strings.TrimSpace(q.Get("dir")))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "目标目录不合法：" + err.Error()})
			return
		}
		name := filepath.Base(strings.TrimSpace(q.Get("name")))
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "文件名不合法"})
			return
		}
		target, err := fsAbsolutePath(filepath.Join(dir, name))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		conn := sess.Get(view.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机已离线，无法上传"})
			return
		}

		// 先按上限收紧正文，再流式推送；多读 1 字节用于判断"是否超限"。
		body := http.MaxBytesReader(w, r.Body, maxFileUploadBytes+1)
		defer body.Close()
		written, err := streamFileToAgent(conn, view.AgentID, target, body, maxFileUploadBytes)
		if err != nil {
			if errors.Is(err, errFileTooLarge) {
				http.Error(w, "文件超过 64 MiB 上限", http.StatusRequestEntityTooLarge)
				return
			}
			logx.Errorf("[files] 上传 %s 到 %s 失败: %v", name, view.AgentID, err)
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "上传失败：" + err.Error()})
			return
		}
		// 流式推送拿不到"Agent 落盘结果"的直接回执（分片 ACK 只代表收到）。
		// 回读一次目录确认文件确实存在且大小一致：既验证了 Agent 侧路径闸门放行，
		// 也避免界面显示"上传成功"而目标机上根本没有这个文件。
		if err := confirmRemoteFile(conn, target, written); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "已发送，但目标机未确认写入：" + err.Error()})
			return
		}
		if audit != nil {
			audit(r, fmt.Sprintf("文件上传：%s → %s（%d 字节）", name, view.AgentID, written))
		}
		logx.Infof("[files] 文件已上传到 %s：%s（%d 字节）", view.AgentID, target, written)
		writeJSON(w, map[string]interface{}{"ok": true, "path": target, "size": written, "msg": "上传完成"})
	}
}

var errFileTooLarge = errors.New("文件超过上限")

// fileError 以非 200 状态返回 JSON 错误。下载接口必须用它：
// writeJSON 默认 200，前端按 blob 接收时会把错误 JSON 当成文件内容保存。
func fileError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": msg})
}

// streamFileToAgent 把 src 按 64KiB 分片推给 Agent。
//
// 不整份缓存：大文件在服务端只占一个 64KiB 缓冲。中途失败/超限时发一帧
// FileSize=-1，由 Agent 关闭并删除半截文件（see pushAbort）。
func streamFileToAgent(conn *session.Conn, agentID, target string, src io.Reader, limit int64) (int64, error) {
	cmdID := "fm-" + fileRandomID()
	buf := make([]byte, protocol.DefaultChunkSize)
	var total int64
	seq := 0
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if total+int64(n) > limit {
				_ = conn.Write(protocol.TypeFilePushReq, protocol.FilePushReq{
					CommandID: cmdID, AgentID: agentID, FilePath: target, FileSize: -1,
				})
				return total, errFileTooLarge
			}
			chunkData := append([]byte(nil), buf[:n]...)
			err := conn.Write(protocol.TypeFilePushReq, protocol.FilePushReq{
				CommandID: cmdID, AgentID: agentID, FilePath: target,
				FileSize: 0, // 总量未知也不影响 Agent 落盘；Done 帧才收尾
				ChunkSeq: seq, ChunkData: chunkData, ChunkMD5: md5Hex(chunkData),
				// 文件管理是整机访问：跳过 Agent 的 -file-roots 沙箱（仍要求绝对路径）。
				Unrestricted: true,
			})
			if err != nil {
				return total, err
			}
			total += int64(n)
			seq++
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = conn.Write(protocol.TypeFilePushReq, protocol.FilePushReq{
				CommandID: cmdID, AgentID: agentID, FilePath: target, FileSize: -1,
			})
			return total, rerr
		}
	}
	if err := conn.Write(protocol.TypeFilePushReq, protocol.FilePushReq{
		CommandID: cmdID, AgentID: agentID, FilePath: target, FileSize: 0, ChunkSeq: seq, Done: true,
		Unrestricted: true,
	}); err != nil {
		return total, err
	}
	return total, nil
}

// md5Hex 计算分片 MD5（与既有文件分发协议一致）。
func md5Hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

// fileTextHandler GET(读取) / POST(保存) /api/files/text?session=&path=&encoding=
//
// GET 把文件转成 UTF-8 返回，并带上"实际使用"与"自动检测"的编码；
// POST 的请求正文就是新内容（UTF-8），Agent 按 encoding 转码写回。
//
// 用原始正文而不是 JSON：10MB 文本走 JSON 要额外转义、还会撞上 maxJSONBody 的
// 小请求体上限；这里与上传一致，边收边限。
func fileTextHandler(m *fileSessionManager, sess *session.Manager, authz *auth.Auth,
	settings *settingsStore, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		q := r.URL.Query()
		id := strings.TrimSpace(q.Get("session"))
		view, err := m.acquire(id, operator, true, true)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		defer m.release(id)

		path, err := fsAbsolutePath(strings.TrimSpace(q.Get("path")))
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		encodingLabel := strings.TrimSpace(q.Get("encoding"))
		// 上限以系统配置为准；Agent 侧还有一层硬上限兜底。
		snap := settings.Snapshot()
		limit := snap.FileEditLimitBytes()
		conn := sess.Get(view.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机已离线，文件会话结束"})
			return
		}

		switch r.Method {
		case http.MethodGet:
			resp, err := requestFsOp(conn, protocol.FsOpReq{
				Op: "readtext", Path: path, Encoding: encodingLabel, MaxBytes: limit,
			}, fsOpTimeout)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{
				"ok": true, "path": path, "content": resp.Content,
				"encoding": resp.Encoding, "detected": resp.Detected,
				"size": resp.Size, "mod_time": resp.ModTime,
			})
		case http.MethodPost:
			body := http.MaxBytesReader(w, r.Body, limit+1024)
			defer body.Close()
			data, rerr := io.ReadAll(body)
			if rerr != nil {
				fileError(w, http.StatusRequestEntityTooLarge, "内容超过在线编辑上限")
				return
			}
			resp, err := requestFsOp(conn, protocol.FsOpReq{
				Op: "writetext", Path: path, Content: string(data),
				Encoding: encodingLabel, MaxBytes: limit,
			}, fsOpTimeout)
			if err != nil {
				logx.Warnf("[files] 保存文本失败 agent=%s path=%s: %v", view.AgentID, path, err)
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			if audit != nil {
				audit(r, fmt.Sprintf("编辑文件：%s（%d 字节，%s）", path, resp.Size, resp.Encoding))
			}
			logx.Infof("[files] 已保存文本 %s（%d 字节，%s）", path, resp.Size, resp.Encoding)
			writeJSON(w, map[string]interface{}{
				"ok": true, "size": resp.Size, "encoding": resp.Encoding, "msg": "已保存",
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// fileDownloadHandler GET /api/files/download?session=&path=
func fileDownloadHandler(m *fileSessionManager, sess *session.Manager, ft *filetransfer.Manager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	// 同一 (agent, path) 的并发下载共用文件传输任务键，必须串行化，
	// 否则两路分片会混进同一个任务，落盘内容互相覆盖。
	var downloadLocks sync.Map
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		id := strings.TrimSpace(r.URL.Query().Get("session"))
		view, err := m.acquire(id, operator, true, true)
		if err != nil {
			fileError(w, http.StatusGone, err.Error())
			return
		}
		defer m.release(id)

		path, err := fsAbsolutePath(strings.TrimSpace(r.URL.Query().Get("path")))
		if err != nil {
			fileError(w, http.StatusBadRequest, err.Error())
			return
		}
		lockKey := view.AgentID + "\x00" + path
		mi, _ := downloadLocks.LoadOrStore(lockKey, &sync.Mutex{})
		mu := mi.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
		defer downloadLocks.Delete(lockKey)

		conn := sess.Get(view.AgentID)
		if conn == nil {
			fileError(w, http.StatusConflict, "主机已离线，无法下载")
			return
		}
		h, err := ft.ExpectDownload(view.AgentID, path)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		cmdID := "fm-" + fileRandomID()
		if err := conn.Write(protocol.TypeFileDownloadReq, protocol.FileDownloadReq{
			CommandID: cmdID, FilePath: path, Unrestricted: true,
		}); err != nil {
			ft.CancelDownload(view.AgentID, path)
			fileError(w, http.StatusBadGateway, "下发下载请求失败："+err.Error())
			return
		}
		if err := h.Wait(fileDownloadTimeout); err != nil {
			ft.CancelDownload(view.AgentID, path)
			fileError(w, http.StatusGatewayTimeout, "下载失败："+err.Error())
			return
		}
		defer os.Remove(h.LocalPath)
		f, err := os.Open(h.LocalPath)
		if err != nil {
			fileError(w, http.StatusInternalServerError, "读取回传文件失败："+err.Error())
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			fileError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if st.Size() > maxFileDownloadBytes {
			fileError(w, http.StatusRequestEntityTooLarge, "文件超过单次下载上限（64 MiB）")
			return
		}
		name := filepath.Base(path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if audit != nil {
			audit(r, fmt.Sprintf("文件下载：%s ← %s（%d 字节）", path, view.AgentID, st.Size()))
		}
		logx.Infof("[files] 文件已从 %s 下载：%s（%d 字节）", view.AgentID, path, st.Size())
		http.ServeContent(w, r, name, st.ModTime(), f)
	}
}

// confirmRemoteFile 通过列目录回读，确认 Agent 侧确实落盘且大小一致。
func confirmRemoteFile(conn *session.Conn, target string, want int64) error {
	resp, err := requestFsOp(conn, protocol.FsOpReq{Op: "list", Path: filepath.Dir(target)}, fileListTimeout)
	if err != nil {
		return err
	}
	base := filepath.Base(target)
	for _, e := range resp.Entries {
		if e.Name != base {
			continue
		}
		if e.Dir {
			return errors.New("目标路径是一个目录")
		}
		if e.Size != want {
			return fmt.Errorf("远端大小 %d 与本次发送的 %d 不一致", e.Size, want)
		}
		return nil
	}
	if resp.Truncated {
		// 目录条目超过单次上限，新文件可能恰好排在 1000 项之外：
		// 此时无法确认但不能据此判失败（否则大目录下永远传不进去）。
		return nil
	}
	// 分片是"写完即返回"的，Agent 侧的失败（最常见是目标目录没有写权限）
	// 不会回给服务端，只能从"传完后目录里没有"推断 —— 提示要写这个原因，
	// 不要再提已经不适用的 -file-roots 沙箱。
	return errors.New("远端目录里找不到该文件（Agent 服务账号对该目录没有写权限？具体原因见该主机 Agent 日志）")
}
