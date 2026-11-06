import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-cicd-pipeline.js", import.meta.url), "utf8");
const css = fs.readFileSync(new URL("../../public/xs-cicd-pipeline.css", import.meta.url), "utf8");
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

function setup(hash, get) {
  const listeners = new Map();
  const el = {
    innerHTML: "", isConnected: true,
    addEventListener(name, handler) { listeners.set(name, handler); },
    removeEventListener(name) { listeners.delete(name); },
    contains() { return true; }, querySelector() { return null; }
  };
  const location = { hash };
  const window = {
    XS: { get, post: async () => ({}), put: async () => ({}), isCancelled: () => false },
    addEventListener() {}, removeEventListener() {}
  };
  const context = vm.createContext({ window, location, document: {}, AbortController, Blob, URL, setTimeout, clearTimeout, console });
  vm.runInContext(source, context);
  return { el, view: window.XSCICDPipelineView, window, listeners, location };
}

const pipeline = { id: 3, app_id: 9, runner_target_id: 4, name: "release", on_push: true,
  stages: [{ name: "build", type: "build", image: "builder:v1", registry_secret: "push-secret", commands: ["make image"], env: { MODE: "release" }, params: { BRANCH: "main" }, timeout_sec: 600 }] };
const run = { id: 61, pipeline_id: 3, app_id: 9, status: "failed", trigger: "manual", triggered_by: "alice", branch: "main",
  commit: "aabbccddeeff0011223344556677889900aabbcc", image_tag: "candidate-61", error: "build failed",
  created_at: "2026-10-28T00:00:00Z", started_at: "2026-10-28T00:01:00Z", finished_at: "2026-10-28T00:02:00Z",
  stages: [{ idx: 0, name: "build", type: "build", status: "failed", job_name: "build-job", approval_id: 17,
    error: "image push failed", started_at: "2026-10-28T00:01:00Z", finished_at: "2026-10-28T00:02:00Z" }] };

test("routes preserve the list and detail hashes, including escaped ids", () => {
  const { view } = setup("#/cicd/pipeline", () => Promise.resolve([]));
  assert.equal(view._internals.route("#/cicd/pipeline").id, "");
  assert.equal(view._internals.route("#/cicd/pipeline/abc%20def").id, "abc def");
  assert.equal(view._internals.route("#/cicd/pipeline/3?run=8").id, "3");
  assert.equal(view._internals.route("#/logs"), null);
});

test("run summary and stages show complete execution facts without truncating commit", () => {
  const { view } = setup("#/cicd/pipeline", () => Promise.resolve([]));
  const summary = view._internals.runSummary(run);
  const stages = view._internals.runStages(run);
  assert.match(summary, /aabbccddeeff0011223344556677889900aabbcc/);
  for (const fact of ["运行 ID", "触发人", "分支", "镜像标签", "创建时间", "开始时间", "结束时间", "运行耗时", "build failed"]) assert.ok(summary.includes(fact), fact);
  for (const fact of ["build-job", "#17", "image push failed", "耗时", "失败"]) assert.ok(stages.includes(fact), fact);
});

test("current configuration is explicitly not passed off as historical snapshot", () => {
  const { view } = setup("#/cicd/pipeline", () => Promise.resolve([]));
  const html = view._internals.currentConfig(pipeline);
  assert.match(html, /历史运行未保存阶段配置快照/);
  for (const fact of ["builder:v1", "make image", "MODE", "push-secret", "Jenkins 参数", "600 秒"]) assert.ok(html.includes(fact), fact);
  assert.doesNotMatch(view._internals.runStages(run), /builder:v1/);
});

test("pipeline table has fixed widths and independently flexible name/application columns", () => {
  assert.match(css, /\.x-cicd-table\s*\{[^}]*table-layout:\s*fixed/s);
  for (const name of ["id", "state", "count", "date", "actions", "flex"]) assert.match(css, new RegExp("\\.x-cicd-col-" + name));
  assert.match(css, /\.x-cicd-table \.x-cicd-row-actions\s*\{[^}]*text-align:\s*right/s);
});

test("pipeline definitions load first; detail does not request Jenkins or Argo CD", async () => {
  let resolvePipelines;
  const calls = [];
  const { el, view } = setup("#/cicd/pipeline/3", (path) => {
    calls.push(path);
    if (path === "/cicd/pipelines") return new Promise((resolve) => { resolvePipelines = resolve; });
    if (path.startsWith("/cicd/applications/")) return Promise.resolve({ id: 9, name: "matrix", repo_url: "https://git.example/matrix", branch: "main" });
    if (path === "/cicd/targets") return Promise.resolve([{ id: 4, name: "runner" }]);
    if (path === "/cicd/runs") return Promise.resolve({ items: [run] });
    if (path === "/cicd/runs/61") return Promise.resolve(run);
    if (path === "/cicd/runs/61/logs") return Promise.resolve([]);
    if (path === "/cicd/credentials" || path.endsWith("/cache")) return Promise.resolve([]);
    throw Error("Unexpected " + path);
  });
  const stop = view.render(el);
  assert.deepEqual(calls, ["/cicd/pipelines"]);
  resolvePipelines([pipeline]);
  await flush(); await flush(); await flush();
  assert.match(el.innerHTML, /https:\/\/git\.example\/matrix/);
  assert.match(el.innerHTML, /aabbccddeeff0011223344556677889900aabbcc/);
  assert.equal(calls.includes("/cicd/jenkins"), false);
  assert.equal(calls.includes("/cicd/argocd"), false);
  stop();
});

test("long logs expose a continuation and load beyond 5000 lines without dropping data", async () => {
  let page = 0;
  const { el, view, listeners } = setup("#/cicd/pipeline/3", (path) => {
    if (path === "/cicd/pipelines") return Promise.resolve([pipeline]);
    if (path === "/cicd/applications/9") return Promise.resolve({ id: 9, name: "matrix" });
    if (path === "/cicd/targets") return Promise.resolve([]);
    if (path === "/cicd/runs") return Promise.resolve({ items: [run] });
    if (path === "/cicd/runs/61") return Promise.resolve(run);
    if (path === "/cicd/runs/61/logs") return Promise.resolve(++page === 1
      ? Array.from({ length: 5000 }, (_, i) => ({ id: i + 1, stage: 0, line: "line " + i, ts: "2026-10-28T00:00:00Z" }))
      : [{ id: 5001, stage: 0, line: "the final line" }]);
    if (path === "/cicd/credentials" || path.endsWith("/cache")) return Promise.resolve([]);
    throw Error("Unexpected " + path);
  });
  const stop = view.render(el);
  await flush(); await flush(); await flush();
  assert.match(el.innerHTML, /5000 条已加载，(还有后续日志)/);
  assert.match(el.innerHTML, /加载后续日志/);
  const button = { getAttribute: () => "more-log", tagName: "BUTTON" };
  listeners.get("click")({ target: { closest: () => button } });
  await flush();
  assert.match(el.innerHTML, /5001 条已加载/);
  assert.match(el.innerHTML, /the final line/);
  assert.doesNotMatch(el.innerHTML, /加载后续日志/);
  stop();
});

test("API failures show an inline retry instead of an empty page", async () => {
  const { el, view } = setup("#/cicd/pipeline", () => Promise.reject(new Error("service unavailable")));
  const stop = view.render(el);
  await flush();
  assert.match(el.innerHTML, /service unavailable/);
  assert.match(el.innerHTML, /data-action="refresh"/);
  stop();
});

test("an empty run list keeps the pipeline definition visible", async () => {
  const { el, view } = setup("#/cicd/pipeline/3", (path) => {
    if (path === "/cicd/pipelines") return Promise.resolve([pipeline]);
    if (path === "/cicd/applications/9") return Promise.resolve({ id: 9, name: "matrix" });
    if (path === "/cicd/targets") return Promise.resolve([]);
    if (path === "/cicd/runs") return Promise.resolve({ items: [] });
    if (path === "/cicd/credentials" || path.endsWith("/cache")) return Promise.resolve([]);
    throw Error("Unexpected " + path);
  });
  const stop = view.render(el);
  await flush(); await flush();
  assert.match(el.innerHTML, /暂无运行记录/);
  assert.match(el.innerHTML, /当前流水线配置/);
  stop();
});

test("a failed run detail request preserves history and offers retry", async () => {
  const { el, view } = setup("#/cicd/pipeline/3", (path) => {
    if (path === "/cicd/pipelines") return Promise.resolve([pipeline]);
    if (path === "/cicd/applications/9") return Promise.resolve({ id: 9, name: "matrix" });
    if (path === "/cicd/targets") return Promise.resolve([]);
    if (path === "/cicd/runs") return Promise.resolve({ items: [run] });
    if (path === "/cicd/runs/61") return Promise.reject(new Error("detail unavailable"));
    if (path === "/cicd/credentials" || path.endsWith("/cache")) return Promise.resolve([]);
    throw Error("Unexpected " + path);
  });
  const stop = view.render(el);
  await flush(); await flush(); await flush();
  assert.match(el.innerHTML, /detail unavailable/);
  assert.match(el.innerHTML, /data-action="retry-run"/);
  assert.match(el.innerHTML, /运行历史/);
  stop();
});

test("escaping, log stage filtering and duration helpers preserve data safely", () => {
  const { view } = setup("#/cicd/pipeline", () => Promise.resolve([]));
  assert.equal(view._internals.escapeHTML('<script>"'), "&lt;script&gt;&quot;");
  assert.match(view._internals.formatLog({ stage: 1, line: "x" }, ["first", "second"]), /\[second\] x/);
  assert.equal(view._internals.duration("2026-10-28T00:00:00Z", "2026-10-28T00:02:30Z"), "2分 30秒");
});
