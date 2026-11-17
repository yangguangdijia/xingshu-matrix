import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

function load(windowExtra = {}) {
  const window = Object.assign({}, windowExtra);
  const context = vm.createContext({ window, AbortController, console, setTimeout, clearTimeout });
  vm.runInContext(fs.readFileSync(new URL("../../public/xs-topology.js", import.meta.url), "utf8"), context);
  return { T: window.XSTopo, window };
}
const { T } = load();
const ids = (list) => Array.from(list, (n) => n.id).sort();
const edgeSet = (g) => new Set(Array.from(g.edges, (e) => `${e.from} > ${e.to}`));

// 测试用的最小 DOM 能力（harness 的 Element 没有 style / classList）
if (!Object.getOwnPropertyDescriptor(Element.prototype, "style")) {
  Object.defineProperty(Element.prototype, "style", { get() { return this._style || (this._style = {}); } });
  Object.defineProperty(Element.prototype, "classList", { get() {
    const el = this;
    const list = () => new Set(String(el.attrs.class || "").split(/\s+/).filter(Boolean));
    return {
      contains: (c) => list().has(c),
      toggle(c, on) { const s = list(); const add = on === undefined ? !s.has(c) : !!on; if (add) s.add(c); else s.delete(c); el.attrs.class = [...s].join(" "); return add; }
    };
  } });
}

test("status mapping uses four colours for pods and workloads", () => {
  assert.equal(T.podStatus({ status: "Running", ready: "1/1" }), "ok");
  assert.equal(T.podStatus({ status: "Running", phase: "Running", ready: "0/1" }), "warn");
  assert.equal(T.podStatus({ status: "CrashLoopBackOff", phase: "Running" }), "bad");
  assert.equal(T.podStatus({ status: "ImagePullBackOff" }), "bad");
  assert.equal(T.podStatus({ status: "Pending", phase: "Pending" }), "warn");
  assert.equal(T.podStatus({ status: "Terminating" }), "warn");
  assert.equal(T.podStatus({ status: "Succeeded" }), "ok");
  assert.equal(T.podStatus({}), "unknown");
  assert.equal(T.workloadStatus({ kind: "Deployment", desired: 3, ready: 3, status: "Available" }), "ok");
  assert.equal(T.workloadStatus({ kind: "Deployment", desired: 3, ready: 1 }), "warn");
  assert.equal(T.workloadStatus({ kind: "Deployment", desired: 2, ready: 0 }), "bad");
  assert.equal(T.workloadStatus({ kind: "Deployment", desired: 0, ready: 0 }), "unknown");
  assert.equal(T.workloadStatus({ kind: "Job", status: "Failed" }), "bad");
});

test("selector and replica set helpers", () => {
  assert.equal(T.selectorMatches({}, { app: "a" }), false);
  assert.equal(T.selectorMatches({ app: "a" }, { app: "a", tier: "x" }), true);
  assert.equal(T.selectorMatches({ app: "a", tier: "y" }, { app: "a", tier: "x" }), false);
  const deps = { web: { name: "web" }, "web-api": { name: "web-api" } };
  assert.equal(T.deploymentForReplicaSet("web-api-5c7d", "5c7d", deps).name, "web-api");
  assert.equal(T.deploymentForReplicaSet("web-api-5c7d", "", deps).name, "web-api", "longest prefix wins without hash");
  assert.equal(T.deploymentForReplicaSet("other-1", "1", deps), null);
  const b = T.ingressBackends({ name: "i", rules: [{ host: "a.com", http: { paths: [{ path: "/", backend: { service: { name: "web", port: { number: 80 } } } },
    { path: "/old", backend: { serviceName: "legacy", servicePort: 8080 } }] } }], defaultBackend: { service: { name: "fallback" } } });
  assert.deepEqual(Array.from(b, (x) => x.service), ["fallback", "web", "legacy"]);
});

test("buildGraph infers ingress → service → workload → pod from rows the API returns", () => {
  const g = T.buildGraph(T.demoData("shop"));
  const e = edgeSet(g);
  assert.ok(e.has("Ingress/shop/shop > Service/shop/web"));
  assert.ok(e.has("Ingress/shop/shop > Service/shop/api"));
  assert.ok(e.has("Service/shop/api > Deployment/shop/api"));
  assert.ok(e.has("Service/shop/redis > StatefulSet/shop/redis"));
  assert.ok(e.has("Deployment/shop/api > Pod/shop/api-58d7-e5"));
  assert.ok(e.has("StatefulSet/shop/redis > Pod/shop/redis-0"));
  assert.ok(!g.nodes.some((n) => n.kind === "ReplicaSet"), "replica sets hidden by default");
  const by = Object.fromEntries(g.nodes.map((n) => [n.id, n]));
  assert.equal(by["Deployment/shop/web"].status, "ok");
  assert.equal(by["Deployment/shop/api"].status, "warn");
  assert.equal(by["Deployment/shop/worker"].status, "bad");
  assert.equal(by["Service/shop/api"].status, "warn", "one of three backends not ready");
  assert.equal(by["Ingress/shop/shop"].status, "warn", "ingress inherits backend warning");
  assert.equal(by["Deployment/shop/api"].layer, 2);
  assert.equal(by["Pod/shop/redis-0"].layer, 4);
});

test("buildGraph handles missing services, selector-less services, inferred edges, ownerReferences and jobs", () => {
  const g = T.buildGraph({
    ingresses: [{ name: "in", namespace: "n", rules: [{ http: { paths: [{ path: "/", backend: { service: { name: "ghost" } } }] } }] }],
    services: [{ name: "ext", namespace: "n", type: "ExternalName", external_name: "db.example.com", selector: {} },
      { name: "idle", namespace: "n", selector: { app: "idle" } }],
    deployments: [{ kind: "Deployment", name: "idle", namespace: "n", replicas: 0, ready: "0/0", labels: { app: "idle" } }],
    pods: [{ metadata: { name: "x-1", namespace: "n", labels: { "pod-template-hash": "77" }, ownerReferences: [{ kind: "ReplicaSet", name: "x-77", controller: true }] }, status: "Running", ready: "1/1" },
      { name: "migrate-abc", namespace: "n", owner: "Job/migrate", status: "Completed", ready: "0/1" },
      { name: "bare", namespace: "n", status: "Running", ready: "1/1", labels: {} }]
  });
  const by = Object.fromEntries(g.nodes.map((n) => [n.id, n]));
  assert.equal(by["Service/n/ghost"].missing, true);
  assert.equal(by["Service/n/ghost"].status, "bad");
  assert.equal(by["Ingress/n/in"].status, "bad");
  assert.ok(g.warnings.some((w) => w.includes("ghost")));
  assert.ok(!g.edges.some((e) => e.from === "Service/n/ext"), "ExternalName service has no backends");
  const inferred = g.edges.find((e) => e.from === "Service/n/idle");
  assert.equal(inferred.to, "Deployment/n/idle");
  assert.equal(inferred.inferred, true);
  assert.equal(by["Deployment/n/idle"].status, "unknown", "scaled to zero is grey");
  assert.equal(by["Job/n/migrate"].synthetic, true);
  assert.ok(edgeSet(g).has("Job/n/migrate > Pod/n/migrate-abc"));
  assert.ok(edgeSet(g).has("ReplicaSet/n/x-77 > Pod/n/x-1"), "replica set without a known deployment stays visible");
  assert.equal(by["Pod/n/bare"].parent, "");
});

test("showReplicaSets inserts the replica set layer between workload and pod", () => {
  const g = T.buildGraph(T.demoData("shop"), { showReplicaSets: true });
  const e = edgeSet(g);
  assert.ok(e.has("Deployment/shop/api > ReplicaSet/shop/api-58d7"));
  assert.ok(e.has("ReplicaSet/shop/api-58d7 > Pod/shop/api-58d7-c3"));
  assert.ok(!e.has("Deployment/shop/api > Pod/shop/api-58d7-c3"));
  const rs = g.nodes.find((n) => n.id === "ReplicaSet/shop/api-58d7");
  assert.equal(rs.layer, 3);
  assert.equal(rs.status, "bad");
  assert.equal(rs.desired, 3);
});

test("filterByApp keeps only the connected part of one application", () => {
  const g = T.buildGraph(T.demoData("shop"), { app: "api" });
  assert.deepEqual(ids(g.nodes), ["Deployment/shop/api", "Ingress/shop/shop", "Pod/shop/api-58d7-c3", "Pod/shop/api-58d7-d4", "Pod/shop/api-58d7-e5", "Service/shop/api"]);
  const byWorkload = T.buildGraph(T.demoData("shop"), { app: { name: "shop", workloads: ["redis"] } });
  assert.deepEqual(ids(byWorkload.nodes), ["Pod/shop/redis-0", "Service/shop/redis", "StatefulSet/shop/redis"]);
  const raw = T.demoData("shop");
  raw.deployments[0].labels["app.kubernetes.io/instance"] = "rel-1";
  const byRelease = T.buildGraph(raw, { app: { name: "x", release: "rel-1" } });
  assert.ok(byRelease.nodes.some((n) => n.id === "Deployment/shop/web"));
  assert.ok(!byRelease.nodes.some((n) => n.id === "Deployment/shop/api"));
  assert.equal(T.buildGraph(T.demoData("shop"), { app: "nothing" }).nodes.length, 0);
});

test("foldPods groups pods per workload when there are more than 60", () => {
  const pods = [];
  for (let i = 0; i < 70; i++) pods.push({ name: `big-aa-${i}`, namespace: "n", owner: "ReplicaSet/big-aa", labels: { app: "big", "pod-template-hash": "aa" }, status: i < 3 ? "CrashLoopBackOff" : "Running", ready: i < 3 ? "0/1" : "1/1" });
  pods.push({ name: "small-0", namespace: "n", owner: "StatefulSet/small", labels: { app: "small" }, status: "Running", ready: "1/1" });
  const raw = { services: [{ name: "big", namespace: "n", selector: { app: "big" } }],
    deployments: [{ kind: "Deployment", name: "big", namespace: "n", replicas: 70, ready: "67/70", labels: { app: "big" } }],
    statefulsets: [{ kind: "StatefulSet", name: "small", namespace: "n", replicas: 1, ready: "1/1" }], pods };
  const g = T.buildGraph(raw);
  assert.equal(g.nodes.filter((n) => n.kind === "Pod").length, 71);
  const f = T.foldPods(g);
  assert.equal(f.folded, true);
  assert.equal(f.nodes.filter((n) => n.kind === "Pod").length, 0);
  const group = f.nodes.find((n) => n.id === "PodGroup/n/Deployment/n/big");
  assert.equal(group.members.length, 70);
  assert.equal(group.counts.bad, 3);
  assert.equal(group.status, "bad");
  assert.ok(edgeSet(f).has("Deployment/n/big > PodGroup/n/Deployment/n/big"));
  assert.ok(f.nodes.some((n) => n.id === "PodGroup/n/StatefulSet/n/small"));
  assert.equal(new Set(f.edges.map((e) => e.from + e.to)).size, f.edges.length, "edges deduplicated");
  const small = T.buildGraph(T.demoData());
  assert.equal(T.foldPods(small), small, "small graphs untouched");
  assert.equal(T.foldPods(T.buildGraph(T.demoData()), { force: true }).folded, true);
  const lay = T.layout(f);
  assert.equal(lay.nodes.find((n) => n.kind === "PodGroup" && n.members.length === 70).stack, 2, "grouped pods render stacked");
});

test("layout places layers left to right without overlaps and centres parents over their pods", () => {
  const g = T.buildGraph(T.demoData("shop"));
  const lay = T.layout(g);
  const { NODE_W, NODE_H, COL_GAP, PAD, ROW_GAP } = T.constants;
  const by = Object.fromEntries(lay.nodes.map((n) => [n.id, n]));
  assert.equal(by["Ingress/shop/shop"].x, PAD);
  assert.equal(by["Service/shop/web"].x, PAD + NODE_W + COL_GAP);
  assert.equal(by["Deployment/shop/web"].x, PAD + 2 * (NODE_W + COL_GAP));
  assert.equal(by["Pod/shop/redis-0"].x, PAD + 3 * (NODE_W + COL_GAP), "empty replica set column is skipped");
  assert.equal(lay.columns.length, 4);
  const cols = {};
  for (const n of lay.nodes) (cols[n.x] ||= []).push(n);
  for (const list of Object.values(cols)) {
    list.sort((a, b) => a.y - b.y);
    for (let i = 1; i < list.length; i++) assert.ok(list[i].y >= list[i - 1].y + NODE_H + ROW_GAP - 0.01, `overlap ${list[i - 1].id} / ${list[i].id}`);
  }
  const api = by["Deployment/shop/api"];
  const kids = lay.nodes.filter((n) => n.id.startsWith("Pod/shop/api-")).map((n) => n.y);
  assert.equal(api.y, (Math.min(...kids) + Math.max(...kids)) / 2);
  assert.equal(by["Service/shop/api"].y, api.y, "service aligned with its workload");
  assert.equal(by["Deployment/shop/worker"].y, PAD, "abnormal workloads sorted first");
  assert.equal(api.stack, 2);
  assert.equal(by["Deployment/shop/web"].stack, 1);
  assert.equal(by["StatefulSet/shop/redis"].stack, 0);
  for (const e of lay.edges) assert.match(e.path, /^M[\d.]+ [\d.]+( H[\d.]+| V[\d.]+)+$/, "orthogonal path");
  assert.ok(lay.width >= PAD * 2 + 4 * NODE_W + 3 * COL_GAP);
  assert.ok(lay.height > Math.max(...lay.nodes.map((n) => n.y + n.h)));
});

test("edgePath draws straight or right-angled segments", () => {
  assert.equal(T.edgePath({ x: 0, y: 0, w: 100, h: 40 }, { x: 200, y: 0, w: 100, h: 40 }), "M100 20 H200");
  assert.equal(T.edgePath({ x: 0, y: 0, w: 100, h: 40 }, { x: 204, y: 100, w: 100, h: 40 }), "M100 20 H152 V120 H204");
});

test("fromInventory renders application inventories without pod labels", () => {
  const raw = T.fromInventory({
    workloads: [{ kind: "Deployment", name: "pay", namespace: "prod", desired: 2, ready: 2, selector: { app: "pay" } }],
    pods: [{ name: "pay-6d8-x", namespace: "prod", owner: "ReplicaSet/pay-6d8", status: "Running", ready: "1/1", containers: [{ image: "pay:1" }] },
      { name: "pay-6d8-y", namespace: "prod", owner: "ReplicaSet/pay-6d8", status: "Running", ready: "1/1" }],
    services: [{ name: "pay", namespace: "prod", selector: { app: "pay" }, type: "ClusterIP" }]
  });
  const g = T.buildGraph(raw);
  const e = edgeSet(g);
  assert.ok(e.has("Deployment/prod/pay > Pod/prod/pay-6d8-x"));
  assert.ok(e.has("Service/prod/pay > Deployment/prod/pay"));
  assert.equal(g.edges.find((x) => x.from === "Service/prod/pay").inferred, true);
  assert.equal(g.nodes.find((n) => n.id === "Deployment/prod/pay").status, "ok");
});

test("side panel and detail targets carry key fields and links", () => {
  const g = T.buildGraph(T.demoData("shop"));
  const svc = g.nodes.find((n) => n.id === "Service/shop/api");
  const html = T.sideHTML(svc, g, { clusterId: 7 });
  for (const s of ["服务", "10.96.0.34", "app=api", "2 / 3", "上游", "下游", "在列表中查看"]) assert.ok(html.includes(s), s);
  assert.ok(!html.includes("查看 YAML"), "services have no YAML endpoint");
  const pod = g.nodes.find((n) => n.id === "Pod/shop/api-58d7-e5");
  const ph = T.sideHTML(pod, g, { clusterId: 7 });
  for (const s of ["反复崩溃", "重启次数", "查看 YAML", "查看日志"]) assert.ok(ph.includes(s), s);
  assert.ok(!T.sideHTML(pod, g, {}).includes("查看 YAML"), "no cluster → no actions");
  assert.equal(T.detailTarget(svc, 7).hash, "#/cluster/network/7");
  assert.equal(T.detailTarget(g.nodes.find((n) => n.kind === "Ingress"), 7).hash, "#/cluster/expose/7");
  assert.deepEqual({ ...T.detailTarget(pod, 7, "logs") }, { kind: "Pod", name: "api-58d7-e5", namespace: "shop", action: "logs", workloadKind: "pods", hash: "#/cluster/logs/7" });
  const scene = T.sceneHTML(T.layout(g));
  assert.ok(scene.includes('data-xtp-node="Deployment/shop/api"'));
  assert.ok(scene.includes("xtp-stack2"));
  assert.ok(scene.includes("副本 2/3"));
  assert.ok(!/<script/i.test(T.sceneHTML(T.layout(T.buildGraph({ services: [{ name: "<script>x", namespace: "n", selector: {} }] })))), "names escaped");
});

test("render reads the real list endpoints, opens the quick-look panel and navigates", async () => {
  const calls = [];
  const demo = T.demoData("shop");
  const api = async (path, o) => {
    calls.push({ path, query: o.query });
    const key = path.split("/").pop();
    if (key === "ingresses") throw Object.assign(new Error("forbidden"), { status: 403 });
    return { items: demo[key] || [], total: (demo[key] || []).length };
  };
  const navs = [];
  const el = new Element("div");
  const view = T.render(el, { clusterId: 7, namespace: "shop", api, navigate: (t) => navs.push(t) });
  await tick(); await tick();
  assert.deepEqual(calls.map((c) => c.path).sort(), ["/clusters/7/network/ingresses", "/clusters/7/network/services", "/clusters/7/workloads/daemonsets",
    "/clusters/7/workloads/deployments", "/clusters/7/workloads/jobs", "/clusters/7/workloads/pods", "/clusters/7/workloads/statefulsets"]);
  assert.ok(calls.every((c) => c.query.namespace === "shop"));
  const note = el.querySelector("[data-xtp-note]");
  assert.equal(note.hidden, false);
  assert.match(note.textContent, /应用路由：没有读取权限/);
  const vp = el.querySelector("[data-xtp-vp]");
  const node = vp.querySelector('[data-xtp-node="Pod/shop/api-58d7-e5"]');
  assert.ok(node, "pod card rendered");
  assert.match(el.querySelector("[data-xtp-sum]").innerHTML, /异常 3/);
  for (const fn of vp.listeners.click) fn({ target: node });
  const side = el.querySelector("[data-xtp-side]");
  assert.equal(side.hidden, false);
  assert.ok(side.innerHTML.includes("api-58d7-e5"));
  const logs = side.querySelector('[data-xtp-nav="logs"]');
  logs.onclick();
  assert.equal(navs[0].hash, "#/cluster/logs/7");
  side.querySelector("[data-xtp-yaml]").onclick();
  await tick();
  assert.equal(calls.at(-1).path, "/clusters/7/namespaces/shop/pods/api-58d7-e5");
  side.querySelector("[data-xtp-close]").onclick();
  assert.equal(side.hidden, true);
  view.destroy();
});

test("render degrades to a short message when every source fails, and uses fallback data when given", async () => {
  const fail = async () => { throw Object.assign(new Error("boom"), { status: 500 }); };
  const el = new Element("div");
  T.render(el, { clusterId: 7, namespace: "shop", api: fail });
  await tick(); await tick();
  const vp = el.querySelector("[data-xtp-vp]");
  assert.match(vp.innerHTML, /拓扑读取失败：暂时无法读取/);
  assert.ok(vp.querySelector("[data-xtp-retry]"));
  const el2 = new Element("div");
  T.render(el2, { clusterId: 7, namespace: "prod", api: fail, app: { name: "zzz", workloads: ["not-matching"] },
    fallbackData: T.fromInventory({ workloads: [{ name: "pay", namespace: "prod", desired: 1, ready: 1, selector: { app: "pay" } }], pods: [], services: [] }) });
  await tick(); await tick();
  assert.ok(el2.querySelector('[data-xtp-node="Deployment/prod/pay"]'), "fallback inventory is not filtered again");
  assert.match(el2.querySelector("[data-xtp-note]").textContent, /应用清单/);
  const el3 = new Element("div");
  T.render(el3, { data: { pods: [] } });
  await tick(); await tick();
  assert.match(el3.querySelector("[data-xtp-vp]").innerHTML, /没有可展示的资源/);
});

test("demo mode renders built-in sample data without calling the API", async () => {
  const { T: T2 } = load({ XS: { mode: "demo", api: () => { throw new Error("should not call"); } } });
  const el = new Element("div");
  T2.render(el, { clusterId: 1, namespace: "shop" });
  await tick(); await tick();
  assert.ok(el.querySelector('[data-xtp-node="Ingress/shop/shop"]'));
  assert.match(el.querySelector("[data-xtp-note]").textContent, /演示数据/);
});
