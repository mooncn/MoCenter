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
	"errors"
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
	// Source 服务端认定的凭证类型（token / shared-key / anonymous）。
	Source string
	// UsedField 本机实际出示凭据的配置字段（enrollment_secret / agent_key）。
	UsedField string
}

// tryEnroll 用注册密钥换取客户端证书并落盘。返回的凭证供后续连接直接使用。
//
// 密钥来源：enrollment_secret 优先，其次 agent_key。
// 两者都要认的理由：换取成功后 enrollment_secret 会被清空（一次性凭据），
// 而 agent_key 是运维留在配置里的长期值。若只看前者，"服务端换过 CA 后自动
// 重新换取"就永远走不通 —— 那正是"必须先人肉删 creds/ 才能恢复"的根因之一。
func (c *Client) tryEnroll() (*enrollmentCredential, error) {
	secret := strings.TrimSpace(c.cfg.EnrollmentSecret)
	usedField := "enrollment_secret"
	if secret == "" {
		secret = strings.TrimSpace(c.cfg.AgentKey)
		usedField = "agent_key"
	}
	if secret == "" {
		return nil, fmt.Errorf("未配置注册密钥（enrollment_secret / agent_key 均为空），无法换取客户端证书")
	}
	logf("[agent] 正在用注册凭据换取客户端证书（agent_id=%q，凭据来自 %s）…", c.cfg.AgentID, usedField)
	// 客户端私钥：本机有则复用，没有才新生成（私钥永不离开本机）。
	// 复用不是省事：服务端按 agent_id → 证书公钥指纹(SPKI) 做 TOFU 绑定，
	// 换私钥就等于换机器身份。若每次重签都换新钥，服务端换过 CA 之后
	// 本机再也换不回证书（现网自动恢复路径会被自己的防护挡住）。
	key, err := c.clientKey()
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
			// 把本机实际使用的 agent_id 一并带出来：令牌是按 agent_id 绑定的，
			// 服务端刻意不区分"名字不匹配/令牌不对"，排障只能靠本机这一侧把
			// "我到底是谁"写清楚，否则只剩一句"注册凭证校验失败"。
			return nil, fmt.Errorf("服务端拒绝换取客户端证书（本机 agent_id=%q）：%s"+
				"；请核对控制台签发令牌时填写的 agent_id 与本机是否完全一致"+
				"（安装向导的「Agent 名称/ID」或 -agentid；留空会回退为机器 UUID）",
				c.cfg.AgentID, resp.Message)
		}
		// 身份连续性：先确认"回应我们的确实是本机认可的那套总控"，再落地任何东西。
		// 这一步顺序很关键 —— 先查后写，才算真正"不信任陌生人"。
		respFP := normFingerprint(resp.ServerFingerprint)
		caFP := caFingerprintFromPEM([]byte(resp.CAPEM))
		if err := c.verifyEnrollServer(respFP, caFP); err != nil {
			return nil, err
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
			Cert: cert, Identity: resp.Identity, Fingerprt: respFP,
			Source: resp.CredSource, UsedField: usedField,
		}
		if err := c.saveCredential(cred, resp.CertPEM, resp.CAPEM, keyPEMText); err != nil {
			return nil, err
		}
		used := ""
		if resp.CredSource == protocol.CredSourceToken {
			used = "，一次性令牌已在服务端标记为已使用"
		}
		if caFP != "" {
			logf("[agent] 已从服务端换取客户端证书（身份 %s%s，服务端指纹 %s）；证书已保存，后续连接直接携带",
				resp.Identity, used, DisplayFingerprint(caFP))
		} else {
			logf("[agent] 已从服务端换取客户端证书（身份 %s%s）；证书已保存，后续连接直接携带", resp.Identity, used)
		}
		return cred, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("服务端未返回换取结果")
}

// verifyEnrollServer 在落地新证书之前校验服务端身份。
//
// 判定规则（新指纹 respFP 取自 EnrollResp.server_fingerprint，caFP 取自响应里的 ca_pem）：
//
//  1. 运维显式填写了 server_fingerprint → 必须以之为准；不一致直接拒绝（这是 pin 的本意）；
//  2. 新指纹与本机已记下的（creds/ca.crt 或 -ca-cert）一致 → 同一套总控，正常；
//  3. 本机此前**没有任何 CA 记录**（首次接入）→ 信任引导，放行（信任根是注册密钥）；
//  4. 本机有记录、但两次都拿不到可用的比较值 → 无法判断，拒绝并要求重新获取；
//  5. 本机有记录、且确实不同 → 服务端换过 CA（重装/换机器）。默认**拒绝**，
//     除非服务端指纹或 CA 指纹与本机配置里的 server_fingerprint 一致 ——
//     那说明运维已经明确授权了这套新身份，可以自动丢弃旧证书重换。
//
// 之所以第 5 条默认拒绝：换取请求里带着注册密钥的 HMAC 证明，无条件重换等于
// "只要能把 Agent 的地址指向我，就能拿走它的注册凭据"，把证书锁定彻底架空。
func (c *Client) verifyEnrollServer(respFP, caFP string) error {
	pinned, known := c.knownServerFingerprints()

	// 1) 显式 pin：只认它，别的都要拒绝
	if len(pinned) > 0 {
		if respFP != "" && respFP != pinned[0] {
			return errors.New("服务端与配置的 server_fingerprint 不一致：" + trustNewCAError(pinned[0], respFP, c.hasEnrollSecret()).Error())
		}
		if caFP != "" && caFP != pinned[0] {
			return errors.New(trustNewCAError(pinned[0], caFP, c.hasEnrollSecret()).Error())
		}
		if respFP == "" && caFP == "" {
			return errors.New("服务端未返回身份指纹，无法与配置的 server_fingerprint 比对（请升级服务端）")
		}
		return nil
	}

	// 4) 有旧记录但新响应完全没给身份信息：无法判断是否换过，宁可拒绝
	if len(known) > 0 && respFP == "" && caFP == "" {
		return errors.New("服务端未返回身份指纹（登录身份 CA 未启用？），无法确认它是否就是本机记下的那套总控")
	}

	// 2) 同一套总控 / 3) 首次接入
	matched := false
	for _, fp := range known {
		if fp == respFP || (caFP != "" && fp == caFP) {
			matched = true
			break
		}
	}
	if matched || len(known) == 0 {
		return nil
	}

	// 5) 身份发生了变化
	old := known[0]
	return trustNewCAError(old, firstNonEmptyStr(caFP, respFP), c.hasEnrollSecret())
}

// hasEnrollSecret 本机是否还持有可用于重新换取的注册密钥。
func (c *Client) hasEnrollSecret() bool {
	return strings.TrimSpace(c.cfg.EnrollmentSecret) != "" || strings.TrimSpace(c.cfg.AgentKey) != ""
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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

// isCertVerifyFailure 判断是否为**证书校验类**错误（而非网络不可达）。
//
// 这里刻意不只看 Go 的英文错误串。原因：Agent 自己产生的校验错误是中文的
// （例如"服务端身份指纹不一致……"），那时的错误里**一个 "certificate" 都没有**，
// 于是一旦走指纹校验，换取/降级判断就会全部失配 —— 表现为
// "服务端换过 CA 后既不会自动重换、还会退化成明文再被拒"，
// 而报错只留下一句"拒绝在明文链路上换取证书"，运维完全看不出是证书的事。
//
// 因此判定口径改成三类：
//  1. Go 的 x509/tls 层错误（英文）；
//  2. 本包的指纹/身份类错误（errors.As 识别 trustError）；
//  3. 兜底：消息里出现"指纹/证书/校验"等中文关键词。
func isCertVerifyFailure(err error) bool {
	if err == nil {
		return false
	}
	var te *trustError
	if errors.As(err, &te) {
		return true
	}
	msg := err.Error()
	for _, kw := range []string{
		"certificate", "x509", "tls:", "record header",
		"指纹", "证书", "校验失败",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
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
		c.onEnrolledFiles(CredentialFiles{
			CertFile: cred.CertFile, KeyFile: cred.KeyFile, CAFile: cred.CAFile,
			Source: cred.Source, UsedField: cred.UsedField,
		})
	}
	return nil
}

// clientKey 取本机客户端私钥：优先复用 <creds>/client.key（上次换取留下的），
// 读不到或解析不了才生成新密钥对。公钥稳定才能让服务端 TOFU 绑定在
// 证书到期重签、服务端换 CA 等场景下继续认这台机器。
func (c *Client) clientKey() (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(filepath.Join(c.credDir(), "client.key")); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if k, kerr := x509.ParseECPrivateKey(blk.Bytes); kerr == nil {
				return k, nil
			}
		}
	}
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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
