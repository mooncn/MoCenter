package agentdeploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidAgentID 守护 AgentID 白名单。
//
// 这是**安全边界**而非格式偏好：AgentID 会进入 shell 与 systemd 单元，
// 一个 `x; rm -rf /` 的 AgentID 就是一次远程代码执行。
func TestValidAgentID(t *testing.T) {
	ok := []string{"web-01", "node_2", "a", "10.0.0.1", "Ubuntu-22.04", strings.Repeat("a", 64)}
	for _, s := range ok {
		if !ValidAgentID(s) {
			t.Errorf("应接受 %q", s)
		}
	}
	bad := []string{
		"", "x; rm -rf /", "a b", "a|b", "a&b", "a$(id)", "a`id`",
		"../../etc/passwd", "a\nb", "a'b", `a"b`, "a>b", "a<b",
		strings.Repeat("a", 65),
	}
	for _, s := range bad {
		if ValidAgentID(s) {
			t.Errorf("应拒绝 %q", s)
		}
	}
}

// TestShellQuote shellQuote 必须让任意内容都只是一个字面量参数。
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"abc":        `'abc'`,
		"a b":        `'a b'`,
		"a'b":        `'a'\''b'`,
		"$(id)":      `'$(id)'`,
		"`id`":       "'`id`'",
		"; rm -rf /": `'; rm -rf /'`,
		"a\nb": `'a
b'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestInstallScriptQuotesInputs 安装脚本里不得出现未加引号的用户输入。
func TestInstallScriptQuotesInputs(t *testing.T) {
	evil := "/tmp/x; rm -rf /"
	script := buildInstallScript(installScriptArgs{
		RemoteBin: evil, RemoteConf: evil, InstallDir: "/usr/local/bin",
		Service: "mocenter-agent", HasSystemd: true, AgentID: "a",
	})
	// 用户输入必须只以 shellQuote 后的形式出现：
	// 反过来说，脚本里不能出现"未加引号的原始值"。
	quoted := shellQuote(evil)
	if !strings.Contains(script, quoted) {
		t.Fatalf("安装脚本里的路径应使用 shellQuote 处理:\n%s", script)
	}
	// 逐行检查：任何包含该输入的赋值行都必须带引号
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "rm -rf") {
			continue
		}
		if !strings.Contains(line, quoted) {
			t.Fatalf("存在未加引号的用户输入: %q", line)
		}
	}
}

// TestBuildAgentConfig 生成的配置必须能被 Agent 侧结构体解析（字段名一致）。
//
// 字段名一旦对不上，Agent 读到的就是一份"有内容但全零值"的配置，
// 表现为"装完了却连不上服务端"—— 这是最难排查的一类问题。
func TestBuildAgentConfig(t *testing.T) {
	d := New(Options{AgentKey: "K3Y", ProxyIP: "http://p/?url="})
	conf := d.buildAgentConfig(&Request{
		AgentID:        "web-01",
		LatencyTargets: []string{"icmp:1.1.1.1", "tcp:1.1.1.1:443"},
	}, "1.2.3.4:9000")

	var got map[string]interface{}
	if err := jsonUnmarshal([]byte(conf), &got); err != nil {
		t.Fatalf("生成的配置不是合法 JSON: %v\n%s", err, conf)
	}
	// 逐一核对 Agent 侧 Config 的 json tag
	wants := map[string]interface{}{
		"server": "1.2.3.4:9000", "agent_id": "web-01", "uuid": "",
		"heartbeat": float64(30), "probe_every": float64(10),
		"tls": false, "insecure": false, "daemon": false,
		"client_cert": "", "client_key": "", "ca_cert": "",
		"proxy_ip": "http://p/?url=", "agent_key": "K3Y",
		"upgrade_pubkey": "", "upgrade_allow_unsigned": false,
	}
	for k, want := range wants {
		got, ok := got[k]
		if !ok {
			t.Errorf("配置缺少字段 %q（Agent 侧会读到零值）", k)
			continue
		}
		if got != want {
			t.Errorf("字段 %q = %v，期望 %v", k, got, want)
		}
	}
	lt, ok := got["latency_targets"].([]interface{})
	if !ok || len(lt) != 2 {
		t.Fatalf("latency_targets 不正确: %v", got["latency_targets"])
	}
	// 配置里不能出现明文密码字段（我们只写 agent_key）
	if strings.Contains(conf, "password") {
		t.Fatalf("配置里不应出现 password 字段:\n%s", conf)
	}
}

// TestBuildAgentConfigNoKey 未配置 agent_key 时也要生成合法的空串，
// 而不是省略字段（Agent 侧统一读同一个 key）。
func TestBuildAgentConfigNoKey(t *testing.T) {
	d := New(Options{})
	conf := d.buildAgentConfig(&Request{AgentID: "a"}, "h:1")
	var got map[string]interface{}
	if err := jsonUnmarshal([]byte(conf), &got); err != nil {
		t.Fatalf("配置不合法: %v", err)
	}
	if v, ok := got["agent_key"]; !ok || v != "" {
		t.Fatalf("agent_key 应为空串，实际 %#v", got["agent_key"])
	}
	if lt, ok := got["latency_targets"].([]interface{}); !ok || len(lt) != 0 {
		t.Fatalf("未配置延迟目标时应为空数组，实际 %#v", got["latency_targets"])
	}
}

// TestMapArch 架构映射必须覆盖常见 uname -m 输出。
func TestMapArch(t *testing.T) {
	cases := map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
		"armv7l": "arm", "armv6l": "arm", "arm": "arm",
		"i386": "386", "i686": "386",
	}
	for in, want := range cases {
		got, err := mapArch(in)
		if err != nil {
			t.Errorf("mapArch(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("mapArch(%q) = %q，期望 %q", in, got, want)
		}
	}
	if _, err := mapArch("sparc64"); err == nil {
		t.Error("不支持的架构应报错")
	}
}

// TestPickBinary 安装包挑选逻辑：按架构匹配、拒绝路径穿越。
func TestPickBinary(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"mocenter-agent-amd64", "mocenter-agent-arm64", "mocenter-agent-arm",
		"mocenter-agent-386", "mocenter-agent.sha256", "mocenter-agent_1.0.1_amd64.deb",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("bin"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	d := New(Options{PkgDir: dir})

	for arch, want := range map[string]string{
		"amd64": "mocenter-agent-amd64",
		"arm64": "mocenter-agent-arm64",
		"arm":   "mocenter-agent-arm",
		"386":   "mocenter-agent-386",
	} {
		got, err := d.pickBinary("", arch)
		if err != nil {
			t.Errorf("挑选 %s 失败: %v", arch, err)
			continue
		}
		if filepath.Base(got) != want {
			t.Errorf("架构 %s 应选 %s，实际 %s", arch, want, filepath.Base(got))
		}
	}

	// 路径穿越必须被拒绝
	for _, bad := range []string{"../../etc/passwd", "sub/dir/bin", "..", "a/../b"} {
		if _, err := d.pickBinary(bad, "amd64"); err == nil {
			t.Errorf("应拒绝路径穿越的安装包名 %q", bad)
		}
	}
	// 显式指定存在的文件
	if _, err := d.pickBinary("mocenter-agent-amd64", "amd64"); err != nil {
		t.Errorf("显式指定应成功: %v", err)
	}
	// 不存在的文件
	if _, err := d.pickBinary("nope", "amd64"); err == nil {
		t.Error("不存在的安装包应报错")
	}
}

// TestPickBinaryNoMatch 目标架构没有对应包时必须给出**可排查**的错误信息。
func TestPickBinaryNoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mocenter-agent-amd64"), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	d := New(Options{PkgDir: dir})
	_, err := d.pickBinary("", "arm64")
	if err == nil {
		t.Fatal("没有 arm64 包时应报错")
	}
	// 错误信息要列出"现有包"，否则运维只能猜
	if !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("错误信息应列出可选包，实际: %v", err)
	}
}

// TestPickBinaryEmptyDir 空目录的提示要指向"如何构建"。
func TestPickBinaryEmptyDir(t *testing.T) {
	d := New(Options{PkgDir: t.TempDir()})
	_, err := d.pickBinary("", "amd64")
	if err == nil {
		t.Fatal("空目录应报错")
	}
	if !strings.Contains(err.Error(), "build-all") {
		t.Fatalf("错误信息应提示构建方式，实际: %v", err)
	}
}

// TestDeployRejectsBadInput 参数校验必须发生在建立 SSH 连接之前。
//
// 否则一个畸形请求会先花 15 秒去连一台机器，再报"参数不合法"。
func TestDeployRejectsBadInput(t *testing.T) {
	d := New(Options{})
	cases := []Request{
		{Host: "", User: "root", Password: "x"},
		{Host: "1.1.1.1", User: "", Password: "x"},
		{Host: "1.1.1.1", User: "root"},
		{Host: "with space", User: "root", Password: "x"},
		{Host: "1.1.1.1", User: "root", Password: "x", AgentID: "bad;id"},
		{Host: "1.1.1.1", User: "root", Password: "x", Port: 99999},
	}
	for i, req := range cases {
		if _, err := d.Deploy(&req, "op", "1.2.3.4"); err == nil {
			t.Errorf("用例 %d 应被拒绝: %+v", i, req)
		}
	}
	if _, err := d.Deploy(nil, "op", "1.2.3.4"); err == nil {
		t.Error("nil 请求应被拒绝")
	}
}

// TestDeployUnreachableSSH 连不上时必须**返回结果**（而不是返回错误），
// 且结果里带步骤明细，让前端能显示"卡在哪一步"。
func TestDeployUnreachableSSH(t *testing.T) {
	d := New(Options{})
	res, err := d.Deploy(&Request{
		Host: "127.0.0.1", Port: 1, User: "root", Password: "x",
	}, "op", "1.2.3.4")
	if err != nil {
		t.Fatalf("连接失败应作为结果返回而不是 error: %v", err)
	}
	if res.OK {
		t.Fatal("不应报告成功")
	}
	if len(res.Steps) == 0 || res.Steps[0].OK {
		t.Fatalf("应有失败的连接步骤: %+v", res.Steps)
	}
	if res.Steps[0].Error == "" {
		t.Fatal("失败步骤应带错误原因")
	}
}

// TestStatusProbeCmd 状态确认命令要能给出 active/enabled/version。
func TestStatusProbeCmd(t *testing.T) {
	c := statusProbeCmd("mocenter-agent", true)
	for _, want := range []string{"is-active", "is-enabled", "-version"} {
		if !strings.Contains(c, want) {
			t.Errorf("systemd 状态命令应包含 %q: %s", want, c)
		}
	}
	c2 := statusProbeCmd("mocenter-agent", false)
	if !strings.Contains(c2, "pgrep") {
		t.Errorf("非 systemd 场景应使用 pgrep: %s", c2)
	}
}

// TestPackageCandidates 应列出候选包并推导架构，且不含 deb/rpm/校验文件。
func TestPackageCandidates(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"mocenter-agent-amd64", "mocenter-agent-arm64.exe",
		"mocenter-agent_1.0.1_amd64.deb", "mocenter-agent.sha256",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	d := New(Options{PkgDir: dir})
	got := d.PackageCandidates()
	if len(got) != 2 {
		t.Fatalf("应列出 2 个候选包，实际 %d: %+v", len(got), got)
	}
	archs := map[string]string{}
	for _, p := range got {
		archs[p.Name] = p.Arch
	}
	if archs["mocenter-agent-amd64"] != "amd64" {
		t.Errorf("amd64 架构推导不正确: %+v", archs)
	}
	if archs["mocenter-agent-arm64.exe"] != "arm64" {
		t.Errorf("arm64.exe 架构推导不正确: %+v", archs)
	}
}

// TestParseKeyVals 探测输出解析（含空值与多行）。
func TestParseKeyVals(t *testing.T) {
	out := "ARCH=x86_64\nUID=0\nOS=\nHOST=web\nSYSTEMD=yes\n\n"
	m := parseKeyVals(out)
	if m["ARCH"] != "x86_64" || m["UID"] != "0" || m["SYSTEMD"] != "yes" {
		t.Fatalf("解析结果不正确: %+v", m)
	}
	if v, ok := m["OS"]; !ok || v != "" {
		t.Fatalf("空值应存在且为空串，实际 %q ok=%v", v, ok)
	}
}

// jsonUnmarshal 局部包装，避免测试文件为了一处调用引入 encoding/json 的命名冲突。
func jsonUnmarshal(b []byte, v interface{}) error {
	return json.Unmarshal(b, v)
}
