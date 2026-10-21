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
function browser() {
  const elements = new Map();
  function element(tag = "div") {
    return {
      tag, value: "", textContent: "", hidden: true, checked: false, disabled: false,
      children: [], listeners: {}, dataset: {}, open: false,
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener(name, fn) { this.listeners[name] = fn; },
      emit(name, extra = {}) { return this.listeners[name]?.({ currentTarget: this, preventDefault() {}, ...extra }); },
      append(...children) { this.children.push(...children); },
      replaceChildren(...children) { this.children = children; },
      setAttribute(name, value) { this[name] = value; },
      removeAttribute(name) { delete this[name]; },
      querySelector() { return this.button ||= element("button"); },
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
  let interval;
  let loggedIn = true;
  const calls = [];
  const timers = new Map();
  let nextTimer = 0;
  const document = { hidden: false, getElementById: $, createElement: element, createDocumentFragment: element, querySelectorAll: () => [], listeners: {}, addEventListener(name, fn) { this.listeners[name] = fn; } };
  const page = {
    $, calls, document, intercept: null, deferClose: false, closeEvents: [],
    settings: { deadline_minutes: [1440, 180, 30], exam_start_minutes: [1440, 60], video_minutes: [4320, 1440, 180], quiet_enabled: true, quiet_start: "23:00", quiet_end: "08:00", timezone: "Asia/Shanghai" },
    click: (id) => $(id).emit("click"),
    submit: (id) => $(id).emit("submit", { submitter: $(id).querySelector("button") }),
    tick: () => interval(),
    takeTimer(delay) { const found = [...timers].find(([, timer]) => timer.delay === delay); assert.ok(found, `timer ${delay} exists`); timers.delete(found[0]); return found[1].fn; },
  };
  const fetch = async (url, options) => {
    const call = { url, method: options.method, body: options.body && JSON.parse(options.body) };
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
    if (url === "/api/status") return response({ version: "fixture", demo: false });
    return response({});
  };
  const context = vm.createContext({ document, fetch, DueRadarTasks, AbortController, Intl, Date,
    window: { addEventListener() {} }, location: { hash: "" }, history: { replaceState() {} },
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
