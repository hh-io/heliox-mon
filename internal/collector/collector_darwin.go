//go:build darwin

package collector

import (
	"log"
	"math/rand"
	"time"
)

// doCollectTraffic 模拟流量采集
func (c *Collector) doCollectTraffic() {
	now := time.Now().Unix()

	// 模拟随机流量 (0 - 10MB)
	tx := uint64(rand.Int63n(10 * 1024 * 1024))
	rx := uint64(rand.Int63n(10 * 1024 * 1024))

	// 累加到假的总计数器 (模拟 /proc/net/dev 递增)
	c.lastTotalTx += tx
	c.lastTotalRx += rx

	_, err := c.db.Exec(
		"INSERT INTO traffic_snapshots (ts, iface, tx_bytes, rx_bytes) VALUES (?, 'total', ?, ?)",
		now, c.lastTotalTx, c.lastTotalRx,
	)
	if err != nil {
		log.Printf("[Mock] 保存流量快照失败: %v", err)
	}

	// 模拟端口流量
	ports := []int{c.cfg.SnellPort, c.cfg.VlessPort}
	for _, port := range ports {
		if port == 0 {
			continue
		}

		// 端口流量少一点
		ptx := uint64(rand.Int63n(2 * 1024 * 1024))
		prx := uint64(rand.Int63n(5 * 1024 * 1024))

		// 维护端口计数器
		if _, ok := c.lastPortTx[port]; !ok {
			c.lastPortTx[port] = 0
			c.lastPortRx[port] = 0
		}
		c.lastPortTx[port] += ptx
		c.lastPortRx[port] += prx

		_, err := c.db.Exec(
			"INSERT INTO port_traffic_snapshots (ts, port, tx_bytes, rx_bytes) VALUES (?, ?, ?, ?)",
			now, port, c.lastPortTx[port], c.lastPortRx[port],
		)
		if err != nil {
			log.Printf("[Mock] 保存端口 %d 流量快照失败: %v", port, err)
		}
	}
}

// initTrafficOffsets 模拟初始化 (不做任何事)
func (c *Collector) initTrafficOffsets() {
	log.Println("[Mock] 初始化计数器偏移量... (Skip)")
}

// collectRealtimeSpeed 模拟实时网速采集
func (c *Collector) collectRealtimeSpeed() {
	defer c.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			tx := rand.Float64() * 5 * 1024 * 1024  // 0-5 MB/s
			rx := rand.Float64() * 10 * 1024 * 1024 // 0-10 MB/s
			now := time.Now().Unix()

			c.realtimeMu.Lock()
			c.realtimeSnapshot = RealtimeSnapshot{
				Ts:      now,
				TxBytes: c.lastTotalTx,
				RxBytes: c.lastTotalRx,
				TxSpeed: tx,
				RxSpeed: rx,
			}
			c.realtimeMu.Unlock()
		}
	}
}

// doCollectSystemMetrics 模拟系统资源采集
func (c *Collector) doCollectSystemMetrics() {
	now := time.Now().Unix()

	cpu := rand.Float64() * 100
	memTotal := uint64(16 * 1024 * 1024 * 1024) // 16GB
	memUsed := uint64(rand.Float64() * float64(memTotal))
	diskTotal := uint64(512 * 1024 * 1024 * 1024) // 512GB
	diskUsed := uint64(rand.Float64() * float64(diskTotal))
	load1 := rand.Float64() * 2
	load5 := rand.Float64() * 2
	load15 := rand.Float64() * 2

	_, err := c.db.Exec(
		`INSERT INTO system_metrics (ts, cpu_percent, mem_used, mem_total, disk_used, disk_total, load_1, load_5, load_15)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now, cpu, memUsed, memTotal, diskUsed, diskTotal, load1, load5, load15,
	)
	if err != nil {
		log.Printf("[Mock] 保存系统指标失败: %v", err)
	}
}

// doCollectLatency 模拟延迟采集
func (c *Collector) doCollectLatency() {
	now := time.Now().Unix()

	for _, target := range c.cfg.PingTargets {
		// 模拟 10ms - 300ms 随机延迟
		rtt := 10.0 + rand.Float64()*290.0

		_, err := c.db.Exec(
			"INSERT INTO latency_records (ts, target, rtt_ms, sent, lost, is_aggregated) VALUES (?, ?, ?, ?, ?, 0)",
			now, target.IP, rtt, 5, 0,
		)
		if err != nil {
			log.Printf("[Mock] 保存延迟数据失败: %v", err)
		}
	}
}
