// Package config 提供服务端启动参数的配置文件读写。
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

// Config 总控服务端启动配置。
//
// 所有字段**不带 omitempty**：首次生成的配置文件必须完整列出全部参数，
// 让用户一眼看到有哪些可调项（值为空/默认即表示未启用或使用内置默认）。
type Config struct {
	Listen     string `json:"listen"`      // Agent TCP 长连接监听地址，默认 :9000
	HTTP       string `json:"http"`        // REST API / WebSocket 监听地址，默认 :8080
	Whitelist  string `json:"whitelist"`   // IP 白名单（逗号分隔，留空不限制）
	WebDir     string `json:"web_dir"`     // Web 静态资源目录，留空则不托管前端
	TLSCert    string `json:"tls_cert"`    // Agent TLS 证书
	TLSKey     string `json:"tls_key"`     // Agent TLS 私钥
	CACert     string `json:"ca_cert"`     // mTLS 客户端 CA
	Insecure   bool   `json:"insecure"`    // 允许非 TLS 的 Agent 连接
	DataDir    string `json:"data_dir"`    // 数据目录（审计日志、users.json、agent.key、uploads 等）
	User       string `json:"user"`        // 登录用户名
	Password   string `json:"password"`    // 登录密码（仅首次创建账号用，**不落盘**，恒为空）
	TOTPSecret string `json:"totp_secret"` // TOTP 密钥（base32；**不落盘**，恒为空）
	// PkgDir 安装包托管目录（Agent 可经长连接直接拉取）；留空用 <data>/packages。
	PkgDir string `json:"pkg_dir"`
	// AgentKey Agent 注册预共享密钥；留空则自动生成并写到 <data>/agent.key。
	AgentKey string `json:"agent_key"`
	// ProxyIP 国内加速代理前缀（如 http://fa.mooncn.win/?url= ）。
	// 服务端用它访问 ip-api.com 解析节点地理位置；直连不稳定时配置。
	ProxyIP string `json:"proxy_ip"`
	// TokenTTL 会话 Token 有效期（Go duration 字符串，如 12h / 30m）；留空用内置默认 12h。
	TokenTTL string `json:"token_ttl"`
	// WebTLSCert / WebTLSKey Web 控制台 TLS 证书与私钥。
	// 不配置则 :8080 为明文 HTTP —— 登录口令与 Token 将明文上网，
	// 仅在前面有 TLS 反向代理时才可接受。
	WebTLSCert string `json:"web_tls_cert"`
	WebTLSKey  string `json:"web_tls_key"`
	// AllowAnonAgents 允许 Agent 无凭证注册（危险，仅迁移期使用）。
	AllowAnonAgents bool `json:"allow_anon_agents"`
	// MMDBPath 离线 GeoIP 库路径（MaxMind DB 格式，如 GeoLite2-Country.mmdb）。
	//
	// 配置后，节点地区**优先**由本地库解析（无网络、无配额、支持 IPv6），
	// 未命中再降级到外部 HTTP 接口。留空 = 不启用（行为与从前一致）。
	MMDBPath string `json:"mmdb_path"`
	// RequireAgentCert 要求 Agent 必须携带客户端证书。
	// 默认 false：允许 Agent 先用注册密钥在 TLS 连接上换取证书（"简单模式"），
	// 换取成功即必须携带证书，故默认配置下依然收敛到 mTLS。
	RequireAgentCert bool `json:"require_agent_cert"`
	// AdminTLSDays Web 控制台自动 HTTPS 证书有效期（天）；0 关闭自动 HTTPS。
	AdminTLSDays int `json:"admin_tls_days"`
	// ServerName 部署身份名：写入服务端 CA，用于区分多套总控（防止 Agent 连错机器）。
	ServerName string `json:"server_name"`
}

// DefaultPath 返回默认配置文件路径（便携态：与可执行文件同目录）。
//
// 关键：基准是**可执行文件所在目录**，而不是当前工作目录 —— 否则服务、计划任务、
// 双击运行会各自落在不同目录，表现为"配置莫名其妙丢了/重新生成了"。
//
// 安装态由安装器显式传 -config 指向 %ProgramData%\MoCenter\server\（配置与数据均在此，
// 与二进制分离，升级不覆盖）。
func DefaultPath() string {
	return filepath.Join(exeDir(), "mocenter-server.json")
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

// DefaultDataDir 返回默认数据目录（便携态：可执行文件同目录）。
// 安装态由安装器显式传 -data 指向 %ProgramData%\MoCenter\server\。
func DefaultDataDir() string { return exeDir() }

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
// 权限 0600：配置虽不含明文口令，但属于服务运行的核心文件，收紧无副作用。
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
