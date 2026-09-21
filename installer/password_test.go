//go:build windows

package main

import (
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// katSalt 与下面两组期望值由独立的 PBKDF2 实现（CPython hashlib.pbkdf2_hmac）产出，
// 不是本文件代码算出来的 —— 这样才能真正校验算法实现，而不是自我印证。
// 同一组向量也被 server/internal/auth/password_format_test.go 使用：
// 服务端认不下安装器写的摘要时，那边会先红。
var katSalt, _ = hex.DecodeString("0123456789abcdef0123456789abcdef")

func TestPbkdf2HashKnownAnswer(t *testing.T) {
	cases := []struct {
		pw   string
		iter int
		dk   string
	}{
		{"mocenter-admin-pw", 1000, "8503e2ff49508cd2f73d4f087da3ea014e4c5d7605e30b246d73ad23502c9beb"},
		{"mocenter-admin-pw", 120000, "4ddf74ea4f9089226143cd20f1d890fcffbb3877e7aa5ccde7c488dae3d8847e"},
		{"s3cret-pw", 120000, "8a6453ebe5deb9e7ab749bc1f768464fa2c4e09c9bcc4cd3175b6ee6f06c039c"},
	}
	for _, c := range cases {
		want := pbkdf2Prefix + strconv.Itoa(c.iter) + "$" + hex.EncodeToString(katSalt) + "$" + c.dk
		if got := pbkdf2Hash(c.pw, katSalt, c.iter); got != want {
			t.Errorf("pbkdf2Hash(%q, iter=%d) =\n  %q\n期望\n  %q", c.pw, c.iter, got, want)
		}
	}
}

func TestHashServerPasswordFormat(t *testing.T) {
	h, err := hashServerPassword("s3cret-pw")
	if err != nil {
		t.Fatalf("hashServerPassword: %v", err)
	}
	if !strings.HasPrefix(h, pbkdf2Prefix+strconv.Itoa(pbkdf2Iterations)+"$") {
		t.Fatalf("摘要格式不符: %q", h)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 5 {
		t.Fatalf("字段数 = %d，期望 5：%q", len(parts), h)
	}
	if len(parts[3]) != pbkdf2SaltLen*2 {
		t.Errorf("盐长度 = %d hex 位，期望 %d", len(parts[3]), pbkdf2SaltLen*2)
	}
	if len(parts[4]) != pbkdf2KeyLen*2 {
		t.Errorf("派生密钥长度 = %d hex 位，期望 %d", len(parts[4]), pbkdf2KeyLen*2)
	}
	// 同一次调用之间盐必须不同，否则相同口令得到相同摘要（横向可比）
	h2, _ := hashServerPassword("s3cret-pw")
	if h == h2 {
		t.Error("两次哈希得到同一盐：摘要可横向比对")
	}
}

func TestLooksLikeWeakHash(t *testing.T) {
	if !looksLikeWeakHash("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Error("64 位 hex 应判为旧版单轮 SHA-256")
	}
	if looksLikeWeakHash("pbkdf2$sha256$120000$00$11") {
		t.Error("新格式不应判为弱摘要")
	}
	if looksLikeWeakHash(strings.Repeat("z", 64)) {
		t.Error("非 hex 的 64 位串不应判为弱摘要")
	}
}
