import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-platform-settings.js", import.meta.url), "utf8");

test("platform settings stays within platform APIs and excludes legacy integrations", () => {
  const window = {};
  vm.runInNewContext(source, { window });
  assert.equal(typeof window.XSPlatformSettings.render, "function");
  for (const path of ["/settings", "/platform/domain", "/env-policies", "/platform/notices"]) assert.ok(source.includes(path));
  for (const forbidden of ["scm.", "registry.url", "k8s.kubeconfig", "obs.prom", "/provision/registry-mirror", "新建设置"]) assert.ok(!source.includes(forbidden), forbidden);
  assert.match(source, /Velero/);
});

test("platform setting values are escaped before insertion", () => {
  assert.match(source, /replace\(\/\[&<>"'\]\/g/);
  assert.match(source, /Promise\.allSettled/);
  assert.match(source, /el\._xsPlatformSerial/);
});
