import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick, Element } from "./xs-resource-pages.harness.mjs";

// The shared harness Element lacks removeAttribute (used by the form error line).
if (!Element.prototype.removeAttribute) {
  Element.prototype.removeAttribute = function (k) { delete this.attrs[k]; if (k === "hidden") this.hidden = false; if (k === "disabled") this.disabled = false; };
}

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
// document-order text of a rendered fragment (the harness keeps parent text before child text)
const plain = (node) => node.innerHTML.replace(/<[^>]+>/g, "").replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">");
const ALL = ["audit:read", "audit:export", "audit:manage"];

function setup({ perms = ALL, verify } = {}) {
  const h = harness("xs-audit-chain.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (path === "/audit/chain/verify") return verify ? verify(opts.query || {}) : { checked: 0, ok: true, head_id: 0, last_id: 0 };
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [], fetches = [], saved = [];
  h.window.XS.toast = (m) => toasts.push(m);
  const io = h.window.XSAuditChain._io;
  io.fetch = async (url, init) => { fetches.push({ url, init }); return fakeResponse("{}\n"); };
  io.save = (blob, name) => saved.push({ blob, name });
  return { ...h, toasts, fetches, saved };
}
function fakeBlob(s) {
  return { size: Buffer.byteLength(s), slice: (a) => fakeBlob(s.slice(a)), text: async () => s };
}
function fakeResponse(body, { status = 200, json } = {}) {
  return {
    status, ok: status >= 200 && status < 300,
    headers: { get: (k) => (k.toLowerCase() === "content-disposition" ? 'attachment; filename="audit-20261101.jsonl"' : null) },
    blob: async () => fakeBlob(body),
    json: async () => json || {}
  };
}
const click = (h, name) => h.emit("click", `[data-xac="${name}"]`);

test("frontend paths and permissions match backend registrations", () => {
  const store = read("../../server/internal/audit/store.go");
  const exp = read("../../server/internal/audit/export.go");
  const src = read("../../public/xs-audit-chain.js");
  assert.ok(store.includes('r.GET("/audit/chain/verify", rbac.RequirePerm("audit:read")'));
  assert.ok(exp.includes('"/audit/export"') && exp.includes('"/audit/archive"'));
  assert.match(exp, /PermAuditExport = "audit:export"/);
  assert.match(exp, /PermAuditManage = "audit:manage"/);
  assert.match(exp, /exportErrorMarker = "export interrupted"/);
  for (const s of ['"/audit/chain/verify"', '"/audit/export?"', '"/audit/archive"', 'can("audit:read")', 'can("audit:export")', 'can("audit:manage")']) assert.ok(src.includes(s), s);
});

test("no permission: toolbar is not rendered", () => {
  const h = setup({ perms: [] });
  assert.equal(h.window.XSAuditChain.render(h.el), null);
  assert.equal(h.el.querySelector("[data-xac-root]"), null);
  assert.equal(h.el.querySelectorAll("[data-xac]").length, 0);
});

test("buttons follow permissions individually", () => {
  const only = (perms) => { const h = setup({ perms }); h.window.XSAuditChain.render(h.el); return ["verify", "export", "archive"].filter((n) => h.el.querySelector(`[data-xac="${n}"]`)); };
  assert.deepEqual(only(["audit:read"]), ["verify"]);
  assert.deepEqual(only(["audit:export"]), ["export"]);
  assert.deepEqual(only(["audit:manage"]), ["archive"]);
  assert.deepEqual(only(ALL), ["verify", "export", "archive"]);
  const h = setup();
  h.window.XSAuditChain.render(h.el);
  const t = text(h.el);
  assert.ok(t.includes("审计完整性") && t.includes("完整性校验"));
  assert.ok(!/\/audit\/|api\/v1|hash/i.test(t), "no developer jargon in the UI");
});

test("verify ok: shows 校验通过 with count and head id in Chinese", async () => {
  const h = setup({ verify: () => ({ checked: 1234, ok: true, head_id: 1300, last_id: 1300 }) });
  h.window.XSAuditChain.render(h.el);
  await click(h, "verify"); await tick();
  const res = h.el.querySelector("[data-xac-verdict]");
  assert.equal(res.getAttribute("data-xac-verdict"), "ok");
  const t = text(h.el.querySelector("[data-xac-result]"));
  assert.ok(t.includes("校验通过"), t);
  assert.ok(t.includes("共检查 1,234 条审计记录"), t);
  assert.ok(t.includes("已检查到最新记录 #1300"), t);
  assert.equal(h.el.querySelector('[data-xac="verify"]').disabled, false, "button re-enabled after verify");
});

test("verify continues in batches until the head", async () => {
  const queries = [];
  const h = setup({
    verify: (q) => {
      queries.push(q);
      return q.from_id ? { checked: 10, ok: true, head_id: 50010, last_id: 50010 } : { checked: 50000, ok: true, head_id: 50010, last_id: 50000 };
    }
  });
  const r = await h.window.XSAuditChain.verifyAll();
  assert.deepEqual(queries.map((q) => q.from_id || 0), [0, 50001]);
  assert.equal(r.ok, true);
  assert.equal(r.checked, 50010);
  assert.equal(r.partial, false);
});

test("verify break: shows 发现断点 with record id and Chinese reason", async () => {
  const cases = {
    hash_mismatch: "内容与写入时不一致",
    prev_mismatch: "缺失、被删除或顺序被改动",
    unchained: "尚未纳入防篡改保护",
    something_new: "校验未通过"
  };
  for (const [kind, want] of Object.entries(cases)) {
    const h = setup({ verify: () => ({ checked: 41, ok: false, break_at: 42, kind, head_id: 99, last_id: 41 }) });
    h.window.XSAuditChain.render(h.el);
    await click(h, "verify"); await tick();
    assert.equal(h.el.querySelector("[data-xac-verdict]").getAttribute("data-xac-verdict"), "broken");
    const t = plain(h.el.querySelector("[data-xac-result]"));
    assert.ok(t.includes("发现断点"), t);
    assert.ok(t.includes("记录编号 #42"), t);
    assert.ok(t.includes(want), kind + ": " + t);
    assert.ok(t.includes("已有 41 条记录校验通过"), t);
    assert.ok(!t.includes(kind), "raw kind code is not shown");
  }
});

test("verify empty and error are shown in Chinese", async () => {
  const h = setup();
  h.window.XSAuditChain.render(h.el);
  await click(h, "verify"); await tick();
  assert.ok(text(h.el).includes("暂无可校验的记录"));

  const e = setup({ verify: () => { const err = new Error("permission denied"); err.status = 403; throw err; } });
  e.window.XSAuditChain.render(e.el);
  await click(e, "verify"); await tick();
  const t = text(e.el.querySelector("[data-xac-result]"));
  assert.ok(t.includes("校验未完成") && t.includes("没有权限执行此操作"), t);
});

test("parseRange validates the export time range", () => {
  const api = setup().window.XSAuditChain;
  // results come from the vm realm; normalise before deep comparison
  const parseRange = (a, b) => JSON.parse(JSON.stringify(api.parseRange(a, b)));
  assert.deepEqual(parseRange("", "2026-11-02T00:00"), { error: "请选择开始时间", field: "from" });
  assert.deepEqual(parseRange("2026-11-01T00:00", ""), { error: "请选择结束时间", field: "to" });
  assert.deepEqual(parseRange("not-a-date", "2026-11-02T00:00"), { error: "开始时间格式不正确", field: "from" });
  assert.deepEqual(parseRange("2026-11-01T00:00", "bad"), { error: "结束时间格式不正确", field: "to" });
  assert.deepEqual(parseRange("2026-11-02T00:00", "2026-11-02T00:00"), { error: "结束时间需晚于开始时间", field: "to" });
  assert.deepEqual(parseRange("2026-11-02T00:00", "2026-11-01T00:00"), { error: "结束时间需晚于开始时间", field: "to" });
  assert.deepEqual(parseRange("2025-01-01T00:00", "2026-11-01T00:00"), { error: "时间段不能超过 366 天", field: "to" });
  const ok = api.parseRange("2026-11-01T00:00", "2026-11-08T00:00");
  assert.equal(ok.error, undefined);
  assert.equal(ok.from, new Date("2026-11-01T00:00").toISOString());
  assert.equal(ok.to, new Date("2026-11-08T00:00").toISOString());
});

test("export form: invalid range is reported inline and nothing is downloaded", async () => {
  const h = setup();
  h.window.XSAuditChain.render(h.el);
  await click(h, "export");
  const form = h.el.querySelector('[data-xac-form="export"]');
  assert.ok(form, "export form opens");
  assert.ok(form.querySelector('[name="from"]').value, "default range is filled");
  form.querySelector('[name="from"]').value = "2026-11-08T00:00";
  form.querySelector('[name="to"]').value = "2026-11-01T00:00";
  await click(h, "export-submit");
  const err = form.querySelector("[data-xac-err]");
  assert.equal(err.textContent, "结束时间需晚于开始时间");
  assert.equal(err.hidden, false);
  assert.equal(form.querySelector('[name="to"]').className, "xac-input xac-input--bad");
  assert.equal(form.querySelector('[name="to"]').focused, true);
  assert.equal(h.fetches.length, 0);

  form.querySelector('[name="from"]').value = "";
  await click(h, "export-submit");
  assert.equal(err.textContent, "请选择开始时间");
  assert.equal(h.fetches.length, 0);
});

test("export with a valid range downloads with token and shows a Chinese summary", async () => {
  const h = setup();
  h.window.XS.token = () => "tok";
  h.window.XSAuditChain.render(h.el);
  await click(h, "export");
  const form = h.el.querySelector('[data-xac-form="export"]');
  form.querySelector('[name="from"]').value = "2026-11-01T00:00";
  form.querySelector('[name="to"]').value = "2026-11-08T00:00";
  form.querySelector('[name="format"]').value = "csv";
  await click(h, "export-submit"); await tick();
  assert.equal(h.fetches.length, 1);
  const u = new URL(h.fetches[0].url, "http://x");
  assert.equal(u.pathname, "/api/v1/audit/export");
  assert.equal(u.searchParams.get("format"), "csv");
  assert.equal(u.searchParams.get("from"), new Date("2026-11-01T00:00").toISOString());
  assert.equal(h.fetches[0].init.headers.Authorization, "Bearer tok");
  assert.ok(!h.fetches[0].url.includes("tok"), "token never in the URL");
  assert.equal(h.saved.length, 1);
  assert.equal(h.saved[0].name, "audit-20261101.jsonl");
  const t = text(h.el.querySelector("[data-xac-result]"));
  assert.ok(t.includes("导出完成") && t.includes("2026-11-01 00:00 至 2026-11-08 00:00"), t);
  assert.equal(h.el.querySelector('[data-xac-form="export"]'), null, "form closes after success");
  assert.deepEqual(h.toasts, ["导出完成"]);
});

test("export: interrupted file and server errors are reported in Chinese", async () => {
  const h = setup();
  h.window.XSAuditChain.render(h.el);
  h.window.XSAuditChain._io.fetch = async () => fakeResponse('{"id":1}\n{"error":"export interrupted"}\n');
  await click(h, "export");
  await click(h, "export-submit"); await tick();
  assert.ok(text(h.el.querySelector("[data-xac-result]")).includes("导出不完整"));
  assert.deepEqual([...h.toasts], ["导出不完整，请重试"]);

  h.window.XSAuditChain._io.fetch = async () => fakeResponse("", { status: 400, json: { code: 40001, message: "range must not exceed 366 days" } });
  const form = h.el.querySelector('[data-xac-form="export"]');
  await click(h, "export-submit"); await tick();
  assert.equal(form.querySelector("[data-xac-err]").textContent, "时间段不能超过 366 天");
});
