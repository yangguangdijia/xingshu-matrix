import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");
const sandbox = {
  console: { log() {}, error() {} },
  process: { versions: { node: "test" }, exitCode: 0 },
  module: { exports: {} }
};
sandbox.globalThis = sandbox;
vm.runInNewContext(source, sandbox, { filename: "xs-live-core.js" });
const P = sandbox.module.exports.pure;

function section(start, end) {
  const from = source.indexOf(start);
  const to = source.indexOf(end, from + start.length);
  assert.ok(from >= 0 && to > from, `missing source section ${start}`);
  return source.slice(from, to);
}

test("overview resolves Prometheus through the ownership contract before querying", () => {
  const metrics = section("function loadSreMetrics", "// 发布公告弹窗");
  const resolveCall = metrics.indexOf('apiGet("/observability/datasources/resolve"');
  const ownershipCheck = metrics.indexOf('observe.resolvedDatasource(result, clusters, cluster, "prometheus")');
  const queryBuild = metrics.indexOf('observe.metricQueries(nodes, [], "", "", scope)');
  assert.ok(resolveCall >= 0 && ownershipCheck > resolveCall && queryBuild > ownershipCheck);
  assert.match(metrics, /var datasourceId = scope\.ds\.id/);
  assert.doesNotMatch(metrics, /(?:dsResult|result)\.id/);
  const overview = section("function renderSreLive", "// 发布公告弹窗");
  assert.match(overview, /contextSnapshot = context && typeof context\.get === "function" \? context\.get\(\)/);
  assert.doesNotMatch(overview, /clusters\s*\[\s*0\s*\]/);
});

test("Prometheus parser accepts only the standard data.result envelope", () => {
  const instant = { data: { result: [{ value: [100, "42.5"] }] } };
  const range = { data: { result: [{ values: [[100, "12.25"], [200, "NaN"], [250, null], [300, "31.5"]] }] } };
  assert.equal(P.prometheusLatest(instant), 42.5);
  assert.equal(P.prometheusLatest(range), 31.5);
  assert.deepEqual(Array.from(P.prometheusValues(range), (point) => Array.from(point)), [
    [100, 12.25], [200, null], [250, null], [300, 31.5]
  ]);
  assert.equal(P.prometheusLatest({ result: [{ value: [100, "99"] }] }), null);
  assert.equal(P.prometheusLatest({ data: { result: [] } }), null);
});

test("overview fails closed when there is no explicit current cluster", () => {
  const clusters = [{ id: 1, name: "prod-a" }, { id: 2, name: "prod-b" }];
  assert.equal(P.overviewClusterFromState(null, clusters), null);
  assert.equal(P.overviewClusterFromState({ mode: "all", cluster_id: 1 }, clusters), null);
  assert.equal(P.overviewClusterFromState({ mode: "unselected", cluster_id: 1 }, clusters), null);
  assert.equal(P.overviewClusterFromState({ kind: "environment", cluster_id: 1 }, clusters), null);
  assert.equal(P.overviewClusterFromState({ kind: "cluster", cluster_id: 9 }, clusters), null);
  assert.equal(P.overviewClusterFromState({ kind: "cluster", cluster_id: 2 }, clusters).name, "prod-b");
});

test("node TOP5 uses cpu_percent descending and keeps missing usage last", () => {
  const nodes = [
    { name: "missing-a" },
    { name: "low", usage: { cpu_percent: 5, memory_percent: 20 } },
    { name: "high", usage: { cpu_percent: 91, memory_percent: 75 } },
    { name: "missing-b", usage: {} },
    { name: "mid", usage: { cpu_percent: "43.5", memory_percent: 50 } },
    { name: "lower", usage: { cpu_percent: 2, memory_percent: 10 } }
  ];
  assert.deepEqual(Array.from(P.topNodesByCpu(nodes, 5), (node) => node.name), [
    "high", "mid", "low", "lower", "missing-a"
  ]);
  assert.equal(P.nodeUsagePercent(nodes[0], "cpu_percent"), null);
  assert.equal(P.nodeUsagePercent(nodes[2], "memory_percent"), 75);
});

test("active overview alerts are keyboard buttons with detail fallback", () => {
  const overview = section("function renderSreLive", "function sreMetricError");
  assert.match(overview, /<button type=\"button\" class=\"x-tlink x-alert-open\" data-sre-alert-open=/);
  assert.match(overview, /typeof window\.XSOpenAlertDetail === \"function\"\) window\.XSOpenAlertDetail\(id\)/);
  assert.match(overview, /labels\.severity/);
  assert.match(overview, /annotations\.summary/);
  assert.match(overview, /labels\.namespace/);
  assert.match(overview, /else if \(id\) goHash\(\"#\/observe\/events\/\" \+ encodeURIComponent\(id\)\)/);
  assert.match(overview, /else goHash\(\"#\/observe\/events\"\)/);
  assert.doesNotMatch(overview, /<li><span[^\n]+<span class=\\?\"x-name/);
});

test("node list exposes the shared search marker and no-match state", () => {
  const nodes = section("function clNodesPage", "function clNamespacesPage");
  assert.match(nodes, /searchInput\(\"搜索节点名称 \/ IP \/ 角色\"\)/);
  assert.match(nodes, /data-live-none hidden>无匹配节点/);
  assert.match(nodes, /filterTable\(box\)/);
});

test("24h chart names both physical-capacity series and has no fixed hex grid", () => {
  const chart = section("function renderSreChart", "function loadSreMetrics");
  const overview = section("function renderSreLive", "function sreMetricError");
  assert.match(overview, /CPU \/ 内存物理容量 24 小时趋势/);
  assert.doesNotMatch(overview, /物理容量口径 ·/);
  assert.match(overview, /CPU \/ 内存占 Kubernetes Allocatable/);
  assert.match(chart, /节点物理容量 CPU 与内存使用率 24 小时曲线/);
  assert.match(chart, /x-sre-line--cpu/);
  assert.match(chart, /x-sre-line--memory/);
  assert.doesNotMatch(chart, /stroke=\"#[0-9a-fA-F]{3,8}\"/);
});

test("node usage is refreshed while the node list stays searchable", () => {
  const nodes = section("function clNodesPage", "function clNamespacesPage");
  assert.match(nodes, /setInterval\(function \(\)[\s\S]*?loadNodes\(\)[\s\S]*?\}, 15000\)/);
  assert.match(nodes, /priorSearch\.value/);
  assert.match(nodes, /每 15 秒更新/);
});

test("overview refreshes cluster metrics and node loads without an explicit reload", () => {
  const overview = section("function renderSreLive", "function sreMetricError");
  assert.match(overview, /setTimeout\(function tick\(\)[\s\S]*?renderSreLive\(el, true\)[\s\S]*?\}, 15000\)/);
  assert.match(overview, /document\.hidden/);
  assert.match(overview, /全部集群/);
  assert.match(overview, /节点物理容量使用率，等待 Prometheus 返回/);
});


test("live install owns the sre route and repaints it on one cluster-context subscription", () => {
  const install = section("var OWN_KEYS", "/* ---------------- 右下角模式徽标");
  assert.match(install, /var OWN_KEYS = \{ sre: 1,/);
  assert.match(install, /patchRoute\(R, "sre", renderSreLive, false\)/);
  assert.match(install, /LIVE\.routes\.sre = renderSreLive/);
  assert.match(install, /SRE_CONTEXT_SUBSCRIBED/);
  assert.match(install, /window\.addEventListener\("xs:cluster-context", handler\)/);
  assert.match(install, /routeOf\(\)\.key === "sre"\) repaint\(\)/);
});

test("overview keeps refresh binding and scopes every cluster-owned request", () => {
  const overview = section("function renderSreLive", "function sreMetricError");
  const metrics = section("function loadSreMetrics", "// 发布公告弹窗");
  const shellHelper = section("function shell", "function loadingPanel");
  const mountHelper = section("function mount", "// 客户端筛选");
  assert.match(shellHelper, /refreshButton\(\)/);
  assert.match(mountHelper, /bindCommon\(el\)/);
  assert.match(overview, /shell\("总览"/);
  assert.match(source, /function apiGet\(path, query, opts\)[\s\S]*?x\.get\(path, query, opts\)/);
  assert.match(overview, /\/nodes", null, \{ scope: "cluster" \}/);
  assert.match(overview, /\/workloads\/pods", \{ page_size: 200 \}, \{ scope: "cluster" \}/);
  assert.match(overview, /\/observability\/alerts", \{ page_size: 200,[^\n]+\{ scope: "cluster" \}/);
  assert.match(metrics, /\/datasources\/resolve", \{ type: "prometheus", cluster_id: cluster\.id \}, \{ scope: "cluster" \}/);
  assert.equal((metrics.match(/\{ scope: "cluster" \}/g) || []).length, 5);
  assert.match(overview, /contextSnapshot/);
  assert.match(overview, /context\.isCurrent\(contextSnapshot\)/);
  assert.match(metrics, /context\.isCurrent\(contextSnapshot\)/);
});

test("TOP5 Pod counts come from pod.node and expose incomplete lower bounds", () => {
  const counts = P.podCountsByNode([
    { name: "a", node: "node-a" }, { name: "b", node: "node-a" },
    { name: "c", node: "node-b" }, { name: "pending", node: "" }
  ]);
  assert.equal(counts["node-a"], 2);
  assert.equal(counts["node-b"], 1);
  assert.equal(P.listIsIncomplete({ items: [1, 2], total: 3 }, [1, 2]), true);
  assert.equal(P.listIsIncomplete({ items: [1, 2], total: 2, truncated: false }, [1, 2]), false);
  assert.equal(P.listIsIncomplete({ items: [1], total: 1, truncated: true }, [1]), true);
  assert.equal(P.countLabel(3, true, true), "3+");
  assert.equal(P.countLabel(0, true, false), "0");
  assert.equal(P.countLabel(0, false, false), "—");
  const overview = section("function renderSreLive", "function sreMetricError");
  assert.match(overview, /podCountsByNode\(pods\)/);
  assert.doesNotMatch(overview, /node\.pods/);
});
