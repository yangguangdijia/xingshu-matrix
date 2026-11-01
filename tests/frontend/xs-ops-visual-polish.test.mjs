import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const read = (path) => fs.readFileSync(new URL(path, import.meta.url), "utf8");

test("isolated services are placed in a horizontal grid instead of a single column", () => {
  const context = vm.createContext({});
  vm.runInContext(read("../../public/xs-traffic-topology.js"), context);
  const nodes = Array.from({ length: 7 }, (_, index) => ({ key: `svc-${index}`, label: `service-${index}` }));
  const graph = context.XSTrafficTopology.layout(nodes, []);
  const positions = Object.values(graph.positions);
  assert.equal(positions.length, nodes.length);
  assert.ok(new Set(positions.map((position) => position.x)).size > 1);
  assert.ok(new Set(positions.map((position) => position.y)).size > 1);
});
test("connected services keep caller-to-callee layers and produce non-empty edge paths", () => {
  const context = vm.createContext({});
  vm.runInContext(read("../../public/xs-traffic-topology.js"), context);
  const graph = context.XSTrafficTopology.layout(
    [{ key: "web" }, { key: "api" }, { key: "db" }, { key: "idle-a" }, { key: "idle-b" }],
    [{ from: "web", to: "api" }, { from: "api", to: "db" }]
  );
  assert.ok(graph.positions.web.x < graph.positions.api.x);
  assert.ok(graph.positions.api.x < graph.positions.db.x);
  assert.notEqual(graph.positions["idle-a"].x, graph.positions["idle-b"].x);
  for (const edge of graph.edges) {
    assert.ok(context.XSTrafficTopology.edgePath(graph.positions[edge.from], graph.positions[edge.to], graph.nodeWidth, graph.nodeHeight));
  }
});
test("exposure view includes resource workbench controls and cluster-discovered class options", () => {
  const source = read("../../public/xs-network-config.js");
  const page = read("../../public/index.html");
  const css = read("../../public/xs-ops-visual-polish.css");
  assert.doesNotMatch(source, /data-exposure-tabs?/);
  assert.match(source, /data-exposure-kind/);
  assert.match(source, /data-exposure-search/);
  assert.match(source, /data-exposure-namespace/);
  assert.match(source, /data-exposure-class/);
  assert.match(source, /data-net-ai/);
  assert.match(source, /\/ai\/models\/options/);
  assert.match(source, /\/ai\/assist/);
  assert.match(source, /classItems\.map/);
  assert.match(source, /data-net-paths/);
  assert.match(source, /data-net-field="host"/);
  assert.doesNotMatch(source, /name="manifest"/);
  assert.match(source, /\/network\/configuration\/apply/);
  assert.doesNotMatch(source, /ctx\.cluster\.id\) \+ "\/apply\?reason=/);
  assert.match(page, /href="xs-ops-visual-polish\.css"/);
  assert.match(page, /data-flow-edge[\s\S]{0,180}tabindex="0"/);
  assert.match(css, /\.x-exposure-filters/);
  assert.match(css, /@media \(max-width: 620px\)/);
  assert.doesNotMatch(css, /var\(--x-mono\)/);
});

test("inline JavaScript in index.html parses", () => {
  const page = read("../../public/index.html");
  const scripts = [...page.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)]
    .filter(([, attributes, body]) =>
      !/\bsrc\s*=/.test(attributes) &&
      !/\btype\s*=\s*["'](?:application\/json|importmap)/i.test(attributes) &&
      /\S/.test(body)
    );
  assert.ok(scripts.length > 0);
  scripts.forEach(([, , body], index) => {
    new vm.Script(body, { filename: `public/index.html:inline-${index + 1}` });
  });
});
