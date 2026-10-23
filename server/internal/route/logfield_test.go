package route

// 日志注入回归：审计行里的 operator / agent_id / command 都可能来自不可信输入
// （登录失败时 operator 直接取自请求体里的用户名），原样打印就能在日志里
// 伪造出额外的 "[server] ..." 行，误导事后排查。

import (
	"strings"
	"testing"
)

func TestLogFieldFlattensInjection(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"admin", "admin"},
		{"admin\n[server] 已关闭鉴权", "admin [server] 已关闭鉴权"},
		{"a\r\nb\rc\td", "a b c d"},
		{"a\x00b\x1b[31mc", "a·b·[31mc"},
		{"  多  空格  ", "多 空格"},
	}
	for _, c := range cases {
		if got := logField(c.in); got != c.want {
			t.Fatalf("logField(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestLogFieldTruncates(t *testing.T) {
	long := make([]byte, logFieldMax*2)
	for i := range long {
		long[i] = 'a'
	}
	if got := logField(string(long)); len(got) > logFieldMax+4 {
		t.Fatalf("超长字段未被截断：%d 字节", len(got))
	}
}

// 登录失败会把请求体里的用户名写进审计，这一行再进日志 —— 端到端确认换行被压平。
//
// sanitizeAuditField 只去掉控制字符，换行（0x0a）恰好不在其中，所以第二道
// logField 不能省：少了它，用户名里的换行就能在日志里另起一行。
func TestAuditLogLineHasNoNewline(t *testing.T) {
	injected := "bob\n[server] Agent 已接管"
	audit := &AuditLog{Operator: sanitizeAuditField(injected), AgentID: "a1\n/x", Command: "whoami"}
	if strings.ContainsAny(audit.Operator, "\r\n") {
		t.Fatalf("前置假设已变（sanitizeAuditField 现在也去换行），本测试需重写: %q", audit.Operator)
	}
	for _, v := range []string{audit.Operator, audit.AgentID, audit.Command} {
		if got := logField(v); strings.ContainsAny(got, "\r\n") {
			t.Fatalf("字段未被压平: %q -> %q", v, got)
		}
	}
}
