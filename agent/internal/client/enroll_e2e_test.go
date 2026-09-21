package client

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mocenter/agent/internal/agentauth"
	"mocenter/agent/internal/protocol"
)

// 本文件是"服务端换过 CA"这条故障链路的端到端回归测试：
// 用一个**真的 TLS 监听**（自带 CA、能签发客户端证书）替代服务端，
// 覆盖 Agent 从"拿着旧证书连不上"到"自动重新换取并连上"的全过程。
// 复现的正是线上那段日志：
//
//	agent:  tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, ::1, not 192.168.117.1
//	server: [server] Agent 连接未完成注册，关闭: 192.168.117.134:57758

// fakeServer 模拟服务端的证书签发与 TLS 监听。
type fakeServer struct {
	t     *testing.T
	caPEM []byte
	caFP  string
	ln    net.Listener

	mu       sync.Mutex
	enrolls  int      // 收到的换取请求次数
	online   int      // 收到的上线消息次数
	rejected int      // 因客户端证书非本套 CA 签发而拒绝连接的次数
	peerCNs  []string // 服务端实际看到的客户端证书 CN（用于断言"证书有没有被发上来"）
}

// newFakeServer 生成一套新 CA 并监听 127.0.0.1。
func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	caPEM, caFP := makeCAWithKey(t, "fake")

	caBlock, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// CA 私钥：测试里直接重新生成一套等价密钥对不现实，改为解析 makeCA 用的密钥
	// —— 因此这里让 makeCA 之外的地方也能签发：从 cert 反推不可行，
	// 故用一个辅助函数重新构建"证书+私钥"对。
	caKey := caKeys[caFP]
	if caKey == nil {
		t.Fatal("测试辅助：makeCA 未登记 CA 私钥")
	}

	// 发送完整链（叶子 + CA）：真实服务端也是这样（见 agentauth.sign），
	// 因此 Agent 才能用稳定不变的"部署身份 CA 指纹"作为 pin。
	serverCert := issueCert(t, caCert, caKey, "fake-server", true)
	serverCert.Certificate = append(serverCert.Certificate, caCert.Raw)
	cfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
		ClientCAs:    poolOf(caCert),
		// 与服务端一致：请求但不强制客户端证书（新 Agent 要先换取）
		ClientAuth: tls.RequestClientCert,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, caPEM: caPEM, caFP: caFP, ln: ln}
	go s.serve(caCert, caKey)
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) counts() (enrolls, online int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enrolls, s.online
}

func (s *fakeServer) rejects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected
}

func (s *fakeServer) serve(caCert *x509.Certificate, caKey *ecdsa.PrivateKey) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			// 与服务端 handleAgentConn 一致：**显式验链**，不是只看有没有证书。
			// 这样"换过 CA 后旧证书被拒"的场景才会在测试里真实发生。
			if tc, ok := c.(*tls.Conn); ok {
				st := tc.ConnectionState()
				s.mu.Lock()
				if len(st.PeerCertificates) > 0 {
					s.peerCNs = append(s.peerCNs, st.PeerCertificates[0].Subject.CommonName)
				} else {
					s.peerCNs = append(s.peerCNs, "<none>")
				}
				s.mu.Unlock()
				if len(st.PeerCertificates) > 0 {
					pool := poolOf(caCert)
					if _, verr := st.PeerCertificates[0].Verify(x509.VerifyOptions{
						Roots:     pool,
						KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
					}); verr != nil {
						s.mu.Lock()
						s.rejected++
						s.mu.Unlock()
						// 与服务端一致：主动告知原因再关闭，客户端才能自动重新换取
						b, _ := json.Marshal(protocol.EnrollRequiredResp{
							Reason: "客户端证书不被本套 CA 认可", Identity: "fake", Fingerprint: s.caFP,
						})
						_ = json.NewEncoder(c).Encode(protocol.Envelope{
							Type: protocol.TypeEnrollRequired, Data: b,
						})
						return
					}
				}
			}
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
			for sc.Scan() {
				var env protocol.Envelope
				if json.Unmarshal(sc.Bytes(), &env) != nil {
					continue
				}
				switch env.Type {
				case protocol.TypeEnrollReq:
					var req protocol.EnrollReq
					_ = json.Unmarshal(env.Data, &req)
					s.mu.Lock()
					s.enrolls++
					s.mu.Unlock()
					resp := protocol.EnrollResp{}
					// 与服务端一致：校验 HMAC 证明后才签发
					if agentauth.Proof("secret", req.AgentID, req.Nonce, req.TS) != req.Proof {
						resp.Message = "注册凭证校验失败"
					} else {
						resp.OK = true
						resp.Message = "证书已签发"
						resp.CertPEM = certPEMOf(s.t, issueClientCert(s.t, caCert, caKey, req.CSR, req.AgentID))
						resp.CAPEM = string(s.caPEM)
						resp.ServerFingerprint = s.caFP
						resp.Identity = "fake"
					}
					b, _ := json.Marshal(resp)
					_ = json.NewEncoder(c).Encode(protocol.Envelope{Type: protocol.TypeEnrollResp, Data: b})
					if !resp.OK {
						return
					}
				case protocol.TypeOnline:
					s.mu.Lock()
					s.online++
					s.mu.Unlock()
					return // 上线即结束（测试只关心能否注册成功）
				}
			}
		}(conn)
	}
}

var (
	caKeysMu sync.Mutex
	caKeys   = map[string]*ecdsa.PrivateKey{}
)

// readFileOrEmpty 读取文件内容（不存在返回空串），用于断言"没被改写"。
func readFileOrEmpty(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func issueCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func issueClientCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, csrPEM, cn string) tls.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("CSR 解析失败")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, csr.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: nil}
}

func certPEMOf(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return string(out)
}

func poolOf(ca *x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca)
	return p
}

// 测试期间屏蔽 Agent 的正常日志噪声。
func init() { log.SetOutput(io.Discard) }

// TestRecoverFromRotatedCA 端到端主回归：
//
//  1. 用 CA-A 完成首次换取（creds 里留下 CA-A 的 ca.crt 与客户端证书）；
//  2. 服务端换成 CA-B（模拟重装/换机器）；
//  3. 运维把 CA-B 的指纹填进 server_fingerprint（授权）后，
//     Agent 应自动丢弃旧证书、重新换取、完成注册 —— 无需登录被控机。
func TestRecoverFromRotatedCA(t *testing.T) {
	dir := t.TempDir()
	secret := "secret"

	// --- 步骤 1：首套 CA 下完成换取 ---
	srvA := newFakeServer(t)
	c := New(Config{
		ServerAddr: srvA.addr(), AgentID: "host-01",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: secret,
	})
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		t.Fatalf("首次换取应成功: %v", err)
	}
	if c.clientCert() == nil {
		t.Fatal("换取后应持有客户端证书")
	}
	if fp := c.credCAFingerprint(); fp != srvA.caFP {
		t.Fatalf("creds/ca.crt 应是 CA-A 的指纹: %s != %s", fp, srvA.caFP)
	}
	keyAfterFirst := readFileOrEmpty(dir, "client.key")
	if keyAfterFirst == "" {
		t.Fatal("首次换取后应落下客户端私钥")
	}

	// --- 步骤 2：服务端换 CA（CA-B），Agent 手上仍是 CA-A 签发的证书 ---
	srvB := newFakeServer(t)
	if srvB.caFP == srvA.caFP {
		t.Fatal("测试前置：两次 CA 指纹不应相同")
	}
	// 模拟真实配置：换取后 enrollment_secret 被清空，agent_key 保留
	c.cfg.EnrollmentSecret = ""
	c.cfg.AgentKey = secret
	c.cfg.ServerAddr = srvB.addr()

	// --- 步骤 3：运维授权（填上新指纹）后自动收敛 ---
	c.cfg.ServerFingerprint = srvB.caFP
	// 第一轮会返回 errReenrolled（"已重新换取，重连即可"），这正是自动恢复的体现；
	// 真实 Agent 由 Run 循环立刻重连，这里显式再连一次以验证最终结果。
	if err := c.connectAndServe(); err != nil && !isReenroll(err) {
		t.Fatalf("授权后应能自动恢复（首次应返回需重连）: %v", err)
	}
	if err := c.connectAndServe(); err != nil && !isReenroll(err) {
		t.Fatalf("重连后应能正常注册: %v", err)
	}
	if _, online := srvB.counts(); online == 0 {
		t.Fatal("恢复后应完成注册")
	}
	// 本机凭据应已换成 CA-B 的一套
	if fp := c.credCAFingerprint(); fp != srvB.caFP {
		t.Fatalf("恢复后 creds/ca.crt 应为 CA-B: %s != %s", fp, srvB.caFP)
	}
	// 私钥必须复用：服务端按 agent_id→公钥指纹做 TOFU 绑定，重签换新钥就等于
	// "换了台机器"，共享密钥路径会被自己的绑定挡死，现网自动恢复随之失效。
	if got := readFileOrEmpty(dir, "client.key"); got != keyAfterFirst {
		t.Fatal("重新换取必须复用本机私钥，否则公钥指纹变化会被服务端 TOFU 拒发")
	}
}

// TestRejectedForeignCertNotTrusted 端到端验证"服务端不认可旧证书"这条最难的路径：
// Agent 通道的客户端证书是"请求但不强制"，因此旧证书**握手不会失败**，
// 只在应用层被拒（连接建立后立刻关闭）。若 Agent 只能靠 TLS 错误判断，
// 这条路径就会变成"连上又断开"的死循环。
// 这里验证：服务端会下发 EnrollRequired，Agent 据此自动重新换取并恢复。
func TestRejectedForeignCertNotTrusted(t *testing.T) {
	dir := t.TempDir()
	srvA := newFakeServer(t)
	c := New(Config{
		ServerAddr: srvA.addr(), AgentID: "host-01",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: "secret",
	})
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		t.Fatal(err)
	}

	srvB := newFakeServer(t)
	c.cfg.EnrollmentSecret = ""
	c.cfg.AgentKey = "secret"
	c.cfg.ServerAddr = srvB.addr()
	c.cfg.ServerFingerprint = srvB.caFP

	// 首次：发现服务端身份变化 → 重新换取 → 返回"需重连"
	if err := c.connectAndServe(); err != nil && !isReenroll(err) {
		t.Fatalf("应自动重新换取: %v", err)
	}
	if err := c.connectAndServe(); err != nil && !isReenroll(err) {
		t.Fatalf("重连后应能正常注册: %v", err)
	}
	if fp := c.credCAFingerprint(); fp != srvB.caFP {
		t.Fatalf("应已换取 CA-B 的证书: %s != %s", fp, srvB.caFP)
	}
	if _, online := srvB.counts(); online == 0 {
		t.Fatal("恢复后应完成注册")
	}
}

// TestRecoverOnEnrollRequiredWithoutPin 回归"未授权不得自动重换"：
// 服务端换过 CA 且运维没有填新指纹时，Agent 收到 EnrollRequired **不能**自动
// 把注册密钥交给对端 —— 否则"谁能让 Agent 连上我，谁就能拿走它的凭据"。
func TestRecoverOnEnrollRequiredWithoutPin(t *testing.T) {
	dir := t.TempDir()
	srvA := newFakeServer(t)
	c := New(Config{
		ServerAddr: srvA.addr(), AgentID: "host-01",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: "secret",
	})
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		t.Fatal(err)
	}
	oldFP := c.credCAFingerprint()

	srvB := newFakeServer(t)
	c.cfg.EnrollmentSecret = ""
	c.cfg.AgentKey = "secret"
	c.cfg.ServerAddr = srvB.addr()

	err := c.connectAndServe()
	if err == nil {
		t.Fatal("未授权时不应静默重连成功")
	}
	// 无论走哪条路径（pin 校验、链校验、身份比对），都必须拒绝、且给出授权办法
	msg := err.Error()
	if !strings.Contains(msg, "不会自动重换") && !strings.Contains(msg, "不会自动信任") {
		t.Fatalf("应说明'不会自动信任'并给出授权方式，实际: %v", err)
	}
	if !strings.Contains(msg, "server_fingerprint") {
		t.Fatalf("应给出恢复办法（server_fingerprint），实际: %v", err)
	}
	if fp := c.credCAFingerprint(); fp != oldFP {
		t.Fatal("未授权时本机凭据不得被替换")
	}
	if e, o := srvB.counts(); e != 0 || o != 0 {
		t.Fatalf("未授权时不应向对方换取或注册: enrolls=%d online=%d", e, o)
	}
}

// TestConnectRefusesPlaintextWithOldCert 回归既有约束：
// 本机持证时握手失败**不得**降级为明文，否则双向认证会被悄悄绕过。
// 这条在 tlsutil_test.go 已有用例，这里补一条"服务端换了 CA"的现实场景。
func TestConnectRefusesPlaintextWithOldCert(t *testing.T) {
	dir := t.TempDir()
	srvA := newFakeServer(t)
	c := New(Config{
		ServerAddr: srvA.addr(), AgentID: "host-01",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: "secret",
	})
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		t.Fatal(err)
	}
	srvB := newFakeServer(t)
	c.cfg.ServerAddr = srvB.addr()
	c.cfg.EnrollmentSecret = ""
	c.cfg.AgentKey = "" // 没有注册密钥 → 无法重换，只能失败

	err := c.connectAndServe()
	if err == nil {
		t.Fatal("持旧证书且无密钥重换时应失败")
	}
	// 关键：不是"悄悄连上了明文"，而是明确失败并说明原因与办法
	msg := err.Error()
	if !strings.Contains(msg, "无法自动重新换取") && !strings.Contains(msg, "agent_key") {
		t.Fatalf("错误信息应说明'无密钥无法重换'并给出办法，实际: %v", err)
	}
	if _, online := srvB.counts(); online != 0 {
		t.Fatal("不应在未认证的情况下注册成功")
	}
}
