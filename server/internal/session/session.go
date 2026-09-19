// Package session 维护在线 Agent 的 TCP 长连接与会话池。
package session

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// Conn 代表一个 Agent 的长连接会话。
type Conn struct {
	mu      sync.Mutex
	AgentID string
	// UUID 硬件唯一标识：设备的真正身份。agent_id 可随配置变更，
	// 同 UUID 的连接视为同一台机器（用于注册时顶替自己的旧连接）。
	UUID       string
	Hostname   string
	NetConn    net.Conn
	Online     bool
	LastSeen   time.Time
	RemoteAddr string
	// CertCN 客户端证书的 CN（"简单模式"下由服务端签发的证书 CN = agent_id）。
	// 有证书时身份以证书为准，避免"报文自报身份"被冒用。
	CertCN string
	enc    *json.Encoder
	// HeartbeatInterval 该 Agent 当前被服务端动态下发的心跳间隔（秒）。
	// 用于在 Tick 中按每个 Agent 的实际心跳频率判定超时，
	// 避免弱网下心跳间隔被拉长后仍按固定阈值误判离线。
	HeartbeatInterval int
	// AgentVersion Agent 注册时自报的版本号（空串 = 旧版 Agent 未上报）。
	AgentVersion string
}

// SetAgentVersion 记录 Agent 自报的版本号（注册时写入）。
func (c *Conn) SetAgentVersion(v string) {
	c.mu.Lock()
	c.AgentVersion = strings.TrimSpace(v)
	c.mu.Unlock()
}

// GetAgentVersion 线程安全地读取 Agent 版本号。
func (c *Conn) GetAgentVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AgentVersion
}

// SetLastSeen 线程安全地刷新连接的最后活跃时间。
//
// LastSeen 会被读连接循环（每次收到报文）写，同时被 Tick（心跳清扫）与
// isStaleLocked（判断旧连接是否已死）读。直接赋值属于数据竞争：
// `go test -race` 能直接报出，且 race 下 Tick 可能读到半写的时间值导致误判离线。
func (c *Conn) SetLastSeen(t time.Time) {
	c.mu.Lock()
	c.LastSeen = t
	c.mu.Unlock()
}

// GetLastSeen 线程安全地读取连接的最后活跃时间。
func (c *Conn) GetLastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.LastSeen
}

// UUID 属于设备身份，连接建立后不再变化；ProvideUUID/GetUUID 兼顾
// 「先用后设」的注册流程，避免在数据竞争检测中被误报。
func (c *Conn) SetUUID(u string) {
	c.mu.Lock()
	c.UUID = strings.TrimSpace(u)
	c.mu.Unlock()
}

// GetUUID 线程安全地读取设备 UUID。
func (c *Conn) GetUUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UUID
}

// SetAgentID 设置连接身份（agent_id）。注册成功后才允许调用。
func (c *Conn) SetAgentID(id string) {
	c.mu.Lock()
	c.AgentID = id
	c.mu.Unlock()
}

// GetAgentID 线程安全地读取连接身份。
func (c *Conn) GetAgentID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AgentID
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

// SetOnline 线程安全地设置在线状态。
func (c *Conn) SetOnline(v bool) {
	c.mu.Lock()
	c.Online = v
	c.mu.Unlock()
}

// IsOnline 线程安全地读取在线状态。
func (c *Conn) IsOnline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Online
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

// DefaultStaleAfter 判定「旧连接已死」的阈值：超过它仍无心跳才允许被新连接替换。
//
// 取 3 倍默认心跳（30s）。太长会让正常重启的 Agent 等太久，太短则给了
// 冒名顶替者可乘之机。
const DefaultStaleAfter = 90 * time.Second

// Manager 维护全部 Agent 会话。
type Manager struct {
	mu         sync.RWMutex
	conns      map[string]*Conn
	staleAfter time.Duration
}

// NewManager 创建会话管理器。
func NewManager() *Manager {
	return &Manager{
		conns:      make(map[string]*Conn),
		staleAfter: DefaultStaleAfter,
	}
}

// SetStaleAfter 覆盖「旧连接视为已死」的阈值。
func (m *Manager) SetStaleAfter(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		d = DefaultStaleAfter
	}
	m.staleAfter = d
}

// Upsert 注册 Agent 连接。
//
// 语义变更（安全加固）：同名 AgentID 已有**仍然存活**的连接时，**拒绝**本次注册并
// 返回 accepted=false —— 旧实现会直接 Close() 旧连接再顶替，等于任何人只要知道
// 一个 AgentID 就能把真实主机踢下线并接管其身份（从而收到发往该主机的命令、
// 推送的文件与 VNC 隧道数据）。
//
// 只有在旧连接已超过 staleAfter 没有心跳（视为死连接）时才允许替换，
// 以免正常重启的 Agent 被自己的僵尸连接挡住。
//
// 返回 (existing, accepted)。
func (m *Manager) Upsert(agentID string, c *Conn) (*Conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.conns[agentID]
	if exists && old != nil && old != c {
		if !m.isStaleLocked(old, time.Now()) {
			return old, false // 拒绝：该 ID 已有活跃连接
		}
		log.Printf("[session] Agent %s 的旧连接已超时未心跳，允许新连接替换（旧来自 %s，新来自 %s）",
			agentID, old.RemoteAddr, c.RemoteAddr)
		if old.NetConn != nil {
			_ = old.NetConn.Close()
		}
	}
	m.conns[agentID] = c
	return old, true
}

// ReplaceByUUID 关闭同一硬件设备（UUID）上除 keep 之外的其它连接，并返回被顶替的
// agent_id 列表。
//
// 场景：Agent 改了配置里的 agent_id（同机重启/改配置）后重连。此时旧连接可能仍在
// 会话池里（尚未心跳超时），若不处理，同一台机器会出现两条会话、两份在线记录。
// 设备身份是 UUID，因此这里以 UUID 为准替换自己的旧连接 —— 这不放松安全约束：
// 只有能通过注册鉴权、且声明同一 UUID 的连接才会命中。
func (m *Manager) ReplaceByUUID(uuid string, keep *Conn) []string {
	if uuid == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var replaced []string
	for id, c := range m.conns {
		// 只按「连接实例」比较：keep 是新连接，尚未入池时也不应误删；
		// 不能按 agent_id 跳过 —— 否则同 agent_id 的僵尸连接会躲过顶替检查。
		if c == nil || c == keep {
			continue
		}
		if strings.EqualFold(c.GetUUID(), strings.TrimSpace(uuid)) {
			if c.NetConn != nil {
				_ = c.NetConn.Close()
			}
			delete(m.conns, id)
			replaced = append(replaced, id)
		}
	}
	return replaced
}

// isStaleLocked 判断连接是否已过 staleAfter 未心跳。
func (m *Manager) isStaleLocked(c *Conn, now time.Time) bool {
	lastSeen := c.GetLastSeen()
	if lastSeen.IsZero() {
		return true
	}
	threshold := m.staleAfter
	if threshold <= 0 {
		threshold = DefaultStaleAfter
	}
	return now.Sub(lastSeen) > threshold
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

// RemoveIfCurrent 仅当 agentID 当前对应的连接就是 c 时才移除，返回是否已移除。
//
// 用途：同一台设备（UUID）改 agent_id 重连后，旧连接的关闭流程会晚于新连接注册。
// 用「比较指针」的方式移除，避免旧连接的收尾逻辑把新连接误删/误标离线。
func (m *Manager) RemoveIfCurrent(agentID string, c *Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.conns[agentID]
	if !ok || cur != c {
		return false
	}
	delete(m.conns, agentID)
	return true
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
		if !c.IsOnline() {
			continue
		}
		threshold := c.heartbeatTimeout()
		if threshold < offlineAfter {
			threshold = offlineAfter
		}
		if now.Sub(c.GetLastSeen()) > threshold {
			offline = append(offline, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range offline {
		m.Remove(id)
	}
	return offline
}
