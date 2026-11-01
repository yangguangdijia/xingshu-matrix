import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const extensionSource = readFileSync(new URL("../../public/xs-cluster-extensions.js", import.meta.url), "utf8");
const coreSource = readFileSync(new URL("../../public/xs-live-core.js", import.meta.url), "utf8");

function loadExtension(extra = {}) {
  const sandbox = { window: {}, ...extra };
  sandbox.window.document = sandbox.document;
  vm.runInNewContext(extensionSource, sandbox, { filename: "xs-cluster-extensions.js" });
  return sandbox.window.XSClusterExtensions;
}

test("extension registration preserves node group controls and installs RBAC/backup renderers", () => {
  const extension = loadExtension();
  const nodePools = () => {};
  const body = {
    nodepools: nodePools,
    rbac() {},
    backup() {}
  };

  assert.equal(extension.register({ body, apiGet() {} }), true);
  assert.equal(body.nodepools, nodePools);
  assert.equal(body.rbac.name, "renderRBAC");
  assert.equal(body.backup.name, "renderBackup");
  assert.equal(extension.register({ body, apiGet() {} }), true);
  assert.equal(body.rbac.name, "renderRBAC");
  assert.match(coreSource, /"nodegroup-name"/);
  assert.match(coreSource, /设置节点分组名称/);
  assert.match(coreSource, /"nodegroup-taint-add"/);
  assert.match(coreSource, /connectClusterExtensions\(\);\s*LIVE\.routes/);
  assert.match(coreSource, /data-xs-cluster-extensions/);
});

test("RBAC route uses declared API methods and renders structured editing controls", () => {
  for (const kind of ["ServiceAccount", "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding"]) {
    assert.ok(extensionSource.includes(kind));
  }
  assert.match(extensionSource, /\/rbac"\)/);
  assert.match(extensionSource, /\/rbac\/apply/);
  assert.match(extensionSource, /\/rbac\/"\s*\+/);
  assert.match(extensionSource, /data-rbac-create/);
  assert.match(extensionSource, /data-rbac-edit/);
  assert.match(extensionSource, /data-rbac-delete/);
  assert.match(coreSource, /生成后请检查，再单独提交/);
  assert.doesNotMatch(extensionSource, /name="yaml"/);
  assert.doesNotMatch(extensionSource, /完整且有效的 Kubernetes RBAC YAML/);
  assert.match(extensionSource, /rowsHtml\("rules", "权限规则"/);
  assert.match(extensionSource, /rowsHtml\("subjects", "绑定主体"/);
  assert.match(extensionSource, /高级字段/);
  assert.match(extensionSource, /RBAC 配置已提交/);
  assert.match(extensionSource, /当前服务端未提供集群 RBAC 读取接口/);
});

test("workload creation uses structured fields, permissions, dry-run and apply result checks", () => {
  for (const kind of ["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"]) {
    assert.ok(extensionSource.includes(kind));
  }
  assert.match(coreSource, /decorateWorkloads\(\{\s*el: el, seq: seq, cluster: cur/);
  assert.match(extensionSource, /data-wl-create/);
  assert.match(extensionSource, /querySelector\("\.x-tbar"\)/);
  assert.match(extensionSource, /can\("workload:write"\)/);
  assert.doesNotMatch(extensionSource, /最终 YAML（可查看和编辑）/);
  assert.doesNotMatch(extensionSource, /name="yaml"/);
  assert.match(extensionSource, /data-check-yaml/);
  assert.match(extensionSource, /dry_run: true/);
  assert.match(extensionSource, /force: false/);
  assert.match(extensionSource, /result\.failed/);
  assert.match(extensionSource, /objects\.filter/);
  assert.match(extensionSource, /StatefulSet Service/);
  assert.match(extensionSource, /restart_policy/);
});

function control(value) {
  return { value: String(value ?? ""), checked: false };
}

function workloadForm(values) {
  const elements = {};
  for (const [name, value] of Object.entries(values)) elements[name] = control(value);
  return { elements };
}

test("workload manifest generator emits valid required fields for every supported kind", () => {
  const extension = loadExtension();
  const common = {
    namespace: "prod",
    name: "orders",
    image: "registry.example/orders:1.2.3",
    replicas: "2",
    container_port: "8080",
    schedule: "0 2 * * *",
    timezone: "Asia/Shanghai",
    service_name: "orders",
    restart_policy: "Never"
  };

  const manifests = {};
  for (const kind of ["Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"]) {
    manifests[kind] = extension.buildWorkloadManifest(workloadForm({ ...common, kind }));
    assert.equal(manifests[kind].kind, kind);
    assert.equal(manifests[kind].metadata.name, "orders");
    assert.equal(manifests[kind].metadata.namespace, "prod");
    const podSpec = kind === "Pod" ? manifests[kind].spec :
      kind === "CronJob" ? manifests[kind].spec.jobTemplate.spec.template.spec : manifests[kind].spec.template.spec;
    assert.equal(podSpec.containers[0].image, common.image);
  }
  assert.equal(manifests.StatefulSet.spec.serviceName, "orders");
  assert.equal(manifests.Job.spec.template.spec.restartPolicy, "Never");
  assert.equal(manifests.CronJob.spec.jobTemplate.spec.template.spec.restartPolicy, "Never");
  assert.equal(manifests.CronJob.spec.schedule, common.schedule);
  assert.equal(manifests.CronJob.spec.timeZone, common.timezone);
  assert.equal(manifests.DaemonSet.spec.replicas, undefined);
});

test("workload manifest generator rejects invalid names, ports, cron and restart policies", () => {
  const extension = loadExtension();
  const base = {
    kind: "Deployment",
    namespace: "prod",
    name: "orders",
    image: "nginx:stable",
    replicas: "1",
    container_port: "8080",
    schedule: "0 * * * *",
    timezone: "",
    service_name: "orders",
    restart_policy: "Never"
  };
  assert.throws(() => extension.buildWorkloadManifest(workloadForm({ ...base, name: "Bad_Name" })), /名称/);
  assert.throws(() => extension.buildWorkloadManifest(workloadForm({ ...base, container_port: "70000" })), /端口/);
  assert.throws(() => extension.buildWorkloadManifest(workloadForm({ ...base, kind: "CronJob", schedule: "not cron" })), /Cron/);
  assert.throws(() => extension.buildWorkloadManifest(workloadForm({ ...base, kind: "Job", restart_policy: "Always" })), /重启策略/);
});

test("apply result checker distinguishes success, partial failure and approval", () => {
  const extension = loadExtension();
  assert.equal(extension.checkApplyResult({ failed: 0, objects: [{ status: "applied", name: "orders" }] }), "");
  assert.match(extension.checkApplyResult({
    failed: 1,
    objects: [{ status: "failed", name: "orders", error: "conflict" }]
  }), /orders.*conflict/);
  assert.match(extension.checkApplyResult({
    failed: 0,
    objects: [{ status: "rejected", name: "orders" }]
  }), /orders.*rejected/);
  assert.match(extension.checkApplyResult({
    status: 202,
    approval: { ticket: { id: 42 } }
  }), /审批单 #42/);
  assert.match(extension.checkApplyResult({
    failed: 0,
    objects: [{ status: "applied", name: "orders" }]
  }), /^$/);
});

test("workload create button uses x-tbar and is gated by workload:write", () => {
  function makeBox() {
    const toolbar = {
      children: [],
      querySelector(selector) {
        return selector === "[data-wl-create]" ? this.children.find((item) => item.attributes["data-wl-create"]) : null;
      },
      appendChild(button) { this.children.push(button); }
    };
    return {
      toolbar,
      querySelector(selector) { return selector === ".x-tbar" ? toolbar : null; }
    };
  }
  const document = {
    createElement() {
      return {
        attributes: {},
        setAttribute(name, value) { this.attributes[name] = value; }
      };
    }
  };
  const extension = loadExtension({ document });
  extension.register({
    body: {},
    apiGet() {},
    xs() { return { can() { return false; } }; }
  });
  const readonly = makeBox();
  extension.decorateWorkloads({ box: readonly });
  assert.equal(readonly.toolbar.children.length, 0);

  extension.register({
    body: {},
    apiGet() {},
    xs() { return { can(permission) { return permission === "workload:write"; } }; }
  });
  const writable = makeBox();
  extension.decorateWorkloads({ box: writable, cluster: { id: 1 } });
  assert.equal(writable.toolbar.children.length, 1);
  assert.equal(writable.toolbar.children[0].attributes["data-wl-create"], "1");
});

test("backup page presents discovered resources and supports snapshot and Velero schedule forms", () => {
  assert.match(extensionSource, /\/backups"\)/);
  assert.match(extensionSource, /\/backups\/apply/);
  assert.match(extensionSource, /\/storage\/snapshots/);
  assert.match(extensionSource, /data-backup-tab="snapshots"/);
  assert.match(extensionSource, /data-backup-tab="velero"/);
  assert.match(extensionSource, /data-snapshot-create/);
  assert.match(extensionSource, /data-schedule-create/);
  assert.match(extensionSource, /Velero 备份接口不可用/);
  assert.match(extensionSource, /不会显示模拟数据/);
});

test("discovered component options are selectable and cached from live cluster APIs", async () => {
  const extension = loadExtension();
  const body = {};
  const fakeRuntime = {
    body,
    apiGet(path) {
      if (path.endsWith("/storage/classes")) return Promise.resolve({ items: [{ name: "fast-ssd" }] });
      if (path.endsWith("/storage/capabilities")) return Promise.resolve({ snapshots: { classes: ["csi-snap"] } });
      if (path.endsWith("/network/configuration")) return Promise.resolve({
        resources: [{ kind: "IngressClass", items: [{ metadata: { name: "nginx" } }] }]
      });
      if (path.endsWith("/backups")) return Promise.resolve({ storage_locations: [{ name: "primary" }] });
      return Promise.resolve({ items: [] });
    },
    itemsOf(data) {
      return Array.isArray(data) ? data : data && (data.items || data.data) || [];
    }
  };
  assert.equal(extension.register(fakeRuntime), true);
  await extension.ensureComponents("cluster-1");
  assert.deepEqual(Array.from(extension.componentOptions("cluster-1", "storageClasses")), ["fast-ssd"]);
  assert.deepEqual(Array.from(extension.componentOptions("cluster-1", "volumeSnapshotClasses")), ["csi-snap"]);
  assert.deepEqual(Array.from(extension.componentOptions("cluster-1", "ingressClasses")), ["nginx"]);
  assert.deepEqual(Array.from(extension.componentOptions("cluster-1", "veleroStorageLocations")), ["primary"]);
  assert.match(extensionSource, /optionsHtml\(locations/);
  assert.match(extensionSource, /optionsHtml\(WORKLOAD_KINDS, kind, "kind"\)/);
  assert.match(extensionSource, /optionsHtml\(values, selected, "namespace"/);
});

test("extension script is only wired through xs-live-core and no forbidden page modules are referenced", () => {
  assert.match(coreSource, /new URL\("xs-cluster-extensions\.js", base\)/);
  assert.match(coreSource, /XSClusterExtensions\.decorateWorkloads/);
  assert.match(coreSource, /XSClusterExtensions\.ensureComponents/);
  assert.doesNotMatch(extensionSource, /xs-live-ops\.js|xs-network-config\.js|index\.html/);
});
