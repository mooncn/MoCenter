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

// TestLoadAgentKeyFromFile 覆盖 -agent-key-file 的三条要害：
// 文件优先于命令行给出的值、空文件不算成功、读完必须删掉落盘的凭据。
func TestLoadAgentKeyFromFile(t *testing.T) {
	t.Run("文件值覆盖旗标值并删除文件", func(t *testing.T) {
		dir := t.TempDir()
		pf := filepath.Join(dir, "ak.tmp")
		if err := os.WriteFile(pf, []byte("  TOK-from-file  \n"), 0600); err != nil {
			t.Fatalf("写凭据文件失败: %v", err)
		}
		o := options{agentKey: "TOK-from-cmdline", agentKeyFile: pf}
		if err := loadAgentKeyFromFile(&o); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if o.agentKey != "TOK-from-file" {
			t.Errorf("agentKey = %q，期望去空白后的文件内容", o.agentKey)
		}
		if _, err := os.Stat(pf); !os.IsNotExist(err) {
			t.Error("凭据文件读完没删，磁盘上多留了一份可读凭据")
		}
	})

	t.Run("空文件报错且不覆盖既有值", func(t *testing.T) {
		pf := filepath.Join(t.TempDir(), "empty.tmp")
		if err := os.WriteFile(pf, []byte(" \t\n"), 0600); err != nil {
			t.Fatalf("写空凭据文件失败: %v", err)
		}
		o := options{agentKey: "keepme", agentKeyFile: pf}
		if err := loadAgentKeyFromFile(&o); err == nil {
			t.Fatal("空凭据文件应当报错：静默当作「没有密钥」会让安装失败得更难懂")
		}
		if o.agentKey != "keepme" {
			t.Errorf("agentKey 被空文件改成了 %q", o.agentKey)
		}
	})

	t.Run("文件不存在报错", func(t *testing.T) {
		o := options{agentKeyFile: filepath.Join(t.TempDir(), "nope.tmp")}
		if err := loadAgentKeyFromFile(&o); err == nil {
			t.Fatal("读不到凭据文件必须报错，不能退回空密钥")
		}
	})

	t.Run("未给该旗标时不动 agentKey", func(t *testing.T) {
		o := options{agentKey: "only-cmdline"}
		if err := loadAgentKeyFromFile(&o); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if o.agentKey != "only-cmdline" {
			t.Errorf("agentKey = %q，应保持原值", o.agentKey)
		}
	})
}

// TestResolveAdminPass 覆盖 Server 口令入口收口（H-9 残留 (i)）的三条要害：
// 命令行明文必须被拒、stdin 读一行能落进 adminPass、stdin 空行不算成功。
func TestResolveAdminPass(t *testing.T) {
	t.Run("拒绝 -password 明文", func(t *testing.T) {
		o := options{adminPass: "leaked-via-cmdline"}
		err := resolveAdminPass(&o, strings.NewReader(""))
		if err == nil {
			t.Fatal("明文口令必须被拒绝：安装进程的命令行对同机任意进程可见")
		}
		if !contains(err.Error(), "-password-file") {
			t.Errorf("报错应指路 -password-file / -password-stdin，实际: %v", err)
		}
	})

	t.Run("stdin 读一行", func(t *testing.T) {
		o := options{adminPassStdin: true}
		if err := resolveAdminPass(&o, strings.NewReader("pw-from-stdin\n")); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if o.adminPass != "pw-from-stdin" {
			t.Errorf("adminPass = %q，期望去掉行尾换行后的内容", o.adminPass)
		}
	})

	t.Run("stdin 兼容 CRLF 与首尾空白", func(t *testing.T) {
		o := options{adminPassStdin: true}
		if err := resolveAdminPass(&o, strings.NewReader("  padded-pw  \r\n")); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if o.adminPass != "padded-pw" {
			t.Errorf("adminPass = %q，期望去 CRLF 与首尾空白后的内容", o.adminPass)
		}
	})

	t.Run("stdin 空行报错", func(t *testing.T) {
		o := options{adminPassStdin: true}
		if err := resolveAdminPass(&o, strings.NewReader("\n")); err == nil {
			t.Fatal("空行口令应报错，不能静默当作成功")
		}
	})

	t.Run("未给任何口令时不报错（交给 doInstall 校验）", func(t *testing.T) {
		o := options{}
		if err := resolveAdminPass(&o, strings.NewReader("")); err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if o.adminPass != "" {
			t.Errorf("adminPass 被改成 %q", o.adminPass)
		}
	})
}

// TestWriteConfigUpgradeMergesExplicitInputs 覆盖"覆盖安装修正 Agent 身份"这条路径：
// 升级时必须把本次显式输入的 agent_id 与令牌写进去，而不是原样保留旧配置 ——
// 否则 1.0.2 装出来的"无 agent_id + 令牌写两处"永远改不过来。
func TestWriteConfigUpgradeMergesExplicitInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mocenter-agent.json")
	old := `{
  "server": "old:9000",
  "daemon": true,
  "enrollment_secret": "old-token",
  "agent_key": "old-token",
  "retry": 7
}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	o := options{
		role: "agent", server: "192.168.117.1:9000", agentID: "vm-win10",
		agentKey: "new-token", agentKeyFile: `C:\Temp\mc-agent-key`,
	}
	if err := writeConfig("agent", path, o); err != nil {
		t.Fatalf("升级写配置失败: %v", err)
	}
	var got map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["server"] != "192.168.117.1:9000" {
		t.Errorf("server = %v，期望应用本次输入", got["server"])
	}
	if got["agent_id"] != "vm-win10" {
		t.Errorf("agent_id = %v，期望应用本次输入", got["agent_id"])
	}
	if got["enrollment_secret"] != "new-token" {
		t.Errorf("enrollment_secret = %v，期望更新为本次令牌", got["enrollment_secret"])
	}
	if _, ok := got["agent_key"]; ok {
		t.Errorf("旧配置里与 enrollment_secret 同值的 agent_key 应被清掉，实际 %v", got["agent_key"])
	}
	if got["retry"] != float64(7) {
		t.Errorf("未识别的既有字段应原样保留，retry = %v", got["retry"])
	}
}

// TestWriteConfigUpgradeKeepsDistinctSharedKey 升级不得误删真正的共享密钥：
// 只有与旧 enrollment_secret 同值的那份才是一次性令牌的重复拷贝。
func TestWriteConfigUpgradeKeepsDistinctSharedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mocenter-agent.json")
	old := `{"server":"old:9000","enrollment_secret":"old-token","agent_key":"shared-key"}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	o := options{server: "old:9000", agentKey: "new-token", agentKeyFile: "x"}
	if err := writeConfig("agent", path, o); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &got)
	if got["agent_key"] != "shared-key" {
		t.Errorf("agent_key = %v，真正的共享密钥必须保留", got["agent_key"])
	}
	if got["enrollment_secret"] != "new-token" {
		t.Errorf("enrollment_secret = %v，期望更新", got["enrollment_secret"])
	}
}

// TestWriteConfigUpgradeWithoutKey 升级可以不带任何凭据：此时不得报错，也不得清空既有值。
func TestWriteConfigUpgradeWithoutKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mocenter-agent.json")
	old := `{"server":"old:9000","enrollment_secret":"old-token","agent_id":"vm-win10"}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig("agent", path, options{}); err != nil {
		t.Fatalf("无凭据升级应允许: %v", err)
	}
	var got map[string]any
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &got)
	if got["enrollment_secret"] != "old-token" || got["agent_id"] != "vm-win10" || got["server"] != "old:9000" {
		t.Errorf("既有配置被改动: %v", got)
	}
}

// TestResetAgentServerCertsKeepsKey agent_id 变更时只清服务端签发的证书，私钥必须留下。
func TestResetAgentServerCertsKeepsKey(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds")
	if err := os.MkdirAll(creds, 0700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"client.crt", "client.key", "ca.crt"} {
		if err := os.WriteFile(filepath.Join(creds, n), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := resetAgentServerCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("删除数 = %d，期望 2（client.crt/ca.crt）", n)
	}
	if _, err := os.Stat(filepath.Join(creds, "client.key")); err != nil {
		t.Fatal("client.key 必须保留，否则 TOFU 绑定会挡住重新签发")
	}
}
