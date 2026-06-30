package collector

import (
	"fmt"
	"strings"
	"testing"
)

// TestParsePingOutput 测试 ping 输出解析
func TestParsePingOutput(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		expectedCount int
		wantOK        bool
		wantRTT       *float64
		wantMin       *float64
		wantMdev      *float64
		wantSent      int
		wantLost      int
	}{
		{
			name: "正常输出",
			output: `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.

--- 8.8.8.8 ping statistics ---
5 packets transmitted, 5 received, 0% packet loss, time 4005ms
rtt min/avg/max/mdev = 10.123/15.456/20.789/3.214 ms`,
			expectedCount: 5,
			wantOK:        true,
			wantRTT:       floatPtr(15.456),
			wantMin:       floatPtr(10.123),
			wantMdev:      floatPtr(3.214),
			wantSent:      5,
			wantLost:      0,
		},
		{
			name: "部分丢包",
			output: `--- 8.8.8.8 ping statistics ---
5 packets transmitted, 3 received, 40% packet loss, time 4005ms
rtt min/avg/max/mdev = 10.0/12.5/15.0/2.5 ms`,
			expectedCount: 5,
			wantOK:        true,
			wantRTT:       floatPtr(12.5),
			wantMin:       floatPtr(10.0),
			wantMdev:      floatPtr(2.5),
			wantSent:      5,
			wantLost:      2,
		},
		{
			name: "全部丢包",
			output: `--- 192.168.99.99 ping statistics ---
5 packets transmitted, 0 received, 100% packet loss, time 4090ms`,
			expectedCount: 5,
			wantOK:        true,
			wantRTT:       nil,
			wantMin:       nil,
			wantMdev:      nil,
			wantSent:      5,
			wantLost:      5,
		},
		{
			name: "旧版 ping 格式 (stddev)",
			output: `--- 1.1.1.1 ping statistics ---
10 packets transmitted, 10 received, 0% packet loss, time 9010ms
rtt min/avg/max/stddev = 8.000/10.500/12.000/1.234 ms`,
			expectedCount: 10,
			wantOK:        true,
			wantRTT:       floatPtr(10.500),
			wantMin:       floatPtr(8.000),
			wantMdev:      floatPtr(1.234),
			wantSent:      10,
			wantLost:      0,
		},
		{
			name: "BSD round-trip 无 stddev",
			output: `--- 1.1.1.1 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss
round-trip min/avg/max = 8.0/10.0/12.0 ms`,
			expectedCount: 3,
			wantOK:        true,
			wantRTT:       floatPtr(10.0),
			wantMin:       floatPtr(8.0),
			wantMdev:      nil,
			wantSent:      3,
			wantLost:      0,
		},
		{
			name: "无效输出",
			output: `invalid ping output
nothing useful here`,
			expectedCount: 5,
			wantOK:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePingOutput(tt.output, tt.expectedCount)

			if ok != tt.wantOK {
				t.Fatalf("parsePingOutput() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return // 解析失败时不校验其余字段
			}
			if !equalFloatPtr(got.avgRtt, tt.wantRTT) {
				t.Errorf("avgRtt = %v, want %v", formatFloatPtr(got.avgRtt), formatFloatPtr(tt.wantRTT))
			}
			if !equalFloatPtr(got.minRtt, tt.wantMin) {
				t.Errorf("minRtt = %v, want %v", formatFloatPtr(got.minRtt), formatFloatPtr(tt.wantMin))
			}
			if !equalFloatPtr(got.mdev, tt.wantMdev) {
				t.Errorf("mdev = %v, want %v", formatFloatPtr(got.mdev), formatFloatPtr(tt.wantMdev))
			}
			if got.sent != tt.wantSent {
				t.Errorf("sent = %v, want %v", got.sent, tt.wantSent)
			}
			if got.lost != tt.wantLost {
				t.Errorf("lost = %v, want %v", got.lost, tt.wantLost)
			}
		})
	}
}

// TestParsePingOutput_EdgeCases 边界测试
func TestParsePingOutput_EdgeCases(t *testing.T) {
	// 接收数大于发送数（异常情况）：丢包应被钳为 0
	output := `5 packets transmitted, 6 received, -20% packet loss`
	got, ok := parsePingOutput(output, 5)
	if !ok {
		t.Fatal("有统计行应解析成功")
	}
	if got.lost != 0 {
		t.Errorf("lost 应为 0（接收数 > 发送数），实际 = %d", got.lost)
	}
	if got.sent != 5 {
		t.Errorf("sent 应为 5，实际 = %d", got.sent)
	}
	if got.avgRtt != nil {
		t.Errorf("无 RTT 行不应解析出 RTT，实际 = %v", formatFloatPtr(got.avgRtt))
	}
}

// 辅助函数
func floatPtr(f float64) *float64 {
	return &f
}

func equalFloatPtr(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	diff := *a - *b
	if diff < 0 {
		diff = -diff
	}
	return diff < 0.001 // 允许浮点误差
}

func formatFloatPtr(f *float64) string {
	if f == nil {
		return "nil"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", *f), "0"), ".")
}
