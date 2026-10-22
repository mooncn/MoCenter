// Package winapp 提供 Windows 平台相关的运行形态支持：系统服务集成与进程唯一性保护。
//
// 设计取舍：本包在 agent 与 server 两个模块中**各存一份**（二者是独立 module），
// 与本仓库既有的 protocol.go 双份存放惯例一致。
package winapp

import (
	"crypto/sha1"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RedirectLogToFile 把标准日志重定向到文件（追加写 + 按大小轮转）。
//
// 服务模式下 stdout/stderr 没有去处，不重定向就等于完全没有日志 —— 出问题无从排查。
// 单份超过 8MB 就切成 <path>.1（只保留一份旧文件）：开 info 级别长时间运行时，
// "只在启动时截断"会让运行期文件无限增长，最终把磁盘写满。
func RedirectLogToFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	w, err := openRotating(path)
	if err != nil {
		return err
	}
	log.SetOutput(w)
	return nil
}

// maxLogBytes 单份日志文件的大小上限（测试可覆盖）。
var maxLogBytes int64 = 8 << 20

// rotatingWriter 按大小轮转的日志写入器。
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotating(path string) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxLogBytes {
		// 启动时就超限：先把旧文件挪走，保留一份现场再开新文件
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.f, w.size = f, fi.Size()
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > maxLogBytes {
		w.rotateLocked()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() {
	_ = w.f.Close()
	_ = os.Remove(w.path + ".1") // Windows 不允许 rename 覆盖已有文件
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		// 轮转失败不能连日志一起丢：继续写原文件（下次再试）
		if f, oerr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); oerr == nil {
			w.f = f
		}
		return
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	w.f, w.size = f, 0
}

// SingletonKey 由「角色 + 配置路径」生成互斥体名。
//
// 之所以把配置路径纳入：同一台机器上用不同配置跑两个实例是合理需求（例如本机测试），
// 只有"同一份配置"才应互斥。用路径哈希而非原文，避免互斥体名过长/含非法字符。
func SingletonKey(role, configPath string) string {
	abs := configPath
	if p, err := filepath.Abs(configPath); err == nil {
		abs = p
	}
	sum := sha1.Sum([]byte(strings.ToLower(filepath.Clean(abs))))
	return "MoCenter-" + role + "-" + hex.EncodeToString(sum[:4])
}
