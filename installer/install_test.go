//go:build windows

package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEnsureServerAccountCreatesHashedAccount 覆盖安装器写出 users.json 的两条契约：
// 结构与 server/internal/auth/user.go 期望一致，且口令只以加盐迭代的 PBKDF2 摘要落盘
// （绝不能出现明文，也不能退回旧版的单轮无盐 SHA-256）。
// 其中的口令是合成测试向量，不对应任何真实凭据。
func TestEnsureServerAccountCreatesHashedAccount(t *testing.T) {
	dir := t.TempDir()
	created, err := ensureServerAccount(dir, "admin", "s3cret-pw")
	if err != nil {
		t.Fatalf("ensureServerAccount: %v", err)
	}
	if !created {
		t.Fatal("首次调用应返回 created=true")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("读取 users.json: %v", err)
	}
	t.Logf("users.json = %s", string(raw))

	// 明文密码绝不能出现在文件里
	if string(raw) == "" {
		t.Fatal("文件为空")
	}
	if contains(string(raw), "s3cret-pw") {
		t.Fatal("users.json 中出现了明文密码")
	}

	// 结构与 server 侧 {Users map[string]*User `json:"users"`} 对齐
	var payload struct {
		Users map[string]struct {
			Username     string `json:"username"`
			PasswordHash string `json:"password_hash"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析 users.json: %v", err)
	}
	u, ok := payload.Users["admin"]
	if !ok {
		t.Fatalf("users 下缺少 admin 键，实际 %v", payload.Users)
	}
	if u.Username != "admin" {
		t.Errorf("username = %q，期望 admin", u.Username)
	}
	// 摘要必须是 PBKDF2（H-9）：单轮无盐 SHA-256 零成本可爆破、且同口令同摘要。
	if !strings.HasPrefix(u.PasswordHash, pbkdf2Prefix+strconv.Itoa(pbkdf2Iterations)+"$") {
		t.Fatalf("password_hash 不是 PBKDF2 格式: %q", u.PasswordHash)
	}
	parts := strings.Split(u.PasswordHash, "$")
	salt, derr := hex.DecodeString(parts[3])
	if derr != nil {
		t.Fatalf("盐段不是 hex: %v", derr)
	}
	if got := pbkdf2Hash("s3cret-pw", salt, pbkdf2Iterations); got != u.PasswordHash {
		t.Errorf("摘要与口令对不上：用同一盐重算得 %q，落盘 %q", got, u.PasswordHash)
	}
}

// TestEnsureServerAccountKeepsExisting 重装/升级不得覆盖用户后来改过的密码。
func TestEnsureServerAccountKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	if _, err := ensureServerAccount(dir, "admin", "first-pw"); err != nil {
		t.Fatalf("首次创建: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "users.json"))

	created, err := ensureServerAccount(dir, "admin", "second-pw")
	if err != nil {
		t.Fatalf("重复调用: %v", err)
	}
	if created {
		t.Error("已存在账号时应返回 created=false")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	if string(before) != string(after) {
		t.Error("已存在账号时 users.json 不应被改动")
	}
	if contains(string(after), "second-pw") {
		t.Error("不应写入第二次调用传入的密码")
	}
}

func TestEnsureServerAccountRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := ensureServerAccount(dir, "", "pw"); err == nil {
		t.Error("空用户名应报错")
	}
	if _, err := ensureServerAccount(dir, "admin", ""); err == nil {
		t.Error("空密码应报错")
	}
	if _, err := ensureServerAccount(dir, "  ", "pw"); err == nil {
		t.Error("全空白用户名应报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "users.json")); !os.IsNotExist(err) {
		t.Error("校验失败时不应创建文件")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
