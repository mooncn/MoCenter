package route

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// InteractiveGuard 对交互终端的输入做逐行黑名单检查。
//
// 背景：黑名单原本只作用于 /api/command 的单发与批量命令（Dispatch），
// 而「远程终端」的输入走 InteractiveInput 直转 Agent，完全绕过黑名单 ——
// 主推功能成了最大的旁路。
//
// 做法：按会话缓冲输入流，遇到行结束（CR/LF）时把**已输入的整行**送黑名单；
// 命中则截断 —— 不把行结束符发给 Agent（该行不会被执行），由调用方补发
// Ctrl-C 清掉 Agent 侧已输入的半行，并提示与审计。
type InteractiveGuard struct {
	mu   sync.Mutex
	bufs map[string]*strings.Builder // key: InteractiveKey(agentID, commandID)
	// order 是键的插入顺序（可能有重复项：重复出现的键在淘汰时会被跳过），
	// 用来在触顶时淘汰"最旧"而不是清空整表。
	order []string
	keep  int // 最多保留的会话数，防止内存无界增长
}

// NewInteractiveGuard 创建交互输入过滤器。
func NewInteractiveGuard() *InteractiveGuard {
	return &InteractiveGuard{bufs: make(map[string]*strings.Builder), keep: 512}
}

// InteractiveKey 构造交互输入缓冲的键。
//
// 键的构造与回收必须来自同一个函数：之前调用方自己拼 "agentID|commandID"，
// 而回收侧（DropInteractive）根本没有调用点，于是两边怎么写都对不上（M-N2）。
// 收在一个函数里之后，"往哪写"与"从哪删"只有一处可能写错。
func InteractiveKey(agentID, commandID string) string {
	return agentID + "|" + commandID
}

// Filter 检查一段交互输入，返回 (safe, blocked)。
//
//   - safe：可以转发给 Agent 的部分（通常等于原输入；命中时为命中行之前的内容）
//   - blocked：命中黑名单的整行内容；空串表示本次未命中
func (r *Router) FilterInteractive(key, data string) (safe, blocked string) {
	if data == "" {
		return data, ""
	}
	r.imu.Lock()
	if r.iguard == nil {
		r.iguard = NewInteractiveGuard()
	}
	buf := r.iguard.bufs[key]
	if buf == nil {
		// 容量控制：正常路径靠会话结束时的 DropInteractive 回收（已接线），
		// 这里只兜底"Agent 侧不发 exit 就断开"这类漏收的情况。
		//
		// 兜底动作是**淘汰最旧的一个键**，不是整表清空 —— 旧版把整张 map 换掉，
		// 等于把当时所有活跃终端的半行输入一起丢掉：表现为"别的窗口打着字突然
		// 半行错乱 / 黑名单过滤看起来失灵"。为淘汰最旧，这里只需要一个插入序即可。
		if len(r.iguard.bufs) >= r.iguard.keep {
			r.evictOldestInteractiveLocked()
		}
		buf = &strings.Builder{}
		r.iguard.bufs[key] = buf
		r.iguard.order = append(r.iguard.order, key)
	}
	r.imu.Unlock()

	var out strings.Builder
	for i := 0; i < len(data); {
		rn, size := utf8.DecodeRuneInString(data[i:])
		i += size
		switch rn {
		case '\r', '\n':
			line := strings.ToLower(strings.TrimSpace(buf.String()))
			buf.Reset()
			if line != "" {
				if hit, reason := r.IsBlocked(line); hit {
					// 不转发行结束符：这一行不会被执行
					return out.String(), reason
				}
			}
			out.WriteRune(rn)
		case '\b', 0x7f: // 退格：同步回缩缓冲
			s := buf.String()
			if s != "" {
				for len(s) > 0 {
					_, sz := utf8.DecodeLastRuneInString(s)
					s = s[:len(s)-sz]
					break
				}
			}
			buf.Reset()
			buf.WriteString(s)
			out.WriteRune(rn)
		case 0x03: // Ctrl-C：清空当前行
			buf.Reset()
			out.WriteRune(rn)
		default:
			buf.WriteRune(rn)
			out.WriteRune(rn)
		}
	}
	return out.String(), ""
}

// DropInteractive 交互会话结束时清理缓冲。
//
// 调用点在服务端收到 TypeInteractiveOut(exit) 处，与 ReleaseCommand 同一收口位置。
// 不做这件事的后果见 M-N2：半行输入（可能含口令）驻留到进程退出。
func (r *Router) DropInteractive(key string) {
	if key == "" {
		return
	}
	r.imu.Lock()
	defer r.imu.Unlock()
	if r.iguard != nil {
		delete(r.iguard.bufs, key)
	}
}

// evictOldestInteractiveLocked 淘汰插入序最旧的一个缓冲键（读调用方持 r.imu）。
func (r *Router) evictOldestInteractiveLocked() {
	for len(r.iguard.order) > 0 {
		k := r.iguard.order[0]
		r.iguard.order = r.iguard.order[1:]
		if _, alive := r.iguard.bufs[k]; alive {
			delete(r.iguard.bufs, k)
			return
		}
	}
	// order 已空而 bufs 仍满（理论上不会发生）：退化为清空，保证内存可控。
	r.iguard.bufs = make(map[string]*strings.Builder)
}

// interactiveBufs 仅供测试观察当前缓冲键数量。
func (r *Router) interactiveBufs() int {
	r.imu.Lock()
	defer r.imu.Unlock()
	if r.iguard == nil {
		return 0
	}
	return len(r.iguard.bufs)
}

// iguardKeep 返回交互缓冲的会话上限（仅供测试构造"触顶"场景）。
func (r *Router) iguardKeep() int {
	r.imu.Lock()
	defer r.imu.Unlock()
	if r.iguard == nil {
		r.iguard = NewInteractiveGuard()
	}
	return r.iguard.keep
}
