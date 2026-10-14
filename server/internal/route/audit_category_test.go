package route

import (
	"path/filepath"
	"testing"
	"time"

	"mocenter/server/internal/session"
	"mocenter/server/internal/storage"
)

// TestAuditCategorySurvivesSQLite 钉住"审计类别"这条路：写进库的 category 必须还在。
//
// 为什么单独立一条：Category 只在**内存回退路径**被拷贝过（SearchAuditsPage 的
// categoryOrDefault 分支），落 SQLite 的行结构漏抄该字段后，InsertAuditLog 会把空值
// 补成 'command'。于是注册面拒绝（auth）与运维操作（op）在库里全部显示成命令执行，
// `/api/audits?category=auth` 一条也查不到 —— 单看内存回退路径的旧用例发现不了，
// 真机复测 H-N1 时按分类统计才发现 22 条拒签记录都挂着 command。
func TestAuditCategorySurvivesSQLite(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "audit-category.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	defer db.Close()

	r := NewRouter(session.NewManager(), db, "")
	defer r.Close()

	want := map[string]string{"auth-host": "auth", "op-host": "op", "cmd-host": "command"}
	for id, cat := range want {
		r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: "tester", AgentID: id,
			Command: "c-" + cat, Result: "r", Category: cat})
	}
	// 落库是异步的（Audit 里 go InsertAuditLog，避免阻塞命令转发），轮询而不是睡死。
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := db.CountAuditLogs()
		if err != nil {
			t.Fatalf("统计失败: %v", err)
		}
		if n == int64(len(want)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待落库超时，实得 %d 条", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for id, cat := range want {
		page, err := r.SearchAuditsPage(AuditQuery{Agent: id, Category: cat})
		if err != nil {
			t.Fatalf("按 %s 检索 %s 失败: %v", cat, id, err)
		}
		if page.Total != 1 {
			t.Errorf("agent=%s 用 category=%s 检索命中 %d 条，期望 1 条（类别在落库时丢了）", id, cat, page.Total)
			continue
		}
		if got := page.Items[0].Category; got != cat {
			t.Errorf("agent=%s 回读的类别是 %q，期望 %q", id, got, cat)
		}
	}
	// 反向：错类别必须查不到，否则"按分类过滤"只是摆设。
	if page, err := r.SearchAuditsPage(AuditQuery{Agent: "auth-host", Category: "command"}); err != nil || page.Total != 0 {
		t.Errorf("auth 记录不应命中 command 分类，实得 Total=%d err=%v", page.Total, err)
	}
}
