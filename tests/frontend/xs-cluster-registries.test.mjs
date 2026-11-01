import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../../public/xs-cluster-registries.js", import.meta.url), "utf8");
const window = {};
vm.runInNewContext(source, { window, URL });

test("registries use cluster Secret metadata and separate future K3s template", () => {
  assert.equal(typeof window.XSClusterRegistries.render, "function");
  assert.match(source, /\/config\/secrets/);
  assert.match(source, /kubernetes\.io\/dockerconfigjson/);
  assert.match(source, /\(s\.keys \|\| \[\]\)/);
  assert.match(source, /\/provision\/registry-mirror/);
  assert.match(source, /现有集群镜像加速状态未知/);
  assert.match(source, /config:write/);
});

test("mirror URL excludes credentials, query and fragments", () => {
  const valid = window.XSClusterRegistries.validEndpoint;
  assert.equal(valid("https://mirror.example.com/v2"), true);
  for (const url of ["ftp://mirror.example.com", "https://user:pass@mirror.example.com", "https://mirror.example.com?a=1", "https://mirror.example.com/#x", "", "https://mirror example.com"]) assert.equal(valid(url), false, url);
});
