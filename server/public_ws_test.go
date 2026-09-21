package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/publicview"
	"mocenter/server/internal/siteconfig"
	"mocenter/server/internal/ws"
)

// newPublicWSTestSite 构造一个只用于公开 WS 的最小 publicSite。
//
// guard 留空（allow 直接放行，本组测试测的是并发/超时/读上限，不是频率限流），
// store 留空 —— 只要把 HostsEnabled 关掉，visibleNodes 就不会碰探针库。
func newPublicWSTestSite(t *testing.T) (*publicSite, *ws.Hub) {
	t.Helper()
	siteCfg := siteconfig.NewStore("")
	cfg := siteCfg.Get()
	cfg.Enabled = true
	cfg.HostsEnabled = false
	if err := siteCfg.Set(cfg); err != nil {
		t.Fatalf("启用公开站点配置失败: %v", err)
	}
	hub := ws.NewHub()
	keys, err := publicview.NewNodeKeys(bytes.Repeat([]byte{0x3c}, 32))
	if err != nil {
		t.Fatalf("构造节点编号密钥失败: %v", err)
	}
	return &publicSite{
		cfg:      siteCfg,
		hub:      hub,
		nodeKeys: keys,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return ws.SameOrigin(r) },
		},
	}, hub
}

// waitFor 轮询直到 cond 为真；超时就失败。用于等待服务端 goroutine 收尾。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）: %s", timeout, msg)
}

// wsSlotsInUse 在锁内读名额计数。waitFor 是从测试 goroutine 轮询的，
// 服务端的 releaseWS 同时在写这个字段，不加锁读就是真实的数据竞争（缺陷在测试侧，不在实现侧）。
func (p *publicSite) wsSlotsInUse() int {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	return p.wsTotal
}

// TestPublicWSSlotCaps 匿名 WS 名额必须同时受每 IP 与全局上限约束，且用完即还。
//
// 背景：这条端点免登录。原来的保护只有"每秒/每分钟频率"，
// 而"把连接开着不走"根本不受频率约束，goroutine 与 hub 订阅项会一路涨。
func TestPublicWSSlotCaps(t *testing.T) {
	p, _ := newPublicWSTestSite(t)

	for i := 0; i < publicWSMaxPerIP; i++ {
		if !p.acquireWS("10.0.0.1") {
			t.Fatalf("第 %d 条同 IP 连接不应被拒（上限 %d）", i+1, publicWSMaxPerIP)
		}
	}
	if p.acquireWS("10.0.0.1") {
		t.Fatal("同一 IP 超过上限后仍拿到名额")
	}
	// 别的地址不受影响
	if !p.acquireWS("10.0.0.2") {
		t.Fatal("不同 IP 不应互相挤占名额")
	}
	p.releaseWS("10.0.0.1")
	if !p.acquireWS("10.0.0.1") {
		t.Fatal("归还后同 IP 应能再次接入")
	}
	// 继续用不同 IP 填满全局名额
	for i := 3; i < publicWSMaxTotal+10; i++ {
		if !p.acquireWS("10.9." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)) {
			break
		}
	}
	if p.acquireWS("192.168.99.99") {
		t.Fatal("全局名额用尽后新地址仍被放行")
	}

	// 全部归还后 per-IP 表必须清空，否则这张表会随访问过的地址单调增长
	for i := 0; i < publicWSMaxTotal*2; i++ {
		p.releaseWS("10.0.0.1")
		p.releaseWS("10.0.0.2")
		p.releaseWS("10.9." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256))
	}
	if p.wsTotal != 0 {
		t.Errorf("归还后全局计数应归零，实际 %d（releaseWS 被重复调用时不得转负）", p.wsTotal)
	}
	if len(p.wsPerIP) != 0 {
		t.Errorf("per-IP 表应删净零值键，实际残留 %d 项", len(p.wsPerIP))
	}
}

// TestPublicWSRejectsOversizedClientFrame 公开 WS 的客户端按设计不发任何数据，
// 因此读上限只需容纳控制帧；超限消息必须直接断掉连接，而不是按声明长度分配内存。
func TestPublicWSRejectsOversizedClientFrame(t *testing.T) {
	p, hub := newPublicWSTestSite(t)
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	conn := dialPublicWS(t, wsURL(srv.URL))
	defer conn.Close()

	// 首帧应当正常到达（服务端连上即推一次当前状态）
	_ = readPublicWSFrame(t, conn)

	if err := conn.WriteMessage(websocket.TextMessage, make([]byte, publicWSReadLimit+1)); err != nil {
		t.Fatalf("客户端发送超限消息失败: %v", err)
	}
	// 服务端应主动断开：客户端侧读到错误/关闭即证
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("超限上行消息被接受了；读上限未生效（未认证内存放大仍可打）")
	}
	waitFor(t, 3*time.Second, func() bool { return hub.PublicSubscribers() == 0 },
		"服务端断开后应摘除该匿名订阅")
}

// TestPublicWSSubscriptionFreedOnDisconnect 访客离开后订阅项必须回收。
//
// 这是原来最容易漏的一条：广播表不清理，每条僵尸连接都会永远被广播遍历到。
func TestPublicWSSubscriptionFreedOnDisconnect(t *testing.T) {
	p, hub := newPublicWSTestSite(t)
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	conn := dialPublicWS(t, wsURL(srv.URL))
	_ = readPublicWSFrame(t, conn)
	if got := hub.PublicSubscribers(); got != 1 {
		t.Fatalf("连接期间应有 1 个订阅者，实际 %d", got)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("关闭客户端失败: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return hub.PublicSubscribers() == 0 },
		"客户端断开后订阅者应归零")
	waitFor(t, 3*time.Second, func() bool { return p.wsSlotsInUse() == 0 },
		"客户端断开后并发名额应归还")
}

// TestPublicWSRejectsWhenSlotsExhausted 名额占满时握手直接被拒（429），不开连接。
func TestPublicWSRejectsWhenSlotsExhausted(t *testing.T) {
	p, _ := newPublicWSTestSite(t)
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	// httptest 的对端地址固定是 127.0.0.1；按上限占满，让下一次握手撞上闸门
	for i := 0; i < publicWSMaxPerIP; i++ {
		if !p.acquireWS("127.0.0.1") {
			t.Fatalf("预占名额失败（第 %d 次）", i+1)
		}
	}
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err == nil {
		t.Fatal("名额占满时握手不应成功")
	}
	if resp == nil {
		t.Fatalf("握手应带回 HTTP 响应，实际 err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("超额握手应返回 429，实际 %d", resp.StatusCode)
	}
}

// TestPublicWSDisabledReturnsNotFound 总开关关闭时端点对外不存在（404），
// 且不会因为握手请求而占用任何名额。
func TestPublicWSDisabledReturnsNotFound(t *testing.T) {
	p, _ := newPublicWSTestSite(t)
	cfg := p.cfg.Get()
	cfg.Enabled = false
	if err := p.cfg.Set(cfg); err != nil {
		t.Fatalf("关闭公开站点配置失败: %v", err)
	}
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL), nil)
	if err == nil {
		t.Fatal("未启用时握手不应成功")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未启用时应返回 404，实际 %+v", resp)
	}
	if p.wsTotal != 0 {
		t.Errorf("未启用的握手不应占名额，实际 wsTotal=%d", p.wsTotal)
	}
}

// wsURL 把 httptest 的 http:// 地址换成 gorilla 可拨的 ws://。
func wsURL(srvURL string) string {
	return "ws://" + strings.TrimPrefix(srvURL, "http://")
}

func dialPublicWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("公开 WS 握手失败: %v (resp=%+v)", err, resp)
	}
	return conn
}

// readPublicWSFrame 读一帧，带超时以免测试挂死。
func readPublicWSFrame(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取公开 WS 首帧失败: %v", err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		trimmed := string(data)
		if len(trimmed) > 200 {
			trimmed = trimmed[:200] + "…"
		}
		t.Fatalf("首帧内容不符合预期: %s", trimmed)
	}
	return data
}

// TestPublicWSRejectsCrossSiteOrigin 匿名公开页端点同样只认同源。
//
// 回归用例：这里曾是 `CheckOrigin: return true`，理由是"公开页可能被嵌在其它站点"。
// 那个理由不成立 —— iframe 里的 socket 带的是**被嵌入文档自己**的 Origin，
// 而前端拼 URL 用的是 location.host，同源必然成立。留着"任意来源"只是给
// 访客浏览器被任意站点驱使着打满我们的连接配额开一扇门。
func TestPublicWSRejectsCrossSiteOrigin(t *testing.T) {
	p, _ := newPublicWSTestSite(t)
	cross := httptest.NewRequest(http.MethodGet, "/api/public/ws", nil)
	cross.Host = "mc.example.com:8080"
	cross.Header.Set("Origin", "http://evil.example")
	if p.upgrader.CheckOrigin(cross) {
		t.Fatal("跨站来源不该能在匿名端点上建长期订阅")
	}
	same := httptest.NewRequest(http.MethodGet, "/api/public/ws", nil)
	same.Host = "mc.example.com:8080"
	same.Header.Set("Origin", "http://mc.example.com:8080")
	if !p.upgrader.CheckOrigin(same) {
		t.Fatal("同源握手被拒，公开页会整片收不到推送")
	}
}

// TestPublicWSInflightClosedOnDisable 运维关掉公开页后，在途匿名连接必须自己结束（M-14）。
//
// 原来"停用"只在握手时查一次开关：已建立的连接会一直看下去，直到访客自己关标签页。
// 而公开页的可见范围是运维随时可能收紧的策略（撤下某台机器、临时隐藏主机），
// 靠访客主动断开等于策略不生效。
func TestPublicWSInflightClosedOnDisable(t *testing.T) {
	p, hub := newPublicWSTestSite(t)
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	conn := dialPublicWS(t, wsURL(srv.URL))
	defer conn.Close()
	_ = readPublicWSFrame(t, conn)
	if got := hub.PublicSubscribers(); got != 1 {
		t.Fatalf("前置条件失效：应有 1 个订阅者，实际 %d", got)
	}

	cfg := p.cfg.Get()
	cfg.Enabled = false
	if err := p.cfg.Set(cfg); err != nil {
		t.Fatalf("关闭公开站点失败: %v", err)
	}
	p.noteConfigChange() // main.go 里这一步由 siteCfg.SetOnChange 触发

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("公开页已停用，在途匿名连接仍能继续接收推送")
	}
	waitFor(t, 3*time.Second, func() bool { return hub.PublicSubscribers() == 0 },
		"停用后匿名订阅应被摘除")
	waitFor(t, 3*time.Second, func() bool { return p.wsSlotsInUse() == 0 },
		"停用后并发名额应归还")
}

// TestPublicWSSurvivesEnabledConfigChange 只是"配置变了"而开关仍开着时，连接不得被误杀。
//
// 反向断言：否则每次改展示配置都会把全站访客踢下线，前端表现为公开页随机断流。
func TestPublicWSSurvivesEnabledConfigChange(t *testing.T) {
	p, hub := newPublicWSTestSite(t)
	srv := httptest.NewServer(p.publicWSHandler())
	defer srv.Close()

	conn := dialPublicWS(t, wsURL(srv.URL))
	defer conn.Close()
	_ = readPublicWSFrame(t, conn)

	p.noteConfigChange() // 开关仍为 true
	hub.BroadcastPublic(map[string]interface{}{"ok": true, "nodes": []interface{}{}})

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("配置变更（仍启用）不应断开访客连接: %v", err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("变更后应继续收到推送，实得 %s", data)
	}
	// 唤醒一次之后仍要能响应下一次停用
	cfg := p.cfg.Get()
	cfg.Enabled = false
	if err := p.cfg.Set(cfg); err != nil {
		t.Fatalf("关闭公开站点失败: %v", err)
	}
	p.noteConfigChange()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("第二次停用未生效：唤醒只起效了一拍")
	}
}
