package auth

import (
	"encoding/base32"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateTOTP(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP" // 常见测试密钥（base32）
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	now := time.Now().Unix() / 30
	code := totpAt(key, now)
	if code == "" || len(code) != 6 {
		t.Fatalf("bad code format: %q", code)
	}
	if !validateTOTP(secret, code, 1) {
		t.Fatal("valid code should pass validation")
	}
	if validateTOTP(secret, "000000", 1) {
		t.Fatal("wrong code should fail")
	}
	if validateTOTP(secret, "", 1) {
		t.Fatal("empty code should fail")
	}
	// 非法 secret
	if validateTOTP("not-base32!!", "123456", 1) {
		t.Fatal("invalid secret should fail")
	}
}

func TestUserManagerLogin(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "pass123", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !m.HasUsers() {
		t.Fatal("should have users")
	}
	// 未启用 OTP：无需验证码
	if err := m.Authenticate("admin", "pass123", ""); err != nil {
		t.Fatalf("auth should pass without otp: %v", err)
	}
	if err := m.Authenticate("admin", "wrong", ""); err != ErrBadLogin {
		t.Fatalf("wrong password should return ErrBadLogin, got %v", err)
	}
	if err := m.Authenticate("nobody", "pass123", ""); err != ErrBadLogin {
		t.Fatalf("unknown user should fail, got %v", err)
	}
}

func TestUserManagerOTPRequired(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "pass", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("init: %v", err)
	}
	// 启用了 OTP：缺失验证码应失败
	if err := m.Authenticate("admin", "pass", ""); err != ErrBadOTP {
		t.Fatalf("missing otp should fail with ErrBadOTP, got %v", err)
	}
	if err := m.Authenticate("admin", "pass", "000000"); err != ErrBadOTP {
		t.Fatalf("wrong otp should fail, got %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "oldpass1", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := m.ChangePassword("admin", "wrong", "newpass1", ""); err != ErrBadOldPassword {
		t.Fatalf("wrong old password should fail, got %v", err)
	}
	if err := m.ChangePassword("admin", "oldpass1", "123", ""); err != ErrBadNewPassword {
		t.Fatalf("short new password should fail, got %v", err)
	}
	if err := m.ChangePassword("admin", "oldpass1", "newpass1", ""); err != nil {
		t.Fatalf("change password failed: %v", err)
	}
	// 旧密码失效、新密码生效
	if err := m.Authenticate("admin", "oldpass1", ""); err != ErrBadLogin {
		t.Fatalf("old password should no longer work, got %v", err)
	}
	if err := m.Authenticate("admin", "newpass1", ""); err != nil {
		t.Fatalf("new password should work: %v", err)
	}
	// 重新加载文件后仍生效（持久化验证）
	m2 := NewUserManager(m.path)
	if err := m2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := m2.Authenticate("admin", "newpass1", ""); err != nil {
		t.Fatalf("persisted password should work: %v", err)
	}
}

func TestUserManagerEmpty(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if m.HasUsers() {
		t.Fatal("empty config should disable users")
	}
	if err := m.Authenticate("x", "y", ""); err != ErrBadLogin {
		t.Fatalf("empty users should fail login, got %v", err)
	}
}