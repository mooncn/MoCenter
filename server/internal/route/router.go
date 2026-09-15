// Package route 实现命令路由、命令黑名单拦截与操作审计日志。
package route

import (
	"bufio"
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
	"mocenter/server/internal/storage"
)

// Router 命令路由器。
type Router struct {
	sessions *session.Manager
	// 命令黑名单
	blacklist []string
	blackMu   sync.RWMutex
	// 审计日志
	auditMu sync.Mutex
	audits  []*AuditLog
	// command_id -> 审计记录 ID 映射（用于命令结果回填）
	cmdAudit map[string]int64
	// 持久化文件（可选）：追加写入 JSONL
	auditFile *os.File
	// SQLite 持久化（可选）
	db *storage.DB
	// 交互终端输入的黑名单过滤状态（惰性初始化）
	imu    sync.Mutex
	iguard *InteractiveGuard
}

// AuditLog 操作审计日志。
type AuditLog struct {
	ID          int64  `json:"id"`
	Time        int64  `json:"time"`
	Operator    string `json:"operator"`
	OperatorIP  string `json:"operator_ip"`
	AgentID     string `json:"agent_id"`
	CommandID   string `json:"command_id"`
	Command     string `json:"command"`
	Result      string `json:"result"`
	ExitCode    int    `json:"exit_code"`
	Blocked     bool   `json:"blocked"`
	BlockReason string `json:"block_reason"`
	Duration    int64  `json:"duration_ms"`
	// Category 审计类别：command（命令执行）/ op（运维操作）/ auth（登录认证）。
	// 由 SQLite 列 category 提供，内存日志未设置时为空（按命令执行处理）。
	Category string `json:"category"`
}

// DefaultBlacklistPatterns 内置黑名单规则列表，用于 SQLite 初始化（幂等）。
var DefaultBlacklistPatterns = []string{
	"rm -rf /", "rm -rf /*", "mkfs", "dd if=/dev/zero",
	"sudo", "su ", "chmod 777", "useradd", "userdel",
	"nmap", "netcat", "nc -", "iptables -F", "iptables -P",
	"format", "shutdown -r now", "reboot", "poweroff",
}

// NewRouter 创建路由器，加载默认黑名单规则。可传审计持久化文件路径（可选）。
// 若提供 SQLite DB，则审计日志同时写入数据库，查询也从数据库读取。
func NewRouter(sess *session.Manager, db *storage.DB, auditPaths ...string) *Router {
	r := &Router{
		sessions:  sess,
		blacklist: append([]string{}, DefaultBlacklistPatterns...),
		cmdAudit:  make(map[string]int64),
		db:        db,
	}
	if len(auditPaths) > 0 && auditPaths[0] != "" {
		if err := os.MkdirAll(filepath.Dir(auditPaths[0]), 0755); err == nil {
			// 导入历史 JSONL 审计（在打开文件追加句柄之前完成，避免读到未落盘内容）
			r.importLegacyAudits(auditPaths[0])
			if f, err := os.OpenFile(auditPaths[0], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				r.auditFile = f
			}
		}
	}
	// 从 SQLite 加载黑名单（合并内置和自定义）
	if db != nil {
		r.loadBlacklistFromDB()
	}
	return r
}

// importLegacyAudits 将历史遗留的 audits.jsonl 记录导入 SQLite。
// 背景：早期版本只把审计写入 JSONL 文件，查询却只读 SQLite，
// 导致老记录在「操作审计」页面完全不可见。此处做一次性补录（幂等、去重）。
func (r *Router) importLegacyAudits(path string) {
	if r.db == nil || path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return // 文件不存在：无需导入
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	imported, skipped, failed := 0, 0, 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row storage.AuditLogRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			failed++
			continue
		}
		// 去重：同一时间 + 同一命令视为已存在
		exists, err := r.db.AuditLogExists(row.Time, row.Command)
		if err != nil {
			log.Printf("[audit] 导入历史审计时查询失败: %v", err)
			failed++
			continue
		}
		if exists {
			skipped++
			continue
		}
		if _, err := r.db.InsertAuditLog(&row); err != nil {
			log.Printf("[audit] 导入历史审计失败: %v", err)
			failed++
			continue
		}
		imported++
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[audit] 读取历史审计文件出错: %v", err)
	}
	if imported > 0 || failed > 0 {
		log.Printf("[audit] 历史审计导入完成：新增 %d 条，跳过重复 %d 条，失败 %d 条", imported, skipped, failed)
	}
}

// Close 关闭审计文件句柄，释放资源（服务退出时调用）。
func (r *Router) Close() error {
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	if r.auditFile != nil {
		err := r.auditFile.Close()
		r.auditFile = nil
		return err
	}
	return nil
}

// loadBlacklistFromDB 从数据库加载黑名单规则到内存。
func (r *Router) loadBlacklistFromDB() {
	rows, err := r.db.LoadBlacklist()
	if err != nil {
		log.Printf("[route] 从数据库加载黑名单失败: %v", err)
		return
	}
	r.blackMu.Lock()
	defer r.blackMu.Unlock()
	// 先用内置默认，再追加数据库中的自定义规则
	var list []string
	builtinSet := make(map[string]bool)
	for _, b := range r.blacklist {
		list = append(list, b)
		builtinSet[strings.ToLower(b)] = true
	}
	for _, row := range rows {
		low := strings.ToLower(row.Pattern)
		if !builtinSet[low] {
			list = append(list, row.Pattern)
		}
	}
	r.blacklist = list
}

// AddBlacklist 添加一条自定义黑名单规则并持久化。
func (r *Router) AddBlacklist(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("规则不能为空")
	}
	r.blackMu.Lock()
	// 检查是否已存在
	for _, b := range r.blacklist {
		if strings.EqualFold(b, pattern) {
			r.blackMu.Unlock()
			return fmt.Errorf("规则已存在")
		}
	}
	r.blacklist = append(r.blacklist, pattern)
	r.blackMu.Unlock()
	if r.db != nil {
		if err := r.db.InsertBlacklist(pattern, false); err != nil {
			return err
		}
	}
	return nil
}

// RemoveBlacklist 删除一条自定义黑名单规则并持久化（不允许删除内置规则）。
func (r *Router) RemoveBlacklist(pattern string) error {
	r.blackMu.Lock()
	found := false
	var newList []string
	for _, b := range r.blacklist {
		if strings.EqualFold(b, pattern) {
			found = true
			continue
		}
		newList = append(newList, b)
	}
	if found {
		r.blacklist = newList
	}
	r.blackMu.Unlock()
	if !found {
		return fmt.Errorf("规则不存在")
	}
	if r.db != nil {
		if err := r.db.DeleteBlacklist(pattern); err != nil {
			return err
		}
	}
	return nil
}

// GetBlacklist 返回当前黑名单列表。
func (r *Router) GetBlacklist() []string {
	r.blackMu.RLock()
	defer r.blackMu.RUnlock()
	out := make([]string, len(r.blacklist))
	copy(out, r.blacklist)
	return out
}

// IsBlocked 判断命令是否命中黑名单。
func (r *Router) IsBlocked(cmd string) (bool, string) {
	low := strings.ToLower(cmd)
	r.blackMu.RLock()
	defer r.blackMu.RUnlock()
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
			AgentID: req.AgentID, CommandID: req.CommandID, Command: req.Command,
			Blocked: true, BlockReason: reason,
		})
		return false, fmt.Sprintf("命令被黑名单拦截: %s", reason)
	}
	conn := r.sessions.Get(req.AgentID)
	if conn == nil {
		r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: operator, OperatorIP: operatorIP,
			AgentID: req.AgentID, CommandID: req.CommandID, Command: req.Command, Result: "agent offline"})
		return false, "目标主机离线"
	}
	// 先落审计（命令下发即留痕，操作人明确），结果由 CommandResp 回填
	id := r.Audit(&AuditLog{Time: time.Now().Unix(), Operator: operator, OperatorIP: operatorIP,
		AgentID: req.AgentID, CommandID: req.CommandID, Command: req.Command})
	// 记录 command_id -> 审计ID 映射，供结果回填
	if req.CommandID != "" {
		r.auditMu.Lock()
		if r.cmdAudit == nil {
			r.cmdAudit = make(map[string]int64)
		}
		r.cmdAudit[req.CommandID] = id
		// 防止映射无限增长
		if len(r.cmdAudit) > 20000 {
			for k := range r.cmdAudit {
				delete(r.cmdAudit, k)
				if len(r.cmdAudit) <= 10000 {
					break
				}
			}
		}
		r.auditMu.Unlock()
	}
	if err := conn.Write(protocol.TypeCommandReq, req); err != nil {
		r.Complete(id, req.AgentID, err.Error(), -1, 0)
		return false, "下发失败: " + err.Error()
	}
	return true, ""
}

// auditIDFor 返回某条命令对应的审计记录 ID（不存在返回 0）。
func (r *Router) auditIDFor(commandID string) int64 {
	if commandID == "" {
		return 0
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	return r.cmdAudit[commandID]
}

// CompleteByCommand 按 command_id 回填命令执行结果（供 Agent 回传 CommandResp 时调用）。
func (r *Router) CompleteByCommand(commandID, agentID, result string, exitCode int, duration int64) {
	if commandID == "" {
		return
	}
	r.completeByCommandID(commandID, result, exitCode, duration)
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

// Audit 记录审计日志，返回落库后的记录 ID（供回填执行结果；无 DB 时返回 0）。
//
// 安全说明：脱敏必须在这里（写入时）完成，而不是只在读取时做 ——
// 否则 audits.jsonl 与 SQLite 里保存的是明文命令，任何能读到数据目录
// 或备份的人都能直接拿到 `mysql -pP@ss`、`net user bob P@ss /add` 这类凭据。
func (r *Router) Audit(l *AuditLog) int64 {
	if l == nil {
		return 0
	}
	l.Command = MaskCommand(l.Command)
	l.Result = MaskCommand(l.Result)
	if l.Operator != "" {
		l.Operator = sanitizeAuditField(l.Operator)
	}
	r.auditMu.Lock()
	r.audits = append(r.audits, l)
	// 内存仅保留最近 auditMemoryKeep 条：历史全量数据由 SQLite 承载，
	// 避免长时间运行后内存无界增长。
	if len(r.audits) > auditMemoryKeep {
		r.audits = r.audits[len(r.audits)-auditMemoryKeep:]
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
	// 异步写入 SQLite（避免阻塞命令转发）
	if r.db != nil {
		go func() {
			id, err := r.db.InsertAuditLog(&storage.AuditLogRow{
				Time:        l.Time,
				Operator:    l.Operator,
				OperatorIP:  l.OperatorIP,
				AgentID:     l.AgentID,
				CommandID:   l.CommandID,
				Command:     l.Command,
				Result:      l.Result,
				ExitCode:    l.ExitCode,
				Blocked:     l.Blocked,
				BlockReason: l.BlockReason,
				Duration:    l.Duration,
			})
			if err != nil {
				log.Printf("[audit] 写入 SQLite 失败: %v", err)
				return
			}
			// 回填 ID，使后续结果回写能定位到同一条记录
			r.auditMu.Lock()
			l.ID = id
			r.auditMu.Unlock()
		}()
	}
	log.Printf("[audit] operator=%s agent=%s cmd=%q blocked=%v", l.Operator, l.AgentID, l.Command, l.Blocked)
	if writeErr != nil {
		log.Printf("[audit] 持久化写入失败: %v", writeErr)
	}
	return l.ID
}

// Complete 按记录 ID 回填命令执行结果（exit_code/result/duration），
// 并同步更新内存中的同一条记录，避免产生重复审计条目。
// 若 ID 为 0（异步落库尚未返回 ID）则按 agent_id 就近匹配尚未有结果的记录。
func (r *Router) Complete(id int64, agentID, result string, exitCode int, duration int64) {
	r.auditMu.Lock()
	for i := len(r.audits) - 1; i >= 0; i-- {
		l := r.audits[i]
		if (id > 0 && l.ID == id) || (id <= 0 && l.AgentID == agentID && l.Result == "") {
			l.Result = result
			l.ExitCode = exitCode
			l.Duration = duration
			if id <= 0 {
				id = l.ID
			}
			break
		}
	}
	r.auditMu.Unlock()
	if r.db != nil && id > 0 {
		go func() {
			if err := r.db.CompleteAuditLog(id, result, exitCode, duration); err != nil {
				log.Printf("[audit] 回填结果失败: %v", err)
			}
		}()
	}
}

// completeByCommandID 按 command_id 回填内存与数据库中的审计记录。
// 以 command_id 为准定位（比 ID 更可靠，因为 ID 是异步落库后才回填的）。
func (r *Router) completeByCommandID(commandID, result string, exitCode int, duration int64) {
	r.auditMu.Lock()
	var id int64
	for i := len(r.audits) - 1; i >= 0; i-- {
		l := r.audits[i]
		if l.CommandID == commandID {
			l.Result = result
			l.ExitCode = exitCode
			l.Duration = duration
			id = l.ID
			break
		}
	}
	if id == 0 {
		// 说明落库尚未回填 ID，稍后由 auditIDFor 映射兜底
		id = r.cmdAudit[commandID]
	}
	delete(r.cmdAudit, commandID)
	r.auditMu.Unlock()
	if r.db != nil && id > 0 {
		go func() {
			if err := r.db.CompleteAuditLog(id, result, exitCode, duration); err != nil {
				log.Printf("[audit] 回填结果失败: %v", err)
			}
		}()
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
	Category string // command / op / auth，空表示全部
	Since    int64  // 时间戳，0 表示不限制
	Until    int64
	Offset   int
	Limit    int
}

// AuditPage 分页查询结果：当前页数据 + 命中总数。
// Total 用于前端渲染页码，避免把全量数据一次性返回给浏览器。
type AuditPage struct {
	Items []*AuditLog `json:"items"`
	Total int64       `json:"total"`
}

// 单页最大返回条数，防止 limit 参数过大导致内存与网络压力。
const auditMaxPageSize = 500

// auditMemoryKeep 内存中保留的审计日志条数上限。
const auditMemoryKeep = 10000

// normalizePage 归一化分页参数：limit 取 (0,500]，offset 非负。
func normalizePage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > auditMaxPageSize {
		limit = auditMaxPageSize
	}
	return offset, limit
}

// SearchAudits 按条件检索审计日志（兼容旧调用：不分页返回列表）。
func (r *Router) SearchAudits(q AuditQuery) []*AuditLog {
	page, err := r.SearchAuditsPage(q)
	if err != nil {
		log.Printf("[audit] 检索审计日志失败: %v", err)
	}
	return page.Items
}

// SearchAuditsPage 分页检索审计日志，返回当前页数据与命中总数。
// 优先从 SQLite 查询（支持全量历史 + 分页计数），DB 不可用时回退内存分页。
func (r *Router) SearchAuditsPage(q AuditQuery) (*AuditPage, error) {
	if r.db != nil {
		offset, limit := normalizePage(q.Offset, q.Limit)
		cond := storage.AuditQueryCond{
			Agent:    q.Agent,
			Operator: q.Operator,
			Command:  q.Command,
			Category: q.Category,
			Since:    q.Since,
			Until:    q.Until,
		}
		total, err := r.db.CountAuditLogsFiltered(cond)
		if err == nil {
			rows, qerr := r.db.SearchAuditLogs(cond, offset, limit)
			if qerr == nil {
				items := make([]*AuditLog, 0, len(rows))
				for _, row := range rows {
					items = append(items, &AuditLog{
						ID:          row.ID,
						Time:        row.Time,
						Operator:    row.Operator,
						OperatorIP:  row.OperatorIP,
						AgentID:     row.AgentID,
						CommandID:   row.CommandID,
						Command:     row.Command,
						Result:      row.Result,
						ExitCode:    row.ExitCode,
						Blocked:     row.Blocked,
						BlockReason: row.BlockReason,
						Duration:    row.Duration,
						Category:    row.Category,
					})
				}
				return &AuditPage{Items: items, Total: total}, nil
			}
			log.Printf("[audit] SQLite 查询失败，回退内存: %v", qerr)
		} else {
			log.Printf("[audit] SQLite 计数失败，回退内存: %v", err)
		}
	}

	// 回退：内存查询（内存仅保留最近 auditMemoryKeep 条，便于重启后立即可见）
	r.auditMu.Lock()
	matched := make([]*AuditLog, 0, len(r.audits))
	for _, l := range r.audits {
		if !matchAudit(l, q) {
			continue
		}
		clone := *l
		clone.Category = categoryOrDefault(clone.Category)
		matched = append(matched, &clone)
	}
	r.auditMu.Unlock()

	// 内存切片按写入顺序递增，倒序后即为时间倒序（与 SQLite 保持一致）
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	total := int64(len(matched))
	offset, limit := normalizePage(q.Offset, q.Limit)
	if offset >= len(matched) {
		return &AuditPage{Items: []*AuditLog{}, Total: total}, nil
	}
	matched = matched[offset:]
	if limit < len(matched) {
		matched = matched[:limit]
	}
	return &AuditPage{Items: matched, Total: total}, nil
}

// categoryOrDefault 空类别按命令执行归类，兼容历史记录。
func categoryOrDefault(c string) string {
	if c == "" {
		return "command"
	}
	return c
}

// matchAudit 判断单条内存审计日志是否命中检索条件（与 SQL 语义保持一致）。
func matchAudit(l *AuditLog, q AuditQuery) bool {
	if q.Agent != "" && !strings.Contains(l.AgentID, q.Agent) {
		return false
	}
	if q.Operator != "" && !strings.Contains(l.Operator, q.Operator) {
		return false
	}
	if q.Command != "" && !strings.Contains(l.Command, q.Command) {
		return false
	}
	if q.Category != "" && q.Category != categoryOrDefault(l.Category) {
		return false
	}
	if q.Since > 0 && l.Time < q.Since {
		return false
	}
	if q.Until > 0 && l.Time > q.Until {
		return false
	}
	return true
}

// MaskCommand 对命令中的敏感字段做脱敏（如密码、密钥、Token）。
//
// 注意覆盖连写形态：`mysql -pP@ss0rd`、`sshpass -pXXX` 这类**没有空格**的写法
// 曾经漏掉，导致口令明文进入审计库。
func MaskCommand(cmd string) string {
	if cmd == "" {
		return cmd
	}
	// 顺序很重要：先处理**最具体**的形态，再处理宽泛的关键字规则。
	// 否则像 `-u admin:secret http://x` 会被关键字规则抢先命中，
	// 把 `secret` 后面的 `http://x` 当成值脱敏掉，而真正的口令留在原文里。

	// 1) -u user:pass / --user user:pass（curl、ftp 等）—— 最具体
	reUser := regexp.MustCompile(`(?i)((?:^|\s)-{1,2}user\s+[^\s:@=]+[:=])(\S+)`)
	cmd = reUser.ReplaceAllString(cmd, "$1***")
	// 2) --password x / --password=x
	reLong := regexp.MustCompile(`(?i)((?:^|\s)--password(?:\s+|=))(\S+)`)
	cmd = reLong.ReplaceAllString(cmd, "$1***")
	// 3) -p x / -p=x（要求空白或等号跟随，避免误伤 -port / -pid / -path）
	reShort := regexp.MustCompile(`((?:^|\s)-p(?:\s+|=))(\S+)`)
	cmd = reShort.ReplaceAllString(cmd, "$1***")
	// 4) -pX@ss 连写：仅当值以**大写字母/数字/符号**开头（区分大小写），mysql/sshpass 风格
	reConcat := regexp.MustCompile(`((?:^|\s)-p)([A-Z0-9@#%^&*!_.~/\\][^\s]*)`)
	cmd = reConcat.ReplaceAllString(cmd, "$1***")
	// 5) 形如 key=value / key: value / key value，且 key 为敏感关键字（最宽泛，放最后）
	reGeneric := regexp.MustCompile(`(?i)(password|passwd|passphrase|secret|token|apikey|api_key|authorization)(\s*[=:]\s*|\s+)(\S+)`)
	cmd = reGeneric.ReplaceAllString(cmd, "$1$2***")
	return cmd
}

// sanitizeAuditField 收敛审计字段：限长并去掉控制字符。
// 用于可能来自**未认证方**的字段（如登录接口的 username），
// 防止向审计库注入伪造条目或超长垃圾。
func sanitizeAuditField(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 128 {
			break
		}
	}
	if b.Len() == 0 {
		return "(空)"
	}
	return b.String()
}
