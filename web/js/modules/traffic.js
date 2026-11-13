// 流量统计：本计费周期总量 + 计费配额进度、按协议明细表
// this_month 这个字段名是历史遗留，后端实际按 reset_day 起算的计费周期聚合

import { $, escapeHtml, setHtml, setText } from "../core/dom.js";
import {
  formatBytes,
  formatBytesParts,
  formatDateValue,
} from "../core/format.js";
import { getJSON, logFetchError } from "../core/http.js";

const GB = 1024 * 1024 * 1024;

const BILLING_MODE_TEXT = {
  bidirectional: "双向计费",
  tx_only: "仅出站 (TX)",
  rx_only: "仅入站 (RX)",
  max_value: "取最大值 (Max)",
};

const DEFAULT_THRESHOLDS = [80, 90, 95];

export function initTraffic() {
  refreshTraffic();
}

// 获取仪表盘数据
export async function refreshTraffic() {
  try {
    const data = await getJSON("/api/stats");

    document.title = data.server_name;
    // 只写文本节点，胶囊里的铃铛图标不能被整体覆盖掉
    setText("server-name-text", data.server_name);
    $("current-time").textContent = data.current_time;

    const cycle = computeCycle(data.reset_day);
    const monthTotalBytes = data.this_month.tx + data.this_month.rx;

    setText(
      "billing-mode-pill",
      `计费 ${BILLING_MODE_TEXT[data.billing_mode] || data.billing_mode}`,
    );
    setText(
      "cycle-pill",
      `重置日 ${cycle.day} 号 · 剩余 ${cycle.remainDays} 天`,
    );
    setText("cycle-start", `${formatDateValue(cycle.start)} 起`);

    const [monthValue, monthUnit] = formatBytesParts(monthTotalBytes);
    setText("month-total", monthValue);
    setText("month-total-unit", monthUnit);

    renderQuota(data, cycle);
    renderTotalRow(data);

    // 端口明细单独一个接口，失败不应连累上面已渲染好的整机数据
    fetchPortTraffic(monthTotalBytes);
  } catch (e) {
    logFetchError("获取统计数据失败:", e);
  }
}

/**
 * 计费周期在前端按 reset_day 推算，与后端 GetBillingCycleDates 同口径：
 * 重置日钳到 1-28，保证任意月份都存在该日历日。
 * @param {number} resetDay
 */
function computeCycle(resetDay) {
  const day = Math.min(28, Math.max(1, Number(resetDay) || 1));
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const start =
    today.getDate() >= day
      ? new Date(today.getFullYear(), today.getMonth(), day)
      : new Date(today.getFullYear(), today.getMonth() - 1, day);
  const next = new Date(start.getFullYear(), start.getMonth() + 1, day);

  const totalDays = daysBetween(start, next);
  // 「剩余」含今天：08-02 看 08-12 重置就是剩 10 天
  const remainDays = Math.max(1, daysBetween(today, next));
  return {
    day,
    start,
    totalDays,
    remainDays,
    elapsedDays: Math.max(1, totalDays - remainDays + 1),
  };
}

// 两个本地午夜跨夏令时可能差不满整数天，取整还原日历日差
/** @param {Date} a @param {Date} b */
function daysBetween(a, b) {
  return Math.round((b.getTime() - a.getTime()) / 86400000);
}

// 配额区间的数字统一按 GB 展示：大额度看整数更清楚，小额度才需要两位小数
/** @param {number} gb */
function formatGB(gb) {
  return gb >= 100 ? Math.round(gb).toString() : gb.toFixed(2);
}

/** @param {any} data @param {ReturnType<typeof computeCycle>} cycle */
function renderQuota(data, cycle) {
  const limitGB = Number(data.monthly_limit_gb) || 0;
  const usedGB = data.used_bytes / GB;
  const dailyGB = usedGB / cycle.elapsedDays;
  const forecastGB = dailyGB * cycle.totalDays;

  const thresholds = (
    data.alert_thresholds && data.alert_thresholds.length > 0
      ? data.alert_thresholds
      : DEFAULT_THRESHOLDS
  )
    .filter((/** @type {number} */ t) => t > 0 && t < 100)
    .sort((/** @type {number} */ a, /** @type {number} */ b) => a - b);

  const block = $("quota-block");
  // 未配置额度时整块配额条无意义，但日均/预估仍按计费口径成立
  if (block) block.hidden = limitGB <= 0;

  if (limitGB > 0) {
    const percent = (usedGB / limitGB) * 100;
    setText("quota-used", formatGB(usedGB));
    setText("quota-limit", formatGB(limitGB));
    setText("quota-percent", `${percent.toFixed(1)}%`);
    setText("quota-remain", formatGB(Math.max(0, limitGB - usedGB)));
    setText("quota-remain-unit", "GB");
    setText(
      "quota-legend",
      thresholds.length > 0 ? `${thresholds.join(" · ")} 预警` : "",
    );

    const fill = $("quota-fill");
    if (fill) {
      fill.style.width = `${Math.min(percent, 100).toFixed(2)}%`;
      // 越过最低预警阈值即转红，与 Telegram 预警的触发口径一致
      fill.classList.toggle(
        "is-danger",
        thresholds.length > 0 && percent >= thresholds[0],
      );
    }
    renderThresholdMarks(thresholds);
  } else {
    setText("quota-remain", "--");
    setText("quota-remain-unit", "");
  }

  setText("daily-avg", formatGB(dailyGB));
  setText("cycle-forecast", formatGB(forecastGB));

  const forecastBox = $("cycle-forecast-box");
  if (forecastBox) {
    const forecastPercent = limitGB > 0 ? (forecastGB / limitGB) * 100 : 0;
    forecastBox.classList.toggle(
      "is-ok",
      limitGB > 0 &&
        (thresholds.length === 0 || forecastPercent < thresholds[0]),
    );
    forecastBox.classList.toggle(
      "is-warn",
      limitGB > 0 &&
        thresholds.length > 0 &&
        forecastPercent >= thresholds[0] &&
        forecastPercent < 100,
    );
    forecastBox.classList.toggle(
      "is-danger",
      limitGB > 0 && forecastPercent >= 100,
    );
  }
}

/** @param {number[]} thresholds */
function renderThresholdMarks(thresholds) {
  const marks = $("quota-marks");
  if (!marks) return;
  marks.innerHTML = "";
  thresholds.forEach((t) => {
    const mark = document.createElement("div");
    mark.className = "tf-quota-mark";
    mark.style.left = `${t}%`;
    mark.title = `预警阈值: ${t}%`;
    marks.appendChild(mark);
  });
}

// 明细表的「总计」行走整机口径，端口行只覆盖 iptables 记账的那几个端口
/** @param {any} data */
function renderTotalRow(data) {
  const periods = /** @type {const} */ (["today", "yesterday"]);
  periods.forEach((period) => {
    const d = data[period];
    setText(`total-${period}-tx`, `↑ ${formatBytes(d.tx)}`);
    setText(`total-${period}-rx`, `↓ ${formatBytes(d.rx)}`);
    setText(`total-${period}-sum`, `⇅ ${formatBytes(d.tx + d.rx)}`);
  });

  const [value, unit] = formatBytesParts(
    data.this_month.tx + data.this_month.rx,
  );
  setText("total-month", value);
  setText("total-month-unit", unit);
}

// 获取端口流量
/** @param {number} monthTotalBytes */
async function fetchPortTraffic(monthTotalBytes) {
  try {
    const data = await getJSON("/api/traffic/ports");

    if (!data.ports || data.ports.length === 0) {
      setHtml("port-rows", '<div class="pt-note">暂无端口流量数据</div>');
      return;
    }

    // 检查 iptables 规则状态
    if (data.iptables_ok === false) {
      setHtml(
        "port-rows",
        '<div class="pt-note">⚠️ 端口统计规则不完整（iptables / ip6tables），请执行 sudo ./deploy.sh monitor restart</div>',
      );
      return;
    }

    renderPortRows(data.ports, monthTotalBytes);
  } catch (e) {
    logFetchError("获取端口流量失败:", e);
  }
}

/** @param {any[]} ports @param {number} monthTotalBytes */
function renderPortRows(ports, monthTotalBytes) {
  setHtml(
    "port-rows",
    ports
      .map((p) => {
        const [monthValue, monthUnit] = formatBytesParts(p.this_month.total);
        // 占比条量的是该协议占整机本周期流量的比重，所以分母用整机口径
        const share =
          monthTotalBytes > 0
            ? (p.this_month.total / monthTotalBytes) * 100
            : 0;
        return `
      <div class="pt-row">
        <span class="pt-proto">${escapeHtml(p.name.toLowerCase())}</span>
        <div class="pt-cell">
          <div class="stat-up">↑ ${formatBytes(p.today.tx)}</div>
          <div class="stat-down">↓ ${formatBytes(p.today.rx)}</div>
        </div>
        <div class="pt-cell">
          <div class="stat-up">↑ ${formatBytes(p.yesterday.tx)}</div>
          <div class="stat-down">↓ ${formatBytes(p.yesterday.rx)}</div>
        </div>
        <div class="pt-right">
          <div class="pt-month">
            <span>${monthValue}</span> <span class="pt-unit">${monthUnit}</span>
          </div>
          <div class="pt-share">
            <div class="pt-share-fill" style="width:${Math.min(share, 100).toFixed(1)}%"></div>
          </div>
        </div>
      </div>
    `;
      })
      .join(""),
  );
}
