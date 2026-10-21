package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
	"mocenter/server/internal/version"
)

// 本文件覆盖升级指令下发口（M-10 的服务端那一半）。
//
// 为什么服务端也要挡：Agent 侧的验签是**强制**的（没有跳过开关），
// 缺签名的指令注定失败。让它从界面上下发出去、十几秒后在一台台主机的
// 回执里看到"未提供签名"，比在这里直接报给操作者难排查得多。

func newUpgradeAuthedToken(t *testing.T) (*auth.Auth, string) {
	t.Helper()
	authz := auth.NewAuth(nil)
	const token = "upgrade-test-token"
	authz.RegisterToken(token, "tester")
	return authz, token
}

func postUpgrade(t *testing.T, h http.HandlerFunc, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/upgrade", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func hex64() string  { return strings.Repeat("a", 64) }
func hexSig() string { return strings.Repeat("b", ed25519.SignatureSize*2) }

// startFrameReader 并发读取连接上的信封帧。
//
// 必须并发：net.Pipe 是无缓冲的同步管道，没有读者时 Write 会一直阻塞到写超时
// （agentWriteWait = 30s），用例表现为平白无故慢半分钟。真实链路上 Agent 一直在读。
func startFrameReader(c net.Conn) chan string {
	frames := make(chan string, 8)
	go func() {
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(frames)
				return
			}
			frames <- line
		}
	}()
	return frames
}

// nextFrame 取一帧；窗口内没有帧则返回 false（用于断言"没写出去"）。
func nextFrame(frames chan string) (string, bool) {
	select {
	case f, ok := <-frames:
		return f, ok && f != ""
	case <-time.After(300 * time.Millisecond):
		return "", false
	}
}

// TestUpgradeRequiresSignature 缺签名必须当场拒绝，且不往连接上写任何东西。
func TestUpgradeRequiresSignature(t *testing.T) {
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	body := `{"agent_id":"live-agent","version":"1.2.3","url":"https://example.com/a.bin","sha256":"` + hex64() + `"}`
	rec := postUpgrade(t, upgradeHandler(sess, authz, "", "", nil), token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200 带业务错误，实际 %d %s", rec.Code, rec.Body.String())
	}
	if out := rec.Body.String(); !strings.Contains(out, "签名") {
		t.Fatalf("响应应说明缺签名，实际 %s", out)
	}
	// 闸门必须在写之前：漏到连接上就是"下发成功但其实注定失败"
	if frame, ok := nextFrame(frames); ok {
		t.Fatalf("缺签名的指令不应该被转发给 Agent，实际收到 %s", frame)
	}
}

// TestUpgradeSignatureFormat 签名长度/字符集不对时提前挡下（省一次无谓下发）。
func TestUpgradeSignatureFormat(t *testing.T) {
	sess := session.NewManager()
	authz, token := newUpgradeAuthedToken(t)
	h := upgradeHandler(sess, authz, "", "", nil)
	for name, sig := range map[string]string{
		"过短":   strings.Repeat("b", 32),
		"非hex": strings.Repeat("z", 128),
	} {
		body := `{"agent_id":"a","version":"1.2.3","url":"https://e/x","sha256":"` + hex64() +
			`","signature":"` + sig + `"}`
		if out := postUpgrade(t, h, token, body).Body.String(); !strings.Contains(out, "签名") {
			t.Errorf("%s: 应报签名格式问题，实际 %s", name, out)
		}
	}
}

// TestUpgradeRejectsUnparsableTargetVersion 目标版本参与签名消息，格式必须是点分数字。
func TestUpgradeRejectsUnparsableTargetVersion(t *testing.T) {
	sess := session.NewManager()
	authz, token := newUpgradeAuthedToken(t)
	body := `{"agent_id":"a","version":"latest","url":"https://e/x","sha256":"` + hex64() +
		`","signature":"` + hexSig() + `"}`
	if out := postUpgrade(t, upgradeHandler(sess, authz, "", "", nil), token, body).Body.String(); !strings.Contains(out, "版本号") {
		t.Fatalf("应拒绝无法解析的目标版本，实际 %s", out)
	}
}

// TestUpgradeRequiresTargetVersion 目标版本留空必须报错，不能自动补服务端版本。
//
// 签名消息里含目标版本号：一旦服务端替操作者猜一个版本，Agent 侧必然报
// "签名校验失败"，看起来像是升级包被动过 —— 比在这里要求填写难排查得多。
func TestUpgradeRequiresTargetVersion(t *testing.T) {
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	for name, ver := range map[string]string{"缺省": `""`, "纯空白": `"   "`} {
		body := `{"agent_id":"live-agent","version":` + ver + `,"url":"https://e/x","sha256":"` + hex64() +
			`","signature":"` + hexSig() + `"}`
		if out := postUpgrade(t, upgradeHandler(sess, authz, "", "", nil), token, body).Body.String(); !strings.Contains(out, "版本号") {
			t.Errorf("%s: 应要求填写目标版本号，实际 %s", name, out)
		}
	}
	if frame, ok := nextFrame(frames); ok {
		t.Fatalf("版本为空的指令不该被下发，实际收到 %s", frame)
	}
}

// TestUpgradeForwardsSignatureAndDowngradeFlag 校验通过的指令要原样落到信封里，
// 尤其是 allow_downgrade —— 它被丢掉就等于运维永远无法回退版本。
func TestUpgradeForwardsSignatureAndDowngradeFlag(t *testing.T) {
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	body := `{"agent_id":"live-agent","version":"0.9.0","url":"https://e/x","sha256":"` + hex64() +
		`","signature":"` + hexSig() + `","force":true,"allow_downgrade":true}`
	rec := postUpgrade(t, upgradeHandler(sess, authz, "pubkey-hex", "", nil), token, body)
	if out := rec.Body.String(); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("应下发成功，实际 %s", out)
	}

	line, ok := nextFrame(frames)
	if !ok {
		t.Fatalf("校验通过的指令应已写到连接上")
	}
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	if env.Type != protocol.TypeUpgradeReq {
		t.Fatalf("消息类型 %q", env.Type)
	}
	var got protocol.UpgradeReq
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("负载解析失败: %v", err)
	}
	if got.Version != "0.9.0" || got.Signature != hexSig() || !got.Force || !got.AllowDowngrade {
		t.Fatalf("下发内容不完整: %+v", got)
	}
	if got.SHA256 != hex64() {
		t.Fatalf("摘要未规范化: %q", got.SHA256)
	}
}

// TestUpgradeHTTPURLStillRejected 放行签名字段不能把 http 那扇门带回来。
func TestUpgradeHTTPURLStillRejected(t *testing.T) {
	sess := session.NewManager()
	authz, token := newUpgradeAuthedToken(t)
	body := `{"agent_id":"a","version":"1.2.3","url":"http://e/x","sha256":"` + hex64() +
		`","signature":"` + hexSig() + `"}`
	if out := postUpgrade(t, upgradeHandler(sess, authz, "", "", nil), token, body).Body.String(); !strings.Contains(out, "https") {
		t.Fatalf("http 下载地址应被拒绝，实际 %s", out)
	}
}

// TestUpgradeLocalPackage 本地 pkg 模式：无需 URL/签名，摘要由服务端现算，
// 下发的 UpgradeReq 带 PackageName 且 URL/Signature 为空。
func TestUpgradeLocalPackage(t *testing.T) {
	dir := t.TempDir()
	const pkgName = "mocenter-agent-amd64"
	content := []byte("fake-agent-binary-content")
	if err := os.WriteFile(filepath.Join(dir, pkgName), content, 0755); err != nil {
		t.Fatal(err)
	}
	probes := probe.NewStore(nil)
	probes.UpsertAgent(&protocol.AgentInfo{AgentID: "live-agent", OSType: "linux", Arch: "amd64"})

	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	h := upgradeHandler(sess, authz, "", dir, probes)
	body := `{"agent_id":"live-agent","version":"1.2.3","package":"` + pkgName + `"}`
	rec := postUpgrade(t, h, token, body)
	if out := rec.Body.String(); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("本地包应下发成功，实际 %s", out)
	}

	line, ok := nextFrame(frames)
	if !ok {
		t.Fatal("本地升级指令应写到连接上")
	}
	var env protocol.Envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatal(err)
	}
	var got protocol.UpgradeReq
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if got.PackageName != pkgName || got.URL != "" || got.Signature != "" ||
		got.SHA256 != hex.EncodeToString(sum[:]) || got.Version != "1.2.3" {
		t.Fatalf("本地升级指令异常: %+v", got)
	}
}

// TestUpgradeLocalDefaultsToServerVersion 本地模式下版本可省略，默认用服务端版本。
func TestUpgradeLocalDefaultsToServerVersion(t *testing.T) {
	dir := t.TempDir()
	const pkgName = "mocenter-agent-amd64"
	if err := os.WriteFile(filepath.Join(dir, pkgName), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	h := upgradeHandler(sess, authz, "", dir, nil)
	body := `{"agent_id":"live-agent","package":"` + pkgName + `"}`
	if out := postUpgrade(t, h, token, body).Body.String(); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("本地包应下发成功，实际 %s", out)
	}
	line, ok := nextFrame(frames)
	if !ok {
		t.Fatal("应收到升级指令")
	}
	var env protocol.Envelope
	_ = json.Unmarshal([]byte(line), &env)
	var got protocol.UpgradeReq
	_ = json.Unmarshal(env.Data, &got)
	if got.Version != version.Version {
		t.Fatalf("本地模式未填版本时应使用服务端版本 %s，实际 %q", version.Version, got.Version)
	}
}

// TestUpgradeLocalRejectsWrongOS 不能把 Windows 包推给 Linux 主机。
func TestUpgradeLocalRejectsWrongOS(t *testing.T) {
	dir := t.TempDir()
	const pkgName = "mocenter-agent-amd64.exe"
	if err := os.WriteFile(filepath.Join(dir, pkgName), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	probes := probe.NewStore(nil)
	probes.UpsertAgent(&protocol.AgentInfo{AgentID: "live-agent", OSType: "linux", Arch: "amd64"})
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess := session.NewManager()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	authz, token := newUpgradeAuthedToken(t)
	frames := startFrameReader(agentSide)

	body := `{"agent_id":"live-agent","version":"1.2.3","package":"` + pkgName + `"}`
	out := postUpgrade(t, upgradeHandler(sess, authz, "", dir, probes), token, body).Body.String()
	if !strings.Contains(out, "windows") || !strings.Contains(out, "linux") {
		t.Fatalf("应拒绝系统不匹配的包，实际 %s", out)
	}
	if frame, ok := nextFrame(frames); ok {
		t.Fatalf("被拒绝的包不应下发，实际 %s", frame)
	}
}

// TestUpgradeLocalRejectsTraversal pkg 名称不能带路径。
func TestUpgradeLocalRejectsTraversal(t *testing.T) {
	authz, token := newUpgradeAuthedToken(t)
	sess := session.NewManager()
	agentSide, serverSide := net.Pipe()
	defer agentSide.Close()
	sess.Upsert("live-agent", session.NewConn(serverSide))
	_ = startFrameReader(agentSide)

	out := postUpgrade(t, upgradeHandler(sess, authz, "", t.TempDir(), nil), token,
		`{"agent_id":"live-agent","version":"1.2.3","package":"../mocenter-agent-amd64"}`).Body.String()
	if !strings.Contains(out, "不合法") {
		t.Fatalf("路径穿越应被拒绝，实际 %s", out)
	}
}
