import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

const source = fs.readFileSync(new URL("../../public/xs-bastion.js", import.meta.url), "utf8");

// harness Element 没有 style / classList，这里补上最小实现
if (!Object.getOwnPropertyDescriptor(Element.prototype, "style")) {
  Object.defineProperty(Element.prototype, "style", { get() { return (this._style ||= { cssText: "" }); } });
  Object.defineProperty(Element.prototype, "classList", {
    get() {
      const el = this;
      const list = () => (el.attrs.class || "").split(/\s+/).filter(Boolean);
      return {
        contains: (c) => list().includes(c),
        add: (c) => { if (!list().includes(c)) el.attrs.class = list().concat(c).join(" "); },
        remove: (c) => { el.attrs.class = list().filter((x) => x !== c).join(" "); }
      };
    }
  });
}

class FakeWS {
  constructor(url) { this.url = url; this.readyState = 0; this.sent = []; this.closedWith = null; FakeWS.all.push(this); }
  send(d) { this.sent.push(JSON.parse(d)); }
  close(code) { this.closedWith = code ?? 1005; this.readyState = 3; }
  open() { this.readyState = 1; this.onopen && this.onopen({}); }
  msg(m) { this.onmessage && this.onmessage({ data: JSON.stringify(m) }); }
  drop(code = 1006) { this.readyState = 3; this.onclose && this.onclose({ code }); }
}
FakeWS.all = [];

function setup({ get } = {}) {
  FakeWS.all = [];
  const timers = [];
  const store = new Map();
  const head = new Element("head"), body = new Element("body"), toasts = [];
  const document = {
    head, body, readyState: "loading",
    createElement: (t) => new Element(t),
    querySelector: (sel) => body.querySelector(sel),
    addEventListener() {}, removeEventListener() {}
  };
  const window = {
    document,
    localStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    XS: {
      mode: "live",
      toast: (m) => toasts.push(m),
      ws: (path, q) => new FakeWS(path + "?" + new URLSearchParams(q)),
      get: get || (async () => { throw Object.assign(new Error("forbidden"), { status: 403 }); })
    }
  };
  const context = vm.createContext({
    window, document, console, URLSearchParams, JSON, Promise,
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    clearTimeout: (id) => { if (timers[id - 1]) timers[id - 1].fn = null; }
  });
  vm.runInContext(source, context);
  const B = window.XSBastion;
  const runTimer = async () => { const t = timers.find((x) => x.fn); if (!t) return null; const fn = t.fn; t.fn = null; fn(); await tick(); await tick(); return t.ms; };
  return { window, B, T: B.term, timers, runTimer, store, body, toasts };
}

function session() {
  const out = [];
  const s = { id: 7, live: true, host: "web-01", acct: "ops", screen: [[]], cx: 0, cy: 0, ws: null, cols: 120, rows: 32 };
  const cb = { feed: (x, t) => out.push(t), paint() {}, rerender() { cb.renders++; }, renders: 0 };
  return { s, cb, out, text: () => out.join("") };
}
const grant = { asset_id: 11, account_id: 21, asset_name: "web-01" };

test("classifyClose: only unexpected drops of an opened session are retried", () => {
  const { T } = setup();
  assert.equal(T.classifyClose(1006, { everOpened: true }).retry, true);
  assert.equal(T.classifyClose(1011, { everOpened: true }).retry, true);
  assert.equal(T.classifyClose(1000, { everOpened: true }).retry, false);
  assert.equal(T.classifyClose(1008, { everOpened: true }).retry, false);
  assert.equal(T.classifyClose(4003, { everOpened: true }).state, "failed");
  assert.equal(T.classifyClose(1006, { everOpened: false }).retry, false, "never opened = rejected before upgrade (auth/ACL/recording)");
  assert.equal(T.classifyClose(1006, { everOpened: true, ended: true }).state, "closed");
  assert.equal(T.classifyClose(1006, { everOpened: true, fatal: true, err: "x" }).state, "failed");
  assert.equal(T.classifyClose(1006, { everOpened: true, manual: true }).retry, false);
  assert.deepEqual([1, 2, 3, 4, 5, 6].map(T.backoff), [1000, 2000, 4000, 8000, 16000, 16000]);
});

test("unexpected close reconnects with exponential backoff, at most 5 tries, then offers manual reconnect", async () => {
  const h = setup();
  const { s, cb, text } = session();
  h.T.connect(s, grant, cb);
  assert.equal(FakeWS.all.length, 1);
  assert.match(FakeWS.all[0].url, /asset=11&account=21&cols=120&rows=32/);
  FakeWS.all[0].msg({ type: "session", data: "501" });
  FakeWS.all[0].open();
  assert.equal(s.conn, "open");
  assert.match(h.T.statusBar(s), /已连接/);
  assert.match(h.T.statusBar(s), /#16a34a/);

  FakeWS.all[0].drop(1006);
  assert.equal(s.conn, "reconnecting");
  assert.equal(s.attempt, 1);
  assert.match(h.T.statusBar(s), /重连中（1\/5）/);
  assert.match(h.T.statusBar(s), /#d97706/);
  assert.match(text(), /正在自动重连/);

  const delays = [];
  for (let i = 0; i < 5; i++) {
    delays.push(await h.runTimer());
    const ws = FakeWS.all.at(-1);
    assert.equal(FakeWS.all.length, i + 2, "new socket per attempt");
    ws.drop(1006); // 每次都在打开前失败
  }
  assert.deepEqual(delays, [1000, 2000, 4000, 8000, 16000]);
  assert.equal(s.conn, "failed");
  assert.equal(await h.runTimer(), null, "no 6th attempt");
  const bar = h.T.statusBar(s);
  assert.match(bar, /已断开/);
  assert.match(bar, /#dc2626/);
  assert.match(bar, /data-xbt="reconnect"/);
  assert.match(s.closeReason, /5 次/);

  h.T.reconnect(s);
  assert.equal(FakeWS.all.length, 7);
  FakeWS.all.at(-1).open();
  assert.equal(s.conn, "open");
  assert.equal(s.attempt, 0);
  assert.match(text(), /新的会话/);
});

test("successful retry resets the counter and marks the session as new", async () => {
  const h = setup();
  const { s, cb, text } = session();
  h.T.connect(s, grant, cb);
  FakeWS.all[0].open();
  FakeWS.all[0].drop(1006);
  await h.runTimer();
  FakeWS.all[1].open();
  assert.equal(s.conn, "open");
  assert.equal(s.attempt, 0);
  assert.equal(s.reconnected, true);
  assert.match(text(), /已重新连接/);
  assert.match(h.T.statusBar(s), /新会话/);
});

test("no reconnect after server 'closed', fatal error, auth codes, never-opened, or manual disconnect", async () => {
  for (const scenario of ["closed", "fatal", "auth", "never", "manual", "normal"]) {
    const h = setup();
    const { s, cb } = session();
    h.T.connect(s, grant, cb);
    const ws = FakeWS.all[0];
    if (scenario !== "never") ws.open();
    if (scenario === "closed") { ws.msg({ type: "closed", data: "session ended" }); ws.drop(1006); }
    if (scenario === "fatal") { ws.msg({ type: "error", data: "SSH handshake, authentication or host key verification failed" }); ws.msg({ type: "closed", data: "session ended" }); ws.drop(1006); }
    if (scenario === "auth") ws.drop(4003);
    if (scenario === "never") ws.drop(1006);
    if (scenario === "normal") ws.drop(1000);
    if (scenario === "manual") { h.T.dispose(s); assert.equal(ws.closedWith, 1000); ws.drop(1000); }
    assert.equal(h.timers.filter((t) => t.fn).length, 0, scenario + ": no retry scheduled");
    assert.equal(FakeWS.all.length, 1, scenario);
    assert.ok(s.conn === "closed" || s.conn === "failed", scenario + " -> " + s.conn);
    if (scenario === "fatal") { assert.equal(s.conn, "failed"); assert.match(s.err, /握手、认证或主机指纹/); }
    if (scenario === "closed") { assert.equal(s.conn, "closed"); assert.match(h.T.statusBar(s), /#6b7280/); }
  }
});

test("retryable server error (instance shutdown) still reconnects", () => {
  const h = setup();
  const { s, cb } = session();
  h.T.connect(s, grant, cb);
  FakeWS.all[0].open();
  FakeWS.all[0].msg({ type: "error", data: "owner is shutting down" });
  FakeWS.all[0].drop(1006);
  assert.equal(s.conn, "reconnecting");
});

test("a session killed by an administrator is not re-opened", async () => {
  const h = setup({ get: async (p) => { assert.equal(p, "/bastion/sessions/501"); return { status: "killed" }; } });
  const { s, cb, text } = session();
  h.T.connect(s, grant, cb);
  FakeWS.all[0].msg({ type: "session", data: "501" });
  FakeWS.all[0].open();
  FakeWS.all[0].drop(1006);
  await h.runTimer();
  assert.equal(FakeWS.all.length, 1);
  assert.equal(s.conn, "closed");
  assert.match(text(), /管理员终止/);
});

test("dispose cancels a pending retry", async () => {
  const h = setup();
  const { s, cb } = session();
  h.T.connect(s, grant, cb);
  FakeWS.all[0].open();
  FakeWS.all[0].drop(1006);
  h.T.dispose(s);
  assert.equal(await h.runTimer(), null);
  assert.equal(FakeWS.all.length, 1);
});

test("resize: rows/cols follow the output size and are sent only when changed", () => {
  const h = setup();
  const z = h.T.fitSize(800, 400, 8, 20);
  assert.deepEqual({ ...z }, { cols: 98, rows: 19 });
  const { s, cb } = session();
  h.T.connect(s, grant, cb);
  const ws = FakeWS.all[0]; ws.open();
  const out = new Element("pre"); out.clientWidth = 1000; out.clientHeight = 600;
  assert.equal(h.T.fit(s, out), true);
  const m = ws.sent.at(-1);
  assert.equal(m.type, "resize");
  assert.ok(m.cols > 40 && m.rows > 10);
  assert.equal(h.T.fit(s, out), false, "unchanged size is not re-sent");
  assert.equal(ws.sent.length, 1);
});

test("toolbar: font size persists, disconnect asks for confirmation", async () => {
  const h = setup();
  const { s, cb } = session();
  h.T.connect(s, grant, cb);
  FakeWS.all[0].open();
  const el = new Element("main");
  el.innerHTML = '<div class="x-bwrap">' + h.T.toolbar(s) + h.T.statusBar(s) + '<pre class="x-lterm-out"></pre></div>';
  for (const label of ["复制选中", "A−", "A+", "全屏", "断开", "清屏"]) assert.ok(el.innerHTML.includes(label), label);
  let closed = 0;
  h.T.bind(el, s, { close: () => closed++ });
  const out = el.querySelector(".x-lterm-out");
  assert.equal(out.style.fontSize, "13px");
  el.querySelector('[data-xbt="fs+"]').onclick();
  assert.equal(out.style.fontSize, "14px");
  assert.equal(h.store.get("xs.bastion.term.fs"), "14");
  el.querySelector('[data-xbt="fs-"]').onclick(); el.querySelector('[data-xbt="fs-"]').onclick();
  assert.equal(out.style.fontSize, "12px");

  el.querySelector('[data-xbt="copy"]').onclick();
  assert.match(h.toasts.at(-1), /选中/);

  el.querySelector('[data-xbt="off"]').onclick();
  assert.equal(closed, 0, "not closed before confirming");
  const dlg = h.body.children.at(-1);
  assert.match(dlg.innerHTML, /断开会话/);
  dlg.querySelector("[data-yes]").onclick();
  await tick();
  assert.equal(closed, 1);
  assert.equal(FakeWS.all[0].closedWith, 1000);
  assert.equal(s.manual, true);
});

test("recent assets float to the top and drive the four-colour status", () => {
  const h = setup();
  const R = h.B.recent;
  const assets = [{ id: 1, name: "a" }, { id: 2, name: "b" }, { id: 3, name: "c" }];
  R.touch(3);
  assert.deepEqual(R.sort(assets, (a) => a.id).map((a) => a.id), [3, 1, 2]);
  R.touch(2, true);
  assert.deepEqual(R.sort(assets, (a) => a.id).map((a) => a.id), [2, 3, 1]);
  const stored = JSON.parse(h.store.get("xs.bastion.recent"));
  assert.ok(stored.every((x) => Object.keys(x).every((k) => ["id", "t", "ok", "c"].includes(k))), "only id/time/result stored");

  const H = h.B.assetHealth;
  assert.equal(H({ id: 9 }, [], []).level, "unknown");
  assert.equal(H({ id: 9 }, [], [{ asset_id: 9, status: "active" }]).level, "ok");
  assert.equal(H({ id: 9 }, [{ verify_status: "ok", verified_at: new Date().toISOString() }, { verify_status: "failed", verified_at: new Date().toISOString() }], []).level, "warn");
  assert.equal(H({ id: 9 }, [{ verify_status: "failed", verified_at: new Date().toISOString() }], []).level, "bad");
  assert.equal(H({ id: 2 }, [], []).level, "ok", "local successful connection");
  assert.equal(H({ id: 9 }, [{ verify_status: "ok", verified_at: new Date(Date.now() - 30 * 864e5).toISOString() }], []).level, "warn", "stale result");
  assert.match(h.B.dot("bad", "不可达"), /#dc2626/);
});
