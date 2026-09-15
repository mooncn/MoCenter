// 交互式终端会话。
//
// Linux/macOS 基于 creack/pty 创建真实伪终端，支持尺寸自适应、实时双向 IO
// 与进程退出通知；Windows 及其他平台以管道桥接 shell，并对输入做换行规范化。
package command

import (
	"bytes"
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
	localEcho  bool      // 管道模式且 shell 自己不回显：由本端代做实时回显
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

	name, args, localEcho := buildCmd(req)
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
		// cmd.exe 在管道模式下不会实时回显（只在读到整行后才滞后打印一次，
		// 且已被 /Q 关掉），因此由本端代做回显，使敲键即时可见。
		// powershell / PTY 场景 localEcho 为 false —— 它们自己就会逐字符回显。
		s.localEcho = localEcho
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
	if s.closed {
		s.mu.Unlock()
		return io.EOF
	}
	if s.xlateInput {
		data = normalizeInput(data)
	}
	// 回显内容在锁内算好，emit 放到锁外：onOutput 可能写网络，阻塞时不应连带
	// 卡住 Close()。
	var echo string
	if s.localEcho {
		echo = echoForPipe(data)
	}
	var err error
	if s.in != nil {
		_, err = s.in.Write(data)
	} else {
		err = io.EOF
	}
	s.mu.Unlock()

	if err == nil && echo != "" {
		s.emit(&protocol.InteractiveOutput{
			CommandID: s.cmdID,
			Data:      normalizePTYOutput(echo),
			Type:      "stdout",
		})
	}
	return err
}

// echoForPipe 生成管道模式下需由本端代做的回显内容。
//
// 背景：Windows 用普通管道接 shell（见 StartInteractive 说明），管道不是控制台，
// cmd.exe 也就没有逐字符回显 —— 用户敲进去的字要等到回车、cmd 把整行打印出来才
// 看得见。这里按「敲什么显示什么」补上，使 Windows 与 Linux(PTY) 体验一致。
//
// 只回显可见字符与两个行编辑键，其余控制键一律不代劳：
//
//	CR / LF      -> 换行
//	BS / DEL     -> 退格（"\b \b" 把上一格擦掉）
//	ESC 起头的串  -> 方向键/功能键等，本身不含可见字符，整段跳过
//	其他控制字符  -> 不显示（交由 shell 自身的输出表达，如 ^C）
//
// 多字节 UTF-8（中文等）所有字节都 >= 0x20，原样透传。
func echoForPipe(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	// xterm 每次按键单独回调，方向键等会整段以 ESC 开头；直接判整段跳过即可。
	if bytes.IndexByte(data, 0x1b) >= 0 {
		return ""
	}
	var sb strings.Builder
	sb.Grow(len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '\r':
			sb.WriteString("\r\n")
			if i+1 < len(data) && data[i+1] == '\n' {
				i++ // CRLF 只回显成一个换行
			}
		case c == '\n':
			sb.WriteString("\r\n")
		case c == '\b' || c == 0x7f:
			sb.WriteString("\b \b")
		case c >= 0x20:
			sb.WriteByte(c)
		default:
			// 其他控制字符不代劳
		}
	}
	return sb.String()
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
