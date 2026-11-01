import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

const source = fs.readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");
const page = fs.readFileSync(new URL("../../public/index.html", import.meta.url), "utf8");

test("live-core creation dialogs bind structured forms and no longer expose raw JSON fields", () => {
  assert.match(source, /XSStructuredForms\.bind\(f\)/);
  assert.match(source, /structuredTableField\("servicePorts"/);
  assert.match(source, /structuredTableField\("selector"/);
  assert.match(source, /structuredTableField\("ingressRules"/);
  assert.match(source, /structuredTableField\("hpa"/);
  assert.match(source, /structuredTableField\("cronJob"/);
  assert.match(source, /storageMapField\("parameters"/);
  assert.match(source, /XSStructuredForms\.secret/);
  assert.doesNotMatch(source, /(端口|选择器|路由规则|TLS|spec|标签|Annotations|参数|Data|BinaryData)[^<"']{0,24}JSON/);
});

test("application shell loads shared form assets before live renderers", () => {
  const css = page.indexOf('href="xs-structured-forms.css"');
  const configCss = page.indexOf('href="xs-network-config-forms.css"');
  const forms = page.indexOf('src="xs-structured-forms.js"');
  const network = page.indexOf('src="xs-network-config.js"');
  const extensions = page.indexOf('src="xs-cluster-extensions.js"');
  const liveCore = page.indexOf('src="xs-live-core.js"');
  assert.ok(css >= 0 && configCss > css);
  assert.ok(forms >= 0 && network > forms && extensions > network);
  assert.ok(liveCore > extensions, "cluster extensions must be registered before live-core boots");
});

test("service topology and policy visualization enter one policy-overlay view", () => {
  assert.match(page, /if \(pg\.k === "topo"\) return renderNetworkFlowLive\(el, \{ policy: true \}\)/);
  assert.match(page, /if \(pg\.k === "graph"\) return renderNetworkFlowLive\(el, \{ policy: true \}\)/);
  assert.match(page, /netpol: \(\) => s === "graph" && go\("#\/traffic\/topo"\)/);
  assert.doesNotMatch(page, /k: "graph", t: "策略可视化"/);
  assert.match(page, /data-flow-policy/);
  assert.match(page, /\/network\/endpoints/);
});
