// 用 Go 标准库生成/重签 MoCenter 的自签名证书（Windows 无需安装 openssl）。
//
// 子命令（客户端与服务端分开）：
//
//	 	go run . ca                          [-dir 目录]
//	 	go run . server -ip 1.2.3.4 [-dns a.com]
//	 	go run . client
//	 	go run . all -ip 1.2.3.4 [-dns a.com]
//
//	常用旗标：
//	  -dir             证书输出目录（默认 certs）
//	  -ip              服务端 IP，逗号分隔可填多个，加入 server 证书 SAN
//	  -dns             服务端域名，逗号分隔可填多个，加入 server 证书 SAN
//	  -server-config   生成后将证书绝对路径写进服务端配置文件
//	  -agent-config    生成后将证书绝对路径写进 Agent 配置文件
//
//	说明：
//	  - server 证书的 SAN 会把 -ip / -dns 加入，供 Agent 按真实服务端 IP/域名做 TLS 校验；
//	    未指定 -ip 时自动探测公网 IP；本机网卡地址与回环地址始终写入
//	    （genServer 的既有行为，保证同机/内网直连也能通过校验）；
//	  - 重复运行 server/client 会用现有 CA 重签，Agent 侧 client.crt/ca.crt 无需更换，
//	    仅服务端替换 server.crt/server.key。
//
//	**一般不需要本工具**：MoCenter 服务端首次启动会自动生成 CA 与证书，
//	Agent 也只需服务端数据目录里的 agent.key 就能自动换取客户端证书（"简单模式"）。
//	本工具适用于以下场景：
//	  - 想用外部 CA / 正式证书（受信任的 HTTPS、内网 PKI）；
//	  - 想先把证书预置好再部署（离线、审计要求固定的长期证书）；
//	  - 需要在多台机器间分发同一套凭证。
//	只有 SAME CA 签出的证书才能互相通过校验 —— 与服务端内置 CA 混用时，服务端的
//	ca.crt/ca.key 需要替换为本工具生成的这套。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultDir = "certs"

func main() {
	fs := flag.NewFlagSet("gencerts", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "证书输出目录")
	ip := fs.String("ip", "", "服务端 IP（逗号分隔，server / all 用，加入 SAN）；留空=自动探测本机公网 IP")
	dns := fs.String("dns", "", "服务端域名（逗号分隔，server / all 用，加入 SAN）")
	noAuto := fs.Bool("no-auto", false, "不自动探测公网 IP（仅用 -ip/-dns 指定的地址）")
	serverCfg := fs.String("server-config", "", "生成后把证书路径写入服务端配置文件")
	agentCfg := fs.String("agent-config", "", "生成后把证书路径写入 Agent 配置文件")
	fs.Parse(os.Args[2:]) // 跳过子命令
	cmd := "all"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	_ = os.MkdirAll(*dir, 0700)

	ips := splitList(*ip)
	dnss := splitList(*dns)
	// 服务端证书必须按「Agent 实际连接的地址」签发。手工敲 -ip 很容易漏
	// （服务器公网 IP 往往部署后才知道），默认改为自动探测公网 IP，
	// 并补上本机网卡 / 回环地址。
	if cmd == "server" || cmd == "all" {
		if len(ips) == 0 && !*noAuto {
			if v4, v6 := detectPublicIPs(); v4 != "" || v6 != "" {
				if v4 != "" {
					fmt.Println("[ip] 探测到公网 IPv4:", v4)
					ips = append(ips, v4)
				}
				if v6 != "" {
					fmt.Println("[ip] 探测到公网 IPv6:", v6)
					ips = append(ips, v6)
				}
			} else {
				fmt.Fprintln(os.Stderr, "[提示] 未能探测到公网 IP，将仅使用本机网卡地址；"+
					"若 Agent 走公网连接，请显式指定: -ip <服务端公网IP>")
			}
		}
		if len(ips) == 0 {
			fmt.Fprintln(os.Stderr, "[提示] 未解析到任何服务端 IP。证书可能无法通过 Agent 校验；"+
				"请用 -ip <公网IP> 或 -dns <域名> 明确指定。")
		}
	}

	switch cmd {
	case "ca":
		genCA(*dir)
	case "server":
		genServer(*dir, ips, dnss)
	case "client":
		genClient(*dir)
	case "all":
		genCA(*dir)
		genServer(*dir, ips, dnss)
		genClient(*dir)
	case "-h", "help", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "未知子命令:", cmd)
		usage()
		os.Exit(2)
	}

	if *serverCfg != "" || *agentCfg != "" {
		updateConfigs(*dir, *serverCfg, *agentCfg)
	}
}

func usage() {
	fmt.Print(`用法:
  go run . ca                            [-dir 目录]
  go run . server [-ip 1.2.3.4] [-dns a.com] [-dir 目录]
  go run . client                             [-dir 目录]
  go run . all    [-ip 1.2.3.4] [-dns a.com]  [-dir 目录]

服务端证书 SAN：-ip 留空时会自动探测本机公网 IP（本机网卡地址与回环地址始终写入）。
可用 -no-auto 关闭公网探测。

可选配置写入:
  -server-config <server.json>   生成后把 tls_cert/tls_key/ca_cert 写入服务端配置
  -agent-config  <agent.json>    生成后把 client_cert/client_key/ca_cert 写入 Agent 配置

示例（在老服务器上为公网域名签证书并写入两侧配置）:
  go run . all -ip 122.5.17.3 -dns your.domain.com -server-config ../mocenter-server.json -agent-config ../mocenter-agent.json
`)
}

// splitList 把逗号分隔的字符串拆成去空元素列表。
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// updateConfigs 把生成的证书绝对路径写入服务端 / Agent 配置文件（保留既有字段）。
func updateConfigs(dir, serverCfg, agentCfg string) {
	abs := func(name string) string {
		p, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			return filepath.Join(dir, name)
		}
		return p
	}
	if serverCfg != "" {
		srv := loadConfig(serverCfg)
		srv["tls_cert"] = abs("server.crt")
		srv["tls_key"] = abs("server.key")
		srv["ca_cert"] = abs("ca.crt")
		if err := saveConfig(serverCfg, srv); err != nil {
			fmt.Fprintln(os.Stderr, "error: 写入服务端配置失败:", err)
		} else {
			fmt.Println("[ok] 服务端配置:", serverCfg)
		}
	}
	if agentCfg != "" {
		agn := loadConfig(agentCfg)
		agn["tls"] = true
		agn["client_cert"] = abs("client.crt")
		agn["client_key"] = abs("client.key")
		agn["ca_cert"] = abs("ca.crt")
		if err := saveConfig(agentCfg, agn); err != nil {
			fmt.Fprintln(os.Stderr, "error: 写入 Agent 配置失败:", err)
		} else {
			fmt.Println("[ok] Agent 配置:", agentCfg)
		}
	}
}

// loadConfig 读取配置文件为通用 map；不存在返回空 map。
func loadConfig(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// saveConfig 以 JSON（保留非零键）写入配置文件，自动创建目录。
func saveConfig(path string, m map[string]any) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	clean := map[string]any{}
	for k, v := range m {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case bool:
			if !t {
				continue
			}
		case float64:
			if t == 0 {
				continue
			}
		}
		clean[k] = v
	}
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func genCA(dir string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "MoCenter-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)
	write(filepath.Join(dir, "ca.crt"), certPEM(der))
	write(filepath.Join(dir, "ca.key"), keyPEM(key))
	fmt.Println("[ok] CA:", filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
}

func genServer(dir string, ips, dnss []string) {
	caCert, caKey := loadCA(dir)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dnsNames := []string{"mocenter-server"}
	dnsNames = append(dnsNames, dnss...)
	ipList := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")}
	// 本机所有地址一并写进 SAN。
	//
	// 为什么必须这么做：SAN 里只有 127.0.0.1 时，Agent 从「本机自己的 LAN 地址」
	// （例如 192.168.117.1）连过来会直接报
	//   x509: certificate is valid for 127.0.0.1, ::1, not 192.168.117.1
	// 而这一跳是**没有用的**：既然 Agent 连的是本机地址，它就是同一台主机，
	// 服务端证书自带的本机身份足以通过校验。少写这几个 IP 只会逼运维改成
	// -insecure（等于彻底关掉校验），反而更不安全。
	ipList = append(ipList, localIPs()...)
	for _, s := range ips {
		ipList = append(ipList, net.ParseIP(s))
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject:      pkix.Name{CommonName: firstNonEmpty(append(dnss, "mocenter-server"))},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dedupeStrs(dnsNames),
		IPAddresses:  dedupeIPs(ipList),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	must(err)
	write(filepath.Join(dir, "server.crt"), certPEM(der))
	write(filepath.Join(dir, "server.key"), keyPEM(key))
	fmt.Printf("[ok] Server: san ip=%v dns=%v\n", tmpl.IPAddresses, tmpl.DNSNames)
}

func dedupeStrs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(in []string) string {
	for _, s := range in {
		if s != "" {
			return s
		}
	}
	return ""
}

// localIPs 返回本机所有非环回、非链路本地地址（含 IPv6）。
// 取不到时返回空，不影响主流程。
func localIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		out = append(out, ip)
	}
	return out
}

func genClient(dir string) {
	caCert, caKey := loadCA(dir)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject:      pkix.Name{CommonName: "mocenter-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"agent"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	must(err)
	write(filepath.Join(dir, "client.crt"), certPEM(der))
	write(filepath.Join(dir, "client.key"), keyPEM(key))
	fmt.Println("[ok] Client:", filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"))
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey) {
	cb, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	must(err)
	kb, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	must(err)
	cblock, _ := pem.Decode(cb)
	kblock, _ := pem.Decode(kb)
	if cblock == nil || kblock == nil {
		must(fmt.Errorf("CA 证书文件缺失或损坏，请先运行: go run . ca"))
	}
	caCert, err := x509.ParseCertificate(cblock.Bytes)
	must(err)
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	must(err)
	return caCert, key
}

func dedupeIPs(ips []net.IP) []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		if s := ip.String(); !seen[s] {
			seen[s] = true
			out = append(out, ip)
		}
	}
	return out
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(k *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func write(p string, b []byte) { must(os.WriteFile(p, b, 0600)) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
