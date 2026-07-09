package collector

import (
	"testing"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
)

// TestAggregateLatencyData_ClientTarget 验证客户端上报目标（client:*）与服务端 ping
// 目标一样被 aggregateLatencyData 降采样，无需任何特殊处理（GROUP BY target 泛化）。
func TestAggregateLatencyData_ClientTarget(t *testing.T) {
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB 失败: %v", err)
	}
	defer db.Close()

	c := &Collector{cfg: &config.Config{Timezone: time.UTC}, db: db}

	// 插入 8 天前的客户端原始记录（早于 7 天原始保留期，应被降采样）
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	for i := int64(0); i < 3; i++ {
		if _, err := db.Exec(
			`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
			 VALUES (?, 'client:home-mac', 30, 25, 2, 10, 0, 0)`,
			old+i,
		); err != nil {
			t.Fatalf("插入原始记录失败: %v", err)
		}
	}

	c.aggregateLatencyData()

	var aggCount, rawCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM latency_records WHERE target='client:home-mac' AND is_aggregated=1`,
	).Scan(&aggCount); err != nil {
		t.Fatalf("查询聚合行失败: %v", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM latency_records WHERE target='client:home-mac' AND is_aggregated=0`,
	).Scan(&rawCount); err != nil {
		t.Fatalf("查询原始行失败: %v", err)
	}

	if aggCount != 1 {
		t.Errorf("应生成 1 条 10 分钟桶聚合行, got %d", aggCount)
	}
	if rawCount != 0 {
		t.Errorf("原始记录应已被清理, got %d", rawCount)
	}
}
