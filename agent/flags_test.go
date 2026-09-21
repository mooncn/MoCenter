package main

import (
	"flag"
	"testing"
)

// TestNormalizeBlankFlagsAgent 钉住 Agent 侧「空串参数按未配置处理」。
//
// 背景：mocenter-agent.service 写成 `-server "${MOCENTER_SERVER}" -agentid "${MOCENTER_AGENT_ID}"`，
// EnvironmentFile 缺键时传进来的是空 argv。它不改变运行期取值（-server 本来就有配置文件回并），
// 但 flag.Visit 会把它当成「命令行显式给出」，于是自动回写把配置文件里已存的服务端地址与
// agent_id 直接抹掉 —— 表现是"重启一次 Agent 就再也连不上服务端"。
func TestNormalizeBlankFlagsAgent(t *testing.T) {
	newSet := func() (*flag.FlagSet, *string, *string) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		// 与真实 flag 一致：默认值取自环境变量，这里模拟"环境里配了值"
		server := fs.String("server", "mc.example.com:9000", "服务端地址")
		agentID := fs.String("agentid", "", "Agent 标识（默认为空）")
		return fs, server, agentID
	}

	fs, server, agentID := newSet()
	if err := fs.Parse([]string{"-server", "", "-agentid", "web-01"}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	blank := normalizeBlankFlags(fs)

	if !blank["server"] {
		t.Fatal("-server 传空串应被登记为「按未配置处理」")
	}
	if *server != "mc.example.com:9000" {
		t.Fatalf("server = %q，期望回落成默认 %q", *server, "mc.example.com:9000")
	}
	if blank["agentid"] {
		t.Fatal("-agentid 给了真实值，不应被归一")
	}
	if *agentID != "web-01" {
		t.Fatalf("agentid = %q，期望原样保留", *agentID)
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	clearBlankExplicit(explicit, blank)
	if explicit["server"] {
		t.Fatal("空串 server 不能再算「显式给出」，否则会把配置文件里的地址回写成空")
	}
	if !explicit["agentid"] {
		t.Fatal("显式给出的 agentid 必须保留，否则改不动配置")
	}
}
