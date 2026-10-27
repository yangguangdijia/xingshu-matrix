import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/index.html", import.meta.url), "utf8");
const start = source.indexOf("      function bindNetpolAIDraft(form, editing) {");
const end = source.indexOf("      async function renderNetpolLive(", start);
assert.ok(start >= 0 && end > start, "NetworkPolicy AI draft binding should be present");
const binding = source.slice(start, end);
const scriptOpen = source.lastIndexOf("<script>", start);
const scriptClose = source.indexOf("</script>", start);
assert.ok(scriptOpen >= 0 && scriptClose > start, "NetworkPolicy modal should be inside an inline script");
assert.doesNotThrow(() => new vm.Script(source.slice(scriptOpen + "<script>".length, scriptClose)));

function createHarness({ editing = false, models = [{ id: 1 }], fields, getError, canAI = true } = {}) {
  const calls = [];
  const status = {
    textContent: "",
    role: "",
    setAttribute(name, value) { if (name === "role") this.role = value; }
  };
  const input = { value: "allow web to api on TCP 8080", focused: false, focus() { this.focused = true; } };
  const button = { disabled: false, textContent: "生成草稿" };
  const root = {
    querySelector(selector) {
      return ({
        "[data-netpol-ai-instruction]": input,
        "[data-netpol-ai-generate]": button,
        "[data-netpol-ai-status]": status
      })[selector] || null;
    }
  };
  const submit = () => {};
  const form = {
    elements: {
      aiInstruction: input,
      namespace: { value: editing ? "prod" : "default" },
      name: { value: editing ? "api-policy" : "" },
      labels: { value: '{"owner":"manual"}' },
      spec: { value: '{"podSelector":{"matchLabels":{"app":"manual"}}}' },
      reason: { value: "manual reason" },
      confirm: { checked: false }
    },
    querySelector(selector) { return selector === "[data-netpol-ai-draft]" ? root : null; },
    onsubmit: submit
  };
  const XS = {
    can(permission) { return permission === "ai:chat" && canAI; },
    async get(path) {
      calls.push({ method: "GET", path });
      if (getError) throw new Error(getError);
      return models;
    },
    async post(path, body, options) {
      calls.push({ method: "POST", path, body, options });
      return { fields };
    }
  };
  const context = vm.createContext({ XS });
  vm.runInContext(binding, context);
  context.bindNetpolAIDraft(form, editing);
  return { calls, form, input, button, status, submit };
}

test("create drafts use the configured AI endpoint and fill only valid policy fields", async () => {
  const { calls, form, button, status, submit } = createHarness({
    fields: {
      namespace: "prod",
      name: "api-access",
      labels: { app: "api" },
      spec: { podSelector: { matchLabels: { app: "api" } }, policyTypes: ["Ingress"], ingress: [] },
      reason: "generated reason",
      confirm: true,
      extra: "ignored"
    }
  });

  await button.onclick();

  assert.deepEqual(calls.map(({ method, path }) => [method, path]), [
    ["GET", "/ai/models/options"],
    ["POST", "/ai/assist"]
  ]);
  const assist = calls[1];
  assert.equal(assist.body.scene, "k8s.networkpolicy.create");
  assert.equal(assist.body.instruction, "allow web to api on TCP 8080");
  assert.deepEqual(Object.keys(assist.body.schema).sort(), ["labels", "name", "namespace", "spec"]);
  assert.equal(assist.options.timeoutMs, 60000);
  assert.equal(form.elements.namespace.value, "prod");
  assert.equal(form.elements.name.value, "api-access");
  assert.deepEqual(JSON.parse(form.elements.labels.value), { app: "api" });
  assert.deepEqual(JSON.parse(form.elements.spec.value), {
    podSelector: { matchLabels: { app: "api" } },
    policyTypes: ["Ingress"],
    ingress: []
  });
  assert.equal(form.elements.reason.value, "manual reason");
  assert.equal(form.elements.confirm.checked, false);
  assert.equal(form.onsubmit, submit);
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, "生成草稿");
  assert.match(status.textContent, /请检查后填写变更原因并单独提交/);
});

test("edit drafts expose only labels and spec and keep resource identity unchanged", async () => {
  const { calls, form, button } = createHarness({
    editing: true,
    fields: {
      namespace: "other",
      name: "renamed",
      labels: { team: "platform" },
      spec: { podSelector: {}, policyTypes: ["Egress"], egress: [] }
    }
  });

  await button.onclick();

  const assist = calls.find((call) => call.path === "/ai/assist");
  assert.equal(assist.body.scene, "k8s.networkpolicy.update");
  assert.deepEqual(Object.keys(assist.body.schema).sort(), ["labels", "spec"]);
  assert.equal(form.elements.namespace.value, "prod");
  assert.equal(form.elements.name.value, "api-policy");
  assert.deepEqual(JSON.parse(form.elements.labels.value), { team: "platform" });
  assert.deepEqual(JSON.parse(form.elements.spec.value), { podSelector: {}, policyTypes: ["Egress"], egress: [] });
});

test("AI failures preserve all manually entered policy and approval fields", async () => {
  const { calls, form, button, status } = createHarness({ getError: "model service unavailable" });
  const before = {
    namespace: form.elements.namespace.value,
    name: form.elements.name.value,
    labels: form.elements.labels.value,
    spec: form.elements.spec.value,
    reason: form.elements.reason.value,
    confirm: form.elements.confirm.checked
  };

  await button.onclick();

  assert.deepEqual(calls.map(({ method, path }) => [method, path]), [["GET", "/ai/models/options"]]);
  assert.deepEqual({
    namespace: form.elements.namespace.value,
    name: form.elements.name.value,
    labels: form.elements.labels.value,
    spec: form.elements.spec.value,
    reason: form.elements.reason.value,
    confirm: form.elements.confirm.checked
  }, before);
  assert.match(status.textContent, /model service unavailable/);
  assert.equal(status.role, "alert");
  assert.equal(button.disabled, false);
});

test("AI draft is unavailable without an enabled model and never submits the form", async () => {
  const { calls, form, button, status, submit } = createHarness({ models: [] });

  await button.onclick();

  assert.deepEqual(calls.map(({ method, path }) => [method, path]), [["GET", "/ai/models/options"]]);
  assert.equal(form.onsubmit, submit);
  assert.match(status.textContent, /尚未配置已启用的 AI 模型/);
});

test("AI draft makes no requests without ai:chat permission", async () => {
  const { calls, form, button, status, submit } = createHarness({ canAI: false });
  const before = [form.elements.labels.value, form.elements.spec.value, form.elements.reason.value];

  await button.onclick();

  assert.deepEqual(calls, []);
  assert.deepEqual([form.elements.labels.value, form.elements.spec.value, form.elements.reason.value], before);
  assert.equal(form.onsubmit, submit);
  assert.match(status.textContent, /没有 ai:chat 权限/);
});
