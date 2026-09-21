package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/publicview"
	"mocenter/server/internal/siteconfig"
)

// H-8 的服务端侧闭环：公开接口对外的主机标识必须由**部署私钥**派生。
//
// 单测只测 publicview 的派生函数不够 —— 真正的暴露面是这两个免登录端点：
// /api/public/nodes 给出编号，/api/public/samples?node=… 用编号反查历史。
// 只要编号是"人人可算"的，匿名访客就能拿主机名候选离线算一遍、用 404/200 当判定器，
// 把"哪些内网主机在册"逐条问出来（hostname 开关关着也挡不住）。

func newPublicKeyedSite(t *testing.T, secret byte, agentIDs ...string) (*publicSite, *publicview.NodeKeys) {
	t.Helper()
	keys, err := publicview.NewNodeKeys(bytes.Repeat([]byte{secret}, 32))
	if err != nil {
		t.Fatalf("构造节点编号密钥失败: %v", err)
	}
	store := probe.NewStore(nil)
	for _, id := range agentIDs {
		store.UpsertAgent(&protocol.AgentInfo{AgentID: id, Remark: id, Online: true})
	}
	siteCfg := siteconfig.NewStore("")
	cfg := siteCfg.Get()
	cfg.Enabled = true
	cfg.HostsEnabled = true
	if err := siteCfg.Set(cfg); err != nil {
		t.Fatalf("写入公开站点配置失败: %v", err)
	}
	return &publicSite{store: store, cfg: siteCfg, nodeKeys: keys}, keys
}

// legacyNodeID 复刻修复前的派生方式（固定盐 + SHA-256 截 4 字节），用于反证。
func legacyNodeID(agentID string) string {
	sum := sha256.Sum256([]byte("mocenter-public-node|" + agentID))
	return "n-" + hex.EncodeToString(sum[:4])
}

func publicGet(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// TestPublicNodesUseKeyedID 列表里的编号必须等于带密钥派生值，且不等于旧的固定盐值。
func TestPublicNodesUseKeyedID(t *testing.T) {
	p, keys := newPublicKeyedSite(t, 0x2a, "web-01", "db-prod")
	rec := publicGet(t, p.nodesHandler(), "/api/public/nodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	got := map[string]bool{}
	for _, n := range body.Nodes {
		got[n.NodeID] = true
	}
	for _, agentID := range []string{"web-01", "db-prod"} {
		if !got[keys.ID(agentID)] {
			t.Errorf("列表应含 %s 的密钥派生编号 %s，实际: %v", agentID, keys.ID(agentID), got)
		}
		if got[legacyNodeID(agentID)] {
			t.Errorf("%s 仍在用固定盐编号（离线字典可命中）", agentID)
		}
	}
	// 编号不得泄漏 agent_id 的任何片段
	if strings.Contains(rec.Body.String(), "mocenter-public-node") {
		t.Error("响应里出现了旧盐常量")
	}
}

// TestPublicSamplesRejectsLegacyID 用旧算法算出的编号必须查不到历史（判定器失效）。
func TestPublicSamplesRejectsLegacyID(t *testing.T) {
	p, keys := newPublicKeyedSite(t, 0x3b, "web-01")
	// 该主机有历史才有 200 的意义；没有历史时是 200 + 空数组，两种都算"命中"。
	p.store.UpdateProbe(&protocol.ProbeDataReq{AgentID: "web-01", CPUUsage: 11, Timestamp: time.Now().Unix()})

	if rec := publicGet(t, p.samplesHandler(), "/api/public/samples?node="+legacyNodeID("web-01")); rec.Code != http.StatusNotFound {
		t.Errorf("旧固定盐编号应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec := publicGet(t, p.samplesHandler(), "/api/public/samples?node="+keys.ID("web-01"))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确编号应命中，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		NodeID  string            `json:"node_id"`
		Samples []json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.NodeID != keys.ID("web-01") {
		t.Errorf("回显编号 = %q，期望 %q", body.NodeID, keys.ID("web-01"))
	}
	if len(body.Samples) == 0 {
		t.Error("命中了主机却没有采样，说明反查没有真正落到 agent_id")
	}
	// 响应里绝不能出现内部标识
	if strings.Contains(rec.Body.String(), `"agent_id"`) {
		t.Error("公开响应泄漏了 agent_id")
	}
}

// TestPublicNodeIDChangesWithKey 换密钥即整体换编号：旧编号一律查不到。
//
// 这是"轮换"的全部代价 —— 历史数据内部按 agent_id 存，因此不需要迁移；
// 受影响的是已经分享出去的 /public/node/… 链接。
func TestPublicNodeIDChangesWithKey(t *testing.T) {
	p, keys := newPublicKeyedSite(t, 0x4c, "web-01")
	p.store.UpdateProbe(&protocol.ProbeDataReq{AgentID: "web-01", CPUUsage: 5, Timestamp: time.Now().Unix()})
	old := keys.ID("web-01")

	rotated, err := publicview.NewNodeKeys(bytes.Repeat([]byte{0x5d}, 32))
	if err != nil {
		t.Fatalf("构造轮换密钥失败: %v", err)
	}
	p.nodeKeys = rotated
	if rec := publicGet(t, p.samplesHandler(), "/api/public/samples?node="+old); rec.Code != http.StatusNotFound {
		t.Errorf("轮换后旧编号应 404，实际 %d", rec.Code)
	}
	if rec := publicGet(t, p.samplesHandler(), "/api/public/samples?node="+rotated.ID("web-01")); rec.Code != http.StatusOK {
		t.Errorf("轮换后新编号应可用，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rotated.ID("web-01") == old {
		t.Error("换密钥后编号未变，等于没有轮换")
	}
}
