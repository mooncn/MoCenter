package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
)

// 口令哈希：PBKDF2-HMAC-SHA256。
//
// 为什么不用原来的单轮无盐 SHA-256：它没有任何计算成本，GPU 每秒可试数十亿次，
// 且无盐导致彩虹表可用、相同口令哈希相同（横向可比）。加盐 + 迭代后单次校验代价
// 可调，能显著抬高离线爆破成本。
//
// 存储格式自描述，便于日后换算法：
//
//	pbkdf2$sha256$<迭代次数>$<salt hex>$<dk hex>
//
// 旧版单轮 SHA-256 的 64 位 hex 仍可校验（见 verifyPassword），并在登录成功后
// 透明升级为新格式，因此不需要强制用户改密。
//
// 实现只用标准库：不引入 golang.org/x/crypto，避免改动 server 模块的依赖与 Go 版本。
const (
	pbkdf2Iterations = 120000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
	pbkdf2Prefix     = "pbkdf2$sha256$"
)

// pbkdf2SHA256 计算 PBKDF2-HMAC-SHA256（RFC 8018 §5.2）。
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

// hashPassword 生成新格式的口令哈希。
func hashPassword(pw string) string {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		// 熵源故障时不应静默降级为弱哈希：返回一个必然无法通过校验的值，
		// 让调用方（创建/改密）失败可见，而不是生成一个"弱但可用"的口令。
		return pbkdf2Prefix + "0$" + strings.Repeat("0", pbkdf2SaltLen*2) + "$" + strings.Repeat("0", pbkdf2KeyLen*2)
	}
	dk := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return pbkdf2Prefix + strconv.Itoa(pbkdf2Iterations) + "$" +
		hex.EncodeToString(salt) + "$" + hex.EncodeToString(dk)
}

// dummyPasswordHash 用于“用户名不存在”时也执行一次同等成本的 PBKDF2。
//
// 直接返回 ErrBadLogin 会让未知用户名的响应明显更快，攻击者可据此枚举账号。
// 这里用固定盐与一段不对应任何账号的口令生成一个合法哈希，校验必然失败，
// 但耗时与真实账号一致。
var dummyPasswordHash = func() string {
	salt := make([]byte, pbkdf2SaltLen)
	for i := range salt {
		salt[i] = byte(i*13 + 7)
	}
	dk := pbkdf2SHA256([]byte("mocenter-timing-equalizer"), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return pbkdf2Prefix + strconv.Itoa(pbkdf2Iterations) + "$" +
		hex.EncodeToString(salt) + "$" + hex.EncodeToString(dk)
}()

// verifyPassword 校验口令。第二个返回值表示「存储的是旧格式哈希，建议升级」。
func verifyPassword(stored, pw string) (ok bool, needsUpgrade bool) {
	if stored == "" {
		return false, false
	}
	if strings.HasPrefix(stored, pbkdf2Prefix) {
		rest := strings.TrimPrefix(stored, pbkdf2Prefix)
		parts := strings.Split(rest, "$")
		if len(parts) != 3 {
			return false, false
		}
		iter, err := strconv.Atoi(parts[0])
		if err != nil || iter <= 0 {
			return false, false
		}
		salt, err1 := hex.DecodeString(parts[1])
		want, err2 := hex.DecodeString(parts[2])
		if err1 != nil || err2 != nil || len(want) == 0 {
			return false, false
		}
		got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
		if subtle.ConstantTimeCompare(got, want) == 1 {
			// 迭代次数低于当前标准时顺带升级
			return true, iter < pbkdf2Iterations
		}
		return false, false
	}
	// 兼容旧格式：单轮 SHA-256 的 64 位 hex（大小写不敏感）
	if len(stored) == 64 && isHex(stored) {
		sum := sha256.Sum256([]byte(pw))
		want := hex.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(stored)), []byte(want)) == 1 {
			return true, true
		}
	}
	return false, false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isStrongPassword 判断是否满足最低强度要求（>= 6 位）。
// 与原有校验保持一致，避免破坏既有用户的改密流程。
func isStrongPassword(pw string) bool {
	return len(pw) >= 6
}
