// Package vnc 实现服务端侧的远程桌面（VNC）支持：
//   - /vnc-ws  WebSocket 查看器：浏览器 <-> 服务端 <-> Agent 内网 VNC 服务的字节中继
//   - Probe   探测目标主机 VNC 是否可连（打开隧道试探）
//   - Install 向目标 Agent 下发「一键安装/启动 VNC」指令并等待结果
//
// 复用既有端口映射隧道消息（TunnelOpen/Data/Close）：查看器隧道 ID 以
// "v-" 开头、探测隧道以 "vp-" 开头，便于服务端 Agent 消息循环按前缀分发。
package vnc

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  256 * 1024,
	WriteBufferSize: 256 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// tunnelEstablishTimeout 隧道建立超时：超过该时间未收到 Agent 的 TunnelOpenResp
// 视为隧道建立失败，关闭查看器避免浏览器一直等待。
const tunnelEstablishTimeout = 15 * time.Second

// viewerConn 浏览器查看器连接。包含 pending 缓冲，用于解决
// 「浏览器 RFB 数据先于隧道建立到达」导致的 RFB 握手数据丢失（黑屏）问题。
type viewerConn struct {
	ws       *websocket.Conn
	agentID  string
	tunnelID string
	wmu      sync.Mutex
	// ready 隧道已就绪（Agent 回传 TunnelOpenResp OK=true）。
	// ready==false 时浏览器数据暂存入 pending，待就绪后统一转发。
	ready   bool
	pending [][]byte
}

type probeResult struct {
	ok  bool
	msg string
}

// Mgr VNC 远程桌面管理器。
type Mgr struct {
	mu          sync.Mutex
	sess        *session.Manager
	viewer      map[string]*viewerConn                      // tunnelID -> 查看器连接
	probeWait   map[string]chan probeResult                 // tunnelID("vp-") -> 探测结果
	installWait map[string]chan *protocol.VncInstallResp    // agentID -> 安装结果
	last        map[string]*protocol.VncInstallResp         // agentID -> 最近一次安装/启动结果（口令）
	seq         uint64
}

// NewMgr 创建 VNC 管理器。
func NewMgr(sess *session.Manager) *Mgr {
	return &Mgr{
		sess:        sess,
		viewer:      make(map[string]*viewerConn),
		probeWait:   make(map[string]chan probeResult),
		installWait: make(map[string]chan *protocol.VncInstallResp),
		last:        make(map[string]*protocol.VncInstallResp),
	}
}

// LastResult 返回该主机最近一次 VNC 安装/启动结果（含口令），无则返回 nil。
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

// closeViewer 关闭查看器连接并清理。注意：调用方不应持有 vw.wmu 锁。
func (m *Mgr) closeViewer(tunnelID string) {
	m.mu.Lock()
	vw, ok := m.viewer[tunnelID]
	if ok {
		delete(m.viewer, tunnelID)
	}
	m.mu.Unlock()
	if ok && vw != nil {
		vw.wmu.Lock()
		_ = vw.ws.Close()
		vw.wmu.Unlock()
		m.sendTunnelClose(tunnelID, vw.agentID, "viewer-closed")
	}
}

// Probe 探测目标 Agent 的 VNC 端口是否可连。可连时立即释放隧道（仅探测）。
func (m *Mgr) Probe(agentID string, port int) (bool, string) {
	if agentID == "" {
		return false, "未指定目标主机"
	}
	a := m.sess.Get(agentID)
	if a == nil {
		return false, "目标主机离线"
	}
	tunnelID := m.nextTunnel("vp")
	ch := make(chan probeResult, 1)
	m.mu.Lock()
	m.probeWait[tunnelID] = ch
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
func (m *Mgr) DeliverTunnelData(tunnelID string, data []byte) {
	m.mu.Lock()
	vw, ok := m.viewer[tunnelID]
	m.mu.Unlock()
	if !ok || vw == nil {
		return
	}
	vw.wmu.Lock()
	if err := vw.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		vw.wmu.Unlock()
		m.closeViewer(tunnelID)
		return
	}
	vw.wmu.Unlock()
}

// DeliverOpenResp 处理 VNC 隧道的建立响应。
// 隧道建立成功后，flush 缓冲的浏览器 RFB 数据。
func (m *Mgr) DeliverOpenResp(tunnelID string, ok bool, msg string) {
	m.mu.Lock()
	ch, isProbe := m.probeWait[tunnelID]
	vw, isViewer := m.viewer[tunnelID]
	m.mu.Unlock()
	if isProbe {
		ch <- probeResult{ok: ok, msg: msg}
		return
	}
	if isViewer && !ok {
		vw.wmu.Lock()
		_ = vw.ws.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"vnc_unavailable\",\"message\":\""+msg+"\"}"))
		vw.wmu.Unlock()
		m.closeViewer(tunnelID)
		return
	}
	if isViewer && ok {
		// 隧道就绪：标记 ready 并 flush 缓冲的浏览器数据
		var pending [][]byte
		vw.wmu.Lock()
		vw.ready = true
		pending = vw.pending
		vw.pending = nil
		vw.wmu.Unlock()

		agent := m.sess.Get(vw.agentID)
		if agent == nil {
			m.closeViewer(tunnelID)
			return
		}
		for _, d := range pending {
			if err := agent.Write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: d}); err != nil {
				m.closeViewer(tunnelID)
				return
			}
		}
	}
}

// NotifyTunnelClosed 处理 Agent 本地 VNC 隧道关闭通知。
func (m *Mgr) NotifyTunnelClosed(tunnelID string) {
	m.mu.Lock()
	_, isViewer := m.viewer[tunnelID]
	m.mu.Unlock()
	if isViewer {
		m.closeViewer(tunnelID)
	}
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

// HandleViewer 处理浏览器查看器 WebSocket：升级为 WS，建立到 Agent 内网 VNC 的隧道，
// 双向中继 RFB 字节流。
// 关键修复：浏览器 RFB 数据可能在隧道建立前到达，此时先缓冲，等 TunnelOpenResp
// 确认隧道就绪后再转发，避免 RFB 握手数据丢失导致黑屏。
func (m *Mgr) HandleViewer(w http.ResponseWriter, r *http.Request, validateToken func(string) bool) {
	if !validateToken(r.URL.Query().Get("token")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	agentID := agentOf(r)
	port := portOf(r, 5900)
	if agentID == "" {
		http.Error(w, "missing agent", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[vnc] upgrade error: %v", err)
		return
	}
	a := m.sess.Get(agentID)
	if a == nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"error\",\"message\":\"目标主机离线\"}"))
		_ = conn.Close()
		return
	}
	tunnelID := m.nextTunnel("v")
	vw := &viewerConn{ws: conn, agentID: agentID, tunnelID: tunnelID}
	m.mu.Lock()
	m.viewer[tunnelID] = vw
	m.mu.Unlock()

	if err := a.Write(protocol.TypeTunnelOpen, &protocol.TunnelOpenReq{
		TunnelID: tunnelID, AgentID: agentID, LocalHost: "127.0.0.1", LocalPort: port,
	}); err != nil {
		m.closeViewer(tunnelID)
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
			m.closeViewer(tid)
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
			vw.pending = append(vw.pending, data)
			vw.wmu.Unlock()
		}
	}
	m.closeViewer(tunnelID)
}
