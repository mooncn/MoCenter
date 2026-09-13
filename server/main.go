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
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/config"
	"mocenter/server/internal/filetransfer"
	"mocenter/server/internal/geo"
	"mocenter/server/internal/portmap"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/remark"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
	"mocenter/server/internal/storage"
	"mocenter/server/internal/vnc"
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
	caCert := flag.String("ca-cert", "", "mTLS 客户端 CA 证书文件路径（提供后启用双向认证，Agent 需携带客户端证书）")
	insecure := flag.Bool("insecure", false, "允许非 TLS 的 Agent 连接（未提供证书时自动启用）")
	dataDir := flag.String("data", ".", "数据目录（审计日志持久化等）")
	user := flag.String("user", "", "登录用户名（设置后启用用户名+密码登录）")
	password := flag.String("password", "", "登录密码")
	totpSecret := flag.String("totp-secret", "", "TOTP 密钥（base32，设置后登录需提供一次性验证码，兼容 Google Authenticator）")
	configPath := flag.String("config", config.DefaultPath(), "配置文件路径（首次启动自动生成）")
	flag.Parse()

	// 加载配置：首次生成，后续读取并让命令行显式参数覆盖
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg == nil {
		cfg = &config.Config{
			Listen:     *addr,
			HTTP:       *httpAddr,
			APIKey:     *apiKey,
			Whitelist:  *whitelist,
			WebDir:     *webDir,
			TLSCert:    *tlsCert,
			TLSKey:     *tlsKey,
			CACert:     *caCert,
			Insecure:   *insecure,
			DataDir:    *dataDir,
			User:       *user,
			// 不落盘明文密码与 TOTP 密钥：账号以哈希持久化到 users.json
			Password:   "",
			TOTPSecret: "",
		}
		if err := config.Save(*configPath, cfg); err != nil {
			log.Printf("警告: 写入配置文件 %s 失败: %v", *configPath, err)
		} else {
			log.Printf("已生成配置文件: %s（后续可直接运行 %s）", *configPath, os.Args[0])
		}
	} else {
		// 已有配置：命令行显式参数（非默认值）覆盖配置文件
		if cfg.Listen != "" && *addr == ":9000" {
			*addr = cfg.Listen
		}
		if cfg.HTTP != "" && *httpAddr == ":8080" {
			*httpAddr = cfg.HTTP
		}
		if cfg.APIKey != "" && *apiKey == os.Getenv("MOCENTER_API_KEY") {
			*apiKey = cfg.APIKey
		}
		if cfg.Whitelist != "" && *whitelist == os.Getenv("MOCENTER_WHITELIST") {
			*whitelist = cfg.Whitelist
		}
		if cfg.WebDir != "" && *webDir == os.Getenv("MOCENTER_WEB_DIR") {
			*webDir = cfg.WebDir
		}
		if cfg.TLSCert != "" && *tlsCert == "" {
			*tlsCert = cfg.TLSCert
		}
		if cfg.TLSKey != "" && *tlsKey == "" {
			*tlsKey = cfg.TLSKey
		}
		if cfg.CACert != "" && *caCert == "" {
			*caCert = cfg.CACert
		}
		if cfg.Insecure && !*insecure {
			*insecure = true
		}
		if cfg.DataDir != "" && *dataDir == "." {
			*dataDir = cfg.DataDir
		}
		if cfg.User != "" && *user == "" {
			*user = cfg.User
		}
		if cfg.Password != "" && *password == "" {
			*password = cfg.Password
		}
		if cfg.TOTPSecret != "" && *totpSecret == "" {
			*totpSecret = cfg.TOTPSecret
		}
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。密码/TOTP 不落盘。
	{
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		// flag 名 -> (配置字段指针, 命令行变量值)
		field := map[string]func(){
			"listen":    func() { cfg.Listen = *addr },
			"http":      func() { cfg.HTTP = *httpAddr },
			"apikey":    func() { cfg.APIKey = *apiKey },
			"whitelist": func() { cfg.Whitelist = *whitelist },
			"web":       func() { cfg.WebDir = *webDir },
			"tls-cert":  func() { cfg.TLSCert = *tlsCert },
			"tls-key":   func() { cfg.TLSKey = *tlsKey },
			"ca-cert":   func() { cfg.CACert = *caCert },
			"insecure":  func() { cfg.Insecure = *insecure },
			"data":      func() { cfg.DataDir = *dataDir },
			"user":      func() { cfg.User = *user },
		}
		dirty := false
		for _, name := range []string{"listen", "http", "apikey", "whitelist", "web", "tls-cert", "tls-key", "ca-cert", "insecure", "data", "user"} {
			if explicit[name] {
				field[name]()
				dirty = true
			}
		}
		if dirty {
			if err := config.Save(*configPath, cfg); err != nil {
				log.Printf("警告: 更新配置文件 %s 失败: %v", *configPath, err)
			} else {
				log.Printf("已把命令行参数保存到配置文件: %s", *configPath)
			}
		}
	}

	// 未显式指定 Web 目录时，自动探测可执行文件旁或当前目录下的 web 前端，
	// 便于单文件部署：把 server 与 web/ 放在同目录即可直接托管前端。
	if *webDir == "" {
		candidates := []string{
			filepath.Join(filepath.Dir(os.Args[0]), "web"),
			filepath.Join(".", "web"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(filepath.Join(c, "index.html")); err == nil {
				*webDir = c
				log.Printf("[server] 自动探测到 Web 前端目录: %s", c)
				break
			}
		}
	}

	if *apiKey == "" {
		*apiKey = "change-me-api-key"
	}

	var wl []string
	for _, s := range strings.Split(*whitelist, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wl = append(wl, s)
		}
	}

	// 打开 SQLite 持久化存储
	dbPath := filepath.Join(*dataDir, "mocenter.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("[server] 打开 SQLite 数据库失败: %v", err)
	}
	defer db.Close()
	log.Printf("[server] SQLite 数据库: %s", dbPath)

	// 初始化内置黑名单规则（幂等）
	if err := db.InitDefaultBlacklist(route.DefaultBlacklistPatterns); err != nil {
		log.Printf("[server] 初始化内置黑名单失败: %v", err)
	}

	sess := session.NewManager()
	store := probe.NewStore(db)
	geoResolver := geo.NewResolver()
	store.SetGeoResolver(geoResolver)
	remarks := remark.NewStore(*dataDir)
	remarks.Load()
	store.SetRemarkFn(remarks.Get)
	authz := auth.NewAuth(*apiKey, wl)
	userMgr := auth.NewUserManager(filepath.Join(*dataDir, "users.json"))
	if err := userMgr.InitFromFlags(*user, *password, *totpSecret); err != nil {
		if errors.Is(err, auth.ErrInitPasswordRequired) {
			// 提供了 -user 但缺少 -password：给出明确提示，避免静默生成空密码坏账号
			log.Printf("警告: 已指定 -user %q 但未提供 -password，未创建账号。请补充 -password 后重启。", *user)
		} else {
			log.Printf("警告: 初始化用户失败: %v", err)
		}
	}
	// 若尚无任何账号，则创建内置默认账号 admin/123456，保证开箱可登录。
	// 可用 -user/-password 覆盖默认用户名与密码。
	if !userMgr.HasUsers() {
		defUser := *user
		if defUser == "" {
			defUser = "admin"
		}
		defPass := *password
		if defPass == "" {
			defPass = "123456"
		}
		if err := userMgr.UpsertUser(defUser, defPass, *totpSecret); err != nil {
			log.Printf("警告: 创建默认账号失败: %v", err)
		} else if defUser == "admin" && defPass == "123456" {
			log.Printf("已创建内置默认账号 admin/123456（请登录后尽快修改密码）")
		} else {
			log.Printf("已创建默认账号 %s", defUser)
		}
	}
	// 修复历史版本因缺少 -password 而误建的空密码账号，重置为 123456 并提示。
	if broken := userMgr.BrokenAccounts(); len(broken) > 0 {
		for _, name := range broken {
			if err := userMgr.SetPassword(name, "123456"); err != nil {
				log.Printf("警告: 修复账号 %s 失败: %v", name, err)
				continue
			}
			log.Printf("已修复历史空密码账号 %s，密码重置为 123456，请登录后立即修改", name)
		}
	}
	router := route.NewRouter(sess, db, filepath.Join(*dataDir, "audits.jsonl"))
	defer router.Close()
	pm := portmap.NewManager(sess, db)
	ft := filetransfer.NewManager("/tmp/mocenter_uploads")
	pushMgr := filetransfer.NewPushManager()
	vncMgr := vnc.NewMgr(sess)

	hub := ws.NewHub()

	// WebSocket 收到 Web 消息后的统一处理入口
	hub.SetMessageHandler(func(operator string, data []byte) {
		handleWebMessage(operator, data, sess, store, router, hub, ft, pm, pushMgr, vncMgr)
	})

	// 服务端启动后台任务
	go heartbeatSweeper(sess, store, hub)
	go cleanupSweeper(pm, hub)
	// 探针历史定期清理（保留 30 天）+ 审计日志留存清理（保留 180 天）
	go probeHistoryCleaner(db)
	go auditRetentionCleaner(db)

	// 启动 HTTP 服务（REST + WebSocket）
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.HandleUpgrade(w, r, authz.ValidateToken, authz.OperatorOf)
	})
	mux.HandleFunc("/vnc-ws", func(w http.ResponseWriter, r *http.Request) {
		vncMgr.HandleViewer(w, r, authz.ValidateToken)
	})
	mux.HandleFunc("/api/command", commandHandler(router, authz))
	mux.HandleFunc("/api/command/batch", batchCommandHandler(router, authz))
	mux.HandleFunc("/api/command/close", commandCloseHandler(sess, authz))
	mux.HandleFunc("/api/vnc/probe", vncProbeHandler(vncMgr, authz))
	mux.HandleFunc("/api/vnc/install", vncInstallHandler(vncMgr, authz))
	mux.HandleFunc("/api/vnc/status", vncStatusHandler(vncMgr, authz))
	mux.HandleFunc("/api/agents", agentsHandler(store, authz))
	mux.HandleFunc("/api/agents/history", historyHandler(store, authz))
	mux.HandleFunc("/api/agents/remark", setRemarkHandler(remarks, authz))
	mux.HandleFunc("/api/audits", auditsHandler(router, authz))
	mux.HandleFunc("/api/portmap/rules", portmapRulesHandler(pm, authz))
	mux.HandleFunc("/api/portmap/op", portmapOpHandler(pm, authz, router))
	mux.HandleFunc("/api/upgrade", upgradeHandler(sess, authz))
	mux.HandleFunc("/api/login", loginHandler(userMgr, authz, router))
	mux.HandleFunc("/api/password", changePasswordHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/setup", otpSetupHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/enable", otpEnableHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/disable", otpDisableHandler(userMgr, authz))
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

	// 若提供证书则启用 TLS（全链路加密）。提供 CA 时启用 mTLS 双向认证。
	var tlsLn net.Listener
	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("[server] 加载 TLS 证书失败: %v", err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		if *caCert != "" {
			caPEM, err := os.ReadFile(*caCert)
			if err != nil {
				log.Fatalf("[server] 读取 CA 证书失败: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				log.Fatalf("[server] 解析 CA 证书失败")
			}
			tlsCfg.ClientCAs = pool
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			log.Printf("[server] Agent TCP 已启用 mTLS 双向认证（校验客户端 CA 证书）")
		} else {
			log.Printf("[server] Agent TCP 已启用 TLS 加密（单向）")
		}
		tlsLn = tls.NewListener(ln, tlsCfg)
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
		go handleAgentConn(conn, sess, store, hub, router, ft, pm, pushMgr, vncMgr)
	}
}

// handleAgentConn 处理单个 Agent 的 TCP 长连接。
func handleAgentConn(conn net.Conn, sess *session.Manager, store *probe.Store, hub *ws.Hub, router *route.Router, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr) {
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
			// 记录动态心跳间隔，供超时清扫按实际心跳频率判定离线
			sessConn.SetHeartbeatInterval(interval)
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
			// 回填命令审计结果（不新增记录，避免 operator 为空的重复条目）
			router.CompleteByCommand(cr.CommandID, sessConn.AgentID, cr.Stdout, cr.ExitCode, cr.Duration)
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
		case protocol.TypeFilePushAck:
			var fa protocol.FilePushAck
			_ = json.Unmarshal(env.Data, &fa)
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: sessConn.AgentID, FilePath: fa.FilePath, Action: "push",
				Status: fa.Status, Message: fa.Message, ChunkSeq: fa.ChunkSeq,
			})
			if fa.Status == "done" || fa.Status == "failed" {
				pushMgr.Remove(fa.CommandID)
			}
		case protocol.TypeUpgradeResp:
			var ur protocol.UpgradeResp
			_ = json.Unmarshal(env.Data, &ur)
			hub.Broadcast(protocol.TypeUpgradeResp, ur)
			log.Printf("[server] Agent %s 升级结果: %v %s", sessConn.AgentID, ur.OK, ur.Message)
		case protocol.TypeTunnelOpenResp:
			var tr protocol.TunnelOpenResp
			if json.Unmarshal(env.Data, &tr) != nil {
				continue
			}
			if isVNCTunnel(tr.TunnelID) {
				vncMgr.DeliverOpenResp(tr.TunnelID, tr.OK, tr.Message)
				if !tr.OK {
					log.Printf("[server] VNC 隧道建立失败 %s: %s", tr.TunnelID, tr.Message)
				}
			} else if !tr.OK {
				// 端口映射隧道建立失败：关闭对应的公网侧连接
				log.Printf("[server] 隧道建立失败 %s: %s", tr.TunnelID, tr.Message)
				pm.NotifyTunnelClosed(tr.TunnelID, tr.Message)
			}
		case protocol.TypeTunnelData:
			var td protocol.TunnelData
			if json.Unmarshal(env.Data, &td) == nil {
				if isVNCTunnel(td.TunnelID) {
					vncMgr.DeliverTunnelData(td.TunnelID, td.Data)
				} else {
					pm.DeliverTunnelData(td.TunnelID, td.Data)
				}
			}
		case protocol.TypeTunnelClose:
			var tc protocol.TunnelClose
			if json.Unmarshal(env.Data, &tc) == nil {
				if isVNCTunnel(tc.TunnelID) {
					vncMgr.NotifyTunnelClosed(tc.TunnelID)
				} else {
					pm.NotifyTunnelClosed(tc.TunnelID, tc.Reason)
				}
			}
		case protocol.TypeVncInstallResp:
			var vr protocol.VncInstallResp
			if json.Unmarshal(env.Data, &vr) == nil {
				vncMgr.NotifyInstall(sessConn.AgentID, &vr)
			}
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
		q := r.URL.Query()
		agentID := q.Get("agent")
		since := time.Now().Add(-24 * time.Hour)
		if q.Get("since") != "" {
			if ts, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
				since = ts
			}
		}
		// max 限制返回点数（默认 300，上限 2000），避免长时间跨度返回海量采样点
		maxPoints, _ := strconv.Atoi(q.Get("max"))
		if maxPoints <= 0 {
			maxPoints = 300
		}
		if maxPoints > 2000 {
			maxPoints = 2000
		}
		writeJSON(w, store.HistoryDownsampled(agentID, since, maxPoints))
	}
}

// operatorIPOf 取请求来源 IP（用于登录审计留痕）。
func operatorIPOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// setRemarkHandler 设置/更新指定主机的备注（POST 或 PUT）。
func setRemarkHandler(remarks *remark.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AgentID string `json:"agent_id"`
			Remark  string `json:"remark"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		if err := remarks.Set(body.AgentID, body.Remark); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "保存备注失败: " + err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "备注已保存"})
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
		ok, msg := router.Dispatch(&req, authz.Operator(r.Header.Get("Authorization")), r.RemoteAddr)
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
		results := router.DispatchBatch(req, body.AgentIDs, authz.Operator(r.Header.Get("Authorization")), r.RemoteAddr)
		writeJSON(w, map[string]interface{}{"ok": true, "results": results})
	}
}

// commandCloseHandler 断开远程终端连接：向目标 Agent 下发 InteractiveClose，结束相关进程。
func commandCloseHandler(sess *session.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req protocol.InteractiveCloseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		conn := sess.Get(req.AgentID)
		if conn == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "目标主机离线"})
			return
		}
		if err := conn.Write(protocol.TypeInteractiveClose, req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "下发失败: " + err.Error()})
			return
		}
		log.Printf("[server] 终端断开请求 agent=%s command_id=%q kill_all=%v", req.AgentID, req.CommandID, req.KillAll)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "已下发断开/结束指令"})
	}
}

// vncProbeHandler 探测目标主机 VNC 是否可连。
func vncProbeHandler(mgr *vnc.Mgr, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agentID := r.URL.Query().Get("agent")
		port := 5900
		if s := r.URL.Query().Get("port"); s != "" {
			if p, err := strconv.Atoi(s); err == nil && p > 0 {
				port = p
			}
		}
		ok, msg := mgr.Probe(agentID, port)
		writeJSON(w, map[string]interface{}{"ok": ok, "msg": msg, "port": port, "agent_id": agentID})
	}
}

// vncInstallHandler 一键下发 VNC 安装/启动指令并返回结果。
func vncInstallHandler(mgr *vnc.Mgr, authz *auth.Auth) http.HandlerFunc {
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
			Port    int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		resp := mgr.Install(body.AgentID, body.Port)
		writeJSON(w, map[string]interface{}{"ok": resp.OK, "msg": resp.Message, "port": resp.Port, "password": resp.Password})
	}
}

// vncStatusHandler 返回该主机最近一次 VNC 安装/启动结果（含口令，供超时后补取）。
func vncStatusHandler(mgr *vnc.Mgr, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agentID := r.URL.Query().Get("agent")
		resp := mgr.LastResult(agentID)
		if resp == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "尚未执行安装/启动"})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": resp.OK, "msg": resp.Message, "port": resp.Port, "password": resp.Password})
	}
}

// isVNCTunnel 判断隧道 ID 是否属于 VNC（查看器 v- / 探测 vp-）。
func isVNCTunnel(tunnelID string) bool {
	return strings.HasPrefix(tunnelID, "v-") || strings.HasPrefix(tunnelID, "vp-")
}

func auditsHandler(router *route.Router, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		q := r.URL.Query()
		var since, until int64
		if v, err := strconv.ParseInt(q.Get("since"), 10, 64); err == nil {
			since = v
		}
		if v, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil {
			until = v
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		query := route.AuditQuery{
			Agent:    q.Get("agent"),
			Operator: q.Get("operator"),
			Command:  q.Get("cmd"),
			Category: q.Get("category"),
			Since:    since,
			Until:    until,
			Offset:   offset,
			Limit:    limit,
		}
		page, err := router.SearchAuditsPage(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 脱敏：命令中的敏感字段掩码后返回
		for _, l := range page.Items {
			l.Command = route.MaskCommand(l.Command)
			l.Result = route.MaskCommand(l.Result)
		}
		if q.Get("format") == "csv" {
			writeAuditCSV(w, page.Items)
			return
		}
		writeJSON(w, page)
	}
}

// writeAuditCSV 将审计日志导出为 CSV。
func writeAuditCSV(w http.ResponseWriter, logs []*route.AuditLog) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audits.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "operator", "agent_id", "command_id", "command", "result", "exit_code", "blocked", "block_reason", "duration_ms"})
	for _, l := range logs {
		_ = cw.Write([]string{
			strconv.FormatInt(l.Time, 10),
			l.Operator, l.AgentID, l.CommandID, l.Command, l.Result,
			strconv.Itoa(l.ExitCode),
			strconv.FormatBool(l.Blocked), l.BlockReason,
			strconv.FormatInt(l.Duration, 10),
		})
	}
	cw.Flush()
}

func portmapRulesHandler(pm *portmap.Manager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		writeJSON(w, pm.Rules())
	}
}

func portmapOpHandler(pm *portmap.Manager, authz *auth.Auth, router *route.Router) http.HandlerFunc {
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
		// 端口映射增删改属于关键运维操作，纳入操作审计留痕
		if router != nil {
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: authz.Operator(r.Header.Get("Authorization")), OperatorIP: operatorIPOf(r),
				Command: fmt.Sprintf("端口映射 %s public:%d local:%d agent:%s", op.Action, op.PublicPort, op.LocalPort, op.AgentID),
				Result:  "ok", Category: "op",
			})
		}
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

// loginHandler 处理用户名+密码（+OTP 可选）登录，成功后颁发访问 Token。
func loginHandler(um *auth.UserManager, authz *auth.Auth, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			OTP      string `json:"otp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !um.HasUsers() {
			http.Error(w, "未配置可登录用户", http.StatusNotImplemented)
			return
		}
		opIP := operatorIPOf(r)
		if err := um.Authenticate(body.Username, body.Password, body.OTP); err != nil {
			// 登录失败同样留痕，便于安全审计与暴力破解排查
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: body.Username, OperatorIP: opIP,
				Command: "登录失败", Result: err.Error(), ExitCode: -1, Category: "auth",
			})
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		tok := randHex(32)
		authz.RegisterToken(tok, body.Username)
		router.Audit(&route.AuditLog{
			Time: time.Now().Unix(), Operator: body.Username, OperatorIP: opIP,
			Command: "登录成功", Result: "ok", Category: "auth",
		})
		log.Printf("[auth] 用户 %s 登录成功", body.Username)
		writeJSON(w, map[string]interface{}{"ok": true, "token": tok})
	}
}

// changePasswordHandler 修改用户密码（需鉴权；校验当前密码，若启用 OTP 还需验证码）。
func changePasswordHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !um.HasUsers() {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未启用账号登录，无需修改密码"})
			return
		}
		var body struct {
			Username    string `json:"username"`
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
			OTP         string `json:"otp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := um.ChangePassword(body.Username, body.OldPassword, body.NewPassword, body.OTP); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		log.Printf("[auth] 用户 %s 修改密码成功", body.Username)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "密码修改成功"})
	}
}

// randHex 生成随机十六进制字符串（用于 Token）。
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// otpSetupHandler 生成一个新的 TOTP 密钥与 otpauth URI，
// 供前端渲染二维码；此时尚未落盘，须经 /api/otp/enable 验证码校验通过后才绑定。
func otpSetupHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		// 支持 GET（URL 参数）与 POST（JSON body）两种调用方式
		username := r.URL.Query().Get("username")
		if username == "" {
			var body struct {
				Username string `json:"username"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			username = body.Username
		}
		if username == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "请提供用户名"})
			return
		}
		enabled, exists := um.TOTPStatus(username)
		if !exists {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "用户不存在"})
			return
		}
		secret, err := auth.GenerateTOTPSecret()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "生成密钥失败: " + err.Error()})
			return
		}
		uri := auth.TOTPUri("MoCenter", username, secret)
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"secret":  secret,
			"uri":     uri,
			"enabled": enabled,
		})
	}
}

// otpEnableHandler 校验用户输入的动态验证码，正确后绑定 OTP 密钥。
func otpEnableHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Username string `json:"username"`
			Secret   string `json:"secret"`
			Code     string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Username == "" || body.Secret == "" || body.Code == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "用户名、密钥与验证码均不能为空"})
			return
		}
		// 扫码绑定前先比对验证码是否正确
		if !um.VerifyTOTP(body.Username, body.Secret, body.Code) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "验证码不正确，请确认扫码后输入的是当前动态码"})
			return
		}
		if err := um.BindTOTP(body.Username, body.Secret); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "绑定失败: " + err.Error()})
			return
		}
		log.Printf("[auth] 用户 %s 已启用 OTP 动态验证码", body.Username)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "OTP 动态验证码已启用"})
	}
}

// otpDisableHandler 关闭 OTP（需校验密码，启用中还需当前验证码）。
func otpDisableHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			OTP      string `json:"otp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := um.UnbindTOTP(body.Username, body.Password, body.OTP); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		log.Printf("[auth] 用户 %s 已关闭 OTP 动态验证码", body.Username)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "OTP 动态验证码已关闭"})
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
func handleWebMessage(operator string, data []byte, sess *session.Manager, store *probe.Store, router *route.Router, hub *ws.Hub, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr) {
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
		ok, msg := router.Dispatch(&req, operator, "ws")
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
	case protocol.TypeFilePushReq:
		// Web 上传文件分发分片：缓存到服务端，全部到齐后推送给 Agent
		handleWebPushChunk(pushMgr, sess, hub, env.Data)
	}
}

// handleWebPushChunk 处理 Web 端上传的文件分发分片。
func handleWebPushChunk(pushMgr *filetransfer.PushManager, sess *session.Manager, hub *ws.Hub, data []byte) {
	var req protocol.FilePushReq
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}
	task, complete := pushMgr.UploadChunk(&req)
	if task == nil {
		// 分发失败/取消
		hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
			AgentID: req.AgentID, FilePath: req.FilePath, Action: "push",
			Status: "failed", Message: "文件分发被取消或目标主机离线",
		})
		return
	}
	// 推送进度给 Web
	hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
		AgentID: req.AgentID, FilePath: req.FilePath, Action: "push",
		TotalBytes: task.Size(), DoneBytes: task.TotalBytes(), ChunkSeq: req.ChunkSeq,
		Status: "transferring",
	})

	if !complete {
		return
	}

	// 全部到齐，向目标 Agent 依次推送分片
	conn := sess.Get(req.AgentID)
	if conn == nil {
		hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
			AgentID: req.AgentID, FilePath: req.FilePath, Action: "push",
			Status: "failed", Message: "目标主机离线",
		})
		pushMgr.Remove(req.CommandID)
		return
	}

	chunks := task.OrderedChunks()
	total := len(chunks)
	for i, chunk := range chunks {
		last := i == total-1
		push := protocol.FilePushReq{
			CommandID: req.CommandID,
			FilePath:  task.FilePath,
			FileSize:  task.FileSize,
			ChunkSeq:  i,
			ChunkData: chunk,
			ChunkMD5:  fmt.Sprintf("%x", md5.Sum(chunk)),
			Done:      last,
		}
		if err := conn.Write(protocol.TypeFilePushReq, push); err != nil {
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: req.AgentID, FilePath: task.FilePath, Action: "push",
				Status: "failed", Message: "推送失败: " + err.Error(),
			})
			pushMgr.Remove(req.CommandID)
			return
		}
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
	case "create":
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
	case "enable":
		if op.PublicPort <= 0 {
			return
		}
		// 规则已存在则恢复监听；否则（规则曾被删除或不存在）按新建处理
		if err := pm.EnableRule(op.PublicPort); err != nil {
			log.Printf("[portmap] 启用映射 %d 失败: %v", op.PublicPort, err)
		}
	case "disable":
		if op.PublicPort > 0 {
			pm.DisableRule(op.PublicPort)
		}
	case "delete":
		if op.PublicPort > 0 {
			pm.DeleteRule(op.PublicPort)
		}
	}
}

// auditRetentionCleaner 定期清理超过留存期的审计日志。
// 审计日志随运行时间持续增长，需按留存策略归档/清理，避免单表无界膨胀。
func auditRetentionCleaner(db *storage.DB) {
	const keepDays = 180
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		n, err := db.CleanupOldAuditLogs(keepDays)
		if err != nil {
			log.Printf("[server] 清理审计日志失败: %v", err)
		} else if n > 0 {
			log.Printf("[server] 清理了 %d 条超出 %d 天留存期的审计日志", n, keepDays)
		}
	}
}

// probeHistoryCleaner 定期清理超过 30 天的探针历史数据。
func probeHistoryCleaner(db *storage.DB) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		n, err := db.CleanupOldProbeHistory(30)
		if err != nil {
			log.Printf("[server] 清理探针历史失败: %v", err)
		} else if n > 0 {
			log.Printf("[server] 清理了 %d 条过期探针历史", n)
		}
	}
}
