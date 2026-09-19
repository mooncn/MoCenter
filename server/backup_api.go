// 备份 / 恢复 与 一键部署 Agent 的后台接口。
//
// 两者都属于**高危运维操作**（能读到全部凭据、能往别的机器装东西），
// 因此统一遵守：
//
//   - 必须经过 guard（Token + IP 白名单 + 限流）；
//   - 每次操作都写审计（含操作者与来源 IP）；
//   - 请求体大小有上限，超限直接拒绝（避免把服务端内存打满）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mocenter/server/internal/agentdeploy"
	"mocenter/server/internal/auth"
	"mocenter/server/internal/backup"
	"mocenter/server/internal/version"
)

// maxDeployBody 一键部署请求体上限（私钥文本可能较长，给 512 KiB）。
const maxDeployBody = 512 << 10

// backupFileName 生成备份文件名（路径安全：只用时间戳，不含用户输入）。
func backupFileName() string {
	return "mocenter-backup-" + time.Now().Format("20060102-150405") + ".zip"
}

// backupHandler 备份 / 恢复接口。
//
//	GET  /api/backup                列出已有备份包（文件名/大小/时间）
//	GET  /api/backup?download=xxx   下载指定备份包
//	POST /api/backup                创建备份（返回清单）
//	POST /api/backup?action=restore&file=xxx  从已有备份恢复
//	POST /api/backup?action=inspect&file=xxx  查看包内清单（恢复前确认）
//
// 备份文件保存在数据目录的 backups/ 子目录下。**只允许按文件名访问**，
// 不做任意路径读取 —— 否则这里会变成一个任意文件下载漏洞。
func backupHandler(dataDir string, authz *auth.Auth, audit AuditFunc) http.HandlerFunc {
	backupDir := filepath.Join(dataDir, "backups")
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			// 下载
			if name := strings.TrimSpace(r.URL.Query().Get("download")); name != "" {
				p, ok := safeBackupPath(backupDir, name)
				if !ok {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				fi, err := os.Stat(p)
				if err != nil {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if audit != nil {
					audit(r, "下载备份包 "+name)
				}
				w.Header().Set("Content-Type", "application/zip")
				w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
				http.ServeContent(w, r, name, fi.ModTime(), mustOpen(p))
				return
			}
			// 列表
			items, err := listBackups(backupDir)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			writeJSON(w, map[string]interface{}{"ok": true, "items": items, "dir": "backups"})

		case http.MethodPost, http.MethodPut:
			action := strings.TrimSpace(r.URL.Query().Get("action"))
			switch action {
			case "", "create":
				res, err := backup.Create(dataDir, filepath.Join(backupDir, backupFileName()), version.Version)
				if err != nil {
					writeJSON(w, map[string]interface{}{"ok": false, "msg": "创建备份失败: " + err.Error()})
					return
				}
				if audit != nil {
					audit(r, fmt.Sprintf("创建数据备份 %s（%d 个文件, %s）",
						filepath.Base(res.Path), res.Files, humanSize(res.Size)))
				}
				writeJSON(w, map[string]interface{}{
					"ok": true, "msg": "备份已创建",
					"file": filepath.Base(res.Path), "size": res.Size,
					"files": res.Files, "entries": res.Entries, "skipped": res.Skipped,
					"duration": res.Duration,
				})

			case "inspect":
				name := strings.TrimSpace(r.URL.Query().Get("file"))
				p, ok := safeBackupPath(backupDir, name)
				if !ok {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				m, err := backup.ReadManifest(p)
				if err != nil {
					writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
					return
				}
				writeJSON(w, map[string]interface{}{"ok": true, "manifest": m})

			case "restore":
				name := strings.TrimSpace(r.URL.Query().Get("file"))
				p, ok := safeBackupPath(backupDir, name)
				if !ok {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				var opts backup.RestoreOptions
				opts.Overwrite = true
				opts.KeepBackupOfExisting = true
				if r.URL.Query().Get("overwrite") == "false" {
					opts.Overwrite = false
				}
				res, err := backup.Restore(p, dataDir, opts)
				if err != nil {
					// 恢复失败时把已有的 .before-restore 提示出来，
					// 让运维知道"旧文件还在，可以手工回退"。
					if audit != nil {
						audit(r, "恢复数据失败: "+name+" - "+err.Error())
					}
					writeJSON(w, map[string]interface{}{
						"ok": false,
						"msg": "恢复失败: " + err.Error() +
							"（被覆盖的旧文件已另存为 <文件名>.before-restore，可手工回退）",
					})
					return
				}
				if audit != nil {
					audit(r, fmt.Sprintf("从备份恢复数据 %s（%d 个文件）", name, len(res.Restored)))
				}
				writeJSON(w, map[string]interface{}{
					"ok": true, "msg": "恢复完成，请重启服务端使配置生效",
					"restored": res.Restored, "skipped": res.Skipped,
					"restart_required": true,
				})

			default:
				http.Error(w, "bad request", http.StatusBadRequest)
			}

		case http.MethodDelete:
			name := strings.TrimSpace(r.URL.Query().Get("file"))
			p, ok := safeBackupPath(backupDir, name)
			if !ok {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if err := os.Remove(p); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "删除失败: " + err.Error()})
				return
			}
			if audit != nil {
				audit(r, "删除备份包 "+name)
			}
			writeJSON(w, map[string]interface{}{"ok": true, "msg": "已删除"})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// safeBackupPath 把用户给的备份文件名解析为 backups 目录内的绝对路径。
//
// 只接受**纯文件名**：任何目录成分、.. 或绝对路径一律拒绝。
// 这是防"任意文件下载/删除"的关键一步：备份接口能下载文件，
// 若允许传路径就等于给了攻击者一个读取 /etc/shadow 的接口。
func safeBackupPath(dir, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name {
		return "", false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", false
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		return "", false
	}
	return filepath.Join(dir, name), true
}

// backupItem 备份列表项。
type backupItem struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Version string `json:"app_version,omitempty"`
	Files   int    `json:"files,omitempty"`
	Error   string `json:"error,omitempty"`
}

// listBackups 列出 backups 目录里的备份包（附带包内清单摘要）。
func listBackups(dir string) ([]backupItem, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []backupItem{}, nil
		}
		return nil, fmt.Errorf("读取备份目录失败: %w", err)
	}
	out := make([]backupItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		it := backupItem{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().Unix()}
		// 读清单失败不算致命（包可能损坏）：标注出来让运维知道。
		if m, merr := backup.ReadManifest(filepath.Join(dir, e.Name())); merr == nil {
			it.Version = m.AppVersion
			it.Files = len(m.Entries)
		} else {
			it.Error = merr.Error()
		}
		out = append(out, it)
	}
	return out, nil
}

// deployHandler 一键部署 Agent 接口。
//
//	POST /api/agents/deploy       执行部署（probe_only=true 时只探测）
//	GET  /api/agents/deploy       查看可用的 Agent 安装包与当前部署参数
//
// 凭证只在本请求内存中存在，不落盘、不入日志。
func deployHandler(deployer *agentdeploy.Deployer, authz *auth.Auth, audit AuditFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r, authz) {
			return
		}
		operator := authz.Operator(r.Header.Get("Authorization"))
		opIP := auth.LoginIPOf(r.RemoteAddr)

		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{"ok": true, "packages": deployer.PackageCandidates()})

		case http.MethodPost:
			// 限制请求体大小：这里会带私钥文本，但也不能无上限
			body, err := io.ReadAll(io.LimitReader(r.Body, maxDeployBody+1))
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(body) > maxDeployBody {
				writeJSON(w, map[string]interface{}{"ok": false,
					"msg": fmt.Sprintf("请求体过大（上限 %d KB）", maxDeployBody>>10)})
				return
			}
			var req agentdeploy.Request
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "msg": "请求格式不合法"})
				return
			}
			res, err := deployer.Deploy(&req, operator, opIP)
			if err != nil {
				// 参数校验类错误（不含凭证），可以直接回给前端
				writeJSON(w, map[string]interface{}{"ok": false, "msg": err.Error()})
				return
			}
			writeJSON(w, res)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// mustOpen 打开文件（失败时返回一个空 Reader，避免 nil panic；
// 调用方已用 Stat 确认过存在性，这里是纯粹的防御）。
func mustOpen(p string) io.ReadSeeker {
	f, err := os.Open(p)
	if err != nil {
		return strings.NewReader("")
	}
	return f
}

// humanSize 人类可读体积（后端日志/审计用）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
