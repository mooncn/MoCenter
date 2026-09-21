package route

// M-2：审计的**结果**也必须在写入时脱敏。
//
// Audit() 一直会对 Command/Result 的初值做 MaskCommand，但命令结果是事后回填的
// （Complete / CompleteByCommand），回填走的是裸赋值 + 直接落库，等于把
// `ps aux`、打印出来的 Token 永久留在审计库里。因此回填路径必须自己再过一遍脱敏，
// 而不是指望调用方或读取侧兜底。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/session"
	"mocenter/server/internal/storage"
)

const secretNeedle = "SuperSecret99"

// waitUntil 轮询 cond 直到为真，超时返回 error。
//
// 审计落库与结果回填都在协程里做（不阻塞命令转发），所以断言库内内容必须等。
func waitUntil(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &timeoutError{timeout}
}

type timeoutError struct{ d time.Duration }

func (e *timeoutError) Error() string { return "等待超时（" + e.d.String() + "）" }

// sensitiveOutput 一段典型的命令输出：本身正常，中间夹着凭据。
func sensitiveOutput() string {
	return "已连接\nmy.cnf: password = " + secretNeedle + "\n--passwd " + secretNeedle + " 已写入"
}

// expectMasked 断言结果里既没有明文、也不是"整段被丢掉"。
func expectMasked(t *testing.T, got string) {
	t.Helper()
	if strings.Contains(got, secretNeedle) {
		t.Fatalf("结果未脱敏，明文仍在审计里: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("结果里没有脱敏标记（说明被整体丢弃而非脱敏）: %q", got)
	}
	if !strings.Contains(got, "已连接") {
		t.Fatalf("脱敏不应吃掉正常输出: %q", got)
	}
}

// TestAuditResultMaskedByComplete 按 ID 回填的结果必须脱敏。
func TestAuditResultMaskedByComplete(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", CommandID: "c1", Command: "uptime"})
	r.Complete(0, "a1", sensitiveOutput(), 0, 12)

	logs := r.Audits()
	if len(logs) != 1 {
		t.Fatalf("期望 1 条审计，实得 %d", len(logs))
	}
	expectMasked(t, logs[0].Result)
}

// TestAuditResultMaskedByCompleteByCommand 按 (command_id, agent_id) 回填的结果必须脱敏。
//
// 这是现网命令回包走的那条路径（Agent → CommandResp），所以它比上一条更关键。
func TestAuditResultMaskedByCompleteByCommand(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", CommandID: "c1", Command: "cat /etc/my.cnf"})
	r.CompleteByCommand("c1", "a1", sensitiveOutput(), 0, 12)

	expectMasked(t, r.Audits()[0].Result)
}

// TestAuditResultMaskedInSQLite 落库的 result 同样必须是脱敏后的串。
//
// 内存里脱敏了不算数：备份包与 /api/audits 读的都是 SQLite。
// 落库与回填都在协程里做（避免阻塞命令转发），所以这里轮询等待。
func TestAuditResultMaskedInSQLite(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	r := NewRouter(nil, db)
	r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: "alice", AgentID: "a1", CommandID: "c-m2", Command: "cat /etc/my.cnf"})
	// 落库是异步的，ID 要等 INSERT 回来才填上；回填结果按 ID 定位，所以必须先等它。
	// 不等一下就是"结果永远落不进库"，测出来的脱敏也就只是内存那份。
	if err := waitUntil(5*time.Second, func() bool {
		logs := r.Audits()
		return len(logs) == 1 && logs[0].ID > 0
	}); err != nil {
		t.Fatalf("等待审计落库取回 ID 失败: %v", err)
	}
	r.CompleteByCommand("c-m2", "a1", sensitiveOutput(), 0, 12)

	var got string
	if err := waitUntil(5*time.Second, func() bool {
		rows, qerr := db.SearchAuditLogs(storage.AuditQueryCond{Command: "my.cnf"}, 0, 10)
		if qerr != nil {
			t.Fatalf("查询审计失败: %v", qerr)
		}
		if len(rows) != 1 {
			t.Fatalf("期望命中 1 条审计，实得 %d", len(rows))
		}
		got = rows[0].Result
		return got != ""
	}); err != nil {
		t.Fatalf("等待结果回填落库失败: %v", err)
	}
	expectMasked(t, got)
}

// TestAuditFilePermissionTightened 历史遗留的 0644 审计文件要在打开时收紧到 0600。
//
// os.OpenFile 的权限位只在新建时生效，所以"改成 0600"若不配一次显式 Chmod，
// 对已存在的文件是空操作 —— 而这正是现网的形态。
func TestAuditFilePermissionTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不实现 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "audits.jsonl")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatalf("预置审计文件失败: %v", err)
	}
	r := NewRouter(session.NewManager(), nil, path)
	defer r.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("审计文件权限 = %o，应为 0600", perm)
	}
}

// TestAuditFileCreatedPrivate 新建的审计文件直接就是 0600。
func TestAuditFileCreatedPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不实现 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "new", "audits.jsonl")
	r := NewRouter(session.NewManager(), nil, path)
	defer r.Close()

	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", Command: "uptime"})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("新建审计文件权限 = %o，应为 0600", perm)
	}
}
