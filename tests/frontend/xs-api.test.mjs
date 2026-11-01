import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-api.js", import.meta.url), "utf8");
const contextSource = fs.readFileSync(new URL("../../public/xs-cluster-context.js", import.meta.url), "utf8");

function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    clear: () => m.clear(),
  };
}

function load({ healthOk = true, failProfileOnce = false, expireOnce = false, refreshFails = false, withContext = false, initialTokens = null, intercept } = {}) {
  const calls = [];
  let accessSeq = 0;
  const storage = memoryStorage();
  if (initialTokens) {
    storage.setItem("xs_access_token", initialTokens.access);
    storage.setItem("xs_refresh_token", initialTokens.refresh);
  }
  const state = { failProfileOnce, expireOnce, refreshFails };
  const fetch = async (url, init = {}) => {
    calls.push({ url, init });
    const u = String(url);
    const headers = new Headers(init.headers || {});
    const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    if (intercept) {
      const response = intercept(u, init, json);
      if (response !== undefined) return response;
    }
    if (u.endsWith("/health")) {
      return healthOk ? json(200, { code: 0, data: { status: "ok" } }) : Promise.reject(new Error("offline"));
    }
    if (u.endsWith("/auth/login")) {
      accessSeq++;
      return json(200, { code: 0, data: { token: { access_token: "a" + accessSeq, refresh_token: "r1" }, user: { id: 1, username: "admin", display_name: "管理员", must_change_password: false } } });
    }
    if (u.endsWith("/auth/refresh")) {
      if (state.refreshFails) return json(500, { code: 50000, message: "refresh failed" });
      accessSeq++;
      return json(200, { code: 0, data: { access_token: "a" + accessSeq, refresh_token: "r2" } });
    }
    if (u.endsWith("/auth/me")) {
      if (state.failProfileOnce) {
        state.failProfileOnce = false;
        return json(401, { code: 40100, message: "expired" });
      }
      return json(200, { code: 0, data: { id: 1, username: "admin", roles: ["超级管理员"], permissions: ["*"], must_change_password: false } });
    }
    if (u.endsWith("/approve")) {
      return json(202, { code: 40304, message: "已提交审批", data: { id: 9 } });
    }
    if (u.endsWith("/users")) {
      const auth = headers.get("authorization");
      if (state.expireOnce && auth === "Bearer a1") {
        state.expireOnce = false;
        return json(401, { code: 40101, message: "expired" });
      }
      if (!auth) return json(401, { code: 40100, message: "no token" });
      return json(200, { code: 0, data: { items: [{ id: 1 }] } });
    }
    return json(404, { code: 40400, message: "not found" });
  };
  const window = { localStorage: storage };
  const localStorage = storage;
  const context = {
    window, localStorage, fetch, console, setTimeout, clearTimeout,
    AbortController, Response, Headers, Request, URLSearchParams, TextDecoder, FormData,
    navigator: { onLine: true },
    location: { protocol: "http:", hostname: "127.0.0.1", port: "18080", href: "http://127.0.0.1:18080/" },
    WebSocket: class {
      constructor(url) { this.url = url; this.closed = false; this.listeners = new Map(); }
      addEventListener(name, fn) { this.listeners.set(name, fn); }
      close() { this.closed = true; this.listeners.get("close")?.(); }
    },
  };
  context.globalThis = context;
  window.window = window;
  vm.createContext(context);
  if (withContext) vm.runInContext(contextSource, context, { filename: "xs-cluster-context.js" });
  vm.runInContext(source, context, { filename: "xs-api.js" });
  return { XS: window.XS, cluster: window.XSClusterContext, calls, storage, state };
}

test("probe + login + authorized request", async () => {
  const { XS, storage } = load();
  assert.equal(await XS.ready, "live");
  assert.equal(XS.base, "/api/v1");
  const r = await XS.auth.login("admin", "pw");
  assert.equal(r.user.username, "admin");
  assert.equal(storage.getItem("xs_access_token"), "a1");
  const me = await XS.auth.me();
  assert.deepEqual(me.roles, ["超级管理员"]);
  const users = await XS.get("/users");
  assert.equal(users.items.length, 1);
});

test("bootstrap probes once and restores one profile from an existing session", async () => {
  const { XS, calls } = load({ initialTokens: { access: "existing", refresh: "refresh" } });
  await XS.ready;
  assert.equal(calls.filter((call) => call.url.endsWith("/auth/me")).length, 0);
  assert.equal((await XS.auth.restore()).username, "admin");
  assert.equal(calls.filter((call) => call.url.endsWith("/health")).length, 1);
  assert.equal(calls.filter((call) => call.url.endsWith("/auth/me")).length, 1);
});

test("401 triggers one refresh and one retry", async () => {
  const { XS, calls } = load({ expireOnce: true });
  await XS.ready;
  await XS.auth.login("admin", "pw");
  const users = await XS.get("/users");
  assert.equal(users.items.length, 1);
  assert.equal(calls.filter((c) => c.url.endsWith("/users")).length, 2);
  assert.equal(calls.filter((c) => c.url.endsWith("/auth/refresh")).length, 1);
  assert.ok(XS.token().startsWith("a"));
});

test("202 approval is surfaced as a typed error", async () => {
  const { XS } = load();
  await XS.ready;
  await XS.auth.login("admin", "pw");
  await assert.rejects(() => XS.post("/approve", { replicas: 3 }), (err) => {
    assert.equal(XS.isApproval(err), true);
    assert.equal(err.code, 40304);
    assert.equal(err.approval.id, 9);
    return true;
  });
});

test("falls back to demo when backend is absent", async () => {
  const { XS } = load({ healthOk: false });
  assert.equal(await XS.ready, "demo");
  assert.equal(XS.mode, "demo");
});

test("refresh failure clears the session", async () => {
  const { XS, storage } = load({ expireOnce: true, refreshFails: true });
  await XS.ready;
  await XS.auth.login("admin", "pw");
  await assert.rejects(() => XS.get("/users"));
  assert.equal(storage.getItem("xs_access_token"), null);
  assert.equal(XS.token(), "");
});

test("cluster-scoped request adds only the active cluster id; platform requests stay unchanged", async () => {
  const { XS, cluster, calls } = load({ withContext: true, intercept: (url, _, json) => {
    if (url.includes("/observability/query")) return json(200, { code: 0, data: { ok: true } });
  } });
  await XS.ready;
  await assert.rejects(() => XS.get("/observability/query", {}, { scope: "cluster" }), (err) => err.code === "XS_CLUSTER_REQUIRED");
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  await XS.get("/observability/query", { q: "up" }, { scope: "cluster" });
  const scoped = calls.find((call) => call.url.includes("/observability/query"));
  assert.equal(new URL(scoped.url, "http://localhost").searchParams.get("cluster_id"), "42");
  assert.equal(new URL(scoped.url, "http://localhost").searchParams.get("q"), "up");
  const platform = await XS.get("/observability/query", { q: "up" }, { scope: "platform" });
  assert.equal(platform.ok, true);
  assert.equal(new URL(calls.at(-1).url, "http://localhost").searchParams.has("cluster_id"), false);
  await assert.rejects(() => XS.get("/observability/query", { cluster_id: "43" }, { scope: "cluster" }),
    (err) => err.code === "XS_CLUSTER_MISMATCH");
  await assert.rejects(() => XS.get("/observability/query?cluster_id=43", null, { scope: "cluster" }),
    (err) => err.code === "XS_CLUSTER_MISMATCH");
  await XS.get("/observability/query?cluster_id=42", { cluster_id: "42" }, { scope: "cluster" });
  assert.equal(new URL(calls.at(-1).url, "http://localhost").searchParams.getAll("cluster_id").length, 1);
  await assert.rejects(() => XS.get("/observability/query", null, { scope: "aggregate" }),
    (err) => err.code === "XS_SCOPE_UNSUPPORTED");
});

test("late responses from a previous cluster cannot commit after a context switch", async () => {
  let release;
  const { XS, cluster, calls } = load({ withContext: true, intercept: (url, _, json) => {
    if (url.includes("/observability/query")) return new Promise((resolve) => { release = () => resolve(json(200, { code: 0, data: { from: "alpha" } })); });
  } });
  await XS.ready;
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  const pending = XS.get("/observability/query", null, { scope: "cluster", timeoutMs: 0 });
  assert.equal(calls.at(-1).init.signal.aborted, false);
  cluster.set({ kind: "cluster", cluster_id: 43, name: "beta", env: "prod" });
  assert.equal(calls.at(-1).init.signal.aborted, true);
  release();
  await assert.rejects(() => pending, (err) => XS.isStale(err));
});

test("page switch cancels caller scopes without disturbing auth or platform requests", async () => {
  let release;
  const { XS, cluster, calls } = load({ withContext: true, intercept: (url, _, json) => {
    if (url.includes("/observability/query")) return new Promise((resolve) => { release = () => resolve(json(200, { code: 0, data: 1 })); });
  } });
  await XS.ready;
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  cluster.registerPageScope("metrics", "cluster");
  cluster.registerPageScope("settings", "platform");
  cluster.enterPage("metrics");
  const scope = XS.createRequestScope();
  const pending = XS.get("/observability/query", null, { scope: "cluster", signal: scope.signal, timeoutMs: 0 });
  cluster.enterPage("settings");
  assert.equal(calls.at(-1).init.signal.aborted, true);
  release();
  await assert.rejects(() => pending, (err) => XS.isCancelled(err));
  assert.equal(await XS.ready, "live");
});

test("cluster switch during 401 refresh never retries an old cluster request", async () => {
  let releaseRefresh;
  let refreshStarted;
  const started = new Promise((resolve) => { refreshStarted = resolve; });
  let attempts = 0;
  const { XS, cluster } = load({ withContext: true, intercept: (url, _, json) => {
    if (url.endsWith("/auth/refresh")) return new Promise((resolve) => {
      releaseRefresh = () => resolve(json(200, { code: 0, data: { access_token: "a2", refresh_token: "r2" } }));
      refreshStarted();
    });
    if (url.includes("/observability/query")) {
      attempts++;
      return json(401, { code: 40101, message: "expired" });
    }
  } });
  await XS.ready;
  await XS.auth.login("admin", "pw");
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  const pending = XS.get("/observability/query", null, { scope: "cluster" });
  await started;
  assert.equal(typeof releaseRefresh, "function");
  cluster.set({ kind: "cluster", cluster_id: 43, name: "beta", env: "prod" });
  releaseRefresh();
  await assert.rejects(() => pending, (err) => XS.isStale(err));
  assert.equal(attempts, 1);
});

test("platform requests remain live during cluster changes", async () => {
  let release;
  const { XS, cluster, calls } = load({ withContext: true, intercept: (url, _, json) => {
    if (url.includes("/settings")) return new Promise((resolve) => { release = () => resolve(json(200, { code: 0, data: { ok: true } })); });
  } });
  await XS.ready;
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  const pending = XS.get("/settings", null, { scope: "platform", timeoutMs: 0 });
  cluster.set({ kind: "cluster", cluster_id: 43, name: "beta", env: "prod" });
  assert.equal(calls.at(-1).init.signal.aborted, false);
  release();
  assert.equal((await pending).ok, true);
});

test("cluster-scoped WebSocket and SSE close before delivering old cluster messages", async () => {
  let readFrame;
  let markReadStarted;
  const readStarted = new Promise((resolve) => { markReadStarted = resolve; });
  const delivered = [];
  const { XS, cluster, calls } = load({ withContext: true, intercept: (url) => {
    if (url.includes("/events")) return {
      ok: true,
      body: { getReader: () => ({ read: () => new Promise((resolve) => {
        readFrame = resolve;
        markReadStarted();
      }) }) },
    };
  } });
  await XS.ready;
  cluster.set({ kind: "cluster", cluster_id: 42, name: "alpha", env: "prod" });
  const ws = XS.ws("/events", { source: "logs" }, { scope: "cluster" });
  assert.equal(new URL(ws.url, "http://localhost").searchParams.get("cluster_id"), "42");
  const stop = XS.stream("/events", { message: (_, data) => delivered.push(data) }, { source: "logs" }, { scope: "cluster" });
  await readStarted;
  assert.equal(typeof readFrame, "function");
  assert.equal(new URL(calls.at(-1).url, "http://localhost").searchParams.get("cluster_id"), "42");
  cluster.set({ kind: "cluster", cluster_id: 43, name: "beta", env: "prod" });
  assert.equal(ws.closed, true);
  const staleEvent = { stopped: false, stopImmediatePropagation() { this.stopped = true; } };
  ws.listeners.get("message")(staleEvent);
  assert.equal(staleEvent.stopped, true);
  assert.equal(calls.at(-1).init.signal.aborted, true);
  readFrame({ done: false, value: new TextEncoder().encode("event: log\ndata: stale\n\n") });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(delivered, []);
  stop();
});
