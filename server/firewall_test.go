package main

import (
	"strings"
	"testing"
)

// TestUFWAllowsPort ufw 状态解析：已有 ALLOW（含 IPv6 行）应判定为已放通。
func TestUFWAllowsPort(t *testing.T) {
	status := `Status: active
To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
9000/tcp                   ALLOW       Anywhere
19001/tcp                  DENY        Anywhere
`
	if !ufwAllowsPort(status, 22) {
		t.Fatal("22/tcp 已放通，应返回 true")
	}
	if !ufwAllowsPort(status, 9000) {
		t.Fatal("9000/tcp 已放通，应返回 true")
	}
	if ufwAllowsPort(status, 19001) {
		t.Fatal("DENY 规则不应被视为已放通")
	}
	if ufwAllowsPort(status, 19002) {
		t.Fatal("不存在的端口不应被视为已放通")
	}
	if !ufwAllowsPort("19003/tcp (v6)  ALLOW  Anywhere (v6)", 19003) {
		t.Fatal("IPv6 的 ALLOW 规则也应识别")
	}
}

func TestFirewallRuleName(t *testing.T) {
	if got := firewallRuleName("Agent", 9000); !strings.Contains(got, "Agent") || !strings.Contains(got, "9000") {
		t.Fatalf("Windows 规则名应包含角色与端口，实际 %q", got)
	}
}

// TestEnsureFirewallPortRejectsInvalid 端口非法时不应执行任何系统命令。
func TestEnsureFirewallPortRejectsInvalid(t *testing.T) {
	for _, p := range []int{0, -1, 70000} {
		if msg := ensureFirewallPort(p, "Agent"); !strings.Contains(msg, "端口无效") {
			t.Fatalf("端口 %d 应被拒绝，实际 %q", p, msg)
		}
	}
}
