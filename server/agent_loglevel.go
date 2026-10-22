// Agent 运行期日志级别的远程下发。
//
// 需求：控制台可以按主机（或全部在线主机）修改 Agent 的日志级别，
// 用于"平时只留 error，排障时临时开到 info"。
//
// 安全边界：这条通道只能改日志级别，不能让 Agent 打开任何危险开关
// （FileRoots / tunnel_hosts / 公网取包仍然只认 Agent 本机配置）。
package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/logx"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

// logLevelAckRegistry 收集 Agent 对 LogLevelUpdate 的回执。
type logLevelAckRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan protocol.LogLevelUpdateAck
}

var logLevelAcks = &logLevelAckRegistry{waiters: make(map[string]chan protocol.LogLevelUpdateAck)}

func (r *logLevelAckRegistry) begin(agentID string) chan protocol.LogLevelUpdateAck {
	ch := make(chan protocol.LogLevelUpdateAck, 1)
	r.mu.Lock()
	r.waiters[agentID] = ch
	r.mu.Unlock()
	return ch
}

func (r *logLevelAckRegistry) end(agentID string, ch chan protocol.LogLevelUpdateAck) {
	r.mu.Lock()
	if r.waiters[agentID] == ch {
		delete(r.waiters, agentID)
	}
	r.mu.Unlock()
}

func (r *logLevelAckRegistry) deliver(ack protocol.LogLevelUpdateAck) {
	r.mu.Lock()
	ch := r.waiters[ack.AgentID]
	r.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ack:
	default:
	}
}

type logLevelPushResult struct {
	AgentID string `json:"agent_id"`
	Level   string `json:"level"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// agentLogLevelHandler 设置 Agent 日志级别。
//
//	POST /api/agents/log-level
//	{"agent_id":"web-01","level":"info"}   单机
//	{"all":true,"level":"error"}           全部在线主机
//
// 级别校验与 Agent 侧同源（off/error/warn/info）；非法值直接拒绝，
// 不会把"未知级别"写进 Agent 配置。
func agentLogLevelHandler(sess *session.Manager, authz *auth.Auth, audit func(*http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			AgentID string `json:"agent_id"`
			Level   string `json:"level"`
			All     bool   `json:"all"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		lv, ok := logx.ParseLevel(body.Level)
		if !ok || strings.TrimSpace(body.Level) == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "不支持的日志级别（可用 off / error / warn / info）"})
			return
		}
		level := lv.String()

		type target struct {
			id   string
			conn *session.Conn
			ch   chan protocol.LogLevelUpdateAck
		}
		var targets []target
		if body.All {
			for _, c := range sess.All() {
				id := c.GetAgentID()
				if id == "" {
					continue
				}
				ch := logLevelAcks.begin(id)
				targets = append(targets, target{id: id, conn: c, ch: ch})
			}
			if len(targets) == 0 {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "当前没有在线主机"})
				return
			}
		} else {
			id := strings.TrimSpace(body.AgentID)
			if id == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "必须指定 agent_id，或选择全部在线主机"})
				return
			}
			c := sess.Get(id)
			if c == nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "主机不在线，无法下发；它下次启动时会按本机配置的级别运行"})
				return
			}
			ch := logLevelAcks.begin(id)
			targets = append(targets, target{id: id, conn: c, ch: ch})
		}

		// 先发完再等回执：逐台等会让"全部主机"退化成串行 RTT 累加。
		var sent []target
		var failed []logLevelPushResult
		for _, t := range targets {
			if err := t.conn.Write(protocol.TypeLogLevelUpdate, protocol.LogLevelUpdate{Level: level}); err != nil {
				logLevelAcks.end(t.id, t.ch)
				failed = append(failed, logLevelPushResult{AgentID: t.id, OK: false, Message: "下发失败: " + err.Error()})
				continue
			}
			sent = append(sent, t)
		}

		results := make(chan logLevelPushResult, len(sent))
		for _, t := range sent {
			go func(t target) {
				select {
				case ack := <-t.ch:
					results <- logLevelPushResult{AgentID: t.id, Level: ack.Level, OK: ack.OK, Message: ack.Message}
				case <-time.After(3 * time.Second):
					results <- logLevelPushResult{AgentID: t.id, OK: false,
						Message: "未收到回执（旧版 Agent 不认识该指令，升级后才会生效）"}
				}
			}(t)
		}
		okCount := 0
		for _, t := range sent {
			res := <-results
			logLevelAcks.end(t.id, t.ch)
			if res.OK {
				okCount++
			} else {
				failed = append(failed, res)
			}
		}

		if audit != nil {
			targetDesc := "主机 " + targets[0].id
			if body.All {
				targetDesc = fmt.Sprintf("全部在线主机（%d 台）", len(targets))
			}
			audit(r, fmt.Sprintf("修改 Agent 日志级别为 %s（%s，成功 %d 台）", level, targetDesc, okCount))
		}
		logx.Infof("[server] Agent 日志级别下发为 %s：成功 %d 台，失败 %d 台", level, okCount, len(failed))
		writeJSON(w, map[string]interface{}{
			"ok":       okCount > 0,
			"level":    level,
			"notified": len(targets),
			"acked":    okCount,
			"failed":   failed,
			"msg": fmt.Sprintf("日志级别已下发为 %s：%d/%d 台确认生效%s",
				level, okCount, len(targets), failedSuffix(failed)),
		})
	}
}

func failedSuffix(failed []logLevelPushResult) string {
	if len(failed) == 0 {
		return ""
	}
	ids := make([]string, 0, len(failed))
	for _, f := range failed {
		ids = append(ids, f.AgentID)
	}
	return "；未生效: " + strings.Join(ids, ", ")
}
