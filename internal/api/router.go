// Package api HTTP API 服务
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
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
	notifier         Notifier
	sessions         sync.Map // token -> expireTime(int64)
	stopCleanup      chan struct{}

	// iptables 规则检测结果缓存（避免每个请求都 fork iptables）
	iptablesMu      sync.Mutex
	iptablesOK      bool
	iptablesChecked time.Time
}

// RealtimeDataProvider 实时数据提供者接口
type RealtimeDataProvider interface {
	GetRealtimeSpeed() (txSpeed, rxSpeed float64, ts int64)
}

// Notifier 通知发送接口（用于页面「测试发送」/「日报预览」手动触发）
type Notifier interface {
	SendTest() error
	SendDailyReport() error
}

// NewServer 创建服务器
func NewServer(cfg *config.Config, db *storage.DB, realtimeProvider RealtimeDataProvider, notifier Notifier) *Server {
	s := &Server{
		cfg:              cfg,
		db:               db,
		realtimeProvider: realtimeProvider,
		notifier:         notifier,
		stopCleanup:      make(chan struct{}),
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
	// 客户端测延与上报：echo 免认证，report 走独立 Bearer token（见各自 handler）
	mux.HandleFunc("/api/echo", handleEcho)
	mux.HandleFunc("/api/latency/report", s.handleLatencyReport)
	mux.HandleFunc("/api/config", s.auth(s.handleConfig))
	mux.HandleFunc("/api/notify/test", s.auth(s.handleNotifyTest))
	mux.HandleFunc("/api/notify/daily-report", s.auth(s.handleNotifyDailyReport))

	// 静态文件 (Auth with exceptions)
	mux.HandleFunc("/", s.auth(s.handleStatic))

	s.server = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
		// 设置读取超时防止 Slowloris 慢速连接耗尽资源。
		// 不设 WriteTimeout：/api/traffic/realtime 是 SSE 长连接，全局写超时会把它掐断。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return s
}

// Start 启动服务器
func (s *Server) Start() error {
	log.Printf("HTTP 服务启动: %s", s.cfg.ListenAddr)
	go s.cleanupSessions()
	return s.server.ListenAndServe()
}

// Stop 停止服务器
func (s *Server) Stop() {
	close(s.stopCleanup)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		log.Printf("HTTP 服务关闭异常: %v", err)
	}
}

// writeJSON 写出 JSON 响应；编码失败仅记录日志
// （响应体已开始写出，无法再修改状态码）
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写出 JSON 响应失败: %v", err)
	}
}

// writeJSONStatus 以指定 HTTP 状态码输出 JSON
func writeJSONStatus(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写出 JSON 响应失败: %v", err)
	}
}

// cleanupSessions 定期清理过期会话，避免长期未访问的 token 永久滞留内存
func (s *Server) cleanupSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCleanup:
			return
		case <-ticker.C:
			now := time.Now().Unix()
			s.sessions.Range(func(key, value interface{}) bool {
				if expire, ok := value.(int64); ok && now > expire {
					s.sessions.Delete(key)
				}
				return true
			})
		}
	}
}

const (
	authCookieName = "heliox_auth"
	authSessionTTL = 30 * 24 * time.Hour // 30 天
	authTokenBytes = 32
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
		if ok && s.checkCredentials(user, pass) {
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
	if _, err := w.Write(data); err != nil {
		log.Printf("写出登录页失败: %v", err)
	}
}

// handleLoginAPI 登录接口
func (s *Server) handleLoginAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 限制请求体大小，防止恶意超大 body
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

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

	if !s.checkCredentials(req.Username, req.Password) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token := s.generateToken()
	if token == "" {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		MaxAge:   int(authSessionTTL.Seconds()),
		SameSite: http.SameSiteLaxMode,
	})

	w.WriteHeader(http.StatusOK)
}

// isHTTPS 判断请求是否经由 HTTPS（兼容 Cloudflare Tunnel/反向代理的 X-Forwarded-Proto）
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// checkCredentials 使用常量时间比较验证用户名和密码
func (s *Server) checkCredentials(user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Password)) == 1
	return userOK && passOK
}

// generateToken 生成加密安全的随机 session token
func (s *Server) generateToken() string {
	b := make([]byte, authTokenBytes)
	if _, err := rand.Read(b); err != nil {
		log.Printf("生成 session token 失败: %v", err)
		return ""
	}
	token := hex.EncodeToString(b)
	s.sessions.Store(token, time.Now().Add(authSessionTTL).Unix())
	return token
}

// validateToken 验证 session token 是否有效
func (s *Server) validateToken(token string) bool {
	if token == "" {
		return false
	}
	v, ok := s.sessions.Load(token)
	if !ok {
		return false
	}
	expire := v.(int64)
	if time.Now().Unix() > expire {
		s.sessions.Delete(token)
		return false
	}
	return true
}

// verifyTurnstile 验证 Turnstile Token
func (s *Server) verifyTurnstile(token string, remoteIP string) bool {
	// 正确剥离端口号 (兼容 IPv4/IPv6)
	host, _, err := net.SplitHostPort(remoteIP)
	if err != nil {
		// 可能没有端口号，直接使用原始值
		host = remoteIP
	}

	formData := url.Values{}
	formData.Set("secret", s.cfg.TurnstileSecretKey)
	formData.Set("response", token)
	formData.Set("remoteip", host)

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
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("Turnstile response parse error: %v", err)
		return false
	}

	return result.Success
}

// handleStats 仪表盘汇总数据
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 计算计费周期（支持 ResetDay）
	billingStart, _ := s.cfg.GetBillingCycleDates(now)
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

	writeJSON(w, stats)
}

// handleSystem 系统资源
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

	writeJSON(w, data)
}

// handleTrafficDaily 每日流量
func (s *Server) handleTrafficDaily(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	rangeType := r.URL.Query().Get("range")

	var startDate time.Time
	var endDate time.Time

	switch rangeType {
	case "cycle":
		startDate, _ = s.cfg.GetBillingCycleDates(now)
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
		if err := rows.Scan(&date, &tx, &rx); err != nil {
			log.Printf("扫描每日流量行失败: %v", err)
			continue
		}
		data = append(data, map[string]interface{}{
			"date": date,
			"tx":   tx,
			"rx":   rx,
		})
	}

	writeJSON(w, data)
}

// handleTrafficMonthly 月度汇总（返回近 6 个月，包含端口数据）
func (s *Server) handleTrafficMonthly(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
			if err := rows.Scan(&month, &tx, &rx); err != nil {
				log.Printf("扫描月度流量行失败: %v", err)
				continue
			}
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
			if err := rows2.Scan(&month, &port, &tx, &rx); err != nil {
				log.Printf("扫描端口月度流量行失败: %v", err)
				continue
			}
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

	writeJSON(w, data)
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

	dataTicker := time.NewTicker(1 * time.Second)
	defer dataTicker.Stop()
	heartbeat := time.NewTicker(30 * time.Second) // 防止代理超时断开
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// 心跳帧，保持连接活跃
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-dataTicker.C:
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
			if _, err := w.Write([]byte("data: " + string(jsonData) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleLatency 延迟数据（支持时间范围、动态粒度聚合）
func (s *Server) handleLatency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

	// 目标列表 = 配置的 ping 目标 + 查询窗口内出现过的客户端上报目标（client:*）。
	// 客户端目标动态发现，无需配置即可在图表出现；ORDER BY 保证颜色分配稳定。
	targets := make([]config.PingTarget, 0, len(s.cfg.PingTargets))
	targets = append(targets, s.cfg.PingTargets...)
	if clientRows, err := s.db.Query(`
		SELECT DISTINCT target FROM latency_records
		WHERE target LIKE 'client:%' AND ts >= ? AND ts <= ?
		ORDER BY target
	`, startTs, endTs); err != nil {
		log.Printf("查询客户端延迟目标失败: %v", err)
	} else {
		for clientRows.Next() {
			var t string
			if err := clientRows.Scan(&t); err != nil {
				log.Printf("扫描客户端延迟目标失败: %v", err)
				continue
			}
			targets = append(targets, config.PingTarget{Tag: strings.TrimPrefix(t, "client:"), IP: t})
		}
		clientRows.Close()
	}

	for _, pt := range targets {
		// 按粒度聚合查询：按时间桶分组，计算平均 RTT
		rows, err := s.db.Query(`
			SELECT (ts / ?) * ? as bucket_ts,
			       AVG(rtt_ms) as avg_rtt,
			       MIN(min_rtt) as min_rtt,
			       AVG(mdev) as jitter,
			       SUM(COALESCE(sent, 0)) as sent,
			       SUM(COALESCE(lost, 0)) as lost
			FROM latency_records
			WHERE target = ? AND ts >= ? AND ts <= ?
			GROUP BY bucket_ts
			ORDER BY bucket_ts
		`, granularitySec, granularitySec, pt.IP, startTs, endTs)
		if err != nil {
			continue
		}

		var points []map[string]interface{}
		var sum, max float64
		var minRttOverall float64 = 999999
		var jitterSum float64
		var jitterCount int
		var count int
		var totalSent, totalLost int64

		for rows.Next() {
			var ts int64
			var rtt, minRtt, jitter sql.NullFloat64
			var sent sql.NullInt64
			var lost sql.NullInt64
			if err := rows.Scan(&ts, &rtt, &minRtt, &jitter, &sent, &lost); err != nil {
				log.Printf("扫描延迟数据行失败: %v", err)
				continue
			}
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
				if rtt.Float64 > max {
					max = rtt.Float64
				}
			}

			// min 取真实最小 RTT；旧数据无 min_rtt 时退化用平均值兜底
			var minVal interface{}
			if minRtt.Valid {
				minVal = minRtt.Float64
				if minRtt.Float64 < minRttOverall {
					minRttOverall = minRtt.Float64
				}
			} else if rtt.Valid && rtt.Float64 < minRttOverall {
				minRttOverall = rtt.Float64
			}

			var jitterVal interface{}
			if jitter.Valid {
				jitterVal = jitter.Float64
				jitterSum += jitter.Float64
				jitterCount++
			}

			points = append(points, map[string]interface{}{
				"ts":      ts,
				"rtt_ms":  rttVal,
				"min_rtt": minVal,
				"jitter":  jitterVal,
				"loss":    lossRate,
				"sent":    sentVal,
				"lost":    lostVal,
			})
		}
		rows.Close()

		avg := 0.0
		if count > 0 {
			avg = sum / float64(count)
		}
		if minRttOverall == 999999 {
			minRttOverall = 0
		}
		avgJitter := 0.0
		if jitterCount > 0 {
			avgJitter = jitterSum / float64(jitterCount)
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
				"avg":    avg,
				"min":    minRttOverall,
				"max":    max,
				"jitter": avgJitter,
				"count":  count,
				"loss":   lossRate,
			},
		}
		result["targets"] = append(result["targets"].([]map[string]interface{}), targetData)
	}

	writeJSON(w, result)
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

// handleEcho 客户端测延用的极简回显端点：免认证、不写库、不打日志，
// 保证服务端耗时相对毫秒级网络 RTT 可忽略（微秒级）。
// 客户端在同一 keep-alive 连接上串行请求多次、取 min 即为端到端净 RTT。
func handleEcho(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// clientNameRe 限制客户端名为字母/数字/下划线/连字符，长度 1-32，
// 既防注入又保证 target = "client:<name>" 不与 IP 目标冲突。
var clientNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// latencySample 单条客户端上报样本。RTT 类字段用指针区分「无数据」与 0ms。
type latencySample struct {
	TS     *int64   `json:"ts"`
	RttMs  *float64 `json:"rtt_ms"`
	MinRtt *float64 `json:"min_rtt"`
	Mdev   *float64 `json:"mdev"`
	Sent   int      `json:"sent"`
	Lost   int      `json:"lost"`
}

// handleLatencyReport 接收客户端主动上报的延迟样本，写入 latency_records，
// 完全复用现有存储/降采样/前端图表（target 用 "client:<name>" 命名空间前缀）。
// 独立 Bearer token 认证，与管理员密码隔离：泄露仅能写入伪造延迟数据，无法读数据或登录。
func (s *Server) handleLatencyReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 未配置上报令牌 = 功能未启用，直接拒绝（而非放行匿名写入）
	if s.cfg.ReportToken == "" {
		writeJSONStatus(w, http.StatusForbidden, map[string]interface{}{
			"ok": false, "message": "上报功能未启用",
		})
		return
	}

	// Bearer token 常量时间比较，防时序攻击（与 checkCredentials 同风格）
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.ReportToken)) != 1 {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]interface{}{
			"ok": false, "message": "无效的上报令牌",
		})
		return
	}

	// 限制请求体大小，防止恶意超大 body
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var req struct {
		Client  string          `json:"client"`
		Samples []latencySample `json:"samples"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "message": "请求体解析失败: " + err.Error(),
		})
		return
	}

	if !clientNameRe.MatchString(req.Client) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "message": "client 名非法（仅允许字母/数字/下划线/连字符，长度 1-32）",
		})
		return
	}
	if len(req.Samples) < 1 || len(req.Samples) > 100 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "message": "samples 数量须在 1-100 之间",
		})
		return
	}

	now := time.Now().Unix()
	// 校验每条样本，任一非法即整体拒绝并回显原因（不静默丢弃）
	for i := range req.Samples {
		if msg := validateLatencySample(&req.Samples[i], now); msg != "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
				"ok": false, "message": fmt.Sprintf("样本[%d] %s", i, msg),
			})
			return
		}
	}

	target := "client:" + req.Client
	tx, err := s.db.Begin()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
			"ok": false, "message": "数据库事务开启失败",
		})
		return
	}
	stmt, err := tx.Prepare(
		`INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
	)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			log.Printf("回滚上报事务失败: %v", rbErr)
		}
		writeJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
			"ok": false, "message": "数据库预处理失败",
		})
		return
	}
	defer stmt.Close()

	for i := range req.Samples {
		smp := &req.Samples[i]
		ts := now
		if smp.TS != nil {
			ts = *smp.TS
		}
		if _, err := stmt.Exec(ts, target, smp.RttMs, smp.MinRtt, smp.Mdev, smp.Sent, smp.Lost); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("回滚上报事务失败: %v", rbErr)
			}
			writeJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
				"ok": false, "message": "写入延迟记录失败",
			})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
			"ok": false, "message": "提交上报事务失败",
		})
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true, "accepted": len(req.Samples)})
}

// validateLatencySample 校验单条上报样本，返回空字符串表示合法，否则为错误描述。
func validateLatencySample(smp *latencySample, now int64) string {
	// ts 若提供须落在 [now-24h, now+5min]，防时钟错乱污染图表
	if smp.TS != nil {
		if *smp.TS < now-24*3600 || *smp.TS > now+300 {
			return "ts 超出允许时间窗（now-24h ~ now+5min）"
		}
	}
	// RTT 类字段：null 或 [0, 60000) 毫秒
	for _, v := range []*float64{smp.RttMs, smp.MinRtt, smp.Mdev} {
		if v != nil && (*v < 0 || *v >= 60000) {
			return "rtt/min_rtt/mdev 须为 null 或 [0,60000) 毫秒"
		}
	}
	if smp.Sent < 1 || smp.Sent > 1000 {
		return "sent 须在 1-1000 之间"
	}
	if smp.Lost < 0 || smp.Lost > smp.Sent {
		return "lost 须在 0-sent 之间"
	}
	return ""
}

// handlePortTraffic 端口流量统计
func (s *Server) handlePortTraffic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tz := s.cfg.Timezone
	now := time.Now().In(tz)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, tz)
	todayEnd := todayStart.Add(24*time.Hour - time.Second)

	// 计算计费周期
	billingStart, _ := s.cfg.GetBillingCycleDates(now)
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
	iptablesOK := s.cachedIptablesOK(portNums)

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
		if err := row.Scan(&todayTx, &todayRx); err != nil && err != sql.ErrNoRows {
			log.Printf("扫描端口今日流量失败: %v", err)
		}
		portData["today"] = map[string]int64{"tx": todayTx, "rx": todayRx, "total": todayTx + todayRx}

		// 昨日流量
		row = s.db.QueryRow(`
			SELECT COALESCE(tx_bytes, 0), COALESCE(rx_bytes, 0)
			FROM port_traffic_daily
			WHERE port = ? AND date = ?
		`, p.Port, yesterday)
		var yesterdayTx, yesterdayRx int64
		if err := row.Scan(&yesterdayTx, &yesterdayRx); err != nil && err != sql.ErrNoRows {
			log.Printf("扫描端口昨日流量失败: %v", err)
		}
		portData["yesterday"] = map[string]int64{"tx": yesterdayTx, "rx": yesterdayRx, "total": yesterdayTx + yesterdayRx}

		// 本月流量（从日表查询，排除今日避免重复）
		row = s.db.QueryRow(`
			SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
			FROM port_traffic_daily
			WHERE port = ? AND date >= ? AND date < ?
		`, p.Port, billingStart.Format("2006-01-02"), today)
		var monthTx, monthRx int64
		if err := row.Scan(&monthTx, &monthRx); err != nil && err != sql.ErrNoRows {
			log.Printf("扫描端口本月流量失败: %v", err)
		}
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
		if err := row.Scan(&lastMonthTx, &lastMonthRx); err != nil && err != sql.ErrNoRows {
			log.Printf("扫描端口上月流量失败: %v", err)
		}
		portData["last_month"] = map[string]int64{"tx": lastMonthTx, "rx": lastMonthRx, "total": lastMonthTx + lastMonthRx}

		result["ports"] = append(result["ports"].([]map[string]interface{}), portData)
	}

	writeJSON(w, result)
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
			"telegram_enabled": s.cfg.TelegramBotToken != "" && s.cfg.TelegramChatID != "",
			"daily_report": map[string]interface{}{
				"enabled": s.cfg.DailyReportEnabled,
				"hour":    s.cfg.DailyReportHour,
			},
		}
		writeJSON(w, cfg)
		return
	}

	// POST 更新配置 (TODO: 持久化到数据库)
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}

// handleNotifyTest 发送一条测试通知，用于在页面上验证 Telegram 是否配置成功
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	s.notifySend(w, r, func() error { return s.notifier.SendTest() }, "测试消息已发送，请检查 Telegram")
}

// handleNotifyDailyReport 手动触发一次每日报告，便于在页面预览真实格式与内容。
// 不影响自动调度：日报调度是纯内存定时器，本次发送不写「已发送」标记。
func (s *Server) handleNotifyDailyReport(w http.ResponseWriter, r *http.Request) {
	// SendDailyReport 在未配置时静默返回 nil（调度器不想报错刷屏），此处手动触发需
	// 如实回显「未配置」，否则会假报「已发送」。
	send := func() error { return s.notifier.SendDailyReport() }
	if s.cfg.TelegramBotToken == "" || s.cfg.TelegramChatID == "" {
		send = func() error {
			return fmt.Errorf("未配置 Telegram（TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID）")
		}
	}
	s.notifySend(w, r, send, "日报已发送，请检查 Telegram")
}

// notifySend 统一处理页面手动触发发送：校验方法/通知器，执行发送并回显结果。
// send 用闭包延迟到通知器非空校验之后再取方法值，避免 nil 接口取方法值 panic。
func (s *Server) notifySend(w http.ResponseWriter, r *http.Request, send func() error, okMsg string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.notifier == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{
			"ok": false, "message": "通知器未初始化",
		})
		return
	}

	if err := send(); err != nil {
		// 未配置或 Telegram API 报错都回显具体原因，便于排查
		writeJSONStatus(w, http.StatusBadGateway, map[string]interface{}{
			"ok": false, "message": err.Error(),
		})
		return
	}

	writeJSON(w, map[string]interface{}{"ok": true, "message": okMsg})
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
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}

	data, err := fs.ReadFile(web.Assets, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := w.Write(data); err != nil {
		log.Printf("写出静态资源 %s 失败: %v", path, err)
	}
}

// cachedIptablesOK 返回带缓存的 iptables 规则检测结果（TTL 60 秒），
// 规则状态极少变化，避免每个请求都 fork iptables 进程
func (s *Server) cachedIptablesOK(ports []int) bool {
	s.iptablesMu.Lock()
	defer s.iptablesMu.Unlock()
	if time.Since(s.iptablesChecked) < 60*time.Second {
		return s.iptablesOK
	}
	s.iptablesOK = s.checkIptablesRules(ports)
	s.iptablesChecked = time.Now()
	return s.iptablesOK
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
