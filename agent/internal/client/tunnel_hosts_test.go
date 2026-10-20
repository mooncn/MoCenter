package client

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTunnelTargetAllowed 隧道目标默认只认回环（L-5）。
//
// 服务端各调用点从来只发 127.0.0.1，所以这条限制不影响任何既有功能；
// 它挡的是"把每台被管主机当成通往其所在网络的代理"这一种放大。
func TestTunnelTargetAllowed(t *testing.T) {
	c := &Client{cfg: Config{}}
	for _, ok := range []string{"127.0.0.1", "localhost", "::1", "127.0.0.53"} {
		if !c.tunnelTargetAllowed(ok) {
			t.Errorf("回环目标 %s 应被允许", ok)
		}
	}
	for _, bad := range []string{"10.0.0.7", "192.168.1.1", "example.com", "", "  8.8.8.8 "} {
		if c.tunnelTargetAllowed(bad) {
			t.Errorf("非回环目标 %q 默认应被拒绝", bad)
		}
	}

	// 本机显式放行后才可指向其它地址（服务端指令无法打开这个开关）
	c2 := &Client{cfg: Config{TunnelHosts: []string{"10.0.0.7", "db.internal"}}}
	if !c2.tunnelTargetAllowed("10.0.0.7") || !c2.tunnelTargetAllowed("db.internal") {
		t.Error("tunnel_hosts 白名单未生效")
	}
	if c2.tunnelTargetAllowed("10.0.0.8") {
		t.Error("白名单外的地址不应被放行")
	}
}

// TestHasTrustAnchor 有锚时换取阶段不得降级为不验签（L-6）。
func TestHasTrustAnchor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	none := &Client{cfg: Config{CredDir: t.TempDir()}}
	if none.hasTrustAnchor() {
		t.Fatal("全新机器（无指纹/无 CA）不应算作已有锚，否则首次接入换不到证书")
	}
	byPin := &Client{cfg: Config{CredDir: dir, ServerFingerprint: "ab12 cd34 ef56 7890"}}
	if !byPin.hasTrustAnchor() {
		t.Fatal("配置了 server_fingerprint 即为有锚")
	}
	byCA := &Client{cfg: Config{CredDir: dir}}
	if !byCA.hasTrustAnchor() {
		t.Fatal("creds/ca.crt（上次换取落盘）即为有锚")
	}
	byCfgCA := &Client{cfg: Config{CredDir: t.TempDir(), CACert: filepath.Join(dir, "ca.crt")}}
	if !byCfgCA.hasTrustAnchor() {
		t.Fatal("配置指定了 ca_cert 即为有锚")
	}
}
