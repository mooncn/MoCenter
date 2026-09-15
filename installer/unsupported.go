//go:build !windows

// 非 Windows 平台的占位入口：安装器只服务于 Windows 服务形态，
// 这里保留一个 main 以免该模块在非 Windows 上"没有 Go 文件"而构建失败。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mocenter-setup 仅支持 Windows；Linux 请使用 systemd（见 docs/deployment.md）")
	os.Exit(1)
}
