// Package protocol 定义 MoCenter 各端之间的消息结构与编解码。
//
// 为避免引入 protoc 工具链，Agent 与服务端、服务端与 Web 之间的消息
// 统一采用 JSON 序列化，消息类型以 Type 字段区分。消息类型与字段命名
// 严格对应项目规划书《九、通信协议规范》中的语义（Heartbeat / Command
// / ProbeData / FileTransfer / InteractiveInput 等），便于后续平滑迁移
// 到 Protobuf。
package protocol

// 消息类型常量
const (
	TypeHeartbeatReq      = "HeartbeatReq"
	TypeHeartbeatResp     = "HeartbeatResp"
	TypeCommandReq        = "CommandReq"
	TypeCommandResp       = "CommandResp"
	TypeProbeDataReq      = "ProbeDataReq"
	TypeFileUploadReq     = "FileUploadReq"
	TypeFileDownloadReq   = "FileDownloadReq"
	TypeFilePushReq       = "FilePushReq"       // Server->Agent 文件分发分片
	TypeFilePushAck       = "FilePushAck"       // Agent->Server 文件分发分片确认
	TypeFileTransferOp    = "FileTransferOp"    // Web->Server 文件传输操作
	TypeFileTransferState = "FileTransferState" // Server->Web 文件传输状态
	TypeInteractiveIn     = "InteractiveInputReq"
	TypeInteractiveOut    = "InteractiveOutput"
	TypeOnline            = "AgentOnline"      // Agent 上线通知（Agent->Server）
	TypeAgentList         = "AgentList"        // Server->Web 主机列表
	TypePortMapOp         = "PortMapOp"        // Web->Server 端口映射操作
	TypePortMapState      = "PortMapState"     // Server->Web 端口映射状态
	TypeUpgradeReq        = "UpgradeReq"       // Server->Agent 远程升级
	TypeUpgradeResp       = "UpgradeResp"      // Agent->Server 升级结果
	TypeTunnelOpen        = "TunnelOpenReq"    // Server->Agent 端口映射隧道建立请求
	TypeTunnelOpenResp    = "TunnelOpenResp"   // Agent->Server 隧道建立响应
	TypeTunnelData        = "TunnelData"       // Server<->Agent 隧道双向数据
	TypeTunnelClose       = "TunnelClose"      // Server<->Agent 隧道关闭
	TypeInteractiveClose  = "InteractiveClose" // Web->Server->Agent 断开交互终端并结束相关进程
	TypeVncInstallReq     = "VncInstallReq"    // Server->Agent 下发 VNC 安装/启动指令
	TypeVncInstallResp    = "VncInstallResp"   // Agent->Server VNC 安装/启动结果
	TypeVncPasswordReq    = "VncPasswordReq"   // Server->Agent 读取本机当前 VNC 口令（只读）
	TypeVncPasswordResp   = "VncPasswordResp"  // Agent->Server 当前 VNC 口令
	TypePkgFetchReq       = "PackageFetchReq"  // Agent->Server 请求拉取服务端托管的安装包
	TypePkgFetchResp      = "PackageFetchResp" // Server->Agent 安装包拉取结果（分片之后回执）
	TypeEnrollReq         = "EnrollReq"        // Agent->Server 注册凭证交换请求（换取客户端证书）
	TypeEnrollResp        = "EnrollResp"       // Server->Agent 注册凭证交换结果
	TypeEnrollRequired    = "EnrollRequired"   // Server->Agent 需重新换取证书（现有证书不被认可）
	// TypeServerAddrUpdate Server->Agent 通知新的服务端地址（监听配置变更前下发）。
	// Agent 落盘成功后回 Ack 并主动重连；旧版 Agent 不认识该消息，会继续用旧地址。
	TypeServerAddrUpdate    = "ServerAddrUpdate"
	TypeServerAddrUpdateAck = "ServerAddrUpdateAck"
)

// Envelope 是所有消息的通用信封：Type 用于路由，Data 为具体负载。
type Envelope struct {
	Type string `json:"type"`
	Data []byte `json:"data"`
}

// HeartbeatReq Agent -> Server 心跳请求
type HeartbeatReq struct {
	AgentID   string `json:"agent_id"`
	Hostname  string `json:"hostname"`
	Timestamp int64  `json:"timestamp"`
	Uptime    int64  `json:"uptime"`
	RTTMs     int64  `json:"rtt_ms"`
}

// HeartbeatResp Server -> Agent 心跳响应
type HeartbeatResp struct {
	Status            int   `json:"status"` // 0=成功 1=异常
	ServerTimestamp   int64 `json:"server_timestamp"`
	HeartbeatInterval int   `json:"heartbeat_interval"` // 动态调整后的心跳间隔(秒)，0 表示保持不变
}

// CommandReq Server -> Agent 远程命令请求
type CommandReq struct {
	CommandID   string            `json:"command_id"`
	AgentID     string            `json:"agent_id"`
	Command     string            `json:"command"`
	ShellType   string            `json:"shell_type"` // bash/sh/cmd/powershell
	Timeout     int               `json:"timeout"`    // 秒，默认 30
	Async       bool              `json:"async"`
	Interactive bool              `json:"interactive"`
	WorkDir     string            `json:"work_dir"`
	Env         map[string]string `json:"env"` // 覆盖/追加环境变量
	Rows        int               `json:"rows"`
	Cols        int               `json:"cols"`
	User        string            `json:"user"` // 执行用户上下文（降权）
}

// CommandResp Agent -> Server 命令响应
type CommandResp struct {
	CommandID string `json:"command_id"`
	Status    int    `json:"status"` // 0=成功 1=失败 2=超时
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Duration  int64  `json:"duration_ms"`
}

// ProbeDataReq Agent -> Server 探针数据
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
	// 扩展指标：系统版本 / CPU 型号与主频 / 内存与磁盘 已用+总量
	OSVersion      string  `json:"os_version,omitempty"`
	CPUModel       string  `json:"cpu_model,omitempty"`
	CPUMHz         float64 `json:"cpu_mhz,omitempty"`
	MemTotalBytes  int64   `json:"mem_total_bytes,omitempty"`
	MemUsedBytes   int64   `json:"mem_used_bytes,omitempty"`
	DiskTotalBytes int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes  int64   `json:"disk_used_bytes,omitempty"`
	// 系统负载（1/5/15 分钟平均）。Unix 来自 /proc/loadavg 或 sysctl；
	// Windows 无此概念（取处理器队列长度近似），LoadKnown=false 表示平台不提供，
	// 前端应显示「—」而不是 0.00。
	// 用指针 + omitempty：0.00 是**合法负载**（空闲机器），不能因 omitempty 被丢弃，
	// 否则公开页会把"负载 0.00"显示成"—"（看起来像不支持该指标）。
	// nil = 平台不提供（Windows 未取到队列长度）或旧版 Agent 未上报。
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`

	// 相邻采样之间的收发字节增量（**仅服务端填写**，Agent 上报报文里没有这两个字段）。
	// 与 NetInBytes（网卡自启动以来的累计值）语义不同：增量用于趋势与公开页流量统计。
	NetInDeltaBytes  int64 `json:"net_in_delta_bytes,omitempty"`
	NetOutDeltaBytes int64 `json:"net_out_delta_bytes,omitempty"`
	// NetDeltaKnown 增量是否可信（首点 / 计数器回绕时为 false，下游应跳过该点）
	NetDeltaKnown bool `json:"net_delta_known,omitempty"`

	// LatencyMS 各延迟目标的往返延迟（毫秒），key 为 "ICMP:1.1.1.1" 形式的目标标识。
	// 缺失的 key 表示"本次未探测或探测失败"——不能当成 0。
	LatencyMS map[string]float64 `json:"latency_ms,omitempty"`
	// LatencyTargets 本机生效的延迟探测目标（服务端据此知道该上报哪些目标）。
	LatencyTargets []string `json:"latency_targets,omitempty"`
}

// AgentInfo Server 保存/向 Web 推送的主机信息
type AgentInfo struct {
	AgentID       string   `json:"agent_id"`
	UUID          string   `json:"uuid,omitempty"` // 硬件唯一标识（如 wmic csproduct get UUID）
	Hostname      string   `json:"hostname"`
	OSType        string   `json:"os_type"`
	Arch          string   `json:"arch"`
	KernelVersion string   `json:"kernel_version"`
	Online        bool     `json:"online"`
	LastSeen      int64    `json:"last_seen"`
	LastOfflineAt int64    `json:"last_offline_at"`
	Uptime        int64    `json:"uptime"`
	CPUUsage      float64  `json:"cpu_usage"`
	MemUsage      float64  `json:"mem_usage"`
	DiskUsage     float64  `json:"disk_usage"`
	NetInBytes    int64    `json:"net_in_bytes"`
	NetOutBytes   int64    `json:"net_out_bytes"`
	RemoteAddr    string   `json:"remote_addr"`
	Tags          []string `json:"tags"`
	PublicIP      string   `json:"public_ip"`
	PublicIPv6    string   `json:"public_ipv6"`
	Remark        string   `json:"remark"`
	Geo           *GeoInfo `json:"geo,omitempty"`
	// 扩展指标：系统版本 / CPU 型号与主频 / 内存与磁盘 已用+总量
	OSVersion      string  `json:"os_version,omitempty"`
	CPUModel       string  `json:"cpu_model,omitempty"`
	CPUMHz         float64 `json:"cpu_mhz,omitempty"`
	MemTotalBytes  int64   `json:"mem_total_bytes,omitempty"`
	MemUsedBytes   int64   `json:"mem_used_bytes,omitempty"`
	DiskTotalBytes int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes  int64   `json:"disk_used_bytes,omitempty"`
	// 网络速率（字节/秒），由服务端根据相邻探针累计值差计算
	NetRateIn  int64 `json:"net_rate_in,omitempty"`
	NetRateOut int64 `json:"net_rate_out,omitempty"`
	// Latency 各探测目标的最新延迟（毫秒），key 为目标标识。
	// 缺失的 key 表示"未探测/失败"，前端显示「超时」而不是 0ms。
	Latency map[string]float64 `json:"latency,omitempty"`
	// LatencyTargets 生效的探测目标列表（用于前端按固定顺序渲染，避免列跳动）。
	LatencyTargets []string `json:"latency_targets,omitempty"`
	// AgentVersion Agent 自报的版本号（如 "1.0.1"）。
	// 服务端用它判断"该主机是否需要升级"，前端在主机详情里展示。
	// 旧版 Agent 不上报此字段，为空串表示"未知版本"（前端显示「—」，
	// 升级判定时按"需升级"处理 —— 未知版本一定不是当前版本）。
	AgentVersion string `json:"agent_version,omitempty"`
	// 系统负载（1/5/15 分钟平均）。用指针：nil = 平台不提供（如 Windows 未取到
	// 队列长度）；非 nil 时 0 是合法值（空闲机器），必须如实下发给前端 ——
	// 用 float64 + omitempty 会把 0 当成"没有数据"丢掉，前端只能显示「—」。
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`

	// 注册鉴权（应用层，见 internal/agentauth）：proof = HMAC-SHA256(key, agentID|nonce|ts)
	// 服务端据此确认对端持有预共享密钥，防止未授权主机注册与身份冒用。
	AuthNonce string `json:"auth_nonce,omitempty"`
	AuthTime  int64  `json:"auth_time,omitempty"`
	AuthProof string `json:"auth_proof,omitempty"`
}

// GeoInfo IP 地理位置信息。
type GeoInfo struct {
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Latitude    float64 `json:"lat"`
	Longitude   float64 `json:"lon"`
}

// PortMapRule 端口映射规则
type PortMapRule struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	PublicPort int    `json:"public_port"`
	LocalPort  int    `json:"local_port"`
	LocalHost  string `json:"local_host"`
	Enabled    bool   `json:"enabled"`
	SourceIP   string `json:"source_ip_whitelist"`
	CreatedAt  int64  `json:"created_at"`
}

// InteractiveInputReq Server -> Agent 交互输入
type InteractiveInputReq struct {
	AgentID   string `json:"agent_id"`
	CommandID string `json:"command_id"`
	InputData string `json:"input_data"`
	Rows      int    `json:"rows"`
	Cols      int    `json:"cols"`
	Control   string `json:"control"` // ctrl_c 等
}

// PortMapOp Web -> Server 端口映射操作
type PortMapOp struct {
	Action     string `json:"action"` // create/delete/enable/disable
	AgentID    string `json:"agent_id"`
	RuleID     string `json:"rule_id"`
	PublicPort int    `json:"public_port"`
	LocalPort  int    `json:"local_port"`
	SourceIP   string `json:"source_ip"`
}

// FileUploadReq Agent -> Server 文件上传分片
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

// FileDownloadReq Server -> Agent 文件下载分片
type FileDownloadReq struct {
	CommandID string `json:"command_id"`
	FilePath  string `json:"file_path"`
	FileSize  int64  `json:"file_size"`
	ChunkSeq  int    `json:"chunk_seq"`
	ChunkData []byte `json:"chunk_data"`
}

// FilePushReq Server -> Agent 文件分发（Web 上传分发到 Agent）分片。
// FileSize<0 表示分发失败通知；Done=true 表示最后一个分片。
type FilePushReq struct {
	CommandID string `json:"command_id"`
	AgentID   string `json:"agent_id"`  // 目标 Agent
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

// InteractiveOutput Agent -> Server -> Web 交互输出
type InteractiveOutput struct {
	CommandID string `json:"command_id"`
	Data      string `json:"data"`
	Type      string `json:"type"` // stdout/stderr/exit
	ExitCode  int    `json:"exit_code"`
}

// ChunkAck 分片确认（ACK/NACK），用于断点续传。
type ChunkAck struct {
	FilePath string `json:"file_path"`
	ChunkSeq int    `json:"chunk_seq"`
	Ack      bool   `json:"ack"` // true=ACK, false=NACK
}

// FileTransferOp Web -> Server 文件传输操作。
type FileTransferOp struct {
	Action    string `json:"action"` // upload/download/list/status
	AgentID   string `json:"agent_id"`
	FilePath  string `json:"file_path"`
	LocalPath string `json:"local_path"` // 服务端本地路径（上传目的地/下载源）
}

// FileTransferState Server -> Web 文件传输状态。
type FileTransferState struct {
	AgentID    string `json:"agent_id"`
	FilePath   string `json:"file_path"`
	Action     string `json:"action"`
	TotalBytes int64  `json:"total_bytes"`
	DoneBytes  int64  `json:"done_bytes"`
	ChunkSeq   int    `json:"chunk_seq"`
	Status     string `json:"status"` // transferring/done/failed
	Message    string `json:"message"`
}

// UpgradeReq Server -> Agent 远程升级请求。
type UpgradeReq struct {
	UpgradeID string `json:"upgrade_id"`
	Version   string `json:"version"`
	URL       string `json:"url"` // 升级包下载地址（必须 https）
	// MD5 已废弃：它不是安全控制（无密钥、可碰撞），保留仅为兼容旧 Agent。
	MD5 string `json:"md5,omitempty"`
	// SHA256 升级包摘要，**必填**。为空则拒绝升级。
	SHA256 string `json:"sha256,omitempty"`
	// Signature Ed25519 签名（hex），对「目标版本号 + 包内容」签名。必填。
	Signature string `json:"signature,omitempty"`
	Force     bool   `json:"force"` // 同版本时强制重装
	// AllowDowngrade 显式允许装回比当前版本更低的包。
	//
	// 默认 false：降级会把机群钉在旧版本上，绕开后续版本里的修复，属于
	// 需要操作员明确表态的动作 —— force 不再顺带放开它。
	AllowDowngrade bool `json:"allow_downgrade,omitempty"`
}

// UpgradeResp Agent -> Server 升级结果。
type UpgradeResp struct {
	UpgradeID string `json:"upgrade_id"`
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Version   string `json:"version"`
}

// TunnelOpenReq Server -> Agent：请求 Agent 在本机（内网）建立到目标服务的隧道连接。
// 公网连接到达服务端后，服务端通过该消息让 Agent 主动 Dial 内网目标，实现反向转发。
type TunnelOpenReq struct {
	TunnelID  string `json:"tunnel_id"` // 服务端公网连接唯一标识
	AgentID   string `json:"agent_id"`
	LocalHost string `json:"local_host"` // 目标地址，默认 127.0.0.1（Agent 本机内网服务）
	LocalPort int    `json:"local_port"`
}

// TunnelOpenResp Agent -> Server：反馈隧道是否建立成功。
type TunnelOpenResp struct {
	TunnelID string `json:"tunnel_id"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// TunnelData Server <-> Agent：隧道载荷，双向转发用户数据。
type TunnelData struct {
	TunnelID string `json:"tunnel_id"`
	Data     []byte `json:"data"`
}

// TunnelClose Server <-> Agent 隧道关闭。
type TunnelClose struct {
	TunnelID string `json:"tunnel_id"`
	Reason   string `json:"reason"`
}

// InteractiveCloseReq Web->Server->Agent：断开交互终端，并结束该会话相关进程。
type InteractiveCloseReq struct {
	AgentID   string `json:"agent_id"`
	CommandID string `json:"command_id"` // 为空表示结束该 Agent 上全部活跃命令/会话
	KillAll   bool   `json:"kill_all"`
}

// VNC 安装模式。
const (
	// VncModeInteractive 交互会话模式：VNC 运行在登录用户的交互会话中。
	VncModeInteractive = "interactive"
	// VncModeService 服务模式：VNC 以系统服务运行，可抓取 Winlogon 安全桌面，
	// 从而在「锁屏 / 未登录」状态下仍能看到画面（Windows 下为 UltraVNC）。
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
	Password string `json:"password,omitempty"` // 安装时生成的随机口令（服务端按需交付给前端）
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
// 与 TypeOnline 使用同一套 HMAC 证明算法（agentauth.Proof）。请求到达时连接尚未进入
// 正常消息循环，服务端在完成校验后立即签发证书并回 EnrollResp。
// 该报文允许在没有客户端证书的 TLS 连接上发送（兼容尚未持有证书的 Agent）。
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
	// Identity 部署身份名（安装时生成，可辨识"这台机器连的是哪套总控"）。
	Identity string `json:"identity,omitempty"`
	// CredSource 本次换取所凭的凭证类型：token（一次性令牌）/ shared-key / anonymous。
	// 供 Agent 在成功后清理"被误填进长期字段的一次性令牌"（见 Agent 侧说明）。
	CredSource string `json:"cred_source,omitempty"`
}

// ServerAddrUpdate Server->Agent：通知服务端监听地址即将变更。
//
// 在服务端新监听绑定成功之后、断开现有连接之前下发，让 Agent 把新地址落盘；
// 随后服务端会主动断开已确认的主机，促使其用新地址重连。旧版 Agent 不认识该类型，会继续用旧地址，
// 因此服务端只在**收到 Ack** 后断开对应连接，避免把未升级的主机直接踢掉。
type ServerAddrUpdate struct {
	// ServerAddr 完整的新地址；为空时只改端口（Port），主机部分沿用 Agent 本机配置。
	// "只改端口"用于服务端监听全部网卡的情况：各 Agent 可能走内网/域名/公网不同路径，
	// 服务端不该替它们猜一个主机名。
	ServerAddr string `json:"server_addr,omitempty"`
	Port       int    `json:"port,omitempty"`
	// ApplyAfterSec 收到后延迟多少秒再断开重连；0 使用默认 2 秒。
	ApplyAfterSec int `json:"apply_after_sec,omitempty"`
}

// ServerAddrUpdateAck Agent->Server：新地址落盘结果。
type ServerAddrUpdateAck struct {
	AgentID    string `json:"agent_id"`
	ServerAddr string `json:"server_addr"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
}
