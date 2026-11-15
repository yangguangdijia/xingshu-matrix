import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const plain = (o) => JSON.parse(JSON.stringify(o));
const T = "2026-11-15T13:00:00Z";
const job = (o = {}) => ({ id: 1, type: "backup.run", payload: { target: "pg", password: "***" }, status: "queued", priority: 0, attempts: 0, max_attempts: 5,
  run_at: T, locked_by: "", locked_until: null, last_error: "", dedupe_key: null, created_by: null, created_at: T, updated_at: T, started_at: null, finished_at: null, ...o });
const rows = () => [
  job({ id: 5, status: "running", attempts: 1, locked_by: "matrix-0" }),
  job({ id: 4, status: "failed", attempts: 2, last_error: XSS }),
  job({ id: 3, status: "dead", attempts: 5, last_error: "连接超时" }),
  job({ id: 2, status: "succeeded", attempts: 1, finished_at: T }),
  job({ id: 1, type: XSS, status: "queued" })
];

function setup({ perms = ["setting:read", "setting:manage"], items = rows(), total, extra } = {}) {
  const h = harness("xs-jobs.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (extra) { const r = await extra(path, opts); if (r !== undefined) return r; }
      if (path === "/jobs") return { items, total: total ?? items.length, page: opts.query?.page || 1, page_size: 20 };
      const m = path.match(/^\/jobs\/(\d+)(\/retry)?$/);
      if (m && !opts.method) return { ...items.find((j) => String(j.id) === m[1]), payload: { target: "pg", api_key: "leaked-value", nested: { token: "x" } } };
      if (m && m[2] && opts.method === "POST") return { ...items.find((j) => String(j.id) === m[1]), status: "queued", attempts: 0 };
      if (m && opts.method === "DELETE") return { id: +m[1], deleted: true };
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [];
  h.window.XS.toast = (m) => toasts.push(m);
  return { ...h, toasts };
}
async function mount(h) { h.window.XSJobs.render(h.el); await tick(); await tick(); }
const row = (h, id) => h.el.querySelector(`[data-jobs-row="${id}"]`);
const btn = (h, act, id) => h.el.querySelectorAll(`[data-jobs-click="${act}"]`).find((b) => b.getAttribute("data-id") === String(id) && row(h, id)?.contains(b));

test("frontend paths and statuses match backend", () => {
  const go = read("../../server/internal/jobs/api.go");
  const jobs = read("../../server/internal/jobs/jobs.go");
  const src = read("../../public/xs-jobs.js");
  for (const r of ['"/jobs"', '"/jobs/:id"', '"/jobs/:id/retry"']) assert.ok(go.includes(r), r);
  assert.ok(go.includes('r.DELETE("/jobs/:id"'));
  for (const r of ['"/jobs"', '"/jobs/"', '"/retry"']) assert.ok(src.includes(r), r);
  for (const s of jobs.matchAll(/Status\w+\s+= "(\w+)"/g)) assert.ok(new RegExp("\\b" + s[1] + ": \\[").test(src), "status label " + s[1]);
});

test("settings registers the jobs tab after backup", () => {
  const src = read("../../public/xs-platform-settings.js");
  assert.ok(/\{ k: "backup", t: "平台备份" \},\n\s*\{ k: "jobs", t: "后台任务" \}/.test(src));
  assert.ok(src.includes('tab === "jobs"') && src.includes("root.XSJobs.render(body)"));
});

test("list shows Chinese columns, statuses, attempts, escaped errors and polls while active", async () => {
  const h = setup();
  await mount(h);
  assert.deepEqual(plain(h.calls[0].options.query), { page: 1, page_size: 20 });
  const t = text(h.el);
  for (const s of ["后台任务", "排队中", "运行中", "失败", "死信", "成功", "尝试次数", "下次执行", "最近错误", "创建时间", "2 / 5", "连接超时"]) assert.ok(t.includes(s), s);
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  assert.ok(!/\/jobs|api\/v1|payload|dead letter|queued/i.test(t), "no developer jargon");
  assert.equal(h.timers.size, 1, "queued/running jobs schedule a refresh");
  await h.runTimer();
  assert.equal(h.calls.filter((c) => c.path === "/jobs").length, 2);
});

test("retry only for failed/dead, delete only for dead/succeeded", async () => {
  const h = setup();
  await mount(h);
  assert.ok(btn(h, "retry", 4) && btn(h, "retry", 3));
  assert.ok(!btn(h, "retry", 5) && !btn(h, "retry", 2) && !btn(h, "retry", 1));
  assert.ok(btn(h, "del", 3) && btn(h, "del", 2));
  assert.ok(!btn(h, "del", 4) && !btn(h, "del", 5) && !btn(h, "del", 1));
});

test("read-only users get no retry or delete", async () => {
  const h = setup({ perms: ["setting:read"] });
  await mount(h);
  assert.equal(h.el.querySelectorAll('[data-jobs-click="retry"]').length, 0);
  assert.equal(h.el.querySelectorAll('[data-jobs-click="del"]').length, 0);
});

test("status and type filters are sent to the server", async () => {
  const h = setup();
  await mount(h);
  h.el.querySelector("[data-jobs-type]").value = "  backup.run ";
  await h.emit("change", "[data-jobs-status]", "failed,dead");
  await tick();
  const last = h.calls.filter((c) => c.path === "/jobs").at(-1);
  assert.deepEqual(plain(last.options.query), { page: 1, page_size: 20, status: "failed,dead", type: "backup.run" });
  await h.emit("click", '[data-jobs-click="clear"]'); await tick();
  assert.deepEqual(plain(h.calls.filter((c) => c.path === "/jobs").at(-1).options.query), { page: 1, page_size: 20 });
});

test("pagination requests next page", async () => {
  const h = setup({ total: 45 });
  await mount(h);
  assert.ok(text(h.el).includes("1 / 3"));
  await h.emit("click", '[data-jobs-click="next"]'); await tick();
  assert.equal(h.calls.filter((c) => c.path === "/jobs").at(-1).options.query.page, 2);
});

test("detail drawer shows redacted parameters and full error", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", btn(h, "open", 4)); await tick();
  assert.ok(h.calls.some((c) => c.path === "/jobs/4" && !c.options.method));
  const pre = h.el.querySelector("[data-jobs-payload]");
  assert.ok(pre);
  const p = text(pre);
  assert.ok(p.includes('"target": "pg"'));
  assert.ok(!p.includes("leaked-value"), "client-side redaction backs up server");
  assert.ok(p.includes('"api_key": "***"') && p.includes('"token": "***"'));
  assert.ok(text(h.el.querySelector("[data-jobs-lasterror]")).includes("<img src=x"));
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  assert.ok(text(h.el.querySelector("[data-jobs-drawer]")).includes("任务参数"));
  await h.emit("click", '[data-jobs-click="closeDrawer"]');
  assert.equal(h.el.querySelector("[data-jobs-drawer]"), null);
});

test("retry posts and reloads", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", btn(h, "retry", 3)); await tick();
  assert.ok(h.calls.some((c) => c.path === "/jobs/3/retry" && c.options.method === "POST"));
  assert.ok(h.toasts.some((m) => m.includes("已重新排队")));
});

test("retry conflict shows Chinese error", async () => {
  const h = setup({ extra: (path, opts) => { if (path === "/jobs/4/retry") throw Object.assign(new Error("已有相同去重键的未完成任务，无法重试"), { status: 409 }); } });
  await mount(h);
  await h.emit("click", btn(h, "retry", 4)); await tick();
  assert.ok(h.toasts.some((m) => m.startsWith("重试失败：已有相同")));
});

test("delete requires typing the job number", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", btn(h, "del", 3));
  assert.ok(h.el.querySelector("[data-jobs-confirm]"));
  await h.emit("click", '[data-jobs-click="confirmDel"]');
  assert.ok(text(h.el).includes("请输入任务编号「3」确认删除"));
  assert.ok(!h.calls.some((c) => c.options.method === "DELETE"));
  h.el.querySelector("[data-jobs-confirm]").value = "2";
  await h.emit("click", '[data-jobs-click="confirmDel"]');
  assert.ok(!h.calls.some((c) => c.options.method === "DELETE"));
  h.el.querySelector("[data-jobs-confirm]").value = "3";
  await h.emit("click", '[data-jobs-click="confirmDel"]'); await tick();
  assert.ok(h.calls.some((c) => c.path === "/jobs/3" && c.options.method === "DELETE"));
  assert.equal(h.el.querySelector("[data-jobs-confirm]"), null);
  assert.ok(h.toasts.some((m) => m.includes("已删除")));
});

test("load error is shown in Chinese", async () => {
  const h = setup({ extra: (path) => { if (path === "/jobs") throw new Error("没有权限"); } });
  await mount(h);
  assert.ok(text(h.el.querySelector("[data-jobs-error]")).includes("没有权限"));
});
