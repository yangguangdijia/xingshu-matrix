/*
 * 星枢 Matrix 研发中心 · 交付价值流（window.XSDevTrace）
 *
 * XSDevTrace.render(el, { type: "requirement" | "bug", id })
 *   在 el 内绘制需求 / 缺陷的交付进度：代码 → 构建 → 制品 → 发布 → 环境，每段四色状态、时间、可展开的明细，
 *   各环境当前版本，手动关联（需 devcenter:manage，移除需二次确认），以及「在提交信息或分支名中写 REQ-12 即可自动关联」提示。
 * XSDevTrace.releaseItems(el, releaseId)   绘制一次发布包含的需求、缺陷、提交与发布说明（可复制）。
 * XSDevTrace.openRelease(releaseId, opts)  在右侧抽屉中打开 releaseItems。
 * XSDevTrace.keyOf(type, id) → "REQ-12" / "BUG-3"；keyChip(type, id) 编号徽标 + 复制按钮；bindCopy(container) 绑定复制。
 * XSDevTrace.md(text) 安全的简易 Markdown 渲染（先整体转义，再做有限的格式化）。
 *
 * 数据只取研发中心接口；演示模式（XS.mode !== "live"）使用内置演示数据。所有外部文本均转义；链接只接受 http/https 与站内 # 路由。
 */
(function (root) {
  "use strict";

  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS || {}; }
  function live() { return xs().mode === "live"; }
  function can(p) { var X = xs(); return typeof X.can === "function" && !!X.can(p); }
  function toast(m) { var X = xs(); if (typeof X.toast === "function") X.toast(m); }
  function api(path, opts) {
    var X = xs();
    if (typeof X.api === "function") return X.api(path, opts || {});
    return Promise.reject(new Error("接口未就绪"));
  }
  function errText(e) {
    if (e && e.status === 403) return "没有权限，请联系管理员分配研发中心权限";
    if (e && e.status === 404) return "暂不可用";
    return (e && (e.message || e.error)) || String(e || "未知错误");
  }
  function pad(n) { return String(n).padStart(2, "0"); }
  function fmt(t) {
    if (!t) return "—";
    var d = new Date(t);
    return isNaN(d) ? String(t).slice(0, 16) : (d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }
  function arr(v) { return Array.isArray(v) ? v : []; }
  // 只放行 http/https 外链与站内 # 路由。
  function safeUrl(u) {
    u = String(u == null ? "" : u).trim();
    if (/^https?:\/\/[^\s"'<>]+$/i.test(u)) return u;
    if (/^#\/[^\s"'<>]*$/.test(u)) return u;
    return "";
  }
  function emptyBox(o) {
    var X = root.XSEmpty;
    if (X && typeof X.html === "function") return X.html(o);
    return '<p class="xdt-empty">' + E(o.title) + "</p>";
  }

  (function ensureCss() {
    if (typeof document === "undefined" || !document.head || document.getElementById("xs-devtrace-css")) return;
    var l = document.createElement("link");
    l.id = "xs-devtrace-css"; l.rel = "stylesheet"; l.href = "xs-devtrace.css";
    document.head.appendChild(l);
  })();

  /* ---------------------------------------------------------------- 常量 */
  var STAGES = [["code", "代码"], ["build", "构建"], ["artifact", "制品"], ["release", "发布"], ["env", "环境"]];
  // 四色：正常 绿 / 警告 橙 / 异常 红 / 未知 灰
  var STATE = { done: ["已完成", "ok"], running: ["进行中", "warn"], failed: ["失败", "bad"], none: ["未开始", "unknown"] };
  var KINDS = [["commit", "提交"], ["mr", "合并请求"], ["run", "构建运行"], ["release", "发布"], ["alert", "告警"]];
  var KIND_NAME = {}; KINDS.forEach(function (k) { KIND_NAME[k[0]] = k[1]; });
  var ENV_NAME = { prod: "生产", production: "生产", pre: "预发", staging: "预发", stage: "预发", uat: "验收", test: "测试", testing: "测试", qa: "测试", dev: "开发", develop: "开发" };
  var REQ_ST = { todo: "待评审", ready: "待开发", doing: "开发中", testing: "测试中", accepting: "待验收", done: "已完成", rejected: "已关闭" };
  var BUG_ST = { todo: "新建", doing: "处理中", fixed: "已修复", verified: "已验证", closed: "已关闭", reopened: "重新打开" };
  var ITEM_ST = { succeeded: ["成功", "ok"], success: ["成功", "ok"], done: ["完成", "ok"], merged: ["已合并", "ok"], healthy: ["正常", "ok"], resolved: ["已恢复", "ok"],
    running: ["运行中", "warn"], queued: ["排队中", "unknown"], pending: ["等待中", "unknown"], opened: ["待合并", "warn"], open: ["待合并", "warn"], progressing: ["进行中", "warn"], firing: ["告警中", "bad"],
    failed: ["失败", "bad"], error: ["失败", "bad"], cancelled: ["已取消", "unknown"], closed: ["已关闭", "unknown"], rolled_back: ["已回滚", "warn"] };
  var COLOR = { ok: "#16a34a", warn: "#d97706", bad: "#dc2626", unknown: "#6b7280" };

  function normType(t) { t = String(t || "").toLowerCase(); return t === "bug" || t === "bugs" ? "bug" : "requirement"; }
  function plural(t) { return normType(t) === "bug" ? "bugs" : "requirements"; }
  function keyOf(type, id) { return (normType(type) === "bug" ? "BUG-" : "REQ-") + String(id == null ? "" : id).replace(/[^\d]/g, ""); }
  function envName(e) { var k = String(e || "").toLowerCase(); return ENV_NAME[k] || String(e || "—"); }
  function stTag(state, text) {
    var c = COLOR[state] || COLOR.unknown;
    return '<span class="xdt-st xdt-st--' + E(state || "unknown") + '" style="color:' + c + '"><i style="background:' + c + '"></i>' + E(text) + "</span>";
  }
  function itemStatus(s) { if (!s) return ""; var x = ITEM_ST[String(s).toLowerCase()] || [s, "unknown"]; return stTag(x[1], x[0]); }
  function shortRef(kind, ref) { ref = String(ref == null ? "" : ref); return kind === "commit" && /^[0-9a-f]{12,}$/i.test(ref) ? ref.slice(0, 8) : ref; }

  /* ---------------------------------------------------------------- 复制 */
  function copyText(text) {
    var nav = root.navigator;
    if (nav && nav.clipboard && typeof nav.clipboard.writeText === "function") {
      return Promise.resolve(nav.clipboard.writeText(text)).then(function () { toast("已复制：" + (text.length > 40 ? text.slice(0, 40) + "…" : text)); return true; }, function () { toast("复制失败，请手动选中复制"); return false; });
    }
    toast("当前浏览器不支持自动复制，请手动选中复制");
    return Promise.resolve(false);
  }
  function keyChip(type, id) {
    var k = keyOf(type, id);
    return '<span class="xdt-key"><code>' + E(k) + '</code><button type="button" class="xdt-copy" data-xdt="copy" data-text="' + E(k) + '" title="复制编号" aria-label="复制编号 ' + E(k) + '">复制</button></span>';
  }
  // bindCopy：为不在本模块容器内的复制按钮（如抽屉标题）绑定点击。
  function bindCopy(box) {
    if (!box || !box.querySelectorAll) return;
    [].forEach.call(box.querySelectorAll('[data-xdt="copy"]'), function (b) {
      b.onclick = function (ev) { if (ev && ev.stopPropagation) ev.stopPropagation(); copyText(b.getAttribute("data-text") || ""); };
    });
  }

  /* ---------------------------------------------------------------- 安全 Markdown */
  function inline(s) {
    // s 已转义
    return s
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
      .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, function (m, t, u) {
        var raw = u.replace(/&amp;/g, "&");
        return safeUrl(raw) && !/^#/.test(raw) ? '<a href="' + u + '" target="_blank" rel="noopener noreferrer">' + t + "</a>" : (safeUrl(raw) ? '<a href="' + u + '">' + t + "</a>" : t);
      });
  }
  function md(text) {
    var lines = E(String(text == null ? "" : text).replace(/\r\n?/g, "\n")).split("\n");
    var out = [], list = "", para = [], code = null;
    function flushPara() { if (para.length) { out.push("<p>" + para.map(inline).join("<br>") + "</p>"); para = []; } }
    function closeList() { if (list) { out.push("</" + list + ">"); list = ""; } }
    lines.forEach(function (ln) {
      if (code) { if (/^\s*```/.test(ln)) { out.push("<pre><code>" + code.join("\n") + "</code></pre>"); code = null; } else code.push(ln); return; }
      if (/^\s*```/.test(ln)) { flushPara(); closeList(); code = []; return; }
      var h = ln.match(/^(#{1,6})\s+(.*)$/), ul = ln.match(/^\s*[-*+]\s+(.*)$/), ol = ln.match(/^\s*\d+[.)]\s+(.*)$/);
      if (h) { flushPara(); closeList(); var lv = Math.min(6, h[1].length + 3); out.push("<h" + lv + ">" + inline(h[2]) + "</h" + lv + ">"); return; }
      if (ul || ol) {
        flushPara();
        var want = ul ? "ul" : "ol";
        if (list !== want) { closeList(); out.push("<" + want + ">"); list = want; }
        out.push("<li>" + inline((ul || ol)[1]) + "</li>");
        return;
      }
      if (!ln.trim()) { flushPara(); closeList(); return; }
      closeList();
      para.push(ln);
    });
    if (code) out.push("<pre><code>" + code.join("\n") + "</code></pre>");
    flushPara(); closeList();
    return out.join("");
  }

  /* ---------------------------------------------------------------- 演示数据 */
  function isoAgo(h) { return new Date(Date.now() - h * 3600000).toISOString(); }
  var DEMO_LINKS = {}; // key → manual items added in demo
  var DEMO_SEQ = 900;
  function demoTrace(type, id) {
    var key = keyOf(type, id), bug = normType(type) === "bug";
    var manual = DEMO_LINKS[key] || [];
    var code = [
      { kind: "mr", ref: "!128", title: key + " 登录页支持企业 SSO", url: "", author: "zhangwei", status: "merged", occurred_at: isoAgo(30), app_id: 3, source: "auto" },
      { kind: "commit", ref: "9f3c2a71d0b4e5f6a7b8", title: "feat(auth): " + key + " 接入 OIDC 回调", url: "", author: "zhangwei", status: "", occurred_at: isoAgo(32), app_id: 3, source: "auto" }
    ].concat(manual.filter(function (x) { return x.kind === "commit" || x.kind === "mr"; }));
    return {
      item: { type: bug ? "bug" : "requirement", id: Number(id) || id, title: bug ? "工作负载列表分页错乱" : "登录页支持企业 SSO", status: bug ? "fixed" : "testing" },
      stages: [
        { key: "code", state: "done", at: isoAgo(30), items: code },
        { key: "build", state: "done", at: isoAgo(29), items: [{ kind: "run", ref: "412", title: "web-ci #412", url: "#/cicd/pipeline/5", author: "zhangwei", status: "succeeded", occurred_at: isoAgo(29), app_id: 3, source: "auto" }].concat(manual.filter(function (x) { return x.kind === "run"; })) },
        { key: "artifact", state: "done", at: isoAgo(29), items: [{ kind: "run", ref: "412", title: "harbor.example.com/team/web:1.8.0-9f3c2a7", url: "", author: "", status: "succeeded", occurred_at: isoAgo(29), app_id: 3, source: "auto" }] },
        { key: "release", state: "running", at: isoAgo(2), items: [
          { kind: "release", ref: "88", title: "web → 测试", url: "", author: "lisi", env: "test", status: "succeeded", occurred_at: isoAgo(26), app_id: 3, source: "auto" },
          { kind: "release", ref: "91", title: "web → 预发", url: "", author: "lisi", env: "pre", status: "running", occurred_at: isoAgo(2), app_id: 3, source: "auto" }
        ].concat(manual.filter(function (x) { return x.kind === "release" || x.kind === "alert"; })) },
        { key: "env", state: "none", at: null, items: [] }
      ],
      envs: [
        { env: "test", app_id: 3, app_name: "web", release_id: 88, image: "harbor.example.com/team/web:1.8.0-9f3c2a7", at: isoAgo(26) },
        { env: "pre", app_id: 3, app_name: "web", release_id: 91, image: "harbor.example.com/team/web:1.8.0-9f3c2a7", at: isoAgo(2) },
        { env: "prod", app_id: 3, app_name: "web", release_id: 80, image: "harbor.example.com/team/web:1.7.2", at: isoAgo(120) }
      ]
    };
  }
  function demoRelease(id) {
    return {
      release: { id: Number(id) || id, app_id: 3, app_name: "web", env: "pre", image: "harbor.example.com/team/web:1.8.0-9f3c2a7", status: "succeeded", created_at: isoAgo(2) },
      requirements: [{ id: 12, title: "登录页支持企业 SSO", status: "testing" }, { id: 15, title: "工作负载列表支持按标签筛选", status: "accepting" }],
      bugs: [{ id: 3, title: "工作负载列表分页错乱", status: "fixed" }],
      commits: [
        { sha: "9f3c2a71d0b4e5f6a7b8", title: "feat(auth): REQ-12 接入 OIDC 回调", author: "zhangwei" },
        { sha: "1a2b3c4d5e6f70819203", title: "fix(list): BUG-3 修正翻页偏移", author: "wangfang" },
        { sha: "77aa88bb99cc00dd11ee", title: "feat(list): REQ-15 标签筛选", author: "lisi" }
      ],
      notes_md: "## web 1.8.0 发布说明\n\n### 新功能\n- REQ-12 登录页支持企业 SSO\n- REQ-15 工作负载列表支持按标签筛选\n\n### 缺陷修复\n- BUG-3 工作负载列表分页错乱\n\n镜像：`harbor.example.com/team/web:1.8.0-9f3c2a7`"
    };
  }

  /* ---------------------------------------------------------------- 链接推导 */
  function itemHref(it, appNames) {
    var u = safeUrl(it.url);
    if (u) return u;
    if (it.kind === "run" && it.pipeline_id) return "#/cicd/pipeline/" + encodeURIComponent(it.pipeline_id);
    if (it.kind === "run") return "#/cicd/build";
    if (it.kind === "release" && it.app_id && appNames[it.app_id]) return "#/delivery/" + encodeURIComponent(appNames[it.app_id]) + "/" + encodeURIComponent("部署编排");
    return "";
  }

  /* ---------------------------------------------------------------- 交付进度 */
  function render(el, opts) {
    if (!el) return null;
    opts = opts || {};
    var type = normType(opts.type), id = opts.id;
    var S = { open: {}, form: false, confirm: "", data: null, err: "", formErr: "", busy: false };
    var ctl = { reload: load };
    el.__xdt = S;
    el.innerHTML = '<p class="xdt-loading">正在读取交付进度…</p>';

    function load() {
      S.err = "";
      var p = live() ? api("/devcenter/" + plural(type) + "/" + encodeURIComponent(id) + "/trace") : Promise.resolve(demoTrace(type, id));
      return p.then(function (r) { if (el.__xdt !== S) return; S.data = r || {}; draw(); })
        .catch(function (e) { if (el.__xdt !== S) return; S.data = null; S.err = e && e.status === 404 ? "交付进度暂不可用。" : e && e.status === 403 ? "没有查看交付进度的权限。" : "读取交付进度失败：" + errText(e); draw(); });
    }

    function hint() {
      var k = keyOf(type, id);
      return '<div class="xdt-hint"><span>在提交信息或分支名中写 <code>' + E(k) + "</code>（也支持 <code>#" + E(k) + "</code>、<code>[" + E(k) + ']</code>，不区分大小写）即可自动关联，例如 <code>git commit -m "' + E(k) + ' 修复登录跳转"</code>。</span>' +
        '<button type="button" class="x-btn x-btn--s xdt-copy" data-xdt="copy" data-text="' + E(k) + '">复制编号</button></div>';
    }

    function draw() {
      var manage = can("devcenter:manage");
      if (S.err) { el.innerHTML = '<p class="xdt-err">' + E(S.err) + "</p>" + hint(); return; }
      var d = S.data || {}, stages = arr(d.stages), envs = arr(d.envs), byKey = {}, appNames = {};
      stages.forEach(function (s) { if (s && s.key) byKey[s.key] = s; });
      envs.forEach(function (e) { if (e && e.app_id && e.app_name) appNames[e.app_id] = e.app_name; });
      var total = 0;
      stages.forEach(function (s) { total += arr(s.items).length; });

      var line = '<ol class="xdt-line" aria-label="交付进度">' + STAGES.map(function (st, i) {
        var s = byKey[st[0]] || { state: "none", items: [] }, x = STATE[s.state] || STATE.none, n = arr(s.items).length, open = !!S.open[st[0]];
        return '<li class="xdt-stage xdt-stage--' + x[1] + (open ? " on" : "") + '">' +
          (i ? '<span class="xdt-bar" style="background:' + (x[1] === "unknown" ? "#e5e7eb" : COLOR[x[1]]) + '"></span>' : "") +
          '<span class="xdt-dot" style="border-color:' + COLOR[x[1]] + ";background:" + (s.state === "done" ? COLOR.ok : "#fff") + '"></span>' +
          '<b class="xdt-name">' + E(st[1]) + "</b>" + stTag(x[1], x[0]) +
          '<span class="xdt-at">' + E(s.at ? fmt(s.at) : "—") + "</span>" +
          (n ? '<button type="button" class="xdt-more" data-xdt="toggle" data-stage="' + st[0] + '" aria-expanded="' + open + '">' + n + " 项 " + (open ? "收起" : "展开") + "</button>" : '<span class="xdt-none">暂无记录</span>') +
          "</li>";
      }).join("") + "</ol>";

      var detail = STAGES.filter(function (st) { return S.open[st[0]] && byKey[st[0]] && arr(byKey[st[0]].items).length; }).map(function (st) {
        return '<div class="xdt-detail"><h5>' + E(st[1]) + "</h5><ul>" + arr(byKey[st[0]].items).map(function (it) { return itemRow(it, appNames, manage); }).join("") + "</ul></div>";
      }).join("");

      var envHtml = envs.length ? '<div class="xdt-envs"><span class="xdt-label">各环境当前版本</span>' + envs.map(function (e) {
        var tagPart = String(e.image || "").split("/").pop();
        var href = e.app_name ? "#/delivery/" + encodeURIComponent(e.app_name) + "/" + encodeURIComponent("部署编排") : "";
        var inner = "<b>" + E(envName(e.env)) + "</b>" + (e.app_name ? '<span class="xdt-sub">' + E(e.app_name) + "</span>" : "") + (tagPart ? '<code title="' + E(e.image) + '">' + E(tagPart) + "</code>" : "") + '<span class="xdt-sub">' + E(fmt(e.at)) + "</span>";
        return href ? '<a class="xdt-chip" href="' + href + '">' + inner + "</a>" : '<span class="xdt-chip">' + inner + "</span>";
      }).join("") + "</div>" : '<p class="xdt-sub xdt-noenv">还没有发布到任何环境。</p>';

      var form = "";
      if (manage && S.form) {
        form = '<div class="xdt-form" data-xdt-form><div class="xdt-form-row">' +
          '<label>类型<select class="x-input" data-f="kind">' + KINDS.map(function (k) { return '<option value="' + k[0] + '">' + E(k[1]) + "</option>"; }).join("") + "</select></label>" +
          '<label>编号<input class="x-input" data-f="ref" maxlength="200" placeholder="提交哈希、合并请求号、运行号或发布号" autocomplete="off"></label></div><div class="xdt-form-row">' +
          '<label>标题（可选）<input class="x-input" data-f="title" maxlength="200" autocomplete="off"></label>' +
          '<label>链接（可选）<input class="x-input" data-f="url" maxlength="500" placeholder="https://…" autocomplete="off"></label></div>' +
          '<p class="xdt-err" data-xdt-ferr' + (S.formErr ? "" : " hidden") + ">" + E(S.formErr) + "</p>" +
          '<div class="xdt-form-foot"><button type="button" class="x-btn" data-xdt="cancel-add">取消</button><button type="button" class="x-btn x-btn--pri" data-xdt="save-add"' + (S.busy ? " disabled" : "") + ">关联</button></div></div>";
      }

      el.innerHTML = '<div class="xdt">' +
        '<div class="xdt-top"><span class="xdt-sub">共 ' + total + " 条关联记录</span>" + (manage && !S.form ? '<button type="button" class="x-btn x-btn--s" data-xdt="add">+ 手动关联</button>' : "") + "</div>" +
        line + detail + envHtml + form + (total ? "" : emptyBox({ kind: "pipeline", size: "s", title: "还没有关联的代码、构建或发布", desc: "按下方提示在提交信息或分支名中写上编号，推送后会自动出现在这里。" })) + hint() + "</div>";
    }

    function itemRow(it, appNames, manage) {
      var href = itemHref(it, appNames), label = it.title || (KIND_NAME[it.kind] || it.kind) + " " + shortRef(it.kind, it.ref);
      var linkId = it.link_id != null ? it.link_id : it.id;
      var removable = manage && it.source === "manual" && linkId != null && linkId !== "";
      var confirming = removable && String(S.confirm) === String(linkId);
      return "<li>" + '<span class="xdt-kind">' + E(KIND_NAME[it.kind] || it.kind || "—") + "</span>" +
        '<code class="xdt-ref">' + E(shortRef(it.kind, it.ref)) + "</code>" +
        (href ? '<a class="xdt-title" href="' + E(href) + '"' + (/^https?:/i.test(href) ? ' target="_blank" rel="noopener noreferrer"' : "") + ">" + E(label) + "</a>" : '<span class="xdt-title">' + E(label) + "</span>") +
        (it.env ? '<span class="xdt-envtag">' + E(envName(it.env)) + "</span>" : "") + itemStatus(it.status) +
        '<span class="xdt-sub">' + E(it.author || "") + (it.author ? " · " : "") + E(fmt(it.occurred_at)) + "</span>" +
        (it.source === "manual" ? '<span class="xdt-manual">手动</span>' : "") +
        (removable ? (confirming
          ? '<span class="xdt-confirm">确定移除这条手动关联？<button type="button" class="x-btn x-btn--s x-btn--danger" data-xdt="del-yes" data-link="' + E(linkId) + '">确认移除</button><button type="button" class="x-btn x-btn--s" data-xdt="del-no">取消</button></span>'
          : '<button type="button" class="x-act x-act--danger" data-xdt="del" data-link="' + E(linkId) + '">移除</button>') : "") + "</li>";
    }

    function val(f) { var n = el.querySelector('[data-f="' + f + '"]'); return n ? String(n.value || "").trim() : ""; }
    function formErr(m) { S.formErr = m; var p = el.querySelector("[data-xdt-ferr]"); if (p) { p.textContent = m; p.hidden = !m; } }

    function save() {
      var body = { kind: val("kind") || "commit", ref: val("ref") };
      var title = val("title"), url = val("url");
      if (!KIND_NAME[body.kind]) return formErr("请选择关联类型");
      if (!body.ref) return formErr("请填写编号");
      if (body.ref.length > 200) return formErr("编号不能超过 200 个字");
      if (/\s/.test(body.ref) && body.kind !== "alert") return formErr("编号不能包含空格");
      if (url && !/^https?:\/\/\S+$/i.test(url)) return formErr("链接需以 http:// 或 https:// 开头");
      if (title) body.title = title;
      if (url) body.url = url;
      formErr("");
      S.busy = true;
      var p;
      if (live()) p = api("/devcenter/" + plural(type) + "/" + encodeURIComponent(id) + "/links", { method: "POST", body: body });
      else {
        var k = keyOf(type, id);
        (DEMO_LINKS[k] = DEMO_LINKS[k] || []).push(Object.assign({ id: ++DEMO_SEQ, source: "manual", author: "演示用户", occurred_at: new Date().toISOString() }, body));
        p = Promise.resolve({});
      }
      return p.then(function () { S.busy = false; S.form = false; S.formErr = ""; toast("已关联"); return load(); })
        .catch(function (e) { S.busy = false; var b = el.querySelector('[data-xdt="save-add"]'); if (b) b.disabled = false; formErr(e && e.status === 409 ? "该记录已关联" : errText(e)); });
    }

    function remove(linkId) {
      var p;
      if (live()) p = api("/devcenter/links/" + encodeURIComponent(linkId), { method: "DELETE" });
      else {
        var k = keyOf(type, id);
        DEMO_LINKS[k] = (DEMO_LINKS[k] || []).filter(function (x) { return String(x.id) !== String(linkId); });
        p = Promise.resolve({});
      }
      return p.then(function () { S.confirm = ""; toast("已移除关联"); return load(); })
        .catch(function (e) { S.confirm = ""; draw(); toast("移除失败：" + errText(e)); });
    }

    function onClick(ev) {
      var t = ev && ev.target && ev.target.closest ? ev.target.closest("[data-xdt]") : null;
      if (!t || el.__xdt !== S) return;
      var a = t.getAttribute("data-xdt");
      if (a === "copy") return copyText(t.getAttribute("data-text") || "");
      if (a === "toggle") { var k = t.getAttribute("data-stage"); S.open[k] = !S.open[k]; return draw(); }
      if (!can("devcenter:manage")) return;
      if (a === "add") { S.form = true; S.formErr = ""; draw(); var r = el.querySelector('[data-f="ref"]'); if (r && r.focus) r.focus(); return; }
      if (a === "cancel-add") { S.form = false; S.formErr = ""; return draw(); }
      if (a === "save-add") { if (S.busy) return; t.disabled = true; return save(); }
      if (a === "del") { S.confirm = t.getAttribute("data-link"); return draw(); }
      if (a === "del-no") { S.confirm = ""; return draw(); }
      if (a === "del-yes") { t.disabled = true; return remove(t.getAttribute("data-link")); }
    }
    if (el.__xdtClick && el.removeEventListener) el.removeEventListener("click", el.__xdtClick);
    el.__xdtClick = onClick;
    if (el.addEventListener) el.addEventListener("click", onClick);
    ctl.done = load();
    return ctl;
  }

  /* ---------------------------------------------------------------- 发布包含的需求 */
  function releaseItems(el, releaseId) {
    if (!el) return null;
    var token = {};
    el.__xdtRel = token;
    el.innerHTML = '<p class="xdt-loading">正在读取发布内容…</p>';
    var notes = "";
    function onClick(ev) {
      var t = ev && ev.target && ev.target.closest ? ev.target.closest("[data-xdt]") : null;
      if (!t || el.__xdtRel !== token) return;
      var a = t.getAttribute("data-xdt");
      if (a === "copy-notes") return copyText(notes);
      if (a === "copy") return copyText(t.getAttribute("data-text") || "");
      if (a === "open-item") {
        var D = root.XSDev;
        if (D && typeof D.openDetail === "function") { closeRelease(); D.openDetail(t.getAttribute("data-k"), t.getAttribute("data-id")); }
      }
    }
    if (el.__xdtRelClick && el.removeEventListener) el.removeEventListener("click", el.__xdtRelClick);
    el.__xdtRelClick = onClick;
    if (el.addEventListener) el.addEventListener("click", onClick);

    var p = live() ? api("/devcenter/releases/" + encodeURIComponent(releaseId) + "/items") : Promise.resolve(demoRelease(releaseId));
    var done = p.then(function (r) {
      if (el.__xdtRel !== token) return;
      r = r || {};
      var rel = r.release || {}, reqs = arr(r.requirements), bugs = arr(r.bugs), commits = arr(r.commits);
      notes = String(r.notes_md || "");
      var openable = !!(root.XSDev && typeof root.XSDev.openDetail === "function");
      var itemList = function (list, type) {
        var k = plural(type), stMap = type === "bug" ? BUG_ST : REQ_ST;
        return "<ul>" + list.map(function (x) {
          return '<li><code class="xdt-ref">' + E(keyOf(type, x.id)) + "</code>" +
            (openable ? '<button type="button" class="xdt-title xdt-linkbtn" data-xdt="open-item" data-k="' + k + '" data-id="' + E(x.id) + '">' + E(x.title || "（已删除）") + "</button>" : '<span class="xdt-title">' + E(x.title || "（已删除）") + "</span>") +
            '<span class="xdt-envtag">' + E(stMap[x.status] || x.status || "—") + "</span></li>";
        }).join("") + "</ul>";
      };
      var st = ITEM_ST[String(rel.status || "").toLowerCase()];
      var html = '<div class="xdt xdt-rel"><dl class="xdt-sum">' +
        "<dt>发布</dt><dd>#" + E(rel.id != null ? rel.id : releaseId) + "</dd>" +
        "<dt>应用</dt><dd>" + E(rel.app_name || (rel.app_id ? "#" + rel.app_id : "—")) + "</dd>" +
        "<dt>环境</dt><dd>" + E(envName(rel.env)) + "</dd>" +
        "<dt>状态</dt><dd>" + (st ? stTag(st[1], st[0]) : E(rel.status || "—")) + "</dd>" +
        '<dt>镜像</dt><dd><code class="xdt-break">' + E(rel.image || "—") + "</code></dd>" +
        "<dt>时间</dt><dd>" + E(fmt(rel.created_at)) + "</dd></dl>";
      if (!reqs.length && !bugs.length && !commits.length) {
        html += emptyBox({ kind: "release", size: "s", title: "这次发布没有识别到关联的需求或缺陷", desc: "在提交信息或分支名中写上 REQ-12 / BUG-3 等编号，之后的发布会自动列出包含的需求。" });
      } else {
        html += '<div class="xdt-sec"><h5>包含需求 <span class="xdt-sub">' + reqs.length + "</span></h5>" + (reqs.length ? itemList(reqs, "requirement") : '<p class="xdt-sub">无</p>') + "</div>" +
          '<div class="xdt-sec"><h5>修复缺陷 <span class="xdt-sub">' + bugs.length + "</span></h5>" + (bugs.length ? itemList(bugs, "bug") : '<p class="xdt-sub">无</p>') + "</div>" +
          '<div class="xdt-sec"><h5>提交 <span class="xdt-sub">' + commits.length + "</span></h5>" + (commits.length ? "<ul>" + commits.map(function (c) {
            return '<li><code class="xdt-ref" title="' + E(c.sha) + '">' + E(shortRef("commit", c.sha)) + '</code><span class="xdt-title">' + E(c.title || "") + '</span><span class="xdt-sub">' + E(c.author || "") + "</span></li>";
          }).join("") + "</ul>" : '<p class="xdt-sub">无</p>') + "</div>";
      }
      html += '<div class="xdt-sec"><h5>发布说明' + (notes ? '<button type="button" class="x-btn x-btn--s" data-xdt="copy-notes">复制 Markdown</button>' : "") + "</h5>" +
        (notes ? '<div class="xdt-md">' + md(notes) + "</div>" : '<p class="xdt-sub">暂无发布说明。</p>') + "</div></div>";
      el.innerHTML = html;
    }).catch(function (e) {
      if (el.__xdtRel !== token) return;
      el.innerHTML = '<p class="xdt-err">' + (e && e.status === 404 ? "发布内容暂不可用。" : e && e.status === 403 ? "没有查看发布内容的权限。" : "读取发布内容失败：" + E(errText(e))) + "</p>";
    });
    return { done: done };
  }

  /* ---------------------------------------------------------------- 抽屉 */
  var RD = null;
  function closeRelease() {
    if (!RD) return;
    RD.mask.remove(); RD.el.remove();
    if (typeof document !== "undefined") document.removeEventListener("keydown", RD.esc);
    RD = null;
  }
  function openRelease(releaseId, opts) {
    if (typeof document === "undefined" || !document.body) return null;
    opts = opts || {};
    closeRelease();
    var mask = document.createElement("div"), el = document.createElement("aside");
    mask.className = "xd-mask xdt-mask"; el.className = "xd-drawer xdt-drawer";
    el.setAttribute("role", "dialog"); el.setAttribute("aria-modal", "true"); el.setAttribute("aria-label", "发布包含的需求");
    el.innerHTML = '<div class="xd-dh"><div class="xd-dh-main"><span class="xdt-sub">发布 #' + E(releaseId) + (opts.subtitle ? " · " + E(opts.subtitle) : "") + "</span><h3>" + E(opts.title || "发布包含的需求") + '</h3></div><div class="xd-dh-act"><button type="button" class="x-btn" data-xdt-close>关闭</button></div></div><div class="xd-db" data-xdt-body></div>';
    document.body.appendChild(mask); document.body.appendChild(el);
    RD = { mask: mask, el: el, esc: function (e) { if (e.key === "Escape") closeRelease(); } };
    mask.onclick = closeRelease;
    el.querySelector("[data-xdt-close]").onclick = closeRelease;
    document.addEventListener("keydown", RD.esc);
    return releaseItems(el.querySelector("[data-xdt-body]"), releaseId);
  }

  root.XSDevTrace = {
    render: render,
    releaseItems: releaseItems,
    openRelease: openRelease,
    closeRelease: closeRelease,
    keyOf: keyOf,
    keyChip: keyChip,
    bindCopy: bindCopy,
    md: md,
    _demoTrace: demoTrace,
    _demoRelease: demoRelease,
    _safeUrl: safeUrl
  };
})(typeof window !== "undefined" ? window : this);
