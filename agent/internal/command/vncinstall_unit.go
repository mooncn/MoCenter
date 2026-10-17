// systemd 服务单元与 x11vnc 启动脚本模板，以及相关路径常量。
//
// 本文件刻意**不带 build tag**：这些都是纯字符串模板、不依赖运行平台，
// 放在这里是为了能在任意平台（含开发机的 Windows）上做单元测试，
// 避免"只有 Linux 才编译得到"而长期缺乏回归保护。
package command

import "runtime"

// x11vncServiceName systemd 服务单元名。
const x11vncServiceName = "mocenter-vnc.service"

// x11vncServicePath systemd 系统服务单元路径。
const x11vncServicePath = "/etc/systemd/system/" + x11vncServiceName

// x11vncServicePassFile 服务模式的口令文件路径（root 可读，0600）。
const x11vncServicePassFile = "/etc/mocenter/vncpasswd"

// x11vncServiceLog 服务模式的 x11vnc 日志路径。
//
// 路径中含 "mocenter" 是有意为之：交互模式与服务模式的 x11vnc 命令行都带该标记，
// 便于切换模式时用 pkill -f 'x11vnc.*mocenter' 精确清掉自己的实例而不误伤他人。
const x11vncServiceLog = "/var/log/mocenter-vnc.log"

// x11vncPkgName 返回当前架构对应的「服务端托管 x11vnc 二进制」文件名。
//
// 重要：LibVNC/x11vnc 官方**只发布源码**（GitHub release 里没有任何预编译产物，
// 只有自动生成的源码归档），因此**不存在可直下的官方成品**。
// 该名称是给「运维自行构建/自备二进制」的场景用的：把与目标机发行版、架构匹配的
// x11vnc 二进制按此约定名放进服务端 packages 目录，Agent 即可从服务端拉取，
// 从而避免从第三方站点下载（那反而与"防劫持/防篡改"的目标相悖）。
//
// 优先路径始终是发行版包管理器（Debian/Ubuntu 的 x11vnc、
// RHEL/Rocky/Alma 的 EPEL 亦提供 x11vnc，实测 EPEL8 为 0.9.17）。
func x11vncPkgName() string {
	switch runtime.GOARCH {
	case "arm64":
		return "x11vnc-linux-arm64"
	case "arm":
		return "x11vnc-linux-arm"
	case "386":
		return "x11vnc-linux-386"
	default:
		return "x11vnc-linux-amd64"
	}
}

// buildX11VNCServiceUnit 生成 systemd 系统服务单元。
//
// 为什么服务模式必须做成系统服务（而不是像交互模式那样拉起一个后台进程）：
// 要覆盖「登录界面」就必须在无人登录时就已在运行，并以 root 挂接控制台显示 :0。
//
// 关键参数：
//   - -auth guess  自动探测 Xauthority。登录界面的 X 授权文件与用户会话不是同一个，
//     写死路径会随发行版/显示管理器变化而失效。
//   - -loop        X 服务重启时（如注销回到登录界面、或切换用户）自动重连而不退出，
//     这是能"跟着会话走"的关键。
//   - -localhost  仅监听回环，配合 Agent 隧道回连，不对外暴露端口。
func buildX11VNCServiceUnit(port int, exe string) string {
	return `[Unit]
Description=MoCenter VNC Server (x11vnc on console display)
After=display-manager.service network.target
Wants=display-manager.service

[Service]
Type=simple
User=root
ExecStart=` + exe + ` -display :0 -auth guess -rfbport ` + itoa(port) + ` -rfbauth ` + x11vncServicePassFile + ` -localhost -forever -shared -loop -noxdamage -repeat -o ` + x11vncServiceLog + `
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`
}

// buildVncStartScript 生成交互会话模式的幂等启动脚本：
// 有真实桌面（:0）就挂 :0；否则拉起 Xvfb 虚拟显示 + openbox + xterm 再挂接。
//
// logFile 经 x11vnc 的 -o 写入，路径含 "mocenter"，使命令行带上标记，
// 便于切换模式时用 pkill -f 'x11vnc.*mocenter' 精确清理自己的实例。
func buildVncStartScript(port int, passFile, logFile string) string {
	return `#!/bin/bash
# MoCenter VNC 启动脚本（幂等，可重复执行）
PORT='` + itoa(port) + `'
PASSFILE='` + passFile + `'
LOG='` + logFile + `'

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
    -localhost -forever -shared -noxdamage -repeat -o "$LOG" $AUTH
fi

# headless：确保 Xvfb 虚拟显示 :99 与桌面组件在运行
if ! pgrep -f 'Xvfb :99' >/dev/null 2>&1; then
  Xvfb :99 -screen 0 1280x800x24 -nolisten tcp &
  sleep 1
fi
export DISPLAY=:99

# 关掉虚拟屏的屏保清屏：X 默认约 600s 无输入即把画面刷黑，远程查看的表现是
# "连上了但全黑"（实测踩过）。虚拟屏没有本地用户，无现场需要保护。
# xset 缺失时静默跳过，不影响后续拉起。
if command -v xset >/dev/null 2>&1; then
  xset s off 2>/dev/null || true
  xset -dpms 2>/dev/null || true
fi

command -v openbox >/dev/null 2>&1 && pgrep -x openbox >/dev/null 2>&1 || (openbox &)
sleep 0.5
command -v xterm >/dev/null 2>&1 && pgrep -x xterm >/dev/null 2>&1 || (xterm -geometry 100x30+0+0 &)

exec x11vnc -display :99 -rfbport "$PORT" -rfbauth "$PASSFILE" \
  -localhost -forever -shared -noxdamage -repeat -o "$LOG"
`
}
