import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-channel-form.js", import.meta.url), "utf8");
const window = {};
class MockFormData {
  constructor(form) { this.values = form.values; }
  get(name) { return this.values[name] ?? null; }
}
vm.runInNewContext(source, { window, FormData: MockFormData });
function form(values) { return { values }; }

test("channel form offers four types with type-specific fields", () => {
  assert.equal(typeof window.XSChannelForm.open, "function");
  assert.match(source, /type === "email"/);
  assert.doesNotMatch(source, /签名密钥/);
  assert.match(source, /Webhook 地址/);
  assert.match(source, /\/observability\/channels/);
  assert.match(source, /method: existing \? "PUT" : "POST"/);
});

test("channel edit omits blank secret and preserves filters", () => {
  const values = { name: "Ops", webhook: "", secret: "" };
  const payload = window.XSChannelForm.build("dingtalk", form(values), { enabled: false, severities: ["critical"], envs: ["prod"] });
  assert.equal(payload.config.webhook, undefined);
  assert.equal(payload.config.secret, undefined);
  assert.equal(payload.enabled, false);
  assert.deepEqual(Array.from(payload.envs), ["prod"]);
});

test("email uses only SMTP fields and validates recipients", () => {
  const values = { name: "Mail", host: "smtp.test", port: "465", username: "ops", password: "", from: "ops@example.com", to: "a@example.com, b@example.com" };
  const payload = window.XSChannelForm.build("email", form(values), null);
  assert.equal(payload.config.webhook, undefined);
  assert.equal(payload.config.password, undefined);
  assert.deepEqual(Array.from(payload.config.to), ["a@example.com", "b@example.com"]);
  assert.throws(() => window.XSChannelForm.build("email", form({ ...values, to: "" }), null), /收件人/);
});
