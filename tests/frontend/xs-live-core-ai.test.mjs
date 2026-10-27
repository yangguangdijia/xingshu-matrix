import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { pure } = require("../../public/xs-live-core.js");
const source = readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");

test("AI drafts keep only declared fields and strip nested credential keys", () => {
  const fields = {
    namespace: { type: "string", maxLength: 63 },
    name: { type: "string", maxLength: 63 },
    data: { type: "string_map" },
    secret_data: { type: "string_map" }
  };
  const draft = pure.sanitizeAIDraft({
    namespace: " prod ",
    name: " payment-auth ",
    data: { LOG_LEVEL: "info", DB_PASSWORD: "must-not-apply" },
    secret_data: { token: "must-not-apply" },
    submit: true
  }, fields);

  assert.deepEqual(JSON.parse(JSON.stringify(draft)), {
    namespace: "prod",
    name: "payment-auth",
    data: { LOG_LEVEL: "info" }
  });
});

test("AI drafts validate enums, booleans, and checkbox arrays", () => {
  const draft = pure.sanitizeAIDraft({
    type: "NodePort",
    reclaim_policy: "Delete",
    allow_volume_expansion: "true",
    access_modes: ["ReadWriteOnce", "AllAccess", "ReadWriteOnce"]
  }, {
    type: { type: "enum", values: ["ClusterIP", "NodePort"] },
    reclaim_policy: { type: "enum", values: ["Retain", "Delete"] },
    allow_volume_expansion: { type: "boolean" },
    access_modes: { type: "array", values: ["ReadWriteOnce", "ReadOnlyMany"] }
  });

  assert.deepEqual(JSON.parse(JSON.stringify(draft)), {
    type: "NodePort",
    reclaim_policy: "Delete",
    access_modes: ["ReadWriteOnce"]
  });
});

test("AI drafts preserve valid structured policy while dropping prototype and credential keys", () => {
  const spec = JSON.parse('{"podSelector":{"matchLabels":{"app":"api","__proto__":"bad"}},"policyTypes":["Ingress"],"ingress":[{"from":[{"podSelector":{"matchLabels":{"app":"web","password":"bad"}}}],"ports":[{"protocol":"TCP","port":8080}]}]}');
  const draft = pure.sanitizeAIDraft({ spec }, {
    spec: { type: "object" }
  });

  assert.deepEqual(JSON.parse(JSON.stringify(draft)), {
    spec: {
      podSelector: { matchLabels: { app: "api" } },
      policyTypes: ["Ingress"],
      ingress: [{
        from: [{ podSelector: { matchLabels: { app: "web" } } }],
        ports: [{ protocol: "TCP", port: 8080 }]
      }]
    }
  });
});

test("secret-capable forms reject likely literal credential values without rejecting metadata requests", () => {
  assert.equal(pure.aiPromptContainsSensitiveValue("创建名为 payment-auth 的 Opaque Secret，给 payment 服务使用"), false);
  assert.equal(pure.aiPromptContainsSensitiveValue("创建 Secret，password: Sup3rSecretValue!"), true);
  assert.equal(pure.aiPromptContainsSensitiveValue("把 token=ghp_12345678901234567890 加到资源描述"), true);
  assert.equal(pure.aiPromptContainsSensitiveValue("Authorization: Bearer abcdef0123456789"), true);
  assert.equal(pure.aiPromptContainsSensitiveValue("创建 ConfigMap，把 LOG_LEVEL 的值设为 info"), false);
  assert.equal(pure.aiPromptContainsSensitiveValue("创建 Secret，data value: Sup3rSecretValue!", true), true);
});

test("AI assistance only prefills a reviewed draft and leaves submission to the native form", () => {
  const aiBinding = source.slice(source.indexOf("function bindAIDraft("), source.indexOf("function resultDialog("));
  const formBinding = source.slice(source.indexOf("function formDialog("), source.indexOf("function aiDraftHtml("));

  assert.match(aiBinding, /api\.post\("\/ai\/assist"/);
  assert.match(aiBinding, /setAIDraftField\(form, key, draft\[key\]/);
  assert.match(aiBinding, /请检查后再单独提交/);
  assert.doesNotMatch(aiBinding, /requestSubmit|storageSubmit|onSubmit\(/);
  assert.match(formBinding, /f\.onsubmit\s*=\s*function/);
  assert.match(formBinding, /onSubmit\(f, bad\)/);
});

test("cluster pages have no horizontal resource tabs and keep node details in the shared drawer", () => {
  assert.doesNotMatch(source, /\bCLUSTER_TABS\b|\bclusterTabsHtml\b|\bdata-live-tab\b/);

  const nodeDetail = source.slice(source.indexOf("function clNodeDetail("), source.indexOf("function clNodesPage("));
  assert.match(nodeDetail, /\$id\("drawer"\)/);
  assert.match(nodeDetail, /classList\.add\("open"\)/);
  assert.match(nodeDetail, /class="x-dbody"/);
  assert.doesNotMatch(nodeDetail, /insertAdjacentHTML|insertBefore|scrollIntoView/);
});

test("live pages hide implementation labels and use the unified system settings name", () => {
  const shell = source.slice(source.indexOf("function shell("), source.indexOf("function loadingPanel("));
  const badge = source.slice(source.indexOf("function updateBadge("), source.indexOf("/* ---------------- boot"));
  const overview = source.slice(source.indexOf("function renderSreLive("), source.indexOf("function renderIamLive("));

  assert.doesNotMatch(shell, /实时数据|API base|matrix-server/);
  assert.doesNotMatch(badge, /实时数据|API base|matrix-server|\/api\/v1/);
  assert.doesNotMatch(overview, /系统接入|实时数据|API v1/);
  assert.match(overview, /系统设置/);
});

test("Service and exposure pages use separate resource routes from NetworkPolicy", () => {
  assert.deepEqual(pure.networkResourcePlan("network"), {
    title: "Service",
    endpoint: "services",
    resource: "Service"
  });
  assert.deepEqual(pure.networkResourcePlan("expose"), {
    title: "对外服务",
    endpoint: "ingresses",
    resource: "Ingress"
  });
  assert.equal(pure.clusterResourceHash("cluster", "network", "42"), "#/cluster/network/42");
  assert.equal(pure.clusterResourceHash("traffic", "expose", "42"), "#/traffic/expose/42");

  const networkPage = source.slice(source.indexOf("function clNetworkPage("), source.indexOf("function clConfigPage("));
  assert.match(networkPage, /apiGet\(base \+ plan\.endpoint\)/);
  assert.doesNotMatch(networkPage, /networkPolicyDialog|networkPolicyDelete|policies\//);
  assert.match(networkPage, /NetworkPolicy 的 Pod 通信规则/);
  const trafficRoute = source.slice(source.indexOf("function renderTrafficLiveRoute("), source.indexOf("function clListPage("));
  assert.match(trafficRoute, /route\.__xsLiveOrig\(el\)/);
});
