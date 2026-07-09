#!/usr/bin/env bash
# 客户端延迟测量并上报 heliox-mon。依赖: bash, curl, awk
#
# 用法:
#   MON_URL=http://vps-ip:9100 CLIENT_NAME=home-mac REPORT_TOKEN=xxx ./latency-client.sh
#
# 可选环境变量:
#   SAMPLES  单轮有效测量次数（默认 10）
#
# 注意: MON_URL 必须为 VPS 直连地址，不要用 Cloudflare Tunnel 域名——
#       经 Tunnel 测得的是「客户端→CF 边缘」RTT，而非到 VPS 的端到端延迟。
set -euo pipefail
: "${MON_URL:?需设置 MON_URL}" "${CLIENT_NAME:?需设置 CLIENT_NAME}" "${REPORT_TOKEN:?需设置 REPORT_TOKEN}"
N="${SAMPLES:-10}"

# 多发一次用于预热（含 TCP/TLS 握手，不代表净 RTT，稍后丢弃第 1 行）
urls=()
for _ in $(seq $((N + 1))); do urls+=("$MON_URL/api/echo"); done

# 单 curl 进程多 URL 自动复用 keep-alive 连接；-w 每个 URL 输出一行耗时（秒）
times=$(curl -s -o /dev/null --max-time 5 -w '%{time_total}\n' "${urls[@]}" || true)

# 丢弃第 1 行（预热/握手），秒转毫秒，计算 avg/min/stddev 与丢包数
stats=$(echo "$times" | tail -n +2 | awk -v n="$N" '
  $1 > 0 { ms = $1 * 1000; s += ms; ss += ms * ms; c++; if (min == "" || ms < min) min = ms }
  END {
    if (c == 0) { printf "null null null %d", n; exit }
    avg = s / c; sd = (c > 1) ? sqrt(ss / c - avg * avg) : 0
    printf "%.2f %.2f %.2f %d", avg, min, sd, n - c
  }')
read -r avg min mdev lost <<< "$stats"

curl -sf -X POST "$MON_URL/api/latency/report" \
  -H "Authorization: Bearer $REPORT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"client\":\"$CLIENT_NAME\",\"samples\":[{\"rtt_ms\":$avg,\"min_rtt\":$min,\"mdev\":$mdev,\"sent\":$N,\"lost\":$lost}]}"
