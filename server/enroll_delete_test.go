package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"mocenter/server/internal/agentauth"
	"mocenter/server/internal/auth"
	"mocenter/server/internal/probe"
	"mocenter/server/internal/protocol"
)

func newDeleteTokenFixture(t *testing.T) (*agentauth.EnrollStore, *probe.Store, http.HandlerFunc) {
	t.Helper()
	store, err := agentauth.NewEnrollStore(filepath.Join(t.TempDir(), agentauth.EnrollStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	probes := probe.NewStore(nil)
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok", "admin")
	h := enrollDeleteHandler(store, probes, authz, nil)
	return store, probes, h
}

func consumeToken(t *testing.T, store *agentauth.EnrollStore, agentID string) *agentauth.EnrollToken {
	t.Helper()
	secret, rec, err := store.CreateToken(agentID, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := agentauth.NewVerifier("")
	v.SetEnrollStore(store)
	ts := time.Now().Unix()
	nonce := agentauth.NewNonce()
	if _, err := v.Verify(agentID, nonce, agentauth.Proof(secret, agentID, nonce, ts), ts); err != nil {
		t.Fatalf("令牌消费失败: %v", err)
	}
	return rec
}

// TestDeleteUsedTokenAfterHostRemoved 已使用令牌：主机不在册即可删除，
// 不再要求"必须还有证书绑定"（手动清理绑定后也应能清理账本）。
func TestDeleteUsedTokenAfterHostRemoved(t *testing.T) {
	store, _, h := newDeleteTokenFixture(t)
	rec := consumeToken(t, store, "host-gone")

	resp := decodeResp(t, doJSON(t, h, http.MethodPost, `{"id":"`+rec.ID+`"}`, "tok").Body.Bytes())
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("主机不在册时应允许删除: %+v", resp)
	}
	if store.GetToken(rec.ID) != nil {
		t.Fatal("删除后令牌记录应消失")
	}
}

// TestDeleteUsedTokenForRegisteredHostRejected 主机仍在册时保留账本。
func TestDeleteUsedTokenForRegisteredHostRejected(t *testing.T) {
	store, probes, h := newDeleteTokenFixture(t)
	rec := consumeToken(t, store, "host-live")
	probes.UpsertAgent(&protocol.AgentInfo{AgentID: "host-live"})

	resp := decodeResp(t, doJSON(t, h, http.MethodPost, `{"id":"`+rec.ID+`"}`, "tok").Body.Bytes())
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("主机仍在册时不应允许删除")
	}
	if store.GetToken(rec.ID) == nil {
		t.Fatal("拒绝删除后令牌记录应保留")
	}
}

// TestDeleteUnusedTokenRejected 未使用令牌只能吊销，不能直接删记录。
func TestDeleteUnusedTokenRejected(t *testing.T) {
	store, _, h := newDeleteTokenFixture(t)
	_, rec, err := store.CreateToken("host-unused", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeResp(t, doJSON(t, h, http.MethodPost, `{"id":"`+rec.ID+`"}`, "tok").Body.Bytes())
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("未使用令牌不应允许删除")
	}
}
