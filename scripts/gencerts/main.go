// 用 Go 标准库生成/重签 MoCenter 的自签名证书（Windows 无需安装 openssl）。
//
// 子命令（客户端与服务端分开）：
//
//	 	go run . ca                  -dir <目录>          生成/覆盖 CA（ca.crt/ca.key）
//	 	go run . server -ip <IP>     -dir <目录>          用 CA 签发服务端证书（server.crt/server.key）
//	 	go run . client              -dir <目录>          用 CA 签发客户端证书（client.crt/client.key）
//	 	go run . all -ip <IP>        -dir <目录>          一次生成全部证书
//
//	说明：
//	  - server 证书的 SAN 会把 -ip 加入，供 Agent 按真实服务端 IP 做 TLS 校验；
//	  - 重复运行 server/client 会用现有 CA 重签，Agent 侧 client.crt/ca.crt 无需更换，
//	    仅服务端替换 server.crt/server.key。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const defaultDir = "certs"

func main() {
	fs := flag.NewFlagSet("gencerts", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "证书输出目录")
	ip := fs.String("ip", "127.0.0.1", "服务端真实 IP（server / all 用，加入 SAN）")
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
		genServer(*dir, *ip)
	case "client":
		genClient(*dir)
	case "all":
		genCA(*dir)
		genServer(*dir, *ip)
		genClient(*dir)
	case "-h", "help", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "未知子命令:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`用法:
  go run . ca            [-dir 目录]
  go run . server -ip IP [-dir 目录]
  go run . client        [-dir 目录]
  go run . all -ip IP    [-dir 目录]
`)
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

func genServer(dir, ip string) {
	caCert, caKey := loadCA(dir)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject:      pkix.Name{CommonName: "mocenter-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"mocenter-server"},
		IPAddresses:  dedupeIPs([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0"), net.ParseIP(ip)}),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	must(err)
	write(filepath.Join(dir, "server.crt"), certPEM(der))
	write(filepath.Join(dir, "server.key"), keyPEM(key))
	fmt.Println("[ok] Server:", filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), " SAN 含 IP", ip)
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