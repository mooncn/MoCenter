// Package storage — 端口映射规则持久化。
package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// PortmapRuleRow 端口映射规则持久化行。
type PortmapRuleRow struct {
	ID         string
	PublicPort int
	AgentID    string
	LocalHost  string
	LocalPort  int
	Enabled    bool
	SourceIP   string
	CreatedAt  int64
}

// SavePortmapRule 插入或替换一条端口映射规则。
func (db *DB) SavePortmapRule(r *PortmapRuleRow) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO portmap_rules
		(public_port, id, agent_id, local_host, local_port, enabled, source_ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.PublicPort, r.ID, r.AgentID, r.LocalHost, r.LocalPort, enabled, r.SourceIP, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("保存端口映射规则失败: %w", err)
	}
	return nil
}

// DeletePortmapRule 删除一条端口映射规则。
func (db *DB) DeletePortmapRule(publicPort int) error {
	_, err := db.Exec(`DELETE FROM portmap_rules WHERE public_port = ?`, publicPort)
	if err != nil {
		return fmt.Errorf("删除端口映射规则失败: %w", err)
	}
	return nil
}

// UpdatePortmapRuleEnabled 更新规则启用状态。
func (db *DB) UpdatePortmapRuleEnabled(publicPort int, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := db.Exec(`UPDATE portmap_rules SET enabled = ? WHERE public_port = ?`, v, publicPort)
	if err != nil {
		return fmt.Errorf("更新端口映射规则状态失败: %w", err)
	}
	return nil
}

// LoadPortmapRules 加载全部端口映射规则。
func (db *DB) LoadPortmapRules() ([]*PortmapRuleRow, error) {
	rows, err := db.Query(`SELECT public_port, id, agent_id, local_host, local_port, enabled, source_ip, created_at
		FROM portmap_rules ORDER BY public_port`)
	if err != nil {
		return nil, fmt.Errorf("查询端口映射规则失败: %w", err)
	}
	defer rows.Close()
	var out []*PortmapRuleRow
	for rows.Next() {
		var r PortmapRuleRow
		var enabled int
		if err := rows.Scan(&r.PublicPort, &r.ID, &r.AgentID, &r.LocalHost, &r.LocalPort, &enabled, &r.SourceIP, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// AuditLogRow 审计日志持久化行。
// 注意：JSON tag 必须与 audits.jsonl 中的字段名一致，否则用本结构体
// 反序列化历史 JSONL 时，snake_case 字段会静默落到零值。
type AuditLogRow struct {
	ID          int64  `json:"id"`
	Time        int64  `json:"time"`
	Operator    string `json:"operator"`
	OperatorIP  string `json:"operator_ip"`
	AgentID     string `json:"agent_id"`
	CommandID   string `json:"command_id"`
	Command     string `json:"command"`
	Result      string `json:"result"`
	ExitCode    int    `json:"exit_code"`
	Blocked     bool   `json:"blocked"`
	BlockReason string `json:"block_reason"`
	Duration    int64  `json:"duration_ms"`
	// Category 审计类别：command / op / auth。
	Category string `json:"category"`
}

// InsertAuditLog 插入一条审计日志，并返回新记录的 ID（供回填命令执行结果）。
func (db *DB) InsertAuditLog(l *AuditLogRow) (int64, error) {
	blocked := 0
	if l.Blocked {
		blocked = 1
	}
	if l.Category == "" {
		l.Category = "command"
	}
	res, err := db.Exec(`INSERT INTO audit_logs
		(time, operator, operator_ip, agent_id, command_id, command, result, exit_code, blocked, block_reason, duration_ms, category)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Time, l.Operator, l.OperatorIP, l.AgentID, l.CommandID, l.Command, l.Result, l.ExitCode, blocked, l.BlockReason, l.Duration, l.Category)
	if err != nil {
		return 0, fmt.Errorf("插入审计日志失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// CompleteAuditLog 回填一条审计日志的执行结果（按 ID 定位，仅更新结果字段）。
// 用于「命令下发」先落审计、Agent 回传结果后补齐 exit_code/result/duration，
// 避免同一次操作产生两条记录（其中一条 operator 为空）。
func (db *DB) CompleteAuditLog(id int64, result string, exitCode int, duration int64) error {
	if id <= 0 {
		return nil
	}
	_, err := db.Exec(`UPDATE audit_logs SET result = ?, exit_code = ?, duration_ms = ? WHERE id = ?`,
		result, exitCode, duration, id)
	if err != nil {
		return fmt.Errorf("回填审计日志结果失败: %w", err)
	}
	return nil
}

// CountAuditLogs 返回审计日志总行数（用于判断是否需要导入历史 JSONL）。
func (db *DB) CountAuditLogs() (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计审计日志失败: %w", err)
	}
	return n, nil
}

// AuditLogExists 判断是否已存在「同一时间 + 同一命令」的审计记录，
// 用于导入历史 JSONL 时去重，避免重复导入。
func (db *DB) AuditLogExists(time int64, command string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE time = ? AND command = ?`,
		time, command).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("查询审计日志是否存在失败: %w", err)
	}
	return n > 0, nil
}

// AuditQueryCond 审计日志查询条件。
type AuditQueryCond struct {
	Agent    string
	Operator string
	Command  string
	Category string
	Since    int64
	Until    int64
}

// buildAuditWhere 根据条件拼装审计日志查询的 WHERE 子句与参数。
func buildAuditWhere(cond AuditQueryCond) (string, []interface{}) {
	q := ` WHERE 1=1`
	var args []interface{}
	if cond.Agent != "" {
		q += ` AND agent_id LIKE ?`
		args = append(args, "%"+cond.Agent+"%")
	}
	if cond.Operator != "" {
		q += ` AND operator LIKE ?`
		args = append(args, "%"+cond.Operator+"%")
	}
	if cond.Command != "" {
		q += ` AND command LIKE ?`
		args = append(args, "%"+cond.Command+"%")
	}
	if cond.Category != "" {
		// 历史记录 category 为空表示命令执行，用 IFNULL 兼容
		q += ` AND IFNULL(NULLIF(category, ''), 'command') = ?`
		args = append(args, cond.Category)
	}
	if cond.Since > 0 {
		q += ` AND time >= ?`
		args = append(args, cond.Since)
	}
	if cond.Until > 0 {
		q += ` AND time <= ?`
		args = append(args, cond.Until)
	}
	return q, args
}

// CountAuditLogsFiltered 统计命中条件的审计日志总数（用于分页）。
func (db *DB) CountAuditLogsFiltered(cond AuditQueryCond) (int64, error) {
	where, args := buildAuditWhere(cond)
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_logs`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计审计日志失败: %w", err)
	}
	return n, nil
}

// SearchAuditLogs 分页查询审计日志：按时间倒序返回 [offset, offset+limit) 区间。
func (db *DB) SearchAuditLogs(cond AuditQueryCond, offset, limit int) ([]*AuditLogRow, error) {
	where, args := buildAuditWhere(cond)
	q := `SELECT id, time, operator, operator_ip, agent_id, command_id, command, result, exit_code, blocked, block_reason, duration_ms, category
		FROM audit_logs` + where + ` ORDER BY time DESC, id DESC`
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	q += ` LIMIT ?`
	args = append(args, limit)
	if offset > 0 {
		q += ` OFFSET ?`
		args = append(args, offset)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer rows.Close()
	var out []*AuditLogRow
	for rows.Next() {
		var r AuditLogRow
		var blocked int
		if err := rows.Scan(&r.ID, &r.Time, &r.Operator, &r.OperatorIP, &r.AgentID, &r.CommandID, &r.Command, &r.Result, &r.ExitCode, &blocked, &r.BlockReason, &r.Duration, &r.Category); err != nil {
			return nil, err
		}
		r.Blocked = blocked == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CountAuditLogsTotal 返回审计日志总行数（含全部类别）。
func (db *DB) CountAuditLogsTotal() (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计审计日志失败: %w", err)
	}
	return n, nil
}

// CleanupOldAuditLogs 清理超过指定天数的审计日志（保留审计留存策略，防止无界增长）。
func (db *DB) CleanupOldAuditLogs(maxDays int) (int64, error) {
	if maxDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -maxDays).Unix()
	res, err := db.Exec(`DELETE FROM audit_logs WHERE time < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理审计日志失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// QueryProbeHistorySince 按时间范围+聚合步长查询探针历史（分桶降采样）。
// step > 1 时按 timestamp/step 分桶取平均，避免长时间范围返回数万个采样点。
// 返回结果按时间升序。
func (db *DB) QueryProbeHistorySince(agentID string, since int64, limit, step int) ([]*ProbeHistoryRow, error) {
	if agentID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	if step < 1 {
		step = 1
	}
	var rows *sql.Rows
	var err error
	if step == 1 {
		rows, err = db.Query(`SELECT agent_id, timestamp, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes,
			os_type, kernel_version, uptime, public_ip, os_version, cpu_model, cpu_mhz,
			mem_total, mem_used, disk_total, disk_used, net_in_delta, net_out_delta,
			load1, load5, load15
			FROM probe_history WHERE agent_id = ? AND timestamp >= ?
			ORDER BY timestamp DESC LIMIT ?`, agentID, since, limit)
	} else {
		// 分桶降采样：桶内取平均指标，桶时间取最新采样时间
		rows, err = db.Query(`SELECT agent_id, MAX(timestamp) AS ts,
			AVG(cpu_usage), AVG(mem_usage), AVG(disk_usage), MAX(net_in_bytes), MAX(net_out_bytes),
			MAX(os_type), MAX(kernel_version), MAX(uptime), MAX(public_ip), MAX(os_version), MAX(cpu_model), AVG(cpu_mhz),
			MAX(mem_total), AVG(mem_used), MAX(disk_total), AVG(disk_used), SUM(net_in_delta), SUM(net_out_delta),
			AVG(load1), AVG(load5), AVG(load15)
			FROM probe_history WHERE agent_id = ? AND timestamp >= ?
			GROUP BY (timestamp / ?) ORDER BY ts DESC LIMIT ?`, agentID, since, step, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("查询探针历史失败: %w", err)
	}
	defer rows.Close()
	var out []*ProbeHistoryRow
	for rows.Next() {
		var r ProbeHistoryRow
		if err := rows.Scan(&r.AgentID, &r.Timestamp, &r.CPUUsage, &r.MemUsage, &r.DiskUsage, &r.NetInBytes, &r.NetOutBytes,
			&r.OSType, &r.KernelVersion, &r.Uptime, &r.PublicIP, &r.OSVersion, &r.CPUModel, &r.CPUMHz,
			&r.MemTotal, &r.MemUsed, &r.DiskTotal, &r.DiskUsed, &r.NetInDelta, &r.NetOutDelta,
			&r.Load1, &r.Load5, &r.Load15); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 转为时间升序，便于前端绘图
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CountProbeHistory 统计某 Agent 在指定时间之后的采样条数（用于计算降采样步长）。
func (db *DB) CountProbeHistory(agentID string, since int64) (int64, error) {
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM probe_history WHERE agent_id = ? AND timestamp >= ?`,
		agentID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计探针历史失败: %w", err)
	}
	return n, nil
}

// ProbeNetTotals 统计某 Agent 在时间窗内的收发字节增量总和（公开页"历史窗口累计"口径）。
//
// 用 SUM(delta) 而不是 MAX(累计)-MIN(累计)：降采样/留存裁剪会让窗口内的
// 首尾累计值缺失，差值口径会低估；而增量在写入时已算好，求和与步长无关。
func (db *DB) ProbeNetTotals(agentID string, since int64) (int64, int64, error) {
	var in, out int64
	err := db.QueryRow(`SELECT COALESCE(SUM(net_in_delta), 0), COALESCE(SUM(net_out_delta), 0)
		FROM probe_history WHERE agent_id = ? AND timestamp >= ?`, agentID, since).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("统计窗口流量失败: %w", err)
	}
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	return in, out, nil
}

// ProbeHistoryRow 探针历史持久化行。
type ProbeHistoryRow struct {
	AgentID       string
	Timestamp     int64
	CPUUsage      float64
	MemUsage      float64
	DiskUsage     float64
	NetInBytes    int64
	NetOutBytes   int64
	OSType        string
	KernelVersion string
	Uptime        int64
	PublicIP      string
	OSVersion     string
	CPUModel      string
	CPUMHz        float64
	MemTotal      int64
	MemUsed       int64
	DiskTotal     int64
	DiskUsed      int64
	// NetInDelta / NetOutDelta 本次采样相对上次采样的收发字节增量。
	// 0 = 未知（计数器回绕、首点、或旧版记录），公开页趋势会跳过该点。
	NetInDelta  int64
	NetOutDelta int64
	// Load1/5/15 系统平均负载；全为 0 表示该平台不提供（Windows）或旧库记录。
	Load1  float64
	Load5  float64
	Load15 float64
}

// InsertProbeHistory 插入一条探针历史记录。
func (db *DB) InsertProbeHistory(p *ProbeHistoryRow) error {
	_, err := db.Exec(`INSERT INTO probe_history
		(agent_id, timestamp, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes,
		 os_type, kernel_version, uptime, public_ip, os_version, cpu_model, cpu_mhz,
		 mem_total, mem_used, disk_total, disk_used, net_in_delta, net_out_delta,
		 load1, load5, load15)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.AgentID, p.Timestamp, p.CPUUsage, p.MemUsage, p.DiskUsage, p.NetInBytes, p.NetOutBytes,
		p.OSType, p.KernelVersion, p.Uptime, p.PublicIP, p.OSVersion, p.CPUModel, p.CPUMHz,
		p.MemTotal, p.MemUsed, p.DiskTotal, p.DiskUsed, p.NetInDelta, p.NetOutDelta,
		p.Load1, p.Load5, p.Load15)
	if err != nil {
		return fmt.Errorf("插入探针历史失败: %w", err)
	}
	return nil
}

// QueryProbeHistory 查询指定 Agent 的探针历史（按时间范围）。
func (db *DB) QueryProbeHistory(agentID string, since int64, limit int) ([]*ProbeHistoryRow, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := db.Query(`SELECT agent_id, timestamp, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes,
		os_type, kernel_version, uptime, public_ip, os_version, cpu_model, cpu_mhz,
		mem_total, mem_used, disk_total, disk_used, net_in_delta, net_out_delta,
		load1, load5, load15
		FROM probe_history WHERE agent_id = ? AND timestamp >= ?
		ORDER BY timestamp ASC LIMIT ?`, agentID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("查询探针历史失败: %w", err)
	}
	defer rows.Close()
	var out []*ProbeHistoryRow
	for rows.Next() {
		var r ProbeHistoryRow
		if err := rows.Scan(&r.AgentID, &r.Timestamp, &r.CPUUsage, &r.MemUsage, &r.DiskUsage, &r.NetInBytes, &r.NetOutBytes,
			&r.OSType, &r.KernelVersion, &r.Uptime, &r.PublicIP, &r.OSVersion, &r.CPUModel, &r.CPUMHz,
			&r.MemTotal, &r.MemUsed, &r.DiskTotal, &r.DiskUsed, &r.NetInDelta, &r.NetOutDelta,
			&r.Load1, &r.Load5, &r.Load15); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CleanupOldProbeHistory 清理超过指定天数的探针历史数据。
func (db *DB) CleanupOldProbeHistory(maxDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -maxDays).Unix()
	res, err := db.Exec(`DELETE FROM probe_history WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理探针历史失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentRow 主机信息持久化行。
type AgentRow struct {
	AgentID       string
	UUID          string
	Hostname      string
	OSType        string
	Arch          string
	KernelVersion string
	Online        bool
	LastSeen      int64
	LastOfflineAt int64
	Uptime        int64
	CPUUsage      float64
	MemUsage      float64
	DiskUsage     float64
	NetInBytes    int64
	NetOutBytes   int64
	RemoteAddr    string
	PublicIP      string
	PublicIPv6    string
	OSVersion     string
	CPUModel      string
	CPUMHz        float64
	MemTotal      int64
	MemUsed       int64
	DiskTotal     int64
	DiskUsed      int64
	NetRateIn     int64
	NetRateOut    int64
	// Load1/5/15 系统平均负载；LoadKnown=false 表示该平台不提供负载指标
	Load1     float64
	Load5     float64
	Load15    float64
	LoadKnown bool
}

// UpsertAgent 插入或更新主机信息。
//
// 设备身份以**硬件 UUID** 为准：UUID 相同即同一台机器。若该 UUID 已存在但
// agent_id 不同（Agent 改配置/重装导致），则把原记录重命名为新 agent_id 并更新
// 指标，而不是插入第二条 —— 这是「一台机器一条记录」的落库保证。
//
// 迁移期间还需处理一种情况：老记录里 agent_id 相同但 UUID 曾为空/不同，
// 此时按 agent_id 定位到旧行并更新，同时把 UUID 补齐。
func (db *DB) UpsertAgent(a *AgentRow) error {
	online := 0
	if a.Online {
		online = 1
	}
	uuid := strings.ToUpper(strings.TrimSpace(a.UUID))

	// UUID 已知且已被别的 agent_id 占用：先删除那条冗余行（历史采样/端口映射在
	// 事务中改归属），保证唯一索引下不会冲突。
	if uuid != "" {
		if err := db.mergeAgentByUUID(a.AgentID, uuid); err != nil {
			return err
		}
	}
	loadKnown := 0
	if a.LoadKnown {
		loadKnown = 1
	}
	_, err := db.Exec(`INSERT INTO agents
		(agent_id, uuid, hostname, os_type, arch, kernel_version, online, last_seen, last_offline_at,
		 uptime, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes, remote_addr,
		 public_ip, public_ipv6, os_version, cpu_model, cpu_mhz, mem_total, mem_used,
		 disk_total, disk_used, net_rate_in, net_rate_out, load1, load5, load15, load_known, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			uuid = excluded.uuid, hostname = excluded.hostname, os_type = excluded.os_type,
			arch = excluded.arch, kernel_version = excluded.kernel_version, online = excluded.online,
			last_seen = excluded.last_seen, last_offline_at = excluded.last_offline_at,
			uptime = excluded.uptime, cpu_usage = excluded.cpu_usage, mem_usage = excluded.mem_usage,
			disk_usage = excluded.disk_usage, net_in_bytes = excluded.net_in_bytes,
			net_out_bytes = excluded.net_out_bytes, remote_addr = excluded.remote_addr,
			public_ip = excluded.public_ip, public_ipv6 = excluded.public_ipv6,
			os_version = excluded.os_version, cpu_model = excluded.cpu_model, cpu_mhz = excluded.cpu_mhz,
			mem_total = excluded.mem_total, mem_used = excluded.mem_used,
			disk_total = excluded.disk_total, disk_used = excluded.disk_used,
			net_rate_in = excluded.net_rate_in, net_rate_out = excluded.net_rate_out,
			load1 = excluded.load1, load5 = excluded.load5, load15 = excluded.load15,
			load_known = excluded.load_known,
			updated_at = excluded.updated_at`,
		a.AgentID, uuid, a.Hostname, a.OSType, a.Arch, a.KernelVersion, online, a.LastSeen, a.LastOfflineAt,
		a.Uptime, a.CPUUsage, a.MemUsage, a.DiskUsage, a.NetInBytes, a.NetOutBytes, a.RemoteAddr,
		a.PublicIP, a.PublicIPv6, a.OSVersion, a.CPUModel, a.CPUMHz, a.MemTotal, a.MemUsed,
		a.DiskTotal, a.DiskUsed, a.NetRateIn, a.NetRateOut,
		a.Load1, a.Load5, a.Load15, loadKnown, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("保存主机信息失败: %w", err)
	}
	return nil
}

// mergeAgentByUUID 在同一事务内把 UUID 已被其它 agent_id 占用的旧行合并掉：
// 历史采样与端口映射规则改归属到当前 agent_id，随后删除旧行。
func (db *DB) mergeAgentByUUID(agentID, uuid string) error {
	var oldID string
	err := db.QueryRow(`SELECT agent_id FROM agents WHERE uuid = ? AND agent_id <> ? LIMIT 1`,
		uuid, agentID).Scan(&oldID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询同 UUID 主机失败: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开始主机合并事务失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE probe_history SET agent_id = ? WHERE agent_id = ?`, agentID, oldID); err != nil {
		return fmt.Errorf("迁移探针历史归属失败: %w", err)
	}
	if _, err := tx.Exec(`UPDATE portmap_rules SET agent_id = ? WHERE agent_id = ?`, agentID, oldID); err != nil {
		return fmt.Errorf("迁移端口映射归属失败: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM agents WHERE agent_id = ?`, oldID); err != nil {
		return fmt.Errorf("删除冗余主机记录失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交主机合并事务失败: %w", err)
	}
	log.Printf("[storage] UUID %s 的 agent_id 由 %s 变更为 %s，已合并为一条记录", uuid, oldID, agentID)
	return nil
}

// LoadAgents 加载全部主机信息（从数据库）。
func (db *DB) LoadAgents() ([]*AgentRow, error) {
	rows, err := db.Query(`SELECT agent_id, uuid, hostname, os_type, arch, kernel_version, online, last_seen, last_offline_at,
		uptime, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes, remote_addr,
		public_ip, public_ipv6, os_version, cpu_model, cpu_mhz, mem_total, mem_used,
		disk_total, disk_used, net_rate_in, net_rate_out, load1, load5, load15, load_known
		FROM agents ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询主机信息失败: %w", err)
	}
	defer rows.Close()
	var out []*AgentRow
	for rows.Next() {
		var a AgentRow
		var online, loadKnown int
		if err := rows.Scan(&a.AgentID, &a.UUID, &a.Hostname, &a.OSType, &a.Arch, &a.KernelVersion, &online, &a.LastSeen, &a.LastOfflineAt,
			&a.Uptime, &a.CPUUsage, &a.MemUsage, &a.DiskUsage, &a.NetInBytes, &a.NetOutBytes, &a.RemoteAddr,
			&a.PublicIP, &a.PublicIPv6, &a.OSVersion, &a.CPUModel, &a.CPUMHz, &a.MemTotal, &a.MemUsed,
			&a.DiskTotal, &a.DiskUsed, &a.NetRateIn, &a.NetRateOut,
			&a.Load1, &a.Load5, &a.Load15, &loadKnown); err != nil {
			return nil, err
		}
		a.Online = online == 1
		a.LoadKnown = loadKnown == 1
		out = append(out, &a)
	}
	return out, rows.Err()
}

// UpdateAgentOnline 更新主机在线状态与时间戳。
//
// 只按 agent_id 精确定位设备记录：调用方传入的身份来自**连接身份**（已通过注册鉴权），
// 而 uuid 是 Agent 自报字段。
//
// 历史实现把参数同时当作 uuid 匹配条件（`OR uuid = ?`），而 MarkOffline 传的是
// agent_id。这会让 agent_id 恰好等于某个设备 UUID 时（例如默认把硬件 UUID 当
// agent_id 的旧版 Agent）把**别的机器**一起标记为离线，且 last_seen 被写坏，
// 表现为正确的机器"离线时长"数值荒谬。设备按 UUID 归并后，agent_id 在 agents
// 表中是唯一的，单键匹配已足够，不再需要 uuid 兜底。
func (db *DB) UpdateAgentOnline(agentID string, online bool, lastSeen int64) error {
	v := 0
	if online {
		v = 1
	}
	_, err := db.Exec(`UPDATE agents SET online = ?, last_seen = ?, updated_at = ?
		WHERE agent_id = ?`,
		v, lastSeen, time.Now().Unix(), agentID)
	if err != nil {
		return fmt.Errorf("更新主机在线状态失败: %w", err)
	}
	return nil
}

// UpdateAgentIdentity 把某设备的 agent_id 改名为新值并补齐 UUID（幂等）。
// 设备身份由 UUID 决定，因此改名只影响展示/路由用的别名，不新增记录。
func (db *DB) UpdateAgentIdentity(oldID, newID, uuid string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	if _, err := db.Exec(`UPDATE agents SET agent_id = ?, uuid = ?, updated_at = ?
		WHERE agent_id = ?`, newID, strings.ToUpper(strings.TrimSpace(uuid)), time.Now().Unix(), oldID); err != nil {
		return fmt.Errorf("更新主机 agent_id 失败: %w", err)
	}
	// 别名改名的同时把历史采样与端口映射一起迁移，避免趋势图断档
	if _, err := db.Exec(`UPDATE probe_history SET agent_id = ? WHERE agent_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("迁移探针历史归属失败: %w", err)
	}
	if _, err := db.Exec(`UPDATE portmap_rules SET agent_id = ? WHERE agent_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("迁移端口映射归属失败: %w", err)
	}
	return nil
}

// LoadAgent 加载单个主机信息。
func (db *DB) LoadAgent(agentID string) (*AgentRow, error) {
	var a AgentRow
	var online, loadKnown int
	err := db.QueryRow(`SELECT agent_id, uuid, hostname, os_type, arch, kernel_version, online, last_seen, last_offline_at,
		uptime, cpu_usage, mem_usage, disk_usage, net_in_bytes, net_out_bytes, remote_addr,
		public_ip, public_ipv6, os_version, cpu_model, cpu_mhz, mem_total, mem_used,
		disk_total, disk_used, net_rate_in, net_rate_out, load1, load5, load15, load_known
		FROM agents WHERE agent_id = ?`, agentID).Scan(
		&a.AgentID, &a.UUID, &a.Hostname, &a.OSType, &a.Arch, &a.KernelVersion, &online, &a.LastSeen, &a.LastOfflineAt,
		&a.Uptime, &a.CPUUsage, &a.MemUsage, &a.DiskUsage, &a.NetInBytes, &a.NetOutBytes, &a.RemoteAddr,
		&a.PublicIP, &a.PublicIPv6, &a.OSVersion, &a.CPUModel, &a.CPUMHz, &a.MemTotal, &a.MemUsed,
		&a.DiskTotal, &a.DiskUsed, &a.NetRateIn, &a.NetRateOut,
		&a.Load1, &a.Load5, &a.Load15, &loadKnown)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询主机信息失败: %w", err)
	}
	a.Online = online == 1
	a.LoadKnown = loadKnown == 1
	return &a, nil
}

// BlacklistRow 命令黑名单持久化行。
type BlacklistRow struct {
	ID      int64
	Pattern string
	Builtin bool
}

// InsertBlacklist 插入一条黑名单规则。
func (db *DB) InsertBlacklist(pattern string, builtin bool) error {
	b := 0
	if builtin {
		b = 1
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO blacklist (pattern, builtin) VALUES (?, ?)`, pattern, b)
	if err != nil {
		return fmt.Errorf("插入黑名单规则失败: %w", err)
	}
	return nil
}

// DeleteBlacklist 删除一条黑名单规则（不允许删除内置规则）。
func (db *DB) DeleteBlacklist(pattern string) error {
	_, err := db.Exec(`DELETE FROM blacklist WHERE pattern = ? AND builtin = 0`, pattern)
	if err != nil {
		return fmt.Errorf("删除黑名单规则失败: %w", err)
	}
	return nil
}

// LoadBlacklist 加载全部黑名单规则。
func (db *DB) LoadBlacklist() ([]*BlacklistRow, error) {
	rows, err := db.Query(`SELECT id, pattern, builtin FROM blacklist ORDER BY builtin DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询黑名单规则失败: %w", err)
	}
	defer rows.Close()
	var out []*BlacklistRow
	for rows.Next() {
		var r BlacklistRow
		var builtin int
		if err := rows.Scan(&r.ID, &r.Pattern, &builtin); err != nil {
			return nil, err
		}
		r.Builtin = builtin == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// InitDefaultBlacklist 初始化默认（内置）黑名单规则（幂等）。
func (db *DB) InitDefaultBlacklist(patterns []string) error {
	for _, p := range patterns {
		if err := db.InsertBlacklist(p, true); err != nil {
			return err
		}
	}
	return nil
}
