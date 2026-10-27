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
vm.runInContext(liveOpsSource, context);
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
  assert.match(liveOpsSource, /GET\("\/cicd\/runs", \{ pipeline_id: selected\.id/);
  assert.match(liveOpsSource, /GET\("\/cicd\/runs\/" \+ encodeURIComponent\(newest\.id\)\)/);
  assert.match(liveOpsSource, /data-plrunlog/);
  assert.match(liveOpsSource, /href="#\/cicd\/pipeline\/' \+ encodeURIComponent\(pl\.id\)/);
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
