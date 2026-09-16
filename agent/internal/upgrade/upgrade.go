// Package upgrade 实现 Agent 的远程升级能力。
//
// 安全模型（加固后）：
//   - 下载地址必须是 https（除非显式 AllowUnsigned，见下）；
//   - SHA-256 摘要**必填**，空值直接拒绝（旧版 MD5 校验可被省略，等于没有校验）；
//   - 配置了 Ed25519 公钥时，签名必须存在且校验通过；
//   - 未配置公钥时默认**拒绝升级**，除非显式设置 AllowUnsigned —— 因为
//     「从服务端给的 URL 下载二进制并执行」本质上就是把代码执行权交给该通道，
//     没有签名就没有可验证的信任链。
//
// 失败时自动回滚到旧版本（保留备份），支持版本信息上报。
package upgrade

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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
	PubKey string
	// AllowUnsigned 显式允许无签名升级（危险，仅用于迁移期）。
	AllowUnsigned bool
}

// currentExecPath 返回当前可执行文件路径。
func currentExecPath() (string, error) {
	return os.Executable()
}

// loadPubKey 解析公钥（hex 或文件路径）。
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

// Perform 执行一次升级。pol 给出签名校验策略。
func Perform(req *protocol.UpgradeReq, pol Policy, report func(*protocol.UpgradeResp)) {
	resp := &protocol.UpgradeResp{UpgradeID: req.UpgradeID, Version: Version()}
	defer report(resp)

	// 1) 校验版本
	if req.Version != "" && req.Version == Version() && !req.Force {
		resp.OK = true
		resp.Message = "已是最新版本 " + Version()
		return
	}

	// 2) 校验下载地址：必须是 https
	if req.URL == "" {
		resp.Message = "未提供升级包下载地址（url）"
		return
	}
	if err := checkURL(req.URL, pol.AllowUnsigned); err != nil {
		resp.Message = err.Error()
		return
	}

	// 3) 摘要必须提供
	wantSum := strings.ToLower(strings.TrimSpace(req.SHA256))
	if wantSum == "" {
		resp.Message = "升级包必须提供 sha256 摘要（旧的 md5 校验已废弃且不可省略）"
		return
	}
	if len(wantSum) != 64 || !isHex(wantSum) {
		resp.Message = "升级包 sha256 摘要格式不合法"
		return
	}

	// 4) 签名策略
	pub, perr := loadPubKey(pol.PubKey)
	if perr != nil {
		resp.Message = "升级公钥不可用: " + perr.Error()
		return
	}
	sig := strings.TrimSpace(req.Signature)
	if pub == nil && !pol.AllowUnsigned {
		resp.Message = "未配置 -upgrade-pubkey，拒绝升级（如需允许无签名升级请显式加 -upgrade-allow-unsigned）"
		return
	}
	if pub != nil && sig == "" {
		resp.Message = "已配置升级公钥，但升级包未提供签名"
		return
	}

	// 5) 下载到临时文件
	exe, err := currentExecPath()
	if err != nil {
		resp.Message = "无法定位可执行文件: " + err.Error()
		return
	}
	dir := filepath.Dir(exe)
	tmpPath := filepath.Join(dir, ".mocenter-upgrade-"+fmt.Sprint(time.Now().UnixNano()))
	_ = os.Remove(tmpPath)

	if err := download(req.URL, tmpPath); err != nil {
		resp.Message = "下载升级包失败: " + err.Error()
		return
	}

	// 6) 校验 SHA-256（必做）
	sum, err := fileSHA256(tmpPath)
	if err != nil || sum != wantSum {
		_ = os.Remove(tmpPath)
		resp.Message = "升级包 sha256 校验失败"
		return
	}

	// 7) 校验 Ed25519 签名（配置了公钥时必做）
	if pub != nil {
		content, rerr := os.ReadFile(tmpPath)
		if rerr != nil {
			_ = os.Remove(tmpPath)
			resp.Message = "读取升级包失败: " + rerr.Error()
			return
		}
		sigBytes, derr := hex.DecodeString(sig)
		if derr != nil || !ed25519.Verify(pub, content, sigBytes) {
			_ = os.Remove(tmpPath)
			resp.Message = "升级包签名校验失败（拒绝执行）"
			return
		}
	}
	if pub == nil && pol.AllowUnsigned {
		log.Printf("[upgrade] 警告: 未配置公钥，本次升级**未做签名校验**（仅 sha256）")
	}

	// 8) 授予执行权限
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0755)
	}

	// 9) 备份旧二进制：先移走运行中的 exe（Windows 允许重命名运行中的文件，
	//    但不允许直接覆盖），再让新文件落位；失败时回滚。
	backupPath := exe + ".bak"
	_ = os.Remove(backupPath)
	if err := os.Rename(exe, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		resp.Message = "备份旧版本失败: " + err.Error()
		return
	}

	// 10) 新二进制落位（重试以避开杀软扫描瞬时锁定；仍失败则回退内容复制）
	if err := renameWithRetry(tmpPath, exe, 5, 2*time.Second); err != nil {
		if cerr := copyFile(tmpPath, exe); cerr != nil {
			_ = os.Remove(tmpPath)
			// 回滚备份
			_ = os.Rename(backupPath, exe)
			resp.Message = "替换二进制失败: " + err.Error()
			return
		}
		_ = os.Remove(tmpPath)
	}
	// Windows 下旧备份可能仍被占用，延迟清理交给系统；Linux 直接清理
	if runtime.GOOS != "windows" {
		_ = os.Remove(backupPath)
	}

	// 11) 以分离进程重新拉起新版本（保持原工作目录与参数），随后退出旧进程
	resp.OK = true
	resp.Message = "升级成功，正在重启"
	go func() {
		time.Sleep(500 * time.Millisecond)
		restartSelf(dir)
	}()
}

// checkURL 只允许 https；AllowUnsigned 时额外放行 http（并告警）。
func checkURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("升级包地址不合法: %v", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if allowInsecure {
			log.Printf("[upgrade] 警告: 使用明文 http 下载升级包 %s（仅在 -upgrade-allow-unsigned 下允许）", raw)
			return nil
		}
		return fmt.Errorf("升级包地址必须是 https（当前为 http）")
	default:
		return fmt.Errorf("升级包地址协议不被允许: %q", u.Scheme)
	}
}

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

// restartSelf 以分离方式重新启动自身（升级后调用），然后退出当前进程。
func restartSelf(dir string) {
	defer os.Exit(0)
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := platformSpawnSelf(exe)
	cmd.Dir = dir
	if f, err := os.OpenFile(filepath.Join(dir, "mocenter-agent.out"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[upgrade] 拉起新进程失败: %v", err)
	}
}

// download 下载文件到指定路径。
func download(url, path string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// fileSHA256 计算文件 SHA-256。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile 复制文件。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	log.Printf("[upgrade] 备份 %s -> %s", src, dst)
	return err
}
