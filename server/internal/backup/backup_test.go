package backup

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// makeDataDir 造一个含全部数据文件的伪数据目录。
func makeDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// mocenter.db 必须是**真实**的 SQLite 文件：备份走 VACUUM INTO
	// （为了拿到事务一致快照），拿一段文本冒充会在这一步失败。
	writeRealSQLite(t, filepath.Join(dir, "mocenter.db"))
	files := map[string]string{
		"users.json":       `{"admin":{"hash":"x"}}`,
		"remarks.json":     `{"a":"备注"}`,
		"public-site.json": `{"public_enabled":true}`,
		"agent.key":        "SECRET-KEY\n",
		// 这些绝不该进备份
		"uploads/tmp.bin":  "TEMP",
		"packages/big.deb": "HUGE",
		"mocenter.db-wal":  "WAL",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestCreateAndRestore 备份 -> 恢复到空目录，内容应逐字节一致。
func TestCreateAndRestore(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")

	res, err := Create(src, out, "1.0.1")
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if res.Files != 5 {
		t.Fatalf("应备份 5 个文件，实际 %d（skipped=%v）", res.Files, res.Skipped)
	}
	if len(res.Entries) == 0 {
		t.Fatal("备份清单不应为空")
	}

	// 恢复到一个全新的目录
	dst := t.TempDir()
	rr, err := Restore(out, dst, RestoreOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if len(rr.Restored) != 5 {
		t.Fatalf("应恢复 5 个文件，实际 %d（%v）", len(rr.Restored), rr.Restored)
	}
	// 非数据库文件必须逐字节一致
	for _, name := range []string{"users.json", "remarks.json", "public-site.json", "agent.key"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("恢复后缺少 %s: %v", name, err)
		}
		want, _ := os.ReadFile(filepath.Join(src, name))
		if string(got) != string(want) {
			t.Fatalf("%s 内容不一致: got=%q want=%q", name, got, want)
		}
	}
	// mocenter.db 走的是 VACUUM INTO 快照，字节不会与源文件相同
	// （这正是它能保证事务一致的原因），因此校验"可打开且数据在里面"。
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dst, "mocenter.db"))
	if err != nil {
		t.Fatalf("恢复后的数据库无法打开: %v", err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM t ORDER BY id LIMIT 1`).Scan(&v); err != nil {
		t.Fatalf("恢复后的数据库内容不正确: %v", err)
	}
	if v != "hello" {
		t.Fatalf("恢复后的数据库内容应为 hello，实际 %q", v)
	}
}

// TestRestoreKeepsDatabaseConsistent 恢复出的库必须是"可独立打开"的完整库，
// 而不是一个依赖 -wal/-shm 才能读的半个文件。
//
// 这条守护的是一个很隐蔽的坑：WAL 模式下直接拷 .db 会在库里留下
// "已提交但落在 wal 里"的事务，拷走主库后它们就丢了 ——
// 恢复出来表现为"最近一段时间的审计/探针记录莫名消失"。
func TestRestoreKeepsDatabaseConsistent(t *testing.T) {
	src := t.TempDir()
	writeRealSQLite(t, filepath.Join(src, "mocenter.db"))
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	dst := t.TempDir()
	if _, err := Restore(out, dst, RestoreOptions{Overwrite: true}); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 恢复目录里不应出现 wal/shm（快照已是自包含的）
	for _, extra := range []string{"mocenter.db-wal", "mocenter.db-shm"} {
		if _, err := os.Stat(filepath.Join(dst, extra)); err == nil {
			t.Fatalf("恢复后不应存在 %s（快照应自包含）", extra)
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dst, "mocenter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("恢复后的库无法查询: %v", err)
	}
	if n != 1 {
		t.Fatalf("恢复后的库应有 1 行，实际 %d", n)
	}
}

// TestBackupExcludesNonData 备份包不得包含 uploads/packages/日志/临时文件。
//
// 这条是"备份包被撑爆"与"凭据与垃圾一起外流"的防线：
// 数据目录里躺着的这些文件体积大且无恢复价值。
func TestBackupExcludesNonData(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		switch {
		case strings.HasPrefix(f.Name, "uploads/"),
			strings.HasPrefix(f.Name, "packages/"),
			strings.HasSuffix(f.Name, "-wal"),
			strings.HasSuffix(f.Name, "-shm"),
			strings.HasSuffix(f.Name, ".tmp"):
			t.Fatalf("备份包不应包含 %s", f.Name)
		}
	}
}

// TestManifestContainsChecksums 清单必须带版本号与每个文件的 SHA-256。
func TestManifestContainsChecksums(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	m, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	if m.FormatVersion != FormatVersion {
		t.Fatalf("格式版本应为 %d，实际 %d", FormatVersion, m.FormatVersion)
	}
	if m.AppVersion != "1.0.1" {
		t.Fatalf("版本号应为 1.0.1，实际 %q", m.AppVersion)
	}
	if m.CreatedAt == 0 {
		t.Fatal("清单应包含生成时间")
	}
	for _, e := range m.Entries {
		if len(e.SHA256) != 64 {
			t.Fatalf("%s 的 SHA-256 长度异常: %q", e.Name, e.SHA256)
		}
		if e.Size <= 0 {
			t.Fatalf("%s 的大小应大于 0", e.Name)
		}
	}
}

// TestRestoreDetectsCorruption 内容被篡改时，恢复必须失败而不是"先恢复再说"。
func TestRestoreDetectsCorruption(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	// 重写包内 mocenter.db 的内容，但保留原清单里的哈希
	corrupt := filepath.Join(t.TempDir(), "corrupt.zip")
	rewriteZip(t, out, corrupt, map[string]string{"mocenter.db": "TAMPERED-CONTENT"})

	dst := t.TempDir()
	_, err := Restore(corrupt, dst, RestoreOptions{Overwrite: true})
	if err == nil {
		t.Fatal("内容被篡改时应报错")
	}
	if !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("错误信息应指出摘要校验失败，实际: %v", err)
	}
}

// TestRestoreRejectsPathTraversal zip-slip 条目必须被拒绝（备份包可能来自外部）。
func TestRestoreRejectsPathTraversal(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	evil := filepath.Join(t.TempDir(), "evil.zip")
	rewriteZip(t, out, evil, map[string]string{
		"../evil.sh":              "rm -rf /",
		"../../etc/cron.d/pwn":    "* * * * * root sh",
		"mocenter.db/../../x.txt": "x",
	})
	// 目标目录选在临时目录之外，便于检查是否真的越界写入
	base := t.TempDir()
	dst := filepath.Join(base, "data")
	rr, err := Restore(evil, dst, RestoreOptions{Overwrite: true})
	// 备份包本身仍是有效的（白名单内的文件能恢复），但恶意条目必须被拒
	if err != nil {
		t.Fatalf("合法文件应能恢复: %v", err)
	}
	if len(rr.Skipped) < 3 {
		t.Fatalf("三个越界条目都应被跳过，实际 skipped=%v", rr.Skipped)
	}
	if _, err := os.Stat(filepath.Join(base, "evil.sh")); err == nil {
		t.Fatal("越界文件被写到了数据目录之外")
	}
	if _, err := os.Stat(filepath.Join(dst, "..", "evil.sh")); err == nil {
		t.Fatal("越界文件被写到了数据目录的父目录")
	}
}

// TestRestoreRejectsUnknownEntries 清单与白名单之外的条目一律跳过。
func TestRestoreRejectsUnknownEntries(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	extra := filepath.Join(t.TempDir(), "extra.zip")
	rewriteZip(t, out, extra, map[string]string{"shadow": "root:x:0:0"})

	dst := t.TempDir()
	rr, err := Restore(extra, dst, RestoreOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "shadow")); err == nil {
		t.Fatal("白名单外的文件不应被解压")
	}
	found := false
	for _, s := range rr.Skipped {
		if strings.HasPrefix(s, "shadow") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报告 shadow 被跳过，实际 %v", rr.Skipped)
	}
}

// TestRestoreZipBombGuard 超大解压体积必须被拦下。
func TestRestoreZipBombGuard(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	dst := t.TempDir()
	// 把上限压到 1 字节：任何一个文件都会超限
	_, err := Restore(out, dst, RestoreOptions{Overwrite: true, MaxSize: 1})
	if err == nil {
		t.Fatal("超过体积上限时应中止")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误信息应说明体积超限，实际: %v", err)
	}
}

// TestRestoreOverwriteFlag Overwrite=false 时应跳过已存在的文件。
func TestRestoreOverwriteFlag(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "mocenter.db"), []byte("EXISTING"), 0600); err != nil {
		t.Fatal(err)
	}
	rr, err := Restore(out, dst, RestoreOptions{Overwrite: false})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "mocenter.db"))
	if string(got) != "EXISTING" {
		t.Fatalf("Overwrite=false 时不应覆盖已有文件，实际 %q", got)
	}
	skipped := false
	for _, s := range rr.Skipped {
		if strings.HasPrefix(s, "mocenter.db") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("应报告 mocenter.db 被跳过，实际 %v", rr.Skipped)
	}
}

// TestRestoreKeepBackupOfExisting 恢复前的"退路"文件应被保留。
func TestRestoreKeepBackupOfExisting(t *testing.T) {
	src := makeDataDir(t)
	out := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := Create(src, out, "1.0.1"); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "remarks.json"), []byte("OLD-REMARKS"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(out, dst, RestoreOptions{Overwrite: true, KeepBackupOfExisting: true}); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	old, err := os.ReadFile(filepath.Join(dst, "remarks.json.before-restore"))
	if err != nil {
		t.Fatalf("应保留恢复前的旧文件: %v", err)
	}
	if string(old) != "OLD-REMARKS" {
		t.Fatalf("旧文件内容不正确: %q", old)
	}
}

// TestReadManifestRejectsNonBackup 非备份包（缺清单）应给出明确错误。
func TestReadManifestRejectsNonBackup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notbackup.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("random.txt")
	_, _ = w.Write([]byte("hello"))
	zw.Close()
	f.Close()
	if _, err := ReadManifest(p); err == nil {
		t.Fatal("缺少清单时应报错")
	}
}

// TestSnapshotIsConsistent SQLite 备份应产出可独立打开的完整库（有主库时）。
func TestSnapshotIsConsistent(t *testing.T) {
	src := t.TempDir()
	// 造一个真实的最小 SQLite 库（纯文本的假库无法被 VACUUM INTO 处理）
	writeRealSQLite(t, filepath.Join(src, "mocenter.db"))
	// 造一个真实的 SQLite 库用于快照测试
	_ = os.WriteFile(filepath.Join(src, "users.json"), []byte("{}"), 0600)
	out := filepath.Join(t.TempDir(), "backup.zip")
	res, err := Create(src, out, "1.0.1")
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if res.Files != 2 {
		t.Fatalf("应备份 2 个文件，实际 %d（skipped=%v）", res.Files, res.Skipped)
	}
	// 校验 mocenter.db 条目大小 > 0（快照不是空文件）
	for _, e := range res.Entries {
		if e.Name == "mocenter.db" && e.Size <= 0 {
			t.Fatal("数据库快照不应为空文件")
		}
	}
}

// rewriteZip 用指定的覆盖/新增内容重写 zip，用于构造损坏/恶意包。
func rewriteZip(t *testing.T, src, dst string, changes map[string]string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	written := map[string]bool{}
	for _, f := range zr.File {
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if body, ok := changes[f.Name]; ok {
			_, _ = w.Write([]byte(body))
			written[f.Name] = true
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		_, _ = w.Write(b)
	}
	// 追加新条目（不存在于原包中的）
	for name, body := range changes {
		if written[name] {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeRealSQLite 写一个真实可用的最小 SQLite 文件（modernc 纯 Go 驱动）。
func writeRealSQLite(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t (v) VALUES ('hello')`); err != nil {
		t.Fatal(err)
	}
}

// TestManifestJSONStable 清单必须是稳定可解析的 JSON（前端要直接展示）。
func TestManifestJSONStable(t *testing.T) {
	m := Manifest{FormatVersion: 1, AppVersion: "1.0.1", Entries: []Entry{{Name: "a", SHA256: "x"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Entries[0].Name != "a" {
		t.Fatalf("往返失败: %+v", back)
	}
}
