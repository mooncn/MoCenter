//go:build windows

package winapp

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// IsWindowsService 报告当前进程是否由 Windows 服务控制管理器（SCM）拉起。
//
// 有了它，同一个 exe 既能双击运行（控制台模式），也能被注册为系统服务 ——
// 无需额外加 -service 之类的开关，也就不会出现"忘了加参数导致服务起不来"。
func IsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// serviceHandler 把 SCM 的状态回调适配成 start/stop 两个钩子。
type serviceHandler struct {
	start func()
	stop  func()
}

// Execute 由 x/sys/windows/svc 在服务线程上调用。
// 关键：必须及时上报 StartPending/Running/StopPending，否则 SCM 会判定服务无响应（1053）。
func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}
	go h.start() // 主循环在独立 goroutine 中运行，本函数必须尽快回到等待状态
	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for req := range r {
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			h.stop()
			return false, 0
		}
	}
	return false, 0
}

// RunAsService 以 Windows 服务方式运行：start 在服务启动时被调用，
// stop 在收到停止/关机请求时被调用（通常直接 os.Exit(0)）。
// 仅在 IsWindowsService() 为 true 时调用。
func RunAsService(name string, start, stop func()) error {
	return svc.Run(name, &serviceHandler{start: start, stop: stop})
}

// procCreateMutexW 直接取 kernel32!CreateMutexW。
//
// 为什么不用 x/sys 的封装：它在调用成功时会丢弃 last error，
// 而"互斥体已存在"恰恰是靠 ERROR_ALREADY_EXISTS 判断的（此时句柄仍能拿到，调用算成功）。
var procCreateMutexW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")

// AcquireSingleton 用命名互斥体保证「同一 key 在本机只允许一个实例」。
//
// 返回 (release, ok)：ok 为 false 表示已有实例在运行；release 用于提前释放，
// 进程退出（含崩溃）时内核会自动释放，不会留下陈旧锁。
//
// 命名空间优先 Global\（跨会话，服务与用户态都能看到）；非管理员无 Global 权限时
// 回退 Local\。两者都建不出来时不阻塞启动 —— 宁可多跑一个，也不要因此起不来。
func AcquireSingleton(key string) (func(), bool) {
	for _, prefix := range []string{`Global\`, `Local\`} {
		name, err := windows.UTF16PtrFromString(prefix + key)
		if err != nil {
			continue
		}
		r1, _, e1 := syscall.SyscallN(procCreateMutexW.Addr(), 0, 0, uintptr(unsafe.Pointer(name)))
		h := windows.Handle(r1)
		if h == 0 {
			continue // 该命名空间不可用，换下一个
		}
		if e1 == windows.ERROR_ALREADY_EXISTS {
			_ = windows.CloseHandle(h)
			return nil, false
		}
		return func() { _ = windows.CloseHandle(h) }, true
	}
	return func() {}, true
}
