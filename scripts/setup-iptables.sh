#!/bin/bash
# Heliox Monitor - 端口流量统计规则设置脚本（IPv4 + IPv6）
# 用法: sudo ./setup-iptables.sh          建立/重建规则
#       sudo ./setup-iptables.sh --clean  删除全部规则（卸载用）
#
# 正式部署由 heliox 的 deploy.sh 生成同逻辑的 /opt/heliox-mon/setup-iptables.sh，
# 并在 systemd ExecStartPre 中执行；本脚本供手动部署或排障使用。

set -u

if [ "$EUID" -ne 0 ]; then
    echo "错误: 请使用 sudo 运行此脚本" >&2
    exit 1
fi

# 端口来源与 heliox-mon 进程保持一致：先读监控配置拿到 HELIOX_ENV_PATH，再读 heliox 的 .env
# shellcheck disable=SC1091
source /opt/heliox-mon/.env 2>/dev/null || true
if [[ -n "${HELIOX_ENV_PATH:-}" && -f "$HELIOX_ENV_PATH" ]]; then
    # shellcheck disable=SC1090
    source "$HELIOX_ENV_PATH"
else
    # shellcheck disable=SC1091
    source /opt/heliox/.env 2>/dev/null || source /root/heliox/.env 2>/dev/null || true
fi

# 默认值必须与 internal/config 的 loadHelioxEnv 一致，否则规则建在 A 端口、程序去读 B 端口
# 启用判定与 deploy.sh 其余部分一致：变量存在时只有字面量 true 算启用
PORTS=()
[[ "${ENABLE_SNELL:-true}" == "true" ]] && PORTS+=("${SNELL_PORT:-36890}")
[[ "${ENABLE_XRAY:-true}" == "true" ]] && PORTS+=("${VLESS_PORT:-443}")

clean_rules() {
    local bin=$1
    # 跳转规则可能因重复执行而存在多条，删到删不动为止
    while "$bin" -w -D INPUT -j HELIOX_STATS 2>/dev/null; do :; done
    while "$bin" -w -D OUTPUT -j HELIOX_STATS 2>/dev/null; do :; done
    "$bin" -w -F HELIOX_STATS 2>/dev/null || true
    "$bin" -w -X HELIOX_STATS 2>/dev/null || true
}

apply_rules() {
    local bin=$1 port proto
    clean_rules "$bin"
    "$bin" -w -N HELIOX_STATS || return 1
    "$bin" -w -I INPUT -j HELIOX_STATS || return 1
    "$bin" -w -I OUTPUT -j HELIOX_STATS || return 1
    # ${arr[@]+...} 写法：bash < 4.4 在 set -u 下展开空数组会报 unbound variable
    for port in ${PORTS[@]+"${PORTS[@]}"}; do
        [[ "$port" == "0" ]] && continue
        for proto in tcp udp; do
            "$bin" -w -A HELIOX_STATS -p "$proto" --sport "$port" || return 1  # TX
            "$bin" -w -A HELIOX_STATS -p "$proto" --dport "$port" || return 1  # RX
        done
    done
}

IPTABLES=$(command -v iptables || echo /usr/sbin/iptables)
IP6TABLES=$(command -v ip6tables || echo /usr/sbin/ip6tables)

if [[ "${1:-}" == "--clean" ]]; then
    [[ -x "$IPTABLES" ]] && clean_rules "$IPTABLES"
    [[ -x "$IP6TABLES" ]] && clean_rules "$IP6TABLES"
    echo "HELIOX_STATS 规则已清理"
    exit 0
fi

# 规则失败只告警不退出非零：本脚本在 ExecStartPre 中执行，失败会让整个监控起不来，
# 端口统计缺失由页面的规则检测提示即可
FAILED=0
if [[ ! -x "$IPTABLES" ]]; then
    echo "警告: 找不到 iptables，端口流量统计不可用（apt install iptables）" >&2
    FAILED=1
elif ! apply_rules "$IPTABLES"; then
    echo "警告: IPv4 端口统计规则设置失败" >&2
    FAILED=1
fi

# xray 监听 "::"，IPv6 客户端的流量只在 ip6tables 里有计数。
# 内核没有 IPv6 协议栈时不存在 IPv6 流量，跳过即可（判定口径与 heliox-mon 的规则检测一致）
if [[ ! -e /proc/net/if_inet6 ]]; then
    echo "提示: 内核未启用 IPv6，跳过 IPv6 端口统计规则"
elif [[ ! -x "$IP6TABLES" ]]; then
    echo "警告: 找不到 ip6tables，IPv6 端口流量将不被统计" >&2
    FAILED=1
elif ! apply_rules "$IP6TABLES"; then
    echo "警告: IPv6 端口统计规则设置失败" >&2
    FAILED=1
fi

if [[ "$FAILED" == "0" ]]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] HELIOX_STATS 规则已就绪 (端口: ${PORTS[*]:-无})"
else
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] HELIOX_STATS 规则未完整建立 (端口: ${PORTS[*]:-无})，见上方警告" >&2
fi
exit 0
