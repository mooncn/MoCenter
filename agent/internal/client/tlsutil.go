package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// baseTLSConfig 构造连接服务端用的 TLS 配置。
//
// 校验策略（按优先级）：
//  1. Insecure=true：跳过校验（仅自签名调试，会打警告）；
//  2. 配置了 ServerFingerprint：**公钥指纹固定** —— 不需要 CA 文件、不需要域名/IP 匹配，
//     服务端换地址/换证书都不必更新（自签部署里 SAN 不匹配是运维最常见的踩坑点）；
//  3. 配置了 CACert：用该 CA 做标准校验；
//  4. 都没有：用系统根（正式证书场景，如 Let's Encrypt）。
//
// ServerFingerprint 一般由服务端 Web「Agent 接入」页或安装器给出（也可取服务端
// <data>/ca.crt 的公钥指纹），等价于"记住这台总控的身份"。
func (c *Client) baseTLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.cfg.Insecure {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	pin := normFingerprint(c.cfg.ServerFingerprint)
	if pin != "" {
		// 固定指纹：跳过标准链校验（自签 CA 不在系统根里），改由回调校验公钥指纹。
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("服务端未提供证书")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got := spkiFingerprint(leaf)
			if got != pin {
				return fmt.Errorf("服务端身份指纹不一致：期望 %s，实际 %s（服务端换过 CA？请向管理员索取新指纹）",
					pin, got)
			}
			return nil
		}
		return cfg, nil
	}
	// CA 文件：优先用配置指定的，其次用"换取证书时服务端回传的 ca.crt"（自动落盘）。
	// 后者是"简单模式"的关键：换取完成后不再需要任何手工证书文件，且服务端
	// 换 IP/域名也不会失败（下面按**公钥**校验链，而不是按域名/IP 校验）。
	for _, caFile := range []string{c.cfg.CACert, filepath.Join(c.credDir(), "ca.crt")} {
		if caFile == "" {
			continue
		}
		b, err := os.ReadFile(caFile)
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			continue
		}
		cfg.RootCAs = pool
		// 关键：只校验"证书链是否由该 CA 签发"，不要求 SAN 与服务端地址一致。
		// 自签部署里 SAN 必须写死服务端真实 IP/域名，换 IP、加 CDN、走内网地址都会
		// 校验失败 —— 这正是运维最终改成 -insecure（等于关掉校验）的原因。
		// 这里用 VerifyConnection 做链校验，语义上不比标准校验弱：能签出该证书的
		// 只有我们信任的这套 CA。
		cfg.VerifyConnection = func(st tls.ConnectionState) error {
			if len(st.PeerCertificates) == 0 {
				return fmt.Errorf("服务端未提供证书")
			}
			_, err := st.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool})
			return err
		}
		return cfg, nil
	}
	return cfg, nil
}

// clientCert 返回当前可用的客户端证书：内存中的换取结果 → 配置指定文件 → 落盘的换取产物。
func (c *Client) clientCert() *tls.Certificate {
	if c.cred != nil {
		return &c.cred.Cert
	}
	if c.cfg.ClientCert != "" && c.cfg.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(c.cfg.ClientCert, c.cfg.ClientKey)
		if err == nil {
			return &cert
		}
		log.Printf("[agent] 加载配置中的客户端证书失败: %v", err)
	}
	dir := c.credDir()
	cert, err := tls.LoadX509KeyPair(dir+"/client.crt", dir+"/client.key")
	if err != nil {
		return nil
	}
	return &cert
}

// spkiFingerprint 取证书公钥指纹（SHA-256 前 16 位 hex）。
// 用公钥而不是整证书：换签发有效期、换 SAN 都不会改变指纹。
func spkiFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])[:16]
}

// normFingerprint 归一化人工填写的指纹（允许冒号/空格、大小写）。
func normFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DisplayFingerprint 格式化指纹便于打印。
func DisplayFingerprint(fp string) string {
	return agentauthDisplay(normFingerprint(fp))
}

// logf 统一日志前缀。
func logf(format string, args ...interface{}) { log.Printf(format, args...) }

// agentauthDisplay 把指纹格式化成 4 位一组（与服务端日志口径一致）。
func agentauthDisplay(fp string) string {
	if len(fp) < 8 {
		return fp
	}
	var parts []string
	for i := 0; i+4 <= len(fp) && len(parts) < 4; i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	return strings.Join(parts, " ")
}
