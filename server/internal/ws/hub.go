// Package ws 实现服务端 WebSocket Hub，向 Web 前端推送实时数据，
// 并接收 Web 前端的命令与端口映射操作。
//
// 依赖 github.com/gorilla/websocket（RFC 6455 标准实现）。
package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/protocol"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Client 代表一个连接的 Web 前端。
type Client struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
}

// Hub 管理所有 WebSocket 客户端，并向上层暴露消息处理回调。
type Hub struct {
	mu        sync.RWMutex
	clients   map[*Client]bool
	onMessage func(data []byte)
}

// NewHub 创建 Hub。
func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]bool)}
}

// SetMessageHandler 设置收到 Web 消息的回调。
func (h *Hub) SetMessageHandler(fn func(data []byte)) {
	h.onMessage = fn
}

// HandleUpgrade 处理 WebSocket 升级请求。token 校验通过后才建立会话。
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request, tokenValidator func(string) bool) {
	token := r.URL.Query().Get("token")
	if tokenValidator == nil || !tokenValidator(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}
	c := &Client{conn: conn, send: make(chan []byte, 256), done: make(chan struct{})}
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
			h.onMessage(data)
		}
	}
}
