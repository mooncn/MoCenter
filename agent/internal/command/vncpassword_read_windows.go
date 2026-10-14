//go:build windows

package command

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VncReadPassword 读取本机当前生效的 VNC 口令（只读）。
//
// 与「一键安装」的区别：这里**只读不写** —— 不改配置、不重启服务、不打断正在进行的
// 远程会话。用于"口令找不到了"这种场景，避免为了拿口令而重装一次（那会断掉在用的连接）。
//
// 取值路径：UltraVNC 的 ultravnc.ini 里 passwd 是 WritePrivateProfileStruct 编码
// （16 位 HEX + 2 位校验和）。校验和必须核对 —— UltraVNC 用 GetPrivateProfileStruct
// 读取，校验和对不上就判数据损坏并**丢弃**，此时绝不能把 HEX 段解出来当口令返回，
// 那等于给出一个必然连不上的错误口令。
func VncReadPassword() VncPasswordResult {
	exe := findWinvncExe()
	if exe == "" {
		return VncPasswordResult{Message: "本机未安装 UltraVNC，无法读取 VNC 口令"}
	}
	mode := VncModeInteractive
	if serviceExists(uvncServiceName) {
		mode = VncModeService
	}

	type found struct{ path, pw string }
	var got []found
	var problems []string
	for _, p := range ultraVNCIniPaths(filepath.Dir(exe)) {
		if _, serr := os.Stat(p); serr != nil {
			continue // 只写了一份是正常的
		}
		val := iniPasswordValue(p)
		if val == "" {
			problems = append(problems, p+" 中没有 passwd")
			continue
		}
		pw, derr := decodeUltraVNCPassword(val)
		if derr != nil {
			problems = append(problems, p+": "+derr.Error())
			continue
		}
		got = append(got, found{path: p, pw: pw})
	}

	if len(got) == 0 {
		msg := "未从 UltraVNC 配置中读到可用口令"
		if len(problems) > 0 {
			msg += "（" + strings.Join(problems, "；") + "）"
		}
		return VncPasswordResult{Message: msg, Mode: mode}
	}
	// 多处配置不一致时不能随便挑一个：那正说明"读到的不一定是实际生效的那个"，
	// 必须如实报出来，而不是给出一个可能连不上的值。
	first := got[0]
	for _, g := range got[1:] {
		if g.pw != first.pw {
			return VncPasswordResult{
				Mode: mode,
				Message: "多处配置中的 VNC 口令不一致（" + first.path + " 与 " + g.path +
					"），无法确定实际生效值；建议重新执行一键安装以统一配置",
			}
		}
	}
	return VncPasswordResult{
		OK: true, Password: first.pw, Mode: mode,
		Message: "已从 " + first.path + " 读取当前 VNC 口令",
	}
}

// decodeUltraVNCPassword 解出 WritePrivateProfileStruct 编码的 passwd 值。
func decodeUltraVNCPassword(val string) (string, error) {
	val = strings.ToUpper(strings.TrimSpace(val))
	if len(val) < 4 || len(val)%2 != 0 {
		return "", fmt.Errorf("passwd 长度异常（%d 个字符）", len(val))
	}
	raw, err := hex.DecodeString(val)
	if err != nil {
		return "", fmt.Errorf("passwd 不是合法 HEX")
	}
	data, sum := raw[:len(raw)-1], raw[len(raw)-1]
	var calc byte
	for _, b := range data {
		calc += b // 与生成侧一致：按 256 取模
	}
	if calc != sum {
		return "", fmt.Errorf("passwd 校验和不符（该值已被 UltraVNC 判为损坏并丢弃）")
	}
	return vncDeobfuscatePassword(data)
}
