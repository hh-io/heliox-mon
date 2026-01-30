package collector

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// collectRealtimeSpeed 每秒采集流量并计算实时网速（只更新内存，不写数据库）
func (c *Collector) collectRealtimeSpeed() {
	defer c.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastTx, lastRx uint64
	var lastTs int64

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			tx, rx, err := c.readProcNetDev()
			if err != nil {
				continue
			}
			now := time.Now().Unix()

			// 计算速度
			var txSpeed, rxSpeed float64
			if lastTs > 0 && now > lastTs {
				dt := float64(now - lastTs)
				if tx >= lastTx {
					txSpeed = float64(tx-lastTx) / dt
				}
				if rx >= lastRx {
					rxSpeed = float64(rx-lastRx) / dt
				}
			}

			// 更新内存快照
			c.realtimeMu.Lock()
			c.realtimeSnapshot = RealtimeSnapshot{
				Ts:      now,
				TxBytes: tx + c.totalTxOffset,
				RxBytes: rx + c.totalRxOffset,
				TxSpeed: txSpeed,
				RxSpeed: rxSpeed,
			}
			c.realtimeMu.Unlock()

			lastTx = tx
			lastRx = rx
			lastTs = now
		}
	}
}

// doCollectTraffic 执行流量采集
func (c *Collector) doCollectTraffic() {
	now := time.Now().Unix()

	// 1. 采集整体流量
	tx, rx, err := c.readProcNetDev()
	if err != nil {
		log.Printf("读取流量数据失败: %v", err)
		return
	}

	// 检测计数器重置（当前值 < 上次值表示重启或溢出）
	if c.lastTotalTx > 0 && tx < c.lastTotalTx {
		// 计数器重置，累加上次值到偏移量
		c.totalTxOffset += c.lastTotalTx
		log.Printf("检测到 TX 计数器重置，累加偏移量: %d", c.lastTotalTx)
	}
	if c.lastTotalRx > 0 && rx < c.lastTotalRx {
		c.totalRxOffset += c.lastTotalRx
		log.Printf("检测到 RX 计数器重置，累加偏移量: %d", c.lastTotalRx)
	}

	// 保存快照（加上偏移量）
	adjustedTx := tx + c.totalTxOffset
	adjustedRx := rx + c.totalRxOffset

	_, err = c.db.Exec(
		"INSERT INTO traffic_snapshots (ts, iface, tx_bytes, rx_bytes) VALUES (?, 'total', ?, ?)",
		now, adjustedTx, adjustedRx,
	)
	if err != nil {
		log.Printf("保存流量快照失败: %v", err)
	}

	c.lastTotalTx = tx
	c.lastTotalRx = rx

	// 2. 采集端口流量（如果配置了端口）
	if c.cfg.SnellPort > 0 || c.cfg.VlessPort > 0 {
		c.collectPortTraffic(now)
	}
}

// initTrafficOffsets 初始化计数器偏移量（用于服务重启后的连续性）
func (c *Collector) initTrafficOffsets() {
	// 总流量偏移
	rawTx, rawRx, err := c.readProcNetDev()
	if err == nil {
		c.lastTotalTx = rawTx
		c.lastTotalRx = rawRx

		var lastTx, lastRx int64
		row := c.db.QueryRow(
			"SELECT tx_bytes, rx_bytes FROM traffic_snapshots WHERE iface = 'total' ORDER BY ts DESC LIMIT 1",
		)
		if err := row.Scan(&lastTx, &lastRx); err == nil {
			if lastTx > 0 && uint64(lastTx) > rawTx {
				c.totalTxOffset = uint64(lastTx) - rawTx
			}
			if lastRx > 0 && uint64(lastRx) > rawRx {
				c.totalRxOffset = uint64(lastRx) - rawRx
			}
		}
	}

	// 端口流量偏移
	ports := []int{c.cfg.SnellPort, c.cfg.VlessPort}
	for _, port := range ports {
		if port == 0 {
			continue
		}

		rawPortTx, rawPortRx, err := c.readIptablesPortTraffic(port)
		if err != nil {
			continue
		}

		c.lastPortTx[port] = rawPortTx
		c.lastPortRx[port] = rawPortRx

		var lastTx, lastRx int64
		row := c.db.QueryRow(
			"SELECT tx_bytes, rx_bytes FROM port_traffic_snapshots WHERE port = ? ORDER BY ts DESC LIMIT 1",
			port,
		)
		if err := row.Scan(&lastTx, &lastRx); err == nil {
			if lastTx > 0 && uint64(lastTx) > rawPortTx {
				c.portTxOffset[port] = uint64(lastTx) - rawPortTx
			}
			if lastRx > 0 && uint64(lastRx) > rawPortRx {
				c.portRxOffset[port] = uint64(lastRx) - rawPortRx
			}
		}
	}
}

// readProcNetDev 从 /proc/net/dev 读取网络流量
func (c *Collector) readProcNetDev() (tx, rx uint64, err error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// 跳过头部
		if !strings.Contains(line, ":") {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}

		iface := strings.TrimSpace(parts[0])
		// 跳过 lo 和 docker 网桥
		if iface == "lo" || strings.HasPrefix(iface, "docker") || strings.HasPrefix(iface, "br-") || strings.HasPrefix(iface, "veth") {
			continue
		}

		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}

		// 字段顺序: rx_bytes, rx_packets, ..., tx_bytes, tx_packets, ...
		// 索引 0 = rx_bytes, 索引 8 = tx_bytes
		ifaceRx, _ := strconv.ParseUint(fields[0], 10, 64)
		ifaceTx, _ := strconv.ParseUint(fields[8], 10, 64)

		rx += ifaceRx
		tx += ifaceTx
	}

	return tx, rx, scanner.Err()
}

// collectPortTraffic 采集端口流量（通过 iptables）
func (c *Collector) collectPortTraffic(now int64) {
	ports := []int{}
	if c.cfg.SnellPort > 0 {
		ports = append(ports, c.cfg.SnellPort)
	}
	if c.cfg.VlessPort > 0 {
		ports = append(ports, c.cfg.VlessPort)
	}

	if len(ports) == 0 {
		return
	}

	counters, err := c.readIptablesPortsTraffic(ports)
	if err != nil {
		// iptables 规则可能不存在，静默失败
		return
	}

	for _, port := range ports {
		stats, ok := counters[port]
		if !ok || !stats.txOK || !stats.rxOK {
			continue
		}
		tx := stats.tx
		rx := stats.rx

		// 检测计数器重置
		lastTx := c.lastPortTx[port]
		lastRx := c.lastPortRx[port]
		if lastTx > 0 && tx < lastTx {
			c.portTxOffset[port] += lastTx
			log.Printf("检测到端口 %d TX 计数器重置，累加偏移量: %d", port, lastTx)
		}
		if lastRx > 0 && rx < lastRx {
			c.portRxOffset[port] += lastRx
			log.Printf("检测到端口 %d RX 计数器重置，累加偏移量: %d", port, lastRx)
		}

		// 保存快照（加上偏移量）
		adjustedTx := tx + c.portTxOffset[port]
		adjustedRx := rx + c.portRxOffset[port]

		_, err = c.db.Exec(
			"INSERT INTO port_traffic_snapshots (ts, port, tx_bytes, rx_bytes) VALUES (?, ?, ?, ?)",
			now, port, adjustedTx, adjustedRx,
		)
		if err != nil {
			log.Printf("保存端口 %d 流量快照失败: %v", port, err)
		}

		c.lastPortTx[port] = tx
		c.lastPortRx[port] = rx
	}
}

type portCounters struct {
	tx   uint64
	rx   uint64
	txOK bool
	rxOK bool
}

func (c *Collector) readIptablesPortsTraffic(ports []int) (map[int]portCounters, error) {
	// 使用 iptables-save -c 获取计数器
	// 输出格式固定：[pkts:bytes] -A HELIOX_STATS -p tcp --dport 443
	cmd := exec.Command("iptables-save", "-c", "-t", "filter")
	output, err := cmd.Output()
	if err != nil {
		// 回退到传统方法
		return c.readIptablesPortsTrafficLegacy(ports)
	}

	counters := make(map[int]portCounters, len(ports))
	targets := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		targets[port] = struct{}{}
		counters[port] = portCounters{}
	}

	// 正则匹配：[pkts:bytes] -A HELIOX_STATS -p tcp -m tcp --(dport|sport) 443
	// 注意：HELIOX_STATS 和 -p 之间可能没有额外内容，使用 .* 而非 .+
	re := regexp.MustCompile(`\[(\d+):(\d+)\] -A HELIOX_STATS\s+-p\s+(tcp|udp)\s+.*--(dport|sport)\s+(\d+)`)

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		match := re.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		// match[1] = pkts, match[2] = bytes, match[3] = proto, match[4] = dport/sport, match[5] = port
		bytes, _ := strconv.ParseUint(match[2], 10, 64)
		port, _ := strconv.Atoi(match[5])
		direction := match[4] // "dport" or "sport"

		if _, ok := targets[port]; !ok {
			continue
		}

		entry := counters[port]
		if direction == "dport" {
			entry.rx += bytes
			entry.rxOK = true
		} else if direction == "sport" {
			entry.tx += bytes
			entry.txOK = true
		}
		counters[port] = entry
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return counters, nil
}

// readIptablesPortsTrafficLegacy 回退方法（兼容旧版本 iptables）
func (c *Collector) readIptablesPortsTrafficLegacy(ports []int) (map[int]portCounters, error) {
	cmd := exec.Command("iptables", "-L", "HELIOX_STATS", "-n", "-v", "-x")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("iptables 命令执行失败: %w", err)
	}

	counters := make(map[int]portCounters, len(ports))
	targets := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		targets[port] = struct{}{}
		counters[port] = portCounters{}
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		proto := fields[3]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if !strings.Contains(line, "dpt:") && !strings.Contains(line, "spt:") {
			continue
		}
		bytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		for port := range targets {
			if strings.Contains(line, fmt.Sprintf("dpt:%d", port)) {
				entry := counters[port]
				entry.rx += bytes
				entry.rxOK = true
				counters[port] = entry
			}
			if strings.Contains(line, fmt.Sprintf("spt:%d", port)) {
				entry := counters[port]
				entry.tx += bytes
				entry.txOK = true
				counters[port] = entry
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return counters, nil
}

// readIptablesPortTraffic 从 iptables 读取端口流量
// 需要预先设置 iptables 规则：
// iptables -N HELIOX_STATS
// iptables -I INPUT -j HELIOX_STATS
// iptables -I OUTPUT -j HELIOX_STATS
// iptables -A HELIOX_STATS -p tcp --sport <port>  # TX
// iptables -A HELIOX_STATS -p tcp --dport <port>  # RX
func (c *Collector) readIptablesPortTraffic(port int) (tx, rx uint64, err error) {
	counters, err := c.readIptablesPortsTraffic([]int{port})
	if err != nil {
		return 0, 0, err
	}
	stats, ok := counters[port]
	if !ok || !stats.txOK || !stats.rxOK {
		return 0, 0, fmt.Errorf("iptables 规则不存在: port %d", port)
	}
	return stats.tx, stats.rx, nil
}

// getIptablesBytes 解析 iptables 输出获取字节数
func (c *Collector) getIptablesBytes(chain, portType string, port int) (uint64, error) {
	cmd := exec.Command("iptables", "-L", chain, "-n", "-v", "-x")
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("iptables 命令执行失败: %w", err)
	}

	// 查找匹配的行：... dpt:443 或 spt:443
	target := fmt.Sprintf("%s:%d", portType, port)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, target) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				bytes, _ := strconv.ParseUint(fields[1], 10, 64)
				return bytes, nil
			}
		}
	}
	// 规则不存在时返回特定错误
	return 0, fmt.Errorf("iptables 规则不存在: %s:%d", portType, port)
}
