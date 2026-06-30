# 优化路线图

heliox-mon 是**单人自用**的轻量监控工具（给自己的 heliox 代理服务看流量/延迟），
不是面向公网多租户的服务。这份清单据此裁剪：只保留对「自用 + 长期可维护」有**实际收益**的项，
明确剔除把它当企业级生产服务规划的过度工程。

图例：工作量 `S`(≤30min) / `M`(0.5–2h) / `L`(半天+)；风险 `低/中/高`。

- **质量安全网（已完成）**：单元测试、CI 门禁、golangci-lint、错误处理、LICENSE/CHANGELOG。

---

## ✅ 值得做

对自用场景投入产出比高、风险低。

### HTTP Server 超时 · `S` · 风险低 ·（已完成 v0.11.0）
- [x] `http.Server` 设 `ReadHeaderTimeout: 10s` / `ReadTimeout: 15s` / `IdleTimeout: 60s`。
- **为什么**：未设超时存在 Slowloris 慢速连接耗尽风险。
- **坑**：`/api/traffic/realtime` 是 SSE 长连接，**不能设全局 `WriteTimeout`**，否则会掐断实时推送。

### Makefile `fmt` 修复 · `S` · 风险低 ·（已完成 v0.11.0）
- [x] `goimports` 未安装导致 `make fmt` 报错：改用 `go run golang.org/x/tools/cmd/goimports@latest -w .`。

---

## 🤔 看情况（取决于实际部署，别为「完整」而做）

### 健康检查端点 `/healthz` · `S` · 风险低
- [ ] 仅当用 systemd / 反代 / uptime 脚本**探活**时才需要：无需鉴权的 `/healthz` 返回 200 + 轻量 JSON。
- **怎么做**：`mux.HandleFunc("/healthz", ...)` 放在 `auth` 之外。
- **不需要的情况**：自己手动看面板，不探活——别加。

### 登录限流 / 防爆破 · `M` · 风险中
- [ ] 仅当**没开 Turnstile 且面板暴露公网**时才需要：按 IP 失败计数 + 退避锁定。
- **为什么**：Turnstile 是可选的，未开启时可无限撞密码。**已配 `TurnstileSecretKey` 则基本不必做**。
- **怎么做**：内存 `map[ip]{fails, until}` + 互斥锁（与现有 `sessions` 风格一致），定期清理。
  取客户端 IP 兼容反代时解析 `X-Forwarded-For` 首段（仅在信任反代时）。

### 安全响应头 · `M` · 风险中
- [ ] 中间件注入 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy`。
- **取舍**：单用户场景点击劫持/嗅探威胁很低，收益有限。
- **CSP 是坑**：`login.html` 有内联 `<script>`/`<style>` 和外部 Turnstile，配错会白屏，需逐页验证
  深浅主题 + 登录流程。**要做就先上宽松但有效的 CSP，别一步到位**。

---

## ❌ 不建议（对单人自用属过度工程）

- **Prometheus `/metrics`**：给一个自用监控工具再接一套 Prometheus/Grafana，典型过度工程。
  除非你本就有 Prometheus 栈。
- **密码 bcrypt 哈希**：密码在你自己的环境变量里、明文比较，无现实威胁。
- **前端工程化全套**（`package.json` / ESLint / Prettier / `tsc --checkJs` / 拆分 `app.js`）：
  投入半天+，收益却最低——**没有第二个人维护这个前端**。2000 行能跑就别动，硬拆还有风险。
- **开源协作设施**（`CONTRIBUTING.md` / `SECURITY.md` / Issue·PR 模板 / dependabot）：
  只有指望外部 PR 的开源项目才有意义，自用是噪音。

---

## 🔧 可顺手清理的工程细节（非必需）

- [ ] **结构化日志（slog）**：真排过障、真需要级别/访问日志再做；自用场景现状中文 `log` 大概率够。· `M`
- [ ] **golangci-lint 本机假阳性**：darwin 上 `collector.go` 4 个字段被误报 `unused`（只在 `_linux.go` 用）。
      已在 `CLAUDE.md` 注明本地用 `GOOS=linux golangci-lint run`；如嫌烦可把字段下沉到 `_linux.go` 专用结构。· `S`
- [ ] **CI 加 `-race`**：竞态检测对并发采集器有价值，但需 `CGO_ENABLED=1`，可单独一个 job。· `S`
- [ ] **会话持久化**：当前 session 存内存，重启即全部登出。介意重启掉线再落 SQLite。· `M`

---

> 每完成一项：`go test ./...` + `GOOS=linux golangci-lint run`（CI 视角），绿了再按
> conventional commit 中文提交。
