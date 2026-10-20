//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// loadAgentKeyFromFile 把 -agent-key-file 的内容搬到 o.agentKey，然后删掉那个文件。
//
// 为什么要走文件：Inno 图形安装程序以命令行调起本引擎，而"安装那几十秒的命令行"
// 对同机任意进程可见（wmic process get commandline / 任务管理器的"命令行"列）。
// 注册密钥比后台口令更该避这条路径 —— 它是 C-1 那套一次性令牌的根凭据。
//
// 为什么单独成函数而不是内联在 doInstall 里：doInstall 需要管理员权限，
// 内联就等于这三条要害（读不到要报错、空文件不算成功、用完必须删）永远测不到。
func loadAgentKeyFromFile(o *options) error {
	kf := strings.TrimSpace(o.agentKeyFile)
	if kf == "" {
		return nil
	}
	b, err := os.ReadFile(kf)
	if err != nil {
		return fmt.Errorf("读取注册凭据文件 %s 失败: %w", kf, err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return fmt.Errorf("注册凭据文件 %s 为空", kf)
	}
	o.agentKey = key
	// 删不掉不当失败处理（可能被安全软件占用），但一定要提示。
	if derr := os.Remove(kf); derr != nil {
		fmt.Printf("  警告: 注册凭据文件 %s 未能删除（%v），请手动删除\n", kf, derr)
	}
	return nil
}

// resolveAdminPass 处理 Server 登录口令的入口收口（H-9 残留 (i)）。
//
// 背景：安装进程的命令行对同机任意进程可见（wmic process get commandline /
// 任务管理器的"命令行"列），明文口令放 -password 等于当众广播。与
// install-linux.sh 拒绝 --password=<明文> 的口径保持一致：
//
//   - -password 给了非空值 → 直接报错并指路（旗标保留但不收值）；
//   - -password-stdin → 从 in 读一行作为口令（type pw.txt | mocenter-deploy.exe ...）；
//   - 都没给 → 交由 doInstall 的"必须指定"校验或升级路径处理。
//
// 单独成函数（而不是散在 main）是为了能注入 io.Reader 做单测；
// 与 loadAgentKeyFromFile 同理，内联在需要管理员权限的 doInstall 里就永远测不到。
func resolveAdminPass(o *options, in io.Reader) error {
	if strings.TrimSpace(o.adminPass) != "" {
		return errors.New("不再接受 -password <明文>：安装进程的命令行对同机任意进程可见（H-9）。\n" +
			"  请改用以下任一方式：\n" +
			"    -password-file <路径>    口令放在仅管理员可读的文件里（图形安装程序走这条）\n" +
			"    -password-stdin          从标准输入读一行（type pw.txt | mocenter-deploy.exe install -role server ...）\n" +
			"  若本机已装过（配置存在），可两项都省略直接升级")
	}
	if !o.adminPassStdin {
		return nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("读取标准输入失败（-password-stdin 需要一行口令，如 type pw.txt | mocenter-deploy.exe ...）: %w", err)
	}
	o.adminPass = strings.TrimRight(line, "\r\n")
	// 口令本身允许含空格，但首尾空白按惯例视作录入噪声（与 -password-file 的
	// TrimSpace 口径一致）。全空白等于没给，让 doInstall 的校验去报错。
	o.adminPass = strings.TrimSpace(o.adminPass)
	if o.adminPass == "" {
		return errors.New("-password-stdin 读到的口令为空：请在标准输入给一行非空白口令")
	}
	return nil
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
	if err := loadAgentKeyFromFile(&o); err != nil {
		return err
	}
	for _, r := range roles {
		// 升级（配置已存在且未指定 -force）：连接参数与账号都沿用既有的，
		// 不要求重新提供，也不覆盖配置 —— 这就是「升级不动配置」。
		if !o.force && existingInstall(o, r) {
			fmt.Printf("[%s] 检测到既有配置，按升级处理（保留配置与账号）\n", r)
			continue
		}
		if r == "agent" && strings.TrimSpace(o.server) == "" {
			return errors.New("安装 Agent 必须指定 -server（服务端地址 host:port）；" +
				"若本机已装过（配置存在），可省略该参数直接升级")
		}
		if r == "agent" && strings.TrimSpace(o.agentKey) == "" {
			return errors.New("安装 Agent 必须指定 -agent-key 或 -agent-key-file（取总控机数据目录下 agent.key 的内容）：" +
				"服务端要求 Agent 出示注册凭据，否则连接会被拒绝。" +
				"推荐用 -agent-key-file（密钥不进命令行）；若本机已装过（配置存在），可省略直接升级")
		}
		if r == "server" && (strings.TrimSpace(o.adminUser) == "" || (strings.TrimSpace(o.adminPass) == "" && strings.TrimSpace(o.adminPassFile) == "")) {
			return errors.New("安装 Server 必须指定 -user 与 -password-file（或 -password-stdin）：" +
				"口令不收命令行明文（安装进程的命令行对同机任意进程可见）。" +
				"若本机已装过（配置存在），可省略这些参数直接升级")
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

	// 升级识别：配置存在即视为「已安装」，只替换二进制与服务定义，
	// **不动配置与数据**（服务端地址、注册密钥、账号、数据库原样保留）。
	// 要推倒重来必须显式加 -force，避免一次误操作抹掉现场。
	upgrade := !o.force && existingInstall(o, role)
	oldAgentID := ""
	if role == "agent" && upgrade {
		if obj, ok, _ := loadConfigObject(confPath); ok {
			oldAgentID, _ = obj["agent_id"].(string)
		}
	}

	fmt.Printf("\n=== %s %s ===\n", map[bool]string{true: "升级", false: "安装"}[upgrade], role)
	fmt.Printf("  服务名  : %s\n", name)
	fmt.Printf("  二进制  : %s\n", exePath)
	fmt.Printf("  配置数据: %s\n", dataDir)
	if upgrade {
		fmt.Printf("  模式    : 保留既有配置（如需清理重装请加 -force）\n")
	} else if o.force {
		fmt.Printf("  模式    : -force 已指定，按【清理重装】处理\n")
	}

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
	// 0755 在 Windows 上不产生任何 ACL：目录只会继承 %ProgramData% 的默认 ACE，
	// 而那里 BUILTIN\Users 是可读。配置、users.json、数据库、客户端私钥全在这个
	// 目录下，所以先把目录本身收成「仅管理员+SYSTEM+当前用户」并让它向后新建的
	// 子项继承，再往里写文件（H-9）。失败只告警：文件已写成功，回滚更糟。
	if err := hardenCredentialPath(dataDir, true); err != nil {
		fmt.Printf("  警告: %v\n", err)
	} else if err := hardenTree(dataDir); err != nil {
		fmt.Printf("  警告: %v\n", err)
	}
	// Server 角色：创建后台登录账号（用户名+密码，密码只存 PBKDF2 摘要）。
	// 升级或已有账号时保持不动，避免覆盖用户后来改过的密码。
	if role == "server" && !upgrade {
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
	hardenQuiet(confPath)
	fmt.Printf("  [2/5] 已写入配置 %s\n", confPath)
	if role == "agent" && upgrade {
		if o.agentID != "" {
			fmt.Printf("  [agent] 已应用本次输入的 Agent 名称/ID：%s\n", o.agentID)
		}
		if o.agentKey != "" {
			fmt.Printf("  [agent] 已应用本次输入的注册凭据（不回显）\n")
		}
		// 服务端以证书 CN 作为连接身份：agent_id 变了但旧证书还在，
		// 服务仍会以旧身份上线，配置看着改了、行为没改。
		if strings.TrimSpace(o.agentID) != "" && strings.TrimSpace(o.agentID) != strings.TrimSpace(oldAgentID) {
			n, cerr := resetAgentServerCerts(dataDir)
			if cerr != nil {
				return fmt.Errorf("Agent 名称/ID 已变更，但清理旧客户端证书失败: %w", cerr)
			}
			fmt.Printf("  [agent] 已清理旧客户端证书 %d 个（保留 client.key），服务将按新身份重新换取\n", n)
		}
	}

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

	// 7) 保活兜底：注册「开机自启 + 每 5 分钟巡检拉起」两个计划任务。
	//    服务能跑时它们是空转的安全网；服务不可用（容器/策略禁服务）时，
	//    它们就是唯一的自启与防杀通道。失败只告警，不影响安装成功。
	if err := installKeepAlive(role, exePath, confPath); err != nil {
		fmt.Printf("  [保活] 警告: 未注册计划任务: %v\n", err)
		fmt.Printf("         服务本身不受影响，但进程被结束后不会再自动拉起。\n")
		fmt.Printf("         把二进制装到仅管理员可写的目录（默认 %s）后重跑安装即可启用保活。\n", `C:\Program Files\MoCenter`)
	} else {
		watchdog, boot := keepAliveTasks(role)
		fmt.Printf("  [保活] 已注册计划任务：开机自启（%s）+ 每 5 分钟巡检（%s）\n", boot, watchdog)
	}
	return nil
}

// existingInstall 判断某角色是否已有安装（以配置文件是否存在为准）。
//
// 配置文件是「装过」的可靠标志：二进制可能被手工替换，而配置代表本机的
// 连接参数与数据目录，升级时必须原样保留。
func existingInstall(o options, role string) bool {
	confPath := filepath.Join(o.dataRoot, role, "mocenter-"+role+".json")
	if fi, err := os.Stat(confPath); err == nil && !fi.IsDir() {
		return true
	}
	// 兼容早期把配置放在安装目录的版本
	if fi, err := os.Stat(filepath.Join(o.installDir, "mocenter-"+role+".json")); err == nil && !fi.IsDir() {
		return true
	}
	return false
}

// loadConfigObject 读取既有配置为可合并的 map。
//
// 返回 exists=false 表示这是全新安装；文件存在但解析失败时返回错误 ——
// 升级时把一份读不懂的配置直接覆盖掉，比停下来让运维处理更糟。
func loadConfigObject(path string) (obj map[string]any, exists bool, err error) {
	b, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		return nil, false, nil
	}
	if rerr != nil {
		return nil, false, rerr
	}
	if uerr := json.Unmarshal(b, &obj); uerr != nil || obj == nil {
		return nil, true, fmt.Errorf("既有配置 %s 无法解析: %v（确认无需保留后可加 -force 清理重装）", path, uerr)
	}
	return obj, true, nil
}

// resetAgentServerCerts 删除由服务端签发的旧客户端证书（保留 client.key）。
//
// agent_id 变更后必须删：服务端以证书 CN 作为连接身份，旧证书会让新配置
// 形同虚设（界面仍显示旧身份）。私钥必须保留，否则服务端 TOFU 绑定会因
// 公钥指纹变化拒绝重签，只能再签发一枚新令牌才能恢复。
func resetAgentServerCerts(dataDir string) (int, error) {
	dir := filepath.Join(dataDir, "creds")
	removed := 0
	for _, name := range []string{"client.crt", "ca.crt"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
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
		existing, exists, err := loadConfigObject(path)
		if err != nil {
			return err
		}
		if !exists {
			if strings.TrimSpace(o.agentKey) == "" {
				return errors.New("安装 Agent 必须指定 -agent-key 或 -agent-key-file（取服务端 <data>\\agent.key 的内容）：" +
					"服务端要求 Agent 出示注册凭据，否则连接会被拒绝")
			}
			obj = map[string]any{
				"server": o.server,
				"daemon": true,
				// 首次启动用注册密钥换取客户端证书（服务端内置 CA 签发），
				// 换取成功后该字段会被清空、证书落到 creds\ 下 —— 运维无需手工准备证书。
				"enrollment_secret": strings.TrimSpace(o.agentKey),
			}
			// 只写一份凭据的条件：走 -agent-key-file 进来的是一台一份、用后即废的注册令牌。
			// 把它同时抄进长期字段 agent_key 没有任何好处 —— 令牌在服务端已作废，
			// "服务端换 CA 后拿 agent_key 自动重签"这条路救不回来，
			// 结果只是让一个凭据形状的值永久躺在配置里。
			// 反之，运维显式用 -agent-key 提供 <data>\agent.key 共享密钥时，
			// 它就是长期凭据，两处都写才符合那条恢复路径的本意。
			if strings.TrimSpace(o.agentKeyFile) == "" {
				obj["agent_key"] = strings.TrimSpace(o.agentKey)
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
		} else {
			// 升级：只覆盖本次显式给出的项，其余原样保留。
			// 旧实现会整份重写，且把"可省略参数"的升级路径卡在"必须给密钥"上；
			// 更关键的是，用户想用一次覆盖安装修正 agent_id / 换令牌时，
			// 输入会被直接丢掉 —— 现象就是"安装成功了，但服务仍以旧身份连"。
			obj = existing
			obj["daemon"] = true
			if s := strings.TrimSpace(o.server); s != "" {
				obj["server"] = s
			}
			if key := strings.TrimSpace(o.agentKey); key != "" {
				oldSecret, _ := existing["enrollment_secret"].(string)
				oldKey, _ := existing["agent_key"].(string)
				obj["enrollment_secret"] = key
				if strings.TrimSpace(o.agentKeyFile) == "" {
					// -agent-key：运维显式提供共享密钥，长期字段也要更新。
					obj["agent_key"] = key
				} else if oldSecret != "" && oldKey == oldSecret {
					// -agent-key-file：这是一台一份的一次性令牌，只应进 enrollment_secret。
					// 若旧配置把同一个值同时写进了两处（1.0.2 安装器如此），
					// 顺手删掉长期字段里那份死凭据；与 enrollment_secret 不同的
					// agent_key 可能是真正的共享密钥，不能碰。
					delete(obj, "agent_key")
				}
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
			if strings.TrimSpace(o.serverFingerprint) != "" {
				obj["server_fingerprint"] = strings.TrimSpace(o.serverFingerprint)
			}
		}
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	// 配置里有注册密钥，权限收紧。
	// 注意 0600 只对 Unix 有意义：Windows 上必须显式设 DACL，否则继承父目录宽授权
	// （既有安装的旧文件带着继承来的 ACE，只对目录设 DACL 不会追溯改它，
	//  所以调用方写完后还要补一次 hardenCredentialPath）。
	return os.WriteFile(path, b, 0600)
}

// hardenQuiet 收紧文件 DACL，失败只打印告警。
//
// 为什么不把它变成硬错误：DACL 设不上（卷未启用 ACL、网络共享、被安全软件拦下）
// 时文件已经写成功了，让整次安装因此失败回滚比带着告警装完更糟 —— 但绝不静默，
// 因为「凭据可被普通用户读到」正是这里要防的事。
func hardenQuiet(path string) {
	if err := hardenCredentialPath(path, false); err != nil {
		fmt.Printf("  警告: %v\n", err)
	}
}

// ensureServerAccount 在数据目录下创建/补全后台登录账号（users.json）。
//
// 为什么不把密码写进 mocenter-server.json：本项目的既有约定是「密码不落盘明文」
// （见 server/main.go：-password / -password-file 只用于首次创建账号，
// 随后以摘要持久化到 users.json；-password-file 是推荐写法，避免口令进命令行）。
// 因此这里直接写 users.json。
//
// 摘要算法：写 PBKDF2-HMAC-SHA256（pbkdf2$sha256$120000$<盐>$<密钥>），与服务端
// auth 包逐参数一致（见 password.go 的说明与两侧的成对已知答案测试）。
// 历史版本这里写的是单轮无盐 SHA-256；服务端仍兼容该格式并在首次登录成功后透明升级，
// 因此老安装不会被锁在门外，但这里会在检测到弱摘要时显式提醒（不等登录才暴露）。
// users.json 的 {"users":{...}} 结构必须与服务端保持一致。
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
			Users map[string]struct {
				PasswordHash string `json:"password_hash"`
			} `json:"users"`
		}
		if json.Unmarshal(b, &cur) == nil && len(cur.Users) > 0 {
			for name, u := range cur.Users {
				if looksLikeWeakHash(u.PasswordHash) {
					fmt.Printf("  提示: 账号 %s 的口令仍是旧版单轮 SHA-256 摘要，下次登录成功后会自动升级为 PBKDF2\n", name)
				}
			}
			hardenQuiet(path) // 老安装留下的文件继承过宽授权，顺手补紧
			return false, nil
		}
	}

	hash, err := hashServerPassword(password)
	if err != nil {
		return false, err
	}
	doc := map[string]any{
		"users": map[string]any{
			username: map[string]any{
				"username":      username,
				"password_hash": hash,
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
	hardenQuiet(path)
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

		// 先摘掉保活任务：否则看门狗会在卸载后把进程又拉起来
		if err := uninstallKeepAlive(r); err != nil {
			fmt.Printf("  警告: 删除保活计划任务失败: %v\n", err)
		} else {
			watchdog, boot := keepAliveTasks(r)
			fmt.Printf("已删除保活计划任务（%s / %s）\n", boot, watchdog)
		}

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
		watchdog, boot := keepAliveTasks(r)
		fmt.Printf("  保活任务: %s=%v  %s=%v\n", boot, taskExists(boot), watchdog, taskExists(watchdog))
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
