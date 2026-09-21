package auth

import (
	"strconv"
	"strings"
	"testing"
)

// 这组已知答案向量与 installer/password_test.go 完全相同，由 CPython
// hashlib.pbkdf2_hmac 独立产出。它锁住的是「安装器写的摘要服务端认得」这条跨模块契约：
// 两边是独立 module、不能互相 import，参数（前缀、迭代数、盐长、密钥长、编码）
// 一旦有一侧改动，另一侧的这个测试就会红。
const (
	katSaltHex = "0123456789abcdef0123456789abcdef"
	katIter    = 120000
	katPW      = "mocenter-admin-pw"
	katStored  = "pbkdf2$sha256$120000$0123456789abcdef0123456789abcdef$4ddf74ea4f9089226143cd20f1d890fcffbb3877e7aa5ccde7c488dae3d8847e"
)

func TestInstallerProducedHashVerifies(t *testing.T) {
	parts := strings.Split(katStored, "$")
	if len(parts) != 5 {
		t.Fatalf("安装器摘要应为 5 段（pbkdf2/sha256/迭代/盐/密钥），实得 %d 段：%q", len(parts), katStored)
	}
	if strings.Join(parts[:2], "$") != strings.TrimSuffix(pbkdf2Prefix, "$") {
		t.Errorf("算法标识段与本模块期望不符：%q", parts[:2])
	}
	if parts[2] != strconv.Itoa(katIter) {
		t.Errorf("迭代数段 = %q，期望 %d", parts[2], katIter)
	}
	if parts[3] != katSaltHex {
		t.Errorf("盐段 = %q，期望 %q", parts[3], katSaltHex)
	}
	if pbkdf2Iterations != katIter {
		t.Errorf("服务端标准迭代数已变更为 %d，安装器仍写 %d，需同步 installer/password.go", pbkdf2Iterations, katIter)
	}
	ok, needsUpgrade := verifyPassword(katStored, katPW)
	if !ok {
		t.Fatal("服务端无法校验安装器写出的 PBKDF2 摘要")
	}
	if needsUpgrade {
		t.Errorf("迭代数已是当前标准 %d，不该要求升级", pbkdf2Iterations)
	}
	if ok, _ := verifyPassword(katStored, katPW+"x"); ok {
		t.Error("错误口令通过了校验")
	}
}
