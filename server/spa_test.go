package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSPA 构造一个临时的前端产物目录与 mux（含预压缩产物）。
func newSPA(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<html>app shell</html>")
	write("assets/index-abc123def.css", "body{color:red}")
	write("assets/index-abc123def.css.gz", "gzipped-css")
	write("assets/index-abc123def.js", "console.log(1)")
	write("assets/index-abc123def.js.gz", "gzipped-js")
	write("assets/index-abc123def.js.br", "brotli-js")
	mux := http.NewServeMux()
	serveSPA(mux, dir)
	return mux, dir
}

func doGet(t *testing.T, mux *http.ServeMux, path, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestSPACacheHeaders 回归：带内容哈希的 /assets/* 必须长缓存，
// 否则移动端每次导航都会回源校验，高版本 Chrome 下首屏串行等待明显。
func TestSPACacheHeaders(t *testing.T) {
	mux, _ := newSPA(t)

	w := doGet(t, mux, "/assets/index-abc123def.js", "")
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("/assets/* 应为 immutable 长缓存，实得 %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("js 的 MIME 必须是 JS，实得 %q（MIME 不对浏览器会拒绝执行模块）", ct)
	}

	// SPA 入口与任意路由回退都必须 no-cache，否则发版后前端拿不到新入口
	for _, p := range []string{"/", "/admin", "/public/node/x"} {
		w := doGet(t, mux, p, "")
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s 应 no-cache，实得 %q", p, got)
		}
		if body := w.Body.String(); !strings.Contains(body, "app shell") {
			t.Fatalf("%s 应回退 index.html，实得 %q", p, body)
		}
	}
}

// TestSPAPrecompressed 回归：有预压缩产物时按 Accept-Encoding 优先回传，
// 并声明 Vary，避免中间层把压缩/未压缩内容串给同一个 URL。
func TestSPAPrecompressed(t *testing.T) {
	mux, _ := newSPA(t)

	w := doGet(t, mux, "/assets/index-abc123def.js", "gzip, deflate, br")
	if got := w.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("应优先使用 brotli，实得 %q", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("必须声明 Vary: Accept-Encoding，实得 %q", got)
	}
	if body := w.Body.String(); body != "brotli-js" {
		t.Fatalf("应回传 .br 产物，实得 %q", body)
	}

	w = doGet(t, mux, "/assets/index-abc123def.js", "gzip")
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("无 br 支持时应回退 gzip，实得 %q", got)
	}
	if body := w.Body.String(); body != "gzipped-js" {
		t.Fatalf("应回传 .gz 产物，实得 %q", body)
	}

	w = doGet(t, mux, "/assets/index-abc123def.js", "")
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("客户端未声明压缩时不得加 Content-Encoding，实得 %q", got)
	}
	if body := w.Body.String(); body != "console.log(1)" {
		t.Fatalf("应回传原始文件，实得 %q", body)
	}
}

// TestSPAFallbackAndAPI 回归：SPA 路由回退到 index.html，但 /api 未命中必须 404
// （否则接口探测会拿到一坨 HTML），且已注册的 API 路由不被静态托管拦截。
func TestSPAFallbackAndAPI(t *testing.T) {
	mux, dir := newSPA(t)
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })

	if w := doGet(t, mux, "/admin/terminal", ""); !strings.Contains(w.Body.String(), "app shell") {
		t.Fatalf("SPA 路由应回退 index.html，实得 %q", w.Body.String())
	}
	if w := doGet(t, mux, "/api/unknown", ""); w.Code != http.StatusNotFound {
		t.Fatalf("/api 未命中应 404，实得 %d", w.Code)
	}
	if w := doGet(t, mux, "/api/agents", ""); w.Body.String() != "api" {
		t.Fatalf("已注册的 API 路由不应被静态托管拦截，实得 %q", w.Body.String())
	}

	// 目录穿越不应读到 web 目录之外的内容。net/http 会把 /../x 规范化为 /x 并 301；
	// URL 里带编码的 ..%2f 直接 400。两条路都必须拿不到目录外内容。
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("topsecret"), 0644); err != nil {
		t.Skipf("跳过穿越用例: %v", err)
	}
	for _, p := range []string{"/../secret.txt", "/..%2fsecret.txt", "/%2e%2e/secret.txt"} {
		if w := doGet(t, mux, p, ""); strings.Contains(w.Body.String(), "topsecret") {
			t.Fatalf("目录穿越 %q 不应读到 web 目录之外的文件", p)
		}
	}
}

// TestSPAMethodGuard 非 GET/HEAD 的静态请求直接 405，避免被当成资源写入入口。
func TestSPAMethodGuard(t *testing.T) {
	mux, _ := newSPA(t)
	req := httptest.NewRequest(http.MethodPost, "/assets/index-abc123def.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 静态资源应 405，实得 %d", w.Code)
	}
}

// TestPickEncodingQValues 回归 L-3：q 值必须参与判定。
// 上一版用 strings.Contains，`br;q=0`（明确拒绝）也判成命中，
// `gzip;q=1.0, br;q=0.2`（更想要 gzip）也按出现顺序回 br。
func TestPickEncodingQValues(t *testing.T) {
	cases := map[string]string{
		"br":                   "br",
		"gzip, deflate, br":    "br",
		"br;q=0.5, gzip;q=1.0": "gzip", // gzip 权重更高
		"gzip;q=1.0, br;q=0.5": "gzip",
		"br;q=0":               "",     // 明确拒绝 brotli
		"br;q=0, gzip":         "gzip", // 拒绝 br 但接受 gzip
		"gzip;q=0, br;q=0":     "",     // 两个都拒绝 → 未压缩
		"identity":             "",     // 只接受 identity
		"br;q=abc":             "",     // q 值畸形 → 按不可接受
		"br;  q=0":             "",     // 参数带空格
		"GZIP":                 "gzip", // 大小写不敏感
		"zstd, br":             "br",
		"br, br;q=0":           "", // 后者覆盖前者
	}
	for header, want := range cases {
		if got := pickEncoding(header).name; got != want {
			t.Errorf("pickEncoding(%q)=%q，期望 %q", header, got, want)
		}
	}
}

// TestSPARangeSkipsSidecar 回归 L-3：带 Range 的请求不能走预压缩产物。
// ServeFile 会用 .br 的字节数解释"未压缩字节"的偏移，命中时回一个
// 长度与内容都错位的 206 —— 静默的数据错误。
func TestSPARangeSkipsSidecar(t *testing.T) {
	mux, _ := newSPA(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc123def.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	req.Header.Set("Range", "bytes=0-6")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求应回 206，实得 %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Range 响应不得带 Content-Encoding（偏移量是针对原始字节的），实得 %q", got)
	}
	if body := w.Body.String(); body != "console" {
		t.Fatalf("应回原始字节的 0-6，实得 %q", body)
	}
	// Vary 无条件声明：这条响应的内容确实随 Accept-Encoding 变化
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("必须声明 Vary: Accept-Encoding，实得 %q", got)
	}
}

// TestHasContentHash 内容哈希识别：用于决定是否长缓存，误判会把可变文件永久缓存。
func TestHasContentHash(t *testing.T) {
	cases := map[string]bool{
		"index-CY0gmwch.js":        true, // vite base62 短哈希
		"index-BeND9vrd.css":       true,
		"vendor-react-CMtkvX8y.js": true,
		"index.html":               false, // 入口不可长缓存
		"main.js":                  false,
		"logo-2024-01.png":         false,
		"app-a-b-c-d.js":           false, // 多段连字符不是哈希
	}
	for name, want := range cases {
		if got := hasContentHash(name); got != want {
			t.Errorf("hasContentHash(%q)=%v，期望 %v", name, got, want)
		}
	}
}

// TestMimeByExt 模块脚本必须是 JS MIME；类型不对浏览器会拒绝执行。
func TestMimeByExt(t *testing.T) {
	cases := map[string]string{
		".js":    "text/javascript; charset=utf-8",
		".mjs":   "text/javascript; charset=utf-8",
		".css":   "text/css; charset=utf-8",
		".html":  "text/html; charset=utf-8",
		".svg":   "image/svg+xml",
		".woff2": "font/woff2",
		".bin":   "",
	}
	for ext, want := range cases {
		if got := mimeByExt(ext); got != want {
			t.Errorf("mimeByExt(%q)=%q，期望 %q", ext, got, want)
		}
	}
}
