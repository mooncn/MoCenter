package main

// M-12 / M-13 / M-14 的对外面闭环。
//
// 这三条都在"匿名可达"这条边界上，单测只测内部函数是不够的：
//   - M-12：限流必须**分桶**。共用登录桶等于让访客把管理员挡在自己的后台外面；
//   - M-13：匿名读路径绝不能驱动出网 GeoIP —— 暴露面是这几个 handler，不是 store；
//   - M-14：总开关关掉后，公开端点对外应当"不存在"，品牌文案则要独立于开关。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mocenter/server/internal/auth"
	"mocenter/server/internal/geo"
	"mocenter/server/internal/protocol"
	"mocenter/server/internal/siteconfig"
)

// newGuardedPublicSite 给公开站点测试装上限流器（newPublicKeyedSite 刻意留 nil）。
func newGuardedPublicSite(t *testing.T, agentIDs ...string) *publicSite {
	t.Helper()
	p, _ := newPublicKeyedSite(t, 0x6e, agentIDs...)
	p.guard = auth.NewLoginGuard()
	return p
}

// drainPublic 反复请求同一端点，返回首个非 200 的状态码（全部 200 时返回 0）。
func drainPublic(t *testing.T, h http.HandlerFunc, target string, n int) (int, *httptest.ResponseRecorder) {
	t.Helper()
	for i := 0; i < n; i++ {
		rec := publicGet(t, h, target)
		if rec.Code != http.StatusOK {
			return rec.Code, rec
		}
	}
	return 0, nil
}

// TestPublicEndpointsRateLimited 匿名端点必须有频率上限（此前只有登录有限流）。
func TestPublicEndpointsRateLimited(t *testing.T) {
	p := newGuardedPublicSite(t, "web-01")
	code, rec := drainPublic(t, p.nodesHandler(), "/api/public/nodes", 400)
	if code != http.StatusTooManyRequests {
		t.Fatalf("刷满后应返回 429，实际 code=%d body=%s", code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After，否则客户端只会继续重试")
	}
}

// TestPublicRoutesHaveSeparateBudget 路由之间互不吃额度：刷满 /nodes 不能锁死 /state。
//
// 反向也要成立 —— 这一条正是 M-12 的正题：匿名访客不得消耗管理员的登录额度。
func TestPublicRoutesHaveSeparateBudget(t *testing.T) {
	p := newGuardedPublicSite(t, "web-01")
	// httptest.NewRequest 的对端地址固定是 1.2.3.4
	if code, _ := drainPublic(t, p.nodesHandler(), "/api/public/nodes", 400); code != http.StatusTooManyRequests {
		t.Fatalf("/nodes 应被限流，实际 %d", code)
	}
	if rec := publicGet(t, p.stateHandler(), "/api/public/state"); rec.Code != http.StatusOK {
		t.Fatalf("/state 不应被 /nodes 的流量拖死，实际 %d: %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 20; i++ {
		if ok, _ := p.guard.Allow("1.2.3.4:1234", "admin"); !ok {
			t.Fatalf("第 %d 次登录尝试被匿名访客挤掉：匿名流量吃了登录额度", i+1)
		}
	}
}

// TestPublicNodesDoNotDriveGeoOutbound 公开列表不得触发外呼 GeoIP（M-13）。
//
// 缓存由后台仪表盘那条 store.All() 路径负责填；访客没有资格消耗出网额度
// （免费接口有配额，且本可走 -proxy-ip）。
func TestPublicNodesDoNotDriveGeoOutbound(t *testing.T) {
	p, _ := newPublicKeyedSite(t, 0x7f, "web-01")
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"code":200,"data":{"country":"美国","as_country":"US","region":"加州","city":"洛杉矶"}}`))
	}))
	defer upstream.Close()
	resolver := geo.NewResolver()
	resolver.SetEndpoints(upstream.URL+"/%s", upstream.URL+"/%s", upstream.URL+"/%s")
	p.store.SetGeoResolver(resolver)
	done := make(chan struct{}, 8)
	p.store.SetGeoNotify(func() { done <- struct{}{} })
	// 对照组要求这台主机有公网 IP：没有 IP 时两条路径都不会外呼，断言就成了空转
	p.store.UpsertAgent(&protocol.AgentInfo{AgentID: "web-01", Remark: "web-01", Online: true, PublicIP: "8.8.4.4"})

	for i := 0; i < 5; i++ {
		if rec := publicGet(t, p.nodesHandler(), "/api/public/nodes"); rec.Code != http.StatusOK {
			t.Fatalf("请求失败: %d %s", rec.Code, rec.Body.String())
		}
		publicGet(t, p.stateHandler(), "/api/public/state")
	}
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("匿名请求驱动了 %d 次出网 GeoIP 解析", n)
	}

	// 对照组：证明计数器真的接在解析链上（否则上面的 0 次没有意义）
	p.store.All()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("后台路径未发起解析：计数器/解析器没接上，本用例失去意义")
	}
	if n := atomic.LoadInt32(&calls); n == 0 {
		t.Fatal("store.All() 也未外呼，说明解析器没接上")
	}
}

// decodePublic 解析公开端点的 JSON 响应。
func decodePublic(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, rec.Body.String())
	}
	return out
}

// TestPublicEnabledProbeGatedBySwitch 开关关闭时 /api/public/enabled 也要 404（M-14）。
//
// 留一个永远应答 {"enabled":false} 的端点，等于给扫描器一个"这里有个开关"的
// 确认点；且它曾顺带下发品牌文案 —— 关掉公开页后仍然能被匿名读到。
func TestPublicEnabledProbeGatedBySwitch(t *testing.T) {
	siteCfg := siteconfig.NewStore("")
	if rec := publicGet(t, publicConfigStateHandler(siteCfg), "/api/public/enabled"); rec.Code != http.StatusNotFound {
		t.Fatalf("未启用时应 404（对外不存在），实际 %d: %s", rec.Code, rec.Body.String())
	}

	cfg := siteCfg.Get()
	cfg.Enabled = true
	cfg.SiteTitle = "生产状态页"
	if err := siteCfg.Set(cfg); err != nil {
		t.Fatalf("启用公开站点失败: %v", err)
	}
	rec := publicGet(t, publicConfigStateHandler(siteCfg), "/api/public/enabled")
	if rec.Code != http.StatusOK {
		t.Fatalf("启用后应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"enabled":true`) {
		t.Errorf("响应应确认已启用，实际 %s", body)
	}
	// 品牌文案与公开策略都不从这里出：前者走 /api/branding，后者根本不该给匿名
	for _, leak := range []string{"site_title", "site_footer", "host_ids", "catalog_enabled", "生产状态页"} {
		if strings.Contains(body, leak) {
			t.Errorf("/api/public/enabled 泄漏了 %q: %s", leak, body)
		}
	}
}

// TestBrandingIndependentOfPublicSwitch 品牌文案登录前就要渲染，因此不受公开页开关影响。
func TestBrandingIndependentOfPublicSwitch(t *testing.T) {
	siteCfg := siteconfig.NewStore("")
	cfg := siteCfg.Get()
	cfg.Enabled = false // 公开页关着
	cfg.SiteTitle = "我的机器"
	cfg.SiteFooter = "© 2026 示例单位"
	if err := siteCfg.Set(cfg); err != nil {
		t.Fatalf("写入站点配置失败: %v", err)
	}
	rec := publicGet(t, brandingHandler(siteCfg), "/api/branding")
	if rec.Code != http.StatusOK {
		t.Fatalf("品牌端点应始终可用（登录页要用），实际 %d: %s", rec.Code, rec.Body.String())
	}
	out := decodePublic(t, rec)
	if out["site_title"] != "我的机器" || out["site_footer"] != "© 2026 示例单位" {
		t.Errorf("品牌文案不符: %+v", out)
	}
	// 只出文案：不得顺带回答"公开页开没开"或任何公开策略
	for _, leak := range []string{"enabled", "host_ids", "catalog_enabled", "fields", "hosts_enabled"} {
		if strings.Contains(rec.Body.String(), `"`+leak+`"`) {
			t.Errorf("/api/branding 泄漏了 %q: %s", leak, rec.Body.String())
		}
	}
}

// TestBrandingDefaultsWhenUnset 全新部署（未配置）也要拿到非空文案，且不出主机数据。
func TestBrandingDefaultsWhenUnset(t *testing.T) {
	siteCfg := siteconfig.NewStore("")
	cfg := siteCfg.Get()
	cfg.SiteTitle = ""
	cfg.SiteFooter = ""
	if err := siteCfg.Set(cfg); err != nil {
		t.Fatalf("清空站点配置失败: %v", err)
	}
	out := decodePublic(t, publicGet(t, brandingHandler(siteCfg), "/api/branding"))
	if out["site_title"] != siteconfig.DefaultSiteTitle {
		t.Errorf("标题应回退默认，实际 %v", out["site_title"])
	}
	footer, _ := out["site_footer"].(string)
	if !strings.Contains(footer, "MOONCN") {
		t.Errorf("页脚应回退默认值，实际 %q", footer)
	}
	// `{year}` 占位符由前端按当前年份展开；API 侧必须原样带出标记，不能被吞掉
	if !strings.Contains(footer, siteconfig.FooterYearToken) {
		t.Errorf("页脚应保留年份占位符 %q，实际 %q", siteconfig.FooterYearToken, footer)
	}
}

// TestPublicHandlersNotFoundWhenDisabled 关掉开关后所有公开端点一律 404。
//
// 用 404 而不是 403：未启用时该功能对外应当"不存在"，不给扫描器留确认点。
func TestPublicHandlersNotFoundWhenDisabled(t *testing.T) {
	p := newGuardedPublicSite(t, "web-01")
	cfg := p.cfg.Get()
	cfg.Enabled = false
	if err := p.cfg.Set(cfg); err != nil {
		t.Fatalf("关闭公开站点失败: %v", err)
	}
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"state", p.stateHandler()},
		{"nodes", p.nodesHandler()},
		{"samples", p.samplesHandler()},
	} {
		if rec := publicGet(t, c.h, "/api/public/"+c.name+"?node=n-0000000000000000"); rec.Code != http.StatusNotFound {
			t.Errorf("/api/public/%s 未启用时应 404，实际 %d: %s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestPublicCfgSignalCloseAndReplace 变更信号必须是"关闭旧通道 + 换新通道"。
//
// 在途连接订阅的是自己那一拍拿到的通道；只发不关（或复用同一通道）都会让它们
// 永远收不到唤醒，"停用"于是又退化成只挡新握手。
func TestPublicCfgSignalCloseAndReplace(t *testing.T) {
	p, _ := newPublicWSTestSite(t)
	sub := p.subscribeCfgChange()
	if p.subscribeCfgChange() != sub {
		t.Fatal("未发生变更时订阅应复用同一通道（否则每次事件都要重订阅）")
	}
	p.noteConfigChange()
	select {
	case <-sub:
	default:
		t.Fatal("noteConfigChange 必须关闭旧通道以唤醒在途连接")
	}
	if p.subscribeCfgChange() == sub {
		t.Fatal("变更后必须换到新通道，否则唤醒只生效一次")
	}
}
