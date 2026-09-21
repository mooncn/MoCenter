package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCA 生成一套自签 CA，返回 (PEM, 公钥指纹)。
// 指纹口径与服务端一致（SPKI 前 16 位 hex），因此可以直接互相比对。
func makeCA(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	caPEM, fp, _ := makeCAFull(t, name)
	return caPEM, fp
}

// makeCAWithKey 同 makeCA，但额外登记私钥，供端到端测试用同一套 CA 签发证书。
func makeCAWithKey(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	caPEM, fp, _ := makeCAFull(t, name)
	return caPEM, fp
}

func makeCAFull(t *testing.T, name string) ([]byte, string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{Organization: []string{name}, CommonName: name + "-CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	fp := certPublicFingerprint(cert)
	caKeysMu.Lock()
	caKeys[fp] = key
	caKeysMu.Unlock()
	return pemText, fp, key
}

// writeCAInCredDir 把 CA 写进 creds/ca.crt（模拟"本机记下的服务端身份"）。
func writeCAInCredDir(t *testing.T, dir string, caPEM []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCAFingerprintFromPEM(t *testing.T) {
	caPEM, want := makeCA(t, "unit")
	if got := caFingerprintFromPEM(caPEM); got != want {
		t.Fatalf("指纹不一致: got %s want %s", got, want)
	}
	if got := caFingerprintFromPEM([]byte("not a pem")); got != "" {
		t.Fatalf("非法 PEM 应返回空指纹，得到 %q", got)
	}
}

// TestVerifyEnrollServerFirstTime 首次接入：本机没有任何 CA 记录，信任引导放行。
// 信任根是注册密钥（HMAC），不是服务端证书 —— 这条路径必须能走通，否则新机永远接不进来。
func TestVerifyEnrollServerFirstTime(t *testing.T) {
	dir := t.TempDir()
	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k"}}
	_, serverFP := makeCA(t, "new")
	if err := c.verifyEnrollServer(serverFP, serverFP); err != nil {
		t.Fatalf("首次接入应放行: %v", err)
	}
}

// TestVerifyEnrollServerSameCA 同一套总控（含"只换了服务端证书、CA 未变"）应正常换取。
func TestVerifyEnrollServerSameCA(t *testing.T) {
	dir := t.TempDir()
	caPEM, fp := makeCA(t, "same")
	writeCAInCredDir(t, dir, caPEM)
	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k"}}
	if err := c.verifyEnrollServer(fp, fp); err != nil {
		t.Fatalf("同一 CA 应放行: %v", err)
	}
}

// TestVerifyEnrollServerRejectsCAChangeWithoutAuthorization 回归本次线上故障的核心决策：
// 服务端换过 CA 之后，**默认不自动信任**新身份。
//
// 原因：换取请求里带着注册密钥的 HMAC 证明。"无条件重换"等价于
// "谁能让 Agent 连上我，谁就能拿到它的注册凭据"，证书锁定会被彻底架空。
// 因此自动恢复必须由运维显式授权（server_fingerprint / ca-cert），否则明确拒绝并给出指引。
func TestVerifyEnrollServerRejectsCAChangeWithoutAuthorization(t *testing.T) {
	dir := t.TempDir()
	oldPEM, oldFP := makeCA(t, "old")
	writeCAInCredDir(t, dir, oldPEM)
	_, newFP := makeCA(t, "new")

	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k"}}
	err := c.verifyEnrollServer(newFP, newFP)
	if err == nil {
		t.Fatal("服务端身份变化且未授权时必须拒绝")
	}
	var te *trustError
	if !errors.As(err, &te) {
		t.Fatalf("应返回 trustError，得到 %T: %v", err, err)
	}
	// 错误里要同时给出新旧指纹和办法，否则运维只知道"失败了"
	msg := err.Error()
	for _, want := range []string{DisplayFingerprint(oldFP), DisplayFingerprint(newFP), "server_fingerprint"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，实际: %s", want, msg)
		}
	}
}

// TestVerifyEnrollServerAcceptsCAChangeWithPin 运维已授权（server_fingerprint = 新指纹）时，
// 换 CA 可以自动收敛：这正是"重装服务端后不必登录每台被控机"的路径。
func TestVerifyEnrollServerAcceptsCAChangeWithPin(t *testing.T) {
	dir := t.TempDir()
	oldPEM, _ := makeCA(t, "old")
	writeCAInCredDir(t, dir, oldPEM)
	_, newFP := makeCA(t, "new")

	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k", ServerFingerprint: newFP}}
	if err := c.verifyEnrollServer(newFP, newFP); err != nil {
		t.Fatalf("已授权的新 CA 应放行: %v", err)
	}
}

// TestVerifyEnrollServerPinMismatch 显式 pin 时，服务端指纹不符必须拒绝 ——
// 否则 pin 就形同虚设，"记住这台总控"的承诺不成立。
func TestVerifyEnrollServerPinMismatch(t *testing.T) {
	dir := t.TempDir()
	_, pinnedFP := makeCA(t, "pinned")
	_, otherFP := makeCA(t, "other")
	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k", ServerFingerprint: pinnedFP}}
	if err := c.verifyEnrollServer(otherFP, otherFP); err == nil {
		t.Fatal("指纹与 pin 不符时必须拒绝")
	}
}

// TestVerifyEnrollServerNoFingerprintWhenHistoryExists 有历史但服务端没给身份信息时拒绝：
// 无法判断是否换了 CA，就不该继续。
func TestVerifyEnrollServerNoFingerprintWhenHistoryExists(t *testing.T) {
	dir := t.TempDir()
	caPEM, _ := makeCA(t, "old")
	writeCAInCredDir(t, dir, caPEM)
	c := &Client{cfg: Config{CredDir: dir, EnrollmentSecret: "k"}}
	if err := c.verifyEnrollServer("", ""); err == nil {
		t.Fatal("有历史身份但服务端未提供指纹时应拒绝")
	}
}

// TestKnownServerFingerprints 指纹来源：配置的 pin、creds/ca.crt、-ca-cert 文件。
func TestKnownServerFingerprints(t *testing.T) {
	dir := t.TempDir()
	credPEM, credFP := makeCA(t, "cred")
	writeCAInCredDir(t, dir, credPEM)

	caFile := filepath.Join(dir, "external.crt")
	extPEM, extFP := makeCA(t, "ext")
	if err := os.WriteFile(caFile, extPEM, 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: Config{
		CredDir: dir, CACert: caFile,
		ServerFingerprint: "AB:cd 12 34 56 78 90 ef",
	}}
	pinned, known := c.knownServerFingerprints()
	if len(pinned) != 1 || pinned[0] != "abcd1234567890ef" {
		t.Fatalf("pin 指纹归一化有误: %v", pinned)
	}
	seen := map[string]bool{}
	for _, fp := range known {
		seen[fp] = true
	}
	if !seen[credFP] || !seen[extFP] {
		t.Fatalf("应同时收录 creds/ca.crt 与 -ca-cert 的指纹，实际 %v", known)
	}
}

// TestDiscardServerIssuedCreds 自动恢复要能安全地删掉旧证书：证书与旧 CA 清掉，
// 私钥与无关文件必须留着（私钥一丢，服务端 TOFU 绑定就把重签路径挡死了）。
func TestDiscardServerIssuedCreds(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"client.crt", "client.key", "ca.crt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{cfg: Config{CredDir: dir}}
	if err := c.discardServerIssuedCreds(); err != nil {
		t.Fatal(err)
	}
	if c.credCAFingerprint() != "" || c.clientCert() != nil {
		t.Fatal("旧证书应已被删除")
	}
	if readFileOrEmpty(dir, "client.key") != "x" {
		t.Fatal("client.key 必须保留，否则重签时公钥指纹变化会被 TOFU 拒发")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("不应删除无关文件")
	}
}

// catTestPair 一套 CA + 用它的公钥指纹签出的"服务端链"（[leaf, CA]），用于 pin 测试。
type catTestPair struct {
	caFP     string
	chainDER [][]byte
}

// makeCATestPair 造一套 CA 与由它签发的服务端链，并返回 CA 的公钥指纹。
func makeCATestPair(t *testing.T, name string) (catTestPair, string) {
	t.Helper()
	caPEM, fp, caKey := makeCAFull(t, name)
	caCert := parseFirstCert(caPEM)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name + "-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return catTestPair{caFP: fp, chainDER: [][]byte{der, caCert.Raw}}, fp
}
