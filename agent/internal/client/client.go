// Package client 管理 Agent 与服务端的 TCP 长连接、心跳保活与断线重连。
package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"mocenter/agent/internal/command"
	"mocenter/agent/internal/probe"
	"mocenter/agent/internal/protocol"
)

// Config Agent 配置。
type Config struct {
	ServerAddr string // 服务端地址 host:port
	AgentID    string
	Heartbeat  time.Duration // 默认 30s
	ProbeEvery time.Duration // 探针采集周期，默认 10s
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
	// 交互会话映射 commandID -> writer
	interactive map[string]*interactiveSession
}

type interactiveSession struct {
	cmdID  string
	input  chan string
	cancel chan struct{}
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
		collect:     probe.NewCollector(cfg.AgentID),
		hb:          cfg.Heartbeat,
		interactive: make(map[string]*interactiveSession),
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
	conn, err := net.Dial("tcp", c.cfg.ServerAddr)
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
	return c.sc.Err()
}

// register 发送上线消息。
func (c *Client) register() {
	info := map[string]interface{}{
		"agent_id":   c.cfg.AgentID,
		"hostname":   c.collect.Host(),
		"os_type":    c.collect.OSType(),
		"arch":       c.collect.Arch(),
		"remote_addr": "",
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
		if s, ok := c.interactive[inp.CommandID]; ok {
			select {
			case s.input <- inp.InputData:
			case <-s.cancel:
			}
		}
	case protocol.TypeFileDownloadReq:
		// MVP：文件下载暂按提示处理
		var fd protocol.FileDownloadReq
		_ = json.Unmarshal(env.Data, &fd)
	}
}

// runCommand 执行命令并回传结果。
func (c *Client) runCommand(req protocol.CommandReq) {
	if req.Interactive {
		// 启动交互会话
		sess := &interactiveSession{cmdID: req.CommandID, input: make(chan string, 128), cancel: make(chan struct{})}
		c.interactive[req.CommandID] = sess
		defer func() {
			delete(c.interactive, req.CommandID)
			close(sess.cancel)
		}()
		go c.interactiveReadLoop(sess, req.CommandID)
	}

	res := command.Execute(&req)
	_ = c.write(protocol.TypeCommandResp, protocol.CommandResp{
		CommandID: req.CommandID,
		Status:    res.Status,
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Duration:  res.Duration,
	})
}

// interactiveReadLoop 交互模式下读取 stdin 输入。
func (c *Client) interactiveReadLoop(sess *interactiveSession, cmdID string) {
	for {
		select {
		case <-sess.cancel:
			return
		case data := <-sess.input:
			_ = c.write(protocol.TypeInteractiveOut, protocol.InteractiveOutput{
				CommandID: cmdID, Data: data, Type: "input",
			})
		}
	}
}
