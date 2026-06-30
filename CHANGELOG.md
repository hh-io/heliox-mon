# 更新日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- 工程化质量门禁：新增 GitHub Actions CI（lint / vet / test / 多平台构建 / govulncheck）。
- `golangci-lint` 配置（errcheck、staticcheck、govet、bodyclose、errorlint 等）。
- 核心逻辑单元测试：计费周期计算、配置与端口解析、虚拟网卡过滤、SQLite 迁移与流量查询。
- 新增 `LICENSE`（MIT）与本 `CHANGELOG`。

### 修复

- 显式处理此前被忽略的错误返回值（`rows.Scan` / `json.Encode` / `w.Write` /
  `db.Exec` / `server.Shutdown` 等），消除静默吞错。

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

[Unreleased]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.43...HEAD
[0.10.43]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.42...v0.10.43
[0.10.42]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.41...v0.10.42
[0.10.41]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.40...v0.10.41
[0.10.40]: https://github.com/Theo-oh/heliox-mon/compare/v0.10.39...v0.10.40
