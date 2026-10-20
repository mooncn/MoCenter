// 反向代理下的"真实客户端地址"（L-7）。
//
// 本仓所有安全判定（IP 白名单、每 IP 限速、登录退避、公开页并发计数、审计里的
// 来源 IP）都以 **TCP 对端** 为准，这是刻意不信任 X-Forwarded-For 的结果 ——
// 那个头是客户端自己写的，照单全收等于让任何人都能伪造来源绕过白名单。
//
// 但"完全不信任"在反代部署下同样有害：所有访客在服务端看来都来自代理那一个地址，于是
//   - `-whitelist` 只对代理 IP 生效（要么整体放行、要么把后台关死）；
//   - 120 次/分的额度由全网访客共用，匿名公开页把管理员挡在自己的控制台外面。
//
// 所以这里给出**显式授权**的那一条路：运维用 `-trusted-proxies` 声明"我前面那层代理
// 是我自己的、它写的 XFF 可信"。只有 TCP 对端落在这个网段里时，才按 XFF 还原客户端地址；
// 不在其中就维持原样（与修复前完全一致）。默认空 = 不启用 = 行为不变。
package auth

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseProxyCIDRs 解析 "-trusted-proxies" 的取值（逗号分隔的 CIDR 或裸 IP）。
// 空串表示不启用；无法解析的条目直接报错 —— 静默丢掉一条 CIDR 会让运维以为已经放行，
// 而实际上判定仍按代理 IP 走，这类"配置写了不生效"比启动失败难查得多。
func ParseProxyCIDRs(spec string) ([]*net.IPNet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("可信代理 %q 既不是 IP 也不是 CIDR", item)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("可信代理网段 %q 无法解析: %w", item, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// TrustProxyClients 把"TCP 对端是可信代理"的请求还原成真实客户端地址。
//
// 做法是直接改写 r.RemoteAddr（保留原值于 X-MoCenter-Peer 之外的内存态里并不必要）：
// 下游有十几处各自取 IP（guardGate 的白名单与限速、登录退避、审计来源、WS/VNC 握手、
// 公开页并发计数），逐个改调用点必然漏一处 —— 而漏掉的那一处正好是最关键的那个。
// 集中在最外层改一次，所有判定口径自然一致。
//
// XFF 取值用"从右往左找第一个不在可信网段里的地址"，而不是最左值：最左值是
// 客户端自己写的，链路上任何一层代理被绕过时它就又是可伪造的；从右往左才是代理链
// 逐跳追加、由己方写入的那一段。
func TrustProxyClients(proxies []*net.IPNet, next http.Handler) http.Handler {
	if len(proxies) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerIP := parseIPOfAddr(r.RemoteAddr)
		if peerIP == nil || !inNets(proxies, peerIP) {
			next.ServeHTTP(w, r)
			return
		}
		if client := clientFromForwarded(r.Header.Get("X-Forwarded-For"), proxies); client != nil {
			r.RemoteAddr = net.JoinHostPort(client.String(), "0")
		}
		next.ServeHTTP(w, r)
	})
}

// clientFromForwarded 按"从右往左第一个不可信跳"取客户端地址；取不到返回 nil。
func clientFromForwarded(xff string, proxies []*net.IPNet) net.IP {
	if strings.TrimSpace(xff) == "" {
		return nil
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			// 链路上出现非 IP 的跳（异常/伪造内容）：到此为止，宁可维持代理 IP，
			// 也不要把一段垃圾当成客户端身份。
			return nil
		}
		if !inNets(proxies, ip) {
			return ip
		}
	}
	// 整串都在可信网段里（典型：本机直连 + 一层同机代理），取最右值。
	return net.ParseIP(strings.TrimSpace(parts[len(parts)-1]))
}

// RealIP 返回请求的客户端地址（去掉端口），口径与 TrustProxyClients 一致。
// 中间件已把 RemoteAddr 归一，因此这里只是 ClientIP 的别名，供可读性需要处调用。
func RealIP(r *http.Request) string {
	return ClientIP(r.RemoteAddr)
}

func inNets(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseIPOfAddr(remoteAddr string) net.IP {
	host := ClientIP(remoteAddr)
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	return net.ParseIP(strings.TrimSpace(host))
}
