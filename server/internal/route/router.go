// Package route 实现命令路由、命令黑名单拦截与操作审计日志。
package route

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
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
	// 持久化文件（可选）：追加写入 JSONL
	auditFile *os.File
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

// NewRouter 创建路由器，加载默认黑名单规则。可传审计持久化文件路径（可选）。
func NewRouter(sess *session.Manager, auditPaths ...string) *Router {
	r := &Router{
		sessions: sess,
		blacklist: []string{
			"rm -rf /", "rm -rf /*", "mkfs", "dd if=/dev/zero",
			"sudo", "su ", "chmod 777", "useradd", "userdel",
			"nmap", "netcat", "nc -", "iptables -F", "iptables -P",
			"format", "shutdown -r now", "reboot", "poweroff",
		},
	}
	if len(auditPaths) > 0 && auditPaths[0] != "" {
		if err := os.MkdirAll(filepath.Dir(auditPaths[0]), 0755); err == nil {
			if f, err := os.OpenFile(auditPaths[0], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				r.auditFile = f
			}
		}
	}
	return r
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
	r.audits = append(r.audits, l)
	if len(r.audits) > 10000 {
		r.audits = r.audits[len(r.audits)-10000:]
	}
	var writeErr error
	if r.auditFile != nil {
		if b, err := json.Marshal(l); err == nil {
			if _, werr := r.auditFile.Write(append(b, '\n')); werr != nil {
				writeErr = werr
			}
		}
	}
	r.auditMu.Unlock()
	log.Printf("[audit] operator=%s agent=%s cmd=%q blocked=%v", l.Operator, l.AgentID, l.Command, l.Blocked)
	if writeErr != nil {
		log.Printf("[audit] 持久化写入失败: %v", writeErr)
	}
}

// Audits 返回审计日志。
func (r *Router) Audits() []*AuditLog {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	out := make([]*AuditLog, len(r.audits))
	copy(out, r.audits)
	return out
}

// AuditQuery 审计日志检索条件。
type AuditQuery struct {
	Agent    string
	Operator string
	Command  string
	Since    int64 // 时间戳，0 表示不限制
	Until    int64
	Offset   int
	Limit    int
}

// SearchAudits 按条件检索审计日志，返回记录的浅拷贝（不含外部队列裁剪影响）。
func (r *Router) SearchAudits(q AuditQuery) []*AuditLog {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	all := make([]*AuditLog, len(r.audits))
	copy(all, r.audits)
	// 初始化为非 nil，确保无匹配时 JSON 序列化返回 [] 而非 null
	out := make([]*AuditLog, 0, len(all))
	for _, l := range all {
		if q.Agent != "" && !strings.Contains(l.AgentID, q.Agent) {
			continue
		}
		if q.Operator != "" && !strings.Contains(l.Operator, q.Operator) {
			continue
		}
		if q.Command != "" && !strings.Contains(l.Command, q.Command) {
			continue
		}
		if q.Since > 0 && l.Time < q.Since {
			continue
		}
		if q.Until > 0 && l.Time > q.Until {
			continue
		}
		out = append(out, l)
	}
	// 分页（按写入顺序，最新在后）
	if q.Offset > 0 && q.Offset < len(out) {
		out = out[q.Offset:]
	}
	if q.Limit > 0 && q.Limit < len(out) {
		out = out[:q.Limit]
	}
	return out
}

// MaskCommand 对命令中的敏感字段做脱敏（如密码、密钥、Token）。
func MaskCommand(cmd string) string {
	if cmd == "" {
		return cmd
	}
	// 形如 key=value / key: value / key value，且 key 为敏感关键字
	re := regexp.MustCompile(`(?i)(password|passwd|secret|token|apikey|api_key|authorization)(\s*[=:]\s*|\s+)(\S+)`)
	cmd = re.ReplaceAllString(cmd, "$1$2***")
	// 短参数形态：-p x / --password x
	re2 := regexp.MustCompile(`(?i)((-p|--password)(\s+))(\S+)`)
	cmd = re2.ReplaceAllString(cmd, "$2$3***")
	return cmd
}
