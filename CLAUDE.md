# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Heliox Monitor 是为 [heliox](https://github.com/Theo-oh/heliox) 代理服务设计的轻量级服务器监控系统：采集系统资源/流量/端口/延迟，存入 SQLite，通过内嵌 Web 前端展示，单二进制部署。

## 命令

```bash
make dev      # 本地开发构建 -> build/heliox-mon（当前平台）
make build    # 生产构建 Linux/amd64（CGO_ENABLED=0）
make release  # 同时构建 Linux amd64 + arm64
make test     # go test ./...
make fmt      # go fmt + goimports（goimports 需先安装）
make lint     # golangci-lint run

# 本地运行（必须设置密码；Mac 上自动产生 mock 数据）
HELIOX_MON_PASS=test go run ./cmd/heliox-mon
```

部署/运维（install/start/stop/update 等）由 heliox 仓库的 `deploy.sh monitor <cmd>` 驱动，不在本仓库内。

## 发布流程（关键：push main ≠ 部署到 VPS）

- **VPS 只认 GitHub Release。** heliox 仓库 `deploy.sh monitor install/update` 从
  `releases/latest/download/heliox-mon-linux-<amd64|arm64>` 下预编译二进制，**从不从源码构建**。
  因此：**只 `git push` 到 main，VPS 永远拉不到新代码**——前端是 `go:embed` 内嵌的，必须重新发版。
- **发新版就三步**：① 改 `CHANGELOG.md`（`[Unreleased]` → `[X.Y.Z] - 日期`）并 commit；
  ② `git tag vX.Y.Z`；③ `git push --follow-tags`。tag 一推，`.github/workflows/release.yml`
  自动 `make release` + 生成 `SHA256SUMS` + 发 Release（成为 `latest`）。**不要再手动
  `gh release create`**，会和 CI 撞车。之后 VPS 上 `sudo ./deploy.sh monitor update`。
- **版本号**由 `git describe --tags` 经 `-ldflags -X main.Version` 注入；release 工作流用
  `fetch-depth: 0` 拿全 tag。新增功能走 minor，修复走 patch（语义化版本）。
- 验证线上二进制是否含改动：`curl -su admin:密码 http://127.0.0.1:9100/ | grep -c <新标记>`。

## 测试与质量门禁

- **CI**（`.github/workflows/ci.yml`）在 push/PR 时跑：`golangci-lint` + `go vet` + `go test` + 三平台构建（linux amd64/arm64 + darwin，保护 mock 路径）+ `govulncheck`。门禁在 **Linux** 上运行。
- **本机跑 lint 必须加 `GOOS=linux`**：`GOOS=linux golangci-lint run ./...`。否则 darwin 上会把 `collector.go` 里只被 `_linux.go` 使用的字段误报为 `unused`（跨平台假阳性）。
- **lint 配置** `.golangci.yml` 启用 errcheck/staticcheck/govet/bodyclose/errorlint 等——**不要静默吞掉错误**，否则 errcheck 会让 CI 变红。
- 单测分布：`config`（计费周期/配置解析）、`storage`（迁移/查询）可在 Mac 跑；`*_linux_test.go`（`isVirtualIface`、`latency`）仅 Linux 构建，本机 `go test` 不执行,靠 CI 覆盖。

## 架构与关键约定

- **跨平台靠 build tag 分离采集逻辑**：`internal/collector/*_linux.go` 是真实采集（读 `/proc`、`iptables`），`collector_darwin.go` 是 Mac 本地开发用的 mock 实现。**修改采集逻辑时通常要同时改 Linux 与 Darwin 两份**，否则 Mac 上跑的是另一套代码。
- **改完务必跨平台编译验证**：本机多为 Darwin，Linux-only 代码不会被本机 `go build` 检查到。提交前跑 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`。
- **带 `_linux` 后缀的测试仅在 Linux 构建**（如 `latency_linux_test.go`、`network_linux_test.go`），Mac 上 `go test` 不会执行它们，依赖 CI 覆盖。
- **前端通过 `go:embed` 内嵌**（`web/embed.go`，含 `vendor/` 目录）：改动 `web/*.{html,js,css,svg}` 后必须重新构建二进制才生效。**图表库（Chart.js / annotation / ECharts）已本地化到 `web/vendor/`，不走 CDN**——升级版本需替换 `vendor/` 下文件并重建。
- **HTTP 响应统一用 `writeJSON` 辅助函数**（`internal/api/router.go`）输出 JSON；写出失败记日志而非静默忽略。
- **SQLite 用纯 Go 驱动 `modernc.org/sqlite`（无需 CGO）**，WAL 模式；表结构与迁移集中在 `internal/storage/sqlite.go` 的 `migrate()`，启动时执行。
- **配置全部来自环境变量**（`internal/config/config.go`），无配置文件；唯一例外是读取 heliox 的 `.env` 以获取 `SNELL_PORT`/`VLESS_PORT`。`HELIOX_MON_PASS` 必填，缺失则启动失败。
- **认证**：Web 用随机 token + HttpOnly Cookie 会话，`/api/*` 同时兼容 Basic Auth；可选 Cloudflare Turnstile。
- **数据流**：`collector` 每秒/每分钟采集 -> 写快照表 -> 每分钟降采样为日汇总(`*_daily`)；`api` 读库返回 JSON，实时网速经 SSE (`/api/traffic/realtime`) 从采集器内存推送。
- **通知（Telegram）** 集中在 `internal/notifier`：阈值流量预警（`alert_records` 表做 24h 冷却）、每日流量报告（`SendDailyReport`，采集器 `runDailyReport` 用定时器对齐到 `DAILY_REPORT_HOUR` 整点、重启不重发）、页面测试发送（`SendTest` ← `POST /api/notify/test`）。三类消息都带 `[SERVER_NAME]` 前缀以区分多机。新增相关配置项时同步更新 `.env.example` 与 README 环境变量表。

## 统计准确性（改动相关代码时务必保持）

- 流量只统计物理网卡：`readProcNetDev` 通过 `isVirtualIface` 排除 `lo`/容器网桥/隧道接口（tun/wg/cloudflared 等），避免代理流量被重复计入。新增隧道类型需更新前缀列表。
- 累计计数器用偏移量处理重启/溢出（`initTrafficOffsets` + 采集时检测回退）；不要改成直接用原始 `/proc` 值。
- 延迟数据：原始记录保留 7 天，更早的按 10 分钟桶降采样并保留 90 天（`aggregateLatencyData`）——不要退回成直接 DELETE。

## 代码风格

- 注释与日志信息使用中文，保持与现有代码一致。
- 遵循 gofmt；提交前确保 `make test`、`go vet ./...` 与 `GOOS=linux golangci-lint run ./...` 通过。
- 显式处理错误，不要静默吞掉返回的 `error`（CI 的 errcheck 会拦截）。
- 版本变更记入 `CHANGELOG.md`（Keep a Changelog 格式）。
