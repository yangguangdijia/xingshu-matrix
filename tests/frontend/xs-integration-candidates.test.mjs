import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-live-ops.js", import.meta.url), "utf8");

test("observability data sources are reachable and expose edit plus manual URL controls", () => {
  assert.match(source, /observe:\s*renderObserveLive/);
  assert.match(source, /data-newds/);
  assert.match(source, /data-editds/);
  assert.match(source, /data-ds-candidate/);
  assert.match(source, /name="url"/);
  assert.match(source, /手动填写/);
});

test("cluster discovery distinguishes candidates and does not imply connectivity", () => {
  assert.match(source, /\/integration-candidates/);
  assert.match(source, /forbidden/);
  assert.match(source, /unavailable/);
  assert.match(source, /发现不代表连接成功/);
  assert.match(source, /data-ds-candidate/);
  assert.match(source, /item\.connection_verified === true && item\.auth_type === "none"/);
  assert.match(source, /choices\.length === 1 && choices\[0\]\.url_candidates\.length === 1/);
  assert.match(source, /候选地址尚未验证连接或归属，不自动绑定/);
});

test("candidate parsing preserves discovery and verified-connection as separate facts", () => {
  const document = { body: { contains: () => false, appendChild() {} }, head: { appendChild() {} }, documentElement: { appendChild() {} }, addEventListener() {}, createElement: () => ({ style: {}, setAttribute() {} }), getElementById: () => null };
  const window = { addEventListener() {} };
  vm.runInNewContext(source, { window, document, location: { hash: "" }, console, setTimeout, setInterval, clearInterval });
  const parse = window.XSLiveOps._internals.integrationCandidates;
  const result = parse({ items: [{ type: "loki", name: "loki", state: "available", url_candidates: ["http://loki.observability.svc:3100"], connection_verified: false }], components: { loki: { state: "available" } } }, "loki");
  assert.equal(result.component.state, "available");
  assert.equal(result.items[0].connection_verified, false);
  assert.equal(parse({ components: { loki: { state: "forbidden" } } }, "loki").component.state, "forbidden");
  assert.equal(parse({ components: { loki: { state: "error" } } }, "loki").component.state, "error");
});

test("legacy observability settings are imported explicitly and datasource options use fields", () => {
  assert.match(source, /data-import-old/);
  assert.match(source, /obs\.prom/);
  assert.match(source, /obs\.log/);
  assert.match(source, /data-ds-option-add/);
  assert.doesNotMatch(source, /label:\s*"options JSON"/);
});

test("ops creation flows use structured rows instead of visible JSON textareas", () => {
  assert.match(source, /k:\s*"envs",\s*label:\s*"部署环境",\s*kind:\s*"appEnvs"/);
  assert.match(source, /k:\s*"steps",\s*label:\s*"流程步骤",\s*kind:\s*"automationSteps"/);
  assert.match(source, /k:\s*"options",[\s\S]{0,120}structuredKind:\s*"keyValue"/);
  assert.match(source, /k:\s*"extra_headers",[\s\S]{0,120}structuredKind:\s*"keyValue"/);
  assert.doesNotMatch(source, /label:\s*"[^"]*JSON[^"]*",\s*kind:\s*"textarea"/);
});
