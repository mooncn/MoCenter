// 公开站点（免登录）与展示配置的后台接口。
//
// 对应 Issue #5：未登录访客可看主页上的**非敏感**信息（卡片/列表 + 节点详情历史趋势），
// 登录后台可配置「总开关 / 主页是否展示主机 / 逐字段开关」。
//
// 安全模型（本文件是唯一的对外暴露面，改动必须保持以下不变量）：
//
//  1. 公共端点只输出 internal/publicview 的脱敏结构，绝不返回 protocol.AgentInfo；
//  2. 公共端点的主机标识是用部署私钥派生的节点编号 n-xxxxxxxxxxxxxxxx（见 publicview.NodeKeys），
//     agent_id / uuid / 内网地址永不出网，编号也无法离线穷举命中；
//  3. 所有公共端点都先检查总开关，关闭时一律 404 —— 未启用时公开页面对外**不存在**；
//  4. 公共端点走独立的、更严的限流（与 /api/login 同一套守卫但键前缀不同），
//     避免匿名的列表/历史查询把服务端拖垮，也避免成为扫描器的免费接口。
package main

import (
	"encoding/json"
	"fmt"
	"mocenter/server/internal/logx"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/publicview"
	"mocenter/server/internal/siteconfig"
	"mocenter/server/internal/ws"
)

// publicAPIBase 公共接口前缀。前端用它判断"公开页是否可用"。
const publicAPIBase = "/api/public"

// publicSamplesMax 公共历史趋势单次返回的最大采样点（比后台更小：匿名入口控制带宽）。
const publicSamplesMax = 300

// publicRangeMaxSec 公共历史趋势允许的最大回溯窗口（30 天，与探针历史留存一致）。
const publicRangeMaxSec = 30 * 24 * 3600

// publicSite 公共端点的依赖集合。
type publicSite struct {
	store    *probe.Store
	cfg      *siteconfig.Store
	guard    *auth.LoginGuard
	hub      *ws.Hub
	authz    *auth.Auth
	upgrader websocket.Upgrader
	// nodeKeys 派生公开 node_id 的部署私钥（见 publicview.NodeKeys）。
	// 它是**匿名可达接口上唯一的单向性来源**，因此必须来自数据目录、不得写死在代码里。
	nodeKeys *publicview.NodeKeys
	// 匿名 WS 并发计数（见 acquireWS）：限流器只管频率，管不住"开一条长期挂着不放"。
	wsMu    sync.Mutex
	wsTotal int
	wsPerIP map[string]int
	// cfgSigMu/cfgSig 是"公开页配置变了"的广播信号，用在途匿名连接核对开关。
	cfgSigMu sync.Mutex
	cfgSig   chan struct{}
}

// subscribeCfgChange 取当前"配置已变更"信号通道（连接进入时取一次）。
//
// 惰性建道：`publicSite` 在测试里以结构体字面量构造，不能要求一定走过构造函数。
func (p *publicSite) subscribeCfgChange() <-chan struct{} {
	p.cfgSigMu.Lock()
	defer p.cfgSigMu.Unlock()
	if p.cfgSig == nil {
		p.cfgSig = make(chan struct{})
	}
	return p.cfgSig
}

// noteConfigChange 通知所有在途匿名连接重新核对开关。
//
// 只发信号、不搬数据：新状态由调用方紧跟着的那次 broadcastAgents 走原有订阅
// 通道下发。这里回答的只有一个问题 —— 开关是不是已经关了（M-14：原来"停用"
// 只挡住新握手，已建立的匿名连接会永远看下去）。
func (p *publicSite) noteConfigChange() {
	p.cfgSigMu.Lock()
	old := p.cfgSig
	p.cfgSig = make(chan struct{})
	p.cfgSigMu.Unlock()
	if old != nil {
		close(old)
	}
}

// publicEnabled 读取总开关。
func (p *publicSite) publicEnabled() bool {
	cfg := p.cfg.Get()
	return cfg != nil && cfg.Enabled
}

// requireEnabled 所有公共端点的第一道闸门：总开关关闭即 404。
//
// 用 404 而不是 403：未启用时该功能对外应当"不存在"，避免向扫描器确认
// "这里有个可以打开的开关"。
func (p *publicSite) requireEnabled(w http.ResponseWriter) bool {
	if !p.publicEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// allow 匿名限流：按「来源 IP + 路由名」限制频率。
//
// 桶与登录**完全分开**（`AllowPublicRoute`，M-12）：公开详情页每 15s 轮询一次，
// 三个标签页就够把共用 20/分/IP 的登录额度吃光 —— 那等于让访客把管理员挡在
// 自己的后台外面。分路由还顺带挡住了"一个路由打满全站"的自锁。
func (p *publicSite) allow(w http.ResponseWriter, r *http.Request, route string) bool {
	if p.guard == nil {
		return true
	}
	if ok, retry := p.guard.AllowPublicRoute(r.RemoteAddr, route); !ok {
		w.Header().Set("Retry-After", auth.RetryAfter(retry))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	return true
}

// stateHandler GET /api/public/state
//
// 返回：总开关状态、节点列表与汇总。未登录访客靠这一个接口即可渲染整个公开首页。
//
// 不下发 publicFieldCatalog()（L-4）：那份目录含每个字段的 sensitive 标记与说明，
// 等于向任何扫到这条匿名端点的人公布"后台展示开关清单长什么样"。前端渲染只需要
// config.fields 这个"哪些项可见"的键值表（PublicStatus.jsx 里的 visible()），
// 目录本身只有登录后的配置页 /api/site/config 才用得上。
func (p *publicSite) stateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			return
		}
		if !p.allow(w, r, "state") {
			return
		}
		cfg := p.cfg.Get()
		nodes := p.visibleNodes(cfg)
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"enabled": true,
			"config":  publicConfigPayload(cfg),
			"nodes":   nodes,
			"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
		})
	}
}

// nodesHandler GET /api/public/nodes
// 仅节点列表（前端轮询刷新用，载荷比 state 小）。
func (p *publicSite) nodesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			return
		}
		if !p.allow(w, r, "nodes") {
			return
		}
		cfg := p.cfg.Get()
		nodes := p.visibleNodes(cfg)
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"nodes":   nodes,
			"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
		})
	}
}

// samplesHandler GET /api/public/samples?node=n-xxxxxxxxxxxxxxxx&range=24h
//
// 节点历史趋势。node 为派生节点编号 —— 服务端在本文件内用部署私钥把它与
// agent_id 常量时间比对（映射只覆盖**当前可公开**的主机），
// 因此伪造一个 node 也无法读取未公开主机的历史数据。
func (p *publicSite) samplesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			return
		}
		if !p.allow(w, r, "samples") {
			return
		}
		cfg := p.cfg.Get()
		nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
		if nodeID == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		agentID := ""
		for _, a := range p.store.AllCached() {
			if a == nil || !cfg.HostAllowed(a.AgentID) {
				continue
			}
			if p.nodeKeys.Match(a.AgentID, nodeID) {
				agentID = a.AgentID
				break
			}
		}
		if agentID == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		window := parsePublicRange(r.URL.Query().Get("range"))
		maxPoints := publicSamplesMax
		if v, err := strconv.Atoi(r.URL.Query().Get("max")); err == nil && v > 0 && v <= publicSamplesMax {
			maxPoints = v
		}
		history := p.store.HistoryDownsampled(agentID, time.Now().Add(-window), maxPoints)
		writeJSON(w, map[string]interface{}{
			"ok":      true,
			"node_id": nodeID,
			"samples": publicview.BuildSamples(history, cfg),
		})
	}
}

// 匿名公开 WS 的资源上限。
//
// 公开页免登录，任何访客都能建长连接，所以这一路必须比登录后的 /ws 更严：
//   - 设计上客户端**一条上行消息都不发**（读循环只为感知断开），故读上限给到
//     只够放控制帧即可。不设限时，gorilla 会按对端声明的长度分配内存，
//     一条巨型帧就是一次未认证 OOM；
//   - 写不设 deadline 时，一个停止读取的访客会让 WriteMessage 永久阻塞，
//     把 goroutine 与 hub 的订阅项一起钉住；
//   - 读不设 deadline 时，静默离开的访客（拔网线/代理悄悄断）会让 ReadMessage
//     永久挂住，同样泄漏 goroutine + 订阅项。靠 30s 心跳 + pong 续期来收敛。
const (
	publicWSReadLimit = 64
	publicWSWriteWait = 10 * time.Second
	// publicWSPongWait 必须大于 30s 的心跳周期，留出一整拍余量。
	publicWSPongWait = 90 * time.Second
	publicWSMaxTotal = 256
	// publicWSMaxPerIP 的作用是"公平性下限"：防一个地址吃光全局名额，而不是压到很小。
	// 取 32 是因为本仓不信任 X-Forwarded-For，计数按 **TCP 对端地址**：
	// 反代/CDN 之后整站访客会共用一个地址，此时过小的每 IP 上限等于对公网访客锁死；
	// 频率仍由上面的限流器（同一口径）把关，这里的并发上限是第二道兜底。
	publicWSMaxPerIP = 32
)

// acquireWS 占用一个匿名 WS 名额（全局与每 IP 双上限）。
func (p *publicSite) acquireWS(ip string) bool {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if p.wsTotal >= publicWSMaxTotal {
		return false
	}
	if p.wsPerIP == nil {
		p.wsPerIP = make(map[string]int)
	}
	if p.wsPerIP[ip] >= publicWSMaxPerIP {
		return false
	}
	p.wsTotal++
	p.wsPerIP[ip]++
	return true
}

// releaseWS 归还名额。计数归零即删键，否则这张表会随访问过的 IP 单调增长。
func (p *publicSite) releaseWS(ip string) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if p.wsTotal > 0 {
		p.wsTotal--
	}
	if n := p.wsPerIP[ip] - 1; n > 0 {
		p.wsPerIP[ip] = n
	} else {
		delete(p.wsPerIP, ip)
	}
}

// publicWSHandler GET /api/public/ws
//
// 免登录实时推送：让公开页的卡片/指标自动刷新，而无需高频轮询。
// 只推脱敏节点列表，且每次推送前重新读取配置（配置一改立即生效）。
func (p *publicSite) publicWSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			return
		}
		// 匿名长连接成本高于普通请求，用更小的限额（同一 guard 的独立键）
		if !p.allow(w, r, "ws") {
			return
		}
		ip := auth.ClientIP(r.RemoteAddr)
		if !p.acquireWS(ip) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
		defer p.releaseWS(ip)
		conn, err := p.upgrader.Upgrade(w, r, nil)
		if err != nil {
			logx.Infof("[public] ws upgrade error: %v", err)
			return
		}
		defer conn.Close()
		conn.SetReadLimit(publicWSReadLimit)
		_ = conn.SetReadDeadline(time.Now().Add(publicWSPongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(publicWSPongWait))
		})
		send := make(chan []byte, 8)
		// 关闭 stop 必须幂等：读循环（对端断开）与函数返回（defer）都会触发一次，
		// 直接 close 两次会 panic（"close of closed channel"），表现为连接从
		// 服务端断开时日志里刷 panic 栈。用 sync.Once 包一层。
		stop := make(chan struct{})
		var stopOnce sync.Once
		closeStop := func() { stopOnce.Do(func() { close(stop) }) }
		p.hub.SubscribePublic(func(payload []byte) {
			select {
			case send <- payload:
			default: // 客户端消费不过来就丢帧，绝不阻塞 hub 广播
			}
		}, stop)
		defer closeStop()

		// 先取信号通道、再读开关：这样任何一次停用要么 close 了手里这条通道，
		// 要么已经体现在下面读到的 cfg 里。顺序反了会漏事件 —— 停用正好落在
		// "读配置"与"订阅"之间时，close 打在上一代通道上，本连接却握住了新的，
		// 于是公开页已关、在访客还在看，直到下一次配置再变才收敛。
		cfgChanged := p.subscribeCfgChange()

		// 先推一次当前状态，避免客户端等待第一个探针周期
		cfg := p.cfg.Get()
		if cfg == nil || !cfg.Enabled {
			return // 握手与建道之间被停用
		}
		nodes := p.visibleNodes(cfg)
		if err := writePublicWS(conn, cfg, nodes); err != nil {
			return
		}

		// 读循环：只为感知断开（客户端不需要发送任何数据）
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					closeStop()
					return
				}
			}
		}()

		// 心跳：既防反向代理掐空闲连接，也是死访客的探测器
		//（对端不回 pong → 读 deadline 到期 → 读循环退出）
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		// 配置开关的即时核对：握手时查过一次不代表之后一直有效，
		// 运维在后台关掉公开页时，在途连接要自己结束（不等下一拍心跳）。
		for {
			select {
			case payload := <-send:
				_ = conn.SetWriteDeadline(time.Now().Add(publicWSWriteWait))
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			case <-cfgChanged:
				// 先换道、再核对开关。顺序反了会漏事件：若在"核对"与"换道"之间
				// 运维完成了停用，那次 close 打在旧通道上，而本连接已经握住了
				// 新通道 —— 结果是公开页关了、在途访客还在看，直到下次配置再变。
				cfgChanged = p.subscribeCfgChange()
				if !p.publicEnabled() {
					return
				}
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(publicWSWriteWait))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-stop:
				return
			}
		}
	}
}

// publicConfigPayload 构造下发给前端的公开配置片段。
//
// 刻意**只**包含展示所需的非敏感项：主机白名单、字段开关之外的东西
// （host_ids / catalog_enabled 等）属于"公开策略"，没必要下发给匿名访客。
// 站点标题与页脚属于**品牌文案**（运维自己配置、面向所有访客展示），
// 因此一并下发，且页脚允许 HTML 原样透传。
//
// 注意：这里必须与 stateHandler 用同一个函数 —— 两处分别手写 map 曾经
// 导致 WS 推送与首屏快照的 config 字段不一致（页面刷新后才变正常）。
func publicConfigPayload(cfg *siteconfig.Config) map[string]interface{} {
	if cfg == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"title":         cfg.Title,
		"description":   cfg.Description,
		"hosts_enabled": cfg.HostsEnabled,
		"traffic_mode":  cfg.TrafficModeOrDefault(),
		"fields":        cfg.Fields,
		"site_title":    cfg.SiteTitleOrDefault(),
		"site_footer":   cfg.SiteFooterOrDefault(),
	}
}

// writePublicWS 生成并下发一条公开 WS 消息。
//
// 首帧同样要设写超时：连接刚建立时对端就可能已经不读了。
func writePublicWS(conn *websocket.Conn, cfg *siteconfig.Config, nodes []*publicview.Node) error {
	payload, err := json.Marshal(map[string]interface{}{
		"ok":      true,
		"nodes":   nodes,
		"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
		"config":  publicConfigPayload(cfg),
	})
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(publicWSWriteWait))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// visibleNodes 取出当前可公开的主机并脱敏（含流量口径装配）。
//
// 用 `AllCached` 而不是 `All`：这条链是**匿名可达**的，访客不该有能力驱动出站
// GeoIP 查询（M-13）。缓存由后台仪表盘那条 `store.All()` 的推送路径负责填，
// 两处读的是同一份。
func (p *publicSite) visibleNodes(cfg *siteconfig.Config) []*publicview.Node {
	if cfg == nil || !cfg.Enabled || !cfg.HostsEnabled {
		return []*publicview.Node{}
	}
	return p.attachTraffic(publicview.BuildNodes(p.store.AllCached(), cfg, p.nodeKeys), cfg)
}

// publicTrafficWindow 公开页"窗口累计流量"的回溯窗口（与历史留存一致，30 天）。
const publicTrafficWindow = 30 * 24 * time.Hour

// attachTraffic 依据配置的流量口径补齐节点流量字段。
//
// 两种口径：
//   - boot：直接用当前探针的网卡累计值（无需查库，且与后台仪表盘口径一致）；
//   - window：查探针增量历史求和，只暴露"留存窗口内"的流量，避免公开主机
//     自启动以来的总承载量。
//
// 注：BuildNode 已按 network 开关裁剪，未开启时本函数是空操作。
func (p *publicSite) attachTraffic(nodes []*publicview.Node, cfg *siteconfig.Config) []*publicview.Node {
	if cfg == nil || !cfg.FieldVisible("network") || len(nodes) == 0 {
		return nodes
	}
	// boot 口径：BuildNode 已带速率，这里只补累计值，避免二次遍历 store
	if cfg.TrafficModeOrDefault() != siteconfig.TrafficModeWindow {
		agents := make(map[string]*protocol.AgentInfo)
		for _, a := range p.store.AllCached() {
			if a != nil {
				agents[p.nodeKeys.ID(a.AgentID)] = a
			}
		}
		for _, n := range nodes {
			if n == nil {
				continue
			}
			n.NetTotalIn = agentNetIn(agents[n.NodeID])
			n.NetTotalOut = agentNetOut(agents[n.NodeID])
		}
		return nodes
	}
	// window 口径：按 node_id 反查设备（映射只包含当前可公开主机），再查增量历史
	nodeIDToAgent := make(map[string]string, len(nodes))
	agentByNode := make(map[string]*protocol.AgentInfo, len(nodes))
	for _, a := range p.store.AllCached() {
		if a == nil || !cfg.HostAllowed(a.AgentID) {
			continue
		}
		id := p.nodeKeys.ID(a.AgentID)
		nodeIDToAgent[id] = a.AgentID
		agentByNode[id] = a
	}
	since := time.Now().Add(-publicTrafficWindow)
	for _, n := range nodes {
		if n == nil {
			continue
		}
		agentID := nodeIDToAgent[n.NodeID]
		if agentID == "" {
			continue
		}
		in, out, known := p.store.NetTotalsSince(agentID, since)
		if !known {
			continue
		}
		n.NetTotalIn = nonNegative64(in)
		n.NetTotalOut = nonNegative64(out)
	}
	return nodes
}

// agentNetIn / agentNetOut 取主机的累计收发字节（nil 安全）。
func agentNetIn(a *protocol.AgentInfo) int64 {
	if a == nil {
		return 0
	}
	return nonNegative64(a.NetInBytes)
}

func agentNetOut(a *protocol.AgentInfo) int64 {
	if a == nil {
		return 0
	}
	return nonNegative64(a.NetOutBytes)
}

func nonNegative64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// publicFieldCatalog 返回字段元数据（不含任何主机数据，登录前后都可安全返回）。
func publicFieldCatalog() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(siteconfig.FieldCatalog))
	for _, f := range siteconfig.FieldCatalog {
		out = append(out, map[string]interface{}{
			"key":         f.Key,
			"label":       f.Label,
			"group":       f.Group,
			"default":     f.Default,
			"sensitive":   f.Sensitive,
			"description": f.Description,
		})
	}
	return out
}

// parsePublicRange 解析趋势时间窗（形如 1h/6h/24h/7d/30d），非法值回退 24h。
// 同时夹住上限，避免 range=100000d 之类请求引发全表扫描。
func parsePublicRange(v string) time.Duration {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return 24 * time.Hour
	}
	unit := v[len(v)-1]
	num, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || num <= 0 {
		return 24 * time.Hour
	}
	var d time.Duration
	switch unit {
	case 'h':
		d = time.Duration(num) * time.Hour
	case 'd':
		d = time.Duration(num) * 24 * time.Hour
	case 'm':
		d = time.Duration(num) * time.Minute
	case 's':
		d = time.Duration(num) * time.Second
	default:
		return 24 * time.Hour
	}
	max := time.Duration(publicRangeMaxSec) * time.Second
	if d > max {
		return max
	}
	return d
}

// --- 后台展示配置接口（需登录） ---

// AuditFunc 记录一条运维审计（操作者身份由调用方从请求中解析）。
type AuditFunc func(r *http.Request, summary string)

// siteConfigHandler GET(读) / POST(写) /api/site/config
//
// 鉴权沿用 guard（Token + IP 白名单 + 限流）：这是**全局展示策略**，
// 能改它的人等同于能决定"哪些机器对外可见"，属于运维权限，不能开放给匿名。
func siteConfigHandler(store *siteconfig.Store, authz *auth.Auth, audit AuditFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfg := store.Get()
			writeJSON(w, map[string]interface{}{
				"ok":     true,
				"config": cfg,
				"fields": publicFieldCatalog(),
			})
		case http.MethodPost, http.MethodPut:
			var body siteconfig.Config
			if !decodeJSON(w, r, maxJSONBody, &body) {
				return
			}
			// 字段白名单：只接受目录内的 key，其余忽略（normalize 会重建 fields）
			if body.Fields == nil {
				body.Fields = map[string]bool{}
			}
			for k := range body.Fields {
				if !siteconfig.IsKnownField(k) {
					delete(body.Fields, k)
				}
			}
			if err := store.Set(&body); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "保存展示配置失败: " + err.Error()})
				return
			}
			// 展示范围变更属于安全相关操作，纳入审计留痕
			if audit != nil {
				audit(r, fmt.Sprintf("更新公开展示配置 enabled=%v hosts=%v catalog=%v hosts_count=%d site_title=%q footer_len=%d",
					body.Enabled, body.HostsEnabled, body.CatalogEnabled, len(body.HostIDs),
					body.SiteTitleOrDefault(), len(body.SiteFooterOrDefault())))
			}
			writeJSON(w, map[string]interface{}{"ok": true, "msg": "展示配置已保存"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// siteConfigResetHandler POST /api/site/config/reset —— 恢复默认（总开关关闭）。
func siteConfigResetHandler(store *siteconfig.Store, authz *auth.Auth, audit AuditFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := store.Set(siteconfig.Default()); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "恢复默认失败: " + err.Error()})
			return
		}
		if audit != nil {
			audit(r, "重置公开展示配置为默认（公开页已关闭）")
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "已恢复默认配置（公开页关闭）"})
	}
}

// siteHostOptionsHandler GET /api/site/hosts —— 供后台挑选公开主机。
//
// 与 /api/agents 的区别：这里只返回挑选所需的**最小字段**
// （agent_id / 备注 / 主机名 / 在线），不把整份主机指纹再复制一遍。
func siteHostOptionsHandler(store *probe.Store, authz *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		all := store.All()
		items := make([]map[string]interface{}, 0, len(all))
		for _, a := range all {
			if a == nil {
				continue
			}
			items = append(items, map[string]interface{}{
				"agent_id": a.AgentID,
				"remark":   a.Remark,
				"hostname": a.Hostname,
				"online":   a.Online,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "items": items})
	}
}

// publicConfigStateHandler GET /api/public/enabled
//
// 只回答一件事：公开状态页开没开。开关关掉时它**直接 404**（M-14）——
// 与其余 /api/public/* 同一口径：功能对外应当"不存在"，而不是留一个
// 永远应答 `{"enabled":false}` 的探针点。
//
// 品牌文案（标题 / 页脚）不在此处，见 brandingHandler：那份内容登录页在
// 登录前就要渲染，与公开页开关无关；把它挤在本端点里，等于"关掉公开页之后，
// 匿名请求仍然能读到本站的品牌配置"。
//
// 仍然刻意**不**返回 host_ids / catalog_enabled / fields 等公开策略 —— 那些是
// "哪些机器对外可见"的决策信息。
func publicConfigStateHandler(store *siteconfig.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		if cfg == nil || !cfg.Enabled {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "enabled": true})
	}
}

// brandingHandler GET /api/branding —— 登录前就要显示的品牌文案。
//
// 免鉴权、且**不受公开页开关影响**：后台登录页要用正确的浏览器标签标题与页脚，
// 而登录前的页面拿不到任何需要鉴权的接口。这两项本来就是给所有人看的内容
// （页脚同样渲染在公开页），放在这里不构成额外暴露 —— 构成暴露的是"顺带回答
// 公开页开没开"，那已经挪回 /api/public/enabled 并由开关把关。
//
// 只出文案，不出字段开关、主机白名单等任何公开策略。
func brandingHandler(store *siteconfig.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		writeJSON(w, map[string]interface{}{
			"ok":          true,
			"site_title":  cfg.SiteTitleOrDefault(),
			"site_footer": cfg.SiteFooterOrDefault(),
		})
	}
}
