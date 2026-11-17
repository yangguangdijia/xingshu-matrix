import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";
import { Element, tick, deferred } from "./xs-resource-pages.harness.mjs";

function load(api, { permissions = ["workload:read"] } = {}) {
  const calls = [], downloads = [], copies = [];
  const body = new Element("body");
  const document = {
    body,
    getElementById: (id) => body.querySelector('[id="' + id + '"]'),
    querySelector: (selector) => body.querySelector(selector),
    createElement: (tag) => {
      const element = new Element(tag);
      element.click = () => downloads.push({ href: element.href, filename: element.download });
      return element;
    }
  };
  const window = {
    document, Blob, URL: { createObjectURL: () => "blob:manifest", revokeObjectURL() {} }, setTimeout,
    navigator: { clipboard: { writeText: async (text) => { copies.push(text); } } },
    dlg: (_title, content, footer) => { body.innerHTML = content + footer; },
    location: { origin: "https://matrix.example.com" },
    XS: {
      can: (permission) => permissions.includes(permission),
      isApproval: (e) => e && e.code === 40304,
      api: async (path, options = {}) => { calls.push({ path, options }); return api(path, options); }
    }
  };
  vm.runInNewContext(fs.readFileSync(new URL("../../public/xs-cluster-agent.js", import.meta.url), "utf8"), { window, URL, console });
  return { agent: window.XSClusterAgent, calls, body, document, downloads, copies };
}

test("agent create validates and posts only name, env and description", async () => {
  const { agent, calls } = load(async () => ({ id: 9, name: "edge-1", connect_mode: "agent" }));
  const res = await agent.create({ name: " edge-1 ", env: "生产", description: "边缘" });
  assert.equal(res.pending, false);
  assert.equal(res.cluster.id, 9);
  assert.equal(calls[0].path, "/clusters/agent");
  assert.equal(calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].options.body)), { name: "edge-1", env: "生产", description: "边缘" });
  for (const bad of [{ name: "Edge", env: "生产" }, { name: "edge_1", env: "生产" }, { name: "edge", env: "" }, { name: "edge", env: "+" }]) {
    await assert.rejects(agent.create(bad));
  }
  assert.equal(calls.length, 1, "invalid input must not reach the server");
});

test("agent create reports approval as pending, never as success", async () => {
  const { agent } = load(async () => { const e = new Error("pending"); e.code = 40304; e.approval = { ticket_id: 55 }; throw e; });
  const res = await agent.create({ name: "edge-1", env: "生产" });
  assert.equal(res.pending, true);
  assert.equal(res.cluster, null);
  assert.equal(res.ticket, 55);
});

test("manifest request requires a clean HTTPS platform address", () => {
  const { agent } = load(async () => ({}));
  const req = agent._internals.manifestRequest;
  assert.deepEqual(JSON.parse(JSON.stringify(req({ server_url: "https://matrix.example.com/", enable_cicd: true }))),
    { server_url: "https://matrix.example.com", image: "", namespace: "", enable_cicd: true, allow_insecure_http: false });
  for (const bad of ["http://matrix.example.com", "https://u:p@matrix.example.com", "https://matrix.example.com/api", "https://matrix.example.com?a=1", "not a url"]) {
    assert.throws(() => req({ server_url: bad }), undefined, bad);
  }
  assert.equal(req({ server_url: "http://10.0.0.5:30080", allow_insecure_http: true }).allow_insecure_http, true);
  assert.equal(req({ server_url: "https://matrix.example.com", allow_insecure_http: true }).allow_insecure_http, false);
});

test("cluster list shows agent connection state and the manifest action only for agent clusters", () => {
  const source = fs.readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");
  const sandbox = { console: { log() {}, error() {} }, process: { versions: { node: "test" }, exitCode: 0 }, module: { exports: {} } };
  sandbox.globalThis = sandbox;
  vm.runInNewContext(source, sandbox);
  const status = sandbox.module.exports.pure.clusterAgentStatus;
  assert.deepEqual([...status({ agent_online: true })], ["agent 已连接", "ok"]);
  assert.deepEqual([...status({ agent_online: false })], ["等待 agent 接入", "warn"]);
  assert.deepEqual([...status({ agent_online: false, agent_bound: true })], ["agent 未连接", "bad"]);
  const list = source.slice(source.indexOf("function clListPage"), source.indexOf("function clNodesPage"));
  assert.match(list, /agent \? actBtn\("接入清单", "cl-agent"/);
});

test("目标集群命令显式固定 context，下载文件名与安装命令一致", () => {
  const { agent } = load(async () => ({}));
  const helpers = agent._internals;
  const commands = helpers.installCommands({ id: 9 }, " target-prod ", "matrix-system");
  assert.equal(helpers.manifestFile({ id: 9 }), "matrix-cluster-agent-9.yaml");
  for (const command of commands.split("\n")) assert.match(command, /^kubectl --context='target-prod' /);
  assert.match(commands, /apply -f 'matrix-cluster-agent-9.yaml'/);
  assert.match(commands, /get namespace kube-system -o jsonpath='\{.metadata.uid\}'/);
  assert.throws(() => helpers.installCommands({ id: 9 }, "", ""), /目标集群/);
  assert.throws(() => helpers.installCommands({ id: 9 }, "bad\ncontext", ""), /控制字符/);
  assert.throws(() => helpers.manifestFile({ id: "../x" }), /编号/);
  assert.ok(helpers.installCommands({ id: 9 }, "prod'$(command)", "matrix-system").includes("--context='prod'\\''$(command)'"));
});

const cluster = { id: 9, name: "自定义登记标签", connect_mode: "agent" };
const manifest = { cluster_id: 9, manifest: "kind: Secret\nstringData:\n  token: fixture-only", namespace: "matrix-system", server_url: "https://matrix.example.com" };
const identity = { exit_code: 0, output: JSON.stringify({ kind: "Namespace", metadata: { name: "kube-system", uid: "actual-cluster-uid" } }) };

async function generate(harness) {
  harness.agent.openManifest(cluster);
  const form = harness.document.getElementById("xca-form");
  form.elements.target_context.value = "target-prod";
  form.onsubmit({ preventDefault() {} });
  await tick();
  return form;
}

test("清单引导要求核对目标，不将登记名称或生成清单当作接入成功", async () => {
  const harness = load(async () => manifest);
  harness.agent.openManifest(cluster);
  const form = harness.document.getElementById("xca-form");
  assert.match(harness.body.innerHTML, /名称只是登记标签/);
  assert.match(harness.body.innerHTML, /InClusterConfig/);
  assert.match(harness.body.innerHTML, /kubectl config current-context/);
  assert.match(harness.body.innerHTML, /不是 Kubernetes API 地址/);
  form.onsubmit({ preventDefault() {} });
  await tick();
  assert.equal(harness.calls.length, 0);
  assert.equal(form.querySelector("[data-xca-err]").hidden, false);
  form.elements.target_context.value = "target-prod";
  form.onsubmit({ preventDefault() {} });
  form.onsubmit({ preventDefault() {} });
  await tick();
  assert.equal(harness.calls.length, 1);
  assert.equal("target_context" in harness.calls[0].options.body, false);
  const result = form.querySelector("[data-xca-result]");
  assert.match(result.innerHTML, /清单已生成，尚未确认代理连接/);
  assert.match(result.innerHTML, /含 Secret 接入令牌，禁止提交到 Git/);
  assert.match(result.innerHTML, /cluster-admin/);
  assert.doesNotMatch(result.innerHTML, /role="tablist"/);
  result.querySelector("[data-xca-download]").onclick();
  assert.equal(harness.downloads[0].filename, "matrix-cluster-agent-9.yaml");
  assert.ok(result.querySelector("[data-xca-commands]").textContent.includes(harness.downloads[0].filename));
  result.querySelector("[data-xca-copy]").onclick();
  await tick();
  assert.deepEqual(harness.copies, [manifest.manifest]);
  assert.equal(form.querySelector("[data-xca-rotation]").hidden, false);
});

test("连接检查从实际集群读取 kube-system UID，不把名称当身份", async () => {
  const harness = load(async (path) => {
    if (path.endsWith("/manifest")) return manifest;
    if (path.endsWith("/kubectl")) return identity;
    return { ...cluster, agent_online: true, agent_bound: true, agent_version: "v1", agent_seen_at: "2026-11-16T08:00:00Z" };
  });
  const form = await generate(harness);
  form.querySelector("[data-xca-check]").onclick();
  await tick();
  assert.match(form.querySelector("[data-xca-status]").textContent, /登记 #9.*已绑定集群指纹.*v1.*2026-11-16/);
  const text = form.querySelector("[data-xca-identity]").textContent;
  assert.match(text, /实际 kube-system UID：actual-cluster-uid/);
  assert.doesNotMatch(text, /自定义登记标签/);
  assert.match(text, /终端输出核对/);
  const request = harness.calls.at(-1);
  assert.equal(request.path, "/clusters/9/kubectl");
  assert.equal(request.options.body.command, "get namespace kube-system -o json");
});

test("无工作负载读取权限时不调用身份接口，也不声称已核对身份", async () => {
  const harness = load(async (path) => path.endsWith("/manifest") ? manifest : { ...cluster, agent_online: true }, { permissions: [] });
  const form = await generate(harness);
  form.querySelector("[data-xca-check]").onclick();
  await tick();
  assert.match(form.querySelector("[data-xca-identity]").textContent, /需要工作负载只读权限/);
  assert.equal(harness.calls.some((call) => call.path.endsWith("/kubectl")), false);
});

test("未连接、错误登记和身份读取失败均不显示身份验证成功", async () => {
  for (const state of ["offline", "wrong-id", "denied"]) {
    const harness = load(async (path) => {
      if (path.endsWith("/manifest")) return manifest;
      if (path.endsWith("/kubectl")) throw new Error("无读取权限");
      return { ...cluster, id: state === "wrong-id" ? 10 : 9, agent_online: state !== "offline" };
    });
    const form = await generate(harness);
    form.querySelector("[data-xca-check]").onclick();
    await tick();
    const status = form.querySelector("[data-xca-status]").textContent;
    const identityText = form.querySelector("[data-xca-identity]").textContent;
    if (state === "offline") assert.match(identityText, /暂不能确认/);
    if (state === "wrong-id") assert.match(status, /登记信息不一致/);
    if (state === "denied") assert.match(identityText, /身份核对失败/);
    assert.doesNotMatch(identityText, /实际 kube-system UID/);
  }
});

test("身份解析拒绝命令错误、截断输出和其他资源", () => {
  const parse = load(async () => ({})).agent._internals.clusterIdentity;
  assert.equal(parse(identity), "actual-cluster-uid");
  for (const bad of [null, { ...identity, exit_code: 1 }, { ...identity, truncated: true }, { ...identity, output: "not json" },
    { ...identity, output: JSON.stringify({ kind: "Namespace", metadata: { name: "default", uid: "other" } }) },
    { ...identity, output: JSON.stringify({ kind: "Namespace", metadata: { name: "kube-system" } }) }]) assert.throws(() => parse(bad));
});

test("重新生成前清除旧清单，失败和审批均不保留失效下载入口", async () => {
  let attempts = 0;
  const harness = load(async () => {
    attempts += 1;
    if (attempts === 1) return manifest;
    throw Object.assign(new Error("等待审批"), { code: 40304 });
  });
  const form = await generate(harness);
  form.onsubmit({ preventDefault() {} });
  assert.equal(form.querySelector("[data-xca-result]").innerHTML, "");
  await tick();
  assert.match(form.querySelector("[data-xca-err]").textContent, /需要审批/);
  assert.equal(form.querySelector("[data-xca-download]"), null);
});

test("重新生成后忽略旧连接检查的异步结果", async () => {
  const connection = deferred();
  const harness = load(async (path) => path.endsWith("/manifest") ? manifest : connection.promise);
  const form = await generate(harness);
  form.querySelector("[data-xca-check]").onclick();
  form.onsubmit({ preventDefault() {} });
  await tick();
  connection.resolve({ ...cluster, agent_online: true });
  await tick();
  assert.match(form.querySelector("[data-xca-status]").textContent, /尚未确认代理连接/);
  assert.equal(harness.calls.some((call) => call.path.endsWith("/kubectl")), false);
});

test("import dialog offers agent onboarding in live mode without asking for an API server", () => {
  const html = fs.readFileSync(new URL("../../public/index.html", import.meta.url), "utf8");
  assert.match(html, /liveMode \? \["kubeconfig", "Agent"\]/);
  assert.match(html, /data-m="\$\{liveMode \? "" : "Agent"\}" hidden><label><span class="x-req">API Server 地址/);
  assert.match(html, /if \(liveMode && m === "Agent"\) \{[\s\S]*XSClusterAgent\.create\(/);
  assert.match(html, /<script src="xs-cluster-agent\.js"><\/script>/);
});
