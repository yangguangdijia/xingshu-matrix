import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const body = {
  children: [],
  contains(node) { return this.children.includes(node); },
  appendChild(node) { this.children.push(node); }
};
const document = {
  body,
  head: { appendChild() {} },
  documentElement: { appendChild() {} },
  addEventListener() {},
  createElement() { return { style: {}, setAttribute() {} }; },
  getElementById() { return null; }
};
const window = { addEventListener() {} };
const context = vm.createContext({
  window, document, location: { hash: "" }, console,
  setTimeout, setInterval, clearInterval
});
const liveOpsSource = fs.readFileSync(new URL("../../public/xs-live-ops.js", import.meta.url), "utf8");
const visualCSS = fs.readFileSync(new URL("../../public/xs-ops-visual-polish.css", import.meta.url), "utf8");
vm.runInContext(liveOpsSource, context);
vm.runInContext(fs.readFileSync(new URL("../../public/xs-observability.js", import.meta.url), "utf8"), context);
const ops = window.XSLiveOps._internals;
const plain = (value) => JSON.parse(JSON.stringify(value));

test("Jenkins parameters preserve runtime variables and values containing equals", () => {
  assert.deepEqual(plain(ops.parseJenkinsParameters("BRANCH=${BRANCH}\nIMAGE=${IMAGE}\nFILTER=a=b")), {
    BRANCH: "${BRANCH}",
    IMAGE: "${IMAGE}",
    FILTER: "a=b"
  });
});

test("empty discovered Jenkins parameters leave the Job default unchanged", () => {
  assert.deepEqual(plain(ops.parseJenkinsParameters("TOKEN=\nBRANCH=main")), { BRANCH: "main" });
});

test("Jenkins parameters reject malformed and duplicate names", () => {
  assert.throws(() => ops.parseJenkinsParameters("NO_EQUALS"), /参数名=参数值/);
  assert.throws(() => ops.parseJenkinsParameters("TOKEN=x\nTOKEN=y"), /重复/);
  assert.throws(() => ops.parseJenkinsParameters("BAD KEY=x"), /参数名无效/);
});

test("pipeline form builds the backend stage contract", () => {
  const stages = ops.buildPipelineStages([
    { name: "test", type: "test", image: "golang:1.23", commands: "go test ./...\n\ngo vet ./...", timeout_sec: "600" },
    { name: "image", type: "build", registry_secret: "harbor-push" },
    { name: "deploy-prod", type: "deploy", deploy_env: "prod" },
    { name: "notify", type: "jenkins", jenkins_id: "4", jenkins_job: "release/notify", params_text: "COMMIT=${COMMIT}" }
  ], ["dev", "prod"], [4]);
  assert.deepEqual(plain(stages), [
    { name: "test", type: "test", timeout_sec: 600, image: "golang:1.23", commands: ["go test ./...", "go vet ./..."] },
    { name: "image", type: "build", registry_secret: "harbor-push" },
    { name: "deploy-prod", type: "deploy", deploy_env: "prod" },
    { name: "notify", type: "jenkins", jenkins_id: 4, jenkins_job: "release/notify", params: { COMMIT: "${COMMIT}" } }
  ]);
});

test("metrics page is a full-screen dashboard with large trends and timed refresh", () => {
  const dashboard = liveOpsSource.slice(liveOpsSource.indexOf("function obsMetrics"), liveOpsSource.indexOf("function runProm"));
  assert.match(liveOpsSource, /range: "24h"/);
  assert.match(dashboard, /data-dashboard-fullscreen/);
  assert.match(dashboard, /requestFullscreen/);
  assert.match(dashboard, /setInterval\(function \(\)[\s\S]*?loadMetricSummary\(OB\.scope\.cluster, OB\.scope\)[\s\S]*?\}, 30000\)/);
  assert.match(dashboard, /ops-metric-trend-grid/);
  assert.match(dashboard, /metricTrend\(items\[0\]/);
  assert.match(dashboard, /metricTrend\(items\[1\]/);
  assert.match(dashboard, /metricTrend\(items\[2\]/);
  assert.match(visualCSS, /\.ops-metrics-page:fullscreen/);
  assert.match(visualCSS, /\.ops-metric-trend-grid\s*\{[\s\S]*?repeat\(3/);
});

test("pipeline stages reject unsafe or incomplete configurations", () => {
  assert.throws(() => ops.buildPipelineStages([], [], []), /至少配置/);
  assert.throws(() => ops.buildPipelineStages([
    { name: "same", type: "build" }, { name: "same", type: "build" }
  ], [], []), /重复/);
  assert.throws(() => ops.buildPipelineStages([
    { name: "tests", type: "test", image: "golang:1.23", commands: "" }
  ], [], []), /至少一条执行命令/);
  assert.throws(() => ops.buildPipelineStages([
    { name: "deploy", type: "deploy", deploy_env: "prod" }
  ], ["dev"], []), /已配置的部署环境/);
  assert.throws(() => ops.buildPipelineStages([
    { name: "jenkins", type: "jenkins", jenkins_id: 4, jenkins_job: "release" }
  ], [], []), /已接入的 Jenkins/);
});

test("pipeline run stages merge by backend index and preserve unconfigured observations", () => {
  const stages = ops.pipelineRunStages([
    { name: "source", type: "script" },
    { name: "compile", type: "build" },
    { name: "release", type: "deploy" }
  ], [
    { idx: 1, name: "compile", type: "build", status: "running", started_at: "2026-10-28T00:00:00Z" },
    { idx: 0, name: "source", type: "script", status: "succeeded", started_at: "2026-10-28T00:00:00Z", finished_at: "2026-10-28T00:01:00Z" },
    { idx: 4, name: "cleanup", type: "jenkins", status: "skipped" }
  ]);
  assert.deepEqual(plain(stages.map(({ idx, name, status, observed }) => ({ idx, name, status, observed }))), [
    { idx: 0, name: "source", status: "succeeded", observed: true },
    { idx: 1, name: "compile", status: "running", observed: true },
    { idx: 2, name: "release", status: "", observed: false },
    { idx: 4, name: "cleanup", status: "skipped", observed: true }
  ]);
  assert.equal(ops.elapsed("2026-10-28T00:00:00Z", "2026-10-28T00:02:00Z"), "2 分钟");
  assert.equal(ops.elapsed("2026-10-28T00:00:00Z", null, Date.parse("2026-10-28T00:00:15Z")), "15 秒");
  assert.equal(ops.elapsed(null, null), "未开始");
});

test("live application delivery keeps the left workspace and searchable API-backed inventory", () => {
  assert.match(liveOpsSource, /return deliveryList\(el\);/);
  assert.match(liveOpsSource, /return deliveryWorkspace\(el, name,/);
  assert.match(liveOpsSource, /nav class="x-menu" aria-label="应用菜单"/);
  assert.match(liveOpsSource, /data-app-search/);
  assert.match(liveOpsSource, /data-app-env/);
  assert.match(liveOpsSource, /appsLoad\(\)\.then\(function \(list\)/);
  assert.doesNotMatch(liveOpsSource, /function deliveryList\(el\)[\s\S]{0,500}APPS\.map/);
});

test("CI/CD uses the main sidebar routes and a live pipeline detail instead of a horizontal subnav", () => {
  assert.match(liveOpsSource, /function cicdBuilds\(el\)[\s\S]{0,260}livePage\(el, "构建"[\s\S]*?, ""\);/);
  assert.match(liveOpsSource, /function cicdArtifacts\(el\)[\s\S]{0,260}livePage\(el, "制品"[\s\S]*?, ""\);/);
  assert.match(liveOpsSource, /function cicdPipelines\(el\)[\s\S]{0,300}livePage\(el, "流水线"[\s\S]*?, ""\);/);
  assert.match(liveOpsSource, /GET\("\/cicd\/runs", \{ pipeline_id: selectedPipeline\.id/);
  assert.match(liveOpsSource, /pipelineRunStages\(definitions, selected && selected\.stages\)/);
  assert.match(liveOpsSource, /class="x-plflow"/);
  assert.match(liveOpsSource, /class="x-plstrip"/);
  assert.match(liveOpsSource, /openRunLog\(button\.getAttribute\("data-plrunlog"\), stageNames\)/);
  assert.doesNotMatch(liveOpsSource, /ops-inline-runlog|data-plrun-select|data-plstage/);
  assert.match(liveOpsSource, /data-plrunlog/);
  assert.match(liveOpsSource, /href="#\/cicd\/pipeline\/' \+ encodeURIComponent\(pl\.id\)/);
});

test("cluster metrics auto-load from the resolved Prometheus datasource", () => {
  assert.match(liveOpsSource, /data-obsm-dashboard/);
  assert.match(liveOpsSource, /node_cpu_seconds_total/);
  assert.match(liveOpsSource, /node_memory_MemAvailable_bytes/);
  assert.match(liveOpsSource, /kube_pod_status_phase/);
  assert.match(liveOpsSource, /loadMetricSummary\(OB\.scope\.cluster, OB\.scope\)/);
  assert.match(liveOpsSource, /data-config-ds>数据源绑定/);
  assert.match(liveOpsSource, /location\.hash = "#\/cluster\/integrations"/);
  assert.doesNotMatch(liveOpsSource, /Math\.random\(|mock|generated series/i);
});

test("dashboard isolates failed metrics and application alerts are cluster-scoped", () => {
  for (const metric of ["node_filesystem_avail_bytes", "kube_node_status_condition", "kube_pod_container_status_restarts_total", "apiserver_request_total", "node_network_receive_bytes_total", "node_network_transmit_bytes_total"]) {
    assert.ok(liveOpsSource.includes(metric), `${metric} must use PromQL`);
  }
  assert.match(liveOpsSource, /scopedPromQuery\(item\.query, source, true\)/);
  assert.match(liveOpsSource, /cluster_id: activeCluster\(\)\.id/);
  assert.match(liveOpsSource, /page_size: 200, cluster_id:/);
  assert.match(liveOpsSource, /requests\.map\(function \(request\) \{ return settle\(request, null\)/);
  assert.match(liveOpsSource, /results\[index\]\.ok \? metricCard/);
});

test("live system settings retain original integration groups without duplicating notifications", () => {
  const schema = plain(ops.systemConfigSchema());
  assert.deepEqual(schema.map(({ key, label }) => [key, label]), [
    ["basic", "基础信息"],
    ["scm", "代码仓库"],
    ["registry", "镜像仓库"],
    ["k8s", "K8s 集群"],
    ["observability", "可观测性"]
  ]);
  const keys = schema.flatMap((section) => section.fields.map((field) => field.key));
  assert.ok(keys.includes("platform.name"));
  assert.ok(keys.includes("scm.token"));
  assert.ok(keys.includes("registry.pass"));
  assert.ok(keys.includes("k8s.kubeconfig"));
  assert.ok(keys.includes("obs.prom"));
  assert.ok(keys.includes("obs.log"));
  assert.equal(keys.some((key) => key.startsWith("notify.")), false);

  const html = ops.systemConfigPanels([
    { key: "scm.token", secret: true, has_value: true },
    { key: "k8s.kubeconfig", secret: true, has_value: true }
  ]);
  assert.match(html, /data-system-config="basic"/);
  assert.match(html, /data-system-config="scm"/);
  assert.match(html, /data-system-config="registry"/);
  assert.match(html, /data-system-config="k8s"/);
  assert.match(html, /data-system-config="observability"/);
  assert.match(html, /密钥已配置，不会回显/);
  assert.match(html, /data-config-result/);
  assert.doesNotMatch(html, /notify\.channel|notify\.callback|secret-value/);

  const unavailable = ops.systemConfigPanelResult(false, []);
  assert.match(unavailable, /平台配置读取失败/);
  assert.doesNotMatch(unavailable, /<form|data-system-config=/);
  assert.match(ops.systemConfigPanelResult(true, []).toString(), /data-system-config="basic"/);
});

test("current cluster context fails closed without a selected cluster", () => {
  assert.equal(ops.activeCluster(), null);
  window.XSClusterContext = { get: () => ({ kind: "all", cluster_id: "", version: 5 }) };
  assert.equal(ops.activeCluster(), null);
  window.XSClusterContext = { get: () => ({ kind: "cluster", cluster_id: "2", name: "dev-b", env: "dev", version: 6 }) };
  assert.deepEqual(plain(ops.activeCluster()), { id: "2", name: "dev-b", env: "dev", version: 6 });
  assert.equal(ops.clusterKey(), "2:6");
  window.XSClusterContext = undefined;
});

test("datasource forms build explicit cluster or central ownership without losing other options", () => {
  const existing = { options: { org_id: "tenant", other: "retained", cluster_values: { 4: "old" } } };
  const base = { name: "test", type: "prometheus", url: "https://prom.example", env: "dev", cluster_id: "1", cluster_name: "dev-a", auth_type: "none" };
  const clusterBody = ops.datasourceBody({ ...base, scope: "cluster" }, existing);
  assert.equal(clusterBody.options.scope, "cluster");
  assert.equal(clusterBody.options.cluster_id, "1");
  assert.equal(clusterBody.options.cluster_values, undefined);
  assert.equal(clusterBody.options.other, "retained");
  const centralBody = ops.datasourceBody({ ...base, scope: "central", cluster_label: "cluster", cluster_values: [{ cluster_id: "1", value: "uid-a" }, { cluster_id: "2", value: "uid-b" }] }, existing);
  assert.deepEqual(plain(centralBody.options.cluster_values), { 1: "uid-a", 2: "uid-b" });
  assert.equal(centralBody.options.cluster_id, undefined);
  assert.match(ops.datasourceScopeText(centralBody), /中央共享.*2 个集群/);
  assert.throws(() => ops.datasourceBody({ ...base, scope: "central", cluster_label: "", cluster_values: [] }), /标签值/);
  assert.throws(() => ops.datasourceBody({ ...base, scope: "central", cluster_label: "cluster", cluster_values: [{ cluster_id: "1", value: "x" }, { cluster_id: "1", value: "y" }] }), /重复/);
  assert.throws(() => ops.datasourceBody({ ...base, scope: "cluster", advanced: [{ key: "scope", value: "central" }] }), /专用表单/);
});

test("alerts and silences require explicit datasource and correct mapped cluster value", () => {
  const clusters = [{ id: 1, name: "dev-a", env: "dev" }, { id: 2, name: "dev-b", env: "dev" }];
  const sources = [
    { id: 11, type: "prometheus", env: "dev", options: { scope: "cluster", cluster_id: 1, cluster_name: "dev-a" } },
    { id: 12, type: "alertmanager", env: "", options: { scope: "central", cluster_label: "k8s_cluster", cluster_values: { 1: "uid-a", 2: "uid-b" } } },
    { id: 13, type: "prometheus", env: "dev" }
  ];
  const matches = (alert, cluster) => ops.alertMatchesCluster(alert, sources, clusters, cluster);
  assert.equal(matches({ datasource_id: 11, labels: {} }, clusters[0]), true);
  assert.equal(matches({ datasource_id: 11, labels: {} }, clusters[1]), false);
  assert.equal(matches({ datasource_id: 13, labels: { cluster: "dev-a" } }, clusters[0]), false);
  assert.equal(matches({ datasource_id: 12, labels: { k8s_cluster: "uid-a" } }, clusters[0]), true);
  assert.equal(matches({ datasource_id: 12, labels: { k8s_cluster: "uid-a" } }, clusters[1]), false);
  assert.equal(matches({ datasource_id: 12, labels: {} }, clusters[0]), false);
  const central = ops.ownedDatasource([sources[1]], clusters, clusters[1], ["alertmanager"]);
  assert.equal(ops.silenceMatchesCluster({ matchers: [{ name: "k8s_cluster", value: "uid-b", isEqual: true }] }, central), true);
  assert.equal(ops.silenceMatchesCluster({ matchers: [{ name: "k8s_cluster", value: "uid-a" }] }, central), false);
  assert.equal(ops.silenceMatchesCluster({ matchers: [] }, central), false);
});

test("legacy import requires explicit cluster scope and matches duplicate binding", () => {
  const source = { type: "loki", name: "legacy-loki", url: "https://logs.example" };
  const cluster = { id: 2, name: "dev-b", env: "dev" };
  assert.throws(() => ops.legacyDatasourceCandidate(source, null, "cluster"), /目标集群/);
  const local = ops.legacyDatasourceCandidate(source, cluster, "cluster");
  assert.equal(local.env, "dev"); assert.equal(local.options.cluster_id, 2);
  const central = ops.legacyDatasourceCandidate(source, cluster, "central");
  assert.equal(central.options.cluster_values[2], "dev-b");
  assert.equal(ops.hasDatasource([local], local), true);
  assert.equal(ops.hasDatasource([local], central), false);
  const html = ops.dsDetailRows({ ...central, options: { ...central.options, api_key: "private" } });
  assert.match(html, /#2.*dev-b/); assert.doesNotMatch(html, /private/);
});

test("rules and alerts send cluster_id, refresh datasource detail and reject stale responses", () => {
  assert.match(liveOpsSource, /GET\("\/observability\/datasources\/" \+ encodeURIComponent\(id\)\)/);
  assert.match(liveOpsSource, /request !== detailSeq \|\| !p\.active\(token\)/);
  assert.match(liveOpsSource, /GET\("\/observability\/prometheus\/" \+ encodeURIComponent\(id\) \+ "\/rules", \{ cluster_id: CLUSTERS\[0\]\.id \}\)/);
  assert.match(liveOpsSource, /var qy = \{ page: 1, page_size: 50, cluster_id: cluster\.id \}/);
  assert.match(liveOpsSource, /GET\("\/observability\/silences", \{ datasource: dsId, cluster_id: current\.id \}\)/);
  assert.match(liveOpsSource, /context !== clusterKey\(\)/);
  assert.match(liveOpsSource, /function renderObserveLive\(el\)[\s\S]{0,230}pg === "events"/);
});
