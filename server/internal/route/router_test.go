package route

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	ok, msg := r.Dispatch(&protocol.CommandReq{AgentID: "ghost", Command: "echo hi"}, "u", "1.1.1.1", "sess-A")
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
	ok, _ := r.Dispatch(&protocol.CommandReq{AgentID: "a1", Command: "rm -rf /"}, "u", "1.1.1.1", "sess-A")
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
		"mysql -u root -p secret123":   "mysql -u root -p ***",
		"export TOKEN=abc123":          "export TOKEN=***",
		"curl -H 'Authorization: xyz'": "curl -H 'Authorization: ***",
		"echo hello world":             "echo hello world",
		// 连写形态（此前漏掉，口令明文进审计库）
		"mysql -u root -pP@ss0rd -h db": "mysql -u root -p*** -h db",
		"sshpass -pSecret123 ssh host":  "sshpass -p*** ssh host",
		"app --password=abc":            "app --password=***",
		"app --password abc":            "app --password ***",
		"set PASSPHRASE=zzz":            "set PASSPHRASE=***",
		// 不得误伤普通旗标（-p 后跟小写字母的连写形式）
		"netstat -port 5900":      "netstat -port 5900",
		"tool -path /tmp -pid 12": "tool -path /tmp -pid 12",
	}
	for c, want := range cases {
		if got := MaskCommand(c); got != want {
			t.Errorf("MaskCommand(%q)=%q, want %q", c, got, want)
		}
	}
}

// TestAuditMasksAtWrite 脱敏必须在写入时完成：audits.jsonl 与 SQLite 存的应是已脱敏内容。
func TestAuditMasksAtWrite(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", Command: "mysql -uroot -pSecret1 x"})
	got := r.Audits()
	if len(got) != 1 {
		t.Fatalf("expected 1 audit, got %d", len(got))
	}
	if strings.Contains(got[0].Command, "Secret1") {
		t.Fatalf("审计落库前未脱敏: %q", got[0].Command)
	}
	if !strings.Contains(got[0].Command, "***") {
		t.Fatalf("应包含脱敏标记: %q", got[0].Command)
	}
}

// TestAuditOperatorSanitized 未认证方提交的字段不得注入控制字符或超长垃圾。
func TestAuditOperatorSanitized(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	raw := "bob\x01\x02\nFAKE: 管理员执行了 rm -rf /" + strings.Repeat("x", 500)
	r.Audit(&AuditLog{Time: 1, Operator: raw, Command: "登录失败"})
	got := r.Audits()[0]
	if strings.ContainsAny(got.Operator, "\x01\x02\n") {
		t.Fatalf("审计字段仍含控制字符: %q", got.Operator)
	}
	if len(got.Operator) > 200 {
		t.Fatalf("审计字段未限长: %d", len(got.Operator))
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
	if r.auditIDFor(cmdID, "a1") != 0 {
		t.Fatal("回填后 command_id 映射应被清理")
	}
}

// TestCommandOriginRejectsOtherAgent 归属校验：命令只认当初接令的那台 Agent。
// 背景：command_id 由浏览器生成（'push-'+Date.now()），完全可猜；不校验归属时
// 任一已接入的 Agent 都能替别人的主机伪造审计结果。
func TestCommandOriginRejectsOtherAgent(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	r.auditMu.Lock()
	r.cmdAudit[cmdKey("c-1", "a1")] = &cmdBinding{agentID: "a1", session: "sess-A", last: time.Now()}
	r.auditMu.Unlock()

	if _, ok := r.CommandOrigin("c-1", "a1"); !ok {
		t.Fatal("a1 下发给它的命令应当可归属")
	}
	if _, ok := r.CommandOrigin("c-1", "evil"); ok {
		t.Fatal("evil 从未接令 c-1，不应归属成功")
	}
	// 空 ID 一律不可归属（避免零值键互相撞上）
	if _, ok := r.CommandOrigin("", "a1"); ok {
		t.Fatal("空 command_id 不应归属")
	}
	if _, ok := r.CommandOrigin("c-1", ""); ok {
		t.Fatal("空 agent_id 不应归属")
	}
}

// TestReleaseSession 分享撤销/到期时，只收口该会话的命令绑定。
func TestReleaseSession(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	now := time.Now()
	r.auditMu.Lock()
	r.cmdAudit[cmdKey("it-1", "a1")] = &cmdBinding{agentID: "a1", session: "share:abc", last: now}
	r.cmdAudit[cmdKey("it-2", "a1")] = &cmdBinding{agentID: "a1", session: "admin", last: now}
	r.cmdAudit[cmdKey("it-3", "a2")] = &cmdBinding{agentID: "a2", session: "share:abc", last: now}
	r.auditMu.Unlock()

	refs := r.ReleaseSession("share:abc")
	if len(refs) != 2 {
		t.Fatalf("应取出 2 条分享会话的命令，实际 %d: %+v", len(refs), refs)
	}
	got := map[string]string{}
	for _, ref := range refs {
		got[ref.CommandID] = ref.AgentID
	}
	if got["it-1"] != "a1" || got["it-3"] != "a2" {
		t.Fatalf("agent_id/command_id 配对错误: %+v", got)
	}
	// 其它会话的绑定必须原样保留
	if _, ok := r.CommandOrigin("it-2", "a1"); !ok {
		t.Fatal("管理端会话的绑定不应被误删")
	}
	if _, ok := r.CommandOrigin("it-1", "a1"); ok {
		t.Fatal("已释放的命令不应再可归属")
	}
	if _, ok := r.CommandOrigin("it-3", "a2"); ok {
		t.Fatal("已释放的命令不应再可归属")
	}
}

// TestDispatchRejectsCrossSessionCommandID 回归：分享访客不能用可猜的 command_id
// 覆盖管理端已有的归属（否则会收到管理端终端输出、并把输入打进对方会话）。
func TestDispatchRejectsCrossSessionCommandID(t *testing.T) {
	sess := session.NewManager()
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := p2.Read(buf); err != nil {
				return
			}
		}
	}()
	if _, ok := sess.Upsert("a1", session.NewConn(p1)); !ok {
		t.Fatal("测试前置：会话登记失败")
	}
	r := NewRouter(sess, nil)
	req := func() *protocol.CommandReq {
		return &protocol.CommandReq{AgentID: "a1", CommandID: "it-1", Command: "echo hi"}
	}

	if ok, msg := r.Dispatch(req(), "share:abc", "ip", "share:abc"); !ok {
		t.Fatalf("首个会话应能下发: %s", msg)
	}
	if ok, _ := r.Dispatch(req(), "admin", "ip", "admin-session"); ok {
		t.Fatal("其他会话用同一 command_id 必须被拒绝")
	}
	origin, found := r.CommandOrigin("it-1", "a1")
	if !found || origin.Session != "share:abc" {
		t.Fatalf("原归属不应被覆盖: %+v", origin)
	}
	// 同一会话重复使用自己的 command_id（重连/重发）仍允许
	if ok, msg := r.Dispatch(req(), "share:abc", "ip", "share:abc"); !ok {
		t.Fatalf("同会话复用 command_id 应允许: %s", msg)
	}
	// 超长 ID 直接拒绝
	long := &protocol.CommandReq{AgentID: "a1", CommandID: strings.Repeat("x", maxCommandIDLen+1), Command: "echo hi"}
	if ok, _ := r.Dispatch(long, "share:abc", "ip", "share:abc"); ok {
		t.Fatal("超长 command_id 必须被拒绝")
	}
}

// TestCompleteByCommandIgnoresOtherAgent 别的 Agent 不得回填（并清掉）他人命令的结果。
func TestCompleteByCommandIgnoresOtherAgent(t *testing.T) {
	r := NewRouter(session.NewManager(), nil)
	const cmdID = "cmd-shared"
	r.Audit(&AuditLog{Time: 1, Operator: "alice", AgentID: "a1", CommandID: cmdID, Command: "uptime"})
	r.auditMu.Lock()
	r.cmdAudit[cmdKey(cmdID, "a1")] = &cmdBinding{agentID: "a1", last: time.Now()}
	r.auditMu.Unlock()

	// evil 用同样的 ID 伪造结果
	r.CompleteByCommand(cmdID, "evil", "forged", 0, 1)
	if got := r.Audits()[0].Result; got != "" {
		t.Fatalf("伪造的回填不应改写 a1 的审计结果，实得 %q", got)
	}
	// a1 自己回填才生效
	r.CompleteByCommand(cmdID, "a1", "real", 0, 7)
	if got := r.Audits()[0]; got.Result != "real" {
		t.Fatalf("a1 的回填应生效，实得 %q", got.Result)
	}
}

// TestBatchCommandsKeepSeparateBindings 批量下发共用一个 command_id，
// 每台机器的归属必须各占一条，否则后写入的会顶掉前面的。
func TestBatchCommandsKeepSeparateBindings(t *testing.T) {
	sess := session.NewManager()
	r := NewRouter(sess, nil)
	r.auditMu.Lock()
	for _, a := range []string{"a1", "a2", "a3"} {
		r.cmdAudit[cmdKey("batch-1", a)] = &cmdBinding{agentID: a, session: "sess-A", last: time.Now()}
	}
	r.auditMu.Unlock()
	for _, a := range []string{"a1", "a2", "a3"} {
		if _, ok := r.CommandOrigin("batch-1", a); !ok {
			t.Fatalf("%s 的回执应可归属", a)
		}
	}
	r.CompleteByCommand("batch-1", "a2", "done", 0, 1)
	r.auditMu.Lock()
	_, a2Exists := r.cmdAudit[cmdKey("batch-1", "a2")]
	_, a1Exists := r.cmdAudit[cmdKey("batch-1", "a1")]
	r.auditMu.Unlock()
	if a2Exists {
		t.Fatal("a2 已结束，其映射应被清理")
	}
	if !a1Exists {
		t.Fatal("清理 a2 不应波及 a1 的映射")
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

// TestSearchAuditsPageMemory 验证内存回退路径下的分页与总数统计。
func TestSearchAuditsPageMemory(t *testing.T) {
	r := NewRouter(nil, nil)
	for i := 0; i < 25; i++ {
		r.Audit(&AuditLog{
			Time:     int64(1000 + i),
			Operator: "admin",
			AgentID:  "agent-1",
			Command:  "echo " + string(rune('a'+i%26)),
		})
	}
	// 第一页：默认 limit=20，返回最新的 20 条，total 为总数
	page, err := r.SearchAuditsPage(AuditQuery{Limit: 20})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if page.Total != 25 {
		t.Fatalf("expected total 25, got %d", page.Total)
	}
	if len(page.Items) != 20 {
		t.Fatalf("expected 20 items, got %d", len(page.Items))
	}
	// 倒序：第一条应为最后写入的
	if page.Items[0].Time != 1024 {
		t.Fatalf("expected first item time 1024, got %d", page.Items[0].Time)
	}

	// 第二页：offset=20，应剩余 5 条
	page2, _ := r.SearchAuditsPage(AuditQuery{Offset: 20, Limit: 20})
	if len(page2.Items) != 5 {
		t.Fatalf("expected 5 items on page 2, got %d", len(page2.Items))
	}
	if page2.Total != 25 {
		t.Fatalf("expected total 25 on page 2, got %d", page2.Total)
	}

	// 超界 offset 应返回空列表而非报错
	page3, _ := r.SearchAuditsPage(AuditQuery{Offset: 100, Limit: 20})
	if len(page3.Items) != 0 {
		t.Fatalf("expected empty page, got %d", len(page3.Items))
	}

	// limit 超上限时收敛到 500
	_, limit := normalizePage(0, 99999)
	if limit != auditMaxPageSize {
		t.Fatalf("expected limit clamped to %d, got %d", auditMaxPageSize, limit)
	}
}

// TestSearchAuditsPageFilter 验证条件过滤与类别过滤。
func TestSearchAuditsPageFilter(t *testing.T) {
	r := NewRouter(nil, nil)
	r.Audit(&AuditLog{Time: 100, Operator: "alice", AgentID: "web-1", Command: "uptime", Category: "command"})
	r.Audit(&AuditLog{Time: 200, Operator: "bob", AgentID: "web-2", Command: "登录成功", Category: "auth"})
	r.Audit(&AuditLog{Time: 300, Operator: "alice", AgentID: "web-1", Command: "扩容磁盘", Category: "op"})

	page, _ := r.SearchAuditsPage(AuditQuery{Category: "auth"})
	if page.Total != 1 || page.Items[0].Command != "登录成功" {
		t.Fatalf("category filter failed: total=%d", page.Total)
	}

	page, _ = r.SearchAuditsPage(AuditQuery{Operator: "alice"})
	if page.Total != 2 {
		t.Fatalf("operator filter failed: total=%d", page.Total)
	}

	// 空类别按命令执行处理（兼容历史记录）
	page, _ = r.SearchAuditsPage(AuditQuery{Category: "command"})
	if page.Total != 1 || page.Items[0].Category != "command" {
		t.Fatalf("default category failed: total=%d", page.Total)
	}

	page, _ = r.SearchAuditsPage(AuditQuery{Since: 150, Until: 250})
	if page.Total != 1 || page.Items[0].Time != 200 {
		t.Fatalf("time range filter failed: total=%d", page.Total)
	}
}

// TestSearchAuditsPageSQLite 验证 SQLite 路径下的分页、计数与类别过滤。
func TestSearchAuditsPageSQLite(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	r := NewRouter(nil, db)
	for i := 0; i < 30; i++ {
		cat := "command"
		if i%3 == 0 {
			cat = "op"
		}
		if _, err := db.InsertAuditLog(&storage.AuditLogRow{
			Time: int64(1000 + i), Operator: "admin", AgentID: "host-1",
			Command: "cmd", Category: cat,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	page, err := r.SearchAuditsPage(AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if page.Total != 30 {
		t.Fatalf("expected total 30, got %d", page.Total)
	}
	if len(page.Items) != 10 {
		t.Fatalf("expected 10 items, got %d", len(page.Items))
	}
	// 分页不重叠：第二页首条时间小于第一页末条
	page2, _ := r.SearchAuditsPage(AuditQuery{Offset: 10, Limit: 10})
	if len(page2.Items) != 10 {
		t.Fatalf("expected 10 items on page2, got %d", len(page2.Items))
	}
	if page2.Items[0].Time >= page.Items[len(page.Items)-1].Time {
		t.Fatalf("pages overlap: %d >= %d", page2.Items[0].Time, page.Items[len(page.Items)-1].Time)
	}

	// 类别过滤 + 计数
	opPage, _ := r.SearchAuditsPage(AuditQuery{Category: "op", Limit: 5})
	if opPage.Total != 10 {
		t.Fatalf("expected 10 op logs, got %d", opPage.Total)
	}

	// 留存清理不影响近期数据
	n, err := db.CleanupOldAuditLogs(1)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 30 {
		t.Fatalf("expected 30 cleaned (old), got %d", n)
	}
}
