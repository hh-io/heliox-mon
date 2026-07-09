// Heliox Monitor 前端

// 工具函数
function formatBytes(bytes) {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

function formatSpeed(bytesPerSec) {
  return formatSpeedParts(bytesPerSec).join(" ");
}

function formatSpeedParts(bytesPerSec) {
  if (bytesPerSec < 1024) return [bytesPerSec.toFixed(1), "B/s"];
  if (bytesPerSec < 1024 * 1024)
    return [(bytesPerSec / 1024).toFixed(1), "KB/s"];
  if (bytesPerSec < 1024 * 1024 * 1024)
    return [(bytesPerSec / 1024 / 1024).toFixed(2), "MB/s"];
  return [(bytesPerSec / 1024 / 1024 / 1024).toFixed(2), "GB/s"];
}

function formatTimeLabel(date) {
  const m = String(date.getMinutes()).padStart(2, "0");
  const s = String(date.getSeconds()).padStart(2, "0");
  return `${m}:${s}`;
}

// 转义用户/配置可控字符串，避免拼接进 innerHTML 时产生注入
function escapeHtml(value) {
  return String(value).replace(
    /[&<>"']/g,
    (c) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[c],
  );
}

function hexToRgba(hex, alpha) {
  const clean = hex.replace("#", "").trim();
  if (clean.length !== 6) return `rgba(0, 0, 0, ${alpha})`;
  const r = parseInt(clean.slice(0, 2), 16);
  const g = parseInt(clean.slice(2, 4), 16);
  const b = parseInt(clean.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

const SPEED_AXIS_UNITS = [
  { unit: "B/s", scale: 1 },
  { unit: "KB/s", scale: 1024 },
  { unit: "MB/s", scale: 1024 * 1024 },
  { unit: "GB/s", scale: 1024 * 1024 * 1024 },
  { unit: "TB/s", scale: 1024 * 1024 * 1024 * 1024 },
];

function getSpeedScale(maxBytesPerSec) {
  const max = Math.max(1, maxBytesPerSec);
  for (let i = 0; i < SPEED_AXIS_UNITS.length; i++) {
    const unit = SPEED_AXIS_UNITS[i];
    const maxInUnit = max / unit.scale;
    const niceMax = niceCeil(maxInUnit);
    if (niceMax < 1000 || i === SPEED_AXIS_UNITS.length - 1) {
      return {
        unit: unit.unit,
        scale: unit.scale,
        maxBytes: Math.round(niceMax * unit.scale),
      };
    }
  }
  return { unit: "B/s", scale: 1, maxBytes: Math.round(max) };
}

function niceCeil(value) {
  if (!value || value <= 0) return 1;
  const exp = Math.floor(Math.log10(value));
  const base = Math.pow(10, exp);
  const f = value / base;
  let nf = 10;
  if (f <= 1) nf = 1;
  else if (f <= 2) nf = 2;
  else if (f <= 5) nf = 5;
  return nf * base;
}

function formatAxisSpeed(value) {
  if (value <= 0) return "0B/s";
  for (let i = SPEED_AXIS_UNITS.length - 1; i >= 0; i--) {
    const unit = SPEED_AXIS_UNITS[i];
    if (value >= unit.scale) {
      const rounded = Math.round(value / unit.scale);
      return `${rounded}${unit.unit}`;
    }
  }
  return "0B/s";
}

// 获取仪表盘数据
async function fetchStats() {
  try {
    const res = await fetch("/api/stats");
    if (res.status === 401) {
      window.location.href = "/login";
      return;
    }
    const data = await res.json();

    document.title = data.server_name;
    const badge = document.getElementById("server-name");
    if (badge) badge.textContent = data.server_name;
    document.getElementById("current-time").textContent = data.current_time;

    // 流量数据
    const todayTxEl = document.getElementById("today-tx");
    const todayRxEl = document.getElementById("today-rx");
    const todayTotalEl = document.getElementById("today-total");
    if (todayTxEl) todayTxEl.textContent = `↑ ${formatBytes(data.today.tx)}`;
    if (todayRxEl) todayRxEl.textContent = `↓ ${formatBytes(data.today.rx)}`;
    if (todayTotalEl)
      todayTotalEl.textContent = `⇅ ${formatBytes(
        data.today.tx + data.today.rx,
      )}`;

    const yesterdayTxEl = document.getElementById("yesterday-tx");
    const yesterdayRxEl = document.getElementById("yesterday-rx");
    const yesterdayTotalEl = document.getElementById("yesterday-total");
    if (yesterdayTxEl)
      yesterdayTxEl.textContent = `↑ ${formatBytes(data.yesterday.tx)}`;
    if (yesterdayRxEl)
      yesterdayRxEl.textContent = `↓ ${formatBytes(data.yesterday.rx)}`;
    if (yesterdayTotalEl)
      yesterdayTotalEl.textContent = `⇅ ${formatBytes(
        data.yesterday.tx + data.yesterday.rx,
      )}`;

    // 本月总计
    const monthTotalBytes = data.this_month.tx + data.this_month.rx;
    const monthTotalGB = (monthTotalBytes / 1024 / 1024 / 1024).toFixed(2);
    document.getElementById("month-total").textContent = monthTotalGB + " GB";

    // 获取端口流量
    fetchPortTraffic();

    if (trendRange === "30d" || trendRange === "cycle") {
      fetchDailyTrend(trendRange);
    }

    // 渲染高级流量进度条
    renderTrafficProgress(data);
  } catch (e) {
    console.error("获取统计数据失败:", e);
  }
}

// 渲染流量进度条（支持双向/单向/刻度）
function renderTrafficProgress(data) {
  const limitGB = data.monthly_limit_gb;
  if (limitGB <= 0) return;

  const usedBytes = data.used_bytes;
  const usedGB = Math.round(usedBytes / 1024 / 1024 / 1024);
  const totalPercent = (usedBytes / (limitGB * 1024 * 1024 * 1024)) * 100;

  // 更新文本
  const usedEl = document.getElementById("quota-used");
  const limitEl = document.getElementById("quota-limit");
  const percentTextEl = document.getElementById("quota-percent-text");
  const badgeEl = document.getElementById("billing-mode-badge");
  const resetDayEl = document.getElementById("reset-day");

  if (usedEl) usedEl.textContent = usedGB;
  if (limitEl) limitEl.textContent = limitGB;
  if (percentTextEl) percentTextEl.textContent = `${totalPercent.toFixed(1)}%`;
  if (resetDayEl) resetDayEl.textContent = data.reset_day;

  // 更新 Badge
  if (badgeEl) {
    let modeText = data.billing_mode;
    if (modeText === "bidirectional") modeText = "双向计费";
    else if (modeText === "tx_only") modeText = "仅出站 (TX)";
    else if (modeText === "rx_only") modeText = "仅入站 (RX)";
    else if (modeText === "max_value") modeText = "取最大值 (Max)";
    badgeEl.textContent = modeText;
  }

  // 渲染进度条轨道
  const track = document.getElementById("progress-track");
  if (!track) return;
  track.innerHTML = ""; // 清空

  // 1. 添加刻度 (Threshold Markers)
  const thresholds =
    data.alert_thresholds && data.alert_thresholds.length > 0
      ? data.alert_thresholds
      : [80, 90, 95];
  thresholds.forEach((t) => {
    if (t > 0 && t < 100) {
      const marker = document.createElement("div");
      marker.className = "threshold-marker";
      marker.style.left = `${t}%`;
      marker.title = `预警阈值: ${t}%`;
      track.appendChild(marker);
    }
  });

  // 2. 计算分段
  const limitBytes = limitGB * 1024 * 1024 * 1024;
  let segments = [];
  let isDanger = false;

  // 检查是否超过最小阈值 (通常是第一个)
  const sortedThresholds = [...thresholds].sort((a, b) => a - b);
  if (sortedThresholds.length > 0 && totalPercent >= sortedThresholds[0]) {
    isDanger = true;
  }

  // 根据模式决定渲染段
  if (data.billing_mode === "bidirectional") {
    // 双向：分开显示 TX 和 RX
    const txPercent = (data.this_month.tx / limitBytes) * 100;
    const rxPercent = (data.this_month.rx / limitBytes) * 100;
    segments.push({ type: "tx", width: txPercent });
    segments.push({ type: "rx", width: rxPercent });
  } else {
    // 单向或其他：显示总计 (tx_only, rx_only, max_value)
    // 注意：max_value 模式下 used_bytes 已是 max(tx, rx)
    segments.push({ type: "total", width: totalPercent });
  }

  // 3. 渲染分段
  segments.forEach((seg) => {
    const div = document.createElement("div");
    div.className = `progress-bar-segment segment-${seg.type}`;
    div.style.width = `${Math.min(seg.width, 100).toFixed(2)}%`; // 防止溢出视觉
    track.appendChild(div);
  });

  // 4. 变红逻辑
  if (isDanger) {
    track.classList.add("danger");
  } else {
    track.classList.remove("danger");
  }
}

// 获取端口流量
async function fetchPortTraffic() {
  try {
    const res = await fetch("/api/traffic/ports");
    const data = await res.json();

    if (!data.ports || data.ports.length === 0) {
      // 显示提示信息
      const todayEl = document.getElementById("port-traffic-today");
      if (todayEl)
        todayEl.innerHTML = '<div class="port-no-data">暂无端口流量数据</div>';
      return;
    }

    // 检查 iptables 规则状态
    if (data.iptables_ok === false) {
      const todayEl = document.getElementById("port-traffic-today");
      if (todayEl) {
        todayEl.innerHTML =
          '<div class="port-warning">⚠️ iptables 规则未完整配置（TCP/UDP），请运行 setup-iptables.sh</div>';
      }
      return;
    }

    // 渲染今日端口流量
    renderPortList("port-traffic-today", data.ports, "today");
    // 渲染昨日端口流量
    renderPortList("port-traffic-yesterday", data.ports, "yesterday");
    // 渲染本月端口流量
    renderPortMonthGrid("port-traffic-month", data.ports);
  } catch (e) {
    console.error("获取端口流量失败:", e);
  }
}

// 渲染端口流量列表
function renderPortList(containerId, ports, period) {
  const container = document.getElementById(containerId);
  if (!container) return;

  container.innerHTML = ports
    .map((p) => {
      const d = p[period];
      const name = escapeHtml(p.name.toLowerCase());
      // 使用 Grid 布局：第一行名称，第二行三组数据
      return `
      <div class="port-item ${name}">
        <span class="port-name">${name}</span>
        <div class="port-stats">
          <div class="stats-group-up">
            <span class="stat-up">↑ ${formatBytes(d.tx)}</span>
          </div>
          <div class="stats-group-down">
            <span class="stat-down">↓ ${formatBytes(d.rx)}</span>
          </div>
          <div class="stats-group-total">
            <span class="stat-total">⇅ ${formatBytes(d.total)}</span>
          </div>
        </div>
      </div>
    `;
    })
    .join("");
}

// 渲染本月端口流量网格
function renderPortMonthGrid(containerId, ports) {
  const container = document.getElementById(containerId);
  if (!container) return;

  container.innerHTML = ports
    .map((p) => {
      const d = p.this_month;
      const gb = (d.total / 1024 / 1024 / 1024).toFixed(2);
      return `
      <div class="month-item">
        <div class="port-name">${escapeHtml(p.name.toLowerCase())}</div>
        <div class="port-value">${gb} GB</div>
      </div>
    `;
    })
    .join("");
}

// 获取系统资源
async function fetchSystem() {
  try {
    const res = await fetch("/api/system", { cache: "no-store" });
    if (res.status === 401) {
      window.location.href = "/login";
      return;
    }

    // 检查 HTTP 状态
    if (!res.ok) {
      console.error(`API 请求失败: ${res.status} ${res.statusText}`);
      if (res.status === 404) {
        console.error("服务端 API 不存在，请检查 heliox-mon 服务是否正常运行");
      }
      return;
    }

    // 检查 Content-Type
    const contentType = res.headers.get("content-type");
    if (!contentType || !contentType.includes("application/json")) {
      const text = await res.text();
      console.error("服务端返回非 JSON 数据:", text);
      return;
    }

    const data = await res.json();

    document.getElementById("cpu").textContent =
      data.cpu_percent.toFixed(1) + "%";
    document.getElementById("memory").textContent =
      formatBytes(data.mem_used) + " / " + formatBytes(data.mem_total);
    document.getElementById("disk").textContent =
      formatBytes(data.disk_used) + " / " + formatBytes(data.disk_total);
    document.getElementById("load").textContent =
      data.load_1.toFixed(2) +
      " / " +
      data.load_5.toFixed(2) +
      " / " +
      data.load_15.toFixed(2);
  } catch (e) {
    console.error("获取系统数据失败:", e);
  }
}

// SSE 实时网速
const realtimeWindowSize = 60;
let realtimeChart = null;
let realtimeScale = getSpeedScale(1024);
const realtimeLabels = Array.from({ length: realtimeWindowSize }, () => "");
const realtimeTxSeries = Array.from({ length: realtimeWindowSize }, () => 0);
const realtimeRxSeries = Array.from({ length: realtimeWindowSize }, () => 0);

function getRealtimePalette() {
  return {
    down: getCssVar("--speed-down") || "#4dd4ff",
    up: getCssVar("--speed-up") || "#b66cff",
    grid: getCssVar("--speed-grid") || "rgba(255, 255, 255, 0.08)",
    muted: getCssVar("--muted") || "#6e6e80",
    text: getCssVar("--text") || "#f5f5f7",
  };
}

function makeSpeedFill(color) {
  return (context) => {
    const chart = context.chart;
    const { chartArea } = chart;
    if (!chartArea) return hexToRgba(color, 0.2);
    const gradient = chart.ctx.createLinearGradient(
      0,
      chartArea.top,
      0,
      chartArea.bottom,
    );
    gradient.addColorStop(0, hexToRgba(color, 0.28));
    gradient.addColorStop(1, hexToRgba(color, 0.02));
    return gradient;
  };
}

function buildRealtimeDatasets(palette) {
  return [
    {
      label: "上传",
      data: realtimeTxSeries,
      borderColor: palette.up,
      backgroundColor: makeSpeedFill(palette.up),
      borderWidth: 2,
      borderJoinStyle: "round",
      borderCapStyle: "round",
      pointRadius: 0,
      pointHitRadius: 8,
      tension: 0,
      fill: true,
    },
    {
      label: "下载",
      data: realtimeRxSeries,
      borderColor: palette.down,
      backgroundColor: makeSpeedFill(palette.down),
      borderWidth: 2,
      borderJoinStyle: "round",
      borderCapStyle: "round",
      pointRadius: 0,
      pointHitRadius: 8,
      tension: 0,
      fill: true,
    },
  ];
}

function applyRealtimeTicks(scale) {
  const maxVal = realtimeScale.maxBytes || scale.max || 1;
  const ticks = [0, maxVal / 2, maxVal].map((value) => ({
    value: Math.round(value),
  }));
  scale.ticks = ticks;
}

function initRealtimeChart() {
  const canvas = document.getElementById("realtime-chart");
  if (!canvas) return;
  const palette = getRealtimePalette();
  const ctx = canvas.getContext("2d");

  realtimeChart = new Chart(ctx, {
    type: "line",
    data: {
      labels: realtimeLabels,
      datasets: buildRealtimeDatasets(palette),
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      layout: { padding: { right: 12 } },
      interaction: { mode: "index", intersect: false },
      plugins: {
        legend: {
          display: true,
          position: "bottom",
          align: "center",
          labels: {
            color: palette.muted,
            usePointStyle: true,
            pointStyle: "line",
            boxWidth: 28,
          },
        },
        tooltip: { enabled: false },
      },
      scales: {
        x: {
          display: false,
          grid: { display: false },
          ticks: { display: false },
          border: { display: false },
        },
        y: {
          beginAtZero: true,
          min: 0,
          max: realtimeScale.maxBytes,
          position: "right",
          grace: "5%",
          grid: { color: palette.grid },
          ticks: {
            color: palette.muted,
            padding: 12,
            callback: (value) => formatAxisSpeed(value),
          },
          afterBuildTicks: applyRealtimeTicks,
          title: {
            display: false,
          },
          border: { display: false },
        },
      },
    },
  });
}

function applyRealtimeTheme() {
  if (!realtimeChart) return;
  const palette = getRealtimePalette();
  const datasets = realtimeChart.data.datasets;
  if (datasets[0]) {
    datasets[0].borderColor = palette.up;
    datasets[0].backgroundColor = makeSpeedFill(palette.up);
  }
  if (datasets[1]) {
    datasets[1].borderColor = palette.down;
    datasets[1].backgroundColor = makeSpeedFill(palette.down);
  }
  realtimeChart.options.scales.x.ticks.color = palette.muted;
  realtimeChart.options.scales.y.ticks.color = palette.muted;
  realtimeChart.options.scales.y.grid.color = palette.grid;
  if (realtimeChart.options.plugins?.legend?.labels) {
    realtimeChart.options.plugins.legend.labels.color = palette.muted;
  }
  realtimeChart.update("none");
}

function updateRealtimeScale() {
  let maxVal = 0;
  for (const v of realtimeTxSeries) {
    if (v > maxVal) maxVal = v;
  }
  for (const v of realtimeRxSeries) {
    if (v > maxVal) maxVal = v;
  }
  realtimeScale = getSpeedScale(maxVal);
  if (!realtimeChart) return;
  realtimeChart.options.scales.y.ticks.callback = (value) =>
    formatAxisSpeed(value);
  realtimeChart.options.scales.y.max = realtimeScale.maxBytes;
  realtimeChart.options.scales.y.title.text = realtimeScale.unit;
}

function updateRealtimeAverage() {
  const txEl = document.getElementById("avg-tx");
  const rxEl = document.getElementById("avg-rx");
  if (!txEl && !rxEl) return;

  let txSum = 0,
    rxSum = 0;
  for (let i = 0; i < realtimeTxSeries.length; i++) {
    txSum += realtimeTxSeries[i];
    rxSum += realtimeRxSeries[i];
  }
  const txAvg = txSum / realtimeTxSeries.length;
  const rxAvg = rxSum / realtimeRxSeries.length;

  const [txNum, txUnit] = formatSpeedParts(txAvg);
  const [rxNum, rxUnit] = formatSpeedParts(rxAvg);
  if (txEl)
    txEl.innerHTML = `<span>↑</span><span>${txNum}</span><span>${txUnit}</span>`;
  if (rxEl)
    rxEl.innerHTML = `<span>↓</span><span>${rxNum}</span><span>${rxUnit}</span>`;
}

function pushRealtimePoint(txSpeed, rxSpeed) {
  const label = formatTimeLabel(new Date());
  realtimeLabels.shift();
  realtimeTxSeries.shift();
  realtimeRxSeries.shift();
  realtimeLabels.push(label);
  realtimeTxSeries.push(txSpeed);
  realtimeRxSeries.push(rxSpeed);

  updateRealtimeAverage();
  updateRealtimeScale();
  if (realtimeChart) realtimeChart.update();
}

function connectRealtime() {
  const eventSource = new EventSource("/api/traffic/realtime");

  eventSource.onmessage = (event) => {
    const data = JSON.parse(event.data);
    const txSpeed = Math.max(0, Number(data.tx_speed) || 0);
    const rxSpeed = Math.max(0, Number(data.rx_speed) || 0);
    const txEl = document.getElementById("tx-speed");
    const rxEl = document.getElementById("rx-speed");
    if (txEl) txEl.textContent = formatSpeed(txSpeed);
    if (rxEl) rxEl.textContent = formatSpeed(rxSpeed);
    pushRealtimePoint(txSpeed, rxSpeed);
  };

  eventSource.onerror = () => {
    console.error("SSE 连接断开，5秒后重连...");
    eventSource.close();
    setTimeout(connectRealtime, 5000);
  };
}

// 延迟图表
let latencyChart = null;
let latencyData = null;
let latencyZoom = { start: 0, end: 100 };
let latencyRange = null;
let latencyStatsRaf = null;
let latencyLossSeries = [];
const latencyLossThreshold = 1.0;
const latencyColors = [
  { border: "#0A84FF", bg: "rgba(10, 132, 255, 0.16)" }, // Blue
  { border: "#30D158", bg: "rgba(48, 209, 88, 0.16)" }, // Green
  { border: "#FF9F0A", bg: "rgba(255, 159, 10, 0.16)" }, // Orange
  { border: "#BF5AF2", bg: "rgba(191, 90, 242, 0.16)" }, // Purple
  { border: "#64D2FF", bg: "rgba(100, 210, 255, 0.16)" }, // Cyan
  { border: "#FF375F", bg: "rgba(255, 55, 95, 0.16)" }, // Pink
];
const latencyLossColor = { border: "#FF453A", bg: "rgba(255, 69, 58, 0.12)" };

// 延迟查询参数
let latencyStartDate = null;
let latencyEndDate = null;
let activeTags = new Set(); // 选中的运营商标签
let filtersInitialized = false;
const themeStorageKey = "heliox-theme";

function formatDateValue(date) {
  return date.toISOString().split("T")[0];
}

function setLatencyRecentActive(active) {
  const recentBtn = document.getElementById("latency-recent");
  if (!recentBtn) return;
  recentBtn.classList.toggle("is-active", active);
}

function getCssVar(name) {
  const root = document.body || document.documentElement;
  return getComputedStyle(root).getPropertyValue(name).trim();
}

function applyTheme(theme) {
  const isLight = theme === "light";
  document.body.classList.toggle("theme-light", isLight);
  const themeText = document.querySelector("#theme-toggle .theme-text");
  if (themeText) {
    themeText.textContent = isLight ? "浅色" : "深色";
  }
  renderLatencyChart();
  applyRealtimeTheme();
}

function initThemeToggle() {
  const stored = localStorage.getItem(themeStorageKey);
  // 未手动选择过时跟随系统偏好，默认深色
  const prefersLight =
    window.matchMedia &&
    window.matchMedia("(prefers-color-scheme: light)").matches;
  const preferred = stored || (prefersLight ? "light" : "dark");
  applyTheme(preferred);

  const toggleBtn = document.getElementById("theme-toggle");
  if (!toggleBtn) return;
  toggleBtn.addEventListener("click", () => {
    const next = document.body.classList.contains("theme-light")
      ? "dark"
      : "light";
    localStorage.setItem(themeStorageKey, next);
    applyTheme(next);
  });
}

function normalizeRange(startVal, endVal) {
  let start = startVal ? String(startVal).trim() : "";
  let end = endVal ? String(endVal).trim() : "";

  if (!start && !end) {
    return { start: null, end: null };
  }

  if (!start && end) start = end;
  if (start && !end) end = start;

  if (start && end && start > end) {
    const tmp = start;
    start = end;
    end = tmp;
  }

  return { start, end };
}

function shiftDateValue(dateStr, offsetDays) {
  const date = new Date(dateStr);
  date.setDate(date.getDate() + offsetDays);
  return formatDateValue(date);
}

async function fetchLatency(start = null, end = null) {
  try {
    let url = "/api/latency";
    const range = normalizeRange(start, end);
    setLatencyRecentActive(!range.start && !range.end);
    if (range.start && range.end) {
      url += `?start=${range.start}&end=${range.end}`;
    }

    const res = await fetch(url);
    if (res.status === 401) {
      window.location.href = "/login";
      return;
    }
    latencyData = await res.json();

    // 显示粒度信息
    const granularityEl = document.getElementById("latency-granularity");
    if (granularityEl && latencyData.granularity) {
      let label = `粒度: ${latencyData.granularity} 分钟`;
      if (!range.start && !range.end) {
        label += " · 最近24小时";
      }
      granularityEl.textContent = label;
    }

    // 初始化过滤器 (仅一次)
    if (!filtersInitialized && latencyData.targets) {
      renderFilterCheckboxes(latencyData.targets);
      filtersInitialized = true;
    }

    renderLatencyChart();
    scheduleLatencyStatsRender();
  } catch (e) {
    console.error("获取延迟数据失败:", e);
  }
}

function renderFilterCheckboxes(targets) {
  const container = document.getElementById("target-filters");
  if (!container) return;

  container.innerHTML = "";
  targets.forEach((t, idx) => {
    // 默认全选
    activeTags.add(t.tag);

    const label = document.createElement("label");
    label.className = "filter-pill";
    const dot = document.createElement("span");
    dot.className = "latency-target-dot";
    dot.style.background = latencyColors[idx % latencyColors.length].border;

    const input = document.createElement("input");
    input.type = "checkbox";
    input.checked = true;
    input.dataset.tag = t.tag;

    input.addEventListener("change", (e) => {
      if (e.target.checked) {
        activeTags.add(t.tag);
      } else {
        activeTags.delete(t.tag);
      }
      renderLatencyChart();
      scheduleLatencyStatsRender();
    });

    label.appendChild(input);
    label.appendChild(dot);
    label.appendChild(document.createTextNode(" " + t.tag));
    container.appendChild(label);
  });
}

function renderLatencyChart() {
  if (!latencyData || !latencyData.targets) return;

  const showMax = document.getElementById("show-max")?.checked ?? false;
  const showAvg = document.getElementById("show-avg")?.checked ?? false;
  const showLoss = document.getElementById("show-loss")?.checked ?? false;

  const chartEl = document.getElementById("latency-chart");
  if (!chartEl || typeof echarts === "undefined") return;

  if (!latencyChart) {
    latencyChart = echarts.init(chartEl, null, {
      renderer: "canvas",
      useDirtyRect: true,
    });
    window.addEventListener("resize", () => {
      if (latencyChart) latencyChart.resize();
    });
  }

  const textColor = getCssVar("--text");
  const mutedColor = getCssVar("--muted");
  const borderColor = getCssVar("--card-border");
  const tooltipBg = getCssVar("--card-bg");
  const isLight = document.body.classList.contains("theme-light");
  const gridLine = isLight
    ? "rgba(0, 0, 0, 0.08)"
    : "rgba(255, 255, 255, 0.06)";
  const zoomBg = isLight ? "rgba(0, 0, 0, 0.08)" : "rgba(0, 0, 0, 0.2)";
  const zoomFill = isLight
    ? "rgba(10, 132, 255, 0.25)"
    : "rgba(10, 132, 255, 0.2)";
  const avgLabelBg = isLight
    ? "rgba(100, 116, 139, 0.45)"
    : "rgba(100, 116, 139, 0.5)"; // 蓝灰色 - 平均值
  const maxLabelBg = isLight
    ? "rgba(220, 104, 104, 0.45)"
    : "rgba(220, 104, 104, 0.5)"; // 柔和红 - 最高值
  const minLabelBg = isLight
    ? "rgba(104, 180, 140, 0.45)"
    : "rgba(104, 180, 140, 0.5)"; // 柔和绿 - 最低值

  // 无数据缺口：相邻点间隔超过 1.5 倍粒度视为监控停采（如服务器重启）
  const gapStepMs = Math.max(1, latencyData.granularity || 1) * 60000;
  const gapThresholdMs = gapStepMs * 1.5;

  const series = latencyData.targets
    .map((target, idx) => {
      if (!activeTags.has(target.tag)) return null;
      const color = latencyColors[idx % latencyColors.length];
      const points = target.points || [];
      // 缺口处插入 null 点让折线断开，避免直线跨越无数据时段造成误导
      const data = [];
      let prevTs = null;
      points.forEach((p) => {
        const ts = p.ts * 1000;
        if (prevTs !== null && ts - prevTs > gapThresholdMs) {
          data.push({ value: [prevTs + gapStepMs, null], isGap: true });
        }
        data.push([
          ts,
          p.rtt_ms === null || p.rtt_ms === undefined ? null : p.rtt_ms,
        ]);
        prevTs = ts;
      });
      const stats = target.stats || {};
      const avg = stats.avg ?? 0;

      return {
        name: target.tag,
        type: "line",
        smooth: true,
        showSymbol: false,
        data,
        itemStyle: { color: color.border },
        lineStyle: { color: color.border, width: 2 },
        areaStyle: {
          color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: color.bg.replace("0.16", "0.28") },
            { offset: 1, color: "rgba(0,0,0,0)" },
          ]),
        },
        emphasis: { focus: "series" },
        markLine:
          showAvg && avg > 0
            ? {
                symbol: "none",
                lineStyle: {
                  type: "dashed",
                  color: color.border,
                  opacity: 0.65,
                },
                label: {
                  color: textColor,
                  backgroundColor: avgLabelBg,
                  borderRadius: 6,
                  padding: [3, 6],
                  formatter: ({ value }) => `${value.toFixed(1)}ms`,
                  position: "insideEndTop",
                },
                data: [{ yAxis: avg }],
              }
            : undefined,
        markPoint: showMax
          ? {
              symbol: "circle",
              symbolSize: 6,
              itemStyle: { color: color.border, opacity: 0.85 },
              label: {
                color: textColor,
                fontSize: 11,
                borderRadius: 6,
                padding: [2, 6],
                formatter: (param) => {
                  const v = Array.isArray(param.value)
                    ? param.value[param.value.length - 1]
                    : param.value;
                  if (v === null || v === undefined || Number.isNaN(v))
                    return "";
                  return `${Number(v).toFixed(1)}ms`;
                },
                position: "top",
                distance: 6,
              },
              data: [
                { type: "max", label: { backgroundColor: maxLabelBg } },
                { type: "min", label: { backgroundColor: minLabelBg } },
              ],
            }
          : undefined,
      };
    })
    .filter(Boolean);

  // 灰色区域标注无数据缺口；挂在第一个目标序列上即可全图显示
  const gapAreas = buildGapMarkAreas(latencyData.targets, gapThresholdMs);
  if (series.length && gapAreas.length) {
    series[0].markArea = {
      silent: true,
      itemStyle: {
        color: isLight
          ? "rgba(100, 116, 139, 0.10)"
          : "rgba(148, 163, 184, 0.08)",
      },
      label: {
        show: true,
        position: "insideTop",
        color: mutedColor,
        fontSize: 10,
      },
      data: gapAreas,
    };
  }

  latencyLossSeries = buildLossSeries(latencyData.targets);
  if (showLoss && latencyLossSeries.length) {
    series.push({
      name: "丢包率",
      type: "line",
      yAxisIndex: 1,
      smooth: true,
      showSymbol: false,
      data: latencyLossSeries.map((p) =>
        p.gap ? { value: [p.ts, null], isGap: true } : [p.ts, p.loss],
      ),
      itemStyle: { color: latencyLossColor.border },
      lineStyle: { color: latencyLossColor.border, width: 1.5 },
      areaStyle: { color: latencyLossColor.bg },
      emphasis: { focus: "series" },
      markArea: {
        silent: true,
        itemStyle: { color: "rgba(255, 99, 71, 0.08)" },
        data: buildLossMarkAreas(latencyLossSeries, latencyLossThreshold),
      },
    });
  }

  latencyRange = getLatencyRange(series);
  const zoomStart = latencyZoom.start ?? 0;
  const zoomEnd = latencyZoom.end ?? 100;

  const option = {
    animation: false,
    grid: { left: 50, right: 24, top: 24, bottom: 54, containLabel: true },
    tooltip: {
      trigger: "axis",
      backgroundColor: tooltipBg,
      borderColor: borderColor,
      textStyle: { color: textColor },
      axisPointer: { type: "cross", label: { color: textColor } },
      formatter: (params) => {
        const time = new Date(params[0].value[0]).toLocaleString("zh-CN");
        const rows = params
          .map((p) => {
            const value = Array.isArray(p.value) ? p.value[1] : p.value;
            const isGap = p.data && !Array.isArray(p.data) && p.data.isGap;
            const text =
              value === null || value === undefined
                ? isGap
                  ? "无数据"
                  : "丢包"
                : p.seriesName === "丢包率"
                  ? `${Number(value).toFixed(1)}%`
                  : `${Number(value).toFixed(1)} ms`;
            return `<span style=\"display:inline-block;margin-right:6px;width:8px;height:8px;border-radius:50%;background:${p.color}\"></span>${escapeHtml(p.seriesName)}: ${text}`;
          })
          .join("<br/>");
        return `${time}<br/>${rows}`;
      },
    },
    xAxis: {
      type: "time",
      axisLine: { lineStyle: { color: borderColor } },
      axisLabel: { color: mutedColor },
      splitLine: { show: false },
    },
    yAxis: [
      {
        type: "value",
        axisLine: { lineStyle: { color: borderColor } },
        axisLabel: { color: mutedColor, formatter: "{value} ms" },
        splitLine: { lineStyle: { color: gridLine } },
      },
      {
        type: "value",
        axisLine: { lineStyle: { color: borderColor } },
        axisLabel: { color: mutedColor, formatter: "{value}%" },
        splitLine: { show: false },
        min: 0,
        max: 100,
      },
    ],
    dataZoom: [
      {
        type: "inside",
        xAxisIndex: 0,
        start: zoomStart,
        end: zoomEnd,
      },
      {
        type: "slider",
        xAxisIndex: 0,
        height: 26,
        bottom: 10,
        start: zoomStart,
        end: zoomEnd,
        borderColor: borderColor,
        backgroundColor: zoomBg,
        fillerColor: zoomFill,
        handleSize: "120%",
        handleStyle: {
          color: "#0a84ff",
          borderColor: borderColor,
        },
        textStyle: { color: mutedColor },
      },
    ],
    series,
  };

  if (!series.length) {
    option.graphic = [
      {
        type: "text",
        left: "center",
        top: "middle",
        style: { text: "暂无数据", fill: mutedColor, fontSize: 14 },
      },
    ];
  }

  latencyChart.setOption(option, true);
  bindLatencyZoom();
  scheduleLatencyStatsRender();
}

function renderLatencyStats() {
  const container = document.getElementById("latency-stats");
  if (!container || !latencyData || !latencyData.targets) return;
  const range = getZoomRange();
  let totalCount = 0;
  let sum = 0;
  let min = Infinity;
  let max = -Infinity;
  let totalSent = 0;
  let totalLost = 0;
  const anomalyMinutes = computeLossAnomalyMinutes(
    latencyLossSeries,
    range,
    latencyLossThreshold,
    latencyData?.granularity,
  );

  const targetCards = latencyData.targets
    .filter((t) => activeTags.has(t.tag))
    .map((t, idx) => {
      const stats = computeTargetStats(t.points || [], range);
      if (stats.count && stats.avg != null) {
        sum += stats.avg * stats.count;
        totalCount += stats.count;
      }
      if (stats.min != null && stats.min < min) min = stats.min;
      if (stats.max != null && stats.max > max) max = stats.max;
      totalSent += stats.sent;
      totalLost += stats.lost;

      const color = latencyColors[idx % latencyColors.length].border;
      return `
        <div class="latency-target-card">
          <div class="latency-target-header">
            <span class="latency-target-dot" style="background:${color}"></span>
            <span>${escapeHtml(t.tag)}</span>
          </div>
          <div class="latency-target-values">
            <span>均值 <strong>${formatNumber(stats.avg)}</strong>ms</span>
            <span>P95 <strong>${formatNumber(stats.p95)}</strong>ms</span>
            <span>抖动 <strong>${formatNumber(stats.jitter)}</strong>ms</span>
            <span>最小 <strong>${formatNumber(stats.min)}</strong>ms</span>
            <span>最大 <strong>${formatNumber(stats.max)}</strong>ms</span>
            <span>丢包 <strong>${formatPercent(stats.lossRate)}</strong></span>
          </div>
        </div>
      `;
    })
    .join("");

  const hasData = totalCount > 0;
  const avg = hasData ? sum / totalCount : null;
  if (min === Infinity) min = null;
  if (max === -Infinity) max = null;
  const lossRate = totalSent > 0 ? (totalLost / totalSent) * 100 : null;

  container.innerHTML = `
    <div class="latency-summary">
      <div class="latency-metric">
        <span class="label">平均延迟</span>
        <span class="value">${formatNumber(avg)}<small>ms</small></span>
      </div>
      <div class="latency-metric">
        <span class="label">最小延迟</span>
        <span class="value">${formatNumber(min)}<small>ms</small></span>
      </div>
      <div class="latency-metric">
        <span class="label">最大延迟</span>
        <span class="value">${formatNumber(max)}<small>ms</small></span>
      </div>
      <div class="latency-metric">
        <span class="label">丢包率</span>
        <span class="value">${formatPercent(lossRate)}</span>
      </div>
      <div class="latency-metric">
        <span class="label">异常时长</span>
        <span class="value">${formatDuration(anomalyMinutes)}</span>
      </div>
    </div>
    <div class="latency-targets">${targetCards}</div>
  `;
}

function scheduleLatencyStatsRender() {
  if (latencyStatsRaf) return;
  latencyStatsRaf = requestAnimationFrame(() => {
    latencyStatsRaf = null;
    renderLatencyStats();
  });
}

function formatNumber(value) {
  if (value === null || value === undefined || Number.isNaN(value)) return "-";
  return value.toFixed(1);
}

function formatPercent(value) {
  if (value === null || value === undefined || Number.isNaN(value)) return "-";
  return `${value.toFixed(1)}%`;
}

function formatDuration(minutes) {
  if (minutes === null || minutes === undefined || Number.isNaN(minutes))
    return "-";
  if (minutes < 1) return "<1m";
  if (minutes < 60) return `${Math.round(minutes)}m`;
  const hours = minutes / 60;
  if (hours < 24) return `${hours.toFixed(1)}h`;
  return `${(hours / 24).toFixed(1)}d`;
}

function getLatencyRange(series) {
  let min = Infinity;
  let max = -Infinity;
  series.forEach((s) => {
    (s.data || []).forEach((point) => {
      const ts = Array.isArray(point) ? point[0] : point.value?.[0];
      if (ts === undefined) return;
      if (ts < min) min = ts;
      if (ts > max) max = ts;
    });
  });
  if (min === Infinity || max === -Infinity) return null;
  return { min, max };
}

function buildLossSeries(targets) {
  const map = new Map();
  targets
    .filter((t) => activeTags.has(t.tag))
    .forEach((t) => {
      (t.points || []).forEach((p) => {
        if (!map.has(p.ts)) {
          map.set(p.ts, { sent: 0, lost: 0 });
        }
        const row = map.get(p.ts);
        row.sent += p.sent || 0;
        row.lost += p.lost || 0;
      });
    });

  const points = Array.from(map.entries())
    .map(([ts, v]) => ({
      ts: ts * 1000,
      loss: v.sent > 0 ? (v.lost / v.sent) * 100 : null,
    }))
    .sort((a, b) => a.ts - b.ts);

  // 缺口处插入 null 断点，同时避免缺口前的高丢包点把整段缺口计入异常时长
  const stepMs = Math.max(1, latencyData?.granularity || 1) * 60000;
  const thresholdMs = stepMs * 1.5;
  const filled = [];
  points.forEach((p) => {
    const prev = filled.length ? filled[filled.length - 1] : null;
    if (prev && p.ts - prev.ts > thresholdMs) {
      filled.push({ ts: prev.ts + stepMs, loss: null, gap: true });
    }
    filled.push(p);
  });
  return filled;
}

// 合并所有目标的时间线找无数据缺口（采集器停采对所有目标同时生效）
function buildGapMarkAreas(targets, thresholdMs) {
  const tsSet = new Set();
  targets.forEach((t) => {
    (t.points || []).forEach((p) => tsSet.add(p.ts * 1000));
  });
  const tsList = Array.from(tsSet).sort((a, b) => a - b);
  const areas = [];
  for (let i = 1; i < tsList.length; i++) {
    if (tsList[i] - tsList[i - 1] > thresholdMs) {
      // 灰色区域两端贴齐相邻真实数据点，完整覆盖缺口（不再往内缩一格）
      areas.push([
        { xAxis: tsList[i - 1], name: "无数据" },
        { xAxis: tsList[i] },
      ]);
    }
  }
  return areas;
}

function buildLossMarkAreas(points, threshold) {
  if (!points.length) return [];
  const areas = [];
  let start = null;

  for (let i = 0; i < points.length; i++) {
    const p = points[i];
    const over = p.loss !== null && p.loss >= threshold;
    if (over && start === null) {
      start = p.ts;
    }
    const isLast = i === points.length - 1;
    if ((start !== null && !over) || (start !== null && isLast)) {
      const end = over && isLast ? p.ts : points[i - 1].ts;
      if (end > start) {
        areas.push([{ xAxis: start }, { xAxis: end }]);
      }
      start = null;
    }
  }
  return areas;
}

function getZoomRange() {
  if (!latencyRange) return null;
  const span = latencyRange.max - latencyRange.min;
  if (span <= 0) return null;
  const start = latencyRange.min + (span * latencyZoom.start) / 100;
  const end = latencyRange.min + (span * latencyZoom.end) / 100;
  return { start, end };
}

function computeLossAnomalyMinutes(points, range, threshold, granularity) {
  if (!points || points.length === 0) return null;
  const start = range?.start ?? null;
  const end = range?.end ?? null;
  let totalMs = 0;
  const defaultStep = (granularity || 1) * 60 * 1000;

  for (let i = 0; i < points.length; i++) {
    const p = points[i];
    const ts = p.ts;
    if (start && ts < start) continue;
    if (end && ts > end) continue;
    const over = p.loss !== null && p.loss >= threshold;
    if (!over) continue;

    const next = points[i + 1];
    const delta = next ? next.ts - ts : defaultStep;
    totalMs += Math.max(0, delta);
  }

  return totalMs / 60000;
}

function computeTargetStats(points, range) {
  let sum = 0;
  let count = 0;
  let min = Infinity;
  let max = -Infinity;
  let sent = 0;
  let lost = 0;

  let validRtts = [];
  // mdev：ping 实测抖动（单次探测内标准差），更标准且经聚合仍有效，优先使用
  let mdevSum = 0;
  let mdevCount = 0;
  // 相邻采样差：仅在缺少 mdev 的旧数据时回退使用
  let deltaSum = 0;
  let deltaCount = 0;
  let prevRtt = null;

  const start = range?.start ?? null;
  const end = range?.end ?? null;

  points.forEach((p) => {
    const ts = p.ts * 1000;
    if (start && ts < start) return;
    if (end && ts > end) return;
    if (p.rtt_ms !== null && p.rtt_ms !== undefined) {
      sum += p.rtt_ms;
      count += 1;
      validRtts.push(p.rtt_ms);

      // min 优先用服务端真实最小 RTT，回退到本点平均值
      const minCandidate =
        p.min_rtt !== null && p.min_rtt !== undefined ? p.min_rtt : p.rtt_ms;
      if (minCandidate < min) min = minCandidate;
      if (p.rtt_ms > max) max = p.rtt_ms;

      // 抖动：优先累加服务端 mdev
      if (p.jitter !== null && p.jitter !== undefined) {
        mdevSum += p.jitter;
        mdevCount += 1;
      }
      if (prevRtt !== null) {
        deltaSum += Math.abs(p.rtt_ms - prevRtt);
        deltaCount += 1;
      }
      prevRtt = p.rtt_ms;
    } else {
      // Missing point (e.g. timeout) breaks the jitter sequence
      prevRtt = null;
    }
    if (p.sent !== undefined && p.sent !== null) {
      sent += p.sent;
      lost += p.lost || 0;
    }
  });

  let p95 = null;
  if (validRtts.length > 0) {
    validRtts.sort((a, b) => a - b);
    const idx = Math.floor(validRtts.length * 0.95);
    p95 = validRtts[idx];
  }

  const jitter = mdevCount
    ? mdevSum / mdevCount
    : deltaCount
      ? deltaSum / deltaCount
      : null;

  return {
    avg: count ? sum / count : null,
    p95: p95,
    jitter: jitter,
    min: min === Infinity ? null : min,
    max: max === -Infinity ? null : max,
    count,
    sent,
    lost,
    lossRate: sent ? (lost / sent) * 100 : null,
  };
}

function bindLatencyZoom() {
  if (!latencyChart || latencyChart.__zoomBound) return;
  latencyChart.on("dataZoom", (evt) => {
    const batch = evt?.batch?.[0];
    if (
      batch &&
      typeof batch.start === "number" &&
      typeof batch.end === "number"
    ) {
      latencyZoom = { start: batch.start, end: batch.end };
      scheduleLatencyStatsRender();
    }
  });
  latencyChart.__zoomBound = true;
}

// 月度趋势图表
let trendChart = null;
let trendChartType = "bar";
let trendMonthlyData = null;
let trendDailyData = null;
let trendCycleData = null;
let trendRange = "monthly"; // monthly | 30d | cycle
let trendView = "total"; // detail | total

function setTrendTitle(text) {
  const titleEl = document.getElementById("trend-title");
  if (titleEl) titleEl.textContent = text;
}

function setToggleState(el, active) {
  if (!el) return;
  if (active) {
    el.classList.add("active");
    el.classList.remove("btn-secondary");
  } else {
    el.classList.remove("active");
    el.classList.add("btn-secondary");
  }
}

function updateTrendToggleState() {
  const detailBtn = document.getElementById("trend-detail");
  const totalBtn = document.getElementById("trend-total");
  const rangeMonthBtn = document.getElementById("trend-range-month");
  const range30Btn = document.getElementById("trend-range-30d");
  const rangeCycleBtn = document.getElementById("trend-range-cycle");
  const viewToggle = document.getElementById("trend-view-toggle");

  setToggleState(rangeMonthBtn, trendRange === "monthly");
  setToggleState(range30Btn, trendRange === "30d");
  setToggleState(rangeCycleBtn, trendRange === "cycle");
  setToggleState(totalBtn, trendView === "total");
  setToggleState(detailBtn, trendView === "detail");

  if (viewToggle) {
    viewToggle.style.display =
      trendRange === "monthly" ? "inline-flex" : "none";
  }
}

async function fetchMonthlyTrend() {
  try {
    const res = await fetch("/api/traffic/monthly");
    trendMonthlyData = await res.json();

    // 空数据保护
    if (!trendMonthlyData || !Array.isArray(trendMonthlyData)) {
      console.warn("月度趋势数据为空");
      return;
    }

    renderTrendChart();
  } catch (e) {
    console.error("获取月度趋势失败:", e);
  }
}

async function fetchDailyTrend(rangeType) {
  const range = rangeType === "cycle" ? "cycle" : "30d";
  try {
    const res = await fetch(`/api/traffic/daily?range=${range}`);
    const data = await res.json();

    if (!data || !Array.isArray(data)) {
      console.warn("每日趋势数据为空");
      return;
    }

    const sorted = data.slice().sort((a, b) => a.date.localeCompare(b.date));

    if (range === "cycle") {
      trendCycleData = sorted;
    } else {
      trendDailyData = sorted;
    }

    renderTrendChart();
  } catch (e) {
    console.error("获取每日趋势失败:", e);
  }
}

function renderTrendChart() {
  let labels = [];
  let datasets = [];
  let legendHtml = "";
  let chartType = "bar";
  let tooltipCallbacks = {
    label: (ctx) => `${ctx.dataset.label}: ${ctx.raw.toFixed(2)} GB`,
  };
  let tickCallback = null;
  let trendAvgAnnotation = null;
  let trendMaxAnnotation = null;
  let trendYMax = null;

  if (trendRange === "monthly") {
    if (!trendMonthlyData) return;

    setTrendTitle("近6个月流量");
    updateTrendToggleState();

    labels = trendMonthlyData.map((d) => {
      const parts = d.month.split("-");
      return parts[1] + "月";
    });
    const totalLabels = trendMonthlyData.map((d) => d.total_gb);

    if (trendView === "detail") {
      // 详细视图：2根柱子（snell/vless），每根柱子堆叠上传下载
      datasets = [
        {
          label: "snell 下载",
          data: trendMonthlyData.map((d) => d.snell_rx / 1024 / 1024 / 1024),
          backgroundColor: "#4DD4FF", // Neon cyan
          borderRadius: { bottomLeft: 4, bottomRight: 4 },
          stack: "snell",
        },
        {
          label: "snell 上传",
          data: trendMonthlyData.map((d) => d.snell_tx / 1024 / 1024 / 1024),
          backgroundColor: "#3B82F6", // Electric blue
          borderRadius: { topLeft: 4, topRight: 4 },
          stack: "snell",
        },
        {
          label: "vless 下载",
          data: trendMonthlyData.map((d) => d.vless_rx / 1024 / 1024 / 1024),
          backgroundColor: "#9B8CFF", // Neon lavender
          borderRadius: { bottomLeft: 4, bottomRight: 4 },
          stack: "vless",
        },
        {
          label: "vless 上传",
          data: trendMonthlyData.map((d) => d.vless_tx / 1024 / 1024 / 1024),
          backgroundColor: "#6D28D9", // Deep violet
          borderRadius: { topLeft: 4, topRight: 4 },
          stack: "vless",
        },
      ];
      legendHtml = `
        <span class="legend-item"><span class="dot" style="background:#3B82F6"></span>snell 上传</span>
        <span class="legend-item"><span class="dot" style="background:#4DD4FF"></span>snell 下载</span>
        <span class="legend-item"><span class="dot" style="background:#6D28D9"></span>vless 上传</span>
        <span class="legend-item"><span class="dot" style="background:#9B8CFF"></span>vless 下载</span>
      `;
    } else {
      // 总计视图：2根柱子（上传/下载）
      datasets = [
        {
          label: "上传",
          data: trendMonthlyData.map((d) => d.total_tx / 1024 / 1024 / 1024),
          backgroundColor: "#4F7DF7", // Cobalt blue
          borderRadius: 4,
        },
        {
          label: "下载",
          data: trendMonthlyData.map((d) => d.total_rx / 1024 / 1024 / 1024),
          backgroundColor: "#39D0C3", // Tech teal
          borderRadius: 4,
        },
      ];
      legendHtml = `
        <span class="legend-item"><span class="dot" style="background:#4F7DF7"></span>上传</span>
        <span class="legend-item"><span class="dot" style="background:#39D0C3"></span>下载</span>
      `;
    }

    tickCallback = function (value, index) {
      return [totalLabels[index], "", labels[index]];
    };
  } else {
    const source = trendRange === "cycle" ? trendCycleData : trendDailyData;
    if (!source) return;

    setTrendTitle(trendRange === "cycle" ? "本计费周期流量" : "近30天流量");
    updateTrendToggleState();

    // 完整日期标签（用于 tooltip）
    labels = source.map((d) => d.date.slice(5));
    const totals = source.map((d) => (d.tx + d.rx) / 1024 / 1024 / 1024);
    const txData = source.map((d) => d.tx / 1024 / 1024 / 1024);
    const rxData = source.map((d) => d.rx / 1024 / 1024 / 1024);

    // 计算平均值用于参考线
    const avgValue = totals.reduce((a, b) => a + b, 0) / totals.length;

    // 汇总总量
    const sumTotal = totals.reduce((a, b) => a + b, 0);
    const sumTx = txData.reduce((a, b) => a + b, 0);
    const sumRx = rxData.reduce((a, b) => a + b, 0);

    // 计算 Y 轴动态范围
    const maxValue = Math.max(...totals, avgValue);
    const yMax = niceCeil(maxValue * 1.15); // 留出 15% 空间

    // 生成渐变填充（40% → 5%）
    const makeTrendGradient = (context) => {
      const chart = context.chart;
      const { chartArea } = chart;
      if (!chartArea) return "rgba(10, 132, 255, 0.2)";
      const gradient = chart.ctx.createLinearGradient(
        0,
        chartArea.top,
        0,
        chartArea.bottom,
      );
      gradient.addColorStop(0, "rgba(10, 132, 255, 0.40)");
      gradient.addColorStop(1, "rgba(10, 132, 255, 0.05)");
      return gradient;
    };

    // 今日数据点特殊样式（最后一个点）+ hover 时高亮
    const pointRadii = totals.map((_, i) => (i === totals.length - 1 ? 6 : 0));
    const pointHoverRadii = totals.map(() => 6); // 悬停时所有点都高亮
    const pointBgColors = totals.map((_, i) =>
      i === totals.length - 1 ? "#007AFF" : "transparent",
    );
    const pointHoverBgColors = totals.map(() => "#007AFF"); // 悬停时蓝色
    const pointBorderColors = totals.map((_, i) =>
      i === totals.length - 1 ? "#fff" : "transparent",
    );
    const pointHoverBorderColors = totals.map(() => "#fff");
    const pointBorderWidths = totals.map((_, i) =>
      i === totals.length - 1 ? 2 : 0,
    );

    // 计算极值索引
    const maxIdx = totals.indexOf(Math.max(...totals));
    const maxVal = totals[maxIdx];
    const maxDate = source[maxIdx]?.date?.slice(5) || "";

    datasets = [
      {
        label: "总流量",
        data: totals,
        borderColor: "#007AFF", // macOS System Blue
        backgroundColor: makeTrendGradient,
        borderWidth: 2.5,
        pointRadius: pointRadii,
        pointHoverRadius: pointHoverRadii,
        pointBackgroundColor: pointBgColors,
        pointHoverBackgroundColor: pointHoverBgColors,
        pointBorderColor: pointBorderColors,
        pointHoverBorderColor: pointHoverBorderColors,
        pointBorderWidth: pointBorderWidths,
        tension: 0.4,
        cubicInterpolationMode: "monotone",
        fill: true,
      },
      {
        label: "上传",
        data: txData,
        borderColor: "rgba(79, 125, 247, 0.6)",
        backgroundColor: "transparent",
        borderWidth: 1.5,
        borderDash: [4, 3],
        pointRadius: 0,
        pointHoverRadius: 4,
        pointHoverBackgroundColor: "#4F7DF7",
        tension: 0.4,
        cubicInterpolationMode: "monotone",
        fill: false,
      },
      {
        label: "下载",
        data: rxData,
        borderColor: "rgba(57, 208, 195, 0.6)",
        backgroundColor: "transparent",
        borderWidth: 1.5,
        borderDash: [4, 3],
        pointRadius: 0,
        pointHoverRadius: 4,
        pointHoverBackgroundColor: "#39D0C3",
        tension: 0.4,
        cubicInterpolationMode: "monotone",
        fill: false,
      },
    ];

    // 格式化汇总值
    const fmtSum = (v) =>
      v >= 1 ? `${v.toFixed(1)} GB` : `${(v * 1024).toFixed(0)} MB`;

    // 今日流量数值（最后一个点）
    const todayValue = totals[totals.length - 1];
    const todayLabel =
      todayValue >= 1
        ? `${todayValue.toFixed(1)} GB`
        : `${(todayValue * 1024).toFixed(0)} MB`;

    legendHtml = `
      <span class="legend-item"><span class="dot" style="background:#007AFF"></span>总流量 ${fmtSum(sumTotal)}</span>
      <span class="legend-item"><span class="dot" style="background:#4F7DF7; opacity:0.6"></span>↑ ${fmtSum(sumTx)}</span>
      <span class="legend-item"><span class="dot" style="background:#39D0C3; opacity:0.6"></span>↓ ${fmtSum(sumRx)}</span>
      <span class="legend-item"><span class="dot" style="background:#86868b; opacity:0.6"></span>日均 ${avgValue.toFixed(2)} GB</span>
      <span class="legend-item trend-today-badge"><span class="trend-today-pulse"></span>今日 ${todayLabel}</span>
    `;
    chartType = "line";

    // X 轴稀疏显示（每 5 天）
    tickCallback = function (value, index) {
      // 显示首尾 + 每隔5天
      if (index === 0 || index === labels.length - 1 || index % 5 === 0) {
        return labels[index];
      }
      return "";
    };

    // 星期名称映射
    const weekdays = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];

    // Tooltip 回调 - 每条线显示自己的数据
    tooltipCallbacks = {
      title: (items) => {
        const idx = items[0]?.dataIndex;
        if (idx === undefined) return "";
        const d = source[idx];
        if (!d) return "";
        const dateObj = new Date(d.date);
        const weekday = weekdays[dateObj.getDay()];
        return `${d.date} (${weekday})`;
      },
      label: (ctx) => {
        const val = ctx.raw;
        const fmt = val >= 1 ? `${val.toFixed(2)} GB` : `${(val * 1024).toFixed(0)} MB`;
        return ` ${ctx.dataset.label}: ${fmt}`;
      },
      afterLabel: (ctx) => {
        // 仅总流量行显示较均值对比
        if (ctx.dataset.label !== "总流量") return "";
        if (avgValue <= 0) return "";
        const diff = ((ctx.raw - avgValue) / avgValue) * 100;
        if (Math.abs(diff) < 1) return "  ≈ 均值";
        const sign = diff > 0 ? "+" : "";
        const arrow = diff > 0 ? "↑" : "↓";
        return `  ${arrow} 较均值 ${sign}${diff.toFixed(0)}%`;
      },
      labelColor: (ctx) => {
        const colors = { "总流量": "#007AFF", "上传": "#4F7DF7", "下载": "#39D0C3" };
        const c = colors[ctx.dataset.label] || "#888";
        return { borderColor: c, backgroundColor: c };
      },
    };

    // 平均参考线注解 + Avg 标签
    trendAvgAnnotation = {
      type: "line",
      yMin: avgValue,
      yMax: avgValue,
      borderColor: "rgba(134, 134, 139, 0.5)",
      borderWidth: 1.5,
      borderDash: [6, 4],
      label: {
        display: true,
        content: "Avg",
        position: "start",
        backgroundColor: "rgba(134, 134, 139, 0.7)",
        color: "#fff",
        font: { size: 10, weight: "500" },
        padding: { top: 2, bottom: 2, left: 4, right: 4 },
        borderRadius: 4,
      },
    };

    // Max 极值标注
    trendMaxAnnotation = {
      type: "point",
      xValue: maxIdx,
      yValue: maxVal,
      backgroundColor: "rgba(255, 69, 58, 0.15)",
      borderColor: "#FF453A",
      borderWidth: 2,
      radius: 8,
      label: {
        display: true,
        content: `Max ${maxVal.toFixed(1)}G`,
        position: "top",
        backgroundColor: "rgba(255, 69, 58, 0.85)",
        color: "#fff",
        font: { size: 10, weight: "600" },
        padding: { top: 3, bottom: 3, left: 6, right: 6 },
        borderRadius: 6,
        yAdjust: -12,
      },
    };

    trendYMax = yMax;
  }

  // 更新图例
  const legendEl = document.getElementById("trend-legend");
  if (legendEl) legendEl.innerHTML = legendHtml;

  // 根据图表类型配置不同的选项
  const isLineChart = chartType === "line";
  const isLight = document.body.classList.contains("theme-light");
  const gridColor = isLight ? "rgba(0, 0, 0, 0.1)" : "rgba(255, 255, 255, 0.1)"; // 10% 透明度
  const tickColor = "#8E8E93"; // System Gray 2

  // 构建 annotations 配置
  const annotationsConfig = {};
  if (trendAvgAnnotation) annotationsConfig.avgLine = trendAvgAnnotation;
  if (trendMaxAnnotation) annotationsConfig.maxPoint = trendMaxAnnotation;

  const options = {
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    layout: {
      padding: { left: 0, right: 0 },
    },
    interaction: {
      mode: "index",
      intersect: false,
    },
    plugins: {
      legend: { display: false },
      tooltip: {
        enabled: true,
        mode: "index",
        intersect: false,
        callbacks: tooltipCallbacks,
        backgroundColor: isLight
          ? "rgba(255, 255, 255, 0.95)"
          : "rgba(28, 28, 30, 0.95)",
        titleColor: isLight ? "#1c1c1e" : "#f5f5f7",
        bodyColor: isLight ? "#1c1c1e" : "#f5f5f7",
        footerColor: isLight ? "#86868b" : "#8E8E93",
        borderColor: isLight
          ? "rgba(0, 0, 0, 0.1)"
          : "rgba(255, 255, 255, 0.1)",
        borderWidth: 1,
        cornerRadius: 8,
        padding: 12,
        displayColors: true,
        usePointStyle: true,
        boxPadding: 4,
        titleFont: { weight: "600" },
      },
      annotation:
        Object.keys(annotationsConfig).length > 0
          ? { annotations: annotationsConfig }
          : undefined,
    },
    scales: {
      x: {
        offset: !isLineChart, // 柱状图保留 offset，折线图撑满
        grid: { display: false },
        ticks: {
          color: tickColor,
          callback: tickCallback || undefined,
          maxRotation: 0,
          autoSkip: false,
        },
      },
      y: isLineChart
        ? {
            display: true,
            position: "left",
            beginAtZero: true,
            max: trendYMax || undefined,
            grid: {
              color: gridColor,
              drawBorder: false,
              borderDash: [4, 4],
            },
            ticks: {
              color: tickColor,
              padding: 8,
              callback: (value) => {
                if (value >= 1024) return `${(value / 1024).toFixed(1)} TB`;
                if (value >= 1) return `${value.toFixed(1)} GB`;
                return `${(value * 1024).toFixed(0)} MB`;
              },
            },
            border: { display: false },
          }
        : {
            display: false,
            beginAtZero: true,
          },
    },
  };

  const ctx = document.getElementById("trend-chart").getContext("2d");
  if (trendChart && trendChartType !== chartType) {
    trendChart.destroy();
    trendChart = null;
  }
  trendChartType = chartType;

  // 自定义 Crosshair 插件（仅对折线图生效）
  const crosshairPlugin = {
    id: "trendCrosshair",
    afterDraw: (chart) => {
      if (chart.tooltip?._active?.length && chartType === "line") {
        const activePoint = chart.tooltip._active[0];
        const { ctx: chartCtx } = chart;
        const { top, bottom } = chart.chartArea;
        const x = activePoint.element.x;

        chartCtx.save();
        chartCtx.beginPath();
        chartCtx.moveTo(x, top);
        chartCtx.lineTo(x, bottom);
        chartCtx.lineWidth = 1;
        chartCtx.strokeStyle = isLight
          ? "rgba(0, 0, 0, 0.15)"
          : "rgba(255, 255, 255, 0.25)";
        chartCtx.setLineDash([4, 4]);
        chartCtx.stroke();
        chartCtx.restore();
      }
    },
  };

  if (trendChart) {
    trendChart.data.labels = labels;
    trendChart.data.datasets = datasets;
    trendChart.options = options;
    trendChart.update("none");
  } else {
    trendChart = new Chart(ctx, {
      type: chartType,
      data: { labels, datasets },
      options,
      plugins: [crosshairPlugin],
    });
  }
}

// 视图切换
function setupTrendToggle() {
  const detailBtn = document.getElementById("trend-detail");
  const totalBtn = document.getElementById("trend-total");
  const rangeMonthBtn = document.getElementById("trend-range-month");
  const range30Btn = document.getElementById("trend-range-30d");
  const rangeCycleBtn = document.getElementById("trend-range-cycle");

  if (detailBtn) {
    detailBtn.addEventListener("click", () => {
      trendView = "detail";
      updateTrendToggleState();
      if (trendRange === "monthly") {
        renderTrendChart();
      }
    });
  }

  if (totalBtn) {
    totalBtn.addEventListener("click", () => {
      trendView = "total";
      updateTrendToggleState();
      if (trendRange === "monthly") {
        renderTrendChart();
      }
    });
  }

  if (rangeMonthBtn) {
    rangeMonthBtn.addEventListener("click", () => {
      trendRange = "monthly";
      updateTrendToggleState();
      renderTrendChart();
    });
  }

  if (range30Btn) {
    range30Btn.addEventListener("click", () => {
      trendRange = "30d";
      updateTrendToggleState();
      if (trendDailyData) {
        renderTrendChart();
        return;
      }
      fetchDailyTrend("30d");
    });
  }

  if (rangeCycleBtn) {
    rangeCycleBtn.addEventListener("click", () => {
      trendRange = "cycle";
      updateTrendToggleState();
      if (trendCycleData) {
        renderTrendChart();
        return;
      }
      fetchDailyTrend("cycle");
    });
  }

  updateTrendToggleState();
}

// 初始化
// 拉取通知配置：未配置 Telegram 则隐藏胶囊，保持顶栏干净；
// 已配置则显示胶囊（每日报告开启时文案为推送时刻）并填充弹层状态
async function fetchNotifyStatus() {
  const pill = document.getElementById("notify-pill");
  const pillText = document.getElementById("notify-pill-text");
  const tgStatus = document.getElementById("tg-pop-status");
  const drStatus = document.getElementById("dr-pop-status");
  if (!pill) return;
  try {
    const res = await fetch("/api/config");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const cfg = await res.json();

    if (!cfg.telegram_enabled) {
      pill.hidden = true;
      return;
    }
    pill.hidden = false;

    const dr = cfg.daily_report || {};
    const timeLabel =
      dr.enabled && dr.hour != null
        ? `每日 ${String(dr.hour).padStart(2, "0")}:00`
        : null;

    if (pillText) pillText.textContent = timeLabel || "通知";
    if (tgStatus) {
      tgStatus.textContent = "已配置";
      tgStatus.className = "notify-badge is-on";
    }
    if (drStatus) {
      drStatus.textContent = timeLabel ? `每天 ${timeLabel.slice(3)}` : "已关闭";
      drStatus.className = "notify-badge " + (dr.enabled ? "is-on" : "is-off");
    }
  } catch (e) {
    console.error("获取通知配置失败", e);
    pill.hidden = true; // 拿不到状态时不显示，避免误导
  }
}

// 通知胶囊：点击开合弹层（点击外部 / Esc 关闭），弹层内「发送测试消息」即时验证
function setupNotifyPill() {
  const pill = document.getElementById("notify-pill");
  const pop = document.getElementById("notify-popover");
  if (!pill || !pop) return;

  const result = document.getElementById("notify-test-result");
  const setOpen = (open) => {
    pop.hidden = !open;
    pill.setAttribute("aria-expanded", String(open));
    // 每次重新打开清掉上次的测试结果，避免残留旧的成功/失败提示
    if (open && result) {
      result.textContent = "";
      result.className = "notify-result";
    }
  };

  pill.addEventListener("click", (e) => {
    e.stopPropagation();
    setOpen(pop.hidden);
  });
  pop.addEventListener("click", (e) => e.stopPropagation());
  document.addEventListener("click", () => {
    if (!pop.hidden) setOpen(false);
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !pop.hidden) setOpen(false);
  });

  wireNotifyButton("notify-test-btn", "/api/notify/test", result);
  wireNotifyButton("notify-daily-btn", "/api/notify/daily-report", result);
}

// wireNotifyButton 给弹层内按钮绑定「点击 -> POST 触发发送 -> 回显结果」逻辑，
// 发送期间禁用按钮避免重复点击；成功/失败都把后端 message 回显到共享结果区。
function wireNotifyButton(btnId, url, result) {
  const btn = document.getElementById(btnId);
  if (!btn) return;
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    const original = btn.textContent;
    btn.textContent = "发送中…";
    if (result) {
      result.textContent = "";
      result.className = "notify-result";
    }
    try {
      const res = await fetch(url, { method: "POST" });
      const data = await res.json().catch(() => ({}));
      if (result) {
        result.textContent = data.message || (res.ok ? "已发送" : "发送失败");
        result.className = "notify-result " + (res.ok ? "is-ok" : "is-err");
      }
    } catch (e) {
      if (result) {
        result.textContent = "请求失败：" + e.message;
        result.className = "notify-result is-err";
      }
    } finally {
      btn.textContent = original;
      btn.disabled = false;
    }
  });
}

document.addEventListener("DOMContentLoaded", () => {
  fetchStats();
  fetchSystem();
  fetchMonthlyTrend();
  initRealtimeChart();
  connectRealtime();
  setupTrendToggle();
  initThemeToggle();
  fetchNotifyStatus();
  setupNotifyPill();

  // 延迟监控时间选择器
  const latencyEndEl = document.getElementById("latency-end");
  const latencyStartEl = document.getElementById("latency-start");
  const latencyQueryBtn = document.getElementById("latency-query");
  const latencyRecentBtn = document.getElementById("latency-recent");
  const latencyResetBtn = document.getElementById("latency-reset");
  const datePrevBtn = document.getElementById("date-prev");
  const dateNextBtn = document.getElementById("date-next");

  // 设置默认日期（今天）
  const today = formatDateValue(new Date());
  if (latencyEndEl) latencyEndEl.value = "";
  if (latencyStartEl) latencyStartEl.value = "";
  latencyStartDate = null;
  latencyEndDate = null;

  fetchLatency();

  // 查询按钮
  if (latencyQueryBtn) {
    latencyQueryBtn.addEventListener("click", () => {
      const { start, end } = normalizeRange(
        latencyStartEl?.value,
        latencyEndEl?.value,
      );

      if (!start && !end) {
        latencyStartDate = null;
        latencyEndDate = null;
        fetchLatency();
        return;
      }

      if (latencyStartEl) latencyStartEl.value = start;
      if (latencyEndEl) latencyEndEl.value = end;
      latencyStartDate = start;
      latencyEndDate = end;
      latencyZoom = { start: 0, end: 100 };
      fetchLatency(start, end);
    });
  }

  if (latencyRecentBtn) {
    latencyRecentBtn.addEventListener("click", () => {
      if (latencyStartEl) latencyStartEl.value = "";
      if (latencyEndEl) latencyEndEl.value = "";
      latencyStartDate = null;
      latencyEndDate = null;
      latencyZoom = { start: 0, end: 100 };
      fetchLatency();
    });
  }

  // 前一天/后一天
  if (datePrevBtn && latencyEndEl) {
    datePrevBtn.addEventListener("click", () => {
      const { start, end } = normalizeRange(
        latencyStartEl?.value,
        latencyEndEl?.value,
      );
      const baseStart = start || today;
      const baseEnd = end || today;
      const newStart = shiftDateValue(baseStart, -1);
      const newEnd = shiftDateValue(baseEnd, -1);

      if (latencyStartEl) latencyStartEl.value = newStart;
      if (latencyEndEl) latencyEndEl.value = newEnd;
      latencyStartDate = newStart;
      latencyEndDate = newEnd;
      latencyZoom = { start: 0, end: 100 };
      fetchLatency(newStart, newEnd);
    });
  }

  if (dateNextBtn && latencyEndEl) {
    dateNextBtn.addEventListener("click", () => {
      const { start, end } = normalizeRange(
        latencyStartEl?.value,
        latencyEndEl?.value,
      );
      const baseStart = start || today;
      const baseEnd = end || today;
      const newStart = shiftDateValue(baseStart, 1);
      const newEnd = shiftDateValue(baseEnd, 1);
      if (newEnd > today) {
        if (latencyStartEl) latencyStartEl.value = "";
        if (latencyEndEl) latencyEndEl.value = "";
        latencyStartDate = null;
        latencyEndDate = null;
        latencyZoom = { start: 0, end: 100 };
        fetchLatency();
        return;
      }

      if (latencyStartEl) latencyStartEl.value = newStart;
      if (latencyEndEl) latencyEndEl.value = newEnd;
      latencyStartDate = newStart;
      latencyEndDate = newEnd;
      latencyZoom = { start: 0, end: 100 };
      fetchLatency(newStart, newEnd);
    });
  }

  // 显示选项事件监听
  document
    .getElementById("show-max")
    ?.addEventListener("change", renderLatencyChart);
  document
    .getElementById("show-avg")
    ?.addEventListener("change", renderLatencyChart);
  document
    .getElementById("show-loss")
    ?.addEventListener("change", renderLatencyChart);

  // 重置按钮
  if (latencyResetBtn) {
    latencyResetBtn.addEventListener("click", () => {
      if (latencyStartEl) latencyStartEl.value = "";
      if (latencyEndEl) latencyEndEl.value = "";
      latencyStartDate = null;
      latencyEndDate = null;
      latencyZoom = { start: 0, end: 100 };
      fetchLatency();
    });
  }

  // 定时刷新
  setInterval(fetchStats, 60000); // 1 分钟
  setInterval(fetchSystem, 5000); // 5 秒
  setInterval(() => {
    // 延迟监控：如果有自定义时间范围则用该范围刷新，否则用默认
    if (latencyStartDate && latencyEndDate) {
      fetchLatency(latencyStartDate, latencyEndDate);
    } else {
      fetchLatency();
    }
  }, 60000); // 1 分钟
  setInterval(fetchMonthlyTrend, 3600000); // 1 小时

  // 页面休眠恢复机制（Chrome 后台标签页休眠后恢复刷新）
  let lastActiveTime = Date.now();
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      const now = Date.now();
      const elapsed = now - lastActiveTime;
      // 如果离开超过 30 秒，立即刷新所有数据
      if (elapsed > 30000) {
        fetchStats();
        fetchSystem();
        fetchMonthlyTrend();
        if (latencyStartDate && latencyEndDate) {
          fetchLatency(latencyStartDate, latencyEndDate);
        } else {
          fetchLatency();
        }
        // SSE 会自动重连，无需手动处理
      }
      lastActiveTime = now;
    } else {
      lastActiveTime = Date.now();
    }
  });
});
