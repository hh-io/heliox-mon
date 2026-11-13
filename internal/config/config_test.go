package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePingTarget(t *testing.T) {
	cases := []struct {
		in      string
		wantTag string
		wantIP  string
	}{
		{"Google:8.8.8.8", "Google", "8.8.8.8"},
		{"Cloudflare:1.1.1.1", "Cloudflare", "1.1.1.1"},
		{"8.8.8.8", "8.8.8.8", "8.8.8.8"}, // 无 TAG 时 Tag 回退为 IP 本身
	}
	for _, c := range cases {
		got := ParsePingTarget(c.in)
		if got.Tag != c.wantTag || got.IP != c.wantIP {
			t.Errorf("ParsePingTarget(%q) = {%q, %q}, want {%q, %q}",
				c.in, got.Tag, got.IP, c.wantTag, c.wantIP)
		}
	}
}

func TestGetBillingCycleDates(t *testing.T) {
	tz := time.UTC
	mk := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 12, 0, 0, 0, tz)
	}

	cases := []struct {
		name      string
		resetDay  int
		now       time.Time
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			name:      "重置日为1号-自然月",
			resetDay:  1,
			now:       mk(2026, time.March, 15),
			wantStart: time.Date(2026, time.March, 1, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.April, 1, 0, 0, 0, 0, tz).Add(-time.Second),
		},
		{
			name:      "当前日大于等于重置日-本月起",
			resetDay:  15,
			now:       mk(2026, time.March, 20),
			wantStart: time.Date(2026, time.March, 15, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.April, 15, 0, 0, 0, 0, tz).Add(-time.Second),
		},
		{
			name:      "当前日小于重置日-上月起",
			resetDay:  15,
			now:       mk(2026, time.March, 10),
			wantStart: time.Date(2026, time.February, 15, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.March, 15, 0, 0, 0, 0, tz).Add(-time.Second),
		},
		{
			name:      "跨年回退-1月小于重置日",
			resetDay:  15,
			now:       mk(2026, time.January, 10),
			wantStart: time.Date(2025, time.December, 15, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.January, 15, 0, 0, 0, 0, tz).Add(-time.Second),
		},
		{
			// 重置日超出 1-28 会被钳制，否则 time.Date 会把 2 月 31 日规范化到 3 月
			name:      "重置日超出上限-钳制到28",
			resetDay:  31,
			now:       mk(2026, time.March, 10),
			wantStart: time.Date(2026, time.February, 28, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.March, 28, 0, 0, 0, 0, tz).Add(-time.Second),
		},
		{
			name:      "重置日为0-钳制到1",
			resetDay:  0,
			now:       mk(2026, time.March, 10),
			wantStart: time.Date(2026, time.March, 1, 0, 0, 0, 0, tz),
			wantEnd:   time.Date(2026, time.April, 1, 0, 0, 0, 0, tz).Add(-time.Second),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{ResetDay: c.resetDay, Timezone: tz}
			start, end := cfg.GetBillingCycleDates(c.now)
			if !start.Equal(c.wantStart) {
				t.Errorf("start = %v, want %v", start, c.wantStart)
			}
			if !end.Equal(c.wantEnd) {
				t.Errorf("end = %v, want %v", end, c.wantEnd)
			}
		})
	}
}

// 调用方传入非配置时区的时间时，日历日应按配置时区换算，而非直接取原时区的年月日
func TestGetBillingCycleDates_NormalizesTimezone(t *testing.T) {
	tz := time.FixedZone("+08", 8*3600)
	cfg := &Config{ResetDay: 15, Timezone: tz}

	// UTC 的 3 月 14 日 20:00 = +08 的 3 月 15 日 04:00，应落在「本月起」的周期
	now := time.Date(2026, time.March, 14, 20, 0, 0, 0, time.UTC)
	start, _ := cfg.GetBillingCycleDates(now)

	want := time.Date(2026, time.March, 15, 0, 0, 0, 0, tz)
	if !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
}

func TestLoad_ClampsResetDay(t *testing.T) {
	t.Setenv("HELIOX_MON_PASS", "secret")
	t.Setenv("HELIOX_ENV_PATH", "/nonexistent/.env")
	t.Setenv("RESET_DAY", "31")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.ResetDay != 28 {
		t.Errorf("ResetDay = %d, want 28", cfg.ResetDay)
	}
}

// PING_COUNT 超过 MaxPingCount 时，一轮探测的样本装不进一个时间桶，API 就不再
// 下发逐包 rtts，前端会永久退回「P95 约」且界面上没有任何提示——所以必须夹住。
func TestLoad_ClampsPingCount(t *testing.T) {
	t.Setenv("HELIOX_MON_PASS", "secret")
	t.Setenv("HELIOX_ENV_PATH", "/nonexistent/.env")

	for _, tt := range []struct {
		in   string
		want int
	}{
		{"100", MaxPingCount},
		{"0", 1},
		{"-5", 1},
		{"20", 20},
	} {
		t.Run(tt.in, func(t *testing.T) {
			t.Setenv("PING_COUNT", tt.in)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			if cfg.PingCount != tt.want {
				t.Errorf("PING_COUNT=%s -> PingCount = %d, want %d", tt.in, cfg.PingCount, tt.want)
			}
		})
	}
}

func TestLoad_RequiresPassword(t *testing.T) {
	// 清空可能影响结果的环境变量
	t.Setenv("HELIOX_MON_PASS", "")
	t.Setenv("HELIOX_ENV_PATH", "/nonexistent/.env")

	if _, err := Load(); err == nil {
		t.Fatal("缺少 HELIOX_MON_PASS 时 Load 应返回错误，实际为 nil")
	}
}

func TestLoad_ParsesThresholdsAndTargets(t *testing.T) {
	t.Setenv("HELIOX_MON_PASS", "secret")
	t.Setenv("HELIOX_ENV_PATH", "/nonexistent/.env")
	t.Setenv("ALERT_THRESHOLDS", "70, 85 ,99")
	t.Setenv("PING_TARGETS", "A:1.1.1.1, B:2.2.2.2 ,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	wantThresholds := []int{70, 85, 99}
	if len(cfg.AlertThresholds) != len(wantThresholds) {
		t.Fatalf("AlertThresholds = %v, want %v", cfg.AlertThresholds, wantThresholds)
	}
	for i, v := range wantThresholds {
		if cfg.AlertThresholds[i] != v {
			t.Errorf("AlertThresholds[%d] = %d, want %d", i, cfg.AlertThresholds[i], v)
		}
	}

	// 空白项应被跳过，仅保留两个有效目标
	if len(cfg.PingTargets) != 2 {
		t.Fatalf("PingTargets 数量 = %d, want 2 (%v)", len(cfg.PingTargets), cfg.PingTargets)
	}
	if cfg.PingTargets[0].Tag != "A" || cfg.PingTargets[0].IP != "1.1.1.1" {
		t.Errorf("PingTargets[0] = %+v, want {A 1.1.1.1}", cfg.PingTargets[0])
	}
}

func TestLoadHelioxEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# 注释行\nSNELL_PORT=12345\n\nVLESS_PORT = 8443 \nOTHER=ignored\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{HelioxEnvPath: envPath}
	if err := cfg.loadHelioxEnv(); err != nil {
		t.Fatalf("loadHelioxEnv 失败: %v", err)
	}
	if cfg.SnellPort != 12345 {
		t.Errorf("SnellPort = %d, want 12345", cfg.SnellPort)
	}
	if cfg.VlessPort != 8443 {
		t.Errorf("VlessPort = %d, want 8443", cfg.VlessPort)
	}
}

func TestLoadHelioxEnvDisabledProtocol(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "SNELL_PORT=12345\nVLESS_PORT=8443\nENABLE_SNELL=false\nENABLE_XRAY=true\n"
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{HelioxEnvPath: envPath}
	if err := cfg.loadHelioxEnv(); err != nil {
		t.Fatalf("loadHelioxEnv 失败: %v", err)
	}
	// 端口号保留，历史数据仍按它查询；只是不再出现在 ActivePorts 里
	if cfg.SnellPort != 12345 {
		t.Errorf("SnellPort = %d, want 12345", cfg.SnellPort)
	}
	if cfg.SnellEnabled || !cfg.XrayEnabled {
		t.Errorf("SnellEnabled=%v XrayEnabled=%v, want false/true", cfg.SnellEnabled, cfg.XrayEnabled)
	}
	if got := cfg.ActivePorts(); len(got) != 1 || got[0] != 8443 {
		t.Errorf("ActivePorts() = %v, want [8443]", got)
	}
}
