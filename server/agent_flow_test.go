package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// counter 记录广播次数的最小桩件。
type counter struct {
	mu    sync.Mutex
	calls int
}

func (c *counter) inc() {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// waitForBroadcasts 轮询等待广播次数达到 want（复用 public_ws_test.go 的 waitFor）。
func waitForBroadcasts(t *testing.T, c *counter, want int, msg string) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool { return c.get() == want }, msg)
}

// TestSoonCoalescesProbeBroadcasts 高频探针只能按窗口放大成有限次广播（M-5）。
//
// 回归用例：每条 ProbeDataReq 都会把整份 store.All() 序列化后发给所有已登录
// 浏览器和公开页订阅者。一条被拿下的 Agent 把上报间隔改成毫秒级，就能把
// 服务端出口和每个操作员标签页的渲染打满 —— 广播次数必须与报文条数解耦。
func TestSoonCoalescesProbeBroadcasts(t *testing.T) {
	c := &counter{}
	r := newAgentRefresh(c.inc, 200*time.Millisecond)

	for i := 0; i < 500; i++ {
		r.soon()
	}
	// 首次立即发一次，其余 499 条合并进同一个窗口
	if got := c.get(); got != 1 {
		t.Fatalf("窗口内应先发 1 次、其余合并，实发 %d 次", got)
	}
	// 窗口末尾的补偿广播必须把最新状态发出去，否则界面停在旧值
	waitForBroadcasts(t, c, 2, "合并后的补偿广播没有发出")

	time.Sleep(250 * time.Millisecond)
	if got := c.get(); got != 2 {
		t.Fatalf("合并结果应为 2 次（首发 + 补偿），实发 %d 次", got)
	}
}

// TestSoonWindowResumes 窗口过去后下一条又能立刻发。
func TestSoonWindowResumes(t *testing.T) {
	c := &counter{}
	r := newAgentRefresh(c.inc, 100*time.Millisecond)
	r.soon()
	r.soon() // 窗口内，合并
	if got := c.get(); got != 1 {
		t.Fatalf("第二条应被合并，实发 %d 次", got)
	}
	waitForBroadcasts(t, c, 2, "补偿广播没有发出")
	// 补偿广播自己就是新的一轮窗口的起点：等它过期，才能验"下一条立刻发"
	time.Sleep(120 * time.Millisecond)
	before := c.get()
	r.soon()
	if c.get() != before+1 {
		t.Fatalf("窗口结束后应立刻再发一次，实发 %d→%d", before, c.get())
	}
}

// TestNowBypassesAndCancelsWindow 状态跃迁立刻广播，并取消待发的补偿广播。
//
// 上线/离线不能等节流窗口（慢一秒在界面上就是"主机没下来"）；而窗口里已经挂了
// 补偿定时器的话不取消，就会紧跟着再发一份同样的列表。
func TestNowBypassesAndCancelsWindow(t *testing.T) {
	c := &counter{}
	r := newAgentRefresh(c.inc, 100*time.Millisecond)
	r.soon() // 发 1 次并挂上补偿定时器
	r.now()  // 立刻再发 1 次，同时撤掉那个定时器
	if got := c.get(); got != 2 {
		t.Fatalf("now() 应立刻广播（累计 2 次），实发 %d 次", got)
	}
	time.Sleep(250 * time.Millisecond)
	if got := c.get(); got != 2 {
		t.Fatalf("被 now() 取代的补偿广播不应再发，实发 %d 次", got)
	}
}

// TestLogSafeKillsInjectedNewlines 外部可控文本进日志前要压成单行、限长。
//
// Agent 自报的 UUID/主机名里塞一个 \n，就能在日志里伪造出一行
// "[server] Agent xxx 上线" 这类看着像服务端结论的记录。
func TestLogSafeKillsInjectedNewlines(t *testing.T) {
	got := logSafe("AAAA\n[server] Agent admin 上线\n\tB")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("日志文本里仍含换行/制表符: %q", got)
	}
	if !strings.Contains(got, "AAAA") || !strings.Contains(got, "B") {
		t.Fatalf("内容不应被吞掉: %q", got)
	}

	long := logSafe(strings.Repeat("x", logSafeMax*3))
	if len(long) > logSafeMax+len("…")+1 {
		t.Fatalf("超长文本未被截断，长度 %d", len(long))
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("截断应留下省略号: %q", long)
	}
}

// TestUUIDSuffixSanitizes uuidSuffix 直接把自报 UUID 拼进日志，同样要过闸。
func TestUUIDSuffixSanitizes(t *testing.T) {
	if s := uuidSuffix(""); s != "" {
		t.Fatalf("空 UUID 不应产生后缀，实得 %q", s)
	}
	if s := uuidSuffix("X\n[server] 伪造"); strings.Contains(s, "\n") {
		t.Fatalf("uuidSuffix 未清洗换行: %q", s)
	}
}
