# 更新日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- 每日流量报告新增**网络延迟**小节：按 `PING_TARGETS` 逐目标汇报昨日平均 RTT、
  最低 RTT 与丢包率（口径与「昨日用量」一致，昨日整天）；丢包 >0 标注 ⚠️，
  全丢包/无数据目标显示「无数据」。复用已有探测数据，无需新增配置。

### 变更

- Telegram 通知改用 HTML 富文本（`parse_mode=HTML`）：标题加粗，数据行放等宽
  `<pre>` 块对齐（修复此前纯文本在手机非等宽字体下对不齐的问题），收敛 emoji 为
  一套克制的语义符号并移除装饰性分隔线。服务器名、探测目标名等用户可控字符串统一
  做 HTML 转义。日报 / 流量预警 / 测试三类消息一并改版。

## [0.13.0] - 2026-07-02

### 变更

- 每日流量报告全面改版，提升信息量与可读性：
  - 新增**环比**——昨日合计相对前日的涨跌百分比。
  - 新增**周期进度条**、**日均用量**与**月底预测**（照当前速度推算周期末总量，
    可能超限时标注 ⚠️）。
  - 统一单位为 GiB（原限额标签写 GB 但按 1024³ 计算，实为 GiB），日期附中文星期。
- 每日报告调度增加日志：启动/每轮打印「下次推送时间」（含时区），发送成功也记一条；
  开启开关但未配 Telegram 时启动即告警，便于排查「配了却不发送」。

## [0.12.1] - 2026-07-01

### 变更

- 通知 UI 改版：移除底部「通知」区块，改为顶栏服务器名旁的胶囊（仅在配置了 Telegram
  时显示，每日报告开启时显示推送时刻）。点击胶囊弹出面板查看配置状态并发送测试消息，
  点击外部 / Esc 关闭。

## [0.12.0] - 2026-07-01

### 新增

- 每日流量报告：可选定时通过 Telegram 推送昨日用量 + 本计费周期累计/剩余/重置日。
  由 `DAILY_REPORT_ENABLED` 开关、`DAILY_REPORT_HOUR` 指定推送时刻（按时区整点，默认 9 点），
  定时器对齐到下一次触发，进程重启不重复推送。
- 页面「通知」区块：显示 Telegram 是否已配置、每日报告开关/时刻，并提供「发送测试消息」
  按钮即时验证配置（成功/失败原因回显）。新增 `POST /api/notify/test`，`GET /api/config`
  补充 `daily_report` 字段。

## [0.11.0] - 2026-07-01

### 新增

- 工程化质量门禁：新增 GitHub Actions CI（lint / vet / test / 多平台构建 / govulncheck）。
- `golangci-lint` 配置（errcheck、staticcheck、govet、bodyclose、errorlint 等）。
- 核心逻辑单元测试：计费周期计算、配置与端口解析、虚拟网卡过滤、SQLite 迁移与流量查询。
- 新增 `LICENSE`（MIT）与本 `CHANGELOG`。
- `http.Server` 读取超时（`ReadHeaderTimeout` / `ReadTimeout` / `IdleTimeout`），
  防御 Slowloris 慢速连接；SSE 实时推送不设 `WriteTimeout` 以免长连接被掐断。

### 修复

- 显式处理此前被忽略的错误返回值（`rows.Scan` / `json.Encode` / `w.Write` /
  `db.Exec` / `server.Shutdown` 等），消除静默吞错。
- `make fmt` 在未安装 `goimports` 时报错：改用 `go run` 临时拉取；补全 Makefile `.PHONY` 目标。

### 变更

- 前端图表库（Chart.js / annotation 插件 / ECharts）由 CDN 改为本地 `go:embed` 内嵌，
  消除墙内 CDN 加载失败导致图表空白的隐患。

## [0.10.43] - 2026-07-01

### 变更

- 前端细节优化：主题首屏跟随系统偏好、登录页支持浅色主题、innerHTML 注入转义、
  `formatSpeed` 去重、移除调试日志。

## [0.10.42] - 2026-07-01

### 变更

- 内嵌图表库去除 CDN 依赖并清理死代码（未使用的 CSS、登录页失效的 shake 动画）。

## [0.10.41] - 2026-07-01

### 修复

- 修正 SQLite DSN 参数格式，解决 `database is locked`。

## [0.10.40] - 2026-07-01

### 新增

- 提升延迟与丢包统计准确性，新增最小 RTT 与抖动（mdev）指标。

[Unreleased]: https://github.com/Theo-oh/heliox-mon/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.43...v0.11.0
[0.10.43]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.42...v0.10.43
[0.10.42]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.41...v0.10.42
[0.10.41]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.40...v0.10.41
[0.10.40]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.39...v0.10.40
