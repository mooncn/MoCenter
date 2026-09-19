//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// hiddenWindowAttr 让子进程不创建控制台窗口。
//
// 计划任务以 SYSTEM 运行 mocenter-agent.exe 这类控制台程序时，若不带
// CREATE_NO_WINDOW，会有黑窗口在某些会话里闪出来。用户看到"有东西在跑"
// 但不知道是什么，反而更容易把进程当成可疑程序手工结束。
func hiddenWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
