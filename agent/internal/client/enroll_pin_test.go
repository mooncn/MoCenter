package client

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEnrollNeverDowngradesWhenPinned 换取阶段的降级必须有边界（L-6）。
//
// 旧实现把"TLS 校验失败"一律当成"自签部署没 CA 文件"，于是重拨一条**不验签**的连接
// 继续换取。可当本机明明配了 server_fingerprint 时，"校验失败"的含义恰恰是
// "对端不是那套总控"（换过部署身份，或中间人）—— 这时候降级等于把注册凭据
// 交给陌生对端，而且填了指纹这件事在日志里毫无痕迹。
//
// 期望：有锚（指纹/CA）时直接失败，**且不向对端发出任何换取请求**。
func TestEnrollNeverDowngradesWhenPinned(t *testing.T) {
	srv := newFakeServer(t)
	dir := t.TempDir()

	c := New(Config{
		ServerAddr: srv.addr(), AgentID: "host-pin",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: "secret",
		// 与服务端完全无关的指纹：模拟"配置里的锚与对端不符"
		ServerFingerprint: strings.Repeat("0", 16),
	})
	if !c.hasTrustAnchor() {
		t.Fatal("配了 server_fingerprint 即视为已有身份锚")
	}
	_, err := c.tryEnroll()
	if err == nil {
		t.Fatal("锚与对端不符时换取必须失败")
	}
	if !strings.Contains(err.Error(), "不会降级为不验签") {
		t.Fatalf("失败原因要点明'拒绝降级'，否则运维会以为是普通网络问题: %v", err)
	}
	if enrolls, _ := srv.counts(); enrolls != 0 {
		t.Fatalf("拒绝之后不应把注册凭据发给对端，实际收到 %d 次换取请求", enrolls)
	}
}

// TestEnrollBootstrapsWithoutAnchor 没有锚的第一次接入仍要走得通（否则"简单模式"被堵死）：
// 自签服务端 + 无指纹 + 无 CA 时应完成换取，并把 CA 落到 creds/ 成为后续的锚。
func TestEnrollBootstrapsWithoutAnchor(t *testing.T) {
	srv := newFakeServer(t)
	dir := t.TempDir()

	c := New(Config{
		ServerAddr: srv.addr(), AgentID: "host-tofu",
		CredDir: dir, ConfigPath: filepath.Join(dir, "agent.json"),
		EnrollmentSecret: "secret",
	})
	if c.hasTrustAnchor() {
		t.Fatal("全新机器不应被判定为已有锚")
	}
	if _, err := c.tryEnroll(); err != nil {
		t.Fatalf("无锚的首次换取应成功: %v", err)
	}
	if !c.hasTrustAnchor() {
		t.Fatal("换取落下的 creds/ca.crt 必须成为下一次连接的锚（TOFU 生效）")
	}
}
