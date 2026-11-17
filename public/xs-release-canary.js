/* CI/CD 应用 → 金丝雀发布。读写真实接口（server/internal/modules/cicd/canary.go）：
 *   列表      GET  /cicd/applications/:id/canaries?env=        （cicd:read）
 *   发起      POST /cicd/applications/:id/canaries             （cicd:deploy；需要审批时 202）
 *             {env,image,service,ingress,steps[{weight,pause_sec,manual}],replicas,auto_promote,reason}
 *   详情      GET  /cicd/canaries/:id                          （cicd:read，含 events 时间线）
 *   操作      POST /cicd/canaries/:id/advance|promote|pause|resume|rollback  {reason?}（cicd:deploy）
 *             推进 / 全量需要审批时 202；暂停与回滚从不等待审批。
 * Helm 部署的环境后端不支持（409），这里直接隐藏「发起」并说明原因。
 * 挂载：window.XSReleaseCanary.render(el, {appId, env})；window.XSReleaseCanary.openCreate(appId, env)
 */
(function (root) {
  "use strict";
  // emptyHTML：统一空状态（XSEmpty）；未加载 xs-empty.js 时回退为原来的一行文字 fallbackHTML。
  function emptyHTML(o, fallbackHTML) {
    var X = root.XSEmpty;
    return typeof X !== "undefined" && X && typeof X.html === "function" ? X.html(o) : fallbackHTML;
  }
  var POLL_MS = 5000;
  var DEFAULT_STEPS = [{ weight: 10, pause_sec: 300, manual: false }, { weight: 30, pause_sec: 300, manual: false },
    { weight: 50, pause_sec: 0, manual: true }, { weight: 100, pause_sec: 0, manual: true }];
  var MAX_STEPS = 20;
  var STATUS = {
    pending: ["待启动", "run"], awaiting_approval: ["等待审批", "warn"], starting: ["启动中", "run"],
    progressing: ["观察中", "run"], awaiting_confirm: ["等待确认", "warn"], paused: ["已暂停", "warn"],
    promoting: ["全量发布中", "run"], rolling_back: ["回滚中", "warn"], succeeded: ["已全量", "ok"],
    rolled_back: ["已回滚", "mute"], failed: ["失败", "bad"], rejected: ["审批未通过", "bad"], cancelled: ["已取消", "mute"]
  };
  var FINAL = { succeeded: 1, rolled_back: 1, failed: 1, rejected: 1, cancelled: 1 };
  // 与后端 canaryAllowed 一致
  var ALLOWED = {
    advance: ["progressing", "awaiting_confirm", "paused"],
    promote: ["progressing", "awaiting_confirm", "paused"],
    pause: ["progressing", "awaiting_confirm"],
    resume: ["paused"],
    rollback: ["pending", "awaiting_approval", "progressing", "awaiting_confirm", "paused", "rolling_back"]
  };
  var ACTION_NAME = { advance: "推进下一步", promote: "全量发布", pause: "暂停", resume: "继续", rollback: "回滚", start: "发起金丝雀" };
  var EVENT_KIND = { info: "信息", step: "步骤", operator: "操作", error: "错误" };

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function api(path, options) { return X().api(path, options || {}); }
  function can(p) { var x = X(); return !!(x.can && x.can(p)); }
  function toast(msg) { var x = X(); if (x.toast) x.toast(msg); else if (typeof root.toast === "function") root.toast(msg); }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function hasCJK(s) { return /[\u4e00-\u9fff]/.test(String(s || "")); }
  function isApproval(e) {
    var x = X();
    return !!(e && (e.status === 202 || e.code === 40304 || e.approval || (x.isApproval && x.isApproval(e))));
  }
  function approvalText(e) {
    var d = (e && (e.approval || e.data)) || {};
    var t = d.ticket || {};
    return "已提交审批" + (t.id ? "（审批单 #" + t.id + "）" : "") + "，审批通过后自动执行";
  }
  // 后端少数错误为英文，按状态码给中文说明。
  function errMsg(e) {
    var m = (e && e.message) || "";
    var s = e && e.status;
    if (s === 403) return hasCJK(m) ? m : "没有该环境的发布权限";
    if (s === 404) return hasCJK(m) ? m : "金丝雀发布不存在或已被删除";
    if (hasCJK(m)) return m;
    if (s === 400) return "填写内容不合法，请检查后重试";
    if (s === 409) return "当前状态不允许该操作";
    return "服务暂不可用，请稍后重试";
  }
  function fmtTime(v) {
    if (!v) return "—";
    var d = new Date(v);
    if (isNaN(d.getTime()) || d.getFullYear() < 2000) return "—";
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }
  function fmtSec(n) {
    n = Number(n) || 0;
    if (n <= 0) return "不等待";
    if (n < 60) return n + " 秒";
    if (n % 3600 === 0) return n / 3600 + " 小时";
    if (n % 60 === 0) return n / 60 + " 分钟";
    return Math.floor(n / 60) + " 分 " + (n % 60) + " 秒";
  }
  function items(r) {
    if (Array.isArray(r)) return r;
    if (r && Array.isArray(r.items)) return r.items;
    return [];
  }

  /* ------------------------------------------------------------ 校验 */

  function intOf(v) {
    var s = String(v == null ? "" : v).trim();
    return /^\d+$/.test(s) ? parseInt(s, 10) : NaN;
  }
  // 返回 {steps, errors:{index:msg}, error}
  function validateSteps(raw) {
    var out = [], errors = {}, prev = 0, first = "";
    raw = raw || [];
    if (!raw.length) first = "至少需要一个步骤";
    if (raw.length > MAX_STEPS) first = "步骤最多 " + MAX_STEPS + " 步";
    raw.forEach(function (s, i) {
      var w = intOf(s.weight), p = intOf(s.pause_sec === "" || s.pause_sec == null ? 0 : s.pause_sec), msg = "";
      if (isNaN(w) || w < 1 || w > 100) msg = "第 " + (i + 1) + " 步权重须为 1–100 的整数";
      else if (w <= prev) msg = "第 " + (i + 1) + " 步权重须大于上一步（" + prev + "%）";
      else if (isNaN(p) || p < 0 || p > 86400) msg = "第 " + (i + 1) + " 步观察时长须为 0–86400 秒";
      if (msg) { errors[i] = msg; if (!first) first = msg; }
      if (!isNaN(w)) prev = Math.max(prev, w);
      out.push({ weight: w, pause_sec: isNaN(p) ? 0 : p, manual: !!s.manual });
    });
    return { steps: out, errors: errors, error: first };
  }
  var DNS1035 = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
  var DNS1123 = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
  // 返回 {body, errors:{field:msg}, error}
  function validateForm(f) {
    var errors = {}, image = String(f.image || "").trim(), service = String(f.service || "").trim(), ingress = String(f.ingress || "").trim();
    if (!f.env) errors.env = "请选择环境";
    if (!image) errors.image = "请填写镜像地址";
    else if (/\s/.test(image) || image.length > 512) errors.image = "镜像地址不能包含空格，且不超过 512 个字符";
    if (!service) errors.service = "请填写服务名称";
    else if (!DNS1035.test(service) || service.length > 56) errors.service = "小写字母开头，只含小写字母、数字、中划线，不超过 56 个字符";
    if (!ingress) errors.ingress = "请填写路由名称";
    else if (!DNS1123.test(ingress) || ingress.length > 246) errors.ingress = "只含小写字母、数字、中划线和点，不超过 246 个字符";
    var r = intOf(f.replicas === "" || f.replicas == null ? 1 : f.replicas);
    if (isNaN(r) || r < 1 || r > 50) errors.replicas = "副本数须为 1–50 的整数";
    if (String(f.reason || "").length > 2000) errors.reason = "原因不超过 2000 个字符";
    var sv = validateSteps(f.steps);
    var first = "";
    ["env", "image", "service", "ingress", "replicas", "reason"].forEach(function (k) { if (!first && errors[k]) first = errors[k]; });
    if (!first) first = sv.error;
    return {
      errors: errors, stepErrors: sv.errors, error: first,
      body: { env: f.env, image: image, service: service, ingress: ingress, steps: sv.steps, replicas: isNaN(r) ? 1 : r, auto_promote: !!f.auto_promote, reason: String(f.reason || "").trim() }
    };
  }

  /* ------------------------------------------------------------ 样式 */

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-rcn-css")) return;
    var s = doc.createElement("style"); s.id = "xs-rcn-css";
    s.textContent = [
      ".xrc{font-size:14px;color:var(--x-text,#1f2937)}",
      ".xrc-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:8px;padding:20px 24px;margin-bottom:16px}",
      ".xrc-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px}.xrc-head h3{margin:0;font-size:16px}",
      ".xrc-acts{display:flex;flex-wrap:wrap;gap:8px}",
      ".xrc-table{width:100%;border-collapse:collapse}.xrc-table th,.xrc-table td{padding:10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:middle}",
      ".xrc-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xrc-mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;word-break:break-all}",
      ".xrc-sub{color:var(--x-sub,#6b7280);font-size:13px}",
      ".xrc-tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid}",
      ".xrc-tag--ok{color:#15803d;background:#f0fdf4;border-color:#bbf7d0}.xrc-tag--bad{color:#b91c1c;background:#fef2f2;border-color:#fecaca}",
      ".xrc-tag--run{color:#1d4ed8;background:#eff6ff;border-color:#bfdbfe}.xrc-tag--warn{color:#b45309;background:#fffbeb;border-color:#fde68a}",
      ".xrc-tag--mute{color:#4b5563;background:#f3f4f6;border-color:#e5e7eb}",
      ".xrc-bar{height:10px;background:#eef1f5;border-radius:5px;overflow:hidden;margin:8px 0 4px}.xrc-bar i{display:block;height:100%;background:#3b82f6}",
      ".xrc-kv{display:grid;grid-template-columns:120px minmax(0,1fr);gap:8px 16px;margin:0 0 12px}.xrc-kv dt{color:var(--x-sub,#6b7280)}.xrc-kv dd{margin:0}",
      ".xrc-steps{list-style:none;margin:0;padding:0;display:flex;flex-wrap:wrap;gap:8px}",
      ".xrc-steps li{border:1px solid var(--x-line,#e6e9ed);border-radius:6px;padding:6px 10px;font-size:13px;background:#fafbfc}",
      ".xrc-steps li.is-done{border-color:#bbf7d0;background:#f0fdf4}.xrc-steps li.is-cur{border-color:#93c5fd;background:#eff6ff;font-weight:500}",
      ".xrc-tl{list-style:none;margin:0;padding:0}.xrc-tl li{padding:8px 0;border-bottom:1px solid var(--x-line,#f0f2f5);display:grid;grid-template-columns:160px 56px minmax(0,1fr);gap:10px}",
      ".xrc-tl .is-error{color:#b91c1c}",
      ".xrc-msg{margin:8px 0;padding:8px 12px;border-radius:6px;background:#f8fafc;border:1px solid var(--x-line,#e6e9ed);font-size:13px}",
      ".xrc-alert{margin:8px 0;padding:8px 12px;border-radius:6px;background:#fef2f2;border:1px solid #fecaca;color:#991b1b;font-size:13px}",
      ".xrc-confirm{margin:12px 0;padding:12px 14px;border-radius:6px;background:#fffbeb;border:1px solid #fde68a}",
      ".xrc-confirm--danger{background:#fef2f2;border-color:#fecaca}",
      ".xrc-form .xrc-row{display:grid;grid-template-columns:120px minmax(0,1fr);gap:12px;align-items:start;margin-bottom:12px}",
      ".xrc-form label{color:var(--x-sub,#4b5563);padding-top:7px}.xrc-form .x-input{width:100%}",
      ".xrc-ferr{color:#b91c1c;font-size:12px;margin-top:4px}",
      ".xrc-step-row{display:grid;grid-template-columns:56px 120px 150px 120px auto;gap:8px;align-items:center;margin-bottom:6px}",
      ".xrc-link{border:0;background:none;padding:0 8px 0 0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:14px}",
      ".xrc-empty{padding:24px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xrc-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:flex-start;justify-content:center;z-index:1000;overflow:auto;padding:48px 16px}",
      ".xrc-modal{background:#fff;border-radius:8px;width:820px;max-width:100%}",
      ".xrc .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  /* ------------------------------------------------------------ 片段 */

  function statusTag(s) {
    var v = STATUS[s] || [s ? "未知状态" : "—", "mute"];
    return '<span class="xrc-tag xrc-tag--' + v[1] + '">' + esc(v[0]) + "</span>";
  }
  function stepsOf(c) { return (c && c.plan && Array.isArray(c.plan.steps)) ? c.plan.steps : []; }
  function stepText(c) {
    var steps = stepsOf(c);
    if (!steps.length) return "—";
    if (c.current_step < 0) return "未开始（共 " + steps.length + " 步）";
    return "第 " + Math.min(c.current_step + 1, steps.length) + " / " + steps.length + " 步";
  }
  function bar(w) {
    w = Math.max(0, Math.min(100, Number(w) || 0));
    return '<div class="xrc-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + w + '"><i style="width:' + w + '%"></i></div>';
  }
  function canDo(c, action) {
    if (!c || ALLOWED[action].indexOf(c.status) < 0) return false;
    if (c.pending_action && action !== "rollback" && action !== "pause") return false;
    if (action === "advance" && c.current_step + 1 >= stepsOf(c).length) return false;
    return true;
  }

  /* ------------------------------------------------------------ 主体 */

  var mounted = []; // {el, st, show}

  function render(el, opts) { return mount(el, opts || {}, "list"); }

  function mount(el, opts, view, onClose) {
    if (!el) return null;
    css();
    var serial = (el._xsRcnSerial || 0) + 1; el._xsRcnSerial = serial;
    var appId = opts.appId, env = opts.env || "";
    var st = {
      view: view || "list", list: [], loading: true, error: "", app: null, detail: null, detailId: null,
      confirm: null, busy: false, form: null, formErr: null, services: []
    };
    var pollTimer = null;
    var canDeploy = can("cicd:deploy");
    function alive() { return el._xsRcnSerial === serial && el.isConnected !== false; }
    function envCfg() {
      var envs = (st.app && st.app.envs) || [];
      for (var i = 0; i < envs.length; i++) if (envs[i].name === env) return envs[i];
      return null;
    }
    function helmEnv() { var e = envCfg(); return !!(e && e.helm); }

    function schedule() {
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
      if (st.view === "create") return;
      var active = st.view === "detail" ? (st.detail && !FINAL[st.detail.status]) : st.list.some(function (c) { return !FINAL[c.status]; });
      if (!active) return;
      pollTimer = setTimeout(function () { pollTimer = null; if (alive() && !st.busy && !st.confirm) refresh(true); else if (alive()) schedule(); }, POLL_MS);
    }

    function loadApp() {
      if (st.app) return Promise.resolve(st.app);
      return api("/cicd/applications/" + encodeURIComponent(appId)).then(function (a) { st.app = a || {}; return st.app; }, function () { st.app = {}; return st.app; });
    }
    function loadList() {
      return api("/cicd/applications/" + encodeURIComponent(appId) + "/canaries", env ? { query: { env: env } } : {}).then(function (r) { st.list = items(r); });
    }
    function loadDetail() {
      return api("/cicd/canaries/" + encodeURIComponent(st.detailId)).then(function (c) { st.detail = c; });
    }
    function refresh(quiet) {
      if (!quiet) { st.loading = true; paint(); }
      var p = st.view === "detail" ? loadDetail() : st.view === "list" ? loadList() : Promise.resolve();
      return Promise.all([loadApp(), p]).then(function () {
        if (!alive()) return;
        st.loading = false; st.error = ""; paint(); schedule();
      }, function (e) {
        if (!alive()) return;
        st.loading = false; st.error = (st.view === "detail" ? "读取金丝雀详情失败：" : "读取金丝雀列表失败：") + errMsg(e); paint(); schedule();
      });
    }

    /* ---------- 视图 */

    function listHTML() {
      var head = '<div class="xrc-head"><h3>金丝雀发布' + (env ? '<span class="xrc-sub">　' + esc(env) + "</span>" : "") + '</h3><div class="xrc-acts">' +
        '<button class="x-btn" type="button" data-xrc-click="reload">刷新</button>' +
        (canDeploy && env && !helmEnv() ? '<button class="x-btn x-btn--pri" type="button" data-xrc-click="create">发起金丝雀</button>' : "") + "</div></div>";
      var note = helmEnv() ? '<p class="xrc-msg">该环境使用 Helm 部署，暂不支持金丝雀发布。</p>' :
        '<p class="xrc-sub">新版本先承接一小部分流量，按步骤逐步放量；任一步可暂停或回滚，确认无误后全量替换主版本。</p>';
      if (st.loading && !st.list.length) return head + note + '<p class="xrc-empty">正在加载…</p>';
      var err = st.error ? '<p class="xrc-alert" role="alert">' + esc(st.error) + "</p>" : "";
      if (!st.list.length) {
        var canCreate = canDeploy && env && !helmEnv();
        return head + note + err + emptyHTML({ kind: "canary", title: "还没有金丝雀发布记录",
          desc: helmEnv() ? "" : canCreate ? "发起后新版本先承接少量流量，确认无误再逐步放量到全量。" : env ? "需要该环境的发布权限才能发起金丝雀发布。" : "请先选择一个环境。",
          actions: canCreate ? [{ text: "发起金丝雀", act: "create", primary: true, attrs: { "data-xrc-click": "create" } }] : [] }, '<p class="xrc-empty">还没有金丝雀发布记录。</p>');
      }
      var rows = st.list.map(function (c) {
        return "<tr><td>#" + esc(c.id) + "</td><td>" + esc(c.env) + '</td><td class="xrc-mono">' + esc(c.image) + "</td><td>" + statusTag(c.status) +
          "</td><td>" + esc(Number(c.weight) || 0) + "%</td><td>" + esc(stepText(c)) + "</td><td>" + esc(c.created_by || "—") + "</td><td>" + esc(fmtTime(c.created_at)) +
          '</td><td><button class="xrc-link" type="button" data-xrc-click="open" data-id="' + esc(c.id) + '">详情</button></td></tr>';
      }).join("");
      return head + note + err + '<table class="xrc-table"><thead><tr><th>编号</th><th>环境</th><th>新镜像</th><th>状态</th><th>当前流量</th><th>进度</th><th>发起人</th><th>发起时间</th><th>操作</th></tr></thead><tbody>' +
        rows + "</tbody></table>";
    }

    function detailHTML() {
      var c = st.detail;
      var head = '<div class="xrc-head"><h3><button class="xrc-link" type="button" data-xrc-click="back">← 返回列表</button>金丝雀 #' + esc(st.detailId) + "</h3>" +
        '<div class="xrc-acts"><button class="x-btn" type="button" data-xrc-click="reload">刷新</button></div></div>';
      var err = st.error ? '<p class="xrc-alert" role="alert">' + esc(st.error) + "</p>" : "";
      if (!c) return head + err + (st.loading ? '<p class="xrc-empty">正在加载…</p>' : "");
      var steps = stepsOf(c), cur = c.current_step;
      var stepList = '<ol class="xrc-steps">' + steps.map(function (s, i) {
        var cls = FINAL[c.status] && c.status === "succeeded" ? "is-done" : i < cur ? "is-done" : i === cur ? "is-cur" : "";
        return '<li class="' + cls + '">第 ' + (i + 1) + " 步　" + esc(s.weight) + "%　" + (s.manual ? "人工确认" : "观察 " + esc(fmtSec(s.pause_sec))) + "</li>";
      }).join("") + "</ol>";
      var next = c.status === "progressing" && c.next_at ? "　预计 " + esc(fmtTime(c.next_at)) + " 自动进入下一阶段" : "";
      var pend = c.pending_action ? '<p class="xrc-msg">等待审批：' + esc(ACTION_NAME[c.pending_action] || "操作") + (c.approval_id ? "（审批单 #" + esc(c.approval_id) + "）" : "") + "</p>" : "";
      var acts = "";
      if (canDeploy && !FINAL[c.status]) {
        acts = '<div class="xrc-acts">' +
          (canDo(c, "advance") ? '<button class="x-btn x-btn--pri" type="button" data-xrc-click="act" data-act="advance">推进到 ' + esc(steps[cur + 1].weight) + "%</button>" : "") +
          (canDo(c, "pause") ? '<button class="x-btn" type="button" data-xrc-click="act" data-act="pause">暂停</button>' : "") +
          (canDo(c, "resume") ? '<button class="x-btn" type="button" data-xrc-click="act" data-act="resume">继续</button>' : "") +
          (canDo(c, "promote") ? '<button class="x-btn" type="button" data-xrc-click="ask" data-act="promote">全量发布</button>' : "") +
          (canDo(c, "rollback") ? '<button class="x-btn x-btn--danger" type="button" data-xrc-click="ask" data-act="rollback">' +
            (c.status === "pending" || c.status === "awaiting_approval" ? "取消" : "回滚") + "</button>" : "") + "</div>";
      }
      var confirm = "";
      if (st.confirm) {
        var promote = st.confirm.action === "promote";
        var cancelOnly = !promote && (c.status === "pending" || c.status === "awaiting_approval");
        confirm = '<div class="xrc-confirm' + (promote ? "" : " xrc-confirm--danger") + '" role="alertdialog">' +
          "<p><b>" + (promote ? "确认全量发布？" : cancelOnly ? "确认取消本次金丝雀？" : "确认回滚本次金丝雀？") + "</b></p><p class=\"xrc-sub\">" +
          (promote ? "主版本将切换到新镜像 " + esc(c.image) + "，完成后清理金丝雀实例与路由。生产环境可能需要审批。"
            : cancelOnly ? "尚未创建任何实例，取消后本次发布结束。" : "金丝雀流量立即归零，并删除金丝雀实例、服务与路由；主版本不受影响。") +
          '</p><div class="xrc-row"><input class="x-input" type="text" data-xrc-reason maxlength="2000" placeholder="原因（选填）" /></div>' +
          (st.confirm.error ? '<p class="xrc-ferr">' + esc(st.confirm.error) + "</p>" : "") +
          '<div class="xrc-acts"><button class="x-btn' + (promote ? " x-btn--pri" : " x-btn--danger") + '" type="button" data-xrc-click="confirm"' + (st.busy ? " disabled" : "") + ">" +
          (promote ? "确认全量" : cancelOnly ? "确认取消" : "确认回滚") + '</button><button class="x-btn" type="button" data-xrc-click="dismiss">再想想</button></div></div>';
      }
      var events = (c.events || []).map(function (e) {
        return '<li><span class="xrc-sub">' + esc(fmtTime(e.ts)) + '</span><span class="' + (e.kind === "error" ? "is-error" : "") + '">' + esc(EVENT_KIND[e.kind] || "信息") +
          '</span><span class="' + (e.kind === "error" ? "is-error" : "") + '">' + esc(e.message) + (e.actor ? '<span class="xrc-sub">　' + esc(e.actor) + "</span>" : "") + "</span></li>";
      }).join("");
      return head + err +
        '<div class="xrc-card"><dl class="xrc-kv"><dt>状态</dt><dd>' + statusTag(c.status) + "　" + esc(stepText(c)) + "</dd>" +
        "<dt>当前流量</dt><dd>新版本 " + esc(Number(c.weight) || 0) + "%" + next + bar(c.weight) + "</dd>" +
        "<dt>环境</dt><dd>" + esc(c.env) + "　" + esc(c.namespace || "") + "</dd>" +
        '<dt>新镜像</dt><dd class="xrc-mono">' + esc(c.image) + "</dd>" +
        '<dt>主版本镜像</dt><dd class="xrc-mono">' + esc(c.stable_image || "—") + "</dd>" +
        "<dt>服务 / 路由</dt><dd>" + esc(c.service) + " / " + esc(c.ingress) + "</dd>" +
        "<dt>金丝雀副本</dt><dd>" + esc((c.plan && c.plan.replicas) || 1) + "　" + (c.plan && c.plan.auto_promote ? "最后一步完成后自动全量" : "最后一步完成后等待确认全量") + "</dd>" +
        (c.reason ? "<dt>原因</dt><dd>" + esc(c.reason) + "</dd>" : "") +
        "<dt>发起</dt><dd>" + esc(c.created_by || "—") + "　" + esc(fmtTime(c.created_at)) + (c.finished_at ? "　结束于 " + esc(fmtTime(c.finished_at)) : "") + "</dd></dl>" +
        (c.message ? '<p class="xrc-msg">' + esc(c.message) + "</p>" : "") + pend + stepList + "</div>" +
        (acts || confirm ? '<div class="xrc-card">' + acts + confirm + "</div>" : "") +
        '<div class="xrc-card"><div class="xrc-head"><h3>时间线</h3></div>' + (events ? '<ul class="xrc-tl">' + events + "</ul>" : '<p class="xrc-empty">暂无记录</p>') + "</div>";
    }

    function formHTML() {
      var f = st.form, fe = st.formErr || { errors: {}, stepErrors: {} };
      function ferr(k) { return fe.errors && fe.errors[k] ? '<div class="xrc-ferr" data-xrc-err="' + k + '">' + esc(fe.errors[k]) + "</div>" : ""; }
      var envs = ((st.app && st.app.envs) || []).filter(function (e) { return !e.helm; });
      var envField = envs.length ? '<select class="x-select" data-xrc-f="env">' + envs.map(function (e) {
        return '<option value="' + esc(e.name) + '"' + (e.name === f.env ? " selected" : "") + ">" + esc(e.name) + "</option>";
      }).join("") + "</select>" : '<input class="x-input" type="text" data-xrc-f="env" value="' + esc(f.env) + '" />';
      var dl = st.services.length ? '<datalist id="xrc-svc-' + serial + '">' + st.services.map(function (n) { return '<option value="' + esc(n) + '"></option>'; }).join("") + "</datalist>" : "";
      var steps = f.steps.map(function (s, i) {
        var se = fe.stepErrors && fe.stepErrors[i];
        return '<div class="xrc-step-row"><span>第 ' + (i + 1) + ' 步</span>' +
          '<input class="x-input" type="text" inputmode="numeric" data-xrc-sw="' + i + '" value="' + esc(s.weight) + '" aria-label="第 ' + (i + 1) + ' 步流量百分比" placeholder="流量 %" />' +
          '<input class="x-input" type="text" inputmode="numeric" data-xrc-sp="' + i + '" value="' + esc(s.pause_sec) + '" aria-label="第 ' + (i + 1) + ' 步观察秒数" placeholder="观察秒数"' + (s.manual ? " disabled" : "") + " />" +
          '<label><input type="checkbox" data-xrc-sm="' + i + '"' + (s.manual ? " checked" : "") + " /> 人工确认</label>" +
          (f.steps.length > 1 ? '<button class="xrc-link" type="button" data-xrc-click="delStep" data-i="' + i + '">删除</button>' : "<span></span>") + "</div>" +
          (se ? '<div class="xrc-ferr" data-xrc-serr="' + i + '">' + esc(se) + "</div>" : "");
      }).join("");
      return '<div class="xrc-head"><h3>发起金丝雀发布</h3><div class="xrc-acts"><button class="x-btn" type="button" data-xrc-click="cancelCreate">取消</button></div></div>' +
        '<div class="xrc-form">' +
        '<div class="xrc-row"><label>环境</label><div>' + envField + ferr("env") + "</div></div>" +
        '<div class="xrc-row"><label>新镜像</label><div><input class="x-input xrc-mono" type="text" data-xrc-f="image" value="' + esc(f.image) + '" placeholder="' +
          esc(((st.app && st.app.image_repo) || "harbor.example.com/team/web") + ":v1.2.0") + '" />' + ferr("image") + "</div></div>" +
        '<div class="xrc-row"><label>服务</label><div><input class="x-input" type="text" data-xrc-f="service" value="' + esc(f.service) + '"' + (dl ? ' list="xrc-svc-' + serial + '"' : "") +
          ' placeholder="web" />' + dl + '<div class="xrc-sub">主版本对外提供访问的服务名称，平台会另建一个只指向新版本实例的服务。</div>' + ferr("service") + "</div></div>" +
        '<div class="xrc-row"><label>路由</label><div><input class="x-input" type="text" data-xrc-f="ingress" value="' + esc(f.ingress) + '" placeholder="web" />' +
          '<div class="xrc-sub">承接外部访问的主路由名称，按步骤把其中一部分流量分给新版本。</div>' + ferr("ingress") + "</div></div>" +
        '<div class="xrc-row"><label>放量步骤</label><div>' + steps +
          (f.steps.length < MAX_STEPS ? '<button class="xrc-link" type="button" data-xrc-click="addStep">+ 添加步骤</button>' : "") +
          '<button class="xrc-link" type="button" data-xrc-click="resetSteps">恢复默认</button>' +
          '<div class="xrc-sub">流量百分比须逐步严格递增（1–100）。勾选「人工确认」的步骤生效后会停下来等待操作，否则观察满设定秒数后自动进入下一步。</div></div></div>' +
        '<div class="xrc-row"><label>金丝雀副本</label><div><input class="x-input" type="text" inputmode="numeric" data-xrc-f="replicas" value="' + esc(f.replicas) + '" />' + ferr("replicas") + "</div></div>" +
        '<div class="xrc-row"><label>自动全量</label><div><label><input type="checkbox" data-xrc-f="auto_promote"' + (f.auto_promote ? " checked" : "") + " /> 最后一步观察结束后自动全量（最后一步为人工确认时仍需确认）</label></div></div>" +
        '<div class="xrc-row"><label>原因</label><div><textarea class="x-input" rows="2" data-xrc-f="reason" maxlength="2000" placeholder="版本说明、变更单号">' + esc(f.reason) + "</textarea>" + ferr("reason") + "</div></div>" +
        (fe.error ? '<p class="xrc-alert" role="alert">' + esc(fe.error) + "</p>" : "") +
        (envCfg() && /prod|生产/i.test(f.env) ? '<p class="xrc-sub">生产环境可能需要审批，提交后会显示「已提交审批」。</p>' : "") +
        '<div class="xrc-acts"><button class="x-btn x-btn--pri" type="button" data-xrc-click="submit"' + (st.busy ? " disabled" : "") + ">" + (st.busy ? "提交中…" : "发起") + "</button>" +
        '<button class="x-btn" type="button" data-xrc-click="cancelCreate">取消</button></div></div>';
    }

    function paint() {
      if (!alive()) return;
      var body = st.view === "create" ? formHTML() : st.view === "detail" ? detailHTML() : listHTML();
      el.innerHTML = '<div class="xrc" data-xrc-view="' + st.view + '">' + body + "</div>";
    }

    /* ---------- 表单读写 */

    function q(sel) { return el.querySelector(sel); }
    function readForm() {
      var f = st.form; if (!f) return;
      ["env", "image", "service", "ingress", "replicas", "reason"].forEach(function (k) {
        var n = q('[data-xrc-f="' + k + '"]'); if (n) f[k] = n.value;
      });
      var ap = q('[data-xrc-f="auto_promote"]'); if (ap) f.auto_promote = !!ap.checked;
      f.steps.forEach(function (s, i) {
        var w = q('[data-xrc-sw="' + i + '"]'), p = q('[data-xrc-sp="' + i + '"]'), m = q('[data-xrc-sm="' + i + '"]');
        if (w) s.weight = w.value; if (p) s.pause_sec = p.value; if (m) s.manual = !!m.checked;
      });
    }
    function startCreate() {
      st.view = "create"; st.formErr = null; st.busy = false;
      st.form = { env: env || "", image: "", service: "", ingress: "", replicas: "1", auto_promote: false, reason: "",
        steps: DEFAULT_STEPS.map(function (s) { return { weight: String(s.weight), pause_sec: String(s.pause_sec), manual: s.manual }; }) };
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
      return loadApp().then(function () {
        if (!alive()) return;
        var envs = ((st.app && st.app.envs) || []).filter(function (e) { return !e.helm; });
        if (envs.length && !envs.some(function (e) { return e.name === st.form.env; })) st.form.env = envs[0].name;
        paint();
        if (!st.form.env) return;
        return api("/cicd/applications/" + encodeURIComponent(appId) + "/inventory", { query: { env: st.form.env } }).then(function (inv) {
          st.services = ((inv && inv.services) || []).map(function (s) { return s.name; }).filter(Boolean);
          if (!alive() || st.view !== "create" || !st.form) return;
          readForm();
          if (st.services.length === 1 && !st.form.service) st.form.service = st.services[0];
          paint();
        }, function () { /* 只是提示用，读取失败不影响填写 */ });
      });
    }
    function closeCreate() {
      if (onClose) { onClose(); return; }
      st.view = "list"; st.form = null; return refresh();
    }

    /* ---------- 操作 */

    function doAction(action, reason) {
      var c = st.detail; if (!c) return Promise.resolve();
      st.busy = true; paint();
      return api("/cicd/canaries/" + encodeURIComponent(c.id) + "/" + action, { method: "POST", body: reason ? { reason: reason } : {} }).then(function (out) {
        if (!alive()) return;
        st.busy = false; st.confirm = null;
        toast((ACTION_NAME[action] || "操作") + "已提交");
        if (out && out.id) st.detail = Object.assign({}, out, { events: st.detail && st.detail.events });
        return refresh(true);
      }, function (e) {
        if (!alive()) return;
        st.busy = false;
        if (isApproval(e)) { st.confirm = null; toast(approvalText(e)); return refresh(true); }
        var msg = (ACTION_NAME[action] || "操作") + "失败：" + errMsg(e);
        if (st.confirm) st.confirm.error = msg; else st.error = msg;
        toast(msg); paint();
      });
    }

    var actions = {
      reload: function () { st.error = ""; return refresh(); },
      create: function () { return startCreate(); },
      cancelCreate: function () { return closeCreate(); },
      open: function (t) { st.view = "detail"; st.detailId = t.getAttribute("data-id"); st.detail = null; st.confirm = null; st.error = ""; return refresh(); },
      back: function () { st.view = "list"; st.detail = null; st.confirm = null; st.error = ""; return refresh(); },
      act: function (t) { st.error = ""; return doAction(t.getAttribute("data-act"), ""); },
      ask: function (t) { st.confirm = { action: t.getAttribute("data-act"), error: "" }; paint(); },
      dismiss: function () { st.confirm = null; paint(); },
      confirm: function () {
        if (!st.confirm || st.busy) return;
        var r = q("[data-xrc-reason]");
        return doAction(st.confirm.action, r ? String(r.value || "").trim() : "");
      },
      addStep: function () {
        readForm();
        var last = st.form.steps[st.form.steps.length - 1];
        var w = last ? Math.min(100, (intOf(last.weight) || 0) + 10) : 10;
        st.form.steps.push({ weight: String(w), pause_sec: "300", manual: false });
        paint();
      },
      delStep: function (t) { readForm(); st.form.steps.splice(+t.getAttribute("data-i"), 1); paint(); },
      resetSteps: function () { readForm(); st.form.steps = DEFAULT_STEPS.map(function (s) { return { weight: String(s.weight), pause_sec: String(s.pause_sec), manual: s.manual }; }); st.formErr = null; paint(); },
      submit: function () {
        if (st.busy) return;
        readForm();
        var v = validateForm(st.form);
        st.formErr = v;
        if (v.error) { paint(); return; }
        st.busy = true; paint();
        return api("/cicd/applications/" + encodeURIComponent(appId) + "/canaries", { method: "POST", body: v.body }).then(function (out) {
          if (!alive()) return;
          st.busy = false; toast("金丝雀发布已发起");
          if (onClose) { onClose(); return; }
          env = v.body.env;
          if (out && out.id) { st.view = "detail"; st.detailId = out.id; st.detail = out; } else st.view = "list";
          st.form = null; return refresh(true);
        }, function (e) {
          if (!alive()) return;
          st.busy = false;
          if (isApproval(e)) {
            toast(approvalText(e));
            if (onClose) { onClose(); return; }
            env = v.body.env; st.view = "list"; st.form = null; return refresh();
          }
          st.formErr = { errors: {}, stepErrors: {}, error: "发起失败：" + errMsg(e) }; paint();
        });
      }
    };

    if (el._xsRcnClick) el.removeEventListener("click", el._xsRcnClick);
    el._xsRcnClick = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-xrc-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-xrc-click")];
      if (fn) return fn(t);
    };
    el.addEventListener("click", el._xsRcnClick);
    if (el._xsRcnChange) el.removeEventListener("change", el._xsRcnChange);
    el._xsRcnChange = function (ev) {
      if (!alive() || st.view !== "create") return;
      var t = ev.target;
      if (t && t.getAttribute && t.getAttribute("data-xrc-sm") != null) { readForm(); paint(); }
    };
    el.addEventListener("change", el._xsRcnChange);

    var inst = { el: el, appId: appId, startCreate: startCreate, setEnv: function (e) { env = e || ""; } };
    mounted = mounted.filter(function (m) { return m.el !== el && m.el.isConnected !== false; });
    if (!onClose) mounted.push(inst);
    if (st.view === "create") startCreate(); else refresh();
    return inst;
  }

  // 应用详情页里已挂载时直接切到发起表单；否则以弹窗打开。
  function openCreate(appId, env) {
    for (var i = mounted.length - 1; i >= 0; i--) {
      var m = mounted[i];
      if (String(m.appId) === String(appId) && m.el.isConnected !== false) { if (env) m.setEnv(env); return m.startCreate(); }
    }
    var doc = root.document;
    if (!doc || !doc.body) return null;
    css();
    var mask = doc.createElement("div"); mask.className = "xrc-mask";
    var box = doc.createElement("div"); box.className = "xrc-modal xrc-card";
    mask.appendChild(box); doc.body.appendChild(mask);
    function close() { box._xsRcnSerial = -1; mask.remove(); }
    return mount(box, { appId: appId, env: env }, "create", close);
  }

  root.XSReleaseCanary = { render: render, openCreate: openCreate, validateSteps: validateSteps, validateForm: validateForm, STATUS: STATUS };
})(window);
