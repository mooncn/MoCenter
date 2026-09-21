//go:build windows

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// 安装器写出的后台账号摘要必须与服务端 auth.verifyPassword 认的格式一致。
//
// 为什么安装器自己实现一份：agent / server / installer 是三个互不引用的独立 module
// （见各自 go.mod），跨模块 import 会把安装器和服务端的构建绑死，属既有取舍；
// 因此这里照抄 server/internal/auth/password.go 的算法与参数，并靠
// installer/password_test.go 与 server/internal/auth/password_format_test.go 里的
// **同一组已知答案向量**成对锁住 —— 任一侧改了参数，另一侧的测试就会红。
//
// 为什么不再写单轮无盐 SHA-256（H-9 的一条）：那是零成本哈希，GPU 每秒可试数十亿次，
// 无盐还让相同口令得到相同摘要（横向可比）。users.json 一旦被读到（在收紧 ACL 之前
// 这是常态），弱口令当场就没了。服务端虽然会在首次登录时透明升级旧摘要，但「装完之后、
// 首次登录之前」这段时间仍然完全暴露，而这段时间往往正是最该有防护的部署窗口。
const (
	pbkdf2Iterations = 120000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
	pbkdf2Prefix     = "pbkdf2$sha256$"
)

// hashServerPassword 生成新格式摘要：pbkdf2$sha256$<迭代>$<salt hex>$<dk hex>。
func hashServerPassword(pw string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		// 熵源故障不能静默降级成弱哈希：宁可安装失败，也不要留下一个"看着能用"的弱凭据。
		return "", errors.New("生成口令盐失败: " + err.Error())
	}
	return pbkdf2Hash(pw, salt, pbkdf2Iterations), nil
}

// pbkdf2Hash 用给定盐与迭代数计算摘要（独立出来便于测试固定盐复现）。
func pbkdf2Hash(pw string, salt []byte, iter int) string {
	dk := pbkdf2SHA256([]byte(pw), salt, iter, pbkdf2KeyLen)
	return pbkdf2Prefix + strconv.Itoa(iter) + "$" +
		hex.EncodeToString(salt) + "$" + hex.EncodeToString(dk)
}

// pbkdf2SHA256 计算 PBKDF2-HMAC-SHA256（RFC 8018 §5.2），只用标准库。
//
//	DK = T1 || T2 || ... || Tn
//	Ti = U1 xor U2 xor ... xor Uc
//	U1 = PRF(P, S || INT_32_BE(i))    Uj = PRF(P, U(j-1))
func pbkdf2SHA256(password, salt []byte, iter, dkLen int) []byte {
	if iter <= 0 || dkLen <= 0 {
		return nil
	}
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (dkLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)

	buf := make([]byte, 4)
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	for i := 1; i <= blocks; i++ {
		buf[0] = byte(i >> 24)
		buf[1] = byte(i >> 16)
		buf[2] = byte(i >> 8)
		buf[3] = byte(i)
		prf.Reset()
		prf.Write(salt)
		prf.Write(buf)
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:dkLen]
}

// looksLikeWeakHash 判断摘要是否是本安装器**旧版本**写下的单轮 SHA-256（64 位 hex）。
// 升级时用它给出一次明确的整改提示，而不是悄悄替用户改文件。
func looksLikeWeakHash(stored string) bool {
	if len(stored) != 64 || strings.HasPrefix(stored, pbkdf2Prefix) {
		return false
	}
	for i := 0; i < len(stored); i++ {
		c := stored[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
