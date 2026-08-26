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
type Config struct {
	Server     string `json:"server"`               // 服务端地址 host:port
	AgentID    string `json:"agent_id,omitempty"`   // Agent 唯一标识
	Heartbeat  int    `json:"heartbeat,omitempty"`  // 心跳间隔（秒）
	ProbeEvery int    `json:"probe_every,omitempty"` // 探针采集周期（秒）
	TLS        bool   `json:"tls,omitempty"`        // 启用 TLS
	Insecure   bool   `json:"insecure,omitempty"`   // 跳过证书校验
	Daemon     bool   `json:"daemon,omitempty"`     // 进程守护模式
	ClientCert string `json:"client_cert,omitempty"` // mTLS 客户端证书
	ClientKey  string `json:"client_key,omitempty"`  // mTLS 客户端私钥
	CACert     string `json:"ca_cert,omitempty"`     // 校验服务端证书的 CA
	ProxyIP    string `json:"proxy_ip,omitempty"`    // 国内加速代理前缀
}

func DefaultPath() string { return "mocenter-agent.json" }

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