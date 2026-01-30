// Package collector 数据采集器
package collector

import (
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
)

// Collector 数据采集器
type Collector struct {
	cfg      *config.Config
	db       *storage.DB
	notifier Notifier
	stop     chan struct{}
	wg       sync.WaitGroup

	// 上次采集的流量数据（用于计算增量）
	lastTotalTx uint64
	lastTotalRx uint64
	lastPortTx  map[int]uint64
	lastPortRx  map[int]uint64

	// 计数器重置偏移量（用于处理重启/溢出）
	totalTxOffset uint64
	totalRxOffset uint64
	portTxOffset  map[int]uint64
	portRxOffset  map[int]uint64

	// CPU 采样（用于计算实时使用率）
	lastCPUTotal uint64
	lastCPUIdle  uint64

	// 实时快照（每秒更新，用于计算实时网速）
	realtimeSnapshot RealtimeSnapshot
	realtimeMu       sync.RWMutex
}

// RealtimeSnapshot 实时流量快照
type RealtimeSnapshot struct {
	Ts      int64
	TxBytes uint64
	RxBytes uint64
	TxSpeed float64 // bytes/s
	RxSpeed float64 // bytes/s
}

// Notifier 通知器接口
type Notifier interface {
	SendTrafficAlert(usedGB, limitGB int, percent float64, resetDate string, daysLeft int, threshold int) error
}

// New 创建采集器
func New(cfg *config.Config, db *storage.DB, notifier Notifier) *Collector {
	return &Collector{
		cfg:          cfg,
		db:           db,
		notifier:     notifier,
		stop:         make(chan struct{}),
		lastPortTx:   make(map[int]uint64),
		lastPortRx:   make(map[int]uint64),
		portTxOffset: make(map[int]uint64),
		portRxOffset: make(map[int]uint64),
	}
}

// GetRealtimeSnapshot 获取实时快照
func (c *Collector) GetRealtimeSnapshot() RealtimeSnapshot {
	c.realtimeMu.RLock()
	defer c.realtimeMu.RUnlock()
	return c.realtimeSnapshot
}

// GetRealtimeSpeed 获取实时网速（供 API 使用）
func (c *Collector) GetRealtimeSpeed() (txSpeed, rxSpeed float64, ts int64) {
	c.realtimeMu.RLock()
	defer c.realtimeMu.RUnlock()
	return c.realtimeSnapshot.TxSpeed, c.realtimeSnapshot.RxSpeed, c.realtimeSnapshot.Ts
}

// Start 启动采集器
func (c *Collector) Start() {
	// 初始化计数器偏移量，避免重启导致统计跳变
	c.initTrafficOffsets()

	// 系统资源采集（每 5 秒）
	c.wg.Add(1)
	go c.collectSystemMetrics()

	// 流量采集写入数据库（每 1 分钟）
	c.wg.Add(1)
	go c.collectTraffic()

	// 实时网速采集（每 1 秒，只更新内存）
	c.wg.Add(1)
	go c.collectRealtimeSpeed()

	// 延迟监控（每 1 分钟）
	c.wg.Add(1)
	go c.collectLatency()

	// 日汇总任务（每分钟检查一次）
	c.wg.Add(1)
	go c.runDailyAggregation()

	log.Println("采集器已启动")
}

// Stop 停止采集器
func (c *Collector) Stop() {
	close(c.stop)
	c.wg.Wait()
	log.Println("采集器已停止")
}

// collectSystemMetrics 采集系统资源
func (c *Collector) collectSystemMetrics() {
	defer c.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.doCollectSystemMetrics()
		}
	}
}

// collectTraffic 采集流量
func (c *Collector) collectTraffic() {
	defer c.wg.Done()
	ticker := time.NewTicker(1 * time.Minute) // 每分钟写入数据库，避免锁定
	defer ticker.Stop()

	// 初始采集
	c.doCollectTraffic()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.doCollectTraffic()
		}
	}
}

// collectLatency 采集延迟
func (c *Collector) collectLatency() {
	defer c.wg.Done()
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.doCollectLatency()
		}
	}
}

// runDailyAggregation 运行日汇总任务
func (c *Collector) runDailyAggregation() {
	defer c.wg.Done()

	// 启动时立即执行一次汇总
	c.doDailyAggregation()

	ticker := time.NewTicker(1 * time.Minute) // 每分钟更新日汇总
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.doDailyAggregation()
		}
	}
}

// doDailyAggregation 执行日汇总
func (c *Collector) doDailyAggregation() {
	now := time.Now().In(c.cfg.Timezone)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 汇总整体流量（今日 + 昨日）
	c.aggregateDailyTraffic(today)
	c.aggregateDailyTraffic(yesterday)

	// 汇总端口流量（今日 + 昨日）
	c.aggregatePortDailyTraffic(today)
	c.aggregatePortDailyTraffic(yesterday)

	// 汇总延迟数据（降采样）
	c.aggregateLatencyData()

	// 清理过期快照
	c.cleanupOldSnapshots()

	// 检查配额并发送通知
	c.checkQuotaAndNotify(now)
}

func (c *Collector) dayBounds(date string) (int64, int64, bool) {
	start, err := time.ParseInLocation("2006-01-02", date, c.cfg.Timezone)
	if err != nil {
		return 0, 0, false
	}
	end := start.Add(24*time.Hour - time.Second)
	return start.Unix(), end.Unix(), true
}

// aggregateDailyTraffic 汇总每日整体流量
func (c *Collector) aggregateDailyTraffic(date string) {
	startTs, endTs, ok := c.dayBounds(date)
	if !ok {
		return
	}

	// 获取当天的流量增量
	row := c.db.QueryRow(`
		SELECT MAX(tx_bytes) - MIN(tx_bytes), MAX(rx_bytes) - MIN(rx_bytes)
		FROM traffic_snapshots
		WHERE iface = 'total'
		  AND ts >= ? AND ts <= ?
	`, startTs, endTs)

	var tx, rx int64
	if err := row.Scan(&tx, &rx); err != nil || (tx <= 0 && rx <= 0) {
		return
	}

	// 插入或更新日汇总
	_, _ = c.db.Exec(`
		INSERT INTO traffic_daily (date, iface, tx_bytes, rx_bytes)
		VALUES (?, 'total', ?, ?)
		ON CONFLICT(date, iface) DO UPDATE SET tx_bytes = excluded.tx_bytes, rx_bytes = excluded.rx_bytes
	`, date, tx, rx)
}

// aggregatePortDailyTraffic 汇总端口流量
func (c *Collector) aggregatePortDailyTraffic(date string) {
	startTs, endTs, ok := c.dayBounds(date)
	if !ok {
		return
	}

	ports := []int{c.cfg.SnellPort, c.cfg.VlessPort}
	for _, port := range ports {
		if port == 0 {
			continue
		}

		row := c.db.QueryRow(`
			SELECT MAX(tx_bytes) - MIN(tx_bytes), MAX(rx_bytes) - MIN(rx_bytes)
			FROM port_traffic_snapshots
			WHERE port = ?
			  AND ts >= ? AND ts <= ?
		`, port, startTs, endTs)

		var tx, rx int64
		if err := row.Scan(&tx, &rx); err != nil || (tx <= 0 && rx <= 0) {
			continue
		}

		_, _ = c.db.Exec(`
			INSERT INTO port_traffic_daily (date, port, tx_bytes, rx_bytes)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(date, port) DO UPDATE SET tx_bytes = excluded.tx_bytes, rx_bytes = excluded.rx_bytes
		`, date, port, tx, rx)
	}
}

// aggregateLatencyData 延迟数据降采样
func (c *Collector) aggregateLatencyData() {
	// 删除 7 天前的原始数据，保留聚合数据
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	_, _ = c.db.Exec("DELETE FROM latency_records WHERE ts < ? AND is_aggregated = 0", cutoff)
}

// cleanupOldSnapshots 清理过期快照
func (c *Collector) cleanupOldSnapshots() {
	// 保留从“昨日零点”开始的流量快照，确保昨日统计完整且不随时间变小
	now := time.Now().In(c.cfg.Timezone)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.cfg.Timezone)
	cutoff := todayStart.AddDate(0, 0, -1).Unix()
	_, _ = c.db.Exec("DELETE FROM traffic_snapshots WHERE ts < ?", cutoff)
	_, _ = c.db.Exec("DELETE FROM port_traffic_snapshots WHERE ts < ?", cutoff)
}

// checkQuotaAndNotify 检查流量配额并发送通知
func (c *Collector) checkQuotaAndNotify(now time.Time) {
	if c.notifier == nil || c.cfg.MonthlyLimitGB <= 0 {
		return
	}

	// 获取计费周期
	billingStart, billingEnd := c.getBillingCycleDates(now)

	// 查询本月已用流量（按 tx/rx 分开计算）
	var tx, rx int64
	row := c.db.QueryRow(`
		SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
		FROM traffic_daily
		WHERE date >= ? AND iface = 'total'
	`, billingStart.Format("2006-01-02"))
	row.Scan(&tx, &rx)

	var usedBytes int64
	switch c.cfg.BillingMode {
	case "tx_only":
		usedBytes = tx
	case "rx_only":
		usedBytes = rx
	case "max_value":
		if tx > rx {
			usedBytes = tx
		} else {
			usedBytes = rx
		}
	default: // bidirectional
		usedBytes = tx + rx
	}

	limitGB := c.cfg.MonthlyLimitGB
	limitBytes := int64(limitGB) * 1024 * 1024 * 1024
	if limitBytes <= 0 {
		return
	}

	percent := float64(usedBytes) / float64(limitBytes) * 100
	usedGB := int(math.Round(float64(usedBytes) / float64(1024*1024*1024)))
	daysLeft := int(billingEnd.Sub(now).Hours() / 24)

	thresholds := append([]int(nil), c.cfg.AlertThresholds...)
	sort.Ints(thresholds)
	for _, threshold := range thresholds {
		if threshold <= 0 {
			continue
		}
		if percent >= float64(threshold) {
			resetDate := billingEnd.Format("2006-01-02")
			if err := c.notifier.SendTrafficAlert(usedGB, limitGB, percent, resetDate, daysLeft, threshold); err != nil {
				log.Printf("发送流量预警失败: %v", err)
			}
		}
	}
}

// getBillingCycleDates 计算计费周期
func (c *Collector) getBillingCycleDates(now time.Time) (start, end time.Time) {
	day := c.cfg.ResetDay
	tz := c.cfg.Timezone

	if now.Day() >= day {
		start = time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, tz)
		end = time.Date(now.Year(), now.Month()+1, day, 0, 0, 0, 0, tz).Add(-time.Second)
	} else {
		start = time.Date(now.Year(), now.Month()-1, day, 0, 0, 0, 0, tz)
		end = time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, tz).Add(-time.Second)
	}
	return
}
