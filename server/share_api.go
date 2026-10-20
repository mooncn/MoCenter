// 本文件实现「邀请远程管理」的两侧接口：
//
//   - 控制台侧（需登录）：创建 / 列举 / 撤销一条分享链接；
//   - 访客侧（凭 X-Share-Token）：读取被分享主机的信息、开终端、连桌面、管理该主机的端口映射。
//
// 访客不是账号：系统本身单用户、没有 RBAC，分享被建模成一枚**有作用域、有寿命、
// 可撤销**的 bearer 链接。因此每个访客端点都必须同时校验「链接有效」与
// 「该功能在链接作用域内」，并且所有写入动作都要以 share:<id> 的身份进审计。
package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/portmap"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/route"
	"mocenter/server/internal/session"
	"mocenter/server/internal/share"
	"mocenter/server/internal/vnc"
	"mocenter/server/internal/ws"
)

// shareTokenHeader 访客的凭据头。用自定义头而不是 Cookie/查询参数：
// Cookie 会被同站其它请求自动带上，查询参数会进访问日志；头只由分享页脚本显式发送。
const shareTokenHeader = "X-Share-Token"

func shareOperator(sh *share.Share) string { return "share:" + sh.ID }

// validShareCommandID 约束分享命令 ID 的命名空间。
//
// 管理端的终端 ID 是 it-<Date.now()>、批量是 batch-*；分享访客若能用这些名字，
// 就可能在 Dispatch 中触到别人的归属（现由 Dispatch 的跨会话检查兜住）。
// 这里再从**入口**收一次：分享命令必须带 share- 前缀，字符与长度也受限。
func validShareCommandID(id string) bool {
	if !strings.HasPrefix(id, "share-") || len(id) < len("share-")+1 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// scheduleShareExpiry 在分享到期时主动关闭其在线会话。
//
// 撤销有独立入口且会立即断开；到期同样必须生效 —— 握手时校验过一次，不代表
// 这条连接在链接失效后还能继续用。定时器随进程存在，服务端重启后所有会话本就断开。
func scheduleShareExpiry(sh *share.Share, closeFn func()) {
	if sh == nil || closeFn == nil {
		return
	}
	d := time.Until(time.Unix(sh.ExpiresAt, 0))
	if d <= 0 {
		closeFn()
		return
	}
	time.AfterFunc(d, closeFn)
}

// shareIDFromOperator 解析 "share:<id>" 形式的操作者标识。
func shareIDFromOperator(operator string) (string, bool) {
	const prefix = "share:"
	if !strings.HasPrefix(operator, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(operator, prefix)
	return id, id != ""
}

// shareAPI 汇总分享功能依赖的服务端组件。
type shareAPI struct {
	store  *share.Store
	probes *probe.Store
	authz  *auth.Auth
	router *route.Router
	hub    *ws.Hub
	vnc    *vnc.Mgr
	sess   *session.Manager
	pm     *portmap.Manager
}

// adminHandler 控制台侧的创建/列举/撤销。
func (a *shareAPI) adminHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, a.authz) {
			return
		}
		if a.store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "分享存储不可用（详见服务端启动日志）"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			agentID := strings.TrimSpace(r.URL.Query().Get("agent"))
			writeJSON(w, map[string]interface{}{"ok": true, "shares": a.store.ListActive(agentID)})
		case http.MethodPost:
			a.create(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (a *shareAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID    string   `json:"agent_id"`
		Label      string   `json:"label"`
		Features   []string `json:"features"`
		VNCPort    int      `json:"vnc_port"`
		TTLSeconds int64    `json:"ttl_seconds"`
	}
	if !decodeJSON(w, r, maxJSONBody, &body) {
		return
	}
	agentID := strings.TrimSpace(body.AgentID)
	if agentID == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "msg": "未指定目标主机"})
		return
	}
	info := a.probes.Get(agentID)
	if info == nil {
		writeJSON(w, map[string]interface{}{"ok": false, "msg": "目标主机不存在（可能已被删除或改过 agent_id）"})
		return
	}
	token, sh, err := a.store.Create(agentID, body.Label, body.Features,
		body.VNCPort, time.Duration(body.TTLSeconds)*time.Second)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
		return
	}
	// 链接 token 放在 URL 片段里：片段不会随请求发给服务端，也不会进反代访问日志。
	link := "/share#" + token
	host := info.Hostname
	if info.Remark != "" {
		host = info.Remark
	}
	a.audit(r, sh, "创建分享链接",
		fmt.Sprintf("主机 %s（%s），功能 %s，有效期至 %s",
			host, agentID, strings.Join(sh.Features, "/"),
			time.Unix(sh.ExpiresAt, 0).Format("2006-01-02 15:04:05")))
	// 明文 token 只在这里出现一次：列表接口永远不回显。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]interface{}{
		"ok": true, "id": sh.ID, "token": token, "link": link,
		"agent_id": sh.AgentID, "features": sh.Features, "vnc_port": sh.VNCPort,
		"expires_at": sh.ExpiresAt, "created_at": sh.CreatedAt,
		"msg": "分享链接已创建（只显示这一次），发给对方后在有效期内可直接打开",
	})
}

// revokeHandler 撤销一条分享，并断开它已经建立的终端 / 桌面连接。
func (a *shareAPI) revokeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, a.authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if a.store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "分享存储不可用（详见服务端启动日志）"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		sh := a.store.Revoke(strings.TrimSpace(body.ID))
		if sh == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未找到该分享（可能已过期清理）"})
			return
		}
		// 撤销不只是"下次校验失败"：已经握着的终端与桌面必须当场断开，
		// 否则"撤销"对正在使用的访客没有实际效果。
		closedWS := 0
		if a.hub != nil {
			closedWS = a.hub.CloseSession(sh.SessionKey)
		}
		closedVNC := 0
		if a.vnc != nil {
			closedVNC = a.vnc.CloseSession(sh.SessionKey)
		}
		closedCmds := closeShareCommands(a.router, a.sess, sh.SessionKey)
		a.audit(r, sh, "撤销分享链接",
			fmt.Sprintf("断开终端连接 %d 条、桌面隧道 %d 条、结束远端命令 %d 条",
				closedWS, closedVNC, closedCmds))
		writeJSON(w, map[string]interface{}{
			"ok": true, "msg": "分享已撤销",
			"closed_ws": closedWS, "closed_vnc": closedVNC, "closed_commands": closedCmds,
		})
	}
}

// guest 校验访客凭据与功能作用域。第二个返回值是被分享主机的当前记录。
func (a *shareAPI) guest(w http.ResponseWriter, r *http.Request, feature string) (*share.Share, *protocol.AgentInfo, bool) {
	if !guardGate(w, r, a.authz) {
		return nil, nil, false
	}
	if a.store == nil {
		http.Error(w, "share unavailable", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	token := strings.TrimSpace(r.Header.Get(shareTokenHeader))
	sh, err := a.store.Validate(token, "", feature)
	if err != nil {
		// 不区分"无效/过期/已撤销"：对访客只给一个口径，避免把分享 ID 空间当探测面。
		http.Error(w, "分享链接无效或已过期", http.StatusUnauthorized)
		return nil, nil, false
	}
	info := a.probes.Get(sh.AgentID)
	if info == nil {
		http.Error(w, "目标主机已注销", http.StatusNotFound)
		return nil, nil, false
	}
	return sh, info, true
}

// infoHandler 返回访客可见的主机与分享范围。
func (a *shareAPI) infoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, info, ok := a.guest(w, r, "")
		if !ok {
			return
		}
		writeJSON(w, map[string]interface{}{
			"ok": true, "agent_id": sh.AgentID,
			"hostname": info.Hostname, "remark": info.Remark,
			"os_type": info.OSType, "online": info.Online,
			"label": sh.Label, "features": sh.Features, "vnc_port": sh.VNCPort,
			"created_at": sh.CreatedAt, "expires_at": sh.ExpiresAt,
		})
	}
}

// commandHandler 访客开一个远程终端（只能开交互会话，且只能开给分享的主机）。
func (a *shareAPI) commandHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, _, ok := a.guest(w, r, share.FeatureTerminal)
		if !ok {
			return
		}
		var body struct {
			CommandID string `json:"command_id"`
			ShellType string `json:"shell_type"`
			Rows      int    `json:"rows"`
			Cols      int    `json:"cols"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		commandID := strings.TrimSpace(body.CommandID)
		if commandID == "" {
			commandID = fmt.Sprintf("share-%d", time.Now().UnixNano())
		} else if !validShareCommandID(commandID) {
			writeJSON(w, map[string]interface{}{"ok": false,
				"msg": "command_id 非法：分享命令需以 share- 开头，仅限字母、数字与 ._-"})
			return
		}
		rows, cols := body.Rows, body.Cols
		if rows <= 0 || rows > 500 {
			rows = 24
		}
		if cols <= 0 || cols > 1000 {
			cols = 80
		}
		req := &protocol.CommandReq{
			CommandID:   commandID,
			AgentID:     sh.AgentID,
			ShellType:   body.ShellType,
			Interactive: true,
			Rows:        rows,
			Cols:        cols,
			Timeout:     0,
		}
		okDispatch, msg := a.router.Dispatch(req, shareOperator(sh), auth.LoginIPOf(r.RemoteAddr), sh.SessionKey)
		if !okDispatch {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": msg})
			return
		}
		// 即使访客从不建立 /ws 连接，到期也必须结束这条远端命令。
		scheduleShareExpiry(sh, func() { closeShareCommands(a.router, a.sess, sh.SessionKey) })
		writeJSON(w, map[string]interface{}{"ok": true, "command_id": commandID, "agent_id": sh.AgentID})
	}
}

// commandCloseHandler 访客结束自己开的终端会话。
func (a *shareAPI) commandCloseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, _, ok := a.guest(w, r, share.FeatureTerminal)
		if !ok {
			return
		}
		var body struct {
			CommandID string `json:"command_id"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		commandID := strings.TrimSpace(body.CommandID)
		if commandID == "" {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "缺少 command_id"})
			return
		}
		// 只允许关闭**本次分享会话**发起的命令：command_id 由浏览器生成、可猜，
		// 不校验归属就等于给了访客一个"按 ID 掐断他人终端"的入口。
		if origin, found := a.router.CommandOrigin(commandID, sh.AgentID); !found || origin.Session != sh.SessionKey {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "该终端会话不属于当前分享"})
			return
		}
		if conn := a.sess.Get(sh.AgentID); conn != nil {
			_ = conn.Write(protocol.TypeInteractiveClose, protocol.InteractiveCloseReq{
				AgentID: sh.AgentID, CommandID: commandID,
			})
		}
		a.router.ReleaseCommand(commandID, sh.AgentID)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "已结束终端会话"})
	}
}

// rulesHandler 列出**该主机**的端口映射规则（不暴露其它主机的规则）。
func (a *shareAPI) rulesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, _, ok := a.guest(w, r, share.FeaturePortmap)
		if !ok {
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "rules": a.rulesOf(sh.AgentID)})
	}
}

// portmapOpHandler 访客在分享范围内维护该主机的端口映射。
func (a *shareAPI) portmapOpHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, _, ok := a.guest(w, r, share.FeaturePortmap)
		if !ok {
			return
		}
		var op protocol.PortMapOp
		if !decodeJSON(w, r, maxJSONBody, &op) {
			return
		}
		// 目标主机只认分享里绑定的那一台：报文里的 agent_id 一律忽略。
		op.AgentID = sh.AgentID
		msg, err := a.applyPortmapOp(sh, &op)
		if err != nil {
			a.audit(r, sh, "端口映射 "+op.Action, "失败："+err.Error())
			writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
			return
		}
		a.audit(r, sh, "端口映射 "+op.Action,
			fmt.Sprintf("public:%d local:%d", op.PublicPort, op.LocalPort))
		writeJSON(w, map[string]interface{}{"ok": true, "msg": msg, "rules": a.rulesOf(sh.AgentID)})
	}
}

// applyPortmapOp 执行端口映射操作并返回给访客的提示。
//
// 与管理员路径的关键差别是**归属校验**：访客不能覆盖或删除其它主机的规则。
// CreateRule 对已存在的公网端口是"覆盖"，缺了这道校验，访客只要填一个别人在用的
// 公网端口就能把那条映射改指向自己的主机。
func (a *shareAPI) applyPortmapOp(sh *share.Share, op *protocol.PortMapOp) (string, error) {
	if a.pm == nil {
		return "", fmt.Errorf("端口映射不可用")
	}
	switch op.Action {
	case "create":
		if op.PublicPort < 1 || op.PublicPort > 65535 || op.LocalPort < 1 || op.LocalPort > 65535 {
			return "", fmt.Errorf("端口范围应为 1-65535")
		}
		// 访客不能占用系统保留端口：1024 以下多为 SSH/HTTP 等基础服务，
		// 让分享链接直接在总控上监听这些端口，等于把"邀请"变成抢占基础服务的入口。
		if op.PublicPort < 1024 {
			return "", fmt.Errorf("分享创建的映射端口不能小于 1024（%d 属系统保留端口）", op.PublicPort)
		}
		if cur := a.pm.GetRule(op.PublicPort); cur != nil && cur.AgentID != sh.AgentID {
			return "", fmt.Errorf("公网端口 %d 已被其它主机占用", op.PublicPort)
		}
		rule := &portmap.Rule{
			ID:         fmt.Sprintf("rule-%d-%s", op.PublicPort, sh.AgentID),
			PublicPort: op.PublicPort,
			AgentID:    sh.AgentID,
			LocalHost:  "127.0.0.1",
			LocalPort:  op.LocalPort,
			SourceIP:   op.SourceIP,
			CreatedAt:  time.Now().Unix(),
		}
		if err := a.pm.CreateRule(rule); err != nil {
			return "", fmt.Errorf("创建失败: %v", err)
		}
		return fmt.Sprintf("映射已创建：公网 :%d → 127.0.0.1:%d", op.PublicPort, op.LocalPort), nil
	case "enable", "disable", "delete":
		if op.PublicPort < 1 || op.PublicPort > 65535 {
			return "", fmt.Errorf("公网端口无效")
		}
		cur := a.pm.GetRule(op.PublicPort)
		if cur == nil {
			return "", fmt.Errorf("规则不存在")
		}
		if cur.AgentID != sh.AgentID {
			return "", fmt.Errorf("无权操作其它主机的映射规则")
		}
		switch op.Action {
		case "enable":
			if err := a.pm.EnableRule(op.PublicPort); err != nil {
				return "", fmt.Errorf("启用失败: %v", err)
			}
			return fmt.Sprintf("映射 :%d 已启用", op.PublicPort), nil
		case "disable":
			a.pm.DisableRule(op.PublicPort)
			return fmt.Sprintf("映射 :%d 已停用", op.PublicPort), nil
		default:
			a.pm.DeleteRule(op.PublicPort)
			return fmt.Sprintf("映射 :%d 已删除", op.PublicPort), nil
		}
	default:
		return "", fmt.Errorf("未知操作")
	}
}

// rulesOf 只返回某台主机的映射规则。
func (a *shareAPI) rulesOf(agentID string) []*portmap.Rule {
	out := make([]*portmap.Rule, 0, 4)
	for _, r := range a.pm.Rules() {
		if r != nil && r.AgentID == agentID {
			out = append(out, r)
		}
	}
	return out
}

// vncPasswordHandler 为访客读取被分享主机的当前 VNC 口令。
//
// 这是"邀请"能真正用起来的前提：访客没有控制台，也没有别的途径知道口令。
// 口令只回给持有 VNC 作用域链接的访客，且每次读取都留审计。
func (a *shareAPI) vncPasswordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sh, _, ok := a.guest(w, r, share.FeatureVNC)
		if !ok {
			return
		}
		if a.vnc == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "远程桌面不可用"})
			return
		}
		resp := a.vnc.ReadPassword(sh.AgentID)
		if resp == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "Agent 未返回口令读取结果"})
			return
		}
		result := "失败：" + resp.Message
		if resp.OK {
			result = "成功"
		}
		a.audit(r, sh, "读取 VNC 口令", result)
		writeJSON(w, map[string]interface{}{
			"ok": resp.OK, "password": resp.Password, "mode": resp.Mode,
			"msg": resp.Message, "port": sh.VNCPort,
		})
	}
}

// audit 以分享身份留痕（不写代表凭据的 token，只写主机、功能与动作）。
func (a *shareAPI) audit(r *http.Request, sh *share.Share, command, result string) {
	if a.router == nil || sh == nil {
		return
	}
	a.router.Audit(&route.AuditLog{
		Time:       time.Now().Unix(),
		Operator:   shareOperator(sh),
		OperatorIP: auth.LoginIPOf(r.RemoteAddr),
		AgentID:    sh.AgentID,
		Command:    command,
		Result:     result,
		Category:   "op",
	})
}

// closeShareCommands 结束某分享会话发起、仍在途的远端命令，返回结束条数。
//
// 只断开浏览器连接不够：远端 shell 仍会在被管主机上运行。撤销/到期时把绑定
// 一并收口，并向对应 Agent 下发 InteractiveClose，才算真正收回这次授权。
func closeShareCommands(router *route.Router, sess *session.Manager, sessionKey string) int {
	if router == nil || sessionKey == "" {
		return 0
	}
	refs := router.ReleaseSession(sessionKey)
	for _, ref := range refs {
		if sess != nil {
			if c := sess.Get(ref.AgentID); c != nil {
				_ = c.Write(protocol.TypeInteractiveClose, protocol.InteractiveCloseReq{
					AgentID: ref.AgentID, CommandID: ref.CommandID,
				})
			}
		}
		router.DropInteractive(route.InteractiveKey(ref.AgentID, ref.CommandID))
	}
	return len(refs)
}

// revokeSharesForAgents 删除主机时撤销其全部分享（含历史别名），并断开在线会话。
func revokeSharesForAgents(store *share.Store, hub *ws.Hub, vncMgr *vnc.Mgr, agentIDs []string) int {
	if store == nil {
		return 0
	}
	total := 0
	for _, id := range agentIDs {
		keys, n := store.RevokeAgent(id)
		total += n
		for _, key := range keys {
			if hub != nil {
				hub.CloseSession(key)
			}
			if vncMgr != nil {
				vncMgr.CloseSession(key)
			}
		}
	}
	return total
}
