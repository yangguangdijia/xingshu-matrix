import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { Element, harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (path) => fs.readFileSync(new URL(path, import.meta.url), "utf8");
const PLAIN = "xst_SuperSecretPlainToken0123456789abcd";
const XSS = `<img src=x onerror=alert(1)>"'&`;

function tokensHarness(extraApi) {
  let items = [{ id: 1, name: "ci-deploy", prefix: "xst_abc", scopes: ["cluster:read"], expires_at: null, last_used_at: null, status: "active" }];
  const h = harness("xs-identity.js", {
    api: async (path, opts = {}) => {
      if (extraApi) { const r = await extraApi(path, opts, items); if (r !== undefined) return r; }
      if (path === "/auth/tokens" && !opts.method) return items.slice();
      if (path === "/auth/tokens" && opts.method === "POST") {
        const t = { id: 2, name: opts.body.name, prefix: PLAIN.slice(0, 8), scopes: opts.body.scopes, expires_at: null, status: "active" };
        items = items.concat(t);
        return { ...t, token: PLAIN };
      }
      if (path.startsWith("/auth/tokens/") && opts.method === "DELETE") {
        const id = Number(path.split("/").pop());
        items = items.map((t) => (t.id === id ? { ...t, status: "revoked", revoked_at: "2026-01-01T00:00:00Z" } : t));
        return { id };
      }
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [], copied = [];
  Object.assign(h.window.XS, { toast: (m) => toasts.push(m), user: { permissions: ["cluster:read", "workload:read"] } });
  h.window.navigator = { clipboard: { writeText: async (t) => { copied.push(t); } } };
  return { ...h, toasts, copied };
}

const text = (node) => node.textContent + node.children.map(text).join("");

test("API routes match the backend registrations", () => {
  const src = read("../../public/xs-identity.js");
  const http = read("../../server/internal/auth/identity_http.go");
  const admin = read("../../server/internal/users/mfa_admin.go");
  for (const r of ["/auth/tokens", "/auth/tokens/:id", "/auth/mfa", "/auth/mfa/setup", "/auth/mfa/activate", "/auth/mfa/disable", "/auth/mfa/recovery-codes", "/auth/mfa/verify"]) {
    assert.ok(http.includes(`"${r}"`), r);
  }
  assert.ok(admin.includes('"/users/mfa-flags"') && admin.includes('"/users/:id/mfa"'));
  assert.match(src, /expires_in_days/);
  assert.match(src, /mfa_ticket/);
});

test("token plaintext is shown exactly once and is gone after closing", async () => {
  const h = tokensHarness();
  h.window.XSIdentity.renderTokens(h.el); await tick(); await tick();
  assert.ok(h.el.querySelector('[data-xid-token-row="1"]'));

  await h.emit("click", '[data-xid-click="create"]');
  const drawer = h.el.querySelector("[data-xid-drawer]");
  assert.ok(drawer, "create drawer opens");
  const opts = drawer.querySelectorAll("option").map((o) => o.value);
  assert.deepEqual(opts, ["7", "30", "90", "365", "0"]);
  h.el.querySelector('[name="name"]').value = "my-token";
  h.el.querySelector('[name="expires"]').value = "30";
  h.el.querySelector('[name="scope"]').checked = true;
  await h.emit("click", '[data-xid-click="submit"]'); await tick();

  const post = h.calls.find((c) => c.options.method === "POST");
  assert.deepEqual(JSON.parse(JSON.stringify(post.options.body)), { name: "my-token", scopes: ["cluster:read"], expires_in_days: 30 });
  const plainNode = h.el.querySelector("[data-xid-plain]");
  assert.equal(plainNode.textContent, PLAIN);
  assert.ok(text(h.el).includes("关闭后无法再次查看"));
  assert.equal(h.el.querySelectorAll("[data-xid-plain]").length, 1);
  // 列表区不出现明文
  assert.ok(!text(h.el.querySelector("[data-xid-main]")).includes(PLAIN));

  await h.emit("click", '[data-xid-click="copyPlain"]');
  assert.deepEqual(h.copied, [PLAIN]);

  // 点遮罩不能误关明文弹窗
  await h.emit("click", h.el.querySelector("[data-xid-mask]"));
  assert.ok(h.el.querySelector("[data-xid-plain]"));

  await h.emit("click", '[data-xid-click="closePlain"]'); await tick();
  assert.equal(h.el.querySelector("[data-xid-plain]"), null);
  assert.ok(!text(h.el).includes(PLAIN), "plaintext must not remain anywhere in the DOM");
  assert.ok(h.el.querySelector('[data-xid-token-row="2"]'), "list reloaded with the new token");
  // 关闭后复制按钮不再能拿到明文
  assert.equal(h.el.querySelector('[data-xid-click="copyPlain"]'), null);
  assert.ok(!h.toasts.some((m) => m.includes(PLAIN)));
});

test("revoking a token requires a second confirmation", async () => {
  const h = tokensHarness();
  h.window.XSIdentity.renderTokens(h.el); await tick(); await tick();
  await h.emit("click", '[data-xid-click="revoke"]');
  assert.equal(h.calls.filter((c) => c.options.method === "DELETE").length, 0, "no DELETE before confirming");
  assert.ok(text(h.el).includes("确定吊销令牌"));

  await h.emit("click", '[data-xid-click="close"]');
  assert.equal(h.el.querySelector("[data-xid-layer]"), null);
  assert.equal(h.calls.filter((c) => c.options.method === "DELETE").length, 0, "cancel does not revoke");

  await h.emit("click", '[data-xid-click="revoke"]');
  await h.emit("click", '[data-xid-click="confirmRevoke"]'); await tick();
  const del = h.calls.filter((c) => c.options.method === "DELETE");
  assert.equal(del.length, 1);
  assert.equal(del[0].path, "/auth/tokens/1");
  assert.equal(h.el.querySelector('[data-xid-click="revoke"]').disabled, true, "revoked token can't be revoked again");
  assert.ok(text(h.el).includes("已吊销"));
});

test("user-controlled values are escaped", async () => {
  const h = tokensHarness(async (path, opts) => {
    if (path === "/auth/tokens" && !opts.method) return [{ id: 9, name: XSS, prefix: XSS, scopes: [XSS], last_used_ip: XSS, last_used_at: "2026-01-01T00:00:00Z", status: "active" }];
  });
  h.window.XSIdentity.renderTokens(h.el); await tick(); await tick();
  const html = h.el.querySelector("[data-xid-main]").innerHTML;
  assert.ok(!html.includes("<img"), "no raw tag injected");
  assert.ok(html.includes("&lt;img src=x onerror=alert(1)&gt;&quot;&#39;&amp;"));
  assert.equal(h.el.querySelectorAll("img").length, 0);
  await h.emit("click", '[data-xid-click="revoke"]');
  assert.ok(!h.el.querySelector("[data-xid-layer]").innerHTML.includes("<img"));
  assert.equal(h.window.XSIdentity._esc(XSS), "&lt;img src=x onerror=alert(1)&gt;&quot;&#39;&amp;");
});

test("MFA setup falls back to secret/URI, activates, shows recovery codes once; disabling needs a code", async () => {
  let enabled = false;
  const uri = "otpauth://totp/Xingshu:alice?secret=JBSWY3DPEHPK3PXP&issuer=<b>";
  const h = harness("xs-identity.js", {
    api: async (path, opts = {}) => {
      if (path === "/auth/mfa") return { enabled, recovery_codes_left: enabled ? 10 : 0, required: false };
      if (path === "/auth/mfa/setup") return { secret: "JBSWY3DPEHPK3PXP", otpauth_uri: uri };
      if (path === "/auth/mfa/activate") { assert.equal(opts.body.code, "123456"); enabled = true; return { enabled: true, recovery_codes: ["AAAA-BBBB-CCCC-DDDD", "EEEE-FFFF-GGGG-HHHH"] }; }
      if (path === "/auth/mfa/disable") { assert.equal(opts.body.code, "654321"); enabled = false; return { enabled: false }; }
      throw new Error(path);
    }
  });
  h.window.XS.toast = () => {};
  const M = h.window.XSIdentity;
  M.renderMFA(h.el); await tick(); await tick();
  await h.emit("click", '[data-xid-click="setup"]'); await tick();
  assert.equal(h.el.querySelector("[data-xid-secret]").textContent, "JBSW Y3DP EHPK 3PXP");
  assert.equal(h.el.querySelector("[data-xid-uri]").textContent, uri);
  assert.ok(!h.el.querySelector("[data-xid-main]").innerHTML.includes("issuer=<b>") && h.el.querySelector("[data-xid-main]").innerHTML.includes("issuer=&lt;b&gt;"), "uri escaped");
  assert.ok(text(h.el).includes("手动添加"));

  h.el.querySelector('[name="code"]').value = "12ab";
  await h.emit("click", '[data-xid-click="activate"]');
  assert.ok(!h.calls.some((c) => c.path === "/auth/mfa/activate"), "invalid code not sent");
  h.el.querySelector('[name="code"]').value = "123456";
  await h.emit("click", '[data-xid-click="activate"]'); await tick();
  assert.equal(h.el.querySelector("[data-xid-codes]").querySelectorAll("span").length, 2);
  assert.ok(text(h.el).includes("仅显示这一次"));
  await h.emit("click", '[data-xid-click="closeCodes"]');
  assert.ok(!text(h.el).includes("AAAA-BBBB-CCCC-DDDD"));
  assert.ok(text(h.el).includes("已开启"));

  await h.emit("click", '[data-xid-click="askDisable"]');
  await h.emit("click", '[data-xid-click="disable"]');
  assert.ok(!h.calls.some((c) => c.path === "/auth/mfa/disable"), "empty code not sent");
  h.el.querySelector("[data-xid-layer]").querySelector('[name="code"]').value = "654321";
  await h.emit("click", '[data-xid-click="disable"]'); await tick(); await tick();
  assert.ok(text(h.el).includes("未开启"));
});

test("mfaChallenge verifies the ticket, stores the session and resolves", async () => {
  const h = harness("xs-identity.js", {
    api: async (path, opts) => {
      assert.equal(path, "/auth/mfa/verify");
      assert.equal(opts.noAuth, true);
      if (opts.body.code !== "111222") { const e = new Error("验证码错误"); e.status = 401; throw e; }
      return { token: { access_token: "a", refresh_token: "r" }, user: { id: 3, username: "bob" } };
    }
  });
  let session = null;
  h.window.XS.setSession = (pair, user) => { session = { pair, user }; };
  const body = new Element("body");
  h.window.document = { body, createElement: (t) => new Element(t) };
  const p = h.window.XSIdentity.mfaChallenge({ ticket: "tkt" });
  const box = body.querySelector("[data-xid-challenge]");
  const click = async (sel) => { for (const fn of box.listeners.click) await fn({ target: box.querySelector(sel) }); await tick(); };
  box.querySelector('[name="code"]').value = "000000";
  await click('[data-xid-click="verify"]');
  assert.equal(box.querySelector("[data-xid-err]").textContent, "验证码错误");
  box.querySelector('[name="code"]').value = "111222";
  await click('[data-xid-click="verify"]');
  const res = await p;
  assert.equal(res.user.username, "bob");
  assert.deepEqual(session.pair, { access_token: "a", refresh_token: "r" });
  assert.equal(body.querySelector("[data-xid-challenge]"), null);
});

test("admin helpers: mfa flags and reset", async () => {
  const h = harness("xs-identity.js", {
    api: async (path, opts = {}) => {
      if (path === "/users/mfa-flags") return Object.fromEntries(opts.query.ids.split(",").map((id) => [id, id === "2"]));
      if (path === "/users/5/mfa" && opts.method === "DELETE") return { id: 5, mfa_enabled: false };
      throw new Error(path);
    }
  });
  const M = h.window.XSIdentity;
  const flags = await M.userMfaBadges([1, 2, 2, "x", 0]);
  assert.deepEqual({ ...flags }, { 1: false, 2: true });
  assert.equal(h.calls[0].options.query.ids, "1,2");
  assert.deepEqual({ ...(await M.userMfaBadges([])) }, {});
  assert.deepEqual({ ...(await M.resetUserMFA(5)) }, { id: 5, mfa_enabled: false });
  await assert.rejects(M.resetUserMFA("abc"));
});
