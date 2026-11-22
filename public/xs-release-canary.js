/* CI/CD 应用 → 金丝雀发布。读写真实接口（server/internal/modules/cicd/canary.go）：
 *   列表      GET  /cicd/applications/:id/canaries?env=        （cicd:read）
 *   发起      POST /cicd/applications/:id/canaries             （cicd:deploy；需要审批时 202）
 *             {env,image,service,ingress,steps[{weight,pause_sec,manual}],replicas,auto_promote,reason}
 *   详情      GET  /cicd/canaries/:id                          （cicd:read，含 events 时间线）
 *   操作      POST /cicd/canaries/:id/advance|promote|pause|resume|rollback  {reason?}（cicd:deploy）
 *             推进 / 全量需要审批时 202；暂停与回滚从不等待审批。
 *   对比      GET  /cicd/canaries/:id/compare                  （cicd:read；权重曲线、步骤进度、两侧定位，canary_compare.go）
 *   两侧监控  observability 已有接口，用当前用户身份读取（失败一律降级为一行提示）：
 *             流量：数据源解析 + 范围查询（ingress-nginx 请求数 / 5xx / 时延直方图，仅独立数据源）
 *             资源：工作负载迷你趋势（近 30 分钟 CPU / 内存，独立与中央数据源均可）
 *   演示模式（XS.mode 存在且不为 live）使用本文件内演示数据，不发请求、不执行操作。
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
      ".xrc .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}",
      ".xrc-split{display:flex;height:12px;border-radius:6px;overflow:hidden;background:#eef1f5;margin:4px 0 6px}.xrc-split i{display:block;height:100%}",
      ".xrc-split-s{background:#6b7280}.xrc-split-c{background:#2563eb}",
      ".xrc-split-legend{display:flex;gap:16px;font-size:13px;color:var(--x-sub,#4b5563);margin-bottom:12px}",
      ".xrc-dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;vertical-align:middle}",
      ".xrc-cmp-steps{display:flex;flex-wrap:wrap;align-items:center;gap:8px 14px;margin-bottom:12px;font-size:13px}",
      ".xrc-pips{list-style:none;margin:0;padding:0;display:flex;gap:4px}.xrc-pips li{min-width:36px;text-align:center;padding:1px 6px;border-radius:4px;font-size:12px;border:1px solid #e5e7eb;color:#6b7280;background:#fff}",
      ".xrc-pips li.is-done{border-color:#bbf7d0;color:#15803d;background:#f0fdf4}.xrc-pips li.is-current{border-color:#93c5fd;color:#1d4ed8;background:#eff6ff}.xrc-pips li.is-stopped{border-color:#fecaca;color:#b91c1c;background:#fef2f2}",
      ".xrc-spark{display:inline-block;vertical-align:middle}",
      ".xrc-mgrid{border:1px solid var(--x-line,#e6e9ed);border-radius:6px;overflow:hidden}",
      ".xrc-mrow{display:grid;grid-template-columns:120px minmax(0,1fr) minmax(0,1fr);gap:12px;align-items:center;padding:8px 12px;border-top:1px solid var(--x-line,#f0f2f5)}",
      ".xrc-mrow:first-child{border-top:0}.xrc-mhead{background:#fafbfc;font-size:13px;color:var(--x-sub,#4b5563)}",
      ".xrc-mname{color:var(--x-sub,#4b5563);font-size:13px}.xrc-mcell{display:flex;align-items:center;gap:10px}.xrc-mcell b{min-width:84px;font-weight:500}",
      ".xrc-verdict{margin:0 0 10px;padding:6px 10px;border-left:3px solid #6b7280;background:#f8fafc;font-size:13px}"
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


  /* ------------------------------------------------------------ 主版本 vs 金丝雀 对比 */

  var CMP_METRICS_MS = 60000;           // 两侧监控最短刷新间隔（详情轮询 5 秒，监控不必跟着刷）
  var CMP_MAX_SPAN = 24 * 3600;         // 流量曲线最长回看 24 小时
  var C_STABLE = "#6b7280", C_CANARY = "#2563eb";
  var C_OK = "#16a34a", C_WARN = "#d97706", C_BAD = "#dc2626";

  function isDemo() { var m = X().mode; return m != null && m !== "live"; }
  function num(v) { var n = Number(v); return isFinite(n) ? n : NaN; }
  function tsOf(v) { var t = new Date(v).getTime(); return isNaN(t) ? 0 : Math.floor(t / 1000); }
  function promQuote(v) { return '"' + String(v == null ? "" : v).replace(/\\/g, "\\\\").replace(/"/g, '\\"') + '"'; }

  // 后端对比接口不可用时，由详情本身推出权重与步骤（没有监控定位信息）。
  function compareFromDetail(c, nowSec) {
    var steps = stepsOf(c), final = !!FINAL[c.status], w = Math.max(0, Math.min(100, Number(c.weight) || 0));
    var start = tsOf(c.created_at) || nowSec, end = final && c.finished_at ? (tsOf(c.finished_at) || nowSec) : nowSec;
    if (end < start) end = start;
    var series = [[start, 0]];
    (c.events || []).forEach(function (e) {
      if (e.kind !== "step") return;
      var m = /（权重\s*(\d+)%）/.exec(String(e.message || ""));
      var at = Math.max(start, tsOf(e.ts) || start);
      if (m) series.push([at, Math.min(100, +m[1])]);
      else if (/已回滚/.test(String(e.message || ""))) series.push([at, 0]);
      else if (/已全量/.test(String(e.message || ""))) series.push([at, 100]);
    });
    var cw = c.status === "succeeded" ? 100 : w;
    series.push([end, cw]);
    var done = 0;
    var list = steps.map(function (s, i) {
      var state = c.status === "succeeded" || i < c.current_step ? "done" : i === c.current_step ? (final ? "stopped" : "current") : "pending";
      if (state === "done") done++;
      return { index: i, weight: s.weight, pause_sec: s.pause_sec, manual: !!s.manual, state: state };
    });
    return { canary_id: c.id, status: c.status, final: final, weight: cw, current_step: c.current_step, total_steps: steps.length, done_steps: done,
      start: start, end: end, weight_series: series, steps: list,
      stable: { workload: c.deployment || "", ingress: c.ingress || "", image: c.stable_image || "", weight: c.status === "succeeded" ? 0 : 100 - w },
      canary: { workload: c.canary_deployment || "", ingress: c.canary_ingress || "", image: c.image || "", weight: cw },
      metrics_target: null, note: "" };
  }

  // Prometheus 范围查询结果（{resultType, result:[{values:[[ts,"v"]]}]}，可能再包一层 data）→ [[ts, v]]，多条序列按时间求和。
  function promPoints(env) {
    var d = env && env.data && env.data.result ? env.data : env;
    var res = d && Array.isArray(d.result) ? d.result : [];
    var acc = {};
    res.forEach(function (s) {
      (s.values || []).forEach(function (p) {
        var t = Math.round(num(p[0])), v = num(p[1]);
        if (!isFinite(t) || !isFinite(v)) return;
        acc[t] = (acc[t] || 0) + v;
      });
    });
    return Object.keys(acc).map(Number).sort(function (a, b) { return a - b; }).map(function (t) { return [t, acc[t]]; });
  }
  function ratioPoints(part, total, scale) {
    var map = {};
    part.forEach(function (p) { map[p[0]] = p[1]; });
    return total.filter(function (p) { return p[1] > 0; }).map(function (p) { return [p[0], ((map[p[0]] || 0) / p[1]) * (scale || 1)]; });
  }
  function clipPoints(pts, start) { return (pts || []).filter(function (p) { return p && num(p[0]) >= start - 60 && isFinite(num(p[1])); }).map(function (p) { return [num(p[0]), num(p[1])]; }); }
  function lastVal(pts) { return pts && pts.length ? pts[pts.length - 1][1] : NaN; }
  function avgVal(pts) {
    if (!pts || !pts.length) return NaN;
    return pts.reduce(function (a, p) { return a + p[1]; }, 0) / pts.length;
  }

  // 流量指标 PromQL（ingress-nginx 按 Ingress 对象打标签，金丝雀路由是独立的 Ingress）。
  function trafficQueries(ns, ingress, rateWin) {
    var sel = "namespace=" + promQuote(ns) + ",ingress=" + promQuote(ingress);
    var w = "[" + rateWin + "s]";
    return {
      rps: "sum(rate(nginx_ingress_controller_requests{" + sel + "}" + w + "))",
      err: "sum(rate(nginx_ingress_controller_requests{" + sel + ',status=~"5.."}' + w + "))",
      p95: "histogram_quantile(0.95, sum by (le) (rate(nginx_ingress_controller_request_duration_seconds_bucket{" + sel + "}" + w + ")))"
    };
  }
  function trafficWindow(cmp, nowSec) {
    var end = cmp.final ? cmp.end : nowSec;
    var start = Math.max(cmp.start, end - CMP_MAX_SPAN);
    if (end - start < 300) start = end - 300;
    var step = Math.max(15, Math.ceil((end - start) / 60));
    return { start: start, end: end, step: step, rateWin: Math.max(120, step) };
  }

  var METRIC_DEF = {
    rps: { label: "请求速率", fmt: function (v) { return v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2); }, unit: " 次/秒" },
    err: { label: "错误率（5xx）", fmt: function (v) { return v.toFixed(v >= 10 ? 1 : 2); }, unit: "%" },
    p95: { label: "P95 时延", fmt: function (v) { return v >= 100 ? v.toFixed(0) : v.toFixed(1); }, unit: " 毫秒" },
    cpu: { label: "CPU", fmt: function (v) { return v >= 10 ? v.toFixed(1) : v.toFixed(3); }, unit: " 核" },
    mem: { label: "内存", fmt: function (v) { v = v / 1048576; return v >= 100 ? v.toFixed(0) : v.toFixed(1); }, unit: " MiB" }
  };
  function fmtMetric(key, v) { var d = METRIC_DEF[key]; return isFinite(v) ? d.fmt(v) + d.unit : "—"; }

  // 迷你折线（两侧共用 x 区间与 y 上限，便于横向比较）。stepLine=true 画阶梯线（权重）。
  function sparkSVG(pts, opt) {
    opt = opt || {};
    var w = opt.w || 160, h = opt.h || 32, x0 = opt.x0, x1 = opt.x1, ymax = opt.ymax;
    if (!pts || pts.length < 1 || !(x1 > x0)) return '<svg class="xrc-spark" width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + " " + h + '" aria-hidden="true"></svg>';
    if (!(ymax > 0)) ymax = 1;
    var pad = 2;
    function X_(t) { return pad + ((Math.min(Math.max(t, x0), x1) - x0) / (x1 - x0)) * (w - pad * 2); }
    function Y_(v) { return h - pad - (Math.max(0, Math.min(v, ymax)) / ymax) * (h - pad * 2); }
    var d = "";
    pts.forEach(function (p, i) {
      var x = X_(p[0]).toFixed(1), y = Y_(p[1]).toFixed(1);
      if (i === 0) d += "M" + x + " " + y;
      else if (opt.stepLine) d += "H" + x + "V" + y;
      else d += "L" + x + " " + y;
    });
    if (pts.length === 1) d += "H" + X_(x1).toFixed(1);
    return '<svg class="xrc-spark" width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + " " + h + '" role="img" aria-label="' + esc(opt.label || "趋势") + '">' +
      '<line x1="0" y1="' + (h - pad) + '" x2="' + w + '" y2="' + (h - pad) + '" stroke="#e5e7eb" stroke-width="1"/>' +
      '<path d="' + d + '" fill="none" stroke="' + (opt.color || C_CANARY) + '" stroke-width="1.5" stroke-linejoin="round"/></svg>';
  }

  // 结论：只在两侧都有流量数据时给出参考判断（四色状态）。
  function verdict(m) {
    if (!m || m.kind !== "traffic") return null;
    var se = avgVal(m.stable.err), ce = avgVal(m.canary.err), sp = avgVal(m.stable.p95), cp = avgVal(m.canary.p95);
    if (!isFinite(ce) || !isFinite(se)) return { color: "#6b7280", text: "两侧流量不足，暂时无法比较错误率" };
    if (ce - se >= 1 || (se > 0 && ce >= se * 2 && ce >= 0.5)) return { color: C_BAD, text: "金丝雀错误率明显高于主版本，建议回滚并排查" };
    if (isFinite(cp) && isFinite(sp) && sp > 0 && cp >= sp * 1.5 && cp - sp >= 50) return { color: C_WARN, text: "金丝雀 P95 时延明显高于主版本，建议观察后再推进" };
    return { color: C_OK, text: "金丝雀错误率与时延和主版本相近，可按计划推进" };
  }

  function splitBarHTML(cmp) {
    var cw = Math.max(0, Math.min(100, Number(cmp.canary.weight) || 0)), sw = 100 - cw;
    return '<div class="xrc-split" role="img" aria-label="主版本 ' + sw + "%，金丝雀 " + cw + '%">' +
      (sw > 0 ? '<i class="xrc-split-s" style="width:' + sw + '%"></i>' : "") + (cw > 0 ? '<i class="xrc-split-c" style="width:' + cw + '%"></i>' : "") + "</div>" +
      '<div class="xrc-split-legend"><span><b class="xrc-dot" style="background:' + C_STABLE + '"></b>主版本 ' + sw + '%</span><span><b class="xrc-dot" style="background:' + C_CANARY + '"></b>金丝雀 ' + cw + "%</span></div>";
  }

  function compareHTML(cmp, metrics) {
    if (!cmp) return "";
    var span = { x0: cmp.start, x1: Math.max(cmp.end, cmp.start + 1) };
    var stepsLine = '<div class="xrc-cmp-steps"><span>放量进度：已完成 ' + esc(cmp.done_steps) + " / " + esc(cmp.total_steps) + " 步</span>" +
      '<ol class="xrc-pips">' + (cmp.steps || []).map(function (s) {
        return '<li class="is-' + esc(s.state) + '" title="第 ' + (s.index + 1) + " 步 " + esc(s.weight) + '%">' + esc(s.weight) + "%</li>";
      }).join("") + "</ol>" +
      '<span class="xrc-sub">流量权重变化</span>' + sparkSVG(cmp.weight_series, { x0: span.x0, x1: span.x1, ymax: 100, stepLine: true, color: C_CANARY, w: 200, label: "金丝雀流量权重变化" }) + "</div>";
    var body = "";
    var m = metrics;
    if (m && (m.kind === "traffic" || m.kind === "resource")) {
      var keys = m.kind === "traffic" ? ["rps", "err", "p95"] : ["cpu", "mem"];
      var mx = { x0: m.x0 || span.x0, x1: m.x1 || span.x1 };
      var rows = keys.map(function (k) {
        var s = m.stable[k] || [], c = m.canary[k] || [];
        var ymax = Math.max.apply(null, s.concat(c).map(function (p) { return p[1]; }).concat([0])) * 1.1;
        if (k === "err") ymax = Math.max(ymax, 1);
        var cv = lastVal(c), sv = lastVal(s), color = "";
        if (k === "err" && isFinite(cv) && isFinite(sv)) color = cv - sv >= 1 ? C_BAD : cv - sv >= 0.3 ? C_WARN : C_OK;
        return '<div class="xrc-mrow" data-xrc-metric="' + k + '"><span class="xrc-mname">' + esc(METRIC_DEF[k].label) + "</span>" +
          '<span class="xrc-mcell"><b>' + esc(fmtMetric(k, sv)) + "</b>" + sparkSVG(s, { x0: mx.x0, x1: mx.x1, ymax: ymax, color: C_STABLE, label: "主版本" + METRIC_DEF[k].label }) + "</span>" +
          '<span class="xrc-mcell"><b' + (color ? ' style="color:' + color + '"' : "") + ">" + esc(fmtMetric(k, cv)) + "</b>" +
          sparkSVG(c, { x0: mx.x0, x1: mx.x1, ymax: ymax, color: C_CANARY, label: "金丝雀" + METRIC_DEF[k].label }) + "</span></div>";
      }).join("");
      var v = verdict(m);
      body = (v ? '<p class="xrc-verdict" data-xrc-verdict style="border-left-color:' + v.color + '">' + esc(v.text) + '<span class="xrc-sub">（仅供参考）</span></p>' : "") +
        '<div class="xrc-mgrid"><div class="xrc-mrow xrc-mhead"><span></span><span><b class="xrc-dot" style="background:' + C_STABLE + '"></b>主版本' +
        (cmp.stable.workload ? '<span class="xrc-sub">　' + esc(cmp.stable.workload) + "</span>" : "") + '</span><span><b class="xrc-dot" style="background:' + C_CANARY + '"></b>金丝雀' +
        (cmp.canary.workload ? '<span class="xrc-sub">　' + esc(cmp.canary.workload) + "</span>" : "") + "</span></div>" + rows + "</div>" +
        '<p class="xrc-sub">' + esc(m.caption || "") + "</p>";
    } else if (m && m.kind === "loading") {
      body = '<p class="xrc-sub" data-xrc-cmp-note>正在读取两侧监控数据…</p>';
    } else {
      body = '<p class="xrc-sub" data-xrc-cmp-note>' + esc((m && m.note) || cmp.note || "暂无两侧监控数据，仅显示流量权重与放量进度") + "</p>";
    }
    return '<div class="xrc-card" data-xrc-compare><div class="xrc-head"><h3>主版本 vs 金丝雀</h3></div>' + splitBarHTML(cmp) + stepsLine + body + "</div>";
  }

  /* ---------- 演示数据（确定性，便于截图与测试） */

  function demoWave(start, end, step, base, amp, seed) {
    var out = [];
    for (var t = start, i = 0; t <= end; t += step, i++) out.push([t, Math.max(0, base + amp * Math.sin((i + seed) / 3) + amp * 0.3 * Math.cos((i * 7 + seed) / 5))]);
    return out;
  }
  function demoMetrics(cmp) {
    var end = cmp.end, start = Math.max(cmp.start, end - 3600), step = Math.max(30, Math.ceil((end - start) / 40));
    var cw = Math.max(1, cmp.canary.weight) / 100, sw = Math.max(0, 1 - cw);
    var weightAt = function (t) { var w = 0; (cmp.weight_series || []).forEach(function (p) { if (p[0] <= t) w = p[1]; }); return w / 100; };
    var srps = demoWave(start, end, step, 120 * Math.max(sw, 0.05), 8, 1), crps = demoWave(start, end, step, 120, 6, 4).map(function (p) { return [p[0], p[1] * weightAt(p[0])]; });
    return { kind: "traffic", x0: start, x1: end,
      stable: { rps: srps, err: demoWave(start, end, step, 0.35, 0.1, 2), p95: demoWave(start, end, step, 182, 14, 3) },
      canary: { rps: crps, err: demoWave(start, end, step, 0.42, 0.12, 5), p95: demoWave(start, end, step, 176, 16, 6) },
      caption: "演示数据：请求速率、错误率与 P95 时延来自入口网关指标" };
  }
  function demoCanaries() {
    var now = Date.now(), iso = function (ms) { return new Date(ms).toISOString(); };
    var steps = DEFAULT_STEPS.map(function (s) { return { weight: s.weight, pause_sec: s.pause_sec, manual: s.manual }; });
    var base = { app_id: 0, namespace: "shop", deployment: "web", canary_deployment: "web-canary", service: "web", canary_service: "web-canary",
      ingress: "web", canary_ingress: "web-canary", plan: { steps: steps, replicas: 1, auto_promote: false }, pending_action: "", created_by: "演示用户" };
    var a = Object.assign({}, base, { id: 902, env: "prod", image: "harbor.example.com/shop/web:v2.4.0", stable_image: "harbor.example.com/shop/web:v2.3.1",
      status: "progressing", current_step: 1, weight: 30, created_at: iso(now - 40 * 60000), next_at: iso(now + 3 * 60000), message: "第 2 步（权重 30%）观察中",
      reason: "订单页性能优化", events: [
        { id: 1, kind: "operator", message: "发起金丝雀发布", actor: "演示用户", ts: iso(now - 40 * 60000) },
        { id: 2, kind: "step", message: "第 1 步（权重 10%）观察中", actor: "演示用户", ts: iso(now - 38 * 60000) },
        { id: 3, kind: "step", message: "第 2 步（权重 30%）观察中", actor: "系统", ts: iso(now - 33 * 60000) }] });
    var b = Object.assign({}, base, { id: 901, env: "prod", image: "harbor.example.com/shop/web:v2.3.1", stable_image: "harbor.example.com/shop/web:v2.3.0",
      status: "succeeded", current_step: 3, weight: 100, created_at: iso(now - 26 * 3600000), finished_at: iso(now - 24 * 3600000), message: "已全量发布", events: [] });
    return [a, b];
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
      confirm: null, busy: false, form: null, formErr: null, services: [],
      cmp: null, cmpMetrics: null, cmpFor: null, cmpMetricsAt: 0, cmpMetricsFinal: false, cmpBusy: false
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
      if (isDemo()) { st.app = { envs: [] }; return Promise.resolve(st.app); }
      return api("/cicd/applications/" + encodeURIComponent(appId)).then(function (a) { st.app = a || {}; return st.app; }, function () { st.app = {}; return st.app; });
    }
    function loadList() {
      if (isDemo()) { st.list = demoCanaries(); return Promise.resolve(); }
      return api("/cicd/applications/" + encodeURIComponent(appId) + "/canaries", env ? { query: { env: env } } : {}).then(function (r) { st.list = items(r); });
    }
    function loadDetail() {
      var id = st.detailId;
      if (isDemo()) {
        st.detail = demoCanaries().filter(function (c) { return String(c.id) === String(id); })[0] || null;
        if (!st.detail) return Promise.reject({ status: 404 });
        return loadCompare(id);
      }
      return api("/cicd/canaries/" + encodeURIComponent(id)).then(function (c) { st.detail = c; return loadCompare(id); });
    }
    // 对比数据：后端接口失败时由详情推出权重与步骤；从不让详情加载失败。
    function loadCompare(id) {
      var nowSec = Math.floor(Date.now() / 1000);
      var fallback = function () { return st.detail ? compareFromDetail(st.detail, nowSec) : null; };
      var p = isDemo() ? Promise.resolve(fallback()) :
        api("/cicd/canaries/" + encodeURIComponent(id) + "/compare").then(function (r) { return r && r.steps ? r : fallback(); }, fallback);
      return p.then(function (cmp) {
        if (String(st.detailId) !== String(id)) return;
        if (st.cmpFor !== id) { st.cmpMetrics = null; st.cmpMetricsAt = 0; st.cmpMetricsFinal = false; }
        st.cmp = cmp; st.cmpFor = id;
        loadMetrics(id);
      });
    }
    function loadMetrics(id) {
      var cmp = st.cmp;
      if (!cmp) return;
      if (isDemo()) { st.cmpMetrics = demoMetrics(cmp); return; }
      var mt = cmp.metrics_target;
      if (!mt) { st.cmpMetrics = { kind: "none", note: cmp.note || "" }; return; }
      var now = Date.now(), have = st.cmpMetrics && st.cmpMetrics.kind !== "loading";
      if (st.cmpBusy || (have && (st.cmpMetricsFinal || now - st.cmpMetricsAt < CMP_METRICS_MS))) return;
      st.cmpBusy = true; st.cmpMetricsAt = now;
      if (!st.cmpMetrics) st.cmpMetrics = { kind: "loading" };
      var nowSec = Math.floor(now / 1000);
      fetchTraffic(cmp, mt, nowSec).then(function (r) { return r || fetchResource(cmp, mt); }).then(function (r) { return r; }, function () { return null; }).then(function (r) {
        st.cmpBusy = false;
        if (!alive() || String(st.detailId) !== String(id)) return;
        st.cmpMetrics = r || { kind: "none", note: "未读取到两侧监控数据（集群未接入监控或无查看权限），仅显示流量权重与放量进度" };
        st.cmpMetricsFinal = !!cmp.final;
        if (!st.confirm && !st.busy) paint(); // 确认框打开时不重绘，避免清掉已输入的原因
      });
    }
    function fetchTraffic(cmp, mt, nowSec) {
      if (!can("datasource:read") || !can("metrics:read") || !mt.stable_ingress || !mt.canary_ingress) return Promise.resolve(null);
      return api("/observability/datasources/resolve", { query: { type: "prometheus", cluster_id: mt.cluster_id } }).then(function (r) {
        var ds = r && r.datasource;
        if (!ds || !ds.id || r.scope === "central") return null; // 中央数据源不支持分组聚合，改用资源曲线
        var win = trafficWindow(cmp, nowSec);
        function q(query) {
          return api("/observability/prometheus/" + encodeURIComponent(ds.id) + "/query_range", { query: { cluster_id: mt.cluster_id, query: query,
            start: String(win.start), end: String(win.end), step: win.step + "s" } }).then(promPoints, function () { return null; });
        }
        var qs = trafficQueries(mt.namespace, mt.stable_ingress, win.rateWin), qc = trafficQueries(mt.namespace, mt.canary_ingress, win.rateWin);
        return Promise.all([q(qs.rps), q(qs.err), q(qs.p95), q(qc.rps), q(qc.err), q(qc.p95)]).then(function (res) {
          var srps = res[0], crps = res[3];
          if (!srps || !crps || !srps.concat(crps).some(function (p) { return p[1] > 0; })) return null;
          var ms = function (pts) { return (pts || []).map(function (p) { return [p[0], p[1] * 1000]; }); };
          return { kind: "traffic", x0: win.start, x1: win.end,
            stable: { rps: srps, err: ratioPoints(res[1] || [], srps, 100), p95: ms(res[2]) },
            canary: { rps: crps, err: ratioPoints(res[4] || [], crps, 100), p95: ms(res[5]) },
            caption: "请求速率、错误率与 P95 时延来自入口网关指标" + (win.start > cmp.start ? "（近 24 小时）" : "（本次金丝雀全程）") };
        });
      }, function () { return null; });
    }
    function fetchResource(cmp, mt) {
      if (!can("workload:read")) return Promise.resolve(null);
      return api("/observability/metrics/sparklines", { query: { cluster_id: mt.cluster_id, kind: "workload", workload_kind: mt.workload_kind || "deployments",
        namespace: mt.namespace, names: mt.stable_workload + "," + mt.canary_workload } }).then(function (r) {
        if (!r || r.source !== "prometheus") return null;
        var find = function (n) { return (r.items || []).filter(function (i) { return i.name === n; })[0] || {}; };
        var a = find(mt.stable_workload), b = find(mt.canary_workload);
        var out = { kind: "resource", x0: Math.max(num(r.start) || cmp.start, cmp.start), x1: num(r.end) || cmp.end,
          stable: { cpu: clipPoints(a.cpu, cmp.start), mem: clipPoints(a.mem, cmp.start) },
          canary: { cpu: clipPoints(b.cpu, cmp.start), mem: clipPoints(b.mem, cmp.start) },
          caption: "未读取到入口流量指标，显示两侧近 30 分钟 CPU / 内存用量" };
        if (!(out.x1 > out.x0)) out.x0 = out.x1 - 1800;
        return [out.stable.cpu, out.stable.mem, out.canary.cpu, out.canary.mem].some(function (x) { return x.length; }) ? out : null;
      }, function () { return null; });
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
        (st.cmp && String(st.cmp.canary_id) === String(c.id) ? compareHTML(st.cmp, st.cmpMetrics) : "") +
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
      if (isDemo()) { st.confirm = null; toast("演示模式不会执行实际操作"); paint(); return Promise.resolve(); }
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
      open: function (t) { st.view = "detail"; st.detailId = t.getAttribute("data-id"); st.detail = null; st.cmp = null; st.cmpMetrics = null; st.cmpFor = null; st.confirm = null; st.error = ""; return refresh(); },
      back: function () { st.view = "list"; st.detail = null; st.cmp = null; st.cmpMetrics = null; st.cmpFor = null; st.confirm = null; st.error = ""; return refresh(); },
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
        if (isDemo()) { toast("演示模式不会执行实际操作"); return closeCreate(); }
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

  root.XSReleaseCanary = { render: render, openCreate: openCreate, validateSteps: validateSteps, validateForm: validateForm, STATUS: STATUS,
    _compareFromDetail: compareFromDetail, _compareHTML: compareHTML, _promPoints: promPoints, _trafficQueries: trafficQueries, _verdict: verdict, _sparkSVG: sparkSVG, _demoMetrics: demoMetrics };
})(window);
