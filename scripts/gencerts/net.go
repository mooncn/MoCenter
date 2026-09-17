package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// detectPublicIPs 探测本机公网出口 IP（IPv4 / IPv6）。
//
// 服务端证书的 SAN 必须包含 Agent 连接时使用的地址，而部署到公网服务器后
// 该地址通常要到运行时才知道 —— 这里自动探测，避免再手敲 -ip。
// 探测失败（断网 / 被拦截）不是致命错误：调用方会退回本机网卡地址并给出提示。
func detectPublicIPs() (string, string) {
	const timeout = 5 * time.Second
	v4, _ := detectFrom([]string{
		"https://api.ipify.org?format=json",
		"https://ipv4.ident.me/",
		"https://ifconfig.me/ip",
		"https://api.ip.sb/ip",
	}, timeout)
	v6, _ := detectFrom([]string{
		"https://api6.ipify.org?format=json",
		"https://ipv6.ident.me/",
	}, timeout)
	return v4, v6
}

// detectFrom 依次请求多个回显服务，返回首个解析出的公网 IP。
func detectFrom(urls []string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for _, u := range urls {
		ip, err := fetchEchoIP(client, u)
		if err != nil {
			lastErr = err
			continue
		}
		if ip != "" {
			return ip, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("所有回显服务均未返回可用地址")
	}
	return "", lastErr
}

func fetchEchoIP(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(body))
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) == nil {
		for _, k := range []string{"ip", "query", "IPv4", "IPv6"} {
			if s, ok := m[k].(string); ok {
				if ip := parsePublicIP(s); ip != "" {
					return ip, nil
				}
			}
		}
	}
	return parsePublicIP(text), nil
}

// parsePublicIP 从响应文本中提取公网 IP（滤掉回环/私网/非法值）。
func parsePublicIP(s string) string {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return ""
	}
	ip := net.ParseIP(strings.Trim(f[0], `"',[]`))
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}
