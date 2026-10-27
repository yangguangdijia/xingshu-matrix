import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const context = vm.createContext({});
vm.runInContext(fs.readFileSync(new URL("../../public/xs-traffic-topology.js", import.meta.url), "utf8"), context);
const T = context.XSTrafficTopology;

test("traffic topology layers callers before callees and keeps nodes compact", () => {
  const nodes = [
    { key: "db", label: "database" },
    { key: "api", label: "api" },
    { key: "web", label: "web" }
  ];
  const graph = T.layout(nodes, [{ from: "web", to: "api" }, { from: "api", to: "db" }]);
  assert.ok(graph.positions.web.x < graph.positions.api.x);
  assert.ok(graph.positions.api.x < graph.positions.db.x);
  assert.equal(graph.nodeWidth, 224);
  assert.equal(graph.nodeHeight, 84);
  assert.equal(graph.direction, "horizontal");
  assert.equal(graph.height, 176);
  assert.deepEqual(Array.from(graph.edges, ({ from, to }) => `${from}:${to}`), ["web:api", "api:db"]);
});

test("traffic branches align through multiple layers without node collisions", () => {
  const nodes = [
    { key: "cache", label: "cache" },
    { key: "api-b", label: "api-b" },
    { key: "worker", label: "worker" },
    { key: "web-b", label: "web-b" },
    { key: "db", label: "database" },
    { key: "web-a", label: "web-a" },
    { key: "api-a", label: "api-a" }
  ];
  const edges = [
    { from: "web-a", to: "api-a" },
    { from: "web-b", to: "api-b" },
    { from: "api-a", to: "db" },
    { from: "api-b", to: "db" },
    { from: "api-a", to: "cache" },
    { from: "api-b", to: "worker" }
  ];
  const graph = T.layout(nodes, edges);
  assert.equal(graph.positions["web-a"].x, graph.positions["web-b"].x);
  assert.ok(graph.positions["web-a"].x < graph.positions["api-a"].x);
  assert.ok(graph.positions["api-a"].x < graph.positions.db.x);
  assert.equal(graph.positions.db.x, graph.positions.cache.x);
  for (const edge of graph.edges) assert.ok(graph.positions[edge.from].x < graph.positions[edge.to].x);

  const ids = Object.keys(graph.positions);
  for (let i = 0; i < ids.length; i++) {
    for (let j = i + 1; j < ids.length; j++) {
      const a = graph.positions[ids[i]], b = graph.positions[ids[j]];
      assert.ok(Math.abs(a.x - b.x) >= graph.nodeWidth || Math.abs(a.y - b.y) >= graph.nodeHeight,
        `${ids[i]} and ${ids[j]} overlap`);
    }
  }
});

test("traffic topology deduplicates nodes and edges and ignores unknown endpoints", () => {
  const graph = T.layout(
    [{ key: "a", label: "a" }, { key: "a", label: "duplicate" }, { key: "b", label: "b" }],
    [{ from: "a", to: "b" }, { from: "a", to: "b" }, { from: "missing", to: "b" }, { from: "a", to: "a" }]
  );
  assert.equal(Object.keys(graph.positions).length, 2);
  assert.deepEqual(Array.from(graph.edges, ({ from, to }) => `${from}:${to}`), ["a:b"]);
});

test("traffic topology keeps a cycle grouped ahead of its downstream services", () => {
  const graph = T.layout(
    [{ key: "d", label: "downstream" }, { key: "b", label: "b" }, { key: "a", label: "a" }, { key: "c", label: "c" }],
    [{ from: "a", to: "b" }, { from: "b", to: "c" }, { from: "c", to: "a" }, { from: "c", to: "d" }]
  );
  assert.equal(graph.positions.a.x, graph.positions.b.x);
  assert.equal(graph.positions.b.x, graph.positions.c.x);
  assert.ok(graph.positions.c.x < graph.positions.d.x);
  for (const position of Object.values(graph.positions)) {
    assert.ok(Number.isFinite(position.x));
    assert.ok(Number.isFinite(position.y));
  }
  assert.ok(graph.width < 2000);
});

test("traffic topology is stable when backend node and edge order changes", () => {
  const nodes = [{ key: "web" }, { key: "api-b" }, { key: "api-a" }, { key: "db" }, { key: "cache" }];
  const edges = [
    { from: "web", to: "api-b" },
    { from: "web", to: "api-a" },
    { from: "api-a", to: "db" },
    { from: "api-b", to: "db" },
    { from: "api-a", to: "cache" }
  ];
  const first = T.layout(nodes, edges);
  const second = T.layout(nodes.slice().reverse(), edges.slice().reverse());
  assert.deepEqual(JSON.parse(JSON.stringify(second.positions)), JSON.parse(JSON.stringify(first.positions)));
});

test("mobile topology stacks dependency layers vertically inside a phone-width viewport", () => {
  const graph = T.layout(
    [{ key: "web" }, { key: "api" }, { key: "orders" }, { key: "db" }],
    [{ from: "web", to: "api" }, { from: "api", to: "orders" }, { from: "orders", to: "db" }],
    { direction: "vertical" }
  );
  assert.equal(graph.width, 360);
  assert.equal(graph.nodeWidth, 304);
  assert.equal(graph.nodeHeight, 88);
  assert.equal(graph.direction, "vertical");
  assert.ok(graph.positions.web.y < graph.positions.api.y);
  assert.ok(graph.positions.api.y < graph.positions.orders.y);
  assert.ok(graph.positions.orders.y < graph.positions.db.y);
  for (const p of Object.values(graph.positions)) {
    assert.ok(p.x - graph.nodeWidth / 2 >= 0);
    assert.ok(p.x + graph.nodeWidth / 2 <= graph.width);
  }
  assert.match(T.edgePath(graph.positions.web, graph.positions.api, graph.nodeWidth, graph.nodeHeight), /^M .* C/);
});

test("traffic nodes retain enough mobile clearance for compact labels", () => {
  const graph = T.layout(
    [{ key: "api" }, { key: "orders" }, { key: "worker" }],
    [{ from: "api", to: "orders" }, { from: "api", to: "worker" }],
    { direction: "vertical" }
  );
  assert.equal(graph.width, 360);
  assert.ok(Object.values(graph.positions).every((p) => p.x - graph.nodeWidth / 2 >= 0 && p.x + graph.nodeWidth / 2 <= graph.width));
  assert.ok(graph.height >= graph.nodeHeight + 2 * 46);
});

test("traffic edge paths attach to node boundaries in either direction", () => {
  const right = T.edgePath({ x: 100, y: 80 }, { x: 320, y: 160 }, 124, 44);
  assert.match(right, /^M 162 80 C/);
  assert.match(right, / 258 160$/);
  const left = T.edgePath({ x: 320, y: 160 }, { x: 100, y: 80 }, 124, 44);
  assert.match(left, /^M 258 160 C/);
  assert.match(left, / 162 80$/);
  const vertical = T.edgePath({ x: 100, y: 80 }, { x: 100, y: 200 }, 124, 44);
  assert.match(vertical, /^M 100 102 C/);
  assert.match(vertical, /M 100 102 C 100 130, 100 150, 100 178$/);
  assert.doesNotMatch(vertical, /C 128 102/);
  const diagonal = T.edgePath({ x: 100, y: 80 }, { x: 160, y: 220 }, 116, 36);
  assert.match(diagonal, /^M 100 98 C 109\.6 /);
  assert.match(diagonal, / 150\.4 /);
});

test("traffic edge labels sit beside the path rather than covering its direction", () => {
  const horizontal = T.edgeLabelPoint({ x: 100, y: 80 }, { x: 320, y: 160 }, 124, 44, 760);
  assert.ok(Math.abs(horizontal.y - 120) >= 34);
  const vertical = T.edgeLabelPoint({ x: 100, y: 80 }, { x: 100, y: 200 }, 124, 44, 360);
  assert.equal(vertical.x, 174);
  assert.equal(vertical.y, 140);
});
