/* 可观测 → 证书到期。读写真实接口（server/internal/modules/observability/cert_watch.go）：
 *   证书列表    GET    /observability/certificates?source=&status=&q=   （alert:read，返回 items/counts/settings）
 *   立即扫描    POST   /observability/certificates/scan                 （notify:manage，同步执行，最长约 2 分钟）
 *   提醒设置    GET    /observability/certificates/settings             （alert:read）
 *               PUT    /observability/certificates/settings             （notify:manage，且需全部环境权限）
 *   监测域名    GET    /observability/certificates/domains              （alert:read）
 *               POST   /observability/certificates/domains              （notify:manage，添加后立即探测一次）
 *               DELETE /observability/certificates/domains/:id          （notify:manage）
 * 界面不出现接口路径；删除域名需输入域名二次确认。
 * 挂载：window.XSCertWatch.render(el)
 */
(function (root) {
  "use strict";
  var BASE = "/observability/certificates";
  var SCAN_TIMEOUT_MS = 150000; // 后端手动扫描上限 2 分钟
  var STATUS = {
    expired: ["已过期", "bad"],
    critical: ["紧急", "bad"],
    warning: ["即将到期", "warn"],
    ok: ["正常", "ok"],
    error: ["读取失败", "grey"]
  };
  var STATUS_ORDER = ["expired", "critical", "warning", "ok", "error"];
  var SOURCE = { secret: "集群证书", domain: "域名" };

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function api(path, options) { return X().api(path, options || {}); }
  function can(p) { var x = X(); return !!(x.can && x.can(p)); }
  function toast(msg) { var x = X(); if (x.toast) x.toast(msg); else if (typeof root.toast === "function") root.toast(msg); }
  function errMsg(e) {
    if (e && Number(e.code) === 40304) return "该操作已提交审批，审批通过后生效";
    return (e && e.message) || "服务暂不可用，请稍后重试";
  }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function list(v) { return Array.isArray(v) ? v : v && Array.isArray(v.items) ? v.items : []; }
  function fmtTime(v, withSec) {
    if (!v) return "—";
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + (withSec ? ":" + pad(d.getSeconds()) : "");
  }
  function fmtDays(n) {
    if (n == null || n === "") return "—";
    n = Number(n);
    if (n < 0) return "已过期 " + (-n) + " 天";
    if (n === 0) return "不足 1 天";
    return n + " 天";
  }

  // 与后端 certDomainInput.validate 保持一致的前端就地校验；返回 {host, port, errors}
  function validDNSName(h) {
    if (h.length > 253) return false;
    var labels = h.split(".");
    for (var i = 0; i < labels.length; i++) {
      var l = labels[i];
      if (!l || l.length > 63) return false;
      if (i === 0 && l === "*") return false;
      if (!/^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?$/.test(l)) return false;
    }
    return true;
  }
  function isIP(h) {
    if (/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.test(h)) return h.split(".").every(function (p) { return +p <= 255; });
    return h.indexOf(":") >= 0 && /^[0-9a-f:.]+$/.test(h);
  }
  function normalizeDomain(input) {
    var errors = {};
    var h = String(input.host || "").trim().toLowerCase().replace(/^https?:\/\//, "");
    var cut = h.search(/[\/?#]/);
    if (cut >= 0) h = h.slice(0, cut);
    var portText = String(input.port == null ? "" : input.port).trim();
    var port = 0;
    if (portText) {
      if (!/^\d+$/.test(portText)) errors.port = "端口需为数字";
      else port = +portText;
    }
    var m = h.match(/^\[([^\]]+)\]:(\d*)$/) || (h.split(":").length === 2 ? h.match(/^([^:]+):(\d*)$/) : null);
    if (m) {
      if (!/^\d+$/.test(m[2])) errors.host = "端口格式不正确";
      else if (port && port !== +m[2]) errors.port = "域名中的端口与端口字段不一致";
      else port = +m[2];
      h = m[1];
    }
    h = h.replace(/^\[|\]$/g, "").replace(/\.$/, "");
    if (!errors.host) {
      if (!h) errors.host = "请填写域名";
      else if (/^\*\./.test(h)) errors.host = "通配符域名无法探测，请填写具体域名";
      else if (!isIP(h) && !validDNSName(h)) errors.host = "域名格式不正确";
    }
    if (!port) port = 443;
    if (!errors.port && (port < 1 || port > 65535)) errors.port = "端口需在 1 到 65535 之间";
    var remark = String(input.remark || "").trim();
    if (Array.from(remark).length > 128) errors.remark = "备注不能超过 128 个字符";
    return { host: h, port: port, env: String(input.env || "").trim(), remark: remark, errors: errors };
  }
  // 阈值文本 → 降序去重数组；与后端 normalizeThresholds 一致
  function parseThresholds(text) {
    var parts = String(text || "").split(/[,，、\s]+/).filter(function (s) { return s !== ""; });
    if (!parts.length || parts.length > 10) return { error: "提醒阈值需为 1 到 10 个" };
    var seen = {}, out = [];
    for (var i = 0; i < parts.length; i++) {
      if (!/^\d+$/.test(parts[i])) return { error: "提醒阈值需为整数天数，多个用逗号分隔" };
      var n = +parts[i];
      if (n < 1 || n > 365) return { error: "提醒阈值需在 1 到 365 天之间" };
      if (!seen[n]) { seen[n] = 1; out.push(n); }
    }
    out.sort(function (a, b) { return b - a; });
    return { value: out };
  }

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-cw-css")) return;
    var s = doc.createElement("style"); s.id = "xs-cw-css";
    s.textContent = [
      ".xcw-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:20px 24px;margin-bottom:16px}",
      ".xcw-card h3{margin:0;font-size:15px}",
      ".xcw-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px;flex-wrap:wrap}",
      ".xcw-acts{display:flex;gap:8px;align-items:center;flex-wrap:wrap}",
      ".xcw-sum{font-size:13px;color:var(--x-sub,#6b7280);margin:0 0 4px}",
      ".xcw-chips{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:14px}",
      ".xcw-chip{border:1px solid var(--x-line,#e6e9ed);background:#fff;border-radius:6px;padding:6px 12px;font-size:13px;cursor:pointer;color:var(--x-text,#1f2937)}",
      ".xcw-chip b{margin-left:6px;font-weight:600}.xcw-chip[aria-pressed=true]{border-color:var(--x-blue,#2563eb);color:var(--x-blue,#2563eb);background:#eff6ff}",
      ".xcw-filter{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:12px}.xcw-filter .x-input{width:240px;height:34px}.xcw-filter .x-select{height:34px}",
      ".xcw-table{width:100%;border-collapse:collapse;font-size:13px}",
      ".xcw-table th,.xcw-table td{padding:10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:top}",
      ".xcw-table th{font-weight:500;color:var(--x-sub,#6b7280);background:#fafbfc}",
      ".xcw-sub{display:block;color:var(--x-sub,#6b7280);font-size:12px;margin-top:2px;word-break:break-all}",
      ".xcw-tag{display:inline-block;padding:1px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid;white-space:nowrap}",
      ".xcw-tag--ok{color:#15803d;background:#f0fdf4;border-color:#bbf7d0}.xcw-tag--bad{color:#b91c1c;background:#fef2f2;border-color:#fecaca}",
      ".xcw-tag--warn{color:#b45309;background:#fffbeb;border-color:#fde68a}.xcw-tag--grey{color:#4b5563;background:#f3f4f6;border-color:#e5e7eb}",
      ".xcw-err{color:#b91c1c;font-size:12px;margin:4px 0 0}.xcw-empty{padding:24px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xcw-warnbox{background:#fffbeb;border:1px solid #fde68a;color:#92400e;border-radius:6px;padding:8px 12px;font-size:13px;margin-top:8px}",
      ".xcw-link{border:0;background:none;padding:0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:13px}.xcw-link--danger{color:#b91c1c}",
      ".xcw-form{display:grid;grid-template-columns:repeat(4,minmax(0,1fr)) auto;gap:12px;align-items:start}",
      ".xcw-form label,.xcw-set label{display:block;font-size:12px;color:var(--x-sub,#4b5563);margin-bottom:4px}",
      ".xcw-form .x-input,.xcw-form .x-select,.xcw-set .x-input{width:100%;height:34px}.xcw-form .x-btn{margin-top:20px}",
      ".xcw-set{display:grid;grid-template-columns:160px 200px minmax(0,1fr);gap:16px;align-items:start}",
      ".xcw-note{font-size:12px;color:var(--x-sub,#6b7280);margin:10px 0 0;line-height:1.6}",
      ".xcw-kv{font-size:13px;line-height:1.9}.xcw-kv span{color:var(--x-sub,#6b7280);display:inline-block;min-width:84px}",
      ".xcw-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:center;justify-content:center;z-index:1000}",
      ".xcw-modal{background:#fff;border-radius:10px;width:460px;max-width:calc(100vw - 32px);padding:22px}",
      ".xcw-modal h3{margin:0 0 12px;font-size:16px}.xcw-modal p{font-size:13px;line-height:1.6;margin:0 0 12px}",
      ".xcw-modal label{display:block;font-size:13px;color:var(--x-sub,#4b5563);margin-bottom:6px}.xcw-modal .x-input{width:100%;height:34px}",
      ".xcw-foot{display:flex;justify-content:flex-end;gap:10px;margin-top:16px}",
      ".xcw .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}",
      "@media (max-width:1100px){.xcw-form{grid-template-columns:1fr 1fr}.xcw-set{grid-template-columns:1fr}}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  function tag(status) {
    var s = STATUS[status] || [status || "未知", "grey"];
    return '<span class="xcw-tag xcw-tag--' + s[1] + '">' + esc(s[0]) + "</span>";
  }

  function render(el) {
    if (!el) return;
    css();
    var serial = (el._xsCwSerial || 0) + 1; el._xsCwSerial = serial;
    var canManage = can("notify:manage");
    var st = {
      loading: true, error: "", items: [], counts: {}, settings: null, domains: [], domainsError: "", envs: [],
      filter: { source: "", status: "", q: "" },
      scanning: false,
      form: { host: "", port: "", env: "", remark: "", errors: {}, busy: false },
      set: null, // 设置表单草稿 {enabled, interval, thresholds, errors, busy}
      del: null // 删除确认 {domain, text, error, busy}
    };
    function alive() { return el._xsCwSerial === serial && el.isConnected !== false; }

    function loadCerts() {
      var q = {};
      if (st.filter.source) q.source = st.filter.source;
      if (st.filter.status) q.status = st.filter.status;
      if (st.filter.q) q.q = st.filter.q;
      return api(BASE, { query: q }).then(function (d) {
        if (!alive()) return;
        d = d || {};
        st.items = list(d.items);
        st.counts = d.counts || {};
        if (d.settings) st.settings = d.settings;
        st.error = "";
      }, function (e) { if (alive()) { st.error = errMsg(e); st.items = []; st.counts = {}; } });
    }
    function loadDomains() {
      return api(BASE + "/domains").then(function (d) {
        if (alive()) { st.domains = list(d); st.domainsError = ""; }
      }, function (e) { if (alive()) st.domainsError = errMsg(e); });
    }
    function loadEnvs() {
      if (!canManage) return Promise.resolve();
      return api("/clusters", { query: { page_size: 200 } }).then(function (r) { return list(r); }, function () { return []; }).then(function (cls) {
        if (!alive()) return;
        var seen = {}, out = [];
        cls.map(function (c) { return c && c.env; }).concat(st.domains.map(function (d) { return d.env; })).forEach(function (n) {
          if (n && !seen[n]) { seen[n] = 1; out.push(n); }
        });
        st.envs = out;
      });
    }
    function loadAll(quiet) {
      if (!quiet) { st.loading = true; paint(); }
      return Promise.all([loadCerts(), loadDomains()]).then(loadEnvs).then(function () {
        if (!alive()) return;
        st.loading = false; paint();
      });
    }

    // ---------------------------------------------------------------- 视图
    function headerHTML() {
      return '<div class="x-sethead"><div><h1 class="x-h1">证书到期</h1></div><div class="x-phacts">' +
        '<button type="button" class="x-btn" data-cw-click="reload">刷新</button>' +
        (canManage ? '<button type="button" class="x-btn x-btn--pri" data-cw-click="scan"' + (st.scanning ? " disabled" : "") + ">" + (st.scanning ? "正在扫描…" : "立即扫描") + "</button>" : "") +
        "</div></div>";
    }

    function lastScanHTML() {
      var s = st.settings;
      if (!s) return "";
      var parts = [];
      if (s.enabled === false) parts.push("定时扫描已关闭");
      else parts.push("每 " + esc(s.interval_hours) + " 小时自动扫描一次");
      if (Array.isArray(s.thresholds) && s.thresholds.length) parts.push("剩余 " + esc(s.thresholds.join(" / ")) + " 天时提醒");
      var r = s.last_result || {};
      var scan = s.last_scan_at ? "上次扫描：" + esc(fmtTime(s.last_scan_at)) : "尚未扫描";
      if (s.last_scan_at && r && r.started_at) {
        scan += "（" + (r.trigger === "manual" ? "手动" : "定时") + "，" + esc(r.clusters || 0) + " 个集群、" + esc(r.secrets || 0) + " 张集群证书、" + esc(r.domains || 0) + " 个域名，发出提醒 " + esc(r.notices || 0) + " 条）";
      }
      parts.push(scan);
      var warn = "";
      var ce = r && r.cluster_errors;
      if (ce && typeof ce === "object" && Object.keys(ce).length) {
        warn = '<div class="xcw-warnbox" data-cw-cluster-errors>以下集群上次扫描失败，列表保留其之前的结果：' +
          Object.keys(ce).map(function (k) { return "<div>" + esc(k) + "：" + esc(ce[k]) + "</div>"; }).join("") + "</div>";
      }
      if (r && r.notice_error) warn += '<div class="xcw-warnbox">' + esc(r.notice_error) + "</div>";
      return '<p class="xcw-sum" data-cw-summary>' + parts.join(" · ") + "</p>" + warn;
    }

    function chipsHTML() {
      var total = 0;
      STATUS_ORDER.forEach(function (k) { total += Number(st.counts[k]) || 0; });
      var chips = ['<button type="button" class="xcw-chip" data-cw-click="status" data-v="" aria-pressed="' + (!st.filter.status) + '">全部<b>' + total + "</b></button>"];
      STATUS_ORDER.forEach(function (k) {
        chips.push('<button type="button" class="xcw-chip" data-cw-click="status" data-v="' + k + '" aria-pressed="' + (st.filter.status === k) + '" data-cw-count="' + k + '">' + esc(STATUS[k][0]) + "<b>" + (Number(st.counts[k]) || 0) + "</b></button>");
      });
      return '<div class="xcw-chips">' + chips.join("") + "</div>";
    }

    function filterHTML() {
      var f = st.filter;
      return '<div class="xcw-filter">' +
        '<select class="x-select" name="source" aria-label="来源" data-cw-filter="source">' +
        [["", "全部来源"], ["secret", "集群证书"], ["domain", "域名"]].map(function (o) { return '<option value="' + o[0] + '"' + (f.source === o[0] ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>" +
        '<select class="x-select" name="status" aria-label="状态" data-cw-filter="status">' +
        [["", "全部状态"]].concat(STATUS_ORDER.map(function (k) { return [k, STATUS[k][0]]; })).map(function (o) { return '<option value="' + o[0] + '"' + (f.status === o[0] ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>" +
        '<input class="x-input" name="q" placeholder="名称、命名空间、集群或域名" aria-label="关键字" data-cw-q value="' + esc(f.q) + '">' +
        '<button type="button" class="x-btn" data-cw-click="search">查询</button>' +
        (f.q || f.source || f.status ? '<button type="button" class="x-btn" data-cw-click="clear">重置</button>' : "") +
        "</div>";
    }

    function certRow(c) {
      var isDomain = c.source === "domain";
      var dns = c.dns_names ? String(c.dns_names).split(",").slice(0, 3).join("、") + (String(c.dns_names).split(",").length > 3 ? " 等" : "") : "";
      var nameCell = esc(c.name) + '<span class="xcw-sub">' + esc(SOURCE[c.source] || c.source) + (c.subject && !isDomain ? " · " + esc(c.subject) : "") + "</span>";
      var nsCell = isDomain ? (dns ? esc(dns) : "—") : esc(c.namespace || "—") + (dns ? '<span class="xcw-sub">' + esc(dns) + "</span>" : "");
      var clusterCell = isDomain ? '<span class="xcw-sub" style="margin:0">外部域名</span>' : esc(c.cluster_name || "—");
      if (c.env) clusterCell += '<span class="xcw-sub">环境：' + esc(c.env) + "</span>";
      var expire = esc(fmtTime(c.not_after)) + (c.issuer ? '<span class="xcw-sub">签发方：' + esc(c.issuer) + "</span>" : "");
      var status = tag(c.status) + (c.status === "error" && c.error ? '<div class="xcw-err">' + esc(c.error) + "</div>" : "");
      return "<tr data-cw-row><td>" + nameCell + "</td><td>" + nsCell + "</td><td>" + clusterCell + "</td><td>" + expire + "</td><td>" +
        esc(c.status === "error" ? "—" : fmtDays(c.days_left)) + "</td><td>" + status + '<span class="xcw-sub">检测于 ' + esc(fmtTime(c.scanned_at)) + "</span></td></tr>";
    }

    function certsHTML() {
      var body;
      if (st.loading && !st.items.length && !st.error) body = '<p class="xcw-empty">正在加载…</p>';
      else if (st.error) body = '<p class="xcw-err" data-cw-error>' + esc(st.error) + "</p>";
      else if (!st.items.length) {
        var filtered = st.filter.q || st.filter.source || st.filter.status;
        body = '<p class="xcw-empty">' + (filtered ? "没有符合条件的证书" : "暂无证书数据" + (canManage ? "，可点击「立即扫描」或添加需要监测的域名" : "")) + "</p>";
      } else {
        body = '<table class="xcw-table"><thead><tr><th>名称</th><th>命名空间 / 域名</th><th>集群</th><th>到期时间</th><th>剩余天数</th><th>状态</th></tr></thead><tbody>' +
          st.items.map(certRow).join("") + "</tbody></table>";
      }
      return '<section class="xcw-card"><div class="xcw-head"><h3>证书列表</h3></div>' + lastScanHTML() + chipsHTML() + filterHTML() + body + "</section>";
    }

    function fieldErr(k, errs) { return errs && errs[k] ? '<p class="xcw-err" data-cw-field-error="' + k + '">' + esc(errs[k]) + "</p>" : ""; }

    function domainsHTML() {
      var f = st.form, e = f.errors || {};
      var form = "";
      if (canManage) {
        var envOpts = ['<option value="">不限环境</option>'].concat(st.envs.map(function (n) { return '<option value="' + esc(n) + '"' + (f.env === n ? " selected" : "") + ">" + esc(n) + "</option>"; }));
        form = '<div class="xcw-form" data-cw-domain-form>' +
          '<div><label for="xcw-host">域名</label><input id="xcw-host" class="x-input" name="host" data-cw-in="host" placeholder="例如 www.example.com" autocomplete="off" value="' + esc(f.host) + '">' + fieldErr("host", e) + "</div>" +
          '<div><label for="xcw-port">端口</label><input id="xcw-port" class="x-input" name="port" data-cw-in="port" placeholder="默认 443" inputmode="numeric" value="' + esc(f.port) + '">' + fieldErr("port", e) + "</div>" +
          '<div><label for="xcw-env">环境</label><select id="xcw-env" class="x-select" name="env" data-cw-in="env">' + envOpts.join("") + "</select>" + fieldErr("env", e) + "</div>" +
          '<div><label for="xcw-remark">备注</label><input id="xcw-remark" class="x-input" name="remark" data-cw-in="remark" maxlength="128" placeholder="可选" value="' + esc(f.remark) + '">' + fieldErr("remark", e) + "</div>" +
          '<div><button type="button" class="x-btn x-btn--pri" data-cw-click="addDomain"' + (f.busy ? " disabled" : "") + ">" + (f.busy ? "正在添加…" : "添加域名") + "</button></div>" +
          "</div>" + (e._ ? '<p class="xcw-err" data-cw-form-error>' + esc(e._) + "</p>" : "") +
          '<p class="xcw-note">添加后会立即检测一次证书；只读取证书到期时间，不发送业务请求。「不限环境」的域名需要全部环境权限。</p>';
      }
      var table;
      if (st.domainsError) table = '<p class="xcw-err">' + esc(st.domainsError) + "</p>";
      else if (!st.domains.length) table = '<p class="xcw-empty">暂无监测域名</p>';
      else {
        table = '<table class="xcw-table" style="margin-top:12px"><thead><tr><th>域名</th><th>端口</th><th>环境</th><th>备注</th><th>添加人</th><th>添加时间</th>' + (canManage ? '<th style="width:70px">操作</th>' : "") + "</tr></thead><tbody>" +
          st.domains.map(function (d) {
            return "<tr data-cw-domain><td>" + esc(d.host) + "</td><td>" + esc(d.port) + "</td><td>" + esc(d.env || "不限") + "</td><td>" + esc(d.remark || "—") + "</td><td>" + esc(d.created_by || "—") + "</td><td>" + esc(fmtTime(d.created_at)) + "</td>" +
              (canManage ? '<td><button type="button" class="xcw-link xcw-link--danger" data-cw-click="delDomain" data-id="' + esc(d.id) + '">删除</button></td>' : "") + "</tr>";
          }).join("") + "</tbody></table>";
      }
      return '<section class="xcw-card"><div class="xcw-head"><h3>监测域名 <span class="xcw-sub" style="display:inline;margin-left:6px">' + st.domains.length + " 个</span></h3></div>" + form + table + "</section>";
    }

    function settingsHTML() {
      var s = st.settings;
      if (!s) return "";
      if (!canManage || !st.set) {
        return '<section class="xcw-card"><div class="xcw-head"><h3>提醒设置</h3>' + (canManage ? '<button type="button" class="x-btn" data-cw-click="editSettings">修改</button>' : "") + "</div>" +
          '<div class="xcw-kv"><div><span>定时扫描</span>' + (s.enabled === false ? "已关闭" : "已开启") + "</div>" +
          "<div><span>扫描周期</span>每 " + esc(s.interval_hours) + " 小时</div>" +
          "<div><span>提醒阈值</span>剩余 " + esc((s.thresholds || []).join("、")) + " 天</div>" +
          (s.updated_by ? "<div><span>最近修改</span>" + esc(s.updated_by) + " · " + esc(fmtTime(s.updated_at)) + "</div>" : "") + "</div>" +
          '<p class="xcw-note">每张证书在每个阈值只提醒一次；证书续期后重新计数。剩余 7 天以内按紧急级别发送，过期后再提醒一次。提醒通过通知中心的「证书」事件发送，可在通知渠道的订阅规则中配置接收人。</p></section>';
      }
      var f = st.set, e = f.errors || {};
      return '<section class="xcw-card" data-cw-settings-form><div class="xcw-head"><h3>提醒设置</h3></div>' +
        '<div class="xcw-set">' +
        '<div><label>定时扫描</label><label style="color:inherit;font-size:13px"><input type="checkbox" name="enabled" data-cw-enabled' + (f.enabled ? " checked" : "") + "> 开启</label></div>" +
        '<div><label for="xcw-interval">扫描周期（小时）</label><input id="xcw-interval" class="x-input" name="interval_hours" data-cw-in="interval" inputmode="numeric" value="' + esc(f.interval) + '">' + fieldErr("interval", e) + "</div>" +
        '<div><label for="xcw-th">提醒阈值（剩余天数，多个用逗号分隔）</label><input id="xcw-th" class="x-input" name="thresholds" data-cw-in="thresholds" placeholder="30,14,7,1" value="' + esc(f.thresholds) + '">' + fieldErr("thresholds", e) + "</div>" +
        "</div>" + (e._ ? '<p class="xcw-err" data-cw-settings-error>' + esc(e._) + "</p>" : "") +
        '<p class="xcw-note">扫描周期 1 到 168 小时；阈值 1 到 365 天，最多 10 个。修改设置需要全部环境权限。</p>' +
        '<div class="xcw-foot"><button type="button" class="x-btn" data-cw-click="cancelSettings"' + (f.busy ? " disabled" : "") + ">取消</button>" +
        '<button type="button" class="x-btn x-btn--pri" data-cw-click="saveSettings"' + (f.busy ? " disabled" : "") + ">" + (f.busy ? "正在保存…" : "保存") + "</button></div></section>";
    }

    function delHTML() {
      var m = st.del;
      if (!m) return "";
      var d = m.domain;
      return '<div class="xcw-mask" data-cw-mask><div class="xcw-modal" role="dialog" aria-modal="true"><h3>删除监测域名</h3>' +
        "<p>删除后将不再检测 <strong>" + esc(d.host) + ":" + esc(d.port) + "</strong> 的证书，列表中该域名的证书记录也会一并移除。</p>" +
        '<label for="xcw-del-confirm">请输入域名「' + esc(d.host) + "」以确认</label>" +
        '<input id="xcw-del-confirm" class="x-input" name="confirm" autocomplete="off" data-cw-del-confirm value="' + esc(m.text || "") + '">' +
        (m.error ? '<p class="xcw-err" data-cw-del-error>' + esc(m.error) + "</p>" : "") +
        '<div class="xcw-foot"><button type="button" class="x-btn" data-cw-click="cancelDel"' + (m.busy ? " disabled" : "") + ">取消</button>" +
        '<button type="button" class="x-btn x-btn--danger" data-cw-click="confirmDel"' + (m.busy ? " disabled" : "") + ">" + (m.busy ? "正在删除…" : "确认删除") + "</button></div></div></div>";
    }

    function paint() {
      if (!alive()) return;
      el.innerHTML = '<div class="xcw" data-cw>' + headerHTML() + certsHTML() + domainsHTML() + settingsHTML() + delHTML() + "</div>";
    }

    function val(sel) { var n = el.querySelector(sel); return n ? String(n.value == null ? "" : n.value) : ""; }
    function readForm() {
      var f = st.form;
      f.host = val('[data-cw-in="host"]');
      f.port = val('[data-cw-in="port"]');
      f.env = val('[data-cw-in="env"]');
      f.remark = val('[data-cw-in="remark"]');
    }
    function readSettings() {
      var f = st.set;
      var cb = el.querySelector("[data-cw-enabled]");
      if (cb) f.enabled = !!cb.checked;
      f.interval = val('[data-cw-in="interval"]');
      f.thresholds = val('[data-cw-in="thresholds"]');
    }
    function findDomain(id) { return st.domains.filter(function (d) { return String(d.id) === String(id); })[0]; }

    var actions = {
      reload: function () { return loadAll(); },
      status: function (t) { st.filter.status = t.getAttribute("data-v") || ""; return loadCerts().then(paint); },
      search: function () { st.filter.q = val("[data-cw-q]").trim(); return loadCerts().then(paint); },
      clear: function () { st.filter = { source: "", status: "", q: "" }; return loadCerts().then(paint); },
      scan: function () {
        if (!canManage || st.scanning) return;
        if (st.form && el.querySelector("[data-cw-domain-form]")) readForm();
        st.scanning = true; paint();
        return api(BASE + "/scan", { method: "POST", timeoutMs: SCAN_TIMEOUT_MS }).then(function (r) {
          if (!alive()) return;
          r = r || {};
          var msg = "扫描完成：" + (r.clusters || 0) + " 个集群、" + (r.secrets || 0) + " 张集群证书、" + (r.domains || 0) + " 个域名";
          if (r.domain_errors) msg += "，" + r.domain_errors + " 个域名检测失败";
          if (r.cluster_errors && Object.keys(r.cluster_errors).length) msg += "，" + Object.keys(r.cluster_errors).length + " 个集群读取失败";
          msg += "，发出提醒 " + (r.notices || 0) + " 条";
          toast(msg);
          st.scanning = false;
          return loadAll(true);
        }, function (e) {
          if (!alive()) return;
          st.scanning = false; paint(); toast("扫描失败：" + errMsg(e));
        });
      },
      addDomain: function () {
        if (!canManage || st.form.busy) return;
        readForm();
        var f = st.form;
        var n = normalizeDomain(f);
        f.errors = n.errors;
        if (Object.keys(n.errors).length) { paint(); return; }
        f.busy = true; paint();
        return api(BASE + "/domains", { method: "POST", body: { host: n.host, port: n.port, env: n.env, remark: n.remark } }).then(function (r) {
          if (!alive()) return;
          var p = (r && r.probe) || {};
          var name = n.host + (n.port === 443 ? "" : ":" + n.port);
          if (p.error) toast("已添加 " + name + "，首次检测失败：" + p.error);
          else if (p.not_after) toast("已添加 " + name + "，证书到期时间 " + fmtTime(p.not_after));
          else toast("已添加 " + name);
          st.form = { host: "", port: "", env: f.env, remark: "", errors: {}, busy: false };
          return loadAll(true);
        }, function (e) {
          if (!alive()) return;
          f.busy = false;
          var m = errMsg(e);
          if (/端口/.test(m)) f.errors = { port: m };
          else if (/域名/.test(m) && Number(e && e.status) === 400) f.errors = { host: m };
          else f.errors = { _: "添加失败：" + m };
          paint();
        });
      },
      delDomain: function (t) {
        if (!canManage) return;
        var d = findDomain(t.getAttribute("data-id"));
        if (!d) return;
        if (el.querySelector("[data-cw-domain-form]")) readForm();
        st.del = { domain: d, text: "", error: "", busy: false }; paint();
      },
      cancelDel: function () { if (st.del && !st.del.busy) { st.del = null; paint(); } },
      confirmDel: function () {
        var m = st.del;
        if (!m || m.busy) return;
        m.text = val("[data-cw-del-confirm]").trim();
        if (m.text.toLowerCase() !== String(m.domain.host).toLowerCase()) { m.error = "输入的域名不一致，请输入「" + m.domain.host + "」"; paint(); return; }
        m.busy = true; m.error = ""; paint();
        return api(BASE + "/domains/" + encodeURIComponent(m.domain.id), { method: "DELETE" }).then(function () {
          if (!alive()) return;
          st.del = null; toast("已删除监测域名 " + m.domain.host);
          return loadAll(true);
        }, function (e) {
          if (!alive()) return;
          m.busy = false; m.error = "删除失败：" + errMsg(e); paint();
        });
      },
      editSettings: function () {
        if (!canManage || !st.settings) return;
        var s = st.settings;
        if (el.querySelector("[data-cw-domain-form]")) readForm();
        st.set = { enabled: s.enabled !== false, interval: String(s.interval_hours || 6), thresholds: (s.thresholds || []).join(","), errors: {}, busy: false };
        paint();
      },
      cancelSettings: function () { if (st.set && !st.set.busy) { st.set = null; paint(); } },
      saveSettings: function () {
        var f = st.set;
        if (!f || f.busy) return;
        readSettings();
        if (el.querySelector("[data-cw-domain-form]")) readForm();
        var errors = {};
        var iv = String(f.interval).trim();
        if (!/^\d+$/.test(iv) || +iv < 1 || +iv > 168) errors.interval = "扫描周期需为 1 到 168 之间的整数小时";
        var th = parseThresholds(f.thresholds);
        if (th.error) errors.thresholds = th.error;
        f.errors = errors;
        if (Object.keys(errors).length) { paint(); return; }
        f.busy = true; paint();
        return api(BASE + "/settings", { method: "PUT", body: { enabled: !!f.enabled, interval_hours: +iv, thresholds: th.value } }).then(function (s) {
          if (!alive()) return;
          if (s) st.settings = s;
          st.set = null; toast("提醒设置已保存");
          return loadCerts().then(paint);
        }, function (e) {
          if (!alive()) return;
          f.busy = false; f.errors = { _: "保存失败：" + errMsg(e) }; paint();
        });
      }
    };

    ["click", "change", "keydown"].forEach(function (type) {
      var key = "_xsCw_" + type;
      if (el[key]) el.removeEventListener(type, el[key]);
    });
    el._xsCw_click = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-cw-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-cw-click")];
      if (fn) return fn(t);
    };
    el._xsCw_change = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-cw-filter]") : null;
      if (!t) return;
      st.filter[t.getAttribute("data-cw-filter")] = String(t.value || "");
      st.filter.q = val("[data-cw-q]").trim();
      return loadCerts().then(paint);
    };
    el._xsCw_keydown = function (ev) {
      if (!alive() || ev.key !== "Enter") return;
      var t = ev.target;
      if (!t || !t.getAttribute) return;
      if (t.hasAttribute("data-cw-q")) { if (ev.preventDefault) ev.preventDefault(); return actions.search(); }
      if (t.closest && t.closest("[data-cw-domain-form]")) { if (ev.preventDefault) ev.preventDefault(); return actions.addDomain(); }
      if (t.hasAttribute("data-cw-del-confirm")) { if (ev.preventDefault) ev.preventDefault(); return actions.confirmDel(); }
    };
    el.addEventListener("click", el._xsCw_click);
    el.addEventListener("change", el._xsCw_change);
    el.addEventListener("keydown", el._xsCw_keydown);
    loadAll();
  }

  root.XSCertWatch = { render: render, normalizeDomain: normalizeDomain, parseThresholds: parseThresholds, fmtDays: fmtDays };
})(window);
