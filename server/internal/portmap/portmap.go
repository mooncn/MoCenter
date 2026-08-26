// Package portmap 实现内网端口到公网服务端的端口映射（反向隧道）。
//
// 整体流程：
//   - 公网连接到达服务端的映射监听端口后，服务端通过已建立的 Agent 长连接
//     下发 TunnelOpenReq，指示 Agent 在本机 Dial 目标内网服务端口；
//   - 隧道建立后，公网侧数据经服务端封装为 TunnelData 转发给 Agent，
//     Agent 写回内网服务；内网服务返回数据经 Agent 封装为 TunnelData 回传，
//     服务端写回公网连接，实现跨公网/内网的双向反向转发。
//
// 连接跟踪表以 TunnelID（公网连接唯一标识，等同规划书中的 PublicConnID）为键，
// 并维护按 AgentID 的二级索引，支持同一公网端口的并发连接。
package portmap

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

// Track 单条连接跟踪记录。
type Track struct {
	TunnelID   string    `json:"tunnel_id"`
	PublicPort int       `json:"public_port"`
	PublicConnID string  `json:"public_conn_id"`
	AgentID    string    `json:"agent_id"`
	AgentConnID string   `json:"agent_conn_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	BytesSent  int64     `json:"bytes_sent"`
	BytesReceived int64  `json:"bytes_received"`
	Status     string    `json:"status"` // Active/Closed/Timeout
}

// Rule 一条端口映射规则。
type Rule struct {
	ID         string `json:"id"`
	PublicPort int    `json:"public_port"`
	AgentID    string `json:"agent_id"`
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
	Enabled    bool   `json:"enabled"`
	SourceIP   string `json:"source_ip_whitelist"`
	CreatedAt  int64  `json:"created_at"`
}

// publicTrack 保存公网侧连接，用于隧道数据回写。
type publicTrack struct {
	pconn      net.Conn
	publicPort int
	agentID    string
	wmu        sync.Mutex // 保护 pconn 写入
}

// Manager 端口映射管理器，负责隧道建立、连接跟踪与双向转发。
type Manager struct {
	mu          sync.RWMutex
	tracks      map[string]*Track            // key: TunnelID
	byAgent     map[string]map[string]*Track // 二级索引：agentID -> TunnelID -> Track
	pub         map[string]*publicTrack      // key: TunnelID -> 公网连接
	rules       map[int]*Rule
	listeners   map[int]net.Listener
	sessions    *session.Manager
	agentForPort func(int) string // 由 Server 注入：根据公网端口找到目标 AgentID
	idleTimeout time.Duration
	seq         uint64
}

// NewManager 创建端口映射管理器。
func NewManager(sess *session.Manager) *Manager {
	return &Manager{
		tracks:      make(map[string]*Track),
		byAgent:     make(map[string]map[string]*Track),
		pub:         make(map[string]*publicTrack),
		rules:       make(map[int]*Rule),
		listeners:   make(map[int]net.Listener),
		sessions:    sess,
		idleTimeout: 300 * time.Second,
	}
}

// SetAgentResolver 设置公网端口 -> AgentID 的解析函数。
func (m *Manager) SetAgentResolver(fn func(int) string) { m.agentForPort = fn }

// TrackConn 记录一条活跃连接跟踪。
func (m *Manager) TrackConn(t *Track) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracks[t.TunnelID] = t
	if m.byAgent[t.AgentID] == nil {
		m.byAgent[t.AgentID] = make(map[string]*Track)
	}
	m.byAgent[t.AgentID][t.TunnelID] = t
}

// UpdateTraffic 更新连接流量统计。
func (m *Manager) UpdateTraffic(tunnelID string, sent, received int64) {
	m.mu.RLock()
	t, ok := m.tracks[tunnelID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	t.BytesSent += sent
	t.BytesReceived += received
	t.LastActiveAt = time.Now()
}

// CloseTrack 关闭并移除连接跟踪记录（并清理公网连接）。
func (m *Manager) CloseTrack(tunnelID string) {
	m.removePublic(tunnelID)
	m.mu.Lock()
	if t, ok := m.tracks[tunnelID]; ok {
		if m.byAgent[t.AgentID] != nil {
			delete(m.byAgent[t.AgentID], tunnelID)
		}
		t.Status = "Closed"
		delete(m.tracks, tunnelID)
	}
	m.mu.Unlock()
}

// CleanupExpired 周期性清理超时连接（分批，单次最多 max 条）。
func (m *Manager) CleanupExpired(max int) []*Track {
	m.mu.Lock()
	var keys []string
	now := time.Now()
	for id, t := range m.tracks {
		if len(keys) >= max {
			break
		}
		if now.Sub(t.LastActiveAt) > m.idleTimeout {
			keys = append(keys, id)
		}
	}
	m.mu.Unlock()

	var expired []*Track
	for _, id := range keys {
		if t := m.GetTrack(id); t != nil {
			// 通知 Agent 关闭隧道
			m.sendTunnelClose(id, t.AgentID, "idle-timeout")
			m.removePublic(id)
			t.Status = "Timeout"
			expired = append(expired, t)
			m.mu.Lock()
			if m.byAgent[t.AgentID] != nil {
				delete(m.byAgent[t.AgentID], id)
			}
			delete(m.tracks, id)
			m.mu.Unlock()
		}
	}
	return expired
}

// GetTrack 返回指定隧道跟踪记录。
func (m *Manager) GetTrack(tunnelID string) *Track {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tracks[tunnelID]
}

// Tracks 返回全部活跃连接跟踪。
func (m *Manager) Tracks() []*Track {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Track, 0, len(m.tracks))
	for _, t := range m.tracks {
		out = append(out, t)
	}
	return out
}

// Rules 返回全部映射规则。
func (m *Manager) Rules() []*Rule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Rule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r)
	}
	return out
}

// GetRule 返回指定公网端口规则。
func (m *Manager) GetRule(publicPort int) *Rule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rules[publicPort]
}

// CreateRule 创建并启动一条端口映射规则。
func (m *Manager) CreateRule(r *Rule) error {
	if r.PublicPort <= 0 || r.LocalPort <= 0 {
		return fmt.Errorf("invalid public/local port")
	}
	if r.LocalHost == "" {
		r.LocalHost = "127.0.0.1"
	}
	r.Enabled = true
	m.mu.Lock()
	m.rules[r.PublicPort] = r
	m.mu.Unlock()
	return m.ServeRule(r)
}

// DeleteRule 删除规则并停止监听。
func (m *Manager) DeleteRule(publicPort int) {
	m.StopRule(publicPort)
	m.mu.Lock()
	delete(m.rules, publicPort)
	m.mu.Unlock()
}

// ServeRule 为一条映射规则启动公网监听端口，将入站连接转发至目标 Agent。
func (m *Manager) ServeRule(r *Rule) error {
	publicPort := r.PublicPort
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", publicPort))
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.listeners[publicPort] = ln
	m.mu.Unlock()

	go func() {
		for {
			clientConn, err := ln.Accept()
			if err != nil {
				return
			}
			go m.handleConnection(clientConn, publicPort, r)
		}
	}()
	return nil
}

// StopRule 停止某公网端口的监听。
func (m *Manager) StopRule(publicPort int) {
	m.mu.Lock()
	if ln, ok := m.listeners[publicPort]; ok {
		_ = ln.Close()
		delete(m.listeners, publicPort)
	}
	m.mu.Unlock()
}

// handleConnection 处理一条公网入站连接：建立隧道并双向转发。
func (m *Manager) handleConnection(clientConn net.Conn, publicPort int, r *Rule) {
	defer clientConn.Close()

	var agent *session.Conn
	if m.sessions != nil {
		agent = m.sessions.Get(r.AgentID)
	}
	if agent == nil {
		log.Printf("[portmap] 目标 Agent %s 离线，拒绝公网端口 %d", r.AgentID, publicPort)
		return
	}

	tunnelID := fmt.Sprintf("t-%d-%d-%d", publicPort, time.Now().UnixNano(), atomic.AddUint64(&m.seq, 1))
	pt := &publicTrack{pconn: clientConn, publicPort: publicPort, agentID: r.AgentID}

	m.mu.Lock()
	m.pub[tunnelID] = pt
	m.mu.Unlock()

	m.TrackConn(&Track{
		TunnelID:    tunnelID,
		PublicConnID: tunnelID,
		PublicPort:  publicPort,
		AgentID:     r.AgentID,
		CreatedAt:   time.Now(),
		LastActiveAt: time.Now(),
		Status:      "Active",
	})

	// 通知 Agent 建立到内网服务的隧道
	open := &protocol.TunnelOpenReq{
		TunnelID:  tunnelID,
		AgentID:   r.AgentID,
		LocalHost: r.LocalHost,
		LocalPort: r.LocalPort,
	}
	if err := agent.Write(protocol.TypeTunnelOpen, open); err != nil {
		m.removePublic(tunnelID)
		m.CloseTrack(tunnelID)
		log.Printf("[portmap] 发送隧道建立请求失败: %v", err)
		return
	}

	// 正向：读公网连接 -> 封装 TunnelData 发给 Agent -> 记录流量
	buf := make([]byte, 32*1024)
	for {
		n, err := clientConn.Read(buf)
		if n > 0 {
			if werr := agent.Write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: buf[:n]}); werr != nil {
				break
			}
			m.UpdateTraffic(tunnelID, int64(n), 0)
		}
		if err != nil {
			break
		}
	}

	// 公网侧断开：关闭并向 Agent 发送关闭通知
	m.removePublic(tunnelID)
	_ = agent.Write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: tunnelID, Reason: "public-closed"})
	m.CloseTrack(tunnelID)
}

// DeliverTunnelData 将 Agent 回传的隧道数据写回公网连接。
func (m *Manager) DeliverTunnelData(tunnelID string, data []byte) {
	m.mu.RLock()
	pt, ok := m.pub[tunnelID]
	m.mu.RUnlock()
	if !ok || pt == nil {
		return
	}
	pt.wmu.Lock()
	defer pt.wmu.Unlock()
	if _, err := pt.pconn.Write(data); err != nil {
		// 公网连接已断开
		m.removePublic(tunnelID)
		m.CloseTrack(tunnelID)
	}
	m.UpdateTraffic(tunnelID, 0, int64(len(data)))
}

// NotifyTunnelClosed 处理 Agent 本地隧道关闭的通知。
func (m *Manager) NotifyTunnelClosed(tunnelID string, _ string) {
	m.removePublic(tunnelID)
	m.CloseTrack(tunnelID)
}

// sendTunnelClose 通过 Agent 长连接下发隧道关闭通知。
func (m *Manager) sendTunnelClose(tunnelID, agentID, reason string) {
	if m.sessions == nil {
		return
	}
	if agent := m.sessions.Get(agentID); agent != nil {
		_ = agent.Write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: tunnelID, Reason: reason})
	}
}

// removePublic 移除并关闭公网连接（幂等）。
func (m *Manager) removePublic(tunnelID string) {
	m.mu.Lock()
	if pt, ok := m.pub[tunnelID]; ok {
		delete(m.pub, tunnelID)
		_ = pt.pconn.Close()
	}
	m.mu.Unlock()
}