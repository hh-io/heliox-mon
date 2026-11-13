package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
)

// newTestServer 构造仅依赖 cfg/db 的最小 Server，用于测 echo/report/latency 三个新路由。
func newTestServer(t *testing.T, cfg *config.Config) (*Server, *storage.DB) {
	t.Helper()
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB 失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	return &Server{cfg: cfg, db: db}, db
}

func TestHandleEcho(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	rec := httptest.NewRecorder()

	handleEcho(rec, req) // 免认证：不传任何凭据

	if rec.Code != http.StatusNoContent {
		t.Errorf("状态码 = %d, want 204", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("body 应为空, got %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func postReport(t *testing.T, s *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/latency/report", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleLatencyReport(rec, req)
	return rec
}

func TestHandleLatencyReport_NoTokenConfigured(t *testing.T) {
	s, _ := newTestServer(t, &config.Config{ReportToken: ""})
	rec := postReport(t, s, "", `{"client":"x","samples":[{"rtt_ms":1,"sent":1,"lost":0}]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("未配置 token 应 403, got %d", rec.Code)
	}
}

func TestHandleLatencyReport_WrongToken(t *testing.T) {
	s, _ := newTestServer(t, &config.Config{ReportToken: "secret"})
	rec := postReport(t, s, "wrong", `{"client":"x","samples":[{"rtt_ms":1,"sent":1,"lost":0}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错误 token 应 401, got %d", rec.Code)
	}
}

func TestHandleLatencyReport_Valid(t *testing.T) {
	s, db := newTestServer(t, &config.Config{ReportToken: "secret"})
	body := `{"client":"home-mac","samples":[
		{"rtt_ms":45.2,"min_rtt":42.1,"mdev":3.4,"sent":10,"lost":0},
		{"rtt_ms":null,"sent":10,"lost":10}
	]}`
	rec := postReport(t, s, "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法上报应 200, got %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK       bool `json:"ok"`
		Accepted int  `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.OK || resp.Accepted != 2 {
		t.Errorf("响应 = %+v, want ok=true accepted=2", resp)
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM latency_records WHERE target = 'client:home-mac' AND is_aggregated = 0`,
	).Scan(&count); err != nil {
		t.Fatalf("查询落库行失败: %v", err)
	}
	if count != 2 {
		t.Errorf("落库行数 = %d, want 2", count)
	}
}

func TestHandleLatencyReport_RttsRecomputed(t *testing.T) {
	s, db := newTestServer(t, &config.Config{ReportToken: "secret"})
	body := `{"client":"home","samples":[{"rtts":[10,11,12,13,20],"sent":6,"lost":1}]}`
	rec := postReport(t, s, "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200, got %d %s", rec.Code, rec.Body.String())
	}
	var max, p95, avg float64
	var recv, lost int
	if err := db.QueryRow(`SELECT max_rtt, p95_rtt, rtt_ms, recv, lost FROM latency_records WHERE target='client:home'`).
		Scan(&max, &p95, &avg, &recv, &lost); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if max != 20 || p95 != 20 || recv != 5 || lost != 1 {
		t.Errorf("max=%v p95=%v recv=%d lost=%d avg=%v", max, p95, recv, lost, avg)
	}
}

func TestHandleLatencyReport_InvalidInputs(t *testing.T) {
	s, _ := newTestServer(t, &config.Config{ReportToken: "secret"})
	now := time.Now().Unix()

	cases := []struct {
		name string
		body string
	}{
		{"非法 client 名", `{"client":"bad name!","samples":[{"rtt_ms":1,"sent":1,"lost":0}]}`},
		{"空 samples", `{"client":"x","samples":[]}`},
		{"rtt 越界", `{"client":"x","samples":[{"rtt_ms":99999,"sent":1,"lost":0}]}`},
		{"lost 大于 sent", `{"client":"x","samples":[{"rtt_ms":1,"sent":1,"lost":5}]}`},
		{"rtts+lost 大于 sent", `{"client":"x","samples":[{"rtts":[1,2],"sent":2,"lost":1}]}`},
		{"ts 超窗", `{"client":"x","samples":[{"ts":` +
			jsonInt(now-48*3600) + `,"rtt_ms":1,"sent":1,"lost":0}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postReport(t, s, "secret", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s 应 400, got %d, body=%s", c.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleLatency_IncludesClientTargets(t *testing.T) {
	s, db := newTestServer(t, &config.Config{ReportToken: "secret"})
	// 插入一条当前时间的客户端记录
	if _, err := db.Exec(
		`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
		 VALUES (?, 'client:home-mac', 20, 18, 2, 10, 0, 0)`,
		time.Now().Unix(),
	); err != nil {
		t.Fatalf("插入客户端记录失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/latency", nil)
	rec := httptest.NewRecorder()
	s.handleLatency(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleLatency 应 200, got %d", rec.Code)
	}

	var resp struct {
		Targets []struct {
			Tag string `json:"tag"`
			IP  string `json:"ip"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	found := false
	for _, tg := range resp.Targets {
		if tg.IP == "client:home-mac" && tg.Tag == "home-mac" {
			found = true
		}
	}
	if !found {
		t.Errorf("targets 应含 {tag:home-mac, ip:client:home-mac}, got %+v", resp.Targets)
	}
}

func TestHandleLatency_PacketFields(t *testing.T) {
	s, db := newTestServer(t, &config.Config{
		PingTargets: []config.PingTarget{{Tag: "G", IP: "8.8.8.8"}},
	})
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO latency_records
		(ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated,
		 max_rtt, p95_rtt, sum_rtt, sum_sq, recv, rtts)
		VALUES (?, '8.8.8.8', 13.2, 10, 3.5, 5, 0, 0, 20, 20, 66, 900, 5, '[10,11,12,13,20]')`,
		now); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/latency", nil)
	rec := httptest.NewRecorder()
	s.handleLatency(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Targets []struct {
			Points []struct {
				Max  *float64  `json:"max_rtt"`
				P95  *float64  `json:"p95"`
				Recv int       `json:"recv"`
				RTTs []float64 `json:"rtts"`
			} `json:"points"`
			Stats struct {
				Max *float64 `json:"max"`
				P95 *float64 `json:"p95"`
				Avg *float64 `json:"avg"`
			} `json:"stats"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Targets) != 1 || len(resp.Targets[0].Points) != 1 {
		t.Fatalf("targets/points 异常: %+v", resp.Targets)
	}
	p := resp.Targets[0].Points[0]
	if p.Max == nil || *p.Max != 20 || p.P95 == nil || *p.P95 != 20 || p.Recv != 5 {
		t.Errorf("point max/p95/recv = %v %v %d", p.Max, p.P95, p.Recv)
	}
	if len(p.RTTs) != 5 {
		t.Errorf("rtts 长度 = %d, want 5", len(p.RTTs))
	}
	st := resp.Targets[0].Stats
	if st.Max == nil || *st.Max != 20 || st.P95 == nil || *st.P95 != 20 {
		t.Errorf("window stats max/p95 = %v %v", st.Max, st.P95)
	}
}

// jsonInt 将 int64 转为 JSON 数字字面量字符串，供拼接测试请求体。
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestValidateLatencySample_AggregateWeights 覆盖 recv/sum_rtt/sum_sq 三个聚合权重字段。
// 无 rtts 时它们被原样写库并参与之后所有窗口均值，且 7 天后固化进聚合行——
// 一条不设上界的上报足以永久污染该目标的读数。
func TestValidateLatencySample_AggregateWeights(t *testing.T) {
	now := time.Now().Unix()
	rtt30 := 30.0
	tests := []struct {
		name    string
		smp     latencySample
		wantErr bool
	}{
		{
			name: "正常：recv 与 sum 自洽",
			smp:  latencySample{Sent: 10, Lost: 0, Recv: 10, SumRTT: 300, SumSq: 9200},
		},
		{
			name:    "sum_rtt 远超 recv 上界",
			smp:     latencySample{Sent: 1, Lost: 0, Recv: 1, SumRTT: 5e11},
			wantErr: true,
		},
		{
			name:    "sum_sq 远超 recv 上界",
			smp:     latencySample{Sent: 1, Lost: 0, Recv: 1, SumSq: 5e15},
			wantErr: true,
		},
		{
			name:    "recv 大于 sent-lost",
			smp:     latencySample{Sent: 10, Lost: 5, Recv: 8},
			wantErr: true,
		},
		{
			name:    "全丢包却带权重",
			smp:     latencySample{Sent: 10, Lost: 10, Recv: 0, SumRTT: 100},
			wantErr: true,
		},
		{
			name:    "负权重",
			smp:     latencySample{Sent: 10, Lost: 0, Recv: 10, SumRTT: -1},
			wantErr: true,
		},
		{
			// 落库会是 recv=10、sum_rtt=0 的桶：既把整个窗口拖回回退路径，
			// 又给加权均值塞进一个分子为 0、分母为 10 的桶
			name:    "recv>0 却一个 RTT 都没给",
			smp:     latencySample{Sent: 10, Lost: 0, Recv: 10},
			wantErr: true,
		},
		{
			name: "recv>0 只给 rtt_ms（旧客户端）仍放行",
			smp:  latencySample{Sent: 10, Lost: 0, Recv: 10, RttMs: &rtt30},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			smp := tt.smp
			got := validateLatencySample(&smp, now)
			if (got != "") != tt.wantErr {
				t.Errorf("validateLatencySample() = %q, wantErr = %v", got, tt.wantErr)
			}
		})
	}
}

func TestStatsRulesComplete(t *testing.T) {
	full := strings.Join([]string{
		"-P INPUT ACCEPT",
		"-N HELIOX_STATS",
		"-A INPUT -j HELIOX_STATS",
		"-A OUTPUT -j HELIOX_STATS",
		"-A HELIOX_STATS -p tcp -m tcp --sport 443",
		"-A HELIOX_STATS -p tcp -m tcp --dport 443",
		"-A HELIOX_STATS -p udp -m udp --sport 443",
		"-A HELIOX_STATS -p udp -m udp --dport 443",
	}, "\n")
	if !statsRulesComplete(full, []int{443}) {
		t.Error("规则齐全时应返回 true")
	}
	if statsRulesComplete(strings.Replace(full, "-A OUTPUT -j HELIOX_STATS", "", 1), []int{443}) {
		t.Error("缺 OUTPUT 跳转时应返回 false")
	}
	if statsRulesComplete(strings.Replace(full, "-p udp -m udp --dport 443", "-p udp -m udp --dport 4430", 1), []int{443}) {
		t.Error("--dport 4430 不应被当作 443 的规则")
	}
	if statsRulesComplete("", []int{443}) {
		t.Error("空输出应返回 false")
	}
}
