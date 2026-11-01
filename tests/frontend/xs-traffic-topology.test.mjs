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
  assert.equal(graph.positions.api.x - graph.positions.web.x, 392);
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
  assert.equal(Math.abs(graph.positions["web-a"].y - graph.positions["web-b"].y), 168);
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
  assert.equal(graph.positions.worker.y - graph.positions.orders.y, 208);
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

test("explicit primary edges keep an entry-to-service-to-pod chain stable beside auxiliary lanes", () => {
  const nodes = [
    { key: "entry", label: "ingress", lane: "entry" },
    { key: "service", label: "orders", lane: "service" },
    { key: "pod", label: "orders-0", lane: "pod" },
    { key: "policy-a", label: "worker policy", lane: "policy" },
    { key: "policy-b", label: "cache policy", lane: "policy" },
    { key: "unlinked", label: "unlinked", lane: "auxiliary" }
  ];
  const primaryEdges = [{ from: "entry", to: "service" }, { from: "service", to: "pod" }];
  const base = T.layout(nodes.slice(0, 3), primaryEdges, { primaryEdges });
  const graph = T.layout(nodes, [
    ...primaryEdges,
    { from: "policy-a", to: "service", kind: "policy" },
    { from: "service", to: "policy-b", kind: "declared" }
  ], { primaryEdges });

  assert.ok(graph.positions.entry.x < graph.positions.service.x);
  assert.ok(graph.positions.service.x < graph.positions.pod.x);
  assert.deepEqual(JSON.parse(JSON.stringify(graph.positions.entry)), JSON.parse(JSON.stringify(base.positions.entry)));
  assert.deepEqual(JSON.parse(JSON.stringify(graph.positions.service)), JSON.parse(JSON.stringify(base.positions.service)));
  assert.deepEqual(JSON.parse(JSON.stringify(graph.positions.pod)), JSON.parse(JSON.stringify(base.positions.pod)));
  assert.ok(graph.auxiliaryKeys.includes("policy-a"));
  assert.ok(graph.auxiliaryKeys.includes("unlinked"));
  assert.ok(graph.positions["policy-a"].y > graph.positions.pod.y);
  assert.ok(graph.height > graph.positions["policy-a"].y);
  assert.equal(graph.ranks["policy-a"], -1);
});

test("multiple entries, services, endpoints and cycles remain deterministic without rank leakage", () => {
  const nodes = [
    { key: "entry-b" }, { key: "entry-a" }, { key: "svc-b" }, { key: "svc-a" },
    { key: "pod-b" }, { key: "pod-a" }, { key: "policy" }
  ];
  const primaryEdges = [
    { from: "entry-a", to: "svc-a" }, { from: "entry-b", to: "svc-b" },
    { from: "svc-a", to: "pod-a" }, { from: "svc-b", to: "pod-b" },
    { from: "svc-a", to: "svc-b" }, { from: "svc-b", to: "svc-a" }
  ];
  const edges = primaryEdges.concat([{ from: "policy", to: "svc-a", kind: "policy" }]);
  const first = T.layout(nodes, edges, { primaryEdges });
  const second = T.layout(nodes.slice().reverse(), edges.slice().reverse(), { primaryEdges: primaryEdges.slice().reverse() });
  assert.deepEqual(JSON.parse(JSON.stringify(first.positions)), JSON.parse(JSON.stringify(second.positions)));
  assert.equal(first.positions["svc-a"].x, first.positions["svc-b"].x);
  assert.ok(first.positions["entry-a"].x < first.positions["svc-a"].x);
  assert.ok(first.positions["svc-a"].x < first.positions["pod-a"].x);
  assert.equal(first.ranks.policy, -1);
});

test("edge composition keeps measured traces solid and NetworkPolicy declarations secondary", () => {
  const composed = T.composeEdges({
    nodeKeys: ["entry", "service", "pod", "trace-service", "policy-service"],
    entryEdges: [{ from: "entry", to: "service" }],
    serviceEndpoints: [{ from: "service", to: "pod", source: "endpointslice" }],
    traceEdges: [{ from: "service", to: "trace-service", count: 3 }],
    policyEdges: [{ from: "service", to: "policy-service" }]
  });
  assert.deepEqual(Array.from(composed.primaryEdges, edge => `${edge.from}:${edge.to}`), ["entry:service", "service:pod"]);
  assert.equal(T.edgeSemantics(composed.traceEdges[0]).solid, true);
  assert.equal(T.edgeSemantics(composed.traceEdges[0]).dashed, false);
  assert.equal(T.edgeSemantics(composed.policyEdges[0]).solid, false);
  assert.equal(T.edgeSemantics(composed.policyEdges[0]).dashed, true);
  assert.equal(T.edgeSemantics(composed.policyEdges[0]).secondary, true);
});

test("trace and policy overlays with identical endpoints remain distinct", () => {
  const result = T.composeEdges({
    nodeKeys: ["entry", "service"],
    entryEdges: [{ from: "entry", to: "service" }],
    traceEdges: [{ from: "entry", to: "service", id: "observed" }],
    policyEdges: [{ from: "entry", to: "service", id: "declared" }]
  });
  assert.equal(result.edges.length, 3);
  assert.deepEqual(Array.from(T.parallelOffsets(result.edges)), [0, -32, 32]);
});

test("parallel offsets separate same-endpoint edges by semantic kind and stay stable across input order", () => {
  const edges = [
    { id: "trace-b", from: "api", to: "db", kind: "trace" },
    { id: "primary", from: "api", to: "db", kind: "primary" },
    { id: "trace-a", from: "api", to: "db", kind: "trace" },
    { id: "policy", from: "api", to: "db", kind: "policy" }
  ];
  const offsets = T.parallelOffsets(edges);
  assert.notEqual(offsets[0], offsets[2]);
  assert.deepEqual(
    Object.fromEntries(edges.map((edge, index) => [edge.id, offsets[index]])),
    Object.fromEntries(edges.slice().reverse().map((edge, index) => [
      edge.id,
      T.parallelOffsets(edges.slice().reverse())[index]
    ]))
  );
  assert.deepEqual(Array.from(offsets), [-48, -16, 16, 48]);
  assert.equal(T.edgeLane({}, 0, 3), 0);
  assert.equal(T.edgeLane({}, 2, 3), 32);
});

test("edge ports distribute along node boundaries and clamp indexes within card height", () => {
  const from = { x: 100, y: 100 }, to = { x: 420, y: 100 };
  const paths = [0, 1, 2].map((portIndex) =>
    T.edgePath(from, to, 224, 84, { portIndex, portCount: 3 }));
  const startYs = paths.map((path) => Number(path.match(/^M [\d.-]+ ([\d.-]+)/)[1]));
  assert.deepEqual(startYs, [66, 100, 134]);
  assert.ok(startYs.every((y) => y >= from.y - 42 && y <= from.y + 42));
  const clamped = T.edgePath(from, to, 224, 84, { portIndex: 99, portCount: 3 });
  assert.match(clamped, /^M 212 134 C/);
  const targetPorts = [0, 1, 2].map((toPortIndex) =>
    T.edgePath(from, to, 224, 84, {
      fromPortIndex: 0, fromPortCount: 1, toPortIndex, toPortCount: 3
    }));
  assert.deepEqual(targetPorts.map((path) => Number(path.match(/, 308 ([\d.-]+)$/)[1])), [66, 100, 134]);
  const wideOffset = T.edgePath(from, to, 224, 84, { offset: 96 });
  assert.match(wideOffset, /^M 212 134 C/);
  assert.match(wideOffset, / 308 134$/);
  assert.notEqual(wideOffset, T.edgePath(from, to, 224, 84, { offset: 64 }));
});

test("offset edge labels keep clearance in dense parallel lanes", () => {
  const from = { x: 100, y: 100 }, to = { x: 420, y: 100 };
  const offsets = Array.from(T.parallelOffsets([
    { from: "api", to: "db", id: "a" },
    { from: "api", to: "db", id: "b" },
    { from: "api", to: "db", id: "c" }
  ]));
  const labels = offsets.map((offset) =>
    T.edgeLabelPoint(from, to, 224, 84, 1024, { offset, labelHeight: 24 }));
  assert.deepEqual(labels.map(({ x }) => x), [260, 260, 260]);
  assert.deepEqual(labels.map(({ y }) => y), [70, 38, 102]);
  assert.ok(labels.every((point, index) => Math.abs(point.y - (100 + offsets[index])) >= 30));
  const sortedLabels = labels.slice().sort((a, b) => a.y - b.y);
  assert.ok(sortedLabels.slice(1).every((point, index) => point.y - sortedLabels[index].y >= 24 + 8));

  const vertical = T.edgeLabelPoint(
    { x: 180, y: 100 }, { x: 180, y: 400 }, 224, 84, 360,
    { offset: 12, labelHeight: 28, labelClearance: 20 }
  );
  assert.equal(vertical.x, 68);
});

test("multiple entries to one service and cycle branches stay deterministic and clear", () => {
  const nodes = ["entry-c", "entry-a", "entry-b", "service", "worker", "cache"]
    .map((key) => ({ key }));
  const edges = [
    ...["entry-c", "entry-a", "entry-b"].map((from) => ({ from, to: "service" })),
    { from: "service", to: "worker" },
    { from: "worker", to: "cache" },
    { from: "cache", to: "worker" }
  ];
  const first = T.layout(nodes, edges);
  const second = T.layout(nodes.slice().reverse(), edges.slice().reverse());
  assert.deepEqual(JSON.parse(JSON.stringify(first.positions)), JSON.parse(JSON.stringify(second.positions)));
  assert.equal(first.positions["entry-c"].x, first.positions["entry-a"].x);
  assert.ok(first.positions["entry-a"].x < first.positions.service.x);
  assert.equal(first.positions.worker.x, first.positions.cache.x);
  const positions = Object.values(first.positions);
  for (let i = 0; i < positions.length; i++) {
    for (let j = i + 1; j < positions.length; j++) {
      assert.ok(Math.abs(positions[i].x - positions[j].x) >= first.nodeWidth ||
        Math.abs(positions[i].y - positions[j].y) >= first.nodeHeight);
    }
  }
});

test("legacy geometry calls preserve their exact path and label behavior", () => {
  const args = [{ x: 100, y: 80 }, { x: 320, y: 160 }, 124, 44];
  assert.equal(
    T.edgePath(...args),
    "M 162 80 C 202.32 80, 217.68 160, 258 160"
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(T.edgeLabelPoint(...args, 760))),
    { x: 210, y: 86 }
  );
  assert.equal(T.edgePath(...args, undefined), T.edgePath(...args));
});
