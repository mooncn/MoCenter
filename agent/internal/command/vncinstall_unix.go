//go:build linux

// Linux VNC 一键安装/启动：
//   1. 优先使用系统包管理器（apt/yum/dnf）安装 x11vnc；
//   2. 否则通过 curl 下载静态 x11vnc 二进制（支持国内加速代理前缀）。
package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// x11vncBinURL 默认 x11vnc 二进制下载地址（可用 DownloadBase 覆盖）。
const x11vncBinURL = "https://github.com/LibVNC/x11vnc/releases/download/0.9.16/x11vnc-0.9.16-x86_64"

// vncInstallLinux 安装并启动 x11vnc，并写入随机口令、仅允许回环访问。
func vncInstallLinux(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port}

	// 生成口令并写入口令文件（自行写文件，免依赖 vncpasswd）
	pw := generateRandomPassword()
	res.Password = pw
	passFile := filepath.Join(homeDir(), ".mocenter", "vncpasswd")
	if err := writeVncPasswdFile(passFile, pw); err != nil {
		res.Message = "创建 VNC 口令文件失败: " + err.Error()
		return res
	}

	// 使用 -rfbauth（口令）与 -localhost（仅回环），配合隧道回连；先结束旧实例以生效新口令
	start := `pkill -f '[x]11vnc' 2>/dev/null; sleep 1; DISPLAY="${DISPLAY:-:0}" x11vnc -forever -shared -noxdamage -rfbauth '` +
		passFile + `' -localhost -rfbport ` + itoa(port) + ` </dev/null >/tmp/mocenter_x11vnc.log 2>&1 &`

	// 已安装则直接启动
	if hasBin("x11vnc") {
		_ = exec.Command("bash", "-c", start).Run()
		res.Message = "已启动 x11vnc（口令见本次下发值，仅回环；端口 " + itoa(port) + "）。若提示无图形会话，请确保存在 DISPLAY。"
		res.OK = portOpen(port)
		return res
	}

	// 尝试包管理器安装
	if installByPkgMgr(start) {
		res.Message = "VNC(x11vnc) 已安装并尝试启动（已启用口令，仅回环），端口 " + itoa(port) + "。"
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

	// 选择下载器并以慢速检测包装，速率过低时切换备选源
	var errm string
	if err := downloadSlowAware(dst, minVNCBps, func(ctx context.Context) error {
		if hasBin("curl") {
			return exec.CommandContext(ctx, "curl", "-fSL", "--max-time", "90", "-o", dst, url).Run()
		}
		return exec.CommandContext(ctx, "wget", "-q", "-O", dst, url).Run()
	}); err != nil {
		errm = err.Error()
	} else {
		_ = exec.Command("bash", "-c", "chmod +x "+dst).Run()
		envStart := "export PATH=\"$HOME/.local/bin:$PATH\"\n" + start
		_ = exec.Command("bash", "-c", envStart).Run()
		return "已通过 curl 下载并尝试启动 x11vnc（端口 " + itoa(port) + "）。", true
	}
	return fmt.Sprintf("x11vnc 自动安装失败：下载失败（%v）。可手动安装：%s", errm, url), false
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}
