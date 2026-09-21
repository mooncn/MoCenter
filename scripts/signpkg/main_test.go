package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// TestSignedMessageLiteralMatchesAgent 钉住签名消息格式。
//
// agent/internal/upgrade 里有一份同名字面量的断言（两个模块无法共享常量）。
// 任何一侧单独改动都会让现网签名全部失效 —— 所以两边各留一条测试，
// 改格式必须同时改两处并知道自己在做什么。
func TestSignedMessageLiteralMatchesAgent(t *testing.T) {
	got := string(signedMessage("1.0.2", []byte("pkg")))
	want := "MoCenter-Upgrade-v1\n" + "1.0.2" + "\n" + "pkg"
	if got != want {
		t.Fatalf("签名消息格式变了:\n got %q\nwant %q", got, want)
	}
	if signatureDomain != "MoCenter-Upgrade-v1\n" {
		t.Fatalf("前缀常量与 Agent 侧不一致: %q", signatureDomain)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	key := hex.EncodeToString(priv.Seed())
	content := []byte("binary content")

	sig, err := signFile(key, "1.0.2", content)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	pubHex := hex.EncodeToString(pub)
	if err := verifyFile(pubHexMust(t, pubHex), "1.0.2", sig, "", content); err != nil {
		t.Fatalf("自签自验应通过: %v", err)
	}
	// 版本号一换就该验不过：这条断言是整个 M-10 收口的意义所在
	if err := verifyFile(pubHexMust(t, pubHex), "9.9.9", sig, "", content); err == nil {
		t.Fatal("签名不绑版本号的话，旧包可以被当成任意版本下发")
	}
	tampered := append([]byte(nil), content...)
	tampered[0] = 'X'
	if err := verifyFile(pubHexMust(t, pubHex), "1.0.2", sig, "", tampered); err == nil {
		t.Fatal("内容被改后应验签失败")
	}
}

func pubHexMust(t *testing.T, s string) ed25519.PublicKey {
	t.Helper()
	k, err := readKey(s)
	if err != nil {
		t.Fatalf("公钥解析失败: %v", err)
	}
	return k
}

func TestLoadPrivateAcceptsSeedAndFullKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	if _, err := loadPrivate(hex.EncodeToString(priv.Seed())); err != nil {
		t.Fatalf("seed 形式应可用: %v", err)
	}
	if _, err := loadPrivate(hex.EncodeToString(priv)); err != nil {
		t.Fatalf("完整私钥形式应可用: %v", err)
	}
	if _, err := loadPrivate("abcd"); err == nil {
		t.Fatal("长度不对的私钥应报错")
	}
}
