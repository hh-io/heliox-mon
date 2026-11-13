//go:build darwin

package collector

import (
	"log"
	"math/rand"
	"runtime"
	"time"
)

// doCollectTraffic 模拟流量采集
func (c *Collector) doCollectTraffic() {
	now := time.Now().Unix()

	// 模拟随机流量 (0 - 10MB)
	tx := uint64(rand.Int63n(10 * 1024 * 1024))
	rx := uint64(rand.Int63n(10 * 1024 * 1024))

	// 累加到假的总计数器 (模拟 /proc/net/dev 递增)
	totalTx := c.lastTotalTx.Add(tx)
	totalRx := c.lastTotalRx.Add(rx)

	_, err := c.db.Exec(
		"INSERT INTO traffic_snapshots (ts, iface, tx_bytes, rx_bytes) VALUES (?, 'total', ?, ?)",
		now, totalTx, totalRx,
	)
	if err != nil {
		log.Printf("[Mock] 保存流量快照失败: %v", err)
	}

	// 模拟端口流量
	for _, port := range c.cfg.ActivePorts() {
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
				TxBytes: c.lastTotalTx.Load(),
				RxBytes: c.lastTotalRx.Load(),
				TxSpeed: tx,
				RxSpeed: rx,
			}
			c.realtimeMu.Unlock()
		}
	}
}

// mockRound 已完成的采集轮次（只在系统采集协程内读写）。
// 用来让首轮走「还没有基准」的分支，本地开发时才能看到线上首次启动的占位符表现
var mockRound int

// doCollectSystemMetrics 模拟系统资源采集
func (c *Collector) doCollectSystemMetrics() {
	mockRound++
	// CPU 使用率与重传率都要两次采样才算得出，首轮无值可给
	hasBaseline := mockRound > 1

	memTotal := uint64(16 * 1024 * 1024 * 1024)   // 16GB
	diskTotal := uint64(512 * 1024 * 1024 * 1024) // 512GB
	diskUsed := uint64(rand.Float64() * float64(diskTotal))

	portConns := make(map[int]int)
	for _, port := range c.cfg.ActivePorts() {
		portConns[port] = rand.Intn(120)
	}

	steal := rand.Float64() * 3
	var stealAvg float64
	if hasBaseline {
		stealAvg = c.pushSteal(steal)
	}

	c.setSystemSnapshot(SystemSnapshot{
		Ts:              time.Now().Unix(),
		CPUPercent:      rand.Float64() * 100,
		CPUValid:        hasBaseline,
		StealPercent:    steal,
		StealAvgPercent: stealAvg,
		CPUCores:        runtime.NumCPU(),
		MemUsed:         uint64(rand.Float64() * float64(memTotal)),
		MemTotal:        memTotal,
		DiskUsed:        diskUsed,
		DiskAvail:       diskTotal - diskUsed,
		DiskTotal:       diskTotal,
		Load1:           rand.Float64() * 2,
		Load5:           rand.Float64() * 2,
		Load15:          rand.Float64() * 2,
		PortConns:       portConns,
		RetransPercent:  rand.Float64() * 2,
		RetransValid:    hasBaseline,
		UptimeSec:       int64(rand.Intn(30 * 86400)),
	})
}

// doCollectLatency 模拟延迟采集
func (c *Collector) doCollectLatency() {
	now := time.Now().Unix()
	count := c.cfg.PingCount
	if count <= 0 {
		count = 20
	}

	for _, target := range c.cfg.PingTargets {
		// 模拟一条基线附近的逐包样本，偶发丢包，走与 Linux 相同的入库摘要
		base := 20.0 + rand.Float64()*80.0
		samples := make([]float64, 0, count)
		lost := 0
		for i := 0; i < count; i++ {
			if rand.Float64() < 0.02 {
				lost++
				continue
			}
			v := base + rand.NormFloat64()*3
			if v < 1 {
				v = 1
			}
			samples = append(samples, v)
		}
		s := summarizeSamples(samples, count, lost)
		if err := c.insertLatency(now, target.IP, s); err != nil {
			log.Printf("[Mock] 保存延迟数据失败: %v", err)
		}
	}
}
