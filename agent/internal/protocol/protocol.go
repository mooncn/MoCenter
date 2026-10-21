// Package protocol Agent 端消息类型定义，与服务端保持一致。
package protocol

// 消息类型常量
const (
	TypeHeartbeatReq     = "HeartbeatReq"
	TypeHeartbeatResp    = "HeartbeatResp"
	TypeCommandReq       = "CommandReq"
	TypeCommandResp      = "CommandResp"
	TypeProbeDataReq     = "ProbeDataReq"
	TypeFileUploadReq    = "FileUploadReq"
	TypeFileDownloadReq  = "FileDownloadReq"
	TypeFilePushReq      = "FilePushReq" // Server -> Agent 文件分发分片
	TypeFilePushAck      = "FilePushAck" // Agent -> Server 文件分发分片确认
	TypeInteractiveIn    = "InteractiveInputReq"
	TypeInteractiveOut   = "InteractiveOutput"
	TypeOnline           = "AgentOnline"
	TypeUpgradeReq       = "UpgradeReq"
	TypeUpgradeResp      = "UpgradeResp"
	TypeTunnelOpen       = "TunnelOpenReq"    // Server->Agent 端口映射隧道建立请求
	TypeTunnelOpenResp   = "TunnelOpenResp"   // Agent->Server 隧道建立响应
	TypeTunnelData       = "TunnelData"       // Server<->Agent 隧道双向数据
	TypeTunnelClose      = "TunnelClose"      // Server<->Agent 隧道关闭
	TypeInteractiveClose = "InteractiveClose" // Web->Server->Agent 断开交互终端并结束相关进程
	TypeVncInstallReq    = "VncInstallReq"    // Server->Agent 下发 VNC 安装/启动指令
	TypeVncInstallResp   = "VncInstallResp"   // Agent->Server VNC 安装/启动结果
	TypeVncPasswordReq   = "VncPasswordReq"   // Server->Agent 读取本机当前 VNC 口令（只读）
	TypeVncPasswordResp  = "VncPasswordResp"  // Agent->Server 当前 VNC 口令
	TypePkgFetchReq      = "PackageFetchReq"  // Agent->Server 请求拉取服务端托管的安装包
	TypePkgFetchResp     = "PackageFetchResp" // Server->Agent 安装包拉取结果（分片之后回执）
	TypeEnrollReq        = "EnrollReq"        // Agent->Server 注册凭证交换请求（换取客户端证书）
	TypeEnrollResp       = "EnrollResp"       // Server->Agent 注册凭证交换结果
	TypeEnrollRequired   = "EnrollRequired"   // Server->Agent 需重新换取证书（现有证书不被认可）
	// TypeServerAddrUpdate 服务端通知新的连接地址；落盘成功后回 Ack 并主动重连。
	// 旧版 Agent 忽略未知类型，继续使用本机配置里的旧地址。
	TypeServerAddrUpdate    = "ServerAddrUpdate"
	TypeServerAddrUpdateAck = "ServerAddrUpdateAck"
)

// Envelope 通用消息信封。
type Envelope struct {
	Type string `json:"type"`
	Data []byte `json:"data"`
}

// HeartbeatReq Agent -> Server
type HeartbeatReq struct {
	AgentID   string `json:"agent_id"`
	Hostname  string `json:"hostname"`
	Timestamp int64  `json:"timestamp"`
	Uptime    int64  `json:"uptime"`
	RTTMs     int64  `json:"rtt_ms"`
}

// HeartbeatResp Server -> Agent
type HeartbeatResp struct {
	Status            int   `json:"status"`
	ServerTimestamp   int64 `json:"server_timestamp"`
	HeartbeatInterval int   `json:"heartbeat_interval"`
}

// CommandReq Server -> Agent
type CommandReq struct {
	CommandID   string            `json:"command_id"`
	AgentID     string            `json:"agent_id"`
	Command     string            `json:"command"`
	ShellType   string            `json:"shell_type"`
	Timeout     int               `json:"timeout"`
	Async       bool              `json:"async"`
	Interactive bool              `json:"interactive"`
	WorkDir     string            `json:"work_dir"`
	Env         map[string]string `json:"env"`
	Rows        int               `json:"rows"`
	Cols        int               `json:"cols"`
	User        string            `json:"user"`
}

// CommandResp Agent -> Server
type CommandResp struct {
	CommandID string `json:"command_id"`
	Status    int    `json:"status"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Duration  int64  `json:"duration_ms"`
}

// ProbeDataReq Agent -> Server
type ProbeDataReq struct {
	AgentID       string  `json:"agent_id"`
	UUID          string  `json:"uuid,omitempty"` // 硬件唯一标识
	Timestamp     int64   `json:"timestamp"`
	CPUUsage      float64 `json:"cpu_usage"`
	MemUsage      float64 `json:"mem_usage"`
	DiskUsage     float64 `json:"disk_usage"`
	NetInBytes    int64   `json:"net_in_bytes"`
	NetOutBytes   int64   `json:"net_out_bytes"`
	OSType        string  `json:"os_type"`
	KernelVersion string  `json:"kernel_version"`
	Uptime        int64   `json:"uptime"`
	Arch          string  `json:"arch"`
	PublicIP      string  `json:"public_ip"`
	PublicIPv6    string  `json:"public_ipv6"`
	// 延迟监测（ICMP/TCP/HTTP）：每个目标的往返延迟（毫秒）。
	// 未探测/探测失败的目标不在 map 中 —— "没有数据"与"延迟为 0"必须区分开，
	// 否则前端会把超时画成 0ms（看起来比谁都快）。
	LatencyMS map[string]float64 `json:"latency_ms,omitempty"`
	// LatencyTargets 本机当前生效的延迟探测目标（供服务端回显与前端展示）。
	LatencyTargets []string `json:"latency_targets,omitempty"`
	// 扩展指标：系统版本 / CPU 型号与主频 / 内存与磁盘 已用+总量 / 网络累计
	OSVersion      string  `json:"os_version,omitempty"`
	CPUModel       string  `json:"cpu_model,omitempty"`
	CPUMHz         float64 `json:"cpu_mhz,omitempty"`
	MemTotalBytes  int64   `json:"mem_total_bytes,omitempty"`
	MemUsedBytes   int64   `json:"mem_used_bytes,omitempty"`
	DiskTotalBytes int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes  int64   `json:"disk_used_bytes,omitempty"`
	// 系统负载（1/5/15 分钟平均）。Unix 来自 /proc/loadavg 或 sysctl；
	// Windows 无此概念（取处理器队列长度近似为 1 分钟值）。
	// 指针语义：nil = 本平台不提供（前端显示「—」），非 nil 时 0 是合法值
	//（空闲机器）—— 用 float64 + omitempty 会把 0 当成"没有数据"丢掉。
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`
}

// FileUploadReq Agent -> Server
const (
	DefaultChunkSize = 64 * 1024 // 默认分片 64KB
)

type FileUploadReq struct {
	AgentID   string `json:"agent_id"`
	FilePath  string `json:"file_path"`
	FileSize  int64  `json:"file_size"`
	ChunkSeq  int    `json:"chunk_seq"`
	ChunkData []byte `json:"chunk_data"`
	ChunkMD5  string `json:"chunk_md5"`
}

// FilePushReq Server -> Agent 文件分发（Web 上传分发到 Agent）分片。
// FileSize<0 表示分发失败通知；done=true 表示最后一个分片。
type FilePushReq struct {
	CommandID string `json:"command_id"`
	FilePath  string `json:"file_path"` // Agent 端目标路径
	FileSize  int64  `json:"file_size"`
	ChunkSeq  int    `json:"chunk_seq"`
	ChunkData []byte `json:"chunk_data"`
	ChunkMD5  string `json:"chunk_md5"`
	Done      bool   `json:"done"`
}

// FilePushAck Agent -> Server 文件分发分片确认。
type FilePushAck struct {
	CommandID string `json:"command_id"`
	FilePath  string `json:"file_path"`
	ChunkSeq  int    `json:"chunk_seq"`
	Ack       bool   `json:"ack"`    // true=ACK false=NACK
	Status    string `json:"status"` // transferring/done/failed
	Message   string `json:"message"`
}

// FileDownloadReq Server -> Agent
type FileDownloadReq struct {
	CommandID string `json:"command_id"`
	FilePath  string `json:"file_path"`
	FileSize  int64  `json:"file_size"`
	ChunkSeq  int    `json:"chunk_seq"`
	ChunkData []byte `json:"chunk_data"`
}

// InteractiveInputReq Server -> Agent
type InteractiveInputReq struct {
	AgentID   string `json:"agent_id"`
	CommandID string `json:"command_id"`
	InputData string `json:"input_data"`
	Rows      int    `json:"rows"`
	Cols      int    `json:"cols"`
	Control   string `json:"control"`
}

// InteractiveOutput Agent -> Server
type InteractiveOutput struct {
	CommandID string `json:"command_id"`
	Data      string `json:"data"`
	Type      string `json:"type"`
	ExitCode  int    `json:"exit_code"`
}

// UpgradeReq Server -> Agent
type UpgradeReq struct {
	UpgradeID string `json:"upgrade_id"`
	Version   string `json:"version"`
	URL       string `json:"url"` // 升级包下载地址（必须 https）
	// PackageName 非空表示"服务端 pkg 目录托管包"模式：
	// 经已认证长连接拉取，不再要求 URL/签名，但仍校验 SHA256。
	PackageName string `json:"package_name,omitempty"`
	// MD5 已废弃：不是安全控制（无密钥、可碰撞），仅为兼容旧服务端保留。
	MD5 string `json:"md5,omitempty"`
	// SHA256 升级包摘要，**必填**。为空则拒绝升级。
	SHA256 string `json:"sha256,omitempty"`
	// Signature Ed25519 签名（hex），对「目标版本号 + 包内容」签名。必填。
	Signature string `json:"signature,omitempty"`
	Force     bool   `json:"force"` // 同版本时强制重装
	// AllowDowngrade 显式允许装回比当前版本更低的包（默认拒绝降级）。
	AllowDowngrade bool `json:"allow_downgrade,omitempty"`
}

// UpgradeResp Agent -> Server
type UpgradeResp struct {
	UpgradeID string `json:"upgrade_id"`
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Version   string `json:"version"`
}

// TunnelOpenReq Server -> Agent：请求 Agent 在本机建立到内网服务的隧道连接。
type TunnelOpenReq struct {
	TunnelID  string `json:"tunnel_id"`
	AgentID   string `json:"agent_id"`
	LocalHost string `json:"local_host"` // 默认 127.0.0.1
	LocalPort int    `json:"local_port"`
}

// TunnelOpenResp Agent -> Server
type TunnelOpenResp struct {
	TunnelID string `json:"tunnel_id"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// TunnelData Server <-> Agent 隧道载荷
type TunnelData struct {
	TunnelID string `json:"tunnel_id"`
	Data     []byte `json:"data"`
}

// TunnelClose Server <-> Agent 隧道关闭
type TunnelClose struct {
	TunnelID string `json:"tunnel_id"`
	Reason   string `json:"reason"`
}

// InteractiveCloseReq Web->Server->Agent：断开交互终端，并结束该会话相关进程。
type InteractiveCloseReq struct {
	AgentID   string `json:"agent_id"`
	CommandID string `json:"command_id"` // 为空表示结束当前 Agent 上全部活跃命令/会话
	KillAll   bool   `json:"kill_all"`
}

// VNC 安装模式。
const (
	// VncModeInteractive 交互会话模式：VNC 运行在登录用户的交互会话中。
	// 仅在「用户已登录且未锁屏」时可见桌面；无头场景需配合虚拟显示器。
	VncModeInteractive = "interactive"
	// VncModeService 服务模式：VNC 以系统服务运行，可跟随会话切换抓取
	// Winlogon 安全桌面，从而在「锁屏 / 未登录」状态下仍能看到画面。
	// Windows 下由 UltraVNC 的 uvnc_service 实现。
	VncModeService = "service"
)

// VncInstallReq Server->Agent：下发 VNC 安装/启动指令。
type VncInstallReq struct {
	AgentID string `json:"agent_id"`
	Port    int    `json:"port"` // VNC 端口，默认 5900
	// Mode 安装模式：interactive（默认）/ service（支持锁屏）。
	Mode string `json:"mode,omitempty"`
}

// VncInstallResp Agent->Server：VNC 安装/启动结果。
type VncInstallResp struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	Port     int    `json:"port"`
	Password string `json:"password,omitempty"` // 安装时生成的随机口令（仅服务端按需交付给前端）
	// Mode 实际生效的模式（可能因降级与请求值不同）。
	Mode string `json:"mode,omitempty"`
}

// VncPasswordReq Server->Agent：读取本机当前生效的 VNC 口令。
//
// 与 VncInstallReq 分开成独立消息，而不是给安装请求加个 action 字段：
// 读取凭据是**只读**操作（不改配置、不重启服务、不打断在用的会话），
// 与"安装/重置"的副作用完全不同，分开才好各自留痕与各自设闸。
type VncPasswordReq struct {
	AgentID string `json:"agent_id"`
}

// VncPasswordResp Agent->Server：当前 VNC 口令读取结果。
type VncPasswordResp struct {
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	Password string `json:"password,omitempty"` // 仅在读取成功时有值
	Mode     string `json:"mode,omitempty"`     // 口令所属模式：interactive / service
}

// PackageFetchReq Agent->Server：请求从服务端拉取托管的安装包。
//
// 目的：避免 Agent 从公网下载安装包（网速慢、可能被劫持），改为复用 Agent 与
// 服务端之间既有的长连接通道（可走 TLS/mTLS）。
// 服务端按 Name 在包目录中查找，以 FilePushReq 分片下发到 DestPath。
type PackageFetchReq struct {
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`       // 包文件名，如 UltraVNC_1824_x64_Setup.exe
	CommandID string `json:"command_id"` // 关联本次分片传输
	DestPath  string `json:"dest_path"`  // Agent 端落盘路径（由 Agent 指定）
}

// PackageFetchResp Server->Agent：安装包拉取结果。
//
// 注意：该回执在所有 FilePushReq 分片之后发送。Agent 顺序处理消息，因此收到本回执
// 即代表分片已全部处理完毕，无需再轮询文件大小。
type PackageFetchResp struct {
	CommandID string `json:"command_id"` // 与请求中的 CommandID 对应，用于关联等待方
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Size      int64  `json:"size"`
}

// EnrollReq Agent->Server：注册凭证交换请求（"简单模式"：用注册密钥换取客户端证书）。
//
// 与 AgentOnline 使用同一套 HMAC 证明算法。服务端校验通过后回 EnrollResp，
// 其中携带由服务端内置 CA 签发的客户端证书 —— 运维无需再手工分发客户端证书。
type EnrollReq struct {
	AgentID string `json:"agent_id"`
	Nonce   string `json:"nonce"`
	Proof   string `json:"proof"`
	TS      int64  `json:"ts"`
	// CSR Agent 自己生成的密钥对对应的证书请求（PEM，DER 的 base64 亦可）。
	// 服务端只签发**证书**，私钥永远留在 Agent 本机 —— 服务端不保管、也无法代签。
	CSR string `json:"csr,omitempty"`
}

// EnrollRequiredResp Server->Agent：要求 Agent 重新换取客户端证书。
//
// 用在应用层拒绝连接之前 —— Agent 通道的客户端证书是"请求但不强制"，
// 证书不被认可时**握手本身不会失败**，客户端只会看到"连上立刻断开"。
// 显式下发这条消息，客户端才能把"服务端换过 CA / 证书过期"与"网络故障"区分开，
// 并在获得运维授权时自动重新换取证书（否则只能登录每台被控机手工删 creds/）。
type EnrollRequiredResp struct {
	Reason string `json:"reason,omitempty"`
	Hint   string `json:"hint,omitempty"`
	// Identity / Fingerprint：服务端当前部署身份，供 Agent 比对与运维核对。
	Identity    string `json:"identity,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// EnrollResp Server->Agent：注册凭证交换结果。
type EnrollResp struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	// CertPEM 客户端证书链（PEM）。Agent 落盘为 client.crt，后续连接直接复用。
	CertPEM string `json:"cert_pem,omitempty"`
	// CAPEM 服务端身份 CA 证书（PEM），Agent 可落盘为 ca.crt 便于排查。
	CAPEM string `json:"ca_pem,omitempty"`
	// ServerFingerprint 服务端身份公钥指纹，便于运维核对与写进 Agent 配置。
	ServerFingerprint string `json:"server_fingerprint,omitempty"`
	// Identity 部署身份名。
	Identity string `json:"identity,omitempty"`
	// CredSource 本次换取所凭的凭证类型：token（一次性令牌）/ shared-key / anonymous。
	// Agent 据此判断成功后能否把 agent_key 里同值的一次性令牌一并清掉 ——
	// 令牌用完即废，留在配置里只会在下次重签时变成一条永远失败的凭据。
	CredSource string `json:"cred_source,omitempty"`
}

// 注册凭证类型，取值与服务端 agentauth.Cred* 保持一致。
const (
	CredSourceToken     = "token"
	CredSourceSharedKey = "shared-key"
	CredSourceAnonymous = "anonymous"
)

// ServerAddrUpdate 服务端新监听就绪后通知新地址；Agent 落盘后回 Ack 并重连。
type ServerAddrUpdate struct {
	// ServerAddr 完整的新地址；为空时只改端口（Port），主机部分沿用本机配置。
	ServerAddr string `json:"server_addr,omitempty"`
	Port       int    `json:"port,omitempty"`
	// ApplyAfterSec 收到后延迟多少秒再断开重连；0 使用默认 2 秒。
	ApplyAfterSec int `json:"apply_after_sec,omitempty"`
}

// ServerAddrUpdateAck 新地址落盘结果。
type ServerAddrUpdateAck struct {
	AgentID    string `json:"agent_id"`
	ServerAddr string `json:"server_addr"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
}
