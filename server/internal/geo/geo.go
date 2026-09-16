// Package geo 提供基于公网 IP 的地理位置解析服务。
//
// 采用**多源解析 + 逐源降级**，顺序即优先级：
//
//  0. **本地离线 MMDB**（可选，配置 mmdb_path 后启用）
//     —— GeoLite2 / DB-IP / IPinfo 等 MaxMind DB 格式，本地文件查询，
//     微秒级、无网络、无配额限制。放在最前是因为它同时解决三个真实问题：
//     服务端无外网出口、免费接口按分钟限流（批量上线时互相挤额度）、
//     以及 api.iping.cc 对 IPv6 恒返回 500。
//  1. api.iping.cc（主源）https://api.iping.cc/v1/query?ip=..&language=zh
//     —— 返回中文国家/省/市名，偶发限流（code=500）。
//  2. ip-api.com（备源）http://ip-api.com/json/<ip>?lang=zh-CN —— 主源失败/限流时兜底。
//  3. ipwho.is（兜底）https://ipwho.is/<ip> —— 主源与备源都不可用时的最后一道。
//
// **未配置离线库时行为与从前完全一致**（三源降级），离线库是纯增强：
// 它是可选的，缺了不会让服务端起不来，也不会改变既有解析语义。
//
// 关键：**主源不返回 ISO 国家代码**（只有中文国名），而前端的国旗完全依赖
// country_code。早期的实现只看主源，于是"地区正常但国旗不显示/地区为空"。
// 这里通过「中英文国名表 + 备源返回的 countryCode」补齐国家代码，
// 并把中文国家名规范化为英文名（两个源口径不一致会让同一批节点看起来像两个国家）。
//
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
	"strconv"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// ipapiResponse ip-api.com 响应结构。
type ipapiResponse struct {
	Status      string  `json:"status"`
	Message     string  `json:"message"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"regionName"`
	City        string  `json:"city"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Query       string  `json:"query"`
}

// ipingResponse api.iping.cc 响应结构。
//
// 注意：该接口**没有 country_code 字段**（只有中文国名），经纬度是字符串，
// 且 IPv6 查询会返回 code=500（"数据升级中"）—— 这些都是必须处理的现实差异。
type ipingResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Lon     string `json:"longitude"`
		Lat     string `json:"latitude"`
		// ASCountry 是**归属国家**的 ISO 代码（as_country / company_country），
		// 不是"这一跳出口在哪"。与 country（按 IP 注册地解析出的国名）口径一致，
		// 因此可以直接用来补 country_code —— 主源本身不返回 IP 所属国家代码。
		ASCountry      string `json:"as_country"`
		CompanyCountry string `json:"company_country"`
	} `json:"data"`
}

// ipwhoResponse ipwho.is 响应结构（兜底源，自带 ISO 国家代码）。
type ipwhoResponse struct {
	Success     bool    `json:"success"`
	Message     string  `json:"message"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

// 默认查询模板（可在配置/测试中覆盖）。
const (
	// defaultIpingEndpoint api.iping.cc 主源：中文国家/省/市，无 ISO 国家代码。
	defaultIpingEndpoint = "https://api.iping.cc/v1/query?ip=%s&language=zh"
	// defaultIPAPIEndpoint ip-api.com 备源：带 countryCode，中文（zh-CN）返回中文国名。
	defaultIPAPIEndpoint = "http://ip-api.com/json/%s?lang=zh-CN&fields=status,message,country,countryCode,regionName,city,lat,lon,query"
	// defaultIPWhoEndpoint ipwho.is 兜底源：HTTPS，带 country_code。
	defaultIPWhoEndpoint = "https://ipwho.is/%s"
)

// Resolver 地理位置解析器。
type Resolver struct {
	mu      sync.Mutex
	cache   map[string]*cacheEntry
	timeout time.Duration
	// proxy 可选：国内加速代理前缀（形如 http://fa.mooncn.win/?url= ）
	proxy string
	// ipingEndpoint / ipapiEndpoint / ipwhoEndpoint 各源查询模板（含一个 %s 占位 IP）。
	// 为空时用内置默认值；测试通过覆盖它们注入 httptest 服务。
	ipingEndpoint string
	ipapiEndpoint string
	ipwhoEndpoint string
	// 兼容字段：早期的单源测试注入点。设置后作为备源（ip-api 口径）生效。
	endpoint string
	// mmdb 可选离线地理库（nil 或未加载 = 未启用，直接跳过该源）。
	mmdb *MMDBProvider
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
		mmdb:    &MMDBProvider{},
	}
}

// SetMMDB 挂载离线地理库（path 为空表示不启用）。
//
// 返回错误仅用于**日志提示**（配置了但加载失败），不阻断启动：
// 离线库加载失败时自动降级到外部 HTTP 源，服务端照常可用。
func (r *Resolver) SetMMDB(path string) error {
	if r.mmdb == nil {
		r.mmdb = &MMDBProvider{}
	}
	if trimSpace(path) == "" {
		return nil
	}
	return r.mmdb.Reopen(path)
}

// MMDBLoaded 离线库是否已生效（供启动日志与 /api/version 展示）。
func (r *Resolver) MMDBLoaded() bool {
	return r.mmdb != nil && r.mmdb.Loaded()
}

// MMDBPath 当前生效的离线库路径（未启用时为空）。
func (r *Resolver) MMDBPath() string {
	if r.mmdb == nil {
		return ""
	}
	return r.mmdb.Path()
}

// ReloadMMDB 热重载离线库（更新数据文件后无需重启服务端）。
func (r *Resolver) ReloadMMDB(path string) error {
	return r.SetMMDB(path)
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

// fetch 依次尝试所有解析源，返回首个成功结果。
//
// 顺序即优先级，但**顺序本身是动态的**：
//
//   - IPv6 直接跳过 api.iping.cc。该源对 IPv6 恒返回 code=500
//     （"IPv6 数据正在升级优化中"），排在前面只会白白多一次失败往返、
//     多一条误导性的错误日志，还拖慢每个 IPv6 节点的首次解析。
//   - 其余情况按 中文口径 → 备源 → 兜底 排列。
//
// 每个源的失败都单独记日志（含原因），这样运维能从日志判断到底是
// "限流"、"网络不通"、"不支持该地址族" 还是 "该 IP 无地理位置数据"。
func (r *Resolver) fetch(ip string) *protocol.GeoInfo {
	// 离线库优先：本地文件查询无网络、无配额，能命中就不必打外部接口。
	// 这也顺带避免了"批量上线打爆免费额度"——有库的部署几乎不会走 HTTP 源。
	if r.mmdb != nil && r.mmdb.Loaded() {
		if g := r.mmdb.Lookup(ip); g != nil {
			return g
		}
	}
	client := &http.Client{Timeout: r.timeout}
	type source struct {
		name string
		tmpl string
		fn   func([]byte) *protocol.GeoInfo
	}
	sources := []source{
		{"ip-api.com", r.ipapiURL(), r.parseIPAPI},
		{"ipwho.is", r.ipwhoURL(), r.parseIPWho},
	}
	if !strings.Contains(ip, ":") {
		// 仅 IPv4 走主源（中文国家/省市口径最好）
		sources = append([]source{{"api.iping.cc", r.ipingURL(), r.parseIping}}, sources...)
	}
	for _, src := range sources {
		body, status, err := r.get(client, fmt.Sprintf(src.tmpl, ip))
		if err != nil {
			log.Printf("[geo] %s 查询 %s 失败: %v", src.name, ip, err)
			continue
		}
		if status != http.StatusOK {
			log.Printf("[geo] %s 查询 %s 失败: HTTP %d（免费接口通常按分钟限流）", src.name, ip, status)
			continue
		}
		if g := src.fn(body); g != nil {
			return normalizeGeo(g)
		}
	}
	return nil
}

// get 执行一次 GET，并做代理前缀包装。
func (r *Resolver) get(client *http.Client, rawURL string) ([]byte, int, error) {
	resp, err := client.Get(r.applyProxy(rawURL))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// ipingURL / ipapiURL / ipwhoURL 返回各源生效的查询模板。
func (r *Resolver) ipingURL() string {
	if r.ipingEndpoint != "" {
		return r.ipingEndpoint
	}
	return defaultIpingEndpoint
}

func (r *Resolver) ipapiURL() string {
	// endpoint 是早期的单源注入点（ip-api 口径），保留以兼容既有测试
	if r.ipapiEndpoint != "" {
		return r.ipapiEndpoint
	}
	if r.endpoint != "" {
		return r.endpoint
	}
	return defaultIPAPIEndpoint
}

func (r *Resolver) ipwhoURL() string {
	if r.ipwhoEndpoint != "" {
		return r.ipwhoEndpoint
	}
	return defaultIPWhoEndpoint
}

// parseIping 解析 api.iping.cc 响应。
//
// 现实约束：
//   - 无 ISO 国家代码 —— 只能给出中文国家名，国家代码交给 normalizeGeo 补；
//   - 经纬度是字符串，解析失败按 0 处理（公开页本来就不展示经纬度）；
//   - 限流/不支持 IPv6 时返回 {"code":500,"msg":"..."}，此时返回 nil 交由下一个源。
func (r *Resolver) parseIping(body []byte) *protocol.GeoInfo {
	var pr ipingResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil
	}
	if pr.Code != 200 || pr.Data == nil {
		// code=500 有两种截然不同的含义，日志必须区分，否则运维会误判：
		//   - msg 含 "frequent" → 按分钟限流（等一会儿自愈）；
		//   - msg 含 "IPv6"     → 该源不支持 IPv6（永远查不到，应换源）。
		log.Printf("[geo] api.iping.cc 未命中: code=%d msg=%s", pr.Code, pr.Msg)
		return nil
	}
	d := pr.Data
	if d.Country == "" && d.Region == "" && d.City == "" {
		return nil
	}
	return &protocol.GeoInfo{
		Country:     d.Country,
		CountryCode: firstNonEmpty(d.ASCountry, d.CompanyCountry),
		Region:      d.Region,
		City:        d.City,
		Latitude:    parseFloat(d.Lat),
		Longitude:   parseFloat(d.Lon),
	}
}

// firstNonEmpty 返回第一个非空值（用于可选字段的降级取值）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseIPAPI 解析 ip-api.com 响应（带 ISO 国家代码）。
func (r *Resolver) parseIPAPI(body []byte) *protocol.GeoInfo {
	var pr ipapiResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil
	}
	if pr.Status != "success" {
		log.Printf("[geo] ip-api.com 未命中: status=%s message=%s", pr.Status, pr.Message)
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

// parseIPWho 解析 ipwho.is 响应（带 ISO 国家代码，HTTPS）。
func (r *Resolver) parseIPWho(body []byte) *protocol.GeoInfo {
	var pr ipwhoResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil
	}
	if !pr.Success {
		log.Printf("[geo] ipwho.is 未命中: %s", pr.Message)
		return nil
	}
	return &protocol.GeoInfo{
		Country:     pr.Country,
		CountryCode: pr.CountryCode,
		Region:      pr.Region,
		City:        pr.City,
		Latitude:    pr.Latitude,
		Longitude:   pr.Longitude,
	}
}

// normalizeGeo 统一两个源的输出口径，并补齐国家代码。
//
// 为什么必须做：
//  1. 主源不返回 ISO 国家代码，而前端的国旗完全由 country_code 生成 ——
//     不补就是"地区有字、国旗空白"，正是此前反馈的问题之一。
//  2. 两个源的国家名口径不同（"中国" vs "China"、"美国" vs "United States"），
//     不统一会让同一批节点在页面上看起来像来自不同国家。
//
// 处理顺序：优先用英文/中文国名反查 ISO 代码；反查不到时保留原值
// （宁可只有国家名，也不要把国家写错）。
func normalizeGeo(g *protocol.GeoInfo) *protocol.GeoInfo {
	if g == nil {
		return nil
	}
	name := trimSpace(g.Country)
	code := strings.ToUpper(trimSpace(g.CountryCode))

	if name != "" {
		if c, en, ok := lookupCountry(name); ok {
			if code == "" {
				code = c
			}
			// 统一为英文名：前端只需一种口径，也便于与 country_code 对应
			if en != "" && countryNameIsChinese(name) {
				g.Country = en
			} else if en != "" {
				g.Country = en
			}
		}
	}
	if len(code) != 2 {
		code = ""
	}
	g.CountryCode = code
	g.Region = trimSpace(g.Region)
	g.City = trimSpace(g.City)
	return g
}

// countryNameIsChinese 判断国名是否包含中文字符（用于决定是否需要换成英文名）。
func countryNameIsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// parseFloat 宽松解析浮点（接口返回的是字符串）。
func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(trimSpace(s), 64)
	return f
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
