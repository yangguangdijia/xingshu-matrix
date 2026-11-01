import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-live-ops.js", import.meta.url), "utf8");
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
const plain = (value) => JSON.parse(JSON.stringify(value));

// A minimal DOM: formModal looks its <form> up by id, so the fake document
// hands out one fake form per generated id with the values a user would enter.
function setup(api) {
  const calls = [];
  const toasts = [];
  const dialogs = [];
  const forms = {};
  const errs = {};
  const submits = {};
  let nextValues = {};
  let closed = 0;
  function fakeForm(id, values) {
    const elements = {};
    for (const [key, value] of Object.entries(values)) {
      elements[key] = { value, disabled: false, readOnly: false, setAttribute(name, v) { this["attr:" + name] = v; } };
    }
    return { id, elements, querySelector() { return null; }, querySelectorAll() { return []; }, onsubmit: null };
  }
  const document = {
    body: { children: [], contains() { return false; }, appendChild() {} },
    head: { appendChild() {} },
    documentElement: { appendChild() {} },
    addEventListener() {},
    createElement() { return { style: {}, setAttribute() {} }; },
    getElementById(id) {
      if (/^opsf\d+err$/.test(id)) return (errs[id] ||= { hidden: true, textContent: "" });
      if (/^opsf\d+$/.test(id)) return (forms[id] ||= fakeForm(id, nextValues));
      return null;
    },
    querySelector(selector) {
      const match = /^\[form="(opsf\d+)"\]$/.exec(selector);
      return match ? (submits[match[1]] ||= { disabled: false }) : null;
    }
  };
  const window = {
    addEventListener() {},
    toast(message) { toasts.push(message); },
    XS: { mode: "demo", api(path, opts) { calls.push({ path, opts: plain(opts || {}) }); return api(path, opts || {}); } }
  };
  const context = vm.createContext({
    window, document, location: { hash: "" }, console, setTimeout, clearTimeout, setInterval, clearInterval,
    dlg(title, html, footer) { dialogs.push({ title, html, footer }); },
    closeAll() { closed++; }
  });
  vm.runInContext(source, context);
  const ops = window.XSLiveOps._internals;
  return {
    ops, calls, toasts, dialogs,
    setValues(values) { nextValues = values; },
    lastForm() { const ids = Object.keys(forms); return forms[ids[ids.length - 1]]; },
    errOf(form) { return errs[form.id + "err"]; },
    submitOf(form) { return submits[form.id]; },
    closed: () => closed
  };
}

const clusters = { items: [
  { id: 3, name: "prod-a", env: "prod", kubeconfig: "********" },
  { id: 4, name: "dev-agent", env: "dev" }
], total: 2 };

test("create form lists registered clusters from /clusters and posts cluster_id without kubeconfig", async () => {
  const posted = [];
  const h = setup((path, opts) => {
    if (path === "/clusters") return Promise.resolve(clusters);
    if (path === "/cicd/targets" && opts.method === "POST") { posted.push(plain(opts.body)); return Promise.resolve({ id: 11 }); }
    return Promise.reject({ status: 404, message: "unexpected " + path });
  });
  h.setValues({ name: "ci-prod", cluster_id: "3", env: "prod", namespace: "" });
  let done = 0;
  h.ops.targetModal(() => { done++; });
  await flush(); await flush();

  assert.equal(h.calls[0].path, "/clusters");
  const dialog = h.dialogs.at(-1);
  assert.match(dialog.html, /<select class="x-select" name="cluster_id"/);
  assert.match(dialog.html, /<option value="3" selected>prod-a \(prod\)<\/option>/);
  assert.match(dialog.html, /<option value="4">dev-agent \(dev\)<\/option>/);
  assert.match(dialog.html, /name="env" value="prod"/);
  assert.match(dialog.html, /name="namespace" value="matrix-cicd"/);
  assert.doesNotMatch(dialog.html, /name="kubeconfig"|<textarea/);
  assert.doesNotMatch(dialog.html, /role="tablist"/);

  const form = h.lastForm();
  assert.equal(form.elements.env.readOnly, true, "env is derived from the cluster and read-only");
  assert.equal(form.elements.cluster_id.disabled, false);
  form.elements.cluster_id.value = "4";
  form.elements.cluster_id.onchange();
  assert.equal(form.elements.env.value, "dev");
  form.elements.cluster_id.value = "3";
  form.elements.cluster_id.onchange();

  form.onsubmit({ preventDefault() {} });
  await flush(); await flush();
  assert.deepEqual(posted, [{ cluster_id: 3, namespace: "matrix-cicd", name: "ci-prod" }]);
  assert.equal(Object.hasOwn(posted[0], "kubeconfig"), false);
  assert.equal(Object.hasOwn(posted[0], "env"), false);
  assert.equal(done, 1);
  assert.equal(h.closed(), 1);
  assert.deepEqual(h.toasts, ["执行集群已接入"]);
});

for (const [label, api] of [
  ["empty", () => Promise.resolve({ items: [], total: 0 })],
  ["failed", () => Promise.reject({ status: 503, message: "cluster registry down" })]
]) {
  test(`create form fails closed when the cluster list is ${label}`, async () => {
    const posted = [];
    const h = setup((path, opts) => {
      if (path === "/clusters") return api();
      posted.push(path + " " + opts.method);
      return Promise.resolve({});
    });
    h.setValues({ name: "ci", cluster_id: "", env: "", namespace: "matrix-cicd" });
    h.ops.targetModal(() => {});
    await flush(); await flush();
    const dialog = h.dialogs.at(-1);
    assert.match(dialog.html, /未注册集群，请先在集群管理接入/);
    assert.match(dialog.html, /<option value="" selected>未注册集群，请先在集群管理接入<\/option>/);
    assert.doesNotMatch(dialog.html, /<option value="[1-9]/, "no fabricated cluster options");
    if (label === "failed") assert.match(dialog.html, /集群列表读取失败：cluster registry down/);
    const form = h.lastForm();
    assert.equal(form.elements.cluster_id.disabled, true);
    assert.equal(h.submitOf(form).disabled, true);
    form.onsubmit({ preventDefault() {} });
    await flush();
    assert.deepEqual(posted, []);
    assert.equal(h.errOf(form).hidden, false);
  });
}

test("legacy rows show the kubeconfig tag and a link action; linked rows show the cluster name", () => {
  const { ops } = setup(() => Promise.resolve([]));
  const legacy = ops.targetRowCells({ id: 7, name: "old", namespace: "ci", env: "prod", legacy_kubeconfig: true, kubeconfig: "********", cluster_id: null });
  const legacyHTML = legacy.join("");
  assert.match(legacyHTML, /旧版 kubeconfig/);
  assert.match(legacyHTML, /data-tglink="7"[^>]*>关联集群</);
  assert.match(legacyHTML, /data-tgdel="7"/);
  assert.doesNotMatch(legacyHTML, /\*\*\*\*/, "the list never renders credential masks");

  const linked = ops.targetRowCells({ id: 8, name: "new", namespace: "ci", env: "prod", legacy_kubeconfig: false, cluster_id: 3, cluster_name: "prod-<a>" });
  const linkedHTML = linked.join("");
  assert.match(linkedHTML, /prod-&lt;a&gt;/);
  assert.doesNotMatch(linkedHTML, /data-tglink|旧版 kubeconfig/);

  const unreadable = ops.targetRowCells({ id: 9, name: "x", cluster_id: 5, cluster_name: "" }).join("");
  assert.match(unreadable, /#5/);
  assert.match(unreadable, /集群不可读/);
  assert.match(source, /tbl\(\["名称", "集群", "环境", "命名空间", "创建时间", "操作"\], tgRows\)/);
});

test("link action opens the same form against PUT /cicd/targets/:id", async () => {
  const put = [];
  const h = setup((path, opts) => {
    if (path === "/clusters") return Promise.resolve(clusters);
    if (opts.method === "PUT") { put.push({ path, body: plain(opts.body) }); return Promise.reject({ status: 202, code: 40304, approval: { ticket: { id: 42 } } }); }
    return Promise.reject({ status: 404 });
  });
  h.setValues({ cluster_id: "3", env: "prod", namespace: "ci-runner" });
  h.ops.targetModal(() => {}, { id: 7, name: "old", namespace: "ci-runner", env: "prod", legacy_kubeconfig: true, cluster_id: null });
  await flush(); await flush();
  const dialog = h.dialogs.at(-1);
  assert.match(dialog.title, /关联集群/);
  assert.doesNotMatch(dialog.html, /name="name"/, "linking keeps the target name");
  assert.match(dialog.html, /清除「old」保存的旧版 kubeconfig/);
  assert.match(dialog.html, /name="namespace" value="ci-runner"/);
  h.lastForm().onsubmit({ preventDefault() {} });
  await flush(); await flush();
  assert.deepEqual(put, [{ path: "/cicd/targets/7", body: { cluster_id: 3, namespace: "ci-runner" } }]);
  assert.deepEqual(h.toasts, ["已提交审批（审批单 #42）"], "an approval is reported as pending, not as linked");
});

test("target payload requires a registered cluster id and never carries kubeconfig", () => {
  const { ops } = setup(() => Promise.resolve([]));
  assert.throws(() => ops.targetPayload({ name: "a", cluster_id: "" }), /请选择已注册集群/);
  assert.throws(() => ops.targetPayload({ name: "a", cluster_id: "abc" }), /请选择已注册集群/);
  assert.throws(() => ops.targetPayload({ name: "", cluster_id: "2" }), /名称/);
  assert.deepEqual(plain(ops.targetPayload({ name: " a ", cluster_id: "2", namespace: "", kubeconfig: "apiVersion: v1" })),
    { cluster_id: 2, namespace: "matrix-cicd", name: "a" });
  assert.deepEqual(plain(ops.targetPayload({ cluster_id: "2", namespace: "ns" }, { id: 1 })), { cluster_id: 2, namespace: "ns" });
  assert.deepEqual(plain(ops.targetClusterOptions([{ id: "x" }, { id: 0 }, { id: 2, name: "c", env: "" }])),
    [{ v: "2", t: "c (未设环境)", env: "" }]);
});
