//go:build windows

package command

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// M-11 的落点：ultravnc.ini 里那份 DES 混淆口令用的是 VNC 家族共享的公开固定密钥，
// 文件对本机任意用户可读就等于口令可读。os.WriteFile 的 0600 在 Windows 上不产生
// 任何 ACL 语义，所以"收紧"必须是显式 DACL —— 这三条测试把这件事钉住：
// 继承来的 BUILTIN\Users 必须被剥掉、SYSTEM+Administrators+写入者必须留下、
// 而且写入者之后还能写自己的文件（否则便携形态直接失效）。

// fileACEs 读 path 的显式 DACL，返回主体 SID 与"是否存在继承型 ACE"。
func fileACEs(t *testing.T, path string) (sids []*windows.SID, inherited bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("读取 %s 的 DACL 失败: %v", path, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("取 %s 的 ACL 失败: %v", path, err)
	}
	if acl == nil {
		return nil, false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatalf("取第 %d 条 ACE 失败: %v", i, err)
		}
		if ace == nil {
			continue
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			inherited = true
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
	}
	return sids, inherited
}

func hasWellKnown(t *testing.T, sids []*windows.SID, wt windows.WELL_KNOWN_SID_TYPE) bool {
	t.Helper()
	for _, s := range sids {
		if s.IsWellKnown(wt) {
			return true
		}
	}
	return false
}

// leakUsersToTempDir 给临时目录加一条可继承的 BUILTIN\Users 只读 ACE，
// 复刻 %ProgramData% 的默认形态：新建文件会继承它。
func leakUsersToTempDir(t *testing.T, dir string) {
	t.Helper()
	usersSID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("构造 Users SID 失败: %v", err)
	}
	e := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ | windows.GENERIC_EXECUTE,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(usersSID),
		},
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{e}, nil)
	if err != nil {
		t.Fatalf("构造夹具 DACL 失败: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatalf("写入夹具 DACL 失败: %v", err)
	}
}

func TestRestrictPathToPrivilegedStripsInheritedUsersRead(t *testing.T) {
	dir := t.TempDir()
	leakUsersToTempDir(t, dir)

	path := filepath.Join(dir, "ultravnc.ini")
	if err := os.WriteFile(path, []byte("[ultravnc]\nPassword=0011223344556677\n"), 0600); err != nil {
		t.Fatalf("写入样例文件失败: %v", err)
	}

	// 前置条件必须先成立：文件确实继承了 Users 可读。
	// 不成立就说明夹具没造出 %ProgramData% 那种默认 DACL，
	// 后面的断言全部会"因为无事发生"而空过。
	sids, _ := fileACEs(t, path)
	if !hasWellKnown(t, sids, windows.WinBuiltinUsersSid) {
		t.Skip("本机未能造出「继承型 Users 可读」的夹具（卷未启用 ACL 或权限不足），本轮不适用")
	}

	if err := restrictPathToPrivileged(path); err != nil {
		t.Fatalf("收紧失败: %v", err)
	}

	sids, inherited := fileACEs(t, path)
	if hasWellKnown(t, sids, windows.WinBuiltinUsersSid) {
		t.Error("收紧后 BUILTIN\\Users 仍在这份含 VNC 口令的文件上")
	}
	if inherited {
		t.Error("收紧后仍有继承型 ACE：PROTECTED_DACL 没起作用，父目录的宽授权照样漏下来")
	}
	if !hasWellKnown(t, sids, windows.WinLocalSystemSid) {
		t.Error("收紧后 SYSTEM 不见了：uvnc_service 将读不到自己的配置文件")
	}
	if !hasWellKnown(t, sids, windows.WinBuiltinAdministratorsSid) {
		t.Error("收紧后 Administrators 不见了")
	}
	me, err := tokenUserSID()
	if err != nil {
		t.Fatalf("取当前进程用户 SID 失败: %v", err)
	}
	var keepMe bool
	for _, s := range sids {
		if s.Equals(me) {
			keepMe = true
		}
	}
	if !keepMe {
		t.Errorf("收紧后写入者 %s 被踢出自己的文件：便携形态的第二次写入必然失败", me.String())
	}
}

// 收紧不能只"这一次生效"：Agent 每次写 ultravnc.ini 都会重新落文件，
// 而调用点是"写完即收紧"。这里验证收紧后的文件仍可被写入者改写并再次收紧，
// 也就是重复安装/改口令的路径不会把自己锁死。
func TestRestrictPathToPrivilegedIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ultravnc.ini")
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(path, []byte("[ultravnc]\nrot="+"x"), 0600); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
		if err := restrictPathToPrivileged(path); err != nil {
			t.Fatalf("第 %d 次收紧失败: %v", i+1, err)
		}
	}
	if err := os.WriteFile(path, []byte("[ultravnc]\nafter-restrict\n"), 0600); err != nil {
		t.Errorf("收紧后写入者无法再改自己的文件: %v", err)
	}
}
