import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const STEPS = [{ weight: 10, pause_sec: 300, manual: false }, { weight: 30, pause_sec: 300, manual: false }, { weight: 50, pause_sec: 0, manual: true }, { weight: 100, pause_sec: 0, manual: true }];
const APP = { id: 3, name: "web", image_repo: "harbor.example.com/team/web", envs: [
  { name: "test", namespace: "web-test", deployment: "web", container: "web", target_id: 1 },
  { name: "prod", namespace: "web", deployment: "web", container: "web", target_id: 2 },
  { name: "helm-env", namespace: "web", helm: { release: "web", chart: "web" }, target_id: 2 }
] };
const canary = (o = {}) => ({ id: 7, app_id: 3, env: "prod", image: "harbor.example.com/team/web:v2", stable_image: "harbor.example.com/team/web:v1",
  namespace: "web", service: "web", ingress: "web", plan: { steps: STEPS, replicas: 1, auto_promote: false }, status: "progressing", current_step: 1,
  weight: 30, next_at: "2026-11-15T13:30:00Z", message: "第 2 步（权重 30%）观察中", pending_action: "", created_by: "alice",
  created_at: "2026-11-15T13:00:00Z", finished_at: null, ...o });
const EVENTS = [
  { id: 1, kind: "operator", message: "发起金丝雀发布", actor: "alice", ts: "2026-11-15T13:00:00Z" },
  { id: 2, kind: "step", message: "第 1 步（权重 10%）观察中", actor: "alice", ts: "2026-11-15T13:01:00Z" },
  { id: 3, kind: "error", message: XSS, actor: "system", ts: "2026-11-15T13:02:00Z" }
];
const approvalErr = () => Object.assign(new Error("该环境的变更需要审批，已创建审批单"), { status: 202, code: 40304, approval: { approval_required: true, ticket: { id: 41 } } });

function setup({ perms = ["cicd:read", "cicd:deploy"], list = [canary(), canary({ id: 6, status: "succeeded", weight: 100, current_step: 3, image: XSS })], detail = () => ({ ...canary(), events: EVENTS }), extra } = {}) {
  const h = harness("xs-release-canary.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (extra) { const r = await extra(path, opts); if (r !== undefined) return r; }
      if (path === "/cicd/applications/3") return APP;
      if (path === "/cicd/applications/3/canaries" && !opts.method) return list;
      if (path === "/cicd/applications/3/inventory") return { services: [{ name: "web" }] };
      if (path === "/cicd/canaries/7" && !opts.method) return detail();
      if (/^\/cicd\/canaries\/7\/\w+$/.test(path) && opts.method === "POST") return canary();
      if (path === "/cicd/applications/3/canaries" && opts.method === "POST") return canary({ id: 8, status: "starting", current_step: -1, weight: 0 });
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [];
  h.window.XS.toast = (m) => toasts.push(m);
  return { ...h, toasts };
}
async function mount(h, env = "prod") { h.window.XSReleaseCanary.render(h.el, { appId: 3, env }); await tick(); await tick(); }
async function openDetail(h) { await h.emit("click", '[data-xrc-click="open"]'); await tick(); }
const posts = (h) => h.calls.filter((c) => c.options.method === "POST");

test("frontend paths match backend registrations", () => {
  const go = read("../../server/internal/modules/cicd/canary.go");
  const src = read("../../public/xs-release-canary.js");
  for (const r of ['"/applications/:id/canaries"', '"/canaries/:id"', '"/canaries/:id/advance"', '"/canaries/:id/promote"', '"/canaries/:id/pause"', '"/canaries/:id/resume"', '"/canaries/:id/rollback"']) assert.ok(go.includes(r), r);
  for (const r of ['"/canaries"', '"/cicd/canaries/"']) assert.ok(src.includes(r), r);
  // every backend status has a Chinese label
  for (const s of go.matchAll(/canary\w+\s+= "(\w+)"/g)) if (!["cicd.canary", "true"].includes(s[1]) && !s[0].startsWith("canaryManager") && !s[0].startsWith("canarySuffix") && !s[0].startsWith("canaryApproval")) {
    assert.ok(new RegExp("\\b" + s[1] + ":").test(src), "status label " + s[1]);
  }
});

test("list shows Chinese status, weight and progress, escapes input and polls while active", async () => {
  const h = setup();
  await mount(h);
  assert.equal(h.calls[1]?.options.query?.env ?? h.calls.find((c) => c.path.endsWith("/canaries")).options.query.env, "prod");
  const t = text(h.el);
  for (const s of ["金丝雀发布", "观察中", "已全量", "30%", "第 2 / 4 步", "发起金丝雀"]) assert.ok(t.includes(s), s);
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  assert.ok(!/\/cicd\/|api\/v1|ingress-nginx|Deployment/i.test(t), "no developer jargon");
  assert.equal(h.timers.size, 1, "active canary schedules a refresh");
  const before = h.calls.length;
  await h.runTimer(); await tick();
  assert.ok(h.calls.slice(before).some((c) => c.path === "/cicd/applications/3/canaries"));
});

test("read-only users get no create or action buttons; Helm env hides create", async () => {
  const h = setup({ perms: ["cicd:read"] });
  await mount(h);
  assert.equal(h.el.querySelector('[data-xrc-click="create"]'), null);
  await openDetail(h);
  assert.equal(h.el.querySelectorAll('[data-xrc-click="act"]').length, 0);
  assert.equal(h.el.querySelectorAll('[data-xrc-click="ask"]').length, 0);

  const h2 = setup({ list: [] });
  await mount(h2, "helm-env");
  assert.equal(h2.el.querySelector('[data-xrc-click="create"]'), null);
  assert.ok(text(h2.el).includes("Helm 部署，暂不支持金丝雀发布"));
});

test("detail shows weight bar, steps, timeline and the right buttons", async () => {
  const h = setup();
  await mount(h);
  await openDetail(h);
  const t = text(h.el);
  for (const s of ["观察中", "新版本 30%", "第 1 步", "10%", "观察 5 分钟", "人工确认", "时间线", "发起金丝雀发布", "错误", "harbor.example.com/team/web:v1", "自动进入下一阶段"]) assert.ok(t.includes(s), s);
  assert.equal(h.el.querySelector('[role="progressbar"]').getAttribute("aria-valuenow"), "30");
  assert.ok(h.el.querySelector(".is-cur").textContent.includes("30%"));
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  const acts = h.el.querySelectorAll('[data-xrc-click="act"]').map((b) => b.getAttribute("data-act"));
  assert.deepEqual(acts, ["advance", "pause"]);
  assert.ok(text(h.el.querySelector('[data-act="advance"]')).includes("推进到 50%"));
  const asks = h.el.querySelectorAll('[data-xrc-click="ask"]').map((b) => b.getAttribute("data-act"));
  assert.deepEqual(asks, ["promote", "rollback"]);
});

test("last manual step: no advance; paused: resume; pending approval hides forward actions", async () => {
  let d = { ...canary({ status: "awaiting_confirm", current_step: 3, weight: 100 }), events: [] };
  const h = setup({ detail: () => d });
  await mount(h); await openDetail(h);
  assert.deepEqual(h.el.querySelectorAll('[data-xrc-click="act"]').map((b) => b.getAttribute("data-act")), ["pause"]);
  assert.ok(text(h.el).includes("等待确认"));

  d = { ...canary({ status: "paused" }), events: [] };
  await h.emit("click", '[data-xrc-click="reload"]'); await tick();
  assert.deepEqual(h.el.querySelectorAll('[data-xrc-click="act"]').map((b) => b.getAttribute("data-act")), ["advance", "resume"]);

  d = { ...canary({ pending_action: "promote", approval_id: 41 }), events: [] };
  await h.emit("click", '[data-xrc-click="reload"]'); await tick();
  assert.deepEqual(h.el.querySelectorAll('[data-xrc-click="act"]').map((b) => b.getAttribute("data-act")), ["pause"]);
  assert.deepEqual(h.el.querySelectorAll('[data-xrc-click="ask"]').map((b) => b.getAttribute("data-act")), ["rollback"]);
  assert.ok(text(h.el).includes("等待审批：全量发布（审批单 #41）"));
});

test("advance and pause post directly", async () => {
  const h = setup();
  await mount(h); await openDetail(h);
  await h.emit("click", '[data-act="advance"]'); await tick();
  assert.equal(posts(h).at(-1).path, "/cicd/canaries/7/advance");
  assert.ok(h.toasts.includes("推进下一步已提交"));
  await h.emit("click", '[data-act="pause"]'); await tick();
  assert.equal(posts(h).at(-1).path, "/cicd/canaries/7/pause");
});

test("promote needs a second confirmation; 202 shows 已提交审批", async () => {
  const h = setup({ extra: async (p, o) => { if (p === "/cicd/canaries/7/promote" && o.method === "POST") throw approvalErr(); } });
  await mount(h); await openDetail(h);
  await h.emit("click", '[data-act="promote"]');
  assert.equal(posts(h).length, 0, "no request before confirmation");
  assert.ok(text(h.el).includes("确认全量发布？"));
  await h.emit("click", '[data-xrc-click="dismiss"]');
  assert.equal(h.el.querySelector('[data-xrc-click="confirm"]'), null);
  await h.emit("click", '[data-act="promote"]');
  h.el.querySelector("[data-xrc-reason]").value = "验证通过";
  await h.emit("click", '[data-xrc-click="confirm"]'); await tick();
  const p = posts(h).at(-1);
  assert.equal(p.path, "/cicd/canaries/7/promote");
  assert.deepEqual(JSON.parse(JSON.stringify(p.options.body)), { reason: "验证通过" });
  assert.ok(h.toasts.some((m) => m.startsWith("已提交审批") && m.includes("#41")));
  assert.equal(h.el.querySelector('[data-xrc-click="confirm"]'), null);
});

test("rollback needs a second confirmation and reports conflicts in Chinese", async () => {
  let fail = true;
  const h = setup({ extra: async (p, o) => { if (p === "/cicd/canaries/7/rollback" && o.method === "POST" && fail) throw Object.assign(new Error("该金丝雀正在执行其他操作，请稍后再试"), { status: 409 }); } });
  await mount(h); await openDetail(h);
  await h.emit("click", '[data-act="rollback"]');
  assert.ok(text(h.el).includes("确认回滚本次金丝雀？"));
  await h.emit("click", '[data-xrc-click="confirm"]'); await tick();
  assert.ok(text(h.el).includes("回滚失败：该金丝雀正在执行其他操作，请稍后再试"));
  fail = false;
  await h.emit("click", '[data-xrc-click="confirm"]'); await tick();
  assert.equal(posts(h).at(-1).path, "/cicd/canaries/7/rollback");
  assert.ok(h.toasts.includes("回滚已提交"));
});

test("English backend errors are shown in Chinese", async () => {
  const h = setup({ extra: async (p, o) => { if (p === "/cicd/canaries/7/advance") throw Object.assign(new Error("environment not in your scope: prod"), { status: 403 }); } });
  await mount(h); await openDetail(h);
  await h.emit("click", '[data-act="advance"]'); await tick();
  assert.ok(text(h.el).includes("推进下一步失败：没有该环境的发布权限"));
  assert.ok(!text(h.el).includes("scope"));
});

test("create form: default steps, inline validation, body and approval", async () => {
  let created = null, approve = false;
  const h = setup({ list: [], extra: async (p, o) => {
    if (p === "/cicd/applications/3/canaries" && o.method === "POST") { created = JSON.parse(JSON.stringify(o.body)); if (approve) throw approvalErr(); }
  } });
  await mount(h);
  await h.emit("click", '[data-xrc-click="create"]'); await tick(); await tick();
  const weights = () => h.el.querySelectorAll("input").filter((n) => n.hasAttribute("data-xrc-sw")).map((n) => n.value);
  assert.deepEqual(weights(), ["10", "30", "50", "100"]);
  // helm env not offered
  assert.deepEqual(h.el.querySelectorAll("option").map((o) => o.value).filter((v) => ["test", "prod", "helm-env"].includes(v)), ["test", "prod"]);
  assert.equal(h.el.querySelector('[data-xrc-f="service"]').value, "web", "single service is prefilled");

  // empty image + non-increasing weights → inline errors, no POST
  h.el.querySelector('[data-xrc-sw="2"]').value = "30";
  await h.emit("click", '[data-xrc-click="submit"]');
  assert.equal(created, null);
  assert.ok(h.el.querySelector('[data-xrc-err="image"]'));
  assert.ok(text(h.el.querySelector('[data-xrc-serr="2"]')).includes("第 3 步权重须大于上一步（30%）"));

  h.el.querySelector('[data-xrc-f="image"]').value = "harbor.example.com/team/web:v2";
  h.el.querySelector('[data-xrc-f="ingress"]').value = "web";
  h.el.querySelector('[data-xrc-sw="2"]').value = "101";
  await h.emit("click", '[data-xrc-click="submit"]');
  assert.equal(created, null);
  assert.ok(text(h.el.querySelector('[data-xrc-serr="2"]')).includes("1–100"));

  // remove step, add step, then a valid plan
  await h.emit("click", '[data-i="2"]');
  assert.deepEqual(weights(), ["10", "30", "100"]);
  h.el.querySelector('[data-xrc-sw="2"]').value = "60";
  await h.emit("click", '[data-xrc-click="addStep"]');
  assert.deepEqual(weights(), ["10", "30", "60", "70"]);
  h.el.querySelector('[data-xrc-sw="3"]').value = "100";
  h.el.querySelector('[data-xrc-sm="3"]').checked = true;
  h.el.querySelector('[data-xrc-sp="0"]').value = "120";
  h.el.querySelector('[data-xrc-f="replicas"]').value = "2";
  h.el.querySelector('[data-xrc-f="auto_promote"]').checked = true;
  h.el.querySelector('[data-xrc-f="reason"]').value = "新版本灰度";
  await h.emit("click", '[data-xrc-click="submit"]'); await tick();
  assert.deepEqual(created, {
    env: "prod", image: "harbor.example.com/team/web:v2", service: "web", ingress: "web",
    steps: [{ weight: 10, pause_sec: 120, manual: false }, { weight: 30, pause_sec: 300, manual: false }, { weight: 60, pause_sec: 0, manual: true }, { weight: 100, pause_sec: 300, manual: true }],
    replicas: 2, auto_promote: true, reason: "新版本灰度"
  });
  assert.ok(h.toasts.includes("金丝雀发布已发起"));
  assert.equal(h.el.querySelector("[data-xrc-view]").getAttribute("data-xrc-view"), "detail");

  // 202 on create
  approve = true; created = null;
  const h2 = setup({ list: [], extra: async (p, o) => { if (p === "/cicd/applications/3/canaries" && o.method === "POST") { created = o.body; throw approvalErr(); } } });
  await mount(h2);
  await h2.emit("click", '[data-xrc-click="create"]'); await tick(); await tick();
  h2.el.querySelector('[data-xrc-f="image"]').value = "harbor.example.com/team/web:v3";
  h2.el.querySelector('[data-xrc-f="ingress"]').value = "web";
  await h2.emit("click", '[data-xrc-click="submit"]'); await tick();
  assert.ok(created);
  assert.ok(h2.toasts.some((m) => m.startsWith("已提交审批")));
  assert.equal(h2.el.querySelector("[data-xrc-view]").getAttribute("data-xrc-view"), "list");
});

test("server validation error stays on the form", async () => {
  const h = setup({ list: [], extra: async (p, o) => {
    if (p === "/cicd/applications/3/canaries" && o.method === "POST") throw Object.assign(new Error("Helm 部署的环境暂不支持金丝雀发布"), { status: 409 });
  } });
  await mount(h);
  await h.emit("click", '[data-xrc-click="create"]'); await tick(); await tick();
  h.el.querySelector('[data-xrc-f="image"]').value = "a/b:1";
  h.el.querySelector('[data-xrc-f="ingress"]').value = "web";
  await h.emit("click", '[data-xrc-click="submit"]'); await tick();
  assert.ok(text(h.el).includes("发起失败：Helm 部署的环境暂不支持金丝雀发布"));
  assert.equal(h.el.querySelector("[data-xrc-view]").getAttribute("data-xrc-view"), "create");
});

test("openCreate switches a mounted page to the form", async () => {
  const h = setup({ list: [] });
  await mount(h);
  h.window.XSReleaseCanary.openCreate(3, "test"); await tick(); await tick();
  assert.equal(h.el.querySelector("[data-xrc-view]").getAttribute("data-xrc-view"), "create");
  assert.equal(h.el.querySelector('[data-xrc-f="env"]').value, "test");
});

test("validateSteps", () => {
  const { validateSteps } = (() => { const h = setup(); return h.window.XSReleaseCanary; })();
  assert.equal(validateSteps([{ weight: 10 }, { weight: 30 }, { weight: 50 }, { weight: 100 }]).error, "");
  assert.match(validateSteps([]).error, /至少/);
  assert.match(validateSteps([{ weight: 0 }]).error, /1–100/);
  assert.match(validateSteps([{ weight: "abc" }]).error, /1–100/);
  assert.match(validateSteps([{ weight: 10 }, { weight: 10 }]).error, /第 2 步权重须大于上一步/);
  assert.match(validateSteps([{ weight: 10, pause_sec: 90000 }]).error, /0–86400/);
  assert.match(validateSteps(Array.from({ length: 21 }, (_, i) => ({ weight: i + 1 }))).error, /最多 20/);
});
