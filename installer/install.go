//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// 服务名必须与 agent / server 中 svc.Run 使用的名字完全一致，
// 否则 SCM 无法完成握手，服务会以 1053 失败。
const (
	agentServiceName  = "MoCenterAgent"
	serverServiceName = "MoCenterServer"
)

// isElevated 判断当前进程是否以管理员权限运行。
func isElevated() bool {
	t, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer t.Close()
	return t.IsElevated()
}

// parseRoles 解析 -role 参数。
func parseRoles(s string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "agent":
		return []string{"agent"}, nil
	case "server":
		return []string{"server"}, nil
	case "all", "both", "":
		return []string{"agent", "server"}, nil
	default:
		return nil, fmt.Errorf("未知角色 %q（应为 agent | server | all）", s)
	}
}

func serviceNameOf(role string) string {
	if role == "server" {
		return serverServiceName
	}
	return agentServiceName
}

// doInstall 安装并注册服务。
func doInstall(o options) error {
	if !isElevated() {
		return errors.New("需要以管理员身份运行（注册服务、写入 Program Files 都需要）")
	}
	roles, err := parseRoles(o.role)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if r == "agent" && strings.TrimSpace(o.server) == "" {
			return errors.New("安装 Agent 必须指定 -server（服务端地址 host:port）")
		}
		if r == "agent" && strings.TrimSpace(o.agentKey) == "" {
			return errors.New("安装 Agent 必须指定 -agent-key（取总控机数据目录下 agent.key 的内容）：" +
				"服务端要求 Agent 携带注册密钥，否则连接会被拒绝")
		}
		if r == "server" && (strings.TrimSpace(o.adminUser) == "" || (strings.TrimSpace(o.adminPass) == "" && strings.TrimSpace(o.adminPassFile) == "")) {
			return errors.New("安装 Server 必须指定 -user 与 -password（后台登录账号）")
		}
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer m.Disconnect()

	for _, r := range roles {
		if err := installRole(m, o, r); err != nil {
			return fmt.Errorf("安装 %s 失败: %w", r, err)
		}
	}
	fmt.Println("\n安装完成。配置与数据位于 " + o.dataRoot + `\<角色>\，卸载默认保留。`)
	return nil
}

// installRole 安装单个角色：停旧 → 释放二进制 → 写配置 → 注册服务 → 配置重启 → 启动。
func installRole(m *mgr.Mgr, o options, role string) error {
	name := serviceNameOf(role)
	exePath := filepath.Join(o.installDir, "mocenter-"+role+".exe")
	dataDir := filepath.Join(o.dataRoot, role)
	confPath := filepath.Join(dataDir, "mocenter-"+role+".json")

	fmt.Printf("\n=== 安装 %s ===\n", role)
	fmt.Printf("  服务名  : %s\n", name)
	fmt.Printf("  二进制  : %s\n", exePath)
	fmt.Printf("  配置数据: %s\n", dataDir)

	// 1) 先停掉并删除旧服务：既保证重复安装幂等，也避免正在运行的 exe 被占用无法覆盖
	if err := stopAndDeleteService(m, name); err != nil {
		return err
	}

	// 2) 确认二进制就位。
	//    引擎不再自带二进制：图形安装程序（Inno）会先把文件放到安装目录；
	//    静默部署时也请把 mocenter-agent.exe / mocenter-server.exe 与引擎放在一起。
	if _, err := os.Stat(exePath); err != nil {
		return fmt.Errorf("未找到 %s；请先把它与本引擎放在同一目录（图形安装程序会自动完成这一步）", exePath)
	}
	fmt.Println("  [1/5] 二进制已就位")

	// 3) 写固定位置的配置（与二进制分离，升级/重装不覆盖用户后续改动）
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	// Server 角色：创建后台登录账号（用户名+密码，密码只存 SHA-256 摘要）。
	// 已存在账号时保持不动，避免重装覆盖用户后来改过的密码。
	if role == "server" {
		// 口令优先从文件读。
		//
		// 为什么需要：图形安装程序（Inno Setup）通过命令行调用本程序，
		// 而"安装向导进程的命令行"在安装进行的那几秒里对其他本地进程可见
		// （典型是 `wmic process get commandline` / 任务管理器"命令行"列）。
		// 由安装程序把口令先写进仅管理员可读的临时文件、再把**路径**传进来，
		// 就可以完全避开这条泄露路径。
		if pf := strings.TrimSpace(o.adminPassFile); pf != "" {
			b, rerr := os.ReadFile(pf)
			if rerr != nil {
				return fmt.Errorf("读取口令文件 %s 失败: %w", pf, rerr)
			}
			o.adminPass = strings.TrimSpace(string(b))
			if o.adminPass == "" {
				return fmt.Errorf("口令文件 %s 为空", pf)
			}
			// 读完立即删除：里面的口令已经被摘要进 users.json，留着只是多一个泄露面。
			// 删不掉不视为失败（可能被安全软件占用），但要提示。
			if derr := os.Remove(pf); derr != nil {
				fmt.Printf("  警告: 口令文件 %s 未能删除（%v），请手动删除\n", pf, derr)
			}
		}
		created, err := ensureServerAccount(dataDir, o.adminUser, o.adminPass)
		if err != nil {
			return err
		}
		if created {
			fmt.Printf("  [2/5] 已创建后台登录账号 %s\n", strings.TrimSpace(o.adminUser))
		} else {
			fmt.Printf("  [2/5] 已存在后台账号，保留原有密码（未按本次输入覆盖）\n")
		}
	}
	if err := writeConfig(role, confPath, o); err != nil {
		return err
	}
	fmt.Printf("  [2/5] 已写入配置 %s\n", confPath)

	// 4) 注册服务。命令行只带 -config（Server 另带 -data），
	//    账号密码不进命令行 —— 密码只以摘要落在 users.json（见 ensureServerAccount），
	//    避免留在服务注册表项里被随意读到。
	args := []string{"-config", confPath}
	if role == "server" {
		args = append(args, "-data", dataDir)
	}
	if s := strings.TrimSpace(o.extra); s != "" {
		args = append(args, strings.Fields(s)...)
	}
	if err := createService(m, name, exePath, args, role); err != nil {
		return err
	}
	fmt.Println("  [3/5] 已注册服务（开机自动启动）")

	// 5) 失败自动重启
	if err := setRecovery(m, name); err != nil {
		fmt.Printf("  [4/5] 警告: 设置失败恢复动作未成功（%v），不影响服务本身\n", err)
	} else {
		fmt.Println("  [4/5] 已配置失败自动重启")
	}

	// 6) 启动并等待真正进入 Running
	if err := startAndWait(m, name); err != nil {
		return err
	}
	fmt.Println("  [5/5] 服务已启动")
	return nil
}

// writeConfig 写配置文件。
//
// 这里刻意不引用 agent/server 的 config 包（三者是独立 module），只按它们的 JSON
// 字段名生成，避免把模块耦合在一起。字段名若变更需同步这里。
func writeConfig(role, path string, o options) error {
	var obj map[string]any
	if role == "server" {
		obj = map[string]any{
			"listen":   o.listen,
			"http":     o.httpAddr,
			"user":     o.adminUser,
			"data_dir": filepath.Dir(path),
		}
		if o.webDir != "" {
			obj["web_dir"] = o.webDir
		}
		// 服务端默认自动生成 CA 与 Agent 通道证书（零配置 TLS），
		// 这里只记录身份名，便于多套总控之间区分。
		if strings.TrimSpace(o.serverName) != "" {
			obj["server_name"] = strings.TrimSpace(o.serverName)
		}
	} else {
		if strings.TrimSpace(o.agentKey) == "" {
			return errors.New("安装 Agent 必须指定 -agent-key（取服务端 <data>\\agent.key 的内容）：" +
				"服务端默认要求 Agent 携带注册密钥，否则连接会被拒绝")
		}
		obj = map[string]any{
			"server":    o.server,
			"agent_key": strings.TrimSpace(o.agentKey),
			"daemon":    true,
			// 首次启动用注册密钥换取客户端证书（服务端内置 CA 签发），
			// 换取成功后该字段会被清空、证书落到 creds\ 下 —— 运维无需手工准备证书。
			"enrollment_secret": strings.TrimSpace(o.agentKey),
		}
		if strings.TrimSpace(o.serverFingerprint) != "" {
			obj["server_fingerprint"] = strings.TrimSpace(o.serverFingerprint)
		}
		if o.agentID != "" {
			obj["agent_id"] = o.agentID
		}
		if strings.TrimSpace(o.agentUUID) != "" {
			obj["uuid"] = strings.TrimSpace(o.agentUUID)
		}
		if o.proxyIP != "" {
			obj["proxy_ip"] = o.proxyIP
		}
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	// 配置里有注册密钥，权限收紧
	return os.WriteFile(path, b, 0600)
}

// ensureServerAccount 在数据目录下创建/补全后台登录账号（users.json）。
//
// 为什么不把密码写进 mocenter-server.json：本项目的既有约定是「密码不落盘明文」
// （见 server/main.go：-password / -password-file 只用于首次创建账号，
// 随后以摘要持久化到 users.json；-password-file 是推荐写法，避免口令进命令行）。
// 因此这里直接写 users.json。
//
// 摘要算法：这里仍写**单轮 SHA-256**（三个 module 相互独立、不做包引用，属既有取舍）。
// 服务端 auth.verifyPassword 兼容该旧格式：首次用它登录成功后会**透明升级**为
// PBKDF2-HMAC-SHA256，因此兼容性没有问题；users.json 的 {"users":{...}} 结构必须一致。
//
// ⚠️ 已存在账号时**不做任何改动**：重装/升级不能用安装时输入的密码覆盖用户后来
// 自己修改过的密码。
func ensureServerAccount(dataDir, username, password string) (created bool, err error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return false, errors.New("创建后台账号需要用户名与密码")
	}
	path := filepath.Join(dataDir, "users.json")

	// 已有账号：原样保留
	if b, rerr := os.ReadFile(path); rerr == nil {
		var cur struct {
			Users map[string]any `json:"users"`
		}
		if json.Unmarshal(b, &cur) == nil && len(cur.Users) > 0 {
			return false, nil
		}
	}

	sum := sha256.Sum256([]byte(password))
	doc := map[string]any{
		"users": map[string]any{
			username: map[string]any{
				"username":      username,
				"password_hash": hex.EncodeToString(sum[:]),
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return false, fmt.Errorf("写入账号文件失败: %w", err)
	}
	return true, nil
}

// stopAndDeleteService 停止并删除同名服务；服务不存在时直接返回（幂等）。
func stopAndDeleteService(m *mgr.Mgr, name string) error {
	s, err := m.OpenService(name)
	if err != nil {
		return nil // 不存在，无需清理
	}
	defer s.Close()

	if st, qerr := s.Query(); qerr == nil && st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		// 等待真正停止：未停稳就删除会报"服务已被标记为删除"，且 exe 仍被占用
		for i := 0; i < 40; i++ {
			time.Sleep(500 * time.Millisecond)
			if st, qerr = s.Query(); qerr == nil && st.State == svc.Stopped {
				break
			}
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("删除旧服务 %s 失败（可能仍在停止中，稍后重试即可）: %w", name, err)
	}
	return nil
}

func createService(m *mgr.Mgr, name, exe string, args []string, role string) error {
	display, desc := "MoCenter Agent", "MoCenter Agent：内网主机探针上报与远程运维执行端"
	if role == "server" {
		display, desc = "MoCenter Server", "MoCenter Server：集中式主机远控管理总控服务端"
	}
	s, err := m.CreateService(name, exe, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: display,
		Description: desc,
	}, args...)
	if err != nil {
		return fmt.Errorf("创建服务失败: %w", err)
	}
	s.Close()
	return nil
}

// startAndWait 启动服务并等待其真正进入 Running。
// 只调用 Start 不等待是不够的 —— 服务可能因配置错误立刻退出，那属于安装失败。
func startAndWait(m *mgr.Mgr, name string) error {
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("启动服务失败: %w", err)
	}
	for i := 0; i < 40; i++ { // 最多等 20 秒
		if st, qerr := s.Query(); qerr == nil && st.State == svc.Running {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("服务未在 20 秒内进入运行状态；请检查 %%ProgramData%%\\MoCenter\\<角色>\\logs 下的日志")
}

// setRecovery 通过 SCM API 配置失败恢复动作（崩溃后自动重启）。
//
// 用 API 而不是 sc.exe：安装器本来就在用 SCM API 管理服务，再依赖外部命令既多余，
// 也会在 sc.exe 不可用或被安全策略拦截的环境下失效。
func setRecovery(m *mgr.Mgr, name string) error {
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	actions := [3]windows.SC_ACTION{
		{Type: windows.SC_ACTION_RESTART, Delay: 5000},  // 首次失败：5 秒后重启
		{Type: windows.SC_ACTION_RESTART, Delay: 10000}, // 第二次：10 秒
		{Type: windows.SC_ACTION_RESTART, Delay: 30000}, // 之后：30 秒
	}
	fa := windows.SERVICE_FAILURE_ACTIONS{
		ResetPeriod:  86400, // 失败计数 24 小时后归零
		ActionsCount: uint32(len(actions)),
		Actions:      &actions[0],
	}
	return windows.ChangeServiceConfig2(
		s.Handle,
		windows.SERVICE_CONFIG_FAILURE_ACTIONS,
		(*byte)(unsafe.Pointer(&fa)),
	)
}

// doUninstall 停止并删除服务，默认保留配置与数据。
func doUninstall(o options) error {
	if !isElevated() {
		return errors.New("需要以管理员身份运行")
	}
	roles, err := parseRoles(o.role)
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("连接服务控制管理器失败: %w", err)
	}
	defer m.Disconnect()

	for _, r := range roles {
		name := serviceNameOf(r)
		if err := stopAndDeleteService(m, name); err != nil {
			return err
		}
		fmt.Printf("已删除服务 %s\n", name)

		exePath := filepath.Join(o.installDir, "mocenter-"+r+".exe")
		// 由外层安装器（Inno）调用时保留二进制，交给它统一删除，
		// 避免两边都删导致"文件不存在"的噪音与竞态。
		if !o.keepBins {
			if err := os.Remove(exePath); err != nil && !os.IsNotExist(err) {
				fmt.Printf("  警告: 删除 %s 失败: %v\n", exePath, err)
			}
		}
		if o.purge {
			dataDir := filepath.Join(o.dataRoot, r)
			if err := os.RemoveAll(dataDir); err != nil {
				fmt.Printf("  警告: 删除 %s 失败: %v\n", dataDir, err)
			} else {
				fmt.Printf("已删除数据目录 %s\n", dataDir)
			}
		}
	}
	if !o.purge {
		fmt.Printf("\n配置与数据已保留在 %s（如需一并删除请加 -purge）\n", o.dataRoot)
	}
	return nil
}

// doStatus 打印各角色的服务状态与路径。
//
// 刻意不走 SCM API：mgr.Connect() 请求的是 SC_MANAGER_ALL_ACCESS，需要管理员权限，
// 而"查看状态"不该有这个门槛。改用 sc.exe query，普通用户即可查看。
func doStatus(o options) error {
	roles, err := parseRoles(o.role)
	if err != nil {
		return err
	}
	for _, r := range roles {
		name := serviceNameOf(r)
		fmt.Printf("\n=== %s ===\n", r)
		fmt.Printf("  服务名  : %s\n", name)
		fmt.Printf("  二进制  : %s\n", filepath.Join(o.installDir, "mocenter-"+r+".exe"))
		fmt.Printf("  配置数据: %s\n", filepath.Join(o.dataRoot, r))
		fmt.Printf("  状态    : %s\n", queryServiceState(name))
	}
	return nil
}

// queryServiceState 查询服务状态，且**不需要管理员权限**。
//
// 两个刻意的选择：
//   - 不走 mgr.Connect()：它请求 SC_MANAGER_ALL_ACCESS，会把普通用户挡在门外；
//     这里只用 SC_MANAGER_CONNECT + SERVICE_QUERY_STATUS 这两个最小掩码。
//   - 不走 sc.exe：避免依赖外部命令（也避免在被安全策略拦截的环境下失效）。
func queryServiceState(name string) string {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "查询失败: " + err.Error()
	}
	defer windows.CloseServiceHandle(scm)

	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "查询失败: " + err.Error()
	}
	h, err := windows.OpenService(scm, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return "未安装"
	}
	defer windows.CloseServiceHandle(h)

	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "查询失败: " + err.Error()
	}
	return stateText(st.CurrentState)
}

func stateText(state uint32) string {
	switch state {
	case windows.SERVICE_STOPPED:
		return "已停止"
	case windows.SERVICE_START_PENDING:
		return "正在启动"
	case windows.SERVICE_STOP_PENDING:
		return "正在停止"
	case windows.SERVICE_RUNNING:
		return "运行中"
	case windows.SERVICE_CONTINUE_PENDING:
		return "正在继续"
	case windows.SERVICE_PAUSE_PENDING:
		return "正在暂停"
	case windows.SERVICE_PAUSED:
		return "已暂停"
	}
	return fmt.Sprintf("未知(%d)", state)
}
