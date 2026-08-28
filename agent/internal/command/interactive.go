// 交互式 PTY 终端执行。
//
// 基于 creack/pty 实现 Linux/macOS 上的真实伪终端，支持终端尺寸自适应、
// 实时双向 IO 与进程退出通知。Windows 使用 ConPTY（通过 winpty/内置退化），
// 当前版本在非 Linux/macOS 平台退化为管道桥接。
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

	mu       sync.Mutex
	ptmx     *os.File // PTY 主端（linux/macOS）
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	in       io.Writer // 实际输入句柄（含平台编码转换）
	closed   bool
	onOutput func(*protocol.InteractiveOutput)
	stop     chan struct{}
	done     chan struct{}

	// Windows ConPTY 会话（非零时优先于普通管道）
	conptyResize func(int, int) error
	conptyClose  func()
	conptyDone   <-chan struct{}
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
		// Windows：优先使用真伪终端 ConPTY，失败则退化为普通管道
		if cs, cerr := startConPTYSession(name, args, req.Rows, req.Cols); cerr == nil {
			s.conptyResize = cs.Resize
			s.conptyClose = cs.Close
			s.conptyDone = cs.Done()
			s.in = cs.Stdin()
			go s.readPipeLoop(cs.Stdout())
			go s.waitConPTY()
		} else {
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
			go s.readPipeLoop(stdout)
			go s.waitLoop()
		}
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
		go s.readPipeLoop(stdout)
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
	if s.in != nil {
		_, err := s.in.Write(data)
		return err
	}
	if s.ptmx != nil {
		_, err := s.ptmx.Write(data)
		return err
	}
	if s.stdin != nil {
		_, err := s.stdin.Write(data)
		return err
	}
	return io.EOF
}

// Resize 调整 PTY 终端尺寸。
func (s *InteractiveSession) Resize(rows, cols int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rows <= 0 || cols <= 0 {
		return nil
	}
	if s.conptyResize != nil {
		return s.conptyResize(rows, cols)
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
	if s.conptyClose != nil {
		s.conptyClose()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// waitConPTY 等待 ConPTY 子进程退出并发送退出事件。
func (s *InteractiveSession) waitConPTY() {
	<-s.conptyDone
	s.emit(&protocol.InteractiveOutput{
		CommandID: s.cmdID,
		Type:      "exit",
		ExitCode:  0,
	})
	close(s.done)
}

// readLoop 从 PTY 主端读取输出并回传。
func (s *InteractiveSession) readLoop(pr io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			s.emit(&protocol.InteractiveOutput{
				CommandID: s.cmdID,
				Data:      string(buf[:n]),
				Type:      "stdout",
			})
		}
		if err != nil {
			return
		}
	}
}

// readPipeLoop Windows 退化路径：读取管道输出。
func (s *InteractiveSession) readPipeLoop(pr io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			s.emit(&protocol.InteractiveOutput{
				CommandID: s.cmdID,
				Data:      string(buf[:n]),
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
