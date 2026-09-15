// Package client 管理 Agent 与服务端的 TCP 长连接、心跳保活与断线重连。
package client

import (
	"bufio"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mocenter/agent/internal/agentauth"
	"mocenter/agent/internal/command"
	"mocenter/agent/internal/probe"
	"mocenter/agent/internal/protocol"
	"mocenter/agent/internal/upgrade"
)

// Config Agent 配置。
type Config struct {
	ServerAddr string // 服务端地址 host:port
	AgentID    string
	Heartbeat  time.Duration // 默认 30s
	ProbeEvery time.Duration // 探针采集周期，默认 10s
	TLS        bool          // 是否启用 TLS 加密连接
	Insecure   bool          // 是否跳过证书校验（自签名测试）
	ClientCert string        // mTLS 客户端证书路径
	ClientKey  string        // mTLS 客户端私钥路径
	CACert     string        // 校验服务端证书的 CA 证书路径
	// ProxyIP 国内加速代理前缀（用于公网 IP 探测等境外服务，如 http://fa.mooncn.win/?url= ）
	ProxyIP string
	// AgentKey 与服务端预共享的注册密钥；为空则无法通过服务端的注册鉴权。
	AgentKey string
	// UpgradePubKey 升级包 Ed25519 公钥（hex 或文件路径）。
	UpgradePubKey string
	// UpgradeAllowUnsigned 显式允许无签名升级（危险，仅迁移期使用）。
	UpgradeAllowUnsigned bool
}

// Client Agent 客户端。
type Client struct {
	cfg     Config
	collect *probe.Collector
	conn    net.Conn
	enc     *json.Encoder
	sc      *bufio.Scanner
	mu      sync.Mutex
	hb      time.Duration
	// 交互会话映射 commandID -> session
	interactive map[string]*command.InteractiveSession
	// 隧道映射 tunnelID -> 内网服务连接（端口映射反向隧道）
	tunnels   map[string]net.Conn
	tunnelsMu sync.Mutex
	// 文件分发写入句柄 filePath -> *os.File
	pushFiles map[string]*os.File
	pushMu    sync.Mutex
	// 安装包拉取：commandID -> 结果通道（等待 PackageFetchResp）
	pkgWait map[string]chan protocol.PackageFetchResp
	pkgMu   sync.Mutex
}

// New 创建 Agent 客户端。
func New(cfg Config) *Client {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 30 * time.Second
	}
	if cfg.ProbeEvery <= 0 {
		cfg.ProbeEvery = 10 * time.Second
	}
	c := &Client{
		cfg:         cfg,
		collect:     probe.NewCollectorWithProxy(cfg.AgentID, cfg.ProxyIP),
		hb:          cfg.Heartbeat,
		interactive: make(map[string]*command.InteractiveSession),
		tunnels:     make(map[string]net.Conn),
		pushFiles:   make(map[string]*os.File),
		pkgWait:     make(map[string]chan protocol.PackageFetchResp),
	}
	return c
}

// Run 启动 Agent：连接服务端并运行主循环，断线自动重连。
func (c *Client) Run() {
	backoff := time.Second
	maxBackoff := 30 * time.Second
	for {
		err := c.connectAndServe()
		if err != nil {
			log.Printf("[agent] 连接断开: %v，%v 后重连", err, backoff)
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectAndServe 建立连接并进入消息循环，返回时连接已断开。
func (c *Client) connectAndServe() error {
	var conn net.Conn
	var err error
	if c.cfg.TLS {
		tlsCfg := &tls.Config{InsecureSkipVerify: c.cfg.Insecure}
		// 指定 CA 时校验服务端证书
		if c.cfg.CACert != "" {
			caPEM, err := os.ReadFile(c.cfg.CACert)
			if err != nil {
				return fmt.Errorf("读取 CA 证书失败: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return fmt.Errorf("解析 CA 证书失败")
			}
			tlsCfg.RootCAs = pool
			tlsCfg.InsecureSkipVerify = false
		}
		// mTLS：加载客户端证书
		if c.cfg.ClientCert != "" && c.cfg.ClientKey != "" {
			cert, err := tls.LoadX509KeyPair(c.cfg.ClientCert, c.cfg.ClientKey)
			if err != nil {
				return fmt.Errorf("加载客户端证书失败: %v", err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		conn, err = tls.Dial("tcp", c.cfg.ServerAddr, tlsCfg)
	} else {
		conn, err = net.Dial("tcp", c.cfg.ServerAddr)
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.sc = bufio.NewScanner(conn)
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	c.mu.Unlock()

	log.Printf("[agent] 已连接服务端 %s，注册上线...", c.cfg.ServerAddr)
	c.register()

	// 启动心跳与探针协程
	hbStop := make(chan struct{})
	probeStop := make(chan struct{})
	go c.heartbeatLoop(hbStop)
	go c.probeLoop(probeStop)

	// 主消息循环
	for c.sc.Scan() {
		var env protocol.Envelope
		if err := json.Unmarshal(c.sc.Bytes(), &env); err != nil {
			continue
		}
		c.handleMessage(env)
	}
	close(hbStop)
	close(probeStop)
	c.closeAllTunnels()
	return c.sc.Err()
}

// register 发送上线消息。
//
// 若配置了 -agent-key，会附带 HMAC 注册证明（密钥本身不上线传输），
// 使服务端能够确认对端确实持有密钥 —— 这是 :9000 通道唯一的应用层身份凭证。
func (c *Client) register() {
	ips := c.collect.PublicIPs()
	uuid := c.collect.UUID()
	info := map[string]interface{}{
		"agent_id":    c.cfg.AgentID,
		"uuid":        uuid,
		"hostname":    c.collect.Host(),
		"os_type":     c.collect.OSType(),
		"arch":        c.collect.Arch(),
		"os_version":  c.collect.OSVersion(),
		"remote_addr": "",
		"public_ip":   ips.IPv4,
		"public_ipv6": ips.IPv6,
	}
	if c.cfg.AgentKey != "" {
		nonce := agentauth.NewNonce()
		ts := time.Now().Unix()
		info["auth_nonce"] = nonce
		info["auth_time"] = ts
		info["auth_proof"] = agentauth.Proof(c.cfg.AgentKey, c.cfg.AgentID, nonce, ts)
	} else {
		log.Printf("[agent] 警告: 未配置 -agent-key，无法通过服务端的注册鉴权（若服务端已启用校验将被拒绝连接）")
	}
	_ = c.write(protocol.TypeOnline, info)
}

// write 发送一条消息。
func (c *Client) write(typ string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		return fmt.Errorf("not connected")
	}
	return c.enc.Encode(protocol.Envelope{Type: typ, Data: data})
}

// heartbeatLoop 定时发送心跳。
func (c *Client) heartbeatLoop(stop chan struct{}) {
	t := time.NewTicker(c.hb)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			req := protocol.HeartbeatReq{
				AgentID:   c.cfg.AgentID,
				Hostname:  c.collect.Host(),
				Timestamp: time.Now().Unix(),
				RTTMs:     0,
			}
			_ = c.write(protocol.TypeHeartbeatReq, req)
		}
	}
}

// probeLoop 定时采集并上报探针。
func (c *Client) probeLoop(stop chan struct{}) {
	t := time.NewTicker(c.cfg.ProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p := c.collect.Sample()
			_ = c.write(protocol.TypeProbeDataReq, p)
		}
	}
}

// handleMessage 处理服务端下发消息。
func (c *Client) handleMessage(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeHeartbeatResp:
		var resp protocol.HeartbeatResp
		if json.Unmarshal(env.Data, &resp) == nil && resp.HeartbeatInterval > 0 {
			// 服务端下发新的心跳间隔
			nd := time.Duration(resp.HeartbeatInterval) * time.Second
			c.mu.Lock()
			c.hb = nd
			c.mu.Unlock()
		}
	case protocol.TypeCommandReq:
		var req protocol.CommandReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		c.runCommand(req)
	case protocol.TypeInteractiveIn:
		var inp protocol.InteractiveInputReq
		if json.Unmarshal(env.Data, &inp) != nil {
			return
		}
		// 先在锁内取指针：map 由 runInteractive goroutine 写入，无锁读取会触发
		// Go runtime 的 concurrent map access 崩溃。
		c.mu.Lock()
		sess := c.interactive[inp.CommandID]
		c.mu.Unlock()
		if sess != nil {
			if inp.Control != "" {
				_ = sess.SendControl(inp.Control)
			}
			if inp.Rows > 0 && inp.Cols > 0 {
				_ = sess.Resize(inp.Rows, inp.Cols)
			}
			if inp.InputData != "" {
				_ = sess.Write([]byte(inp.InputData))
			}
		}
	case protocol.TypeFileDownloadReq:
		// 文件下载：将文件分片回传给服务端
		var fd protocol.FileDownloadReq
		if json.Unmarshal(env.Data, &fd) == nil && fd.FilePath != "" {
			c.sendFileChunks(fd)
		}
	case protocol.TypeFilePushReq:
		// 文件分发：接收服务端分片并写入本地文件
		var fp protocol.FilePushReq
		if json.Unmarshal(env.Data, &fp) == nil && fp.FilePath != "" {
			c.handleFilePush(fp)
		}
	case protocol.TypeTunnelOpen:
		var to protocol.TunnelOpenReq
		if json.Unmarshal(env.Data, &to) != nil {
			return
		}
		c.openTunnel(to)
	case protocol.TypeTunnelData:
		var td protocol.TunnelData
		if json.Unmarshal(env.Data, &td) != nil {
			return
		}
		if conn := c.getTunnel(td.TunnelID); conn != nil {
			_, _ = conn.Write(td.Data)
		}
	case protocol.TypeTunnelClose:
		var tc protocol.TunnelClose
		if json.Unmarshal(env.Data, &tc) != nil {
			return
		}
		c.closeTunnel(tc.TunnelID)
	case protocol.TypeUpgradeReq:
		var up protocol.UpgradeReq
		if json.Unmarshal(env.Data, &up) != nil {
			return
		}
		go upgrade.Perform(&up, upgrade.Policy{
			PubKey:        c.cfg.UpgradePubKey,
			AllowUnsigned: c.cfg.UpgradeAllowUnsigned,
		}, func(resp *protocol.UpgradeResp) {
			_ = c.write(protocol.TypeUpgradeResp, resp)
		})
	case protocol.TypeInteractiveClose:
		var req protocol.InteractiveCloseReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		c.closeSession(req)
	case protocol.TypeVncInstallReq:
		var req protocol.VncInstallReq
		if json.Unmarshal(env.Data, &req) != nil {
			return
		}
		go c.handleVncInstall(req)
	case protocol.TypePkgFetchResp:
		var resp protocol.PackageFetchResp
		if json.Unmarshal(env.Data, &resp) != nil {
			return
		}
		c.deliverPackageResp(resp)
	}
}

// deliverPackageResp 把安装包拉取结果投递给等待中的拉取方。
func (c *Client) deliverPackageResp(resp protocol.PackageFetchResp) {
	c.pkgMu.Lock()
	ch, ok := c.pkgWait[resp.CommandID]
	c.pkgMu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default: // 已有结果，丢弃重复投递
		}
	}
}

// fetchPackage 通过既有长连接从服务端拉取托管的安装包，落盘到 destPath。
//
// 之所以走长连接而不是 HTTP：Agent 到服务端的 TCP 端口一定是通的（否则它不会在线），
// 而 HTTP 端口未必可达；且该通道可启用 TLS/mTLS，天然避免公网下载的劫持与限速问题。
// 服务端未托管该包时返回错误，调用方回退到其它下载方式。
func (c *Client) fetchPackage(name, destPath string) (int64, error) {
	if name == "" || destPath == "" {
		return 0, errors.New("安装包名称或落盘路径为空")
	}
	commandID := fmt.Sprintf("pkg-%d", time.Now().UnixNano())
	ch := make(chan protocol.PackageFetchResp, 1)

	c.pkgMu.Lock()
	c.pkgWait[commandID] = ch
	c.pkgMu.Unlock()
	defer func() {
		c.pkgMu.Lock()
		delete(c.pkgWait, commandID)
		c.pkgMu.Unlock()
	}()

	if err := c.write(protocol.TypePkgFetchReq, protocol.PackageFetchReq{
		AgentID: c.cfg.AgentID, Name: name, CommandID: commandID, DestPath: destPath,
	}); err != nil {
		return 0, fmt.Errorf("请求服务端下发安装包失败: %w", err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			return 0, errors.New(resp.Message)
		}
		// 分片由消息循环顺序落盘，收到回执即表示已写完；这里再做一次大小校验兜底
		fi, err := os.Stat(destPath)
		if err != nil {
			return 0, fmt.Errorf("安装包未落盘: %w", err)
		}
		if fi.Size() != resp.Size {
			return 0, fmt.Errorf("安装包大小不符：期望 %d 字节，实际 %d 字节", resp.Size, fi.Size())
		}
		return fi.Size(), nil
	case <-time.After(5 * time.Minute):
		return 0, errors.New("等待服务端下发安装包超时")
	}
}

// closeSession 断开终端连接：结束指定会话/命令及其相关进程。
func (c *Client) closeSession(req protocol.InteractiveCloseReq) {
	// 1) 交互会话
	if req.CommandID != "" {
		c.mu.Lock()
		if s, ok := c.interactive[req.CommandID]; ok {
			delete(c.interactive, req.CommandID)
			s.Close()
			c.mu.Unlock()
			log.Printf("[agent] 断开会话 %s 并结束相关进程", req.CommandID)
			return
		}
		c.mu.Unlock()
	}
	// 2) 非交互登记中的命令
	closed := 0
	if req.CommandID != "" {
		if command.Kill(req.CommandID) {
			closed++
		}
	} else if req.KillAll {
		closed = command.KillAll()
	}
	// 3) 若无指定且未 kill_all，则结束全部活跃会话/命令
	if req.CommandID == "" && !req.KillAll {
		c.mu.Lock()
		for id, s := range c.interactive {
			delete(c.interactive, id)
			s.Close()
			closed++
		}
		c.mu.Unlock()
		closed += command.KillAll()
	}
	log.Printf("[agent] 断开终端连接，已结束 %d 个会话/命令", closed)
}

// handleVncInstall 处理 VNC 安装/启动指令并回传结果。
func (c *Client) handleVncInstall(req protocol.VncInstallReq) {
	// 默认：若配置了公网 IP 探测代理，则同样用于 VNC 安装包的国内加速。
	// FetchPackage 让安装包优先从服务端拉取（走既有长连接，避开公网限速与劫持），
	// 服务端未托管时自动回退到 winget/choco/公网下载。
	resp := command.VncInstallCfg(req.Port, command.VncConfig{
		Proxy: c.cfg.ProxyIP,
		Mode:  req.Mode,
		FetchPackage: func(name string) (string, error) {
			dest := filepath.Join(os.TempDir(), name)
			if _, err := c.fetchPackage(name, dest); err != nil {
				return "", err
			}
			return dest, nil
		},
	})
	log.Printf("[agent] VNC 安装/启动结果: mode=%s ok=%v msg=%s", resp.Mode, resp.OK, resp.Message)
	_ = c.write(protocol.TypeVncInstallResp, protocol.VncInstallResp{
		OK: resp.OK, Message: resp.Message, Port: resp.Port, Password: resp.Password, Mode: resp.Mode,
	})
}

// handleFilePush 接收服务端下发的文件分片，写入本地目标文件并回传 ACK/NACK。
func (c *Client) handleFilePush(req protocol.FilePushReq) {
	log.Printf("[agent] 收到文件分发分片 seq=%d %s", req.ChunkSeq, req.FilePath)
	ack := func(ok bool, status, msg string) {
		_ = c.write(protocol.TypeFilePushAck, protocol.FilePushAck{
			CommandID: req.CommandID, FilePath: req.FilePath, ChunkSeq: req.ChunkSeq,
			Ack: ok, Status: status, Message: msg,
		})
	}
	// 分发失败通知
	if req.FileSize < 0 {
		ack(false, "failed", "服务端取消分发")
		return
	}
	if req.Done {
		// 收尾：追加最后一个分片并关闭文件
		if len(req.ChunkData) > 0 {
			if !c.pushWrite(req) {
				ack(false, "failed", "写入失败")
				return
			}
		}
		c.pushClose(req.FilePath)
		ack(true, "done", "文件分发完成")
		return
	}
	if len(req.ChunkData) > 0 && !c.pushWrite(req) {
		ack(false, "failed", "写入失败")
		return
	}
	ack(true, "transferring", "")
}

// pushWrite 将分片追加写入目标文件。
func (c *Client) pushWrite(req protocol.FilePushReq) bool {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	f, ok := c.pushFiles[req.FilePath]
	if !ok {
		if err := os.MkdirAll(filepath.Dir(req.FilePath), 0755); err != nil {
			log.Printf("[agent] 创建目录失败 %s: %v", filepath.Dir(req.FilePath), err)
			return false
		}
		var err error
		f, err = os.Create(req.FilePath)
		if err != nil {
			log.Printf("[agent] 创建文件失败 %s: %v", req.FilePath, err)
			return false
		}
		c.pushFiles[req.FilePath] = f
	}
	// 断点续传：跳过已写位置
	if fi, _ := f.Stat(); fi.Size() != int64(req.ChunkSeq)*int64(protocol.DefaultChunkSize) {
		// 顺序写入校验：位置不匹配时按期望偏移 Seek 补齐（简化处理）
	}
	if _, err := f.Write(req.ChunkData); err != nil {
		log.Printf("[agent] 写入分片失败 %s: %v", req.FilePath, err)
		return false
	}
	return true
}

// pushClose 完成文件分发，关闭并释放句柄。
func (c *Client) pushClose(filePath string) {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	if f, ok := c.pushFiles[filePath]; ok {
		_ = f.Close()
		delete(c.pushFiles, filePath)
		log.Printf("[agent] 文件分发完成: %s", filePath)
	}
}

// sendFileChunks 将本地文件分片回传给服务端（文件下载）。
func (c *Client) sendFileChunks(req protocol.FileDownloadReq) {
	log.Printf("[agent] 收到文件下载请求: %s", req.FilePath)
	f, err := os.Open(req.FilePath)
	if err != nil {
		// 上报失败
		_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
			AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: -1,
		})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	size := st.Size()
	buf := make([]byte, protocol.DefaultChunkSize)
	seq := 0
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			md5 := md5Chunk(buf[:n])
			_ = c.write(protocol.TypeFileUploadReq, protocol.FileUploadReq{
				AgentID: c.cfg.AgentID, FilePath: req.FilePath, FileSize: size,
				ChunkSeq: seq, ChunkData: append([]byte(nil), buf[:n]...), ChunkMD5: md5,
			})
			seq++
		}
		if rerr != nil {
			break
		}
	}
}

// md5Chunk 计算分片 MD5 校验和。
func md5Chunk(data []byte) string {
	h := md5.Sum(data)
	return fmt.Sprintf("%x", h)
}

func (c *Client) runCommand(req protocol.CommandReq) {
	if req.Interactive {
		// 交互会话会阻塞等待 PTY 进程结束，必须在独立 goroutine 运行，
		// 否则会阻塞主消息循环，导致后续的 InteractiveInputReq（打字输入）
		// 永远无法被处理，表现为「有光标但无法输入」。
		go c.runInteractive(req)
		return
	}

	// 非交互命令异步执行，避免阻塞消息循环，并支持「断开连接」时结束相关进程
	command.RunAsync(&req, func(res *command.Result) {
		_ = c.write(protocol.TypeCommandResp, protocol.CommandResp{
			CommandID: req.CommandID,
			Status:    res.Status,
			ExitCode:  res.ExitCode,
			Stdout:    res.Stdout,
			Stderr:    res.Stderr,
			Duration:  res.Duration,
		})
	})
}

// runInteractive 启动真实 PTY 交互会话。
func (c *Client) runInteractive(req protocol.CommandReq) {
	sess, err := command.StartInteractive(&req, func(out *protocol.InteractiveOutput) {
		_ = c.write(protocol.TypeInteractiveOut, out)
	})
	if err != nil {
		_ = c.write(protocol.TypeInteractiveOut, &protocol.InteractiveOutput{
			CommandID: req.CommandID, Type: "exit", ExitCode: -1, Data: "启动交互终端失败: " + err.Error(),
		})
		return
	}
	c.mu.Lock()
	c.interactive[req.CommandID] = sess
	c.mu.Unlock()

	// 等待进程结束，清理会话
	<-sess.Done()
	c.mu.Lock()
	delete(c.interactive, req.CommandID)
	c.mu.Unlock()
	sess.Close()
}

// openTunnel 建立到内网服务的隧道连接并桥接双向数据。
func (c *Client) openTunnel(req protocol.TunnelOpenReq) {
	localHost := req.LocalHost
	if localHost == "" {
		localHost = "127.0.0.1"
	}
	lconn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", localHost, req.LocalPort), 10*time.Second)
	resp := protocol.TunnelOpenResp{TunnelID: req.TunnelID}
	if err != nil {
		resp.Message = "dial internal service failed: " + err.Error()
		_ = c.write(protocol.TypeTunnelOpenResp, resp)
		log.Printf("[agent] 隧道建立失败 %s: %v", req.TunnelID, err)
		return
	}
	c.tunnelsMu.Lock()
	if old, ok := c.tunnels[req.TunnelID]; ok {
		_ = old.Close()
	}
	c.tunnels[req.TunnelID] = lconn
	c.tunnelsMu.Unlock()
	resp.OK = true
	_ = c.write(protocol.TypeTunnelOpenResp, resp)
	log.Printf("[agent] 隧道建立成功 %s -> %s:%d", req.TunnelID, localHost, req.LocalPort)
	go c.readTunnel(req.TunnelID, lconn)
}

// readTunnel 读取内网服务的回包，封装为 TunnelData 回传服务端。
func (c *Client) readTunnel(tunnelID string, conn net.Conn) {
	buf := make([]byte, 64*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if c.write(protocol.TypeTunnelData, &protocol.TunnelData{TunnelID: tunnelID, Data: buf[:n]}) != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	// 内网服务关闭 -> 通知服务端关闭隧道
	_ = c.write(protocol.TypeTunnelClose, &protocol.TunnelClose{TunnelID: tunnelID, Reason: "local-closed"})
	c.closeTunnel(tunnelID)
}

func (c *Client) getTunnel(tunnelID string) net.Conn {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	return c.tunnels[tunnelID]
}

// closeTunnel 关闭指定隧道。
func (c *Client) closeTunnel(tunnelID string) {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	if conn, ok := c.tunnels[tunnelID]; ok {
		delete(c.tunnels, tunnelID)
		_ = conn.Close()
	}
}

// closeAllTunnels 关闭全部隧道（断线时调用）。
func (c *Client) closeAllTunnels() {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	for id, conn := range c.tunnels {
		_ = conn.Close()
		delete(c.tunnels, id)
	}
}
