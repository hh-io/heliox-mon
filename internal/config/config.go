// Package config 配置管理
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PingTarget 延迟监控目标
type PingTarget struct {
	Tag string // 显示名称
	IP  string // IP 地址
}

// ParsePingTarget 解析 ping 目标 (格式: TAG:IP 或 IP)
func ParsePingTarget(s string) PingTarget {
	if idx := strings.Index(s, ":"); idx > 0 {
		return PingTarget{Tag: s[:idx], IP: s[idx+1:]}
	}
	return PingTarget{Tag: s, IP: s}
}

// Config 应用配置
type Config struct {
	// 数据目录
	DataDir string

	// HTTP 服务
	ListenAddr string
	Username   string
	Password   string

	// Heliox 配置路径
	HelioxEnvPath string

	// 端口监控
	SnellPort int
	VlessPort int

	// 时区
	Timezone *time.Location

	// Telegram
	TelegramBotToken string
	TelegramChatID   string

	// 每日流量报告（Telegram 定时推送）
	DailyReportEnabled bool
	DailyReportHour    int // 推送时刻（按 Timezone 的整点，0-23）

	// 流量报警
	MonthlyLimitGB  int
	BillingMode     string // bidirectional, tx_only, rx_only, max_value
	ResetDay        int    // 计费周期重置日 (1-28)
	AlertThresholds []int  // 报警阈值百分比，如 [80, 90, 95]

	// 延迟监控目标
	PingTargets []PingTarget
	PingCount   int
	PingTimeout time.Duration
	PingGap     time.Duration

	// 服务器标识
	ServerName string

	// 安全
	TurnstileSecretKey string

	// 客户端延迟上报令牌（空则关闭上报功能）
	ReportToken string
}

// Load 加载配置
func Load() (*Config, error) {
	cfg := &Config{
		DataDir:            getEnv("HELIOX_MON_DATA_DIR", "/var/lib/heliox-mon"),
		ListenAddr:         getEnv("HELIOX_MON_LISTEN", "127.0.0.1:9100"),
		Username:           getEnv("HELIOX_MON_USER", "admin"),
		Password:           getEnv("HELIOX_MON_PASS", ""),
		HelioxEnvPath:      getEnv("HELIOX_ENV_PATH", "../heliox/.env"),
		TelegramBotToken:   getEnv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:     getEnv("TELEGRAM_CHAT_ID", ""),
		DailyReportEnabled: getEnvBool("DAILY_REPORT_ENABLED", false),
		DailyReportHour:    getEnvInt("DAILY_REPORT_HOUR", 9),
		MonthlyLimitGB:     getEnvInt("MONTHLY_LIMIT_GB", 1000),
		BillingMode:        getEnv("BILLING_MODE", "bidirectional"),
		ResetDay:           getEnvInt("RESET_DAY", 1),
		ServerName:         getEnv("SERVER_NAME", "Heliox"),
		PingCount:          getEnvInt("PING_COUNT", 5),
		PingTimeout:        time.Duration(getEnvInt("PING_TIMEOUT_MS", 1000)) * time.Millisecond,
		PingGap:            time.Duration(getEnvInt("PING_GAP_MS", 200)) * time.Millisecond,
		TurnstileSecretKey: getEnv("HELIOX_TURNSTILE_SECRET", ""),
		ReportToken:        getEnv("HELIOX_MON_REPORT_TOKEN", ""),
	}

	// 解析报警阈值
	thresholds := getEnv("ALERT_THRESHOLDS", "80,90,95")
	for _, t := range strings.Split(thresholds, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			cfg.AlertThresholds = append(cfg.AlertThresholds, v)
		}
	}

	// 解析 Ping 目标 (格式: TAG:IP 或 IP)
	targets := getEnv("PING_TARGETS", "Google:8.8.8.8,Cloudflare:1.1.1.1")
	for _, t := range strings.Split(targets, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		pt := ParsePingTarget(t)
		cfg.PingTargets = append(cfg.PingTargets, pt)
	}

	// 设置时区
	tzName := getEnv("HELIOX_MON_TZ", "Asia/Singapore")
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		// 无法加载指定时区，使用固定偏移
		tz = time.FixedZone("+08", 8*3600)
	}
	if tz == nil {
		// 极端情况兜底：使用 UTC
		tz = time.UTC
	}
	cfg.Timezone = tz

	// 读取 heliox .env 获取端口
	if err := cfg.loadHelioxEnv(); err != nil {
		// 非致命错误，使用默认值
		cfg.SnellPort = 36890
		cfg.VlessPort = 443
	}

	// 钳制每日报告时刻到合法整点，非法值回退到默认 9 点
	if cfg.DailyReportHour < 0 || cfg.DailyReportHour > 23 {
		cfg.DailyReportHour = 9
	}

	// 验证必填项
	if cfg.Password == "" {
		return nil, fmt.Errorf("HELIOX_MON_PASS 未设置")
	}

	return cfg, nil
}

// loadHelioxEnv 从 heliox/.env 读取端口配置
func (c *Config) loadHelioxEnv() error {
	data, err := os.ReadFile(c.HelioxEnvPath)
	if err != nil {
		return err
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "SNELL_PORT":
			if v, err := strconv.Atoi(value); err == nil {
				c.SnellPort = v
			}
		case "VLESS_PORT":
			if v, err := strconv.Atoi(value); err == nil {
				c.VlessPort = v
			}
		}
	}

	if c.SnellPort == 0 {
		c.SnellPort = 36890
	}
	if c.VlessPort == 0 {
		c.VlessPort = 443
	}

	return nil
}

// DataPath 返回数据目录下的文件路径
func (c *Config) DataPath(name string) string {
	return filepath.Join(c.DataDir, name)
}

// GetBillingCycleDates 根据 ResetDay 计算计费周期起止日期
func (c *Config) GetBillingCycleDates(now time.Time) (start, end time.Time) {
	day := c.ResetDay
	tz := c.Timezone

	if now.Day() >= day {
		start = time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, tz)
	} else {
		start = time.Date(now.Year(), now.Month()-1, day, 0, 0, 0, 0, tz)
	}
	end = start.AddDate(0, 1, 0).Add(-time.Second)
	return
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return defaultVal
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}
