// Package logx 提供带级别的运行期日志（off / error / warn / info）。
//
// 设计取舍：本包在 server 与 agent 两个模块中**各存一份**（二者是独立 module），
// 与本仓库既有的 protocol.go / flags.go / winapp 双份存放惯例一致。
//
// 为什么仍然走标准库 log：Windows 服务模式靠 winapp.RedirectLogToFile 调
// log.SetOutput 把输出接到文件，日志器只要写同一个 logger 就自动跟随，
// 不必再维护第二套输出重定向。
package logx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// Level 日志级别。数值越大越详细：off < error < warn < info。
type Level int

const (
	// Off 关闭运行期日志。进程仍会写致命错误（见 Fatalf），否则服务起不来时零线索。
	Off Level = iota
	Error
	Warn
	Info
)

var (
	mu    sync.RWMutex
	level = Error // 默认 error：Agent 部署量大，默认不产生日常噪声
	// exitFn 便于单测验证致命路径，正式运行就是 os.Exit。
	exitFn = os.Exit
)

// ParseLevel 解析配置/命令行里的级别字符串。空串按 error 处理。
// 返回值 ok=false 表示无法识别，调用方应回落默认并告警。
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "error":
		return Error, true
	case "off", "none", "disable", "disabled":
		return Off, true
	case "warn", "warning":
		return Warn, true
	case "info", "debug":
		// debug 归到 info：本项目的详细日志都打在 info，不单设一级。
		return Info, true
	}
	return Error, false
}

func (l Level) String() string {
	switch l {
	case Off:
		return "off"
	case Warn:
		return "warn"
	case Info:
		return "info"
	default:
		return "error"
	}
}

// SetLevel 设置当前级别（并发安全）。
func SetLevel(l Level) {
	mu.Lock()
	level = l
	mu.Unlock()
}

// Current 返回当前级别。
func Current() Level {
	mu.RLock()
	defer mu.RUnlock()
	return level
}

// enabled 判断某条日志是否应输出。
func enabled(l Level) bool {
	cur := Current()
	return cur != Off && l <= cur
}

func emit(tag, format string, args ...interface{}) {
	log.Printf("[%s] %s", tag, fmt.Sprintf(format, args...))
}

// Infof 生命周期与事实（启动、监听地址、注册上线、升级成功等）。
func Infof(format string, args ...interface{}) {
	if enabled(Info) {
		emit("INFO", format, args...)
	}
}

// Warnf 可恢复/降级的情况（重连重试、回退兜底、尽力而为的步骤失败等）。
func Warnf(format string, args ...interface{}) {
	if enabled(Warn) {
		emit("WARN", format, args...)
	}
}

// Errorf 单次操作失败但进程仍可继续。
func Errorf(format string, args ...interface{}) {
	if enabled(Error) {
		emit("ERROR", format, args...)
	}
}

// Criticalf 安全不变量被破坏或可能丢数据。与 Error 同属"error 级别即输出"，
// 单独一级是为了让运维能在日志里一眼筛出需要立即处理的事件。
func Criticalf(format string, args ...interface{}) {
	if enabled(Error) {
		emit("CRITICAL", format, args...)
	}
}

// Fatalf 启动期致命错误：**不受级别影响**，永远输出后以退出码 1 结束进程。
//
// 为什么 off 也要输出：这类事件每个进程最多发生一次，是"服务起不来"的唯一线索；
// 真把它也静默掉，排障就只剩"服务没起来"这一句。运行期日志（Info/Warn/Error/
// Critical）在 off 下全部关闭。
func Fatalf(format string, args ...interface{}) {
	emit("CRITICAL", format, args...)
	exitFn(1)
}
