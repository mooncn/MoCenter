// Package probe 维护 Agent 的探针数据、历史趋势与离线机器列表。
package probe

import (
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// Store 保存全部 Agent 的探针状态与历史趋势。
type Store struct {
	mu     sync.RWMutex
	agents map[string]*protocol.AgentInfo
	// history 记录每个 Agent 的历史指标采样，key=agentID
	history map[string][]*protocol.ProbeDataReq
}

// NewStore 创建探针仓库。
func NewStore() *Store {
	return &Store{
		agents:  make(map[string]*protocol.AgentInfo),
		history: make(map[string][]*protocol.ProbeDataReq),
	}
}

// UpsertAgent 注册/更新 Agent 基础信息。
func (s *Store) UpsertAgent(info *protocol.AgentInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.agents[info.AgentID]; ok {
		// 保留离线时间
		if !info.Online && old.Online {
			info.LastOfflineAt = time.Now().Unix()
		}
		info.LastOfflineAt = old.LastOfflineAt
	}
	s.agents[info.AgentID] = info
}

// UpdateProbe 更新探针指标并追加历史采样。
func (s *Store) UpdateProbe(p *protocol.ProbeDataReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[p.AgentID]; ok {
		a.CPUUsage = p.CPUUsage
		a.MemUsage = p.MemUsage
		a.DiskUsage = p.DiskUsage
		a.NetInBytes = p.NetInBytes
		a.NetOutBytes = p.NetOutBytes
		a.KernelVersion = p.KernelVersion
		a.OSType = p.OSType
		a.Uptime = p.Uptime
		a.LastSeen = p.Timestamp
	}
	h := s.history[p.AgentID]
	h = append(h, p)
	if len(h) > 1000 { // 保留最近 1000 条采样
		h = h[len(h)-1000:]
	}
	s.history[p.AgentID] = h
}

// MarkOffline 将 Agent 标记为离线。
func (s *Store) MarkOffline(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[agentID]; ok {
		a.Online = false
		a.LastOfflineAt = time.Now().Unix()
	}
}

// Get 返回单个 Agent。
func (s *Store) Get(agentID string) *protocol.AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, _ := s.agents[agentID]
	return a
}

// All 返回全部 Agent（含离线）。
func (s *Store) All() []*protocol.AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*protocol.AgentInfo, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, a)
	}
	return out
}

// History 返回某 Agent 的历史采样（按时间范围过滤）。
func (s *Store) History(agentID string, since time.Time) []*protocol.ProbeDataReq {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.history[agentID]
	var out []*protocol.ProbeDataReq
	for _, p := range h {
		if time.Unix(p.Timestamp, 0).After(since) {
			out = append(out, p)
		}
	}
	return out
}
