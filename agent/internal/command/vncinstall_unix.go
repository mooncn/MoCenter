//go:build linux

// Linux VNC 一键安装/启动（x11vnc），按「连接模式」分流：
//   - 交互会话模式：在当前用户会话中拉起 x11vnc。有图形会话（:0）时挂接真实桌面；
//     无桌面（headless/极简安装）时用 Xvfb 虚拟显示 + openbox + xterm 兜底。
//   - 服务模式：注册 systemd 系统服务（root），随系统启动并挂接控制台显示 :0，
//     可覆盖登录界面与锁屏（服务单元见 vncinstall_unit.go）。
//
// 两种模式均：随机口令 + 仅回环监听（配合 Agent 隧道回连）。
package command

import (
	"context"
	"fmt"
	"io"
	"mocenter/agent/internal/logx"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// vncScriptPath Linux VNC 启动脚本路径（幂等：重复执行不会重复拉起）。
func vncScriptPath() string { return filepath.Join(homeDir(), ".mocenter", "vnc-start.sh") }

// vncInstallLinux 安装并启动 VNC（x11vnc），写入随机口令、仅回环访问。
//
// 按 cfg.Mode 分流：
//   - VncModeService：注册 systemd 系统服务（root 运行），随系统启动，挂接控制台
//     显示 :0，可覆盖登录界面（GDM/LightDM/SDDM 等）与锁屏；
//   - 其它（默认）：在当前会话中直接拉起 x11vnc（最小权限），仅在用户登录后可用。
//
// 与 Windows 的对应关系：Windows 需靠 uvnc_service 抓 Winlogon「安全桌面」才能看到
// 登录界面；Linux/X11 下登录界面与用户会话共用同一个 X 显示 :0（不存在安全桌面隔离），
// 因此以 root 挂接 :0 即可同时覆盖登录界面、锁屏与用户桌面。
func vncInstallLinux(port int, cfg VncConfig) VncInstallResult {
	if cfg.Mode == VncModeService {
		return installX11VNCService(port, cfg)
	}
	return installX11VNCInteractive(port, cfg)
}

// installX11VNCInteractive 交互会话模式：在当前（用户）会话中拉起 x11vnc。
// 有图形会话（:0）时挂接真实桌面；无桌面（headless）时用 Xvfb 虚拟显示兜底。
func installX11VNCInteractive(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port, Mode: VncModeInteractive}

	// 0) 若此前装过服务模式，先停掉服务，避免两个实例争抢同一端口
	stopX11VNCService()

	// 1) 口令文件（8 字节 DES 混淆，x11vnc -rfbauth 直接读取）
	pw := generateRandomPassword()
	res.Password = pw
	passFile := filepath.Join(homeDir(), ".mocenter", "vncpasswd")
	if err := writeVncPasswdFile(passFile, pw); err != nil {
		res.Message = "创建 VNC 口令文件失败: " + err.Error()
		return res
	}

	// 2) 依赖：headless 场景需要整套虚拟桌面；x11vnc 是硬依赖。
	//    包管理器失败不直接返回 —— x11vnc 仍可能从服务端托管的自备二进制取到。
	missing := linuxVncMissingDeps()
	pkgErr := ""
	if len(missing) > 0 {
		if msg, ok := installLinuxPkgs(missing); !ok {
			pkgErr = msg
			logx.Infof("[vnc-install] 依赖安装未完全成功: %s", msg)
		}
	}
	if !x11vncAvailable() {
		msg, ok := installX11VNCFallback(cfg)
		if !ok {
			// 把包管理器的失败原因一并展示：只写日志的话前端看不到，用户无法判断原因
			res.Message = joinReasons(pkgErr, msg)
			return res
		}
	}

	// 3) 写启动脚本并后台执行（setsid 脱离 Agent 命令会话，防止被连带结束）
	base := filepath.Join(homeDir(), ".mocenter")
	if err := os.MkdirAll(base, 0755); err != nil {
		res.Message = "创建脚本目录失败: " + err.Error()
		return res
	}
	vncLog := filepath.Join(base, "vnc.log")
	launchLog := filepath.Join(base, "vnc-launch.log")
	script := buildVncStartScript(port, passFile, vncLog)
	if err := os.WriteFile(vncScriptPath(), []byte(script), 0755); err != nil {
		res.Message = "写入启动脚本失败: " + err.Error()
		return res
	}
	_ = exec.Command("bash", "-c",
		fmt.Sprintf("setsid nohup bash %s >%s 2>&1 &", vncScriptPath(), launchLog)).Run()

	// 4) 等端口就绪
	if waitPortOpen(port, 15*time.Second) {
		res.OK = true
		res.Message = "VNC(x11vnc) 已就绪，端口 " + itoa(port) + "。锁屏/登录界面不可见，需要时请改用服务模式。" + waylandWarning()
	} else {
		res.Message = "VNC 启动后端口未就绪，详见 " + vncLog + waylandWarning()
	}
	return res
}

// （服务单元模板与相关路径常量见 vncinstall_unit.go）

// installX11VNCService 以 systemd 系统服务方式运行 x11vnc（root），挂接控制台显示 :0，
// 从而覆盖登录界面与锁屏。
//
// 关键参数：
//   - -auth guess  自动探测 Xauthority（登录界面的 X 授权文件与用户会话不是同一个）
//   - -loop        X 服务重启（如注销后回到登录界面）时自动重连，不会退出
func installX11VNCService(port int, cfg VncConfig) VncInstallResult {
	res := VncInstallResult{Port: port, Mode: VncModeService}

	// 1) 口令
	pw := generateRandomPassword()
	res.Password = pw
	if err := writeVncPasswdFile(x11vncServicePassFile, pw); err != nil {
		res.Message = "创建 VNC 口令文件失败（需要 root）: " + err.Error()
		return res
	}

	// 2) 依赖：服务模式只需 x11vnc 本体（不需要 Xvfb —— 它要挂的是真实控制台显示）。
	//    包管理器失败不直接返回，仍尝试服务端托管的自备二进制。
	pkgErr := ""
	if !x11vncAvailable() {
		if msg, ok := installLinuxPkgs([]string{"x11vnc"}); !ok {
			pkgErr = msg
			logx.Errorf("[vnc-install] 包管理器安装 x11vnc 失败: %s", msg)
		}
	}
	if !x11vncAvailable() {
		msg, ok := installX11VNCFallback(cfg)
		if !ok {
			res.Message = joinReasons(pkgErr, msg)
			return res
		}
	}

	// 3) 结束可能抢占端口的旧实例（含交互模式脚本拉起的 x11vnc）
	_ = exec.Command("bash", "-c", "pkill -f '[x]11vnc.*mocenter' 2>/dev/null; true").Run()
	time.Sleep(1 * time.Second)

	// 4) 写 systemd 单元并启用（开机自启 + 立即启动）
	exe := x11vncPath()
	if exe == "" {
		res.Message = "未找到 x11vnc 可执行文件，无法生成 systemd 单元"
		return res
	}
	unit := buildX11VNCServiceUnit(port, exe)
	if err := os.MkdirAll(filepath.Dir(x11vncServicePath), 0755); err != nil {
		res.Message = "创建 systemd 目录失败: " + err.Error()
		return res
	}
	if err := os.WriteFile(x11vncServicePath, []byte(unit), 0644); err != nil {
		res.Message = "写入 systemd 服务单元失败（需要 root）: " + err.Error()
		return res
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		res.Message = "systemctl daemon-reload 失败: " + err.Error() + " " + tailLines(string(out), 3)
		return res
	}
	if out, err := exec.Command("systemctl", "enable", "--now", x11vncServiceName).CombinedOutput(); err != nil {
		res.Message = "启用 VNC 服务失败: " + err.Error() + " " + tailLines(string(out), 3)
		return res
	}

	// 5) 服务必须真的 active，再确认端口监听
	//    （只检查端口会被残留实例骗过，导致"报成功但连不上"）
	if !serviceActive(x11vncServiceName) {
		res.Message = "VNC 服务未进入 active 状态，请查看 systemctl status " + x11vncServiceName
		return res
	}
	if !waitPortOpen(port, 20*time.Second) {
		res.Message = "VNC 服务已启动但端口未监听，请查看 systemctl status " + x11vncServiceName + " 与 " + x11vncServiceLog
		return res
	}
	res.OK = true
	res.Message = "VNC(x11vnc) 服务模式已就绪，端口 " + itoa(port) + "。登录界面/锁屏亦可查看。口令请使用本次下发值。" + waylandWarning()
	return res
}

// stopX11VNCService 停止并禁用 systemd 服务（切回交互模式时用），避免两个实例争抢端口。
func stopX11VNCService() {
	if !fileExists(x11vncServicePath) {
		return
	}
	_ = exec.Command("systemctl", "disable", "--now", x11vncServiceName).Run()
	time.Sleep(500 * time.Millisecond)
}

// serviceActive 判断 systemd 单元是否处于 active 状态。
func serviceActive(name string) bool {
	out, err := exec.Command("systemctl", "is-active", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

// x11vncPath 返回 x11vnc 的绝对路径（systemd 单元要求绝对路径）；未找到时返回空串。
func x11vncPath() string {
	if p, err := exec.LookPath("x11vnc"); err == nil && p != "" {
		return p
	}
	cands := []string{
		"/usr/bin/x11vnc",
		"/usr/local/bin/x11vnc",
		"/root/.local/bin/x11vnc",
		filepath.Join(homeDir(), ".local", "bin", "x11vnc"),
	}
	for _, p := range cands {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// x11vncAvailable 判断 x11vnc 是否可用（在 PATH 中，或位于已知安装位置）。
// 不能只用 hasBin：服务端下发的二进制会被装到 ~/.local/bin，该目录未必在 PATH 里。
func x11vncAvailable() bool {
	return hasBin("x11vnc") || x11vncPath() != ""
}

// linuxSessionIsWayland 判断当前图形会话是否为 Wayland。
func linuxSessionIsWayland() bool {
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
		return true
	}
	// Wayland 合成器会在 /run/user/<uid>/ 下创建 wayland-0 之类的 socket；
	// Agent 多由 systemd 以 root 运行，环境变量不可靠，故用该特征兜底判断。
	matches, _ := filepath.Glob("/run/user/*/wayland-*")
	return len(matches) > 0
}

// waylandWarning 返回 Wayland 会话下的告警文案（非 Wayland 时返回空串）。
// x11vnc 只能挂接 X11 显示；Wayland 下没有可挂接的 :0（或只有空的 Xwayland），会黑屏。
func waylandWarning() string {
	if !linuxSessionIsWayland() {
		return ""
	}
	return " ⚠️ 检测到 Wayland 会话：x11vnc 只能挂接 X11，画面可能黑屏；" +
		"需改用 Xorg 会话（如 GDM 在 /etc/gdm3/custom.conf 设 WaylandEnable=false 后重启）。"
}

// linuxVncMissingDeps 返回缺失的依赖命令列表。
func linuxVncMissingDeps() []string {
	var missing []string
	for _, bin := range []string{"x11vnc"} {
		if !hasBin(bin) {
			missing = append(missing, bin)
		}
	}
	// headless（无 :0 图形会话）时需要整套虚拟桌面
	if !hasDisplay0() {
		for _, bin := range []string{"Xvfb", "openbox", "xterm"} {
			if !hasBin(bin) {
				missing = append(missing, bin)
			}
		}
	}
	return missing
}

// hasDisplay0 判断是否存在本地图形会话（X socket :0）。
func hasDisplay0() bool {
	if os.Getenv("DISPLAY") != "" && fileExists("/tmp/.X11-unix/X0") {
		return true
	}
	return fileExists("/tmp/.X11-unix/X0")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// installLinuxPkgs 用系统包管理器安装依赖。
//
// 流程：先按系统类型**同步包列表**，再安装。同步是必要的 —— 索引过期会导致
// "包明明存在却 404"（实测 Kali 镜像 index/pool 不同步即如此）。
//
// 两条安全约束（来自一次踩坑）：
//   - 同步步骤一律加 `timeout`：网络不通时不能长时间挂起，否则前端会一直卡在"安装中"；
//   - 只做一次尝试、不叠加重试链：失败即返回，把原因交给调用方展示给用户。
func installLinuxPkgs(bins []string) (string, bool) {
	// 依赖命令 -> 包名映射
	pkgOf := map[string]string{
		"x11vnc":  "x11vnc",
		"Xvfb":    "xorg-x11-server-Xvfb",
		"openbox": "openbox",
		"xterm":   "xterm",
	}
	var pkgs []string
	for _, b := range bins {
		if p, ok := pkgOf[b]; ok {
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) == 0 {
		return "", true
	}
	pkgList := quoteJoin(pkgs, " ")
	binList := quoteJoin(bins, " ")
	script := `
# 0) 依赖已全部就绪则直接跳过（幂等）
ready=1
for b in ` + binList + `; do
  command -v "$b" >/dev/null 2>&1 || ready=0
done
[ "$ready" = 1 ] && exit 0

need_epel=0
for p in ` + pkgList + `; do
  case "$p" in x11vnc) need_epel=1;; esac
done

# 1) 按系统类型同步包列表（timeout 兜底，避免源不可达时长时间挂起）
# 2) 再执行安装
if command -v apt-get >/dev/null 2>&1; then
  timeout 90 apt-get update -qq
  apt-get install -y ` + mapDebPkgs(pkgs) + `
elif command -v dnf >/dev/null 2>&1; then
  [ "$need_epel" = 1 ] && (timeout 90 dnf install -y epel-release || true)
  timeout 90 dnf -y makecache
  dnf install -y ` + pkgList + `
elif command -v yum >/dev/null 2>&1; then
  [ "$need_epel" = 1 ] && (timeout 90 yum install -y epel-release || true)
  timeout 90 yum -y makecache
  yum install -y ` + pkgList + `
else
  echo "no package manager"; exit 1
fi`
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	if err != nil || !linuxDepsReady(bins) {
		tail := tailLines(string(out), 5)
		return fmt.Sprintf("依赖安装失败（%v）：可手动安装 %s。详情: %s", err, pkgList, tail), false
	}
	return "", true
}

// mapDebPkgs 将包名映射为 Debian 系等价包。
func mapDebPkgs(pkgs []string) string {
	var out []string
	for _, p := range pkgs {
		switch p {
		case "xorg-x11-server-Xvfb":
			out = append(out, "xvfb")
		default:
			out = append(out, p)
		}
	}
	return quoteJoin(out, " ")
}

func linuxDepsReady(bins []string) bool {
	for _, b := range bins {
		if !hasBin(b) {
			return false
		}
	}
	return true
}

func quoteJoin(list []string, sep string) string {
	s := ""
	for i, v := range list {
		if i > 0 {
			s += sep
		}
		s += v
	}
	return s
}

func tailLines(s string, n int) string {
	lines := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// （启动脚本模板见 vncinstall_unit.go）

// installX11VNCFallback 包管理器无法提供 x11vnc 时的兜底：从服务端拉取运维自备的二进制。
//
// 为什么没有「公网直下」这一档：LibVNC/x11vnc 官方只发布源码，不存在可直下的官方成品；
// 而改从第三方站点拉二进制，恰恰与「防劫持 / 防篡改」的目标相悖。
// 因此这里只认服务端托管的、由运维自行构建或自备的二进制。
func installX11VNCFallback(cfg VncConfig) (string, bool) {
	const hint = "可手动安装（Debian/Ubuntu：apt-get install -y x11vnc；" +
		"RHEL/Rocky/Alma：dnf install -y epel-release && dnf install -y x11vnc）"

	if cfg.FetchPackage == nil {
		return "未找到 x11vnc，且当前环境无法自动安装。" + hint, false
	}
	name := x11vncPkgName()
	path, err := cfg.FetchPackage(name)
	if err != nil {
		return "未找到 x11vnc，且服务端未托管 " + name + "（" + err.Error() + "）。" + hint, false
	}
	if !installX11VNCBinary(path) {
		return "服务端下发的 " + name + " 安装失败（可能不是可执行文件或架构不匹配）。" + hint, false
	}
	return "已通过服务端下发的 x11vnc 二进制完成安装。", true
}

// joinReasons 把若干失败原因拼成一条可读消息（跳过空串，避免出现悬空分隔符）。
func joinReasons(reasons ...string) string {
	var parts []string
	for _, r := range reasons {
		if r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "；")
}

// installX11VNCBinary 把（服务端下发的）x11vnc 二进制安装到用户目录并赋予可执行权限。
// 安装前校验 ELF 魔数，避免把错误页/被篡改的内容当成二进制装上去。
func installX11VNCBinary(src string) bool {
	if src == "" {
		return false
	}
	if !isELFExecutable(src) {
		logx.Errorf("[vnc-install] %s 不是 ELF 可执行文件（可能架构不匹配或内容被替换），已拒绝安装", src)
		return false
	}
	dst := filepath.Join(homeDir(), ".local", "bin", "x11vnc")
	// 用 os/exec 的直接调用而不是 `bash -c "mkdir -p '...' && cp '...' '...'"`（L-9）：
	// 单引号包裹只在"路径里没有单引号"时成立，而 HOME 是本机可控变量。
	// 这里是纯文件操作，本来就不需要 shell。
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		logx.Errorf("[vnc-install] 创建 %s 失败: %v", filepath.Dir(dst), err)
		return false
	}
	if err := copyExecutable(src, dst); err != nil {
		logx.Errorf("[vnc-install] 安装 x11vnc 二进制失败: %v", err)
		return false
	}
	_ = os.Remove(src)
	return true
}

// copyExecutable 把 src 复制到 dst 并赋予可执行权限（0755）。
//
// O_NOFOLLOW：dst 是用户目录下的可预测路径，若被预先放成指向其它文件的符号链接，
// 直接 O_TRUNC 会以 Agent 的权限覆盖那个目标。拒绝跟随链接即可堵住这条本机提权链。
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0755)
	if err != nil {
		return err
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		_ = out.Close()
		return cerr
	}
	return out.Close()
}

// hasBin 判断命令是否可在 PATH 中解析到。
// 用 exec.LookPath 而不是 `bash -c "command -v <name>"`（L-9）：后者把 name 拼进 shell
// 命令行，name 里带一个 `;` 或 `$(...)` 就成了本机命令执行 —— 而它只是想知道"有没有这个程序"。
func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}
