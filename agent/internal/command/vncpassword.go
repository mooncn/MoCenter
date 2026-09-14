// VNC 口令混淆（存储用）：TightVNC/TigerVNC/UltraVNC/RealVNC 共享同一份 DES-ECB 混淆。
//
// 存储口令 = DES-ECB( 口令补齐到8字节, 固定密钥 0xe84ad660c4721ae0 )
// （该固定密钥为 d3des 内部排列后的有效 DES 密钥，见 TigerVNC common/rfb/obfuscate.cxx）
package command

import (
	"crypto/des"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var vncStoreKey, _ = hex.DecodeString("e84ad660c4721ae0")

// vncObfuscatePassword 生成与 VNC 服务器存储一致的口令字节（8 字节）。
// VNC 口令最长 8 字节：超长必须截断，否则存储侧（TightVNC/TigerVNC）与
// 客户端各自截断后可能不一致，导致 Authentication failed。
func vncObfuscatePassword(pw string) []byte {
	key := vncStoreKey
	block, err := des.NewCipher(key)
	if err != nil {
		return make([]byte, 8)
	}
	in := make([]byte, 8)
	copy(in, []byte(pw)) // 不足 8 字节以 0 补齐；超长截断为前 8 字节
	out := make([]byte, 8)
	block.Encrypt(out, in)
	return out
}

// generateRandomPassword 生成用于 VNC 的随机口令（去易混淆字符，长度 8=VNC 上限）。
func generateRandomPassword() string {
	const charset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "mocenter"[:8]
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// writeVncPasswdFile 创建 Linux VNC 口令文件（x11vnc/tigervnc 均读取该格式）。
// 文件内容即 8 字节的 DES 混淆口令，无需依赖 vncpasswd 工具。
func writeVncPasswdFile(path, pw string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data := vncObfuscatePassword(pw)
	if len(data) != 8 {
		return errors.New("vnc 口令混淆失败")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return nil
}

// privateProfileStructValue 生成 Win32 WritePrivateProfileStruct 写入 INI 的字符串值。
//
// 格式：大写 HEX(数据) 之后追加 2 位大写 HEX 校验和（数据所有字节之和 mod 256）。
// 例：24 字节结构体 → 48 位 HEX + 2 位校验和 = 50 个字符。
//
// 为什么需要它：UltraVNC 用 GetPrivateProfileStruct 读取 ultravnc.ini 的 passwd，
// 该 API 会重新计算校验和并与写入值比对，不一致即判定数据损坏、直接丢弃，
// 表现为「口令写入成功但认证始终失败」。因此必须精确复现该编码，
// 不能只写 16 位 HEX（那样校验和对不上）。
func privateProfileStructValue(data []byte) string {
	const hexDigits = "0123456789ABCDEF"
	var sb strings.Builder
	sb.Grow(len(data)*2 + 2)
	var sum byte
	for _, b := range data {
		sb.WriteByte(hexDigits[b>>4])
		sb.WriteByte(hexDigits[b&0x0f])
		sum += b // 字节相加自动按 256 取模
	}
	sb.WriteByte(hexDigits[sum>>4])
	sb.WriteByte(hexDigits[sum&0x0f])
	return sb.String()
}
