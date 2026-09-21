package ws

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SameOrigin 判断请求的 Origin 是否就是本服务自身。
//
// 为什么必须自己判：浏览器的 WebSocket 握手**不受 CORS 约束**，任何页面都能
// 向本机服务建一条长连接；跨站劫持唯一挡得住的地方是服务端认来源。
// H-2 之后凭证只走子协议头，攻击页面拿不到运维的 Token，这条从"主要防线"
// 降级为"第二道"—— 但两道本来就是不同故障域：Token 泄漏（扩展、XSS、截图）
// 与页面被诱导建连是两回事。
//
// 允许"没有 Origin 头"：那只能是浏览器以外的客户端（curl、探测脚本），
// 而跨站劫持必须从浏览器页面发起、必然带上 Origin。这与 gorilla 默认的
// checkSameOrigin 语义一致，避免把非浏览器客户端一起拍死。
//
// 比对规则（不严格的地方都是为了让常见部署形态活着）：
//   - 主机名必须相等（大小写不敏感），`mc.example.com.evil.cn` 这类后缀伪装拒掉；
//   - 端口只在**两侧都显式写出**时才比对。TLS 终止型反向代理下服务端看到的是
//     http + 不含端口的 Host，而浏览器发的 Origin 是 https://主机（隐含 443）；
//     只要有一侧隐含端口就不比。若改成从 r.TLS 推断协议再补默认端口，就等于
//     要求代理额外透传 X-Forwarded-Proto，而"保留 Host"才是部署文档的前提。
func SameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// "null"（沙箱 iframe / file: 页面）解析不出主机，同样落在这里被拒。
		return false
	}
	oh, op, err := net.SplitHostPort(u.Host)
	if err != nil {
		oh, op = u.Host, ""
	}
	rh, rp, err := net.SplitHostPort(r.Host)
	if err != nil {
		rh, rp = r.Host, ""
	}
	if !strings.EqualFold(rh, oh) {
		return false
	}
	// 两侧都写了端口才比端口；任一侧隐含（默认端口/被代理吞掉）时放行。
	if op != "" && rp != "" {
		return op == rp
	}
	return true
}
