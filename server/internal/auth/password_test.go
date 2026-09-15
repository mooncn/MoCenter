package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPBKDF2SHA256AgainstReference 用独立实现（Python hashlib.pbkdf2_hmac）算出的
// 期望值校验自研 PBKDF2。这比"自己算一遍再和自己比"有意义得多。
func TestPBKDF2SHA256AgainstReference(t *testing.T) {
	cases := []struct {
		pw       string
		saltHex  string
		iter     int
		dkLen    int
		want     string
	}{
		{"password", "73616c7473616c74", 1, 32,
			"3bec48c577653afdc2433b93a7d8ee8eae79895ed93bf627e97674c7f45f8a4c"},
		{"password", "73616c7473616c74", 4096, 32,
			"662303fd5b832012ccf2b4fd665164726f2f3b8e0f1f68edbe9b07eace29c2f5"},
		// 多块（dkLen > hLen）且与 RFC 6070 的 SHA-1 用例同形，验证块拼接正确
		{"passwordPASSWORDpassword",
			"73616c7453414c5473616c7453414c5473616c7453414c5473616c7453414c5473616c74", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
		// 非 ASCII（多字节 UTF-8）口令
		{"p@ss w0rd 中文", "0f1e2d3c4b5a69788796a5b4c3d2e1f0", 1000, 32,
			"8eb2030ab2462dfeca62efeb4acccf959cab8d3ffbc3dbfd92ef4853c0eebb6c"},
		// 边界：空口令、空盐、单次迭代
		{"", "", 1, 32,
			"f7ce0b653d2d72a4108cf5abe912ffdd777616dbbb27a70e8204f3ae2d0f6fad"},
	}
	for _, c := range cases {
		salt, err := hex.DecodeString(c.saltHex)
		if err != nil {
			t.Fatalf("salt hex 非法: %v", err)
		}
		got := hex.EncodeToString(pbkdf2SHA256([]byte(c.pw), salt, c.iter, c.dkLen))
		if got != c.want {
			t.Errorf("PBKDF2(pw=%q,iter=%d,dkLen=%d)\n got %s\nwant %s",
				c.pw, c.iter, c.dkLen, got, c.want)
		}
	}
}

// TestPBKDF2EdgeCases 非法参数不应 panic。
func TestPBKDF2EdgeCases(t *testing.T) {
	for _, c := range []struct{ iter, dkLen int }{{0, 32}, {-1, 32}, {1, 0}, {1, -1}} {
		if got := pbkdf2SHA256([]byte("x"), []byte("y"), c.iter, c.dkLen); got != nil {
			t.Errorf("iter=%d dkLen=%d 期望 nil，得到 %d 字节", c.iter, c.dkLen, len(got))
		}
	}
}

// TestHashPasswordRoundTrip 新格式哈希可校验，且格式自描述。
func TestHashPasswordRoundTrip(t *testing.T) {
	for _, pw := range []string{"123456", "a", strings.Repeat("x", 200), "p@ss w0rd 中文"} {
		h := hashPassword(pw)
		if !strings.HasPrefix(h, pbkdf2Prefix) {
			t.Fatalf("哈希缺少前缀: %q", h)
		}
		ok, upgrade := verifyPassword(h, pw)
		if !ok {
			t.Errorf("口令 %q 应校验通过，哈希=%q", pw, h)
		}
		if upgrade {
			t.Errorf("新格式哈希不应要求升级: %q", h)
		}
		if ok, _ := verifyPassword(h, pw+"x"); ok {
			t.Errorf("错误口令不应通过: %q", pw+"x")
		}
	}
}

// TestHashPasswordSalted 相同口令每次哈希不同（盐随机），且不含明文。
func TestHashPasswordSalted(t *testing.T) {
	a, b := hashPassword("same-password"), hashPassword("same-password")
	if a == b {
		t.Fatal("相同口令的两次哈希相同 —— 说明没有加盐")
	}
	if ok, _ := verifyPassword(a, "same-password"); !ok {
		t.Fatal("a 应校验通过")
	}
	if ok, _ := verifyPassword(b, "same-password"); !ok {
		t.Fatal("b 应校验通过")
	}
}

// TestVerifyLegacySHA256 旧版单轮 sha256 仍可校验，且标记为需升级。
func TestVerifyLegacySHA256(t *testing.T) {
	pw := "legacy-pass"
	sum := sha256.Sum256([]byte(pw))
	legacy := hex.EncodeToString(sum[:])

	ok, upgrade := verifyPassword(legacy, pw)
	if !ok {
		t.Fatal("旧格式哈希应仍可校验（否则老用户全部无法登录）")
	}
	if !upgrade {
		t.Fatal("旧格式哈希应标记为需升级")
	}
	if ok, _ := verifyPassword(legacy, "wrong"); ok {
		t.Fatal("错误口令不应通过")
	}
	// 大写 hex 也应接受
	if ok, _ := verifyPassword(strings.ToUpper(legacy), pw); !ok {
		t.Fatal("大写 hex 的旧哈希应可校验")
	}
}

// TestVerifyRejectsMalformed 畸形哈希不应误判为通过。
func TestVerifyRejectsMalformed(t *testing.T) {
	for _, h := range []string{
		"", "abc", strings.Repeat("0", 63), strings.Repeat("0", 65),
		"pbkdf2$sha256$", "pbkdf2$sha256$abc$x$y",
		"pbkdf2$sha256$0$00$00", "pbkdf2$sha256$-1$00$00",
		"pbkdf2$sha256$1000$zz$00", "pbkdf2$sha256$1000$00$zz",
		"pbkdf2$md5$1000$00$00", "PBKDF2$SHA256$1000$00$00",
	} {
		if ok, _ := verifyPassword(h, "anything"); ok {
			t.Errorf("畸形哈希 %q 不应校验通过", h)
		}
	}
}

// TestEmptyPasswordHashDetection 旧版空密码账号仍能被识别出来（用于修复流程）。
func TestEmptyPasswordHashDetection(t *testing.T) {
	sum := sha256.Sum256(nil)
	if hex.EncodeToString(sum[:]) != emptyPasswordHash {
		t.Fatal("emptyPasswordHash 常量与 sha256(\"\") 不一致")
	}
	if !passwordIsEmptyHash(emptyPasswordHash) {
		t.Fatal("应识别为空密码哈希")
	}
	if passwordIsEmptyHash(hashPassword("123456")) {
		t.Fatal("新格式哈希不应被误判为空密码哈希")
	}
}

// TestLowerIterationsUpgradedWhenIterationCountTooLow 迭代次数低于当前标准的旧 pbkdf2 哈希应提示升级。
func TestLowerIterationsUpgradedWhenIterationCountTooLow(t *testing.T) {
	salt, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	dk := pbkdf2SHA256([]byte("pw"), salt, 1000, pbkdf2KeyLen)
	stored := pbkdf2Prefix + "1000$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(dk)
	ok, upgrade := verifyPassword(stored, "pw")
	if !ok {
		t.Fatal("低迭代次数的 pbkdf2 哈希应可校验")
	}
	if !upgrade {
		t.Fatal("迭代次数低于当前标准时应提示升级")
	}
}
