//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 上的凭据落盘与「SYSTEM 重跑用户可选路径」两道闸门（安全评估 H-9）。
//
// 写侧：Go 的 os.WriteFile / os.MkdirAll 传的权限位（0600）在 Windows 上**不产生任何
// ACL 语义**，对象只会继承父目录的默认 ACE。%ProgramData% 默认给 BUILTIN\Users 读权限，
// 于是含注册密钥的 mocenter-agent.json 与 users.json 对全体本地用户可读。
// 这里用「受保护的显式 DACL」取代继承来的宽授权。
//
// 读侧：保活计划任务以 SYSTEM 身份、每 5 分钟重跑安装目录里的二进制，而该目录由用户在
// 安装向导里自选。只要该路径可被普通用户改写，任意本地用户改一次文件就能在 5 分钟内拿到
// SYSTEM —— 所以注册前必须先判定「谁能写」，判不过就不注册。

const (
	// FILE_DELETE_CHILD 在 x/sys v0.22.0 未导出，按 WinNT.h 补：
	// 目录上拿到它即可删除其中的文件，配合建文件权就是「替换二进制」。
	fileDeleteChild = 0x00000040

	// 能改写内容或改 ACL 的访问位并集（文件与目录合并判定，宁可多判不漏判）。
	// WRITE_DAC 也算：能把 DACL 改回宽松状态就等于能写。
	writeLikeAccess = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
		fileDeleteChild | windows.DELETE |
		windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL

	// ntAuthority 与 serviceSidRevision：S-1-5-80-* 是服务账号（含 NT SERVICE\
	// TrustedInstaller），不是「任何本地用户」，放行。
	serviceSidRevision = 80
	// creatorAuthority：S-1-3-x（CREATOR OWNER / CREATOR GROUP）是条件主体，
	// 只在对象创建时落地成创建者自身，不给「别的用户」改写既有文件的权利。
	creatorAuthority = 3
)

// broadPrincipals 是「几乎等于全体本地用户」的宽泛身份：命中其一且带写权限，
// 就说明这台机器上的普通用户能改写该路径。
var broadPrincipals = []struct {
	sidType windows.WELL_KNOWN_SID_TYPE
	name    string
}{
	{windows.WinNullSid, "NULL SID"},
	{windows.WinWorldSid, "Everyone"},
	{windows.WinLocalSid, "NT AUTHORITY\\LOCAL"},
	{windows.WinNetworkSid, "NT AUTHORITY\\NETWORK"},
	{windows.WinBatchSid, "NT AUTHORITY\\BATCH"},
	{windows.WinInteractiveSid, "NT AUTHORITY\\INTERACTIVE"},
	{windows.WinAuthenticatedUserSid, "NT AUTHORITY\\Authenticated Users"},
	{windows.WinBuiltinUsersSid, "BUILTIN\\Users"},
	{windows.WinBuiltinGuestsSid, "BUILTIN\\Guests"},
	{windows.WinBuiltinDomainSid, "BUILTIN（机器账号组）"},
	{windows.WinAccountDomainUsersSid, "Domain Users"},
	{windows.WinAccountDomainGuestsSid, "Domain Guests"},
	{windows.WinBuiltinPreWindows2000CompatibleAccessSid, "BUILTIN\\Pre-Win2k Compatibility"},
}

// trustedWriterSidTypes 是系统里可以合法改写凭据/二进制的身份。
var trustedWriterSidTypes = []windows.WELL_KNOWN_SID_TYPE{
	windows.WinLocalSystemSid,
	windows.WinBuiltinAdministratorsSid,
	windows.WinLocalServiceSid,
	windows.WinNetworkServiceSid,
}

// hardenCredentialPath 把凭据/配置所在路径收紧为「仅 SYSTEM + Administrators + 当前用户」。
// dir=true 时该 DACL 会继续传给此后新建的子项（升级时安装程序会重新落文件）。
//
// 为什么带上当前进程用户，而不是报告字面写的「仅 Administrators+SYSTEM」：
// 便携形态下 Agent 可能以普通用户身份重写自己的配置，把写入者踢出自己的文件只会
// 让第二次写入必然失败，而威胁模型要挡的是**其他**本地用户。关键是剥掉继承来的
// Everyone / Authenticated Users / BUILTIN\Users。
//
// 失败要报出来但不阻断安装：卷未启用 ACL、目标是网络共享等情况会让 SetNamedSecurityInfo
// 失败，此时文件已经写成功，正确处置是提示「凭据可读面仍在」，而不是让整次安装回滚。
func hardenCredentialPath(path string, dir bool) error {
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

	var inheritance uint32
	if dir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		e := windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.SPECIFIC_RIGHTS_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID,
				// 三个主体分别是 well-known 组、别名组与登录用户，类型不一；
				// SetEntriesInAcl 在 TrusteeForm=TRUSTEE_IS_SID 时不看该字段。
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
	// PROTECTED：不继承父目录 ACE，否则宽授权会照样漏下来。
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("写入受保护 DACL 失败: %w", err)
	}
	return nil
}

// hardenTree 把 root 下**已存在**的目录与文件逐个收紧。
//
// 只对 root 设一次受保护 DACL 是不够的：既有安装留下的凭据文件（`agent.key`、
// `ca.key`、`node.key`、TLS 私钥、数据库）带着旧的继承型宽授权，而
// SetNamedSecurityInfo 不会追溯修改既有子项 —— 只有此后新建的文件才会继承。
// 因此升级路径上必须走这一遍，否则"收紧 ACL"只保护了本次新写的两个文件。
//
// 遍历有上限：数据目录里的日志可能成千上万，超过 hardenTreeLimit 项即停止并告警，
// 避免一次升级在慢盘上转个不停。
func hardenTree(root string) error {
	const hardenTreeLimit = 5000
	var count, failed int
	var firstErr error
	err := filepath.Walk(root, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return nil // 单个条目读不到不影响其余，计入 failed 后面统一提示
		}
		if p == root {
			return nil // 调用方已经收紧过 root
		}
		if count >= hardenTreeLimit {
			return filepath.SkipAll
		}
		count++
		if err := hardenCredentialPath(p, fi.IsDir()); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%s 下有 %d 项未能收紧（共遍历 %d 项），首条原因: %w", root, failed, count, firstErr)
	}
	if count >= hardenTreeLimit {
		fmt.Printf("  提示: %s 下条目超过 %d 项，仅收紧了前 %d 项\n", root, hardenTreeLimit, hardenTreeLimit)
	}
	return nil
}

// tokenUserSID 返回当前进程令牌的登录用户 SID。
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

// nonAdminWritable 判断 path 自身或其所在目录能否被非管理员改写。
//
// 返回值 offender 为空表示安全；否则带上主体名、受影响路径与整改命令。
// 判定口径不是「当前用户能不能写」，而是「宽泛身份（Everyone / Authenticated Users /
// BUILTIN\Users / INTERACTIVE / Domain Users…）或任何陌生身份能不能写」。
//
// 为什么连父目录一起看：文件自身 ACL 再紧，只要目录给了别人「建文件 + 删除子项」，
// 对方删掉重建即可替换二进制；反过来目录上的可继承写 ACE 会直接落到我们的 exe 上。
//
// 已知取舍：
//   - 只解析 ACCESS_ALLOWED / ACCESS_DENIED 两类 ACE（文件对象上其它类型极少，
//     且 SID 偏移不同、无法安全取址），无法解析的 ACE 不判为风险；
//   - 按 DACL 存储顺序做「先记 DENY 再判 ALLOW」的近似求值，没有调 AccessCheck：
//     后者需要一个「普通用户令牌」才能问出正确答案，本机并不存在这样的令牌。
func nonAdminWritable(path string) (string, error) {
	type target struct {
		path    string
		isDir   bool
		comment string
	}
	targets := []target{{path, false, "文件本身"}}
	if dir := filepath.Dir(path); dir != "" && dir != filepath.Clean(path) {
		targets = append(targets, target{dir, true, "所在目录（可删除重建，或经继承落到文件上）"})
	}
	for _, t := range targets {
		trustee, err := writeRiskOf(t.path, t.isDir)
		if err != nil {
			return "", fmt.Errorf("读取 %s 的 ACL 失败: %w", t.path, err)
		}
		if trustee != "" {
			return fmt.Sprintf("%s：%s ← %s 拥有写权限\n         整改: %s",
				t.comment, t.path, trustee, icaclsHint(t.path, t.isDir)), nil
		}
	}
	return "", nil
}

// writeRiskOf 返回 path 的 DACL 上第一个「有写权限且不属于可信身份」的主体；无则空串。
// isDir=true 时按目录对待：可继承的写 ACE 也算，因为它会落到子对象上。
func writeRiskOf(path string, isDir bool) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	// 区分两种「拿不到 ACL」：NULL DACL = 对所有人全开放（最大风险），
	// 空 DACL（present 但零 ACE）= 拒绝一切（安全）。x/sys 的 DACL() 只回 defaulted
	// 标志，是否 present 得从安全描述符控制位里读。
	control, _, err := sd.Control()
	if err != nil {
		return "", err
	}
	if control&windows.SE_DACL_PRESENT == 0 {
		return "该对象没有 DACL（Win32 语义下等同于向所有人开放）", nil
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return "", err
	}
	if acl == nil || acl.AceCount == 0 {
		return "", nil // 空 DACL = 拒绝一切，安全
	}

	denied := map[string]uint32{}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return "", err
		}
		if ace == nil {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE:
		default:
			continue
		}
		// INHERIT_ONLY 的 ACE 不影响对象自身；目录要算（它正是「影响子对象」的那种）。
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 && !isDir {
			continue
		}
		mask := uint32(ace.Mask) & writeLikeAccess
		if mask == 0 {
			continue
		}
		who, risky := classifySid((*windows.SID)(unsafe.Pointer(&ace.SidStart)))
		if !risky {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			denied[who] |= mask
			continue
		}
		if denied[who]&mask == mask {
			continue // 同主体的 DENY 已先覆盖这些位
		}
		return who, nil
	}
	return "", nil
}

// classifySid 判定 ACE 主体归属。risky=true 即「非管理员可写」。
//
// 刻意**不**把令牌的组 SID 当作可信身份：普通用户的令牌里本来就含 BUILTIN\Users、
// INTERACTIVE、AUTHENTICATED LOGON，一旦按组放行，检查就永远查不出「全体本地用户可写」。
// 只有当前登录用户的**那一个** SID 算自己（安装器以谁的身份跑，谁就是运维本人）。
func classifySid(sid *windows.SID) (who string, risky bool) {
	str := sid.String()
	auth := sid.IdentifierAuthority()
	if auth.Value[5] == creatorAuthority {
		return "", false
	}
	if auth.Value[5] == 5 && sid.SubAuthorityCount() > 0 && sid.SubAuthority(0) == serviceSidRevision {
		return "", false
	}
	for _, t := range trustedWriterSidTypes {
		if sid.IsWellKnown(t) {
			return "", false
		}
	}
	if me, err := tokenUserSID(); err == nil && me != nil && sid.Equals(me) {
		return "", false
	}
	for _, b := range broadPrincipals {
		if sid.IsWellKnown(b.sidType) {
			return b.name + " (" + str + ")", true
		}
	}
	// 陌生 SID 且有写权限：按风险处理，保活会把 SYSTEM 交给它。
	account, domain, _, err := sid.LookupAccount("")
	if err != nil || account == "" {
		return "无法识别的主体 (" + str + ")", true
	}
	if domain != "" {
		account = domain + "\\" + account
	}
	return account + " (" + str + ")", true
}

// icaclsHint 给出一条可直接执行的整改命令。
// 用 *S-1-5-18 这类 SID 写法而不是账户名：中文/本地化系统上名字对不上。
func icaclsHint(path string, isDir bool) string {
	ace := "(F)"
	if isDir {
		ace = "(OI)(CI)(F)"
	}
	return fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "*S-1-5-18:%s" "*S-1-5-32-544:%s"`, path, ace, ace)
}
