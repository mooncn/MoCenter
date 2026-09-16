// 公开站点（免登录）与展示配置的后台接口。
//
// 对应 Issue #5：未登录访客可看主页上的**非敏感**信息（卡片/列表 + 节点详情历史趋势），
// 登录后台可配置「总开关 / 主页是否展示主机 / 逐字段开关」。
//
// 安全模型（本文件是唯一的对外暴露面，改动必须保持以下不变量）：
//
//  1. 公共端点只输出 internal/publicview 的脱敏结构，绝不返回 protocol.AgentInfo；
//  2. 公共端点的主机标识是派生节点编号 n-xxxxxxxx，agent_id / uuid / 内网地址永不出网；
//  3. 所有公共端点都先检查总开关，关闭时一律 404 —— 未启用时公开页面对外**不存在**；
//  4. 公共端点走独立的、更严的限流（与 /api/login 同一套守卫但键前缀不同），
//     避免匿名的列表/历史查询把服务端拖垮，也避免成为扫描器的免费接口。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/probe"
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

// allow 匿名限流：按来源 IP + 路由名限制频率。
//
// 复用登录守卫的实现（滑动窗口 + 失败退避），但每个路由用独立计数键，
// 使"列表轮询"不会消耗"历史查询"的额度，也不会把管理员挡在登录外。
func (p *publicSite) allow(w http.ResponseWriter, r *http.Request, route string) bool {
	if p.guard == nil {
		return true
	}
	if ok, retry := p.guard.Allow(r.RemoteAddr, "public:"+route); !ok {
		w.Header().Set("Retry-After", auth.RetryAfter(retry))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	return true
}

// stateHandler GET /api/public/state
//
// 返回：总开关状态、字段可见性元数据（供前端决定渲染哪些项）、节点列表与汇总。
// 未登录访客靠这一个接口即可渲染整个公开首页。
func (p *publicSite) stateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			http.Error(w, "not found", http.StatusNotFound)
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
			"config": map[string]interface{}{
				"title":         cfg.Title,
				"description":   cfg.Description,
				"hosts_enabled": cfg.HostsEnabled,
				"fields":        cfg.Fields,
			},
			"fields":  publicFieldCatalog(),
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
			http.Error(w, "not found", http.StatusNotFound)
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

// samplesHandler GET /api/public/samples?node=n-xxxxxxxx&range=24h
//
// 节点历史趋势。node 为派生节点编号 —— 服务端在本文件内把它反查回
// agent_id（映射只存在于内存中、且只包含**当前可公开**的主机），
// 因此伪造一个 node 也无法读取未公开主机的历史数据。
func (p *publicSite) samplesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			http.Error(w, "not found", http.StatusNotFound)
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
		for _, a := range p.store.All() {
			if a == nil || !cfg.HostAllowed(a.AgentID) {
				continue
			}
			if publicview.NodeID(a.AgentID) == nodeID {
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

// publicWSHandler GET /api/public/ws
//
// 免登录实时推送：让公开页的卡片/指标自动刷新，而无需高频轮询。
// 只推脱敏节点列表，且每次推送前重新读取配置（配置一改立即生效）。
func (p *publicSite) publicWSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireEnabled(w) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// 匿名长连接成本高于普通请求，用更小的限额（同一 guard 的独立键）
		if !p.allow(w, r, "ws") {
			return
		}
		conn, err := p.upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[public] ws upgrade error: %v", err)
			return
		}
		defer conn.Close()
		send := make(chan []byte, 8)
		stop := make(chan struct{})
		p.hub.SubscribePublic(func(payload []byte) {
			select {
			case send <- payload:
			default: // 客户端消费不过来就丢帧，绝不阻塞 hub 广播
			}
		}, stop)
		defer close(stop)

		// 先推一次当前状态，避免客户端等待第一个探针周期
		cfg := p.cfg.Get()
		nodes := p.visibleNodes(cfg)
		writePublicWS(conn, cfg, nodes)

		// 读循环：只为感知断开（客户端不需要发送任何数据）
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					close(stop)
					return
				}
			}
		}()

		// 心跳：部分反向代理会掐掉长时间无数据的空闲连接
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case payload := <-send:
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-stop:
				return
			}
		}
	}
}

// writePublicWS 生成并下发一条公开 WS 消息。
func writePublicWS(conn *websocket.Conn, cfg *siteconfig.Config, nodes []*publicview.Node) {
	payload, err := json.Marshal(map[string]interface{}{
		"ok":      true,
		"nodes":   nodes,
		"summary": publicview.BuildSummary(nodes, time.Now().Unix()),
		"config": map[string]interface{}{
			"title":         cfg.Title,
			"description":   cfg.Description,
			"hosts_enabled": cfg.HostsEnabled,
			"fields":        cfg.Fields,
		},
	})
	if err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}

// visibleNodes 取出当前可公开的主机并脱敏。
func (p *publicSite) visibleNodes(cfg *siteconfig.Config) []*publicview.Node {
	if cfg == nil || !cfg.Enabled || !cfg.HostsEnabled {
		return []*publicview.Node{}
	}
	return publicview.BuildNodes(p.store.All(), cfg)
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
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
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
				audit(r, fmt.Sprintf("更新公开展示配置 enabled=%v hosts=%v catalog=%v hosts_count=%d",
					body.Enabled, body.HostsEnabled, body.CatalogEnabled, len(body.HostIDs)))
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
// 供前端在**登录前**判断公开页是否启用（例如登录页给出"查看公开状态页"入口）。
// 刻意只返回一个布尔值：该端点本身必须免鉴权，因此不泄露任何其它信息。
func publicConfigStateHandler(store *siteconfig.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		writeJSON(w, map[string]interface{}{"ok": true, "enabled": cfg != nil && cfg.Enabled})
	}
}
