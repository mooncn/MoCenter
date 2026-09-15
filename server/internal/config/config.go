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
type Config struct {
	Listen     string `json:"listen,omitempty"`      // Agent TCP 长连接监听地址
	HTTP       string `json:"http,omitempty"`        // REST API / WebSocket 监听地址
	APIKey     string `json:"apikey,omitempty"`      // 全局 API Key
	Whitelist  string `json:"whitelist,omitempty"`   // IP 白名单（逗号分隔）
	WebDir     string `json:"web_dir,omitempty"`     // Web 静态资源目录
	TLSCert    string `json:"tls_cert,omitempty"`    // Agent TLS 证书
	TLSKey     string `json:"tls_key,omitempty"`     // Agent TLS 私钥
	CACert     string `json:"ca_cert,omitempty"`     // mTLS 客户端 CA
	Insecure   bool   `json:"insecure,omitempty"`    // 允许非 TLS 的 Agent 连接
	DataDir    string `json:"data_dir,omitempty"`    // 数据目录（审计日志持久化等）
	User       string `json:"user,omitempty"`        // 登录用户名
	Password   string `json:"password,omitempty"`    // 登录密码
	TOTPSecret string `json:"totp_secret,omitempty"` // TOTP 密钥（base32）
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
func Save(path string, c *Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
