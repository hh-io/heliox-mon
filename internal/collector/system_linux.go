package collector

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// connsCollectInterval 连接数重新扫描的间隔。
// 「此刻有多少人在用」不需要 5 秒粒度，而 /proc/net/tcp 是 seq_file 全表遍历、
// 遍历期间内核还要持 hash 桶锁；连接数上万时每 5 秒扫一次，等于让监控自己去
// 加重被监控机器的负载
const connsCollectInterval = 15 * time.Second

// doCollectSystemMetrics 执行系统资源采集
func (c *Collector) doCollectSystemMetrics() {
	now := time.Now()

	cpuPercent, stealPercent, cpuOK := c.getCPUPercent()
	memUsed, memTotal := c.getMemoryInfo()
	diskUsed, diskAvail, diskTotal := c.getDiskInfo()
	load1, load5, load15 := c.getLoadAvg()
	retrans, retransOK := c.getRetransPercent()

	// 无基准的那一轮 stealPercent 只是占位的 0，不该混进均值把窗口拉低
	var stealAvg float64
	if cpuOK {
		stealAvg = c.pushSteal(stealPercent)
	}

	c.setSystemSnapshot(SystemSnapshot{
		Ts:              now.Unix(),
		CPUPercent:      cpuPercent,
		CPUValid:        cpuOK,
		StealPercent:    stealPercent,
		StealAvgPercent: stealAvg,
		CPUCores:        runtime.NumCPU(),
		MemUsed:         memUsed,
		MemTotal:        memTotal,
		DiskUsed:        diskUsed,
		DiskAvail:       diskAvail,
		DiskTotal:       diskTotal,
		Load1:           load1,
		Load5:           load5,
		Load15:          load15,
		PortConns:       c.collectPortConns(now),
		RetransPercent:  retrans,
		RetransValid:    retransOK,
		UptimeSec:       getUptime(),
	})
}

// collectPortConns 按 connsCollectInterval 降频扫描连接数，间隔内复用上轮结果
func (c *Collector) collectPortConns(now time.Time) map[int]int {
	if !c.lastConnsAt.IsZero() && now.Sub(c.lastConnsAt) < connsCollectInterval {
		return c.lastConns
	}

	c.lastConns = countEstablished(c.cfg.ActivePorts())
	c.lastConnsAt = now
	return c.lastConns
}

// getCPUPercent 通过两次采样的差值计算 CPU 使用率与 steal 占比。
// 首次采样只记录基准，无法给出使用率，此时 ok=false（由调用方决定如何呈现，
// 而不是伪造成 0%）。
func (c *Collector) getCPUPercent() (usage, steal float64, ok bool) {
	cur, valid := readCPUStat()
	if !valid {
		return 0, 0, false
	}

	prev := c.lastCPU
	hadBaseline := c.hasCPUBaseline
	c.lastCPU, c.hasCPUBaseline = cur, true
	if !hadBaseline {
		return 0, 0, false
	}

	// 累计计数器正常只增不减；CPU 热插拔等极端情况下可能回退，
	// 此时用 uint64 相减会溢出成天文数字，直接跳过这一轮并以当前值为新基准
	if cur.total <= prev.total || cur.idle < prev.idle || cur.steal < prev.steal {
		return 0, 0, false
	}

	deltaTotal := float64(cur.total - prev.total)
	deltaIdle := float64(cur.idle - prev.idle)
	deltaSteal := float64(cur.steal - prev.steal)

	return clampPercent(100 * (deltaTotal - deltaIdle) / deltaTotal),
		clampPercent(100 * deltaSteal / deltaTotal),
		true
}

// readCPUStat 读取 /proc/stat 首行的 CPU 累计时间
func readCPUStat() (cpuTimes, bool) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return cpuTimes{}, false
	}

	return parseCPUStatLine(scanner.Text())
}

// parseCPUStatLine 解析 /proc/stat 的 cpu 汇总行：
//
//	cpu user nice system idle iowait irq softirq steal guest guest_nice
//
// 解析失败返回 ok=false —— 若把无法解析的字段当 0 累加，total 会偏小，
// 算出的使用率是错的，不如直接丢弃这一轮采样。
func parseCPUStatLine(line string) (cpuTimes, bool) {
	fields := strings.Fields(line)
	// 至少要有 idle 与 iowait 才能算出使用率
	if len(fields) < 6 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}

	var t cpuTimes
	// 只累加到 steal（下标 8）为止：guest/guest_nice 是 user/nice 的子集，
	// 内核已把它们计入前两项，再加一遍会让 total 偏大、使用率偏低
	for i := 1; i < len(fields) && i <= 8; i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		t.total += v
		switch i {
		case 4, 5: // idle, iowait
			t.idle += v
		case 8: // steal
			t.steal = v
		}
	}

	return t, true
}

// clampPercent 把因浮点误差或统计抖动而轻微越界的百分比夹回 [0, 100]
func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// getUptime 获取系统运行时长（秒）
func getUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}

	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(secs)
}

// getMemoryInfo 获取内存信息
func (c *Collector) getMemoryInfo() (used, total uint64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	var memTotal, memAvailable, memFree, buffers, cached uint64
	hasMemAvailable := false

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		value *= 1024 // kB to bytes

		switch fields[0] {
		case "MemTotal:":
			memTotal = value
		case "MemAvailable:":
			memAvailable = value
			hasMemAvailable = true
		case "MemFree:":
			memFree = value
		case "Buffers:":
			buffers = value
		case "Cached:":
			cached = value
		}
	}

	// 优先使用 MemAvailable（更准确），否则回退到传统计算
	if hasMemAvailable {
		used = memTotal - memAvailable
	} else {
		used = memTotal - memFree - buffers - cached
	}
	return used, memTotal
}

// getDiskInfo 获取磁盘使用情况（根目录）。
// used 用 Bfree 计算，与 df 的 Used 口径一致；avail 用 Bavail，即普通用户真正
// 能写入的容量——两者差着 ext4 默认预留给 root 的 5%，只报 total-used 会让
// 剩余空间显得比实际多。
func (c *Collector) getDiskInfo() (used, avail, total uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0, 0
	}

	bsize := uint64(stat.Bsize)
	total = stat.Blocks * bsize
	used = (stat.Blocks - stat.Bfree) * bsize
	avail = stat.Bavail * bsize

	return used, avail, total
}

// getLoadAvg 获取系统负载
func (c *Collector) getLoadAvg() (load1, load5, load15 float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0
	}

	load1, _ = strconv.ParseFloat(fields[0], 64)
	load5, _ = strconv.ParseFloat(fields[1], 64)
	load15, _ = strconv.ParseFloat(fields[2], 64)

	return load1, load5, load15
}
