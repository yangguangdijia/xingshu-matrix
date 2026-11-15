import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const plain = (o) => JSON.parse(JSON.stringify(o));
const XSS = `<img src=x onerror=alert(1)>"'&`;
const T = "2026-11-15T13:00:00Z";
const PIPES = [
  { id: 11, app_id: 3, name: "主干构建", stages: [{ name: "构建", type: "build" }, { name: "安全扫描", type: "scan", scan: { source: "trivy" } }, { name: "部署", type: "deploy" }] },
  { id: 12, app_id: 3, name: "仅部署", stages: [{ name: "部署", type: "deploy" }] }
];
const VULNS = [
  { id: "CVE-2024-0001", package: "openssl", installed: "3.0.1", fixed: "3.0.13", severity: "CRITICAL", title: XSS, url: "https://avd.example/CVE-2024-0001", blocking: true },
  { id: "CVE-2024-0002", package: "zlib", installed: "1.2.11", fixed: "", severity: "HIGH", title: "堆溢出", url: "javascript:alert(1)", ignored: true },
  { id: "CVE-2024-0003", package: "libxml2", installed: "2.9.1", fixed: "2.9.14", severity: "MEDIUM", title: "" },
  { id: "CVE-2024-0004", package: "bash", installed: "5.1", fixed: "5.2", severity: "LOW" }
];
const POLICY = { source: "trivy", block_severities: ["CRITICAL", "HIGH"], ignore_cves: ["CVE-2024-0002"], ignore_unfixed: true, top_n: 50 };
const scan = (o = {}) => ({ id: 21, run_id: 101, stage_idx: 1, image: "harbor.example.com/team/web:abc", source: "trivy", status: "blocked",
  summary: { critical: 1, high: 2, medium: 3, low: 4, unknown: 0, total: 10, ignored: 1, blocking: 1 }, policy: POLICY, vulnerabilities: VULNS, error: "", scanned_at: T, ...o });
const RUNS = [
  { id: 102, pipeline_id: 12, app_id: 3, status: "succeeded" },
  { id: 101, pipeline_id: 11, app_id: 3, status: "failed" },
  { id: 100, pipeline_id: 11, app_id: 3, status: "succeeded" }
];
const DETAILS = {
  101: { ...RUNS[1], scans: [scan()] },
  100: { ...RUNS[2], scans: [scan({ id: 20, run_id: 100, status: "error", summary: {}, vulnerabilities: [], error: "Harbor 中该镜像还没有扫描报告" + XSS })] }
};

function setup({ pipes = PIPES, runs = RUNS, details = DETAILS, total, extra } = {}) {
  const h = harness("xs-image-scan.js", {
    permissions: ["cicd:read"],
    api: async (path, opts = {}) => {
      if (extra) { const r = await extra(path, opts); if (r !== undefined) return r; }
      if (path === "/cicd/pipelines") return pipes;
      if (path === "/cicd/runs") return { items: runs, total: total ?? runs.length, page: 1, page_size: 10 };
      const m = path.match(/^\/cicd\/runs\/(\d+)$/);
      if (m && details[m[1]]) return details[m[1]];
      throw new Error("unexpected " + path);
    }
  });
  return h;
}
async function mount(h) { h.window.XSImageScan.render(h.el, { appId: 3 }); for (let i = 0; i < 5; i++) await tick(); }

test("frontend reads real backend fields and routes", () => {
  const scanGo = read("../../server/internal/modules/cicd/scan.go");
  const mod = read("../../server/internal/modules/cicd/module.go");
  const runs = read("../../server/internal/modules/cicd/runs.go");
  const src = read("../../public/xs-image-scan.js");
  for (const r of ['g.GET("/runs", read', 'g.GET("/runs/:id", read', 'g.GET("/pipelines", read']) assert.ok(mod.includes(r), r);
  assert.ok(runs.includes('json:"scans,omitempty"'));
  for (const f of ['json:"vulnerabilities"', 'json:"summary"', 'json:"policy"', 'json:"installed"', 'json:"fixed"', 'json:"package"', 'json:"block_severities,omitempty"', 'json:"blocking"']) assert.ok(scanGo.includes(f), f);
  for (const s of ["passed", "blocked", "error"]) assert.ok(new RegExp("\\b" + s + ": \\[").test(src), "status " + s);
  for (const s of ["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"]) assert.ok(src.includes(s + ": ["), s);
  // no dedicated trigger endpoint exists, so the page must not post anything
  assert.ok(!/method:\s*"POST"/.test(src));
});

test("lists scans only from pipelines with a scan stage, with severity counts", async () => {
  const h = setup();
  await mount(h);
  assert.deepEqual(plain(h.calls[0].options.query), { app_id: 3 });
  assert.deepEqual(plain(h.calls[1].options.query), { app_id: 3, page: 1, page_size: 10 });
  const fetched = h.calls.filter((c) => /^\/cicd\/runs\/\d+$/.test(c.path)).map((c) => c.path).sort();
  assert.deepEqual(fetched, ["/cicd/runs/100", "/cicd/runs/101"], "run 102 belongs to a pipeline without scan stage");
  const t = text(h.el);
  for (const s of ["镜像扫描", "已阻断", "扫描失败", "严重", "高危", "中危", "低危", "Trivy", "主干构建", "harbor.example.com/team/web:abc"]) assert.ok(t.includes(s), s);
  const cards = text(h.el.querySelector("[data-isc-summary]"));
  assert.ok(/1严重/.test(cards) && /2高危/.test(cards) && /3中危/.test(cards) && /4低危/.test(cards), cards);
  assert.ok(!/\/cicd|api\/v1|CRITICAL|blocked|stage_idx/i.test(t), "no developer jargon");
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  assert.equal(h.timers.size, 0, "no active runs, no polling");
});

test("vulnerability detail shows CVE, package, versions, severity, gate policy and filters", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", h.el.querySelectorAll('[data-isc-click="open"]').find((b) => b.getAttribute("data-id") === "21"));
  const d = h.el.querySelector("[data-isc-detail]");
  assert.ok(d);
  const t = text(d);
  for (const s of ["CVE-2024-0001", "openssl", "3.0.1", "3.0.13", "暂无修复", "阻断", "已忽略", "堆溢出"]) assert.ok(t.includes(s), s);
  assert.ok(text(h.el.querySelector("[data-isc-policy]")).includes("出现严重、高危漏洞时阻断发布"));
  assert.ok(text(h.el.querySelector("[data-isc-policy]")).includes("暂无修复版本的漏洞不阻断"));
  assert.ok(t.includes("仅保存最需关注的前 4 个"));
  const links = d.querySelectorAll("a");
  assert.equal(links.length, 1, "only http(s) advisory links are rendered");
  assert.equal(links[0].getAttribute("href"), "https://avd.example/CVE-2024-0001");
  await h.emit("click", h.el.querySelectorAll('[data-isc-click="sev"]').find((b) => b.getAttribute("data-sev") === "MEDIUM"));
  const vt = text(h.el.querySelector("[data-isc-vulns]"));
  assert.ok(vt.includes("CVE-2024-0003") && !vt.includes("CVE-2024-0001"));
  await h.emit("click", '[data-isc-click="close"]');
  assert.equal(h.el.querySelector("[data-isc-detail]"), null);
});

test("failed scan shows the escaped reason", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", h.el.querySelectorAll('[data-isc-click="open"]').find((b) => b.getAttribute("data-id") === "20"));
  const e = h.el.querySelector("[data-isc-scan-error]");
  assert.ok(text(e).includes("还没有扫描报告") && text(e).includes("<img src=x"));
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
});

test("app without scan stage explains how to enable it and fetches no runs", async () => {
  const h = setup({ pipes: [PIPES[1]] });
  await mount(h);
  assert.ok(h.el.querySelector("[data-isc-noscan]"));
  assert.ok(!h.calls.some((c) => c.path.startsWith("/cicd/runs")));
});

test("active runs trigger polling; pagination moves by run pages", async () => {
  const runs = [{ id: 103, pipeline_id: 11, app_id: 3, status: "running" }, ...RUNS];
  const h = setup({ runs, details: { ...DETAILS, 103: { ...runs[0], scans: [] } }, total: 25 });
  await mount(h);
  assert.equal(h.timers.size, 1);
  assert.ok(text(h.el).includes("1 / 3"));
  await h.emit("click", '[data-isc-click="next"]'); for (let i = 0; i < 5; i++) await tick();
  assert.equal(h.calls.filter((c) => c.path === "/cicd/runs").at(-1).options.query.page, 2);
});

test("errors are shown in Chinese; a single failed run detail is reported, not fatal", async () => {
  const h = setup({ extra: (path) => { if (path === "/cicd/runs/100") throw new Error("运行不存在"); } });
  await mount(h);
  assert.ok(text(h.el).includes("1 次运行的扫描结果读取失败"));
  assert.ok(text(h.el).includes("已阻断"));
  const h2 = setup({ extra: (path) => { if (path === "/cicd/pipelines") throw new Error("没有权限"); } });
  await mount(h2);
  assert.ok(text(h2.el.querySelector("[data-isc-error]")).includes("读取镜像扫描结果失败：没有权限"));
});
