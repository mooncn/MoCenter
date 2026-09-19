//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExistingInstallDetectsConfig 回归：升级识别以配置文件为准。
// 这是「升级」与「重装」的分水岭 —— 判错了就会把用户的
// 服务端地址 / 注册密钥 / 账号 / 数据库一起抹掉。
func TestExistingInstallDetectsConfig(t *testing.T) {
	root := t.TempDir()
	o := options{
		installDir: filepath.Join(root, "ProgramFiles", "MoCenter"),
		dataRoot:   filepath.Join(root, "ProgramData", "MoCenter"),
	}

	if existingInstall(o, "agent") {
		t.Fatal("未安装时不应识别为已安装")
	}

	// 现行布局：配置在数据目录
	dir := filepath.Join(o.dataRoot, "agent")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mocenter-agent.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if !existingInstall(o, "agent") {
		t.Fatal("配置存在时应识别为已安装（走升级路径）")
	}
	if existingInstall(o, "server") {
		t.Fatal("agent 的安装不应让 server 也被判为已安装")
	}

	// 目录（而非文件）不算安装，避免把空目录当成已装
	empty := filepath.Join(o.dataRoot, "server", "mocenter-server.json")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if existingInstall(o, "server") {
		t.Fatal("同名目录不应被当成配置文件")
	}

	// 兼容早期把配置放在安装目录的版本
	legacy := options{installDir: filepath.Join(root, "legacy"), dataRoot: filepath.Join(root, "none")}
	if err := os.MkdirAll(legacy.installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy.installDir, "mocenter-agent.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if !existingInstall(legacy, "agent") {
		t.Fatal("安装目录下的旧版配置也应被识别")
	}
}

// TestKeepAliveTaskNames 任务名必须与卸载/状态查询时完全一致。
// 两组名字对不上会留下"装了删不掉"的孤儿计划任务，看门狗仍会拉起进程。
func TestKeepAliveTaskNames(t *testing.T) {
	watchdog, boot := keepAliveTasks("agent")
	if watchdog != "MoCenterAgentKeepAlive" {
		t.Fatalf("agent 巡检任务名 = %q，期望 MoCenterAgentKeepAlive", watchdog)
	}
	if boot != "MoCenterAgentBoot" {
		t.Fatalf("agent 自启任务名 = %q，期望 MoCenterAgentBoot", boot)
	}
	sw, sb := keepAliveTasks("server")
	if sw != "MoCenterServerKeepAlive" || sb != "MoCenterServerBoot" {
		t.Fatalf("server 任务名 = %q/%q", sw, sb)
	}
	// 服务名与任务名前缀必须一致，否则卸载时删不掉任务
	if serviceNameOf("agent") != "MoCenterAgent" || serviceNameOf("server") != "MoCenterServer" {
		t.Fatal("服务名发生变化：需同步 keepAliveTasks 与 setup.iss 的卸载参数")
	}
}

// TestTaskActionQuotesPaths 命令行里带空格的路径必须被正确引用，
// 否则 schtasks 会把 C:\Program Files\... 拆成两个参数、任务创建失败。
func TestTaskActionQuotesPaths(t *testing.T) {
	got := taskAction("agent", `C:\Program Files\MoCenter\mocenter-agent.exe`,
		`C:\ProgramData\MoCenter\agent\mocenter-agent.json`)
	if got == "" || got[0] != '"' {
		t.Fatalf("整条命令应被双引号包裹，实得 %q", got)
	}
	// 含空格的路径需要转义引号，避免被 schtasks 二次拆分
	if !contains(got, `C:\\Program Files`) && !contains(got, `\"C:\Program Files`) {
		t.Fatalf("含空格路径未被引用: %q", got)
	}
	if !contains(got, "run") {
		t.Fatalf("应走 mocenter-deploy.exe run 隐藏窗口通道: %q", got)
	}
}
