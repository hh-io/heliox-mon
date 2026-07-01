package collector

import (
	"testing"
	"time"
)

func TestNextReportTime(t *testing.T) {
	tz := time.UTC

	// 当天目标时刻尚未到 -> 取今天
	now := time.Date(2026, 7, 1, 8, 30, 0, 0, tz)
	got := nextReportTime(now, 9, tz)
	want := time.Date(2026, 7, 1, 9, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("未到点应取今日 9 点：got %v want %v", got, want)
	}

	// 已过目标时刻 -> 顺延到次日
	now = time.Date(2026, 7, 1, 9, 0, 1, 0, tz)
	got = nextReportTime(now, 9, tz)
	want = time.Date(2026, 7, 2, 9, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("已过点应顺延次日：got %v want %v", got, want)
	}

	// 正好等于目标时刻 -> 顺延到次日（避免同一刻重复触发）
	now = time.Date(2026, 7, 1, 9, 0, 0, 0, tz)
	got = nextReportTime(now, 9, tz)
	want = time.Date(2026, 7, 2, 9, 0, 0, 0, tz)
	if !got.Equal(want) {
		t.Errorf("恰逢点应顺延次日：got %v want %v", got, want)
	}
}
