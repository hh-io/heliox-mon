// Package api HTTP API 服务
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hh/heliox-mon/internal/collector"
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
	systemProvider   SystemDataProvider
	notifier         Notifier
	sessionSecret    []byte // 会话 token 的 HMAC 签名密钥，持久化在库里（见 session.go）
	loginPage        []byte // 登录页按配置渲染一次（Turnstile 有无），避免每个未授权请求都重建

	// iptables 规则检测结果缓存（避免每个请求都 fork iptables）
	iptablesMu      sync.Mutex
	iptablesOK      bool
	iptablesChecked time.Time
	iptablesTTL     time.Duration // 本次结论的有效期，见 cachedIptablesOK
}

// turnstileClient 校验登录人机验证。默认 http.Client 没有整体超时，
// Cloudflare 一侧不响应时登录请求会一直挂着
var turnstileClient = &http.Client{Timeout: 10 * time.Second}

// iptablesCmdTimeout 是单条规则检测命令的上限（含 -w 5 的等锁时间）。检测时持有
// iptablesMu，命令挂住会让所有端口流量请求排队卡死
const iptablesCmdTimeout = 10 * time.Second

// RealtimeDataProvider 实时数据提供者接口
type RealtimeDataProvider interface {
	GetRealtimeSpeed() (txSpeed, rxSpeed float64, ts int64)
}

// SystemDataProvider 系统资源提供者接口（数据来自采集器内存，不落库）
type SystemDataProvider interface {
	GetSystemSnapshot() collector.SystemSnapshot
}

// Notifier 通知发送接口（用于页面「测试发送」/「日报预览」手动触发）
type Notifier interface {
	SendTest() error
	SendDailyReport() error
}

// NewServer 创建服务器。realtimeProvider 与 systemProvider 实际都由采集器实现，
// 分成两个接口是为了让各 handler 的依赖显式可见。
func NewServer(cfg *config.Config, db *storage.DB, realtimeProvider RealtimeDataProvider, systemProvider SystemDataProvider, notifier Notifier) (*Server, error) {
	secret, err := loadSessionSecret(db)
	if err != nil {
		return nil, fmt.Errorf("初始化会话密钥失败: %w", err)
	}

	s := &Server{
		cfg:              cfg,
		db:               db,
		realtimeProvider: realtimeProvider,
		systemProvider:   systemProvider,
		notifier:         notifier,
		sessionSecret:    secret,
		loginPage:        buildLoginPage(cfg),
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
	mux.HandleFunc("/api/latency", s.auth(gzipJSON(s.handleLatency)))
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

	return s, nil
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
	if err := s.server.Shutdown(ctx); err != nil {
		log.Printf("HTTP 服务关闭异常: %v", err)
	}
}

// writeJSON 写出 JSON 响应；编码失败仅记录日志
// （响应体已开始写出，无法再修改状态码）
// gzipJSON 压缩响应体。延迟接口在 24h × 1 分钟粒度下每个桶都带逐包样本，
// 单目标未压缩约 500KB，而前端每 60 秒全量重拉一次——走 Tunnel 或移动网络时
// 这是持续的 MB 级流量。样本是数值文本，压缩比很高。
// 注意：SSE（/api/traffic/realtime）不能套，压缩流会被缓冲，实时推送变成一次性吐出。
func gzipJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer func() {
			if err := gz.Close(); err != nil {
				log.Printf("关闭 gzip 响应失败: %v", err)
			}
		}()
		next(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	}
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) { return w.gz.Write(b) }

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

// assetETags 内嵌静态资源的内容哈希（含首尾引号，符合 ETag 语法）。
// 资源在编译期就固定了，启动时算一次即可；有强校验器后浏览器的重验证才能命中 304。
var assetETags = buildAssetETags()

func buildAssetETags() map[string]string {
	m := make(map[string]string)
	err := fs.WalkDir(web.Assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(web.Assets, p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		m[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		// 拿不到 ETag 只是退化成每次重传，不影响正确性，因此不阻断启动
		log.Printf("计算静态资源 ETag 失败: %v", err)
	}
	return m
}

// publicAssets 无需认证即可访问的静态资源。
// 登录页要用到样式与图标；manifest.json 由 <link rel="manifest"> 默认不带凭据地拉取，
// sw.js 与图标则可能在未持有会话时被浏览器请求，都必须放行。
var publicAssets = map[string]bool{
	"/style.css":             true,
	"/favicon.svg":           true,
	"/manifest.json":         true,
	"/sw.js":                 true,
	"/icon-192.png":          true,
	"/icon-512.png":          true,
	"/icon-512-maskable.png": true,
}

// isLoopbackRequest 判断请求是否来自本机。只取 RemoteAddr（TCP 对端），
// 不看 X-Forwarded-For —— 那是客户端可伪造的头，用它判断会让免登录形同虚设。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// auth 认证中间件 (Cookie + Basic Fallback)
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 公开资源 (CSS/JS/Favicon)
		// 注意: /login 由单独 handler 处理，实际上不会经过这里(除非 mux 匹配逻辑特殊)，
		// 但为了保险起见，style.css 等静态资源如果是通过 "/" handleStatic 服务的，
		// 必须在这里放行。
		if publicAssets[r.URL.Path] {
			next(w, r)
			return
		}

		// 1.5 本地开发免登录。cfg.DevNoAuth 只可能在 darwin 构建里为 true（见 config/devauth_darwin.go），
		// 这里再叠一道回环地址检查：即使本机开着调试实例，同网段的其他人也进不来。
		if s.cfg.DevNoAuth && isLoopbackRequest(r) {
			next(w, r)
			return
		}

		// 2. Cookie 验证
		if cookie, err := r.Cookie(authCookieName); err == nil {
			if exp, ok := s.validateToken(cookie.Value); ok {
				// 滑动续期：临近过期就重签，持续使用的浏览器不会在第 30 天被打回登录页。
				// 必须赶在 next 写出响应体之前设置 header，SSE 长连接同理。
				if time.Until(time.Unix(exp, 0)) < sessionRenewBefore {
					setSessionCookie(w, r, s.issueToken(time.Now()))
				}
				next(w, r)
				return
			}
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

// buildLoginPage 把 login.html 里的 Turnstile 占位符按配置替换掉。
// 没配 HELIOX_TURNSTILE_SECRET（本地开发的常态）时两处都替换成空串：后端本来就不校验 token，
// 再去加载 challenges.cloudflare.com 只会在离线/内网环境留下一个"连接失败"的空框。
func buildLoginPage(cfg *config.Config) []byte {
	data, err := fs.ReadFile(web.Assets, "login.html")
	if err != nil {
		log.Printf("读取登录页失败: %v", err)
		return nil
	}

	script, widget := "", ""
	if cfg.TurnstileSecretKey != "" {
		script = `<script src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>`
		widget = `<div class="cf-turnstile" data-sitekey="` +
			html.EscapeString(cfg.TurnstileSiteKey) +
			`" data-theme="auto" style="margin: 0 auto"></div>`
	}
	data = bytes.ReplaceAll(data, []byte("<!--HELIOX_TURNSTILE_SCRIPT-->"), []byte(script))
	data = bytes.ReplaceAll(data, []byte("<!--HELIOX_TURNSTILE_WIDGET-->"), []byte(widget))
	return data
}

// handleLoginView 登录页面
func (s *Server) handleLoginView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 如果已登录，跳转首页
	if cookie, err := r.Cookie(authCookieName); err == nil {
		if _, ok := s.validateToken(cookie.Value); ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	if s.loginPage == nil {
		http.Error(w, "Login page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(s.loginPage); err != nil {
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

	setSessionCookie(w, r, s.issueToken(time.Now()))

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

	resp, err := turnstileClient.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify", formData)
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

	// 本计费周期流量（根据 ResetDay 计算，非自然月；字段名 this_month 是历史遗留）
	row = s.db.QueryRow(
		"SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0) FROM traffic_daily WHERE date >= ? AND iface = 'total'",
		billingStart.Format("2006-01-02"),
	)
	var monthTx, monthRx int64
	if err := row.Scan(&monthTx, &monthRx); err != nil && err != sql.ErrNoRows {
		log.Printf("查询计费周期流量失败: %v", err)
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

	snap := s.systemProvider.GetSystemSnapshot()
	if snap.Ts == 0 {
		http.Error(w, "No data", http.StatusNotFound)
		return
	}

	writeJSON(w, systemSnapshotJSON(snap))
}

// systemSnapshotJSON 将系统快照转成前端使用的字段名
func systemSnapshotJSON(snap collector.SystemSnapshot) map[string]interface{} {
	data := map[string]interface{}{
		"ts":                snap.Ts,
		"steal_percent":     snap.StealPercent,
		"steal_avg_percent": snap.StealAvgPercent,
		"cpu_cores":         snap.CPUCores,
		"mem_used":          snap.MemUsed,
		"mem_total":         snap.MemTotal,
		"disk_used":         snap.DiskUsed,
		"disk_avail":        snap.DiskAvail,
		"disk_total":        snap.DiskTotal,
		"load_1":            snap.Load1,
		"load_5":            snap.Load5,
		"load_15":           snap.Load15,
		"uptime_sec":        snap.UptimeSec,
	}

	// PortConns 为 nil 说明这轮没读到连接数，输出 null 让前端显示占位符——
	// 否则 0 会被当成「没人连接」，而那是完全不同的一件事
	if snap.PortConns == nil {
		data["conns_total"] = nil
		data["conns_by_port"] = nil
	} else {
		// 端口连接数按端口号升序输出，避免 map 顺序随机导致前端卡片跳来跳去
		ports := make([]int, 0, len(snap.PortConns))
		for port := range snap.PortConns {
			ports = append(ports, port)
		}
		sort.Ints(ports)

		conns := make([]map[string]interface{}, 0, len(ports))
		totalConns := 0
		for _, port := range ports {
			conns = append(conns, map[string]interface{}{
				"port":  port,
				"count": snap.PortConns[port],
			})
			totalConns += snap.PortConns[port]
		}
		data["conns_total"] = totalConns
		data["conns_by_port"] = conns
	}

	// 与 cpu_percent 同理：没有基准时不伪造成 0%
	if snap.RetransValid {
		data["retrans_percent"] = snap.RetransPercent
	} else {
		data["retrans_percent"] = nil
	}

	// 首次采样还没有差值可算，返回 null 让前端显示占位符，
	// 而不是把「暂时不知道」谎报成 0%
	if snap.CPUValid {
		data["cpu_percent"] = snap.CPUPercent
	} else {
		data["cpu_percent"] = nil
	}

	return data
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

	// 系统快照 5 秒才更新一次，但这里按快照 Ts 变化推送而非用独立的 5 秒 ticker：
	// 独立 ticker 与采集周期不同相位，页面拿到的数据会平白多陈旧最多 5 秒
	var lastSystemTs int64

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
			// 系统快照有更新就顺带推一帧，采集完最多 1 秒内送达
			if !s.writeSystemEvent(w, flusher, &lastSystemTs) {
				return
			}

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

// writeSystemEvent 系统快照相对 lastTs 有更新时推送一帧具名事件，
// 与默认的网速消息互不干扰。返回 false 表示连接已断开
func (s *Server) writeSystemEvent(w http.ResponseWriter, flusher http.Flusher, lastTs *int64) bool {
	snap := s.systemProvider.GetSystemSnapshot()
	if snap.Ts == 0 || snap.Ts == *lastTs {
		return true
	}

	jsonData, err := json.Marshal(systemSnapshotJSON(snap))
	if err != nil {
		log.Printf("序列化系统快照失败: %v", err)
		return true
	}

	*lastTs = snap.Ts
	if _, err := w.Write([]byte("event: system\ndata: " + string(jsonData) + "\n\n")); err != nil {
		return false
	}
	flusher.Flush()
	return true
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

	targetsData := make([]map[string]interface{}, 0, len(targets))
	for _, pt := range targets {
		pts, window, err := collector.QueryLatencyPoints(s.db, pt.IP, startTs, endTs, granularitySec)
		if err != nil {
			log.Printf("查询延迟数据失败 [%s]: %v", pt.Tag, err)
			continue
		}
		points := make([]map[string]interface{}, 0, len(pts))
		for _, p := range pts {
			points = append(points, latencyPointJSON(p))
		}
		targetsData = append(targetsData, map[string]interface{}{
			"tag":    pt.Tag,
			"ip":     pt.IP,
			"points": points,
			"stats":  latencyWindowJSON(window, len(pts)),
		})
	}
	result["targets"] = targetsData

	writeJSON(w, result)
}

func latencyPointJSON(p collector.LatencyPoint) map[string]interface{} {
	loss := 0.0
	if p.Sent > 0 {
		loss = float64(p.Lost) / float64(p.Sent) * 100
	}
	m := map[string]interface{}{
		"ts":      p.TS,
		"rtt_ms":  floatOrNil(p.Avg),
		"min_rtt": floatOrNil(p.Min),
		"max_rtt": floatOrNil(p.Max),
		"p95":     floatOrNil(p.P95),
		"jitter":  floatOrNil(p.Jitter),
		"loss":    loss,
		"sent":    p.Sent,
		"lost":    p.Lost,
		"recv":    p.Recv,
		"sum_rtt": p.SumRTT,
		"sum_sq":  p.SumSq,
	}
	if len(p.RTTs) > 0 {
		m["rtts"] = p.RTTs
	}
	return m
}

func latencyWindowJSON(s collector.LatencySummary, buckets int) map[string]interface{} {
	loss := 0.0
	if s.Sent > 0 {
		loss = float64(s.Lost) / float64(s.Sent) * 100
	}
	count := buckets
	if s.Avg == nil {
		count = 0
	}
	return map[string]interface{}{
		"avg":    floatOrNil(s.Avg),
		"min":    floatOrNil(s.Min),
		"max":    floatOrNil(s.Max),
		"p95":    floatOrNil(s.P95),
		"jitter": floatOrNil(s.Mdev),
		"count":  count,
		"loss":   loss,
		"recv":   s.Recv,
	}
}

func floatOrNil(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
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
// 单个 RTT 的上界（毫秒）。超过 1 分钟的「延迟」只可能是上报方算错了单位
const maxRttMs = 60000

type latencySample struct {
	TS     *int64    `json:"ts"`
	RttMs  *float64  `json:"rtt_ms"`
	MinRtt *float64  `json:"min_rtt"`
	MaxRtt *float64  `json:"max_rtt"`
	P95    *float64  `json:"p95_rtt"`
	Mdev   *float64  `json:"mdev"`
	Sent   int       `json:"sent"`
	Lost   int       `json:"lost"`
	Recv   int       `json:"recv"`
	SumRTT float64   `json:"sum_rtt"`
	SumSq  float64   `json:"sum_sq"`
	RTTs   []float64 `json:"rtts"`
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
	for i := range req.Samples {
		smp := &req.Samples[i]
		ts := now
		if smp.TS != nil {
			ts = *smp.TS
		}
		if err := collector.WriteLatency(tx, ts, target, summaryFromReport(smp)); err != nil {
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
	// RTT 类字段：null 或 [0, maxRttMs) 毫秒
	for _, v := range []*float64{smp.RttMs, smp.MinRtt, smp.MaxRtt, smp.P95, smp.Mdev} {
		if v != nil && (*v < 0 || *v >= maxRttMs) {
			return "rtt/min_rtt/max_rtt/p95_rtt/mdev 须为 null 或 [0,60000) 毫秒"
		}
	}
	if smp.Sent < 1 || smp.Sent > 1000 {
		return "sent 须在 1-1000 之间"
	}
	if smp.Lost < 0 || smp.Lost > smp.Sent {
		return "lost 须在 0-sent 之间"
	}
	if len(smp.RTTs) > 1000 {
		return "rtts 数量不能超过 1000"
	}
	for _, v := range smp.RTTs {
		if v < 0 || v >= maxRttMs {
			return "rtts 元素须在 [0,60000) 毫秒"
		}
	}
	if len(smp.RTTs)+smp.Lost > smp.Sent {
		return "rtts+lost 不能大于 sent"
	}
	if smp.Recv < 0 || smp.Recv > smp.Sent-smp.Lost {
		return "recv 须在 0-(sent-lost) 之间"
	}
	// 没有 rtts 时 sum_rtt/sum_sq 被原样当聚合权重写库，之后所有窗口均值都按它算，
	// 且 7 天后会被降采样固化进聚合行。不设上界的话一条上报就能永久污染该目标
	if smp.SumRTT < 0 || smp.SumSq < 0 {
		return "sum_rtt/sum_sq 不能为负"
	}
	if smp.Recv == 0 && (smp.SumRTT > 0 || smp.SumSq > 0) {
		return "recv 为 0 时 sum_rtt/sum_sq 须为 0"
	}
	// 反向同样要堵：recv>0 却一个 RTT 都没给，落库就是 recv=N、sum_rtt=0 的桶。
	// 它既把整个窗口从逐包精确路径拖回回退路径（P95 变「约」），又给加权均值塞进
	// 一个分子为 0、分母为 N 的桶，把该目标的均值往 0 拉
	if smp.Recv > 0 && len(smp.RTTs) == 0 && smp.SumRTT <= 0 && smp.RttMs == nil {
		return "recv 大于 0 时须提供 rtts、sum_rtt 或 rtt_ms 之一"
	}
	if smp.SumRTT > float64(smp.Recv)*maxRttMs {
		return "sum_rtt 不能超过 recv × 60000 毫秒"
	}
	if smp.SumSq > float64(smp.Recv)*maxRttMs*maxRttMs {
		return "sum_sq 不能超过 recv × 60000² 毫秒²"
	}
	return ""
}

func summaryFromReport(smp *latencySample) collector.LatencySummary {
	if len(smp.RTTs) > 0 {
		return collector.SummarizeSamples(smp.RTTs, smp.Sent, smp.Lost)
	}
	s := collector.LatencySummary{
		Avg:    smp.RttMs,
		Min:    smp.MinRtt,
		Max:    smp.MaxRtt,
		P95:    smp.P95,
		Mdev:   smp.Mdev,
		Sent:   smp.Sent,
		Lost:   smp.Lost,
		Recv:   smp.Recv,
		SumRTT: smp.SumRTT,
		SumSq:  smp.SumSq,
	}
	s.Normalize()
	return s
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
	// 已停用协议不再列出（历史月度汇总仍按端口号查询，不受影响）
	if s.cfg.SnellEnabled && s.cfg.SnellPort > 0 {
		ports = append(ports, struct {
			Port int    `json:"port"`
			Name string `json:"name"`
		}{Port: s.cfg.SnellPort, Name: "Snell"})
	}
	if s.cfg.XrayEnabled && s.cfg.VlessPort > 0 {
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
		"iptables_ok": iptablesOK,
	}

	portsData := make([]map[string]interface{}, 0, len(ports))
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

		// 本计费周期流量（从日表查询，排除今日避免重复）
		row = s.db.QueryRow(`
			SELECT COALESCE(SUM(tx_bytes), 0), COALESCE(SUM(rx_bytes), 0)
			FROM port_traffic_daily
			WHERE port = ? AND date >= ? AND date < ?
		`, p.Port, billingStart.Format("2006-01-02"), today)
		var monthTx, monthRx int64
		if err := row.Scan(&monthTx, &monthRx); err != nil && err != sql.ErrNoRows {
			log.Printf("扫描端口计费周期流量失败: %v", err)
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

		portsData = append(portsData, portData)
	}
	result["ports"] = portsData

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
		w.Header().Set("Cache-Control", "max-age=86400")
	case strings.HasSuffix(path, ".png"):
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
	case strings.HasSuffix(path, ".json"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	}

	data, err := fs.ReadFile(web.Assets, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if etag, ok := assetETags[path]; ok {
		w.Header().Set("ETag", etag)
	}
	// 交给 ServeContent 处理 If-None-Match：命中时回 304，
	// 否则 no-cache 的资源（含 vendor 下 ~1.3MB 图表库）每次页面加载都要整包重传
	http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
}

// cachedIptablesOK 返回带缓存的 iptables 规则检测结果，避免每个请求都 fork iptables 进程。
// 规则状态极少变化，确定的结论缓存 60 秒；命令本身失败（等锁超时、被 kill 等）只说明
// 「这次没看清」，只缓存 10 秒——否则一次撞锁就让页面连续一分钟误报规则缺失
func (s *Server) cachedIptablesOK(ports []int) bool {
	s.iptablesMu.Lock()
	defer s.iptablesMu.Unlock()
	if time.Since(s.iptablesChecked) < s.iptablesTTL {
		return s.iptablesOK
	}
	ok, err := s.checkIptablesRules(ports)
	s.iptablesTTL = 60 * time.Second
	if err != nil {
		log.Printf("端口统计规则检测失败: %v", err)
		s.iptablesTTL = 10 * time.Second
	}
	s.iptablesOK = ok
	s.iptablesChecked = time.Now()
	return ok
}

// runIptablesList 执行 <bin> -w 5 -S。-w 5 等 xtables 锁最多 5 秒：不等会把偶发的锁竞争
// 误报成规则缺失，不带秒数的 -w 则是无限期等待。每条命令单独计时，v4 等锁耗掉的时间
// 不会挤占 v6 的额度
func runIptablesList(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), iptablesCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-w", "5", "-S").Output()
	if err != nil {
		return "", fmt.Errorf("%s -S 执行失败: %w", bin, err)
	}
	return string(out), nil
}

// checkIptablesRules 检测端口统计规则是否完整。IPv4 必须有；IPv6 只在本机具备
// IPv6 协议栈时才要求——xray 监听 "::"，缺了 v6 规则会让 IPv6 客户端的流量悄悄漏统计。
// err 非空表示命令没跑成，结论不可靠（此时 ok 为 false）
func (s *Server) checkIptablesRules(ports []int) (ok bool, err error) {
	if len(ports) == 0 {
		return true, nil
	}

	v4, err := runIptablesList("iptables")
	if err != nil {
		return false, err
	}
	if !statsRulesComplete(v4, ports) {
		return false, nil
	}

	// 内核没有 IPv6 协议栈时不会有 IPv6 流量，无需 v6 规则。用 /proc 判定而非
	// 「ip6tables 执行失败就放行」：后者会把权限、锁等临时错误也当成规则正常
	if _, err := os.Stat("/proc/net/if_inet6"); err != nil {
		return true, nil
	}
	v6, err := runIptablesList("ip6tables")
	if err != nil {
		return false, err
	}
	return statsRulesComplete(v6, ports), nil
}

// statsRulesComplete 判断 iptables -S / ip6tables -S 的输出里 HELIOX_STATS 链是否完整：
// INPUT/OUTPUT 跳转都在，且每个端口的 TCP/UDP、sport/dport 四条规则齐全
func statsRulesComplete(output string, ports []int) bool {
	hasInputJump := false
	hasOutputJump := false
	dptTCP := make(map[int]bool)
	sptTCP := make(map[int]bool)
	dptUDP := make(map[int]bool)
	sptUDP := make(map[int]bool)

	for _, line := range strings.Split(output, "\n") {
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
		// 补尾部空格做词边界，避免 --dport 443 把 4430 的规则也算进来
		line += " "
		for _, port := range ports {
			if port <= 0 {
				continue
			}
			if strings.Contains(line, fmt.Sprintf("--dport %d ", port)) {
				if proto == "tcp" {
					dptTCP[port] = true
				} else {
					dptUDP[port] = true
				}
			}
			if strings.Contains(line, fmt.Sprintf("--sport %d ", port)) {
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
