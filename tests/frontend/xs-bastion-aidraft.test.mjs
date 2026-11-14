import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, tick } from "./xs-resource-pages.harness.mjs";

const source = fs.readFileSync(new URL("../../public/xs-bastion-aidraft.js", import.meta.url), "utf8");

function setup(api) {
  const calls = [];
  const head = new Element("head");
  const body = new Element("body");
  const docListeners = {};
  const document = {
    head,
    body,
    createElement: (tag) => new Element(tag),
    getElementById: (id) => head.children.find((n) => n.id === id) || null,
    addEventListener: (t, fn) => { docListeners[t] = fn; },
    removeEventListener: (t) => { delete docListeners[t]; }
  };
  const window = {
    document,
    XS: { api: async (path, options) => { calls.push({ path, options }); return api(path, options); } }
  };
  const context = vm.createContext({ window, setTimeout: (fn) => fn(), console });
  vm.runInContext(source, context);
  return { window, document, body, head, calls, docListeners };
}

async function click(root, target) {
  for (const fn of root.listeners.click || []) await fn({ target });
  await tick();
}

const goodResult = {
  draft: { name: "禁止删除根目录", match_type: "regex", pattern: "^rm\\s+-rf\\s+/(\\s|$)", action: "deny", reason: "拦截 <危险> 删除" },
  valid: true,
  errors: [],
  preview: [
    { command: "rm -rf /", expected: "block", matched: true, pass: true },
    { command: "ls; sudo rm -rf /", expected: "block", matched: true, pass: true },
    { command: "rm -rf ./tmp", expected: "allow", matched: false, pass: true }
  ]
};

test("generates a draft, renders preview with escaping, and applies it", async () => {
  const h = setup(async () => goodResult);
  let applied = null;
  h.window.XSBastionAIDraft.open({ onApply: (d) => { applied = d; } });
  const root = h.body.children[0];
  assert.ok(root, "drawer mounted");
  assert.equal(h.head.children.length, 1, "style injected once");
  assert.match(root.innerHTML, /AI 起草命令过滤规则/);

  root.querySelector("[data-xsad-desc]").value = "禁止删除根目录";
  root.querySelector("[data-xsad-block]").value = "rm -rf /\n\n  ls; sudo rm -rf /  \n";
  root.querySelector("[data-xsad-allow]").value = "rm -rf ./tmp";
  const apply = root.querySelector("[data-xsad-apply]");
  assert.equal(apply.disabled, true);

  await click(root, root.querySelector("[data-xsad-generate]"));

  assert.equal(h.calls.length, 1);
  assert.equal(h.calls[0].path, "/bastion/command-filters/ai-draft");
  assert.equal(h.calls[0].options.method, "POST");
  assert.deepEqual(JSON.parse(JSON.stringify(h.calls[0].options.body)), {
    description: "禁止删除根目录",
    samples_block: ["rm -rf /", "ls; sudo rm -rf /"],
    samples_allow: ["rm -rf ./tmp"]
  });
  const result = root.querySelector("[data-xsad-result]");
  assert.match(result.innerHTML, /✓ 草稿校验通过/);
  assert.match(result.innerHTML, /拦截 &lt;危险&gt; 删除/);
  assert.doesNotMatch(result.innerHTML, /<危险>/);
  assert.equal(result.querySelectorAll("tr").length, 4);
  assert.equal(result.querySelectorAll('[data-xsad-row="pass"]').length, 3);
  assert.equal(apply.disabled, false);

  await click(root, apply);
  assert.equal(applied.name, "禁止删除根目录");
  assert.equal(applied.pattern, "^rm\\s+-rf\\s+/(\\s|$)");
  assert.equal(applied.action, "deny");
  assert.equal(applied.valid, true);
  assert.equal(h.body.children.length, 0, "drawer closed after apply");
});

test("invalid draft shows errors and ✗ rows", async () => {
  const h = setup(async () => ({
    draft: { name: "删除", match_type: "regex", pattern: "^rm\\b", action: "deny", reason: "" },
    valid: false,
    errors: ["有 1 条样例与预期不符"],
    preview: [{ command: "rm -rf ./tmp", expected: "allow", matched: true, pass: false }]
  }));
  h.window.XSBastionAIDraft.open({ onApply() {} });
  const root = h.body.children[0];
  root.querySelector("[data-xsad-desc]").value = "禁止删除";
  await click(root, root.querySelector("[data-xsad-generate]"));
  const result = root.querySelector("[data-xsad-result]");
  assert.match(result.innerHTML, /✗ 草稿未通过校验/);
  assert.match(result.innerHTML, /有 1 条样例与预期不符/);
  assert.equal(result.querySelectorAll('[data-xsad-row="fail"]').length, 1);
  assert.match(root.querySelector("[data-xsad-status]").textContent, /草稿存在问题/);
});

test("empty description is rejected locally and 503 shows 未配置大模型", async () => {
  const h = setup(async () => { throw { status: 503, code: 50300, message: "未配置大模型" }; });
  h.window.XSBastionAIDraft.open({});
  const root = h.body.children[0];
  const status = root.querySelector("[data-xsad-status]");
  await click(root, root.querySelector("[data-xsad-generate]"));
  assert.equal(h.calls.length, 0);
  assert.match(status.textContent, /请先填写规则描述/);

  root.querySelector("[data-xsad-desc]").value = "禁止 mkfs";
  await click(root, root.querySelector("[data-xsad-generate]"));
  assert.equal(h.calls.length, 1);
  assert.match(status.textContent, /生成失败：未配置大模型/);
  assert.equal(root.querySelector("[data-xsad-apply]").disabled, true);
  assert.equal(root.querySelector("[data-xsad-generate]").disabled, false);

  await click(root, root.querySelector("[data-xsad-cancel]"));
  assert.equal(h.body.children.length, 0);
  assert.equal(h.docListeners.keydown, undefined, "Escape listener removed");
});
