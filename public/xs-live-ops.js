/* 星枢 Matrix — 运维页实时数据层
 * 覆盖：可观测（数据源 / PromQL / 告警规则 / 告警认领关闭 / 静默 / 通知渠道）、日志检索、
 *       CI-CD（应用 / 流水线 / 运行与 SSE 日志 / 部署回滚 / Jenkins）、中间件（实例 / MQ 概览 /
 *       SQL / Redis 控制台风险提示）、值班（排班 / 故障 / SLO）、设置、AI（模型 / 提示词）、
 *       自动化编排（window.XSLiveOpsAutomation，供 Lead 加路由/菜单）。
 * 引入方式：index.html 主内联脚本之后追加 <script src="xs-live-ops.js"></script>。
 * 只有 window.XS 探测到后端（XS.mode === "live"）时才接管 routes.<key>.render；
 * 后端不可用时保留原页面结构，并显示后端连接状态。
 * 依赖主脚本的全局绑定：routes / render / navGo / phead / tbl / opt / toast / $ / esc /
 * dlg / closeAll / k8Confirm / aiBtn（均为 classic script 的全局词法绑定或 window 函数）。
 */
(function () {
  "use strict";

  var VERSION = "1.0.0";
  var SHARED = (typeof window !== "undefined" && window.XS_LIVE && typeof window.XS_LIVE === "object") ? window.XS_LIVE : null;
  var MODE = "unknown";
  var ST = { installed: false, seq: 0, originals: {}, badge: null, formSeq: 0, runStream: null, refresh: null };
  var ROUTE_KEYS = ["cicd", "delivery", "release", "middleware", "oncall", "settings", "appstatus", "audit", "testing"];
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
  function parseJenkinsParameters(text) {
    var out = {};
    String(text || "").split(/\r?\n/).forEach(function (line, index) {
      line = line.trim();
      if (!line) return;
      var splitAt = line.indexOf("=");
      if (splitAt < 1) throw new Error("第 " + (index + 1) + " 行应为 参数名=参数值");
      var key = line.slice(0, splitAt).trim(), value = line.slice(splitAt + 1).trim();
      if (!/^[A-Za-z_][A-Za-z0-9_.-]{0,127}$/.test(key)) throw new Error("第 " + (index + 1) + " 行参数名无效：" + key);
      if (Object.prototype.hasOwnProperty.call(out, key)) throw new Error("Jenkins 参数重复：" + key);
      if (value !== "") out[key] = value;
    });
    return out;
  }
  function buildPipelineStages(rows, envNames, jenkinsIds) {
    if (!Array.isArray(rows) || !rows.length) throw new Error("至少配置一个流水线阶段");
    if (rows.length > 12) throw new Error("最多配置 12 个阶段");
    var names = {};
    return rows.map(function (row, index) {
      var name = String(row.name || "").trim(), type = String(row.type || "").trim();
      if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name) || name.length > 40) throw new Error("第 " + (index + 1) + " 个阶段名称须为不超过 40 位的小写 DNS 标签");
      if (names[name]) throw new Error("阶段名称重复：" + name);
      names[name] = true;
      var stage = { name: name, type: type }, timeout = Number(row.timeout_sec || 0);
      if (["build", "test", "script"].indexOf(type) >= 0 && timeout) {
        if (!Number.isInteger(timeout) || timeout < 1 || timeout > 86400) throw new Error(name + " 的超时须为 1 到 86400 秒");
        stage.timeout_sec = timeout;
      }
      if (type === "build") {
        if (row.image) stage.image = String(row.image).trim();
        if (row.registry_secret) stage.registry_secret = String(row.registry_secret).trim();
      } else if (type === "test" || type === "script") {
        stage.image = String(row.image || "").trim();
        stage.commands = String(row.commands || "").split(/\r?\n/).map(function (command) { return command.trim(); }).filter(Boolean);
        if (!stage.image || !stage.commands.length) throw new Error(name + " 需要容器镜像和至少一条执行命令");
      } else if (type === "deploy") {
        stage.deploy_env = String(row.deploy_env || "").trim();
        if (!stage.deploy_env || envNames.indexOf(stage.deploy_env) < 0) throw new Error(name + " 必须选择当前应用已配置的部署环境");
      } else if (type === "jenkins") {
        stage.jenkins_id = Number(row.jenkins_id || 0);
        stage.jenkins_job = String(row.jenkins_job || "").trim();
        if (!stage.jenkins_id || jenkinsIds.indexOf(stage.jenkins_id) < 0 || !stage.jenkins_job) throw new Error(name + " 需要已接入的 Jenkins 和 Job 名称");
        stage.params = parseJenkinsParameters(row.params_text || "");
      } else {
        throw new Error(name + " 的阶段类型不受支持");
      }
      return stage;
    });
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
    s.textContent = ".ops-json{max-height:55vh;overflow:auto;margin:0;white-space:pre-wrap;word-break:break-word}.ops-sub{color:var(--x-sub);font-size:12px;margin:8px 0 0}.ops-nowrap{white-space:nowrap}.ops-ro{max-height:60vh;overflow:auto;margin:0}" +
      ".ops-stage{min-width:0;margin:0;padding:12px;border:1px solid var(--x-line);border-radius:6px}.ops-stage+.ops-stage{margin-top:10px}.ops-stage-head{display:flex;align-items:center;gap:8px;margin-bottom:10px}.ops-stage-head strong{font-size:13px}.ops-stage-head .x-btn{margin-left:auto}.ops-stage-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px 12px}.ops-stage-grid>label{display:grid;min-width:0;gap:5px;font-size:12px;color:var(--x-sub)}.ops-stage-grid .ops-stage-wide{grid-column:1/-1}.ops-stage-grid textarea{min-height:72px;resize:vertical}.ops-stage-actions{display:flex;justify-content:flex-start;margin-top:10px}.ops-jenkins-job{display:flex;gap:6px;min-width:0}.ops-jenkins-job .x-input{flex:1;min-width:0}.ops-jenkins-schema{grid-column:1/-1;margin:0;color:var(--x-sub);font-size:12px}.ops-jenkins-schema summary{cursor:pointer}";
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
      b.innerHTML = "<b>后端未连接</b>   当前页面未读取实时数据";
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
  function livePage(el, title, desc, tools, tabs) {
    var p = { el: el, box: null, token: 0, seq: ++ST.seq, retry: null };
    el.innerHTML = phead(E(title), desc || "", tools || "") + (tabs || "");
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
    if (seg === "alerts") return "events";
    return ["metrics", "rules", "events", "channels"].indexOf(seg) >= 0 ? seg : "metrics";
  }
  function alertDeepLink() {
    var parts = (location.hash || "").split("?")[0].split("/");
    if ((parts[2] !== "alerts" && parts[2] !== "events") || !parts[3]) return "";
    var id;
    try { id = decodeURIComponent(parts[3]); } catch (e) { id = parts[3]; }
    history.replaceState(null, "", "#/observe/events");
    return id;
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
    if (pg === "events") return obsEvents(el, alertDeepLink());
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
      var opts = proms.map(function (d) { return { v: d.id, t: (d.name || ("#" + d.id)) + (d.env ? "   " + d.env : "") }; });
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
            if (r && r.ok) toast("连接成功" + (r.latency_ms != null ? "（" + r.latency_ms + " ms）" : "") + (r.info && r.info.version ? "   " + r.info.version : ""));
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
    var p = livePage(el, "告警规则", "Prometheus 告警 / 记录规则与状态", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-rulenew disabled>+ 新建规则</button>', tabsHTML(OBS_TABS, "#/observe/rules"));
    var PROMS = [], CLUSTERS = [];
    function load() {
      var t = p.newToken();
      Promise.all([settle(dsList({ type: "prometheus" }), []), settle(GET("/observability/aiops/config"), null), settle(GET("/clusters?page_size=500"), [])]).then(function (result) {
        var data = result[0].value, config = result[1].value;
        CLUSTERS = result[2].ok ? asArray(result[2].value, ["items", "clusters"]) : [];
        var policyPanel = panelHTML("Kubernetes 健康巡检与 AI 诊断", config ? '<p class="ops-sub">自动巡检：' + (config.enabled ? "已启用" : "未启用") +
          '   自动 AI 分析：' + (config.auto_diagnose ? "已启用" : "未启用") + '   巡检间隔：' + E(config.interval_sec || 60) +
          ' 秒</p><p class="ops-sub">检测节点就绪 / 资源压力、Pod 启动异常、控制器不可用和 PVC 未绑定；处置由运维选择，生产变更走环境审批。</p>' :
          banner("AIOps 配置读取失败：" + errMsg(result[1].error)),
          config ? '<button class="x-btn" type="button" data-aiops-policy style="margin-left:auto">配置</button>' : "");
        function bindPolicy() {
          var button = p.box.querySelector("[data-aiops-policy]");
          if (button) button.onclick = function () { aiopsPolicyModal(config, load); };
        }
        var create = el.querySelector("[data-rulenew]");
        if (create) {
          create.disabled = !CLUSTERS.length;
          create.title = CLUSTERS.length ? "" : "请先接入 Kubernetes 集群";
          create.onclick = function () { newAlertRule(); };
        }
        PROMS = Array.isArray(data) ? data : asArray(data, ["items"]);
        if (!PROMS.length) { if (p.paint(t, panelHTML("告警规则", emptyHTML("未接入 Prometheus 数据源，无法读取告警规则。")) + policyPanel)) bindPolicy(); return; }
        var sel = '<div class="x-tbar"><select class="x-select" data-ruleds style="width:auto">' + PROMS.map(function (d) { return '<option value="' + E(d.id) + '">' + E(d.name || ("#" + d.id)) + "   " + E(d.env || "*") + "</option>"; }).join("") + '</select><span class="x-frac" data-rulemeta></span></div>';
        if (!p.paint(t, panelHTML("告警规则", sel + '<div data-ruleres>' + loadingHTML("读取规则中…") + "</div>") + policyPanel)) return;
        bindPolicy();
        var s = p.box.querySelector("[data-ruleds]");
        s.onchange = function () { loadRules(s.value); };
        loadRules(PROMS[0].id);
      }).catch(function (err) { p.error(t, err); });
    }
    p.retry = load;
    function newAlertRule() {
      if (!CLUSTERS.length) { toast("请先接入 Kubernetes 集群"); return; }
      formModal("新建告警规则", [
        { k: "cluster_id", label: "目标集群", kind: "select", required: true, value: String(CLUSTERS[0].id), options: CLUSTERS.map(function (c) { return { v: String(c.id), t: (c.name || ("#" + c.id)) + "   " + (c.env || "未标记") }; }) },
        { k: "namespace", label: "PrometheusRule 命名空间", required: true, value: "monitoring", placeholder: "monitoring" },
        { k: "rule_group", label: "规则组", required: true, value: "matrix-ui", placeholder: "matrix-ui" },
        { k: "name", label: "告警名称", required: true, placeholder: "NodeHighCPU" },
        { k: "expr", label: "PromQL 表达式", kind: "textarea", rows: 4, required: true, placeholder: 'sum(rate(node_cpu_seconds_total{mode!="idle"}[5m])) by (instance) > 0.85' },
        { k: "for", label: "持续时间", value: "5m", placeholder: "5m" },
        { k: "severity", label: "级别", kind: "select", value: "warning", options: [{ v: "critical", t: "严重" }, { v: "warning", t: "警告" }, { v: "info", t: "提示" }] },
        { k: "summary", label: "告警摘要", placeholder: "节点 CPU 使用率持续偏高" }
      ], "创建规则", function (v, form, err) {
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(v.name)) {
          err.textContent = "告警名称仅支持字母、数字和下划线，且不能以数字开头";
          err.hidden = false;
          form.elements.name.focus();
          return;
        }
        var body = { namespace: v.namespace, rule_group: v.rule_group, name: v.name, expr: v.expr, for: v.for, severity: v.severity, summary: v.summary };
        write(POST("/clusters/" + encodeURIComponent(v.cluster_id) + "/alert-rules", body), "告警规则已写入目标集群").then(function (result) {
          if (!result) return;
          closeAll();
          load();
        });
      }, "规则将写入所选集群的 monitoring.coreos.com/v1 PrometheusRule；需具备集群管理权限，并由 Prometheus Operator 加载。");
    }
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

  function aiopsPolicyModal(config, done) {
    var url = location.origin + location.pathname.replace(/\/[^/]*$/, "");
    formModal("Kubernetes 健康巡检与 AI 诊断", [
      { k: "enabled", label: "自动巡检已接入集群", kind: "checkbox", value: config.enabled, hint: "启用" },
      { k: "auto_diagnose", label: "自动向已配置模型发送脱敏告警证据", kind: "checkbox", value: config.auto_diagnose, hint: "启用自动 AI 分析" },
      { k: "host_diagnostics", label: "允许 Matrix Agent 采集只读主机诊断", kind: "checkbox", value: config.host_diagnostics, hint: "固定检查系统压力、服务状态、进程名称和近期内核告警；限长脱敏后纳入 AI 证据" },
      { k: "interval_sec", label: "巡检间隔（60–3600 秒）", kind: "number", required: true, value: config.interval_sec || 60 },
      { k: "model_id", label: "模型 ID（0 使用平台默认模型）", kind: "number", value: config.model_id || 0 },
      { k: "public_url", label: "通知回流地址（自动识别当前平台地址）", required: true, value: config.public_url || url }
    ], "保存", function (v) {
      var interval = Number(v.interval_sec), model = Number(v.model_id || 0);
      if (!Number.isInteger(interval) || interval < 60 || interval > 3600 || !Number.isInteger(model) || model < 0) {
        toast("请填写有效的巡检间隔和模型 ID"); return;
      }
      write(PUT("/observability/aiops/config", { enabled: v.enabled, auto_diagnose: v.auto_diagnose, host_diagnostics: v.host_diagnostics, interval_sec: interval, model_id: model, public_url: v.public_url }),
        "AIOps 配置已保存").then(function (r) { if (r) { closeAll(); done(); } });
    }, "主机采集仅运行平台固定的只读命令，结果会限长、脱敏并随诊断证据发送给已配置模型。自动分析只生成建议，不直接执行变更；人工选择 AI 处理后仍需通过权限、资源身份校验和环境审批。");
  }

  // 告警路径 key：优先数字 id（同一 fingerprint 可能存在于多个 Alertmanager 数据源，
  // 用 id 可避免 ?datasource 消歧 409）；claim/close/timeline 都接受 id 或 fingerprint。
  function alertKey(a) { return String(a && a.id != null && a.id !== "" ? a.id : ((a && (a.fingerprint || a.key)) || "")); }
  document.addEventListener("click", function (event) {
    var target = event.target && event.target.nodeType === 1 ? event.target : event.target && event.target.parentElement;
    var link = target && target.closest ? target.closest("[data-alert-open]") : null;
    if (!link) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    var id = link.getAttribute("data-alert-open");
    if (id && typeof window.XSOpenAlertDetail === "function") window.XSOpenAlertDetail(id);
    else toast("告警详情暂不可用，请刷新页面重试");
  }, true);
  function claimPayload(a, comment, duration, silence) {
    var body = { comment: comment || "", duration: duration || "2h", silence: silence !== false };
    if (a && a.datasource_id) body.datasource_id = a.datasource_id;
    return body;
  }

  function obsEvents(el, openAlertId) {
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
        if (openAlertId) {
          var id = openAlertId;
          openAlertId = "";
          if (typeof window.XSOpenAlertDetail === "function") window.XSOpenAlertDetail(id);
          else toast("告警详情暂不可用，请刷新页面重试");
        }
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
        return [stTag(a.status), sevTag(a.severity || labels.severity), '<button class="x-tlink x-alert-open" type="button" data-alert-open="' + E(alertKey(a)) + '">' + E(a.alertname || labels.alertname || "—") + "</button>",
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
          var type = String(c.type || "").toLowerCase();
          var logo = { feishu: "/assets/channels/feishu.svg", lark: "/assets/channels/feishu.svg", dingtalk: "/assets/channels/dingtalk.svg", dingding: "/assets/channels/dingtalk.svg", wecom: "/assets/channels/wecom.png", wechatwork: "/assets/channels/wecom.png", wxwork: "/assets/channels/wecom.png" }[type];
          var name = (logo ? '<img src="' + logo + '" alt="" aria-hidden="true" style="width:24px;height:24px;object-fit:contain;vertical-align:middle;margin-right:8px" />' : "") + E(c.name || "—");
          return [name, tag(c.type || "—", "x-blue"), c.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
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
        '<select class="x-select" data-lgds style="width:auto">' + DSTS.map(function (d) { return '<option value="' + E(d.id) + '"' + (String(d.id) === String(LG.dsId) ? " selected" : "") + ">" + E(d.name) + "   " + E(d.type) + "</option>"; }).join("") + "</select>",
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
      box.innerHTML = '<div class="x-tbar"><span class="ops-sub" style="margin:0">按级别筛选当前查询结果</span>' + chips + '<span class="x-frac" style="margin-left:auto">数据源 ' + E(data.datasource || "—") + "   引擎 " + E(data.engine || "—") + "   命中 " + num(data.total || rows.length) + "</span></div>" +
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
    var p = livePage(el, "构建", "流水线运行记录与实时日志（SSE）", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-runpl>+ 运行流水线</button>', "");
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
      { k: "pipeline", label: "流水线", kind: "select", options: PL.map(function (pl) { return { v: pl.id, t: (pl.name || ("#" + pl.id)) + "   应用 " + appName(pl.app_id) }; }) },
      { k: "branch", label: "分支（留空使用应用默认分支）", placeholder: "main" },
      { k: "commit", label: "提交 SHA（可选）" }
    ], "运行", function (v) {
      write(POST("/cicd/pipelines/" + encodeURIComponent(v.pipeline) + "/run", { branch: v.branch, commit: v.commit }), "已提交运行").then(function (r) { if (r) { closeAll(); if (done) done(); } });
    });
  }

  function renderTestingLive(el) {
    var p = livePage(el, "测试管理", "基于流水线 test 阶段查看真实配置与运行结果",
      '<button class="x-btn" type="button" data-refresh>刷新</button><a class="x-btn x-btn--pri" href="#/cicd/pipeline">管理流水线</a>', "");
    el.querySelector("[data-refresh]").onclick = function () { load(); };
    function load() {
      var token = p.newToken();
      Promise.all([
        appsLoad(),
        GET("/cicd/pipelines"),
        GET("/cicd/runs", { page: 1, page_size: 100 })
      ]).then(function (data) {
        CICD_APPS = data[0] || [];
        CICD_PL = asArray(data[1], ["items", "pipelines"]);
        var allRuns = asArray(data[2], ["items", "runs"]);
        var testPipelines = CICD_PL.filter(function (pipeline) {
          return (pipeline.stages || []).some(function (stage) { return stage.type === "test"; });
        });
        var ids = {};
        testPipelines.forEach(function (pipeline) { ids[String(pipeline.id)] = true; });
        var runs = allRuns.filter(function (run) { return !!ids[String(run.pipeline_id)]; });
        var pipelineRows = testPipelines.map(function (pipeline) {
          var tests = (pipeline.stages || []).filter(function (stage) { return stage.type === "test"; });
          return [mono("#" + pipeline.id), E(appName(pipeline.app_id)), E(pipeline.name),
            E(tests.map(function (stage) { return stage.name; }).join(", ")), E("#" + pipeline.runner_target_id),
            pipeline.on_push ? tag("代码推送", "x-blue") : tag("手动", "x-grey"),
            '<a class="x-btn" href="#/cicd/pipeline">配置</a>'];
        });
        var runRows = runs.map(function (run) {
          return [mono("#" + run.id), E(appName(run.app_id)), E(pipelineName(run.pipeline_id)), stTag(run.status),
            code(run.branch || "—", 20), E(run.triggered_by || "—"), fmtMin(run.started_at || run.created_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-test-run="' + E(run.id) + '">详情</button>' +
            '<button class="x-btn" type="button" data-test-log="' + E(run.id) + '">日志</button></span>'];
        });
        var html = panelHTML("测试流水线", testPipelines.length
          ? tbl(["ID", "应用", "流水线", "测试阶段", "执行集群", "触发方式", "操作"], pipelineRows)
          : emptyHTML("暂无包含 test 阶段的流水线。请在流水线配置中添加测试阶段。")) +
          panelHTML("最近运行", (runs.length ? tbl(["运行", "应用", "流水线", "状态", "分支", "触发人", "开始时间", "操作"], runRows)
            : emptyHTML("最近 100 次运行中没有测试流水线记录。")) +
            '<p class="x-frac">运行记录最多读取最近 100 条；执行与阶段配置统一由 CI/CD 流水线管理。</p>');
        if (!p.paint(token, html)) return;
        p.box.querySelectorAll("[data-test-run]").forEach(function (button) {
          button.onclick = function () { runDetailModal(button.getAttribute("data-test-run")); };
        });
        p.box.querySelectorAll("[data-test-log]").forEach(function (button) {
          button.onclick = function () { openRunLog(button.getAttribute("data-test-log")); };
        });
      }).catch(function (err) { p.error(token, err); });
    }
    p.retry = load;
    load();
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
      dlg("运行详情   #" + E(r.id), '<div class="x-form">' + info + (rows.length ? tbl(["#", "阶段", "类型", "状态", "Job", "开始", "结束", "错误"], rows) : emptyHTML("该运行没有阶段记录")) + "</div>", foot);
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
    dlg("运行日志   #" + E(runId), '<pre class="x-log ops-ro" id="ops-runlog" aria-live="polite" aria-label="运行日志">连接日志流…' + "\n" + "</pre>",
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
          try { var d = JSON.parse(data); setSt("已结束   " + (d.status || "")); } catch (e) { setSt("已结束"); }
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
    var p = livePage(el, "制品", "构建产出的镜像标签（来自运行记录）", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
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
    var routeParts = (location.hash || "").split("/");
    var detailID = routeParts[3] ? decodeURIComponent(routeParts[3].split("?")[0]) : "";
    var p = livePage(el, "流水线", "流水线定义、执行集群与 Jenkins 接入", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newpl>+ 新建流水线</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { cicdPipelines(el); };
    el.querySelector("[data-newpl]").onclick = function () { pipelineModal(function () { cicdPipelines(el); }); };
    function load() {
      var t = p.newToken();
      Promise.all([GET("/cicd/pipelines"), settle(GET("/cicd/targets"), []), settle(GET("/cicd/jenkins"), []), settle(appsLoad(), []), settle(GET("/cicd/argocd"), [])]).then(function (rs) {
        CICD_PL = Array.isArray(rs[0]) ? rs[0] : asArray(rs[0], ["items"]);
        CICD_TG = asArray(rs[1].value, ["items", "targets"]);
        CICD_JK = asArray(rs[2].value, ["items", "jenkins"]);
        CICD_APPS = rs[3].value || [];
        if (detailID) {
          var selected = CICD_PL.filter(function (pl) { return String(pl.id) === String(detailID); })[0];
          if (!selected) {
            p.paint(t, '<p class="x-crumb">应用交付<i>/</i><a class="x-tlink" href="#/cicd/pipeline">流水线</a></p>' +
              panelHTML("流水线", emptyHTML("流水线不存在或已被删除。")));
            return;
          }
          GET("/cicd/runs", { pipeline_id: selected.id, page: 1, page_size: 50 }).then(function (runData) {
            if (t !== p.token) return;
            var runItems = Array.isArray(runData) ? runData : asArray(runData, ["items", "runs"]);
            var newest = runItems[0] || null;
            var detailRequest = newest ? settle(GET("/cicd/runs/" + encodeURIComponent(newest.id)), null) : Promise.resolve({ ok: true, value: null });
            detailRequest.then(function (runResult) {
              var runDetail = runResult.value;
              var stages = Array.isArray(selected.stages) ? selected.stages : [];
              var statusByIndex = {};
              (runDetail && Array.isArray(runDetail.stages) ? runDetail.stages : []).forEach(function (stage) { statusByIndex[stage.idx] = stage; });
              var flow = stages.length
                ? '<p class="x-plflow" aria-label="流水线执行阶段">' + stages.map(function (stage, index) {
                  var observed = statusByIndex[index], cls = observed && observed.status === "skipped" ? "off" : "";
                  return '<b class="' + cls + '">' + E(stage.name || ("阶段 " + (index + 1))) + '</b>';
                }).join('<span aria-hidden="true">→</span>') + "</p>"
                : emptyHTML("此流水线没有阶段配置。");
              var runStages = runDetail && Array.isArray(runDetail.stages) ? runDetail.stages : [];
              var strip = runStages.length
                ? '<ol class="x-plstrip">' + runStages.map(function (stage) {
                  var colors = { succeeded: "#12b76a", running: "#2c66f5", failed: "#f04438", cancelled: "#98a2b3", skipped: "#d0d5dd", rejected: "#f79009", pending: "#d0d5dd" };
                  return '<li><button class="x-plst' + (stage.status === "running" ? " run" : "") + '" style="--c:' + (colors[stage.status] || "#d0d5dd") +
                    '" type="button" data-stage-run="' + E(runDetail.id) + '" aria-label="' + E(stage.name + "：" + stage.status) + '">' +
                    "<b>" + E(stage.name) + "</b><small>" + stTag(stage.status) + (stage.job_name ? "   " + E(stage.job_name) : "") + "</small></button></li>";
                }).join("") + "</ol>"
                : emptyHTML("该流水线暂无运行记录。");
              var runRows = runItems.map(function (run) {
                var active = ["queued", "running", "cancelling"].indexOf(String(run.status)) >= 0;
                return [mono("#" + run.id), stTag(run.status), code(run.branch || "—", 20), code(run.commit || "—", 14),
                  E(run.triggered_by || "—"), fmtMin(run.started_at || run.created_at),
                  '<span class="ops-nowrap"><button class="x-btn" type="button" data-plrund="' + E(run.id) + '">详情</button>' +
                  '<button class="x-btn" type="button" data-plrunlog="' + E(run.id) + '">日志</button>' +
                  (active ? '<button class="x-btn" type="button" data-plruncancel="' + E(run.id) + '">取消</button>' : "") + "</span>"];
              });
              var flowNames = stages.map(function (stage) { return E(stage.name); }).join(" → ");
              var html = '<p class="x-crumb">应用交付<i>/</i><a class="x-tlink" href="#/cicd/pipeline">流水线</a><i>/</i><b>' + E(selected.name) + "</b></p>" +
                '<div class="x-tbar"><a class="x-btn" href="#/cicd/pipeline">返回流水线列表</a><span class="x-grow"></span>' +
                '<button class="x-btn" type="button" data-detail-edit="' + E(selected.id) + '">编辑</button>' +
                '<button class="x-btn" type="button" data-detail-run="' + E(selected.id) + '">运行</button></div>' +
                panelHTML("流水线信息", '<dl class="x-sum" style="margin:0">' +
                  [["应用", E(appName(selected.app_id))], ["执行集群", E(targetName(selected.runner_target_id))],
                    ["触发方式", selected.on_push ? "代码推送" : "手动"], ["阶段", E(stages.length + " 个")],
                    ["最近更新", fmtMin(selected.updated_at)]].map(function (item) { return "<dt>" + item[0] + "</dt><dd>" + item[1] + "</dd>"; }).join("") +
                  "</dl><div class=\"x-sethead\" style=\"margin-top:16px\"><h2 class=\"x-h2\">执行顺序</h2></div>" + flow +
                  (flowNames ? '<p class="x-frac" style="margin-top:8px">' + flowNames + "</p>" : "")) +
                panelHTML("最近运行阶段", runDetail ? '<p class="x-frac">' + mono("#" + runDetail.id) + "   " + stTag(runDetail.status) + "   " + fmtMin(runDetail.started_at || runDetail.created_at) + "</p>" + strip :
                  emptyHTML("该流水线暂无运行记录。")) +
                panelHTML("运行记录", runRows.length ? tbl(["运行", "状态", "分支", "提交", "触发人", "开始时间", "操作"], runRows) :
                  emptyHTML("该流水线暂无运行记录。"));
              if (!p.paint(t, html)) return;
              var edit = p.box.querySelector("[data-detail-edit]");
              if (edit) edit.onclick = function () { pipelineModal(function () { cicdPipelines(el); }, selected); };
              var run = p.box.querySelector("[data-detail-run]");
              if (run) run.onclick = function () { runPipelineModal(function () { cicdPipelines(el); }, [selected]); };
              [].slice.call(p.box.querySelectorAll("[data-stage-run]")).forEach(function (button) {
                button.onclick = function () { runDetailModal(button.getAttribute("data-stage-run")); };
              });
              [].slice.call(p.box.querySelectorAll("[data-plrund]")).forEach(function (button) {
                button.onclick = function () { runDetailModal(button.getAttribute("data-plrund")); };
              });
              [].slice.call(p.box.querySelectorAll("[data-plrunlog]")).forEach(function (button) {
                button.onclick = function () { openRunLog(button.getAttribute("data-plrunlog")); };
              });
              [].slice.call(p.box.querySelectorAll("[data-plruncancel]")).forEach(function (button) {
                button.onclick = function () {
                  var id = button.getAttribute("data-plruncancel");
                  confirmDo("取消运行", "确定取消流水线运行 #" + E(id) + "？", "取消运行", function () {
                    write(POST("/cicd/runs/" + encodeURIComponent(id) + "/cancel"), "已请求取消").then(function (result) { if (result) load(); });
                  });
                };
              });
            });
          }).catch(function (err) { p.error(t, err); });
          return;
        }
        var plRows = CICD_PL.map(function (pl) {
          return [mono("#" + pl.id), '<a class="x-tlink" href="#/cicd/pipeline/' + encodeURIComponent(pl.id) + '">' + E(pl.name || "—") + "</a>", E(appName(pl.app_id)), E(targetName(pl.runner_target_id)), pl.on_push ? tag("代码推送", "x-blue") : tag("手动", "x-grey"),
            E((pl.stages || []).length + " 阶段"), fmtMin(pl.updated_at),
            '<span class="ops-nowrap"><a class="x-btn" href="#/cicd/pipeline/' + encodeURIComponent(pl.id) + '">详情</a><button class="x-btn" type="button" data-pledit="' + E(pl.id) + '">编辑</button><button class="x-btn" type="button" data-plrun="' + E(pl.id) + '">运行</button><button class="x-btn" type="button" data-pldel="' + E(pl.id) + '">删除</button></span>'];
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
            '<button class="x-btn" type="button" data-newjk style="margin-left:auto">+ 接入 Jenkins</button>') +
          panelHTML("Argo CD", rs[4].ok ? argoConnectionsHTML(asArray(rs[4].value, ["items"])) : banner("Argo CD：" + errMsg(rs[4].error)),
            '<button class="x-btn" type="button" data-newargo style="margin-left:auto">+ 接入 Argo CD</button>');
        if (!p.paint(t, html)) return;
        bind();
        bindArgoConnections(p.box, asArray(rs[4].value, ["items"]), load);
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
      [].slice.call(p.box.querySelectorAll("[data-pledit]")).forEach(function (b) {
        b.onclick = function () {
          var id = b.getAttribute("data-pledit"), pl = null;
          for (var i = 0; i < CICD_PL.length; i++) if (String(CICD_PL[i].id) === String(id)) pl = CICD_PL[i];
          if (pl) pipelineModal(function () { cicdPipelines(el); }, pl);
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
    function pipelineModal(done, existing) {
      if (!CICD_APPS.length) { toast("请先在「应用」页创建应用"); return; }
      if (!CICD_TG.length) { toast("请先接入执行集群"); return; }
      var id = "opspipe" + (++ST.formSeq), stageTypes = [
        { v: "build", t: "构建镜像" }, { v: "test", t: "测试" }, { v: "script", t: "脚本" },
        { v: "deploy", t: "部署" }, { v: "jenkins", t: "Jenkins Job" }
      ];
      var stages = existing && Array.isArray(existing.stages) && existing.stages.length
        ? existing.stages.map(function (s) {
          return Object.assign({}, s, {
            commands: (s.commands || []).join("\n"),
            params_text: Object.keys(s.params || {}).map(function (k) { return k + "=" + s.params[k]; }).join("\n")
          });
        })
        : [{ name: "unit-test", type: "test", image: "golang:1.23", commands: "go test ./..." }, { name: "build", type: "build", registry_secret: "" }];
      var appOptions = CICD_APPS.map(function (a) { return { v: a.id, t: a.name }; });
      var targetOptions = CICD_TG.map(function (t) { return { v: t.id, t: t.name + " / " + t.namespace }; });
      function options(items, selected, empty) {
        if (!items.length) return '<option value="">' + E(empty || "暂无可选项") + "</option>";
        return items.map(function (o) {
          return '<option value="' + E(o.v) + '"' + (String(o.v) === String(selected) ? " selected" : "") + ">" + E(o.t) + "</option>";
        }).join("");
      }
      var selectedApp = existing ? existing.app_id : CICD_APPS[0].id;
      var selectedTarget = existing ? existing.runner_target_id : CICD_TG[0].id;
      var formHTML = '<form class="x-form" id="' + id + '" novalidate>' +
        '<label><span>流水线名称</span><input class="x-input" name="name" maxlength="64" required value="' + E(existing && existing.name || "") + '" placeholder="例如 release-main" /></label>' +
        '<label><span>应用</span><select class="x-select" name="app">' + options(appOptions, selectedApp) + "</select></label>" +
        '<label><span>执行集群</span><select class="x-select" name="target">' + options(targetOptions, selectedTarget) + "</select></label>" +
        '<label><span>代码推送时自动触发</span><span class="x-check"><input type="checkbox" name="on_push"' + (existing ? (existing.on_push ? " checked" : "") : " checked") + ' style="width:auto" /> 启用</span></label>' +
        '<section><div class="x-sethead"><h2 class="x-h2">执行阶段</h2><button class="x-btn" type="button" data-stage-add>添加阶段</button></div><div data-stage-list></div></section>' +
        '<p class="ops-sub">Jenkins Job 可读取参数定义并填入默认值；留空参数不覆盖 Job 默认值。支持 ${BRANCH}、${COMMIT}、${IMAGE_TAG}、${IMAGE}、${RUN_ID}。</p>' +
        '<span class="x-err" id="' + id + 'err" role="alert" hidden></span></form>';
      dlg(existing ? "编辑流水线" : "新建流水线", formHTML,
        '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="' + id + '">' + (existing ? "保存" : "创建") + "</button>");
      var form = q(id), err = q(id + "err"), stageList = form && form.querySelector("[data-stage-list]");
      if (!form || !stageList) return;
      function currentApp() {
        for (var i = 0; i < CICD_APPS.length; i++) if (String(CICD_APPS[i].id) === String(form.elements.app.value)) return CICD_APPS[i];
        return null;
      }
      function readStages() {
        stages = [].slice.call(stageList.querySelectorAll("[data-stage-row]")).map(function (row) {
          var data = {};
          [].slice.call(row.querySelectorAll("[data-stage-field]")).forEach(function (field) { data[field.getAttribute("data-stage-field")] = field.value; });
          return data;
        });
      }
      function field(label, key, value, placeholder, type) {
        return '<label><span>' + E(label) + '</span><input class="x-input" data-stage-field="' + E(key) + '" type="' + (type || "text") + '" value="' + E(value || "") + '" placeholder="' + E(placeholder || "") + '"' + (type === "number" ? ' min="0" max="86400" step="1"' : "") + " /></label>";
      }
      function selectField(label, key, items, selected, empty) {
        return '<label><span>' + E(label) + '</span><select class="x-select" data-stage-field="' + E(key) + '">' + options(items, selected, empty) + "</select></label>";
      }
      function stageBody(stage) {
        var app = currentApp(), envs = app && Array.isArray(app.envs) ? app.envs.map(function (env) { return { v: env.name, t: env.name + " / " + env.namespace }; }) : [];
        if (stage.type === "build") return field("镜像覆盖（可选）", "image", stage.image, "留空使用应用镜像仓库") +
          field("镜像仓库 Secret（可选）", "registry_secret", stage.registry_secret, "如 harbor-push") + field("超时秒数", "timeout_sec", stage.timeout_sec, "默认超时", "number");
        if (stage.type === "test" || stage.type === "script") return field("运行镜像", "image", stage.image, "如 golang:1.23", "text") +
          '<label class="ops-stage-wide"><span>执行命令（每行一条）</span><textarea class="x-input" data-stage-field="commands" rows="3" placeholder="go test ./...">' + E(stage.commands || "") + "</textarea></label>" +
          field("超时秒数", "timeout_sec", stage.timeout_sec, "默认超时", "number");
        if (stage.type === "deploy") return selectField("部署环境", "deploy_env", envs, stage.deploy_env, "当前应用没有可部署环境");
        if (stage.type === "jenkins") {
          var jenkinsOptions = CICD_JK.map(function (j) { return { v: j.id, t: j.name + "   " + j.url }; });
          return selectField("Jenkins", "jenkins_id", jenkinsOptions, stage.jenkins_id, "请先接入 Jenkins") +
            '<label><span>Job 名称</span><div class="ops-jenkins-job"><input class="x-input" data-stage-field="jenkins_job" value="' + E(stage.jenkins_job || "") + '" placeholder="folder/job-name" /><button class="x-btn" type="button" data-jenkins-discover>读取参数</button></div></label>' +
            '<label class="ops-stage-wide"><span>Job 参数（每行 name=value）</span><textarea class="x-input" data-stage-field="params_text" rows="3" placeholder="BRANCH=${BRANCH}">' + E(stage.params_text || "") + "</textarea></label>";
        }
        return "";
      }
      function renderStages() {
        stageList.innerHTML = stages.map(function (stage, index) {
          return '<fieldset class="ops-stage" data-stage-row><div class="ops-stage-head"><strong>阶段 ' + (index + 1) + "</strong>" +
            '<button class="x-btn" type="button" data-stage-remove="' + index + '" aria-label="移除阶段 ' + (index + 1) + '">移除</button></div>' +
            '<div class="ops-stage-grid">' + field("阶段标识", "name", stage.name, "如 unit-test") +
            selectField("执行类型", "type", stageTypes, stage.type) + stageBody(stage) + "</div></fieldset>";
        }).join("");
      }
      stageList.addEventListener("change", function (event) {
        var fieldNode = event.target.closest("[data-stage-field]");
        if (!fieldNode) return;
        if (fieldNode.getAttribute("data-stage-field") === "jenkins_id") {
          var parent = fieldNode.closest("[data-stage-row]"), schema = parent && parent.querySelector(".ops-jenkins-schema");
          if (schema) schema.remove();
          return;
        }
        if (fieldNode.getAttribute("data-stage-field") === "type") {
          readStages();
          var rows = [].slice.call(stageList.querySelectorAll("[data-stage-row]")), index = rows.indexOf(fieldNode.closest("[data-stage-row]"));
          if (index >= 0 && stages[index] && !stages[index].name) stages[index].name = "stage-" + (index + 1);
          renderStages();
        }
      });
      stageList.addEventListener("input", function (event) {
        if (event.target.matches('[data-stage-field="jenkins_job"]')) {
          var row = event.target.closest("[data-stage-row]"), schema = row && row.querySelector(".ops-jenkins-schema");
          if (schema) schema.remove();
        }
      });
      stageList.addEventListener("click", function (event) {
        var discover = event.target.closest("[data-jenkins-discover]");
        if (discover) {
          var row = discover.closest("[data-stage-row]");
          if (!row) return;
          var idField = row.querySelector('[data-stage-field="jenkins_id"]');
          var jobField = row.querySelector('[data-stage-field="jenkins_job"]');
          var paramsField = row.querySelector('[data-stage-field="params_text"]');
          var jenkinsId = String(idField && idField.value || ""), job = String(jobField && jobField.value || "").trim();
          if (!jenkinsId || !job) { toast("请选择 Jenkins 并填写 Job 名称"); return; }
          discover.disabled = true;
          discover.textContent = "读取中…";
          GET("/cicd/jenkins/" + encodeURIComponent(jenkinsId) + "/job-parameters", { job: job }).then(function (data) {
            if (!stageList.contains(row)) return;
            var definitions = asArray(data, ["parameters"]);
            var values;
            try { values = parseJenkinsParameters(paramsField.value); }
            catch (error) { toast(error.message || "现有 Jenkins 参数格式错误"); return; }
            definitions.forEach(function (definition) {
              if (Object.prototype.hasOwnProperty.call(values, definition.name) || definition.sensitive) return;
              var value = definition.default_value;
              if (value == null || /[\r\n]/.test(String(value))) value = "";
              values[definition.name] = String(value);
            });
            paramsField.value = Object.keys(values).map(function (key) { return key + "=" + values[key]; }).join("\n");
            var descriptions = definitions.map(function (definition) {
              var detail = [definition.type || "参数", definition.description || ""].filter(Boolean).join("   ");
              if (definition.choices && definition.choices.length) detail += (detail ? "   " : "") + "可选值：" + definition.choices.join(" / ");
              if (definition.sensitive) detail += (detail ? "   " : "") + "敏感参数，默认值未读取";
              return "<li><span class=\"x-name\">" + E(definition.name) + "</span>   " + E(detail) + "</li>";
            }).join("");
            var old = row.querySelector(".ops-jenkins-schema");
            if (old) old.remove();
            var report = document.createElement("details");
            report.className = "ops-jenkins-schema";
            report.innerHTML = "<summary>已发现 " + definitions.length + " 个 Job 参数</summary><ul>" + descriptions + "</ul>";
            paramsField.parentNode.insertAdjacentElement("afterend", report);
          }).catch(function (error) { toast("读取 Jenkins 参数失败：" + errMsg(error)); })
            .finally(function () { if (discover.isConnected) { discover.disabled = false; discover.textContent = "读取参数"; } });
          return;
        }
        var remove = event.target.closest("[data-stage-remove]");
        if (!remove) return;
        readStages();
        stages.splice(Number(remove.getAttribute("data-stage-remove")), 1);
        renderStages();
      });
      form.querySelector("[data-stage-add]").onclick = function () {
        readStages();
        if (stages.length >= 12) { err.textContent = "最多配置 12 个阶段"; err.hidden = false; return; }
        var next = stages.length + 1;
        stages.push({ name: "stage-" + next, type: "test", image: "", commands: "", timeout_sec: "" });
        err.hidden = true;
        renderStages();
      };
      form.elements.app.onchange = function () { readStages(); renderStages(); };
      form.onsubmit = function (event) {
        event.preventDefault();
        err.hidden = true;
        readStages();
        var app = currentApp(), body;
        try {
          var name = String(form.elements.name.value || "").trim();
          if (!name || name.length > 64) throw new Error("流水线名称为必填，且不能超过 64 个字符");
          if (!app) throw new Error("请选择有效的应用");
          body = {
            name: name,
            app_id: Number(app.id),
            runner_target_id: Number(form.elements.target.value),
            on_push: !!form.elements.on_push.checked,
            stages: buildPipelineStages(stages, (app.envs || []).map(function (env) { return env.name; }), CICD_JK.map(function (j) { return Number(j.id); }))
          };
        } catch (error) {
          err.textContent = error.message || "流水线配置无效";
          err.hidden = false;
          return;
        }
        var submit = document.querySelector('button[form="' + id + '"][type="submit"]');
        if (submit) { submit.disabled = true; submit.textContent = "保存中…"; }
        var request = existing
          ? PUT("/cicd/pipelines/" + encodeURIComponent(existing.id), body)
          : POST("/cicd/pipelines", body);
        write(request, existing ? "流水线已更新" : "流水线已创建").then(function (result) {
          if (submit) submit.disabled = false;
          if (result) { closeAll(); if (done) done(); }
        });
      };
      renderStages();
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

  function argoConnectionsHTML(configs) {
    return configs.length ? tbl(["名称", "地址", "环境", "项目", "操作"], configs.map(function (c) {
      return [E(c.name), code(c.url, 60), E(c.env), E(c.project), '<span class="ops-nowrap">' +
        '<button class="x-btn" type="button" data-argo-apps="' + E(c.id) + '">应用</button>' +
        '<button class="x-btn" type="button" data-argo-test="' + E(c.id) + '">测试连接</button>' +
        '<button class="x-btn" type="button" data-argo-edit="' + E(c.id) + '">编辑</button>' +
        '<button class="x-btn" type="button" data-argo-delete="' + E(c.id) + '">移除</button></span>'];
    })) : emptyHTML("暂无 Argo CD 接入。可连接已有实例并同步指定项目的应用。");
  }
  function bindArgoConnections(box, configs, reload) {
    function form(existing) {
      var c = existing || {};
      formModal(existing ? "编辑 Argo CD" : "接入 Argo CD", [
        { k: "name", label: "名称", required: true, value: c.name || "" },
        { k: "url", label: "地址", required: true, value: c.url || "", placeholder: "https://argocd.example.com" },
        { k: "env", label: "平台环境", required: true, value: c.env || "", placeholder: "与平台环境策略名称一致" },
        { k: "project", label: "Argo CD 项目", required: true, value: c.project || "default" },
        { k: "token", label: existing ? "Token（留空保留）" : "Token", kind: "password", value: "" }
      ], "保存", function (v) {
        write(existing ? PUT("/cicd/argocd/" + c.id, v) : POST("/cicd/argocd", v), "Argo CD 接入已保存").then(function (res) {
          if (res) { closeAll(); reload(); }
        });
      });
    }
    var add = box.querySelector("[data-newargo]");
    if (add) add.onclick = function () { form(); };
    [["test", function (c) {
      write(POST("/cicd/argocd/" + c.id + "/test", {}), "Argo CD 连接正常");
    }], ["edit", form], ["delete", function (c) {
      confirmDo("移除 Argo CD", "仅移除平台接入配置，Argo CD 和应用不会被删除。", "移除", function () {
        write(DEL("/cicd/argocd/" + c.id), "Argo CD 接入已移除").then(function (res) { if (res) reload(); });
      });
    }], ["apps", argoApplications]].forEach(function (action) {
      box.querySelectorAll("[data-argo-" + action[0] + "]").forEach(function (button) {
        button.onclick = function () {
          var c = configs.find(function (item) { return String(item.id) === button.getAttribute("data-argo-" + action[0]); });
          if (c) action[1](c);
        };
      });
    });
  }
  function argoApplications(config) {
    var root = "/cicd/argocd/" + encodeURIComponent(config.id) + "/applications";
    infoModal("Argo CD   " + config.name, '<div id="opsArgoApps">' + emptyHTML("正在读取应用…") + '</div>');
    var box = q("opsArgoApps");
    function load() {
      GET(root).then(function (result) {
        if (!box.isConnected) return;
        var items = asArray(result, ["items"]);
        box.innerHTML = items.length ? tbl(["应用", "目标集群 / 命名空间", "同步", "健康", "版本", "操作"], items.map(function (a) {
          var spec = a.spec || {}, destination = spec.destination || {}, status = a.status || {}, name = a.metadata && a.metadata.name || "";
          return [E(name), code((destination.name || destination.server || "") + " / " + (destination.namespace || ""), 60),
            E(status.sync && status.sync.status || "Unknown"), E(status.health && status.health.status || "Unknown"),
            code(status.sync && status.sync.revision || "—", 44),
            '<button class="x-btn" type="button" data-argo-sync="' + E(name) + '">同步</button>'];
        })) : emptyHTML("该项目暂无应用");
        box.querySelectorAll("[data-argo-sync]").forEach(function (button) {
          button.onclick = function () {
            var name = button.dataset.argoSync, app = items.find(function (a) { return a.metadata && a.metadata.name === name; });
            var revision = app && app.status && app.status.sync && app.status.sync.revision;
            if (!revision) { toast("尚未取得应用目标版本，请先在 Argo CD 刷新应用"); return; }
            formModal("同步 " + name, [
              { k: "revision", label: "目标版本", required: true, value: revision },
              { k: "reason", label: "变更原因", kind: "textarea", rows: 2 }
            ], "确认同步", function (v) {
              write(POST(root + "/" + encodeURIComponent(name) + "/sync", v), "同步请求已提交，请查看应用健康状态").then(function (res) {
                if (res) { closeAll(); argoApplications(config); }
              });
            }, E(config.env) + "   " + E(config.project) + "。同步沿用 Argo CD 应用配置；不启用 prune 或 force；受平台环境审批控制。");
          };
        });
      }).catch(function (err) { if (box.isConnected) box.innerHTML = banner("读取应用失败：" + errMsg(err)); });
    }
    load();
  }

  /* ---------------------------------------------------------------- delivery 应用 */

  function renderDeliveryLive(el) {
    var parts = (location.hash || "").split("/");
    var seg = parts[2] || "";
    var name = seg && seg !== "apps" ? decodeURIComponent(seg) : "";
    if (name) return deliveryWorkspace(el, name, parts[3] ? decodeURIComponent(parts[3]) : "");
    return deliveryList(el);
  }

  function deliveryList(el) {
    var p = livePage(el, "应用", "CI/CD 应用列表与环境配置", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newapp>+ 新建应用</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { deliveryList(el); };
    el.querySelector("[data-newapp]").onclick = function () { appModal(function () { deliveryList(el); }); };
    var searchText = "", selectedEnv = "", applicationList = [];
    function load() {
      var t = p.newToken();
      appsLoad().then(function (list) {
        applicationList = list;
        paint(t);
      }).catch(function (err) { p.error(t, err); });
    }
    function paint(token) {
        var envs = [];
        applicationList.forEach(function (app) { (app.envs || []).forEach(function (env) {
          if (env.name && envs.indexOf(env.name) < 0) envs.push(env.name);
        }); });
        var needle = searchText.trim().toLowerCase();
        var filtered = applicationList.filter(function (app) {
          var text = [app.name, app.repo_url, app.branch, app.image_repo].concat((app.envs || []).map(function (env) { return env.name; })).join(" ").toLowerCase();
          return (!needle || text.indexOf(needle) >= 0) &&
            (!selectedEnv || (app.envs || []).some(function (env) { return env.name === selectedEnv; }));
        });
        var rows = filtered.map(function (a) {
          var href = "#/delivery/" + encodeURIComponent(a.name);
          return ['<a class="x-tlink" href="' + href + '">' + E(a.name) + "</a>", code(a.repo_url, 46), code(a.branch || "—", 18), code(a.image_repo, 34),
            E((a.envs || []).map(function (e) { return e.name; }).join(", ") || "—"), fmtMin(a.updated_at || a.created_at),
            '<span class="ops-nowrap"><a class="x-btn" href="' + href + '">详情</a><button class="x-btn" type="button" data-appdel="' + E(a.id) + '">删除</button></span>'];
        });
        var filters = barHTML([
          '<input class="x-input" type="search" data-app-search aria-label="搜索应用" placeholder="搜索应用、仓库或镜像仓库" value="' + E(searchText) + '" />',
          '<select class="x-select" data-app-env style="width:auto" aria-label="按环境筛选"><option value="">全部环境</option>' +
            envs.map(function (env) { return '<option value="' + E(env) + '"' + (env === selectedEnv ? " selected" : "") + ">" + E(env) + "</option>"; }).join("") + "</select>",
          '<span class="x-frac" style="margin-left:auto">' + filtered.length + " / " + applicationList.length + " 个应用</span>"
        ]);
        var html = panelHTML("应用", filters + (rows.length
          ? tbl(["应用", "代码仓库", "默认分支", "镜像仓库", "部署环境", "更新时间", "操作"], rows)
          : emptyHTML(applicationList.length ? "没有符合当前筛选条件的应用。" : "尚未创建应用。点击右上角「+ 新建应用」接入代码仓库与镜像仓库。")));
        if (!p.paint(token, html)) return;
        var search = p.box.querySelector("[data-app-search]");
        var env = p.box.querySelector("[data-app-env]");
        if (search) search.oninput = function () {
          searchText = search.value;
          var cursor = search.selectionStart;
          paint(null);
          var next = p.box.querySelector("[data-app-search]");
          if (next) {
            next.focus();
            try { next.setSelectionRange(cursor, cursor); } catch (e) {}
          }
        };
        if (env) env.onchange = function () { selectedEnv = env.value; paint(null); };
        [].slice.call(p.box.querySelectorAll("[data-appdel]")).forEach(function (b) {
          b.onclick = function () {
            confirmDo("删除应用", "确定删除应用？将级联删除其流水线、运行与发布记录。", "删除", function () {
              write(DEL("/cicd/applications/" + encodeURIComponent(b.getAttribute("data-appdel"))), "应用已删除").then(function (r) { if (r) load(); });
            });
          };
        });
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
    var p = livePage(el, "应用   " + name, "环境部署与发布记录", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { deliveryDetail(el, name); };
    var APP = null, RELS = [], RUNS = [], PIPES = [], RELENV = "";
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
      return Promise.all([
        GET("/cicd/applications/" + encodeURIComponent(APP.id) + "/releases", env ? { env: env } : undefined),
        GET("/cicd/runs", { page: 1, page_size: 100, app_id: APP.id }),
        GET("/cicd/pipelines", { app_id: APP.id })
      ]).then(function (data) {
        RELS = Array.isArray(data[0]) ? data[0] : asArray(data[0], ["items", "releases"]);
        RUNS = asArray(data[1], ["items", "runs"]);
        PIPES = Array.isArray(data[2]) ? data[2] : asArray(data[2], ["items", "pipelines"]);
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
      var runRows = RUNS.map(function (r) {
        var pipe = PIPES.filter(function (x) { return String(x.id) === String(r.pipeline_id); })[0];
        return [mono("#" + r.id), E(pipe ? pipe.name : "#" + r.pipeline_id), stTag(r.status), code(r.branch || "—", 20), code(r.image_tag || "—", 28), fmtMin(r.started_at || r.created_at),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-apprun="' + E(r.id) + '">详情</button><button class="x-btn" type="button" data-applog="' + E(r.id) + '">日志</button></span>'];
      });
      var pipeRows = PIPES.map(function (pl) {
        return [mono("#" + pl.id), E(pl.name || "—"), E("#" + pl.runner_target_id), pl.on_push ? tag("代码推送", "x-blue") : tag("手动", "x-grey"), E((pl.stages || []).length + " 阶段"), fmtMin(pl.updated_at),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-apppl="' + E(pl.id) + '">详情</button><button class="x-btn" type="button" data-appplrun="' + E(pl.id) + '">运行</button></span>'];
      });
      return '<p style="margin:0 0 12px"><a class="x-tlink" href="#/delivery/apps">← 返回应用列表</a>' +
        '<span style="float:right" class="ops-nowrap"><button class="x-btn" type="button" data-deploy2>部署</button><button class="x-btn" type="button" data-rotate>轮换 Webhook 密钥</button></span></p>' +
        '<dl class="x-sum">' + [["ID", mono(a.id)], ["代码仓库", code(a.repo_url, 70)], ["分支", E(a.branch || "—")], ["Dockerfile", code(a.dockerfile || "Dockerfile", 40)],
          ["上下文目录", code(a.context_dir || ".", 40)], ["镜像仓库", code(a.image_repo, 60)], ["创建", fmtMin(a.created_at)]]
          .map(function (kv) { return "<dt>" + kv[0] + "</dt><dd>" + kv[1] + "</dd>"; }).join("") + "</dl>" +
        panelHTML("环境", envRows.length ? tbl(["环境", "部署对象", "命名空间", "执行集群", "操作"], envRows) : emptyHTML("该应用未配置环境")) +
        panelHTML("流水线", pipeRows.length ? tbl(["ID", "名称", "执行集群", "触发方式", "阶段", "更新时间", "操作"], pipeRows) : emptyHTML("该应用尚未配置流水线。可在「应用交付 › 流水线」创建并关联此应用。")) +
        panelHTML("构建记录", runRows.length ? tbl(["运行", "流水线", "状态", "分支", "镜像标签", "开始时间", "操作"], runRows) : emptyHTML("该应用暂无构建运行记录。")) +
        panelHTML("发布记录", barHTML([envSel, '<span class="x-frac" style="margin-left:auto">共 ' + RELS.length + " 条</span>"]) + (RELS.length ? tbl(["ID", "环境", "类型", "镜像", "上一镜像", "状态", "操作人", "创建", "完成", "操作"], rows) : emptyHTML("暂无发布记录")));
    }
    function bind() {
      var envSel = p.box.querySelector("[data-relenv]");
      if (envSel) envSel.onchange = function () { RELENV = envSel.value; var t = p.newToken(); loadReleases(RELENV || null, t).catch(function (err) { p.error(t, err); }); };
      var d2 = p.box.querySelector("[data-deploy2]");
      if (d2) d2.onclick = function () { deployModal(APP, "", function () { deliveryDetail(el, name); }); };
      [].slice.call(p.box.querySelectorAll("[data-apprun]")).forEach(function (b) { b.onclick = function () { runDetailModal(b.getAttribute("data-apprun")); }; });
      [].slice.call(p.box.querySelectorAll("[data-applog]")).forEach(function (b) { b.onclick = function () { openRunLog(b.getAttribute("data-applog")); }; });
      [].slice.call(p.box.querySelectorAll("[data-apppl]")).forEach(function (b) {
        b.onclick = function () { var pl = PIPES.filter(function (x) { return String(x.id) === String(b.getAttribute("data-apppl")); })[0]; if (pl) infoModal("流水线   " + (pl.name || pl.id), preBlock(pl)); };
      });
      [].slice.call(p.box.querySelectorAll("[data-appplrun]")).forEach(function (b) {
        b.onclick = function () { var pl = PIPES.filter(function (x) { return String(x.id) === String(b.getAttribute("data-appplrun")); })[0]; if (pl) runPipelineModal(function () { deliveryDetail(el, name); }, [pl]); };
      });
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

  var DELIVERY_MENU = ["导览", "服务列表", "实例列表", "部署编排", "监控告警", "应用成员", "操作记录", "流程中心"];
  function deliveryWorkspace(el, name, initialPane) {
    var pane = DELIVERY_MENU.indexOf(initialPane) >= 0 ? initialPane : "导览";
    var app = null, selectedEnv = "", inventory = null, alerts = [], appClusters = [], appDatasources = [], auditRows = [], releases = [], runs = [], pipes = [];
    var p = livePage(el, "应用   " + name, "应用服务、实例、发布与运行记录", '<button class="x-btn" type="button" data-refresh>刷新</button>', "");
    el.querySelector("[data-refresh]").onclick = load;
    p.retry = load;

    function envConfig() {
      var envs = app && Array.isArray(app.envs) ? app.envs : [];
      return envs.filter(function (e) { return e.name === selectedEnv; })[0] || envs[0] || null;
    }
    function load() {
      var token = p.newToken();
      appsLoad().then(function (items) {
        app = items.filter(function (a) { return a.name === name; })[0] || null;
        if (!app) { p.paint(token, '<p class="x-crumb">应用交付<i>/</i>应用</p>' + emptyHTML("应用「" + name + "」不存在或已被删除。")); return; }
        var envs = Array.isArray(app.envs) ? app.envs : [];
        if (!envs.some(function (e) { return e.name === selectedEnv; })) selectedEnv = envs.length ? envs[0].name : "";
        return loadPane(token);
      }).catch(function (err) { p.error(token, err); });
    }
    function loadPane(token) {
      token = token == null ? p.newToken() : token;
      var env = envConfig(), base = "/cicd/applications/" + encodeURIComponent(app.id);
      var needReleases = pane === "导览" || pane === "部署编排";
      var needFlow = pane === "导览" || pane === "流程中心";
      var requests = [
        needReleases && env ? GET(base + "/releases", { env: env.name }) : Promise.resolve([]),
        needFlow ? GET("/cicd/runs", { page: 1, page_size: 100, app_id: app.id }) : Promise.resolve([]),
        needFlow ? GET("/cicd/pipelines", { app_id: app.id }) : Promise.resolve([]),
        ["导览", "服务列表", "实例列表", "监控告警"].indexOf(pane) >= 0 && env ? GET(base + "/inventory", { env: env.name }) : Promise.resolve(null),
        pane === "监控告警" ? GET("/observability/alerts", { page: 1, page_size: 500 }) : Promise.resolve([]),
        pane === "操作记录" ? GET("/audit-logs", { page: 1, page_size: 100, resource: "cicd_application", q: String(app.id) }) : Promise.resolve([]),
        pane === "监控告警" ? GET("/clusters", { page_size: 500 }) : Promise.resolve([]),
        pane === "监控告警" ? GET("/observability/datasources", { page_size: 500 }) : Promise.resolve([])
      ];
      Promise.all(requests).then(function (data) {
        if (token !== p.token) return;
        releases = Array.isArray(data[0]) ? data[0] : asArray(data[0], ["items", "releases"]);
        runs = asArray(data[1], ["items", "runs"]);
        pipes = Array.isArray(data[2]) ? data[2] : asArray(data[2], ["items", "pipelines"]);
        inventory = data[3];
        alerts = asArray(data[4], ["items", "alerts"]);
        auditRows = asArray(data[5], ["items"]);
        appClusters = asArray(data[6], ["items", "clusters"]);
        appDatasources = asArray(data[7], ["items", "datasources"]);
        if (!p.paint(token, workspaceHTML())) return;
        bindWorkspace();
      }).catch(function (err) { p.error(token, err); });
    }
    function envBar() {
      var envs = app.envs || [];
      if (!envs.length) return '<p class="x-empty">该应用尚未配置部署环境。</p>';
      return '<div class="x-tbar"><label>环境 <select class="x-select" data-app-env aria-label="环境">' + envs.map(function (e) {
        return '<option value="' + E(e.name) + '"' + (e.name === selectedEnv ? " selected" : "") + ">" + E(e.name) + "</option>";
      }).join("") + "</select></label></div>";
    }
    function envMatches(value) {
      var key = String(value || "").trim().toLowerCase();
      var aliases = { development: "dev", "开发": "dev", "开发环境": "dev", test: "test", testing: "test", "测试": "test", "测试环境": "test", staging: "staging", pre: "staging", "预发": "staging", "预发布": "staging", production: "prod", "生产": "prod", "生产环境": "prod" };
      return aliases[key] || key;
    }
    function appAlerts() {
      var env = envConfig(), inv = inventory || {}, pods = inv.pods || [], services = inv.services || [], workloads = inv.workloads || [];
      var targetId = inv.target_id || (env && env.target_id), targetCluster = appClusters.filter(function (c) { return String(c.id) === String(targetId); })[0] || null;
      var sameEnvClusters = appClusters.filter(function (c) { return envMatches(c.env) === envMatches(env && env.name); });
      var podNames = pods.map(function (x) { return x.name; }), serviceNames = services.map(function (x) { return x.name; });
      var workloadNames = workloads.map(function (x) { return x.name; });
      return alerts.filter(function (a) {
        if (!env || (a.env && envMatches(a.env) !== envMatches(env.name))) return false;
        return window.XSObserve && window.XSObserve.alertBelongsToScope(a, {
          appName: app.name, namespace: env.namespace, targetId: targetId, targetCluster: targetCluster,
          environmentClusterCount: sameEnvClusters.length, datasources: appDatasources,
          pods: podNames, services: serviceNames, workloads: workloadNames
        });
      });
    }
    function paneContent() {
      var env = envConfig(), inv = inventory || {}, ws = inv.workloads || [], pods = inv.pods || [], services = inv.services || [];
      if (pane === "导览") {
        var last = releases[0];
        return '<div class="x-sethead"><h2 class="x-h2">应用信息</h2><span class="x-frac">' + E(app.updated_at ? fmtMin(app.updated_at) : "") + "</span></div>" +
          '<dl class="x-sum">' + [["代码仓库", code(app.repo_url, 70)], ["默认分支", E(app.branch || "—")], ["镜像仓库", code(app.image_repo || "—", 60)], ["配置环境", E((app.envs || []).length + " 个")],
            ["最近发布", last ? E(last.env + "   " + (last.image || last.status || "发布")) : "暂无发布记录"]].map(function (x) { return "<dt>" + x[0] + "</dt><dd>" + x[1] + "</dd>"; }).join("") + "</dl>" +
          '<div class="x-cards x-cards4"><button class="x-card x-cardbtn" type="button" data-pane="服务列表"><p>服务</p><strong>' + (env ? services.length : "—") + "</strong></button>" +
          '<button class="x-card x-cardbtn" type="button" data-pane="实例列表"><p>实例</p><strong>' + (env ? pods.length : "—") + "</strong></button>" +
          '<button class="x-card x-cardbtn" type="button" data-pane="部署编排"><p>发布</p><strong>' + releases.length + "</strong></button>" +
          '<button class="x-card x-cardbtn" type="button" data-pane="流程中心"><p>流水线</p><strong>' + pipes.length + "</strong></button></div>" +
          (env ? '<div class="x-tbar"><span class="x-frac">' + E(env.name) + "   " + E(env.namespace || "未配置命名空间") + "   target #" + E(env.target_id) + '</span><span class="x-grow"></span><a class="x-btn" href="#/cicd/build">构建</a><a class="x-btn" href="#/cicd/artifact">制品</a></div>' : emptyHTML("该应用没有环境配置。"));
      }
      if (pane === "服务列表") {
        if (!env) return emptyHTML("该应用没有环境配置。");
        var serviceRows = services.map(function (s) { return [E(s.name), E(s.namespace), tag(s.type || "ClusterIP", "x-blue"), code(s.cluster_ip || "—", 24),
          E((s.ports || []).map(function (p) { return (p.port || "") + "/" + (p.protocol || "TCP"); }).join(", ") || "—"), code(JSON.stringify(s.selector || {}), 56)]; });
        return envBar() + '<p class="x-frac">' + E(env.name + "   " + env.namespace + "   " + services.length + " 个与该应用工作负载 selector 匹配的 Kubernetes Service") + (inv.warnings && inv.warnings.length ? "<br />" + inv.warnings.map(E).join("<br />") : "") + "</p>" +
          (serviceRows.length ? tbl(["服务", "命名空间", "类型", "Cluster IP", "端口", "Pod selector"], serviceRows) : emptyHTML("此环境中没有 selector 匹配该应用工作负载的 Service。"));
      }
      if (pane === "实例列表") {
        if (!env) return emptyHTML("该应用没有环境配置。");
        var podRows = pods.map(function (pod, index) { return [E(pod.name), E(pod.namespace), E(pod.ready || "—"), stTag(pod.status || pod.phase || "未知"),
          E(pod.restarts || 0), E(pod.node || "—"), code(pod.pod_ip || "—", 20), '<button class="x-act" type="button" data-app-pod="' + index + '">详情</button>']; });
        return envBar() + '<p class="x-frac">' + E(env.name + "   " + env.namespace + "   实时 Pod 清单") + (inv.warnings && inv.warnings.length ? "<br />" + inv.warnings.map(E).join("<br />") : "") + "</p>" +
          (podRows.length ? tbl(["Pod", "命名空间", "Ready", "状态", "重启", "节点", "Pod IP", "详情"], podRows) : emptyHTML("此环境当前没有该应用的 Pod。"));
      }
      if (pane === "部署编排") {
        if (!env) return emptyHTML("该应用没有环境配置。");
        var configRows = (app.envs || []).map(function (item) { return [E(item.name), E(item.namespace || "—"), E(item.deployment || (item.helm && item.helm.release) || "—"), E("#" + item.target_id),
          '<span class="ops-nowrap"><button class="x-btn" type="button" data-app-deploy="' + E(item.name) + '">部署</button><button class="x-btn" type="button" data-app-rollback="' + E(item.name) + '">回滚</button></span>']; });
        var releaseRows = releases.map(function (r) { return [mono("#" + r.id), E(r.env), tag(r.kind || "deploy", r.kind === "rollback" ? "x-orange" : "x-blue"), code(r.image || "—", 42),
          stTag(r.status), fmtMin(r.created_at), '<button class="x-btn" type="button" data-app-relrb="' + E(r.id) + '" data-app-relenv="' + E(r.env) + '">回滚到此</button>']; });
        return envBar() + '<div class="x-sethead"><h2 class="x-h2">部署目标</h2></div>' + tbl(["环境", "命名空间", "工作负载", "执行集群", "操作"], configRows) +
          '<div class="x-sethead" style="margin-top:20px"><h2 class="x-h2">发布记录</h2><span class="x-frac">' + releases.length + " 条</span></div>" +
          (releaseRows.length ? tbl(["发布", "环境", "类型", "镜像", "状态", "创建时间", "操作"], releaseRows) : emptyHTML("该环境没有发布记录。"));
      }
      if (pane === "监控告警") {
        if (!env) return emptyHTML("该应用没有环境配置。");
        var appAlertRows = appAlerts().map(function (a) { var lb = a.labels || {}, an = a.annotations || {}; return [sevTag(a.severity || lb.severity),
          '<button class="x-tlink x-alert-open" type="button" data-app-alert="' + E(alertKey(a)) + '">' + E(a.alertname || lb.alertname || "告警") + "</button>",
          E(lb.namespace || env.namespace), E(lb.pod || lb.deployment || lb.service || lb.app || "—"), stTag(a.status || "未知"), fmtMin(a.starts_at), E(an.summary || an.description || "—")]; });
        return envBar() + '<p class="x-frac">仅显示与当前应用清单中的 Pod、工作负载或 Service 标签匹配的告警。</p>' +
          (appAlertRows.length ? tbl(["级别", "告警", "命名空间", "对象", "状态", "开始时间", "摘要"], appAlertRows) : emptyHTML("当前环境没有可确认属于此应用的告警。"));
      }
      if (pane === "应用成员") return '<div class="x-sethead"><h2 class="x-h2">应用成员</h2></div><p class="x-empty">当前后端没有应用级成员关系数据，无法准确列出负责人或授权成员。平台账号与角色请在系统设置中管理。</p>';
      if (pane === "操作记录") {
        var filtered = auditRows.filter(function (r) { return String(r.resource_id || "") === String(app.id) || String(r.path || "").indexOf("/cicd/applications/" + app.id) >= 0; });
        var auditTable = filtered.map(function (r) { return [fmtMin(r.created_at || r.at), E(r.username || r.user || "—"), E(r.method || "—"), code(r.path || r.action || "—", 64), E(r.action || r.resource || "—"), stTag(r.result || r.status || "—")]; });
        return '<div class="x-sethead"><h2 class="x-h2">操作记录</h2><span class="x-frac">' + filtered.length + " 条</span></div>" +
          (auditTable.length ? tbl(["时间", "操作人", "方法", "路径", "动作", "结果"], auditTable) : emptyHTML("没有找到该应用的审计记录。"));
      }
      var pipelineRows = pipes.map(function (pl) { return [mono("#" + pl.id), E(pl.name || "—"), E("#" + pl.runner_target_id), E((pl.stages || []).length + " 阶段"), fmtMin(pl.updated_at),
        '<span class="ops-nowrap"><button class="x-btn" type="button" data-app-pipeline="' + E(pl.id) + '">详情</button><button class="x-btn" type="button" data-app-runpipe="' + E(pl.id) + '">运行</button></span>']; });
      var runRows = runs.map(function (r) { var pl = pipes.filter(function (x) { return String(x.id) === String(r.pipeline_id); })[0]; return [mono("#" + r.id), E(pl ? pl.name : "#" + r.pipeline_id), stTag(r.status), code(r.branch || "—", 22), code(r.image_tag || "—", 28), fmtMin(r.started_at || r.created_at),
        '<span class="ops-nowrap"><button class="x-btn" type="button" data-app-rund="' + E(r.id) + '">详情</button><button class="x-btn" type="button" data-app-runlog="' + E(r.id) + '">日志</button></span>']; });
      return '<div class="x-sethead"><h2 class="x-h2">流水线</h2><span class="ops-nowrap"><a class="x-btn" href="#/cicd/pipeline">管理流水线</a><button class="x-btn x-btn--pri" type="button" data-app-newrun>运行流水线</button></span></div>' +
        (pipelineRows.length ? tbl(["ID", "名称", "执行集群", "阶段", "更新时间", "操作"], pipelineRows) : emptyHTML("该应用尚未配置流水线。")) +
        '<div class="x-sethead" style="margin-top:20px"><h2 class="x-h2">运行记录</h2><span class="x-frac">' + runs.length + " 次</span></div>" +
        (runRows.length ? tbl(["运行", "流水线", "状态", "分支", "镜像标签", "开始时间", "操作"], runRows) : emptyHTML("该应用暂无流水线运行记录。"));
    }
    function workspaceHTML() {
      var crumbs = '<p class="x-crumb">应用交付<i>/</i>' + E(app.name) + '<i>/</i><b>' + E(pane) + "</b></p>";
      var nav = '<nav class="x-menu" aria-label="应用菜单"><a href="#/delivery/apps" class="x-back">← 返回应用列表</a><p class="x-appname">' + E(app.name) + "</p>" +
        DELIVERY_MENU.map(function (item) { return '<button type="button" data-pane="' + E(item) + '"' + (item === pane ? ' class="on" aria-current="page"' : "") + ">" + E(item) + "</button>"; }).join("") + "</nav>";
      return '<div class="x-appv">' + nav + '<section class="x-panel">' + crumbs + paneContent() + "</section></div>";
    }
    function bindWorkspace() {
      [].slice.call(p.box.querySelectorAll("[data-pane]")).forEach(function (button) {
        button.onclick = function () { pane = button.getAttribute("data-pane"); history.replaceState(null, "", "#/delivery/" + encodeURIComponent(name) + "/" + encodeURIComponent(pane)); loadPane(); };
      });
      var envSelect = p.box.querySelector("[data-app-env]");
      if (envSelect) envSelect.onchange = function () { selectedEnv = envSelect.value; loadPane(); };
      [].slice.call(p.box.querySelectorAll("[data-app-pod]")).forEach(function (button) {
        button.onclick = function () { infoModal("Pod 详情", preBlock((inventory.pods || [])[+button.getAttribute("data-app-pod")] || {})); };
      });
      [].slice.call(p.box.querySelectorAll("[data-app-alert]")).forEach(function (button) {
        button.onclick = function () { if (typeof window.XSOpenAlertDetail === "function") window.XSOpenAlertDetail(button.getAttribute("data-app-alert")); };
      });
      [].slice.call(p.box.querySelectorAll("[data-app-deploy]")).forEach(function (button) { button.onclick = function () { appDeployModal(APP, button.getAttribute("data-app-deploy"), load); }; });
      [].slice.call(p.box.querySelectorAll("[data-app-rollback]" )).forEach(function (button) {
        button.onclick = function () { var env = button.getAttribute("data-app-rollback"); confirmDo("回滚应用", "确认回滚「" + E(app.name) + "」环境 <b>" + E(env) + "</b>？", "回滚", function () {
          write(POST("/cicd/applications/" + encodeURIComponent(app.id) + "/rollback", { env: env }), "回滚请求已提交").then(function (result) { if (result) load(); });
        }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-app-relrb]")).forEach(function (button) {
        button.onclick = function () { var env = button.getAttribute("data-app-relenv"), id = button.getAttribute("data-app-relrb"); confirmDo("回滚到指定发布", "确认将环境 <b>" + E(env) + "</b> 重新部署发布 #" + E(id) + " 的镜像？", "回滚", function () {
          write(POST("/cicd/applications/" + encodeURIComponent(app.id) + "/rollback", { env: env, release_id: +id }), "回滚请求已提交").then(function (result) { if (result) load(); });
        }); };
      });
      [].slice.call(p.box.querySelectorAll("[data-app-pipeline]")).forEach(function (button) { button.onclick = function () { var pl = pipes.filter(function (x) { return String(x.id) === button.getAttribute("data-app-pipeline"); })[0]; if (pl) infoModal("流水线   " + (pl.name || pl.id), preBlock(pl)); }; });
      [].slice.call(p.box.querySelectorAll("[data-app-runpipe]")).forEach(function (button) { button.onclick = function () { var pl = pipes.filter(function (x) { return String(x.id) === button.getAttribute("data-app-runpipe"); })[0]; if (pl) runPipelineModal(load, [pl]); }; });
      var newRun = p.box.querySelector("[data-app-newrun]"); if (newRun) newRun.onclick = function () { runPipelineModal(load, pipes); };
      [].slice.call(p.box.querySelectorAll("[data-app-rund]")).forEach(function (button) { button.onclick = function () { runDetailModal(button.getAttribute("data-app-rund")); }; });
      [].slice.call(p.box.querySelectorAll("[data-app-runlog]")).forEach(function (button) { button.onclick = function () { openRunLog(button.getAttribute("data-app-runlog")); }; });
    }
    load();
  }

  function appDeployModal(app, env, done) {
    var envs = (app.envs || []).map(function (item) { return item.name; });
    if (!envs.length) { toast("该应用未配置环境"); return; }
    formModal("部署 " + app.name, [
      { k: "env", label: "环境", kind: "select", options: envs, value: env || envs[0] },
      { k: "image", label: "镜像", required: true, placeholder: app.image_repo + ":v1.2.0" },
      { k: "reason", label: "变更原因", placeholder: "hotfix / 需求发布" }
    ], "部署", function (values) {
      write(POST("/cicd/applications/" + encodeURIComponent(app.id) + "/deploy", { env: values.env, image: values.image, reason: values.reason }), "部署请求已提交").then(function (result) {
        if (!result) return;
        closeAll();
        if (result.__approval) infoModal("已提交审批", '<p class="x-desc">生产环境部署需要审批，审批通过后自动执行。</p>');
        if (done) done();
      });
    }, isProd(env) ? "生产环境可能需要审批（提交后显示“已提交审批”）。" : "");
  }

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
        b.onclick = function () { var it = byId(items, b.getAttribute("data-mwdet")); if (it) infoModal("实例详情   " + (it.name || it.id), preBlock(it)); };
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
    return tag(r.label + (r.reason ? "   " + r.reason : ""), cls);
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
    dlg("SQL 控制台   " + E(it.name),
      '<div class="x-form"><p class="x-desc" style="margin:0">环境 <b>' + E(it.env || "*") + "</b>   类型 " + E(it.type) + (prod ? '   <span class="x-tag x-red x-tag--s">生产</span>' : "") + "</p>" +
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
    dlg("Redis 控制台   " + E(it.name),
      '<div class="x-form"><p class="x-desc" style="margin:0">环境 <b>' + E(it.env || "*") + "</b>   类型 Redis" + (isProd(it.env) ? '   <span class="x-tag x-red x-tag--s">生产</span>' : "") + "</p>" +
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
    var address = window.location.origin || (window.location.protocol + "//" + window.location.host);
    var state = !hasIngress
      ? '<span class="x-tag x-orange">未检测到 Ingress</span> 域名不会生效，请用 IP:NodePort 访问'
      : '<span class="x-tag x-green">Ingress 可用' + (cls ? "（" + E(cls) + "）" : "") + "</span>";
    return '<div class="x-panel" style="margin-bottom:16px">' +
      '<div class="x-tbar" style="margin-bottom:8px"><h2 class="x-h2" style="margin:0">平台入口</h2>' +
      '<span class="x-frac">TLS 使用平台已配置的 Secret</span></div>' +
      '<div class="x-tbar" style="margin:0 0 8px"><span class="x-frac">当前访问地址</span><code class="x-mono">' + E(address) + "</code></div>" +
      '<div class="x-tbar" style="margin-bottom:8px"><h3 class="x-h2" style="margin:0">Ingress 域名（可选）</h3>' +
      '<span class="x-frac">当前地址自动读取；域名仅用于 Ingress 配置</span></div>' +
      '<div class="x-tbar" style="margin:0">' +
      '<input class="x-input" data-dom value="' + E(d.domain || "") + '" placeholder="如 xingshu-matrix.com" aria-label="平台访问域名" style="max-width:320px" />' +
      '<button class="x-btn x-btn--pri" type="button" data-domsave>保存并应用</button>' +
      '<button class="x-btn" type="button" data-domclear>清除域名</button>' +
      '<span class="x-frac">' + state + "</span></div>" +
      '<p class="x-frac" style="margin:10px 0 0;line-height:1.7">' +
      '  域名需要集群内存在 Ingress 控制器；保存时平台会自动创建/更新 Ingress（host=该域名，TLS 复用 <code class="x-mono">' + E(d.tls_secret || "xingshu-matrix-tls") + '</code>）。<br />' +
      '  集群没有 Ingress 时保存依旧成功，但域名不生效，请使用 <code class="x-mono">http(s)://节点IP:' + E(d.nodeport || "30080") + '</code> 访问。<br />' +
      "  清空并保存会删除平台自己的 Ingress，回退到 NodePort。" +
      '</p></div>';
  }
  function registryMirrorPanel(mirror) {
    var provider = mirror && mirror.provider || "dockerhub";
    var endpoint = mirror && mirror.endpoint || "";
    var selected = function (value, text) {
      return '<option value="' + value + '"' + (provider === value ? " selected" : "") + ">" + text + "</option>";
    };
    var hint = provider === "aliyun"
      ? "填写阿里云账号控制台分配的地址。官方说明：已停止同步最新镜像，仅限个人非商业用途，且非阿里云 ECS 访问可能返回 403。"
      : provider === "daocloud"
        ? "默认使用 DaoCloud Docker Hub 镜像代理；镜像路径会按官方格式改写。"
        : provider === "custom"
          ? "填写兼容 Docker Registry v2 且无需认证的镜像代理基础地址，不要包含账号密码或查询参数。"
          : "直接从 docker.io 拉取，不配置代理。";
    return panelHTML("集群镜像源", '<div class="x-form" style="max-width:760px">' +
      '<div class="x-row2"><label><span>镜像源</span><select class="x-select" data-mirror-provider>' +
      selected("dockerhub", "Docker Hub（docker.io 直连）") +
      selected("aliyun", "阿里云镜像加速（个人开发）") +
      selected("daocloud", "DaoCloud") +
      selected("custom", "自定义") +
      '</select></label><label><span>镜像代理地址</span><input class="x-input" data-mirror-endpoint value="' + E(endpoint) +
      '" placeholder="https://registry-mirror.example.com"' + (provider === "dockerhub" ? " disabled" : "") + ' /></label></div>' +
      '<div class="x-tbar" style="margin:0"><span class="x-frac" data-mirror-hint>' + E(hint) +
      '</span><button class="x-btn x-btn--pri" type="button" data-mirror-save>保存镜像源</button></div>' +
      '<p class="x-frac" style="margin:8px 0 0">仅影响之后通过平台新建的 K3s 集群中 Docker Hub 镜像的拉取；已存在集群不会被修改，其他镜像仓库保持原配置。</p>' +
      '</div>');
  }
  function renderSettingsLive(el) {
    var p = livePage(el, "系统设置", "基础信息、代码仓库、镜像仓库、集群与可观测性配置", '<button class="x-btn" type="button" data-refresh>刷新</button><button class="x-btn x-btn--pri" type="button" data-newset>+ 新建设置</button>', "");
    el.querySelector("[data-refresh]").onclick = function () { renderSettingsLive(el); };
    var newSettingButton = el.querySelector("[data-newset]");
    newSettingButton.disabled = true;
    newSettingButton.onclick = function () { settingModal(null, function () { renderSettingsLive(el); }); };
    var SET = [], POL = [];
    function load() {
      var t = p.newToken();
      Promise.all([settle(GET("/settings"), []), settle(GET("/env-policies"), []), settle(GET("/platform/domain"), {}), settle(GET("/provision/registry-mirror"), { provider: "dockerhub", endpoint: "" })]).then(function (rs) {
        SET = rs[0].ok ? asArray(rs[0].value, ["items", "settings"]) : [];
        POL = rs[1].ok ? asArray(rs[1].value, ["items", "policies"]) : [];
        var DOM = rs[2] && rs[2].ok && rs[2].value ? rs[2].value : {};
        var MIRROR = rs[3] && rs[3].ok && rs[3].value ? rs[3].value : { provider: "dockerhub", endpoint: "" };
        if (newSettingButton) newSettingButton.disabled = !rs[0].ok;
        var failed = [];
        if (!rs[0].ok) failed.push("平台设置：" + errMsg(rs[0].error));
        if (!rs[1].ok) failed.push("环境策略：" + errMsg(rs[1].error));
        if (!rs[3].ok) failed.push("集群镜像源：" + errMsg(rs[3].error));
        var configKeys = systemConfigKeys();
        var setRows = SET.map(function (s) {
          var notifyKey = s.key === "notify" || s.key.indexOf("notify.") === 0;
          if (s.key === "platform.domain" || s.key === "aiops.policy" || s.key === "provision.registry_mirror" ||
              s.key === "ai.features" || s.key === "public.notices" || notifyKey || configKeys[s.key]) return null;
          return [code(s.key, 42), settingVal(s), E(s.description || "—"), s.secret ? tag("加密", "x-orange") : E("—"), fmtMin(s.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-setedit="' + E(s.key) + '">编辑</button> <button class="x-btn" type="button" data-setdel="' + E(s.key) + '">删除</button></span>'];
        }).filter(Boolean);
        var polRows = POL.map(function (x) {
          return [E(x.env), E(x.name || "—"), x.require_approval ? tag("需要审批", "x-orange") : tag("直接执行", "x-green"),
            E(x.approver_permission || "—"), fmtMin(x.updated_at), '<button class="x-btn" type="button" data-poledit="' + E(x.env) + '">编辑</button>'];
        });
        var configPanel = systemConfigPanelResult(rs[0].ok, SET);
        var html = domainPanel(DOM) + registryMirrorPanel(MIRROR) + configPanel +
          panelHTML("告警通知", '<div class="x-tbar" style="margin:0"><span class="x-frac">通知渠道由监控告警统一管理' +
            (SET.some(function (s) { return s.key === "notify" || s.key.indexOf("notify.") === 0; }) ? "；历史 notify.* 配置已隐藏且不会参与发送。" : "。") +
            '</span><a class="x-btn x-btn--pri" href="#/observe/channels">管理通知渠道</a></div>') +
          (failed.length ? banner("部分设置区块加载失败（其余区块仍可用）：" + failed.join("；")) : "") +
          panelHTML("平台设置", setRows.length ? tbl(["键", "值", "说明", "加密", "更新时间", "操作"], setRows) : emptyHTML("暂无设置项。点击右上角「+ 新建设置」。")) +
          panelHTML("环境审批策略", polRows.length ? tbl(["环境", "名称", "变更审批", "审批权限", "更新时间", "操作"], polRows) : emptyHTML("暂无环境策略。可编辑添加 prod / test / dev 的审批要求。"));
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
      [].slice.call(p.box.querySelectorAll("[data-system-config]")).forEach(function (form) {
        var section = systemConfigSchema().filter(function (item) { return item.key === form.getAttribute("data-system-config"); })[0];
        if (!section) return;
        var check = form.querySelector("[data-config-check]");
        if (check) check.onclick = function () {
          var urls = section.fields.filter(function (field) { return field.kind === "url"; }).map(function (field) {
            return form.elements[field.key].value.trim();
          }).filter(Boolean);
          var valid = urls.every(function (value) {
            try { return /^https?:$/.test(new URL(value).protocol); } catch (e) { return false; }
          });
          var result = form.querySelector(".x-test");
          result.className = "x-test" + (valid ? "" : " bad");
          result.textContent = valid
            ? "地址格式有效；此检查不验证远端连通性。"
            : "地址格式无效，请填写 http:// 或 https:// 地址。";
        };
        form.onsubmit = function (event) {
          event.preventDefault();
          var requests = [];
          section.fields.forEach(function (field) {
            var control = form.elements[field.key];
            var clear = field.secret && form.elements[field.key + "__clear"];
            var value = field.kind === "url" ? control.value.trim() : control.value;
            if (field.secret && clear && clear.checked) {
              requests.push({ key: field.key, run: function () { return DEL("/settings/" + encodeURIComponent(field.key)); } });
            } else if (field.secret) {
              if (value !== "") requests.push({ key: field.key, run: function () {
                return PUT("/settings/" + encodeURIComponent(field.key), {
                  value: value, secret: true, description: section.label + "集成参数"
                });
              } });
            } else {
              requests.push({ key: field.key, run: function () {
                return PUT("/settings/" + encodeURIComponent(field.key), {
                  value: value, secret: false, description: section.label + "集成参数"
                });
              } });
            }
          });
          if (!requests.length) {
            toast("没有需要保存或清除的配置");
            return;
          }
          form.querySelectorAll("button").forEach(function (button) { button.disabled = true; });
          var savedKeys = [], failedKeys = [];
          requests.reduce(function (chain, request) {
            return chain.then(function () {
              return request.run().then(function () { savedKeys.push(request.key); }, function () { failedKeys.push(request.key); });
            });
          }, Promise.resolve()).then(function () {
            if (failedKeys.length) {
              form.querySelectorAll("button").forEach(function (button) { button.disabled = false; });
              var result = form.querySelector("[data-config-result]");
              if (result) {
                result.className = "x-test bad";
                result.textContent = (savedKeys.length ? "部分配置已保存；" : "配置未保存；") +
                  "失败项：" + failedKeys.join("、") + "。当前输入已保留，请修正后重试。";
              }
              toast((savedKeys.length ? "部分配置已保存。" : "配置未保存。") + "失败项：" + failedKeys.join("、"));
            } else {
              toast(section.label + "配置已保存");
              renderSettingsLive(el);
            }
          });
        };
      });
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
      var mirrorProvider = p.box.querySelector("[data-mirror-provider]");
      var mirrorEndpoint = p.box.querySelector("[data-mirror-endpoint]");
      var mirrorHint = p.box.querySelector("[data-mirror-hint]");
      var lastMirrorProvider = mirrorProvider && mirrorProvider.value;
      var mirrorHints = {
        dockerhub: "直接从 docker.io 拉取，不配置代理。",
        aliyun: "填写阿里云账号控制台分配的地址。官方说明：已停止同步最新镜像，仅限个人非商业用途，且非阿里云 ECS 访问可能返回 403。",
        daocloud: "默认使用 DaoCloud Docker Hub 镜像代理；镜像路径会按官方格式改写。",
        custom: "填写兼容 Docker Registry v2 的镜像代理基础地址，不要包含账号密码或查询参数。"
      };
      if (mirrorProvider && mirrorEndpoint) {
        mirrorProvider.onchange = function () {
          if (mirrorProvider.value === "dockerhub") mirrorEndpoint.value = "";
          else if (mirrorProvider.value === "daocloud" && lastMirrorProvider !== "daocloud") mirrorEndpoint.value = "https://m.daocloud.io";
          else if (mirrorProvider.value === "aliyun" && lastMirrorProvider === "daocloud" && mirrorEndpoint.value.trim() === "https://m.daocloud.io") mirrorEndpoint.value = "";
          mirrorEndpoint.disabled = mirrorProvider.value === "dockerhub";
          if (mirrorHint) mirrorHint.textContent = mirrorHints[mirrorProvider.value] || "";
          lastMirrorProvider = mirrorProvider.value;
        };
      }
      var mirrorSave = p.box.querySelector("[data-mirror-save]");
      if (mirrorSave) mirrorSave.onclick = function () {
        var config = {
          provider: mirrorProvider.value,
          endpoint: mirrorProvider.value === "dockerhub" ? "" : mirrorEndpoint.value.trim()
        };
        if (config.provider !== "dockerhub" && !config.endpoint) {
          toast(config.provider === "aliyun" ? "请填写阿里云账号专属加速地址" : "请填写镜像代理地址");
          mirrorEndpoint.focus();
          return;
        }
        mirrorSave.disabled = true;
        PUT("/provision/registry-mirror", config).then(function () {
          toast("集群镜像源已保存");
          renderSettingsLive(el);
        }).catch(function (err) {
          mirrorSave.disabled = false;
          toast("保存失败：" + errMsg(err));
        });
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
      formModal(isEdit ? "编辑设置   " + st.key : "新建设置", [
        { k: "key", label: "键（小写点分名称）", required: true, value: st ? st.key : "", placeholder: "public.site_name" },
        { k: "value", label: "值（JSON，例如 \"https://...\" 或 {\"k\":1}）", kind: "textarea", rows: 4, required: true, value: valText },
        { k: "secret", label: "加密存储", kind: "checkbox", value: !!(st && st.secret), hint: "加密" },
        { k: "description", label: "说明", value: st ? st.description || "" : "" }
      ], isEdit ? "保存" : "创建", function (v) {
        if (v.key === "platform.domain" || v.key === "aiops.policy" || v.key === "provision.registry_mirror" || v.key === "notify" || v.key.indexOf("notify.") === 0) {
          toast("此配置由专属模块管理，请使用平台入口或通知渠道页面");
          return;
        }
        var value;
        try { value = parseJSON(v.value, ""); } catch (err) { toast(errMsg(err)); return; }
        write(PUT("/settings/" + encodeURIComponent(v.key), { value: value, secret: v.secret, description: v.description }), "设置已保存").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      }, isEdit ? "secret 类型编辑时不会回显原值；留空将保存 JSON 空字符串。" : "值必须是合法 JSON；secret=true 时整体加密存储并在读取时掩码。");
    }
    function envPolicyModal(env, cur, done) {
      formModal("环境策略   " + env, [
        { k: "name", label: "环境名称", value: cur ? cur.name || env : env },
        { k: "require_approval", label: "变更需要审批", kind: "checkbox", value: cur ? !!cur.require_approval : isProd(env), hint: "需要审批" },
        { k: "approver_permission", label: "审批权限码", value: cur ? cur.approver_permission || "" : "approval:approve", placeholder: "approval:approve" }
      ], "保存", function (v) {
        write(PUT("/env-policies/" + encodeURIComponent(env), { name: v.name, require_approval: v.require_approval, approver_permission: v.approver_permission }), "环境策略已保存").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    load();
  }

  function systemConfigSchema() {
    return [
      { key: "basic", label: "基础信息", fields: [
        { key: "platform.name", label: "平台名称", kind: "text", placeholder: "星枢 Matrix" }
      ] },
      { key: "scm", label: "代码仓库", fields: [
        { key: "scm.type", label: "类型", kind: "select", options: ["GitLab", "Gitea", "GitHub Enterprise"] },
        { key: "scm.url", label: "地址", kind: "url", placeholder: "https://git.example.com" },
        { key: "scm.token", label: "Token", kind: "password", secret: true }
      ] },
      { key: "registry", label: "镜像仓库", fields: [
        { key: "registry.url", label: "Harbor 地址", kind: "url", placeholder: "https://harbor.example.com" },
        { key: "registry.user", label: "用户名", kind: "text" },
        { key: "registry.pass", label: "密码", kind: "password", secret: true }
      ] },
      { key: "k8s", label: "K8s 集群", fields: [
        { key: "k8s.name", label: "集群名称", kind: "text" },
        { key: "k8s.api", label: "API Server 地址", kind: "url", placeholder: "https://kubernetes.example.com:6443" },
        { key: "k8s.kubeconfig", label: "kubeconfig", kind: "textarea", secret: true, rows: 5 }
      ] },
      { key: "observability", label: "可观测性", fields: [
        { key: "obs.prom", label: "Prometheus 地址", kind: "url", placeholder: "https://prometheus.example.com" },
        { key: "obs.log", label: "日志地址", kind: "url", placeholder: "https://loki.example.com" }
      ] }
    ];
  }

  function systemConfigKeys() {
    var out = {};
    systemConfigSchema().forEach(function (section) {
      section.fields.forEach(function (field) { out[field.key] = true; });
    });
    return out;
  }

  function systemConfigPanels(settings) {
    function find(key) {
      for (var i = 0; i < settings.length; i++) if (settings[i].key === key) return settings[i];
      return null;
    }
    return systemConfigSchema().map(function (section) {
      var fields = section.fields.map(function (field) {
        var setting = find(field.key), value = "";
        if (setting && !setting.secret) {
          try {
            var decoded = typeof setting.value === "string" ? setting.value : JSON.stringify(setting.value);
            value = decoded == null ? "" : decoded;
          } catch (e) { value = ""; }
        }
        var input;
        if (field.kind === "select") {
          input = '<select class="x-select" name="' + E(field.key) + '">' + field.options.map(function (option) {
            return '<option value="' + E(option) + '"' + (value === option ? " selected" : "") + ">" + E(option) + "</option>";
          }).join("") + "</select>";
        } else if (field.kind === "textarea") {
          input = '<textarea class="x-input x-mono" name="' + E(field.key) + '" rows="' + (field.rows || 4) +
            '" placeholder="' + E(setting && setting.has_value ? "留空保持已保存的密钥" : "粘贴 kubeconfig 内容") + '">' + E(value) + "</textarea>";
        } else {
          input = '<input class="x-input" type="' + E(field.kind) + '" name="' + E(field.key) + '" value="' + E(value) +
            '" placeholder="' + E(setting && setting.has_value && field.secret ? "留空保持已保存的密钥" : field.placeholder || "") +
            '" autocomplete="off" />';
        }
        var secretStatus = field.secret
          ? '<span class="x-frac">' + (setting && setting.has_value ? "密钥已配置，不会回显" : "未设置") + '</span>' +
            (setting && setting.has_value ? '<label class="x-check"><input type="checkbox" name="' + E(field.key) +
              '__clear" /> 清除已保存值</label>' : "")
          : "";
        return '<label' + (field.kind === "textarea" ? ' class="x-full"' : "") + '><span>' + E(field.label) +
          "</span>" + input + secretStatus + "</label>";
      }).join("");
      var targets = section.fields.filter(function (field) { return field.kind === "url"; });
      return '<form class="x-panel x-form x-setform" data-system-config="' + E(section.key) + '" novalidate>' +
        '<div class="x-sethead"><h2 class="x-h2">' + E(section.label) + "</h2></div>" +
        '<div class="x-fields">' + fields + "</div>" +
        '<div class="x-testrow">' + (targets.length ? '<button class="x-btn" type="button" data-config-check>检查地址</button>' +
          '<span class="x-test" role="status"></span>' : "") +
        '<span class="x-test" data-config-result role="status" aria-live="polite"></span>' +
        '<button class="x-btn x-btn--pri" type="submit" style="margin-left:auto">保存</button></div></form>';
    }).join("");
  }

  function systemConfigPanelResult(available, settings) {
    return available
      ? systemConfigPanels(settings)
      : panelHTML("平台配置", banner("平台配置读取失败，已停用配置编辑，避免空值覆盖已有设置。请刷新后重试。"));
  }

  /* ---------------------------------------------------------------- ai 智能助手 */

  function aiPageKey() {
    var seg = (location.hash || "").split("/")[2] || "config";
    return ["config", "chat", "history"].indexOf(seg) >= 0 ? seg : "config";
  }
  function renderAiLive(el) {
    var pg = aiPageKey();
    if (pg !== "config") {
      el.innerHTML = '<div class="x-panel">' + emptyHTML("该子页尚未接入后端接口") + "</div>";
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
      formModal(m ? "编辑模型   " + m.name : "新建模型", [
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
      formModal(x ? "编辑提示词   " + x.name : "新建提示词", [
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
      E(w.next_username || "—") + (w.next_starts_at ? "   " + fmtMin(w.next_starts_at) : "")];
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
      dlg("排班详情   " + E(s.name || id), '<div class="x-form">' + info +
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
    formModal(s ? "编辑排班   " + s.name : "新建排班", [
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
    formModal(rot ? "编辑轮值层   L" + rot.layer : "新增轮值层", [
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
      var schOpts = [{ v: 0, t: "不关联排班" }].concat(items.map(function (s) { return { v: s.id, t: s.name + (s.env ? "   " + s.env : "") }; }));
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
          ["声明时间", fmtMin(x.declared_at)], ["认领", E(x.acked_by || "—") + (x.acked_at ? "   " + fmtMin(x.acked_at) : "")],
          ["解决", E(x.resolved_by || "—") + (x.resolved_at ? "   " + fmtMin(x.resolved_at) : "")], ["解决说明", E(x.resolve_note || "—")],
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
        var promOpts = [{ v: 0, t: "不指定（manual 可留空）" }].concat(proms.map(function (d) { return { v: d.id, t: d.name + "   " + (d.env || "*") }; }));
        formModal(s ? "编辑 SLO   " + s.name : "新建 SLO", [
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
        infoModal("错误预算   " + (data.slo && data.slo.name ? data.slo.name : id), warn + cards +
          '<p class="ops-sub">' + health + "   来源 " + E(data.source || "") + "   区间 " + fmtMin(data.from) + " → " + fmtMin(data.to) + "</p>" + trend + preBlock(b));
      }).catch(function (err) { toast(errMsg(err)); });
    }
    function entriesModal(s) {
      GET("/oncall/slos/" + encodeURIComponent(s.id) + "/entries").then(function (data) {
        var items = asArray(data, ["items", "entries"]);
        var rows = items.map(function (e) {
          return [mono("#" + e.id), fmtMin(e.period_start), fmtMin(e.period_end), num(e.good), num(e.total), E(e.note || "—"),
            '<button class="x-btn" type="button" data-entdel="' + E(e.id) + '">删除</button>'];
        });
        dlg("手工 SLI 录入   " + E(s.name), '<div class="x-form">' +
          '<p class="x-desc" style="margin:0">SLO #' + E(s.id) + "（" + E(s.source) + "）   仅 manual 来源可录入手工窗口。</p>" +
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
        '<span class="x-frac" style="margin-left:auto">第 ' + st.page + " 页   每页 " + st.size + " 条</span>") +
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
    cicd: renderCicdLive,
    delivery: renderDeliveryLive,
    release: renderReleaseLive,
    middleware: renderMiddlewareLive,
    oncall: renderOncallLive,
    settings: renderSettingsLive,
    audit: renderAuditLive,
    appstatus: renderDeliveryLive,
    testing: renderTestingLive
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
      p.paint(p.newToken(), '<div class="x-panel">' + banner("自动化编排需要连接后端后才可用。") + emptyHTML("后端未连接") + "</div>");
      return;
    }
    el.querySelector("[data-refresh]").onclick = function () { renderAutomationLive(el); };
    var WFS = [];
    function load() {
      var t = p.newToken();
      Promise.all([
        GET("/automation/workflows", { page_size: 100 }),
        GET("/automation/runs", { page_size: 50 }),
        settle(GET("/automation/ansible/runtime"), null)
      ]).then(function (rs) {
        var w = rs[0] || {}, runs = rs[1] || {};
        var runtimeResult = rs[2] || {};
        WFS = Array.isArray(w.items) ? w.items : asArray(w, ["workflows"]);
        var runList = Array.isArray(runs.items) ? runs.items : asArray(runs, ["runs"]);
        var runtime = runtimeResult.value;
        var runtimePanel = runtime
          ? panelHTML("Ansible 执行引擎", runtime.available
            ? '<p class="ops-sub">' + E(runtime.version || "Ansible Core") + '   平台服务端内置   可用</p>'
            : banner(runtime.error || "平台服务端 Ansible 运行时不可用。"))
          : panelHTML("Ansible 执行引擎", banner("无法读取平台服务端运行时状态。"));
        var wfTable = WFS.length ? tbl(["ID", "名称", "触发方式", "状态", "版本", "更新时间", "操作"], WFS.map(function (wf) {
          return [mono(wf.id), E(wf.name), E(wf.trigger || "—"), stTag(wf.status || "active"), E(wf.current_version != null ? "v" + wf.current_version : "—"), fmtMin(wf.updated_at),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-wftrig="' + E(wf.id) + '">触发</button><button class="x-btn" type="button" data-wfver="' + E(wf.id) + '">新版本</button></span>'];
        })) : emptyHTML("暂无流程定义。");
        var runTable = runList.length ? tbl(["运行", "流程", "版本", "状态", "触发人", "开始", "结束", "错误", "操作"], runList.map(function (r) {
          return [mono(r.id), E(r.workflow_id), E(r.version != null ? "v" + r.version : "—"), stTag(r.status), E(r.triggered_by || "—"), fmtMin(r.started_at), fmtMin(r.finished_at), code(r.error || "—", 60),
            '<span class="ops-nowrap"><button class="x-btn" type="button" data-runinfo="' + E(r.id) + '">详情</button>' +
            (r.status === "failed" && r.engine !== "ansible" ? '<button class="x-btn" type="button" data-runretry="' + E(r.id) + '">重试</button>' : "") +
            (r.status === "awaiting_approval" || r.status === "queued" || r.status === "running" ? '<button class="x-btn" type="button" data-runcancel="' + E(r.id) + '">取消</button>' : "") + "</span>"];
        })) : emptyHTML("暂无执行记录。");
        var html = runtimePanel + panelHTML("流程定义", wfTable, '<button class="x-btn x-btn--pri" type="button" data-wfnew style="margin-left:auto">+ 新建流程</button>') + panelHTML("执行记录", runTable);
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
          var id = b.getAttribute("data-wftrig");
          GET("/automation/workflows/" + encodeURIComponent(id)).then(function (wf) {
            var steps = Array.isArray(wf.steps) ? wf.steps : asArray(wf, ["steps"]);
            var needsAnsible = String(wf.playbook || "").trim() !== "" ||
              steps.some(function (step) { return String(step && step.type || "").indexOf("ansible.") === 0; });
            if (needsAnsible) { ansibleRunModal(wf, function () { load(); }); return; }
            confirmDo("触发流程", "确认按当前版本创建一次受控执行？", "触发", function () {
              write(POST("/automation/workflows/" + encodeURIComponent(id) + "/trigger", {}), "已触发，执行记录为 queued").then(function (r) { if (r) load(); });
            });
          }).catch(function (err) { toast(errMsg(err)); });
        };
      });
      [].slice.call(p.box.querySelectorAll("[data-runinfo]")).forEach(function (b) {
        b.onclick = function () {
          GET("/automation/runs/" + encodeURIComponent(b.getAttribute("data-runinfo"))).then(function (run) {
            infoModal("运行详情   #" + run.id, preBlock(run));
          }).catch(function (err) { toast(errMsg(err)); });
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
        { k: "steps", label: "简化步骤（Ansible builtin 模块）", kind: "textarea", rows: 8, value: '[\n  {"type": "ansible.ping", "name": "连通性检查"},\n  {"type": "ansible.package_present", "name": "确保 nginx 已安装", "package": "nginx"},\n  {"type": "ansible.service_started", "name": "确保 nginx 运行", "service": "nginx", "enabled": true}\n]' },
        { k: "playbook", label: "自定义 playbook.yml（填写后优先于简化步骤）", kind: "textarea", rows: 10, placeholder: '- name: Configure target hosts\n  hosts: all\n  gather_facts: false\n  tasks:\n    - name: Check connectivity\n      ansible.builtin.ping: {}' }
      ], "创建", function (v) {
        var steps;
        try { steps = parseJSON(v.steps || "[]", []); } catch (err) { toast(errMsg(err)); return; }
        write(POST("/automation/workflows", { name: v.name, description: v.description, trigger: v.trigger, steps: steps, playbook: v.playbook }), "流程已创建（v1）").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    function versionModal(id, done) {
      formModal("创建新版本", [
        { k: "steps", label: "简化步骤（Ansible builtin 模块）", kind: "textarea", rows: 8, value: '[\n  {"type": "ansible.ping", "name": "连通性检查"}\n]' },
        { k: "playbook", label: "自定义 playbook.yml（填写后优先于简化步骤）", kind: "textarea", rows: 10 }
      ], "创建版本", function (v) {
        var steps;
        try { steps = parseJSON(v.steps || "[]", []); } catch (err) { toast(errMsg(err)); return; }
        write(POST("/automation/workflows/" + encodeURIComponent(id) + "/versions", { steps: steps, playbook: v.playbook }), "新版本已创建").then(function (r) { if (r) { closeAll(); if (done) done(); } });
      });
    }
    function ansibleRunModal(wf, done) {
      var id = String(wf.id);
      formModal("运行 Ansible 流程   " + (wf.name || ("#" + id)), [
        { k: "env", label: "环境", required: true, placeholder: "prod" },
        { k: "ips", label: "目标 IP（逗号或换行分隔）", required: true, placeholder: "10.0.0.10, 10.0.0.11" },
        { k: "port", label: "SSH 端口", kind: "number", value: 22, required: true },
        { k: "username", label: "SSH 用户名", required: true, placeholder: "root" },
        { k: "password", label: "SSH 密码", kind: "password", required: true },
        { k: "become_password", label: "sudo 密码（可留空）", kind: "password" }
      ], "预检并继续", function (v) {
        var port = Number(v.port);
        if (!Number.isInteger(port) || port < 1 || port > 65535) { toast("SSH 端口须为 1 到 65535"); return; }
        write(POST("/automation/ansible/preflight", {
          env: v.env, ips: v.ips, port: port, username: v.username,
          password: v.password, become_password: v.become_password
        }), "SSH 预检完成").then(function (preflight) {
          if (!preflight || preflight.__approval) return;
          var items = asArray(preflight, ["items"]);
          if (!items.length) { toast("SSH 预检没有返回目标主机"); return; }
          var fingerprints = items.map(function (host) { return E(host.ip) + "   " + E(host.fingerprint); }).join("<br>");
          var message = "请核对目标 IP 对应的 SSH 主机密钥指纹。确认后凭据仅用于本次运行，并在结束后清除。<br><br>" + fingerprints;
          confirmDo("确认 SSH 主机密钥", message, "执行流程", function () {
            closeAll();
            write(POST("/automation/workflows/" + encodeURIComponent(id) + "/trigger", {
              env: v.env, preflight_token: preflight.confirmation_token
            }),
              "Ansible 流程已入队").then(function (result) { if (result && done) done(); });
          });
        });
      }, "平台服务端内置 Ansible；目标机不需要安装 Ansible。自定义 Playbook 限定为受支持的 ansible.builtin 模块并必须使用 hosts: all；目标机需提供 Python，非 root 用户需有 sudo 权限。SSH 密钥预检十分钟有效且只能使用一次。");
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
  window.XSLiveOpsRules = obsRules;
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
      parseJenkinsParameters: parseJenkinsParameters, buildPipelineStages: buildPipelineStages,
      systemConfigSchema: systemConfigSchema, systemConfigPanels: systemConfigPanels,
      systemConfigPanelResult: systemConfigPanelResult,
      toISO: toISO, dtLocal: dtLocal, memberList: memberList,
      tabsHTML: tabsHTML, stTag: stTag, sevTag: sevTag, fieldHTML: fieldHTML,
      routes: ROUTE_KEYS, renderers: RENDERERS
    }
  };
  try { boot(); } catch (e) { console.error("[xs-live-ops] boot", e); }
})();
