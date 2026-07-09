# 设计方案：客户端延迟上报 + Echo 高精度测延接口

> 状态：设计定稿，待实现。本文档面向实现者（Claude Opus），所有代码位置均为当前 main 分支。
> 版本影响：新增功能，发版走 **minor**。

## 1. 背景与目标

当前延迟监控只有服务端主动探测（Pull）：`internal/collector/latency_linux.go` 按
`PING_TARGETS` 周期 ping 固定公网目标，写入 `latency_records`，前端渲染类 Smokeping 趋势图。

它测不了**用户侧到 VPS 的方向**：客户端（家宽/办公网）在 NAT 后，服务端无法反向探测。
而"我到 VPS 的真实体验"恰恰是代理服务用户最关心的指标。

**目标：**

1. 客户端主动上报（Push）：新增上报 API，客户端把测得的 RTT 样本推给服务端；数据
   完全复用现有 `latency_records` 表、10 分钟桶降采样、7 天/90 天保留策略与前端图表，
   效果等价于在 `PING_TARGETS` 里"无缝多了一项"。
2. Echo 测延 API：极简、无阻塞、免认证的 HTTP 204 端点，供客户端在 keep-alive 连接上
   测量端到端 TCP RTT，排除业务逻辑耗时。

**非目标：**

- 不做多服务端聚合、不做客户端管理界面/注册流程。
- 不保证上报幂等（重复上报同一时间点会产生重复行，聚合时被 AVG 平均，可接受）。
- 不做 UDP echo（防火墙/部署复杂度不值得，见 §6.1）。

## 2. 总体架构

```
┌────────── 客户端（Mac/家宽等） ──────────┐
│ 测量脚本（cron/launchd 周期运行）          │
│  1. GET /api/echo ×1   预热（含握手，丢弃） │
│  2. GET /api/echo ×N   同连接串行测时       │
│  3. min→净RTT  avg  stddev→mdev  失败→lost │
│  4. POST /api/latency/report (Bearer token)│
└──────────────┬───────────────────────────┘
               │ 必须直连 VPS（见 §6.2）
               ▼
┌────────────── heliox-mon 服务端 ──────────────┐
│ /api/echo            免认证，204，不写库不打日志 │
│ /api/latency/report  Bearer token 校验          │
│        └─ INSERT INTO latency_records           │
│           target = "client:<name>"              │
│                                                 │
│ 以下全部【零改动】自动生效：                      │
│  · aggregateLatencyData  按 target 泛化降采样    │
│  · 7 天原始 / 90 天聚合保留策略                   │
│  · 前端图表/筛选框（纯由 API targets 数组驱动）    │
│                                                 │
│ /api/latency  唯一需扩展点：target 列表 =        │
│   cfg.PingTargets + DISTINCT client:* 目标       │
└─────────────────────────────────────────────────┘
```

## 3. 详细设计

### 3.1 配置（`internal/config/config.go`）

新增一个环境变量：

| 变量 | 说明 | 默认 |
|---|---|---|
| `HELIOX_MON_REPORT_TOKEN` | 客户端上报令牌。**未设置则上报功能关闭**（`/api/latency/report` 返回 403） | 空 |

- `Config` 增加字段 `ReportToken string`，`Load()` 里 `getEnv("HELIOX_MON_REPORT_TOKEN", "")`。
- **不复用管理员密码做上报认证**：客户端机器上放的凭据只应有"写入延迟数据"这一种权限，
  泄露后不能登录面板。
- 同步更新 `.env.example` 与 README 环境变量表（CLAUDE.md 的既有约定）。

### 3.2 数据模型（零 schema 变更）

复用 `latency_records`，用 `target` 列的**命名空间前缀**区分来源：

- 服务端 ping：`target = <IP>`（现状不变）。
- 客户端上报：`target = "client:<name>"`，如 `client:home-mac`。

前缀保证两点：① 不与 IP 目标冲突；② 客户端无法通过起同名 target 伪造/污染服务端
ping 目标的数据。展示层 tag 为去掉前缀的 `<name>`。

降采样（`aggregateLatencyData`，`internal/collector/collector.go:344`）是
`GROUP BY target, bucket`，对新 target 自动生效，**不改**。

### 3.3 API：`POST /api/latency/report`

注册在 `NewServer` 的 mux 上，**不套 `s.auth`**（有独立 Bearer 认证）。

**认证**：`Authorization: Bearer <HELIOX_MON_REPORT_TOKEN>`，用
`subtle.ConstantTimeCompare` 比较（与 `checkCredentials` 同风格）。
- 服务端未配置 token → `403 {"ok":false,"message":"上报功能未启用"}`。
- token 不匹配/缺失 → `401`。

**请求体**（`http.MaxBytesReader` 限 64KB）：

```json
{
  "client": "home-mac",
  "samples": [
    {
      "ts": 1751990400,        // 可选，Unix 秒；缺省用服务端当前时间
      "rtt_ms": 45.2,          // 平均 RTT，可为 null（全丢包）
      "min_rtt": 42.1,         // 可选
      "mdev": 3.4,             // 可选，抖动
      "sent": 10,
      "lost": 0
    }
  ]
}
```

**校验规则**（任一不满足 → `400` + 具体原因；不静默丢弃）：

- `client` 匹配 `^[A-Za-z0-9_-]{1,32}$`（防注入/防超长；同名 client 视为同一序列，文档写明需自行保证唯一）。
- `samples` 长度 1–100（允许断网期间小批量补报）。
- `ts` 若提供，须落在 `[now-24h, now+5min]`；超窗拒绝该请求（防时钟错乱污染图表）。
- `rtt_ms`/`min_rtt`/`mdev`：null 或 `[0, 60000)` 的数值。
- `sent`∈[1,1000]，`lost`∈[0,sent]。

**写入**：与 `doCollectLatency`（`latency_linux.go:46`）完全同构：

```sql
INSERT INTO latency_records (ts, target, rtt_ms, min_rtt, mdev, sent, lost, is_aggregated)
VALUES (?, 'client:<name>', ?, ?, ?, ?, ?, 0)
```

多条样本用单事务批量插入。成功返回 `200 {"ok":true,"accepted":N}`。
只接受 `POST`，其余方法 405。响应统一走 `writeJSON`/`writeJSONStatus`。

### 3.4 API：`GET /api/echo`

**Handler 全文（刻意保持这么小，不要加任何东西）：**

```go
// handleEcho 客户端测延用的极简回显端点：免认证、不写库、不打日志，
// 保证服务端耗时相对毫秒级网络 RTT 可忽略（微秒级）
func handleEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
```

- 注册时**不套 `s.auth`**：`mux.HandleFunc("/api/echo", handleEcho)`。免认证是刻意的——
  它比走一遍 auth 中间件返回 401 更便宜，不构成额外攻击面（无状态、无 IO、无信息泄露）。
- 支持 GET/HEAD 即可，不必显式拒其他方法（响应对任何方法都无副作用）。
- 不打访问日志：客户端每轮会打 10+ 次，打日志既拖慢又刷屏。

### 3.5 查询端扩展：`handleLatency`（`internal/api/router.go:691`）

这是**唯一**需要改动的既有读路径。当前循环 `for _, pt := range s.cfg.PingTargets`。

改动：构造目标列表时，在配置目标之后追加查询窗口内出现过的客户端目标：

```sql
SELECT DISTINCT target FROM latency_records
WHERE target LIKE 'client:%' AND ts >= ? AND ts <= ?
ORDER BY target
```

每个结果映射为 `{Tag: strings.TrimPrefix(t, "client:"), IP: t}` 追加到循环列表
（`IP` 字段即查询键，与现有 per-target SQL 完全复用；建议把循环体保持原样，只改
循环的输入切片）。`ORDER BY target` 保证图表颜色分配稳定。

**前端零改动**：`web/app.js` 的图表、筛选框、统计行全部由 `/api/latency` 返回的
`targets` 数组驱动（`renderFilterCheckboxes(latencyData.targets)`），新目标自动出现。
不改前端 ⇒ 本功能不依赖 `go:embed` 重新内嵌前端的注意事项，但发版仍需重新构建二进制。

### 3.6 客户端测量方法学 + 参考脚本

精度靠客户端方法学保证，服务端只提供"够便宜的靶子"：

1. **预热**：先发 1 次请求并丢弃结果——它包含 TCP（+TLS）握手，不代表净 RTT。
2. **同连接串行测量**：在同一 keep-alive 连接上串行发 N 次（默认 10）`GET /api/echo`，
   记录每次耗时。curl 单进程多 URL 会自动复用连接，天然满足。
3. **统计**：`min` ≈ 净 RTT（最接近链路传播延迟）、`avg` 作 rtt_ms、`stddev` 作 mdev；
   超时/失败计入 lost。语义与 ping 的 min/avg/mdev 对齐，图表无需区别对待。
4. **上报**：POST 到 `/api/latency/report`。

**参考实现**：新增 `scripts/latency-client.sh`（bash + curl，无其他依赖）：

```bash
#!/usr/bin/env bash
# 客户端延迟测量并上报 heliox-mon。依赖: bash, curl, awk
# 用法: MON_URL=http://vps-ip:9100 CLIENT_NAME=home-mac REPORT_TOKEN=xxx ./latency-client.sh
set -euo pipefail
: "${MON_URL:?}" "${CLIENT_NAME:?}" "${REPORT_TOKEN:?}"
N="${SAMPLES:-10}"

urls=(); for _ in $(seq $((N+1))); do urls+=("$MON_URL/api/echo"); done
# 单 curl 进程多 URL 复用连接；-w 每个 URL 输出一行耗时（秒）
times=$(curl -s -o /dev/null --max-time 5 -w '%{time_total}\n' "${urls[@]}" || true)

# 丢弃第 1 行（预热/握手），秒转毫秒，计算 min/avg/stddev/丢包
stats=$(echo "$times" | tail -n +2 | awk -v n="$N" '
  $1>0 { ms=$1*1000; s+=ms; ss+=ms*ms; c++; if(min==""||ms<min) min=ms }
  END {
    if (c==0) { printf "null null null %d", n; exit }
    avg=s/c; sd=(c>1)?sqrt(ss/c-avg*avg):0
    printf "%.2f %.2f %.2f %d", avg, min, sd, n-c
  }')
read -r avg min mdev lost <<< "$stats"

curl -sf -X POST "$MON_URL/api/latency/report" \
  -H "Authorization: Bearer $REPORT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"client\":\"$CLIENT_NAME\",\"samples\":[{\"rtt_ms\":$avg,\"min_rtt\":$min,\"mdev\":$mdev,\"sent\":$N,\"lost\":$lost}]}"
```

调度建议（写入 README）：cron/launchd 每 60s 一轮，与服务端 ping 周期一致，图表粒度自然对齐。

## 4. 安全考虑

- **凭据隔离**：上报 token 与管理员密码分离；token 泄露最多能写入伪造延迟数据，不能读任何数据、不能登录。
- **命名空间**：`client:` 前缀使上报方无法覆盖/伪装服务端 ping 目标序列。
- **输入约束**：client 名白名单正则、样本数/数值范围/时间窗校验、64KB body 上限，全部显式拒绝并回显原因（不静默吞）。
- **echo 无认证**：无状态无 IO，滥用成本 ≥ 服务端成本；且服务本身建议只经 CF Tunnel + 直连端口最小暴露。
- token 不出现在代码与日志中，走环境变量（`.env`）。

## 5. 实施清单（按序）

| # | 文件 | 改动 |
|---|---|---|
| 1 | `internal/config/config.go` | `ReportToken` 字段 + env 读取 |
| 2 | `internal/api/router.go` | `handleEcho`、`handleLatencyReport`（含 Bearer 校验、请求解析/校验、批量插入）、`handleLatency` 目标列表扩展、mux 注册两个新路由 |
| 3 | `scripts/latency-client.sh` | 参考客户端（§3.6），`chmod +x` |
| 4 | `.env.example`、README | 新增 `HELIOX_MON_REPORT_TOKEN` 说明、上报/echo API 文档、客户端脚本用法与 §6.2 直连要求 |
| 5 | `CHANGELOG.md` | `[Unreleased]` 记两条 feat |
| 6 | 测试 | 见下 |

**测试**（新增 `internal/api/router_test.go`，用 `httptest` + 临时目录 SQLite，参照 `storage/sqlite_test.go` 建库方式）：

- report：无 token 配置→403；错 token→401；合法请求→200 且落库行 `target='client:x'`、`is_aggregated=0`；非法 client 名/超窗 ts/越界数值→400；批量插入 accepted 数正确。
- echo：GET→204、无 body、`Cache-Control: no-store`；未认证可访问。
- handleLatency：库中存在 `client:x` 记录时，返回的 targets 含 `{tag:"x", ip:"client:x"}` 且在配置目标之后。
- 聚合复用：插入 8 天前的 `client:x` 原始记录，跑 `aggregateLatencyData`，确认生成 `is_aggregated=1` 行（可放 storage 或 collector 的平台无关测试中；注意 collector 现有测试文件的构建约束）。

**质量门禁**（CLAUDE.md 既有要求）：`make test`、`go vet ./...`、
`GOOS=linux golangci-lint run ./...`、`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`
全绿后提交。本功能不涉及 collector 平台分支（Linux/Darwin 两份 mock 均无需改动）。

## 6. 注意事项与已知取舍

### 6.1 为什么用 HTTP echo 而不是 ICMP / UDP / WebSocket

- 客户端直接 ping VPS 也可行（零服务端改动），但 ICMP 常被 ISP/机房 QoS 降级或屏蔽，
  且测的是 ICMP 路径；代理流量走 TCP，**TCP 路径 RTT 更能代表真实体验**。
- UDP echo 更"纯净"，但要新开端口、配防火墙、处理 NAT，运维成本远超收益。
- keep-alive 上的 HTTP GET，服务端处理耗时为微秒级，相对毫秒级 RTT 误差 <1%；
  再配合"取 min"统计，业务耗时干扰已被有效排除。

### 6.2 ⚠️ 必须直连，不能走 Cloudflare Tunnel 测延

本项目建议部署为 `127.0.0.1:9100` + CF Tunnel 对外。**若客户端经 Tunnel 域名访问
`/api/echo`，测得的是"客户端→CF 边缘"的 RTT**（CF anycast 就近接入，数值会漂亮但无意义），
不是到 VPS 的端到端延迟。

正确姿势（README 必须写明）：测延与上报使用 **VPS 直连地址**（`http://<vps-ip>:9100`）。
这要求把监听改为 `0.0.0.0:9100`（或加一条防火墙放行）——安全性由 report token +
既有认证保障；不愿暴露端口的用户可退化为只用上报（客户端自己 ping VPS IP 后上报），
echo 是可选增强。

### 6.3 数据语义

- 客户端上报的 `sent/lost` 是 HTTP 请求成败统计，与 ping 丢包语义近似但非严格等价，
  共享同一图表可接受。
- 客户端离线期间该序列出现数据空洞——前端已有 Smokeping 风格断线/灰色缺口标注
  （0.16.0 特性），自动正确呈现，无需处理。
- 上报不幂等：重复上报同 ts 产生重复行，聚合 AVG 会摊平；客户端侧避免重复调度即可。

## 7. 验收标准

1. 配置 `HELIOX_MON_REPORT_TOKEN` 后，在 Mac 上跑 `scripts/latency-client.sh` 指向
   本地 dev 实例（`HELIOX_MON_PASS=test go run ./cmd/heliox-mon`），
   刷新页面延迟图表出现名为 `<CLIENT_NAME>` 的新序列，筛选框可勾选、统计行数值合理。
2. 未配置 token 时 report 返回 403；echo 无需任何凭据返回 204。
3. §5 所列测试与质量门禁全部通过。
