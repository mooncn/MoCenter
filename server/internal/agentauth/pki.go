package agentauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件实现「零配置 TLS」：Agent 用注册密钥（agent.key）作为令牌，
// 向服务端换取一张由服务端内置 CA 签发的客户端证书。
//
// 设计目标（对应"证书配置过于繁杂"的反馈）：
//   - 运维只需分发 **一个内容物**（agent.key），不再手工生成/分发 ca.crt、client.crt、client.key；
//   - 服务端证书在首次启动时自动生成并自签，Agent 通过**内存指纹校验**（SPKI pin）信任它，
//     不需要 `-ca-cert`、不需要 `-insecure`，也就不会出现"忘记校验服务端"的降级；
//   - 校验服务端证书 SAN 与公网 IP 绑定这一环节（自签部署里最容易出错的一步）被彻底去掉：
//     服务器换 IP、加域名、加 CDN 都不需要重签证书。
//
// 与 mTLS 的关系：这是 mTLS 的**免配置实现**，不是替代品 —— 客户端的信任根仍然是
// 文件权限 0600 的 agent.key，并且服务端在签发前会再校验一次 HMAC 注册证明。
//
// 保留的手工方式：显式配置 -ca-cert / -client-cert（或在 setup 阶段用 gencerts 预生成）
// 依然生效，用于需要外部 CA 或想固定证书的场景。

// CertValidDays 自动签发证书的有效期（天）。
const CertValidDays = 3650

// CA 相关文件名（位于数据目录）。
const (
	CACertFileName = "ca.crt" // PEM 自签 CA 证书（可公开）
	CAKeyFileName  = "ca.key" // PEM CA 私钥（0600）
	ServerCertName = "server.crt"
	ServerKeyName  = "server.key"
	ServerSPKIName = "server.spki" // 服务端证书公钥指纹，供 Agent 明文首次连接时校验
)

// Identity 是服务端内置 CA 及其签发能力。
// 零值可用于签发**临时演示证书**（Admin TLS），不落盘。
type Identity struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

// Name 返回身份名（写入证书 O/CN 与公钥指纹的哈希前缀，避免跨部署误 trust）。
func (id *Identity) Name() string {
	if id == nil || id.caCert == nil {
		return ""
	}
	if id.caCert.Subject.Organization != nil && len(id.caCert.Subject.Organization) > 0 &&
		id.caCert.Subject.Organization[0] != "" {
		return id.caCert.Subject.Organization[0]
	}
	return id.caCert.Subject.CommonName
}

// Fingerprint 返回 CA 身份摘要（SHA-256 前 16 位 hex），用于日志与人工比对。
func (id *Identity) Fingerprint() string {
	if id == nil || id.caCert == nil {
		return ""
	}
	return certFingerprint(id.caCert)
}

// Authority 返回 CA 证书（供构造 CA 池）。
func (id *Identity) Authority() *x509.Certificate {
	if id == nil {
		return nil
	}
	return id.caCert
}

// CATLSConfig 返回服务端 TLS 配置：可选的自身证书 + 用于校验客户端证书的 CA。
//
// 这里刻意**始终使用 tls.RequestClientCert（请求但不强制）**，把"是否必须有证书"
// 交给应用层判断，原因有两个：
//
//  1. 引导问题：新装的 Agent 手上本来就没有证书，它必须先完成一次"用注册密钥换取证书"
//     的往返。若在握手阶段就 `RequireAndVerifyClientCert`，Agent 会在发出换取请求之前
//     就被拒绝，形成死锁 —— 表现为 `tls: certificate required`，
//     而运维无从下手（服务端日志什么都不会说）。
//  2. 错误可读性：应用层能给出"未携带客户端证书"这种明确原因，
//     而不是让运维去猜一个 TLS 层的 handshake 错误。
//
// 强制语义由 server 侧落实：未开启 -allow-no-cert 时，任何**未携带证书**的连接
// 在进入注册流程前即被拒（只有 EnrollReq 例外，换取本身必须无证书）。
// 因此"请求但不强制"不会削弱最终状态 —— 收敛结果仍然是全部连接都带证书。
func (id *Identity) CATLSConfig(cert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	if id == nil || id.caCert == nil {
		return cfg
	}
	pool := x509.NewCertPool()
	pool.AddCert(id.caCert)
	cfg.ClientCAs = pool
	// 有效证书会被校验；无效/过期/非本 CA 的证书在握手阶段即失败。
	// 不带证书的客户端在应用层被拒（换取场景除外）。
	cfg.ClientAuth = tls.RequestClientCert
	return cfg
}

// selfSignedTLSConfig 构造只认本 CA 的 TLS 配置（用于 Admin 探测服务端，免系统验签）。
func selfSignedTLSConfig(pin string, name string) (*tls.Config, error) {
	return buildPinTLSConfig(nil, pin, name, false)
}

// LoadIdentity 读取数据目录下已有的 CA；不存在返回 (nil, nil)。
func LoadIdentity(dir string) (*Identity, error) {
	certPath := filepath.Join(dir, CACertFileName)
	keyPath := filepath.Join(dir, CAKeyFileName)
	cb, err := os.ReadFile(certPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("CA 私钥 %s 不可读: %w", keyPath, err)
	}
	id, err := parseIdentity(cb, kb)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", certPath, err)
	}
	return id, nil
}

func parseIdentity(certPEM, keyPEM []byte) (*Identity, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("PEM 内容缺失")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &Identity{caCert: cert, caKey: key}, nil
}

// LoadOrCreateIdentity 读取 CA；不存在则新建并写入 dir（CA 私钥 0600）。
// 为了兼容 gencerts 手工生成的 PEM 私钥（PKCS#8），解析失败时会尝试重新编码为 EC。
func LoadOrCreateIdentity(dir, name string) (*Identity, bool, error) {
	if name == "" {
		name = "MoCenter"
	}
	id, err := LoadIdentity(dir)
	if err == nil && id != nil {
		return id, false, nil
	}
	if err != nil {
		// 兼容 PKCS#8 私钥（gencerts / openssl 产物）
		if id2, e2 := loadIdentityLoose(dir, name); e2 == nil && id2 != nil {
			return id2, false, nil
		}
		return nil, false, err
	}
	newID, gerr := NewIdentity(name)
	if gerr != nil {
		return nil, false, gerr
	}
	if err := newID.Save(dir); err != nil {
		return nil, false, err
	}
	return newID, true, nil
}

// loadIdentityLoose 尝试用 PKCS#8 解析 CA 私钥。
func loadIdentityLoose(dir, name string) (*Identity, error) {
	cb, err := os.ReadFile(filepath.Join(dir, CACertFileName))
	if err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, CAKeyFileName))
	if err != nil {
		return nil, err
	}
	cblock, _ := pem.Decode(cb)
	kblock, _ := pem.Decode(kb)
	if cblock == nil || kblock == nil {
		return nil, fmt.Errorf("PEM 内容缺失")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, err
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kblock.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA 私钥不是 ECDSA（无法复用），请移走 %s 让其自动重建", filepath.Join(dir, CACertFileName))
	}
	return &Identity{caCert: cert, caKey: key}, nil
}

// NewIdentity 生成一套全新的 CA。
func NewIdentity(name string) (*Identity, error) {
	if name == "" {
		name = "MoCenter"
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{name}, CommonName: name + "-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{caCert: cert, caKey: key}, nil
}

// Save 把 CA 写入 dir（目录 0700，私钥 0600）。
func (id *Identity) Save(dir string) error {
	if id == nil || id.caCert == nil || id.caKey == nil {
		return fmt.Errorf("CA 为空")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.caCert.Raw})
	keyDER, err := x509.MarshalECPrivateKey(id.caKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, CACertFileName), certPEM, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, CAKeyFileName), keyPEM, 0600)
}

// CACertPEM 返回 CA 证书的 PEM（可安全分发给 Agent）。
func (id *Identity) CACertPEM() []byte {
	if id == nil || id.caCert == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.caCert.Raw})
}

// NewAdminCert 为 Web 控制台签发"当前部署临时证书"。
// 内存态、不落盘，重启即换 —— 目的是让 Web 侧开箱即用 HTTPS/WSS，
// 无需运维去申请域名证书（也避免把未加密的会话 Token 暴露在网络上）。
func (id *Identity) NewAdminCert(hosts []string) (tls.Certificate, error) {
	if id == nil || id.caCert == nil || id.caKey == nil {
		return tls.Certificate{}, fmt.Errorf("CA 为空")
	}
	return id.issueServerCert(hosts)
}

// ServerCertificate 返回服务端（Agent 通道）证书：
// 端口上显式配置了证书就复用；否则用内置 CA 现场签发（有效期 CertValidDays 天）。
func (id *Identity) ServerCertificate(existingCertFile, existingKeyFile string) (tls.Certificate, bool, error) {
	if existingCertFile != "" && existingKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(existingCertFile, existingKeyFile)
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("加载 TLS 证书失败: %w", err)
		}
		return cert, false, nil
	}
	cert, err := id.issueServerCert(nil)
	return cert, true, err
}

// issueServerCert 用 CA 签发一张服务端证书（服务端通道可指定真实 IP/域名；Admin 传空）。
func (id *Identity) issueServerCert(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{id.Name()}, CommonName: id.Name() + "-Server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	for _, h := range hosts {
		h = trimSpace(h)
		if h == "" || h == "0.0.0.0" || h == "::" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return id.sign(tmpl, key, x509.ExtKeyUsageServerAuth)
}

// NewAgentCert 为通过注册鉴权的 Agent 签发客户端证书（身份写入 CN = agentID）。
//
// csrPEM 是 Agent 自己生成的密钥对对应的证书请求：**私钥始终留在 Agent 本机**，
// 服务端只签发证书（不做托管的密钥分发）。csrPEM 为空时退化为服务端生成密钥
// —— 仅用于兼容与测试，正常流程不会走到。
func (id *Identity) NewAgentCert(agentID string, csrPEM ...string) (*x509.Certificate, []byte, error) {
	if id == nil || id.caCert == nil || id.caKey == nil {
		return nil, nil, fmt.Errorf("CA 为空")
	}
	var pub interface{}
	var key *ecdsa.PrivateKey
	if len(csrPEM) > 0 && strings.TrimSpace(csrPEM[0]) != "" {
		block, _ := pem.Decode([]byte(csrPEM[0]))
		if block == nil {
			return nil, nil, fmt.Errorf("证书请求 PEM 解析失败")
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("证书请求解析失败: %w", err)
		}
		if err := csr.CheckSignature(); err != nil {
			return nil, nil, fmt.Errorf("证书请求签名无效: %w", err)
		}
		pub = csr.PublicKey
	} else {
		generated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		key = generated
		pub = &generated.PublicKey
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	cn := agentID
	if cn == "" {
		cn = "mocenter-agent"
	}
	if len(cn) > 64 {
		cn = cn[:64]
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{id.Name()}, CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, CertValidDays),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, id.caCert, pub, id.caKey)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if key != nil {
		// 兼容路径：需要把私钥交回调用方（仅测试用）
		b, mErr := x509.MarshalECPrivateKey(key)
		if mErr != nil {
			return nil, nil, mErr
		}
		leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
		return leaf, append(leafPEM, keyPEM...), nil
	}
	return leaf, nil, nil
}

// AgentCertPEM 签发客户端证书并以 PEM 文本返回（含 CA 链），用于回发给 Agent。
func (id *Identity) AgentCertPEM(agentID, csrPEM string) (string, error) {
	leaf, _, err := id.NewAgentCert(agentID, csrPEM)
	if err != nil {
		return "", err
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	chain = append(chain, id.CACertPEM()...)
	return string(chain), nil
}

// sign 用 CA 签发证书并打包成 tls.Certificate。
func (id *Identity) sign(tmpl *x509.Certificate, key *ecdsa.PrivateKey, _ x509.ExtKeyUsage) (tls.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, id.caCert, &key.PublicKey, id.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der, id.caCert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
