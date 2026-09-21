package ws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/protocol"
)

// addTestClient 登记一个只到内存队列的"客户端"：不建真实 WebSocket 连接，
// 收下的帧直接从 send 里读，用于验证投递范围而不是协议细节。
func addTestClient(h *Hub, session string) (*Client, chan []byte) {
	c := &Client{send: make(chan []byte, 16), done: make(chan struct{}), session: session}
	h.register(c)
	return c, c.send
}

// readFrame 取一帧，超时即失败（避免测试挂死）。
func readFrame(t *testing.T, ch chan []byte, want bool) []byte {
	t.Helper()
	select {
	case msg := <-ch:
		if !want {
			t.Fatalf("不该收到推送，实得 %s", msg)
		}
		return msg
	case <-time.After(300 * time.Millisecond):
		if want {
			t.Fatal("应收到推送，实得空")
		}
		return nil
	}
}

// TestSessionKeyIsStableAndOpaque 会话键必须可复现（REST 与 WS 各自派生后要能对上），
// 又必须与 Token 本身无关（键可能进日志，不能是可重放的凭据）。
func TestSessionKeyIsStableAndOpaque(t *testing.T) {
	if k := SessionKey(""); k != "" {
		t.Fatalf("空 Token 应得空键，实得 %q", k)
	}
	a, b := SessionKey("token-a"), SessionKey("token-a")
	if a != b {
		t.Fatalf("同一 Token 两次派生不一致: %q vs %q", a, b)
	}
	if a == SessionKey("token-b") {
		t.Fatal("不同 Token 撞到了同一个键")
	}
	if strings.Contains(a, "token-a") || len(a) != 32 {
		t.Fatalf("键应是与 Token 无关的定长十六串: %q", a)
	}
}

// TestBroadcastToSessionReachesOnlyOrigin 命令输出只能到达发起方会话。
//
// 回归用例：回执曾走 Broadcast，任一已登录会话都能收到（并按可猜的
// command_id 注入）别人的终端内容。
func TestBroadcastToSessionReachesOnlyOrigin(t *testing.T) {
	h := NewHub()
	_, mine := addTestClient(h, "sess-A")
	_, other := addTestClient(h, "sess-B")
	_, anon := addTestClient(h, "sess-C")

	h.BroadcastToSession("sess-A", protocol.TypeCommandResp, protocol.CommandResp{CommandID: "c-1", Stdout: "hi"})

	msg := readFrame(t, mine, true)
	var env protocol.Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("帧不可解析: %v", err)
	}
	if env.Type != protocol.TypeCommandResp {
		t.Fatalf("消息类型应为 %s，实得 %s", protocol.TypeCommandResp, env.Type)
	}
	var cr protocol.CommandResp
	if err := json.Unmarshal(env.Data, &cr); err != nil || cr.CommandID != "c-1" {
		t.Fatalf("载荷未原样送达: %+v err=%v", cr, err)
	}
	readFrame(t, other, false)
	readFrame(t, anon, false)
}

// TestBroadcastToSessionEmptyKeyFallsBack 无法归属的推送退回全量广播，
// 否则新加的定向逻辑会把结果静默丢掉。
func TestBroadcastToSessionEmptyKeyFallsBack(t *testing.T) {
	h := NewHub()
	_, a := addTestClient(h, "sess-A")
	_, b := addTestClient(h, "sess-B")

	h.BroadcastToSession("", protocol.TypeInteractiveOut, protocol.InteractiveOutput{CommandID: "c-2", Data: "x"})

	readFrame(t, a, true)
	readFrame(t, b, true)
}

// TestBroadcastStillReachesAll 全局广播（主机列表等共享状态）不受定向逻辑影响。
func TestBroadcastStillReachesAll(t *testing.T) {
	h := NewHub()
	_, a := addTestClient(h, "sess-A")
	_, b := addTestClient(h, "")

	h.Broadcast(protocol.TypeAgentList, []string{"h1"})

	readFrame(t, a, true)
	readFrame(t, b, true)
}

// TestSessionFullCountsPerSession 握手并发上限按会话计，到顶才拒。
//
// 回归用例：一枚泄漏的 Token 曾能开任意多条 /ws，把每连接的发送队列和
// 两个泵 goroutine 成倍撑起来；上限判定若整体失效，这里第一次就会漏判。
func TestSessionFullCountsPerSession(t *testing.T) {
	h := NewHub()
	for i := 0; i < maxConnsPerSession; i++ {
		if h.sessionFull("sess-A") {
			t.Fatalf("第 %d 条连接不应判定为已满（上限 %d）", i+1, maxConnsPerSession)
		}
		addTestClient(h, "sess-A")
	}
	if !h.sessionFull("sess-A") {
		t.Fatalf("达到上限 %d 后应拒绝该会话的新握手", maxConnsPerSession)
	}
	if h.sessionFull("sess-B") {
		t.Fatal("其他会话不应被 sess-A 占用的连接数拖累")
	}
}

// TestSessionFullIgnoresEmptyKey 空会话键不受上限约束。
//
// 空键表示"无法归属"，而上限是按会话计的名额，没有键就无从计量；生产路径上
// guardWS 已挡下无 Token 的握手。这里锁定判定语义，别被误改成"空键一律拒绝"。
func TestSessionFullIgnoresEmptyKey(t *testing.T) {
	h := NewHub()
	for i := 0; i < maxConnsPerSession*2; i++ {
		addTestClient(h, "")
	}
	if h.sessionFull("") {
		t.Fatal("空会话键不应触发上限")
	}
}
