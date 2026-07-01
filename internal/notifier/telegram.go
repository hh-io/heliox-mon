// Package notifier 通知发送
package notifier

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

// SendDailyReport 推送每日流量摘要（昨日用量 + 本计费周期累计）。
// 未配置 Telegram 时静默跳过，由调用方决定是否启用定时器。
func (n *Notifier) SendDailyReport() error {
	if n.cfg.TelegramBotToken == "" || n.cfg.TelegramChatID == "" {
		return nil
	}

	tz := n.cfg.Timezone
	now := time.Now().In(tz)
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 昨日流量（此时昨日的日汇总已由采集器固化）
	var yTx, yRx int64
	if err := n.db.QueryRow(
		"SELECT COALESCE(tx_bytes, 0), COALESCE(rx_bytes, 0) FROM traffic_daily WHERE date = ? AND iface = 'total'",
		yesterday,
	).Scan(&yTx, &yRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询昨日流量失败: %v", err)
	}

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

	msg := fmt.Sprintf(`📊 每日流量报告 [%s]
🗓 %s

昨日: ↑%s ↓%s（共 %s）
本周期已用: %s`,
		n.cfg.ServerName,
		yesterday,
		formatBytes(yTx), formatBytes(yRx), formatBytes(yTx+yRx),
		formatBytes(used),
	)

	// 设了限额才给出百分比/剩余量，否则只报累计值
	if n.cfg.MonthlyLimitGB > 0 {
		limitBytes := int64(n.cfg.MonthlyLimitGB) * 1024 * 1024 * 1024
		percent := float64(used) / float64(limitBytes) * 100
		remain := limitBytes - used
		if remain < 0 {
			remain = 0
		}
		msg += fmt.Sprintf(` / %d GB（%.1f%%）
剩余: %s`,
			n.cfg.MonthlyLimitGB, percent, formatBytes(remain))
	}

	msg += fmt.Sprintf(`
重置: %s（%d 天后）`,
		billingEnd.Format("2006-01-02"), daysLeft)

	return n.sendTelegram(msg)
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
