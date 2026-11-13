// Package collector 数据采集器
package collector

import (
	"database/sql"
	"errors"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
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

	// 上次采集的流量数据（用于计算增量）。总量字段用原子类型：
	// 实时网速协程（每秒）与流量采集协程（每分钟）会并发访问，
	// 端口相关的 map 只在流量采集协程内使用，无需同步
	lastTotalTx atomic.Uint64
	lastTotalRx atomic.Uint64

	// 端口累计流量（写入快照的值）。由各地址族计数器的增量累加而来，
	// 不是原始计数器之和：v4/v6 两份计数器各自可能被重置或某轮读不到，
	// 直接求和再做回退检测，会把单边缺失误判为重置、把另一边重复计入
	lastPortTx  map[int]uint64
	lastPortRx  map[int]uint64
	lastPortRaw map[portFamilyKey]rawPortCounter
	// 各地址族最近一次读取失败的原因，只在状态变化时打日志，避免每分钟刷屏
	portReadErr map[string]string

	// 计数器重置偏移量（用于处理重启/溢出）
	totalTxOffset atomic.Uint64
	totalRxOffset atomic.Uint64

	// CPU 采样基准（只在系统采集协程内读写）。用独立的 bool 标记而非
	// 「total == 0」判断是否已有基准：total 本身可以合法地为任意值
	lastCPU        cpuTimes
	hasCPUBaseline bool

	// TCP 重传采样基准（同样只在系统采集协程内读写）
	lastTCP        tcpStats
	hasTCPBaseline bool

	// steal 滑动窗口（只在系统采集协程内读写）：单轮采样的 steal 抖动很常见，
	// 直接拿瞬时值判定「宿主机超售」会让角标反复闪烁，改用近 1 分钟均值
	stealWindow [stealWindowSize]float64
	stealFilled int
	stealNext   int

	// 连接数采集缓存（只在系统采集协程内读写）：
	// 扫描 /proc/net/tcp{,6} 是 O(连接数) 且内核持锁，连接数上万时不宜每 5 秒来一次
	lastConns   map[int]int
	lastConnsAt time.Time

	// 实时快照（每秒更新，用于计算实时网速）
	realtimeSnapshot RealtimeSnapshot
	realtimeMu       sync.RWMutex

	// 系统资源快照（每 5 秒更新，只存内存）
	systemSnapshot SystemSnapshot
	systemMu       sync.RWMutex
}

// RealtimeSnapshot 实时流量快照
type RealtimeSnapshot struct {
	Ts      int64
	TxBytes uint64
	RxBytes uint64
	TxSpeed float64 // bytes/s
	RxSpeed float64 // bytes/s
}

// SystemSnapshot 系统资源快照。
// 前端只关心「此刻」的资源占用，历史值从未被查询过，
// 因此不落库——避免每 5 秒一次 INSERT+DELETE 空耗 VPS 的磁盘 IO。
type SystemSnapshot struct {
	Ts int64

	CPUPercent      float64
	CPUValid        bool    // 首次采样只记基准，此时使用率不可用
	StealPercent    float64 // 本轮宿主机抢占占比
	StealAvgPercent float64 // 近 1 分钟均值，用于判定 VPS 所在物理机是否超售
	CPUCores        int

	MemUsed  uint64
	MemTotal uint64

	DiskUsed  uint64
	DiskAvail uint64 // 普通用户可写容量，比 total-used 少掉 root 保留块
	DiskTotal uint64

	Load1  float64
	Load5  float64
	Load15 float64

	// 各代理端口的 ESTABLISHED 连接数。采集协程每轮都新建 map 再整体替换，
	// 读取方拿到的 map 不会再被写入，因此无需额外加锁。
	// nil 表示这一轮没读到（而非「0 个连接」），API 会输出 null
	PortConns map[int]int

	RetransPercent float64 // 两次采样之间的 TCP 重传率
	RetransValid   bool

	UptimeSec int64
}

// cpuTimes /proc/stat 首行的累计 CPU 时间（单位 jiffies）
type cpuTimes struct {
	total uint64 // 各时间片之和
	idle  uint64 // idle + iowait：iowait 期间 CPU 同样是空闲的，不该算进使用率
	steal uint64 // 被宿主机抢走的时间片，VPS 超售的直接信号
}

// tcpStats /proc/net/snmp 中的 TCP 累计计数
type tcpStats struct {
	outSegs     uint64
	retransSegs uint64
}

// 系统采集每 5 秒一轮，12 轮即近 1 分钟
const stealWindowSize = 12

// pushSteal 记录本轮 steal 并返回近 1 分钟均值
func (c *Collector) pushSteal(v float64) float64 {
	c.stealWindow[c.stealNext] = v
	c.stealNext = (c.stealNext + 1) % stealWindowSize
	if c.stealFilled < stealWindowSize {
		c.stealFilled++
	}

	var sum float64
	// 窗口按顺序填充，未填满时前 stealFilled 个即全部有效样本
	for i := 0; i < c.stealFilled; i++ {
		sum += c.stealWindow[i]
	}
	return sum / float64(c.stealFilled)
}

// Notifier 通知器接口
type Notifier interface {
	SendTrafficAlert(usedGB, limitGB int, percent float64, resetDate string, daysLeft int, threshold int) error
	SendDailyReport() error
}

// New 创建采集器
func New(cfg *config.Config, db *storage.DB, notifier Notifier) *Collector {
	return &Collector{
		cfg:         cfg,
		db:          db,
		notifier:    notifier,
		stop:        make(chan struct{}),
		lastPortTx:  make(map[int]uint64),
		lastPortRx:  make(map[int]uint64),
		lastPortRaw: make(map[portFamilyKey]rawPortCounter),
		portReadErr: make(map[string]string),
	}
}

// portFamilyKey 标识某个地址族（ipv4/ipv6）下某端口的计数器
type portFamilyKey struct {
	family string
	port   int
}

// rawPortCounter 是 iptables/ip6tables 计数器的原始字节数
type rawPortCounter struct {
	tx uint64
	rx uint64
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

// GetSystemSnapshot 获取系统资源快照（供 API 使用）
func (c *Collector) GetSystemSnapshot() SystemSnapshot {
	c.systemMu.RLock()
	defer c.systemMu.RUnlock()
	return c.systemSnapshot
}

// setSystemSnapshot 由采集协程写入快照
func (c *Collector) setSystemSnapshot(s SystemSnapshot) {
	c.systemMu.Lock()
	defer c.systemMu.Unlock()
	c.systemSnapshot = s
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

	// 每日流量报告（需显式开启且配好 Telegram）
	if c.notifier != nil && c.cfg.DailyReportEnabled {
		if c.cfg.TelegramBotToken == "" || c.cfg.TelegramChatID == "" {
			log.Println("警告: DAILY_REPORT_ENABLED=true 但未配置 TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID，每日报告将不会发送")
		}
		c.wg.Add(1)
		go c.runDailyReport()
	}

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

	// 先采集一次，避免服务刚启动的 5 秒内接口无数据可返回
	c.doCollectSystemMetrics()

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

	// 延迟执行首次汇总，避免与其他 goroutine 初始化并发导致 SQLite 锁冲突
	time.Sleep(3 * time.Second)
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

// runDailyReport 每天在配置的整点推送一次流量报告。
// 用定时器对齐到「下一个触发时刻」而非每分钟轮询：重启后会重新计算下一次，
// 因此进程重启不会重复推送（session 同样存内存，与项目现状一致）。
func (c *Collector) runDailyReport() {
	defer c.wg.Done()

	// base 是推算下一次推送的基准时刻。推送后取「本次目标时刻」与「当前时刻」的较晚者：
	// 若时钟回拨导致唤醒时 now 仍早于目标整点，用 now 会把同一整点再算成下一次而重复推送
	base := time.Now().In(c.cfg.Timezone)
	for {
		next := nextReportTime(base, c.cfg.DailyReportHour, c.cfg.Timezone)
		log.Printf("每日流量报告下次推送时间: %s", next.Format("2006-01-02 15:04:05 MST"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-c.stop:
			timer.Stop()
			return
		case <-timer.C:
			if err := c.notifier.SendDailyReport(); err != nil {
				log.Printf("发送每日流量报告失败: %v", err)
			} else {
				log.Println("每日流量报告已推送")
			}
			base = next
			if now := time.Now().In(c.cfg.Timezone); now.After(base) {
				base = now
			}
		}
	}
}

// nextReportTime 计算 now 之后下一个 hour 整点（按 tz）
func nextReportTime(now time.Time, hour int, tz *time.Location) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, tz)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
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
	if _, err := c.db.Exec(`
		INSERT INTO traffic_daily (date, iface, tx_bytes, rx_bytes)
		VALUES (?, 'total', ?, ?)
		ON CONFLICT(date, iface) DO UPDATE SET tx_bytes = excluded.tx_bytes, rx_bytes = excluded.rx_bytes
	`, date, tx, rx); err != nil {
		log.Printf("写入日汇总流量失败 [%s]: %v", date, err)
	}
}

// aggregatePortDailyTraffic 汇总端口流量
func (c *Collector) aggregatePortDailyTraffic(date string) {
	startTs, endTs, ok := c.dayBounds(date)
	if !ok {
		return
	}

	for _, port := range c.cfg.ActivePorts() {
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

		if _, err := c.db.Exec(`
			INSERT INTO port_traffic_daily (date, port, tx_bytes, rx_bytes)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(date, port) DO UPDATE SET tx_bytes = excluded.tx_bytes, rx_bytes = excluded.rx_bytes
		`, date, port, tx, rx); err != nil {
			log.Printf("写入端口日汇总流量失败 [%s port %d]: %v", date, port, err)
		}
	}
}

// 延迟数据保留策略
const (
	latencyRawRetention = 7 * 24 * time.Hour  // 原始（每分钟）数据保留 7 天
	latencyAggRetention = 90 * 24 * time.Hour // 聚合数据保留 90 天
	latencyAggBucketSec = 600                 // 聚合粒度：10 分钟
)

// aggregateLatencyData 延迟数据降采样
// 将 7 天前的原始数据按 10 分钟桶聚合后写入（is_aggregated=1），再删除原始记录。
// 有逐包样本时桶内 P95/max 按包计算，写完丢掉 rtts，避免历史被直接删光。
func (c *Collector) aggregateLatencyData() {
	cutoff := time.Now().Add(-latencyRawRetention).Unix()

	tx, err := c.db.Begin()
	if err != nil {
		log.Printf("延迟降采样开启事务失败: %v", err)
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			log.Printf("延迟降采样回滚失败: %v", rbErr)
		}
	}()

	rows, err := tx.Query(`
		SELECT ts, target, rtt_ms, min_rtt, mdev, sent, lost,
		       max_rtt, p95_rtt, sum_rtt, sum_sq, recv, rtts
		FROM latency_records
		WHERE is_aggregated = 0 AND ts < ?
		ORDER BY target, ts
	`, cutoff)
	if err != nil {
		log.Printf("延迟降采样查询失败: %v", err)
		return
	}

	type bucketKey struct {
		target string
		ts     int64
	}
	groups := make(map[bucketKey][]LatencySummary)
	for rows.Next() {
		var ts int64
		var target string
		var rtt, minRtt, mdev, maxRtt, p95, sumRtt, sumSq sql.NullFloat64
		var sent, lost, recv sql.NullInt64
		var rtts sql.NullString
		if err := rows.Scan(&ts, &target, &rtt, &minRtt, &mdev, &sent, &lost,
			&maxRtt, &p95, &sumRtt, &sumSq, &recv, &rtts); err != nil {
			log.Printf("延迟降采样扫描失败: %v", err)
			rows.Close()
			return
		}
		s := scanLatencySummary(rtt, minRtt, mdev, maxRtt, p95, sumRtt, sumSq, sent, lost, recv, rtts)
		key := bucketKey{target: target, ts: (ts / latencyAggBucketSec) * latencyAggBucketSec}
		groups[key] = append(groups[key], s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Printf("延迟降采样遍历失败: %v", err)
		return
	}
	rows.Close()

	for key, parts := range groups {
		merged := mergeSummaries(parts)
		if err := execLatencyInsert(tx, key.ts, key.target, merged, true); err != nil {
			log.Printf("延迟降采样写入失败: %v", err)
			return
		}
	}

	if _, err := tx.Exec("DELETE FROM latency_records WHERE is_aggregated = 0 AND ts < ?", cutoff); err != nil {
		log.Printf("清理延迟原始数据失败: %v", err)
		return
	}

	aggCutoff := time.Now().Add(-latencyAggRetention).Unix()
	if _, err := tx.Exec("DELETE FROM latency_records WHERE is_aggregated = 1 AND ts < ?", aggCutoff); err != nil {
		log.Printf("清理过期聚合数据失败: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("延迟降采样提交失败: %v", err)
	}
}

// cleanupOldSnapshots 清理过期快照
func (c *Collector) cleanupOldSnapshots() {
	// 保留从“昨日零点”开始的流量快照，确保昨日统计完整且不随时间变小
	now := time.Now().In(c.cfg.Timezone)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.cfg.Timezone)
	cutoff := todayStart.AddDate(0, 0, -1).Unix()
	if _, err := c.db.Exec("DELETE FROM traffic_snapshots WHERE ts < ?", cutoff); err != nil {
		log.Printf("清理流量快照失败: %v", err)
	}
	if _, err := c.db.Exec("DELETE FROM port_traffic_snapshots WHERE ts < ?", cutoff); err != nil {
		log.Printf("清理端口流量快照失败: %v", err)
	}
}

// checkQuotaAndNotify 检查流量配额并发送通知
func (c *Collector) checkQuotaAndNotify(now time.Time) {
	if c.notifier == nil || c.cfg.MonthlyLimitGB <= 0 {
		return
	}

	billingStart, billingEnd := c.cfg.GetBillingCycleDates(now)

	// 查询本计费周期已用流量（按 tx/rx 分开计算）
	var tx, rx int64
	row := c.db.QueryRow(`
		SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
		FROM traffic_daily
		WHERE date >= ? AND iface = 'total'
	`, billingStart.Format("2006-01-02"))
	if err := row.Scan(&tx, &rx); err != nil {
		log.Printf("查询计费周期流量失败: %v", err)
	}

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

	// 只就「已跨过的最高阈值」发送一条预警，避免同时触达 80/90/95 三条消息。
	// 各阈值的 24h 冷却由 notifier 基于 alert_records 独立维护，
	// 因此用量逐步攀升时仍会按 90→95 的顺序依次提醒。
	thresholds := append([]int(nil), c.cfg.AlertThresholds...)
	sort.Ints(thresholds)
	highest := -1
	for _, threshold := range thresholds {
		if threshold > 0 && percent >= float64(threshold) {
			highest = threshold
		}
	}
	if highest > 0 {
		resetDate := billingEnd.Format("2006-01-02")
		if err := c.notifier.SendTrafficAlert(usedGB, limitGB, percent, resetDate, daysLeft, highest); err != nil {
			log.Printf("发送流量预警失败: %v", err)
		}
	}
}
