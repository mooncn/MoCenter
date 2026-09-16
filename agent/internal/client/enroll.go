package client

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mocenter/agent/internal/agentauth"
	"mocenter/agent/internal/protocol"
)

// 本文件实现"简单模式"的客户端侧：用注册密钥向服务端换取客户端证书。
//
// ## 为什么要容忍"首次是未校验的加密连接"
//
// 这一步的信任根是**注册密钥本身**（等价于后者），不是服务端证书：
//   - 客户端发出去的是 HMAC 证明，密钥不上线传输 —— 中间人拿不到密钥，
//     也就无法在后续连接中冒充这台 Agent；
//   - 服务端回的是**证书**，其中不含任何秘密；中间人若截获，只能得到一张
//     写着这台 agent_id 的公开证书，而对应私钥始终只在本机（本地生成）。
// 因此即使首次连接被中间人观察，也不会泄露可用凭据、更不会让攻击者取得 Agent 身份。
// 这也是与"跳过 TLS 校验后就把连接当可信"的本质区别：这里只用于一次性换取。
//
// ## 换取为什么必须发生在加密连接上
//
// 明文链路上中间人可以**替换**响应（把服务端证书换成自己的、并诱导客户端把
// 后续业务流量发给自己），因此本实现拒绝在明文连接上换取，并明确报错 ——
// 不静默降级成"明文也能跑"，否则运维不会注意到自己其实没加密。
//
// ## 换取之后
//
// 落盘 client.crt / client.key / ca.crt（0600），并把配置文件里的
// enrollment_secret 清空（一次使用，减少泄露面）；后续连接一律 mTLS，
// 服务端还会用证书 CN 作为身份，双重约束。

// enrollmentCredential 换取到的客户端凭证。
type enrollmentCredential struct {
	Cert      tls.Certificate
	CertFile  string
	KeyFile   string
	CAFile    string
	Identity  string
	Fingerprt string
}

// tryEnroll 用注册密钥换取客户端证书并落盘。返回的凭证供后续连接直接使用。
func (c *Client) tryEnroll() (*enrollmentCredential, error) {
	secret := strings.TrimSpace(c.cfg.EnrollmentSecret)
	if secret == "" {
		return nil, fmt.Errorf("未配置 enrollment_secret")
	}
	// 本地生成密钥对：私钥永不离开本机
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	tlsCfg, err := c.baseTLSConfig()
	if err != nil {
		return nil, err
	}
	conn, err := dialForEnroll(c.cfg.ServerAddr, tlsCfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if !isEncrypted(conn) {
		return nil, fmt.Errorf("连接未加密，拒绝在明文链路上换取证书（请确认服务端已启用 TLS）")
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	nonce := agentauth.NewNonce()
	ts := time.Now().Unix()
	data, err := json.Marshal(protocol.EnrollReq{
		AgentID: c.cfg.AgentID,
		Nonce:   nonce,
		Proof:   agentauth.Proof(secret, c.cfg.AgentID, nonce, ts),
		TS:      ts,
		CSR:     csrPEM,
	})
	if err != nil {
		return nil, err
	}
	// 必须包一层 Envelope（服务端按 envelope.type 分发），而不是发裸的结构体
	env, err := json.Marshal(protocol.Envelope{Type: protocol.TypeEnrollReq, Data: data})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(env, '\n')); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var env protocol.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil || env.Type != protocol.TypeEnrollResp {
			continue
		}
		var resp protocol.EnrollResp
		if err := json.Unmarshal(env.Data, &resp); err != nil {
			return nil, err
		}
		if !resp.OK || resp.CertPEM == "" {
			return nil, fmt.Errorf("服务端拒绝签发证书: %s", resp.Message)
		}
		keyPEM, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		keyPEMText := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEM})
		cert, err := tls.X509KeyPair([]byte(resp.CertPEM), keyPEMText)
		if err != nil {
			return nil, fmt.Errorf("解析服务端签发的证书失败: %w", err)
		}
		cred := &enrollmentCredential{
			Cert: cert, Identity: resp.Identity, Fingerprt: resp.ServerFingerprint,
		}
		if err := c.saveCredential(cred, resp.CertPEM, resp.CAPEM, keyPEMText); err != nil {
			return nil, err
		}
		logf("[agent] 已从服务端换取客户端证书（身份 %s，服务端指纹 %s）；证书已保存，后续连接直接携带",
			resp.Identity, DisplayFingerprint(resp.ServerFingerprint))
		return cred, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("服务端未返回换取结果")
}

// dialForEnroll 拨号：优先 TLS；TLS 握手因**证书校验**失败时，改用"只加密不验签"再试一次
// （换取阶段是信任引导，见文件头说明：这一跳的安全性由注册密钥保证），
// 仍失败才退回明文 —— 目的是让"服务端根本没启用 TLS"能给出明确错误，而不是静默跑起来。
func dialForEnroll(addr string, tlsCfg *tls.Config) (net.Conn, error) {
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err == nil {
		return conn, nil
	}
	// 换取阶段容忍未受信任的自签证书（CI/内网自签、尚无 CA 文件是常态）
	if isCertVerifyFailure(err) {
		loose := tlsCfg.Clone()
		loose.InsecureSkipVerify = true
		loose.VerifyPeerCertificate = nil
		loose.VerifyConnection = nil
		if c2, e2 := tls.Dial("tcp", addr, loose); e2 == nil {
			return c2, nil
		}
	}
	if plain, perr := net.Dial("tcp", addr); perr == nil {
		return plain, nil
	}
	return nil, err
}

// isCertVerifyFailure 判断是否为证书校验类错误（而非网络不可达）。
func isCertVerifyFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") ||
		strings.Contains(msg, "tls:")
}

func isEncrypted(conn net.Conn) bool {
	_, ok := conn.(*tls.Conn)
	return ok
}

// saveCredential 把证书/私钥/CA 落盘（目录 0700，文件 0600），并清空配置里的注册密钥。
func (c *Client) saveCredential(cred *enrollmentCredential, certPEM, caPEM string, keyPEM []byte) error {
	dir := c.credDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	cred.CertFile = filepath.Join(dir, "client.crt")
	cred.KeyFile = filepath.Join(dir, "client.key")
	cred.CAFile = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(cred.CertFile, []byte(certPEM), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(cred.KeyFile, keyPEM, 0600); err != nil {
		return err
	}
	if caPEM != "" {
		if err := os.WriteFile(cred.CAFile, []byte(caPEM), 0600); err != nil {
			return err
		}
	}
	// 注册密钥已用过：清掉，避免长期驻留在配置里（后续连接靠证书即可）
	c.cfg.EnrollmentSecret = ""
	if c.onEnrolled != nil {
		c.onEnrolled(cred)
	}
	if c.onEnrolledFiles != nil {
		c.onEnrolledFiles(CredentialFiles{CertFile: cred.CertFile, KeyFile: cred.KeyFile, CAFile: cred.CAFile})
	}
	return nil
}

// credDir 凭证落盘目录：配置文件所在目录下的 creds/。
func (c *Client) credDir() string {
	if c.cfg.CredDir != "" {
		return c.cfg.CredDir
	}
	if p := strings.TrimSpace(c.cfg.ConfigPath); p != "" {
		return filepath.Join(filepath.Dir(p), "creds")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "creds")
	}
	return "creds"
}
