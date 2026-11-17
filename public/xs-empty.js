/*
 * 星枢 Matrix 统一空状态与状态标签（window.XSEmpty）
 *
 * XSEmpty.html({ kind, title, desc, actions, size, attrs })
 *   kind    插画类型：pipeline / app / release / canary / requirement / bug / test / host / cluster /
 *           search（无匹配）/ error / generic，另有 credential / workspace / member / job / scan；未知类型按 generic。
 *   title   主文案（必填，自动转义）；desc 补充说明（可选，自动转义）。
 *   actions 「下一步」按钮：[{ text, act | href, primary, attrs }]
 *           act   → <button data-xse-act="act">，可用 XSEmpty.bind(容器, { act: fn }) 绑定；
 *           href  → <a href>（只接受站内 # 路由、相对路径或 http/https）；
 *           attrs → 额外的 data-* / aria-* / title 属性，供使用事件委托的模块（如 data-jobs-click）直接接入。
 *   actionsHTML 可选，模块已有按钮的 HTML（调用方负责转义），追加在 actions 之后。
 *   size    "s" 为紧凑版（用于详情页里的小区块）。
 *   attrs   加在空状态根节点上的 data-* 属性（便于测试或模块定位）。
 * XSEmpty.tag(state, text?) 统一四色状态标签：正常 绿 / 警告 橙 / 异常 红 / 未知(停用) 灰。
 * XSEmpty.state(state) 把各模块的状态值归一为 ok / warn / bad / unknown。
 *
 * 插画全部为手写内联 SVG 线描（灰色线条 + 主色点缀），不引入图片或第三方库。
 * 注意：空状态不带 data-xs-hint（该属性会被全局隐藏）。
 */
(function (root) {
  "use strict";

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  var GREY = "#c4c9d1", ACCENT = "#1677ff";
  // 主色点缀用的线条属性；CSS 里 .xse-a 会改用 var(--x-blue)
  var A = ' class="xse-a" stroke="' + ACCENT + '"';
  var FLOOR = '<path d="M26 86h68" stroke-dasharray="2 5"/>';

  var ART = {
    pipeline:
      '<circle cx="22" cy="44" r="9"/><circle cx="60" cy="44" r="9"' + A + '/><circle cx="98" cy="44" r="9"/>' +
      '<path d="M31 44h20M69 44h20"/><path d="M18 44l3 3 5-6"/>' +
      '<path d="M57.5 40v8l6.5-4z"' + A + '/><path d="M95 41h6M95 44h6M95 47h4"/>' +
      '<path d="M22 53v12h76V53" stroke-dasharray="3 4"/>' + FLOOR,
    app:
      '<rect x="30" y="16" width="60" height="54" rx="5"/><path d="M30 27h60"/>' +
      '<path d="M36 21.5h.01M41 21.5h.01M46 21.5h.01" stroke-width="2.4"/>' +
      '<rect x="38" y="34" width="18" height="13" rx="2"' + A + '/><rect x="64" y="34" width="18" height="13" rx="2"/>' +
      '<rect x="38" y="52" width="18" height="12" rx="2"/><path d="M73 52v12M67 58h12"' + A + '/>' + FLOOR,
    release:
      '<path d="M36 50l24-12 24 12-24 12z"/><path d="M36 50v18l24 12 24-12V50"/><path d="M60 62v18"/>' +
      '<path d="M60 32V8M52 16l8-8 8 8"' + A + '/>',
    canary:
      '<circle cx="16" cy="46" r="5"/><path d="M21 46h22"/>' +
      '<path d="M43 46c16 0 18-20 36-20h14"' + A + ' stroke-dasharray="4 3"/><path d="M43 46c16 0 18 20 36 20h14"/>' +
      '<circle cx="99" cy="26" r="6"' + A + '/><circle cx="99" cy="66" r="6"/>' +
      '<path d="M96 26h6"' + A + '/><path d="M96 66h6M99 63v6"/>' + FLOOR,
    requirement:
      '<path d="M36 10h36l14 14v56H36z"/><path d="M72 10v14h14"/><path d="M44 34h30M44 44h34M44 54h22"/>' +
      '<rect x="44" y="62" width="10" height="10" rx="2"' + A + '/><path d="M46.5 67l2 2 3.5-4"' + A + '/>',
    bug:
      '<ellipse cx="60" cy="52" rx="14" ry="18"/><circle cx="60" cy="28" r="7"/>' +
      '<path d="M46 44l-11-6M46 54H33M48 63l-10 8M74 44l11-6M74 54h13M72 63l10 8"/><path d="M56 22l-4-7M64 22l4-7"/>' +
      '<path d="M54 46h12M54 56h12"' + A + '/>' + FLOOR,
    test:
      '<path d="M46 10h28M51 10v24L35 70a6 6 0 0 0 5.5 9h39a6 6 0 0 0 5.5-9L69 34V10"/>' +
      '<path d="M41 58h38"' + A + '/><path d="M51 67l6 5 11-11"' + A + '/>',
    host:
      '<rect x="30" y="12" width="60" height="18" rx="3"/><rect x="30" y="35" width="60" height="18" rx="3"/><rect x="30" y="58" width="60" height="18" rx="3"/>' +
      '<circle cx="38" cy="21" r="2"' + A + '/><circle cx="38" cy="44" r="2"/><circle cx="38" cy="67" r="2"/>' +
      '<path d="M66 21h16M66 44h16M66 67h16"/>' + FLOOR,
    cluster:
      '<path d="M60 34l10.4 6v12L60 58l-10.4-6V40z"' + A + '/>' +
      '<circle cx="24" cy="20" r="8"/><circle cx="96" cy="20" r="8"/><circle cx="24" cy="72" r="8"/><circle cx="96" cy="72" r="8"/>' +
      '<path d="M50 40L31 25M70 40l19-15M50 52L31 67M70 52l19 15"/>',
    search:
      '<circle cx="52" cy="40" r="21"/><path d="M67 55l19 19" stroke-width="3"/>' +
      '<path d="M45 33l14 14M59 33L45 47"' + A + '/>' + FLOOR,
    error:
      '<path d="M60 12l36 62H24z"/><path d="M60 34v20"' + A + '/><path d="M60 63.5h.01"' + A + ' stroke-width="3.2"/>' + FLOOR,
    generic:
      '<path d="M28 52l10-26h44l10 26v22H28z"/><path d="M28 52h20l4 8h16l4-8h20"/>' +
      '<path d="M50 18l-4-8M60 16V6M70 18l4-8"' + A + '/>' + FLOOR,
    credential:
      '<circle cx="40" cy="44" r="15"/><circle cx="40" cy="44" r="4.5"' + A + '/><path d="M55 44h37M80 44v10M88 44v7"/>' + FLOOR,
    workspace:
      '<rect x="22" y="16" width="34" height="26" rx="3"/><rect x="64" y="16" width="34" height="26" rx="3"' + A + '/>' +
      '<rect x="22" y="50" width="34" height="26" rx="3"/><rect x="64" y="50" width="34" height="26" rx="3" stroke-dasharray="3 3"/>' +
      '<path d="M81 58v10M76 63h10"/>',
    member:
      '<circle cx="48" cy="32" r="11"/><path d="M28 74c0-13 9-22 20-22s20 9 20 22"/>' +
      '<circle cx="82" cy="36" r="8"' + A + '/><path d="M72 56c3-2 6-3 10-3 9 0 15 7 15 17"' + A + '/>' + FLOOR,
    job:
      '<circle cx="60" cy="44" r="27"/><path d="M60 28v16l11 7"' + A + '/><path d="M60 13v4M60 71v4M29 44h4M87 44h4"/>',
    scan:
      '<path d="M60 10l28 10v20c0 19-12 32-28 38C44 72 32 59 32 40V20z"/><path d="M47 43l9 9 17-17"' + A + '/>'
  };
  var KINDS = Object.keys(ART);

  function safeHref(h) {
    h = String(h == null ? "" : h).replace(/[\u0000-\u0020\u007f]+/g, "");
    if (!h || /^\/\//.test(h)) return "";
    if (/^https?:\/\//i.test(h)) return h;
    if (/^[a-z][\w+.-]*:/i.test(h)) return "";
    return h;
  }
  // 只透传 data-* / aria-* / title / role，值统一转义
  function attrsHTML(a) {
    if (!a || typeof a !== "object") return "";
    return Object.keys(a).filter(function (k) {
      return /^(data-[a-z0-9_-]+|aria-[a-z]+|title|role)$/i.test(k) && !/^data-xs-hint$/i.test(k) && a[k] != null && a[k] !== false;
    }).map(function (k) { return " " + k + '="' + esc(a[k] === true ? "" : a[k]) + '"'; }).join("");
  }

  function art(kind, size) {
    var k = ART[kind] ? kind : "generic";
    var w = size === "s" ? 80 : 120, h = size === "s" ? 60 : 90;
    return '<svg class="xse-art" data-xse-art="' + k + '" viewBox="0 0 120 90" width="' + w + '" height="' + h + '" aria-hidden="true" focusable="false" fill="none" stroke="' + GREY +
      '" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" xmlns="http://www.w3.org/2000/svg">' + ART[k] + "</svg>";
  }

  function actionHTML(a) {
    if (!a || !a.text) return "";
    var cls = "x-btn" + (a.primary ? " x-btn--pri" : "") + " xse-btn";
    var extra = attrsHTML(a.attrs);
    var href = a.href ? safeHref(a.href) : "";
    if (href) return '<a class="' + cls + '" href="' + esc(href) + '"' + extra + ">" + esc(a.text) + "</a>";
    return '<button type="button" class="' + cls + '"' + (a.act ? ' data-xse-act="' + esc(a.act) + '"' : "") + extra + (a.disabled ? " disabled" : "") + ">" + esc(a.text) + "</button>";
  }

  function html(o) {
    o = o || {};
    var kind = ART[o.kind] ? o.kind : "generic";
    // actionsHTML：调用方自己拼好的按钮 HTML（必须已转义，可信内容），用于复用模块已有的按钮组件
    var acts = (o.actions || []).map(actionHTML).join("") + (typeof o.actionsHTML === "string" ? o.actionsHTML : "");
    return '<div class="xse xse--' + kind + (o.size === "s" ? " xse--s" : "") + '" data-xse="' + kind + '" role="status"' + attrsHTML(o.attrs) + ">" +
      art(kind, o.size) +
      '<p class="xse-title">' + esc(o.title || "暂无数据") + "</p>" +
      (o.desc ? '<p class="xse-desc">' + esc(o.desc) + "</p>" : "") +
      (acts ? '<div class="xse-acts">' + acts + "</div>" : "") + "</div>";
  }

  // bind：把容器里 [data-xse-act] 按钮绑定到 handlers[act]
  function bind(box, handlers) {
    if (!box || !box.querySelectorAll || !handlers) return;
    [].forEach.call(box.querySelectorAll("[data-xse-act]"), function (b) {
      var fn = handlers[b.getAttribute("data-xse-act")];
      if (typeof fn === "function") b.onclick = function (ev) { if (ev && ev.stopPropagation) ev.stopPropagation(); return fn(b, ev); };
    });
  }

  /* ---------------------------------------------------------------- 四色状态标签 */
  var STATE_TEXT = { ok: "正常", warn: "警告", bad: "异常", unknown: "未知" };
  var STATE_COLOR = { ok: "#16a34a", warn: "#d97706", bad: "#dc2626", unknown: "#6b7280" };
  var ALIAS = {
    ok: ["ok", "normal", "healthy", "ready", "running", "active", "enabled", "succeeded", "success", "passed", "pass", "online", "available", "bound", "true", "正常", "健康", "运行中", "成功", "通过", "在线", "就绪"],
    warn: ["warn", "warning", "degraded", "pending", "paused", "progressing", "retrying", "partial", "notready", "警告", "降级", "等待", "暂停", "部分可用"],
    bad: ["bad", "error", "err", "danger", "failed", "failure", "fail", "critical", "dead", "unhealthy", "down", "offline", "crashloopbackoff", "false", "异常", "错误", "失败", "离线", "故障", "严重"],
    unknown: ["unknown", "disabled", "stopped", "cancelled", "canceled", "inactive", "none", "未知", "停用", "已停止", "已取消"]
  };
  var LOOKUP = {};
  Object.keys(ALIAS).forEach(function (k) { ALIAS[k].forEach(function (a) { LOOKUP[a] = k; }); });

  function state(s) {
    if (s === true) return "ok";
    if (s === false) return "bad";
    var k = String(s == null ? "" : s).trim().toLowerCase().replace(/[\s_-]+/g, "");
    return LOOKUP[k] || LOOKUP[String(s == null ? "" : s).trim()] || "unknown";
  }
  function tag(s, text) {
    var k = state(s);
    return '<span class="xse-tag xse-tag--' + k + '" data-xse-state="' + k + '"><i aria-hidden="true"></i>' + esc(text != null && text !== "" ? text : STATE_TEXT[k]) + "</span>";
  }

  (function ensureCss() {
    if (typeof document === "undefined" || !document || !document.head || !document.createElement) return;
    if (document.getElementById && document.getElementById("xs-empty-css")) return;
    if (document.querySelector && document.querySelector('link[href$="xs-empty.css"]')) return;
    var l = document.createElement("link");
    l.id = "xs-empty-css"; l.rel = "stylesheet"; l.href = "xs-empty.css";
    document.head.appendChild(l);
  })();

  root.XSEmpty = { html: html, tag: tag, state: state, bind: bind, art: art, kinds: KINDS.slice(), COLORS: STATE_COLOR, TEXT: STATE_TEXT };
})(typeof window !== "undefined" ? window : this);
