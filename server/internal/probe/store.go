// Package probe 维护 Agent 的探针数据、历史趋势与离线机器列表。
package probe

import (
	"log"
	"sync"
	"time"

	"mocenter/server/internal/geo"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/storage"
)

// Store 保存全部 Agent 的探针状态与历史趋势。
type Store struct {
	mu     sync.RWMutex
	agents map[string]*protocol.AgentInfo
	// history 记录每个 Agent 的历史指标采样，key=agentID
	history map[string][]*protocol.ProbeDataReq
	// remarkFn 备注获取函数（由 server 注入）
	remarkFn func(agentID string) string
	// geoResolver 地理位置解析器
	geoResolver *geo.Resolver
	// db SQLite 持久化（可选）
	db *storage.DB
}

// NewStore 创建探针仓库。可选传入 SQLite DB 启用持久化。
func NewStore(db *storage.DB) *Store {
	s := &Store{
		agents:  make(map[string]*protocol.AgentInfo),
		history: make(map[string][]*protocol.ProbeDataReq),
		db:      db,
	}
	// 从数据库恢复主机信息
	if db != nil {
		s.loadAgentsFromDB()
	}
	return s
}

// loadAgentsFromDB 从数据库加载全部主机信息到内存（标记为离线，等待 Agent 重新上线）。
func (s *Store) loadAgentsFromDB() {
	rows, err := s.db.LoadAgents()
	if err != nil {
		log.Printf("[probe] 从数据库加载主机信息失败: %v", err)
		return
	}
	for _, row := range rows {
		info := &protocol.AgentInfo{
			AgentID:        row.AgentID,
			UUID:           row.UUID,
			Hostname:       row.Hostname,
			OSType:         row.OSType,
			Arch:           row.Arch,
			KernelVersion:  row.KernelVersion,
			Online:         false, // 重启后全部标记为离线，等待 Agent 重新连接
			LastSeen:       row.LastSeen,
			LastOfflineAt:  row.LastOfflineAt,
			Uptime:         row.Uptime,
			CPUUsage:       row.CPUUsage,
			MemUsage:       row.MemUsage,
			DiskUsage:      row.DiskUsage,
			NetInBytes:     row.NetInBytes,
			NetOutBytes:    row.NetOutBytes,
			RemoteAddr:     row.RemoteAddr,
			PublicIP:       row.PublicIP,
			PublicIPv6:     row.PublicIPv6,
			OSVersion:      row.OSVersion,
			CPUModel:       row.CPUModel,
			CPUMHz:         row.CPUMHz,
			MemTotalBytes:  row.MemTotal,
			MemUsedBytes:   row.MemUsed,
			DiskTotalBytes: row.DiskTotal,
			DiskUsedBytes:  row.DiskUsed,
			NetRateIn:      row.NetRateIn,
			NetRateOut:     row.NetRateOut,
		}
		s.agents[info.AgentID] = info
	}
	log.Printf("[probe] 从数据库恢复了 %d 台主机信息", len(rows))
}

// SetRemarkFn 注入备注获取函数。
func (s *Store) SetRemarkFn(fn func(string) string) { s.remarkFn = fn }

// SetGeoResolver 注入地理位置解析器。
func (s *Store) SetGeoResolver(r *geo.Resolver) { s.geoResolver = r }

// UpsertAgent 注册/更新 Agent 基础信息。
func (s *Store) UpsertAgent(info *protocol.AgentInfo) {
	s.mu.Lock()
	if old, ok := s.agents[info.AgentID]; ok {
		// 保留离线时间
		if !info.Online && old.Online {
			info.LastOfflineAt = time.Now().Unix()
		}
		info.LastOfflineAt = old.LastOfflineAt
		// 本次未上报 UUID 时沿用历史值，避免重连后丢失设备唯一标识
		if info.UUID == "" {
			info.UUID = old.UUID
		}
	}
	s.agents[info.AgentID] = info
	s.mu.Unlock()

	// 异步写入 SQLite
	if s.db != nil {
		go s.persistAgent(info)
	}
}

// UpdateProbe 更新探针指标并追加历史采样。
func (s *Store) UpdateProbe(p *protocol.ProbeDataReq) {
	s.mu.Lock()
	if a, ok := s.agents[p.AgentID]; ok {
		// 探针携带硬件 UUID 时补齐/更新（兼容旧版 Agent 未上报的情形）
		if p.UUID != "" {
			a.UUID = p.UUID
		}
		a.CPUUsage = p.CPUUsage
		a.MemUsage = p.MemUsage
		a.DiskUsage = p.DiskUsage
		// 计算网络速率（字节/秒）：相邻探针累计值差 / 时间间隔
		if a.NetInBytes > 0 && p.NetInBytes >= a.NetInBytes && a.LastSeen > 0 {
			dt := p.Timestamp - a.LastSeen
			if dt > 0 {
				a.NetRateIn = (p.NetInBytes - a.NetInBytes) / dt
			}
		} else {
			a.NetRateIn = 0
		}
		if a.NetOutBytes > 0 && p.NetOutBytes >= a.NetOutBytes && a.LastSeen > 0 {
			dt := p.Timestamp - a.LastSeen
			if dt > 0 {
				a.NetRateOut = (p.NetOutBytes - a.NetOutBytes) / dt
			}
		} else {
			a.NetRateOut = 0
		}
		a.NetInBytes = p.NetInBytes
		a.NetOutBytes = p.NetOutBytes
		a.KernelVersion = p.KernelVersion
		a.OSType = p.OSType
		a.Uptime = p.Uptime
		a.LastSeen = p.Timestamp
		a.PublicIP = firstNonEmpty(p.PublicIP, a.PublicIP)
		a.PublicIPv6 = firstNonEmpty(p.PublicIPv6, a.PublicIPv6)
		// 扩展指标（旧版 Agent 未上报时保留原值）
		a.OSVersion = firstNonEmpty(p.OSVersion, a.OSVersion)
		a.CPUModel = firstNonEmpty(p.CPUModel, a.CPUModel)
		if p.CPUMHz > 0 {
			a.CPUMHz = p.CPUMHz
		}
		if p.MemTotalBytes > 0 {
			a.MemTotalBytes, a.MemUsedBytes = p.MemTotalBytes, p.MemUsedBytes
		}
		if p.DiskTotalBytes > 0 {
			a.DiskTotalBytes, a.DiskUsedBytes = p.DiskTotalBytes, p.DiskUsedBytes
		}
	}
	h := s.history[p.AgentID]
	h = append(h, p)
	if len(h) > 1000 { // 保留最近 1000 条采样
		h = h[len(h)-1000:]
	}
	s.history[p.AgentID] = h
	s.mu.Unlock()

	// 异步写入探针历史到 SQLite
	if s.db != nil {
		go s.persistProbeHistory(p)
	}
}

// MarkOffline 将 Agent 标记为离线。
func (s *Store) MarkOffline(agentID string) {
	s.mu.Lock()
	if a, ok := s.agents[agentID]; ok {
		a.Online = false
		a.LastOfflineAt = time.Now().Unix()
	}
	s.mu.Unlock()

	// 异步更新 SQLite 中的离线状态
	if s.db != nil {
		go func() {
			if err := s.db.UpdateAgentOnline(agentID, false, time.Now().Unix()); err != nil {
				log.Printf("[probe] 更新主机离线状态到数据库失败: %v", err)
			}
		}()
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
		clone := *a
		// 注入备注
		if s.remarkFn != nil {
			clone.Remark = s.remarkFn(clone.AgentID)
		}
		// 解析公网 IP 地理位置
		ip := clone.PublicIP
		if ip == "" {
			ip = clone.PublicIPv6
		}
		if s.geoResolver != nil && ip != "" {
			clone.Geo = s.geoResolver.Lookup(ip)
		}
		out = append(out, &clone)
	}
	return out
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// History 返回某 Agent 的历史采样（按时间范围过滤）。
// 优先从 SQLite 查询（突破 1000 条内存上限），DB 不可用时回退内存。
func (s *Store) History(agentID string, since time.Time) []*protocol.ProbeDataReq {
	if s.db != nil {
		rows, err := s.db.QueryProbeHistory(agentID, since.Unix(), 10000)
		if err == nil {
			out := make([]*protocol.ProbeDataReq, 0, len(rows))
			for _, r := range rows {
				out = append(out, &protocol.ProbeDataReq{
					AgentID:       r.AgentID,
					Timestamp:     r.Timestamp,
					CPUUsage:      r.CPUUsage,
					MemUsage:      r.MemUsage,
					DiskUsage:     r.DiskUsage,
					NetInBytes:    r.NetInBytes,
					NetOutBytes:   r.NetOutBytes,
					OSType:        r.OSType,
					KernelVersion: r.KernelVersion,
					Uptime:        r.Uptime,
					PublicIP:      r.PublicIP,
					OSVersion:     r.OSVersion,
					CPUModel:      r.CPUModel,
					CPUMHz:        r.CPUMHz,
					MemTotalBytes: r.MemTotal,
					MemUsedBytes:  r.MemUsed,
					DiskTotalBytes: r.DiskTotal,
					DiskUsedBytes:  r.DiskUsed,
				})
			}
			return out
		}
		log.Printf("[probe] 从数据库查询探针历史失败，回退内存: %v", err)
	}

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

// persistAgent 异步将主机信息写入 SQLite。
func (s *Store) persistAgent(info *protocol.AgentInfo) {
	row := &storage.AgentRow{
		AgentID:       info.AgentID,
		UUID:          info.UUID,
		Hostname:      info.Hostname,
		OSType:        info.OSType,
		Arch:          info.Arch,
		KernelVersion: info.KernelVersion,
		Online:        info.Online,
		LastSeen:      info.LastSeen,
		LastOfflineAt: info.LastOfflineAt,
		Uptime:        info.Uptime,
		CPUUsage:      info.CPUUsage,
		MemUsage:      info.MemUsage,
		DiskUsage:     info.DiskUsage,
		NetInBytes:    info.NetInBytes,
		NetOutBytes:   info.NetOutBytes,
		RemoteAddr:    info.RemoteAddr,
		PublicIP:      info.PublicIP,
		PublicIPv6:    info.PublicIPv6,
		OSVersion:     info.OSVersion,
		CPUModel:      info.CPUModel,
		CPUMHz:        info.CPUMHz,
		MemTotal:      info.MemTotalBytes,
		MemUsed:       info.MemUsedBytes,
		DiskTotal:     info.DiskTotalBytes,
		DiskUsed:      info.DiskUsedBytes,
		NetRateIn:     info.NetRateIn,
		NetRateOut:    info.NetRateOut,
	}
	if err := s.db.UpsertAgent(row); err != nil {
		log.Printf("[probe] 持久化主机信息失败: %v", err)
	}
}

// persistProbeHistory 异步将探针历史写入 SQLite。
func (s *Store) persistProbeHistory(p *protocol.ProbeDataReq) {
	row := &storage.ProbeHistoryRow{
		AgentID:       p.AgentID,
		Timestamp:     p.Timestamp,
		CPUUsage:      p.CPUUsage,
		MemUsage:      p.MemUsage,
		DiskUsage:     p.DiskUsage,
		NetInBytes:    p.NetInBytes,
		NetOutBytes:   p.NetOutBytes,
		OSType:        p.OSType,
		KernelVersion: p.KernelVersion,
		Uptime:        p.Uptime,
		PublicIP:      p.PublicIP,
		OSVersion:     p.OSVersion,
		CPUModel:      p.CPUModel,
		CPUMHz:        p.CPUMHz,
		MemTotal:      p.MemTotalBytes,
		MemUsed:       p.MemUsedBytes,
		DiskTotal:     p.DiskTotalBytes,
		DiskUsed:      p.DiskUsedBytes,
	}
	if err := s.db.InsertProbeHistory(row); err != nil {
		log.Printf("[probe] 持久化探针历史失败: %v", err)
	}
}
