// 研发中心工作台「我的交付」（public/xs-devcenter.js 的 workbench）
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { tick, Element } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const settle = async () => { for (let i = 0; i < 8; i++) await tick(); };

function loadEmpty() {
  const window = {};
  vm.runInContext(read("../../public/xs-empty.js"), vm.createContext({ window }));
  return window.XSEmpty;
}
const XE = loadEmpty();

const hoursAgo = (h) => new Date(Date.now() - h * 3600000).toISOString();
const st = (a) => Object.fromEntries(["code", "build", "artifact", "release", "env"].map((k, i) => [k, a[i]]));

function sample() {
  return {
    requirements: [
      { id: 11, title: "普通需求", status: "doing", priority: "P2", stage: "code", stage_state: "running", stages: st(["running", "none", "none", "none", "none"]), env_latest: "", bucket: "dev", updated_at: hoursAgo(1) },
      { id: 12, title: "受阻需求", status: "doing", priority: "P1", stage: "build", stage_state: "failed", stages: st(["done", "failed", "done", "none", "none"]), env_latest: "", blocked: true, blocked_reason: "最近一次构建失败", bucket: "blocked", updated_at: hoursAgo(5) },
      { id: 13, title: "上线需求", status: "done", priority: "P2", stage: "env", stage_state: "done", stages: st(["done", "done", "done", "done", "done"]), env_latest: "prod", released_at: hoursAgo(30), bucket: "released", updated_at: hoursAgo(30) },
      { id: 14, title: "很早上线", status: "done", priority: "P3", stage: "env", stage_state: "done", stages: st(["done", "done", "done", "done", "done"]), env_latest: "prod", released_at: hoursAgo(24 * 10), bucket: "released", updated_at: hoursAgo(240) }
    ],
    bugs: [
      { id: 3, title: "构建中缺陷", status: "doing", priority: "P1", stage: "artifact", stage_state: "done", stages: st(["done", "done", "done", "none", "none"]), env_latest: "", bucket: "build", updated_at: hoursAgo(2) }
    ],
    reviews: [],
    counts: { in_dev: 1, in_build: 1, released: 2, blocked: 1 }
  };
}

function mk({ mode = "live", delivery = sample(), fail = null, withEmpty = true } = {}) {
  const calls = [], toasts = [];
  const body = new Element("body"), head = new Element("head");
  const document = { createElement: (t) => new Element(t), getElementById: () => null, head, body, documentElement: body, addEventListener() {}, removeEventListener() {}, querySelector: () => null };
  const window = {
    XS: {
      mode, user: { username: "alice", display_name: "Alice" }, toast: (m) => toasts.push(m),
      get: async (p, q) => { calls.push([p, q]); if (p === "/devcenter/my/delivery") { if (fail) throw fail; return delivery; } return { items: [], total: 0 }; },
      post: async () => ({}), patch: async () => ({}), del: async () => ({})
    },
    XSFormKit: { open: () => ({}) }
  };
  window.window = window;
  if (withEmpty) window.XSEmpty = XE;
  vm.runInContext(read("../../public/xs-devcenter.js"), vm.createContext({ window, document, location: { hash: "#/workbench" }, console, setTimeout, clearTimeout }));
  return { X: window.XSDev, el: new Element("main"), calls, toasts, body };
}
const rows = (el) => el.querySelector("[data-mlist]").querySelectorAll("[data-k]");

test("live: four counters from counts, blocked pinned first with reason, stage strip in four colours", async () => {
  const h = mk();
  h.X.render(h.el, "workbench"); await settle();
  assert.ok(h.calls.some((c) => c[0] === "/devcenter/my/delivery"));
  const counters = h.el.querySelectorAll("[data-mf]");
  assert.deepEqual(counters.map((b) => b.getAttribute("data-mf")), ["dev", "build", "released", "blocked"]);
  assert.deepEqual(counters.map((b) => text(b.querySelector("b"))), ["1", "1", "2", "1"]);
  assert.deepEqual(counters.map((b) => text(b.querySelector("span"))), ["开发中", "构建中", "已上线", "受阻"]);
  const r = rows(h.el);
  assert.equal(r.length, 5);
  assert.equal(r[0].getAttribute("data-id"), "12");
  assert.ok(r[0].getAttribute("class").includes("xmw-row--blocked"));
  assert.ok(text(r[0]).includes("受阻：最近一次构建失败"));
  assert.ok(text(r[0]).includes("REQ-12"));
  // 其余按更新时间倒序；缺陷编号为 BUG-
  assert.deepEqual(r.slice(1).map((x) => x.getAttribute("data-id")), ["11", "3", "13", "14"]);
  assert.ok(text(r[2]).includes("BUG-3"));
  const dots = r[0].querySelectorAll("circle");
  assert.deepEqual(dots.map((d) => d.getAttribute("data-state")), ["done", "failed", "done", "none", "none"]);
  assert.equal(dots[1].getAttribute("fill"), "#dc2626");
  assert.equal(dots[0].getAttribute("fill"), "#16a34a");
  assert.equal(dots[3].getAttribute("fill"), "#fff");
  const run = r[1].querySelectorAll("circle")[0];
  assert.equal(run.getAttribute("fill"), "#d97706");
  // 最新环境
  assert.ok(text(r[3]).includes("prod"));
  assert.ok(text(r[1]).includes("未上线"));
});

test("最近上线 shows only items released within 7 days", async () => {
  const h = mk();
  h.X.render(h.el, "workbench"); await settle();
  const rec = h.el.querySelector("[data-recent]").querySelectorAll("[data-k]");
  assert.deepEqual(rec.map((x) => x.getAttribute("data-id")), ["13"]);
  assert.ok(text(rec[0]).includes("prod"));
});

test("counters filter the list; clicking again clears the filter", async () => {
  const h = mk();
  h.X.render(h.el, "workbench"); await settle();
  h.el.querySelectorAll("[data-mf]")[3].onclick({ stopPropagation() {} }); await settle();
  assert.deepEqual(rows(h.el).map((x) => x.getAttribute("data-id")), ["12"]);
  assert.equal(h.el.querySelectorAll("[data-mf]")[3].getAttribute("aria-pressed"), "true");
  assert.ok(text(h.el.querySelector("[data-mflabel]")).includes("受阻"));
  h.el.querySelectorAll("[data-mf]")[2].onclick({ stopPropagation() {} }); await settle();
  assert.deepEqual(rows(h.el).map((x) => x.getAttribute("data-id")), ["13", "14"]);
  h.el.querySelectorAll("[data-mf]")[2].onclick({ stopPropagation() {} }); await settle();
  assert.equal(rows(h.el).length, 5);
});

test("filter with no match offers 显示全部", async () => {
  const d = sample(); d.bugs = []; d.counts.in_build = 0;
  const h = mk({ delivery: d });
  h.X.render(h.el, "workbench"); await settle();
  h.el.querySelectorAll("[data-mf]")[1].onclick({ stopPropagation() {} }); await settle();
  const box = h.el.querySelector('[data-xse="search"]');
  assert.ok(box && text(box).includes("构建中"));
  box.querySelector('[data-xse-act="mwall"]').onclick({ stopPropagation() {} }); await settle();
  assert.equal(rows(h.el).length, 4);
});

test("clicking an item opens the existing detail drawer", async () => {
  const h = mk();
  h.X.render(h.el, "workbench"); await settle();
  rows(h.el)[2].onclick(); await settle();
  assert.ok(h.calls.some((c) => c[0] === "/devcenter/bugs/3"));
  h.X.render(h.el, "workbench"); await settle(); // render closes drawer
  h.el.querySelector("[data-recent]").querySelectorAll("[data-k]")[0].onclick(); await settle();
  assert.ok(h.calls.some((c) => c[0] === "/devcenter/requirements/13"));
});

test("demo mode renders demo data without calling the API; click only toasts", async () => {
  const h = mk({ mode: "demo" });
  h.X.render(h.el, "workbench"); await settle();
  assert.equal(h.calls.length, 0);
  assert.ok(rows(h.el).length >= 4);
  assert.ok(rows(h.el)[0].getAttribute("class").includes("xmw-row--blocked"));
  assert.ok(!h.el.querySelector("[data-newreq]"));
  rows(h.el)[0].onclick();
  assert.equal(h.toasts.length, 1);
  assert.ok(h.el.querySelector("[data-recent]").querySelectorAll("[data-k]").length >= 1);
});

test("empty delivery shows XSEmpty with 新建需求, and a plain line without XSEmpty", async () => {
  const empty = { requirements: [], bugs: [], reviews: [], counts: { in_dev: 0, in_build: 0, released: 0, blocked: 0 } };
  let h = mk({ delivery: empty });
  h.X.render(h.el, "workbench"); await settle();
  const box = h.el.querySelector('[data-xse="requirement"]');
  assert.ok(box && text(box).includes("还没有你负责或创建的需求与缺陷"));
  assert.ok(box.querySelector('[data-xse-act="newreq"]'));
  assert.ok(h.el.querySelector('[data-xse="release"]'));
  h = mk({ delivery: empty, withEmpty: false });
  h.X.render(h.el, "workbench"); await settle();
  assert.ok(text(h.el.querySelector("[data-mlist]")).includes("还没有你负责或创建的需求与缺陷。"));
});

test("API failure shows a Chinese error, titles are escaped", async () => {
  let h = mk({ fail: Object.assign(new Error("boom"), { status: 500 }) });
  h.X.render(h.el, "workbench"); await settle();
  assert.ok(text(h.el).includes("读取失败"));
  const d = sample(); d.requirements[0].title = XSS; d.requirements[1].blocked_reason = XSS;
  h = mk({ delivery: d });
  h.X.render(h.el, "workbench"); await settle();
  const html = h.el.querySelector("[data-mlist]").innerHTML;
  assert.ok(!html.includes("<img"));
  assert.ok(html.includes("&lt;img"));
});

test("helpers: bucket fallback and strip from stage when per-stage states are missing", () => {
  const { X } = mk();
  const M = X._mywork;
  assert.equal(M.bucketOf("requirements", { blocked: true }), "blocked");
  assert.equal(M.bucketOf("requirements", { status: "rejected" }), "closed");
  assert.equal(M.bucketOf("bugs", { status: "closed", stage: "none" }), "released");
  assert.equal(M.bucketOf("requirements", { status: "doing", stage: "release", stage_state: "running" }), "build");
  assert.equal(M.bucketOf("requirements", { status: "doing", stage: "code" }), "dev");
  const e = new Element("div"); e.innerHTML = M.stageStrip({ stage: "artifact", stage_state: "running" });
  assert.deepEqual(e.querySelectorAll("circle").map((c) => c.getAttribute("data-state")), ["done", "done", "running", "none", "none"]);
  assert.ok(e.querySelector("svg").getAttribute("aria-label").includes("构建：已完成"));
  assert.equal(M.key("bugs", 7), "BUG-7");
  assert.equal(M.recentReleased([["requirements", { released_at: hoursAgo(1) }], ["requirements", { released_at: null }]]).length, 1);
});

test("no third-party chart lib, no CDN, no gradients in the new section", () => {
  const s = read("../../public/xs-devcenter.js");
  const sec = s.slice(s.indexOf("工作台 · 我的交付"), s.indexOf("迭代 & 看板"));
  assert.ok(sec.length > 1000);
  assert.ok(!/Gradient|https?:\/\/|Chart\(/.test(sec));
  assert.ok(!/\/devcenter\/my\/delivery/.test(sec.replace(/xs\(\)\.get\("\/devcenter\/my\/delivery"\)/, "")), "path only used in the API call, never shown");
});
