import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (path) => fs.readFileSync(new URL(path, import.meta.url), "utf8");

test("certificate page is a sidebar route with a real TLS Secret import", () => {
  const page = read("../../public/index.html");
  const certificates = read("../../public/xs-certificates.js");

  assert.match(page, /\{ k: "certificates", t: "证书"/);
  assert.match(page, /pg\.k === "certificates"[\s\S]{0,120}XSCertificates\.render/);
  assert.match(page, /href="xs-certificates\.css"/);
  assert.match(page, /src="xs-certificates\.js"/);
  assert.match(certificates, /\/certificates\/import/);
  assert.match(certificates, /data-cert-file="certificate"/);
  assert.match(certificates, /data-cert-file="private_key"/);
  assert.match(certificates, /data-cert-submit/);
  assert.doesNotMatch(certificates, /data-cert-source|cert-manager[^<]*未接入/);
});

function clusterRoutes() {
  const h = harness("xs-component-center.js", {
    api: async (path) => {
      if (path.endsWith("/resolve")) throw { status: 404, message: "not bound" };
      if (path.endsWith("/integration-discovery")) return { result: { items: [{ type: "loki", name: "loki", namespace: "obs", url_candidates: ["http://loki.obs.svc:3100"] }] } };
      return { items: [], truncated: false };
    }
  });
  vm.runInNewContext(read("../../public/xs-cluster-integrations.js"), { window: h.window, AbortController, URL });
  const calls = [], destroyed = [], cluster = { id: 7, name: "cluster-a", env: "test" };
  for (const name of ["XSClusterIntegrations", "XSComponentCenter"]) {
    const module = h.window[name], render = module.render, destroy = module.destroy;
    module.render = (el, ctx) => { calls.push({ name, el, ctx }); return render(el, ctx); };
    module.destroy = (el) => { destroyed.push(name); destroy(el); };
  }
  Object.assign(h.window.XS, { mode: "live", get: async (path) => {
    assert.equal(path, "/clusters"); return { items: [cluster] };
  } });
  Object.assign(h.window, {
    XSClusterExtensions: { register: () => true },
    XSSelectedCluster: () => cluster,
    XSLiveOpsClusterConfig: () => assert.fail("legacy integration renderer must not dispatch")
  });
  const body = new Element("body"), head = new Element("head");
  h.el.setAttribute("id", "view"); body.appendChild(h.el);
  const document = {
    body, head, readyState: "complete",
    getElementById: (id) => body.querySelector(`[id="${id}"]`) || head.querySelector(`[id="${id}"]`),
    createElement: (tag) => new Element(tag)
  };
  const routes = { cluster: { render: () => assert.fail("demo renderer must not dispatch") } };
  const location = { hash: "#/cluster/integrations/7" };
  vm.runInNewContext(read("../../public/xs-live-core.js"), { window: h.window, document, routes, location, URL, console, setTimeout: () => assert.fail("unexpected deferred core boot") });
  return { ...h, routes, location, callsToModules: calls, destroyed };
}

test("live cluster dispatcher mounts real integration/component pages with sidebar-only navigation", async () => {
  const h = clusterRoutes();
  for (const [sub, module, selector] of [
    ["integrations", "XSClusterIntegrations", "[data-xi-list]"],
    ["components", "XSComponentCenter", "[data-xcc-catalog]"],
    ["integrations", "XSClusterIntegrations", "[data-xi-list]"]
  ]) {
    h.location.hash = `#/cluster/${sub}/7`;
    h.routes.cluster.render(h.el); await tick(); await tick();
    const call = h.callsToModules.at(-1);
    assert.equal(call.name, module);
    assert.equal(call.ctx.cluster_id, 7);
    assert.equal(call.ctx.route.sub, sub);
    assert.equal(call.el, h.el.querySelector('[id="liveClusterBody"]'));
    assert.ok(call.el.querySelector(selector), `${sub} must render the actual page`);
    assert.equal(h.el.querySelector('[role="tablist"]'), null);
    assert.equal(h.el.querySelector('[role="tab"]'), null);
    assert.equal(h.el.querySelector(".x-tabs"), null);
    assert.equal(h.el.querySelector("nav"), null);
  }
  assert.deepEqual(h.destroyed, ["XSClusterIntegrations", "XSComponentCenter"]);
  assert.equal(h.listeners.size, 1, "only the mounted page retains a context subscription");
  assert.ok(h.calls.some((c) => c.path === "/clusters/7/integration-discovery"));
  const mounted = h.callsToModules.at(-1).el;
  for (const listener of mounted.listeners.click) await listener({ target: h.el.querySelectorAll("[data-xi-edit]")[1] });
  const form = h.el.querySelector("[data-xi-form]");
  assert.ok(form.elements.url, "manual address remains editable");
  assert.match(h.el.querySelector("[data-xi-detail]").innerHTML, /http:\/\/loki\.obs\.svc:3100/);
  h.window.XSClusterIntegrations.destroy(mounted);
  assert.equal(h.listeners.size, 0);
});

test("live cluster dispatcher fails closed when the owning page module is unavailable", async () => {
  const h = clusterRoutes(); delete h.window.XSClusterIntegrations;
  h.routes.cluster.render(h.el); await tick(); await tick();
  assert.equal(h.callsToModules.length, 0);
  const box = h.el.querySelector('[id="liveClusterBody"]');
  assert.ok(box.querySelector('[role="alert"]'));
  assert.ok(box.querySelector("[data-live-retry]"));
  assert.equal(box.querySelector("[data-xi-list]"), null);
});
