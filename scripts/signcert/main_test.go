package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCA 生成一对临时 CA 证书/私钥并写入 dir，返回解析后的对象。
func testCA(t *testing.T, dir string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "MoCenter-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, caCertName), certPEM(der), 0600); err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(filepath.Join(dir, caKeyName),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return caCert, key
}

// parseCert 读取 PEM 证书。
func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s 不是合法 PEM", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSignServerSAN 验证签发的服务端证书 SAN 精确包含给定 IP 与域名 ——
// 这是本工具存在的意义：SAN 少一个地址，Agent 就会连不上。
func TestSignServerSAN(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := testCA(t, dir)

	ips := []string{"82.156.7.85", "10.0.0.5", "127.0.0.1", "::1"}
	dnss := []string{"mocenter.example.com", "mocenter-server"}
	if err := signServer(filepath.Join(dir, srvCertName), filepath.Join(dir, srvKeyName),
		caCert, caKey, ips, dnss); err != nil {
		t.Fatalf("signServer: %v", err)
	}

	cert := parseCert(t, filepath.Join(dir, srvCertName))
	for _, ip := range ips {
		want := net.ParseIP(ip)
		found := false
		for _, got := range cert.IPAddresses {
			if got.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SAN 缺少 IP %s（实际 %v）", ip, cert.IPAddresses)
		}
	}
	for _, d := range dnss {
		found := false
		for _, got := range cert.DNSNames {
			if got == d {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SAN 缺少 DNS %s（实际 %v）", d, cert.DNSNames)
		}
	}

	// 必须是服务端证书（ExtKeyUsage = ServerAuth），否则浏览器/Agent 会拒绝
	var hasServerAuth bool
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
	}
	if !hasServerAuth {
		t.Error("服务端证书缺少 ExtKeyUsage=ServerAuth")
	}

	// 必须由该 CA 签发（重签后 Agent 侧无需换 ca.crt 的前提）
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		t.Errorf("证书不是由该 CA 签发: %v", err)
	}
}

// TestSignClientExtKeyUsage 客户端证书必须是 ClientAuth，用于 mTLS。
func TestSignClientExtKeyUsage(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := testCA(t, dir)
	if err := signClient(filepath.Join(dir, clntCertName), filepath.Join(dir, clntKeyName),
		caCert, caKey); err != nil {
		t.Fatalf("signClient: %v", err)
	}
	cert := parseCert(t, filepath.Join(dir, clntCertName))
	var hasClientAuth bool
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		t.Error("客户端证书缺少 ExtKeyUsage=ClientAuth")
	}
}

// TestResignKeepsCA 验证「重签服务端证书」不会换 CA ——
// 这是无需给所有 Agent 重发客户端证书的关键。
func TestResignKeepsCA(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := testCA(t, dir)
	before := sha256Hex(caCert.Raw)

	// 模拟换 IP 后重签两次
	for _, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		if err := signServer(filepath.Join(dir, srvCertName), filepath.Join(dir, srvKeyName),
			caCert, caKey, []string{ip}, nil); err != nil {
			t.Fatalf("signServer(%s): %v", ip, err)
		}
		got := parseCert(t, filepath.Join(dir, srvCertName))
		if len(got.IPAddresses) != 1 || got.IPAddresses[0].String() != ip {
			t.Errorf("重签后 SAN 应为 [%s]，实际 %v", ip, got.IPAddresses)
		}
		if err := got.CheckSignatureFrom(caCert); err != nil {
			t.Errorf("重签后证书 CA 校验失败: %v", err)
		}
	}

	after, err := os.ReadFile(filepath.Join(dir, caCertName))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(after)
	caAfter, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(caAfter.Raw) != before {
		t.Error("重签服务端证书不应改变 CA")
	}
}

// TestDedupeAndParseIPs 覆盖输入清洗：去重、非法 IP 剔除。
func TestDedupeAndParseIPs(t *testing.T) {
	got := dedupe([]string{"1.1.1.1", "1.1.1.1", " 2.2.2.2 ", "", "not-an-ip"}, true)
	want := map[string]bool{"1.1.1.1": true, "2.2.2.2": true}
	if len(got) != len(want) {
		t.Fatalf("dedupe 结果 %v，期望 %d 项", got, len(want))
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("dedupe 结果含非预期项 %q", s)
		}
	}
	// 非法 IP 不应进入 SAN
	if ips := parseIPs([]string{"1.1.1.1", "junk"}); len(ips) != 1 {
		t.Errorf("parseIPs 应剔除非法值，实际 %v", ips)
	}
}

// TestSplitList 覆盖逗号/分号/空格混用。
func TestSplitList(t *testing.T) {
	got := splitList("a.com, b.com;c.com ,, ")
	if len(got) != 3 {
		t.Fatalf("splitList 得到 %v，期望 3 项", got)
	}
}

// TestParsePublicIP 回显服务响应解析：JSON 与纯文本，且必须滤掉私网/回环。
func TestParsePublicIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"82.156.7.85", "82.156.7.85"},
		{" 82.156.7.85\n", "82.156.7.85"},
		{`"82.156.7.85"`, "82.156.7.85"},
		{"127.0.0.1", ""},   // 回环
		{"10.0.0.5", ""},    // 私网
		{"192.168.1.1", ""}, // 私网
		{"not-an-ip", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parsePublicIP(c.in); got != c.want {
			t.Errorf("parsePublicIP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadSaveJSON 配置写回不能丢字段（服务端配置里还有 agent_key 等）。
func TestLoadSaveJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mocenter-server.json")
	original := map[string]any{"listen": ":9000", "agent_key": "secret", "user": "admin"}
	if err := saveJSON(path, original); err != nil {
		t.Fatal(err)
	}
	loaded := loadJSON(path)
	if loaded["agent_key"] != "secret" || loaded["user"] != "admin" {
		t.Fatalf("写回后原有字段丢失: %v", loaded)
	}
	loaded["tls_cert"] = "/x/server.crt"
	if err := saveJSON(path, loaded); err != nil {
		t.Fatal(err)
	}
	final := loadJSON(path)
	if final["agent_key"] != "secret" || final["tls_cert"] != "/x/server.crt" {
		t.Fatalf("追加字段后原有字段丢失: %v", final)
	}
	// 配置文件含敏感字段，权限必须收紧
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("配置文件权限 %o，期望 0600", perm)
	}
}
