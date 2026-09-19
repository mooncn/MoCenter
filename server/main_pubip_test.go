package main

import "testing"

// TestPublicIPFromEcho 覆盖回显服务响应解析：必须能取出公网 IP，
// 且把私网/回环/非法值挡掉 —— 误把 10.x 写进证书 SAN 比不写更糟
// （看着"配了"，实际任何公网 Agent 都校验不过）。
func TestPublicIPFromEcho(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"json ip", `{"ip":"82.156.7.85"}`, "82.156.7.85"},
		{"json query", `{"query":"82.156.7.85","country":"CN"}`, "82.156.7.85"},
		{"json ipv4", `{"IPv4":"82.156.7.85"}`, "82.156.7.85"},
		{"plain", "82.156.7.85", "82.156.7.85"},
		{"plain with newline", "82.156.7.85\n", "82.156.7.85"},
		{"quoted", `"82.156.7.85"`, "82.156.7.85"},
		{"ipv6", `{"ip":"2001:4860:4860::8888"}`, "2001:4860:4860::8888"},
		{"loopback rejected", `{"ip":"127.0.0.1"}`, ""},
		{"private rejected", `{"ip":"10.0.0.5"}`, ""},
		{"private 192 rejected", `{"ip":"192.168.1.1"}`, ""},
		{"garbage rejected", "not-an-ip", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := publicIPFromEcho(c.in); got != c.want {
				t.Errorf("publicIPFromEcho(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestListenAddrOf 覆盖 SAN 提示里用到的地址抽取。
func TestListenAddrOf(t *testing.T) {
	cases := map[string]string{
		":9000":        "",        // 通配监听：交给地址探测补齐
		"0.0.0.0:9000": "",        // 同上
		"[::]:9000":    "",        // 同上
		"1.2.3.4:9000": "1.2.3.4", // 显式绑定：就是 Agent 要连的地址
		"my.host:9000": "my.host",
		"1.2.3.4":      "1.2.3.4", // 无端口时原样返回
	}
	for in, want := range cases {
		if got := listenAddrOf(in); got != want {
			t.Errorf("listenAddrOf(%q) = %q, want %q", in, got, want)
		}
	}
}
