package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/config"
)

func decodeResp(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	return resp
}

// TestSystemSessionTTLUpdate 会话时长只接受白名单值，并同步写入配置文件与运行期。
func TestSystemSessionTTLUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mocenter-server.json")
	settings := &settingsStore{path: path, cfg: &config.Config{TokenTTL: "12h"}}
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok", "admin")
	h := systemSessionHandler(authz, settings, nil)

	rec := doJSON(t, h, http.MethodPost, `{"ttl":"1h"}`, "tok")
	resp := decodeResp(t, rec.Body.Bytes())
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("合法 TTL 应保存成功: %s", rec.Body.String())
	}
	if got := authz.TokenTTL(); got != time.Hour {
		t.Fatalf("运行期 TTL 应为 1h，实际 %v", got)
	}
	loaded, err := config.Load(path)
	if err != nil || loaded == nil {
		t.Fatalf("读取配置文件失败: %v", err)
	}
	if loaded.TokenTTL != "1h" {
		t.Fatalf("配置文件应保存 1h，实际 %q", loaded.TokenTTL)
	}

	rec = doJSON(t, h, http.MethodPost, `{"ttl":"10000h"}`, "tok")
	resp = decodeResp(t, rec.Body.Bytes())
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("不在白名单里的 TTL 必须拒绝")
	}
	if got := authz.TokenTTL(); got != time.Hour {
		t.Fatalf("非法请求不应改变 TTL，实际 %v", got)
	}
}

// TestSystemListenRequiresPasswordAndConfirm 修改监听是高风险操作：
// 缺密码、错密码、未勾选确认都必须在触碰监听器之前拒绝。
func TestSystemListenRequiresPasswordAndConfirm(t *testing.T) {
	dir := t.TempDir()
	um := auth.NewUserManager(filepath.Join(dir, "users.json"))
	if err := um.UpsertUser("admin", "secret123", ""); err != nil {
		t.Fatal(err)
	}
	authz := auth.NewAuth(nil)
	authz.RegisterToken("tok", "admin")
	settings := &settingsStore{path: filepath.Join(dir, "cfg.json"), cfg: &config.Config{}}
	noListeners := func() *listenerHub { return nil }

	agent := systemAgentListenHandler(authz, um, settings, noListeners, nil, nil)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"缺确认", `{"host":"","port":9001,"password":"secret123"}`},
		{"缺密码", `{"host":"","port":9001,"password":"","confirm":true}`},
		{"错密码", `{"host":"","port":9001,"password":"wrong","confirm":true}`},
	} {
		rec := doJSON(t, agent, http.MethodPost, tc.body, "tok")
		resp := decodeResp(t, rec.Body.Bytes())
		if ok, _ := resp["ok"].(bool); ok {
			t.Fatalf("%s：应被拒绝", tc.name)
		}
	}

	httpH := systemHTTPListenHandler(authz, um, settings, noListeners, nil)
	rec := doJSON(t, httpH, http.MethodPost, `{"host":"","port":8081,"password":"wrong","confirm":true}`, "tok")
	resp := decodeResp(t, rec.Body.Bytes())
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("后台监听：错误密码应被拒绝")
	}
}

// TestNormalizeListenAddr 监听地址只接受 IP；通配 host 归一为 ":port"。
func TestNormalizeListenAddr(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
		bad  bool
	}{
		{"", 9000, ":9000", false},
		{"0.0.0.0", 9000, "0.0.0.0:9000", false},
		{"::", 9000, "[::]:9000", false},
		{"192.168.1.10", 8080, "192.168.1.10:8080", false},
		{"example.com", 9000, "", true},
		{"1.2.3.4", 0, "", true},
		{"1.2.3.4", 70000, "", true},
	}
	for _, tc := range cases {
		got, err := normalizeListenAddr(tc.host, tc.port)
		if tc.bad {
			if err == nil {
				t.Fatalf("host=%q port=%d 应被拒绝，实际得到 %q", tc.host, tc.port, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("host=%q port=%d: want %q, got %q err=%v", tc.host, tc.port, tc.want, got, err)
		}
	}
}
