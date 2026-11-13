<div align="center">

# Heliox Monitor

**轻量级单二进制 VPS 监控系统** — 系统资源 / 流量统计 / 端口流量 / 延迟监控

专为 [Heliox](https://github.com/Theo-oh/heliox) 代理服务设计，单文件部署，无外部依赖

[![CI](https://github.com/hh-io/heliox-mon/actions/workflows/ci.yml/badge.svg)](https://github.com/hh-io/heliox-mon/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/hh-io/heliox-mon)](https://github.com/hh-io/heliox-mon/releases/latest)
[![Go Version](https://img.shields.io/github/go-mod/go-version/hh-io/heliox-mon)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Built with Claude Code](https://img.shields.io/badge/Built%20with-Claude%20Code-D97757?logo=claude&logoColor=white)](https://claude.com/claude-code)

</div>

> 🤖 本项目由作者与 [Claude Code](https://claude.com/claude-code) 结对开发完成——从架构设计、编码实现、测试到发布流程全程 AI 协作，详见 [AI 协作](#-ai-协作)。

---

## 目录

- [特性](#-特性)
- [快速开始](#-快速开始)
- [配置](#️-配置)
- [认证与安全](#-认证与安全)
- [端口流量监控](#-端口流量监控)
- [客户端延迟上报](#-客户端延迟上报可选)
- [架构](#-架构)
- [开发](#️-开发)
- [AI 协作](#-ai-协作)
- [License](#-license)

---

## ✨ 特性

- 📊 **系统资源监控** — CPU（含宿主机 steal 占比）/ 内存 / 磁盘 / 系统负载 / 活跃连接数 /
  TCP 重传率共 6 张卡片，带阈值角标与进度条，SSE 推送，5 秒刷新（连接数 15 秒）；
  运行时长在页眉常驻
- 🚀 **实时网速** — SSE 推送，1 秒刷新，含实时趋势图
- 📈 **流量统计** — 本计费周期总量与按协议明细（今日 / 昨日 / 周期累计）同屏，每分钟更新；近 6 个月趋势图
- 🔌 **端口流量** — 基于 iptables 计数器，Snell / VLESS 分别统计，支持自定义端口
- ⚠️ **流量配额** — 自定义计费周期（`RESET_DAY`）与计费模式（双向 / 仅上行 / 仅下行 / 取最大值），
  含剩余额度、日均消耗与周期末预估
- 📡 **延迟监控** — 多目标 Ping + 可选客户端端到端 RTT 上报，交互式时间范围、动态粒度聚合
- 🔔 **Telegram 通知** — 阈值流量预警（24h 冷却）、每日流量报告定时推送
- 📦 **单文件部署** — 前端与图表库（Chart.js / ECharts）全部 `go:embed` 进二进制，**无外部 CDN 依赖**，纯 Go SQLite 驱动无需 CGO，下载即用

## 🚀 快速开始

### 前置条件

VPS 已部署 [heliox](https://github.com/Theo-oh/heliox)。

### 一键安装

```bash
cd ~/heliox && git pull            # 1. 进入 heliox 目录
sudo ./deploy.sh monitor install   # 2. 安装监控
sudo ./deploy.sh monitor start     # 3. 启动
cat /opt/heliox-mon/.env | grep PASS   # 4. 查看自动生成的密码
```

### 访问

浏览器打开站点会跳转到登录页（`/login`），登录后通过 Cookie 会话保持 30 天。脚本/命令行可继续使用 Basic Auth：

```bash
# 命令行（Basic Auth，兼容脚本集成）
curl -u admin:密码 http://127.0.0.1:9100/api/system

# 通过 Cloudflare Tunnel 外部访问（配置 URL: http://host.docker.internal:9100）
```

### 运维命令

部署与运维由 heliox 仓库的 `deploy.sh` 驱动：

```bash
./deploy.sh monitor <command>

install    # 安装          start      # 启动
stop       # 停止          restart    # 重启
status     # 查看状态      logs       # 查看日志
update     # 更新到最新版  uninstall  # 卸载
```

### 更新

```bash
cd ~/heliox && git pull
sudo ./deploy.sh monitor update
```

### 多 VPS 部署

```bash
for vps in vps-la vps-tyo vps-hk; do
  ssh root@$vps 'cd ~/heliox && git pull && sudo ./deploy.sh monitor install && sudo ./deploy.sh monitor start'
done
```

每台 VPS 的 `SERVER_NAME` 自动使用主机名区分，Telegram 通知均带 `[SERVER_NAME]` 前缀。

## ⚙️ 配置

配置全部来自环境变量，配置文件：`/opt/heliox-mon/.env`（参考 [`.env.example`](.env.example)），修改后执行 `sudo ./deploy.sh monitor restart` 生效。

| 变量                      | 说明                          | 默认值                            |
| ------------------------- | ----------------------------- | --------------------------------- |
| `HELIOX_MON_PASS`         | 登录密码（**必填**）          | 安装时自动生成                    |
| `HELIOX_MON_USER`         | 登录用户名                    | admin                             |
| `HELIOX_MON_LISTEN`       | 监听地址                      | 127.0.0.1:9100                    |
| `HELIOX_MON_DATA_DIR`     | 数据目录（SQLite）            | /var/lib/heliox-mon               |
| `SERVER_NAME`             | 服务器标识                    | Heliox（建议设为主机名）          |
| `HELIOX_MON_TZ`           | 时区                          | Asia/Singapore                    |
| `HELIOX_ENV_PATH`         | heliox 的 .env 路径（读端口） | ../heliox/.env                    |
| `MONTHLY_LIMIT_GB`        | 月流量限额(GB)                | 1000                              |
| `BILLING_MODE`            | 计费模式（见下表）            | bidirectional                     |
| `RESET_DAY`               | 计费周期重置日 (1-28)         | 1 (每月1号)                       |
| `ALERT_THRESHOLDS`        | 报警阈值百分比（逗号分隔）    | 80,90,95                          |
| `TELEGRAM_BOT_TOKEN`      | Telegram Bot Token            | 空                                |
| `TELEGRAM_CHAT_ID`        | Telegram 接收会话 ID          | 空                                |
| `DAILY_REPORT_ENABLED`    | 每日流量报告推送开关          | false                             |
| `DAILY_REPORT_HOUR`       | 每日报告推送时刻（0-23 整点） | 9                                 |
| `HELIOX_TURNSTILE_SECRET` | Cloudflare Turnstile 密钥     | 空（设置后启用人机验证）          |
| `HELIOX_TURNSTILE_SITEKEY`| Turnstile 站点公钥            | 内置默认值（换自己的站点时才需改）|
| `PING_TARGETS`            | 延迟监控目标 (`TAG:IP`)       | Google:8.8.8.8,Cloudflare:1.1.1.1 |
| `PING_COUNT`              | 每次 ping 发包数，夹在 1-40    | 20                                |
| `PING_TIMEOUT_MS`         | 单次 ping 超时(ms)            | 1000                              |
| `PING_GAP_MS`             | ping 发包间隔(ms)，传给 `-i`；非 root 下自动夹到 200 | 200（Linux 非 root 下限）         |
| `HELIOX_MON_REPORT_TOKEN` | 客户端延迟上报令牌            | 空（设置后启用上报接口）          |

### 计费模式 (BILLING_MODE)

| 值            | 说明              |
| ------------- | ----------------- |
| bidirectional | 上行+下行 (默认)  |
| tx_only       | 仅计算上行        |
| rx_only       | 仅计算下行        |
| max_value     | 取上行/下行较大值 |

### Telegram 通知

流量预警和每日报告都通过 Telegram Bot 推送，需要两个值：

1. **Bot Token**：在 Telegram 里找 [@BotFather](https://t.me/BotFather)，发送 `/newbot`，
   按提示创建后返回形如 `123456789:AA...` 的 token，即 `TELEGRAM_BOT_TOKEN`。
2. **Chat ID**：先给刚建的机器人发一条任意消息（否则机器人无权私聊你），然后：
   - 私聊场景：把消息转发给 [@userinfobot](https://t.me/userinfobot) 获取数字 ID；
     或访问 `https://api.telegram.org/bot<TOKEN>/getUpdates`，在返回 JSON 里找 `chat.id`。
   - 群组/频道推送：把机器人拉进群，群里发一条消息后同样用 `getUpdates` 取 `chat.id`
     （群 ID 通常是负数）。
3. 把两个值填入 `.env`，可在 Web 页面用「测试发送」验证连通。

**每日流量报告**：设 `DAILY_REPORT_ENABLED=true` 后，每天在 `DAILY_REPORT_HOUR`（按
`HELIOX_MON_TZ` 时区的整点，默认 9 点）推送昨日上/下行用量、本计费周期累计、占限额百分比
与剩余量。推送时刻按定时器对齐，进程重启会重新计算下一次，不会重复推送。

## 🔐 认证与安全

- **登录会话**：Web 登录签发 HMAC-SHA256 签名的 token，存 HttpOnly Cookie，有效期 30 天且会滑动续期。签名密钥随机生成后存在数据库里，**服务重启或升级不会让已登录的浏览器掉线**。
- **数据库权限**：数据库文件（含 WAL）权限为 0600，仅运行用户可读。
- **Basic Auth 回退**：`/api/*` 同时接受 Basic Auth，便于脚本与监控集成。
- **常量时间比较**：用户名/密码校验使用 `crypto/subtle`，防时序攻击。
- **Cloudflare Turnstile**（可选）：设置 `HELIOX_TURNSTILE_SECRET` 后登录需通过人机验证，校验失败按「拒绝」处理（fail-secure）。
- **Secure Cookie**：经 HTTPS（含 Cloudflare Tunnel 的 `X-Forwarded-Proto`）访问时自动启用 `Secure` 标志。
- 建议监听 `127.0.0.1`，仅通过 Cloudflare Tunnel 等反向代理对外暴露。

## 🔌 端口流量监控

按端口（如 Snell、VLESS）分别统计流量，显示各协议的今日/昨日/本计费周期使用量。

### 工作原理

使用 iptables / ip6tables 计数器统计端口流量：在两个地址族下各创建 `HELIOX_STATS` 链，
按端口分别记录上行（TX）与下行（RX），同时统计 TCP/UDP，IPv4 与 IPv6 累加；每秒采集快照，
每分钟汇总到日统计。本机没有可用 ip6tables（如内核未启用 IPv6）时只统计 IPv4。

**无需手动配置。** `./deploy.sh monitor install` 时自动生成规则脚本
`/opt/heliox-mon/setup-iptables.sh` 并配置 systemd `ExecStartPre`，服务器重启后自动恢复规
则；`monitor update` 会重写该脚本，`monitor uninstall` 会清理规则。验证：
`iptables -L HELIOX_STATS -n -v` 与 `ip6tables -L HELIOX_STATS -n -v`。端口从
`HELIOX_ENV_PATH` 指向的 Heliox `.env` 读取（`SNELL_PORT` / `VLESS_PORT`），
`ENABLE_SNELL=false` / `ENABLE_XRAY=false` 的协议不统计。手动部署可用仓库里的
`scripts/setup-iptables.sh`（`--clean` 清理规则）。

### 统计准确性

- 流量仅采集物理网卡，自动排除 `lo`、容器网桥（docker/br-/veth）及隧道接口
  （tun/wg/cloudflared/tailscale 等），避免代理流量被重复计入。
- 累计计数器用偏移量处理进程重启与计数器溢出，历史统计不跳变。

### 数据持久化

| 数据         | 保留策略                                        |
| ------------ | ----------------------------------------------- |
| 流量快照     | 从昨日 00:00 起（确保昨日统计完整）             |
| 流量日统计   | 永久保存                                        |
| 延迟数据     | 原始记录 7 天；更早按 10 分钟桶降采样保留 90 天 |
| 系统资源指标 | 不落库，仅保留内存中的最新一份快照              |

## 📡 客户端延迟上报（可选）

服务端 `PING_TARGETS` 只能主动探测公网目标，测不了「用户侧到 VPS」这个方向——客户端在
NAT 后无法被反向探测。开启客户端上报后，客户端周期性测量到 VPS 的端到端 RTT 并推送，
数据完全复用现有延迟图表，效果等价于在 `PING_TARGETS` 里无缝多了一项（序列名为客户端名）。

在 `.env` 设置 `HELIOX_MON_REPORT_TOKEN=<随机串>` 后重启生效（与登录密码隔离；泄露仅能
写入延迟数据，无法读数据或登录），留空则上报接口返回 403。

| 接口 | 认证 | 说明 |
| ---- | ---- | ---- |
| `GET /api/echo` | 无 | 极简回显（204，不写库不打日志），供客户端测端到端 TCP RTT |
| `POST /api/latency/report` | `Authorization: Bearer <token>` | 上报样本，写入延迟库 |

上报请求体：

```json
{ "client": "home-mac",
  "samples": [ {
    "rtt_ms": 45.2, "min_rtt": 42.1, "max_rtt": 52.0, "p95_rtt": 50.1,
    "mdev": 3.4, "sent": 10, "lost": 1, "rtts": [42.1, 44.0, 45.2]
  } ] }
```

有 `rtts` 时服务端按成功包重算 min/avg/max/p95，摘要字段可省略。超时、非 204
不得写进 `rtts`，应计入 `lost`（参考脚本已按 `http_code` 区分）。

`client` 仅允许字母/数字/下划线/连字符（≤32 字符），作为图表序列名，需自行保证各客户端唯一。
`ts` 可选（Unix 秒，须在 now-24h ~ now+5min 内），缺省用服务端当前时间。

仓库 [`scripts/latency-client.sh`](scripts/latency-client.sh)（依赖 bash/curl/awk）已实现
「预热丢弃握手 → keep-alive 串行测 N 次 → 204 为成功、超时计丢包 → 取 min/avg/p95/max → 上报」，配 cron/launchd
每 60s 一轮即可：

```bash
MON_URL=http://<vps-ip>:9100 CLIENT_NAME=home-mac REPORT_TOKEN=xxx ./latency-client.sh
```

> ⚠️ **必须直连 VPS，不能用 Cloudflare Tunnel 域名。** 经 Tunnel 测得的是「客户端→CF
> 边缘」的 RTT（anycast 就近接入，数值漂亮但无意义），非到 VPS 的端到端延迟。这需要把
> `HELIOX_MON_LISTEN` 放开到 `0.0.0.0:9100` 或加防火墙放行；不愿暴露端口者可退化为客户端
> 自行 `ping <vps-ip>` 后仅调用上报接口，echo 是可选增强。

## 🏗 架构

```
┌────────────┐  每秒/每分钟   ┌─────────────┐  每分钟降采样  ┌──────────────┐
│ collector  │ ─────────────> │   SQLite    │ ─────────────> │  *_daily 表  │
│ /proc      │                │ (纯 Go 驱动  │                └──────────────┘
│ iptables   │                │  WAL 模式)  │ <── 读库 ── api ──> Web UI (go:embed)
│ ping       │                └─────────────┘                     │
└─────┬──────┘                                                    │
      │ 实时网速（内存） ── SSE /api/traffic/realtime ────────────┘
      └─ 阈值预警 / 每日报告 ──> notifier (Telegram)
```

| 目录                  | 职责                                                         |
| --------------------- | ------------------------------------------------------------ |
| `cmd/heliox-mon`      | 入口，版本号经 `-ldflags` 注入                               |
| `internal/collector`  | 采集：Linux 真实实现（`*_linux.go`）/ macOS mock（本地开发） |
| `internal/storage`    | SQLite 存储与迁移（`modernc.org/sqlite`，无 CGO）            |
| `internal/api`        | HTTP API、认证、SSE                                          |
| `internal/notifier`   | Telegram 通知（预警 / 日报 / 测试发送）                      |
| `internal/config`     | 环境变量解析、计费周期计算                                   |
| `web/`                | 前端（`js/` 原生 ES Modules，`vendor/` 本地化图表库），`go:embed` 内嵌 |

## 🛠️ 开发

```bash
make dev      # 本地构建（当前平台）-> build/heliox-mon
make build    # 生产构建 Linux/amd64（CGO_ENABLED=0）
make release  # 同时构建 Linux amd64 + arm64
make test     # 单元测试
make lint     # golangci-lint（本机建议 GOOS=linux 跑，避免跨平台假阳性）

# 本地运行（必须设置密码；Mac 上自动产生 mock 数据）
HELIOX_MON_PASS=test go run ./cmd/heliox-mon
```

### 前端调试

改 `web/` 下的页面时用这条命令，五个模块都能看到数据：

```bash
HELIOX_MON_PASS=test HELIOX_MON_DEV_NO_AUTH=1 HELIOX_MON_DATA_DIR=/tmp/heliox-data \
  go run ./cmd/heliox-mon
# → http://127.0.0.1:9100
```

- **`HELIOX_MON_DEV_NO_AUTH=1` 跳过登录**，省掉每次调试都要过一遍登录页。
  这个开关只存在于 **darwin 构建**（见 `internal/config/devauth_darwin.go`），生产的 Linux
  二进制里没有对应分支；即便如此，服务端仍只对**回环地址**放行，同网段的其他机器进不来。
- **Turnstile 本地自动消失**：没配 `HELIOX_TURNSTILE_SECRET` 时，服务端不会把 Cloudflare
  的脚本与部件注入登录页（`buildLoginPage`），离线/内网环境不会再卡在一个连不上的验证框。
- **改完前端必须重启进程**：前端是 `go:embed` 内嵌的，**编译期快照**——只在浏览器里刷新
  永远看的是上次启动时的那份文件。这是本地调试最容易反复踩的坑。校验方式：
  ```bash
  curl -s localhost:9100/style.css | grep -c <你刚加的类名>   # 0 就是没重启
  ```
- ES module **不能用 `file://` 打开**（CORS 拒绝，报错与真实故障无关），只能走上面的服务。
- **模块图失败是静默的**：任何一个 js 文件 404 或语法出错，整个入口都不执行，页面停在骨架
  且界面上没有任何提示。排查第一步永远是看 Network 找 404；控制台里的
  `heliox ui booted` 是「模块跑起来了」的信标。
- 改动前端后自查：DOM id 交叉验证、相对 import 的 `.js` 后缀、`web/embed.go` 与 `web/sw.js`
  的清单是否补齐（新增模块文件三处都要改），详见 [`CLAUDE.md`](CLAUDE.md)。

跨平台采集逻辑按 build tag 分离（`*_linux.go` 真实采集 / `collector_darwin.go` mock），
改采集代码需同步两端，并跑 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` 验证。
push/PR 会触发 CI（lint、vet、test、多平台构建、govulncheck）。约定详见 [`CLAUDE.md`](CLAUDE.md)，
版本变更记入 [`CHANGELOG.md`](CHANGELOG.md)（Keep a Changelog 格式）。

## 🤖 AI 协作

本项目是一次完整的 **人机结对开发** 实践：由作者主导需求与决策，
[Claude Code](https://claude.com/claude-code)（Anthropic 的终端 AI 编程助手）深度参与了：

- **架构设计** — 跨平台 build tag 分离、SQLite 降采样策略、SSE 实时推送等方案设计
- **编码实现** — 采集器、存储层、API、前端图表与 Telegram 通知的绝大部分代码
- **质量保障** — 单元测试、lint 规则治理、CI 工作流与跨平台构建验证
- **工程流程** — 语义化版本发布、CHANGELOG 维护与部署脚本对接

提交历史中的 `Co-Authored-By: Claude` 即为协作记录。项目的工程约定沉淀在
[`CLAUDE.md`](CLAUDE.md) 中，供 AI 与人类协作者共同遵循。

## 📄 License

[MIT](LICENSE) © hh
