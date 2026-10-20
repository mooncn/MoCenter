// Package route 实现命令路由、命令黑名单拦截、在途命令归属与操作审计日志。
package route

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	// command_id -> 在途命令归属（审计记录、接令 Agent、发起方会话）
	cmdAudit map[string]*cmdBinding
	// 持久化文件（可选）：追加写入 JSONL
	auditFile *os.File
	// SQLite 持久化（可选）
	db *storage.DB
	// 交互终端输入的黑名单过滤状态（惰性初始化）
	imu    sync.Mutex
	iguard *InteractiveGuard
}

// cmdBinding 一条在途命令的归属。
//
// 存在的理由：Agent 回传的 command_id 是它自己填的，早先只按这个 ID 找审计记录、
// 找发起方，于是任一已接入的 Agent 都能替别人的命令写"执行成功"，或往别人正开着
// 的终端里塞文本。这里把"当初下发给了谁、是谁下发的"钉住，回传时逐条比对。
type cmdBinding struct {
	auditID int64  // 待回填结果的审计记录 ID（落库异步，可能暂时为 0）
	agentID string // 接令 Agent：回传连接的身份（证书或注册密钥证明）必须与之一致
	session string // 发起方会话键：命令输出只回投给它
	// last 为最近一次被回执触及的时间。一条交互命令可以跑上几小时并持续回显，
	// 容量驱逐因此按"最久未触及"下手：随机删会误伤活跃终端的绑定，
	// 让那个终端从此静默失联。
	last time.Time
}

// CommandOrigin 一条在途命令的归属信息（回传给发起方会话时需要两者都匹配）。
type CommandOrigin struct {
	AgentID string // 当初接令的 Agent
	Session string // 发起方操作员会话键（空表示无法定向）
}

// CommandRef 一条在途命令的定位信息（用于会话结束时主动收口）。
type CommandRef struct {
	AgentID   string
	CommandID string
}

// 在途命令映射的容量上下限：超过 cmdAuditCapMax 时驱逐到 cmdAuditCapMin。
const (
	cmdAuditCapMax = 20000
	cmdAuditCapMin = 10000

	// maxCommandIDLen command_id 的最大长度。它会成为 map 键与审计字段，
	// 不设上限时一个请求就能让一条大字符串驻留到命令结束。
	maxCommandIDLen = 128
)

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
		cmdAudit:  make(map[string]*cmdBinding),
		db:        db,
	}
	if len(auditPaths) > 0 && auditPaths[0] != "" {
		if err := os.MkdirAll(filepath.Dir(auditPaths[0]), 0700); err == nil {
			// 导入历史 JSONL 审计（在打开文件追加句柄之前完成，避免读到未落盘内容）
			r.importLegacyAudits(auditPaths[0])
			if f, err := os.OpenFile(auditPaths[0], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				// 再显式 Chmod 一次：os.OpenFile 的权限位**只在创建时**生效，
				// 而旧版本留下的 0644 文件会一路保持下去 —— 里面是审计明文。
				if cerr := f.Chmod(0600); cerr != nil {
					log.Printf("[audit] 收紧 %s 权限失败: %v（审计含命令与结果，请手工改为 0600）", auditPaths[0], cerr)
				}
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
//
// ## 它是什么，不是什么
//
// 这是**纵深防御**中的一层界面护栏，**不是安全边界**。任意 shell 语法都能把
// 一个被拦的字面串改写成等价但不同的字符串（`rm  -rf  /` 只是多了个空格、引号拼接到 `rm`、
// `$(echo cm0gLXJmIC8=|base64 -d|sh)`），所以"绕不过"从来不是它的目标。
// 目标是把"手滑"和"照着教程贴一条破坏性命令"挡住，并且不给界面/文档
// 留下"已经拦住了"的错觉（那条错觉比没有护栏更危险）。
//
// ## 相比旧版多做了什么
//
// 旧版只做 `strings.Contains(原文)` 这一条匹配，于是**连不涉及任何混淆技巧的
// 常见写法都漏**：`rm  -rf  /`（多打一个空格）、`rm -rf / #`（后面跟注释）、
// 制表符分隔。这里在原文之外再对"归一化后的形式"匹配一次：
//   - 去掉引号字符（' " ` \）—— shell 会吃掉它们；
//   - 把连续空白（空格 / 制表 / CR / LF）压成一个空格；
//   - 去掉行内注释起始的 ` #` 之后的内容。
//
// 真正的边界是"命令执行本身是产品功能"：被管主机上的 Agent 以高权限执行任意
// 命令是设计意图，所以护栏失守不等于越权。收口位置在审计与授权，不在字符串匹配。
func (r *Router) IsBlocked(cmd string) (bool, string) {
	variants := []string{strings.ToLower(cmd)}
	if norm := normalizeForBlacklist(cmd); norm != "" {
		variants = append(variants, norm)
	}
	r.blackMu.RLock()
	defer r.blackMu.RUnlock()
	for _, b := range r.blacklist {
		pat := strings.ToLower(strings.TrimSpace(b))
		if pat == "" {
			continue
		}
		for _, v := range variants {
			if strings.Contains(v, pat) {
				return true, b
			}
		}
	}
	return false, ""
}

// normalizeForBlacklist 把一条命令压成"shell 展开前"的近似形态，用于黑名单匹配。
//
// 刻意保守：只做**去掉引号 + 压缩空白 + 截掉行内注释**这三件不会改变语义方向的
// 归一化。不做变量展开、不做命令替换求值、不猜 base64 —— 那些要么需要真 shell、
// 要么把护栏变成"看起来能拦"的假象（见 IsBlocked 的说明）。
func normalizeForBlacklist(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	prevSpace := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case '\'', '"', '`', '\\':
			// shell 会吃掉这些引号/转义符，匹配前先去掉
			continue
		case ' ', '\t', '\r', '\n', '\v', '\f':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		case '#':
			// 行内注释：# 只在"词首"（前一字符是空白或行首）时开始注释
			if i == 0 || cmd[i-1] == ' ' || cmd[i-1] == '\t' || b.Len() == 0 ||
				(b.Len() > 0 && b.String()[b.Len()-1] == ' ') {
				out := strings.TrimSpace(b.String())
				return strings.ToLower(out)
			}
		}
		b.WriteByte(c)
		prevSpace = false
	}
	return strings.ToLower(strings.TrimSpace(b.String()))
}

// Dispatch 将命令下发到指定 Agent。
// interactive 为 true 时进入交互模式（通过 WebSocket 实时转发）。
// session 为发起方操作员会话键（见 ws.SessionKey）：命令输出只回投给这一会话。
func (r *Router) Dispatch(req *protocol.CommandReq, operator, operatorIP, session string) (bool, string) {
	if len(req.CommandID) > maxCommandIDLen {
		return false, fmt.Sprintf("命令 ID 过长（最多 %d 字节），请重新发起", maxCommandIDLen)
	}
	// command_id 由前端生成、可猜；同一键只能属于一个会话。分享访客若能覆盖管理端
	// 已有的绑定，就能收到对方的终端输出并把输入打进对方的会话 —— 这里先做一次快速拒绝。
	if r.commandIDTakenByOther(req.CommandID, req.AgentID, session) {
		return false, "命令 ID 已被其他会话占用，请重新发起"
	}
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
	// 记录 command_id -> 归属（审计 ID + 接令 Agent + 发起方会话），供结果回填与回投
	if req.CommandID != "" {
		r.auditMu.Lock()
		if r.cmdAudit == nil {
			r.cmdAudit = make(map[string]*cmdBinding)
		}
		// 快速检查与这里之间可能插入另一会话的同 ID 下发；持锁再判一次，
		// 保证"后写入者"不会把先到者的归属顶掉。
		if cur := r.cmdAudit[cmdKey(req.CommandID, req.AgentID)]; cur != nil && cur.session != session {
			r.auditMu.Unlock()
			r.Complete(id, req.AgentID, "命令 ID 已被其他会话占用", -1, 0)
			return false, "命令 ID 已被其他会话占用，请重新发起"
		}
		r.cmdAudit[cmdKey(req.CommandID, req.AgentID)] = &cmdBinding{
			auditID: id,
			agentID: req.AgentID,
			session: session,
			last:    time.Now(),
		}
		r.evictCmdAuditLocked()
		r.auditMu.Unlock()
	}
	if err := conn.Write(protocol.TypeCommandReq, req); err != nil {
		r.Complete(id, req.AgentID, err.Error(), -1, 0)
		return false, "下发失败: " + err.Error()
	}
	return true, ""
}

// commandIDTakenByOther 判断 (agentID, commandID) 是否已被**其他会话**占用。
func (r *Router) commandIDTakenByOther(commandID, agentID, session string) bool {
	if commandID == "" {
		return false
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	b := r.cmdAudit[cmdKey(commandID, agentID)]
	return b != nil && b.session != session
}

// cmdKey 在途命令的索引键。
//
// 必须带上 agentID：批量下发时 N 台机器共用同一个由浏览器生成的 command_id，
// 只按 ID 索引会让后写入的归属覆盖前 N-1 台，届时既回填错审计记录，
// 又把其余 Agent 的正常回执误判成"身份不符"。
func cmdKey(commandID, agentID string) string {
	return agentID + "|" + commandID
}

// evictCmdAuditLocked 限制在途命令映射的规模，按"最久未触及"整批驱逐。
// 调用方需持有 auditMu。
//
// 一次性排好序删到下限，而不是每次删一条最旧的：后者在触发阈值后是 O(n²)
// （n=2 万时约 2 亿次比较），会卡在命令下发路径上。
func (r *Router) evictCmdAuditLocked() {
	if len(r.cmdAudit) <= cmdAuditCapMax {
		return
	}
	keys := make([]string, 0, len(r.cmdAudit))
	for k := range r.cmdAudit {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return r.cmdAudit[keys[i]].last.Before(r.cmdAudit[keys[j]].last)
	})
	for _, k := range keys {
		if len(r.cmdAudit) <= cmdAuditCapMin {
			return
		}
		delete(r.cmdAudit, k)
	}
}

// CommandOrigin 查询"这条命令当初是不是下发给了这台 Agent"，并给出发起方会话。
//
// 回传侧（Agent → 服务端）的身份取自连接证书，不取报文里的自报字段；
// 查不到绑定 = 这条 command_id 从未下发给该 Agent，调用方应丢弃并留痕。
func (r *Router) CommandOrigin(commandID, agentID string) (CommandOrigin, bool) {
	if commandID == "" || agentID == "" {
		return CommandOrigin{}, false
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	b := r.cmdAudit[cmdKey(commandID, agentID)]
	if b == nil {
		return CommandOrigin{}, false
	}
	// 交互终端的一条命令可以跑很久：每次被触及就续期，避免被容量驱逐
	b.last = time.Now()
	return CommandOrigin{AgentID: b.agentID, Session: b.session}, true
}

// ReleaseSession 取出并移除某个会话名下的全部在途命令绑定。
//
// 分享撤销/过期时调用：只断开 WebSocket 不会结束远端 shell，命令进程会继续在
// 被管主机上运行（且再也无人能输入）。把绑定一并收口，调用方才能向对应 Agent
// 下发 InteractiveClose。
func (r *Router) ReleaseSession(session string) []CommandRef {
	if session == "" {
		return nil
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	var out []CommandRef
	prefix := ""
	for k, b := range r.cmdAudit {
		if b == nil || b.session != session {
			continue
		}
		prefix = b.agentID + "|"
		out = append(out, CommandRef{
			AgentID:   b.agentID,
			CommandID: strings.TrimPrefix(k, prefix),
		})
		delete(r.cmdAudit, k)
	}
	return out
}

// auditIDFor 返回某条命令对应的审计记录 ID（不存在返回 0）。
func (r *Router) auditIDFor(commandID, agentID string) int64 {
	if commandID == "" {
		return 0
	}
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	if b := r.cmdAudit[cmdKey(commandID, agentID)]; b != nil {
		return b.auditID
	}
	return 0
}

// CompleteByCommand 按 (command_id, agent_id) 回填命令执行结果。
// agentID 必须与当初接令的一致，否则既不回填也不清除绑定（那是别人的命令）。
func (r *Router) CompleteByCommand(commandID, agentID, result string, exitCode int, duration int64) {
	if commandID == "" || agentID == "" {
		return
	}
	r.completeByCommandID(commandID, agentID, result, exitCode, duration)
}

// ReleaseCommand 结束一条交互会话的归属登记。
//
// 交互终端不会回 CommandResp（它只发 InteractiveOutput{type:exit}），
// 若不在此处收口，这条映射要等到容量驱逐才消失。
func (r *Router) ReleaseCommand(commandID, agentID string) {
	if commandID == "" || agentID == "" {
		return
	}
	r.auditMu.Lock()
	delete(r.cmdAudit, cmdKey(commandID, agentID))
	r.auditMu.Unlock()
}

// BatchResult 批量命令单台结果。
type BatchResult struct {
	AgentID string `json:"agent_id"`
	OK      bool   `json:"ok"`
	Msg     string `json:"msg"`
}

// DispatchBatch 将同一命令批量下发到多台在线 Agent。
// 各台共用一个 command_id（由浏览器生成），因此归属只能按 (command_id, agent_id) 记录。
func (r *Router) DispatchBatch(req *protocol.CommandReq, agentIDs []string, operator, operatorIP, session string) []*BatchResult {
	results := make([]*BatchResult, 0, len(agentIDs))
	for _, id := range agentIDs {
		clone := *req
		clone.AgentID = id
		ok, msg := r.Dispatch(&clone, operator, operatorIP, session)
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
	l.Command = truncateAuditText(MaskCommand(l.Command), auditCommandMax)
	fullResult := truncateAuditText(MaskCommand(l.Result), auditResultStoreMax)
	l.Result = truncateAuditText(fullResult, auditResultMemoryMax)
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
	var row *storage.AuditLogRow
	if r.db != nil {
		// 行必须在持锁期间抄好：交给协程去读 l 的字段，就等于让它在
		// Complete 改写 Result/ExitCode/Duration 的同时裸读同一批字段。
		row = &storage.AuditLogRow{
			Time:        l.Time,
			Operator:    l.Operator,
			OperatorIP:  l.OperatorIP,
			AgentID:     l.AgentID,
			CommandID:   l.CommandID,
			Command:     l.Command,
			Result:      fullResult,
			ExitCode:    l.ExitCode,
			Blocked:     l.Blocked,
			BlockReason: l.BlockReason,
			Duration:    l.Duration,
			// Category 必须一并抄过去：漏了它，InsertAuditLog 会把空值补成 'command'，
			// 于是"注册面拒绝(Category=auth)"与"运维操作(op)"两类审计在库里永远显示成
			// 命令执行，`/api/audits?category=auth` 一条也查不到（真机 H-N1 端到端复测撞出）。
			Category: l.Category,
		}
	}
	r.auditMu.Unlock()
	// 异步写入 SQLite（避免阻塞命令转发）
	if row != nil {
		go func() {
			id, err := r.db.InsertAuditLog(row)
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
	// ID 由上面那个协程回填，读它必须同样持锁 —— 否则会与其写入竞争同一字段
	// （这里通常仍是 0：落库没回来，回填结果改走 command_id 定位）。
	r.auditMu.Lock()
	defer r.auditMu.Unlock()
	return l.ID
}

// Complete 按记录 ID 回填命令执行结果（exit_code/result/duration），
// 并同步更新内存中的同一条记录，避免产生重复审计条目。
// 若 ID 为 0（异步落库尚未返回 ID）则按 agent_id 就近匹配尚未有结果的记录。
func (r *Router) Complete(id int64, agentID, result string, exitCode int, duration int64) {
	result = truncateAuditText(MaskCommand(result), auditResultStoreMax)
	memResult := truncateAuditText(result, auditResultMemoryMax)
	r.auditMu.Lock()
	for i := len(r.audits) - 1; i >= 0; i-- {
		l := r.audits[i]
		if (id > 0 && l.ID == id) || (id <= 0 && l.AgentID == agentID && l.Result == "") {
			l.Result = memResult
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

// completeByCommandID 按 (command_id, agent_id) 回填内存与数据库中的审计记录。
//
// 两个条件都要匹配：只有 command_id 时，一条猜测得到的 ID（浏览器生成的
// 'push-'+Date.now() 完全可猜）就能改写到别人的审计行；批量下发时同一 ID
// 又对应多条记录，不看 agent 会回填到任意一条上。
func (r *Router) completeByCommandID(commandID, agentID, result string, exitCode int, duration int64) {
	result = truncateAuditText(MaskCommand(result), auditResultStoreMax)
	memResult := truncateAuditText(result, auditResultMemoryMax)
	r.auditMu.Lock()
	var id int64
	for i := len(r.audits) - 1; i >= 0; i-- {
		l := r.audits[i]
		if l.CommandID == commandID && l.AgentID == agentID {
			l.Result = memResult
			l.ExitCode = exitCode
			l.Duration = duration
			id = l.ID
			break
		}
	}
	key := cmdKey(commandID, agentID)
	if id == 0 {
		// 说明落库尚未回填 ID，改用下发时记下的审计 ID 兜底
		if b := r.cmdAudit[key]; b != nil {
			id = b.auditID
		}
	}
	// 只删自己这一条：命令已结束，归属映射随之作废
	delete(r.cmdAudit, key)
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
	// 逐条**拷值**而不是拷指针：落库回填 ID 与结果回填都在别的协程里改同一个结构体，
	// 交出指针就等于把没有锁保护的字段（ID/Result）发给调用方 —— /api/audits 序列化
	// 与这些写入并发时是真实的数据竞争（-race 可复现）。
	for i, l := range r.audits {
		c := *l
		out[i] = &c
	}
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
//
// 历史全量数据由 SQLite 承载，内存这份只用于「数据库暂时不可用」时的兜底查询，
// 因此不需要与库同量级。原值 10000 在每条结果都完整保存时，单是
// 「几条几十 KB 的命令输出 × 上万条」就能把常驻内存推到数百 MB。
const auditMemoryKeep = 2000

// 审计字段的长度闸门。
//
// 命令输出由被管主机回传，长度完全不可控（`cat` 一个大日志就能到几十 MB）。
// 结果写库上限 64 KiB，足够事后定位问题；内存副本再收紧到 8 KiB，
// 因为内存这份只服务于兜底查询，完整内容本来就在库里。
const (
	auditCommandMax      = 8 << 10
	auditResultStoreMax  = 64 << 10
	auditResultMemoryMax = 8 << 10
)

// truncateAuditText 将文本截断到 limit 字节（回退到 UTF-8 字符边界）并附提示。
func truncateAuditText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n\n[内容过长，已截断]"
}

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
