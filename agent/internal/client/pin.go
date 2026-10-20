package client

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
)

// 本文件处理"服务端换过 CA"这一类故障的**自动收敛**，以及 CA 身份的连续性校验。
//
// 背景（实机踩过的坑）：
//
//	服务端重装/换机器后，数据目录下的 ca.crt + ca.key 是全新的一套；而 Agent 本机
//	creds/ 里还留着上一套 CA 签发的 client.crt。此时 TLS 握手必然失败（客户端证书
//	不被新 CA 认可、服务端证书也不被旧 ca.crt 认可），表现为 Agent 反复重连、服务端
//	只打一行"Agent 连接未完成注册，关闭"，运维完全看不出是证书的事。
//
//	而"删除 creds/ 重新换取"这一步明明可以自动完成：Agent 手上还有注册密钥
//	（agent_key / enroll-secret），服务端也允许无证书连接发起一次换取。
//
// 因此这里引入**身份连续性校验**：
//
//   - 换取证书时服务端会回传它的 CA 证书（ca_pem）。若本机已持有上一套 ca.crt，
//     先比对两者的公钥指纹：
//       · 相同 → 同一套总控换证书，正常换取；
//       · 不同 → 说明"当前连上的这套总控"已经不是原来那套（重装/换机器/连错地址）。
//     默认拒绝在**未经授权**的情况下信任一套新 CA：否则任何人起一个假总控、
//     把地址换过去就能让 Agent 交出手上的注册密钥（换取请求里带 HMAC 证明），
//     等于把"证书锁定"退回成"跳一次就能骗走凭据"。
//   - 但"换 CA 后自动恢复"是明确需要的：只要新指纹与**配置里显式填写的**
//     server_fingerprint 一致（或显式配置了 ca-cert 且新 CA 与之一致），
//     就视为运维已授权，自动丢弃旧证书并重新换取，无需登录被控机。

// certPublicFingerprint 取证书公钥指纹（完整 SHA-256，64 位 hex）。
// 与 server/internal/agentauth.certFingerprint 口径一致：绑定**公钥**，
// 因此同一个 CA 重签证书、换有效期都不会改变它。
func certPublicFingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// caFingerprintFromPEM 解析 PEM 链中第一张证书并取公钥指纹。
func caFingerprintFromPEM(pemText []byte) string {
	cert := parseFirstCert(pemText)
	return certPublicFingerprint(cert)
}

// credCAFingerprint 读取本机已持有 ca.crt 的公钥指纹（不存在/不可解析返回空串）。
func (c *Client) credCAFingerprint() string {
	b, err := os.ReadFile(filepath.Join(c.credDir(), "ca.crt"))
	if err != nil {
		return ""
	}
	return caFingerprintFromPEM(b)
}

// configCAFingerprint 读取 -ca-cert 指定文件的公钥指纹。
func (c *Client) configCAFingerprint() string {
	p := strings.TrimSpace(c.cfg.CACert)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return caFingerprintFromPEM(b)
}

// knownServerFingerprints 返回"运维已经明确授权"的服务端 CA 指纹集合。
//
// 来源两类：
//   - 配置里的 server_fingerprint（运维手工填的，等价于"我确认是这套总控"）；
//   - 本机 creds/ca.crt 与 -ca-cert 文件的指纹（换取/配置时确认过的旧身份）。
//
// 注意 creds/ca.crt 属于"上一次的身份"，只用于判定"服务端是否换过"，不作为放行依据。
func (c *Client) knownServerFingerprints() (pinned, known []string) {
	if fp := normFingerprint(c.cfg.ServerFingerprint); len(fp) == legacyFingerprintLen || len(fp) == 64 {
		pinned = append(pinned, fp)
	}
	for _, fp := range []string{c.credCAFingerprint(), c.configCAFingerprint()} {
		if fp != "" {
			known = append(known, fp)
		}
	}
	return pinned, known
}

// trustNewCAError 组装"服务端 CA 变了但没人授权"的可读错误。
func trustNewCAError(oldFP, newFP string, hasSecret bool) error {
	var b strings.Builder
	b.WriteString("服务端部署身份 CA 与本机记下的不一致（")
	b.WriteString("本机 ")
	if oldFP == "" {
		b.WriteString("无记录")
	} else {
		b.WriteString(DisplayFingerprint(oldFP))
	}
	b.WriteString("，服务端 ")
	if newFP == "" {
		b.WriteString("未提供")
	} else {
		b.WriteString(DisplayFingerprint(newFP))
	}
	b.WriteString("）。这通常意味着服务端重装过、更换了总控，或当前连到的不是原来的地址。\n")
	b.WriteString("  这是**身份变化**，不会自动信任对方。三选一即可恢复：\n")
	b.WriteString("    1) 确认服务端指纹无误后，把上面「服务端」指纹填进本机配置 server_fingerprint（最长效，不需登录被控机即可远程下发配置文件）；\n")
	if hasSecret {
		b.WriteString("    2) 确认这是你自己的总控：删除本机 creds/ 里的 client.crt 与 ca.crt（**保留 client.key**：服务端按证书公钥指纹做首次信任绑定，换掉私钥就要重新出示一次性注册令牌）后重启 Agent，将用注册凭据重新换取证书；\n")
	} else {
		b.WriteString("    2) 本机已没有注册凭据（换取后被清空）：在控制台为该主机签发一次性注册令牌（或服务端 <data>/agent.key 的共享密钥）填进配置 agent_key 后重启，Agent 会自动重新换取证书；\n")
	}
	b.WriteString("    3) 服务端确实是新的总控 → 按运维流程重新接入这台主机。")
	return &trustError{msg: b.String(), newFP: newFP}
}

// trustError 表示"服务端身份变化且未获授权"。
// 单独定义类型，便于调用方（以及测试）识别，而不是靠字符串匹配。
type trustError struct {
	msg   string
	newFP string
}

func (e *trustError) Error() string { return e.msg }

// parseFirstCert 解析 PEM 中的第一张证书。
func parseFirstCert(pemText []byte) *x509.Certificate {
	block, _ := pem.Decode(pemText)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}
