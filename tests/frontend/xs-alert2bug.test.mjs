import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const plain = (v) => JSON.parse(JSON.stringify(v));
const all = (node) => [node, ...node.children.flatMap(all)];
const text = (node) => all(node).map((n) => n.textContent).join("");

function setup({ mode = "live", perms = ["devcenter:manage"], api, hash = "#/observe/events" } = {}) {
  const calls = [], toasts = [], winListeners = {}, timers = [];
  const body = new Element("body");
  const document = {
    body,
    createElement: (t) => new Element(t),
    querySelector: (s) => body.querySelector(s),
    addEventListener() {}, removeEventListener() {}
  };
  const opened = [];
  const window = {
    document,
    location: { hash },
    addEventListener: (t, fn) => { (winListeners[t] ||= []).push(fn); },
    removeEventListener: (t, fn) => { winListeners[t] = (winListeners[t] || []).filter((f) => f !== fn); },
    setTimeout: (fn) => { timers.push(fn); return timers.length; },
    XS: {
      mode, can: (p) => perms.includes(p), toast: (m) => toasts.push(m),
      api: async (path, options = {}) => { calls.push({ path, options }); return api ? api(path, options) : {}; }
    },
    XSDev: { openDetail: (k, id) => opened.push([k, id]) }
  };
  vm.runInContext(read("../../public/xs-alert2bug.js"), vm.createContext({ window, console }));
  const fireHash = () => { for (const fn of [...(winListeners.hashchange || [])]) fn(); const due = timers.splice(0); due.forEach((fn) => fn()); };
  return { window, body, calls, toasts, opened, fireHash, A: window.XSAlert2Bug };
}

const PREVIEW = {
  preview: true, title: "[告警] KubePodCrashLooping · prod-sh/shop", existing: false,
  suspect_releases: [{ release_id: 9, app_name: XSS, env: "prod", image: "r/order:v2", minutes_before: 95, reasons: ["应用名匹配（pod=order-service）"] }]
};

test("frontend path matches backend registration and contract", () => {
  const go = read("../../server/internal/modules/devcenter/alert_bug.go");
  const src = read("../../public/xs-alert2bug.js");
  assert.ok(go.includes('g.POST("/bugs/from-alert"'));
  assert.ok(src.includes('"/devcenter/bugs/from-alert"'));
  assert.ok(src.includes("devcenter:manage"));
});

test("button is gated by devcenter:manage in live mode and escapes ids", () => {
  let h = setup({ perms: [] });
  assert.equal(h.A.buttonHTML({ id: 5 }), "");
  h = setup();
  const html = h.A.buttonHTML({ id: 5 });
  assert.match(html, /data-xa2b="5"/);
  assert.match(html, /转为缺陷/);
  assert.equal(h.A.buttonHTML({ fingerprint: "abc" }), "", "no platform id → no button");
  assert.equal(h.A.buttonHTML({ id: XSS }), "", "live mode needs numeric alert id");
  const demo = setup({ mode: "demo", perms: [] });
  assert.ok(!demo.A.buttonHTML({ id: XSS }).includes("<img"), "escaped in demo mode");
  assert.match(demo.A.buttonHTML({ id: "e1" }, { cls: "x-act" }), /class="x-act"/);
});

test("live: confirm dialog shows suspect releases, then creates bug and links to it", async () => {
  const h = setup({
    api: (path, o) => {
      assert.equal(path, "/devcenter/bugs/from-alert");
      assert.equal(o.method, "POST");
      if (o.body.dry_run) return PREVIEW;
      return { id: 77, title: PREVIEW.title, existing: false, suspect_releases: PREVIEW.suspect_releases };
    }
  });
  const p = h.A.open({ id: 42 });
  await tick(); await tick();
  const dlg = h.body.querySelector(".xa2b-confirm");
  assert.ok(dlg, "confirm dialog opened");
  assert.deepEqual(plain(h.calls[0].options.body), { alert_id: 42, dry_run: true });
  const t = text(dlg);
  assert.match(t, /KubePodCrashLooping · prod-sh\/shop/);
  assert.match(t, /可疑发布（告警前 24 小时，1 条）/);
  assert.match(t, /告警1 小时 35 分钟前/);
  assert.ok(!dlg.innerHTML.includes("<img src=x"), "release fields escaped");
  assert.equal(h.calls.length, 1, "nothing created before confirmation");
  dlg.querySelector("[data-xa2b-yes]").onclick();
  const bug = await p;
  assert.equal(bug.id, 77);
  assert.deepEqual(plain(h.calls[1].options.body), { alert_id: 42 });
  assert.equal(h.body.querySelector(".xa2b-confirm"), null, "dialog closed");
  const toastEl = h.body.querySelector(".xa2b-toast");
  assert.match(text(toastEl), /已创建缺陷 BUG-77/);
  toastEl.querySelector("[data-xa2b-view]").onclick();
  assert.equal(h.window.location.hash, "#/bugs");
  h.fireHash();
  assert.deepEqual(h.opened, [["bugs", 77]], "bug drawer opened after route change");
});

test("live: existing bug is shown instead of creating a duplicate", async () => {
  const h = setup({ hash: "#/bugs", api: () => ({ ...PREVIEW, existing: true, bug: { id: 12, title: "旧缺陷" } }) });
  await h.A.open({ id: 42 });
  const dlg = h.body.querySelector(".xa2b-confirm");
  assert.match(text(dlg), /已转为缺陷/);
  assert.match(text(dlg), /BUG-12/);
  assert.equal(dlg.querySelector("[data-xa2b-yes]"), null);
  dlg.querySelector("[data-xa2b-view]").onclick();
  assert.deepEqual(h.opened, [["bugs", 12]], "already on #/bugs → open drawer directly");
  assert.equal(h.calls.length, 1);
});

test("live: errors are shown in Chinese and keep the dialog open", async () => {
  let fail = false;
  const h = setup({ api: (p, o) => { if (o.body.dry_run) return { ...PREVIEW, suspect_releases: [] }; fail = true; throw Object.assign(new Error("forbidden"), { status: 403 }); } });
  const p = h.A.open({ id: 3 });
  await tick(); await tick();
  const dlg = h.body.querySelector(".xa2b-confirm");
  assert.match(text(dlg), /未发现同应用、同命名空间的发布记录/);
  dlg.querySelector("[data-xa2b-yes]").onclick();
  await tick(); await tick();
  assert.ok(fail);
  const err = dlg.querySelector("[data-xa2b-err]");
  assert.equal(err.textContent, "没有研发中心的管理权限");
  assert.equal(err.hidden, false);
  assert.equal(dlg.querySelector("[data-xa2b-yes]").disabled, false);
  void p;
});

test("live: preview failure shows error", async () => {
  const h = setup({ api: () => { throw Object.assign(new Error("not found"), { status: 404 }); } });
  await h.A.open({ id: 3 });
  assert.match(text(h.body.querySelector(".xa2b-confirm")), /告警不存在或已被清理/);
});

test("demo mode works without requests and is idempotent", async () => {
  const h = setup({ mode: "demo", perms: [] });
  const alert = { id: "e1", rule: "HighErrorRate", app: "api-gateway", env: "prod" };
  const p = h.A.open(alert);
  await tick(); await tick();
  const dlg = h.body.querySelector(".xa2b-confirm");
  assert.match(text(dlg), /\[告警\] HighErrorRate · prod-shanghai\/prod/);
  assert.match(text(dlg), /api-gateway · prod/);
  dlg.querySelector("[data-xa2b-yes]").onclick();
  const bug = await p;
  assert.equal(h.calls.length, 0, "demo mode never calls the API");
  assert.equal(bug.existing, false);
  const again = await h.A.convert(alert);
  assert.equal(again.id, bug.id);
  assert.equal(again.existing, true);
  const pv = await h.A.preview(alert);
  assert.equal(pv.existing, true);
});

test("bind / mount / mountIncident attach buttons", async () => {
  const h = setup({ api: (path, o) => {
    if (path === "/oncall/incidents/9/alerts") return { items: [{ id: 51, alertname: "X" }] };
    if (path === "/oncall/incidents/10/alerts") return { items: [] };
    if (o.body && o.body.dry_run) return PREVIEW;
    return { id: 1 };
  } });
  // 列表行
  const box = new Element("div");
  box.innerHTML = h.A.buttonHTML({ id: 5 }) + h.A.buttonHTML({ id: 6 });
  h.A.bind(box, (id) => ({ id: +id, alertname: "A" + id }));
  box.querySelectorAll("[data-xa2b]")[1].onclick({ stopPropagation() {} });
  await tick(); await tick();
  assert.deepEqual(plain(h.calls.at(-1).options.body), { alert_id: 6, dry_run: true });
  // 告警详情弹窗
  const modal = new Element("div");
  modal.innerHTML = '<div class="x-wbody"></div><div class="x-dfoot"><button class="x-btn" data-close>关闭</button></div>';
  const btn = h.A.mount(modal, { id: 42 });
  assert.ok(btn);
  assert.equal(modal.querySelector(".x-dfoot").querySelector("[data-xa2b]").getAttribute("data-xa2b"), "42");
  assert.equal(h.A.mount(modal, { id: 42 }), btn, "mount is idempotent");
  const noFoot = new Element("div");
  assert.equal(h.A.mount(noFoot, { id: 42 }), null);
  // 故障详情
  const slot = new Element("span");
  const b = await h.A.mountIncident(slot, 9);
  assert.ok(b);
  assert.equal(b.getAttribute("data-xa2b"), "51");
  const empty = new Element("span");
  assert.equal(await h.A.mountIncident(empty, 10), null);
  assert.equal(empty.children.length, 0);
  const denied = setup({ perms: [] });
  assert.equal(await denied.A.mountIncident(new Element("span"), 9), null);
  assert.equal(denied.calls.length, 0);
});
