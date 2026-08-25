// Package route 实现命令路由、命令黑名单拦截与操作审计日志。
package route

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

// Router 命令路由器。
type Router struct {
	sessions *session.Manager
	// 命令黑名单
	blacklist []string
	// 审计日志
	auditMu sync.Mutex
	audits  []*AuditLog
}

// AuditLog 操作审计日志。
type AuditLog struct {
	Time       int64  `json:"time"`
	Operator   string `json:"operator"`
	OperatorIP string `json:"operator_ip"`
	AgentID    string `json:"agent_id"`
	Command    string `json:"command"`
	Result     string `json:"result"`
	ExitCode   int    `json:"exit_code"`
	Blocked    bool   `json:"blocked"`
	BlockReason string `json:"block_reason"`
	Duration   int64  `json:"duration_ms"`
}

// NewRouter 创建路由器，加载默认黑名单规则。
func NewRouter(sess *session.Manager) *Router {
	return &Router{
		sessions: sess,
		blacklist: []string{
			"rm -rf /", "rm -rf /*", "mkfs", "dd if=/dev/zero",
			"sudo", "su ", "chmod 777", "useradd", "userdel",
			"nmap", "netcat", "nc -", "iptables -F", "iptables -P",
			"format", "shutdown -r now", "reboot", "poweroff",
		},
	}
}

// IsBlocked 判断命令是否命中黑名单。
func (r *Router) IsBlocked(cmd string) (bool, string) {
	low := strings.ToLower(cmd)
	for _, b := range r.blacklist {
		if strings.Contains(low, strings.ToLower(b)) {
			return true, b
		}
	}
	return false, ""
}

// Dispatch 将命令下发到指定 Agent。
// interactive 为 true 时进入交互模式（通过 WebSocket 实时转发）。
func (r *Router) Dispatch(req *protocol.CommandReq, operator, operatorIP string) (bool, string) {
	if blocked, reason := r.IsBlocked(req.Command); blocked {
		r.Audit(&AuditLog{
			Time: time.Now().Unix(), Operator: operator, OperatorIP: operatorIP,
			AgentID: req.AgentID, Command: req.Command,
			Blocked: true, BlockReason: reason,
		})
		return false, fmt.Sprintf("命令被黑名单拦截: %s", reason)
	}
	conn := r.sessions.Get(req.AgentID)
	if conn == nil {
		r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: operator, AgentID: req.AgentID, Command: req.Command, Result: "agent offline"})
		return false, "目标主机离线"
	}
	if err := conn.Write(protocol.TypeCommandReq, req); err != nil {
		r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: operator, AgentID: req.AgentID, Command: req.Command, Result: err.Error()})
		return false, "下发失败: " + err.Error()
	}
	return true, ""
}

// BatchResult 批量命令单台结果。
type BatchResult struct {
	AgentID string `json:"agent_id"`
	OK      bool   `json:"ok"`
	Msg     string `json:"msg"`
}

// DispatchBatch 将同一命令批量下发到多台在线 Agent。
func (r *Router) DispatchBatch(req *protocol.CommandReq, agentIDs []string, operator, operatorIP string) []*BatchResult {
	results := make([]*BatchResult, 0, len(agentIDs))
	for _, id := range agentIDs {
		clone := *req
		clone.AgentID = id
		ok, msg := r.Dispatch(&clone, operator, operatorIP)
		results = append(results, &BatchResult{AgentID: id, OK: ok, Msg: msg})
	}
	return results
}

// Audit 记录审计日志。
func (r *Router) Audit(l *AuditLog) {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	r.audits = append(r.audits, l)
	if len(r.audits) > 10000 {
		r.audits = r.audits[len(r.audits)-10000:]
	}
	log.Printf("[audit] operator=%s agent=%s cmd=%q blocked=%v", l.Operator, l.AgentID, l.Command, l.Blocked)
}

// Audits 返回审计日志。
func (r *Router) Audits() []*AuditLog {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	out := make([]*AuditLog, len(r.audits))
	copy(out, r.audits)
	return out
}
