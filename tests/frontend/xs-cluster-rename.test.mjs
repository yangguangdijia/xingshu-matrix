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

test("cluster edit payload renames without changing environment or credentials", () => {
  const cluster = { id: 7, name: "prod-a", env: "生产" };
  const payload = P.clusterEditPayload(cluster, { name: "  prod-east  ", description: " 华东 " });
  assert.deepEqual(JSON.parse(JSON.stringify(payload)), { name: "prod-east", env: "生产", description: "华东" });
  assert.equal("kubeconfig" in payload, false);
  const replaced = P.clusterEditPayload(cluster, { name: "prod-a", kubeconfig: "apiVersion: v1\n" });
  assert.equal(replaced.kubeconfig, "apiVersion: v1");
});

test("cluster edit payload rejects empty, oversized and unsafe names", () => {
  const cluster = { id: 7, name: "prod-a", env: "生产" };
  for (const name of ["", "   ", "a/b", "a\\b", "x\ny", "集".repeat(129)]) {
    assert.throws(() => P.clusterEditPayload(cluster, { name }), /集群名称/);
  }
  assert.equal(P.clusterEditPayload(cluster, { name: "集".repeat(128) }).name.length, 128);
});

test("cluster list offers edit instead of the environment change the server rejects", () => {
  const list = section("function clListPage", "function clNodesPage");
  assert.match(list, /actBtn\("编辑", "cl-edit"/);
  assert.doesNotMatch(list, /cl-env|改环境/);
  const dialog = section("function clusterEditDialog", "function clListPage");
  assert.match(dialog, /name="name"[^>]*required/);
  assert.match(dialog, /value="' \+ gEsc\(cluster\.env \|\| "—"\) \+ '" disabled/);
  assert.match(dialog, /留空保持不变/);
  assert.match(dialog, /if \(updated\)/, "approval responses must not be reported as a rename");
});
