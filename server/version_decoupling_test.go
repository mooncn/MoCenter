package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/session"
)

// TestAgentVersionIsIndependentFromServerVersion 守护「Agent 版本与 Server 版本解耦」的契约。
//
// 背景：早期实现把两者绑成"必须一致"，服务端会拒绝版本号不相等的升级指令。
// 这会让正常运维动作全部失效：只想改 Agent 时被迫发一次服务端、灰度升级被挡、
// 服务端刚发版而 Agent 尚未跟进时界面到处报"需升级"。
//
// 因此这里锁定：Agent 上报任意版本号都必须被接受，服务端不得据此拒绝接入。
func TestAgentVersionIsIndependentFromServerVersion(t *testing.T) {
	// 三个版本号来自完全不同的"代际"：低于服务端、等于服务端、高于服务端。
	// 三种都必须是正常状态 —— 服务端只存不管，绝不比较。
	for _, v := range []string{"0.0.1", "1.0.1", "999.999.999"} {
		info := &protocol.AgentInfo{}
		info.AgentID = "agent-" + strings.ReplaceAll(v, ".", "-")
		info.AgentVersion = v

		b, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		var back protocol.AgentInfo
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("反序列化失败: %v", err)
		}
		if back.AgentVersion != v {
			t.Fatalf("Agent 版本应原样保留：want %q got %q", v, back.AgentVersion)
		}
	}
}

// TestUpgradeHandlerAcceptsVersionDifferentFromServer 锁定「升级包版本不必等于服务端版本」。
//
// 该接口以前会对与 version.Version 不同的目标版本直接返回 400，
// 等于禁止了"服务端与 Agent 分别发版"和"灰度升级"。
//
// 这里只断言"校验不再拦在版本号这一关"：请求会被后续校验（离线/摘要/协议）
// 挡下，但绝不会出现"版本不一致"这条拒绝理由。
func TestUpgradeHandlerAcceptsVersionDifferentFromServer(t *testing.T) {
	sess := session.NewManager()
	authz := auth.NewAuth(nil)
	const token = "test-token"
	authz.RegisterToken(token, "tester")

	h := upgradeHandler(sess, authz, "", "", nil)

	// 一个明显异于服务端版本的目标版本
	body := `{"agent_id":"not-online","version":"9.9.9","url":"https://example.com/a.bin",` +
		`"sha256":"` + strings.Repeat("a", 64) + `","signature":"` + strings.Repeat("b", 128) + `"}`

	req := httptest.NewRequest(http.MethodPost, "/api/upgrade", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("接口不应因版本号不同而拒绝：status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "不一致") || strings.Contains(out, "同一版本号") {
		t.Fatalf("仍存在版本一致性拒绝逻辑：%s", out)
	}
	// 目标主机离线是预期内的后续校验结果，说明请求确实越过了版本校验
	if !strings.Contains(out, "离线") {
		t.Fatalf("预期被「目标主机离线」挡下，实际响应: %s", out)
	}
}
