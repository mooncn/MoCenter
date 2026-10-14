//go:build linux

package command

import (
	"os"
	"path/filepath"
)

// VncReadPassword 读取本机当前生效的 VNC 口令（只读）。
//
// 与「一键安装」的区别：只读不写，不重启服务、不打断正在进行的远程会话。
//
// 取值路径：x11vnc 的 -rfbauth 口令文件，内容就是 8 字节 DES 混淆口令，
// 与服务模式/交互模式共用同一套编码（见 vncObfuscatePassword）。
func VncReadPassword() VncPasswordResult {
	// 服务模式写 /etc/mocenter/vncpasswd，交互模式写 ~/.mocenter/vncpasswd。
	// 两者都在时要求一致：不一致说明读到的不一定是实际生效的那个。
	cands := []struct{ path, mode string }{
		{x11vncServicePassFile, VncModeService},
		{filepath.Join(homeDir(), ".mocenter", "vncpasswd"), VncModeInteractive},
	}

	type found struct{ path, pw, mode string }
	var got []found
	for _, c := range cands {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		pw, derr := vncDeobfuscatePassword(data)
		if derr != nil {
			continue
		}
		got = append(got, found{path: c.path, pw: pw, mode: c.mode})
	}

	if len(got) == 0 {
		return VncPasswordResult{Message: "未找到 VNC 口令文件（可能尚未执行一键安装）"}
	}
	first := got[0]
	for _, g := range got[1:] {
		if g.pw != first.pw {
			return VncPasswordResult{
				Mode: first.mode,
				Message: "多处口令文件内容不一致（" + first.path + " 与 " + g.path +
					"），无法确定实际生效值；建议重新执行一键安装以统一配置",
			}
		}
	}
	return VncPasswordResult{
		OK: true, Password: first.pw, Mode: first.mode,
		Message: "已从 " + first.path + " 读取当前 VNC 口令",
	}
}
