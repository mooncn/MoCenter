//go:build windows

package command

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// 凭据文件的 Windows ACL 收紧（安全评估 M-11）。
//
// Go 的 os.WriteFile 传 0600 在 Windows 上**不产生任何 ACL 语义**：文件只会继承
// 父目录的默认 ACE。%ProgramData% 的默认 DACL 给 BUILTIN\Users 读权限，
// 于是内含 DES 混淆 VNC 口令的 ultravnc.ini 对本机任意用户可读 —— 而那份混淆用的
// 是 VNC 家族共享的公开固定密钥（见 vncpassword.go），拿到文件就等于拿到口令，
// 再接上远程桌面即是该会话桌面的完整控制权。
//
// 因此这里显式写一份受保护的 DACL：SYSTEM + Administrators + 当前进程用户。
//
// 为什么保留「当前进程用户」而不是只留前两者：ultravnc.ini 的读者有三种 ——
// uvnc_service（SYSTEM）、由 Agent 拉起的 winvnc（继承 Agent 身份，服务模式即 SYSTEM）、
// 以及便携形态下以普通用户身份运行的 Agent 自己。把写入者踢出自己的文件会让
// 第三种形态直接连不上口令，而威胁模型要挡的是**其他**本地用户。
//
// 与 installer 模块里的同名实现是刻意的重复：agent / server / installer 是三个互不
// 引用独立 module（见各自 go.mod），不为一个 ACL helper 把它们绑在一起。
func restrictPathToPrivileged(path string) error {
	sysSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("构造 SYSTEM SID 失败: %w", err)
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("构造 Administrators SID 失败: %w", err)
	}
	sids := []*windows.SID{sysSID, adminSID}
	if u, uerr := tokenUserSID(); uerr == nil && u != nil {
		sids = append(sids, u)
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		e := windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.SPECIFIC_RIGHTS_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID,
				// TrusteeType 仅在按名字授权时使用，按 SID 授权时 API 忽略它
				TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
			},
		}
		e.Trustee.TrusteeValue = windows.TrusteeValueFromSID(sid)
		entries = append(entries, e)
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("构造 DACL 失败: %w", err)
	}
	// PROTECTED：断开继承，否则父目录的 BUILTIN\Users 读权限照样漏下来。
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("写入受保护 DACL 失败: %w", err)
	}
	return nil
}

func tokenUserSID() (*windows.SID, error) {
	t, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}
