import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-api.js", import.meta.url), "utf8");

function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    clear: () => m.clear(),
  };
}

function load({ healthOk = true, failProfileOnce = false, expireOnce = false, refreshFails = false } = {}) {
  const calls = [];
  let accessSeq = 0;
  const storage = memoryStorage();
  const state = { failProfileOnce, expireOnce, refreshFails };
  const fetch = async (url, init = {}) => {
    calls.push({ url, init });
    const u = String(url);
    const headers = new Headers(init.headers || {});
    const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
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
  const window = {};
  const localStorage = storage;
  const context = {
    window, localStorage, fetch, console, setTimeout, clearTimeout,
    AbortController, Response, Headers, Request, URLSearchParams, TextDecoder, FormData,
    navigator: { onLine: true },
    location: { protocol: "http:", hostname: "127.0.0.1", port: "18080", href: "http://127.0.0.1:18080/" },
    WebSocket: class {},
  };
  context.globalThis = context;
  window.window = window;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "xs-api.js" });
  return { XS: window.XS, calls, storage, state };
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
