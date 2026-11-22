import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;

const TRACE = () => ({
  item: { type: "requirement", id: 12, title: "SSO", status: "testing" },
  stages: [
    { key: "code", state: "done", at: "2026-11-20T08:00:00Z", items: [
      { kind: "commit", ref: "9f3c2a71d0b4e5f6a7b8", title: XSS, url: "javascript:alert(1)", author: "zhangwei", occurred_at: "2026-11-20T08:00:00Z", source: "auto" },
      { id: 77, kind: "mr", ref: "!12", title: "手动 MR", url: "https://git.example.com/mr/12", author: "lisi", source: "manual" }
    ] },
    { key: "build", state: "failed", at: "2026-11-20T09:00:00Z", items: [{ kind: "run", ref: "412", title: "web-ci #412", pipeline_id: 5, status: "failed", source: "auto" }] },
    { key: "artifact", state: "none", at: null, items: [] },
    { key: "release", state: "running", at: "2026-11-21T09:00:00Z", items: [{ kind: "release", ref: "91", title: "web → 预发", env: "pre", app_id: 3, status: "running", source: "auto" }] },
    { key: "env", state: "none", items: [] }
  ],
  envs: [
    { env: "test", app_id: 3, app_name: "web", release_id: 88, image: "harbor/web:1.8.0", at: "2026-11-21T08:00:00Z" },
    { env: "pre", app_id: 3, app_name: "web", release_id: 91, image: "harbor/web:1.8.0", at: "2026-11-21T09:00:00Z" },
    { env: "prod", app_id: 3, app_name: XSS, release_id: 80, image: "harbor/web:1.7.2", at: "2026-11-18T09:00:00Z" }
  ]
});

function setup({ perms = ["devcenter:read", "devcenter:manage"], mode = "live", api } = {}) {
  const h = harness("xs-devtrace.js", {
    permissions: perms,
    api: api || (async (p, o = {}) => {
      if (p === "/devcenter/requirements/12/trace") return TRACE();
      if (p === "/devcenter/bugs/3/trace") return { ...TRACE(), stages: [], envs: [] };
      if (p === "/devcenter/requirements/12/links" && o.method === "POST") return { id: 99 };
      if (p === "/devcenter/links/77" && o.method === "DELETE") return {};
      throw new Error("unexpected " + p);
    })
  });
  h.window.XS.mode = mode;
  const toasts = [];
  h.window.XS.toast = (m) => toasts.push(m);
  return { ...h, toasts };
}
async function mount(h, type = "requirement", id = 12) { const c = h.window.XSDevTrace.render(h.el, { type, id }); await c.done; await tick(); return c; }

test("trace renders five stages with four-colour states, env chips and escaped items", async () => {
  const h = setup();
  await mount(h);
  assert.equal(h.calls[0].path, "/devcenter/requirements/12/trace");
  const t = text(h.el);
  for (const s of ["代码", "构建", "制品", "发布", "环境", "已完成", "失败", "进行中", "未开始", "测试", "预发", "生产", "各环境当前版本", "REQ-12", "即可自动关联"]) assert.ok(t.includes(s), s);
  assert.equal(h.el.querySelectorAll(".xdt-stage").length, 5);
  assert.ok(h.el.innerHTML.includes("#16a34a") && h.el.innerHTML.includes("#dc2626") && h.el.innerHTML.includes("#d97706") && h.el.innerHTML.includes("#6b7280"));
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  // env chip links to the app page
  assert.ok(h.el.querySelectorAll(".xdt-chip").some((c) => c.getAttribute("href") === "#/delivery/web/" + encodeURIComponent("部署编排")));
  assert.ok(!/\/devcenter\/|api\/v1/.test(t), "no developer paths");
});

test("stage expands to list items with safe links; manual item shows remove", async () => {
  const h = setup();
  await mount(h);
  assert.equal(h.el.querySelector(".xdt-detail"), null);
  await h.emit("click", '[data-stage="code"]');
  const rows = h.el.querySelector(".xdt-detail").querySelectorAll("li");
  assert.equal(rows.length, 2);
  assert.ok(!h.el.innerHTML.includes("javascript:"), "unsafe url dropped");
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  assert.ok(text(h.el).includes("9f3c2a71"));
  const ext = h.el.querySelectorAll(".xdt-title").find((a) => a.getAttribute("href") === "https://git.example.com/mr/12");
  assert.ok(ext && ext.getAttribute("rel").includes("noopener"));
  assert.equal(h.el.querySelectorAll('[data-xdt="del"]').length, 1, "only manual link removable");
  await h.emit("click", '[data-stage="build"]');
  assert.ok(h.el.querySelectorAll(".xdt-title").some((a) => a.getAttribute("href") === "#/cicd/pipeline/5"), "run links to pipeline detail");
  await h.emit("click", '[data-stage="code"]');
  assert.equal(h.el.querySelectorAll(".xdt-detail").length, 1, "collapse works");
});

test("read-only users see no add or remove controls", async () => {
  const h = setup({ perms: ["devcenter:read"] });
  await mount(h);
  await h.emit("click", '[data-stage="code"]');
  assert.equal(h.el.querySelector('[data-xdt="add"]'), null);
  assert.equal(h.el.querySelector('[data-xdt="del"]'), null);
});

test("manual link form validates in place and posts kind/ref/title/url", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-xdt="add"]');
  assert.ok(h.el.querySelector("[data-xdt-form]"));
  await h.emit("click", '[data-xdt="save-add"]');
  assert.ok(text(h.el).includes("请填写编号"));
  h.el.querySelector('[data-f="ref"]').value = "abc123";
  h.el.querySelector('[data-f="url"]').value = "javascript:alert(1)";
  await h.emit("click", '[data-xdt="save-add"]');
  assert.ok(text(h.el).includes("链接需以"));
  assert.equal(h.calls.filter((c) => c.options.method === "POST").length, 0);
  h.el.querySelector('[data-f="kind"]').value = "mr";
  h.el.querySelector('[data-f="url"]').value = "https://git.example.com/mr/3";
  h.el.querySelector('[data-f="title"]').value = "修复";
  await h.emit("click", '[data-xdt="save-add"]');
  await tick();
  const post = h.calls.find((c) => c.options.method === "POST");
  assert.equal(post.path, "/devcenter/requirements/12/links");
  assert.deepEqual(JSON.parse(JSON.stringify(post.options.body)), { kind: "mr", ref: "abc123", title: "修复", url: "https://git.example.com/mr/3" });
  assert.ok(h.toasts.includes("已关联"));
  assert.equal(h.el.querySelector("[data-xdt-form]"), null);
});

test("removing a manual link needs confirmation", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-stage="code"]');
  await h.emit("click", '[data-xdt="del"]');
  assert.ok(text(h.el).includes("确定移除"));
  assert.equal(h.calls.filter((c) => c.options.method === "DELETE").length, 0);
  await h.emit("click", '[data-xdt="del-no"]');
  assert.equal(h.el.querySelector('[data-xdt="del-yes"]'), null);
  await h.emit("click", '[data-xdt="del"]');
  await h.emit("click", '[data-xdt="del-yes"]');
  await tick();
  const del = h.calls.find((c) => c.options.method === "DELETE");
  assert.equal(del.path, "/devcenter/links/77");
  assert.ok(h.toasts.includes("已移除关联"));
});

test("bug trace uses BUG key and empty state; 404 degrades to short notice", async () => {
  const h = setup();
  await mount(h, "bug", 3);
  assert.equal(h.calls[0].path, "/devcenter/bugs/3/trace");
  const t = text(h.el);
  assert.ok(t.includes("BUG-3") && t.includes("还没有关联的代码、构建或发布") && t.includes("还没有发布到任何环境"));
  const h2 = setup({ api: async () => { throw Object.assign(new Error("not found"), { status: 404 }); } });
  await mount(h2);
  assert.ok(text(h2.el).includes("交付进度暂不可用"));
});

test("demo mode uses built-in data without requests and supports manual links", async () => {
  const h = setup({ mode: "demo" });
  await mount(h, "requirement", 5);
  assert.equal(h.calls.length, 0);
  assert.ok(text(h.el).includes("REQ-5"));
  await h.emit("click", '[data-xdt="add"]');
  h.el.querySelector('[data-f="ref"]').value = "deadbeef";
  await h.emit("click", '[data-xdt="save-add"]'); await tick();
  await h.emit("click", '[data-stage="code"]');
  assert.ok(text(h.el).includes("deadbeef"));
  assert.equal(h.el.querySelectorAll('[data-xdt="del"]').length, 1);
  await h.emit("click", '[data-xdt="del"]');
  await h.emit("click", '[data-xdt="del-yes"]'); await tick();
  assert.ok(!text(h.el).includes("deadbeef"));
  assert.equal(h.calls.length, 0);
});

test("release items render requirements, bugs, commits and safe release notes", async () => {
  const h = setup({ api: async (p) => {
    if (p === "/devcenter/releases/91/items") return {
      release: { id: 91, app_id: 3, app_name: "web", env: "prod", image: "harbor/web:1.8.0", status: "succeeded", created_at: "2026-11-21T09:00:00Z" },
      requirements: [{ id: 12, title: XSS, status: "testing" }], bugs: [{ id: 3, title: "分页", status: "fixed" }],
      commits: [{ sha: "9f3c2a71d0b4e5f6a7b8", title: "feat: REQ-12", author: "zhangwei" }],
      notes_md: "## 说明\n- REQ-12 <script>alert(1)</script>\n- [危险](javascript:alert(1)) [文档](https://docs.example.com/a?b=1&c=2)\n\n`code`"
    };
    throw new Error("unexpected " + p);
  } });
  await h.window.XSDevTrace.releaseItems(h.el, 91).done; await tick();
  const t = text(h.el);
  for (const s of ["包含需求", "修复缺陷", "提交", "发布说明", "REQ-12", "BUG-3", "9f3c2a71", "生产", "测试中", "已修复", "复制 Markdown"]) assert.ok(t.includes(s), s);
  assert.ok(!h.el.innerHTML.includes("<script>") && !h.el.innerHTML.includes("<img src=x"));
  assert.ok(!h.el.innerHTML.includes('href="javascript'));
  assert.ok(h.el.innerHTML.includes('href="https://docs.example.com/a?b=1&amp;c=2"'));
  assert.ok(h.el.querySelector("h5") && h.el.innerHTML.includes("<code>code</code>"));
});

test("release items empty and failure states", async () => {
  const h = setup({ api: async () => ({ release: { id: 4 }, requirements: [], bugs: [], commits: [], notes_md: "" }) });
  await h.window.XSDevTrace.releaseItems(h.el, 4).done; await tick();
  assert.ok(text(h.el).includes("没有识别到关联的需求或缺陷"));
  const h2 = setup({ api: async () => { throw Object.assign(new Error("x"), { status: 403 }); } });
  await h2.window.XSDevTrace.releaseItems(h2.el, 4).done; await tick();
  assert.ok(text(h2.el).includes("没有查看发布内容的权限"));
  const h3 = setup({ mode: "demo" });
  await h3.window.XSDevTrace.releaseItems(h3.el, 7).done; await tick();
  assert.equal(h3.calls.length, 0);
  assert.ok(text(h3.el).includes("REQ-12"));
});

test("md, keyOf and safeUrl helpers", () => {
  const h = setup();
  const X = h.window.XSDevTrace;
  assert.equal(X.keyOf("bugs", 3), "BUG-3");
  assert.equal(X.keyOf("requirement", "12"), "REQ-12");
  assert.ok(X.keyChip("requirement", 12).includes('data-text="REQ-12"'));
  assert.equal(X._safeUrl("javascript:alert(1)"), "");
  assert.equal(X._safeUrl("#/cicd/pipeline/5"), "#/cicd/pipeline/5");
  const html = X.md("# 标题\n1. 一\n2. 二\n```\n<b>x</b>\n```\n**粗**");
  assert.ok(html.includes("<h4>标题</h4>") && html.includes("<ol><li>一</li><li>二</li></ol>") && html.includes("&lt;b&gt;x&lt;/b&gt;") && html.includes("<b>粗</b>"));
});

test("integration is wired into devcenter drawer, release lists and index.html", () => {
  const pub = new URL("../../public/", import.meta.url);
  const dc = fs.readFileSync(new URL("xs-devcenter.js", pub), "utf8");
  assert.ok(dc.includes("XSDevTrace.render(") && dc.includes("XSDevTrace.keyChip("));
  const ops = fs.readFileSync(new URL("xs-live-ops.js", pub), "utf8");
  assert.ok(ops.includes("包含需求") && ops.includes("XSDevTrace.openRelease("));
  const html = fs.readFileSync(new URL("index.html", pub), "utf8");
  assert.ok(html.includes('src="xs-devtrace.js"'));
});
