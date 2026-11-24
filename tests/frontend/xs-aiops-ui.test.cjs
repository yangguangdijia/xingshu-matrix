"use strict";
// node --test public/tests/xs-aiops-ui.test.js
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function load() {
  const window = {};
  const ctx = vm.createContext({ window, console, setTimeout, clearTimeout });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "..", "..", "public", "xs-aiops-ui.js"), "utf8"), ctx);
  return window.XSAiopsUI;
}
const U = load();
const T = U._t;

test("路由识别", () => {
  for (const k of ["cases", "runbooks", "policy", "case/12"]) assert.ok(U.has(k), k);
  for (const k of ["", "case/", "case/abc", "settings"]) assert.ok(!U.has(k), k);
});

test("工单与步骤状态映射为中文", () => {
  assert.equal(T.caseStatusName("investigating"), "排查中");
  assert.equal(T.caseStatusName("concluded"), "待审阅");
  assert.equal(T.caseStatusName("plan_accepted"), "已采纳方案");
  assert.equal(T.caseStatusName("awaiting_approval"), "等待审批");
  assert.equal(T.caseStatusName("resolved"), "已解决");
  assert.equal(T.caseStatusName("weird"), "weird");
  assert.equal(T.caseStatusName(""), "—");
  assert.equal(T.stepStatusName("manual_done"), "人工完成");
  assert.equal(T.stepStatusName("rolled_back"), "已回滚");
  assert.equal(T.stepStatusName("blocked"), "已拦截");
  assert.ok(T.isActive("executing") && T.isActive("investigating"));
  assert.ok(!T.isActive("resolved") && !T.isActive("failed"));
  assert.match(T.caseTag("failed"), /x-red/);
});

test("风险标签与自动处置限制", () => {
  assert.equal(T.riskName("read"), "只读");
  assert.equal(T.riskName("change"), "变更");
  assert.equal(T.riskName("high"), "高风险");
  assert.match(T.riskTag("high"), /x-red[^"]*">高风险</);
  assert.match(T.riskTag("read"), /x-green/);
  assert.equal(T.maxRisk([{ risk: "read" }, { risk: "high" }, { risk: "change" }]), "high");
  assert.equal(T.maxRisk([]), "read");
  assert.equal(T.canAutoExecute("high"), false);
  assert.equal(T.canAutoExecute("change"), true);
  assert.equal(T.executorName("human"), "人工");
  assert.equal(T.executorName("ai"), "AI");
});

test("步骤表渲染：中文动作名、参数、风险、执行者、失败高亮、转义", () => {
  const actions = [{ name: "k8s.restart", title: "滚动重启工作负载" }, { name: "k8s.scale", title: "调整副本数" }];
  const steps = [
    { id: 2, seq: 2, phase: "verify", action: "k8s.get", params: {}, risk: "read", status: "pending" },
    { id: 1, seq: 1, phase: "main", action: "k8s.restart", params: { namespace: "prod", name: "<web>" }, risk: "change", executor: "human", status: "failed", error: "超时" },
    { id: 3, seq: 3, phase: "main", action: "host.exec", params: { password: "s3cret", cmd: "uptime" }, risk: "high", status: "succeeded", title: "执行命令" }
  ];
  const html = T.stepRows(steps, { actions });
  assert.match(html, /滚动重启工作负载/);
  assert.match(html, /namespace：prod/);
  assert.match(html, /&lt;web&gt;/);
  assert.ok(!html.includes("<web>"));
  assert.ok(!html.includes("s3cret"), "凭据参数必须打码");
  assert.match(html, /password：\*\*\*\*\*\*/);
  assert.match(html, /<tr data-step-row="1" class="xai-row-bad">/);
  assert.match(html, />人工</);
  assert.match(html, />高风险</);
  assert.match(html, />验证</);
  // 验证步骤排在最后
  assert.ok(html.indexOf('data-step-row="2"') > html.indexOf('data-step-row="3"'));
  assert.ok(!/\/api\/v1|\/aiops\//.test(html), "界面不出现接口路径");
  const edit = T.stepRows(steps, { actions, edit: true });
  assert.match(edit, /data-step-params="1"/);
  assert.ok(!/data-step-params="2"/.test(edit), "验证步骤不可编辑");
  const ctl = T.stepRows(steps, { controls: (s) => (s.id === 1 ? "<button>完成</button>" : "") });
  assert.match(ctl, /<th>操作<\/th>/);
  assert.match(T.stepRows([], {}), /没有步骤/);
});

test("修改参数：合法 JSON 读回，非法 JSON 报中文错误", () => {
  const vals = { 1: '{"replicas":3}', x1: "human" };
  const box = {
    querySelector(sel) {
      let m = sel.match(/data-step-params="(\d+)"/);
      if (m) return vals[m[1]] != null ? { value: vals[m[1]] } : null;
      m = sel.match(/data-step-exec="(\d+)"/);
      if (m) return { value: vals["x" + m[1]] };
      return null;
    }
  };
  const out = T.collectStepEdits(box, [{ id: 1, seq: 1, phase: "main" }, { id: 9, seq: 2, phase: "verify" }]);
  assert.equal(out.length, 1);
  assert.equal(out[0].params.replicas, 3);
  assert.equal(out[0].executor, "human");
  vals[1] = "{bad";
  assert.throws(() => T.collectStepEdits(box, [{ id: 1, seq: 1, phase: "main" }]), /第 1 步的参数不是合法的 JSON/);
});

function fakeForm(v) {
  const elements = {};
  for (const k of Object.keys(v)) elements[k] = { value: v[k] };
  return { elements, querySelector: () => null };
}

test("密码字段清空：clearSecretFields 与 onboardPayload", () => {
  const f = fakeForm({ password: "p@ss", private_key: "-----BEGIN KEY-----", username: "root" });
  assert.deepEqual([...T.clearSecretFields(f)].sort(), ["password", "private_key"]);
  assert.equal(f.elements.password.value, "");
  assert.equal(f.elements.private_key.value, "");
  assert.equal(f.elements.username.value, "root");
  assert.equal(T.clearSecretFields(null).length, 0);

  const g = fakeForm({ address: "10.0.0.5", port: "2222", username: "root", env: "test", auth: "password", password: "p@ss", private_key: "" });
  const body = T.onboardPayload(g);
  assert.equal(g.elements.password.value, "", "读取后输入框立即清空");
  assert.equal(body.password, "p@ss");
  assert.equal(body.private_key, undefined);
  assert.equal(body.port, 2222);
  assert.equal(body.name, "10.0.0.5");
  assert.equal(T.validateOnboard(body), "");

  const k = fakeForm({ address: "10.0.0.6", port: "", username: "ops", auth: "key", password: "leftover", private_key: "KEY" });
  const kb = T.onboardPayload(k);
  assert.equal(kb.private_key, "KEY");
  assert.equal(kb.password, undefined);
  assert.equal(kb.port, 22);
  assert.equal(k.elements.password.value, "");
  assert.equal(k.elements.private_key.value, "");

  assert.match(T.validateOnboard({ address: "", port: 22, username: "a", password: "x" }), /IP/);
  assert.match(T.validateOnboard({ address: "1.1.1.1", port: 70000, username: "a", password: "x" }), /端口/);
  assert.match(T.validateOnboard({ address: "1.1.1.1", port: 22, username: "", password: "x" }), /账号/);
  assert.match(T.validateOnboard({ address: "1.1.1.1", port: 22, username: "a" }), /密码或私钥/);
});

test("错误信息中文化且不透出接口路径", () => {
  assert.equal(T.errText({ message: "permission denied" }), "没有执行该操作的权限");
  assert.equal(T.errText({ message: "outside change window" }), "当前不在变更窗口内，高风险操作暂不能执行");
  assert.equal(T.errText({ message: "boom", status: 404 }), "记录不存在或功能未开放");
  assert.ok(!T.errText({ message: "失败 /api/v1/aiops/cases/1" }).includes("/api/v1"));
});

test("策略归一化", () => {
  const p = T.normalizePolicy({ auto_execute: { enabled: 1, max_per_hour: "5" } });
  assert.equal(p.auto_execute.enabled, true);
  assert.equal(p.auto_execute.max_per_hour, 5);
  assert.equal(p.auto_investigate.enabled, false);
  assert.equal(p.auto_investigate.severities.length, 0);
  assert.equal(p.change_window.cron_like, "Mon-Fri 09:00-21:00");
  assert.equal(p.model_id, null);
});
