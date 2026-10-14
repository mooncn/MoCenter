// MoCenter Agent 客户端入口。
//
// Agent 部署在各内网主机上，主动连接公网总控服务端维持 TCP 长连接，
// 上报探针数据、接收并执行远程命令。
//
// 用法：
//
//	首次：./agent -server host:port -agentid <id> [-hb 30] [-probe 10]
//	      （首次运行会自动把参数写入配置文件 mocenter-agent.json）
//	后续：./agent（自动读取配置文件；命令行显式参数优先级更高）
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mocenter/agent/internal/client"
	"mocenter/agent/internal/config"
	"mocenter/agent/internal/daemon"
	probepkg "mocenter/agent/internal/probe"
	"mocenter/agent/internal/version"
	"mocenter/agent/internal/winapp"
)

func main() {
	server := flag.String("server", os.Getenv("MOCENTER_SERVER"), "服务端地址 host:port")
	agentID := flag.String("agentid", os.Getenv("MOCENTER_AGENT_ID"), "Agent 名称标识（可变，仅用于显示与路由）")
	agentUUID := flag.String("uuid", os.Getenv("MOCENTER_AGENT_UUID"), "设备硬件 UUID（设备身份，留空则自动探测并写入配置）")
	hb := flag.Int("hb", 30, "心跳间隔(秒)")
	probe := flag.Int("probe", 10, "探针采集周期(秒)")
	daemonize := flag.Bool("daemon", false, "以守护进程方式运行（后台运行并崩溃自启）")
	tlsEnabled := flag.Bool("tls", false, "启用 TLS 加密连接")
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书校验（自签名测试）")
	clientCert := flag.String("client-cert", "", "mTLS 客户端证书文件路径（服务端启用双向认证时必填）")
	clientKey := flag.String("client-key", "", "mTLS 客户端私钥文件路径")
	caCert := flag.String("ca-cert", "", "用于校验服务端证书的 CA 证书路径（不指定则用 -insecure 跳过校验）")
	configPath := flag.String("config", config.DefaultPath(), "配置文件路径（首次启动自动生成）")
	proxyIP := flag.String("proxy-ip", os.Getenv("MOCENTER_PROXY_IP"), "国内加速代理前缀（用于公网 IP 探测等境外服务；安装包下载只接受 https:// 前缀）")
	allowPublicPkg := flag.Bool("allow-public-package-download", false, "允许从公网取 VNC/驱动安装包（winget/choco/厂商直连）。默认关闭：只从服务端托管目录经长连接下发")
	agentKey := flag.String("agent-key", os.Getenv("MOCENTER_AGENT_KEY"), "与服务端预共享的注册密钥（服务端 agent.key 的内容）")
	enrollSecret := flag.String("enroll-secret", os.Getenv("MOCENTER_ENROLL_SECRET"), "首次换取客户端证书用的注册密钥（换取成功后自动清空）")
	serverFingerprint := flag.String("server-fingerprint", os.Getenv("MOCENTER_SERVER_FINGERPRINT"), "服务端身份公钥指纹（免 CA 文件校验服务端，服务端换地址/证书都不必更新）")
	credsDir := flag.String("creds-dir", "", "客户端证书落盘目录（默认 <配置目录>/creds）")
	upgradePubKey := flag.String("upgrade-pubkey", os.Getenv("MOCENTER_UPGRADE_PUBKEY"), "升级包 Ed25519 公钥（hex 或文件路径）；未配置则拒绝一切远程升级")
	fileRoots := flag.String("file-roots", os.Getenv("MOCENTER_FILE_ROOTS"), "允许服务端下发读写的目录（逗号分隔），默认仅 <配置目录>/files")
	latencyTargets := flag.String("latency-targets", "", "延迟探测目标，逗号分隔（如 icmp:1.1.1.1,tcp:1.1.1.1:443,http:https://example.com ）")
	showVersion := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	// 空串参数归一（原因见 flags.go）：mocenter-agent.service 用 "${MOCENTER_*}" 引号插值
	// 传参，EnvironmentFile 里缺键时传进来的是空 argv，会把配置文件里的地址/ID 回写成空。
	blankFlags := normalizeBlankFlags(flag.CommandLine)

	// -version：打印后立即退出，不读配置文件、不探测硬件、不连服务端。
	// 放在最前面是为了让打包脚本能快速取版本，也不会因环境缺配置而失败。
	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	// 加载配置：首次生成，后续读取并让命令行显式参数覆盖
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg == nil {
		// 首次启动：用当前参数生成配置
		cfg = &config.Config{
			Server:     *server,
			AgentID:    *agentID,
			UUID:       *agentUUID,
			Heartbeat:  *hb,
			ProbeEvery: *probe,
			TLS:        *tlsEnabled,
			Insecure:   *insecure,
			Daemon:     *daemonize,
			ClientCert: *clientCert,
			ClientKey:  *clientKey,
			CACert:     *caCert,
			ProxyIP:    *proxyIP,
			// 公网取包解锁只认本机命令行/配置，不接受服务端下发（见 config 字段注释）
			AllowPublicPackageDownload: *allowPublicPkg,
			AgentKey:                   *agentKey,
			ServerFingerprint:          *serverFingerprint,
			EnrollmentSecret:           firstNonEmpty(*enrollSecret, *agentKey),
			UpgradePubKey:              *upgradePubKey,
			FileRoots:                  splitList(*fileRoots),
			LatencyTargets:             splitList(*latencyTargets),
		}
		if err := config.Save(*configPath, cfg); err != nil {
			log.Printf("警告: 写入配置文件 %s 失败: %v", *configPath, err)
		} else {
			log.Printf("已生成配置文件: %s（后续可直接运行 %s 或 %s 读取）", *configPath, os.Args[0], *configPath)
		}
	} else {
		// 已有配置：命令行显式参数（非默认值）覆盖配置文件
		if cfg.Server != "" && *server == os.Getenv("MOCENTER_SERVER") {
			*server = cfg.Server
		}
		if cfg.AgentID != "" && *agentID == os.Getenv("MOCENTER_AGENT_ID") {
			*agentID = cfg.AgentID
		}
		if cfg.UUID != "" && *agentUUID == os.Getenv("MOCENTER_AGENT_UUID") {
			*agentUUID = cfg.UUID
		}
		if cfg.Heartbeat > 0 && *hb == 30 {
			*hb = cfg.Heartbeat
		}
		if cfg.ProbeEvery > 0 && *probe == 10 {
			*probe = cfg.ProbeEvery
		}
		if cfg.TLS && !*tlsEnabled {
			*tlsEnabled = true
		}
		if cfg.Insecure && !*insecure {
			*insecure = true
		}
		if cfg.Daemon && !*daemonize {
			*daemonize = true
		}
		if cfg.ClientCert != "" && *clientCert == "" {
			*clientCert = cfg.ClientCert
		}
		if cfg.ClientKey != "" && *clientKey == "" {
			*clientKey = cfg.ClientKey
		}
		if cfg.CACert != "" && *caCert == "" {
			*caCert = cfg.CACert
		}
		if cfg.ProxyIP != "" && *proxyIP == os.Getenv("MOCENTER_PROXY_IP") {
			*proxyIP = cfg.ProxyIP
		}
		if cfg.AgentKey != "" && *agentKey == os.Getenv("MOCENTER_AGENT_KEY") {
			*agentKey = cfg.AgentKey
		}
		if cfg.ServerFingerprint != "" && *serverFingerprint == os.Getenv("MOCENTER_SERVER_FINGERPRINT") {
			*serverFingerprint = cfg.ServerFingerprint
		}
		if cfg.EnrollmentSecret != "" && *enrollSecret == os.Getenv("MOCENTER_ENROLL_SECRET") {
			*enrollSecret = cfg.EnrollmentSecret
		}
		if cfg.UpgradePubKey != "" && *upgradePubKey == os.Getenv("MOCENTER_UPGRADE_PUBKEY") {
			*upgradePubKey = cfg.UpgradePubKey
		}
		if len(cfg.FileRoots) > 0 && strings.TrimSpace(*fileRoots) == "" {
			*fileRoots = strings.Join(cfg.FileRoots, ",")
		}
		// 注意 flagWasSet 判断：显式传 -allow-public-package-download=false 必须能关掉，
		// 不能被配置里的 true 顶回去 —— 收紧用的开关不能 fail-open。
		if cfg.AllowPublicPackageDownload && !*allowPublicPkg && !flagWasSet("allow-public-package-download") {
			*allowPublicPkg = true
		}
		if len(cfg.LatencyTargets) > 0 && strings.TrimSpace(*latencyTargets) == "" {
			*latencyTargets = strings.Join(cfg.LatencyTargets, ",")
		}
	}

	// 服务端地址的两道校验必须放在参数回写**之前**：回写会把命令行值持久化，
	// 若配置里原本存着有效地址，一次 `-server your.server.com:9000`（老安装脚本就会这么传）
	// 会先把有效配置覆盖成占位符再退出，下次无参启动也读不回来了。
	if *server == "" {
		log.Fatal("请通过 -server、环境变量 MOCENTER_SERVER 或配置文件指定服务端地址")
	}
	// 安装介质里预置过的占位地址不能当配置用（L-7）。
	// 现在模板已改留空，这条是给**老部署**兜底的：从旧包/旧脚本装上的机器配置里
	// 仍然是 your.server.com:9000，不拦的话它会一直解析一个不存在的域名无限重连，
	// 界面表现为"服务在跑但主机永远不在线"，运维无从下手。
	if isPlaceholderServer(*server) {
		log.Fatalf("服务端地址仍是安装占位符 %q：请改成实际地址（-server host:port、环境变量 MOCENTER_SERVER，或配置文件里的 MOCENTER_SERVER）后重启", *server)
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。
	{
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		// 传空串的参数已按「未配置」归一，不能再算「显式给出」：
		// 否则 -server "" 会把配置文件里已存的服务端地址抹掉（服务单元缺键时就会这样）。
		clearBlankExplicit(explicit, blankFlags)
		field := map[string]func(){
			"server":                        func() { cfg.Server = *server },
			"agentid":                       func() { cfg.AgentID = *agentID },
			"uuid":                          func() { cfg.UUID = *agentUUID },
			"hb":                            func() { cfg.Heartbeat = *hb },
			"probe":                         func() { cfg.ProbeEvery = *probe },
			"tls":                           func() { cfg.TLS = *tlsEnabled },
			"insecure":                      func() { cfg.Insecure = *insecure },
			"daemon":                        func() { cfg.Daemon = *daemonize },
			"client-cert":                   func() { cfg.ClientCert = *clientCert },
			"client-key":                    func() { cfg.ClientKey = *clientKey },
			"ca-cert":                       func() { cfg.CACert = *caCert },
			"proxy-ip":                      func() { cfg.ProxyIP = *proxyIP },
			"allow-public-package-download": func() { cfg.AllowPublicPackageDownload = *allowPublicPkg },
			"agent-key":                     func() { cfg.AgentKey = *agentKey },
			"enroll-secret":                 func() { cfg.EnrollmentSecret = *enrollSecret },
			"server-fingerprint":            func() { cfg.ServerFingerprint = *serverFingerprint },
			"upgrade-pubkey":                func() { cfg.UpgradePubKey = *upgradePubKey },
			"file-roots":                    func() { cfg.FileRoots = splitList(*fileRoots) },
			"latency-targets":               func() { cfg.LatencyTargets = splitList(*latencyTargets) },
		}
		dirty := false
		for _, name := range []string{"server", "agentid", "uuid", "hb", "probe", "tls", "insecure", "daemon", "client-cert", "client-key", "ca-cert", "proxy-ip", "allow-public-package-download", "agent-key", "enroll-secret", "server-fingerprint", "upgrade-pubkey", "file-roots", "latency-targets"} {
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

	// 设备身份（UUID）：优先用配置里已持久化的值，其次实时探测；探测不到就留空
	//（服务端会退化为按 agent_id 识别）。拿到后写回配置，保证重启/改配置都不漂移。
	if *agentUUID == "" {
		if u := probepkg.HardwareUUID(); u != "" {
			*agentUUID = u
			log.Printf("已探测到硬件 UUID %q，作为设备唯一标识", *agentUUID)
		}
	}
	if *agentUUID != "" && cfg.UUID != *agentUUID {
		cfg.UUID = *agentUUID
		if err := config.Save(*configPath, cfg); err != nil {
			log.Printf("警告: 写入设备 UUID 到配置失败: %v", err)
		}
	}
	if *agentID == "" {
		// 未指定名称时：优先复用已持久化的 UUID，否则用硬件 UUID，再退回主机名
		if *agentUUID != "" {
			*agentID = *agentUUID
			log.Printf("未指定 AgentID，使用设备 UUID %q", *agentID)
		} else {
			host, _ := os.Hostname()
			*agentID = host
			log.Printf("未指定 AgentID，硬件 UUID 获取失败，回退使用主机名 %q", *agentID)
		}
	}

	// 单实例保护：同一份配置只允许一个 Agent 实例。
	//
	// 此前只在 Windows 生效（命名互斥体），Linux/macOS 直接放行 —— 于是
	// 「systemd 服务 + 定时任务保活 + 手工排查启动一份」很容易同时跑起多个实例：
	// 它们共用同一配置（同一 agent_id、同一套客户端证书）一起连服务端互相顶号，
	// 隧道、VNC、端口映射的会话状态错乱；定时任务每 5 分钟拉一个，越滚越多。
	// 现在非 Windows 走 flock（见 internal/winapp/singleton_unix.go），
	// 进程无论怎么退出（含被 SIGKILL、断电）锁都由内核自动释放，不会留下陈旧锁。
	//
	// 注意：这里**不会**去终止已有实例，只让自己的启动失败 —— 误杀一个正在
	// 跑远控会话的 Agent 后果不可控，交给运维判断更安全。
	singleKey := winapp.SingletonKey("Agent", *configPath)
	release, ok := winapp.AcquireSingleton(singleKey, *configPath)
	if !ok {
		log.Fatalf("已有一个使用同一配置(%s)的 Agent 在运行，本次启动终止（%s）",
			*configPath, winapp.DescribeSingleton(singleKey, *configPath))
	}
	defer release()
	log.Printf("[agent] 已取得单实例锁：%s", winapp.DescribeSingleton(singleKey, *configPath))

	// 换取成功后清理：证书路径写回配置、注册密钥清空（一次性使用）。
	// 这样运维在配置里再也看不到可直接冒充的长期凭据，只有本机 0600 的证书私钥。
	handleEnrolled := func(cred client.CredentialFiles) {
		cfg.ClientCert = cred.CertFile
		cfg.ClientKey = cred.KeyFile
		if cfg.CACert == "" {
			cfg.CACert = cred.CAFile
		}
		cfg.EnrollmentSecret = ""
		if err := config.Save(*configPath, cfg); err != nil {
			log.Printf("警告: 保存换取结果到配置失败: %v", err)
		}
	}

	start := func() {
		// 服务模式：进程级重启交给 SCM（安装器已配置失败恢复动作），但进程内仍需兜住
		// panic —— 否则 goroutine 静默死掉，服务却仍显示 Running。
		o := runOpts{
			server: *server, agentID: *agentID, agentUUID: *agentUUID, hb: *hb, probe: *probe,
			tls: *tlsEnabled, insecure: *insecure,
			clientCert: *clientCert, clientKey: *clientKey, caCert: *caCert,
			proxyIP: *proxyIP, agentKey: *agentKey,
			allowPublicPkg: *allowPublicPkg,
			upgradePubKey:  *upgradePubKey, fileRoots: splitList(*fileRoots),
			latencyTargets:    splitList(*latencyTargets),
			serverFingerprint: *serverFingerprint,
			enrollSecret:      firstNonEmpty(*enrollSecret, *agentKey),
			onEnrolled:        handleEnrolled,
			configPath:        *configPath, credsDir: *credsDir,
		}
		if *daemonize || winapp.IsWindowsService() {
			daemon.Supervise(func() { runClient(o) })
			return
		}
		runClient(o)
	}

	// 被 SCM 拉起时走服务控制协议（sc query/stop 才能正常工作）；否则按普通进程运行。
	// 用 IsWindowsService 自动识别，无需额外开关，也就不会"忘了加参数导致服务起不来"。
	if winapp.IsWindowsService() {
		logPath := filepath.Join(filepath.Dir(*configPath), "logs", agentServiceName+".log")
		if err := winapp.RedirectLogToFile(logPath); err != nil {
			log.Printf("警告: 重定向服务日志到 %s 失败: %v", logPath, err)
		} else {
			log.Printf("[agent] 服务模式启动，日志输出到 %s", logPath)
		}
		if err := winapp.RunAsService(agentServiceName, start, func() { os.Exit(0) }); err != nil {
			log.Fatalf("以 Windows 服务方式运行失败: %v", err)
		}
		return
	}

	start()
}

// agentServiceName Windows 服务名。必须与安装器注册时使用的名称一致
// （svc.Run 的名字对不上会导致 SCM 无法完成握手）。
const agentServiceName = "MoCenterAgent"

// runOpts 收敛 runClient 的入参，避免继续往位置参数上堆字段时错位。
type runOpts struct {
	server, agentID, agentUUID string
	hb, probe                  int
	tls, insecure              bool
	// allowPublicPkg 解锁公网取安装包（见 client.Config.AllowPublicDownload）。
	// 只来自本机命令行/配置文件，服务端无法下发。
	allowPublicPkg                         bool
	clientCert, clientKey, caCert, proxyIP string
	agentKey, upgradePubKey                string
	// fileRoots 允许服务端读写的本机目录；空 = client.DefaultFileRoot 的沙箱。
	// 只来自命令行/配置文件，服务端指令无法扩大。
	fileRoots                       []string
	latencyTargets                  []string
	serverFingerprint, enrollSecret string
	configPath, credsDir            string
	onEnrolled                      func(client.CredentialFiles)
}

func runClient(o runOpts) {
	c := client.New(client.Config{
		ServerAddr:          o.server,
		AgentID:             o.agentID,
		UUID:                o.agentUUID,
		Heartbeat:           seconds(o.hb),
		ProbeEvery:          seconds(o.probe),
		TLS:                 o.tls,
		Insecure:            o.insecure,
		ClientCert:          o.clientCert,
		ClientKey:           o.clientKey,
		CACert:              o.caCert,
		ProxyIP:             o.proxyIP,
		AllowPublicDownload: o.allowPublicPkg,
		AgentKey:            o.agentKey,
		UpgradePubKey:       o.upgradePubKey,
		FileRoots:           o.fileRoots,
		LatencyTargets:      o.latencyTargets,
		ServerFingerprint:   o.serverFingerprint,
		EnrollmentSecret:    o.enrollSecret,
		ConfigPath:          o.configPath,
		CredDir:             o.credsDir,
	})
	if o.onEnrolled != nil {
		c.SetOnEnrolled(o.onEnrolled)
	}
	log.Printf("[agent] 启动 %s，Agent %s，目标服务端 %s", version.Full(), o.agentID, o.server)
	// 两个"默认收紧、需本机显式放开"的开关都在启动日志里给出生效值：
	// 出问题时第一手证据就是"它到底以为自己的边界在哪"。
	log.Printf("[agent] 文件读写根目录: %s（用 -file-roots 扩大）",
		strings.Join(c.FileRoots(), ", "))
	if o.upgradePubKey == "" {
		log.Printf("[agent] 未配置 -upgrade-pubkey：远程升级已被拒绝（这是默认安全态）")
	}
	c.Run()
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// splitList 拆分逗号分隔的列表并去除空白项。
func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// flagWasSet 判断某参数是否在命令行显式出现（区别于「只是取到默认值」）。
// 用于「显式 false 必须能关掉配置里的 true」这类收紧语义。
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// firstNonEmpty 返回第一个非空白字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// placeholderServerHost 是历次安装介质（conf 模板、install-linux.sh、旧打包产物）
// 里预置过的占位主机名。
const placeholderServerHost = "your.server.com"

// isPlaceholderServer 判断服务端地址的主机段是否仍是安装占位符（L-7）。
// 只看主机段、大小写不敏感、允许带端口；`https://your.server.com` 这类带前缀的
// 写法主机段不是它，因此不误判。
func isPlaceholderServer(addr string) bool {
	host := strings.TrimSpace(strings.ToLower(addr))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		host = strings.TrimSuffix(host, "/")
	}
	return host == placeholderServerHost
}
