/* 星枢 Matrix — 运维页实时数据层
 * 覆盖：可观测（数据源 / PromQL / 告警规则 / 告警认领关闭 / 静默 / 通知渠道）、日志检索、
 *       CI-CD（应用 / 流水线 / 运行与 SSE 日志 / 部署回滚 / Jenkins）、中间件（实例 / MQ 概览 /
 *       SQL / Redis 控制台风险提示）、值班（排班 / 故障 / SLO）、设置、AI（模型 / 提示词）、
 *       自动化编排（window.XSLiveOpsAutomation，供 Lead 加路由/菜单）。
 * 引入方式：index.html 主内联脚本之后追加 <script src="xs-live-ops.js"></script>。
 * 只有 window.XS 探测到后端（XS.mode === "live"）时才接管 routes.<key>.render；
 * 后端不可用时完全保留原演示渲染器，仅显示“演示模式”徽标。不修改 index.html。
 * 依赖主脚本的全局绑定：routes / render / navGo / phead / tbl / opt / toast / $ / esc /
 * dlg / closeAll / k8Confirm / aiBtn（均为 classic script 的全局词法绑定或 window 函数）。
 */
(function () {
  "use strict";

  var VERSION = "1.0.0";
  var SHARED = (typeof window !== "undefined" && window.XS_LIVE && typeof window.XS_LIVE === "object") ? window.XS_LIVE : null;
  var MODE = "unknown";
  var ST = { installed: false, seq: 0, originals: {}, badge: null, formSeq: 0, runStream: null, refresh: null };
  var ROUTE_KEYS = ["observe", "logs", "cicd", "delivery", "release", "middleware", "oncall", "settings", "ai", "appstatus", "audit"];
  var APPR_CODE = 40304;

  /* ---------------------------------------------------------------- bas 基础 */

  function q(id) { return document.getElementById(id); }
  function E(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function toast(msg) {
    try { if (typeof window.toast === "function") { window.toast(msg); return; } } catch (e) {}
    try { console.info("[xs-live-ops]", msg); } catch (e2) {}
  }
  // 模式判断：优先复用 xs-live-core.js 暴露的 window.XS_LIVE.isLive()，保证两个脚本口径一致。
  function live() {
    if (SHARED && typeof SHARED.isLive === "function") {
      try { return !!SHARED.isLive(); } catch (e) {}
    }
    var x = window.XS;
    if (!x) return false;
    if (typeof x.mode === "function") { try { return x.mode() === "live"; } catch (e) { return false; } }
    return x.mode === "live";
  }
  function api(path, opts) {
    var x = window.XS;
    if (!x || typeof x.api !== "function") return Promise.reject({ status: 0, message: "后端未连接" });
    return x.api(path, opts || {});
  }
  function GET(path, query) { return api(path, { query: query }); }
  function POST(path, body, opts) { return api(path, Object.assign({ method: "POST", body: body }, opts || {})); }
  function PUT(path, body) { return api(path, { method: "PUT", body: body }); }
  function DEL(path, query) { return api(path, { method: "DELETE", query: query }); }

  function errMsg(err) {
    if (!err) return "请求失败";
    if (err.name === "AbortError") return "请求超时";
    if (err.status === 401 || err.code === 40100 || err.code === 40101) return "会话已失效，请重新登录";
    if (err.status === 403) return "权限不足：" + (err.message || "无访问权限");
    return err.message || ("请求失败（HTTP " + (err.status || 0) + "）");
  }
  function detailOf(err) {
    var d = err && err.details;
    if (d && typeof d === "object" && d !== null && "details" in d) return d.details;
    return d;
  }
  function isApproval(err) { return !!(err && (err.code === APPR_CODE || (err.status === 202 && err.approval))); }
  function approvalToast(err) {
    var t = err && err.approval && err.approval.ticket;
    toast("已提交审批" + (t && t.id ? "（审批单 #" + t.id + "）" : ""));
  }
  function write(p, okMsg) {
    return p.then(function (d) { toast(okMsg || "操作成功"); return d === undefined ? {} : d; }).catch(function (err) {
      if (isApproval(err)) { approvalToast(err); return { __approval: true }; }
      toast(errMsg(err));
      return null;
    });
  }
  function parseJSON(text, def) {
    try { return JSON.parse(text); } catch (e) { throw { message: "JSON 格式不正确：" + (e && e.message ? e.message : "") }; }
  }
  function asArray(data, keys) {
    if (Array.isArray(data)) return data;
    if (data && typeof data === "object") {
      for (var i = 0; i < (keys || []).length; i++) {
        var v = data[keys[i]];
        if (Array.isArray(v)) return v;
      }
    }
    return [];
  }
  // 数据源列表：后端同时提供 /observability/datasources 与 /observability/data-sources 别名。
  function dsList(query) {
    return GET("/observability/datasources", query).catch(function (err) {
      if (err && (err.status === 404 || err.status === 405 || err.status === 501)) return GET("/observability/data-sources", query);
      throw err;
    });
  }
  // 单个区块失败不影响整页：统一包装为 {ok,value,error}，逐块渲染错误/空状态。
  function settle(p, fallback) {
    return p.then(function (v) { return { ok: true, value: v }; }, function (e) { return { ok: false, error: e, value: fallback }; });
  }
  function pick(obj, names, def) {
    if (!obj || typeof obj !== "object") return def;
    for (var i = 0; i < names.length; i++) {
      var v = obj[names[i]];
      if (v !== undefined && v !== null && v !== "") return v;
    }
    return def;
  }
  function assign(target) {
    for (var i = 1; i < arguments.length; i++) {
      var s = arguments[i] || {};
      for (var k in s) if (Object.prototype.hasOwnProperty.call(s, k)) target[k] = s[k];
    }
    return target;
  }

  /* ---------------------------------------------------------------- fmt 格式化 */

  function pad2(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(v) {
    if (!v) return "—";
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return E(v);
    return pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + ":" + pad2(d.getSeconds());
  }
  function fmtDay(v) {
    if (!v) return "—";
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return E(v);
    return d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate());
  }
  function fmtMin(v) {
    if (!v) return "—";
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return E(v);
    return pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }
  function ago(v) {
    if (!v) return "—";
    var t = v instanceof Date ? v.getTime() : new Date(v).getTime();
    if (!isFinite(t)) return "—";
    var s = Math.max(0, Math.round((Date.now() - t) / 1000));
    if (s < 60) return s + " 秒前";
    if (s < 3600) return Math.round(s / 60) + " 分钟前";
    if (s < 86400) return Math.round(s / 3600) + " 小时前";
    return Math.round(s / 86400) + " 天前";
  }
  function dur(sec) {
    if (sec == null || sec === "") return "—";
    var s = Number(sec);
    if (!isFinite(s)) return E(sec);
    if (s < 60) return s + " 秒";
    if (s < 3600) return Math.round(s / 60) + " 分钟";
    if (s < 86400) return (s / 3600).toFixed(1) + " 小时";
    return (s / 86400).toFixed(1) + " 天";
  }
  function num(v) {
    if (v == null || v === "") return "—";
    var n = Number(v);
    return isFinite(n) ? n.toLocaleString("zh-CN") : E(v);
  }
  function pct(v) {
    var n = Number(v);
    if (!isFinite(n)) return "—";
    return (Math.round(n * 10000) / 100) + "%";
  }
  function tag(text, cls) { return '<span class="x-tag ' + (cls || "x-grey") + ' x-tag--s">' + E(text) + "</span>"; }
  function dotTag(text, cls) { return '<span class="x-tag x-dot x-pill ' + (cls || "x-grey") + '">' + E(text) + "</span>"; }
  var ST_TAG = {
    succeeded: "x-green", success: "x-green", resolved: "x-green", closed: "x-grey", ok: "x-green",
    failed: "x-red", error: "x-red", firing: "x-red", rejected: "x-red", danger: "x-red", critical: "x-red",
    running: "x-blue", queued: "x-grey", pending: "x-grey", cancelling: "x-orange", cancelled: "x-grey",
    skipped: "x-grey", open: "x-orange", acked: "x-blue", awaiting_approval: "x-orange", approved: "x-blue",
    active: "x-green", disabled: "x-grey", paused: "x-grey", write: "x-orange"
  };
  function stTag(s) { return s == null || s === "" ? "—" : tag(s, ST_TAG[String(s)] || "x-grey"); }
  var SEV_TAG = { p0: "x-red", p1: "x-orange", p2: "x-blue", p3: "x-grey", critical: "x-red", warning: "x-orange", info: "x-blue", none: "x-grey" };
  function sevTag(s) { return s == null || s === "" ? "—" : tag(s, SEV_TAG[String(s).toLowerCase()] || "x-grey"); }
  function mono(v) { return '<span class="x-mono">' + E(v == null ? "—" : v) + "</span>"; }
  function code(v, max) {
    var s = String(v == null ? "" : v);
    var cut = max && s.length > max ? s.slice(0, max) + "…" : s;
    return '<span class="x-mono" title="' + E(s) + '">' + E(cut) + "</span>";
  }

  /* ---------------------------------------------------------------- ui 通用组件 */

  function injectCSS() {
    if (q("ops-live-css")) return;
    var s = document.createElement("style");
    s.id = "ops-live-css";
    s.textContent = ".ops-json{max-height:55vh;overflow:auto;margin:0;white-space:pre-wrap;word-break:break-word}.ops-sub{color:var(--x-sub);font-size:12px;margin:8px 0 0}.ops-nowrap{white-space:nowrap}.ops-ro{max-height:60vh;overflow:auto;margin:0}";
    (document.head || document.documentElement).appendChild(s);
  }
  // 徽标：xs-live-core.js 已提供共享的 #xs-live-badge（幂等），这里优先复用；
  // 仅当 core 未加载时才创建一个自带兜底徽标，避免重复 DOM/样式互相覆盖。
  function ensureFallbackBadge() {
    if (ST.badge && document.body.contains(ST.badge)) return ST.badge;
    var b = document.createElement("div");
    b.id = "xs-live-ops-badge";
    b.setAttribute("role", "status");
    b.style.cssText = "position:fixed;left:14px;bottom:14px;z-index:70;padding:4px 10px;border-radius:999px;font-size:12px;line-height:1.5;background:#fffaeb;color:#b54708;border:1px solid #fedf89;box-shadow:0 2px 8px rgba(16,24,40,.08);pointer-events:none;font-weight:400;display:none";
    document.body.appendChild(b);
    ST.badge = b;
    return b;
  }
  function opsHash() {
    var h = (location.hash || "").replace(/^#\/?/, "").split("/")[0];
    return h;
  }
  function updateBadge() {
    if (SHARED && typeof SHARED.updateBadge === "function") {
      try { SHARED.updateBadge(MODE === "live" ? "live" : MODE === "unknown" ? "boot" : "demo"); } catch (e) {}
      return;
    }
    var b = ensureFallbackBadge();
    var onOps = ROUTE_KEYS.indexOf(opsHash()) >= 0 || opsHash() === "automation";
    if (MODE === "demo" && onOps) {
      b.style.display = "inline-flex";
      b.innerHTML = "<b>演示模式</b> · 未连接后端，以下为演示数据";
    } else {
      b.style.display = "none";
      b.textContent = "";
    }
  }

  function preBlock(obj) {
    var text = typeof obj === "string" ? obj : JSON.stringify(obj, null, 2);
    return '<pre class="x-log ops-json">' + E(text === undefined ? "null" : text) + "</pre>";
  }
  function infoModal(title, html) {
    dlg(E(title), '<div class="x-form">' + html + "</div>", '<button class="x-btn" data-close>关闭</button>');
  }
  function confirmDo(title, msg, ok, fn) {
    if (typeof window.k8Confirm === "function") { window.k8Confirm(E(title), msg, E(ok || "确定"), fn); return; }
    if (window.confirm(String(title))) fn();
  }
  function panelHTML(title, inner, tools) {
    return '<section class="x-panel"><div class="x-tbar"><h2 class="x-h2">' + E(title) + "</h2>" + (tools || "") + "</div>" + inner + "</section>";
  }
  function loadingHTML(text) { return '<div class="x-panel"><p class="x-empty">' + E(text || "正在加载…") + "</p></div>"; }
  function emptyHTML(text) { return '<p class="x-empty">' + E(text || "暂无数据") + "</p>"; }
  function errorHTML(err) {
    return '<div class="x-panel"><p class="x-aerr" role="alert">' + E(errMsg(err)) + '</p><p style="margin:12px 0 0"><button class="x-btn" type="button" data-opretry>重试</button></p></div>';
  }
  function banner(msg, cls) {
    return '<div class="x-warn" role="note" style="margin-bottom:12px">' + E(msg) + "</div>";
  }
  function liveTag() { return ' <span class="x-tag x-dot x-green x-tag--s">实时</span>'; }

  function livePage(el, title, desc, tools, tabs) {
    var p = { el: el, box: null, token: 0, seq: ++ST.seq, retry: null };
    el.innerHTML = phead(E(title), desc + liveTag(), tools || "") + (tabs || "");
    var box = document.createElement("div");
    box.setAttribute("data-ops-body", "");
    box.innerHTML = loadingHTML();
    el.appendChild(box);
    p.box = box;
    bindTabs(el);
    p.newToken = function () { return ++p.token; };
    p.paint = function (token, html) {
      if (token != null && token !== p.token) return false;
      if (!document.body.contains(el)) return false;
      p.box.innerHTML = html;
      return true;
    };
    p.error = function (token, err) {
      if (!p.paint(token, errorHTML(err))) return;
      var b = p.box.querySelector("[data-opretry]");
      if (b) b.onclick = function () { if (p.retry) p.retry(); };
    };
    p.addTools = function (html) { el.querySelector(".x-phacts").insertAdjacentHTML("afterbegin", html); };
    return p;
  }

  function tabsHTML(items, cur) {
    return '<div class="x-bar"><div class="x-tabs" role="tablist">' + items.map(function (it) {
      var on = it.h === cur;
      return '<button type="button" role="tab" aria-selected="' + on + '" class="' + (on ? "on" : "") + '" data-ops-tab="' + E(it.h) + '">' + E(it.t) + "</button>";
    }).join("") + "</div></div>";
  }
  function bindTabs(root) {
    [].slice.call(root.querySelectorAll("[data-ops-tab]")).forEach(function (b) {
      b.onclick = function () { try { window.navGo(b.getAttribute("data-ops-tab")); } catch (e) { location.hash = b.getAttribute("data-ops-tab"); } };
    });
  }
  function selHTML(name, label, options, cur, attrs) {
    return "<label><span>" + E(label || "") + '</span><select class="x-select" name="' + E(name) + '"' + (attrs || "") + ">" +
      options.map(function (o) {
        var v = typeof o === "object" ? o.v : o, t = typeof o === "object" ? o.t : o;
        return '<option value="' + E(v) + '"' + (String(v) === String(cur) ? " selected" : "") + ">" + E(t) + "</option>";
      }).join("") + "</select></label>";
  }
  function inputHTML(name, label, value, placeholder, attrs) {
    return "<label><span>" + E(label || "") + '</span><input class="x-input" name="' + E(name) + '" value="' + E(value == null ? "" : value) + '" placeholder="' + E(placeholder || "") + '" autocomplete="off"' + (attrs || "") + " /></label>";
  }
  function barHTML(parts) { return '<div class="x-tbar">' + parts.join("") + "</div>"; }

  function fieldHTML(f) {
    var attrs = f.required ? ' required aria-required="true"' : "";
    var span = "<span>" + (f.required ? '<b class="x-req">*</b> ' : "") + E(f.label || f.k) + "</span>";
    var ctl;
    if (f.kind === "select") {
      ctl = '<select class="x-select" name="' + E(f.k) + '"' + attrs + ">" + (f.options || []).map(function (o) {
        var v = typeof o === "object" ? o.v : o, t = typeof o === "object" ? o.t : o;
        return '<option value="' + E(v) + '"' + (String(v) === String(f.value) ? " selected" : "") + ">" + E(t) + "</option>";
      }).join("") + "</select>";
    } else if (f.kind === "textarea") {
      ctl = '<textarea class="x-input" name="' + E(f.k) + '" rows="' + (f.rows || 4) + '" placeholder="' + E(f.placeholder || "") + '"' + attrs + ">" + E(f.value || "") + "</textarea>";
    } else if (f.kind === "checkbox") {
      ctl = '<label class="x-check" style="display:flex;align-items:center;gap:8px;font-weight:400"><input type="checkbox" name="' + E(f.k) + '"' + (f.value ? " checked" : "") + ' style="width:auto" /> ' + E(f.hint || "启用") + "</label>";
      return '<label><span>' + E(f.label || f.k) + "</span>" + ctl + "</label>";
    } else {
      ctl = '<input class="x-input" type="' + (f.kind || "text") + '" name="' + E(f.k) + '" value="' + E(f.value == null ? "" : f.value) + '" placeholder="' + E(f.placeholder || "") + '" autocomplete="off"' + (f.kind === "number" ? ' min="0"' : "") + attrs + " />";
    }
    return "<label" + (f.kind === "textarea" ? ' class="x-full"' : "") + ">" + span + ctl + "</label>";
  }
  function formModal(title, fields, okLabel, onSubmit, hint) {
    var id = "opsf" + (++ST.formSeq);
    var html = '<form class="x-form" id="' + id + '" novalidate>' + (hint ? '<p class="x-desc" style="margin:0">' + hint + "</p>" : "") +
      fields.map(fieldHTML).join("") + '<span class="x-err" id="' + id + 'err" hidden></span></form>';
    dlg(E(title), html, '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="' + id + '">' + E(okLabel || "保存") + "</button>");
    var f = q(id);
    if (!f) return null;
    f.onsubmit = function (ev) {
      ev.preventDefault();
      var err = q(id + "err");
      err.hidden = true;
      var vals = {}, bad = "";
      fields.forEach(function (fd) {
        var node = f.elements[fd.k];
        if (!node) return;
        var v = fd.kind === "checkbox" ? !!node.checked : String(node.value || "").trim();
        if (fd.required && (v === "" || v === false)) bad = bad || ("请填写：" + (fd.label || fd.k));
        vals[fd.k] = v;
      });
      if (bad) { err.textContent = bad; err.hidden = false; return; }
      onSubmit(vals, f, err);
    };
    if (f.elements && f.elements.length) { try { f.elements[0].focus(); } catch (e) {} }
    return f;
  }

  /* ---------------------------------------------------------------- obs 可观测 */

  function obPage() {
    var seg = (location.hash || "").split("/")[2] || "metrics";
    return ["metrics", "rules", "events", "channels"].indexOf(seg) >= 0 ? seg : "metrics";
  }
  var OBS_TABS = [
    { t: "指标监控", h: "#/observe" },
    { t: "告警规则", h: "#/observe/rules" },
    { t: "告警事件", h: "#/observe/events" },
    { t: "通知渠道", h: "#/observe/channels" }
  ];
  var OB = { promId: 0, range: "1h", query: "up" };

  function renderObserveLive(el) {
    var pg = obPage();
    if (pg === "rules") return obsRules(el);
    if (pg === "events") return obsEvents(el);
    if (pg === "channels") return obsChannels(el);
    return obsMetrics(el);
  }

  function obsMetrics(el) {
    var p = livePage(el, "指标监控", "可观测数据源与 PromQL 查询", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newds>+ 新建数据源</button>', tabsHTML(OBS_TABS, "#/observe"));
    el.querySelector("[data-refresh]").onclick = function () { obsMetrics(el); };
    el.querySelector("[data-newds]").onclick = function () { dsModal(function () { obsMetrics(el); }); };
    function load() {
      var t = p.newToken();
      dsList().then(function (data) {
        var list = Array.isArray(data) ? data : asArray(data, ["items", "datasources"]);
        var proms = list.filter(function (d) { return d.type === "prometheus"; });
        if (OB.promId && !proms.some(function (d) { return String(d.id) === String(OB.promId); })) OB.promId = 0;
        if (!OB.promId && proms.length) OB.promId = proms[0].id;
        var html = panelHTML("数据源", dsTable(list), '<button class="x-btn" type="button" data-act="newds" style="margin-left:auto">+ 新建</button>');
        html += promPanel(proms);
        if (!p.paint(t, html)) return;
        bindMetrics(list, proms);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    load();

    function dsTable(list) {
      if (!list.length) return emptyHTML("未配置数据源。点击“新建数据源”接入 Prometheus / Loki / Alertmanager / Jaeger / Tempo。");
      var rows = list.map(function (d) {
        return [E(d.name || "—"), tag(d.type, d.type === "prometheus" ? "x-blue" : "x-grey"), E(d.env || "*"), code(d.url, 60),
          tag(d.auth_type || "none"), d.has_webhook_token ? tag("webhook", "x-green") : E("—"),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-testds="' + E(d.id) + '">测试</button><button class="x-btn" type="button" data-delds="' + E(d.id) + '">删除</button></span>'];
      });
      return tbl(["名称", "类型", "环境", "地址", "认证", "Webhook", "操作"], rows);
    }
    function promPanel(proms) {
      if (!proms.length) return panelHTML("PromQL 查询", emptyHTML("未接入 Prometheus 数据源，无法执行查询。"));
      var opts = proms.map(function (d) { return { v: d.id, t: (d.name || ("#" + d.id)) + (d.env ? " · " + d.env : "") }; });
      var ranges = [{ v: "15m", t: "近 15 分钟" }, { v: "1h", t: "近 1 小时" }, { v: "6h", t: "近 6 小时" }, { v: "24h", t: "近 24 小时" }, { v: "7d", t: "近 7 天" }];
      return panelHTML("PromQL 查询", barHTML([
        '<select class="x-select" data-promds aria-label="Prometheus 数据源" style="width:auto">' + opts.map(function (o) { return '<option value="' + E(o.v) + '"' + (String(o.v) === String(OB.promId) ? " selected" : "") + ">" + E(o.t) + "</option>"; }).join("") + "</select>",
        '<select class="x-select" data-promrange aria-label="时间范围" style="width:auto">' + ranges.map(function (r) { return '<option value="' + r.v + '"' + (r.v === OB.range ? " selected" : "") + ">" + r.t + "</option>"; }).join("") + "</select>",
        '<input class="x-input x-search" data-promq value="' + E(OB.query) + '" placeholder="PromQL，如 up 或 rate(http_requests_total[5m])" aria-label="PromQL 查询语句" />',
        '<button class="x-btn x-btn--pri" type="button" data-promrun>查询</button>'
      ]) + '<div data-promres>' + emptyHTML("输入 PromQL 后点击查询") + "</div>");
    }
    function bindMetrics(list, proms) {
      [].slice.call(p.box.querySelectorAll("[data-testds]")).forEach(function (b) {
        b.onclick = function () {
          b.disabled = true;
          POST("/observability/datasources/" + encodeURIComponent(b.getAttribute("data-testds")) + "/test").then(function (r) {
            b.disabled = false;
            if (r && r.ok) toast("连接成功" + (r.latency_ms != null ? "（" + r.latency_ms + " ms）" : "") + (r.info && r.info.version ? " · " + r.info.version : ""));
            else toast("连接失败：" + ((r && r.error) || "上游不可用"));
          }).catch(function (err) { b.disabled = false; toast(errMsg(err)); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-delds]")).forEach(function (b) {
        b.onclick = function () {
          var id = b.getAttribute("data-delds");
          confirmDo("删除数据源", "确定删除数据源 <b>#" + E(id) + "</b>？其告警记录会级联删除。", "删除", function () {
            write(DEL("/observability/datasources/" + encodeURIComponent(id)), "数据源已删除").then(function (r) { if (r) load(); });
          });
        };
      });
      var sel = p.box.querySelector("[data-promds]"), range = p.box.querySelector("[data-promrange]");
      if (sel) sel.onchange = function () { OB.promId = +sel.value; };
      if (range) range.onchange = function () { OB.range = range.value; };
      var run = p.box.querySelector("[data-promrun]");
      if (run) run.onclick = function () { runProm(p.box); };
      var inp = p.box.querySelector("[data-promq]");
      if (inp) inp.onkeydown = function (e) { if (e.key === "Enter") { e.preventDefault(); runProm(p.box); } };
      var nw = p.box.querySelector('[data-act="newds"]');
      if (nw) nw.onclick = function () { dsModal(function () { obsMetrics(el); }); };
    }
  }

  function runProm(root) {
    var ds = root.querySelector("[data-promds]");
    var range = root.querySelector("[data-promrange]");
    var inp = root.querySelector("[data-promq]");
    var box = root.querySelector("[data-promres]");
    if (!ds || !box) return;
    OB.promId = ds.value; OB.range = range ? range.value : "1h"; OB.query = inp ? inp.value.trim() : "";
    if (!OB.query) { box.innerHTML = emptyHTML("请输入 PromQL 查询语句"); return; }
    var steps = { "15m": "15s", "1h": "30s", "6h": "2m", "24h": "5m", "7d": "30m" };
    box.innerHTML = loadingHTML("查询中…");
    GET("/observability/prometheus/" + encodeURIComponent(OB.promId) + "/query_range", {
      query: OB.query, start: "now-" + OB.range, end: "now", step: steps[OB.range] || "30s"
    }).then(function (res) { box.innerHTML = promHTML(res); })
      .catch(function (err) {
        box.innerHTML = errorHTML(err);
        var b = box.querySelector("[data-opretry]");
        if (b) b.onclick = function () { runProm(root); };
      });
  }

  function promLabels(metric) {
    var keys = Object.keys(metric || {}).sort();
    if (!keys.length) return '<span class="x-frac">{}</span>';
    return '<span class="x-mono" title="' + E(JSON.stringify(metric)) + '">' + E(keys.map(function (k) { return k + "=" + String(metric[k]); }).join(", ")) + "</span>";
  }
  function spark(values) {
    var nums = (values || []).map(function (p) { return parseFloat(p[1]); }).filter(function (v) { return isFinite(v); });
    if (nums.length < 2) return '<span class="x-frac">—</span>';
    var min = Math.min.apply(null, nums), max = Math.max.apply(null, nums), w = 120, h = 26;
    var pts = nums.map(function (v, i) {
      var x = (i / (nums.length - 1)) * w;
      var y = h - 2 - ((v - min) / (max - min || 1)) * (h - 4);
      return x.toFixed(1) + "," + y.toFixed(1);
    }).join(" ");
    return '<svg width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + " " + h + '" aria-hidden="true" style="display:block"><polyline fill="none" stroke="#2563eb" stroke-width="1.5" points="' + pts + '"></polyline></svg>';
  }
  function promHTML(res) {
    if (!res || (res.status && res.status !== "success")) {
      return '<p class="x-aerr" role="alert">' + E((res && (res.error || res.status)) || "Prometheus 未返回有效结果") + "</p>";
    }
    var d = res.data || {}, type = d.resultType, result = d.result;
    if (type === "vector" && Array.isArray(result)) {
      var rows = result.map(function (r) { return [promLabels(r.metric), fmtTime(r.value && r.value[0] ? +r.value[0] * 1000 : null), num(r.value && r.value[1])]; });
      return result.length ? tbl(["指标", "时间", "值"], rows) : emptyHTML("查询成功，但结果为空");
    }
    if (type === "matrix" && Array.isArray(result)) {
      var mrows = result.map(function (r) {
        var vals = r.values || [], last = vals.length ? vals[vals.length - 1][1] : null;
        return [promLabels(r.metric), num(last), num(vals.length), spark(vals)];
      });
      return result.length ? tbl(["指标", "最新值", "数据点", "趋势"], mrows) : emptyHTML("查询成功，但结果为空");
    }
    if (type === "scalar" || type === "string") return preBlock(result);
    return preBlock(res);
  }

  function dsModal(done) {
    formModal("新建数据源", [
      { k: "name", label: "名称", required: true, placeholder: "prom-prod" },
      { k: "type", label: "类型", kind: "select", required: true, options: ["prometheus", "loki", "elasticsearch", "alertmanager", "jaeger", "tempo"], value: "prometheus" },
      { k: "url", label: "地址", required: true, placeholder: "https://prometheus:9090" },
      { k: "env", label: "环境", placeholder: "prod / test / dev（可留空）" },
      { k: "auth_type", label: "认证方式", kind: "select", options: ["none", "basic", "bearer"], value: "none" },
      { k: "username", label: "用户名（basic）" },
      { k: "secret", label: "密码 / Bearer Token", kind: "password" },
      { k: "tls_skip_verify", label: "跳过 TLS 校验", kind: "checkbox", value: false, hint: "跳过" },
      { k: "timeout_sec", label: "超时（秒）", kind: "number", value: "30" },
      { k: "options", label: "options JSON（ES: index/time_field；Loki: org_id）", kind: "textarea", rows: 3, value: "{}" }
    ], "创建", function (v) {
      var opts;
      try { opts = parseJSON(v.options || "{}", {}); } catch (err) { toast(errMsg(err)); return; }
      var body = {
        name: v.name, type: v.type, url: v.url, env: v.env, auth_type: v.auth_type, username: v.username,
        secret: v.secret, tls_skip_verify: v.tls_skip_verify, timeout_sec: +v.timeout_sec || 30, options: opts
      };
      write(POST("/observability/datasources", body), "数据源已创建").then(function (r) {
        if (!r) return;
        closeAll();
        if (r.webhook_token) infoModal("Alertmanager Webhook（仅显示一次）", '<p class="x-desc">请立即复制保存，关闭后不可再次查看：</p>' + preBlock("URL: " + (r.webhook_path || "") + "\nToken: " + r.webhook_token));
        if (done) done();
      });
    }, "URL 必须是 http(s) 绝对地址；secret 会加密存储，接口只返回掩码。");
  }

  function obsRules(el) {
    var p = livePage(el, "告警规则", "Prometheus 告警 / 记录规则与状态", '<button class="x-btn" type="button" data-refresh>刷新</button>', tabsHTML(OBS_TABS, "#/observe/rules"));
    var PROMS = [];
    function load() {
      var t = p.newToken();
      dsList({ type: "prometheus" }).then(function (data) {
        PROMS = Array.isArray(data) ? data : asArray(data, ["items"]);
        if (!PROMS.length) { p.paint(t, panelHTML("告警规则", emptyHTML("未接入 Prometheus 数据源，无法读取告警规则。"))); return; }
        var sel = '<div class="x-tbar"><select class="x-select" data-ruleds style="width:auto">' + PROMS.map(function (d) { return '<option value="' + E(d.id) + '">' + E(d.name || ("#" + d.id)) + " · " + E(d.env || "*") + "</option>"; }).join("") + '</select><span class="x-frac" data-rulemeta></span></div>';
        if (!p.paint(t, panelHTML("告警规则", sel + '<div data-ruleres>' + loadingHTML("读取规则中…") + "</div>"))) return;
        var s = p.box.querySelector("[data-ruleds]");
        s.onchange = function () { loadRules(s.value); };
        loadRules(PROMS[0].id);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function loadRules(id) {
      var box = p.box.querySelector("[data-ruleres]");
      if (!box) return;
      box.innerHTML = loadingHTML("读取规则中…");
      GET("/observability/prometheus/" + encodeURIComponent(id) + "/rules").then(function (res) {
        var groups = res && res.data && Array.isArray(res.data.groups) ? res.data.groups : [];
        var total = groups.reduce(function (n, g) { return n + ((g.rules || []).length); }, 0);
        var meta = p.box.querySelector("[data-rulemeta]");
        if (meta) meta.textContent = "共 " + groups.length + " 组 / " + total + " 条规则";
        if (!groups.length) { box.innerHTML = emptyHTML("该 Prometheus 没有告警规则"); return; }
        box.innerHTML = groups.map(function (g) {
          var rows = (g.rules || []).map(function (r) {
            var sev = r.labels && (r.labels.severity || r.labels.level);
            var st = r.state || r.health || r.type || (r.type === "recording" ? "recording" : "—");
            return [E(r.name || "—"), code(r.query, 120), E(r.duration || "—"), sevTag(sev), stTag(st), E(r.type || "alerting")];
          });
          return '<h3 class="x-h2" style="font-size:13px;margin:16px 0 8px">' + E(g.name || g.file || "规则组") + " <span class=\"x-frac\">" + (rows.length) + " 条</span></h3>" +
            tbl(["规则", "表达式", "持续时间", "级别", "状态", "类型"], rows);
        }).join("");
      }).catch(function (err) {
        box.innerHTML = errorHTML(err);
        var b = box.querySelector("[data-opretry]");
        if (b) b.onclick = function () { loadRules(id); };
      });
    }
    load();
  }

  // 告警路径 key：优先数字 id（同一 fingerprint 可能存在于多个 Alertmanager 数据源，
  // 用 id 可避免 ?datasource 消歧 409）；claim/close/timeline 都接受 id 或 fingerprint。
  function alertKey(a) { return String(a && a.id != null && a.id !== "" ? a.id : ((a && (a.fingerprint || a.key)) || "")); }
  function claimPayload(a, comment, duration, silence) {
    var body = { comment: comment || "", duration: duration || "2h", silence: silence !== false };
    if (a && a.datasource_id) body.datasource_id = a.datasource_id;
    return body;
  }

  function obsEvents(el) {
    var p = livePage(el, "告警事件", "Alertmanager 告警的认领、关闭与静默", '<button class="x-btn" type="button" data-refresh>刷新</button>', tabsHTML(OBS_TABS, "#/observe/events"));
    var F = { status: "", handle: "", severity: "", q: "" };
    var AMS = [];
    function load() {
      var t = p.newToken();
      var qy = { page: 1, page_size: 50 };
      if (F.status) qy.status = F.status;
      if (F.handle) qy.handle_status = F.handle;
      if (F.severity) qy.severity = F.severity;
      if (F.q) qy.q = F.q;
      Promise.all([
        GET("/observability/alerts", qy),
        settle(dsList({ type: "alertmanager" }), [])
      ]).then(function (rs) {
        var data = rs[0] || {}, items = Array.isArray(data) ? data : asArray(data, ["items"]);
        AMS = rs[1].ok ? (Array.isArray(rs[1].value) ? rs[1].value : asArray(rs[1].value, ["items"])) : [];
        if (!p.paint(t, evHTML(items, data))) return;
        bindEvents(items);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function evHTML(items, data) {
      var filters = barHTML([
        selHTML("st", "状态", [{ v: "", t: "全部状态" }, { v: "firing", t: "告警中" }, { v: "resolved", t: "已恢复" }], F.status, " data-fst style=\"width:auto\""),
        selHTML("handle", "处理状态", [{ v: "", t: "全部处理状态" }, { v: "open", t: "未处理" }, { v: "acked", t: "已认领" }, { v: "closed", t: "已关闭" }], F.handle, " data-fhandle style=\"width:auto\""),
        selHTML("sev", "级别", [{ v: "", t: "全部级别" }, { v: "P0", t: "P0" }, { v: "P1", t: "P1" }, { v: "P2", t: "P2" }, { v: "P3", t: "P3" }, { v: "critical", t: "critical" }, { v: "warning", t: "warning" }, { v: "info", t: "info" }], F.severity, " data-fsev style=\"width:auto\""),
        '<input class="x-input x-search" data-fq value="' + E(F.q) + '" placeholder="搜索告警名 / 注解" aria-label="搜索告警" />',
        '<button class="x-btn x-btn--pri" type="button" data-frun>查询</button>',
        '<span class="x-frac" style="margin-left:auto">共 ' + num((data && data.total) != null ? data.total : items.length) + " 条</span>"
      ]);
      var rows = items.map(function (a, i) {
        var labels = a.labels || {}, anno = a.annotations || {};
        var summary = anno.summary || anno.description || a.message || "";
        var closed = String(a.handle_status) === "closed";
        return [stTag(a.status), sevTag(a.severity || labels.severity), E(a.alertname || labels.alertname || "—"),
          E(a.env || "—"), code(summary, 90), fmtMin(a.starts_at || a.startsAt), stTag(a.handle_status || "open"),
          E(a.acked_by || a.closed_by || "—"),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-claim="' + i + '"' + (closed ? " disabled" : "") + ">认领</button>" +
          '<button class="x-btn" type="button" data-closea="' + i + '"' + (closed ? " disabled" : "") + ">关闭</button>" +
          '<button class="x-btn" type="button" data-tl="' + i + '">时间线</button></span>'];
      });
      var alertPanel = panelHTML("告警列表", filters + (items.length ? tbl(["状态", "级别", "告警", "环境", "摘要", "开始", "处理", "处理人", "操作"], rows) : emptyHTML("当前没有告警记录。Alertmanager Webhook 触发后会自动写入。")));
      return alertPanel + silencePanel();
    }
    function silencePanel() {
      if (!AMS.length) return panelHTML("静默", emptyHTML("未接入 Alertmanager 数据源，无法管理静默。"));
      var sel = '<select class="x-select" data-silds style="width:auto">' + AMS.map(function (d) { return '<option value="' + E(d.id) + '">' + E(d.name || ("#" + d.id)) + "</option>"; }).join("") + "</select>";
      return panelHTML("静默", barHTML([sel, '<button class="x-btn" type="button" data-newsil>+ 新建静默</button>', '<span class="x-frac" data-silmeta style="margin-left:auto"></span>']) + '<div data-silres>' + loadingHTML("读取静默中…") + "</div>");
    }
    function bindEvents(items) {
      var st = p.box.querySelector("[data-fst]"), hd = p.box.querySelector("[data-fhandle]"), sev = p.box.querySelector("[data-fsev]"), fq = p.box.querySelector("[data-fq]");
      if (st) st.onchange = function () { F.status = st.value; load(); };
      if (hd) hd.onchange = function () { F.handle = hd.value; load(); };
      if (sev) sev.onchange = function () { F.severity = sev.value; load(); };
      var run = p.box.querySelector("[data-frun]");
      if (run) run.onclick = function () { F.q = fq.value.trim(); load(); };
      if (fq) fq.onkeydown = function (e) { if (e.key === "Enter") { e.preventDefault(); F.q = fq.value.trim(); load(); } };
      function pick(i) { return items[+i] || {}; }
      [].slice.call(p.box.querySelectorAll("[data-claim]")).forEach(function (b) {
        b.onclick = function () { claimModal(pick(b.getAttribute("data-claim"))); };
      });
      [].slice.call(p.box.querySelectorAll("[data-closea]")).forEach(function (b) {
        b.onclick = function () { closeModal(pick(b.getAttribute("data-closea"))); };
      });
      [].slice.call(p.box.querySelectorAll("[data-tl]")).forEach(function (b) {
        b.onclick = function () { timelineModal(pick(b.getAttribute("data-tl"))); };
      });
      var silds = p.box.querySelector("[data-silds]");
      if (silds) { silds.onchange = function () { loadSilences(silds.value); }; loadSilences(silds.value); }
      var newsil = p.box.querySelector("[data-newsil]");
      if (newsil) newsil.onclick = function () { silenceModal(silds ? silds.value : (AMS[0] && AMS[0].id), function () { if (silds) loadSilences(silds.value); }); };
      bindSilenceList();
    }
    function loadSilences(dsId) {
      var box = p.box.querySelector("[data-silres]");
      if (!box || !dsId) return;
      box.innerHTML = loadingHTML("读取静默中…");
      GET("/observability/silences", { datasource: dsId }).then(function (data) {
        var list = Array.isArray(data) ? data : asArray(data, ["silences", "items"]);
        var meta = p.box.querySelector("[data-silmeta]");
        if (meta) meta.textContent = "共 " + list.length + " 条静默";
        if (!list.length) { box.innerHTML = emptyHTML("当前没有生效中的静默"); return; }
        var rows = list.map(function (s) {
          var m = (s.matchers || []).map(function (x) { return (x.name || x.Name || "") + (x.isRegex || x.IsRegex ? "=~" : "=") + (x.value || x.Value || ""); }).join(", ");
          return [stTag(s.status || "active"), code(m, 80), fmtMin(s.startsAt || s.starts_at), fmtMin(s.endsAt || s.ends_at),
            E(s.createdBy || s.created_by || "—"), code(s.comment || "—", 60),
            '<button class="x-btn" type="button" data-delsil="' + E(s.id) + '">删除</button>'];
        });
        box.innerHTML = tbl(["状态", "匹配器", "开始", "结束", "创建人", "备注", "操作"], rows);
        bindSilenceList();
      }).catch(function (err) { box.innerHTML = errorHTML(err); });
    }
    function bindSilenceList() {
      [].slice.call(p.box.querySelectorAll("[data-delsil]")).forEach(function (b) {
        b.onclick = function () {
          var dsId = p.box.querySelector("[data-silds]").value;
          confirmDo("删除静默", "确定删除该 Alertmanager 静默？", "删除", function () {
            write(DEL("/observability/silences/" + encodeURIComponent(b.getAttribute("data-delsil")), { datasource: dsId }), "静默已删除").then(function (r) { if (r) loadSilences(dsId); });
          });
        };
      });
    }
    // 认领：POST /observability/alerts/:id/claim {comment,duration,datasource_id,silence}
    // 412 = 该 env 没有 Alertmanager 数据源（可只认领）；502 = 上游失败（告警不会被认领）。
    function claimModal(a) {
      formModal("认领并静默告警", [
        { k: "comment", label: "认领备注", kind: "textarea", rows: 3, placeholder: "已介入处理…" },
        { k: "duration", label: "静默时长", kind: "select", options: [{ v: "1h", t: "1 小时" }, { v: "2h", t: "2 小时" }, { v: "6h", t: "6 小时" }, { v: "24h", t: "24 小时" }], value: "2h" }
      ], "认领并静默", function (v) { doClaim(a, v.comment, v.duration, true); },
      "认领后由后端在 Alertmanager 创建静默；若该环境未配置 Alertmanager 会提示是否只认领。");
    }
    function doClaim(a, comment, duration, silence) {
      POST("/observability/alerts/" + encodeURIComponent(alertKey(a)) + "/claim", claimPayload(a, comment, duration, silence)).then(function (r) {
        closeAll();
        toast(r && r.silenced === false ? "已认领（未创建静默）" : "已认领并创建静默");
        load();
      }).catch(function (err) {
        if (err && err.status === 412 && silence) {
          closeAll();
          confirmDo("未创建静默", E(errMsg(err)) + "<br />是否只认领告警（不创建静默）？", "只认领", function () { doClaim(a, comment, duration, false); });
          return;
        }
        toast(errMsg(err));
      });
    }
    function closeModal(a) {
      formModal("关闭告警", [{ k: "comment", label: "关闭说明", kind: "textarea", rows: 3, placeholder: "已恢复 / 误报说明" }], "关闭", function (v) {
        doClose(a, v.comment, false);
      }, "关闭会先删除认领时创建的 Alertmanager 静默；删除失败时不会假装关闭。");
    }
    function doClose(a, comment, force) {
      POST("/observability/alerts/" + encodeURIComponent(alertKey(a)) + "/close", { comment: comment || "", force: !!force }).then(function (r) {
        closeAll();
        toast("告警已关闭" + (r && r.silence_deleted === false ? "（静默删除失败，已记录时间线）" : ""));
        load();
      }).catch(function (err) {
        if (err && err.status === 502 && !force) {
          closeAll();
          confirmDo("静默删除失败", E(errMsg(err)) + "<br />确需关闭可强制关闭（保留上游静默并记录失败）。", "强制关闭", function () { doClose(a, comment, true); });
          return;
        }
        toast(errMsg(err));
      });
    }
    function timelineModal(a) {
      GET("/observability/alerts/" + encodeURIComponent(alertKey(a)) + "/timeline").then(function (data) {
        var list = asArray(data, ["items", "events", "timeline"]);
        if (!list.length) { infoModal("告警时间线", emptyHTML("暂无时间线记录")); return; }
        var rows = list.map(function (e) {
          return [fmtTime(e.created_at || e.at || e.time), stTag(e.kind || e.type || "—"), E(e.actor || e.username || "—"), E(e.message || e.comment || e.detail || "—")];
        });
        infoModal("告警时间线", tbl(["时间", "事件", "操作人", "说明"], rows));
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function silenceModal(dsId, done) {
      if (!dsId) { toast("请先配置 Alertmanager 数据源"); return; }
      formModal("新建静默", [
        { k: "matcher", label: "匹配器（name=value，多个用逗号分隔）", required: true, placeholder: 'alertname=HighErrorRate, env=prod' },
        { k: "duration", label: "静默时长", kind: "select", options: [{ v: "30m", t: "30 分钟" }, { v: "1h", t: "1 小时" }, { v: "2h", t: "2 小时" }, { v: "6h", t: "6 小时" }, { v: "24h", t: "24 小时" }], value: "2h" },
        { k: "comment", label: "备注", required: true, placeholder: "计划维护 / 已知问题" }
      ], "创建静默", function (v) {
        var matchers = String(v.matcher).split(",").map(function (s) { return s.trim(); }).filter(Boolean).map(function (s) {
          var i = s.indexOf("=");
          if (i < 1) throw { message: "匹配器格式应为 name=value：" + s };
          return { name: s.slice(0, i).trim(), value: s.slice(i + 1).trim(), isRegex: false, isEqual: true };
        });
        write(POST("/observability/silences", { datasource: +dsId, startsAt: "now", matchers: matchers, duration: v.duration, comment: v.comment }), "静默已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }

  function obsChannels(el) {
    var p = livePage(el, "通知渠道", "告警通知渠道与测试发送", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newch>+ 新建渠道</button>', tabsHTML(OBS_TABS, "#/observe/channels"));
    el.querySelector("[data-refresh]").onclick = function () { obsChannels(el); };
    el.querySelector("[data-newch]").onclick = function () { channelModal(function () { obsChannels(el); }); };
    function load() {
      var t = p.newToken();
      GET("/observability/channels").then(function (data) {
        var list = Array.isArray(data) ? data : asArray(data, ["items", "channels"]);
        var rows = list.map(function (c) {
          return [E(c.name || "—"), tag(c.type || "—", "x-blue"), c.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
            E((c.severities || []).join(", ") || "全部"), E((c.envs || []).join(", ") || "全部"),
            fmtMin(c.updated_at), '<span class="ops-nowrap"><button class="x-btn" type="button" data-chtest="' + E(c.id) + '">测试</button>' +
            '<button class="x-btn" type="button" data-chtoggle="' + E(c.id) + '">' + (c.enabled ? "停用" : "启用") + "</button>" +
            '<button class="x-btn" type="button" data-chdel="' + E(c.id) + '">删除</button></span>'];
        });
        var html = panelHTML("通知渠道", list.length ? tbl(["名称", "类型", "状态", "级别过滤", "环境过滤", "更新时间", "操作"], rows) : emptyHTML("尚未配置通知渠道。支持飞书 / 钉钉 / 企业微信 / 邮件。"));
        if (!p.paint(t, html)) return;
        bindChannels(list);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function bindChannels(list) {
      function byId(id) { for (var i = 0; i < list.length; i++) if (String(list[i].id) === String(id)) return list[i]; return null; }
      [].slice.call(p.box.querySelectorAll("[data-chtest]")).forEach(function (b) {
        b.onclick = function () {
          b.disabled = true;
          POST("/observability/channels/" + encodeURIComponent(b.getAttribute("data-chtest")) + "/test").then(function (r) {
            b.disabled = false;
            toast("测试消息已发送" + (r && r.ok === false ? "失败：" + (r.error || r.message) : ""));
          }).catch(function (err) { b.disabled = false; toast(errMsg(err)); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-chtoggle]")).forEach(function (b) {
        b.onclick = function () {
          var c = byId(b.getAttribute("data-chtoggle"));
          if (!c) return;
          write(PUT("/observability/channels/" + encodeURIComponent(c.id), { name: c.name, type: c.type, enabled: !c.enabled, severities: c.severities || [], envs: c.envs || [] }), !c.enabled ? "渠道已启用" : "渠道已停用").then(function (r) { if (r) load(); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-chdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除通知渠道", "确定删除该通知渠道？", "删除", function () {
            write(DEL("/observability/channels/" + encodeURIComponent(b.getAttribute("data-chdel"))), "渠道已删除").then(function (r) { if (r) load(); });
          });
        };
      });
    }
    function channelModal(done) {
      formModal("新建通知渠道", [
        { k: "name", label: "名称", required: true, placeholder: "SRE 告警群" },
        { k: "type", label: "类型", kind: "select", required: true, options: [{ v: "feishu", t: "飞书" }, { v: "dingtalk", t: "钉钉" }, { v: "wecom", t: "企业微信" }, { v: "email", t: "邮件" }], value: "feishu" },
        { k: "webhook", label: "Webhook 地址（飞书/钉钉/企业微信）", placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/..." },
        { k: "secret", label: "签名密钥（可选）" },
        { k: "host", label: "SMTP 主机（邮件）", placeholder: "smtp.exmail.qq.com" },
        { k: "port", label: "SMTP 端口（邮件）", kind: "number", value: "465" },
        { k: "from", label: "发件人（邮件）" },
        { k: "to", label: "收件人（邮件，逗号分隔）" },
        { k: "password", label: "SMTP 密码（邮件）", kind: "password" }
      ], "创建", function (v) {
        var cfg;
        if (v.type === "email") cfg = { host: v.host, port: +v.port || 25, from: v.from, to: String(v.to || "").split(",").map(function (s) { return s.trim(); }).filter(Boolean), password: v.password };
        else cfg = { webhook: v.webhook, secret: v.secret };
        write(POST("/observability/channels", { name: v.name, type: v.type, config: cfg, enabled: true, severities: [], envs: [] }), "渠道已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, "仅飞书/钉钉/企业微信使用 Webhook；邮件使用 SMTP 配置。");
    }
    load();
  }

  /* ---------------------------------------------------------------- logs 日志检索 */

  var LG = { dsId: 0, win: 60, limit: 100, level: "", q: "", index: "" };

  function renderLogsLive(el) {
    var p = livePage(el, "日志检索", "Loki / Elasticsearch 统一日志查询", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    var DSTS = [];
    function load() {
      var t = p.newToken();
      dsList().then(function (data) {
        var list = (Array.isArray(data) ? data : asArray(data, ["items"])).filter(function (d) { return d.type === "loki" || d.type === "elasticsearch"; });
        DSTS = list;
        if (!list.length) {
          p.paint(t, '<div class="x-panel">' + emptyHTML("未接入 Loki 或 Elasticsearch 数据源，无法查询日志。请先在「监控告警 › 指标监控」中添加数据源。") + "</div>");
          return;
        }
        if (!list.some(function (d) { return String(d.id) === String(LG.dsId); })) LG.dsId = list[0].id;
        if (!p.paint(t, formHTML())) return;
        bindForm();
        runQuery();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function curDs() {
      for (var i = 0; i < DSTS.length; i++) if (String(DSTS[i].id) === String(LG.dsId)) return DSTS[i];
      return null;
    }
    function formHTML() {
      var ds = curDs(), isES = ds && ds.type === "elasticsearch";
      return panelHTML("查询条件", barHTML([
        '<select class="x-select" data-lgds style="width:auto">' + DSTS.map(function (d) { return '<option value="' + E(d.id) + '"' + (String(d.id) === String(LG.dsId) ? " selected" : "") + ">" + E(d.name) + " · " + E(d.type) + "</option>"; }).join("") + "</select>",
        '<select class="x-select" data-lgwin style="width:auto">' + [{ v: 15, t: "近 15 分钟" }, { v: 60, t: "近 1 小时" }, { v: 360, t: "近 6 小时" }, { v: 1440, t: "近 24 小时" }].map(function (w) { return '<option value="' + w.v + '"' + (+LG.win === w.v ? " selected" : "") + ">" + w.t + "</option>"; }).join("") + "</select>",
        '<input class="x-input x-search" data-lgq value="' + E(LG.q) + '" placeholder="' + (isES ? "Lucene/KQL，如 level:error and service:api" : "LogQL，如 {app=\"api\"} |= \"error\"") + '" aria-label="日志查询语句" />',
        isES ? '<input class="x-input" data-lgindex style="width:130px" value="' + E(LG.index) + '" placeholder="索引，如 logs-*" aria-label="索引" />' : "",
        '<select class="x-select" data-lglimit style="width:auto">' + [100, 200, 500, 1000].map(function (n) { return '<option value="' + n + '"' + (+LG.limit === n ? " selected" : "") + ">" + n + " 条</option>"; }).join("") + "</select>",
        '<button class="x-btn x-btn--pri" type="button" data-lgrun>查询</button>'
      ]) + '<div data-lgres>' + emptyHTML("点击查询检索日志") + "</div>");
    }
    function bindForm() {
      var ds = p.box.querySelector("[data-lgds]");
      if (ds) ds.onchange = function () { LG.dsId = +ds.value; if (!p.paint(null, formHTML())) return; bindForm(); runQuery(); };
      var win = p.box.querySelector("[data-lgwin]");
      if (win) win.onchange = function () { LG.win = +win.value; };
      var lim = p.box.querySelector("[data-lglimit]");
      if (lim) lim.onchange = function () { LG.limit = +lim.value; };
      var idx = p.box.querySelector("[data-lgindex]");
      var iq = p.box.querySelector("[data-lgq]");
      if (iq) iq.onkeydown = function (e) { if (e.key === "Enter") { e.preventDefault(); runQuery(); } };
      var run = p.box.querySelector("[data-lgrun]");
      if (run) run.onclick = function () { runQuery(); };
      function readInputs() {
        var d = p.box.querySelector("[data-lgds]"), w = p.box.querySelector("[data-lgwin]"), l = p.box.querySelector("[data-lglimit]");
        if (d) LG.dsId = +d.value;
        if (w) LG.win = +w.value;
        if (l) LG.limit = +l.value;
        if (iq) LG.q = iq.value.trim();
        if (idx) LG.index = idx.value.trim();
      }
      p._readLogInputs = readInputs;
    }
    function runQuery() {
      var box = p.box.querySelector("[data-lgres]");
      if (!box) return;
      if (p._readLogInputs) p._readLogInputs();
      box.innerHTML = loadingHTML("查询日志中…");
      var body = { datasource: +LG.dsId, query: LG.q, from: "now-" + (LG.win >= 1440 ? LG.win / 60 + "d" : LG.win + "m"), to: "now", limit: +LG.limit };
      if (LG.index) body.index = LG.index;
      POST("/observability/logs/query", body).then(function (data) { paintLogs(box, data || {}); })
        .catch(function (err) {
          box.innerHTML = errorHTML(err);
          var b = box.querySelector("[data-opretry]");
          if (b) b.onclick = function () { runQuery(); };
        });
    }
    function paintLogs(box, data) {
      var rows = Array.isArray(data.rows) ? data.rows : [];
      var counts = {};
      rows.forEach(function (r) { var lv = String(r.level || "info").toUpperCase(); counts[lv] = (counts[lv] || 0) + 1; });
      var lvOrder = ["ERROR", "WARN", "INFO", "DEBUG", "FATAL", "TRACE"];
      var chips = ['<button class="x-btn' + (LG.level ? "" : " x-btn--pri") + '" type="button" data-lvl="">全部 ' + rows.length + "</button>"]
        .concat(lvOrder.filter(function (lv) { return counts[lv]; }).map(function (lv) {
          return '<button class="x-btn' + (LG.level === lv ? " x-btn--pri" : "") + '" type="button" data-lvl="' + lv + '">' + lv + " " + counts[lv] + "</button>";
        })).join("");
      var vis = rows.filter(function (r) { return !LG.level || String(r.level || "").toUpperCase() === LG.level; });
      var trs = vis.map(function (r, i) {
        var labels = r.labels || {};
        var lv = String(r.level || "info").toUpperCase();
        var cls = lv === "ERROR" || lv === "FATAL" ? "x-red" : lv === "WARN" ? "x-orange" : lv === "DEBUG" ? "x-grey" : "x-blue";
        return '<tr data-lgrow="' + i + '" tabindex="0"><td class="ops-nowrap">' + fmtTime(r.ts) + "</td><td>" + tag(lv, cls) + "</td><td>" + E(r.service || "—") + "</td><td>" + E(r.host || "—") +
          '</td><td style="max-width:620px;word-break:break-word">' + E(r.message || "") + "</td></tr>" +
          '<tr hidden data-lgdet="' + i + '"><td colspan="5"><pre class="x-log ops-json">' + E(JSON.stringify(labels, null, 2)) + "</pre></td></tr>";
      }).join("");
      box.innerHTML = '<div class="x-tbar">' + chips + '<span class="x-frac" style="margin-left:auto">数据源 ' + E(data.datasource || "—") + " · 引擎 " + E(data.engine || "—") + " · 命中 " + num(data.total || rows.length) + "</span></div>" +
        (rows.length ? '<div style="overflow-x:auto"><table class="x-table x-table--plain"><thead><tr><th>时间</th><th>级别</th><th>服务</th><th>主机</th><th>消息</th></tr></thead><tbody>' + trs + "</tbody></table></div>" : emptyHTML("没有匹配的日志，试试放宽时间范围或修改查询语句"));
      var pre = data.query ? '<p class="ops-sub">实际查询：' + code(data.query, 200) + "</p>" : "";
      if (pre) box.insertAdjacentHTML("beforeend", pre);
      [].slice.call(box.querySelectorAll("[data-lvl]")).forEach(function (b) {
        b.onclick = function () { LG.level = b.getAttribute("data-lvl"); paintLogs(box, data); };
      });
      [].slice.call(box.querySelectorAll("[data-lgrow]")).forEach(function (tr) {
        tr.onclick = function () {
          var det = box.querySelector('[data-lgdet="' + tr.getAttribute("data-lgrow") + '"]');
          if (det) det.hidden = !det.hidden;
        };
      });
    }
    load();
  }


  /* ---------------------------------------------------------------- cicd CI/CD */

  var CICD_F = { status: "", appId: 0 };
  var CICD_APPS = [], CICD_PL = [], CICD_TG = [], CICD_JK = [];

  function appsLoad() {
    return GET("/cicd/applications", { page: 1, size: 200 }).then(function (data) {
      CICD_APPS = Array.isArray(data) ? data : asArray(data, ["items", "applications"]);
      return CICD_APPS;
    });
  }
  function appName(id) {
    for (var i = 0; i < CICD_APPS.length; i++) if (String(CICD_APPS[i].id) === String(id)) return CICD_APPS[i].name;
    return "#" + id;
  }
  function pipelineName(id) {
    for (var i = 0; i < CICD_PL.length; i++) if (String(CICD_PL[i].id) === String(id)) return CICD_PL[i].name;
    return "#" + id;
  }
  function cicdTabs(cur) {
    return tabsHTML([{ t: "构建", h: "#/cicd/build" }, { t: "制品", h: "#/cicd/artifact" }, { t: "流水线", h: "#/cicd/pipeline" }], cur);
  }
  function renderCicdLive(el) {
    var seg = (location.hash || "").split("/")[2] || "build";
    if (seg === "pipeline") return cicdPipelines(el);
    if (seg === "artifact") return cicdArtifacts(el);
    return cicdBuilds(el);
  }
  function runStatusOptions() {
    return [{ v: "", t: "全部状态" }, { v: "queued", t: "排队中" }, { v: "running", t: "运行中" }, { v: "succeeded", t: "成功" }, { v: "failed", t: "失败" }, { v: "cancelled", t: "已取消" }];
  }

  function cicdBuilds(el) {
    var p = livePage(el, "构建", "流水线运行记录与实时日志（SSE）", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-runpl>+ 运行流水线</button>', cicdTabs("#/cicd/build"));
    el.querySelector("[data-refresh]").onclick = function () { cicdBuilds(el); };
    el.querySelector("[data-runpl]").onclick = function () { runPipelineModal(function () { cicdBuilds(el); }); };
    function load() {
      var t = p.newToken();
      var qy = { page: 1, page_size: 50 };
      if (CICD_F.status) qy.status = CICD_F.status;
      if (CICD_F.appId) qy.app_id = CICD_F.appId;
      Promise.all([GET("/cicd/runs", qy), settle(appsLoad(), []), settle(GET("/cicd/pipelines"), [])]).then(function (rs) {
        var data = rs[0] || {}, items = Array.isArray(data) ? data : asArray(data, ["items"]);
        CICD_APPS = rs[1].value || [];
        CICD_PL = asArray(rs[2].value, ["items"]);
        var filters = barHTML([
          '<select class="x-select" data-cfst style="width:auto">' + runStatusOptions().map(function (o) { return '<option value="' + E(o.v) + '"' + (o.v === CICD_F.status ? " selected" : "") + ">" + o.t + "</option>"; }).join("") + "</select>",
          '<select class="x-select" data-cfapp style="width:auto"><option value="">全部应用</option>' + CICD_APPS.map(function (a) { return '<option value="' + E(a.id) + '"' + (String(a.id) === String(CICD_F.appId) ? " selected" : "") + ">" + E(a.name) + "</option>"; }).join("") + "</select>",
          '<span class="x-frac" style="margin-left:auto">共 ' + num((data && data.total) != null ? data.total : items.length) + " 次运行</span>"
        ]);
        var rows = items.map(function (r) {
          var active = ["queued", "running", "cancelling"].indexOf(String(r.status)) >= 0;
          return [mono("#" + r.id), E(appName(r.app_id)), E(pipelineName(r.pipeline_id)), stTag(r.status), code(r.branch || "—", 18),
            code(r.commit || "—", 10), code(r.image_tag || "—", 18), E(r.triggered_by || "—"), fmtMin(r.started_at || r.created_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-rund="' + E(r.id) + '">详情</button>' +
            '<button class="x-btn" type="button" data-runl="' + E(r.id) + '">日志</button>' +
            (active ? '<button class="x-btn" type="button" data-runc="' + E(r.id) + '">取消</button>' : "") + "</span>"];
        });
        var html = panelHTML("运行记录", filters + (items.length ? tbl(["运行", "应用", "流水线", "状态", "分支", "提交", "镜像标签", "触发人", "开始", "操作"], rows) : emptyHTML("暂无运行记录。")));
        if (!p.paint(t, html)) return;
        bind(items);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function bind(items) {
      var st = p.box.querySelector("[data-cfst]"), ap = p.box.querySelector("[data-cfapp]");
      if (st) st.onchange = function () { CICD_F.status = st.value; load(); };
      if (ap) ap.onchange = function () { CICD_F.appId = +ap.value; load(); };
      [].slice.call(p.box.querySelectorAll("[data-rund]")).forEach(function (b) {
        b.onclick = function () { runDetailModal(b.getAttribute("data-rund")); };
      });
      [].slice.call(p.box.querySelectorAll("[data-runl]")).forEach(function (b) {
        b.onclick = function () { openRunLog(b.getAttribute("data-runl")); };
      });
      [].slice.call(p.box.querySelectorAll("[data-runc]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("取消运行", "确定取消运行 #" + E(b.getAttribute("data-runc")) + "？", "取消运行", function () {
            write(POST("/cicd/runs/" + encodeURIComponent(b.getAttribute("data-runc")) + "/cancel"), "已请求取消").then(function (r) { if (r) load(); });
          });
        };
      });
    }
    load();
  }

  function runPipelineModal(done, pipelines) {
    var PL = pipelines || CICD_PL;
    if (!PL || !PL.length) { toast("暂无流水线，请先在「流水线」页创建"); return; }
    formModal("运行流水线", [
      { k: "pipeline", label: "流水线", kind: "select", options: PL.map(function (pl) { return { v: pl.id, t: (pl.name || ("#" + pl.id)) + " · 应用 " + appName(pl.app_id) }; }) },
      { k: "branch", label: "分支（留空使用应用默认分支）", placeholder: "main" },
      { k: "commit", label: "提交 SHA（可选）" }
    ], "运行", function (v) {
      write(POST("/cicd/pipelines/" + encodeURIComponent(v.pipeline) + "/run", { branch: v.branch, commit: v.commit }), "已提交运行").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }

  function runDetailModal(id) {
    GET("/cicd/runs/" + encodeURIComponent(id)).then(function (r) {
      var stages = Array.isArray(r.stages) ? r.stages : [];
      var names = stages.map(function (s) { return s.name; });
      var rows = stages.map(function (s) {
        return [E(s.idx), E(s.name), E(s.type), stTag(s.status), code(s.job_name || "—", 40), fmtMin(s.started_at), fmtMin(s.finished_at), code(s.error || "—", 80)];
      });
      var info = '<dl class="x-sum" style="margin:0 0 12px">' +
        [["运行", "#" + r.id], ["应用", E(appName(r.app_id))], ["流水线", E(pipelineName(r.pipeline_id))], ["状态", stTag(r.status)],
          ["分支", code(r.branch || "—", 30)], ["提交", code(r.commit || "—", 16)], ["镜像标签", code(r.image_tag || "—", 30)],
          ["触发", E(r.triggered_by || "—")], ["开始", fmtMin(r.started_at)], ["结束", fmtMin(r.finished_at)], ["错误", code(r.error || "—", 120)]]
          .map(function (kv) { return "<dt>" + kv[0] + "</dt><dd>" + kv[1] + "</dd>"; }).join("") + "</dl>";
      var foot = '<button class="x-btn" type="button" data-close>关闭</button><button class="x-btn x-btn--pri" type="button" id="opsRunLogBtn">查看实时日志</button>';
      dlg("运行详情 · #" + E(r.id), '<div class="x-form">' + info + (rows.length ? tbl(["#", "阶段", "类型", "状态", "Job", "开始", "结束", "错误"], rows) : emptyHTML("该运行没有阶段记录")) + "</div>", foot);
      var lb = q("opsRunLogBtn");
      if (lb) lb.onclick = function () { openRunLog(r.id, names); };
    }).catch(function (err) { toast(errMsg(err)); });
  }

  function stopRunStream() {
    if (typeof ST.runStream === "function") { try { ST.runStream(); } catch (e) {} }
    ST.runStream = null;
  }
  function openRunLog(runId, stageNames) {
    stopRunStream();
    dlg("运行日志 · #" + E(runId), '<pre class="x-log ops-ro" id="ops-runlog" aria-live="polite" aria-label="运行日志">连接日志流…' + "\n" + "</pre>",
      '<button class="x-btn" type="button" data-close>关闭</button><span class="x-test x-mr" id="ops-runst" role="status">实时跟随中…</span>', true);
    var pre = q("ops-runlog"), st = q("ops-runst"), seen = 0;
    function setSt(text) { if (st && document.body.contains(st)) st.textContent = text; }
    function append(line) {
      if (!pre || !document.body.contains(pre)) { stopRunStream(); return; }
      if (seen > 6000) return;
      seen++;
      pre.insertAdjacentText("beforeend", line + "\n");
      pre.scrollTop = pre.scrollHeight;
    }
    function label(l) {
      var s = l.stage;
      if (s == null || s < 0) return "";
      var n = stageNames && stageNames[s];
      return "[" + (n || ("stage " + s)) + "] ";
    }
    var x = window.XS;
    if (x && typeof x.stream === "function") {
      ST.runStream = x.stream("/cicd/runs/" + encodeURIComponent(runId) + "/logs", {
        log: function (data) {
          try { var l = JSON.parse(data); append(label(l) + (l.line || "")); } catch (e) { append(String(data)); }
        },
        end: function (data) {
          try { var d = JSON.parse(data); setSt("已结束 · " + (d.status || "")); } catch (e) { setSt("已结束"); }
        },
        error: function (err) { append("[日志流错误] " + errMsg(err)); setSt("日志流中断"); }
      }, { follow: 1 });
    } else {
      var after = 0, timer = null, stopped = false;
      ST.runStream = function () { stopped = true; clearTimeout(timer); };
      function poll() {
        if (stopped) return;
        GET("/cicd/runs/" + encodeURIComponent(runId) + "/logs", { after: after }).then(function (list) {
          var rows = Array.isArray(list) ? list : asArray(list, ["items", "logs"]);
          rows.forEach(function (l) { after = Math.max(after, +l.id || 0); append(label(l) + (l.line || "")); });
          if (!stopped && pre && document.body.contains(pre)) timer = setTimeout(poll, 2000); else stopped = true;
        }).catch(function (err) { append("[日志轮询失败] " + errMsg(err)); setSt("日志流中断"); });
      }
      poll();
    }
    var watch = setInterval(function () {
      if (!pre || !document.body.contains(pre)) { stopRunStream(); clearInterval(watch); }
    }, 800);
  }

  function cicdArtifacts(el) {
    var p = livePage(el, "制品", "构建产出的镜像标签（来自运行记录）", '<button class="x-btn" type="button" data-refresh>刷新</button>', cicdTabs("#/cicd/artifact"));
    el.querySelector("[data-refresh]").onclick = function () { cicdArtifacts(el); };
    function load() {
      var t = p.newToken();
      Promise.all([GET("/cicd/runs", { page: 1, page_size: 100 }), settle(appsLoad(), [])]).then(function (rs) {
        CICD_APPS = rs[1].value || [];
        var data = rs[0] || {}, items = Array.isArray(data) ? data : asArray(data, ["items"]);
        var rows = items.filter(function (r) { return r.image_tag; }).map(function (r) {
          return [code(r.image_tag, 34), E(appName(r.app_id)), mono("#" + r.id), code(r.commit || "—", 12), stTag(r.status), fmtMin(r.created_at)];
        });
        var html = panelHTML("镜像制品", banner("后端暂未提供独立制品仓库接口，此处按运行记录中的镜像标签聚合展示。") + (rows.length ? tbl(["镜像标签", "应用", "运行", "提交", "状态", "时间"], rows) : emptyHTML("暂无镜像制品记录")));
        p.paint(t, html);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    load();
  }

  function cicdPipelines(el) {
    var p = livePage(el, "流水线", "流水线定义、执行集群与 Jenkins 接入", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newpl>+ 新建流水线</button>', cicdTabs("#/cicd/pipeline"));
    el.querySelector("[data-refresh]").onclick = function () { cicdPipelines(el); };
    el.querySelector("[data-newpl]").onclick = function () { pipelineModal(function () { cicdPipelines(el); }); };
    function load() {
      var t = p.newToken();
      Promise.all([GET("/cicd/pipelines"), settle(GET("/cicd/targets"), []), settle(GET("/cicd/jenkins"), []), settle(appsLoad(), [])]).then(function (rs) {
        CICD_PL = Array.isArray(rs[0]) ? rs[0] : asArray(rs[0], ["items"]);
        CICD_TG = asArray(rs[1].value, ["items", "targets"]);
        CICD_JK = asArray(rs[2].value, ["items", "jenkins"]);
        CICD_APPS = rs[3].value || [];
        var plRows = CICD_PL.map(function (pl) {
          return [mono("#" + pl.id), E(pl.name || "—"), E(appName(pl.app_id)), E(targetName(pl.runner_target_id)), pl.on_push ? tag("代码推送", "x-blue") : tag("手动", "x-grey"),
            E((pl.stages || []).length + " 阶段"), fmtMin(pl.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-pld="' + E(pl.id) + '">详情</button><button class="x-btn" type="button" data-plrun="' + E(pl.id) + '">运行</button><button class="x-btn" type="button" data-pldel="' + E(pl.id) + '">删除</button></span>'];
        });
        var tgRows = CICD_TG.map(function (g) {
          return [E(g.name || "—"), code(g.namespace || "—", 20), code(g.kubeconfig || "********", 14), fmtMin(g.created_at),
            '<button class="x-btn" type="button" data-tgdel="' + E(g.id) + '">删除</button>'];
        });
        var jkRows = CICD_JK.map(function (j) {
          return [E(j.name || "—"), code(j.url || "—", 60), E(j.username || "—"), fmtMin(j.created_at), '<button class="x-btn" type="button" data-jkdel="' + E(j.id) + '">删除</button>'];
        });
        var html = panelHTML("流水线定义", (plRows.length ? tbl(["ID", "名称", "应用", "执行集群", "触发", "阶段", "更新时间", "操作"], plRows) : emptyHTML("暂无流水线。点击右上角「+ 新建流水线」。")),
          '<button class="x-btn" type="button" data-newpl2 style="margin-left:auto">+ 新建</button>') +
          panelHTML("执行集群（runner target）", (tgRows.length ? tbl(["名称", "命名空间", "Kubeconfig", "创建时间", "操作"], tgRows) : emptyHTML("暂无执行集群。流水线构建需要至少一个执行集群。")),
            '<button class="x-btn" type="button" data-newtg style="margin-left:auto">+ 接入集群</button>') +
          panelHTML("Jenkins 服务器", (jkRows.length ? tbl(["名称", "地址", "用户名", "创建时间", "操作"], jkRows) : emptyHTML("暂无 Jenkins 接入。流水线的 jenkins 阶段需要它。")),
            '<button class="x-btn" type="button" data-newjk style="margin-left:auto">+ 接入 Jenkins</button>');
        if (!p.paint(t, html)) return;
        bind();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function targetName(id) {
      for (var i = 0; i < CICD_TG.length; i++) if (String(CICD_TG[i].id) === String(id)) return CICD_TG[i].name;
      return "#" + id;
    }
    function bind() {
      var n2 = p.box.querySelector("[data-newpl2]"); if (n2) n2.onclick = function () { pipelineModal(function () { cicdPipelines(el); }); };
      var nt = p.box.querySelector("[data-newtg]"); if (nt) nt.onclick = function () { targetModal(function () { cicdPipelines(el); }); };
      var nj = p.box.querySelector("[data-newjk]"); if (nj) nj.onclick = function () { jenkinsModal(function () { cicdPipelines(el); }); };
      [].slice.call(p.box.querySelectorAll("[data-pld]")).forEach(function (b) {
        b.onclick = function () {
          var id = b.getAttribute("data-pld"), pl = null;
          for (var i = 0; i < CICD_PL.length; i++) if (String(CICD_PL[i].id) === String(id)) pl = CICD_PL[i];
          if (pl) infoModal("流水线 · " + (pl.name || id), preBlock(pl));
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-plrun]")).forEach(function (b) {
        b.onclick = function () {
          var id = b.getAttribute("data-plrun"), pl = null;
          for (var i = 0; i < CICD_PL.length; i++) if (String(CICD_PL[i].id) === String(id)) pl = CICD_PL[i];
          runPipelineModal(function () { cicdPipelines(el); }, pl ? [pl] : CICD_PL);
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-pldel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除流水线", "确定删除流水线 #" + E(b.getAttribute("data-pldel")) + "？", "删除", function () {
            write(DEL("/cicd/pipelines/" + encodeURIComponent(b.getAttribute("data-pldel"))), "流水线已删除").then(function (r) { if (r) load(); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-tgdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除执行集群", "确定删除执行集群？若被流水线引用会被拒绝。", "删除", function () {
            write(DEL("/cicd/targets/" + encodeURIComponent(b.getAttribute("data-tgdel"))), "执行集群已删除").then(function (r) { if (r) load(); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-jkdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除 Jenkins", "确定删除该 Jenkins 接入？", "删除", function () {
            write(DEL("/cicd/jenkins/" + encodeURIComponent(b.getAttribute("data-jkdel"))), "Jenkins 接入已删除").then(function (r) { if (r) load(); });
          });
        };
      });
    }
    function pipelineModal(done) {
      if (!CICD_APPS.length) { toast("请先在「应用」页创建应用"); return; }
      if (!CICD_TG.length) { toast("请先接入执行集群"); return; }
      formModal("新建流水线", [
        { k: "name", label: "名称（DNS label，≤40）", required: true, placeholder: "main" },
        { k: "app", label: "应用", kind: "select", options: CICD_APPS.map(function (a) { return { v: a.id, t: a.name }; }) },
        { k: "target", label: "执行集群", kind: "select", options: CICD_TG.map(function (g) { return { v: g.id, t: g.name + " / " + g.namespace }; }) },
        { k: "on_push", label: "代码推送自动触发", kind: "checkbox", value: true, hint: "开启" },
        { k: "stages", label: "阶段 JSON", kind: "textarea", rows: 8, required: true, value: '[\n  {"name": "unit-test", "type": "test", "image": "golang:1.23", "commands": ["go test ./..."]},\n  {"name": "build", "type": "build", "registry_secret": "harbor-push"}\n]' }
      ], "创建", function (v) {
        var stages;
        try { stages = parseJSON(v.stages, []); } catch (err) { toast(errMsg(err)); return; }
        write(POST("/cicd/pipelines", { name: v.name, app_id: +v.app, runner_target_id: +v.target, on_push: v.on_push, stages: stages }), "流水线已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, "阶段类型：test / script / build / deploy / jenkins；支持 depends_on 并行与 workspace（后端能力）。");
    }
    function targetModal(done) {
      formModal("接入执行集群", [
        { k: "name", label: "名称", required: true, placeholder: "ci-cluster" },
        { k: "namespace", label: "Job 命名空间", value: "matrix-cicd" },
        { k: "kubeconfig", label: "kubeconfig（服务端解析后加密存储）", kind: "textarea", rows: 8, required: true, placeholder: "apiVersion: v1\nkind: Config\n..." }
      ], "接入", function (v) {
        write(POST("/cicd/targets", { name: v.name, namespace: v.namespace, kubeconfig: v.kubeconfig }), "执行集群已接入").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    function jenkinsModal(done) {
      formModal("接入 Jenkins", [
        { k: "name", label: "名称", required: true, placeholder: "jk" },
        { k: "url", label: "Jenkins 地址", required: true, placeholder: "https://jenkins.example.com" },
        { k: "username", label: "用户名" },
        { k: "token", label: "API Token", kind: "password" }
      ], "接入", function (v) {
        write(POST("/cicd/jenkins", { name: v.name, url: v.url, username: v.username, token: v.token }), "Jenkins 已接入").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }

  /* ---------------------------------------------------------------- delivery 应用 */

  function renderDeliveryLive(el) {
    var seg = (location.hash || "").split("/")[2] || "";
    var name = seg && seg !== "apps" ? decodeURIComponent(seg) : "";
    if (name) return deliveryDetail(el, name);
    return deliveryList(el);
  }

  function deliveryList(el) {
    var p = livePage(el, "应用", "CI/CD 应用列表与环境配置", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newapp>+ 新建应用</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { deliveryList(el); };
    el.querySelector("[data-newapp]").onclick = function () { appModal(function () { deliveryList(el); }); };
    function load() {
      var t = p.newToken();
      appsLoad().then(function (list) {
        var rows = list.map(function (a) {
          var href = "#/delivery/" + encodeURIComponent(a.name);
          return ['<a class="x-tlink" href="' + href + '">' + E(a.name) + "</a>", code(a.repo_url, 46), code(a.branch || "—", 18), code(a.image_repo, 34),
            E((a.envs || []).map(function (e) { return e.name; }).join(", ") || "—"), fmtMin(a.updated_at || a.created_at),
            '<span class="ops-nowrap"><a class="x-btn" href="' + href + '">详情</a><button class="x-btn" type="button" data-appdel="' + E(a.id) + '">删除</button></span>'];
        });
        var html = panelHTML("应用", rows.length ? tbl(["名称", "代码仓库", "分支", "镜像仓库", "环境", "更新时间", "操作"], rows) : emptyHTML("尚未创建应用。点击右上角「+ 新建应用」接入代码仓库与镜像仓库。"));
        if (!p.paint(t, html)) return;
        [].slice.call(p.box.querySelectorAll("[data-appdel]")).forEach(function (b) {
          b.onclick = function () {
            confirmDo("删除应用", "确定删除应用？将级联删除其流水线、运行与发布记录。", "删除", function () {
              write(DEL("/cicd/applications/" + encodeURIComponent(b.getAttribute("data-appdel"))), "应用已删除").then(function (r) { if (r) load(); });
            });
          };
        });
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    load();
  }

  function appModal(done) {
    formModal("新建应用", [
      { k: "name", label: "名称（≤64）", required: true, placeholder: "web" },
      { k: "repo_url", label: "代码仓库地址", required: true, placeholder: "https://gitlab.example.com/team/web.git" },
      { k: "branch", label: "默认分支", value: "main" },
      { k: "dockerfile", label: "Dockerfile 路径", value: "Dockerfile" },
      { k: "context_dir", label: "构建上下文目录", value: "." },
      { k: "image_repo", label: "镜像仓库地址", required: true, placeholder: "harbor.example.com/team/web" },
      { k: "git_username", label: "Git 用户名" },
      { k: "git_token", label: "Git Token", kind: "password" },
      { k: "envs", label: "环境 JSON（name/target_id/namespace/deployment/container 或 helm）", kind: "textarea", rows: 5, required: true, value: '[\n  {"name": "test", "target_id": 1, "namespace": "web-test", "deployment": "web", "container": "app"}\n]' }
    ], "创建", function (v) {
      var envs;
      try { envs = parseJSON(v.envs, []); } catch (err) { toast(errMsg(err)); return; }
      write(POST("/cicd/applications", {
        name: v.name, repo_url: v.repo_url, branch: v.branch, dockerfile: v.dockerfile, context_dir: v.context_dir,
        image_repo: v.image_repo, git_username: v.git_username, git_token: v.git_token, envs: envs
      }), "应用已创建").then(function (r) {
        if (!r) return;
        closeAll();
        if (r.webhook_secret) infoModal("Webhook 密钥（仅显示一次）", '<p class="x-desc">请立即配置到 Git 仓库 Webhook：</p>' + preBlock((r.webhook_url || ("/api/v1/cicd/webhooks/" + (r.id || ""))) + "\nSecret: " + r.webhook_secret));
        if (done) done();
      });
    }, "每个环境要么 deployment+container，要么 helm；target_id 指向执行集群。");
  }

  function deliveryDetail(el, name) {
    var p = livePage(el, "应用 · " + name, "环境部署与发布记录", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { deliveryDetail(el, name); };
    var APP = null, RELS = [], RELENV = "";
    var back = '<p style="margin:0 0 12px"><a class="x-tlink" href="#/delivery/apps">← 返回应用列表</a></p>';
    function load() {
      var t = p.newToken();
      appsLoad().then(function (list) {
        APP = null;
        for (var i = 0; i < list.length; i++) if (list[i].name === name) APP = list[i];
        if (!APP) { p.paint(t, back + panelHTML("应用", emptyHTML("应用「" + name + "」不存在或已被删除。"))); return; }
        return loadReleases(null, t);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function loadReleases(env, t) {
      return GET("/cicd/applications/" + encodeURIComponent(APP.id) + "/releases", env ? { env: env } : undefined).then(function (data) {
        RELS = Array.isArray(data) ? data : asArray(data, ["items", "releases"]);
        if (!p.paint(t, bodyHTML())) return;
        bind();
      });
    }
    function bodyHTML() {
      var a = APP;
      var envRows = (a.envs || []).map(function (e) {
        var target = e.helm ? "Helm " + (e.helm.release || "") + " / " + (e.helm.chart || "") : "Deployment " + (e.deployment || "") + " / " + (e.container || "");
        return [E(e.name), E(target), E(e.namespace || "—"), E(e.target_id), '<span class="ops-nowrap"><button class="x-btn" type="button" data-deploy="' + E(e.name) + '">部署</button><button class="x-btn" type="button" data-rollback="' + E(e.name) + '">回滚</button></span>'];
      });
      var envSel = '<select class="x-select" data-relenv style="width:auto"><option value="">全部环境</option>' + (a.envs || []).map(function (e) { return '<option value="' + E(e.name) + '"' + (e.name === RELENV ? " selected" : "") + ">" + E(e.name) + "</option>"; }).join("") + "</select>";
      var rows = RELS.map(function (r) {
        return [mono("#" + r.id), E(r.env), tag(r.kind || "deploy", r.kind === "rollback" ? "x-orange" : "x-blue"), code(r.image, 40), code(r.previous_image, 30),
          stTag(r.status), E(r.created_by || "—"), fmtMin(r.created_at), fmtMin(r.finished_at),
          '<span class="ops-nowrap">' + (r.kind === "rollback" || !r.image ? "" : '<button class="x-btn" type="button" data-relrb="' + E(r.id) + '" data-relenv2="' + E(r.env) + '">回滚到此</button>') + "</span>"];
      });
      return '<p style="margin:0 0 12px"><a class="x-tlink" href="#/delivery/apps">← 返回应用列表</a>' +
        '<span style="float:right" class="ops-nowrap"><button class="x-btn" type="button" data-deploy2>部署</button><button class="x-btn" type="button" data-rotate>轮换 Webhook 密钥</button></span></p>' +
        '<dl class="x-sum">' + [["ID", mono(a.id)], ["代码仓库", code(a.repo_url, 70)], ["分支", E(a.branch || "—")], ["Dockerfile", code(a.dockerfile || "Dockerfile", 40)],
          ["上下文目录", code(a.context_dir || ".", 40)], ["镜像仓库", code(a.image_repo, 60)], ["创建", fmtMin(a.created_at)]]
          .map(function (kv) { return "<dt>" + kv[0] + "</dt><dd>" + kv[1] + "</dd>"; }).join("") + "</dl>" +
        panelHTML("环境", envRows.length ? tbl(["环境", "部署对象", "命名空间", "执行集群", "操作"], envRows) : emptyHTML("该应用未配置环境")) +
        panelHTML("发布记录", barHTML([envSel, '<span class="x-frac" style="margin-left:auto">共 ' + RELS.length + " 条</span>"]) + (RELS.length ? tbl(["ID", "环境", "类型", "镜像", "上一镜像", "状态", "操作人", "创建", "完成", "操作"], rows) : emptyHTML("暂无发布记录")));
    }
    function bind() {
      var envSel = p.box.querySelector("[data-relenv]");
      if (envSel) envSel.onchange = function () { RELENV = envSel.value; var t = p.newToken(); loadReleases(RELENV || null, t).catch(function (err) { p.error(t, err); }); };
      var d2 = p.box.querySelector("[data-deploy2]");
      if (d2) d2.onclick = function () { deployModal(APP, "", function () { deliveryDetail(el, name); }); };
      [].slice.call(p.box.querySelectorAll("[data-deploy]")).forEach(function (b) {
        b.onclick = function () { deployModal(APP, b.getAttribute("data-deploy"), function () { deliveryDetail(el, name); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-rollback]")).forEach(function (b) {
        b.onclick = function () {
          var env = b.getAttribute("data-rollback");
          confirmDo("回滚", "确认回滚「" + E(APP.name) + "」环境 <b>" + E(env) + "</b> 到最近一次成功发布前的镜像？" + (isProd(env) ? "<br />生产环境可能需要审批。" : ""), "回滚", function () {
            write(POST("/cicd/applications/" + encodeURIComponent(APP.id) + "/rollback", { env: env }), "回滚请求已提交").then(function (r) { if (r) deliveryDetail(el, name); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-relrb]")).forEach(function (b) {
        b.onclick = function () {
          var env = b.getAttribute("data-relenv2"), rel = b.getAttribute("data-relrb");
          confirmDo("回滚到指定发布", "确认将环境 <b>" + E(env) + "</b> 重新部署发布 #" + E(rel) + " 的镜像？", "回滚", function () {
            write(POST("/cicd/applications/" + encodeURIComponent(APP.id) + "/rollback", { env: env, release_id: +rel }), "回滚请求已提交").then(function (r) { if (r) deliveryDetail(el, name); });
          });
        };
      });
      var rot = p.box.querySelector("[data-rotate]");
      if (rot) rot.onclick = function () {
        confirmDo("轮换 Webhook 密钥", "轮换后旧密钥立即失效，需要同步更新 Git 仓库 Webhook。", "轮换", function () {
          POST("/cicd/applications/" + encodeURIComponent(APP.id) + "/webhook-secret").then(function (r) {
            infoModal("新的 Webhook 密钥（仅显示一次）", preBlock((r && (r.url || r.webhook_url) ? (r.url || r.webhook_url) + "\n" : "") + "Secret: " + ((r && (r.webhook_secret || r.secret)) || "")));
          }).catch(function (err) { toast(errMsg(err)); });
        });
      };
    }
    function deployModal(app, env, done) {
      var envs = (app.envs || []).map(function (e) { return e.name; });
      if (!envs.length) { toast("该应用未配置环境"); return; }
      formModal("部署 " + app.name, [
        { k: "env", label: "环境", kind: "select", options: envs, value: env || envs[0] },
        { k: "image", label: "镜像", required: true, placeholder: app.image_repo + ":v1.2.0" },
        { k: "reason", label: "变更原因", placeholder: "hotfix / 需求发布" }
      ], "部署", function (v) {
        write(POST("/cicd/applications/" + encodeURIComponent(app.id) + "/deploy", { env: v.env, image: v.image, reason: v.reason }), "部署请求已提交").then(function (r) {
          if (!r) return;
          closeAll();
          if (r.__approval) infoModal("已提交审批", "<p class=\"x-desc\">生产环境部署需要审批，审批通过后自动执行。</p>");
          if (done) done();
        });
      }, isProd(env) ? "生产环境可能需要审批（提交后显示“已提交审批”）。" : "");
    }
    load();
  }
  function isProd(env) { return /prod|生产/i.test(String(env || "")); }

  /* ---------------------------------------------------------------- release 发布单 */

  var REL = { appId: 0, env: "" };
  function renderReleaseLive(el) {
    var p = livePage(el, "发布单", "应用发布与回滚记录（按应用聚合）", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { renderReleaseLive(el); };
    var APP = null, RELS = [];
    function load() {
      var t = p.newToken();
      appsLoad().then(function (apps) {
        if (!apps.length) { p.paint(t, panelHTML("发布单", emptyHTML("暂无应用，请先在「应用」页创建应用。"))); return; }
        if (!apps.some(function (a) { return String(a.id) === String(REL.appId); })) { REL.appId = apps[0].id; REL.env = ""; }
        APP = null;
        for (var i = 0; i < apps.length; i++) if (String(apps[i].id) === String(REL.appId)) APP = apps[i];
        return fetchRels(t, apps);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function fetchRels(t, apps) {
      return GET("/cicd/applications/" + encodeURIComponent(APP.id) + "/releases", REL.env ? { env: REL.env } : undefined).then(function (data) {
        RELS = Array.isArray(data) ? data : asArray(data, ["items", "releases"]);
        var appSel = '<select class="x-select" data-relapp style="width:auto">' + apps.map(function (a) { return '<option value="' + E(a.id) + '"' + (String(a.id) === String(APP.id) ? " selected" : "") + ">" + E(a.name) + "</option>"; }).join("") + "</select>";
        var envSel = '<select class="x-select" data-relenvpick style="width:auto"><option value="">全部环境</option>' + (APP.envs || []).map(function (e) { return '<option value="' + E(e.name) + '"' + (e.name === REL.env ? " selected" : "") + ">" + E(e.name) + "</option>"; }).join("") + "</select>";
        var rows = RELS.map(function (r) {
          return [mono("#" + r.id), E(r.env), tag(r.kind || "deploy", r.kind === "rollback" ? "x-orange" : "x-blue"), code(r.image, 42), code(r.previous_image, 30),
            stTag(r.status), E(r.created_by || "—"), fmtMin(r.created_at), fmtMin(r.finished_at),
            '<button class="x-btn" type="button" data-rrb="' + E(r.id) + '" data-rrb2="' + E(r.env) + '">回滚到此</button>'];
        });
        var html = banner("发布单页按应用聚合展示后端发布记录（POST /cicd/applications/:id/deploy|rollback）；后端暂未提供发布单聚合实体。") +
          panelHTML("发布记录", barHTML([appSel, envSel, '<span class="x-frac" style="margin-left:auto">共 ' + RELS.length + " 条</span>"]) + (RELS.length ? tbl(["ID", "环境", "类型", "镜像", "上一镜像", "状态", "操作人", "创建", "完成", "操作"], rows) : emptyHTML("该应用暂无发布记录")));
        if (!p.paint(t, html)) return;
        bind(apps);
      });
    }
    function bind(apps) {
      var as = p.box.querySelector("[data-relapp]"), es = p.box.querySelector("[data-relenvpick]");
      if (as) as.onchange = function () { REL.appId = +as.value; REL.env = ""; renderReleaseLive(el); };
      if (es) es.onchange = function () { REL.env = es.value; renderReleaseLive(el); };
      [].slice.call(p.box.querySelectorAll("[data-rrb]")).forEach(function (b) {
        b.onclick = function () {
          var env = b.getAttribute("data-rrb2"), rel = b.getAttribute("data-rrb");
          confirmDo("回滚到指定发布", "确认将应用 <b>" + E(APP.name) + "</b> 环境 <b>" + E(env) + "</b> 重新部署发布 #" + E(rel) + " 的镜像？", "回滚", function () {
            write(POST("/cicd/applications/" + encodeURIComponent(APP.id) + "/rollback", { env: env, release_id: +rel }), "回滚请求已提交").then(function (r) { if (r) renderReleaseLive(el); });
          });
        };
      });
    }
    load();
  }


  /* ---------------------------------------------------------------- middleware 中间件 */

  var MW_TYPES = [
    { v: "kafka", t: "Kafka" }, { v: "rabbitmq", t: "RabbitMQ" }, { v: "rocketmq", t: "RocketMQ" },
    { v: "mysql", t: "MySQL" }, { v: "postgres", t: "PostgreSQL" }, { v: "redis", t: "Redis" }
  ];
  function mwTypeLabel(t) { for (var i = 0; i < MW_TYPES.length; i++) if (MW_TYPES[i].v === t) return MW_TYPES[i].t; return t; }
  function mwIsMQ(t) { return t === "kafka" || t === "rabbitmq" || t === "rocketmq"; }
  function mwIsDB(t) { return t === "mysql" || t === "postgres"; }

  function renderMiddlewareLive(el) {
    var parts = (location.hash || "").split("/");
    var tab = parts[2] || "";
    if (!MW_TYPES.some(function (x) { return x.v === tab; })) tab = "";
    var deepId = parts[3] ? decodeURIComponent(parts[3]) : "";
    var p = livePage(el, "中间件", "Kafka / RabbitMQ / RocketMQ / MySQL / PostgreSQL / Redis 实例", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newmw>+ 新建实例</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { renderMiddlewareLive(el); };
    el.querySelector("[data-newmw]").onclick = function () { mwModal(function () { renderMiddlewareLive(el); }); };
    function load() {
      var t = p.newToken();
      var qy = { page: 1, page_size: 100 };
      if (tab) qy.type = tab;
      GET("/middleware/instances", qy).then(function (data) {
        var items = Array.isArray(data) ? data : asArray(data, ["items"]);
        var tabs = '<div class="x-bar"><div class="x-tabs" role="tablist">' +
          '<button type="button" role="tab" class="' + (tab ? "" : "on") + '" data-mwtab="">全部</button>' +
          MW_TYPES.map(function (x) { return '<button type="button" role="tab" class="' + (tab === x.v ? "on" : "") + '" data-mwtab="' + x.v + '">' + x.t + "</button>"; }).join("") + "</div></div>";
        var rows = items.map(function (it) {
          var types = it.type, isMQ = mwIsMQ(types), isDB = mwIsDB(types), isRedis = types === "redis";
          return [E(it.name || "—"), tag(mwTypeLabel(types), "x-blue"), E(it.env || "*"), code((it.endpoints || []).join(", "), 60),
            E((it.auth && it.auth.username) || "—"), E((it.options && (it.options.database || it.options.master_name || it.options.db)) || "—"),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-mwtest="' + E(it.id) + '">测试</button>' +
            (isMQ ? '<button class="x-btn" type="button" data-mwov="' + E(it.id) + '">概览</button>' : "") +
            (isDB || isRedis ? '<button class="x-btn" type="button" data-mwcon="' + E(it.id) + '">控制台</button>' : "") +
            '<button class="x-btn" type="button" data-mwdet="' + E(it.id) + '">详情</button>' +
            '<button class="x-btn" type="button" data-mwdel="' + E(it.id) + '">删除</button></span>'];
        });
        var html = panelHTML("实例", tabs + (items.length ? tbl(["名称", "类型", "环境", "地址", "用户", "库/主库", "操作"], rows) : emptyHTML("暂无中间件实例。点击右上角「+ 新建实例」接入 Kafka / RabbitMQ / RocketMQ / MySQL / PostgreSQL / Redis。")));
        if (!p.paint(t, html)) return;
        bind(items);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function byId(items, id) { for (var i = 0; i < items.length; i++) if (String(items[i].id) === String(id)) return items[i]; return null; }
    function bind(items) {
      [].slice.call(p.box.querySelectorAll("[data-mwtab]")).forEach(function (b) {
        b.onclick = function () { var v = b.getAttribute("data-mwtab"); try { window.navGo(v ? "#/middleware/" + v : "#/middleware"); } catch (e) { location.hash = v ? "#/middleware/" + v : "#/middleware"; } };
      });
      [].slice.call(p.box.querySelectorAll("[data-mwtest]")).forEach(function (b) {
        b.onclick = function () {
          b.disabled = true;
          POST("/middleware/instances/" + encodeURIComponent(b.getAttribute("data-mwtest")) + "/test").then(function (r) {
            b.disabled = false;
            if (r && r.ok) toast("连接成功" + (r.version ? "：" + r.version : "") + (r.latency_ms != null ? "（" + r.latency_ms + " ms）" : ""));
            else toast("连接失败：" + ((r && r.message) || "上游不可用"));
          }).catch(function (err) { b.disabled = false; toast(errMsg(err)); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-mwov]")).forEach(function (b) {
        b.onclick = function () { mqOverviewModal(b.getAttribute("data-mwov")); };
      });
      [].slice.call(p.box.querySelectorAll("[data-mwcon]")).forEach(function (b) {
        b.onclick = function () { var it = byId(items, b.getAttribute("data-mwcon")); if (it) consoleModal(it); };
      });
      [].slice.call(p.box.querySelectorAll("[data-mwdet]")).forEach(function (b) {
        b.onclick = function () { var it = byId(items, b.getAttribute("data-mwdet")); if (it) infoModal("实例详情 · " + (it.name || it.id), preBlock(it)); };
      });
      [].slice.call(p.box.querySelectorAll("[data-mwdel]")).forEach(function (b) {
        b.onclick = function () {
          var it = byId(items, b.getAttribute("data-mwdel"));
          confirmDo("删除实例", "确定删除中间件实例 <b>" + E(it ? it.name : b.getAttribute("data-mwdel")) + "</b>？", "删除", function () {
            write(DEL("/middleware/instances/" + encodeURIComponent(b.getAttribute("data-mwdel"))), "实例已删除").then(function (r) { if (r) renderMiddlewareLive(el); });
          });
        };
      });
      if (deepId) {
        var deep = byId(items, deepId);
        if (deep) consoleModal(deep);
        history.replaceState(null, "", "#/middleware" + (tab ? "/" + tab : ""));
      }
    }
    function mqOverviewModal(id) {
      GET("/middleware/instances/" + encodeURIComponent(id) + "/overview").then(function (data) {
        var summary = autoSummary(data);
        var sum = summary.length ? '<dl class="x-sum">' + summary.map(function (kv) { return "<dt>" + E(kv[0]) + "</dt><dd>" + E(kv[1]) + "</dd>"; }).join("") + "</dl>" : "";
        infoModal("MQ 概览", sum + preBlock(data));
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function mwModal(done) {
      formModal("新建中间件实例", [
        { k: "name", label: "名称", required: true, placeholder: "kafka-prod-01" },
        { k: "type", label: "类型", kind: "select", required: true, options: MW_TYPES.map(function (x) { return { v: x.v, t: x.t }; }) },
        { k: "env", label: "环境", placeholder: "prod / test / dev" },
        { k: "endpoints", label: "地址（每行一个；Kafka bootstrap / RabbitMQ 管理地址 / RocketMQ dashboard / DB host:port / Redis）", kind: "textarea", rows: 3, required: true, placeholder: "10.0.0.1:9092" },
        { k: "username", label: "用户名" },
        { k: "password", label: "密码", kind: "password" },
        { k: "tls", label: "启用 TLS", kind: "checkbox", value: false, hint: "TLS" },
        { k: "tls_skip_verify", label: "跳过 TLS 校验", kind: "checkbox", value: false, hint: "跳过校验" },
        { k: "options", label: "options JSON（DB: database；Redis: db/master_name；Postgres: sslmode）", kind: "textarea", rows: 2, value: "{}" },
        { k: "description", label: "描述", kind: "textarea", rows: 2 }
      ], "创建", function (v) {
        var endpoints = String(v.endpoints).split(/[\n,]+/).map(function (s) { return s.trim(); }).filter(Boolean);
        var opts;
        try { opts = parseJSON(v.options || "{}", {}); } catch (err) { toast(errMsg(err)); return; }
        var body = {
          name: v.name, type: v.type, env: v.env, endpoints: endpoints, description: v.description,
          auth: { username: v.username, password: v.password, tls: v.tls, tls_skip_verify: v.tls_skip_verify }, options: opts
        };
        write(POST("/middleware/instances", body), "实例已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, "密码会加密存储；接口只返回掩码。");
    }
    load();
  }

  function autoSummary(o) {
    var out = [];
    if (!o || typeof o !== "object") return out;
    Object.keys(o).forEach(function (k) {
      var v = o[k];
      if (v == null) return;
      if (Array.isArray(v)) out.push([k, v.length + " 项"]);
      else if (typeof v === "object") out.push([k, "对象"]);
      else out.push([k, String(v)]);
    });
    return out.slice(0, 14);
  }

  function sqlRisk(sql) {
    var s = String(sql || "").replace(/^[\s;]+/, "").replace(/^(--[^\n]*\n|\/\*[\s\S]*?\*\/|\s)+/g, "").trim();
    var kw = (s.match(/^[a-zA-Z]+/) || [""])[0].toUpperCase();
    var danger = { DROP: "DROP 删除对象", TRUNCATE: "TRUNCATE 清空表", ALTER: "ALTER 变更结构", RENAME: "RENAME 重命名对象", GRANT: "GRANT 授权变更", REVOKE: "REVOKE 回收权限", SET: "SET 会话/全局变量变更" };
    var writeKW = { INSERT: 1, UPDATE: 1, DELETE: 1, CREATE: 1, REPLACE: 1, MERGE: 1, CALL: 1, COPY: 1, VACUUM: 1, REINDEX: 1 };
    var readKW = { SELECT: 1, SHOW: 1, EXPLAIN: 1, DESC: 1, DESCRIBE: 1, WITH: 1, ANALYZE: 1 };
    if (danger[kw]) return { level: "dangerous", label: "高危", reason: danger[kw], kw: kw };
    if (writeKW[kw]) return { level: "write", label: "写操作", reason: kw + " 会修改数据", kw: kw };
    if (readKW[kw]) return { level: "read", label: "只读", reason: "只读查询", kw: kw };
    return { level: "unknown", label: "未知", reason: "未识别的语句，默认按写操作处理", kw: kw || "?" };
  }
  function redisRisk(cmd) {
    var args = String(cmd || "").trim().split(/\s+/).filter(Boolean);
    var c = (args[0] || "").toUpperCase();
    var danger = { FLUSHALL: 1, FLUSHDB: 1, SHUTDOWN: 1, DEBUG: 1, CONFIG: 1, CLUSTER: 1, SCRIPT: 1, SLAVEOF: 1, REPLICAOF: 1, MODULE: 1, KEYS: 1 };
    var writeKW = { SET: 1, DEL: 1, EXPIRE: 1, INCR: 1, DECR: 1, HSET: 1, HDEL: 1, LPUSH: 1, RPUSH: 1, LPOP: 1, RPOP: 1, SADD: 1, SREM: 1, ZADD: 1, ZREM: 1, RENAME: 1, MOVE: 1, RESTORE: 1, EVAL: 1, EVALSHA: 1, MSET: 1, SETEX: 1 };
    if (danger[c]) return { level: "dangerous", label: "高危", reason: "危险命令：" + c };
    if (writeKW[c]) return { level: "write", label: "写操作", reason: "会修改数据：" + c };
    return { level: "read", label: "只读", reason: "只读或未分类命令" };
  }
  function riskTag(r) {
    if (!r) return "";
    var cls = r.level === "dangerous" ? "x-red" : r.level === "write" ? "x-orange" : "x-green";
    return tag(r.label + (r.reason ? " · " + r.reason : ""), cls);
  }
  function cellHTML(v) {
    if (v == null) return '<span class="x-frac">—</span>';
    if (typeof v === "object") return code(JSON.stringify(v), 120);
    return E(v);
  }

  function consoleModal(it) {
    if (it.type === "redis") return redisConsoleModal(it);
    return sqlConsoleModal(it);
  }

  function sqlConsoleModal(it) {
    var prod = isProd(it.env);
    dlg("SQL 控制台 · " + E(it.name),
      '<div class="x-form"><p class="x-desc" style="margin:0">环境 <b>' + E(it.env || "*") + "</b> · 类型 " + E(it.type) + (prod ? ' · <span class="x-tag x-red x-tag--s">生产</span>' : "") + "</p>" +
      inputHTML("opsdb", "数据库（可选）", (it.options && it.options.database) || "", "默认库") +
      '<label class="x-full"><span>SQL</span><textarea class="x-input" id="opssql" rows="6" placeholder="SELECT * FROM users LIMIT 10" aria-label="SQL 语句"></textarea></label>' +
      inputHTML("opstmo", "超时（秒）", "30", "") +
      '<div id="opsrisk" aria-live="polite" style="min-height:22px"></div>' +
      '<p class="x-err" id="opssqlerr" role="alert" hidden></p><div id="opsres"></div></div>',
      '<button class="x-btn" type="button" data-close>关闭</button><button class="x-btn x-btn--pri" type="button" id="opsexec">执行</button>');
    var riskBox = q("opsrisk"), ta = q("opssql"), errBox = q("opssqlerr");
    if (ta) {
      ta.focus();
      ta.oninput = function () { riskBox.innerHTML = ta.value.trim() ? riskTag(sqlRisk(ta.value)) : ""; };
      ta.onkeydown = function (e) { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); q("opsexec").click(); } };
    }
    q("opsexec").onclick = function () { submitSQL(it, false); };
    function submitSQL(inst, confirmed) {
      var sql = ta.value.trim(), err = q("opssqlerr"), res = q("opsres");
      if (!sql) { err.textContent = "请输入 SQL"; err.hidden = false; return; }
      var body = { sql: sql, database: (q("opsdb").value || "").trim(), timeout_sec: +q("opstmo").value || 30, confirm: !!confirmed };
      err.hidden = true;
      res.innerHTML = loadingHTML("执行中…");
      POST("/middleware/instances/" + encodeURIComponent(inst.id) + "/sql", body).then(function (r) {
        res.innerHTML = sqlResultHTML(r);
      }).catch(function (e2) {
        res.innerHTML = "";
        if (isApproval(e2)) {
          res.innerHTML = '<p class="x-test ok" role="status">已提交审批。生产环境高危 SQL 审批通过后由服务端以申请人身份执行。</p>';
          return;
        }
        var msg = errMsg(e2), detail = detailOf(e2);
        if (!confirmed && /confirm|确认/i.test(msg)) {
          confirmDo("高危 SQL 确认", E(msg) + (detail ? "<br />" + preBlock(detail) : "") + "<br />确认后将以 confirm=true 重新提交。", "确认执行", function () { submitSQL(inst, true); });
          return;
        }
        err.textContent = msg;
        err.hidden = false;
        if (detail) res.innerHTML = preBlock(detail);
      });
    }
    function sqlResultHTML(r) {
      if (!r) return "";
      var meta = barHTML([riskTag(r.risk), '<span class="x-frac">耗时 ' + num(r.duration_ms) + " ms</span>",
        r.rows_affected != null ? '<span class="x-frac">影响行数 ' + num(r.rows_affected) + "</span>" : "",
        r.audit_id != null ? '<span class="x-frac">审计 #' + E(r.audit_id) + "</span>" : ""]);
      if (Array.isArray(r.columns) && r.columns.length) {
        var rows = (r.rows || []).map(function (row) { return (Array.isArray(row) ? row : [row]).map(cellHTML); });
        return meta + tbl(r.columns.map(function (c) { return E(c); }), rows) + (r.truncated ? '<p class="ops-sub">结果已截断</p>' : "");
      }
      return meta + '<p class="x-test ok" style="margin:12px 0 0">执行成功' + (r.row_count != null ? "，返回 " + num(r.row_count) + " 行" : "") + (r.rows_affected != null ? "，影响 " + num(r.rows_affected) + " 行" : "") + "</p>";
    }
  }

  function redisConsoleModal(it) {
    dlg("Redis 控制台 · " + E(it.name),
      '<div class="x-form"><p class="x-desc" style="margin:0">环境 <b>' + E(it.env || "*") + "</b> · 类型 Redis" + (isProd(it.env) ? ' · <span class="x-tag x-red x-tag--s">生产</span>' : "") + "</p>" +
      '<label class="x-full"><span>命令</span><textarea class="x-input" id="opsredis" rows="3" placeholder="HGET user:1 name" aria-label="Redis 命令"></textarea></label>' +
      inputHTML("opsrdb", "DB（可选，留空使用实例配置）", "", "") +
      '<div id="opsrisk" aria-live="polite" style="min-height:22px"></div>' +
      '<p class="x-err" id="opsrediserr" role="alert" hidden></p><div id="opsres"></div></div>',
      '<button class="x-btn" type="button" data-close>关闭</button><button class="x-btn x-btn--pri" type="button" id="opsredexec">执行</button>');
    var ta = q("opsredis"), riskBox = q("opsrisk"), errBox = q("opsrediserr");
    if (ta) {
      ta.focus();
      ta.oninput = function () { riskBox.innerHTML = ta.value.trim() ? riskTag(redisRisk(ta.value)) : ""; };
      ta.onkeydown = function (e) { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); q("opsredexec").click(); } };
    }
    q("opsredexec").onclick = function () {
      var cmd = ta.value.trim(), err = q("opsrediserr"), res = q("opsres");
      if (!cmd) { err.textContent = "请输入 Redis 命令"; err.hidden = false; return; }
      var rk = redisRisk(cmd);
      if (rk.level === "dangerous") {
        confirmDo("高危 Redis 命令", "命令 <b>" + E(cmd) + "</b> 被判定为高危（" + E(rk.reason) + "），确认执行？", "确认执行", function () { doExec(true); });
      } else doExec(false);
      function doExec() {
        err.hidden = true;
        res.innerHTML = loadingHTML("执行中…");
        var body = { command: cmd };
        var dbv = (q("opsrdb").value || "").trim();
        if (dbv !== "") body.db = +dbv;
        POST("/middleware/instances/" + encodeURIComponent(it.id) + "/redis", body).then(function (r) {
          res.innerHTML = barHTML([riskTag(r.risk), '<span class="x-frac">耗时 ' + num(r.duration_ms) + " ms</span>", r.audit_id != null ? '<span class="x-frac">审计 #' + E(r.audit_id) + "</span>" : ""]) +
            (typeof r.result === "object" && r.result !== null ? preBlock(r.result) : '<p class="x-test ok" style="margin:12px 0 0">' + E(String(r.result == null ? "(nil)" : r.result)) + "</p>");
        }).catch(function (e2) {
          res.innerHTML = "";
          err.textContent = errMsg(e2);
          err.hidden = false;
        });
      }
    };
  }

  /* ---------------------------------------------------------------- settings 设置 */

  // 平台访问域名：保存后由后端自动创建/更新 Ingress（见 server/internal/platform）
  function domainPanel(d) {
    var hasIngress = !!d.ingress_available, cls = d.ingress_class || "";
    var state = !hasIngress
      ? '<span class="x-tag x-orange">未检测到 Ingress</span> 域名不会生效，请用 IP:NodePort 访问'
      : '<span class="x-tag x-green">Ingress 可用' + (cls ? "（" + E(cls) + "）" : "") + "</span>";
    return '<div class="x-panel" style="margin-bottom:16px">' +
      '<div class="x-tbar" style="margin-bottom:8px"><h2 class="x-h2" style="margin:0">访问域名</h2>' +
      '<span class="x-frac">证书：自签 · CN=xingshu matrix · 有效期 10 年</span></div>' +
      '<div class="x-tbar" style="margin:0">' +
      '<input class="x-input" data-dom value="' + E(d.domain || "") + '" placeholder="如 xingshu-matrix.com" aria-label="平台访问域名" style="max-width:320px" />' +
      '<button class="x-btn x-btn--pri" type="button" data-domsave>保存并应用</button>' +
      '<button class="x-btn" type="button" data-domclear>清除域名</button>' +
      '<span class="x-frac">' + state + "</span></div>" +
      '<p class="x-frac" style="margin:10px 0 0;line-height:1.7">' +
      '· 域名需要集群内存在 Ingress 控制器；保存时平台会自动创建/更新 Ingress（host=该域名，TLS 复用 <code class="x-mono">' + E(d.tls_secret || "xingshu-matrix-tls") + '</code>）。<br />' +
      '· 集群没有 Ingress 时保存依旧成功，但域名不生效，请使用 <code class="x-mono">http(s)://节点IP:' + E(d.nodeport || "30080") + '</code> 访问。<br />' +
      "· 清空并保存会删除平台自己的 Ingress，回退到 NodePort。" +
      '</p></div>';
  }
  function renderSettingsLive(el) {
    var p = livePage(el, "设置", "平台设置、公开配置与环境变更策略", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newset>+ 新建设置</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { renderSettingsLive(el); };
    el.querySelector("[data-newset]").onclick = function () { settingModal(null, function () { renderSettingsLive(el); }); };
    var SET = [], PUB = {}, POL = [];
    function load() {
      var t = p.newToken();
      Promise.all([settle(GET("/settings"), []), settle(GET("/settings/public"), {}), settle(GET("/env-policies"), []), settle(GET("/platform/domain"), {})]).then(function (rs) {
        SET = rs[0].ok ? asArray(rs[0].value, ["items", "settings"]) : [];
        PUB = rs[1].ok && rs[1].value && typeof rs[1].value === "object" ? rs[1].value : {};
        POL = rs[2].ok ? asArray(rs[2].value, ["items", "policies"]) : [];
        var DOM = rs[3] && rs[3].ok && rs[3].value ? rs[3].value : {};
        var failed = [];
        if (!rs[0].ok) failed.push("平台设置：" + errMsg(rs[0].error));
        if (!rs[1].ok) failed.push("公开设置：" + errMsg(rs[1].error));
        if (!rs[2].ok) failed.push("环境策略：" + errMsg(rs[2].error));
        var setRows = SET.map(function (s) {
          return [code(s.key, 42), settingVal(s), E(s.description || "—"), s.secret ? tag("加密", "x-orange") : E("—"), fmtMin(s.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-setedit="' + E(s.key) + '">编辑</button> <button class="x-btn" type="button" data-setdel="' + E(s.key) + '">删除</button></span>'];
        });
        var pubKeys = Object.keys(PUB);
        var pubRows = pubKeys.map(function (k) { return [code(k, 42), code(JSON.stringify(PUB[k]), 120)]; });
        var polRows = POL.map(function (x) {
          return [E(x.env), E(x.name || "—"), x.require_approval ? tag("需要审批", "x-orange") : tag("直接执行", "x-green"),
            E(x.approver_permission || "—"), fmtMin(x.updated_at), '<button class="x-btn" type="button" data-poledit="' + E(x.env) + '">编辑</button>'];
        });
        var html = domainPanel(DOM) + (failed.length ? banner("部分设置区块加载失败（其余区块仍可用）：" + failed.join("；")) : "") +
          panelHTML("平台设置", setRows.length ? tbl(["键", "值", "说明", "加密", "更新时间", "操作"], setRows) : emptyHTML("暂无设置项。点击右上角「+ 新建设置」。")) +
          panelHTML("公开设置（/settings/public）", pubRows.length ? tbl(["键", "值"], pubRows) : emptyHTML("暂无 public.* 公开配置")) +
          panelHTML("环境变更策略（/env-policies）", polRows.length ? tbl(["环境", "名称", "变更审批", "审批权限", "更新时间", "操作"], polRows) : emptyHTML("暂无环境策略。可编辑添加 prod / test / dev 的审批要求。"));
        if (!p.paint(t, html)) return;
        bind();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function settingVal(s) {
      if (s.secret) return s.has_value ? code("••••••••（已设置）", 30) : '<span class="x-frac">未设置</span>';
      var v = s.value;
      return code(typeof v === "string" ? v : JSON.stringify(v), 100);
    }
    function bind() {
      var domInput = p.box.querySelector("[data-dom]");
      var save = p.box.querySelector("[data-domsave]");
      if (save) save.onclick = function () {
        var v = (domInput && domInput.value || "").trim();
        save.disabled = true;
        PUT("/platform/domain", { domain: v }).then(function (st) {
          var msg = st && st.message ? st.message : "已保存";
          toast(v ? msg : "已清除域名，回退到 NodePort 访问");
          renderSettingsLive(el);
        }).catch(function (err) {
          save.disabled = false;
          toast("保存失败：" + errMsg(err));
        });
      };
      var clear = p.box.querySelector("[data-domclear]");
      if (clear) clear.onclick = function () {
        if (domInput) domInput.value = "";
        if (save) save.click();
      };
      [].slice.call(p.box.querySelectorAll("[data-setedit]")).forEach(function (b) {
        b.onclick = function () {
          var key = b.getAttribute("data-setedit"), st = null;
          for (var i = 0; i < SET.length; i++) if (SET[i].key === key) st = SET[i];
          settingModal(st, function () { renderSettingsLive(el); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-setdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除设置", "确定删除设置项 <b>" + E(b.getAttribute("data-setdel")) + "</b>？", "删除", function () {
            write(DEL("/settings/" + encodeURIComponent(b.getAttribute("data-setdel"))), "设置已删除").then(function (r) { if (r) renderSettingsLive(el); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-poledit]")).forEach(function (b) {
        b.onclick = function () {
          var env = b.getAttribute("data-poledit"), cur = null;
          for (var i = 0; i < POL.length; i++) if (POL[i].env === env) cur = POL[i];
          envPolicyModal(env, cur, function () { renderSettingsLive(el); });
        };
      });
    }
    function settingModal(st, done) {
      var isEdit = !!st;
      var valText = st ? (st.secret ? "" : JSON.stringify(st.value)) : '""';
      formModal(isEdit ? "编辑设置 · " + st.key : "新建设置", [
        { k: "key", label: "键（小写点分名称，如 notify.webhook）", required: true, value: st ? st.key : "", placeholder: "notify.webhook" },
        { k: "value", label: "值（JSON，例如 \"https://...\" 或 {\"k\":1}）", kind: "textarea", rows: 4, required: true, value: valText },
        { k: "secret", label: "加密存储", kind: "checkbox", value: !!(st && st.secret), hint: "加密" },
        { k: "description", label: "说明", value: st ? st.description || "" : "" }
      ], isEdit ? "保存" : "创建", function (v) {
        var value;
        try { value = parseJSON(v.value, ""); } catch (err) { toast(errMsg(err)); return; }
        write(PUT("/settings/" + encodeURIComponent(v.key), { value: value, secret: v.secret, description: v.description }), "设置已保存").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, isEdit ? "secret 类型编辑时不会回显原值；留空将保存 JSON 空字符串。" : "值必须是合法 JSON；secret=true 时整体加密存储并在读取时掩码。");
    }
    function envPolicyModal(env, cur, done) {
      formModal("环境策略 · " + env, [
        { k: "name", label: "环境名称", value: cur ? cur.name || env : env },
        { k: "require_approval", label: "变更需要审批", kind: "checkbox", value: cur ? !!cur.require_approval : isProd(env), hint: "需要审批" },
        { k: "approver_permission", label: "审批权限码", value: cur ? cur.approver_permission || "" : "approval:approve", placeholder: "approval:approve" }
      ], "保存", function (v) {
        write(PUT("/env-policies/" + encodeURIComponent(env), { name: v.name, require_approval: v.require_approval, approver_permission: v.approver_permission }), "环境策略已保存").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }

  /* ---------------------------------------------------------------- ai 智能助手 */

  function aiPageKey() {
    var seg = (location.hash || "").split("/")[2] || "config";
    return ["config", "chat", "history"].indexOf(seg) >= 0 ? seg : "config";
  }
  function renderAiLive(el) {
    var pg = aiPageKey();
    if (pg !== "config") {
      if (typeof ST.originals.ai === "function" && ST.originals.ai !== renderAiLive) {
        ST.originals.ai.call(null, el);
        var note = document.createElement("div");
        note.className = "x-warn";
        note.setAttribute("role", "note");
        note.style.marginBottom = "12px";
        note.textContent = "该子页暂未接入实时接口，以下为演示内容（模型与提示词请到「AI 模型与提示词」页管理）。";
        if (el.firstChild) el.insertBefore(note, el.firstChild); else el.appendChild(note);
        return;
      }
      el.innerHTML = '<div class="x-panel">' + emptyHTML("该页暂不可用") + "</div>";
      return;
    }
    var p = livePage(el, "AI 模型与提示词", "OpenAI 兼容模型接入与场景提示词", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newmodel>+ 新建模型</button><button class="x-btn" type="button" data-newprompt>+ 新建提示词</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { renderAiLive(el); };
    el.querySelector("[data-newmodel]").onclick = function () { modelModal(null, function () { renderAiLive(el); }); };
    el.querySelector("[data-newprompt]").onclick = function () { promptModal(null, function () { renderAiLive(el); }); };
    var MODELS = [], PROMPTS = [];
    function load() {
      var t = p.newToken();
      Promise.all([settle(GET("/ai/models"), []), settle(GET("/ai/prompts"), [])]).then(function (rs) {
        MODELS = rs[0].ok ? asArray(rs[0].value, ["items", "models"]) : [];
        PROMPTS = rs[1].ok ? asArray(rs[1].value, ["items", "prompts"]) : [];
        var failed = [];
        if (!rs[0].ok) failed.push("模型：" + errMsg(rs[0].error));
        if (!rs[1].ok) failed.push("提示词：" + errMsg(rs[1].error));
        var mRows = MODELS.map(function (m) {
          return [E(m.name), E(m.provider || "—"), code(m.model, 30), code(m.base_url, 46), m.has_api_key ? code(m.api_key_masked || "已设置", 18) : tag("未设置", "x-grey"),
            m.is_default ? tag("默认", "x-green") : E("—"), m.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"), fmtMin(m.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-mtest="' + E(m.id) + '">测试</button>' +
            '<button class="x-btn" type="button" data-medit="' + E(m.id) + '">编辑</button>' +
            (m.is_default ? "" : '<button class="x-btn" type="button" data-mdef="' + E(m.id) + '">设为默认</button>') +
            '<button class="x-btn" type="button" data-mdel="' + E(m.id) + '">删除</button></span>'];
        });
        var pRows = PROMPTS.map(function (x) {
          return [E(x.scene), E(x.name), code(x.description || "—", 40), x.is_default ? tag("默认", "x-green") : E("—"), x.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
            code(x.content || "", 40), fmtMin(x.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-pedit="' + E(x.id) + '">编辑</button><button class="x-btn" type="button" data-pdel="' + E(x.id) + '">删除</button></span>'];
        });
        var html = (failed.length ? banner("部分 AI 配置加载失败（其余区块仍可用）：" + failed.join("；")) : "") +
          panelHTML("模型", mRows.length ? tbl(["名称", "提供方", "模型", "Base URL", "API Key", "默认", "状态", "更新时间", "操作"], mRows) : emptyHTML("尚未配置模型。点击右上角「+ 新建模型」接入 OpenAI 兼容端点。"),
          '<button class="x-btn" type="button" data-newmodel2 style="margin-left:auto">+ 新建</button>') +
          panelHTML("提示词", pRows.length ? tbl(["场景", "名称", "描述", "默认", "状态", "内容摘要", "更新时间", "操作"], pRows) : emptyHTML("尚未配置提示词。"),
            '<button class="x-btn" type="button" data-newprompt2 style="margin-left:auto">+ 新建</button>');
        if (!p.paint(t, html)) return;
        bind();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function byId(list, id) { for (var i = 0; i < list.length; i++) if (String(list[i].id) === String(id)) return list[i]; return null; }
    function bind() {
      var m2 = p.box.querySelector("[data-newmodel2]"); if (m2) m2.onclick = function () { modelModal(null, function () { renderAiLive(el); }); };
      var p2 = p.box.querySelector("[data-newprompt2]"); if (p2) p2.onclick = function () { promptModal(null, function () { renderAiLive(el); }); };
      [].slice.call(p.box.querySelectorAll("[data-mtest]")).forEach(function (b) {
        b.onclick = function () {
          b.disabled = true;
          toast("正在测试模型连接…");
          POST("/ai/models/" + encodeURIComponent(b.getAttribute("data-mtest")) + "/test").then(function (r) {
            b.disabled = false;
            toast("连接成功" + (r && r.reply ? "，回复：" + r.reply : ""));
          }).catch(function (err) { b.disabled = false; toast("模型连接失败：" + errMsg(err)); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-medit]")).forEach(function (b) {
        b.onclick = function () { modelModal(byId(MODELS, b.getAttribute("data-medit")), function () { renderAiLive(el); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-mdef]")).forEach(function (b) {
        b.onclick = function () {
          var m = byId(MODELS, b.getAttribute("data-mdef"));
          if (!m) return;
          write(PUT("/ai/models/" + encodeURIComponent(m.id), modelBody(m, true)), "默认模型已更新").then(function (r) { if (r) renderAiLive(el); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-mdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除模型", "确定删除该模型？使用它的场景会改为跟随默认模型。", "删除", function () {
            write(DEL("/ai/models/" + encodeURIComponent(b.getAttribute("data-mdel"))), "模型已删除").then(function (r) { if (r) renderAiLive(el); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-pedit]")).forEach(function (b) {
        b.onclick = function () { promptModal(byId(PROMPTS, b.getAttribute("data-pedit")), function () { renderAiLive(el); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-pdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除提示词", "确定删除该提示词模板？", "删除", function () {
            write(DEL("/ai/prompts/" + encodeURIComponent(b.getAttribute("data-pdel"))), "提示词已删除").then(function (r) { if (r) renderAiLive(el); });
          });
        };
      });
    }
    function modelBody(m, forceDefault) {
      var body = {
        name: m.name, provider: m.provider || "", base_url: m.base_url, model: m.model,
        temperature: m.temperature, max_tokens: m.max_tokens || 0, extra_headers: m.extra_headers || {},
        is_default: forceDefault === true ? true : !!m.is_default, enabled: !!m.enabled
      };
      return body;
    }
    function modelModal(m, done) {
      formModal(m ? "编辑模型 · " + m.name : "新建模型", [
        { k: "name", label: "名称", required: true, value: m ? m.name : "", placeholder: "gpt-4o" },
        { k: "provider", label: "提供方", value: m ? m.provider || "" : "openai", placeholder: "openai / deepseek / azure" },
        { k: "base_url", label: "Base URL", required: true, value: m ? m.base_url : "", placeholder: "https://api.openai.com/v1" },
        { k: "api_key", label: "API Key（编辑时留空表示保留原值）", kind: "password", placeholder: m && m.has_api_key ? "已设置，留空保留" : "" },
        { k: "model", label: "模型名", required: true, value: m ? m.model : "", placeholder: "gpt-4o-mini" },
        { k: "temperature", label: "Temperature", value: m && m.temperature != null ? m.temperature : "0.2" },
        { k: "max_tokens", label: "Max Tokens", kind: "number", value: m && m.max_tokens ? m.max_tokens : "2048" },
        { k: "enabled", label: "启用", kind: "checkbox", value: m ? !!m.enabled : true, hint: "启用" },
        { k: "is_default", label: "设为默认模型", kind: "checkbox", value: m ? !!m.is_default : false, hint: "默认" },
        { k: "extra_headers", label: "额外请求头 JSON", kind: "textarea", rows: 2, value: m ? JSON.stringify(m.extra_headers || {}) : "{}" }
      ], m ? "保存" : "创建", function (v) {
        var headers;
        try { headers = parseJSON(v.extra_headers || "{}", {}); } catch (err) { toast(errMsg(err)); return; }
        var body = {
          name: v.name, provider: v.provider, base_url: v.base_url, model: v.model,
          temperature: +v.temperature || 0, max_tokens: +v.max_tokens || 0, extra_headers: headers,
          enabled: v.enabled, is_default: v.is_default
        };
        if (v.api_key) body.api_key = v.api_key;
        else if (!m) body.api_key = "";
        write(m ? PUT("/ai/models/" + encodeURIComponent(m.id), body) : POST("/ai/models", body), m ? "模型已保存" : "模型已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, "仅支持 OpenAI 兼容的 /chat/completions 端点；API Key 加密存储。");
    }
    function promptModal(x, done) {
      formModal(x ? "编辑提示词 · " + x.name : "新建提示词", [
        { k: "scene", label: "场景（如 alert / log / chat）", required: true, value: x ? x.scene : "", placeholder: "alert" },
        { k: "name", label: "名称", required: true, value: x ? x.name : "" },
        { k: "description", label: "描述", value: x ? x.description || "" : "" },
        { k: "content", label: "内容（Go text/template 语法，如 {{.cluster}}）", kind: "textarea", rows: 8, required: true, value: x ? x.content : "" },
        { k: "enabled", label: "启用", kind: "checkbox", value: x ? !!x.enabled : true, hint: "启用" },
        { k: "is_default", label: "设为该场景默认", kind: "checkbox", value: x ? !!x.is_default : false, hint: "默认" }
      ], x ? "保存" : "创建", function (v) {
        var body = { scene: v.scene, name: v.name, description: v.description, content: v.content, enabled: v.enabled, is_default: v.is_default };
        write(x ? PUT("/ai/prompts/" + encodeURIComponent(x.id), body) : POST("/ai/prompts", body), x ? "提示词已保存" : "提示词已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }


  /* ---------------------------------------------------------------- oncall 值班与 SLO */

  var OC = { incStatus: "", incSev: "", incQ: "", sloId: 0 };
  function ocPageKey() {
    var seg = (location.hash || "").split("/")[2] || "sched";
    return ["inc", "review", "slo"].indexOf(seg) >= 0 ? seg : "sched";
  }
  function ocTabs(cur) {
    return tabsHTML([{ t: "值班排班", h: "#/oncall" }, { t: "故障管理", h: "#/oncall/inc" }, { t: "故障复盘", h: "#/oncall/review" }, { t: "SLO", h: "#/oncall/slo" }], cur);
  }
  function renderOncallLive(el) {
    var pg = ocPageKey();
    if (pg === "inc") {
      var id = +((location.hash || "").split("/")[3] || 0);
      if (id > 0) return ocIncidentDetail(el, id);
      return ocIncidents(el);
    }
    if (pg === "review") return ocReview(el);
    if (pg === "slo") return ocSlos(el);
    return ocSchedules(el);
  }
  function kpiCards(items) {
    return '<div class="x-cards x-cards4">' + items.map(function (it) {
      return '<div class="x-card x-kpi"><p>' + E(it[0]) + "</p><strong>" + it[1] + "</strong></div>";
    }).join("") + "</div>";
  }
  function toISO(v) {
    if (!v) return "";
    var d = new Date(v);
    if (isNaN(d.getTime())) d = new Date(String(v).replace(" ", "T"));
    return isNaN(d.getTime()) ? String(v) : d.toISOString();
  }
  function dtLocal(v) {
    if (!v) return "";
    var d = new Date(v);
    if (isNaN(d.getTime())) return "";
    return d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + "T" + pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }
  function memberList(v) {
    return Array.isArray(v) ? v : String(v || "").split(",").map(function (s) { return s.trim(); }).filter(Boolean);
  }
  function oncallRow(w) {
    return [E(w.schedule || ("#" + w.schedule_id)), E("L" + w.layer), E(w.username || "—"), E(w.env || "*"),
      fmtMin(w.starts_at), fmtMin(w.ends_at), w.override ? tag("代班", "x-orange") : tag("轮值", "x-blue"),
      E(w.next_username || "—") + (w.next_starts_at ? " · " + fmtMin(w.next_starts_at) : "")];
  }

  function ocSchedules(el) {
    var p = livePage(el, "值班排班", "分层轮值、代班与当前值班", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newsch>+ 新建排班</button>', ocTabs("#/oncall"));
    el.querySelector("[data-refresh]").onclick = function () { ocSchedules(el); };
    el.querySelector("[data-newsch]").onclick = function () { ocScheduleModal(null, function () { ocSchedules(el); }); };
    function load() {
      var t = p.newToken();
      Promise.all([settle(GET("/oncall/overview"), {}), GET("/oncall/schedules", { page: 1, page_size: 100 })]).then(function (rs) {
        var ov = rs[0].ok ? (rs[0].value || {}) : {}, ovErr = rs[0].ok ? null : rs[0].error, data = rs[1] || {};
        var current = Array.isArray(ov.current) ? ov.current : asArray(ov, ["current"]);
        var inc = ov.incidents || {};
        var sloCount = Array.isArray(ov.slos) ? ov.slos.length : 0;
        var kpis = kpiCards([
          ["当前值班", (current.length ? current.length + " 人" : "—")],
          ["未解决故障", num((+inc.open || 0) + (+inc.acknowledged || 0))],
          ["已解决故障", num(inc.resolved || 0)],
          ["SLO", num(sloCount)]
        ]);
        var curRows = current.map(oncallRow);
        var items = Array.isArray(data.items) ? data.items : asArray(data, ["items", "schedules"]);
        var schRows = items.map(function (s) {
          return [mono("#" + s.id), E(s.name), E(s.env || "*"), E(s.timezone || "—"), s.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
            code(s.description || "—", 50), fmtMin(s.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-schdet="' + E(s.id) + '">详情</button><button class="x-btn" type="button" data-schdel="' + E(s.id) + '">删除</button></span>'];
        });
        var recent = Array.isArray(ov.recent_incidents) ? ov.recent_incidents : [];
        var recentRows = recent.slice(0, 5).map(function (x) {
          return [mono("#" + x.id), '<a class="x-tlink" href="#/oncall/inc/' + E(x.id) + '">' + E(x.title) + "</a>", sevTag(x.severity), stTag(x.status), E(x.service || "—"), fmtMin(x.declared_at)];
        });
        var html = (ovErr ? banner("值班概览（GET /oncall/overview）加载失败：" + errMsg(ovErr) + "；排班列表仍可用。") : "") + kpis +
          panelHTML("当前值班（GET /oncall/current 聚合）", curRows.length ? tbl(["排班", "层级", "值班人", "环境", "开始", "结束", "类型", "下一班"], curRows) : emptyHTML("当前没有生效中的轮值，请先在排班中配置轮值层。")) +
          panelHTML("排班", schRows.length ? tbl(["ID", "名称", "环境", "时区", "状态", "说明", "更新时间", "操作"], schRows) : emptyHTML("暂无排班。点击右上角「+ 新建排班」，再在详情中配置轮值层。")) +
          panelHTML("最近故障", recentRows.length ? tbl(["ID", "标题", "级别", "状态", "服务", "声明时间"], recentRows) : emptyHTML("暂无故障记录"));
        if (!p.paint(t, html)) return;
        bind(items);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function bind(items) {
      function byId(id) { for (var i = 0; i < items.length; i++) if (String(items[i].id) === String(id)) return items[i]; return null; }
      [].slice.call(p.box.querySelectorAll("[data-schdet]")).forEach(function (b) {
        b.onclick = function () { ocScheduleDetail(b.getAttribute("data-schdet")); };
      });
      [].slice.call(p.box.querySelectorAll("[data-schdel]")).forEach(function (b) {
        b.onclick = function () {
          var s = byId(b.getAttribute("data-schdel"));
          confirmDo("删除排班", "确定删除排班 <b>" + E(s ? s.name : "") + "</b>？轮值与代班将级联删除。", "删除", function () {
            write(DEL("/oncall/schedules/" + encodeURIComponent(b.getAttribute("data-schdel"))), "排班已删除").then(function (r) { if (r) load(); });
          });
        };
      });
    }
    load();
  }

  function ocScheduleDetail(id) {
    GET("/oncall/schedules/" + encodeURIComponent(id)).then(function (data) {
      var s = data.schedule || {}, rots = asArray(data, ["rotations"]), ovs = asArray(data, ["overrides"]);
      var rotRows = rots.map(function (r) {
        return [E("L" + r.layer), E(r.name || "—"), fmtMin(r.start_at), E(dur(r.length_seconds)), E(memberList(r.members).join(" → ") || "—"),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-rotedit="' + E(r.layer) + '">编辑</button><button class="x-btn" type="button" data-rotdel="' + E(r.layer) + '">删除</button></span>'];
      });
      var ovRows = ovs.map(function (o) {
        return [E("L" + o.layer), E(o.username), fmtMin(o.starts_at), fmtMin(o.ends_at), E(o.reason || "—"), '<button class="x-btn" type="button" data-ovdel="' + E(o.id) + '">删除</button>'];
      });
      var info = '<dl class="x-sum" style="margin:0 0 12px">' + [["排班", E(s.name || "")], ["环境", E(s.env || "*")], ["时区", E(s.timezone || "")],
        ["状态", s.enabled ? tag("启用", "x-green") : tag("停用", "x-grey")], ["说明", E(s.description || "—")], ["更新时间", fmtMin(s.updated_at)]]
        .map(function (kv) { return "<dt>" + kv[0] + "</dt><dd>" + kv[1] + "</dd>"; }).join("") + "</dl>";
      var foot = '<button class="x-btn" type="button" data-close>关闭</button>' +
        '<button class="x-btn" type="button" data-schededit>编辑排班</button><button class="x-btn" type="button" data-rotadd>+ 轮值层</button><button class="x-btn x-btn--pri" type="button" data-ovadd>+ 代班</button>';
      dlg("排班详情 · " + E(s.name || id), '<div class="x-form">' + info +
        panelHTML("轮值层", rotRows.length ? tbl(["层级", "名称", "起始", "换班周期", "成员顺序", "操作"], rotRows) : emptyHTML("尚未配置轮值层")) +
        panelHTML("代班", ovRows.length ? tbl(["层级", "值班人", "开始", "结束", "原因", "操作"], ovRows) : emptyHTML("暂无代班")) + "</div>", foot);
      var can = function (sel, fn) { var b = q(sel); if (b) b.onclick = fn; };
      can("data-schededit", function () { ocScheduleModal(s, function () { closeAll(); ocScheduleDetail(id); }); });
      can("data-rotadd", function () { rotationModal(id, null, function () { closeAll(); ocScheduleDetail(id); }); });
      can("data-ovadd", function () { overrideModal(id, function () { closeAll(); ocScheduleDetail(id); }); });
      [].slice.call(document.querySelectorAll("[data-rotedit]")).forEach(function (b) {
        b.onclick = function () {
          var layer = +b.getAttribute("data-rotedit"), cur = null;
          rots.forEach(function (r) { if (+r.layer === layer) cur = r; });
          rotationModal(id, cur, function () { closeAll(); ocScheduleDetail(id); });
        };
      });
      [].slice.call(document.querySelectorAll("[data-rotdel]")).forEach(function (b) {
        b.onclick = function () {
          var layer = b.getAttribute("data-rotdel");
          confirmDo("删除轮值层", "确定删除 L" + E(layer) + " 轮值层？", "删除", function () {
            write(DEL("/oncall/schedules/" + encodeURIComponent(id) + "/rotations/" + encodeURIComponent(layer)), "轮值层已删除").then(function (r) { if (r) { closeAll(); ocScheduleDetail(id); } });
          });
        };
      });
      [].slice.call(document.querySelectorAll("[data-ovdel]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("删除代班", "确定删除该代班？", "删除", function () {
            write(DEL("/oncall/overrides/" + encodeURIComponent(b.getAttribute("data-ovdel"))), "代班已删除").then(function (r) { if (r) { closeAll(); ocScheduleDetail(id); } });
          });
        };
      });
    }).catch(function (err) { toast(errMsg(err)); });
  }

  function ocScheduleModal(s, done) {
    formModal(s ? "编辑排班 · " + s.name : "新建排班", [
      { k: "name", label: "名称（唯一）", required: true, value: s ? s.name : "", placeholder: "core-api" },
      { k: "description", label: "说明", kind: "textarea", rows: 2, value: s ? s.description || "" : "" },
      { k: "env", label: "环境（空 = 全局）", value: s ? s.env || "" : "" },
      { k: "timezone", label: "时区（仅展示）", value: s ? s.timezone || "Asia/Shanghai" : "Asia/Shanghai" },
      { k: "enabled", label: "启用", kind: "checkbox", value: s ? !!s.enabled : true, hint: "启用" }
    ], s ? "保存" : "创建", function (v) {
      var body = { name: v.name, description: v.description, env: v.env, timezone: v.timezone, enabled: v.enabled };
      write(s ? PUT("/oncall/schedules/" + encodeURIComponent(s.id), body) : POST("/oncall/schedules", body), s ? "排班已保存" : "排班已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }
  function rotationModal(scheduleId, rot, done) {
    formModal(rot ? "编辑轮值层 · L" + rot.layer : "新增轮值层", [
      { k: "layer", label: "层级（1 = 一线，2+ = 升级层）", kind: "number", required: true, value: rot ? rot.layer : "1" },
      { k: "name", label: "名称", value: rot ? rot.name || "primary" : "primary" },
      { k: "start_at", label: "起始时间", kind: "datetime-local", required: true, value: dtLocal(rot ? rot.start_at : new Date()) },
      { k: "length_seconds", label: "换班周期（秒）", kind: "number", required: true, value: rot ? rot.length_seconds : 604800 },
      { k: "members", label: "成员（逗号分隔，按顺序轮值）", required: true, value: rot ? memberList(rot.members).join(", ") : "" }
    ], "保存", function (v) {
      var members = memberList(v.members);
      if (!members.length) { toast("成员不能为空"); return; }
      write(PUT("/oncall/schedules/" + encodeURIComponent(scheduleId) + "/rotations/" + encodeURIComponent(v.layer), {
        name: v.name, start_at: toISO(v.start_at), length_seconds: +v.length_seconds, members: members
      }), "轮值层已保存").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    }, "同一 layer 再次保存为整体替换；members 顺序即轮值顺序。");
  }
  function overrideModal(scheduleId, done) {
    formModal("新增代班", [
      { k: "layer", label: "层级", kind: "number", required: true, value: "1" },
      { k: "username", label: "代班人", required: true },
      { k: "starts_at", label: "开始时间", kind: "datetime-local", required: true, value: dtLocal(new Date()) },
      { k: "ends_at", label: "结束时间", kind: "datetime-local", required: true, value: dtLocal(new Date(Date.now() + 864e5)) },
      { k: "reason", label: "原因", value: "休假" }
    ], "保存", function (v) {
      if (new Date(toISO(v.ends_at)) <= new Date(toISO(v.starts_at))) { toast("结束时间必须晚于开始时间"); return; }
      write(POST("/oncall/schedules/" + encodeURIComponent(scheduleId) + "/overrides", {
        layer: +v.layer, username: v.username, starts_at: toISO(v.starts_at), ends_at: toISO(v.ends_at), reason: v.reason
      }), "代班已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    }, "代班区间左闭右开；重叠时最晚开始（同开始取最大 id）者生效。");
  }

  function ocIncidents(el) {
    var p = livePage(el, "故障管理", "故障声明、认领、解决与时间线", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newinc>+ 发起故障</button>', ocTabs("#/oncall/inc"));
    el.querySelector("[data-refresh]").onclick = function () { ocIncidents(el); };
    el.querySelector("[data-newinc]").onclick = function () { incidentModal(function () { ocIncidents(el); }); };
    function load() {
      var t = p.newToken();
      var qy = { page: 1, page_size: 50 };
      if (OC.incStatus) qy.status = OC.incStatus;
      if (OC.incSev) qy.severity = OC.incSev;
      if (OC.incQ) qy.q = OC.incQ;
      GET("/oncall/incidents", qy).then(function (data) {
        var items = Array.isArray(data) ? data : asArray(data, ["items", "incidents"]);
        var filters = barHTML([
          '<select class="x-select" data-icst style="width:auto">' + [{ v: "", t: "全部状态" }, { v: "declared", t: "已声明" }, { v: "acknowledged", t: "处理中" }, { v: "resolved", t: "已解决" }].map(function (o) { return '<option value="' + o.v + '"' + (o.v === OC.incStatus ? " selected" : "") + ">" + o.t + "</option>"; }).join("") + "</select>",
          '<select class="x-select" data-icsev style="width:auto">' + [{ v: "", t: "全部级别" }, "P1", "P2", "P3", "P4"].map(function (o) { var v = typeof o === "object" ? o.v : o, tx = typeof o === "object" ? o.t : o; return '<option value="' + v + '"' + (v === OC.incSev ? " selected" : "") + ">" + tx + "</option>"; }).join("") + "</select>",
          '<input class="x-input x-search" data-icq value="' + E(OC.incQ) + '" placeholder="搜索标题 / 服务" aria-label="搜索故障" />',
          '<button class="x-btn x-btn--pri" type="button" data-icrun>查询</button>',
          '<span class="x-frac" style="margin-left:auto">共 ' + num((data && data.total) != null ? data.total : items.length) + " 条</span>"
        ]);
        var rows = items.map(function (x) {
          return [mono("#" + x.id), '<a class="x-tlink" href="#/oncall/inc/' + E(x.id) + '">' + E(x.title) + "</a>", sevTag(x.severity), stTag(x.status),
            E(x.service || "—"), E(x.env || "*"), E(x.assigned_to || x.acked_by || "—"), E(x.declared_by || "—"), fmtMin(x.declared_at), fmtMin(x.resolved_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-icack="' + E(x.id) + '"' + (x.status !== "declared" ? " disabled" : "") + ">认领</button>" +
            '<button class="x-btn" type="button" data-icres="' + E(x.id) + '"' + (x.status === "resolved" ? " disabled" : "") + ">解决</button></span>"];
        });
        var html = panelHTML("故障", filters + (items.length ? tbl(["ID", "标题", "级别", "状态", "服务", "环境", "处理人", "声明人", "声明时间", "解决时间", "操作"], rows) : emptyHTML("暂无故障记录。")));
        if (!p.paint(t, html)) return;
        bind();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function bind() {
      var st = p.box.querySelector("[data-icst]"), sev = p.box.querySelector("[data-icsev]"), iq = p.box.querySelector("[data-icq]");
      if (st) st.onchange = function () { OC.incStatus = st.value; load(); };
      if (sev) sev.onchange = function () { OC.incSev = sev.value; load(); };
      var run = p.box.querySelector("[data-icrun]"); if (run) run.onclick = function () { OC.incQ = iq.value.trim(); load(); };
      if (iq) iq.onkeydown = function (e) { if (e.key === "Enter") { e.preventDefault(); OC.incQ = iq.value.trim(); load(); } };
      [].slice.call(p.box.querySelectorAll("[data-icack]")).forEach(function (b) {
        b.onclick = function () { ackModal(b.getAttribute("data-icack"), function () { load(); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-icres]")).forEach(function (b) {
        b.onclick = function () { resolveModal(b.getAttribute("data-icres"), function () { load(); }); };
      });
    }
    load();
  }
  function ackModal(id, done) {
    formModal("认领故障", [{ k: "comment", label: "处理备注", kind: "textarea", rows: 3, placeholder: "正在排查" }], "认领", function (v) {
      write(POST("/oncall/incidents/" + encodeURIComponent(id) + "/ack", { comment: v.comment }), "故障已认领").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }
  function resolveModal(id, done) {
    formModal("解决故障", [{ k: "note", label: "解决说明", kind: "textarea", rows: 3, placeholder: "扩容后恢复" }], "标记解决", function (v) {
      write(POST("/oncall/incidents/" + encodeURIComponent(id) + "/resolve", { note: v.note }), "故障已解决").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }
  function noteModal(id, done) {
    formModal("添加处理记录", [{ k: "message", label: "内容", kind: "textarea", rows: 3, required: true }], "添加", function (v) {
      write(POST("/oncall/incidents/" + encodeURIComponent(id) + "/notes", { message: v.message }), "处理记录已添加").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }
  function incidentModal(done) {
    GET("/oncall/schedules", { page: 1, page_size: 100 }).then(function (data) {
      var items = asArray(data, ["items", "schedules"]);
      var schOpts = [{ v: 0, t: "不关联排班" }].concat(items.map(function (s) { return { v: s.id, t: s.name + (s.env ? " · " + s.env : "") }; }));
      formModal("发起故障", [
        { k: "title", label: "标题", required: true, placeholder: "API 5xx 飙升" },
        { k: "description", label: "描述", kind: "textarea", rows: 3 },
        { k: "severity", label: "级别", kind: "select", options: ["P1", "P2", "P3", "P4"], value: "P3" },
        { k: "service", label: "服务", placeholder: "core-api" },
        { k: "env", label: "环境", placeholder: "prod" },
        { k: "schedule_id", label: "关联排班（可选，自动指派一线值班）", kind: "select", options: schOpts },
        { k: "assigned_to", label: "指派给（可选，覆盖自动指派）" }
      ], "发起", function (v) {
        var body = { title: v.title, description: v.description, severity: v.severity, service: v.service, env: v.env, assigned_to: v.assigned_to };
        if (+v.schedule_id) body.schedule_id = +v.schedule_id;
        write(POST("/oncall/incidents", body), "故障已声明").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }).catch(function (err) { toast(errMsg(err)); });
  }
  function ocIncidentDetail(el, id) {
    var p = livePage(el, "故障详情", "INC-" + id + " 的状态、时间线与处理动作", '<button class="x-btn" type="button" data-refresh>刷新</button>', ocTabs("#/oncall/inc"));
    el.querySelector("[data-refresh]").onclick = function () { renderOncallLive(el); };
    function load() {
      var t = p.newToken();
      GET("/oncall/incidents/" + encodeURIComponent(id)).then(function (data) {
        var x = data.incident || {}, tl = asArray(data, ["timeline"]);
        var info = '<dl class="x-sum">' + [["ID", mono("#" + x.id)], ["标题", E(x.title)], ["级别", sevTag(x.severity)], ["状态", stTag(x.status)],
          ["服务", E(x.service || "—")], ["环境", E(x.env || "*")], ["处理人", E(x.assigned_to || "—")], ["声明人", E(x.declared_by || "—")],
          ["声明时间", fmtMin(x.declared_at)], ["认领", E(x.acked_by || "—") + (x.acked_at ? " · " + fmtMin(x.acked_at) : "")],
          ["解决", E(x.resolved_by || "—") + (x.resolved_at ? " · " + fmtMin(x.resolved_at) : "")], ["解决说明", E(x.resolve_note || "—")],
          ["描述", E(x.description || "—")]]
          .map(function (kv) { return "<dt>" + kv[0] + "</dt><dd>" + kv[1] + "</dd>"; }).join("") + "</dl>";
        var tlRows = tl.map(function (e) { return [fmtTime(e.created_at), stTag(e.kind), E(e.actor || "—"), E(e.message || "—")]; });
        var actions = '<span class="ops-nowrap">' +
          (x.status === "declared" ? '<button class="x-btn x-btn--pri" type="button" data-ack>认领</button>' : "") +
          (x.status !== "resolved" ? '<button class="x-btn" type="button" data-res>标记解决</button>' : "") +
          '<button class="x-btn" type="button" data-note>添加记录</button></span>';
        var html = '<p style="margin:0 0 12px"><a class="x-tlink" href="#/oncall/inc">← 返回故障列表</a><span style="float:right">' + actions + "</span></p>" + info +
          panelHTML("时间线", tlRows.length ? tbl(["时间", "事件", "操作人", "说明"], tlRows) : emptyHTML("暂无时间线"));
        if (!p.paint(t, html)) return;
        var ack = p.box.querySelector("[data-ack]"); if (ack) ack.onclick = function () { ackModal(id, function () { renderOncallLive(el); }); };
        var res = p.box.querySelector("[data-res]"); if (res) res.onclick = function () { resolveModal(id, function () { renderOncallLive(el); }); };
        var note = p.box.querySelector("[data-note]"); if (note) note.onclick = function () { noteModal(id, function () { renderOncallLive(el); }); };
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    load();
  }
  function ocReview(el) {
    var p = livePage(el, "故障复盘", "已解决故障与处理时间线", '<button class="x-btn" type="button" data-refresh>刷新</button>', ocTabs("#/oncall/review"));
    el.querySelector("[data-refresh]").onclick = function () { ocReview(el); };
    function load() {
      var t = p.newToken();
      GET("/oncall/incidents", { status: "resolved", page: 1, page_size: 100 }).then(function (data) {
        var items = asArray(data, ["items", "incidents"]);
        var rows = items.map(function (x) {
          var mins = x.resolved_at && x.declared_at ? Math.max(1, Math.round((new Date(x.resolved_at) - new Date(x.declared_at)) / 6e4)) : null;
          return [mono("#" + x.id), '<a class="x-tlink" href="#/oncall/inc/' + E(x.id) + '">' + E(x.title) + "</a>", sevTag(x.severity), E(x.service || "—"),
            E(x.env || "*"), fmtMin(x.declared_at), fmtMin(x.resolved_at), mins != null ? E(mins + " 分钟") : "—", E(x.resolve_note || "—")];
        });
        var html = banner("后端暂无独立复盘实体，此页展示已解决故障及处理时间线（在故障详情中可追加处理记录）。") +
          panelHTML("已解决故障", rows.length ? tbl(["ID", "标题", "级别", "服务", "环境", "声明", "解决", "历时", "解决说明"], rows) : emptyHTML("暂无已解决故障"));
        p.paint(t, html);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    load();
  }

  function ocSlos(el) {
    var p = livePage(el, "SLO", "目标、错误预算与手工 SLI 录入", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newslo>+ 新建 SLO</button>', ocTabs("#/oncall/slo"));
    el.querySelector("[data-refresh]").onclick = function () { ocSlos(el); };
    el.querySelector("[data-newslo]").onclick = function () { sloModal(null, function () { ocSlos(el); }); };
    var SLOS = [];
    function load() {
      var t = p.newToken();
      GET("/oncall/slos", { page: 1, page_size: 100 }).then(function (data) {
        SLOS = asArray(data, ["items", "slos"]);
        var rows = SLOS.map(function (s) {
          return [mono("#" + s.id), E(s.name), E(s.service || "—"), E(s.env || "*"), E(s.target + "%"), E(s.window_days + " 天"),
            tag(s.source || "manual", s.source === "prometheus" ? "x-blue" : "x-grey"), tag(s.sli_type || "ratio"),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-slobud="' + E(s.id) + '">错误预算</button>' +
            '<button class="x-btn" type="button" data-sloent="' + E(s.id) + '">手工录入</button>' +
            (s.source === "prometheus" ? "" : '<button class="x-btn" type="button" data-sloedit="' + E(s.id) + '">编辑</button>') +
            '<button class="x-btn" type="button" data-slodel="' + E(s.id) + '">删除</button></span>'];
        });
        var html = panelHTML("SLO 定义", rows.length ? tbl(["ID", "名称", "服务", "环境", "目标", "窗口", "来源", "SLI 类型", "操作"], rows) : emptyHTML("暂无 SLO。点击右上角「+ 新建 SLO」。"));
        if (!p.paint(t, html)) return;
        bind();
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function byId(id) { for (var i = 0; i < SLOS.length; i++) if (String(SLOS[i].id) === String(id)) return SLOS[i]; return null; }
    function bind() {
      [].slice.call(p.box.querySelectorAll("[data-slobud]")).forEach(function (b) { b.onclick = function () { budgetModal(b.getAttribute("data-slobud")); }; });
      [].slice.call(p.box.querySelectorAll("[data-sloent]")).forEach(function (b) {
        b.onclick = function () { var s = byId(b.getAttribute("data-sloent")); if (s) entriesModal(s); };
      });
      [].slice.call(p.box.querySelectorAll("[data-sloedit]")).forEach(function (b) {
        b.onclick = function () { sloModal(byId(b.getAttribute("data-sloedit")), function () { ocSlos(el); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-slodel]")).forEach(function (b) {
        b.onclick = function () {
          var s = byId(b.getAttribute("data-slodel"));
          confirmDo("删除 SLO", "确定删除 SLO <b>" + E(s ? s.name : "") + "</b>？", "删除", function () {
            write(DEL("/oncall/slos/" + encodeURIComponent(b.getAttribute("data-slodel"))), "SLO 已删除").then(function (r) { if (r) ocSlos(el); });
          });
        };
      });
    }
    function sloModal(s, done) {
      dsList({ type: "prometheus" }).then(function (data) {
        var proms = Array.isArray(data) ? data : asArray(data, ["items"]);
        var promOpts = [{ v: 0, t: "不指定（manual 可留空）" }].concat(proms.map(function (d) { return { v: d.id, t: d.name + " · " + (d.env || "*") }; }));
        formModal(s ? "编辑 SLO · " + s.name : "新建 SLO", [
          { k: "name", label: "名称", required: true, value: s ? s.name : "" },
          { k: "service", label: "服务", value: s ? s.service || "" : "" },
          { k: "env", label: "环境", value: s ? s.env || "" : "" },
          { k: "description", label: "描述", kind: "textarea", rows: 2, value: s ? s.description || "" : "" },
          { k: "target", label: "目标（百分比，0 < x ≤ 100）", required: true, value: s ? s.target : "99.9" },
          { k: "window_days", label: "窗口天数（1–365）", kind: "number", value: s ? s.window_days : "30" },
          { k: "source", label: "数据来源", kind: "select", options: [{ v: "manual", t: "手工录入" }, { v: "prometheus", t: "Prometheus 查询" }], value: s ? s.source : "manual" },
          { k: "sli_type", label: "SLI 类型", kind: "select", options: [{ v: "ratio", t: "ratio（good/total）" }, { v: "query", t: "query（直接返回 0–1 比率）" }], value: s ? s.sli_type : "ratio" },
          { k: "datasource_id", label: "Prometheus 数据源（prometheus 来源必填）", kind: "select", options: promOpts, value: s && s.datasource_id ? s.datasource_id : "" },
          { k: "good_query", label: "good 查询（ratio）", kind: "textarea", rows: 2, value: s ? s.good_query || "" : "" },
          { k: "total_query", label: "total 查询（ratio）", kind: "textarea", rows: 2, value: s ? s.total_query || "" : "" },
          { k: "query", label: "SLI 查询（query，返回 0–1）", kind: "textarea", rows: 2, value: s ? s.query || "" : "" }
        ], s ? "保存" : "创建", function (v) {
          var target = +v.target;
          if (!(target > 0 && target <= 100)) { toast("目标必须在 (0,100] 之间"); return; }
          var body = { name: v.name, service: v.service, env: v.env, description: v.description, target: target, window_days: +v.window_days || 30,
            sli_type: v.sli_type, source: v.source, good_query: v.good_query, total_query: v.total_query, query: v.query };
          if (+v.datasource_id) body.datasource_id = +v.datasource_id;
          write(s ? PUT("/oncall/slos/" + encodeURIComponent(s.id), body) : POST("/oncall/slos", body), s ? "SLO 已保存" : "SLO 已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
        }, "manual：手工录入 good/total；prometheus：需选数据源并按 sli_type 填查询。");
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function budgetModal(id) {
      GET("/oncall/slos/" + encodeURIComponent(id) + "/budget").then(function (data) {
        var b = (data && data.budget) || {};
        var warn = b.has_data === false ? banner("窗口内无数据：预算视为未消耗，请检查数据源与查询。") : "";
        var health = b.healthy ? tag("健康", "x-green") : tag("消耗异常", "x-red");
        var cards = kpiCards([
          ["SLI", b.has_data !== false && b.sli != null ? pct(b.sli) : "无数据"],
          ["目标", b.target != null ? b.target + "%" : "—"],
          ["剩余预算", b.remaining_pct != null ? '<span style="color:' + (b.healthy ? "#237804" : "#b42318") + '">' + (Math.round(b.remaining_pct * 10) / 10) + "%</span>" : "—"],
          ["燃烧率", b.burn_rate != null ? Math.round(b.burn_rate * 100) / 100 : "—"]
        ]);
        var pts = Array.isArray(data.points) ? data.points : [];
        var trend = pts.length > 1 ? '<p class="ops-sub">预算趋势（剩余 %）</p>' + spark(pts.map(function (pt) { return [pt.ts, pt.remaining_pct]; })) : "";
        infoModal("错误预算 · " + (data.slo && data.slo.name ? data.slo.name : id), warn + cards +
          '<p class="ops-sub">' + health + " · 来源 " + E(data.source || "") + " · 区间 " + fmtMin(data.from) + " → " + fmtMin(data.to) + "</p>" + trend + preBlock(b));
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function entriesModal(s) {
      GET("/oncall/slos/" + encodeURIComponent(s.id) + "/entries").then(function (data) {
        var items = asArray(data, ["items", "entries"]);
        var rows = items.map(function (e) {
          return [mono("#" + e.id), fmtMin(e.period_start), fmtMin(e.period_end), num(e.good), num(e.total), E(e.note || "—"),
            '<button class="x-btn" type="button" data-entdel="' + E(e.id) + '">删除</button>'];
        });
        dlg("手工 SLI 录入 · " + E(s.name), '<div class="x-form">' +
          '<p class="x-desc" style="margin:0">SLO #' + E(s.id) + "（" + E(s.source) + "） · 仅 manual 来源可录入手工窗口。</p>" +
          (rows.length ? tbl(["ID", "窗口开始", "窗口结束", "good", "total", "备注", "操作"], rows) : emptyHTML("暂无手工条目")) + "</div>",
          '<button class="x-btn" type="button" data-close>关闭</button>' + (s.source === "manual" ? '<button class="x-btn x-btn--pri" type="button" data-entadd>+ 新增窗口</button>' : ""));
        [].slice.call(document.querySelectorAll("[data-entadd]")).forEach(function (b) {
          b.onclick = function () { closeAll(); entryModal(s); };
        });
        [].slice.call(document.querySelectorAll("[data-entdel]")).forEach(function (b) {
          b.onclick = function () {
            confirmDo("删除条目", "确定删除该手工窗口？", "删除", function () {
              write(DEL("/oncall/slos/" + encodeURIComponent(s.id) + "/entries/" + encodeURIComponent(b.getAttribute("data-entdel"))), "条目已删除").then(function (r) { if (r) { closeAll(); entriesModal(s); } });
            });
          };
        });
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function entryModal(s) {
      formModal("新增手工 SLI 窗口", [
        { k: "period_start", label: "窗口开始", kind: "datetime-local", required: true, value: dtLocal(new Date(Date.now() - 864e5)) },
        { k: "period_end", label: "窗口结束", kind: "datetime-local", required: true, value: dtLocal(new Date()) },
        { k: "good", label: "good（成功量）", kind: "number", required: true, value: "0" },
        { k: "total", label: "total（总量）", kind: "number", required: true, value: "0" },
        { k: "note", label: "备注" }
      ], "保存", function (v) {
        var good = +v.good, total = +v.total;
        if (!(total > 0)) { toast("total 必须大于 0"); return; }
        if (good > total) { toast("good 不能大于 total"); return; }
        if (new Date(toISO(v.period_end)) <= new Date(toISO(v.period_start))) { toast("窗口结束必须晚于开始"); return; }
        write(POST("/oncall/slos/" + encodeURIComponent(s.id) + "/entries", {
          period_start: toISO(v.period_start), period_end: toISO(v.period_end), good: good, total: total, note: v.note
        }), "手工窗口已添加").then(function (r) { if (r) { closeAll(); entriesModal(s); } });
      });
    }
    load();
  }

  // ---------------------------------------------------------------- boot / export
  // 主脚本里的 routes / phead / tbl 是 classic script 的全局词法绑定（不是 window 属性），
  // 因此这里必须用裸标识符探测与访问；包在 try/catch 里应对脚本加载顺序异常。
  function helpersReady() {
    try {
      return typeof routes === "object" && routes !== null && typeof render === "function" &&
        typeof phead === "function" && typeof tbl === "function" && typeof navGo === "function" &&
        typeof dlg === "function" && typeof k8Confirm === "function";
    } catch (e) { return false; }
  }
  /* ---------------------------------------------------------------- 运维审计 */
  // 实时审计页：数据来自 GET /audit-logs（审计中间件自动记录全部写操作）
  var AUDIT_RES = ["", "auth", "users", "roles", "settings", "approvals", "cluster", "hosts", "bastion", "cicd", "middleware", "observability", "ai", "devcenter", "automation"];
  function renderAuditLive(el) {
    var p = livePage(el, "运维审计", "审计中间件记录的全部写操作：按资源、操作人、结果筛选，点行查看原始记录", 
      '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn" type="button" data-export>导出 CSV</button>', "");
    var st = { page: 1, size: 50, resource: "", username: "", result: "" };
    var rows = [], total = 0;
    el.querySelector("[data-refresh]").onclick = function () { load(); };
    el.querySelector("[data-export]").onclick = function () {
      if (!rows.length) return toast("当前没有可导出的记录");
      var head = ["时间", "操作人", "IP", "方法", "路径", "资源", "动作", "HTTP", "结果", "耗时(ms)"];
      var csv = [head.join(",")].concat(rows.map(function (r) {
        return [fmtMin(r.created_at), r.username, r.ip, r.method, r.path, r.resource, r.action, r.status_code, r.result, r.duration_ms]
          .map(function (v) { return '"' + String(v == null ? "" : v).replace(/"/g, '""') + '"'; }).join(",");
      })).join("\n");
      try {
        var a = document.createElement("a");
        a.href = URL.createObjectURL(new Blob(["\ufeff" + csv], { type: "text/csv;charset=utf-8" }));
        a.download = "audit-logs-page" + st.page + ".csv";
        document.body.appendChild(a); a.click(); a.remove();
        toast("已导出 " + rows.length + " 条记录");
      } catch (e) { toast("导出失败：" + errMsg(e)); }
    };
    function filters() {
      return '<div class="x-panel" style="margin-bottom:16px"><div class="x-tbar" style="margin:0">' +
        selHTML("resource", "资源", AUDIT_RES.map(function (r) { return { v: r, t: r || "全部资源" }; }), st.resource, " data-f=resource") +
        '<label><span>操作人</span><input class="x-input" name="username" data-f=username placeholder="用户名，支持模糊" value="' + E(st.username) + '" /></label>' +
        selHTML("result", "结果", [{ v: "", t: "全部结果" }, { v: "success", t: "成功" }, { v: "failure", t: "失败" }], st.result, " data-f=result") +
        '<button class="x-btn x-btn--pri" type="button" data-apply style="margin-left:auto">查询</button>' +
        '<button class="x-btn" type="button" data-reset>重置</button></div></div>';
    }
    function metrics() {
      var fail = rows.filter(function (r) { return r.result !== "success" || (r.status_code || 0) >= 400; }).length;
      var res = {}; rows.forEach(function (r) { res[r.resource || "-"] = 1; });
      var latest = rows.length ? fmtMin(rows[0].created_at) : "—";
      return '<div class="x-cards" style="grid-template-columns:repeat(auto-fit,minmax(150px,1fr));margin-bottom:16px">' +
        [["匹配记录", String(total), ""], ["本页失败", String(fail), fail ? "#cf1322" : ""], ["涉及资源", String(Object.keys(res).length), ""], ["最近一条", latest, ""]]
          .map(function (x) { return '<div class="x-card"><p>' + x[0] + "</p><strong" + (x[2] ? ' style="color:' + x[2] + '"' : "") + ">" + E(x[1]) + "</strong></div>"; }).join("") +
        "</div>";
    }
    function table() {
      if (!rows.length) return panelHTML("审计记录", emptyHTML("没有匹配的审计记录。换个筛选条件，或先做一次写操作。"), "");
      var body = rows.map(function (r, i) {
        var failed = r.result !== "success" || (r.status_code || 0) >= 400;
        return [fmtMin(r.created_at), E(r.username || "—"), code(r.ip, 18), E(r.method), code(r.path, 48), tag(r.resource || "—", "x-blue"),
          E(r.action || "—"), failed ? tag("失败 " + (r.status_code || ""), "x-red") : tag("成功", "x-green"), E((r.duration_ms || 0) + " ms"),
          '<button class="x-btn" type="button" data-row="' + i + '">详情</button>'];
      });
      return panelHTML("审计记录", tbl(["时间", "操作人", "IP", "方法", "路径", "资源", "动作", "结果", "耗时", "操作"], body),
        '<span class="x-frac" style="margin-left:auto">第 ' + st.page + " 页 · 每页 " + st.size + " 条</span>") +
        '<div class="x-tbar" style="justify-content:flex-end">' +
        '<button class="x-btn" type="button" data-prev' + (st.page <= 1 ? " disabled" : "") + '>上一页</button>' +
        '<button class="x-btn" type="button" data-next' + (st.page * st.size >= total ? " disabled" : "") + '>下一页</button></div>';
    }
    function paint() {
      p.paint(null, filters() + metrics() + table());
      var box = p.box;
      var f = box.querySelector("[data-f=resource]"), u = box.querySelector("[data-f=username]"), r = box.querySelector("[data-f=result]");
      if (f) f.onchange = function () { st.resource = f.value; st.page = 1; load(); };
      if (r) r.onchange = function () { st.result = r.value; st.page = 1; load(); };
      var apply = box.querySelector("[data-apply]");
      if (apply) apply.onclick = function () { st.username = (u && u.value || "").trim(); st.page = 1; load(); };
      var reset = box.querySelector("[data-reset]");
      if (reset) reset.onclick = function () { st = { page: 1, size: 50, resource: "", username: "", result: "" }; load(); };
      var prev = box.querySelector("[data-prev]"), next = box.querySelector("[data-next]");
      if (prev) prev.onclick = function () { if (st.page > 1) { st.page--; load(); } };
      if (next) next.onclick = function () { if (st.page * st.size < total) { st.page++; load(); } };
      [].slice.call(box.querySelectorAll("[data-row]")).forEach(function (b) {
        b.onclick = function () { var rec = rows[+b.getAttribute("data-row")] || {}; infoModal("审计记录 #" + (rec.id || ""), preBlock(rec)); };
      });
    }
    function load() {
      var token = p.newToken();
      var q = { page: st.page, page_size: st.size };
      if (st.resource) q.resource = st.resource;
      if (st.username) q.username = st.username;
      if (st.result) q.result = st.result;
      p.box.innerHTML = loadingHTML("正在读取审计记录…");
      GET("/audit-logs", q).then(function (data) {
        if (token !== p.token) return;
        rows = asArray(data, ["items"]);
        total = num(data && data.total != null ? data.total : rows.length);
        paint();
      }).catch(function (err) { p.error(token, err); });
    }
    load();
  }

  var RENDERERS = {
    observe: renderObserveLive,
    logs: renderLogsLive,
    cicd: renderCicdLive,
    delivery: renderDeliveryLive,
    release: renderReleaseLive,
    middleware: renderMiddlewareLive,
    oncall: renderOncallLive,
    settings: renderSettingsLive,
    ai: renderAiLive,
    audit: renderAuditLive,
    appstatus: renderDeliveryLive
  };
  function install() {
    if (!live()) return false;
    if (ST.installed) return true;
    try {
      if (!helpersReady()) return false;
      var patched = 0;
      ROUTE_KEYS.forEach(function (key) {
        var fn = RENDERERS[key];
        if (!fn) return;
        var r = routes[key];
        if (!r || typeof r.render !== "function") return;
        // 保留原演示渲染器（AI chat/history 子页需要回退到它）
        if (!ST.originals[key]) ST.originals[key] = r.__xsLiveOrig || r.render;
        if (r.__xlopsPatched) return;
        r.__xlopsPatched = true;
        var useShared = !!(SHARED && typeof SHARED.registerRoute === "function");
        if (useShared) {
          // 复用 xs-live-core 的共享注册表：live 时 patchRoute，demo 时保留原渲染器
          try { SHARED.registerRoute(key, fn); } catch (e) { useShared = false; }
        }
        if (!useShared) {
          var orig = r.render;
          r.render = function (el) { return live() ? fn(el) : (orig ? orig(el) : undefined); };
        }
        if (routes[key].render !== ST.originals[key]) patched++;
      });
      ST.installed = patched > 0;
      return ST.installed;
    } catch (e) {
      console.error("[xs-live-ops] install", e);
      return false;
    }
  }
  function repaint() {
    try {
      var auth = q("auth");
      if (auth && !auth.hidden) return;
      var h = opsHash();
      if (ROUTE_KEYS.indexOf(h) >= 0 || h === "automation") { if (typeof render === "function") render(); }
    } catch (e) {}
  }
  function setMode(m) { MODE = m; updateBadge(); }

  // 兼容脚本加载顺序：即便本文件在主内联脚本之前被引入，也会在 DOMContentLoaded / load / 轮询中重试安装。
  function installWithRetry() {
    if (install()) { setTimeout(repaint, 0); return; }
    var tries = 0;
    var timer = setInterval(function () {
      tries++;
      if (install()) { clearInterval(timer); setTimeout(repaint, 0); return; }
      if (tries >= 50) clearInterval(timer);
    }, 200);
    try {
      window.addEventListener("DOMContentLoaded", function () { if (install()) setTimeout(repaint, 0); });
      window.addEventListener("load", function () { if (install()) setTimeout(repaint, 0); });
    } catch (e) {}
  }
  function boot() {
    injectCSS();
    var x = window.XS;
    if (!x || !x.ready || typeof x.ready.then !== "function") { setMode("demo"); bindHashBadge(); return; }
    if (x.on) {
      x.on("mode", function (m) {
        setMode(m === "live" ? "live" : "demo");
        if (m === "live") installWithRetry();
      });
    }
    x.ready.then(function (m) {
      setMode(m === "live" ? "live" : "demo");
      if (m === "live") installWithRetry();
    }).catch(function () { setMode("demo"); });
    bindHashBadge();
  }
  function bindHashBadge() {
    try { window.addEventListener("hashchange", updateBadge); } catch (e) {}
    updateBadge();
  }

  // 自动化编排单独暴露：Lead 在 index.html 加 routes.automation = { render: function (el) { return window.XSLiveOpsAutomation(el); } } 与左侧菜单。
  function renderAutomationLive(el) {
    var p = livePage(el, "自动化编排", "版本化流程定义与受控执行记录", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    if (!live()) {
      p.paint(p.newToken(), '<div class="x-panel">' + banner("当前为演示模式：自动化编排需要连接后端后才可用。") + emptyHTML("未连接后端") + "</div>");
      return;
    }
    el.querySelector("[data-refresh]").onclick = function () { renderAutomationLive(el); };
    var WFS = [];
    function load() {
      var t = p.newToken();
      Promise.all([
        GET("/automation/workflows", { page_size: 100 }),
        GET("/automation/runs", { page_size: 50 })
      ]).then(function (rs) {
        var w = rs[0] || {}, runs = rs[1] || {};
        WFS = Array.isArray(w.items) ? w.items : asArray(w, ["workflows"]);
        var runList = Array.isArray(runs.items) ? runs.items : asArray(runs, ["runs"]);
        var wfTable = WFS.length ? tbl(["ID", "名称", "触发方式", "状态", "版本", "更新时间", "操作"], WFS.map(function (wf) {
          return [mono(wf.id), E(wf.name), E(wf.trigger || "—"), stTag(wf.status || "active"), E(wf.current_version != null ? "v" + wf.current_version : "—"), fmtMin(wf.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-wftrig="' + E(wf.id) + '">触发</button><button class="x-btn" type="button" data-wfver="' + E(wf.id) + '">新版本</button></span>'];
        })) : emptyHTML("暂无流程定义。");
        var runTable = runList.length ? tbl(["运行", "流程", "版本", "状态", "触发人", "开始", "结束", "错误", "操作"], runList.map(function (r) {
          return [mono(r.id), E(r.workflow_id), E(r.version != null ? "v" + r.version : "—"), stTag(r.status), E(r.triggered_by || "—"), fmtMin(r.started_at), fmtMin(r.finished_at), code(r.error || "—", 60),
            '<span class="ops-nowrap">' + (r.status === "failed" ? '<button class="x-btn" type="button" data-runretry="' + E(r.id) + '">重试</button>' : "") +
            (r.status === "queued" || r.status === "running" ? '<button class="x-btn" type="button" data-runcancel="' + E(r.id) + '">取消</button>' : "") + "</span>"];
        })) : emptyHTML("暂无执行记录。");
        var html = panelHTML("流程定义", wfTable, '<button class="x-btn x-btn--pri" type="button" data-wfnew style="margin-left:auto">+ 新建流程</button>') + panelHTML("执行记录", runTable);
        if (!p.paint(t, html)) return;
        bind(runList);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function bind(runList) {
      var nw = p.box.querySelector("[data-wfnew]");
      if (nw) nw.onclick = function () { wfModal(function () { load(); }); };
      [].slice.call(p.box.querySelectorAll("[data-wftrig]")).forEach(function (b) {
        b.onclick = function () {
          confirmDo("触发流程", "确认按当前版本创建一次受控执行？", "触发", function () {
            write(POST("/automation/workflows/" + encodeURIComponent(b.getAttribute("data-wftrig")) + "/trigger"), "已触发，执行记录为 queued").then(function (r) { if (r) load(); });
          });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-wfver]")).forEach(function (b) {
        b.onclick = function () { versionModal(b.getAttribute("data-wfver"), function () { load(); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-runretry]")).forEach(function (b) {
        b.onclick = function () { write(POST("/automation/runs/" + encodeURIComponent(b.getAttribute("data-runretry")) + "/retry"), "已重新排队").then(function (r) { if (r) load(); }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-runcancel]")).forEach(function (b) {
        b.onclick = function () { write(POST("/automation/runs/" + encodeURIComponent(b.getAttribute("data-runcancel")) + "/cancel"), "已取消").then(function (r) { if (r) load(); }); };
      });
    }
    function wfModal(done) {
      formModal("新建流程", [
        { k: "name", label: "名称", required: true, placeholder: "夜间巡检" },
        { k: "description", label: "描述", kind: "textarea", rows: 3 },
        { k: "trigger", label: "触发方式", kind: "select", options: [{ v: "manual", t: "手动" }, { v: "schedule", t: "定时" }, { v: "event", t: "事件" }], value: "manual" },
        { k: "steps", label: "步骤 JSON（受控步骤白名单，不能执行任意 Shell）", kind: "textarea", rows: 6, required: true, value: '[\n  {"type": "noop", "name": "开始"}\n]' }
      ], "创建", function (v) {
        var steps;
        try { steps = parseJSON(v.steps, []); } catch (err) { toast(errMsg(err)); return; }
        write(POST("/automation/workflows", { name: v.name, description: v.description, trigger: v.trigger, steps: steps }), "流程已创建（v1）").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    function versionModal(id, done) {
      formModal("创建新版本", [{ k: "steps", label: "步骤 JSON", kind: "textarea", rows: 8, required: true, value: "[]" }], "创建版本", function (v) {
        var steps;
        try { steps = parseJSON(v.steps, []); } catch (err) { toast(errMsg(err)); return; }
        write(POST("/automation/workflows/" + encodeURIComponent(id) + "/versions", { steps: steps }), "新版本已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }

  // 与 xs-live-core.js 的共享命名空间对接：登记版本、复用徽标/路由注册表；
  // 其余脚本可通过 window.XS_LIVE.routes 看到本文件接管的路由键。
  if (SHARED) {
    try {
      SHARED.files = SHARED.files || {};
      SHARED.files["xs-live-ops.js"] = VERSION;
      SHARED.routes = SHARED.routes || {};
      Object.keys(RENDERERS).forEach(function (k) { if (!SHARED.routes[k]) SHARED.routes[k] = RENDERERS[k]; });
      if (typeof SHARED.updateBadge === "function") SHARED.updateBadge(MODE === "live" ? "live" : "demo");
    } catch (e) {}
  }
  window.XSLiveOpsAutomation = renderAutomationLive;
  window.XSLiveOps = {
    version: VERSION,
    live: live,
    mode: function () { return MODE; },
    install: install,
    repaint: repaint,
    renderers: RENDERERS,
    automation: renderAutomationLive,
    _internals: {
      fmtTime: fmtTime, fmtMin: fmtMin, ago: ago, dur: dur, num: num, pct: pct,
      promHTML: promHTML, spark: spark, promLabels: promLabels,
      asArray: asArray, pick: pick, parseJSON: parseJSON, autoSummary: autoSummary,
      alertKey: alertKey, claimPayload: claimPayload,
      sqlRisk: sqlRisk, redisRisk: redisRisk, cellHTML: cellHTML,
      toISO: toISO, dtLocal: dtLocal, memberList: memberList,
      tabsHTML: tabsHTML, stTag: stTag, sevTag: sevTag, fieldHTML: fieldHTML,
      routes: ROUTE_KEYS, renderers: RENDERERS
    }
  };
  try { boot(); } catch (e) { console.error("[xs-live-ops] boot", e); }
})();
