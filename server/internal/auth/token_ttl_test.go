package auth

import (
	"testing"
	"time"
)

// TestSetTokenTTLClampsExistingSessions 缩短会话时长时必须同步收紧已有 Token。
// 否则把 30 天改成 1 小时后，旧会话仍能用到原来的 30 天 —— 配置改了却等于没改。
func TestSetTokenTTLClampsExistingSessions(t *testing.T) {
	a := NewAuth(nil)
	a.SetTokenTTL(30 * 24 * time.Hour)
	a.RegisterToken("tok", "admin")

	a.SetTokenTTL(time.Hour)
	a.mu.Lock()
	exp := a.tokens["tok"].expiresAt
	a.mu.Unlock()

	if exp.After(time.Now().Add(2 * time.Hour)) {
		t.Fatalf("缩短 TTL 后旧 Token 仍保留了过长有效期: %v", exp)
	}
	if !a.ValidateToken("tok") {
		t.Fatal("收紧后未过期的 Token 仍应有效")
	}
}

// TestSetTokenTTLDoesNotExtendExistingSessions 延长 TTL 不反向改写已有会话；
// 它们在下一次请求滑动续期时自然采用新值。
func TestSetTokenTTLDoesNotExtendExistingSessions(t *testing.T) {
	a := NewAuth(nil)
	a.SetTokenTTL(time.Hour)
	a.RegisterToken("tok", "admin")
	a.mu.Lock()
	before := a.tokens["tok"].expiresAt
	a.mu.Unlock()

	a.SetTokenTTL(24 * time.Hour)
	a.mu.Lock()
	after := a.tokens["tok"].expiresAt
	a.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("延长 TTL 不应改写现有到期时间: before=%v after=%v", before, after)
	}
}
