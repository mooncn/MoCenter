package main

import (
	"encoding/csv"
	"net/http/httptest"
	"strings"
	"testing"

	"mocenter/server/internal/route"
)

// 本文件锚住 L-1（审计 CSV 公式注入）与 L-2（日志注入残留）。
//
// 为什么这两条值得专门写用例：它们的载体都是**被管主机回传的字符串**，
// 也就是"一台被拿下的 Agent"能直接决定运维在本地打开的文件/日志里写什么。
// 单人使用也一样中招 —— 导出的正是自己机器上那些主机的回传结果。

func TestWriteAuditCSVNeutralizesFormulaInjection(t *testing.T) {
	logs := []*route.AuditLog{
		// 典型的 DDE / 外链载荷：Excel 打开就会执行
		{Time: 1, Operator: "op", AgentID: "a1", CommandID: "c1",
			Command: `=cmd|'/c calc'!A1`, Result: "ok"},
		// 以 + - @ 开头同样被当公式
		{Time: 2, Operator: "op", AgentID: "a2", CommandID: "c2",
			Command: "+1+1", Result: "@SUM(1)"},
		// 前导制表符/CR：Excel 会先吃掉空白再看首字符
		{Time: 3, Operator: "op", AgentID: "a3", CommandID: "c3",
			Command: "\t=1+1", Result: "\r-2+3"},
		// 前面插空格再写公式
		{Time: 4, Operator: "op", AgentID: "a4", CommandID: "c4",
			Command: "   =1+1", Result: "-1"},
		// 正常内容不许被改动
		{Time: 5, Operator: "op", AgentID: "a5", CommandID: "c5",
			Command: "df -h", Result: "Filesystem  Size"},
	}
	rec := httptest.NewRecorder()
	writeAuditCSV(rec, logs)
	body := rec.Body.String()

	rd := csv.NewReader(strings.NewReader(body))
	rows, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("导出的 CSV 无法解析: %v", err)
	}
	if len(rows) != len(logs)+1 {
		t.Fatalf("行数不对：%d（含表头应为 %d）", len(rows), len(logs)+1)
	}

	// 关键断言：任何单元格都不得以 = + - @ 开头（前导空白已由 csv 引用消化）
	for r := 1; r < len(rows); r++ {
		for c, cell := range rows[r] {
			if cell == "" {
				continue
			}
			if strings.ContainsAny(cell[:1], "=+-@") {
				t.Errorf("第 %d 行第 %d 列仍以危险字符开头：%q", r, c, cell)
			}
		}
	}

	// 内容本身必须保留（加的是前导单引号这种"按文本处理"约定，不是剥除）
	joined := body
	for _, want := range []string{"cmd|", "'/c calc'!A1", "+1+1", "@SUM(1)", "df -h"} {
		if !strings.Contains(joined, want) {
			t.Errorf("导出件丢失了原始内容片段 %q —— 中和不应改成剥除", want)
		}
	}
}

func TestCSVCellNeutralizationTable(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"=1+1", "'=1+1"},
		{"+1", "'+1"},
		{"-1", "'-1"},
		{"@x", "'@x"},
		{"\t=1", "'\t=1"},
		{"\r=1", "'\r=1"},
		{"  =1", "'  =1"},
		{"df -h", "df -h"},
		{"", ""},
		{"   ", "   "},
		{"中文内容", "中文内容"},
	}
	for _, c := range cases {
		if got := csvCell(c.in); got != c.want {
			t.Errorf("csvCell(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// L-2：被管主机自报的自由文本进日志前必须被压成单行。
//
// 不这么做的后果不是"多打一行"：塞一个 \n 就能伪造出 "[server] Agent xxx 上线"
// 这类看着像服务端结论的记录，事后排障直接被带偏。
func TestLogSafeStripsForgedServerLines(t *testing.T) {
	payload := "boom\n[server] Agent evil 已获得 root，所有主机已失陷"
	got := logSafe(payload)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("logSafe 没有压掉换行：%q", got)
	}
	if strings.Contains(got, "\n[server]") {
		t.Fatalf("伪造的服务端日志行仍可注入：%q", got)
	}
	long := strings.Repeat("A", 500)
	if n := len(logSafe(long)); n > logSafeMax+4 {
		t.Fatalf("logSafe 未限长：%d", n)
	}
	if logSafe("ok") != "ok" {
		t.Fatal("logSafe 改动了正常文本")
	}
}
