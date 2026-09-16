// MoCenter 总控服务端入口。
//
// 职责：
//   - 监听 TCP 端口接收内网 Agent 的主动长连接
//   - 维护在线主机会话池，处理心跳、探针、命令、交互、文件传输
//   - 提供 WebSocket 端点供 Web 前端通信
//   - 提供 REST API（鉴权、主机列表、命令下发、端口映射管理等）
//   - 端口映射反向代理与连接跟踪
//
// 用法：go run . 或 ./server -listen :9000 -http :8080 -user admin -password <密码>
// 登录凭证只有一种：用户名 + 密码（可选 OTP）。首次启动未指定 -user 时
// 会自动创建内置账号 admin/123456。
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
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/auth"
	"mocenter/server/internal/config"
	"mocenter/server/internal/filetransfer"
	"mocenter/server/internal/geo"
	"mocenter/server/internal/portmap"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/publicview"
	"mocenter/server/internal/remark"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
	"mocenter/server/internal/siteconfig"
	"mocenter/server/internal/storage"
	"mocenter/server/internal/vnc"
	"mocenter/server/internal/winapp"
	"mocenter/server/internal/ws"
)

func main() {
	addr := flag.String("listen", ":9000", "Agent TCP 长连接监听地址")
	httpAddr := flag.String("http", ":8080", "REST API / WebSocket 监听地址")
	whitelist := flag.String("whitelist", os.Getenv("MOCENTER_WHITELIST"), "IP 白名单，逗号分隔")
	webDir := flag.String("web", os.Getenv("MOCENTER_WEB_DIR"), "Web 静态资源目录，留空则不托管前端")
	tlsCert := flag.String("tls-cert", "", "Agent TCP 连接 TLS 证书文件路径")
	tlsKey := flag.String("tls-key", "", "Agent TCP 连接 TLS 私钥文件路径")
	caCert := flag.String("ca-cert", "", "mTLS 客户端 CA 证书文件路径（提供后启用双向认证，Agent 需携带客户端证书）")
	insecure := flag.Bool("insecure", false, "允许非 TLS 的 Agent 连接（未提供证书时自动启用）")
	// 便携态默认与可执行文件同目录（不随启动方式变化）；安装态由安装器显式传 -data
	defaultDataDir := config.DefaultDataDir()
	dataDir := flag.String("data", defaultDataDir, "数据目录（审计日志持久化等）")
	pkgDir := flag.String("pkg-dir", "", "安装包托管目录（Agent 可经长连接直接拉取，避免公网下载）；留空则用 <data>/packages")
	user := flag.String("user", "", "登录用户名（设置后启用用户名+密码登录）")
	password := flag.String("password", "", "登录密码")
	totpSecret := flag.String("totp-secret", "", "TOTP 密钥（base32，设置后登录需提供一次性验证码，兼容 Google Authenticator）")
	configPath := flag.String("config", config.DefaultPath(), "配置文件路径（首次启动自动生成）")
	agentKey := flag.String("agent-key", os.Getenv("MOCENTER_AGENT_KEY"), "Agent 注册预共享密钥（留空则自动生成并写到 <data>/agent.key）")
	allowAnonAgents := flag.Bool("allow-unauthenticated-agents", false, "允许 Agent 无凭证注册（危险：任何能连通 :9000 的主机都可注册/冒名）")
	webTLSCert := flag.String("web-tls-cert", "", "Web 控制台 TLS 证书（不配置则 :8080 为明文 HTTP）")
	webTLSKey := flag.String("web-tls-key", "", "Web 控制台 TLS 私钥")
	tokenTTL := flag.Duration("token-ttl", auth.DefaultTokenTTL, "会话 Token 有效期（默认 12h）")
	flag.Parse()

	// 加载配置：首次生成，后续读取并让命令行显式参数覆盖
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg == nil {
		cfg = &config.Config{
			Listen:    *addr,
			HTTP:      *httpAddr,
			Whitelist: *whitelist,
			WebDir:    *webDir,
			TLSCert:   *tlsCert,
			TLSKey:    *tlsKey,
			CACert:    *caCert,
			Insecure:  *insecure,
			DataDir:   *dataDir,
			User:      *user,
			// 不落盘明文密码与 TOTP 密钥：账号以哈希持久化到 users.json
			Password:   "",
			TOTPSecret: "",
			PkgDir:     *pkgDir,
			AgentKey:   *agentKey,
			TokenTTL:   tokenTTL.String(),
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
		if cfg.DataDir != "" && *dataDir == defaultDataDir {
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
		if cfg.WebTLSCert != "" && *webTLSCert == "" {
			*webTLSCert = cfg.WebTLSCert
		}
		if cfg.WebTLSKey != "" && *webTLSKey == "" {
			*webTLSKey = cfg.WebTLSKey
		}
		if cfg.PkgDir != "" && *pkgDir == "" {
			*pkgDir = cfg.PkgDir
		}
		if cfg.AgentKey != "" && *agentKey == os.Getenv("MOCENTER_AGENT_KEY") {
			*agentKey = cfg.AgentKey
		}
		if cfg.TokenTTL != "" && tokenTTL.String() == auth.DefaultTokenTTL.String() {
			if d, err := time.ParseDuration(cfg.TokenTTL); err == nil && d > 0 {
				*tokenTTL = d
			}
		}
		if cfg.AllowAnonAgents {
			*allowAnonAgents = true
		}
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。密码/TOTP 不落盘。
	{
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		// flag 名 -> (配置字段指针, 命令行变量值)
		field := map[string]func(){
			"listen":       func() { cfg.Listen = *addr },
			"http":         func() { cfg.HTTP = *httpAddr },
			"whitelist":    func() { cfg.Whitelist = *whitelist },
			"web":          func() { cfg.WebDir = *webDir },
			"web-tls-cert": func() { cfg.WebTLSCert = *webTLSCert },
			"web-tls-key":  func() { cfg.WebTLSKey = *webTLSKey },
			"tls-cert":     func() { cfg.TLSCert = *tlsCert },
			"tls-key":      func() { cfg.TLSKey = *tlsKey },
			"ca-cert":      func() { cfg.CACert = *caCert },
			"insecure":     func() { cfg.Insecure = *insecure },
			"data":         func() { cfg.DataDir = *dataDir },
			"user":         func() { cfg.User = *user },
			"pkg-dir":      func() { cfg.PkgDir = *pkgDir },
			"agent-key":    func() { cfg.AgentKey = *agentKey },
			"token-ttl":    func() { cfg.TokenTTL = tokenTTL.String() },
		}
		dirty := false
		for _, name := range []string{"listen", "http", "whitelist", "web", "web-tls-cert", "web-tls-key", "tls-cert", "tls-key", "ca-cert", "insecure", "data", "user", "pkg-dir", "agent-key", "token-ttl"} {
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

	// 安装包托管目录：默认 <data>/packages。
	// Agent 一键安装 VNC 时会优先通过既有长连接从这里拉取安装包，
	// 避免走公网（网速慢、且可能被劫持/篡改）。
	if *pkgDir == "" {
		*pkgDir = filepath.Join(*dataDir, "packages")
	}
	if err := os.MkdirAll(*pkgDir, 0755); err != nil {
		log.Printf("[server] 创建安装包目录失败 %s: %v", *pkgDir, err)
	}
	logAvailablePackages(*pkgDir)

	// Agent 注册预共享密钥：应用层鉴权的信任根。
	// 优先级：-agent-key > <dataDir>/agent.key（自动生成）。
	// 有了它，未授权主机无法注册，也无法用别人的 AgentID 冒名顶替。
	agentKeyPath := filepath.Join(*dataDir, agentauth.KeyFileName)
	agentKeyVal := strings.TrimSpace(*agentKey)
	agentKeyCreated := false
	if agentKeyVal == "" {
		k, created, err := agentauth.LoadOrCreateKey(agentKeyPath)
		if err != nil {
			log.Fatalf("[server] 读取/生成 Agent 注册密钥失败: %v", err)
		}
		agentKeyVal, agentKeyCreated = k, created
	} else if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Printf("[server] 警告: 创建数据目录失败: %v", err)
	} else if err := os.WriteFile(agentKeyPath, []byte(agentKeyVal+"\n"), 0600); err != nil {
		log.Printf("[server] 警告: 保存 Agent 注册密钥失败: %v", err)
	}
	verifier := agentauth.NewVerifier(agentKeyVal)
	if agentKeyCreated {
		log.Printf("[server] 已生成 Agent 注册密钥并写入 %s（权限 0600）", agentKeyPath)
	}
	if verifier.Enabled() {
		log.Printf("[server] Agent 注册鉴权已启用（密钥指纹 %s）。请给每台 Agent 配置同一密钥："+
			"agent 启动参数 -agent-key，或配置文件字段 agent_key；来源：本文件 / -agent-key 参数", verifier.KeyFingerprint())
	} else {
		log.Printf("[server] 警告: Agent 注册鉴权已关闭（-allow-unauthenticated-agents）——"+
			"任何能连通 %s 的主机都可注册为 Agent 或冒用他人 AgentID", *addr)
	}

	var wl []string
	for _, s := range strings.Split(*whitelist, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wl = append(wl, s)
		}
	}

	// 单实例保护：必须在打开数据库之前拿到 —— 两个实例共用同一个 SQLite 文件
	// （以及同一个审计日志）会互相踩踏。服务与手工启动的实例共用同一配置时尤其要注意。
	release, ok := winapp.AcquireSingleton(winapp.SingletonKey("Server", *configPath))
	if !ok {
		log.Fatalf("已有一个使用同一配置(%s)的 Server 在运行，本次启动终止", *configPath)
	}
	defer release()

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
	// agent_id 变更（同一台机器改配置）时，备注跟随设备一起迁移，避免"备注丢失"
	store.SetRemarkRenameFn(remarks.Rename)
	// 公开站点展示配置（Issue #5）：默认**关闭**，升级安装不会因本功能对外暴露主机信息。
	siteCfg := siteconfig.NewStore(filepath.Join(*dataDir, "public-site.json"))
	if err := siteCfg.Load(); err != nil {
		log.Printf("警告: 加载公开站点配置失败（按默认=关闭处理）: %v", err)
	}
	authz := auth.NewAuth(wl)
	authz.SetTokenTTL(*tokenTTL)
	loginGuard := auth.NewLoginGuard()
	userMgr := auth.NewUserManager(filepath.Join(*dataDir, "users.json"))
	if err := userMgr.InitFromFlags(*user, *password, *totpSecret); err != nil {
		if errors.Is(err, auth.ErrInitPasswordRequired) {
			// 提供了 -user 但缺少 -password：给出明确提示，避免静默生成空密码坏账号
			log.Printf("警告: 已指定 -user %q 但未提供 -password，未创建账号。请补充 -password 后重启。", *user)
		} else {
			log.Printf("警告: 初始化用户失败: %v", err)
		}
	}
	// 若尚无任何账号，则创建初始账号（**不再使用固定弱口令** admin/123456 ——
	// 口令写在源码里等于公开，"凭证未泄露"的前提对它不成立）。
	// 可用 -user/-password 覆盖；未指定密码时生成随机强口令，写入 <data>/initial-password.txt（0600）。
	if !userMgr.HasUsers() {
		defUser := *user
		if defUser == "" {
			defUser = "admin"
		}
		defPass := *password
		if defPass == "" {
			defPass = randPassword(16)
		}
		if err := userMgr.UpsertUser(defUser, defPass, *totpSecret); err != nil {
			log.Printf("警告: 创建默认账号失败: %v", err)
		} else if strings.TrimSpace(*password) == "" {
			pwFile := filepath.Join(*dataDir, "initial-password.txt")
			_ = os.MkdirAll(*dataDir, 0700)
			body := fmt.Sprintf("账号: %s\n初始密码: %s\n生成时间: %s\n\n请登录后立即在「账号设置」修改密码，随后删除本文件。\n",
				defUser, defPass, time.Now().Format("2006-01-02 15:04:05"))
			if werr := os.WriteFile(pwFile, []byte(body), 0600); werr != nil {
				log.Printf("已创建初始账号 %s，但初始密码写入 %s 失败: %v", defUser, pwFile, werr)
			} else {
				log.Printf("已创建初始账号 %s，初始密码已写入 %s（权限 0600，请查看后删除）", defUser, pwFile)
			}
			if !winapp.IsWindowsService() {
				log.Printf(">>> 初始账号 %s / %s （请登录后立即修改密码） <<<", defUser, defPass)
			}
		} else {
			log.Printf("已创建默认账号 %s", defUser)
		}
	}
	// 修复历史版本因缺少 -password 而误建的空密码账号：改用**随机口令**（不再固定 123456）。
	if broken := userMgr.BrokenAccounts(); len(broken) > 0 {
		for _, name := range broken {
			pw := randPassword(16)
			if err := userMgr.SetPassword(name, pw); err != nil {
				log.Printf("警告: 修复账号 %s 失败: %v", name, err)
				continue
			}
			if !winapp.IsWindowsService() {
				log.Printf("已修复历史空密码账号 %s，临时密码：%s（请登录后立即修改）", name, pw)
			} else {
				log.Printf("已修复历史空密码账号 %s，临时密码见 %s", name,
					filepath.Join(*dataDir, "initial-password.txt"))
			}
		}
	}
	router := route.NewRouter(sess, db, filepath.Join(*dataDir, "audits.jsonl"))
	defer router.Close()
	pm := portmap.NewManager(sess, db)
	// 上传/下传文件的暂存目录：放在数据目录下并收紧权限。
	// 旧版硬编码 /tmp/mocenter_uploads（全局可写目录），且 Linux 下任何用户可读。
	ft := filetransfer.NewManager(filepath.Join(*dataDir, "uploads"))
	pushMgr := filetransfer.NewPushManager()
	vncMgr := vnc.NewMgr(sess)

	hub := ws.NewHub()

	// --- 公开站点（免登录，Issue #5）---
	// 注意：这些端点不经过 guard，一切数据都取自 publicview 的脱敏视图，
	// agent_id / uuid / 内网地址永不出网。
	pub := &publicSite{
		store: store,
		cfg:   siteCfg,
		guard: auth.NewLoginGuard(),
		hub:   hub,
		authz: authz,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
			// 公开页可能被嵌在其它站点/内网门户里，允许任意来源；
			// 数据本身已是脱敏结果，跨站读取不构成越权。
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	// WebSocket 收到 Web 消息后的统一处理入口
	hub.SetMessageHandler(func(operator string, data []byte) {
		handleWebMessage(operator, data, sess, store, router, hub, ft, pm, pushMgr, vncMgr)
	})

	// 服务端启动后台任务
	go heartbeatSweeper(sess, store, hub, pub)
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
	mux.HandleFunc("/api/login", loginHandler(userMgr, authz, loginGuard, router))
	mux.HandleFunc("/api/logout", logoutHandler(authz))
	mux.HandleFunc("/api/password", changePasswordHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/setup", otpSetupHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/enable", otpEnableHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/disable", otpDisableHandler(userMgr, authz))
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc(publicAPIBase+"/state", pub.stateHandler())
	mux.HandleFunc(publicAPIBase+"/nodes", pub.nodesHandler())
	mux.HandleFunc(publicAPIBase+"/samples", pub.samplesHandler())
	mux.HandleFunc(publicAPIBase+"/ws", pub.publicWSHandler())
	mux.HandleFunc(publicAPIBase+"/enabled", publicConfigStateHandler(siteCfg))
	// 后台展示配置（需登录）
	siteAudit := func(r *http.Request, summary string) {
		router.Audit(&route.AuditLog{
			Time:       time.Now().Unix(),
			Operator:   authz.Operator(r.Header.Get("Authorization")),
			OperatorIP: auth.LoginIPOf(r.RemoteAddr),
			Command:    summary, Result: "ok", Category: "op",
		})
	}
	mux.HandleFunc("/api/site/config", siteConfigHandler(siteCfg, authz, siteAudit))
	mux.HandleFunc("/api/site/config/reset", siteConfigResetHandler(siteCfg, authz, siteAudit))
	mux.HandleFunc("/api/site/hosts", siteHostOptionsHandler(store, authz))

	if siteCfg.Get().Enabled {
		log.Printf("[server] 公开站点已启用：%s（字段开关见 后台 → 展示配置）", publicAPIBase)
	} else {
		log.Printf("[server] 公开站点未启用（默认关闭）；如需对外展示运行指标，请在 Web 后台「展示配置」中开启")
	}

	// 可选：托管 Web 前端静态资源（SPA 回退到 index.html）
	if *webDir != "" {
		serveSPA(mux, *webDir)
		log.Printf("[server] 托管 Web 静态资源: %s", *webDir)
	}

	// 监听与接收循环封装成函数：被 SCM 拉起时由服务启动回调调用。
	start := func() {
		go func() {
			// Web 控制台 TLS：配置了证书就走 https，否则明文 ——
			// 明文意味着登录口令与 Token 会上网，必须在前面挂 TLS 反代才能接受。
			if *webTLSCert != "" && *webTLSKey != "" {
				cert, err := tls.LoadX509KeyPair(*webTLSCert, *webTLSKey)
				if err != nil {
					log.Fatalf("[server] 加载 Web TLS 证书失败: %v", err)
				}
				webSrv := &http.Server{
					Addr:    *httpAddr,
					Handler: securityHeaders(mux),
					TLSConfig: &tls.Config{
						Certificates: []tls.Certificate{cert},
						MinVersion:   tls.VersionTLS12,
					},
					ReadHeaderTimeout: 10 * time.Second,
				}
				log.Printf("[server] REST API / Web 控制台监听 %s（TLS）", *httpAddr)
				if err := webSrv.ListenAndServeTLS("", ""); err != nil {
					log.Fatalf("[server] https error: %v", err)
				}
				return
			}
			log.Printf("[server] REST API / Web 控制台监听 %s（**明文 HTTP**）", *httpAddr)
			if !isLoopbackAddr(*httpAddr) {
				log.Printf("[server] 警告: Web 控制台未启用 TLS，登录口令与 Token 将明文传输。" +
					"请配置 -web-tls-cert/-web-tls-key，或确保服务只通过 TLS 反向代理对外提供")
			}
			srv := &http.Server{
				Addr:              *httpAddr,
				Handler:           securityHeaders(mux),
				ReadHeaderTimeout: 10 * time.Second,
			}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("[server] http error: %v", err)
			}
		}()
		log.Printf("[server] Agent TCP 监听 %s", *addr)
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
			// Agent 通道没有应用层鉴权（握手不校验任何口令）：能连通本端口的主机
			// 就可以注册为 Agent。唯一可用的身份校验是 mTLS，故在此明确提示。
			log.Printf("[server] 警告: Agent 通道未启用 mTLS，任何能连通 %s 的主机都可注册为 Agent；"+
				"如需限制请用 -ca-cert 启用双向认证，或用防火墙/安全组限制来源", *addr)
		}

		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				log.Printf("[server] accept error: %v", err)
				continue
			}
			go handleAgentConn(conn, sess, store, hub, router, ft, pm, pushMgr, vncMgr, *pkgDir, verifier, *allowAnonAgents, pub)
		}
	}

	// 被 SCM 拉起时走服务控制协议（sc query/stop 才能正常工作）；否则按普通进程运行。
	// 用 IsWindowsService 自动识别，无需额外开关。
	if winapp.IsWindowsService() {
		logPath := filepath.Join(filepath.Dir(*configPath), "logs", serverServiceName+".log")
		if err := winapp.RedirectLogToFile(logPath); err != nil {
			log.Printf("警告: 重定向服务日志到 %s 失败: %v", logPath, err)
		} else {
			log.Printf("[server] 服务模式启动，日志输出到 %s", logPath)
		}
		if err := winapp.RunAsService(serverServiceName, start, func() { os.Exit(0) }); err != nil {
			log.Fatalf("以 Windows 服务方式运行失败: %v", err)
		}
		return
	}

	start()
}

// serverServiceName Windows 服务名。必须与安装器注册时使用的名称一致
// （svc.Run 的名字对不上会导致 SCM 无法完成握手）。
const serverServiceName = "MoCenterServer"

// connOwns 判断报文自报的身份是否与连接身份一致。
//
// 报文里的 AgentID 一律不可信 —— 攻击者可用它冒充别的主机上报假探针、
// 甚至把文件写到服务端任意路径。身份只能来自注册成功的那条连接。
func connOwns(c *session.Conn, claimed string) bool {
	if c == nil || claimed == "" {
		return false
	}
	id := c.GetAgentID()
	if id == "" {
		return false
	}
	return claimed == id
}

// uuidSuffix 返回日志里用于标识设备的 UUID 后缀（无 UUID 时为空串）。
func uuidSuffix(uuid string) string {
	if strings.TrimSpace(uuid) == "" {
		return ""
	}
	return "，设备 UUID " + uuid
}

// validAgentID 校验 AgentID 是否可用。
// 允许较宽松（兼容中文主机名 / 硬件 UUID），但禁止空白与控制字符，
// 防止不可控内容进入日志与审计。
func validAgentID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// handleAgentConn 处理单个 Agent 的 TCP 长连接。
//
// 安全模型（加固后）：
//   - 注册需携带预共享密钥的 HMAC 证明（verifier），否则拒绝；
//   - 同名 AgentID 的活跃连接**不允许被顶替**（由 session.Upsert 拒绝）；
//   - 报文里自带的 AgentID 一律以**连接身份**为准，不一致即丢弃 —— 这是
//     "身份取自连接而不是取自报文"的关键，否则伪造 AgentID 仍能冒用他人。
func handleAgentConn(conn net.Conn, sess *session.Manager, store *probe.Store, hub *ws.Hub, router *route.Router, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr, pkgDir string, verifier *agentauth.Verifier, allowAnon bool, pub *publicSite) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	sessConn := session.NewConn(conn)
	sessConn.RemoteAddr = conn.RemoteAddr().String()
	// 先读取 Agent 上线消息以完成注册
	registered := false
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			log.Printf("[server] decode error from %s: %v", conn.RemoteAddr(), err)
			continue
		}
		// 注册完成前只接受上线消息，其余一律丢弃（避免未注册连接触碰业务逻辑）
		if env.Type != protocol.TypeOnline {
			continue
		}
		var info protocol.AgentInfo
		if err := json.Unmarshal(env.Data, &info); err != nil {
			continue
		}
		if !validAgentID(info.AgentID) {
			log.Printf("[server] 拒绝注册（agent_id 非法或为空）: %s", conn.RemoteAddr())
			return
		}
		// 应用层注册鉴权：HMAC 证明 + 时间窗 + 防重放
		if verifier.Enabled() {
			if err := verifier.Verify(info.AgentID, info.AuthNonce, info.AuthProof, info.AuthTime); err != nil {
				// 只写日志、不写审计库：这里的内容来自未认证方，写库会被灌垃圾
				log.Printf("[server] 拒绝 Agent %q 注册: %v（来自 %s）", info.AgentID, err, conn.RemoteAddr())
				return
			}
		} else if !allowAnon {
			log.Printf("[server] 拒绝 Agent %q 注册: 服务端未配置注册密钥", info.AgentID)
			return
		}
		sessConn.SetAgentID(info.AgentID)
		sessConn.SetUUID(info.UUID)
		sessConn.Hostname = info.Hostname
		info.Online = true
		info.LastSeen = time.Now().Unix()
		info.RemoteAddr = conn.RemoteAddr().String()

		// 设备身份以 UUID 为准：先把该 UUID 名下的旧连接顶掉。
		// 否则「同一台机器改了 agent_id 重连」会在会话池里留下两条会话，
		// 前端与 store 都表现为两台机器（数据冗余的根因之一）。
		//
		// 顺序：先确认新连接能被接纳（Upsert 通过），再顶掉旧连接。
		// 反过来会在「新连接被拒绝」时误杀仍在服务的旧连接。
		old, accepted := sess.Upsert(info.AgentID, sessConn)
		if !accepted {
			log.Printf("[server] 拒绝 Agent %s 重复注册：该 ID 已有活跃连接（正在被 %s 使用）",
				info.AgentID, old.RemoteAddr)
			return
		}
		// 新连接已接入，再顶掉同一设备（UUID）下的旧会话：此时旧连接是被"自己"
		// 替换的，无需标离线（否则会把刚上线的新连接状态冲掉）。
		for _, oldID := range sess.ReplaceByUUID(info.UUID, sessConn) {
			log.Printf("[server] 设备 %s 的旧会话 %s 已被本次连接顶替（同机 agent_id 变更或重复上线）",
				info.UUID, oldID)
		}
		store.UpsertAgent(&info)
		log.Printf("[server] Agent %s 上线（来自 %s%s）", info.AgentID, conn.RemoteAddr(),
			uuidSuffix(info.UUID))
		broadcastAgents(hub, store, pub)
		registered = true
		break // 上线后跳出循环，进入主消息循环
	}
	if !registered || sessConn.GetAgentID() == "" {
		log.Printf("[server] Agent 连接未完成注册，关闭: %s", conn.RemoteAddr())
		return
	}

	// 主消息循环
	for sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			continue
		}
		sessConn.SetLastSeen(time.Now())
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
			// 身份以连接为准：报文里的 agent_id 不可信，
			// 否则一条连接可以伪造其它主机的探针数据 / 凭空造出假主机。
			if !connOwns(sessConn, p.AgentID) {
				log.Printf("[server] 丢弃 %s 的探针报文（自报身份 %q 与连接身份不符）", sessConn.GetAgentID(), p.AgentID)
				continue
			}
			store.UpdateProbe(&p)
			broadcastAgents(hub, store, pub)
			_ = sessConn.Write(protocol.TypeProbeDataResp, protocol.ProbeDataResp{
				ProbeID: fmt.Sprintf("probe-%d", time.Now().UnixNano()), Timestamp: time.Now().Unix(),
			})
		case protocol.TypeCommandResp:
			var cr protocol.CommandResp
			_ = json.Unmarshal(env.Data, &cr)
			// 转发给 Web 端
			hub.Broadcast(protocol.TypeCommandResp, cr)
			// 回填命令审计结果（不新增记录，避免 operator 为空的重复条目）
			router.CompleteByCommand(cr.CommandID, sessConn.GetAgentID(), cr.Stdout, cr.ExitCode, cr.Duration)
		case protocol.TypeInteractiveOut:
			var io protocol.InteractiveOutput
			_ = json.Unmarshal(env.Data, &io)
			hub.Broadcast(protocol.TypeInteractiveOut, io)
		case protocol.TypeFileUploadReq:
			var fu protocol.FileUploadReq
			_ = json.Unmarshal(env.Data, &fu)
			// 身份以连接为准：防止一条连接伪造他人身份把文件写到服务端
			if !connOwns(sessConn, fu.AgentID) {
				log.Printf("[server] 丢弃 %s 的上传分片（自报身份 %q 与连接身份不符）", sessConn.GetAgentID(), fu.AgentID)
				continue
			}
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
			// 安装包拉取的分片 ACK 属于内部行为，不向 Web 广播传输状态，
			// 否则文件分发列表里会冒出一条临时路径的记录。
			if _, isPkg := pkgFetchCmds.Load(fa.CommandID); isPkg {
				continue
			}
			hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
				AgentID: sessConn.GetAgentID(), FilePath: fa.FilePath, Action: "push",
				Status: fa.Status, Message: fa.Message, ChunkSeq: fa.ChunkSeq,
			})
			if fa.Status == "done" || fa.Status == "failed" {
				pushMgr.Remove(fa.CommandID)
			}
		case protocol.TypeUpgradeResp:
			var ur protocol.UpgradeResp
			_ = json.Unmarshal(env.Data, &ur)
			hub.Broadcast(protocol.TypeUpgradeResp, ur)
			log.Printf("[server] Agent %s 升级结果: %v %s", sessConn.GetAgentID(), ur.OK, ur.Message)
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
				vncMgr.NotifyInstall(sessConn.GetAgentID(), &vr)
			}
		case protocol.TypePkgFetchReq:
			var pr protocol.PackageFetchReq
			if json.Unmarshal(env.Data, &pr) == nil {
				// 独立 goroutine：大包分片下发不应阻塞该 Agent 的主消息循环
				go servePackage(sessConn, pkgDir, &pr)
			}
		}
	}

	// 连接断开 -> 标记离线。
	// 注意：设备身份是 UUID。旧连接被同 UUID 的新连接顶替后也会走到这里
	// （或被顶替时已从会话池移除），此时不能把同一台机器标成离线。
	if !sess.RemoveIfCurrent(sessConn.GetAgentID(), sessConn) {
		log.Printf("[server] Agent %s 的旧连接已断开（该设备已有新会话），跳过离线标记", sessConn.GetAgentID())
		return
	}
	store.MarkOffline(sessConn.GetAgentID())
	broadcastAgents(hub, store, pub)
	log.Printf("[server] Agent %s 离线", sessConn.GetAgentID())
}

// heartbeatSweeper 心跳清扫：超过 3 个心跳周期（默认 90s）视为离线。
func heartbeatSweeper(sess *session.Manager, store *probe.Store, hub *ws.Hub, pub *publicSite) {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		offline := sess.Tick(90 * time.Second)
		for _, id := range offline {
			store.MarkOffline(id)
			log.Printf("[server] Agent %s 心跳超时离线", id)
		}
		if len(offline) > 0 {
			broadcastAgents(hub, store, pub)
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

// broadcastAgents 向已登录 Web 与公开页**同时**推送主机列表。
//
// 为什么要包一层：服务端有 4 处都会更新主机列表（上线/探针/离线/心跳清扫），
// 分散写两次 Broadcast 极易漏掉一边，表现为"公开页数据停在某个时刻不刷新"。
// 公开侧的载荷由 publicview 脱敏后再生成，绝不复用 store.All() 的原始结构。
func broadcastAgents(hub *ws.Hub, store *probe.Store, pub *publicSite) {
	hub.Broadcast(protocol.TypeAgentList, store.All())
	if pub == nil {
		return
	}
	cfg := pub.cfg.Get()
	if cfg == nil || !cfg.Enabled || !cfg.HostsEnabled {
		// 未启用/不展示主机：不发任何数据，避免匿名连接收到"空列表"以外的信息。
		return
	}
	nodes := publicview.BuildNodes(store.All(), cfg)
	hub.BroadcastPublic(map[string]interface{}{
		"ok":      true,
		"nodes":   nodes,
		"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
	})
}

// --- REST 处理器 ---

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
			AgentIDs  []string          `json:"agent_ids"`
			CommandID string            `json:"command_id"`
			Command   string            `json:"command"`
			ShellType string            `json:"shell_type"`
			Timeout   int               `json:"timeout"`
			Env       map[string]string `json:"env"`
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
			Mode    string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		// 归一化安装模式：仅识别 service，其余按交互会话模式处理
		mode := protocol.VncModeInteractive
		if strings.EqualFold(strings.TrimSpace(body.Mode), protocol.VncModeService) {
			mode = protocol.VncModeService
		}
		resp := mgr.Install(body.AgentID, body.Port, mode)
		writeJSON(w, map[string]interface{}{
			"ok": resp.OK, "msg": resp.Message, "port": resp.Port,
			"password": resp.Password, "mode": resp.Mode,
		})
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
		writeJSON(w, map[string]interface{}{"ok": resp.OK, "msg": resp.Message, "port": resp.Port, "password": resp.Password, "mode": resp.Mode})
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
				Time: time.Now().Unix(), Operator: authz.Operator(r.Header.Get("Authorization")), OperatorIP: auth.LoginIPOf(r.RemoteAddr),
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
			AgentID   string `json:"agent_id"`
			Version   string `json:"version"`
			URL       string `json:"url"`
			SHA256    string `json:"sha256"`
			Signature string `json:"signature"`
			MD5       string `json:"md5"` // 已废弃，仅为兼容旧前端
			Force     bool   `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		// 服务端侧先做一次校验，尽早把错误反馈给操作者（Agent 侧还会再校验一次）
		if strings.TrimSpace(body.SHA256) == "" {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "必须提供升级包的 sha256 摘要（md5 已废弃，且不能作为唯一校验）"})
			return
		}
		if !isHex64(strings.ToLower(strings.TrimSpace(body.SHA256))) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "sha256 摘要格式不合法"})
			return
		}
		// 下载源协议白名单：http 会被明文传输、易被中间人篡改，等同于给目标机投毒
		if u, perr := url.Parse(strings.TrimSpace(body.URL)); perr != nil || u.Scheme == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "升级包地址不合法"})
			return
		} else if strings.EqualFold(u.Scheme, "http") {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "升级包地址必须是 https（http 明文传输，升级包可能被篡改）"})
			return
		} else if !strings.EqualFold(u.Scheme, "https") {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "升级包地址协议不被允许: " + u.Scheme})
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
			SHA256:    strings.ToLower(strings.TrimSpace(body.SHA256)),
			Signature: strings.TrimSpace(body.Signature),
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

// isHex64 判断是否为 64 位十六进制（SHA-256 摘要）。
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// loginHandler 处理用户名+密码（+OTP 可选）登录，成功后颁发访问 Token。
//
// 安全说明：这是唯一无法要求 Token 的入口，因此必须自行做
// IP 白名单校验、专用限流与失败退避 —— 此前完全没有，弱口令可被无限次在线尝试。
func loginHandler(um *auth.UserManager, authz *auth.Auth, loginGuard *auth.LoginGuard, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// IP 白名单：配置了白名单就同样适用于登录（否则限流只能挡速率，挡不住来源）
		if !authz.IsAllowedIP(r.RemoteAddr) {
			http.Error(w, "ip not allowed", http.StatusForbidden)
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
		// 限流 + 失败退避（按 IP 与用户名双重约束）
		if ok, retry := loginGuard.Allow(r.RemoteAddr, body.Username); !ok {
			w.Header().Set("Retry-After", auth.RetryAfter(retry))
			writeJSON(w, map[string]interface{}{"ok": false, "msg": fmt.Sprintf("尝试过于频繁，请 %d 秒后再试", int(retry/time.Second)+1)})
			return
		}
		opIP := auth.LoginIPOf(r.RemoteAddr)
		if err := um.Authenticate(body.Username, body.Password, body.OTP); err != nil {
			loginGuard.Fail(body.Username)
			// 登录失败同样留痕，便于安全审计与暴力破解排查
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: auditSafeText(body.Username), OperatorIP: opIP,
				Command: "登录失败", Result: err.Error(), ExitCode: -1, Category: "auth",
			})
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		loginGuard.Success(body.Username)
		tok := randHex(32)
		authz.RegisterToken(tok, body.Username)
		router.Audit(&route.AuditLog{
			Time: time.Now().Unix(), Operator: auditSafeText(body.Username), OperatorIP: opIP,
			Command: "登录成功", Result: "ok", Category: "auth",
		})
		log.Printf("[auth] 用户 %s 登录成功（来自 %s）", body.Username, opIP)
		writeJSON(w, map[string]interface{}{"ok": true, "token": tok})
	}
}

// logoutHandler 登出：吊销当前会话 Token。
func logoutHandler(authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !guard(w, r, authz) {
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		authz.RevokeToken(token)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "已登出"})
	}
}

// auditSafeText 收敛来自未认证方的审计字段：限长并去掉控制字符，
// 防止未认证请求向审计库注入伪造条目或超长垃圾。
func auditSafeText(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 128 {
			break
		}
	}
	if b.Len() == 0 {
		return "(空)"
	}
	return b.String()
}

// changePasswordHandler 修改用户密码（需鉴权；校验当前密码，若启用 OTP 还需验证码）。
//
// 身份以 Token 为准：不再信任请求体里的 username —— 否则任何持有有效 Token 的
// 会话都能对其它账号发起改密尝试。成功后吊销该用户的**其它**会话
// （被盗的旧 Token 也会一并失效，但当前会话不受影响）。
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
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		username := authz.OperatorOf(token)
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		var body struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
			OTP         string `json:"otp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := um.ChangePassword(username, body.OldPassword, body.NewPassword, body.OTP); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		// 吊销该用户的其它会话：被盗 Token 即使不知道新密码也会立即失效
		if n := authz.RevokeUserTokens(username, token); n > 0 {
			log.Printf("[auth] 用户 %s 修改密码成功，已吊销 %d 个其它会话", username, n)
		} else {
			log.Printf("[auth] 用户 %s 修改密码成功", username)
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "密码修改成功"})
	}
}

// randHex 生成随机十六进制字符串（用于 Token）。
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randPassword 生成可读的随机口令（去除易混淆字符，默认 16 位）。
// 用于「不再使用固定弱口令」之后的初始账号生成。
func randPassword(n int) string {
	const charset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	if n <= 0 {
		n = 16
	}
	out := make([]byte, n)
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 熵源故障时不应生成可预测口令：直接退出，让运维看到原因
		log.Fatalf("生成随机口令失败（系统熵源不可用）: %v", err)
	}
	for i := range out {
		out[i] = charset[int(buf[i])%len(charset)]
	}
	return string(out)
}

// otpSetupHandler 生成一个新的 TOTP 密钥与 otpauth URI，供前端渲染二维码。
//
// 身份以 Token 为准：不再接受请求体/查询串里的 username —— 否则任何会话都能为
// 其它账号生成密钥。此时尚未落盘，须经 /api/otp/enable 验证码校验通过后才绑定。
func otpSetupHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		username := authz.OperatorOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
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
//
// 身份以 Token 为准（请求体里的 username 被忽略）：
// 否则任何会话都能把**自己知道**的密钥绑到其它账号上，
// 造成他人账号被锁在门外 / 二次验证被悄然替换。
func otpEnableHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		username := authz.OperatorOf(token)
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		var body struct {
			Secret string `json:"secret"`
			Code   string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Secret == "" || body.Code == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "密钥与验证码均不能为空"})
			return
		}
		// 扫码绑定前先比对验证码是否正确
		if !um.VerifyTOTP(username, body.Secret, body.Code) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "验证码不正确，请确认扫码后输入的是当前动态码"})
			return
		}
		if err := um.BindTOTP(username, body.Secret); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "绑定失败: " + err.Error()})
			return
		}
		// 绑定 OTP 属于安全状态变更：吊销其它会话
		if n := authz.RevokeUserTokens(username, token); n > 0 {
			log.Printf("[auth] 用户 %s 启用 OTP，吊销 %d 个其它会话", username, n)
		} else {
			log.Printf("[auth] 用户 %s 已启用 OTP 动态验证码", username)
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "OTP 动态验证码已启用"})
	}
}

// otpDisableHandler 关闭 OTP（需校验密码，启用中还需当前验证码）。身份以 Token 为准。
func otpDisableHandler(um *auth.UserManager, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		username := authz.OperatorOf(token)
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		var body struct {
			Password string `json:"password"`
			OTP      string `json:"otp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := um.UnbindTOTP(username, body.Password, body.OTP); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		log.Printf("[auth] 用户 %s 已关闭 OTP 动态验证码", username)
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

// securityHeaders 给所有响应加基础防护头。
//
// 刻意不含 CSP：Vite 产物对内联样式的依赖需要实测后才能收紧，
// 贸然上线可能把前端整个拍死（那本身也是一次"破坏原有功能"）。
// 这里只放确定无副作用的三个头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// isLoopbackAddr 判断监听地址是否只绑定了回环地址（用于明文告警）。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return false // 绑所有接口，必然包含非回环
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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
		if json.Unmarshal(env.Data, &inp) != nil {
			return
		}
		// 交互输入同样过黑名单（逐行检查）：远程终端此前是黑名单的最大旁路
		safe, blocked := router.FilterInteractive(inp.AgentID+"|"+inp.CommandID, inp.InputData)
		if blocked != "" {
			hub.Broadcast(protocol.TypeInteractiveOut, protocol.InteractiveOutput{
				CommandID: inp.CommandID,
				Data:      "\r\n[server] 输入被黑名单拦截: " + blocked + "\r\n",
				Type:      "stderr",
			})
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: operator, AgentID: inp.AgentID,
				CommandID: inp.CommandID, Command: inp.InputData,
				Blocked: true, BlockReason: blocked, Category: "command",
			})
			// 补发 Ctrl-C：清掉 Agent 侧已经敲进去的半行，恢复提示符
			if c := sess.Get(inp.AgentID); c != nil {
				_ = c.Write(protocol.TypeInteractiveIn, &protocol.InteractiveInputReq{
					AgentID: inp.AgentID, CommandID: inp.CommandID, Control: "ctrl_c",
				})
			}
			return
		}
		inp.InputData = safe
		// 交互输入转发给目标 Agent
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

// --- 安装包托管与下发 ---

// pkgFetchCmds 记录正在进行的安装包拉取 command_id。
// 这些分片的 ACK 属于内部行为，不应作为「文件分发」广播给 Web。
var pkgFetchCmds sync.Map

// logAvailablePackages 启动时列出安装包目录中可下发的包，便于运维确认。
func logAvailablePackages(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[pkg] 读取安装包目录失败 %s: %v", dir, err)
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fi, ferr := e.Info(); ferr == nil {
			names = append(names, fmt.Sprintf("%s(%.1fMB)", e.Name(), float64(fi.Size())/1024/1024))
		}
	}
	if len(names) == 0 {
		log.Printf("[pkg] 安装包目录 %s 为空；如需让 Agent 从服务端取包（避免公网下载），把安装包放进去即可", dir)
		return
	}
	log.Printf("[pkg] 安装包目录 %s 已托管: %s", dir, strings.Join(names, ", "))
}

// servePackage 将服务端托管的安装包以分片形式下发给 Agent。
//
// 分片复用 FilePushReq（Agent 侧已有成熟的落盘实现：建目录、追加写、Done 时关闭），
// 全部分片发完后再回执 PackageFetchResp。Agent 顺序处理消息，因此它收到回执时
// 分片必然已全部落盘，无需轮询文件大小。
func servePackage(conn *session.Conn, pkgDir string, req *protocol.PackageFetchReq) {
	// 防目录穿越：只取文件名，拒绝含路径分隔或上级引用的名称
	name := filepath.Base(strings.TrimSpace(req.Name))
	resp := protocol.PackageFetchResp{CommandID: req.CommandID, Name: name}
	fail := func(msg string) {
		resp.OK = false
		resp.Message = msg
		_ = conn.Write(protocol.TypePkgFetchResp, resp)
		log.Printf("[pkg] 向 %s 下发 %s 失败: %s", conn.AgentID, name, msg)
	}
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		fail("未指定有效的安装包名称")
		return
	}
	if req.DestPath == "" {
		fail("未指定 Agent 端落盘路径")
		return
	}
	path := filepath.Join(pkgDir, name)
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		fail("服务端未托管该安装包：" + name)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fail("打开安装包失败: " + err.Error())
		return
	}
	defer f.Close()

	if req.CommandID != "" {
		pkgFetchCmds.Store(req.CommandID, struct{}{})
		defer pkgFetchCmds.Delete(req.CommandID)
	}

	total := fi.Size()
	buf := make([]byte, protocol.DefaultChunkSize)
	var seq int
	var sent int64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			sent += int64(n)
			last := sent >= total
			sum := md5.Sum(chunk)
			if werr := conn.Write(protocol.TypeFilePushReq, &protocol.FilePushReq{
				CommandID: req.CommandID,
				FilePath:  req.DestPath,
				FileSize:  total,
				ChunkSeq:  seq,
				ChunkData: chunk,
				ChunkMD5:  fmt.Sprintf("%x", sum),
				Done:      last,
			}); werr != nil {
				log.Printf("[pkg] 向 %s 下发 %s 中断: %v", conn.AgentID, name, werr)
				return
			}
			seq++
			if last {
				break
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("[pkg] 读取安装包 %s 失败: %v", name, rerr)
			}
			break
		}
	}
	resp.OK = true
	resp.Size = total
	resp.Message = "安装包下发完成"
	_ = conn.Write(protocol.TypePkgFetchResp, resp)
	log.Printf("[pkg] 已向 %s 下发安装包 %s（%d 字节，%d 分片）", conn.AgentID, name, total, seq)
}
