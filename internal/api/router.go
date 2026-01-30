// Package api HTTP API 服务
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/hh/heliox-mon/internal/config"
	"github.com/hh/heliox-mon/internal/storage"
	"github.com/hh/heliox-mon/web"
)

// Server HTTP 服务器
type Server struct {
	cfg              *config.Config
	db               *storage.DB
	server           *http.Server
	realtimeProvider RealtimeDataProvider
}

// RealtimeDataProvider 实时数据提供者接口
type RealtimeDataProvider interface {
	GetRealtimeSpeed() (txSpeed, rxSpeed float64, ts int64)
}

// NewServer 创建服务器
func NewServer(cfg *config.Config, db *storage.DB, realtimeProvider RealtimeDataProvider) *Server {
	s := &Server{
		cfg:              cfg,
		db:               db,
		realtimeProvider: realtimeProvider,
	}

	mux := http.NewServeMux()

	// API 路由
	// Public
	mux.HandleFunc("/login", s.handleLoginView)
	mux.HandleFunc("/api/login", s.handleLoginAPI)

	// API 路由 (Auth)
	mux.HandleFunc("/api/stats", s.auth(s.handleStats))
	mux.HandleFunc("/api/system", s.auth(s.handleSystem))
	mux.HandleFunc("/api/traffic/daily", s.auth(s.handleTrafficDaily))
	mux.HandleFunc("/api/traffic/monthly", s.auth(s.handleTrafficMonthly))
	mux.HandleFunc("/api/traffic/realtime", s.auth(s.handleTrafficRealtime))
	mux.HandleFunc("/api/traffic/ports", s.auth(s.handlePortTraffic))
	mux.HandleFunc("/api/latency", s.auth(s.handleLatency))
	mux.HandleFunc("/api/config", s.auth(s.handleConfig))

	// 静态文件 (Auth with exceptions)
	mux.HandleFunc("/", s.auth(s.handleStatic))

	s.server = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	return s
}

// Start 启动服务器
func (s *Server) Start() error {
	log.Printf("HTTP 服务启动: %s", s.cfg.ListenAddr)
	return s.server.ListenAndServe()
}

// Stop 停止服务器
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.server.Shutdown(ctx)
}

const (
	authCookieName = "heliox_auth"
	authSalt       = "heliox_static_salt_v1"
)

// auth 认证中间件 (Cookie + Basic Fallback)
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 公开资源 (CSS/JS/Favicon)
		// 注意: /login 由单独 handler 处理，实际上不会经过这里(除非 mux 匹配逻辑特殊)，
		// 但为了保险起见，style.css 等静态资源如果是通过 "/" handleStatic 服务的，
		// 必须在这里放行。
		if r.URL.Path == "/style.css" || r.URL.Path == "/favicon.svg" {
			next(w, r)
			return
		}

		// 2. Cookie 验证
		cookie, err := r.Cookie(authCookieName)
		if err == nil && s.validateToken(cookie.Value) {
			next(w, r)
			return
		}

		// 3. Basic Auth 验证 (API兼容性/旧脚本)
		user, pass, ok := r.BasicAuth()
		if ok && user == s.cfg.Username && pass == s.cfg.Password {
			next(w, r)
			return
		}

		// 4. 未授权
		if strings.HasPrefix(r.URL.Path, "/api") {
			// API 请求返回 401
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// 浏览器请求重定向到 /login
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// handleLoginView 登录页面
func (s *Server) handleLoginView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 如果已登录，跳转首页
	if cookie, err := r.Cookie(authCookieName); err == nil && s.validateToken(cookie.Value) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	data, err := fs.ReadFile(web.Assets, "login.html")
	if err != nil {
		http.Error(w, "Login page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleLoginAPI 登录接口
func (s *Server) handleLoginAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Turnstile 验证
	if s.cfg.TurnstileSecretKey != "" {
		if req.TurnstileToken == "" {
			http.Error(w, "Missing captcha token", http.StatusForbidden)
			return
		}
		if !s.verifyTurnstile(req.TurnstileToken, r.RemoteAddr) {
			http.Error(w, "Captcha validation failed", http.StatusForbidden)
			return
		}
	}

	if req.Username != s.cfg.Username || req.Password != s.cfg.Password {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token := s.generateToken()
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 30, // 30天
		SameSite: http.SameSiteStrictMode,
	})

	w.WriteHeader(http.StatusOK)
}

func (s *Server) generateToken() string {
	hash := sha256.Sum256([]byte(s.cfg.Username + ":" + s.cfg.Password + ":" + authSalt))
	return hex.EncodeToString(hash[:])
}

func (s *Server) validateToken(token string) bool {
	return token == s.generateToken()
}

// verifyTurnstile 验证 Turnstile Token
func (s *Server) verifyTurnstile(token string, remoteIP string) bool {
	// 去除端口号 (IPv4/IPv6 compatibility)
	if idx := strings.LastIndex(remoteIP, ":"); idx != -1 {
		// 简单的判断，如果有点，可能是ipv4:port，或者ipv6 [::]:port
		// 这里简化处理，直接透传或不传ip给cloudflare也行
		// Cloudflare 文档说 remoteip 是可选的
	}

	formData := url.Values{}
	formData.Set("secret", s.cfg.TurnstileSecretKey)
	formData.Set("response", token)
	formData.Set("remoteip", remoteIP)

	resp, err := http.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify", formData)
	if err != nil {
		log.Printf("Turnstile verification error: %v", err)
		return false // Fail secure
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	var result struct {
		Success bool `json:"success"`
		// ErrorCodes []string `json:"error-codes"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("Turnstile response parse error: %v", err)
		return false
	}

	return result.Success
}

// handleStats 仪表盘汇总数据
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 计算计费周期（支持 ResetDay）
	billingStart, _ := s.getBillingCycleDates(now)
	// 计算自然月（用于上月流量）
	lastMonthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, tz)
	lastMonthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, tz).Add(-time.Second)

	stats := map[string]interface{}{
		"server_name":  s.cfg.ServerName,
		"timezone":     tz.String(),
		"current_time": now.Format("2006-01-02 15:04:05"),
	}

	// 今日流量（直接从快照表实时计算，与端口流量保持一致）
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	todayEnd := todayStart.Add(24*time.Hour - time.Second)
	row := s.db.QueryRow(`
		SELECT COALESCE(MAX(tx_bytes) - MIN(tx_bytes), 0),
		       COALESCE(MAX(rx_bytes) - MIN(rx_bytes), 0)
		FROM traffic_snapshots
		WHERE iface = 'total' AND ts >= ? AND ts <= ?
	`, todayStart.Unix(), todayEnd.Unix())
	var todayTx, todayRx int64
	if err := row.Scan(&todayTx, &todayRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询今日流量失败: %v", err)
	}
	stats["today"] = map[string]int64{"tx": todayTx, "rx": todayRx}

	// 昨日流量
	row = s.db.QueryRow(
		"SELECT COALESCE(tx_bytes, 0), COALESCE(rx_bytes, 0) FROM traffic_daily WHERE date = ? AND iface = 'total'",
		yesterday,
	)
	var yesterdayTx, yesterdayRx int64
	if err := row.Scan(&yesterdayTx, &yesterdayRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询昨日流量失败: %v", err)
	}
	stats["yesterday"] = map[string]int64{"tx": yesterdayTx, "rx": yesterdayRx}

	// 本月/当前周期流量（根据 ResetDay 计算）
	row = s.db.QueryRow(
		"SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0) FROM traffic_daily WHERE date >= ? AND iface = 'total'",
		billingStart.Format("2006-01-02"),
	)
	var monthTx, monthRx int64
	if err := row.Scan(&monthTx, &monthRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询本月流量失败: %v", err)
	}
	stats["this_month"] = map[string]int64{"tx": monthTx, "rx": monthRx}

	// 上月流量（自然月）
	row = s.db.QueryRow(
		"SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0) FROM traffic_daily WHERE date >= ? AND date <= ? AND iface = 'total'",
		lastMonthStart.Format("2006-01-02"),
		lastMonthEnd.Format("2006-01-02"),
	)
	var lastMonthTx, lastMonthRx int64
	if err := row.Scan(&lastMonthTx, &lastMonthRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询上月流量失败: %v", err)
	}
	stats["last_month"] = map[string]int64{"tx": lastMonthTx, "rx": lastMonthRx}

	// 根据 billing_mode 计算已用流量
	var usedBytes int64
	switch s.cfg.BillingMode {
	case "tx_only":
		usedBytes = monthTx
	case "rx_only":
		usedBytes = monthRx
	case "max_value":
		if monthTx > monthRx {
			usedBytes = monthTx
		} else {
			usedBytes = monthRx
		}
	default: // bidirectional
		usedBytes = monthTx + monthRx
	}
	stats["used_bytes"] = usedBytes

	stats["monthly_limit_gb"] = s.cfg.MonthlyLimitGB
	stats["billing_mode"] = s.cfg.BillingMode
	stats["reset_day"] = s.cfg.ResetDay
	stats["alert_thresholds"] = s.cfg.AlertThresholds

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// getBillingCycleDates 根据 ResetDay 计算计费周期起止日期
func (s *Server) getBillingCycleDates(now time.Time) (start, end time.Time) {
	day := s.cfg.ResetDay
	tz := s.cfg.Timezone

	if now.Day() >= day {
		// 当前周期从本月 ResetDay 开始
		start = time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, tz)
	} else {
		// 当前周期从上月 ResetDay 开始
		start = time.Date(now.Year(), now.Month()-1, day, 0, 0, 0, 0, tz)
	}
	end = start.AddDate(0, 1, 0).Add(-time.Second)
	return
}

// handleSystem 系统资源
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	row := s.db.QueryRow(
		"SELECT ts, cpu_percent, mem_used, mem_total, disk_used, disk_total, load_1, load_5, load_15 FROM system_metrics ORDER BY ts DESC LIMIT 1",
	)

	var ts int64
	var cpu, load1, load5, load15 float64
	var memUsed, memTotal, diskUsed, diskTotal int64

	if err := row.Scan(&ts, &cpu, &memUsed, &memTotal, &diskUsed, &diskTotal, &load1, &load5, &load15); err != nil {
		http.Error(w, "No data", http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"ts":          ts,
		"cpu_percent": cpu,
		"mem_used":    memUsed,
		"mem_total":   memTotal,
		"disk_used":   diskUsed,
		"disk_total":  diskTotal,
		"load_1":      load1,
		"load_5":      load5,
		"load_15":     load15,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleTrafficDaily 每日流量
func (s *Server) handleTrafficDaily(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	rangeType := r.URL.Query().Get("range")

	var startDate time.Time
	var endDate time.Time

	switch rangeType {
	case "cycle":
		startDate, _ = s.getBillingCycleDates(now)
		endDate = now
	default:
		// 默认最近 30 天
		endDate = now
		startDate = now.AddDate(0, 0, -29)
	}

	rows, err := s.db.Query(
		`SELECT date, tx_bytes, rx_bytes
		 FROM traffic_daily
		 WHERE iface = 'total' AND date >= ? AND date <= ?
		 ORDER BY date ASC`,
		startDate.Format("2006-01-02"),
		endDate.Format("2006-01-02"),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var data []map[string]interface{}
	for rows.Next() {
		var date string
		var tx, rx int64
		rows.Scan(&date, &tx, &rx)
		data = append(data, map[string]interface{}{
			"date": date,
			"tx":   tx,
			"rx":   rx,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleTrafficMonthly 月度汇总（返回近 6 个月，包含端口数据）
func (s *Server) handleTrafficMonthly(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.Timezone
	now := time.Now().In(tz)

	// 生成近 6 个月的月份列表
	months := make([]string, 6)
	for i := 0; i < 6; i++ {
		m := now.AddDate(0, -i, 0)
		months[5-i] = m.Format("2006-01") // 倒序填充，最终正序
	}

	// 查询整体流量（分上传下载）
	type totalTraffic struct {
		tx, rx int64
	}
	totalData := make(map[string]totalTraffic)
	rows, err := s.db.Query(`
		SELECT strftime('%Y-%m', date) as month, SUM(tx_bytes), SUM(rx_bytes)
		FROM traffic_daily
		WHERE iface = 'total'
		GROUP BY month
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var month string
			var tx, rx int64
			rows.Scan(&month, &tx, &rx)
			totalData[month] = totalTraffic{tx, rx}
		}
	}

	// 查询端口流量（分上传下载）
	type portTraffic struct {
		tx, rx int64
	}
	portData := make(map[string]map[int]portTraffic) // month -> port -> {tx, rx}
	rows2, err := s.db.Query(`
		SELECT strftime('%Y-%m', date) as month, port, SUM(tx_bytes), SUM(rx_bytes)
		FROM port_traffic_daily
		GROUP BY month, port
	`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var month string
			var port int
			var tx, rx int64
			rows2.Scan(&month, &port, &tx, &rx)
			if portData[month] == nil {
				portData[month] = make(map[int]portTraffic)
			}
			portData[month][port] = portTraffic{tx, rx}
		}
	}

	// 组装结果
	data := make([]map[string]interface{}, 6)
	for i, month := range months {
		snell := portTraffic{}
		vless := portTraffic{}
		if pd, ok := portData[month]; ok {
			snell = pd[s.cfg.SnellPort]
			vless = pd[s.cfg.VlessPort]
		}
		total := totalData[month]
		totalSum := total.tx + total.rx
		totalGB := float64(totalSum) / 1024 / 1024 / 1024

		data[i] = map[string]interface{}{
			"month":    month,
			"snell_tx": snell.tx,
			"snell_rx": snell.rx,
			"vless_tx": vless.tx,
			"vless_rx": vless.rx,
			"total_tx": total.tx,
			"total_rx": total.rx,
			"total":    totalSum,
			"total_gb": fmt.Sprintf("%.2f", totalGB),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleTrafficRealtime SSE 实时推送
func (s *Server) handleTrafficRealtime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(1 * time.Second) // 1秒推送
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// 从内存读取实时网速（采集器每秒更新）
			txSpeed, rxSpeed, ts := s.realtimeProvider.GetRealtimeSpeed()
			if ts == 0 {
				continue
			}

			data := map[string]interface{}{
				"tx_speed": txSpeed,
				"rx_speed": rxSpeed,
				"ts":       ts,
			}
			jsonData, _ := json.Marshal(data)
			w.Write([]byte("data: " + string(jsonData) + "\n\n"))
			flusher.Flush()
		}
	}
}

// handleLatency 延迟数据（支持时间范围、动态粒度聚合）
func (s *Server) handleLatency(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.Timezone
	now := time.Now().In(tz)

	// 解析时间范围参数，默认最近 24 小时
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	todayStr := now.Format("2006-01-02")

	var startTime, endTime time.Time
	if startStr != "" && endStr != "" {
		// 解析 YYYY-MM-DD 格式
		startTime, _ = time.ParseInLocation("2006-01-02", startStr, tz)
		endTime, _ = time.ParseInLocation("2006-01-02", endStr, tz)
		if endStr == todayStr {
			endTime = now
		} else {
			endTime = endTime.Add(24*time.Hour - time.Second) // 包含当天最后一秒
		}
	} else {
		// 默认最近 24 小时
		endTime = now
		startTime = now.Add(-24 * time.Hour)
	}

	// 计算时间跨度和粒度（保持约 1440 个点）
	duration := endTime.Sub(startTime)
	granularityMinutes := chooseLatencyGranularity(duration)
	if startStr == "" || endStr == "" {
		granularityMinutes = 1
	}
	granularitySec := int64(granularityMinutes * 60)

	startTs := startTime.Unix()
	endTs := endTime.Unix()

	// 返回所有 target 的数据
	result := map[string]interface{}{
		"targets":     []map[string]interface{}{},
		"start":       startTime.Format("2006-01-02 15:04:05"),
		"end":         endTime.Format("2006-01-02 15:04:05"),
		"granularity": granularityMinutes,
	}

	for _, pt := range s.cfg.PingTargets {
		// 按粒度聚合查询：按时间桶分组，计算平均 RTT
		rows, err := s.db.Query(`
			SELECT (ts / ?) * ? as bucket_ts,
			       AVG(rtt_ms) as avg_rtt,
			       SUM(COALESCE(sent, 0)) as sent,
			       SUM(COALESCE(lost, 0)) as lost
			FROM latency_records
			WHERE target = ? AND ts >= ? AND ts <= ?
			GROUP BY bucket_ts
			ORDER BY bucket_ts
		`, granularitySec, granularitySec, pt.Tag, startTs, endTs)
		if err != nil {
			continue
		}

		var points []map[string]interface{}
		var sum, min, max float64
		var count int
		min = 999999
		var totalSent, totalLost int64

		for rows.Next() {
			var ts int64
			var rtt sql.NullFloat64
			var sent sql.NullInt64
			var lost sql.NullInt64
			rows.Scan(&ts, &rtt, &sent, &lost)
			sentVal := sent.Int64
			lostVal := lost.Int64
			if !sent.Valid {
				sentVal = 0
			}
			if !lost.Valid {
				lostVal = 0
			}
			totalSent += sentVal
			totalLost += lostVal
			lossRate := 0.0
			if sentVal > 0 {
				lossRate = float64(lostVal) / float64(sentVal) * 100
			}

			var rttVal interface{}
			if rtt.Valid {
				rttVal = rtt.Float64
				sum += rtt.Float64
				count++
				if rtt.Float64 < min {
					min = rtt.Float64
				}
				if rtt.Float64 > max {
					max = rtt.Float64
				}
			} else {
				rttVal = nil
			}

			points = append(points, map[string]interface{}{
				"ts":     ts,
				"rtt_ms": rttVal,
				"loss":   lossRate,
				"sent":   sentVal,
				"lost":   lostVal,
			})
		}
		rows.Close()

		avg := 0.0
		if count > 0 {
			avg = sum / float64(count)
		}
		if min == 999999 {
			min = 0
		}
		lossRate := 0.0
		if totalSent > 0 {
			lossRate = float64(totalLost) / float64(totalSent) * 100
		}

		targetData := map[string]interface{}{
			"tag":    pt.Tag,
			"ip":     pt.IP,
			"points": points,
			"stats": map[string]interface{}{
				"avg":   avg,
				"min":   min,
				"max":   max,
				"count": count,
				"loss":  lossRate,
			},
		}
		result["targets"] = append(result["targets"].([]map[string]interface{}), targetData)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func chooseLatencyGranularity(duration time.Duration) int {
	minutes := int(math.Ceil(duration.Minutes()))
	if minutes <= 0 {
		return 1
	}

	raw := int(math.Ceil(float64(minutes) / 1440.0))
	if raw < 1 {
		raw = 1
	}

	steps := []int{1, 2, 3, 5, 10, 15, 30, 60, 120, 180, 240, 360, 720, 1440}
	for _, step := range steps {
		if raw <= step {
			return step
		}
	}

	return raw
}

// handlePortTraffic 端口流量统计
func (s *Server) handlePortTraffic(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	todayEnd := todayStart.Add(24*time.Hour - time.Second)

	// 计算计费周期
	billingStart, _ := s.getBillingCycleDates(now)
	lastMonthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, tz)
	lastMonthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, tz).Add(-time.Second)

	// 获取配置的端口
	ports := []struct {
		Port int    `json:"port"`
		Name string `json:"name"`
	}{}
	if s.cfg.SnellPort > 0 {
		ports = append(ports, struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}{Port: s.cfg.SnellPort, Name: "Snell"})
	}
	if s.cfg.VlessPort > 0 {
		ports = append(ports, struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}{Port: s.cfg.VlessPort, Name: "VLESS"})
	}

	// 检测 iptables 规则是否存在
	portNums := make([]int, 0, len(ports))
	for _, p := range ports {
		portNums = append(portNums, p.Port)
	}
	iptablesOK := s.checkIptablesRules(portNums)

	result := map[string]interface{}{
		"ports":       []map[string]interface{}{},
		"iptables_ok": iptablesOK,
	}

	for _, p := range ports {
		portData := map[string]interface{}{
			"port": p.Port,
			"name": p.Name,
		}

		// 今日流量
		row := s.db.QueryRow(`
			SELECT COALESCE(MAX(tx_bytes) - MIN(tx_bytes), 0),
			       COALESCE(MAX(rx_bytes) - MIN(rx_bytes), 0)
			FROM port_traffic_snapshots
			WHERE port = ? AND ts >= ? AND ts <= ?
		`, p.Port, todayStart.Unix(), todayEnd.Unix())
		var todayTx, todayRx int64
		row.Scan(&todayTx, &todayRx)
		portData["today"] = map[string]int64{"tx": todayTx, "rx": todayRx, "total": todayTx + todayRx}

		// 昨日流量
		row = s.db.QueryRow(`
			SELECT COALESCE(tx_bytes, 0), COALESCE(rx_bytes, 0)
			FROM port_traffic_daily
			WHERE port = ? AND date = ?
		`, p.Port, yesterday)
		var yesterdayTx, yesterdayRx int64
		row.Scan(&yesterdayTx, &yesterdayRx)
		portData["yesterday"] = map[string]int64{"tx": yesterdayTx, "rx": yesterdayRx, "total": yesterdayTx + yesterdayRx}

		// 本月流量（从日表查询，排除今日避免重复）
		row = s.db.QueryRow(`
			SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
			FROM port_traffic_daily
			WHERE port = ? AND date >= ? AND date < ?
		`, p.Port, billingStart.Format("2006-01-02"), today)
		var monthTx, monthRx int64
		row.Scan(&monthTx, &monthRx)
		// 加上今日（从快照计算的实时数据）
		monthTx += todayTx
		monthRx += todayRx
		portData["this_month"] = map[string]int64{"tx": monthTx, "rx": monthRx, "total": monthTx + monthRx}

		// 上月流量
		row = s.db.QueryRow(`
			SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
			FROM port_traffic_daily
			WHERE port = ? AND date >= ? AND date <= ?
		`, p.Port, lastMonthStart.Format("2006-01-02"), lastMonthEnd.Format("2006-01-02"))
		var lastMonthTx, lastMonthRx int64
		row.Scan(&lastMonthTx, &lastMonthRx)
		portData["last_month"] = map[string]int64{"tx": lastMonthTx, "rx": lastMonthRx, "total": lastMonthTx + lastMonthRx}

		result["ports"] = append(result["ports"].([]map[string]interface{}), portData)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleConfig 配置管理
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// 返回当前配置
		cfg := map[string]interface{}{
			"monthly_limit_gb": s.cfg.MonthlyLimitGB,
			"billing_mode":     s.cfg.BillingMode,
			"reset_day":        s.cfg.ResetDay,
			"alert_thresholds": s.cfg.AlertThresholds,
			"ping_targets":     s.cfg.PingTargets,
			"telegram_enabled": s.cfg.TelegramBotToken != "",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
		return
	}

	// POST 更新配置 (TODO: 持久化到数据库)
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}

// handleStatic 静态文件服务（使用嵌入文件）
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	path = strings.TrimPrefix(path, "/")

	// 设置 Content-Type
	switch {
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}

	data, err := fs.ReadFile(web.Assets, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

// checkIptablesRules 检测 iptables 规则是否存在
func (s *Server) checkIptablesRules(ports []int) bool {
	if len(ports) == 0 {
		return true
	}

	cmd := exec.Command("iptables", "-S")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	rules := strings.Split(string(output), "\n")
	hasInputJump := false
	hasOutputJump := false
	dptTCP := make(map[int]bool)
	sptTCP := make(map[int]bool)
	dptUDP := make(map[int]bool)
	sptUDP := make(map[int]bool)

	for _, line := range rules {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "-A INPUT -j HELIOX_STATS" {
			hasInputJump = true
			continue
		}
		if line == "-A OUTPUT -j HELIOX_STATS" {
			hasOutputJump = true
			continue
		}
		if !strings.HasPrefix(line, "-A HELIOX_STATS ") {
			continue
		}
		proto := ""
		if strings.Contains(line, "-p tcp") {
			proto = "tcp"
		} else if strings.Contains(line, "-p udp") {
			proto = "udp"
		} else {
			continue
		}
		for _, port := range ports {
			if port <= 0 {
				continue
			}
			if strings.Contains(line, fmt.Sprintf("--dport %d", port)) {
				if proto == "tcp" {
					dptTCP[port] = true
				} else {
					dptUDP[port] = true
				}
			}
			if strings.Contains(line, fmt.Sprintf("--sport %d", port)) {
				if proto == "tcp" {
					sptTCP[port] = true
				} else {
					sptUDP[port] = true
				}
			}
		}
	}

	if !hasInputJump || !hasOutputJump {
		return false
	}
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		if !dptTCP[port] || !sptTCP[port] || !dptUDP[port] || !sptUDP[port] {
			return false
		}
	}
	return true
}
