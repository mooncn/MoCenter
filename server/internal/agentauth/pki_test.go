package agentauth

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	id, created, err := LoadOrCreateIdentity(dir, "Acme")
	if err != nil || !created {
		t.Fatalf("首次加载应生成 CA: created=%v err=%v", created, err)
	}
	if id.Name() != "Acme" {
		t.Fatalf("身份名应为 Acme，得到 %q", id.Name())
	}
	fp := id.Fingerprint()
	if len(fp) != 16 {
		t.Fatalf("指纹长度应为 16，得到 %d (%q)", len(fp), fp)
	}
	// 私钥权限必须收紧
	fi, err := os.Stat(filepath.Join(dir, CAKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("CA 私钥权限应为 0600，得到 %v", fi.Mode().Perm())
	}
	// 再次加载应复用同一 CA（指纹不变），否则所有 Agent 的 pin 会失效
	id2, created2, err := LoadOrCreateIdentity(dir, "Acme")
	if err != nil || created2 {
		t.Fatalf("第二次加载不应重建: created=%v err=%v", created2, err)
	}
	if id2.Fingerprint() != fp {
		t.Fatalf("重新加载后指纹变化: %s -> %s", fp, id2.Fingerprint())
	}
}

func TestAgentCertChainsAndIdentity(t *testing.T) {
	id, err := NewIdentity("Acme")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _, err := id.NewAgentCert("host-01")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "host-01" {
		t.Fatalf("CN 应为 agent_id，得到 %q", leaf.Subject.CommonName)
	}
	pool := x509.NewCertPool()
	pool.AddCert(id.Authority())
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("客户端证书应能由本 CA 验证（clientAuth）: %v", err)
	}
	// 两张证书的公钥不同（每次现签，私钥在 Agent 本地唯一）
	leaf2, _, _ := id.NewAgentCert("host-01")
	if string(leaf.RawSubjectPublicKeyInfo) == string(leaf2.RawSubjectPublicKeyInfo) {
		t.Fatal("两次签发的证书不应复用同一密钥对")
	}
}

func TestServerCertSkipsUnusableSAN(t *testing.T) {
	id, _ := NewIdentity("Acme")
	cert, auto, err := id.ServerCertificate("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !auto {
		t.Fatal("未提供证书文件时应自动签发")
	}
	names := map[string]bool{}
	for _, d := range cert.Leaf.DNSNames {
		names[d] = true
	}
	for _, ip := range cert.Leaf.IPAddresses {
		names[ip.String()] = true
	}
	if !names["127.0.0.1"] {
		t.Fatalf("服务端证书应至少包含 127.0.0.1，实际 %v", names)
	}
	if names["0.0.0.0"] {
		t.Fatal("0.0.0.0 不应写入 SAN")
	}
}

func TestLoadIdentityRejectsNonECDSA(t *testing.T) {
	dir := t.TempDir()
	id, _ := NewIdentity("Acme")
	if err := id.Save(dir); err != nil {
		t.Fatal(err)
	}
	// 写入一个非法私钥：应报错而不是静默生成新 CA（静默换 CA 会让所有 Agent 掉线）
	if err := os.WriteFile(filepath.Join(dir, CAKeyFileName), []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("CA 私钥损坏时应报错")
	}
}

func TestParseCertificatePEM(t *testing.T) {
	id, _ := NewIdentity("Acme")
	certPEM, err := id.AgentCertPEM("host-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePEMFirstCertificate([]byte(certPEM)); err != nil {
		t.Fatalf("PEM 链解析失败: %v", err)
	}
}
