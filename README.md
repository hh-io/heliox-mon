# Heliox Monitor

[![CI](https://github.com/Theo-oh/heliox-mon/actions/workflows/ci.yml/badge.svg)](https://github.com/Theo-oh/heliox-mon/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

轻量级服务器监控系统，专为 [Heliox](https://github.com/Theo-oh/heliox) 代理服务设计。

## 特性

- 📊 **系统资源监控** - CPU / 内存 / 磁盘 / 负载（实时 5 秒刷新）
- 🚀 **实时网速** - SSE 推送，1 秒刷新，含实时趋势图
- 📈 **流量统计** - 今日 / 昨日 / 本月 / 上月（每分钟更新）
- 🔌 **端口流量** - Snell / VLESS 分别统计，支持自定义端口
- ⚠️ **流量配额** - 支持自定义计费周期（ResetDay）和计费模式（billing_mode）
- 📡 **延迟监控** - 多目标 Ping，交互式时间范围选择，动态粒度聚合
- 📊 **月度趋势** - 近 6 个月流量趋势图
- 📦 **单文件部署** - 前端与图表库（Chart.js / ECharts）全部 `go:embed` 进二进制，**无外部 CDN 依赖**，下载即用

---

## 快速部署

### 前置条件

VPS 已部署 [heliox](https://github.com/Theo-oh/heliox)

### 一键安装

```bash
# 1. 进入 heliox 目录
cd ~/heliox && git pull

# 2. 安装监控
sudo ./deploy.sh monitor install

# 3. 启动
sudo ./deploy.sh monitor start

# 4. 查看密码
cat /opt/heliox-mon/.env | grep PASS
```

### 访问

浏览器打开站点会跳转到登录页（`/login`），输入用户名/密码后通过 Cookie 会话保持登录（有效期 30 天）。脚本/命令行可继续使用 Basic Auth：

```bash
# 命令行（Basic Auth，兼容旧脚本）
curl -u admin:密码 http://127.0.0.1:9100/api/system

# 通过 Cloudflare Tunnel 外部访问（配置 URL: http://host.docker.internal:9100）
```

---

## 认证与安全

- **登录会话**：Web 登录使用加密随机 token + HttpOnly Cookie，过期会话定时清理。
- **Basic Auth 回退**：`/api/*` 同时接受 Basic Auth，便于脚本与监控集成。
- **常量时间比较**：用户名/密码校验使用 `crypto/subtle`，防时序攻击。
- **Cloudflare Turnstile**（可选）：设置 `HELIOX_TURNSTILE_SECRET` 后，登录需通过人机验证，校验失败按「拒绝」处理（fail-secure）。
- **Secure Cookie**：经 HTTPS（含 Cloudflare Tunnel 的 `X-Forwarded-Proto`）访问时自动启用 `Secure` 标志。
- 建议监听 `127.0.0.1`，仅通过 Cloudflare Tunnel 等反向代理对外暴露。

---

## 命令

```bash
./deploy.sh monitor <command>

install    # 安装
start      # 启动
stop       # 停止
restart    # 重启
status     # 查看状态
logs       # 查看日志
update     # 更新到最新版
uninstall  # 卸载
```

---

## 配置

配置文件：`/opt/heliox-mon/.env`

| 变量                      | 说明                          | 默认值                            |
| ------------------------- | ----------------------------- | --------------------------------- |
| `HELIOX_MON_PASS`         | 登录密码（必填）              | 自动生成                          |
| `HELIOX_MON_USER`         | 登录用户名                    | admin                             |
| `HELIOX_MON_LISTEN`       | 监听地址                      | 127.0.0.1:9100                    |
| `HELIOX_MON_DATA_DIR`     | 数据目录（SQLite）            | /var/lib/heliox-mon               |
| `SERVER_NAME`             | 服务器标识                    | Heliox（建议设为主机名）          |
| `HELIOX_MON_TZ`           | 时区                          | Asia/Singapore                    |
| `HELIOX_ENV_PATH`         | heliox 的 .env 路径（读端口） | ../heliox/.env                    |
| `MONTHLY_LIMIT_GB`        | 月流量限额(GB)                | 1000                              |
| `BILLING_MODE`            | 计费模式                      | bidirectional                     |
| `RESET_DAY`               | 计费周期重置日 (1-28)         | 1 (每月1号)                       |
| `ALERT_THRESHOLDS`        | 报警阈值百分比（逗号分隔）    | 80,90,95                          |
| `TELEGRAM_BOT_TOKEN`      | Telegram Bot Token            | 空                                |
| `TELEGRAM_CHAT_ID`        | Telegram 接收会话 ID          | 空                                |
| `DAILY_REPORT_ENABLED`    | 每日流量报告推送开关          | false                             |
| `DAILY_REPORT_HOUR`       | 每日报告推送时刻（0-23 整点） | 9                                 |
| `HELIOX_TURNSTILE_SECRET` | Cloudflare Turnstile 密钥     | 空（设置后启用人机验证）          |
| `PING_TARGETS`            | 延迟监控目标 (`TAG:IP`)       | Google:8.8.8.8,Cloudflare:1.1.1.1 |
| `PING_COUNT`              | 每次 ping 发包数              | 5                                 |
| `PING_TIMEOUT_MS`         | 单次 ping 超时(ms)            | 1000                              |
| `PING_GAP_MS`             | ping 发包间隔(ms)             | 200                               |
| `HELIOX_MON_REPORT_TOKEN` | 客户端延迟上报令牌            | 空（设置后启用上报接口）          |

### 计费模式 (BILLING_MODE)

| 值            | 说明              |
| ------------- | ----------------- |
| bidirectional | 上行+下行 (默认)  |
| tx_only       | 仅计算上行        |
| rx_only       | 仅计算下行        |
| max_value     | 取上行/下行较大值 |

### 申请 Telegram Bot Token 与 Chat ID

流量预警和每日报告都通过 Telegram Bot 推送，需要两个值：

1. **Bot Token**：在 Telegram 里找 [@BotFather](https://t.me/BotFather)，发送 `/newbot`，
   按提示设置机器人名称，创建后它会返回形如 `123456789:AA...` 的 token，即 `TELEGRAM_BOT_TOKEN`。
2. **Chat ID**：先给你刚建的机器人发一条任意消息（否则机器人无权私聊你），然后：
   - 私聊场景：把消息转发给 [@userinfobot](https://t.me/userinfobot)，它会告诉你数字 ID；
     或访问 `https://api.telegram.org/bot<TOKEN>/getUpdates`，在返回 JSON 里找 `chat.id`。
   - 群组/频道推送：把机器人拉进群，群里发一条消息后同样用 `getUpdates` 取 `chat.id`
     （群 ID 通常是负数）。
3. 把两个值填入 `.env` 的 `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID`。

### 每日流量报告 (DAILY_REPORT_ENABLED)

设 `DAILY_REPORT_ENABLED=true` 后，每天在 `DAILY_REPORT_HOUR`（按 `HELIOX_MON_TZ`
时区的整点，默认 9 点）通过 Telegram 推送一条摘要：昨日上/下行用量、本计费周期累计、
占限额百分比与剩余量、周期重置日。**需先配好 `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID`**，
否则静默跳过。推送时刻按定时器对齐，进程重启会重新计算下一次，不会重复推送。

修改后执行 `sudo ./deploy.sh monitor restart` 生效。

---

## 客户端延迟上报（可选）

服务端 `PING_TARGETS` 只能主动探测公网目标，测不了「用户侧到 VPS」这个方向——客户端在
NAT 后无法被反向探测。开启客户端上报后，客户端周期性测量到 VPS 的端到端 RTT 并推送，
数据完全复用现有延迟图表，效果等价于在 `PING_TARGETS` 里无缝多了一项（序列名为客户端名）。

### 开启

在 `.env` 设置 `HELIOX_MON_REPORT_TOKEN=<随机串>`（与登录密码分开；泄露仅能写入延迟数据，
无法读数据或登录），重启生效。留空则上报接口返回 403。

### 两个接口

| 接口 | 认证 | 说明 |
| ---- | ---- | ---- |
| `GET /api/echo` | 无 | 极简回显（204，不写库不打日志），供客户端测端到端 TCP RTT |
| `POST /api/latency/report` | `Authorization: Bearer <token>` | 上报样本，写入延迟库 |

上报请求体：

```json
{ "client": "home-mac",
  "samples": [ { "rtt_ms": 45.2, "min_rtt": 42.1, "mdev": 3.4, "sent": 10, "lost": 0 } ] }
```

`client` 仅允许字母/数字/下划线/连字符（≤32 字符），作为图表序列名，需自行保证各客户端唯一。
`ts` 可选（Unix 秒，须在 now-24h ~ now+5min 内），缺省用服务端当前时间。

### 客户端脚本

仓库 `scripts/latency-client.sh`（依赖 bash/curl/awk）已实现「预热丢弃握手 → keep-alive
串行测 N 次 → 取 min/avg/stddev → 上报」，配 cron/launchd 每 60s 一轮即可：

```bash
MON_URL=http://<vps-ip>:9100 CLIENT_NAME=home-mac REPORT_TOKEN=xxx ./latency-client.sh
```

> ⚠️ **必须直连 VPS，不能用 Cloudflare Tunnel 域名。** 经 Tunnel 测得的是「客户端→CF
> 边缘」的 RTT（anycast 就近接入，数值漂亮但无意义），非到 VPS 的端到端延迟。这需要把
> `HELIOX_MON_LISTEN` 放开到 `0.0.0.0:9100` 或加防火墙放行；不愿暴露端口者可退化为客户端
> 自行 `ping <vps-ip>` 后仅调用上报接口，echo 是可选增强。

---

## 多 VPS 部署

```bash
for vps in vps-la vps-tyo vps-hk; do
  ssh root@$vps 'cd ~/heliox && git pull && sudo ./deploy.sh monitor install && sudo ./deploy.sh monitor start'
done
```

每台 VPS 的 `SERVER_NAME` 自动使用主机名区分。

---

## 更新

```bash
cd ~/heliox && git pull
sudo ./deploy.sh monitor update
```

---

## 端口流量监控

支持按端口（如 Snell、VLESS）分别统计流量，显示各协议的今日/昨日/本月使用量。

### 工作原理

使用 iptables 计数器统计端口流量：

- 创建 `HELIOX_STATS` 链统计进出流量
- 按端口分别记录上行（TX）和下行（RX）
- 同时统计 TCP/UDP
- 每秒采集快照，每分钟汇总到日统计

### 自动配置

**无需手动操作。** 执行 `./deploy.sh monitor install` 时自动完成以下配置：

1. 生成 iptables 规则脚本 `/opt/heliox-mon/setup-iptables.sh`
2. 配置 systemd `ExecStartPre` 自动恢复规则
3. **服务器重启后自动恢复**，无需 rc.local 或 iptables-persistent

验证规则：

```bash
iptables -L HELIOX_STATS -n -v
```

### 配置端口

端口从 Heliox 的 `/opt/heliox/.env` 文件自动读取：

| 变量         | 说明       | 示例  |
| ------------ | ---------- | ----- |
| `SNELL_PORT` | Snell 端口 | 36890 |
| `VLESS_PORT` | VLESS 端口 | 443   |

### 数据持久化

- 流量快照保留从昨日 00:00 起（确保昨日统计完整）
- 流量日统计永久保存在 SQLite 数据库
- 延迟数据：原始记录（每分钟）保留 7 天，更早的数据按 10 分钟粒度降采样并保留 90 天（保留长期趋势的同时控制体积）
- 系统资源指标仅保留最近 1 小时（用于实时展示）
- 重启服务不影响历史数据（计数器偏移量自动恢复，避免统计跳变）

> 流量统计仅采集物理网卡，自动排除 `lo`、容器网桥（docker/br-/veth）及隧道接口（tun/wg/cloudflared/tailscale 等），避免代理流量被重复计入。

---

## 开发

```bash
make dev      # 本地构建（当前平台）-> build/heliox-mon
make build    # 生产构建 Linux/amd64（CGO_ENABLED=0）
make release  # 同时构建 Linux amd64 + arm64
make test     # 单元测试
make lint     # golangci-lint（本机建议 GOOS=linux 跑，避免跨平台假阳性）

# 本地运行（必须设置密码；Mac 上自动产生 mock 数据）
HELIOX_MON_PASS=test go run ./cmd/heliox-mon
```

跨平台采集逻辑按 build tag 分离（`*_linux.go` 真实采集 / `collector_darwin.go` mock），
改采集代码需同步两端，并跑 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` 验证。
push/PR 会触发 CI（lint、vet、test、多平台构建、govulncheck）。详见 [`CLAUDE.md`](CLAUDE.md)。

---

## 更新日志

见 [`CHANGELOG.md`](CHANGELOG.md)。

---

## License

[MIT](LICENSE)
