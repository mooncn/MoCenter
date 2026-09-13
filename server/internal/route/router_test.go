package route

import (
	"os"
	"path/filepath"
	"testing"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
	"mocenter/server/internal/storage"
)

func TestIsBlocked(t *testing.T) {
	r := NewRouter(nil, nil)
	cases := map[string]bool{
		"rm -rf /":      true,
		"mkfs /dev/sdb": true,
		"sudo rm":       true,
		"chmod 777 x":   true,
		"uptime":        false,
		"echo hello":    false,
		"ls -la":        false,
	}
	for cmd, want := range cases {
		got, _ := r.IsBlocked(cmd)
		if got != want {
			t.Errorf("IsBlocked(%q)=%v, want %v", cmd, got, want)
		}
	}
}

func TestDispatchOffline(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	ok, msg := r.Dispatch(&protocol.CommandReq{AgentID: "ghost", Command: "echo hi"}, "u", "1.1.1.1")
	if ok {
		t.Fatal("dispatch to offline agent should fail")
	}
	if msg == "" {
		t.Fatal("offline dispatch should return a message")
	}
	// 离线也应留下审计记录
	if len(r.Audits()) != 1 {
		t.Fatalf("expected 1 audit log, got %d", len(r.Audits()))
	}
}

func TestBlacklistBlocksAndAudits(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	ok, _ := r.Dispatch(&protocol.CommandReq{AgentID: "a1", Command: "rm -rf /"}, "u", "1.1.1.1")
	if ok {
		t.Fatal("blacklisted command should be rejected")
	}
	logs := r.Audits()
	if len(logs) == 0 || !logs[0].Blocked {
		t.Fatal("blocked command should be audited with Blocked=true")
	}
	// 审计应保留操作人，不得为空
	if logs[0].Operator != "u" {
		t.Fatalf("expected operator u, got %q", logs[0].Operator)
	}
}

func TestMaskCommand(t *testing.T) {
	cases := map[string]string{
		"mysql -u root -p secret123":  "mysql -u root -p ***",
		"export TOKEN=abc123":         "export TOKEN=***",
		"curl -H 'Authorization: xyz'": "curl -H 'Authorization: ***",
		"echo hello world":            "echo hello world",
	}
	for c, want := range cases {
		if got := MaskCommand(c); got != want {
			t.Errorf("MaskCommand(%q)=%q, want %q", c, got, want)
		}
	}
}

func TestSearchAudits(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	r.Audit(&AuditLog{Time: 100, AgentID: "a1", Operator: "alice", Command: "uptime"})
	r.Audit(&AuditLog{Time: 200, AgentID: "a2", Operator: "bob", Command: "ls -la"})
	r.Audit(&AuditLog{Time: 300, AgentID: "a1", Operator: "alice", Command: "df -h"})

	// 按 agent 检索
	r1 := r.SearchAudits(AuditQuery{Agent: "a1"})
	if len(r1) != 2 {
		t.Fatalf("agent filter: expected 2, got %d", len(r1))
	}
	// 按命令关键字检索
	r2 := r.SearchAudits(AuditQuery{Command: "ls"})
	if len(r2) != 1 || r2[0].AgentID != "a2" {
		t.Fatalf("command filter failed: %d results", len(r2))
	}
	// 按时间范围
	r3 := r.SearchAudits(AuditQuery{Since: 150, Until: 250})
	if len(r3) != 1 {
		t.Fatalf("time range filter: expected 1, got %d", len(r3))
	}
}

// TestCompleteByCommand 验证命令结果回填：不新增记录，只补齐结果字段。
func TestCompleteByCommand(t *testing.T) {
	sess := session.NewManager()
	r := NewRouter(sess, nil)
	const cmdID = "cmd-1"
	// 模拟 Dispatch 已落审计（带 command_id）
	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", CommandID: cmdID, Command: "uptime"})

	r.CompleteByCommand(cmdID, "a1", "ok-output", 0, 42)

	logs := r.Audits()
	if len(logs) != 1 {
		t.Fatalf("回填不应新增审计记录，期望 1 条，实得 %d", len(logs))
	}
	if logs[0].Result != "ok-output" || logs[0].Duration != 42 {
		t.Fatalf("结果回填失败: result=%q duration=%d", logs[0].Result, logs[0].Duration)
	}
	if logs[0].Operator != "alice" {
		t.Fatalf("回填不应丢失操作人，实得 %q", logs[0].Operator)
	}
	// 回填后映射应被清理
	if r.auditIDFor(cmdID) != 0 {
		t.Fatal("回填后 command_id 映射应被清理")
	}
}

// TestImportLegacyAudits 验证历史 audits.jsonl 能被补录进数据库，且重复启动不产生重复记录。
// 背景：早期版本只写 JSONL 不写 DB，导致老记录在审计页面不可见。
func TestImportLegacyAudits(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	defer db.Close()

	// 构造一个「旧版」JSONL：字段不含 command_id
	legacy := `{"time":1700000001,"operator":"alice","operator_ip":"1.1.1.1","agent_id":"host-a","command":"whoami","result":"root","exit_code":0,"blocked":false,"block_reason":"","duration_ms":12}
{"time":1700000002,"operator":"bob","operator_ip":"2.2.2.2","agent_id":"host-b","command":"uptime","result":"","exit_code":0,"blocked":true,"block_reason":"sudo","duration_ms":0}
`
	jsonlPath := filepath.Join(t.TempDir(), "audits.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(legacy), 0644); err != nil {
		t.Fatalf("写入测试 JSONL 失败: %v", err)
	}

	// 首次创建：应导入 2 条
	r := NewRouter(session.NewManager(), db, jsonlPath)
	defer r.Close()
	n, err := db.CountAuditLogs()
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("期望导入 2 条历史审计，实得 %d", n)
	}

	// 导入后应能从数据库查询到（即审计页面可见）
	found := r.SearchAudits(AuditQuery{})
	if len(found) != 2 {
		t.Fatalf("查询应返回 2 条，实得 %d", len(found))
	}

	// 再次创建（模拟重启）：去重，不应重复导入
	r2 := NewRouter(session.NewManager(), db, jsonlPath)
	_ = r2.Close()
	n2, _ := db.CountAuditLogs()
	if n2 != 2 {
		t.Fatalf("重复启动不应重复导入，期望 2 条，实得 %d", n2)
	}

	if len(found) >= 1 {
		// 历史记录的字段应被完整保留
		var ok bool
		for _, l := range found {
			if l.Operator == "alice" && l.AgentID == "host-a" && l.Result == "root" && l.Duration == 12 {
				ok = true
			}
		}
		if !ok {
			t.Fatal("导入的历史记录字段不完整")
		}
	}
}

// TestImportLegacyAuditsMissingFile 文件不存在时不应报错或 panic。
func TestImportLegacyAuditsMissingFile(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	defer db.Close()
	r := NewRouter(session.NewManager(), db, filepath.Join(t.TempDir(), "nope.jsonl"))
	_ = r.Close()
	n, _ := db.CountAuditLogs()
	if n != 0 {
		t.Fatalf("无文件时不应导入任何记录，实得 %d", n)
	}
}