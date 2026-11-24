/*
 * 星枢 Matrix · AI 运维工作台（v1.33，window.XSAiopsUI）
 *
 * 页面（路由 #/aiops/<key>）：
 *   cases       排查工单列表（状态筛选、搜索、分页、新建排查、纳管主机）
 *   case/<id>   工单详情（结论、证据、方案审阅、执行方式、执行进度、存为预案）
 *   runbooks    预案库（匹配条件、自动处置开关、删除）
 *   policy      策略设置（自动排查、自动处置上限、变更窗口、模型）
 *
 * index.html 调用：
 *   XSAiopsUI.has(key) / XSAiopsUI.render(key, el)
 *   XSAiopsUI.mountAlertButton(modal, alert)   在告警详情里加「AI 排查」按钮
 *   XSAiopsUI.openOnboard()                    打开主机纳管抽屉
 *
 * 旧的告警诊断挂载 xs-aiops.js（window.XSAIOps）保持不变。
 * 凭据（密码/私钥）只在提交瞬间读取，读取后立即清空输入框，不写入任何状态、日志或提示。
 */
(function (root) {
  "use strict";
  var BASE = "/aiops";
  var POLL_MS = 3000;

  /* ---------------- 纯函数（可在 node 中测试） ---------------- */

  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }

  var CASE_ST = {
    investigating: ["排查中", "x-blue"], concluded: ["待审阅", "x-orange"], plan_accepted: ["已采纳方案", "x-blue"],
    awaiting_approval: ["等待审批", "x-orange"], executing: ["执行中", "x-blue"], verifying: ["验证中", "x-blue"],
    paused: ["已暂停", "x-grey"], resolved: ["已解决", "x-green"], failed: ["失败", "x-red"],
    cancelled: ["已终止", "x-grey"], rejected: ["方案已驳回", "x-red"], closed: ["已关闭", "x-grey"]
  };
  var CASE_FILTER = [
    { v: "", t: "全部" }, { v: "investigating", t: "排查中" }, { v: "concluded", t: "待审阅" },
    { v: "awaiting_approval", t: "等待审批" }, { v: "executing", t: "执行中" }, { v: "resolved", t: "已解决" },
    { v: "failed", t: "失败" }, { v: "rejected", t: "已驳回" }
  ];
  var ACTIVE = ["investigating", "plan_accepted", "awaiting_approval", "executing", "verifying"];
  var STEP_ST = {
    pending: ["待执行", "x-grey"], running: ["执行中", "x-blue"], succeeded: ["成功", "x-green"], failed: ["失败", "x-red"],
    skipped: ["已跳过", "x-grey"], blocked: ["已拦截", "x-red"], manual_done: ["人工完成", "x-green"], rolled_back: ["已回滚", "x-orange"]
  };
  var RISK = { read: ["只读", "x-green"], change: ["变更", "x-orange"], high: ["高风险", "x-red"] };
  var PLAN_ST = { proposed: ["待审阅", "x-orange"], accepted: ["已采纳", "x-green"], rejected: ["已驳回", "x-red"], superseded: ["已替换", "x-grey"] };
  var CONF = { low: "低", medium: "中", high: "高" };
  var KIND = { alert: "告警", deploy: "部署", request: "运维请求" };
  var SOURCE = { alert: "告警", chat: "对话", manual: "手动", auto: "自动策略" };
  var PHASE = { main: "处置", verify: "验证", rollback: "回滚" };
  var MODE = [{ v: "ai", t: "AI 执行", d: "平台按顺序自动执行全部步骤" }, { v: "manual", t: "人工清单", d: "按清单手工操作并逐项确认" }, { v: "mixed", t: "混合", d: "逐步指定由 AI 或人工执行" }];
  var EV_ST = { ok: ["正常", "x-green"], warn: ["警告", "x-orange"], error: ["异常", "x-red"], unavailable: ["不可用", "x-grey"] };
  var ENV = [{ v: "prod", t: "生产" }, { v: "pre", t: "预发" }, { v: "test", t: "测试" }, { v: "dev", t: "开发" }];

  function pick(map, v) { return map[String(v == null ? "" : v)] || null; }
  function caseStatusName(s) { var m = pick(CASE_ST, s); return m ? m[0] : (s ? String(s) : "—"); }
  function stepStatusName(s) { var m = pick(STEP_ST, s); return m ? m[0] : (s ? String(s) : "—"); }
  function riskName(r) { var m = pick(RISK, r); return m ? m[0] : (r ? String(r) : "—"); }
  function executorName(x) { return x === "human" ? "人工" : "AI"; }
  function isActive(s) { return ACTIVE.indexOf(s) >= 0; }
  function tag(text, cls) { return '<span class="x-tag x-tag--s ' + (cls || "x-grey") + '">' + E(text) + "</span>"; }
  function mapTag(map, v) { var m = pick(map, v); return m ? tag(m[0], m[1]) : (v ? tag(v) : "—"); }
  function caseTag(s) { return mapTag(CASE_ST, s); }
  function stepTag(s) { return mapTag(STEP_ST, s); }
  function riskTag(r) { return mapTag(RISK, r); }
  function envName(v) { var e = ENV.filter(function (x) { return x.v === v; })[0]; return e ? e.t : (v || "—"); }

  function actionTitle(actions, name, fallback) {
    var a = (actions || []).filter(function (x) { return x && x.name === name; })[0];
    return (a && a.title) || fallback || name || "—";
  }
  function parseJSON(v, dflt) {
    if (v == null || v === "") return dflt;
    if (typeof v === "object") return v;
    try { return JSON.parse(v); } catch (e) { return dflt; }
  }
  // 参数显示为「键：值」，凭据类字段一律打码
  var SECRET_KEY = /pass(word)?|secret|token|private_?key|credential/i;
  function paramsText(p) {
    var o = parseJSON(p, null);
    if (!o || typeof o !== "object") return o == null ? "—" : String(o);
    var keys = Object.keys(o);
    if (!keys.length) return "—";
    return keys.map(function (k) {
      var v = o[k];
      if (SECRET_KEY.test(k)) v = "******";
      else if (v && typeof v === "object") v = JSON.stringify(v);
      return k + "：" + v;
    }).join("；");
  }
  function maxRisk(steps) {
    var order = { read: 0, change: 1, high: 2 }, r = "read";
    (steps || []).forEach(function (s) { if ((order[s.risk] || 0) > order[r]) r = s.risk; });
    return r;
  }
  function canAutoExecute(risk) { return risk !== "high"; }

  /*
   * 步骤表 HTML。opts: { actions, edit:bool（参数/执行者可编辑）, mixed:bool（显示执行者选择）,
   *   controls:fn(step)→html（执行期按钮）, highlightFailed:bool }
   */
  function stepRows(steps, opts) {
    opts = opts || {};
    var list = (steps || []).slice().sort(function (a, b) { return (a.phase === "verify") - (b.phase === "verify") || (a.seq || 0) - (b.seq || 0); });
    if (!list.length) return '<p class="xai-empty-s">该方案没有步骤</p>';
    var cols = ["#", "动作", "阶段", "参数", "风险", "执行者", "状态"];
    if (opts.controls) cols.push("操作");
    var rows = list.map(function (s, i) {
      var bad = s.status === "failed" || s.status === "blocked";
      var exec = s.executor || "ai";
      var execCell = (opts.edit || opts.mixed) && s.phase !== "verify"
        ? '<select class="x-input x-input--s" data-step-exec="' + E(s.id) + '"><option value="ai"' + (exec !== "human" ? " selected" : "") + '>AI</option><option value="human"' + (exec === "human" ? " selected" : "") + ">人工</option></select>"
        : E(executorName(exec));
      var paramCell = opts.edit && s.phase !== "verify"
        ? '<textarea class="x-input xai-mono xai-params" rows="2" data-step-params="' + E(s.id) + '">' + E(JSON.stringify(parseJSON(s.params, {}), null, 0)) + "</textarea>"
        : '<span class="xai-params-t">' + E(paramsText(s.params)) + "</span>";
      var title = '<b>' + E(s.title || actionTitle(opts.actions, s.action)) + "</b>" +
        (s.error ? '<div class="xai-err-t">' + E(s.error) + "</div>" : "");
      var cells = [String(i + 1), title, E(PHASE[s.phase] || PHASE.main), paramCell, riskTag(s.risk), execCell, stepTag(s.status || "pending")];
      if (opts.controls) cells.push(opts.controls(s) || "—");
      return '<tr data-step-row="' + E(s.id) + '"' + (bad && opts.highlightFailed !== false ? ' class="xai-row-bad"' : "") + ">" +
        cells.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>";
    });
    return '<div class="xai-table"><table><thead><tr>' + cols.map(function (c) { return "<th>" + E(c) + "</th>"; }).join("") +
      "</tr></thead><tbody>" + rows.join("") + "</tbody></table></div>";
  }

  // 读取步骤表中被修改的参数与执行者；参数 JSON 不合法时抛出中文错误
  function collectStepEdits(container, steps) {
    var out = [];
    (steps || []).forEach(function (s) {
      if (s.phase === "verify") return;
      var p = container.querySelector('[data-step-params="' + s.id + '"]');
      var x = container.querySelector('[data-step-exec="' + s.id + '"]');
      var item = { id: s.id };
      if (p) {
        var v;
        try { v = JSON.parse(p.value || "{}"); } catch (e) { throw new Error("第 " + (s.seq || "?") + " 步的参数不是合法的 JSON"); }
        item.params = v;
      }
      if (x) item.executor = x.value === "human" ? "human" : "ai";
      out.push(item);
    });
    return out;
  }

  var SECRET_FIELDS = ["password", "private_key", "passphrase"];
  // 清空表单中的凭据字段；返回被清空的字段名（用于测试）
  function clearSecretFields(form) {
    var cleared = [];
    if (!form) return cleared;
    SECRET_FIELDS.forEach(function (n) {
      var f = form.elements ? form.elements[n] : null;
      if (!f && form.querySelector) f = form.querySelector('[name="' + n + '"]');
      if (f && "value" in f) { f.value = ""; cleared.push(n); }
    });
    return cleared;
  }
  // 从纳管表单读出提交体，并在返回前清空凭据输入框
  function onboardPayload(form) {
    var el = form.elements || {};
    var val = function (n) { return el[n] && el[n].value != null ? String(el[n].value).trim() : ""; };
    var body = {
      name: val("name") || val("address"),
      address: val("address"),
      port: parseInt(val("port"), 10) || 22,
      env: val("env") || "test",
      username: val("username")
    };
    var auth = val("auth") || "password";
    var pw = el.password ? String(el.password.value || "") : "";
    var pk = el.private_key ? String(el.private_key.value || "") : "";
    clearSecretFields(form);
    if (auth === "key") body.private_key = pk; else body.password = pw;
    return body;
  }
  function validateOnboard(b) {
    if (!b.address || !/^[A-Za-z0-9.:\-]+$/.test(b.address)) return "请填写正确的主机 IP 或域名";
    if (!(b.port >= 1 && b.port <= 65535)) return "端口需为 1–65535";
    if (!b.username) return "请填写登录账号";
    if (!b.password && !b.private_key) return "请填写密码或私钥";
    return "";
  }

  var ERR_ZH = [
    [/permission denied|forbidden/i, "没有执行该操作的权限"],
    [/^not found$|record not found/i, "记录不存在或已被删除"],
    [/change window/i, "当前不在变更窗口内，高风险操作暂不能执行"],
    [/approval/i, "该操作需要审批，已提交审批单"],
    [/unknown action/i, "方案包含未注册的动作"],
    [/invalid params|invalid request body/i, "提交内容格式不正确"],
    [/high risk|auto_execute/i, "高风险预案不能开启自动处置"],
    [/not executing|invalid state|status/i, "当前状态不允许该操作"],
    [/already exists|duplicate key/i, "名称已存在"],
    [/remote exec.*unavailable|not configured/i, "执行通道未配置"],
    [/timeout|timed out/i, "请求超时，请稍后重试"],
    [/^请求失败（HTTP 403）$/, "没有权限"]
  ];
  function errText(e) {
    var m = (e && (e.message || e.error)) || String(e || "未知错误");
    for (var i = 0; i < ERR_ZH.length; i++) if (ERR_ZH[i][0].test(m)) return ERR_ZH[i][1];
    if (e && e.status === 403 && !/[\u4e00-\u9fa5]/.test(m)) return "没有权限";
    if (e && e.status === 404 && !/[\u4e00-\u9fa5]/.test(m)) return "记录不存在或功能未开放";
    // 不把后端路径透出到界面
    return m.replace(/\/api\/v1\S*/g, "").trim() || "请求失败";
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(t) {
    if (!t) return "—";
    var d = new Date(t);
    if (isNaN(d)) return String(t);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }
  function items(r) { return Array.isArray(r) ? r : (r && (r.items || r.data || r.list)) || []; }
  function total(r, arr) { return (r && (r.total != null ? r.total : r.count)) != null ? (r.total != null ? r.total : r.count) : arr.length; }

  function normalizePolicy(p) {
    p = p || {};
    var ai = p.auto_investigate || {}, ae = p.auto_execute || {}, cw = p.change_window || {};
    return {
      auto_investigate: { enabled: !!ai.enabled, severities: Array.isArray(ai.severities) ? ai.severities : [], envs: Array.isArray(ai.envs) ? ai.envs : [] },
      auto_execute: { enabled: !!ae.enabled, max_per_hour: Math.max(0, parseInt(ae.max_per_hour, 10) || 0) },
      change_window: { enabled: !!cw.enabled, cron_like: cw.cron_like || "Mon-Fri 09:00-21:00" },
      model_id: p.model_id == null ? null : p.model_id
    };
  }

  /* ---------------- 浏览器端 ---------------- */

  function XS() { return root.XS; }
  function api(method, path, body) {
    var x = XS();
    if (!x) return Promise.reject(new Error("接口未就绪"));
    var p = BASE + path;
    if (method === "GET") return x.get(p, body);
    if (method === "POST") return x.post(p, body || {});
    if (method === "PUT") return x.put(p, body || {});
    return x.del(p);
  }
  function can(perm) { var x = XS(); return !x || typeof x.can !== "function" ? true : !!x.can(perm); }
  function toast(m) { var x = XS(); if (x && x.toast) x.toast(m); }
  function go(k) { location.hash = "#/aiops/" + k; }
  function on(el, attr, fn) { [].forEach.call(el.querySelectorAll("[" + attr + "]"), function (b) { b.onclick = function (ev) { fn(b.getAttribute(attr), b, ev); }; }); }
  function empty(kind, title, desc, actions) {
    if (root.XSEmpty) return root.XSEmpty.html({ kind: kind, title: title, desc: desc, actions: actions || [], size: "s" });
    return '<p class="xai-empty-s">' + E(title) + "</p>";
  }
  function head(title, desc, acts) {
    return '<div class="xai-head"><div><h2>' + E(title) + "</h2>" + (desc ? "<p>" + E(desc) + "</p>" : "") + "</div>" + (acts ? '<div class="xai-head-act">' + acts + "</div>" : "") + "</div>";
  }
  function tabs(cur) {
    var t = [{ k: "cases", t: "排查工单" }, { k: "runbooks", t: "预案库" }];
    if (can("aiops:manage")) t.push({ k: "policy", t: "策略设置" });
    return '<div class="xai-tabs" role="tablist">' + t.map(function (x) {
      return '<a role="tab" href="#/aiops/' + x.k + '" class="xai-tab' + (x.k === cur ? " on" : "") + '" aria-selected="' + (x.k === cur) + '">' + E(x.t) + "</a>";
    }).join("") + "</div>";
  }

  // 简易弹窗：opts { title, body, ok, danger, onOk(dialogEl) → Promise|false }
  function modal(opts) {
    var wrap = document.createElement("div");
    wrap.className = "xai-mask";
    wrap.innerHTML = '<div class="xai-modal" role="dialog" aria-modal="true" aria-label="' + E(opts.title) + '"><div class="xai-mhead"><span>' + E(opts.title) +
      '</span><button type="button" class="x-x" data-m-x aria-label="关闭">×</button></div><div class="xai-mbody">' + (opts.body || "") +
      '<p class="xai-merr" role="alert" hidden></p></div><div class="xai-mfoot"><button type="button" class="x-btn" data-m-x>取消</button><button type="button" class="x-btn ' +
      (opts.danger ? "x-btn--danger" : "x-btn--pri") + '" data-m-ok>' + E(opts.ok || "确定") + "</button></div></div>";
    document.body.appendChild(wrap);
    var close = function () { if (wrap.parentNode) wrap.parentNode.removeChild(wrap); };
    var errEl = wrap.querySelector(".xai-merr"), okBtn = wrap.querySelector("[data-m-ok]");
    on(wrap, "data-m-x", close);
    okBtn.onclick = function () {
      errEl.hidden = true;
      var r;
      try { r = opts.onOk ? opts.onOk(wrap) : null; } catch (e) { errEl.textContent = errText(e); errEl.hidden = false; return; }
      if (r === false) return;
      if (!r || typeof r.then !== "function") { close(); return; }
      okBtn.disabled = true;
      r.then(close, function (e) { okBtn.disabled = false; errEl.textContent = errText(e); errEl.hidden = false; });
    };
    var first = wrap.querySelector("input,textarea,select");
    if (first) first.focus();
    return wrap;
  }

  function drawer(title, body, foot) {
    var d = document.getElementById("drawer"), mask = document.getElementById("mask");
    var own = !d;
    if (own) {
      d = document.createElement("aside"); d.className = "x-drawer xai-drawer"; document.body.appendChild(d);
      mask = document.createElement("div"); mask.className = "xai-dmask"; document.body.appendChild(mask);
    }
    d.classList.add("x-drawer--wide");
    d.setAttribute("aria-label", title);
    d.innerHTML = '<div class="x-dhead"><span>' + E(title) + '</span><button class="x-x" type="button" data-d-x aria-label="关闭">×</button></div><div class="x-dbody">' + body + '</div><div class="x-dfoot">' + foot + "</div>";
    if (mask) { mask.hidden = false; mask.onclick = close; }
    function close() {
      d.classList.remove("open");
      if (mask) mask.hidden = true;
      if (own) { setTimeout(function () { if (d.parentNode) d.parentNode.removeChild(d); if (mask && mask.parentNode) mask.parentNode.removeChild(mask); }, 200); }
      else d.innerHTML = "";
    }
    on(d, "data-d-x", close);
    requestAnimationFrame(function () { d.classList.add("open"); });
    return { el: d, close: close };
  }

  /* ---------------- 共享缓存 ---------------- */
  var cache = { actions: null, clusters: null };
  function actions() {
    if (cache.actions) return Promise.resolve(cache.actions);
    return api("GET", "/actions").then(function (r) { cache.actions = items(r); return cache.actions; }, function () { return []; });
  }
  function clusters() {
    if (cache.clusters) return Promise.resolve(cache.clusters);
    var x = XS();
    if (!x) return Promise.resolve([]);
    return x.get("/clusters", { page_size: 500 }).then(function (r) { cache.clusters = items(r); return cache.clusters; }, function () { return []; });
  }
  function namespaces(cid) {
    var x = XS();
    if (!x || !cid) return Promise.resolve([]);
    return x.get("/clusters/" + encodeURIComponent(cid) + "/namespaces").then(function (r) {
      return items(r).map(function (n) { return typeof n === "string" ? n : (n.name || (n.metadata && n.metadata.name)); }).filter(Boolean);
    }, function () { return []; });
  }

  /* ---------------- 工单列表 ---------------- */
  var listState = { status: "", q: "", page: 1, size: 20 };

  function renderCases(el) {
    var acts = (can("aiops:handle") ? '<button class="x-btn x-btn--pri" type="button" data-ai-new>新建排查</button>' : "") +
      (can("aiops:execute") ? '<button class="x-btn" type="button" data-ai-onboard>纳管主机</button>' : "");
    el.innerHTML = '<div class="xai">' + head("AI 运维", "告警与运维请求由 AI 只读排查，给出结论与处置方案，经运维审阅后执行", acts) + tabs("cases") +
      '<div class="xai-card"><div class="xai-toolbar"><div class="xai-segs">' + CASE_FILTER.map(function (f) {
        return '<button type="button" class="x-seg" data-ai-st="' + E(f.v) + '" aria-pressed="' + (f.v === listState.status) + '">' + E(f.t) + "</button>";
      }).join("") + '</div><input class="x-input" type="search" placeholder="搜索标题或描述" data-ai-q value="' + E(listState.q) + '"></div><div data-ai-list><p class="xai-loading">正在读取…</p></div></div></div>';
    on(el, "data-ai-st", function (v) { listState.status = v; listState.page = 1; renderCases(el); });
    var q = el.querySelector("[data-ai-q]"), t;
    q.oninput = function () { clearTimeout(t); t = setTimeout(function () { listState.q = q.value.trim(); listState.page = 1; loadCases(el); }, 300); };
    var nb = el.querySelector("[data-ai-new]"); if (nb) nb.onclick = function () { openNewCase(); };
    var ob = el.querySelector("[data-ai-onboard]"); if (ob) ob.onclick = function () { openOnboard(); };
    loadCases(el);
  }

  function loadCases(el) {
    var box = el.querySelector("[data-ai-list]");
    api("GET", "/cases", { status: listState.status, q: listState.q, page: listState.page, size: listState.size }).then(function (r) {
      if (!box.isConnected) return;
      var arr = items(r), n = total(r, arr);
      if (!arr.length) {
        box.innerHTML = listState.q || listState.status ? empty("search", "没有匹配的排查工单", "换个筛选条件试试")
          : empty("generic", "还没有排查工单", "从告警详情点「AI 排查」，或在这里新建一次排查", can("aiops:handle") ? [{ text: "新建排查", act: "new", primary: true }] : []);
        if (root.XSEmpty && root.XSEmpty.bind) root.XSEmpty.bind(box, { "new": function () { openNewCase(); } });
        return;
      }
      var pages = Math.max(1, Math.ceil(n / listState.size));
      box.innerHTML = '<div class="xai-table"><table><thead><tr><th>编号</th><th>标题</th><th>类型</th><th>状态</th><th>环境</th><th>来源</th><th>发起人</th><th>创建时间</th></tr></thead><tbody>' +
        arr.map(function (c) {
          return "<tr><td class=\"xai-mono\">#" + E(c.id) + '</td><td><a class="x-tlink" href="#/aiops/case/' + E(c.id) + '">' + E(c.title || c.request || "未命名工单") + "</a></td><td>" +
            E(KIND[c.kind] || c.kind || "—") + "</td><td>" + caseTag(c.status) + "</td><td>" + E(envName(c.env)) + "</td><td>" + E(SOURCE[c.source] || c.source || "—") +
            "</td><td>" + E(c.created_name || "—") + "</td><td>" + E(fmtTime(c.created_at)) + "</td></tr>";
        }).join("") + '</tbody></table></div><div class="xai-pager"><span>共 ' + n + ' 条</span><button class="x-btn x-btn--s" data-pg="' + (listState.page - 1) + '"' +
        (listState.page <= 1 ? " disabled" : "") + '>上一页</button><span>' + listState.page + " / " + pages + '</span><button class="x-btn x-btn--s" data-pg="' + (listState.page + 1) + '"' +
        (listState.page >= pages ? " disabled" : "") + ">下一页</button></div>";
      on(box, "data-pg", function (v) { listState.page = +v; loadCases(el); });
    }, function (e) { if (box.isConnected) box.innerHTML = '<p class="xai-err" role="alert">读取失败：' + E(errText(e)) + "</p>"; });
  }

  function openNewCase() {
    var w = modal({
      title: "新建排查", ok: "开始排查",
      body: '<form class="xai-form" data-new-form onsubmit="return false">' +
        '<label><span class="x-req">问题描述</span><textarea class="x-input" name="request" rows="4" maxlength="2000" placeholder="例如：订单服务 Pod 反复重启，最近一次发布后出现"></textarea></label>' +
        '<div class="xai-row2"><label><span>集群</span><select class="x-input" name="cluster"><option value="">不指定</option></select></label>' +
        '<label><span>命名空间</span><input class="x-input" name="namespace" list="xai-ns-list" autocomplete="off" placeholder="不指定"><datalist id="xai-ns-list"></datalist></label></div>' +
        '<div class="xai-row2"><label><span>环境</span><select class="x-input" name="env">' + ENV.map(function (e) { return '<option value="' + e.v + '"' + (e.v === "test" ? " selected" : "") + ">" + e.t + "</option>"; }).join("") + "</select></label>" +
        '<label><span>类型</span><select class="x-input" name="kind"><option value="request">运维请求</option><option value="deploy">部署</option></select></label></div>' +
        '<p class="xai-hint">AI 只做只读排查，任何变更都需要你审阅方案后才会执行。</p></form>',
      onOk: function (m) {
        var f = m.querySelector("[data-new-form]").elements;
        var req = f.request.value.trim();
        if (!req) throw new Error("请填写问题描述");
        var body = { kind: f.kind.value, request: req, env: f.env.value };
        if (f.cluster.value) body.cluster_id = +f.cluster.value;
        if (f.namespace.value.trim()) body.target = { namespace: f.namespace.value.trim() };
        return api("POST", "/cases", body).then(function (c) { var id = c && (c.id || (c.case && c.case.id)); toast("已开始排查"); if (id) go("case/" + id); });
      }
    });
    var sel = w.querySelector('[name="cluster"]'), dl = w.querySelector("#xai-ns-list");
    clusters().then(function (cs) {
      sel.innerHTML += cs.map(function (c) { return '<option value="' + E(c.id) + '">' + E(c.name || ("集群 " + c.id)) + "</option>"; }).join("");
    });
    sel.onchange = function () {
      dl.innerHTML = "";
      namespaces(sel.value).then(function (ns) { dl.innerHTML = ns.map(function (n) { return '<option value="' + E(n) + '">'; }).join(""); });
    };
  }

  /* ---------------- 工单详情 ---------------- */
  var detail = { id: null, timer: null, data: null, editing: {}, mode: "ai", modeExec: {} };

  function stopPoll() { if (detail.timer) { clearTimeout(detail.timer); detail.timer = null; } }

  function renderCase(id, el) {
    stopPoll();
    if (detail.id !== id) detail = { id: id, timer: null, data: null, editing: {}, mode: "ai", modeExec: {} };
    el.innerHTML = '<div class="xai">' + head("排查工单 #" + id, "", '<a class="x-btn" href="#/aiops/cases">返回列表</a>') + '<div data-ai-detail><p class="xai-loading">正在读取…</p></div></div>';
    Promise.all([actions(), api("GET", "/cases/" + encodeURIComponent(id))]).then(function (r) {
      paintCase(el, r[1], r[0]);
    }, function (e) {
      var b = el.querySelector("[data-ai-detail]");
      if (b) b.innerHTML = '<p class="xai-err" role="alert">读取失败：' + E(errText(e)) + "</p>";
    });
  }

  function schedule(el, acts) {
    stopPoll();
    var c = detail.data && detail.data.case;
    if (!c || !isActive(c.status)) return;
    detail.timer = setTimeout(function () {
      if (!el.isConnected || location.hash.indexOf("#/aiops/case/" + detail.id) !== 0) { stopPoll(); return; }
      // 编辑参数时不刷新，避免覆盖输入
      if (Object.keys(detail.editing).length) { schedule(el, acts); return; }
      api("GET", "/cases/" + encodeURIComponent(detail.id)).then(function (d) { paintCase(el, d, acts); }, function () { schedule(el, acts); });
    }, POLL_MS);
  }

  function conclusionCard(c) {
    var cc = parseJSON(c.conclusion, {}) || {};
    var engine = c.engine === "rules" ? tag("规则引擎", "x-grey") : tag("大模型", "x-blue");
    if (c.status === "investigating" && !cc.summary) return '<div class="xai-card xai-concl"><div class="xai-card-h"><b>排查结论</b>' + engine + '</div><p class="xai-loading">AI 正在只读排查，通常需要几十秒…</p></div>';
    var conf = cc.confidence ? tag("置信度 " + (CONF[cc.confidence] || cc.confidence), cc.confidence === "high" ? "x-green" : cc.confidence === "medium" ? "x-orange" : "x-grey") : "";
    var li = function (arr) { return (arr || []).length ? "<ul>" + arr.map(function (x) { return "<li>" + E(x) + "</li>"; }).join("") + "</ul>" : ""; };
    return '<div class="xai-card xai-concl"><div class="xai-card-h"><b>排查结论</b>' + engine + conf + "</div>" +
      (cc.summary ? '<p class="xai-summary">' + E(cc.summary) + "</p>" : '<p class="xai-empty-s">暂无结论</p>') +
      '<dl class="xai-kv"><dt>根因</dt><dd>' + E(cc.root_cause || "不确定") + "</dd>" +
      (cc.impact ? "<dt>影响</dt><dd>" + E(cc.impact) + "</dd>" : "") +
      ((cc.hypotheses || []).length ? "<dt>其他可能</dt><dd>" + li(cc.hypotheses) + "</dd>" : "") +
      ((cc.missing_info || []).length ? "<dt>补充信息可提高置信度</dt><dd>" + li(cc.missing_info) + "</dd>" : "") + "</dl>" +
      (c.error ? '<p class="xai-err">' + E(c.error) + "</p>" : "") + "</div>";
  }

  function evidenceCard(c) {
    var ev = parseJSON(c.evidence, []) || [];
    if (!ev.length) return "";
    return '<div class="xai-card"><div class="xai-card-h"><b>排查证据</b><span class="xai-sub">共 ' + ev.length + " 项，均为只读检查</span></div>" +
      ev.map(function (x) {
        var data = x.data == null ? "" : '<pre class="xai-pre">' + E(typeof x.data === "string" ? x.data : JSON.stringify(x.data, null, 2)) + "</pre>";
        return '<details class="xai-ev"><summary>' + mapTag(EV_ST, x.status) + "<b>" + E(x.title || x.key) + "</b><span>" + E(x.summary || "") + "</span></summary>" + (data || '<p class="xai-empty-s">无更多数据</p>') + "</details>";
      }).join("") + "</div>";
  }

  function planCard(c, p, acts) {
    var editing = !!detail.editing[p.id];
    var reviewable = c.status === "concluded" && p.status === "proposed" && can("aiops:handle");
    var btns = "";
    if (reviewable) {
      btns = editing
        ? '<button class="x-btn x-btn--pri x-btn--s" data-plan-accept="' + p.id + '">按修改后的参数采纳</button><button class="x-btn x-btn--s" data-plan-cancel="' + p.id + '">取消修改</button>'
        : '<button class="x-btn x-btn--pri x-btn--s" data-plan-accept="' + p.id + '">采纳</button><button class="x-btn x-btn--s" data-plan-edit="' + p.id + '">修改参数</button><button class="x-btn x-btn--s" data-plan-reject="' + p.id + '">驳回</button>';
    }
    return '<div class="xai-card xai-plan' + (p.status === "accepted" ? " is-on" : "") + '"><div class="xai-card-h"><b>方案 ' + E(p.seq || "") + "：" + E(p.title || "") + "</b>" +
      mapTag(PLAN_ST, p.status) + riskTag(p.risk || maxRisk(p.steps)) + "</div>" +
      (p.description ? "<p>" + E(p.description) + "</p>" : "") +
      '<dl class="xai-kv xai-kv--row">' + (p.impact ? "<dt>影响</dt><dd>" + E(p.impact) + "</dd>" : "") +
      (p.estimate_sec ? "<dt>预计耗时</dt><dd>约 " + Math.max(1, Math.round(p.estimate_sec / 60)) + " 分钟</dd>" : "") +
      (p.rollback ? "<dt>回滚方式</dt><dd>" + E(p.rollback) + "</dd>" : "") + "</dl>" +
      stepRows(p.steps, { actions: acts, edit: editing }) +
      (p.reject_note ? '<p class="xai-sub">驳回理由：' + E(p.reject_note) + "</p>" : "") +
      (btns ? '<div class="xai-btns">' + btns + "</div>" : "") + "</div>";
  }

  function modeCard(c, plan) {
    if (c.status !== "plan_accepted" || !plan || !can("aiops:execute")) return "";
    var high = (plan.risk || maxRisk(plan.steps)) === "high";
    return '<div class="xai-card"><div class="xai-card-h"><b>选择执行方式</b></div><div class="xai-modes">' + MODE.map(function (m) {
      return '<label class="xai-mode' + (detail.mode === m.v ? " on" : "") + '"><input type="radio" name="xai-mode" value="' + m.v + '"' + (detail.mode === m.v ? " checked" : "") + "><b>" + E(m.t) + "</b><span>" + E(m.d) + "</span></label>";
    }).join("") + "</div>" + (detail.mode === "mixed" ? stepRows(plan.steps, { mixed: true }) : "") +
      (high ? '<p class="xai-warn">该方案包含高风险步骤，提交后需要第二人审批，并且只能在变更窗口内执行。</p>' : "") +
      '<div class="xai-btns"><button class="x-btn x-btn--pri" data-exec-go>' + (high ? "提交审批" : "开始执行") + "</button></div></div>";
  }

  function stepControls(c, s) {
    var running = ["executing", "verifying", "paused"].indexOf(c.status) >= 0;
    if (!running || !can("aiops:execute")) return "";
    if (s.status !== "pending" && s.status !== "running") return "";
    var human = c.mode === "manual" || (c.mode === "mixed" && s.executor === "human");
    if (human) {
      return '<button class="x-btn x-btn--s" data-man="done:' + s.id + '">完成</button><button class="x-btn x-btn--s" data-man="skipped:' + s.id + '">跳过</button><button class="x-btn x-btn--s" data-man="failed:' + s.id + '">失败</button>';
    }
    if (c.mode === "mixed" && s.status === "pending") return '<button class="x-btn x-btn--s" data-run-step="' + s.id + '">执行此步</button>';
    return "";
  }

  function progressCard(c, plan, timeline) {
    if (!plan || ["executing", "verifying", "paused", "awaiting_approval", "resolved", "failed", "cancelled"].indexOf(c.status) < 0 || !c.mode) return "";
    var ctl = [];
    if (can("aiops:execute")) {
      if (c.status === "executing" || c.status === "verifying") ctl.push('<button class="x-btn x-btn--s" data-ctl="pause">暂停</button>');
      if (c.status === "paused") ctl.push('<button class="x-btn x-btn--s" data-ctl="resume">继续</button>');
      if (["executing", "verifying", "paused", "awaiting_approval"].indexOf(c.status) >= 0) ctl.push('<button class="x-btn x-btn--s x-btn--danger" data-ctl="abort">终止</button>');
      if (["failed", "executing", "paused", "resolved"].indexOf(c.status) >= 0) ctl.push('<button class="x-btn x-btn--s" data-ctl="rollback">回滚</button>');
    }
    var manualHints = (plan.steps || []).filter(function (s) { return s.manual && (c.mode === "manual" || s.executor === "human") && (s.status === "pending" || s.status === "running"); });
    var tl = (timeline || []).slice(-50);
    return '<div class="xai-card"><div class="xai-card-h"><b>执行进度</b>' + caseTag(c.status) + tag(MODE.filter(function (m) { return m.v === c.mode; }).map(function (m) { return m.t; })[0] || c.mode, "x-grey") +
      (ctl.length ? '<span class="xai-grow"></span>' + ctl.join("") : "") + "</div>" +
      (c.status === "awaiting_approval" ? '<p class="xai-warn">等待审批' + (c.approval_id ? "（审批单 #" + E(c.approval_id) + "）" : "") + "，审批通过后自动开始执行。</p>" : "") +
      stepRows(plan.steps, { controls: function (s) { return stepControls(c, s); } }) +
      (manualHints.length ? '<div class="xai-manual"><b>人工操作说明</b>' + manualHints.map(function (s) { return "<div><span>" + E(s.title || s.action) + '</span><pre class="xai-pre">' + E(s.manual) + "</pre></div>"; }).join("") + "</div>" : "") +
      (tl.length ? '<ol class="xai-tl">' + tl.map(function (t) {
        var bad = /fail|error|blocked/i.test(t.status || t.kind || "");
        return '<li class="' + (bad ? "bad" : "") + '"><time>' + E(fmtTime(t.at || t.created_at)) + "</time><span>" + E(t.message || t.title || t.kind || "") + "</span>" + (t.actor ? "<em>" + E(t.actor) + "</em>" : "") + "</li>";
      }).join("") + "</ol>" : "") + "</div>";
  }

  function paintCase(el, d, acts) {
    var box = el.querySelector("[data-ai-detail]");
    if (!box) return;
    d = d || {};
    var c = d.case || d, plans = d.plans || [], timeline = d.timeline || [];
    detail.data = { case: c, plans: plans, timeline: timeline };
    var h2 = el.querySelector(".xai-head h2");
    if (h2) h2.textContent = (c.title || "排查工单") + "  #" + c.id;
    var plan = plans.filter(function (p) { return String(p.id) === String(c.plan_id) || p.status === "accepted"; })[0];
    var tail = [];
    if (can("aiops:handle") && ["concluded", "rejected", "failed"].indexOf(c.status) >= 0) tail.push('<button class="x-btn" data-case-reinv>补充信息重新排查</button>');
    if (can("aiops:manage") && plan && ["resolved"].indexOf(c.status) >= 0 && !c.runbook_id) tail.push('<button class="x-btn x-btn--pri" data-case-save>存为预案</button>');
    if (can("aiops:handle") && ["resolved", "failed", "rejected", "concluded", "cancelled"].indexOf(c.status) >= 0) tail.push('<button class="x-btn" data-case-close>关闭工单</button>');
    box.innerHTML = '<div class="xai-meta">' + caseTag(c.status) + "<span>" + E(KIND[c.kind] || c.kind || "") + "</span><span>环境：" + E(envName(c.env)) + "</span><span>发起人：" + E(c.created_name || "—") +
      "</span><span>" + E(fmtTime(c.created_at)) + "</span>" + (c.reviewed_by ? "<span>审阅人：" + E(c.reviewed_by) + "</span>" : "") + "</div>" +
      (c.request ? '<div class="xai-card"><div class="xai-card-h"><b>问题描述</b></div><p class="xai-pre-wrap">' + E(c.request) + "</p></div>" : "") +
      conclusionCard(c) + evidenceCard(c) +
      (plans.length ? '<h3 class="xai-h3">处置方案</h3>' + plans.map(function (p) { return planCard(c, p, acts); }).join("") : (c.status === "investigating" ? "" : '<div class="xai-card">' + empty("generic", "没有可执行的方案", "可以补充信息后重新排查") + "</div>")) +
      modeCard(c, plan) + progressCard(c, plan, timeline) +
      (tail.length ? '<div class="xai-btns xai-tail">' + tail.join("") + "</div>" : "");
    bindCase(el, box, c, plans, plan, acts);
    schedule(el, acts);
  }

  function act(el, acts, p, okMsg) {
    return p.then(function (r) { if (okMsg) toast(okMsg); renderCase(detail.id, el); return r; });
  }

  function bindCase(el, box, c, plans, plan, acts) {
    var cid = encodeURIComponent(c.id);
    var findPlan = function (id) { return plans.filter(function (p) { return String(p.id) === String(id); })[0]; };
    on(box, "data-plan-edit", function (id) { detail.editing[id] = true; paintCase(el, { case: c, plans: plans, timeline: detail.data.timeline }, acts); });
    on(box, "data-plan-cancel", function (id) { delete detail.editing[id]; paintCase(el, { case: c, plans: plans, timeline: detail.data.timeline }, acts); });
    on(box, "data-plan-accept", function (id, b) {
      var p = findPlan(id), body = { decision: "accept", note: "" };
      try { if (detail.editing[id]) body.steps = collectStepEdits(box, p.steps); } catch (e) { toast(e.message); return; }
      b.disabled = true;
      api("POST", "/cases/" + cid + "/plans/" + encodeURIComponent(id) + "/review", body).then(function () {
        delete detail.editing[id]; toast("已采纳方案，请选择执行方式"); renderCase(detail.id, el);
      }, function (e) { b.disabled = false; toast(errText(e)); });
    });
    on(box, "data-plan-reject", function (id) {
      modal({
        title: "驳回方案", ok: "驳回", danger: true,
        body: '<label class="xai-form"><span class="x-req">驳回理由</span><textarea class="x-input" rows="3" maxlength="500" data-rj placeholder="说明为什么不采纳，AI 重新排查时会参考"></textarea></label>',
        onOk: function (m) {
          var note = m.querySelector("[data-rj]").value.trim();
          if (!note) throw new Error("请填写驳回理由");
          return act(el, acts, api("POST", "/cases/" + cid + "/plans/" + encodeURIComponent(id) + "/review", { decision: "reject", note: note }), "已驳回");
        }
      });
    });
    [].forEach.call(box.querySelectorAll('input[name="xai-mode"]'), function (r) {
      r.onchange = function () { detail.mode = r.value; paintCase(el, { case: c, plans: plans, timeline: detail.data.timeline }, acts); };
    });
    var goBtn = box.querySelector("[data-exec-go]");
    if (goBtn) goBtn.onclick = function () {
      var body = { plan_id: plan.id, mode: detail.mode };
      if (detail.mode === "mixed") {
        body.executors = {};
        [].forEach.call(box.querySelectorAll("[data-step-exec]"), function (s) { body.executors[s.getAttribute("data-step-exec")] = s.value === "human" ? "human" : "ai"; });
      }
      modal({
        title: "确认执行", ok: "确认",
        body: "<p>将按「" + E(MODE.filter(function (m) { return m.v === detail.mode; })[0].t) + "」方式执行方案「" + E(plan.title) + "」，共 " + (plan.steps || []).length + " 步。</p><p class=\"xai-hint\">执行前平台会再次校验参数、权限与目标资源；失败步骤可回滚。</p>",
        onOk: function () {
          return api("POST", "/cases/" + cid + "/execute", body).then(function (r) {
            var st = r && (r.status || (r.case && r.case.status));
            toast(st === "awaiting_approval" ? "已提交审批，审批通过后开始执行" : "已开始执行");
            renderCase(detail.id, el);
          });
        }
      });
    };
    on(box, "data-run-step", function (sid, b) {
      b.disabled = true;
      act(el, acts, api("POST", "/cases/" + cid + "/steps/" + encodeURIComponent(sid) + "/run"), "已开始执行该步骤").catch(function (e) { b.disabled = false; toast(errText(e)); });
    });
    on(box, "data-man", function (v) {
      var parts = v.split(":"), st = parts[0], sid = parts[1];
      var name = { done: "完成", skipped: "跳过", failed: "失败" }[st];
      modal({
        title: "标记为" + name, ok: "确认", danger: st === "failed",
        body: '<label class="xai-form"><span' + (st === "done" ? "" : ' class="x-req"') + ">备注</span><textarea class=\"x-input\" rows=\"3\" maxlength=\"500\" data-mn placeholder=\"" + (st === "done" ? "可填写执行结果" : "请说明原因") + "\"></textarea></label>",
        onOk: function (m) {
          var note = m.querySelector("[data-mn]").value.trim();
          if (st !== "done" && !note) throw new Error("请说明原因");
          return act(el, acts, api("POST", "/cases/" + cid + "/steps/" + encodeURIComponent(sid) + "/manual", { status: st, note: note }), "已记录");
        }
      });
    });
    on(box, "data-ctl", function (k) {
      var name = { pause: "暂停", resume: "继续", abort: "终止", rollback: "回滚" }[k];
      var run = function () { return act(el, acts, api("POST", "/cases/" + cid + "/" + k), "已" + name); };
      if (k === "pause" || k === "resume") { run().catch(function (e) { toast(errText(e)); }); return; }
      modal({
        title: "确认" + name, ok: name, danger: true,
        body: k === "abort" ? "<p>终止后剩余步骤不再执行，已执行的步骤不会自动撤销。</p>" : "<p>将按逆序回滚已成功执行的步骤，无法回滚的步骤会在进度中标出。</p>",
        onOk: run
      });
    });
    var re = box.querySelector("[data-case-reinv]");
    if (re) re.onclick = function () {
      modal({
        title: "重新排查", ok: "开始排查",
        body: '<label class="xai-form"><span>补充信息</span><textarea class="x-input" rows="3" maxlength="1000" data-ri placeholder="例如：上周改过数据库连接池配置"></textarea></label>',
        onOk: function (m) { return act(el, acts, api("POST", "/cases/" + cid + "/reinvestigate", { note: m.querySelector("[data-ri]").value.trim() }), "已重新开始排查"); }
      });
    };
    var cl = box.querySelector("[data-case-close]");
    if (cl) cl.onclick = function () {
      modal({
        title: "关闭工单", ok: "关闭",
        body: '<label class="xai-form"><span>处理结论</span><textarea class="x-input" rows="3" maxlength="500" data-res placeholder="例如：已扩容并确认恢复"></textarea></label>',
        onOk: function (m) { return act(el, acts, api("POST", "/cases/" + cid + "/close", { resolution: m.querySelector("[data-res]").value.trim() }), "工单已关闭"); }
      });
    };
    var sv = box.querySelector("[data-case-save]");
    if (sv) sv.onclick = function () { openSaveRunbook(el, acts, c, plan); };
  }

  function guessMatch(c) {
    var cc = parseJSON(c.conclusion, {}) || {}, m = {};
    var f = (cc.findings || [])[0];
    if (f && f.analyzer) m.analyzer = f.analyzer;
    if (c.env) m.env = c.env;
    return m;
  }

  function matchFields(m) {
    m = m || {};
    return '<div class="xai-row2"><label><span>告警名称</span><input class="x-input xai-mono" name="m_alertname" value="' + E(m.alertname || "") + '" placeholder="如 KubePodCrashLooping"></label>' +
      '<label><span>分析规则</span><input class="x-input xai-mono" name="m_analyzer" value="' + E(m.analyzer || "") + '" placeholder="如 crashloop"></label></div>' +
      '<label><span>环境</span><select class="x-input" name="m_env"><option value="">不限</option>' + ENV.map(function (e) { return '<option value="' + e.v + '"' + (m.env === e.v ? " selected" : "") + ">" + e.t + "</option>"; }).join("") + "</select></label>";
  }
  function readMatch(f) {
    var m = {};
    ["alertname", "analyzer", "env"].forEach(function (k) { var v = f["m_" + k] && f["m_" + k].value.trim(); if (v) m[k] = v; });
    return m;
  }
  function autoFields(risk, on_, scope) {
    scope = scope || {};
    var dis = !canAutoExecute(risk);
    return '<label class="xai-check"><input type="checkbox" name="auto_execute"' + (on_ && !dis ? " checked" : "") + (dis ? " disabled" : "") + "> 命中时自动处置" + (dis ? '<em class="xai-sub">（高风险预案不能自动处置）</em>' : "") + "</label>" +
      '<div class="xai-row2"><label><span>自动处置环境</span><input class="x-input" name="s_envs" value="' + E((scope.envs || []).join(",")) + '" placeholder="如 test,dev，留空为不限"></label>' +
      '<label><span>自动处置时段</span><input class="x-input xai-mono" name="s_hours" value="' + E(scope.hours || "") + '" placeholder="如 09-21"></label></div>';
  }
  function readScope(f) {
    var s = {};
    var envs = f.s_envs.value.split(/[,，\s]+/).filter(Boolean);
    if (envs.length) s.envs = envs;
    if (f.s_hours.value.trim()) s.hours = f.s_hours.value.trim();
    return s;
  }

  function openSaveRunbook(el, acts, c, plan) {
    var risk = plan.risk || maxRisk(plan.steps);
    modal({
      title: "存为预案", ok: "保存",
      body: '<form class="xai-form" data-rb onsubmit="return false"><label><span class="x-req">预案名称</span><input class="x-input" name="name" maxlength="64" value="' + E(plan.title || c.title || "") + '"></label>' +
        "<h4>匹配条件</h4>" + matchFields(guessMatch(c)) + "<h4>自动处置</h4>" + autoFields(risk, false, {}) + "</form>",
      onOk: function (m) {
        var f = m.querySelector("[data-rb]").elements;
        if (!f.name.value.trim()) throw new Error("请填写预案名称");
        var match = readMatch(f);
        if (!Object.keys(match).length) throw new Error("请至少填写一个匹配条件");
        return act(el, acts, api("POST", "/cases/" + encodeURIComponent(c.id) + "/save-runbook", {
          name: f.name.value.trim(), match: match, auto_execute: canAutoExecute(risk) && f.auto_execute.checked, auto_scope: readScope(f)
        }), "已存为预案");
      }
    });
  }

  /* ---------------- 预案库 ---------------- */
  function renderRunbooks(el) {
    el.innerHTML = '<div class="xai">' + head("AI 运维", "预案由已解决的工单沉淀，命中匹配条件时直接给出方案；低风险预案可开启自动处置") + tabs("runbooks") + '<div class="xai-card" data-rb-list><p class="xai-loading">正在读取…</p></div></div>';
    var box = el.querySelector("[data-rb-list]");
    api("GET", "/runbooks").then(function (r) {
      var arr = items(r);
      if (!arr.length) { box.innerHTML = empty("generic", "还没有预案", "在已解决的工单详情点「存为预案」即可沉淀"); return; }
      var manage = can("aiops:manage");
      box.innerHTML = '<div class="xai-table"><table><thead><tr><th>名称</th><th>匹配条件</th><th>风险</th><th>自动处置</th><th>使用 / 成功</th><th>更新时间</th>' + (manage ? "<th>操作</th>" : "") + "</tr></thead><tbody>" +
        arr.map(function (b) {
          var m = parseJSON(b.match, {}) || {};
          var mt = [m.alertname && "告警 " + m.alertname, m.analyzer && "规则 " + m.analyzer, m.env && "环境 " + envName(m.env)].filter(Boolean).join("；") || "—";
          return "<tr><td><b>" + E(b.name) + "</b></td><td>" + E(mt) + "</td><td>" + riskTag(b.risk) + "</td><td>" + (b.auto_execute ? tag("已开启", "x-green") : tag("关闭")) + "</td><td>" + E((b.uses || 0) + " / " + (b.successes || 0)) +
            "</td><td>" + E(fmtTime(b.updated_at)) + "</td>" + (manage ? '<td class="xai-ops"><button class="x-tlink" data-rb-edit="' + b.id + '">编辑</button><button class="x-tlink xai-danger" data-rb-del="' + b.id + '">删除</button></td>' : "") + "</tr>";
        }).join("") + "</tbody></table></div>";
      var byId = function (id) { return arr.filter(function (x) { return String(x.id) === String(id); })[0]; };
      on(box, "data-rb-edit", function (id) { openRunbookEdit(el, byId(id)); });
      on(box, "data-rb-del", function (id) {
        var b = byId(id);
        modal({
          title: "删除预案", ok: "删除", danger: true,
          body: "<p>删除后不再匹配新的工单，已有工单不受影响。请输入预案名称「<b>" + E(b.name) + "</b>」确认。</p><input class=\"x-input\" data-del-name autocomplete=\"off\">",
          onOk: function (m) {
            if (m.querySelector("[data-del-name]").value.trim() !== b.name) throw new Error("名称不一致");
            return api("DELETE", "/runbooks/" + encodeURIComponent(id)).then(function () { toast("已删除"); renderRunbooks(el); });
          }
        });
      });
    }, function (e) { box.innerHTML = '<p class="xai-err" role="alert">读取失败：' + E(errText(e)) + "</p>"; });
  }

  function openRunbookEdit(el, b) {
    var plan = b.plan || {};
    var d = drawer("编辑预案", '<form class="xai-form" data-rbe onsubmit="return false"><label><span class="x-req">预案名称</span><input class="x-input" name="name" maxlength="64" value="' + E(b.name) + '"></label>' +
      "<h4>匹配条件</h4>" + matchFields(parseJSON(b.match, {})) + "<h4>自动处置</h4>" + autoFields(b.risk, b.auto_execute, parseJSON(b.auto_scope, {})) +
      "<h4>方案步骤</h4>" + stepRows((plan.steps || []).map(function (s, i) { return { id: "d" + i, seq: i + 1, action: s.action, params: s.params, title: s.note, risk: "", phase: "main", status: "pending" }; }), { actions: cache.actions }) +
      '<p class="xai-merr" role="alert" hidden></p></form>',
      '<button class="x-btn" type="button" data-d-x>取消</button><button class="x-btn x-btn--pri" type="button" data-rbe-save>保存</button>');
    // 预案步骤表里风险/状态列无意义，表头保持统一即可
    var save = d.el.querySelector("[data-rbe-save]"), err = d.el.querySelector(".xai-merr");
    save.onclick = function () {
      var f = d.el.querySelector("[data-rbe]").elements;
      var body = { name: f.name.value.trim(), match: readMatch(f), auto_execute: canAutoExecute(b.risk) && f.auto_execute.checked, auto_scope: readScope(f), plan: b.plan };
      if (!body.name) { err.textContent = "请填写预案名称"; err.hidden = false; return; }
      save.disabled = true;
      api("PUT", "/runbooks/" + encodeURIComponent(b.id), body).then(function () { d.close(); toast("已保存"); renderRunbooks(el); },
        function (e) { save.disabled = false; err.textContent = errText(e); err.hidden = false; });
    };
  }

  /* ---------------- 策略设置 ---------------- */
  function renderPolicy(el) {
    el.innerHTML = '<div class="xai">' + head("AI 运维", "控制 AI 何时自动排查、何时允许自动处置") + tabs("policy") + '<div class="xai-card" data-pol><p class="xai-loading">正在读取…</p></div></div>';
    var box = el.querySelector("[data-pol]");
    var models = XS() ? XS().get("/ai/models/options").then(items, function () { return null; }) : Promise.resolve(null);
    Promise.all([api("GET", "/policy"), models]).then(function (r) {
      var p = normalizePolicy(r[0] && r[0].policy ? r[0].policy : r[0]), ms = r[1];
      var sev = ["critical", "warning", "info"], sevName = { critical: "严重", warning: "警告", info: "提示" };
      var chk = function (name, v, arr) { return '<label class="xai-check"><input type="checkbox" name="' + name + '" value="' + v + '"' + (arr.indexOf(v) >= 0 ? " checked" : "") + ">"; };
      box.innerHTML = '<form class="xai-form xai-pol" data-polf onsubmit="return false">' +
        '<section><h4>自动排查</h4><label class="xai-check"><input type="checkbox" name="ai_on"' + (p.auto_investigate.enabled ? " checked" : "") + "> 新告警触发时自动发起只读排查</label>" +
        '<div class="xai-inline"><span>告警级别</span>' + sev.map(function (s) { return chk("ai_sev", s, p.auto_investigate.severities) + " " + sevName[s] + "</label>"; }).join("") + "</div>" +
        '<div class="xai-inline"><span>环境</span>' + ENV.map(function (e) { return chk("ai_env", e.v, p.auto_investigate.envs) + " " + e.t + "</label>"; }).join("") + "</div></section>" +
        '<section><h4>自动处置</h4><label class="xai-check"><input type="checkbox" name="ae_on"' + (p.auto_execute.enabled ? " checked" : "") + "> 允许命中预案白名单的低风险方案自动执行</label>" +
        '<label class="xai-narrow"><span>每小时最多自动处置次数</span><input class="x-input" type="number" min="0" max="100" name="ae_max" value="' + p.auto_execute.max_per_hour + '"></label>' +
        '<p class="xai-hint">高风险方案始终需要人工审批，不受此开关影响。</p></section>' +
        '<section><h4>变更窗口</h4><label class="xai-check"><input type="checkbox" name="cw_on"' + (p.change_window.enabled ? " checked" : "") + "> 高风险操作只能在变更窗口内执行</label>" +
        '<label class="xai-narrow"><span>窗口</span><input class="x-input xai-mono" name="cw" value="' + E(p.change_window.cron_like) + '" placeholder="Mon-Fri 09:00-21:00"></label></section>' +
        (ms ? '<section><h4>模型</h4><label class="xai-narrow"><span>排查使用的模型</span><select class="x-input" name="model"><option value="">不使用模型（规则引擎）</option>' +
          ms.map(function (m) { return '<option value="' + E(m.id) + '"' + (String(m.id) === String(p.model_id) ? " selected" : "") + ">" + E(m.name || m.model || ("模型 " + m.id)) + "</option>"; }).join("") + "</select></label></section>" : "") +
        '<p class="xai-merr" role="alert" hidden></p><div class="xai-btns"><button class="x-btn x-btn--pri" type="button" data-pol-save>保存</button></div></form>';
      var f = box.querySelector("[data-polf]"), err = box.querySelector(".xai-merr");
      box.querySelector("[data-pol-save]").onclick = function (ev) {
        var vals = function (n) { return [].filter.call(f.querySelectorAll('[name="' + n + '"]'), function (x) { return x.checked; }).map(function (x) { return x.value; }); };
        var max = parseInt(f.ae_max.value, 10);
        if (isNaN(max) || max < 0 || max > 100) { err.textContent = "每小时上限需为 0–100"; err.hidden = false; return; }
        if (f.cw_on.checked && !/^\S+\s+\d{2}:\d{2}-\d{2}:\d{2}$/.test(f.cw.value.trim())) { err.textContent = "变更窗口格式如 Mon-Fri 09:00-21:00"; err.hidden = false; return; }
        var body = {
          auto_investigate: { enabled: f.ai_on.checked, severities: vals("ai_sev"), envs: vals("ai_env") },
          auto_execute: { enabled: f.ae_on.checked, max_per_hour: max },
          change_window: { enabled: f.cw_on.checked, cron_like: f.cw.value.trim() },
          model_id: f.model ? (f.model.value ? (isNaN(+f.model.value) ? f.model.value : +f.model.value) : null) : p.model_id
        };
        err.hidden = true; ev.target.disabled = true;
        api("PUT", "/policy", body).then(function () { ev.target.disabled = false; toast("已保存"); }, function (e) { ev.target.disabled = false; err.textContent = errText(e); err.hidden = false; });
      };
    }, function (e) { box.innerHTML = '<p class="xai-err" role="alert">读取失败：' + E(errText(e)) + "</p>"; });
  }

  /* ---------------- 主机纳管抽屉 ---------------- */
  function openOnboard() {
    var d = drawer("纳管主机", '<form class="xai-form" data-onb autocomplete="off" onsubmit="return false">' +
      '<p class="xai-hint">凭据只交给堡垒机加密托管，不会发送给模型，也不会出现在工单或聊天记录中。</p>' +
      '<div class="xai-row2"><label><span class="x-req">主机 IP</span><input class="x-input xai-mono" name="address" maxlength="253" placeholder="10.0.0.5"></label>' +
      '<label><span class="x-req">SSH 端口</span><input class="x-input" type="number" name="port" min="1" max="65535" value="22"></label></div>' +
      '<div class="xai-row2"><label><span>名称</span><input class="x-input" name="name" maxlength="64" placeholder="默认使用 IP"></label>' +
      '<label><span>环境</span><select class="x-input" name="env">' + ENV.map(function (e) { return '<option value="' + e.v + '"' + (e.v === "test" ? " selected" : "") + ">" + e.t + "</option>"; }).join("") + "</select></label></div>" +
      '<label><span class="x-req">登录账号</span><input class="x-input" name="username" maxlength="64" autocomplete="off" placeholder="root"></label>' +
      '<div class="xai-inline"><span>认证方式</span><label class="xai-check"><input type="radio" name="auth" value="password" checked> 密码</label><label class="xai-check"><input type="radio" name="auth" value="key"> 私钥</label></div>' +
      '<label data-auth-pw><span class="x-req">密码</span><input class="x-input" type="password" name="password" autocomplete="new-password"></label>' +
      '<label data-auth-key hidden><span class="x-req">私钥</span><textarea class="x-input xai-mono xai-secret" name="private_key" rows="5" autocomplete="off" spellcheck="false" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label>' +
      '<p class="xai-merr" role="alert" hidden></p></form>',
      '<button class="x-btn" type="button" data-d-x>取消</button><button class="x-btn x-btn--pri" type="button" data-onb-go>纳管</button>');
    var form = d.el.querySelector("[data-onb]"), err = d.el.querySelector(".xai-merr"), btn = d.el.querySelector("[data-onb-go]");
    [].forEach.call(form.querySelectorAll('[name="auth"]'), function (r) {
      r.onchange = function () {
        var key = form.querySelector('[name="auth"]:checked').value === "key";
        form.querySelector("[data-auth-pw]").hidden = key; form.querySelector("[data-auth-key]").hidden = !key;
        clearSecretFields(form);
      };
    });
    var origClose = d.close;
    d.close = function () { clearSecretFields(form); origClose(); };
    on(d.el, "data-d-x", d.close);
    btn.onclick = function () {
      var body = onboardPayload(form); // 读取后输入框已清空
      var msg = validateOnboard(body);
      if (msg) { body = null; err.textContent = msg; err.hidden = false; return; }
      err.hidden = true; btn.disabled = true;
      var req = api("POST", "/hosts/onboard", body);
      body = null;
      req.then(function (a) {
        d.close();
        toast("已纳管主机" + (a && (a.name || a.address) ? "「" + (a.name || a.address) + "」" : ""));
      }, function (e) { btn.disabled = false; err.textContent = "纳管失败：" + errText(e) + "（凭据已清空，请重新填写）"; err.hidden = false; });
    };
  }

  /* ---------------- 告警详情挂载 ---------------- */
  function mountAlertButton(modalEl, alert) {
    if (!modalEl || !alert || alert.id == null || !can("aiops:handle")) return null;
    if (modalEl.querySelector("[data-aiops-case]")) return modalEl.querySelector("[data-aiops-case]");
    var anchor = modalEl.querySelector("[data-alert-ai]");
    var host = anchor ? anchor.parentNode : (modalEl.querySelector(".x-mfoot, .x-dfoot") || null);
    if (!host) return null;
    var b = document.createElement("button");
    b.type = "button"; b.className = "x-btn"; b.setAttribute("data-aiops-case", ""); b.textContent = "AI 排查";
    b.onclick = function () {
      b.disabled = true; b.textContent = "正在发起…";
      api("POST", "/cases/from-alert/" + encodeURIComponent(alert.id)).then(function (c) {
        var id = c && (c.id || (c.case && c.case.id));
        toast("已发起 AI 排查");
        if (id) go("case/" + id);
        b.disabled = false; b.textContent = "AI 排查";
      }, function (e) { b.disabled = false; b.textContent = "AI 排查"; toast(errText(e)); });
    };
    host.appendChild(b);
    return b;
  }

  /* ---------------- 路由 ---------------- */
  function has(k) { return /^(cases|runbooks|policy|case\/\d+)$/.test(String(k || "")); }
  function render(k, el) {
    k = String(k || "cases");
    if (k.indexOf("case/") !== 0) stopPoll();
    if (k.indexOf("case/") === 0) return renderCase(k.slice(5), el);
    if (k === "runbooks") return renderRunbooks(el);
    if (k === "policy") return can("aiops:manage") ? renderPolicy(el) : renderCases(el);
    return renderCases(el);
  }

  var U = {
    has: has, render: render, mountAlertButton: mountAlertButton, openOnboard: openOnboard,
    // 测试用纯函数
    _t: {
      E: E, caseStatusName: caseStatusName, stepStatusName: stepStatusName, riskName: riskName, riskTag: riskTag, caseTag: caseTag, stepTag: stepTag,
      executorName: executorName, isActive: isActive, paramsText: paramsText, maxRisk: maxRisk, canAutoExecute: canAutoExecute, stepRows: stepRows,
      collectStepEdits: collectStepEdits, clearSecretFields: clearSecretFields, onboardPayload: onboardPayload, validateOnboard: validateOnboard,
      errText: errText, normalizePolicy: normalizePolicy, actionTitle: actionTitle, has: has
    }
  };
  if (typeof module !== "undefined" && module.exports) module.exports = U;
  root.XSAiopsUI = U;
})(typeof window !== "undefined" ? window : globalThis);
