// Package upgrade 实现 Agent 的远程升级能力。
//
// 支持从服务端指定的 URL 下载升级包，校验 MD5，替换二进制并重启。
// 失败时自动回滚到旧版本（保留备份），支持版本信息上报。
package upgrade

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mocenter/agent/internal/protocol"
)

// Version 当前 Agent 版本。
const Version = "1.0.0"

// currentExecPath 返回当前可执行文件路径。
func currentExecPath() (string, error) {
	return os.Executable()
}

// Perform 执行一次升级。
func Perform(req *protocol.UpgradeReq, report func(*protocol.UpgradeResp)) {
	resp := &protocol.UpgradeResp{UpgradeID: req.UpgradeID, Version: Version}
	defer report(resp)

	// 1. 校验版本
	if req.Version != "" && req.Version == Version && !req.Force {
		resp.OK = true
		resp.Message = "已是最新版本 " + Version
		return
	}

	// 2. 下载升级包到临时目录
	exe, err := currentExecPath()
	if err != nil {
		resp.Message = "无法定位可执行文件: " + err.Error()
		return
	}
	dir := filepath.Dir(exe)
	tmpPath := filepath.Join(dir, ".mocenter-upgrade-"+fmt.Sprint(time.Now().UnixNano()))
	_ = os.Remove(tmpPath)

	if req.URL == "" {
		// 未提供下载地址：无新二进制可替换。
		// 若指定了新版本但无包，无法完成替换，直接报错。
		resp.Message = "未提供升级包下载地址（url）"
		return
	}

	if err := download(req.URL, tmpPath); err != nil {
		resp.Message = "下载升级包失败: " + err.Error()
		return
	}
	// 校验 MD5
	if req.MD5 != "" {
		sum, err := fileMD5(tmpPath)
		if err != nil || sum != req.MD5 {
			_ = os.Remove(tmpPath)
			resp.Message = "升级包 MD5 校验失败"
			return
		}
	}
	// 授予执行权限
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0755)
	}

	// 3. 备份旧二进制
	backupPath := exe + ".bak"
	if err := copyFile(exe, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		resp.Message = "备份旧版本失败: " + err.Error()
		return
	}

	// 4. 替换二进制
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Remove(tmpPath)
		// 回滚备份
		_ = os.Rename(backupPath, exe)
		resp.Message = "替换二进制失败: " + err.Error()
		return
	}

	// 5. 立即重启以加载新版本（通过服务端或 daemon 重新拉起）
	resp.OK = true
	resp.Message = "升级成功，正在重启"
	go func() {
		time.Sleep(500 * time.Millisecond)
		// 触发守护进程自启：直接退出当前进程
		os.Exit(0)
	}()
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

// fileMD5 计算文件 MD5。
func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
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
