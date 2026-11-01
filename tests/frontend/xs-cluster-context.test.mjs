import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-cluster-context.js", import.meta.url), "utf8");

function createContext(saved) {
  const data = new Map(saved ? [["xs_cluster_context", JSON.stringify(saved)]] : []);
  const storage = {
    getItem: (key) => data.get(key) ?? null,
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
  };
  const events = [];
  const window = {
    localStorage: storage,
    dispatchEvent: (event) => events.push(event),
    CustomEvent: class CustomEvent { constructor(type, options) { this.type = type; this.detail = options.detail; } },
    console,
  };
  vm.runInNewContext(source, { window, AbortController, console }, { filename: "xs-cluster-context.js" });
  return { ctx: window.XSClusterContext, storage, events };
}

const clusterA = { id: 42, name: "alpha", env: "prod" };
const clusterB = { id: 43, name: "beta", env: "prod" };
const select = (cluster) => ({ kind: "cluster", cluster_id: cluster.id, name: cluster.name, env: cluster.env });

test("saved choice remains pending until validated against authorized clusters and environment", () => {
  const { ctx, storage } = createContext({ ...select(clusterA), version: 6 });
  assert.equal(ctx.get().kind, "unselected");
  assert.equal(ctx.epoch(), 6);
  assert.equal(ctx.restoreCandidate().cluster_id, "42");
  assert.equal(ctx.validate(ctx.restoreCandidate(), [clusterB], "prod"), false);
  assert.throws(() => ctx.activate(ctx.restoreCandidate(), { clusters: [clusterB], env: "prod" }));
  assert.throws(() => ctx.activate(ctx.restoreCandidate(), { clusters: [clusterA], env: "dev" }));
  assert.equal(ctx.get().kind, "unselected");
  const current = ctx.activate(ctx.restoreCandidate(), { clusters: [clusterA], env: "prod" });
  assert.equal(current.cluster_id, "42");
  assert.equal(current.version, 7);
  assert.equal(JSON.parse(storage.getItem("xs_cluster_context")).version, 7);
});

test("invalid or removed saved cluster never falls back to first available cluster", () => {
  const { ctx, storage } = createContext({ ...select(clusterB), version: 1 });
  assert.equal(ctx.validate(ctx.restoreCandidate(), [clusterA], "prod"), false);
  assert.throws(() => ctx.activate(ctx.restoreCandidate(), { clusters: [clusterA], env: "prod" }));
  assert.equal(ctx.get().cluster_id, "");
  ctx.clear();
  assert.equal(storage.getItem("xs_cluster_context"), null);
});

test("explicit all state requires an opt-in to restore and cannot enter cluster cache", () => {
  const { ctx } = createContext({ kind: "all", env: "prod", version: 2 });
  assert.throws(() => ctx.activate(ctx.restoreCandidate(), { clusters: [clusterA], env: "prod" }));
  ctx.activate(ctx.restoreCandidate(), { clusters: [clusterA], env: "prod", allowAll: true });
  assert.equal(ctx.get().kind, "all");
  assert.throws(() => ctx.cache.get("workloads"), /未选择集群/);
  ctx.cache.set("summary", 3, "aggregate");
  assert.equal(ctx.cache.get("summary", "aggregate"), 3);
});

test("cluster change increments epoch synchronously, emits event, and isolates cache", () => {
  const { ctx, events } = createContext();
  const observed = [];
  const unsubscribe = ctx.subscribe((event) => observed.push(event));
  ctx.cache.set("settings", 1, "platform");
  ctx.registerPageScope("settings", "platform");
  ctx.registerPageScope("metrics", "cluster");
  assert.equal(ctx.pageScope("settings"), "platform");
  assert.equal(ctx.pageScope("metrics"), "cluster");
  assert.equal(ctx.pageScope("unknown"), null);
  const first = ctx.set(select(clusterA));
  ctx.cache.set("pods", "alpha-only");
  const firstKey = ctx.cache.key("pods");
  const second = ctx.set(select(clusterB));
  assert.equal(second.version, first.version + 1);
  assert.equal(second.state, "cluster");
  assert.equal(ctx.epoch(), second.version);
  assert.equal(ctx.isCurrent(first), false);
  assert.equal(ctx.isCurrent(second), true);
  assert.throws(() => ctx.assertCurrent(first), { code: "XS_CONTEXT_CHANGED" });
  assert.equal(ctx.cache.get("pods"), undefined);
  assert.notEqual(ctx.cache.key("pods"), firstKey);
  assert.equal(ctx.cache.get("settings", "platform"), 1);
  assert.equal(observed.length, 2);
  assert.equal(events[1].detail.current.cluster_id, "43");
  assert.equal(events[1].type, "xs:cluster-context");
  unsubscribe();
  ctx.invalidate("refresh");
  assert.equal(observed.length, 2);
  assert.equal(ctx.cache.get("settings", "platform"), 1);
});

test("page scopes cancel page requests; context change cancels all scoped requests", () => {
  const { ctx } = createContext();
  ctx.registerPageScope("metrics", "cluster");
  ctx.registerPageScope("logs", "cluster");
  ctx.enterPage("metrics");
  const metrics = ctx.createRequestScope();
  ctx.enterPage("logs");
  assert.equal(metrics.signal.aborted, true);
  assert.equal(metrics.reason(), "page-changed");
  assert.throws(() => metrics.assertCurrent(), { code: "XS_REQUEST_CANCELLED" });
  const logs = ctx.createRequestScope();
  ctx.set(select(clusterA));
  assert.equal(logs.signal.aborted, true);
  assert.equal(logs.reason(), "context-changed");
  assert.throws(() => logs.assertCurrent(), { code: "XS_REQUEST_CANCELLED" });
  assert.throws(() => ctx.enterPage("unknown"));
});

test("changing environment removes its cache, retains platform cache, and clears selection", () => {
  const { ctx, storage } = createContext();
  ctx.set(select(clusterA));
  ctx.cache.set("environment-page", "old", "environment");
  ctx.cache.set("nav", "persistent", "platform");
  ctx.clear();
  assert.equal(ctx.get().kind, "unselected");
  assert.equal(storage.getItem("xs_cluster_context"), null);
  assert.equal(ctx.cache.get("nav", "platform"), "persistent");
  assert.throws(() => ctx.cache.get("environment-page", "environment"), /未选择环境/);
});

test("platform request scope stays valid when the selected cluster changes", () => {
  const { ctx } = createContext();
  ctx.registerPageScope("settings", "platform");
  ctx.enterPage("settings");
  const pending = ctx.createRequestScope();
  ctx.set(select(clusterA));
  ctx.set(select(clusterB));
  assert.equal(pending.signal.aborted, false);
  assert.equal(pending.isCurrent(), true);
  assert.equal(pending.assertCurrent(), true);
  pending.release();
});
