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

test("channel form offers robot, email, Slack and webhook types", () => {
  assert.equal(typeof window.XSChannelForm.open, "function");
  for (const t of ["dingtalk", "feishu", "wecom", "email", "slack", "webhook"]) assert.ok(JSON.stringify(window.XSChannelForm.types).includes(t), t);
  assert.match(source, /Webhook 地址/);
  assert.match(source, /\/observability\/channels/);
});

test("channel edit omits blank secret and preserves filters", () => {
  const v = { name: "Ops", type: "dingtalk", webhook: "", secret: "", enabled: false, severities: ["critical"], envs: ["prod"] };
  assert.equal(window.XSChannelForm.check(v, { id: 1 }), "");
  const payload = window.XSChannelForm.payload(v, { id: 1 });
  assert.equal(payload.config.webhook, undefined);
  assert.equal(payload.config.secret, undefined);
  assert.equal(payload.enabled, false);
  assert.deepEqual(Array.from(payload.envs), ["prod"]);
  assert.equal(payload.subscribe_alerts, undefined);
});

test("email uses only SMTP fields and validates recipients", () => {
  const v = { name: "Mail", type: "email", host: "smtp.test", port: "465", username: "ops", password: "", from: "ops@example.com", to: ["a@example.com", "b@example.com"] };
  assert.equal(window.XSChannelForm.check(v, null), "");
  const payload = window.XSChannelForm.payload(v, null);
  assert.equal(payload.config.webhook, undefined);
  assert.equal(payload.config.password, undefined);
  assert.deepEqual(Array.from(payload.config.to), ["a@example.com", "b@example.com"]);
  assert.match(window.XSChannelForm.check({ ...v, to: [] }, null), /收件人/);
});
