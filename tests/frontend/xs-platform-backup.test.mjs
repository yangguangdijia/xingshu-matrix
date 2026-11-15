import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { webcrypto, createHash } from "node:crypto";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const DUMP = Buffer.from("PGDMP-test-dump-bytes");
const SHA = createHash("sha256").update(DUMP).digest("hex");

const rows = () => [
  { id: 3, kind: "manual", status: "running", file: "", size: 0, sha256: "", error: "", created_at: "2026-11-14T10:00:00Z", finished_at: null },
  { id: 2, kind: "scheduled", status: "succeeded", file: "xs-20261114T020000Z.dump", size: 5 * 1024 * 1024, sha256: SHA, error: "", created_at: "2026-11-14T02:00:00Z", finished_at: "2026-11-14T02:01:05Z" },
  { id: 1, kind: "pre_migrate", status: "failed", file: "", size: 0, sha256: "", error: XSS, created_at: "2026-11-13T02:00:00Z", finished_at: "2026-11-13T02:00:01Z" }
];
const POLICY = { schedule_enabled: true, schedule: "0 2 * * *", keep: 7, before_migrate: true, before_migrate_on_failure: "abort", storage: "local", online_restore: false };

function setup({ perms = ["setting:read", "setting:manage"], policy = POLICY, items = rows(), extra } = {}) {
  const h = harness("xs-platform-backup.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (extra) { const r = await extra(path, opts); if (r !== undefined) return r; }
      if (path === "/platform/backups" && !opts.method) return { items, total: items.length, page: 1, page_size: 20 };
      if (path === "/platform/backup-policy") {
        if (!policy) { const e = new Error("not found"); e.status = 404; throw e; }
        return policy;
      }
      if (path === "/platform/backups" && opts.method === "POST") return { id: 4, kind: "manual", status: "running" };
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [];
  h.window.XS.toast = (m) => toasts.push(m);
  return { ...h, toasts };
}
async function mount(h) { h.window.XSPlatformBackup.render(h.el); await tick(); await tick(); }

test("frontend paths match backend registrations", () => {
  const go = read("../../server/internal/backup/api.go");
  const src = read("../../public/xs-platform-backup.js");
  for (const r of ['"/platform/backups"', '"/platform/backups/:id/download"', '"/platform/backup-policy"']) assert.ok(go.includes(r), r);
  for (const r of ['"/platform/backups"', '"/platform/backup-policy"', '"/download"']) assert.ok(src.includes(r), r);
  assert.match(go, /PermRead\s+= "setting:read"/);
  assert.match(go, /PermManage = "setting:manage"/);
  assert.ok(src.includes('can("setting:manage")'));
});

test("list shows kind, time, size, status and escapes errors", async () => {
  const h = setup();
  await mount(h);
  const t = text(h.el);
  for (const s of ["手动备份", "定时备份", "升级前备份", "进行中", "成功", "失败", "5.0 MB", "1 分 5 秒", "共 3 条"]) assert.ok(t.includes(s), s);
  assert.ok(!h.el.innerHTML.includes("<img src=x"), "error text must be escaped");
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  // policy (read-only)
  for (const s of ["每天 02:00", "保留最近 7 份成功的定时备份", "中止升级", "服务器本地备份目录", "离线恢复", "部署配置"]) assert.ok(t.includes(s), s);
  // download only for succeeded rows; no restore button while online restore is off
  assert.equal(h.el.querySelectorAll('[data-pbk-click="download"]').length, 1);
  assert.equal(h.el.querySelector('[data-pbk-click="download"]').getAttribute("data-id"), "2");
  assert.equal(h.el.querySelectorAll('[data-pbk-click="restore"]').length, 0);
  // no developer jargon in the UI
  assert.ok(!/\/platform\/|api\/v1|cron/i.test(t));
  // a running row schedules a quiet refresh
  assert.equal(h.timers.size, 1);
  const before = h.calls.length;
  await h.runTimer(); await tick();
  assert.ok(h.calls.slice(before).some((c) => c.path === "/platform/backups"));
});

test("read-only users see no create or download", async () => {
  const h = setup({ perms: ["setting:read"] });
  await mount(h);
  assert.equal(h.el.querySelector('[data-pbk-click="create"]'), null);
  assert.equal(h.el.querySelectorAll('[data-pbk-click="download"]').length, 0);
});

test("立即备份 posts and reloads; busy error is shown in Chinese", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-pbk-click="create"]'); await tick();
  assert.ok(h.calls.some((c) => c.path === "/platform/backups" && c.options.method === "POST"));
  assert.ok(h.toasts.some((m) => m.includes("备份已开始")));

  const h2 = setup({ extra: async (p, o) => { if (o.method === "POST") throw Object.assign(new Error("已有平台备份正在进行，请稍后再试"), { status: 409 }); } });
  await mount(h2);
  await h2.emit("click", '[data-pbk-click="create"]'); await tick();
  assert.ok(h2.toasts.some((m) => m === "发起备份失败：已有平台备份正在进行，请稍后再试"));
  assert.equal(h2.el.querySelector('[data-pbk-click="create"]').disabled, false);
});

test("download sends bearer token, verifies SHA-256 and saves with the backup file name", async () => {
  const h = setup();
  const saved = [], fetched = [];
  Object.assign(h.window.XS, { token: () => "tok-abc", base: "/api/v1" });
  h.window.crypto = webcrypto;
  h.window.fetch = async (url, init) => {
    fetched.push({ url, init });
    return {
      ok: true, status: 200,
      headers: { get: (k) => (k === "X-Content-SHA256" ? SHA : null) },
      blob: async () => ({ size: DUMP.length, arrayBuffer: async () => DUMP.buffer.slice(DUMP.byteOffset, DUMP.byteOffset + DUMP.length) })
    };
  };
  await mount(h);
  h.window.XSPlatformBackup._save = (blob, name) => saved.push(name);
  await h.emit("click", '[data-pbk-click="download"]'); await tick(); await tick();
  assert.equal(fetched[0].url, "/api/v1/platform/backups/2/download");
  assert.equal(fetched[0].init.headers.Authorization, "Bearer tok-abc");
  assert.deepEqual(saved, ["xs-20261114T020000Z.dump"]);
  assert.ok(h.toasts.includes("下载完成，文件校验通过"));
});

test("download rejects a corrupted file and reports server errors", async () => {
  const h = setup();
  const saved = [];
  h.window.crypto = webcrypto;
  h.window.fetch = async () => ({ ok: true, status: 200, headers: { get: () => "00".repeat(32) }, blob: async () => ({ size: 3, arrayBuffer: async () => new Uint8Array([1, 2, 3]).buffer }) });
  await mount(h);
  h.window.XSPlatformBackup._save = (b, n) => saved.push(n);
  await h.emit("click", '[data-pbk-click="download"]'); await tick(); await tick();
  assert.equal(saved.length, 0);
  assert.ok(h.toasts.some((m) => m.includes("校验值与备份记录不一致")));

  h.window.fetch = async () => ({ ok: false, status: 404, json: async () => ({ code: 40400, message: "备份文件已不存在" }) });
  await h.emit("click", '[data-pbk-click="download"]'); await tick(); await tick();
  assert.ok(h.toasts.includes("备份文件已不存在"));
});

test("online restore requires typing 恢复 and warns about overwriting", async () => {
  let restoreBody = null;
  const h = setup({
    policy: { ...POLICY, online_restore: true },
    extra: async (p, o) => { if (p === "/platform/backups/2/restore") { restoreBody = o.body; return { id: 2 }; } }
  });
  await mount(h);
  const btns = h.el.querySelectorAll('[data-pbk-click="restore"]');
  assert.equal(btns.length, 1, "only succeeded backups can be restored");
  await h.emit("click", btns[0]);
  assert.ok(text(h.el).includes("覆盖平台当前的全部数据"));
  await h.emit("click", '[data-pbk-click="confirmRestore"]');
  assert.equal(restoreBody, null);
  assert.ok(text(h.el).includes("请输入「恢复」二字确认"));
  h.el.querySelector("[data-pbk-confirm]").value = "恢 复";
  await h.emit("click", '[data-pbk-click="confirmRestore"]');
  assert.equal(restoreBody, null);
  h.el.querySelector("[data-pbk-confirm]").value = "恢复";
  await h.emit("click", '[data-pbk-click="confirmRestore"]'); await tick();
  assert.deepEqual(JSON.parse(JSON.stringify(restoreBody)), { confirm: "恢复" });
  assert.equal(h.el.querySelector("[data-pbk-mask]"), null);
});

test("older backend without policy endpoint still lists backups", async () => {
  const h = setup({ policy: null });
  await mount(h);
  assert.ok(text(h.el).includes("备份记录"));
  assert.ok(!text(h.el).includes("备份策略"));
});

test("load failure shows Chinese error", async () => {
  const h = setup({ extra: async (p, o) => { if (p === "/platform/backups" && !o.method) throw new Error("没有权限访问该资源"); } });
  await mount(h);
  assert.ok(text(h.el.querySelector("[data-pbk-error]")).includes("没有权限访问该资源"));
});

test("cron descriptions", async () => {
  const h = setup();
  const d = h.window.XSPlatformBackup.describeCron;
  assert.equal(d("0 2 * * *"), "每天 02:00");
  assert.equal(d("30 3 * * 0"), "每周日 03:30");
  assert.equal(d("0 4 1 * *"), "每月 1 日 04:00");
  assert.equal(d("15 * * * *"), "每小时第 15 分");
  assert.equal(d("*/5 1-3 * * *"), "按计划「*/5 1-3 * * *」");
});
