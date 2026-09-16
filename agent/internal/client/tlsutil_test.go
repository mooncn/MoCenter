package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormFingerprint(t *testing.T) {
	cases := map[string]string{
		"AB:cd 12 34": "abcd1234",
		"abcd1234":    "abcd1234",
	}
	for in, want := range cases {
		if got := normFingerprint(in); got != want {
			t.Errorf("normFingerprint(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestBaseTLSConfigInsecure(t *testing.T) {
	c := &Client{cfg: Config{Insecure: true}}
	cfg, err := c.baseTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("Insecure=true 时应跳过校验")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatal("Insecure=true 时不应设置指纹回调")
	}
}

func TestBaseTLSConfigFingerprintPins(t *testing.T) {
	c := &Client{cfg: Config{ServerFingerprint: "AB:cd 12 34 56 78 90 ef"}}
	cfg, err := c.baseTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("配置指纹时应跳过标准链校验，由回调自行校验")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("配置指纹时必须设置校验回调，否则等于完全不校验")
	}
}

func TestBaseTLSConfigRejectsBrokenCA(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(bad, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: Config{CACert: bad}}
	cfg, err := c.baseTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs != nil {
		t.Fatal("无法解析的 CA 文件不应被设为 RootCAs")
	}
}

func TestBaseTLSConfigMinVersion(t *testing.T) {
	c := &Client{cfg: Config{}}
	cfg, err := c.baseTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS 最低版本应为 1.2，得到 %v", cfg.MinVersion)
	}
	if cfg.VerifyConnection != nil {
		t.Fatal("没有 CA 时不应设置自定义校验")
	}
}

func TestCredDirDefaults(t *testing.T) {
	c := &Client{cfg: Config{ConfigPath: "/etc/mocenter/agent.json"}}
	if got := c.credDir(); got != "/etc/mocenter/creds" {
		t.Fatalf("凭证目录应在配置目录下: %s", got)
	}
	c2 := &Client{cfg: Config{ConfigPath: "/etc/mocenter/agent.json", CredDir: "/tmp/x"}}
	if got := c2.credDir(); got != "/tmp/x" {
		t.Fatalf("显式 CredDir 应优先: %s", got)
	}
}

// TestNoPlaintextFallbackWhenCertHeld 回归：本机已持有客户端证书时，
// TLS 校验失败**不允许**静默降级为明文 —— 否则"双向认证"会退化成"什么都没认证"。
//
// 这条在实机联调时踩到过：服务端换了 CA，Agent 因为校验失败改走明文，
// 结果服务端的 require-agent-cert 完全没起作用，日志里只有一行不起眼的提示。
func TestNoPlaintextFallbackWhenCertHeld(t *testing.T) {
	dir := t.TempDir()
	writeDummyClientCert(t, dir)
	c := &Client{cfg: Config{
		ServerAddr:        "127.0.0.1:1", // 必然连不上，触发 TLS 失败分支
		CredDir:           dir,
		ServerFingerprint: "0000000000000000", // 校验必定失败
	}}
	if c.clientCert() == nil {
		t.Fatal("测试前置：应能加载到客户端证书")
	}
	err := c.connectAndServe()
	if err == nil {
		t.Fatal("已持证时 TLS 失败不应被静默降级")
	}
	if !strings.Contains(err.Error(), "拒绝降级为明文") {
		t.Fatalf("错误信息应说明拒绝降级，实际: %v", err)
	}
}

// writeDummyClientCert 写出一对可被 tls.LoadX509KeyPair 接受的自签证书，
// 使 clientCert() 返回非 nil（证书内容本身不参与后续校验）。
func writeDummyClientCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "unit-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.key"),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
}
