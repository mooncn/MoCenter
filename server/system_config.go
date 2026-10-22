package main

import (
	"fmt"
	"mocenter/server/internal/logx"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/config"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

// settingsStore 串行化"改内存配置 + 写配置文件"，避免会话时长与两个监听
// 配置并发保存时互相覆盖（config.Save 是整文件写入）。
type settingsStore struct {
	mu   sync.Mutex
	path string
	cfg  *config.Config
}

func (s *settingsStore) Snapshot() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.cfg
}

// Update 应用修改并落盘；写盘失败时回滚内存值，保证"看到的=已保存的"。
func (s *settingsStore) Update(fn func(*config.Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := *s.cfg
	fn(s.cfg)
	if err := config.Save(s.path, s.cfg); err != nil {
		*s.cfg = before
		return err
	}
	return nil
}

// sessionTTLOptions 是后台允许选择的会话保持时长（键为规范 duration 字符串）。
var sessionTTLOptions = map[string]time.Duration{
	"1h":   time.Hour,
	"12h":  12 * time.Hour,
	"24h":  24 * time.Hour,
	"168h": 7 * 24 * time.Hour,
	"360h": 15 * 24 * time.Hour,
	"720h": 30 * 24 * time.Hour,
}

type listenInfo struct {
	Addr     string `json:"addr"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Wildcard bool   `json:"wildcard"`
}

func makeListenInfo(addr string) listenInfo {
	host, port, err := splitListenAddr(addr)
	if err != nil {
		return listenInfo{Addr: addr}
	}
	return listenInfo{Addr: addr, Host: host, Port: port, Wildcard: wildcardListenHost(host)}
}

// systemConfigHandler 返回系统配置页需要的当前值（均需登录）。
func systemConfigHandler(authz *auth.Auth, settings *settingsStore, addrs func() (string, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := settings.Snapshot()
		agentAddr, httpAddr := addrs()
		ttl := snap.TokenTTL
		if _, ok := sessionTTLOptions[ttl]; !ok {
			// 启动时 cfg 里可能存的是 12h0m0s（Duration.String()），
			// 归一成前端下拉框认识的规范值。
			ttl = authz.TokenTTL().String()
			for k, v := range sessionTTLOptions {
				if v == authz.TokenTTL() {
					ttl = k
					break
				}
			}
		}
		writeJSON(w, map[string]interface{}{
			"ok":                true,
			"token_ttl":         ttl,
			"token_ttl_seconds": int(authz.TokenTTL().Seconds()),
			"agent_listen":      makeListenInfo(agentAddr),
			"http_listen":       makeListenInfo(httpAddr),
			"public_ip":         cachedPublicIP(),
		})
	}
}

// systemSessionHandler 修改会话保持时长。
//
// 只接受白名单里的六个取值：TTL 直接决定 Token 能活多久，任由前端传任意
// duration 会让"误填 10000h"变成一次静默的永久会话。
func systemSessionHandler(authz *auth.Auth, settings *settingsStore, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			TTL string `json:"ttl"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		ttl := strings.TrimSpace(body.TTL)
		d, ok := sessionTTLOptions[ttl]
		if !ok {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "不支持的会话保持时长"})
			return
		}
		if err := settings.Update(func(c *config.Config) { c.TokenTTL = ttl }); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "写入配置失败: " + err.Error()})
			return
		}
		authz.SetTokenTTL(d)
		if audit != nil {
			audit(r, "修改会话保持时长为 "+ttl)
		}
		logx.Infof("[server] 会话保持时长已更新为 %s（新有效期同时收紧现有会话）", ttl)
		writeJSON(w, map[string]interface{}{
			"ok":  true,
			"ttl": ttl,
			"msg": "会话保持时长已更新；现有会话会在下次请求时按新时长续期",
		})
	}
}

// serverAddrAckRegistry 收集 Agent 对 ServerAddrUpdate 的确认。
type serverAddrAckRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan protocol.ServerAddrUpdateAck
}

var serverAddrAcks = &serverAddrAckRegistry{waiters: make(map[string]chan protocol.ServerAddrUpdateAck)}

func (r *serverAddrAckRegistry) begin(agentID string) chan protocol.ServerAddrUpdateAck {
	ch := make(chan protocol.ServerAddrUpdateAck, 1)
	r.mu.Lock()
	r.waiters[agentID] = ch
	r.mu.Unlock()
	return ch
}

func (r *serverAddrAckRegistry) end(agentID string, ch chan protocol.ServerAddrUpdateAck) {
	r.mu.Lock()
	if r.waiters[agentID] == ch {
		delete(r.waiters, agentID)
	}
	r.mu.Unlock()
}

func (r *serverAddrAckRegistry) deliver(ack protocol.ServerAddrUpdateAck) {
	r.mu.Lock()
	ch := r.waiters[ack.AgentID]
	r.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ack:
	default:
	}
}

// pushServerAddrUpdate 向所有在线 Agent 下发新地址，并等待落盘确认。
// 返回成功写入通知的主机数与确认数；旧版 Agent 不会回 Ack，因此不会被踢。
func pushServerAddrUpdate(sess *session.Manager, upd protocol.ServerAddrUpdate) (notified int, acked map[string]bool) {
	acked = make(map[string]bool)
	chs := make(map[string]chan protocol.ServerAddrUpdateAck)
	for _, c := range sess.All() {
		id := c.GetAgentID()
		if id == "" {
			continue
		}
		ch := serverAddrAcks.begin(id)
		chs[id] = ch
		upd.ApplyAfterSec = 2
		if err := c.Write(protocol.TypeServerAddrUpdate, upd); err != nil {
			serverAddrAcks.end(id, ch)
			delete(chs, id)
			continue
		}
		notified++
	}
	if notified > 0 {
		time.Sleep(1500 * time.Millisecond)
	}
	for id, ch := range chs {
		select {
		case ack := <-ch:
			if ack.OK {
				acked[id] = true
			}
		default:
		}
		serverAddrAcks.end(id, ch)
	}
	return notified, acked
}

// systemListenApplyMu 串行化两个监听配置的修改：地址下发涉及"等待 Ack + 切监听 +
// 落盘"多步，两个请求交错会把 Ack 登记表与配置文件搅在一起。
var systemListenApplyMu sync.Mutex

// systemAgentListenHandler 修改 Agent TCP 监听地址。
//
// 顺序：校验密码 → 先绑定新监听 → 再向在线 Agent 下发新地址并等待确认 →
// 断开已确认的主机促使其立即重连。
//
// 先绑新监听再下发，是为了杜绝"地址已写进 Agent 配置、服务端却绑定失败"的死局：
// 那种情况下 Agent 一重连就会连到一个不存在的端口，而且服务端已经无法再通知它。
// 旧监听在换端口时仍会保持到新监听就绪；同端口换 Host 时旧监听虽已释放，
// 但已建立的 TCP 连接不受影响，下发与断开仍可控。
func systemAgentListenHandler(authz *auth.Auth, um *auth.UserManager, settings *settingsStore,
	listeners func() *listenerHub, sess *session.Manager, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Password string `json:"password"`
			Confirm  bool   `json:"confirm"`
		}
		if !decodeJSON(w, r, maxCredBody, &body) {
			return
		}
		if !body.Confirm {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "请先确认已放通防火墙，并知晓 Agent 可能短暂离线"})
			return
		}
		systemListenApplyMu.Lock()
		defer systemListenApplyMu.Unlock()
		username := authz.OperatorOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		if strings.TrimSpace(body.Password) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "修改监听地址需要验证当前密码"})
			return
		}
		if err := um.VerifyPassword(username, body.Password); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "当前密码错误"})
			return
		}
		addr, err := normalizeListenAddr(body.Host, body.Port)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		lm := listeners()
		if lm == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "监听管理器尚未就绪"})
			return
		}
		oldAddr := lm.AgentAddr()
		if addr == oldAddr {
			writeJSON(w, map[string]interface{}{"ok": true, "msg": "监听地址未变化", "addr": addr})
			return
		}
		// 下发给 Agent 的地址：明确 host 用 host:port；监听全部网卡时只下发端口，
		// 由各 Agent 沿用自己配置里的主机（内网/域名/公网路径可能各不相同）。
		host, port, _ := splitListenAddr(addr)
		upd := protocol.ServerAddrUpdate{}
		agentAddrText := ""
		if wildcardListenHost(host) {
			upd.Port = port
			agentAddrText = fmt.Sprintf("端口 %d（保留各 Agent 现有主机）", port)
		} else {
			upd.ServerAddr = net.JoinHostPort(host, fmt.Sprint(port))
			agentAddrText = upd.ServerAddr
		}
		// 先切换监听：失败时 Agent 完全不受影响，也避免把不可达地址写进它们配置。
		if err := lm.RebindAgent(addr); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		// 端口变了才需要放通；已有放行规则时 ensureFirewallPort 会跳过。
		_, oldPort, _ := splitListenAddr(oldAddr)
		firewallMsg := "端口未变化，无需新增防火墙规则"
		if port != oldPort {
			firewallMsg = ensureFirewallPort(port, "Agent")
			logx.Infof("[server] Agent 监听防火墙处理: %s", firewallMsg)
		}
		warn := ""
		if err := settings.Update(func(c *config.Config) {
			c.Listen = addr
			c.ListenManaged = true
		}); err != nil {
			warn = "（新监听已生效，但写入配置失败：重启后将回到旧地址）"
			logx.Errorf("[server] Agent 监听已切换，但写配置失败: %v", err)
		}
		// 新监听已就绪，再下发地址；确认后的主机直接断开让它们立刻重连，
		// 未确认（旧版 Agent）保持原连接，不做无差别踢下线。
		notified, acked := pushServerAddrUpdate(sess, upd)
		for id := range acked {
			if c := sess.Get(id); c != nil && c.NetConn != nil {
				_ = c.NetConn.Close()
			}
		}
		if audit != nil {
			audit(r, fmt.Sprintf("修改 Agent 监听地址 %s → %s（通知 %d 台，确认 %d 台）", oldAddr, addr, notified, len(acked)))
		}
		logx.Infof("[server] Agent 监听已切换 %s → %s（通知 %d 台，确认 %d 台）", oldAddr, addr, notified, len(acked))
		writeJSON(w, map[string]interface{}{
			"ok":         true,
			"addr":       addr,
			"old_addr":   oldAddr,
			"agent_addr": agentAddrText,
			"notified":   notified,
			"acked":      len(acked),
			"firewall":   firewallMsg,
			"msg": fmt.Sprintf("Agent 监听已切换为 %s；已通知 %d 台在线主机，其中 %d 台确认并将重连；防火墙：%s%s",
				addr, notified, len(acked), firewallMsg, warn),
		})
	}
}

// systemHTTPListenHandler 修改 Web 控制台监听地址。
func systemHTTPListenHandler(authz *auth.Auth, um *auth.UserManager, settings *settingsStore,
	listeners func() *listenerHub, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Password string `json:"password"`
			Confirm  bool   `json:"confirm"`
		}
		if !decodeJSON(w, r, maxCredBody, &body) {
			return
		}
		if !body.Confirm {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "请先确认已放通防火墙，并知晓当前页面可能断开"})
			return
		}
		systemListenApplyMu.Lock()
		defer systemListenApplyMu.Unlock()
		username := authz.OperatorOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if username == "" || username == "unknown" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "会话无效，请重新登录"})
			return
		}
		if strings.TrimSpace(body.Password) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "修改后台监听地址需要验证当前密码"})
			return
		}
		if err := um.VerifyPassword(username, body.Password); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "当前密码错误"})
			return
		}
		addr, err := normalizeListenAddr(body.Host, body.Port)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		lm := listeners()
		if lm == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "监听管理器尚未就绪"})
			return
		}
		oldAddr := lm.HTTPAddr()
		if addr == oldAddr {
			writeJSON(w, map[string]interface{}{"ok": true, "msg": "监听地址未变化", "addr": addr})
			return
		}
		pending, err := lm.RebindHTTP(addr, func(a string) error {
			return settings.Update(func(c *config.Config) {
				c.HTTP = a
				c.HTTPManaged = true
			})
		})
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		_, oldPort, _ := splitListenAddr(oldAddr)
		_, newPort, _ := splitListenAddr(addr)
		firewallMsg := "端口未变化，无需新增防火墙规则"
		if newPort != oldPort {
			firewallMsg = ensureFirewallPort(newPort, "Web")
			logx.Infof("[server] Web 控制台防火墙处理: %s", firewallMsg)
		}
		if audit != nil {
			audit(r, fmt.Sprintf("修改后台监听地址 %s → %s", oldAddr, addr))
		}
		msg := fmt.Sprintf("后台监听已切换为 %s；防火墙：%s", addr, firewallMsg)
		if pending {
			msg = fmt.Sprintf("已保存并开始切换为 %s：当前连接会断开，请用新地址访问；防火墙：%s"+
				"（若同端口换主机，切换失败会自动恢复旧地址）", addr, firewallMsg)
		}
		logx.Infof("[server] 后台监听配置请求: %s → %s（pending=%v）", oldAddr, addr, pending)
		writeJSON(w, map[string]interface{}{
			"ok":       true,
			"addr":     addr,
			"old_addr": oldAddr,
			"pending":  pending,
			"firewall": firewallMsg,
			"msg":      msg,
		})
	}
}
