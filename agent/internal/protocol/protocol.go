// Package protocol Agent 端消息类型定义，与服务端保持一致。
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
	TypeFilePushReq   = "FilePushReq"    // Server -> Agent 文件分发分片
	TypeFilePushAck   = "FilePushAck"    // Agent -> Server 文件分发分片确认
	TypeInteractiveIn  = "InteractiveInputReq"
	TypeInteractiveOut = "InteractiveOutput"
	TypeOnline         = "AgentOnline"
	TypeUpgradeReq     = "UpgradeReq"
	TypeUpgradeResp    = "UpgradeResp"
	TypeTunnelOpen     = "TunnelOpenReq"  // Server->Agent 端口映射隧道建立请求
	TypeTunnelOpenResp = "TunnelOpenResp" // Agent->Server 隧道建立响应
	TypeTunnelData     = "TunnelData"     // Server<->Agent 隧道双向数据
	TypeTunnelClose    = "TunnelClose"    // Server<->Agent 隧道关闭
	TypeInteractiveClose = "InteractiveClose" // Web->Server->Agent 断开交互终端并结束相关进程
	TypeVncInstallReq    = "VncInstallReq"   // Server->Agent 下发 VNC 安装/启动指令
	TypeVncInstallResp   = "VncInstallResp"  // Agent->Server VNC 安装/启动结果
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
}

// ProbeDataResp Server -> Agent
type ProbeDataResp struct {
	ProbeID   string `json:"probe_id"`
	Timestamp int64  `json:"timestamp"`
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
	FilePath  string `json:"file_path"`  // Agent 端目标路径
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
	Ack       bool   `json:"ack"` // true=ACK false=NACK
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
	AgentID  string `json:"agent_id"`
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
	URL       string `json:"url"`
	MD5       string `json:"md5"`
	Force     bool   `json:"force"`
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

// VncInstallReq Server->Agent：下发 VNC 安装/启动指令。
type VncInstallReq struct {
	AgentID string `json:"agent_id"`
	Port    int    `json:"port"` // VNC 端口，默认 5900
}

// VncInstallResp Agent->Server：VNC 安装/启动结果。
type VncInstallResp struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Port    int    `json:"port"`
}
