# 生产化路线图

把 heliox-mon 从「工程底子不错的个人项目」推进到「可持续维护的生产级项目」的分阶段计划。

- **阶段 1（已完成）**：质量安全网 —— 单元测试、CI 门禁、golangci-lint、错误处理、LICENSE/CHANGELOG。
- 下面是**剩余的阶段 2 / 3 / 4 与零散项**，可按顺序逐项推进；每项独立、可单独提交、可单独回退。

图例：工作量 `S`(≤30min) / `M`(0.5–2h) / `L`(半天+)；风险 `低/中/高`。
每项「验收标准」即「做完的判定条件」。

---

## 阶段 2 — 后端生产硬化

目标：抵御常见攻击面、可被探活、日志可观测。都是低风险增量改动。

### 2.1 HTTP Server 超时 · `S` · 风险低
- [ ] 给 `http.Server` 设置 `ReadHeaderTimeout`、`ReadTimeout`、`IdleTimeout`。
- **为什么**：当前未设任何超时，存在 Slowloris 慢速连接耗尽风险。
- **怎么做**：在 `internal/api/router.go` 的 `NewServer` 里给 `&http.Server{...}` 补字段。
  `ReadHeaderTimeout: 10s`、`ReadTimeout: 15s`、`IdleTimeout: 60s`。
  **注意 `WriteTimeout`**：`/api/traffic/realtime` 是 SSE 长连接，全局 `WriteTimeout` 会把它掐断；
  要么不设 `WriteTimeout`，要么用 `http.ResponseController` 对 SSE 单独豁免。
- **验收标准**：构建通过；手动用慢速 header 测试连接会被超时关闭；SSE 实时网速仍能持续推送不断流。

### 2.2 安全响应头中间件 · `M` · 风险中
- [ ] 增加统一中间件注入安全头：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、
      `Referrer-Policy: no-referrer`、`Content-Security-Policy`。
- **为什么**：当前响应无任何安全头，易受点击劫持/MIME 嗅探。
- **怎么做**：包一层 middleware 套在 mux 外。**CSP 是难点**：
  - `index.html` 现在脚本已本地化（`vendor/*`），可用 `script-src 'self'`；
  - 但 `login.html` 有内联 `<script>`/`<style>` 和外部 Turnstile（`challenges.cloudflare.com`），
    需要 `script-src 'self' https://challenges.cloudflare.com`，内联部分要么加 `'unsafe-inline'`（弱化效果）
    要么改为外链文件 + nonce。建议先上一个**宽松但有效**的 CSP，后续再收紧。
- **验收标准**：浏览器 Network 面板可见安全头；登录页 Turnstile 仍正常加载、可登录；主面板三张图正常渲染（无 CSP 拦截报错）。
- **风险点**：CSP 配错会让前端白屏，务必本地逐页验证深色/浅色 + 登录流程。

### 2.3 健康检查端点 `/healthz` · `S` · 风险低
- [ ] 新增**无需鉴权**的 `/healthz`，返回 200 + 轻量 JSON（含版本、运行时长）。可选 `/readyz` 校验 DB `Ping()`。
- **为什么**：systemd / 反向代理 / uptime 探活脚本目前无法判活（首页要鉴权会 302）。
- **怎么做**：`mux.HandleFunc("/healthz", ...)`，放在 `auth` 之外。注意别记成访问日志噪音。
- **验收标准**：`curl -i http://127.0.0.1:9100/healthz` 返回 200 且无需 Cookie；DB 异常时 `/readyz` 返回 503。

### 2.4 登录限流 / 防爆破 · `M` · 风险中
- [ ] 对 `/api/login` 增加按 IP 的失败计数 + 退避/锁定（如 5 次失败锁 5 分钟）。
- **为什么**：Turnstile 是**可选**的，未开启时可无限撞密码；常量时间比较只防时序侧信道，不防爆破。
- **怎么做**：内存 `map[ip]{fails, until}` + 互斥锁（与现有 `sessions sync.Map` 风格一致），定期清理。
  取客户端 IP 要兼容反代：解析 `X-Forwarded-For` 首段（仅在信任反代时）。
- **验收标准**：连续多次错误密码后返回 429 且短时间内拒绝该 IP；正确密码在锁定窗口外可正常登录；单测覆盖计数/解锁逻辑。

### 2.5 结构化日志（slog）+ 访问日志 · `M` · 风险中
- [ ] 把标准库 `log` 迁移到 `log/slog`，带级别；新增访问日志中间件（方法/路径/状态码/耗时/请求 ID）。
- **为什么**：当前日志无级别、无结构、无访问记录，线上排障困难。
- **怎么做**：全局 `slog.Logger`（生产 JSON handler、本地 text）。中间件用 `responseWriter` 包装捕获状态码；
  生成请求 ID 注入 `context` 并回写响应头。`/healthz`、SSE 可降级或采样以免刷屏。
- **验收标准**：日志含级别与结构化字段；每个 HTTP 请求一条访问日志；现有中文日志语义保留；测试与 vet 通过。
- **关联**：与 2.3 / 3.2 协同（访问日志可作为可观测性基础）。

### 2.6（可选）密码哈希支持 · `M` · 风险中
- [ ] 支持以 bcrypt 哈希形式配置密码（如 `HELIOX_MON_PASS_HASH`），与明文 `HELIOX_MON_PASS` 二选一。
- **为什么**：当前密码以明文存于环境变量并明文比较。单用户场景可接受，但生产更稳妥。
- **验收标准**：配置哈希后能正常登录；明文与哈希两种方式互斥且都有测试。

---

## 阶段 3 — 可观测性

> 一个「监控工具」自身却不可被监控，略讽刺。补上自身指标。

### 3.1 Prometheus `/metrics` · `M` · 风险低
- [ ] 暴露 `/metrics`（建议置于配置开关后，默认关闭或绑定内网）。指标含：HTTP 请求计数/延迟、采集器运行状态、DB 错误计数、当前会话数等。
- **为什么**：可接入 Prometheus/Grafana 做自身健康与趋势告警。
- **怎么做**：引入 `prometheus/client_golang`；注意单二进制体积增量；鉴权策略（内网 only 或复用 auth）。
- **验收标准**：`/metrics` 返回标准格式；关键路径有计数器/直方图；开关可关闭。

### 3.2 访问日志落地与采样 · `S` · 风险低
- [ ] 若 2.5 已做访问日志，这里补采样/轮转策略（避免 SSE/健康检查刷屏、避免磁盘打满）。
- **验收标准**：高频端点日志被采样；日志量可控。

---

## 阶段 4 — 前端工程化 + 结构重构

> 放最后：前面的 lint/format/类型检查到位后，重构才有安全网。

### 4.1 前端工具链入仓 · `M` · 风险低
- [ ] 新增 `package.json` + Prettier + ESLint 配置；CI 增加前端 lint/format 检查。
- **为什么**：当前前端无任何格式/质量约束，风格全靠手动保持。
- **验收标准**：`npx prettier --check` / `eslint` 在 CI 跑且通过；本地有 `npm run lint`。

### 4.2 轻量类型检查（JSDoc + tsc checkJs）· `M` · 风险低
- [ ] 给 `app.js` 关键函数加 JSDoc 类型注释，用 `tsc --checkJs --noEmit` 在 CI 做类型校验（不引入 TS 构建）。
- **为什么**：2000 行纯 JS 无类型，重命名/重构易出隐性错误。
- **验收标准**：`tsc --checkJs` 在 CI 通过；关键数据结构（API 返回、图表配置）有类型标注。

### 4.3 app.js 模块拆分 · `L` · 风险中
- [ ] 把 `web/app.js`（约 2000 行）按职责拆为 `core.js` / `realtime.js` / `latency.js` / `trend.js`，
      `index.html` 按顺序多 `<script>` 引入（全局作用域不变，无需打包器），更新 `embed.go`。
- **为什么**：单文件 + 十几个全局变量，维护性差（这也是之前不敢直接拆的原因）。
- **前置条件**：4.1 / 4.2 完成后再做——有 format/lint/类型检查兜底，拆分才安全。
- **怎么做**：纯剪切粘贴、保持函数与全局变量名不变；逐文件验证；**全程人肉点一遍**深浅主题、实时网速、流量、趋势、延迟五个区块。
- **验收标准**：页面功能与拆分前完全一致；lint/类型检查通过；二进制重建后生效。
- **风险点**：剪切易漏函数/重复定义；建议拆完用浏览器逐区块对照验证。

### 4.4（可选）自有静态资源压缩 · `S` · 风险低
- [ ] 构建时压缩自有 `app.js` / `style.css`（第三方 `vendor/*` 已是 min 版）。
- **验收标准**：产物体积下降且功能不变；不破坏 `go:embed` 流程。

---

## 零散项 / 跨阶段

### 项目卫生
- [ ] `CONTRIBUTING.md`（如何构建/测试/提交规范）· `S`
- [ ] `SECURITY.md`（漏洞披露渠道）· `S`
- [ ] `.github/dependabot.yml`（自动依赖更新 PR）· `S` · 风险低
- [ ] GitHub Issue / PR 模板 · `S`

### 工程细节
- [ ] **Makefile `fmt` 修复**：`goimports` 未安装导致 `make fmt` 报错。
      改为 `go run golang.org/x/tools/cmd/goimports@latest -w .` 或加入 `deps` 安装。· `S` · 风险低
- [ ] **golangci-lint 本机假阳性**：darwin 上 `collector.go` 4 个字段被误报 `unused`（只在 `_linux.go` 用）。
      可在文档注明本地用 `GOOS=linux golangci-lint run`，或将这些字段下沉到 `_linux.go` 专用结构以消除误报。· `S`
- [ ] **CI 加 `-race`**：竞态检测对并发采集器价值高，但需 `CGO_ENABLED=1`。可单独加一个 race 测试 job。· `S`
- [ ] **会话持久化（可选）**：当前 session 存内存，重启即全部登出。如需重启不掉线，可落 SQLite。· `M`
- [ ] **Schema 版本管理（可选）**：当前迁移是幂等 `CREATE TABLE IF NOT EXISTS`，无版本表/降级。
      规模变大后可引入 `schema_version` 跟踪。· `M`

---

## 建议推进顺序

1. **阶段 2.1 / 2.3**（超时 + healthz）：最快、最低风险，立竿见影。
2. **阶段 2.4 / 2.5**（限流 + slog）：安全与可观测性核心。
3. **零散项里的项目卫生 + Makefile 修复**：顺手清理。
4. **阶段 2.2**（CSP）：单独做，需逐页验证。
5. **阶段 3**（metrics）：按需。
6. **阶段 4**（前端工程化 → 拆分）：最后，按 4.1 → 4.2 → 4.3 顺序。

> 每完成一项：跑 `go test ./...` + `GOOS=linux golangci-lint run`（CI 视角），绿了再按
> conventional commit 中文提交，累积成一个完整单元提交一次。
