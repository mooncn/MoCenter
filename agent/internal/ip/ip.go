// Package ip 提供 Agent 公网出口 IP（IPv4/IPv6）探测能力。
//
// Agent 通过 HTTPS 请求公共回显服务获取本机在公网上的出口地址。
// 国内网络环境下部分境外回显服务可能不可达，支持通过加速代理前缀
// （如 http://fa.mooncn.win/?url=https://xxxxx）访问；探测结果缓存，
// 失败时不阻塞 Agent 主流程。
package ip

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Result 公网出口 IP 探测结果。
type Result struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

// Detector 公网 IP 探测器。
type Detector struct {
	// Proxy 加速代理前缀，如 http://fa.mooncn.win/?url= ，为空则不使用。
	Proxy string
	// Timeout 单次探测超时（默认 5 秒）。
	Timeout time.Duration

	mu     sync.Mutex
	result Result
	// 缓存过期时间，避免频繁探测
	expireAt time.Time
}

// NewDetector 创建探测器。proxy 为可选的国内加速代理前缀。
func NewDetector(proxy string) *Detector {
	return &Detector{
		Proxy:   proxy,
		Timeout: 5 * time.Second,
	}
}

// httpGet 带代理前缀与超时的 GET 请求。
func (d *Detector) httpGet(rawURL string) ([]byte, error) {
	url := rawURL
	if d.Proxy != "" {
		// 代理格式形如 http://fa.mooncn.win/?url=https%3A%2F%2Fxxxxx
		if strings.Contains(d.Proxy, "?url=") {
			url = d.Proxy + urlEncode(rawURL)
		} else if !strings.HasSuffix(d.Proxy, "/") {
			url = d.Proxy + "/?url=" + urlEncode(rawURL)
		} else {
			url = d.Proxy + "?url=" + urlEncode(rawURL)
		}
	}
	client := &http.Client{
		Timeout: d.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func urlEncode(s string) string {
	// 简单编码：保留公开回显 URL，仅转义必要字符
	replacer := strings.NewReplacer(
		" ", "%20", "?", "%3F", "&", "%26", "=", "%3D",
		"#", "%23", "%", "%25",
	)
	return replacer.Replace(s)
}

// DetectIPv4 探测公网 IPv4。
func (d *Detector) DetectIPv4() (string, error) {
	// 依次尝试多个回显服务，取首个成功结果。
	//
	// 顺序按**国内可达性**排：ipify / ident.me 在部分国内网络下不可达
	// （连接重置 / DNS 污染），会导致公网 IP 拿不到 —— 而服务端的地理位置
	// （节点地区、国旗）完全依赖这个 IP，于是表现为"地区和国旗不显示"。
	// 因此把国内厂商回显（ip.sb、腾讯 Anycast）放在前面，境外服务仅作兜底。
	services := []string{
		"https://api-ipv4.ip.sb/ip",
		"https://ipv4.icanhazip.com",
		"https://ipv4.ident.me/",
		"https://api.ipify.org?format=json",
	}
	var lastErr error
	for _, svc := range services {
		body, err := d.httpGet(svc)
		if err != nil {
			lastErr = err
			continue
		}
		if ip := parseIPResponse(body); ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("无法获取公网 IPv4: %v", lastErr)
}

// DetectIPv6 探测公网 IPv6。
func (d *Detector) DetectIPv6() (string, error) {
	// 同样优先国内可达的回显服务（ip.sb 走腾讯 Anycast）。
	services := []string{
		"https://api-ipv6.ip.sb/ip",
		"https://ipv6.icanhazip.com",
		"https://ipv6.ident.me/",
		"https://api6.ipify.org?format=json",
	}
	var lastErr error
	for _, svc := range services {
		body, err := d.httpGet(svc)
		if err != nil {
			lastErr = err
			continue
		}
		if ip := parseIPResponse(body); ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("无法获取公网 IPv6: %v", lastErr)
}

// parseIPResponse 从多种回显服务响应中解析 IP 字符串。
func parseIPResponse(body []byte) string {
	text := strings.TrimSpace(string(body))
	// 优先尝试 JSON：{ "ip": "x.x.x.x" } 或 { "ip":"..." }
	var m map[string]interface{}
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"ip", "query", "IPv4", "IPv6"} {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	// 纯文本形式：取首个合法的 IPv4/IPv6 片段
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		tok = strings.Trim(tok, `",'`)
		if strings.Contains(tok, ".") || strings.Contains(tok, ":") {
			return tok
		}
	}
	return ""
}

// 探测结果的缓存时长。
const (
	// cacheOK 探测到公网 IP 后的缓存时长：地址在运营商层面变化不频繁。
	cacheOK = 30 * time.Minute
	// cacheFail 探测失败后的重试间隔：必须**远短于** cacheOK。
	//
	// 这里曾是一个隐藏 Bug：失败结果（空 IP）也被当成"有效结果"缓存 30 分钟，
	// 于是开机/断网/回显服务抖动导致的首轮失败，会让这台机器在半小时内
	// 一直上报空公网 IP —— 服务端因此解析不出地理位置，公开页的
	// "节点地区 + 国旗"就一直是空的，且**重启 Agent 也可能再撞上**。
	cacheFail = 60 * time.Second
)

// Resolve 综合探测 IPv4 与 IPv6，带结果缓存。
func (d *Detector) Resolve() Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Now().Before(d.expireAt) {
		return d.result
	}
	var r Result
	if v4, err := d.DetectIPv4(); err == nil {
		r.IPv4 = v4
	}
	if v6, err := d.DetectIPv6(); err == nil {
		r.IPv6 = v6
	}
	d.result = r
	// 只探测到部分结果时按"成功"缓存，但下一次仍会重新尝试补齐缺失的那一类
	// （expireAt 到期即重探，成功结果本身也会被重新探测刷新）。
	if r.IPv4 != "" || r.IPv6 != "" {
		d.expireAt = time.Now().Add(cacheOK)
	} else {
		d.expireAt = time.Now().Add(cacheFail)
	}
	return r
}
