// CrazyGet 前端逻辑：qBittorrent 风格任务表格 + 过滤/排序/信息面板/右键菜单
"use strict";

// 运行时错误显示（由 index.html 内联钩子调用；正常状态下状态栏无痕）
function probe(msg) {
  try {
    const el = document.getElementById("st-dht");
    if (el) {
      el.textContent = "⚠ " + msg;
      el.dataset.err = "1";
    }
  } catch {}
}

const $ = (id) => document.getElementById(id);
const API = window.go.main.App;

let tasks = [];
let selectedID = null;
let cfg = null;

// 视图状态
let statusFilter = "all";
let kindFilter = "all";
let sortKey = "createdAt";
let sortDir = -1; // -1 降序（默认新任务在前）
let activeTab = "overview";
let infoCollapsed = false;

/* ================= 工具 ================= */
function fmtBytes(n) {
  if (n == null || n < 0) return "—";
  const u = 1024;
  if (n < u) return n + " B";
  let div = u, exp = 0;
  for (let m = n / u; m >= u; m /= u) { div *= u; exp++; }
  return (n / div).toFixed(1) + " " + "KMGTPE"[exp] + "B";
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function etaSeconds(t) {
  if (t.size > 0 && t.speed > 0 && t.status !== "已完成") {
    return Math.ceil((t.size - t.completed) / t.speed);
  }
  return -1;
}

function fmtEta(sec) {
  if (sec < 0 || !isFinite(sec)) return "—";
  if (sec < 1) return "0s";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function fmtTime(iso) {
  if (!iso) return "—";
  try {
    const d = new Date(iso);
    const pad = (x) => String(x).padStart(2, "0");
    return `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  } catch { return "—"; }
}

const KIND_LABEL = { http: "HTTP", ftp: "FTP", bt: "BT" };

/* ================= 过滤与排序 ================= */
function statusBucket(st) {
  switch (st) {
    case "下载中":
    case "获取元数据": return "downloading";
    case "等待中": return "waiting";
    case "已暂停": return "paused";
    case "已完成": return "completed";
    case "失败": return "error";
    default: return "waiting";
  }
}

const STATUS_ORDER = {
  "下载中": 0, "获取元数据": 1, "等待中": 2, "已暂停": 3, "失败": 4, "已完成": 5,
};

function filteredTasks() {
  let list = tasks.filter((t) => {
    if (statusFilter !== "all" && statusBucket(t.status) !== statusFilter) return false;
    if (kindFilter !== "all" && t.kind !== kindFilter) return false;
    return true;
  });
  const keyFns = {
    name: (t) => (t.name || "").toLowerCase(),
    size: (t) => (t.size < 0 ? -1 : t.size),
    progress: (t) => (t.size > 0 ? t.completed / t.size : -1),
    status: (t) => STATUS_ORDER[t.status] ?? 9,
    speed: (t) => t.speed || 0,
    eta: (t) => etaSeconds(t) < 0 ? Number.MAX_SAFE_INTEGER : etaSeconds(t),
    createdAt: (t) => new Date(t.createdAt).getTime() || 0,
  };
  const fn = keyFns[sortKey] || keyFns.createdAt;
  list = [...list].sort((a, b) => {
    const va = fn(a), vb = fn(b);
    if (typeof va === "string" || typeof vb === "string") {
      return String(va).localeCompare(String(vb), "zh-CN") * sortDir;
    }
    return (va - vb) * sortDir;
  });
  return list;
}

function updateFilterCounts() {
  const buckets = { all: tasks.length, downloading: 0, waiting: 0, paused: 0, completed: 0, error: 0 };
  for (const t of tasks) buckets[statusBucket(t.status)]++;
  $("c-all").textContent = buckets.all;
  $("c-downloading").textContent = buckets.downloading;
  $("c-waiting").textContent = buckets.waiting;
  $("c-paused").textContent = buckets.paused;
  $("c-completed").textContent = buckets.completed;
  $("c-error").textContent = buckets.error;
}

/* ================= 表格渲染（行级 diff：保留行 DOM，动画/过渡不因轮询重建而抖动） ================= */
const rowMap = new Map(); // id -> { tr, refs }

function makeRow(t) {
  const tr = document.createElement("tr");
  tr.dataset.id = t.id;
  tr.innerHTML = `
    <td><div class="t-name-cell"><span class="kind-badge"></span><span class="t-name-text"></span></div></td>
    <td class="num-cell t-size"></td>
    <td><div class="prog-cell"><div class="prog-track"><div class="prog-fill"></div></div><span class="prog-pct"></span></div></td>
    <td><span class="st-badge"></span></td>
    <td class="num-cell t-speed"></td>
    <td class="num-cell t-eta"></td>
    <td class="num-cell dim t-time"></td>`;
  const refs = {
    nameCell: tr.querySelector("td"),
    kind: tr.querySelector(".kind-badge"),
    name: tr.querySelector(".t-name-text"),
    size: tr.querySelector(".t-size"),
    fill: tr.querySelector(".prog-fill"),
    pct: tr.querySelector(".prog-pct"),
    badge: tr.querySelector(".st-badge"),
    speed: tr.querySelector(".t-speed"),
    eta: tr.querySelector(".t-eta"),
    time: tr.querySelector(".t-time"),
  };
  const entry = { tr, refs };
  rowMap.set(t.id, entry);
  return entry;
}

function updateRow(t, refs, selected) {
  const tr = refs.nameCell.closest("tr");
  tr.classList.toggle("selected", selected);

  const kind = KIND_LABEL[t.kind] || String(t.kind).toUpperCase();
  const kindCls = "kind-" + (t.kind === "bt" ? "bt" : t.kind === "ftp" ? "ftp" : "http");
  refs.kind.textContent = kind;
  refs.kind.className = "kind-badge " + kindCls;
  refs.name.textContent = t.name;
  refs.nameCell.title = t.name;

  const unknown = t.size < 0;
  refs.size.textContent = unknown
    ? (t.completed > 0 ? fmtBytes(t.completed) : "未知")
    : `${fmtBytes(t.completed)} / ${fmtBytes(t.size)}`;

  const active = t.status === "下载中" || t.status === "获取元数据";
  let fillCls = "prog-fill";
  if (t.status === "已完成") fillCls += " done";
  else if (!active && t.status !== "失败") fillCls += " stalled";

  const pct = t.size > 0 ? Math.min(100, (t.completed / t.size) * 100) : -1;
  if (pct >= 0) {
    refs.fill.className = fillCls;
    refs.fill.style.width = pct + "%";
    refs.pct.textContent = pct.toFixed(1) + "%";
  } else if (t.status === "已完成") {
    refs.fill.className = fillCls;
    refs.fill.style.width = "100%";
    refs.pct.textContent = "100%";
  } else {
    refs.fill.className = fillCls + " indeterminate";
    refs.pct.textContent = "—";
  }

  if (t.status === "失败" && t.errMsg) {
    refs.badge.className = "st-badge error";
    refs.badge.textContent = "失败";
    refs.badge.title = t.errMsg;
  } else {
    refs.badge.className = "st-badge " + badgeClass(t.status);
    refs.badge.textContent = t.status;
    refs.badge.title = "";
  }

  // 活动任务恒显速度（含短暂 0），避免速度列间歇性消失
  refs.speed.innerHTML = active
    ? `<span class="speed-on">${fmtBytes(Math.max(t.speed || 0, 0))}/s</span>`
    : (t.speed > 0 ? `<span class="speed-on">${fmtBytes(t.speed)}/s</span>` : `<span class="dim">—</span>`);
  refs.eta.textContent = active ? fmtEta(etaSeconds(t)) : "—";
  refs.time.textContent = fmtTime(t.createdAt);
}

function renderTable() {
  const list = filteredTasks();
  const tbody = $("tbody");
  $("empty").classList.toggle("hidden", list.length > 0);

  // 表头排序指示
  document.querySelectorAll("#task-table thead th").forEach((th) => {
    const ind = th.querySelector(".sort-ind");
    ind.textContent = th.dataset.sort === sortKey ? (sortDir === 1 ? "▲" : "▼") : "";
  });

  if (selectedID && !tasks.some((t) => t.id === selectedID)) selectedID = null;

  // 移除已不存在的行
  for (const [id, entry] of Array.from(rowMap)) {
    if (!list.some((t) => t.id === id)) {
      entry.tr.remove();
      rowMap.delete(id);
    }
  }
  // 更新 + 按目标顺序排列（appendChild 对已在末尾的节点是零操作，动画不重置）
  for (const t of list) {
    const entry = rowMap.get(t.id) || makeRow(t);
    updateRow(t, entry.refs, t.id === selectedID);
    if (tbody.lastElementChild !== entry.tr) tbody.appendChild(entry.tr);
  }
}

function badgeClass(status) {
  switch (status) {
    case "下载中": return "running";
    case "已完成": return "completed";
    case "已暂停": return "paused";
    case "失败": return "error";
    case "获取元数据": return "metadata";
    default: return "";
  }
}

/* ================= 信息面板 ================= */
function renderInfo() {
  const body = $("info-body");
  const t = tasks.find((x) => x.id === selectedID);
  if (!t) {
    body.innerHTML = `<div class="info-placeholder">选中一个任务查看详情</div>`;
    return;
  }
  if (activeTab === "overview") {
    const pct = t.size > 0 ? ((t.completed / t.size) * 100).toFixed(1) + "%" : "—";
    body.innerHTML = `<div class="kv-grid">
      <div class="kv"><span class="k">名称</span><span class="v" title="${esc(t.name)}">${esc(t.name)}</span></div>
      <div class="kv"><span class="k">状态</span><span class="v">${esc(t.status)}${t.errMsg ? " · " + esc(t.errMsg) : ""}</span></div>
      <div class="kv"><span class="k">类型</span><span class="v">${esc(KIND_LABEL[t.kind] || t.kind)}</span></div>
      <div class="kv"><span class="k">大小</span><span class="v">${t.size < 0 ? "未知" : fmtBytes(t.size)}</span></div>
      <div class="kv"><span class="k">已下载</span><span class="v">${fmtBytes(t.completed)}（${pct}）</span></div>
      <div class="kv"><span class="k">速度</span><span class="v">${t.speed > 0 ? fmtBytes(t.speed) + "/s" : "—"}</span></div>
      <div class="kv"><span class="k">剩余时间</span><span class="v">${t.status === "下载中" ? fmtEta(etaSeconds(t)) : "—"}</span></div>
      <div class="kv"><span class="k">线程数</span><span class="v">${t.threads}</span></div>
      <div class="kv"><span class="k">保存目录</span><span class="v" title="${esc(t.saveDir)}">${esc(t.saveDir)}</span></div>
      <div class="kv"><span class="k">添加时间</span><span class="v">${fmtTime(t.createdAt)}</span></div>
      <div class="kv"><span class="k">完成时间</span><span class="v">${t.finishedAt ? fmtTime(t.finishedAt) : "—"}</span></div>
      ${t.infoHash ? `<div class="kv"><span class="k">InfoHash</span><span class="v">${esc(t.infoHash)}</span></div>` : ""}
    </div>`;
  } else if (activeTab === "segments") {
    const segs = t.segments || [];
    if (!segs.length) {
      body.innerHTML = `<div class="info-placeholder">${t.kind === "bt" ? "BT 任务没有分段信息" : "该任务没有分段数据（单流模式或已完成清理）"}</div>`;
      return;
    }
    body.innerHTML = `<div class="seg-list">` + segs.map((s, i) => {
      const len = s.end - s.start + 1;
      const p = Math.min(100, (s.done / len) * 100);
      return `<div class="seg-row">
        <span class="seg-label">段 ${String(i + 1).padStart(2, "0")}</span>
        <div class="seg-track"><div class="seg-fill ${p >= 99.9 ? "full" : ""}" style="width:${p}%"></div></div>
        <span class="seg-pct">${p.toFixed(0)}%</span>
        <span class="seg-label" style="width:auto">${fmtBytes(s.done)} / ${fmtBytes(len)}</span>
      </div>`;
    }).join("") + `</div>`;
  } else if (activeTab === "link") {
    body.innerHTML = `<textarea class="link-box" readonly>${esc(t.url)}</textarea>`;
  }
}

/* ================= 状态栏 ================= */
function renderStatus() {
  let speed = 0, running = 0, done = 0;
  for (const t of tasks) {
    if (t.status === "下载中" || t.status === "获取元数据") { running++; speed += t.speed || 0; }
    if (t.status === "已完成") done++;
  }
  $("st-speed").textContent = `↓ ${fmtBytes(speed)}/s`;
  $("st-count").textContent = `任务 ${tasks.length} ｜ 下载中 ${running} ｜ 已完成 ${done}`;
  const sel = tasks.find((x) => x.id === selectedID);
  $("st-sel").textContent = sel
    ? `已选中：${sel.name}（${sel.kind.toUpperCase()}）`
    : "";
  // 若状态栏正显示运行时错误（⚠ 前缀），保留不被覆盖
  const dhtEl = $("st-dht");
  if (!dhtEl.dataset.err) dhtEl.textContent = "DHT: —";
}

/* ================= 轮询与刷新 ================= */
async function poll() {
  try {
    tasks = (await API.GetTasks()) || [];
    updateFilterCounts();
    renderTable();
    renderInfo();
    renderStatus();
  } catch (e) { /* 窗口关闭等场景忽略 */ }
}

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme === "light" ? "light" : "dark";
}

/* ================= 操作 ================= */
function requireSelected() {
  const t = tasks.find((x) => x.id === selectedID);
  if (!t) return null;
  return t;
}

async function removeTask(id) {
  const t = tasks.find((x) => x.id === id);
  if (!t) return;
  if (!confirm(`确定删除任务「${t.name}」吗？\n（将同时删除已下载的数据）`)) return;
  await API.RemoveTask(id, true);
  if (selectedID === id) selectedID = null;
  poll();
}

async function openNewDialog(preset) {
  try {
    if ($("dlg-new").open) return; // 已打开则不重复弹（避免 showModal 报错）
    if (!cfg) cfg = await API.GetConfig();
    $("new-dir").value = cfg.saveDir;
    $("new-url").value = preset || "";
    $("dlg-new").showModal();
    $("new-url").focus();
  } catch (e) {
    document.title = "添加错误: " + (e && e.message ? e.message : e);
    alert("打开添加窗口失败：" + (e && e.message ? e.message : e));
  }
}

async function submitNew() {
  const lines = $("new-url").value.split(/\n+/).map((s) => s.trim()).filter(Boolean);
  if (!lines.length) return;
  const dir = $("new-dir").value.trim();
  const threads = parseInt($("new-threads").value, 10) || 8;
  for (const url of lines) {
    try {
      const t = await API.AddTask(url, dir);
      if (t && t.kind !== "bt") await API.SetThreads(t.id, threads);
    } catch (e) {
      alert("「" + url + "」添加失败：" + (e && e.message ? e.message : e));
    }
  }
  $("dlg-new").close();
  poll();
}

async function openSettings() {
  cfg = await API.GetConfig();
  $("set-dir").value = cfg.saveDir;
  $("set-conc").value = String(cfg.maxConcurrent);
  $("set-threads").value = String(cfg.threads);
  $("set-dl").value = cfg.btDownloadKB || 0;
  $("set-ul").value = cfg.btUploadKB || 0;
  $("set-theme").value = cfg.theme || "dark";
  $("set-clip").checked = !cfg.disableClipboardWatch;
  $("dlg-settings").showModal();
}

async function submitSettings() {
  const newCfg = {
    saveDir: $("set-dir").value.trim(),
    maxConcurrent: parseInt($("set-conc").value, 10) || 3,
    threads: parseInt($("set-threads").value, 10) || 8,
    btDownloadKB: parseInt($("set-dl").value, 10) || 0,
    btUploadKB: parseInt($("set-ul").value, 10) || 0,
    theme: $("set-theme").value,
    disableClipboardWatch: !$("set-clip").checked,
  };
  await API.SaveConfig(newCfg);
  cfg = newCfg;
  applyTheme(cfg.theme);
  $("dlg-settings").close();
}

/* ================= 右键菜单 ================= */
const ctxMenu = $("ctx-menu");

function showCtxMenu(x, y, task) {
  ctxMenu.dataset.id = task.id;
  const running = task.status === "下载中" || task.status === "获取元数据";
  ctxMenu.querySelector('[data-act="start"]').style.display = running ? "none" : "";
  ctxMenu.querySelector('[data-act="pause"]').style.display = running ? "" : "none";
  ctxMenu.classList.remove("hidden");
  const mw = ctxMenu.offsetWidth, mh = ctxMenu.offsetHeight;
  ctxMenu.style.left = Math.min(x, innerWidth - mw - 4) + "px";
  ctxMenu.style.top = Math.min(y, innerHeight - mh - 4) + "px";
}

function hideCtxMenu() { ctxMenu.classList.add("hidden"); }

/* ================= 事件绑定 ================= */
function initEvents() {
  // 工具栏
  $("tb-add").addEventListener("click", () => openNewDialog());
  $("tb-torrent").addEventListener("click", async () => {
    try {
      const path = await API.PickTorrent();
      if (path) { await API.AddTask(path, ""); poll(); }
    } catch (e) {
      document.title = "种子错误: " + (e && e.message ? e.message : e);
      alert("选择种子失败：" + (e && e.message ? e.message : e));
    }
  });
  $("tb-start").addEventListener("click", () => { if (selectedID) API.StartTask(selectedID); });
  $("tb-pause").addEventListener("click", () => { if (selectedID) API.PauseTask(selectedID); });
  $("tb-folder").addEventListener("click", () => { if (selectedID) API.OpenFolder(selectedID); });
  $("tb-remove").addEventListener("click", () => { if (selectedID) removeTask(selectedID); });
  $("tb-startall").addEventListener("click", () => API.StartAll());
  $("tb-pauseall").addEventListener("click", () => API.PauseAll());
  $("tb-clear").addEventListener("click", () => API.ClearCompleted());
  $("tb-settings").addEventListener("click", openSettings);

  // 过滤栏
  document.querySelectorAll("#status-filters .filter-item").forEach((el) => {
    el.addEventListener("click", () => {
      document.querySelectorAll("#status-filters .filter-item").forEach((x) => x.classList.remove("active"));
      el.classList.add("active");
      statusFilter = el.dataset.filter;
      renderTable();
    });
  });
  document.querySelectorAll("#kind-filters .filter-item").forEach((el) => {
    el.addEventListener("click", () => {
      document.querySelectorAll("#kind-filters .filter-item").forEach((x) => x.classList.remove("active"));
      el.classList.add("active");
      kindFilter = el.dataset.kind;
      renderTable();
    });
  });

  // 表头排序
  document.querySelectorAll("#task-table thead th").forEach((th) => {
    th.addEventListener("click", () => {
      const key = th.dataset.sort;
      if (sortKey === key) sortDir = -sortDir;
      else { sortKey = key; sortDir = key === "createdAt" ? -1 : 1; }
      renderTable();
    });
  });

  // 表格：选中 / 双击 / 右键
  const tbody = $("tbody");
  tbody.addEventListener("click", (e) => {
    const tr = e.target.closest("tr[data-id]");
    if (!tr) return;
    selectedID = tr.dataset.id;
    renderTable(); renderInfo(); renderStatus();
  });
  tbody.addEventListener("dblclick", (e) => {
    const tr = e.target.closest("tr[data-id]");
    if (tr) API.OpenFolder(tr.dataset.id);
  });
  tbody.addEventListener("contextmenu", (e) => {
    const tr = e.target.closest("tr[data-id]");
    if (!tr) return;
    e.preventDefault();
    selectedID = tr.dataset.id;
    renderTable(); renderInfo(); renderStatus();
    const t = requireSelected();
    if (t) showCtxMenu(e.clientX, e.clientY, t);
  });

  // 右键菜单动作
  ctxMenu.addEventListener("click", (e) => {
    const item = e.target.closest(".ctx-item");
    if (!item) return;
    const id = ctxMenu.dataset.id;
    const act = item.dataset.act;
    hideCtxMenu();
    if (act === "start") API.StartTask(id);
    if (act === "pause") API.PauseTask(id);
    if (act === "folder") API.OpenFolder(id);
    if (act === "copy") {
      const t = tasks.find((x) => x.id === id);
      if (t) navigator.clipboard.writeText(t.url).catch(() => {});
    }
    if (act === "remove") removeTask(id);
    poll();
  });
  document.addEventListener("click", (e) => {
    if (!ctxMenu.contains(e.target)) hideCtxMenu();
  });
  document.addEventListener("scroll", hideCtxMenu, true);

  // 信息面板
  document.querySelectorAll(".info-tab").forEach((el) => {
    el.addEventListener("click", () => {
      document.querySelectorAll(".info-tab").forEach((x) => x.classList.remove("active"));
      el.classList.add("active");
      activeTab = el.dataset.tab;
      renderInfo();
    });
  });
  $("info-toggle").addEventListener("click", () => {
    infoCollapsed = !infoCollapsed;
    $("info-pane").classList.toggle("collapsed", infoCollapsed);
    $("info-toggle").textContent = infoCollapsed ? "▴" : "▾";
  });

  // 键盘
  document.addEventListener("keydown", (e) => {
    if (e.key === "Delete" && selectedID) { removeTask(selectedID); }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "v") {
      navigator.clipboard.readText().then((txt) => {
        if (txt && /^\s*(https?|ftp|magnet):/i.test(txt)) openNewDialog(txt.trim());
      }).catch(() => {});
    }
    if (e.key === "F2") { /* 预留 */ }
  });

  // 对话框
  $("new-browse").addEventListener("click", async () => {
    const d = await API.PickFolder();
    if (d) $("new-dir").value = d;
  });
  $("new-cancel").addEventListener("click", () => $("dlg-new").close());
  $("dlg-new").addEventListener("submit", (e) => { e.preventDefault(); submitNew(); });

  $("set-browse").addEventListener("click", async () => {
    const d = await API.PickFolder();
    if (d) $("set-dir").value = d;
  });
  $("set-cancel").addEventListener("click", () => $("dlg-settings").close());
  $("dlg-settings").addEventListener("submit", (e) => { e.preventDefault(); submitSettings(); });
}

/* ================= 剪贴板监听提示 ================= */
let clipURL = null;
let clipTimer = null;

function hideClipToast() {
  $("clip-toast").classList.add("hidden");
  clearTimeout(clipTimer);
}

function initClipboardEvents() {
  if (!window.runtime || !window.runtime.EventsOn) return;
  window.runtime.EventsOn("clipboard-link", (text) => {
    clipURL = text;
    $("clip-text").textContent = text.length > 76 ? text.slice(0, 76) + "…" : text;
    $("clip-text").title = text;
    $("clip-toast").classList.remove("hidden");
    clearTimeout(clipTimer);
    clipTimer = setTimeout(hideClipToast, 10000); // 10 秒后自动消失
  });
  $("clip-download").addEventListener("click", async () => {
    if (!clipURL) return;
    try {
      await API.AddTask(clipURL, "");
      hideClipToast();
      poll();
    } catch (e) {
      alert("添加失败：" + (e && e.message ? e.message : e));
    }
  });
  $("clip-ignore").addEventListener("click", () => {
    if (clipURL) API.IgnoreClipboard(clipURL); // 本次会话不再提示该链接
    hideClipToast();
  });
}

/* ================= 启动 ================= */
(async function init() {
  initEvents();
  initClipboardEvents();
  try {
    cfg = await API.GetConfig();
    applyTheme(cfg.theme);
  } catch (e) { /* 默认深色 */ }
  await poll();
  setInterval(poll, 400);
  // 诊断：检查第一个进度条的动画计算值
  setTimeout(() => {
    const f = document.querySelector(".prog-fill");
    if (f) {
      const cs = getComputedStyle(f);
      probe("anim=" + cs.animationName + " dur=" + cs.animationDuration);
    } else {
      probe("no-fill-element");
    }
  }, 1200);
})();
