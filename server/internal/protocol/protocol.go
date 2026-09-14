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
	TypeProbeDataResp     = "ProbeDataResp"
	TypeFileUploadReq     = "FileUploadReq"
	TypeFileDownloadReq   = "FileDownloadReq"
	TypeFilePushReq       = "FilePushReq"       // Server->Agent 文件分发分片
	TypeFilePushAck       = "FilePushAck"       // Agent->Server 文件分发分片确认
	TypeFileAck           = "FileAck"           // Server<->Agent 分片确认
	TypeFileTransferOp    = "FileTransferOp"    // Web->Server 文件传输操作
	TypeFileTransferState = "FileTransferState" // Server->Web 文件传输状态
	TypeInteractiveIn     = "InteractiveInputReq"
	TypeInteractiveOut    = "InteractiveOutput"
	TypeOnline            = "AgentOnline"  // Agent 上线通知（Agent->Server）
	TypeAgentList         = "AgentList"    // Server->Web 主机列表
	TypePortMapOp         = "PortMapOp"    // Web->Server 端口映射操作
	TypePortMapState      = "PortMapState" // Server->Web 端口映射状态
	TypeAuth              = "AuthReq"      // Web->Server 登录/鉴权
	TypeAuthResp          = "AuthResp"
	TypeUpgradeReq        = "UpgradeReq"       // Server->Agent 远程升级
	TypeUpgradeResp       = "UpgradeResp"      // Agent->Server 升级结果
	TypeBatchCommand      = "BatchCommand"     // Server->Agent 批量命令（预留）
	TypeTunnelOpen        = "TunnelOpenReq"    // Server->Agent 端口映射隧道建立请求
	TypeTunnelOpenResp    = "TunnelOpenResp"   // Agent->Server 隧道建立响应
	TypeTunnelData        = "TunnelData"       // Server<->Agent 隧道双向数据
	TypeTunnelClose       = "TunnelClose"      // Server<->Agent 隧道关闭
	TypeInteractiveClose  = "InteractiveClose" // Web->Server->Agent 断开交互终端并结束相关进程
	TypeVncInstallReq     = "VncInstallReq"    // Server->Agent 下发 VNC 安装/启动指令
	TypeVncInstallResp    = "VncInstallResp"   // Agent->Server VNC 安装/启动结果
	TypePkgFetchReq       = "PackageFetchReq"  // Agent->Server 请求拉取服务端托管的安装包
	TypePkgFetchResp      = "PackageFetchResp" // Server->Agent 安装包拉取结果（分片之后回执）
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
}

// ProbeDataResp Server -> Agent 探针确认
type ProbeDataResp struct {
	ProbeID   string `json:"probe_id"`
	Timestamp int64  `json:"timestamp"`
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
	URL       string `json:"url"`   // 升级包下载地址
	MD5       string `json:"md5"`   // 升级包 MD5 校验和
	Force     bool   `json:"force"` // 是否强制升级
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
