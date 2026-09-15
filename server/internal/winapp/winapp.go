// Package winapp 提供 Windows 平台相关的运行形态支持：系统服务集成与进程唯一性保护。
//
// 设计取舍：本包在 agent 与 server 两个模块中**各存一份**（二者是独立 module），
// 与本仓库既有的 protocol.go 双份存放惯例一致。
package winapp

import (
	"crypto/sha1"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// RedirectLogToFile 把标准日志重定向到文件（追加写）。
//
// 服务模式下 stdout/stderr 没有去处，不重定向就等于完全没有日志 —— 出问题无从排查。
// 文件超过 8MB 时在启动时截断，避免长期运行把磁盘写满。
func RedirectLogToFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 8<<20 {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	return nil
}

// SingletonKey 由「角色 + 配置路径」生成互斥体名。
//
// 之所以把配置路径纳入：同一台机器上用不同配置跑两个实例是合理需求（例如本机测试），
// 只有"同一份配置"才应互斥。用路径哈希而非原文，避免互斥体名过长/含非法字符。
func SingletonKey(role, configPath string) string {
	abs := configPath
	if p, err := filepath.Abs(configPath); err == nil {
		abs = p
	}
	sum := sha1.Sum([]byte(strings.ToLower(filepath.Clean(abs))))
	return "MoCenter-" + role + "-" + hex.EncodeToString(sum[:4])
}
