package auth

import (
	"encoding/base32"
	"path/filepath"
	"strings"
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

// 同一验证码不能重放：否则截获者可以在 30 秒有效窗口内反复登录。
func TestUserManagerTOTPReplayRejected(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	secret := "JBSWY3DPEHPK3PXP"
	if err := m.InitFromFlags("admin", "pass", secret); err != nil {
		t.Fatalf("init: %v", err)
	}
	code := codeFor(t, secret)
	if err := m.Authenticate("admin", "pass", code); err != nil {
		t.Fatalf("first login with valid otp should pass: %v", err)
	}
	if err := m.Authenticate("admin", "pass", code); err != ErrBadOTP {
		t.Fatalf("replayed otp should be rejected, got %v", err)
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

// codeFor 计算指定 base32 密钥当前时间对应的 6 位验证码。
func codeFor(t *testing.T, secret string) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("decode secret %q: %v", secret, err)
	}
	return totpAt(key, time.Now().Unix()/30)
}

// InitFromFlags 在未提供密码时必须拒绝创建账号，避免产生「空密码」坏账号。
func TestInitFromFlagsRejectsEmptyPassword(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u.json")
	m := NewUserManager(p)
	if err := m.InitFromFlags("admin", "   ", ""); err != ErrInitPasswordRequired {
		t.Fatalf("empty password should be rejected, got %v", err)
	}
	if m.HasUsers() {
		t.Fatal("no user should be created on rejection")
	}
	// 已存在账号时，flag 被忽略（以文件为准）
	if err := m.InitFromFlags("admin", "realpass", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := m.InitFromFlags("other", "another", ""); err != nil {
		t.Fatalf("second init should be a no-op: %v", err)
	}
	if err := m.Authenticate("other", "another", ""); err != ErrBadLogin {
		t.Fatalf("second init must not add users, got %v", err)
	}
}

// 历史坏账号（空密码哈希）应能被识别并修复。
func TestBrokenAccountRepair(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u.json")
	m := NewUserManager(p)
	// 直接写入空密码哈希，模拟旧版本误建账号
	m.users["admin"] = &User{Username: "admin", PasswordHash: emptyPasswordHash}
	if err := m.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 未修复前任何密码都登录失败
	if err := m.Authenticate("admin", "123456", ""); err != ErrBadLogin {
		t.Fatalf("broken account should reject login, got %v", err)
	}
	if got := m.BrokenAccounts(); len(got) != 1 || got[0] != "admin" {
		t.Fatalf("BrokenAccounts = %v, want [admin]", got)
	}
	// 修复
	if err := m.SetPassword("admin", "123456"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if got := m.BrokenAccounts(); len(got) != 0 {
		t.Fatalf("after repair BrokenAccounts = %v, want empty", got)
	}
	if err := m.Authenticate("admin", "123456", ""); err != nil {
		t.Fatalf("repaired password should work: %v", err)
	}
	// 空新密码需被拒绝
	if err := m.SetPassword("admin", ""); err != ErrBadNewPassword {
		t.Fatalf("empty new password should be rejected, got %v", err)
	}
	// 不存在的用户
	if err := m.SetPassword("ghost", "x12345"); err != ErrBadUsername {
		t.Fatalf("unknown user should fail, got %v", err)
	}
	// 修复结果需持久化
	m2 := NewUserManager(p)
	if err := m2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := m2.Authenticate("admin", "123456", ""); err != nil {
		t.Fatalf("persisted repair should work: %v", err)
	}
}

// GenerateTOTPSecret 生成合法的 160bit base32（32 字符、无填充）。
func TestGenerateTOTPSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		s, err := GenerateTOTPSecret()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(s) != 32 {
			t.Fatalf("secret length = %d, want 32 (%q)", len(s), s)
		}
		if strings.ContainsAny(s, "=") {
			t.Fatalf("secret should have no padding: %q", s)
		}
		if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s); err != nil {
			t.Fatalf("secret is not valid base32: %q", s)
		}
		if seen[s] {
			t.Fatalf("duplicate secret generated: %q", s)
		}
		seen[s] = true
	}
}

// TOTPUri 生成可被 otpauth 解析的 URI，含 issuer / secret 且已转义。
func TestTOTPUri(t *testing.T) {
	uri := TOTPUri("MoCenter", "admin", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{
		"otpauth://totp/MoCenter:admin?",
		"secret=JBSWY3DPEHPK3PXP",
		"issuer=MoCenter",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Fatalf("uri %q missing %q", uri, want)
		}
	}
}

// 扫码 → 输入验证码 → 校验 → 绑定 的完整闭环。
func TestBindTOTPRoundTrip(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "pass123", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("gen secret: %v", err)
	}
	// 绑定前：OTP 未启用，登录无需验证码
	if enabled, exists := m.TOTPStatus("admin"); enabled || !exists {
		t.Fatalf("TOTPStatus = (%v,%v), want (false,true)", enabled, exists)
	}
	if _, exists := m.TOTPStatus("ghost"); exists {
		t.Fatal("unknown user should report exists=false")
	}
	// 错误的验证码不应通过自检、也不应绑定
	if m.VerifyTOTP("admin", secret, "000000") {
		t.Fatal("wrong code should not verify")
	}
	if m.VerifyTOTP("admin", "", "000000") {
		t.Fatal("empty secret should not verify")
	}
	code := codeFor(t, secret)
	if !m.VerifyTOTP("admin", secret, code) {
		t.Fatalf("code %q should verify against secret %q", code, secret)
	}
	// 绑定（带空格/小写应被规范化）
	if err := m.BindTOTP("admin", strings.ToLower("  "+secret+"  ")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if enabled, exists := m.TOTPStatus("admin"); !enabled || !exists {
		t.Fatalf("TOTPStatus after bind = (%v,%v), want (true,true)", enabled, exists)
	}
	if !m.EnabledOTP("admin") {
		t.Fatal("EnabledOTP should be true after bind")
	}
	// 绑定后登录必须提供验证码
	if err := m.Authenticate("admin", "pass123", ""); err != ErrBadOTP {
		t.Fatalf("login without otp should fail, got %v", err)
	}
	if err := m.Authenticate("admin", "pass123", codeFor(t, secret)); err != nil {
		t.Fatalf("login with valid otp should pass: %v", err)
	}
	// 绑定到不存在的用户
	if err := m.BindTOTP("ghost", secret); err != ErrBadUsername {
		t.Fatalf("bind unknown user should fail, got %v", err)
	}
	// 空密钥不可绑定
	if err := m.BindTOTP("admin", "   "); err != ErrBadOTP {
		t.Fatalf("empty secret bind should fail, got %v", err)
	}
}

// VerifyPassword 用于 OTP 重新生成/关闭前的二次确认，只认当前密码。
func TestVerifyPassword(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "pass123", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := m.VerifyPassword("admin", "pass123"); err != nil {
		t.Fatalf("正确密码应通过: %v", err)
	}
	if err := m.VerifyPassword("admin", "wrong"); err != ErrBadOldPassword {
		t.Fatalf("错误密码应返回 ErrBadOldPassword，实得 %v", err)
	}
	if err := m.VerifyPassword("ghost", "pass123"); err != ErrBadUsername {
		t.Fatalf("未知用户应返回 ErrBadUsername，实得 %v", err)
	}
}

// UnbindTOTP 需密码校验；已启用 OTP 时还需验证码。
func TestUnbindTOTP(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.InitFromFlags("admin", "pass123", ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("gen secret: %v", err)
	}
	if err := m.BindTOTP("admin", secret); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := m.UnbindTOTP("admin", "wrongpass", ""); err != ErrBadLogin {
		t.Fatalf("wrong password should fail, got %v", err)
	}
	if err := m.UnbindTOTP("admin", "pass123", "000000"); err != ErrBadOTP {
		t.Fatalf("wrong otp should fail, got %v", err)
	}
	if err := m.UnbindTOTP("ghost", "pass123", ""); err != ErrBadUsername {
		t.Fatalf("unknown user should fail, got %v", err)
	}
	if err := m.UnbindTOTP("admin", "pass123", codeFor(t, secret)); err != nil {
		t.Fatalf("unbind with valid otp: %v", err)
	}
	if m.EnabledOTP("admin") {
		t.Fatal("OTP should be disabled after unbind")
	}
	// 解绑后登录恢复免验证码
	if err := m.Authenticate("admin", "pass123", ""); err != nil {
		t.Fatalf("login after unbind should pass: %v", err)
	}
	// 持久化验证
	m2 := NewUserManager(m.path)
	if err := m2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m2.EnabledOTP("admin") {
		t.Fatal("unbind should persist")
	}
}

// UpsertUser 创建或更新账号，不覆盖已有非空 OTP 密钥。
func TestUpsertUser(t *testing.T) {
	m := NewUserManager(filepath.Join(t.TempDir(), "u.json"))
	if err := m.UpsertUser("admin", "first123", ""); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if !m.HasUsers() {
		t.Fatal("upsert should create user")
	}
	if err := m.UpsertUser("admin", "second123", ""); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if err := m.Authenticate("admin", "first123", ""); err != ErrBadLogin {
		t.Fatalf("old password should be replaced, got %v", err)
	}
	if err := m.Authenticate("admin", "second123", ""); err != nil {
		t.Fatalf("new password should work: %v", err)
	}
	// 空用户名 / 空密码需被拒绝
	if err := m.UpsertUser("  ", "x12345", ""); err != ErrBadUsername {
		t.Fatalf("empty username should fail, got %v", err)
	}
	if err := m.UpsertUser("bob", "", ""); err != ErrInitPasswordRequired {
		t.Fatalf("empty password should fail, got %v", err)
	}
	// 已存在用户：空 totpSecret 不应清除原密钥
	if err := m.BindTOTP("admin", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := m.UpsertUser("admin", "third123", ""); err != nil {
		t.Fatalf("upsert keep otp: %v", err)
	}
	if secret := m.users["admin"].TOTPSecret; secret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("existing TOTP secret should be preserved, got %q", secret)
	}
}
