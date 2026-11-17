import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

// v1.31 u-overview：总览健康卡 / 最近事件 / Pod 资源消耗 Top 5 / 趋势降级 / 主机纳管筛选与异常置顶
// 共享 harness 不支持后代选择器（"[data-live-tablewrap] tbody tr"）；仅在本测试进程内补上，便于验证真实的筛选逻辑
const baseQueryAll = Element.prototype.querySelectorAll;
const queryPart = (node, part) => {
  const m = part.match(/^([a-z]+)(\[[^\]]+\])$/);
  return m ? baseQueryAll.call(node, m[2]).filter((n) => n.tag === m[1]) : baseQueryAll.call(node, part);
};
Element.prototype.querySelectorAll = function (selector) {
  const parts = String(selector).trim().split(/\s+/);
  if (selector.includes(",")) return baseQueryAll.call(this, selector);
  let scope = [this];
  for (const part of parts) scope = [...new Set(scope.flatMap((n) => queryPart(n, part)))];
  return scope;
};

const source = fs.readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");

const pureBox = { console: { log() {}, error() {} }, process: { versions: { node: "test" }, exitCode: 0 }, module: { exports: {} } };
pureBox.globalThis = pureBox;
vm.runInNewContext(source, pureBox, { filename: "xs-live-core.js" });
const P = pureBox.module.exports.pure;

const ago = (sec) => new Date(Date.now() - sec * 1000).toISOString();
const okNode = (name) => ({ name, ready: true, allocatable: { cpu: "4", memory: "8Gi", pods: "110" }, conditions: [{ type: "Ready", status: "True" }, { type: "MemoryPressure", status: "False" }] });
const pod = (name, extra = {}) => ({ name, namespace: "default", phase: "Running", status: "Running", ready: "1/1", restarts: 0, node: "n1", ...extra });

function load({ api, observe, hash = "#/sre" } = {}) {
  const body = new Element("body"), view = new Element("main"), head = new Element("head");
  view.setAttribute("id", "view");
  body.appendChild(view);
  const calls = [], navs = [], timers = [];
  const document = {
    body, head, hidden: false,
    getElementById: (id) => body.querySelector('[id="' + id + '"]') || head.querySelector('[id="' + id + '"]'),
    createElement: (tag) => new Element(tag),
    querySelector: (sel) => body.querySelector(sel)
  };
  const location = { hash };
  const history = { replaceState: (_s, _t, h) => { location.hash = h; } };
  const window = {
    XS: { mode: "live", can: () => true, get: async (path, query, opts) => { calls.push({ path, query, opts }); return api(path, query); }, isAuth: (e) => e?.status === 401 },
    XSObserve: observe, scrollY: 0, scrollTo() {}
  };
  const sandbox = {
    window, document, location, history, URL, console: { log() {}, error() {}, warn() {} },
    setTimeout: (fn) => { timers.push(fn); return timers.length; }, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
    localStorage: { getItem: () => null, setItem() {} },
    navGo: (h) => { navs.push(h); location.hash = h; }
  };
  vm.runInNewContext(source.replace("  boot();", "  GLOBAL.__u = { sre: renderSreLive, hosts: renderHostsLive, spark: ksSpark, chart: renderSreChart, applyHashFilters: applyHashFilters, clstate: function () { return CLSTATE; }, hostFilter: function () { return HOST_FILTER; }, workloads: clWorkloadsPage, nodes: clNodesPage, bump: bump };"), sandbox);
  return { u: sandbox.__u, view, calls, navs, location, timers };
}

async function settle(n = 12) { for (let i = 0; i < n; i++) await tick(); }

function clusterApi({ nodes, pods, deployments = [], events = [], alerts = [], alertsFail = false, extra } = {}) {
  return async (path, query) => {
    if (extra) { const v = await extra(path, query); if (v !== undefined) return v; }
    if (path === "/clusters") return { items: [{ id: 3, name: "prod-a", env: "生产" }] };
    if (path === "/platform/notices") return { items: [] };
    if (path.endsWith("/nodes")) return { items: nodes };
    if (path.endsWith("/workloads/pods")) return { items: pods };
    if (path.endsWith("/workloads/deployments")) return { items: deployments };
    if (path.endsWith("/events")) return { items: events };
    if (path.endsWith("/components")) return { items: [] };
    if (path === "/observability/alerts") { if (alertsFail) { const e = new Error("未接入"); e.status = 404; throw e; } return { items: alerts }; }
    if (path === "/observability/datasources/resolve") { const e = new Error("未配置"); e.status = 404; throw e; }
    return { items: [] };
  };
}

const observeStub = {
  resolvedDatasource: () => ({ ds: { id: 9 }, scope: "cluster" }),
  metricQueries: () => ({ cpu: "cpu_q", mem: "mem_q" }),
  scopedPromQuery: (q) => q
};

test("健康总览：全部正常时给出一句结论，正常格不可点击", () => {
  const h = P.overviewHealth({
    clusterId: 3, apiOk: true, nodesOk: true, podsOk: true, deploysOk: true, alertsOk: true,
    nodes: [okNode("n1"), okNode("n2")], pods: [pod("a"), pod("dns", { namespace: "kube-system" })],
    deployments: [{ name: "web", ready: "2/2" }], alerts: []
  });
  assert.equal(h.summary, "全部正常");
  assert.equal(h.issues, 0);
  assert.equal(h.tone, "ok");
  assert.deepEqual(Array.from(h.cells, (c) => c.key), ["control", "nodes", "workloads", "alerts"]);
  assert.ok(h.cells.every((c) => c.tone === "ok" && c.href === ""));
  assert.equal(h.cells[1].value, "2/2");
});

test("健康总览：异常格带状态色、数字与筛选跳转", () => {
  const h = P.overviewHealth({
    clusterId: 3, apiOk: true, nodesOk: true, podsOk: true, deploysOk: true, alertsOk: true,
    nodes: [okNode("n1"), { ...okNode("n2"), ready: false }],
    pods: [pod("ok"), pod("crash", { status: "CrashLoopBackOff", ready: "0/1", restarts: 12 }), pod("pending", { phase: "Pending", status: "Pending", ready: "0/1" }),
      pod("flappy", { restarts: 7 }), pod("job", { phase: "Succeeded", status: "Completed", ready: "0/1" }), pod("proxy", { namespace: "kube-system", ready: "0/1" })],
    deployments: [{ name: "web", ready: "1/3" }, { name: "idle", ready: "0/0" }],
    alerts: [{ severity: "critical" }, { labels: { severity: "warning" } }]
  });
  assert.equal(h.summary, "4 项需处理");
  assert.equal(h.tone, "bad");
  const by = Object.fromEntries(h.cells.map((c) => [c.key, c]));
  assert.equal(by.control.tone, "warn");
  assert.equal(by.control.href, "#/cluster/workloads/3?kind=pods&ns=kube-system&status=abnormal");
  assert.equal(by.nodes.tone, "bad");
  assert.equal(by.nodes.value, "1/2");
  assert.equal(by.nodes.href, "#/cluster/nodes/3?status=abnormal");
  assert.equal(by.workloads.tone, "bad");
  assert.match(by.workloads.detail, /异常 Pod 3/);
  assert.match(by.workloads.detail, /未就绪 Deployment 1/);
  assert.match(by.workloads.detail, /频繁重启 1/);
  assert.equal(by.workloads.value, "5");
  assert.equal(by.workloads.href, "#/cluster/workloads/3?kind=pods&status=abnormal");
  assert.equal(by.alerts.tone, "bad");
  assert.equal(by.alerts.value, "2");
  assert.equal(by.alerts.href, "#/observe/events");
});

test("健康总览：告警源未接入、API 不可达与只有 Deployment 异常的分支", () => {
  const unknown = P.overviewHealth({ clusterId: 1, apiOk: true, nodesOk: true, podsOk: true, deploysOk: true, alertsOk: false, nodes: [okNode("n1")], pods: [pod("a")], deployments: [] });
  assert.equal(unknown.summary, "未发现异常");
  assert.equal(unknown.cells[3].tone, "muted");
  assert.match(unknown.note, /部分数据暂不可用/);
  const down = P.overviewHealth({ clusterId: 1, apiOk: false, nodesOk: false, podsOk: false, deploysOk: false, alertsOk: true, alerts: [] });
  assert.equal(down.cells[0].tone, "bad");
  assert.equal(down.cells[0].value, "不可达");
  assert.equal(down.summary, "1 项需处理");
  const deployOnly = P.overviewHealth({ clusterId: 2, apiOk: true, nodesOk: true, podsOk: true, deploysOk: true, alertsOk: true, nodes: [okNode("n")], pods: [pod("a")], deployments: [{ ready: "0/2" }], alerts: [] });
  assert.equal(deployOnly.cells[2].href, "#/cluster/workloads/2?kind=deployments&status=abnormal");
});

test("最近事件按时间倒序、最多 8 条，可只看警告", () => {
  const events = Array.from({ length: 12 }, (_, i) => ({ reason: "R" + i, type: i % 3 === 0 ? "Warning" : "Normal", last_seen: ago(i * 60) }));
  const recent = P.recentEvents(events.slice().reverse());
  assert.equal(recent.length, 8);
  assert.equal(recent[0].reason, "R0");
  assert.equal(recent[7].reason, "R7");
  const warn = P.recentEvents(events, 8, "Warning");
  assert.deepEqual(Array.from(warn, (e) => e.reason), ["R0", "R3", "R6", "R9"]);
});

test("Pod Top 5 解析 Prometheus 向量并格式化", () => {
  const res = { data: { result: [
    { metric: { namespace: "a", pod: "p1" }, value: [1, "0.25"] },
    { metric: { namespace: "b", pod: "p2" }, value: [1, "1.5"] },
    { metric: { namespace: "c", pod: "" }, value: [1, "9"] },
    { metric: { namespace: "d", pod: "p3" }, value: [1, "NaN"] }
  ] } };
  assert.deepEqual(Array.from(P.podTopConsumers(res, 5), (x) => x.pod), ["p2", "p1"]);
  assert.deepEqual(Array.from(P.podTopConsumers({ result: [] })), []);
  assert.equal(P.fmtCores(0.25), "250m");
  assert.equal(P.fmtCores(1.5), "1.50 核");
});

test("筛选参数解析与列表状态预置", () => {
  assert.deepEqual({ ...P.hashQuery("#/cluster/workloads/3?kind=pods&ns=kube-system&status=abnormal") }, { kind: "pods", ns: "kube-system", status: "abnormal" });
  assert.deepEqual({ ...P.hashQuery("#/cluster/hosts") }, {});
  assert.equal(P.stripHashQuery("#/cluster/hosts?status=offline"), "#/cluster/hosts");
  const h = load({ api: clusterApi({ nodes: [], pods: [] }) });
  assert.equal(h.u.applyHashFilters("#/cluster/workloads/3?kind=pods&status=abnormal"), true);
  assert.equal(h.u.clstate().kind, "pods");
  assert.equal(h.u.clstate().ns, "");
  assert.equal(h.u.clstate().wlStatus, "abnormal");
  h.u.applyHashFilters("#/cluster/nodes/3?status=abnormal");
  assert.equal(h.u.clstate().nodeStatus, "abnormal");
  h.u.applyHashFilters("#/cluster/events/3?type=Warning");
  assert.equal(h.u.clstate().evtype, "Warning");
  h.u.applyHashFilters("#/cluster/hosts?status=offline");
  assert.equal(h.u.hostFilter(), "offline");
  assert.equal(h.u.applyHashFilters("#/sre?x=1"), false);
});

test("趋势图没有采样点时只给一行说明，1 个点时显示当前值", () => {
  const h = load({ api: clusterApi({ nodes: [], pods: [] }) });
  assert.doesNotMatch(h.u.spark([], "cpu", "#000"), /采样中|height:120px/);
  assert.match(h.u.spark([], "cpu", "#000"), /ks-line/);
  assert.match(h.u.spark([{ t: 1, cpu: 32.14 }], "cpu", "#000"), /当前 <b>32\.1%<\/b>/);
  assert.match(h.u.spark([{ t: 1, cpu: 10 }, { t: 2, cpu: 20 }], "cpu", "#000"), /<svg/);
  assert.match(h.u.chart([], [], [], 800), /近 24 小时没有可绘制/);
  assert.doesNotMatch(h.u.chart([], [], [], 800), /padding-top/);
  assert.match(h.u.chart([[100, 12.5]], [[100, 40]], [], 800), /当前 CPU <b>12\.5%<\/b> · 内存 <b>40\.0%<\/b>/);
  assert.match(h.u.chart([[100, 1], [200, 2]], [[100, 3], [200, 4]], [], 800), /<svg/);
});

test("总览页：结论在最前，异常格可点击跳到带筛选的列表，最近事件高亮警告", async () => {
  const events = [
    ...Array.from({ length: 9 }, (_, i) => ({ type: "Normal", reason: "Pulled", namespace: "default", object_name: "web-" + i, message: "ok", last_seen: ago(600 + i) })),
    { type: "Warning", reason: "BackOff", namespace: "default", object_name: "crash", message: "<b>restart</b>", last_seen: ago(30), count: 4 }
  ];
  const h = load({
    observe: observeStub,
    api: clusterApi({
      nodes: [okNode("n1"), { ...okNode("n2"), ready: false }],
      pods: [pod("web"), pod("crash", { status: "CrashLoopBackOff", ready: "0/1", restarts: 9 })],
      deployments: [{ name: "web", ready: "1/1" }], events, alertsFail: true
    })
  });
  h.u.sre(h.view);
  await settle();
  const html = h.view.innerHTML;
  const iHealth = html.indexOf("data-ks-health-card"), iEvents = html.indexOf("data-ks-events"), iTrend = html.indexOf("data-ks-trendrow"), iDetail = html.indexOf("ks-counts");
  assert.ok(iHealth > 0 && iHealth < iEvents && iEvents < iTrend && iTrend < iDetail, "布局顺序：结论→异常→趋势→明细");
  assert.equal(h.view.querySelector("[data-ks-health-summary]").textContent, "2 项需处理");
  const cells = h.view.querySelectorAll("[data-ks-health]");
  assert.equal(cells.length, 4);
  assert.equal(cells.find((c) => c.getAttribute("data-ks-health") === "alerts").getAttribute("data-tone"), "muted");
  const nodeCell = cells.find((c) => c.getAttribute("data-ks-health") === "nodes");
  assert.equal(nodeCell.tag, "a");
  assert.equal(nodeCell.getAttribute("data-tone"), "bad");
  assert.equal(cells.find((c) => c.getAttribute("data-ks-health") === "control").tag, "div");
  // 最近事件：最多 8 条、警告置于时间最新处并高亮、消息已转义
  const items = h.view.querySelector("[data-ks-events]").querySelectorAll("li");
  assert.equal(items.length, 8);
  assert.match(items[0].getAttribute("class"), /is-warn/);
  assert.match(html, /&lt;b&gt;restart&lt;\/b&gt;/);
  assert.ok(h.calls.some((c) => c.path === "/clusters/3/events" && c.query.limit === 100 && !c.query.type));
  // 无 Pod 指标：Top 5 隐藏；没有 metrics-server：实时曲线只占一行
  assert.equal(h.view.querySelector("[data-ks-top]").hidden, true);
  assert.match(html, /data-ks-spark-note/);
  assert.doesNotMatch(html, /采样中/);
  assert.match(h.view.querySelector("[data-sre-chart]").innerHTML, /未接入 Prometheus/);
  // 点击节点异常格：预置筛选并跳到不带参数的节点页
  nodeCell.onclick({ preventDefault() {} });
  assert.equal(h.navs.at(-1), "#/cluster/nodes/3");
  assert.equal(h.u.clstate().nodeStatus, "abnormal");
  // 切换「警告」页签
  h.view.querySelector('[data-ks-ev="warn"]').onclick();
  const warnItems = h.view.querySelector("[data-ks-events]").querySelectorAll("li");
  assert.equal(warnItems.length, 1);
  assert.match(h.view.querySelector("[data-ks-events]").innerHTML, /cluster\/events\/3\?type=Warning/);
});

test("总览页：有 Prometheus 时显示 Pod 资源消耗 Top 5，可在 CPU / 内存间切换", async () => {
  const h = load({
    observe: observeStub,
    api: clusterApi({
      nodes: [okNode("n1")], pods: [pod("web")],
      extra: async (path, query) => {
        if (path === "/observability/datasources/resolve") return { id: 9 };
        if (path === "/observability/prometheus/9/query" && /topk/.test(query.query)) {
          return /memory/.test(query.query)
            ? { data: { result: [{ metric: { namespace: "db", pod: "mysql-0" }, value: [1, String(2 * 1024 ** 3)] }] } }
            : { data: { result: [{ metric: { namespace: "web", pod: "api-1" }, value: [1, "0.42"] }, { metric: { namespace: "web", pod: "api-2" }, value: [1, "1.2"] }] } };
        }
        if (path === "/observability/prometheus/9/query") return { data: { result: [{ value: [1, "30"] }] } };
        if (path === "/observability/prometheus/9/query_range") return { data: { result: [{ values: [[1, "20"], [2, "25"]] }] } };
        return undefined;
      }
    })
  });
  h.u.sre(h.view);
  await settle(20);
  const top = h.view.querySelector("[data-ks-top]");
  assert.equal(top.hidden, false);
  assert.match(top.innerHTML, /资源消耗 Top 5/);
  assert.ok(top.innerHTML.indexOf("api-2") < top.innerHTML.indexOf("api-1"));
  assert.match(top.innerHTML, /420m/);
  top.querySelector('[data-ks-top-by="mem"]').onclick();
  assert.match(h.view.querySelector("[data-ks-top]").innerHTML, /mysql-0/);
  assert.match(h.view.querySelector("[data-ks-top]").innerHTML, /2\.0 GiB/);
  assert.match(h.view.querySelector("[data-sre-chart]").innerHTML, /<svg/);
});

test("主机状态四色与异常置顶", () => {
  assert.equal(P.hostState({ online: true, enrolled: true }).tone, "ok");
  assert.equal(P.hostState({ online: true, enrolled: true, facts: { cpu_percent: 95 } }).tone, "warn");
  assert.equal(P.hostState({ online: false, enrolled: false }).tone, "muted");
  assert.equal(P.hostState({ online: false, enrolled: true }).tone, "bad");
  const sorted = P.sortHostsByHealth([
    { name: "a", online: true, enrolled: true }, { name: "b", online: false, enrolled: true },
    { name: "c", online: false, enrolled: false }, { name: "d", online: true, enrolled: true, facts: { mem_percent: 93 } }, { name: "e", online: false, enrolled: true }
  ]);
  assert.deepEqual(Array.from(sorted, (h) => h.name), ["b", "e", "d", "c", "a"]);
});

test("主机纳管：离线置顶，统计卡可点击筛选列表", async () => {
  const hosts = [
    { id: 1, name: "web-1", ip: "10.0.0.1", env: "生产", online: true, enrolled: true, facts: {} },
    { id: 2, name: "db-1", ip: "10.0.0.2", env: "生产", online: false, enrolled: true, facts: {} },
    { id: 3, name: "new-1", ip: "10.0.0.3", env: "测试", online: false, enrolled: false, facts: {} }
  ];
  const h = load({ hash: "#/cluster/hosts", api: async (path) => (path === "/hosts" ? { items: hosts } : { items: [] }) });
  h.u.hosts(h.view);
  await settle();
  const rows = h.view.querySelectorAll("[data-live-tablewrap] tbody tr");
  assert.deepEqual(rows.map((r) => r.getAttribute("data-f-status")), ["offline", "offline", "online"]);
  assert.equal(rows[0].querySelector(".x-name").textContent, "db-1");
  assert.equal(rows[0].querySelector(".x-red").textContent, "离线");
  assert.equal(rows[1].querySelector(".x-grey").textContent, "未注册");
  assert.equal(rows[2].querySelector(".x-green").textContent, "在线");
  assert.match(h.view.innerHTML, /未注册/);
  const cards = h.view.querySelectorAll("[data-host-filter]");
  assert.deepEqual(cards.map((c) => c.getAttribute("data-host-filter")), ["", "online", "offline"]);
  cards.find((c) => c.getAttribute("data-host-filter") === "offline").onclick();
  assert.equal(h.u.hostFilter(), "offline");
  assert.deepEqual(rows.map((r) => r.hidden), [false, false, true]);
  assert.equal(cards.find((c) => c.getAttribute("data-host-filter") === "offline").getAttribute("aria-pressed"), "true");
  cards.find((c) => c.getAttribute("data-host-filter") === "online").onclick();
  assert.deepEqual(rows.map((r) => r.hidden), [true, true, false]);
  cards.find((c) => c.getAttribute("data-host-filter") === "online").onclick();
  assert.equal(h.u.hostFilter(), "");
  assert.deepEqual(rows.map((r) => r.hidden), [false, false, false]);
});

test("工作负载与节点列表支持「仅异常」筛选（由总览异常格预置）", async () => {
  const pods = [pod("web"), pod("crash", { status: "CrashLoopBackOff", ready: "0/1" }), pod("flappy", { restarts: 8 })];
  const nodes = [okNode("n1"), { ...okNode("n2"), ready: false }, { ...okNode("n3"), unschedulable: true }];
  const h = load({ api: async (path) => (path.endsWith("/workloads/pods") ? { items: pods } : path.endsWith("/nodes") ? { items: nodes, metrics_available: false } : { items: [] }) });
  h.u.applyHashFilters("#/cluster/workloads/3?kind=pods&status=abnormal");
  const box = new Element("div");
  h.view.appendChild(box);
  h.u.workloads(h.view, h.u.bump(), { id: 3 }, box);
  await settle();
  const names = box.querySelectorAll(".x-name").map((n) => n.textContent);
  assert.deepEqual(names, ["crash", "flappy"]);
  assert.match(box.innerHTML, /2 \/ 3 个/);
  assert.equal(box.querySelector("[data-live-wlstatus]").value, "abnormal");
  h.u.applyHashFilters("#/cluster/nodes/3?status=abnormal");
  const nbox = new Element("div");
  h.view.appendChild(nbox);
  h.u.nodes(h.view, h.u.bump(), { id: 3 }, nbox);
  await settle();
  assert.deepEqual(nbox.querySelectorAll("[data-node]").map((n) => n.getAttribute("data-node")), ["n2", "n3"]);
  assert.equal(nbox.querySelector("[data-live-nodestatus]").value, "abnormal");
});
