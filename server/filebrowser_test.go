package main

import (
	"path/filepath"
	"testing"
	"time"
)

// 服务端路径校验：整机模式只要求绝对路径；列表额外允许 "/"（盘符/根）。
func TestFsPathValidation(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "a.txt")
	if _, err := fsAbsolutePath(abs); err != nil {
		t.Errorf("绝对路径应接受: %v", err)
	}
	for _, p := range []string{"", "relative/path", "a.txt", string([]byte{0x00})} {
		if _, err := fsAbsolutePath(p); err == nil {
			t.Errorf("应拒绝 %q", p)
		}
	}
	// "/" 只在列表入口被放行（Windows 表示盘符列表）
	if got, err := fsListPath("/", "C:\\"); err != nil || got != "/" {
		t.Fatalf("fsListPath(/) = %q, %v", got, err)
	}
	fallback := filepath.Join(t.TempDir(), "root")
	if got, err := fsListPath("", fallback); err != nil || got != fallback {
		t.Fatalf("空路径应回落会话根: %q, %v", got, err)
	}
}

// 服务端可能跑在与 Agent 不同的 OS 上（Windows 总控管理 Linux 主机）：
// 路径判断/拼接必须按路径自身风格，不能套用本机 filepath 语义。
func TestFsPathCrossOS(t *testing.T) {
	for _, p := range []string{"/", "/etc", "/var/log", `C:\Windows`, `C:/Windows`, `\\server\share\x`} {
		if _, err := fsAbsolutePath(p); err != nil {
			t.Errorf("%q 应被视为绝对路径: %v", p, err)
		}
	}
	for _, p := range []string{"", "etc", "a/b", "C:relative"} {
		if _, err := fsAbsolutePath(p); err == nil {
			t.Errorf("%q 应被拒绝", p)
		}
	}
	// 不做本机 filepath.Clean：Linux 路径在 Windows 服务端上必须原样下发
	if got, _ := fsAbsolutePath("/etc/../etc/"); got != "/etc/../etc/" {
		t.Errorf("路径被本机 Clean 改写: %q", got)
	}
	if got := joinPathAnyOS("/etc", "a.conf"); got != "/etc/a.conf" {
		t.Errorf("POSIX 拼接错误: %q", got)
	}
	if got := joinPathAnyOS("/", "x"); got != "/x" {
		t.Errorf("根目录拼接错误: %q", got)
	}
	if got := joinPathAnyOS(`C:\Windows`, "a.txt"); got != `C:\Windows\a.txt` {
		t.Errorf("Windows 拼接错误: %q", got)
	}
	if got := baseNameAnyOS("/var/log/messages"); got != "messages" {
		t.Errorf("POSIX base 错误: %q", got)
	}
	if got := baseNameAnyOS(`C:\a\b.txt`); got != "b.txt" {
		t.Errorf("Windows base 错误: %q", got)
	}
	if got := dirNameAnyOS("/var/log/messages"); got != "/var/log" {
		t.Errorf("POSIX dir 错误: %q", got)
	}
	if got := dirNameAnyOS("/etc"); got != "/" {
		t.Errorf("根下子目录的父目录应为 /: %q", got)
	}
	if got := dirNameAnyOS(`C:\a\b.txt`); got != `C:\a` {
		t.Errorf("Windows dir 错误: %q", got)
	}
}

// 会话：操作者绑定、空闲回收、每主机上限、主动关闭。
func TestFileSessionLifecycle(t *testing.T) {
	m := newFileSessionManager()
	v, err := m.create("agent-a", "admin", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	// 其他操作员不能接管
	if _, err := m.acquire(v.ID, "other", false, true); err == nil {
		t.Fatal("不同操作员不应能使用该会话")
	}
	// 进行中操作会阻止空闲回收
	if _, err := m.acquire(v.ID, "admin", true, true); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[v.ID].LastActive = time.Now().Add(-fileSessionIdle - time.Minute)
	m.mu.Unlock()
	if n := m.sweep(time.Now()); n != 0 {
		t.Fatalf("进行中的会话不应被空闲回收，实际回收 %d", n)
	}
	m.release(v.ID)
	// release 会刷新 LastActive（结束操作即算一次活动），这里再把它拨回过去模拟真正空闲。
	m.mu.Lock()
	m.sessions[v.ID].LastActive = time.Now().Add(-fileSessionIdle - time.Minute)
	m.mu.Unlock()
	if n := m.sweep(time.Now()); n != 1 {
		t.Fatalf("空闲会话应被回收，实际 %d", n)
	}
	if _, err := m.acquire(v.ID, "admin", false, true); err == nil {
		t.Fatal("回收后的会话不应还能使用")
	}

	// 每主机上限
	var ids []string
	for i := 0; i < maxFileSessionsPerAgent; i++ {
		nv, err := m.create("agent-b", "admin", []string{"/data"})
		if err != nil {
			t.Fatalf("第 %d 个会话应允许: %v", i+1, err)
		}
		ids = append(ids, nv.ID)
	}
	if _, err := m.create("agent-b", "admin", []string{"/data"}); err == nil {
		t.Fatal("超过每主机上限应被拒绝")
	}
	if !m.close(ids[0], "admin") {
		t.Fatal("主动关闭应成功")
	}
	if m.close(ids[0], "admin") {
		t.Fatal("重复关闭应返回 false")
	}
}

// 会话只受"无操作时长"约束：久开但一直在用不会被踢，满 20 分钟空闲才回收。
func TestFileSessionIdleOnlyNoCumulativeCap(t *testing.T) {
	m := newFileSessionManager()
	v, err := m.create("agent-a", "admin", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	backdate := func(d time.Duration) {
		m.mu.Lock()
		m.sessions[v.ID].LastActive = time.Now().Add(-d)
		m.mu.Unlock()
	}
	// 一直有操作：即便这轮浏览已经拖过原先的 2 小时累计上限，也不该被回收
	backdate(time.Minute)
	if n := m.sweep(time.Now()); n != 0 {
		t.Fatalf("使用中的会话不应被回收，实际 %d", n)
	}
	if _, err := m.acquire(v.ID, "admin", false, false); err != nil {
		t.Fatalf("使用中的会话应可用: %v", err)
	}
	backdate(fileSessionIdle - time.Second)
	if n := m.sweep(time.Now()); n != 0 {
		t.Fatalf("无操作未满 %s 不应回收，实际 %d", fileSessionIdle, n)
	}
	backdate(fileSessionIdle + time.Second)
	if n := m.sweep(time.Now()); n != 1 {
		t.Fatalf("无操作满 %s 应回收，实际 %d", fileSessionIdle, n)
	}
}

// 全局会话上限：防止某条会话反复开新会话把服务端状态撑大。
func TestFileSessionGlobalCap(t *testing.T) {
	m := newFileSessionManager()
	for i := 0; i < maxFileSessions; i++ {
		if _, err := m.create("agent-"+string(rune('a'+i%26)), "admin", []string{"/data"}); err != nil {
			// 每主机上限可能先触发：换一个 agent 再试
			t.Fatalf("第 %d 个会话应允许: %v", i, err)
		}
	}
	if _, err := m.create("agent-zz", "admin", []string{"/data"}); err == nil {
		t.Fatal("超过全局上限应被拒绝")
	}
}
