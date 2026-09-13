// Package storage 提供 SQLite 持久化存储能力。
//
// 使用 modernc.org/sqlite（纯 Go 实现，无需 CGO），通过 database/sql 标准接口
// 管理端口映射规则、审计日志、探针历史、主机信息、命令黑名单等数据的持久化。
package storage

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB 全局 SQLite 数据库句柄。
type DB struct {
	*sql.DB
}

// Open 打开（或创建）SQLite 数据库文件，并执行所有建表迁移。
func Open(dbPath string) (*DB, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", dbPath)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 单连接，避免 SQLite 并发写冲突
	sdb.SetMaxOpenConns(1)
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}
	db := &DB{DB: sdb}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}
	log.Printf("[storage] SQLite 数据库已就绪: %s", dbPath)
	return db, nil
}

// migrate 执行建表迁移（幂等）。
func (db *DB) migrate() error {
	migrations := []string{
		// 端口映射规则
		`CREATE TABLE IF NOT EXISTS portmap_rules (
			public_port  INTEGER PRIMARY KEY,
			id           TEXT    NOT NULL,
			agent_id     TEXT    NOT NULL,
			local_host   TEXT    NOT NULL DEFAULT '127.0.0.1',
			local_port   INTEGER NOT NULL,
			enabled      INTEGER NOT NULL DEFAULT 1,
			source_ip    TEXT    NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL DEFAULT 0
		)`,
		// 审计日志
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			time          INTEGER NOT NULL,
			operator      TEXT    NOT NULL DEFAULT '',
			operator_ip   TEXT    NOT NULL DEFAULT '',
			agent_id      TEXT    NOT NULL DEFAULT '',
			command_id    TEXT    NOT NULL DEFAULT '',
			command       TEXT    NOT NULL DEFAULT '',
			result        TEXT    NOT NULL DEFAULT '',
			exit_code     INTEGER NOT NULL DEFAULT 0,
			blocked       INTEGER NOT NULL DEFAULT 0,
			block_reason  TEXT    NOT NULL DEFAULT '',
			duration_ms   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_logs(time)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_agent ON audit_logs(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_operator ON audit_logs(operator)`,
		// 探针历史
		`CREATE TABLE IF NOT EXISTS probe_history (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id        TEXT    NOT NULL,
			timestamp       INTEGER NOT NULL,
			cpu_usage       REAL    NOT NULL DEFAULT 0,
			mem_usage       REAL    NOT NULL DEFAULT 0,
			disk_usage      REAL    NOT NULL DEFAULT 0,
			net_in_bytes    INTEGER NOT NULL DEFAULT 0,
			net_out_bytes   INTEGER NOT NULL DEFAULT 0,
			os_type         TEXT    NOT NULL DEFAULT '',
			kernel_version  TEXT    NOT NULL DEFAULT '',
			uptime          INTEGER NOT NULL DEFAULT 0,
			public_ip       TEXT    NOT NULL DEFAULT '',
			os_version      TEXT    NOT NULL DEFAULT '',
			cpu_model       TEXT    NOT NULL DEFAULT '',
			cpu_mhz         REAL    NOT NULL DEFAULT 0,
			mem_total       INTEGER NOT NULL DEFAULT 0,
			mem_used        INTEGER NOT NULL DEFAULT 0,
			disk_total      INTEGER NOT NULL DEFAULT 0,
			disk_used       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_probe_agent_time ON probe_history(agent_id, timestamp)`,
		// 主机信息
		`CREATE TABLE IF NOT EXISTS agents (
			agent_id        TEXT    PRIMARY KEY,
			uuid            TEXT    NOT NULL DEFAULT '',
			hostname        TEXT    NOT NULL DEFAULT '',
			os_type         TEXT    NOT NULL DEFAULT '',
			arch            TEXT    NOT NULL DEFAULT '',
			kernel_version  TEXT    NOT NULL DEFAULT '',
			online          INTEGER NOT NULL DEFAULT 0,
			last_seen       INTEGER NOT NULL DEFAULT 0,
			last_offline_at INTEGER NOT NULL DEFAULT 0,
			uptime          INTEGER NOT NULL DEFAULT 0,
			cpu_usage       REAL    NOT NULL DEFAULT 0,
			mem_usage       REAL    NOT NULL DEFAULT 0,
			disk_usage      REAL    NOT NULL DEFAULT 0,
			net_in_bytes    INTEGER NOT NULL DEFAULT 0,
			net_out_bytes   INTEGER NOT NULL DEFAULT 0,
			remote_addr     TEXT    NOT NULL DEFAULT '',
			public_ip       TEXT    NOT NULL DEFAULT '',
			public_ipv6     TEXT    NOT NULL DEFAULT '',
			os_version      TEXT    NOT NULL DEFAULT '',
			cpu_model       TEXT    NOT NULL DEFAULT '',
			cpu_mhz         REAL    NOT NULL DEFAULT 0,
			mem_total       INTEGER NOT NULL DEFAULT 0,
			mem_used        INTEGER NOT NULL DEFAULT 0,
			disk_total      INTEGER NOT NULL DEFAULT 0,
			disk_used       INTEGER NOT NULL DEFAULT 0,
			net_rate_in     INTEGER NOT NULL DEFAULT 0,
			net_rate_out    INTEGER NOT NULL DEFAULT 0,
			updated_at      INTEGER NOT NULL DEFAULT 0
		)`,
		// 命令黑名单
		`CREATE TABLE IF NOT EXISTS blacklist (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern TEXT    NOT NULL UNIQUE,
			builtin INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("执行迁移失败 [%s]: %w", m, err)
		}
	}
	// 兼容旧库：先补齐列，再建依赖这些列的索引。
	// 注意顺序——索引必须在列存在之后创建，否则旧库启动即失败。
	if err := db.ensureColumn("agents", "uuid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 缺失 command_id 会导致审计写入直接报错，表现为「操作审计看不到记录」。
	if err := db.ensureColumn("audit_logs", "command_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 依赖上述列的索引（放在补列之后执行）
	postMigrations := []string{
		// 硬件 UUID 索引：用于按设备唯一标识定位/去重主机
		`CREATE INDEX IF NOT EXISTS idx_agents_uuid ON agents(uuid)`,
		// 命令 ID 索引：用于审计结果回填与按命令追踪
		`CREATE INDEX IF NOT EXISTS idx_audit_command_id ON audit_logs(command_id)`,
	}
	for _, m := range postMigrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("执行迁移失败 [%s]: %w", m, err)
		}
	}
	return nil
}

// ensureColumn 检查表中是否存在指定列，缺失则通过 ALTER TABLE 补上（用于老库平滑升级）。
func (db *DB) ensureColumn(table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("读取表结构失败 [%s]: %w", table, err)
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("为表 %s 添加列 %s 失败: %w", table, column, err)
	}
	return nil
}

// Close 关闭数据库。
func (db *DB) Close() error {
	return db.DB.Close()
}
