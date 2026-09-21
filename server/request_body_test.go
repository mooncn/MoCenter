package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mocenter/server/internal/auth"
)

// TestDecodeJSONBodyCap 所有 JSON 入口都必须在读满内存之前对请求体设上限。
func TestDecodeJSONBodyCap(t *testing.T) {
	cases := []struct {
		name string
		max  int64
		body string
		want int
	}{
		{"超限请求体返回 413", maxCredBody, `{"username":"` + strings.Repeat("a", int(maxCredBody)) + `"}`, http.StatusRequestEntityTooLarge},
		{"上限随参数生效，不是固定值", maxJSONBody, `{"username":"` + strings.Repeat("a", int(maxJSONBody)) + `"}`, http.StatusRequestEntityTooLarge},
		{"刚好在上限内的合法请求体可用", maxCredBody, `{"username":"` + strings.Repeat("a", maxCredBody-len(`{"username":""}`)) + `"}`, 0},
		{"非法 JSON 返回 400", maxJSONBody, `{not json`, http.StatusBadRequest},
		{"上限内正常解析", maxCredBody, `{"username":"admin"}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader(c.body))
			var body struct {
				Username string `json:"username"`
			}
			ok := decodeJSON(rec, req, c.max, &body)
			if c.want == 0 {
				if !ok {
					t.Fatalf("应解析成功，实际 %d: %s", rec.Code, rec.Body.String())
				}
				return
			}
			if ok {
				t.Fatal("超限/非法请求体不应返回成功")
			}
			if rec.Code != c.want {
				t.Fatalf("状态码应为 %d，实际 %d", c.want, rec.Code)
			}
		})
	}
}

// TestLoginHandlerRejectsOversizedBody 匿名可达的登录入口必须先行拒绝超大请求体，
// 而不是把整段内容读进内存后再解析。
func TestLoginHandlerRejectsOversizedBody(t *testing.T) {
	um := auth.NewUserManager(filepath.Join(t.TempDir(), "users.json"))
	h := loginHandler(um, auth.NewAuth(nil), auth.NewLoginGuard(), nil)
	body := `{"username":"` + strings.Repeat("a", maxCredBody) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("登录入口应拒绝超大请求体，实际 %d", rec.Code)
	}
}
