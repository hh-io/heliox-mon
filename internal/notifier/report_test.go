package notifier

import (
	"strings"
	"testing"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
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
		{100, 0, "无对比"},      // 前日无数据
		{120, 100, "↑20.0%"}, // 上升
		{80, 100, "↓20.0%"},  // 下降
		{100, 100, "持平"},     // 无变化
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

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"Google": 6,
		"电信":     4, // 两个 CJK 字符
		"CT电信":   6, // ASCII+CJK 混合
	}
	for in, want := range cases {
		if got := displayWidth(in); got != want {
			t.Errorf("displayWidth(%q)=%d，期望 %d", in, got, want)
		}
	}
}

func TestDailyLatency(t *testing.T) {
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	defer db.Close()

	// 窗口 [1000, 2000)
	const start, end int64 = 1000, 2000
	// good: 两条有效记录，avg=(10+20)/2=15，min=8，无丢包
	insertLatency(t, db, "1.1.1.1", 1100, 10, 8, 5, 0)
	insertLatency(t, db, "1.1.1.1", 1200, 20, 9, 5, 0)
	// 窗口外的记录不应计入
	insertLatency(t, db, "1.1.1.1", 2500, 99, 99, 5, 0)
	// bad: 全丢包，rtt/min 为 NULL，sent=5 lost=5
	insertLatencyNull(t, db, "2.2.2.2", 1300, 5, 5)

	cfg := &config.Config{
		Timezone: time.UTC, // latencyWindow 需要非 nil 时区
		PingTargets: []config.PingTarget{
			{Tag: "A&B", IP: "1.1.1.1"}, // 含 HTML 元字符，验证转义
			{Tag: "Bad", IP: "2.2.2.2"},
		},
	}
	n := New(cfg, db)

	stats := n.dailyLatency(start, end)
	if len(stats) != 2 {
		t.Fatalf("stats 长度=%d，期望 2", len(stats))
	}
	if !stats[0].ok || stats[0].avgRTT != 15 || stats[0].minRTT != 8 || stats[0].loss != 0 {
		t.Errorf("good 目标 stats=%+v，期望 avg=15 min=8 loss=0 ok=true", stats[0])
	}
	if stats[1].ok || stats[1].loss != 100 {
		t.Errorf("bad 目标 stats=%+v，期望 ok=false loss=100", stats[1])
	}

	// 小节应为 HTML 富文本：含加粗标题、<pre> 等宽块、ms 单位、无数据标记，且 Tag 已转义
	sec := n.latencySection(start, end)
	for _, want := range []string{"<b>网络延迟</b>", "<pre>", "</pre>", "ms", "无数据", "A&amp;B"} {
		if !strings.Contains(sec, want) {
			t.Errorf("小节缺少 %q：\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "A&B") { // 未转义的原文不应出现
		t.Errorf("Tag 未转义：\n%s", sec)
	}
}

// insertLatency 插入一条有效延迟记录（is_aggregated=0）。
func insertLatency(t *testing.T, db *storage.DB, target string, ts int64, rtt, min float64, sent, lost int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
		 VALUES (?, ?, ?, ?, 0, ?, ?, 0)`,
		ts, target, rtt, min, sent, lost,
	); err != nil {
		t.Fatalf("插入延迟记录失败: %v", err)
	}
}

// insertLatencyNull 插入一条全丢包记录（rtt_ms/min_rtt 为 NULL）。
func insertLatencyNull(t *testing.T, db *storage.DB, target string, ts int64, sent, lost int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
		 VALUES (?, ?, NULL, NULL, NULL, ?, ?, 0)`,
		ts, target, sent, lost,
	); err != nil {
		t.Fatalf("插入全丢包记录失败: %v", err)
	}
}
