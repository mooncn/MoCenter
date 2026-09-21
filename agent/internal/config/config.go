// Package config 提供 Agent 启动参数的配置文件读写。
//
// 首次启动可通过命令行参数运行，系统会自动把参数写入配置文件；
// 后续启动无需重复填写，直接读取配置文件。显式传入的命令行参数
// 优先级高于配置文件中的值。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config Agent 启动配置。
//
// 所有字段**不带 omitempty**：首次生成的配置文件必须完整列出全部参数，
// 让用户一眼看到有哪些可调项（值为空/默认即表示未启用或使用内置默认）。
type Config struct {
	Server  string `json:"server"`   // 服务端地址 host:port
	AgentID string `json:"agent_id"` // Agent 名称/标识（可变，仅作显示与路由）
	// UUID 硬件唯一标识：**设备身份**，同一台机器恒定不变。
	// 首次启动自动探测并写入配置，之后即使 agent_id 改了也仍被识别为同一台机器。
	UUID       string `json:"uuid"`
	Heartbeat  int    `json:"heartbeat"`   // 心跳间隔（秒），默认 30
	ProbeEvery int    `json:"probe_every"` // 探针采集周期（秒），默认 10
	TLS        bool   `json:"tls"`         // 启用 TLS 加密连接
	Insecure   bool   `json:"insecure"`    // 跳过证书校验（自签名测试用）
	Daemon     bool   `json:"daemon"`      // 进程守护模式（后台运行并崩溃自启）
	ClientCert string `json:"client_cert"` // mTLS 客户端证书
	ClientKey  string `json:"client_key"`  // mTLS 客户端私钥
	CACert     string `json:"ca_cert"`     // 校验服务端证书的 CA
	ProxyIP    string `json:"proxy_ip"`    // 国内加速代理前缀（安装包下载只认 https:// 前缀）
	// AllowPublicPackageDownload 解锁「从公网取安装包」：winget / choco / 厂商直连 / 加速代理。
	// 默认 false —— 安装包（以 SYSTEM 执行的安装器、内核驱动包）只从服务端托管目录经长连接下发。
	// 刻意**不接受服务端下发**：否则等于让服务端一键放弃这条防护；只能改本机配置或命令行。
	AllowPublicPackageDownload bool `json:"allow_public_package_download"`
	// AgentKey 与服务端预共享的注册密钥（应用层鉴权）。
	// 服务端在数据目录生成 agent.key 并打印，Agent 侧填同一个值。
	AgentKey string `json:"agent_key"`
	// UpgradePubKey 升级包 Ed25519 公钥（hex 或文件路径）。
	// 配置后升级必须携带有效签名；未配置时**拒绝升级**。
	// 这里刻意没有"允许无签名升级"的开关：升级是"下载即执行"，能关掉校验就等于没有校验。
	UpgradePubKey string `json:"upgrade_pubkey"`
	// FileRoots 允许服务端下发读写的本地目录（文件分发写入、文件下载读取）。
	// 留空 = 只用默认沙箱 <配置目录>/files。
	//
	// 与 AllowPublicPackageDownload 同类：**只认本机命令行/配置文件**，服务端指令打不开它。
	// 扩大范围是运维决定，不该由被攻击的那条通道代劳。
	FileRoots []string `json:"file_roots"`
	// LatencyTargets 延迟探测目标（ICMP/TCP/HTTP）。
	// 规格形如 "icmp:1.1.1.1" / "tcp:1.1.1.1:443" / "http:https://example.com"，
	// 省略 kind 前缀时按 icmp 处理。留空 = 不探测（不影响任何其它功能）。
	LatencyTargets []string `json:"latency_targets"`
	// ServerFingerprint 服务端身份公钥指纹（16 位 hex，可带分隔符）。
	//
	// 填了它就用"公钥指纹固定"校验服务端，**不需要 CA 文件、也不需要 -insecure**，
	// 且服务端更换 IP/域名/证书都不必更新。留空则退回标准校验（系统根或 -ca-cert）。
	ServerFingerprint string `json:"server_fingerprint"`
	// EnrollmentSecret 首次连接时可用于换取客户端证书的注册密钥。
	// 换取成功后证书与私钥落盘为 <配置目录>/client.crt|client.key（0600），
	// 随后本字段会被**自动清空**（不再需要，减少泄露面）。
	EnrollmentSecret string `json:"enrollment_secret"`
}

// DefaultPath 返回默认配置文件路径（便携态：与可执行文件同目录）。
//
// 关键：基准是**可执行文件所在目录**，而不是当前工作目录 —— 否则服务、计划任务、
// 双击运行会各自落在不同目录，表现为"配置莫名其妙丢了/重新生成了"。
//
// 安装态由安装器显式传 -config 指向 %ProgramData%\MoCenter\agent\（配置与二进制分离，
// 升级不覆盖配置）。
func DefaultPath() string {
	return filepath.Join(exeDir(), "mocenter-agent.json")
}

// exeDir 返回可执行文件所在目录；取不到时退回当前工作目录。
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	// 解析符号链接失败不算错误，直接用原路径
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		p = resolved
	}
	return filepath.Dir(p)
}

// Load 读取配置文件；文件不存在时返回 (nil, nil)。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return &c, nil
}

// Save 将配置写入文件（自动创建目录）。
// 权限 0600：配置可能包含 agent_key（注册密钥），不能放宽。
func Save(path string, c *Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0700)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
