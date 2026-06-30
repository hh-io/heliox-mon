package collector

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 预编译正则（避免每次采集重复编译）
var (
	rePingStats = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received`)
	// 捕获 min/avg/mdev（max 用 [\d.]+ 跳过不入库）；mdev/stddev 必有
	rePingRTT = regexp.MustCompile(`rtt min/avg/max/(?:mdev|stddev) = ([\d.]+)/([\d.]+)/[\d.]+/([\d.]+)`)
	// 兼容 BSD/旧版输出 "round-trip min/avg/max[/stddev] = ..."，stddev 可选
	rePingRTTAlt = regexp.MustCompile(`(?:rtt|round-trip) [^=]*= ([\d.]+)/([\d.]+)/[\d.]+(?:/([\d.]+))?`)
)

// pingResult 单次 ping 的解析结果。
// RTT 类指标用指针：nil 表示该次无有效 RTT（如全丢包），与 0ms 区分。
type pingResult struct {
	avgRtt *float64
	minRtt *float64 // 最小 RTT，最接近真实链路传播延迟
	mdev   *float64 // 抖动（mdev/stddev）
	sent   int
	lost   int
}

// doCollectLatency 执行延迟采集
func (c *Collector) doCollectLatency() {
	now := time.Now().Unix()

	for _, target := range c.cfg.PingTargets {
		res, ok := c.pingStats(target.IP)
		if !ok {
			// 环境/执行错误（ping 不可用、命令超时等）跳过本次记录，
			// 留数据空洞而非伪造成“目标丢包”，避免污染丢包率统计
			continue
		}

		// 使用 IP 作为 target 标识（解耦显示名称）
		_, dbErr := c.db.Exec(
			`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
			now, target.IP, res.avgRtt, res.minRtt, res.mdev, res.sent, res.lost,
		)
		if dbErr != nil {
			log.Printf("保存延迟记录失败: %v", dbErr)
		}
	}
}

// pingStats 使用系统 ping 命令进行延迟测试。
// 返回解析结果与是否成功；ok=false 表示本次因环境/执行错误失败，调用方应跳过记录。
func (c *Collector) pingStats(target string) (pingResult, bool) {
	count := c.cfg.PingCount
	if count <= 0 {
		count = 5
	}
	timeout := c.cfg.PingTimeout
	if timeout <= 0 {
		timeout = time.Second
	}

	// -W 直接用秒（含小数），不再 int() 截断把毫秒级配置静默归零；
	// 新版 iputils 支持小数 -W，旧版会自行向下取整
	timeoutArg := strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)

	// 给整条命令留预算：每包最坏 ≈ 发包间隔(1s) + 单包超时，再加缓冲，
	// 用 context 兜底，防止 ping 卡死拖垮采集 goroutine
	budget := time.Duration(count)*(time.Second+timeout) + 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// -c count: 发送次数  -W timeout: 单次超时(秒)  -q: 静默，只输出统计
	cmd := exec.CommandContext(ctx, "ping", "-c", strconv.Itoa(count), "-W", timeoutArg, "-q", target)
	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("警告：ping %s 超时被强制终止，跳过本次记录", target)
		return pingResult{}, false
	}

	// 无论退出码如何都先尝试解析：100% 丢包时 ping 退出码为 1，
	// 但仍会打印完整统计行，必须据此解析而非一律当作错误
	if res, parsed := parsePingOutput(string(output), count); parsed {
		return res, true
	}

	// 解析失败，区分“目标全丢包”与“执行环境错误”
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// 退出码 1 = 有发包但无任何回应，视为全丢包
		return pingResult{sent: count, lost: count}, true
	}

	// 退出码 2 / ping 不存在 / 参数错误等环境问题：不记录，避免伪造丢包
	log.Printf("警告：ping %s 执行失败，跳过本次记录: %v，输出: %s",
		target, err, strings.TrimSpace(string(output)))
	return pingResult{}, false
}

// parsePingOutput 解析系统 ping 命令输出。
// 返回解析结果与是否解析成功（拿到 transmitted/received 统计即视为成功）。
// 示例输出：
//
//	--- 8.8.8.8 ping statistics ---
//	5 packets transmitted, 5 received, 0% packet loss, time 4005ms
//	rtt min/avg/max/mdev = 10.123/15.456/20.789/3.214 ms
func parsePingOutput(output string, expectedCount int) (pingResult, bool) {
	match := rePingStats.FindStringSubmatch(output)
	if match == nil {
		// 无统计行，无法判断发送/接收，交由上层决定如何处理
		return pingResult{}, false
	}
	transmitted, _ := strconv.Atoi(match[1])
	received, _ := strconv.Atoi(match[2])

	lost := transmitted - received
	if lost < 0 {
		lost = 0
	}
	res := pingResult{sent: transmitted, lost: lost}

	// 全部丢包：无 RTT 数据，但统计本身有效
	if received == 0 {
		return res, true
	}

	// 解析 RTT：min/avg/mdev
	m := rePingRTT.FindStringSubmatch(output)
	if m == nil {
		m = rePingRTTAlt.FindStringSubmatch(output)
	}
	if m == nil {
		// 有回应却拿不到 RTT 行：发送/接收统计仍有效，RTT 留空
		log.Printf("警告：无法解析 ping 输出的 RTT：%s", strings.TrimSpace(output))
		return res, true
	}

	if v, err := strconv.ParseFloat(m[1], 64); err == nil {
		res.minRtt = &v
	}
	if v, err := strconv.ParseFloat(m[2], 64); err == nil {
		res.avgRtt = &v
	}
	// mdev/stddev 为可选捕获组（旧版 round-trip 可能缺失）
	if len(m) > 3 && m[3] != "" {
		if v, err := strconv.ParseFloat(m[3], 64); err == nil {
			res.mdev = &v
		}
	}

	return res, true
}
