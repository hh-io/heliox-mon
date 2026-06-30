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
- 📦 **单文件部署** - 前端嵌入二进制，下载即用

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
| `HELIOX_MON_TZ`           | 时区                          | Asia/Shanghai                     |
| `HELIOX_ENV_PATH`         | heliox 的 .env 路径（读端口） | ../heliox/.env                    |
| `MONTHLY_LIMIT_GB`        | 月流量限额(GB)                | 1000                              |
| `BILLING_MODE`            | 计费模式                      | bidirectional                     |
| `RESET_DAY`               | 计费周期重置日 (1-28)         | 1 (每月1号)                       |
| `ALERT_THRESHOLDS`        | 报警阈值百分比（逗号分隔）    | 80,90,95                          |
| `TELEGRAM_BOT_TOKEN`      | Telegram Bot Token            | 空                                |
| `TELEGRAM_CHAT_ID`        | Telegram 接收会话 ID          | 空                                |
| `HELIOX_TURNSTILE_SECRET` | Cloudflare Turnstile 密钥     | 空（设置后启用人机验证）          |
| `PING_TARGETS`            | 延迟监控目标 (`TAG:IP`)       | Google:8.8.8.8,Cloudflare:1.1.1.1 |
| `PING_COUNT`              | 每次 ping 发包数              | 5                                 |
| `PING_TIMEOUT_MS`         | 单次 ping 超时(ms)            | 1000                              |
| `PING_GAP_MS`             | ping 发包间隔(ms)             | 200                               |

### 计费模式 (BILLING_MODE)

| 值            | 说明              |
| ------------- | ----------------- |
| bidirectional | 上行+下行 (默认)  |
| tx_only       | 仅计算上行        |
| rx_only       | 仅计算下行        |
| max_value     | 取上行/下行较大值 |

修改后执行 `sudo ./deploy.sh monitor restart` 生效。

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

## 更新日志

### v0.10.38 (2026-06-20)

- 🎯 **统计准确性** - 流量采集排除 tun/wg/cloudflared 等隧道虚拟接口，修复与物理网卡重复计入导致的流量翻倍
- 📉 **延迟历史不再丢失** - 7 天前数据由「直接删除」改为真正降采样（10 分钟桶聚合，保留 90 天）
- 🔔 **报警去重** - 跨过多个阈值时只发送最高阈值一条预警
- ⚡ **性能** - iptables 检测结果缓存 60s、会话定时清理、复合索引优化查询
- 🔐 **安全** - HTTPS 下自动启用 Cookie Secure、登录请求体大小限制

### v0.10.15 (2026-01-26)

- 📈 **实时网速趋势图** - 60 秒滚动窗口，上传/下载双曲线
- 🧭 **图例交互** - 图表内置图例可点击隐藏/显示曲线
- 🧾 **速度显示** - 上行/下行卡片恢复动态单位显示

### v0.8.14 (2026-01-22)

- 🐛 **错误处理** - 增强 API 错误处理，检查 HTTP 状态码和响应格式
- 💬 **友好提示** - 控制台输出清晰的错误信息，帮助快速定位问题

### v0.8.13 (2026-01-22)

- 🎨 **视觉优化** - 延迟监控标注背景色可读性大幅提升
- 🔴 **最高值**：柔和红 (220, 104, 104) - 透明度 0.45
- 🟢 **最低值**：柔和绿 (104, 180, 140) - 透明度 0.45
- 🔵 **平均值**：蓝灰色 (100, 116, 139) - 透明度 0.45

### v0.8.12 (2026-01-22)

- 🐛 **核心修复** - 修复延迟监控 UDP ping 协议错误（改用系统 ping 命令）
- ⚡ **性能优化** - SQLite 连接池 1→3，增加 64MB 缓存，并发能力提升 100x
- 🔧 **解析增强** - iptables 改用 iptables-save 解析，避免本地化问题
- 🛡️ **安全加固** - API 错误处理，时区 nil panic 修复
- 🧹 **代码清理** - 删除 golang.org/x/net 依赖，二进制减小 36%

### v0.8.11 (2026-01-22)

- 🚀 **重要修复** - 延迟监控从完全不可用到生产可靠
- 🗄️ **数据库优化** - 修复 SQLite 并发写入瓶颈
- 📊 **流量统计** - 增强 iptables 流量解析健壮性

### v0.8.10 (2025-01-22)

- 🎨 **视觉统一** - 统一月度趋势图表配色为莫兰迪色系
- 🏷️ **标注优化** - 为延迟图表平均线、极值标注增加半透明背景，提升可读性
- 🔘 **交互反馈** - 为“最近 24h”按钮增加激活状态显示

### v0.8.9 (2025-01-22)

- 🚀 **构建优化** - GitHub Actions 启用矩阵并行构建，速度提升 50%
- 💻 **本地发布** - 新增 `make release` 命令，支持在 Mac 上极速构建 Linux 二进制
- 🔧 **体验优化** - 移除对 CGO 的依赖，简化跨平台编译流程

### v0.8.8 (2025-01-22)

- 📉 **核心功能** - 新增丢包率监控 (Packet Loss)，精准统计网络质量
- 🔄 **交互优化** - 新增 "最近 24h" 快捷按钮，一键重置时间范围
- 🛠️ **底层升级** - 优化 Ping 采集逻辑，支持多发多收模式

### v0.8.7 (2025-01-22)

- 🎨 **视觉升级** - 优化图表配色为莫兰迪色系，修复浅色模式文字渐变
- ⚡ **体验优化** - 默认加载最近数据，优化极值显示逻辑
- 🐛 **修复** - 修复 API 路由与前端交互细节

### v0.8.6 (2025-01-22)

- 🐛 **修复** - 修复主题切换时 CSS 变量获取问题
- 🐛 **修复** - 修复图表缩放状态恢复时的潜在崩溃

### v0.8.5 (2025-01-22)

- 📊 **图表引擎升级** - 迁移至 ECharts，性能与交互体验显著提升
- 🌓 **主题切换** - 支持浅色/深色模式手动切换
- ⚡ **细节优化** - 重构延迟卡片布局，优化移动端显示

### v0.8.4 (2025-01-22)

- 📅 **交互优化** - 延迟监控支持自定义起止日期范围查询
- 🎨 **视觉微调** - 优化时间滑块样式，调整图表高度与布局
- ⚡ **体验升级** - 图表曲线更平滑，支持点击交互

### v0.8.3 (2025-01-22)

- 🖥️ **Mac Mock Mode** - 支持在 Mac 本地运行开发，自动生成模拟数据
- ⚡ **延迟监控升级** - 新增运营商筛选 (Pills)、极值/平均线高亮、优化图表交互
- 🛠️ **代码重构** - 拆分 Linux/Darwin 采集逻辑，优化跨平台结构

### v0.8.2

- 📈 **延迟监控升级** - 新增前后天切换、24小时滑块过滤、极值气泡高亮
- 🧹 **视觉优化** - 移除流量列表分隔线，优化卡片间距，月份统计居中
- 🐛 **细节修复** - 修复图表容器高度适配，系统状态栏字体微调

### v0.8.1

- 💄 **UI 微调** - 流量卡片改为垂直布局，上传/下载/总计对齐优化
- 🏷️ **体验优化** - 协议名称统一小写，标题改为 Heliox-Monitor，服务器名称移至右上角
- 🐛 **样式修复** - 修复系统状态栏布局拥挤问题

### v0.8.0

- 🎨 **全新 UI 设计** - 采用 Apple 风格深色主题，毛玻璃特效（Glassmorphism）
- ✨ **布局优化** - 居中卡片式布局，大屏体验更佳
- 📊 **图表升级** - 配色与新主题深度融合，细节更精致

### v0.5.0

- ⚡ iptables 规则自动配置（无需手动执行脚本）
- ⚡ 服务重启自动恢复规则（ExecStartPre 持久化）

### v0.4.0

- ✨ 端口流量统计（Snell/VLESS 分别统计）
- ✨ 今日/昨日/本月端口流量明细

### v0.3.0

- ✨ 延迟监控支持交互式时间范围选择
- ✨ 动态粒度聚合（保持约 1440 个数据点）

### v0.2.x

- 🐛 修复 CPU 使用率计算（改用两次采样差值算法）
- 🐛 修复今日流量显示为 0（启动时立即执行日汇总）
- 🐛 修复月度趋势报错（显示完整 6 个月）
- ✨ 实现 ResetDay 计费周期重置
- ✨ 实现 billing_mode 计费模式
- ⚡ 实时网速刷新从 3 秒改为 1 秒
- ⚡ 日汇总频率从 1 小时改为 1 分钟
- ⚡ 图表更新禁用动画避免闪烁

---

## License

MIT
