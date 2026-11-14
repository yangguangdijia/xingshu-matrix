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

function load({ protocol = "http:", ticket = "tk/1+=", ticketFails = false } = {}) {
  const calls = [];
  const storage = memoryStorage();
  storage.setItem("xs_access_token", "acc");
  const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  const fetch = async (url, init = {}) => {
    const u = String(url);
    calls.push({ url: u, init });
    if (u.endsWith("/health")) return json(200, { code: 0, data: { status: "ok" } });
    if (u.endsWith("/auth/ws-ticket")) {
      if (ticketFails) return json(500, { code: 50000, message: "boom" });
      return json(200, { code: 0, data: { ticket } });
    }
    return json(404, { code: 40400, message: "not found" });
  };
  const window = { localStorage: storage };
  const context = {
    window, localStorage: storage, fetch, console, setTimeout, clearTimeout,
    AbortController, Response, Headers, Request, URLSearchParams, TextDecoder, FormData,
    navigator: { onLine: true },
    location: { protocol, hostname: "127.0.0.1", port: "18080", href: protocol + "//127.0.0.1:18080/" },
    WebSocket: class {
      constructor(url) { this.url = url; this.listeners = new Map(); }
      addEventListener(name, fn) { this.listeners.set(name, fn); }
      close() { this.listeners.get("close")?.(); }
    },
  };
  context.globalThis = context;
  window.window = window;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "xs-api.js" });
  return { XS: window.XS, calls };
}

test("wsTicket POSTs /auth/ws-ticket and returns data.ticket", async () => {
  const { XS, calls } = load({ ticket: "abc" });
  await XS.ready;
  assert.equal(await XS.wsTicket(), "abc");
  const call = calls.find((c) => c.url.endsWith("/auth/ws-ticket"));
  assert.equal(call.init.method, "POST");
});

test("wsConnect appends ?ticket= with ws scheme", async () => {
  const { XS } = load({ ticket: "tk/1+=" });
  await XS.ready;
  XS.base = "http://127.0.0.1:18080/api/v1";
  const ws = await XS.wsConnect("/pods/exec");
  assert.equal(ws.url, "ws://127.0.0.1:18080/api/v1/pods/exec?ticket=" + encodeURIComponent("tk/1+="));
});

test("wsConnect uses &ticket= when path has a query and wss on https", async () => {
  const { XS } = load({ protocol: "https:", ticket: "t2" });
  await XS.ready;
  XS.base = "https://example.com/api/v1";
  const ws = await XS.wsConnect("logs?follow=1");
  assert.equal(ws.url, "wss://example.com/api/v1/logs?follow=1&ticket=t2");
  assert.ok(!ws.url.includes("access_token"));
});

test("wsConnect falls back to XS.ws when ticket fetch fails", async () => {
  const { XS } = load({ ticketFails: true });
  await XS.ready;
  XS.base = "http://127.0.0.1:18080/api/v1";
  const ws = await XS.wsConnect("/pods/exec");
  assert.equal(ws.url, "ws://127.0.0.1:18080/api/v1/pods/exec?access_token=acc");
  assert.ok(!ws.url.includes("ticket="));
});
