package collector

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 预编译正则（避免每次采集重复编译）
var reIptablesCounter = regexp.MustCompile(`\[(\d+):(\d+)\] -A HELIOX_STATS\s+-p\s+(tcp|udp)\s+.*--(dport|sport)\s+(\d+)`)

// 需要排除的虚拟/隧道接口前缀（这些接口的流量会与物理网卡重复）
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "vmnet", // 容器/虚拟机网桥
	"tun", "tap", "wg", "cloudflared", // VPN/隧道
	"tailscale", "zt", "gre", "sit", "ip6tnl", // 其他隧道
}

// isVirtualIface 判断是否为应排除的虚拟/隧道接口
func isVirtualIface(iface string) bool {
	if iface == "" || iface == "lo" {
		return true
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(iface, p) {
			return true
		}
	}
	return false
}

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
				TxBytes: tx + c.totalTxOffset.Load(),
				RxBytes: rx + c.totalRxOffset.Load(),
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
	if lastTx := c.lastTotalTx.Load(); lastTx > 0 && tx < lastTx {
		// 计数器重置，累加上次值到偏移量
		c.totalTxOffset.Add(lastTx)
		log.Printf("检测到 TX 计数器重置，累加偏移量: %d", lastTx)
	}
	if lastRx := c.lastTotalRx.Load(); lastRx > 0 && rx < lastRx {
		c.totalRxOffset.Add(lastRx)
		log.Printf("检测到 RX 计数器重置，累加偏移量: %d", lastRx)
	}

	// 保存快照（加上偏移量）
	adjustedTx := tx + c.totalTxOffset.Load()
	adjustedRx := rx + c.totalRxOffset.Load()

	_, err = c.db.Exec(
		"INSERT INTO traffic_snapshots (ts, iface, tx_bytes, rx_bytes) VALUES (?, 'total', ?, ?)",
		now, adjustedTx, adjustedRx,
	)
	if err != nil {
		log.Printf("保存流量快照失败: %v", err)
	}

	c.lastTotalTx.Store(tx)
	c.lastTotalRx.Store(rx)

	// 2. 采集端口流量（无启用端口时内部直接返回）
	c.collectPortTraffic(now)
}

// initTrafficOffsets 初始化计数器偏移量（用于服务重启后的连续性）
func (c *Collector) initTrafficOffsets() {
	// 总流量偏移
	rawTx, rawRx, err := c.readProcNetDev()
	if err == nil {
		c.lastTotalTx.Store(rawTx)
		c.lastTotalRx.Store(rawRx)

		var lastTx, lastRx int64
		row := c.db.QueryRow(
			"SELECT tx_bytes, rx_bytes FROM traffic_snapshots WHERE iface = 'total' ORDER BY ts DESC LIMIT 1",
		)
		if err := row.Scan(&lastTx, &lastRx); err == nil {
			if lastTx > 0 && uint64(lastTx) > rawTx {
				c.totalTxOffset.Store(uint64(lastTx) - rawTx)
			}
			if lastRx > 0 && uint64(lastRx) > rawRx {
				c.totalRxOffset.Store(uint64(lastRx) - rawRx)
			}
		}
	}

	// 端口累计值：启动时读一次计数器作为基准，并与库里最后一条快照衔接
	ports := c.cfg.ActivePorts()
	if len(ports) > 0 {
		readings := c.readPortCounters(ports)
		for _, port := range ports {
			c.seedPort(port, readings)
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
		// 跳过回环、容器网桥及各类隧道/虚拟接口，避免与物理网卡重复计入流量。
		// 代理（VLESS/Snell/WireGuard 等）的实际出入流量最终都经物理网卡转发，
		// 若同时统计 tun/wg/cloudflared 等隧道接口会导致流量翻倍，使配额统计失真。
		if isVirtualIface(iface) {
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

// 端口计数器的地址族。xray 监听 "::"（双栈），IPv6 客户端的流量只在 ip6tables 里有计数
const (
	familyV4 = "ipv4"
	familyV6 = "ipv6"
)

var portFamilies = []struct {
	name string
	bin  string
}{
	{familyV4, "iptables-save"},
	{familyV6, "ip6tables-save"},
}

// collectPortTraffic 采集端口流量（通过 iptables / ip6tables 计数器）
func (c *Collector) collectPortTraffic(now int64) {
	ports := c.cfg.ActivePorts()
	if len(ports) == 0 {
		return
	}

	readings := c.readPortCounters(ports)
	for _, port := range ports {
		if _, seeded := c.lastPortTx[port]; !seeded {
			// 启动时规则还没就绪等情况下没能建立基准，这里补上；本轮没有增量
			if !c.seedPort(port, readings) {
				continue
			}
		} else if !c.advancePort(port, readings) {
			continue
		}

		_, err := c.db.Exec(
			"INSERT INTO port_traffic_snapshots (ts, port, tx_bytes, rx_bytes) VALUES (?, ?, ?, ?)",
			now, port, c.lastPortTx[port], c.lastPortRx[port],
		)
		if err != nil {
			log.Printf("保存端口 %d 流量快照失败: %v", port, err)
		}
	}
}

// seedPort 为端口建立计数器基准，并让累计值与库里最后一条快照衔接（取两者较大者：
// 服务停机期间计数器未重置时，停机那段流量仍应计入）。没有任何地址族可读时返回 false
func (c *Collector) seedPort(port int, readings map[string]map[int]rawPortCounter) bool {
	var sumTx, sumRx uint64
	found := false
	for _, fam := range portFamilies {
		cur, ok := readings[fam.name][port]
		if !ok {
			continue
		}
		c.lastPortRaw[portFamilyKey{fam.name, port}] = cur
		sumTx += cur.tx
		sumRx += cur.rx
		found = true
	}
	if !found {
		return false
	}

	var dbTx, dbRx int64
	row := c.db.QueryRow(
		"SELECT tx_bytes, rx_bytes FROM port_traffic_snapshots WHERE port = ? ORDER BY ts DESC LIMIT 1",
		port,
	)
	switch err := row.Scan(&dbTx, &dbRx); {
	case err == nil:
		if dbTx > 0 && uint64(dbTx) > sumTx {
			sumTx = uint64(dbTx)
		}
		if dbRx > 0 && uint64(dbRx) > sumRx {
			sumRx = uint64(dbRx)
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		log.Printf("读取端口 %d 最近快照失败: %v", port, err)
	}

	c.lastPortTx[port] = sumTx
	c.lastPortRx[port] = sumRx
	return true
}

// advancePort 把各地址族计数器相对上一轮的增量累加到端口累计值。
// 某个地址族本轮读不到时不动它的基准，下一轮读到时增量自然补齐；
// 运行中新出现的地址族（如刚补上 ip6tables 规则）首轮只建基准。
// 没有任何地址族可读时返回 false，本轮不写快照
func (c *Collector) advancePort(port int, readings map[string]map[int]rawPortCounter) bool {
	var dTx, dRx uint64
	found := false
	for _, fam := range portFamilies {
		cur, ok := readings[fam.name][port]
		if !ok {
			continue
		}
		found = true
		key := portFamilyKey{fam.name, port}
		last, seen := c.lastPortRaw[key]
		c.lastPortRaw[key] = cur
		if !seen {
			continue
		}
		if cur.tx < last.tx || cur.rx < last.rx {
			log.Printf("检测到端口 %d 的 %s 计数器重置（规则被重建或系统重启）", port, fam.name)
		}
		dTx += counterDelta(last.tx, cur.tx)
		dRx += counterDelta(last.rx, cur.rx)
	}
	if !found {
		return false
	}
	c.lastPortTx[port] += dTx
	c.lastPortRx[port] += dRx
	return true
}

// counterDelta 计算累计计数器的增量；计数器回退视为从 0 重新开始
func counterDelta(last, cur uint64) uint64 {
	if cur >= last {
		return cur - last
	}
	return cur
}

// readPortCounters 读取各地址族下目标端口的计数器。
// 只收录 TX/RX 两个方向规则都在的端口；读取失败的地址族整体缺席
func (c *Collector) readPortCounters(ports []int) map[string]map[int]rawPortCounter {
	targets := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port > 0 {
			targets[port] = struct{}{}
		}
	}

	readings := make(map[string]map[int]rawPortCounter, len(portFamilies))
	for _, fam := range portFamilies {
		output, err := exec.Command(fam.bin, "-c", "-t", "filter").Output()
		if err != nil {
			c.reportPortReadErr(fam.name, fmt.Errorf("%s 执行失败: %w", fam.bin, err))
			continue
		}
		counters, err := parseIptablesSave(string(output), targets)
		if err != nil {
			c.reportPortReadErr(fam.name, fmt.Errorf("解析 %s 输出失败: %w", fam.bin, err))
			continue
		}
		c.reportPortReadErr(fam.name, nil)
		readings[fam.name] = counters
	}
	return readings
}

// reportPortReadErr 只在读取状态变化（首次失败、原因改变、恢复）时打日志：
// 规则丢失或命令缺失通常持续存在，逐轮打印会淹没其他日志
func (c *Collector) reportPortReadErr(family string, err error) {
	prev := c.portReadErr[family]
	if err == nil {
		if prev != "" {
			log.Printf("端口流量计数器（%s）读取已恢复", family)
			delete(c.portReadErr, family)
		}
		return
	}
	msg := err.Error()
	if msg != prev {
		log.Printf("端口流量计数器（%s）读取失败，该地址族流量暂不计入: %s", family, msg)
		c.portReadErr[family] = msg
	}
}

// parseIptablesSave 从 iptables-save -c / ip6tables-save -c 的输出中提取 HELIOX_STATS 链
// 的端口计数器。行格式：[pkts:bytes] -A HELIOX_STATS -p tcp -m tcp --dport 443。
// dport 计入 RX，sport 计入 TX，TCP/UDP 累加；两个方向都有规则的端口才收录
func parseIptablesSave(output string, targets map[int]struct{}) (map[int]rawPortCounter, error) {
	type partial struct {
		rawPortCounter
		txOK, rxOK bool
	}
	found := make(map[int]*partial, len(targets))

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		match := reIptablesCounter.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		// match[2] = bytes, match[4] = dport/sport, match[5] = port
		port, err := strconv.Atoi(match[5])
		if err != nil {
			continue
		}
		if _, ok := targets[port]; !ok {
			continue
		}
		bytes, err := strconv.ParseUint(match[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("端口 %d 计数器非法: %w", port, err)
		}

		entry := found[port]
		if entry == nil {
			entry = &partial{}
			found[port] = entry
		}
		if match[4] == "dport" {
			entry.rx += bytes
			entry.rxOK = true
		} else {
			entry.tx += bytes
			entry.txOK = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	counters := make(map[int]rawPortCounter, len(found))
	for port, entry := range found {
		if entry.txOK && entry.rxOK {
			counters[port] = entry.rawPortCounter
		}
	}
	return counters, nil
}
