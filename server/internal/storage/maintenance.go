package storage

import (
	"compress/gzip"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 历史数据策略：
//   - probe_history：只保留最近 7 天的原始采样；更早的数据先导出为
//     `<起始>2<结束>2<随机>_probe_history.gz`（gzip CSV）再删除；
//   - latency_history：仍按 24 小时原始 + 24 小时~30 天 5 分钟聚合。
//
// 10 秒采样 × 8 台主机约 7 万行/天，7 天约 50 万行；超过 7 天的原始行归档后
// 从库里移除，趋势查询与 VACUUM 的压力都随时间保持有界。
const (
	ProbeRetention       = 7 * 24 * time.Hour
	LatencyFineAge       = 24 * time.Hour
	LatencyRetention     = 30 * 24 * time.Hour
	HistoryBucketSeconds = 300
)

// HistoryCompactStats 一次维护的统计口径（便于启动日志与测试断言）。
type HistoryCompactStats struct {
	ProbeBefore   int64
	ProbeAfter    int64
	LatencyBefore int64
	LatencyAfter  int64
	// ArchivedProbe 本次导出并删除的探针历史行数。
	ArchivedProbe int64
	ArchivePath   string
	ArchiveStart  int64
	ArchiveEnd    int64
	// PrunedLatency 超过保留期直接删除的延迟行数。
	PrunedLatency  int64
	RemovedProbe   int64
	RemovedLatency int64
}

// CompactHistory 执行一次"归档过期探针 + 延迟历史分层留存"。
//
// 探针归档失败时**不删除**库里任何一行（宁可下次再试，也不做无备份清理）；
// 延迟聚合在独立事务里完成。
func (db *DB) CompactHistory() (HistoryCompactStats, error) {
	var st HistoryCompactStats
	now := time.Now()
	probeCutoff := now.Add(-ProbeRetention).Unix()
	latencyCutoff := now.Add(-LatencyRetention).Unix()
	latencyFineCutoff := now.Add(-LatencyFineAge).Unix()

	if err := db.QueryRow(`SELECT COUNT(*) FROM probe_history`).Scan(&st.ProbeBefore); err != nil {
		return st, fmt.Errorf("统计探针历史失败: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM latency_history`).Scan(&st.LatencyBefore); err != nil {
		return st, fmt.Errorf("统计延迟历史失败: %w", err)
	}

	// 1) 探针历史：先导出，确认文件落盘后再删除。
	archived, path, start, end, err := db.archiveProbeHistory(probeCutoff)
	if err != nil {
		return st, err
	}
	st.ArchivedProbe = archived
	st.ArchivePath = path
	st.ArchiveStart = start
	st.ArchiveEnd = end
	if archived > 0 {
		if _, err := db.Exec(`DELETE FROM probe_history WHERE timestamp < ?`, probeCutoff); err != nil {
			return st, fmt.Errorf("归档成功但删除过期探针历史失败: %w", err)
		}
	}

	// 2) 延迟历史：过期清理 + 5 分钟聚合。
	if err := db.compactLatency(latencyCutoff, latencyFineCutoff); err != nil {
		return st, err
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM probe_history`).Scan(&st.ProbeAfter); err != nil {
		return st, fmt.Errorf("统计压缩后探针历史失败: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM latency_history`).Scan(&st.LatencyAfter); err != nil {
		return st, fmt.Errorf("统计压缩后延迟历史失败: %w", err)
	}
	st.RemovedProbe = st.ProbeBefore - st.ProbeAfter
	st.RemovedLatency = st.LatencyBefore - st.LatencyAfter
	return st, nil
}

func (db *DB) compactLatency(retentionCutoff, fineCutoff int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM latency_history WHERE timestamp < ?`, retentionCutoff); err != nil {
		return fmt.Errorf("清理过期延迟历史失败: %w", err)
	}
	if fineCutoff > retentionCutoff {
		if err := compactLatencyRange(tx, retentionCutoff, fineCutoff, HistoryBucketSeconds); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// archiveProbeHistory 把 cutoff 之前的探针行写成 gzip CSV。
//
// 文件名：`yyyymmddhhiiss2yyyymmddhhiiss2<8位随机hex>_probe_history.gz`，
// 两个时间分别是这批数据的实际最早/最晚采样时间（本地时区）。
// 采用流式查询 + 临时文件 rename，避免百万行全部驻留内存或留下半个归档。
func (db *DB) archiveProbeHistory(cutoff int64) (int64, string, int64, int64, error) {
	var count, minTS, maxTS int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(timestamp), 0), COALESCE(MAX(timestamp), 0)
		FROM probe_history WHERE timestamp < ?`, cutoff).Scan(&count, &minTS, &maxTS); err != nil {
		return 0, "", 0, 0, fmt.Errorf("统计待归档探针历史失败: %w", err)
	}
	if count == 0 {
		return 0, "", 0, 0, nil
	}
	dir := db.ArchiveDir()
	if dir == "" {
		return 0, "", 0, 0, fmt.Errorf("数据库路径为空，无法确定归档目录")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, "", 0, 0, fmt.Errorf("创建归档目录失败: %w", err)
	}
	_ = os.Chmod(dir, 0700) // 目录可能由旧版本以更宽权限创建

	suffix, err := randomHex(4)
	if err != nil {
		return 0, "", 0, 0, fmt.Errorf("生成归档文件名失败: %w", err)
	}
	name := fmt.Sprintf("%s2%s2%s_probe_history.gz",
		time.Unix(minTS, 0).Format("20060102150405"),
		time.Unix(maxTS, 0).Format("20060102150405"),
		suffix)
	finalPath := filepath.Join(dir, name)
	tmpPath := finalPath + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, "", 0, 0, fmt.Errorf("创建归档文件失败: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	// 归档优先速度：百万行级别用 BestSpeed，压缩率仍足以把 CSV 降低一个量级。
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("初始化 gzip 失败: %w", err)
	}
	cw := csv.NewWriter(gz)
	header := []string{
		"id", "agent_id", "timestamp", "cpu_usage", "mem_usage", "disk_usage",
		"net_in_bytes", "net_out_bytes", "os_type", "kernel_version", "uptime",
		"public_ip", "os_version", "cpu_model", "cpu_mhz", "mem_total", "mem_used",
		"disk_total", "disk_used", "net_in_delta", "net_out_delta",
		"load1", "load5", "load15",
	}
	if err := cw.Write(header); err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("写入归档表头失败: %w", err)
	}

	rows, err := db.Query(`SELECT id, agent_id, timestamp, cpu_usage, mem_usage, disk_usage,
		net_in_bytes, net_out_bytes, os_type, kernel_version, uptime, public_ip, os_version,
		cpu_model, cpu_mhz, mem_total, mem_used, disk_total, disk_used,
		net_in_delta, net_out_delta, load1, load5, load15
		FROM probe_history WHERE timestamp < ? ORDER BY timestamp, id`, cutoff)
	if err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("读取待归档探针历史失败: %w", err)
	}
	var written int64
	var scanErr error
	for rows.Next() {
		var (
			id, ts, netIn, netOut, uptime                      int64
			agentID, osType, kernel, publicIP, osVer, cpuModel string
			memTotal, memUsed, diskTotal, diskUsed             int64
			inDelta, outDelta                                  int64
			cpu, mem, disk, cpuMHz                             float64
			load1, load5, load15                               float64
		)
		if err := rows.Scan(&id, &agentID, &ts, &cpu, &mem, &disk,
			&netIn, &netOut, &osType, &kernel, &uptime, &publicIP, &osVer,
			&cpuModel, &cpuMHz, &memTotal, &memUsed, &diskTotal, &diskUsed,
			&inDelta, &outDelta, &load1, &load5, &load15); err != nil {
			scanErr = err
			break
		}
		record := []string{
			strconv.FormatInt(id, 10), agentID, strconv.FormatInt(ts, 10),
			formatFloat(cpu), formatFloat(mem), formatFloat(disk),
			strconv.FormatInt(netIn, 10), strconv.FormatInt(netOut, 10),
			osType, kernel, strconv.FormatInt(uptime, 10), publicIP, osVer,
			cpuModel, formatFloat(cpuMHz),
			strconv.FormatInt(memTotal, 10), strconv.FormatInt(memUsed, 10),
			strconv.FormatInt(diskTotal, 10), strconv.FormatInt(diskUsed, 10),
			strconv.FormatInt(inDelta, 10), strconv.FormatInt(outDelta, 10),
			formatFloat(load1), formatFloat(load5), formatFloat(load15),
		}
		if err := cw.Write(record); err != nil {
			scanErr = err
			break
		}
		written++
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if scanErr == nil {
		scanErr = rowsErr
	}
	if scanErr != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("导出探针历史失败: %w", scanErr)
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("写入归档 CSV 失败: %w", err)
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("关闭 gzip 失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return 0, "", 0, 0, fmt.Errorf("同步归档文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", 0, 0, fmt.Errorf("关闭归档文件失败: %w", err)
	}
	if written != count {
		_ = os.Remove(tmpPath)
		return 0, "", 0, 0, fmt.Errorf("归档行数不一致：应有 %d 条，实际写入 %d 条", count, written)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, "", 0, 0, fmt.Errorf("归档文件落盘失败: %w", err)
	}
	return written, finalPath, minTS, maxTS, nil
}

func compactLatencyRange(tx *sql.Tx, from, to int64, bucketSec int) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS tmp_latency_compact`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TEMP TABLE tmp_latency_compact AS
		SELECT agent_id, MAX(timestamp) AS timestamp, target, kind,
		       COALESCE(AVG(CASE WHEN ok = 1 THEN latency_ms END), 0) AS latency_ms,
		       MAX(ok) AS ok
		FROM latency_history
		WHERE timestamp >= ? AND timestamp < ?
		GROUP BY agent_id, target, kind, timestamp / ?`, from, to, bucketSec)
	if err != nil {
		return fmt.Errorf("聚合延迟历史失败: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM latency_history WHERE timestamp >= ? AND timestamp < ?`, from, to); err != nil {
		return fmt.Errorf("替换延迟历史失败（删除）: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO latency_history
		(agent_id, timestamp, target, kind, latency_ms, ok)
		SELECT agent_id, timestamp, target, kind, latency_ms, ok FROM tmp_latency_compact`); err != nil {
		return fmt.Errorf("替换延迟历史失败（写入）: %w", err)
	}
	_, err = tx.Exec(`DROP TABLE IF EXISTS tmp_latency_compact`)
	return err
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CompactStorage 回收 SQLite 文件空间。
//
// 首次把 auto_vacuum 切到 INCREMENTAL 需要一次 VACUUM（存量百万行库会明显缩小）；
// 之后只需 incremental_vacuum 归还删除产生的空闲页。VACUUM 期间单连接会被占用，
// 因此只在启动压缩后或一次性清理量很大时调用。
func (db *DB) CompactStorage() error {
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

	var autoVacuum int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		autoVacuum = 0
	}
	if autoVacuum != 2 {
		if _, err := db.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err == nil {
			if _, err := db.Exec(`VACUUM`); err != nil {
				return fmt.Errorf("VACUUM 失败: %w", err)
			}
		}
	} else {
		if _, err := db.Exec(`PRAGMA incremental_vacuum`); err != nil {
			return fmt.Errorf("incremental_vacuum 失败: %w", err)
		}
	}
	_, _ = db.Exec(`PRAGMA optimize`)
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}
