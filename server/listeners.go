package main

import (
	"context"
	"errors"
	"fmt"
	"mocenter/server/internal/logx"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// listenerHub 管理两个对外监听器（Agent TCP / Web HTTP）的启动与热切换。
//
// 热切换只替换监听器，不重启进程：已建立的连接不受影响，新连接走新地址。
// 同端口换绑定主机时必须先释放旧监听（通配地址占用整个端口），因此 HTTP 侧
// 在那种情况下会先返回响应、再由后台完成任务，失败则自动恢复旧监听。
type listenerHub struct {
	mu      sync.Mutex
	applyMu sync.Mutex
	// agentAddr/httpAddr 当前生效的监听地址；与配置文件里的值不一定相同
	// （配置文件写入失败时运行期仍会继续用新地址，直到重启）。
	agentAddr string
	httpAddr  string
	agentLn   net.Listener
	httpSrv   *http.Server

	// onAgent 处理每条新接入的 Agent 连接（由 main 注入）。
	onAgent func(net.Conn)
	// buildAgent/buildHTTP 创建监听器；必须完成 TLS 包装与证书签发。
	buildAgent func(addr string) (net.Listener, error)
	buildHTTP  func(addr string) (net.Listener, *http.Server, error)
}

// Start 启动两个监听器；任一失败返回错误（由调用方决定是否退出进程）。
func (l *listenerHub) Start(agentAddr, httpAddr string) error {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()

	webLn, webSrv, err := l.buildHTTP(httpAddr)
	if err != nil {
		return fmt.Errorf("Web 控制台监听 %s 失败: %w", httpAddr, err)
	}
	l.mu.Lock()
	l.httpAddr = httpAddr
	l.httpSrv = webSrv
	l.mu.Unlock()
	go serveWeb(webSrv, webLn)

	agentLn, err := l.buildAgent(agentAddr)
	if err != nil {
		_ = webSrv.Close()
		return fmt.Errorf("Agent TCP 监听 %s 失败: %w", agentAddr, err)
	}
	l.mu.Lock()
	l.agentAddr = agentAddr
	l.agentLn = agentLn
	l.mu.Unlock()
	go l.acceptAgents(agentLn)
	return nil
}

// AgentAddr / HTTPAddr 返回当前生效地址。
func (l *listenerHub) AgentAddr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.agentAddr
}

func (l *listenerHub) HTTPAddr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.httpAddr
}

func (l *listenerHub) acceptAgents(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 瞬时错误（句柄耗尽等）不应把监听协程永久打死；
			// 但也不能空转：退避 100ms 再继续。
			logx.Infof("[server] Agent accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go l.onAgent(conn)
	}
}

func serveWeb(srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil &&
		!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		logx.Infof("[server] Web 控制台监听结束: %v", err)
	}
}

// RebindAgent 切换 Agent TCP 监听。
//
// 先绑新地址、成功后再关旧地址（端口不同时两者可短暂并存，不中断新连接）。
// 同端口换主机时旧监听占着整个端口，只能先释放；失败则恢复旧地址，
// 保证最坏情况下 Agent 仍能按原地址接入。
func (l *listenerHub) RebindAgent(addr string) error {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()

	l.mu.Lock()
	oldAddr, oldLn := l.agentAddr, l.agentLn
	l.mu.Unlock()
	if addr == oldAddr {
		return nil
	}

	ln, err := l.buildAgent(addr)
	closedOld := false
	if err != nil && sameListenPort(oldAddr, addr) && oldLn != nil {
		_ = oldLn.Close()
		closedOld = true
		ln, err = l.buildAgent(addr)
		if err != nil {
			restore, rerr := l.buildAgent(oldAddr)
			if rerr != nil {
				l.mu.Lock()
				l.agentLn = nil
				l.mu.Unlock()
				return fmt.Errorf("新地址监听失败: %v；旧地址 %s 也无法恢复: %v，请检查本机网络/防火墙后重启服务",
					err, oldAddr, rerr)
			}
			l.mu.Lock()
			l.agentAddr = oldAddr
			l.agentLn = restore
			l.mu.Unlock()
			go l.acceptAgents(restore)
			return fmt.Errorf("新地址监听失败: %v（已恢复旧地址 %s）", err, oldAddr)
		}
	} else if err != nil {
		return fmt.Errorf("新地址监听失败: %w", err)
	}

	l.mu.Lock()
	l.agentAddr = addr
	l.agentLn = ln
	l.mu.Unlock()
	go l.acceptAgents(ln)
	if oldLn != nil && !closedOld {
		_ = oldLn.Close()
	}
	logx.Infof("[server] Agent TCP 监听已切换: %s → %s", oldAddr, addr)
	return nil
}

// RebindHTTP 切换 Web 监听。pending=true 表示同端口换主机，必须先释放旧监听：
// 调用方应先返回响应，后台会完成切换并在失败时自动恢复旧监听。
func (l *listenerHub) RebindHTTP(addr string, persist func(string) error) (pending bool, err error) {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()

	l.mu.Lock()
	oldAddr, oldSrv := l.httpAddr, l.httpSrv
	l.mu.Unlock()
	if addr == oldAddr {
		return false, nil
	}

	if sameListenPort(oldAddr, addr) {
		// 同一端口换绑定主机：旧监听（尤其是 :8080）占着端口，先返回再切换。
		time.AfterFunc(time.Second, func() {
			l.applySamePortHTTP(addr, oldAddr, oldSrv, persist)
		})
		return true, nil
	}

	ln, srv, err := l.buildHTTP(addr)
	if err != nil {
		return false, fmt.Errorf("新地址监听失败: %w", err)
	}
	if err := persist(addr); err != nil {
		_ = ln.Close()
		return false, fmt.Errorf("写入配置文件失败（未切换）: %w", err)
	}
	l.mu.Lock()
	l.httpAddr = addr
	l.httpSrv = srv
	l.mu.Unlock()
	go serveWeb(srv, ln)
	if oldSrv != nil {
		// 给当前请求留出写响应的时间，再优雅关闭旧监听。
		time.AfterFunc(time.Second, func() { shutdownWeb(oldSrv) })
	}
	logx.Infof("[server] Web 控制台监听已切换: %s → %s", oldAddr, addr)
	return false, nil
}

func (l *listenerHub) applySamePortHTTP(addr, oldAddr string, oldSrv *http.Server, persist func(string) error) {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()

	l.mu.Lock()
	curAddr, curSrv := l.httpAddr, l.httpSrv
	l.mu.Unlock()
	// 期间已被其它操作切换：放弃本次，避免覆盖新状态。
	if curAddr != oldAddr || curSrv != oldSrv {
		return
	}
	if oldSrv != nil {
		shutdownWeb(oldSrv)
	}

	ln, srv, err := l.buildHTTP(addr)
	if err != nil {
		restoreLn, restoreSrv, rerr := l.buildHTTP(oldAddr)
		if rerr != nil {
			l.mu.Lock()
			l.httpSrv = nil
			l.mu.Unlock()
			logx.Errorf("[server] Web 监听切换失败且无法恢复 %s: %v", oldAddr, rerr)
			return
		}
		l.mu.Lock()
		l.httpAddr = oldAddr
		l.httpSrv = restoreSrv
		l.mu.Unlock()
		go serveWeb(restoreSrv, restoreLn)
		logx.Errorf("[server] Web 监听切换失败，已恢复旧地址 %s: %v", oldAddr, err)
		if perr := persist(oldAddr); perr != nil {
			logx.Errorf("[server] 恢复旧地址后写回配置失败: %v", perr)
		}
		return
	}

	l.mu.Lock()
	l.httpAddr = addr
	l.httpSrv = srv
	l.mu.Unlock()
	go serveWeb(srv, ln)
	if err := persist(addr); err != nil {
		logx.Errorf("[server] 新监听已生效，但写入配置失败（重启后将回到旧地址）: %v", err)
	}
	logx.Infof("[server] Web 控制台监听已切换: %s → %s", oldAddr, addr)
}

func shutdownWeb(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

// normalizeListenAddr 把 host + port 归一成 net.Listen 可用的地址。
// host 只接受 IP（留空 = 全部网卡），避免把主机名解析交给 DNS 后绑定到意外地址。
func normalizeListenAddr(host string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("端口必须在 1-65535 之间")
	}
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host != "" {
		ip := net.ParseIP(host)
		if ip == nil {
			return "", fmt.Errorf("host 必须是 IPv4/IPv6 地址；留空表示监听全部网卡")
		}
		host = ip.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// splitListenAddr 拆出 host/port；空 host 返回 ""。
func splitListenAddr(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func sameListenPort(a, b string) bool {
	_, pa, ea := splitListenAddr(a)
	_, pb, eb := splitListenAddr(b)
	return ea == nil && eb == nil && pa == pb
}

// wildcardListenHost 判断监听地址是否绑定全部网卡。
func wildcardListenHost(host string) bool {
	host = strings.TrimSpace(host)
	return host == "" || host == "0.0.0.0" || host == "::"
}
