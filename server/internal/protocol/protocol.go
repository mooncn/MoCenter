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
	TypeHeartbeatReq   = "HeartbeatReq"
	TypeHeartbeatResp  = "HeartbeatResp"
	TypeCommandReq     = "CommandReq"
	TypeCommandResp    = "CommandResp"
	TypeProbeDataReq   = "ProbeDataReq"
	TypeProbeDataResp  = "ProbeDataResp"
	TypeFileUploadReq  = "FileUploadReq"
	TypeFileDownloadReq = "FileDownloadReq"
	TypeInteractiveIn  = "InteractiveInputReq"
	TypeInteractiveOut = "InteractiveOutput"
	TypeOnline         = "AgentOnline"   // Agent 上线通知（Agent->Server）
	TypeAgentList      = "AgentList"     // Server->Web 主机列表
	TypePortMapOp      = "PortMapOp"     // Web->Server 端口映射操作
	TypePortMapState   = "PortMapState"  // Server->Web 端口映射状态
	TypeAuth           = "AuthReq"       // Web->Server 登录/鉴权
	TypeAuthResp       = "AuthResp"
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
	AgentID        string  `json:"agent_id"`
	Timestamp      int64   `json:"timestamp"`
	CPUUsage       float64 `json:"cpu_usage"`
	MemUsage       float64 `json:"mem_usage"`
	DiskUsage      float64 `json:"disk_usage"`
	NetInBytes     int64   `json:"net_in_bytes"`
	NetOutBytes    int64   `json:"net_out_bytes"`
	OSType         string  `json:"os_type"`
	KernelVersion  string  `json:"kernel_version"`
	Uptime         int64   `json:"uptime"`
	Arch           string  `json:"arch"`
}

// ProbeDataResp Server -> Agent 探针确认
type ProbeDataResp struct {
	ProbeID   string `json:"probe_id"`
	Timestamp int64  `json:"timestamp"`
}

// AgentInfo Server 保存/向 Web 推送的主机信息
type AgentInfo struct {
	AgentID        string  `json:"agent_id"`
	Hostname       string  `json:"hostname"`
	OSType         string  `json:"os_type"`
	Arch           string  `json:"arch"`
	KernelVersion  string  `json:"kernel_version"`
	Online         bool    `json:"online"`
	LastSeen       int64   `json:"last_seen"`
	LastOfflineAt  int64   `json:"last_offline_at"`
	Uptime         int64   `json:"uptime"`
	CPUUsage       float64 `json:"cpu_usage"`
	MemUsage       float64 `json:"mem_usage"`
	DiskUsage      float64 `json:"disk_usage"`
	NetInBytes     int64   `json:"net_in_bytes"`
	NetOutBytes    int64   `json:"net_out_bytes"`
	RemoteAddr     string  `json:"remote_addr"`
	Tags           []string `json:"tags"`
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
	AgentID  string `json:"agent_id"`
	CommandID string `json:"command_id"`
	InputData  string `json:"input_data"`
	Rows       int    `json:"rows"`
	Cols       int    `json:"cols"`
	Control    string `json:"control"` // ctrl_c 等
}

// PortMapOp Web -> Server 端口映射操作
type PortMapOp struct {
	Action   string `json:"action"` // create/delete/enable/disable
	AgentID  string `json:"agent_id"`
	RuleID   string `json:"rule_id"`
	PublicPort int  `json:"public_port"`
	LocalPort  int  `json:"local_port"`
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

// InteractiveOutput Agent -> Server -> Web 交互输出
type InteractiveOutput struct {
	CommandID string `json:"command_id"`
	Data      string `json:"data"`
	Type      string `json:"type"` // stdout/stderr/exit
	ExitCode  int    `json:"exit_code"`
}
