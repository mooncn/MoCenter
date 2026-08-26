//go:build linux

// Linux VNC 一键安装/启动：
//   1. 优先使用系统包管理器（apt/yum/dnf）安装 x11vnc；
//   2. 否则通过 curl 下载静态 x11vnc 二进制（支持国内加速代理前缀）。
package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// x11vncBinURL 默认 x11vnc 二进制下载地址（可用 DownloadBase 覆盖）。
const x11vncBinURL = "https://github.com/LibVNC/x11vnc/releases/download/0.9.16/x11vnc-0.9.16-x86_64"

// vncInstallLinux 安装并启动 x11vnc。
func vncInstallLinux(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port}

	start := `DISPLAY="${DISPLAY:-:0}" x11vnc -forever -shared -noxdamage -nopw -rfbport ` +
		itoa(port) + ` </dev/null >/tmp/mocenter_x11vnc.log 2>&1 &`

	// 已安装则直接启动
	if hasBin("x11vnc") {
		_ = exec.Command("bash", "-c", start).Run()
		res.Message = "已启动 x11vnc（无密码，端口 " + itoa(port) + "）。若提示无图形会话，请确保主机存在 DISPLAY。"
		res.OK = portOpen(port)
		return res
	}

	// 尝试包管理器安装
	if installByPkgMgr(start) {
		res.Message = "VNC(x11vnc) 已安装并尝试启动，端口 " + itoa(port) + "。"
		if portOpen(port) {
			res.OK = true
		} else {
			res.Message += "（端口暂未就绪，可能需图形会话 X/桌面）"
		}
		return res
	}

	// 兜底：curl 下载静态 x11vnc 二进制
	if msg, ok := installByCurl(cfg, start, port); ok {
		res.Message = msg
		if portOpen(port) {
			res.OK = true
		} else {
			res.Message += "（端口暂未就绪，可能需图形会话 X/桌面）"
		}
		return res
	} else {
		res.Message = msg
	}
	return res
}

func hasBin(name string) bool {
	return exec.Command("bash", "-c", "command -v "+name+" >/dev/null 2>&1").Run() == nil
}

// installByPkgMgr 通过系统包管理器安装 x11vnc。
func installByPkgMgr(start string) bool {
	script := `
if ! command -v x11vnc >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    (sudo apt-get update -qq && sudo apt-get install -y -qq x11vnc) || (apt-get update -qq && apt-get install -y -qq x11vnc)
  elif command -v yum >/dev/null 2>&1; then
    (sudo yum install -y x11vnc) || yum install -y x11vnc
  elif command -v dnf >/dev/null 2>&1; then
    (sudo dnf install -y x11vnc) || (dnf install -y -q x11vnc)
  fi
fi
if command -v x11vnc >/dev/null 2>&1; then
  ` + start + `
  echo "OK"
fi`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	_ = out
	return err == nil && hasBin("x11vnc")
}

// installByCurl 通过 curl 下载 x11vnc 静态二进制到用户目录并尝试启动。
func installByCurl(cfg VncConfig, start string, port int) (string, bool) {
	if !hasBin("curl") && !hasBin("wget") {
		return "未找到 x11vnc 且无法自动安装（无 curl/wget）。请手动安装（如 sudo apt-get install -y x11vnc）。", false
	}
	url := cfg.DownloadBase
	if url == "" {
		url = x11vncBinURL
		// 按架构选择下载包
		switch runtime.GOARCH {
		case "arm64", "aarch64":
			url = "https://github.com/LibVNC/x11vnc/releases/download/0.9.16/x11vnc-0.9.16-aarch64"
		case "arm":
			url = "https://github.com/LibVNC/x11vnc/releases/download/0.9.16/x11vnc-0.9.16-armv7l"
		}
	}
	if cfg.Proxy != "" {
		url = proxyURL(cfg.Proxy, url)
	}
	dst := filepath.Join(homeDir(), ".local", "bin", "x11vnc")
	_ = exec.Command("bash", "-c", "mkdir -p "+filepath.Dir(dst)).Run()

	var cmd *exec.Cmd
	if hasBin("curl") {
		cmd = exec.Command("curl", "-fSL", "-o", dst, url)
	} else {
		cmd = exec.Command("wget", "-q", "-O", dst, url)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("x11vnc 自动安装失败：下载失败（%v）。可手动安装：%s\n%s", err, url, strings.TrimSpace(string(out))), false
	}
	_ = exec.Command("bash", "-c", "chmod +x "+dst).Run()
	// 将 x11vnc 加入 PATH 并启动
	envStart := "export PATH=\"$HOME/.local/bin:$PATH\"\n" + start
	_ = exec.Command("bash", "-c", envStart).Run()
	return "已通过 curl 下载并尝试启动 x11vnc（端口 " + itoa(port) + "）。", true
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}
