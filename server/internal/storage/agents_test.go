package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestUpsertAgentMergesByUUID 回归：同一台机器（同 UUID）改了 agent_id 后，
// agents 表只应保留一条记录，且历史采样与端口映射跟随迁移。
func TestUpsertAgentMergesByUUID(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertAgent(&AgentRow{AgentID: "old-id", UUID: "uuid-1", Hostname: "h1", LastSeen: 100}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if err := db.InsertProbeHistory(&ProbeHistoryRow{AgentID: "old-id", Timestamp: 100, CPUUsage: 1}); err != nil {
		t.Fatalf("写入历史失败: %v", err)
	}
	if err := db.SavePortmapRule(&PortmapRuleRow{PublicPort: 8080, ID: "r1", AgentID: "old-id", LocalHost: "127.0.0.1", LocalPort: 80, Enabled: true}); err != nil {
		t.Fatalf("写入端口映射失败: %v", err)
	}

	// 同一台机器改了 agent_id
	if err := db.UpsertAgent(&AgentRow{AgentID: "new-id", UUID: "uuid-1", Hostname: "h1", LastSeen: 200}); err != nil {
		t.Fatalf("改名写入失败: %v", err)
	}

	rows, err := db.LoadAgents()
	if err != nil {
		t.Fatalf("读取主机失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("同 UUID 应只有一条记录，实得 %d", len(rows))
	}
	if rows[0].AgentID != "new-id" {
		t.Fatalf("应保留最新 agent_id，实得 %q", rows[0].AgentID)
	}
	// 历史采样归属已迁移
	h, err := db.QueryProbeHistory("new-id", 0, 10)
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("历史应迁移到新 agent_id，实得 %d 条", len(h))
	}
	// 端口映射归属已迁移
	rules, err := db.LoadPortmapRules()
	if err != nil {
		t.Fatalf("查询端口映射失败: %v", err)
	}
	if len(rules) != 1 || rules[0].AgentID != "new-id" {
		t.Fatalf("端口映射应迁移到新 agent_id，实得 %+v", rules)
	}
}

// TestDeleteAgentData 删除主机时只清目标设备的记录与历史，别名一并清理，
// 其它主机不受影响。
func TestDeleteAgentData(t *testing.T) {
	db := openTestDB(t)
	for _, id := range []string{"keep-id", "del-id"} {
		if err := db.UpsertAgent(&AgentRow{AgentID: id, UUID: "uuid-" + id, Hostname: id, LastSeen: 100}); err != nil {
			t.Fatalf("写入主机 %s 失败: %v", id, err)
		}
		if err := db.InsertProbeHistory(&ProbeHistoryRow{AgentID: id, Timestamp: 100, CPUUsage: 1}); err != nil {
			t.Fatalf("写入探针历史 %s 失败: %v", id, err)
		}
	}
	if err := db.InsertLatencyHistory([]*LatencyRow{
		{AgentID: "keep-id", Timestamp: 100, Target: "icmp:1.1.1.1", LatencyMS: 5, OK: true},
		{AgentID: "del-id", Timestamp: 100, Target: "icmp:1.1.1.1", LatencyMS: 5, OK: true},
	}); err != nil {
		t.Fatalf("写入延迟历史失败: %v", err)
	}

	if err := db.DeleteAgentData([]string{"del-id", "old-del-id"}); err != nil {
		t.Fatalf("删除主机失败: %v", err)
	}

	rows, err := db.LoadAgents()
	if err != nil {
		t.Fatalf("读取主机失败: %v", err)
	}
	if len(rows) != 1 || rows[0].AgentID != "keep-id" {
		t.Fatalf("只应保留 keep-id，实得 %+v", rows)
	}
	if h, _ := db.QueryProbeHistory("del-id", 0, 10); len(h) != 0 {
		t.Fatalf("目标主机的探针历史应被清除，实得 %d 条", len(h))
	}
	if h, _ := db.QueryProbeHistory("keep-id", 0, 10); len(h) != 1 {
		t.Fatalf("其它主机的探针历史不应受影响，实得 %d 条", len(h))
	}
	if h, _ := db.QueryLatencyHistory("del-id", 0, "", 10); len(h) != 0 {
		t.Fatalf("目标主机的延迟历史应被清除，实得 %d 条", len(h))
	}
	if h, _ := db.QueryLatencyHistory("keep-id", 0, "", 10); len(h) != 1 {
		t.Fatalf("其它主机的延迟历史不应受影响，实得 %d 条", len(h))
	}
}

// TestDedupOnMigration 回归：老库中同一 UUID 的重复行应在启动迁移时被合并，
// 且不因唯一索引创建失败导致服务端起不来。
func TestDedupOnMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// 先用「无唯一索引」的旧结构造重复数据：直接手写一张表
	sqlDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开原始库失败: %v", err)
	}
	if _, err := sqlDB.Exec(`CREATE TABLE agents (
		agent_id TEXT PRIMARY KEY, uuid TEXT NOT NULL DEFAULT '', hostname TEXT NOT NULL DEFAULT '',
		os_type TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '', kernel_version TEXT NOT NULL DEFAULT '',
		online INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, last_offline_at INTEGER NOT NULL DEFAULT 0,
		uptime INTEGER NOT NULL DEFAULT 0, cpu_usage REAL NOT NULL DEFAULT 0, mem_usage REAL NOT NULL DEFAULT 0,
		disk_usage REAL NOT NULL DEFAULT 0, net_in_bytes INTEGER NOT NULL DEFAULT 0, net_out_bytes INTEGER NOT NULL DEFAULT 0,
		remote_addr TEXT NOT NULL DEFAULT '', public_ip TEXT NOT NULL DEFAULT '', public_ipv6 TEXT NOT NULL DEFAULT '',
		os_version TEXT NOT NULL DEFAULT '', cpu_model TEXT NOT NULL DEFAULT '', cpu_mhz REAL NOT NULL DEFAULT 0,
		mem_total INTEGER NOT NULL DEFAULT 0, mem_used INTEGER NOT NULL DEFAULT 0, disk_total INTEGER NOT NULL DEFAULT 0,
		disk_used INTEGER NOT NULL DEFAULT 0, net_rate_in INTEGER NOT NULL DEFAULT 0, net_rate_out INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	for _, r := range []struct {
		id string
		ts int64
	}{{"dup-a", 100}, {"dup-b", 300}} {
		if _, err := sqlDB.Exec(`INSERT INTO agents (agent_id, uuid, hostname, last_seen) VALUES (?,?,?,?)`,
			r.id, "SAME-UUID", "h1", r.ts); err != nil {
			t.Fatalf("插入重复数据失败: %v", err)
		}
	}
	// 探针历史里也要有旧 agent_id 的数据
	if _, err := sqlDB.Exec(`CREATE TABLE probe_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id TEXT NOT NULL, timestamp INTEGER NOT NULL,
		cpu_usage REAL NOT NULL DEFAULT 0, mem_usage REAL NOT NULL DEFAULT 0, disk_usage REAL NOT NULL DEFAULT 0,
		net_in_bytes INTEGER NOT NULL DEFAULT 0, net_out_bytes INTEGER NOT NULL DEFAULT 0, os_type TEXT NOT NULL DEFAULT '',
		kernel_version TEXT NOT NULL DEFAULT '', uptime INTEGER NOT NULL DEFAULT 0, public_ip TEXT NOT NULL DEFAULT '',
		os_version TEXT NOT NULL DEFAULT '', cpu_model TEXT NOT NULL DEFAULT '', cpu_mhz REAL NOT NULL DEFAULT 0,
		mem_total INTEGER NOT NULL DEFAULT 0, mem_used INTEGER NOT NULL DEFAULT 0, disk_total INTEGER NOT NULL DEFAULT 0,
		disk_used INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("建探针历史表失败: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO probe_history (agent_id, timestamp) VALUES ('dup-a', 100)`); err != nil {
		t.Fatalf("插入历史失败: %v", err)
	}
	sqlDB.Close()

	// 用正式 Open 触发迁移：应自动去重并建唯一索引
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("迁移后打开数据库失败: %v", err)
	}
	defer db2.Close()

	rows, err := db2.LoadAgents()
	if err != nil {
		t.Fatalf("读取主机失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("重复 UUID 应被合并为一条，实得 %d", len(rows))
	}
	if rows[0].AgentID != "dup-b" {
		t.Fatalf("应保留 last_seen 最新的记录 dup-b，实得 %q", rows[0].AgentID)
	}
	h, err := db2.QueryProbeHistory("dup-b", 0, 10)
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	if len(h) != 1 {
		t.Fatalf("历史应迁移到 dup-b，实得 %d 条", len(h))
	}
}

// TestUpdateAgentOnlineDoesNotTouchOtherDevices 安全回归：
// UpdateAgentOnline 只应按 agent_id 精确匹配。
//
// 原实现把入参同时当作 uuid 匹配条件（`OR uuid = ?`），而 MarkOffline 传的是
// agent_id。当某个 agent_id 恰好等于另一台设备的 UUID 时（旧版 Agent 默认把
// 硬件 UUID 当 agent_id，很常见），会把**别的机器**一并标离线，且它的
// last_seen 被写成当前时间，前端展示出荒谬的"离线时长"。
func TestUpdateAgentOnlineDoesNotTouchOtherDevices(t *testing.T) {
	db := openTestDB(t)

	// 设备 A：agent_id 恰好是设备 B 的 UUID（旧版 Agent 的典型取值）
	if err := db.UpsertAgent(&AgentRow{AgentID: "UUID-B", UUID: "UUID-A", Hostname: "a", Online: true, LastSeen: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAgent(&AgentRow{AgentID: "host-b", UUID: "UUID-B", Hostname: "b", Online: true, LastSeen: 2000}); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateAgentOnline("UUID-B", false, 3000); err != nil {
		t.Fatalf("更新离线状态失败: %v", err)
	}

	a, err := db.LoadAgent("UUID-B")
	if err != nil || a == nil {
		t.Fatalf("读取设备 A 失败: %v", err)
	}
	if a.Online {
		t.Fatal("设备 A 应被标记离线")
	}
	b, err := db.LoadAgent("host-b")
	if err != nil || b == nil {
		t.Fatalf("读取设备 B 失败: %v", err)
	}
	if !b.Online {
		t.Fatal("设备 B 是另一台机器，不应被连带标记离线")
	}
	if b.LastSeen != 2000 {
		t.Fatalf("设备 B 的 last_seen 不应被改写，实得 %d", b.LastSeen)
	}
}

// TestDedupLeavesSingleUUIDlessRows 回归：UUID 为空的行无法判定是否同机，
// 迁移去重必须保持原样，否则会把不同机器的历史/备注合并。
func TestDedupLeavesSingleUUIDlessRows(t *testing.T) {
	db := openTestDB(t)
	for _, id := range []string{"legacy-1", "legacy-2"} {
		if err := db.UpsertAgent(&AgentRow{AgentID: id, UUID: "", Hostname: id, LastSeen: 100}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.LoadAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("无 UUID 的行不应被合并，实得 %d 条", len(rows))
	}
}
