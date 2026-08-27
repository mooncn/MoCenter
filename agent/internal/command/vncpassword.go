// VNC 口令混淆（存储用）：TightVNC/TigerVNC/RealVNC 共享同一份 DES-ECB 混淆。
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
)

var vncStoreKey, _ = hex.DecodeString("e84ad660c4721ae0")

// vncObfuscatePassword 生成与 VNC 服务器存储一致的口令字节（8 字节）。
func vncObfuscatePassword(pw string) []byte {
	key := vncStoreKey
	block, err := des.NewCipher(key)
	if err != nil {
		return make([]byte, 8)
	}
	in := make([]byte, 8)
	copy(in, []byte(pw)) // 不足 8 字节以 0 补齐；超长截断
	out := make([]byte, 8)
	block.Encrypt(out, in)
	return out
}

// generateRandomPassword 生成用于 VNC 的随机口令（去易混淆字符，长度 12）。
func generateRandomPassword() string {
	const charset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "mocenter-vnc-2026"
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