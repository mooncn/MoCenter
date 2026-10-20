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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
	"mocenter/server/internal/storage"
)

// Track 单条连接跟踪记录。
type Track struct {
	TunnelID      string    `json:"tunnel_id"`
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

// 越权隧道回传的留痕状态（每台 Agent 一条，限频用）。
// 归属校验本身在 publicTrack.agentID 上，这里只是防止被高频伪造回传刷满日志。
var (
	mismatchMu sync.Mutex
	mismatchAt = make(map[string]time.Time)
)

// Manager 端口映射管理器，负责隧道建立、连接跟踪与双向转发。
type Manager struct {
	mu           sync.RWMutex
	tracks       map[string]*Track            // key: TunnelID
	byAgent      map[string]map[string]*Track // 二级索引：agentID -> TunnelID -> Track
	pub          map[string]*publicTrack      // key: TunnelID -> 公网连接
	rules        map[int]*Rule
	listeners    map[int]net.Listener
	sessions     *session.Manager
	db           *storage.DB      // SQLite 持久化
	agentForPort func(int) string // 由 Server 注入：根据公网端口找到目标 AgentID
	idleTimeout  time.Duration
	seq          uint64
}

// NewManager 创建端口映射管理器。可选传入 SQLite DB 启用持久化。
func NewManager(sess *session.Manager, db *storage.DB) *Manager {
	m := &Manager{
		tracks:      make(map[string]*Track),
		byAgent:     make(map[string]map[string]*Track),
		pub:         make(map[string]*publicTrack),
		rules:       make(map[int]*Rule),
		listeners:   make(map[int]net.Listener),
		sessions:    sess,
		db:          db,
		idleTimeout: 300 * time.Second,
	}
	// 从数据库加载已持久化的规则
	if db != nil {
		m.loadRulesFromDB()
	}
	return m
}

// loadRulesFromDB 从数据库加载全部规则到内存，并自动恢复启用的规则监听。
func (m *Manager) loadRulesFromDB() {
	rows, err := m.db.LoadPortmapRules()
	if err != nil {
		log.Printf("[portmap] 从数据库加载规则失败: %v", err)
		return
	}
	for _, row := range rows {
		r := &Rule{
			ID:         row.ID,
			PublicPort: row.PublicPort,
			AgentID:    row.AgentID,
			LocalHost:  row.LocalHost,
			LocalPort:  row.LocalPort,
			Enabled:    row.Enabled,
			SourceIP:   row.SourceIP,
			CreatedAt:  row.CreatedAt,
		}
		m.rules[r.PublicPort] = r
		if r.Enabled {
			if err := m.ServeRule(r); err != nil {
				log.Printf("[portmap] 恢复规则 %d 监听失败: %v", r.PublicPort, err)
				r.Enabled = false
			} else {
				log.Printf("[portmap] 恢复规则 %d -> agent=%s:%d", r.PublicPort, r.AgentID, r.LocalPort)
			}
		}
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

// CreateRule 创建或覆盖一条端口映射规则，并让其立即按新规则转发。
//
// "改一条已存在的映射"是最容易做错的路径：必须先停旧监听、监听成功后才提交
// 规则，否则会出现"界面显示已收紧、实际还在按旧规则放行"（M-7）：
//   - 旧实现先覆盖 m.rules 再 Listen，端口还被自己的旧监听占着 → Listen 必然失败，
//     而旧 accept 循环闭包里握着**旧** *Rule，继续按旧 SourceIP 白名单放行；
//   - 现在改成「停旧 → 起新 → 提交」：Listen 失败时规则与库里都保持旧值，
//     在跑的转发与看到的配置不可能分叉。
func (m *Manager) CreateRule(r *Rule) error {
	if r.PublicPort <= 0 || r.LocalPort <= 0 {
		return fmt.Errorf("invalid public/local port")
	}
	if r.LocalHost == "" {
		r.LocalHost = "127.0.0.1"
	}
	r.Enabled = true

	prev := m.GetRule(r.PublicPort)
	m.StopRule(r.PublicPort)
	if err := m.ServeRule(r); err != nil {
		// 起不来就把旧监听还原：否则这次失败的修改会把原本在跑的端口一起弄没
		if prev != nil {
			if err2 := m.ServeRule(prev); err2 != nil {
				log.Printf("[portmap] 规则 %d 修改失败（%v）且旧监听恢复失败，端口已停止对外服务: %v",
					r.PublicPort, err, err2)
			}
		}
		return err
	}
	m.mu.Lock()
	m.rules[r.PublicPort] = r
	m.mu.Unlock()
	// 持久化到 SQLite
	if m.db != nil {
		if err := m.db.SavePortmapRule(&storage.PortmapRuleRow{
			ID:         r.ID,
			PublicPort: r.PublicPort,
			AgentID:    r.AgentID,
			LocalHost:  r.LocalHost,
			LocalPort:  r.LocalPort,
			Enabled:    r.Enabled,
			SourceIP:   r.SourceIP,
			CreatedAt:  r.CreatedAt,
		}); err != nil {
			log.Printf("[portmap] 规则 %d 持久化失败: %v", r.PublicPort, err)
		}
	}
	return nil
}

// DeleteRule 删除规则并停止监听。
func (m *Manager) DeleteRule(publicPort int) {
	m.StopRule(publicPort)
	m.mu.Lock()
	delete(m.rules, publicPort)
	m.mu.Unlock()
	// 从 SQLite 删除
	if m.db != nil {
		if err := m.db.DeletePortmapRule(publicPort); err != nil {
			log.Printf("[portmap] 删除规则 %d 持久化失败: %v", publicPort, err)
		}
	}
}

// DeleteRulesForAgents 删除目标主机（含历史别名）的全部端口映射规则，
// 返回删除的规则数。删除主机时调用：规则指向一台已不存在的设备，继续监听
// 只会让公网连接被接受后无处可转。
func (m *Manager) DeleteRulesForAgents(agentIDs []string) int {
	want := make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return 0
	}
	ports := make([]int, 0, 4)
	m.mu.RLock()
	for port, r := range m.rules {
		if r != nil && want[r.AgentID] {
			ports = append(ports, port)
		}
	}
	m.mu.RUnlock()
	for _, port := range ports {
		m.DeleteRule(port)
	}
	return len(ports)
}

// DisableRule 停用规则：停止公网监听，但保留规则供后续重新启用。
//
// 规则对象会被 Rules() 交给 API 层并发序列化，因此状态变更一律"换对象"而不是
// 原地改字段 —— 原地写会与读取者构成数据竞争（-race 可直接报出）。
func (m *Manager) DisableRule(publicPort int) {
	m.StopRule(publicPort)
	m.mu.Lock()
	if r, ok := m.rules[publicPort]; ok && r != nil {
		next := *r
		next.Enabled = false
		m.rules[publicPort] = &next
	}
	m.mu.Unlock()
	// 更新 SQLite 中的启用状态
	if m.db != nil {
		if err := m.db.UpdatePortmapRuleEnabled(publicPort, false); err != nil {
			log.Printf("[portmap] 停用规则 %d 持久化失败: %v", publicPort, err)
		}
	}
}

// EnableRule 重新启用规则：若规则仍存在且已停用，则恢复公网监听。
// 返回错误信息（如端口被占用或规则不存在）。
func (m *Manager) EnableRule(publicPort int) error {
	m.mu.Lock()
	r, ok := m.rules[publicPort]
	enabled := ok && r != nil && r.Enabled
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("规则 %d 不存在", publicPort)
	}
	if enabled {
		return nil
	}
	if err := m.ServeRule(r); err != nil {
		return err
	}
	m.mu.Lock()
	if cur := m.rules[publicPort]; cur != nil {
		next := *cur
		next.Enabled = true
		m.rules[publicPort] = &next
	}
	m.mu.Unlock()
	// 更新 SQLite 中的启用状态
	if m.db != nil {
		if err := m.db.UpdatePortmapRuleEnabled(publicPort, true); err != nil {
			log.Printf("[portmap] 启用规则 %d 持久化失败: %v", publicPort, err)
		}
	}
	return nil
}

// parseSourceIPList 解析逗号分隔的源 IP 白名单（IP 或 CIDR），忽略空项。
func parseSourceIPList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ServeRule 为一条映射规则启动公网监听端口，将入站连接转发至目标 Agent。
//
// 安全提示：监听绑定在所有接口（:port）。规则未设置源 IP 白名单时，
// 映射出去的内网服务对**全网**开放 —— 这一点会打印警告。
func (m *Manager) ServeRule(r *Rule) error {
	publicPort := r.PublicPort
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", publicPort))
	if err != nil {
		return err
	}
	if len(parseSourceIPList(r.SourceIP)) == 0 {
		log.Printf("[portmap] 警告: 规则 %d (%s:%d) 未设置源 IP 白名单，将对全网开放",
			publicPort, r.AgentID, r.LocalPort)
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
//
// 这里是源 IP 白名单真正生效的地方（此前字段只存不用，等于没做）。
func (m *Manager) handleConnection(clientConn net.Conn, publicPort int, r *Rule) {
	defer clientConn.Close()

	// 源 IP 白名单：逗号分隔的 IP / CIDR；为空表示不限制
	if list := parseSourceIPList(r.SourceIP); len(list) > 0 && !auth.AddrAllowed(list, clientConn.RemoteAddr()) {
		log.Printf("[portmap] 拒绝来自 %s 的连接（不在端口 %d 的源 IP 白名单内）",
			clientConn.RemoteAddr(), publicPort)
		return
	}

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
		TunnelID:     tunnelID,
		PublicConnID: tunnelID,
		PublicPort:   publicPort,
		AgentID:      r.AgentID,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		Status:       "Active",
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
// agentID 必须是回传方的**连接身份**：隧道 ID 由服务端生成但可被其他 Agent 猜到或
// 重放，不校验归属就等于任一台被攻陷的主机能往别人的公网连接里塞数据。
func (m *Manager) DeliverTunnelData(tunnelID, agentID string, data []byte) {
	m.mu.RLock()
	pt := m.pub[tunnelID]
	m.mu.RUnlock()
	if pt == nil {
		return
	}
	if pt.agentID != agentID {
		m.logMismatch(agentID, pt.agentID, tunnelID, "隧道数据")
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
// reason 目前不落审计，只保留在协议字段里；归属校验以 agentID 为准。
func (m *Manager) NotifyTunnelClosed(tunnelID, agentID, _ string) {
	m.mu.RLock()
	pt := m.pub[tunnelID]
	m.mu.RUnlock()
	if pt == nil {
		return
	}
	if pt.agentID != agentID {
		m.logMismatch(agentID, pt.agentID, tunnelID, "隧道关闭")
		return
	}
	m.removePublic(tunnelID)
	m.CloseTrack(tunnelID)
}

// mismatchLogInterval 越权隧道回传的留痕频率：每台伪造方 Agent 每分钟一条。
const mismatchLogInterval = time.Minute

// logMismatch 记录一次"用别人的隧道 ID 回传"，按伪造方 agentID 限频。
func (m *Manager) logMismatch(from, owner, tunnelID, kind string) {
	log.Printf("[portmap] 丢弃 %s 的%s：隧道 %s 属于 %s（身份与归属不符）", from, kind, tunnelID, owner)
	mismatchMu.Lock()
	defer mismatchMu.Unlock()
	now := time.Now()
	if last, seen := mismatchAt[from]; seen && now.Sub(last) < mismatchLogInterval {
		return
	}
	mismatchAt[from] = now
	for k, t := range mismatchAt {
		if now.Sub(t) > 10*time.Minute {
			delete(mismatchAt, k)
		}
	}
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
