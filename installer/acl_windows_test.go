//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// setDaclForTest 重写 path 的受保护 DACL：在「坏主体」之外同时保留
// SYSTEM / Administrators / 当前用户的全权，否则 t.TempDir 清理会因我们自己
// 被挡在门外而失败（测试目标是判定逻辑，不是把测试进程锁出去）。
func setDaclForTest(t *testing.T, path string, sidType windows.WELL_KNOWN_SID_TYPE, access windows.ACCESS_MASK, inheritance uint32) {
	t.Helper()
	riskySID, err := windows.CreateWellKnownSid(sidType)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	sids := []*windows.SID{riskySID}
	for _, wt := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		if s, err := windows.CreateWellKnownSid(wt); err == nil {
			sids = append(sids, s)
		}
	}
	if u, err := tokenUserSID(); err == nil && u != nil {
		sids = append(sids, u)
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for i, sid := range sids {
		e := windows.EXPLICIT_ACCESS{
			AccessMode: windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID,
				TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
			},
		}
		if i == 0 {
			e.AccessPermissions = access
			e.Inheritance = inheritance
		} else {
			e.AccessPermissions = windows.STANDARD_RIGHTS_ALL | windows.SPECIFIC_RIGHTS_ALL
			e.Inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		}
		e.Trustee.TrusteeValue = windows.TrusteeValueFromSID(sid)
		entries = append(entries, e)
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatalf("SetNamedSecurityInfo(%s): %v", path, err)
	}
}

// TestHardenCredentialPathKeepsUsReadableAndOthersOut 是 H-9 写侧的主用例：
// 收紧之后我们自己仍能读写，而"谁能写"的判定不再命中任何宽泛身份。
func TestHardenCredentialPathKeepsUsReadableAndOthersOut(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mocenter-agent.json")
	if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := hardenCredentialPath(dir, true); err != nil {
		t.Skipf("当前环境不允许设置受保护 DACL（%v），跳过", err)
	}
	if err := hardenCredentialPath(file, false); err != nil {
		t.Fatalf("hardenCredentialPath(文件): %v", err)
	}

	// 仍可读写：收紧不能把自己锁在门外，否则升级时写不回去
	f, err := os.OpenFile(file, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("收紧后无法读写自己的凭据文件: %v", err)
	}
	_ = f.Close()

	if offender, err := nonAdminWritable(file); err != nil {
		t.Fatalf("nonAdminWritable: %v", err)
	} else if offender != "" {
		t.Errorf("收紧后仍判定可被非管理员改写: %s", offender)
	}
}

// TestNonAdminWritableDetectsBroadWrite 覆盖读侧判定的三种真实形态：
// 文件直接给了 Everyone 写、目录给了 Users「建文件+删子项」、
// 目录只给了可继承的写权（会落到我们的 exe 上）。
func TestNonAdminWritableDetectsBroadWrite(t *testing.T) {
	cases := []struct {
		name    string
		sidType windows.WELL_KNOWN_SID_TYPE
		access  windows.ACCESS_MASK
		inherit uint32
		onDir   bool // true: 只改目录 DACL；false: 改文件 DACL
		want    string
	}{
		{"文件对 Everyone 开写", windows.WinWorldSid, windows.STANDARD_RIGHTS_ALL | windows.SPECIFIC_RIGHTS_ALL, 0, false, "Everyone"},
		{"目录对 Users 开建文件+删子项", windows.WinBuiltinUsersSid,
			windows.ACCESS_MASK(fileDeleteChild | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA),
			windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, true, "BUILTIN\\Users"},
		{"目录对 INTERACTIVE 开可继承写", windows.WinInteractiveSid,
			windows.ACCESS_MASK(windows.FILE_GENERIC_WRITE), windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, true, "INTERACTIVE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "mocenter-agent.exe")
			if err := os.WriteFile(file, []byte("bin"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := hardenCredentialPath(file, false); err != nil {
				t.Skipf("无法设置 DACL（%v），跳过", err)
			}
			target := file
			if c.onDir {
				target = dir
			}
			setDaclForTest(t, target, c.sidType, c.access, c.inherit)

			offender, err := nonAdminWritable(file)
			if err != nil {
				t.Fatalf("nonAdminWritable: %v", err)
			}
			if offender == "" {
				t.Fatalf("未检出风险：目标 %s 已被显式授予宽泛写权限", target)
			}
			if !strings.Contains(offender, c.want) {
				t.Errorf("风险描述应含 %q，实得 %q", c.want, offender)
			}
			if !strings.Contains(offender, "icacls") {
				t.Errorf("风险描述里应带整改命令，实得 %q", offender)
			}
		})
	}
}

// TestReadOnlyACEsAreNotFlagged 反向用例：只给读执行权不该被判成风险，
// 否则默认安装目录（C:\Program Files 给 BUILTIN\Users 仅 RX）会被误拒注册保活。
func TestReadOnlyACEsAreNotFlagged(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mocenter-server.exe")
	if err := os.WriteFile(file, []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	// 目录：Users 只读 + 可继承只读（等价于 Program Files 的默认形态）
	setDaclForTest(t, dir, windows.WinBuiltinUsersSid,
		windows.ACCESS_MASK(windows.FILE_GENERIC_READ), windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	if offender, err := nonAdminWritable(file); err != nil {
		t.Fatalf("nonAdminWritable: %v", err)
	} else if offender != "" {
		t.Errorf("只读授权被误判为可改写: %s", offender)
	}
}

// TestSystemTaskPathsGateFailsClosed 校验闸门本身：路径不存在（ACL 读不到）时必须
// 判为不放行 —— 判不出来就把 SYSTEM 交出去，是 H-9 最不能接受的结果。
func TestSystemTaskPathsGateFailsClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "mocenter-agent.exe")
	err := checkSystemTaskPaths(missing)
	if err == nil {
		t.Fatal("路径不可判定时应拒绝注册")
	}
	if !strings.Contains(err.Error(), "拒绝注册") {
		t.Errorf("错误信息应说明是 ACL 判定失败导致拒绝注册: %v", err)
	}
}

// TestHardenTreeRemediatesLegacyFiles 覆盖升级路径：只对数据目录设一次 DACL
// 不会追溯修改既有文件（agent.key / node.key / ca.key 都是服务端自己写的），
// 必须逐个走一遍，否则"收紧 ACL"只保护本次新写的文件。
func TestHardenTreeRemediatesLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	if err := hardenCredentialPath(dir, true); err != nil {
		t.Skipf("无法设置受保护 DACL（%v），跳过", err)
	}
	// 模拟旧安装留下的凭据：子目录 + 文件，DACL 上给 Everyone 开了写
	sub := filepath.Join(dir, "creds")
	legacy := filepath.Join(sub, "agent.key")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}
	setDaclForTest(t, sub, windows.WinWorldSid,
		windows.STANDARD_RIGHTS_ALL|windows.SPECIFIC_RIGHTS_ALL,
		windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)

	if offender, err := nonAdminWritable(legacy); err != nil {
		t.Fatalf("nonAdminWritable: %v", err)
	} else if offender == "" {
		t.Fatal("构造的旧授权未被检出，测试本身失效")
	}
	if err := hardenTree(dir); err != nil {
		t.Fatalf("hardenTree: %v", err)
	}
	if offender, err := nonAdminWritable(legacy); err != nil {
		t.Fatalf("nonAdminWritable: %v", err)
	} else if offender != "" {
		t.Errorf("遍历后旧凭据文件仍可被判为可改写: %s", offender)
	}
}

// TestRealMachinePathsVerdict 对系统里真实存在的二进制做一次判定，作为
// 「不会误伤默认安装位置」的证据：保活注册走的是这个判定，判错就等于
// 在正常机器上永久禁用自启。只读，不改任何 ACL。
func TestRealMachinePathsVerdict(t *testing.T) {
	sysdir := os.Getenv("SystemRoot")
	if sysdir == "" {
		sysdir = `C:\Windows`
	}
	var candidates []string
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base != "" {
			candidates = append(candidates, filepath.Join(base, "Git", "bin", "git.exe"))
		}
	}
	candidates = append(candidates, filepath.Join(sysdir, "System32", "cmd.exe"))

	for _, p := range candidates {
		if _, err := os.Stat(p); err != nil {
			t.Logf("跳过不存在的样本 %s", p)
			continue
		}
		offender, err := nonAdminWritable(p)
		if err != nil {
			t.Fatalf("nonAdminWritable(%s): %v", p, err)
		}
		if offender != "" {
			t.Errorf("%s 被判为可被非管理员改写；若该位置 ACL 本是默认形态，说明判定有假阳性: %s", p, offender)
		}
	}
}
