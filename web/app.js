"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const defaults = { deadline_minutes: [1440, 180, 30], exam_start_minutes: [1440, 60], video_minutes: [4320, 1440, 180], quiet_enabled: true, quiet_start: "23:00", quiet_end: "08:00", timezone: "Asia/Shanghai" };
  const viewNames = { tasks: "任务总览", accounts: "平台账号", settings: "通知设置", activity: "运行记录" };
  const kindNames = { homework: "作业", exam: "考试", chapter: "章节测试", video: "视频学习" };
  const taskStatus = { pending: ["待完成", "warning"], completed: ["已完成", "success"], unknown: ["状态未知", "neutral"] };
  const eventNames = { new_task: "新任务", new: "新任务", deadline: "截止提醒", exam_start: "考试开始", video_deadline: "视频截止", time_changed: "时间变更", changed: "时间变更", login_expired: "登录过期", sync_failure: "同步故障", sync_failed: "同步故障", platform_failure: "平台故障", overdue: "截止提醒" };
  const eventStatuses = { pending: ["等待发送", "neutral"], deferred: ["静默延后", "neutral"], sent: ["已发送", "success"], failed: ["发送失败", "danger"], retry: ["等待重试", "warning"], skipped: ["已跳过", "neutral"], cancelled: ["已取消", "neutral"], canceled: ["已取消", "neutral"], expired: ["已失效", "neutral"] };
  const loginStatuses = { demo: ["演示模式", "neutral"], unbound: ["未绑定", "neutral"], not_bound: ["未绑定", "neutral"], pending: ["等待验证", "warning"], waiting: ["等待验证", "warning"], logged_in: ["已登录", "success"], authenticated: ["已登录", "success"], active: ["已登录", "success"], expired: ["已过期", "danger"], failed: ["验证失败", "danger"], unsupported: ["待接入", "neutral"], unimplemented: ["待接入", "neutral"], not_configured: ["未配置", "neutral"] };
  const syncStatuses = { demo: ["示例数据", "neutral"], idle: ["等待同步", "neutral"], pending: ["等待同步", "neutral"], success: ["同步成功", "success"], ok: ["同步成功", "success"], syncing: ["同步中", "warning"], failed: ["同步失败", "danger"], error: ["同步失败", "danger"], blocked: ["等待登录", "warning"], unavailable: ["待接入", "neutral"], unsupported: ["待接入", "neutral"], unimplemented: ["待接入", "neutral"], not_configured: ["未配置", "neutral"] };
  eventNames.fault = "平台故障";
  eventStatuses.sending = ["正在发送", "warning"];
  syncStatuses.revalidating = ["重新验证中", "warning"];
  let state = null;
  let currentView = "tasks";
  let statusFilter = "all";
  let toastTimer;
  let authenticated = false;
  let loading = false;
  let settingsDirty = false;
  let barkDirty = false;

  function node(tag, className, content) {
    const result = document.createElement(tag);
    if (className) result.className = className;
    if (content !== undefined && content !== null) result.textContent = String(content);
    return result;
  }

  function badge(label, tone = "neutral") { return node("span", `badge ${tone}`, label); }
  function timestamp(value) { const date = value ? new Date(value) : null; return date && Number.isFinite(date.getTime()) ? date : null; }
  function formatDate(value, short = false) {
    const date = timestamp(value);
    if (!date) return "未知";
    try { return new Intl.DateTimeFormat("zh-CN", { timeZone: state?.settings?.timezone || "Asia/Shanghai", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false, ...(short ? {} : { year: "numeric" }) }).format(date); }
    catch { return date.toLocaleString("zh-CN", { hour12: false }); }
  }
  function platformName(id) { return state?.platforms?.find((platform) => platform.id === id)?.name || ({ rain: "普通雨课堂", yuketang: "普通雨课堂", zhidao_shared: "知到共享课", zhidao_campus: "知到校内课", zhihuishu_shared: "知到共享课", zhihuishu_campus: "知到校内课" })[id] || id || "未知平台"; }
  function cleanStatus(mapping, value) { return mapping[value] || [value || "未知", "neutral"]; }
  function notify(message, error = false) {
    clearTimeout(toastTimer);
    $("toast").textContent = message;
    $("toast").className = `toast${error ? " error" : ""}`;
    $("toast").hidden = false;
    toastTimer = setTimeout(() => { $("toast").hidden = true; }, error ? 7000 : 4000);
  }
  function showLogin() {
    authenticated = false;
    $("boot").hidden = true;
    $("app").hidden = true;
    $("login-screen").hidden = false;
  }
  async function api(path, method = "GET", body) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 20000);
    try {
      const response = await fetch(path, { method, credentials: "same-origin", cache: "no-store", headers: { ...(method === "GET" ? {} : { "X-DueRadar": "1" }), ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal });
      const contentType = response.headers.get("content-type") || "";
      const result = contentType.includes("application/json") ? await response.json() : { message: (await response.text()).slice(0, 300) };
      if (!response.ok) {
        if (response.status === 401 && path !== "/api/login") showLogin();
        throw new Error(result.error || result.message || `请求失败（${response.status}）`);
      }
      return result;
    } catch (error) {
      if (error.name === "AbortError") throw new Error("请求超时，请稍后重试。");
      throw error;
    } finally { clearTimeout(timeout); }
  }
  async function busy(button, action) {
    button.disabled = true;
    try { await action(); } catch (error) { notify(error.message || "操作失败，请稍后重试。", true); }
    finally { button.disabled = false; }
  }
  function changeView(view, updateHash = true) {
    if (!Object.hasOwn(viewNames, view)) view = "tasks";
    currentView = view;
    for (const key of Object.keys(viewNames)) $("view-" + key).hidden = key !== view;
    document.querySelectorAll(".nav-link").forEach((button) => {
      const active = button.dataset.view === view;
      button.classList.toggle("active", active);
      if (active) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
    });
    $("breadcrumb-view").textContent = viewNames[view];
    document.title = `${viewNames[view]} · DueRadar`;
    if (updateHash && location.hash !== `#${view}`) history.replaceState(null, "", `#${view}`);
  }

  function emptyState(title, message, action) {
    const wrap = node("div", "empty-state");
    const icon = node("span", "empty-icon", "◎"); icon.setAttribute("aria-hidden", "true");
    wrap.append(icon, node("h3", "", title), node("p", "", message));
    if (action) { const button = node("button", "button secondary", action.label); button.type = "button"; button.addEventListener("click", action.run); wrap.append(button); }
    return wrap;
  }
  function deadlineLabel(task) {
    const date = timestamp(task.due_at);
    if (task.status === "completed") return { main: "已完成", sub: date ? `${formatDate(task.due_at, true)} 截止` : "平台未提供截止时间", urgent: false };
    if (!date) return { main: "截止时间未知", sub: "等待平台提供", urgent: false };
    const diff = date.getTime() - Date.now();
    if (diff <= 0) return { main: "已过截止时间", sub: formatDate(task.due_at, true), urgent: false };
    const minutes = Math.ceil(diff / 60000);
    const label = minutes < 60 ? `${minutes} 分钟后截止` : minutes < 1440 ? `${Math.ceil(minutes / 60)} 小时后截止` : `${Math.ceil(minutes / 1440)} 天后截止`;
    return { main: label, sub: formatDate(task.due_at, true), urgent: diff <= 86400000 };
  }
  function renderTasks() {
    const tasks = state.tasks || [];
    const query = $("task-search").value.trim().toLocaleLowerCase();
    const platform = $("platform-filter").value;
    const filtered = tasks.filter((task) => (statusFilter === "all" || task.status === statusFilter) && (platform === "all" || task.platform === platform) && (!query || `${task.title || ""} ${task.course || ""} ${platformName(task.platform)}`.toLocaleLowerCase().includes(query))).sort((a, b) => {
      if ((a.status === "completed") !== (b.status === "completed")) return a.status === "completed" ? 1 : -1;
      return (timestamp(a.due_at)?.getTime() ?? Infinity) - (timestamp(b.due_at)?.getTime() ?? Infinity);
    });
    $("task-count").textContent = filtered.length;
    $("task-list").replaceChildren();
    if (!filtered.length) {
      $("task-list").append(tasks.length ? emptyState("没有符合筛选条件的任务", "试着切换任务状态、平台，或使用其他搜索关键词。", { label: "清除筛选", run: () => { statusFilter = "all"; $("task-search").value = ""; $("platform-filter").value = "all"; updateFilterTabs(); renderTasks(); } }) : emptyState("还没有学习任务", "平台首次成功同步后，作业、考试与视频截止信息会集中出现在这里。", { label: "查看平台接入状态 →", run: () => changeView("accounts") }));
      return;
    }
    const fragment = document.createDocumentFragment();
    for (const task of filtered) {
      const card = node("article", "task-item");
      const kind = Object.hasOwn(kindNames, task.kind) ? task.kind : "homework";
      const icon = node("div", `task-kind-icon ${kind}`, { homework: "▤", exam: "◇", chapter: "▥", video: "▷" }[kind]); icon.setAttribute("aria-hidden", "true");
      const main = node("div", "task-main");
      const titleLine = node("div", "task-title-line");
      titleLine.append(node("h3", "task-title", task.title || "未命名任务"), badge(...cleanStatus(taskStatus, task.status)));
      const meta = node("div", "task-meta");
      [platformName(task.platform), task.course || "课程未知", kindNames[task.kind] || "其他任务"].forEach((value, index) => { if (index) meta.append(node("span", "separator", "·")); meta.append(node("span", "", value)); });
      main.append(titleLine, meta);
      if (task.kind === "exam") main.append(node("p", "task-start", `开始：${formatDate(task.start_at, true)}`));
      if (task.kind === "video") {
        if (typeof task.progress === "number" && Number.isFinite(task.progress)) {
          const progress = Math.max(0, Math.min(100, task.progress));
          const track = node("progress", "progress-track"); track.max = 100; track.value = progress; track.setAttribute("aria-label", "视频学习进度");
          main.append(track, node("p", "progress-label", `已学习 ${Math.round(progress)}%`));
        } else main.append(node("p", "progress-label", "学习进度未知"));
      }
      main.title = `最近观测：${formatDate(task.last_seen_at)}`;
      const deadline = deadlineLabel(task);
      const time = node("div", "task-deadline"); time.append(node("div", `deadline-main${deadline.urgent ? " urgent" : ""}`, deadline.main), node("div", "deadline-sub", deadline.sub));
      card.append(icon, main, time); fragment.append(card);
    }
    $("task-list").append(fragment);
  }
  function updateFilterTabs() { document.querySelectorAll("[data-status]").forEach((button) => { const active = button.dataset.status === statusFilter; button.classList.toggle("active", active); button.setAttribute("aria-pressed", String(active)); }); }
  function renderPlatforms() {
    $("platform-list").replaceChildren();
    if (!state.platforms.length) { $("platform-list").append(emptyState("暂无平台信息", "服务尚未返回平台状态，请稍后刷新运行状态。")); return; }
    for (const platform of state.platforms) {
      const card = node("article", "platform-card");
      card.append(node("div", "platform-avatar", (platform.name || "课").slice(0, 1)), node("h2", "", platform.name || platform.id), node("p", "platform-subtitle", "登录与采集分别验证"));
      for (const [label, mapping, value] of [["账号状态", loginStatuses, platform.login_status], ["同步状态", syncStatuses, platform.sync_status]]) { const row = node("div", "platform-status"); row.append(node("span", "", label), badge(...cleanStatus(mapping, value))); card.append(row); }
      card.append(node("p", "platform-message", platform.message || "平台登录与任务读取待验证，当前没有已确认的真实会话。"));
      card.append(node("p", "platform-date", `最近成功同步：${timestamp(platform.last_success) ? formatDate(platform.last_success) : "尚无成功记录"}`));
      if (Number(platform.failures) > 0) card.append(node("p", "platform-date", `连续失败：${Number(platform.failures)} 轮`));
      if (["rain", "zhidao_campus"].includes(platform.id) && platform.ready && !state.demo) {
        const bind = node("button", "button primary", platform.login_status === "logged_in" ? "重新扫码绑定" : (platform.id === "rain" ? "微信扫码绑定" : "知到 APP 扫码绑定"));
        bind.type = "button"; bind.addEventListener("click", () => busy(bind, () => openRainLogin(platform.id))); card.append(bind);
      }
      $("platform-list").append(card);
    }
  }
  let activeLogin;
  async function openRainLogin(id) {
    if (activeLogin) { notify("请先完成或关闭当前扫码绑定。"); return; }
    const login = { path: id === "rain" ? "/api/accounts/rain/login" : "/api/accounts/zhidao/login", timer: null, completed: false };
    activeLogin = login;
    const label = id === "rain" ? "普通雨课堂" : "知到";
    const scanner = id === "rain" ? "微信" : "知到 APP";
    let result;
    try { result = await api(login.path, "POST"); }
    catch (error) {
      try { await api(`${login.path}/cancel`, "POST"); } catch (_) { /* Keep the original creation error. */ }
      if (activeLogin === login) activeLogin = null;
      throw error;
    }
    $("rain-title").textContent = `绑定${label}`;
    $("rain-qr").alt = `${label}官方登录二维码`;
    $("rain-qr").src = result.image;
    $("rain-login-status").textContent = `请使用${scanner}扫码，并在手机上确认登录。二维码约 3 分钟有效。`;
    $("rain-dialog").showModal();
    async function poll() {
      try {
        const result = await api(login.path);
        if (activeLogin !== login || !$("rain-dialog").open) return;
        if (result.state === "logged_in") { login.completed = true; $("rain-dialog").close(); notify(`${label}登录已确认，正在读取任务。`); await refreshState(); return; }
        if (result.state === "failed") { $("rain-login-status").textContent = result.error || "二维码失效，请关闭后重新绑定。"; return; }
        if (result.state === "canceled" || !$("rain-dialog").open) return;
        login.timer = setTimeout(poll, 2000);
      } catch (error) { if (activeLogin === login && $("rain-dialog").open) $("rain-login-status").textContent = error.message; }
    }
    login.timer = setTimeout(poll, 1500);
  }
  $("rain-close").addEventListener("click", () => $("rain-dialog").close());
  $("rain-dialog").addEventListener("close", async () => {
    const login = activeLogin;
    if (!login) return;
    clearTimeout(login.timer);
    $("rain-qr").removeAttribute("src");
    try { if (!login.completed) await api(`${login.path}/cancel`, "POST"); }
    catch (_) { /* Closing the dialog still releases the local flow. */ }
    finally { if (activeLogin === login) activeLogin = null; }
  });
  function eventTitle(event) { return state.tasks.find((task) => task.id === event.task_id)?.title || event.title || (event.task_id ? `任务 ${event.task_id}` : (eventNames[event.kind] || "系统事件")); }
  function renderEvents() {
    const events = state.events || [];
    const upcoming = events.filter((event) => ["pending", "deferred", "retry"].includes(event.status)).sort((a, b) => (timestamp(a.due_at)?.getTime() ?? Infinity) - (timestamp(b.due_at)?.getTime() ?? Infinity)).slice(0, 3);
    $("upcoming-list").replaceChildren();
    if (!upcoming.length) $("upcoming-list").append(node("p", "upcoming-empty", "暂无等待发送的提醒。任务同步后，已安排的提醒会显示在这里。"));
    for (const event of upcoming) { const item = node("div", "upcoming-item"); const dot = node("span", "timeline-dot"); dot.setAttribute("aria-hidden", "true"); const content = node("div"); content.append(node("p", "upcoming-title", eventTitle(event)), node("p", "upcoming-meta", eventNames[event.kind] || event.kind || "任务提醒"), node("p", "upcoming-time", formatDate(event.due_at, true))); item.append(dot, content); $("upcoming-list").append(item); }
    $("event-count").textContent = events.length;
    $("events-empty").hidden = events.length > 0;
    $("event-list").replaceChildren();
    const fragment = document.createDocumentFragment();
    for (const event of [...events].sort((a, b) => (timestamp(b.due_at)?.getTime() || 0) - (timestamp(a.due_at)?.getTime() || 0))) {
      const row = node("tr"); const name = node("td", "", eventTitle(event)); name.append(node("small", "", eventNames[event.kind] || event.kind || "任务提醒"));
      const status = node("td"); status.append(badge(...cleanStatus(eventStatuses, event.status)));
      row.append(name, node("td", "", formatDate(event.due_at)), status, node("td", "", event.attempts ?? 0), node("td", "", event.last_error || "—")); fragment.append(row);
    }
    $("event-list").append(fragment);
  }
  function populateSettings(settings) {
    $("deadline-minutes").value = (settings.deadline_minutes || []).join(", ");
    $("exam-minutes").value = (settings.exam_start_minutes || []).join(", ");
    $("video-minutes").value = (settings.video_minutes || []).join(", ");
    $("quiet-enabled").checked = Boolean(settings.quiet_enabled);
    $("quiet-start").value = settings.quiet_start || "23:00";
    $("quiet-end").value = settings.quiet_end || "08:00";
    $("timezone").value = settings.timezone || "Asia/Shanghai";
  }
  function render() {
    const tasks = state.tasks;
    const pending = tasks.filter((task) => task.status === "pending");
    $("stat-pending").textContent = pending.length;
    $("nav-count").textContent = pending.length;
    $("stat-soon").textContent = pending.filter((task) => { const due = timestamp(task.due_at)?.getTime(); return due && due > Date.now() && due - Date.now() <= 86400000; }).length;
    $("stat-completed").textContent = tasks.filter((task) => task.status === "completed").length;
    $("stat-platforms").textContent = state.platforms.filter((platform) => ["logged_in", "authenticated", "active"].includes(platform.login_status)).length;
    $("stat-platform-total").textContent = ` / ${state.platforms.length}`;
    const recent = state.platforms.map((platform) => timestamp(platform.last_success)).filter(Boolean).sort((a, b) => b - a)[0];
    $("stat-sync").textContent = recent ? `最近同步 ${formatDate(recent, true)}` : "等待首次成功同步";
    const selectedPlatform = $("platform-filter").value;
    $("platform-filter").replaceChildren(new Option("全部平台", "all"));
    const ids = new Set([...state.platforms.map((platform) => platform.id), ...tasks.map((task) => task.platform)].filter(Boolean));
    for (const id of ids) $("platform-filter").append(new Option(platformName(id), id));
    if (ids.has(selectedPlatform)) $("platform-filter").value = selectedPlatform;
    $("demo-banner").hidden = !state.demo;
    const settings = state.settings || defaults;
    $("quiet-title").textContent = settings.quiet_enabled ? "安静的夜晚，及时的提醒" : "提醒照常，关注每个节点";
    $("quiet-summary").textContent = settings.quiet_enabled ? `${settings.quiet_start}—${settings.quiet_end} 为静默时段。普通提醒延后，后台同步照常运行。时区：${settings.timezone}。` : `静默时段已关闭。所有有效提醒按配置时间发送。时区：${settings.timezone}。`;
    if (!settingsDirty) populateSettings(settings);
    const barkConfigured = Boolean(state.bark?.configured);
    $("bark-status").textContent = barkConfigured ? "已配置" : "未配置";
    $("bark-status").className = `badge ${barkConfigured ? "success" : "neutral"}`;
    $("bark-test").disabled = !barkConfigured || state.demo;
    $("bark-form").querySelector("button[type=submit]").disabled = state.demo;
    $("bark-key").disabled = state.demo;
    $("bark-server").disabled = state.demo;
    if (!barkDirty) $("bark-server").value = state.bark?.server || "https://api.day.app";
    $("bark-key").placeholder = barkConfigured ? "已保存，留空保留当前密钥" : "输入 Bark 设备密钥";
    $("runtime-grid").replaceChildren();
    const runtime = state.runtime || {};
    const uptime = Number.isFinite(runtime.uptime_seconds) ? `${Math.floor(runtime.uptime_seconds / 3600)} 小时 ${Math.floor(runtime.uptime_seconds % 3600 / 60)} 分钟` : "未知";
    const interval = Number.isFinite(runtime.scheduler_interval_seconds) ? `每 ${runtime.scheduler_interval_seconds} 秒` : "未知";
    for (const [label, value] of [["服务版本", runtime.version || "未知"], ["持续运行", uptime], ["通知调度周期", interval], ["通知运行状态", runtime.notification_error || "正常"]]) { const card = node("article", "runtime-card"); card.append(node("span", "", label), node("strong", "", value)); $("runtime-grid").append(card); }
    $("version").textContent = runtime.version ? `· ${runtime.version}` : "";
    try { $("today").textContent = new Intl.DateTimeFormat("zh-CN", { dateStyle: "full", timeZone: settings.timezone || "Asia/Shanghai" }).format(new Date()); } catch { $("today").textContent = new Date().toLocaleDateString("zh-CN"); }
    renderTasks(); renderPlatforms(); renderEvents();
  }
  async function refreshState(showSuccess = false) {
    if (loading) return;
    loading = true;
    try {
      // Authenticate before parallel data reads so an expired session has one clear outcome.
      await api("/api/session");
      const [tasks, events, accounts, settings, bark, runtime] = await Promise.all([api("/api/tasks"), api("/api/events"), api("/api/accounts"), api("/api/settings"), api("/api/bark"), api("/api/status")]);
      state = { tasks: (Array.isArray(tasks) ? tasks : []).map((task) => ({ ...task, due_at: task.end_at, last_seen_at: task.last_synced_at })), events: Array.isArray(events) ? events : [], platforms: (Array.isArray(accounts) ? accounts : []).map((account) => ({ ...account, id: account.platform, login_status: account.login_state, sync_status: account.sync_state, last_success: account.last_success_at, failures: account.consecutive_failures, message: account.last_error || account.message })), settings, bark, runtime, demo: Boolean(runtime.demo) };
      authenticated = true;
      $("boot").hidden = true; $("login-screen").hidden = true; $("app").hidden = false;
      $("connection-error").hidden = true;
      $("service-label").textContent = "管理服务已连接"; $("service-dot").classList.remove("offline");
      render();
      if (showSuccess) notify("状态已刷新。");
    } catch (error) {
      $("boot").hidden = true;
      if (!authenticated) { showLogin(); if (!String(error.message).includes("401") && !/unauthorized|未登录|认证|登录/i.test(error.message)) $("login-error").textContent = `连接服务失败：${error.message}`; }
      else { $("connection-error").textContent = `状态刷新失败：${error.message}。当前显示上次成功读取的数据。`; $("connection-error").hidden = false; $("service-label").textContent = "连接暂时中断"; $("service-dot").classList.add("offline"); }
    } finally { loading = false; }
  }
  function parseMinutes(id, label) {
    const raw = $(id).value.trim();
    if (!raw) return [];
    const parts = raw.split(/[,，\s]+/).filter(Boolean);
    if (parts.some((value) => !/^\d+$/.test(value) || !Number.isSafeInteger(Number(value)) || Number(value) < 1 || Number(value) > 525600)) throw new Error(`${label}：请输入 1 至 525600 的整数分钟，以逗号分隔。`);
    return [...new Set(parts.map(Number))].sort((a, b) => b - a);
  }

  document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => changeView(button.dataset.view)));
  window.addEventListener("hashchange", () => changeView(location.hash.slice(1), false));
  document.querySelectorAll("[data-status]").forEach((button) => button.addEventListener("click", () => { statusFilter = button.dataset.status; updateFilterTabs(); renderTasks(); }));
  $("task-search").addEventListener("input", renderTasks);
  $("platform-filter").addEventListener("change", renderTasks);
  $("settings-form").addEventListener("input", () => { settingsDirty = true; });
  $("bark-form").addEventListener("input", () => { barkDirty = true; });
  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); const button = event.submitter || $("login-form").querySelector("button"); button.disabled = true; $("login-error").textContent = "";
    try { await api("/api/login", "POST", { password: $("login-password").value }); $("login-password").value = ""; await refreshState(); }
    catch (error) { $("login-error").textContent = error.message; }
    finally { button.disabled = false; }
  });
  $("logout").addEventListener("click", (event) => busy(event.currentTarget, async () => { await api("/api/logout", "POST"); state = null; settingsDirty = false; barkDirty = false; $("bark-key").value = ""; showLogin(); $("login-password").focus(); }));
  $("refresh").addEventListener("click", (event) => busy(event.currentTarget, () => refreshState(true)));
  $("sync").addEventListener("click", (event) => busy(event.currentTarget, async () => { const result = await api("/api/sync", "POST"); await refreshState(); notify(result.message || "同步请求已处理，实际采集结果请查看平台状态。"); }));
  $("bark-form").addEventListener("submit", (event) => {
    event.preventDefault(); busy(event.submitter || $("bark-form").querySelector("button"), async () => {
      const server = $("bark-server").value.trim(); const key = $("bark-key").value.trim();
      if (!key && !state.bark?.configured) throw new Error("首次配置需要填写 Bark 设备密钥。");
      if (!key && state.bark?.configured && server !== state.bark.server) throw new Error("修改 Bark 服务地址时，请重新填写设备密钥。");
      await api("/api/bark", "PUT", { server, key }); barkDirty = false; $("bark-key").value = ""; await refreshState(); notify("Bark 配置已保存。");
    });
  });
  $("bark-test").addEventListener("click", (event) => busy(event.currentTarget, async () => { const result = await api("/api/bark/test", "POST"); notify(result.message || "Bark 服务已接受测试通知，请检查 iPhone。"); }));
  $("settings-defaults").addEventListener("click", () => { populateSettings(defaults); settingsDirty = true; notify("已填入默认值，点击保存提醒规则后生效。"); });
  $("settings-form").addEventListener("submit", (event) => {
    event.preventDefault(); busy(event.submitter || $("settings-form").querySelector("button"), async () => {
      const settings = { deadline_minutes: parseMinutes("deadline-minutes", "截止提醒"), exam_start_minutes: parseMinutes("exam-minutes", "考试开始"), video_minutes: parseMinutes("video-minutes", "视频截止"), quiet_enabled: $("quiet-enabled").checked, quiet_start: $("quiet-start").value, quiet_end: $("quiet-end").value, timezone: $("timezone").value.trim() };
      try { new Intl.DateTimeFormat("zh-CN", { timeZone: settings.timezone }); } catch { throw new Error("请填写有效的 IANA 时区，例如 Asia/Shanghai。"); }
      if (settings.quiet_enabled && settings.quiet_start === settings.quiet_end) throw new Error("静默开始和结束时间应不同；全天接收提醒可关闭静默时段。");
      await api("/api/settings", "PUT", settings); settingsDirty = false; await refreshState(); notify("提醒规则已保存。");
    });
  });
  document.addEventListener("visibilitychange", () => { if (!document.hidden && authenticated) refreshState(); });
  setInterval(() => { if (authenticated && !document.hidden) refreshState(); }, 30000);
  changeView(location.hash.slice(1), false);
  refreshState();
})();
