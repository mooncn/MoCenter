//go:build linux

// Linux VNC 一键安装/启动：优先 x11vnc，使用 apt/yum/dnf 分发。
package command

import (
	"os/exec"
	"strings"
)

func vncInstallLinux(port int) VncInstallResult {
	res := VncInstallResult{Port: port}

	// 已安装则直接启动
	start := `DISPLAY="${DISPLAY:-:0}" x11vnc -forever -shared -noxdamage -nopw -rfbport ` +
		itoa(port) + ` </dev/null >/tmp/mocenter_x11vnc.log 2>&1 &`
	if _, err := exec.Command("bash", "-c", "command -v x11vnc >/dev/null 2>&1").Output(); err == nil {
		_ = exec.Command("bash", "-c", start).Run()
		res.Message = "已启动 x11vnc（无密码，端口 " + itoa(port) + "）。若提示无图形会话，请确保主机存在 DISPLAY。"
		res.OK = portOpen(port)
		return res
	}

	script := start + `
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
  echo "VNC 已安装并尝试启动"
else
  echo "未找到 x11vnc 且自动安装失败，请手动安装（如 sudo apt-get install -y x11vnc）"
fi`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	res.Message = strings.TrimSpace(string(out))
	if err == nil && portOpen(port) {
		res.OK = true
		res.Message = "VNC(x11vnc) 已就绪，端口 " + itoa(port) + "\n" + res.Message
	} else if err == nil {
		res.Message = "VNC 安装已尝试，端口暂未就绪（可能需图形会话 X/桌面）：\n" + res.Message
	} else {
		res.Message = "安装异常: " + err.Error() + "\n" + res.Message
	}
	return res
}