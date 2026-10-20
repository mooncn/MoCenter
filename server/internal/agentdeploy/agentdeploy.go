// Package agentdeploy 实现「后台一键部署 Agent」。
//
// 需求原文：
//
//	考虑在后台新增一键部署agent功能，用户可输入账号/密码（如linux直接ssh登录后）
//	直接安装服务，并完成agent端配置写入，windows的话如果没有类似实现方案则略过就行
//
// 因此本包走 **SSH + 密码/私钥认证** 这条路：Linux 主机只要 sshd 可达就能装，
// 无需预先在目标机上放任何东西。Windows 侧未实现（OpenSSH for Windows 虽然存在，
// 但凭证、服务注册与权限模型差异大，按需求"略过"）。
//
// 部署流程（每步都会回传进度，前端可见）：
//
//  1. 连接并认证（密码 / 私钥 / 私钥+passphrase）
//  2. 探测目标机：架构（uname -m）、是否有 root 权限（id -u）
//  3. 把 Agent 二进制上传到 /tmp（**从服务端本地的 packages 目录读取**，
//     不依赖目标机能访问公网 —— 内网机器往往没有外网）
//  4. 写入 Agent 配置文件（服务端地址、AgentID、注册密钥、代理、延迟目标…）
//  5. 安装为 systemd 服务并启动
//  6. 回读状态确认（systemctl is-active + 版本号）
//
// 安全约束（这块必须写清楚，因为它是"从服务端主动连别人机器"的能力）：
//
//   - **凭证不落盘、不入日志**。密码/私钥只存在于单次请求内存中，
//     审计日志里只记"谁在什么时候部署了哪个地址"。
//   - **命令注入防护**：所有拼进 shell 的变量都经 shellQuote 处理；
//     AgentID 等还要过 validateAgentID 的白名单校验
//     —— 否则一个叫 `x; rm -rf /` 的 AgentID 就是一次远程代码执行。
//   - **上传体积上限**：避免被当作"往目标机写大文件"的通道。
//   - **主机密钥必须校验**（known_hosts TOFU，见 hostKeyCallback）：本通道是
//     服务端主动连别人机器，握手时不校验主机密钥就等于把注册密钥和操作员的
//     口令/私钥交给路径上的中间人。
//   - 仅管理员可用（REST 层 guard 已覆盖）。
package agentdeploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// maxUploadSize 单个上传文件的大小上限（64 MB，够放各架构 Agent 二进制）。
//
// uploadFile 会先把整个文件读进内存再经 SSH 写入，所以这个值同时是
// 「一次部署占多少内存」的上限；200 MB 在并发部署多台机器时会直接放大成 GB 级峰值。
const maxUploadSize = 64 << 20

// knownHostsFileName 部署通道的主机密钥记账文件（位于数据目录下）。
//
// 单独一个文件，而不是复用运维自己的 ~/.ssh/known_hosts：
// 部署用的是表单临时填的账号（常常是 root），把它写进个人配置会污染对方环境，
// 反过来个人文件里的通配/注释写法也可能让这个包的解析器读不懂。
const knownHostsFileName = "ssh_known_hosts"

// stepTimeout 单步命令的执行超时。
const stepTimeout = 60 * time.Second

// Options 部署器的环境依赖（由 server 注入）。
type Options struct {
	// DataDir 服务端数据目录（用于定位 packages/ 与 agent.key）。
	DataDir string
	// PkgDir 安装包托管目录（Agent 二进制从这里取）。
	PkgDir string
	// ServerAddr 服务端监听地址（写入 Agent 配置）。
	// 形如 ":9000" 或 "0.0.0.0:9000" 时，会尝试用本次请求的 Host 推导可路由地址。
	ServerAddr string
	// AgentKeyFunc 实时取当前共享注册密钥（写入目标机的 Agent 配置）。
	//
	// 必须是"取"而不是启动时"抄一份"：密钥现在能在运行期轮换（M-1），
	// 快照会让部署一路报成功、而目标机拿着旧密钥永远注册不上。
	AgentKeyFunc func() string
	// ProxyIP 可选的国内加速代理前缀。
	ProxyIP string
	// UpgradePubKey 升级包 Ed25519 公钥，写进目标机的 Agent 配置。
	//
	// 留空也要照写：Agent 侧没有"跳过验签"的开关，配置里缺这个键的表现是
	// "装好的主机永远升级不动"，而日志只有一句"未配置 -upgrade-pubkey"。
	// 空值让运维在生成的配置里直接看见该填什么。
	UpgradePubKey string
	// TLS / mTLS 相关：目标 Agent 要连上启用了 TLS 的服务端就必须带上这些文件。
	TLSCert string
	TLSKey  string
	CACert  string
	// MMDBPath 仅用于"提示"（离线 GeoIP 是服务端能力，与 Agent 无关）。
	MMDBPath string
}

// Request 一次部署请求（密码等敏感字段只在内存中）。
type Request struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// Password 密码认证（与 PrivateKey 二选一）
	Password string `json:"password"`
	// PrivateKey PEM 私钥内容（与 Password 二选一）
	PrivateKey string `json:"private_key"`
	// Passphrase 私钥口令（私钥加密时提供）
	Passphrase string `json:"passphrase"`
	// AgentID 目标机上的 Agent 名称（留空用主机名）。
	// 必须通过白名单校验（只允许 [A-Za-z0-9._-]，最长 64）。
	AgentID string `json:"agent_id"`
	// LatencyTargets 该机要探测的延迟目标（可选，写入 Agent 配置）。
	LatencyTargets []string `json:"latency_targets"`
	// ServerAddr 覆盖服务端地址（留空自动推导）。
	ServerAddr string `json:"server_addr"`
	// Binary 指定要上传的二进制文件名（packages 目录内的文件名）。
	// 留空则自动按目标架构挑选 mocenter-agent-* 中匹配的一项。
	Binary string `json:"binary"`
	// InstallDir 安装目录（默认 /usr/local/bin）。
	InstallDir string `json:"install_dir"`
	// ServiceName systemd 服务名（默认 mocenter-agent）。
	ServiceName string `json:"service_name"`
	// ProbeOnly 仅做连通性与环境探测，不上传、不安装。
	ProbeOnly bool `json:"probe_only"`
	// HostKeyFingerprint 目标机主机密钥指纹（SHA256:… 或只写 base64 部分）。
	//
	// 填了就是**强制钉扎**：握手时指纹不符直接中止，不再走"首次接触信任"。
	// 留空则按 TOFU 处理 —— 第一次连接记录密钥并放行，之后变更一律硬失败。
	HostKeyFingerprint string `json:"host_key_fingerprint"`
}

// StepResult 单步执行结果（用于前端逐步展示）。
type StepResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// Result 部署结果。
type Result struct {
	OK      bool         `json:"ok"`
	Host    string       `json:"host"`
	Steps   []StepResult `json:"steps"`
	Message string       `json:"message"`
	// Target 探测到的目标机信息（架构/权限/发行版），便于运维核对
	Target *TargetInfo `json:"target,omitempty"`
	// HostKey 本次连接实际采用的目标机主机密钥指纹（SHA256:…）。
	// 公开指纹不是秘密：它是给运维**核对**用的，也是审计里能留下的唯一凭据线索。
	HostKey string `json:"host_key,omitempty"`
	// HostKeyNewTrust 表示该密钥是本次首次接触时记入 known_hosts 的。
	// 运维应当把它当成"请核对指纹"的提示，而不是"一切正常"。
	HostKeyNewTrust bool `json:"host_key_new_trust,omitempty"`
}

// TargetInfo 目标机探测结果。
type TargetInfo struct {
	Arch       string `json:"arch"`        // uname -m
	OSName     string `json:"os_name"`     // /etc/os-release 的 PRETTY_NAME
	IsRoot     bool   `json:"is_root"`     // id -u == 0
	SudoOK     bool   `json:"sudo_ok"`     // 非 root 时 sudo -n 是否可用
	HasSystemd bool   `json:"has_systemd"` // 是否存在 systemctl
	Hostname   string `json:"hostname"`
	// GOARCH 由 Arch 推导（amd64 / arm64 / arm / 386）
	GOARCH string `json:"goarch"`
}

// Deployer 部署器。
type Deployer struct {
	opts  Options
	audit func(operator, ip, summary string)
	mu    sync.Mutex
}

// New 创建部署器。
func New(opts Options) *Deployer { return &Deployer{opts: opts} }

// SetAudit 注入审计回调（operator, ip, summary）。
func (d *Deployer) SetAudit(fn func(operator, ip, summary string)) { d.audit = fn }

// auditf 记录一条审计（不含任何凭证）。
func (d *Deployer) auditf(operator, ip, format string, args ...interface{}) {
	if d == nil || d.audit == nil {
		return
	}
	d.audit(operator, ip, fmt.Sprintf(format, args...))
}

// agentIDRe AgentID 白名单：只允许字母数字与 . _ -，最长 64。
//
// 这个校验是**安全边界**，不是格式偏好：AgentID 会被写进 shell 脚本与
// systemd 单元，`x; rm -rf /` 这样的值就是一次远程代码执行。
var agentIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidAgentID 校验 AgentID 合法性。
func ValidAgentID(s string) bool { return agentIDRe.MatchString(s) }

// sshHostRe 允许的主机标识（IPv4 / IPv6 / 域名）。
var sshHostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)

// fingerprintRe 部署表单里允许的主机密钥指纹写法：可省 "SHA256:" 前缀。
//
// 这里做格式校验而不只是 trim：这个值会出现在错误信息与审计摘要里，
// 放任任意字符串进来就等于开了一个"往审计里写任意内容"的口子。
var fingerprintRe = regexp.MustCompile(`^(?:SHA256:)?[A-Za-z0-9+/=]{20,88}$`)

// normalizeFingerprint 把用户填写的指纹规整为 ssh.FingerprintSHA256 的写法。
func normalizeFingerprint(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "\"'"))
	if s == "" || strings.HasPrefix(s, "SHA256:") {
		return s
	}
	return "SHA256:" + s
}

// hostKeyTrust 一次握手中最终采用的主机密钥信息（用于回显与审计）。
type hostKeyTrust struct {
	// Fingerprint 实际收到的密钥指纹。
	Fingerprint string
	// NewlyTrusted 首次接触并已记入 known_hosts。
	NewlyTrusted bool
}

// knownHostsPath 返回主机密钥记账文件路径，必要时创建数据目录。
func (d *Deployer) knownHostsPath() (string, error) {
	dir := strings.TrimSpace(d.opts.DataDir)
	if dir == "" {
		return "", errors.New("服务端未配置数据目录，无法记录 SSH 主机密钥；请在部署表单里填写目标机的主机密钥指纹")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	return filepath.Join(dir, knownHostsFileName), nil
}

// hostKeyCallback 构造 known_hosts 校验回调（TOFU：首次接触记账，之后变更硬失败）。
//
// 这里的默认取向是 **fail-closed**：任何"读不了记录"的错误都中止部署，而不是退回
// 忽略主机密钥。退回去不会被任何人察觉 —— 连接照样成功，只是运维的口令、私钥和
// 注册密钥已经送给了中间人。
func (d *Deployer) hostKeyCallback(addr, pin string, trust *hostKeyTrust) (ssh.HostKeyCallback, error) {
	path, err := d.knownHostsPath()
	if err != nil {
		return nil, err
	}
	// 先确保文件存在（0600）：knownhosts.New 打不开文件是报错而不是当空表处理，
	// 而"这台服务端还没部署过任何机器"显然不该是失败理由。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建主机密钥记录 %s 失败: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("写入主机密钥记录 %s 失败: %w", path, err)
	}
	known, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("读取主机密钥记录 %s 失败: %w", path, err)
	}
	pin = normalizeFingerprint(pin)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		trust.Fingerprint = fp
		if pin != "" && fp != pin {
			return fmt.Errorf("目标机 %s 的主机密钥为 %s，与部署时填写的 %s 不一致，已中止。"+
				"若该机确实换过密钥（重装/重新生成），请核对无误后用新指纹重试", addr, fp, pin)
		}
		checkAddr := hostname
		if checkAddr == "" {
			checkAddr = addr
		}
		err := known(checkAddr, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return fmt.Errorf("主机密钥校验失败: %w", err)
		}
		if len(ke.Want) > 0 {
			// 记过账但密钥对不上 —— 这正是 known_hosts 存在的理由，硬失败。
			return fmt.Errorf("目标机 %s 的主机密钥与已记录的不一致：记录为 %s，实际为 %s。"+
				"可能存在中间人，或该机确实重装过；确认无误后删除 %s 中的对应行再部署",
				addr, ssh.FingerprintSHA256(ke.Want[0].Key), fp, path)
		}
		// 首次接触：记账后放行。
		if err := d.recordHostKey(path, checkAddr, key); err != nil {
			return err
		}
		trust.NewlyTrusted = true
		return nil
	}, nil
}

// recordHostKey 追加一条主机密钥记录。
//
// 用追加而不是"读出来改完再整体写回"：并发部署时整体写回会丢掉别人刚记的行。
func (d *Deployer) recordHostKey(path, addr string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("打开主机密钥记录 %s 失败: %w", path, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, line+"\n"); err != nil {
		return fmt.Errorf("写入主机密钥记录 %s 失败: %w", path, err)
	}
	return nil
}

// trustSummary 生成一句给前端/审计看的话（只含指纹，不含任何凭证）。
func trustSummary(t *hostKeyTrust) string {
	if t == nil || t.Fingerprint == "" {
		return ""
	}
	if t.NewlyTrusted {
		return "主机密钥 " + t.Fingerprint + "（首次接触，已记入 known_hosts，请自行核对）"
	}
	return "主机密钥 " + t.Fingerprint + "（与已知记录一致）"
}

// hostKeyForAudit 审计用的一小段（握手都没走到时记 "-"，便于区分"没连上"和"密钥被拒"）。
func hostKeyForAudit(t *hostKeyTrust) string {
	if t == nil || t.Fingerprint == "" {
		return "-"
	}
	if t.NewlyTrusted {
		return t.Fingerprint + "(首次信任)"
	}
	return t.Fingerprint
}

// Deploy 执行一次部署。
//
// operator / operatorIP 仅用于审计。凭证不会被记录。
func (d *Deployer) Deploy(req *Request, operator, operatorIP string) (*Result, error) {
	if req == nil {
		return nil, errors.New("请求为空")
	}
	host := strings.TrimSpace(req.Host)
	if !sshHostRe.MatchString(host) {
		return nil, fmt.Errorf("主机地址不合法: %q", host)
	}
	if strings.TrimSpace(req.User) == "" {
		return nil, errors.New("请填写登录用户名")
	}
	if req.Password == "" && strings.TrimSpace(req.PrivateKey) == "" {
		return nil, errors.New("请提供密码或私钥")
	}
	if req.AgentID != "" && !ValidAgentID(req.AgentID) {
		return nil, errors.New("Agent 名称只允许字母、数字与 . _ -（最长 64 字符）")
	}
	if pin := normalizeFingerprint(req.HostKeyFingerprint); pin != "" && !fingerprintRe.MatchString(pin) {
		return nil, errors.New("主机密钥指纹格式不正确（形如 SHA256:xxxxxxxx…，也允许省略 SHA256: 前缀）")
	}
	if req.ProbeOnly && req.Password == "" && req.PrivateKey == "" {
		return nil, errors.New("探测也需要提供认证方式")
	}

	port := req.Port
	if port <= 0 {
		port = 22
	}
	if port > 65535 {
		return nil, errors.New("端口不合法")
	}

	res := &Result{OK: false, Host: net.JoinHostPort(host, fmt.Sprint(port))}
	addStep := func(s StepResult) {
		res.Steps = append(res.Steps, s)
	}
	step := func(name string, fn func() (string, error)) error {
		start := time.Now()
		out, err := fn()
		sr := StepResult{Name: name, OK: err == nil, Output: out, Elapsed: time.Since(start).Round(time.Millisecond).String()}
		if err != nil {
			sr.Error = err.Error()
			addStep(sr)
			return err
		}
		addStep(sr)
		return nil
	}

	// --- 1. 连接认证 ---
	client, trust, err := d.dial(req, host, port)
	res.HostKey = trust.Fingerprint
	res.HostKeyNewTrust = trust.NewlyTrusted
	if err != nil {
		// 认证失败的具体原因可能是"密码错"或"网络不通"，两者对运维意义不同，
		// 因此把原始错误保留在步骤里（不含凭证内容）。
		addStep(StepResult{Name: "SSH 连接与认证", OK: false, Error: err.Error()})
		res.Message = "SSH 连接失败"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（SSH 连接）: %s@%s:%d 主机密钥=%s",
			req.User, host, port, hostKeyForAudit(trust))
		return res, nil
	}
	defer client.Close()
	connectOut := "已连接"
	if s := trustSummary(trust); s != "" {
		connectOut += " / " + s
	}
	addStep(StepResult{Name: "SSH 连接与认证", OK: true, Output: connectOut})

	// 部署过程中若 SSH 连接中断（网络抖动、目标机被重启），后续每一步都会失败，
	// 这里给出整体提示，避免运维只看到"最后一步失败"而无从判断。
	defer func() {
		if !res.OK {
			res.Message = strings.TrimSpace(res.Message)
			if res.Message == "" {
				res.Message = "部署未完成，请查看各步骤输出"
			}
		}
	}()

	// --- 2. 环境探测 ---
	var info *TargetInfo
	if err := step("探测目标机环境", func() (string, error) {
		ti, out, err := probeTarget(client)
		if err != nil {
			return out, err
		}
		info = ti
		res.Target = ti
		return out, nil
	}); err != nil {
		res.Message = "目标机环境探测失败"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（环境探测）: %s@%s:%d", req.User, host, port)
		return res, nil
	}

	if !info.IsRoot && !info.SudoOK {
		addStep(StepResult{Name: "权限检查", OK: false,
			Error: "需要 root 权限（或免密 sudo）才能安装为系统服务"})
		res.Message = "目标机缺少 root 权限"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（权限不足）: %s@%s:%d", req.User, host, port)
		return res, nil
	}
	if !info.HasSystemd {
		// 无 systemd（容器、init 为别的实现）时退化为"后台进程 + 简单守护"，
		// 这里明确告知，而不是静默装成一个不会开机自启的进程。
		addStep(StepResult{Name: "服务管理", OK: true,
			Output: "目标机没有 systemd，将使用进程守护方式启动（无开机自启）"})
	}

	if req.ProbeOnly {
		res.OK = true
		res.Message = "探测完成（未安装）"
		addStep(StepResult{Name: "探测模式", OK: true, Output: "仅探测，未做任何改动"})
		d.auditf(operator, operatorIP, "一键部署 Agent 环境探测: %s@%s:%d (%s/%s) 主机密钥=%s",
			req.User, host, port, info.OSName, info.Arch, hostKeyForAudit(trust))
		return res, nil
	}

	// --- 3. 定位并上传二进制 ---
	binary, err := d.pickBinary(req.Binary, info.GOARCH)
	if err != nil {
		addStep(StepResult{Name: "准备 Agent 安装包", OK: false, Error: err.Error()})
		res.Message = "未找到匹配的 Agent 二进制"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（缺少安装包）: %s@%s:%d", req.User, host, port)
		return res, nil
	}
	remoteBin := "/tmp/mocenter-agent." + fmt.Sprint(time.Now().Unix())
	if err := step("上传 Agent 二进制（"+filepath.Base(binary)+"）", func() (string, error) {
		src, oerr := os.Open(binary)
		if oerr != nil {
			return "", oerr
		}
		defer src.Close()
		fi, serr := src.Stat()
		if serr != nil {
			return "", serr
		}
		if fi.Size() > maxUploadSize {
			return "", fmt.Errorf("安装包过大（%d 字节，上限 %d）", fi.Size(), int64(maxUploadSize))
		}
		if uerr := uploadFile(client, src, fi.Size(), remoteBin, info.IsRoot); uerr != nil {
			return "", uerr
		}
		// 校验上传完整性：SFTP 中断会留下半个二进制，装上去就是"服务反复重启"。
		return fmt.Sprintf("已上传 %s（%s）", filepath.Base(binary), humanSize(fi.Size())), nil
	}); err != nil {
		res.Message = "上传 Agent 二进制失败"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（上传）: %s@%s:%d", req.User, host, port)
		return res, nil
	}

	// --- 4. 生成并写入配置 ---
	serverAddr := strings.TrimSpace(req.ServerAddr)
	if serverAddr == "" {
		serverAddr = d.opts.ServerAddr
	}
	conf := d.buildAgentConfig(req, serverAddr)
	remoteConf := "/tmp/mocenter-agent-config-" + fmt.Sprint(time.Now().Unix()) + ".json"
	if err := step("写入 Agent 配置", func() (string, error) {
		if err := uploadBytes(client, []byte(conf), remoteConf, info.IsRoot); err != nil {
			return "", err
		}
		return "配置已准备（含服务端地址、Agent 名称、注册密钥、延迟目标）", nil
	}); err != nil {
		res.Message = "写入 Agent 配置失败"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（写配置）: %s@%s:%d", req.User, host, port)
		return res, nil
	}

	// --- 5. 安装为服务并启动 ---
	installDir := strings.TrimSpace(req.InstallDir)
	if installDir == "" {
		installDir = "/usr/local/bin"
	}
	if !regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`).MatchString(installDir) {
		addStep(StepResult{Name: "安装目录校验", OK: false, Error: "安装目录格式不合法: " + installDir})
		res.Message = "安装目录不合法"
		return res, nil
	}
	svcName := strings.TrimSpace(req.ServiceName)
	if svcName == "" {
		svcName = "mocenter-agent"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(svcName) {
		addStep(StepResult{Name: "服务名校验", OK: false, Error: "服务名格式不合法: " + svcName})
		res.Message = "服务名不合法"
		return res, nil
	}

	script := buildInstallScript(installScriptArgs{
		RemoteBin:  remoteBin,
		RemoteConf: remoteConf,
		InstallDir: installDir,
		Service:    svcName,
		HasSystemd: info.HasSystemd,
		AgentID:    req.AgentID,
		IsRoot:     info.IsRoot,
	})
	if err := step("安装为系统服务并启动", func() (string, error) {
		out, err := runScript(client, script, info.IsRoot)
		if err != nil {
			return out, err
		}
		return strings.TrimSpace(out), nil
	}); err != nil {
		res.Message = "安装服务失败"
		d.auditf(operator, operatorIP, "一键部署 Agent 失败（安装服务）: %s@%s:%d", req.User, host, port)
		return res, nil
	}

	// --- 6. 回读状态确认 ---
	if err := step("确认服务状态", func() (string, error) {
		out, err := runCmd(client, info.IsRoot, statusProbeCmd(svcName, info.HasSystemd))
		if err != nil {
			return out, err
		}
		return strings.TrimSpace(out), nil
	}); err != nil {
		res.Message = "服务已安装但状态确认失败（请登录目标机查看日志）"
		d.auditf(operator, operatorIP, "一键部署 Agent 部分成功（状态确认失败）: %s@%s:%d", req.User, host, port)
		return res, nil
	}

	res.OK = true
	res.Message = "Agent 已安装并启动"
	d.auditf(operator, operatorIP, "一键部署 Agent 成功: %s@%s:%d agent_id=%s arch=%s 主机密钥=%s",
		req.User, host, port, req.AgentID, info.GOARCH, hostKeyForAudit(trust))
	return res, nil
}

// dial 建立 SSH 连接。
//
// 返回的 hostKeyTrust 在失败时也可能有值（握手阶段已收到密钥），调用方可以据此
// 在审计里留下"哪台机、哪个密钥被拒了"这条线索。
func (d *Deployer) dial(req *Request, host string, port int) (*ssh.Client, *hostKeyTrust, error) {
	trust := &hostKeyTrust{}
	var auths []ssh.AuthMethod
	if req.Password != "" {
		auths = append(auths, ssh.Password(req.Password))
		// 键盘交互兜底：部分 sshd 配置（ChallengeResponseAuthentication）
		// 会把密码认证走成 keyboard-interactive，只给 Password 方法会失败。
		auths = append(auths, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range questions {
				answers[i] = req.Password
			}
			return answers, nil
		}))
	}
	if strings.TrimSpace(req.PrivateKey) != "" {
		var signer ssh.Signer
		var err error
		key := strings.TrimSpace(req.PrivateKey)
		if req.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(req.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key))
		}
		if err != nil {
			return nil, trust, fmt.Errorf("解析私钥失败: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	// 主机密钥校验是**强制**的：没有"忽略校验"的开关可退。
	// 加一个 -deploy-ssh-insecure 之类的逃生口，等于把这里刚修掉的洞原样留下，
	// 而且它一旦被某次部署用上，就不会有人再关回去。
	cb, err := d.hostKeyCallback(addr, req.HostKeyFingerprint, trust)
	if err != nil {
		return nil, trust, err
	}
	cfg := &ssh.ClientConfig{
		User:            req.User,
		Auth:            auths,
		HostKeyCallback: cb,
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, trust, err
	}
	return client, trust, nil
}

// probeTarget 探测目标机的架构与权限。
func probeTarget(client *ssh.Client) (*TargetInfo, string, error) {
	// 一次性把所有需要的值打出来，减少往返（每往返都有网络延迟）。
	const probe = `echo "ARCH=$(uname -m)"; ` +
		`echo "UID=$(id -u)"; ` +
		`echo "HOST=$(hostname 2>/dev/null)"; ` +
		`echo "OS=$( . /etc/os-release 2>/dev/null && echo $PRETTY_NAME )"; ` +
		// 只判断 systemctl 是否存在还不够：容器里往往有 systemctl 但 PID 1 不是
		// systemd，此时 systemctl 会直接报 "System has not been booted with systemd"。
		// 因此额外检查 /run/systemd/system（systemd 运行时目录）。
		`echo "SYSTEMD=$( [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 && echo yes || echo no )"; ` +
		`echo "SUDO=$(sudo -n true >/dev/null 2>&1 && echo yes || echo no)"`
	out, err := runCmd(client, true, probe)
	if err != nil {
		return nil, out, fmt.Errorf("执行探测命令失败: %w（输出: %s）", err, strings.TrimSpace(out))
	}
	vals := parseKeyVals(out)
	ti := &TargetInfo{
		Arch:       strings.TrimSpace(vals["ARCH"]),
		OSName:     trimQuotes(strings.TrimSpace(vals["OS"])),
		Hostname:   strings.TrimSpace(vals["HOST"]),
		IsRoot:     strings.TrimSpace(vals["UID"]) == "0",
		HasSystemd: strings.TrimSpace(vals["SYSTEMD"]) == "yes",
		SudoOK:     strings.TrimSpace(vals["SUDO"]) == "yes",
	}
	ti.GOARCH, err = mapArch(ti.Arch)
	if err != nil {
		return ti, strings.TrimSpace(out), err
	}
	return ti, fmt.Sprintf("架构 %s（%s） / 系统 %s / root=%v / systemd=%v",
		ti.Arch, ti.GOARCH, firstNonEmpty(ti.OSName, "未知"), ti.IsRoot, ti.HasSystemd), nil
}

// mapArch 把 uname -m 映射到 GOARCH。
func mapArch(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "armv7l", "armv6l", "arm":
		return "arm", nil
	case "i386", "i686", "386":
		return "386", nil
	default:
		return "", fmt.Errorf("不支持的目标架构: %q（可用 amd64/arm64/arm/386）", m)
	}
}

// pickBinary 从 packages 目录中挑选匹配目标架构的 Agent 二进制。
func (d *Deployer) pickBinary(explicit, goarch string) (string, error) {
	dir := strings.TrimSpace(d.opts.PkgDir)
	if dir == "" {
		dir = filepath.Join(d.opts.DataDir, "packages")
	}
	if explicit != "" {
		// 文件名白名单：防止 ../ 之类的路径穿越读到任意文件
		if filepath.Base(explicit) != explicit || strings.Contains(explicit, "..") {
			return "", fmt.Errorf("安装包名称不合法: %q", explicit)
		}
		p := filepath.Join(dir, explicit)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("安装包不存在: %s", explicit)
		}
		return p, nil
	}
	// 自动挑选：优先名字里带目标架构的，其次不带架构后缀的通用名
	candidates, err := listBinaries(dir)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("安装包目录 %s 中没有可用的 Agent 二进制。"+
			"请先用 scripts/build-linux-all.sh --type=agent --package=none 构建，或把 mocenter-agent-<arch> 放进该目录", dir)
	}
	// 精确匹配架构
	for _, c := range candidates {
		lower := strings.ToLower(filepath.Base(c))
		if strings.Contains(lower, goarch) {
			// amd64 也可能写成 x86_64，优先精确的 amd64
			if goarch == "amd64" && strings.Contains(lower, "x86_64") {
				continue
			}
			return c, nil
		}
	}
	for _, c := range candidates {
		lower := strings.ToLower(filepath.Base(c))
		if goarch == "amd64" && strings.Contains(lower, "x86_64") {
			return c, nil
		}
	}
	return "", fmt.Errorf("安装包目录中没有匹配 %s 架构的 Agent 二进制（现有: %s）",
		goarch, strings.Join(basenames(candidates), ", "))
}

// listBinaries 列出 packages 目录中候选的 agent 二进制。
func listBinaries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取安装包目录失败 %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasPrefix(name, "mocenter-agent") {
			continue
		}
		if strings.HasSuffix(name, ".deb") || strings.HasSuffix(name, ".rpm") ||
			strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".sha256") ||
			strings.HasSuffix(name, ".sha1") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func basenames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

// agentKey 取当前共享注册密钥。
// 返回空串表示这条路径当前不可用（已关闭或未配置）—— 写空串比写一把旧密钥更诚实。
func (d *Deployer) agentKey() string {
	if d.opts.AgentKeyFunc == nil {
		return ""
	}
	return d.opts.AgentKeyFunc()
}

// buildAgentConfig 生成 Agent 的 JSON 配置。
//
// 字段名必须与 agent/internal/config.Config 的 json tag 完全一致，
// 否则 Agent 读到的是一份"看着有内容、实际全零值"的配置 ——
// 表现就是"装完了但连不上服务端"。
func (d *Deployer) buildAgentConfig(req *Request, serverAddr string) string {
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  %q: %q,\n", "server", serverAddr)
	fmt.Fprintf(&b, "  %q: %q,\n", "agent_id", req.AgentID)
	fmt.Fprintf(&b, "  %q: %q,\n", "uuid", "")
	// 采集与心跳用默认值（10s / 30s），显式写出便于运维调整
	fmt.Fprintf(&b, "  %q: %d,\n", "heartbeat", 30)
	fmt.Fprintf(&b, "  %q: %d,\n", "probe_every", 10)
	fmt.Fprintf(&b, "  %q: %v,\n", "tls", false)
	fmt.Fprintf(&b, "  %q: %v,\n", "insecure", false)
	fmt.Fprintf(&b, "  %q: %v,\n", "daemon", false)
	fmt.Fprintf(&b, "  %q: %q,\n", "client_cert", "")
	fmt.Fprintf(&b, "  %q: %q,\n", "client_key", "")
	fmt.Fprintf(&b, "  %q: %q,\n", "ca_cert", "")
	fmt.Fprintf(&b, "  %q: %q,\n", "proxy_ip", d.opts.ProxyIP)
	// 注册密钥：没有它 Agent 会被服务端拒绝注册（表现为"装好了却一直不上线"）
	fmt.Fprintf(&b, "  %q: %q,\n", "agent_key", d.agentKey())
	fmt.Fprintf(&b, "  %q: %q,\n", "upgrade_pubkey", strings.TrimSpace(d.opts.UpgradePubKey))
	// 文件读写根目录：留空即 Agent 用默认沙箱 <配置目录>/files（M-9）。
	// 这里刻意不给服务端下发扩大范围的能力 —— 那等于让被攻击的通道自己放开自己。
	fmt.Fprintf(&b, "  %q: %s,\n", "file_roots", jsonStringArray(nil))
	// 延迟目标：把服务端配置的目标直接写进 Agent 配置，装完立刻开始探测
	fmt.Fprintf(&b, "  %q: %s\n", "latency_targets", jsonStringArray(req.LatencyTargets))
	b.WriteString("}\n")
	return b.String()
}

// jsonStringArray 手写 JSON 字符串数组（避免为一处格式化引入额外抽象）。
func jsonStringArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%q", it))
	}
	if len(parts) == 0 {
		return "[]"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// installScriptArgs 安装脚本参数。
type installScriptArgs struct {
	RemoteBin  string
	RemoteConf string
	InstallDir string
	Service    string
	HasSystemd bool
	AgentID    string
	// IsRoot 当前 SSH 用户是否为 root。
	//
	// 决定脚本里用不用 sudo：**已经是 root 时绝不能用 sudo** ——
	// 很多精简镜像/容器里根本没装 sudo，会直接以 127（command not found）失败；
	// 而部署者明明是 root，报错却指向 sudo，排查起来非常绕。
	IsRoot bool
}

// buildInstallScript 生成安装脚本。
//
// 所有变量都经 shellQuote 处理：即便上游校验有疏漏，也不会因为
// 一个引号/分号就变成命令注入（纵深防御，不依赖单点校验）。
func buildInstallScript(a installScriptArgs) string {
	bin := shellQuote(a.RemoteBin)
	conf := shellQuote(a.RemoteConf)
	dir := shellQuote(a.InstallDir)
	svc := shellQuote(a.Service)
	// 非 root 时靠 `sudo -n`（探测阶段已确认免密可用）；是 root 就直接执行。
	sup := "sudo -n"
	if a.IsRoot {
		sup = ""
	}
	var sb strings.Builder
	sb.WriteString("set -e\n")
	fmt.Fprintf(&sb, "SUDO=%q\n", sup)
	fmt.Fprintf(&sb, "BIN=%s\n", bin)
	fmt.Fprintf(&sb, "CONF=%s\n", conf)
	fmt.Fprintf(&sb, "DIR=%s\n", dir)
	fmt.Fprintf(&sb, "SVC=%s\n", svc)
	// 二进制始终替换（部署/升级都要换），配置**只在首次部署时写入**。
	//
	// 为什么不再覆盖配置：覆盖会丢掉本机定制（file_roots / proxy_ip / latency_targets
	// 等）以及 Agent 启动后写回的设备身份，与 install-linux.sh 的既定升级语义
	//（"已安装则只换二进制与服务，配置原样保留"）也不一致。
	// 注意：这也意味着**无法**再用重新部署的方式给存量 Agent 补 upgrade_pubkey ——
	// 那种情况需要手工改配置，或先删除配置再部署一次。
	sb.WriteString(`$SUDO mkdir -p "$DIR" /etc/mocenter
if command -v install >/dev/null 2>&1; then
  $SUDO install -m 0755 "$BIN" "$DIR/mocenter-agent"
else
  # 部分最小化系统没有 install 命令，退回 cp + chmod
  $SUDO cp -f "$BIN" "$DIR/mocenter-agent"
  $SUDO chmod 0755 "$DIR/mocenter-agent"
fi
CONF_DST=/etc/mocenter/mocenter-agent.json
if [ -f "$CONF_DST" ]; then
  echo "[config] 已保留既有配置（升级模式，未覆盖 $CONF_DST）"
else
  if command -v install >/dev/null 2>&1; then
    $SUDO install -m 0600 "$CONF" "$CONF_DST"
  else
    $SUDO cp -f "$CONF" "$CONF_DST"
    $SUDO chmod 0600 "$CONF_DST"
  fi
  echo "[config] 首次部署，已写入 $CONF_DST"
fi
`)
	if a.HasSystemd {
		sb.WriteString(`$SUDO tee "/etc/systemd/system/$SVC.service" >/dev/null <<'UNIT'
[Unit]
Description=MoCenter Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=__AGENT_BIN__ -config /etc/mocenter/mocenter-agent.json -daemon
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT
`)
		// 单元里的二进制路径用绝对路径替换（避免 heredoc 引号带来的变量展开问题）
		sb.WriteString(`$SUDO sed -i "s#__AGENT_BIN__#$DIR/mocenter-agent#" "/etc/systemd/system/$SVC.service"
$SUDO systemctl daemon-reload
$SUDO systemctl enable "$SVC" >/dev/null 2>&1 || true
$SUDO systemctl restart "$SVC"
`)
	} else {
		// 无 systemd：用 nohup 起后台进程（不提供开机自启，已在步骤里明确告知）。
		// 注意 set -e 下 pkill 没匹配到会返回 1，必须 `|| true`。
		sb.WriteString(`$SUDO pkill -f "$DIR/mocenter-agent" >/dev/null 2>&1 || true
if [ -n "$SUDO" ]; then
  $SUDO sh -c "nohup $DIR/mocenter-agent -config /etc/mocenter/mocenter-agent.json -daemon >/var/log/mocenter-agent.log 2>&1 &"
else
  nohup "$DIR/mocenter-agent" -config /etc/mocenter/mocenter-agent.json -daemon >/var/log/mocenter-agent.log 2>&1 &
fi
sleep 2
`)
	}
	// 清理临时文件（二进制与配置都含密钥，不能留在 /tmp）
	sb.WriteString(`$SUDO rm -f "$BIN" "$CONF"
echo "安装完成"
`)
	return sb.String()
}

// statusProbeCmd 生成状态确认命令。
func statusProbeCmd(svc string, hasSystemd bool) string {
	if hasSystemd {
		return fmt.Sprintf(`echo "active=$(systemctl is-active %s 2>/dev/null || true)"; `+
			`echo "enabled=$(systemctl is-enabled %s 2>/dev/null || true)"; `+
			`echo "version=$(/usr/local/bin/mocenter-agent -version 2>/dev/null || true)"`,
			shellQuote(svc), shellQuote(svc))
	}
	return `echo "active=$(pgrep -f mocenter-agent >/dev/null && echo active || echo inactive)"; ` +
		`echo "enabled=manual"`
}

// runCmd 执行一条命令并返回合并输出。
//
// 命令通过 **stdin** 送给 `sh -s`，而不是拼成 `sh -c "<命令>"`。
// 原因：命令里普遍含有 $() 与双引号，拼进命令行要经历"本层 shell 引号 ->
// SSH 远端 shell 引号"两次转义，任一层的差异（不同 shell 对 \$ 的处理）都会
// 静默吃掉变量展开 —— 实测就出现过 `ARCH=$(uname -m)` 整行消失、只剩其它变量的
// 情况，表现为"探测失败"但输出看着大部分正常。走 stdin 后完全绕开这个问题。
func runCmd(client *ssh.Client, root bool, cmd string) (string, error) {
	// 所有命令都用 sh 执行：目标机的默认 shell 可能是 csh/fish，
	// 而我们的脚本语法是 POSIX sh。
	shellCmd := cmd
	if !root {
		// 非 root 用户用 sudo 提权（探测阶段已确认 sudo -n 可用）
		shellCmd = "sudo -n sh -s"
	} else {
		shellCmd = "sh -s"
	}
	return runStdin(client, shellCmd, cmd)
}

// runStdin 以 stdin 送脚本的方式执行远端命令。
func runStdin(client *ssh.Client, remoteCmd, script string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &out
	sess.Stdin = strings.NewReader(script)
	done := make(chan error, 1)
	go func() { done <- sess.Run(remoteCmd) }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(stepTimeout):
		_ = sess.Signal(ssh.SIGKILL)
		return out.String(), fmt.Errorf("命令执行超时（%s）", stepTimeout)
	}
}

// runScript 执行一段多行脚本（同样走 stdin，避免引号与长度问题）。
func runScript(client *ssh.Client, script string, root bool) (string, error) {
	cmd := "sh -s"
	if !root {
		cmd = "sudo -n sh -s"
	}
	return runStdin(client, cmd, script)
}

// uploadFile 用 `cat > file` 的方式上传（不依赖 sftp-server 子系统）。
//
// 为什么不走 SFTP：很多精简发行版/容器里的 sshd 未启用 sftp 子系统，
// 而 `cat` 是任何 shell 都有的。代价是要自己做 shell 层的提权与权限设置。
//
// 以流式方式喂给 SSH，而不是先 io.ReadAll 进内存：Agent 二进制约 7–15 MB，
// 并发部署多台时「每台一份完整副本」会直接放大成几十上百 MB 的峰值。
func uploadFile(client *ssh.Client, src io.Reader, size int64, remotePath string, root bool) error {
	if size > maxUploadSize {
		return fmt.Errorf("文件过大（超过 %d MB）", maxUploadSize>>20)
	}
	return uploadReader(client, io.LimitReader(src, maxUploadSize+1), remotePath, root)
}

func uploadBytes(client *ssh.Client, data []byte, remotePath string, root bool) error {
	return uploadReader(client, bytes.NewReader(data), remotePath, root)
}

func uploadReader(client *ssh.Client, data io.Reader, remotePath string, root bool) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	sess.Stdin = data
	// umask 0077：二进制与配置含密钥，落地时就要收紧权限，
	// 而不是先放开再 chmod（中间窗口期可被同机其他用户读走）。
	pre := "umask 0077; "
	if root {
		pre += "cat > " + shellQuote(remotePath)
	} else {
		pre += "sudo -n sh -c " + shellQuote("cat > "+remotePath)
	}
	if err := sess.Run(pre); err != nil {
		return fmt.Errorf("写入远端文件失败: %w（%s）", err, strings.TrimSpace(stderr.String()))
	}
	_ = remotePath
	return nil
}

// parseKeyVals 解析 "KEY=value" 行。
func parseKeyVals(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		m[strings.TrimSpace(line[:i])] = line[i+1:]
	}
	return m
}

// shellQuote 用单引号包裹并转义内部单引号（POSIX sh 通用写法）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// trimQuotes 去掉值两端可能出现的引号。
//
// 为什么要去掉：不同 shell/发行版下 `echo "$PRETTY_NAME"` 的输出可能带引号
// （PRETTY_NAME 本身就含引号，或 shell 把引号也打了出来）。带引号显示在
// 前端只是观感问题，但它会连带影响"系统名"的比对判断，统一在这里收口。
func trimQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// humanSize 人类可读的体积。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// PackageInfo 一个可下发的安装包。
type PackageInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Arch 由文件名推导（mocenter-agent-arm64 -> arm64）
	Arch string `json:"arch"`
}

// PackageCandidates 列出可下发的 Agent 安装包（供前端选择）。
func (d *Deployer) PackageCandidates() []PackageInfo {
	dir := strings.TrimSpace(d.opts.PkgDir)
	if dir == "" {
		dir = filepath.Join(d.opts.DataDir, "packages")
	}
	paths, err := listBinaries(dir)
	if err != nil {
		return []PackageInfo{}
	}
	out := make([]PackageInfo, 0, len(paths))
	for _, p := range paths {
		name := filepath.Base(p)
		var size int64
		if fi, err := os.Stat(p); err == nil {
			size = fi.Size()
		}
		out = append(out, PackageInfo{Name: name, Size: size, Arch: archFromName(name)})
	}
	return out
}

// archFromName 从文件名推导架构（mocenter-agent-arm64.exe -> arm64）。
func archFromName(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSuffix(lower, ".exe")
	for _, a := range []string{"amd64", "x86_64", "arm64", "aarch64", "arm", "386", "i386"} {
		if strings.HasSuffix(lower, "-"+a) {
			switch a {
			case "x86_64":
				return "amd64"
			case "aarch64":
				return "arm64"
			case "i386":
				return "386"
			}
			return a
		}
	}
	return ""
}
