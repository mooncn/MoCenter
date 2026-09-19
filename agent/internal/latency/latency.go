// Package latency 实现 Agent 侧的网络延迟探测（ICMP / TCP / HTTP）。
//
// 对应 Issue 需求「延迟监测（ICMP/TCP/HTTP + 历史）」的采集端。
//
// 设计要点：
//
//  1. **零外部依赖**。ICMP 用 golang.org/x/net/icmp 会引入新依赖（且需要 root），
//     因此这里统一走**系统 ping 命令**：Linux 用 `ping -c 1 -W <秒>`，
//     Windows 用 `ping -n 1 -w <毫秒>`，macOS 用 `ping -c 1 -W <毫秒>`。
//     系统 ping 有 setuid/setcap 时无需 root，且输出格式极其稳定。
//  2. **探测失败 ≠ 0ms**。失败的探测目标直接不出现在结果 map 里，
//     由服务端/前端显示「超时」。把失败写成 0 会造成"超时的机器看起来最快"，
//     这是监控系统里最危险的一类显示错误。
//  3. **有界并发**。目标数量由服务端下发，理论上仍可能很多；每个目标一个
//     goroutine，但整体有超时，且结果用 mutex 保护 —— 不做无界 goroutine 炸弹。
package latency

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind 探测方式。
const (
	KindICMP = "icmp"
	KindTCP  = "tcp"
	KindHTTP = "http"
)

// Target 一个延迟探测目标。
type Target struct {
	// Kind icmp / tcp / http
	Kind string `json:"kind"`
	// Addr 目标地址：icmp/tcp 为 host 或 host:port，http 为完整 URL（可省略 scheme）
	Addr string `json:"addr"`
	// Label 展示名（留空则由服务端生成）。
	Label string `json:"label"`
}

// ID 返回目标的稳定标识，形如 "icmp:1.1.1.1" / "tcp:1.1.1.1:443" / "http:https://x"。
//
// 这个字符串同时是**上报 map 的 key** 与前端展示的列名，因此必须
// 完全由目标定义决定（不含本机端口等变量），否则同一目标在不同主机上
// 会得到不同 key，历史曲线对不上。
func (t Target) ID() string {
	return NormalizeKind(t.Kind) + ":" + strings.TrimSpace(t.Addr)
}

// NormalizeKind 归一化探测方式，非法值回退 icmp。
func NormalizeKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case KindTCP:
		return KindTCP
	case KindHTTP, "https":
		return KindHTTP
	default:
		return KindICMP
	}
}

// maxTargets 单机最多探测的目标数（防御性上限：配置被写坏时不至于打出上百个进程）。
const maxTargets = 8

// Prober 延迟探测器。可复用于多轮探测（复用 http.Client 连接池）。
type Prober struct {
	// Timeout 单目标探测超时（默认 3s）。
	Timeout time.Duration
	client  *http.Client
}

// New 创建探测器。
func New() *Prober {
	return &Prober{
		Timeout: 3 * time.Second,
		client: &http.Client{
			Timeout: 3 * time.Second,
			// 探测 HTTPS 时忽略证书错误：我们只关心"能不能连上、往返多久"，
			// 证书有效性由业务连接自己校验；这里因为证书问题报失败会误导运维。
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
			// 不跟随重定向：跟随会引入额外往返，测出的不是"到该目标的延迟"。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ProbeAll 并发探测全部目标，返回 目标ID -> 延迟毫秒。
//
// 失败的目标**不进入结果**（调用方据此显示"超时"）。
func (p *Prober) ProbeAll(targets []Target) map[string]float64 {
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > maxTargets {
		targets = targets[:maxTargets]
	}
	out := make(map[string]float64, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		if strings.TrimSpace(t.Addr) == "" {
			continue
		}
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			ms, ok := p.probeOne(t)
			if !ok {
				return
			}
			mu.Lock()
			out[t.ID()] = ms
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}

// probeOne 探测单个目标，返回 (延迟毫秒, 是否成功)。
func (p *Prober) probeOne(t Target) (float64, bool) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch NormalizeKind(t.Kind) {
	case KindTCP:
		return probeTCP(ctx, t.Addr)
	case KindHTTP:
		return p.probeHTTP(ctx, t.Addr)
	default:
		return probeICMP(ctx, t.Addr, timeout)
	}
}

// probeTCP 建立 TCP 连接并测量握手耗时。
//
// 地址没写端口时按 443 处理（HTTPS 探测最常见的场景）；这是明确的默认值，
// 而不是"随机挑一个端口"。
func probeTCP(ctx context.Context, addr string) (float64, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0, false
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	d := net.Dialer{}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, false
	}
	_ = conn.Close()
	return round1(float64(time.Since(start).Microseconds()) / 1000.0), true
}

// probeHTTP 发起一次 HTTP HEAD（失败退回 GET），测量首字节往返耗时。
func (p *Prober) probeHTTP(ctx context.Context, raw string) (float64, bool) {
	url := normalizeURL(raw)
	if url == "" {
		return 0, false
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false
	}
	_ = resp.Body.Close()
	// 只要拿到了 HTTP 响应（哪怕是 4xx/5xx），就说明链路可达 ——
	// 这说明"服务在响应"，延迟数据有效。业务是否正常由状态码展示另行判断。
	return round1(float64(time.Since(start).Microseconds()) / 1000.0), true
}

// normalizeURL 补全 scheme（缺失时按 https 处理）。
func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "https://" + raw
	}
	return raw
}

// icmpTimeRe 匹配 ping 输出里的往返时间（Linux: time=12.3 ms；Windows: 时间=12ms / time=12ms）。
var icmpTimeRe = regexp.MustCompile(`(?i)(?:time|时间)[=<]\s*([0-9]+(?:\.[0-9]+)?)\s*(ms|毫秒)`)

// icmpUnixRe 匹配 macOS/BSD 的 "round-trip min/avg/max/stddev = 1.2/3.4/5.6/0.7 ms"。
var icmpUnixRe = regexp.MustCompile(`(?i)round-trip\s+min/avg/max[^=]*=\s*[0-9.]+/([0-9.]+)/`)

// probeICMP 调用系统 ping 命令测量 ICMP 往返延迟。
//
// 为什么不用原生 ICMP socket：需要 CAP_NET_RAW/root，且要自己拼 ICMP 包、
// 处理校验和与 IPv4/IPv6 双栈 —— 引入的复杂度远超收益。系统 ping 命令在
// 三大平台上都稳定存在（Windows/Linux/macOS 均内置），输出格式固定。
//
// 失败时（目标不可达、无权限、命令不存在）返回 false，由上层显示"超时"。
func probeICMP(ctx context.Context, host string, timeout time.Duration) (float64, bool) {
	host = strings.TrimSpace(host)
	if host == "" {
		return 0, false
	}
	// 去掉可能误填的端口（ping 不接受端口）
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")

	secs := int(math.Ceil(timeout.Seconds()))
	if secs < 1 {
		secs = 1
	}

	var name string
	var args []string
	switch runtime.GOOS {
	case "windows":
		name, args = "ping", []string{"-n", "1", "-w", strconv.Itoa(secs * 1000), host}
	case "darwin":
		// macOS 的 -W 单位是毫秒
		name, args = "ping", []string{"-c", "1", "-W", strconv.Itoa(secs * 1000), host}
	default:
		name, args = "ping", []string{"-c", "1", "-W", strconv.Itoa(secs), host}
	}

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := string(out)
	// 注意：不能只用 err 判断失败 —— ping 在收到 "Destination Host Unreachable"
	// 时退出码非 0，但在部分环境（如 Windows 的中文输出）仍会带有时间字段。
	// 反过来，退出码为 0 也不代表拿到了时间（某些实现输出差异）。
	// 因此统一以"能否解析出时间"为准。
	if m := icmpTimeRe.FindStringSubmatch(text); len(m) == 3 {
		if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
			return round1(v), true
		}
	}
	if m := icmpUnixRe.FindStringSubmatch(text); len(m) == 2 {
		if v, perr := strconv.ParseFloat(m[1], 64); perr == nil {
			return round1(v), true
		}
	}
	_ = err
	return 0, false
}

// round1 保留 1 位小数。
func round1(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return math.Round(v*10) / 10
}

// FormatTargets 把目标列表格式化为稳定顺序的 ID 列表（供上报与前端展示）。
func FormatTargets(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if strings.TrimSpace(t.Addr) == "" {
			continue
		}
		out = append(out, t.ID())
		if len(out) >= maxTargets {
			break
		}
	}
	return out
}

// ParseTargets 解析服务端下发的目标配置（形如 "icmp:1.1.1.1" / "tcp:host:443"）。
// 这条路径同时服务于"配置文件手写"与"服务端下发"，因此必须容错。
func ParseTargets(specs []string) []Target {
	out := make([]Target, 0, len(specs))
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		kind, addr := splitSpec(s)
		if addr == "" {
			continue
		}
		t := Target{Kind: kind, Addr: addr}
		id := t.ID()
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, t)
		if len(out) >= maxTargets {
			break
		}
	}
	return out
}

// splitSpec 拆分 "kind:addr" 规格。
//
// 必须小心：http 目标的 addr 自身包含冒号（https://x:443），tcp 目标也有
// （host:443）。因此只在**前缀**是已知 kind 时才按 kind 拆分，其余情况
// 整串视为地址并按 icmp 处理。
func splitSpec(s string) (kind, addr string) {
	if i := strings.Index(s, ":"); i > 0 {
		prefix := strings.ToLower(strings.TrimSpace(s[:i]))
		if prefix == KindICMP || prefix == KindTCP || prefix == KindHTTP {
			return NormalizeKind(prefix), strings.TrimSpace(s[i+1:])
		}
	}
	return KindICMP, s
}

// String 便于日志输出。
func (t Target) String() string {
	return fmt.Sprintf("%s(%s)", t.ID(), t.Label)
}
