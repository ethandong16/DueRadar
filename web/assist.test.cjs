const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function browser(api) {
  const elements = new Map();
  let timer;
  const $ = (id) => {
    if (!elements.has(id)) elements.set(id, {
      open: false, value: "", listeners: {},
      addEventListener(name, fn) { this.listeners[name] = fn; },
      showModal() { this.open = true; },
      close() { this.open = false; return this.listeners.close?.(); },
      removeAttribute(name) { delete this[name]; },
    });
    return elements.get(id);
  };
  const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
  const flow = source.slice(source.indexOf("  let activeLogin;"), source.indexOf("  function eventTitle("));
  const context = vm.createContext({ $, api, notify() {}, refreshState: async () => {},
    location: { origin: "https://fixture.example" },
    setTimeout(fn) { timer = fn; return fn; }, clearTimeout(fn) { if (timer === fn) timer = null; },
  });
  vm.runInContext(flow, context);
  return { open: context.openRainLogin, clear: context.clearActiveLogin, $, hasTimer: () => !!timer,
    poll: async () => { const fn = timer; timer = null; assert.ok(fn, "poll scheduled"); await fn(); } };
}
const creation = { flow_id: "fixture-flow", pairing_code: "synthetic-one-use-code", expires_at: "2099-01-01T00:00:00Z", expires_in: 300 };

test("assistant pairing displays once and uses only its scoped flow for poll/cancel", async () => {
  const calls = [];
  const page = browser(async (url, method = "GET") => {
    calls.push([url, method]);
    return url === "/api/accounts/rain/assist" ? creation : { state: "awaiting_user" };
  });
  await page.open("rain", "assist");
  assert.equal(page.$("rain-assist").hidden, false);
  assert.equal(page.$("rain-qr").hidden, true);
  assert.equal(page.$("assist-code").value, creation.pairing_code);
  assert.equal(page.$("assist-instance").value, "https://fixture.example");
  await page.open("zhidao_campus");
  assert.equal(calls.length, 1);
  await page.poll();
  assert.equal(page.$("assist-code").value, "", "consumed code removed from DOM");
  await page.$("rain-dialog").close();
  assert.deepEqual(calls, [["/api/accounts/rain/assist", "POST"], ["/api/accounts/rain/assist/fixture-flow", "GET"], ["/api/accounts/rain/assist/fixture-flow/cancel", "POST"]]);
  assert.equal(page.$("assist-flow").value, "");
  assert.equal(JSON.stringify(calls).includes(creation.pairing_code), false);
});

test("assistant terminal states clear pairing information and stop polling", async () => {
  for (const state of ["expired", "failed", "canceled"]) {
    const page = browser(async (url) => url === "/api/accounts/rain/assist" ? creation : { state });
    await page.open("rain", "assist");
    await page.poll();
    for (const id of ["assist-code", "assist-flow", "assist-instance"]) assert.equal(page.$(id).value, "");
    assert.equal(page.hasTimer(), false);
    assert.notEqual(page.$("rain-login-status").textContent, "");
  }
});

test("transient status error retries without reconnecting or uploading", async () => {
  let polls = 0;
  const calls = [];
  const page = browser(async (url, method = "GET") => {
    calls.push([url, method]);
    if (method === "POST") return creation;
    if (++polls === 1) throw new Error("fixture timeout");
    return { state: "verifying" };
  });
  await page.open("rain", "assist");
  await page.poll();
  assert.equal(page.hasTimer(), true);
  await page.poll();
  assert.match(page.$("rain-login-status").textContent, /验证/);
  assert.equal(calls.filter(([, method]) => method === "POST").length, 1);
});

test("logout and late assistant response cannot reveal pairing information", async () => {
  let finish;
  const pending = new Promise((resolve) => { finish = resolve; });
  const page = browser(() => pending);
  const opening = page.open("rain", "assist");
  page.clear();
  finish(creation);
  await opening;
  assert.equal(page.$("rain-dialog").open, false);
  assert.equal(page.$("assist-code").value, "");
  assert.equal(page.hasTimer(), false);
});

test("confirmed assisted login clears secrets and closes without cancel", async () => {
  const calls = [];
  const page = browser(async (url, method = "GET") => {
    calls.push([url, method]);
    return method === "POST" ? creation : { state: "logged_in" };
  });
  await page.open("rain", "assist");
  await page.poll();
  assert.equal(page.$("rain-dialog").open, false);
  assert.equal(page.$("assist-code").value, "");
  assert.equal(calls.some(([url]) => url.endsWith("/cancel")), false);
});

test("an expired pairing clears locally even if status is unavailable", async () => {
  let calls = 0;
  const page = browser(async () => { calls++; return { ...creation, expires_at: "2000-01-01T00:00:00Z" }; });
  await page.open("rain", "assist");
  await page.poll();
  assert.equal(calls, 1);
  assert.equal(page.$("assist-code").value, "");
  assert.equal(page.hasTimer(), false);
});
