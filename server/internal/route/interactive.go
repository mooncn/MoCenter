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
	bufs map[string]*strings.Builder // key: agentID|commandID
	keep int                         // 最多保留的会话数，防止内存无界增长
}

// NewInteractiveGuard 创建交互输入过滤器。
func NewInteractiveGuard() *InteractiveGuard {
	return &InteractiveGuard{bufs: make(map[string]*strings.Builder), keep: 512}
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
		// 简单的容量控制：超过上限时清空重来（极端场景下牺牲半行输入换内存可控）
		if len(r.iguard.bufs) >= r.iguard.keep {
			r.iguard.bufs = make(map[string]*strings.Builder)
		}
		buf = &strings.Builder{}
		r.iguard.bufs[key] = buf
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
func (r *Router) DropInteractive(key string) {
	r.imu.Lock()
	defer r.imu.Unlock()
	if r.iguard != nil {
		delete(r.iguard.bufs, key)
	}
}
