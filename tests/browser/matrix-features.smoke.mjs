import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../public/", import.meta.url));
const appHtml = fs.readFileSync(path.join(root, "index.html"), "utf8");
assert.doesNotMatch(appHtml, /Tokyo|demo-test|test-sh|prod-bj|staging-bj|dev-gz|prod-sh-01|sh-master-01|web-frontend-7d9f8b6c4/,
  "the product page must not ship preloaded test-cluster or workload state");
const chromePath = process.env.CHROME_PATH || "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const now = new Date().toISOString();
const commit = "0123456789abcdef0123456789abcdef01234567";
const user = { id: 1, user_id: 1, username: "smoke", display_name: "Smoke", roles: ["admin"], permissions: ["*"], env_scope: null };
let clusters = [
  { id: 1, name: "prod-cluster", env: "prod", status: "connected", api_server: "https://10.0.0.1:6443", version: "v1.31.4", updated_at: now },
  { id: 2, name: "dev-cluster", env: "dev", status: "connected", api_server: "https://10.0.0.2:6443", version: "v1.31.4", updated_at: now }
];
const pod = { name: "web-abc-123", namespace: "prod", uid: "pod-uid", status: "CrashLoopBackOff", node: "node-1", owner: "ReplicaSet/web-abc", labels: { app: "web" }, ready: "0/1", restarts: 5 };
const readyPods = [
  { name: "api-0", namespace: "prod", uid: "api-uid", phase: "Running", status: "Running", containers: ["api"], labels: { app: "api" }, ready: "1/1", restarts: 0 },
  { name: "orders-0", namespace: "prod", uid: "orders-uid", phase: "Running", status: "Running", containers: ["orders"], labels: { app: "orders" }, ready: "1/1", restarts: 0 },
  { name: "db-0", namespace: "prod", uid: "db-uid", phase: "Running", status: "Running", containers: ["db"], labels: { app: "db" }, ready: "1/1", restarts: 0 }
];
const node = { name: "node-1", internal_ip: "10.0.0.1", status: "Ready", ready: true, roles: ["control-plane"], version: "v1.31.4", cpu_capacity: "4", memory_capacity: "8Gi", labels: {}, usage: { cpu_percent: 15, memory_percent: 30 } };
const alert = { id: 42, datasource_id: 1, datasource: "native health", env: "prod", alertname: "CrashLoopBackOff", severity: "warning", status: "firing", handle_status: "open", fire_count: 1,
  starts_at: now, last_received_at: now, labels: { cluster_id: "1", cluster: "prod-cluster", namespace: "prod", pod: pod.name, uid: pod.uid, kind: "Pod" }, annotations: { summary: "Container restart loop", description: "Readiness failed" } };
let services = ["web", "api", "orders", "db"].map((name) => ({ namespace: "prod", name, type: "ClusterIP", uid: `service-${name}-uid`,
  resource_version: "4", cluster_ip: "10.43.0.1", ports: [{ port: 80, protocol: "TCP" }], selector: { app: name } }));
let clusterIngresses = [{ namespace: "prod", name: "web-ingress", uid: "ingress-uid", resource_version: "5",
  hosts: ["web.example"], class_name: "nginx", address_status: "pending", tls: [], load_balancer: [] }];
let networkPolicies = [
  { namespace: "prod", name: "web-to-api", uid: "policy-uid-1", resource_version: "7", labels: { team: "web" }, pod_selector: { matchLabels: { app: "api" } }, policy_types: ["Ingress"], ingress: [{ from: [{ podSelector: { matchLabels: { app: "web" } } }] }] },
  { namespace: "prod", name: "orders-to-db", uid: "policy-uid-2", resource_version: "3", labels: { team: "orders" }, pod_selector: { matchLabels: { app: "orders" } }, policy_types: ["Egress"], egress: [{ to: [{ podSelector: { matchLabels: { app: "db" } } }] }] }
];
const argo = { id: 3, name: "existing-argo", url: "https://argocd.example", env: "prod", project: "team", token: "********" };
const app = { id: 1, name: "web", repo_url: "https://git.example/web.git", image_repo: "registry.example/web", branch: "main", envs: [{ name: "prod", target_id: 1, namespace: "prod", deployment: "web", container: "app" }] };
const logRows = [
  { ts: now, message: JSON.stringify({ level: "ERROR", message: "readiness failed", app: "web" }), labels: { namespace: "prod", app: "web", container: "web", pod: "web-0" } },
  { ts: now, message: JSON.stringify({ level: "WARN", message: "slow query", app: "web" }), labels: { namespace: "prod", app: "web", container: "web", pod: "web-1" } },
  { ts: now, message: JSON.stringify({ level: "ERROR", message: "backend timeout", app: "worker" }), labels: { namespace: "prod", app: "worker", container: "worker", pod: 'api"\\worker' } }
];
let diagnosis = { id: 7, alert_id: 42, occurrence: 1, status: "ready", analysis: { impact_scope: "workload", impact_summary: "Application availability affected", probable_causes: "Configuration needs inspection", recommendation: "Controlled rolling restart after inspection", risk: "Temporary application interruption", proposed_action: "restart_workload", confidence: "medium", verification_steps: "Verify all replicas and service probes" },
  target: { cluster_id: 1, cluster_name: "prod-cluster", namespace: "prod", name: "web", kind: "deployments", uid: "deployment-uid", generation: 2, replicas: 2, ready: 1 } };
let createdAlertRules = [];
let job = { id: 9, name: "new-cluster", env: "test", status: "queued", plan: { hosts: [] }, logs: [], error: "", created_at: now };
let pending = false;
const writes = [];
const reads = [];
const aiChats = [];
let aiChatFailure = false;
let aiModelsAvailable = true;
let traceSamplesAvailable = true;
const diagnosisHistory = [{ id: 7, alert_id: 42, occurrence: 1, status: "ready", alert_name: "CrashLoopBackOff", environment: "prod",
  severity: "warning", alert_status: "firing", impact_summary: "Application availability affected", recommendation: "Inspect rollout before restart",
  risk: "Temporary application interruption", decision_name: "", error: "", created_at: now, updated_at: now }];
let bastionAssets = [{ id: 21, name: "web-01", host: "10.0.0.11", port: 22, protocol: "ssh", env: "prod" }];
let bastionAccounts = [{ id: 31, asset_id: 21, name: "root@web-01", username: "root", auth_type: "password", secret: "******", updated_at: now }];
let bastionRules = [];
let bastionFilters = [];
const managedHosts = [
  { id: 101, name: "prod-agent-01", ip: "10.0.0.21", env: "prod-east", online: true },
  { id: 102, name: "offline-agent", ip: "10.0.0.22", env: "dev", online: false },
  { id: 103, name: "dev-agent-01", ip: "10.0.0.23", env: "dev", online: true }
];
let managedTasks = [];
let registryMirror = { provider: "dockerhub", endpoint: "" };
let platformEnv = {
  deploy: "kubernetes", cluster: "prod-cluster", namespace: "matrix-system", nodes: 2,
  cpu: { used: 1.5, total: 6, unit: "核" }, memory: { used: 2, total: 12, unit: "GiB" },
  ephemeral_storage: { used: 5, total: 100, unit: "GiB" }, storage_class: "fast",
  storage_classes: ["fast", "shared"], ingress_available: true, metrics_available: true
};
let automationWorkflows = [];
const automationRuns = [];
const bastionSessions = [
  { id: 71, user_id: 1, username: "smoke", asset_id: 21, asset_name: "web-01", account_user: "root", protocol: "ssh", client_ip: "192.0.2.4",
    status: "closed", started_at: now, ended_at: now, has_recording: false },
  { id: 72, user_id: 1, username: "smoke", asset_id: 21, asset_name: "web-01", account_user: "deploy", protocol: "ssh", client_ip: "192.0.2.5",
    status: "active", started_at: now, ended_at: null, has_recording: false }
];
const bastionCommands = [{ id: 91, session_id: 71, command: "systemctl status nginx", action: "allow", created_at: now }];
let accessApprovals = [];
const ingress = { apiVersion: "networking.k8s.io/v1", kind: "Ingress", metadata: { name: "web", namespace: "prod", uid: "ing-uid", resourceVersion: "1" },
  spec: { ingressClassName: "nginx", rules: [{ host: "web.example", http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "web", port: { number: 80 } } } }] } }] } };
let snapshotSupported = false;
let storagePending = false;
let storageClasses = [
  { name: "fast-ssd", uid: "sc-uid", resource_version: "11", provisioner: "csi.example.io", reclaim_policy: "Delete",
    volume_binding_mode: "WaitForFirstConsumer", allow_volume_expansion: true, default: true, parameters: { type: "gp3" },
    mount_options: [], created_at: now }
];
let storageVolumes = [
  { name: "pv-smoke", uid: "pv-uid", resource_version: "21", phase: "Bound", capacity: "20Gi", access_modes: ["ReadWriteOnce"],
    storage_class: "fast-ssd", claim_ref: "prod/data", reclaim_policy: "Delete", volume_mode: "Filesystem",
    host_path: "/mnt/data", labels: {}, created_at: now }
];
let storageClaims = [
  { namespace: "prod", name: "data", uid: "pvc-uid", resource_version: "31", phase: "Bound", requested: "20Gi", capacity: "20Gi",
    access_modes: ["ReadWriteOnce"], storage_class: "fast-ssd", volume_name: "pv-smoke", volume_mode: "Filesystem",
    labels: { app: "web" }, created_at: now }
];
let storageSnapshots = [];
let hpas = [{ namespace: "prod", name: "web-hpa", uid: "hpa-uid", resource_version: "61", labels: { app: "web" },
  spec: { scaleTargetRef: { apiVersion: "apps/v1", kind: "Deployment", name: "web" }, minReplicas: 1, maxReplicas: 4,
    metrics: [{ type: "Resource", resource: { name: "cpu", target: { type: "Utilization", averageUtilization: 70 } } }] },
  status: { currentReplicas: 2, desiredReplicas: 2 }, created_at: now }];
const cronJobs = [{ namespace: "prod", name: "nightly-cleanup", uid: "cron-uid", resource_version: "71", labels: { app: "maintenance" },
  spec: { schedule: "0 2 * * *", suspend: false, jobTemplate: { spec: { template: { spec: { restartPolicy: "Never",
    containers: [{ name: "cleanup", image: "busybox:1.36", command: ["sh", "-c", "echo done"] }] } } } } },
  status: { active: [], lastScheduleTime: now }, created_at: now }];
const cronJobRuns = [{ name: "nightly-cleanup-29010000", uid: "job-uid", status: "Succeeded", active: 0, succeeded: 1,
  failed: 0, start_time: now, completion_time: now, created_at: now }];
const snapshotCapability = () => snapshotSupported
  ? { supported: true, reason: "", classes: ["fast-snap"] }
  : { supported: false, reason: "集群未安装 snapshot.storage.k8s.io/v1 CRD", classes: [] };

async function api(req, res, u) {
  const p = u.pathname.replace(/^\/api\/v1/, ""), method = req.method;
  let body = "";
  for await (const chunk of req) body += chunk;
  let input = {};
  if (body) input = JSON.parse(body);
  const reply = (data, status = 200, code = 0) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify({ code, data, message: code ? "approval pending" : "ok" })); };
  if (method !== "GET") writes.push({ path: p, search: u.search, method, body: input });
  else reads.push({ path: p, search: u.search });
  if (p === "/health") return reply({ ok: true });
  if (p === "/auth/me") return reply(user);
  if (p === "/settings" && method === "GET") return reply({ items: [
    { key: "notify.webhook", value: "https://hooks.example/legacy-secret", description: "legacy notification config", secret: true, has_value: true, updated_at: now },
    { key: "ai.features", value: { logs: false, alert: true }, description: "AI 功能开关", secret: false, updated_at: now },
    { key: "public.notices", value: [], description: "平台公告（总览页展示）", secret: false, updated_at: now },
    { key: "platform.name", value: "Matrix", description: "name", secret: false, updated_at: now },
    { key: "scm.type", value: "GitLab", secret: false, updated_at: now },
    { key: "scm.url", value: "https://git.example.com", secret: false, updated_at: now },
    { key: "scm.token", value: "******", secret: true, has_value: true, updated_at: now },
    { key: "registry.url", value: "https://harbor.example.com", secret: false, updated_at: now },
    { key: "registry.user", value: "robot", secret: false, updated_at: now },
    { key: "registry.pass", value: "******", secret: true, has_value: true, updated_at: now },
    { key: "k8s.name", value: "prod-cluster", secret: false, updated_at: now },
    { key: "k8s.api", value: "https://10.0.0.1:6443", secret: false, updated_at: now },
    { key: "k8s.kubeconfig", value: "******", secret: true, has_value: true, updated_at: now },
    { key: "obs.prom", value: "https://prometheus.example.com", secret: false, updated_at: now },
    { key: "obs.log", value: "https://loki.example.com", secret: false, updated_at: now }
  ] });
  if (p === "/settings/public") return reply({ "public.notices": [] });
  if (p === "/provision/registry-mirror" && method === "GET") return reply(registryMirror);
  if (p === "/provision/registry-mirror" && method === "PUT") {
    registryMirror = input;
    return reply(registryMirror);
  }
  if (p === "/automation/ansible/runtime") return reply({ available: true, version: "ansible-playbook [core 2.21.5]", controller: "platform" });
  if (p === "/automation/workflows" && method === "GET") return reply({ items: automationWorkflows, total: automationWorkflows.length });
  if (p === "/automation/workflows" && method === "POST") {
    const created = { id: 77, name: input.name, description: input.description, trigger: input.trigger, status: "active", current_version: 1, updated_at: now };
    automationWorkflows = [created];
    created.steps = input.steps || [];
    created.playbook = input.playbook || "";
    return reply({ id: created.id, version: 1 }, 201);
  }
  if (p === "/automation/workflows/77" && method === "GET") return reply(automationWorkflows[0]);
  if (p === "/automation/runs" && method === "GET") return reply({ items: automationRuns, total: automationRuns.length });
  if (p === "/automation/ansible/preflight" && method === "POST") return reply({
    items: [{ ip: "10.2.0.5", port: 22, fingerprint: "SHA256:verified-by-server" }],
    confirmation_token: "opaque-one-use-confirmation", expires_in_seconds: 600
  });
  if (p === "/automation/workflows/77/trigger" && method === "POST") {
    automationRuns.unshift({ id: 88, workflow_id: 77, version: 1, status: "queued", triggered_by: "smoke", engine: "ansible", env: input.env });
    return reply({ id: 88, status: "queued" }, 201);
  }
  if (p === "/platform/domain") return reply({ ingress_available: true, ingress_class: "nginx" });
  if (p === "/platform/env") return reply(platformEnv);
  if (p === "/hosts" && method === "GET") {
    const env = u.searchParams.get("env") || "", keyword = (u.searchParams.get("keyword") || "").toLowerCase();
    const items = managedHosts.filter((h) => (!env || h.env === env) && (!keyword || `${h.name} ${h.ip}`.toLowerCase().includes(keyword)));
    return reply({ items, total: items.length });
  }
  if (p === "/hosts/batch-exec" && method === "POST") {
    managedTasks = input.host_ids.map((host_id, i) => ({ id: 200 + i, host_id, kind: "exec", batch_id: "batch-smoke", command: input.command,
      timeout_sec: input.timeout_sec, status: "success", exit_code: 0, output: "up 4 days", error: "" }));
    return reply({ batch_id: "batch-smoke", tasks: managedTasks, pending_approvals: [] }, 202);
  }
  if (p === "/hosts/tasks" && method === "GET") return reply({ items: managedTasks, total: managedTasks.length });
  if (p === "/bastion/requestable-assets" && method === "GET") return reply([
    { id: 21, name: "web-01", host: "10.0.0.11", env: "prod", accounts: [{ id: 31, name: "root@web-01", username: "root" }] }
  ]);
  if (p === "/bastion/access-requests" && method === "POST") {
    const ticket = { id: 81, type: "bastion.access_request", title: "SSH 临时访问：web-01（30 分钟）", env: "prod",
      reason: input.reason, applicant_name: "smoke", status: "pending", created_at: now, result: null };
    accessApprovals.unshift(ticket);
    return reply(ticket, 201);
  }
  if (p === "/approvals" && method === "GET") {
    const items = u.searchParams.get("type") === "bastion.access_request" ? accessApprovals : [];
    return reply({ items, total: items.length });
  }
  if (p === "/bastion/assets" && method === "GET") return reply({ items: bastionAssets, total: bastionAssets.length });
  if (p === "/bastion/accounts" && method === "GET") return reply(bastionAccounts);
  if (p === "/bastion/accounts" && method === "POST") {
    const created = { id: 32, ...input, secret: "******", updated_at: now };
    delete created.passphrase;
    bastionAccounts.push(created);
    return reply(created, 201);
  }
  if (/^\/bastion\/accounts\/\d+$/.test(p)) {
    const id = Number(p.split("/").pop()), i = bastionAccounts.findIndex((x) => x.id === id);
    if (method === "PUT" && i >= 0) {
      bastionAccounts[i] = { ...bastionAccounts[i], ...input, secret: input.secret ? "******" : bastionAccounts[i].secret, updated_at: now };
      delete bastionAccounts[i].passphrase;
      return reply(bastionAccounts[i]);
    }
    if (method === "DELETE" && i >= 0) { bastionAccounts.splice(i, 1); return reply({ id }); }
  }
  if (p === "/bastion/rules" && method === "GET") return reply(bastionRules);
  if (p === "/bastion/rules" && method === "POST") { const row = { id: 41 + bastionRules.length, ...input }; bastionRules.unshift(row); return reply(row, 201); }
  if (/^\/bastion\/rules\/\d+$/.test(p)) {
    const id = Number(p.split("/").pop()), i = bastionRules.findIndex((x) => x.id === id);
    if (method === "PUT" && i >= 0) { bastionRules[i] = { ...bastionRules[i], ...input }; return reply(bastionRules[i]); }
    if (method === "DELETE" && i >= 0) { bastionRules.splice(i, 1); return reply({ id }); }
  }
  if (p === "/bastion/filters" && method === "GET") return reply(bastionFilters);
  if (p === "/bastion/filters" && method === "POST") { const row = { id: 51 + bastionFilters.length, ...input }; bastionFilters.unshift(row); return reply(row, 201); }
  if (/^\/bastion\/filters\/\d+$/.test(p)) {
    const id = Number(p.split("/").pop()), i = bastionFilters.findIndex((x) => x.id === id);
    if (method === "PUT" && i >= 0) { bastionFilters[i] = { ...bastionFilters[i], ...input }; return reply(bastionFilters[i]); }
    if (method === "DELETE" && i >= 0) { bastionFilters.splice(i, 1); return reply({ id }); }
  }
  if (p === "/bastion/sessions" && method === "GET") return reply({ items: bastionSessions, total: bastionSessions.length });
  if (p === "/bastion/sessions/71" && method === "GET") return reply({ session: bastionSessions[0], commands: bastionCommands });
  if (/^\/bastion\/sessions\/\d+\/kill$/.test(p) && method === "POST") {
    const id = Number(p.split("/")[3]), session = bastionSessions.find((x) => x.id === id);
    if (session) session.status = "closed";
    return reply({ id, killed: !!session });
  }
  if (p === "/observability/aiops/config") return reply({ enabled: false, auto_diagnose: false, interval_sec: 60, model_id: 0, public_url: "" });
  if (p === "/observability/diagnoses") return reply({ items: diagnosisHistory, total: diagnosisHistory.length, page: +(u.searchParams.get("page") || 1), page_size: +(u.searchParams.get("page_size") || 20) });
  if (p === "/observability/alerts/42/diagnosis") return reply(diagnosis);
  if (p === "/observability/alerts/42/decision") {
    diagnosis = { ...diagnosis, status: input.decision === "manual" ? "manual" : "awaiting_approval", decision_by: 1, decision_name: "smoke", approval_id: input.decision === "ai" ? 10 : null };
    return input.decision === "ai" ? reply({ ticket: { id: 10 } }, 202, 40304) : reply({ status: "manual" });
  }
  if (p === "/observability/alerts/42/diagnose") { diagnosis.status = "queued"; return reply(diagnosis); }
  if (p === "/observability/alerts/42") return reply(alert);
  if (p === "/observability/alerts") return reply({ items: [alert], total: 1 });
  if (p === "/observability/datasources" || p === "/observability/data-sources") return reply(u.searchParams.get("type") === "prometheus" ? [] : [
    { id: 1, name: "native health", type: "kubernetes", env: "prod", options: { cluster_id: "1", managed_by: "aiops" } },
    { id: 2, name: "traces", type: "jaeger", env: "prod", options: { cluster_id: "1" } },
    { id: 3, name: "logs", type: "loki", env: "prod", options: { cluster_id: "1" } }
  ]);
  if (p === "/observability/traces/services") return reply({ items: ["web", "api", "orders"] });
  if (p === "/observability/traces/search") return reply({ items: traceSamplesAvailable ? [{ trace_id: "trace-1" }] : [] });
  if (p === "/observability/traces/trace-1") return reply({ trace_id: "trace-1", spans: [
    { span_id: "a", parent_id: "", service: "web" }, { span_id: "b", parent_id: "a", service: "api" }, { span_id: "c", parent_id: "b", service: "orders" }
  ] });
  if (p === "/observability/logs/query") return reply({ rows: logRows });
  if (p.startsWith("/observability/logs")) return reply({ rows: logRows });
  if (p === "/clusters/contexts" && method === "POST") return reply({ items: [
    { name: "prod-admin", cluster: "prod", namespace: "prod", server: "https://10.0.0.1:6443", current: true },
    { name: "dev-admin", cluster: "dev", namespace: "dev", server: "https://10.0.0.2:6443", current: false },
    { name: "offline-admin", cluster: "offline", namespace: "default", server: "https://10.0.0.3:6443", current: false }
  ], total: 3 });
  if (p === "/clusters/test" && method === "POST") return input.context === "offline-admin"
    ? reply({ ok: false, error: "连接被拒绝" })
    : reply({ ok: true, version: "v1.31.4", api_server: "https://10.0.0.1:6443" });
  if (p === "/clusters/import" && method === "POST") {
    const results = input.items.map((item, index) => ({
      context: item.context, name: item.name,
      status: item.context === "prod-admin" ? "connected" : "failed",
      ...(item.context === "prod-admin"
        ? { id: 10 + clusters.length + index, version: "v1.31.4" }
        : { error: "模拟导入失败" })
    }));
    clusters = [...clusters, ...results.filter((item) => item.status === "connected").map((item) => ({
      id: item.id, name: item.name, env: input.items.find((source) => source.name === item.name)?.env || "",
      status: "connected", api_server: "https://imported.example:6443", version: item.version, updated_at: now
    }))];
    return reply({ created: results.filter((item) => item.status === "connected").length,
      failed: results.filter((item) => item.status !== "connected").length, total: results.length, items: results }, 201);
  }
  if (p === "/clusters" && method === "POST") {
    const created = { id: 10 + clusters.length, name: input.name, env: input.env, status: "connected", api_server: `https://${input.context}.example:6443`, version: "v1.31.4", updated_at: now };
    clusters = [...clusters, created];
    return reply(created, 201);
  }
  if (p === "/clusters" && method === "GET") return reply({ items: clusters, total: clusters.length });
  if (/^\/clusters\/\d+\/alert-rules$/.test(p) && method === "GET") return reply({ items: createdAlertRules, total: createdAlertRules.length });
  if (/^\/clusters\/\d+\/alert-rules$/.test(p) && method === "POST") {
    const created = { ...input, source: "matrix-matrix-ui" };
    createdAlertRules.push(created);
    return reply({ prometheus_rule: created.source, namespace: input.namespace }, 201);
  }
  if (/^\/clusters\/\d+\/hpa$/.test(p)) {
    if (method === "GET") return reply({ items: hpas, total: hpas.length });
    if (method === "POST") {
      const created = { ...input, uid: "hpa-created-uid", resource_version: "62", status: {}, created_at: now };
      hpas = [...hpas, created];
      return reply(created, 201);
    }
  }
  if (/^\/clusters\/\d+\/cronjobs$/.test(p)) {
    if (method === "GET") return reply({ items: cronJobs, total: cronJobs.length });
    if (method === "POST") {
      const created = { ...input, uid: "cron-created-uid", resource_version: "72", status: {}, created_at: now };
      cronJobs.push(created);
      return reply(created, 201);
    }
  }
  const cronHistoryMatch = p.match(/^\/clusters\/\d+\/cronjobs\/([^/]+)\/([^/]+)\/jobs$/);
  if (cronHistoryMatch && method === "GET") return reply({ items: cronJobRuns, total: cronJobRuns.length });
  if (/\/network\/probe$/.test(p)) return reply({ reachable: true, message: "源容器的 TCP 连接成功", duration_ms: 12, source: "prod/api-0/api", host: input.host, port: input.port });
  if (/^\/clusters\/\d+\/nodes\/[^/]+$/.test(p)) return reply({ node, metrics_available: true });
  if (/^\/clusters\/\d+\/nodes$/.test(p)) return reply({ items: [node] });
  if (/^\/clusters\/\d+\/node-groups$/.test(p)) return reply({ label: "node.xingshu.com/pool", note: "按节点标签分组；未接入云厂商节点池扩缩容。", items: [
    { name: "system", label_value: "system", nodes: [node.name], ready: 1, unschedulable: 0, roles: ["control-plane"],
      taints: ["node-role.kubernetes.io/control-plane:NoSchedule"] }
  ] });
  if (/^\/clusters\/\d+\/node-batch-labels$/.test(p)) return reply({ updated: input.nodes, labels: input.labels });
  if (/^\/clusters\/\d+\/node-batch-taints$/.test(p)) return reply({ updated: input.items });
  if (/^\/clusters\/\d+\/namespaces$/.test(p)) return reply({ items: [{ name: "prod", status: "Active", labels: {} }, { name: "default", status: "Active", labels: {} }] });
  if (/^\/clusters\/\d+\/storage\/capabilities$/.test(p)) return reply({ snapshots: snapshotCapability() });
  if (/^\/clusters\/\d+\/storage\/classes$/.test(p)) {
    if (method === "GET") return reply({ items: storageClasses, total: storageClasses.length });
    if (method === "POST") {
      if (storagePending) return reply({ ticket: { id: 91 } }, 202, 40304);
      const created = { ...input, uid: "sc-new-uid", resource_version: "41", default: !!input.is_default, created_at: now };
      storageClasses = [...storageClasses, created];
      return reply(created, 201);
    }
  }
  if (/^\/clusters\/\d+\/storage\/classes\/[^/]+$/.test(p)) {
    const name = decodeURIComponent(p.split("/").pop());
    const i = storageClasses.findIndex((x) => x.name === name);
    if (method === "PUT" && i >= 0) {
      storageClasses[i] = { ...storageClasses[i], ...input, name, resource_version: "12", default: !!input.is_default };
      return reply(storageClasses[i]);
    }
    if (method === "DELETE") {
      storageClasses = storageClasses.filter((x) => x.name !== name);
      return reply({ name, deleted: true });
    }
  }
  if (/^\/clusters\/\d+\/storage\/volumes$/.test(p)) {
    if (method === "GET") return reply({ items: storageVolumes, total: storageVolumes.length });
    if (method === "POST") {
      const created = { ...input, uid: "pv-new-uid", resource_version: "41", phase: "Available", claim_ref: "", created_at: now };
      storageVolumes = [...storageVolumes, created];
      return reply(created, 201);
    }
  }
  if (/^\/clusters\/\d+\/storage\/volumes\/[^/]+$/.test(p)) {
    const name = decodeURIComponent(p.split("/").pop());
    const i = storageVolumes.findIndex((x) => x.name === name);
    if (method === "PUT" && i >= 0) {
      storageVolumes[i] = { ...storageVolumes[i], ...input, name, resource_version: "22" };
      return reply(storageVolumes[i]);
    }
    if (method === "DELETE") {
      storageVolumes = storageVolumes.filter((x) => x.name !== name);
      return reply({ name, deleted: true });
    }
  }
  if (/^\/clusters\/\d+\/storage\/claims$/.test(p)) {
    if (method === "GET") return reply({ items: storageClaims, total: storageClaims.length });
    if (method === "POST") {
      const created = { ...input, uid: "pvc-new-uid", resource_version: "41", phase: "Pending", requested: input.size, capacity: "", volume_name: "", created_at: now };
      storageClaims = [...storageClaims, created];
      return reply(created, 201);
    }
  }
  if (/^\/clusters\/\d+\/storage\/claims\/[^/]+\/[^/]+$/.test(p)) {
    const parts = p.split("/");
    const namespace = decodeURIComponent(parts[5]), name = decodeURIComponent(parts[6]);
    const i = storageClaims.findIndex((x) => x.namespace === namespace && x.name === name);
    if (method === "PUT" && i >= 0) {
      storageClaims[i] = { ...storageClaims[i], ...input, namespace, name, requested: input.size, resource_version: "32" };
      return reply(storageClaims[i]);
    }
    if (method === "DELETE") {
      storageClaims = storageClaims.filter((x) => !(x.namespace === namespace && x.name === name));
      return reply({ namespace, name, deleted: true });
    }
  }
  if (/^\/clusters\/\d+\/storage\/snapshots$/.test(p)) {
    if (method === "GET") return reply({ items: storageSnapshots, total: storageSnapshots.length, capability: snapshotCapability() });
    if (method === "POST") {
      if (!snapshotSupported) return reply({ message: "VolumeSnapshot is unavailable" }, 409, 40901);
      const created = { namespace: input.namespace, name: input.name, uid: "snap-uid-1", resource_version: "51",
        class: input.class_name || "", source_pvc: input.source_pvc, ready: false, restore_size: "", labels: input.labels || {}, created_at: now };
      storageSnapshots = [...storageSnapshots, created];
      return reply(created, 201);
    }
  }
  if (/^\/clusters\/\d+\/storage\/snapshots\/[^/]+\/[^/]+$/.test(p)) {
    const parts = p.split("/");
    const namespace = decodeURIComponent(parts[5]), name = decodeURIComponent(parts[6]);
    if (method === "DELETE") {
      storageSnapshots = storageSnapshots.filter((x) => !(x.namespace === namespace && x.name === name));
      return reply({ namespace, name, deleted: true });
    }
  }
  if (/^\/clusters\/\d+\/config\/configmaps$/.test(p)) return reply({ items: [{ namespace: "prod", name: "web-config", uid: "cm-uid",
    resource_version: "8", keys: ["LOG_LEVEL"], data: { LOG_LEVEL: "info" }, immutable: false }] });
  if (/^\/clusters\/\d+\/config\/secrets$/.test(p)) return reply({ items: [{ namespace: "prod", name: "registry-auth", uid: "secret-uid",
    resource_version: "9", type: "kubernetes.io/dockerconfigjson", keys: [".dockerconfigjson"], immutable: false }] });
  if (/\/workloads\/pods$/.test(p)) return reply({ items: [pod, ...readyPods] });
  if (/\/workloads\/deployments$/.test(p)) return reply({ items: [{ name: "web", namespace: "prod", uid: "deployment-uid", replicas: 2, ready: "1/2", ready_replicas: 1, status: "Progressing", containers: [{ name: "app", image: "web:1" }] }] });
  if (/\/network\/services$/.test(p)) {
    if (method === "POST") {
      const created = { ...input, uid: "service-new-uid", resource_version: "10", cluster_ip: "10.43.0.99", load_balancer: [] };
      services = [...services, created];
      return reply(created, 201);
    }
    return reply({ items: services });
  }
  if (/\/network\/ingresses$/.test(p)) {
    if (method === "POST") {
      const created = { ...input, uid: "ingress-new-uid", resource_version: "10",
        hosts: input.rules.map((rule) => rule.host).filter(Boolean), address_status: "pending", load_balancer: [] };
      clusterIngresses = [...clusterIngresses, created];
      return reply(created, 201);
    }
    return reply({ items: clusterIngresses });
  }
  if (/\/network\/policies$/.test(p)) {
    if (method === "POST") {
      const name = decodeURIComponent(u.searchParams.get("name") || input.name || "smoke-policy");
      const parts = p.split("/");
      const created = { namespace: "prod", name, uid: "policy-uid-new", resource_version: "8", labels: input.labels || {},
        pod_selector: input.spec.podSelector || {}, policy_types: input.spec.policyTypes || [], ingress: input.spec.ingress || [], egress: input.spec.egress || [] };
      networkPolicies.push(created);
      return reply(created, 201);
    }
    return reply({ items: networkPolicies });
  }
  const policyMatch = p.match(/\/network\/policies\/([^/]+)\/([^/]+)$/);
  if (policyMatch) {
    const namespace = decodeURIComponent(policyMatch[1]), name = decodeURIComponent(policyMatch[2]);
    const index = networkPolicies.findIndex((policy) => policy.namespace === namespace && policy.name === name);
    if (method === "POST" && index < 0) {
      const created = { namespace, name, uid: "policy-uid-new", resource_version: "8", labels: input.labels || {},
        pod_selector: input.spec.podSelector || {}, policy_types: input.spec.policyTypes || [], ingress: input.spec.ingress || [], egress: input.spec.egress || [] };
      networkPolicies.push(created);
      return reply(created, 201);
    }
    if (method === "PUT" && index >= 0) {
      networkPolicies[index] = { ...networkPolicies[index], labels: input.labels || {}, pod_selector: input.spec.podSelector || {},
        policy_types: input.spec.policyTypes || [], ingress: input.spec.ingress || [], egress: input.spec.egress || [], resource_version: "8" };
      return reply(networkPolicies[index]);
    }
    if (method === "DELETE" && index >= 0) {
      networkPolicies = networkPolicies.filter((_, i) => i !== index);
      return reply({ namespace, name, deleted: true });
    }
  }
  if (/\/network\/configuration$/.test(p)) return reply({ resources: [
    { kind: "Ingress", api_version: "networking.k8s.io/v1", available: true, items: [ingress] },
    { kind: "ApisixRoute", api_version: "apisix.apache.org/v2", available: false, state: "not_installed", items: [] }
  ] });
  if (/\/apply$/.test(p)) return reply({ objects: [{ status: "applied", name: "web" }], failed: 0 });
  if (p === "/cicd/applications") return reply([app]);
  if (p === "/cicd/targets") return reply([{ id: 1, name: "runner", namespace: "matrix-cicd" }]);
  if (p === "/cicd/helm/repos") return reply([{ id: 10, name: "charts-internal", url: "https://charts.internal", env: "dev",
    auth: "not_supported", last_sync_status: "succeeded", last_sync_at: now, updated_at: now }]);
  if (p === "/cicd/helm/charts") return reply({ items: [{ repo_id: 10, repo_name: "charts-internal", env: "dev",
    chart: { name: "api-service", version: "1.2.0", app_version: "1.0.0", description: "Internal API service" } }] });
  if (p === "/cicd/jenkins") return reply([{ id: 2, name: "existing-jenkins", url: "https://jenkins.example", username: "ci" }]);
  if (/^\/cicd\/jenkins\/\d+\/job-parameters$/.test(p)) return reply({ job: u.searchParams.get("job"), parameters: [
    { name: "BRANCH", description: "Git ref", type: "StringParameterDefinition", default_value: "main" },
    { name: "DEPLOY_ENV", description: "Target cluster", type: "ChoiceParameterDefinition", choices: ["test", "prod"] },
    { name: "TOKEN", type: "PasswordParameterDefinition", sensitive: true }
  ] });
  if (p === "/cicd/argocd") return reply([argo]);
  if (p === "/cicd/argocd/3/applications") return reply({ items: [{ metadata: { name: "web" }, spec: { project: "team", destination: { namespace: "prod" } }, status: { sync: { status: "OutOfSync", revision: commit }, health: { status: "Healthy" } } }] });
  if (p === "/cicd/pipelines") return reply([{ id: 1, name: "web-pipeline", app_id: 1, runner_target_id: 1, stages: [{ name: "test", type: "test", image: "alpine", commands: ["true"] }] }]);
  if (p === "/cicd/runs") return reply({ items: [{ id: 55, pipeline_id: 1, app_id: 1, status: "succeeded", branch: "main", triggered_by: "smoke", created_at: now }], total: 1 });
  if (p === "/provision/preflight") {
    const masters = input.mode === "single" || input.hosts.length < 3 ? 1 : 3;
    return reply({ method: "k3s", version: input.version, masters, workers: input.hosts.length - masters, ha: masters === 3,
      endpoint: "https://api.example:6443", registry_mirror: registryMirror, warnings: ["Fresh hosts only"], hosts: input.hosts.map((h, i) => ({ ip: h.ip, port: h.port, username: h.username, role: i < masters ? "master" : "worker", fingerprint: "SHA256:mock-host-" + i })) });
  }
  if (p === "/provision/jobs" && method === "POST") return pending ? reply({ ticket: { id: 11 } }, 202, 40304) : reply(job, 201);
  if (p === "/provision/jobs") return reply({ items: [job] });
  if (p === "/provision/jobs/9") return reply(job);
  if (p === "/provision/jobs/9/cancel") { job = { ...job, status: "cancelled" }; return reply({ cancel_requested: true }); }
  if (p === "/ai/features") return reply({});
  if (p === "/ai/models/options") return reply(aiModelsAvailable ? [{ id: 5, name: "smoke-model", model: "deepseek-chat", is_default: true }] : []);
  if (p === "/ai/chat" && method === "POST") {
    aiChats.push(input);
    if (aiChatFailure) return reply({ message: "model unavailable" }, 502, 50201);
    return reply({ content: "建议先检查 Pod 事件与最近一次发布。", finish_reason: "stop", events: [] });
  }
  if (p === "/ai/models" || p === "/ai/prompts") return reply([]);
  if (p === "/oncall/overview") return reply({});
  return reply({ items: [], total: 0 });
}

class CDP {
  constructor(ws) {
    this.ws = ws; this.id = 0; this.pending = new Map(); this.errors = [];
    ws.onmessage = (event) => {
      const m = JSON.parse(event.data);
      if (m.id) {
        const p = this.pending.get(m.id);
        if (p) { this.pending.delete(m.id); m.error ? p.reject(new Error(m.error.message)) : p.resolve(m.result); }
      } else if (m.method === "Runtime.exceptionThrown") this.errors.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
    };
  }
  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((resolve, reject) => { this.pending.set(id, { resolve, reject }); this.ws.send(JSON.stringify({ id, method, params })); });
  }
  async eval(expression) {
    const r = await this.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true, userGesture: true });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
    return r.result.value;
  }
  async until(expression, message, timeout = 8000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) { if (await this.eval(expression)) return; await sleep(100); }
    throw new Error(message + ": " + await this.eval("document.querySelector('#view')?.innerText.slice(0,600)") + "\n" + this.errors.join("\n"));
  }
}

const server = http.createServer(async (req, res) => {
  try {
    const u = new URL(req.url, "http://localhost");
    if (u.pathname.startsWith("/api/v1")) return await api(req, res, u);
    const target = path.resolve(root, "." + (u.pathname === "/" ? "/index.html" : decodeURIComponent(u.pathname)));
    if (!target.startsWith(root)) { res.writeHead(403); res.end(); return; }
    res.writeHead(200, { "Content-Type": target.endsWith(".js") ? "text/javascript" : target.endsWith(".css") ? "text/css" : target.endsWith(".svg") ? "image/svg+xml" : "text/html" });
    res.end(fs.readFileSync(target));
  } catch (err) { res.writeHead(404); res.end("not found"); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const base = "http://127.0.0.1:" + server.address().port;
const debugPort = 19426;
const profile = fs.mkdtempSync(path.join(os.tmpdir(), "matrix-features-"));
const chrome = spawn(chromePath, ["--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  "--remote-debugging-port=" + debugPort, "--remote-allow-origins=*", "--user-data-dir=" + profile, "--window-size=1440,1000", "about:blank"], { stdio: "ignore" });
let c;
try {
  let version;
  for (let i = 0; i < 60; i++) { try { version = await (await fetch("http://127.0.0.1:" + debugPort + "/json/version")).json(); break; } catch {} await sleep(200); }
  assert.ok(version, "headless Chrome did not start");
  const target = await (await fetch("http://127.0.0.1:" + debugPort + "/json/new?about:blank", { method: "PUT" })).json();
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
  c = new CDP(ws);
  await c.send("Runtime.enable"); await c.send("Page.enable");
  await c.send("Page.addScriptToEvaluateOnNewDocument", { source: "localStorage.setItem('xs_access_token','mock-test-token');" });
  await c.send("Page.navigate", { url: base + "/#/cluster/list" });
  await c.until("window.XS?.mode==='live' && document.querySelector('#auth')?.hidden && window.XSLiveOps", "application did not start");
  assert.equal(await c.eval("routes.observe.render === renderObserve && routes.logs.render.toString().includes('renderLogs') && routes.cluster.__xsLivePatched && routes.cluster.__xsLiveOrig === renderCluster && !routes.cluster.__xlopsPatched && routes.sre.render === renderSRE"), true,
    "cluster should have one live wrapper retaining its original renderer; unrelated renderers must remain intact");
  await c.until("!!document.querySelector('[data-climport]')", "initial cluster route was not repainted with the live renderer");
  console.log("PASS cluster route has one live wrapper; unrelated renderers are retained");
  await c.eval("openWizard()");
  await c.until("!!document.querySelector('#eform') && document.querySelector('#eform').innerText.includes('4.5 核可用')", "environment wizard did not render live probe data");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes(location.origin)"), true, "wizard should detect and show the current platform address");
  assert.equal(await c.eval("/东京|devops\\.example\\.com|ccr\\.xingshu\\.dev|local-path|nfs-client/.test(document.querySelector('#modal').innerText)"), false,
    "environment wizard must not render fixture clusters, resources, mirrors or storage classes");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes('未检测（无网络探测接口）')"), true, "network reachability must not be fabricated");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes('fast、shared')"), true, "wizard should show probed StorageClasses");
  await c.eval("closeAll();openWizard(6)");
  await c.until("!!document.querySelector('#wform [name=wmode][value=use]')", "Kubernetes integration wizard did not open");
  await c.eval("(()=>{const r=document.querySelector('#wform [name=wmode][value=deploy]');r.checked=true;r.dispatchEvent(new Event('change',{bubbles:true}));})()");
  await c.until("document.querySelector('#modal').innerText.includes('平台内嵌 Ansible') && !!document.querySelector('#wdeploy')", "Kubernetes deployment did not use the embedded Ansible flow");
  await c.eval("document.querySelector('#wdeploy').click()");
  await c.until("!!document.querySelector('#provisionForm [data-host-key=ip]')", "Kubernetes deployment did not open the real SSH preflight form");
  assert.equal(await c.eval("document.querySelector('#provisionForm [data-host-key=ip]').value"), "", "deployment must not prefill an invented host IP");
  console.log("PASS live environment detection, honest unknown states and real SSH-based Kubernetes deployment entry");
  await c.eval("closeAll()");
  platformEnv = { deploy: "kubernetes", cluster: "prod-cluster", namespace: "matrix-system", nodes: 2,
    cpu: null, memory: null, ephemeral_storage: null, storage_class: null, storage_classes: null };
  await c.eval("openWizard()");
  await c.until("!!document.querySelector('#eform') && document.querySelector('#modal').innerText.includes('CPU 未接入')", "unknown environment probe data was not surfaced");
  await c.eval("closeAll();openWizard(5)");
  await c.until("!!document.querySelector('#wform [name=wmode][value=use]')", "component deploy wizard did not open");
  await c.eval("(()=>{const r=document.querySelector('#wform [name=wmode][value=deploy]');r.checked=true;r.dispatchEvent(new Event('change',{bubbles:true}));})()");
  await c.until("!!document.querySelector('#wdeploy')", "component deploy mode was not rendered");
  await c.eval("document.querySelector('#wdeploy').click()");
  await c.until("document.querySelector('#modal').innerText.includes('探测数据不完整')", "incomplete resource probes were not blocked");
  assert.equal(await c.eval("!!document.querySelector('#pgo') || !!document.querySelector('#pforce')"), false,
    "unknown resource data must not allow either deployment or force-through");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes('未判定')"), true);
  console.log("PASS unknown resource/storage measurements block component deployment instead of passing preflight");
  await c.eval("closeAll()");
  const visit = async (hash, check) => {
    await c.eval("location.hash=" + JSON.stringify(hash));
    await c.until(check || "!/正在|读取中/.test(document.querySelector('#view').innerText)", hash + " did not load");
    await sleep(150);
    assert.equal(await c.eval("/页面建设中|该页面尚未接入|页面加载出错|无可展示数据/.test(document.querySelector('#view').innerText)"), false, hash + " still shows a placeholder");
    assert.deepEqual(c.errors, [], hash + " browser exception");
  };
  await visit("#/traffic/topo", "document.querySelectorAll('[data-flow-node]').length===3");
  assert.equal(await c.eval("document.querySelectorAll('[data-flow-edge]').length"), 2);
  assert.equal(await c.eval("document.querySelector('.flow-node-card').getAttribute('width')"), "224");
  assert.equal(await c.eval("document.querySelector('.flow-node-card').getAttribute('height')"), "84");
  await c.eval("(()=>document.querySelector('[data-flow-node]').dispatchEvent(new PointerEvent('pointerover',{bubbles:true})))()");
  assert.equal(await c.eval("document.querySelector('[data-flow-node]').classList.contains('flow-active') && document.querySelector('[data-flow-edge]').classList.contains('flow-focus')"), true, "hover should spotlight the connected route");
  await c.eval("(()=>document.querySelector('[data-flow-node]').dispatchEvent(new PointerEvent('pointerout',{bubbles:true,relatedTarget:document.body})))()");
  assert.ok(await c.eval("document.querySelectorAll('.flow-edge-line[marker-end]').length===2"), "observed calls need directional arrows");
  const topologyDesktop = await c.send("Page.captureScreenshot", { format: "png" });
  const topologyDesktopPath = path.join(os.tmpdir(), "matrix-traffic-topology-desktop.png");
  fs.writeFileSync(topologyDesktopPath, Buffer.from(topologyDesktop.data, "base64"));
  const dragStart = await c.eval("(()=>{const n=document.querySelector('[data-flow-node]'),r=n.querySelector('.flow-node-card').getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2,transform:n.getAttribute('transform'),path:document.querySelector('.flow-edge-line').getAttribute('d')};})()");
  await c.send("Input.dispatchMouseEvent", { type: "mousePressed", x: dragStart.x, y: dragStart.y, button: "left", clickCount: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: dragStart.x + 70, y: dragStart.y + 28, button: "left", buttons: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: dragStart.x + 70, y: dragStart.y + 28, button: "left", clickCount: 1 });
  assert.notEqual(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), dragStart.transform);
  assert.notEqual(await c.eval("document.querySelector('.flow-edge-line').getAttribute('d')"), dragStart.path);
  await c.eval("document.querySelector('[data-flow-reset]').click()");
  assert.equal(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), dragStart.transform);
  await c.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
  await c.until("document.querySelector('[data-flow-topology]')?.dataset.flowDirection==='vertical'", "traffic graph did not reflow for mobile");
  assert.equal(await c.eval("document.querySelector('.flow-node-card').getAttribute('width')"), "304");
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge-motion').length"), 2);
  const topologyMobile = await c.send("Page.captureScreenshot", { format: "png" });
  const topologyMobilePath = path.join(os.tmpdir(), "matrix-traffic-topology-mobile.png");
  fs.writeFileSync(topologyMobilePath, Buffer.from(topologyMobile.data, "base64"));
  assert.equal(await c.eval("document.documentElement.scrollWidth<=innerWidth+2"), true, "mobile topology should not overflow the viewport");
  await c.send("Emulation.clearDeviceMetricsOverride");
  await c.until("document.querySelector('[data-flow-topology]')?.dataset.flowDirection==='horizontal'", "traffic graph did not return to desktop layout");
  console.log("PASS traffic flow arrows, compact nodes, drag/reset, and responsive re-layout", topologyDesktopPath, topologyMobilePath);
  for (const hash of ["#/sre", "#/cluster/list", "#/ops", "#/iam", "#/approvals", "#/team", "#/observe", "#/observe/rules",
    "#/observe/channels", "#/cicd/pipeline", "#/delivery/apps", "#/release", "#/middleware", "#/oncall", "#/settings", "#/ai", "#/automation", "#/testing"]) {
    await visit(hash);
    if (hash === "#/settings") {
      assert.equal(await c.eval("(()=>{const view=document.querySelector('#view');const text=view.innerText;return text.includes('系统设置')&&!!view.querySelector('[data-system-config=\"basic\"]')&&['基础信息','代码仓库','镜像仓库','K8s 集群','可观测性'].every(label=>text.includes(label))&&!text.includes('系统接入')&&!text.includes('ai.features')&&!text.includes('public.notices')&&!text.includes('公开配置')})()"), true,
        "live settings should retain the original configuration groups under the confirmed page name");
    }
    console.log("PASS route", hash);
  }
  await visit("#/delivery/apps");
  assert.equal(await c.eval("/东京|demo-(dev|test|pre|prod)|prod-sh|staging-bj|dev-gz/.test(document.querySelector('#view').innerText)"), false,
    "live application delivery must not show seeded clusters");
  await visit("#/ops/multicluster");
  assert.equal(await c.eval("/东京|demo-(dev|test|pre|prod)|prod-sh|staging-bj|dev-gz/.test(document.querySelector('#view').innerText)"), false,
    "live multi-cluster view must only show clusters returned by the API");
  console.log("PASS live delivery and multi-cluster views contain no seeded cluster names");
  await visit("#/cluster/hpa/1", "document.querySelector('#view').innerText.includes('web-hpa')");
  assert.equal(await c.eval("!document.querySelector('[data-live-tab]') && document.querySelector('#view').innerText.includes('2 / 2')"), true,
    "HPA page should show live replica state without adding horizontal navigation");
  await c.eval("document.querySelector('[data-hpa-new]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 HPA')", "HPA create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.namespace.value='prod';f.elements.name.value='api-hpa';f.elements.spec.value=JSON.stringify({scaleTargetRef:{apiVersion:'apps/v1',kind:'Deployment',name:'api'},minReplicas:2,maxReplicas:6,metrics:[{type:'Resource',resource:{name:'cpu',target:{type:'Utilization',averageUtilization:70}}}]});f.elements.labels.value='{\"team\":\"platform\"}';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && document.querySelector('#view').innerText.includes('api-hpa')", "HPA create did not refresh into the inventory");
  const hpaCreate = writes.find((w) => w.path === "/clusters/1/hpa" && w.method === "POST");
  assert.equal(hpaCreate?.body.name, "api-hpa");
  assert.equal(hpaCreate?.body.spec.scaleTargetRef.name, "api");
  assert.equal(hpaCreate?.body.spec.metrics[0].resource.target.averageUtilization, 70);
  assert.deepEqual(hpaCreate?.body.labels, { team: "platform" });
  await visit("#/cluster/cron/1", "document.querySelector('#view').innerText.includes('nightly-cleanup')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('0 2 * * *') && !!document.querySelector('[data-act=\"cron-history\"][data-id=\"prod/nightly-cleanup\"]')"), true,
    "CronJob page should expose schedule and run history");
  await c.eval("document.querySelector('[data-act=\"cron-history\"][data-id=\"prod/nightly-cleanup\"]').click()");
  await c.until("document.querySelector('#modal')?.innerText.includes('nightly-cleanup-29010000')", "CronJob Job history absent");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes('Succeeded')"), true);
  assert.equal(reads.some((r) => r.path === "/clusters/1/cronjobs/prod/nightly-cleanup/jobs"), true,
    "CronJob run history should use the resource-scoped live API");
  await c.eval("document.querySelector('#modal [data-close]')?.click()");
  await c.eval("document.querySelector('[data-cron-new]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 CronJob')", "CronJob create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.namespace.value='prod';f.elements.name.value='hourly-cleanup';f.elements.spec.value=JSON.stringify({schedule:'0 * * * *',suspend:false,jobTemplate:{spec:{template:{spec:{restartPolicy:'Never',containers:[{name:'cleanup',image:'busybox:1.36',command:['sh','-c','echo done']}]}}}}});f.elements.labels.value='{}';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && document.querySelector('#view').innerText.includes('hourly-cleanup')", "CronJob create did not refresh into the inventory");
  const cronCreate = writes.find((w) => w.path === "/clusters/1/cronjobs" && w.method === "POST");
  assert.equal(cronCreate?.body.spec.schedule, "0 * * * *");
  assert.equal(cronCreate?.body.spec.jobTemplate.spec.template.spec.containers[0].image, "busybox:1.36");
  await visit("#/cluster/hpa/1");
  await c.eval("XS.user.permissions=[];XS.user.roles=[]");
  await visit("#/cluster/cron/1", "document.querySelector('#view').innerText.includes('nightly-cleanup')");
  assert.equal(await c.eval("(()=>{const t=document.querySelector('#view table'),row=t?.querySelector('tbody tr');return !!t&&t.querySelector('thead th:last-child')?.innerText.trim()==='操作'&&row?.children.length===t.querySelectorAll('thead th').length&&!document.querySelector('[data-cron-new]')&&!!document.querySelector('[data-act=\"cron-history\"]')})()"), true,
    "read-only CronJob inventory should keep the actions header aligned while retaining run history");
  await c.eval("document.querySelector('[data-act=\"cron-history\"]').click()");
  await c.until("document.querySelector('#modal')?.innerText.includes('nightly-cleanup-29010000')", "read-only users should be able to inspect CronJob history");
  await c.eval("document.querySelector('#modal [data-close]')?.click();XS.user.permissions=['*'];XS.user.roles=['admin']");
  await visit("#/cluster/backup/1", "document.querySelector('#view').innerText.includes('VolumeSnapshot')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('未安装 snapshot.storage.k8s.io/v1 CRD') && !document.querySelector('[data-storage-new=\"snapshot\"]')"), true,
    "backup view should explain missing snapshot capability and avoid fake backups");
  console.log("PASS live HPA creation, CronJob create/history, and honest snapshot backup capability state");
  assert.equal(await c.eval("(()=>{const group=(href)=>document.querySelector('#nav a[href=\"'+href+'\"]').closest('.x-ng').querySelector('.x-ngh .x-ni span').textContent.trim();return group('#/cluster/network')==='工作负载'&&group('#/cluster/storage')==='工作负载'&&!!document.querySelector('#nav a[href=\"#/cluster/network\"]')&&!!document.querySelector('#nav a[href=\"#/cluster/storage\"]')})()"), true,
    "Kubernetes Service and storage should sit under the Workloads sidebar group");
  assert.equal(await c.eval("document.querySelectorAll('#nav a[href=\"#/cluster/network\"]').length"), 1,
    "Service route should only appear once in the sidebar");
  assert.equal(await c.eval("!!document.querySelector('#nav a[href=\"#/traffic/expose\"]') && !document.querySelector('#nav a[href=\"#/traffic/gateway\"]') && !document.querySelector('#nav a[href=\"#/traffic/ingress\"]')"), true,
    "Ingress and API gateway should be consolidated under one exposure page");
  await visit("#/cluster/nodes");
  assert.equal(await c.eval("/prod-bj|test-sh|dev-gz|10\\.0\\.1\\.11|10\\.1\\.1\\.11/.test(document.querySelector('#view').innerText)"), false,
    "live node inventory must not render bundled sample nodes");
  await visit("#/cluster/nodepools/2", "document.querySelector('#view').innerText.includes('system') && document.querySelector('#view').innerText.includes('未接入云厂商节点池扩缩容')");
  assert.equal(await c.eval("!document.querySelector('[data-live-tab]')"), true, "cluster pages must not render horizontal in-page navigation");
  await visit("#/cluster/storage/2", "document.querySelector('#view').innerText.includes('fast-ssd')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('pv-smoke') && document.querySelector('#view').innerText.includes('prod/data')"), true,
    "storage inventory should render fixture PV and PVC rows");
  assert.equal(await c.eval("!!document.querySelector('[data-storage-new=\"class\"]') && !!document.querySelector('[data-storage-new=\"pvc\"]') && !!document.querySelector('[data-storage-new=\"pv\"]')"), true,
    "storage page should expose create workflows for classes, claims and volumes");
  assert.equal(await c.eval("!document.querySelector('[data-storage-new=\"snapshot\"]')"), true,
    "snapshot creation must stay hidden until the API capability is actually supported");
  assert.equal(await c.eval("!!document.querySelector('[data-act=\"storage-pv-edit\"][data-id=\"pv-smoke\"]') && !document.querySelector('[data-act=\"storage-pv-edit\"][data-id=\"pv-smoke\"]').disabled"), true,
    "HostPath PVs with resource versions must expose editing");
  await c.eval("document.querySelector('[data-storage-new=\"class\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 StorageClass')", "storage class create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.name.value='smoke-class';f.elements.provisioner.value='csi.smoke.io';f.elements.reclaim_policy.value='Retain';f.elements.volume_binding_mode.value='Immediate';f.elements.parameters.value=JSON.stringify({type:'gp3'});f.elements.mount_options.value='noatime,discard';f.elements.is_default.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "storage class create did not submit");
  const classCreate = writes.find((w) => w.path === "/clusters/2/storage/classes" && w.method === "POST");
  assert.deepEqual(classCreate?.body, { name: "smoke-class", provisioner: "csi.smoke.io", reclaim_policy: "Retain",
    volume_binding_mode: "Immediate", allow_volume_expansion: true, is_default: true, parameters: { type: "gp3" },
    mount_options: ["noatime", "discard"] });
  await c.eval("document.querySelector('[data-act=\"storage-sc-edit\"][data-id=\"fast-ssd\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('编辑 StorageClass')", "storage class edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.allow_volume_expansion.checked=false;f.elements.is_default.checked=false;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "storage class update did not submit");
  const classUpdate = writes.find((w) => w.path === "/clusters/2/storage/classes/fast-ssd" && w.method === "PUT");
  assert.equal(classUpdate?.body.resource_version, "11", "StorageClass update must carry the loaded resource version");
  assert.equal(classUpdate?.body.name, "fast-ssd");
  assert.equal(classUpdate?.body.provisioner, "csi.example.io", "immutable provisioner must be resent unchanged");
  assert.equal(classUpdate?.body.volume_binding_mode, "WaitForFirstConsumer", "immutable binding mode must be resent from the loaded object");
  assert.equal(classUpdate?.body.allow_volume_expansion, false);
  assert.equal(classUpdate?.body.is_default, false);
  await c.eval("document.querySelector('[data-act=\"storage-sc-del\"][data-id=\"smoke-class\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('删除 StorageClass')", "storage class delete confirmation absent");
  await c.eval("document.querySelector('#liveForm').requestSubmit()");
  await sleep(150);
  assert.equal(await c.eval("!document.querySelector('#modal').hidden"), true, "StorageClass delete must require explicit confirmation");
  assert.equal(writes.some((w) => w.path === "/clusters/2/storage/classes/smoke-class" && w.method === "DELETE"), false,
    "unconfirmed StorageClass deletion must not reach the API");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "storage class delete did not submit");
  const classDelete = writes.find((w) => w.path === "/clusters/2/storage/classes/smoke-class" && w.method === "DELETE");
  assert.deepEqual(classDelete?.body, { uid: "sc-new-uid", resource_version: "41", force: false });
  await c.eval("document.querySelector('[data-storage-new=\"pvc\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 PersistentVolumeClaim')", "PVC create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.namespace.value='prod';f.elements.name.value='new-claim';f.elements.size.value='5Gi';f.elements.storage_class.value='fast-ssd';f.elements.labels.value=JSON.stringify({app:'smoke'});f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PVC create did not submit");
  const pvcCreate = writes.find((w) => w.path === "/clusters/2/storage/claims" && w.method === "POST");
  assert.deepEqual(pvcCreate?.body, { name: "new-claim", namespace: "prod", storage_class: "fast-ssd",
    access_modes: ["ReadWriteOnce"], size: "5Gi", volume_mode: "Filesystem", volume_name: "", labels: { app: "smoke" } });
  await c.eval("document.querySelector('[data-act=\"storage-pvc-edit\"][data-id=\"prod/data\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('编辑 PVC')", "PVC edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.size.value='30Gi';f.elements.labels.value=JSON.stringify({app:'web',tier:'data'});f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PVC update did not submit");
  const pvcUpdate = writes.find((w) => w.path === "/clusters/2/storage/claims/prod/data" && w.method === "PUT");
  assert.deepEqual(pvcUpdate?.body, { size: "30Gi", labels: { app: "web", tier: "data" }, resource_version: "31" });
  await c.eval("document.querySelector('[data-act=\"storage-pvc-del\"][data-id=\"prod/new-claim\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('删除 PVC')", "PVC delete confirmation absent");
  await c.eval("document.querySelector('#liveForm').requestSubmit()");
  await sleep(150);
  assert.equal(await c.eval("!document.querySelector('#modal').hidden"), true, "PVC delete must require explicit confirmation");
  assert.equal(writes.some((w) => w.path === "/clusters/2/storage/claims/prod/new-claim" && w.method === "DELETE"), false,
    "unconfirmed PVC deletion must not reach the API");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PVC delete did not submit");
  const pvcDelete = writes.find((w) => w.path === "/clusters/2/storage/claims/prod/new-claim" && w.method === "DELETE");
  assert.deepEqual(pvcDelete?.body, { uid: "pvc-new-uid", resource_version: "41", confirm: "prod/new-claim", allow_reclaim_delete: false });
  await c.eval("document.querySelector('[data-storage-new=\"pv\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 PersistentVolume')", "PV create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.name.value='new-pv';f.elements.capacity.value='50Gi';f.elements.host_path.value='/mnt/new';f.elements.labels.value=JSON.stringify({tier:'smoke'});f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PV create did not submit");
  const pvCreate = writes.find((w) => w.path === "/clusters/2/storage/volumes" && w.method === "POST");
  assert.deepEqual(pvCreate?.body, { name: "new-pv", capacity: "50Gi", access_modes: ["ReadWriteOnce"], reclaim_policy: "Retain",
    storage_class: "", volume_mode: "Filesystem", labels: { tier: "smoke" }, host_path: "/mnt/new" });
  await c.eval("document.querySelector('[data-act=\"storage-pv-edit\"][data-id=\"pv-smoke\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('编辑 PV')", "PV edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.capacity.value='25Gi';f.elements.labels.value=JSON.stringify({tier:'data'});f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PV update did not submit");
  const pvUpdate = writes.find((w) => w.path === "/clusters/2/storage/volumes/pv-smoke" && w.method === "PUT");
  assert.deepEqual(pvUpdate?.body, { name: "pv-smoke", capacity: "25Gi", access_modes: ["ReadWriteOnce"], reclaim_policy: "Delete",
    storage_class: "fast-ssd", volume_mode: "Filesystem", labels: { tier: "data" }, uid: "pv-uid", resource_version: "21", host_path: "/mnt/data" });
  await c.eval("document.querySelector('[data-act=\"storage-pv-del\"][data-id=\"new-pv\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('删除 PV')", "PV delete confirmation absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "PV delete did not submit");
  const pvDelete = writes.find((w) => w.path === "/clusters/2/storage/volumes/new-pv" && w.method === "DELETE");
  assert.deepEqual(pvDelete?.body, { uid: "pv-new-uid", resource_version: "41", confirm: "new-pv", force: false });
  console.log("PASS storage class/PVC/PV create, update and versioned delete payloads require confirmation");
  snapshotSupported = true;
  await c.eval("document.querySelector('[data-live-refresh]').click()");
  await c.until("!!document.querySelector('[data-storage-new=\"snapshot\"]')", "snapshot create control did not appear when capability was supported");
  await c.eval("document.querySelector('[data-storage-new=\"snapshot\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 VolumeSnapshot')", "snapshot create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.name.value='snap-smoke';f.elements.source_pvc.value='prod/data';f.elements.class_name.value='fast-snap';f.elements.labels.value=JSON.stringify({app:'web'});f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "snapshot create did not submit");
  const snapshotCreate = writes.find((w) => w.path === "/clusters/2/storage/snapshots" && w.method === "POST");
  assert.deepEqual(snapshotCreate?.body, { namespace: "prod", name: "snap-smoke", source_pvc: "data", class_name: "fast-snap", labels: { app: "web" } });
  await c.until("!!document.querySelector('[data-act=\"storage-snapshot-del\"][data-id=\"prod/snap-smoke\"]')", "created snapshot did not refresh into the inventory");
  await c.eval("document.querySelector('[data-act=\"storage-snapshot-del\"][data-id=\"prod/snap-smoke\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('删除 VolumeSnapshot')", "snapshot delete confirmation absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-storage-new=\"class\"]')", "snapshot delete did not submit");
  const snapshotDelete = writes.find((w) => w.path === "/clusters/2/storage/snapshots/prod/snap-smoke" && w.method === "DELETE");
  assert.deepEqual(snapshotDelete?.body, { uid: "snap-uid-1", resource_version: "51" });
  snapshotSupported = false;
  storagePending = true;
  await c.eval("document.querySelector('[data-storage-new=\"class\"]').click()");
  await c.until("!!document.querySelector('#liveForm')", "pending approval form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.name.value='pending-class';f.elements.provisioner.value='csi.pending.io';f.requestSubmit();})()");
  await c.until("document.querySelector('#toast')?.textContent.includes('已提交审批')", "production storage write did not surface a pending approval");
  assert.equal(await c.eval("document.querySelector('#modal').hidden"), true, "pending approval should close the form without claiming execution");
  await c.until("!!document.querySelector('[data-storage-new=\"class\"]')", "storage page did not repaint after pending approval");
  const pendingCreate = writes.find((w) => w.path === "/clusters/2/storage/classes" && w.method === "POST" && w.body.name === "pending-class");
  assert.equal(pendingCreate?.body.provisioner, "csi.pending.io");
  assert.equal(await c.eval("document.querySelector('#toast')?.textContent.includes('已创建 StorageClass pending-class')"), false,
    "pending approval must never be reported as an executed write");
  storagePending = false;
  console.log("PASS storage writes use cluster.change; production 202 surfaces approval pending and snapshot creation follows capability");
  await visit("#/cluster/network/2", "document.querySelector('#view').innerText.includes('prod/web')");
  assert.equal(await c.eval("!!document.querySelector('[data-network-new=\"service\"]') && !document.querySelector('[data-network-new=\"ingress\"]') && !document.querySelector('[data-network-new=\"policy\"]')"), true,
    "the workload Service page should manage Services only");
  await c.eval("document.querySelector('[data-network-new=\"service\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('创建 Service')", "Service create form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.namespace.value='prod';f.elements.name.value='api-edge';f.elements.type.value='NodePort';f.elements.ports.value=JSON.stringify([{name:'http',port:80,targetPort:8080,nodePort:30080,protocol:'TCP'}]);f.elements.selector.value=JSON.stringify({app:'api'});f.elements.external_ips.value='192.0.2.10, 192.0.2.11';f.elements.external_traffic_policy.value='Local';f.elements.labels.value=JSON.stringify({team:'platform'});f.elements.annotations.value=JSON.stringify({owner:'smoke'});f.elements.reason.value='smoke service';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && document.querySelector('#view').innerText.includes('api-edge')", "Service create did not refresh the inventory");
  const serviceCreate = writes.find((w) => w.path === "/clusters/2/network/services" && w.method === "POST");
  assert.deepEqual(serviceCreate?.body, { name: "api-edge", namespace: "prod", type: "NodePort",
    ports: [{ name: "http", port: 80, targetPort: 8080, nodePort: 30080, protocol: "TCP" }],
    selector: { app: "api" }, external_ips: ["192.0.2.10", "192.0.2.11"], external_name: "", load_balancer_ip: "",
    external_traffic_policy: "Local", session_affinity: "None", labels: { team: "platform" }, annotations: { owner: "smoke" } });
  assert.equal(new URLSearchParams(serviceCreate?.search).get("reason"), "smoke service");
  console.log("PASS Service is managed on its own workload page and submits an approved resource payload");
  await visit("#/cluster/config/2", "document.querySelector('#view').innerText.includes('registry-auth')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('.dockerconfigjson') && !document.querySelector('#view').innerText.includes('frontend-should-never-render-secret') && !!document.querySelector('[data-act=\"cm-detail\"]') && !document.querySelector('[data-act=\"secret-detail\"]')"), true,
    "Secret inventory should expose key metadata without a value-view action");
  await c.eval("document.querySelector('[data-act=\"cm-detail\"]').click()");
  await c.until("!document.querySelector('#modal').hidden && document.querySelector('#modal').innerText.includes('LOG_LEVEL')", "ConfigMap data detail absent");
  assert.equal(await c.eval("document.querySelector('#modal').innerText.includes('info')"), true, "non-secret ConfigMap values should remain inspectable");
  await c.eval("closeAll()");
  await c.eval("document.querySelector('[data-act=\"secret-edit\"][data-id=\"prod/registry-auth\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('Secret 旧值不会回显')", "Secret edit did not explain that existing values are omitted");
  assert.equal(await c.eval("document.querySelector('#liveForm').elements.data.value==='{}' && !document.querySelector('#liveForm').elements.data.value.includes('frontend-should-never-render-secret')"), true,
    "Secret edit must not prefill existing secret values");
  await c.eval("closeAll()");
  await visit("#/cluster/nodepools/2", "document.querySelector('#view').innerText.includes('system')");
  assert.equal(await c.eval("!!document.querySelector('[data-act=\"nodegroup-label\"][data-id=\"system\"]') && !!document.querySelector('[data-act=\"nodegroup-taint-add\"][data-id=\"system\"]') && !!document.querySelector('[data-act=\"nodegroup-taint-remove\"][data-id=\"system\"]')"), true,
    "non-empty node groups should expose label and taint batch controls");
  await c.eval("document.querySelector('[data-act=\"nodegroup-label\"][data-id=\"system\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('批量设置节点标签')", "node label batch form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.labels.value=JSON.stringify({'node.xingshu.com/pool':'system','team':'platform','retired':null});f.elements.reason.value='node label smoke';f.requestSubmit();})()");
  await c.until("document.querySelector('#toast')?.textContent.includes('已提交 1 个节点的标签变更') && !!document.querySelector('[data-act=\"nodegroup-taint-add\"]')", "node label batch did not submit");
  const nodeLabels = writes.find((w) => w.path === "/clusters/2/node-batch-labels" && w.method === "POST");
  assert.deepEqual(nodeLabels?.body, { nodes: ["node-1"], labels: { "node.xingshu.com/pool": "system", team: "platform", retired: null } });
  assert.equal(new URLSearchParams(nodeLabels?.search).get("reason"), "node label smoke");
  await c.eval("document.querySelector('[data-act=\"nodegroup-taint-add\"][data-id=\"system\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('批量新增节点污点')", "node taint-add form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.key.value='smoke.xingshu.io/dedicated';f.elements.value.value='batch';f.elements.effect.value='NoSchedule';f.elements.reason.value='node taint add smoke';f.requestSubmit();})()");
  await c.until("document.querySelector('#toast')?.textContent.includes('已提交 1 个节点的污点变更') && !!document.querySelector('[data-act=\"nodegroup-taint-remove\"]')", "node taint add did not submit");
  const nodeTaintAdd = writes.find((w) => w.path === "/clusters/2/node-batch-taints" && w.method === "PUT");
  assert.deepEqual(nodeTaintAdd?.body, { items: [{ name: "node-1", add: [{ key: "smoke.xingshu.io/dedicated", value: "batch", effect: "NoSchedule" }], remove: [] }] });
  assert.equal(new URLSearchParams(nodeTaintAdd?.search).get("reason"), "node taint add smoke");
  await c.eval("document.querySelector('[data-act=\"nodegroup-taint-remove\"][data-id=\"system\"]').click()");
  await c.until("!!document.querySelector('#liveForm') && document.querySelector('#modal').innerText.includes('批量移除节点污点')", "node taint-remove form absent");
  await c.eval("(()=>{const f=document.querySelector('#liveForm');f.elements.key.value='smoke.xingshu.io/dedicated';f.elements.value.value='batch';f.elements.effect.value='NoSchedule';f.elements.reason.value='node taint remove smoke';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && document.querySelector('#toast')?.textContent.includes('已提交 1 个节点的污点变更') && !!document.querySelector('[data-act=\"nodegroup-label\"]')", "node taint removal did not submit");
  const nodeTaintRemove = writes.find((w) => w.path === "/clusters/2/node-batch-taints" && w.method === "PUT" && w.body.items?.[0]?.remove?.length);
  assert.deepEqual(nodeTaintRemove?.body, { items: [{ name: "node-1", add: [], remove: [{ key: "smoke.xingshu.io/dedicated", value: "batch", effect: "NoSchedule" }] }] });
  console.log("PASS node-group label merge/removal and exact taint add/remove batch payloads");
  await c.eval("location.hash='#/cluster/storage/2'");
  await c.until("location.hash==='#/cluster/storage/2' && document.querySelector('#view').innerText.includes('fast-ssd')", "storage tab did not return to the selected cluster");
  assert.equal(await c.eval("!!document.querySelector('[data-storage-new=\"pvc\"]') && !document.querySelector('[data-storage-new=\"snapshot\"]')"), true,
    "storage management should remain available while unsupported snapshot creation stays hidden");
  const selectedClusterReadsStart = reads.length;
  await c.eval("(()=>{const s=document.querySelector('[data-live-cluster-select]');s.value='1';s.dispatchEvent(new Event('change',{bubbles:true}));})()");
  await c.until("location.hash==='#/cluster/storage/1' && document.querySelector('#view').innerText.includes('fast-ssd')", "cluster selector did not retain the selected resource page");
  assert.equal(reads.slice(selectedClusterReadsStart).filter((r) => /^\/clusters\/1\/storage\/(capabilities|classes|volumes|claims|snapshots)$/.test(r.path)).length >= 5, true,
    "switching clusters must query storage resources from the newly selected cluster");
  assert.equal(writes.some((w) => /^\/clusters\/\d+\/config\//.test(w.path)), false,
    "opening ConfigMap/Secret metadata must not send a resource mutation");
  await visit("#/settings");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('系统设置')"), true, "settings route should use the confirmed page name");
  assert.equal(await c.eval("(()=>{const view=document.querySelector('#view');const text=view.innerText;return !!view.querySelector('[data-system-config=\"basic\"]')&&['基础信息','代码仓库','镜像仓库','K8s 集群','可观测性'].every(label=>text.includes(label))&&!text.includes('系统接入')})()"), true,
    "live settings should retain the original configuration groups under the confirmed page name");
  await visit("#/cicd/pipeline");
  await visit("#/cluster/helm", "!!document.querySelector('[data-hmlive-tab]')");
  await c.eval("document.querySelectorAll('[data-hmlive-tab]')[2].click()");
  await c.until("document.querySelector('#view').innerText.includes('charts-internal') && document.querySelector('#view').innerText.includes('api-service')",
    "Helm live repository and catalog APIs did not render their returned data");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('通用 Chart 安装尚未接入')"), true,
    "Helm page must make the unsupported generic installer explicit");
  console.log("PASS Helm live repository and Chart catalog render backend data and state unsupported operations");
  await visit("#/cluster/storage/2");
  await visit("#/cluster/nodes/1");
  assert.equal(await c.eval("location.hash==='#/cluster/nodes/1' && document.querySelector('#view').innerText.includes('node-1')"), true,
    "cluster selector route should switch to the requested cluster after leaving other routes");
  await visit("#/cluster/config");
  assert.equal(await c.eval("/mysql-root|harbor-pull|Xs#2024!prodDb|sso-test\\.xingshu\\.com/.test(document.querySelector('#view').innerText)"), false,
    "live configuration inventory must not render bundled sample secrets");
  console.log("PASS cluster storage/network/config/node-group pages retain IDs, preserve route navigation, and hide Secret values");
  await visit("#/observe/rules", "!!document.querySelector('[data-rulenew]') && !document.querySelector('[data-rulenew]').disabled");
  await c.eval("document.querySelector('[data-rulenew]').click()");
  await c.until("!!document.querySelector('#modal form') && !!document.querySelector('#modal [name=cluster_id]')", "alert rule form did not open");
  await c.eval("(()=>{const f=document.querySelector('#modal form');f.elements.cluster_id.value='2';f.elements.namespace.value='monitoring';f.elements.rule_group.value='matrix-ui';f.elements.name.value='NodeHighCPU';f.elements.expr.value='sum(rate(node_cpu_seconds_total[5m])) > 0.85';f.elements.for.value='5m';f.elements.severity.value='warning';f.elements.summary.value='Node CPU is high';f.requestSubmit();})()");
  await c.until("document.querySelector('#toast')?.textContent.includes('告警规则已写入目标集群')", "alert rule was not written to the selected cluster");
  const alertRuleWrite = writes.find((w) => w.path === "/clusters/2/alert-rules" && w.method === "POST");
  assert.deepEqual(alertRuleWrite?.body, { namespace: "monitoring", rule_group: "matrix-ui", name: "NodeHighCPU",
    expr: "sum(rate(node_cpu_seconds_total[5m])) > 0.85", for: "5m", severity: "warning", summary: "Node CPU is high" });
  console.log("PASS live alert rules can be created against the explicitly selected cluster");
  await visit("#/testing");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('web-pipeline') && document.querySelector('#view').innerText.includes('#55') && !document.querySelector('#view').innerText.includes('网关限流回归')"), true,
    "live test management should show actual pipeline runs instead of demo plans");
  console.log("PASS live test management reads real test pipelines and run history");
  await visit("#/automation", "document.querySelector('#view').innerText.includes('ansible-playbook [core 2.21.5]')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('平台服务端内置')"), true, "automation should show the embedded platform runtime");
  await c.eval("window.__smokeConfirm=window.confirm;window.__smokeK8Confirm=window.k8Confirm;window.confirm=()=>true;window.k8Confirm=(_title,_message,_label,fn)=>fn();document.querySelector('[data-wfnew]').click()");
  await c.until("!!document.querySelector('#modal form')", "workflow editor absent");
  const customPlaybook = "- name: check hosts\n  hosts: all\n  gather_facts: false\n  tasks:\n    - ansible.builtin.ping: {}";
  await c.eval("(()=>{const f=document.querySelector('#modal form');f.elements.name.value='custom-playbook';f.elements.steps.value='';f.elements.playbook.value=" + JSON.stringify(customPlaybook) + ";f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "custom workflow did not save");
  for (let i = 0; i < 50 && !writes.some((w) => w.path === "/automation/workflows" && w.method === "POST"); i++) await sleep(100);
  const workflowCreate = writes.find((w) => w.path === "/automation/workflows" && w.method === "POST");
  assert.deepEqual(workflowCreate.body.steps, []);
  assert.equal(workflowCreate.body.playbook, customPlaybook);
  await visit("#/automation", "!!document.querySelector('[data-wftrig=\"77\"]')");
  await c.eval("document.querySelector('[data-wftrig=\"77\"]').click()");
  await c.until("!!document.querySelector('#modal form') && !!document.querySelector('#modal [name=password]')", "Ansible target credential form absent");
  await c.eval("(()=>{const f=document.querySelector('#modal form');f.elements.env.value='prod';f.elements.ips.value='10.2.0.5';f.elements.port.value='22';f.elements.username.value='deploy';f.elements.password.value='must-not-be-forwarded';f.requestSubmit();})()");
  for (let i = 0; i < 50 && !writes.some((w) => w.path === "/automation/workflows/77/trigger" && w.method === "POST"); i++) await sleep(100);
  const preflightWrite = writes.find((w) => w.path === "/automation/ansible/preflight");
  const triggerWrite = writes.find((w) => w.path === "/automation/workflows/77/trigger");
  assert.equal(preflightWrite.body.password, "must-not-be-forwarded");
  assert.equal("password" in triggerWrite.body, false, "trigger must only pass the opaque one-use preflight token");
  assert.equal(triggerWrite.body.preflight_token, "opaque-one-use-confirmation");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('Ansible')"), true);
  console.log("PASS embedded Ansible runtime, custom playbook version, verified-host confirmation and token-only trigger");
  await c.eval("window.confirm=window.__smokeConfirm;window.k8Confirm=window.__smokeK8Confirm;delete window.__smokeConfirm;delete window.__smokeK8Confirm");
  await visit("#/settings");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('notify.webhook')"), false, "legacy notification keys should not appear as duplicate platform settings");
  assert.equal(await c.eval("!!document.querySelector('a[href=\"#/observe/channels\"]')"), true, "settings should link to the single notification channel manager");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes(location.origin)"), true, "the current platform address should be detected automatically");
  assert.equal(await c.eval("!!document.querySelector('[data-mirror-provider] option[value=\"daocloud\"]')"), true, "settings should offer DaoCloud mirror preset");
  await c.eval("(()=>{const s=document.querySelector('[data-mirror-provider]');s.value='aliyun';s.dispatchEvent(new Event('change',{bubbles:true}));const e=document.querySelector('[data-mirror-endpoint]');e.value='https://tenant.mirror.aliyuncs.com';document.querySelector('[data-mirror-save]').click();})()");
  for (let i = 0; i < 40 && !writes.some((w) => w.path === "/provision/registry-mirror" && w.method === "PUT"); i++) await sleep(100);
  const aliMirrorWrite = writes.find((w) => w.path === "/provision/registry-mirror" && w.method === "PUT");
  assert.deepEqual(aliMirrorWrite?.body, { provider: "aliyun", endpoint: "https://tenant.mirror.aliyuncs.com" });
  await sleep(250);
  await c.eval("(()=>{const s=document.querySelector('[data-mirror-provider]');s.value='daocloud';s.dispatchEvent(new Event('change',{bubbles:true}));document.querySelector('[data-mirror-save]').click();})()");
  for (let i = 0; i < 40 && writes.filter((w) => w.path === "/provision/registry-mirror" && w.method === "PUT").length < 2; i++) await sleep(100);
  const mirrorWrite = writes.filter((w) => w.path === "/provision/registry-mirror" && w.method === "PUT").at(-1);
  assert.deepEqual(mirrorWrite.body, { provider: "daocloud", endpoint: "https://m.daocloud.io" });
  console.log("PASS settings save account-specific and DaoCloud image mirror selections without style changes");
  await visit("#/cluster/list", "!!document.querySelector('[data-climport]')");
  await c.eval("document.querySelector('[data-climport]').click()");
  await c.until("!!document.querySelector('#claf') && !!document.querySelector('#cltest')", "live multi-context import dialog absent");
  const kubeconfig = "apiVersion: v1\nkind: Config\nusers:\n- name: admin\n  user:\n    token: supersecret-import-token\ncontexts:\n- name: prod-admin\n  context: {cluster: prod, user: admin}\ncurrent-context: prod-admin\n";
  await c.eval("(()=>{const t=document.querySelector('#claf [name=kc]');t.value=" + JSON.stringify(kubeconfig) + ";t.dispatchEvent(new Event('input',{bubbles:true}));})()");
  await c.until("document.querySelectorAll('#claf [name=kcs]').length===3", "backend kubeconfig context parser did not populate the import table");
  assert.equal(await c.eval("document.querySelectorAll('#claf [name=kcs]')[0].checked"), true, "only the current context should be selected by default");
  assert.equal(await c.eval("document.querySelectorAll('#claf [name=kcs]')[1].checked || document.querySelectorAll('#claf [name=kcs]')[2].checked"), false);
  assert.equal(await c.eval("document.querySelector('#kcl').innerText.includes('supersecret-import-token')"), false, "credentials must not be returned or displayed in parsed context metadata");
  await c.eval("(()=>{const boxes=document.querySelectorAll('#claf [name=kcs]');for(const i of [1,2]){boxes[i].checked=true;boxes[i].dispatchEvent(new Event('change',{bubbles:true}));}document.querySelector('#cltest').click();})()");
  await c.until("document.querySelector('#kcl').innerText.includes('连接成功') && document.querySelector('#kcl').innerText.includes('连接被拒绝')", "per-context connection results absent");
  const contextTests = writes.filter((w) => w.path === "/clusters/test" && w.method === "POST");
  assert.deepEqual(contextTests.map((w) => w.body.context).sort(), ["dev-admin", "offline-admin", "prod-admin"]);
  await c.eval("document.querySelector('#claf').requestSubmit()");
  await c.until("document.querySelector('#claerr')?.innerText.includes('已接入 1 个') && document.querySelector('#claerr').innerText.includes('模拟导入失败')",
    "partial batch import results should be reported without treating failed contexts as imported");
  const clusterImports = writes.filter((w) => w.path === "/clusters/import" && w.method === "POST");
  assert.equal(clusterImports.length, 1, "selected kubeconfig contexts should be imported in one batch request");
  assert.equal(clusterImports[0].body.kubeconfig, kubeconfig);
  assert.deepEqual(clusterImports[0].body.items.map((item) => item.context).sort(), ["dev-admin", "prod-admin"]);
  assert.equal(clusterImports[0].body.items.some((item) => item.context === "offline-admin"), false, "failed context must never be submitted for import");
  assert.equal(await c.eval("document.querySelector('#claf [name=kc]').value.includes('supersecret-import-token')"), true, "the kubeconfig should stay in the active form while the import is in progress");
  assert.equal(await c.eval("Object.values(localStorage).join(' ').includes('supersecret-import-token')"), false, "kubeconfig credentials must never be persisted in browser storage");
  await c.eval("closeAll()");
  console.log("PASS multi-context import parses server-side, tests each selected connection and excludes failed contexts");
  await visit("#/ai/chat", "!!document.querySelector('[data-aichat-model]') && !!document.querySelector('.x-aiform input:not([disabled])')");
  assert.equal(await c.eval("Array.from(document.querySelectorAll('.x-ocpend li')).find(li=>li.innerText.includes('诊断记录'))?.querySelector('b')?.innerText"), "—",
    "live chat must not present an unread diagnosis history as an empty/demo count");
  await c.eval("(()=>{const input=document.querySelector('.x-aiform input');input.value='如何排查 Pod 重启';document.querySelector('.x-aiform button').click();})()");
  await c.until("document.querySelector('#view').innerText.includes('建议先检查 Pod 事件')", "live AI response absent");
  assert.equal(aiChats.length, 1);
  assert.equal(aiChats[0].tools, false, "interactive chat must never enable tools");
  assert.equal(aiChats[0].stream, false, "interactive chat should use the JSON response mode");
  assert.deepEqual(aiChats[0].messages, [{ role: "user", content: "如何排查 Pod 重启" }]);
  await c.eval("(()=>{const input=document.querySelector('.x-aiform input');input.value='继续';document.querySelector('.x-aiform button').click();})()");
  await c.until("document.querySelectorAll('#view .x-ailog > li').length>=5", "live AI follow-up absent");
  await c.until("!document.querySelector('.x-aiform button').disabled", "live AI follow-up is still processing");
  assert.deepEqual(aiChats[1].messages.map((m) => m.role), ["user", "assistant", "user"]);
  aiChatFailure = true;
  await c.eval("(()=>{const input=document.querySelector('.x-aiform input');input.value='trigger error';document.querySelector('.x-aiform button').click();})()");
  await c.until("document.querySelector('#view').innerText.includes('AI 请求失败')", "live AI error state absent");
  aiChatFailure = false;
  console.log("PASS AI chat sends non-streaming, tools-disabled requests and surfaces errors");
  await visit("#/ai/history", "!!document.querySelector('[data-ai-history-alert=\"42\"]')");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('Application availability affected')"), true);
  await c.eval("document.querySelector('[data-ai-history-alert=\"42\"]').click()");
  assert.equal(await c.eval("location.hash"), "#/observe/alerts/42", "AI history should link to its source alert");
  console.log("PASS AI diagnosis history reads live records and links to the originating alert");
  await visit("#/bastion/accounts", "!!document.querySelector('#basAdminAccountNew')");
  await c.eval("document.querySelector('#basAdminAccountNew').click()");
  await c.until("!!document.querySelector('#basAccountForm')", "bastion account form absent");
  assert.equal(await c.eval("document.querySelector('#basAccountForm [name=secret]').value"), "");
  await c.eval("(()=>{const f=document.querySelector('#basAccountForm');f.elements.asset_id.value='21';f.elements.name.value='deploy@web-01';f.elements.username.value='deploy';f.elements.secret.value='one-time-smoke-secret';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && document.querySelector('[data-bacc-edit=\"32\"]')", "bastion account create did not refresh");
  const accountCreate = writes.find((w) => w.path === "/bastion/accounts" && w.method === "POST");
  assert.equal(accountCreate?.body.secret, "one-time-smoke-secret");
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('one-time-smoke-secret')"), false, "account secret leaked into the rendered page");
  await c.eval("document.querySelector('[data-bacc-edit=\"32\"]').click()");
  await c.until("!!document.querySelector('#basAccountForm')", "bastion account edit form absent");
  assert.equal(await c.eval("document.querySelector('#basAccountForm [name=secret]').value"), "", "existing credentials must never be echoed into edit forms");
  await c.eval("document.querySelector('#basAccountForm').requestSubmit()");
  await c.until("document.querySelector('#modal').hidden", "bastion account update did not finish");
  const accountUpdate = writes.find((w) => w.path === "/bastion/accounts/32" && w.method === "PUT");
  assert.equal(Object.hasOwn(accountUpdate?.body || {}, "secret"), false, "blank update should retain the encrypted backend secret");
  await c.eval("document.querySelector('[data-bacc-del=\"32\"]').click()");
  await c.until("!!document.querySelector('#kcOk')", "bastion account delete confirmation absent");
  await c.eval("document.querySelector('#kcOk').click()");
  await c.until("!document.querySelector('[data-bacc-edit=\"32\"]')", "bastion account delete did not refresh");
  assert.ok(writes.some((w) => w.path === "/bastion/accounts/32" && w.method === "DELETE"));
  console.log("PASS bastion account CRUD keeps credentials masked and blank updates non-destructive");
  await visit("#/bastion/policy", "!!document.querySelector('#basAdminRuleNew')");
  await c.eval("document.querySelector('#basAdminRuleNew').click()");
  await c.until("!!document.querySelector('#basRuleForm')", "bastion policy form absent");
  await c.eval("(()=>{const f=document.querySelector('#basRuleForm');f.elements.name.value='smoke-role';f.elements.subject_type.value='role';f.elements.subject.value='platform-ops';f.querySelector('[name=asset_id]').checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-brule-edit=\"41\"]')", "bastion policy create did not refresh");
  const ruleCreate = writes.find((w) => w.path === "/bastion/rules" && w.method === "POST");
  assert.equal(ruleCreate?.body.subject_type, "role");
  assert.deepEqual(ruleCreate?.body.asset_ids, [21]);
  await c.eval("document.querySelector('[data-brule-edit=\"41\"]').click()");
  await c.until("!!document.querySelector('#basRuleForm')", "bastion policy edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#basRuleForm');f.elements.name.value='smoke-role-updated';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "bastion policy update did not finish");
  assert.ok(writes.some((w) => w.path === "/bastion/rules/41" && w.method === "PUT"));
  await c.eval("document.querySelector('[data-brule-del=\"41\"]').click()");
  await c.until("!!document.querySelector('#kcOk')", "bastion policy delete confirmation absent");
  await c.eval("document.querySelector('#kcOk').click()");
  await c.until("!document.querySelector('[data-brule-edit=\"41\"]')", "bastion policy delete did not refresh");
  assert.ok(writes.some((w) => w.path === "/bastion/rules/41" && w.method === "DELETE"));
  console.log("PASS bastion authorization policy CRUD persists explicit user/role and asset scopes");
  await visit("#/bastion/cmd", "!!document.querySelector('#basAdminFilterNew')");
  await c.eval("document.querySelector('#basAdminFilterNew').click()");
  await c.until("!!document.querySelector('#basFilterForm')", "bastion command filter form absent");
  await c.eval("(()=>{const f=document.querySelector('#basFilterForm');f.elements.name.value='block-root-remove';f.elements.pattern.value='^rm\\\\s+-rf\\\\s+/$';f.elements.action.value='deny';f.elements.priority.value='5';f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden && !!document.querySelector('[data-bfilter-edit=\"51\"]')", "bastion filter create did not refresh");
  const filterCreate = writes.find((w) => w.path === "/bastion/filters" && w.method === "POST");
  assert.equal(filterCreate?.body.pattern, "^rm\\s+-rf\\s+/$");
  assert.equal(filterCreate?.body.action, "deny");
  await c.eval("document.querySelector('[data-bfilter-edit=\"51\"]').click()");
  await c.until("!!document.querySelector('#basFilterForm')", "bastion command filter edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#basFilterForm');f.elements.enabled.checked=false;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "bastion filter update did not finish");
  assert.ok(writes.some((w) => w.path === "/bastion/filters/51" && w.method === "PUT"));
  await c.eval("document.querySelector('[data-bfilter-del=\"51\"]').click()");
  await c.until("!!document.querySelector('#kcOk')", "bastion filter delete confirmation absent");
  await c.eval("document.querySelector('#kcOk').click()");
  await c.until("!document.querySelector('[data-bfilter-edit=\"51\"]')", "bastion filter delete did not refresh");
  assert.ok(writes.some((w) => w.path === "/bastion/filters/51" && w.method === "DELETE"));
  console.log("PASS bastion RE2 command filters persist deny/approval, priority and enabled state");
  await visit("#/bastion/sess", "!!document.querySelector('[data-bs-kill=\"72\"]')");
  await c.eval("document.querySelector('[data-bs-kill=\"72\"]').click()");
  await c.until("!!document.querySelector('#kcOk')", "live bastion session confirmation absent");
  await c.eval("document.querySelector('#kcOk').click()");
  await c.until("!document.querySelector('[data-bs-kill=\"72\"]')", "live bastion session did not refresh after termination");
  assert.ok(writes.some((w) => w.path === "/bastion/sessions/72/kill" && w.method === "POST"));
  console.log("PASS live bastion session list calls the backend kill endpoint with explicit confirmation");
  await visit("#/bastion/audit", "!!document.querySelector('[data-bas-audit=\"71\"]')");
  await c.eval("document.querySelector('[data-bas-audit=\"71\"]').click()");
  await c.until("document.querySelector('#basAuditDetail')?.innerText.includes('systemctl status nginx')", "bastion command audit detail absent");
  assert.equal(await c.eval("document.querySelector('#basAuditDetail').innerText.includes('one-time-smoke-secret')"), false, "audit page exposed a credential");
  await c.eval("closeAll()");
  await c.eval("(()=>{URL.createObjectURL=(blob)=>{window.__auditBlobText=blob.text();return 'blob:smoke'};HTMLAnchorElement.prototype.click=function(){};document.querySelector('#basAuditExport').click();})()");
  const csvText = await c.eval("window.__auditBlobText");
  assert.ok(csvText.includes("systemctl") === false && csvText.includes("smoke") && csvText.includes("web-01"), "audit CSV should export the filtered session list without command payloads");
  console.log("PASS bastion audit reads live session commands and exports actual session records");
  await visit("#/bastion/batch", "!!document.querySelector('#btLiveF')");
  assert.equal(await c.eval("document.querySelectorAll('[data-bt-live-host]').length"), 3, "batch page should read the live managed-host inventory");
  assert.equal(await c.eval("document.querySelector('[data-bt-live-host=\"102\"]').disabled"), true, "offline hosts must not be selectable");
  await c.eval("(()=>{window.__prodConfirmCount=0;window.confirm=()=>{window.__prodConfirmCount++;return true;};const h=document.querySelector('[data-bt-live-host=\"101\"]');h.checked=true;h.dispatchEvent(new Event('change',{bubbles:true}));const cmd=document.querySelector('#btLiveCommand');cmd.value='uptime';cmd.dispatchEvent(new Event('input',{bubbles:true}));const timeout=document.querySelector('#btLiveF [name=timeout]');timeout.value='30';timeout.dispatchEvent(new Event('change',{bubbles:true}));document.querySelector('#btLiveF').requestSubmit();})()");
  await c.until("document.querySelector('#view').innerText.includes('batch-smoke') && !!document.querySelector('#view details')", "live managed-host batch result absent");
  await c.eval("document.querySelector('#view details summary').click()");
  await c.until("document.querySelector('#view').innerText.includes('up 4 days')", "live managed-host command output absent");
  assert.equal(await c.eval("window.__prodConfirmCount"), 1, "production-like environment must require explicit confirmation");
  const batchWrite = writes.find((w) => w.path === "/hosts/batch-exec" && w.method === "POST");
  assert.deepEqual(batchWrite?.body, { host_ids: [101], command: "uptime", timeout_sec: 30 });
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('Agent 在线')"), true, "live page should identify Agent-managed hosts");
  console.log("PASS Bastion batch page runs real Agent tasks with offline exclusion and production confirmation");
  await visit("#/bastion/req", "!!document.querySelector('#basReqForm')");
  await c.eval("(()=>{const f=document.querySelector('#basReqForm');f.elements.asset_id.value='21';f.elements.asset_id.dispatchEvent(new Event('change',{bubbles:true}));f.elements.account_id.value='31';f.elements.duration_minutes.value='30';f.elements.reason.value='排查生产服务故障';f.requestSubmit();})()");
  await c.until("document.querySelector('#view').innerText.includes('#81') && document.querySelector('#view').innerText.includes('待审批')", "live access request did not appear in approval history");
  const accessRequestWrite = writes.find((w) => w.path === "/bastion/access-requests" && w.method === "POST");
  assert.deepEqual(accessRequestWrite?.body, { asset_id: 21, account_id: 31, duration_minutes: 30, reason: "排查生产服务故障" });
  assert.equal(await c.eval("document.querySelector('#view').innerText.includes('secret')"), false, "requestable-asset view must not expose credentials");
  console.log("PASS bastion access request uses scoped assets and the platform approval history");
  await visit("#/logs", "!!document.querySelector('#lgin') && document.querySelectorAll('#lgw tbody tr.x-lgr').length===3");
  assert.equal(await c.eval("!!document.querySelector('.x-lgside')"), false, "log filters should not consume a growing sidebar");
  assert.equal(await c.eval("!!document.querySelector('#lgfacetbtn') && !document.querySelector('#lgfacetpop')"), true, "field filters should start as a compact closed control");
  const selectLogFacet = async (field, value, exclude = false) => {
    const script = `(()=>{const btn=document.querySelector('#lgfacetbtn');if(!document.querySelector('#lgfacetpop'))btn.click();const f=document.querySelector('#lgfacetfield');f.value=${JSON.stringify(field)};f.dispatchEvent(new Event('change',{bubbles:true}));const q=document.querySelector('#lgfacetsearch');q.value=${JSON.stringify(value)};q.dispatchEvent(new Event('input',{bubbles:true}));const b=[...document.querySelectorAll('#lgfacetvalues [data-lgadd]')].find(x=>x.dataset.v===${JSON.stringify(value)}&&!!x.dataset.neg===${JSON.stringify(exclude)});if(!b)throw new Error('facet value not found: '+${JSON.stringify(value)});b.click();})()`;
    await c.eval(script);
  };
  await c.eval("document.querySelector('#lgfacetbtn').click()");
  assert.equal(await c.eval("!!document.querySelector('#lgfacetpop') && getComputedStyle(document.querySelector('#lgfacetpop')).maxHeight!=='none' && getComputedStyle(document.querySelector('#lgfacetvalues')).maxHeight!=='none'"), true,
    "large value sets should stay in a height-bounded popover");
  const logFilterDesktop = await c.send("Page.captureScreenshot", { format: "png" });
  const logFilterDesktopPath = path.join(os.tmpdir(), "matrix-log-filter-desktop.png");
  fs.writeFileSync(logFilterDesktopPath, Buffer.from(logFilterDesktop.data, "base64"));
  await c.eval("document.querySelector('#lgfacetbtn').click()");
  await c.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
  await c.eval("document.querySelector('#lgfacetbtn').click()");
  await c.until("!!document.querySelector('#lgfacetpop')", "mobile field filter did not open");
  assert.equal(await c.eval("(()=>{const r=document.querySelector('#lgfacetpop').getBoundingClientRect();return r.left>=0&&r.right<=innerWidth+2&&document.documentElement.scrollWidth<=innerWidth+2;})()"), true,
    "field filter popover should fit a mobile viewport");
  const logFilterMobile = await c.send("Page.captureScreenshot", { format: "png" });
  const logFilterMobilePath = path.join(os.tmpdir(), "matrix-log-filter-mobile.png");
  fs.writeFileSync(logFilterMobilePath, Buffer.from(logFilterMobile.data, "base64"));
  await c.eval("document.querySelector('#lgfacetbtn').click()");
  await c.send("Emulation.clearDeviceMetricsOverride");
  console.log("LOG FILTER SCREENSHOTS", logFilterDesktopPath, logFilterMobilePath);
  const logCount = () => c.eval("document.querySelectorAll('#lgw tbody tr.x-lgr').length");
  await selectLogFacet("level", "ERROR");
  assert.equal(await c.eval("document.querySelector('#lgin').value"), 'level:"ERROR"');
  assert.equal(await logCount(), 2, "level facet did not filter the result rows");
  await selectLogFacet("app", "worker");
  assert.equal(await c.eval("document.querySelector('#lgin').value"), 'level:"ERROR" and app:"worker"');
  assert.equal(await logCount(), 1, "combined facet conditions did not narrow the result rows");
  const escapedPod = 'api"\\worker';
  await selectLogFacet("pod", escapedPod);
  const escapedQuery = 'level:"ERROR" and app:"worker" and pod:' + JSON.stringify(escapedPod);
  assert.equal(await c.eval("document.querySelector('#lgin').value"), escapedQuery, "facet values must be serialized as escaped KQL strings");
  assert.equal(await logCount(), 1, "escaped quote/backslash facet did not match");
  await selectLogFacet("pod", escapedPod, true);
  assert.equal(await logCount(), 0, "exclude facet did not filter the result rows");
  const runLogQuery = async (query) => {
    await c.eval("(()=>{const i=document.querySelector('#lgin');i.value=" + JSON.stringify(query) + ";i.dispatchEvent(new Event('input',{bubbles:true}));document.querySelector('[data-lgrun]').click();})()");
    await c.until("!!document.querySelector('#lgin') && (!!document.querySelector('#lgw tbody') || !!document.querySelector('#lgw .x-empty'))", "KQL query did not reload logs");
  };
  await runLogQuery('level:(ERROR or WARN) and app:"web"');
  assert.equal(await logCount(), 2, "field OR group did not match either value: " + await c.eval("JSON.stringify({query:document.querySelector('#lgin').value,error:document.querySelector('.x-lgerr')?.innerText,rows:document.querySelector('#lgw tbody')?.innerText})"));
  await selectLogFacet("level", "WARN");
  assert.equal(await c.eval("document.querySelector('#lgin').value"), 'app:"web" and level:"WARN"', "choosing a level should replace the same-field OR condition");
  assert.equal(await logCount(), 1, "replacement level facet did not preserve the app condition");
  await selectLogFacet("level", "ERROR");
  assert.equal(await c.eval("document.querySelector('#lgin').value"), 'app:"web" and level:"ERROR"', "successive level selections should not accumulate conflicting clauses");
  assert.equal(await logCount(), 1, "successive level selection did not update results");
  await runLogQuery('app:"web" and level:ERROR and level:WARN');
  assert.equal(await logCount(), 0, "contradictory same-field query should have no matches");
  await selectLogFacet("level", "WARN");
  assert.equal(await c.eval("document.querySelector('#lgin').value"), 'app:"web" and level:"WARN"', "facet selection should collapse duplicate same-field clauses");
  assert.equal(await logCount(), 1, "facet selection did not recover a contradictory same-field query");
  await runLogQuery('pod:' + JSON.stringify(escapedPod));
  assert.equal(await logCount(), 1, "quoted field value with escaped characters did not match");
  assert.deepEqual(c.errors, [], "#/logs browser exception");
  console.log("PASS KQL query, include/exclude facets, OR groups and escaped field values");
  await visit("#/traffic/ingress", "location.hash==='#/traffic/expose' && !!document.querySelector('[data-net-edit]')");
  await c.eval("document.querySelector('[data-net-edit]').click()");
  await c.until("!!document.querySelector('#netConfigForm')", "network editor absent");
  await c.eval("closeAll()");
  await visit("#/traffic/gateway", "location.hash==='#/traffic/expose' && !!document.querySelector('[data-net-create]')");
  await visit("#/traffic/mesh", "!!document.querySelector('[data-net-create]')");
  await visit("#/traffic/expose", "!!document.querySelector('[data-net-create]')");
  console.log("PASS consolidated external exposure page and legacy route redirects");
  await visit("#/traffic/topo", "document.querySelectorAll('[data-flow-node]').length===3");
  assert.equal(await c.eval("document.querySelectorAll('[data-flow-edge]').length"), 2);
  assert.equal(await c.eval("document.querySelector('.flow-node-card').getAttribute('width')"), "224");
  assert.match(await c.eval("document.querySelector('.x-flow-unobserved').textContent"), /db/);
  assert.ok(await c.eval("parseFloat(getComputedStyle(document.querySelector('.flow-edge-motion')).strokeWidth) >= 3"), "observed flow should be visually distinct");
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge-motion').length"), 2);
  assert.match(await c.eval("document.querySelector('.flow-edge-label').textContent"), /样本.*错/);
  assert.ok(await c.eval("(()=>{const e=document.querySelector('[data-flow-edge]'),p=e.querySelector('.flow-edge-line'),m=p.getPointAtLength(p.getTotalLength()/2),l=e.querySelector('.flow-edge-label');return Math.abs(Number(l.getAttribute('y'))-m.y)>=24;})()"), "desktop edge captions should clear the flow path");
  assert.notEqual(await c.eval("getComputedStyle(document.querySelector('.flow-edge-motion')).animationName"), "none");
  const start = await c.eval("(()=>{const e=document.querySelector('[data-flow-node]');const r=e.querySelector('.flow-node-card').getBoundingClientRect(),edge=document.querySelector('[data-flow-edge]'),line=edge.querySelector('.flow-edge-line');return {x:r.x+r.width/2,y:r.y+r.height/2,transform:e.getAttribute('transform'),path:line.getAttribute('d'),motion:edge.querySelector('.flow-edge-motion').getAttribute('d'),label:edge.querySelector('.flow-edge-label').getAttribute('x'),labelBg:edge.querySelector('[data-flow-label-bg]').getAttribute('x')};})()");
  await c.send("Input.dispatchMouseEvent", { type: "mousePressed", x: start.x, y: start.y, button: "left", clickCount: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: start.x + 75, y: start.y + 35, button: "left", buttons: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: start.x + 75, y: start.y + 35, button: "left", clickCount: 1 });
  assert.notEqual(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), start.transform);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] path').getAttribute('d')"), start.path);
  assert.equal(await c.eval("document.querySelector('[data-flow-edge] .flow-edge-line').getAttribute('d')===document.querySelector('[data-flow-edge] .flow-edge-motion').getAttribute('d')"), true);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] .flow-edge-motion').getAttribute('d')"), start.motion);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] .flow-edge-label').getAttribute('x')"), start.label);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] [data-flow-label-bg]').getAttribute('x')"), start.labelBg);
  await c.eval("document.querySelector('[data-flow-reset]').click()");
  assert.equal(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), start.transform);
  const keyboardStart = await c.eval("(()=>{const e=document.querySelector('[data-flow-node]');e.focus();return {transform:e.getAttribute('transform'),path:document.querySelector('[data-flow-edge] path').getAttribute('d')};})()");
  await c.eval("document.querySelector('[data-flow-node]').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true,cancelable:true}))");
  assert.notEqual(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), keyboardStart.transform);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] path').getAttribute('d')"), keyboardStart.path);
  await c.eval("document.querySelector('[data-flow-reset]').click()");
  assert.equal(await c.eval("!!document.querySelector('[data-flow-empty]')"), false);
  traceSamplesAvailable = false;
  await c.eval("renderTrafficLive(document.querySelector('#view'))");
  await c.until("!!document.querySelector('[data-flow-empty]') && document.querySelectorAll('[data-flow-edge]').length===0", "empty trace window should be stated without inventing service calls");
  assert.equal(await c.eval("document.querySelectorAll('[data-flow-node].flow-connected').length"), 0);
  assert.match(await c.eval("document.querySelector('[data-flow-empty]').textContent"), /不推测调用关系/);
  traceSamplesAvailable = true;
  await c.eval("renderTrafficLive(document.querySelector('#view'))");
  await c.until("document.querySelectorAll('[data-flow-edge]').length===2", "trace refresh did not restore observed calls");
  console.log("PASS mouse and keyboard node movement, connected directional flow, reset, and honest empty state");
  await visit("#/netpol/graph", "document.querySelectorAll('[data-flow-edge]').length===4 && !!document.querySelector('.flow-edge.observed') && !!document.querySelector('.flow-edge.declared')");
  const desktopPolicy = await c.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  const desktopPolicyPath = path.join(os.tmpdir(), "matrix-netpol-desktop.png");
  fs.writeFileSync(desktopPolicyPath, Buffer.from(desktopPolicy.data, "base64"));
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge.observed').length"), 2);
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge.declared').length"), 2);
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge.observed .flow-edge-motion').length"), 2);
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge.declared .flow-edge-motion').length"), 0);
  const policyDrag = await c.eval("(()=>{const e=document.querySelector('[data-flow-node]');const r=e.querySelector('.flow-node-card').getBoundingClientRect(),edge=document.querySelector('[data-flow-edge]');return {x:r.x+r.width/2,y:r.y+r.height/2,transform:e.getAttribute('transform'),path:edge.querySelector('path').getAttribute('d'),label:edge.querySelector('.flow-edge-label').getAttribute('x')};})()");
  await c.send("Input.dispatchMouseEvent", { type: "mousePressed", x: policyDrag.x, y: policyDrag.y, button: "left", clickCount: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: policyDrag.x + 45, y: policyDrag.y + 30, button: "left", buttons: 1 });
  await c.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: policyDrag.x + 45, y: policyDrag.y + 30, button: "left", clickCount: 1 });
  assert.notEqual(await c.eval("document.querySelector('[data-flow-node]').getAttribute('transform')"), policyDrag.transform);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] path').getAttribute('d')"), policyDrag.path);
  assert.notEqual(await c.eval("document.querySelector('[data-flow-edge] .flow-edge-label').getAttribute('x')"), policyDrag.label);
  console.log("PASS policy graph separates measured traffic from declared rules and remains draggable");
  await visit("#/netpol", "!!document.querySelector('[data-netpol-create]') && !!document.querySelector('[data-netpol-edit=\"0\"]')");
  await c.eval("document.querySelector('[data-netpol-create]').click()");
  await c.until("!!document.querySelector('#netpolEditForm')", "NetworkPolicy create form absent");
  await c.eval("(()=>{const f=document.querySelector('#netpolEditForm');f.elements.namespace.value='prod';f.elements.name.value='smoke-policy';f.elements.labels.value='{\"team\":\"platform\"}';f.elements.spec.value=JSON.stringify({podSelector:{matchLabels:{app:'api'}},policyTypes:['Ingress'],ingress:[],egress:[]});f.elements.reason.value='smoke create';f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "NetworkPolicy create did not submit");
  const policyCreate = writes.find((w) => w.path.endsWith("/network/policies/prod/smoke-policy") && w.method === "POST");
  assert.equal(policyCreate?.body.spec.podSelector.matchLabels.app, "api");
  assert.equal(policyCreate?.body.labels.team, "platform");
  await c.until("document.querySelector('#view').innerText.includes('prod/smoke-policy')", "created NetworkPolicy did not refresh into the live list");
  await c.until("!!document.querySelector('[data-netpol-edit-key=\"prod/web-to-api\"]')", "NetworkPolicy list did not refresh after create");
  await c.eval("document.querySelector('[data-netpol-edit-key=\"prod/web-to-api\"]').click()");
  await c.until("!!document.querySelector('#netpolEditForm')", "NetworkPolicy edit form absent");
  await c.eval("(()=>{const f=document.querySelector('#netpolEditForm');f.elements.spec.value=JSON.stringify({podSelector:{matchLabels:{app:'updated-api'}},policyTypes:['Ingress'],ingress:[],egress:[]});f.elements.reason.value='smoke update';f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "NetworkPolicy update did not submit");
  const policyUpdate = writes.find((w) => w.path.endsWith("/network/policies/prod/web-to-api") && w.method === "PUT");
  assert.equal(policyUpdate?.body.uid, "policy-uid-1");
  assert.equal(policyUpdate?.body.resource_version, "7");
  await c.until("document.querySelector('#view').innerText.includes('app=updated-api')", "updated NetworkPolicy did not refresh into the live list");
  await c.until("!!document.querySelector('[data-netpol-delete-key=\"prod/web-to-api\"]')", "NetworkPolicy list did not refresh after update");
  await c.eval("document.querySelector('[data-netpol-delete-key=\"prod/web-to-api\"]').click()");
  await c.until("!!document.querySelector('#netpolDeleteForm')", "NetworkPolicy delete confirmation absent");
  await c.eval("(()=>{const f=document.querySelector('#netpolDeleteForm');f.elements.reason.value='smoke delete';f.elements.confirm.checked=true;f.requestSubmit();})()");
  await c.until("document.querySelector('#modal').hidden", "NetworkPolicy delete did not submit");
  const policyDelete = writes.find((w) => w.path.endsWith("/network/policies/prod/web-to-api") && w.method === "DELETE");
  assert.equal(policyDelete?.body.uid, "policy-uid-1");
  assert.equal(policyDelete?.body.resource_version, "8");
  await c.until("!document.querySelector('#view').innerText.includes('prod/web-to-api')", "deleted NetworkPolicy remained in the live list");
  console.log("PASS NetworkPolicy create/update/delete controls retain identity and require confirmation");
  await visit("#/netpol/test", "!!document.querySelector('#networkProbeForm')");
  await c.eval("(()=>{const f=document.querySelector('#networkProbeForm');f.elements.host.value='api.prod.svc';f.elements.port.value='8080';f.requestSubmit();})()");
  await c.until("document.querySelector('[data-probe-result]')?.dataset.reachable==='true'", "real connectivity probe result absent");
  const probe = writes.find((w) => w.path.endsWith("/network/probe"));
  assert.equal(probe.body.uid, readyPods[0].uid);
  assert.equal(probe.body.container, "api");
  console.log("PASS authenticated source-Pod TCP probe UI");
  await c.eval("XSProvision.open()");
  await c.until("!!document.querySelector('#provisionForm')", "provision dialog absent");
  await c.eval("(()=>{const f=document.querySelector('#provisionForm');f.elements.name.value='new-cluster';f.elements.env.value='test';f.elements.version.value='v1.31.4+k3s1';f.querySelector('[data-host-key=ip]').value='10.1.1.1';f.querySelector('[data-host-key=password]').value='mock-password';document.querySelector('#provisionPreflight').click();})()");
  await c.until("!!document.querySelector('[data-confirm-plan]')", "preflight plan absent");
  assert.equal(await c.eval("document.querySelector('#provisionSubmit').disabled"), true);
  await c.eval("(()=>{const f=document.querySelector('#provisionForm');f.elements.name.value='changed';f.elements.name.dispatchEvent(new Event('input',{bubbles:true}));})()");
  assert.equal(await c.eval("!document.querySelector('[data-confirm-plan]') && document.querySelector('#provisionSubmit').disabled"), true);
  await c.eval("document.querySelector('#provisionPreflight').click()");
  await c.until("!!document.querySelector('[data-confirm-plan]')", "second preflight absent");
  await c.eval("(()=>{const b=document.querySelector('[data-confirm-plan]');b.checked=true;b.dispatchEvent(new Event('change',{bubbles:true}));document.querySelector('#provisionForm').requestSubmit();})()");
  await c.until("document.querySelector('#modal').innerText.includes('排队中')", "job status absent");
  const submitted = writes.find((w) => w.path === "/provision/jobs" && w.method === "POST");
  assert.ok(submitted?.body.confirmed && submitted.body.hosts[0].fingerprint.startsWith("SHA256:"));
  assert.deepEqual(submitted?.body.registry_mirror, { provider: "daocloud", endpoint: "https://m.daocloud.io" });
  assert.equal(await c.eval("!!document.querySelector('[data-host-key=password]')"), false);
  console.log("PASS SSH preflight, mirror plan confirmation, fingerprint confirmation and persistent job");
  await c.eval("closeAll()");
  await visit("#/cicd/pipeline", "!!document.querySelector('[data-argo-apps]')");
  await c.eval("document.querySelector('[data-newpl]').click()");
  await c.until("!!document.querySelector('[data-stage-add]')", "pipeline editor absent");
  await c.eval("(()=>{const f=document.querySelector('.x-modal form');f.querySelector('[data-stage-add]').click();const rows=f.querySelectorAll('[data-stage-row]');const row=rows[rows.length-1];const type=row.querySelector('[data-stage-field=type]');type.value='jenkins';type.dispatchEvent(new Event('change',{bubbles:true}));const active=f.querySelectorAll('[data-stage-row]');const stage=active[active.length-1];stage.querySelector('[data-stage-field=name]').value='notify-ci';stage.querySelector('[data-stage-field=jenkins_job]').value='team/app';stage.querySelector('[data-jenkins-discover]').click();})()");
  await c.until("document.querySelector('.ops-jenkins-schema')?.innerText.includes('已发现 3 个 Job 参数')", "Jenkins parameter discovery absent");
  assert.equal(await c.eval("document.querySelector('[data-stage-field=params_text]').value.includes('BRANCH=main')"), true);
  assert.equal(await c.eval("document.querySelector('[data-stage-field=params_text]').value.includes('DEPLOY_ENV=')"), true);
  assert.equal(await c.eval("document.querySelector('[data-stage-field=params_text]').value.includes('do-not-return')"), false);
  assert.equal(await c.eval("document.querySelector('.ops-jenkins-schema').textContent.includes('可选值：test / prod') && document.querySelector('.ops-jenkins-schema').textContent.includes('敏感参数，默认值未读取')"), true);
  await c.eval("closeAll()");
  await c.eval("document.querySelector('[data-argo-apps]').click()");
  await c.until("!!document.querySelector('[data-argo-sync]')", "Argo applications absent");
  await c.eval("document.querySelector('[data-argo-sync]').click()");
  await c.until("!!document.querySelector('input[name=revision]')", "Argo revision confirmation absent");
  assert.equal(await c.eval("document.querySelector('input[name=revision]').value"), commit);
  await c.eval("closeAll()");
  console.log("PASS Jenkins parameter discovery and existing Argo integration");
  await visit("#/observe/alerts/42?decision=ai", "!!document.querySelector('[data-alert-remediate]:not([hidden])')");
  assert.equal(writes.some((w) => w.path.endsWith("/decision")), false, "notification link performed an operation");
  await c.eval("document.querySelector('[data-alert-manual]').click()");
  await c.until("document.querySelector('[data-alert-ai-state]')?.innerText.includes('人工处理')", "manual decision did not persist");
  assert.equal(writes.filter((w) => w.path.endsWith("/decision")).length, 1);
  console.log("PASS notification opens authenticated UI; manual decision is explicit and persisted");
  await c.eval("closeAll()");
  await visit("#/traffic/topo", "!!document.querySelector('[data-flow-topology]')");
  const desktop = await c.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  const desktopPath = path.join(os.tmpdir(), "matrix-features-desktop.png");
  fs.writeFileSync(desktopPath, Buffer.from(desktop.data, "base64"));
  await c.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
  await c.until("document.querySelector('[data-flow-topology]')?.viewBox.baseVal.width===360", "mobile traffic topology did not switch to vertical layout");
  await sleep(400);
  assert.equal(await c.eval("document.querySelectorAll('[data-flow-edge]').length"), 2, "mobile trace arrows absent");
  assert.equal(await c.eval("document.querySelector('.flow-node-card').getAttribute('width')"), "304", "mobile cards should fit the narrow graph layout");
  assert.equal(await c.eval("document.querySelectorAll('.flow-edge-motion').length"), 2, "mobile observed flow animation absent");
  assert.ok(await c.eval("(()=>{const edge=document.querySelector('[data-flow-edge]'),path=edge.querySelector('.flow-edge-line'),p=path.getPointAtLength(path.getTotalLength()/2),label=edge.querySelector('.flow-edge-label');return Math.abs(Number(label.getAttribute('x'))-p.x)>=68;})()"), "mobile edge labels should sit beside vertical paths");
  assert.ok(await c.eval("(()=>{const svg=document.querySelector('[data-flow-topology]'),label=document.querySelector('[data-flow-edge] [data-flow-label-bg]'),x=Number(label.getAttribute('x')),w=Number(label.getAttribute('width'));return x>=0&&x+w<=svg.viewBox.baseVal.width;})()"), "mobile edge labels should remain fully inside the topology canvas");
  const mobile = await c.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  const mobilePath = path.join(os.tmpdir(), "matrix-features-mobile.png");
  fs.writeFileSync(mobilePath, Buffer.from(mobile.data, "base64"));
  await c.eval("location.hash='#/netpol/graph'");
  await c.until("document.querySelector('[data-flow-topology]')?.viewBox.baseVal.width===360 && document.querySelectorAll('[data-flow-edge]').length>=3", "mobile policy topology did not render");
  assert.ok(await c.eval("(()=>[...document.querySelectorAll('[data-flow-label-bg]')].every(label=>{const x=Number(label.getAttribute('x')),w=Number(label.getAttribute('width'));return x>=0&&x+w<=360;}))()"), "mobile policy labels should remain fully inside the topology canvas");
  const mobilePolicy = await c.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  const mobilePolicyPath = path.join(os.tmpdir(), "matrix-netpol-mobile.png");
  fs.writeFileSync(mobilePolicyPath, Buffer.from(mobilePolicy.data, "base64"));
  assert.equal(await c.eval("document.querySelector('.x-topo--flow').scrollWidth <= document.querySelector('.x-topo--flow').clientWidth+1"), true, "mobile flow graph is clipped horizontally");
  assert.equal(await c.eval("document.documentElement.scrollWidth <= innerWidth+2"), true, "mobile viewport overflows");
  console.log("SCREENSHOTS", desktopPath, mobilePath, desktopPolicyPath, mobilePolicyPath);
  assert.deepEqual(c.errors, []);
  console.log("PASS all browser checks; API writes were local mocks only");
  await c.send("Network.enable");
  await c.send("Network.setBlockedURLs", { urls: ["*xs-live-core.js*"] });
  await c.send("Page.navigate", { url: base + "/?core-script-blocked=1#/cluster/storage/2" });
  await c.until("window.XS?.mode==='live' && document.querySelector('#view')?.innerText.includes('该页面尚未接入真实数据接口')",
    "cluster route should fail closed when the live core script is unavailable");
  const fallbackText = await c.eval("document.querySelector('#view').innerText");
  assert.doesNotMatch(fallbackText, /web-config|mysql-root|prod-bj|node-1|frontend-should-never-render-secret/,
    "missing live core must not fall through to bundled cluster or secret fixtures");
  assert.equal(await c.eval("!document.querySelector('[data-live-cluster-select]') && !document.querySelector('#view form')"), true,
    "missing live core must show an unavailable state, not a stale cluster editor");
  console.log("PASS cluster routes fail closed when xs-live-core.js is blocked");
} finally {
  if (c) {
    try { await c.send("Browser.close"); } catch {}
    c.ws.close();
  }
  chrome.kill();
  await new Promise((resolve) => server.close(resolve));
  for (let i = 0; i < 10; i++) {
    try { fs.rmSync(profile, { recursive: true, force: true }); break; } catch { await sleep(200); }
  }
}
