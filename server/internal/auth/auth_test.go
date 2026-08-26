package auth

import (
	"testing"
)

func TestTokenValidation(t *testing.T) {
	a := NewAuth("secret", nil)
	if !a.ValidateToken("secret") {
		t.Fatal("api key should validate")
	}
	if a.ValidateToken("wrong") {
		t.Fatal("wrong token should be rejected")
	}
	a.RegisterToken("tok1")
	if !a.ValidateToken("tok1") {
		t.Fatal("registered token should validate")
	}
	a.RevokeToken("tok1")
	if a.ValidateToken("tok1") {
		t.Fatal("revoked token should be rejected")
	}
}

func TestIPWhitelist(t *testing.T) {
	a := NewAuth("s", []string{"10.0.0.0/8", "127.0.0.1"})
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
	a := NewAuth("s", nil)
	if !a.IsAllowedIP("1.2.3.4:9000") {
		t.Fatal("empty whitelist should allow all")
	}
}

func TestRateLimit(t *testing.T) {
	a := NewAuth("s", nil)
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