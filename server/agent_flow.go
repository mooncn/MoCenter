package main

import (
	"sync"
	"time"
)

// agentRefreshInterval 主机列表节流广播的最小间隔。
//
// 探针默认 30s 一条 * 全部主机，每条都要把整份 store.All() 序列化后发给所有
// 已登录浏览器 + 公开页订阅者。主机数一多，或被拿下的 Agent 把上报间隔改成
// 亚秒级，这条路径就能把服务端出口和每个操作员标签页的渲染打满（M-5）。
// 取 2s：面板上没有任何指标需要秒级精度，而人眼也分辨不出 2s 的滞后。
const agentRefreshInterval = 2 * time.Second

// agentRefresh 把"主机数据变了"合并成最多每 interval 一次的全量广播。
//
// 两类调用点语义不同，因此分开两个入口而不是一个函数带开关：
//   - now()：上线/离线这类状态跃迁，必须立刻可见（慢一秒就是"主机没下来"）；
//   - soon()：探针这种高频数据源，允许合并。
//
// 合并不能把最后一次变更吃掉，否则界面会停在旧值不再更新 —— 所以落在窗口内
// 的变更会挂一个补偿定时器。
type agentRefresh struct {
	broadcast func()
	interval  time.Duration

	mu    sync.Mutex
	last  time.Time
	timer *time.Timer
}

func newAgentRefresh(broadcast func(), interval time.Duration) *agentRefresh {
	if interval <= 0 {
		interval = agentRefreshInterval
	}
	return &agentRefresh{broadcast: broadcast, interval: interval}
}

// now 立即广播一次，并取消尚未触发的补偿广播（避免紧接着又发一份同样的数据）。
func (r *agentRefresh) now() {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.last = time.Now()
	r.mu.Unlock()
	r.broadcast()
}

// soon 节流广播一次：距上次不足 interval 时合并到窗口末尾。
func (r *agentRefresh) soon() {
	if r == nil {
		return
	}
	r.mu.Lock()
	wait := r.interval - time.Since(r.last)
	if wait <= 0 {
		r.last = time.Now()
		r.mu.Unlock()
		r.broadcast()
		return
	}
	if r.timer == nil {
		r.timer = time.AfterFunc(wait, r.fire)
	}
	r.mu.Unlock()
}

// fire 补偿广播：窗口结束后把最新状态发出去，并把这一时刻记为"已广播"。
func (r *agentRefresh) fire() {
	r.mu.Lock()
	r.timer = nil
	r.last = time.Now()
	r.mu.Unlock()
	r.broadcast()
}

// maxConcurrentPkgFetch 单条 Agent 连接上并行的安装包下发流数。
//
// 每条 PkgFetchReq 会起一路"持续往这条连接写分片"的协程；不设上限时一条连接
// 就能用几十路并发把服务端出口和内存占满（还会互相抢带宽把每路都拖死）。
// Agent 侧本身是串行发起的，取 2 留了一路余量。
const maxConcurrentPkgFetch = 2
