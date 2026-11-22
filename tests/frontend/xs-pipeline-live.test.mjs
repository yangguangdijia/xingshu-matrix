import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

const source = fs.readFileSync(new URL("../../public/xs-cicd-pipeline.js", import.meta.url), "utf8");

/* 递归取文本（测试 DOM 的 innerHTML 只保留赋值时的原文，局部刷新后父节点不会更新） */
function txt(node) { return node ? (node.textContent || "") + node.children.map(txt).join("") : ""; }
const live = (el) => txt(el.querySelector("[data-live-state]"));
function iso(msAgo) { return new Date(Date.now() - msAgo).toISOString(); }

function setup({ get, mode, hash = "#/cicd/pipeline/3" }) {
  const timers = new Map(); let seq = 0;
  const calls = [];
  const XS = {
    get: async (path, query) => { calls.push({ path, query }); return get(path, query); },
    post: async () => ({}), put: async () => ({}), del: async () => ({}), isCancelled: () => false
  };
  if (mode) XS.mode = mode;
  const window = { XS, addEventListener() {}, removeEventListener() {} };
  const context = vm.createContext({
    window, location: { hash }, document: { hidden: false, addEventListener() {}, removeEventListener() {} },
    AbortController, Blob, URL, console, Date, Promise,
    setTimeout: (fn, delay) => { const id = ++seq; timers.set(id, { fn, delay }); return id; },
    clearTimeout: (id) => timers.delete(id)
  });
  vm.runInContext(source, context);
  const el = new Element("main");
  const view = window.XSCICDPipelineView;
  const dispose = view.render(el);
  async function emit(type, selector, patch) {
    const target = el.querySelector(selector);
    if (!target) throw new Error("missing " + selector);
    Object.assign(target, patch || {});
    for (const fn of [...(el.listeners[type] || [])]) await fn({ target, preventDefault() {} });
    await tick();
  }
  async function flush() { for (let i = 0; i < 6; i++) await tick(); }
  /* 运行一个满足条件的定时器（按延时筛选：轮询 3000+，计时 1000） */
  async function fire(pred) {
    for (const [id, t] of timers) if (pred(t.delay)) { timers.delete(id); await t.fn(); await flush(); return t.delay; }
    return null;
  }
  return { el, view, calls, timers, emit, flush, fire, window, dispose };
}

const pipeline = { id: 3, app_id: 9, runner_target_id: 4, name: "release", on_push: false, stages: [{ name: "test", type: "test" }, { name: "image", type: "build" }, { name: "deploy", type: "deploy" }] };
function runningRun(id = 70) {
  return { id, pipeline_id: 3, status: "running", trigger: "manual", branch: "main", created_at: iso(60000), started_at: iso(55000), finished_at: null,
    stages: [
      { idx: 0, name: "test", type: "test", status: "succeeded", started_at: iso(55000), finished_at: iso(30000) },
      { idx: 1, name: "image", type: "build", status: "running", started_at: iso(30000), finished_at: null },
      { idx: 2, name: "deploy", type: "deploy", status: "pending" }
    ] };
}
function failedRun(id = 69) {
  return { id, pipeline_id: 3, status: "failed", trigger: "manual", branch: "fix", error: "阶段失败", created_at: iso(3600e3), started_at: iso(3600e3), finished_at: iso(3500e3),
    stages: [
      { idx: 0, name: "test", type: "test", status: "succeeded", started_at: iso(3600e3), finished_at: iso(3560e3) },
      { idx: 1, name: "image", type: "build", status: "failed", error: "push denied by registry", started_at: iso(3560e3), finished_at: iso(3500e3) },
      { idx: 2, name: "deploy", type: "deploy", status: "skipped" }
    ] };
}
function baseApi(runs, detail, logs) {
  return (path, query) => {
    if (path === "/cicd/pipelines") return [pipeline];
    if (path.startsWith("/cicd/applications/")) return { id: 9, name: "matrix", branch: "main" };
    if (path === "/cicd/targets") return [{ id: 4, name: "runner" }];
    if (path === "/cicd/credentials") return [];
    if (path === "/cicd/runs") return { items: runs() };
    let m = /^\/cicd\/runs\/(\d+)$/.exec(path); if (m) return detail(m[1]);
    m = /^\/cicd\/runs\/(\d+)\/logs$/.exec(path); if (m) return logs(m[1], query);
    throw new Error("unexpected " + path);
  };
}

test("pure helpers: run filter, failed-first order, focus stage, error lines, backoff", () => {
  const { view } = setup({ get: () => [] , hash: "#/cicd/pipeline" });
  const I = view._internals;
  const runs = [{ id: 5, status: "succeeded" }, { id: 4, status: "failed" }, { id: 3, status: "running" }, { id: 2, status: "failed" }, { id: 1, status: "cancelled" }];
  assert.deepEqual(I.filterRuns(runs, "failed").map((r) => r.id), [4, 2]);
  assert.deepEqual(I.filterRuns(runs, "active").map((r) => r.id), [3]);
  assert.deepEqual(I.filterRuns(runs, "", true).map((r) => r.id), [4, 2, 5, 3, 1]);
  assert.deepEqual({ ...I.focusStage(runningRun()) }, { running: "1", failed: null });
  assert.deepEqual({ ...I.focusStage(failedRun()) }, { running: null, failed: "1" });
  assert.ok(I.isErrorLine("error: failed to push: unauthorized"));
  assert.ok(I.isErrorLine("单元测试失败"));
  assert.ok(!I.isErrorLine("ok  shop/internal/cart 0.4s"));
  assert.ok(!I.isErrorLine("0 errors found"));
  let d = 3000; const seen = [];
  for (let i = 0; i < 5; i++) { d = I.nextDelay(d, true); seen.push(d); }
  assert.deepEqual(seen, [6000, 12000, 24000, 30000, 30000]);
  assert.equal(I.nextDelay(30000, false), 3000);
});

test("stage graph: running stage pulses with a ticking timer, finished stages show duration, failed shows error", () => {
  const { view } = setup({ get: () => [], hash: "#/cicd/pipeline" });
  const html = view._internals.stageGraph(runningRun(), "1");
  assert.match(html, /x-cicd-node--ok[^"]*" data-stage-node="0"/);
  assert.match(html, /x-cicd-node--busy is-active is-running" data-stage-node="1"/);
  assert.match(html, /x-cicd-pulse/);
  assert.match(html, /data-tick-start="/);
  assert.match(html, /25秒|2[4-6]秒/); // finished test stage: 25s
  assert.match(html, /未开始/);
  const failed = view._internals.stageGraph(failedRun(), "1");
  assert.match(failed, /x-cicd-node--bad/);
  assert.match(failed, /push denied by registry/);
  assert.doesNotMatch(failed, /data-tick-start/);
  assert.match(view._internals.runSummary(runningRun(), { text: "自动刷新中" }), /自动刷新中/);
});

test("running run polls detail every 3s, ticks elapsed, follows the running stage and stops when terminal", async () => {
  let detail = runningRun();
  const logAfter = [];
  const { el, calls, timers, flush, fire, view } = setup({ get: baseApi(() => [detail], () => detail, (_id, q) => {
    logAfter.push(q.after);
    return q.after ? [] : [{ id: 1, stage: 0, line: "go test ./...", ts: iso(50000) }, { id: 2, stage: 1, line: "[2/4] RUN go build", ts: iso(20000) }];
  }) });
  void view; await flush();
  assert.match(txt(el.querySelector("[data-run-live]")), /运行 #70/);
  assert.equal(live(el), "自动刷新中");
  assert.equal(el.querySelector("[data-log-stage]").value, "1", "follows the running stage");
  assert.equal(el.querySelector("[data-log-follow]").checked, true);
  assert.match(txt(el.querySelector(".x-cicd-follow")), /^\s*跟随$/);
  assert.match(el.querySelector("[data-log-count]").textContent, /2 条已加载/);
  assert.ok([...timers.values()].some((t) => t.delay === 3000), "poll scheduled at 3s");
  assert.ok([...timers.values()].some((t) => t.delay === 1000), "elapsed ticker scheduled");
  const tickNode = el.querySelector("[data-tick-start]");
  tickNode.textContent = "x";
  await fire((d) => d === 1000);
  assert.match(tickNode.textContent, /秒/);

  // stage 1 finished, stage 2 now running → log filter moves to stage 2
  detail = runningRun();
  detail.stages[1] = { ...detail.stages[1], status: "succeeded", finished_at: iso(1000) };
  detail.stages[2] = { ...detail.stages[2], status: "running", started_at: iso(1000) };
  const before = calls.filter((c) => c.path === "/cicd/runs/70").length;
  await fire((d) => d >= 3000);
  assert.equal(calls.filter((c) => c.path === "/cicd/runs/70").length, before + 1);
  assert.equal(el.querySelector("[data-log-stage]").value, "2");
  assert.ok(logAfter.includes(2), "log polling resumes after the last id");

  // terminal → polling stops
  detail = { ...runningRun(), status: "succeeded", finished_at: iso(0), stages: runningRun().stages.map((s) => ({ ...s, status: "succeeded", started_at: s.started_at || iso(1000), finished_at: iso(0) })) };
  await fire((d) => d >= 3000);
  assert.match(txt(el.querySelector("[data-run-live]").querySelector("h2")), /成功/);
  assert.ok(![...timers.values()].some((t) => t.delay >= 3000), "no more polling after terminal state");
  assert.equal(live(el), "");
  assert.equal(el.querySelectorAll("[data-tick-start]").length, 0, "no ticking timers once finished");
});

test("poll failures back off and show a calm retry hint; dispose clears timers", async () => {
  let fail = false;
  const { el, timers, flush, fire, view } = setup({ get: baseApi(() => [runningRun()], () => { if (fail) throw new Error("网络超时"); return runningRun(); }, () => []) });
  await flush();
  fail = true;
  await fire((d) => d >= 3000);
  assert.ok([...timers.values()].some((t) => t.delay === 6000), "backoff to 6s");
  assert.equal(live(el), "刷新失败，6 秒后重试");
  await fire((d) => d >= 3000);
  assert.ok([...timers.values()].some((t) => t.delay === 12000), "backoff to 12s");
  fail = false;
  await fire((d) => d >= 3000);
  assert.ok([...timers.values()].some((t) => t.delay === 3000), "back to 3s after success");
  assert.equal(live(el), "自动刷新中");
  void view;
});

test("leaving the page (dispose) stops polling and ticking", async () => {
  const s = setup({ get: baseApi(() => [runningRun()], () => runningRun(), () => []), hash: "#/cicd/pipeline/3" });
  await s.flush();
  assert.ok(s.timers.size > 0);
  s.dispose();
  assert.equal(s.timers.size, 0);
});

test("failed run auto-selects the failed stage, shows its error and highlights error lines", async () => {
  const { el, emit, flush } = setup({ get: baseApi(() => [failedRun(), { ...failedRun(68), status: "succeeded" }], () => failedRun(), () => [
    { id: 1, stage: 0, line: "PASS", ts: iso(3590e3) },
    { id: 2, stage: 1, line: "pushing image", ts: iso(3520e3) },
    { id: 3, stage: 1, line: "error: push denied by registry", ts: iso(3510e3) }
  ]) });
  await flush();
  assert.equal(el.querySelector("[data-log-stage]").value, "1");
  const banner = el.querySelector("[data-log-fail]");
  assert.match(txt(banner), /阶段「image」失败/);
  assert.match(txt(banner), /push denied by registry/);
  const errs = el.querySelectorAll("[data-log-err]");
  assert.equal(errs.length, 1);
  assert.match(errs[0].textContent, /error: push denied/);
  assert.equal(live(el), "");
  // user picks all stages → pinned; banner offers jump back
  await emit("change", "[data-log-stage]", { value: "" });
  assert.equal(el.querySelector("[data-log-stage]").value, "");
  assert.match(txt(el.querySelector("[data-log-fail]")), /查看该阶段日志/);
});

test("run history: status filter, failed first, relative time and duration column", async () => {
  const runs = [
    { id: 72, status: "succeeded", branch: "main", trigger: "manual", created_at: iso(120e3), started_at: iso(120e3), finished_at: iso(30e3) },
    { id: 71, status: "failed", branch: "dev", trigger: "manual", created_at: iso(7200e3), started_at: iso(7200e3), finished_at: iso(7140e3) },
    { id: 70, status: "succeeded", branch: "main", trigger: "manual", created_at: iso(2 * 86400e3), started_at: iso(2 * 86400e3), finished_at: iso(2 * 86400e3 - 75e3) }
  ];
  const { el, emit, flush } = setup({ get: baseApi(() => runs, (id) => ({ ...runs.find((r) => String(r.id) === id), stages: [] }), () => []) });
  await flush();
  const hist = () => el.querySelector("[data-run-history]");
  const h = txt(hist());
  for (const want of ["耗时", "2 分钟前", "2 小时前", "2 天前", "1分 30秒", "1分 15秒"]) assert.ok(h.includes(want), want);
  assert.equal(hist().querySelectorAll(".x-cicd-hwhen")[0].getAttribute("title").length > 5, true, "absolute time in title");
  await emit("change", "[data-run-filter]", { value: "failed" });
  let ids = hist().querySelectorAll("[data-action]").map((b) => b.getAttribute("data-id"));
  assert.deepEqual(ids, ["71"]);
  assert.match(el.querySelector("[data-run-count]").textContent, /1 \/ 3/);
  await emit("change", "[data-run-filter]", { value: "" });
  await emit("change", "[data-run-failed-first]", { checked: true });
  ids = hist().querySelectorAll("[data-action]").map((b) => b.getAttribute("data-id"));
  assert.deepEqual(ids, ["71", "72", "70"]);
  await emit("change", "[data-run-filter]", { value: "cancelled" });
  assert.match(txt(hist()), /没有符合筛选条件的运行/);
});

test("unchecking 跟随 keeps the user's stage; re-checking jumps back to the running stage", async () => {
  const { el, emit, flush, fire } = setup({ get: baseApi(() => [runningRun()], () => runningRun(), () => []) });
  await flush();
  await emit("change", "[data-log-follow]", { checked: false });
  await emit("change", "[data-log-stage]", { value: "0" });
  await fire((d) => d >= 3000);
  assert.equal(el.querySelector("[data-log-stage]").value, "0");
  await emit("change", "[data-log-follow]", { checked: true });
  assert.equal(el.querySelector("[data-log-stage]").value, "1");
});

test("demo mode renders demo pipelines and a live demo run without calling the platform", async () => {
  const { el, calls, flush, view } = setup({ get: () => { throw new Error("should not call"); }, mode: "demo", hash: "#/cicd/pipeline/1" });
  await flush();
  assert.equal(calls.length, 0);
  const all = txt(el);
  assert.match(all, /演示模式/);
  assert.match(all, /shop-web-ci/);
  assert.match(all, /运行 #128/);
  assert.match(all, /运行中/);
  assert.match(txt(el.querySelector("[data-run-history]")), /#127/);
  const api = view._internals.demoAPI({});
  const r127 = await api.get("/cicd/runs/127");
  assert.equal(r127.status, "failed");
  assert.equal(r127.stages[0].status, "failed");
  assert.equal(r127.stages[1].status, "skipped");
  const logs = await api.get("/cicd/runs/127/logs", { after: 0 });
  assert.ok(logs.some((l) => /exit status 1/.test(l.line)));
  const list = await api.get("/cicd/pipelines");
  assert.equal(list.length, 2);
  const created = await api.post("/cicd/pipelines/1/run", { branch: "main" });
  assert.equal((await api.get("/cicd/runs/" + created.id)).status, "queued");
  await assert.rejects(api.put("/cicd/pipelines/1", {}), /演示模式/);
});
