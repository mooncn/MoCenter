package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/agent/internal/protocol"
)

// 本包测试锁的是 M-10 的三条不变量：
//   1. 摘要与签名对**同一份内存字节**校验（不存在"校验的内容"与"执行的内容"两份）；
//   2. 签名覆盖目标版本号，换 version 就换不掉验签；
//   3. 降级需要显式表态，force 不再顺带放开它。

func newKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return pub, priv
}

func sumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func signOf(priv ed25519.PrivateKey, target string, content []byte) string {
	return hex.EncodeToString(ed25519.Sign(priv, signedMessage(target, content)))
}

func TestDecideUpgrade(t *testing.T) {
	cases := []struct {
		name           string
		target, cur    string
		force, down    bool
		wantSkip       bool
		wantErrInclude string
	}{
		{name: "更高版本正常升级", target: "1.2.0", cur: "1.1.0"},
		{name: "同版本默认跳过", target: "1.1.0", cur: "1.1.0", wantSkip: true},
		{name: "同版本强制重装", target: "1.1.0", cur: "1.1.0", force: true},
		{name: "降级默认拒绝", target: "1.0.9", cur: "1.1.0", wantErrInclude: "降级"},
		{name: "force 不能越过降级闸门", target: "0.1", cur: "1.1.0", force: true, wantErrInclude: "降级"},
		{name: "显式允许降级才放行", target: "1.0.9", cur: "1.1.0", down: true},
		{name: "缺目标版本拒绝", target: "", cur: "1.1.0", wantErrInclude: "目标版本号"},
		{name: "畸形目标版本拒绝", target: "latest", cur: "1.1.0", wantErrInclude: "无法比较"},
		{name: "本机版本畸形时也不放行", target: "1.2.0", cur: "unknown", wantErrInclude: "无法比较"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, err := decideUpgrade(tc.target, tc.cur, tc.force, tc.down)
			if tc.wantErrInclude != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrInclude) {
					t.Fatalf("期望包含 %q 的错误，实际 %v", tc.wantErrInclude, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应出错: %v", err)
			}
			if skip != tc.wantSkip {
				t.Fatalf("skip=%v 期望 %v", skip, tc.wantSkip)
			}
		})
	}
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://example.com/a", ""},
		{"HTTPS://example.com/a", ""},
		{"http://example.com/a", "https"},
		{"ftp://example.com/a", "不被允许"},
		{"file:///etc/passwd", "不被允许"},
		{"://bad", "不合法"},
	} {
		err := checkURL(tc.url)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s 应放行，实际 %v", tc.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s 应报 %q，实际 %v", tc.url, tc.want, err)
		}
	}
}

// TestVerifyPackageBindsVersion 同一份签名换掉 version 字段必须验不过。
//
// 这是 M-10 的核心：旧实现只对包内容签名，于是"合法的老包 + 任意 version"
// 是一个自洽的组合，拿到通道的人可以把机群一路钉回旧版本。
func TestVerifyPackageBindsVersion(t *testing.T) {
	pub, priv := newKeyPair(t)
	content := []byte("binary-payload")
	target := "1.2.0"
	sig := signOf(priv, target, content)

	if err := verifyPackage(pub, target, sig, sumOf(content), content); err != nil {
		t.Fatalf("正确签名应通过: %v", err)
	}
	if err := verifyPackage(pub, "9.9.9", sig, sumOf(content), content); err == nil {
		t.Fatal("把目标版本改成 9.9.9 后签名仍有效 —— 版本号没有进签名")
	}
	tampered := append([]byte(nil), content...)
	tampered[0] ^= 0xff
	if err := verifyPackage(pub, target, sig, sumOf(tampered), tampered); err == nil {
		t.Fatal("内容被替换后应校验失败")
	}
	if err := verifyPackage(pub, target, sig, strings.Repeat("0", 64), content); err == nil ||
		!strings.Contains(err.Error(), "sha256") {
		t.Fatalf("摘要不匹配应报 sha256，实际 %v", err)
	}
}

func TestVerifyPackageSignatureFormat(t *testing.T) {
	pub, _ := newKeyPair(t)
	content := []byte("x")
	for name, sig := range map[string]string{
		"非 hex": strings.Repeat("z", 128),
		"长度不足":  "abcd",
		"空":     "",
	} {
		if err := verifyPackage(pub, "1.0.0", sig, sumOf(content), content); err == nil {
			t.Errorf("%s: 非法签名应被拒绝", name)
		}
	}
}

func TestSignedMessageShape(t *testing.T) {
	got := string(signedMessage("1.2.3", []byte("abc")))
	if got != signatureDomain+"1.2.3\nabc" {
		t.Fatalf("签名消息格式变了，历史签名会全部失效: %q", got)
	}
}

// TestPerformRefusesWithoutPubKey 没公钥时不得发起任何下载（逃生口已删除）。
func TestPerformRefusesWithoutPubKey(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	var resp *protocol.UpgradeResp
	Perform(&protocol.UpgradeReq{
		UpgradeID: "u1", Version: "9.9.9", URL: "https://127.0.0.1:1/agent",
		SHA256: strings.Repeat("a", 64), Signature: strings.Repeat("b", 128),
	}, Policy{}, func(r *protocol.UpgradeResp) { resp = r })

	if resp == nil {
		t.Fatal("必须回报结果")
	}
	if resp.OK || !strings.Contains(resp.Message, "拒绝") {
		t.Fatalf("无公钥应拒绝升级，实际 %+v", resp)
	}
	if strings.Contains(resp.Message, "allow-unsigned") {
		t.Fatalf("提示信息不该再引导去开逃生口: %s", resp.Message)
	}
	if hits != 0 {
		t.Fatalf("拒绝路径不应产生外呼")
	}
}

// TestPerformRejectsDowngradeBeforeNetwork 降级在联网取包之前就被挡下。
func TestPerformRejectsDowngradeBeforeNetwork(t *testing.T) {
	pub, _ := newKeyPair(t)
	var resp *protocol.UpgradeResp
	Perform(&protocol.UpgradeReq{
		Version: "0.0.1", Force: true,
		URL: "https://127.0.0.1:1/agent", SHA256: strings.Repeat("a", 64),
		Signature: hex.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}, Policy{PubKey: hex.EncodeToString(pub)}, func(r *protocol.UpgradeResp) { resp = r })
	if resp == nil || resp.OK || !strings.Contains(resp.Message, "降级") {
		t.Fatalf("force 降级应被拒绝，实际 %+v", resp)
	}
}

// TestPerformRejectsPlainHTTP 明文下载地址永远不放行（旧版 AllowUnsigned 会放行）。
func TestPerformRejectsPlainHTTP(t *testing.T) {
	pub, _ := newKeyPair(t)
	var resp *protocol.UpgradeResp
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	Perform(&protocol.UpgradeReq{
		Version: "9.9.9", URL: srv.URL, SHA256: strings.Repeat("a", 64),
		Signature: hex.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}, Policy{PubKey: hex.EncodeToString(pub)}, func(r *protocol.UpgradeResp) { resp = r })
	if resp == nil || resp.OK || !strings.Contains(resp.Message, "https") {
		t.Fatalf("http 地址应被拒绝，实际 %+v", resp)
	}
}

func TestFetchPackageRejectsOversize(t *testing.T) {
	body := strings.Repeat("x", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	orig := maxPackageBytes
	maxPackageBytes = 16
	t.Cleanup(func() { maxPackageBytes = orig })

	if _, err := fetchPackage(srv.URL); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超限响应应被拒绝，实际 %v", err)
	}
	maxPackageBytes = 1 << 20
	got, err := fetchPackage(srv.URL)
	if err != nil || string(got) != body {
		t.Fatalf("未超限应完整取回: %q %v", got, err)
	}
}

func TestFetchPackageStatusAndEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/500":
			http.Error(w, "boom", http.StatusInternalServerError)
		default: // 空包
		}
	}))
	defer srv.Close()
	if _, err := fetchPackage(srv.URL + "/500"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("非 200 应报错，实际 %v", err)
	}
	if _, err := fetchPackage(srv.URL + "/empty"); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Fatalf("空升级包应被拒绝，实际 %v", err)
	}
}

// TestInstallBinaryReplacesWithVerifiedBytes 落盘的就是内存里那份已验签的字节。
func TestInstallBinaryReplacesWithVerifiedBytes(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mocenter-agent")
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatalf("准备旧二进制失败: %v", err)
	}
	content := []byte("new-signed-payload")
	if err := installBinary(exe, content, sumOf(content)); err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != string(content) {
		t.Fatalf("新内容未落位: %q %v", got, err)
	}
	// 临时文件不该留在目录里
	rest, _ := filepath.Glob(filepath.Join(dir, ".mocenter-upgrade-*"))
	if len(rest) != 0 {
		t.Fatalf("遗留临时文件: %v", rest)
	}
}

// TestInstallBinaryAbortsOnDiskDigestMismatch 盘上内容与校验值不符时放弃替换，
// 旧二进制必须还在原位。
func TestInstallBinaryAbortsOnDiskDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mocenter-agent")
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatalf("准备旧二进制失败: %v", err)
	}
	content := []byte("new")
	if err := installBinary(exe, content, strings.Repeat("0", 64)); err == nil {
		t.Fatal("摘要不符时应该失败")
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "old" {
		t.Fatalf("失败路径必须保住旧二进制，实际 %q %v", got, err)
	}
}

func TestLoadPubKey(t *testing.T) {
	pub, _ := newKeyPair(t)
	hexKey := hex.EncodeToString(pub)
	if got, err := loadPubKey(" ed25519:" + hexKey + " "); err != nil || len(got) != len(pub) {
		t.Fatalf("hex 公钥解析失败: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "upgrade.pub")
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0600); err != nil {
		t.Fatalf("写公钥文件失败: %v", err)
	}
	if got, err := loadPubKey(path); err != nil || hex.EncodeToString(got) != hexKey {
		t.Fatalf("公钥文件解析失败: %v", err)
	}
	if got, err := loadPubKey(""); err != nil || got != nil {
		t.Fatalf("空串应解析为未配置: %v %v", got, err)
	}
	if _, err := loadPubKey(strings.Repeat("z", 64)); err == nil {
		t.Fatal("非法 hex 应报错")
	}
	if _, err := loadPubKey("abcd"); err == nil {
		t.Fatal("长度不对的公钥应报错")
	}
}

// TestPerformLocalVerifiesHashAndInstalls 本地 pkg 模式：不要求公钥/签名，
// 但必须校验 SHA-256；校验通过后用同一份内存字节安装。
func TestPerformLocalVerifiesHashAndInstalls(t *testing.T) {
	content := []byte("local-pkg-binary")
	goodSum := sumOf(content)

	origExec, origInstall, origRestart := currentExecPathFn, installBinaryFn, restartSelfFn
	t.Cleanup(func() {
		currentExecPathFn, installBinaryFn, restartSelfFn = origExec, origInstall, origRestart
	})
	currentExecPathFn = func() (string, error) { return filepath.Join(t.TempDir(), "mocenter-agent"), nil }
	var installed []byte
	installBinaryFn = func(_ string, c []byte, _ string) error {
		installed = append([]byte(nil), c...)
		return nil
	}
	restarted := make(chan struct{}, 1)
	restartSelfFn = func(string) { restarted <- struct{}{} }

	var resp *protocol.UpgradeResp
	PerformLocal(&protocol.UpgradeReq{
		UpgradeID: "u-local", Version: "99.0.0", PackageName: "mocenter-agent-amd64", SHA256: goodSum,
	}, content, func(r *protocol.UpgradeResp) { resp = r })
	if resp == nil || !resp.OK {
		t.Fatalf("本地包应升级成功，实际 %+v", resp)
	}
	if string(installed) != string(content) {
		t.Fatalf("安装内容不是校验过的那份字节")
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("升级成功后应触发重启")
	}
}

// TestPerformLocalRejectsBadHash 摘要不符（或缺失）时不得安装。
func TestPerformLocalRejectsBadHash(t *testing.T) {
	origInstall := installBinaryFn
	installed := false
	installBinaryFn = func(string, []byte, string) error { installed = true; return nil }
	t.Cleanup(func() { installBinaryFn = origInstall })

	var resp *protocol.UpgradeResp
	PerformLocal(&protocol.UpgradeReq{
		UpgradeID: "u", Version: "99.0.0", PackageName: "pkg", SHA256: strings.Repeat("0", 64),
	}, []byte("content"), func(r *protocol.UpgradeResp) { resp = r })
	if resp == nil || resp.OK || !strings.Contains(resp.Message, "SHA-256") {
		t.Fatalf("摘要不符应拒绝，实际 %+v", resp)
	}
	if installed {
		t.Fatal("校验失败不应进入安装")
	}
}
