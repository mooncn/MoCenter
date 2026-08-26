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
	// 依次尝试多个回显服务，取首个成功结果
	services := []string{
		"https://api.ipify.org?format=json",
		"https://ipv4.ident.me/",
		"https://api.ip.sb/geoip",
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
	services := []string{
		"https://api6.ipify.org?format=json",
		"https://ipv6.ident.me/",
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

// Resolve 综合探测 IPv4 与 IPv6，带结果缓存。
func (d *Detector) Resolve() Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	// 缓存 30 分钟
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
	d.expireAt = time.Now().Add(30 * time.Minute)
	return r
}
