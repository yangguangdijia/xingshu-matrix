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
  setTimeout, clearTimeout, setInterval, clearInterval
});
const source = fs.readFileSync(new URL("../../public/xs-live-ops.js", import.meta.url), "utf8");
vm.runInContext(source, context);
const ops = window.XSLiveOps._internals;
const plain = (value) => JSON.parse(JSON.stringify(value));

test("PromQL templates carry the known-template flag and suitable ranges", () => {
  assert.deepEqual(plain(ops.promTemplates.map((item) => item.label)), ["CPU", "内存", "磁盘", "Pod", "重启", "网络", "API 错误"]);
  const cpu = plain(ops.promTemplateSelection("cpu"));
  assert.equal(cpu.knownTemplate, true);
  assert.equal(cpu.range, "1h");
  assert.match(cpu.query, /node_cpu_seconds_total/);
  assert.equal(ops.promTemplateSelection("custom"), null);
  assert.match(source, /scopedPromQuery\(OB\.query, scope, true\)/);
  assert.match(source, /: window\.XSObserve\.scopedPromQuery\(OB\.query, scope\)/);
});

test("delivery hash parser never treats apps query as an application name", () => {
  const parsed = plain(ops.parseDeliveryHash("#/delivery/apps?helm_chart=bitnami%2Fnginx&helm_repo=https%3A%2F%2Fcharts.example.com&helm_version=18.2.1&helm_env=prod"));
  assert.equal(parsed.appName, "");
  assert.equal(parsed.pane, "");
  assert.deepEqual(parsed.helm, {
    name: "prod",
    target_id: "",
    namespace: "",
    helm: { chart: "bitnami/nginx", repo: "https://charts.example.com", version: "18.2.1", release: "" }
  });
  assert.deepEqual(plain(ops.parseDeliveryHash("#/delivery/payments/%E5%AF%BC%E8%A7%88")), { appName: "payments", pane: "导览", helm: null });
  assert.match(source, /history\.replaceState\(null, "", "#\/delivery\/apps"\)/);
  const deliveryListSource = source.slice(source.indexOf("function deliveryList"), source.indexOf("function appModal"));
  assert.doesNotMatch(deliveryListSource, /setTimeout/);
  assert.match(deliveryListSource, /p\.active\(\).*#\/delivery\/apps/s);
  assert.match(source, /仍需补齐应用名、代码仓库、镜像仓库、执行集群、Namespace 和 Release/);
  assert.match(source, /表单不会自动提交/);
});

test("Helm environment contract preserves repository and version", () => {
  const env = plain(ops.normalizeAppEnvValues({
    name: "prod", target_id: "7", namespace: "payments", helm_chart: "payments/api",
    helm_repo: "https://charts.example.com", helm_version: "2.4.0", helm_release: "payments"
  }));
  assert.deepEqual(env, {
    name: "prod", target_id: 7, namespace: "payments",
    helm: { chart: "payments/api", release: "payments", repo: "https://charts.example.com", version: "2.4.0" }
  });
  assert.match(ops.appEnvRowHTML(env), /data-app-env-field="helm_repo"/);
  assert.match(ops.appEnvRowHTML(env), /data-app-env-field="helm_version"/);
  assert.throws(() => ops.normalizeAppEnvValues({ name: "prod", target_id: "7", namespace: "payments", helm_chart: "payments/api" }), /Chart 与 Release/);
});

test("application edits preserve Git Token when blank or masked", () => {
  const base = {
    name: "payments", repo_url: "https://git.example/payments.git", branch: "main",
    dockerfile: "Dockerfile", context_dir: ".", image_repo: "registry.example/payments",
    git_username: "git", envs: [{ name: "prod" }]
  };
  assert.equal(Object.hasOwn(plain(ops.applicationPayload({ ...base, git_token: "" })), "git_token"), false);
  assert.equal(Object.hasOwn(plain(ops.applicationPayload({ ...base, git_token: "••••••" })), "git_token"), false);
  assert.equal(Object.hasOwn(plain(ops.applicationPayload({ ...base, git_token: "********" })), "git_token"), false);
  assert.equal(Object.hasOwn(plain(ops.applicationPayload({ ...base, git_token: "------" })), "git_token"), false);
  assert.equal(plain(ops.applicationPayload({ ...base, git_token: "real*token" })).git_token, "real*token");
  assert.equal(plain(ops.applicationPayload({ ...base, git_token: "replacement" })).git_token, "replacement");
  assert.match(source, /PUT\("\/cicd\/applications\/" \+ encodeURIComponent\(app\.id\), payload\)/);
  assert.doesNotMatch(source, /value: source\.git_token/);
});

test("middleware type defaults add hints without overwriting user input", () => {
  const existing = plain(ops.middlewareTypeDefaults("postgres", {
    endpoints: "db.internal:15432", tls: true, tls_skip_verify: false
  }));
  assert.equal(existing.endpoints, "db.internal:15432");
  assert.equal(existing.tls, true);
  assert.equal(existing.tls_skip_verify, false);
  assert.equal(existing.endpointPlaceholder, undefined);
  const empty = plain(ops.middlewareTypeDefaults("redis", {}));
  assert.equal(empty.endpointPlaceholder, "redis.example.com:6379");
  assert.equal(ops.middlewareTypeDefaults("rabbitmq", {}).endpointPlaceholder, "https://rabbitmq.example.com:15672");
  assert.equal(ops.middlewareTypeDefaults("rocketmq", {}).endpointPlaceholder, "https://rocketmq.example.com:8080");
  assert.equal(empty.tls, false);
  assert.equal(empty.tls_skip_verify, false);
  assert.match(source, /kind: "details", label: "高级设置"/);
});

test("middleware server search keeps type and pagination, then treats the response as authoritative", () => {
  assert.deepEqual(plain(ops.middlewareListQuery("kafka", "  orders  ")), {
    page: 1, page_size: 100, type: "kafka", keyword: "orders"
  });
  assert.deepEqual(plain(ops.middlewareListQuery("", "")), { page: 1, page_size: 100 });
  assert.match(source, /GET\("\/middleware\/instances", middlewareListQuery\(tab, requestedSearch\)\)\.then\(function \(data\) \{\s*if \(!p\.active\(t\)\) return;/);
  assert.match(source, /serverSearchText === searchText\.trim\(\)/);
  assert.match(source, /setTimeout\(load, 150\)/);
  assert.match(source, /正在从服务端搜索全部实例/);
});

test("middleware payload includes advanced fields and blank edit password", () => {
  const payload = plain(ops.middlewareInstancePayload({
    name: "events", type: "kafka", env: "prod", endpoints: "kafka-1:9092\nkafka-2:9092",
    username: "svc", password: "", tls: true, tls_skip_verify: true,
    sasl_mechanism: "SCRAM-SHA-512", options: { client_id: "matrix", retries: "3" }, description: "event bus"
  }));
  assert.deepEqual(payload, {
    name: "events", type: "kafka", env: "prod", endpoints: ["kafka-1:9092", "kafka-2:9092"],
    description: "event bus",
    auth: { username: "svc", password: "", tls: true, tls_skip_verify: true, sasl_mechanism: "SCRAM-SHA-512" },
    options: { client_id: "matrix", retries: "3" }
  });
  assert.match(source, /data-mwedit/);
  assert.match(source, /PUT\("\/middleware\/instances\/" \+ encodeURIComponent\(existing\.id\), body\)/);
  assert.match(source, /middleware:manage/);
  assert.match(source, /SCRAM-SHA-256/);
  assert.match(source, /SCRAM-SHA-512/);
});

test("middleware search uses only the real supplied instance list", () => {
  const instances = [
    { id: 1, name: "orders-db", type: "postgres", env: "prod", endpoints: ["db.prod:5432"] },
    { id: 2, name: "events", type: "kafka", env: "dev", endpoints: ["kafka.dev:9092"] }
  ];
  assert.deepEqual(plain(ops.filterMiddlewareInstances(instances, "prod")), [instances[0]]);
  assert.deepEqual(plain(ops.filterMiddlewareInstances(instances, "9092")), [instances[1]]);
  assert.equal(ops.filterMiddlewareInstances(instances, "").length, 2);
});

test("release search stays within the real supplied release list", () => {
  const releases = [
    { id: 11, env: "prod", kind: "deploy", image: "registry/api:v2", status: "succeeded", created_by: "alice" },
    { id: 12, env: "test", kind: "rollback", image: "registry/api:v1", status: "failed", created_by: "bob" }
  ];
  assert.deepEqual(plain(ops.filterReleaseRecords(releases, "rollback")), [releases[1]]);
  assert.deepEqual(plain(ops.filterReleaseRecords(releases, "alice")), [releases[0]]);
  assert.equal(ops.filterReleaseRecords(releases, "").length, 2);
});

test("build search stays within the real supplied filtered run list", () => {
  const runs = [
    { id: 21, app_id: 1, pipeline_id: 4, status: "succeeded", branch: "main", commit: "abc123", triggered_by: "alice" },
    { id: 22, app_id: 2, pipeline_id: 5, status: "failed", branch: "hotfix", commit: "def456", triggered_by: "bob" }
  ];
  const apps = [{ id: 1, name: "payments" }, { id: 2, name: "portal" }];
  const pipelines = [{ id: 4, name: "release" }, { id: 5, name: "verify" }];
  assert.deepEqual(plain(ops.filterBuildRecords(runs, "payments", apps, pipelines)), [runs[0]]);
  assert.deepEqual(plain(ops.filterBuildRecords(runs, "hotfix", apps, pipelines)), [runs[1]]);
  assert.equal(ops.filterBuildRecords(runs, "", apps, pipelines).length, 2);
  assert.match(source, /data-mwsearch/);
  assert.match(source, /data-relsearch/);
  assert.match(source, /data-cfsearch/);
});
