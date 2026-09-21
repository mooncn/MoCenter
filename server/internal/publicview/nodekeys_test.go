package publicview

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖 H-8：公开 node_id 必须是**带部署私钥**的派生值。
//
// 旧实现是 SHA-256("mocenter-public-node|" + agent_id) 截 4 字节 —— 人人可算，
// 匿名访客拿主机名候选离线哈希、用 samples 接口的 404/200 当判定器，
// 就能把"哪些内网主机在册"逐条问出来（且跨部署相同 → 可跟踪）。

// testKeys 是本包测试共用的部署私钥。
var testKeys = mustNodeKeys(fixedSecret(0x41))

// otherKeys 另一把部署私钥（代表"另一个部署"）。
var otherKeys = mustNodeKeys(fixedSecret(0x80))

func fixedSecret(b byte) []byte {
	secret := make([]byte, minNodeKeyBytes)
	for i := range secret {
		secret[i] = byte(int(b) + i%26)
	}
	return secret
}

func mustNodeKeys(secret []byte) *NodeKeys {
	k, err := NewNodeKeys(secret)
	if err != nil {
		panic(err)
	}
	return k
}

// TestNewNodeKeysRejectsShortKey 短密钥会让编号退回"可枚举"，必须在构造时就拒绝。
func TestNewNodeKeysRejectsShortKey(t *testing.T) {
	for _, s := range [][]byte{nil, {}, make([]byte, minNodeKeyBytes-1)} {
		if _, err := NewNodeKeys(s); err == nil {
			t.Errorf("%d 字节密钥应被拒绝", len(s))
		}
	}
	if _, err := NewNodeKeys(make([]byte, minNodeKeyBytes)); err != nil {
		t.Errorf("刚好达标的密钥应可用: %v", err)
	}
}

// TestNodeIDIsKeyDependent 同一 agent_id 在两个部署里必须得到不同编号。
//
// 这是"固定盐"与"部署私钥"的分水岭：旧实现在这条上必然失败。
func TestNodeIDIsKeyDependent(t *testing.T) {
	if testKeys.ID("web-01") == otherKeys.ID("web-01") {
		t.Fatal("不同部署密钥得到相同编号，等于没有密钥")
	}
}

// TestNodeIDNotReproducibleWithoutKey 没有密钥就复现不出编号（离线字典失去前提）。
func TestNodeIDNotReproducibleWithoutKey(t *testing.T) {
	id := testKeys.ID("db-prod")
	sum := sha256.Sum256([]byte("mocenter-public-node|db-prod"))
	rawDigest := sha256.Sum256([]byte("db-prod"))
	for _, candidate := range []string{
		"n-" + hex.EncodeToString(sum[:4]),
		"n-" + hex.EncodeToString(sum[:8]),
		"n-" + hex.EncodeToString(sum[:]),
		"n-" + hex.EncodeToString(rawDigest[:8]),
	} {
		if candidate == id {
			t.Fatalf("编号可由公开信息算出: %s", candidate)
		}
	}
	mac := hmac.New(sha256.New, fixedSecret(0x11))
	mac.Write([]byte("db-prod"))
	if "n-"+hex.EncodeToString(mac.Sum(nil)[:nodeIDBytes]) == id {
		t.Error("用了别的密钥却算出了正确编号")
	}
}

// TestNodeKeysMatch 反查口径与 ID 一致，且不匹配时必须为 false。
func TestNodeKeysMatch(t *testing.T) {
	id := testKeys.ID("host-7")
	if !testKeys.Match("host-7", id) {
		t.Fatal("自身编号应匹配")
	}
	if testKeys.Match("host-8", id) {
		t.Error("别的主机不应匹配该编号")
	}
	last := id[len(id)-1]
	tampered := id[:len(id)-1]
	if last == '0' {
		tampered += "1"
	} else {
		tampered += "0"
	}
	if testKeys.Match("host-7", tampered) {
		t.Error("改动过末位的编号不应匹配")
	}
	if otherKeys.Match("host-7", id) {
		t.Error("换一把密钥就不该再认出旧编号（轮换即整体失效）")
	}
}

// TestLoadNodeKeysCreatesThenReuses 首次生成落盘（0600），重启后读回同一把。
func TestLoadNodeKeysCreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), NodeKeyFileName)
	keys, created, err := LoadNodeKeys(path)
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if !created {
		t.Error("首次应报告 created=true，便于启动日志提示运维")
	}
	id := keys.ID("host-1")

	again, created2, err := LoadNodeKeys(path)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if created2 {
		t.Error("二次不应重新生成（编号会整体漂移）")
	}
	if again.ID("host-1") != id {
		t.Fatal("重启后编号变化：密钥没有稳定持久化")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if !isHexLine(string(data)) {
		t.Errorf("密钥文件应为 hex 文本，实际: %q", data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("生成过程残留了临时文件")
	}
	if os.PathSeparator == '\\' {
		t.Skip("Windows 上权限位不可信")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("密钥文件权限 = %o，不应允许同组/其他用户读取", perm)
	}
}

// TestLoadNodeKeysRejectsBrokenFile 文件存在但内容不可用时**报错而不是换一把**。
//
// 静默换钥匙会让全部公开编号凭空改变，看起来像"页面坏了"，
// 而不是"密钥文件被截断了" —— 后者才是运维需要知道的诊断。
func TestLoadNodeKeysRejectsBrokenFile(t *testing.T) {
	cases := map[string]string{
		"空文件":   "",
		"只含换行":  "\n",
		"非 hex": "zzzz\n",
		"长度不足":  hex.EncodeToString(make([]byte, minNodeKeyBytes-1)) + "\n",
		"含空白":   "  " + hex.EncodeToString(make([]byte, minNodeKeyBytes-8)) + "  ",
	}
	for name, content := range cases {
		path := filepath.Join(t.TempDir(), NodeKeyFileName)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("预置密钥文件失败: %v", err)
		}
		if _, _, err := LoadNodeKeys(path); err == nil {
			t.Errorf("%s：应报错而不是自动生成新密钥", name)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s：失败时不应动原文件: %v", name, err)
		}
	}
}

// TestLoadNodeKeysEmptyPath 路径为空时不能退化成"内存随机"（重启即换编号）。
func TestLoadNodeKeysEmptyPath(t *testing.T) {
	if _, _, err := LoadNodeKeys(""); err == nil {
		t.Fatal("路径为空应报错")
	}
}

// TestNodeKeysNilReceiverPanics 忘接密钥时 panic，而不是静默退回可预测派生。
func TestNodeKeysNilReceiverPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("nil 密钥应当场炸出接线错误")
		}
	}()
	var k *NodeKeys
	_ = k.ID("host-1")
}

func isHexLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
