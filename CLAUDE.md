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

## 发布流程（关键：发版 ≠ 部署到 VPS）

- **VPS 只认 GitHub Release。** heliox 仓库 `deploy.sh monitor install/update` 从
  `releases/latest/download/heliox-mon-linux-<amd64|arm64>` 下预编译二进制，**从不从源码构建**。
  前端是 `go:embed` 内嵌的，改了 `web/` 不重新发版线上就看不到。
- **push main 即发版。** `.github/workflows/release.yml` 在 branch push 和 `v*` tag 上都会跑，
  产物一致、都会成为 `releases/latest`：
  - **push main** → 自动发版，Release 的 tag 名是 `build-<run_number>`，版本号取
    `git describe`（形如 `v0.26.2-3-g40910b2`，能唯一定位 commit）。日常改完直接 push 即可。
  - **push `vX.Y.Z` tag** → 正式发版，版本号是干净的 `vX.Y.Z`。发正式版仍是三步：
    改 `CHANGELOG.md`（`[Unreleased]` → `[X.Y.Z] - 日期`）并 commit → `git tag vX.Y.Z` →
    `git push --follow-tags`。新增功能走 minor，修复走 patch。
  - 两种情况都**不要手动 `gh release create`**，会和 CI 撞车。
- **自动发版的安全前提是 release.yml 里的 `go vet` + `go test` 门禁**（`ci.yml` 与它并行跑，
  拦不住发布）。**不要为了省 CI 时间删掉这一步**——push 就发版是无人值守的，
  没有门禁就等于把红着的构建直接推成 `latest`。
- **`Makefile` 的 `git describe` 必须带 `--match 'v[0-9]*'`**：自动发版会在仓库里留下
  `build-N` tag，不过滤的话下一次 describe 就以它为基准，版本号会一轮比一轮长。
- **发版不等于线上更新。** `latest` 变了 VPS 也不会自己动，仍要 `sudo ./deploy.sh monitor update`
  才生效——这道手动闸门是自动发版能成立的前提，不要改成定时自动拉取（监控服务自动更新
  失败时，你恰好失去了发现它失败的手段）。
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
- **前端通过 `go:embed` 内嵌**（`web/embed.go`，含 `js/` 与 `vendor/` 目录）：改动 `web/` 下任何前端文件后必须重新构建二进制才生效。**图表库（Chart.js / annotation / ECharts）已本地化到 `web/vendor/`，不走 CDN**——升级版本需替换 `vendor/` 下文件并重建。
- **前端是浏览器原生 ES Modules，无构建步骤、无 npm**。`web/js/core/` 是横切能力（格式化、DOM、HTTP、SSE 总线、主题、图表库出口），`web/js/modules/` 一个文件对应页面一个区块；区块太大时拆成同名子目录（`modules/latency/{index,chart,stats,palette}.js`，入口固定叫 `index.js`）。**硬约束：`core/` 不得 import `modules/`**，否则主题模块与图表模块会形成循环依赖，命中 TDZ 且只在特定加载顺序下复现。**子目录内同理：`chart.js` / `stats.js` 不得反向 import `index.js`**——它们要的口径由 `index.js` 算好当参数传入（渲染函数的副产品被别处读取，一旦渲染早返回就会拿到上一轮的值，画面与数字对不上且不报错）。
  - 相对 import **必须写 `.js` 后缀**（浏览器不做扩展名补全）；新增模块文件要同步改三处：`web/embed.go` 的 embed 列表（漏了编译期不报错、运行时 404）、`web/sw.js` 的 `ASSETS_TO_CACHE` 并 bump `CACHE_NAME`、必要时 `web/index.html` 的 `modulepreload`。
  - **模块图失败是静默的**：任何一个 js 文件 404 或语法出错，整个入口都不执行，页面停在骨架且界面上没有任何提示。排查第一步永远是看 Network 找 404，`main.js` 顶部的 `console.info("heliox ui booted")` 是「模块跑起来了」的信标。
  - 图表模块的**颜色一律在 render 函数体内现取**（`getCssVar`），不能在模块顶层求值；主题切换靠 `core/theme.js` 的 `onThemeChange` 注册重绘回调，不要退回成在 `applyTheme` 里硬编码调用列表（那份列表历史上漏过趋势图）。
  - 实时网速的 `txSeries`/`rxSeries` 引用被 Chart.js dataset 持有，**只能原地 `shift`/`push`**；整体赋值会让图表静止而读数继续跳，不报错且极难查。
  - **窄屏要覆盖的 CSS 属性，其基础规则不能比响应式断点更"特异"**：`.metric-bar.lat-metrics`（0,2,0）会压过断点里的 `.metric-bar`（0,1,0），指标条锁列数的规则失效、去分隔线的 `nth-child` 全部错位。给模块专属类加列定义时，断点一节要同时点名它。
  - 弹层开合（点外部 / Esc 关闭）统一走 `core/dom.js` 的 `bindPopover`，外壳样式统一用 `.popover`。
  - ES module 不能用 `file://` 打开（CORS 拒绝，报错与真实故障无关），调试一律跑 `HELIOX_MON_PASS=test go run ./cmd/heliox-mon`。
  - 类型检查由根目录 `jsconfig.json`（`checkJs` + JSDoc）提供，只作用于编辑器，不进 CI、不引入 npm 依赖。`jsconfig.json` 刻意放在仓库根而非 `web/` 下，避免被 `go:embed js` 打进二进制。
- **HTTP 响应统一用 `writeJSON` 辅助函数**（`internal/api/router.go`）输出 JSON；写出失败记日志而非静默忽略。
  `/api/latency` 额外套 `gzipJSON`（逐包样本让 24h 视图接近 1MB，前端每 60 秒全量重拉）；**SSE 端点不能套**，压缩流会被缓冲，实时推送变成一次性吐出。
- **SQLite 用纯 Go 驱动 `modernc.org/sqlite`（无需 CGO）**，WAL 模式；表结构与迁移集中在 `internal/storage/sqlite.go` 的 `migrate()`，启动时执行。
- **配置全部来自环境变量**（`internal/config/config.go`），无配置文件；唯一例外是读取 heliox 的 `.env` 以获取 `SNELL_PORT`/`VLESS_PORT`。`HELIOX_MON_PASS` 必填，缺失则启动失败。
- **认证**：Web 用 HMAC 签名的无状态 token + HttpOnly Cookie 会话（`internal/api/session.go`），`/api/*` 同时兼容 Basic Auth；可选 Cloudflare Turnstile。**签名密钥持久化在 `config` 表**，不要改回进程内存里的 session map——那样每次更新/重启都会把所有浏览器踢回登录页；也不要改成从 `HELIOX_MON_PASS` 派生密钥，那等于让持有合法 token 的人可以离线爆破管理员密码。
- **数据流**：`collector` 每秒/每分钟采集 -> 写快照表 -> 每分钟降采样为日汇总(`*_daily`)；`api` 读库返回 JSON。**流量与系统资源两类实时数据不落库**，只在采集器内存里维护最新快照（`RealtimeSnapshot` / `SystemSnapshot`），都经 SSE (`/api/traffic/realtime`) 推送：默认消息是每秒的网速，具名事件 `system` 是系统资源（每秒检查 `SystemSnapshot.Ts` 是否变化，变了才推——**不要退回成独立的 5 秒 ticker**，那与采集周期不同相位，会让页面数据平白多陈旧最多 5 秒）；`/api/system` 保留为同一份快照的一次性拉取入口。
- **通知（Telegram）** 集中在 `internal/notifier`：阈值流量预警（`alert_records` 表做 24h 冷却）、每日流量报告（`SendDailyReport`，采集器 `runDailyReport` 用定时器对齐到 `DAILY_REPORT_HOUR` 整点、重启不重发）、页面测试发送（`SendTest` ← `POST /api/notify/test`）。三类消息都带 `[SERVER_NAME]` 前缀以区分多机。新增相关配置项时同步更新 `.env.example` 与 README 环境变量表。

## 统计准确性（改动相关代码时务必保持）

- 流量只统计物理网卡：`readProcNetDev` 通过 `isVirtualIface` 排除 `lo`/容器网桥/隧道接口（tun/wg/cloudflared 等），避免代理流量被重复计入。新增隧道类型需更新前缀列表。
- 累计计数器用偏移量处理重启/溢出（`initTrafficOffsets` + 采集时检测回退）；不要改成直接用原始 `/proc` 值。
- 端口流量 = iptables + ip6tables 两个地址族之和（xray 监听 `::`，只读 v4 会漏掉 IPv6 客户端）。**按地址族各自维护基准、累加增量**（`advancePort`），不要退回成「先求和再检测回退」：某一族某轮读不到或被单独重建时，和会下降，被当成重置后把另一族整份重复计入。运行中新出现的地址族首轮只建基准。「当前要采集/展示哪些端口」一律走 `cfg.ActivePorts()`（排除 `ENABLE_*` 停用的协议）；`SnellPort`/`VlessPort` 停用后仍保留，历史查询按端口号取数，别为了「停用」把它们置 0。规则脚本的权威版本在 heliox `deploy.sh` 的 `write_monitor_iptables_script`，与本仓库 `scripts/setup-iptables.sh` 逻辑保持一致（含默认端口必须与 `loadHelioxEnv` 相同）。
- 延迟数据：每次探测解析逐包 RTT（不要只存 ping 摘要均值），原始记录保留 7 天，更早的按 10 分钟桶从真实样本算出 P95/max 后再丢掉 `rtts`，聚合行保留 90 天（`aggregateLatencyData`）——不要退回成直接 DELETE 或对均值再平均。`PING_COUNT` 夹在 `[1, config.MaxPingCount]`——它必须等于 `maxRTTsPerPoint`，否则一轮探测的样本装不进一个时间桶，API 不再下发 `rtts`，前端静默退回「P95 约」。`PING_GAP_MS` 必须传给 `ping -i`，且非 root 下要夹到 200ms——iputils 对非 root 强制 `-i >= 0.2`，更小的值会让 ping 以退出码 2 失败，而该退出码被归为「执行环境错误」只打日志跳过，结果是延迟图整片变空。
- 多目标时**前端读数一律只讲焦点目标**（`statsTag`，点胶囊切换）：横幅与指标条都不把不同
  链路的 RTT 求平均——40ms 与 170ms 平均出的 105ms 不是任何一条链路的现状。横幅的状态灯
  与标题例外，按所有选中目标里最差的那条判定并点名出事目标，否则焦点选中健康那条时另一条
  断了就没人看得见；读数的状态色因此走 `focus-*` 类而非 `is-*`，不能合并回一套类。
- CPU 使用率按 `busy = total - idle - iowait` 计算（`parseCPUStatLine`）：iowait 期间 CPU 是空闲的，不能算进使用率；`guest`/`guest_nice` 已被内核计入 `user`/`nice`，累加 `/proc/stat` 时只取到 `steal` 为止，多加会让分母偏大。steal 单独作为指标暴露，不要并回使用率。
- CPU 使用率与 TCP 重传率都依赖两次采样的差值，首次采样无基准时返回 `ok=false`，API 输出 `null` 让前端显示 `--`——不要为了「好看」填 0，那是把「不知道」谎报成「正常」。同理 `countEstablished` 读 `/proc/net/tcp{,6}` 出错（含 `scanner.Err()`）时返回 `nil` 而非部分结果，`conns_total` 随之输出 `null`：少算的连接数一样会被当真值展示。
- 连接数扫描每 15 秒一次（`connsCollectInterval`），其余轮次复用 `lastConns`。`/proc/net/tcp` 是 seq_file 全表遍历且内核持锁，别为了「更实时」改回每 5 秒——高连接数下监控自己就会成为负载源。
- steal 角标用近 1 分钟均值（`pushSteal` 维护 12 轮滑动窗口）判定，不用瞬时值：单轮 steal 抖到 1% 很常见，直接判定会让角标反复闪烁。无基准的那一轮不入窗口。

## 代码风格

- 注释与日志信息使用中文，保持与现有代码一致。
- 遵循 gofmt；提交前确保 `make test`、`go vet ./...` 与 `GOOS=linux golangci-lint run ./...` 通过。
- 显式处理错误，不要静默吞掉返回的 `error`（CI 的 errcheck 会拦截）。
- 版本变更记入 `CHANGELOG.md`（Keep a Changelog 格式）。
