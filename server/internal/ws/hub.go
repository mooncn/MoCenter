// Package ws 实现服务端 WebSocket Hub，向 Web 前端推送实时数据，
// 并接收 Web 前端的命令与端口映射操作。
//
// 推送分两档：全局广播（主机列表等共享状态）与按会话定向（命令输出、终端回显），
// 后者的键由 SessionKey 从登录 Token 派生。
//
// 依赖 github.com/gorilla/websocket（RFC 6455 标准实现）。
package ws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mocenter/server/internal/logx"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/protocol"
)

// WSSubprotocolName WebSocket 子协议名：前端用它携带会话 Token。
//
// 为什么不用 URL query：Token 写进 URL 会被反向代理访问日志、浏览器历史、
// 截图工具留存。子协议头只出现在握手请求里，不会进日志。
const WSSubprotocolName = "mocenter"

const (
	// maxConnsPerSession 单个操作员会话的并发连接上限。
	//
	// 一个浏览器标签页一条连接，正常用量远够。不设上限时，一枚泄漏的 Token
	// 就能开任意多条连接 —— 每条两个 goroutine 加 256 帧发送队列，等于把
	// "凭据泄露"直接放大成服务端的内存与调度压力。
	maxConnsPerSession = 8
	// wsReadLimit 单帧上限。Web 端上行只有三类小报文（命令下发、终端输入、
	// 端口映射操作），最大的是文件分发的 64 KiB 分片 base64 后约 88 KB，
	// 故 1 MiB 既能容下正常用法，又不给"声明超大帧让服务端按长度分配"留空间。
	// 与公开页那条（public_site.go 的 64 字节，按设计零上行）不同口径。
	wsReadLimit = 1 << 20
	// maxClientQueueBytes 单个连接发送队列的字节预算。
	//
	// 队列原来只限 256 帧，但"帧"没有大小口径：一条命令结果、一份主机列表
	// 都可以是几 MB，256 帧就等于把单个慢客户端的积压放大到上 GB。
	// 这里再按字节设一道闸，超过即丢帧（与队列满同语义，前端靠下一次全量推送收敛）。
	maxClientQueueBytes = 8 << 20
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	// 见 SameOrigin：浏览器的 WS 握手不吃 CORS，来源必须由服务端自己认。
	CheckOrigin:  func(r *http.Request) bool { return SameOrigin(r) },
	Subprotocols: []string{WSSubprotocolName},
}

// WSToken 从请求中解析会话 Token —— 只认子协议头。
//
// 第二子协议放 Token（第一个是固定名 "mocenter"）。原先还兼容 `?token=`：
// 那一条会把凭据写进访问日志、浏览器历史与 Referer，而且 H-2 之后
// guardWS 与握手两处都要各自记得"query 也算"，等于给收紧留了个洞。
// 前端（api.js）从来只用子协议，故直接摘掉这条回退。
func WSToken(r *http.Request) string {
	h := r.Header.Get("Sec-WebSocket-Protocol")
	for _, p := range strings.Split(h, ",") {
		p = strings.TrimSpace(p)
		if p == "" || p == WSSubprotocolName {
			continue
		}
		return p
	}
	return ""
}

// SessionKey 把一个会话 Token 归一为定长、不可逆的键，用于把推送定向回发起方会话。
//
// 为什么哈希而不是直接用 Token：这个键会长期留在映射与命令绑定里，也可能出现在
// 日志里；而 Token 是可重放的凭据，不该以明文形态在进程内多存一份。
// 空 Token 返回空键 —— 空键表示"无法归属"，调用方据此退回广播或丢弃。
func SessionKey(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// Client 代表一个连接的 Web 前端。
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	done     chan struct{}
	operator string // 该连接的操作者（登录用户名），用于审计留痕
	session  string // 会话键（见 SessionKey），用于把命令输出定向回发起方
	// queued 已入队但尚未写出的字节数（见 maxClientQueueBytes）。
	queued atomic.Int64
	// guest 标记分享访客连接：只收本会话的定向消息。
	// 全局广播（主机清单、端口映射状态等）对访客是越权信息面，
	// 一条只分享 A 机桌面的链接不应该换来整张资产表。
	guest bool
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
	mu      sync.RWMutex
	clients map[*Client]bool
	publics map[*publicSubscriber]bool
	// onMessage 的 session 参数是发起方会话键（见 SessionKey），上层据此把
	// 命令绑定到"谁下发的"，而不是只知道"哪个用户名"。
	onMessage func(session, operator string, data []byte)
}

// NewHub 创建 Hub。
func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]bool), publics: make(map[*publicSubscriber]bool)}
}

// SubscribePublic 注册一个匿名订阅者。
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

// PublicSubscribers 返回当前匿名订阅者数量。
//
// 公开页免登录，连接泄漏会直接体现为这张表的单调增长，故留出可观测口径。
func (h *Hub) PublicSubscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.publics)
}

// BroadcastPublic 向所有匿名订阅者推送一条脱敏消息。
func (h *Hub) BroadcastPublic(payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		logx.Infof("[ws] public marshal error: %v", err)
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

// SetMessageHandler 设置收到 Web 消息的回调。
// session 为该连接的会话键（见 SessionKey），operator 为登录用户名。
func (h *Hub) SetMessageHandler(fn func(session, operator string, data []byte)) {
	h.onMessage = fn
}

// ResolveOperator 由调用方提供 token -> 操作者名称的解析函数。
// HandleUpgrade 用它把操作者绑定到连接上，供审计与定向回投使用。
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request, tokenValidator func(string) bool, resolveOperator func(string) string) {
	token := WSToken(r)
	if tokenValidator == nil || !tokenValidator(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	operator := "unknown"
	if resolveOperator != nil {
		if op := resolveOperator(token); op != "" {
			operator = op
		}
	}
	h.upgrade(w, r, SessionKey(token), operator, false)
}

// HandleUpgradeShare 为分享链接的访客建立连接。
//
// 访客没有登录 Token，凭据已在调用方（main.go 的 /ws 路由）校验过；
// 这里只负责绑定该分享的会话键，并把 operator 标成 "share:<id>"，
// 上层据此把访客的上行消息限制为终端输入。
func (h *Hub) HandleUpgradeShare(w http.ResponseWriter, r *http.Request, session, operator string) {
	h.upgrade(w, r, session, operator, true)
}

func (h *Hub) upgrade(w http.ResponseWriter, r *http.Request, session, operator string, guest bool) {
	// 并发上限必须在 Upgrade **之前**判：升级成功后响应已是 101，再写 429 只是
	// 一行 "response.WriteHeader on hijacked response" 的服务端日志，浏览器什
	// 么也看不出。判定与登记之间有极小的窗口（同一会话同时握够 8 次会略微超额），
	// 换来的是拒绝路径干净，这个取舍可以接受。
	if h.sessionFull(session) {
		logx.Errorf("[ws] 会话 %s… 连接数已达上限 %d，拒绝握手（来自 %s）", SessionPrefix(session), maxConnsPerSession, r.RemoteAddr)
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logx.Infof("[ws] upgrade error: %v", err)
		return
	}
	c := &Client{
		conn:     conn,
		send:     make(chan []byte, 256),
		done:     make(chan struct{}),
		operator: operator,
		session:  session,
		guest:    guest,
	}
	h.register(c)
	go h.writePump(c)
	go h.readPump(c)
}

// CloseSession 关闭某会话的全部连接（分享撤销/过期时使用）。
// 返回被关闭的连接数。连接对象从表中摘除后由各自的泵收尾，避免重复关闭通道。
func (h *Hub) CloseSession(session string) int {
	if session == "" {
		return 0
	}
	h.mu.Lock()
	var victims []*Client
	for c := range h.clients {
		if c.session != session {
			continue
		}
		delete(h.clients, c)
		close(c.done)
		victims = append(victims, c)
	}
	h.mu.Unlock()
	for _, c := range victims {
		_ = c.conn.Close()
	}
	return len(victims)
}

// sessionFull 判断某会话当前连接数是否已达上限。
// 会话键为空（拿不到 Token 的入口）不受此限：能走到这里的连接都已通过
// guardWS 的 Token 校验，正常不会带空键，留这一支只是为了不把内部入口锁死。
func (h *Hub) sessionFull(session string) bool {
	if session == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for c := range h.clients {
		if c.session == session {
			n++
		}
	}
	return n >= maxConnsPerSession
}

// SessionPrefix 返回会话键的短前缀，只用于日志定位（键可能同时有多个）。
func SessionPrefix(session string) string {
	if len(session) > 8 {
		return session[:8]
	}
	return session
}

// Broadcast 向所有 Web 客户端推送一条消息。
func (h *Hub) Broadcast(typ string, payload interface{}) {
	env, err := marshalEnv(typ, payload)
	if err != nil {
		logx.Infof("[ws] marshal error: %v", err)
		return
	}
	h.deliver(env, nil)
}

// BroadcastToSession 只向发起方会话推送，其余操作员收不到。
//
// 用途：命令回显与远程终端输出。这类内容属于"谁下发、谁看"，早期实现走
// Broadcast，于是任一已登录会话都能读到（并按 command_id 注入）别人的终端。
// session 为空表示这条推送无法归属到某个会话（例如未带 Token 的内部入口），
// 此时退回广播，行为与修复前一致，不会把结果悄悄丢掉。
func (h *Hub) BroadcastToSession(session, typ string, payload interface{}) {
	env, err := marshalEnv(typ, payload)
	if err != nil {
		logx.Infof("[ws] marshal error: %v", err)
		return
	}
	if session == "" {
		h.deliver(env, nil)
		return
	}
	h.deliver(env, func(c *Client) bool { return c.session == session })
}

// marshalEnv 打包成前端约定的 Envelope（Data 为 JSON 字节）。
func marshalEnv(typ string, payload interface{}) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Envelope{Type: typ, Data: data})
}

// deliver 把已序列化的消息投递给客户端；match 为 nil 表示全量投递。
// 队列满则丢弃该客户端的这帧，避免一个慢连接阻塞广播路径。
//
// 全量投递必须排除分享访客：他们只该收到自己会话的定向消息
// （终端输出），而不是主机清单/端口映射状态这类控制台共享数据。
func (h *Hub) deliver(env []byte, match func(*Client) bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if match == nil {
			if c.guest {
				continue
			}
		} else if !match(c) {
			continue
		}
		size := int64(len(env))
		// 队列里已有积压时按字节限额丢弃；空队列允许单帧超预算通过，
		// 否则一条略超 8 MiB 的命令结果会在队列空时也被静默扔掉。
		if cur := c.queued.Load(); cur > 0 && cur+size > maxClientQueueBytes {
			continue
		}
		c.queued.Add(size)
		select {
		case c.send <- env:
		default:
			c.queued.Add(-size)
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
			c.queued.Add(-int64(len(msg)))
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
	c.conn.SetReadLimit(wsReadLimit)
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if h.onMessage != nil {
			h.onMessage(c.session, c.operator, data)
		}
	}
}
