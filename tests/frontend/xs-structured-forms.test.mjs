import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-structured-forms.js", import.meta.url), "utf8");
const plain = value => JSON.parse(JSON.stringify(value));
const decode = text => text.replace(/&(amp|lt|gt|quot|#39|times);/g, (_, name) =>
  ({ amp: "&", lt: "<", gt: ">", quot: '"', "#39": "'", times: "\u00d7" })[name]);

// Only HTML elements used by this module are needed. Values, selection, bubbling and
// re-parsing innerHTML model the browser behavior that source-regex tests cannot cover.
class Element {
  constructor(tag = "div", attributes = {}) {
    this.tagName = tag.toUpperCase();
    this.attributes = { ...attributes };
    this.children = [];
    this.parentElement = null;
    this.listeners = {};
    this._value = undefined;
    this.checked = Object.hasOwn(attributes, "checked");
    this.disabled = Object.hasOwn(attributes, "disabled");
    this.focused = false;
    this._text = "";
  }
  get id() { return this.getAttribute("id") || ""; }
  get type() { return this.getAttribute("type") || (this.tagName === "SELECT" ? "select-one" : "text"); }
  set type(value) { this.setAttribute("type", value); }
  get className() { return this.getAttribute("class") || ""; }
  set className(value) { this.setAttribute("class", value); }
  get value() {
    if (this._value !== undefined) return this._value;
    if (this.tagName === "SELECT") {
      const options = this.querySelectorAll("option");
      return (options.find(option => option.hasAttribute("selected")) || options[0])?.getAttribute("value") || "";
    }
    if (this.tagName === "TEXTAREA") return decode(this._text);
    return this.getAttribute("value") || "";
  }
  set value(value) { this._value = String(value); }
  get lastElementChild() { return this.children.at(-1); }
  set innerHTML(html) {
    for (const child of this.children) child.parentElement = null;
    this.children = [];
    appendHTML(this, html);
  }
  getAttribute(name) { return Object.hasOwn(this.attributes, name) ? this.attributes[name] : null; }
  hasAttribute(name) { return Object.hasOwn(this.attributes, name); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  removeAttribute(name) { delete this.attributes[name]; }
  matches(selector) {
    const attribute = /^\[([\w-]+)(?:="([^"]*)")?\]$/.exec(selector);
    if (attribute) return this.hasAttribute(attribute[1]) && (attribute[2] === undefined || this.getAttribute(attribute[1]) === attribute[2]);
    return this.tagName === selector.toUpperCase();
  }
  querySelectorAll(selector) {
    const result = [];
    const walk = element => {
      for (const child of element.children) {
        if (child.matches(selector)) result.push(child);
        walk(child);
      }
    };
    walk(this);
    return result;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) {
    for (let element = this; element; element = element.parentElement) if (element.matches(selector)) return element;
    return null;
  }
  contains(other) {
    for (let element = other; element; element = element.parentElement) if (element === this) return true;
    return false;
  }
  remove() {
    const parent = this.parentElement;
    if (parent) parent.children.splice(parent.children.indexOf(this), 1);
    this.parentElement = null;
  }
  focus() { this.focused = true; }
  insertAdjacentHTML(position, html) {
    assert.equal(position, "beforeend");
    appendHTML(this, html);
  }
  addEventListener(name, fn) { (this.listeners[name] ||= new Set()).add(fn); }
  removeEventListener(name, fn) { this.listeners[name]?.delete(fn); }
  dispatchEvent(event) {
    event.target ||= this;
    event.preventDefault ||= () => { event.defaultPrevented = true; };
    for (const listener of this.listeners[event.type] || []) listener(event);
    if (event.bubbles && this.parentElement) this.parentElement.dispatchEvent(event);
    return !event.defaultPrevented;
  }
}

function appendHTML(parent, html) {
  const stack = [parent];
  const tokens = html.match(/<[^>]*>|[^<]+/g) || [];
  for (const token of tokens) {
    if (token.startsWith("</")) { stack.pop(); continue; }
    if (!token.startsWith("<")) { stack.at(-1)._text += token; continue; }
    const tag = /^<([\w-]+)/.exec(token)?.[1];
    assert.ok(tag, `Unexpected markup: ${token}`);
    const attributes = {};
    const body = token.slice(tag.length + 1, -1);
    for (const attribute of body.matchAll(/([\w-]+)(?:="([^"]*)")?/g)) attributes[attribute[1]] = decode(attribute[2] || "");
    const element = new Element(tag, attributes);
    element.parentElement = stack.at(-1);
    element.parentElement.children.push(element);
    if (!["input", "br", "hr"].includes(tag)) stack.push(element);
  }
  assert.equal(stack.length, 1, "HTML tags are balanced");
}

function setup() {
  const window = {
    document: { getElementById: () => null },
    CustomEvent: class {
      constructor(type, init) { this.type = type; Object.assign(this, init); }
    }
  };
  vm.runInNewContext(source, { window, URL });
  return window.XSStructuredForms;
}

function fixture(api, kind, value, options) {
  const container = new Element();
  const form = api.mount(container, kind, value, options);
  return { form, element: form.element, container };
}
const field = (element, key) => element.querySelector(`[data-xsf-field="${key}"]`);
const rows = element => element.querySelectorAll("[data-xsf-row]");
const event = (target, type) => target.dispatchEvent({ type, bubbles: true });
const set = (element, key, value) => {
  const input = field(element, key);
  if (input.type === "checkbox") input.checked = value;
  else input.value = value;
};

test("HTML is escaped, ids are unique and unsafe or oversized JSON is rejected", () => {
  const api = setup();
  const html = api.keyValue({ '<script>"&': '"><img src=x onerror=1>' }, { id: 'unsafe" id' });
  assert.ok(html.includes("&lt;script&gt;"));
  assert.ok(!html.includes("<img"));
  const host = new Element();
  host.innerHTML = html + api.keyValue({}, { id: 'unsafe" id' });
  const forms = api.bind(host);
  assert.equal(forms.length, 2);
  assert.notEqual(forms[0].id, forms[1].id);
  assert.deepEqual(plain(forms[0].read()), { '<script>"&': '"><img src=x onerror=1>' });
  const ids = host.querySelectorAll("[id]").map(element => element.id);
  assert.equal(new Set(ids).size, ids.length);
  assert.throws(() => api.parseJSON('{"__proto__":{"polluted":true}}'), /不安全/);
  assert.throws(() => api.parseJSON('{"options":{"constructor":{}}}'), /不安全/);
  assert.throws(() => api.parseJSON(" ".repeat(2000001)), /过大/);
  assert.throws(() => api.parseJSON("[".repeat(66) + "0" + "]".repeat(66)), /层数/);
  assert.throws(() => api.keyValue({ value: Infinity }), /无效数值/);
  assert.equal({}.polluted, undefined);
  assert.throws(() => api.read(host), /指定/);
  assert.deepEqual(plain(api.read(host, forms[0].id)), { '<script>"&': '"><img src=x onerror=1>' });
});

test("key/value rendering, type changes, dynamic add/delete and refill preserve types", () => {
  const api = setup();
  const { form, element } = fixture(api, "keyValue", { text: "0042", count: 7, enabled: false }, { name: "options" });
  assert.equal(field(rows(element)[1], "value").type, "number");
  assert.equal(field(rows(element)[2], "value").type, "checkbox");
  assert.deepEqual(plain(form.read()), { text: "0042", count: 7, enabled: false });
  set(rows(element)[2], "value", true);
  event(element.querySelector('[data-xsf-action="add"]'), "click");
  let added = rows(element).at(-1);
  assert.equal(field(added, "key").focused, true);
  set(added, "key", "ratio");
  set(added, "type", "number");
  event(field(added, "type"), "change");
  assert.equal(field(added, "value").type, "number");
  set(added, "value", "1.25");
  assert.deepEqual(plain(form.read()), { text: "0042", count: 7, enabled: true, ratio: 1.25 });
  set(added, "value", "nan");
  assert.throws(() => form.read(), /数值/);
  event(added.querySelector('[data-xsf-action="remove"]'), "click");
  assert.equal(rows(element).length, 3);
  form.fill({ replacement: false });
  assert.equal(rows(element).length, 1);
  assert.deepEqual(plain(form.read()), { replacement: false });
  set(rows(element)[0], "type", "text");
  event(field(rows(element)[0], "type"), "change");
  assert.deepEqual(plain(form.read()), { replacement: "false" });
  form.addRow({ key: "replacement", value: "duplicate" });
  assert.throws(() => form.read(), /重复/);
});

test("binding is idempotent and destroy removes event handlers", () => {
  const api = setup();
  const { form, element } = fixture(api, "keyValue", {});
  api.bind(element);
  event(element.querySelector('[data-xsf-action="add"]'), "click");
  assert.equal(rows(element).length, 1);
  form.destroy();
  event(element.querySelector('[data-xsf-action="add"]'), "click");
  assert.equal(rows(element).length, 1);
  api.bind(element);
  event(element.querySelector('[data-xsf-action="add"]'), "click");
  assert.equal(rows(element).length, 2);
});

test("Service ports preserve named/numeric targets and reject invalid ports", () => {
  const api = setup();
  const value = [{ name: "http", protocol: "TCP", port: 80, targetPort: "web", appProtocol: "http" }];
  const { form, element } = fixture(api, "servicePorts", value);
  assert.deepEqual(plain(form.read()), value);
  set(rows(element)[0], "targetPort", "8080");
  set(rows(element)[0], "nodePort", "30080");
  assert.deepEqual(plain(form.read()), [{ ...value[0], targetPort: 8080, nodePort: 30080 }]);
  form.addRow({ name: "dns", protocol: "UDP", port: 53, targetPort: 5353 });
  assert.equal(form.read()[1].protocol, "UDP");
  set(rows(element)[1], "port", "65536");
  assert.throws(() => form.read(), /范围/);
  set(rows(element)[1], "port", "53.1");
  assert.throws(() => form.read(), /整数/);
  set(rows(element)[1], "port", "53");
  set(rows(element)[1], "name", "http");
  assert.throws(() => form.read(), /重复/);
  form.fill([{ port: 443 }]);
  assert.deepEqual(plain(form.read()), [{ protocol: "TCP", port: 443, targetPort: 443 }]);
});

test("selectors and ConfigMaps use literal strings and retain multiline data", () => {
  const api = setup();
  const { form, element } = fixture(api, "selector", { app: "web", enabled: "false", replicas: "5" });
  assert.deepEqual(plain(form.read()), { app: "web", enabled: "false", replicas: "5" });
  assert.equal(element.querySelectorAll('[data-xsf-field="type"]').length, 0);
  const cm = fixture(api, "configMap", { "nginx.conf": 'first\n<tag>& "second"\n', empty: "" });
  assert.equal(field(rows(cm.element)[0], "value").tagName, "TEXTAREA");
  assert.deepEqual(plain(cm.form.read()), { "nginx.conf": 'first\n<tag>& "second"\n', empty: "" });
  set(rows(cm.element)[0], "value", "updated\nline");
  assert.equal(cm.form.read()["nginx.conf"], "updated\nline");
});

test("Ingress rules and TLS refill from full resources without dropping advanced fields", () => {
  const api = setup();
  const original = {
    apiVersion: "networking.k8s.io/v1", metadata: { name: "web", annotations: { custom: "keep" } },
    spec: {
      ingressClassName: "nginx", defaultBackend: { service: { name: "fallback", port: { number: 80 } } },
      rules: [
        { host: "web.example", extra: 1, http: { custom: true, paths: [
          { path: "/api", pathType: "Prefix", custom: "keep", backend: { service: { name: "web", port: { name: "http" } } } },
          { path: "/static", pathType: "Exact", backend: { service: { name: "assets", port: { number: 8080 } } } }
        ] } },
        { host: "untouched.example" }
      ],
      tls: [{ secretName: "web-tls", hosts: ["web.example"], custom: true }, { secretName: "other", hosts: ["other.example"] }]
    }
  };
  const { form, element } = fixture(api, "ingressRules", original);
  assert.equal(rows(element).length, 2);
  assert.equal(field(rows(element)[0], "serviceName").value, "web");
  assert.equal(field(rows(element)[0], "tlsSecretName").value, "web-tls");
  assert.deepEqual(plain(form.read()), original);
  set(rows(element)[0], "serviceName", "new-web");
  set(rows(element)[0], "servicePort", "9090");
  const result = plain(form.read());
  assert.equal(result.spec.rules[0].http.paths[0].backend.service.port.number, 9090);
  assert.equal(result.spec.rules[0].http.paths[0].custom, "keep");
  assert.equal(result.spec.defaultBackend.service.name, "fallback");
  assert.deepEqual(original.spec.rules[0].http.paths[0].backend.service.port, { name: "http" });
  form.fill([{ host: "api.example", path: "/", pathType: "Prefix", serviceName: "api", servicePort: "80", tlsSecretName: "api-tls" }]);
  assert.deepEqual(plain(form.read()), {
    rules: [{ host: "api.example", http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "api", port: { number: 80 } } } }] } }],
    tls: [{ secretName: "api-tls", hosts: ["api.example"] }]
  });
});

test("Ingress supports resource backends and explicit TLS removal", () => {
  const api = setup();
  const value = { rules: [{ host: "web", http: { paths: [{ path: "/", pathType: "Prefix", backend: { resource: { kind: "StorageBucket", name: "assets" } } }] } }], tls: [{ hosts: ["web"], secretName: "tls" }] };
  const { form, element } = fixture(api, "ingressRules", value);
  assert.deepEqual(plain(form.read()), value);
  set(rows(element)[0], "tlsSecretName", "");
  assert.deepEqual(plain(form.read()).tls, []);
});

test("HPA renders and edits numeric fields while preserving non-CPU metrics and behavior", () => {
  const api = setup();
  const spec = {
    minReplicas: 2, maxReplicas: 8, scaleTargetRef: { kind: "Deployment", name: "api", apiVersion: "apps/v1" },
    metrics: [
      { type: "Resource", resource: { name: "cpu", target: { type: "Utilization", averageUtilization: 65 } } },
      { type: "Pods", pods: { metric: { name: "requests" }, target: { type: "AverageValue", averageValue: "5" } } }
    ], behavior: { scaleDown: { stabilizationWindowSeconds: 300 } }
  };
  const { form, element } = fixture(api, "hpa", { metadata: { name: "scaler" }, spec });
  assert.equal(field(element, "targetName").value, "api");
  assert.deepEqual(plain(form.read()), { metadata: { name: "scaler" }, spec });
  set(element, "maxReplicas", "12");
  set(element, "utilization", "80");
  const result = plain(form.read());
  assert.equal(result.spec.maxReplicas, 12);
  assert.equal(result.spec.metrics[0].resource.target.averageUtilization, 80);
  assert.deepEqual(result.spec.metrics[1], spec.metrics[1]);
  assert.deepEqual(result.spec.behavior, spec.behavior);
  set(element, "minReplicas", "13");
  assert.throws(() => form.read(), /不能超过/);
  form.fill({ scaleTargetRef: { kind: "Rollout", name: "custom", apiVersion: "argoproj.io/v1alpha1" }, maxReplicas: 3, metrics: [] });
  assert.equal(form.read().scaleTargetRef.kind, "Rollout");
  assert.deepEqual(plain(form.read().metrics), []);
});

test("CronJob round-trips fields and commands while retaining other containers and settings", () => {
  const api = setup();
  const original = {
    metadata: { name: "backup" }, spec: {
      schedule: "0 2 * * *", timeZone: "Asia/Shanghai", suspend: true, concurrencyPolicy: "Forbid",
      successfulJobsHistoryLimit: 2,
      jobTemplate: { metadata: { labels: { owner: "ops" } }, spec: { backoffLimit: 3, template: { spec: {
        restartPolicy: "Never", containers: [
          { name: "backup", image: "backup:v1", command: ["/bin/sh", "-c", "echo yes"], env: [{ name: "BUCKET", value: "x" }] },
          { name: "sidecar", image: "helper:v1" }
        ]
      } } } }
    }
  };
  const { form, element } = fixture(api, "cronJob", original);
  assert.equal(field(element, "command").value, "/bin/sh\n-c\necho yes");
  assert.equal(field(element, "suspend").checked, true);
  assert.deepEqual(plain(form.read()), original);
  set(element, "image", "backup:v2");
  set(element, "command", "/usr/bin/run\r\n--all");
  set(element, "suspend", false);
  const result = plain(form.read());
  const pod = result.spec.jobTemplate.spec.template.spec;
  assert.deepEqual(pod.containers[0].command, ["/usr/bin/run", "--all"]);
  assert.equal(pod.containers[0].env[0].name, "BUCKET");
  assert.equal(pod.containers[1].image, "helper:v1");
  assert.equal(result.spec.successfulJobsHistoryLimit, 2);
  assert.equal(result.spec.suspend, false);
  form.fill({});
  set(element, "schedule", "@hourly");
  set(element, "image", "busybox:1.37");
  assert.equal(form.read().jobTemplate.spec.template.spec.restartPolicy, "OnFailure");
  set(element, "schedule", "nonsense");
  assert.throws(() => form.read(), /Cron/);
});

test("Secret never embeds old values and returns explicit replacement/deletion patches", () => {
  const api = setup();
  const secret = { data: { TOKEN: "base64-old-secret" }, stringData: { PASSWORD: "plaintext-old-secret" } };
  const html = api.secret(secret);
  assert.ok(!html.includes("base64-old-secret"));
  assert.ok(!html.includes("plaintext-old-secret"));
  const { form, element } = fixture(api, "secret", secret);
  assert.equal(field(rows(element)[0], "value").value, "");
  assert.deepEqual(plain(form.read()), { set: {}, remove: [] });
  // A placeholder is status only; it never becomes a replacement value.
  field(rows(element)[0], "value").setAttribute("placeholder", "********");
  assert.deepEqual(plain(form.read()), { set: {}, remove: [] });
  set(rows(element)[0], "value", "new-token");
  event(field(rows(element)[0], "value"), "input");
  assert.equal(field(rows(element)[0], "replace").checked, true);
  event(rows(element)[1].querySelector('[data-xsf-action="remove"]'), "click");
  assert.deepEqual(plain(form.read()), { set: { TOKEN: "new-token" }, remove: ["PASSWORD"] });
  event(rows(element)[1].querySelector('[data-xsf-action="remove"]'), "click");
  set(rows(element)[1], "replace", true);
  assert.deepEqual(plain(form.read()), { set: { TOKEN: "new-token", PASSWORD: "" }, remove: [] });
  const added = form.addRow({ key: "NEW_KEY", value: "must-not-inject" });
  assert.equal(field(added, "value").value, "");
  set(added, "value", "entered");
  assert.equal(form.read().set.NEW_KEY, "entered");
  form.fill(secret);
  assert.deepEqual(plain(form.read()), { set: {}, remove: [] });
});

test("component candidate selection, manual override, refreshed discovery and URL validation", () => {
  const api = setup();
  const candidates = [
    { type: "loki", namespace: "logs", name: "loki", url_candidates: ["http://loki.logs.svc:3100"], state: "available" },
    { name: "blocked", url: "http://blocked", state: "forbidden" },
    { name: "unsafe", url: "javascript:alert(1)", state: "available" },
    { name: "missing", state: "missing" }
  ];
  const { form, element } = fixture(api, "componentEndpoint", { url: "https://outside.example", cluster_id: 7 }, { candidates });
  const select = field(element, "candidate");
  assert.equal(select.value, "");
  const options = select.querySelectorAll("option");
  assert.equal(options[2].disabled, true);
  assert.equal(options[3].disabled, true);
  select.value = "0";
  event(select, "change");
  assert.equal(field(element, "url").value, "http://loki.logs.svc:3100");
  assert.deepEqual(plain(form.read()), { url: "http://loki.logs.svc:3100", cluster_id: 7 });
  set(element, "url", "https://manual.example:8443");
  event(field(element, "url"), "input");
  assert.equal(select.value, "");
  form.updateCandidates([{ name: "new", url: "http://new.svc:3100" }]);
  assert.equal(field(element, "url").value, "https://manual.example:8443");
  set(element, "url", "javascript:alert(1)");
  assert.throws(() => form.read(), /HTTP/);
  set(element, "url", "https://user:secret@example.com");
  assert.throws(() => form.read(), /凭据/);
  set(element, "url", "");
  assert.equal(form.read().url, "");
  const required = fixture(api, "componentEndpoint", {}, { required: true });
  assert.throws(() => required.form.read(), /HTTP/);
});

test("serialized object inputs and empty values survive render, edit refill and read", () => {
  const api = setup();
  const config = fixture(api, "configMap", '{"EMPTY":"","ZERO":"0","MULTILINE":"first\\nsecond"}');
  assert.deepEqual(plain(config.form.read()), { EMPTY: "", ZERO: "0", MULTILINE: "first\nsecond" });
  config.form.fill('{"EMPTY":"","UPDATED":"yes"}');
  assert.deepEqual(plain(config.form.read()), { EMPTY: "", UPDATED: "yes" });

  const selector = fixture(api, "selector", '{"app":"orders","track":""}');
  assert.deepEqual(plain(selector.form.read()), { app: "orders", track: "" });

  const typed = fixture(api, "keyValue", { empty: "", zero: 0, disabled: false });
  assert.deepEqual(plain(typed.form.read()), { empty: "", zero: 0, disabled: false });
});

test("full-resource edits preserve metadata when spec starts absent", () => {
  const api = setup();
  const resource = {
    apiVersion: "autoscaling/v2", kind: "HorizontalPodAutoscaler",
    metadata: { name: "api-scaler", namespace: "production", labels: { owner: "platform" } }
  };
  const { form, element } = fixture(api, "hpa", JSON.stringify(resource));
  set(element, "targetName", "api");
  const result = plain(form.read());
  assert.equal(result.apiVersion, resource.apiVersion);
  assert.equal(result.kind, resource.kind);
  assert.deepEqual(result.metadata, resource.metadata);
  assert.equal(result.spec.scaleTargetRef.name, "api");
  assert.equal(result.spec.maxReplicas, 10);
});

test("rule rows refill from serialized resources and add/remove without losing TLS data", () => {
  const api = setup();
  const original = {
    metadata: { name: "gateway", annotations: { owner: "ops" } },
    spec: {
      ingressClassName: "nginx",
      rules: [{ host: "api.example", http: { paths: [
        { path: "/", pathType: "Prefix", backend: { service: { name: "api", port: { number: 8080 } } } }
      ] } }],
      tls: [{ hosts: ["api.example"], secretName: "api-tls" }]
    }
  };
  const { form, element } = fixture(api, "ingressRules", {});
  form.fill(JSON.stringify(original));
  assert.equal(rows(element).length, 1);
  assert.equal(field(rows(element)[0], "serviceName").value, "api");
  assert.deepEqual(plain(form.read()), original);
  form.addRow({ host: "admin.example", path: "/", pathType: "Prefix", serviceName: "admin", servicePort: "80" });
  assert.equal(rows(element).length, 2);
  let result = plain(form.read());
  assert.equal(result.spec.rules.length, 2);
  assert.equal(result.spec.rules[1].http.paths[0].backend.service.port.number, 80);
  event(rows(element)[1].querySelector('[data-xsf-action="remove"]'), "click");
  result = plain(form.read());
  assert.equal(rows(element).length, 1);
  assert.deepEqual(result, original);
});

test("generic object fields render typed controls, preserve unknown fields and validate writes", () => {
  const api = setup();
  const { form, element } = fixture(api, "objectFields", { name: "api", replicas: 2, enabled: false, extra: "keep" }, {
    fields: [
      { key: "name", label: "名称", required: true },
      { key: "replicas", label: "副本数", type: "number", min: 1, max: 10 },
      { key: "enabled", label: "启用", type: "boolean" },
      { key: "mode", label: "模式", type: "select", options: ["safe", "fast"] }
    ]
  });
  assert.equal(field(element, "name").value, "api");
  assert.equal(field(element, "replicas").value, "2");
  assert.equal(field(element, "enabled").checked, false);
  assert.deepEqual(plain(form.read()), { name: "api", replicas: 2, enabled: false, extra: "keep" });
  set(element, "mode", "fast");
  assert.deepEqual(plain(form.read()), { name: "api", replicas: 2, enabled: false, extra: "keep", mode: "fast" });
  set(element, "replicas", "11");
  assert.throws(() => form.read(), /数值无效/);
  set(element, "replicas", "4");
  set(element, "name", "");
  assert.throws(() => form.read(), /请填写/);
  form.fill({ name: "worker", replicas: 3, enabled: true, extra: "preserved" });
  assert.deepEqual(plain(form.read()), { name: "worker", replicas: 3, enabled: true, extra: "preserved" });
});

test("Secret edit refill accepts serialized metadata and never exposes configured values", () => {
  const api = setup();
  const { form, element } = fixture(api, "secret", {});
  const serialized = '{"data":{"TOKEN":"base64-value"},"stringData":{"PASSWORD":"plain-value"}}';
  const markup = api.render("secret", serialized);
  form.fill(serialized);
  assert.deepEqual(rows(element).map(row => field(row, "key").value), ["TOKEN", "PASSWORD"]);
  assert.equal(field(rows(element)[0], "value").value, "");
  assert.deepEqual(plain(form.read()), { set: {}, remove: [] });
  assert.ok(!markup.includes("base64-value"));
  assert.ok(!markup.includes("plain-value"));
  form.addRow({ key: "EMPTY" });
  set(rows(element)[2], "value", "");
  assert.deepEqual(plain(form.read()), { set: {}, remove: [] });
  event(rows(element)[2].querySelector('[data-xsf-action="remove"]'), "click");
  assert.equal(rows(element).length, 2);
});
