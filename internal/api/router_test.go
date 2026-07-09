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

// jsonInt 将 int64 转为 JSON 数字字面量字符串，供拼接测试请求体。
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
