package agentdeploy

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// 本文件覆盖 H-6：部署通道的主机密钥校验。
//
// 修之前这里是 ssh.InsecureIgnoreHostKey()：服务端带着注册密钥和操作员口令去连
// 别人机器，握手时却不认对方是谁 —— 路径上的中间人可以直接收下这些凭据。

// testHostKey 生成一把临时的 ED25519 主机密钥。
func testHostKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("构造 signer 失败: %v", err)
	}
	return signer, signer.PublicKey()
}

// runCallback 用真实的 remote 地址调用回调（knownhosts 要求 remote 带端口）。
func runCallback(cb ssh.HostKeyCallback, addr string, key ssh.PublicKey) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("测试用例地址写法不对: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("测试用例端口写法不对: %w", err)
	}
	return cb(addr, &net.TCPAddr{IP: net.ParseIP(host), Port: n}, key)
}

func newStoreDeployer(t *testing.T) *Deployer {
	t.Helper()
	return New(Options{DataDir: t.TempDir()})
}

func storePath(t *testing.T, d *Deployer) string {
	t.Helper()
	p, err := d.knownHostsPath()
	if err != nil {
		t.Fatalf("knownHostsPath 失败: %v", err)
	}
	return p
}

func lineFor(addr string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
}

// TestHostKeyCallbackTOFU 首次接触记账放行，同一密钥再次放行，密钥变更硬失败。
func TestHostKeyCallbackTOFU(t *testing.T) {
	d := newStoreDeployer(t)
	addr := "192.0.2.10:22"
	_, keyA := testHostKey(t)
	_, keyB := testHostKey(t)

	trust := &hostKeyTrust{}
	cb, err := d.hostKeyCallback(addr, "", trust)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	if err := runCallback(cb, addr, keyA); err != nil {
		t.Fatalf("首次接触应放行（TOFU）: %v", err)
	}
	if !trust.NewlyTrusted {
		t.Error("首次接触应标记 NewlyTrusted，让前端提示核对")
	}
	if trust.Fingerprint != ssh.FingerprintSHA256(keyA) {
		t.Errorf("指纹 = %q，期望 %q", trust.Fingerprint, ssh.FingerprintSHA256(keyA))
	}

	data, err := os.ReadFile(storePath(t, d))
	if err != nil {
		t.Fatalf("读取记账文件失败: %v", err)
	}
	if !strings.Contains(string(data), lineFor(addr, keyA)) {
		t.Errorf("记账文件应含首次接触的密钥行 %q，实际:\n%s", lineFor(addr, keyA), data)
	}

	// 同一密钥再来一次：放行，且不再算"首次信任"
	trust2 := &hostKeyTrust{}
	cb2, err := d.hostKeyCallback(addr, "", trust2)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	if err := runCallback(cb2, addr, keyA); err != nil {
		t.Fatalf("已记账的密钥应放行: %v", err)
	}
	if trust2.NewlyTrusted {
		t.Error("第二次连接不应再记为首次信任")
	}

	// 密钥变更：必须硬失败，且要说清"记录是谁、实际是谁"
	trust3 := &hostKeyTrust{}
	cb3, err := d.hostKeyCallback(addr, "", trust3)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	err = runCallback(cb3, addr, keyB)
	if err == nil {
		t.Fatal("主机密钥变更后仍然放行，known_hosts 等于没生效")
	}
	for _, want := range []string{"不一致", ssh.FingerprintSHA256(keyA), ssh.FingerprintSHA256(keyB)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("变更错误信息缺少 %q: %s", want, err)
		}
	}
}

// TestHostKeyCallbackPinned 表单填了指纹时按指纹强制校验，不放行错的密钥。
func TestHostKeyCallbackPinned(t *testing.T) {
	d := newStoreDeployer(t)
	addr := "192.0.2.11:22"
	_, keyA := testHostKey(t)
	_, keyB := testHostKey(t)

	wrong := &hostKeyTrust{}
	cb, err := d.hostKeyCallback(addr, ssh.FingerprintSHA256(keyB), wrong)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	if err := runCallback(cb, addr, keyA); err == nil {
		t.Fatal("指纹与填写值不一致时必须中止")
	} else if !strings.Contains(err.Error(), "已中止") {
		t.Errorf("错误信息应说明已中止: %s", err)
	}
	// 被拒的密钥不能被顺手记进账，否则一次中间人就能把自己变成"已知主机"
	data, _ := os.ReadFile(storePath(t, d))
	if strings.Contains(string(data), lineFor(addr, keyA)) {
		t.Error("指纹校验失败的密钥被写入了记账文件")
	}

	// 只写 base64 部分（省略 SHA256: 前缀）也应当接受
	trimmed := &hostKeyTrust{}
	raw := strings.TrimPrefix(ssh.FingerprintSHA256(keyA), "SHA256:")
	cb2, err := d.hostKeyCallback(addr, raw, trimmed)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	if err := runCallback(cb2, addr, keyA); err != nil {
		t.Fatalf("省略前缀的指纹应能用: %v", err)
	}
}

// TestHostKeyCallbackFailsClosed 记录不可用时不能退回"忽略主机密钥"。
func TestHostKeyCallbackFailsClosed(t *testing.T) {
	_, key := testHostKey(t)

	t.Run("未配置数据目录", func(t *testing.T) {
		d := New(Options{})
		if _, err := d.hostKeyCallback("192.0.2.12:22", "", &hostKeyTrust{}); err == nil {
			t.Fatal("没有数据目录时应报错（无法记账就不能做 TOFU）")
		}
	})

	t.Run("记账文件被写坏", func(t *testing.T) {
		d := newStoreDeployer(t)
		if err := os.WriteFile(storePath(t, d), []byte("not a known_hosts line ;;;\n"), 0600); err != nil {
			t.Fatalf("预置文件失败: %v", err)
		}
		cb, err := d.hostKeyCallback("192.0.2.12:22", "", &hostKeyTrust{})
		if err == nil {
			if err := runCallback(cb, "192.0.2.12:22", key); err == nil {
				t.Fatal("记录文件解析失败时不应放行")
			}
			return
		}
		if !strings.Contains(err.Error(), "读取主机密钥记录") {
			t.Fatalf("错误信息不符: %v", err)
		}
	})
}

// TestKnownHostsFileMode 记账文件必须只有服务端可读（里面是全网的主机身份）。
func TestKnownHostsFileMode(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows 上权限位不可信")
	}
	d := newStoreDeployer(t)
	_, key := testHostKey(t)
	trust := &hostKeyTrust{}
	cb, err := d.hostKeyCallback("192.0.2.13:22", "", trust)
	if err != nil {
		t.Fatalf("构造回调失败: %v", err)
	}
	if err := runCallback(cb, "192.0.2.13:22", key); err != nil {
		t.Fatalf("首次接触应放行: %v", err)
	}
	st, err := os.Stat(storePath(t, d))
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("记账文件权限 = %o，不应允许同组/其他用户访问", perm)
	}
}

// TestDeployRejectsBadFingerprint 指纹格式在进入 SSH 前就要挡下（这个值会进错误信息与审计）。
func TestDeployRejectsBadFingerprint(t *testing.T) {
	d := newStoreDeployer(t)
	for _, fp := range []string{"SHA256:;rm -rf /", "short", "MD5:aa:bb:cc", "SHA256:abc def"} {
		_, err := d.Deploy(&Request{
			Host: "192.0.2.14", User: "root", Password: "x", HostKeyFingerprint: fp,
		}, "op", "1.2.3.4")
		if err == nil {
			t.Errorf("非法指纹 %q 应被拒绝", fp)
		} else if !strings.Contains(err.Error(), "指纹") {
			t.Errorf("错误信息应指向指纹: %v", err)
		}
	}
}

// TestDialVerifiesHostKey 端到端：真走一次 SSH 握手，确认没有"忽略主机密钥"的退路。
//
// 只测回调的话，把 HostKeyCallback 换回 InsecureIgnoreHostKey 也能全绿，
// 所以这条必须经过 dial。
func TestDialVerifiesHostKey(t *testing.T) {
	signerA, keyA := testHostKey(t)
	addr := startTestSSHServer(t, signerA)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("监听地址异常: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("监听端口异常: %v", err)
	}

	d := newStoreDeployer(t)
	req := &Request{Host: host, Port: port, User: "root", Password: "whatever"}

	client, trust, err := d.dial(req, host, port)
	if err != nil {
		t.Fatalf("首次连接（TOFU）应成功: %v", err)
	}
	client.Close()
	if trust.Fingerprint != ssh.FingerprintSHA256(keyA) {
		t.Errorf("指纹 = %q，期望 %q", trust.Fingerprint, ssh.FingerprintSHA256(keyA))
	}
	if !trust.NewlyTrusted {
		t.Error("首次连接应记为首次信任")
	}

	// 模拟"目标机密钥变了"（换密钥或被顶替）：把记账换成另一把密钥再连，必须失败。
	// 这一步在 InsecureIgnoreHostKey 下是必然通过的。
	_, otherKey := testHostKey(t)
	path := storePath(t, d)
	if err := os.WriteFile(path, []byte(lineFor(addr, otherKey)+"\n"), 0600); err != nil {
		t.Fatalf("改写记账文件失败: %v", err)
	}
	client2, trust2, err := d.dial(req, host, port)
	if client2 != nil {
		client2.Close()
	}
	if err == nil {
		t.Fatal("主机密钥与记录不一致时仍然连上了")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("应报密钥不一致，实际: %v", err)
	}
	if trust2.Fingerprint != ssh.FingerprintSHA256(keyA) {
		t.Errorf("被拒时也要留下实际收到的指纹，供审计: %q", trust2.Fingerprint)
	}

	// 记账清干净后，钉扎到正确指纹应当放行。
	if err := os.Remove(path); err != nil {
		t.Fatalf("清理记账文件失败: %v", err)
	}
	pinned := &Request{Host: host, Port: port, User: "root", Password: "whatever",
		HostKeyFingerprint: ssh.FingerprintSHA256(keyA)}
	client3, _, err := d.dial(pinned, host, port)
	if err != nil {
		t.Fatalf("钉扎到正确指纹应放行: %v", err)
	}
	client3.Close()
}

// startTestSSHServer 起一个只完成握手 + 密码认证的 in-process sshd，返回监听地址。
func startTestSSHServer(t *testing.T, hostKey ssh.Signer) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil // 主机密钥校验在认证之前，这里不关心口令
		},
	}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("本机无法监听 TCP 端口: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestSSH(c, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveTestSSH(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Wait()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		_ = ch.Reject(ssh.Prohibited, "test server accepts no channels")
	}
}

// TestTrustSummaryMentionsVerification 首次信任的文案必须带"请核对"，
// 不能把 TOFU 写成一件看起来无需理会的事。
func TestTrustSummaryMentionsVerification(t *testing.T) {
	s := trustSummary(&hostKeyTrust{Fingerprint: "SHA256:aaa", NewlyTrusted: true})
	if !strings.Contains(s, "核对") {
		t.Errorf("首次信任提示应要求核对: %s", s)
	}
	if hostKeyForAudit(nil) != "-" {
		t.Error("未握手成功时应记为 '-'，便于区分网络不通与密钥被拒")
	}
	if got := hostKeyForAudit(&hostKeyTrust{Fingerprint: "SHA256:aaa", NewlyTrusted: true}); !strings.Contains(got, "首次信任") {
		t.Errorf("审计摘要应标出首次信任: %s", got)
	}
}

// TestNormalizeFingerprint 归一化用户填写的指纹写法。
func TestNormalizeFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  SHA256:abc123  ", "SHA256:abc123"},
		{"abc123", "SHA256:abc123"},
		{`"SHA256:abc123"`, "SHA256:abc123"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalizeFingerprint(c.in); got != c.want {
			t.Errorf("normalizeFingerprint(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
