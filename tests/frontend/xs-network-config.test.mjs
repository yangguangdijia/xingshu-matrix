import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-network-config.js", import.meta.url), "utf8");

async function renderExpose({ resources = [] } = {}) {
  const calls = [];
  let html = "";
  const context = vm.createContext({
    window: {},
    pageRenderRev: 11,
    current: () => "traffic",
    XS: {
      get: async (path) => {
        calls.push(path);
        if (path.endsWith("/configuration?view=expose")) return { resources };
        throw new Error("unexpected request: " + path);
      }
    },
    tbl: (headers, rows) => "<table>" + JSON.stringify({ headers, rows }) + "</table>",
    document: {}
  });
  vm.runInContext(source, context);
  const el = {
    querySelector: () => ({}),
    querySelectorAll: () => []
  };
  const page = { rev: 11, draw: (_ctx, body) => { html = body; } };
  await context.window.XSNetworkConfig.render(el, { k: "expose" }, page, {
    cluster: { id: 7, name: "cluster-a", env: "prod" }
  });
  return { calls, html };
}

test("external exposure uses the aggregate view and only lists Ingress and APISIX entries", async () => {
  const { calls, html } = await renderExpose({
    resources: [
      { kind: "Ingress", api_version: "networking.k8s.io/v1", available: true, items: [{ kind: "Ingress", metadata: { namespace: "apps", name: "web" }, spec: { rules: [{ host: "web.example", http: { paths: [{ path: "/", backend: { service: { name: "web", port: { number: 80 } } } }] } }] } }] },
      { kind: "ApisixRoute", api_version: "apisix.apache.org/v2", available: true, items: [{ kind: "ApisixRoute", metadata: { namespace: "apps", name: "api" }, spec: { http: [{ match: { hosts: ["api.example"], paths: ["/*"] }, backends: [{ serviceName: "api", servicePort: 8080 }] }] } }] },
      { kind: "HTTPRoute", api_version: "gateway.networking.k8s.io/v1", available: true, items: [{ kind: "HTTPRoute", metadata: { namespace: "apps", name: "ignored-gateway" } }] },
      { kind: "VirtualService", api_version: "networking.istio.io/v1", available: true, items: [{ kind: "VirtualService", metadata: { namespace: "apps", name: "ignored-istio" } }] }
    ]
  });

  assert.ok(calls.includes("/clusters/7/network/configuration?view=expose"));
  assert.ok(!calls.includes("/clusters/7/network/configuration?view=ingress"));
  assert.doesNotMatch(html, /\/services|外部 Service|apps\/edge|203\.0\.113\.8/);
  assert.match(html, /APISIX 是 API 网关及 Ingress Controller/);
  assert.match(html, /NetworkPolicy 定义 Pod 的 L3\/L4 入站、出站规则/);
  assert.match(html, /实际拦截效果取决于集群 CNI/);
  assert.match(html, /APISIX 路由 CRD（ApisixRoute）/);
  assert.match(html, /apps/);
  assert.match(html, /web/);
  assert.match(html, /api/);
  assert.doesNotMatch(html, /HTTPRoute|VirtualService|ignored-gateway|ignored-istio/);
});

test("unavailable APISIX resource details remain visible without claiming Gateway API support", async () => {
  const { html } = await renderExpose({
    resources: [
      { kind: "Ingress", api_version: "networking.k8s.io/v1", available: true, items: [] },
      {
        kind: "ApisixRoute",
        api_version: "apisix.apache.org/v2",
        available: false,
        state: "unavailable",
        error: "API 请求超时；控制器状态未知",
        items: []
      },
      { kind: "HTTPRoute", api_version: "gateway.networking.k8s.io/v1", available: false, state: "not_installed", items: [] }
    ]
  });

  assert.match(html, /API 请求超时；控制器状态未知/);
  assert.match(html, /APISIX 路由 CRD（ApisixRoute）/);
  assert.doesNotMatch(html, /HTTPRoute/);
});
