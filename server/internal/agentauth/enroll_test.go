package agentauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// csrPEMFor 生成一份自持私钥的证书请求，模拟 Agent 侧行为。
func csrPEMFor(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), key
}

// TestAgentCertPEMBindsToClientPublicKey 是本方案的核心断言：
// 签发的证书必须绑定在**客户端提交的公钥**上（私钥不下发、不托管）。
func TestAgentCertPEMBindsToClientPublicKey(t *testing.T) {
	id, _ := NewIdentity("Acme")
	csr, key := csrPEMFor(t)
	chain, err := id.AgentCertPEM("host-01", csr)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(chain))
	if block == nil {
		t.Fatal("回发的证书链不是合法 PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "host-01" {
		t.Fatalf("CN 应为 agent_id，得到 %q", leaf.Subject.CommonName)
	}
	// 公钥必须与客户端私钥匹配
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("公钥类型不是 ECDSA")
	}
	if pub.X.Cmp(key.PublicKey.X) != 0 || pub.Y.Cmp(key.PublicKey.Y) != 0 {
		t.Fatal("证书公钥与 Agent 本地私钥不匹配（会导致 private key does not match public key）")
	}
	// 链可由本 CA 校验，且仅用于 clientAuth
	pool := x509.NewCertPool()
	pool.AddCert(id.Authority())
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("签发结果应能通过本 CA 校验: %v", err)
	}
}

func TestAgentCertPEMRejectsTamperedCSR(t *testing.T) {
	id, _ := NewIdentity("Acme")
	csr, _ := csrPEMFor(t)
	// 篡改 CSR 的 base64 内容：签名校验必须失败，不能"照签不误"
	tampered := csr[:len(csr)-40] + "AAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"
	if _, err := id.AgentCertPEM("host-01", tampered); err == nil {
		t.Fatal("被篡改的证书请求应被拒绝")
	}
}

func TestAgentCertPEMRejectsGarbage(t *testing.T) {
	id, _ := NewIdentity("Acme")
	if _, err := id.AgentCertPEM("host-01", "this is not a csr"); err == nil {
		t.Fatal("非法 CSR 应被拒绝")
	}
}

// TestIdentitySharedAcrossCerts 确认服务端证书与客户端证书出自同一 CA
// —— 否则 Agent 换取后仍旧无法校验服务端（就是"换证成功但仍连不上"的形态）。
func TestIdentitySharedAcrossCerts(t *testing.T) {
	id, _ := NewIdentity("Acme")
	csr, _ := csrPEMFor(t)
	chain, err := id.AgentCertPEM("host-01", csr)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(chain)) {
		t.Fatal("证书链解析失败")
	}
	if !pool.AppendCertsFromPEM(id.CACertPEM()) {
		t.Fatal("CA 解析失败")
	}
	serverCert, _, err := id.ServerCertificate("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverCert.Leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("服务端证书应能由同一 CA 校验: %v", err)
	}
}
