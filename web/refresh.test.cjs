const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const DueRadarTasks = require("./tasks.js");

function deferred() {
  let resolve, reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
function response(value, status = 200) {
  return { ok: status < 400, status, headers: { get: () => "application/json" }, json: async () => value };
}
async function settle() { for (let i = 0; i < 10; i++) await new Promise(setImmediate); }

// Run the complete application with real event handlers and controlled HTTP
// completion order. No implementation functions are exported or replaced.
function browser(intercept = null) {
  const elements = new Map();
  function element(tag = "div") {
    let text = "";
    return {
      tag, value: "", hidden: true, checked: false, disabled: false,
      get textContent() { return text; },
      set textContent(value) { this.textWrites = (this.textWrites || 0) + 1; text = String(value); },
      children: [], listeners: {}, dataset: {}, open: false,
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener(name, fn) { this.listeners[name] = fn; },
      emit(name, extra = {}) { return this.listeners[name]?.({ currentTarget: this, preventDefault() {}, ...extra }); },
      append(...children) { this.children.push(...children); },
      replaceChildren(...children) { this.children = children; },
      setAttribute(name, value) { this[name] = value; },
      removeAttribute(name) { delete this[name]; },
      querySelector() { return this.button ||= element("button"); },
      querySelectorAll() { return this.inputs || []; },
      showModal() { this.open = true; },
      close() { this.open = false; if (page.deferClose) page.closeEvents.push(() => this.emit("close")); else return this.emit("close"); },
      focus() {}, scrollIntoView() {},
    };
  }
  function $(id) {
    if (!elements.has(id)) elements.set(id, element());
    return elements.get(id);
  }
  for (const id of ["platform-filter", "kind-filter", "deadline-filter"]) $(id).value = "all";
  $("settings-form").inputs = ["deadline-minutes", "exam-minutes", "video-minutes", "quiet-enabled", "quiet-start", "quiet-end", "timezone", "in-class-enabled", "in-class-poll", "zhidao-in-class-enabled", "poll-wake-enabled", "rain-fallback-enabled", "rain-analysis-enabled"].map($);
  $("analysis-form").inputs = ["analysis-key", "analysis-model"].map($);
  let interval;
  let loggedIn = true;
  const calls = [];
  const timers = new Map();
  let nextTimer = 0;
  const document = { hidden: false, getElementById: $, createElement: element, createDocumentFragment: element, querySelectorAll: () => [], listeners: {}, addEventListener(name, fn) { this.listeners[name] = fn; } };
  const location = { hash: "" };
  const window = { listeners: {}, addEventListener(name, fn) { this.listeners[name] = fn; } };
  const page = {
    $, calls, document, location, window, intercept, deferClose: false, closeEvents: [],
    settings: { deadline_minutes: [1440, 180, 30], exam_start_minutes: [1440, 60], video_minutes: [4320, 1440, 180], quiet_enabled: true, quiet_start: "23:00", quiet_end: "08:00", timezone: "Asia/Shanghai", zhidao_in_class_enabled: true },
    click: (id) => $(id).emit("click"),
    submit: (id) => $(id).emit("submit", { submitter: $(id).querySelector("button") }),
    tick: () => interval(),
    takeTimer(delay) { const found = [...timers].find(([, timer]) => timer.delay === delay); assert.ok(found, `timer ${delay} exists`); timers.delete(found[0]); return found[1].fn; },
  };
  const fetch = async (url, options) => {
    const call = { url, method: options.method, headers: options.headers, body: options.body && JSON.parse(options.body) };
    calls.push(call);
    const delayed = page.intercept?.(call);
    if (delayed) return delayed;
    if (url === "/api/logout") { loggedIn = false; return response({}); }
    if (url === "/api/login") { loggedIn = true; return response({}); }
    if (!loggedIn) return response({ error: "unauthorized" }, 401);
    if (url === "/api/session") return response({});
    if (url === "/api/settings") {
      if (call.method === "PUT") page.settings = call.body;
      return response({ ...page.settings });
    }
    if (url === "/api/tasks" || url === "/api/events") return response([]);
    if (url === "/api/accounts") return response([{ platform: "rain", ready: true, login_state: "logged_in", sync_state: "success" }]);
    if (url === "/api/bark") return response({ configured: true, server: "https://api.day.app" });
    if (url === "/api/analysis") return response({ configured: true, model: "claude-opus-5-5" });
    if (url === "/api/status") return response({ version: "fixture", demo: false });
    return response({});
  };
  const context = vm.createContext({ document, fetch, DueRadarTasks, AbortController, Intl, Date,
    window, location, history: { replaceState() {} },
    Option: function (text, value) { return { textContent: text, value }; },
    setTimeout(fn, delay) { const id = ++nextTimer; timers.set(id, { fn, delay }); return id; }, clearTimeout(id) { timers.delete(id); }, setInterval(fn) { interval = fn; },
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "app.js"), "utf8"), context);
  return page;
}

async function backgroundSettings(page) {
  const pending = deferred();
  const previous = structuredClone(page.settings);
  let waiting = false;
  page.intercept = ({ url, method }) => {
    if (!waiting && url === "/api/settings" && method === "GET") { waiting = true; return pending.promise; }
  };
  page.tick();
  await settle();
  assert.equal(waiting, true, "background refresh reached the delayed settings response");
  return { ...pending, previous };
}

test("saving during a background read queues a new snapshot and waits for it", async () => {
  const page = browser();
  await settle();
  const old = await backgroundSettings(page);
  page.$("deadline-minutes").value = "90";
  page.$("settings-form").emit("input");
  page.submit("settings-form");
  await settle();
  const stayedBusy = page.$("settings-form").querySelector("button").disabled;
  // Multiple callers coalesce into one subsequent snapshot.
  const refresh = page.click("refresh");
  page.document.listeners.visibilitychange();
  old.resolve(response(old.previous));
  await refresh;
  await settle();
  assert.equal(stayedBusy, true, "save must wait for a snapshot newer than the completed mutation");
  assert.equal(page.$("deadline-minutes").value, "90", "old background settings must not overwrite the saved value");
  assert.equal(page.calls.filter(call => call.url === "/api/session").length, 3, "queued callers share one follow-up snapshot");
});

for (const outcome of ["success", "failure"]) {
  test(`logout discards an earlier refresh ${outcome}`, async () => {
    const page = browser();
    await settle();
    const old = await backgroundSettings(page);
    await page.click("logout");
    assert.equal(page.$("login-screen").hidden, false);
    if (outcome === "success") old.resolve(response(old.previous));
    else old.reject(new Error("late old-session failure"));
    await settle();
    assert.equal(page.$("app").hidden, true, "late snapshot must not reopen the application");
    assert.equal(page.$("login-screen").hidden, false);
    assert.equal(page.$("login-error").textContent, "", "late error must not alter the login screen");
  });
}

test("new login starts a fresh read while an old request hangs and ignores its late 401", async () => {
  const page = browser();
  await settle();
  const old = await backgroundSettings(page);
  await page.click("logout");
  page.$("login-password").value = "synthetic-password";
  await page.submit("login-form");
  const newLoginVisible = !page.$("app").hidden;
  old.resolve(response({ error: "expired old request" }, 401));
  await settle();
  assert.equal(newLoginVisible, true, "new session must not wait for old network work");
  assert.equal(page.$("app").hidden, false, "old 401 must not invalidate a newer login");
  assert.equal(page.$("login-screen").hidden, true);
});

test("late mutation success after logout cannot refresh or notify the next screen", async () => {
  const page = browser();
  await settle();
  const sync = deferred();
  page.intercept = ({ url }) => url === "/api/sync" && sync.promise;
  const mutation = page.click("sync");
  await settle();
  await page.click("logout");
  const requests = page.calls.length;
  sync.resolve(response({ message: "late sync success" }));
  await mutation;
  await settle();
  assert.equal(page.calls.length, requests, "stale mutation must not initiate another refresh");
  assert.equal(page.$("toast").hidden, true, "stale mutation must not post a toast after logout");
});

test("late QR creation after logout cannot open a dialog or cancel a newer session", async () => {
  const page = browser();
  await settle();
  const creation = deferred();
  page.intercept = ({ url, method }) => url === "/api/accounts/rain/login" && method === "POST" && creation.promise;
  const bind = page.$("platform-list").children[0].children.find(child => child.tag === "button");
  const pending = bind.emit("click");
  await settle();
  await page.click("logout");
  const requests = page.calls.length;
  creation.resolve(response({ image: "synthetic-qr" }));
  await pending;
  await settle();
  assert.equal(page.$("rain-dialog").open, false, "old QR creation must not open over the login screen");
  assert.equal(page.calls.length, requests, "stale creation must not cancel a current server login");
  assert.equal(page.$("toast").hidden, true);
});

test("response body parsing from an old session cannot overwrite a new login", async () => {
  const page = browser();
  await settle();
  const body = deferred();
  let held = false;
  page.intercept = ({ url, method }) => {
    if (!held && url === "/api/settings" && method === "GET") {
      held = true;
      return { ...response({}), json: () => body.promise };
    }
  };
  page.tick();
  await settle();
  await page.click("logout");
  page.$("login-password").value = "synthetic-password";
  await page.submit("login-form");
  body.reject(new Error("old body parse error"));
  await settle();
  assert.equal(page.$("app").hidden, false);
  assert.equal(page.$("connection-error").hidden, true);
  assert.equal(page.$("login-error").textContent, "");
});

test("an already queued QR poll is inert after logout and a new login", async () => {
  const page = browser();
  await settle();
  const bind = page.$("platform-list").children[0].children.find(child => child.tag === "button");
  await bind.emit("click");
  const queuedPoll = page.takeTimer(1500);
  await page.click("logout");
  page.$("login-password").value = "synthetic-password";
  await page.submit("login-form");
  const requests = page.calls.length;
  await queuedPoll();
  assert.equal(page.calls.length, requests, "detached poll must not contact the server with current credentials");
  assert.equal(page.$("rain-dialog").open, false);
});

test("a queued old close event cannot cancel a replacement QR creation", async () => {
  const page = browser();
  await settle();
  let bind = page.$("platform-list").children[0].children.find(child => child.tag === "button");
  await bind.emit("click");
  page.deferClose = true;
  await page.click("logout");
  assert.equal(page.closeEvents.length, 1);
  page.$("login-password").value = "synthetic-password";
  await page.submit("login-form");
  const creation = deferred();
  page.intercept = ({ url, method }) => url === "/api/accounts/rain/login" && method === "POST" && creation.promise;
  bind = page.$("platform-list").children[0].children.find(child => child.tag === "button");
  const pending = bind.emit("click");
  await settle();
  const requests = page.calls.length;
  await page.closeEvents.shift()();
  creation.resolve(response({ image: "new-session-qr" }));
  await pending;
  assert.equal(page.calls.length, requests, "old close event must not cancel a replacement login");
  assert.equal(page.$("rain-dialog").open, true);
  assert.equal(page.$("rain-qr").src, "new-session-qr");
});

const fixtureTasks = [{ id: "performance-fixture", platform: "rain", title: "优先显示的测试任务", kind: "homework", status: "pending" }];
function visibleText(element) { return [element.textContent, ...element.children.map(visibleText)].join(" "); }

test("a changed account in another tab clears the old snapshot before reading again", async () => {
  let identity = "alpha";
  const page = browser(({ url }) => url === "/api/session" ? response({ user: { id: identity, username: identity } }) : null);
  await settle();
  assert.equal(page.$("app").hidden, false);
  for (const call of page.calls.filter(call => call.url !== "/api/session")) assert.equal(call.headers["X-DueRadar-Account"], "alpha");
  const reads = page.calls.length;
  identity = "bravo";
  page.tick();
  await settle();
  assert.equal(page.$("app").hidden, true);
  assert.equal(page.$("login-screen").hidden, false);
  assert.match(page.$("login-error").textContent, /其他页面切换/);
  assert.deepEqual(page.calls.slice(reads).map(call => call.url), ["/api/session"]);
});

test("an account mismatch during a refresh hides the prior user's data", async () => {
  const page = browser(({ url }) => url === "/api/session" ? response({ user: { id: "alpha", username: "alpha" } }) : null);
  await settle();
  page.intercept = ({ url }) => url === "/api/session" ? response({ user: { id: "alpha", username: "alpha" } }) : url === "/api/tasks" ? response({ error: "登录账号已切换，请重新加载" }, 409) : null;
  page.tick();
  await settle();
  assert.equal(page.$("app").hidden, true);
  assert.match(page.$("login-error").textContent, /其他页面切换/);
});

test("logout pins the account even after the view is cleared", async () => {
  const page = browser(({ url }) => url === "/api/session" ? response({ user: { id: "alpha", username: "alpha" } }) : null);
  await settle();
  await page.click("logout");
  const call = page.calls.find(call => call.url === "/api/logout");
  assert.equal(call.headers["X-DueRadar-Account"], "alpha");
});

test("an in-page anchor does not replace the current view", async () => {
  const page = browser();
  await settle();
  assert.equal(page.$("view-tasks").hidden, false);
  page.location.hash = "#accounts";
  page.window.listeners.hashchange();
  assert.equal(page.$("view-accounts").hidden, false);
  assert.equal(page.$("view-tasks").hidden, true);
  page.location.hash = "#main";
  page.window.listeners.hashchange();
  assert.equal(page.$("view-accounts").hidden, false, "skip-link target must not switch views");
  assert.equal(page.$("view-tasks").hidden, true);
});

test("a filter that matches nothing is not reported as an empty semester", async () => {
  const tasks = [
    { id: "current", platform: "rain", title: "本学期任务", kind: "homework", status: "pending", historical: false },
    { id: "previous", platform: "rain", title: "上学期任务", kind: "homework", status: "completed", historical: true },
  ];
  const page = browser(({ url }) => url === "/api/tasks" ? response(tasks) : null);
  await settle();
  page.$("task-search").value = "no-such-task";
  page.$("task-search").emit("input");
  const text = visibleText(page.$("task-list"));
  assert.match(text, /没有符合筛选条件/);
  assert.doesNotMatch(text, /本学期暂无任务/);
});

for (const slowPath of ["/api/events", "/api/accounts", "/api/settings", "/api/bark", "/api/analysis", "/api/status"]) {
  test(`tasks appear while ${slowPath} is still pending`, async () => {
    const slow = deferred();
    const page = browser(({ url }) => url === slowPath ? slow.promise : url === "/api/tasks" ? response(fixtureTasks) : null);
    await settle();
    assert.equal(page.$("app").hidden, false, "authenticated task list must not wait for auxiliary reads");
    assert.match(visibleText(page.$("task-list")), /优先显示的测试任务/);
    assert.match(page.$("service-label").textContent, /正在读取信息/, "pending sections must be indicated");
    assert.equal(page.$("connection-error").hidden, true, "loading progress is not an alert");
    if (slowPath === "/api/settings" || slowPath === "/api/status") assert.equal(page.$("settings-form").querySelector("button").disabled, true);
    if (slowPath === "/api/bark" || slowPath === "/api/status") assert.equal(page.$("bark-form").querySelector("button").disabled, true);
    if (slowPath === "/api/analysis" || slowPath === "/api/status") assert.equal(page.$("analysis-form").querySelector("button").disabled, true);
    slow.reject(new Error("auxiliary fixture failure"));
    await settle();
    assert.equal(page.$("app").hidden, false);
    assert.match(visibleText(page.$("task-list")), /优先显示的测试任务/);
    assert.equal(page.$("connection-error").hidden, false);
    assert.match(page.$("connection-error").textContent, /auxiliary fixture failure/);
  });
}

test("unknown authentication never fetches or reveals tasks", async () => {
  const session = deferred();
  const page = browser(({ url }) => url === "/api/session" ? session.promise : response(fixtureTasks));
  await settle();
  assert.equal(page.$("app").hidden, true);
  assert.deepEqual(page.calls.map(call => call.url), ["/api/session"]);
  session.resolve(response({ error: "session expired" }, 401));
  await settle();
  assert.equal(page.$("app").hidden, true);
  assert.equal(page.$("login-screen").hidden, false);
  assert.equal(page.$("login-error").textContent, "", "a first visit is not an expired session");
  assert.deepEqual(page.calls.map(call => call.url), ["/api/session"]);
});

for (const path of ["/api/tasks", "/api/settings"]) {
  test(`current ${path} 401 hides the application and ignores later reads`, async () => {
    const denied = deferred(), other = deferred();
    const page = browser(({ url }) => url === path ? denied.promise : url === "/api/bark" ? other.promise : url === "/api/tasks" ? response(fixtureTasks) : null);
    await settle();
    denied.resolve(response({ error: "session expired" }, 401));
    await settle();
    assert.equal(page.$("app").hidden, true);
    assert.equal(page.$("login-screen").hidden, false);
    if (path === "/api/settings") {
      assert.match(page.$("login-error").textContent, /管理会话已过期/, "an already signed-in 401 explains the return to login");
    }
    other.resolve(response({ configured: true, server: "fixture" }));
    await settle();
    assert.equal(page.$("app").hidden, true);
  });
}

test("an auxiliary failure does not release the refresh gate while another read is pending", async () => {
  const slow = deferred();
  let hold = true;
  const page = browser(({ url }) => hold && url === "/api/bark" ? slow.promise : hold && url === "/api/settings" ? response({ error: "settings fixture failure" }, 500) : null);
  await settle();
  const refresh = page.click("refresh");
  page.tick();
  page.document.listeners.visibilitychange();
  await settle();
  assert.equal(page.calls.filter(call => call.url === "/api/session").length, 1, "no overlapping refresh after a partial failure");
  hold = false;
  slow.resolve(response({ configured: true }));
  await refresh;
  await settle();
  assert.equal(page.calls.filter(call => call.url === "/api/session").length, 2, "all queued callers share one subsequent refresh");
  assert.equal(page.$("connection-error").hidden, true, "successful retry clears partial failure");
});

test("an unchanged refresh failure is not re-announced", async () => {
  let failing = false;
  const page = browser(({ url }) => failing && url === "/api/tasks" ? response({ error: "persistent fixture failure" }, 500) : null);
  await settle();
  failing = true;
  page.tick();
  await settle();
  assert.match(page.$("connection-error").textContent, /persistent fixture failure/);
  const writes = page.$("connection-error").textWrites;
  page.tick();
  await settle();
  assert.equal(page.$("connection-error").textWrites, writes, "the alert live region must not be rewritten with identical text");
});

test("a critical task failure is visible without presenting an empty successful list", async () => {
  const page = browser(({ url }) => url === "/api/tasks" ? response({ error: "tasks fixture failure" }, 500) : null);
  await settle();
  assert.equal(page.$("app").hidden, true);
  assert.match(page.$("login-error").textContent, /tasks fixture failure/);
});

test("settings inputs stay disabled until their saved values arrive", async () => {
  const settings = deferred();
  const page = browser(({ url }) => url === "/api/settings" && settings.promise);
  await settle();
  assert.equal(page.$("app").hidden, false);
  assert.equal(page.$("deadline-minutes").disabled, true, "unloaded settings must not become an editable empty draft");
  assert.equal(page.$("timezone").disabled, true);
  settings.resolve(response(page.settings));
  await settle();
  assert.equal(page.$("deadline-minutes").disabled, false);
  assert.equal(page.$("deadline-minutes").value, "1440, 180, 30");
  assert.equal(page.$("timezone").value, "Asia/Shanghai");
});

test("classroom assistants default off and save independent opt-ins", async () => {
  const page = browser();
  await settle();
  assert.equal(page.$("rain-fallback-enabled").checked, false);
  assert.equal(page.$("rain-analysis-enabled").checked, false);
  assert.equal(page.$("zhidao-in-class-enabled").checked, true);
  assert.equal(page.$("poll-wake-enabled").checked, false);
  page.$("rain-fallback-enabled").checked = true;
  page.$("poll-wake-enabled").checked = true;
  page.$("settings-form").emit("input");
  await page.submit("settings-form");
  await settle();
  const saved = page.calls.find(call => call.url === "/api/settings" && call.method === "PUT");
  assert.equal(saved.body.rain_fallback_enabled, true);
  assert.equal(saved.body.rain_analysis_enabled, false);
  assert.equal(saved.body.poll_wake_enabled, true);
  await page.click("settings-defaults");
  assert.equal(page.$("rain-fallback-enabled").checked, false);
  assert.equal(page.$("rain-analysis-enabled").checked, false);
  assert.equal(page.$("zhidao-in-class-enabled").checked, true);
  assert.equal(page.$("poll-wake-enabled").checked, false);
});

test("background refresh preserves an unsaved model and key", async () => {
  const page = browser();
  await settle();
  page.$("analysis-model").value = "claude-sonnet-5-5";
  page.$("analysis-key").value = "fixture-unsaved-model-key";
  page.$("analysis-form").emit("input");
  await page.click("refresh");
  assert.equal(page.$("analysis-model").value, "claude-sonnet-5-5");
  assert.equal(page.$("analysis-key").value, "fixture-unsaved-model-key");
  assert.equal(page.calls.some(call => call.url === "/api/analysis" && call.method === "PUT"), false);
});

test("saving model configuration clears the draft key only after success", async () => {
  const page = browser();
  await settle();
  const save = deferred();
  page.intercept = ({url, method}) => url === "/api/analysis" && method === "PUT" && save.promise;
  page.$("analysis-model").value = "claude-sonnet-5-5";
  page.$("analysis-key").value = "fixture-unsaved-model-key";
  page.$("analysis-form").emit("input");
  page.submit("analysis-form");
  await settle();
  assert.equal(page.$("analysis-key").value, "fixture-unsaved-model-key");
  const sent = page.calls.find(call => call.url === "/api/analysis" && call.method === "PUT");
  assert.deepEqual(sent.body, {key:"fixture-unsaved-model-key", model:"claude-sonnet-5-5"});
  save.resolve(response({configured:true, model:"claude-sonnet-5-5"}));
  await settle();
  assert.equal(page.$("analysis-key").value, "");
  assert.match(page.$("toast").textContent, /未调用模型/);
  assert.equal(page.calls.some(call => call.url === "/api/bark/test"), false);
});

test("clearing model key sends an explicit removal request", async () => {
  const page = browser();
  await settle();
  await page.click("analysis-clear");
  const sent = page.calls.find(call => call.url === "/api/analysis" && call.method === "PUT");
  assert.deepEqual(sent.body, {model:"claude-opus-5-5", clear_key:true});
  assert.match(page.$("toast").textContent, /不再调用模型/);
});

test("logout clears model drafts and fences a late model configuration save", async () => {
  const page = browser();
  await settle();
  const save = deferred();
  page.intercept = ({url, method}) => url === "/api/analysis" && method === "PUT" && save.promise;
  page.$("analysis-key").value = "fixture-unsaved-model-key";
  page.$("analysis-form").emit("input");
  page.submit("analysis-form");
  await settle();
  await page.click("logout");
  assert.equal(page.$("analysis-key").value, "");
  assert.equal(page.$("analysis-model").value, "");
  const requests = page.calls.length;
  save.resolve(response({configured:true}));
  await settle();
  assert.equal(page.calls.length, requests);
  assert.equal(page.$("app").hidden, true);
  assert.equal(page.$("toast").hidden, true);
});
