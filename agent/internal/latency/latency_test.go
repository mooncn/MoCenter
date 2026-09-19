package latency

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTargetID 目标标识必须**完全由目标定义决定**（不含本机变量）：
// 它是上报 map 的 key，也是前端展示的列名，同一目标在不同主机上必须得到
// 同一个 key，否则历史曲线对不上。
func TestTargetID(t *testing.T) {
	cases := []struct {
		t    Target
		want string
	}{
		{Target{Kind: "icmp", Addr: "1.1.1.1"}, "icmp:1.1.1.1"},
		{Target{Kind: "tcp", Addr: "1.1.1.1:443"}, "tcp:1.1.1.1:443"},
		{Target{Kind: "http", Addr: "https://example.com"}, "http:https://example.com"},
		{Target{Kind: "ICMP", Addr: " 8.8.8.8 "}, "icmp:8.8.8.8"},
		{Target{Kind: "https", Addr: "x"}, "http:x"},
		{Target{Kind: "weird", Addr: "x"}, "icmp:x"},
	}
	for _, c := range cases {
		if got := c.t.ID(); got != c.want {
			t.Errorf("ID() = %q，期望 %q", got, c.want)
		}
	}
}

// TestParseTargets 解析服务端/配置文件下发的目标规格。
//
// 关键陷阱：tcp 与 http 目标的地址自身含冒号，不能简单按第一个冒号切分。
func TestParseTargets(t *testing.T) {
	got := ParseTargets([]string{
		"icmp:1.1.1.1",
		"tcp:1.1.1.1:443",
		"http:https://example.com/path",
		" 8.8.8.8 ",    // 无 kind 前缀 -> icmp
		"icmp:1.1.1.1", // 重复 -> 去重
		"",             // 空 -> 忽略
	})
	if len(got) != 4 {
		t.Fatalf("应解析出 4 个目标，实际 %d: %+v", len(got), got)
	}
	if got[1].Kind != "tcp" || got[1].Addr != "1.1.1.1:443" {
		t.Fatalf("tcp 目标解析错误: %+v", got[1])
	}
	if got[2].Kind != "http" || got[2].Addr != "https://example.com/path" {
		t.Fatalf("http 目标解析错误: %+v", got[2])
	}
	if got[3].Kind != "icmp" || got[3].Addr != "8.8.8.8" {
		t.Fatalf("无前缀目标应按 icmp 处理: %+v", got[3])
	}
}

// TestParseTargetsCap 目标数量必须有上限（防御配置被写坏打出上百个进程）。
func TestParseTargetsCap(t *testing.T) {
	specs := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		specs = append(specs, "icmp:10.0.0."+itoa(i%256))
	}
	got := ParseTargets(specs)
	if len(got) > maxTargets {
		t.Fatalf("目标数应被限制在 %d 以内，实际 %d", maxTargets, len(got))
	}
}

// TestProbeTCP 真实起一个 TCP 监听，验证能测出延迟。
func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p := New()
	p.Timeout = 2 * time.Second
	ms := p.ProbeAll([]Target{{Kind: "tcp", Addr: ln.Addr().String()}})
	if len(ms) != 1 {
		t.Fatalf("应探到 1 个目标，实际 %d: %+v", len(ms), ms)
	}
	for k, v := range ms {
		if k != "tcp:"+ln.Addr().String() {
			t.Errorf("key 不正确: %q", k)
		}
		// 本机回环应该在 100ms 以内；这里给宽松上限只为挡住"秒级"的错误实现
		if v < 0 || v > 500 {
			t.Errorf("本机 TCP 延迟异常: %v ms", v)
		}
	}
}

// TestProbeHTTP 真实起一个 HTTP 服务，验证能测出首字节往返。
func TestProbeHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	p := New()
	p.Timeout = 2 * time.Second
	ms := p.ProbeAll([]Target{{Kind: "http", Addr: srv.URL}})
	if len(ms) != 1 {
		t.Fatalf("应探到 1 个目标，实际 %+v", ms)
	}
}

// TestProbeHTTPNon2xxStillMeasured 4xx/5xx 只要拿到响应就算可达。
//
// 理由：这条链路测的是"网络是否可达、往返多久"，而不是"业务是否正常"。
// 把 403 当成"探测失败"会让"有防火墙但链路正常"的机器显示成超时。
func TestProbeHTTPNon2xxStillMeasured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	p := New()
	p.Timeout = 2 * time.Second
	ms := p.ProbeAll([]Target{{Kind: "http", Addr: srv.URL}})
	if len(ms) != 1 {
		t.Fatalf("403 也应被记为一个有效的延迟点，实际 %+v", ms)
	}
}

// TestProbeFailureNotZero **超时目标必须缺席结果 map**，不能写成 0ms。
//
// 这是本包最重要的一条契约：把失败写成 0 会让"完全不通的机器"
// 在延迟曲线上看起来比谁都快 —— 这是监控系统里最危险的一类显示错误。
func TestProbeFailureNotZero(t *testing.T) {
	p := New()
	p.Timeout = 500 * time.Millisecond
	// 192.0.2.0/24 是 RFC5737 文档保留段，必然不可达
	ms := p.ProbeAll([]Target{
		{Kind: "tcp", Addr: "192.0.2.1:1"},
		{Kind: "http", Addr: "http://192.0.2.1:1/"},
	})
	if len(ms) != 0 {
		t.Fatalf("不可达目标不应出现在结果中，实际 %+v", ms)
	}
}

// TestProbeAllEmpty 空目标列表返回 nil（而不是空 map）：
// 调用方据此判断"未配置延迟探测"，从而不上报任何延迟字段。
func TestProbeAllEmpty(t *testing.T) {
	if got := New().ProbeAll(nil); got != nil {
		t.Fatalf("空目标应返回 nil，实际 %+v", got)
	}
}

// TestProbeUnreachableICMP ICMP 到保留地址必然失败，不得返回 0。
func TestProbeUnreachableICMP(t *testing.T) {
	p := New()
	p.Timeout = 1 * time.Second
	ms := p.ProbeAll([]Target{{Kind: "icmp", Addr: "192.0.2.1"}})
	// 允许"环境里 ping 命令不存在"导致的失败，两者都必须是"不返回结果"
	if len(ms) != 0 {
		t.Fatalf("不可达目标不应出现在结果中，实际 %+v", ms)
	}
}

// TestProbeICMPLocalhost 若系统有 ping，本机应能探到（用于确认解析逻辑可用）。
func TestProbeICMPLocalhost(t *testing.T) {
	p := New()
	p.Timeout = 2 * time.Second
	ms := p.ProbeAll([]Target{{Kind: "icmp", Addr: "127.0.0.1"}})
	if len(ms) == 0 {
		t.Skip("当前环境没有可用的 ping 命令或不允许 ICMP，跳过")
	}
	for k, v := range ms {
		if v < 0 || v > 1000 {
			t.Errorf("%s 的本机延迟异常: %v", k, v)
		}
	}
}

// TestProbeConcurrent 多目标并发探测，结果集互不干扰（-race 下有价值）。
func TestProbeConcurrent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p := New()
	p.Timeout = 2 * time.Second
	targets := []Target{
		{Kind: "tcp", Addr: ln.Addr().String()},
		{Kind: "tcp", Addr: ln.Addr().String()},
		{Kind: "icmp", Addr: "192.0.2.1"},
	}
	ms := p.ProbeAll(targets)
	// 两个相同目标去重后只算一个（调用方应先 ParseTargets 去重），
	// 这里只验证不会因为并发写 map 而 panic
	for k := range ms {
		if !strings.Contains(k, "127.0.0.1") {
			t.Errorf("意外的 key: %q", k)
		}
	}
}

// TestFormatTargets 上报的目标列表顺序必须稳定（前端按它渲染列）。
func TestFormatTargets(t *testing.T) {
	got := FormatTargets([]Target{
		{Kind: "icmp", Addr: "1.1.1.1"},
		{Kind: "tcp", Addr: "1.1.1.1:443"},
		{Kind: "http", Addr: ""}, // 空地址应被跳过
	})
	want := []string{"icmp:1.1.1.1", "tcp:1.1.1.1:443"}
	if len(got) != len(want) {
		t.Fatalf("FormatTargets 长度应为 %d，实际 %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FormatTargets[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestNormalizeURL 缺 scheme 时按 https 处理。
func TestNormalizeURL(t *testing.T) {
	if got := normalizeURL("example.com"); got != "https://example.com" {
		t.Errorf("normalizeURL = %q", got)
	}
	if got := normalizeURL("http://a/b"); got != "http://a/b" {
		t.Errorf("已有 scheme 不应被改写: %q", got)
	}
	if got := normalizeURL("  "); got != "" {
		t.Errorf("空输入应返回空: %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
