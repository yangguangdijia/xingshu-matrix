/* Real observability data normalization. No generated series or sample records. */
(function (root) {
  "use strict";
  const items = (r) => Array.isArray(r) ? r : (r && r.items) || [];
  const quote = (s) => JSON.stringify(String(s));
  const regex = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const env = (v) => ({ development: "dev", 开发: "dev", production: "prod", 生产: "prod", 预发: "staging", pre: "staging", 测试: "test", testing: "test", qa: "test" }[String(v).toLowerCase()] || String(v).toLowerCase());
  function datasource(list, clusters, cluster, types) {
    if (!cluster) return null;
    const candidates = list.filter((d) => types.includes(d.type) && env(d.env) === env(cluster.env));
    const owned = candidates.filter((d) => {
      const o = d.options || {};
      return String(o.cluster_id || "") === String(cluster.id) || o.cluster_name === cluster.name;
    });
    if (owned.length === 1) return { ds: owned[0], dedicated: true };
    if (owned.length > 1) throw new Error("当前集群存在多个同类数据源，请在数据源中明确唯一归属");
    const shared = candidates.filter((d) => !(d.options || {}).cluster_id && !(d.options || {}).cluster_name);
    if (!shared.length) return null;
    if (shared.length !== 1) throw new Error("当前环境存在多个同类数据源，请设置 options.cluster_id 或 cluster_name");
    return { ds: shared[0], dedicated: clusters.filter((c) => env(c.env) === env(cluster.env)).length === 1 };
  }
  function metricQueries(nodes, pods, ns, app) {
    const ips = nodes.map((n) => n.internal_ip).filter(Boolean).map((ip) => regex((ip.includes(":") ? "[" + ip + "]" : ip) + ":9100"));
    if (!ips.length) throw new Error("集群没有可用于关联 node-exporter 的节点地址");
    const selector = 'instance=~' + quote(ips.join("|"));
    const node = nodes.map((n) => regex(n.name)).join("|");
    const names = pods.filter((p) => (!ns || p.namespace === ns) && (!app || (p.labels || {}).app === app || (p.labels || {})["app.kubernetes.io/name"] === app)).map((p) => regex(p.name));
    const workload = !!(ns || app);
    const ps = 'namespace=~' + quote(ns ? regex(ns) : ".+") + ',pod=~' + quote(names.length ? names.join("|") : "a^");
    const match = 'namespace=~' + quote(ns ? regex(ns) : ".+") + ',node=~' + quote(node);
    const join = ' * on(namespace,pod) group_left() max by(namespace,pod) (kube_pod_info{' + match + '})';
    const container = ps + ',container!="",container!="POD",image!=""';
    return {
      cpu: workload ? '100 * sum(rate(container_cpu_usage_seconds_total{' + container + '}[5m])' + join + ') / sum(kube_node_status_allocatable{resource="cpu",node=~' + quote(node) + '})' : '100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle",' + selector + '}[5m])))',
      mem: workload ? '100 * sum(container_memory_working_set_bytes{' + container + '}' + join + ') / sum(kube_node_status_allocatable{resource="memory",node=~' + quote(node) + '})' : '100 * (1 - sum(node_memory_MemAvailable_bytes{' + selector + '}) / sum(node_memory_MemTotal_bytes{' + selector + '}))',
      rx: 'sum(rate(node_network_receive_bytes_total{device!="lo",' + selector + '}[5m])) / 1000000',
      tx: 'sum(rate(node_network_transmit_bytes_total{device!="lo",' + selector + '}[5m])) / 1000000',
      disk: '100 * (1 - sum(node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|overlay",' + selector + '}) / sum(node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay",' + selector + '}))',
      rst: 'sum(kube_pod_container_status_restarts_total{' + ps + '}' + join + ')'
    };
  }
  function matrix(raw) {
    const data = raw && raw.data || raw || {};
    return (data.result || []).map((s) => ({ labels: s.metric || {}, values: (s.values || (s.value ? [s.value] : [])).map(([t, v]) => [Number(t), Number.isFinite(Number(v)) ? Number(v) : null]) }));
  }
  function logQuery(scope) {
    if (!scope) return null;
    const o = scope.ds.options || {};
    let selector = o.log_selector || '{job="fluent-bit"}';
    if (!/^\{[^{}]+\}$/.test(selector)) throw new Error("Loki log_selector 必须是标签选择器");
    if (!scope.dedicated) {
      const label = o.cluster_label || "cluster";
      if (!/^[a-zA-Z_][a-zA-Z0-9_]*$/.test(label)) throw new Error("Loki 集群标签名无效");
      selector = selector.slice(0, -1) + "," + label + "=" + quote(scope.cluster.name) + "}";
    }
    return selector;
  }
  function normalizeLog(row, cluster) {
    const labels = row.labels || {};
    const raw = String(row.message || "");
    let data = {}, inner = raw;
    try { data = JSON.parse(raw); if (!data || typeof data !== "object" || Array.isArray(data)) data = {}; } catch {}
    inner = String(data.message || data.msg || data.log || raw).trim();
    try { const nested = JSON.parse(inner); if (nested && typeof nested === "object") { data = { ...data, ...nested }; inner = String(nested.message || nested.msg || inner); } } catch {}
    const kub = data.kubernetes || {};
    const lvRaw = String(data.level || data.severity || row.level || "").toUpperCase();
    let lv = /ERR|FATAL|CRIT|PANIC/.test(lvRaw) ? "ERROR" : /WARN/.test(lvRaw) ? "WARN" : /DEBUG|TRACE/.test(lvRaw) ? "DEBUG" : "INFO";
    const klog = inner.match(/^([IWEF])\d{4}\s+[\d:.]+\s+\d+\s+([^\]]+)\]\s*([\s\S]*)$/);
    if (klog) { lv = { I: "INFO", W: "WARN", E: "ERROR", F: "ERROR" }[klog[1]]; data.source = klog[2]; inner = klog[3]; }
    const logfmt = Object.fromEntries([...inner.matchAll(/(?:^|\s)([\w.]+)=("(?:\\.|[^"\\])*"|[^\s]+)/g)].map((m) => { let v = m[2]; try { v = JSON.parse(v); } catch {} return [m[1], v]; }));
    if (logfmt.msg || logfmt.message) { data = { ...data, ...logfmt }; inner = String(logfmt.msg || logfmt.message); }
    else if (Object.keys(logfmt).length) { data = { ...data, ...logfmt }; inner = `${inner.match(/^"[^"]+"/)?.[0] || inner.split(/\s+[\w.]+=.*$/s)[0]}（${Object.entries(logfmt).map(([k,v]) => k + "=" + v).join("，")}）`; }
    if (logfmt.level) lv = /error|fatal/i.test(logfmt.level) ? "ERROR" : /warn/i.test(logfmt.level) ? "WARN" : /debug|trace/i.test(logfmt.level) ? "DEBUG" : "INFO";
    const method = data.method || data.http_method || (data.request || {}).method;
    const requestPath = data.path || data.uri || data.url || (data.request || {}).path;
    const status = data.status || data.status_code || data.code;
    const latency = data.latency_ms ?? data.duration_ms ?? data.elapsed_ms;
    if (method && requestPath) {
      inner = `HTTP ${method} ${requestPath}${status != null ? " → " + status : ""}${latency != null ? "，耗时 " + latency + " 毫秒" : ""}${data.error ? "   " + data.error : ""}`;
      if (Number(status) >= 500) lv = "ERROR"; else if (Number(status) >= 400) lv = "WARN";
    }
    const access = inner.match(/"(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(\S+)\s+HTTP\/[\d.]+"\s+(\d{3})\s+(\d+|-)/);
    if (access) { data.method = access[1]; data.path = access[2]; data.status = +access[3]; data.response_bytes = access[4]; inner = `HTTP ${access[1]} ${access[2]} → ${access[3]}（${+access[3] >= 500 ? "服务端错误" : +access[3] >= 400 ? "请求未成功" : +access[3] >= 300 ? "重定向" : "成功"}），响应 ${access[4]} 字节`; if (+access[3] >= 500) lv = "ERROR"; else if (+access[3] >= 400) lv = "WARN"; }
    const reason = /connection refused/i.test(inner) ? "连接被拒绝" : /context deadline exceeded|i\/o timeout|timed out/i.test(inner) ? "请求或连接超时" : /no such host/i.test(inner) ? "域名解析失败" : /permission denied|forbidden/i.test(inner) ? "访问权限不足" : /CrashLoopBackOff/.test(inner) ? "容器反复崩溃，正在等待重启" : /device .+ is unhealthy on this node/i.test(inner) ? "该节点的设备健康检查未通过" : /Device Registered/i.test(inner) ? "设备资源已注册" : /failed to (?:watch|list)/i.test(inner) ? "读取或监听 Kubernetes 资源失败" : "";
    if (reason) inner = reason + "   " + inner;
    const app = labels.container || kub.container_name || row.service || data.service || "—";
    return { ts: new Date(row.ts), lv, app, cluster: cluster.name, msg: inner || "（空日志）", json: { ...labels, ...data, cluster: cluster.name, cluster_id: cluster.id, level: lv, app, namespace: labels.namespace || kub.namespace_name || "", pod: labels.pod || kub.pod_name || "", msg: inner, raw } };
  }
  function traceRecord(raw, cluster) {
    const source = raw.spans || [];
    const start = source.length ? Math.min(...source.map((s) => +new Date(s.start))) : +new Date(raw.start);
    const ordered = [], pending = [...source].sort((a, b) => +new Date(a.start) - +new Date(b.start)), seen = new Set();
    function visit(s, depth, parent) {
      if (seen.has(s.span_id)) return;
      seen.add(s.span_id); const i = ordered.length;
      ordered.push({ svc: s.service, op: s.operation, kind: (s.tags || {})["span.kind"] || "internal", depth, parent, start: (+new Date(s.start) - start), dur: s.duration_us / 1000, err: !!s.error, tags: s.tags || {}, logs: [] });
      pending.filter((c) => c.parent_id === s.span_id).forEach((c) => visit(c, depth + 1, i));
    }
    pending.filter((s) => !pending.some((p) => p.span_id === s.parent_id)).forEach((s) => visit(s, 0, -1));
    pending.forEach((s) => visit(s, 0, -1));
    const spans = ordered.length ? ordered : [{ svc: raw.root_service, op: raw.root_operation, start: 0, dur: raw.duration_ms, err: !!raw.error, tags: {}, logs: [], parent: -1, depth: 0, kind: "server" }];
    return { id: raw.trace_id, ts: new Date(start), cluster: cluster.name, op: raw.root_operation || spans[0].op, dur: source.length ? Math.max(...spans.map((s) => s.start + s.dur)) : raw.duration_ms, err: !!raw.error || spans.some((s) => s.err), spans, spanCount: raw.span_count || spans.length, summaryOnly: !source.length, svcs: raw.services || [...new Set(spans.map((s) => s.svc))] };
  }
  // Resolve alert location only from explicit ownership or unique inventory matches.
  // A node-exporter Pod is the collector, not the affected Pod of a node alert.
  function alertLocation(alert, clusters, datasources, inventories = []) {
    const l = alert.labels || {}, ds = datasources.find((d) => String(d.id) === String(alert.datasource_id)), options = ds && ds.options || {};
    const clusterName = l.cluster || l.cluster_name || l["k8s.cluster.name"], clusterId = l.cluster_id || l["k8s.cluster.uid"];
    let candidates = clusters.filter((c) => !alert.env || env(c.env) === env(alert.env)), basis = "同环境唯一集群";
    if (clusterName || clusterId) { candidates = candidates.filter((c) => (!clusterName || c.name === clusterName || String(c.id) === String(clusterName)) && (!clusterId || String(c.id) === String(clusterId))); basis = "告警集群标签"; }
    else if (options.cluster_id || options.cluster_name) { candidates = candidates.filter((c) => (!options.cluster_id || String(c.id) === String(options.cluster_id)) && (!options.cluster_name || c.name === options.cluster_name)); basis = "数据源集群归属"; }
    const namespace = l.namespace || l.exported_namespace || l["k8s.namespace.name"] || "", podName = l.pod || l.pod_name || l["k8s.pod.name"] || "";
    const nodeName = l.node || l.nodename || l["k8s.node.name"] || "", instance = l.instance || "";
    const ip = instance.startsWith("[") ? instance.slice(1, instance.indexOf("]")) : instance.replace(/:\d+$/, "");
    const matches = candidates.map((c) => {
      const inv = inventories.find((i) => String(i.clusterId) === String(c.id)) || {};
      const pods = (inv.pods || []).filter((p) => podName && p.name === podName && (!namespace || p.namespace === namespace));
      const nodes = (inv.nodes || []).filter((n) => nodeName ? n.name === nodeName : ip && n.internal_ip === ip);
      return { cluster: c, pod: pods.length === 1 ? pods[0] : null, node: nodes.length === 1 ? nodes[0] : null, error: inv.error };
    });
    if (candidates.length > 1) {
      const unique = matches.filter((m) => m.pod || m.node);
      if (unique.length === 1) { candidates = [unique[0].cluster]; basis = "告警资源与集群清单唯一匹配"; }
    }
    const cluster = candidates.length === 1 ? candidates[0] : null, match = matches.find((m) => cluster && m.cluster.id === cluster.id) || {};
    const nodeAlert = /^Node/.test(alert.alertname || "") || l.job === "node-exporter";
    const pod = match.pod, node = match.node;
    let type = "Namespace", name = namespace || "未提供资源标签";
    if (nodeAlert || nodeName) { type = "Node"; name = node && node.name || nodeName || instance || "未提供节点"; }
    else if (podName) { type = "Pod"; name = podName; }
    else {
      const kinds = [["persistentvolumeclaim", "PersistentVolumeClaim"], ["persistentvolume", "PersistentVolume"], ["deployment", "Deployment"], ["statefulset", "StatefulSet"], ["daemonset", "DaemonSet"], ["service", "Service"], ["job_name", "Job"], ["job", "Job"]];
      if (l.service) { type = "Service"; name = l.service; }
      const target = kinds.find(([key]) => l[key]); if (target) { type = target[1]; name = l[target[0]]; }
    }
    return { cluster, candidates, basis, namespace, type, name, pod, node, podName, nodeName: node && node.name || pod && pod.node || nodeName, instance, nodeAlert, inventoryError: match.error || "", note: !cluster ? "无法唯一确认集群，请为数据源配置 cluster_id 或为告警增加 cluster 标签。" : type === "Namespace" ? "此事件仅定位到命名空间，原始告警未提供 Pod 或其他具体资源；不能据此认定某个 Pod 故障。" : podName && !pod ? "原始标签中的 Pod 当前未在清单中找到，可能已删除或重建；保留事件触发时的资源名称。" : "" };
  }
  const alertWorkloadLabels = [
    ["deployment", "deployments", "Deployment"],
    ["k8s.deployment.name", "deployments", "Deployment"],
    ["statefulset", "statefulsets", "StatefulSet"],
    ["k8s.statefulset.name", "statefulsets", "StatefulSet"],
    ["daemonset", "daemonsets", "DaemonSet"],
    ["k8s.daemonset.name", "daemonsets", "DaemonSet"]
  ];
  function alertWorkloadCandidates(alert) {
    const labels = alert && alert.labels || {}, found = new Map();
    alertWorkloadLabels.forEach(([key, kind, kindLabel]) => {
      const name = String(labels[key] || "").trim();
      if (name) found.set(kind + "/" + name, { kind, kindLabel, name });
    });
    return [...found.values()];
  }
  function alertRemediationTarget(alert, cluster, workloads) {
    if (!alert || alert.status !== "firing" || !cluster || cluster.id == null) return null;
    const candidates = alertWorkloadCandidates(alert), labels = alert.labels || {};
    const namespace = labels.namespace || labels.exported_namespace || labels["k8s.namespace.name"] || "";
    if (candidates.length !== 1 || !namespace) return null;
    const candidate = candidates[0];
    const matches = items(workloads && workloads[candidate.kind]).filter((row) =>
      row && row.name === candidate.name && row.namespace === namespace
    );
    if (matches.length !== 1) return null;
    const row = matches[0];
    return {
      clusterId: cluster.id, clusterName: cluster.name, env: cluster.env,
      namespace, kind: candidate.kind, kindLabel: candidate.kindLabel, name: candidate.name,
      ready: row.ready || "", replicas: row.replicas == null ? null : row.replicas, status: row.status || ""
    };
  }
  function redactDiagnosticText(value) {
    return String(value == null ? "" : value)
      .replace(/\b(authorization)\b(\s*[:=]\s*)(bearer\s+)?[^\s,;]+/gi, (_, key, sep, scheme) => key + sep + (scheme || "") + "[REDACTED]")
      .replace(/\b(bearer)\s+[A-Za-z0-9._~+/=-]+/gi, "$1 [REDACTED]")
      .replace(/\b(password|passwd|token|secret|api[_-]?key)\b(\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)/gi, "$1$2[REDACTED]")
      .slice(0, 1200);
  }
  function alertBelongsToScope(alert, scope) {
    const labels = alert.labels || {}, targetId = String(scope.targetId || ""), target = scope.targetCluster || null;
    const source = (scope.datasources || []).find((d) => String(d.id) === String(alert.datasource_id));
    const options = source && source.options || {}, sourceId = options.cluster_id, sourceName = options.cluster_name;
    if (sourceId != null && String(sourceId) !== targetId) return false;
    if (sourceName && (!target || String(sourceName) !== String(target.name))) return false;
    const clusterLabel = labels.cluster || labels.cluster_name || labels.cluster_id || labels["k8s.cluster.name"] || labels["k8s.cluster.uid"];
    if (clusterLabel) {
      const value = String(clusterLabel);
      if (value !== targetId && (!target || value !== String(target.name))) return false;
    } else if (sourceId == null && !sourceName && (!target || scope.environmentClusterCount !== 1)) return false;
    if (labels.namespace && scope.namespace && labels.namespace !== scope.namespace) return false;
    const appLabels = [labels.app, labels.application, labels.app_name, labels["app.kubernetes.io/name"], labels["service.name"]].filter(Boolean).map(String);
    if (appLabels.some((value) => value === scope.appName)) return true;
    const resources = [labels.pod, labels.pod_name, labels.service, labels.deployment, labels.statefulset, labels.daemonset, labels.job, labels.job_name].filter(Boolean).map(String);
    return resources.some((value) => (scope.pods || []).indexOf(value) >= 0 || (scope.services || []).indexOf(value) >= 0 || (scope.workloads || []).indexOf(value) >= 0);
  }
  root.XSObserve = { items, env, datasource, metricQueries, matrix, logQuery, normalizeLog, traceRecord, alertLocation, alertWorkloadCandidates, alertRemediationTarget, redactDiagnosticText, alertBelongsToScope, quote, regex };
  if (typeof module !== "undefined") module.exports = root.XSObserve;
})(typeof window === "undefined" ? globalThis : window);
