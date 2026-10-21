package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/auth"
)

// 本文件锚住「OTP 重新生成/关闭必须验证当前密码」这三条入口：
//   - /api/otp/setup：已启用时重新生成密钥
//   - /api/otp/enable：已启用时再次绑定（绕过前端直接调 API 的路径）
//   - /api/otp/disable：关闭
//
// 只凭会话 Token 就能换掉二次验证，等于把「Token 泄露」升级成「永久接管账号」。

func otpGuardFixture(t *testing.T) (*auth.UserManager, *auth.Auth, string) {
	t.Helper()
	um := auth.NewUserManager(filepath.Join(t.TempDir(), "users.json"))
	if err := um.InitFromFlags("admin", "pass123", ""); err != nil {
		t.Fatalf("初始化用户失败: %v", err)
	}
	az := auth.NewAuth(nil)
	az.RegisterToken("otp-test-token", "admin")
	return um, az, "otp-test-token"
}

func otpGuardRequest(t *testing.T, h http.HandlerFunc, method, token string, body interface{}) map[string]interface{} {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, "/api/otp", nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		req = httptest.NewRequest(method, "/api/otp", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %q", w.Body.String())
	}
	return out
}

// totpCodeForTest 生成当前时间步的 6 位验证码（与 auth 包的算法一致）。
func totpCodeForTest(t *testing.T, secret string) string {
	t.Helper()
	secret = strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("解析测试密钥失败: %v", err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(time.Now().Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (int(sum[off]&0x7f) << 24) | (int(sum[off+1]) << 16) | (int(sum[off+2]) << 8) | int(sum[off+3])
	return fmt.Sprintf("%06d", v%1000000)
}

func TestOTPRegenerateRequiresPassword(t *testing.T) {
	um, az, tok := otpGuardFixture(t)
	oldSecret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := um.BindTOTP("admin", oldSecret); err != nil {
		t.Fatalf("启用 OTP 失败: %v", err)
	}
	h := otpSetupHandler(um, az)

	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{}); got["ok"] != false {
		t.Fatalf("未带密码的重新生成应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{"password": "wrong"}); got["ok"] != false {
		t.Fatalf("错误密码的重新生成应被拒绝: %v", got)
	}
	got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{"password": "pass123"})
	if got["ok"] != true || got["secret"] == nil || got["secret"] == "" {
		t.Fatalf("正确密码应返回新密钥: %v", got)
	}
}

func TestOTPEnableRebindRequiresPassword(t *testing.T) {
	um, az, tok := otpGuardFixture(t)
	oldSecret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := um.BindTOTP("admin", oldSecret); err != nil {
		t.Fatalf("启用 OTP 失败: %v", err)
	}
	newSecret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	h := otpEnableHandler(um, az)

	// 绕过前端直接调用 enable：已启用时必须同样要求密码
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"secret": newSecret, "code": totpCodeForTest(t, newSecret),
	}); got["ok"] != false {
		t.Fatalf("未带密码的重新绑定应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"secret": newSecret, "code": totpCodeForTest(t, newSecret), "password": "wrong",
	}); got["ok"] != false {
		t.Fatalf("错误密码的重新绑定应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"secret": newSecret, "code": totpCodeForTest(t, newSecret), "password": "pass123",
	}); got["ok"] != true {
		t.Fatalf("正确密码 + 验证码应完成替换: %v", got)
	}
}

func TestOTPDisableRequiresPasswordAndCode(t *testing.T) {
	um, az, tok := otpGuardFixture(t)
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := um.BindTOTP("admin", secret); err != nil {
		t.Fatalf("启用 OTP 失败: %v", err)
	}
	h := otpDisableHandler(um, az)

	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"otp": totpCodeForTest(t, secret),
	}); got["ok"] != false {
		t.Fatalf("未带密码的关闭应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"password": "wrong", "otp": totpCodeForTest(t, secret),
	}); got["ok"] != false {
		t.Fatalf("错误密码的关闭应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"password": "pass123",
	}); got["ok"] != false {
		t.Fatalf("缺少动态验证码的关闭应被拒绝: %v", got)
	}
	if got := otpGuardRequest(t, h, http.MethodPost, tok, map[string]string{
		"password": "pass123", "otp": totpCodeForTest(t, secret),
	}); got["ok"] != true {
		t.Fatalf("正确密码 + 验证码应关闭 OTP: %v", got)
	}
	if um.EnabledOTP("admin") {
		t.Fatal("关闭后 OTP 应处于未启用状态")
	}
}
