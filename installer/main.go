//go:build windows

// MoCenter Windows 安装器（mocenter-deploy.exe）。
//
// 设计目标：
//   - 单文件分发：agent / server 二进制用 go:embed 内嵌，用户只需拿到一个 exe；
//   - 真服务：注册为 Windows 服务（走 SCM 握手），并配置失败自动重启；
//   - 固定位置：二进制进 %ProgramFiles%\MoCenter，配置与数据进 %ProgramData%\MoCenter\<角色>，
//     与二进制分离，升级不覆盖配置；
//   - 幂等：重复安装会先停掉旧服务、覆盖二进制、重建配置，不留残留；
//   - 单实例：被安装的程序自带单实例保护（Windows 命名互斥体 / Linux·macOS flock），
//     避免"服务 + 手工启动 + 定时任务保活"两个实例并存。
//
// 便携形态仍然保留：不安装时直接把 mocenter-agent.exe 拷走双击即可运行，
// 配置会落在 exe 同目录。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// options 安装/卸载参数。
type options struct {
	role          string // agent | server | all
	server        string // Agent 连接的服务端地址
	agentID       string
	agentUUID     string // 设备硬件 UUID（设备身份；留空由 Agent 自动探测并持久化）
	proxyIP       string
	agentKey      string // Agent 注册预共享密钥（与服务端 agent.key 相同）
	adminUser     string // Server 的登录用户名
	adminPass     string // Server 的登录密码
	adminPassFile string // Server 登录密码所在文件（优先于 adminPass）
	listen        string // Server 的 Agent TCP 监听
	httpAddr      string // Server 的 HTTP 监听
	webDir        string // Server 托管的 Web 静态资源目录
	installDir    string // 二进制目录
	dataRoot      string // 数据根目录
	extra         string // 追加到服务命令行的额外参数
	// serverFingerprint 服务端身份公钥指纹（免 CA 文件校验服务端；可选）。
	serverFingerprint string
	// serverName 部署身份名（写入服务端 CA，便于区分多套总控；可选）。
	serverName string
	force      bool   // 安装/卸载时强制清理既有配置与数据后重来（默认升级模式，保留配置）
	runExe     string // run 子命令：要拉起的可执行文件（默认按角色推导）
	runConfig  string // run 子命令：目标进程的配置文件路径
	purge      bool   // 卸载时是否删除数据
	keepBins   bool   // 卸载时保留二进制（由外部安装器负责删除时用）
}

func main() {
	log.SetFlags(0) // 安装器输出保持干净，不带时间戳

	o := options{}
	flag.StringVar(&o.role, "role", "agent", "安装角色：agent | server | all")
	flag.StringVar(&o.server, "server", "", "Agent 要连接的服务端地址 host:port（role 含 agent 时必填）")
	flag.StringVar(&o.agentID, "agentid", "", "Agent 名称标识（可变，仅用于显示与路由；留空则回退为设备 UUID/主机名）")
	flag.StringVar(&o.agentUUID, "uuid", "", "设备硬件 UUID（设备身份，留空则由 Agent 自动探测并写入配置）")
	flag.StringVar(&o.proxyIP, "proxy-ip", "", "国内加速代理前缀（可选）")
	flag.StringVar(&o.agentKey, "agent-key", "", "Agent 注册预共享密钥（取服务端 <data>\\agent.key 的内容；role 含 agent 时必填）")
	flag.StringVar(&o.adminUser, "user", "", "Server 的登录用户名（role 含 server 时必填）")
	flag.StringVar(&o.adminPass, "password", "", "Server 的登录密码（role 含 server 时必填）")
	flag.StringVar(&o.adminPassFile, "password-file", "", "从文件读取 Server 登录密码（优先于 -password；图形安装程序走这条路径，避免口令出现在命令行里）")
	flag.StringVar(&o.listen, "listen", ":9000", "Server 的 Agent TCP 监听地址")
	flag.StringVar(&o.httpAddr, "http", ":8080", "Server 的 REST/WebSocket 监听地址")
	flag.StringVar(&o.webDir, "web", "", "Server 托管的 Web 静态资源目录（留空则不托管前端）")
	flag.StringVar(&o.installDir, "dir", `C:\Program Files\MoCenter`, "安装目录（存放二进制）")
	flag.StringVar(&o.dataRoot, "data", `C:\ProgramData\MoCenter`, "数据根目录（配置/数据库/日志）")
	flag.StringVar(&o.extra, "extra", "", "追加到服务命令行的额外参数")
	flag.StringVar(&o.serverFingerprint, "server-fingerprint", "", "服务端身份公钥指纹（可选；填了就不需要 CA 文件，也不受服务端换 IP/证书影响）")
	flag.StringVar(&o.serverName, "server-name", "", "部署身份名（可选；写入服务端 CA，用于区分多套总控）")
	flag.BoolVar(&o.force, "force", false, "安装/卸载时强制清理既有配置与数据后重装（默认升级模式：保留配置）")
	flag.StringVar(&o.runExe, "exe", "", "run 子命令内部使用：要拉起的可执行文件路径")
	flag.StringVar(&o.runConfig, "config", "", "run 子命令内部使用：目标进程的配置文件路径")
	flag.BoolVar(&o.purge, "purge", false, "卸载时同时删除配置与数据（默认保留）")
	flag.BoolVar(&o.keepBins, "keep-binaries", false, "卸载时保留二进制（由外层安装器负责删除时使用）")

	flag.Usage = usage

	// 子命令必须先摘掉，再解析其余参数 —— 顺序不能反过来。
	//
	// Go 的 flag 包在遇到**第一个非旗标参数**时就停止解析，其后的一切都原样进入
	// flag.Args()。而本工具的习惯用法是「子命令在前」：
	//     mocenter-deploy.exe install -role agent -server host:9000
	// 若直接调用 flag.Parse()，install 之后的所有旗标都会被整段丢弃、静默回落到
	// 默认值（role=agent、server=""），于是必然卡在「必须指定 -server」上，
	// 表现为「服务注册失败（退出码 1）」却看不出原因。
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	_ = flag.CommandLine.Parse(args)
	if cmd == "" {
		// 兼容「旗标在前」的写法：mocenter-deploy.exe -role server install
		cmd = "install"
		if flag.NArg() > 0 {
			cmd = flag.Arg(0)
		}
	}

	var err error
	switch cmd {
	case "install", "upgrade":
		// upgrade 与 install 同路径：是否为升级由"配置是否存在"决定
		err = doInstall(o)
	case "uninstall":
		err = doUninstall(o)
	case "status":
		err = doStatus(o)
	case "run":
		// 内部动作：由计划任务（保活/开机自启）调用，拉起工作进程。
		// 包一层是为了让子进程带 CREATE_NO_WINDOW —— schtasks /tr 直接起
		// 控制台程序会在用户会话里闪出黑窗口。
		err = doRun(o)
	case "keepalive":
		// 内部动作：保活巡检（进程不在则拉起）。
		err = doKeepalive(o)
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
		logFailure(cmd, err)
		os.Exit(1)
	}
}

// logFailure 把失败原因追加到引擎同目录的日志文件。
//
// 为什么必须这么做：图形安装器（Inno）以隐藏窗口调用本引擎，stdout/stderr 没有
// 任何去处，用户只能看到一句「退出码 1」—— 排错完全没有依据。落盘一份日志后，
// 无论谁调用（Inno / 计划任务 / CI），失败原因都留得下来。
func logFailure(cmd string, err error) {
	path := filepath.Join(exeDir(), "mocenter-deploy.log")
	f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if ferr != nil {
		return // 日志写不了不是主要错误，不要覆盖真正的失败原因
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  cmd=%s  argv=%v\n  [错误] %v\n",
		time.Now().Format("2006-01-02 15:04:05"), cmd, os.Args[1:], err)
}

// exeDir 返回可执行文件所在目录；取不到时退回当前工作目录。
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func usage() {
	fmt.Fprintf(os.Stderr, `MoCenter Windows 安装器

用法:
  mocenter-deploy.exe install   [选项]    安装并注册为 Windows 服务（默认动作）
  mocenter-deploy.exe upgrade   [选项]    升级：只替换二进制与服务，保留配置（等同 install）
  mocenter-deploy.exe uninstall [选项]    停止并删除服务
  mocenter-deploy.exe status    [选项]    查看服务、保活任务与路径状态
  mocenter-deploy.exe run       [选项]    内部动作：拉起工作进程（供计划任务调用，勿手工使用）
  mocenter-deploy.exe keepalive [选项]    内部动作：巡检并拉起（供计划任务调用，勿手工使用）

升级 semantics:
  · 检测到 <数据目录>\<角色>\mocenter-<角色>.json 已存在时按**升级**处理：
    只替换二进制与服务定义，账号/连接参数/数据库全部保留；
  · 升级时无需重复提供 -server / -agent-key / -user / -password；
  · 要清理重装（删除既有配置与数据）需显式加 -force。

保活:
  安装时会额外注册两个计划任务（以 SYSTEM 运行）：
  · <角色>Boot       开机（不可用时改为登录）自启；
  · <角色>KeepAlive  每 5 分钟巡检，进程不在就拉起。
  服务能跑时它们是安全网；容器/禁用服务的环境里它们是唯一的自启与防杀通道。
  卸载时会一并删除。

示例:
  REM 在被控机上安装 Agent（-agent-key 取总控机 <data>\agent.key 的内容）
  mocenter-deploy.exe install -role agent -server your.server.com:9000 -agent-key <注册密钥>

  REM 在总控机上安装 Server
  mocenter-deploy.exe install -role server -user admin -password-file <0600 口令文件>   # 推荐
  mocenter-deploy.exe install -role server -user admin -password <登录密码>            # 不推荐：口令会出现在命令行里

  REM 一台机器同时装两者
  mocenter-deploy.exe install -role all -server 127.0.0.1:9000 -user admin -password-file <0600 口令文件>

  REM 卸载（保留配置与数据）
  mocenter-deploy.exe uninstall -role all

选项:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
说明:
  - 需以管理员身份运行（注册服务、写入 Program Files 都需要）。
  - 子命令写在最前，例如 install -role agent -server host:9000。
  - 配置与数据固定放在 %%ProgramData%%\MoCenter\<角色>\，卸载默认保留。
  - 任何失败都会把原因追加到本引擎同目录的 mocenter-deploy.log ——
    图形安装器以隐藏窗口调用本程序，那里是唯一能看到真实原因的地方。
  - 便携形态不受影响：直接运行 mocenter-agent.exe 即可，配置落在 exe 同目录。
`)
}

// doRun 内部动作：以隐藏窗口拉起工作进程（供计划任务「开机自启」调用）。
//
// 命令行为：mocenter-deploy.exe run -role <角色> -exe <路径> -config <配置路径>
// 单独实现它的意义：schtasks 直接启动控制台程序会在用户会话里闪出黑窗口，
// 经由本引擎创建 CREATE_NO_WINDOW 子进程即可完全隐藏。
func doRun(o options) error {
	role := strings.TrimSpace(o.role)
	if role != "agent" && role != "server" {
		return fmt.Errorf("run 需要 -role agent|server，实得 %q", o.role)
	}
	exe := strings.TrimSpace(o.runExe)
	if exe == "" {
		exe = filepath.Join(o.installDir, "mocenter-"+role+".exe")
	}
	args := []string{}
	if conf := strings.TrimSpace(o.runConfig); conf != "" {
		args = append(args, "-config", conf)
	} else {
		args = append(args, "-config", filepath.Join(o.dataRoot, role, "mocenter-"+role+".json"))
	}
	if role == "server" {
		args = append(args, "-data", filepath.Join(o.dataRoot, role))
	}
	if s := strings.TrimSpace(o.extra); s != "" {
		args = append(args, strings.Fields(s)...)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = hiddenWindowAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("拉起 %s 失败: %v（计划任务会按间隔重试）", exe, err)
	}
	fmt.Printf("已拉起 %s（pid=%d）\n", exe, cmd.Process.Pid)
	// 立即返回：进程交由系统接管。若已有实例在跑，目标程序自身的单实例保护
	// 会让新进程安静退出，不会出现两个实例并存。
	return nil
}

// doKeepalive 内部动作：巡检并在必要时拉起（供计划任务每 5 分钟调用）。
func doKeepalive(o options) error {
	role := strings.TrimSpace(o.role)
	if role != "agent" && role != "server" {
		return fmt.Errorf("keepalive 需要 -role agent|server，实得 %q", o.role)
	}
	exe := strings.TrimSpace(o.runExe)
	if exe == "" {
		exe = filepath.Join(o.installDir, "mocenter-"+role+".exe")
	}
	conf := strings.TrimSpace(o.runConfig)
	if conf == "" {
		conf = filepath.Join(o.dataRoot, role, "mocenter-"+role+".json")
	}
	if err := runKeepAlive(role, exe, conf); err != nil {
		// 巡检失败不算致命：下一个周期会重试，日志记录即可
		fmt.Printf("巡检未完成: %v\n", err)
		return nil
	}
	return nil
}
