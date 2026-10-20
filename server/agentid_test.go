package main

import "testing"

// TestValidAgentIDRejectsMetachars 身份入口的字符集收紧（L-9）。
//
// 这些串都是"下游若不转义就会出事"的形态：HTML 标记、shell 分隔符、路径片段。
// 合法主机名从来不需要它们，所以在入口一次拒掉，比在每条出口各自记得转义可靠。
func TestValidAgentIDRejectsMetachars(t *testing.T) {
	legal := []string{
		"Rocky8", "win10-pc", "4C4C4544-0037-3010-8046-B7C04F4A3332",
		"主控-01.机房", "agent_1",
	}
	for _, s := range legal {
		if !validAgentID(s) {
			t.Errorf("合法身份被拒: %q", s)
		}
	}
	rejected := []string{
		"", "   ", "a b", "tab\there", "new\nline",
		"<script>alert(1)</script>", `a"b`, "a'b", "a`b",
		"a;b|c", "a$(id)", "a&", "../../etc/passwd", "a\\b",
		"host:9000", "a*b?c", "a#1", "a%0a", "mailto:a@b.c",
	}
	for _, s := range rejected {
		if validAgentID(s) {
			t.Errorf("危险身份未被拒: %q", s)
		}
	}
	if validAgentID(string(make([]byte, 129))) {
		t.Error("超长身份应被拒")
	}
}
