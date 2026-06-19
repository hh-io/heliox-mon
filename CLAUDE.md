# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Heliox Monitor 是为 [heliox](https://github.com/Theo-oh/heliox) 代理服务设计的轻量级服务器监控系统：采集系统资源/流量/端口/延迟，存入 SQLite，通过内嵌 Web 前端展示，单二进制部署。

## 命令

```bash
make dev      # 本地开发构建 -> build/heliox-mon（当前平台）
make build    # 生产构建 Linux/amd64（CGO_ENABLED=0）
make release  # 同时构建 Linux amd64 + arm64
make test     # go test ./...
make fmt      # go fmt + goimports

# 本地运行（必须设置密码；Mac 上自动产生 mock 数据）
HELIOX_MON_PASS=test go run ./cmd/heliox-mon
```

部署/运维（install/start/stop/update 等）由 heliox 仓库的 `deploy.sh monitor <cmd>` 驱动，不在本仓库内。

## 架构与关键约定

- **跨平台靠 build tag 分离采集逻辑**：`internal/collector/*_linux.go` 是真实采集（读 `/proc`、`iptables`），`collector_darwin.go` 是 Mac 本地开发用的 mock 实现。**修改采集逻辑时通常要同时改 Linux 与 Darwin 两份**，否则 Mac 上跑的是另一套代码。
- **改完务必跨平台编译验证**：本机多为 Darwin，Linux-only 代码不会被本机 `go build` 检查到。提交前跑 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`。
- **测试文件 `latency_linux_test.go` 仅在 Linux 构建**（文件名带 `_linux`），Mac 上 `go test` 不会执行它。
- **前端通过 `go:embed` 内嵌**（`web/embed.go`）：改动 `web/*.{html,js,css,svg}` 后必须重新构建二进制才生效。
- **SQLite 用纯 Go 驱动 `modernc.org/sqlite`（无需 CGO）**，WAL 模式；表结构与迁移集中在 `internal/storage/sqlite.go` 的 `migrate()`，启动时执行。
- **配置全部来自环境变量**（`internal/config/config.go`），无配置文件；唯一例外是读取 heliox 的 `.env` 以获取 `SNELL_PORT`/`VLESS_PORT`。`HELIOX_MON_PASS` 必填，缺失则启动失败。
- **认证**：Web 用随机 token + HttpOnly Cookie 会话，`/api/*` 同时兼容 Basic Auth；可选 Cloudflare Turnstile。
- **数据流**：`collector` 每秒/每分钟采集 -> 写快照表 -> 每分钟降采样为日汇总(`*_daily`)；`api` 读库返回 JSON，实时网速经 SSE (`/api/traffic/realtime`) 从采集器内存推送。

## 统计准确性（改动相关代码时务必保持）

- 流量只统计物理网卡：`readProcNetDev` 通过 `isVirtualIface` 排除 `lo`/容器网桥/隧道接口（tun/wg/cloudflared 等），避免代理流量被重复计入。新增隧道类型需更新前缀列表。
- 累计计数器用偏移量处理重启/溢出（`initTrafficOffsets` + 采集时检测回退）；不要改成直接用原始 `/proc` 值。
- 延迟数据：原始记录保留 7 天，更早的按 10 分钟桶降采样并保留 90 天（`aggregateLatencyData`）——不要退回成直接 DELETE。

## 代码风格

- 注释与日志信息使用中文，保持与现有代码一致。
- 遵循 gofmt；提交前确保 `make test` 与 `go vet ./...` 通过。
