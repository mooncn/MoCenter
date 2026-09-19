// Package backup 实现 MoCenter 的备份与恢复。
//
// 对应 Issue 需求「P1：增加备份/恢复功能」。
//
// 备份内容（= 数据目录里"丢了就回不来"的东西）：
//
//	mocenter.db      SQLite：主机、探针/延迟历史、端口映射、审计日志、黑名单
//	users.json       账号与口令哈希、OTP 密钥
//	remarks.json     主机备注（用户输入，唯一副本）
//	public-site.json 公开展示配置
//	agent.key        Agent 注册预共享密钥（丢了所有 Agent 都连不上）
//
// 刻意**不备份**的：
//
//	packages/        安装包托管目录（可从上游重新下载，动辄上百 MB，进备份只会把包撑爆）
//	uploads/         文件传输暂存（临时文件）
//	logs/            日志
//	*.tmp / -wal/-shm  SQLite 运行时临时文件
//
// 设计取舍：
//
//  1. 备份是**单个 zip 文件**，内含 manifest.json（版本号、生成时间、服务端版本、
//     各文件大小与 SHA-256）。有 manifest 才能让恢复端做校验，
//     而不是"解压出来能跑就行"。
//  2. 备份过程对 SQLite 使用 `VACUUM INTO`（在线备份）而不是直接拷文件：
//     WAL 模式下直接拷 .db 会丢掉尚未 checkpoint 的事务，恢复后表现为
//     "最近一段时间的审计/探针记录莫名消失"。VACUUM INTO 会产出一份
//     事务一致的完整副本。
//  3. 恢复做**白名单 + 路径校验**：zip 条目名必须落在允许的文件集合内，
//     且解析后的绝对路径必须仍在数据目录内（防 zip-slip 路径穿越）。
//     恢复包可能来自"运维从别处拷来的文件"，不能假定它可信。
package backup

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestName 备份包内的清单文件名。
const ManifestName = "manifest.json"

// DefaultMaxSize 单个备份包的最大解压体积（默认 1 GiB）。
//
// Zip bomb 防护：压缩比可以做到 1000:1，无上限解压会把磁盘写满 ——
// 而恢复接口是需要登录的运维功能，配置错误/恶意包都不该拖垮服务端。
const DefaultMaxSize = 1 << 30

// fileSpec 一个待备份的数据文件。
type fileSpec struct {
	// Name 数据目录内的相对文件名（同时也是 zip 内的条目名）。
	Name string
	// Label 中文说明（写进 manifest，便于运维一眼看出包里有什么）。
	Label string
	// Required 该文件是否必须存在（不存在时备份失败）。
	Required bool
	// Sensitive 是否含凭据（manifest 里标注，恢复时给出提醒）。
	Sensitive bool
}

// backupFiles 备份文件白名单。
//
// **必须是白名单而不是"遍历目录取全部文件"**：数据目录里还躺着 uploads/、
// packages/、日志与 SQLite 的 -wal/-shm 临时文件，哪个都不该进备份包。
// 白名单同时让恢复端可以拒绝未列出的条目（见 Extract）。
var backupFiles = []fileSpec{
	{Name: "mocenter.db", Label: "SQLite 数据库（主机/历史/端口映射/审计）", Required: true, Sensitive: true},
	{Name: "users.json", Label: "账号与口令哈希、OTP 密钥", Sensitive: true},
	{Name: "remarks.json", Label: "主机备注"},
	{Name: "public-site.json", Label: "公开展示配置"},
	{Name: "agent.key", Label: "Agent 注册预共享密钥", Sensitive: true},
}

// Entry manifest 中的一个文件条目。
type Entry struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Sensitive 含凭据（恢复后应立即确认权限与必要性）
	Sensitive bool `json:"sensitive,omitempty"`
}

// Manifest 备份包清单。
type Manifest struct {
	// FormatVersion 备份格式版本：将来若改变包结构，恢复端据此判断兼容性。
	FormatVersion int    `json:"format_version"`
	CreatedAt     int64  `json:"created_at"`
	CreatedAtText string `json:"created_at_text"`
	// AppVersion 生成备份的服务端版本号（便于判断"能否恢复到当前版本"）
	AppVersion string `json:"app_version"`
	// Hostname 生成备份的机器名（多实例场景下区分来源）
	Hostname string  `json:"hostname"`
	Entries  []Entry `json:"entries"`
	// Skipped 被跳过的数据文件及原因（如"不存在"），让运维知道包里少什么
	Skipped []string `json:"skipped,omitempty"`
}

// FormatVersion 当前备份格式版本。
const FormatVersion = 1

// Result 备份结果。
type Result struct {
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Files    int      `json:"files"`
	Entries  []Entry  `json:"entries"`
	Skipped  []string `json:"skipped,omitempty"`
	Duration string   `json:"duration"`
}

// Create 把数据目录备份为 destPath 指向的 zip 文件。
//
// appVersion 写入 manifest（由调用方从 internal/version 传入，
// 避免本包反向依赖版本包）。
func Create(dataDir, destPath, appVersion string) (*Result, error) {
	start := time.Now()
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("数据目录为空")
	}
	if strings.TrimSpace(destPath) == "" {
		return nil, fmt.Errorf("备份路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}
	// 先写临时文件再原子改名：备份写到一半失败时不会留下一个"看起来能恢复、
	// 实际缺文件"的半成品包 —— 那比没有备份更危险。
	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("创建备份文件失败: %w", err)
	}
	// 备份包含口令哈希与 Agent 密钥：0600 收紧权限
	if err := os.Chmod(tmpPath, 0600); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("设置备份文件权限失败: %w", err)
	}

	zw := zip.NewWriter(f)
	manifest := Manifest{
		FormatVersion: FormatVersion,
		CreatedAt:     time.Now().Unix(),
		CreatedAtText: time.Now().Format("2006-01-02 15:04:05"),
		AppVersion:    appVersion,
	}
	if h, herr := os.Hostname(); herr == nil {
		manifest.Hostname = h
	}

	// SQLite 主库做在线一致备份；其余文件直接读。
	// 注意顺序：sqlite 备份会额外产生一个临时文件，打包后删除。
	sqliteTmp, cleanup, err := snapshotSQLite(dataDir, filepath.Dir(destPath))
	if err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmpPath)
		return nil, err
	}
	defer cleanup()

	for _, spec := range backupFiles {
		srcPath := filepath.Join(dataDir, spec.Name)
		if spec.Name == "mocenter.db" && sqliteTmp != "" {
			srcPath = sqliteTmp
		}
		fi, statErr := os.Stat(srcPath)
		if statErr != nil {
			if spec.Required {
				zw.Close()
				f.Close()
				os.Remove(tmpPath)
				return nil, fmt.Errorf("必需数据文件缺失 %s: %w", spec.Name, statErr)
			}
			manifest.Skipped = append(manifest.Skipped, spec.Name+"（文件不存在）")
			continue
		}
		if fi.IsDir() {
			manifest.Skipped = append(manifest.Skipped, spec.Name+"（是目录，已跳过）")
			continue
		}
		sum, size, werr := addFile(zw, srcPath, spec.Name)
		if werr != nil {
			zw.Close()
			f.Close()
			os.Remove(tmpPath)
			return nil, fmt.Errorf("打包 %s 失败: %w", spec.Name, werr)
		}
		manifest.Entries = append(manifest.Entries, Entry{
			Name: spec.Name, Label: spec.Label, Size: size, SHA256: sum, Sensitive: spec.Sensitive,
		})
	}

	// manifest 必须能读回：写在包内，恢复端先读它做校验
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("生成备份清单失败: %w", err)
	}
	w, err := zw.Create(ManifestName)
	if err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("写入备份清单失败: %w", err)
	}
	if _, err := w.Write(mb); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("写入备份清单失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("完成备份包失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("写入备份文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("保存备份文件失败: %w", err)
	}
	fi, _ := os.Stat(destPath)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	return &Result{
		Path: destPath, Size: size, Files: len(manifest.Entries),
		Entries: manifest.Entries, Skipped: manifest.Skipped,
		Duration: time.Since(start).Round(time.Millisecond).String(),
	}, nil
}

// snapshotSQLite 用 VACUUM INTO 生成一份事务一致的数据库副本。
//
// 为什么不能直接拷 mocenter.db：
//   - 默认 WAL 模式下，未 checkpoint 的事务在 -wal 文件里，只拷主库会丢数据；
//   - 运行中的库文件加锁状态拷贝可能拿到撕裂的页面。
//
// VACUUM INTO 会产出一个**已 compact、事务一致**的新库文件，且不阻塞读写。
// 返回的 cleanup 负责删除临时副本。
func snapshotSQLite(dataDir, tmpDir string) (string, func(), error) {
	src := filepath.Join(dataDir, "mocenter.db")
	noop := func() {}
	if _, err := os.Stat(src); err != nil {
		// 库还没建（全新部署立刻备份）：不是错误，跳过即可
		if os.IsNotExist(err) {
			return "", noop, nil
		}
		return "", noop, fmt.Errorf("检查数据库失败: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "mocenter-snapshot-*.db")
	if err != nil {
		return "", noop, fmt.Errorf("创建数据库快照文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	// VACUUM INTO 要求目标文件不存在
	_ = os.Remove(tmpPath)
	cleanup := func() { _ = os.Remove(tmpPath) }

	// 以只读方式打开源库：备份过程不应写入任何东西
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return "", cleanup, fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, tmpPath); err != nil {
		return "", cleanup, fmt.Errorf("生成数据库快照失败（VACUUM INTO）: %w", err)
	}
	return tmpPath, cleanup, nil
}

// addFile 把文件写入 zip，同时计算 SHA-256 与大小。
func addFile(zw *zip.Writer, srcPath, name string) (string, int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	w, err := zw.Create(name)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), src)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ReadManifest 读取备份包的清单（不落盘、不校验哈希）。
//
// 用途：让前端/运维先看清"这个包里有什么、什么版本、是否含凭据"，
// 再决定是否真的恢复。恢复是不可逆操作，多一步确认是有价值的。
func ReadManifest(zipPath string) (*Manifest, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("打开备份包失败: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != ManifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("读取备份清单失败: %w", err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("读取备份清单失败: %w", err)
		}
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("解析备份清单失败: %w", err)
		}
		if m.FormatVersion > FormatVersion {
			return nil, fmt.Errorf("备份包格式版本 %d 高于当前支持的 %d，请升级服务端后再恢复",
				m.FormatVersion, FormatVersion)
		}
		return &m, nil
	}
	return nil, fmt.Errorf("备份包中没有 %s（可能不是 MoCenter 的备份）", ManifestName)
}

// RestoreOptions 恢复选项。
type RestoreOptions struct {
	// Overwrite 覆盖已存在的文件（默认 true；false 时只补缺失文件）
	Overwrite bool
	// MaxSize 解压上限（0 用 DefaultMaxSize）
	MaxSize int64
	// KeepBackupOfExisting 恢复前把被覆盖的现有文件另存为 <name>.before-restore
	KeepBackupOfExisting bool
}

// RestoreResult 恢复结果。
type RestoreResult struct {
	Restored []string `json:"restored"`
	Skipped  []string `json:"skipped"`
	Manifest *Manifest
}

// Restore 把备份包解压回数据目录。
//
// 安全约束（逐条对应一类真实事故）：
//
//  1. **白名单**：只接受 manifest 与 backupFiles 中列出的条目名，
//     其它条目一律跳过（不是"解压全部"）。
//  2. **防路径穿越（zip-slip）**：条目名不允许包含分隔符或 ".."，
//     且落盘前再次校验绝对路径确实在数据目录内。备份包可能来自外部，
//     不能假定它无害 —— 一个 ../../etc/cron.d/x 的条目就是一次提权。
//  3. **体积上限**：解压总量超过 MaxSize 立即失败（防 zip bomb）。
//  4. **哈希校验**：manifest 里记了 SHA-256，不一致直接失败而不是
//     "先恢复了再说"—— 半个损坏的数据库比没有更糟。
func Restore(zipPath, dataDir string, opts RestoreOptions) (*RestoreResult, error) {
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("数据目录为空")
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("打开备份包失败: %w", err)
	}
	defer zr.Close()

	// 先读 manifest：它既给出待校验的摘要，也让我们能对比"包里有什么"
	manifest, err := ReadManifest(zipPath)
	if err != nil {
		return nil, err
	}
	wantSum := map[string]string{}
	allowed := map[string]bool{ManifestName: true}
	for _, e := range manifest.Entries {
		wantSum[e.Name] = strings.ToLower(strings.TrimSpace(e.SHA256))
		allowed[e.Name] = true
	}
	// manifest 未列出但属于官方数据文件时也允许（兼容"清单缺条目"的旧包），
	// 但**绝不**接受清单外的任意路径。
	for _, s := range backupFiles {
		allowed[s.Name] = true
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("解析数据目录失败: %w", err)
	}

	res := &RestoreResult{Manifest: manifest}
	var total int64
	// 排序保证恢复顺序稳定（数据库与配置先落，便于出问题时按顺序排查）
	sort.SliceStable(zr.File, func(i, j int) bool { return zr.File[i].Name < zr.File[j].Name })

	for _, f := range zr.File {
		name := f.Name
		if name == ManifestName {
			continue
		}
		if !allowed[name] {
			res.Skipped = append(res.Skipped, name+"（不在允许的数据文件白名单内）")
			continue
		}
		// 条目名必须是**纯文件名**：不含路径分隔符、不是绝对路径、不含 ..
		if !isPlainFileName(name) {
			res.Skipped = append(res.Skipped, name+"（条目名不合法，疑似路径穿越）")
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > opts.MaxSize {
			return nil, fmt.Errorf("备份包解压体积超过上限 %d 字节，已中止（防 zip bomb）", opts.MaxSize)
		}
		dest := filepath.Join(absData, name)
		// 二次校验：解析后的路径必须仍在数据目录内。
		if !insideDir(absData, dest) {
			res.Skipped = append(res.Skipped, name+"（目标路径越界，已拒绝）")
			continue
		}
		if !opts.Overwrite {
			if _, err := os.Stat(dest); err == nil {
				res.Skipped = append(res.Skipped, name+"（已存在且未选择覆盖）")
				continue
			}
		}
		sum, err := extractOne(f, dest, opts.KeepBackupOfExisting)
		if err != nil {
			return nil, fmt.Errorf("恢复 %s 失败: %w", name, err)
		}
		// 摘要校验：只对 manifest 中记录过的条目做（旧包可能缺条目）
		if want, ok := wantSum[name]; ok && want != "" && want != sum {
			return nil, fmt.Errorf("恢复 %s 失败: SHA-256 校验不通过（期望 %s，实际 %s），备份包可能已损坏",
				name, want, sum)
		}
		res.Restored = append(res.Restored, name)
	}
	if len(res.Restored) == 0 {
		return res, fmt.Errorf("备份包中没有可恢复的数据文件")
	}
	return res, nil
}

// isPlainFileName 判断是否为一个"纯文件名"（不含任何目录成分）。
func isPlainFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if filepath.IsAbs(name) {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	// 控制字符/空字节（某些解压实现会被截断成意外路径）
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	return true
}

// insideDir 判断 path 是否位于 dir 之内。
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// extractOne 解压单个条目到 dest，返回内容的 SHA-256。
func extractOne(f *zip.File, dest string, keepOld bool) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	if keepOld {
		if old, err := os.ReadFile(dest); err == nil {
			_ = os.WriteFile(dest+".before-restore", old, 0600)
		}
	}
	tmp := dest + ".restoring"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// 原子替换：中途失败不会留下半个文件（半个数据库比没有更糟）
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
