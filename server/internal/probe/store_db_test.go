package probe

import (
	"path/filepath"
	"testing"
	"time"

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
