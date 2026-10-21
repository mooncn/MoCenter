package agentauth

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if len(fp) != 64 {
		t.Fatalf("指纹应为完整 SHA-256（64 位 hex），得到 %d (%q)", len(fp), fp)
	}
	fi, err := os.Stat(filepath.Join(dir, CAKeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	// 私钥权限须收紧；NTFS 不看 mode 位（收紧靠 ACL，见 H-9），故只在类 Unix 上断言。
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
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

// 指纹比较：完整值必须全等；历史 16 位短指纹按前缀兼容，更短的值一律不认。
func TestFingerprintMatching(t *testing.T) {
	id, _ := NewIdentity("Acme")
	fp := id.Fingerprint()
	if !MatchFingerprint(strings.ToUpper(fp), fp) {
		t.Fatal("大小写不同应视为同一指纹")
	}
	if !MatchFingerprint(fp[:legacyFingerprintLen], fp) {
		t.Fatal("历史 16 位短指纹应按前缀匹配")
	}
	if MatchFingerprint(fp[:8], fp) {
		t.Fatal("8 位短指纹熵不足，不应再被接受")
	}
	// 首字符可能本来就是 0，换一个确定不同的字符，避免偶发假失败。
	different := "0" + fp[1:]
	if different == fp {
		different = "1" + fp[1:]
	}
	if MatchFingerprint(different, fp) {
		t.Fatal("内容不同不应匹配")
	}
	if MatchFingerprint("", fp) || MatchFingerprint(fp, "") {
		t.Fatal("空指纹不应匹配")
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

// TestServerCertIncludesRequestedHosts 守住「自动签发的服务端证书要带上本机真实地址」。
//
// 这是线上最常踩的坑：服务端证书只含 127.0.0.1 时，公网部署的 Agent 一连接就报
//
//	x509: certificate is valid for 127.0.0.1, ::1, not <公网IP>
//
// 修复方式是把探测到的地址传进 ServerCertificate（此前调用点传的是空列表）。
func TestServerCertIncludesRequestedHosts(t *testing.T) {
	id, _ := NewIdentity("Acme")
	cert, auto, err := id.ServerCertificate("", "",
		"82.156.7.85", "10.0.0.5", "mocenter.example.com", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if !auto {
		t.Fatal("未提供证书文件时应自动签发")
	}

	ips := map[string]bool{}
	for _, ip := range cert.Leaf.IPAddresses {
		ips[ip.String()] = true
	}
	dnss := map[string]bool{}
	for _, d := range cert.Leaf.DNSNames {
		dnss[d] = true
	}

	for _, want := range []string{"82.156.7.85", "10.0.0.5", "127.0.0.1"} {
		if !ips[want] {
			t.Errorf("SAN 缺少 IP %s（实际 %v）", want, ips)
		}
	}
	if !dnss["mocenter.example.com"] {
		t.Errorf("SAN 缺少域名 mocenter.example.com（实际 %v）", dnss)
	}
	// 通配监听地址不是可用于校验的地址，始终不该出现
	if ips["0.0.0.0"] {
		t.Error("0.0.0.0 不应写入 SAN")
	}
}

// TestServerCertSANIsDeduplicated 去重：本地网卡地址常与传入地址重叠，
// 重复项会让每份证书白白变大，也让 SAN 日志更难读。
func TestServerCertSANIsDeduplicated(t *testing.T) {
	id, _ := NewIdentity("Acme")
	cert, _, err := id.ServerCertificate("", "",
		"127.0.0.1", "127.0.0.1", "localhost", "localhost", "::1")
	if err != nil {
		t.Fatal(err)
	}
	seenIP := map[string]int{}
	for _, ip := range cert.Leaf.IPAddresses {
		seenIP[ip.String()]++
	}
	seenDNS := map[string]int{}
	for _, d := range cert.Leaf.DNSNames {
		seenDNS[d]++
	}
	for ip, n := range seenIP {
		if n > 1 {
			t.Errorf("SAN 中 IP %s 重复出现 %d 次", ip, n)
		}
	}
	for d, n := range seenDNS {
		if n > 1 {
			t.Errorf("SAN 中域名 %s 重复出现 %d 次", d, n)
		}
	}
}

// TestServerCertIgnoresWildcardListenAddr 传入的通配地址必须被忽略，
// 否则证书里出现 "0.0.0.0" 会让人误以为已覆盖所有地址。
func TestServerCertIgnoresWildcardListenAddr(t *testing.T) {
	id, _ := NewIdentity("Acme")
	cert, _, err := id.ServerCertificate("", "", "0.0.0.0", "::", "  ", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range cert.Leaf.IPAddresses {
		switch ip.String() {
		case "0.0.0.0", "::":
			t.Errorf("通配地址 %s 不应写入 SAN", ip)
		}
	}
	found := false
	for _, ip := range cert.Leaf.IPAddresses {
		if ip.String() == "1.2.3.4" {
			found = true
		}
	}
	if !found {
		t.Error("合法地址 1.2.3.4 应写入 SAN")
	}
}
