import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const BASE = "/observability/certificates";

const SETTINGS = {
  enabled: true, interval_hours: 6, thresholds: [30, 14, 7, 1], last_scan_at: "2026-11-15T02:00:00Z",
  last_result: { trigger: "schedule", started_at: "2026-11-15T02:00:00Z", clusters: 2, secrets: 3, domains: 2, domain_errors: 1, notices: 1, cluster_errors: { "prod-b": "读取 TLS Secret 失败，请检查集群连接与 secrets 读取权限" } },
  updated_by: "admin", updated_at: "2026-11-14T02:00:00Z"
};
const certs = () => [
  { source: "secret", cluster_id: 1, cluster_name: "prod-a", env: "prod", namespace: "ingress", name: "web-tls", subject: "CN=web.example.com", issuer: "CN=R3", dns_names: "web.example.com,api.example.com", not_after: "2026-11-10T00:00:00Z", scanned_at: "2026-11-15T02:00:00Z", days_left: -5, status: "expired" },
  { source: "secret", cluster_id: 1, cluster_name: "prod-a", env: "prod", namespace: "default", name: "admin-tls", subject: "", issuer: "", dns_names: "", not_after: "2026-11-20T00:00:00Z", scanned_at: "2026-11-15T02:00:00Z", days_left: 4, status: "critical" },
  { source: "domain", cluster_id: 0, cluster_name: "", env: "", namespace: "", name: "shop.example.com:443", subject: "", issuer: "CN=GTS", dns_names: "shop.example.com", not_after: "2026-12-05T00:00:00Z", scanned_at: "2026-11-15T02:00:00Z", days_left: 20, status: "warning" },
  { source: "domain", cluster_id: 0, cluster_name: "", env: "test", namespace: "", name: "bad.example.com:443", error: XSS, not_after: null, scanned_at: "2026-11-15T02:00:00Z", days_left: null, status: "error" }
];
const COUNTS = { expired: 1, critical: 1, warning: 1, ok: 0, error: 1 };
const domains = () => [
  { id: 7, host: "shop.example.com", port: 443, env: "", remark: XSS, created_by: "admin", created_at: "2026-11-14T02:00:00Z" },
  { id: 8, host: "bad.example.com", port: 443, env: "test", remark: "", created_by: "ops", created_at: "2026-11-14T03:00:00Z" }
];

function setup({ perms = ["alert:read", "notify:manage"], extra } = {}) {
  const h = harness("xs-cert-watch.js", {
    permissions: perms,
    api: async (path, opts = {}) => {
      if (extra) { const r = await extra(path, opts); if (r !== undefined) return r; }
      const m = opts.method || "GET";
      if (path === BASE && m === "GET") return { items: certs(), counts: COUNTS, settings: SETTINGS };
      if (path === BASE + "/domains" && m === "GET") return domains();
      if (path === "/clusters") return { items: [{ id: 1, name: "prod-a", env: "prod" }, { id: 2, name: "t", env: "test" }] };
      if (path === BASE + "/scan" && m === "POST") return { trigger: "manual", clusters: 2, secrets: 3, domains: 2, domain_errors: 0, notices: 2 };
      if (path === BASE + "/domains" && m === "POST") return { domain: { id: 9, ...opts.body }, probe: { name: "x", not_after: "2027-01-01T00:00:00Z" } };
      if (path.startsWith(BASE + "/domains/") && m === "DELETE") return { deleted: +path.split("/").pop() };
      if (path === BASE + "/settings" && m === "PUT") return { ...SETTINGS, ...opts.body };
      throw new Error("unexpected " + m + " " + path);
    }
  });
  const toasts = [];
  h.window.XS.toast = (msg) => toasts.push(msg);
  return { ...h, toasts };
}
async function mount(h) { h.window.XSCertWatch.render(h.el); await tick(); await tick(); await tick(); }
const btn = (h, action, id) => h.el.querySelectorAll(`[data-cw-click="${action}"]`).find((n) => (id === undefined ? true : n.getAttribute(id[0]) === id[1]));
const callsTo = (h, path, method = "GET") => h.calls.filter((c) => c.path === path && (c.options.method || "GET") === method);

test("frontend endpoints and permissions match backend registrations", () => {
  const go = read("../../server/internal/modules/observability/cert_watch.go");
  const mod = read("../../server/internal/modules/observability/module.go");
  const src = read("../../public/xs-cert-watch.js");
  for (const r of ['g.Group("/certificates")', 'cg.GET("", rbac.RequirePerm(PermAlertRead)', 'cg.POST("/scan", rbac.RequirePerm(PermNotify)',
    'cg.GET("/settings", rbac.RequirePerm(PermAlertRead)', 'cg.PUT("/settings", rbac.RequirePerm(PermNotify)',
    'cg.GET("/domains", rbac.RequirePerm(PermAlertRead)', 'cg.POST("/domains", rbac.RequirePerm(PermNotify)', 'cg.DELETE("/domains/:id", rbac.RequirePerm(PermNotify)']) assert.ok(go.includes(r), r);
  assert.match(mod, /PermAlertRead\s*= "alert:read"/);
  assert.match(mod, /PermNotify\s*= "notify:manage"/);
  for (const f of ['json:"days_left"', 'json:"not_after"', 'json:"cluster_name"', 'json:"dns_names"', 'json:"interval_hours"', 'json:"thresholds"', 'json:"last_result"', 'json:"cluster_errors,omitempty"']) assert.ok(go.includes(f), f);
  assert.ok(src.includes('"/observability/certificates"'));
  assert.ok(src.includes('can("notify:manage")'));
  for (const s of ['"expired"', '"critical"', '"warning"', '"ok"', '"error"']) assert.ok(go.includes(s) && src.includes(s), s);
});

test("list shows certificates with Chinese status, counts, escaping and no jargon", async () => {
  const h = setup();
  await mount(h);
  const t = text(h.el);
  for (const s of ["证书到期", "web-tls", "ingress", "prod-a", "web.example.com", "已过期 5 天", "4 天", "20 天", "已过期", "紧急", "即将到期", "读取失败", "外部域名", "shop.example.com:443", "每 6 小时自动扫描一次", "30 / 14 / 7 / 1", "prod-b"]) assert.ok(t.includes(s), s);
  assert.equal(h.el.querySelectorAll("[data-cw-row]").length, 4);
  assert.ok(text(h.el.querySelector('[data-cw-count="expired"]')).includes("1"));
  assert.ok(text(btn(h, "status", ["data-v", ""])).includes("4"));
  assert.ok(!h.el.innerHTML.includes("<img src=x"), "error and remark text must be escaped");
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  assert.ok(!/\/observability|api\/v1|certificates|notify:manage|alert:read/i.test(t), "no developer jargon");
  assert.ok(h.el.querySelector('[data-cw-click="scan"]'));
  assert.ok(h.el.querySelector("[data-cw-domain-form]"));
  assert.equal(h.el.querySelectorAll('[data-cw-click="delDomain"]').length, 2);
});

test("filters pass source, status and keyword to the list request", async () => {
  const h = setup();
  await mount(h);
  await h.emit("change", '[data-cw-filter="source"]', "domain"); await tick();
  let last = callsTo(h, BASE).at(-1);
  assert.deepEqual({ ...last.options.query }, { source: "domain" });
  h.el.querySelector("[data-cw-q]").value = "  shop ";
  await h.emit("click", '[data-cw-click="search"]'); await tick();
  last = callsTo(h, BASE).at(-1);
  assert.deepEqual({ ...last.options.query }, { source: "domain", q: "shop" });
  await h.emit("click", btn(h, "status", ["data-v", "warning"])); await tick();
  last = callsTo(h, BASE).at(-1);
  assert.deepEqual({ ...last.options.query }, { source: "domain", status: "warning", q: "shop" });
  await h.emit("click", '[data-cw-click="clear"]'); await tick();
  last = callsTo(h, BASE).at(-1);
  assert.deepEqual({ ...last.options.query }, {});
});

test("read-only users see the list but no scan, add, delete or edit", async () => {
  const h = setup({ perms: ["alert:read"] });
  await mount(h);
  assert.equal(h.el.querySelector('[data-cw-click="scan"]'), null);
  assert.equal(h.el.querySelector("[data-cw-domain-form]"), null);
  assert.equal(h.el.querySelectorAll('[data-cw-click="delDomain"]').length, 0);
  assert.equal(h.el.querySelector('[data-cw-click="editSettings"]'), null);
  assert.ok(text(h.el).includes("剩余 30、14、7、1 天"));
  assert.equal(callsTo(h, "/clusters").length, 0);
});

test("立即扫描 posts with a long timeout, reports a summary and shows busy errors in Chinese", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-cw-click="scan"]'); await tick(); await tick();
  const post = callsTo(h, BASE + "/scan", "POST");
  assert.equal(post.length, 1);
  assert.ok(post[0].options.timeoutMs >= 120000);
  assert.ok(h.toasts.some((m) => m.startsWith("扫描完成：2 个集群、3 张集群证书、2 个域名") && m.includes("发出提醒 2 条")));

  const h2 = setup({ extra: async (p, o) => { if (p === BASE + "/scan") throw Object.assign(new Error("已有证书扫描正在进行，请稍后再试"), { status: 409, code: 40900 }); } });
  await mount(h2);
  await h2.emit("click", '[data-cw-click="scan"]'); await tick();
  assert.ok(h2.toasts.includes("扫描失败：已有证书扫描正在进行，请稍后再试"));
  assert.equal(h2.el.querySelector('[data-cw-click="scan"]').disabled, false);
});

test("add domain validates in place and posts the normalized host and port", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-cw-click="addDomain"]');
  assert.ok(text(h.el.querySelector('[data-cw-field-error="host"]')).includes("请填写域名"));
  assert.equal(callsTo(h, BASE + "/domains", "POST").length, 0);

  h.el.querySelector('[name="host"]').value = "*.example.com";
  await h.emit("click", '[data-cw-click="addDomain"]');
  assert.ok(text(h.el.querySelector('[data-cw-field-error="host"]')).includes("通配符"));

  h.el.querySelector('[name="host"]').value = "bad_host!";
  await h.emit("click", '[data-cw-click="addDomain"]');
  assert.ok(text(h.el.querySelector('[data-cw-field-error="host"]')).includes("域名格式不正确"));
  assert.equal(h.el.querySelector('[name="host"]').value, "bad_host!", "input keeps what the user typed");

  h.el.querySelector('[name="host"]').value = "api.example.com:8443";
  h.el.querySelector('[name="port"]').value = "443";
  await h.emit("click", '[data-cw-click="addDomain"]');
  assert.ok(text(h.el.querySelector('[data-cw-field-error="port"]')).includes("不一致"));

  h.el.querySelector('[name="host"]').value = "https://API.Example.com:8443/path?x=1";
  h.el.querySelector('[name="port"]').value = "";
  h.el.querySelector('[name="env"]').value = "prod";
  h.el.querySelector('[name="remark"]').value = " 对外接口 ";
  await h.emit("click", '[data-cw-click="addDomain"]'); await tick(); await tick();
  const post = callsTo(h, BASE + "/domains", "POST");
  assert.equal(post.length, 1);
  assert.deepEqual({ ...post[0].options.body }, { host: "api.example.com", port: 8443, env: "prod", remark: "对外接口" });
  assert.ok(h.toasts.some((m) => m.startsWith("已添加 api.example.com:8443，证书到期时间")));
  assert.equal(h.el.querySelector('[name="host"]').value, "");
});

test("add domain shows server errors next to the form; probe failure is reported", async () => {
  const h = setup({ extra: async (p, o) => { if (p === BASE + "/domains" && o.method === "POST") throw Object.assign(new Error("该域名已在监测列表中"), { status: 409, code: 40900 }); } });
  await mount(h);
  h.el.querySelector('[name="host"]').value = "shop.example.com";
  await h.emit("click", '[data-cw-click="addDomain"]'); await tick();
  assert.ok(text(h.el.querySelector("[data-cw-form-error]")).includes("添加失败：该域名已在监测列表中"));

  const h2 = setup({ extra: async (p, o) => { if (p === BASE + "/domains" && o.method === "POST") return { domain: { id: 10 }, probe: { error: "域名无法解析" } }; } });
  await mount(h2);
  h2.el.querySelector('[name="host"]').value = "new.example.com";
  await h2.emit("click", '[data-cw-click="addDomain"]'); await tick();
  assert.ok(h2.toasts.includes("已添加 new.example.com，首次检测失败：域名无法解析"));
});

test("delete domain requires typing the domain", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", btn(h, "delDomain", ["data-id", "7"]));
  assert.ok(h.el.querySelector("[data-cw-mask]"));
  assert.ok(text(h.el).includes("shop.example.com:443"));
  await h.emit("click", '[data-cw-click="confirmDel"]');
  assert.equal(callsTo(h, BASE + "/domains/7", "DELETE").length, 0);
  assert.ok(text(h.el.querySelector("[data-cw-del-error]")).includes("shop.example.com"));
  h.el.querySelector("[data-cw-del-confirm]").value = "shop.example";
  await h.emit("click", '[data-cw-click="confirmDel"]');
  assert.equal(callsTo(h, BASE + "/domains/7", "DELETE").length, 0);
  await h.emit("click", '[data-cw-click="cancelDel"]');
  assert.equal(h.el.querySelector("[data-cw-mask]"), null);

  await h.emit("click", btn(h, "delDomain", ["data-id", "7"]));
  h.el.querySelector("[data-cw-del-confirm]").value = "shop.example.com";
  await h.emit("click", '[data-cw-click="confirmDel"]'); await tick(); await tick();
  assert.equal(callsTo(h, BASE + "/domains/7", "DELETE").length, 1);
  assert.equal(h.el.querySelector("[data-cw-mask]"), null);
  assert.ok(h.toasts.includes("已删除监测域名 shop.example.com"));
});

test("settings form validates interval and thresholds, then saves normalized values", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-cw-click="editSettings"]');
  assert.equal(h.el.querySelector('[name="thresholds"]').value, "30,14,7,1");
  h.el.querySelector('[name="interval_hours"]').value = "200";
  h.el.querySelector('[name="thresholds"]').value = "30,0";
  await h.emit("click", '[data-cw-click="saveSettings"]');
  assert.ok(text(h.el.querySelector('[data-cw-field-error="interval"]')).includes("1 到 168"));
  assert.ok(text(h.el.querySelector('[data-cw-field-error="thresholds"]')).includes("1 到 365"));
  assert.equal(callsTo(h, BASE + "/settings", "PUT").length, 0);

  h.el.querySelector('[name="interval_hours"]').value = "12";
  h.el.querySelector('[name="thresholds"]').value = "7，30, 7 3";
  h.el.querySelector("[data-cw-enabled]").checked = false;
  await h.emit("click", '[data-cw-click="saveSettings"]'); await tick(); await tick();
  const put = callsTo(h, BASE + "/settings", "PUT");
  assert.equal(put.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(put[0].options.body)), { enabled: false, interval_hours: 12, thresholds: [30, 7, 3] });
  assert.ok(h.toasts.includes("提醒设置已保存"));
  assert.equal(h.el.querySelector("[data-cw-settings-form]"), null);

  const h2 = setup({ extra: async (p, o) => { if (o.method === "PUT") throw Object.assign(new Error("修改证书提醒设置需要全部环境权限"), { status: 403, code: 40300 }); } });
  await mount(h2);
  await h2.emit("click", '[data-cw-click="editSettings"]');
  await h2.emit("click", '[data-cw-click="saveSettings"]'); await tick();
  assert.ok(text(h2.el.querySelector("[data-cw-settings-error]")).includes("保存失败：修改证书提醒设置需要全部环境权限"));
});

test("load failure shows a Chinese error", async () => {
  const h = setup({ extra: async (p, o) => { if (p === BASE && !o.method) throw new Error("没有权限访问该资源"); } });
  await mount(h);
  assert.ok(text(h.el.querySelector("[data-cw-error]")).includes("没有权限访问该资源"));
});

test("helpers", () => {
  const h = setup();
  const { normalizeDomain, parseThresholds, fmtDays } = h.window.XSCertWatch;
  assert.equal(normalizeDomain({ host: "Example.COM." }).host, "example.com");
  assert.equal(normalizeDomain({ host: "example.com" }).port, 443);
  assert.equal(normalizeDomain({ host: "10.0.0.1:8443" }).port, 8443);
  assert.equal(normalizeDomain({ host: "[::1]:9443" }).host, "::1");
  assert.ok(normalizeDomain({ host: "a.com", port: "70000" }).errors.port);
  assert.ok(normalizeDomain({ host: "a.com", remark: "字".repeat(129) }).errors.remark);
  assert.deepEqual([...parseThresholds("1,30,14,14").value], [30, 14, 1]);
  assert.ok(parseThresholds("").error);
  assert.ok(parseThresholds("1,2,3,4,5,6,7,8,9,10,11").error);
  assert.equal(fmtDays(0), "不足 1 天");
  assert.equal(fmtDays(-3), "已过期 3 天");
  assert.equal(fmtDays(null), "—");
});
