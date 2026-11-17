/* 应用 / 命名空间资源拓扑图（v1.31 u-topo）
 * 只读真实接口（全部为 GET，权限 workload:read）：
 *   GET /clusters/:id/network/ingresses?namespace=      Ingress（rules[].http.paths[].backend.service.name）
 *   GET /clusters/:id/network/services?namespace=       Service（selector）
 *   GET /clusters/:id/workloads/deployments|statefulsets|daemonsets|jobs|pods?namespace=
 *   GET /clusters/:id/namespaces/:ns/:kind/:name        速览侧板里的 YAML
 * 后端没有 ReplicaSet 列表接口：副本集由 Pod 的 owner（ReplicaSet/<name>）与 pod-template-hash 标签推算。
 * 关系推算（纯函数，见 XSTopo.buildGraph）：
 *   Ingress → Service：ingress backend 的 service.name（含 defaultBackend 与旧版 serviceName）
 *   Service → 工作负载：selector ⊆ Pod 标签，再沿 Pod 的 owner 归到工作负载；无 Pod 时退化为 selector ⊆ 工作负载标签（虚线，推断）
 *   工作负载 → (副本集 →) Pod：ownerReferences / owner 字段
 * 挂载：XSTopo.render(el, { clusterId, namespace, app?, data?, fallbackData?, navigate?, height? }) → { refresh, destroy }
 *   app：字符串或 { name, workloads:[], release, selector:{} }，只展示与应用相关的连通部分
 *   data：预先取好的原始数据（{ingresses, services, deployments, statefulsets, daemonsets, jobs, pods}），不再请求接口
 *   fallbackData：实时接口全部不可用时使用的数据（如应用清单），会给出提示
 * 列表页接入：XSTopo.decorateWorkloads(args)（列表 / 拓扑 切换），XSTopo.setView("topo")
 */
(function (root) {
  "use strict";

  var NODE_W = 208, NODE_H = 64, COL_GAP = 104, ROW_GAP = 18, GROUP_GAP = 10, PAD = 32;
  var POD_FOLD_THRESHOLD = 60;
  var VIEW_KEY = "xs.topo.view";
  var LAYER = { Ingress: 0, Service: 1, Deployment: 2, StatefulSet: 2, DaemonSet: 2, Job: 2, CronJob: 2, ReplicaSet: 3, Pod: 4, PodGroup: 4 };
  var WORKLOAD_KINDS = { Deployment: "deployments", StatefulSet: "statefulsets", DaemonSet: "daemonsets", Job: "jobs", CronJob: "cronjobs" };
  var RANK = { ok: 0, unknown: 1, warn: 2, bad: 3 };
  var STATUS_TEXT = { ok: "正常", warn: "警告", bad: "异常", unknown: "未知" };
  var KIND_TEXT = { Ingress: "应用路由", Service: "服务", Deployment: "无状态", StatefulSet: "有状态", DaemonSet: "守护进程", Job: "任务", CronJob: "定时任务", ReplicaSet: "副本集", Pod: "容器组", PodGroup: "容器组" };
  var RAW_TEXT = {
    running: "运行中", pending: "等待调度", succeeded: "已完成", completed: "已完成", complete: "已完成", failed: "失败", unknown: "未知",
    crashloopbackoff: "反复崩溃", imagepullbackoff: "镜像拉取失败", errimagepull: "镜像拉取失败", terminating: "终止中", containercreating: "创建中",
    podinitializing: "初始化中", oomkilled: "内存溢出", evicted: "已驱逐", error: "出错", available: "可用", progressing: "更新中", ready: "就绪",
    createcontainerconfigerror: "配置错误", invalidimagename: "镜像名无效", active: "活跃", suspended: "已暂停"
  };
  var BAD_RE = /crashloop|error|fail|imagepull|errimage|oomkilled|evicted|invalidimage|cannotrun|deadlineexceeded|backoff/i;
  var WARN_RE = /pending|creat|init|terminat|progress|waiting|unschedulable/i;
  var APP_LABELS = ["app", "app.kubernetes.io/name", "app.kubernetes.io/instance", "release", "k8s-app"];

  // ---------------------------------------------------------------- 工具

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === "object" && !Array.isArray(v) ? v : {}; }
  function itemsOf(d) { if (Array.isArray(d)) return d; if (d && Array.isArray(d.items)) return d.items; if (d && Array.isArray(d.data)) return d.data; return []; }
  function worst(a, b) { return (RANK[b] || 0) > (RANK[a] || 0) ? b : a; }
  function rawText(s) { var k = String(s || "").toLowerCase(); return RAW_TEXT[k] || String(s || "—"); }
  function meta(o) { var m = obj(o && o.metadata); return { name: o.name || m.name || "", namespace: o.namespace || m.namespace || "", labels: obj(o.labels || m.labels), uid: o.uid || m.uid || "" }; }
  function idOf(kind, ns, name) { return kind + "/" + (ns || "") + "/" + name; }

  function parseReady(v) {
    if (v && typeof v === "object") return { ready: Number(v.ready) || 0, total: Number(v.total) || 0 };
    var m = /^\s*(\d+)\s*\/\s*(\d+)\s*$/.exec(String(v == null ? "" : v));
    return m ? { ready: +m[1], total: +m[2] } : null;
  }

  function selectorMatches(selector, labels) {
    var keys = Object.keys(obj(selector));
    if (!keys.length) return false;
    labels = obj(labels);
    return keys.every(function (k) { return Object.prototype.hasOwnProperty.call(labels, k) && String(labels[k]) === String(selector[k]); });
  }

  // "ReplicaSet/web-7d9f" 或 ownerReferences[{kind,name,controller}]
  function ownerOf(p) {
    var refs = arr(p.ownerReferences || (p.metadata && p.metadata.ownerReferences) || p.owner_references);
    if (refs.length) {
      var c = refs.filter(function (r) { return r && r.controller; })[0] || refs[0];
      if (c && c.kind && c.name) return { kind: String(c.kind), name: String(c.name) };
    }
    var s = String(p.owner || "");
    var i = s.indexOf("/");
    if (i > 0) return { kind: s.slice(0, i), name: s.slice(i + 1) };
    return null;
  }

  // ---------------------------------------------------------------- 状态

  function podStatus(p) {
    var raw = String(p.status || p.phase || "");
    var phase = String(p.phase || "");
    if (!raw) return "unknown";
    if (BAD_RE.test(raw) || /^failed$/i.test(phase)) return "bad";
    if (/^(succeeded|completed)$/i.test(raw)) return "ok";
    if (/^unknown$/i.test(raw)) return "unknown";
    if (WARN_RE.test(raw)) return "warn";
    if (/^running$/i.test(raw) || /^running$/i.test(phase)) {
      var r = parseReady(p.ready);
      if (r && r.total > 0 && r.ready < r.total) return "warn";
      return "ok";
    }
    return "unknown";
  }

  function workloadStatus(w) {
    var kind = w.kind;
    var st = String(w.status || "");
    if (kind === "Job") return /fail/i.test(st) ? "bad" : "ok";
    if (kind === "CronJob") return /suspend/i.test(st) ? "unknown" : "ok";
    var desired = w.desired, ready = w.ready;
    if (desired == null) return "unknown";
    if (desired === 0) return "unknown";
    if (ready >= desired) return /progress/i.test(st) ? "warn" : "ok";
    if (ready === 0) return "bad";
    return "warn";
  }

  // ---------------------------------------------------------------- 归一化

  function normWorkload(w, fallbackKind) {
    var m = meta(w);
    var kind = w.kind || fallbackKind;
    var r = parseReady(w.ready);
    var desired = w.desired != null ? Number(w.desired) : w.replicas != null ? Number(w.replicas) : r ? r.total : null;
    var ready = typeof w.ready === "number" ? w.ready : r ? r.ready : null;
    if (kind === "DaemonSet" && r) desired = r.total;
    var out = { kind: kind, name: m.name, namespace: m.namespace, labels: m.labels, selector: obj(w.selector), desired: desired, ready: ready == null ? 0 : ready,
      status: w.status || "", images: arr(w.images), created: w.created_at || "", raw: w };
    out.health = workloadStatus(out);
    return out;
  }

  function normalize(raw) {
    raw = raw || {};
    var wl = [];
    [["deployments", "Deployment"], ["statefulsets", "StatefulSet"], ["daemonsets", "DaemonSet"], ["jobs", "Job"]].forEach(function (pair) {
      itemsOf(raw[pair[0]]).forEach(function (w) { wl.push(normWorkload(w, pair[1])); });
    });
    itemsOf(raw.workloads).forEach(function (w) { wl.push(normWorkload(w, w.kind || "Deployment")); });
    return {
      ingresses: itemsOf(raw.ingresses).map(function (x) { var m = meta(x); return { name: m.name, namespace: m.namespace, labels: m.labels, rules: arr(x.rules || (x.spec && x.spec.rules)),
        defaultBackend: x.default_backend || x.defaultBackend || (x.spec && x.spec.defaultBackend) || null, hosts: arr(x.hosts), className: x.class_name || "", tls: arr(x.tls),
        addressStatus: x.address_status || "", lb: arr(x.load_balancer), raw: x }; }),
      services: itemsOf(raw.services).map(function (x) { var m = meta(x); return { name: m.name, namespace: m.namespace, labels: m.labels, selector: obj(x.selector || (x.spec && x.spec.selector)),
        type: x.type || "ClusterIP", clusterIP: x.cluster_ip || "", ports: arr(x.ports), externalName: x.external_name || "", addressStatus: x.address_status || "", raw: x }; }),
      workloads: wl,
      pods: itemsOf(raw.pods).map(function (x) { var m = meta(x); return { name: m.name, namespace: m.namespace, labels: m.labels, owner: ownerOf(x), status: x.status || x.phase || "",
        phase: x.phase || "", ready: x.ready || "", restarts: x.restarts || 0, node: x.node || "", podIP: x.pod_ip || "", images: arr(x.images), created: x.created_at || "", raw: x }; })
    };
  }

  // 应用清单（/cicd/applications/:id/inventory）→ 原始数据
  function fromInventory(inv) {
    inv = inv || {};
    return {
      services: arr(inv.services),
      workloads: arr(inv.workloads).map(function (w) {
        return { kind: w.kind || "Deployment", name: w.name, namespace: w.namespace, desired: w.desired, ready: Number(w.ready) || 0,
          labels: obj(w.selector), selector: obj(w.selector), images: arr(w.images), created_at: w.created_at,
          status: (Number(w.ready) || 0) >= (Number(w.desired) || 0) ? "Available" : "Progressing" };
      }),
      pods: arr(inv.pods).map(function (p) { return Object.assign({}, p, { images: arr(p.containers).map(function (c) { return c.image; }) }); })
    };
  }

  // ---------------------------------------------------------------- 关系推算

  function ingressBackends(ing) {
    var out = [];
    function add(b, host, path) {
      if (!b) return;
      var svc = b.service || null;
      var name = svc ? svc.name : b.serviceName;
      if (!name) return;
      var port = svc && svc.port ? (svc.port.number || svc.port.name || "") : (b.servicePort || "");
      out.push({ service: String(name), port: port, host: host || "", path: path || "" });
    }
    add(ing.defaultBackend, "", "");
    ing.rules.forEach(function (r) {
      arr(r && r.http && r.http.paths).forEach(function (p) { add(p && p.backend, r.host, p && p.path); });
    });
    return out;
  }

  // 由副本集名推算 Deployment：优先 pod-template-hash，其次最长名称前缀
  function deploymentForReplicaSet(rsName, hash, deployByName) {
    if (hash && rsName.slice(-(hash.length + 1)) === "-" + hash) {
      var base = rsName.slice(0, -(hash.length + 1));
      if (deployByName[base]) return deployByName[base];
    }
    var best = null;
    Object.keys(deployByName).forEach(function (n) {
      if (rsName.indexOf(n + "-") === 0 && rsName.length > n.length + 1 && (!best || n.length > best.name.length)) best = deployByName[n];
    });
    return best;
  }

  function buildGraph(raw, opts) {
    opts = opts || {};
    var d = normalize(raw);
    var nodes = {}, edges = [], edgeSeen = {}, warnings = [];
    function addNode(n) { if (!nodes[n.id]) nodes[n.id] = n; return nodes[n.id]; }
    function addEdge(from, to, kind, extra) {
      if (!nodes[from] || !nodes[to] || from === to) return;
      var key = from + "\u0000" + to;
      if (edgeSeen[key]) { if (extra && extra.label && edgeSeen[key].labels.indexOf(extra.label) < 0) edgeSeen[key].labels.push(extra.label); return; }
      var e = { from: from, to: to, kind: kind, inferred: !!(extra && extra.inferred), labels: extra && extra.label ? [extra.label] : [] };
      edgeSeen[key] = e; edges.push(e);
    }

    var deployByName = {};
    d.workloads.forEach(function (w) {
      var n = addNode({ id: idOf(w.kind, w.namespace, w.name), kind: w.kind, name: w.name, namespace: w.namespace, layer: LAYER[w.kind] != null ? LAYER[w.kind] : 2,
        status: w.health, rawStatus: w.status, desired: w.desired, ready: w.ready, labels: w.labels, selector: w.selector, images: w.images, created: w.created, pods: [], raw: w.raw });
      if (w.kind === "Deployment") deployByName[w.namespace + "/" + w.name] = n;
    });
    function deployIndex(ns) {
      var m = {};
      Object.keys(deployByName).forEach(function (k) { if (k.indexOf(ns + "/") === 0) m[k.slice(ns.length + 1)] = deployByName[k]; });
      return m;
    }

    d.pods.forEach(function (p) {
      var pn = addNode({ id: idOf("Pod", p.namespace, p.name), kind: "Pod", name: p.name, namespace: p.namespace, layer: LAYER.Pod, status: podStatus(p), rawStatus: p.status,
        readyText: p.ready, restarts: p.restarts, node: p.node, podIP: p.podIP, labels: p.labels, images: p.images, created: p.created, raw: p.raw, owner: p.owner });
      var o = p.owner, workload = null, rs = null;
      if (o && o.kind === "ReplicaSet") {
        var dep = deploymentForReplicaSet(o.name, p.labels["pod-template-hash"], deployIndex(p.namespace));
        rs = addNode({ id: idOf("ReplicaSet", p.namespace, o.name), kind: "ReplicaSet", name: o.name, namespace: p.namespace, layer: LAYER.ReplicaSet, status: "ok", pods: [],
          hash: p.labels["pod-template-hash"] || "", parent: dep ? dep.id : "" });
        rs.pods.push(pn.id);
        workload = dep;
      } else if (o && WORKLOAD_KINDS[o.kind]) {
        workload = nodes[idOf(o.kind, p.namespace, o.name)] || (o.kind === "Job" ? addNode({ id: idOf("Job", p.namespace, o.name), kind: "Job", name: o.name, namespace: p.namespace,
          layer: LAYER.Job, status: "ok", rawStatus: "", desired: null, ready: 0, labels: {}, selector: {}, images: p.images, pods: [], synthetic: true }) : null);
      }
      pn.parent = workload ? workload.id : "";
      pn.rs = rs ? rs.id : "";
      if (workload) workload.pods.push(pn.id);
    });

    // 副本集与工作负载状态汇总
    Object.keys(nodes).forEach(function (id) {
      var n = nodes[id];
      if (n.kind === "ReplicaSet") {
        n.status = n.pods.reduce(function (s, pid) { return worst(s, nodes[pid].status === "unknown" ? "ok" : nodes[pid].status); }, "ok");
        n.desired = n.pods.length; n.ready = n.pods.filter(function (pid) { return nodes[pid].status === "ok"; }).length;
        if (n.parent) addEdge(n.parent, n.id, "own");
      }
    });
    Object.keys(nodes).forEach(function (id) {
      var n = nodes[id];
      if (n.layer !== 2) return;
      if (n.synthetic) { n.desired = n.pods.length; n.ready = n.pods.filter(function (pid) { return nodes[pid].status === "ok"; }).length; }
      var podWorst = n.pods.reduce(function (s, pid) { return worst(s, nodes[pid].status === "unknown" ? "ok" : nodes[pid].status); }, "ok");
      if (podWorst === "bad" && n.status === "ok") n.status = "warn";
      else if (podWorst !== "ok" && RANK[podWorst] > RANK[n.status] && n.status !== "unknown") n.status = podWorst === "bad" && n.ready > 0 ? "warn" : podWorst;
      n.revisions = Object.keys(nodes).filter(function (k) { return nodes[k].kind === "ReplicaSet" && nodes[k].parent === n.id; }).length;
    });
    // 工作负载 → (副本集 →) Pod
    d.pods.forEach(function (p) {
      var pn = nodes[idOf("Pod", p.namespace, p.name)];
      if (opts.showReplicaSets && pn.rs) addEdge(pn.rs, pn.id, "own");
      else if (pn.parent) addEdge(pn.parent, pn.id, "own");
      else if (pn.rs) addEdge(pn.rs, pn.id, "own");
    });
    if (!opts.showReplicaSets) {
      Object.keys(nodes).forEach(function (id) { if (nodes[id].kind === "ReplicaSet" && nodes[id].parent) delete nodes[id]; });
      edges = edges.filter(function (e) { return nodes[e.from] && nodes[e.to]; });
    }

    // Service → 工作负载 / Pod
    d.services.forEach(function (s) {
      var sn = addNode({ id: idOf("Service", s.namespace, s.name), kind: "Service", name: s.name, namespace: s.namespace, layer: LAYER.Service, status: "unknown",
        svcType: s.type, clusterIP: s.clusterIP, ports: s.ports, selector: s.selector, labels: s.labels, externalName: s.externalName, addressStatus: s.addressStatus, raw: s.raw, backends: [] });
      if (!Object.keys(s.selector).length) { sn.note = s.externalName ? "外部名称服务" : "未配置选择器"; return; }
      var matched = d.pods.filter(function (p) { return p.namespace === s.namespace && selectorMatches(s.selector, p.labels); });
      matched.forEach(function (p) {
        var pn = nodes[idOf("Pod", p.namespace, p.name)];
        sn.backends.push(pn.id);
        var target = pn.parent && nodes[pn.parent] ? pn.parent : pn.rs && nodes[pn.rs] ? pn.rs : pn.id;
        addEdge(sn.id, target, "select");
      });
      if (!matched.length) {
        d.workloads.forEach(function (w) {
          if (w.namespace !== s.namespace) return;
          if (selectorMatches(s.selector, w.labels) || selectorMatches(s.selector, w.selector)) addEdge(sn.id, idOf(w.kind, w.namespace, w.name), "select", { inferred: true });
        });
      }
      var readyPods = sn.backends.filter(function (pid) { return nodes[pid].status === "ok"; }).length;
      sn.readyBackends = readyPods;
      if (sn.backends.length) sn.status = readyPods === 0 ? "bad" : readyPods < sn.backends.length ? "warn" : "ok";
      else { sn.status = "warn"; sn.note = "没有匹配的容器组"; }
      if (s.type === "LoadBalancer" && s.addressStatus === "pending") { sn.status = worst(sn.status, "warn"); sn.note = (sn.note ? sn.note + "；" : "") + "负载均衡地址分配中"; }
    });

    // Ingress → Service
    d.ingresses.forEach(function (ing) {
      var inN = addNode({ id: idOf("Ingress", ing.namespace, ing.name), kind: "Ingress", name: ing.name, namespace: ing.namespace, layer: LAYER.Ingress, status: "ok",
        hosts: ing.hosts, className: ing.className, tls: ing.tls.length > 0, addressStatus: ing.addressStatus, lb: ing.lb, labels: ing.labels, raw: ing.raw, routes: [] });
      ingressBackends(ing).forEach(function (b) {
        var sid = idOf("Service", ing.namespace, b.service);
        inN.routes.push({ host: b.host, path: b.path, service: b.service, port: b.port });
        if (!nodes[sid]) {
          addNode({ id: sid, kind: "Service", name: b.service, namespace: ing.namespace, layer: LAYER.Service, status: "bad", missing: true, note: "Service 不存在", backends: [], selector: {}, ports: [] });
          warnings.push("应用路由 " + ing.name + " 指向的服务 " + b.service + " 不存在");
        }
        addEdge(inN.id, sid, "route", { label: (b.host || "*") + (b.path || "") });
        inN.status = worst(inN.status, nodes[sid].missing ? "bad" : nodes[sid].status === "bad" ? "warn" : nodes[sid].status === "warn" ? "warn" : "ok");
      });
      if (!inN.routes.length) { inN.status = "unknown"; inN.note = "未配置后端服务"; }
    });

    var list = Object.keys(nodes).map(function (k) { return nodes[k]; });
    var graph = { nodes: list, edges: edges, warnings: warnings, folded: false };
    if (opts.app) graph = filterByApp(graph, opts.app);
    return graph;
  }

  // ---------------------------------------------------------------- 应用过滤

  function appSpec(app) {
    if (!app) return null;
    if (typeof app === "string") return { name: app, workloads: [], release: "", selector: {} };
    return { name: app.name || "", workloads: arr(app.workloads).map(String), release: app.release || "", selector: obj(app.selector) };
  }

  function matchesApp(n, spec) {
    if (spec.workloads.length) return spec.workloads.indexOf(n.name) >= 0;
    var labels = obj(n.labels);
    if (Object.keys(spec.selector).length && (selectorMatches(spec.selector, labels) || selectorMatches(spec.selector, n.selector))) return true;
    if (spec.release && (labels["app.kubernetes.io/instance"] === spec.release || labels.release === spec.release)) return true;
    if (!spec.name) return false;
    if (n.name === spec.name) return true;
    return APP_LABELS.some(function (k) { return labels[k] === spec.name; });
  }

  function filterByApp(graph, app) {
    var spec = appSpec(app);
    if (!spec) return graph;
    var byId = {}, keep = {};
    graph.nodes.forEach(function (n) { byId[n.id] = n; });
    var out = {}, inc = {};
    graph.edges.forEach(function (e) { (out[e.from] = out[e.from] || []).push(e.to); (inc[e.to] = inc[e.to] || []).push(e.from); });
    var seeds = graph.nodes.filter(function (n) { return n.layer === 2 && matchesApp(n, spec); });
    if (!seeds.length && !spec.workloads.length) seeds = graph.nodes.filter(function (n) { return n.kind === "Pod" && !n.parent && matchesApp(n, spec); });
    // 向下：工作负载 → 副本集 → Pod
    var stack = seeds.map(function (n) { return n.id; });
    while (stack.length) {
      var id = stack.pop();
      if (keep[id]) continue;
      keep[id] = true;
      (out[id] || []).forEach(function (t) { if (byId[t] && byId[t].layer > byId[id].layer) stack.push(t); });
    }
    // 向上：指向已选节点的 Service，再到 Ingress
    [LAYER.Service, LAYER.Ingress].forEach(function (layer) {
      graph.nodes.forEach(function (n) {
        if (n.layer !== layer || keep[n.id]) return;
        if ((out[n.id] || []).some(function (t) { return keep[t]; })) keep[n.id] = true;
      });
    });
    return {
      nodes: graph.nodes.filter(function (n) { return keep[n.id]; }),
      edges: graph.edges.filter(function (e) { return keep[e.from] && keep[e.to]; }),
      warnings: graph.warnings, folded: graph.folded, app: spec
    };
  }

  // ---------------------------------------------------------------- 折叠

  function podCount(graph) { return graph.nodes.filter(function (n) { return n.kind === "Pod"; }).length; }

  function foldPods(graph, opts) {
    opts = opts || {};
    var threshold = opts.threshold == null ? POD_FOLD_THRESHOLD : opts.threshold;
    if (!opts.force && podCount(graph) <= threshold) return graph;
    var byId = {}, groups = {}, remap = {};
    graph.nodes.forEach(function (n) { byId[n.id] = n; });
    var ownerOfPod = {};
    graph.edges.forEach(function (e) { if (e.kind === "own" && byId[e.to] && byId[e.to].kind === "Pod") ownerOfPod[e.to] = e.from; });
    graph.nodes.forEach(function (n) {
      if (n.kind !== "Pod") return;
      var parent = ownerOfPod[n.id] || "";
      var parentNode = byId[parent];
      var gid = "PodGroup/" + (n.namespace || "") + "/" + (parent || "__standalone");
      var g = groups[gid] || (groups[gid] = { id: gid, kind: "PodGroup", name: parentNode ? parentNode.name : "独立容器组", namespace: n.namespace, layer: LAYER.PodGroup,
        status: "ok", parent: parent, members: [], counts: { ok: 0, warn: 0, bad: 0, unknown: 0 } });
      g.members.push(n.id);
      g.counts[n.status] = (g.counts[n.status] || 0) + 1;
      g.status = worst(g.status, n.status === "unknown" ? "ok" : n.status);
      remap[n.id] = gid;
    });
    var groupList = Object.keys(groups).map(function (k) { var g = groups[k]; g.desired = g.members.length; g.ready = g.counts.ok; g.memberNodes = g.members.map(function (id) { return byId[id]; }); return g; });
    var seen = {}, edges = [];
    graph.edges.forEach(function (e) {
      var f = remap[e.from] || e.from, t = remap[e.to] || e.to, key = f + "\u0000" + t;
      if (f === t || seen[key]) return;
      seen[key] = true;
      edges.push(Object.assign({}, e, { from: f, to: t }));
    });
    return { nodes: graph.nodes.filter(function (n) { return n.kind !== "Pod"; }).concat(groupList), edges: edges, warnings: graph.warnings, folded: true, app: graph.app };
  }

  // ---------------------------------------------------------------- 布局

  function sortKey(n) { return (3 - (RANK[n.status] || 0)) + "\u0000" + n.name; }

  function layout(graph, opts) {
    opts = opts || {};
    var W = opts.nodeWidth || NODE_W, H = opts.nodeHeight || NODE_H;
    var byId = {}, children = {}, parents = {};
    graph.nodes.forEach(function (n) { byId[n.id] = n; });
    graph.edges.forEach(function (e) {
      if (!byId[e.from] || !byId[e.to]) return;
      (children[e.from] = children[e.from] || []).push(e.to);
      (parents[e.to] = parents[e.to] || []).push(e.from);
    });
    var layers = [];
    graph.nodes.forEach(function (n) { if (layers.indexOf(n.layer) < 0) layers.push(n.layer); });
    layers.sort(function (a, b) { return a - b; });
    var colOf = {};
    layers.forEach(function (l, i) { colOf[l] = i; });
    function xOf(n) { return PAD + colOf[n.layer] * (W + COL_GAP); }
    var pos = {};
    var cursor = PAD;

    // 主干：工作负载 → (副本集 →) Pod，按块堆叠，父节点在子节点中间
    function ownKids(id) {
      return (children[id] || []).filter(function (c) { return byId[c].layer > byId[id].layer && byId[c].layer >= 2; })
        .map(function (c) { return byId[c]; }).sort(function (a, b) { return sortKey(a) < sortKey(b) ? -1 : sortKey(a) > sortKey(b) ? 1 : 0; });
    }
    var placed = {};
    function place(n, top) {
      placed[n.id] = true;
      var kids = ownKids(n.id).filter(function (k) { return !placed[k.id]; });
      var y = top, bottom = top;
      if (kids.length) {
        kids.forEach(function (k) { var b = place(k, y); bottom = b; y = b + ROW_GAP; });
        var first = pos[kids[0].id].y, last = pos[kids[kids.length - 1].id].y;
        var cy = (first + last) / 2;
        pos[n.id] = { x: xOf(n), y: Math.max(top, cy) };
        return Math.max(bottom, pos[n.id].y + H);
      }
      pos[n.id] = { x: xOf(n), y: top };
      return top + H;
    }
    var trunk = graph.nodes.filter(function (n) { return n.layer === 2; }).sort(function (a, b) { return sortKey(a) < sortKey(b) ? -1 : sortKey(a) > sortKey(b) ? 1 : 0; });
    var orphans = graph.nodes.filter(function (n) { return n.layer > 2 && !(parents[n.id] || []).some(function (p) { return byId[p].layer >= 2; }); })
      .sort(function (a, b) { return a.layer - b.layer || (sortKey(a) < sortKey(b) ? -1 : 1); });
    trunk.concat(orphans).forEach(function (n) {
      if (placed[n.id]) return;
      cursor = place(n, cursor) + ROW_GAP + GROUP_GAP;
    });

    // 上游列：按所连节点的中心对齐，再顺序消除重叠
    function alignColumn(layer) {
      var col = graph.nodes.filter(function (n) { return n.layer === layer; });
      col.forEach(function (n) {
        var ys = (children[n.id] || []).filter(function (c) { return pos[c]; }).map(function (c) { return pos[c].y; });
        n.__want = ys.length ? ys.reduce(function (s, v) { return s + v; }, 0) / ys.length : Infinity;
      });
      col.sort(function (a, b) { return a.__want - b.__want || (a.name < b.name ? -1 : 1); });
      var prev = PAD - ROW_GAP;
      col.forEach(function (n) {
        var want = n.__want === Infinity ? prev + ROW_GAP : n.__want;
        var y = Math.max(want, prev + ROW_GAP);
        pos[n.id] = { x: xOf(n), y: y };
        prev = y + H;
        delete n.__want;
      });
    }
    alignColumn(LAYER.Service);
    alignColumn(LAYER.Ingress);

    var maxY = PAD, out = [];
    graph.nodes.forEach(function (n) {
      var p = pos[n.id] || { x: xOf(n), y: PAD };
      out.push(Object.assign({}, n, { x: p.x, y: p.y, w: W, h: H, stack: stackDepth(n) }));
      maxY = Math.max(maxY, p.y + H);
    });
    var posById = {};
    out.forEach(function (n) { posById[n.id] = n; });
    var edges = graph.edges.filter(function (e) { return posById[e.from] && posById[e.to]; }).map(function (e) {
      return Object.assign({}, e, { path: edgePath(posById[e.from], posById[e.to]) });
    });
    return { nodes: out, edges: edges, width: PAD * 2 + layers.length * W + Math.max(0, layers.length - 1) * COL_GAP, height: maxY + PAD + 8,
      columns: layers.map(function (l) { return { layer: l, x: PAD + colOf[l] * (W + COL_GAP) }; }), nodeWidth: W, nodeHeight: H };
  }

  function stackDepth(n) {
    var count = n.kind === "PodGroup" ? n.members.length : n.layer === 2 || n.kind === "ReplicaSet" ? Number(n.desired) || 0 : 1;
    return count > 2 ? 2 : count > 1 ? 1 : 0;
  }

  // 直角折线：源右侧中点 → 目标左侧中点，竖段落在目标前的列间隙中
  function edgePath(a, b) {
    var x1 = a.x + a.w, y1 = a.y + a.h / 2, x2 = b.x, y2 = b.y + b.h / 2;
    if (Math.abs(y1 - y2) < 0.5) return "M" + x1 + " " + y1 + " H" + x2;
    var mx = x2 - Math.min(COL_GAP / 2, Math.max(12, (x2 - x1) / 2));
    return "M" + x1 + " " + y1 + " H" + mx + " V" + y2 + " H" + x2;
  }

  function summarize(graph) {
    var s = { ok: 0, warn: 0, bad: 0, unknown: 0, total: 0 };
    graph.nodes.forEach(function (n) { s[n.status] = (s[n.status] || 0) + 1; s.total++; });
    return s;
  }

  // ---------------------------------------------------------------- 绘制

  var ICON = {
    Ingress: '<path d="M2 8h9M8 5l3 3-3 3" /><path d="M11 3h3v10h-3" />',
    Service: '<circle cx="8" cy="8" r="2.2" /><circle cx="3" cy="4" r="1.4" /><circle cx="3" cy="12" r="1.4" /><circle cx="13" cy="8" r="1.4" /><path d="M4.2 4.8 6.3 6.8M4.2 11.2l2.1-2M10.2 8h1.4" />',
    Deployment: '<rect x="2.5" y="2.5" width="11" height="11" rx="1.5" /><path d="M5 6h6M5 8h6M5 10h4" />',
    StatefulSet: '<ellipse cx="8" cy="4" rx="5" ry="1.8" /><path d="M3 4v8c0 1 2.2 1.8 5 1.8s5-.8 5-1.8V4M3 8c0 1 2.2 1.8 5 1.8S13 9 13 8" />',
    DaemonSet: '<rect x="2" y="2.5" width="5" height="5" rx="1" /><rect x="9" y="2.5" width="5" height="5" rx="1" /><rect x="2" y="9" width="5" height="5" rx="1" /><rect x="9" y="9" width="5" height="5" rx="1" />',
    Job: '<circle cx="8" cy="8" r="5.5" /><path d="M8 5v3l2 1.5" />',
    CronJob: '<circle cx="8" cy="8" r="5.5" /><path d="M8 5v3l2 1.5" />',
    ReplicaSet: '<rect x="4.5" y="4.5" width="9" height="9" rx="1.2" /><path d="M2.5 11.5v-9h9" />',
    Pod: '<path d="M8 2 13.5 5v6L8 14 2.5 11V5z" /><path d="M2.5 5 8 8l5.5-3M8 8v6" />',
    PodGroup: '<path d="M8 2 13.5 5v6L8 14 2.5 11V5z" /><path d="M2.5 5 8 8l5.5-3M8 8v6" />'
  };
  function iconSVG(kind) { return '<svg class="xtp-ico" viewBox="0 0 16 16" aria-hidden="true" focusable="false">' + (ICON[kind] || ICON.Pod) + "</svg>"; }

  function replicaText(n) {
    if (n.kind === "PodGroup") return n.members.length + " 个 · 异常 " + n.counts.bad;
    if (n.kind === "Pod") return n.readyText ? "就绪 " + n.readyText + (n.restarts ? " · 重启 " + n.restarts : "") : rawText(n.rawStatus);
    if (n.kind === "Service") return (n.missing ? "不存在" : n.svcType || "ClusterIP") + (n.backends && n.backends.length ? " · 后端 " + n.readyBackends + "/" + n.backends.length : "");
    if (n.kind === "Ingress") return (n.hosts && n.hosts.length ? n.hosts[0] + (n.hosts.length > 1 ? " +" + (n.hosts.length - 1) : "") : "无域名");
    if (n.desired == null) return rawText(n.rawStatus);
    return "副本 " + (n.ready || 0) + "/" + n.desired;
  }

  function nodeHTML(n) {
    var cls = "xtp-node xtp-" + n.status + (n.stack ? " xtp-stack" + n.stack : "") + (n.missing ? " xtp-missing" : "");
    return '<button type="button" class="' + cls + '" data-xtp-node="' + esc(n.id) + '" style="left:' + n.x + "px;top:" + n.y + "px;width:" + n.w + "px;height:" + n.h + 'px" ' +
      'aria-label="' + esc(KIND_TEXT[n.kind] + " " + n.name + "，" + STATUS_TEXT[n.status]) + '" title="' + esc(n.name) + '">' +
      '<span class="xtp-bar" aria-hidden="true"></span>' + iconSVG(n.kind) +
      '<span class="xtp-txt"><span class="xtp-kind">' + esc(n.kind === "PodGroup" ? "Pod 组" : n.kind) + '</span><span class="xtp-name">' + esc(n.name) + "</span>" +
      '<span class="xtp-sub">' + esc(replicaText(n)) + "</span></span>" +
      '<span class="xtp-dot" aria-hidden="true"></span></button>';
  }

  function sceneHTML(lay) {
    var cols = { 0: "应用路由", 1: "服务", 2: "工作负载", 3: "副本集", 4: "容器组" };
    var heads = lay.columns.map(function (c) { return '<span class="xtp-colh" style="left:' + c.x + "px;width:" + lay.nodeWidth + 'px">' + esc(cols[c.layer] || "") + "</span>"; }).join("");
    var paths = lay.edges.map(function (e) {
      return '<path class="xtp-edge xtp-e-' + e.kind + (e.inferred ? " xtp-e-inferred" : "") + '" data-from="' + esc(e.from) + '" data-to="' + esc(e.to) + '" d="' + e.path + '" marker-end="url(#xtpArrow)">' +
        (e.labels && e.labels.length ? "<title>" + esc(e.labels.join("\n")) + "</title>" : "") + "</path>";
    }).join("");
    return '<div class="xtp-stage" data-xtp-stage style="width:' + lay.width + "px;height:" + lay.height + 'px">' + heads +
      '<svg class="xtp-edges" width="' + lay.width + '" height="' + lay.height + '" viewBox="0 0 ' + lay.width + " " + lay.height + '" aria-hidden="true">' +
      '<defs><marker id="xtpArrow" viewBox="0 0 8 8" refX="7.5" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0 8 4 0 8z" fill="#9aa4b2" /></marker></defs>' + paths + "</svg>" +
      lay.nodes.map(nodeHTML).join("") + "</div>";
  }

  function tagHTML(status, text) { return '<span class="xtp-tag xtp-tag-' + status + '">' + esc(text || STATUS_TEXT[status]) + "</span>"; }

  function kvRows(n) {
    var rows = [["命名空间", n.namespace || "—"]];
    if (n.kind === "Ingress") {
      rows.push(["域名", (n.hosts || []).join(", ") || "—"], ["路由类", n.className || "—"], ["HTTPS", n.tls ? "已配置证书" : "未配置"],
        ["入口地址", (n.lb || []).map(function (a) { return a.ip || a.hostname; }).filter(Boolean).join(", ") || (n.addressStatus === "pending" ? "分配中" : "—")]);
      (n.routes || []).forEach(function (r) { rows.push(["路由", (r.host || "*") + (r.path || "/") + " → " + r.service + (r.port ? ":" + r.port : "")]); });
    } else if (n.kind === "Service") {
      if (n.missing) rows.push(["说明", "被应用路由引用，但该命名空间中没有此服务"]);
      else rows.push(["类型", n.svcType || "ClusterIP"], ["集群 IP", n.clusterIP || "—"],
        ["端口", (n.ports || []).map(function (p) { return (p.port || "") + (p.targetPort != null ? "→" + p.targetPort : "") + "/" + (p.protocol || "TCP") + (p.nodePort ? " 节点 " + p.nodePort : ""); }).join("，") || "—"],
        ["选择器", Object.keys(n.selector || {}).map(function (k) { return k + "=" + n.selector[k]; }).join(", ") || "—"],
        ["就绪后端", n.backends && n.backends.length ? n.readyBackends + " / " + n.backends.length : "0"]);
    } else if (n.kind === "Pod") {
      rows.push(["状态", rawText(n.rawStatus) + (n.rawStatus && rawText(n.rawStatus) !== n.rawStatus ? "（" + n.rawStatus + "）" : "")], ["就绪", n.readyText || "—"], ["重启次数", String(n.restarts || 0)],
        ["节点", n.node || "—"], ["容器组 IP", n.podIP || "—"], ["镜像", (n.images || []).join(", ") || "—"], ["归属", n.owner ? n.owner.kind + "/" + n.owner.name : "无（独立容器组）"]);
    } else if (n.kind === "PodGroup") {
      rows.push(["数量", String(n.members.length)], ["正常 / 警告 / 异常 / 未知", n.counts.ok + " / " + n.counts.warn + " / " + n.counts.bad + " / " + n.counts.unknown]);
    } else if (n.kind === "ReplicaSet") {
      rows.push(["模板哈希", n.hash || "—"], ["容器组", (n.ready || 0) + " / " + (n.desired || 0) + " 正常"]);
    } else {
      rows.push(["副本", n.desired == null ? "—" : (n.ready || 0) + " / " + n.desired + " 就绪"], ["状态", rawText(n.rawStatus)], ["镜像", (n.images || []).join(", ") || "—"]);
      if (n.revisions > 1) rows.push(["运行中的版本", n.revisions + " 个（可能正在滚动更新）"]);
      if (n.synthetic) rows.push(["说明", "由容器组归属推算，未在任务列表中读取到"]);
    }
    if (n.note) rows.push(["提示", n.note]);
    return rows;
  }

  function sideHTML(n, graph, ctx) {
    var byId = {};
    graph.nodes.forEach(function (x) { byId[x.id] = x; });
    var up = [], down = [];
    graph.edges.forEach(function (e) { if (e.to === n.id && byId[e.from]) up.push(byId[e.from]); if (e.from === n.id && byId[e.to]) down.push(byId[e.to]); });
    function rel(title, list) {
      if (!list.length) return "";
      return '<h4 class="xtp-sh">' + esc(title) + '</h4><ul class="xtp-rel">' + list.slice(0, 30).map(function (x) {
        return '<li><button type="button" class="xtp-link" data-xtp-goto="' + esc(x.id) + '"><span class="xtp-sdot xtp-' + x.status + '"></span>' + esc((x.kind === "PodGroup" ? "Pod 组" : x.kind) + " · " + x.name) + "</button></li>";
      }).join("") + (list.length > 30 ? '<li class="xtp-muted">另有 ' + (list.length - 30) + " 个</li>" : "") + "</ul>";
    }
    var members = "";
    if (n.kind === "PodGroup") {
      members = '<h4 class="xtp-sh">容器组</h4><ul class="xtp-rel">' + n.memberNodes.slice().sort(function (a, b) { return RANK[b.status] - RANK[a.status]; }).slice(0, 100).map(function (p) {
        return '<li><span class="xtp-sdot xtp-' + p.status + '"></span><span class="xtp-mono">' + esc(p.name) + '</span> <span class="xtp-muted">' + esc(rawText(p.rawStatus)) + (p.restarts ? " · 重启 " + esc(p.restarts) : "") + "</span></li>";
      }).join("") + (n.memberNodes.length > 100 ? '<li class="xtp-muted">仅显示前 100 个</li>' : "") + "</ul>";
    }
    var acts = [];
    if (ctx.clusterId && !n.missing) {
      if (n.kind === "Pod" || (WORKLOAD_KINDS[n.kind] && !n.synthetic)) acts.push('<button type="button" class="x-btn" data-xtp-yaml>查看 YAML</button>');
      if (n.kind !== "ReplicaSet" && n.kind !== "PodGroup") acts.push('<button type="button" class="x-btn" data-xtp-nav="detail">在列表中查看</button>');
      if (n.kind === "Pod") acts.push('<button type="button" class="x-btn" data-xtp-nav="logs">查看日志</button>');
    }
    return '<div class="xtp-sidehd">' + iconSVG(n.kind) + '<div class="xtp-sidet"><p class="xtp-kind">' + esc(KIND_TEXT[n.kind] + " · " + (n.kind === "PodGroup" ? "Pod 组" : n.kind)) + "</p><h3>" + esc(n.name) + "</h3></div>" +
      '<button type="button" class="xtp-x" data-xtp-close aria-label="关闭速览">×</button></div>' +
      '<p class="xtp-sidest">' + tagHTML(n.status) + '<span class="xtp-muted">' + esc(replicaText(n)) + "</span></p>" +
      '<dl class="xtp-kv">' + kvRows(n).map(function (r) { return "<dt>" + esc(r[0]) + "</dt><dd>" + esc(r[1]) + "</dd>"; }).join("") + "</dl>" +
      (acts.length ? '<div class="xtp-acts">' + acts.join("") + "</div>" : "") + '<div data-xtp-yamlbox></div>' +
      rel("上游", up) + rel("下游", down) + members;
  }

  function detailTarget(n, clusterId, action) {
    var kind = n.kind;
    var sub = kind === "Ingress" ? "expose" : kind === "Service" ? "network" : action === "logs" ? "logs" : "workloads";
    return { kind: kind, name: n.name, namespace: n.namespace, action: action || "detail", workloadKind: kind === "Pod" ? "pods" : WORKLOAD_KINDS[kind] || "",
      hash: "#/cluster/" + sub + "/" + encodeURIComponent(String(clusterId)) };
  }

  // ---------------------------------------------------------------- 演示数据

  function demoData(ns) {
    ns = ns || "shop";
    function pod(name, owner, labels, status, ready, restarts) { return { name: name, namespace: ns, owner: owner, labels: labels, status: status, phase: status === "Running" ? "Running" : "Pending", ready: ready, restarts: restarts || 0, node: "node-1", images: [] }; }
    var web = { app: "web", "pod-template-hash": "6f9c" }, api = { app: "api", "pod-template-hash": "58d7" };
    return {
      ingresses: [{ name: "shop", namespace: ns, hosts: ["shop.example.com"], rules: [{ host: "shop.example.com", http: { paths: [
        { path: "/", backend: { service: { name: "web", port: { number: 80 } } } }, { path: "/api", backend: { service: { name: "api", port: { number: 8080 } } } }] } }], address_status: "ready" }],
      services: [{ name: "web", namespace: ns, type: "ClusterIP", cluster_ip: "10.96.0.21", selector: { app: "web" }, ports: [{ port: 80, protocol: "TCP" }] },
        { name: "api", namespace: ns, type: "ClusterIP", cluster_ip: "10.96.0.34", selector: { app: "api" }, ports: [{ port: 8080, protocol: "TCP" }] },
        { name: "redis", namespace: ns, type: "ClusterIP", cluster_ip: "None", selector: { app: "redis" }, ports: [{ port: 6379, protocol: "TCP" }] }],
      deployments: [{ kind: "Deployment", name: "web", namespace: ns, replicas: 2, ready: "2/2", status: "Available", labels: { app: "web" }, images: ["nginx:1.27"] },
        { kind: "Deployment", name: "api", namespace: ns, replicas: 3, ready: "2/3", status: "Progressing", labels: { app: "api" }, images: ["shop/api:1.8.2"] },
        { kind: "Deployment", name: "worker", namespace: ns, replicas: 1, ready: "0/1", status: "Progressing", labels: { app: "worker" }, images: ["shop/worker:1.8.2"] }],
      statefulsets: [{ kind: "StatefulSet", name: "redis", namespace: ns, replicas: 1, ready: "1/1", status: "Ready", labels: { app: "redis" }, images: ["redis:7"] }],
      pods: [pod("web-6f9c-a1", "ReplicaSet/web-6f9c", web, "Running", "1/1"), pod("web-6f9c-b2", "ReplicaSet/web-6f9c", web, "Running", "1/1"),
        pod("api-58d7-c3", "ReplicaSet/api-58d7", api, "Running", "1/1"), pod("api-58d7-d4", "ReplicaSet/api-58d7", api, "Running", "1/1"),
        pod("api-58d7-e5", "ReplicaSet/api-58d7", api, "CrashLoopBackOff", "0/1", 7),
        pod("worker-7b2-f6", "ReplicaSet/worker-7b2", { app: "worker", "pod-template-hash": "7b2" }, "ImagePullBackOff", "0/1"),
        pod("redis-0", "StatefulSet/redis", { app: "redis" }, "Running", "1/1")]
    };
  }

  // ---------------------------------------------------------------- 组件

  function X() { return root.XS || {}; }
  function isLive() { return X().mode === "live"; }
  function errText(e) {
    var m = (e && e.message) || "";
    if (e && e.status === 403) return "没有读取权限";
    if (/[\u4e00-\u9fff]/.test(m)) return m;
    return "暂时无法读取";
  }
  function settle(p) { return Promise.resolve(p).then(function (v) { return { ok: true, value: v }; }, function (e) { return { ok: false, error: e }; }); }

  var SOURCES = [["ingresses", "/network/ingresses", "应用路由"], ["services", "/network/services", "服务"], ["deployments", "/workloads/deployments", "无状态工作负载"],
    ["statefulsets", "/workloads/statefulsets", "有状态工作负载"], ["daemonsets", "/workloads/daemonsets", "守护进程集"], ["jobs", "/workloads/jobs", "任务"], ["pods", "/workloads/pods", "容器组"]];

  function load(opts, signal) {
    if (opts.data) return Promise.resolve({ raw: opts.data, notes: [] });
    if (!isLive() && !opts.api) return Promise.resolve({ raw: demoData(opts.namespace), notes: [], demo: true });
    var apiFn = opts.api || function (path, o) { return X().api(path, o); };
    var base = "/clusters/" + encodeURIComponent(String(opts.clusterId));
    var query = opts.namespace ? { namespace: opts.namespace } : {};
    return Promise.all(SOURCES.map(function (s) { return settle(apiFn(base + s[1], { query: query, signal: signal })); })).then(function (rs) {
      var raw = {}, notes = [], failed = 0, firstErr = null;
      rs.forEach(function (r, i) {
        if (r.ok) raw[SOURCES[i][0]] = itemsOf(r.value);
        else { failed++; firstErr = firstErr || r.error; notes.push(SOURCES[i][2] + "：" + errText(r.error)); raw[SOURCES[i][0]] = []; }
      });
      if (failed === SOURCES.length) {
        if (opts.fallbackData) return { raw: opts.fallbackData, notes: ["集群资源暂时无法读取，已改用应用清单展示（不含应用路由）"], fallback: true };
        var err = new Error(errText(firstErr)); err.cause = firstErr; err.status = firstErr && firstErr.status; throw err;
      }
      return { raw: raw, notes: notes };
    });
  }

  function render(el, opts) {
    opts = Object.assign({}, opts || {});
    if (!el) return null;
    var state = { k: 1, tx: 0, ty: 0, showRS: false, fold: null, selected: "", graph: null, lay: null, raw: null, notes: [], token: 0, at: null, destroyed: false };
    var ctl = typeof AbortController === "undefined" ? null : new AbortController();
    var heightPx = opts.height || 560;
    el.innerHTML = '<div class="xtp" data-xtp>' +
      '<div class="xtp-bar0" role="toolbar" aria-label="拓扑工具栏"><span class="xtp-sum" data-xtp-sum></span><span class="xtp-grow"></span>' +
      '<label class="xtp-chk"><input type="checkbox" data-xtp-rs /> 显示副本集</label>' +
      '<button type="button" class="x-btn xtp-sm" data-xtp-fold hidden></button>' +
      '<span class="xtp-zoom" role="group" aria-label="缩放"><button type="button" class="x-btn xtp-sm" data-xtp-zoom="-1" aria-label="缩小">−</button>' +
      '<button type="button" class="x-btn xtp-sm" data-xtp-zoom="0" aria-label="适应画布">适应</button>' +
      '<button type="button" class="x-btn xtp-sm" data-xtp-zoom="1" aria-label="放大">+</button></span>' +
      '<button type="button" class="x-btn xtp-sm" data-xtp-refresh>刷新</button><span class="xtp-muted" data-xtp-at></span></div>' +
      '<p class="xtp-note" data-xtp-note hidden></p>' +
      '<div class="xtp-body"><div class="xtp-vp" data-xtp-vp tabindex="0" aria-label="资源拓扑画布，拖动平移，滚轮缩放" style="height:' + heightPx + 'px"><p class="xtp-muted xtp-loading">正在读取资源…</p></div>' +
      '<aside class="xtp-side" data-xtp-side hidden aria-label="资源速览"></aside></div>' +
      '<p class="xtp-legend" aria-hidden="true"><span><i class="xtp-sdot xtp-ok"></i>正常</span><span><i class="xtp-sdot xtp-warn"></i>警告</span><span><i class="xtp-sdot xtp-bad"></i>异常</span><span><i class="xtp-sdot xtp-unknown"></i>未知 / 停用</span>' +
      '<span><i class="xtp-ln"></i>归属 / 选择</span><span><i class="xtp-ln xtp-ln-d"></i>推断</span><span>叠层表示多副本</span></p></div>';
    var q = function (s) { return el.querySelector(s); };
    var vp = q("[data-xtp-vp]"), side = q("[data-xtp-side]");

    function applyTransform() {
      var st = vp.querySelector("[data-xtp-stage]");
      if (st) st.style.transform = "translate(" + state.tx + "px," + state.ty + "px) scale(" + state.k + ")";
    }
    function fit() {
      if (!state.lay) return;
      var w = vp.clientWidth || 960, h = vp.clientHeight || heightPx;
      var k = Math.min(1, w / state.lay.width, h / state.lay.height);
      state.k = Math.max(0.2, k);
      state.tx = Math.max(0, (w - state.lay.width * state.k) / 2);
      state.ty = Math.max(0, Math.min(16, (h - state.lay.height * state.k) / 2));
      applyTransform();
    }
    function zoomAt(factor, cx, cy) {
      var k = Math.min(2.5, Math.max(0.2, state.k * factor));
      if (cx == null) { cx = (vp.clientWidth || 960) / 2; cy = (vp.clientHeight || heightPx) / 2; }
      state.tx = cx - (cx - state.tx) * (k / state.k);
      state.ty = cy - (cy - state.ty) * (k / state.k);
      state.k = k;
      applyTransform();
    }

    function compute() {
      var g = buildGraph(state.raw, { showReplicaSets: state.showRS, app: state.prefiltered ? null : opts.app });
      var many = podCount(g) > POD_FOLD_THRESHOLD;
      var fold = state.fold == null ? many : state.fold;
      state.graph = fold ? foldPods(g, { force: true }) : g;
      state.manyPods = many;
      state.podTotal = podCount(g);
      state.lay = layout(state.graph);
    }

    function paint() {
      if (state.destroyed) return;
      compute();
      var s = summarize(state.graph);
      q("[data-xtp-sum]").innerHTML = '<b>' + s.total + " 个资源</b>" + ["ok", "warn", "bad", "unknown"].map(function (k) {
        return s[k] ? '<span class="xtp-chip xtp-chip-' + k + '"><i class="xtp-sdot xtp-' + k + '"></i>' + STATUS_TEXT[k] + " " + s[k] + "</span>" : "";
      }).join("");
      var fb = q("[data-xtp-fold]");
      if (state.podTotal > 0) { fb.hidden = false; fb.textContent = state.graph.folded ? "展开容器组（" + state.podTotal + "）" : "按工作负载折叠"; }
      else fb.hidden = true;
      var notes = state.notes.concat(state.graph.warnings || []);
      if (state.graph.folded && state.fold == null) notes.unshift("容器组超过 " + POD_FOLD_THRESHOLD + " 个，已按工作负载折叠");
      var note = q("[data-xtp-note]");
      note.hidden = !notes.length;
      note.textContent = notes.join("；");
      if (!state.graph.nodes.length) {
        vp.innerHTML = '<p class="xtp-empty">' + esc(opts.app ? "没有找到与该应用相关的资源。" : opts.namespace ? "命名空间 " + opts.namespace + " 中没有可展示的资源。" : "没有可展示的资源。") + "</p>";
        state.lay = null; side.hidden = true;
        return;
      }
      vp.innerHTML = sceneHTML(state.lay);
      fit();
      if (state.selected && state.graph.nodes.some(function (n) { return n.id === state.selected; })) select(state.selected, true);
      else { state.selected = ""; side.hidden = true; }
    }

    function select(id, keep) {
      var n = null;
      state.graph.nodes.forEach(function (x) { if (x.id === id) n = x; });
      [].slice.call(vp.querySelectorAll("[data-xtp-node]")).forEach(function (b) { b.classList && b.classList.toggle("xtp-on", b.getAttribute("data-xtp-node") === id); });
      var linked = {};
      if (n) { linked[id] = true; state.graph.edges.forEach(function (e) { if (e.from === id) linked[e.to] = true; if (e.to === id) linked[e.from] = true; }); }
      [].slice.call(vp.querySelectorAll(".xtp-edge")).forEach(function (p) {
        var hit = n && (p.getAttribute("data-from") === id || p.getAttribute("data-to") === id);
        if (p.classList) { p.classList.toggle("xtp-hot", !!hit); p.classList.toggle("xtp-dim", !!n && !hit); }
      });
      [].slice.call(vp.querySelectorAll("[data-xtp-node]")).forEach(function (b) { if (b.classList) b.classList.toggle("xtp-dim", !!n && !linked[b.getAttribute("data-xtp-node")]); });
      if (!n) { state.selected = ""; side.hidden = true; return; }
      state.selected = id;
      side.innerHTML = sideHTML(n, state.graph, { clusterId: opts.clusterId });
      side.hidden = false;
      if (!keep && side.focus) { side.setAttribute("tabindex", "-1"); side.focus(); }
      bindSide(n);
    }

    function bindSide(n) {
      var close = side.querySelector("[data-xtp-close]");
      if (close) close.onclick = function () { select(""); };
      [].slice.call(side.querySelectorAll("[data-xtp-goto]")).forEach(function (b) { b.onclick = function () { select(b.getAttribute("data-xtp-goto")); }; });
      [].slice.call(side.querySelectorAll("[data-xtp-nav]")).forEach(function (b) {
        b.onclick = function () {
          var t = detailTarget(n, opts.clusterId, b.getAttribute("data-xtp-nav"));
          if (typeof opts.navigate === "function") opts.navigate(t);
          else if (typeof location !== "undefined") location.hash = t.hash;
        };
      });
      var yb = side.querySelector("[data-xtp-yaml]");
      if (yb) yb.onclick = function () {
        var box = side.querySelector("[data-xtp-yamlbox]");
        var kind = n.kind === "Pod" ? "pods" : WORKLOAD_KINDS[n.kind];
        box.innerHTML = '<p class="xtp-muted">正在读取…</p>';
        var apiFn = opts.api || function (path, o) { return X().api(path, o); };
        apiFn("/clusters/" + encodeURIComponent(String(opts.clusterId)) + "/namespaces/" + encodeURIComponent(n.namespace) + "/" + kind + "/" + encodeURIComponent(n.name), { signal: ctl && ctl.signal })
          .then(function (d) { if (state.selected === n.id) box.innerHTML = '<pre class="xtp-yaml">' + esc((d && d.yaml) || "# 无内容") + "</pre>"; },
            function (e) { if (state.selected === n.id) box.innerHTML = '<p class="xtp-err" role="alert">读取失败：' + esc(errText(e)) + "</p>"; });
      };
    }

    function refresh() {
      var token = ++state.token;
      if (!state.raw) vp.innerHTML = '<p class="xtp-muted xtp-loading">正在读取资源…</p>';
      return load(opts, ctl && ctl.signal).then(function (res) {
        if (token !== state.token || state.destroyed) return;
        state.raw = res.raw; state.notes = res.notes || []; state.prefiltered = !!res.fallback;
        if (res.demo) state.notes = ["演示数据"].concat(state.notes);
        state.at = new Date();
        var at = q("[data-xtp-at]");
        if (at) at.textContent = "更新于 " + pad(state.at.getHours()) + ":" + pad(state.at.getMinutes()) + ":" + pad(state.at.getSeconds());
        paint();
      }, function (e) {
        if (token !== state.token || state.destroyed) return;
        state.raw = null; state.lay = null;
        q("[data-xtp-sum]").innerHTML = "";
        q("[data-xtp-note]").hidden = true;
        vp.innerHTML = '<div class="xtp-empty" role="alert">拓扑读取失败：' + esc(e && e.message || "暂时无法读取") + ' <button type="button" class="x-btn xtp-sm" data-xtp-retry>重试</button></div>';
        var r = vp.querySelector("[data-xtp-retry]");
        if (r) r.onclick = refresh;
      });
    }
    function pad(v) { return (v < 10 ? "0" : "") + v; }

    // 工具栏
    q("[data-xtp-refresh]").onclick = refresh;
    var rs = q("[data-xtp-rs]");
    rs.onchange = function () { state.showRS = !!rs.checked; if (state.raw) paint(); };
    q("[data-xtp-fold]").onclick = function () { state.fold = !state.graph.folded; paint(); };
    [].slice.call(el.querySelectorAll("[data-xtp-zoom]")).forEach(function (b) {
      b.onclick = function () { var d = +b.getAttribute("data-xtp-zoom"); if (!d) fit(); else zoomAt(d > 0 ? 1.2 : 1 / 1.2); };
    });

    // 平移 / 缩放 / 点击
    var drag = null;
    function onDown(ev) {
      if (ev.button != null && ev.button !== 0) return;
      if (ev.target && ev.target.closest && ev.target.closest("[data-xtp-node],button")) return;
      drag = { x: ev.clientX, y: ev.clientY, tx: state.tx, ty: state.ty, moved: false };
      if (vp.setPointerCapture && ev.pointerId != null) { try { vp.setPointerCapture(ev.pointerId); } catch (e) { /* ignore */ } }
    }
    function onMove(ev) {
      if (!drag) return;
      var dx = ev.clientX - drag.x, dy = ev.clientY - drag.y;
      if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true;
      state.tx = drag.tx + dx; state.ty = drag.ty + dy;
      applyTransform();
    }
    function onUp() { if (drag && !drag.moved && state.selected) select(""); drag = null; }
    function onWheel(ev) {
      if (!state.lay) return;
      ev.preventDefault();
      var r = vp.getBoundingClientRect ? vp.getBoundingClientRect() : { left: 0, top: 0 };
      zoomAt(ev.deltaY < 0 ? 1.1 : 1 / 1.1, ev.clientX - r.left, ev.clientY - r.top);
    }
    function onClick(ev) {
      var b = ev.target && ev.target.closest && ev.target.closest("[data-xtp-node]");
      if (b) select(b.getAttribute("data-xtp-node"));
    }
    function onKey(ev) {
      if (ev.key === "Escape" && state.selected) select("");
      else if (ev.key === "+" || ev.key === "=") zoomAt(1.2);
      else if (ev.key === "-") zoomAt(1 / 1.2);
      else if (ev.key === "0") fit();
    }
    vp.addEventListener("pointerdown", onDown);
    vp.addEventListener("pointermove", onMove);
    vp.addEventListener("pointerup", onUp);
    vp.addEventListener("pointercancel", onUp);
    vp.addEventListener("wheel", onWheel, { passive: false });
    vp.addEventListener("click", onClick);
    vp.addEventListener("keydown", onKey);

    refresh();
    return {
      refresh: refresh,
      select: function (id) { select(id); },
      graph: function () { return state.graph; },
      destroy: function () {
        state.destroyed = true;
        if (ctl) ctl.abort();
        vp.removeEventListener("pointerdown", onDown); vp.removeEventListener("pointermove", onMove); vp.removeEventListener("pointerup", onUp);
        vp.removeEventListener("pointercancel", onUp); vp.removeEventListener("wheel", onWheel); vp.removeEventListener("click", onClick); vp.removeEventListener("keydown", onKey);
      }
    };
  }

  // ---------------------------------------------------------------- 工作负载列表页：列表 / 拓扑 切换

  function getView() { try { return root.sessionStorage && root.sessionStorage.getItem(VIEW_KEY) === "topo" ? "topo" : "list"; } catch (e) { return "list"; } }
  function setView(v) { try { if (root.sessionStorage) root.sessionStorage.setItem(VIEW_KEY, v === "topo" ? "topo" : "list"); } catch (e) { /* ignore */ } }

  var mounted = typeof WeakMap === "function" ? new WeakMap() : null;

  // args: { box, cluster, namespace, namespaces, navigate?, repaint? }（由工作负载列表页渲染后调用）
  function decorateWorkloads(args) {
    var box = args && args.box;
    var toolbar = box && box.querySelector(".x-tbar");
    if (!toolbar || toolbar.querySelector("[data-xtp-view]")) return;
    var panel = toolbar.parentNode;
    var view = getView();
    var seg = document.createElement("span");
    seg.className = "xtp-seg";
    seg.setAttribute("role", "group");
    seg.setAttribute("aria-label", "视图");
    seg.setAttribute("data-xtp-view", "1");
    seg.innerHTML = '<button type="button" data-v="list" aria-pressed="' + (view === "list") + '"' + (view === "list" ? ' class="on"' : "") + ">列表</button>" +
      '<button type="button" data-v="topo" aria-pressed="' + (view === "topo") + '"' + (view === "topo" ? ' class="on"' : "") + ">拓扑</button>";
    toolbar.insertBefore(seg, toolbar.firstChild);
    var prev = mounted && mounted.get(box);
    if (prev) { prev.destroy(); mounted.delete(box); }
    [].slice.call(seg.querySelectorAll("button")).forEach(function (b) {
      b.onclick = function () {
        var v = b.getAttribute("data-v");
        if (v === getView()) return;
        setView(v);
        if (typeof args.repaint === "function") args.repaint();
        else { seg.remove(); decorateWorkloads(args); }
      };
    });
    if (view !== "topo") return;
    // 拓扑视图：隐藏与列表相关的控件和表格
    [].slice.call(toolbar.querySelectorAll("[data-live-kind],[data-live-q],[data-live-none]")).forEach(function (n) { n.hidden = true; n.style.display = "none"; });
    [].slice.call(panel.children).forEach(function (c) { if (c !== toolbar && !(c.classList && c.classList.contains("x-iamhd"))) { c.hidden = true; c.style.display = "none"; } });
    var meta = panel.querySelector(".x-iamhd .x-frac");
    if (meta) meta.textContent = "资源拓扑";
    var host = document.createElement("div");
    host.className = "xtp-host";
    panel.appendChild(host);
    var cluster = args.cluster || {};
    if (!args.namespace) {
      host.innerHTML = '<p class="xtp-empty">拓扑按命名空间展示，请在上方选择一个命名空间。</p>';
      return;
    }
    var inst = render(host, { clusterId: cluster.id, namespace: args.namespace, navigate: args.navigate });
    if (mounted && inst) mounted.set(box, inst);
  }

  var api = {
    render: render,
    decorateWorkloads: decorateWorkloads,
    setView: setView,
    getView: getView,
    // 纯函数（供测试与其它页面复用）
    normalize: normalize,
    fromInventory: fromInventory,
    buildGraph: buildGraph,
    filterByApp: filterByApp,
    foldPods: foldPods,
    layout: layout,
    edgePath: edgePath,
    summarize: summarize,
    podStatus: podStatus,
    workloadStatus: workloadStatus,
    selectorMatches: selectorMatches,
    ingressBackends: function (ing) { return ingressBackends(normalize({ ingresses: [ing] }).ingresses[0]); },
    deploymentForReplicaSet: deploymentForReplicaSet,
    detailTarget: detailTarget,
    sideHTML: sideHTML,
    sceneHTML: sceneHTML,
    demoData: demoData,
    constants: { NODE_W: NODE_W, NODE_H: NODE_H, COL_GAP: COL_GAP, ROW_GAP: ROW_GAP, PAD: PAD, POD_FOLD_THRESHOLD: POD_FOLD_THRESHOLD }
  };
  root.XSTopo = api;
})(typeof window !== "undefined" ? window : globalThis);
