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
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 128 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type viewerConn struct {
	ws   *websocket.Conn
	agentID string
	tunnelID string
	wmu  sync.Mutex
}

type probeResult struct {
	ok  bool
	msg string
}

// Mgr VNC 远程桌面管理器。
type Mgr struct {
	mu         sync.Mutex
	sess       *session.Manager
	viewer     map[string]*viewerConn        // tunnelID -> 查看器连接
	probeWait  map[string]chan probeResult   // tunnelID("vp-") -> 探测结果
	installWait map[string]chan *protocol.VncInstallResp // agentID -> 安装结果
	seq        uint64
}

// NewMgr 创建 VNC 管理器。
func NewMgr(sess *session.Manager) *Mgr {
	return &Mgr{
		sess:        sess,
		viewer:      make(map[string]*viewerConn),
		probeWait:   make(map[string]chan probeResult),
		installWait: make(map[string]chan *protocol.VncInstallResp),
	}
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
func (m *Mgr) Install(agentID string, port int) *protocol.VncInstallResp {
	nf := func(msg string) *protocol.VncInstallResp {
		return &protocol.VncInstallResp{OK: false, Message: msg, Port: port}
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

	if err := a.Write(protocol.TypeVncInstallReq, &protocol.VncInstallReq{AgentID: agentID, Port: port}); err != nil {
		return nf("下发安装指令失败: " + err.Error())
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(120 * time.Second):
		return nf("安装超时（可能需更高权限或已占用）")
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
	defer vw.wmu.Unlock()
	if err := vw.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		m.closeViewer(tunnelID)
	}
}

// DeliverOpenResp 处理 VNC 隧道的建立响应。
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

// NotifyInstall 接收 Agent 回传的 VNC 安装/启动结果。
func (m *Mgr) NotifyInstall(agentID string, resp *protocol.VncInstallResp) {
	m.mu.Lock()
	ch, ok := m.installWait[agentID]
	m.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// HandleViewer 处理浏览器查看器 WebSocket：升级为 WS，建立到 Agent 内网 VNC 的隧道，
// 双向中继 RFB 字节流。
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
	m.mu.Lock()
	m.viewer[tunnelID] = &viewerConn{ws: conn, agentID: agentID, tunnelID: tunnelID}
	m.mu.Unlock()

	if err := a.Write(protocol.TypeTunnelOpen, &protocol.TunnelOpenReq{
		TunnelID: tunnelID, AgentID: agentID, LocalHost: "127.0.0.1", LocalPort: port,
	}); err != nil {
		m.closeViewer(tunnelID)
		return
	}
	// 浏览器 -> Agent：读取浏览器 RFB 帧并封装转发
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if a := m.sess.Get(agentID); a != nil {
			if werr := a.Write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: data}); werr != nil {
				break
			}
		} else {
			break
		}
	}
	m.closeViewer(tunnelID)
}