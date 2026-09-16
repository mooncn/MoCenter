// Package geo 提供基于公网 IP 的地理位置解析服务。
//
// 使用免费且无需密钥的 ip-api.com（支持 IPv4/IPv6，返回国家/地区/城市与坐标）。
// 成功结果缓存 24 小时避免频繁请求；**失败结果只缓存 10 分钟**，因为
// 免费接口按分钟限流，长时间缓存失败会让"节点地区/国旗"一整天不显示。
// 请求失败时返回 nil，不影响 Agent 列表展示。
//
// 内网 / 保留地址（RFC1918、127.0.0.0/8、链路本地、CGNAT、IPv6 ULA 等）
// 直接判定为"无地区"，既不消耗外部额度，也不会让运维误以为是接口故障 ——
// 这类地址本身在公网上就没有地理位置。
package geo

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// ipapiResponse ip-api.com 响应结构。
type ipapiResponse struct {
	Status      string  `json:"status"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"regionName"`
	City        string  `json:"city"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Query       string  `json:"query"`
}

// Resolver 地理位置解析器。
type Resolver struct {
	mu      sync.Mutex
	cache   map[string]*cacheEntry
	timeout time.Duration
	// proxy 可选：国内加速代理前缀（形如 http://fa.mooncn.win/?url= ）
	proxy string
	// endpoint 查询模板（含一个 %s 占位 IP），仅测试注入用；为空时用 ip-api.com。
	endpoint string
}

// 缓存时长。
//
// 失败结果必须用**短 TTL**：ip-api.com 免费额度按分钟限流（约 45 次/分钟），
// 一批 Agent 同时上线时很容易触发 429。若把失败也缓存 24 小时，那么
// "节点地区 + 国旗"就会整整一天不显示（且重启 Server 前无法自愈），
// 这正是此前"地区/国旗不显示"的成因之一。短 TTL 让它在额度恢复后
// 自动补上，而不是等一天。
const (
	cacheOK       = 24 * time.Hour
	cacheFail     = 10 * time.Minute
	maxFailCached = 2048
)

type cacheEntry struct {
	geo      *protocol.GeoInfo
	expireAt time.Time
}

// NewResolver 创建解析器。
func NewResolver() *Resolver {
	return &Resolver{
		cache:   make(map[string]*cacheEntry),
		timeout: 5 * time.Second,
	}
}

// NewResolverWithProxy 创建带国内加速代理的解析器。
// proxy 形如 http://fa.mooncn.win/?url= （与 Agent 的 -proxy-ip 同格式）；
// 用于 Server 所在网络直连 ip-api.com 不稳定时的加速。
func NewResolverWithProxy(proxy string) *Resolver {
	r := NewResolver()
	r.proxy = proxy
	return r
}

// Private 判断 IP 是否为内网 / 保留地址（无公网地理位置）。
// 空串也返回 true：调用方据此跳过查询。
func Private(ip string) bool {
	if ip == "" {
		return true
	}
	// 带端口的形式先剥掉端口（IPv4 host:port）
	if i := strings.LastIndex(ip, ":"); i > 0 && strings.Count(ip, ":") == 1 {
		ip = ip[:i]
	}
	p := net.ParseIP(ip)
	if p == nil {
		return true // 无法解析的地址不查询
	}
	if p.IsLoopback() || p.IsPrivate() || p.IsLinkLocalUnicast() ||
		p.IsLinkLocalMulticast() || p.IsInterfaceLocalMulticast() ||
		p.IsMulticast() || p.IsUnspecified() {
		return true
	}
	// 255.255.255.255 广播地址
	if v4 := p.To4(); v4 != nil && v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255 {
		return true
	}
	// 100.64.0.0/10 运营商级 NAT (CGNAT)
	if v4 := p.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	// 192.0.2.0/24、198.51.100.0/24、203.0.113.0/24 文档示例地址
	if v4 := p.To4(); v4 != nil && v4[0] == 192 && v4[1] == 0 && v4[2] == 2 {
		return true
	}
	return false
}

// Lookup 解析给定 IP 的地理位置，带缓存。无法解析时返回 nil。
func (r *Resolver) Lookup(ip string) *protocol.GeoInfo {
	ip = trimIP(ip)
	// 内网 / 保留地址一律不查询：既省额度（避免限流让所有节点都查不到），
	// 也避免在日志里刷出一堆无意义的失败记录。
	if Private(ip) {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[ip]; ok && time.Now().Before(e.expireAt) {
		r.mu.Unlock()
		return e.geo
	}
	r.mu.Unlock()

	geo := r.fetch(ip)

	r.mu.Lock()
	if geo != nil {
		r.cache[ip] = &cacheEntry{geo: geo, expireAt: time.Now().Add(cacheOK)}
	} else {
		// 失败也缓存（否则每次拉主机列表都狂打外部接口），但只缓存很短时间。
		// 解析失败的累积（限流、断网、DNS 抖动）会白白撑大 map，定期清一次。
		if len(r.cache) >= maxFailCached {
			r.pruneLocked()
		}
		r.cache[ip] = &cacheEntry{geo: nil, expireAt: time.Now().Add(cacheFail)}
	}
	r.mu.Unlock()
	return geo
}

// pruneLocked 清理已过期条目（调用方须持有锁）。
func (r *Resolver) pruneLocked() {
	now := time.Now()
	for k, e := range r.cache {
		if now.After(e.expireAt) {
			delete(r.cache, k)
		}
	}
}

// fetch 实际请求 ip-api.com。
func (r *Resolver) fetch(ip string) *protocol.GeoInfo {
	client := &http.Client{Timeout: r.timeout}
	// http 批量接口免费；ip-api.com 免费版单 IP 查询需 http（避免 https 依赖）
	tmpl := r.endpoint
	if tmpl == "" {
		tmpl = "http://ip-api.com/json/%s?lang=zh-CN&fields=status,country,countryCode,regionName,city,lat,lon,query"
	}
	url := r.applyProxy(fmt.Sprintf(tmpl, ip))
	resp, err := client.Get(url)
	if err != nil {
		// 解析失败不影响主机列表展示，但必须留下痕迹：
		// 否则运维只能看到"地区/国旗是空的"，无从判断是限流、断网还是 IP 为空。
		log.Printf("[geo] 查询 %s 地理位置失败: %v", ip, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[geo] 查询 %s 地理位置失败: HTTP %d（ip-api.com 免费额度约 45 次/分钟，可能被限流）", ip, resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var pr ipapiResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil
	}
	if pr.Status != "success" {
		log.Printf("[geo] 查询 %s 地理位置未命中: status=%s（内网/保留地址无法解析）", ip, pr.Status)
		return nil
	}
	return &protocol.GeoInfo{
		Country:     pr.Country,
		CountryCode: pr.CountryCode,
		Region:      pr.Region,
		City:        pr.City,
		Latitude:    pr.Lat,
		Longitude:   pr.Lon,
	}
}

// applyProxy 按代理前缀包装 URL（格式与 Agent 端一致）。
func (r *Resolver) applyProxy(rawURL string) string {
	if r.proxy == "" {
		return rawURL
	}
	p := r.proxy
	if strings.Contains(p, "?url=") {
		return p + urlEncode(rawURL)
	}
	if strings.HasSuffix(p, "/") {
		return p + "?url=" + urlEncode(rawURL)
	}
	return p + "/?url=" + urlEncode(rawURL)
}

// urlEncode 最小转义，保留回显 URL 可读性（与 Agent 端同实现）。
func urlEncode(s string) string {
	return strings.NewReplacer(
		" ", "%20", "?", "%3F", "&", "%26", "=", "%3D",
		"#", "%23", "%", "%25",
	).Replace(s)
}

// trimIP 去除 IP 字符串中的端口与空白。
func trimIP(s string) string {
	s = trimSpace(s)
	if i := indexByte(s, ':'); i > 0 && stringsCountColon(s) == 1 {
		// 形如 host:port 的 IPv4
		s = s[:i]
	}
	return s
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func stringsCountColon(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			n++
		}
	}
	return n
}
