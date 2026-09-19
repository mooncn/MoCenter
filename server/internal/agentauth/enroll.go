package agentauth

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// 注册凭证交换（"简单模式"省掉手工证书的核心）：
//
//	Agent  --EnrollReq(agent_id, nonce, proof)-->  Server
//	Agent  <--EnrollResp(ok, cert_pem, ca_pem)---  Server
//
// 服务端在**同一条已通过 HMAC 校验的连接**上用内置 CA 现签一张客户端证书回发。
// 因此运维只需要分发一个内容物（agent.key），不再需要 ca.crt / client.crt / client.key。
type EnrollReq struct {
	AgentID string `json:"agent_id"`
	Nonce   string `json:"nonce"`
	Proof   string `json:"proof"`
	TS      int64  `json:"ts"`
}

// EnrollResp 注册凭证交换响应。
type EnrollResp struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	// CertPEM 客户端证书链（PEM），Agent 落盘为 client.crt（0600）。
	CertPEM string `json:"cert_pem,omitempty"`
	// CAPEM 服务端身份 CA 证书（PEM），Agent 可落盘为 ca.crt 便于人工排查。
	CAPEM string `json:"ca_pem,omitempty"`
	// ServerFingerprint 服务端身份公钥指纹，Agent 用于（首次明文连接时）确认对端。
	ServerFingerprint string `json:"server_fingerprint,omitempty"`
	// Identity 部署身份名，便于日志辨识。
	Identity string `json:"identity,omitempty"`
}

// parsePEMFirstCertificate 解析 PEM 链中的第一张证书（用于自查签发结果）。
func parsePEMFirstCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("证书 PEM 解析失败")
	}
	return x509.ParseCertificate(block.Bytes)
}
