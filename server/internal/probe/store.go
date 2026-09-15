// Package probe 维护 Agent 的探针数据、历史趋势与离线机器列表。
//
// 设备身份模型（重要）：
//
//	一台机器的唯一身份是**硬件 UUID**，agent_id 只是「当前这一份配置文件用的名字」。
//	同一个 UUID 无论 agent_id 怎么变（改配置、重装、换目录），都只应是一台机器。
//
//	因此内部以 deviceKey(uuid) 作为主键：
//	  - UUID 已知   -> key = "uuid:" + UUID
//	  - UUID 未知   -> key = "aid:"  + agent_id（旧版 Agent / 取不到 UUID 的宿主兜底）
//
//	同时维护 agent_id -> key 的别名索引，保证按 agent_id 的查询、命令路由、
//	探针上报都能落到同一台机器上；agent_id 变更时执行「重命名 + 合并历史」，
//	不产生第二条记录（这正是本次修复的核心）。
package probe

import (
	"log"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/geo"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/storage"
)

// deviceKey 由 UUID 计算设备主键；UUID 为空时回退到 agent_id。
func deviceKey(uuid, agentID string) string {
	if u := normalizeUUID(uuid); u != "" {
		return "uuid:" + u
	}
	return "aid:" + agentID
}

// normalizeUUID 归一化 UUID：去空白、统一大写。
// 不同工具输出的 UUID 大小写/空白不一致（wmic 大写、dmidecode 小写），
// 不归一化会被判成两台机器。
func normalizeUUID(uuid string) string {
	return strings.ToUpper(strings.TrimSpace(uuid))
}

// Store 保存全部 Agent 的探针状态与历史趋势。
type Store struct {
	mu sync.RWMutex
	// devices 以设备身份（UUID 优先）为键，一台机器一条记录
	devices map[string]*protocol.AgentInfo
	// byAgent agent_id -> deviceKey 别名索引（agent_id 可随配置变更）
	byAgent map[string]string
	// history 记录每台设备的历史指标采样，key=deviceKey
	history map[string][]*protocol.ProbeDataReq
	// remarkFn 备注获取函数（由 server 注入）
	remarkFn func(agentID string) string
	// remarkRenameFn 备注迁移函数（由 server 注入）：agent_id 变更时同步备注
	remarkRenameFn func(oldID, newID string) error
	// geoResolver 地理位置解析器
	geoResolver *geo.Resolver
	// db SQLite 持久化（可选）
	db *storage.DB
}

// NewStore 创建探针仓库。可选传入 SQLite DB 启用持久化。
func NewStore(db *storage.DB) *Store {
	s := &Store{
		devices: make(map[string]*protocol.AgentInfo),
		byAgent: make(map[string]string),
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
//
// 数据库里可能存在历史遗留的「同一 UUID 多条记录」（旧版本按 agent_id 建主键导致），
// 这里按 UUID 归并：只保留最新一条作为设备记录，其余 agent_id 注册为别名，
// 并继承可用的历史采样 key。
func (s *Store) loadAgentsFromDB() {
	rows, err := s.db.LoadAgents()
	if err != nil {
		log.Printf("[probe] 从数据库加载主机信息失败: %v", err)
		return
	}
	merged := 0
	for _, row := range rows {
		key := deviceKey(row.UUID, row.AgentID)
		info := &protocol.AgentInfo{
			AgentID:        row.AgentID,
			UUID:           normalizeUUID(row.UUID),
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
		// 同一设备已存在（UUID 相同）：保留更「新」的一条作为主记录，其余作别名
		if old, ok := s.devices[key]; ok {
			merged++
			aliasKey := "aid:" + row.AgentID
			if aliasKey != key {
				s.byAgent[row.AgentID] = key
			}
			s.byAgent[old.AgentID] = key
			if info.LastSeen > old.LastSeen {
				// 沿用旧记录里更早的首次离线时间，避免时间线倒退
				if info.LastOfflineAt == 0 {
					info.LastOfflineAt = old.LastOfflineAt
				}
				s.devices[key] = info
			}
			continue
		}
		s.devices[key] = info
		s.byAgent[row.AgentID] = key
	}
	if merged > 0 {
		log.Printf("[probe] 检测到 %d 条按 agent_id 重复的主机记录，已按 UUID 归并", merged)
	}
	log.Printf("[probe] 从数据库恢复了 %d 台主机信息", len(s.devices))
}

// SetRemarkFn 注入备注获取函数。
func (s *Store) SetRemarkFn(fn func(string) string) { s.remarkFn = fn }

// SetRemarkRenameFn 注入备注迁移函数（agent_id 变更时把备注带到新 ID）。
func (s *Store) SetRemarkRenameFn(fn func(oldID, newID string) error) { s.remarkRenameFn = fn }

// SetGeoResolver 注入地理位置解析器。
func (s *Store) SetGeoResolver(r *geo.Resolver) { s.geoResolver = r }

// resolveLocked 返回 agentID 对应的设备主键（调用方须持有锁）。
// 未知 agentID 时回退为「把它当作 UUID 未知的新设备」的键，便于纯查询场景。
func (s *Store) resolveLocked(agentID string) string {
	if k, ok := s.byAgent[agentID]; ok {
		return k
	}
	return "aid:" + agentID
}

// DeviceKey 返回某 agent_id 当前对应的设备主键（不存在返回空）。
func (s *Store) DeviceKey(agentID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if k, ok := s.byAgent[agentID]; ok {
		return k
	}
	return ""
}

// UpsertAgent 注册/更新 Agent 基础信息。
//
// 关键行为：若本次上报的 UUID 已对应一台已知设备，则把该设备「重命名」为新
// agent_id（保留历史指标、离线时间与备注），而不是新增一条记录。
func (s *Store) UpsertAgent(info *protocol.AgentInfo) {
	info.UUID = normalizeUUID(info.UUID)

	s.mu.Lock()
	// 先按 agent_id 别名定位已知设备：本次未上报 UUID 时也要能落到同一台机器，
	// 否则会被当成新设备（uuid 键 vs aid 键），反而制造冗余。
	key := deviceKey(info.UUID, info.AgentID)
	if prevKey, ok := s.byAgent[info.AgentID]; ok {
		if info.UUID == "" {
			// 未上报 UUID：沿用该 agent_id 已绑定的设备
			key = prevKey
		} else if prevKey != key {
			// UUID 与既有绑定不一致：该 agent_id 被迁到了新机器。
			// 解除旧绑定；旧设备若仅靠 agent_id 兜底标识，则视为设备已消失。
			if prev := s.devices[prevKey]; prev != nil && strings.HasPrefix(prevKey, "aid:") {
				delete(s.devices, prevKey)
				if h, ok := s.history[prevKey]; ok {
					s.history[key] = append(s.history[key], h...)
					delete(s.history, prevKey)
				}
			}
		}
	}
	// 兜底：本次带着 UUID 上线，但库里只有一条「同主机名且无 UUID」的旧记录
	// （旧版 Agent 没有上报 / 上报失败）。把它认作同一台设备并迁移到 UUID 主键，
	// 否则这台机器会因"首次拿到 UUID"而被记成两台。
	if _, exists := s.devices[key]; !exists && info.UUID != "" {
		if adopt, k := s.findUUIDLessByHostLocked(info); adopt != nil {
			if h, ok := s.history[k]; ok {
				s.history[key] = append(s.history[key], h...)
				delete(s.history, k)
			}
			if adopt.LastOfflineAt > 0 && info.LastOfflineAt == 0 {
				info.LastOfflineAt = adopt.LastOfflineAt
			}
			delete(s.devices, k)
			log.Printf("[probe] 主机 %q 首次上报 UUID %s，已把旧记录（agent_id=%s）归并到该设备",
				info.Hostname, info.UUID, adopt.AgentID)
		}
	}
	old, exists := s.devices[key]

	if exists && old != nil {
		// 保留设备历史上更早的离线时间；在线状态取本次结果
		if !info.Online && old.Online {
			info.LastOfflineAt = time.Now().Unix()
		} else {
			info.LastOfflineAt = old.LastOfflineAt
		}
		// 本次未上报 UUID 时沿用历史值，避免重连后丢失设备唯一标识
		if info.UUID == "" {
			info.UUID = old.UUID
		}
		// agent_id 变更（配置变更 / 重装）：沿用旧 agent_id 的历史指标兜底，
		// 保证「一台机器一条记录」且趋势图连续。
		if info.AgentID != old.AgentID {
			log.Printf("[probe] 设备 %s 的 agent_id 由 %q 变更为 %q，按同一台机器归并",
				displayDevice(info), old.AgentID, info.AgentID)
		}
	}

	// agent_id 变更（同一台机器改了配置）时，把旧 agent_id 对应的内存历史
	// 一并归到当前设备键下，保证趋势图不断档。
	renamedFrom := ""
	if exists && old != nil && old.AgentID != "" && old.AgentID != info.AgentID {
		renamedFrom = old.AgentID
		if h, ok := s.history[renamedFrom]; ok && renamedFrom != key {
			s.history[key] = append(s.history[key], h...)
			delete(s.history, renamedFrom)
		}
	}

	s.devices[key] = info
	s.byAgent[info.AgentID] = key
	// 旧 agent_id 继续作为别名指向同一设备，避免前端/审计里按旧 ID 查不到
	if renamedFrom != "" {
		s.byAgent[renamedFrom] = key
	}
	s.mu.Unlock()

	// agent_id 变更时同步迁移备注（备注按 agent_id 存储，不迁移会丢失）
	if renamedFrom != "" && s.remarkRenameFn != nil {
		if err := s.remarkRenameFn(renamedFrom, info.AgentID); err != nil {
			log.Printf("[probe] 迁移主机备注 (%s -> %s) 失败: %v", renamedFrom, info.AgentID, err)
		}
	}

	// 异步写入 SQLite
	if s.db != nil {
		go func() {
			// agent_id 变更：先把库里旧行的 agent_id 与历史采样改归属，再落新值，
			// 否则会留下一条"孤儿主机"（数据冗余）。
			if renamedFrom != "" {
				if err := s.db.UpdateAgentIdentity(renamedFrom, info.AgentID, info.UUID); err != nil {
					log.Printf("[probe] 迁移主机身份 (%s -> %s) 失败: %v", renamedFrom, info.AgentID, err)
				}
			}
			s.persistAgent(info)
		}()
	}
}

// findUUIDLessByHostLocked 查找可被「首次上报 UUID」的设备认领的无 UUID 旧记录。
//
// 规则（从严到宽，避免误合并不同机器）：
//  1. 主机名完全一致的无 UUID 记录；
//  2. 仅当全局只有一条无 UUID 记录时才认领（典型场景：单机从旧版升级）。
//
// 调用方须持有锁。
func (s *Store) findUUIDLessByHostLocked(info *protocol.AgentInfo) (*protocol.AgentInfo, string) {
	var onlyOne *protocol.AgentInfo
	var onlyKey string
	count := 0
	for k, d := range s.devices {
		if d == nil || d.UUID != "" || !strings.HasPrefix(k, "aid:") {
			continue
		}
		count++
		onlyOne, onlyKey = d, k
		if info.Hostname != "" && d.Hostname == info.Hostname {
			return d, k
		}
	}
	if count == 1 {
		return onlyOne, onlyKey
	}
	return nil, ""
}

// displayDevice 返回用于日志标识的字符串（UUID 优先，便于定位同一台机器）。
func displayDevice(info *protocol.AgentInfo) string {
	if info.UUID != "" {
		return info.UUID
	}
	return info.AgentID
}

// relocateByUUIDLocked 把 (agentID, uuid) 解析为设备主键，必要时迁移主键。
//
// 服务端启动时或 Agent 补齐 UUID 后，设备可能还挂在 "aid:<agent_id>" 键下；
// 这里把它迁到 "uuid:<UUID>" 键，保证「一台机器一个主键」。调用方须持有锁。
func (s *Store) relocateByUUIDLocked(agentID, uuid string) string {
	key := s.resolveLocked(agentID)
	uk := deviceKey(uuid, agentID)
	if uuid == "" || uk == key {
		return key
	}
	cur := s.devices[key] // 必须在 mergeDeviceLocked 删除该键之前取出
	// 目标键已有设备：把当前键（若有）并入目标，避免出现两条记录
	if target, ok := s.devices[uk]; ok && target != nil {
		if cur != nil && cur.Hostname != "" && target.Hostname == "" {
			target.Hostname = cur.Hostname
		}
		if cur != nil && cur.UUID == "" {
			target.UUID = uuid
		}
		s.mergeDeviceLocked(key, uk)
		s.byAgent[agentID] = uk
		return uk
	}
	if cur != nil {
		s.mergeDeviceLocked(key, uk)
		s.devices[uk] = cur
		cur.UUID = uuid
		s.byAgent[agentID] = uk
		return uk
	}
	// 既无已知设备也无已知 UUID 记录：直接以 UUID 建别名，
	// 让随后的注册/指标都落在同一台机器上。
	s.byAgent[agentID] = uk
	return uk
}

// mergeDeviceLocked 把 from 键的历史采样与设备记录并入 to 键（from==to 时无操作）。
func (s *Store) mergeDeviceLocked(from, to string) {
	if from == to {
		return
	}
	if h, ok := s.history[from]; ok {
		s.history[to] = append(s.history[to], h...)
		delete(s.history, from)
	}
	delete(s.devices, from)
}

// UpdateProbe 更新探针指标并追加历史采样。
func (s *Store) UpdateProbe(p *protocol.ProbeDataReq) {
	p.UUID = normalizeUUID(p.UUID)

	s.mu.Lock()
	// 以连接身份（agent_id）解析到设备；UUID 上报时按 UUID 重新归位
	key := s.relocateByUUIDLocked(p.AgentID, p.UUID)
	a, ok := s.devices[key]
	if !ok {
		// 探针先于注册到达（或设备信息被清理）：先建一条最小记录，
		// 避免指标无处落库、后续注册时又新建一条（数据冗余）。
		a = &protocol.AgentInfo{AgentID: p.AgentID, UUID: p.UUID}
		s.devices[key] = a
		s.byAgent[p.AgentID] = key
	}
	if a != nil {
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
	h := s.history[key]
	// 历史采样里的 agent_id 统一写成设备当前 agent_id，保证趋势图/查询口径一致
	p.AgentID = s.deviceAgentIDLocked(key, p.AgentID)
	h = append(h, p)
	if len(h) > 1000 { // 保留最近 1000 条采样
		h = h[len(h)-1000:]
	}
	s.history[key] = h
	s.mu.Unlock()

	// 异步写入探针历史到 SQLite
	if s.db != nil {
		go s.persistProbeHistory(p)
	}
}

// deviceAgentIDLocked 返回设备当前对外使用的 agent_id（调用方须持有锁）。
func (s *Store) deviceAgentIDLocked(key, fallback string) string {
	if a, ok := s.devices[key]; ok && a != nil && a.AgentID != "" {
		return a.AgentID
	}
	return fallback
}

// MarkOffline 将 Agent 标记为离线。
func (s *Store) MarkOffline(agentID string) {
	s.mu.Lock()
	key := s.resolveLocked(agentID)
	if a, ok := s.devices[key]; ok {
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

// Get 返回单个 Agent（按 agent_id 查询，自动通过别名索引定位设备）。
func (s *Store) Get(agentID string) *protocol.AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, _ := s.devices[s.resolveLocked(agentID)]
	return a
}

// GetByUUID 返回某个硬件 UUID 对应的 Agent（不存在返回 nil）。
func (s *Store) GetByUUID(uuid string) *protocol.AgentInfo {
	if uuid == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, _ := s.devices[deviceKey(uuid, "")]
	return a
}

// All 返回全部 Agent（含离线）。
func (s *Store) All() []*protocol.AgentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*protocol.AgentInfo, 0, len(s.devices))
	for _, a := range s.devices {
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

// HistoryDownsampled 返回某 Agent 的历史采样，并按需降采样到 maxPoints 个点。
// 数据量随时间增长，长时间跨度直接返回全量会拖慢前端渲染与网络传输，
// 因此按「总采样数 / maxPoints」计算分桶步长，在 SQL 层聚合求平均。
func (s *Store) HistoryDownsampled(agentID string, since time.Time, maxPoints int) []*protocol.ProbeDataReq {
	if maxPoints <= 0 {
		maxPoints = 300
	}
	if agentID == "" {
		return []*protocol.ProbeDataReq{}
	}
	if s.db != nil {
		count, err := s.db.CountProbeHistory(agentID, since.Unix())
		if err == nil && count > 0 {
			step := int(count) / maxPoints
			if step < 1 {
				step = 1
			}
			rows, qerr := s.db.QueryProbeHistorySince(agentID, since.Unix(), maxPoints, step)
			if qerr == nil {
				out := make([]*protocol.ProbeDataReq, 0, len(rows))
				for _, r := range rows {
					out = append(out, &protocol.ProbeDataReq{
						AgentID:        r.AgentID,
						Timestamp:      r.Timestamp,
						CPUUsage:       r.CPUUsage,
						MemUsage:       r.MemUsage,
						DiskUsage:      r.DiskUsage,
						NetInBytes:     r.NetInBytes,
						NetOutBytes:    r.NetOutBytes,
						OSType:         r.OSType,
						KernelVersion:  r.KernelVersion,
						Uptime:         r.Uptime,
						PublicIP:       r.PublicIP,
						OSVersion:      r.OSVersion,
						CPUModel:       r.CPUModel,
						CPUMHz:         r.CPUMHz,
						MemTotalBytes:  r.MemTotal,
						MemUsedBytes:   r.MemUsed,
						DiskTotalBytes: r.DiskTotal,
						DiskUsedBytes:  r.DiskUsed,
					})
				}
				return out
			}
			log.Printf("[probe] 降采样查询失败，回退常规查询: %v", qerr)
		}
	}
	// 内存回退：直接切片抽稀
	all := s.History(agentID, since)
	if len(all) <= maxPoints {
		return all
	}
	step := float64(len(all)) / float64(maxPoints)
	out := make([]*protocol.ProbeDataReq, 0, maxPoints)
	for i := 0; i < maxPoints; i++ {
		out = append(out, all[int(float64(i)*step)])
	}
	return out
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
	h := s.history[s.resolveLocked(agentID)]
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
