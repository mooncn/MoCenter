package agentauth

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

// 回归：服务端必须能判定"对端证书是不是本套 CA 签的"。
//
// 背景（线上故障）：Agent 通道用 tls.RequestClientCert（请求但不强制，为了允许
// 新 Agent 先换取证书）。这种模式下 **Go 不会在握手阶段校验客户端证书链**，
// 对端证书只要能被解析就会出现在 PeerCertificates 里。
// 若应用层只读 CN 而不校验链，那么"任何自签一张 CN=某主机名的证书"都能冒充该主机 ——
// 而且这个洞在"服务端换过 CA、Agent 还拿着旧证书"的正常运维场景里就会被触发：
// 旧证书交上来，服务端把它当成了已验证身份。
func TestPeerCertMatchesCA(t *testing.T) {
	server, err := NewIdentity("Srv")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewIdentity("Other")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _, err := server.NewAgentCert("host-01")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := other.NewAgentCert("host-01")
	if err != nil {
		t.Fatal(err)
	}
	if !server.CertSignedByThisCA(leaf) {
		t.Fatal("本套 CA 签发的证书应被认可")
	}
	if server.CertSignedByThisCA(foreign) {
		t.Fatal("别的 CA 签发的证书（哪怕 CN 相同）不得被认可 —— 否则可冒充任意主机")
	}
	if server.CertSignedByThisCA(nil) {
		t.Fatal("空证书不得被认可")
	}
	// 自签证书（攻击者最容易造的那种）同样必须被拒
	self := selfSignedLeaf(t)
	if server.CertSignedByThisCA(self) {
		t.Fatal("自签证书不得被认可")
	}
}

func selfSignedLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	id, err := NewIdentity("Attacker")
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := id.NewAgentCert("host-01")
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

var _ = tls.VersionTLS12
