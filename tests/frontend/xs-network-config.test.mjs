import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-network-config.js", import.meta.url), "utf8");

function parseHTML(html) {
  const root = new FakeElement("root");
  const stack = [root];
  const tokens = String(html || "").match(/<!--[\s\S]*?-->|<[^>]+>|[^<]+/g) || [];
  for (const token of tokens) {
    if (token.startsWith("<!--")) continue;
    if (token.startsWith("</")) { if (stack.length > 1) stack.pop(); continue; }
    if (!token.startsWith("<")) { stack.at(-1).textContent += token; continue; }
    const match = token.match(/^<([\w-]+)([\s\S]*?)(\/?)>$/);
    if (!match) continue;
    const element = new FakeElement(match[1]);
    const attrs = match[2].match(/([:\w-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g) || [];
    for (const attr of attrs) {
      const item = attr.match(/^([:\w-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/);
      if (!item) continue;
      element.setAttribute(item[1], item[2] ?? item[3] ?? item[4] ?? "");
    }
    stack.at(-1).appendChild(element);
    if (!/^(input|br|hr|img|meta|link)$/.test(element.tagName) && match[3] !== "/") stack.push(element);
  }
  return root;
}

function decodeEntities(value) {
  return String(value)
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&amp;/g, "&");
}

class FakeElement {
  constructor(tagName) {
    this.tagName = tagName.toLowerCase();
    this.children = [];
    this.parentNode = null;
    this.attributes = {};
    this.dataset = {};
    this.style = {};
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.value = "";
    this.textContent = "";
    this.isConnected = true;
  }
  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);
    if (this.tagName === "select" && child.tagName === "option" && child.getAttribute("selected") != null) {
      this.value = child.value || child.textContent;
    }
    return child;
  }
  setAttribute(name, value) {
    const key = String(name);
    const text = decodeEntities(value);
    this.attributes[key] = text;
    if (key === "id") this.id = text;
    if (key === "class") this.className = text;
    if (key === "value") this.value = text;
    if (key === "checked") this.checked = true;
    if (key === "disabled") this.disabled = true;
    if (key.startsWith("data-")) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = text;
  }
  getAttribute(name) { return this.attributes[name] ?? null; }
  set innerHTML(value) {
    this.children = [];
    this.textContent = "";
    const parsed = parseHTML(value);
    for (const child of parsed.children) this.appendChild(child);
  }
  get innerHTML() { return this.children.map((child) => child.outerHTML()).join("") + this.textContent; }
  outerHTML() {
    const attrs = Object.entries(this.attributes).map(([key, value]) => ` ${key}="${value}"`).join("");
    const voidTag = /^(input|br|hr|img|meta|link)$/.test(this.tagName);
    return `<${this.tagName}${attrs}>${voidTag ? "" : this.innerHTML + "</" + this.tagName + ">"}`;
  }
  matches(selector) {
    if (selector.startsWith("#")) return this.id === selector.slice(1);
    if (selector.startsWith(".")) return (this.className || "").split(/\s+/).includes(selector.slice(1));
    const attr = selector.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);
    if (attr) return Object.prototype.hasOwnProperty.call(this.attributes, attr[1]) && (attr[2] == null || this.attributes[attr[1]] === attr[2]);
    if (selector === "*") return true;
    return this.tagName === selector.toLowerCase();
  }
  querySelector(selector) {
    const result = this.querySelectorAll(selector)[0] || null;
    return result;
  }
  querySelectorAll(selector) {
    const found = [];
    const visit = (node) => {
      for (const child of node.children) {
        if (child.matches(selector)) found.push(child);
        visit(child);
      }
    };
    visit(this);
    return found;
  }
  get elements() {
    const result = {};
    for (const field of this.querySelectorAll("input").concat(this.querySelectorAll("select"), this.querySelectorAll("textarea"))) {
      const name = field.getAttribute("name");
      if (name) result[name] = field;
    }
    return result;
  }
  reportValidity() {
    for (const field of this.querySelectorAll("input").concat(this.querySelectorAll("select"), this.querySelectorAll("textarea"))) {
      if (field.getAttribute("required") != null && (field.type === "checkbox" ? !field.checked : !String(field.value || "").trim())) return false;
    }
    return true;
  }
  get isConnected() { return this._connected !== false; }
  set isConnected(value) { this._connected = value; }
  focus() { this.focused = true; }
  get type() { return this.getAttribute("type") || "text"; }
}

class FakeDocument extends FakeElement {
  constructor() {
    super("document");
    this.head = new FakeElement("head");
    this.body = new FakeElement("body");
    this.modal = new FakeElement("div");
    this.modal.id = "modal";
    this.mask = new FakeElement("div");
    this.mask.id = "mask";
    this.body.appendChild(this.modal);
    this.body.appendChild(this.mask);
  }
  createElement(tagName) { return new FakeElement(tagName); }
  getElementById(id) { return [this.modal, this.mask, ...this.body.querySelectorAll("*"), ...this.head.querySelectorAll("*")].find((element) => element.id === id) || null; }
  querySelector(selector) { return this.body.querySelector(selector) || this.head.querySelector(selector); }
}

function ingress(name = "web") {
  return {
    apiVersion: "networking.k8s.io/v1",
    kind: "Ingress",
    metadata: { name, namespace: "apps", uid: "uid-1", annotations: { "example.com/owner": "platform" } },
    spec: {
      ingressClassName: "nginx",
      defaultBackend: { service: { name: "fallback", port: { number: 80 } } },
      rules: [{ host: "web.example.com", http: { paths: [{ path: "/shop", pathType: "Prefix", backend: { service: { name: "shop", port: { number: 8080 }, extraBackendField: "keep" } }, extraPathField: "keep" }] } }],
      tls: [{ hosts: ["web.example.com"], secretName: "web-tls" }],
      customPolicy: { mode: "preserve" }
    }
  };
}

function makeHarness({ resources, latestResources = resources, discoveryResources = [], canWrite = true } = {}) {
  const document = new FakeDocument();
  const el = new FakeElement("main");
  const page = { rev: 11, draw: (_ctx, body) => { el.innerHTML = body; } };
  const calls = [];
  const posts = [];
  const context = vm.createContext({
    window: {},
    pageRenderRev: 11,
    current: () => "traffic",
    document,
    closeAll: () => { document.modal.hidden = true; },
    toast: (message) => { context.lastToast = message; },
    dlg: (title, body, foot) => {
      document.modal.innerHTML = `<div id="mtitle">${title}</div><div class="x-wbody">${body}</div><div class="x-dfoot">${foot || ""}</div>`;
      document.modal.hidden = false;
    },
    tbl: (_headers, rows) => `<table>${rows.map((row) => `<tr>${row.map((cell) => `<td>${cell}</td>`).join("")}</tr>`).join("")}</table>`,
    XS: {
      can: (permission) => permission === "network:write" ? canWrite : false,
      get: async (path) => {
        calls.push(path);
        if (path.endsWith("configuration?view=expose")) return { resources };
        if (path.endsWith("configuration?view=ingress")) return { resources: discoveryResources };
        throw new Error(`unexpected GET ${path}`);
      },
      post: async (path, payload) => { posts.push({ path, payload }); return { status: "applied", failed: 0, objects: [{ status: "applied" }] }; }
    }
  });
  vm.runInContext(source, context);
  return { context, document, el, page, calls, posts, cluster: { id: 7, name: "cluster-a", env: "prod" } };
}

async function renderExpose(harness) {
  await harness.context.window.XSNetworkConfig.render(harness.el, { k: "expose" }, harness.page, { cluster: harness.cluster });
  await new Promise((resolve) => setImmediate(resolve));
}

const exposureResources = (object = ingress()) => [
  { kind: "Ingress", api_version: "networking.k8s.io/v1", available: true, items: [object] },
  { kind: "ApisixRoute", api_version: "apisix.apache.org/v2", available: false, state: "not_installed", items: [] },
  { kind: "IngressClass", api_version: "networking.k8s.io/v1", available: true, items: [{ kind: "IngressClass", metadata: { name: "nginx" }, spec: { controller: "k8s.io/ingress-nginx" } }] }
];

test("external exposure removes ordinary footer explanations but keeps compact capability state", async () => {
  const harness = makeHarness({ resources: exposureResources() });
  await renderExpose(harness);
  assert.doesNotMatch(harness.el.innerHTML, /仅显示集群 API 返回|APISIX 是 API 网关|NetworkPolicy 定义 Pod/);
  assert.match(harness.el.innerHTML, /Ingress：可读取/);
  assert.match(harness.el.innerHTML, /ApisixRoute：集群未安装/);
});

test("editing an existing Ingress uses complete live fields and has no manifest input", async () => {
  const object = ingress();
  const harness = makeHarness({ resources: exposureResources(object), discoveryResources: exposureResources(object) });
  await renderExpose(harness);
  const edit = harness.el.querySelector("[data-net-edit]");
  assert.ok(edit);
  edit.onclick();
  await new Promise((resolve) => setImmediate(resolve));
  const form = harness.document.querySelector("#netConfigForm");
  assert.ok(form);
  assert.equal(form.elements.resourceName.value, "web");
  assert.equal(form.elements.namespace.value, "apps");
  assert.equal(form.elements.className.value, "nginx");
  assert.equal(form.querySelector('[data-net-field="host"]').value, "web.example.com");
  assert.equal(form.querySelector('[data-net-field="path"]').value, "/shop");
  assert.equal(form.querySelector("[data-net-service]").value, "shop");
  assert.equal(form.querySelector("[data-net-port]").value, "8080");
  assert.equal(form.querySelector('[data-net-field="tls"]').value, "web-tls");
  assert.equal(form.elements.manifest, undefined);
  assert.equal(form.querySelector("textarea"), null);
});

test("structured edit submits JSON internally while preserving unedited fields", async () => {
  const object = ingress();
  const harness = makeHarness({ resources: exposureResources(object), discoveryResources: exposureResources(object) });
  await renderExpose(harness);
  harness.el.querySelector("[data-net-edit]").onclick();
  await new Promise((resolve) => setImmediate(resolve));
  const form = harness.document.querySelector("#netConfigForm");
  form.elements.reason.value = "publish shop route";
  form.elements.confirm.checked = true;
  form.onsubmit({ preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.posts.length, 1);
  assert.doesNotMatch(harness.posts[0].payload.yaml, /<textarea|manifest/);
  const payload = JSON.parse(harness.posts[0].payload.yaml);
  assert.equal(payload.metadata.annotations["example.com/owner"], "platform");
  assert.equal(payload.spec.customPolicy.mode, "preserve");
  assert.equal(payload.spec.defaultBackend.service.name, "fallback");
  assert.equal(payload.spec.rules[0].http.paths[0].extraPathField, "keep");
  assert.equal(payload.spec.rules[0].http.paths[0].backend.service.extraBackendField, "keep");
  assert.deepEqual(payload.spec.tls, [{ hosts: ["web.example.com"], secretName: "web-tls" }]);
});

test("stale asynchronous editor responses cannot open a form after page revision changes", async () => {
  const object = ingress();
  const harness = makeHarness({ resources: exposureResources(object), discoveryResources: exposureResources(object) });
  await renderExpose(harness);
  const edit = harness.el.querySelector("[data-net-edit]");
  edit.onclick();
  harness.page.rev = 12;
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(harness.document.querySelector("#netConfigForm"), null);
});
