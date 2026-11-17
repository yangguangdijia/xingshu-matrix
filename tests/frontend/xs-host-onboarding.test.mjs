import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";
import { Element, tick, deferred } from "./xs-resource-pages.harness.mjs";

const source = fs.readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");
const installCommand = "matrix-agent -server https://matrix.example.com -token mxe_testonly123";
const enrollment = { token: { id: 41, name: "接入令牌", env: "测试", token: "mxe_testonly123" }, install_command: installCommand };

function load({ permissions = ["host:read", "host:write"], api = async () => [] } = {}) {
  const body = new Element("body"), view = new Element("main"), modal = new Element("section");
  view.setAttribute("id", "view");
  modal.setAttribute("id", "modal");
  body.appendChild(view); body.appendChild(modal);
  const calls = [], copies = [], notices = [], titles = [];
  const document = {
    body,
    getElementById: (id) => body.querySelector('[id="' + id + '"]'),
    querySelector: (selector) => selector === 'button[form="liveForm"][type="submit"]'
      ? body.querySelectorAll("button").find((button) => button.getAttribute("form") === "liveForm" && button.type === "submit")
      : body.querySelector(selector)
  };
  const request = async (method, path, payload) => { calls.push({ method, path, payload }); return api(method, path, payload); };
  const window = { XS: {
    mode: "live", can: (permission) => permissions.includes(permission),
    get: (path, query) => request("GET", path, query), post: (path, payload) => request("POST", path, payload),
    isAuth: (error) => error?.status === 401
  } };
  const sandbox = {
    window, document, URL, console, setTimeout, clearTimeout,
    navigator: { clipboard: { writeText: async (text) => { copies.push(text); } } },
    toast: (text) => notices.push(text),
    dlg: (title, content, footer) => {
      titles.push(title); modal.hidden = false; modal.innerHTML = content + footer;
      for (const textarea of modal.querySelectorAll("textarea")) textarea.value = textarea.textContent;
    }
  };
  vm.runInNewContext(source.replace("  boot();", "  GLOBAL.__hostOnboarding = { render: renderHostsLive, open: openHostTokenForm, result: hostEnrollmentDialog };"), sandbox);
  return { ...sandbox.__hostOnboarding, pure: sandbox.XS_LIVE_CORE.pure, view, modal, document, window, calls, copies, notices, titles };
}

const event = () => ({ preventDefault() {}, stopPropagation() {} });

async function submit(harness) {
  harness.open();
  const form = harness.document.getElementById("liveForm");
  form.elements.name.value = " 接入令牌 ";
  form.elements.env.value = " 测试 ";
  form.onsubmit(event());
  await tick();
  return form;
}

test("主入口与空态直接提供添加主机，保留令牌管理和批量执行", async () => {
  const harness = load();
  harness.render(harness.view);
  await tick();
  const entries = harness.view.querySelectorAll('[data-act="host-add"]');
  assert.equal(entries.length, 2);
  assert.ok(harness.view.querySelector('[data-act="token-new"]'));
  assert.ok(harness.view.querySelector('[data-act="host-batch"]'));
  entries[0].onclick(event());
  assert.equal(harness.titles.at(-1), "添加主机 · 创建注册令牌");
  assert.match(harness.modal.innerHTML, /创建令牌不会添加主机/);
  assert.match(harness.modal.innerHTML, /创建令牌 → 准备代理程序 → 在目标主机执行 → 确认心跳/);
  assert.doesNotMatch(harness.modal.innerHTML, /role="tablist"/);
});

test("无主机写权限不展示添加或令牌入口，也不请求令牌", async () => {
  const harness = load({ permissions: ["host:read"] });
  harness.render(harness.view);
  await tick();
  assert.equal(harness.view.querySelector('[data-act="host-add"]'), null);
  assert.equal(harness.view.querySelector('[data-act="token-new"]'), null);
  assert.match(harness.view.innerHTML, /联系有主机管理权限的管理员/);
  assert.equal(harness.calls.some((call) => call.path === "/hosts/enroll-tokens"), false);
  harness.open();
  assert.equal(harness.titles.length, 0);
  assert.match(harness.notices.at(-1), /需要主机管理权限/);
});

test("服务端安装命令保留原令牌，只允许调整有效平台地址", () => {
  const helper = load().pure.hostInstallCommand;
  assert.equal(helper(installCommand).command, installCommand);
  assert.equal(helper(installCommand, "https://edge.example.com:8443/").command,
    "matrix-agent -server https://edge.example.com:8443 -token mxe_testonly123");
  assert.equal(helper(installCommand, "https://[2001:db8::1]:443").server, "https://[2001:db8::1]");
  for (const address of ["", "not a url", "http://edge.example.com", "https://user:pass@matrix.example.com", "https://matrix.example.com/api", "https://matrix.example.com?q=1", "https://matrix.example.com#x", "https://mat\nrix.example.com", "https://$(whoami).example.com"]) {
    assert.throws(() => helper(installCommand, address), undefined, address);
  }
  for (const command of ["", "mxe_testonly123", "matrix-agent -server https://matrix.example.com -token token;bad", "curl https://invented/install.sh"]) {
    assert.throws(() => helper(command), /接口未返回可用的安装命令/);
  }
});

test("令牌提交只调用真实注册接口，抑制重复提交并显示等待接入", async () => {
  const pending = deferred();
  const harness = load({ api: async () => pending.promise });
  harness.open();
  const form = harness.document.getElementById("liveForm");
  form.elements.env.value = "测试";
  form.onsubmit(event()); form.onsubmit(event());
  assert.equal(harness.calls.length, 1);
  assert.equal(harness.calls[0].path, "/hosts/enroll-tokens");
  assert.deepEqual(JSON.parse(JSON.stringify(harness.calls[0].payload)), { name: "", env: "测试", ttl_hours: 24 });
  pending.resolve(enrollment);
  await tick();
  assert.equal(harness.titles.at(-1), "添加主机 · 等待接入");
  assert.match(harness.modal.innerHTML, /令牌已创建，主机尚未确认/);
  assert.match(harness.modal.innerHTML, /当前接口不提供代理程序下载地址/);
  assert.match(harness.modal.innerHTML, /系统和 CPU 架构匹配/);
  assert.doesNotMatch(harness.modal.innerHTML, /curl|releases\/download|已成功添加主机/);
  harness.document.getElementById("liveCopyTok").onclick();
  await tick();
  assert.deepEqual(harness.copies, [installCommand]);
  const server = harness.document.getElementById("hostEnrollServer");
  server.value = "https://edge.example.com"; server.oninput();
  harness.document.getElementById("liveCopyTok").onclick();
  await tick();
  assert.equal(harness.copies.at(-1), installCommand.replace("matrix.example.com", "edge.example.com"));
  server.value = "http://edge.example.com"; server.oninput();
  assert.equal(harness.document.getElementById("liveCopyTok").disabled, true);
  assert.equal(harness.document.getElementById("hostEnrollCommand").value, "");
});

test("关闭或替换注册表单后，迟到结果不得重新展示令牌", async () => {
  for (const action of ["close", "replace", "error"]) {
    const pending = deferred();
    const harness = load({ api: async () => pending.promise });
    harness.open();
    const form = harness.document.getElementById("liveForm");
    form.elements.env.value = "测试";
    form.onsubmit(event());
    if (action === "close") harness.modal.hidden = true;
    else harness.open();
    if (action === "error") pending.reject(new Error("迟到错误"));
    else pending.resolve(enrollment);
    await tick();
    assert.equal(harness.document.getElementById("hostEnrollCommand"), null);
    assert.equal(harness.titles.includes("添加主机 · 等待接入"), false);
    if (action === "error") assert.doesNotMatch(harness.document.getElementById("liveErr").textContent, /迟到错误/);
  }
});

test("缺少安装命令不把令牌当命令复制", () => {
  const harness = load();
  harness.result({ token: enrollment.token });
  assert.equal(harness.document.getElementById("liveCopyTok").disabled, true);
  assert.match(harness.document.getElementById("hostEnrollError").textContent, /接口未返回可用的安装命令/);
  harness.document.getElementById("liveCopyTok").onclick();
  assert.deepEqual(harness.copies, []);
});

test("心跳检查只追踪令牌实际消费的主机，不误认其他在线主机", async () => {
  const harness = load({ api: async (_method, path) => {
    if (path === "/hosts/enroll-tokens") return [
      { id: 99, used_host_id: 999 }, { id: 41, used_host_id: 7 }
    ];
    if (path === "/hosts/7") return { id: 7, name: "实际主机", ip: "192.0.2.7", online: true, enrolled: true, last_seen_at: "2026-11-16T08:00:00Z" };
    throw new Error("不应读取其他主机");
  } });
  harness.result(enrollment);
  harness.document.getElementById("hostEnrollCheck").onclick();
  await tick();
  const status = harness.document.getElementById("hostEnrollStatus").textContent;
  assert.match(status, /已确认心跳.*主机 #7.*实际主机.*192.0.2.7/);
  assert.deepEqual(harness.calls.map((call) => call.path), ["/hosts/enroll-tokens", "/hosts/7"]);
});

test("未使用、过期、不可见及已注册离线状态均不报告心跳成功", async () => {
  for (const state of ["unused", "expired", "missing", "offline", "wrong-id", "denied"]) {
    const harness = load({ api: async (_method, path) => {
      if (state === "denied") throw new Error("无读取权限");
      if (path === "/hosts/enroll-tokens") {
        if (state === "missing") return [];
        return [{ id: 41, used_host_id: ["offline", "wrong-id"].includes(state) ? 7 : null, expires_at: state === "expired" ? "2000-01-01T00:00:00Z" : "2099-01-01T00:00:00Z" }];
      }
      return { id: state === "wrong-id" ? 8 : 7, online: false, enrolled: true };
    } });
    harness.result(enrollment);
    harness.document.getElementById("hostEnrollCheck").onclick();
    await tick();
    const status = harness.document.getElementById("hostEnrollStatus").textContent;
    assert.doesNotMatch(status, /已确认心跳/);
    const expected = { unused: /尚未收到/, expired: /已过期/, missing: /已删除或不可见/, offline: /尚未确认在线心跳/, "wrong-id": /身份信息不一致/, denied: /检查失败/ };
    assert.match(status, expected[state]);
    assert.equal(harness.document.getElementById("hostEnrollCheck").disabled, false);
  }
});

test("审批或创建失败保留表单，不展示伪成功接入引导", async () => {
  for (const error of [Object.assign(new Error("等待审批"), { code: 40304 }), new Error("无创建权限")]) {
    const harness = load({ api: async () => { throw error; } });
    await submit(harness);
    assert.equal(harness.document.getElementById("hostEnrollCommand"), null);
    assert.match(harness.document.getElementById("liveErr").textContent, /已提交审批|无创建权限/);
    assert.equal(harness.titles.length, 1);
  }
});

test("提交前再次检查写权限并验证有效期", () => {
  const harness = load();
  harness.open();
  const form = harness.document.getElementById("liveForm");
  form.elements.env.value = "测试";
  form.elements.ttl.value = "1.5";
  form.onsubmit(event());
  assert.match(harness.document.getElementById("liveErr").textContent, /整数/);
  form.elements.ttl.value = "24";
  harness.window.XS.can = () => false;
  form.onsubmit(event());
  assert.match(harness.document.getElementById("liveErr").textContent, /需要主机管理权限/);
  assert.equal(harness.calls.length, 0);
});
