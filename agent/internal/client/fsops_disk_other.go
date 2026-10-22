//go:build !windows && !linux

package client

import "mocenter/agent/internal/protocol"

// fsVolumes 只在 Windows / Linux 上枚举分区；其它平台返回空，界面会隐藏磁盘条。
func fsVolumes() []protocol.FsVolume { return nil }
