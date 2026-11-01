import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
const context = vm.createContext({});
vm.runInContext(fs.readFileSync(new URL("../../public/xs-observability.js", import.meta.url), "utf8"), context);
const O = context.XSObserve;
const cluster = { id: 1, name: "dev-a", env: "dev" };

test("datasource resolution never infers sharing from same environment", () => {
  const other = { id: 2, name: "dev-b", env: "dev" };
  const ds = [{ id: 1, type: "loki", env: "dev" }, { id: 2, type: "loki", env: "prod" }];
  assert.equal(O.datasource(ds, [cluster], cluster, ["loki"]), null);
  assert.equal(O.datasource(ds, [cluster], null, ["loki"]), null);
  const central = { id: 3, type: "loki", env: "", options: { scope: "central", cluster_label: "k8s_cluster", cluster_values: { 1: "a-label" } } };
  const s = O.datasource([central], [cluster, other], cluster, ["loki"]);
  assert.equal(s.clusterValue, "a-label"); assert.equal(s.dedicated, false);
  assert.equal(O.logQuery(s), '{job="fluent-bit",k8s_cluster="a-label"}');
  assert.equal(O.datasource([central], [cluster, other], other, ["loki"]), null);
});
test("explicit ownership wins and ambiguous datasources are rejected", () => {
  const ds = [{ type: "loki", env: "dev", options: { scope: "cluster", cluster_id: 1, cluster_name: "dev-a" }, id: 3 }, { type: "loki", env: "", options: { scope: "central", cluster_label: "cluster", cluster_values: { 1: "dev-a" } }, id: 4 }];
  assert.equal(O.datasource(ds, [cluster], cluster, ["loki"]).ds.id, 3);
  assert.throws(() => O.datasource([ds[0], { ...ds[0], id: 5 }], [cluster], cluster, ["loki"]), /多个/);
  assert.throws(() => O.datasource([ds[1], { ...ds[1], id: 5 }], [cluster], cluster, ["loki"]), /多个/);
  assert.throws(() => O.datasource([{ ...ds[0], options: { ...ds[0].options, cluster_name: "dev-b" } }], [cluster], cluster, ["loki"]), /不一致/);
  assert.throws(() => O.datasource([{ ...ds[1], options: { ...ds[1].options, cluster_values: null } }], [cluster], cluster, ["loki"]), /缺少/);
});
test("legacy explicit cluster ownership remains readable while unbound sources stay unresolved", () => {
  const legacy = { id: 6, type: "loki", env: "dev", options: { cluster_id: 1, cluster_name: "dev-a" } };
  const unbound = { id: 7, type: "loki", env: "dev", options: {} };
  assert.equal(O.datasource([legacy], [cluster], cluster, ["loki"]).ds.id, 6);
  assert.equal(O.datasource([unbound], [cluster], cluster, ["loki"]), null);
});
test("node and workload selectors escape identifiers and exclude unmatched pods", () => {
  const q = O.metricQueries([{ name: "node.1", internal_ip: "10.0.0.1" }], [], "prod", "missing");
  assert.match(q.cpu, /pod=~"a\^"/);
  assert.ok(q.rx.includes('instance=~"10\\\\.0\\\\.0\\\\.1:9100"'));
  assert.match(q.rst, /node=~/);
  assert.throws(() => O.metricQueries([], [], "", ""), /节点地址/);
});
test("central PromQL, Loki and trace selectors use the mapped label value", () => {
  const scope = O.datasource([{ type: "prometheus", env: "", options: { scope: "central", cluster_label: "cluster", cluster_values: { 1: "cluster-uid-a" } } }], [cluster], cluster, ["prometheus"]);
  const q = O.metricQueries([{ name: "node", internal_ip: "10.0.0.1" }], [{ namespace: "ns", name: "pod" }], "ns", "", scope);
  Object.values(q).forEach((value) => {
    assert.match(value, /cluster="cluster-uid-a"/);
    assert.doesNotMatch(value, /cluster="dev-a"/);
  });
  assert.equal(O.scopedPromQuery("up", scope), 'up{cluster="cluster-uid-a"}');
  assert.throws(() => O.scopedPromQuery('up{cluster="cluster-uid-b"}', scope), /冲突/);
  assert.match(O.scopedPromQuery('up{job="a,cluster=\\"cluster-uid-a\\""}', scope), /,cluster="cluster-uid-a"\}$/);
  assert.throws(() => O.scopedPromQuery('up{job="a",cluster=~"cluster-uid-a"}', scope), /冲突/);
  assert.throws(() => O.scopedPromQuery('sum(rate(up[5m]))', scope), /仅支持/);
  assert.equal(O.traceTags(scope).cluster, "cluster-uid-a");
  assert.throws(() => O.traceTags(scope, { cluster: "cluster-uid-b" }), /冲突/);
  assert.throws(() => O.logQuery({ ...scope, ds: { options: { log_selector: '{cluster="cluster-uid-b"}' } } }), /冲突/);
  assert.equal(O.scopedLogQuery('{app="api"} |= "failed"', scope), '{app="api",cluster="cluster-uid-a"} |= "failed"');
  assert.throws(() => O.scopedLogQuery('{app="api",cluster="cluster-uid-b"}', scope), /冲突/);
  assert.throws(() => O.scopedLogQuery('sum(rate(foo[5m]))', scope), /仅支持/);
  const trace = { type: "tempo", env: "", options: { scope: "central", cluster_label: "k8s.cluster.name", cluster_values: { 1: "local-a" } } };
  assert.equal(O.traceTags(O.datasource([trace], [cluster], cluster, ["tempo"]))["k8s.cluster.name"], "local-a");
});
test("Prometheus NaN remains missing, never converted to a healthy zero", () => {
  const r = O.matrix({ data: { result: [{ metric: {}, values: [[1, "NaN"], [2, "26.3"]] }] } });
  assert.equal(r[0].values[0][1], null);
  assert.equal(r[0].values[1][1], 26.3);
  assert.equal(O.matrix({ data: { result: [] } }).length, 0);
});
test("structured logs show the message and preserve full raw payload", () => {
  const raw = JSON.stringify({ log: JSON.stringify({ level: "error", msg: "connection refused", trace_id: "abc" }), kubernetes: { namespace_name: "apps", container_name: "api" } });
  const log = O.normalizeLog({ ts: "2026-10-23T00:00:00Z", message: raw }, cluster);
  assert.equal(log.lv, "ERROR"); assert.equal(log.app, "api");
  assert.match(log.msg, /连接被拒绝/); assert.equal(log.json.trace_id, "abc");
  assert.equal(log.json.raw, raw); assert.equal(log.json.cluster, "dev-a");
});
test("access and Kubernetes logs retain factual status and severity", () => {
  const access = O.normalizeLog({ message: '10.0.0.2 - - [date] "GET /api HTTP/1.1" 503 42' }, cluster);
  assert.match(access.msg, /服务端错误/); assert.equal(access.lv, "ERROR");
  const klog = O.normalizeLog({ message: "W1023 01:02:03.456 123 watcher.go:12] context deadline exceeded" }, cluster);
  assert.equal(klog.lv, "WARN"); assert.match(klog.msg, /超时/);
});
test("trace details preserve real span hierarchy and microsecond durations", () => {
  const t = O.traceRecord({ trace_id: "real", spans: [
    { span_id: "child", parent_id: "root", start: "2026-10-23T00:00:00.002Z", duration_us: 1500, service: "db", operation: "select", tags: {} },
    { span_id: "root", start: "2026-10-23T00:00:00Z", duration_us: 10000, service: "api", operation: "GET", tags: {} }
  ] }, cluster);
  assert.equal(t.spans[1].parent, 0); assert.equal(t.spans[1].start, 2);
  assert.equal(t.spans[1].dur, 1.5); assert.equal(t.dur, 10); assert.equal(t.summaryOnly, false);
});
test("alert location resolves cluster, pod, and node without treating exporters as affected pods", () => {
  const clusters = [cluster, { id: 2, name: "dev-b", env: "dev" }];
  const sources = [{ id: 7, type: "alertmanager", env: "dev" }];
  const inventories = [
    { clusterId: 1, pods: [{ name: "api-123", namespace: "shop", node: "worker-a" }, { name: "node-exporter-a", namespace: "monitoring", node: "worker-a" }], nodes: [{ name: "worker-a", internal_ip: "10.0.0.1", status: "Ready" }] },
    { clusterId: 2, pods: [{ name: "api-456", namespace: "shop", node: "worker-b" }], nodes: [{ name: "worker-b", internal_ip: "10.0.0.2", status: "Ready" }] }
  ];
  const pod = O.alertLocation({ datasource_id: 7, env: "dev", alertname: "PodDown", labels: { namespace: "shop", pod: "api-123" } }, clusters, sources, inventories);
  assert.equal(pod.cluster.name, "dev-a"); assert.equal(pod.pod.name, "api-123"); assert.equal(pod.nodeName, "worker-a");
  const node = O.alertLocation({ datasource_id: 7, env: "dev", alertname: "NodeFilesystemWillFillIn24h", labels: { namespace: "monitoring", pod: "node-exporter-a", instance: "10.0.0.1:9100", job: "node-exporter" } }, clusters, sources, inventories);
  assert.equal(node.cluster.name, "dev-a"); assert.equal(node.type, "Node"); assert.equal(node.name, "worker-a"); assert.equal(node.pod.name, "node-exporter-a");
  const broad = O.alertLocation({ datasource_id: 7, env: "dev", alertname: "AlertmanagerProblem", labels: { namespace: "monitoring" } }, clusters, sources, inventories);
  assert.equal(broad.cluster, null); assert.match(broad.note, /无法唯一确认/);
});
test("alert remediation requires one explicit live workload match in a firing alert", () => {
  const alert = { status: "firing", labels: { namespace: "shop", deployment: "checkout" } };
  const workloads = { deployments: [{ name: "checkout", namespace: "shop", ready: "1/2", replicas: 2, status: "Progressing" }] };
  const target = O.alertRemediationTarget(alert, cluster, workloads);
  assert.deepEqual({ ...target }, {
    clusterId: 1, clusterName: "dev-a", env: "dev", namespace: "shop",
    kind: "deployments", kindLabel: "Deployment", name: "checkout",
    ready: "1/2", replicas: 2, status: "Progressing"
  });
  assert.equal(O.alertRemediationTarget({ ...alert, status: "resolved" }, cluster, workloads), null);
  assert.equal(O.alertRemediationTarget(alert, null, workloads), null);
  assert.equal(O.alertRemediationTarget(alert, cluster, { deployments: [] }), null);
  assert.equal(O.alertRemediationTarget({ ...alert, labels: { ...alert.labels, statefulset: "checkout" } }, cluster, workloads), null);
  assert.equal(O.alertRemediationTarget({ ...alert, labels: { deployment: "checkout" } }, cluster, workloads), null);
});
test("diagnostic input redacts common credential forms and bounds length", () => {
  assert.equal(O.redactDiagnosticText("Authorization: Bearer abc123 token=xyz password: 'hidden'"),
    "Authorization: Bearer [REDACTED] token=[REDACTED] password: [REDACTED]");
  assert.equal(O.redactDiagnosticText("x".repeat(1300)).length, 1200);
});
test("application alerts require exact workload and cluster ownership", () => {
  const scope = {
    appName: "checkout", namespace: "shop", targetId: 1, targetCluster: cluster, environmentClusterCount: 2,
    clusters: [cluster], datasources: [{ id: 7, env: "dev", options: { scope: "cluster", cluster_id: 1 } }], pods: ["checkout-abc"], services: ["checkout"], workloads: ["checkout"]
  };
  assert.equal(O.alertBelongsToScope({ datasource_id: 7, labels: { namespace: "shop", pod: "checkout-abc" } }, scope), true);
  assert.equal(O.alertBelongsToScope({ datasource_id: 8, labels: { namespace: "shop", pod: "checkout-abc" } }, scope), false);
  assert.equal(O.alertBelongsToScope({ datasource_id: 7, labels: { namespace: "other", pod: "checkout-abc" } }, scope), false);
  assert.equal(O.alertBelongsToScope({ labels: { namespace: "shop", pod: "checkout-abc" } }, { ...scope, datasources: [] }), false);
  assert.equal(O.alertBelongsToScope({ labels: { namespace: "shop", pod: "checkout-ab" } }, { ...scope, datasources: [], environmentClusterCount: 1 }), false);
  assert.equal(O.alertBelongsToScope({ labels: { namespace: "shop", app: "checkout" } }, { ...scope, datasources: [], environmentClusterCount: 1 }), false);
  const central = { ...scope, datasources: [{ id: 9, env: "", options: { scope: "central", cluster_label: "cluster", cluster_values: { 1: "uid-a" } } }] };
  assert.equal(O.alertBelongsToScope({ datasource_id: 9, labels: { namespace: "shop", app: "checkout", cluster: "uid-a" } }, central), true);
  assert.equal(O.alertBelongsToScope({ datasource_id: 9, labels: { namespace: "shop", app: "checkout" } }, central), false);
});
