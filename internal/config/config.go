// Package config 配置管理
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultTurnstileSiteKey 站点公钥原先硬编码在 login.html 里；提出来只是为了让登录页能按需渲染，
// 默认值保持不变，已部署的实例不必新增环境变量。site key 本就是公开信息，密钥是 secret。
const defaultTurnstileSiteKey = "0x4AAAAAACOhRkua9joDOJiB"

// PingTarget 延迟监控目标
// MaxPingCount 单次探测的最大发包数。上限存在的理由不是 ping 本身，而是
// collector 的 maxRTTsPerPoint：一个时间桶装不下整轮样本时，API 就不再下发
// 逐包 rtts，前端的真样本 P95 会静默退化。两者必须是同一个数。
const MaxPingCount = 40

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

	// 端口监控。端口号即便协议已停用也保留：历史数据按端口号查询，
	// 停用只影响「是否继续采集/展示当前端口」，见 ActivePorts
	SnellPort    int
	VlessPort    int
	SnellEnabled bool
	XrayEnabled  bool

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
	TurnstileSiteKey   string
	// DevNoAuth 本地开发免登录：只有 darwin 构建 + 显式开关才为 true，且服务端仍只对
	// 回环地址放行（见 api.auth）。生产 Linux 二进制里恒为 false。
	DevNoAuth bool

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
		SnellEnabled:       true,
		XrayEnabled:        true,
		TelegramBotToken:   getEnv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:     getEnv("TELEGRAM_CHAT_ID", ""),
		DailyReportEnabled: getEnvBool("DAILY_REPORT_ENABLED", false),
		DailyReportHour:    getEnvInt("DAILY_REPORT_HOUR", 9),
		MonthlyLimitGB:     getEnvInt("MONTHLY_LIMIT_GB", 1000),
		BillingMode:        getEnv("BILLING_MODE", "bidirectional"),
		ResetDay:           getEnvInt("RESET_DAY", 1),
		ServerName:         getEnv("SERVER_NAME", "Heliox"),
		PingCount:          getEnvInt("PING_COUNT", 20),
		PingTimeout:        time.Duration(getEnvInt("PING_TIMEOUT_MS", 1000)) * time.Millisecond,
		PingGap:            time.Duration(getEnvInt("PING_GAP_MS", 200)) * time.Millisecond,
		TurnstileSecretKey: getEnv("HELIOX_TURNSTILE_SECRET", ""),
		TurnstileSiteKey:   getEnv("HELIOX_TURNSTILE_SITEKEY", defaultTurnstileSiteKey),
		DevNoAuth:          devAuthAllowed && getEnvBool("HELIOX_MON_DEV_NO_AUTH", false),
		ReportToken:        getEnv("HELIOX_MON_REPORT_TOKEN", ""),
	}

	// PING_COUNT 夹在 [1, MaxPingCount]：一次探测的逐包样本必须能整包放进一个时间桶，
	// 超了 API 就不再下发 rtts，前端会永久退回「P95 约」且界面上没有任何提示
	if cfg.PingCount < 1 {
		log.Printf("警告：PING_COUNT=%d 无效，按 1 处理", cfg.PingCount)
		cfg.PingCount = 1
	}
	if cfg.PingCount > MaxPingCount {
		log.Printf("警告：PING_COUNT=%d 超过上限，按 %d 处理", cfg.PingCount, MaxPingCount)
		cfg.PingCount = MaxPingCount
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

	// 钳制计费重置日到 1-28：29-31 在短月不存在，会被 time.Date 规范化到下个月
	if day := clampResetDay(cfg.ResetDay); day != cfg.ResetDay {
		log.Printf("RESET_DAY=%d 超出有效范围 1-28，已调整为 %d", cfg.ResetDay, day)
		cfg.ResetDay = day
	}

	// 验证必填项
	if cfg.Password == "" {
		return nil, fmt.Errorf("HELIOX_MON_PASS 未设置")
	}

	if cfg.DevNoAuth {
		log.Printf("⚠️  HELIOX_MON_DEV_NO_AUTH 已开启：来自回环地址的请求免登录，仅供本机调试")
	}

	return cfg, nil
}

// loadHelioxEnv 从 heliox/.env 读取端口配置及协议启用状态
func (c *Config) loadHelioxEnv() error {
	data, err := os.ReadFile(c.HelioxEnvPath)
	if err != nil {
		return err
	}

	snellEnabled, xrayEnabled := true, true
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
		// 判定口径与 heliox deploy.sh 一致：变量存在时只有字面量 true 算启用
		case "ENABLE_SNELL":
			snellEnabled = value == "true"
		case "ENABLE_XRAY":
			xrayEnabled = value == "true"
		}
	}

	if c.SnellPort == 0 {
		c.SnellPort = 36890
	}
	if c.VlessPort == 0 {
		c.VlessPort = 443
	}
	// heliox 的 uninstall-snell / uninstall-xray 会置 ENABLE_*=false 但保留端口变量
	c.SnellEnabled = snellEnabled
	c.XrayEnabled = xrayEnabled

	return nil
}

// ActivePorts 返回当前需要采集与展示的端口：已配置且协议未停用。
// 停用协议不再统计，否则规则脚本跳过了它、规则检测却仍要求它，页面会一直报规则缺失
func (c *Config) ActivePorts() []int {
	ports := make([]int, 0, 2)
	if c.SnellEnabled && c.SnellPort > 0 {
		ports = append(ports, c.SnellPort)
	}
	if c.XrayEnabled && c.VlessPort > 0 {
		ports = append(ports, c.VlessPort)
	}
	return ports
}

// DataPath 返回数据目录下的文件路径
func (c *Config) DataPath(name string) string {
	return filepath.Join(c.DataDir, name)
}

// GetBillingCycleDates 根据 ResetDay 计算计费周期起止日期
func (c *Config) GetBillingCycleDates(now time.Time) (start, end time.Time) {
	tz := c.Timezone
	if tz == nil {
		tz = time.UTC
	}
	// 统一换算到配置时区再取年月日，避免调用方传入其他时区的时间导致日历日错位
	now = now.In(tz)

	// 再钳一次重置日：Load 已钳过，但 Config 也可能被直接构造。
	// 超出 1-28 会让 time.Date 触发月份规范化（如 2 月 31 日变成 3 月 3 日），
	// 计费周期起点会跳到未来
	day := clampResetDay(c.ResetDay)

	if now.Day() >= day {
		start = time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, tz)
	} else {
		start = time.Date(now.Year(), now.Month()-1, day, 0, 0, 0, 0, tz)
	}
	end = start.AddDate(0, 1, 0).Add(-time.Second)
	return
}

// clampResetDay 将计费重置日钳制到 1-28，保证任意月份都存在该日期
func clampResetDay(day int) int {
	if day < 1 {
		return 1
	}
	if day > 28 {
		return 28
	}
	return day
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
