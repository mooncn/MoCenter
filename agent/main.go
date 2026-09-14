// MoCenter Agent 客户端入口。
//
// Agent 部署在各内网主机上，主动连接公网总控服务端维持 TCP 长连接，
// 上报探针数据、接收并执行远程命令。
//
// 用法：
//   首次：./agent -server host:port -agentid <id> [-hb 30] [-probe 10]
//         （首次运行会自动把参数写入配置文件 mocenter-agent.json）
//   后续：./agent（自动读取配置文件；命令行显式参数优先级更高）
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"mocenter/agent/internal/client"
	"mocenter/agent/internal/config"
	"mocenter/agent/internal/daemon"
	probepkg "mocenter/agent/internal/probe"
)

func main() {
	server := flag.String("server", os.Getenv("MOCENTER_SERVER"), "服务端地址 host:port")
	agentID := flag.String("agentid", os.Getenv("MOCENTER_AGENT_ID"), "Agent 唯一标识")
	hb := flag.Int("hb", 30, "心跳间隔(秒)")
	probe := flag.Int("probe", 10, "探针采集周期(秒)")
	daemonize := flag.Bool("daemon", false, "以守护进程方式运行（后台运行并崩溃自启）")
	tlsEnabled := flag.Bool("tls", false, "启用 TLS 加密连接")
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书校验（自签名测试）")
	clientCert := flag.String("client-cert", "", "mTLS 客户端证书文件路径（服务端启用双向认证时必填）")
	clientKey := flag.String("client-key", "", "mTLS 客户端私钥文件路径")
	caCert := flag.String("ca-cert", "", "用于校验服务端证书的 CA 证书路径（不指定则用 -insecure 跳过校验）")
	configPath := flag.String("config", config.DefaultPath(), "配置文件路径（首次启动自动生成）")
	proxyIP := flag.String("proxy-ip", os.Getenv("MOCENTER_PROXY_IP"), "国内加速代理前缀（用于公网 IP 探测等境外服务，如 http://fa.mooncn.win/?url= ）")
	flag.Parse()

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
			Heartbeat:  *hb,
			ProbeEvery: *probe,
			TLS:        *tlsEnabled,
			Insecure:   *insecure,
			Daemon:     *daemonize,
			ClientCert: *clientCert,
			ClientKey:  *clientKey,
			CACert:     *caCert,
			ProxyIP:    *proxyIP,
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
	}

	// 命令行显式传入的参数（flag.Visit）优先级最高，并自动保存回配置文件，
	// 这样下次可直接按最新参数启动，无需重复传参。
	{
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		field := map[string]func(){
			"server":      func() { cfg.Server = *server },
			"agentid":     func() { cfg.AgentID = *agentID },
			"hb":          func() { cfg.Heartbeat = *hb },
			"probe":       func() { cfg.ProbeEvery = *probe },
			"tls":         func() { cfg.TLS = *tlsEnabled },
			"insecure":    func() { cfg.Insecure = *insecure },
			"daemon":      func() { cfg.Daemon = *daemonize },
			"client-cert": func() { cfg.ClientCert = *clientCert },
			"client-key":  func() { cfg.ClientKey = *clientKey },
			"ca-cert":     func() { cfg.CACert = *caCert },
			"proxy-ip":    func() { cfg.ProxyIP = *proxyIP },
		}
		dirty := false
		for _, name := range []string{"server", "agentid", "hb", "probe", "tls", "insecure", "daemon", "client-cert", "client-key", "ca-cert", "proxy-ip"} {
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

	if *server == "" {
		log.Fatal("请通过 -server、环境变量 MOCENTER_SERVER 或配置文件指定服务端地址")
	}
	if *agentID == "" {
		// 优先使用硬件 UUID 作为 AgentID，获取失败时回退到主机名
		if uuid := probepkg.HardwareUUID(); uuid != "" {
			*agentID = uuid
			log.Printf("未指定 AgentID，使用硬件 UUID %q", *agentID)
		} else {
			host, _ := os.Hostname()
			*agentID = host
			log.Printf("未指定 AgentID，硬件 UUID 获取失败，回退使用主机名 %q", *agentID)
		}
	}

	// 进程守护：崩溃自动重启
	if *daemonize {
		daemon.Supervise(func() {
			runClient(*server, *agentID, *hb, *probe, *tlsEnabled, *insecure, *clientCert, *clientKey, *caCert, *proxyIP)
		})
		return
	}

	runClient(*server, *agentID, *hb, *probe, *tlsEnabled, *insecure, *clientCert, *clientKey, *caCert, *proxyIP)
}

func runClient(server, agentID string, hb, probe int, tlsEnabled, insecure bool, clientCert, clientKey, caCert, proxyIP string) {
	c := client.New(client.Config{
		ServerAddr: server,
		AgentID:    agentID,
		Heartbeat:  seconds(hb),
		ProbeEvery: seconds(probe),
		TLS:        tlsEnabled,
		Insecure:   insecure,
		ClientCert: clientCert,
		ClientKey:  clientKey,
		CACert:     caCert,
		ProxyIP:    proxyIP,
	})
	log.Printf("[agent] 启动 Agent %s，目标服务端 %s", agentID, server)
	c.Run()
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }