// Package notifier 通知发送
package notifier

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
)

// Notifier 通知发送器
type Notifier struct {
	cfg *config.Config
	db  *storage.DB
}

// New 创建通知器
func New(cfg *config.Config, db *storage.DB) *Notifier {
	return &Notifier{cfg: cfg, db: db}
}

// SendTrafficAlert 发送流量报警
func (n *Notifier) SendTrafficAlert(usedGB, limitGB int, percent float64, resetDate string, daysLeft int, threshold int) error {
	if n.cfg.TelegramBotToken == "" || n.cfg.TelegramChatID == "" {
		return nil
	}

	// 检查冷却期（同级别 24 小时内不重复发送）
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	var count int
	if err := n.db.QueryRow("SELECT COUNT(*) FROM alert_records WHERE threshold = ? AND ts > ?", threshold, cutoff).Scan(&count); err != nil {
		log.Printf("查询报警冷却记录失败: %v", err)
	}
	if count > 0 {
		return nil // 冷却期内
	}

	// 构造消息
	msg := fmt.Sprintf(`⚠️ 流量预警 [%s]

📊 当前: %d GB / %d GB (%.1f%%)
📉 剩余: %d GB
📅 重置: %s (%d 天后)

⏰ 检测时间: %s`,
		n.cfg.ServerName,
		usedGB, limitGB, percent,
		limitGB-usedGB,
		resetDate, daysLeft,
		time.Now().In(n.cfg.Timezone).Format("2006-01-02 15:04 MST"),
	)

	if err := n.sendTelegram(msg); err != nil {
		return err
	}

	// 记录报警
	if _, err := n.db.Exec("INSERT INTO alert_records (ts, threshold, message) VALUES (?, ?, ?)",
		time.Now().Unix(), threshold, msg); err != nil {
		log.Printf("记录报警发送历史失败: %v", err)
	}

	return nil
}

// SendDailyReport 推送每日流量摘要（昨日用量 + 环比 + 本计费周期累计与月底预测）。
// 未配置 Telegram 时静默跳过，由调用方决定是否启用定时器。
func (n *Notifier) SendDailyReport() error {
	if n.cfg.TelegramBotToken == "" || n.cfg.TelegramChatID == "" {
		return nil
	}

	tz := n.cfg.Timezone
	now := time.Now().In(tz)
	yTime := now.AddDate(0, 0, -1)
	yesterday := yTime.Format("2006-01-02")
	dayBefore := now.AddDate(0, 0, -2).Format("2006-01-02")

	// 昨日流量（此时昨日的日汇总已由采集器固化）
	yTx, yRx := n.dailyTotal(yesterday)
	// 前日流量：用于计算环比趋势
	bTx, bRx := n.dailyTotal(dayBefore)

	yTotal := yTx + yRx
	bTotal := bTx + bRx

	// 本计费周期累计（含今日已采集部分，与配额预警口径一致）
	billingStart, billingEnd := n.cfg.GetBillingCycleDates(now)
	var cTx, cRx int64
	if err := n.db.QueryRow(
		"SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0) FROM traffic_daily WHERE date >= ? AND iface = 'total'",
		billingStart.Format("2006-01-02"),
	).Scan(&cTx, &cRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询计费周期流量失败: %v", err)
	}

	used := billingUsed(n.cfg.BillingMode, cTx, cRx)
	daysLeft := int(billingEnd.Sub(now).Hours() / 24)

	// 周期已过天数（含今日）与总天数，用于日均和月底预测
	elapsedDays := int(now.Sub(billingStart).Hours()/24) + 1
	if elapsedDays < 1 {
		elapsedDays = 1
	}
	totalDays := int(billingEnd.Sub(billingStart).Hours()/24) + 1
	dailyAvg := used / int64(elapsedDays)
	projected := dailyAvg * int64(totalDays)

	msg := fmt.Sprintf(`📊 每日流量报告 · %s
🗓 %s（%s）
━━━━━━━━━━━━━━

昨日用量
  ↑ %s    ↓ %s
  合计 %s（环比 %s）`,
		n.cfg.ServerName,
		yesterday, weekdayCN(yTime),
		formatBytes(yTx), formatBytes(yRx),
		formatBytes(yTotal), trendText(yTotal, bTotal),
	)

	// 设了限额：给出进度条/百分比/剩余量/月底预测；否则只报累计与均值
	if n.cfg.MonthlyLimitGB > 0 {
		limitBytes := int64(n.cfg.MonthlyLimitGB) * 1024 * 1024 * 1024
		percent := float64(used) / float64(limitBytes) * 100
		remain := limitBytes - used
		if remain < 0 {
			remain = 0
		}
		projWarn := ""
		if projected > limitBytes {
			projWarn = " ⚠️ 或超限"
		}
		msg += fmt.Sprintf(`

本周期用量
  %s %.1f%%
  已用 %s / %s
  剩余 %s
  日均 %s ｜ 预计 %s%s
  重置 %s（%d 天后）`,
			progressBar(percent), percent,
			formatBytes(used), formatBytes(limitBytes),
			formatBytes(remain),
			formatBytes(dailyAvg), formatBytes(projected), projWarn,
			billingEnd.Format("2006-01-02"), daysLeft,
		)
	} else {
		msg += fmt.Sprintf(`

本周期用量
  已用 %s
  日均 %s ｜ 预计 %s
  重置 %s（%d 天后）`,
			formatBytes(used),
			formatBytes(dailyAvg), formatBytes(projected),
			billingEnd.Format("2006-01-02"), daysLeft,
		)
	}

	// 昨日各探测目标的网络延迟（窗口与「昨日用量」一致：昨日 00:00–24:00）
	dayStart := time.Date(yTime.Year(), yTime.Month(), yTime.Day(), 0, 0, 0, 0, tz)
	if sec := n.latencySection(dayStart.Unix(), dayStart.AddDate(0, 0, 1).Unix()); sec != "" {
		msg += sec
	}

	return n.sendTelegram(msg)
}

// latencyStat 昨日单个探测目标的延迟汇总
type latencyStat struct {
	tag    string
	avgRTT float64 // 平均 RTT（ms）
	minRTT float64 // 整段最低 RTT（ms），最接近真实链路
	loss   float64 // 丢包率（%）
	ok     bool    // 是否有有效 RTT 数据点
}

// dailyLatency 汇总 [startTs, endTs) 内各 PingTarget 的延迟。
// 聚合口径与 API 层 handleLatency 保持一致：AVG(rtt_ms)/MIN(min_rtt)/SUM(sent)/SUM(lost)。
// 存库以 IP 作 target 标识，展示用 Tag。
func (n *Notifier) dailyLatency(startTs, endTs int64) []latencyStat {
	stats := make([]latencyStat, 0, len(n.cfg.PingTargets))
	for _, pt := range n.cfg.PingTargets {
		var avg, min sql.NullFloat64
		var sent, lost int64
		if err := n.db.QueryRow(
			`SELECT AVG(rtt_ms), MIN(min_rtt), COALESCE(SUM(sent), 0), COALESCE(SUM(lost), 0)
			 FROM latency_records WHERE target = ? AND ts >= ? AND ts < ?`,
			pt.IP, startTs, endTs,
		).Scan(&avg, &min, &sent, &lost); err != nil && err != sql.ErrNoRows {
			log.Printf("查询 %s 延迟失败: %v", pt.Tag, err)
			continue
		}

		loss := 0.0
		if sent > 0 {
			loss = float64(lost) / float64(sent) * 100
		}
		stats = append(stats, latencyStat{
			tag:    pt.Tag,
			avgRTT: avg.Float64,
			minRTT: min.Float64,
			loss:   loss,
			ok:     avg.Valid, // 全丢包时 AVG(rtt_ms) 为 NULL
		})
	}
	return stats
}

// latencySection 拼装「网络延迟」小节；无目标或全部无数据时返回空串（不加空白小节）。
func (n *Notifier) latencySection(startTs, endTs int64) string {
	stats := n.dailyLatency(startTs, endTs)
	if len(stats) == 0 {
		return ""
	}
	hasData := false
	width := 0 // 目标名对齐宽度（CJK 记 2）
	for _, s := range stats {
		if s.ok {
			hasData = true
		}
		if w := displayWidth(s.tag); w > width {
			width = w
		}
	}
	if !hasData {
		return ""
	}

	sec := "\n\n网络延迟"
	for _, s := range stats {
		name := s.tag + spaces(width-displayWidth(s.tag))
		if !s.ok {
			sec += fmt.Sprintf("\n  %s  无数据", name)
			continue
		}
		lossText := fmt.Sprintf("丢%.0f%%", s.loss)
		if s.loss > 0 {
			lossText += " ⚠️"
		}
		sec += fmt.Sprintf("\n  %s  %.1fms（最低 %.1f）%s", name, s.avgRTT, s.minRTT, lossText)
	}
	return sec
}

// displayWidth 估算字符串显示宽度：CJK 及全角字符记 2，其余记 1，用于纯文本列对齐。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115F || // Hangul Jamo
			(r >= 0x2E80 && r <= 0xA4CF) || // CJK 及部首
			(r >= 0xAC00 && r <= 0xD7A3) || // Hangul 音节
			(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容
			(r >= 0xFF00 && r <= 0xFF60) || // 全角
			(r >= 0xFFE0 && r <= 0xFFE6)) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// spaces 返回 n 个空格（n<=0 时为空串）。
func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

// dailyTotal 读取某天 total 网卡的上/下行字节，无记录时返回 0。
func (n *Notifier) dailyTotal(date string) (tx, rx int64) {
	if err := n.db.QueryRow(
		"SELECT COALESCE(tx_bytes, 0), COALESCE(rx_bytes, 0) FROM traffic_daily WHERE date = ? AND iface = 'total'",
		date,
	).Scan(&tx, &rx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询 %s 流量失败: %v", date, err)
	}
	return tx, rx
}

// weekdayCN 返回中文星期几
func weekdayCN(t time.Time) string {
	return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[t.Weekday()]
}

// trendText 计算今日相对前一日的环比变化文案；前一日无数据时返回占位符。
func trendText(today, prev int64) string {
	if prev <= 0 {
		return "无对比"
	}
	delta := float64(today-prev) / float64(prev) * 100
	switch {
	case delta > 0:
		return fmt.Sprintf("↑%.1f%%", delta)
	case delta < 0:
		return fmt.Sprintf("↓%.1f%%", -delta)
	default:
		return "持平"
	}
}

// progressBar 用 10 格方块渲染百分比进度条（0-100，超出按满格显示）。
func progressBar(percent float64) string {
	const width = 10
	filled := int(percent/10 + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	bar := make([]rune, 0, width)
	for i := 0; i < width; i++ {
		if i < filled {
			bar = append(bar, '█')
		} else {
			bar = append(bar, '░')
		}
	}
	return string(bar)
}

// billingUsed 按计费模式从上/下行字节计算「已用流量」
func billingUsed(mode string, tx, rx int64) int64 {
	switch mode {
	case "tx_only":
		return tx
	case "rx_only":
		return rx
	case "max_value":
		if tx > rx {
			return tx
		}
		return rx
	default: // bidirectional
		return tx + rx
	}
}

// formatBytes 将字节数格式化为带单位的可读字符串
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// SendTest 发送一条测试消息，用于在页面上验证 Telegram 是否配置成功。
// 未配置时返回错误，让调用方把原因回显到界面。
func (n *Notifier) SendTest() error {
	if n.cfg.TelegramBotToken == "" || n.cfg.TelegramChatID == "" {
		return fmt.Errorf("未配置 Telegram（TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID）")
	}

	msg := fmt.Sprintf(`✅ 测试消息 [%s]

Heliox Monitor 的 Telegram 通知已配置成功。
⏰ %s`,
		n.cfg.ServerName,
		time.Now().In(n.cfg.Timezone).Format("2006-01-02 15:04:05 MST"),
	)

	return n.sendTelegram(msg)
}

// sendTelegram 发送 Telegram 消息
func (n *Notifier) sendTelegram(text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramBotToken)

	payload := map[string]string{
		"chat_id": n.cfg.TelegramChatID,
		"text":    text,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram API 返回 %d", resp.StatusCode)
	}

	return nil
}
