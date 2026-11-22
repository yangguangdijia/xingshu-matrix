import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>`;
const STEPS = [{ weight: 10, pause_sec: 300, manual: false }, { weight: 30, pause_sec: 300, manual: false }, { weight: 50, pause_sec: 0, manual: true }, { weight: 100, pause_sec: 0, manual: true }];
const APP = { id: 3, name: "web", envs: [{ name: "prod", namespace: "web", deployment: "web", container: "web", target_id: 2 }] };
const NOW = Math.floor(Date.now() / 1000);
const canary = (o = {}) => ({ id: 7, app_id: 3, env: "prod", image: "reg/web:v2", stable_image: "reg/web:v1", namespace: "web", service: "web", ingress: "web",
  deployment: "web", canary_deployment: "web-canary", canary_ingress: "web-canary", plan: { steps: STEPS, replicas: 1, auto_promote: false },
  status: "progressing", current_step: 1, weight: 30, pending_action: "", created_by: "alice", created_at: new Date((NOW - 1800) * 1000).toISOString(), finished_at: null,
  events: [{ id: 1, kind: "step", message: "第 1 步（权重 10%）观察中", ts: new Date((NOW - 1700) * 1000).toISOString() },
    { id: 2, kind: "step", message: "第 2 步（权重 30%）观察中", ts: new Date((NOW - 900) * 1000).toISOString() }], ...o });
const COMPARE = (o = {}) => ({ canary_id: 7, status: "progressing", final: false, weight: 30, current_step: 1, total_steps: 4, done_steps: 1, start: NOW - 1800, end: NOW,
  weight_series: [[NOW - 1800, 0], [NOW - 1700, 10], [NOW - 900, 30], [NOW, 30]],
  steps: [{ index: 0, weight: 10, state: "done" }, { index: 1, weight: 30, state: "current" }, { index: 2, weight: 50, state: "pending" }, { index: 3, weight: 100, state: "pending" }],
  stable: { workload: "web", ingress: "web", image: "reg/web:v1", weight: 70 }, canary: { workload: "web-canary", ingress: "web-canary", image: "reg/web:v2", weight: 30 },
  metrics_target: { cluster_id: 9, namespace: "web", workload_kind: "deployments", stable_workload: "web", canary_workload: "web-canary", stable_ingress: "web", canary_ingress: "web-canary" }, ...o });
const matrix = (pts) => ({ resultType: "matrix", result: [{ metric: {}, values: pts.map(([t, v]) => [t, String(v)]) }] });
const series = (v) => [[NOW - 600, v], [NOW - 300, v], [NOW, v]];

function promAnswer(query, { canaryErr = 0.12 } = {}) {
  const canarySide = query.includes('ingress="web-canary"');
  if (query.startsWith("histogram_quantile")) return matrix(series(canarySide ? 0.19 : 0.18));
  if (query.includes('status=~"5.."')) return matrix(series(canarySide ? canaryErr : 0.3));
  return matrix(series(canarySide ? 30 : 70));
}

function setup({ perms = ["cicd:read", "cicd:deploy", "datasource:read", "metrics:read", "workload:read"], compare = () => COMPARE(), scope = "cluster", mode, prom = promAnswer, spark } = {}) {
  const h = harness("xs-release-canary.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (path === "/cicd/applications/3") return APP;
      if (path === "/cicd/applications/3/canaries" && !opts.method) return [canary()];
      if (path === "/cicd/canaries/7" && !opts.method) return canary();
      if (path === "/cicd/canaries/7/compare") return compare();
      if (path === "/observability/datasources/resolve") return { datasource: { id: 5, type: "prometheus" }, cluster_id: 9, scope };
      if (path === "/observability/prometheus/5/query_range") return { status: "success", data: prom(opts.query.query) };
      if (path === "/observability/metrics/sparklines") return spark ? spark(opts) : { source: "none", items: [] };
      if (/^\/cicd\/canaries\/7\/\w+$/.test(path) && opts.method === "POST") return canary();
      throw new Error("unexpected " + path);
    }
  });
  if (mode) h.window.XS.mode = mode;
  const toasts = [];
  h.window.XS.toast = (m) => toasts.push(m);
  return { ...h, toasts };
}
async function open(h) {
  h.window.XSReleaseCanary.render(h.el, { appId: 3, env: "prod" });
  for (let i = 0; i < 4; i++) await tick();
  await h.emit("click", '[data-xrc-click="open"]');
  for (let i = 0; i < 8; i++) await tick();
}
const card = (h) => h.el.querySelector("[data-xrc-compare]");
const paths = (h, p) => h.calls.filter((c) => c.path === p);

test("frontend compare path matches the Go handler and route patch", () => {
  const go = read("../../server/internal/modules/cicd/canary_compare.go");
  const src = read("../../public/xs-release-canary.js");
  assert.match(go, /func \(m \*Module\) canaryCompare\(c \*gin\.Context\)/);
  assert.match(go, /GET \/cicd\/canaries\/:id\/compare/);
  assert.ok(src.includes('"/compare"'));
  assert.ok(!/https?:\/\//.test(src.replace(/http:\/\/www\.w3\.org\/2000\/svg/g, "")), "no CDN / external urls");
});

test("traffic metrics: split bar, step pips, side-by-side rows with SVG lines and a green verdict", async () => {
  const h = setup();
  await open(h);
  const c = card(h);
  assert.ok(c, "compare card rendered");
  const t = text(c);
  assert.match(t, /主版本 70%/); assert.match(t, /金丝雀 30%/);
  assert.match(t, /已完成 1 \/ 4 步/);
  assert.equal(c.querySelectorAll("li").filter((n) => n.attrs.class === "is-current").length, 1);
  for (const k of ["rps", "err", "p95"]) assert.ok(c.querySelector(`[data-xrc-metric="${k}"]`), k);
  assert.match(t, /70\.0 次\/秒/); assert.match(t, /30\.0 次\/秒/);
  assert.match(t, /180 毫秒|180\.0 毫秒/); assert.match(t, /190 毫秒|190\.0 毫秒/);
  assert.ok(c.querySelectorAll("svg").length >= 7, "weight + 6 metric sparklines");
  assert.ok(c.querySelectorAll("path").length >= 7);
  assert.match(text(c.querySelector("[data-xrc-verdict]")), /可按计划推进/);
  // queries carry cluster scope, both ingresses and a lifetime window
  const qs = paths(h, "/observability/prometheus/5/query_range");
  assert.equal(qs.length, 6);
  assert.ok(qs.every((q) => q.options.query.cluster_id === 9 && Number(q.options.query.start) <= NOW - 1800 + 1));
  assert.ok(qs.some((q) => q.options.query.query.includes('ingress="web-canary"')) && qs.some((q) => q.options.query.query.includes('ingress="web"')));
  assert.equal(paths(h, "/observability/metrics/sparklines").length, 0);
  // actions keep existing behaviour: advance button present, rollback still needs confirmation
  assert.ok(h.el.querySelector('[data-act="advance"]'));
  await h.emit("click", '[data-act="rollback"]');
  assert.ok(h.el.querySelector('[role="alertdialog"]'));
  assert.equal(h.calls.filter((x) => x.options.method === "POST").length, 0);
});

test("canary error rate clearly above stable turns the verdict red", async () => {
  const h = setup({ prom: (q) => promAnswer(q, { canaryErr: 1.5 }) });
  await open(h);
  const v = card(h).querySelector("[data-xrc-verdict]");
  assert.match(text(v), /建议回滚/);
  assert.match(v.getAttribute("style"), /#dc2626/);
});

test("central datasource falls back to CPU / memory per side", async () => {
  const spark = () => ({ source: "prometheus", start: NOW - 1800, end: NOW, items: [
    { name: "web", cpu: series(0.5), mem: series(268435456) }, { name: "web-canary", cpu: series(0.2), mem: series(134217728) }] });
  const h = setup({ scope: "central", spark });
  await open(h);
  assert.equal(paths(h, "/observability/prometheus/5/query_range").length, 0);
  const sp = paths(h, "/observability/metrics/sparklines")[0];
  assert.equal(sp.options.query.names, "web,web-canary");
  assert.equal(sp.options.query.kind, "workload");
  const c = card(h);
  assert.ok(c.querySelector('[data-xrc-metric="cpu"]') && c.querySelector('[data-xrc-metric="mem"]'));
  assert.ok(!c.querySelector('[data-xrc-metric="rps"]'));
  assert.match(text(c), /256 MiB/); assert.match(text(c), /128 MiB/);
  assert.ok(!c.querySelector("[data-xrc-verdict]"));
});

test("no metrics: only weight and steps with a one-line note", async () => {
  const h = setup({ compare: () => COMPARE({ metrics_target: null, note: "执行目标未关联已注册集群，暂无两侧监控数据" }) });
  await open(h);
  const c = card(h);
  assert.ok(c.querySelector(".xrc-split"));
  assert.match(text(c.querySelector("[data-xrc-cmp-note]")), /未关联已注册集群/);
  assert.equal(c.querySelectorAll(".xrc-mrow").length, 0);
  assert.equal(h.calls.filter((x) => x.path.startsWith("/observability")).length, 0);
});

test("prometheus without traffic and no resource data degrades to the note", async () => {
  const h = setup({ prom: () => matrix([]) });
  await open(h);
  assert.match(text(card(h).querySelector("[data-xrc-cmp-note]")), /未读取到两侧监控数据/);
});

test("compare endpoint failure falls back to the detail itself without an error banner", async () => {
  const h = setup({ compare: () => { throw Object.assign(new Error("boom"), { status: 500 }); } });
  await open(h);
  assert.ok(!h.el.querySelector('[role="alert"]'));
  const c = card(h);
  assert.match(text(c), /主版本 70%/);
  assert.match(text(c), /已完成 1 \/ 4 步/);
  assert.ok(c.querySelector("[data-xrc-cmp-note]"));
  const f = h.window.XSReleaseCanary._compareFromDetail(canary({ status: "rolled_back", weight: 0, finished_at: new Date(NOW * 1000).toISOString(),
    events: [...canary().events, { kind: "step", message: "已回滚：金丝雀流量归零", ts: new Date((NOW - 10) * 1000).toISOString() }] }), NOW + 100);
  assert.equal(f.steps[1].state, "stopped");
  assert.equal(JSON.stringify(f.weight_series.slice(-2)), JSON.stringify([[NOW - 10, 0], [NOW, 0]]));
});

test("metrics are throttled while the detail keeps polling", async () => {
  const h = setup();
  await open(h);
  assert.equal(paths(h, "/observability/prometheus/5/query_range").length, 6);
  await h.runTimer(); for (let i = 0; i < 6; i++) await tick();
  assert.equal(paths(h, "/cicd/canaries/7/compare").length, 2, "compare refreshed with the detail");
  assert.equal(paths(h, "/observability/prometheus/5/query_range").length, 6, "metrics not re-queried within a minute");
});

test("missing observability permissions skip queries", async () => {
  const h = setup({ perms: ["cicd:read"] });
  await open(h);
  assert.equal(h.calls.filter((x) => x.path.startsWith("/observability")).length, 0);
  assert.ok(card(h).querySelector("[data-xrc-cmp-note]"));
  assert.ok(!h.el.querySelector('[data-act="advance"]'), "no deploy permission, no actions");
});

test("demo mode renders demo comparison and never calls the API", async () => {
  const h = harness("xs-release-canary.js", { permissions: ["cicd:read", "cicd:deploy"], api: async (p) => { throw new Error("should not call " + p); } });
  h.window.XS.mode = "demo";
  const toasts = []; h.window.XS.toast = (m) => toasts.push(m);
  h.window.XSReleaseCanary.render(h.el, { appId: 3, env: "prod" });
  for (let i = 0; i < 4; i++) await tick();
  assert.equal(h.calls.length, 0);
  await h.emit("click", '[data-xrc-click="open"]');
  for (let i = 0; i < 6; i++) await tick();
  const c = card(h);
  assert.ok(c);
  assert.match(text(c), /金丝雀 30%/);
  assert.ok(c.querySelector('[data-xrc-metric="p95"]'));
  assert.match(text(c), /演示数据/);
  await h.emit("click", '[data-act="advance"]');
  assert.equal(h.calls.length, 0);
  assert.ok(toasts.some((m) => /演示模式/.test(m)));
});

test("helpers escape names and parse prometheus envelopes", () => {
  const R = harness("xs-release-canary.js").window.XSReleaseCanary;
  const html = R._compareHTML(COMPARE({ stable: { workload: XSS, weight: 70 }, canary: { workload: "c", weight: 30 } }),
    { kind: "resource", stable: { cpu: series(1), mem: [] }, canary: { cpu: [], mem: [] }, caption: XSS });
  assert.ok(!html.includes("<img"));
  assert.equal(JSON.stringify(R._promPoints({ data: { result: [{ values: [[1, "2"], [2, "NaN"]] }, { values: [[1, "3"]] }] } })), "[[1,5]]");
  assert.equal(JSON.stringify(R._promPoints({ result: [] })), "[]");
  assert.match(R._trafficQueries('ns"x', "web", 120).rps, /namespace="ns\\"x"/);
  assert.equal(R._verdict(null), null);
  assert.match(R._sparkSVG([], { x0: 0, x1: 1 }), /<svg/);
});
