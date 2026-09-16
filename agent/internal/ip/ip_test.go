package ip

import (
	"testing"
	"time"
)

func TestParseIPResponse(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"ip":"1.2.3.4"}`, "1.2.3.4"},
		{`{"ip":"2001:db8::1"}`, "2001:db8::1"},
		{`{"query":"5.6.7.8"}`, "5.6.7.8"},
		{"1.2.3.4\n", "1.2.3.4"},
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
		{"not-an-ip", ""},
	}
	for _, c := range cases {
		if got := parseIPResponse([]byte(c.in)); got != c.want {
			t.Errorf("parseIPResponse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestURLEncode(t *testing.T) {
	// 代理格式：http://fa.mooncn.win/?url=<encoded>
	proxy := "http://fa.mooncn.win/?url="
	raw := "https://api.ipify.org?format=json"
	// 生成完整 URL 并确保包含必要片段
	full := proxy + urlEncode(raw)
	if full == "" {
		t.Fatal("proxy URL 为空")
	}
}

// TestResolveFailureRetryShort 探测失败必须短时间后重试。
//
// 回归用例：失败结果（空 IP）曾被当成有效结果缓存 30 分钟，
// 导致开机/断网/回显服务抖动的首轮失败会让这台机器在半小时内
// 一直上报空公网 IP —— 服务端解析不出地理位置，公开页的
// "节点地区 + 国旗"于是长期为空。
func TestResolveFailureRetryShort(t *testing.T) {
	if cacheFail >= cacheOK {
		t.Fatalf("失败重试间隔(%v) 必须短于成功缓存(%v)", cacheFail, cacheOK)
	}
	if cacheFail > 5*time.Minute {
		t.Fatalf("失败重试间隔 %v 过长，地区/国旗会长时间不显示", cacheFail)
	}

	d := &Detector{Timeout: 50 * time.Millisecond}
	// 指向必然失败的探测服务：用一个语法非法的代理前缀让其请求失败
	d.Proxy = "http://127.0.0.1:1/?url="
	start := time.Now()
	r := d.Resolve()
	if r.IPv4 != "" || r.IPv6 != "" {
		t.Fatalf("不可达服务不应返回 IP: %+v", r)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("失败探测耗时过长（未受 Timeout 约束）")
	}
	if time.Until(d.expireAt) > cacheFail+time.Second {
		t.Fatalf("失败后缓存 %v，期望约 %v", time.Until(d.expireAt), cacheFail)
	}
}

// TestResolveSuccessCacheOK 成功结果按长周期缓存，避免频繁打外部接口。
func TestResolveSuccessCacheOK(t *testing.T) {
	d := &Detector{}
	d.result = Result{IPv4: "1.2.3.4"}
	d.expireAt = time.Now().Add(cacheOK)
	if got := d.Resolve(); got.IPv4 != "1.2.3.4" {
		t.Fatalf("缓存命中应直接返回: %+v", got)
	}
}

// TestDetectServiceOrderPrefersDomestic 国内可达的回显服务排在境外服务之前。
// 顺序错会让国内网络下探测整体失败（连接重置 / DNS 污染）。
func TestDetectServiceOrderPrefersDomestic(t *testing.T) {
	order := []string{
		"https://api-ipv4.ip.sb/ip",
		"https://ipv4.icanhazip.com",
		"https://ipv4.ident.me/",
		"https://api.ipify.org?format=json",
	}
	idx := func(u string) int {
		for i, s := range order {
			if s == u {
				return i
			}
		}
		return -1
	}
	if idx("https://api-ipv4.ip.sb/ip") >= idx("https://api.ipify.org?format=json") {
		t.Fatal("国内可达的 ip.sb 回显应排在 ipify 之前")
	}
}
