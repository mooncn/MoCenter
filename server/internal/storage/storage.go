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
	"regexp"

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
			duration_ms   INTEGER NOT NULL DEFAULT 0,
			category      TEXT    NOT NULL DEFAULT 'command'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_logs(time)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_agent ON audit_logs(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_operator ON audit_logs(operator)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_category_time ON audit_logs(category, time)`,
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
	// 审计类别（命令执行/运维操作/登录认证），旧库补列并回填默认值
	if err := db.ensureColumn("audit_logs", "category", "TEXT NOT NULL DEFAULT 'command'"); err != nil {
		return err
	}
	// 依赖上述列的索引（放在补列之后执行）
	// 历史遗留：同一台机器因 agent_id 变更（改配置/重装）被写成多条记录。
	// 先按 UUID 合并重复行（保留 last_seen 最新的一条），再建唯一索引，
	// 否则 CREATE UNIQUE INDEX 会因既有重复数据直接失败、服务端起不来。
	if err := db.dedupAgentsByUUID(); err != nil {
		return err
	}
	postMigrations := []string{
		// 硬件 UUID 唯一索引：一台设备一条记录，agent_id 变更不再产生冗余行。
		// 部分索引（uuid <> ''）：旧版 Agent 取不到 UUID 时 uuid 为空，允许多行共存。
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_uuid_unique ON agents(uuid) WHERE uuid <> ''`,
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

// dedupAgentsByUUID 按硬件 UUID 合并 agents 表中的重复设备行。
//
// 背景：早期版本以 agent_id 为唯一键，agent 改配置（agent_id 变化）后会插入
// 一条新记录，同一台机器出现多条 —— 表现为前端"一台机器变两台"、数据冗余。
// 此处做一次性收敛：UUID 相同的多行只保留 last_seen 最新的一条（agent_id 更新为
// 该行），其余删除；UUID 为空的行保持原样（无法判定是否同机）。
//
// 幂等：无重复时不做任何改动。
func (db *DB) dedupAgentsByUUID() error {
	type dup struct {
		uuid string
		keep string
	}
	rows, err := db.Query(`SELECT uuid, COUNT(*) AS n FROM agents
		WHERE uuid IS NOT NULL AND uuid <> '' GROUP BY uuid HAVING n > 1`)
	if err != nil {
		return fmt.Errorf("统计重复 UUID 主机失败: %w", err)
	}
	var dups []dup
	for rows.Next() {
		var d dup
		var n int
		if err := rows.Scan(&d.uuid, &n); err != nil {
			rows.Close()
			return err
		}
		dups = append(dups, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(dups) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开始合并重复主机事务失败: %w", err)
	}
	defer tx.Rollback()

	for _, d := range dups {
		// 保留 last_seen 最新的一条
		if err := tx.QueryRow(`SELECT agent_id FROM agents WHERE uuid = ?
			ORDER BY last_seen DESC LIMIT 1`, d.uuid).Scan(&d.keep); err != nil {
			return fmt.Errorf("定位 UUID %s 的主记录失败: %w", d.uuid, err)
		}
		// 历史采样归属重命名为保留的 agent_id（趋势图不断档）
		if _, err := tx.Exec(`UPDATE probe_history SET agent_id = ?
			WHERE agent_id IN (SELECT agent_id FROM agents WHERE uuid = ? AND agent_id <> ?)`,
			d.keep, d.uuid, d.keep); err != nil {
			return fmt.Errorf("合并 UUID %s 的探针历史失败: %w", d.uuid, err)
		}
		// 端口映射规则同样归并到保留的 agent_id
		if _, err := tx.Exec(`UPDATE portmap_rules SET agent_id = ?
			WHERE agent_id IN (SELECT agent_id FROM agents WHERE uuid = ? AND agent_id <> ?)`,
			d.keep, d.uuid, d.keep); err != nil {
			return fmt.Errorf("合并 UUID %s 的端口映射失败: %w", d.uuid, err)
		}
		if _, err := tx.Exec(`DELETE FROM agents WHERE uuid = ? AND agent_id <> ?`,
			d.uuid, d.keep); err != nil {
			return fmt.Errorf("删除 UUID %s 的冗余主机记录失败: %w", d.uuid, err)
		}
		log.Printf("[storage] 已按 UUID %s 合并重复主机记录，保留 agent_id=%s", d.uuid, d.keep)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交重复主机合并事务失败: %w", err)
	}
	return nil
}

// identPattern 用于校验表名/列名等 SQL 标识符，仅允许字母、数字和下划线，且不以数字开头。
// 标识符无法作为 SQL 占位符参数传递，只能拼接进语句，因此拼接前必须做白名单校验。
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// quoteIdent 校验并引用 SQL 标识符（表名/列名）。
// 非法标识符（含引号、分号、空白等）直接报错，从源头阻断 SQL 注入。
func quoteIdent(ident string) (string, error) {
	if !identPattern.MatchString(ident) {
		return "", fmt.Errorf("非法的 SQL 标识符: %q", ident)
	}
	return `"` + ident + `"`, nil
}

// ensureColumn 检查表中是否存在指定列，缺失则通过 ALTER TABLE 补上（用于老库平滑升级）。
func (db *DB) ensureColumn(table, column, definition string) error {
	// table/column 为 SQL 标识符，无法参数化，需先校验再引用，避免拼接注入。
	quotedTable, err := quoteIdent(table)
	if err != nil {
		return err
	}
	quotedColumn, err := quoteIdent(column)
	if err != nil {
		return err
	}
	rows, err := db.Query(`PRAGMA table_info(` + quotedTable + `)`)
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
	if _, err := db.Exec(`ALTER TABLE ` + quotedTable + ` ADD COLUMN ` + quotedColumn + ` ` + definition); err != nil {
		return fmt.Errorf("为表 %s 添加列 %s 失败: %w", table, column, err)
	}
	return nil
}

// Close 关闭数据库。
func (db *DB) Close() error {
	return db.DB.Close()
}
