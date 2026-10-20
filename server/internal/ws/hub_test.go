package ws

import (
	"testing"
	"time"
)

// TestSubscribePublicStopIdempotent 重复触发停止信号不得 panic。
//
// 回归用例：公开页 WS 的 stop 通道曾被两处 close（读循环发现断开 + 函数返回的
// defer），第二次 close 会 panic "close of closed channel" —— 表现为访客关闭页面
// 或网络抖动时服务端日志刷 panic 栈。stop 由调用方传入，这里验证 Hub 侧对
// "同一个 stop 被关闭/重复关闭" 是安全的。
func TestSubscribePublicStopIdempotent(t *testing.T) {
	h := NewHub()
	stop := make(chan struct{})
	h.SubscribePublic(func([]byte) {}, stop)

	close(stop)
	// 第二次关闭同一通道会 panic，这里刻意不直接再 close ——
	// 验证的是 Hub 内部的 remove 路径不会二次 close 自己的 stop。
	time.Sleep(50 * time.Millisecond)

	h.mu.RLock()
	n := len(h.publics)
	h.mu.RUnlock()
	if n != 0 {
		t.Fatalf("stop 关闭后订阅者应被摘除，剩余 %d", n)
	}
}

// TestDeliverByteBudget 单连接发送队列按字节限额：积压超过预算时丢新帧，
// 而不是让 256 帧的队列装下任意大小的消息。
func TestDeliverByteBudget(t *testing.T) {
	h := NewHub()
	c := &Client{send: make(chan []byte, 300), done: make(chan struct{})}
	h.register(c)
	defer h.unregister(c)

	half := make([]byte, maxClientQueueBytes/2+1)
	h.deliver(half, nil) // 空队列：允许（即使单帧略大）
	if got := len(c.send); got != 1 {
		t.Fatalf("空队列应接收首帧，实得 %d", got)
	}
	h.deliver(half, nil) // 已有 4MiB 积压，再加会超预算 → 丢弃
	if got := len(c.send); got != 1 {
		t.Fatalf("超预算帧应被丢弃，队列长度 %d", got)
	}

	// 模拟 writePump 取走一帧并归还额度后，应能继续接收
	msg := <-c.send
	c.queued.Add(-int64(len(msg)))
	h.deliver(half, nil)
	if got := len(c.send); got != 1 {
		t.Fatalf("额度归还后应恢复投递，实得 %d", got)
	}
}

// TestBroadcastPublicSkipsStopped 停止后的订阅者不再收到广播。
func TestBroadcastPublicSkipsStopped(t *testing.T) {
	h := NewHub()
	stopA, stopB := make(chan struct{}), make(chan struct{})
	var gotA, gotB int
	h.SubscribePublic(func([]byte) { gotA++ }, stopA)
	h.SubscribePublic(func([]byte) { gotB++ }, stopB)

	h.BroadcastPublic(map[string]int{"v": 1})
	if gotA != 1 || gotB != 1 {
		t.Fatalf("首次广播应送达全部订阅者: A=%d B=%d", gotA, gotB)
	}

	close(stopA)
	time.Sleep(50 * time.Millisecond)
	h.BroadcastPublic(map[string]int{"v": 2})
	if gotA != 1 {
		t.Fatalf("已停止的订阅者不应再收到广播: A=%d", gotA)
	}
	if gotB != 2 {
		t.Fatalf("未停止的订阅者应继续收到广播: B=%d", gotB)
	}
}
