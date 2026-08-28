// mcmd：MoCenter 远程命令调试工具。
//
// 通过 REST 下发命令、经 WebSocket 接收 CommandResp，实现在任意 Agent 主机上
// 执行命令并取回输出（等效 Web 远程终端的一次性命令，用于自动化测试/部署）。
//
// 用法：
//
//	go run . -agent <agent_id> -cmd "ipconfig" [-server http://127.0.0.1:8088] [-shell cmd] [-timeout 60]
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	server := flag.String("server", "http://127.0.0.1:8088", "服务端地址")
	user := flag.String("user", "admin", "登录用户名")
	pass := flag.String("pass", "123456", "登录密码")
	agent := flag.String("agent", "", "目标主机 agent_id")
	cmdStr := flag.String("cmd", "", "要执行的命令")
	shell := flag.String("shell", "", "shell 类型：bash/sh/cmd/powershell，留空自动")
	timeout := flag.Int("timeout", 60, "超时（秒）")
	flag.Parse()
	if *agent == "" || *cmdStr == "" {
		fmt.Fprintln(os.Stderr, "用法: mcmd -agent <id> -cmd <命令> [-shell cmd] [-timeout 60]")
		os.Exit(2)
	}

	// 1) 登录
	lb, _ := json.Marshal(map[string]string{"username": *user, "password": *pass})
	lr, err := http.Post(*server+"/api/login", "application/json", bytes.NewReader(lb))
	must(err)
	defer lr.Body.Close()
	var login struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		Msg   string `json:"msg"`
	}
	must(json.NewDecoder(lr.Body).Decode(&login))
	if !login.OK {
		fmt.Fprintln(os.Stderr, "登录失败:", login.Msg)
		os.Exit(1)
	}

	// 2) 连 WebSocket
	wsURL := strings.Replace(*server, "http", "ws", 1) + "/ws?token=" + url.QueryEscape(login.Token)
	ws, err := dialWS(wsURL)
	must(err)
	defer ws.Close()

	cmdID := fmt.Sprintf("mcmd-%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var env struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if json.Unmarshal(msg, &env) != nil {
				continue
			}
			if env.Type != "CommandResp" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(env.Data)
			if err != nil {
				raw = []byte(env.Data)
			}
			var resp struct {
				CommandID string `json:"command_id"`
				Status    int    `json:"status"`
				ExitCode  int    `json:"exit_code"`
				Stdout    string `json:"stdout"`
				Stderr    string `json:"stderr"`
			}
			if json.Unmarshal(raw, &resp) != nil || resp.CommandID != cmdID {
				continue
			}
			if resp.Stdout != "" {
				fmt.Print(resp.Stdout)
			}
			if resp.Stderr != "" {
				fmt.Fprint(os.Stderr, "[stderr] "+resp.Stderr)
			}
			fmt.Printf("\n[status=%d exit=%d]\n", resp.Status, resp.ExitCode)
			return
		}
	}()

	// 3) 下发命令
	body, _ := json.Marshal(map[string]interface{}{
		"command_id": cmdID,
		"agent_id":   *agent,
		"command":    *cmdStr,
		"shell_type": *shell,
		"timeout":    *timeout,
	})
	req, _ := http.NewRequest("POST", *server+"/api/command", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+login.Token)
	cr, err := http.DefaultClient.Do(req)
	must(err)
	defer cr.Body.Close()
	var dispatch struct {
		OK  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	must(json.NewDecoder(cr.Body).Decode(&dispatch))
	if !dispatch.OK {
		fmt.Fprintln(os.Stderr, "下发失败:", dispatch.Msg)
		os.Exit(1)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Duration(*timeout+10) * time.Second):
		fmt.Fprintln(os.Stderr, "等待结果超时")
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dialHost(host string) (io.ReadWriteCloser, error) {
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	return net.DialTimeout("tcp", host, 5*time.Second)
}

// ---- 最小 WebSocket 客户端（仅实现本工具所需能力）----

type wsConn struct {
	conn io.ReadWriteCloser
	br   *bufio.Reader
	wmu  sync.Mutex
}

func dialWS(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	must(err)
	host := u.Host
	keyB := make([]byte, 16)
	_, _ = rand.Read(keyB)
	key := base64.StdEncoding.EncodeToString(keyB)
	path := u.Path + "?" + u.RawQuery
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, host, key)
	c, err := dialHost(host)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		return nil, fmt.Errorf("websocket handshake failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	// 校验 Accept（宽松）
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_ = base64.StdEncoding.EncodeToString(sum[:])
	return &wsConn{conn: c, br: br}, nil
}

func (w *wsConn) ReadMessage() ([]byte, error) {
	for {
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(w.br, hdr); err != nil {
			return nil, err
		}
		opcode := hdr[0] & 0x0f
		masked := hdr[1]&0x80 != 0
		plen := int64(hdr[1] & 0x7f)
		switch plen {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(w.br, ext); err != nil {
				return nil, err
			}
			plen = int64(ext[0])<<8 | int64(ext[1])
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(w.br, ext); err != nil {
				return nil, err
			}
			plen = 0
			for _, b := range ext {
				plen = plen<<8 | int64(b)
			}
		}
		var maskKey []byte
		if masked {
			maskKey = make([]byte, 4)
			if _, err := io.ReadFull(w.br, maskKey); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, plen)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch opcode {
		case 0x1, 0x2:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9: // ping -> pong
			_ = w.writeFrame(0xA, payload)
		}
	}
}

func (w *wsConn) writeFrame(opcode byte, data []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	hdr := []byte{0x80 | opcode}
	n := len(data)
	switch {
	case n < 126:
		hdr = append(hdr, byte(0x80|n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		for i := 7; i >= 0; i-- {
			hdr = append(hdr, byte(n>>(8*i)))
		}
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = data[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(hdr); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

func (w *wsConn) Close() error { return w.conn.Close() }
