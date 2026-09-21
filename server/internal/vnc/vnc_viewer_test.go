package vnc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"mocenter/server/internal/session"
)

// newTestWSConn 造一条真实的 WebSocket 连接给 closeViewer 用。
// 之所以不用 nil：名额归还挂在"关闭浏览器连接"这条路径上，
// 塞个 nil 进去会在 Close 处 panic，测不到想测的东西。
func newTestWSConn(t *testing.T) *websocket.Conn {
	t.Helper()

	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		close(ready)
		// 持续读以保持连接存活；对端关闭后读会出错，handler 随之退出。
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))

	client, _, err := websocket.DefaultDialer.DialContext(context.Background(),
		"ws"+srv.URL[len("http"):]+"/vnc-ws", nil)
	if err != nil {
		srv.Close()
		t.Fatalf("拨号测试 WebSocket 失败: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		_ = client.Close()
		srv.Close()
		t.Fatal("测试服务端未完成升级")
	}
	t.Cleanup(func() {
		_ = client.Close() // 先断开对端，handler 才会退出，srv.Close() 才不卡
		srv.Close()
	})
	return client
}

// TestViewerQuotaIsPerSession 并发名额按会话独立计算，不互相挤占。
func TestViewerQuotaIsPerSession(t *testing.T) {
	m := NewMgr(nil)

	for i := 0; i < maxViewersPerSession; i++ {
		if !m.acquireViewer("sess-A") {
			t.Fatalf("第 %d 条 A 会话隧道不应被拒（上限 %d）", i+1, maxViewersPerSession)
		}
	}
	if m.acquireViewer("sess-A") {
		t.Fatal("超出上限的 A 会话隧道应被拒")
	}
	if !m.acquireViewer("sess-B") {
		t.Fatal("B 会话不应被 A 会话占满的名额拖累")
	}
}

// TestReleaseViewerFreesSlotAndKey 归还后要能再开，且计数归零时删除键位。
// 只加不减会让这张表随历史会话无限增长。
func TestReleaseViewerFreesSlotAndKey(t *testing.T) {
	m := NewMgr(nil)
	for i := 0; i < maxViewersPerSession; i++ {
		m.acquireViewer("sess-A")
	}
	m.releaseViewer("sess-A")
	if !m.acquireViewer("sess-A") {
		t.Fatal("归还一个名额后应能再开一条隧道")
	}

	m2 := NewMgr(nil)
	m2.acquireViewer("sess-C")
	m2.releaseViewer("sess-C")
	m2.mu.Lock()
	n := len(m2.viewersBySs)
	m2.mu.Unlock()
	if n != 0 {
		t.Fatalf("全部归还后计数表应为空，实得 %d 项", n)
	}
}

// TestCloseViewerReleasesQuotaOnce 关闭隧道归还名额，且重复关闭不二次扣减。
//
// 回归用例：计数若被扣成负数，`>= maxViewersPerSession` 对负值恒不成立，
// 这条并发防线等于被放开。归还动作挂在"从表中删掉这条隧道"这一步上，
// 因此第二次 closeViewer 找不到条目就不会再扣。
func TestCloseViewerReleasesQuotaOnce(t *testing.T) {
	m := NewMgr(session.NewManager())
	conn := newTestWSConn(t)
	const sess = "sess-A"

	if !m.acquireViewer(sess) {
		t.Fatal("未达上限的隧道不应被拒")
	}
	tunnelID := m.nextTunnel("v")
	m.mu.Lock()
	m.viewer[tunnelID] = &viewerConn{ws: conn, agentID: "a1", session: sess, tunnelID: tunnelID}
	m.mu.Unlock()

	m.closeViewer(tunnelID)
	// 重复关闭不得二次扣减
	m.closeViewer(tunnelID)

	m.mu.Lock()
	got := m.viewersBySs[sess]
	m.mu.Unlock()
	if got != 0 {
		t.Fatalf("关闭后该会话名额应回到 0，实得 %d", got)
	}
}

// VNC 隧道能把键鼠注入送进目标主机的登录会话，所以握手必须认来源。
// 这条断言的作用是：谁把 CheckOrigin 改回"允许任意来源"，这里先红。
func TestVNCUpgraderChecksOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/vnc-ws", nil)
	r.Host = "mc.example.com:8080"
	r.Header.Set("Origin", "http://evil.example")
	if upgrader.CheckOrigin(r) {
		t.Fatal("/vnc-ws 的 upgrader 没有在判同源")
	}
}
