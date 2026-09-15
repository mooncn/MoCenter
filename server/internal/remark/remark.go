// Package remark 管理主机备注的持久化存储。
//
// 备注保存在服务端数据目录的 remarks.json 中，键为 agent_id，值为备注文本。
// 提供读取、设置、删除能力，线程安全。
package remark

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store 主机备注存储。
type Store struct {
	mu     sync.Mutex
	path   string
	items  map[string]string
}

// NewStore 创建备注存储，dataDir 为服务端数据目录。
func NewStore(dataDir string) *Store {
	return &Store{
		path:  filepath.Join(dataDir, "remarks.json"),
		items: make(map[string]string),
	}
}

// Load 从磁盘加载备注。
func (s *Store) Load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) == nil {
		s.items = m
	}
}

// Get 返回指定主机的备注。
func (s *Store) Get(agentID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[agentID]
}

// Set 设置指定主机的备注并持久化。
func (s *Store) Set(agentID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == "" {
		delete(s.items, agentID)
	} else {
		s.items[agentID] = text
	}
	return s.persistLocked()
}

// persistLocked 将备注写入磁盘（调用方需持有锁）。
func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Rename 把旧 agent_id 的备注迁移到新 agent_id。
//
// 备注挂在 agent_id 上，但设备身份是 UUID；agent 改配置后 agent_id 会变，
// 若不迁移，用户此前写的备注就会"莫名消失"（并且留成一条孤儿数据）。
func (s *Store) Rename(oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.items[oldID]
	if !ok {
		return nil
	}
	if _, exists := s.items[newID]; !exists {
		s.items[newID] = text
	}
	delete(s.items, oldID)
	return s.persistLocked()
}

// All 返回全部备注的副本。
func (s *Store) All() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.items))
	for k, v := range s.items {
		out[k] = v
	}
	return out
}
