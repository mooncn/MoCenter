// 本文件是"一次性注册令牌"的控制台侧接口（评估项 C-1 的落地入口）。
//
// 令牌签发与证书 TOFU 绑定改变了"谁能拿到一张合法客户端证书"，因此每一次写入都必须
// 经过登录鉴权并落审计。审计里只放人类可读摘要 —— 令牌明文与私钥一律不入。
package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/auth"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/route"
)

// enrollTokensHandler GET 列出令牌与绑定 / POST 签发一枚一次性注册令牌。
//
// 令牌明文只在**创建响应**里出现一次：列表接口不再回显 —— 存储里为了校验 HMAC 必须
// 保留明文（见 tokens.go 的说明），少一处回显就少一处泄露面，而它本来也只在该机
// 首次接入时用一次。
func enrollTokensHandler(store *agentauth.EnrollStore, verifier *agentauth.Verifier, authz *auth.Auth, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "注册令牌存储不可用（详见服务端启动日志）"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{
				"ok":       true,
				"tokens":   store.ListTokens(),
				"bindings": store.Bindings(),
				// 迁移盘点用：还能不能靠"一把密钥认全网"上来、当前有几枚待用令牌。
				"shared_key_enabled": verifier.SharedKeyEnabled(),
				"pending_tokens":     store.PendingTokens(),
			})
		case http.MethodPost:
			var body struct {
				AgentID    string `json:"agent_id"`
				Label      string `json:"label"`
				TTLSeconds int64  `json:"ttl_seconds"`
			}
			if !decodeJSON(w, r, maxJSONBody, &body) {
				return
			}
			agentID := strings.TrimSpace(body.AgentID)
			// agent_id 可留空：留空表示"不绑定"，任何主机可用一次，身份由该机决定
			// （客户端未指定 agent_id 时回退为机器 UUID）。填写时仍必须通过合法性校验。
			if agentID != "" && !validAgentID(agentID) {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "agent_id 非法（不能含空白或超过 128 字符；留空表示不绑定）"})
				return
			}
			token, rec, err := store.CreateToken(agentID, body.Label, time.Duration(body.TTLSeconds)*time.Second)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			binding := "绑定 agent_id=" + agentID
			if agentID == "" {
				binding = "不绑定 agent_id（任意主机一次，默认按机器 UUID 上线）"
			}
			auditEnrollOp(r, authz, router, agentID, "签发一次性注册令牌",
				"令牌 "+rec.ID+"，"+binding+"，有效期至 "+time.Unix(rec.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
			msg := "令牌已签发（只显示这一次）。填进该机的 agent_key 或 enrollment_secret 后启动 Agent 即可"
			if agentID == "" {
				msg = "不绑定令牌已签发（只显示这一次）：任意主机可用一次，身份由该机决定（未指定 agent_id 时默认机器 UUID）。填进 agent_key 或 enrollment_secret 后启动 Agent 即可"
			}
			writeJSON(w, map[string]interface{}{
				"ok": true, "token": token, "id": rec.ID, "agent_id": rec.AgentID,
				"unbound":    rec.AgentID == "",
				"expires_at": rec.ExpiresAt,
				"msg":        msg,
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// enrollRevokeHandler 吊销令牌：按 id 单条吊销，或按 agent_id 清掉该机全部未使用令牌。
func enrollRevokeHandler(store *agentauth.EnrollStore, authz *auth.Auth, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "注册令牌存储不可用（详见服务端启动日志）"})
			return
		}
		var body struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		id := strings.TrimSpace(body.ID)
		agentID := strings.TrimSpace(body.AgentID)
		switch {
		case id != "":
			if !store.RevokeToken(id) {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "未找到该令牌（可能已过期被清理）"})
				return
			}
			auditEnrollOp(r, authz, router, agentID, "吊销一次性注册令牌", "令牌 "+id)
		case agentID != "":
			if !validAgentID(agentID) {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "agent_id 非法"})
				return
			}
			n := store.RevokeAgentTokens(agentID)
			auditEnrollOp(r, authz, router, agentID, "吊销该机全部未使用注册令牌", "共 "+strconv.Itoa(n)+" 枚")
		default:
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "需指定 id 或 agent_id"})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "已吊销"})
	}
}

// enrollBindingResetHandler 清除某台主机的证书 TOFU 绑定。
//
// 正常恢复路径其实是「给这台机签一枚新令牌」（令牌本身就是运维对这台主机的授权）；
// 这个显式清除口留给"绑定来自误操作/迁移前旧数据"的场合 —— 它进审计，
// 而手删 enroll.json 会把全部主机的绑定一起抹掉，风险大得多。
func enrollBindingResetHandler(store *agentauth.EnrollStore, authz *auth.Auth, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "注册令牌存储不可用（详见服务端启动日志）"})
			return
		}
		var body struct {
			AgentID string `json:"agent_id"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		agentID := strings.TrimSpace(body.AgentID)
		if !validAgentID(agentID) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "agent_id 非法"})
			return
		}
		if !store.ResetBinding(agentID) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "该主机没有证书绑定记录"})
			return
		}
		auditEnrollOp(r, authz, router, agentID, "清除证书 TOFU 绑定", "该机下次换取证书需出示新令牌")
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "绑定已清除"})
	}
}

// sharedKeyRotateHandler 轮换部署级共享注册密钥，并把新明文一次性回显给运维。
//
// 为什么还要留一个能拿到明文的接口：M-1 之后 /api/server-info 不再下发密钥，而"给一台
// 还没迁到一次性令牌的老机器配密钥"仍然需要明文。与其保留被动泄露，不如把它做成显式动作 ——
// 要确认、进审计、且旧密钥对新注册立刻失效。
//
// keyFromArgs 表示当前密钥来自 -agent-key / 配置文件 / MOCENTER_AGENT_KEY：这种形态下
// 启动时会用那个值盖掉 <data>/agent.key，所以轮换只在本次进程内有效，必须告警。
func sharedKeyRotateHandler(verifier *agentauth.Verifier, authz *auth.Auth, router *route.Router, keyPath string, keyFromArgs bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 响应里有根凭证明文：绝不许浏览器或反代缓存这一份。
		w.Header().Set("Cache-Control", "no-store")
		// 轮换会让"手里拿着旧密钥、还没接进来"的机器注册失败，所以要求显式确认，
		// 而不是一个不带确认字段的请求就能触发。
		var body struct {
			Confirm bool `json:"confirm"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		if !body.Confirm {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "轮换后旧密钥立即失效（已连接主机不受影响，尚未接入的机器须改用新密钥），确认请重发 confirm=true"})
			return
		}
		if !verifier.SharedKeyEnabled() {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "共享注册密钥当前不可用（已关闭或未配置），无需轮换；请改用一次性注册令牌"})
			return
		}
		oldFP := verifier.KeyFingerprint()
		newKey, err := verifier.RotateSharedKey(keyPath)
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "轮换失败: " + err.Error()})
			return
		}
		newFP := verifier.KeyFingerprint()
		warnings := []string{
			"新密钥只显示这一次，之后接口只回指纹",
			"仍在用旧密钥准备的机器要改用新密钥（已凭证书在线的主机不受影响）",
		}
		if keyFromArgs {
			warnings = append(warnings, "当前密钥来自启动参数/配置文件/环境变量，下次启动它会盖掉 "+keyPath+
				"：本次轮换到重启即失效。要长期生效，请删除配置项 agent_key（或 MOCENTER_AGENT_KEY、-agent-key）后重启")
		}
		// 审计只留指纹：把明文写进审计库等于换了个地方继续泄露。
		auditEnrollOp(r, authz, router, "", "轮换部署级共享注册密钥", "指纹 "+oldFP+" → "+newFP)
		writeJSON(w, map[string]interface{}{
			"ok":          true,
			"agent_key":   newKey,
			"fingerprint": newFP,
			"key_path":    keyPath,
			"warnings":    warnings,
			"msg":         "密钥已轮换并写入 " + keyPath,
		})
	}
}

// enrollDeleteHandler 永久删除一枚**已使用**令牌。
//
// 与「吊销」的差别：吊销是"还没用的令牌作废"；删除是清理账本 —— 令牌已经用过，
// 且它对应的主机已不在册（被删除/手动清理，或绑定记录已清除），这条记录不再
// 对应任何可接入对象，留着只会让页面越来越长。
// 约束（既在界面上体现，也在服务端强制）：
//   - 令牌必须已被使用（未使用的请吊销，避免误删还等着用的凭据）；
//   - 若记录了 agent_id，该主机当前必须不在册；主机仍在册时保留账本。
//     历史上"不绑定、已使用"的令牌没有 agent_id，已消耗且不指向任何主机，允许清理。
func enrollDeleteHandler(store *agentauth.EnrollStore, probes *probe.Store, authz *auth.Auth, router *route.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "注册令牌存储不可用（详见服务端启动日志）"})
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		if !decodeJSON(w, r, maxJSONBody, &body) {
			return
		}
		id := strings.TrimSpace(body.ID)
		rec := store.GetToken(id)
		if rec == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "未找到该令牌（可能已过期被清理）"})
			return
		}
		if rec.UsedAt == 0 {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "该令牌尚未使用：请使用吊销（吊销即作废），不能删除记录"})
			return
		}
		if rec.AgentID != "" && probes != nil && probes.Get(rec.AgentID) != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "该主机仍在册，只能吊销令牌，不能删除记录"})
			return
		}
		if !store.RevokeToken(id) {
			writeJSON(w, map[string]interface{}{"ok": false, "msg": "删除失败（令牌可能已被清理）"})
			return
		}
		auditEnrollOp(r, authz, router, rec.AgentID, "删除已注销主机的注册令牌", "令牌 "+id)
		writeJSON(w, map[string]interface{}{"ok": true, "msg": "令牌记录已删除"})
	}
}

// auditEnrollOp 把令牌的签发/吊销/绑定变更写入操作审计（Category=op）。
func auditEnrollOp(r *http.Request, authz *auth.Auth, router *route.Router, agentID, command, result string) {
	if router == nil {
		return
	}
	router.Audit(&route.AuditLog{
		Time:       time.Now().Unix(),
		Operator:   auditSafeText(authz.Operator(r.Header.Get("Authorization"))),
		OperatorIP: auth.LoginIPOf(r.RemoteAddr),
		AgentID:    auditSafeText(agentID),
		Command:    auditSafeText(command),
		Result:     auditSafeText(result),
		Category:   "op",
	})
}
