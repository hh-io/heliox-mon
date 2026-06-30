// Package storage SQLite 存储层
package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB 数据库封装
type DB struct {
	*sql.DB
}

// NewDB 创建数据库连接并执行迁移
func NewDB(dataDir string) (*DB, error) {
	// 确保数据目录存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(dataDir, "heliox-mon.db")
	// WAL 模式 + 优化参数
	// 注意：modernc.org/sqlite 只识别 _pragma=xxx(val) 形式，
	// mattn 风格的 _journal_mode/_busy_timeout 等参数会被静默忽略，
	// 导致 WAL 未开启、busy_timeout=0，并发写立即 SQLITE_BUSY。
	// - busy_timeout=10000: 锁等待 10 秒
	// - cache_size=-64000: 64MB 内存缓存
	// - temp_store=2: 临时表存内存
	dsn := dbPath + "?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-64000)" +
		"&_pragma=temp_store(2)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接池优化：WAL 模式支持 1 writer + 多 readers
	db.SetMaxOpenConns(3) // 足够应对并发读写
	db.SetMaxIdleConns(2) // 保持 2 个空闲连接

	// 执行迁移
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return &DB{db}, nil
}

// migrate 执行数据库迁移
func migrate(db *sql.DB) error {
	migrations := []string{
		// 网络流量快照（每分钟采集）
		`CREATE TABLE IF NOT EXISTS traffic_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			iface TEXT NOT NULL DEFAULT 'total',
			tx_bytes INTEGER NOT NULL,
			rx_bytes INTEGER NOT NULL
		)`,
		// 复合索引匹配查询条件 (iface = ? AND ts BETWEEN ?)，避免全表扫描
		`CREATE INDEX IF NOT EXISTS idx_traffic_snapshots_iface_ts ON traffic_snapshots(iface, ts)`,

		// 端口流量快照
		`CREATE TABLE IF NOT EXISTS port_traffic_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			port INTEGER NOT NULL,
			tx_bytes INTEGER NOT NULL,
			rx_bytes INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_port_traffic_port_ts ON port_traffic_snapshots(port, ts)`,

		// 流量日汇总
		`CREATE TABLE IF NOT EXISTS traffic_daily (
			date TEXT NOT NULL,
			iface TEXT NOT NULL DEFAULT 'total',
			tx_bytes INTEGER NOT NULL,
			rx_bytes INTEGER NOT NULL,
			PRIMARY KEY (date, iface)
		)`,

		// 端口流量日汇总
		`CREATE TABLE IF NOT EXISTS port_traffic_daily (
			date TEXT NOT NULL,
			port INTEGER NOT NULL,
			tx_bytes INTEGER NOT NULL,
			rx_bytes INTEGER NOT NULL,
			PRIMARY KEY (date, port)
		)`,

		// 延迟监控
		`CREATE TABLE IF NOT EXISTS latency_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			target TEXT NOT NULL,
			rtt_ms REAL,
			min_rtt REAL,
			mdev REAL,
			sent INTEGER DEFAULT 0,
			lost INTEGER DEFAULT 0,
			is_aggregated INTEGER DEFAULT 0
		)`,
		// 复合索引匹配查询条件 (target = ? AND ts BETWEEN ?) 及聚合清理
		`CREATE INDEX IF NOT EXISTS idx_latency_target_ts ON latency_records(target, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_latency_agg_ts ON latency_records(is_aggregated, ts)`,

		// 系统资源快照
		`CREATE TABLE IF NOT EXISTS system_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			cpu_percent REAL,
			mem_used INTEGER,
			mem_total INTEGER,
			disk_used INTEGER,
			disk_total INTEGER,
			load_1 REAL,
			load_5 REAL,
			load_15 REAL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_system_metrics_ts ON system_metrics(ts)`,

		// 报警记录（用于冷却）
		`CREATE TABLE IF NOT EXISTS alert_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			threshold INTEGER NOT NULL,
			message TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_alert_ts ON alert_records(ts)`,

		// 配置表
		`CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("执行迁移失败 [%s]: %w", snippet(m, 50), err)
		}
	}

	// 兼容旧版本（增加丢包统计字段）
	_, _ = db.Exec("ALTER TABLE latency_records ADD COLUMN sent INTEGER DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE latency_records ADD COLUMN lost INTEGER DEFAULT 0")
	// 兼容旧版本（增加最小 RTT 与抖动字段，用于更准确的延迟参考）
	_, _ = db.Exec("ALTER TABLE latency_records ADD COLUMN min_rtt REAL")
	_, _ = db.Exec("ALTER TABLE latency_records ADD COLUMN mdev REAL")

	// 清理已被复合索引取代的旧单列索引（忽略不存在的情况）
	for _, idx := range []string{
		"idx_traffic_snapshots_ts", "idx_traffic_snapshots_iface",
		"idx_port_traffic_ts", "idx_port_traffic_port",
		"idx_latency_ts", "idx_latency_target",
	} {
		_, _ = db.Exec("DROP INDEX IF EXISTS " + idx)
	}

	return nil
}

// snippet 安全截断字符串用于日志（避免越界）
func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
