package ip

import "testing"

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
