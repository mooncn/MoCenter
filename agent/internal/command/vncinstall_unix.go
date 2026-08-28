//go:build linux

// Linux VNC 一键安装/启动：
//   - 有图形会话（:0 存在）：直接用 x11vnc 挂接当前桌面；
//   - 无桌面（headless/极简安装）：安装并启动 Xvfb 虚拟显示 + openbox + xterm，
//     再由 x11vnc 挂接该虚拟显示，实现可用的远程桌面。
//   始终：随机口令 + 仅回环监听（配合 Agent 隧道回连）。
package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// x11vncBinURL 默认 x11vnc 二进制下载地址（可用 DownloadBase 覆盖）。
const x11vncBinURL = "https://github.com/LibVNC/x11vnc/releases/download/0.9.16/x11vnc-0.9.16-x86_64"

// vncScriptPath Linux VNC 启动脚本路径（幂等：重复执行不会重复拉起）。
func vncScriptPath() string { return filepath.Join(homeDir(), ".mocenter", "vnc-start.sh") }

// vncInstallLinux 安装并启动 VNC（x11vnc），写入随机口令、仅回环访问。
func vncInstallLinux(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port}

	// 1) 口令文件（8 字节 DES 混淆，x11vnc -rfbauth 直接读取）
	pw := generateRandomPassword()
	res.Password = pw
	passFile := filepath.Join(homeDir(), ".mocenter", "vncpasswd")
	if err := writeVncPasswdFile(passFile, pw); err != nil {
		res.Message = "创建 VNC 口令文件失败: " + err.Error()
		return res
	}

	// 2) 确保依赖存在（x11vnc 必须；headless 场景还需 Xvfb/WM/终端）
	missing := linuxVncMissingDeps()
	if len(missing) > 0 {
		if msg, ok := installLinuxPkgs(missing); !ok {
			res.Message = msg
			return res
		}
	}
	if !hasBin("x11vnc") {
		if msg, ok := installByCurl(cfg); !ok {
			res.Message = msg
			return res
		}
	}

	// 3) 写启动脚本并后台执行（setsid 脱离 Agent 命令会话，防止被连带结束）
	script := buildVncStartScript(port, passFile)
	if err := os.MkdirAll(filepath.Dir(vncScriptPath()), 0755); err != nil {
		res.Message = "创建脚本目录失败: " + err.Error()
		return res
	}
	if err := os.WriteFile(vncScriptPath(), []byte(script), 0755); err != nil {
		res.Message = "写入启动脚本失败: " + err.Error()
		return res
	}
	logFile := filepath.Join(homeDir(), ".mocenter", "vnc.log")
	_ = exec.Command("bash", "-c",
		fmt.Sprintf("setsid nohup bash %s >%s 2>&1 &", vncScriptPath(), logFile)).Run()

	// 4) 等端口就绪
	if waitPortOpen(port, 15*time.Second) {
		res.OK = true
		res.Message = "VNC(x11vnc) 已就绪，端口 " + itoa(port) + "。"
	} else {
		res.Message = "VNC 启动后端口未就绪，详见 " + logFile
	}
	return res
}

// linuxVncMissingDeps 返回缺失的依赖命令列表。
func linuxVncMissingDeps() []string {
	var missing []string
	for _, bin := range []string{"x11vnc"} {
		if !hasBin(bin) {
			missing = append(missing, bin)
		}
	}
	// headless（无 :0 图形会话）时需要整套虚拟桌面
	if !hasDisplay0() {
		for _, bin := range []string{"Xvfb", "openbox", "xterm"} {
			if !hasBin(bin) {
				missing = append(missing, bin)
			}
		}
	}
	return missing
}

// hasDisplay0 判断是否存在本地图形会话（X socket :0）。
func hasDisplay0() bool {
	if os.Getenv("DISPLAY") != "" && fileExists("/tmp/.X11-unix/X0") {
		return true
	}
	return fileExists("/tmp/.X11-unix/X0")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// installLinuxPkgs 用系统包管理器安装依赖（RHEL 系自动启用 EPEL）。
func installLinuxPkgs(bins []string) (string, bool) {
	// 依赖命令 -> 包名映射
	pkgOf := map[string]string{
		"x11vnc":  "x11vnc",
		"Xvfb":    "xorg-x11-server-Xvfb",
		"openbox": "openbox",
		"xterm":   "xterm",
	}
	var pkgs []string
	for _, b := range bins {
		if p, ok := pkgOf[b]; ok {
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) == 0 {
		return "", true
	}
	pkgList := quoteJoin(pkgs, " ")
	script := `
need_epel=0
for p in ` + pkgList + `; do
  case "$p" in x11vnc) need_epel=1;; esac
done
if command -v dnf >/dev/null 2>&1; then
  [ "$need_epel" = 1 ] && (dnf install -y epel-release || true)
  dnf install -y ` + pkgList + `
elif command -v yum >/dev/null 2>&1; then
  [ "$need_epel" = 1 ] && (yum install -y epel-release || true)
  yum install -y ` + pkgList + `
elif command -v apt-get >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y ` + mapDebPkgs(pkgs) + `
else
  echo "no package manager"; exit 1
fi`
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	if err != nil || !linuxDepsReady(bins) {
		tail := tailLines(string(out), 5)
		return fmt.Sprintf("依赖安装失败（%v）：可手动安装 %s。详情: %s", err, pkgList, tail), false
	}
	return "", true
}

// mapDebPkgs 将包名映射为 Debian 系等价包。
func mapDebPkgs(pkgs []string) string {
	var out []string
	for _, p := range pkgs {
		switch p {
		case "xorg-x11-server-Xvfb":
			out = append(out, "xvfb")
		default:
			out = append(out, p)
		}
	}
	return quoteJoin(out, " ")
}

func linuxDepsReady(bins []string) bool {
	for _, b := range bins {
		if !hasBin(b) {
			return false
		}
	}
	return true
}

func quoteJoin(list []string, sep string) string {
	s := ""
	for i, v := range list {
		if i > 0 {
			s += sep
		}
		s += v
	}
	return s
}

func tailLines(s string, n int) string {
	lines := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// buildVncStartScript 生成幂等的 VNC 启动脚本：
// 有真实桌面挂 :0；否则拉起 Xvfb 虚拟显示 + openbox + xterm 再挂接。
func buildVncStartScript(port int, passFile string) string {
	return `#!/bin/bash
# MoCenter VNC 启动脚本（幂等，可重复执行）
PORT='` + itoa(port) + `'
PASSFILE='` + passFile + `'
LOGTAG=mocenter-vnc

# 结束旧 x11vnc（确保加载最新口令）
pkill -f '[x]11vnc.*mocenter' 2>/dev/null
sleep 1

if [ -e /tmp/.X11-unix/X0 ]; then
  # 存在真实图形会话：直接挂接 :0（自动探测 Xauthority）
  AUTH=""
  if [ -n "$XAUTHORITY" ] && [ -f "$XAUTHORITY" ]; then
    AUTH="-auth $XAUTHORITY"
  else
    for f in /run/gdm/auth-for-DM-*/database /run/gdm3/auth-for-DM-*/database /home/*/.Xauthority /root/.Xauthority; do
      if [ -f "$f" ]; then AUTH="-auth $f"; break; fi
    done
  fi
  exec x11vnc -display :0 -rfbport "$PORT" -rfbauth "$PASSFILE" \
    -localhost -forever -shared -noxdamage -repeat -logfile /dev/stdout
fi

# headless：确保 Xvfb 虚拟显示 :99 与桌面组件在运行
if ! pgrep -f 'Xvfb :99' >/dev/null 2>&1; then
  Xvfb :99 -screen 0 1280x800x24 -nolisten tcp &
  sleep 1
fi
export DISPLAY=:99
command -v openbox >/dev/null 2>&1 && pgrep -x openbox >/dev/null 2>&1 || (openbox &)
sleep 0.5
command -v xterm >/dev/null 2>&1 && pgrep -x xterm >/dev/null 2>&1 || (xterm -geometry 100x30+0+0 &)

exec x11vnc -display :99 -rfbport "$PORT" -rfbauth "$PASSFILE" \
  -localhost -forever -shared -noxdamage -repeat
`
}

// installByCurl 通过 curl 下载 x11vnc 静态二进制到用户目录（兜底，无包管理器时）。
func installByCurl(cfg VncConfig) (string, bool) {
	if !hasBin("curl") && !hasBin("wget") {
		return "未找到 x11vnc 且无法自动安装（无 curl/wget）。请手动安装（如 dnf install -y x11vnc）。", false
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
		return "已通过 curl 下载 x11vnc 二进制。", true
	}
	return fmt.Sprintf("x11vnc 自动安装失败：下载失败（%v）。可手动安装：%s", errm, url), false
}

func hasBin(name string) bool {
	return exec.Command("bash", "-c", "command -v "+name+" >/dev/null 2>&1").Run() == nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}
