// Package portmap 实现内网端口到公网服务端的端口映射（反向代理）。
//
// 每个映射规则绑定服务端一个公网端口，通过连接跟踪表维护
// 公网端口到内网 Agent 的映射关系，实现多路复用。采用 Go 原生
// net/http + TCP 转发，不依赖外部 Nginx。
package portmap

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"mocenter/server/internal/session"
)

// Track 单条连接跟踪记录。
type Track struct {
	PublicPort    int       `json:"public_port"`
	PublicConnID  string    `json:"public_conn_id"`
	AgentID       string    `json:"agent_id"`
	AgentConnID   string    `json:"agent_conn_id"`
	CreatedAt     time.Time `json:"created_at"`
	LastActiveAt  time.Time `json:"last_active_at"`
	BytesSent     int64     `json:"bytes_sent"`
	BytesReceived int64     `json:"bytes_received"`
	Status        string    `json:"status"` // Active/Closed/Timeout
}

// Manager 端口映射管理器，负责连接跟踪与转发。
type Manager struct {
	mu          sync.RWMutex
	tracks      map[string]*Track            // key: public port(string)
	byAgent     map[string]map[string]*Track // 二级索引：agentID -> publicPort -> track
	listeners   map[int]net.Listener
	sessions    *session.Manager
	agentForPort func(int) string // 由 Server 注入：根据公网端口找到目标 AgentID
	idleTimeout time.Duration
}

// NewManager 创建端口映射管理器。
func NewManager(sess *session.Manager) *Manager {
	m := &Manager{
		tracks:    make(map[string]*Track),
		byAgent:   make(map[string]map[string]*Track),
		listeners: make(map[int]net.Listener),
		sessions:  sess,
		idleTimeout: 300 * time.Second,
	}
	return m
}

// SetAgentResolver 设置公网端口 -> AgentID 的解析函数。
func (m *Manager) SetAgentResolver(fn func(int) string) { m.agentForPort = fn }

// TrackConn 记录一条活跃连接跟踪。
func (m *Manager) TrackConn(t *Track) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracks[fmt.Sprint(t.PublicPort)] = t
	if m.byAgent[t.AgentID] == nil {
		m.byAgent[t.AgentID] = make(map[string]*Track)
	}
	m.byAgent[t.AgentID][fmt.Sprint(t.PublicPort)] = t
}

// UpdateTraffic 更新连接流量统计（读锁复制快照后更新）。
func (m *Manager) UpdateTraffic(publicPort int, sent, received int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tracks[fmt.Sprint(publicPort)]; ok {
		t.BytesSent += sent
		t.BytesReceived += received
		t.LastActiveAt = time.Now()
	}
}

// CloseTrack 关闭并移除连接跟踪记录。
func (m *Manager) CloseTrack(publicPort int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprint(publicPort)
	if t, ok := m.tracks[key]; ok {
		if m.byAgent[t.AgentID] != nil {
			delete(m.byAgent[t.AgentID], key)
		}
		t.Status = "Closed"
		delete(m.tracks, key)
	}
}

// CleanupExpired 周期性清理超时连接（分批，单次最多 100 条）。
func (m *Manager) CleanupExpired(max int) []*Track {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var expired []*Track
	count := 0
	for key, t := range m.tracks {
		if count >= max {
			break
		}
		if now.Sub(t.LastActiveAt) > m.idleTimeout {
			t.Status = "Timeout"
			expired = append(expired, t)
			if m.byAgent[t.AgentID] != nil {
				delete(m.byAgent[t.AgentID], key)
			}
			delete(m.tracks, key)
			count++
		}
	}
	return expired
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

// ServeRule 为一条映射规则启动公网监听端口，将入站连接转发至目标 Agent。
func (m *Manager) ServeRule(publicPort int, agentID string) error {
	if m.agentForPort == nil {
		return fmt.Errorf("agent resolver not set")
	}
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
			go m.handleConnection(clientConn, publicPort, agentID)
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

// handleConnection 转发一条公网入站连接到目标 Agent 的映射端口。
func (m *Manager) handleConnection(clientConn net.Conn, publicPort int, agentID string) {
	defer clientConn.Close()
	defer m.CloseTrack(publicPort)

	// 目标 Agent 需在线，并通过其连接发起端口映射隧道（此处简化为直接转发到本地端口）。
	// 实际项目中，此处应通过 Agent 长连接建立隧道通道；这里使用 Agent 映射的本机端口。
	localHost := "127.0.0.1"
	// 从 agentForPort 无法拿到本地端口，故端口映射规则中的 local 信息由 Server 维护。
	localPort := m.localPortFor(agentID)
	if localPort == 0 {
		return
	}
	upstream, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", localHost, localPort), 5*time.Second)
	if err != nil {
		log.Printf("[portmap] dial upstream %s:%d error: %v", localHost, localPort, err)
		return
	}
	defer upstream.Close()

	t := &Track{
		PublicPort:   publicPort,
		AgentID:      agentID,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		Status:       "Active",
	}
	m.TrackConn(t)

	// 双向复制流量
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(upstream, clientConn)
		m.UpdateTraffic(publicPort, 0, n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(clientConn, upstream)
		m.UpdateTraffic(publicPort, n, 0)
		done <- struct{}{}
	}()
	<-done
}

// localPortFor 为简化的默认本地端口；实际由端口映射规则提供。
func (m *Manager) localPortFor(agentID string) int {
	_ = agentID
	return 0
}
