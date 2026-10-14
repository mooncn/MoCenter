//go:build !windows && !linux

package command

// VncReadPassword 其它平台暂不支持读取 VNC 口令。
//
// 保留实现而不是让编译失败：命令包在 darwin 上也要能构建（交叉编译与 CI 会跑到）。
func VncReadPassword() VncPasswordResult {
	return VncPasswordResult{Message: "当前平台暂不支持读取 VNC 口令"}
}
