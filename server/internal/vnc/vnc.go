// Package vnc 实现服务端侧的远程桌面（VNC）支持：
//   - /vnc-ws  WebSocket 查看器：浏览器 <-> 服务端 <-> Agent 内网 VNC 服务的字节中继
//   - Probe   探测目标主机 VNC 是否可连（打开隧道试探）
//   - Install 向目标 Agent 下发「一键安装/启动 VNC」指令并等待结果
//
// 复用既有端口映射隧道消息（TunnelOpen/Data/Close）：查看器隧道 ID 以
// "v-" 开头、探测隧道以 "vp-" 开头，便于服务端 Agent 消息循环按前缀分发。
//
// 两条硬约束（本包内所有入口共用，不各自放水）：
//   - 隧道目标端口必须落在 CheckPort 放行的范围内（默认 5900-5999），
//     否则这条"连到被管主机回环"的通道就是任意端口的内网代理；
//   - 安装结果里的 VNC 口令不随状态接口批量下发，只由调用方显式取一次。
package vnc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
	"mocenter/server/internal/ws"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  256 * 1024,
	WriteBufferSize: 256 * 1024,
	// 与 /ws 同口径认来源（见 ws.SameOrigin）：这条通道直达桌面，
	// 跨站劫持成功的后果是"别人的页面能往运维的 VNC 会话里发键鼠"。
	CheckOrigin:  func(r *http.Request) bool { return ws.SameOrigin(r) },
	Subprotocols: []string{ws.WSSubprotocolName},
}

// defaultVNCPort RFB 的显示号 0 对应端口，也是 Agent 侧端口缺省时的归一值。
const defaultVNCPort = 5900

// tunnelEstablishTimeout 隧道建立超时：超过该时间未收到 Agent 的 TunnelOpenResp
// 视为隧道建立失败，关闭查看器避免浏览器一直等待。
const tunnelEstablishTimeout = 15 * time.Second

const (
	// maxViewersPerSession 单个操作员会话的并发远程桌面上限。
	//
	// 每条查看器隧道都会在目标机的回环上开一个 TCP 连接并常驻两个泵。
	// 没有上限时，一枚泄漏的 Token 就能把整片机群的 5900/自定义端口逐个连开，
	// 而这条路径当时连 IP 白名单与限流都不走（见 main.go 的 guardWS）。
	// 取 8：多屏/多机并排操作的实际用量在 4 以内，留出翻倍余量。
	maxViewersPerSession = 8
	// viewerReadLimit 浏览器→服务端单帧上限。RFB 的客户端消息（按键、指针、
	// 剪贴板 cut-text）都在 KB 量级，1 MiB 足够宽，只是关掉"声明超长帧让
	// gorilla 按长度分配"这条无上限路径。
	viewerReadLimit = 1 << 20
	// viewerPendingLimit 隧道就绪前浏览器数据的缓冲上限。
	// 隧道建立要等 Agent 回执（最长 15s），这期间收到的帧先暂存以免丢掉 RFB
	// 握手；无上限时客户端掐着这个窗口猛灌就能吃满内存。
	viewerPendingLimit = 4 << 20
	// viewerWriteWait 向浏览器写一帧的超时。写是在 Agent 消息循环里做的，
	// 浏览器不收就会把这台主机的整条隧道处理链堵住。
	viewerWriteWait = 10 * time.Second
)

// viewerConn 浏览器查看器连接。包含 pending 缓冲，用于解决
// 「浏览器 RFB 数据先于隧道建立到达」导致的 RFB 握手数据丢失（黑屏）问题。
type viewerConn struct {
	ws       *websocket.Conn
	agentID  string
	session  string // 发起方会话键，用于并发名额归还
	tunnelID string
	wmu      sync.Mutex
	// ready 隧道已就绪（Agent 回传 TunnelOpenResp OK=true）。
	// ready==false 时浏览器数据暂存入 pending，待就绪后统一转发。
	ready        bool
	pending      [][]byte
	pendingBytes int
	// relayed 已成功写向浏览器的字节数（仅诊断：closeViewer 日志带上它，
	// "连上却黑屏"要能区分"对面根本没回数据"与"数据在服务端这段丢了"）。
	relayed int64
}

type probeResult struct {
	ok  bool
	msg string
}

// probeWaiter 一次探测的等待登记。
//
// 必须连"应答该由谁给出"一起记：只有发起 TunnelOpen 的那条 Agent 连接的回执
// 才算数，否则任意一台在线主机都能替别人伪造"VNC 可连/不可连"。
type probeWaiter struct {
	agentID string
	ch      chan probeResult
}

// Mgr VNC 远程桌面管理器。
type Mgr struct {
	mu          sync.Mutex
	sess        *session.Manager
	viewer      map[string]*viewerConn                    // tunnelID -> 查看器连接
	viewersBySs map[string]int                            // 操作员会话键 -> 并发查看器数
	probeWait   map[string]*probeWaiter                   // tunnelID("vp-") -> 探测结果 + 归属 Agent
	installWait map[string]chan *protocol.VncInstallResp  // agentID -> 安装结果
	last        map[string]*protocol.VncInstallResp       // agentID -> 最近一次安装/启动结果（口令）
	pwWait      map[string]chan *protocol.VncPasswordResp // agentID -> 口令读取结果
	ports       []portRange                               // 允许作为隧道目标的目标端口（空=不限制）
	portSpec    string                                    // 端口清单原文，只用于错误提示
	// mismatchAt 记录每台 Agent 上一次"越权隧道回传"的留痕时间，用于限频。
	mismatchAt map[string]time.Time
	seq        uint64
}

// NewMgr 创建 VNC 管理器。
func NewMgr(sess *session.Manager) *Mgr {
	// 端口闸门默认就是开的：漏配 -vnc-ports 的后果应当是"连不上非常规端口"，
	// 而不是"退回任意端口代理"。
	def, _ := parsePortSpec(DefaultPortSpec)
	return &Mgr{
		sess:        sess,
		viewer:      make(map[string]*viewerConn),
		viewersBySs: make(map[string]int),
		probeWait:   make(map[string]*probeWaiter),
		installWait: make(map[string]chan *protocol.VncInstallResp),
		last:        make(map[string]*protocol.VncInstallResp),
		pwWait:      make(map[string]chan *protocol.VncPasswordResp),
		mismatchAt:  make(map[string]time.Time),
		ports:       def,
	}
}

// acquireViewer 为该操作员会话占用一个远程桌面并发名额；已达上限返回 false。
//
// 名额按会话（Token 派生）而非按 IP 计：同机的多个运维人员各自有独立的额度，
// 一个共享出口 IP 后面也不会因为别人在开桌面而被误限。
// 空会话键（拿不到 Token）单独共用一份额度而不是免除限制：生产路径上
// HandleViewer 已先校验过 Token，这里只会兜住测试/内部调用。
func (m *Mgr) acquireViewer(session string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.viewersBySs[session] >= maxViewersPerSession {
		return false
	}
	m.viewersBySs[session]++
	return true
}

// releaseViewer 归还一个名额。计数归零即删键，避免长期运行后 map 只增不减。
func (m *Mgr) releaseViewer(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.viewersBySs[session] <= 1 {
		delete(m.viewersBySs, session)
		return
	}
	m.viewersBySs[session]--
}

// DefaultPortSpec VNC 端口的默认允许范围。
//
// RFB 的端口约定是 5900+显示号（:0 → 5900，:1 → 5901…），UltraVNC 与 x11vnc
// 都落在这个区间内。把它作为闸门是因为：VNC 隧道的目标是**被管主机的回环地址**，
// 不限制端口就等于给每个登录会话发了一台"打到任意机器的任意端口代理"——
// 目标机上的 Redis、SSH、各类调试端口都会顺带变成可连的内网服务。
// 跑在非常规端上的部署用 -vnc-ports 显式放行，而不是把闸门整体放开。
const DefaultPortSpec = "5900-5999"

// portRange 一段闭区间端口。
type portRange struct{ lo, hi int }

// parsePortSpec 解析 "5900-5999,6900" 形式的端口清单。
// 空串表示"不限制"（返回 nil 与 true）—— 只在运维显式配置时出现。
func parsePortSpec(spec string) ([]portRange, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, true
	}
	var out []portRange
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		loS, hiS := item, item
		if i := strings.IndexByte(item, '-'); i >= 0 {
			loS, hiS = strings.TrimSpace(item[:i]), strings.TrimSpace(item[i+1:])
		}
		lo, err := strconv.Atoi(loS)
		if err != nil {
			return nil, false
		}
		hi, err := strconv.Atoi(hiS)
		if err != nil {
			return nil, false
		}
		if lo < 1 || hi < lo || hi > 65535 {
			return nil, false
		}
		out = append(out, portRange{lo, hi})
	}
	return out, true
}

// portAllowed 判断端口是否落在允许范围内；范围表为空表示不限制。
func portAllowed(ranges []portRange, port int) bool {
	if len(ranges) == 0 {
		return true
	}
	for _, r := range ranges {
		if port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}

// SetAllowedPorts 配置 VNC 隧道/探测可指向的目标端口。解析失败时保持原值并返回错误，
// 让服务端启动阶段就把配错的清单暴露出来，而不是静默变成"全部放行"。
// 空清单表示不限制（只在运维显式配置时出现）。
func (m *Mgr) SetAllowedPorts(spec string) error {
	ranges, ok := parsePortSpec(spec)
	if !ok {
		return fmt.Errorf("VNC 端口清单 %q 无法解析（形如 5900-5999,6900）", spec)
	}
	m.mu.Lock()
	m.ports = ranges
	m.portSpec = strings.TrimSpace(spec)
	m.mu.Unlock()
	return nil
}

// CheckPort 报告该端口能否作为 VNC 隧道目标；不允许时返回给操作员看的说明。
func (m *Mgr) CheckPort(port int) error {
	m.mu.Lock()
	ranges, spec := m.ports, m.portSpec
	m.mu.Unlock()
	if portAllowed(ranges, port) {
		return nil
	}
	if spec == "" {
		spec = DefaultPortSpec
	}
	return fmt.Errorf("端口 %d 不在允许的 VNC 端口范围内（当前 %s）；确需使用该端口请用 -vnc-ports 放行", port, spec)
}

// LastResult 返回该主机最近一次 VNC 安装/启动结果（含口令），无则返回 nil。
//
// 注意：口令只应经 `/api/vnc/password` 这一个"按需 + 留痕"的端点交付，
// 不要顺手塞进可轮询的状态响应里 —— 状态接口会被前端每几秒拉一次，
// 口令进了它就等于长期驻留在浏览器与访问日志能触及的响应体中。
func (m *Mgr) LastResult(agentID string) *protocol.VncInstallResp {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last[agentID]
}

func (m *Mgr) nextTunnel(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&m.seq, 1))
}

func portOf(r *http.Request, def int) int {
	if s := r.URL.Query().Get("port"); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			return p
		}
	}
	return def
}

func agentOf(r *http.Request) string { return r.URL.Query().Get("agent") }

func (m *Mgr) sendTunnelClose(tunnelID, agentID, reason string) {
	if a := m.sess.Get(agentID); a != nil {
		_ = a.Write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: tunnelID, Reason: reason})
	}
}

// mismatchLogInterval 越权隧道回传的留痕频率：每台伪造方 Agent 每分钟一条。
// 被攻陷的主机可以拿猜出来的隧道 ID 高频回传，每条都写日志就等于把修复本身
// 变成灌满日志的 DoS 面。
const mismatchLogInterval = time.Minute

// logMismatch 记录一次"用别人的隧道 ID 回传"，按 agentID 限频。
func (m *Mgr) logMismatch(from, owner, tunnelID, kind string) {
	log.Printf("[vnc] 丢弃 %s 的%s：隧道 %s 属于 %s（身份与归属不符）", from, kind, tunnelID, owner)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mismatchAt == nil {
		m.mismatchAt = make(map[string]time.Time)
	}
	now := time.Now()
	if last, seen := m.mismatchAt[from]; seen && now.Sub(last) < mismatchLogInterval {
		return
	}
	m.mismatchAt[from] = now
	for k, t := range m.mismatchAt {
		if now.Sub(t) > 10*time.Minute {
			delete(m.mismatchAt, k)
		}
	}
}

// closeViewer 关闭查看器连接并清理。注意：调用方不应持有 vw.wmu 锁。
// closeViewer 关闭查看器连接并通知 Agent 拆掉本地隧道。
//
// reason 只为日志存在：以前这条隧道怎么没的，服务端一个字都不说，
// 浏览器端只剩"连接断开"。真机排障时（远程桌面连上却没画面）只能靠猜。
// 用变参是为了不逼着所有调用点和测试都改签名 —— 但新加的调用点应当给原因。
func (m *Mgr) closeViewer(tunnelID string, reason ...string) {
	m.mu.Lock()
	vw, ok := m.viewer[tunnelID]
	if ok {
		delete(m.viewer, tunnelID)
	}
	m.mu.Unlock()
	if !ok || vw == nil {
		return // 已经被关过：不打日志，否则一次关闭出现两条，排障时反而误导
	}
	r := "未记录原因"
	if len(reason) > 0 && reason[0] != "" {
		r = reason[0]
	}
	// 这条日志是排障的唯一抓手：远程桌面"连上就断/没画面"时，以前服务端什么都没说。
	m.releaseViewer(vw.session)
	vw.wmu.Lock()
	relayed := vw.relayed
	_ = vw.ws.Close()
	vw.wmu.Unlock()
	log.Printf("[vnc] 关闭隧道 %s agent=%s 原因=%s 已转发=%dB", tunnelID, vw.agentID, r, relayed)
	m.sendTunnelClose(tunnelID, vw.agentID, r)
}

// Probe 探测目标 Agent 的 VNC 端口是否可连。可连时立即释放隧道（仅探测）。
func (m *Mgr) Probe(agentID string, port int) (bool, string) {
	if agentID == "" {
		return false, "未指定目标主机"
	}
	if port <= 0 {
		port = defaultVNCPort
	}
	if err := m.CheckPort(port); err != nil {
		return false, err.Error()
	}
	a := m.sess.Get(agentID)
	if a == nil {
		return false, "目标主机离线"
	}
	tunnelID := m.nextTunnel("vp")
	ch := make(chan probeResult, 1)
	m.mu.Lock()
	m.probeWait[tunnelID] = &probeWaiter{agentID: agentID, ch: ch}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.probeWait, tunnelID)
		m.mu.Unlock()
	}()

	if err := a.Write(protocol.TypeTunnelOpen, &protocol.TunnelOpenReq{
		TunnelID: tunnelID, AgentID: agentID, LocalHost: "127.0.0.1", LocalPort: port,
	}); err != nil {
		return false, "下发探测失败: " + err.Error()
	}
	select {
	case r := <-ch:
		// 探测到可连后释放隧道，避免占用连接
		if r.ok {
			m.sendTunnelClose(tunnelID, agentID, "probe-done")
		}
		return r.ok, r.msg
	case <-time.After(6 * time.Second):
		m.sendTunnelClose(tunnelID, agentID, "probe-timeout")
		return false, "探测超时：目标未运行 VNC 或不可达"
	}
}

// Install 向目标 Agent 下发一键安装/启动 VNC 指令并等待结果。
// mode 为安装模式：protocol.VncModeInteractive（默认）/ protocol.VncModeService（支持锁屏）。
func (m *Mgr) Install(agentID string, port int, mode string) *protocol.VncInstallResp {
	nf := func(msg string) *protocol.VncInstallResp {
		return &protocol.VncInstallResp{OK: false, Message: msg, Port: port, Mode: mode}
	}
	if agentID == "" {
		return nf("未指定目标主机")
	}
	// 与 Agent 侧 `VncInstallCfg` 的口径保持一致：端口缺省即 5900。
	// 闸门必须校验"最终会被连的那个端口"，否则 0/负数会绕过范围检查后又
	// 被 Agent 归一成 5900。
	if port <= 0 {
		port = defaultVNCPort
	}
	if err := m.CheckPort(port); err != nil {
		return nf(err.Error())
	}
	a := m.sess.Get(agentID)
	if a == nil {
		return nf("目标主机离线")
	}
	ch := make(chan *protocol.VncInstallResp, 1)
	m.mu.Lock()
	if _, busy := m.installWait[agentID]; busy {
		m.mu.Unlock()
		return nf("该主机已有安装任务进行中，请稍候")
	}
	m.installWait[agentID] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.installWait, agentID)
		m.mu.Unlock()
	}()

	if err := a.Write(protocol.TypeVncInstallReq, &protocol.VncInstallReq{AgentID: agentID, Port: port, Mode: mode}); err != nil {
		return nf("下发安装指令失败: " + err.Error())
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(240 * time.Second):
		return &protocol.VncInstallResp{OK: false, Message: "安装超时（下载较慢或需更高权限）", Port: port, Mode: mode}
	}
}

// DeliverTunnelData 将 Agent 回传的 VNC 数据写回浏览器查看器 WebSocket。
// agentID 必须是本条报文的**连接身份**（取自 session.Conn），不是报文里的自报字段。
func (m *Mgr) DeliverTunnelData(tunnelID, agentID string, data []byte) {
	m.mu.Lock()
	vw, ok := m.viewer[tunnelID]
	m.mu.Unlock()
	if !ok || vw == nil {
		return
	}
	if vw.agentID != agentID {
		m.logMismatch(agentID, vw.agentID, tunnelID, "帧数据")
		return
	}
	vw.wmu.Lock()
	_ = vw.ws.SetWriteDeadline(time.Now().Add(viewerWriteWait))
	if err := vw.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		vw.wmu.Unlock()
		m.closeViewer(tunnelID, "写查看器失败（WS 写超时或浏览器已关）")
		return
	}
	vw.relayed += int64(len(data))
	vw.wmu.Unlock()
}

// DeliverOpenResp 处理 VNC 隧道的建立响应。
// 隧道建立成功后，flush 缓冲的浏览器 RFB 数据。
// agentID 为回传方的连接身份：隧道归属只认发起它的那台 Agent，否则一台被攻陷的
// 主机可以给别人的桌面会话回"就绪"，把浏览器缓冲的数据灌进它的隧道。
func (m *Mgr) DeliverOpenResp(tunnelID, agentID string, ok bool, msg string) {
	m.mu.Lock()
	pw := m.probeWait[tunnelID]
	vw := m.viewer[tunnelID]
	m.mu.Unlock()
	if pw != nil {
		if pw.agentID != agentID {
			m.logMismatch(agentID, pw.agentID, tunnelID, "探测结果")
			return
		}
		pw.ch <- probeResult{ok: ok, msg: msg}
		return
	}
	if vw == nil {
		return
	}
	if vw.agentID != agentID {
		m.logMismatch(agentID, vw.agentID, tunnelID, "隧道建立响应")
		return
	}
	if !ok {
		// msg 来自 Agent 报文，必须经 json.Marshal 再出去：字符串拼接时
		// 一个 `","type":"...` 就能越出字段边界，让被攻陷的主机给运维的
		// 桌面通道注入任意帧（或提前终止 RFB 解析）。
		notice, nerr := json.Marshal(map[string]string{"type": "vnc_unavailable", "message": msg})
		if nerr != nil {
			notice = []byte(`{"type":"vnc_unavailable","message":"隧道建立失败"}`)
		}
		vw.wmu.Lock()
		_ = vw.ws.SetWriteDeadline(time.Now().Add(viewerWriteWait))
		_ = vw.ws.WriteMessage(websocket.TextMessage, notice)
		vw.wmu.Unlock()
		m.closeViewer(tunnelID, "Agent 回隧道不可用")
		return
	}
	// 隧道就绪：标记 ready 并 flush 缓冲的浏览器数据
	var pending [][]byte
	vw.wmu.Lock()
	vw.ready = true
	pending = vw.pending
	vw.pending = nil
	vw.pendingBytes = 0
	vw.wmu.Unlock()

	agent := m.sess.Get(vw.agentID)
	if agent == nil {
		m.closeViewer(tunnelID, "转发缓冲前发现 Agent 已断开")
		return
	}
	for _, d := range pending {
		if err := agent.Write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: d}); err != nil {
			m.closeViewer(tunnelID, "flush 缓冲数据写向 Agent 失败")
			return
		}
	}
}

// NotifyTunnelClosed 处理 Agent 本地 VNC 隧道关闭通知。
// 只有发起该隧道的 Agent 才能关闭它：否则一台主机能靠猜隧道 ID 掐断别人的桌面。
func (m *Mgr) NotifyTunnelClosed(tunnelID, agentID string) {
	m.mu.Lock()
	vw := m.viewer[tunnelID]
	m.mu.Unlock()
	if vw == nil {
		return
	}
	if vw.agentID != agentID {
		m.logMismatch(agentID, vw.agentID, tunnelID, "隧道关闭通知")
		return
	}
	m.closeViewer(tunnelID, "Agent 侧已关闭隧道")
}

// NotifyInstall 接收 Agent 回传的 VNC 安装/启动结果（始终缓存最近结果，便于口令补发）。
func (m *Mgr) NotifyInstall(agentID string, resp *protocol.VncInstallResp) {
	m.mu.Lock()
	if resp != nil {
		m.last[agentID] = resp
	}
	ch, ok := m.installWait[agentID]
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// ReadPassword 向目标 Agent 读取本机当前生效的 VNC 口令（只读）。
//
// 为什么必须实时去读，而不是回放 m.last 里缓存的上次安装口令：
//   - 服务端一重启，缓存就没了，可机器上的 VNC 还在正常跑；
//   - 换个人或换一套服务端操作过同一主机，本进程同样没有缓存；
//   - VNC 若是手工装的，从来就没有过缓存。
//
// 这几种情况下缓存都答不上"现在的口令是什么"，而重装一次又会打断正在使用的会话。
// 另外，VNC 隧道本身经 Agent 中转 —— Agent 离线时即便有口令也连不上，
// 所以"用缓存兜底"在这里没有实际价值：实时读取失败就如实报失败，不返回可能过期的值。
func (m *Mgr) ReadPassword(agentID string) *protocol.VncPasswordResp {
	nf := func(msg string) *protocol.VncPasswordResp {
		return &protocol.VncPasswordResp{OK: false, Message: msg}
	}
	if agentID == "" {
		return nf("未指定目标主机")
	}
	a := m.sess.Get(agentID)
	if a == nil {
		return nf("目标主机离线")
	}
	ch := make(chan *protocol.VncPasswordResp, 1)
	m.mu.Lock()
	if _, busy := m.pwWait[agentID]; busy {
		m.mu.Unlock()
		return nf("该主机已有口令读取任务进行中，请稍候")
	}
	m.pwWait[agentID] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pwWait, agentID)
		m.mu.Unlock()
	}()

	if err := a.Write(protocol.TypeVncPasswordReq, &protocol.VncPasswordReq{AgentID: agentID}); err != nil {
		return nf("下发口令读取指令失败: " + err.Error())
	}
	select {
	case r := <-ch:
		if r == nil {
			return nf("Agent 未返回口令读取结果")
		}
		return r
	case <-time.After(20 * time.Second):
		// 读取只是读文件，20 秒足够；超时通常意味着 Agent 侧卡住
		return nf("读取超时（Agent 未在 20 秒内响应）")
	}
}

// NotifyPassword 接收 Agent 回传的口令读取结果。
func (m *Mgr) NotifyPassword(agentID string, resp *protocol.VncPasswordResp) {
	m.mu.Lock()
	ch, ok := m.pwWait[agentID]
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// CloseSession 关闭某会话键下的全部远程桌面查看器（分享撤销/过期时使用）。
// 返回被关闭的隧道数。
func (m *Mgr) CloseSession(session string) int {
	if session == "" {
		return 0
	}
	m.mu.Lock()
	ids := make([]string, 0, 4)
	for id, vw := range m.viewer {
		if vw != nil && vw.session == session {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closeViewer(id, "分享已撤销或过期")
	}
	return len(ids)
}

// HandleViewer 处理浏览器查看器 WebSocket：升级为 WS，建立到 Agent 内网 VNC 的隧道，
// 双向中继 RFB 字节流。
// 关键修复：浏览器 RFB 数据可能在隧道建立前到达，此时先缓冲，等 TunnelOpenResp
// 确认隧道就绪后再转发，避免 RFB 握手数据丢失导致黑屏。
func (m *Mgr) HandleViewer(w http.ResponseWriter, r *http.Request, validateToken func(string) bool) {
	m.handleViewer(w, r, func(token, _ string, _ int) bool { return validateToken(token) })
}

// HandleViewerScoped 同 HandleViewer，但把 token 校验扩展到「主机 + 端口」：
// 分享链接只能看它被授权的那台主机、那个 VNC 端口，不能换参数去看别处。
func (m *Mgr) HandleViewerScoped(w http.ResponseWriter, r *http.Request, validate func(token, agentID string, port int) bool) {
	m.handleViewer(w, r, validate)
}

func (m *Mgr) handleViewer(w http.ResponseWriter, r *http.Request, validate func(token, agentID string, port int) bool) {
	// Token 优先取子协议头（避免出现在 URL 里被日志留存），兼容旧 query 传参
	token := ws.WSToken(r)
	agentID := agentOf(r)
	port := portOf(r, defaultVNCPort)
	if validate == nil || !validate(token, agentID, port) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if agentID == "" {
		http.Error(w, "missing agent", http.StatusBadRequest)
		return
	}
	// 目标端口闸门：放在握手与名额之前。VNC 隧道连的是被管主机的回环，
	// 放开任意端口就等于给每个登录会话一台内网端口代理（Redis/SSH/调试端口）。
	if err := m.CheckPort(port); err != nil {
		log.Printf("[vnc] 拒绝隧道请求 agent=%s port=%d: %s", agentID, port, err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 并发上限必须在 Upgrade 之前判：一旦回了 101 就无法再改口发 429。
	// 这里与"真正登记隧道"之间还有 upgrade/查 Agent 两步，最坏情况是同一会话
	// 瞬时多开一条——窗口毫秒级，不足以防住刷隧道。
	session := ws.SessionKey(token)
	if !m.acquireViewer(session) {
		log.Printf("[vnc] 会话 %s… 远程桌面并发数已达上限 %d，拒绝握手（来自 %s）",
			ws.SessionPrefix(session), maxViewersPerSession, r.RemoteAddr)
		http.Error(w, "too many viewers", http.StatusTooManyRequests)
		return
	}
	// 名额先记在 HandleViewer 手上；登记进 viewer 表后交接给 closeViewer 归还。
	acquired := true
	defer func() {
		if acquired {
			m.releaseViewer(session)
		}
	}()

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[vnc] upgrade error: %v", err)
		return
	}
	conn.SetReadLimit(viewerReadLimit)
	a := m.sess.Get(agentID)
	if a == nil {
		_ = conn.SetWriteDeadline(time.Now().Add(viewerWriteWait))
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"error\",\"message\":\"目标主机离线\"}"))
		_ = conn.Close()
		return
	}
	tunnelID := m.nextTunnel("v")
	vw := &viewerConn{ws: conn, agentID: agentID, session: session, tunnelID: tunnelID}
	m.mu.Lock()
	m.viewer[tunnelID] = vw
	m.mu.Unlock()
	acquired = false

	if err := a.Write(protocol.TypeTunnelOpen, &protocol.TunnelOpenReq{
		TunnelID: tunnelID, AgentID: agentID, LocalHost: "127.0.0.1", LocalPort: port,
	}); err != nil {
		m.closeViewer(tunnelID, "向 Agent 发 TunnelOpen 失败")
		return
	}

	// 隧道建立超时：若 Agent 在指定时间内未回传 TunnelOpenResp，
	// 关闭查看器并向浏览器发送错误消息。
	go func(tid string) {
		time.Sleep(tunnelEstablishTimeout)
		m.mu.Lock()
		v, ok := m.viewer[tid]
		m.mu.Unlock()
		if !ok || v == nil {
			return
		}
		v.wmu.Lock()
		ready := v.ready
		v.wmu.Unlock()
		if !ready {
			log.Printf("[vnc] 隧道建立超时 %s，关闭查看器", tid)
			m.closeViewer(tid, "隧道建立超时（Agent 未回 openResp）")
		}
	}(tunnelID)

	// 浏览器 -> Agent：读取浏览器 RFB 帧。
	// 若隧道尚未就绪（DeliverOpenResp 未收到），先存入 pending 缓冲；
	// 待隧道建立后由 DeliverOpenResp 统一转发。
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// 判断隧道是否已就绪
		vw.wmu.Lock()
		ready := vw.ready
		if ready {
			vw.wmu.Unlock()
			if cur := m.sess.Get(agentID); cur != nil {
				if werr := cur.Write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: data}); werr != nil {
					break
				}
			} else {
				break
			}
		} else {
			vw.pendingBytes += len(data)
			over := vw.pendingBytes > viewerPendingLimit
			vw.pending = append(vw.pending, data)
			vw.wmu.Unlock()
			if over {
				log.Printf("[vnc] 隧道 %s 建立前缓冲超过 %d 字节，关闭查看器", tunnelID, viewerPendingLimit)
				break
			}
		}
	}
	m.closeViewer(tunnelID)
}
