package probe

import (
	"path/filepath"
	"testing"
	"time"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/storage"
)

// TestStoreRestoresMergedDevices 集成回归：老库中同一 UUID 的多条主机记录，
// 在 Store 启动加载时应归并为一台，历史采样随之继承。
func TestStoreRestoresMergedDevices(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "mocenter.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// 模拟旧版行为：同一台机器因 agent_id 变更留下两条记录
	if err := db.UpsertAgent(&storage.AgentRow{AgentID: "old-id", UUID: "HW-1", Hostname: "h", LastSeen: 100}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertProbeHistory(&storage.ProbeHistoryRow{AgentID: "old-id", Timestamp: 100, CPUUsage: 3}); err != nil {
		t.Fatal(err)
	}

	// 再用同一 UUID、新 agent_id 写入（正式实现会合并，这里直接插一条旧行）
	if err := db.UpsertAgent(&storage.AgentRow{AgentID: "new-id", UUID: "HW-1", Hostname: "h", LastSeen: 200}); err != nil {
		t.Fatal(err)
	}

	s := NewStore(db)
	if got := len(s.All()); got != 1 {
		t.Fatalf("同 UUID 应恢复为一台机器，实得 %d", got)
	}
	a := s.Get("new-id")
	if a == nil || a.UUID != "HW-1" {
		t.Fatalf("应按新 agent_id 查到设备，实得 %+v", a)
	}
	// 历史采样应能查到（DB 层已迁移归属）
	h := s.History("new-id", time.Unix(0, 0))
	if len(h) != 1 {
		t.Fatalf("历史应迁移到 new-id，实得 %d 条", len(h))
	}
}

// TestDeleteOfflineRemovesRecordAndHistory 删除离线主机后，内存与数据库里都不应
// 再找到它；其它主机与其历史不受影响。
func TestDeleteOfflineRemovesRecordAndHistory(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "mocenter.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	for _, id := range []string{"keep-id", "del-id"} {
		if err := db.UpsertAgent(&storage.AgentRow{AgentID: id, UUID: "uuid-" + id, Hostname: id, LastSeen: 100}); err != nil {
			t.Fatalf("写入主机 %s 失败: %v", id, err)
		}
		if err := db.InsertProbeHistory(&storage.ProbeHistoryRow{AgentID: id, Timestamp: 100, CPUUsage: 1}); err != nil {
			t.Fatalf("写入探针历史 %s 失败: %v", id, err)
		}
	}

	s := NewStore(db)
	info, aliases, err := s.DeleteOffline("del-id")
	if err != nil {
		t.Fatalf("删除离线主机失败: %v", err)
	}
	if info == nil || info.AgentID != "del-id" {
		t.Fatalf("返回的被删主机不正确: %+v", info)
	}
	if len(aliases) != 1 || aliases[0] != "del-id" {
		t.Fatalf("别名列表应只含 del-id，实得 %v", aliases)
	}
	if s.Get("del-id") != nil {
		t.Fatal("内存中仍能查到已删除的主机")
	}
	if got := len(s.All()); got != 1 {
		t.Fatalf("应只剩一台主机，实得 %d", got)
	}
	rows, err := db.LoadAgents()
	if err != nil {
		t.Fatalf("读取主机失败: %v", err)
	}
	if len(rows) != 1 || rows[0].AgentID != "keep-id" {
		t.Fatalf("数据库应只剩 keep-id，实得 %+v", rows)
	}
	if h, _ := db.QueryProbeHistory("del-id", 0, 10); len(h) != 0 {
		t.Fatalf("被删主机的探针历史应清除，实得 %d 条", len(h))
	}
	if h, _ := db.QueryProbeHistory("keep-id", 0, 10); len(h) != 1 {
		t.Fatalf("其它主机的探针历史不应受影响，实得 %d 条", len(h))
	}
}

// TestDeleteOfflineRejectsOnline 在线主机不允许删除（否则下一次探针上报会把它
// 重新写回来，界面出现"删了又回来"的假动作）。
func TestDeleteOfflineRejectsOnline(t *testing.T) {
	s := NewStore(nil)
	s.UpsertAgent(&protocol.AgentInfo{AgentID: "on-id", UUID: "uuid-on", Online: true})
	if _, _, err := s.DeleteOffline("on-id"); err == nil {
		t.Fatal("在线主机应拒绝删除")
	}
	if s.Get("on-id") == nil {
		t.Fatal("拒绝删除时不应把主机从内存中移除")
	}

	s.UpsertAgent(&protocol.AgentInfo{AgentID: "on-id", UUID: "uuid-on", Online: false})
	if _, _, err := s.DeleteOffline("on-id"); err != nil {
		t.Fatalf("离线后应允许删除: %v", err)
	}
	if s.Get("on-id") != nil {
		t.Fatal("删除后内存中仍能查到主机")
	}
}
