// Package upgrade 实现 Agent 的远程升级能力。
//
// 安全模型（加固后）：
//   - **必须**配置 Ed25519 公钥：没有公钥就没有可验证的信任链，直接拒绝升级。
//     这里刻意**不留**"允许无签名升级"的开关 —— "从服务端给的 URL 下载二进制并执行"
//     等于把代码执行权交给那条通道，能关掉校验就等于没有校验；
//   - 下载地址只允许 https（http 明文可被中间人换包，与不校验等价）；
//   - SHA-256 摘要必填；
//   - 签名覆盖**目标版本号 + 包内容**，因此"签好名的包"与"要升到的版本"是绑死的，
//     拿一个合法的旧包配上新的 version 字段过不去；
//   - 包内容**只在内存里取一次**，摘要与验签用的是同一份字节，落盘的也是这份字节：
//     此前"算摘要读一次、验签再读一次"之间存在可替换窗口，校验过的内容与
//     最终执行的二进制可以不是同一份。
//
// 失败时自动回滚到旧版本（保留备份），支持版本信息上报。
package upgrade

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mocenter/agent/internal/protocol"
	"mocenter/agent/internal/version"
)

// Version 返回当前 Agent 版本（转发到 version 包，避免"升级模块以为自己还是
// 1.0.0 而实际上报已是 1.0.1"这类不一致）。
//
// 这里用函数而不是常量别名：version.Version 是 var（为了支持构建期
// -ldflags 注入），无法在编译期折叠成常量。
func Version() string { return version.Version }

// Policy 升级校验策略。
type Policy struct {
	// PubKey 升级包 Ed25519 公钥：hex 字符串或公钥文件路径。
	//
	// **没有默认值，也没有"跳过校验"的替代项**：留空即所有升级请求都被拒绝。
	// 要能用远程升级，就得先把签名公钥配上来（服务端 -upgrade-pubkey 会把它
	// 写进一键部署生成的 Agent 配置）。
	PubKey string
}

const (
	// signatureDomain 签名消息的前缀：把"这是升级包签名"钉进消息里，
	// 避免同一把密钥在别的用途下产出的签名被挪用。
	signatureDomain = "MoCenter-Upgrade-v1\n"
	// downloadTimeout 整个下载过程的总时限（含读取响应体）。
	downloadTimeout = 10 * time.Minute
)

// maxPackageBytes 升级包体积上限。Agent 二进制在几十 MB 量级，留足余量；
// 没有上限就等于让对端用一个流式响应把 Agent 打成 OOM。
//
// 用 var 只为测试能便宜地触发这条分支（真塞 256MB 进单测不合理）；
// 运行期没有任何入口能改它。
var maxPackageBytes = int64(256 << 20)

// currentExecPath 返回当前可执行文件路径。
func currentExecPath() (string, error) {
	return os.Executable()
}

// loadPubKey 解析公钥（hex 或文件路径）。返回 nil 表示未配置。
func loadPubKey(spec string) (ed25519.PublicKey, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	raw := spec
	if fi, err := os.Stat(spec); err == nil && !fi.IsDir() {
		b, rerr := os.ReadFile(spec)
		if rerr != nil {
			return nil, fmt.Errorf("读取公钥文件失败: %w", rerr)
		}
		raw = strings.TrimSpace(string(b))
	}
	raw = strings.TrimPrefix(raw, "ed25519:")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("公钥不是合法 hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("公钥长度应为 %d 字节，实际 %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// signedMessage 组装待签/待验消息：用途前缀 + 目标版本 + 包内容。
//
// 版本号必须进消息：否则同一个签名可以被配上任意 version 字段复用，
// 拿到通道的人就能用一份合法签名的**旧**包把机群降级钉死（M-10 的后半段）。
func signedMessage(target string, content []byte) []byte {
	buf := make([]byte, 0, len(signatureDomain)+len(target)+1+len(content))
	buf = append(buf, signatureDomain...)
	buf = append(buf, target...)
	buf = append(buf, '\n')
	return append(buf, content...)
}

// verifyPackage 用同一份内存内容做摘要比对与 Ed25519 验签。
func verifyPackage(pub ed25519.PublicKey, target, sigHex string, wantSum string, content []byte) error {
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != wantSum {
		return fmt.Errorf("升级包 sha256 校验失败（期望 %s，实际 %s）", wantSum, got)
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return errors.New("升级包签名格式不合法")
	}
	if !ed25519.Verify(pub, signedMessage(target, content), sigBytes) {
		return errors.New("升级包签名校验失败（拒绝执行）")
	}
	return nil
}

// decideUpgrade 判断这条指令相对当前版本该做什么。
//
// 返回 skip=true 表示无需动作；err 非空表示指令本身不可接受。
// 注意 force 只用于"同版本重装"，**不能**越过降级闸门：
// 旧版行为是 force 一开就能装任意版本，那正是把机群钉在旧包上的路径。
func decideUpgrade(target, current string, force, allowDowngrade bool) (skip bool, err error) {
	if strings.TrimSpace(target) == "" {
		return false, errors.New("升级指令必须带目标版本号（它参与签名校验）")
	}
	cmp, cerr := version.Compare(target, current)
	if cerr != nil {
		return false, fmt.Errorf("目标版本 %q 与当前版本 %q 无法比较: %w", target, current, cerr)
	}
	switch {
	case cmp == 0 && !force:
		return true, nil
	case cmp < 0 && !allowDowngrade:
		return false, fmt.Errorf("目标是降级到 %s（当前 %s），降级会绕开后续版本里的修复，已拒绝；确需回退请勾选允许降级", target, current)
	}
	return false, nil
}

// Perform 执行一次升级。pol 给出签名校验策略。
func Perform(req *protocol.UpgradeReq, pol Policy, report func(*protocol.UpgradeResp)) {
	resp := &protocol.UpgradeResp{UpgradeID: req.UpgradeID, Version: Version()}
	defer report(resp)

	// 1) 公钥是硬前提：拿不到公钥时后面每一步都没有意义，先失败在联网之前。
	pub, perr := loadPubKey(pol.PubKey)
	if perr != nil {
		resp.Message = "升级公钥不可用: " + perr.Error()
		return
	}
	if pub == nil {
		resp.Message = "未配置 -upgrade-pubkey，无法验签，拒绝升级（远程升级要求包内容由离线私钥签名）"
		return
	}

	// 2) 版本判定（含降级闸门）
	skip, derr := decideUpgrade(req.Version, Version(), req.Force, req.AllowDowngrade)
	if derr != nil {
		resp.Message = derr.Error()
		return
	}
	if skip {
		resp.OK = true
		resp.Message = "已是最新版本 " + Version()
		return
	}

	// 3) 下载地址：只允许 https
	if req.URL == "" {
		resp.Message = "未提供升级包下载地址（url）"
		return
	}
	if err := checkURL(req.URL); err != nil {
		resp.Message = err.Error()
		return
	}

	// 4) 摘要与签名必填
	wantSum := strings.ToLower(strings.TrimSpace(req.SHA256))
	if wantSum == "" {
		resp.Message = "升级包必须提供 sha256 摘要（旧的 md5 校验已废弃且不可省略）"
		return
	}
	if len(wantSum) != 64 || !isHex(wantSum) {
		resp.Message = "升级包 sha256 摘要格式不合法"
		return
	}
	sig := strings.TrimSpace(req.Signature)
	if sig == "" {
		resp.Message = "已配置升级公钥，但升级包未提供签名"
		return
	}
	if req.MD5 != "" {
		log.Printf("[upgrade] 提示: 指令携带的 md5 字段已废弃，不参与校验")
	}

	// 5) 取包：一次读入内存，摘要与验签共用这份字节
	content, err := fetchPackage(req.URL)
	if err != nil {
		resp.Message = "下载升级包失败: " + err.Error()
		return
	}
	if err := verifyPackage(pub, req.Version, sig, wantSum, content); err != nil {
		resp.Message = err.Error()
		return
	}

	exe, err := currentExecPath()
	if err != nil {
		resp.Message = "无法定位可执行文件: " + err.Error()
		return
	}
	if err := installBinary(exe, content, wantSum); err != nil {
		resp.Message = err.Error()
		return
	}

	// 6) 换入新版本运行（Unix 原地 exec 替换进程镜像；Windows 另起一份后退出旧进程）
	resp.OK = true
	resp.Message = "升级成功，正在重启"
	go func() {
		time.Sleep(500 * time.Millisecond)
		restartSelf(exe)
	}()
}

// checkURL 只允许 https。
//
// 这里不再有任何"顺带放行 http"的分支：明文 http 上的响应可被中间人整包替换，
// 而升级包是"下载即执行"的东西，放行它等于把签名校验旁路掉。
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("升级包地址不合法: %v", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		return fmt.Errorf("升级包地址必须是 https（http 明文可被中间人换包）")
	default:
		return fmt.Errorf("升级包地址协议不被允许: %q", u.Scheme)
	}
}

// fetchPackage 下载升级包到内存。
//
// 之所以不"先落盘再读"：那样校验用的内容与盘上最终被执行的内容就是两份东西，
// 中间还夹着一次可被替换的读取（本轮修掉的 TOCTOU 正是这个）。
func fetchPackage(rawurl string) ([]byte, error) {
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Get(rawurl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxPackageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxPackageBytes {
		return nil, fmt.Errorf("升级包超过 %d 字节上限", maxPackageBytes)
	}
	if len(content) == 0 {
		return nil, errors.New("升级包为空")
	}
	return content, nil
}

// installBinary 把**已验签的字节**换到 exe 位置：先落到同目录临时文件，
// 再备份旧二进制、原子改名过去；任何一步失败都回滚备份。
//
// 落盘写的是内存里那份 content（不是"再下一遍"或"从盘上读回来重校验"），
// 所以校验过的内容与被执行的内容是同一份字节。
func installBinary(exe string, content []byte, wantSum string) error {
	dir := filepath.Dir(exe)
	tmpPath := filepath.Join(dir, ".mocenter-upgrade-"+fmt.Sprint(time.Now().UnixNano()))

	// 落盘校验过的内容，并复核盘上的字节仍是同一份
	if err := writeVerified(tmpPath, content, wantSum); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("写入升级包失败: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0755)
	}

	// 备份旧二进制：先移走运行中的 exe（Windows 允许重命名运行中的文件，
	// 但不允许直接覆盖），再让新文件落位；失败时回滚。
	backupPath := exe + ".bak"
	_ = os.Remove(backupPath)
	if err := os.Rename(exe, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("备份旧版本失败: %w", err)
	}

	// 新二进制落位（重试以避开杀软扫描瞬时锁定；仍失败则回退为直接写校验过的字节）
	if err := renameWithRetry(tmpPath, exe, 5, 2*time.Second); err != nil {
		if cerr := writeVerified(exe, content, wantSum); cerr != nil {
			_ = os.Remove(tmpPath)
			_ = os.Rename(backupPath, exe) // 回滚备份
			return fmt.Errorf("替换二进制失败: %w", err)
		}
		_ = os.Remove(tmpPath)
	}
	// Windows 下旧备份可能仍被占用，延迟清理交给系统；其它平台直接清理
	if runtime.GOOS != "windows" {
		_ = os.Remove(backupPath)
	}
	return nil
}

// writeVerified 以独占方式写入内容，并复核盘上字节的摘要仍是校验过的那一份。
func writeVerified(path string, content []byte, wantSum string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(content); werr != nil {
		_ = f.Close()
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if hexSum(onDisk) != wantSum {
		return errors.New("落盘后的升级包摘要与校验值不一致（写入过程被干扰，放弃替换）")
	}
	return nil
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// isHex 判断是否为小写十六进制串。
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// renameWithRetry 带重试的 rename：新下载的二进制可能被杀软瞬时锁定，
// 稍候重试通常即可成功。
func renameWithRetry(from, to string, attempts int, wait time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(wait)
		}
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		log.Printf("[upgrade] rename 重试 %d/%d: %v", i+1, attempts, err)
	}
	return err
}

// restartSelf 切换到新版本二进制（升级成功后调用）。
//
// 参数必须是**换入之前**取到的可执行文件路径：安装时旧二进制会先改名成 .bak
// 再删除，那时 /proc/self/exe 已经是 "…(deleted)"，照它拉起必然 ENOENT ——
// Rocky8 实机撞到过，症状是日志里先"升级成功，正在重启"、紧跟一句
// "拉起新进程失败: fork/exec /usr/local/bin/mocenter-agent.bak"，
// 新版本最终只是靠 systemd 的 Restart=always 才回来。
func restartSelf(exe string) {
	if err := platformRelaunch(exe); err != nil {
		log.Printf("[upgrade] 换入新版本失败: %v（退出交给守护方按新二进制重新拉起）", err)
	}
	os.Exit(0)
}
