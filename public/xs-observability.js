/* Real observability data normalization. No generated series or sample records. */
(function (root) {
  "use strict";
  const items = (r) => Array.isArray(r) ? r : (r && r.items) || [];
  const quote = (s) => JSON.stringify(String(s));
  const regex = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const env = (v) => ({ development: "dev", 开发: "dev", production: "prod", 生产: "prod", 预发: "staging", pre: "staging", 测试: "test", testing: "test", qa: "test" }[String(v).toLowerCase()] || String(v).toLowerCase());
  const validLabel = (label) => /^[a-zA-Z_][a-zA-Z0-9_]*$/.test(label);
  function sourceScope(source, clusters, cluster) {
    const o = source.options || {};
    let scope = o.scope;
    if (!scope && (o.cluster_id || o.cluster_name)) scope = "cluster";
    if (scope !== "cluster" && scope !== "central") return null;
    if (scope === "cluster") {
      if (!o.cluster_id || env(source.env) !== env(cluster.env)) return null;
      const owner = clusters.find((c) => String(c.id) === String(o.cluster_id));
      if (o.cluster_name && (!owner || owner.name !== o.cluster_name)) throw new Error("数据源 cluster_id 与 cluster_name 不一致");
      return String(o.cluster_id) === String(cluster.id) ? { ds: source, scope, dedicated: true, cluster } : null;
    }
    if (o.cluster_id || o.cluster_name) throw new Error("中央数据源不能同时设置单集群归属");
    if (source.env && env(source.env) !== env(cluster.env)) return null;
    const values = o.cluster_values;
    if (!values || typeof values !== "object" || Array.isArray(values)) throw new Error("中央数据源缺少 cluster_values 集群映射");
    if (!Object.prototype.hasOwnProperty.call(values, String(cluster.id))) return null;
    const clusterValue = values[String(cluster.id)], clusterLabel = o.cluster_label;
    const traceLabel = /^(?:jaeger|tempo)$/.test(source.type) && /^[a-zA-Z_][a-zA-Z0-9_.]*$/.test(clusterLabel);
    if (typeof clusterValue !== "string" || !clusterValue.trim() || !(validLabel(clusterLabel) || traceLabel)) throw new Error("中央数据源缺少有效的集群标签映射");
    return { ds: source, scope, dedicated: false, cluster, clusterValue, clusterLabel };
  }
  function datasource(list, clusters, cluster, types) {
    if (!cluster) return null;
    const candidates = (list || []).filter((d) => types.includes(d.type));
    const matching = candidates.map((d) => sourceScope(d, clusters || [], cluster)).filter(Boolean);
    const owned = matching.filter((d) => d.scope === "cluster");
    if (owned.length === 1) return owned[0];
    if (owned.length > 1) throw new Error("当前集群存在多个同类数据源，请在数据源中明确唯一归属");
    const shared = matching.filter((d) => d.scope === "central");
    if (shared.length > 1) throw new Error("当前集群存在多个中央数据源，归属不明确");
    return shared[0] || null;
  }
  function resolvedDatasource(result, clusters, cluster, type) {
    if (!cluster || cluster.id == null) throw new Error("请选择目标集群");
    const outer = result && result.data && typeof result.data === "object" ? result.data : result || {};
    const source = outer.datasource || (outer.data && outer.data.datasource) || outer;
    if (!source || source.id == null) throw new Error("数据源解析接口未返回数据源");
    if (source.type !== type) throw new Error("数据源解析接口返回了不匹配的类型");
    const scope = outer.scope || (outer.data && outer.data.scope) || (source.options || {}).scope;
    if (scope !== "cluster" && scope !== "central") throw new Error("数据源解析接口未返回有效归属");
    const options = Object.assign({}, source.options || {}, { scope });
    const clusterId = outer.cluster_id != null ? outer.cluster_id : outer.data && outer.data.cluster_id;
    if (scope === "cluster") {
      if (options.cluster_id == null && clusterId != null) options.cluster_id = clusterId;
      if (options.cluster_name == null) options.cluster_name = cluster.name || "";
    }
    const normalized = Object.assign({}, source, { options });
    const ownership = sourceScope(normalized, clusters || [cluster], cluster);
    if (!ownership) throw new Error("解析到的数据源不属于当前集群");
    return Object.assign({}, ownership, { resolved: true });
  }
  function clusterMatcher(selector, scope) {
    if (!scope || scope.scope !== "central") return selector;
    const label = scope.clusterLabel, value = scope.clusterValue;
    if (!validLabel(label) || typeof value !== "string" || !value.trim()) throw new Error("中央数据源缺少有效的集群标签映射");
    const parts = [], source = String(selector || "");
    let start = 0, quoted = false, escaped = false;
    for (let i = 0; i < source.length; i++) {
      if (escaped) { escaped = false; continue; }
      if (quoted && source[i] === "\\") { escaped = true; continue; }
      if (source[i] === '"') { quoted = !quoted; continue; }
      if (source[i] === "," && !quoted) { parts.push(source.slice(start, i)); start = i + 1; }
    }
    if (quoted || escaped) throw new Error("无法验证当前集群标签条件");
    if (source) parts.push(source.slice(start));
    let present = false;
    for (const part of parts) {
      const match = part.match(/^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!=|!~|=)\s*("(?:\\.|[^"\\])*")\s*$/);
      if (!match) throw new Error("无法验证当前集群标签条件");
      if (match[1] !== label) continue;
      let actual;
      try { actual = JSON.parse(match[3]); } catch { throw new Error("无法验证当前集群标签条件"); }
      if (present || match[2] !== "=" || actual !== value) throw new Error("查询条件与当前集群标签冲突");
      present = true;
    }
    return present ? source : source ? source + "," + label + "=" + quote(value) : label + "=" + quote(value);
  }
  function scopedPromQuery(query, scope, generated = false) {
    if (!scope || scope.scope !== "central") return query;
    if (!generated) {
      const single = String(query).trim().match(/^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^{}]*)\})?$/);
      if (!single) throw new Error("中央数据源仅支持可验证的单指标选择器；复杂 PromQL 请等待服务端隔离支持");
      return single[1] + "{" + clusterMatcher(single[2] || "", scope) + "}";
    }
    let count = 0;
    const result = query.replace(/([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^{}]*)\}/g, (_, name, selector) => {
      count++;
      return name + "{" + clusterMatcher(selector, scope) + "}";
    });
    if (!count) throw new Error("无法验证指标查询的集群范围");
    return result;
  }
  function metricQueries(nodes, pods, ns, app, scope) {
    nodes = nodes || [];
    pods = pods || [];
    const ips = nodes.map((n) => n.internal_ip).filter(Boolean).map((ip) => regex((ip.includes(":") ? "[" + ip + "]" : ip) + ":9100"));
    const instanceSelector = ips.length ? 'instance=~' + quote(ips.join("|")) : "";
    const withInstance = (selector) => selector + (selector && instanceSelector ? "," : "") + instanceSelector;
    const nodeNames = nodes.map((n) => n.name).filter(Boolean).map((name) => regex(name));
    const node = nodeNames.length ? nodeNames.join("|") : "a^";
    const names = pods.filter((p) => (!ns || p.namespace === ns) && (!app || (p.labels || {}).app === app || (p.labels || {})["app.kubernetes.io/name"] === app)).map((p) => regex(p.name));
    const workload = !!(ns || app);
    const ps = 'namespace=~' + quote(ns ? regex(ns) : ".+") + ',pod=~' + quote(names.length ? names.join("|") : "a^");
    const match = 'namespace=~' + quote(ns ? regex(ns) : ".+") + ',node=~' + quote(node);
    const join = ' * on(namespace,pod) group_left() max by(namespace,pod) (kube_pod_info{' + match + '})';
    const container = ps + ',container!="",container!="POD",image!=""';
    const queries = {
      cpu: workload ? '100 * sum(rate(container_cpu_usage_seconds_total{' + container + '}[5m])' + join + ') / sum(kube_node_status_allocatable{resource="cpu",node=~' + quote(node) + '})' : '100 * (1 - avg(rate(node_cpu_seconds_total{' + withInstance('mode="idle"') + '}[5m])))',
      mem: workload ? '100 * sum(container_memory_working_set_bytes{' + container + '}' + join + ') / sum(kube_node_status_allocatable{resource="memory",node=~' + quote(node) + '})' : '100 * (1 - sum(node_memory_MemAvailable_bytes{' + withInstance("") + '}) / sum(node_memory_MemTotal_bytes{' + withInstance("") + '}))',
      rx: 'sum(rate(node_network_receive_bytes_total{' + withInstance('device!="lo"') + '}[5m])) / 1000000',
      tx: 'sum(rate(node_network_transmit_bytes_total{' + withInstance('device!="lo"') + '}[5m])) / 1000000',
      disk: '100 * (1 - sum(node_filesystem_avail_bytes{' + withInstance('mountpoint="/",fstype!~"tmpfs|overlay"') + '}) / sum(node_filesystem_size_bytes{' + withInstance('mountpoint="/",fstype!~"tmpfs|overlay"') + '}))',
      rst: 'sum(kube_pod_container_status_restarts_total{' + ps + '}' + join + ')'
    };
    return Object.fromEntries(Object.entries(queries).map(([key, value]) => [key, scopedPromQuery(value, scope, true)]));
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
    selector = "{" + clusterMatcher(selector.slice(1, -1), scope) + "}";
    return selector;
  }
  function scopedLogQuery(input, scope) {
    if (!scope || scope.scope !== "central") return input;
    if (!String(input || "").trim()) return logQuery(scope);
    const parsed = String(input).trim().match(/^(\{[^{}]+\})(\s*(?:\|[=~!]\s*"(?:\\.|[^"\\])*"\s*)*)$/);
    if (!parsed) throw new Error("中央日志查询仅支持可验证的标签选择器和简单过滤条件");
    return "{" + clusterMatcher(parsed[1].slice(1, -1), scope) + "}" + parsed[2];
  }
  function traceTags(scope, tags = {}) {
    if (!scope || scope.scope !== "central") return { ...tags };
    const label = scope.clusterLabel, value = scope.clusterValue;
    if (!/^[a-zA-Z_][a-zA-Z0-9_.]*$/.test(label) || typeof value !== "string" || !value.trim()) throw new Error("中央数据源缺少有效的集群标签映射");
    if (Object.prototype.hasOwnProperty.call(tags, label) && tags[label] !== value) throw new Error("链路标签与所选集群冲突");
    return { ...tags, [label]: value };
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
    const explicitClusterValues = [l.cluster, l.cluster_name, l.cluster_id, l["k8s.cluster.name"], l["k8s.cluster.uid"]]
      .filter((value) => value !== undefined && value !== null && String(value).trim() !== "").map(String);
    const mappedValues = options.cluster_values;
    const matchesClusterValue = (cluster, value) => String(cluster.id) === value || String(cluster.name) === value ||
      !!(mappedValues && typeof mappedValues === "object" && !Array.isArray(mappedValues) &&
        Object.prototype.hasOwnProperty.call(mappedValues, String(cluster.id)) && String(mappedValues[String(cluster.id)]) === value);
    let candidates = clusters.filter((c) => !alert.env || env(c.env) === env(alert.env)), basis = "同环境唯一集群", ownershipFailure = "";
    if (options.scope === "central") {
      const label = options.cluster_label;
      const mappedValue = validLabel(label) ? l[label] : undefined;
      if (!validLabel(label) || !mappedValues || typeof mappedValues !== "object" || Array.isArray(mappedValues) ||
        typeof mappedValue !== "string" || !mappedValue.trim()) {
        candidates = [];
        ownershipFailure = "中央数据源告警缺少有效的集群标签映射，无法确认集群。";
      } else {
        const reverse = clusters.filter((c) => Object.prototype.hasOwnProperty.call(mappedValues, String(c.id)) &&
          typeof mappedValues[String(c.id)] === "string" && mappedValues[String(c.id)] === mappedValue);
        if (reverse.length !== 1) {
          candidates = [];
          ownershipFailure = reverse.length > 1 ? "中央数据源集群标签匹配多个集群，已拒绝推断。" : "中央数据源集群标签未匹配已接入集群，已拒绝推断。";
        } else if (alert.env && env(reverse[0].env) !== env(alert.env)) {
          candidates = [];
          ownershipFailure = "中央数据源集群标签与告警环境冲突，已拒绝推断。";
        } else if (explicitClusterValues.some((value) => !matchesClusterValue(reverse[0], value))) {
          candidates = [];
          ownershipFailure = "中央数据源集群标签与显式 cluster 标签冲突，已拒绝推断。";
        } else {
          candidates = reverse;
          basis = "中央数据源 " + label + " 标签映射";
        }
      }
    } else {
      if (options.cluster_id || options.cluster_name) {
        candidates = candidates.filter((c) => (!options.cluster_id || String(c.id) === String(options.cluster_id)) &&
          (!options.cluster_name || c.name === options.cluster_name));
        basis = "数据源集群归属";
      }
      if (explicitClusterValues.length) {
        candidates = candidates.filter((c) => explicitClusterValues.every((value) => matchesClusterValue(c, value)));
        basis = options.cluster_id || options.cluster_name ? "数据源归属与告警集群标签" : "告警集群标签";
      }
    }
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
    return { cluster, candidates, basis, namespace, type, name, pod, node, podName, nodeName: node && node.name || pod && pod.node || nodeName, instance, nodeAlert, inventoryError: match.error || "", note: !cluster ? ownershipFailure || "无法唯一确认集群，请为数据源配置 cluster_id 或为告警增加 cluster 标签。" : type === "Namespace" ? "此事件仅定位到命名空间，原始告警未提供 Pod 或其他具体资源；不能据此认定某个 Pod 故障。" : podName && !pod ? "原始标签中的 Pod 当前未在清单中找到，可能已删除或重建；保留事件触发时的资源名称。" : "" };
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
    if (!source || !target || String(target.id) !== targetId) return false;
    let ownership;
    try { ownership = sourceScope(source, scope.clusters || [target], target); } catch { return false; }
    if (!ownership) return false;
    if (ownership.scope === "central" && labels[ownership.clusterLabel] !== ownership.clusterValue) return false;
    const clusterLabel = labels.cluster || labels.cluster_name || labels.cluster_id || labels["k8s.cluster.name"] || labels["k8s.cluster.uid"];
    if (clusterLabel) {
      const value = String(clusterLabel);
      if (value !== targetId && value !== String(target.name) && (ownership.scope !== "central" || value !== ownership.clusterValue)) return false;
    }
    if (labels.namespace && scope.namespace && labels.namespace !== scope.namespace) return false;
    const appLabels = [labels.app, labels.application, labels.app_name, labels["app.kubernetes.io/name"], labels["service.name"]].filter(Boolean).map(String);
    if (appLabels.some((value) => value === scope.appName)) return true;
    const resources = [labels.pod, labels.pod_name, labels.service, labels.deployment, labels.statefulset, labels.daemonset, labels.job, labels.job_name].filter(Boolean).map(String);
    return resources.some((value) => (scope.pods || []).indexOf(value) >= 0 || (scope.services || []).indexOf(value) >= 0 || (scope.workloads || []).indexOf(value) >= 0);
  }
  root.XSObserve = { items, env, datasource, sourceScope, resolvedDatasource, metricQueries, scopedPromQuery, matrix, logQuery, scopedLogQuery, traceTags, normalizeLog, traceRecord, alertLocation, alertWorkloadCandidates, alertRemediationTarget, redactDiagnosticText, alertBelongsToScope, quote, regex };
  if (typeof module !== "undefined") module.exports = root.XSObserve;
})(typeof window === "undefined" ? globalThis : window);
