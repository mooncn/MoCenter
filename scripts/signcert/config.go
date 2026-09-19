package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// loadJSON 读取配置文件为通用 map；文件不存在或解析失败返回空 map（不阻塞签发）。
func loadJSON(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// saveJSON 以 JSON 写回配置（缩进 2 空格，权限 0600 —— 配置里有 agent_key 等敏感字段）。
func saveJSON(path string, m map[string]any) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

// sha256Hex 返回字节摘要的十六进制表示（用于打印 CA 指纹，便于人工核对）。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
