import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;

function load({ mode = "live", api } = {}) {
  const calls = [];
  const window = { XS: { mode, api: async (path, opts = {}) => { calls.push({ path, opts }); return api(path, opts); } } };
  const ctx = vm.createContext({ window, console, Promise, Date, Math, Number, String, Array, Object, isFinite });
  vm.runInContext(read("../../public/xs-empty.js"), ctx);
  vm.runInContext(read("../../public/xs-dora.js"), ctx);
  const D = window.XSDora;
  D._state.days = 30; D._state.app_id = ""; D._state.env = "";
  return { window, D, calls };
}

const REPORT = {
  window_days: 7,
  deploy_frequency: { per_day: 0.286, total: 2, series: [["2026-11-08", 2], ["2026-11-09", 0], ["2026-11-10", 0], ["2026-11-11", 0], ["2026-11-12", 0], ["2026-11-13", 0], ["2026-11-14", 0]] },
  lead_time: { median_hours: 13, p90_hours: 21.8, samples: 2, basis: "run" },
  change_failure_rate: { rate: 0.6667, failed: 2, total: 3 },
  mttr: { median_hours: 3, samples: 2, available: true },
  level: { deploy_frequency: "high", lead_time: "elite", change_failure_rate: "low", mttr: "bogus" },
};

test("pure helpers: level names, hours, frequency", () => {
  const { D } = load({ api: async () => ({}) });
  assert.equal(D.levelText("elite"), "精英");
  assert.equal(D.levelText("high"), "高");
  assert.equal(D.levelText("medium"), "中");
  assert.equal(D.levelText("low"), "低");
  assert.equal(D.levelText("unknown"), "数据不足");
  assert.equal(D.levelText("x"), "数据不足");
  assert.equal(D.fmtHours(null), "—");
  assert.equal(D.fmtHours(0.5), "30 分钟");
  assert.equal(D.fmtHours(13), "13 小时");
  assert.equal(D.fmtHours(72), "3 天");
  assert.equal(D.perText(2, 60), "每天 2 次");
  assert.equal(D.perText(0.286, 2), "每周 2 次");
  assert.equal(D.perText(0.05, 1), "每月 1.5 次");
  assert.equal(D.perText(0, 0), "0 次");
});

test("chart is inline SVG with one bar per day and escaped labels", () => {
  const { D } = load({ api: async () => ({}) });
  const e = new Element("div");
  e.innerHTML = D.chart([["2026-11-01", 3], [XSS, 1], ["2026-11-03", 0]]);
  const svg = e.querySelector("svg");
  assert.ok(svg);
  assert.equal(e.querySelectorAll("rect").length, 3);
  assert.ok(!e.innerHTML.includes("<img"));
  assert.equal(D.chart([]), "");
  assert.ok(!/https?:\/\/(?!www\.w3\.org)|Gradient|<filter/.test(read("../../public/xs-dora.js")));
});

test("live: renders four metric cards with level badges, basis note and chart", async () => {
  const { D, calls } = load({ api: async (path) => (path === "/cicd/applications" ? { items: [{ id: 5, name: XSS, envs: [{ name: "prod" }, { name: "test" }] }] } : REPORT) });
  const el = new Element("main");
  await D.render(el); await tick();
  const dora = calls.find((c) => c.path === "/devcenter/dora");
  assert.ok(dora);
  assert.equal(dora.opts.query.days, 30);
  const cards = el.querySelectorAll("[data-metric]");
  assert.equal(cards.length, 4);
  const badge = (k) => el.querySelectorAll("[data-metric]").find((n) => n.getAttribute("data-metric") === k).querySelector("[data-level]");
  assert.equal(badge("deploy_frequency").getAttribute("data-level"), "high");
  assert.equal(text(badge("lead_time")), "精英");
  assert.equal(text(badge("change_failure_rate")), "低");
  assert.equal(text(badge("mttr")), "数据不足");
  const t = text(el);
  assert.ok(t.includes("交付效能") && t.includes("每周 2 次") && t.includes("66.7%") && t.includes("构建开始时间估算"));
  assert.ok(el.querySelector("svg"));
  assert.ok(!el.innerHTML.includes("<img"), "app name escaped");
  assert.ok(!t.includes("/devcenter"), "no api path in UI");
});

test("filters re-query with days/app/env; changing app resets env", async () => {
  const { D, calls } = load({ api: async (path) => (path === "/cicd/applications" ? [{ id: 5, name: "订单", envs: [{ name: "prod" }] }] : REPORT) });
  const el = new Element("main");
  await D.render(el); await tick();
  const fire = async (sel, v) => { const n = el.querySelectorAll("select").find((s) => s.getAttribute("data-dora") === sel); n.value = v; for (const fn of el.listeners.change) await fn({ target: n }); await tick(); };
  await fire("days", "7");
  await fire("app", "5");
  await fire("env", "prod");
  const q = calls.filter((c) => c.path === "/devcenter/dora").at(-1).opts.query;
  assert.equal(q.days, 7); assert.equal(q.app_id, "5"); assert.equal(q.env, "prod");
  await fire("app", "");
  const q2 = calls.filter((c) => c.path === "/devcenter/dora").at(-1).opts.query;
  assert.equal(q2.app_id, undefined); assert.equal(q2.env, undefined);
});

test("empty state when the window has no releases", async () => {
  const { D } = load({ api: async (path) => (path === "/cicd/applications" ? { items: [] } : { window_days: 30, deploy_frequency: { per_day: 0, total: 0, series: [] }, lead_time: { samples: 0, basis: "none" }, change_failure_rate: { rate: null, failed: 0, total: 0 }, mttr: { samples: 0, available: false }, level: {} }) });
  const el = new Element("main");
  await D.render(el); await tick();
  assert.ok(el.querySelector("[data-xse]"));
  assert.equal(el.querySelectorAll("[data-metric]").length, 0);
  assert.ok(text(el).includes("还没有发布记录"));
});

test("errors are shown in Chinese; 403 explains permission", async () => {
  const { D } = load({ api: async (path) => { if (path === "/cicd/applications") return []; const e = new Error("forbidden"); e.status = 403; throw e; } });
  const el = new Element("main");
  await D.render(el); await tick();
  assert.ok(text(el).includes("没有权限查看交付效能"));
});

test("demo mode uses demo data and never calls the API", async () => {
  const { D, calls } = load({ mode: "demo", api: async () => { throw new Error("should not call"); } });
  const el = new Element("main");
  await D.render(el); await tick();
  assert.equal(calls.length, 0);
  assert.equal(el.querySelectorAll("[data-metric]").length, 4);
  assert.ok(text(el).includes("演示数据"));
  const d = D.demo(7);
  assert.equal(d.deploy_frequency.series.length, 7);
});
