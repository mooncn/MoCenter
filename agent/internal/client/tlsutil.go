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
	"time"
)

// baseTLSConfig 构造连接服务端用的 TLS 配置。
//
// 校验策略（按优先级）：
//  1. Insecure=true：跳过校验（仅自签名调试，会打警告）；
//  2. 配置了 ServerFingerprint：**公钥指纹固定** —— 不需要 CA 文件、不需要域名/IP 匹配，
//     服务端换地址/换证书都不必更新（自签部署里 SAN 不匹配是运维最常见的踩坑点）；
//     指纹比对覆盖**整条链**（叶子或其签发 CA），因此填"部署身份 CA 指纹"（服务端
//     启动日志 / Web「Agent 接入」页显示的那个）即可长期有效，不受服务端重启换证书影响；
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
		//
		// 关键语义：**指纹绑定的是"部署身份"（CA），不是服务端那张叶子证书**。
		// 这一点必须讲清楚，否则会踩到下面这个坑（线上真实故障）：
		// 服务端每次启动都会**重新签发**一张 Agent 通道监听证书（新密钥、新有效期），
		// 而部署身份指纹（`<data>/ca.crt` 的公钥指纹，也是 Web「Agent 接入」页与
		// 服务端启动日志里显示的那个"身份指纹"）是稳定不变的。
		// 若拿 pin 去比对**叶子证书**的公钥指纹，那么：
		//   · 运维按界面提示填的 CA 指纹永远对不上 → 每次连接都报"身份指纹不一致"；
		//   · 服务端每重启一次，即便 CA 没变，pin 也会失效。
		// 于是"用指纹免掉 CA 文件"这个卖点实际不可用，运维只能退回 -insecure（=不校验）。
		//
		// 因此校验规则为：pin == 链上任一证书的公钥指纹（叶子或签发它的 CA）即通过。
		// 这既不比原实现弱（叶子仍在校验集合内），又让 CA 指纹成为稳定、可长期使用
		// 的锚点 —— 与服务端换 IP、换证书、重启全都无关。
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("服务端未提供证书")
			}
			certs := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					return err
				}
				certs = append(certs, cert)
			}
			// 命中叶子：pin 的是这张证书本身，公钥不可伪造；只需再确认它当前有效。
			if fingerprintMatches(pin, spkiFingerprint(certs[0])) {
				now := time.Now()
				if now.Before(certs[0].NotBefore) || now.After(certs[0].NotAfter) {
					return fmt.Errorf("服务端证书不在有效期内（%s ~ %s）",
						certs[0].NotBefore.Format(time.RFC3339), certs[0].NotAfter.Format(time.RFC3339))
				}
				return nil
			}
			// 命中链上的 CA：**必须真的验签叶子**。
			//
			// 只遍历 rawCerts 比指纹是不够的：服务端证书链由对端自行给出，攻击者可以把
			// 公开的 CA 证书附在自己的自签叶子之后 —— CA 公钥是公开信息（本服务端每次
			// 握手都会下发，换取响应里也会给），指纹一命中就放行等于 pin 完全失效。
			// 因此这里以命中的证书为信任根，走一次标准的链校验（签名、有效期、ServerAuth 用途），
			// 不校验 SAN（自签部署换 IP 是常态）。
			for i := 1; i < len(certs); i++ {
				if !fingerprintMatches(pin, spkiFingerprint(certs[i])) {
					continue
				}
				roots := x509.NewCertPool()
				roots.AddCert(certs[i])
				inter := x509.NewCertPool()
				for j := 1; j < len(certs); j++ {
					if j != i {
						inter.AddCert(certs[j])
					}
				}
				if _, err := certs[0].Verify(x509.VerifyOptions{
					Roots:         roots,
					Intermediates: inter,
					KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				}); err != nil {
					return fmt.Errorf("服务端证书链与固定的部署身份不符（链上虽带有该 CA，但叶子并非由其签发）: %w", err)
				}
				return nil
			}
			return fmt.Errorf("服务端身份指纹不一致：期望 %s（部署身份 CA 指纹），实际服务端证书 %s"+
				"（服务端换过部署身份？请向管理员索取新指纹，或核对 Agent 配置里的 server_fingerprint）",
				DisplayFingerprint(pin), DisplayFingerprint(spkiFingerprint(certs[0])))
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

// hasTrustAnchor 本机是否已经持有一个"服务端身份锚"（L-6）。
//
// 三个来源任一即算：显式配置的 server_fingerprint、配置指定的 CA 文件、
// 上一次换取时服务端回传并落盘的 creds/ca.crt。有锚时校验失败意味着"对端不是那套总控"，
// 换取阶段绝不能再退回去验签；无锚才代表"第一次接入、还没建立过信任"，那才是降级路径的场景。
func (c *Client) hasTrustAnchor() bool {
	if normFingerprint(c.cfg.ServerFingerprint) != "" {
		return true
	}
	if strings.TrimSpace(c.cfg.CACert) != "" {
		return true
	}
	if p := filepath.Join(c.credDir(), "ca.crt"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
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

// certCommonName 取客户端证书 DN 里的 CN（服务端就是拿它当连接身份的）。
// 解析失败或没有 CN 时返回空串，调用方应跳过身份一致性判断。
func certCommonName(cert *tls.Certificate) string {
	if cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(leaf.Subject.CommonName)
}

// spkiFingerprint 取证书公钥指纹（完整 SHA-256，64 位 hex）。
// 用公钥而不是整证书：换签发有效期、换 SAN 都不会改变指纹。
func spkiFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// legacyFingerprintLen 历史版本配置文件里的短指纹长度（只取 SHA-256 前 16 位）。
const legacyFingerprintLen = 16

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

// fingerprintMatches 比较预期指纹与实际指纹。
//
// 完整 64 位必须完全一致；历史 16 位短指纹按前缀匹配，保证老配置升级后仍能连接。
func fingerprintMatches(want, got string) bool {
	w, g := normFingerprint(want), normFingerprint(got)
	if w == "" || g == "" {
		return false
	}
	if w == g {
		return true
	}
	return len(w) == legacyFingerprintLen && len(g) > legacyFingerprintLen && strings.HasPrefix(g, w)
}

// ValidateServerFingerprint 校验配置里的 server_fingerprint 格式。
//
// 接受完整 64 位十六进制与历史 16 位短指纹（分隔符可为冒号/空格/短横线）；
// 其它长度（尤其是 8 位）直接拒绝：pin 的强度取决于长度，静默接受短值
// 会让"填了指纹"看起来比实际更安全。
func ValidateServerFingerprint(fp string) error {
	raw := strings.ToLower(strings.TrimSpace(fp))
	if raw == "" {
		return nil
	}
	for _, r := range raw {
		switch {
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'), r == ':', r == ' ', r == '-':
			continue
		default:
			return fmt.Errorf("server_fingerprint 含非法字符 %q（只允许十六进制、冒号/空格/短横线）", r)
		}
	}
	switch n := len(normFingerprint(raw)); n {
	case legacyFingerprintLen, 64:
		return nil
	default:
		return fmt.Errorf("server_fingerprint 长度无效（归一化后 %d 位）：应为 64 位完整指纹，或兼容历史 16 位短指纹", n)
	}
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
	for i := 0; i+4 <= len(fp); i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	if rem := len(fp) % 4; rem > 0 {
		parts = append(parts, fp[len(fp)-rem:])
	}
	return strings.Join(parts, " ")
}
