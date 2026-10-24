"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const defaults = { deadline_minutes: [1440, 180, 30], exam_start_minutes: [1440, 60], video_minutes: [4320, 1440, 180], quiet_enabled: true, quiet_start: "23:00", quiet_end: "08:00", timezone: "Asia/Shanghai", in_class_enabled: true, in_class_poll_seconds: 20 };
  const viewNames = { tasks: "任务总览", accounts: "平台账号", settings: "通知设置", activity: "运行记录" };
  const kindNames = { homework: "作业", exam: "考试", chapter: "章节测试", video: "视频学习" };
  const taskStatus = { pending: ["待完成", "warning"], completed: ["已完成", "success"], unknown: ["状态未知", "neutral"] };
  const eventNames = { new_task: "新任务", new: "新任务", deadline: "截止提醒", exam_start: "考试开始", video_deadline: "视频截止", time_changed: "时间变更", changed: "时间变更", login_expired: "登录过期", sync_failure: "同步故障", sync_failed: "同步故障", platform_failure: "平台故障", overdue: "截止提醒", in_class: "课上答题" };
  const eventStatuses = { pending: ["等待发送", "neutral"], deferred: ["静默延后", "neutral"], sent: ["已发送", "success"], failed: ["发送失败", "danger"], retry: ["等待重试", "warning"], skipped: ["已跳过", "neutral"], cancelled: ["已取消", "neutral"], canceled: ["已取消", "neutral"], expired: ["已失效", "neutral"] };
  const loginStatuses = { demo: ["演示模式", "neutral"], unbound: ["未绑定", "neutral"], not_bound: ["未绑定", "neutral"], pending: ["等待验证", "warning"], waiting: ["等待验证", "warning"], logged_in: ["已登录", "success"], authenticated: ["已登录", "success"], active: ["已登录", "success"], expired: ["已过期", "danger"], failed: ["验证失败", "danger"], unsupported: ["待接入", "neutral"], unimplemented: ["待接入", "neutral"], not_configured: ["未配置", "neutral"] };
  const syncStatuses = { demo: ["示例数据", "neutral"], idle: ["等待同步", "neutral"], pending: ["等待同步", "neutral"], success: ["同步成功", "success"], ok: ["同步成功", "success"], syncing: ["同步中", "warning"], failed: ["同步失败", "danger"], error: ["同步失败", "danger"], blocked: ["等待登录", "warning"], unavailable: ["待接入", "neutral"], unsupported: ["待接入", "neutral"], unimplemented: ["待接入", "neutral"], not_configured: ["未配置", "neutral"] };
  eventNames.fault = "平台故障";
  eventStatuses.sending = ["正在发送", "warning"];
  syncStatuses.revalidating = ["重新验证中", "warning"];
  let state = null;
  let currentView = "tasks";
  let statusFilter = "all";
  let taskPage = 1;
  let toastTimer;
  let authenticated = false;
  let authGeneration = 0;
  let refreshJob = null;
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
  function dateParts(value) {
    const date = timestamp(value);
    if (!date) return { date: "—", time: "未知" };
    const zone = state?.settings?.timezone || defaults.timezone;
    try {
      const day = new Intl.DateTimeFormat("zh-CN", { timeZone: zone, month: "2-digit", day: "2-digit" }).format(date);
      const clock = new Intl.DateTimeFormat("zh-CN", { timeZone: zone, hour: "2-digit", minute: "2-digit", hour12: false }).format(date);
      return { date: day.replace(/[/-]/g, "."), time: clock };
    } catch { return { date: "—", time: "未知" }; }
  }
  function dayKey(date) {
    const zone = state?.settings?.timezone || defaults.timezone;
    try { return new Intl.DateTimeFormat("en-CA", { timeZone: zone, year: "numeric", month: "2-digit", day: "2-digit" }).format(date); }
    catch { return new Intl.DateTimeFormat("en-CA", { year: "numeric", month: "2-digit", day: "2-digit" }).format(date); }
  }
  function relativeLabel(date, verb = "截止") {
    const diff = date.getTime() - Date.now();
    if (diff <= 0) return `已过${verb}`;
    const minutes = Math.ceil(diff / 60000);
    if (minutes < 60) return `${minutes} 分钟后${verb}`;
    if (minutes < 2880) return `${Math.ceil(minutes / 60)} 小时后${verb}`;
    return `${Math.ceil(minutes / 1440)} 天后${verb}`;
  }
  function agendaGroup(task) {
    if (task.status === "completed") return "已完成";
    const due = timestamp(task.due_at);
    if (!due) return "截止未知";
    const now = Date.now();
    if (due.getTime() <= now) return "已过截止";
    const key = dayKey(due);
    if (key === dayKey(new Date(now))) return "今天";
    if (key === dayKey(new Date(now + 86400000))) return "明天";
    if (due.getTime() - now <= 7 * 86400000) return "未来七天";
    return "更远";
  }
  // Exams matter at their start, everything else at its deadline.
  function nextMoment(task) {
    const now = Date.now();
    const start = task.kind === "exam" ? timestamp(task.start_at) : null;
    const due = timestamp(task.due_at);
    const moments = [];
    if (start && start.getTime() > now) moments.push({ at: start, verb: "开始", label: "考试开始" });
    if (due && due.getTime() > now) moments.push({ at: due, verb: "截止", label: task.kind === "video" ? "视频截止" : "截止" });
    return moments.sort((a, b) => a.at - b.at)[0] || null;
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
    authGeneration++;
    authenticated = false;
    state = null;
    settingsDirty = false;
    barkDirty = false;
    $("bark-key").value = "";
    clearActiveLogin();
    $("boot").hidden = true;
    $("app").hidden = true;
    $("login-screen").hidden = false;
  }
  async function api(path, method = "GET", body) {
    const generation = authGeneration;
    const stale = () => Object.assign(new Error("会话已切换。"), { name: "StaleSessionError" });
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 20000);
    try {
      const response = await fetch(path, { method, credentials: "same-origin", cache: "no-store", headers: { ...(method === "GET" ? {} : { "X-DueRadar": "1" }), ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, body: body === undefined ? undefined : JSON.stringify(body), signal: controller.signal });
      const contentType = response.headers.get("content-type") || "";
      const result = contentType.includes("application/json") ? await response.json() : { message: (await response.text()).slice(0, 300) };
      if (generation !== authGeneration) throw stale();
      if (!response.ok) {
        if (response.status === 401 && path !== "/api/login") showLogin();
        throw new Error(result.error || result.message || `请求失败（${response.status}）`);
      }
      return result;
    } catch (error) {
      if (generation !== authGeneration) throw stale();
      if (error.name === "AbortError") throw new Error("请求超时，请稍后重试。");
      throw error;
    } finally { clearTimeout(timeout); }
  }
  async function busy(button, action) {
    button.dataset.busy = "true";
    button.disabled = true;
    try { await action(); } catch (error) { if (error.name !== "StaleSessionError") notify(error.message || "操作失败，请稍后重试。", true); }
    finally { delete button.dataset.busy; button.disabled = false; }
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
    wrap.append(icon, node("h3", "", title));
    if (message) wrap.append(node("p", "", message));
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
    const label = minutes < 60 ? `${minutes} 分钟后截止` : minutes < 2880 ? `${Math.ceil(minutes / 60)} 小时后截止` : `${Math.ceil(minutes / 1440)} 天后截止`;
    return { main: label, sub: formatDate(task.due_at, true), urgent: diff <= 86400000 };
  }
  function updateFilterCount() {
    const count = [
      $("task-search").value.trim() !== "",
      $("platform-filter").value && $("platform-filter").value !== "all",
      $("kind-filter").value && $("kind-filter").value !== "all",
      $("deadline-filter").value && $("deadline-filter").value !== "all",
      $("scope-filter").value && $("scope-filter").value !== "current"
    ].filter(Boolean).length;
    $("filter-count").textContent = count;
    $("filter-count").hidden = count === 0;
  }
  function renderTasks() {
    const tasks = state.tasks || [];
    const query = $("task-search").value.trim().toLocaleLowerCase();
    const platform = $("platform-filter").value;
    const scope = $("scope-filter").value;
    updateFilterCount();
    const filtered = DueRadarTasks.select(tasks, { status: statusFilter, platform, query, kind: $("kind-filter").value, timing: $("deadline-filter").value, scope, platformLabel: platformName });
    const page = DueRadarTasks.paginate(filtered, taskPage);
    taskPage = page.page;
    $("task-pagination").hidden = filtered.length === 0;
    $("task-page-label").textContent = `显示 ${page.start}–${page.end} 项，共 ${page.total} 项 · 第 ${page.page}/${page.pages} 页`;
    $("task-prev").disabled = page.page <= 1;
    $("task-next").disabled = page.page >= page.pages;
    $("task-count").textContent = filtered.length;
    $("task-list").replaceChildren();
    if (!filtered.length) {
      const zone = state.settings?.timezone || defaults.timezone;
      const scoped = DueRadarTasks.scopeTasks(tasks, { scope, timezone: zone });
      const hiddenHistory = DueRadarTasks.scopeTasks(tasks, { scope: "all", timezone: zone }).length > DueRadarTasks.scopeTasks(tasks, { scope: "current", timezone: zone }).length;
      if (scope === "current" && hiddenHistory) {
        $("task-list").append(emptyState("本学期暂无任务", "", { label: "查看历史学期", run: () => { $("scope-filter").value = "all"; taskPage = 1; render(); } }));
      } else if (scoped.length) {
        $("task-list").append(emptyState("没有符合筛选条件的任务", "", { label: "清除筛选", run: () => { statusFilter = "all"; taskPage = 1; $("task-search").value = ""; for (const id of ["platform-filter", "kind-filter", "deadline-filter"]) $(id).value = "all"; $("scope-filter").value = "current"; render(); } }));
      } else {
        $("task-list").append(emptyState("还没有学习任务", "同步成功后显示作业、考试和视频。", { label: "查看平台接入状态 →", run: () => changeView("accounts") }));
      }
      return;
    }
    const fragment = document.createDocumentFragment();
    const order = ["今天", "明天", "未来七天", "更远", "截止未知", "已过截止", "已完成"];
    const groups = new Map(order.map((label) => [label, []]));
    for (const task of page.items) groups.get(agendaGroup(task)).push(task);
    for (const label of order) {
      const items = groups.get(label);
      if (!items.length) continue;
      const group = node("section", "agenda-group");
      group.setAttribute("aria-label", label);
      const head = node("div", "agenda-head");
      head.append(node("h3", "agenda-head__label", label), node("span", "agenda-head__rule"), node("span", "agenda-head__count", items.length));
      group.append(head);
      for (const task of items) {
        const kind = Object.hasOwn(kindNames, task.kind) ? task.kind : "homework";
        const due = timestamp(task.due_at);
        const now = Date.now();
        const isCompleted = task.status === "completed";
        const isOverdue = !isCompleted && Boolean(due) && due.getTime() <= now;
        const isUrgent = task.status === "pending" && Boolean(due) && due.getTime() > now && due.getTime() - now <= 86400000;
        const card = node("article", `task-item${isUrgent ? " is-urgent" : ""}${isOverdue ? " is-overdue" : ""}${isCompleted ? " is-completed" : ""}`);
        const parts = dateParts(task.due_at);
        const when = node("div", "task-when");
        when.append(node("span", "task-when__date", parts.date), node("span", "task-when__time", parts.time));
        const main = node("div", "task-main");
        const titleLine = node("div", "task-title-line");
        titleLine.append(node("h3", "task-title", task.title || "未命名任务"), badge(...cleanStatus(taskStatus, task.status)));
        const meta = node("div", "task-meta");
        [platformName(task.platform), task.course || "课程未知", kindNames[task.kind] || "其他任务"].forEach((value, index) => { if (index) meta.append(node("span", "separator", "/")); meta.append(node("span", "", value)); });
        main.append(titleLine, meta);
        if (task.kind === "exam") main.append(node("p", "task-start", `开始：${formatDate(task.start_at, true)}`));
        if (task.kind === "video") {
          if (typeof task.progress === "number" && Number.isFinite(task.progress)) {
            const progress = Math.max(0, Math.min(100, task.progress));
            const track = node("progress", "progress-track"); track.max = 100; track.value = progress; track.setAttribute("aria-label", "视频学习进度");
            main.append(track, node("p", "progress-label", `已学习 ${Math.round(progress)}%`));
          } else main.append(node("p", "progress-label", "学习进度未知"));
        }
        const deadline = deadlineLabel(task);
        const foot = node("div", "task-foot");
        foot.append(node("span", `deadline-main${deadline.urgent && !isCompleted ? " urgent" : ""}`, isCompleted ? "平台已记录完成" : deadline.main));
        const source = DueRadarTasks.safeSource(task.source_url);
        if (source) { const link = node("a", "task-source", "打开课程平台 ↗"); link.href = source; link.target = "_blank"; link.rel = "noopener noreferrer"; foot.append(link); }
        main.append(foot);
        main.title = `最近观测：${formatDate(task.last_seen_at)}`;
        const mark = node("span", `task-kind ${kind}`, { homework: "作", exam: "考", chapter: "章", video: "视" }[kind]);
        mark.setAttribute("aria-hidden", "true");
        card.append(when, main, mark);
        group.append(card);
      }
      fragment.append(group);
    }
    $("task-list").append(fragment);
  }
  function updateFilterTabs() { const scoped = DueRadarTasks.scopeTasks(state?.tasks || [], { scope: $("scope-filter").value, timezone: state?.settings?.timezone || defaults.timezone }); document.querySelectorAll("[data-status]").forEach((button) => { const active = button.dataset.status === statusFilter; button.classList.toggle("active", active); button.setAttribute("aria-pressed", String(active)); const status = button.dataset.status; const count = scoped.filter(task => status === "all" || task.status === status).length; button.textContent = `${status === "all" ? "全部" : taskStatus[status][0]} ${count}`; }); }
  function renderPlatforms() {
    $("platform-list").replaceChildren();
    if (!state.platforms.length) { $("platform-list").append(emptyState("暂无平台信息", "")); return; }
    for (const platform of state.platforms) {
      const card = node("article", "platform-card");
      const head = node("div", "platform-card__head");
      const title = node("div", "platform-card__title");
      const subtitle = platform.id === "rain" ? "微信扫码 · 可选用电脑辅助" : platform.id === "zhidao_campus" ? "知到 APP 扫码" : platform.id === "zhidao_shared" ? "共享课 · 待接入验证" : "平台接入";
      title.append(node("h2", "", platform.name || platform.id), node("p", "platform-subtitle", subtitle));
      head.append(node("div", "platform-avatar", (platform.name || "课").slice(0, 1)), title);
      card.append(head);
      for (const [label, mapping, value] of [["账号状态", loginStatuses, platform.login_status], ["同步状态", syncStatuses, platform.sync_status]]) { const row = node("div", "platform-status"); row.append(node("span", "", label), badge(...cleanStatus(mapping, value))); card.append(row); }
      card.append(node("p", "platform-message", platform.message || "待验证"));
      card.append(node("p", "platform-date", `最近成功同步：${timestamp(platform.last_success) ? formatDate(platform.last_success) : "尚无成功记录"}`));
      if (Number(platform.failures) > 0) card.append(node("p", "platform-date", `连续失败：${Number(platform.failures)} 轮`));
      if (["rain", "zhidao_campus"].includes(platform.id) && platform.ready && state.runtime && !state.demo) {
        const bind = node("button", "button primary", platform.login_status === "logged_in" ? "重新扫码绑定" : (platform.id === "rain" ? "微信扫码绑定" : "知到 APP 扫码绑定"));
        bind.type = "button"; bind.addEventListener("click", () => busy(bind, () => openRainLogin(platform.id))); card.append(bind);
        if (platform.id === "rain") {
          const assist = node("button", "button secondary assist-button", "账号密码登录（电脑辅助）");
          assist.type = "button"; assist.addEventListener("click", () => busy(assist, () => openRainLogin("rain", "assist"))); card.append(assist);
        }
      }
      $("platform-list").append(card);
    }
  }
  let activeLogin;
  function clearLoginSecrets() {
    $("rain-qr").removeAttribute("src");
    for (const id of ["assist-instance", "assist-flow", "assist-code"]) $(id).value = "";
    $("assist-expires").textContent = "";
  }
  function clearActiveLogin() {
    const login = activeLogin;
    activeLogin = null;
    if (!login) return;
    clearTimeout(login.timer);
    clearLoginSecrets();
    if ($("rain-dialog").open) $("rain-dialog").close();
  }
  async function openRainLogin(id, mode = "qr") {
    if (activeLogin) { notify("请先完成或关闭当前账号绑定。"); return; }
    const assist = id === "rain" && mode === "assist";
    const login = { path: assist ? "/api/accounts/rain/assist" : id === "rain" ? "/api/accounts/rain/login" : "/api/accounts/zhidao/login", timer: null, completed: false, assist };
    activeLogin = login;
    clearLoginSecrets();
    const label = id === "rain" ? "普通雨课堂" : "知到";
    const scanner = id === "rain" ? "微信" : "知到 APP";
    let result;
    try { result = await api(login.path, "POST"); }
    catch (error) {
      if (!assist && error.name !== "StaleSessionError" && activeLogin === login) {
        try { await api(`${login.path}/cancel`, "POST"); } catch (_) { /* Keep the original creation error. */ }
      }
      if (activeLogin === login) activeLogin = null;
      throw error;
    }
    if (activeLogin !== login) return;
    login.deadline = assist ? Date.parse(result.expires_at) : Date.now() + (result.expires_in || 180) * 1000;
    if (assist) login.path += `/${encodeURIComponent(result.flow_id)}`;
    $("rain-title").textContent = assist ? "电脑辅助登录普通雨课堂" : `绑定${label}`;
    $("rain-description").textContent = assist ? "在电脑上打开官方登录页，输入账号密码并完成平台验证。" : "通过平台官方二维码确认登录";
    $("rain-qr").hidden = assist;
    $("rain-qr-frame").hidden = assist;
    $("rain-assist").hidden = !assist;
    if (assist) {
      $("assist-instance").value = location.origin;
      $("assist-flow").value = result.flow_id;
      $("assist-code").value = result.pairing_code;
      $("assist-expires").textContent = `配对有效至 ${new Date(result.expires_at).toLocaleTimeString("zh-CN")}，关闭弹窗将取消本次绑定。`;
      $("rain-login-status").textContent = "等待电脑辅助程序连接。";
    } else {
      $("rain-qr").alt = `${label}官方登录二维码`;
      $("rain-qr").src = result.image;
      $("rain-login-status").textContent = `请使用${scanner}扫码，并在手机上确认登录。二维码约 3 分钟有效。`;
    }
    login.opened = true;
    $("rain-dialog").showModal();
    async function poll() {
      if (activeLogin !== login || !$("rain-dialog").open) return;
      if (Date.now() >= login.deadline) {
        clearLoginSecrets();
        $("rain-login-status").textContent = "绑定已过期，请关闭后重新开始。";
        return;
      }
      try {
        const result = await api(login.path);
        if (activeLogin !== login || !$("rain-dialog").open) return;
        if (result.state === "logged_in") { login.completed = true; $("rain-dialog").close(); notify(`${label}登录已确认，正在读取任务。`); await refreshState(); return; }
        if (["failed", "expired", "canceled"].includes(result.state)) {
          clearLoginSecrets();
          $("rain-login-status").textContent = result.error || (result.state === "expired" ? "绑定已过期，请关闭后重新开始。" : result.state === "canceled" ? "本次绑定已取消。" : "登录未完成，请关闭后重新绑定。");
          return;
        }
        if (assist) {
          const messages = { awaiting_helper: "等待电脑辅助程序连接。", awaiting_user: "辅助程序已连接，请在官方页面完成账号密码和验证码登录。", verifying: "设备正在独立验证登录会话，请稍候。" };
          $("rain-login-status").textContent = messages[result.state] || "等待登录结果。";
          if (result.state !== "awaiting_helper") $("assist-code").value = "";
        }
        login.timer = setTimeout(poll, 2000);
      } catch (error) {
        if (activeLogin === login && $("rain-dialog").open) {
          $("rain-login-status").textContent = `暂未取得登录状态，正在重试：${error.message}`;
          login.timer = setTimeout(poll, 2000);
        }
      }
    }
    login.timer = setTimeout(poll, 1500);
  }
  $("rain-close").addEventListener("click", () => $("rain-dialog").close());
  $("rain-dialog").addEventListener("close", async () => {
    const login = activeLogin;
    // Native close events are queued. An old dialog's event must not cancel a
    // replacement flow that is creating its QR or has already reopened.
    if (!login || !login.opened || $("rain-dialog").open) return;
    clearTimeout(login.timer);
    clearLoginSecrets();
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
    for (const [index, event] of upcoming.entries()) {
      const item = node("div", "upcoming-item");
      item.append(node("span", "upcoming-index", String(index + 1).padStart(2, "0")));
      const content = node("div", "upcoming-body");
      content.append(node("p", "upcoming-title", eventTitle(event)), node("p", "upcoming-meta", eventNames[event.kind] || event.kind || "任务提醒"), node("p", "upcoming-time", formatDate(event.due_at, true)));
      item.append(content);
      $("upcoming-list").append(item);
    }
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
    $("in-class-enabled").checked = settings.in_class_enabled !== false;
    $("in-class-poll").value = settings.in_class_poll_seconds || defaults.in_class_poll_seconds;
  }
  function render() {
    const tasks = DueRadarTasks.scopeTasks(state.tasks, { scope: $("scope-filter").value, timezone: state.settings?.timezone || defaults.timezone });
    const pending = tasks.filter((task) => task.status === "pending");
    $("stat-pending").textContent = pending.length;
    const overdue = pending.filter(task => DueRadarTasks.timeGroup(task, Date.now()) === "overdue").length;
    const noDue = pending.filter(task => DueRadarTasks.timeGroup(task, Date.now()) === "no_due").length;
    $("pending-detail").textContent = `${overdue} 项已过截止 · ${noDue} 项截止未知`;
    $("nav-count").textContent = pending.length;
    $("stat-soon").textContent = pending.filter((task) => { const due = timestamp(task.due_at)?.getTime(); return due && due > Date.now() && due - Date.now() <= 86400000; }).length;
    $("stat-completed").textContent = tasks.filter((task) => task.status === "completed").length;
    $("stat-platforms").textContent = state.platforms.filter((platform) => ["logged_in", "authenticated", "active"].includes(platform.login_status)).length;
    $("stat-platform-total").textContent = ` / ${state.platforms.length}`;
    const recent = state.platforms.map((platform) => timestamp(platform.last_success)).filter(Boolean).sort((a, b) => b - a)[0];
    $("stat-sync").textContent = recent ? formatDate(recent, true) : "尚无成功记录";
    const next = pending
      .map((task) => ({ task, moment: nextMoment(task) }))
      .filter((entry) => entry.moment)
      .sort((a, b) => a.moment.at - b.moment.at)[0];
    $("next-block").classList.toggle("is-empty", !next);
    $("next-title").textContent = next ? next.task.title : "暂无临近截止";
    $("next-when").textContent = next ? `${next.moment.label} · ${formatDate(next.moment.at, true)}` : "等待平台提供新的任务";
    $("next-countdown").textContent = next ? relativeLabel(next.moment.at, next.moment.verb) : "—";
    const selectedPlatform = $("platform-filter").value;
    $("platform-filter").replaceChildren(new Option("全部平台", "all"));
    const ids = new Set([...state.platforms.map((platform) => platform.id), ...state.tasks.map((task) => task.platform)].filter(Boolean));
    for (const id of ids) $("platform-filter").append(new Option(platformName(id), id));
    if (ids.has(selectedPlatform)) $("platform-filter").value = selectedPlatform;
    $("demo-banner").hidden = !state.demo;
    const settings = state.settings || defaults;
    $("quiet-title").textContent = !state.settings ? "提醒设置尚未加载" : settings.quiet_enabled ? "静默时段已开启" : "静默时段已关闭";
    $("quiet-summary").textContent = !state.settings ? "任务已显示，正在读取提醒设置。" : settings.quiet_enabled ? `${settings.quiet_start}—${settings.quiet_end}，普通提醒延后。时区：${settings.timezone}。` : `所有提醒按配置时间发送。时区：${settings.timezone}。`;
    if (state.settings && !settingsDirty) populateSettings(settings);
    $("settings-form").querySelectorAll("input, select").forEach(input => { input.disabled = !state.settings || !state.runtime; });
    const settingsButton = $("settings-form").querySelector("button[type=submit]");
    settingsButton.disabled = !state.settings || !state.runtime || settingsButton.dataset.busy === "true";
    $("settings-defaults").disabled = !state.settings || !state.runtime;
    const barkConfigured = Boolean(state.bark?.configured);
    $("bark-status").textContent = !state.bark ? "尚未加载" : barkConfigured ? "已配置" : "未配置";
    $("bark-status").className = `badge ${barkConfigured ? "success" : "neutral"}`;
    const barkUnavailable = !state.bark || !state.runtime || state.demo;
    $("bark-test").disabled = !barkConfigured || barkUnavailable;
    $("in-class-test").disabled = !barkConfigured || barkUnavailable;
    const barkButton = $("bark-form").querySelector("button[type=submit]");
    barkButton.disabled = barkUnavailable || barkButton.dataset.busy === "true";
    $("bark-key").disabled = barkUnavailable;
    $("bark-server").disabled = barkUnavailable;
    if (state.bark && !barkDirty) $("bark-server").value = state.bark.server || "https://api.day.app";
    $("bark-key").placeholder = barkConfigured ? "已保存，留空保留当前密钥" : "输入 Bark 设备密钥";
    $("runtime-grid").replaceChildren();
    const runtime = state.runtime || {};
    const uptime = Number.isFinite(runtime.uptime_seconds) ? `${Math.floor(runtime.uptime_seconds / 3600)} 小时 ${Math.floor(runtime.uptime_seconds % 3600 / 60)} 分钟` : "未知";
    const interval = Number.isFinite(runtime.scheduler_interval_seconds) ? `每 ${runtime.scheduler_interval_seconds} 秒` : "未知";
    const inClassOn = runtime.in_class_enabled !== false;
    const inClassLessons = Array.isArray(runtime.in_class_lessons) ? runtime.in_class_lessons : [];
    const inClassState = !inClassOn ? "已关闭" : runtime.in_class_error ? runtime.in_class_error : inClassLessons.length ? `正在上课 · ${inClassLessons.join("、")}` : runtime.in_class_last_check_at ? `守候中 · ${formatDate(runtime.in_class_last_check_at, true)}` : "等待首次检查";
    const inClassRecent = runtime.in_class_last_quiz_at ? `${runtime.in_class_last_quiz || "课上答题"} · ${formatDate(runtime.in_class_last_quiz_at, true)}` : "暂无检测记录";
    const readouts = [["服务版本", runtime.version || "未知"], ["持续运行", uptime], ["通知调度周期", interval], ["课上答题监测", state.runtime ? inClassState : "尚未加载"], ["最近课上答题", state.runtime ? inClassRecent : "尚未加载"], ["通知运行状态", state.runtime ? runtime.notification_error || "正常" : "尚未加载"], ["最近通知成功", runtime.notification_last_success_at ? formatDate(runtime.notification_last_success_at) : "暂无成功记录"]];
    readouts.forEach(([label, value], index) => {
      const card = node("article", "runtime-card");
      if (index === readouts.length - 1) card.classList.add("runtime-card--wide");
      card.append(node("span", "", label), node("strong", "", value));
      $("runtime-grid").append(card);
    });
    $("version").textContent = runtime.version ? `· ${runtime.version}` : "";
    try {
      const zone = settings.timezone || "Asia/Shanghai";
      const day = new Intl.DateTimeFormat("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit", timeZone: zone }).format(new Date()).replace(/[/-]/g, ".");
      const week = new Intl.DateTimeFormat("zh-CN", { weekday: "long", timeZone: zone }).format(new Date());
      $("today").textContent = `${day} · ${week}`;
    } catch { $("today").textContent = new Date().toLocaleDateString("zh-CN"); }
    updateFilterTabs(); renderTasks(); renderPlatforms(); renderEvents();
  }
  function refreshState(showSuccess = false) {
    const generation = authGeneration;
    if (refreshJob?.generation === generation) {
      refreshJob.queued = true;
      refreshJob.showSuccess ||= showSuccess;
      return refreshJob.promise;
    }
    const job = { generation, queued: false, showSuccess, promise: null };
    refreshJob = job;
    const showError = (error) => {
      if (generation !== authGeneration || error.name === "StaleSessionError" || job.queued) return;
      $("boot").hidden = true;
      if (!authenticated) { showLogin(); if (!String(error.message).includes("401") && !/unauthorized|未登录|认证|登录/i.test(error.message)) $("login-error").textContent = `连接服务失败：${error.message}`; }
      else { $("connection-error").textContent = `状态刷新失败：${error.message}。当前显示上次成功读取的数据。`; $("connection-error").hidden = false; $("service-label").textContent = "连接暂时中断"; $("service-dot").classList.add("offline"); }
    };
    job.promise = (async () => {
      try {
        do {
          job.queued = false;
          try {
            // Authenticate before parallel reads. A queued refresh starts after
            // this pass, so a save never joins a snapshot taken before its write.
            await api("/api/session");
            if (generation !== authGeneration) return false;
            const snapshot = { tasks: [], events: [], platforms: [], settings: null, bark: null, runtime: null, demo: false, ...state };
            const sections = { tasks: "任务", events: "通知记录", accounts: "平台账号", settings: "提醒设置", bark: "Bark 配置", status: "运行状态" };
            const pending = new Set(Object.keys(sections));
            const errors = new Map();
            let tasksReady = false;
            const publish = () => {
              if (generation !== authGeneration || job.queued || !tasksReady) return;
              state = { ...snapshot };
              authenticated = true;
              $("boot").hidden = true; $("login-screen").hidden = true; $("app").hidden = false;
              const messages = [...errors].map(([key, error]) => `${sections[key]}加载失败：${error.message}。该部分保留上次数据（如有）。`);
              if (pending.size) messages.push(`正在加载：${[...pending].map(key => sections[key]).join("、")}。`);
              $("connection-error").textContent = messages.join(" ");
              $("connection-error").hidden = messages.length === 0;
              $("service-label").textContent = errors.size ? "部分信息加载失败" : "管理服务已连接";
              $("service-dot").classList.toggle("offline", errors.size > 0);
              render();
            };
            // Publish tasks as soon as authenticated reads succeed. Handle each
            // rejection locally so the refresh gate survives partial failures
            // until every request settles, including queued mutation refreshes.
            await Promise.all(Object.keys(sections).map(async (key) => {
              try {
                const value = await api(`/api/${key}`);
                if (generation !== authGeneration) return;
                if (key === "tasks") {
                  snapshot.tasks = (Array.isArray(value) ? value : []).map(task => ({ ...task, due_at: task.end_at, last_seen_at: task.last_synced_at }));
                  tasksReady = true;
                } else if (key === "accounts") snapshot.platforms = (Array.isArray(value) ? value : []).map(account => ({ ...account, id: account.platform, login_status: account.login_state, sync_status: account.sync_state, last_success: account.last_success_at, failures: account.consecutive_failures, message: account.last_error || account.message }));
                else if (key === "events") snapshot.events = Array.isArray(value) ? value : [];
                else if (key === "status") { snapshot.runtime = value; snapshot.demo = Boolean(value.demo); }
                else snapshot[key] = value;
              } catch (error) {
                if (generation !== authGeneration || error.name === "StaleSessionError") return;
                errors.set(key, error);
                if (key === "tasks") showError(error);
              } finally { pending.delete(key); }
              publish();
            }));
            if (generation !== authGeneration) return false;
            if (job.queued) continue;
            if (errors.size) return false;
          } catch (error) {
            if (generation !== authGeneration || error.name === "StaleSessionError") return false;
            if (job.queued) continue;
            showError(error);
            return false;
          }
        } while (job.queued);
        if (job.showSuccess) notify("状态已刷新。");
        return true;
      } finally { if (refreshJob === job) refreshJob = null; }
    })();
    return job.promise;
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
  document.querySelectorAll("[data-status]").forEach((button) => button.addEventListener("click", () => { statusFilter = button.dataset.status; taskPage = 1; updateFilterTabs(); renderTasks(); }));
  $("task-search").addEventListener("input", () => { taskPage = 1; renderTasks(); });
  for (const id of ["platform-filter", "kind-filter", "deadline-filter", "scope-filter"]) $(id).addEventListener("change", () => { taskPage = 1; render(); });
  $("task-prev").addEventListener("click", () => { taskPage--; renderTasks(); $("task-heading").scrollIntoView({ block: "start" }); });
  $("task-next").addEventListener("click", () => { taskPage++; renderTasks(); $("task-heading").scrollIntoView({ block: "start" }); });
  $("settings-form").addEventListener("input", () => { settingsDirty = true; });
  $("bark-form").addEventListener("input", () => { barkDirty = true; });
  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); const button = event.submitter || $("login-form").querySelector("button"); button.disabled = true; $("login-error").textContent = "";
    try { await api("/api/login", "POST", { password: $("login-password").value }); authGeneration++; $("login-password").value = ""; await refreshState(); }
    catch (error) { if (error.name !== "StaleSessionError") $("login-error").textContent = error.message; }
    finally { button.disabled = false; }
  });
  $("logout").addEventListener("click", (event) => busy(event.currentTarget, async () => { authGeneration++; clearActiveLogin(); await api("/api/logout", "POST"); showLogin(); $("login-password").focus(); }));
  $("refresh").addEventListener("click", (event) => busy(event.currentTarget, () => refreshState(true)));
  $("sync").addEventListener("click", (event) => busy(event.currentTarget, async () => { const result = await api("/api/sync", "POST"); if (await refreshState()) notify(result.message || "已开始同步。"); }));
  $("bark-form").addEventListener("submit", (event) => {
    event.preventDefault(); busy(event.submitter || $("bark-form").querySelector("button"), async () => {
      if (!state?.bark || !state?.runtime || state.demo) throw new Error("请等待 Bark 配置及运行状态加载完成后再保存。");
      const server = $("bark-server").value.trim(); const key = $("bark-key").value.trim();
      if (!key && !state.bark?.configured) throw new Error("首次配置需要填写 Bark 设备密钥。");
      if (!key && state.bark?.configured && server !== state.bark.server) throw new Error("修改 Bark 服务地址时，请重新填写设备密钥。");
      await api("/api/bark", "PUT", { server, key }); barkDirty = false; $("bark-key").value = ""; if (await refreshState()) notify("Bark 配置已保存。");
    });
  });
  $("bark-test").addEventListener("click", (event) => busy(event.currentTarget, async () => { const result = await api("/api/bark/test", "POST"); notify(result.message || "测试通知已发送。"); }));
  $("in-class-test").addEventListener("click", (event) => busy(event.currentTarget, async () => { const result = await api("/api/in-class/test", "POST"); notify(result.message || "测试通知已发送。"); }));
  $("settings-defaults").addEventListener("click", () => { populateSettings(defaults); settingsDirty = true; notify("已填入默认值，保存后生效。"); });
  $("settings-form").addEventListener("submit", (event) => {
    event.preventDefault(); busy(event.submitter || $("settings-form").querySelector("button"), async () => {
      if (!state?.settings || !state?.runtime) throw new Error("请等待提醒设置及运行状态加载完成后再保存。");
      const pollSeconds = Number.parseInt($("in-class-poll").value, 10);
      if (!Number.isInteger(pollSeconds) || pollSeconds < 10 || pollSeconds > 300) throw new Error("课上答题检查间隔请填写 10—300 秒。");
      const settings = { deadline_minutes: parseMinutes("deadline-minutes", "截止提醒"), exam_start_minutes: parseMinutes("exam-minutes", "考试开始"), video_minutes: parseMinutes("video-minutes", "视频截止"), quiet_enabled: $("quiet-enabled").checked, quiet_start: $("quiet-start").value, quiet_end: $("quiet-end").value, timezone: $("timezone").value.trim(), in_class_enabled: $("in-class-enabled").checked, in_class_poll_seconds: pollSeconds };
      try { new Intl.DateTimeFormat("zh-CN", { timeZone: settings.timezone }); } catch { throw new Error("请填写有效的 IANA 时区，例如 Asia/Shanghai。"); }
      if (settings.quiet_enabled && settings.quiet_start === settings.quiet_end) throw new Error("静默开始和结束时间应不同；全天接收提醒可关闭静默时段。");
      await api("/api/settings", "PUT", settings); settingsDirty = false; if (await refreshState()) notify("提醒规则已保存。");
    });
  });
  function syncThemeControls() {
    const dark = document.documentElement?.dataset.theme === "dark";
    document.querySelectorAll("[data-theme-toggle]").forEach((button) => button.setAttribute("aria-pressed", String(dark)));
  }
  document.querySelectorAll("[data-theme-toggle]").forEach((button) => button.addEventListener("click", () => {
    const root = document.documentElement;
    if (!root) return;
    const next = root.dataset.theme === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem("dueradar-theme", next); } catch { /* Storage may be blocked. */ }
    syncThemeControls();
  }));
  syncThemeControls();
  $("filter-toggle").addEventListener("click", () => {
    const toolbar = $("task-toolbar");
    const open = !toolbar.classList.contains("is-open");
    toolbar.classList.toggle("is-open", open);
    $("filter-toggle").setAttribute("aria-expanded", String(open));
    $("filter-toggle").setAttribute("aria-label", open ? "收起筛选" : "展开筛选");
  });
  document.addEventListener("visibilitychange", () => { if (!document.hidden && authenticated) refreshState(); });
  setInterval(() => { if (authenticated && !document.hidden) refreshState(); }, 30000);
  changeView(location.hash.slice(1), false);
  refreshState();
})();
