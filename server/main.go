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
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/agentdeploy"
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
	"mocenter/server/internal/version"
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
	requireAgentCert := flag.Bool("require-agent-cert", false, "要求 Agent 必须携带客户端证书（默认允许无证书连接自行换取，兼容旧版 Agent）")
	insecure := flag.Bool("insecure", false, "允许非 TLS 的 Agent 连接（未提供证书时自动启用）")
	// 便携态默认与可执行文件同目录（不随启动方式变化）；安装态由安装器显式传 -data
	defaultDataDir := config.DefaultDataDir()
	dataDir := flag.String("data", defaultDataDir, "数据目录（审计日志持久化等）")
	pkgDir := flag.String("pkg-dir", "", "安装包托管目录（Agent 可经长连接直接拉取，避免公网下载）；留空则用 <data>/packages")
	user := flag.String("user", "", "登录用户名（设置后启用用户名+密码登录）")
	password := flag.String("password", "", "登录密码（**不推荐**：会出现在 ps 与 systemctl show 里，建议改用 -password-file）")
	passwordFile := flag.String("password-file", os.Getenv("MOCENTER_PASSWORD_FILE"), "初始口令文件路径（权限应 0600）。仅用于首次创建账号，优先于 -password")
	totpSecret := flag.String("totp-secret", "", "TOTP 密钥（base32，设置后登录需提供一次性验证码，兼容 Google Authenticator）")
	configPath := flag.String("config", config.DefaultPath(), "配置文件路径（首次启动自动生成）")
	agentKey := flag.String("agent-key", os.Getenv("MOCENTER_AGENT_KEY"), "Agent 注册预共享密钥（留空则自动生成并写到 <data>/agent.key）")
	proxyIP := flag.String("proxy-ip", os.Getenv("MOCENTER_PROXY_IP"), "国内加速代理前缀（如 http://fa.mooncn.win/?url= ），用于节点地理位置的境外接口 ip-api.com")
	mmdbPath := flag.String("mmdb", os.Getenv("MOCENTER_MMDB"), "离线 GeoIP 库路径（MaxMind DB 格式，如 GeoLite2-Country.mmdb）；配置后优先本地解析，避免外部接口限流与无外网环境失效")
	allowAnonAgents := flag.Bool("allow-unauthenticated-agents", false, "允许 Agent 无凭证注册（危险：任何能连通 :9000 的主机都可注册/冒名）")
	webTLSCert := flag.String("web-tls-cert", "", "Web 控制台 TLS 证书（不配置则 :8080 为明文 HTTP）")
	webTLSKey := flag.String("web-tls-key", "", "Web 控制台 TLS 私钥")
	adminTLSDays := flag.Int("admin-tls-days", 30, "Web 控制台自动 HTTPS 证书有效期（天）；0 表示关闭自动 HTTPS")
	serverName := flag.String("server-name", "", "部署身份名（写入服务端 CA，用于区分多套总控；留空自动生成）")
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
			Password:          "",
			TOTPSecret:        "",
			AdminPasswordFile: *passwordFile,
			PkgDir:           *pkgDir,
			AgentKey:         *agentKey,
			ProxyIP:          *proxyIP,
			MMDBPath:         *mmdbPath,
			WebTLSCert:       *webTLSCert,
			WebTLSKey:        *webTLSKey,
			TokenTTL:         tokenTTL.String(),
			RequireAgentCert: *requireAgentCert,
			AdminTLSDays:     *adminTLSDays,
			ServerName:       *serverName,
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
		// 口令文件路径同样从配置文件回填：服务模式（systemd）不带命令行参数，
		// 配置文件是它唯一的来源；不回填就表现为"配置里写了却不生效"。
		if cfg.AdminPasswordFile != "" && *passwordFile == os.Getenv("MOCENTER_PASSWORD_FILE") {
			*passwordFile = cfg.AdminPasswordFile
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
		// ProxyIP 必须回填到 flag：启动时 geo.NewResolverWithProxy(*proxyIP) 用的是
		// flag 变量，而客户端配置只从 flag/environment 读取。
		// 不回填的后果是"配置文件里写了 proxy_ip 却不生效"，只能靠命令行或环境变量，
		// 而安装器/服务模式恰恰不会带命令行参数 —— 表现为"地区一直解析不出来"。
		if cfg.ProxyIP != "" && *proxyIP == os.Getenv("MOCENTER_PROXY_IP") {
			*proxyIP = cfg.ProxyIP
		}
		// MMDB 路径同样必须回填到 flag（与 ProxyIP 同理：安装器/服务模式
		// 不带命令行参数，配置文件是唯一的来源）。
		if cfg.MMDBPath != "" && *mmdbPath == os.Getenv("MOCENTER_MMDB") {
			*mmdbPath = cfg.MMDBPath
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
		if cfg.RequireAgentCert {
			*requireAgentCert = true
		}
		if cfg.AdminTLSDays != 0 {
			*adminTLSDays = cfg.AdminTLSDays
		}
		if cfg.ServerName != "" && *serverName == "" {
			*serverName = cfg.ServerName
		}
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。密码/TOTP 不落盘。
	{
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		// flag 名 -> (配置字段指针, 命令行变量值)
		field := map[string]func(){
			"listen":             func() { cfg.Listen = *addr },
			"http":               func() { cfg.HTTP = *httpAddr },
			"whitelist":          func() { cfg.Whitelist = *whitelist },
			"web":                func() { cfg.WebDir = *webDir },
			"web-tls-cert":       func() { cfg.WebTLSCert = *webTLSCert },
			"web-tls-key":        func() { cfg.WebTLSKey = *webTLSKey },
			"tls-cert":           func() { cfg.TLSCert = *tlsCert },
			"tls-key":            func() { cfg.TLSKey = *tlsKey },
			"ca-cert":            func() { cfg.CACert = *caCert },
			"insecure":           func() { cfg.Insecure = *insecure },
			"data":               func() { cfg.DataDir = *dataDir },
			"user":               func() { cfg.User = *user },
			"password-file":      func() { cfg.AdminPasswordFile = *passwordFile },
			"pkg-dir":            func() { cfg.PkgDir = *pkgDir },
			"agent-key":          func() { cfg.AgentKey = *agentKey },
			"proxy-ip":           func() { cfg.ProxyIP = *proxyIP },
			"mmdb":               func() { cfg.MMDBPath = *mmdbPath },
			"token-ttl":          func() { cfg.TokenTTL = tokenTTL.String() },
			"require-agent-cert": func() { cfg.RequireAgentCert = *requireAgentCert },
			"admin-tls-days":     func() { cfg.AdminTLSDays = *adminTLSDays },
			"server-name":        func() { cfg.ServerName = *serverName },
		}
		dirty := false
		for _, name := range []string{"listen", "http", "whitelist", "web", "web-tls-cert", "web-tls-key", "tls-cert", "tls-key", "ca-cert", "insecure", "data", "user", "password-file", "pkg-dir", "agent-key", "proxy-ip", "mmdb", "token-ttl", "require-agent-cert", "admin-tls-days", "server-name"} {
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

	log.Printf("[server] %s 启动中（数据目录 %s）", version.Full(), *dataDir)

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

	// 内置 CA：Agent 通道/Web 控制台证书的签发者，也是"零配置 TLS"的信任根。
	// 首次启动自动生成到 <data>/ca.crt + ca.key（0600），之后保持不变 ——
	// 它一变，所有 Agent 的 pin 都要更新（见 -server-name 与升级说明）。
	identityName := strings.TrimSpace(*serverName)
	if identityName == "" {
		identityName = "MoCenter"
	}
	identity, identityCreated, idErr := agentauth.LoadOrCreateIdentity(*dataDir, identityName)
	if idErr != nil {
		log.Printf("[server] 警告: 初始化内置 CA 失败: %v（将退回手工证书/明文）", idErr)
	} else if identityCreated {
		log.Printf("[server] 已生成部署身份 CA 并写入 %s（权限 0600）；身份名 %q，指纹 %s",
			filepath.Join(*dataDir, agentauth.CACertFileName), identity.Name(),
			agentauth.DisplayFingerprint(identity.Fingerprint()))
	} else {
		// 身份名以已存在的 CA 为准（改动它会让指纹变化，进而让所有 Agent 需要更新 pin）
		_ = identityName
		log.Printf("[server] 部署身份 CA: %s（身份名 %q，指纹 %s）",
			filepath.Join(*dataDir, agentauth.CACertFileName), identity.Name(),
			agentauth.DisplayFingerprint(identity.Fingerprint()))
	}

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
	// 与 Agent 同理：非 Windows 平台此前完全没有这层保护（Linux 上定时任务保活
	// 会拉出多个 Server，直接损坏数据库），现由 flock 覆盖。
	// 同理不去终止已有实例，只让本次启动退出。
	singleKey := winapp.SingletonKey("Server", *configPath)
	release, ok := winapp.AcquireSingleton(singleKey, *configPath)
	if !ok {
		log.Fatalf("已有一个使用同一配置(%s)的 Server 在运行，本次启动终止（%s）",
			*configPath, winapp.DescribeSingleton(singleKey, *configPath))
	}
	defer release()
	log.Printf("[server] 已取得单实例锁：%s", winapp.DescribeSingleton(singleKey, *configPath))

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
	geoResolver := geo.NewResolverWithProxy(*proxyIP)
	// 离线 GeoIP 库（可选）：配置了就用本地库优先解析，解掉外部接口限流与
	// 无外网部署下的"地区全空"问题。加载失败只告警并降级，不影响启动。
	if strings.TrimSpace(*mmdbPath) != "" {
		if err := geoResolver.SetMMDB(*mmdbPath); err != nil {
			log.Printf("[geo] 离线 GeoIP 库加载失败（将降级使用外部接口）: %v", err)
		} else if geoResolver.MMDBLoaded() {
			log.Printf("[geo] 离线 GeoIP 已启用: %s（本地解析优先，未命中才走外部接口）", geoResolver.MMDBPath())
		}
	} else {
		log.Printf("[geo] 未配置离线 GeoIP 库（-mmdb / 配置项 mmdb_path）；地区解析走外部接口，" +
			"无外网或接口限流时可能出现「地区不显示」。可下载 GeoLite2-Country.mmdb 后配置该项")
	}
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

	// 初始口令的来源优先级：口令文件 > -password。
	//
	// 口令文件是**推荐**路径：命令行参数会被 `ps` 与 `systemctl show -p ExecStart`
	// 暴露给任意本地用户，等于把后台口令写进服务单元。这里只做"读一次"：
	// 账号一旦存在，文件内容不再被使用（见下方 HasUsers 判断），
	// 因此用户改过口令后重启服务不会被文件里的旧值覆盖回去。
	//
	// fail-closed：文件存在但读不动/解析失败时**不静默回退**到 -password 或随机口令，
	// 而是明确报错并退出 —— 否则运维以为在用文件里的口令，实际用的是别的，
	// 只会在"登录不上"时才发现，且极难定位。
	// Load 必须先于下面所有 HasUsers 判断：账号是从 users.json 读进来的，
	// 不先 Load 的话 HasUsers() 永远返回 false，于是每次重启都会走"首次创建"分支
	// —— 结果是启动时读口令文件（文件已删/权限变化就启动失败），
	// 而且看起来像"账号没被记住"。
	if err := userMgr.Load(); err != nil {
		log.Fatalf("加载账号文件 %s 失败: %v", filepath.Join(*dataDir, "users.json"), err)
	}

	if pf := strings.TrimSpace(*passwordFile); pf != "" {
		if userMgr.HasUsers() {
			// 已有账号：**不读、也不校验**口令文件。读取会因权限/内容问题
			// 在"其实用不到它"的机器上导致启动失败；而即使读到也不会生效 ——
			// 账号以 users.json 里的哈希为准，这里只提示可以删掉这个文件了。
			log.Printf("已存在账号，忽略初始口令文件 %s（该文件仅在首次创建账号时使用，可安全删除）", pf)
		} else {
			pw, ferr := readPasswordFile(pf)
			if ferr != nil {
				log.Fatalf("读取初始口令文件 %s 失败: %v（该文件应为仅 root 可读的 0600 文件）", pf, ferr)
			}
			*password = pw
			log.Printf("已从口令文件 %s 读取初始口令（仅本次创建账号使用，登录后请立即修改并删除该文件）", pf)
		}
	}

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

	// 地理位置异步解析完成后补推一次主机列表。
	// 解析走外部接口（可达数秒），因此列表推送先返回缓存、解析完成后自动补一帧，
	// 而不是在推送路径上同步等待 —— 后者是首屏长时间空白的主因。
	store.SetGeoNotify(func() { hub.Broadcast(protocol.TypeAgentList, store.All()) })

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
	// 延迟监测（ICMP/TCP/HTTP）：查询历史曲线 + 下发/查看探测目标
	mux.HandleFunc("/api/latency", latencyHandler(store, authz))
	mux.HandleFunc("/api/latency/targets", latencyTargetsHandler(store, authz))
	// 版本信息（服务端版本 + 各 Agent 在线版本与升级判定）
	mux.HandleFunc("/api/version", versionHandler(store, geoResolver, authz))
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
	// Agent 接入信息：让运维不必翻服务器终端就能拿到"一条命令"。
	// 属于敏感信息（密钥等同于 Agent 身份凭证），因此必须登录后访问。
	mux.HandleFunc("/api/server-info", serverInfoHandler(authz, identity, verifier, *addr, *dataDir, *requireAgentCert))

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

	// 备份 / 恢复（P1）：导出数据目录为 zip、查看包内清单、从包恢复
	mux.HandleFunc("/api/backup", backupHandler(*dataDir, authz, siteAudit))
	// 一键部署 Agent：SSH 登录目标机并安装 Agent、写入对应配置（Linux）
	mux.HandleFunc("/api/agents/deploy", deployHandler(agentdeploy.New(agentdeploy.Options{
		DataDir:    *dataDir,
		PkgDir:     *pkgDir,
		ServerAddr: *addr,
		AgentKey:   agentKeyVal,
		ProxyIP:    *proxyIP,
		TLSCert:    *tlsCert,
		TLSKey:     *tlsKey,
		CACert:     *caCert,
		MMDBPath:   *mmdbPath,
	}), authz, siteAudit))

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
			// Web 控制台（Admin：登录口令 + 会话 Token + 远程终端/VNC）**默认走 HTTPS/WSS**：
			//   - 显式给了 -web-tls-cert/-web-tls-key：用运维提供的证书；
			//   - 否则用内置 CA 现场签发一张"当前部署临时证书"（内存态、重启即换、不落盘）。
			// 自签证书会让浏览器提示"不安全"，这不是安全问题而是信任展示问题：
			// 首次访问点一次"继续"即可，或用 -web-tls-cert 换正式证书 / 前面挂 TLS 反代。
			// 这样默认部署下口令与 Token 不再明文上网，也就能连带启用 HSTS。
			if *webTLSCert != "" && *webTLSKey != "" {
				cert, err := tls.LoadX509KeyPair(*webTLSCert, *webTLSKey)
				if err != nil {
					log.Fatalf("[server] 加载 Web TLS 证书失败: %v", err)
				}
				serveAdminTLS(*httpAddr, securityHeaders(mux), cert, true)
				return
			}
			if identity != nil && *adminTLSDays > 0 {
				hosts := adminCertHosts(*httpAddr, *serverName)
				cert, err := identity.NewAdminCert(hosts)
				if err == nil {
					log.Printf("[server] Web 控制台使用内置 CA 自动签发的临时证书（%d 天，SAN: %v）；"+
						"浏览器会提示自签名证书，接受即可；如需受信任证书请配置 -web-tls-cert/-web-tls-key",
						*adminTLSDays, hosts)
					serveAdminTLS(*httpAddr, securityHeaders(mux), cert, false)
					return
				}
				log.Printf("[server] 警告: 生成 Web 控制台临时证书失败: %v", err)
			}
			log.Printf("[server] REST API / Web 控制台监听 %s（**明文 HTTP**）", *httpAddr)
			if !isLoopbackAddr(*httpAddr) {
				log.Printf("[server] 警告: Web 控制台未启用 TLS，登录口令与 Token 将明文传输。" +
					"请去掉 -admin-tls-days=0，或配置 -web-tls-cert/-web-tls-key，或确保只通过 TLS 反向代理提供")
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

		// Agent 通道的 TLS 采取"能开就开、开不了才降级"：
		//   - 显式给了 -tls-cert/-tls-key：用运维提供的证书；
		//   - 否则用内置 CA 现场签发（CA 首次启动自动生成，无需任何证书文件）；
		//   - 只有 identity 初始化失败（例如 CA 私钥不可读）才退回明文并告警。
		// 之所以默认开：明文链路上注册凭证与后续报文都可被嗅探/篡改，
		// 而"默认不开"正是此前需要手工铺证书、结果大家干脆不铺的原因。
		var tlsLn net.Listener
		// 自动签发的服务端证书必须带上「本机真实地址」，否则 Agent 用 -ca-cert 校验时
		// 会报 x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>。
		// 这里复用 Web 控制台的地址汇总（监听地址 + 本机所有网卡 + -server-name），
		// 并把探测到的公网 IP 一并写入 —— 它是 Agent 最常使用的连接地址。
		agentCertHosts := adminCertHosts(*addr, *serverName)
		if publicIP := detectPublicIP(); publicIP != "" {
			agentCertHosts = append(agentCertHosts, publicIP)
			log.Printf("[server] 探针到本机公网 IP %s，已加入服务端证书 SAN", publicIP)
		}
		agentCert, usingAutoCert, certErr := identity.ServerCertificate(*tlsCert, *tlsKey, agentCertHosts...)
		if certErr != nil {
			if *tlsCert != "" || *tlsKey != "" {
				log.Fatalf("[server] %v", certErr)
			}
			tlsLn = ln
			log.Printf("[server] 警告: 自动生成 Agent 通道证书失败（%v），该通道为明文传输", certErr)
		} else {
			noClientCert := !*requireAgentCert
			if *caCert != "" {
				// 手工指定 CA：沿用旧语义（必须携带由该 CA 签发的客户端证书）
				caPEM, rerr := os.ReadFile(*caCert)
				if rerr != nil {
					log.Fatalf("[server] 读取 CA 证书失败: %v", rerr)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(caPEM) {
					log.Fatalf("[server] 解析 CA 证书失败")
				}
				tlsCfg := &tls.Config{
					Certificates: []tls.Certificate{agentCert},
					MinVersion:   tls.VersionTLS12,
					ClientCAs:    pool,
					ClientAuth:   tls.RequireAndVerifyClientCert,
				}
				if identity != nil && identity.Authority() != nil {
					// 内置 CA 的证书同样被接受（Agent 换取来的证书由内置 CA 签发）
					pool.AddCert(identity.Authority())
				}
				tlsLn = tls.NewListener(ln, tlsCfg)
				log.Printf("[server] Agent TCP 已启用 mTLS 双向认证（校验客户端证书，CA: %s）", *caCert)
			} else {
				tlsLn = tls.NewListener(ln, identity.CATLSConfig(&agentCert))
				if noClientCert {
					log.Printf("[server] Agent TCP 已启用 TLS；允许无证书的 Agent 用注册密钥现场换取证书（换取后必须携带）")
				} else {
					log.Printf("[server] Agent TCP 已启用 mTLS 双向认证（内置 CA: %s，指纹 %s）",
						identity.Name(), agentauth.DisplayFingerprint(identity.Fingerprint()))
				}
			}
			if usingAutoCert {
				log.Printf("[server] Agent 通道使用内置 CA 自动签发的服务端证书（SAN: %v）；无需运维准备证书，换用自有证书可加 -tls-cert/-tls-key",
					agentCertHosts)
			} else {
				// 运维用 -tls-cert 提供的证书：其 SAN 决定 Agent 能否用 -ca-cert 校验通过。
				// 部署后换过公网 IP / 用域名连接是最常见的连不上原因，这里在启动时
				// 就把"证书里的地址"打出来，免得排障时只能靠 Agent 侧报错反推。
				logCertSANWarning(agentCert, *addr, agentCertHosts)
			}
			_ = *insecure
		}

		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				log.Printf("[server] accept error: %v", err)
				continue
			}
			go handleAgentConn(conn, sess, store, hub, router, ft, pm, pushMgr, vncMgr, *pkgDir, verifier, *allowAnonAgents, pub, identity, !*requireAgentCert)
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

// serveAdminTLS 以 HTTPS 提供 Web 控制台。
//
// trusted=false 表示用的是"当前部署临时证书"（自签），此时额外加上 HSTS 之外的说明日志；
// 两者都加 HSTS：Web 侧默认已经不再有明文入口，HSTS 不会把自己锁死。
func serveAdminTLS(addr string, handler http.Handler, cert tls.Certificate, trusted bool) {
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	webSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if trusted {
		log.Printf("[server] REST API / Web 控制台监听 %s（TLS）", addr)
	} else {
		log.Printf("[server] REST API / Web 控制台监听 %s（TLS，自签名证书；浏览器首次访问需确认信任）", addr)
	}
	if err := webSrv.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[server] https error: %v", err)
	}
}

// adminCertHosts 汇总 Web 控制台临时证书的 SAN：
// 监听地址的 IP/主机名 + 本机探测到的所有地址 + 可选 -server-name。
// 证书是内存态、每次启动重签，所以地址变化不需要重签文件。
func adminCertHosts(listenAddr, serverName string) []string {
	var hosts []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, h := range hosts {
			if h == v {
				return
			}
		}
		hosts = append(hosts, v)
	}
	if _, port, err := net.SplitHostPort(listenAddr); err == nil && port != "" {
		add(listenAddrOf(listenAddr))
	}
	add(serverName)
	if ifaces, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaces {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP != nil {
				add(ipnet.IP.String())
			}
		}
	}
	add("localhost")
	return hosts
}

// logCertSANWarning 打印手工证书的 SAN，并在"证书地址与本机实际地址不一致"时告警。
//
// 场景：运维用 -tls-cert 指定自有证书，之后服务器换了公网 IP（或 Agent 改用域名连接），
// 证书里的旧 SAN 不再匹配 —— 此时 Agent 会报
//
//	x509: certificate is valid for 127.0.0.1, ::1, not 82.156.7.85
//
// 与其等对方来问，不如启动时主动提示"怎么重签"。
func logCertSANWarning(cert tls.Certificate, listenAddr string, hosts []string) {
	if len(cert.Certificate) == 0 {
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, d := range leaf.DNSNames {
		have[d] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	san := make([]string, 0, len(have))
	for _, d := range leaf.DNSNames {
		san = append(san, d)
	}
	for _, ip := range leaf.IPAddresses {
		san = append(san, ip.String())
	}
	log.Printf("[server] Agent 通道使用手工指定的证书（SAN: %v）", san)

	// 只在"证书里的地址"完全覆盖不到本机实际地址"时告警：说明 Agent 用这些地址
	// 连接（含 -ca-cert 校验）必然失败。
	local := map[string]bool{}
	if h := listenAddrOf(listenAddr); h != "" {
		local[h] = true
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" && h != "0.0.0.0" && h != "::" {
			local[h] = true
		}
	}
	var missing []string
	for h := range local {
		if !have[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	log.Printf("[server] 警告: 该证书的 SAN 不含本机地址 %v。"+
		"Agent 若用这些地址连接并用 -ca-cert 校验服务端证书，会报 "+
		"\"x509: certificate is valid for ..., not <地址>\"。重签方式（自动按本机公网 IP 写入 SAN）: "+
		"cd scripts/signcert && go run . -ip %s -server-config <服务端配置文件>；或给 Agent 配 server_fingerprint 免 CA 校验",
		missing, strings.Join(missing, ","))
}

// detectPublicIP 探测本机公网出口 IPv4，用于写进服务端证书 SAN。
//
// 为什么值得探测：部署在云主机上时，"Agent 该连哪个地址"往往只有出口 IP 说了算，
// 而证书里若没有它就必然校验失败（这正是"服务端换了 IP 就连不上"的成因）。
// 探测失败（内网机器 / 出口被拦 / 启动太早）不是错误：返回空串，调用方照常签发，
// 由本机网卡地址与 -server-name 兜底。
func detectPublicIP() string {
	client := &http.Client{Timeout: 4 * time.Second}
	urls := []string{
		"https://api.ipify.org?format=json",
		"https://ipv4.ident.me/",
		"https://api.ip.sb/ip",
	}
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if rerr != nil {
			continue
		}
		if ip := publicIPFromEcho(string(body)); ip != "" {
			return ip
		}
	}
	return ""
}

// publicIPFromEcho 从回显服务响应里取出公网 IP（支持 JSON 与纯文本），
// 并滤掉私网/回环/非法值 —— 误把 10.x 写进 SAN 比不写更糟（看着"配了"其实没用）。
func publicIPFromEcho(body string) string {
	text := strings.TrimSpace(body)
	candidates := []string{}
	// JSON: {"ip":"1.2.3.4"} / {"query":"1.2.3.4"}
	var m map[string]interface{}
	if json.Unmarshal([]byte(text), &m) == nil {
		for _, k := range []string{"ip", "query", "IPv4"} {
			if v, ok := m[k].(string); ok {
				candidates = append(candidates, v)
			}
		}
	}
	candidates = append(candidates, strings.Fields(text)...)
	for _, c := range candidates {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(c), `"',[]`))
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
			continue
		}
		return ip.String()
	}
	return ""
}

// listenAddrOf 去掉 host:port 的端口部分；空主机视为 0.0.0.0（不作为 SAN，交由地址探测补齐）。
func listenAddrOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "0.0.0.0" || host == "::" || host == "" {
		return ""
	}
	return host
}

// certPEMChain 把 tls.Certificate 的证书链编码为 PEM 文本。
func certPEMChain(cert tls.Certificate) string {
	var b strings.Builder
	for _, der := range cert.Certificate {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	return b.String()
}

// mustJSON 序列化协议结构体（失败时返回空 JSON，避免让调用方处理错误分支）。
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// sendEnrollRequired 告知对端"你的客户端证书不被本套 CA 认可，请重新换取"。
//
// 带上当前部署身份指纹与身份名，客户端据此可以：
//   - 判断这是"服务端换过 CA"（而不是网络问题）；
//   - 在获得运维授权（server_fingerprint 一致）时自动完成重新换取。
//
// 这是唯一会在"尚未通过注册鉴权"的连接上下发的消息，内容全部可公开（不含任何密钥）。
func sendEnrollRequired(conn net.Conn, identity *agentauth.Identity) {
	resp := protocol.EnrollRequiredResp{
		Reason:      "客户端证书不被本套 CA 认可",
		Hint:        "删除本地 creds/ 后用注册密钥重新换取，或更新 server_fingerprint",
		Identity:    identityNameOf(identity),
		Fingerprint: identity.Fingerprint(),
	}
	if err := json.NewEncoder(conn).Encode(protocol.Envelope{
		Type: protocol.TypeEnrollRequired, Data: mustJSON(resp),
	}); err != nil {
		log.Printf("[server] 通知 %s 重新换取证书失败: %v", conn.RemoteAddr(), err)
	}
}

// peerCert 取出对端客户端证书（无证书返回 nil）。
func peerCert(conn net.Conn) *x509.Certificate {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return nil
	}
	_ = tc.SetReadDeadline(time.Time{})
	state := tc.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0]
}

// identityNameOf 取部署身份名（用于日志）。
func identityNameOf(id *agentauth.Identity) string {
	if id == nil {
		return "（未启用）"
	}
	return id.Name()
}

// peerCertCN 取出对端客户端证书的 CN（无证书返回 false）。
func peerCertCN(conn net.Conn) (string, bool) {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return "", false
	}
	_ = tc.SetReadDeadline(time.Time{})
	state := tc.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", false
	}
	leaf := state.PeerCertificates[0]
	if leaf.Subject.CommonName == "" {
		return "", false
	}
	return leaf.Subject.CommonName, true
}

// tlsInfo 生成日志用的连接描述（含是否加密、对端证书身份）。
func tlsInfo(conn net.Conn) string {
	addr := conn.RemoteAddr().String()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return addr + " 明文"
	}
	st := tc.ConnectionState()
	desc := addr + " TLS"
	if len(st.PeerCertificates) > 0 {
		desc += "，客户端证书 CN=" + st.PeerCertificates[0].Subject.CommonName
	}
	return desc
}

// unregisteredHint 给出"连接已建立但未完成注册"的可能原因（拼到日志尾部）。
//
// 三种典型情形：
//   - 握手阶段就失败：对端出示的客户端证书不被本机 CA 认可（最常见：服务端换过
//     CA / Agent 未清掉旧 creds）或根本没带证书但服务端要求强制；
//   - 握手成功但没走到上线消息：对端连上后在等待/发别的东西（换取证书失败等）；
//   - 其它：保持沉默。
func unregisteredHint(conn net.Conn) string {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	st := tc.ConnectionState()
	if !st.HandshakeComplete {
		return "；TLS 握手未完成 —— 通常是客户端证书不被本机 CA 认可（服务端换过 CA？" +
			"让 Agent 删除 creds/ 重新用注册密钥换取，或把新指纹填进 Agent 的 server_fingerprint），" +
			"也可能是端口收到了非 TLS 流量（例如 HTTP 探测）"
	}
	if len(st.PeerCertificates) == 0 {
		return "；连接未携带客户端证书（Agent 尚未换取证书，或换取失败）"
	}
	return "；已携带客户端证书 CN=" + st.PeerCertificates[0].Subject.CommonName +
		"，但未发送上线消息（注册鉴权被拒？详见上方 [server] 拒绝 Agent 日志）"
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
//     "身份取自连接而不是取自报文"的关键，否则伪造 AgentID 仍能冒用他人；
//   - "简单模式"：Agent 可在 TLS 上先发 EnrollReq（同一套 HMAC 证明）换取
//     由内置 CA 签发的客户端证书，之后每次连接都携带证书（收敛为 mTLS），
//     运维因此不必手工分发 client.crt/client.key/ca.crt。
//
// 传输层策略：TLS 连接要求注册报文必须带客户端证书（换取阶段除外）；
// 明文连接只在 allowNoCert 且服务端确实未能启用 TLS 时放行。
func handleAgentConn(conn net.Conn, sess *session.Manager, store *probe.Store, hub *ws.Hub, router *route.Router, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr, pkgDir string, verifier *agentauth.Verifier, allowAnon bool, pub *publicSite, identity *agentauth.Identity, allowNoCert bool) {
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
		// 换取客户端证书：允许在**尚无证书**的 TLS 连接上发起（这正是它的用途）。
		if env.Type == protocol.TypeEnrollReq {
			var er protocol.EnrollReq
			if err := json.Unmarshal(env.Data, &er); err != nil {
				continue
			}
			resp := protocol.EnrollResp{}
			if !validAgentID(er.AgentID) {
				resp.Message = "agent_id 非法"
			} else if err := verifier.Verify(er.AgentID, er.Nonce, er.Proof, er.TS); err != nil {
				log.Printf("[server] 拒绝 Agent %q 换取证书: %v（来自 %s）", er.AgentID, err, tlsInfo(conn))
				resp.Message = "注册凭证校验失败: " + err.Error()
			} else if identity == nil {
				resp.Message = "服务端未启用内置 CA，无法签发证书"
			} else if certPEM, cerr := identity.AgentCertPEM(er.AgentID, er.CSR); cerr != nil {
				resp.Message = "签发证书失败: " + cerr.Error()
			} else {
				resp.OK = true
				resp.Message = "证书已签发"
				resp.CertPEM = certPEM
				resp.CAPEM = string(identity.CACertPEM())
				resp.ServerFingerprint = identity.Fingerprint()
				resp.Identity = identity.Name()
				log.Printf("[server] 已为 Agent %q 签发客户端证书（来自 %s）", er.AgentID, tlsInfo(conn))
			}
			_ = json.NewEncoder(conn).Encode(protocol.Envelope{Type: protocol.TypeEnrollResp, Data: mustJSON(resp)})
			if !resp.OK {
				return
			}
			continue
		}
		// 客户端证书存在时，身份由证书 CN 提供（换取来的证书 CN 即 agent_id）；
		// 没有证书的连接只能靠注册报文里的 HMAC 证明 —— 后者是"简单模式"的引导路径。
		//
		// **必须验链**：Agent 通道用的是 tls.RequestClientCert（为兼容新 Agent 首次换取），
		// 这种模式下 Go 不会在握手阶段校验客户端证书，只要能被解析就会出现在
		// PeerCertificates 里。旧实现直接取 CN 当身份，于是：
		//   · 服务端换过 CA 后，Agent 交上来的旧证书会被当成"已验证身份"；
		//   · 任何自签一张 CN=<主机名> 的证书都能冒充该主机（比"没带证书"更危险，
		//     因为它看起来是"已认证"的，日志里也不会出现任何拒绝记录）。
		// 因此这里显式校验签发链，只认可本套 CA 签发的证书。
		if peer := peerCert(conn); peer != nil {
			if identity != nil && identity.CertSignedByThisCA(peer) {
				sessConn.CertCN = peer.Subject.CommonName
			} else {
				log.Printf("[server] 拒绝来自 %s 的连接：客户端证书 CN=%q 不是本套 CA（%s）签发的"+
					"（服务端换过 CA？请让该 Agent 删除 creds/ 后用注册密钥重新换取，"+
					"或把新指纹填进它的 server_fingerprint）",
					conn.RemoteAddr(), peer.Subject.CommonName, identityNameOf(identity))
				// 主动告知原因再关闭：否则客户端只会看到"连上又断开、"
				// 服务端只留下一行"未完成注册"，两边日志对不上，运维只能人肉猜。
				// 更重要的是：客户端据此可以**自动**丢弃旧证书并重新换取，
				// 而不用登录每台被控机删 creds/。
				sendEnrollRequired(conn, identity)
				return
			}
		}
		if !allowNoCert && sessConn.CertCN == "" {
			log.Printf("[server] 拒绝未携带有效客户端证书的连接（-require-agent-cert）：%s（来自: %s）。"+
				"新装 Agent 请先在不带该开关的情况下完成一次证书换取，或为其预置 -client-cert/-client-key",
				"该 Agent 尚未持有证书", tlsInfo(conn))
			return
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
		// 身份来自连接的客户端证书（若携带）时，以证书为准；
		// 否则要求报文里的 HMAC 证明（自报身份 + 持有注册密钥）。
		if sessConn.CertCN != "" && sessConn.CertCN != info.AgentID {
			log.Printf("[server] 拒绝 Agent %q 注册: 客户端证书身份为 %q，与自报身份不符（来自 %s）",
				info.AgentID, sessConn.CertCN, tlsInfo(conn))
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
		// 这条日志以前只说"未完成注册"，运维看不出到底是什么问题 ——
		// 而线上最常见的成因是**服务端换过 CA（重装/换机器）后，Agent 还拿着旧
		// 客户端证书**：TLS 握手失败的那一侧只有客户端有明确报错，服务端这边
		// 往往只剩"连接建立又断开"，于是双方日志对不上、只能人肉猜。
		// 这里补上"对端证书是谁签的/有没有握手失败痕迹"，让一行日志就能定性。
		log.Printf("[server] Agent 连接未完成注册，关闭: %s%s", conn.RemoteAddr(), unregisteredHint(conn))
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
	// 走 pub.visibleNodes 而非直接 BuildNodes：流量累计值需要按配置口径装配
	nodes := pub.visibleNodes(cfg)
	hub.BroadcastPublic(map[string]interface{}{
		"ok":      true,
		"nodes":   nodes,
		"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
	})
}

// --- REST 处理器 ---

// --- REST 处理器 ---

// serverInfoHandler 返回「Agent 接入」所需的信息：服务端地址、身份指纹、注册密钥与
// 可直接粘贴的安装命令。目的是把"配证书"从手工散落多文件的流程，收敛成一次复制。
func serverInfoHandler(authz *auth.Auth, identity *agentauth.Identity, verifier *agentauth.Verifier, listenAddr, dataDir string, requireCert bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agentKey := ""
		if verifier != nil && verifier.Enabled() {
			// 显式配置的密钥只存在于内存/环境变量，这里只回传可读文件里的那份
			if b, err := os.ReadFile(filepath.Join(dataDir, agentauth.KeyFileName)); err == nil {
				agentKey = strings.TrimSpace(string(b))
			}
		}
		resp := map[string]interface{}{
			"listen":                listenAddr,
			"require_agent_cert":    requireCert,
			"agent_key":             agentKey,
			"agent_key_configured":  verifier != nil && verifier.Enabled(),
			"agent_key_fingerprint": verifier.KeyFingerprint(),
		}
		if identity != nil {
			resp["identity"] = identity.Name()
			resp["fingerprint"] = identity.Fingerprint()
			resp["fingerprint_display"] = agentauth.DisplayFingerprint(identity.Fingerprint())
			resp["ca_cert_path"] = filepath.Join(dataDir, agentauth.CACertFileName)
		}
		writeJSON(w, resp)
	}
}

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
// latencyHandler GET /api/latency?agent=<id>&range=24h&target=<目标ID>
//
// 返回某主机的延迟历史曲线。响应结构对每个目标给出：
//
//	{ target, kind, points: [{ts, ms, ok}] }
//
// 失败的探测点**保留在序列里**（ok=false, ms=0），由前端画成断点/超时标记 ——
// 若直接丢弃失败点，曲线会把"这段时间一直超时"画成"没有采样"，
// 而这两者在运维判断上完全不同（前者是故障，后者是 Agent 没上报）。
func latencyHandler(store *probe.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
		if agentID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if store.Get(agentID) == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机不存在"})
			return
		}
		window := parseLatencyRange(r.URL.Query().Get("range"))
		target := strings.TrimSpace(r.URL.Query().Get("target"))
		limit := 2000
		if v, err := strconv.Atoi(r.URL.Query().Get("max")); err == nil && v > 0 && v <= 5000 {
			limit = v
		}
		rows := store.LatencyHistory(agentID, time.Now().Add(-window), target, limit)
		series := map[string]*latencySeries{}
		order := []string{}
		for _, row := range rows {
			if row == nil {
				continue
			}
			ser, ok := series[row.Target]
			if !ok {
				ser = &latencySeries{Target: row.Target, Kind: row.Kind, Points: []latencyPoint{}}
				series[row.Target] = ser
				order = append(order, row.Target)
			}
			ser.Points = append(ser.Points, latencyPoint{TS: row.Timestamp, MS: roundMS(row.LatencyMS), OK: row.OK})
		}
		out := make([]*latencySeries, 0, len(order))
		for _, t := range order {
			out = append(out, series[t])
		}
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"agent":   agentID,
			"targets": store.LatencyTargets(agentID),
			"series":  out,
		})
	}
}

// latencyPoint 单个延迟采样点。
type latencyPoint struct {
	TS int64   `json:"ts"`
	MS float64 `json:"ms"`
	// OK=false 表示本次探测失败/超时（ms 无意义，前端应画断点）
	OK bool `json:"ok"`
}

// latencySeries 一个目标的延迟序列。
type latencySeries struct {
	Target string         `json:"target"`
	Kind   string         `json:"kind"`
	Points []latencyPoint `json:"points"`
}

// roundMS 延迟保留 1 位小数。
func roundMS(v float64) float64 {
	if v < 0 {
		return 0
	}
	return float64(int64(v*10+0.5)) / 10
}

// parseLatencyRange 解析延迟趋势时间窗（形如 1h/6h/24h/7d），非法值回退 24h。
// 上限 30 天，与历史留存窗口一致 —— 请求更长的窗口只会扫到空数据。
func parseLatencyRange(v string) time.Duration {
	v = strings.TrimSpace(strings.ToLower(v))
	def := 24 * time.Hour
	max := 30 * 24 * time.Hour
	if v == "" {
		return def
	}
	unit := v[len(v)-1]
	num, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || num <= 0 {
		return def
	}
	var d time.Duration
	switch unit {
	case 'h':
		d = time.Duration(num) * time.Hour
	case 'd':
		d = time.Duration(num) * 24 * time.Hour
	case 'm':
		d = time.Duration(num) * time.Minute
	default:
		return def
	}
	if d > max {
		return max
	}
	return d
}

// latencyTargetsHandler GET/POST /api/latency/targets
//
//	GET  ?agent=<id>          查看该主机当前生效的目标
//	POST { agent_id, targets } 下发目标（写入内存 + 转发给在线的 Agent）
//
// 目标格式为 "icmp:1.1.1.1" / "tcp:1.1.1.1:443" / "http:https://example.com"。
// 校验放在服务端：非法格式在这里被拦住，否则 Agent 侧只能整条忽略，
// 运维看到的是"配了但没生效"且无从排查。
func latencyTargetsHandler(store *probe.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
			if agentID == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]interface{}{
				"ok": true, "agent": agentID, "targets": store.LatencyTargets(agentID),
			})
		case http.MethodPost, http.MethodPut:
			var body struct {
				AgentID string   `json:"agent_id"`
				Targets []string `json:"targets"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			agentID := strings.TrimSpace(body.AgentID)
			if agentID == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
				return
			}
			if store.Get(agentID) == nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机不存在"})
				return
			}
			cleaned, verr := normalizeLatencyTargets(body.Targets)
			if verr != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": verr.Error()})
				return
			}
			store.SetLatencyTargets(agentID, cleaned)
			// 在线主机：等下一轮探针上报即可（Agent 会在采集时读配置）。
			// 这里不做实时下发（那需要新增协议消息），因为延迟探测本身就是
			// 周期性指标，等一个采集周期（默认 10s）是符合预期的。
			writeJSON(w, map[string]interface{}{
				"ok": true, "agent": agentID, "targets": cleaned,
				"msg": "目标已保存，将在 Agent 下一个采集周期（默认 10 秒）生效",
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// maxLatencyTargets 单机延迟目标上限（与 agent 侧 latency.maxTargets 同口径）。
const maxLatencyTargets = 8

// normalizeLatencyTargets 校验并归一化延迟目标。
func normalizeLatencyTargets(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(raw) > 512 {
			return nil, fmt.Errorf("目标超长（最长 512 字符）: %.64s…", raw)
		}
		kind, addr := splitLatencySpec(raw)
		if addr == "" {
			return nil, fmt.Errorf("目标缺少地址: %s", raw)
		}
		if strings.ContainsAny(addr, " \t\r\n") {
			return nil, fmt.Errorf("目标地址含空白字符: %s", raw)
		}
		id := kind + ":" + addr
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) > maxLatencyTargets {
			return nil, fmt.Errorf("延迟目标最多 %d 个", maxLatencyTargets)
		}
	}
	return out, nil
}

// splitLatencySpec 拆分 "kind:addr"，只在前缀是已知 kind 时按 kind 拆分
// （http/https 目标自身含冒号，不能简单按第一个冒号切）。
func splitLatencySpec(s string) (kind, addr string) {
	if i := strings.Index(s, ":"); i > 0 {
		p := strings.ToLower(s[:i])
		if p == "icmp" || p == "tcp" || p == "http" {
			if p == "http" {
				p = "http"
			}
			return p, strings.TrimSpace(s[i+1:])
		}
	}
	return "icmp", strings.TrimSpace(s)
}

// versionHandler GET /api/version
//
// 返回服务端版本 + 每个 Agent 上报的版本。
// 前端「关于」页与主机列表都读它，避免版本口径在不同页面各写一遍。
//
// 关于 "outdated"：Agent 版本与服务端版本**互相独立**，因此不能把
// "与 server_version 不同" 直接说成"需要升级" —— 那会把正常状态误报成异常
// （Agent 走在服务端前面、或服务端刚发版而 Agent 尚未跟进，都是常态）。
// 这里返回的 outdated 只表示"该 Agent 版本号与服务端当前版本号不同"，
// 是否需要升级由运维自行判断；字段名保留是为了兼容既有前端。
func versionHandler(store *probe.Store, resolver *geo.Resolver, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		agents := store.All()
		items := make([]map[string]interface{}, 0, len(agents))
		outdated := 0
		for _, a := range agents {
			if a == nil {
				continue
			}
			cur := strings.TrimSpace(a.AgentVersion)
			// 只做"版本号是否与服务端当前版本不同"的事实陈述，不代表升级建议：
			// Agent 独立发版，版本不同属正常。空版本 = 旧版 Agent 未上报。
			differs := cur != version.Version
			if differs {
				outdated++
			}
			items = append(items, map[string]interface{}{
				"agent_id": a.AgentID, "remark": a.Remark, "hostname": a.Hostname,
				"online": a.Online, "agent_version": cur,
				"outdated": differs, "version_known": cur != "",
			})
		}
		// 离线 GeoIP 库状态：部署后无法只看日志确认"是否真的生效"，
		// 这里给出可查询的状态（是否加载 / 库文件路径）。
		mmdbLoaded, mmdbPath := false, ""
		if resolver != nil {
			mmdbLoaded, mmdbPath = resolver.MMDBLoaded(), resolver.MMDBPath()
		}
		writeJSON(w, map[string]interface{}{
			"ok":                 true,
			"server_version":     version.Version,
			"mmdb_loaded":        mmdbLoaded,
			"mmdb_path":          mmdbPath,
			"product":            version.Name,
			"agent_total":        len(items),
			"agent_outdated":     outdated,
			"agents":             items,
			"upgrade_rule":       "升级包的 version 表示本次要升到的版本，与 server_version 无关（不填则默认取 server_version）",
			"version_policy":     "Agent 版本与 Server 版本互相独立：两者不做一致性校验，版本高低都属于正常状态",
			"release_discipline": "版本号仅在明确发版时修改（见各自的 internal/version）",
		})
	}
}

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
		// Agent 版本与 Server 版本互相独立：这里**不做**"升级包版本必须等于
		// 服务端版本"之类的强制校验。Agent 的发布节奏可以与服务端不同步，
		// 运维也经常需要灰度「先把一部分 Agent 升到 x.y.z」——强制对齐只会挡路。
		//
		// 这个字段的含义仅是"本次要升到哪个版本"，由 Agent 侧用于判断是否跳过：
		// Agent 只比较它与自身版本是否相等，相等则跳过（除非 force）。
		// 因此它可以是任何合法版本号，与当前服务端版本没有关系。
		//
		// 未填写时自动补当前服务端版本，仅作为"顺手给个默认值"，方便只改 Agent
		// 且与服务端同步发版的场景；它不代表任何约束。
		if v := strings.TrimSpace(body.Version); v == "" {
			body.Version = version.Version
		} else {
			body.Version = v
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
		// Web 控制台默认就是 HTTPS（见 serveAdminTLS），所以可以安全地启用 HSTS：
		// 它会阻止后续降级到明文，保护 Bearer Token 与远程终端/远程桌面的会话。
		// 注意：只在 TLS 请求上加，避免反代外的明文态被浏览器记住。
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
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

// readPasswordFile 读取初始口令文件。
//
// 兼容两种写法，都是运维顺手就会写出来的形式：
//
//	纯口令一行           -> 直接用它
//	KEY=VALUE 形式       -> 取 MOCENTER_PASSWORD / PASSWORD / ADMIN_PASSWORD / 任意 *_PASSWORD 的值
//
// 这样 `install-linux.sh` 生成的文件、以及手工 `echo 'MOCENTER_PASSWORD=x' > f`
// 都能直接喂进来，不必先教会用户"格式不对"。
//
// 另外做两个安全约束：
//
//	· 文件不能是"人人可读"（含 group/other 读位）—— 口令文件可读意味着口令泄露，
//	  宁可启动失败也要让运维先修权限；
//	· 空口令直接报错，落到 auth 层会变成一个"空密码可用"的坏账号。
func readPasswordFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是目录，不是文件", path)
	}
	// Windows 上 Mode() 的权限位语义与 Unix 不同（永远带 0666 左右的位），
	// 因此只在类 Unix 平台做这个检查。
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm&0077 != 0 {
			return "", fmt.Errorf("%s 的权限为 %04o，同组/其他用户可读；请改为 0600（chmod 600 %s）", path, perm, path)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	pw := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			key := strings.ToUpper(strings.TrimSpace(line[:eq]))
			val := strings.TrimSpace(line[eq+1:])
			val = strings.Trim(val, `"'`)
			// 只认"像口令"的键名，避免把 USER=admin 这类行误当口令
			if key == "PASSWORD" || key == "MOCENTER_PASSWORD" || key == "ADMIN_PASSWORD" || strings.HasSuffix(key, "_PASSWORD") {
				pw = val
				break
			}
			continue
		}
		pw = strings.Trim(line, `"'`)
		break
	}
	if strings.TrimSpace(pw) == "" {
		return "", fmt.Errorf("%s 中未找到有效口令（空文件？）", path)
	}
	return pw, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveSPA 托管 SPA 静态资源，并对非 API/WS 路径回退到 index.html。
//
// 移动端（尤其 Chrome 145+）加载慢的三个原因，这里都做了处理：
//  1. **压缩传输**：优先回传构建期预压缩产物（.br/.gz，见
//     scripts/deploy/precompress-web.sh）。主包压缩后 868KB -> 283KB(gzip)/约240KB(br)，
//     弱网移动端的差别是数倍；缺失预压缩产物时按 Accept-Encoding 走标准协商。
//  2. **缓存头**：带内容哈希的 /assets/* 直接 immutable 长缓存（不再回源校验），
//     index.html 则 no-cache —— 否则每次导航都要发一次条件请求，手机端串行等待明显。
//  3. **正确 MIME**：模块脚本必须是 JS MIME，类型不对浏览器会拒绝执行。
func serveSPA(mux *http.ServeMux, webDir string) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// API 与 WS 有独立路由（ServeMux 精确匹配优先），到这里说明未命中，返回 404
		if strings.HasPrefix(p, "/api") || strings.HasPrefix(p, "/ws") || strings.HasPrefix(p, "/vnc-ws") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// 根路径与目录都用应用外壳（SPA 由前端路由接管）
		if p == "/" || p == "" {
			serveIndexHTML(w, r, webDir)
			return
		}
		rel := filepath.Clean("/" + p)
		filePath := filepath.Join(webDir, rel)
		// 目录穿越兜底：Clean 后仍越界一律 404（不回退 index.html，
		// 否则 /../etc/passwd 之类的探测会拿到 200，掩盖真实意图）
		root := filepath.Clean(webDir) + string(os.PathSeparator)
		if !strings.HasPrefix(filePath, root) {
			http.NotFound(w, r)
			return
		}
		if fi, err := os.Stat(filePath); err == nil && !fi.IsDir() {
			serveStaticFile(w, r, filePath, fi, false)
			return
		}
		// SPA 路由回退：未知路径交给前端路由
		serveIndexHTML(w, r, webDir)
	})
}

// serveIndexHTML 返回应用外壳。入口文件不缓存，保证发版后立刻生效。
func serveIndexHTML(w http.ResponseWriter, r *http.Request, webDir string) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	serveStaticFile(w, r, filepath.Join(webDir, "index.html"), nil, true)
}

// serveStaticFile 输出静态文件：压缩协商 + 缓存头。
// entry=true 表示这是 SPA 入口（含任意路由回退），一律 no-cache。
func serveStaticFile(w http.ResponseWriter, r *http.Request, path string, fi os.FileInfo, entry bool) {
	if fi == nil {
		st, err := os.Stat(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		fi = st
	}
	// 带内容哈希的静态资源（vite 产物在 /assets/ 下）永久缓存
	if !entry && (strings.HasPrefix(r.URL.Path, "/assets/") || hasContentHash(filepath.Base(path))) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if ct := mimeByExt(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// 预压缩产物优先：省去每次请求的压缩开销，也避免弱网下反复传输大包
	if enc := pickEncoding(r.Header.Get("Accept-Encoding")); enc.name != "" {
		if cand := path + "." + enc.ext; fileExists(cand) {
			w.Header().Set("Content-Encoding", enc.name)
			w.Header().Set("Vary", "Accept-Encoding")
			http.ServeFile(w, r, cand)
			return
		}
	}
	http.ServeFile(w, r, path)
}

type encoding struct {
	name string // Content-Encoding 值
	ext  string // 预压缩产物后缀
}

// pickEncoding 解析 Accept-Encoding，支持 br / gzip（br 优先，体积更小）。
func pickEncoding(header string) encoding {
	h := strings.ToLower(header)
	if h == "" {
		return encoding{}
	}
	if strings.Contains(h, "br") {
		return encoding{name: "br", ext: "br"}
	}
	if strings.Contains(h, "gzip") {
		return encoding{name: "gzip", ext: "gz"}
	}
	return encoding{}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// hasContentHash 判断文件名是否带内容哈希（vite 产物形如 index-CY0gmwch.js）。
// 误判会把可变文件永久缓存，因此要求哈希段是字母数字且长度合理。
func hasContentHash(name string) bool {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	i := strings.LastIndex(base, "-")
	if i < 0 || i == len(base)-1 {
		return false
	}
	h := base[i+1:]
	if len(h) < 5 || len(h) > 24 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// mimeByExt 指定常见前端资源的 MIME。
// .js 必须是 JS MIME，否则 module script 会被浏览器拒绝执行。
func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	default:
		return ""
	}
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

// probeHistoryCleaner 定期清理超过 30 天的探针 / 延迟历史数据。
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
		// 延迟历史与探针历史同口径（30 天）：都是趋势数据，留存窗口不一致
		// 会导致"趋势图有数据、延迟曲线却空了一段"这种口径错位。
		m, lerr := db.CleanupOldLatencyHistory(30)
		if lerr != nil {
			log.Printf("[server] 清理延迟历史失败: %v", lerr)
		} else if m > 0 {
			log.Printf("[server] 清理了 %d 条过期延迟历史", m)
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
