// 交互式终端会话。
//
// Linux/macOS 基于 creack/pty 创建真实伪终端，支持尺寸自适应、实时双向 IO
// 与进程退出通知；Windows 及其他平台以管道桥接 shell，并对输入做换行规范化。
package command

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"mocenter/agent/internal/protocol"
)

// InteractiveSession 表示一个正在运行的交互式会话。
type InteractiveSession struct {
	cmdID string

	mu         sync.Mutex
	ptmx       *os.File // PTY 主端（linux/macOS）
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	in         io.Writer // 实际输入句柄（含平台编码转换）
	xlateInput bool      // 管道模式：需把 CR 规范为 CRLF、DEL 规范为 BS
	closed     bool
	onOutput   func(*protocol.InteractiveOutput)
	stop       chan struct{}
	done       chan struct{}
}

// StartInteractive 启动一个交互式命令，onOutput 用于回传输出。
func StartInteractive(req *protocol.CommandReq, onOutput func(*protocol.InteractiveOutput)) (*InteractiveSession, error) {
	s := &InteractiveSession{
		cmdID:    req.CommandID,
		onOutput: onOutput,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	name, args := buildCmd(req)
	cmd := exec.Command(name, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	// PTY 终端类型：让 bash/ls 等输出彩色与正确光标控制
	if runtime.GOOS != "windows" {
		hasTerm := false
		for _, e := range env {
			if strings.HasPrefix(e, "TERM=") {
				hasTerm = true
				break
			}
		}
		if !hasTerm {
			env = append(env, "TERM=xterm-256color")
		}
	}
	cmd.Env = env
	s.cmd = cmd

	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		ptmx, err := startPTY(cmd, req.Rows, req.Cols)
		if err != nil {
			return nil, err
		}
		s.ptmx = ptmx
		s.in = ptmx
		go s.readLoop(ptmx)
		go s.waitLoop()
	} else if runtime.GOOS == "windows" {
		// Windows：以管道桥接交互 shell（cmd/powershell）。
		// 不用 ConPTY 的原因：实测目标机（Win10 19044，用户会话与服务会话均试过）
		// CreatePseudoConsole 返回 S_OK 却从不向输出管道写入字节，一旦选中 ConPTY
		// 终端就完全无输出；管道读写则稳定可用地支撑连续交互（多命令、退出码、回显）。
		// 代价：不提供全屏 TUI 与逐字符行编辑，故开启 xlateInput 规范换行。
		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		s.stdin = stdinPipe
		s.in = stdinPipe
		s.xlateInput = true
		go s.readLoop(stdoutPipe)
		go s.readLoop(stderrPipe)
		go s.waitLoop()
	} else {
		// 其他平台退化：使用管道
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		s.stdin = stdin
		s.in = stdin
		go s.readLoop(stdout)
		go s.waitLoop()
	}

	return s, nil
}

// Write 向交互进程写入输入。
func (s *InteractiveSession) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.EOF
	}
	if s.xlateInput {
		data = normalizeInput(data)
	}
	if s.in != nil {
		_, err := s.in.Write(data)
		return err
	}
	return io.EOF
}

// normalizeInput 规范化送进管道模式 shell 的控制字符：
// xterm 回车发的是 CR(\r)，而管道模式的 cmd/powershell 以 CRLF 作为行结束符，
// 只收到 CR 时会一直等待，表现为「输入了命令但毫无反应」；退格键同理需转成 BS。
func normalizeInput(data []byte) []byte {
	s := string(data)
	// 先统一成 LF，再统一展开为 CRLF，避免把已有的 CRLF 变成 CR CR LF
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	s = strings.ReplaceAll(s, "\x7f", "\b")
	return []byte(s)
}

// Resize 调整 PTY 终端尺寸。
func (s *InteractiveSession) Resize(rows, cols int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rows <= 0 || cols <= 0 {
		return nil
	}
	if s.ptmx == nil {
		return nil
	}
	return resizePTY(s.ptmx, rows, cols)
}

// SendControl 发送特殊控制码（如 Ctrl-C）。
func (s *InteractiveSession) SendControl(control string) error {
	switch control {
	case "ctrl_c":
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
			return nil
		}
		return s.Write([]byte{0x03})
	default:
		return nil
	}
}

// Done 返回进程结束通知通道。
func (s *InteractiveSession) Done() <-chan struct{} { return s.done }

// Close 终止会话。
func (s *InteractiveSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	close(s.stop)
	s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// readLoop 读取一路输出（PTY 主端或管道）并规范化换行后回传。
func (s *InteractiveSession) readLoop(pr io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			s.emit(&protocol.InteractiveOutput{
				CommandID: s.cmdID,
				Data:      normalizePTYOutput(string(buf[:n])),
				Type:      "stdout",
			})
		}
		if err != nil {
			return
		}
	}
}

// waitLoop 等待进程退出并发送退出事件。
func (s *InteractiveSession) waitLoop() {
	defer close(s.done)
	code := 0
	if s.cmd != nil {
		err := s.cmd.Wait()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
	}
	s.emit(&protocol.InteractiveOutput{
		CommandID: s.cmdID,
		Type:      "exit",
		ExitCode:  code,
	})
}

// emit 安全回传输出（线程安全，会话关闭后丢弃）。
func (s *InteractiveSession) emit(out *protocol.InteractiveOutput) {
	select {
	case <-s.stop:
		return
	default:
	}
	if s.onOutput != nil {
		s.onOutput(out)
	}
}

// normalizePTYOutput 规范化 PTY 输出中的换行符：
// 将独立的 LF(\n) 转换为 CRLF(\r\n)，确保 Web 端 xterm 正确回到行首，
// 避免"每行比上一行前留空格更多"的递进缩进问题。
// 已存在的 CRLF 保持不变。
func normalizePTYOutput(data string) string {
	// 快速路径：不含 \n 则直接返回
	if !strings.Contains(data, "\n") {
		return data
	}
	// 将独立的 \n（前面不是 \r 的）替换为 \r\n
	var sb strings.Builder
	sb.Grow(len(data) + 8)
	prev := byte(0)
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == '\n' && prev != '\r' {
			sb.WriteByte('\r')
		}
		sb.WriteByte(c)
		prev = c
	}
	return sb.String()
}
