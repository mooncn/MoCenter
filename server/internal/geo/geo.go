// Package geo 提供基于公网 IP 的地理位置解析服务。
//
// 使用免费且无需密钥的 ip-api.com（支持 IPv4/IPv6，返回国家/地区/城市与坐标），
// 结果缓存 24 小时避免频繁请求；请求失败时返回 nil，不影响 Agent 列表展示。
package geo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"mocenter/server/internal/protocol"
)

// ipapiResponse ip-api.com 响应结构。
type ipapiResponse struct {
	Status      string  `json:"status"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"regionName"`
	City        string  `json:"city"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Query       string  `json:"query"`
}

// Resolver 地理位置解析器。
type Resolver struct {
	mu      sync.Mutex
	cache   map[string]*cacheEntry
	timeout time.Duration
}

type cacheEntry struct {
	geo      *protocol.GeoInfo
	expireAt time.Time
}

// NewResolver 创建解析器。
func NewResolver() *Resolver {
	return &Resolver{
		cache:   make(map[string]*cacheEntry),
		timeout: 5 * time.Second,
	}
}

// Lookup 解析给定 IP 的地理位置，带缓存。无法解析时返回 nil。
func (r *Resolver) Lookup(ip string) *protocol.GeoInfo {
	ip = trimIP(ip)
	if ip == "" || ip == "127.0.0.1" || ip == "::1" || ip == "localhost" {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[ip]; ok && time.Now().Before(e.expireAt) {
		r.mu.Unlock()
		return e.geo
	}
	r.mu.Unlock()

	geo := r.fetch(ip)

	r.mu.Lock()
	r.cache[ip] = &cacheEntry{geo: geo, expireAt: time.Now().Add(24 * time.Hour)}
	r.mu.Unlock()
	return geo
}

// fetch 实际请求 ip-api.com。
func (r *Resolver) fetch(ip string) *protocol.GeoInfo {
	client := &http.Client{Timeout: r.timeout}
	// http 批量接口免费；ip-api.com 免费版单 IP 查询需 http（避免 https 依赖）
	url := fmt.Sprintf("http://ip-api.com/json/%s?lang=zh-CN&fields=status,country,countryCode,regionName,city,lat,lon,query", ip)
	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var pr ipapiResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil
	}
	if pr.Status != "success" {
		return nil
	}
	return &protocol.GeoInfo{
		Country:     pr.Country,
		CountryCode: pr.CountryCode,
		Region:      pr.Region,
		City:        pr.City,
		Latitude:    pr.Lat,
		Longitude:   pr.Lon,
	}
}

// trimIP 去除 IP 字符串中的端口与空白。
func trimIP(s string) string {
	s = trimSpace(s)
	if i := indexByte(s, ':'); i > 0 && stringsCountColon(s) == 1 {
		// 形如 host:port 的 IPv4
		s = s[:i]
	}
	return s
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func stringsCountColon(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			n++
		}
	}
	return n
}
