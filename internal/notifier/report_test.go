package notifier

import (
	"testing"
	"time"
)

func TestBillingUsed(t *testing.T) {
	const tx, rx int64 = 100, 250
	cases := map[string]int64{
		"bidirectional": tx + rx,
		"tx_only":       tx,
		"rx_only":       rx,
		"max_value":     rx,
		"unknown":       tx + rx, // 未知模式回退到双向
	}
	for mode, want := range cases {
		if got := billingUsed(mode, tx, rx); got != want {
			t.Errorf("billingUsed(%q)=%d，期望 %d", mode, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		512:                    "512 B",
		1024:                   "1.00 KiB",
		1024 * 1024:            "1.00 MiB",
		1024 * 1024 * 1024:     "1.00 GiB",
		3 * 1024 * 1024 * 1024: "3.00 GiB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d)=%q，期望 %q", in, got, want)
		}
	}
}

func TestTrendText(t *testing.T) {
	cases := []struct {
		today, prev int64
		want        string
	}{
		{100, 0, "无对比"},  // 前日无数据
		{120, 100, "↑20.0%"}, // 上升
		{80, 100, "↓20.0%"},  // 下降
		{100, 100, "持平"},   // 无变化
	}
	for _, c := range cases {
		if got := trendText(c.today, c.prev); got != c.want {
			t.Errorf("trendText(%d,%d)=%q，期望 %q", c.today, c.prev, got, c.want)
		}
	}
}

func TestProgressBar(t *testing.T) {
	cases := map[float64]string{
		0:   "░░░░░░░░░░",
		50:  "█████░░░░░",
		100: "██████████",
		150: "██████████", // 超限按满格
	}
	for in, want := range cases {
		if got := progressBar(in); got != want {
			t.Errorf("progressBar(%.0f)=%q，期望 %q", in, got, want)
		}
	}
}

func TestWeekdayCN(t *testing.T) {
	// 2026-07-01 是周三
	d := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if got := weekdayCN(d); got != "周三" {
		t.Errorf("weekdayCN=%q，期望 周三", got)
	}
}
