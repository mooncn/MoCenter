package storage

import (
	"compress/gzip"
	"encoding/csv"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// TestCompactHistory 验证：
//   - 超过 7 天的探针原始数据先导出 gzip CSV，再从库里删除；
//   - 7 天内原始采样原样保留；
//   - 延迟历史仍按 24h/30d 分层聚合；
//   - 重复执行幂等。
func TestCompactHistory(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().Unix()
	// 2 台主机 × 5000 个原始点，落在 10 天前 ~ 9 天前（全部超过 7 天）。
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (
			SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x < 9999
		)
		INSERT INTO probe_history
			(agent_id, timestamp, cpu_usage, mem_usage, disk_usage, net_in_delta, net_out_delta)
		SELECT CASE WHEN x % 2 = 0 THEN 'agent-a' ELSE 'agent-b' END,
		       ? - 10*86400 + x*10, 1.0, 2.0, 3.0, 100, 50
		FROM n`, now); err != nil {
		t.Fatal(err)
	}
	// 更老的一条（31 天前）也应归档，而不是被单独丢弃。
	if _, err := db.Exec(`INSERT INTO probe_history
		(agent_id, timestamp, cpu_usage, net_in_delta, net_out_delta) VALUES ('agent-a', ?, 5, 0, 0)`,
		now-31*86400); err != nil {
		t.Fatal(err)
	}
	// 7 天内的近期点：不归档、不聚合。
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (
			SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x < 100
		)
		INSERT INTO probe_history
			(agent_id, timestamp, cpu_usage, mem_usage, disk_usage, net_in_delta, net_out_delta)
		SELECT 'agent-a', ? - 3600 + x*10, 9.9, 8.8, 7.7, 7, 3
		FROM n`, now); err != nil {
		t.Fatal(err)
	}
	// 延迟历史：3 天前（>24h、<30d）应聚合；31 天前应删除。
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (
			SELECT 0 UNION ALL SELECT x+1 FROM n WHERE x < 9999
		)
		INSERT INTO latency_history (agent_id, timestamp, target, kind, latency_ms, ok)
		SELECT 'agent-a', ? - 3*86400 + x*10, 'icmp:1.1.1.1', 'icmp', 20, 1
		FROM n`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO latency_history (agent_id, timestamp, target, kind, latency_ms, ok)
		VALUES ('agent-a', ?, 'icmp:1.1.1.1', 'icmp', 30, 1)`, now-31*86400); err != nil {
		t.Fatal(err)
	}

	st, err := db.CompactHistory()
	if err != nil {
		t.Fatal(err)
	}
	if st.ProbeBefore != 10102 {
		t.Fatalf("归档前探针行数应为 10102，实际 %d", st.ProbeBefore)
	}
	if st.ArchivedProbe != 10001 || st.ProbeAfter != 101 {
		t.Fatalf("归档/保留行数异常: archived=%d after=%d", st.ArchivedProbe, st.ProbeAfter)
	}
	if st.ArchivePath == "" {
		t.Fatal("应生成归档文件")
	}
	if _, err := os.Stat(st.ArchivePath); err != nil {
		t.Fatalf("归档文件不存在: %v", err)
	}
	name := filepath.Base(st.ArchivePath)
	if !regexp.MustCompile(`^\d{14}2\d{14}2[0-9a-f]{8}_probe_history\.gz$`).MatchString(name) {
		t.Fatalf("归档文件名不符合约定: %s", name)
	}
	if filepath.Dir(st.ArchivePath) != db.ArchiveDir() {
		t.Fatalf("归档应落在 %s，实际 %s", db.ArchiveDir(), st.ArchivePath)
	}

	// 校验 gzip CSV：表头 + 10001 行，且保留全部列。
	f, err := os.Open(st.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	records, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 10002 {
		t.Fatalf("归档应有 10001 条数据 + 1 行表头，实际 %d 行", len(records))
	}
	if len(records[0]) != 24 || records[0][1] != "agent_id" {
		t.Fatalf("归档表头异常: %v", records[0])
	}

	var recent int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM probe_history WHERE timestamp >= ?`, now-86400).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if recent != 101 {
		t.Fatalf("7 天内原始采样应保持 101 条，实际 %d", recent)
	}
	var recentCPU float64
	if err := db.QueryRow(`SELECT cpu_usage FROM probe_history WHERE timestamp >= ? LIMIT 1`, now-86400).Scan(&recentCPU); err != nil {
		t.Fatal(err)
	}
	if recentCPU != 9.9 {
		t.Fatalf("近期原始值不应被改动，cpu=%v", recentCPU)
	}

	var latRows int64
	var latAvg float64
	var latOK int
	if err := db.QueryRow(`SELECT COUNT(*), AVG(latency_ms), MAX(ok) FROM latency_history`).Scan(&latRows, &latAvg, &latOK); err != nil {
		t.Fatal(err)
	}
	if latRows < 200 || latRows > 500 || latAvg != 20 || latOK != 1 {
		t.Fatalf("延迟聚合结果异常: rows=%d avg=%v ok=%d", latRows, latAvg, latOK)
	}

	// 第二次：没有新的过期探针，不应再生成归档。
	st2, err := db.CompactHistory()
	if err != nil {
		t.Fatal(err)
	}
	if st2.ArchivedProbe != 0 || st2.ArchivePath != "" {
		t.Fatalf("重复执行不应再次归档: archived=%d path=%q", st2.ArchivedProbe, st2.ArchivePath)
	}
	if st2.ProbeAfter != st.ProbeAfter || st2.RemovedProbe != 0 {
		t.Fatalf("重复压缩应幂等: first=%d second=%d removed=%d", st.ProbeAfter, st2.ProbeAfter, st2.RemovedProbe)
	}
}

// TestCompactStorageRuns 冒烟：VACUUM/auto_vacuum 路径在真实库文件上可执行。
func TestCompactStorageRuns(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "vacuum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO probe_history (agent_id, timestamp, cpu_usage) VALUES ('a', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompactStorage(); err != nil {
		t.Fatalf("空间回收失败: %v", err)
	}
	var autoVacuum int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		t.Fatal(err)
	}
	if autoVacuum != 2 {
		t.Fatalf("压缩后 auto_vacuum 应为 2（INCREMENTAL），实际 %d", autoVacuum)
	}
}
