package auth

import (
	"testing"
)

// TestTokenValidation 校验「只认登录后颁发的 Token」这一唯一凭证模型。
// 全局 API Key 已移除：任何未被 RegisterToken 登记过的字符串都必须被拒绝。
func TestTokenValidation(t *testing.T) {
	a := NewAuth(nil)
	if a.ValidateToken("secret") {
		t.Fatal("未登记的 Token 必须被拒绝（API Key 已移除）")
	}
	if a.ValidateToken("") {
		t.Fatal("空 Token 必须被拒绝")
	}
	if a.ValidateToken("wrong") {
		t.Fatal("错误的 Token 应被拒绝")
	}
	a.RegisterToken("tok1", "admin")
	if !a.ValidateToken("tok1") {
		t.Fatal("已登记的 Token 应通过校验")
	}
	if op := a.OperatorOf("tok1"); op != "admin" {
		t.Fatalf("期望操作者 admin，实际 %q", op)
	}
	if op := a.OperatorOf("secret"); op != "unknown" {
		t.Fatalf("未登记 Token 的操作者应为 unknown，实际 %q", op)
	}
	a.RevokeToken("tok1")
	if a.ValidateToken("tok1") {
		t.Fatal("已吊销的 Token 应被拒绝")
	}
}

func TestIPWhitelist(t *testing.T) {
	a := NewAuth([]string{"10.0.0.0/8", "127.0.0.1"})
	if !a.IsAllowedIP("10.1.2.3:9000") {
		t.Fatal("cidr ip should be allowed")
	}
	if !a.IsAllowedIP("127.0.0.1:1234") {
		t.Fatal("exact ip should be allowed")
	}
	if a.IsAllowedIP("8.8.8.8:80") {
		t.Fatal("non-whitelisted ip should be rejected")
	}
}

func TestEmptyWhitelistAllowsAll(t *testing.T) {
	a := NewAuth(nil)
	if !a.IsAllowedIP("1.2.3.4:9000") {
		t.Fatal("empty whitelist should allow all")
	}
}

func TestRateLimit(t *testing.T) {
	a := NewAuth(nil)
	a.rateMax = 5
	allowed := 0
	for i := 0; i < 8; i++ {
		if a.AllowRequest("9.9.9.9:1") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("expected 5 allowed requests, got %d", allowed)
	}
}