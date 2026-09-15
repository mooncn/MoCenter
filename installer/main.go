//go:build windows

// MoCenter Windows 安装器（mocenter-deploy.exe）。
//
// 设计目标：
//   - 单文件分发：agent / server 二进制用 go:embed 内嵌，用户只需拿到一个 exe；
//   - 真服务：注册为 Windows 服务（走 SCM 握手），并配置失败自动重启；
//   - 固定位置：二进制进 %ProgramFiles%\MoCenter，配置与数据进 %ProgramData%\MoCenter\<角色>，
//     与二进制分离，升级不覆盖配置；
//   - 幂等：重复安装会先停掉旧服务、覆盖二进制、重建配置，不留残留；
//   - 单实例：被安装的程序自带命名互斥体保护，避免"服务 + 手工启动"两个实例并存。
//
// 便携形态仍然保留：不安装时直接把 mocenter-agent.exe 拷走双击即可运行，
// 配置会落在 exe 同目录。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// options 安装/卸载参数。
type options struct {
	role       string // agent | server | all
	server     string // Agent 连接的服务端地址
	agentID    string
	proxyIP    string
	agentKey   string // Agent 注册预共享密钥（与服务端 agent.key 相同）
	adminUser  string // Server 的登录用户名
	adminPass  string // Server 的登录密码
	listen     string // Server 的 Agent TCP 监听
	httpAddr   string // Server 的 HTTP 监听
	webDir     string // Server 托管的 Web 静态资源目录
	installDir string // 二进制目录
	dataRoot   string // 数据根目录
	extra      string // 追加到服务命令行的额外参数
	purge      bool   // 卸载时是否删除数据
	keepBins   bool   // 卸载时保留二进制（由外部安装器负责删除时用）
}

func main() {
	log.SetFlags(0) // 安装器输出保持干净，不带时间戳

	o := options{}
	flag.StringVar(&o.role, "role", "agent", "安装角色：agent | server | all")
	flag.StringVar(&o.server, "server", "", "Agent 要连接的服务端地址 host:port（role 含 agent 时必填）")
	flag.StringVar(&o.agentID, "agentid", "", "Agent 唯一标识（留空则由 Agent 自动取硬件 UUID/主机名）")
	flag.StringVar(&o.proxyIP, "proxy-ip", "", "国内加速代理前缀（可选）")
	flag.StringVar(&o.agentKey, "agent-key", "", "Agent 注册预共享密钥（取服务端 <data>\\agent.key 的内容；role 含 agent 时必填）")
	flag.StringVar(&o.adminUser, "user", "", "Server 的登录用户名（role 含 server 时必填）")
	flag.StringVar(&o.adminPass, "password", "", "Server 的登录密码（role 含 server 时必填）")
	flag.StringVar(&o.listen, "listen", ":9000", "Server 的 Agent TCP 监听地址")
	flag.StringVar(&o.httpAddr, "http", ":8080", "Server 的 REST/WebSocket 监听地址")
	flag.StringVar(&o.webDir, "web", "", "Server 托管的 Web 静态资源目录（留空则不托管前端）")
	flag.StringVar(&o.installDir, "dir", `C:\Program Files\MoCenter`, "安装目录（存放二进制）")
	flag.StringVar(&o.dataRoot, "data", `C:\ProgramData\MoCenter`, "数据根目录（配置/数据库/日志）")
	flag.StringVar(&o.extra, "extra", "", "追加到服务命令行的额外参数（如 -tls -ca-cert C:\\ca.crt）")
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
	case "install":
		err = doInstall(o)
	case "uninstall":
		err = doUninstall(o)
	case "status":
		err = doStatus(o)
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
  mocenter-deploy.exe uninstall [选项]    停止并删除服务
  mocenter-deploy.exe status    [选项]    查看服务与路径状态

示例:
  REM 在被控机上安装 Agent（-agent-key 取总控机 <data>\agent.key 的内容）
  mocenter-deploy.exe install -role agent -server your.server.com:9000 -agent-key <注册密钥>

  REM 在总控机上安装 Server
  mocenter-deploy.exe install -role server -user admin -password <登录密码>

  REM 一台机器同时装两者
  mocenter-deploy.exe install -role all -server 127.0.0.1:9000 -user admin -password <登录密码>

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
