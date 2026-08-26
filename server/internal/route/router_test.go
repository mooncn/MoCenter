package route

import (
	"testing"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

func TestIsBlocked(t *testing.T) {
	r := NewRouter(nil)
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
	r := NewRouter(session.NewManager())
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
	r := NewRouter(session.NewManager())
	ok, _ := r.Dispatch(&protocol.CommandReq{AgentID: "a1", Command: "rm -rf /"}, "u", "1.1.1.1")
	if ok {
		t.Fatal("blacklisted command should be rejected")
	}
	logs := r.Audits()
	if len(logs) == 0 || !logs[0].Blocked {
		t.Fatal("blocked command should be audited with Blocked=true")
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
	r := NewRouter(session.NewManager())
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