// MoCenter 总控服务端入口。
//
// 职责：
//   - 监听 TCP 端口接收内网 Agent 的主动长连接
//   - 维护在线主机会话池，处理心跳、探针、命令、交互、文件传输
//   - 提供 WebSocket 端点供 Web 前端通信
//   - 提供 REST API（鉴权、主机列表、命令下发、端口映射管理等）
//   - 端口映射反向代理与连接跟踪
//
// 用法：go run . 或 ./server -listen :9000 -http :8080 -user admin -password-file <文件>
// 登录凭证只有一种：用户名 + 密码（可选 OTP）。首次启动未指定 -user/-password 时
// 会创建 admin 与**随机**初始口令（写到 <data>/initial-password.txt，0600），
// 不再使用固定弱口令。
package main

import (
	"bufio"
	"crypto/ed25519"
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
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"mocenter/server/internal/share"
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
	// 反代场景下的"哪几层代理可信"（L-7）。默认空 = 完全不信任 X-Forwarded-For，
	// 所有安全判定按 TCP 对端（与旧版一致）；填了它才按 XFF 还原真实客户端地址。
	trustedProxies := flag.String("trusted-proxies", os.Getenv("MOCENTER_TRUSTED_PROXIES"),
		"反向代理的可信网段（逗号分隔 CIDR 或裸 IP，如 127.0.0.1,10.0.0.0/8）。"+
			"仅在前面挂了自有反代时配置：配置后 IP 白名单/限速/审计来源按 X-Forwarded-For 还原的客户端地址判定；"+
			"留空表示不信任任何代理（默认，按 TCP 对端判定）")
	webDir := flag.String("web", os.Getenv("MOCENTER_WEB_DIR"), "Web 静态资源目录，留空则不托管前端")
	tlsCert := flag.String("tls-cert", "", "Agent TCP 连接 TLS 证书文件路径")
	tlsKey := flag.String("tls-key", "", "Agent TCP 连接 TLS 私钥文件路径")
	caCert := flag.String("ca-cert", "", "mTLS 客户端 CA 证书文件路径（提供后启用双向认证，Agent 需携带客户端证书）")
	requireAgentCert := flag.Bool("require-agent-cert", false, "要求 Agent 必须携带客户端证书（默认允许无证书连接自行换取，兼容旧版 Agent）")
	disableSharedAgentKey := flag.Bool("disable-shared-agent-key", false, "关闭部署级共享注册密钥这条注册路径，只接受 per-agent 一次性注册令牌（现网全部迁移后再开）")
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
	// VNC 端口闸门：默认只放行 RFB 约定端口段，显式传空串才放开（=不限制）。
	// 默认值取内置常量，运维在生成的配置文件里能直接看到它，改起来不用查文档。
	defaultVncPorts := vnc.DefaultPortSpec
	if v := os.Getenv("MOCENTER_VNC_PORTS"); v != "" {
		defaultVncPorts = v
	}
	vncPorts := flag.String("vnc-ports", defaultVncPorts,
		"VNC 隧道/探测可指向的目标端口（逗号分隔，支持区间，如 5900-5999,6900）。"+
			"VNC 隧道连的是被管主机的回环，放开任意端口就等于给每个登录会话一台内网端口代理；"+
			"显式传空串表示不限制（该状态不会存进配置文件，需每次启动显式给出）")
	upgradePubKey := flag.String("upgrade-pubkey", os.Getenv("MOCENTER_UPGRADE_PUBKEY"),
		"升级包 Ed25519 公钥（hex 或公钥文件路径）：一键部署时写进 Agent 配置，"+
			"并在下发升级时拦下缺签名的请求。私钥**不**在本机，签包是离线动作")
	flag.Parse()

	// 空串参数归一（原因见 flags.go）。-vnc-ports 排除在外：它的帮助文本明写
	// 「显式传空串表示不限制」，那是一个有效取值，不能被当成"未配置"。
	blankFlags := normalizeBlankFlags(flag.CommandLine, "vnc-ports")
	// 命令行是否**显式给出**某参数。配置回填必须让位给它：
	// `-admin-tls-days 0` 这类"显式关掉"的取值恰好等于…不，它的默认值是 30，
	// 于是"值等于默认就当没传"的老写法会把运维的显式意图吞掉 ——
	// 表现是"命令行写了 -admin-tls-days 0 却仍然起 TLS"，只能改配置文件。
	givenFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { givenFlags[f.Name] = true })
	clearBlankExplicit(givenFlags, blankFlags)

	// 加载配置：首次生成，后续读取并让命令行显式参数覆盖
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg == nil {
		cfg = &config.Config{
			Listen:         *addr,
			HTTP:           *httpAddr,
			Whitelist:      *whitelist,
			WebDir:         *webDir,
			TrustedProxies: *trustedProxies,
			TLSCert:        *tlsCert,
			TLSKey:         *tlsKey,
			CACert:         *caCert,
			Insecure:       *insecure,
			DataDir:        *dataDir,
			User:           *user,
			// 不落盘明文密码与 TOTP 密钥：账号以哈希持久化到 users.json
			Password:          "",
			TOTPSecret:        "",
			AdminPasswordFile: *passwordFile,
			PkgDir:            *pkgDir,
			AgentKey:          *agentKey,
			ProxyIP:           *proxyIP,
			MMDBPath:          *mmdbPath,
			WebTLSCert:        *webTLSCert,
			WebTLSKey:         *webTLSKey,
			TokenTTL:          tokenTTL.String(),
			RequireAgentCert:  *requireAgentCert,
			// 一次性注册令牌的迁移终点开关：打开后共享注册密钥不再能注册任何主机。
			DisableSharedAgentKey: *disableSharedAgentKey,
			AdminTLSDays:          *adminTLSDays,
			ServerName:            *serverName,
			VncPorts:              *vncPorts,
			UpgradePubKey:         *upgradePubKey,
		}
		if err := config.Save(*configPath, cfg); err != nil {
			log.Printf("警告: 写入配置文件 %s 失败: %v", *configPath, err)
		} else {
			log.Printf("已生成配置文件: %s（后续可直接运行 %s）", *configPath, os.Args[0])
		}
	} else {
		// 已有配置：命令行显式参数（非默认值）覆盖配置文件
		// 控制台接管过监听地址时，配置文件优先于服务命令里的 -listen/-http；
		// 否则服务重启会把界面上改过的地址又覆盖回安装时的旧值。
		if cfg.Listen != "" && (cfg.ListenManaged || *addr == ":9000") {
			*addr = cfg.Listen
		}
		if cfg.HTTP != "" && (cfg.HTTPManaged || *httpAddr == ":8080") {
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
		// 可信代理网段同样要从配置回填：服务模式不带命令行参数，
		// 不回填就表现为"配了 trusted_proxies 却不生效"。
		if cfg.TrustedProxies != "" && *trustedProxies == os.Getenv("MOCENTER_TRUSTED_PROXIES") {
			*trustedProxies = cfg.TrustedProxies
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
		if cfg.DisableSharedAgentKey {
			*disableSharedAgentKey = true
		}
		if cfg.AdminTLSDays != 0 && !givenFlags["admin-tls-days"] {
			*adminTLSDays = cfg.AdminTLSDays
		}
		if cfg.ServerName != "" && *serverName == "" {
			*serverName = cfg.ServerName
		}
		if cfg.VncPorts != "" && *vncPorts == defaultVncPorts {
			*vncPorts = cfg.VncPorts
		}
		if cfg.UpgradePubKey != "" && *upgradePubKey == os.Getenv("MOCENTER_UPGRADE_PUBKEY") {
			*upgradePubKey = cfg.UpgradePubKey
		}
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。密码/TOTP 不落盘。
	{
		// 与上面的配置回填共用同一份「显式给出」判定（已剔除被归一化的空值参数）
		explicit := givenFlags
		// flag 名 -> (配置字段指针, 命令行变量值)
		field := map[string]func(){
			"listen": func() {
				if !cfg.ListenManaged {
					cfg.Listen = *addr
				}
			},
			"http": func() {
				if !cfg.HTTPManaged {
					cfg.HTTP = *httpAddr
				}
			},
			"whitelist":                func() { cfg.Whitelist = *whitelist },
			"trusted-proxies":          func() { cfg.TrustedProxies = *trustedProxies },
			"web":                      func() { cfg.WebDir = *webDir },
			"web-tls-cert":             func() { cfg.WebTLSCert = *webTLSCert },
			"web-tls-key":              func() { cfg.WebTLSKey = *webTLSKey },
			"tls-cert":                 func() { cfg.TLSCert = *tlsCert },
			"tls-key":                  func() { cfg.TLSKey = *tlsKey },
			"ca-cert":                  func() { cfg.CACert = *caCert },
			"insecure":                 func() { cfg.Insecure = *insecure },
			"data":                     func() { cfg.DataDir = *dataDir },
			"user":                     func() { cfg.User = *user },
			"password-file":            func() { cfg.AdminPasswordFile = *passwordFile },
			"pkg-dir":                  func() { cfg.PkgDir = *pkgDir },
			"agent-key":                func() { cfg.AgentKey = *agentKey },
			"proxy-ip":                 func() { cfg.ProxyIP = *proxyIP },
			"mmdb":                     func() { cfg.MMDBPath = *mmdbPath },
			"token-ttl":                func() { cfg.TokenTTL = tokenTTL.String() },
			"require-agent-cert":       func() { cfg.RequireAgentCert = *requireAgentCert },
			"disable-shared-agent-key": func() { cfg.DisableSharedAgentKey = *disableSharedAgentKey },
			"admin-tls-days":           func() { cfg.AdminTLSDays = *adminTLSDays },
			"server-name":              func() { cfg.ServerName = *serverName },
			"vnc-ports":                func() { cfg.VncPorts = *vncPorts },
			"upgrade-pubkey":           func() { cfg.UpgradePubKey = *upgradePubKey },
		}
		dirty := false
		for _, name := range []string{"listen", "http", "whitelist", "trusted-proxies", "web", "web-tls-cert", "web-tls-key", "tls-cert", "tls-key", "ca-cert", "insecure", "data", "user", "password-file", "pkg-dir", "agent-key", "proxy-ip", "mmdb", "token-ttl", "require-agent-cert", "disable-shared-agent-key", "admin-tls-days", "server-name", "vnc-ports", "upgrade-pubkey"} {
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
	// 密钥是否由启动参数/配置文件显式给出：显式给出时它会盖掉 <data>/agent.key，
	// 控制台轮换后必须提醒运维同步改这里，否则下次重启悄悄退回旧密钥。
	agentKeyExplicit := agentKeyVal != ""
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

	// per-agent 一次性注册令牌 + 证书 TOFU 绑定（<data>/enroll.json，0600）。
	//
	// 为什么要有它：共享注册密钥是"一把钥匙认全网"，任何一台被攻破的机器都能拿它
	// 申请一张 CN = 受害者 agent_id 的合法客户端证书。令牌把信任粒度收回
	// "一台一份、一次一用"，TOFU 绑定再挡住"用泄露的共享密钥给别人的 ID 换新生证书"。
	enrollStore, ensErr := agentauth.NewEnrollStore(filepath.Join(*dataDir, agentauth.EnrollStoreFileName))
	if ensErr != nil {
		log.Printf("[server] 警告: 注册令牌存储不可用（%v）—— 本次启动只接受共享注册密钥，"+
			"「Agent 接入」页的令牌签发功能将返回错误", ensErr)
	} else {
		verifier.SetEnrollStore(enrollStore)
	}
	if *disableSharedAgentKey {
		verifier.DisableSharedKey()
	}

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
	switch {
	case !verifier.Enabled():
		if *disableSharedAgentKey {
			log.Fatalf("[server] 已要求「只接受一次性注册令牌」，但令牌存储不可用（见上方警告），" +
				"且共享密钥路径已关闭 —— 没有任何可用的注册凭据，拒绝启动")
		}
		log.Printf("[server] 警告: Agent 注册鉴权已关闭（-allow-unauthenticated-agents）——"+
			"任何能连通 %s 的主机都可注册为 Agent 或冒用他人 AgentID", *addr)
	case !verifier.SharedKeyEnabled():
		log.Printf("[server] Agent 注册鉴权已启用：**仅接受 per-agent 一次性注册令牌**" +
			"（共享注册密钥路径已由 -disable-shared-agent-key 关闭）。在「Agent 接入」页为每台主机签发令牌，" +
			"或调用 POST /api/enroll/tokens")
	default:
		log.Printf("[server] Agent 注册鉴权已启用。推荐做法：在 Web 控制台「Agent 接入」页为每台主机签发"+
			"**一次性注册令牌**（一台一份、用后即废），填进该机的 agent_key / enrollment_secret；"+
			"当前仍同时接受部署级共享注册密钥（指纹 %s，来自 %s 或 -agent-key），"+
			"它是一把钥匙认全网 —— 泄露任一台即可冒名注册任意主机，"+
			"请在现网全部迁移到令牌后用 -disable-shared-agent-key 关闭",
			verifier.KeyFingerprint(), agentKeyPath)
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
	// 端口闸门在启动时生效：配置写错应当是"起不来"，而不是"以为已收紧、实际还在放行"。
	if err := vncMgr.SetAllowedPorts(*vncPorts); err != nil {
		log.Fatalf("VNC 端口配置无效: %v", err)
	}
	// 生效范围要在启动日志里看得见：闸门放开后如果只体现在"某个端口连得上"，
	// 运维很难察觉它已经不在默认状态了。
	if spec := strings.TrimSpace(*vncPorts); spec != "" {
		log.Printf("[vnc] 隧道目标端口闸门: %s", spec)
	} else {
		log.Printf("[vnc] 隧道目标端口闸门已放开（任意端口），建议用 -vnc-ports 收回 RFB 区间")
	}

	hub := ws.NewHub()

	// 「邀请远程管理」的分享链接存储（<data>/shares.json，0600）。
	// 加载失败只降级本功能：主机监控/命令/桌面等既有能力不应因一份分享文件损坏而停服。
	shareStore, shareErr := share.NewStore(filepath.Join(*dataDir, share.StoreFileName))
	if shareErr != nil {
		log.Printf("[server] 警告: 分享存储不可用（%v）—— 本次启动「分享控制」将返回错误", shareErr)
		shareStore = nil
	}
	shareSvc := &shareAPI{
		store: shareStore, probes: store, authz: authz, router: router,
		hub: hub, vnc: vncMgr, sess: sess, pm: pm,
	}

	// 地理位置异步解析完成后补推一次主机列表。
	// 解析走外部接口（可达数秒），因此列表推送先返回缓存、解析完成后自动补一帧，
	// 而不是在推送路径上同步等待 —— 后者是首屏长时间空白的主因。
	store.SetGeoNotify(func() { hub.Broadcast(protocol.TypeAgentList, store.All()) })

	// --- 公开站点（免登录，Issue #5）---
	// 注意：这些端点不经过 guard，一切数据都取自 publicview 的脱敏视图，
	// agent_id / uuid / 内网地址永不出网。
	//
	// node_id 的派生私钥（<data>/node.key，首次启动随机生成）：
	// 公开页上唯一对外的主机标识必须是**带密钥**的派生值，否则匿名访客可以拿一批
	// 候选主机名离线算编号、再用 samples 接口的 404/200 反查"哪些机器在册"。
	nodeKeys, nodeKeysCreated, nkErr := publicview.LoadNodeKeys(filepath.Join(*dataDir, publicview.NodeKeyFileName))
	if nkErr != nil {
		log.Fatalf("[server] 读取/生成公开节点编号密钥失败: %v", nkErr)
	}
	if nodeKeysCreated {
		log.Printf("[server] 已生成公开节点编号密钥 %s（0600）。删除它即可整体更换 node 编号：公开页的历史数据不受影响（内部仍按 agent_id 存），但已分享的 /public/node/… 链接会全部失效",
			filepath.Join(*dataDir, publicview.NodeKeyFileName))
	}
	pub := &publicSite{
		store:    store,
		cfg:      siteCfg,
		guard:    auth.NewLoginGuard(),
		hub:      hub,
		authz:    authz,
		nodeKeys: nodeKeys,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
			// 只认同源。原先写的是"公开页可能被嵌在其它站点里，允许任意来源"，
			// 但那条理由站不住：iframe 里的 socket 带的是**被嵌入文档自己**的 Origin
			// （我们的页面），不是外层站点的；而前端拼 URL 用的是 location.host
			// （api.js 的 PublicSocket），同源必然成立。收紧后依然能嵌入。
			// 反过来说，匿名端点上"任意 Origin 都能建一条长期订阅"是访客浏览器
			// 被任意站点驱使着打我们的连接数配额 —— 关掉它没有功能代价。
			CheckOrigin: func(r *http.Request) bool { return ws.SameOrigin(r) },
		},
	}

	// WebSocket 收到 Web 消息后的统一处理入口
	hub.SetMessageHandler(func(sessionKey, operator string, data []byte) {
		handleWebMessage(sessionKey, operator, data, sess, store, router, hub, ft, pm, pushMgr, vncMgr, shareStore)
	})

	// 主机列表的节流广播入口：探针这类高频数据源走 soon()（合并），
	// 上线/离线这类状态跃迁走 now()（必须立刻可见）。
	refresh := newAgentRefresh(func() { broadcastAgents(hub, store, pub) }, agentRefreshInterval)

	// 服务端启动后台任务
	go heartbeatSweeper(sess, store, refresh)
	go cleanupSweeper(pm, hub, ft, pushMgr)
	// 探针历史定期清理（保留 30 天）+ 审计日志留存清理（保留 180 天）
	go probeHistoryCleaner(db)
	go auditRetentionCleaner(db)
	go memStatsLogger()

	// 运行期可变的系统配置（会话时长、两个监听地址）：
	// 所有修改都经由同一把锁落盘，避免并发保存互相覆盖。
	settings := &settingsStore{path: *configPath, cfg: cfg}
	// listeners 在 start() 里赋值；HTTP 处理协程开始运行时它必然已就绪。
	var listeners *listenerHub

	// 启动 HTTP 服务（REST + WebSocket）
	mux := http.NewServeMux()
	// Agent 通道最终是否真的加密，要到 start() 里签证书时才知道（自动签发可能失败退回明文），
	// 而 /api/server-info 在此之前就要注册 —— 用原子量把结论带过去（L-8：这些"降级状态"
	// 必须能在后台被看见，只写在启动日志里等于没人看）。
	var agentChannelTLS atomic.Bool
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !guardGate(w, r, authz) {
			return
		}
		token := ws.WSToken(r)
		if authz.ValidateToken(token) {
			hub.HandleUpgrade(w, r, authz.ValidateToken, authz.OperatorOf)
			return
		}
		// 分享访客：凭分享 token 建立推送通道。这里只放行「远程终端」作用域，
		// 且只允许上行终端输入（见 handleWebMessage 的 share 分支）。
		if shareStore == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sh, err := shareStore.Validate(token, "", share.FeatureTerminal)
		if err != nil || store.Get(sh.AgentID) == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		hub.HandleUpgradeShare(w, r, sh.SessionKey, shareOperator(sh))
		// 到期主动收口：连接建立后不再有握手校验，必须有人按 ExpiresAt 关掉它。
		scheduleShareExpiry(sh, func() {
			hub.CloseSession(sh.SessionKey)
			closeShareCommands(router, sess, sh.SessionKey)
		})
	})
	mux.HandleFunc("/vnc-ws", func(w http.ResponseWriter, r *http.Request) {
		if !guardGate(w, r, authz) {
			return
		}
		token := ws.WSToken(r)
		if authz.ValidateToken(token) {
			vncMgr.HandleViewer(w, r, authz.ValidateToken)
			return
		}
		// 分享访客的桌面：主机与端口都必须与分享作用域完全一致，
		// 不能拿一条桌面分享去连别的主机或别的端口。
		if shareStore == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agentID := r.URL.Query().Get("agent")
		port := 5900
		if s := r.URL.Query().Get("port"); s != "" {
			if v, e := strconv.Atoi(s); e == nil && v > 0 {
				port = v
			}
		}
		sh, err := shareStore.Validate(token, agentID, share.FeatureVNC)
		if err != nil || store.Get(agentID) == nil || sh.VNCPort != port {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// HandleViewerScoped 会一直阻塞到查看器断开，因此到期收口必须在它之前登记。
		scheduleShareExpiry(sh, func() { vncMgr.CloseSession(sh.SessionKey) })
		vncMgr.HandleViewerScoped(w, r, func(tok, agent string, p int) bool {
			if tok != token || agent != agentID || p != port {
				return false
			}
			cur, verr := shareStore.Validate(tok, agent, share.FeatureVNC)
			return verr == nil && cur.VNCPort == p && store.Get(agent) != nil
		})
	})
	mux.HandleFunc("/api/command", commandHandler(router, authz))
	mux.HandleFunc("/api/command/batch", batchCommandHandler(router, authz))
	mux.HandleFunc("/api/command/close", commandCloseHandler(sess, authz))
	mux.HandleFunc("/api/vnc/probe", vncProbeHandler(vncMgr, authz))
	mux.HandleFunc("/api/vnc/install", vncInstallHandler(vncMgr, authz, router))
	mux.HandleFunc("/api/vnc/status", vncStatusHandler(vncMgr, authz))
	mux.HandleFunc("/api/vnc/password", vncPasswordHandler(vncMgr.ReadPassword, authz, router))
	mux.HandleFunc("/api/agents", agentsHandler(store, authz))
	mux.HandleFunc("/api/agents/history", historyHandler(store, authz))
	// 延迟监测（ICMP/TCP/HTTP）：查询历史曲线 + 下发/查看探测目标
	mux.HandleFunc("/api/latency", latencyHandler(store, authz))
	mux.HandleFunc("/api/latency/targets", latencyTargetsHandler(store, authz))
	// 版本信息（服务端版本 + 各 Agent 在线版本与升级判定）
	mux.HandleFunc("/api/version", versionHandler(store, geoResolver, authz))
	mux.HandleFunc("/api/agents/remark", setRemarkHandler(remarks, authz))
	mux.HandleFunc("/api/agents/delete", deleteAgentHandler(store, remarks, pm, authz, router, shareStore, hub, vncMgr))
	mux.HandleFunc("/api/audits", auditsHandler(router, authz))
	mux.HandleFunc("/api/portmap/rules", portmapRulesHandler(pm, authz))
	mux.HandleFunc("/api/portmap/op", portmapOpHandler(pm, authz, router))
	// 邀请远程管理：控制台侧创建/列举/撤销 + 访客侧凭分享 token 的能力入口。
	// 访客侧不走登录鉴权（没有账号），由各 handler 内的作用域校验把关。
	mux.HandleFunc("/api/shares", shareSvc.adminHandler())
	mux.HandleFunc("/api/shares/revoke", shareSvc.revokeHandler())
	mux.HandleFunc("/api/share/info", shareSvc.infoHandler())
	mux.HandleFunc("/api/share/command", shareSvc.commandHandler())
	mux.HandleFunc("/api/share/command/close", shareSvc.commandCloseHandler())
	mux.HandleFunc("/api/share/portmap/rules", shareSvc.rulesHandler())
	mux.HandleFunc("/api/share/portmap/op", shareSvc.portmapOpHandler())
	mux.HandleFunc("/api/share/vnc/password", shareSvc.vncPasswordHandler())
	mux.HandleFunc("/api/upgrade", upgradeHandler(sess, authz, *upgradePubKey))
	mux.HandleFunc("/api/login", loginHandler(userMgr, authz, loginGuard, router))
	mux.HandleFunc("/api/logout", logoutHandler(authz))
	// 修改 Web 监听端口后跨 origin 保持登录：一次性、短时效、绑定 IP 的交接码。
	mux.HandleFunc("/api/session/handoff", sessionHandoffCreateHandler(authz))
	mux.HandleFunc("/api/session/handoff/consume", sessionHandoffConsumeHandler(authz))
	mux.HandleFunc("/api/password", changePasswordHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/setup", otpSetupHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/enable", otpEnableHandler(userMgr, authz))
	mux.HandleFunc("/api/otp/disable", otpDisableHandler(userMgr, authz))
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// Agent 接入信息：让运维不必翻服务器终端就能拿到"一条命令"。
	// 需要登录，但**只含指纹等可公开线索**（密钥明文走下面的轮换接口）。
	mux.HandleFunc("/api/server-info", serverInfoHandler(authz, identity, verifier, func() string {
		if listeners != nil {
			return listeners.AgentAddr()
		}
		return *addr
	}, *dataDir, *requireAgentCert, *allowAnonAgents, *disableSharedAgentKey, agentChannelTLS.Load))
	// 一次性注册令牌：签发 / 列举 / 吊销 + 证书 TOFU 绑定重置（均需登录，且全部进审计）。
	mux.HandleFunc("/api/enroll/tokens", enrollTokensHandler(enrollStore, verifier, authz, router))
	mux.HandleFunc("/api/enroll/token/revoke", enrollRevokeHandler(enrollStore, authz, router))
	mux.HandleFunc("/api/enroll/token/delete", enrollDeleteHandler(enrollStore, store, authz, router))
	mux.HandleFunc("/api/enroll/cert/reset", enrollBindingResetHandler(enrollStore, authz, router))
	// 轮换部署级共享注册密钥：这是 M-1 之后唯一能拿到该密钥明文的出口。
	mux.HandleFunc("/api/enroll/shared-key/rotate", sharedKeyRotateHandler(verifier, authz, router, agentKeyPath, agentKeyExplicit))

	mux.HandleFunc(publicAPIBase+"/state", pub.stateHandler())
	mux.HandleFunc(publicAPIBase+"/nodes", pub.nodesHandler())
	mux.HandleFunc(publicAPIBase+"/samples", pub.samplesHandler())
	mux.HandleFunc(publicAPIBase+"/ws", pub.publicWSHandler())
	mux.HandleFunc(publicAPIBase+"/enabled", publicConfigStateHandler(siteCfg))
	// 品牌文案（标题/页脚）：登录页在登录前就要渲染，因此免鉴权；
	// 但它**不含**"公开页开没开"—— 那个答案归 /api/public/enabled，由开关把关。
	mux.HandleFunc("/api/branding", brandingHandler(siteCfg))
	// 配置变更 → ① 立刻把新口径推给已连接的访客与后台，② 通知在途匿名连接核对开关
	// （关掉公开页时它们必须自己结束，否则"停用"只挡住了新握手）。
	siteCfg.SetOnChange(func(*siteconfig.Config) {
		pub.noteConfigChange()
		broadcastAgents(hub, store, pub)
	})
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

	// 系统配置：会话保持时长 + 两个监听地址（均需登录；监听变更还要二次验密）。
	mux.HandleFunc("/api/system/config", systemConfigHandler(authz, settings, func() (string, string) {
		if listeners == nil {
			return "", ""
		}
		return listeners.AgentAddr(), listeners.HTTPAddr()
	}))
	mux.HandleFunc("/api/system/session", systemSessionHandler(authz, settings, siteAudit))
	mux.HandleFunc("/api/system/agent-listen", systemAgentListenHandler(authz, userMgr, settings,
		func() *listenerHub { return listeners }, sess, siteAudit))
	mux.HandleFunc("/api/system/http-listen", systemHTTPListenHandler(authz, userMgr, settings,
		func() *listenerHub { return listeners }, siteAudit))

	// 备份 / 恢复（P1）：导出数据目录为 zip、查看包内清单、从包恢复
	mux.HandleFunc("/api/backup", backupHandler(*dataDir, authz, siteAudit))
	// 一键部署 Agent：SSH 登录目标机并安装 Agent、写入对应配置（Linux）
	mux.HandleFunc("/api/agents/deploy", deployHandler(agentdeploy.New(agentdeploy.Options{
		DataDir:    *dataDir,
		PkgDir:     *pkgDir,
		ServerAddr: *addr,
		ServerAddrFunc: func() string {
			if listeners != nil {
				return listeners.AgentAddr()
			}
			return *addr
		},
		// 注册密钥取"当前值"而不是启动时的快照：控制台轮换共享密钥后，
		// 一键部署必须跟着写新密钥，否则装好的 Agent 会因旧密钥被拒注册，
		// 现象是"部署成功但主机永远不上线"。
		AgentKeyFunc: verifier.SharedKey,
		ProxyIP:      *proxyIP,
		// 升级公钥透传：没配的话装出来的 Agent 一律拒绝升级（Agent 侧无跳过开关），
		// 所以在生成的配置里保留这个键，让运维看得见要填什么。
		UpgradePubKey: *upgradePubKey,
		TLSCert:       *tlsCert,
		TLSKey:        *tlsKey,
		CACert:        *caCert,
		MMDBPath:      *mmdbPath,
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

	// 反代下的真实客户端地址（L-7）：只在最外层做一次归一，让白名单、限速、登录退避、
	// 审计来源、WS/VNC 握手、公开页并发都用同一个口径。
	// 未配置 -trusted-proxies 时 proxies 为空，这里直接返回原 handler —— 行为与从前一致。
	proxies, pxErr := auth.ParseProxyCIDRs(*trustedProxies)
	if pxErr != nil {
		log.Fatalf("[server] -trusted-proxies 无法解析: %v（应形如 127.0.0.1,10.0.0.0/8）", pxErr)
	}
	webHandler := auth.TrustProxyClients(proxies, securityHeaders(mux))
	if len(proxies) > 0 {
		log.Printf("[server] 已按可信代理还原客户端地址（网段: %s）；"+
			"X-Forwarded-For 只有来自这些网段的连接才会被采信", *trustedProxies)
	} else if *whitelist != "" {
		// 白名单 + 反代是最容易配坏的组合：此时所有访客都落在代理那一个 IP 上，
		// 白名单要么整体放行、要么把后台关死，而现象只表现为"偶发 403"。
		log.Printf("[server] 提示: 已配置 IP 白名单但未配置 -trusted-proxies；"+
			"若本站在反向代理之后，白名单/限速只能看到代理地址 %s。请把代理网段填进 -trusted-proxies", *whitelist)
	}

	// 监听与接收循环封装成函数：被 SCM 拉起时由服务启动回调调用。
	start := func() {
		// Web 控制台监听（Admin：登录口令 + 会话 Token + 远程终端/VNC）**默认走 HTTPS/WSS**：
		//   - 显式给了 -web-tls-cert/-web-tls-key：用运维提供的证书；
		//   - 否则用内置 CA 现场签发一张"当前部署临时证书"（内存态、重启即换、不落盘）。
		// 自签证书会让浏览器提示"不安全"，这不是安全问题而是信任展示问题：
		// 首次访问点一次"继续"即可，或用 -web-tls-cert 换正式证书 / 前面挂 TLS 反代。
		buildWebTLS := func(addr string) (*tls.Config, bool, error) {
			if *webTLSCert != "" && *webTLSKey != "" {
				cert, err := tls.LoadX509KeyPair(*webTLSCert, *webTLSKey)
				if err != nil {
					return nil, false, fmt.Errorf("加载 Web TLS 证书失败: %w", err)
				}
				return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, true, nil
			}
			if identity != nil && *adminTLSDays > 0 {
				hosts := adminCertHosts(addr, *serverName)
				cert, err := identity.NewAdminCert(hosts)
				if err == nil {
					log.Printf("[server] Web 控制台使用内置 CA 自动签发的临时证书（%d 天，SAN: %v）；"+
						"浏览器会提示自签名证书，接受即可；如需受信任证书请配置 -web-tls-cert/-web-tls-key",
						*adminTLSDays, hosts)
					return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, false, nil
				}
				log.Printf("[server] 警告: 生成 Web 控制台临时证书失败: %v", err)
			}
			return nil, false, nil
		}

		// buildHTTP 创建监听器；热切换与首次启动共用同一套 TLS 策略。
		buildHTTP := func(addr string) (net.Listener, *http.Server, error) {
			tlsCfg, trusted, err := buildWebTLS(addr)
			if err != nil {
				return nil, nil, err
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return nil, nil, err
			}
			if tlsCfg != nil {
				ln = tls.NewListener(ln, tlsCfg)
				if trusted {
					log.Printf("[server] REST API / Web 控制台监听 %s（TLS）", addr)
				} else {
					log.Printf("[server] REST API / Web 控制台监听 %s（TLS，自签名证书；浏览器首次访问需确认信任）", addr)
				}
			} else {
				log.Printf("[server] REST API / Web 控制台监听 %s（**明文 HTTP**）", addr)
				if !isLoopbackAddr(addr) {
					log.Printf("[server] 警告: Web 控制台未启用 TLS，登录口令与 Token 将明文传输。" +
						"请去掉 -admin-tls-days=0，或配置 -web-tls-cert/-web-tls-key，或确保只通过 TLS 反向代理提供")
				}
			}
			return ln, &http.Server{Handler: webHandler, ReadHeaderTimeout: 10 * time.Second}, nil
		}

		// 启动阶段探测一次公网 IP：写进证书 SAN，也作为监听配置的默认主机。
		if publicIP := detectPublicIP(); publicIP != "" {
			setPublicIP(publicIP)
			log.Printf("[server] 探针到本机公网 IP %s，已加入服务端证书 SAN", publicIP)
		}

		// buildAgent 创建 Agent TCP 监听（含 TLS/mTLS 包装）。
		// Agent 通道的 TLS 采取"能开就开、开不了才降级"：
		//   - 显式给了 -tls-cert/-tls-key：用运维提供的证书；
		//   - 否则用内置 CA 现场签发（CA 首次启动自动生成，无需任何证书文件）；
		//   - 只有 identity 初始化失败（例如 CA 私钥不可读）才退回明文并告警。
		buildAgent := func(addr string) (net.Listener, error) {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return nil, err
			}
			// 自动签发的服务端证书必须带上「本机真实地址」，否则 Agent 用 -ca-cert 校验时
			// 会报 x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>。
			agentCertHosts := adminCertHosts(addr, *serverName)
			if publicIP := cachedPublicIP(); publicIP != "" {
				agentCertHosts = append(agentCertHosts, publicIP)
			}
			agentCert, usingAutoCert, certErr := identity.ServerCertificate(*tlsCert, *tlsKey, agentCertHosts...)
			if certErr != nil {
				if *tlsCert != "" || *tlsKey != "" {
					_ = ln.Close()
					return nil, certErr
				}
				agentChannelTLS.Store(false)
				log.Printf("[server] 警告: 自动生成 Agent 通道证书失败（%v），该通道为明文传输", certErr)
				return ln, nil
			}
			agentChannelTLS.Store(true)
			noClientCert := !*requireAgentCert
			if *caCert != "" {
				// 手工指定 CA：沿用旧语义（必须携带由该 CA 签发的客户端证书）
				caPEM, rerr := os.ReadFile(*caCert)
				if rerr != nil {
					_ = ln.Close()
					return nil, fmt.Errorf("读取 CA 证书失败: %w", rerr)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(caPEM) {
					_ = ln.Close()
					return nil, fmt.Errorf("解析 CA 证书失败")
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
				log.Printf("[server] Agent TCP 已启用 mTLS 双向认证（校验客户端证书，CA: %s）", *caCert)
				return tls.NewListener(ln, tlsCfg), nil
			}
			tlsLn := tls.NewListener(ln, identity.CATLSConfig(&agentCert))
			if noClientCert {
				log.Printf("[server] Agent TCP 已启用 TLS；允许无证书的 Agent 用注册密钥现场换取证书（换取后必须携带）")
			} else {
				log.Printf("[server] Agent TCP 已启用 mTLS 双向认证（内置 CA: %s，指纹 %s）",
					identity.Name(), agentauth.DisplayFingerprint(identity.Fingerprint()))
			}
			if usingAutoCert {
				log.Printf("[server] Agent 通道使用内置 CA 自动签发的服务端证书（SAN: %v）；无需运维准备证书，换用自有证书可加 -tls-cert/-tls-key",
					agentCertHosts)
			} else {
				// 运维用 -tls-cert 提供的证书：其 SAN 决定 Agent 能否用 -ca-cert 校验通过。
				// 部署后换过公网 IP / 用域名连接是最常见的连不上原因，这里在启动时
				// 就把"证书里的地址"打出来，免得排障时只能靠 Agent 侧报错反推。
				logCertSANWarning(agentCert, addr, agentCertHosts)
			}
			_ = *insecure
			return tlsLn, nil
		}

		listeners = &listenerHub{
			onAgent: func(conn net.Conn) {
				handleAgentConn(conn, sess, store, hub, router, ft, pm, pushMgr, vncMgr, *pkgDir,
					verifier, *allowAnonAgents, identity, !*requireAgentCert, refresh)
			},
			buildAgent: buildAgent,
			buildHTTP:  buildHTTP,
		}
		if err := listeners.Start(*addr, *httpAddr); err != nil {
			log.Fatalf("[server] %v", err)
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
	// 监听器在后台协程里 accept，start() 会立即返回：
	// 普通进程必须在此阻塞，否则 main 返回会让整个服务退出。
	select {}
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

// 归属不符回传的留痕频率上限（每台 Agent 一条）。
// 被攻陷的 Agent 可以用每秒上千条假 command_id 回传来"报复"这条校验 ——
// 若每条都写审计，修复本身就成了灌满审计库的 DoS 面，故限频。
var cmdRejectMu sync.Mutex
var cmdRejectAt = make(map[string]time.Time)

// rejectUnownedCommand 丢弃一条不属于本连接的回执，并在日志与审计里留痕（限频）。
func rejectUnownedCommand(router *route.Router, c *session.Conn, commandID, kind string) {
	agentID := ""
	if c != nil {
		agentID = c.GetAgentID()
	}
	log.Printf("[server] 丢弃来自 %s 的%s：command_id=%q 从未下发给该连接（身份与归属不符）",
		agentID, kind, commandID)

	cmdRejectMu.Lock()
	now := time.Now()
	if last, seen := cmdRejectAt[agentID]; seen && now.Sub(last) < time.Minute {
		cmdRejectMu.Unlock()
		return
	}
	cmdRejectAt[agentID] = now
	for k, t := range cmdRejectAt {
		if now.Sub(t) > 10*time.Minute {
			delete(cmdRejectAt, k)
		}
	}
	cmdRejectMu.Unlock()

	router.Audit(&route.AuditLog{
		Time:        now.Unix(),
		Operator:    "unknown",
		AgentID:     agentID,
		CommandID:   commandID,
		Command:     "丢弃归属不符的" + kind,
		Blocked:     true,
		BlockReason: "command_ownership",
		Category:    "op",
	})
}

// logSafeMax 单条日志里外部文本的字节上限。
const logSafeMax = 96

// logSafe 把外部可控文本（Agent 自报的 UUID、主机名等）压成单行、限长后送进日志。
//
// 日志注入的后果不是"多打一行"：自报字段里塞一个 \n 就能伪造出
// "[server] Agent xxx 上线" 这类看着像服务端结论的记录，事后排查直接被带偏。
func logSafe(s string) string {
	var b strings.Builder
	truncated := false
	for i, r := range s {
		if i >= logSafeMax {
			truncated = true
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == ' ':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			b.WriteRune('·')
		default:
			b.WriteRune(r)
		}
	}
	if truncated {
		b.WriteString("…")
	}
	return b.String()
}

// uuidSuffix 在日志里补一句设备身份。UUID 来自 Agent 自报（经 TOFU 裁决），
// 仍需按 logSafe 处理后才能进日志。
func uuidSuffix(uuid string) string {
	if strings.TrimSpace(uuid) == "" {
		return ""
	}
	return "，设备 UUID " + logSafe(uuid)
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
// 本机公网 IP 的缓存。
//
// 为什么需要缓存：探测要联网、单个源 4 秒超时（三个源最坏 12 秒），
// 而「Agent 接入」页面要读它来拼部署命令 —— 每次请求都同步探测会让页面卡住。
// 所以：启动时探测一次（本来就要为证书 SAN 做），之后只在拿到空值时后台补探。
var (
	publicIPMu        sync.Mutex
	publicIPCached    string
	publicIPLastTry   time.Time
	publicIPDetecting bool
)

// publicIPRetryInterval 探测失败后的最小重试间隔。
// 启动时网络未就绪是常见情况，一次失败就永久放弃会让页面永远拿不到地址。
const publicIPRetryInterval = 60 * time.Second

// setPublicIP 记录启动阶段探测到的公网 IP。
func setPublicIP(ip string) {
	if ip == "" {
		return
	}
	publicIPMu.Lock()
	publicIPCached = ip
	publicIPMu.Unlock()
}

// cachedPublicIP 返回已缓存的公网 IP；未探测到返回空串（不阻塞）。
func cachedPublicIP() string {
	publicIPMu.Lock()
	defer publicIPMu.Unlock()
	return publicIPCached
}

// refreshPublicIP 在后台补探公网 IP（已有值或正在探测则直接返回）。
func refreshPublicIP() {
	publicIPMu.Lock()
	if publicIPCached != "" || publicIPDetecting {
		publicIPMu.Unlock()
		return
	}
	if !publicIPLastTry.IsZero() && time.Since(publicIPLastTry) < publicIPRetryInterval {
		publicIPMu.Unlock()
		return
	}
	publicIPLastTry = time.Now()
	publicIPDetecting = true
	publicIPMu.Unlock()

	ip := detectPublicIP() // 在锁外做，避免拖住其它读取方

	publicIPMu.Lock()
	publicIPDetecting = false
	if ip != "" {
		publicIPCached = ip
	}
	publicIPMu.Unlock()
}

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
			"让 Agent 删除 creds/client.crt 与 creds/ca.crt、保留 client.key 后用注册凭据重新换取，" +
			"或把新指纹填进 Agent 的 server_fingerprint），" +
			"也可能是端口收到了非 TLS 流量（例如 HTTP 探测）"
	}
	if len(st.PeerCertificates) == 0 {
		return "；连接未携带客户端证书（Agent 尚未换取证书，或换取失败）"
	}
	return "；已携带客户端证书 CN=" + st.PeerCertificates[0].Subject.CommonName +
		"，但未发送上线消息（注册鉴权被拒？详见上方 [server] 拒绝 Agent 日志）"
}

// validAgentID 校验 AgentID 是否可用。
//
// 允许字母、数字、`.` `-` `_` 与非 ASCII（兼容中文主机名 / 硬件 UUID），
// 但**显式拒绝 ASCII 元字符**（L-9）。控制字符与空白是旧版就挡的两类；这一层新挡的是
// shell 与标记语法：`< > " ' \` & ; | $ ( ) { } [ ] / \ : * ? ! # ~ ^ % @ + = ,`。
//
// 为什么现在才挡：这个 ID 会流进证书 CN、审计库、CSV 导出、前端界面与运维复制粘贴的
// shell 命令。今天每条出口都各自做了转义（参数化 SQL、csvCell、logSafe、React 转义、
// 部署侧另有 agentIDRe），所以它不是可利用漏洞；但"每个下游都要记得转义"正是会失守的
// 那种约定，而合法主机名从来不需要这些字符 —— 在身份入口收一次，比在每条出口各防一遍稳。
//
// 注意：这会拒绝**历史上带过这些字符**的身份重新上线。留空/改名的正确路径是重新签发
// 一次性令牌（ID 由 UUID 兜底），而不是放宽这里的字符集。
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
		if r < 0x80 && strings.IndexRune(agentIDForbiddenASCII, r) >= 0 {
			return false
		}
	}
	return true
}

// agentIDForbiddenASCII 是 agent_id 里一律不接受的 ASCII 元字符集合（见 validAgentID）。
const agentIDForbiddenASCII = "<>\"'&;|(){}[]/\\:*?!#~^%@+,=$`"

// enrollAgentCert 处理 Agent 的"用注册凭据换取客户端证书"请求（"简单模式"的签发端）。
//
// 签发前依次把关（顺序本身就是安全设计）：
//
//	a) agent_id 合法性 —— 拦住进入日志/审计/CN 的不可控内容；
//	b) 注册凭证（优先 per-agent 一次性令牌，其次过渡期共享密钥）。**先验凭证再往下看**：
//	   反过来会让没有凭证的一方用这条通道探测"某个 agent_id 现在有没有主机在线"；
//	c) 拒为**当前已有活动会话**的 agent_id 发证 —— 否则攻破一台机器后，可以用它的
//	   凭据去抢一台正在线的主机身份（虽然旧连接不会被顶替，但一旦它短暂重连，
//	   抢到的证书就取代了真主）；
//	d) TOFU 绑定：该 agent_id 之前绑过别的公钥时，除非本次出示了**它自己的新令牌**，
//	   否则拒绝 —— 这一条专门用来挡"共享密钥泄露后拿别人的 ID 换一张新证书"。
//
// 这里必须与上线鉴权用同一套闸门：签发出来的证书**会**让后续连接跳过 HMAC
// （身份取自证书），所以"服务端没配凭据"时若还肯发证，就等于绕过
// verifyOnlineRegistration 的默认拒绝 —— 拿到证书即获得任意 agent_id 身份。
//
// 拒绝换发时写审计（Category=auth）：这类事件的意义正好是"有人拿到了凭据但被绑定拦住"，
// 只进日志的话没人会看。
func enrollAgentCert(conn net.Conn, er protocol.EnrollReq, identity *agentauth.Identity,
	verifier *agentauth.Verifier, sess *session.Manager, router *route.Router, allowAnon bool) protocol.EnrollResp {
	resp := protocol.EnrollResp{}
	if !validAgentID(er.AgentID) {
		resp.Message = "agent_id 非法"
		return resp
	}
	// ── 凭据裁决（H-N1 的修复点）──────────────────────────────────────────
	//
	// 旧实现是两道**互相抵消**的判断：这里写 `if !verifier.Enabled() && !allowAnon {拒}`，
	// 而 Verifier.Verify 对"未启用"返回 ("", nil)（把"没配凭据"当成"校验通过"）。
	// 于是「服务端一个凭据都没配 + 开了 -allow-unauthenticated-agents」时，
	// 一张自选 agent_id 的 CSR + **完全空的凭据**就能换到本套 CA 签发的合法客户端证书
	// （已在本轮用 POC 实测复现，见 docs/security-review-2026-09-22.md）。
	//
	// 现在的口径只有一句：**匿名与否只看"这张报文有没有出示凭据"，放行与否只看运维开关。**
	//
	//   · 出示了凭据（proof 非空）→ 必须验过，与 allowAnon 无关。绝不降级成匿名，
	//     否则"令牌配错了"会被伪装成"正常接入了"，排障时查不出来。
	//   · 没出示凭据（proof 为空）→ 这是唯一的匿名入口，只有 allowAnon 为真才放行，
	//     且必须带 TS。此时**是否配置了凭据已不影响判定**：没配 = 运维配置缺失，
	//     没出示 = 匿名意图，两者不是一回事，不能相乘。
	credentialFree := er.Proof == ""
	credSource := agentauth.CredAnon
	// 命中的一次性令牌是否显式绑定到本 agent_id。未绑定令牌只够用于首次接入；
	// 已有绑定的主机换公钥必须出示绑定令牌（见 CheckOrBind）。
	credTokenBound := false

	if !credentialFree {
		// 出示了凭据 → 必须验过，与 allowAnon 无关
		src, bound, err := verifier.VerifyEx(er.AgentID, er.Nonce, er.Proof, er.TS)
		if err != nil {
			// 凭据校验失败同样要留 auth 审计：这类事件的信号价值与"被绑定拦住"同级 ——
			// 可能是令牌抄错，也可能是有人在试别人的令牌 / 重放旧报文。
			// 只进日志的话没人会看（而 rejectEnrollment 会把理由一并写进审计）。
			rejectEnrollment(router, conn, er.AgentID, err.Error())
			resp.Message = err.Error()
			return resp
		}
		credSource = src
		credTokenBound = bound
	} else {
		// 完全没带凭据：这是唯一的匿名入口，需**运维显式开启**才放行。
		// 注意此处不再看 verifier.Enabled()：无论服务端配没配凭据，没带凭据就是匿名，
		// 匿名只由开关决定 —— 旧版把这两件事混在一起判断，才出现"未配凭据 + 开开关
		// = 谁都能换证书"（H-N1）。
		if !allowAnon {
			resp.Message = "未携带注册凭据（需要一次性注册令牌 / 共享注册密钥），" +
				"或由运维开启 -allow-unauthenticated-agents 后重试"
			if !verifier.Enabled() {
				resp.Message = "服务端未配置注册凭据（一次性注册令牌或共享注册密钥），拒绝签发"
			}
			rejectEnrollment(router, conn, er.AgentID, resp.Message)
			return resp
		}
		// 开了开关也不接受"空壳"报文：nonce 与 TS 是防重放/防时序乱用的最低要求。
		// 它们与 agent_id 一起被记进审计，事后能判定"这次签发是匿名来的"。
		if er.TS <= 0 || er.Nonce == "" {
			resp.Message = "匿名接入仍需携带 nonce 与时间戳（防重放）"
			rejectEnrollment(router, conn, er.AgentID, resp.Message)
			return resp
		}
	}
	if credSource == agentauth.CredAnon {
		// 显式匿名接入必须留痕：事后排查看得出"这台主机的身份是没有凭据支撑的"。
		log.Printf("[server] 警告: 以**无凭据**方式为 Agent %q 签发证书（服务端开启了 "+
			"-allow-unauthenticated-agents，来自 %s）；该身份不可作为信任依据，"+
			"请为该主机签发一次性注册令牌后重新接入", er.AgentID, tlsInfo(conn))
	}
	if sess != nil && sess.Busy(er.AgentID) {
		resp.Message = "该 agent_id 当前有活动会话，拒绝换发证书；本次注册凭据已消耗，" +
			"请先停掉该主机的 Agent，再为该主机重新签发一枚一次性令牌后重试"
		rejectEnrollment(router, conn, er.AgentID, resp.Message)
		return resp
	}
	if identity == nil {
		resp.Message = "服务端未启用内置 CA，无法签发证书"
		return resp
	}
	certPEM, cerr := identity.AgentCertPEM(er.AgentID, er.CSR)
	if cerr != nil {
		resp.Message = "签发证书失败: " + cerr.Error()
		return resp
	}
	// TOFU 绑定只在存在令牌存储时执行：匿名路径通常没有存储（否则它就不是匿名路径了），
	// 此时"谁来绑定"没有承载者。这个空档是 -allow-unauthenticated-agents 的固有代价，
	// 已在启动日志里告警，并被上面的 CredAnon 留痕。
	if store := verifier.Tokens(); store != nil {
		if spki := agentauth.SpkiHashOfPEM(certPEM); spki != "" {
			if berr := store.CheckOrBind(er.AgentID, spki, credSource, credTokenBound); berr != nil {
				rejectEnrollment(router, conn, er.AgentID, berr.Error())
				resp.Message = berr.Error()
				return resp
			}
		}
	}
	resp.OK = true
	resp.Message = "证书已签发"
	resp.CertPEM = certPEM
	resp.CAPEM = string(identity.CACertPEM())
	resp.ServerFingerprint = identity.Fingerprint()
	resp.Identity = identity.Name()
	resp.CredSource = credSource
	log.Printf("[server] 已为 Agent %q 签发客户端证书（凭证类型 %s，来自 %s）", er.AgentID, credSource, tlsInfo(conn))
	if credSource == agentauth.CredSharedKey {
		// 双轨过渡期的提醒：还靠"一把密钥认全网"上来的主机，是 C-1 未收口的部分。
		log.Printf("[server] 注意: Agent %q 用**部署级共享注册密钥**换取了证书。"+
			"该密钥泄露一台即等于全网身份泄露；请为每台主机改用一次性注册令牌"+
			"（Web 控制台「Agent 接入」页签发，或 POST /api/enroll/tokens），"+
			"全部迁移完成后加 -disable-shared-agent-key 彻底关闭此路径", er.AgentID)
	}
	return resp
}

// verifyOnlineRegistration 校验"以自报身份上线"的连接（连接上没有可信客户端证书时）。
//
// 与证书换取（enrollAgentCert）用同一个 Verifier，因此两条路径认的是同一套凭据：
// per-agent 一次性注册令牌优先，过渡期共享密钥其次。
//
// 一条凭据路径都没配时（Enabled=false）不在此处放行：是否允许无凭证注册由
// -allow-unauthenticated-agents 显式决定，默认拒绝 —— 不能让"忘了配密钥"静默变成"谁都能注册"。
func verifyOnlineRegistration(verifier *agentauth.Verifier, info protocol.AgentInfo, allowAnon bool) error {
	if verifier != nil && verifier.Enabled() {
		_, err := verifier.Verify(info.AgentID, info.AuthNonce, info.AuthProof, info.AuthTime)
		return err
	}
	if allowAnon {
		return nil
	}
	return fmt.Errorf("未通过注册鉴权（服务端未配置注册密钥或一次性令牌，且未允许无凭证注册）")
}

// rejectEnrollment 记录一次被拒的证书换发（日志 + 审计）。
func rejectEnrollment(router *route.Router, conn net.Conn, agentID, reason string) {
	log.Printf("[server] 拒绝为 Agent %q 换发证书: %s（来自 %s）", agentID, reason, conn.RemoteAddr())
	if router == nil {
		return
	}
	router.Audit(&route.AuditLog{
		Time: time.Now().Unix(), Operator: "agent", OperatorIP: auth.LoginIPOf(conn.RemoteAddr().String()),
		AgentID: agentID, Command: "换取客户端证书被拒", Result: auditSafeText(reason), ExitCode: -1, Category: "auth",
	})
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
func handleAgentConn(conn net.Conn, sess *session.Manager, store *probe.Store, hub *ws.Hub, router *route.Router, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr, pkgDir string, verifier *agentauth.Verifier, allowAnon bool, identity *agentauth.Identity, allowNoCert bool, refresh *agentRefresh) {
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
			resp := enrollAgentCert(conn, er, identity, verifier, sess, router, allowAnon)
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
		// 应用层注册鉴权：HMAC 证明 + 时间窗 + 防重放。
		//
		// 例外：**连接已带上本套 CA 签发、且 CN 与自报 ID 一致的客户端证书**时不再要求证明。
		// 证书就是"这台机器曾经用一次性令牌完成过换取"的产物，比一次 HMAC 更强；
		// 而且一次性令牌用后即废 —— 若仍要求带证明，靠令牌接入的主机重启后就再也上不来，
		// 那会让"每台一份凭据"变成事实上的不可用（也正是 C-1 收口后应有的语义）。
		if sessConn.CertCN == "" {
			if err := verifyOnlineRegistration(verifier, info, allowAnon); err != nil {
				// 只写日志、不写审计库：这里的内容来自未认证方，写库会被灌垃圾
				log.Printf("[server] 拒绝 Agent %q 注册: %v（来自 %s）", info.AgentID, err, conn.RemoteAddr())
				return
			}
		}
		sessConn.SetAgentID(info.AgentID)
		// 设备身份（UUID）按 TOFU 裁决后再采信：自报值可能属于别台机器，
		// 照单全收等于任一条连接都能顶掉别人的在线会话、并入别人的设备记录。
		trustedUUID, uuidConflict := store.ResolveUUID(info.AgentID, info.UUID)
		if uuidConflict {
			// 只写日志、不写审计库：内容与来源都由未可信的一方给出。
			// 成因有两种：① 别台机器冒用了这个身份；② 本机换了硬件后 UUID 变了。
			// 两者都按"已绑定值"处理，运维可从日志里的两个值判断是哪一种。
			log.Printf("[server] Agent %s 上报的设备身份与已绑定值不符：自报 %q，按已绑定的 %q 处理"+
				"（若确为该机换了 UUID，请在主机列表删除该记录后让 Agent 重新上线）",
				info.AgentID, logSafe(info.UUID), logSafe(trustedUUID))
		}
		info.UUID = trustedUUID
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
				logSafe(info.UUID), oldID)
		}
		store.UpsertAgent(&info)
		log.Printf("[server] Agent %s 上线（来自 %s%s）", info.AgentID, conn.RemoteAddr(),
			uuidSuffix(info.UUID))
		refresh.now() // 上线是状态跃迁，不能等节流窗口
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
	// 安装包下发的并发闸门（见 TypePkgFetchReq 分支）。
	pkgSlots := make(chan struct{}, maxConcurrentPkgFetch)
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
				log.Printf("[server] 丢弃 %s 的探针报文（自报身份 %q 与连接身份不符）",
					sessConn.GetAgentID(), logSafe(p.AgentID))
				continue
			}
			store.UpdateProbe(&p)
			// 探针是高频数据源：整份主机列表的广播按窗口合并，
			// 否则一条连接把上报间隔改小就能放大成"每条报文 × 所有订阅者"的全量推送。
			refresh.soon()
			// 探针不回执（L-6）：Agent 的 handleMessage 里没有 ProbeDataResp 分支，
			// 回执发出即被丢弃，等于每台主机每 10 秒多一帧纯开销。
		case protocol.TypeCommandResp:
			var cr protocol.CommandResp
			if json.Unmarshal(env.Data, &cr) != nil {
				continue
			}
			// 归属校验：这条命令当初是不是下发给**本连接**的。
			// command_id 由浏览器生成（'push-'+Date.now()、'it-'+Date.now()），
			// 完全可以猜到；不校验时任一接入的 Agent 都能替别人的主机伪造
			// "执行成功"的审计记录，并把内容灌进不相关的操作员会话。
			origin, ok := router.CommandOrigin(cr.CommandID, sessConn.GetAgentID())
			if !ok {
				rejectUnownedCommand(router, sessConn, cr.CommandID, "命令回执")
				continue
			}
			// 输出只回投发起方会话：其他操作员看不到，也无法借它伪造界面
			hub.BroadcastToSession(origin.Session, protocol.TypeCommandResp, cr)
			// 回填命令审计结果（不新增记录，避免 operator 为空的重复条目）
			router.CompleteByCommand(cr.CommandID, sessConn.GetAgentID(), cr.Stdout, cr.ExitCode, cr.Duration)
		case protocol.TypeInteractiveOut:
			var io protocol.InteractiveOutput
			if json.Unmarshal(env.Data, &io) != nil {
				continue
			}
			origin, ok := router.CommandOrigin(io.CommandID, sessConn.GetAgentID())
			if !ok {
				rejectUnownedCommand(router, sessConn, io.CommandID, "终端输出")
				continue
			}
			hub.BroadcastToSession(origin.Session, protocol.TypeInteractiveOut, io)
			if io.Type == "exit" {
				// 交互终端不回 CommandResp，会话结束就在这里收口归属登记，
				// 否则这条映射要等到容量驱逐（2 万条上限）才清掉。
				router.ReleaseCommand(io.CommandID, sessConn.GetAgentID())
				// 交互输入的**逐行黑名单缓冲**必须一并回收（M-N2）。
				//
				// 它与 ReleaseCommand 是同一次收口的两半，之前只接了前一半，
				// DropInteractive 是死代码（全仓零调用点）：
				//   · 每个会话在 iguard.bufs 里留一个永不删除的键，持到进程退出；
				//   · 更糟的是触顶时的处置是**整表 wipe**（interactive.go:46），
				//     不是淘汰最旧 —— 一次 wipe 会连带清掉当时活跃终端的半行输入，
				//     表现为"打着字突然黑名单过滤失灵/半行错乱"；
				//   · 缓冲里含运维在终端里敲的**半行命令**（可能含口令），
				//     会一直驻留到下一次整表 wipe。
				router.DropInteractive(route.InteractiveKey(sessConn.GetAgentID(), io.CommandID))
			}
		case protocol.TypeFileUploadReq:
			var fu protocol.FileUploadReq
			_ = json.Unmarshal(env.Data, &fu)
			// 身份以连接为准：防止一条连接伪造他人身份把文件写到服务端
			if !connOwns(sessConn, fu.AgentID) {
				log.Printf("[server] 丢弃 %s 的上传分片（自报身份 %q 与连接身份不符）", sessConn.GetAgentID(), fu.AgentID)
				continue
			}
			// 分片接收：MD5 校验 + 断点续传 + 容量闸门
			_ = ft.HandleUploadChunk(&fu)
			// 不回写 FileAck（L-6）：Agent 在吐分片期间**不接收任何消息**
			// （sendFileChunks 由读循环内联调用，client.go:677），每片一条 ACK
			// 只会堆在 socket 里；跨网段链路上未读缓冲一旦顶满，服务端这一侧的
			// 写要等到 agentWriteWait(30s) 才返回，期间它连分片都没法继续收。
			// 2026-10-01 真机口径：同机环回上复现不出这个停顿（512 条回执被内核
			// 队列整段吸收，旧服务端 32MiB 回传 1s 完成、无超时），故此处按
			// "无用的反向流量"而非"已知故障"处理。缺片由服务端
			// "分片齐且总长相符才落盘"+ CleanupStale 兜底，不需要 Agent 实时确认。
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
			// L-2：ur.Message 是 Agent 自报字段，原样进日志能被用来伪造 "[server] ..." 行
			log.Printf("[server] Agent %s 升级结果: %v %s", sessConn.GetAgentID(), ur.OK, logSafe(ur.Message))
		case protocol.TypeTunnelOpenResp:
			var tr protocol.TunnelOpenResp
			if json.Unmarshal(env.Data, &tr) != nil {
				continue
			}
			// 隧道回传的归属一律以连接身份为准：隧道 ID 由服务端生成，但报文里的
			// tunnel_id 是 Agent 可写字段，不校验就等于任一台主机能替别人的隧道
			// 回"已就绪"/"已关闭"/数据。
			if isVNCTunnel(tr.TunnelID) {
				vncMgr.DeliverOpenResp(tr.TunnelID, sessConn.GetAgentID(), tr.OK, tr.Message)
				if !tr.OK {
					log.Printf("[server] VNC 隧道建立失败 %s: %s", logSafe(tr.TunnelID), logSafe(tr.Message))
				}
			} else if !tr.OK {
				// 端口映射隧道建立失败：关闭对应的公网侧连接
				log.Printf("[server] 隧道建立失败 %s: %s", logSafe(tr.TunnelID), logSafe(tr.Message))
				pm.NotifyTunnelClosed(tr.TunnelID, sessConn.GetAgentID(), tr.Message)
			}
		case protocol.TypeTunnelData:
			var td protocol.TunnelData
			if json.Unmarshal(env.Data, &td) == nil {
				if isVNCTunnel(td.TunnelID) {
					vncMgr.DeliverTunnelData(td.TunnelID, sessConn.GetAgentID(), td.Data)
				} else {
					pm.DeliverTunnelData(td.TunnelID, sessConn.GetAgentID(), td.Data)
				}
			}
		case protocol.TypeTunnelClose:
			var tc protocol.TunnelClose
			if json.Unmarshal(env.Data, &tc) == nil {
				if isVNCTunnel(tc.TunnelID) {
					vncMgr.NotifyTunnelClosed(tc.TunnelID, sessConn.GetAgentID())
				} else {
					// tc.Reason 是 Agent 自报的自由文本，portmap 侧已按"_ string"忽略它
					// （隧道关闭不需要原因），因此这里不构成日志注入面；L-2 的三处
					// 只剩升级回执与两条隧道建立失败的 Message，均已过 logSafe。
					pm.NotifyTunnelClosed(tc.TunnelID, sessConn.GetAgentID(), tc.Reason)
				}
			}
		case protocol.TypeVncInstallResp:
			var vr protocol.VncInstallResp
			if json.Unmarshal(env.Data, &vr) == nil {
				vncMgr.NotifyInstall(sessConn.GetAgentID(), &vr)
			}
		case protocol.TypeVncPasswordResp:
			var pr protocol.VncPasswordResp
			if json.Unmarshal(env.Data, &pr) == nil {
				vncMgr.NotifyPassword(sessConn.GetAgentID(), &pr)
			}
		case protocol.TypeServerAddrUpdateAck:
			var ack protocol.ServerAddrUpdateAck
			if json.Unmarshal(env.Data, &ack) == nil && connOwns(sessConn, ack.AgentID) {
				serverAddrAcks.deliver(ack)
			}
		case protocol.TypePkgFetchReq:
			var pr protocol.PackageFetchReq
			if json.Unmarshal(env.Data, &pr) == nil {
				// 独立 goroutine：大包分片下发不应阻塞该 Agent 的主消息循环。
				// 但并发必须有闸：每条请求起一路"持续往这条连接写分片"的协程，
				// 不设上限时一条连接就能用几十路并发把服务端出口占满。
				select {
				case pkgSlots <- struct{}{}:
					go func() {
						defer func() { <-pkgSlots }()
						servePackage(sessConn, pkgDir, &pr)
					}()
				default:
					log.Printf("[server] %s 的安装包下发并发已满（%d 路），本次请求忽略，等 Agent 重试",
						sessConn.GetAgentID(), cap(pkgSlots))
				}
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
	refresh.now() // 离线是状态跃迁，必须立刻反映到界面
	log.Printf("[server] Agent %s 离线", sessConn.GetAgentID())
}

// heartbeatSweeper 心跳清扫：超过 3 个心跳周期（默认 90s）视为离线。
func heartbeatSweeper(sess *session.Manager, store *probe.Store, refresh *agentRefresh) {
	t := time.NewTicker(10 * time.Second)
	for range t.C {
		offline := sess.Tick(90 * time.Second)
		for _, id := range offline {
			store.MarkOffline(id)
			log.Printf("[server] Agent %s 心跳超时离线", id)
		}
		if len(offline) > 0 {
			refresh.now()
		}
	}
}

// cleanupSweeper 端口映射超时连接清理 + 上传/分发任务闲置回收。
//
// 三者都是"有界资源 + 定时清扫"的同一类活儿，放一个协程里跑，
// 省得将来加新清扫器时又要记一遍"别忘开 goroutine"。
func cleanupSweeper(pm *portmap.Manager, hub *ws.Hub, ft *filetransfer.Manager, pushMgr *filetransfer.PushManager) {
	t := time.NewTicker(30 * time.Second)
	for range t.C {
		expired := pm.CleanupExpired(100)
		if len(expired) > 0 {
			log.Printf("[server] 清理 %d 条超时映射连接", len(expired))
			hub.Broadcast(protocol.TypePortMapState, pm.Tracks())
		}
		ft.CleanupStale()
		pushMgr.CleanupStale()
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

// serverInfoHandler 返回「Agent 接入」所需的信息：服务端地址、身份指纹、注册密钥指纹与
// 可直接粘贴的安装命令。目的是把"配证书"从手工散落多文件的流程，收敛成一次复制。
//
// 注意这里刻意不含任何密钥明文：本接口是控制台页面加载时顺手拉的，把部署级根凭证
// 放在这里等于让 XSS、浏览器扩展、反向代理日志各替攻击者保管一份（M-1）。
func serverInfoHandler(authz *auth.Auth, identity *agentauth.Identity, verifier *agentauth.Verifier, listenAddr func() string, dataDir string, requireCert, allowAnon, sharedKeyDisabled bool, agentTLS func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		listen := ""
		if listenAddr != nil {
			listen = listenAddr()
		}
		sharedKeyOn := verifier.SharedKeyEnabled()
		tlsOn := true
		if agentTLS != nil {
			tlsOn = agentTLS()
		}
		resp := map[string]interface{}{
			"listen":             listen,
			"require_agent_cert": requireCert,
			// shared_key_enabled：部署级共享注册密钥当前还能不能用来注册
			// （false = 只认一次性令牌，此时指纹与密钥路径一律不下发）。
			"shared_key_enabled": sharedKeyOn,
			// 三条"身份保障已被削弱"的状态（L-8）。只写启动日志的告警等于没人看，
			// 而这三项恰恰是运维会一路带着走完整个生命周期的配置。
			"allow_anon_agents":   allowAnon,
			"agent_tls_on":        tlsOn,
			"shared_key_disabled": sharedKeyDisabled,
		}
		// public_ip：本机探测到的公网 IP，供「Agent 接入」页面把部署命令里的总控地址
		// 补全 —— -listen 是 ":9000" / "0.0.0.0:9000" 这类写法时，命令里根本没有可用主机名。
		// 取不到就**不下发该字段**（前端据此退回占位符），而不是给一个空串让前端拼出坏地址。
		if ip := cachedPublicIP(); ip != "" {
			resp["public_ip"] = ip
		} else {
			// 启动时没探到（例如开机早于网络就绪）：后台补探，下次请求即可用。
			go refreshPublicIP()
		}
		if sharedKeyOn {
			// 明文改由 /api/enroll/shared-key/rotate 这个显式动作一次性给出，
			// 这里只留指纹：够运维比对"两台机器上配的是不是同一把"，不够冒用。
			resp["agent_key_fingerprint"] = verifier.KeyFingerprint()
			resp["agent_key_path"] = filepath.Join(dataDir, agentauth.KeyFileName)
		}
		// 一次性令牌的签发/吊销走 /api/enroll/tokens，不在此回显。
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
			if !decodeJSON(w, r, maxJSONBody, &body) {
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
		if !decodeJSON(w, r, maxJSONBody, &body) {
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

// deleteAgentHandler 删除一台离线主机的登记与历史数据。
//
// 只清服务端这一侧：主机记录、备注、探针/延迟历史、端口映射规则，以及指向它的
// 分享链接（主机都不在册了，继续可用的邀请没有意义）。
// 不碰证书 TOFU 绑定与未使用的一次性令牌 —— 那些是"这台机器还能不能换证书"
// 的信任状态，误清会把一道安全闸门一起删掉；需要时在「Agent 接入」页单独清除/删除。
// 也不卸载目标机上的 Agent：若它仍在运行，会重新注册并再次出现在列表里。
func deleteAgentHandler(store *probe.Store, remarks *remark.Store, pm *portmap.Manager, authz *auth.Auth, router *route.Router, shares *share.Store, hub *ws.Hub, vncMgr *vnc.Mgr) http.HandlerFunc {
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
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		body.AgentID = strings.TrimSpace(body.AgentID)
		if body.AgentID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
			return
		}
		info, aliases, err := store.DeleteOffline(body.AgentID)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		for _, id := range aliases {
			if err := remarks.Set(id, ""); err != nil {
				log.Printf("[server] 删除主机 %q 的备注失败: %v", id, err)
			}
		}
		removed := 0
		if pm != nil {
			removed = pm.DeleteRulesForAgents(aliases)
		}
		revokedShares := revokeSharesForAgents(shares, hub, vncMgr, aliases)
		router.Audit(&route.AuditLog{
			Time:       time.Now().Unix(),
			Operator:   authz.Operator(r.Header.Get("Authorization")),
			OperatorIP: auth.LoginIPOf(r.RemoteAddr),
			AgentID:    info.AgentID,
			Command:    "删除离线主机 " + info.AgentID,
			Result:     "ok",
			Category:   "op",
		})
		writeJSON(w, map[string]interface{}{
			"ok":              true,
			"agent_id":        info.AgentID,
			"portmap_removed": removed,
			"shares_revoked":  revokedShares,
			"msg":             "主机已删除",
		})
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
		if !decodeJSON(w, r, maxJSONBody, &req) {
			return
		}
		ok, msg := router.Dispatch(&req, authz.Operator(r.Header.Get("Authorization")), r.RemoteAddr, dispatchSession(r))
		writeJSON(w, map[string]interface{}{"ok": ok, "msg": msg})
	}
}

// dispatchSession 取本次请求所属操作员的会话键，命令输出据此定向回投。
//
// REST 与 /ws 用同一个 Token（前端都存在 localStorage 里），所以这里派生出的键
// 与该浏览器 WebSocket 连接的键相同 —— 通过 REST 下发的命令，输出照样能回到它自己
// 的那条 WS 上，而不会跑到别人的控制台里。
func dispatchSession(r *http.Request) string {
	return ws.SessionKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
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
		if !decodeJSON(w, r, maxJSONBody, &body) {
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
		results := router.DispatchBatch(req, body.AgentIDs, authz.Operator(r.Header.Get("Authorization")), r.RemoteAddr, dispatchSession(r))
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
		if !decodeJSON(w, r, maxJSONBody, &req) {
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
//
// 这是唯一"就地带回口令"的响应：口令由这条指令在目标机上生成，谁下的指令谁拿一次。
// 之后的取用走 /api/vnc/password（同样留痕），不再挂在可轮询的状态接口上。
func vncInstallHandler(mgr *vnc.Mgr, authz *auth.Auth, router *route.Router) http.HandlerFunc {
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
		if !decodeJSON(w, r, maxJSONBody, &body) {
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
		// 在目标机上装/起远程桌面服务并生成本机口令，属改动主机状态的运维操作，必须留痕
		if router != nil {
			exit := 0
			if !resp.OK {
				exit = 1
			}
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: authz.Operator(r.Header.Get("Authorization")), OperatorIP: auth.LoginIPOf(r.RemoteAddr),
				AgentID: body.AgentID,
				Command: fmt.Sprintf("VNC 安装/启动 端口:%d 模式:%s", resp.Port, mode),
				Result:  auditSafeText(resp.Message), ExitCode: exit, Category: "op",
			})
		}
		writeJSON(w, map[string]interface{}{
			"ok": resp.OK, "msg": resp.Message, "port": resp.Port,
			"password": resp.Password, "mode": resp.Mode,
		})
	}
}

// vncStatusHandler 返回该主机最近一次 VNC 安装/启动结果（不含口令）。
//
// 刻意不带口令：状态接口是前端等待安装完成时轮询用的（最长几分钟、每 3s 一次），
// 口令进它的响应体就等于长期驻留在响应、浏览器内存与可能被记录的一层里。
// 只回 has_password 让前端知道"该去取"，取的动作走 /api/vnc/password 并留痕。
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
		writeJSON(w, map[string]interface{}{
			"ok": resp.OK, "msg": resp.Message, "port": resp.Port,
			"has_password": resp.Password != "", "mode": resp.Mode,
		})
	}
}

// vncPasswordHandler 按需交付某主机**当前生效**的 VNC 口令，并留痕。
//
// 实现是实时向 Agent 读取（Agent 从 ultravnc.ini / x11vnc 口令文件还原），
// 不是回放服务端缓存的上次安装结果 —— 服务端重启、换人操作、或 VNC 是手工装的，
// 缓存都答不上来。读取是只读操作，不会重启服务、不会打断正在使用的远程会话。
//
// 为什么要单开一个 POST 端点并记审计：口令本身是"能打开这台机器桌面"的凭据，
// 谁在什么时候取过是事后追查远程桌面事件的第一手线索。
// readPassword 由调用方注入（生产环境传 vncMgr.ReadPassword）。
// 注入而不是直接依赖 *vnc.Mgr，是为了让 HTTP 层能单独测"交付与留痕"这条契约 ——
// 否则要造一个假的 Agent 连接才能覆盖到。
func vncPasswordHandler(readPassword func(string) *protocol.VncPasswordResp, authz *auth.Auth, router *route.Router) http.HandlerFunc {
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
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		// 实时向 Agent 读取机器上**当前生效**的口令，而不是回放本进程缓存的上次安装结果：
		// 服务端重启、换人操作、或 VNC 是手工装的，缓存都答不上来，而重装又会打断在用会话。
		resp := readPassword(body.AgentID)
		pw, msg := "", ""
		if resp != nil {
			pw, msg = resp.Password, resp.Message
		}
		// 审计只记"取到了没有"，绝不把口令本身写进审计（那是又存了一份明文凭据）。
		result := "无可用口令"
		if pw != "" {
			result = "ok"
		}
		if router != nil {
			router.Audit(&route.AuditLog{
				Time: time.Now().Unix(), Operator: authz.Operator(r.Header.Get("Authorization")), OperatorIP: auth.LoginIPOf(r.RemoteAddr),
				AgentID: body.AgentID, Command: "获取 VNC 口令", Result: result, Category: "op",
			})
		}
		if pw == "" {
			if msg == "" {
				msg = "该主机没有可交付的 VNC 口令，请先执行一键安装/启动"
			}
			writeJSON(w, map[string]interface{}{"ok": false, "msg": msg})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "password": pw, "mode": resp.Mode})
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
			csvCell(l.Operator), csvCell(l.AgentID), csvCell(l.CommandID),
			csvCell(l.Command), csvCell(l.Result),
			strconv.Itoa(l.ExitCode),
			strconv.FormatBool(l.Blocked), csvCell(l.BlockReason),
			strconv.FormatInt(l.Duration, 10),
		})
	}
	cw.Flush()
}

// csvCell 中和 CSV 的**公式注入**（L-1）。
//
// 为什么必须做：csv.Writer 只负责"正确地引用"，它不管打开方（Excel/WPS/LibreOffice）
// 会不会把单元格内容当公式执行。以 = + - @ 开头的值，或被前导制表符/回车"顶"到
// 首位的值，会被表格软件解释成公式，DDE / 外链 / HYPERLINK 都能从那里打到本机。
//
// 危险面为什么真实存在：Command 与 Result 的值来自 **Agent 回传** —— 也就是说
// "某台被拿下的被管主机"能直接决定运维导出的 CSV 里写什么。单人使用也一样中招：
// 他导出的正是自己机器上那些被管主机的回传结果。
//
// 做法：首字符命中危险集合时前置一个单引号。这是表格软件的"按文本处理"约定，
// 视觉上只多一个前导 '，而内容本身一位不改 —— 比剥离危险字符更安全，因为
// 导出件仍能完整还原被管主机到底回了什么。
func csvCell(s string) string {
	if s == "" {
		return s
	}
	// 前导空白（含制表/回车/换行）会被表格软件忽略后再看首字符，所以先跳过它们
	// 找到"真正会被当成公式首位"的那个字符。
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
			i++
			continue
		}
		break
	}
	if i >= len(s) {
		// 全是空白，不可能构成公式；原样返回，交给 csv.Writer 引用
		return s
	}
	switch s[i] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	// 前导空白之后是危险字符时也要拦：Excel 会先吃掉空白再看首字符
	if i > 0 {
		switch s[0] {
		case '\t', '\r', '\n':
			return "'" + s
		}
	}
	return s
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
		if !decodeJSON(w, r, maxJSONBody, &op) {
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

func upgradeHandler(sess *session.Manager, authz *auth.Auth, upgradePubKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AgentID        string `json:"agent_id"`
			Version        string `json:"version"`
			URL            string `json:"url"`
			SHA256         string `json:"sha256"`
			Signature      string `json:"signature"`
			MD5            string `json:"md5"` // 已废弃，仅为兼容旧前端
			Force          bool   `json:"force"`
			AllowDowngrade bool   `json:"allow_downgrade"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
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
		// 这个字段的含义仅是"本次要升到哪个版本"，由 Agent 侧判断该不该装：
		// 与自身版本相等时跳过（除非 force），低于自身版本时按降级拒绝（除非 allow_downgrade）。
		// 因此它可以是任何合法版本号，与当前服务端版本没有关系 —— 但它会进签名消息，必须是可解析的形态。
		//
		// 留空不再自动补服务端版本：签名消息里含目标版本号，猜一个版本只会让 Agent
		// 报"签名校验失败"，比在这里直接要求填写更难排查。
		body.Version = strings.TrimSpace(body.Version)
		if body.Version == "" {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "必须提供目标版本号（它参与升级包签名，不能用默认值代替）"})
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
		// 目标版本号会进签名消息，因此必须是 Agent 侧可解析的形态；
		// 在这里拦住，比让每台目标机各自回一句"无法比较"更好排查。
		if !isVersionLike(body.Version) {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "目标版本号需形如 1.2.3（它参与升级包签名校验）"})
			return
		}
		// 签名：Agent 侧强制要求，且**没有跳过校验的开关**，所以缺签名必然是白下发一次。
		sig := strings.TrimSpace(body.Signature)
		if sig == "" {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "必须提供升级包签名（对\"目标版本号 + 包内容\"的 Ed25519 签名，用 scripts/signpkg 离线生成）"})
			return
		}
		if !isHexLen(sig, ed25519.SignatureSize*2) {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": fmt.Sprintf("签名应为 %d 位十六进制（Ed25519 签名）", ed25519.SignatureSize*2)})
			return
		}
		if strings.TrimSpace(upgradePubKey) == "" {
			// 不阻断：公钥可以只配在 Agent 侧（本服务端只是转发方）。
			// 但一键部署生成的 Agent 配置里会是空值，那条链就升级不动了，得留个提示。
			log.Printf("[upgrade] 提示: 服务端未配置 -upgrade-pubkey，一键部署的 Agent 将因无公钥而拒绝所有升级指令")
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
			UpgradeID:      fmt.Sprintf("up-%d", time.Now().UnixNano()),
			Version:        body.Version,
			URL:            body.URL,
			SHA256:         strings.ToLower(strings.TrimSpace(body.SHA256)),
			Signature:      sig,
			MD5:            body.MD5,
			Force:          body.Force,
			AllowDowngrade: body.AllowDowngrade,
		}
		if err := conn.Write(protocol.TypeUpgradeReq, req); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "upgrade_id": req.UpgradeID})
	}
}

// isHex64 判断是否为 64 位十六进制（SHA-256 摘要）。
func isHex64(s string) bool { return isHexLen(s, 64) }

// isHexLen 判断是否为定长小写十六进制串。
func isHexLen(s string, n int) bool {
	if len(s) != n {
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

// isVersionLike 判断版本号是否是"点分数字（可带 v 前缀与 -/_/+ 构建标记）"。
//
// Agent 侧要拿它做升降级判定并纳入签名消息，所以格式先在这里钉死：
// 传个 "latest" 下去，代价是每台目标机各自失败一次。
func isVersionLike(s string) bool {
	t := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if t == "" {
		return false
	}
	if i := strings.IndexAny(t, "-+_"); i >= 0 {
		t = t[:i]
	}
	digits := false
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c >= '0' && c <= '9':
			digits = true
		case c == '.':
		default:
			return false
		}
	}
	return digits
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
		if !decodeJSON(w, r, maxCredBody, &body) {
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
		tok, terr := randHex(32)
		if terr != nil {
			// 宁可登录失败，也不发一个可预测的会话 Token
			log.Printf("[auth] 生成会话 Token 失败（系统熵源不可用）: %v", terr)
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "服务端无法生成安全会话，请稍后重试"})
			return
		}
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
		if !decodeJSON(w, r, maxCredBody, &body) {
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

// randHex 生成 n 字节随机值的 hex 串。
//
// 熵源失败必须报错而不是返回一串全零：调用方拿它当**会话 Token**，
// 全零等于把"可预测的管理员凭据"发出去，而失败本身在日志里一个字都不会留。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
		// 重新生成（OTP 已启用）必须验证当前密码：否则一枚被窃取的会话
		// Token 就能把二次验证悄悄换成攻击者自己的验证器。
		// 首次绑定不需要密码 —— 此时还没有可被"换掉"的现有 factor。
		if enabled {
			var body struct {
				Password string `json:"password"`
			}
			if r.Method == http.MethodPost {
				if !decodeJSON(w, r, maxCredBody, &body) {
					return
				}
			}
			if strings.TrimSpace(body.Password) == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "重新生成动态验证码需要验证当前密码"})
				return
			}
			if err := um.VerifyPassword(username, body.Password); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "当前密码错误"})
				return
			}
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
			Secret   string `json:"secret"`
			Code     string `json:"code"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, maxCredBody, &body) {
			return
		}
		if body.Secret == "" || body.Code == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "密钥与验证码均不能为空"})
			return
		}
		// 已启用 OTP 时再次绑定等于"换掉现有验证器"，必须验证当前密码。
		// 只在 setup 环节验密码是不够的：enable 是公开的 API 入口，
		// 绕过前端直接带上一个自选密钥就能完成替换。
		if enabled, _ := um.TOTPStatus(username); enabled {
			if strings.TrimSpace(body.Password) == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "重新绑定动态验证码需要验证当前密码"})
				return
			}
			if err := um.VerifyPassword(username, body.Password); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "当前密码错误"})
				return
			}
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
		if !decodeJSON(w, r, maxCredBody, &body) {
			return
		}
		if strings.TrimSpace(body.Password) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "关闭动态验证码需要验证当前密码"})
			return
		}
		if enabled, _ := um.TOTPStatus(username); enabled && strings.TrimSpace(body.OTP) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "请输入当前动态验证码"})
			return
		}
		if err := um.UnbindTOTP(username, body.Password, body.OTP); err != nil {
			msg := err.Error()
			if errors.Is(err, auth.ErrBadLogin) {
				msg = "当前密码错误"
			}
			writeJSON(w, map[string]interface{}{"ok": false, "msg": msg})
			return
		}
		log.Printf("[auth] 用户 %s 已关闭 OTP 动态验证码", username)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "OTP 动态验证码已关闭"})
	}
}

// guard 统一鉴权中间件。
func guard(w http.ResponseWriter, r *http.Request, authz *auth.Auth) bool {
	if !guardGate(w, r, authz) {
		return false
	}
	token := r.Header.Get("Authorization")
	if !authz.ValidateToken(strings.TrimPrefix(token, "Bearer ")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// guardWS 是 WebSocket 握手的闸门：与 guard 走同一套 IP 白名单与每 IP 限流，
// 只是凭证从子协议头取（浏览器的 WS 握手带不了 Authorization 头）。
//
// 这两条路由曾直接调 HandleUpgrade / HandleViewer，只做了 ValidateToken，
// 于是运维用 allow_ips 把控制台关进内网之后，最强的两个入口（操作员推送通道、
// 打进内网的 VNC 代理）其实一直对全网敞开，而且完全不吃限流 —— 可以无限速试 Token。
func guardWS(w http.ResponseWriter, r *http.Request, authz *auth.Auth) bool {
	if !guardGate(w, r, authz) {
		return false
	}
	if !authz.ValidateToken(ws.WSToken(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// guardGate 是与传输方式无关的前置闸门（IP 白名单 + 每 IP 速率），
// 两条 WS 路由与全部 /api/* 共用，避免各修一半。
func guardGate(w http.ResponseWriter, r *http.Request, authz *auth.Auth) bool {
	if !authz.IsAllowedIP(r.RemoteAddr) {
		http.Error(w, "ip not allowed", http.StatusForbidden)
		return false
	}
	if !authz.AllowRequest(r.RemoteAddr) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	return true
}

// securityHeaders 给所有响应加基础防护头（含 CSP，见 cspHeader 的取值依据）。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", cspHeader)
		// /api 一律不可缓存：登录响应带 Token、审计响应带命令结果、备份响应带文件，
		// 而它们都是"每个会话各自不同"的动态内容 —— 让中间代理存一份就等于多一处泄露点。
		// 静态资源不在 /api 下，压缩产物仍可被正常缓存。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		// Web 控制台默认就是 HTTPS（见 start() 的 Web TLS 策略），所以可以安全地启用 HSTS：
		// 它会阻止后续降级到明文，保护 Bearer Token 与远程终端/远程桌面的会话。
		// 注意：只在 TLS 请求上加，避免反代外的明文态被浏览器记住。
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// cspHeader 是整站（后台 + 公开页）共用的 Content-Security-Policy。
//
// 取值依据不是"照抄网上的模板"，而是实测本仓产物：
//   - script-src 'self'：`dist/assets/*.js` 全部是外链，产物里 **没有** eval /
//     new Function / new Worker（已 grep 三平台构建产物确认），所以既不需要
//     'unsafe-inline' 也不需要 'unsafe-eval'。这一条是本策略真正的价值所在 ——
//     页脚那类 HTML 注入即使绕过净化，也拿不到执行 JavaScript 的能力。
//   - style-src 'unsafe-inline'：index.html 的首屏底色是刻意的内联 <style>
//     （样式表被代理拦掉时不会白屏，见该文件的注释），改哈希会让每次构建都变哈希。
//     内联样式的危害是界面遮挡/数据外渗，远小于内联脚本，这里接受。
//   - img-src 'self' data:：国旗精灵是运行时画出来的 —— `flag.js:137` 的
//     `canvas.toDataURL('image/png')` 直接当 img 源用，不给 data: 就是满屏方框。
//   - connect-src 'self' ws: wss:：'self' 对 ws:// 同源是否算匹配，浏览器实现口径不一，
//     写死 ws:/wss: 才不会把终端和远程桌面拍死。代价是"若发生 XSS 可用 WS 外带"，
//     而 script-src 已经不允许注入脚本，这个代价只在依赖被投毒时才成立。
//     HTTP 侧仍严格 'self'（前端从不跨域取数，已 grep src/ 确认）。
//   - frame-ancestors 'none' + form-action 'self' + base-uri 'none'：公开页不做嵌入，
//     也没有跨页表单；`base` 被改写会连带把 'self' 变成攻击者的源，所以必须钉住。
const cspHeader = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self' ws: wss:; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'; " +
	"base-uri 'none'; " +
	"object-src 'none'"

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

// 请求体上限。此前所有 JSON 入口都按不定长读体，匿名者可用一个小请求换走大内存。
// 凭据类单独收紧：登录用户名会被 LoginGuard 作为 map 键驻留 loginFailTTL（30 分钟）。
const (
	maxJSONBody = 256 << 10
	maxCredBody = 32 << 10
)

// decodeJSON 在 max 字节上限内解析请求体；失败时已写好响应，返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, max int64, v interface{}) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
	} else {
		http.Error(w, "bad request", http.StatusBadRequest)
	}
	return false
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
	// 预压缩产物优先：省去每次请求的压缩开销，也避免弱网下反复传输大包。
	//
	// 带 Range 的请求必须走原文件（L-3）：http.ServeFile 是拿 **.br/.gz 的字节数**
	// 去解释客户端那份"未压缩字节"的偏移量的，Range 命中时会回一个长度与内容都
	// 对不上的 206 —— 客户端解完压只拿到错位的一截。分段请求不常见，但一旦发生
	// 就是静默的数据错误，所以直接放弃这条路径的压缩收益。
	//
	// Vary 无条件声明：这条响应的内容确实随 Accept-Encoding 变化，只在命中 sidecar
	// 时才写会让中间缓存把"未压缩的那份"当成唯一表示存下来。
	w.Header().Set("Vary", "Accept-Encoding")
	if enc := pickEncoding(r.Header.Get("Accept-Encoding")); enc.name != "" && r.Header.Get("Range") == "" {
		if cand := path + "." + enc.ext; fileExists(cand) {
			w.Header().Set("Content-Encoding", enc.name)
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

// pickEncoding 解析 Accept-Encoding，支持 br / gzip（同为可接受时 br 优先，体积更小）。
//
// 必须按 q 值判定（L-3）：原先用 strings.Contains 匹配子串，于是
//   - `br;q=0` —— 客户端**明确拒绝** brotli —— 照样判成命中，回一份 .br；
//   - `gzip;q=1.0, br;q=0.2` —— 客户端更想要 gzip —— 仍然按"谁先出现"回 br。
//
// 两种情况都只是"给了一份客户端不想要的编码"，浏览器会照单全收，
// 症状是内容能显示但某些中间层/老设备解压失败，很难查到这里。
//
// 不识别的编码一律按"不可接受"处理：客户端列了 br/gzip 之外的清单而没有提到
// 这两个，就说明它不支持，不能靠 Contains 的巧合把它算成支持。
func pickEncoding(header string) encoding {
	brQ, gzQ := 0.0, 0.0
	for _, tok := range strings.Split(strings.ToLower(header), ",") {
		name, q := splitEncodingToken(tok)
		switch name {
		case "br":
			brQ = q
		case "gzip":
			gzQ = q
		}
	}
	// 权重高者胜；相等（含两者都未标 q，即都为 1）时按服务端偏好回 br。
	if gzQ > brQ {
		return encoding{name: "gzip", ext: "gz"}
	}
	if brQ > 0 {
		return encoding{name: "br", ext: "br"}
	}
	return encoding{}
}

// splitEncodingToken 拆出一个编码名与它的 q 值（缺省 1）。
// q 值畸形时按 0 处理：宁可回退未压缩，也不要把解析不了的参数当成"客户端同意了"。
func splitEncodingToken(tok string) (string, float64) {
	name := strings.TrimSpace(tok)
	q := 1.0
	if i := strings.IndexByte(name, ';'); i >= 0 {
		param := strings.TrimSpace(name[i+1:])
		name = strings.TrimSpace(name[:i])
		if strings.HasPrefix(param, "q=") {
			v, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64)
			if err != nil || v < 0 {
				q = 0
			} else if v > 1 {
				q = 1
			} else {
				q = v
			}
		}
	}
	return name, q
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
// sessionKey 为该连接的会话键，命令要按它登记归属，回执才只回投给发起方。
//
// 分享访客的连接也走这里（operator = "share:<id>"），但只允许终端输入：
// 命令下发、端口映射、文件分发全部拒绝 —— 这些能力如果有需要，必须走
// /api/share/* 的专用端点，那里才有"功能作用域 + 主机绑定"的校验。
func handleWebMessage(sessionKey, operator string, data []byte, sess *session.Manager, store *probe.Store, router *route.Router, hub *ws.Hub, ft *filetransfer.Manager, pm *portmap.Manager, pushMgr *filetransfer.PushManager, vncMgr *vnc.Mgr, shareStore *share.Store) {
	// WebSocket 消息中 data 为 JSON 对象，需用 RawMessage 解码（不同于 TCP 的 base64 []byte）
	var raw struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("[server] web message decode error: %v", err)
		return
	}
	if shareID, isShare := shareIDFromOperator(operator); isShare {
		// 分享可能在连接建立后过期/被撤销：每条上行消息都复查一次，
		// 失效就把这条连接断开，避免"链接过期了终端还能继续输入"。
		if shareStore == nil || !shareStore.Active(shareID) {
			hub.CloseSession(sessionKey)
			closeShareCommands(router, sess, sessionKey)
			return
		}
		if raw.Type != protocol.TypeInteractiveIn {
			log.Printf("[server] 丢弃分享 %s 的上行消息 %q（只允许终端输入）", shareID, raw.Type)
			return
		}
	}
	// 统一转成 Envelope 供 switch 复用
	env := protocol.Envelope{Type: raw.Type, Data: raw.Data}
	switch env.Type {
	case protocol.TypeCommandReq:
		var req protocol.CommandReq
		if err := json.Unmarshal(env.Data, &req); err != nil {
			return
		}
		ok, msg := router.Dispatch(&req, operator, "ws", sessionKey)
		if !ok {
			// 失败原因只回给发起方：别人既不该看到也不该被这条消息误导
			hub.BroadcastToSession(sessionKey, protocol.TypeCommandResp, protocol.CommandResp{
				CommandID: req.CommandID, Status: 1, Stderr: msg,
			})
		}
	case protocol.TypeInteractiveIn:
		var inp protocol.InteractiveInputReq
		if json.Unmarshal(env.Data, &inp) != nil {
			return
		}
		// 这条终端会话必须是由**本会话**发起的：command_id 由浏览器生成且可猜，
		// 不校验的话任一登录者都能往别人正开着的远程终端里打字，
		// 等于借用他人已授权的 shell（比读别人输出更严重）。
		if origin, ok := router.CommandOrigin(inp.CommandID, inp.AgentID); !ok || origin.Session != sessionKey {
			log.Printf("[server] 丢弃操作员 %s 对 %s 的交互输入：会话 %q 不属于该连接",
				operator, inp.AgentID, inp.CommandID)
			return
		}
		// 交互输入同样过黑名单（逐行检查）：远程终端此前是黑名单的最大旁路
		safe, blocked := router.FilterInteractive(route.InteractiveKey(inp.AgentID, inp.CommandID), inp.InputData)
		if blocked != "" {
			hub.BroadcastToSession(sessionKey, protocol.TypeInteractiveOut, protocol.InteractiveOutput{
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
	task, complete, err := pushMgr.UploadChunk(&req)
	if err != nil {
		// 超限拒绝必须让界面看见原因：静默停在半截只会让人以为"还在传"。
		hub.Broadcast(protocol.TypeFileTransferState, &protocol.FileTransferState{
			AgentID: req.AgentID, FilePath: req.FilePath, Action: "push",
			Status: "failed", Message: "分发已中止：" + err.Error(),
		})
		return
	}
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

// memStatsLogger 周期记录 Go 堆占用，供事后定位内存增长。
//
// 只在堆超过阈值时留痕：空载通常只有十几 MB，固定间隔打印只会刷屏。
// 取 HeapAlloc（存活对象）而不是 Sys（含保留地址空间，Windows 上参考价值低）。
func memStatsLogger() {
	const threshold = 64 << 20
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc < threshold {
			continue
		}
		log.Printf("[mem] heap=%dMiB heap_sys=%dMiB sys=%dMiB goroutines=%d",
			m.HeapAlloc>>20, m.HeapSys>>20, m.Sys>>20, runtime.NumGoroutine())
	}
}

// probeHistoryCleaner 维护探针 / 延迟历史的分层留存。
//
// 10 秒采样直接保留 30 天会把库撑到百万行量级（8 台主机约 7 万行/天），
// 所以启动后先维护一次存量，之后每 6 小时滚动执行：
//   - probe_history 只保留最近 7 天原始采样，更早数据导出为 gzip CSV 归档后删除；
//   - latency_history 仍为 24 小时原始 + 24 小时~30 天 5 分钟聚合。
func probeHistoryCleaner(db *storage.DB) {
	// 让监听与首屏先就绪，压缩在后台进行；单连接下它会短暂占用数据库，
	// 但不影响静态页面与内存态接口。
	time.Sleep(5 * time.Second)
	compactHistoryOnce(db, true)

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		compactHistoryOnce(db, false)
	}
}

func compactHistoryOnce(db *storage.DB, first bool) {
	start := time.Now()
	if first {
		log.Printf("[server] 开始维护历史数据（探针保留 7 天原始采样；更早数据先归档再删除）")
	}
	st, err := db.CompactHistory()
	if err != nil {
		log.Printf("[server] 历史维护失败（下次重试）: %v", err)
		return
	}
	if st.ArchivedProbe > 0 {
		log.Printf("[server] 已归档 %d 条探针历史到 %s（%s ~ %s）",
			st.ArchivedProbe, st.ArchivePath,
			time.Unix(st.ArchiveStart, 0).Format("2006-01-02 15:04:05"),
			time.Unix(st.ArchiveEnd, 0).Format("2006-01-02 15:04:05"))
	}
	if st.RemovedProbe > 0 || st.RemovedLatency > 0 || first {
		log.Printf("[server] 历史维护完成：探针 %d→%d，延迟 %d→%d（过期 %d），耗时 %s",
			st.ProbeBefore, st.ProbeAfter,
			st.LatencyBefore, st.LatencyAfter, st.PrunedLatency,
			time.Since(start).Round(time.Millisecond))
	}
	// 存量库第一次压缩通常删除量很大，做一次 VACUUM 才能真正缩小文件；
	// 平峰期的小批量清理交给 auto_vacuum 的增量回收。
	removed := st.RemovedProbe + st.RemovedLatency
	if (first && (st.ProbeBefore > 100000 || st.LatencyBefore > 100000)) || removed > 50000 {
		vstart := time.Now()
		if err := db.CompactStorage(); err != nil {
			log.Printf("[server] 数据库空间回收失败: %v", err)
		} else {
			log.Printf("[server] 数据库空间已回收（VACUUM，耗时 %s）", time.Since(vstart).Round(time.Millisecond))
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
