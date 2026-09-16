package agentauth

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// buildPinTLSConfig 构造"公钥指纹固定"的 TLS 配置。
//
// 用途：自签证书环境下，让 Agent 在不依赖系统根、也不需要 -insecure 的前提下
// 校验服务端身份 —— 服务端证书的**公钥指纹**必须与预期一致。
//
// 为什么这样比"CA 校验 + SAN 匹配"更省事：
//   - 自签部署里 SAN 必须写死服务端真实 IP/域名，服务器换 IP、加 CDN、走内网地址
//     都会导致 x509 校验失败（这正是运维被迫改用 -insecure 的原因，等于关掉校验）；
//   - 指纹绑定的是 CA 身份，与地址无关，也不需要把 ca.crt 文件铺到每一台被控机。
//
// caPEM / caFile 二者其一（或都不提供时仅用指纹）：
//   - 提供时同时做链校验（更强的完整性检查）；
//   - pin 为空时不启用固定（退回标准校验，由系统根或 RootCAs 决定）。
func buildPinTLSConfig(caPEM []byte, pin, caFile string, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("读取服务端 CA 证书失败: %w", err)
		}
		caPEM = b
	}
	if len(caPEM) == 0 {
		return cfg, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("解析服务端 CA 证书失败")
	}
	cfg.RootCAs = pool
	want := normFingerprint(pin)
	if want == "" {
		return cfg, nil
	}
	// 固定指纹：跳过链校验，只校验对端证书的公钥指纹（CA 一致即通过）。
	cfg.InsecureSkipVerify = true
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("服务端未提供证书")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		got := certFingerprint(leaf)
		if got != want {
			return FingerprintError(pin, got)
		}
		// 有 CA 时再做一次链校验（防中间人拿 CA 之外的证书 + 撑大证书链）
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			return fmt.Errorf("服务端证书与 CA 不匹配: %w", err)
		}
		return nil
	}
	return cfg, nil
}
