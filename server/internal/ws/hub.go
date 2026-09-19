// Package ws 实现服务端 WebSocket Hub，向 Web 前端推送实时数据，
// 并接收 Web 前端的命令与端口映射操作。
//
// 依赖 github.com/gorilla/websocket（RFC 6455 标准实现）。
package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/protocol"
)

// WSSubprotocolName WebSocket 子协议名：前端用它携带会话 Token。
//
// 为什么不用 URL query：Token 写进 URL 会被反向代理访问日志、浏览器历史、
// 截图工具留存。子协议头只出现在握手请求里，不会进日志。
const WSSubprotocolName = "mocenter"

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
	Subprotocols:    []string{WSSubprotocolName},
}

// WSToken 从请求中解析会话 Token。
// 优先取 Sec-WebSocket-Protocol 中的第二个子协议（第一个是固定名 "mocenter"），
// 兼容旧的 query 传参方式。
func WSToken(r *http.Request) string {
	if h := r.Header.Get("Sec-WebSocket-Protocol"); h != "" {
		for _, p := range strings.Split(h, ",") {
			p = strings.TrimSpace(p)
			if p == "" || p == WSSubprotocolName {
				continue
			}
			return p
		}
	}
	return r.URL.Query().Get("token")
}

// Client 代表一个连接的 Web 前端。
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	done     chan struct{}
	operator string // 该连接的操作者（登录用户名），用于审计留痕
}

// publicSubscriber 匿名（公开站点）订阅者。
//
// 公开页是免登录的，不能复用 Client（后者绑定 operator 且能发命令），
// 因此单独维护一张订阅表：只收推送、不接受任何上行消息。
// 回调里自带写超时与丢帧保护，避免一个慢客户端拖住 hub 的广播路径。
type publicSubscriber struct {
	fn   func(payload []byte)
	stop chan struct{}
	once sync.Once
}

// Hub 管理所有 WebSocket 客户端，并向上层暴露消息处理回调。
type Hub struct {
	mu        sync.RWMutex
	clients   map[*Client]bool
	publics   map[*publicSubscriber]bool
	onMessage func(operator string, data []byte)
}

// NewHub 创建 Hub。
func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]bool), publics: make(map[*publicSubscriber]bool)}
}

// SubscribePublic 注册一个匿名订阅者，返回取消订阅函数。
//
// stop 由调用方关闭（连接断开时）；收到 stop 后本订阅者会被摘除，
// 期间到达的广播会走 fn —— fn 内部必须自己做非阻塞投递。
func (h *Hub) SubscribePublic(fn func(payload []byte), stop <-chan struct{}) {
	if fn == nil || stop == nil {
		return
	}
	sub := &publicSubscriber{fn: fn, stop: make(chan struct{})}
	// 把外部 stop 桥接到内部通道（只转一次，允许外部重复关闭）
	go func() {
		<-stop
		sub.once.Do(func() { close(sub.stop) })
		h.removePublic(sub)
	}()
	h.mu.Lock()
	h.publics[sub] = true
	h.mu.Unlock()
}

// removePublic 摘除匿名订阅者。
func (h *Hub) removePublic(sub *publicSubscriber) {
	h.mu.Lock()
	delete(h.publics, sub)
	h.mu.Unlock()
	sub.once.Do(func() { close(sub.stop) })
}

// BroadcastPublic 向所有匿名订阅者推送一条脱敏消息。
func (h *Hub) BroadcastPublic(payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ws] public marshal error: %v", err)
		return
	}
	h.mu.RLock()
	subs := make([]*publicSubscriber, 0, len(h.publics))
	for s := range h.publics {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, s := range subs {
		select {
		case <-s.stop:
			continue
		default:
		}
		s.fn(data)
	}
}

// SetMessageHandler 设置收到 Web 消息的回调。operator 为该连接的操作者名称。
func (h *Hub) SetMessageHandler(fn func(operator string, data []byte)) {
	h.onMessage = fn
}

// ResolveOperator 由调用方提供 token -> 操作者名称的解析函数。
// HandleUpgrade 用它把操作者绑定到连接上，供审计使用。
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request, tokenValidator func(string) bool, resolveOperator func(string) string) {
	token := WSToken(r)
	if tokenValidator == nil || !tokenValidator(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	operator := "unknown"
	if resolveOperator != nil {
		if op := resolveOperator(token); op != "" {
			operator = op
		}
	}
	c := &Client{conn: conn, send: make(chan []byte, 256), done: make(chan struct{}), operator: operator}
	h.register(c)
	go h.writePump(c)
	go h.readPump(c)
}

// Broadcast 向所有 Web 客户端推送一条消息。
func (h *Hub) Broadcast(typ string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[ws] marshal error: %v", err)
		return
	}
	env, _ := json.Marshal(protocol.Envelope{Type: typ, Data: data})
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- env:
		default: // 队列满则丢弃该客户端，避免阻塞
		}
	}
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.done)
	}
}

func (h *Hub) writePump(c *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
		h.unregister(c)
	}()
	for {
		select {
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (h *Hub) readPump(c *Client) {
	defer func() {
		_ = c.conn.Close()
		h.unregister(c)
	}()
	_ = c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if h.onMessage != nil {
			h.onMessage(c.operator, data)
		}
	}
}
