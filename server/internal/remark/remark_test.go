package remark

import "testing"

// TestRename 回归：agent_id 变更（同一台机器改配置）时备注应跟随设备迁移，
// 不能让用户"备注莫名消失"。
func TestRename(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Set("old-id", "生产库A"); err != nil {
		t.Fatalf("设置备注失败: %v", err)
	}
	if err := s.Rename("old-id", "new-id"); err != nil {
		t.Fatalf("迁移备注失败: %v", err)
	}
	if got := s.Get("new-id"); got != "生产库A" {
		t.Fatalf("备注应迁移到新 agent_id，实得 %q", got)
	}
	if got := s.Get("old-id"); got != "" {
		t.Fatalf("旧 agent_id 不应残留备注，实得 %q", got)
	}
	if err := s.Rename("", "x"); err != nil {
		t.Fatalf("空参数应安全返回: %v", err)
	}
	if err := s.Rename("x", "x"); err != nil {
		t.Fatalf("同名应安全返回: %v", err)
	}
}
