// 命令 signcert —— 为服务端签发「按实际地址」的 TLS 证书（默认用本机公网 IP）。
//
// 背景：服务端证书的 SAN 必须包含 Agent 连接时使用的地址，否则 Agent 侧
// 会报 `x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>`。
// 部署到公网服务器后地址往往是部署时才确定的，手工改 SAN 很容易漏。
//
// 本命令的作用：
//  1. 解析「本机实际对外地址」—— -ip/-dns 显式指定优先，
//     否则自动探测公网 IP（IPv4 + IPv6）、回退本机网卡地址；
//  2. 用同一个 CA（ca.crt/ca.key）签发服务端证书，把地址写进 SAN；
//  3. 可选把证书路径写回服务端配置（-server-config）、生成 CA 客户端证书（-client）。
//
// 与 scripts/gencerts 的关系：gencerts 负责「CA / 客户端」等证书的生成与整体管理，
// 本命令专注服务端证书的地址解析与签发，也可直接用它生成的 CA。
//
// 用法（在 scripts/signcert 目录下）：
//
//	go run .                      # 按公网 IP 签发到 certs/（CA 不存在则自动创建）
//	go run . -ip 82.156.7.85      # 显式指定 IP（可多个，逗号分隔）
//	go run . -dns mocenter.example.com
//	go run . -server-config ../../mocenter-server.json   # 顺便写回服务端配置
//	go run . -no-auto -ip 10.0.0.5 # 不做公网探测，仅用指定地址
//	go run . -print               # 只显示解析到的地址，不签发
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultDir   = "certs"
	caCertName   = "ca.crt"
	caKeyName    = "ca.key"
	srvCertName  = "server.crt"
	srvKeyName   = "server.key"
	clntCertName = "client.crt"
	clntKeyName  = "client.key"
	certDays     = 3650
)

func main() {
	fs := flag.NewFlagSet("signcert", flag.ExitOnError)
	dir := fs.String("dir", defaultDir, "证书输出目录")
	ipFlag := fs.String("ip", "", "服务端 IP（逗号分隔，可多个）；留空=自动探测公网 IP")
	dnsFlag := fs.String("dns", "", "服务端域名（逗号分隔，可多个，如 server.example.com）")
	auto := fs.Bool("auto", true, "未指定 -ip 时自动探测公网 IP")
	localIPs := fs.Bool("local-ips", true, "把本机网卡地址也加入 SAN（便于内网直连调试）")
	loopback := fs.Bool("loopback", true, "把 127.0.0.1/::1 加入 SAN（便于本机调试）")
	extra := fs.String("extra-ip", "", "额外附加 IP（逗号分隔，如内网地址、0.0.0.0）")
	clientCert := fs.Bool("client", false, "同时用同一 CA 签发 Agent 客户端证书（缺失或需要轮换时用）")
	serverCfg := fs.String("server-config", "", "生成后把 tls_cert/tls_key/ca_cert 绝对路径写入服务端配置文件")
	agentCfg := fs.String("agent-config", "", "生成后把 client_cert/client_key/ca_cert 写入 Agent 配置文件")
	renewCA := fs.Bool("renew-ca", false, "重新生成 CA（会作废已分发的 Agent 客户端证书）")
	printOnly := fs.Bool("print", false, "只打印解析到的地址与结论，不签发证书")
	timeout := fs.Duration("timeout", 5*time.Second, "公网 IP 探测超时")
	certOut := fs.String("cert-out", "", "服务端证书输出路径（默认 <dir>/server.crt）")
	keyOut := fs.String("key-out", "", "服务端私钥输出路径（默认 <dir>/server.key）")
	_ = fs.Parse(os.Args[1:])

	if err := os.MkdirAll(*dir, 0700); err != nil {
		fail("创建证书目录失败: %v", err)
	}

	// ---- 1. 解析地址 ----
	ips := splitList(*ipFlag)
	dnss := splitList(*dnsFlag)
	explicit := len(ips) > 0 || len(dnss) > 0

	var warn []string
	if len(ips) == 0 && *auto {
		pub4, pub6, errs := detectPublicIPs(*timeout)
		for _, e := range errs {
			warn = append(warn, e.Error())
		}
		if pub4 != "" {
			ips = append(ips, pub4)
			fmt.Printf("[ip] 探测到公网 IPv4: %s\n", pub4)
		}
		if pub6 != "" {
			ips = append(ips, pub6)
			fmt.Printf("[ip] 探测到公网 IPv6: %s\n", pub6)
		}
		if pub4 == "" && pub6 == "" {
			warn = append(warn, "未能探测到公网出口 IP（网络不可达或被代理拦截），将仅使用本机网卡地址")
		}
	}
	if *localIPs {
		if l := localInterfaceIPs(); len(l) > 0 {
			ips = append(ips, l...)
			fmt.Printf("[ip] 本机网卡地址: %s\n", strings.Join(l, ", "))
		}
	}
	ips = append(ips, splitList(*extra)...)
	if *loopback {
		ips = append(ips, "127.0.0.1", "::1")
	}

	ips = dedupe(ips, true)
	dnss = dedupe(dnss, false)
	sort.Strings(ips)

	// 兜底：什么都没解析到（且用户也没指定）时不能签出一张无 SAN 的废证书
	if len(ips) == 0 && len(dnss) == 0 {
		fail("没有解析到任何可用地址。请显式指定：-ip <服务端公网IP> 或 -dns <域名>")
	}
	if len(ips) == 0 && !explicit {
		warn = append(warn, "未解析到 IP，证书仅含域名 SAN")
	}
	if len(dnss) == 0 {
		warn = append(warn, "未指定 -dns：Agent 若用「域名:9000」连接服务端，仍会校验失败；"+
			"建议同时加上 -dns <Agent 实际使用的域名>")
	}

	// 可选：发一张仅含 IP 的证书副本，避免内网 Agent 连内网地址、公网 Agent 连公网地址的取舍问题
	fmt.Println()
	fmt.Println("== 将写入服务端证书 SAN ==")
	if len(ips) > 0 {
		fmt.Printf("   IP : %s\n", strings.Join(ips, ", "))
	}
	if len(dnss) > 0 {
		fmt.Printf("   DNS: %s\n", strings.Join(dnss, ", "))
	}
	if *printOnly {
		fmt.Println()
		fmt.Println("（-print 只预览，未签发证书）")
		return
	}

	// ---- 2. 准备 CA ----
	if *renewCA {
		if _, err := os.Stat(filepath.Join(*dir, caCertName)); err == nil {
			backupCA(*dir)
		}
		genCA(*dir)
	}
	caCert, caKey, err := loadCA(*dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("[ca] 未找到现有 CA，自动创建:")
			genCA(*dir)
			caCert, caKey, err = loadCA(*dir)
		}
		if err != nil {
			fail("%v", err)
		}
	}
	caFingerprint := sha256Hex(caCert.Raw)

	// ---- 3. 签发服务端证书 ----
	srvCertPath := firstNonEmpty(*certOut, filepath.Join(*dir, srvCertName))
	srvKeyPath := firstNonEmpty(*keyOut, filepath.Join(*dir, srvKeyName))
	if err := signServer(srvCertPath, srvKeyPath, caCert, caKey, ips, dnss); err != nil {
		fail("签发服务端证书失败: %v", err)
	}
	fmt.Printf("[ok] 服务端证书: %s\n[ok] 服务端私钥: %s\n", srvCertPath, srvKeyPath)

	// ---- 4. 可选：客户端证书 ----
	if *clientCert {
		if err := signClient(filepath.Join(*dir, clntCertName), filepath.Join(*dir, clntKeyName), caCert, caKey); err != nil {
			fail("签发客户端证书失败: %v", err)
		}
		fmt.Printf("[ok] 客户端证书: %s\n[ok] 客户端私钥: %s\n",
			filepath.Join(*dir, clntCertName), filepath.Join(*dir, clntKeyName))
	}

	// ---- 5. 可选：写回配置 ----
	if *serverCfg != "" {
		srv := loadJSON(*serverCfg)
		srv["tls_cert"] = abs(srvCertPath)
		srv["tls_key"] = abs(srvKeyPath)
		srv["ca_cert"] = abs(filepath.Join(*dir, caCertName))
		if err := saveJSON(*serverCfg, srv); err != nil {
			fmt.Fprintf(os.Stderr, "!! 写入服务端配置失败: %v\n", err)
		} else {
			fmt.Printf("[ok] 服务端配置: %s\n", *serverCfg)
		}
	}
	if *agentCfg != "" {
		agn := loadJSON(*agentCfg)
		agn["tls"] = true
		agn["client_cert"] = abs(filepath.Join(*dir, clntCertName))
		agn["client_key"] = abs(filepath.Join(*dir, clntKeyName))
		agn["ca_cert"] = abs(filepath.Join(*dir, caCertName))
		if err := saveJSON(*agentCfg, agn); err != nil {
			fmt.Fprintf(os.Stderr, "!! 写入 Agent 配置失败: %v\n", err)
		} else {
			fmt.Printf("[ok] Agent 配置: %s\n", *agentCfg)
		}
	}

	// ---- 6. 收尾提示 ----
	fmt.Println()
	fmt.Printf("CA 指纹(SHA-256): %s\n", caFingerprint)
	fmt.Println("Agent 侧无需更换 client.crt/ca.crt（同一 CA 重签，只替换服务端证书）")
	fmt.Println()
	fmt.Println("重启服务端即可生效：")
	fmt.Printf("  ./mocenter-server -listen :9000 -http :8080 -tls-cert %s -tls-key %s -ca-cert %s\n",
		srvCertPath, srvKeyPath, filepath.Join(*dir, caCertName))
	if !*clientCert {
		fmt.Println("若 Agent 使用 mTLS，请确认 certs/client.crt 已分发；需要重发时加 -client 参数。")
	}
	fmt.Println()
	fmt.Println("Agent 侧连接示例（校验服务端证书，需分发 ca.crt）：")
	fmt.Printf("  ./mocenter-agent -server <上面某个地址>:9000 -tls -ca-cert ca.crt \\\n" +
		"      -client-cert client.crt -client-key client.key\n")
	for _, w := range warn {
		fmt.Fprintf(os.Stderr, "[提示] %s\n", w)
	}
}

// ---------------- 地址解析 ----------------

// detectPublicIPs 探测本机公网出口 IP（IPv4 / IPv6）。返回错误列表用于提示，不中断流程。
func detectPublicIPs(timeout time.Duration) (string, string, []error) {
	var errs []error
	v4, err := detectFrom([]string{
		"https://api.ipify.org?format=json",
		"https://ipv4.ident.me/",
		"https://ifconfig.me/ip",
		"https://api.ip.sb/ip",
	}, timeout)
	if err != nil {
		errs = append(errs, fmt.Errorf("公网 IPv4 探测失败: %v", err))
	}
	v6, err := detectFrom([]string{
		"https://api6.ipify.org?format=json",
		"https://ipv6.ident.me/",
	}, timeout)
	if err != nil {
		errs = append(errs, fmt.Errorf("公网 IPv6 探测失败: %v", err))
	}
	return v4, v6, errs
}

// detectFrom 依次请求多个回显服务，返回首个解析出的公网 IP。
func detectFrom(urls []string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for _, u := range urls {
		ip, err := fetchEchoIP(client, u)
		if err != nil {
			lastErr = err
			continue
		}
		if ip != "" {
			return ip, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("所有回显服务均未返回可用地址")
	}
	return "", lastErr
}

func fetchEchoIP(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(body))
	// JSON 响应：{"ip":"1.2.3.4"} / {"ip":"1.2.3.4","ipv6":"..."}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) == nil {
		for _, k := range []string{"ip", "query", "IPv4", "IPv6"} {
			if s, ok := m[k].(string); ok {
				if ip := parsePublicIP(s); ip != "" {
					return ip, nil
				}
			}
		}
	}
	return parsePublicIP(text), nil
}

// parsePublicIP 从响应文本中提取公网 IP（过滤回环/私网/非法值）。
func parsePublicIP(s string) string {
	// 纯文本响应可能带换行或多余说明，取第一段 token
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return ""
	}
	ip := net.ParseIP(strings.Trim(f[0], `"',[]`))
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}

// localInterfaceIPs 返回本机网卡上的单播地址（含私网），用于内网直连调试。
func localInterfaceIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		} else {
			out = append(out, ipnet.IP.String())
		}
	}
	return out
}

// ---------------- 证书签发 ----------------

func genCA(dir string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	serial, err := randSerial()
	must(err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "MoCenter-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(err)
	write(filepath.Join(dir, caCertName), certPEM(der))
	write(filepath.Join(dir, caKeyName), keyPEM(key))
	fmt.Printf("[ok] CA: %s %s\n", filepath.Join(dir, caCertName), filepath.Join(dir, caKeyName))
}

// signServer 按给定地址签发服务端证书（SAN = IP + DNS）。
func signServer(certPath, keyPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, ips, dnss []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	// CN 用首个域名，否则首个 IP —— 现代 TLS 只看 SAN，这里仅为可读性。
	cn := firstNonEmpty(append(append([]string{}, dnss...), ips...)...)
	if cn == "" {
		cn = "mocenter-server"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dedupe(dnss, false),
		IPAddresses:  parseIPs(ips),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := write(certPath, certPEM(der)); err != nil {
		return err
	}
	return write(keyPath, keyPEM(key))
}

func signClient(certPath, keyPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mocenter-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := write(certPath, certPEM(der)); err != nil {
		return err
	}
	return write(keyPath, keyPEM(key))
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(filepath.Join(dir, caCertName))
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, caKeyName))
	if err != nil {
		return nil, nil, err
	}
	cblock, _ := pem.Decode(cb)
	if cblock == nil {
		return nil, nil, fmt.Errorf("%s 不是合法的 PEM 证书", filepath.Join(dir, caCertName))
	}
	caCert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 CA 证书失败: %v", err)
	}
	kblock, _ := pem.Decode(kb)
	if kblock == nil {
		return nil, nil, fmt.Errorf("%s 不是合法的 PEM 私钥", filepath.Join(dir, caKeyName))
	}
	caKey, err := parseECKey(kblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("解析 CA 私钥失败: %v", err)
	}
	return caCert, caKey, nil
}

// backupCA 把现有 CA 备份成 ca.crt.bak-<时间> / ca.key.bak-<时间>，避免误覆盖后无法恢复。
func backupCA(dir string) {
	stamp := time.Now().Format("20060102-150405")
	for _, n := range []string{caCertName, caKeyName} {
		src := filepath.Join(dir, n)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := src + ".bak-" + stamp
		if err := os.Rename(src, dst); err == nil {
			fmt.Printf("[ca] 已备份原 CA: %s\n", dst)
		}
	}
}

// ---------------- 小工具 ----------------

// splitList 拆分逗号分隔列表，去空白；同时容忍分号与空格混用。
func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';'
	}) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dedupe 去重；whenIP 为真时忽略非法 IP 值（并规范化书写形式）。
func dedupe(in []string, whenIP bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if whenIP {
			ip := net.ParseIP(s)
			if ip == nil {
				fmt.Fprintf(os.Stderr, "[提示] 忽略非法 IP: %q（如需按域名签发请用 -dns）\n", s)
				continue
			}
			s = ip.String()
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func parseIPs(in []string) []net.IP {
	var out []net.IP
	for _, s := range in {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// firstNonEmpty 返回首个非空字符串，全空时返回 ""。
func firstNonEmpty(in ...string) string {
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parseECKey(der []byte) (*ecdsa.PrivateKey, error) {
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	// 兼容 PKCS#8（EC PRIVATE KEY / PRIVATE KEY 两种 PEM 头）
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("私钥不是 ECDSA 类型")
	}
	return ec, nil
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(k *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func write(p string, b []byte) error {
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}
	// 私钥/证书统一 0600：只有属主可读
	return os.WriteFile(p, b, 0600)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
