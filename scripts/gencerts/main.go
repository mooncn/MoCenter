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
//	  - 重复运行 server/client 会用现有 CA 重签，Agent 侧 client.crt/ca.crt 无需更换，
//	    仅服务端替换 server.crt/server.key。
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
	ip := fs.String("ip", "127.0.0.1", "服务端 IP（逗号分隔，server / all 用，加入 SAN）")
	dns := fs.String("dns", "", "服务端域名（逗号分隔，server / all 用，加入 SAN）")
	serverCfg := fs.String("server-config", "", "生成后把证书路径写入服务端配置文件")
	agentCfg := fs.String("agent-config", "", "生成后把证书路径写入 Agent 配置文件")
	fs.Parse(os.Args[2:]) // 跳过子命令
	cmd := "all"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	_ = os.MkdirAll(*dir, 0700)

	switch cmd {
	case "ca":
		genCA(*dir)
	case "server":
		genServer(*dir, splitList(*ip), splitList(*dns))
	case "client":
		genClient(*dir)
	case "all":
		genCA(*dir)
		genServer(*dir, splitList(*ip), splitList(*dns))
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
  go run . server -ip 1.2.3.4 [-dns a.com] [-dir 目录]
  go run . client                        [-dir 目录]
  go run . all -ip 1.2.3.4 [-dns a.com]  [-dir 目录]

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
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "MoCenter-CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
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

func certPEM(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }

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