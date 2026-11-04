/*
 * 创建工作负载向导（对标 KubeSphere「创建部署」：基本信息 → 容器 → 存储 → 访问方式 → 高级设置）。
 * 不懂 Kubernetes 也能填：资源规格预设、端口自动生成 Service、开关式 Ingress、健康检查一键开启；
 * 每一步都可切到 YAML 查看/修改最终会提交的全部对象，也可以用一句话让 AI 生成。
 *
 * 提交顺序：工作负载服务端 dry-run 校验 → 新建 PVC → 工作负载 → Service → Ingress。
 * 后端按资源类型分别走受控接口（/apply 只接受工作负载；Service/Ingress/PVC 各有专用接口和审批）。
 */
(function (root) {
  "use strict";

  var KINDS = [
    { v: "Deployment", t: "无状态服务", d: "Web、API 等可随意扩缩的服务" },
    { v: "StatefulSet", t: "有状态服务", d: "数据库、消息队列等需要固定标识和存储" },
    { v: "DaemonSet", t: "守护进程", d: "每个节点运行一份，如日志/监控采集" },
    { v: "Job", t: "任务", d: "运行一次直到完成，如数据迁移" },
    { v: "CronJob", t: "定时任务", d: "按 cron 计划周期运行" }
  ];
  var SIZES = [
    { v: "xs", t: "迷你 0.1 核 / 128Mi（上限 0.5 核 / 256Mi）", r: ["100m", "128Mi", "500m", "256Mi"] },
    { v: "s", t: "小 0.25 核 / 256Mi（上限 1 核 / 512Mi）", r: ["250m", "256Mi", "1", "512Mi"] },
    { v: "m", t: "中 0.5 核 / 512Mi（上限 2 核 / 1Gi）", r: ["500m", "512Mi", "2", "1Gi"] },
    { v: "l", t: "大 1 核 / 2Gi（上限 4 核 / 4Gi）", r: ["1", "2Gi", "4", "4Gi"] },
    { v: "custom", t: "自定义" },
    { v: "none", t: "不限制（不推荐）" }
  ];
  var DNS = "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$";
  var replicated = function (v) { return v.kind === "Deployment" || v.kind === "StatefulSet"; };
  var serving = function (v) { return v.kind === "Deployment" || v.kind === "StatefulSet" || v.kind === "DaemonSet"; };

  function sizeOf(c) {
    var s = SIZES.filter(function (x) { return x.v === c.size; })[0];
    if (s && s.r) return { requests: { cpu: s.r[0], memory: s.r[1] }, limits: { cpu: s.r[2], memory: s.r[3] } };
    if (c.size === "custom") {
      var o = { requests: {}, limits: {} };
      if (c.cpu_req) o.requests.cpu = c.cpu_req; if (c.mem_req) o.requests.memory = c.mem_req;
      if (c.cpu_lim) o.limits.cpu = c.cpu_lim; if (c.mem_lim) o.limits.memory = c.mem_lim;
      return o;
    }
    return null;
  }
  function parsePorts(list) {
    return (list || []).map(function (p) {
      var m = /^(\d{1,5})(?:\/(TCP|UDP|SCTP))?$/i.exec(String(p).trim());
      if (!m || +m[1] < 1 || +m[1] > 65535) throw new Error("端口格式应为 8080 或 53/UDP：" + p);
      return { containerPort: +m[1], protocol: (m[2] || "TCP").toUpperCase() };
    });
  }
  function lines(s) { return String(s || "").split("\n").map(function (x) { return x.trim(); }).filter(Boolean); }
  function shellSplit(s) {
    var out = [], cur = "", q = "";
    String(s || "").trim().split("").forEach(function (ch) {
      if (q) { if (ch === q) q = ""; else cur += ch; }
      else if (ch === '"' || ch === "'") q = ch;
      else if (/\s/.test(ch)) { if (cur) { out.push(cur); cur = ""; } }
      else cur += ch;
    });
    if (cur) out.push(cur);
    return out;
  }
  function kv(list) { return root.XSFormKit.kvToMap(list); }

  /* ---------------------------------------------------------- 表单值 → 对象 */

  function build(v) {
    var labels = Object.assign({ app: v.name }, kv(v.labels));
    var containers = (v.containers || []).map(function (c, i) {
      var o = { name: c.name || (i ? v.name + "-" + i : v.name), image: c.image };
      if (c.pull && c.pull !== "IfNotPresent") o.imagePullPolicy = c.pull;
      var ports = parsePorts(c.ports);
      if (ports.length) o.ports = ports.map(function (p, j) { var x = { name: (p.protocol === "TCP" ? "tcp" : p.protocol.toLowerCase()) + "-" + p.containerPort, containerPort: p.containerPort }; if (p.protocol !== "TCP") x.protocol = p.protocol; return x; });
      if (c.command) o.command = shellSplit(c.command);
      if (c.args) o.args = shellSplit(c.args);
      var env = (c.env || []).filter(function (e) { return e.k; }).map(function (e) { return { name: e.k, value: e.v }; });
      if (env.length) o.env = env;
      var res = sizeOf(c);
      if (res) o.resources = res;
      if (c.probe && c.probe !== "none") {
        var port = +c.probe_port || ports[0] && ports[0].containerPort;
        if (!port) throw new Error("容器 " + o.name + "：健康检查需要端口");
        var p = c.probe === "http" ? { httpGet: { path: c.probe_path || "/", port: port } } : { tcpSocket: { port: port } };
        o.readinessProbe = Object.assign({ initialDelaySeconds: 5, periodSeconds: 10 }, JSON.parse(JSON.stringify(p)));
        o.livenessProbe = Object.assign({ initialDelaySeconds: 15, periodSeconds: 20, failureThreshold: 3 }, JSON.parse(JSON.stringify(p)));
      }
      var mounts = (v.volumes || []).filter(function (vol) { return vol.mount && (!vol.container || vol.container === o.name); })
        .map(function (vol) { return { name: vol.name, mountPath: vol.mount, readOnly: vol.type === "configMap" || vol.type === "secret" ? true : undefined }; });
      if (mounts.length) o.volumeMounts = JSON.parse(JSON.stringify(mounts));
      return o;
    });
    var podSpec = { containers: containers };
    var vols = [], claims = [], pvcs = [];
    (v.volumes || []).forEach(function (vol) {
      if (vol.type === "emptyDir") vols.push({ name: vol.name, emptyDir: {} });
      else if (vol.type === "configMap") vols.push({ name: vol.name, configMap: { name: vol.source } });
      else if (vol.type === "secret") vols.push({ name: vol.name, secret: { secretName: vol.source } });
      else if (vol.type === "pvc") vols.push({ name: vol.name, persistentVolumeClaim: { claimName: vol.source } });
      else if (vol.type === "newpvc") {
        var spec = { accessModes: ["ReadWriteOnce"], resources: { requests: { storage: vol.size || "1Gi" } } };
        if (vol.storage_class) spec.storageClassName = vol.storage_class;
        if (v.kind === "StatefulSet") claims.push({ metadata: { name: vol.name }, spec: spec });
        else {
          var pvcName = v.name + "-" + vol.name;
          pvcs.push({ apiVersion: "v1", kind: "PersistentVolumeClaim", metadata: { name: pvcName, namespace: v.namespace, labels: { app: v.name } }, spec: spec });
          vols.push({ name: vol.name, persistentVolumeClaim: { claimName: pvcName } });
        }
      }
    });
    if (vols.length) podSpec.volumes = vols;
    var ns = kv(v.node_selector);
    if (Object.keys(ns).length) podSpec.nodeSelector = ns;
    if (v.pull_secret) podSpec.imagePullSecrets = [{ name: v.pull_secret }];
    var template = { metadata: { labels: labels }, spec: podSpec };
    var meta = { name: v.name, namespace: v.namespace, labels: labels };
    if (v.desc) meta.annotations = { "kubesphere.io/description": v.desc, "xingshu.io/description": v.desc };
    var wl;
    if (v.kind === "Deployment") {
      wl = { apiVersion: "apps/v1", kind: "Deployment", metadata: meta, spec: { replicas: +v.replicas || 1, selector: { matchLabels: { app: v.name } }, template: template } };
      if (v.strategy === "Recreate") wl.spec.strategy = { type: "Recreate" };
    } else if (v.kind === "StatefulSet") {
      wl = { apiVersion: "apps/v1", kind: "StatefulSet", metadata: meta, spec: { replicas: +v.replicas || 1, serviceName: v.name, selector: { matchLabels: { app: v.name } }, template: template } };
      if (claims.length) wl.spec.volumeClaimTemplates = claims;
    } else if (v.kind === "DaemonSet") {
      wl = { apiVersion: "apps/v1", kind: "DaemonSet", metadata: meta, spec: { selector: { matchLabels: { app: v.name } }, template: template } };
    } else {
      podSpec.restartPolicy = v.restart || "Never";
      var job = { backoffLimit: +v.backoff >= 0 ? +v.backoff : 3, template: template };
      wl = v.kind === "Job" ? { apiVersion: "batch/v1", kind: "Job", metadata: meta, spec: job }
        : { apiVersion: "batch/v1", kind: "CronJob", metadata: meta, spec: Object.assign({ schedule: v.schedule || "0 * * * *", jobTemplate: { spec: job } }, v.timezone ? { timeZone: v.timezone } : {}) };
    }
    var objects = pvcs.concat([wl]);
    var svcPorts = [];
    containers.forEach(function (c) { (c.ports || []).forEach(function (p) { svcPorts.push({ name: p.name, port: p.containerPort, targetPort: p.containerPort, protocol: p.protocol || "TCP" }); }); });
    if (serving(v) && v.svc && svcPorts.length) {
      var svc = { apiVersion: "v1", kind: "Service", metadata: { name: v.name, namespace: v.namespace, labels: { app: v.name } }, spec: { type: v.svc_type || "ClusterIP", selector: { app: v.name }, ports: svcPorts } };
      if (v.kind === "StatefulSet" && v.svc_type === "Headless") { svc.spec.type = "ClusterIP"; svc.spec.clusterIP = "None"; }
      objects.push(svc);
      if (v.ing && v.ing_host) {
        var backendPort = +v.ing_port || svcPorts[0].port;
        var ing = { apiVersion: "networking.k8s.io/v1", kind: "Ingress", metadata: { name: v.name, namespace: v.namespace, labels: { app: v.name } },
          spec: { rules: [{ host: v.ing_host, http: { paths: [{ path: v.ing_path || "/", pathType: "Prefix", backend: { service: { name: v.name, port: { number: backendPort } } } }] } }] } };
        if (v.ing_class) ing.spec.ingressClassName = v.ing_class;
        if (v.ing_tls) ing.spec.tls = [{ hosts: [v.ing_host], secretName: v.ing_tls }];
        objects.push(ing);
      }
    }
    return { apiVersion: "v1", kind: "List", items: objects };
  }

  /* ---------------------------------------------------------- 对象 → 表单值（YAML 手动修改后回填） */

  function parse(obj, prev) {
    var items = obj.kind === "List" ? obj.items || [] : [obj];
    var wl = items.filter(function (o) { return /^(Deployment|StatefulSet|DaemonSet|Job|CronJob)$/.test(o.kind); })[0];
    if (!wl) throw new Error("YAML 中没有工作负载（Deployment / StatefulSet / DaemonSet / Job / CronJob）");
    var unsupported = items.filter(function (o) { return !/^(Deployment|StatefulSet|DaemonSet|Job|CronJob|Service|Ingress|PersistentVolumeClaim)$/.test(o.kind); });
    if (unsupported.length) throw new Error("向导不支持的资源类型：" + unsupported.map(function (o) { return o.kind; }).join(", ") + "（请到对应页面创建）");
    var meta = wl.metadata || {}, spec = wl.spec || {};
    var jobSpec = wl.kind === "CronJob" ? (spec.jobTemplate || {}).spec || {} : spec;
    var tpl = (wl.kind === "Job" || wl.kind === "CronJob" ? jobSpec.template : spec.template) || {};
    var pod = tpl.spec || {};
    var labels = Object.assign({}, meta.labels || {}); delete labels.app;
    var volsByName = {};
    (pod.volumes || []).forEach(function (vol) { volsByName[vol.name] = vol; });
    var pvcObjs = items.filter(function (o) { return o.kind === "PersistentVolumeClaim"; });
    var volumes = [];
    Object.keys(volsByName).forEach(function (n) {
      var vol = volsByName[n], row = { name: n };
      if (vol.emptyDir) row.type = "emptyDir";
      else if (vol.configMap) { row.type = "configMap"; row.source = vol.configMap.name; }
      else if (vol.secret) { row.type = "secret"; row.source = vol.secret.secretName; }
      else if (vol.persistentVolumeClaim) {
        var claim = vol.persistentVolumeClaim.claimName, newer = pvcObjs.filter(function (p) { return p.metadata && p.metadata.name === claim; })[0];
        if (newer) { row.type = "newpvc"; row.size = (((newer.spec || {}).resources || {}).requests || {}).storage || "1Gi"; row.storage_class = (newer.spec || {}).storageClassName || ""; }
        else { row.type = "pvc"; row.source = claim; }
      } else throw new Error("向导不支持卷 " + n + " 的类型，请在 YAML 页面直接创建");
      volumes.push(row);
    });
    (spec.volumeClaimTemplates || []).forEach(function (c) {
      volumes.push({ name: c.metadata && c.metadata.name, type: "newpvc", size: (((c.spec || {}).resources || {}).requests || {}).storage || "1Gi", storage_class: (c.spec || {}).storageClassName || "" });
    });
    var containers = (pod.containers || []).map(function (c) {
      volumes.forEach(function (row) {
        (c.volumeMounts || []).forEach(function (m) { if (m.name === row.name) { row.mount = m.mountPath; if ((pod.containers || []).length > 1) row.container = c.name; } });
      });
      var res = c.resources || {}, rq = res.requests || {}, lm = res.limits || {};
      var size = "none";
      if (rq.cpu || rq.memory || lm.cpu || lm.memory) {
        size = "custom";
        SIZES.forEach(function (s) { if (s.r && s.r[0] === rq.cpu && s.r[1] === rq.memory && s.r[2] === lm.cpu && s.r[3] === lm.memory) size = s.v; });
      }
      var probe = c.readinessProbe || c.livenessProbe || null;
      return {
        name: c.name, image: c.image, pull: c.imagePullPolicy || "IfNotPresent",
        ports: (c.ports || []).map(function (p) { return p.containerPort + (p.protocol && p.protocol !== "TCP" ? "/" + p.protocol : ""); }),
        command: (c.command || []).join(" "), args: (c.args || []).join(" "),
        env: (c.env || []).filter(function (e) { return e.value != null; }).map(function (e) { return { k: e.name, v: String(e.value) }; }),
        size: size, cpu_req: rq.cpu || "", mem_req: rq.memory || "", cpu_lim: lm.cpu || "", mem_lim: lm.memory || "",
        probe: probe ? (probe.httpGet ? "http" : "tcp") : "none", probe_path: probe && probe.httpGet ? probe.httpGet.path : "", probe_port: probe ? String((probe.httpGet || probe.tcpSocket || {}).port || "") : ""
      };
    });
    var svc = items.filter(function (o) { return o.kind === "Service"; })[0];
    var ing = items.filter(function (o) { return o.kind === "Ingress"; })[0];
    var rule = ing && ((ing.spec || {}).rules || [])[0] || {}, path = rule.http && (rule.http.paths || [])[0] || {};
    return {
      kind: wl.kind, name: meta.name || "", namespace: meta.namespace || prev.namespace, desc: (meta.annotations || {})["xingshu.io/description"] || "",
      replicas: spec.replicas || 1, strategy: spec.strategy && spec.strategy.type === "Recreate" ? "Recreate" : "RollingUpdate",
      schedule: spec.schedule || prev.schedule, timezone: spec.timeZone || "", restart: pod.restartPolicy || "Never", backoff: jobSpec.backoffLimit != null ? jobSpec.backoffLimit : 3,
      containers: containers, volumes: volumes,
      svc: !!svc, svc_type: svc ? ((svc.spec || {}).clusterIP === "None" ? "Headless" : (svc.spec || {}).type || "ClusterIP") : prev.svc_type,
      ing: !!ing, ing_host: rule.host || "", ing_path: path.path || "/", ing_port: path.backend && path.backend.service && path.backend.service.port ? String(path.backend.service.port.number || "") : "",
      ing_class: ing && (ing.spec || {}).ingressClassName || "", ing_tls: ing && ((ing.spec || {}).tls || [])[0] ? ing.spec.tls[0].secretName : "",
      labels: root.XSFormKit.mapToKv(labels), node_selector: root.XSFormKit.mapToKv(pod.nodeSelector || {}),
      pull_secret: pod.imagePullSecrets && pod.imagePullSecrets[0] ? pod.imagePullSecrets[0].name : ""
    };
  }

  /* ---------------------------------------------------------- 向导 */

  function open(args, done) {
    var rt = args.runtime, clusterId = args.cluster.id;
    var base = "/clusters/" + encodeURIComponent(clusterId);
    var nsList = (args.namespaces || []).map(function (n) { return n.name || n.metadata && n.metadata.name; }).filter(Boolean);
    var initialKind = { deployments: "Deployment", statefulsets: "StatefulSet", daemonsets: "DaemonSet", jobs: "Job", cronjobs: "CronJob" }[args.kind] || "Deployment";
    var values = {
      kind: initialKind, namespace: args.namespace || nsList[0] || "default", replicas: 1, strategy: "RollingUpdate", schedule: "0 2 * * *", restart: "Never", backoff: 3,
      containers: [{ image: "", pull: "IfNotPresent", ports: [], size: "s", probe: "none" }], volumes: [],
      svc: true, svc_type: "ClusterIP", ing: false, ing_path: "/", labels: [], node_selector: []
    };
    var yaml = function (obj) { return obj.items.map(function (o) { return root.XSFormKit.dump(o); }).join("---\n"); };
    var kit = root.XSFormKit.open({
      title: "创建工作负载", subtitle: args.cluster.name ? "集群 " + args.cluster.name : "",
      scene: "k8s.workload.wizard",
      aiExample: "在 shop 命名空间部署 nginx:1.27，2 个副本，端口 80，用域名 shop.example.com 对外访问",
      aiContext: function () { return "可选命名空间：" + JSON.stringify(nsList); },
      wizard: true, width: "xl", okText: "创建",
      values: values,
      sections: [
        { title: "基本信息", fields: [
          { k: "kind", type: "radio", options: KINDS, required: true },
          { k: "name", label: "名称", required: true, pattern: DNS, patternMsg: "小写字母、数字、中划线，≤63", placeholder: "shop-web", help: "同时用作 Service、Ingress 的名称和 app 标签" },
          { k: "namespace", label: "项目 / 命名空间", type: nsList.length ? "select" : "text", required: true, options: nsList.map(function (n) { return { v: n, t: n }; }) },
          { k: "replicas", label: "副本数", type: "number", min: 0, max: 500, show: replicated, help: "运行几个实例；无状态服务建议 ≥2" },
          { k: "strategy", label: "更新策略", type: "select", required: true, show: function (v) { return v.kind === "Deployment"; },
            options: [{ v: "RollingUpdate", t: "滚动更新（不停机）" }, { v: "Recreate", t: "重新创建（先停旧再起新）" }] },
          { k: "schedule", label: "执行计划（cron）", mono: true, required: true, show: function (v) { return v.kind === "CronJob"; }, help: "分 时 日 月 周，例如 0 2 * * * 表示每天 02:00",
            pattern: "^(\\S+\\s+){4}\\S+$|^@(yearly|annually|monthly|weekly|daily|hourly)$", patternMsg: "需要 5 段 cron 表达式" },
          { k: "timezone", label: "时区", placeholder: "Asia/Shanghai", show: function (v) { return v.kind === "CronJob"; } },
          { k: "restart", label: "失败后", type: "select", required: true, show: function (v) { return v.kind === "Job" || v.kind === "CronJob"; }, options: [{ v: "Never", t: "新建 Pod 重试" }, { v: "OnFailure", t: "原地重启容器" }] },
          { k: "backoff", label: "最多重试次数", type: "number", min: 0, max: 20, show: function (v) { return v.kind === "Job" || v.kind === "CronJob"; } },
          { k: "desc", label: "描述", full: true, placeholder: "这个服务是做什么的（可选）" }
        ] },
        { title: "容器", desc: "填镜像和端口即可运行；资源规格决定调度时预留多少 CPU/内存，上限防止拖垮节点。", fields: [
          { k: "containers", type: "list", itemTitle: "容器", addText: "添加容器（Sidecar）", newItem: { pull: "IfNotPresent", size: "xs", probe: "none", ports: [] }, item: [
            { k: "image", label: "镜像", required: true, mono: true, full: true, placeholder: "nginx:1.27 或 harbor.example.com/shop/web:v1.2.0" },
            { k: "name", label: "容器名", mono: true, placeholder: "默认同工作负载名" },
            { k: "ports", label: "端口", type: "tags", placeholder: "8080, 9090/UDP", help: "填了端口才能创建 Service 对外访问" },
            { k: "size", label: "资源规格", type: "select", required: true, options: SIZES.map(function (s) { return { v: s.v, t: s.t }; }) },
            { k: "pull", label: "拉取策略", type: "select", required: true, options: [{ v: "IfNotPresent", t: "本地没有时拉取" }, { v: "Always", t: "每次都拉取（latest 标签用）" }, { v: "Never", t: "只用本地镜像" }] },
            { k: "cpu_req", label: "CPU 预留", mono: true, placeholder: "250m", show: function (c) { return c.size === "custom"; } },
            { k: "cpu_lim", label: "CPU 上限", mono: true, placeholder: "1", show: function (c) { return c.size === "custom"; } },
            { k: "mem_req", label: "内存预留", mono: true, placeholder: "256Mi", show: function (c) { return c.size === "custom"; } },
            { k: "mem_lim", label: "内存上限", mono: true, placeholder: "512Mi", show: function (c) { return c.size === "custom"; } },
            { k: "probe", label: "健康检查", type: "select", required: true, options: [{ v: "none", t: "不检查" }, { v: "http", t: "HTTP 请求" }, { v: "tcp", t: "TCP 端口" }],
              help: "开启后未就绪的实例不接流量，卡死会自动重启" },
            { k: "probe_path", label: "检查路径", mono: true, placeholder: "/healthz", show: function (c) { return c.probe === "http"; } },
            { k: "probe_port", label: "检查端口", type: "number", placeholder: "默认第一个端口", show: function (c) { return c.probe && c.probe !== "none"; } },
            { k: "env", label: "环境变量", type: "kv", addText: "添加变量", keyPh: "APP_ENV", valPh: "prod", full: true, help: "密码等敏感值请用 Secret 挂载，不要写在这里" },
            { k: "command", label: "启动命令（可选）", mono: true, placeholder: "覆盖镜像的 ENTRYPOINT，如 /app/server" },
            { k: "args", label: "启动参数（可选）", mono: true, placeholder: "--port 8080 --log-level info" }
          ] }
        ], validate: function (v) {
          if (!v.containers || !v.containers.length) return "至少需要一个容器";
          for (var i = 0; i < v.containers.length; i++) {
            var c = v.containers[i];
            if (!c.image) return "容器 " + (i + 1) + "：请填写镜像";
            if (/\s/.test(c.image)) return "容器 " + (i + 1) + "：镜像地址不能有空格";
            try { parsePorts(c.ports); } catch (e) { return "容器 " + (i + 1) + "：" + e.message; }
            if (c.name && !new RegExp(DNS).test(c.name)) return "容器 " + (i + 1) + "：容器名格式不正确";
          }
          return "";
        } },
        { title: "存储", desc: "容器重启后文件会丢失；需要保留数据就挂载持久卷。有状态服务的新建持久卷会为每个副本各创建一块。", fields: [
          { k: "volumes", type: "list", itemTitle: "存储卷", addText: "添加存储卷", newItem: { type: "newpvc", size: "1Gi" }, item: [
            { k: "type", label: "类型", type: "select", required: true, options: [
              { v: "newpvc", t: "新建持久卷（PVC）" }, { v: "pvc", t: "使用已有持久卷" }, { v: "emptyDir", t: "临时目录（随 Pod 删除）" },
              { v: "configMap", t: "配置文件（ConfigMap）" }, { v: "secret", t: "密钥（Secret）" }] },
            { k: "name", label: "卷名", required: true, mono: true, placeholder: "data" },
            { k: "mount", label: "挂载路径", required: true, mono: true, placeholder: "/var/lib/data" },
            { k: "container", label: "挂载到容器", mono: true, placeholder: "留空=所有容器" },
            { k: "size", label: "容量", mono: true, placeholder: "10Gi", show: function (r) { return r.type === "newpvc"; } },
            { k: "storage_class", label: "存储类", mono: true, placeholder: "留空用默认存储类", show: function (r) { return r.type === "newpvc"; } },
            { k: "source", label: "来源名称", mono: true, placeholder: "已有 PVC / ConfigMap / Secret 名称", show: function (r) { return r.type === "pvc" || r.type === "configMap" || r.type === "secret"; } }
          ] }
        ], validate: function (v) {
          var seen = {};
          for (var i = 0; i < (v.volumes || []).length; i++) {
            var r = v.volumes[i], n = "存储卷 " + (i + 1) + "：";
            if (!r.name || !new RegExp(DNS).test(r.name)) return n + "卷名需为小写字母、数字、中划线";
            if (seen[r.name]) return n + "卷名重复"; seen[r.name] = 1;
            if (!r.mount || r.mount.charAt(0) !== "/") return n + "挂载路径需以 / 开头";
            if (r.type === "newpvc" && !/^\d+(\.\d+)?(Mi|Gi|Ti)$/.test(r.size || "")) return n + "容量格式如 10Gi";
            if ((r.type === "pvc" || r.type === "configMap" || r.type === "secret") && !r.source) return n + "请填写来源名称";
            if (r.container && !(v.containers || []).some(function (c, j) { return (c.name || (j ? v.name + "-" + j : v.name)) === r.container; })) return n + "找不到容器 " + r.container;
          }
          return "";
        } },
        { title: "访问方式", show: serving, desc: "Service 给服务一个固定的集群内地址；需要从浏览器访问再开启 Ingress 绑定域名。", fields: [
          { k: "svc", label: "集群内访问", type: "switch", text: "创建 Service（按容器端口自动生成）" },
          { k: "svc_type", label: "Service 类型", type: "select", required: true, show: function (v) { return v.svc; }, options: [
            { v: "ClusterIP", t: "仅集群内（ClusterIP）" }, { v: "NodePort", t: "节点端口（NodePort，30000-32767）" }, { v: "LoadBalancer", t: "负载均衡（LoadBalancer）" }, { v: "Headless", t: "Headless（有状态服务固定域名）" }] },
          { k: "ing", label: "域名访问", type: "switch", text: "创建 Ingress", show: function (v) { return v.svc; } },
          { k: "ing_host", label: "域名", mono: true, placeholder: "shop.example.com", required: true, show: function (v) { return v.svc && v.ing; },
            pattern: "^([a-z0-9]([-a-z0-9]*[a-z0-9])?\\.)+[a-z]{2,}$|^\\*\\.([a-z0-9-]+\\.)+[a-z]{2,}$", patternMsg: "填写域名，不带 http://" },
          { k: "ing_path", label: "路径", mono: true, show: function (v) { return v.svc && v.ing; } },
          { k: "ing_port", label: "转发到端口", type: "number", placeholder: "默认第一个端口", show: function (v) { return v.svc && v.ing; } },
          { k: "ing_class", label: "Ingress Class", mono: true, placeholder: "留空用默认（如 traefik / nginx）", show: function (v) { return v.svc && v.ing; } },
          { k: "ing_tls", label: "HTTPS 证书 Secret", mono: true, placeholder: "留空=仅 HTTP", show: function (v) { return v.svc && v.ing; } }
        ], validate: function (v) {
          if (v.svc && !(v.containers || []).some(function (c) { return (c.ports || []).length; })) return "要创建 Service 需在「容器」步骤填写至少一个端口；不需要可关闭「集群内访问」";
          return "";
        } },
        { title: "高级设置", fields: [
          { k: "labels", label: "标签", type: "kv", addText: "添加标签", keyPh: "team", valPh: "shop", help: "app=<名称> 会自动添加" },
          { k: "node_selector", label: "调度到指定节点", type: "kv", addText: "添加节点标签", keyPh: "node-role.kubernetes.io/worker", valPh: "true" },
          { k: "pull_secret", label: "镜像拉取凭证", mono: true, placeholder: "私有镜像仓库的 Secret 名称（可选）" }
        ] }
      ],
      toObject: build,
      fromObject: function (o, prev) { return parse(o, prev); },
      afterAI: function (v) {
        if (!Array.isArray(v.containers) || !v.containers.length) v.containers = [{ image: v.image || "", ports: v.ports || [], size: "s", pull: "IfNotPresent", probe: "none" }];
        v.containers = v.containers.map(function (c) { return Object.assign({ size: "s", pull: "IfNotPresent", probe: "none", ports: [] }, c, { ports: (c.ports || []).map(String) }); });
        if (v.ing_host) { v.svc = true; v.ing = true; }
        if (nsList.length && nsList.indexOf(v.namespace) < 0) v.namespace = args.namespace || nsList[0];
      },
      submit: function (v, obj, ctx) { return submit(v, obj, ctx); }
    });
    return kit;

    function submit(v, obj, ctx) {
      var wl = obj.items.filter(function (o) { return /^(Deployment|StatefulSet|DaemonSet|Job|CronJob)$/.test(o.kind); })[0];
      var others = obj.items.filter(function (o) { return o !== wl; });
      var wlYaml = root.XSFormKit.dump(wl);
      var done_ = [];
      ctx.status("服务端校验中…", true);
      return rt.apiPost(base + "/apply", { yaml: wlYaml, namespace: v.namespace, dry_run: true, force: false }).then(function (r) {
        var issue = rt.applyIssue(r);
        if (issue) throw new Error("校验未通过：" + issue);
        return rt.apiGet(base + "/workloads/" + wl.kind.toLowerCase() + "s", { namespace: v.namespace });
      }).then(function (list) {
        if (list && Array.isArray(list.items) && list.items.some(function (x) { return x.name === v.name; })) throw new Error("命名空间 " + v.namespace + " 下已有同名 " + wl.kind);
        var pvcs = others.filter(function (o) { return o.kind === "PersistentVolumeClaim"; });
        return pvcs.reduce(function (p, pvc) {
          return p.then(function () {
            ctx.status("创建持久卷 " + pvc.metadata.name + "…", true);
            var s = pvc.spec;
            return rt.apiPost(base + "/storage/claims", { name: pvc.metadata.name, namespace: v.namespace, storage_class: s.storageClassName || "", access_modes: s.accessModes, size: s.resources.requests.storage, labels: pvc.metadata.labels })
              .then(function () { done_.push("PVC " + pvc.metadata.name); });
          });
        }, Promise.resolve());
      }).then(function () {
        ctx.status("创建 " + wl.kind + "…", true);
        return rt.apiPost(base + "/apply", { yaml: wlYaml, namespace: v.namespace, force: false }).then(function (r) {
          var issue = rt.applyIssue(r);
          if (issue && !/审批/.test(issue)) throw new Error(issue);
          done_.push(wl.kind + " " + v.name + (issue ? "（" + issue + "）" : ""));
        });
      }).then(function () {
        var svc = others.filter(function (o) { return o.kind === "Service"; })[0];
        if (!svc) return;
        ctx.status("创建 Service…", true);
        return rt.apiPost(base + "/network/services", { name: svc.metadata.name, namespace: v.namespace, type: svc.spec.type, cluster_ip: svc.spec.clusterIP || "", selector: svc.spec.selector, ports: svc.spec.ports, labels: svc.metadata.labels })
          .then(function () { done_.push("Service " + svc.metadata.name); });
      }).then(function () {
        var ing = others.filter(function (o) { return o.kind === "Ingress"; })[0];
        if (!ing) return;
        ctx.status("创建 Ingress…", true);
        return rt.apiPost(base + "/network/ingresses", { name: ing.metadata.name, namespace: v.namespace, class_name: ing.spec.ingressClassName || null, rules: ing.spec.rules, tls: ing.spec.tls || [], labels: ing.metadata.labels })
          .then(function () { done_.push("Ingress " + ing.metadata.name); });
      }).then(function () {
        ctx.close();
        if (rt.toast) rt.toast("已创建：" + done_.join("、"));
        if (done) done();
      }, function (err) {
        var msg = root.XSFormKit.errText(err);
        throw new Error((done_.length ? "已创建 " + done_.join("、") + "；后续失败：" : "") + msg);
      });
    }
  }

  root.XSWorkloadWizard = { open: open, build: build, parse: parse };
})(window);
