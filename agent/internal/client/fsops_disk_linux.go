//go:build linux

package client

import (
	"os"
	"strings"
	"syscall"

	"mocenter/agent/internal/protocol"
)

// fsVolumes 从 /proc/mounts 列出真实挂载点，再用 statfs 取容量。
// proc/sysfs/tmpfs 这类伪文件系统不算"分区"，混进来只会干扰判断，直接跳过。
func fsVolumes() []protocol.FsVolume {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []protocol.FsVolume
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mount, fstype := unescapeMount(fields[1]), fields[2]
		if !strings.HasPrefix(mount, "/") || skipFSType(fstype) || seen[mount] {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil || st.Blocks == 0 {
			continue
		}
		seen[mount] = true
		out = append(out, protocol.FsVolume{
			Name: mount, Path: mount, FSType: fstype,
			Total: int64(st.Blocks) * int64(st.Bsize),
			Free:  int64(st.Bavail) * int64(st.Bsize),
		})
	}
	return out
}

// unescapeMount 还原 /proc/mounts 对空格等字符的八进制转义（\040 = 空格）。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func skipFSType(t string) bool {
	switch t {
	case "proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "ramfs", "cgroup", "cgroup2",
		"securityfs", "debugfs", "tracefs", "configfs", "fusectl", "binfmt_misc",
		"pstore", "autofs", "mqueue", "hugetlbfs", "bpf", "nsfs", "rpc_pipefs",
		"selinuxfs", "efivarfs":
		return true
	}
	return false
}
