// Package session 维护在线 Agent 的 TCP 长连接与会话池。
package session

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// Conn 代表一个 Agent 的长连接会话。
type Conn struct {
	mu         sync.Mutex
	AgentID    string
	Hostname   string
	NetConn    net.Conn
	Online     bool
	LastSeen   time.Time
	RemoteAddr string
	enc        *json.Encoder
	// HeartbeatInterval 该 Agent 当前被服务端动态下发的心跳间隔（秒）。
	// 用于在 Tick 中按每个 Agent 的实际心跳频率判定超时，
	// 避免弱网下心跳间隔被拉长后仍按固定阈值误判离线。
	HeartbeatInterval int
}

// SetHeartbeatInterval 记录服务端下发给该 Agent 的动态心跳间隔（秒）。
func (c *Conn) SetHeartbeatInterval(sec int) {
	if sec <= 0 {
		return
	}
	c.mu.Lock()
	c.HeartbeatInterval = sec
	c.mu.Unlock()
}

// heartbeatTimeout 返回该连接基于动态心跳间隔的超时阈值：
// 仅当服务端已下发超过默认心跳（30s）的间隔时才启用动态阈值，
// 否则返回 0（表示由调用方按 offlineAfter 判定）。
func (c *Conn) heartbeatTimeout() time.Duration {
	c.mu.Lock()
	iv := c.HeartbeatInterval
	c.mu.Unlock()
	if iv <= 30 {
		// 默认或更短心跳：不启用动态阈值，交由 offlineAfter 兜底
		return 0
	}
	return time.Duration(iv) * 3 * time.Second
}

// NewConn 创建基于给定网络连接的长连接会话。
func NewConn(nc net.Conn) *Conn {
	return &Conn{NetConn: nc, Online: true, LastSeen: time.Now(), enc: json.NewEncoder(nc)}
}

// Write 线程安全地向该连接写一条消息。
func (c *Conn) Write(typ string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(protocol.Envelope{Type: typ, Data: data})
}

// Manager 维护全部 Agent 会话。
type Manager struct {
	mu    sync.RWMutex
	conns map[string]*Conn
}

// NewManager 创建会话管理器。
func NewManager() *Manager {
	return &Manager{conns: make(map[string]*Conn)}
}

// Upsert 注册或更新 Agent 连接。
func (m *Manager) Upsert(agentID string, c *Conn) (*Conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.conns[agentID]
	if exists && old != nil && old.NetConn != nil {
		_ = old.NetConn.Close()
	}
	m.conns[agentID] = c
	return old, exists
}

// Get 获取指定 Agent 连接。
func (m *Manager) Get(agentID string) *Conn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[agentID]
}

// Remove 移除 Agent 连接。
func (m *Manager) Remove(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.conns, agentID)
}

// All 返回全部连接。
func (m *Manager) All() []*Conn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Conn, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c)
	}
	return out
}

// SendTo 向指定 Agent 发送消息。
func (m *Manager) SendTo(agentID, typ string, payload interface{}) error {
	c := m.Get(agentID)
	if c == nil {
		return fmt.Errorf("agent %s offline", agentID)
	}
	return c.Write(typ, payload)
}

// Broadcast 向所有 Agent 广播消息。
func (m *Manager) Broadcast(typ string, payload interface{}) {
	for _, c := range m.All() {
		if err := c.Write(typ, payload); err != nil {
			log.Printf("[session] broadcast to %s error: %v", c.AgentID, err)
		}
	}
}

// Tick 扫描心跳超时（超过 3 个心跳周期视为离线），返回离线的 AgentID 列表。
// 每个 Agent 按其动态心跳间隔计算超时阈值（offlineAfter 作为下限兜底）。
func (m *Manager) Tick(offlineAfter time.Duration) []string {
	m.mu.RLock()
	var offline []string
	now := time.Now()
	for id, c := range m.conns {
		if !c.Online {
			continue
		}
		threshold := c.heartbeatTimeout()
		if threshold < offlineAfter {
			threshold = offlineAfter
		}
		if now.Sub(c.LastSeen) > threshold {
			offline = append(offline, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range offline {
		m.Remove(id)
	}
	return offline
}
