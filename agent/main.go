// MoCenter Agent 客户端入口。
//
// Agent 部署在各内网主机上，主动连接公网总控服务端维持 TCP 长连接，
// 上报探针数据、接收并执行远程命令。
//
// 用法：
//   ./agent -server host:port -agentid <id> [-hb 30] [-probe 10]
package main

import (
	"flag"
	"log"
	"os"
	"time"

	"mocenter/agent/internal/client"
	"mocenter/agent/internal/daemon"
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
	flag.Parse()

	if *server == "" {
		log.Fatal("请通过 -server 或环境变量 MOCENTER_SERVER 指定服务端地址")
	}
	if *agentID == "" {
		host, _ := os.Hostname()
		*agentID = host
		log.Printf("未指定 AgentID，使用主机名 %q", *agentID)
	}

	// 进程守护：崩溃自动重启
	if *daemonize {
		daemon.Supervise(func() {
			runClient(*server, *agentID, *hb, *probe, *tlsEnabled, *insecure, *clientCert, *clientKey, *caCert)
		})
		return
	}

	runClient(*server, *agentID, *hb, *probe, *tlsEnabled, *insecure, *clientCert, *clientKey, *caCert)
}

func runClient(server, agentID string, hb, probe int, tlsEnabled, insecure bool, clientCert, clientKey, caCert string) {
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
	})
	log.Printf("[agent] 启动 Agent %s，目标服务端 %s", agentID, server)
	c.Run()
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }
