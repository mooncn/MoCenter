// MoCenter 总控服务端入口。
//
// 职责：
//   - 监听 TCP 端口接收内网 Agent 的主动长连接
//   - 维护在线主机会话池，处理心跳、探针、命令、交互、文件传输
//   - 提供 WebSocket 端点供 Web 前端通信
//   - 提供 REST API（鉴权、主机列表、命令下发、端口映射管理等）
//   - 端口映射反向代理与连接跟踪
//
// 用法：go run . 或 ./server -listen :9000 -http :8080 -apikey xxx
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/filetransfer"
	"mocenter/server/internal/portmap"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
	"mocenter/server/internal/ws"
)

func main() {
	addr := flag.String("listen", ":9000", "Agent TCP 长连接监听地址")
	httpAddr := flag.String("http", ":8080", "REST API / WebSocket 监听地址")
	apiKey := flag.String("apikey", os.Getenv("MOCENTER_API_KEY"), "全局 API Key")
	whitelist := flag.String("whitelist", os.Getenv("MOCENTER_WHITELIST"), "IP 白名单，逗号分隔")
	webDir := flag.String("web", os.Getenv("MOCENTER_WEB_DIR"), "Web 静态资源目录，留空则不托管前端")
	tlsCert := flag.String("tls-cert", "", "Agent TCP 连接 TLS 证书文件路径")
	tlsKey := flag.String("tls-key", "", "Agent TCP 连接 TLS 私钥文件路径")
	insecure := flag.Bool("insecure", false, "允许非 TLS 的 Agent 连接（未提供证书时自动启用）")
	flag.Parse()

	if *apiKey == "" {
		*apiKey = "change-me-api-key"
	}

	var wl []string
	for _, s := range strings.Split(*whitelist, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wl = append(wl, s)
		}
	}

	sess := session.NewManager()
	store := probe.NewStore()
	authz := auth.NewAuth(*apiKey, wl)
	router := route.NewRouter(sess)
	pm := portmap.NewManager(sess)
	ft := filetransfer.NewManager("/tmp/mocenter_uploads")

	hub := ws.NewHub()

	// WebSocket 收到 Web 消息后的统一处理入口
	hub.SetMessageHandler(func(data []byte) {
		handleWebMessage(data, sess, store, router, hub, ft, pm)
	})

	// 服务端启动后台任务
	go heartbeatSweeper(sess, store, hub)
	go cleanupSweeper(pm, hub)

	// 启动 HTTP 服务（REST + WebSocket）
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.HandleUpgrade(w, r, authz.ValidateToken)
	})
	mux.HandleFunc("/api/agents", agentsHandler(store, authz))
	mux.HandleFunc("/api/agents/history", historyHandler(store, authz))
	mux.HandleFunc("/api/command", commandHandler(router, authz))
	mux.HandleFunc("/api/command/batch", batchCommandHandler(router, authz))
	mux.HandleFunc("/api/audits", auditsHandler(router, authz))
	mux.HandleFunc("/api/portmap/rules", portmapRulesHandler(pm, authz))
	mux.HandleFunc("/api/portmap/op", portmapOpHandler(pm, authz))
	mux.HandleFunc("/api/upgrade", upgradeHandler(sess, authz))
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// 可选：托管 Web 前端静态资源（SPA 回退到 index.html）
	if *webDir != "" {
		serveSPA(mux, *webDir)
		log.Printf("[server] 托管 Web 静态资源: %s", *webDir)
	}

	go func() {
		log.Printf("[server] REST API 监听 %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, mux); err != nil {
			log.Fatalf("[server] http error: %v", err)
		}
	}()
	log.Printf("[server] Agent TCP 监听 %s (apikey=%s)", *addr, auth.HashToken(*apiKey))
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("[server] listen error: %v", err)
	}

	// 若提供证书则启用 TLS（全链路加密）
	var tlsLn net.Listener
	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("[server] 加载 TLS 证书失败: %v", err)
		}
		tlsLn = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		log.Printf("[server] Agent TCP 已启用 TLS 加密")
	} else {
		tlsLn = ln
		if !*insecure {
			log.Printf("[server] 警告: 未提供 TLS 证书，Agent 连接为明文传输；生产环境请配置证书")
		}
	}

	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			log.Printf("[server] accept error: %v", err)
			continue
		}
		go handleAgentConn(conn, sess, store, hub, router, ft)
	}
}

// handleAgentConn 处理单个 Agent 的 TCP 长连接。
func handleAgentConn(conn net.Conn, sess *session.Manager, store *probe.Store, hub *ws.Hub, router *route.Router, ft *filetransfer.Manager) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	sessConn := session.NewConn(conn)
	sessConn.RemoteAddr = conn.RemoteAddr().String()
	// 先读取 Agent 上线消息以获取 AgentID
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			log.Printf("[server] decode error from %s: %v", conn.RemoteAddr(), err)
			continue
		}
		switch env.Type {
		case protocol.TypeOnline:
			var info protocol.AgentInfo
			if err := json.Unmarshal(env.Data, &info); err != nil {
				continue
			}
			sessConn.AgentID = info.AgentID
			sessConn.Hostname = info.Hostname
			sessConn.RemoteAddr = conn.RemoteAddr().String()
			info.Online = true
			info.LastSeen = time.Now().Unix()
			info.RemoteAddr = conn.RemoteAddr().String()
			old, existed := sess.Upsert(info.AgentID, sessConn)
			_ = old
			store.UpsertAgent(&info)
			if existed {
				log.Printf("[server] Agent %s 重新上线", info.AgentID)
			} else {
				log.Printf("[server] Agent %s 上线", info.AgentID)
			}
			hub.Broadcast(protocol.TypeAgentList, store.All())
		}
		break // 上线后跳出循环，进入主消息循环
	}
	if sessConn.AgentID == "" {
		log.Printf("[server] Agent 连接未完成注册，关闭: %s", conn.RemoteAddr())
		return
	}

	// 主消息循环
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		sessConn.LastSeen = time.Now()
		switch env.Type {
		case protocol.TypeHeartbeatReq:
			var hb protocol.HeartbeatReq
			_ = json.Unmarshal(env.Data, &hb)
			// 动态心跳：RTT 优则保持 30s，弱网拉长
			interval := 30
			if hb.RTTMs > 200 {
				interval = 120
			} else if hb.RTTMs > 50 {
				interval = 60
			}
			resp := protocol.HeartbeatResp{Status: 0, ServerTimestamp: time.Now().Unix(), HeartbeatInterval: interval}
			_ = sessConn.Write(protocol.TypeHeartbeatResp, resp)
		case protocol.TypeProbeDataReq:
			var p protocol.ProbeDataReq
			_ = json.Unmarshal(env.Data, &p)
			store.UpdateProbe(&p)
			hub.Broadcast(protocol.TypeAgentList, store.All())
			_ = sessConn.Write(protocol.TypeProbeDataResp, protocol.ProbeDataResp{
				ProbeID: fmt.Sprintf("probe-%d", time.Now().UnixNano()), Timestamp: time.Now().Unix(),
			})
		case protocol.TypeCommandResp:
			var cr protocol.CommandResp
			_ = json.Unmarshal(env.Data, &cr)
			// 转发给 Web 端
			hub.Broadcast(protocol.TypeCommandResp, cr)
			info := store.Get(sessConn.AgentID)
			_ = info
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), AgentID: sessConn.AgentID, Command: "(cmd-" + cr.CommandID + ")",
				Result: cr.Stdout, ExitCode: cr.ExitCode, Duration: cr.Duration,
			})
		case protocol.TypeInteractiveOut:
			var io protocol.InteractiveOutput
			_ = json.Unmarshal(env.Data, &io)
			hub.Broadcast(protocol.TypeInteractiveOut, io)
		case protocol.TypeFileUploadReq:
			var fu protocol.FileUploadReq
			_ = json.Unmarshal(env.Data, &fu)
			// 分片接收：MD5 校验 + 断点续传 + ACK/NACK
			ack := ft.HandleUploadChunk(&fu)
			_ = sessConn.Write(protocol.TypeFileAck, ack)
			// 推送状态给 Web
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: fu.AgentID, FilePath: fu.FilePath, Action: "upload",
				TotalBytes: fu.FileSize, DoneBytes: int64(fu.ChunkSeq+1) * int64(len(fu.ChunkData)),
				ChunkSeq: fu.ChunkSeq, Status: "transferring",
			})
		case protocol.TypeUpgradeResp:
			var ur protocol.UpgradeResp
			_ = json.Unmarshal(env.Data, &ur)
			hub.Broadcast(protocol.TypeUpgradeResp, ur)
			log.Printf("[server] Agent %s 升级结果: %v %s", sessConn.AgentID, ur.OK, ur.Message)
		}
	}

	// 连接断开 -> 标记离线
	sess.Remove(sessConn.AgentID)
	store.MarkOffline(sessConn.AgentID)
	hub.Broadcast(protocol.TypeAgentList, store.All())
	log.Printf("[server] Agent %s 离线", sessConn.AgentID)
}

// heartbeatSweeper 心跳清扫：超过 3 个心跳周期（默认 90s）视为离线。
func heartbeatSweeper(sess *session.Manager, store *probe.Store, hub *ws.Hub) {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		offline := sess.Tick(90 * time.Second)
		for _, id := range offline {
			store.MarkOffline(id)
			log.Printf("[server] Agent %s 心跳超时离线", id)
		}
		if len(offline) > 0 {
			hub.Broadcast(protocol.TypeAgentList, store.All())
		}
	}
}

// cleanupSweeper 端口映射超时连接清理。
func cleanupSweeper(pm *portmap.Manager, hub *ws.Hub) {
	t := time.NewTicker(30 * time.Second)
	for range t.C {
		expired := pm.CleanupExpired(100)
		if len(expired) > 0 {
			log.Printf("[server] 清理 %d 条超时映射连接", len(expired))
			hub.Broadcast(protocol.TypePortMapState, pm.Tracks())
		}
	}
}

// --- REST 处理器 ---

func agentsHandler(store *probe.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		writeJSON(w, store.All())
	}
}

func historyHandler(store *probe.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agentID := r.URL.Query().Get("agent")
		since := time.Now().Add(-24 * time.Hour)
		if r.URL.Query().Get("since") != "" {
			if ts, err := time.Parse(time.RFC3339, r.URL.Query().Get("since")); err == nil {
				since = ts
			}
		}
		writeJSON(w, store.History(agentID, since))
	}
}

func commandHandler(router *route.Router, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req protocol.CommandReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ok, msg := router.Dispatch(&req, "web-admin", r.RemoteAddr)
		writeJSON(w, map[string]interface{}{"ok": ok, "msg": msg})
	}
}

func batchCommandHandler(router *route.Router, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AgentIDs  []string            `json:"agent_ids"`
			CommandID string              `json:"command_id"`
			Command   string              `json:"command"`
			ShellType string              `json:"shell_type"`
			Timeout   int                 `json:"timeout"`
			Env       map[string]string   `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(body.AgentIDs) == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		req := &protocol.CommandReq{
			CommandID: body.CommandID,
			Command:   body.Command,
			ShellType: body.ShellType,
			Timeout:   body.Timeout,
			Env:       body.Env,
		}
		if req.CommandID == "" {
			req.CommandID = fmt.Sprintf("batch-%d", time.Now().UnixNano())
		}
		results := router.DispatchBatch(req, body.AgentIDs, "web-admin", r.RemoteAddr)
		writeJSON(w, map[string]interface{}{"ok": true, "results": results})
	}
}

func auditsHandler(router *route.Router, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		writeJSON(w, router.Audits())
	}
}

func portmapRulesHandler(pm *portmap.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		writeJSON(w, pm.Rules())
	}
}

func portmapOpHandler(pm *portmap.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var op protocol.PortMapOp
		if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		handlePortMapOp(pm, nil, &op)
		writeJSON(w, pm.Rules())
	}
}

func upgradeHandler(sess *session.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AgentID string `json:"agent_id"`
			Version string `json:"version"`
			URL     string `json:"url"`
			MD5     string `json:"md5"`
			Force   bool   `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		conn := sess.Get(body.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "目标主机离线"})
			return
		}
		req := &protocol.UpgradeReq{
			UpgradeID: fmt.Sprintf("up-%d", time.Now().UnixNano()),
			Version:   body.Version,
			URL:       body.URL,
			MD5:       body.MD5,
			Force:     body.Force,
		}
		if err := conn.Write(protocol.TypeUpgradeReq, req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "upgrade_id": req.UpgradeID})
	}
}

// guard 统一鉴权中间件。
func guard(w http.ResponseWriter, r *http.Request, authz *auth.Auth) bool {
	if !authz.IsAllowedIP(r.RemoteAddr) {
		http.Error(w, "ip not allowed", http.StatusForbidden)
		return false
	}
	if !authz.AllowRequest(r.RemoteAddr) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	token := r.Header.Get("Authorization")
	if !authz.ValidateToken(strings.TrimPrefix(token, "Bearer ")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveSPA 托管 SPA 静态资源，并对非 API/WS 路径回退到 index.html。
func serveSPA(mux *http.ServeMux, webDir string) {
	fs := http.FileServer(http.Dir(webDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// API 与 WS 已有独立路由，这里只处理静态资源
		p := r.URL.Path
		if p == "/" || strings.HasPrefix(p, "/api") || strings.HasPrefix(p, "/ws") {
			if p != "/" && (strings.HasPrefix(p, "/api") || strings.HasPrefix(p, "/ws")) {
				// 交由其他处理器
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		// SPA 路由回退：若请求路径不是实际文件，返回 index.html
		filePath := filepath.Join(webDir, filepath.Clean("/"+r.URL.Path))
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}

// handleWebMessage 处理 Web 前端通过 WebSocket 发来的消息。
// 支持：命令下发、交互输入、端口映射操作。
func handleWebMessage(data []byte, sess *session.Manager, store *probe.Store, router *route.Router, hub *ws.Hub, ft *filetransfer.Manager, pm *portmap.Manager) {
	// WebSocket 消息中 data 为 JSON 对象，需用 RawMessage 解码（不同于 TCP 的 base64 []byte）
	var raw struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("[server] web message decode error: %v", err)
		return
	}
	// 统一转成 Envelope 供 switch 复用
	env := protocol.Envelope{Type: raw.Type, Data: raw.Data}
	switch env.Type {
	case protocol.TypeCommandReq:
		var req protocol.CommandReq
		if err := json.Unmarshal(env.Data, &req); err != nil {
			return
		}
		ok, msg := router.Dispatch(&req, "web-admin", "ws")
		if !ok {
			hub.Broadcast(protocol.TypeCommandResp, protocol.CommandResp{
				CommandID: req.CommandID, Status: 1, Stderr: msg,
			})
		}
	case protocol.TypeInteractiveIn:
		var inp protocol.InteractiveInputReq
		if err := json.Unmarshal(env.Data, &inp); err != nil {
			return
		}
		// 交互输入转发给目标 Agent（按 command 中的 agent 字段下发，这里简化直接广播给指定 agent）
		if c := sess.Get(inp.AgentID); c != nil {
			_ = c.Write(protocol.TypeInteractiveIn, inp)
		}
	case protocol.TypePortMapOp:
		var op protocol.PortMapOp
		if err := json.Unmarshal(env.Data, &op); err != nil {
			return
		}
		handlePortMapOp(pm, sess, &op)
		hub.Broadcast(protocol.TypePortMapState, pm.Tracks())
		log.Printf("[server] 端口映射操作: %+v", op)
	case protocol.TypeFileTransferOp:
		var op protocol.FileTransferOp
		if err := json.Unmarshal(env.Data, &op); err != nil {
			return
		}
		handleFileTransferOp(ft, sess, hub, &op)
	}
}

// handleFileTransferOp 处理文件传输操作。
func handleFileTransferOp(ft *filetransfer.Manager, sess *session.Manager, hub *ws.Hub, op *protocol.FileTransferOp) {
	switch op.Action {
	case "download":
		// 向目标 Agent 下发文件下载请求，Agent 回传分片
		if c := sess.Get(op.AgentID); c != nil {
			_ = c.Write(protocol.TypeFileDownloadReq, &protocol.FileDownloadReq{
				CommandID: "dl-" + op.AgentID + "-" + fmt.Sprint(time.Now().UnixNano()),
				FilePath:  op.FilePath,
			})
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: op.AgentID, FilePath: op.FilePath, Action: "download", Status: "transferring",
			})
		} else {
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: op.AgentID, FilePath: op.FilePath, Action: "download", Status: "failed", Message: "目标主机离线",
			})
		}
	case "list":
		// 列出该 Agent 已接收的文件
		hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
			AgentID: op.AgentID, FilePath: op.FilePath, Action: "list", Status: "done",
		})
	}
}

// handlePortMapOp 处理端口映射操作（转发给服务端端口映射管理器）。
func handlePortMapOp(pm *portmap.Manager, sess *session.Manager, op *protocol.PortMapOp) {
	switch op.Action {
	case "create", "enable":
		if op.PublicPort <= 0 || op.LocalPort <= 0 {
			return
		}
		r := &portmap.Rule{
			ID:         fmt.Sprintf("rule-%d-%s", op.PublicPort, op.AgentID),
			PublicPort: op.PublicPort,
			AgentID:    op.AgentID,
			LocalHost:  "127.0.0.1",
			LocalPort:  op.LocalPort,
			SourceIP:   op.SourceIP,
			CreatedAt:  time.Now().Unix(),
		}
		if err := pm.CreateRule(r); err != nil {
			log.Printf("[portmap] 创建映射 %d 失败: %v", op.PublicPort, err)
		}
	case "delete", "disable":
		if op.PublicPort > 0 {
			pm.DeleteRule(op.PublicPort)
		}
	}
}
