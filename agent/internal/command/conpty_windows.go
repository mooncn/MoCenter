//go:build windows

// ConPTY（Console Pseudo-Terminal）实现。
//
// 通过 Go 标准库 syscall.NewLazyDLL 直接调用 kernel32 的伪终端 API
// （CreatePseudoConsole / ResizePseudoConsole / ClosePseudoConsole），
// 并以 PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE 附加到子进程，
// 为子进程提供真实的 PTY 控制台，支持交互式命令（ssh/vim/目录切换等）。
//
// 不引入任何第三方依赖。正式支持 Windows 10 1809 / Server 2019 及以上。
package command

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreatePseudoConsole  = kernel32.NewProc("CreatePseudoConsole")
	procClosePseudoConsole   = kernel32.NewProc("ClosePseudoConsole")
	procResizePseudoConsole  = kernel32.NewProc("ResizePseudoConsole")
	procInitThrAttrList      = kernel32.NewProc("InitializeProcThreadAttributeList")
	procDelThrAttrList       = kernel32.NewProc("DeleteProcThreadAttributeList")
	procUpdThrAttr           = kernel32.NewProc("UpdateProcThreadAttribute")
	procCreateProcessW       = kernel32.NewProc("CreateProcessW")
	procCloseHandle          = kernel32.NewProc("CloseHandle")
	procWaitForSingleObject  = kernel32.NewProc("WaitForSingleObject")
	procTerminateProcess     = kernel32.NewProc("TerminateProcess")
)

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupInfoPresent       = 0x00080000
	infinite                         = 0xFFFFFFFF
)

type coordWindows struct{ X, Y int16 }

type startupInfoEx struct {
	sb              syscall.StartupInfo
	procThreadAttr uintptr
}

type processInformation struct {
	Process, Thread syscall.Handle
	ProcessID       uint32
	ThreadID        uint32
}

// conptySession 表示一个已启动的 ConPTY 会话。
type conptySession struct {
	pc       uintptr
	inR, inW *os.File
	outR, outW *os.File
	attrList *byte
	proc     syscall.Handle

	doneCh chan struct{}
	once   sync.Once
}

// Stdin 返回向伪终端写入用户输入的句柄。
func (s *conptySession) Stdin() *os.File { return s.inW }

// Stdout 返回读取子进程输出的句柄。
func (s *conptySession) Stdout() *os.File { return s.outR }

// Done 在子进程退出后关闭。
func (s *conptySession) Done() <-chan struct{} { return s.doneCh }

// Resize 调整伪终端尺寸。
func (s *conptySession) Resize(rows, cols int) error {
	cr := coordWindows{X: int16(cols), Y: int16(rows)}
	rr, _, e := procResizePseudoConsole.Call(s.pc, *(*uintptr)(unsafe.Pointer(&cr)))
	if rr == 0 {
		return fmt.Errorf("ResizePseudoConsole failed: %v", e)
	}
	return nil
}

// Close 清理 ConPTY 会话资源并终止子进程。
func (s *conptySession) Close() {
	if s.proc != 0 {
		procTerminateProcess.Call(uintptr(s.proc), 1)
		procWaitForSingleObject.Call(uintptr(s.proc), 2000) // 等待终止
		procCloseHandle.Call(uintptr(s.proc))
		s.proc = 0
	}
	if s.pc != 0 {
		procClosePseudoConsole.Call(s.pc)
		s.pc = 0
	}
	if s.attrList != nil {
		procDelThrAttrList.Call(uintptr(unsafe.Pointer(s.attrList)))
		s.attrList = nil
	}
	for _, f := range []*os.File{s.inR, s.inW, s.outR, s.outW} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// startConPTYSession 启动一个 ConPTY 会话。
func startConPTYSession(name string, args []string, rows, cols int) (*conptySession, error) {
	s := &conptySession{doneCh: make(chan struct{})}
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	s.inR, s.inW, s.outR, s.outW = inR, inW, outR, outW

	var pc uintptr
	coord := coordWindows{X: int16(cols), Y: int16(rows)}
	if r1, _, e1 := procCreatePseudoConsole.Call(*(*uintptr)(unsafe.Pointer(&coord)), inR.Fd(), outW.Fd(), 0, uintptr(unsafe.Pointer(&pc))); r1 == 0 {
		s.Close()
		return nil, fmt.Errorf("CreatePseudoConsole failed: %v", e1)
	}
	s.pc = pc

	// 初始化线程属性列表
	var sz uintptr
	procInitThrAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&sz)))
	attrBuf := make([]byte, sz)
	if r1, _, e1 := procInitThrAttrList.Call(uintptr(unsafe.Pointer(&attrBuf[0])), 1, 0, uintptr(unsafe.Pointer(&sz))); r1 == 0 {
		s.Close()
		return nil, fmt.Errorf("InitializeProcThreadAttributeList failed: %v", e1)
	}
	s.attrList = &attrBuf[0]
	if r1, _, e1 := procUpdThrAttr.Call(uintptr(unsafe.Pointer(&attrBuf[0])), 0, procThreadAttributePseudoConsole, uintptr(unsafe.Pointer(&pc)), unsafe.Sizeof(pc), 0, 0); r1 == 0 {
		s.Close()
		return nil, fmt.Errorf("UpdateProcThreadAttribute failed: %v", e1)
	}

	cmdLine := winCommandLine(name, args)
	cmdPtr, _ := syscall.UTF16PtrFromString(cmdLine)
	si := &startupInfoEx{}
	si.sb.Cb = uint32(unsafe.Sizeof(*si))
	si.procThreadAttr = uintptr(unsafe.Pointer(&attrBuf[0]))
	si.sb.Flags = 0
	pi := &processInformation{}
	if r1, _, e1 := procCreateProcessW.Call(
		0, uintptr(unsafe.Pointer(cmdPtr)),
		0, 0,
		1, // bInheritHandles = TRUE
		extendedStartupInfoPresent,
		0, 0,
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(pi))); r1 == 0 {
		s.Close()
		return nil, fmt.Errorf("CreateProcess failed: %v", e1)
	}
	procCloseHandle.Call(uintptr(pi.Thread))
	s.proc = pi.Process

	// 监听子进程退出，关闭 Done 通道
	go func() {
		procWaitForSingleObject.Call(uintptr(pi.Process), infinite)
		s.once.Do(func() { close(s.doneCh) })
	}()
	return s, nil
}

// startConPTY 兼容确切接口的简易封装（供冒烟测试使用）。
func startConPTY(name string, args []string, rows, cols int) (stdin *os.File, stdout *os.File, resize func(int, int) error, cleanup func(), err error) {
	s, err := startConPTYSession(name, args, rows, cols)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return s.Stdin(), s.Stdout(), s.Resize, s.Close, nil
}

// winCommandLine 将程序名与参数拼装为命令行字符串。
func winCommandLine(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, a := range args {
		if strings.ContainsAny(a, " \t") {
			parts = append(parts, `"`+a+`"`)
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}