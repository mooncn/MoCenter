package vnc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
	"mocenter/server/internal/ws"
)

// TestParsePortSpec 端口清单解析：闸门的安全价值全押在"配出来的区间就是想要的区间"上，
// 解析得比运维写的宽（或反过来静默变窄）都会让结论失效。
func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		name   string
		spec   string
		wantOK bool
		accept []int
		deny   []int
	}{
		{name: "默认区间", spec: DefaultPortSpec, wantOK: true, accept: []int{5900, 5901, 5999}, deny: []int{0, -1, 22, 5899, 6000}},
		{name: "区间与单端口混排", spec: "5900-5999,6900", wantOK: true, accept: []int{5900, 6900}, deny: []int{6899, 7000}},
		{name: "容忍空白", spec: " 5901 , 7000-7002 ", wantOK: true, accept: []int{5901, 7002}, deny: []int{5900, 7003}},
		{name: "只有逗号等同不限制", spec: ", ,", wantOK: true, accept: []int{1, 22, 65535}},
		{name: "空串表示不限制", spec: "", wantOK: true, accept: []int{1, 22, 65535}},
		{name: "非数字", spec: "abc", wantOK: false},
		{name: "区间起点为 0", spec: "0-10", wantOK: false},
		{name: "超出端口上限", spec: "70000", wantOK: false},
		{name: "区间反向", spec: "6000-5900", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ranges, ok := parsePortSpec(c.spec)
			if ok != c.wantOK {
				t.Fatalf("wantOK=%v，实际 %v（ranges=%v）", c.wantOK, ok, ranges)
			}
			if !ok {
				return
			}
			for _, p := range c.accept {
				if !portAllowed(ranges, p) {
					t.Errorf("%q 应放行端口 %d", c.spec, p)
				}
			}
			for _, p := range c.deny {
				if portAllowed(ranges, p) {
					t.Errorf("%q 不应放行端口 %d", c.spec, p)
				}
			}
		})
	}
}

// TestNewMgrDefaultsToGateClosedOnOddPorts 默认（不显式配置）就必须带闸门：
// 漏配 -vnc-ports 的后果应当是"连不上非常规端口"，而不是给每个登录会话
// 一台打到内网任意端口的代理。
func TestNewMgrDefaultsToGateClosedOnOddPorts(t *testing.T) {
	m := NewMgr(nil)
	if err := m.CheckPort(5900); err != nil {
		t.Fatalf("默认应放行 5900，实际 %v", err)
	}
	if err := m.CheckPort(5999); err != nil {
		t.Fatalf("默认应放行区间末端 5999，实际 %v", err)
	}
	err := m.CheckPort(22)
	if err == nil {
		t.Fatal("默认不应放行 22")
	}
	msg := err.Error()
	if !strings.Contains(msg, DefaultPortSpec) || !strings.Contains(msg, "-vnc-ports") {
		t.Fatalf("拒绝理由要带上当前区间与放行办法，实际：%s", msg)
	}
}

// TestSetAllowedPortsKeepsOldValueOnError 解析失败必须保持原值：
// 静默退化成"全部放行"是这个闸门最坏的失效方式。
func TestSetAllowedPortsKeepsOldValueOnError(t *testing.T) {
	m := NewMgr(nil)
	if err := m.SetAllowedPorts("6900"); err != nil {
		t.Fatalf("合法清单不应报错：%v", err)
	}
	if err := m.CheckPort(6900); err != nil {
		t.Fatalf("应放行 6900：%v", err)
	}
	if err := m.CheckPort(5900); err == nil {
		t.Fatal("收紧到 6900 后不应再放行 5900")
	}
	if err := m.SetAllowedPorts("not-a-port"); err == nil {
		t.Fatal("非法清单应返回错误")
	}
	if err := m.CheckPort(6900); err != nil {
		t.Fatalf("解析失败后应保持原清单，实际 %v", err)
	}
}

// TestSetAllowedPortsEmptyMeansUnrestricted 显式传空串是运维主动放开，
// 而默认值不是：两者都必须能测出来，否则"忘了配"和"故意放开"在行为上无法区分。
func TestSetAllowedPortsEmptyMeansUnrestricted(t *testing.T) {
	m := NewMgr(nil)
	if err := m.SetAllowedPorts(""); err != nil {
		t.Fatalf("空清单应被接受：%v", err)
	}
	for _, p := range []int{22, 3306, 6379} {
		if err := m.CheckPort(p); err != nil {
			t.Errorf("不限制时不应拒绝端口 %d：%v", p, err)
		}
	}
}

// TestHandleViewerRejectsPortBeforeUpgrade 闸门要在 Upgrade 与占用名额之前判掉。
// 放在后面有两个后果：101 之后发不出 429/400（浏览器只看到一条连不上的 WS），
// 以及每次被拒的握手都可能把一个名额留在计数里。
func TestHandleViewerRejectsPortBeforeUpgrade(t *testing.T) {
	m := NewMgr(session.NewManager())
	req := httptest.NewRequest(http.MethodGet, "/vnc-ws?agent=host-1&port=22", nil)
	req.Header.Set("Sec-WebSocket-Protocol", ws.WSSubprotocolName+",tok-portgate")
	rec := httptest.NewRecorder()
	m.HandleViewer(rec, req, func(string) bool { return true })

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界端口应在握手前被拒为 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "22") {
		t.Fatalf("拒绝理由要指出是哪个端口：%s", rec.Body.String())
	}
	if n := len(m.viewersBySs); n != 0 {
		t.Fatalf("被拒的握手不应留下并发名额，实际 %d 个键", n)
	}
}

// TestProbeAndInstallGateBeforeAgentLookup 探测与安装都要在"查主机是否在线"之前过闸门：
// 反之为离线主机构造请求就能绕过校验探到端口；同时端口 0 会被归一成 5900，
// 闸门必须校验归一后的值，否则传 0 就是一条绕过口（Agent 侧同样按 5900 处理）。
func TestProbeAndInstallGateBeforeAgentLookup(t *testing.T) {
	m := NewMgr(session.NewManager()) // 没有任何 Agent 在线

	if ok, msg := m.Probe("host-1", 6379); ok || !strings.Contains(msg, "6379") {
		t.Fatalf("探测应被端口闸门拦下，实际 ok=%v msg=%q", ok, msg)
	}
	if resp := m.Install("host-1", 6379, protocol.VncModeInteractive); resp.OK || !strings.Contains(resp.Message, "6379") {
		t.Fatalf("安装应被端口闸门拦下，实际 %+v", resp)
	}
	if _, msg := m.Probe("host-1", 0); !strings.Contains(msg, "离线") {
		t.Fatalf("端口 0 应归一成 5900 后通过闸门（默认区间内），实际 %q", msg)
	}

	if err := m.SetAllowedPorts("6379"); err != nil {
		t.Fatalf("配置失败：%v", err)
	}
	if _, msg := m.Probe("host-1", 0); !strings.Contains(msg, "6379") {
		t.Fatalf("收紧到 6379 后，归一成 5900 的请求也应被拦下，实际 %q", msg)
	}
	if _, msg := m.Probe("host-1", 6379); !strings.Contains(msg, "离线") {
		t.Fatalf("放行端口应走到在线判定，实际 %q", msg)
	}
}
