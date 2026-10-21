const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

// Exercise the browser flow with deferred HTTP responses and dialog events.
function browser(api) {
  const elements = new Map();
  let timer;
  const $ = (id) => {
    if (!elements.has(id)) elements.set(id, {
      open: false, listeners: {},
      addEventListener(name, fn) { this.listeners[name] = fn; },
      showModal() { this.open = true; },
      close() { this.open = false; return this.listeners.close?.(); },
      removeAttribute(name) { delete this[name]; },
    });
    return elements.get(id);
  };
  const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
  const flow = source.slice(source.indexOf("  let activeLogin;"), source.indexOf("  function eventTitle("));
  assert.ok(flow.length > 100, "login flow found in application source");
  const context = vm.createContext({ $, api, notify() {}, refreshState: async () => {},
    setTimeout(fn) { timer = fn; return fn; }, clearTimeout(fn) { if (timer === fn) timer = null; },
  });
  vm.runInContext(flow, context);
  return { open: context.openRainLogin, $, poll: () => { const fn = timer; timer = null; return fn(); } };
}

test("simultaneous platform clicks keep creation, polling and cancel on the same route", async () => {
  const creation = deferred();
  const cancel = deferred();
  const calls = [];
  const page = browser((url, method = "GET") => {
    calls.push([url, method]);
    if (url.endsWith("/cancel")) return cancel.promise;
    if (method === "POST") return creation.promise;
    return Promise.resolve({ state: "waiting" });
  });
  const first = page.open("rain");
  await page.open("zhidao_campus");
  assert.equal(calls.length, 1);
  creation.resolve({ image: "fixture-qr" });
  await first;
  await page.poll();
  const closing = page.$("rain-dialog").close();
  await page.open("zhidao_campus");
  assert.deepEqual(calls, [
    ["/api/accounts/rain/login", "POST"],
    ["/api/accounts/rain/login", "GET"],
    ["/api/accounts/rain/login/cancel", "POST"],
  ]);
  cancel.resolve({});
  await closing;
  await page.open("zhidao_campus");
  assert.deepEqual(calls.at(-1), ["/api/accounts/zhidao/login", "POST"]);
});

test("a late closed-flow poll cannot change a later platform dialog", async () => {
  const oldPoll = deferred();
  const page = browser(async (url, method = "GET") => {
    if (method === "POST") return { image: "fixture-qr" };
    return oldPoll.promise;
  });
  await page.open("rain");
  const pending = page.poll();
  await page.$("rain-dialog").close();
  await page.open("zhidao_campus");
  const before = page.$("rain-login-status").textContent;
  oldPoll.resolve({ state: "failed", error: "old flow failure" });
  await pending;
  assert.equal(page.$("rain-login-status").textContent, before);
  assert.equal(page.$("rain-dialog").open, true);
});

test("successful login closes without canceling the confirmed session", async () => {
  const calls = [];
  const page = browser(async (url, method = "GET") => {
    calls.push([url, method]);
    return method === "POST" ? { image: "fixture-qr" } : { state: "logged_in" };
  });
  await page.open("zhidao_campus");
  await page.poll();
  assert.equal(page.$("rain-dialog").open, false);
  assert.equal(calls.some(([url]) => url.endsWith("/cancel")), false);
  await page.open("rain");
  assert.equal(calls.at(-1)[0], "/api/accounts/rain/login");
});
