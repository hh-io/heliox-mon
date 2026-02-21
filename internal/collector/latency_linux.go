package collector

import (
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 预编译正则（避免每次采集重复编译）
var (
	rePingStats  = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received`)
	rePingRTT    = regexp.MustCompile(`rtt min/avg/max/(?:mdev|stddev) = [\d.]+/([\d.]+)/`)
	rePingRTTAlt = regexp.MustCompile(`(?:rtt|round-trip) .*?= [\d.]+/([\d.]+)/`)
)

// doCollectLatency 执行延迟采集
func (c *Collector) doCollectLatency() {
	now := time.Now().Unix()

	for _, target := range c.cfg.PingTargets {
		rttMs, sent, lost := c.pingStats(target.IP)

		// 使用 IP 作为 target 标识（解耦显示名称）
		_, dbErr := c.db.Exec(
			"INSERT INTO latency_records (ts, target, rtt_ms, sent, lost, is_aggregated) VALUES (?, ?, ?, ?, ?, 0)",
			now, target.IP, rttMs, sent, lost,
		)
		if dbErr != nil {
			log.Printf("保存延迟记录失败: %v", dbErr)
		}
	}
}

// pingStats 使用系统 ping 命令进行延迟测试，返回平均 RTT（ms）、发送次数、丢失次数
func (c *Collector) pingStats(target string) (*float64, int, int) {
	count := c.cfg.PingCount
	if count <= 0 {
		count = 5
	}
	timeout := c.cfg.PingTimeout
	if timeout <= 0 {
		timeout = 1 * time.Second
	}

	// 使用系统 ping 命令
	// -c count: 发送次数
	// -W timeout: 单次超时（秒）
	// -q: 静默模式，只输出统计
	timeoutSec := int(timeout.Seconds())
	if timeoutSec < 1 {
		timeoutSec = 1
	}

	cmd := exec.Command("ping", "-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSec), "-q", target)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// ping 命令失败（如目标不可达），返回全丢包
		return nil, count, count
	}

	// 解析 ping 输出
	return parsePingOutput(string(output), count)
}

// parsePingOutput 解析系统 ping 命令输出
// 示例输出：
// --- 8.8.8.8 ping statistics ---
// 5 packets transmitted, 5 received, 0% packet loss, time 4005ms
// rtt min/avg/max/mdev = 10.123/15.456/20.789/3.214 ms
func parsePingOutput(output string, expectedCount int) (*float64, int, int) {
	var transmitted, received int
	var avgRtt float64

	// 解析发送/接收统计
	// 格式: "5 packets transmitted, 5 received, 0% packet loss"
	statsRe := rePingStats
	if match := statsRe.FindStringSubmatch(output); match != nil {
		transmitted, _ = strconv.Atoi(match[1])
		received, _ = strconv.Atoi(match[2])
	} else {
		// 解析失败，返回全丢包
		return nil, expectedCount, expectedCount
	}

	lost := transmitted - received
	if lost < 0 {
		lost = 0
	}

	// 如果全部丢包，不解析 RTT
	if received == 0 {
		return nil, transmitted, lost
	}

	// 解析 RTT 统计
	// 格式: "rtt min/avg/max/mdev = 10.123/15.456/20.789/3.214 ms"
	rttRe := rePingRTT
	if match := rttRe.FindStringSubmatch(output); match != nil {
		avgRtt, _ = strconv.ParseFloat(match[1], 64)
	} else {
		// 兼容旧版本 ping 输出（可能没有 mdev）
		// 尝试匹配 "round-trip min/avg/max = ..."
		altRttRe := rePingRTTAlt
		if match := altRttRe.FindStringSubmatch(output); match != nil {
			avgRtt, _ = strconv.ParseFloat(match[1], 64)
		} else {
			// 无法解析 RTT，但有接收包，返回 nil 表示数据不可信
			log.Printf("警告：无法解析 ping 输出的 RTT：%s", strings.TrimSpace(output))
			return nil, transmitted, lost
		}
	}

	return &avgRtt, transmitted, lost
}
