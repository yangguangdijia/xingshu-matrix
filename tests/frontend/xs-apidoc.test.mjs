import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-apidoc.js", import.meta.url), "utf8");

const spec = {
  openapi: "3.0.3",
  info: { title: "星枢", version: "1.0" },
  paths: {
    "/clusters": {
      get: { tags: ["cluster"], summary: "集群列表", "x-xs-access": { public: false, permissions: ["cluster:read"], any_of: [], env_scoped: false } },
      post: { tags: ["cluster"], summary: "创建集群", "x-xs-access": { public: false, permissions: ["cluster:write"], any_of: [], env_scoped: true } }
    },
    "/auth/login": {
      post: { tags: ["auth"], summary: "登录", "x-xs-access": { public: true, permissions: [], any_of: [], env_scoped: false } }
    },
    "/audit/logs": {
      get: { tags: ["audit"], summary: "<img src=x onerror=alert(1)>", "x-xs-access": { public: false, permissions: ["audit:<read>"], any_of: ["a\"b"], env_scoped: false } }
    }
  }
};

function makeEl() {
  const nodes = {};
  const node = (key) => (nodes[key] ||= { value: "", innerHTML: "", listeners: {}, addEventListener(t, fn) { this.listeners[t] = fn; } });
  return {
    innerHTML: "",
    querySelector(sel) {
      if (sel === "[data-apidoc-search]") return node("search");
      if (sel === "[data-apidoc-list]") return node("list");
      if (sel === "[data-apidoc-download]") return node("download");
      return null;
    },
    nodes
  };
}

function load(api) {
  const window = { XS: { api } };
  vm.runInNewContext(source, { window, URL, setTimeout, Blob });
  return window;
}

test("render fetches envelope spec and groups operations by module with Chinese names", async () => {
  const calls = [];
  const w = load(async (path) => { calls.push(path); return spec; });
  const el = makeEl();
  await w.XSApiDoc.render(el);
  assert.equal(JSON.stringify(calls), JSON.stringify(["/openapi.json?envelope=1"]));
  assert.match(el.innerHTML, /<h3>集群<small>2 个接口<\/small><\/h3>/);
  assert.match(el.innerHTML, /<h3>审计<small>1 个接口<\/small><\/h3>/);
  assert.match(el.innerHTML, /<h3>auth<small>1 个接口<\/small><\/h3>/);
  assert.match(el.innerHTML, /下载 openapi\.json/);
  assert.match(el.innerHTML, /公开/);
  assert.match(el.innerHTML, /需登录/);
  assert.match(el.innerHTML, /<code>cluster:write<\/code>/);

  const groups = w.XSApiDoc._internal.group(w.XSApiDoc._internal.operations(spec));
  assert.equal(JSON.stringify(groups.map((g) => [g.key, g.name, g.ops.length])), JSON.stringify([["audit", "审计", 1], ["auth", "auth", 1], ["cluster", "集群", 2]]));
});

test("search filters by path and summary", async () => {
  const w = load(async () => spec);
  const el = makeEl();
  await w.XSApiDoc.render(el);
  const search = el.nodes.search, list = el.nodes.list;
  search.value = "/auth";
  search.listeners.input();
  assert.match(list.innerHTML, /\/auth\/login/);
  assert.doesNotMatch(list.innerHTML, /\/clusters/);
  search.value = "创建";
  search.listeners.input();
  assert.match(list.innerHTML, /\/clusters/);
  assert.match(list.innerHTML, /<h3>集群<small>1 个接口/);
  search.value = "不存在的接口";
  search.listeners.input();
  assert.match(list.innerHTML, /没有匹配的接口/);
});

test("all interpolated values are escaped", async () => {
  const w = load(async () => spec);
  const el = makeEl();
  await w.XSApiDoc.render(el);
  assert.doesNotMatch(el.innerHTML, /<img/);
  assert.match(el.innerHTML, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(el.innerHTML, /audit:&lt;read&gt;/);
  assert.match(el.innerHTML, /a&quot;b/);

  const w2 = load(async () => { throw new Error("<b>boom</b>"); });
  const el2 = makeEl();
  await w2.XSApiDoc.render(el2);
  assert.match(el2.innerHTML, /加载失败：&lt;b&gt;boom&lt;\/b&gt;/);
});
