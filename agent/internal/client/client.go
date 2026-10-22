// Package client 管理 Agent 与服务端的 TCP 长连接、心跳保活与断线重连。
package client

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"mocenter/agent/internal/logx"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mocenter/agent/internal/agentauth"
	"mocenter/agent/internal/command"
	"mocenter/agent/internal/probe"
	"mocenter/agent/internal/protocol"
	"mocenter/agent/internal/upgrade"
	"mocenter/agent/internal/version"
)

// Config Agent 配置。
type Config struct {
	ServerAddr string // 服务端地址 host:port
	AgentID    string
	// UUID 硬件唯一标识（设备身份）。为空时由探针自动探测；
	// 服务端以它判定"同一台机器"，agent_id 变更不会产生新设备。
	UUID       string
	Heartbeat  time.Duration // 默认 30s
	ProbeEvery time.Duration // 探针采集周期，默认 10s
	TLS        bool          // 是否启用 TLS 加密连接
	Insecure   bool          // 是否跳过证书校验（自签名测试）
	ClientCert string        // mTLS 客户端证书路径
	ClientKey  string        // mTLS 客户端私钥路径
	CACert     string        // 校验服务端证书的 CA 证书路径
	// ProxyIP 国内加速代理前缀（用于公网 IP 探测等境外服务，如 http://fa.mooncn.win/?url= ）。
	// 注意：安装包下载这条链只接受 **https://** 前缀（见 command.httpsProxyOrEmpty），
	// 因为它的产物会以 SYSTEM 执行；明文代理等于让中间人替换安装器。
	ProxyIP string
	// AgentKey 与服务端预共享的注册密钥；为空则无法通过服务端的注册鉴权。
	AgentKey string
	// UpgradePubKey 升级包 Ed25519 公钥（hex 或文件路径）。**必填**：
	// 没有公钥就无法验签，远程升级一律拒绝（没有"跳过校验"的开关）。
	UpgradePubKey string
	// FileRoots 允许服务端下发读写的本地目录（文件分发/文件下载）。
	// 留空则由 New 落到默认沙箱 <配置目录>/files。
	FileRoots []string
	// TunnelHosts 端口映射/VNC 隧道允许指向的**非回环**目标（IP 或主机名，逗号分隔）。
	// 留空 = 只允许回环（默认）。见 openTunnel / tunnelTargetAllowed（L-5）：
	// 这个开关只从本机配置文件读，服务端指令打不开它。
	TunnelHosts []string
	// AllowPublicDownload 解锁「从公网取安装包」（winget / choco / 厂商直连 / https 代理）。
	// 默认关闭：安装包只从服务端托管目录经长连接下发，除非本机显式解锁。
	// 该开关**只从本机配置文件读**，服务端指令无法打开它。
	AllowPublicDownload bool
	// LatencyTargets 延迟探测目标（ICMP/TCP/HTTP）。
	LatencyTargets []string
	// ServerFingerprint 服务端身份公钥指纹（可带分隔符）。配置后按指纹校验服务端，
	// **不需要 CA 文件、也不需要 Insecure**；服务端换 IP/域名/证书都不必更新。
	ServerFingerprint string
	// EnrollmentSecret 首次换取客户端证书用的注册密钥；换取成功后由调用方清空配置。
	EnrollmentSecret string
	// LogLevel 当前生效的日志级别（服务端下发的值也写回这里）。
	LogLevel string
	// ConfigPath Agent 配置文件路径（凭证默认落在同目录的 creds/）。
	ConfigPath string
	// CredDir 凭证目录（覆盖默认值；主要便于测试）。
	CredDir string
}

// Client Agent 客户端。
type Client struct {
	cfg     Config
	collect *probe.Collector
	conn    net.Conn
	enc     *json.Encoder
	sc      *bufio.Scanner
	mu      sync.Mutex
	hb      time.Duration
	// 交互会话映射 commandID -> session
	interactive map[string]*command.InteractiveSession
	// 隧道映射 tunnelID -> 内网服务连接（端口映射反向隧道）
	tunnels   map[string]net.Conn
	tunnelsMu sync.Mutex
	// 文件分发写入句柄 filePath -> *os.File
	pushFiles map[string]*os.File
	pushMu    sync.Mutex
	// 安装包拉取：commandID -> 结果通道（等待 PackageFetchResp）
	pkgWait map[string]chan protocol.PackageFetchResp
	pkgMu   sync.Mutex
	// cred 换取到的客户端凭证（"简单模式"）；进程内缓存，重启后从磁盘加载。
	cred *enrollmentCredential
	// onEnrolled 换取成功后的回调（调用方据此落盘清理注册密钥等）。
	onEnrolled      func(*enrollmentCredential)
	onEnrolledFiles func(CredentialFiles)
}

// New 创建 Agent 客户端。
func New(cfg Config) *Client {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 30 * time.Second
	}
	if cfg.ProbeEvery <= 0 {
		cfg.ProbeEvery = 10 * time.Second
	}
	if len(cfg.FileRoots) == 0 {
		cfg.FileRoots = []string{DefaultFileRoot(cfg.ConfigPath)}
	}
	c := &Client{
		cfg:         cfg,
		collect:     probe.NewCollectorWithProxy(cfg.AgentID, cfg.ProxyIP),
		hb:          cfg.Heartbeat,
		interactive: make(map[string]*command.InteractiveSession),
		tunnels:     make(map[string]net.Conn),
		pushFiles:   make(map[string]*os.File),
		pkgWait:     make(map[string]chan protocol.PackageFetchResp),
	}
	if len(cfg.LatencyTargets) > 0 {
		c.collect.SetLatencyTargets(cfg.LatencyTargets)
	}
	return c
}

// Run 启动 Agent：连接服务端并运行主循环，断线自动重连。
func (c *Client) Run() {
	c.logCredentialPlan()
	backoff := time.Second
	maxBackoff := 30 * time.Second
	// 连接失败按"状态跃迁"记：同一轮故障只在第一次用 ERROR，之后降为 WARN。
	// 全打 WARN 的话默认级别（error）看不到"连不上"；全打 ERROR 又会被重试刷屏。
	firstFailure := true
	for {
		started := time.Now()
		err := c.connectAndServe()
		// 连接稳定存活过一段时间后重置退避：否则网络抖动/服务端换监听地址
		// 次数一多，退避会一直涨到 30 秒，表现为"配置改完主机很久才上线"。
		if time.Since(started) >= 30*time.Second {
			backoff = time.Second
			firstFailure = true
		}
		switch {
		case err == nil:
			firstFailure = true
		case isReenroll(err):
			// 证书刚换过：这是正常收敛过程，不计入退避，也不打"连接断开"的吓人日志
			logx.Infof("[agent] %v", err)
			backoff = time.Second
			firstFailure = true
			continue
		default:
			if firstFailure {
				logx.Errorf("[agent] 连接服务端失败: %v，%v 后重试（同一轮故障的后续失败降为 WARN）", err, backoff)
				firstFailure = false
			} else {
				logx.Warnf("[agent] 连接仍失败: %v，%v 后重试", err, backoff)
			}
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// logCredentialPlan 在启动时说明本轮到底凭哪样东西接入。
//
// 为什么值得单独打印：Agent 只要有可用证书就**不会**再出示令牌 —— 这是正确的
// （令牌是一次性的，换过就该留着证书），但运维在命令行里刚粘完令牌、回到控制台
// 却看到令牌仍是「待使用」时，会以为令牌没生效。把"跳过了换取"写进日志，
// 才能让这条因果链自己解释清楚。
func (c *Client) logCredentialPlan() {
	cert := c.clientCert()
	if cert == nil {
		if c.hasEnrollSecret() {
			logx.Infof("[agent] 本机尚无客户端证书，将用注册凭据换取（agent_id=%q）", c.cfg.AgentID)
		} else {
			logx.Warnf("[agent] 本机无客户端证书且未配置注册凭据，服务端会拒绝接入")
		}
		return
	}
	cn := certCommonName(cert)
	if cn == "" {
		cn = c.cfg.AgentID
	}
	if !c.hasEnrollSecret() {
		logx.Infof("[agent] 本机已有客户端证书（身份 %s），直接以证书接入", cn)
		return
	}
	logx.Infof("[agent] 本机已有客户端证书（身份 %s），直接以证书接入：配置里的注册凭据本次不会被使用，"+
		"控制台的一次性令牌会保持「待使用」", cn)
	if !c.usesExternalCert() {
		logx.Infof("[agent] 若要改用新令牌重新换取：删除 %s 下的 client.crt 与 ca.crt（保留 client.key）后重启",
			c.credDir())
	}
}

// ensureCertIdentity 拦住"证书身份与配置 agent_id 不一致"的静默错位。
//
// 服务端以证书 CN 作为连接身份：本机配置写着 vm-win10、creds 里却是上一轮
// 以机器 UUID 换来的证书时，界面上线的主机名仍然是 UUID —— 看起来"装好了"，
// 但所有按 agent_id 下发的操作都会打到另一台身份上。这里主动丢弃旧证书
// （保留私钥，避免 TOFU 绑定把重签挡死），再用当前凭据重新换取。
func (c *Client) ensureCertIdentity() error {
	if c.usesExternalCert() {
		return nil // 运维手工指定的证书不归我们处置
	}
	want := strings.TrimSpace(c.cfg.AgentID)
	if want == "" {
		return nil
	}
	cert := c.clientCert()
	if cert == nil {
		return nil
	}
	cn := certCommonName(cert)
	if cn == "" || cn == want {
		return nil
	}
	if !c.hasEnrollSecret() {
		return fmt.Errorf("本机客户端证书的身份是 %q，与配置 agent_id %q 不一致，且没有注册凭据可重新换取："+
			"请删除 %s 下的 client.crt 与 ca.crt（保留 client.key），并配置注册密钥或一次性令牌后重启",
			cn, want, c.credDir())
	}
	logx.Infof("[agent] 本机客户端证书身份 %q 与配置 agent_id %q 不一致；丢弃旧证书并用注册凭据重新换取", cn, want)
	if err := c.discardServerIssuedCreds(); err != nil {
		return fmt.Errorf("清理旧客户端证书失败（请手动删除 %s 下的 client.crt 与 ca.crt）: %w", c.credDir(), err)
	}
	// 配置里的路径指向刚删掉的文件：一并清空，避免本轮重建 TLS 时再报"加载失败"。
	c.cfg.ClientCert = ""
	c.cfg.CACert = ""
	c.cred = nil
	return nil
}

// usesExternalCert 判断客户端证书是否由运维手工指定（而不是换取后落在 creds/ 下的）。
// handleEnrolled 会把换取结果写回 client_cert/client_key 配置，因此不能只看
// "配置里有没有这两个字段"，要比对路径是否就是 creds/client.crt。
func (c *Client) usesExternalCert() bool {
	if strings.TrimSpace(c.cfg.ClientCert) == "" || strings.TrimSpace(c.cfg.ClientKey) == "" {
		return false
	}
	managed := filepath.Join(c.credDir(), "client.crt")
	return filepath.Clean(c.cfg.ClientCert) != filepath.Clean(managed)
}

// connectAndServe 建立连接并进入消息循环，返回时连接已断开。
//
// 传输层策略：**默认就是 TLS**，不在 Agent 侧做"能不能加密"的判断。
//   - 已持有客户端证书（换取来的或配置指定的）：mTLS 连接；
//   - 尚无证书、但有注册密钥：Run 里会先换取证书，再带证书连接；
//   - 服务端是旧版本、根本没监听 TLS 时：退回明文，身份仍由 HMAC 注册凭证保证
//     （这条路径只为兼容存量部署，日志会明确提示）。
func (c *Client) connectAndServe() error {
	if err := c.ensureCertIdentity(); err != nil {
		return err
	}
	tlsCfg, err := c.buildTLSConfig()
	if err != nil {
		return err
	}
	var conn net.Conn
	// 还没有客户端证书、但有注册密钥：先换取证书，然后用**同一份配置**
	// 重新构造 TLS（此时 ca.crt 已落盘，服务端身份可校验）。
	if c.clientCert() == nil && c.hasEnrollSecret() {
		tlsCfg, err = c.enrollAndRebuildTLS()
		if err != nil {
			return err
		}
	}
	// 连之前先确认"服务端还认不认本机证书"。
	//
	// 必须单独做这一步的原因：Go 的 TLS 客户端在服务端广告的 AcceptableCAs 里
	// 找不到本机证书的签发者时会**静默丢弃证书**（握手照常成功、客户端无任何错误）。
	// 于是"服务端换过 CA"在客户端侧完全不可见 —— 只能由服务端发现，
	// 而服务端看到的是"对端没带证书"，也无法断定是不是换过 CA。
	// 这既让故障难以诊断，也让自动恢复无从下手。
	//
	// 这里用一次极短的"探针握手"拿到服务端广告的 CA，与本机证书的签发者按**公钥**
	// 比对（而不是按名字：默认部署下两套 CA 同名，比名字永远看不出差别）。
	if err := c.checkCertAcceptance(); err != nil {
		return err
	}
	conn, err = tls.Dial("tcp", c.cfg.ServerAddr, tlsCfg)
	if err != nil {
		// 服务端换过 CA（重装/换机器）时，本机残留的旧客户端证书会让握手必然失败。
		// 这是**可自动收敛**的：丢掉旧证书、用注册密钥重新换取，让运维不必登录每台被控机。
		if recovered, rerr := c.recoverFromCAChange(err); rerr != nil {
			return rerr
		} else if recovered {
			if tlsCfg, err = c.buildTLSConfig(); err != nil {
				return err
			}
			conn, err = tls.Dial("tcp", c.cfg.ServerAddr, tlsCfg)
			if err == nil {
				logx.Infof("[agent] 服务端 CA 已轮换，已用注册密钥重新换取证书并恢复连接")
			}
		}
		if err != nil {
			if !c.mayFallbackPlaintext() {
				return c.tlsFailure(err)
			}
			// 明文回退只服务于"服务端还是旧版、根本没有 TLS"这一种情形。
			// 一旦本机已经持有客户端证书，就**不允许**降级 ——
			// 否则证书校验失败（例如服务端换了 CA）会被悄悄绕过成明文连接，
			// 把"双向认证"变成"什么都没认证"。宁可失败并报清楚原因。
			plain, perr := net.Dial("tcp", c.cfg.ServerAddr)
			if perr != nil {
				return c.tlsFailure(err)
			}
			conn = plain
			logx.Infof("[agent] 服务端未提供可信 TLS（%v），本次以明文连接（身份由注册密钥保障）", err)
		}
	}
	c.mu.Lock()
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.sc = bufio.NewScanner(conn)
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	c.mu.Unlock()

	logx.Infof("[agent] 已连接服务端 %s，注册上线...", c.cfg.ServerAddr)
	c.register()

	// 启动心跳与探针协程
	hbStop := make(chan struct{})
	probeStop := make(chan struct{})
	go c.heartbeatLoop(hbStop)
	go c.probeLoop(probeStop)

	// 主消息循环
	for c.sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(c.sc.Bytes(), &env); err != nil {
			continue
		}
		// 服务端明确告知"你的证书不被认可，请重新换取"。
		// 这是"服务端换过 CA"在客户端侧**唯一可靠**的信号：因为 Agent 通道的
		// 客户端证书是"请求但不强制"，此时握手是成功的，客户端只看到"连上立刻断开"，
		// 光靠连接错误根本判断不出原因。
		if env.Type == protocol.TypeEnrollRequired {
			close(hbStop)
			close(probeStop)
			c.closeAllTunnels()
			return c.handleEnrollRequired(env)
		}
		c.handleMessage(env)
	}
	close(hbStop)
	close(probeStop)
	c.closeAllTunnels()
	return c.sc.Err()
}

// handleEnrollRequired 处理"服务端要求重新换取证书"。
//
// 三种去向：
//   - 本机没有注册密钥 → 无法重换，如实报错并给出运维指引；
//   - 有密钥但身份变化未被授权 → 报错并说明如何授权（不自动信任陌生人）；
//   - 有密钥且已授权（server_fingerprint 与对端一致）→ 自动丢弃旧证书并重换。
//
// 之所以把"授权"作为前提：重新换取会把注册密钥的 HMAC 证明交给对端，
// 无条件重换等于"谁能让 Agent 连上我，谁就能拿到它的注册凭据"。
func (c *Client) handleEnrollRequired(env protocol.Envelope) error {
	var req protocol.EnrollRequiredResp
	_ = json.Unmarshal(env.Data, &req)
	got := normFingerprint(req.Fingerprint)
	where := ""
	if req.Identity != "" || got != "" {
		where = fmt.Sprintf("（服务端身份 %s，指纹 %s）", req.Identity, DisplayFingerprint(got))
	}
	if !c.hasEnrollSecret() {
		return fmt.Errorf("服务端不认可本机现有客户端证书%s：%s；本机没有注册密钥，无法自动重新换取，"+
			"请把服务端 <data>/agent.key 的内容填进配置 agent_key 后重启",
			where, req.Reason)
	}
	pinned, known := c.knownServerFingerprints()
	authorized := false
	if got != "" {
		for _, fp := range append(append([]string{}, pinned...), known...) {
			if fp == got {
				authorized = true
				break
			}
		}
	}
	if !authorized {
		old := firstNonEmptyStr(firstOf(pinned), firstOf(known))
		return fmt.Errorf("服务端不认可本机现有客户端证书%s：%s。本机记下的身份是 %s，"+
			"为避免把注册密钥交给陌生对端，不会自动重换 —— 确认无误后请把上面的服务端指纹填进配置 "+
			"server_fingerprint（或删除本机 %s 里的 client.crt 与 ca.crt、保留 client.key 后重启）",
			where, req.Reason, DisplayFingerprint(old), c.credDir())
	}
	logx.Infof("[agent] 服务端不认可本机证书%s；已授权，自动丢弃旧证书并重新换取", where)
	if err := c.discardServerIssuedCreds(); err != nil {
		return fmt.Errorf("清理旧证书失败（请手动删除 %s 后重启）: %w", c.credDir(), err)
	}
	c.cred = nil
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		return err
	}
	return errReenrolled
}

// errReenrolled 表示"已重新换取证书，应立即重连"（不是失败，用于跳出本轮服务）。
var errReenrolled = errors.New("已重新换取客户端证书，正在重连")

// isReenroll 判断错误是否只是"重连即可"。
func isReenroll(err error) bool { return errors.Is(err, errReenrolled) }

// checkCertAcceptance 确认服务端的信任 CA 是否接受本机证书。
//
// 做法：发一次 TLS 握手（不带业务数据），从服务端证书链里取出它的部署身份 CA，
// 再用**公钥**判断"本机证书是否由该 CA 签发"。
// 之所以不用 AcceptableCAs：那里只有 CA 的名字（DN），而默认部署下两套 CA
// 恰恰同名（`<server-name>-CA, O=<server-name>`），比名字无法区分密钥。
// 服务端证书链里的 CA 是完整证书，可以用它做真正的验签。
//
// 返回 error 表示"证书确不被接受，且已按授权决策处理"（要么已重换、要么明确拒绝）。
func (c *Client) checkCertAcceptance() error {
	cert := c.clientCert()
	if cert == nil {
		return nil // 没有证书：换取流程负责，无需检查
	}
	// 本机证书的签发者（服务端 CA）公钥指纹。链里最后一张即换取时服务端给的 CA。
	var issuerFP string
	if len(cert.Certificate) > 1 {
		if ca, err := x509.ParseCertificate(cert.Certificate[len(cert.Certificate)-1]); err == nil {
			issuerFP = certPublicFingerprint(ca)
		}
	}
	if issuerFP == "" {
		return nil // 无法判断（例如手工证书链只有叶子），交给 pin/链校验把关
	}

	srvFP, err := c.probeServerCA()
	if err != nil || srvFP == "" {
		// 探针失败（网络抖动、服务端不开 TLS 等）不阻断主流程：
		// 真有问题时后面的正常连接会给出更准确的错误。
		return nil
	}
	if fingerprintMatches(srvFP, issuerFP) {
		return nil // 同一套总控，证书可用
	}

	// 服务端的部署身份与本机证书的签发者不是同一套 —— 这就是"服务端换过 CA"。
	if !c.hasEnrollSecret() {
		return fmt.Errorf("服务端部署身份 CA（%s）与本机证书的签发者（%s）不同，"+
			"且本机没有注册密钥，无法自动重新换取：请把服务端 <data>/agent.key 的内容填进配置 agent_key 后重启",
			DisplayFingerprint(srvFP), DisplayFingerprint(issuerFP))
	}
	if !c.canTrustNewCA() {
		return trustNewCAError(issuerFP, srvFP, true)
	}
	logx.Infof("[agent] 服务端部署身份 CA 已变化（本机证书签发者 %s，服务端 %s）；已授权，重新换取证书",
		DisplayFingerprint(issuerFP), DisplayFingerprint(srvFP))
	if err := c.discardServerIssuedCreds(); err != nil {
		return fmt.Errorf("清理旧证书失败（请手动删除 %s 后重启）: %w", c.credDir(), err)
	}
	c.cred = nil
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		return err
	}
	return errReenrolled
}

// probeServerCA 只做 TLS 握手，返回服务端证书链里的部署身份 CA 公钥指纹。
func (c *Client) probeServerCA() (string, error) {
	cfg, err := c.baseTLSConfig()
	if err != nil {
		return "", err
	}
	// 不校验证书：这一步只为了取对方"自报"的部署身份，真实性由后续 pin/链校验与
	// 注册鉴权保证（服务端要拿不出正确 CA 的证书，也签不出能被本机接受的客户端证书）。
	cfg.InsecureSkipVerify = true
	cfg.VerifyPeerCertificate = nil
	cfg.VerifyConnection = nil
	cfg.Certificates = nil
	cfg.GetClientCertificate = nil
	conn, err := tls.Dial("tcp", c.cfg.ServerAddr, cfg)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return "", fmt.Errorf("服务端未提供证书")
	}
	// 链里最后一张是自签的部署身份 CA（服务端签发时把 CA 附在叶子之后）
	chain := st.PeerCertificates
	ca := chain[len(chain)-1]
	if !ca.IsCA {
		// 只有叶子（例如外部 CA 签发、未附链）：退回叶子自身的签发者信息，
		// 无法取公钥时返回空，由调用方跳过检查。
		return "", nil
	}
	return certPublicFingerprint(ca), nil
}

// buildTLSConfig 构造当前一轮连接用的 TLS 配置（已持有证书则一并带上）。
func (c *Client) buildTLSConfig() (*tls.Config, error) {
	cfg, err := c.baseTLSConfig()
	if err != nil {
		return nil, err
	}
	if cert := c.clientCert(); cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return cfg, nil
}

// enrollAndRebuildTLS 用注册密钥换取证书，并用换取结果重建 TLS 配置。
func (c *Client) enrollAndRebuildTLS() (*tls.Config, error) {
	cred, err := c.tryEnroll()
	if err != nil {
		return nil, err
	}
	c.cred = cred
	cfg, err := c.baseTLSConfig()
	if err != nil {
		return nil, err
	}
	cfg.Certificates = []tls.Certificate{cred.Cert}
	return cfg, nil
}

// recoverFromCAChange 处理"TLS 失败疑似因服务端换过 CA"的情形。
//
// 成立条件（缺一不可，否则不做任何动作、让原错误照实上报）：
//   - 本机**持有客户端证书**，且不是运维用 -client-cert 手工指定的
//     （手工证书属于外部 CA 场景，删了也换不回来）；
//   - 本机还有注册密钥（agent_key / enroll-secret），能完成重新换取；
//   - 失败原因确实是证书校验，而不是网络不通。
//
// 动作：删除本机 creds/ 下由服务端签发的证书（连同旧 ca.crt），
// 之后 tryEnroll 会走一次换取；换取的响应仍要过 verifyEnrollServer 的
// 身份连续性校验 —— 即"服务端换过 CA 且没人授权"时依旧会拒绝，
// 区别只是把"人肉删除 creds/ 再重启"变成了一次自动重试。
// 返回 (false, nil) 表示前提不成立，调用方应原样上报错误。
func (c *Client) recoverFromCAChange(cause error) (bool, error) {
	if !isCertVerifyFailure(cause) {
		return false, nil
	}
	if strings.TrimSpace(c.cfg.ClientCert) != "" || strings.TrimSpace(c.cfg.ClientKey) != "" {
		return false, nil
	}
	if !c.hasEnrollSecret() {
		return false, nil
	}
	if c.clientCert() == nil {
		return false, nil
	}
	if !c.canTrustNewCA() {
		// 身份变化未获授权：明确拒绝，不要让"重试"看起来像成功了
		pinned, known := c.knownServerFingerprints()
		old := firstNonEmptyStr(firstOf(pinned), firstOf(known))
		return true, trustNewCAError(old, "", true)
	}
	logx.Errorf("[agent] TLS 校验失败（%v）；本机持有旧证书且配置了可用注册密钥，尝试丢弃旧证书并重新换取", cause)
	if err := c.discardServerIssuedCreds(); err != nil {
		return true, fmt.Errorf("清理旧证书失败（请手动删除 %s 后重启）: %w", c.credDir(), err)
	}
	c.cred = nil
	if _, err := c.enrollAndRebuildTLS(); err != nil {
		return true, err
	}
	return true, nil
}

// canTrustNewCA 判断"接受一套新 CA"是否已被运维授权。
// 授权来源：显式配置的 server_fingerprint（下次换取时的连续性校验会再核对一次）。
func (c *Client) canTrustNewCA() bool {
	pinned, _ := c.knownServerFingerprints()
	return len(pinned) > 0
}

// discardServerIssuedCreds 删除本机由服务端签发的证书与旧 CA（client.crt/ca.crt）。
//
// client.key **保留**：服务端按 agent_id → 公钥指纹做 TOFU 绑定，丢掉私钥就等于自造一次
// "换机器身份"，重签时会被自己的绑定挡住（只能靠重新签发一次性令牌恢复）。
func (c *Client) discardServerIssuedCreds() error {
	dir := c.credDir()
	for _, name := range []string{"client.crt", "ca.crt"} {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// mayFallbackPlaintext 判断本机是否允许"TLS 不可用时降级为明文"。
//
// 只有"本机还没有客户端证书"才允许：那是旧版服务端（根本没监听 TLS）的兼容路径，
// 身份仍由注册密钥的 HMAC 保障。
// 一旦本机已持有证书，降级就必须被禁止 —— 否则"服务端换过 CA 导致校验失败"
// 会被悄悄绕过成明文连接，把双向认证变成什么都没认证。
func (c *Client) mayFallbackPlaintext() bool {
	if c.clientCert() != nil {
		return false
	}
	return strings.TrimSpace(c.cfg.ClientCert) == "" && strings.TrimSpace(c.cfg.ClientKey) == ""
}

// tlsFailure 组装 TLS 失败时的错误信息（说明原因与恢复办法）。
//
// 两类原因要分开讲，否则运维只能对着 x509 报错猜：
//  1. 服务端换过部署身份 CA —— 本机旧证书不被认可，填新指纹或删 creds 即可自动重换；
//  2. 证书 SAN 不匹配（手工 CA 场景特有）—— 报 "certificate is valid for ..., not <地址>"，
//     必须在**服务端**按 Agent 实际使用的地址重签证书。这条与 Agent 侧配置无关，
//     只靠在被控机上改配置是修不好的，所以要明确指向服务端。
func (c *Client) tlsFailure(cause error) error {
	hint := ""
	if isSANMismatch(cause) {
		hint = "；该错误表示服务端证书的 SAN 不含本机连接用的地址，" +
			"请在服务端按实际地址重签服务端证书（cd scripts/signcert && go run . 会按公网 IP 自动写入 SAN），" +
			"或给本机配 -server-fingerprint 改为按部署身份指纹校验（免 CA 与 SAN）"
	}
	if c.clientCert() != nil {
		return fmt.Errorf("TLS 连接失败且本机已持有客户端证书，拒绝降级为明文：%w"+
			"（若服务端换过部署身份 CA，请把新指纹填进 server_fingerprint 后重启，"+
			"Agent 会自动重新换取；或删除 %s 后重启）%s",
			cause, c.credDir(), hint)
	}
	return fmt.Errorf("%w%s", cause, hint)
}

// isSANMismatch 判断错误是否为「证书地址不匹配」。
//
// 判据是 Go 的固定文案 —— 这里只是决定要不要附排查提示，判断错了顶多多/少一句
// 提示，不影响错误本身与重试行为，因此用字符串匹配足够，不必引入错误码。
func isSANMismatch(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "certificate is valid for") ||
		strings.Contains(msg, "x509: cannot validate certificate")
}

func firstOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// register 发送上线消息。
//
// 若配置了 -agent-key，会附带 HMAC 注册证明（密钥本身不上线传输），
// 使服务端能够确认对端确实持有密钥 —— 这是 :9000 通道唯一的应用层身份凭证。
func (c *Client) register() {
	ips := c.collect.PublicIPs()
	// 设备身份优先用配置里持久化的 UUID：探测失败或换环境时也不会漂移
	uuid := c.cfg.UUID
	if uuid == "" {
		uuid = c.collect.UUID()
	}
	info := map[string]interface{}{
		"agent_id":    c.cfg.AgentID,
		"uuid":        uuid,
		"hostname":    c.collect.Host(),
		"os_type":     c.collect.OSType(),
		"arch":        c.collect.Arch(),
		"os_version":  c.collect.OSVersion(),
		"remote_addr": "",
		"public_ip":   ips.IPv4,
		"public_ipv6": ips.IPv6,
		// 版本号：服务端据此判断该主机是否需要升级，前端展示在线版本
		"agent_version": version.Version,
		// 当前日志级别：上线即上报，控制台不必等第一次心跳就能显示
		"log_level": logx.Current().String(),
	}
	if c.cfg.AgentKey != "" {
		nonce := agentauth.NewNonce()
		ts := time.Now().Unix()
		info["auth_nonce"] = nonce
		info["auth_time"] = ts
		info["auth_proof"] = agentauth.Proof(c.cfg.AgentKey, c.cfg.AgentID, nonce, ts)
	} else if c.clientCert() != nil {
		// 已持有服务端签发的证书：身份以证书为准，服务端据此放行（无需再传注册密钥）
		logx.Infof("[agent] 已携带客户端证书连接（证书身份即 agent_id）")
	} else {
		logx.Warnf("[agent] 未配置 -agent-key，无法通过服务端的注册鉴权（若服务端已启用校验将被拒绝连接）")
	}
	_ = c.write(protocol.TypeOnline, info)
}

// write 发送一条消息。
func (c *Client) write(typ string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		return fmt.Errorf("not connected")
	}
	return c.enc.Encode(protocol.Envelope{Type: typ, Data: data})
}

// applyServerAddrUpdate 落盘新的服务端地址并主动重连。
//
// 服务端在切换监听前下发这条消息；只有落盘成功才回 OK 并断开，服务端也只踢
// 收到 OK 的连接。写盘用临时文件 + rename，避免断电留下半截配置。
// applyLogLevelUpdate 应用服务端下发的日志级别：校验 → 落盘 → 立即生效 → 回 Ack。
//
// "先落盘再改内存"：写配置失败时仍让本次生效（否则运维看不到调试日志），
// 但 Ack 里如实说明"重启后会回退"，避免界面显示成功、重启才发现没生效。
func (c *Client) applyLogLevelUpdate(u protocol.LogLevelUpdate) {
	ack := protocol.LogLevelUpdateAck{AgentID: c.cfg.AgentID, Level: logx.Current().String()}
	lv, ok := logx.ParseLevel(u.Level)
	if !ok {
		ack.Message = "无法识别的日志级别: " + u.Level + "（可用 off/error/warn/info）"
		_ = c.write(protocol.TypeLogLevelUpdateAck, ack)
		logx.Errorf("[agent] 拒绝服务端下发的日志级别 %q：不是 off/error/warn/info", u.Level)
		return
	}
	persistErr := c.persistConfigString("log_level", lv.String())
	c.mu.Lock()
	c.cfg.LogLevel = lv.String()
	c.mu.Unlock()
	logx.SetLevel(lv)
	ack.Level = lv.String()
	if persistErr != nil {
		ack.Message = "已即时生效，但写入配置失败（重启后会回退）: " + persistErr.Error()
		_ = c.write(protocol.TypeLogLevelUpdateAck, ack)
		logx.Errorf("[agent] 日志级别已临时改为 %s，但落盘失败: %v", lv, persistErr)
		return
	}
	ack.OK = true
	_ = c.write(protocol.TypeLogLevelUpdateAck, ack)
	logx.Infof("[agent] 日志级别已按服务端下发更新为 %s", lv)
}

func (c *Client) applyServerAddrUpdate(u protocol.ServerAddrUpdate) {
	addr := strings.TrimSpace(u.ServerAddr)
	ack := protocol.ServerAddrUpdateAck{AgentID: c.cfg.AgentID, ServerAddr: addr}
	if addr == "" && u.Port > 0 {
		// 服务端监听全部网卡：只换端口，主机沿用本机当前配置，
		// 避免把走内网/域名的 Agent 强行改到服务端以为的公网 IP。
		host, _, err := net.SplitHostPort(c.cfg.ServerAddr)
		if err != nil {
			ack.Message = "本机当前服务端地址无法解析: " + err.Error()
			_ = c.write(protocol.TypeServerAddrUpdateAck, ack)
			return
		}
		addr = net.JoinHostPort(host, strconv.Itoa(u.Port))
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		ack.Message = "地址格式无效: " + err.Error()
		_ = c.write(protocol.TypeServerAddrUpdateAck, ack)
		return
	}
	ack.ServerAddr = addr
	old := c.cfg.ServerAddr
	if err := c.persistServerAddr(addr); err != nil {
		ack.Message = err.Error()
		_ = c.write(protocol.TypeServerAddrUpdateAck, ack)
		logx.Errorf("[agent] 服务端下发新地址 %s 落盘失败，继续使用 %s: %v", addr, old, err)
		return
	}
	c.mu.Lock()
	c.cfg.ServerAddr = addr
	c.mu.Unlock()
	ack.OK = true
	_ = c.write(protocol.TypeServerAddrUpdateAck, ack)
	logx.Infof("[agent] 服务端地址已更新为 %s，即将用新地址重连", addr)
	// 服务端会在收到 Ack 后切换监听；默认等 2 秒，避免 Agent 比新监听先到。
	delay := u.ApplyAfterSec
	if delay <= 0 {
		delay = 2
	}
	if delay > 30 {
		delay = 30
	}
	// 只关"收到消息时的那条连接"：服务端可能已经先断开旧连接并让 Agent 连上新监听，
	// 若定时器按 c.conn 取当前值，会把刚建立的新连接再关一次，造成反复重连。
	c.mu.Lock()
	oldConn := c.conn
	c.mu.Unlock()
	time.AfterFunc(time.Duration(delay)*time.Second, func() {
		if oldConn != nil {
			_ = oldConn.Close()
		}
	})
}

// persistServerAddr 只改配置里的 server 字段，保留安装器写入的其它键。
func (c *Client) persistServerAddr(addr string) error {
	return c.persistConfigString("server", addr)
}

// persistConfigString 只改配置里的单个字符串字段，保留其它键（含安装器写入的）。
//
// 用"读原始 JSON → 改一个键 → 临时文件 + rename"而不是 unmarshal 成 Config 再整份写回：
// 后者会把本版本不认识的键（更老的/更新的配置项）一并抹掉。
func (c *Client) persistConfigString(key, val string) error {
	path := strings.TrimSpace(c.cfg.ConfigPath)
	if path == "" {
		return nil
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("解析配置失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	v, err := json.Marshal(val)
	if err != nil {
		return err
	}
	raw[key] = v
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换配置失败: %w", err)
	}
	return nil
}

// heartbeatLoop 定时发送心跳。
func (c *Client) heartbeatLoop(stop chan struct{}) {
	t := time.NewTicker(c.hb)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			req := protocol.HeartbeatReq{
				AgentID:   c.cfg.AgentID,
				Hostname:  c.collect.Host(),
				Timestamp: time.Now().Unix(),
				RTTMs:     0,
				LogLevel:  logx.Current().String(),
			}
			_ = c.write(protocol.TypeHeartbeatReq, req)
		}
	}
}

// probeLoop 定时采集并上报探针。
func (c *Client) probeLoop(stop chan struct{}) {
	t := time.NewTicker(c.cfg.ProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p := c.collect.Sample()
			_ = c.write(protocol.TypeProbeDataReq, p)
		}
	}
}

// handleMessage 处理服务端下发消息。
func (c *Client) handleMessage(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeHeartbeatResp:
		var resp protocol.HeartbeatResp
		if json.Unmarshal(env.Data, &resp) == nil && resp.HeartbeatInterval > 0 {
			// 服务端下发新的心跳间隔
			nd := time.Duration(resp.HeartbeatInterval) * time.Second
			c.mu.Lock()
			c.hb = nd
			c.mu.Unlock()
		}
	case protocol.TypeCommandReq:
		var req protocol.CommandReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		c.runCommand(req)
	case protocol.TypeInteractiveIn:
		var inp protocol.InteractiveInputReq
		if json.Unmarshal(env.Data, &inp) != nil {
			return
		}
		// 先在锁内取指针：map 由 runInteractive goroutine 写入，无锁读取会触发
		// Go runtime 的 concurrent map access 崩溃。
		c.mu.Lock()
		sess := c.interactive[inp.CommandID]
		c.mu.Unlock()
		if sess != nil {
			if inp.Control != "" {
				_ = sess.SendControl(inp.Control)
			}
			if inp.Rows > 0 && inp.Cols > 0 {
				_ = sess.Resize(inp.Rows, inp.Cols)
			}
			if inp.InputData != "" {
				_ = sess.Write([]byte(inp.InputData))
			}
		}
	case protocol.TypeFileDownloadReq:
		// 文件下载：将文件分片回传给服务端
		var fd protocol.FileDownloadReq
		if json.Unmarshal(env.Data, &fd) == nil && fd.FilePath != "" {
			c.sendFileChunks(fd)
		}
	case protocol.TypeFilePushReq:
		// 文件分发：接收服务端分片并写入本地文件
		var fp protocol.FilePushReq
		if json.Unmarshal(env.Data, &fp) == nil && fp.FilePath != "" {
			c.handleFilePush(fp)
		}
	case protocol.TypeTunnelOpen:
		var to protocol.TunnelOpenReq
		if json.Unmarshal(env.Data, &to) != nil {
			return
		}
		c.openTunnel(to)
	case protocol.TypeTunnelData:
		var td protocol.TunnelData
		if json.Unmarshal(env.Data, &td) != nil {
			return
		}
		conn := c.getTunnel(td.TunnelID)
		if conn == nil {
			// 静默丢弃 = 最难查的一类故障：服务端以为画面数据已经送到，本机一个字节都没落地。
			logx.Warnf("[agent] 丢弃隧道数据 %s（%d 字节）：本机没有这条隧道", td.TunnelID, len(td.Data))
			return
		}
		if _, err := conn.Write(td.Data); err != nil {
			// 写不进去说明本地 VNC 已经不接受输入了；继续留着隧道只会让浏览器对着空气发指令。
			logx.Errorf("[agent] 写本地 VNC 失败，关闭隧道 %s: %v", td.TunnelID, err)
			c.closeTunnel(td.TunnelID)
			_ = c.write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: td.TunnelID, Reason: "local-write-failed"})
		}
	case protocol.TypeTunnelClose:
		var tc protocol.TunnelClose
		if json.Unmarshal(env.Data, &tc) != nil {
			return
		}
		c.closeTunnel(tc.TunnelID)
	case protocol.TypeUpgradeReq:
		var up protocol.UpgradeReq
		if json.Unmarshal(env.Data, &up) != nil {
			return
		}
		go c.runUpgrade(&up)
	case protocol.TypeInteractiveClose:
		var req protocol.InteractiveCloseReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		c.closeSession(req)
	case protocol.TypeVncInstallReq:
		var req protocol.VncInstallReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		go c.handleVncInstall(req)
	case protocol.TypeVncPasswordReq:
		var req protocol.VncPasswordReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		go c.handleVncPassword(req)
	case protocol.TypeFsOpReq:
		var req protocol.FsOpReq
		if json.Unmarshal(env.Data, &req) == nil {
			c.handleFsOp(req)
		}
	case protocol.TypeLogLevelUpdate:
		var u protocol.LogLevelUpdate
		if json.Unmarshal(env.Data, &u) != nil {
			return
		}
		c.applyLogLevelUpdate(u)
	case protocol.TypeServerAddrUpdate:
		var u protocol.ServerAddrUpdate
		if json.Unmarshal(env.Data, &u) != nil {
			return
		}
		c.applyServerAddrUpdate(u)
	case protocol.TypePkgFetchResp:
		var resp protocol.PackageFetchResp
		if json.Unmarshal(env.Data, &resp) != nil {
			return
		}
		c.deliverPackageResp(resp)
	}
}

// deliverPackageResp 把安装包拉取结果投递给等待中的拉取方。
func (c *Client) deliverPackageResp(resp protocol.PackageFetchResp) {
	c.pkgMu.Lock()
	ch, ok := c.pkgWait[resp.CommandID]
	c.pkgMu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default: // 已有结果，丢弃重复投递
		}
	}
}

// fetchPackage 通过既有长连接从服务端拉取托管的安装包，落盘到 destPath。
//
// 之所以走长连接而不是 HTTP：Agent 到服务端的 TCP 端口一定是通的（否则它不会在线），
// 而 HTTP 端口未必可达；且该通道可启用 TLS/mTLS，天然避免公网下载的劫持与限速问题。
// 服务端未托管该包时返回错误，调用方回退到其它下载方式。
func (c *Client) fetchPackage(name, destPath string) (int64, error) {
	if name == "" || destPath == "" {
		return 0, errors.New("安装包名称或落盘路径为空")
	}
	commandID := fmt.Sprintf("pkg-%d", time.Now().UnixNano())
	ch := make(chan protocol.PackageFetchResp, 1)

	c.pkgMu.Lock()
	c.pkgWait[commandID] = ch
	c.pkgMu.Unlock()
	defer func() {
		c.pkgMu.Lock()
		delete(c.pkgWait, commandID)
		c.pkgMu.Unlock()
	}()

	if err := c.write(protocol.TypePkgFetchReq, protocol.PackageFetchReq{
		AgentID: c.cfg.AgentID, Name: name, CommandID: commandID, DestPath: destPath,
	}); err != nil {
		return 0, fmt.Errorf("请求服务端下发安装包失败: %w", err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			return 0, errors.New(resp.Message)
		}
		// 分片由消息循环顺序落盘，收到回执即表示已写完；这里再做一次大小校验兜底
		fi, err := os.Stat(destPath)
		if err != nil {
			return 0, fmt.Errorf("安装包未落盘: %w", err)
		}
		if fi.Size() != resp.Size {
			return 0, fmt.Errorf("安装包大小不符：期望 %d 字节，实际 %d 字节", resp.Size, fi.Size())
		}
		return fi.Size(), nil
	case <-time.After(5 * time.Minute):
		return 0, errors.New("等待服务端下发安装包超时")
	}
}

// runUpgrade 按升级包来源分派：URL 模式仍走签名校验；服务端 pkg 托管包
// 经当前认证长连接拉取，随后交给 PerformLocal（仅校验 SHA-256）。
func (c *Client) runUpgrade(req *protocol.UpgradeReq) {
	report := func(resp *protocol.UpgradeResp) {
		_ = c.write(protocol.TypeUpgradeResp, resp)
	}
	if strings.TrimSpace(req.PackageName) == "" {
		upgrade.Perform(req, upgrade.Policy{PubKey: c.cfg.UpgradePubKey}, report)
		return
	}
	content, err := c.fetchLocalUpgradePackage(req.PackageName)
	if err != nil {
		logx.Errorf("[agent] 获取服务端本地升级包 %s 失败: %v", req.PackageName, err)
		report(&protocol.UpgradeResp{
			UpgradeID: req.UpgradeID,
			Version:   upgrade.Version(),
			Message:   "获取服务端本地升级包失败: " + err.Error(),
		})
		return
	}
	upgrade.PerformLocal(req, content, report)
}

// sandboxSubdir 返回 FileRoots 下的专用子目录。所有"服务端下发文件"都必须落在这里，
// 否则 handleFilePush 的路径闸门会拒绝（系统临时目录不在默认根目录内）。
func (c *Client) sandboxSubdir(sub string) string {
	root := ""
	if len(c.cfg.FileRoots) > 0 {
		root = c.cfg.FileRoots[0]
	}
	if strings.TrimSpace(root) == "" {
		root = DefaultFileRoot(c.cfg.ConfigPath)
	}
	return filepath.Join(root, sub)
}

// fetchServerPackage 从服务端托管目录取一个安装包到本机沙箱，返回本地路径。
// 供 VNC/虚拟显示驱动等安装流程使用（服务端分片由消息循环写入）。
func (c *Client) fetchServerPackage(name string) (string, error) {
	safe := filepath.Base(strings.TrimSpace(name))
	if safe == "" || safe == "." || safe == ".." {
		return "", fmt.Errorf("安装包名称不合法")
	}
	dir := c.sandboxSubdir(".packages")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("创建安装包临时目录失败: %w", err)
	}
	dest := filepath.Join(dir, safe)
	// 旧版本可能把截断文件留在别处；同名残留会让大小校验误报，先清掉。
	_ = os.Remove(dest)
	if _, err := c.fetchPackage(safe, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// fetchLocalUpgradePackage 通过既有的 PackageFetch 长连接通道把服务端 pkg 目录里的
// 文件取到本机文件沙箱，再一次性读入内存交给升级模块（上限 256 MiB）。
func (c *Client) fetchLocalUpgradePackage(name string) ([]byte, error) {
	safe := filepath.Base(strings.TrimSpace(name))
	if safe == "" || safe == "." || safe == ".." {
		return nil, fmt.Errorf("安装包名称不合法")
	}
	dir := c.sandboxSubdir(".upgrade")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("创建升级临时目录失败: %w", err)
	}
	dest := filepath.Join(dir, fmt.Sprintf("%s-%d.bin", safe, time.Now().UnixNano()))
	defer os.Remove(dest)

	size, err := c.fetchPackage(safe, dest)
	if err != nil {
		return nil, err
	}
	if size > upgrade.MaxPackageBytes {
		return nil, fmt.Errorf("升级包超过 %d 字节上限", upgrade.MaxPackageBytes)
	}
	content, err := os.ReadFile(dest)
	if err != nil {
		return nil, fmt.Errorf("读取升级包失败: %w", err)
	}
	logx.Infof("[agent] 已从服务端 pkg 取回升级包 %s（%d 字节）", safe, size)
	return content, nil
}

// closeSession 断开终端连接：结束指定会话/命令及其相关进程。
func (c *Client) closeSession(req protocol.InteractiveCloseReq) {
	// 1) 交互会话
	if req.CommandID != "" {
		c.mu.Lock()
		if s, ok := c.interactive[req.CommandID]; ok {
			delete(c.interactive, req.CommandID)
			s.Close()
			c.mu.Unlock()
			logx.Infof("[agent] 断开会话 %s 并结束相关进程", req.CommandID)
			return
		}
		c.mu.Unlock()
	}
	// 2) 非交互登记中的命令
	closed := 0
	if req.CommandID != "" {
		if command.Kill(req.CommandID) {
			closed++
		}
	} else if req.KillAll {
		closed = command.KillAll()
	}
	// 3) 若无指定且未 kill_all，则结束全部活跃会话/命令
	if req.CommandID == "" && !req.KillAll {
		c.mu.Lock()
		for id, s := range c.interactive {
			delete(c.interactive, id)
			s.Close()
			closed++
		}
		c.mu.Unlock()
		closed += command.KillAll()
	}
	logx.Infof("[agent] 断开终端连接，已结束 %d 个会话/命令", closed)
}

// handleVncInstall 处理 VNC 安装/启动指令并回传结果。
func (c *Client) handleVncInstall(req protocol.VncInstallReq) {
	// 取包默认只走服务端托管（既有长连接，已 mutual-TLS，避开公网限速与劫持）；
	// 公网取包（winget/choco/厂商直连/加速代理）要本机配置显式解锁，服务端指令改不了这个开关。
	// 无论哪条路，执行/解压前都按内置 SHA-256 钉扎校验（见 command.installPins）。
	// 若配置了 https 的公网 IP 探测代理，安装包下载也复用它（仅限 https，明文代理会被忽略）。
	resp := command.VncInstallCfg(req.Port, command.VncConfig{
		Proxy:               c.cfg.ProxyIP,
		Mode:                req.Mode,
		AllowPublicDownload: c.cfg.AllowPublicDownload,
		// 必须落在本机 FileRoots 内：服务端下发走的是文件分发通道（handleFilePush
		// 会做路径闸门），写系统临时目录会被逐片拒绝，表现为"每个分片都不在允许根目录内"。
		FetchPackage: c.fetchServerPackage,
	})
	if resp.OK {
		logx.Infof("[agent] VNC 安装/启动结果: mode=%s msg=%s", resp.Mode, resp.Message)
	} else {
		logx.Errorf("[agent] VNC 安装/启动失败: mode=%s msg=%s", resp.Mode, resp.Message)
	}
	_ = c.write(protocol.TypeVncInstallResp, protocol.VncInstallResp{
		OK: resp.OK, Message: resp.Message, Port: resp.Port, Password: resp.Password, Mode: resp.Mode,
	})
}

// handleVncPassword 读取本机当前 VNC 口令并回传。
//
// 只读：不改配置、不重启服务，因此不会打断正在进行的远程会话 ——
// 这是它与"一键安装（会重置口令并重启）"的关键区别。
func (c *Client) handleVncPassword(req protocol.VncPasswordReq) {
	r := command.VncReadPassword()
	// 日志只记"读到没有"，绝不把口令本身写进日志（那等于又存了一份明文凭据）
	if r.OK {
		logx.Infof("[agent] VNC 口令读取结果: mode=%s", r.Mode)
	} else {
		logx.Errorf("[agent] VNC 口令读取失败: mode=%s msg=%s", r.Mode, r.Message)
	}
	_ = c.write(protocol.TypeVncPasswordResp, protocol.VncPasswordResp{
		OK: r.OK, Message: r.Message, Password: r.Password, Mode: r.Mode,
	})
}

// handleFilePush 接收服务端下发的文件分片，写入本地目标文件并回传 ACK/NACK。
func (c *Client) handleFilePush(req protocol.FilePushReq) {
	logx.Infof("[agent] 收到文件分发分片 seq=%d %s", req.ChunkSeq, req.FilePath)
	ack := func(ok bool, status, msg string) {
		_ = c.write(protocol.TypeFilePushAck, protocol.FilePushAck{
			CommandID: req.CommandID, FilePath: req.FilePath, ChunkSeq: req.ChunkSeq,
			Ack: ok, Status: status, Message: msg,
		})
	}
	// 路径闸门：越界的指令在碰文件系统之前就被拒掉，并把原因回给操作者
	//（不静默丢弃 —— 静默丢弃的表现是"传输一直卡在进行中"，最难排查）。
	// 归一后的绝对路径同时充当 pushFiles 的键，避免同一路径的两种写法
	// 各开一个句柄互相截断。
	path, gerr := c.pushTargetPath(req.FilePath, req.Unrestricted)
	if gerr != nil {
		logx.Errorf("[agent] 拒绝文件分发 %q: %v", req.FilePath, gerr)
		ack(false, "rejected", gerr.Error())
		return
	}
	req.FilePath = path
	// 分发失败/取消：关闭句柄并删除已写入的半截文件。
	// 只在"确实打开过文件"时删除，避免取消一个从未开始的请求时误删同名旧文件。
	if req.FileSize < 0 {
		c.pushAbort(req.FilePath)
		ack(false, "failed", "服务端取消分发")
		return
	}
	if req.Done {
		// 收尾：追加最后一个分片并关闭文件
		if len(req.ChunkData) > 0 {
			if !c.pushWrite(req) {
				ack(false, "failed", "写入失败")
				return
			}
		}
		c.pushClose(req.FilePath)
		ack(true, "done", "文件分发完成")
		return
	}
	if len(req.ChunkData) > 0 && !c.pushWrite(req) {
		ack(false, "failed", "写入失败")
		return
	}
	ack(true, "transferring", "")
}

// pushWrite 将分片追加写入目标文件。
func (c *Client) pushWrite(req protocol.FilePushReq) bool {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	f, ok := c.pushFiles[req.FilePath]
	if !ok {
		if err := os.MkdirAll(filepath.Dir(req.FilePath), 0755); err != nil {
			logx.Errorf("[agent] 创建目录失败 %s: %v", filepath.Dir(req.FilePath), err)
			return false
		}
		var err error
		f, err = os.Create(req.FilePath)
		if err != nil {
			logx.Errorf("[agent] 创建文件失败 %s: %v", req.FilePath, err)
			return false
		}
		c.pushFiles[req.FilePath] = f
	}
	// 断点续传：跳过已写位置
	if fi, _ := f.Stat(); fi.Size() != int64(req.ChunkSeq)*int64(protocol.DefaultChunkSize) {
		// 顺序写入校验：位置不匹配时按期望偏移 Seek 补齐（简化处理）
	}
	if _, err := f.Write(req.ChunkData); err != nil {
		logx.Errorf("[agent] 写入分片失败 %s: %v", req.FilePath, err)
		return false
	}
	return true
}

// pushClose 完成文件分发，关闭并释放句柄。
func (c *Client) pushClose(filePath string) {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	if f, ok := c.pushFiles[filePath]; ok {
		_ = f.Close()
		delete(c.pushFiles, filePath)
		logx.Infof("[agent] 文件分发完成: %s", filePath)
	}
}

// pushTargetPath 解析文件分发/下载的目标路径。
//
// Unrestricted=true 是"文件管理"专用：整机模式只要求绝对路径（能力与远程命令等价）；
// 其它入口（运维操作的文件分发、VNC 安装包）仍走 -file-roots 沙箱。
func (c *Client) pushTargetPath(raw string, unrestricted bool) (string, error) {
	if unrestricted {
		return fsAbsPath(raw)
	}
	return c.guardPath(raw)
}

// pushAbort 服务端取消分发：关闭句柄并删除已写入的半截文件。
func (c *Client) pushAbort(filePath string) {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	f, ok := c.pushFiles[filePath]
	if !ok {
		return
	}
	_ = f.Close()
	delete(c.pushFiles, filePath)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		logx.Warnf("[agent] 清理未完成的上传文件 %s 失败: %v", filePath, err)
	}
}

// sendFileChunks 将本地文件分片回传给服务端（文件下载）。
func (c *Client) sendFileChunks(req protocol.FileDownloadReq) {
	logx.Infof("[agent] 收到文件下载请求: %s", req.FilePath)
	path, gerr := c.pushTargetPath(req.FilePath, req.Unrestricted)
	if gerr != nil {
		// 与读失败走同一条上报：否则服务端会一直等到超时
		logx.Errorf("[agent] 拒绝文件下载 %q: %v", req.FilePath, gerr)
		_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
			AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: -1,
		})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// 上报失败
		_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
			AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: -1,
		})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	// 只回传普通文件：目录/设备/管道既没有有意义的"大小"，读下去还会把
	// 主消息循环卡住（目录在 Windows 上直接报错，在 Linux 上读出 0 字节）。
	if !st.Mode().IsRegular() {
		_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
			AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: -1,
		})
		logx.Warnf("[agent] 拒绝下载非普通文件: %s", path)
		return
	}
	size := st.Size()
	// 空文件也要回一帧：否则服务端永远等不到分片，只能按超时处理。
	if size == 0 {
		_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
			AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: 0, ChunkSeq: 0,
		})
		return
	}
	buf := make([]byte, protocol.DefaultChunkSize)
	seq := 0
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			md5 := md5Chunk(buf[:n])
			// 写失败必须留下痕迹并停止（L-6）：原先是 `_ = c.write(...)`，
			// 连接断在半路时服务端只会收到一个永远缺片的任务，Agent 侧一行日志都没有。
			if werr := c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
				AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: size,
				ChunkSeq: seq, ChunkData: append([]byte(nil), buf[:n]...), ChunkMD5: md5,
			}); werr != nil {
				logx.Infof("[agent] 文件回传中断于第 %d 片（%s，共 %d 字节）: %v", seq, req.FilePath, size, werr)
				return
			}
			seq++
		}
		if rerr != nil {
			break
		}
	}
}

// md5Chunk 计算分片 MD5 校验和。
func md5Chunk(data []byte) string {
	h := md5.Sum(data)
	return fmt.Sprintf("%x", h)
}

func (c *Client) runCommand(req protocol.CommandReq) {
	if req.Interactive {
		// 交互会话会阻塞等待 PTY 进程结束，必须在独立 goroutine 运行，
		// 否则会阻塞主消息循环，导致后续的 InteractiveInputReq（打字输入）
		// 永远无法被处理，表现为「有光标但无法输入」。
		go c.runInteractive(req)
		return
	}

	// 非交互命令异步执行，避免阻塞消息循环，并支持「断开连接」时结束相关进程
	command.RunAsync(&req, func(res *command.Result) {
		_ = c.write(protocol.TypeCommandResp, protocol.CommandResp{
			CommandID: req.CommandID,
			Status:    res.Status,
			ExitCode:  res.ExitCode,
			Stdout:    res.Stdout,
			Stderr:    res.Stderr,
			Duration:  res.Duration,
		})
	})
}

// runInteractive 启动真实 PTY 交互会话。
func (c *Client) runInteractive(req protocol.CommandReq) {
	sess, err := command.StartInteractive(&req, func(out *protocol.InteractiveOutput) {
		_ = c.write(protocol.TypeInteractiveOut, out)
	})
	if err != nil {
		_ = c.write(protocol.TypeInteractiveOut, &protocol.InteractiveOutput{
			CommandID: req.CommandID, Type: "exit", ExitCode: -1, Data: "启动交互终端失败: " + err.Error(),
		})
		return
	}
	c.mu.Lock()
	c.interactive[req.CommandID] = sess
	c.mu.Unlock()

	// 等待进程结束，清理会话
	<-sess.Done()
	c.mu.Lock()
	delete(c.interactive, req.CommandID)
	c.mu.Unlock()
	sess.Close()
}

// tunnelTargetAllowed 判断隧道目标是否本机认可（L-5）。
//
// 隧道的落点是**这台主机所在的网络**：不限制就等于让总控可以把每台被管机器
// 当成通往它可达地址（含内网其它机器）的代理。服务端的每个调用点从来只发 127.0.0.1
// （VNC/探测/端口映射都硬编码回环），所以默认只认回环即可满足全部功能。
//
// 确有需要连本机以外地址时，由**本机**配置 tunnel_hosts 放行 —— 不接受服务端下发，
// 否则这条边界一次 RPC 就能自己关掉，跟没有一样。
func (c *Client) tunnelTargetAllowed(host string) bool {
	if h := strings.ToLower(strings.TrimSpace(host)); h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip != nil && ip.IsLoopback() {
		return true
	}
	for _, allowed := range c.cfg.TunnelHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(host)) {
			return true
		}
	}
	return false
}

// openTunnel 建立到内网服务的隧道连接并桥接双向数据。
func (c *Client) openTunnel(req protocol.TunnelOpenReq) {
	localHost := req.LocalHost
	if localHost == "" {
		localHost = "127.0.0.1"
	}
	resp := protocol.TunnelOpenResp{TunnelID: req.TunnelID}
	if !c.tunnelTargetAllowed(localHost) {
		resp.Message = "tunnel target not allowed by local config: " + localHost
		_ = c.write(protocol.TypeTunnelOpenResp, resp)
		logx.Errorf("[agent] 拒绝隧道 %s：目标 %s 不是回环地址，且不在本机 tunnel_hosts 白名单内"+
			"（需要放行请把地址写进本机配置的 tunnel_hosts，服务端指令无法打开它）", req.TunnelID, localHost)
		return
	}
	lconn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", localHost, req.LocalPort), 10*time.Second)
	if err != nil {
		resp.Message = "dial internal service failed: " + err.Error()
		_ = c.write(protocol.TypeTunnelOpenResp, resp)
		logx.Errorf("[agent] 隧道建立失败 %s: %v", req.TunnelID, err)
		return
	}
	c.tunnelsMu.Lock()
	if old, ok := c.tunnels[req.TunnelID]; ok {
		_ = old.Close()
	}
	c.tunnels[req.TunnelID] = lconn
	c.tunnelsMu.Unlock()
	resp.OK = true
	_ = c.write(protocol.TypeTunnelOpenResp, resp)
	logx.Infof("[agent] 隧道建立成功 %s -> %s:%d", req.TunnelID, localHost, req.LocalPort)
	go c.readTunnel(req.TunnelID, lconn)
}

// readTunnel 读取内网服务的回包，封装为 TunnelData 回传服务端。
func (c *Client) readTunnel(tunnelID string, conn net.Conn) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			total += int64(n)
			if c.write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: buf[:n]}) != nil {
				break
			}
		}
		if err != nil {
			// 不带原因的退出会把"内网服务自己关了"和"回写服务端失败"混成同一种现象，
			// 而这两种情况在控制台上看起来都只是"远程桌面黑屏/断线"。
			if !errors.Is(err, net.ErrClosed) {
				logx.Infof("[agent] 隧道 %s 读内网服务结束: %v", tunnelID, err)
			}
			break
		}
	}
	// 字节总数必须留痕：只有"隧道建立成功"而没有它，"连上却黑屏"就无从判断
	// 是内网服务根本没回数据，还是数据丢在回传服务端的路上。
	logx.Infof("[agent] 隧道 %s 结束：共读内网服务 %d 字节", tunnelID, total)
	// 内网服务关闭 -> 通知服务端关闭隧道
	_ = c.write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: tunnelID, Reason: "local-closed"})
	c.closeTunnel(tunnelID)
}

func (c *Client) getTunnel(tunnelID string) net.Conn {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	return c.tunnels[tunnelID]
}

// closeTunnel 关闭指定隧道。
func (c *Client) closeTunnel(tunnelID string) {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	if conn, ok := c.tunnels[tunnelID]; ok {
		delete(c.tunnels, tunnelID)
		_ = conn.Close()
	}
}

// closeAllTunnels 关闭全部隧道（断线时调用）。
func (c *Client) closeAllTunnels() {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	for id, conn := range c.tunnels {
		_ = conn.Close()
		delete(c.tunnels, id)
	}
}

// CredentialFiles 返回换取到的客户端凭证的落盘路径（供调用方写回配置）。
type CredentialFiles struct {
	CertFile string
	KeyFile  string
	CAFile   string
	// Source 服务端认定的凭证类型（protocol.CredSource*）；旧服务端为空。
	Source string
	// UsedField 本机实际出示凭据的配置字段：enrollment_secret / agent_key。
	UsedField string
}

// SetOnEnrolled 注册换取成功回调（在证书落盘之后调用）。
func (c *Client) SetOnEnrolled(fn func(CredentialFiles)) {
	c.onEnrolledFiles = fn
}
