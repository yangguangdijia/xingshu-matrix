import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

function load(api) {
  const calls = [];
  const window = {
    location: { origin: "https://matrix.example.com" },
    XS: {
      isApproval: (e) => e && e.code === 40304,
      api: async (path, options = {}) => { calls.push({ path, options }); return api(path, options); }
    }
  };
  vm.runInNewContext(fs.readFileSync(new URL("../../public/xs-cluster-agent.js", import.meta.url), "utf8"), { window, URL, console });
  return { agent: window.XSClusterAgent, calls };
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

test("import dialog offers agent onboarding in live mode without asking for an API server", () => {
  const html = fs.readFileSync(new URL("../../public/index.html", import.meta.url), "utf8");
  assert.match(html, /liveMode \? \["kubeconfig", "Agent"\]/);
  assert.match(html, /data-m="\$\{liveMode \? "" : "Agent"\}" hidden><label><span class="x-req">API Server 地址/);
  assert.match(html, /if \(liveMode && m === "Agent"\) \{[\s\S]*XSClusterAgent\.create\(/);
  assert.match(html, /<script src="xs-cluster-agent\.js"><\/script>/);
});
