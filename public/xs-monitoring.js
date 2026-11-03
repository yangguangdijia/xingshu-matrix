/* 监控面板（参照 KubeSphere「集群监控 / 应用资源」）。数据全部来自已绑定的 Prometheus：
 *   GET /observability/prometheus/:id/query        即时查询
 *   GET /observability/prometheus/:id/query_range  区间查询
 * 标签页：集群资源 · 节点 · 工作负载 · API Server。全局时间范围与自动刷新。
 * 依赖的指标：node-exporter、kube-state-metrics、cAdvisor(kubelet)、apiserver。
 */
(function (root) {
  "use strict";
  var TABS = [{ k: "cluster", t: "集群资源" }, { k: "nodes", t: "节点" }, { k: "workloads", t: "工作负载" }, { k: "apiserver", t: "API Server" }];
  var RANGES = [{ v: "15m", t: "近 15 分钟", s: 15 }, { v: "1h", t: "近 1 小时", s: 30 }, { v: "6h", t: "近 6 小时", s: 120 }, { v: "24h", t: "近 24 小时", s: 300 }, { v: "7d", t: "近 7 天", s: 1800 }];
  var REFRESH = [{ v: 0, t: "不自动刷新" }, { v: 30, t: "每 30 秒" }, { v: 60, t: "每 1 分钟" }, { v: 300, t: "每 5 分钟" }];
  var COLORS = ["#2563eb", "#16a34a", "#d97706", "#7c3aed", "#dc2626", "#0891b2", "#db2777", "#65a30d"];
  // 节点本地块设备文件系统；每个 device 只计一次（同一设备可能挂在多个挂载点）
  var FS = 'fstype=~"ext[234]|xfs|btrfs|zfs"';
  function fsSum(metric) { return 'sum by(instance)(max by(instance,device)(' + metric + '{' + FS + '}))'; }
  var NET = 'device!~"lo|veth.*|cni.*|flannel.*|cali.*|docker.*|br-.*|vxlan.*|tunl.*"';
  var CPOD = 'container!="",container!="POD",image!=""';
  var OWNER = ' * on(namespace,pod) group_left(owner_kind,owner_name) max by(namespace,pod,owner_kind,owner_name)(kube_pod_owner)';

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function store(k, v) { try { if (v === undefined) return root.localStorage.getItem("xs.mon." + k); root.localStorage.setItem("xs.mon." + k, v); } catch (e) { return null; } }
  function css() {
    if (document.getElementById("xs-mon-css")) return;
    var s = document.createElement("style"); s.id = "xs-mon-css";
    s.textContent = [
      ".xm-bar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin:4px 0 20px}",
      ".xm-tabs{display:inline-flex;background:#f1f3f6;border-radius:8px;padding:3px}",
      ".xm-tabs button{border:0;background:none;padding:7px 18px;border-radius:6px;font-size:14px;color:#4b5563;cursor:pointer}",
      ".xm-tabs button.on{background:#fff;color:#111827;font-weight:600;box-shadow:0 1px 2px rgba(0,0,0,.08)}",
      ".xm-bar .xm-sp{flex:1}.xm-bar select{width:auto}.xm-upd{font-size:12px;color:#8a919c}",
      ".xm-stats{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:16px;margin-bottom:16px}",
      ".xm-stat{background:#fff;border:1px solid #e6e9ed;border-radius:10px;padding:18px 20px}",
      ".xm-stat p{margin:0 0 10px;font-size:13px;color:#6b7280}.xm-stat strong{font-size:24px;font-weight:600;color:#111827}",
      ".xm-stat small{font-size:13px;color:#6b7280;margin-left:4px;font-weight:400}",
      ".xm-meter{height:6px;background:#eef1f5;border-radius:3px;margin-top:12px;overflow:hidden}.xm-meter i{display:block;height:100%;border-radius:3px}",
      ".xm-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}",
      ".xm-card{background:#fff;border:1px solid #e6e9ed;border-radius:10px;padding:18px 20px 14px;min-width:0}",
      ".xm-card header{display:flex;align-items:baseline;justify-content:space-between;margin-bottom:12px}",
      ".xm-card h4{margin:0;font-size:14px;font-weight:600;color:#1f2937}.xm-card header span{font-size:13px;color:#6b7280}",
      ".xm-chart{position:relative;height:220px}.xm-empty{height:220px;display:flex;align-items:center;justify-content:center;color:#9aa1ab;font-size:13px}",
      ".xm-table{width:100%;border-collapse:collapse;background:#fff;font-size:14px}",
      ".xm-tw{background:#fff;border:1px solid #e6e9ed;border-radius:10px;overflow:auto}",
      ".xm-table th{text-align:left;font-weight:500;color:#6b7280;font-size:13px;padding:12px 16px;border-bottom:1px solid #eef0f3;white-space:nowrap;background:#fafbfc}",
      ".xm-table td{padding:13px 16px;border-bottom:1px solid #f1f3f5;white-space:nowrap}.xm-table tr:last-child td{border-bottom:0}",
      ".xm-table tr[data-xm-row]{cursor:pointer}.xm-table tr[data-xm-row]:hover td{background:#f8fafc}",
      ".xm-pct{display:flex;align-items:center;gap:10px;min-width:150px}.xm-pct .xm-meter{flex:1;margin:0;max-width:110px}.xm-pct b{font-weight:500;min-width:48px}",
      ".xm-detail td{background:#fbfcfd;padding:16px}.xm-detail .xm-grid{gap:12px}",
      ".xm-filter{display:flex;gap:12px;align-items:center;margin-bottom:16px}.xm-filter select{width:auto}",
      ".xm-err{color:#b42318;font-size:13px;margin:0 0 12px}",
      "@media (max-width:1100px){.xm-grid{grid-template-columns:1fr}.xm-stats{grid-template-columns:repeat(2,minmax(0,1fr))}}"
    ].join("");
    document.head.appendChild(s);
  }
  function meterColor(p) { return p >= 90 ? "#dc2626" : p >= 75 ? "#d97706" : "#16a34a"; }
  function meter(p) { p = Math.max(0, Math.min(100, Number(p) || 0)); return '<div class="xm-meter"><i style="width:' + p.toFixed(1) + '%;background:' + meterColor(p) + '"></i></div>'; }
  function fmtBytes(b) {
    if (b == null || !isFinite(b)) return "—";
    var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0; b = Number(b);
    while (Math.abs(b) >= 1024 && i < u.length - 1) { b /= 1024; i++; }
    return (i ? b.toFixed(b >= 100 ? 0 : 1) : b.toFixed(0)) + " " + u[i];
  }
  function fmtRate(b) { return b == null ? "—" : fmtBytes(b) + "/s"; }
  function fmtCores(c) { return c == null || !isFinite(c) ? "—" : (c < 1 ? (c * 1000).toFixed(0) + "m" : Number(c).toFixed(2)); }
  function fmtNum(n, d) { return n == null || !isFinite(n) ? "—" : Number(n).toFixed(d == null ? 0 : d); }

  function Monitor(host, ctx) {
    this.host = host; this.ctx = ctx; this.charts = [];
    this.tab = store("tab") || "cluster";
    this.range = store("range") || "1h";
    this.refresh = Number(store("refresh") || 60);
    this.ns = store("ns") || ""; this.sort = store("sort") || "cpu";
    this.seq = 0; this.timer = null;
  }
  Monitor.prototype.q = function (query) {
    var c = this.ctx;
    return root.XS.get("/observability/prometheus/" + encodeURIComponent(c.source.ds.id) + "/query", { cluster_id: c.cluster.id, query: scoped(query, c.source) })
      .then(function (r) { return (r && r.data && r.data.result) || (r && r.result) || []; });
  };
  Monitor.prototype.qr = function (query) {
    var c = this.ctx, r = RANGES.filter(function (x) { return x.v === this.range; }, this)[0] || RANGES[1];
    return root.XS.get("/observability/prometheus/" + encodeURIComponent(c.source.ds.id) + "/query_range", {
      cluster_id: c.cluster.id, query: scoped(query, c.source), start: "now-" + r.v, end: "now", step: r.s + "s"
    }).then(function (res) { return (res && res.data && res.data.result) || (res && res.result) || []; });
  };
  function scoped(q, source) {
    try { return root.XSObserve && root.XSObserve.scopedPromQuery ? root.XSObserve.scopedPromQuery(q, source, true) : q; } catch (e) { return q; }
  }
  function val(series) { if (!series || !series.length) return null; var v = series[0].value || (series[0].values || []).slice(-1)[0]; var n = v ? parseFloat(v[1]) : NaN; return isFinite(n) ? n : null; }
  function settle(p) { return p.then(function (v) { return { ok: true, v: v }; }, function (e) { return { ok: false, e: e }; }); }

  Monitor.prototype.destroyCharts = function () { this.charts.forEach(function (c) { try { c.destroy(); } catch (e) {} }); this.charts = []; };
  Monitor.prototype.render = function () {
    css();
    var self = this;
    this.destroyCharts();
    this.host.innerHTML = '<div class="xm-bar"><div class="xm-tabs" role="tablist">' + TABS.map(function (t) {
      return '<button type="button" role="tab" data-xm-tab="' + t.k + '"' + (t.k === self.tab ? ' class="on" aria-selected="true"' : "") + ">" + t.t + "</button>";
    }).join("") + '</div><span class="xm-sp"></span><span class="xm-upd" data-xm-upd></span>' +
      '<select class="x-select" data-xm-range aria-label="时间范围">' + RANGES.map(function (r) { return '<option value="' + r.v + '"' + (r.v === self.range ? " selected" : "") + ">" + r.t + "</option>"; }).join("") + "</select>" +
      '<select class="x-select" data-xm-refresh aria-label="自动刷新">' + REFRESH.map(function (r) { return '<option value="' + r.v + '"' + (r.v === self.refresh ? " selected" : "") + ">" + r.t + "</option>"; }).join("") + "</select>" +
      '<button class="x-btn" type="button" data-xm-reload>刷新</button></div><div data-xm-body></div>';
    [].forEach.call(this.host.querySelectorAll("[data-xm-tab]"), function (b) {
      b.onclick = function () { self.tab = b.getAttribute("data-xm-tab"); store("tab", self.tab); self.render(); };
    });
    this.host.querySelector("[data-xm-range]").onchange = function (e) { self.range = e.target.value; store("range", self.range); self.load(); };
    this.host.querySelector("[data-xm-refresh]").onchange = function (e) { self.refresh = Number(e.target.value); store("refresh", String(self.refresh)); self.schedule(); };
    this.host.querySelector("[data-xm-reload]").onclick = function () { self.load(); };
    this.load(); this.schedule();
  };
  Monitor.prototype.schedule = function () {
    var self = this; clearInterval(this.timer);
    if (this.refresh > 0) this.timer = setInterval(function () { if (!self.host.isConnected) { clearInterval(self.timer); self.destroyCharts(); return; } self.load(true); }, this.refresh * 1000);
  };
  Monitor.prototype.body = function () { return this.host.querySelector("[data-xm-body]"); };
  Monitor.prototype.stamp = function () {
    var u = this.host.querySelector("[data-xm-upd]");
    if (u) u.textContent = new Date().toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" }) + " 更新";
  };
  Monitor.prototype.load = function (quiet) {
    var seq = ++this.seq, self = this, fn = { cluster: this.loadCluster, nodes: this.loadNodes, workloads: this.loadWorkloads, apiserver: this.loadApi }[this.tab] || this.loadCluster;
    if (!quiet) { this.destroyCharts(); this.body().innerHTML = '<p class="x-frac" style="padding:40px 0;text-align:center">正在读取监控数据…</p>'; }
    return fn.call(this, function () { return seq === self.seq && self.host.isConnected; }).then(function () { if (seq === self.seq) self.stamp(); });
  };

  /* ---------- 图表 ---------- */
  Monitor.prototype.chart = function (canvasHost, series, opt) {
    opt = opt || {};
    var has = series.some(function (s) { return s.points.some(function (p) { return p[1] != null; }); });
    if (!has || !root.Chart) { canvasHost.innerHTML = '<div class="xm-empty">当前时间范围暂无数据</div>'; return; }
    canvasHost.innerHTML = '<div class="xm-chart"><canvas></canvas></div>';
    var fmt = opt.fmt || function (v) { return fmtNum(v, 2); };
    var multiDay = this.range === "7d";
    var c = new root.Chart(canvasHost.querySelector("canvas"), {
      type: "line",
      data: { datasets: series.map(function (s, i) {
        var col = s.color || COLORS[i % COLORS.length];
        return { label: s.name, data: s.points.map(function (p) { return { x: p[0] * 1000, y: p[1] }; }), borderColor: col, backgroundColor: col + "1f",
          fill: series.length === 1 || !!opt.fill, borderWidth: 2, pointRadius: 0, tension: 0.25, spanGaps: true };
      }) },
      options: {
        responsive: true, maintainAspectRatio: false, animation: false, interaction: { mode: "index", intersect: false },
        plugins: { datalabels: { display: false }, legend: { display: series.length > 1, position: "bottom", labels: { boxWidth: 10, boxHeight: 10, font: { size: 12 } } },
          tooltip: { callbacks: { title: function (it) { return it.length ? new Date(it[0].parsed.x).toLocaleString("zh-CN", { hour12: false }) : ""; }, label: function (it) { return " " + it.dataset.label + "：" + fmt(it.parsed.y); } } } },
        scales: {
          x: { type: "linear", grid: { display: false }, ticks: { maxTicksLimit: 6, font: { size: 11 }, color: "#8a919c", callback: function (v) {
            var d = new Date(v); return multiDay ? (d.getMonth() + 1) + "/" + d.getDate() + " " + String(d.getHours()).padStart(2, "0") + ":00" : String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0"); } } },
          y: { beginAtZero: true, max: opt.max, suggestedMax: opt.max ? undefined : opt.minMax || (opt.fmt === fmtRate ? 1024 : opt.int ? 1 : undefined), grid: { color: "#f0f2f5" }, ticks: { maxTicksLimit: 5, precision: opt.int ? 0 : undefined, font: { size: 11 }, color: "#8a919c", callback: function (v) { return fmt(v); } } }
        }
      }
    });
    this.charts.push(c);
  };
  function toSeries(result, nameFn) {
    return (result || []).map(function (r, i) {
      return { name: nameFn ? nameFn(r.metric || {}, i) : "值", points: (r.values || []).map(function (v) { var n = parseFloat(v[1]); return [Number(v[0]), isFinite(n) ? n : null]; }) };
    });
  }
  function card(id, title, extra) { return '<section class="xm-card"><header><h4>' + esc(title) + '</h4><span data-xm-x="' + id + '">' + (extra || "") + '</span></header><div data-xm-c="' + id + '"></div></section>'; }
  Monitor.prototype.fill = function (alive, specs) {
    var self = this;
    return Promise.all(specs.map(function (sp) { return settle(Promise.all(sp.q.map(function (q) { return self.qr(q); }))); })).then(function (all) {
      if (!alive()) return;
      self.destroyCharts();
      specs.forEach(function (sp, i) {
        var hostEl = self.host.querySelector('[data-xm-c="' + sp.id + '"]'); if (!hostEl) return;
        if (!all[i].ok) { hostEl.innerHTML = '<div class="xm-empty">查询失败：' + esc(all[i].e && all[i].e.message || "Prometheus 不可用") + "</div>"; return; }
        var series = [];
        all[i].v.forEach(function (res, j) { series = series.concat(toSeries(res, sp.names ? function (m, k) { return sp.names(m, j, k); } : function () { return sp.labels ? sp.labels[j] : sp.title; })); });
        if (sp.colors) series.forEach(function (s, k) { s.color = sp.colors[k]; });
        self.chart(hostEl, series, sp);
        if (sp.extra) { var x = self.host.querySelector('[data-xm-x="' + sp.id + '"]'); if (x) x.innerHTML = sp.extra(series); }
      });
    });
  };
  function lastOf(series, k) { var s = series[k || 0]; if (!s) return null; for (var i = s.points.length - 1; i >= 0; i--) if (s.points[i][1] != null) return s.points[i][1]; return null; }

  /* ---------- 集群资源 ---------- */
  Monitor.prototype.loadCluster = function (alive) {
    var self = this, b = this.body();
    var Q = {
      cpuUsed: 'sum(rate(node_cpu_seconds_total{mode!~"idle|iowait|steal"}[5m]))', cpuTotal: 'count(node_cpu_seconds_total{mode="idle"})',
      memUsed: 'sum(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes)', memTotal: 'sum(node_memory_MemTotal_bytes)',
      diskUsed: 'sum(' + fsSum("node_filesystem_size_bytes") + ' - ' + fsSum("node_filesystem_avail_bytes") + ')', diskTotal: 'sum(' + fsSum("node_filesystem_size_bytes") + ')',
      pods: 'sum(kube_pod_status_phase{phase="Running"} == 1)', podCap: 'sum(kube_node_status_allocatable{resource="pods"})'
    };
    var keys = Object.keys(Q);
    return Promise.all(keys.map(function (k) { return settle(self.q(Q[k])); })).then(function (r) {
      if (!alive()) return;
      var v = {}; keys.forEach(function (k, i) { v[k] = r[i].ok ? val(r[i].v) : null; });
      var pct = function (a, t) { return a != null && t ? a / t * 100 : null; };
      var stat = function (title, main, unit, p) {
        return '<div class="xm-stat"><p>' + title + '</p><strong>' + main + '<small>' + unit + '</small></strong>' + (p == null ? "" : meter(p)) + "</div>";
      };
      b.innerHTML = (r.every(function (x) { return !x.ok; }) ? '<p class="xm-err">Prometheus 查询失败，请检查数据源连接。</p>' : "") +
        '<div class="xm-stats">' +
        stat("CPU", fmtCores(v.cpuUsed), "/ " + fmtNum(v.cpuTotal) + " 核" + (pct(v.cpuUsed, v.cpuTotal) == null ? "" : " · " + pct(v.cpuUsed, v.cpuTotal).toFixed(1) + "%"), pct(v.cpuUsed, v.cpuTotal)) +
        stat("内存", fmtBytes(v.memUsed), "/ " + fmtBytes(v.memTotal) + (pct(v.memUsed, v.memTotal) == null ? "" : " · " + pct(v.memUsed, v.memTotal).toFixed(1) + "%"), pct(v.memUsed, v.memTotal)) +
        stat("本地存储", fmtBytes(v.diskUsed), "/ " + fmtBytes(v.diskTotal) + (pct(v.diskUsed, v.diskTotal) == null ? "" : " · " + pct(v.diskUsed, v.diskTotal).toFixed(1) + "%"), pct(v.diskUsed, v.diskTotal)) +
        stat("Pod", fmtNum(v.pods), "/ " + fmtNum(v.podCap), pct(v.pods, v.podCap)) +
        '</div><div class="xm-grid">' +
        card("cpu", "CPU 使用率") + card("mem", "内存使用率") + card("disk", "磁盘吞吐") + card("net", "网络带宽") +
        card("pods", "Pod 状态") + card("restart", "容器重启（每 5 分钟）") + "</div>";
      return self.fill(alive, [
        { id: "cpu", title: "CPU 使用率", q: ['100 * ' + Q.cpuUsed + ' / ' + Q.cpuTotal], max: 100, fmt: function (x) { return fmtNum(x, 1) + "%"; }, extra: function (s) { var l = lastOf(s); return l == null ? "" : l.toFixed(1) + "%"; } },
        { id: "mem", title: "内存使用率", q: ['100 * ' + Q.memUsed + ' / ' + Q.memTotal], max: 100, colors: ["#16a34a"], fmt: function (x) { return fmtNum(x, 1) + "%"; }, extra: function (s) { var l = lastOf(s); return l == null ? "" : l.toFixed(1) + "%"; } },
        { id: "disk", q: ['sum(rate(node_disk_read_bytes_total[5m]))', 'sum(rate(node_disk_written_bytes_total[5m]))'], labels: ["读取", "写入"], colors: ["#0891b2", "#d97706"], fmt: fmtRate },
        { id: "net", q: ['sum(rate(node_network_receive_bytes_total{' + NET + '}[5m]))', 'sum(rate(node_network_transmit_bytes_total{' + NET + '}[5m]))'], labels: ["接收", "发送"], colors: ["#2563eb", "#7c3aed"], fmt: fmtRate },
        { id: "pods", int: true, q: ['sum by(phase)(kube_pod_status_phase == 1)'], names: function (m) { return { Running: "运行中", Pending: "等待中", Failed: "失败", Succeeded: "已完成", Unknown: "未知" }[m.phase] || m.phase; }, fmt: function (x) { return fmtNum(x); } },
        { id: "restart", int: true, q: ['sum(increase(kube_pod_container_status_restarts_total[5m]))'], colors: ["#dc2626"], title: "重启次数", fmt: function (x) { return fmtNum(x); } }
      ]);
    });
  };

  /* ---------- 节点 ---------- */
  Monitor.prototype.loadNodes = function (alive) {
    var self = this, b = this.body();
    var Q = {
      cpu: '100 * sum by(instance)(rate(node_cpu_seconds_total{mode!~"idle|iowait|steal"}[5m])) / count by(instance)(node_cpu_seconds_total{mode="idle"})',
      cores: 'count by(instance)(node_cpu_seconds_total{mode="idle"})',
      mem: '100 * (1 - sum by(instance)(node_memory_MemAvailable_bytes) / sum by(instance)(node_memory_MemTotal_bytes))',
      memTotal: 'sum by(instance)(node_memory_MemTotal_bytes)',
      disk: '100 * (1 - ' + fsSum("node_filesystem_avail_bytes") + ' / ' + fsSum("node_filesystem_size_bytes") + ')',
      load1: 'max by(instance)(node_load1)',
      rx: 'sum by(instance)(rate(node_network_receive_bytes_total{' + NET + '}[5m]))',
      tx: 'sum by(instance)(rate(node_network_transmit_bytes_total{' + NET + '}[5m]))',
      uname: 'max by(instance,nodename)(node_uname_info)'
    };
    var keys = Object.keys(Q);
    return Promise.all(keys.map(function (k) { return settle(self.q(Q[k])); })).then(function (r) {
      if (!alive()) return;
      var rows = {}, names = {};
      keys.forEach(function (k, i) {
        (r[i].ok ? r[i].v : []).forEach(function (s) {
          var inst = s.metric.instance || "—"; rows[inst] = rows[inst] || { inst: inst };
          if (k === "uname") names[inst] = s.metric.nodename; else rows[inst][k] = parseFloat(s.value[1]);
        });
      });
      var list = Object.keys(rows).map(function (k) { rows[k].name = names[k] || k; return rows[k]; }).sort(function (a, c) { return (c.cpu || 0) - (a.cpu || 0); });
      if (!list.length) { b.innerHTML = '<div class="xm-tw"><div class="xm-empty">没有 node-exporter 指标，请确认采集目标已就绪</div></div>'; return; }
      var pctCell = function (p) { return p == null || !isFinite(p) ? "—" : '<div class="xm-pct"><b>' + p.toFixed(1) + '%</b>' + meter(p) + "</div>"; };
      b.innerHTML = '<div class="xm-tw"><table class="xm-table"><thead><tr><th>节点</th><th>CPU</th><th>内存</th><th>根分区</th><th>Load 1m</th><th>网络接收</th><th>网络发送</th></tr></thead><tbody>' +
        list.map(function (n, i) {
          return '<tr data-xm-row="' + i + '"><td><b style="font-weight:600">' + esc(n.name) + '</b><div class="x-frac" style="font-size:12px;color:#8a919c">' + esc(n.inst) + " · " + fmtNum(n.cores) + " 核 · " + fmtBytes(n.memTotal) + "</div></td><td>" +
            pctCell(n.cpu) + "</td><td>" + pctCell(n.mem) + "</td><td>" + pctCell(n.disk) + "</td><td>" + fmtNum(n.load1, 2) + "</td><td>" + fmtRate(n.rx) + "</td><td>" + fmtRate(n.tx) + "</td></tr>";
        }).join("") + "</tbody></table></div>";
      [].forEach.call(b.querySelectorAll("[data-xm-row]"), function (tr) {
        tr.onclick = function () {
          var open = tr.nextSibling && tr.nextSibling.className === "xm-detail";
          [].forEach.call(b.querySelectorAll(".xm-detail"), function (d) { d.remove(); });
          self.destroyCharts();
          if (open) return;
          var n = list[Number(tr.getAttribute("data-xm-row"))], sel = 'instance="' + n.inst.replace(/"/g, '\\"') + '"';
          var d = document.createElement("tr"); d.className = "xm-detail";
          d.innerHTML = '<td colspan="7"><div class="xm-grid">' + card("ncpu", "CPU 使用率") + card("nmem", "内存使用率") + card("nload", "系统负载") + card("nnet", "网络带宽") + "</div></td>";
          tr.parentNode.insertBefore(d, tr.nextSibling);
          self.fill(alive, [
            { id: "ncpu", title: "CPU", q: ['100 * sum(rate(node_cpu_seconds_total{' + sel + ',mode!~"idle|iowait|steal"}[5m])) / count(node_cpu_seconds_total{' + sel + ',mode="idle"})'], max: 100, fmt: function (x) { return fmtNum(x, 1) + "%"; } },
            { id: "nmem", title: "内存", q: ['100 * (1 - node_memory_MemAvailable_bytes{' + sel + '} / node_memory_MemTotal_bytes{' + sel + '})'], max: 100, colors: ["#16a34a"], fmt: function (x) { return fmtNum(x, 1) + "%"; } },
            { id: "nload", q: ['node_load1{' + sel + '}', 'node_load5{' + sel + '}', 'node_load15{' + sel + '}'], labels: ["1 分钟", "5 分钟", "15 分钟"], fmt: function (x) { return fmtNum(x, 2); } },
            { id: "nnet", q: ['sum(rate(node_network_receive_bytes_total{' + sel + ',' + NET + '}[5m]))', 'sum(rate(node_network_transmit_bytes_total{' + sel + ',' + NET + '}[5m]))'], labels: ["接收", "发送"], colors: ["#2563eb", "#7c3aed"], fmt: fmtRate }
          ]);
        };
      });
    });
  };

  /* ---------- 工作负载 ---------- */
  function ownerKey(m) {
    var kind = m.owner_kind || "Pod", name = m.owner_name || m.pod || "—";
    if (kind === "ReplicaSet") { kind = "Deployment"; name = name.replace(/-[a-z0-9]{5,10}$/, ""); }
    if (kind === "Job" && /-\d{8,}$/.test(name)) { kind = "CronJob"; name = name.replace(/-\d{8,}$/, ""); }
    if (kind === "<none>") kind = "Pod";
    return { ns: m.namespace || "—", kind: kind, name: name, id: (m.namespace || "") + "/" + kind + "/" + name };
  }
  var KIND_CN = { Deployment: "部署", StatefulSet: "有状态副本集", DaemonSet: "守护进程集", CronJob: "定时任务", Job: "任务", Pod: "容器组", Node: "静态 Pod" };
  Monitor.prototype.loadWorkloads = function (alive) {
    var self = this, b = this.body();
    var nsSel = this.ns ? 'namespace="' + this.ns.replace(/"/g, "") + '",' : "";
    var by = " by(namespace,owner_kind,owner_name)";
    var Q = {
      cpu: "sum" + by + "(rate(container_cpu_usage_seconds_total{" + nsSel + CPOD + "}[5m])" + OWNER + ")",
      mem: "sum" + by + "(container_memory_working_set_bytes{" + nsSel + CPOD + "}" + OWNER + ")",
      pods: "count" + by + "(kube_pod_owner{" + nsSel.replace(/,$/, "") + "})",
      restarts: "sum" + by + "(kube_pod_container_status_restarts_total{" + nsSel.replace(/,$/, "") + "}" + OWNER + ")",
      ns: "count by(namespace)(kube_namespace_status_phase{phase=\"Active\"} == 1)"
    };
    var keys = Object.keys(Q);
    return Promise.all(keys.map(function (k) { return settle(self.q(Q[k])); })).then(function (r) {
      if (!alive()) return;
      var map = {}, nss = [];
      keys.forEach(function (k, i) {
        (r[i].ok ? r[i].v : []).forEach(function (s) {
          if (k === "ns") { nss.push(s.metric.namespace); return; }
          var o = ownerKey(s.metric), cur = map[o.id] = map[o.id] || { o: o, cpu: 0, mem: 0, pods: 0, restarts: 0 };
          cur[k] += parseFloat(s.value[1]) || 0;
        });
      });
      nss.sort();
      var list = Object.keys(map).map(function (k) { return map[k]; }).filter(function (x) { return x.cpu || x.mem || x.pods; });
      list.sort(function (a, c) { return self.sort === "mem" ? c.mem - a.mem : self.sort === "restarts" ? c.restarts - a.restarts : c.cpu - a.cpu; });
      var top = list.slice(0, 15), maxCpu = Math.max.apply(null, top.map(function (x) { return x.cpu; }).concat([1e-9])), maxMem = Math.max.apply(null, top.map(function (x) { return x.mem; }).concat([1]));
      b.innerHTML = '<div class="xm-filter"><select class="x-select" data-xm-ns aria-label="命名空间"><option value="">全部命名空间</option>' + nss.map(function (n) { return '<option' + (n === self.ns ? " selected" : "") + ">" + esc(n) + "</option>"; }).join("") +
        '</select><select class="x-select" data-xm-sort aria-label="排序"><option value="cpu"' + (self.sort === "cpu" ? " selected" : "") + '>按 CPU 排序</option><option value="mem"' + (self.sort === "mem" ? " selected" : "") + '>按内存排序</option><option value="restarts"' + (self.sort === "restarts" ? " selected" : "") + '>按重启次数排序</option></select>' +
        '<span class="x-frac" style="color:#8a919c;font-size:13px">共 ' + list.length + " 个工作负载，显示前 " + top.length + " 个</span></div>" +
        (top.length ? '<div class="xm-tw"><table class="xm-table"><thead><tr><th>工作负载</th><th>命名空间</th><th>类型</th><th>CPU</th><th>内存</th><th>容器组</th><th>重启</th></tr></thead><tbody>' +
          top.map(function (x, i) {
            var bar = function (v, m, txt) { var p = v / m * 100; return '<div class="xm-pct"><b>' + txt + '</b><div class="xm-meter"><i style="width:' + p.toFixed(1) + '%;background:#2563eb"></i></div></div>'; };
            return '<tr data-xm-row="' + i + '"><td><b style="font-weight:600">' + esc(x.o.name) + "</b></td><td>" + esc(x.o.ns) + "</td><td>" + esc(KIND_CN[x.o.kind] || x.o.kind) + "</td><td>" + bar(x.cpu, maxCpu, fmtCores(x.cpu)) + "</td><td>" + bar(x.mem, maxMem, fmtBytes(x.mem)) + "</td><td>" + fmtNum(x.pods) + '</td><td' + (x.restarts > 0 ? ' style="color:#b42318;font-weight:600"' : "") + ">" + fmtNum(x.restarts) + "</td></tr>";
          }).join("") + "</tbody></table></div>" : '<div class="xm-tw"><div class="xm-empty">没有容器指标，请确认 cAdvisor 与 kube-state-metrics 已采集</div></div>');
      b.querySelector("[data-xm-ns]").onchange = function (e) { self.ns = e.target.value; store("ns", self.ns); self.load(); };
      b.querySelector("[data-xm-sort]").onchange = function (e) { self.sort = e.target.value; store("sort", self.sort); self.load(); };
      [].forEach.call(b.querySelectorAll("[data-xm-row]"), function (tr) {
        tr.onclick = function () {
          var open = tr.nextSibling && tr.nextSibling.className === "xm-detail";
          [].forEach.call(b.querySelectorAll(".xm-detail"), function (d) { d.remove(); });
          self.destroyCharts();
          if (open) return;
          var x = top[Number(tr.getAttribute("data-xm-row"))].o;
          var sel = 'namespace="' + x.ns + '"', of = 'owner_name=~"' + (x.kind === "Deployment" ? x.name + '-[a-z0-9]+' : x.kind === "CronJob" ? x.name + '-[0-9]+' : x.name) + '"';
          var join = ' * on(namespace,pod) group_left() max by(namespace,pod)(kube_pod_owner{' + sel + "," + of + '})';
          var d = document.createElement("tr"); d.className = "xm-detail";
          d.innerHTML = '<td colspan="7"><div class="xm-grid">' + card("wcpu", "CPU 用量（按容器组）") + card("wmem", "内存用量（按容器组）") + card("wnet", "网络带宽") + card("wrst", "容器重启（每 5 分钟）") + "</div></td>";
          tr.parentNode.insertBefore(d, tr.nextSibling);
          self.fill(alive, [
            { id: "wcpu", q: ['sum by(pod)(rate(container_cpu_usage_seconds_total{' + sel + "," + CPOD + '}[5m])' + join + ")"], names: function (m) { return m.pod; }, fmt: fmtCores },
            { id: "wmem", q: ['sum by(pod)(container_memory_working_set_bytes{' + sel + "," + CPOD + '}' + join + ")"], names: function (m) { return m.pod; }, fmt: fmtBytes },
            { id: "wnet", q: ['sum(rate(container_network_receive_bytes_total{' + sel + '}[5m])' + join + ")", 'sum(rate(container_network_transmit_bytes_total{' + sel + '}[5m])' + join + ")"], labels: ["接收", "发送"], colors: ["#2563eb", "#7c3aed"], fmt: fmtRate },
            { id: "wrst", int: true, q: ['sum(increase(kube_pod_container_status_restarts_total{' + sel + '}[5m])' + join + ")"], colors: ["#dc2626"], title: "重启次数", fmt: function (v) { return fmtNum(v); } }
          ]);
        };
      });
    });
  };

  /* ---------- API Server ---------- */
  Monitor.prototype.loadApi = function (alive) {
    var self = this, b = this.body();
    var Q = {
      qps: 'sum(rate(apiserver_request_total[5m]))',
      err: '100 * sum(rate(apiserver_request_total{code=~"5.."}[5m])) / sum(rate(apiserver_request_total[5m]))',
      p99: 'histogram_quantile(0.99, sum by(le)(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])))',
      inflight: 'sum(apiserver_current_inflight_requests)'
    };
    var keys = Object.keys(Q);
    return Promise.all(keys.map(function (k) { return settle(self.q(Q[k])); })).then(function (r) {
      if (!alive()) return;
      var v = {}; keys.forEach(function (k, i) { v[k] = r[i].ok ? val(r[i].v) : null; });
      var stat = function (t, m, u) { return '<div class="xm-stat"><p>' + t + "</p><strong>" + m + "<small>" + u + "</small></strong></div>"; };
      b.innerHTML = '<div class="xm-stats">' + stat("请求速率", fmtNum(v.qps, 2), "次/秒") + stat("5xx 错误率", fmtNum(v.err, 2), "%") +
        stat("P99 延迟", v.p99 == null ? "—" : (v.p99 * 1000).toFixed(0), "ms") + stat("在途请求", fmtNum(v.inflight), "") + '</div><div class="xm-grid">' +
        card("averb", "请求速率（按动作）") + card("alat", "请求延迟 P99（按动作）") + card("acode", "响应码") + card("aerr", "5xx 错误率") + "</div>";
      return self.fill(alive, [
        { id: "averb", q: ['sum by(verb)(rate(apiserver_request_total[5m]))'], names: function (m) { return m.verb; }, fmt: function (x) { return fmtNum(x, 2) + "/s"; } },
        { id: "alat", q: ['histogram_quantile(0.99, sum by(le,verb)(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])))'], names: function (m) { return m.verb; }, fmt: function (x) { return fmtNum(x * 1000, 0) + " ms"; } },
        { id: "acode", q: ['sum by(code)(rate(apiserver_request_total[5m]))'], names: function (m) { return m.code; }, fmt: function (x) { return fmtNum(x, 2) + "/s"; } },
        { id: "aerr", q: [Q.err], colors: ["#dc2626"], title: "5xx 错误率", fmt: function (x) { return fmtNum(x, 2) + "%"; } }
      ]);
    });
  };

  root.XSMonitoring = {
    mount: function (host, ctx) {
      if (!host) return;
      var key = ctx.cluster.id + ":" + ctx.source.ds.id;
      if (host._xsMon && host._xsMon.key === key && host.isConnected && host.querySelector("[data-xm-body]")) return host._xsMon.load(true);
      if (host._xsMon) { clearInterval(host._xsMon.timer); host._xsMon.destroyCharts(); }
      var m = new Monitor(host, ctx); m.key = key; host._xsMon = m; m.render();
    }
  };
})(window);
