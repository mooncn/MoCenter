//go:build windows

package client

import (
	"os"

	"golang.org/x/sys/windows"

	"mocenter/agent/internal/protocol"
)

// fsVolumes 枚举盘符并取容量与文件系统类型（NTFS / FAT32 / exFAT …）。
// 没插盘的读卡器、已断开的网络盘会让 GetDiskFreeSpaceEx 失败，直接跳过：
// 这类卷出现在列表里既点不进去，容量也是假的。
func fsVolumes() []protocol.FsVolume {
	var out []protocol.FsVolume
	for c := 'A'; c <= 'Z'; c++ {
		root := string(c) + `:\`
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			continue
		}
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		var free, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
			continue
		}
		volName := make([]uint16, windows.MAX_PATH+1)
		fsName := make([]uint16, windows.MAX_PATH+1)
		var serial, maxLen, flags uint32
		fsType := ""
		if err := windows.GetVolumeInformation(p, &volName[0], uint32(len(volName)),
			&serial, &maxLen, &flags, &fsName[0], uint32(len(fsName))); err == nil {
			fsType = windows.UTF16ToString(fsName)
		}
		out = append(out, protocol.FsVolume{
			Name: string(c) + ":", Path: root, FSType: fsType,
			Total: int64(total), Free: int64(free),
		})
	}
	return out
}
