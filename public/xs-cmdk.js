/*
 * 星枢 Matrix · 全局搜索面板（⌘K / Ctrl+K）增强  window.XSCmdk
 *
 * 接管 index.html 顶栏已有的 #cmdk 弹层（不改其打开 / 关闭逻辑）：
 *   - 空输入：「最近访问」（localStorage，最多 8 条，按用户隔离）+「常用操作」（按 XS.can 权限过滤）。
 *   - 输入时：页面 + 操作 + 真实资源分组（集群、主机、应用、企业空间、需求、缺陷），每组最多 5 条；
 *     资源请求 300ms 防抖，新输入会取消旧请求并丢弃过期结果；接口不可用时只给一行简洁提示。
 *   - ↑↓ 选择、↵ 打开、Esc 关闭；点击资源跳到详情，或跳到列表并把名称填进列表的搜索框。
 *   - 演示模式（XS.mode !== "live"）只搜页面与主脚本提供的演示数据，不发请求。
 *
 * 接线：主脚本在 ⌘K 代码之后设置 window.XSCmdkHost = { el, input, list, pages, navGo, ic, esc, closeAll, demo }，
 * 并在 drawCmdk() 开头调用 XSCmdk.onOpen()（见 /workspace/work/briefs/v131/u-cmdk.md 的补丁）。
 */
(function (root) {
  "use strict";

  var MAX_RECENT = 8, PER_GROUP = 5, DEBOUNCE = 300, MAX_PAGES = 8, FOLLOW_MS = 100, FOLLOW_TRIES = 40;
  var RECENT_PREFIX = "xs-cmdk-recent:";
  var DOT = { ok: "#16a34a", warn: "#d97706", bad: "#dc2626", off: "#6b7280" };

  function X() { return root.XS || null; }
  function live() { var x = X(); return !!(x && x.mode === "live"); }
  function can(p) { var x = X(); return !p || !!(x && typeof x.can === "function" && x.can(p)); }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function low(s) { return String(s == null ? "" : s).toLowerCase(); }
  function hit(q, parts) { if (!q) return true; for (var i = 0; i < parts.length; i++) if (low(parts[i]).indexOf(q) >= 0) return true; return false; }
  function itemsOf(d) { return !d ? [] : Array.isArray(d) ? d : Array.isArray(d.items) ? d.items : Array.isArray(d.data) ? d.data : []; }
  function safeHash(h) { h = String(h || ""); return /^#\/[^\s"'<>`]*$/.test(h) ? h : ""; }
  function baseHash(h) { return String(h || "").split("?")[0]; }
  function timers() { return { set: root.setTimeout ? root.setTimeout.bind(root) : setTimeout, clear: root.clearTimeout ? root.clearTimeout.bind(root) : clearTimeout }; }

  /* ---------------------------------------------------------------- 最近访问 */
  function storage() { try { return root.localStorage || null; } catch (e) { return null; } }
  function recentKey() {
    var u = (X() && X().user) || null;
    return RECENT_PREFIX + (u && (u.id != null ? u.id : u.username) != null ? String(u.id != null ? u.id : u.username) : "local");
  }
  function recentRead() {
    var ls = storage(), raw = null;
    try { raw = ls && ls.getItem(recentKey()); } catch (e) { raw = null; }
    var list; try { list = JSON.parse(raw || "[]"); } catch (e) { list = []; }
    if (!Array.isArray(list)) return [];
    return list.filter(function (r) { return r && typeof r.t === "string" && r.t && safeHash(r.h); }).slice(0, MAX_RECENT).map(function (r) {
      var o = { h: safeHash(r.h), t: String(r.t).slice(0, 120), m: typeof r.m === "string" ? r.m.slice(0, 60) : "" };
      if (r.f && typeof r.f === "object") o.f = { q: r.f.q != null ? String(r.f.q).slice(0, 120) : "", open: r.f.open != null ? String(r.f.open).slice(0, 64) : "", kind: r.f.kind ? String(r.f.kind).slice(0, 20) : "" };
      return o;
    });
  }
  function recentPush(entry) {
    var h = safeHash(entry && entry.h);
    if (!h || !entry.t || /^#\/login\b/.test(h)) return;
    var id = h + "|" + (entry.f ? entry.f.kind + ":" + (entry.f.open || entry.f.q) : "");
    var list = recentRead().filter(function (r) { return (r.h + "|" + (r.f ? r.f.kind + ":" + (r.f.open || r.f.q) : "")) !== id; });
    var o = { h: h, t: String(entry.t), m: entry.m || "" };
    if (entry.f) o.f = entry.f;
    list.unshift(o);
    var ls = storage();
    try { if (ls) ls.setItem(recentKey(), JSON.stringify(list.slice(0, MAX_RECENT))); } catch (e) {}
  }
  function recentClear() { var ls = storage(); try { if (ls) ls.removeItem(recentKey()); } catch (e) {} }

  /* ---------------------------------------------------------------- 页面 */
  function pagesAll() {
    var host = S.host;
    var list = [];
    try { list = host && typeof host.pages === "function" ? host.pages() || [] : []; } catch (e) { list = []; }
    return list.filter(function (p) { return p && p.t && safeHash(p.h); });
  }
  // 路由对应的页面标题：先精确匹配，再取最长前缀（如 #/delivery/foo 归到「应用」）
  function titleFor(hash) {
    var h = baseHash(hash), pages = pagesAll(), exact = null, pre = null;
    pages.forEach(function (p) {
      if (p.h === h) exact = exact || p;
      else if (h.indexOf(p.h + "/") === 0 && (!pre || p.h.length > pre.h.length)) pre = p;
    });
    if (exact) return { t: exact.t, m: exact.m && exact.m !== exact.t ? exact.m : exact.g || "" };
    var rest = pre ? h.slice(pre.h.length + 1).split("/")[0] : "";
    if (!pre) {
      // 详情页与列表页同级（如 #/delivery/<应用名> 与 #/delivery/apps）：归到同一路由下最短的页面
      var segs = h.slice(2).split("/");
      if (segs.length >= 2 && segs[1]) pages.forEach(function (p) {
        if (p.h.split("/")[1] === segs[0] && (!pre || p.h.length < pre.h.length)) pre = p;
      });
      rest = segs[segs.length - 1] || "";
    }
    if (pre) {
      var name = rest;
      try { name = decodeURIComponent(rest); } catch (e) {}
      return { t: name ? pre.t + " / " + name : pre.t, m: pre.g || "" };
    }
    return null;
  }

  /* ---------------------------------------------------------------- 常用操作 */
  // 每项要么跳到已存在入口所在页并点击该页的按钮（clicks 依次尝试），要么直接调用已有模块。
  // 演示模式下运维页不展示数据也没有这些按钮，只保留可用的「接入集群」（demo: true）。
  var ACTIONS = [
    { id: "app-new", t: "新建应用", m: "CI/CD · 应用", i: "apps", perm: "cicd:manage", h: "#/delivery/apps", clicks: ["[data-newapp]", "#appNew"] },
    { id: "pl-new", t: "新建流水线", m: "CI/CD · 流水线", i: "pipe", perm: "cicd:manage", h: "#/cicd/pipeline", clicks: ["[data-newpl]", "[data-action=\"create\"]", "#plNew"] },
    { id: "host-add", t: "添加主机", m: "集群 · 主机", i: "term", perm: "host:write", h: "#/cluster/hosts", clicks: ["[data-act=\"host-add\"]", "#hImp"] },
    { id: "cl-import", t: "接入集群", m: "集群 · 集群列表", i: "cube", perm: "cluster:manage", run: function () { root.document.dispatchEvent(new root.Event("xingshu:cluster-import")); }, demo: true },
    { id: "kubectl", t: "打开 kubectl 终端", m: "Web Shell", i: "console", perm: "workload:read", run: function () { root.XSKubectl.open(); }, when: function () { return !!(root.XSKubectl && typeof root.XSKubectl.open === "function"); } },
    { id: "ws-new", t: "新建企业空间", m: "设置 · 企业空间", i: "users", perm: "workspace:manage", h: "#/iam/workspaces", clicks: ["[data-wsp-click=\"new\"]"], when: function () { return !!root.XSWorkspace; } },
    { id: "req-new", t: "新建需求", m: "研发中心 · 需求管理", i: "list", perm: "devcenter:manage", h: "#/requirements", clicks: ["[data-new]"], when: function () { return !!root.XSDev; } },
    { id: "bug-new", t: "提交缺陷", m: "研发中心 · 缺陷总览", i: "bug", perm: "devcenter:manage", h: "#/bugs", clicks: ["[data-new]"], when: function () { return !!root.XSDev; } }
  ];
  function actionsFor(q) {
    var isLive = live();
    return ACTIONS.filter(function (a) {
      if (isLive ? !can(a.perm) : !a.demo) return false;
      if (a.when && !a.when()) return false;
      return hit(q, [a.t, a.m]);
    });
  }

  /* ---------------------------------------------------------------- 资源 */
  function statusDot(kind, x) {
    if (kind === "cluster") {
      var s = low(x.status || x.st);
      if (!s) return DOT.off;
      if (/^(ready|running|healthy|ok|connected|online)$|运行|正常|在线/.test(s)) return DOT.ok;
      if (/pending|connecting|unknown|待|检测/.test(s)) return DOT.warn;
      if (/error|fail|down|offline|unreachable|异常|失败|离线/.test(s)) return DOT.bad;
      return DOT.off;
    }
    if (kind === "host") return x.online === true || x.ag === "在线" ? DOT.ok : x.online === false && x.enrolled ? DOT.bad : DOT.off;
    return "";
  }
  var DEV_ST = {
    requirements: { todo: "待评审", ready: "待开发", doing: "开发中", testing: "测试中", accepting: "待验收", done: "已完成", rejected: "已关闭" },
    bugs: { todo: "新建", doing: "处理中", fixed: "已修复", verified: "已验证", closed: "已关闭", reopened: "重新打开" }
  };
  function joinMeta(a) { return a.filter(function (v) { return v != null && String(v) !== ""; }).join(" · "); }

  // 每组：perm 用于实时模式过滤；fetch(q, signal) 只用已有 GET 列表接口；to(x) 生成跳转目标。
  var GROUPS = [
    { k: "cluster", t: "集群", i: "cube", perm: "cluster:read",
      fetch: function (q, sig) { return get("/clusters", { keyword: q, page_size: PER_GROUP }, sig); },
      row: function (x) { return { t: x.name, m: joinMeta([x.env, x.version]), dot: statusDot("cluster", x) }; },
      to: function (x) { return { h: "#/cluster/list", f: { kind: "cluster", q: x.name } }; } },
    { k: "host", t: "主机", i: "term", perm: "host:read",
      fetch: function (q, sig) { return get("/hosts", { keyword: q, page_size: PER_GROUP }, sig); },
      row: function (x) { return { t: x.name || x.ip, m: joinMeta([x.ip, x.env, x.online ? "在线" : x.enrolled ? "离线" : "未注册"]), dot: statusDot("host", x) }; },
      to: function (x) { return { h: "#/cluster/hosts", f: { kind: "host", q: x.name || x.ip } }; } },
    { k: "app", t: "应用", i: "apps", perm: "cicd:read",
      // 应用列表接口不支持关键字：每次打开面板拉一页（100 条）后前端过滤
      fetch: function (q) {
        if (!S.appsCache) S.appsCache = get("/cicd/applications", { page: 1, size: 100 }).then(itemsOf, function (e) { S.appsCache = null; throw e; });
        return S.appsCache.then(function (list) { return list.filter(function (a) { return hit(q, [a.name, a.repo_url, a.image_repo]); }); });
      },
      row: function (x) { return { t: x.name, m: joinMeta([(x.envs || []).map(function (e) { return e && e.name; }).filter(Boolean).join("、"), x.branch]) }; },
      to: function (x) { return { h: "#/delivery/" + encodeURIComponent(x.name) }; } },
    { k: "ws", t: "企业空间", i: "users", perm: "workspace:read",
      fetch: function (q, sig) { return get("/workspaces", { keyword: q, page_size: PER_GROUP }, sig); },
      row: function (x) { return { t: x.name, m: joinMeta([x.description && String(x.description).slice(0, 30), x.member_count != null ? x.member_count + " 名成员" : ""]) }; },
      to: function (x) { return { h: "#/iam/workspaces", f: { kind: "ws", q: x.name, open: String(x.id) } }; } },
    { k: "req", t: "需求", i: "list", perm: "devcenter:read",
      fetch: function (q, sig) { return get("/devcenter/requirements", { q: q, page_size: PER_GROUP }, sig); },
      row: function (x) { return { t: x.title || x.name, m: joinMeta(["#" + x.id, DEV_ST.requirements[x.status] || x.status, x.priority]) }; },
      to: function (x) { return { h: "#/requirements", f: { kind: "req", q: x.title || "", open: String(x.id) } }; } },
    { k: "bug", t: "缺陷", i: "bug", perm: "devcenter:read",
      fetch: function (q, sig) { return get("/devcenter/bugs", { q: q, page_size: PER_GROUP }, sig); },
      row: function (x) { return { t: x.title || x.name, m: joinMeta(["#" + x.id, DEV_ST.bugs[x.status] || x.status, x.priority]) }; },
      to: function (x) { return { h: "#/bugs", f: { kind: "bug", q: x.title || "", open: String(x.id) } }; } }
  ];
  function get(path, query, sig) {
    var x = X();
    if (!x || typeof x.get !== "function") return Promise.reject(new Error("接口模块未加载"));
    var opts = { timeoutMs: 8000 };
    if (sig) opts.signal = sig;
    return Promise.resolve(x.get(path, query, opts)).then(itemsOf);
  }
  function liveGroups() { return GROUPS.filter(function (g) { return can(g.perm); }); }

  // 演示数据：主脚本 host.demo() 返回 { clusters, hosts, apps, reqs }（字段沿用主脚本里的简写）
  function demoResults(q) {
    var d = {};
    try { d = (S.host && typeof S.host.demo === "function" && S.host.demo()) || {}; } catch (e) { d = {}; }
    var out = {};
    function take(k, list, test, row, to) {
      var r = (Array.isArray(list) ? list : []).filter(test).slice(0, PER_GROUP).map(function (x) { var o = row(x); o.target = to(x); return o; });
      if (r.length) out[k] = r;
    }
    take("cluster", d.clusters, function (c) { return hit(q, [c.n || c.name, c.env]); },
      function (c) { return { t: c.n || c.name, m: joinMeta([c.env, c.ver || c.version]), dot: DOT.ok }; },
      function (c) { return { h: "#/cluster/list/" + encodeURIComponent(c.n || c.name) }; });
    take("host", d.hosts, function (h) { return hit(q, [h.n || h.name, h.ip, h.env]); },
      function (h) { return { t: h.n || h.name || h.ip, m: joinMeta([h.ip, h.env]), dot: statusDot("host", h) }; },
      function (h) { return { h: "#/cluster/hosts", f: { kind: "host", q: h.n || h.name || h.ip } }; });
    take("app", d.apps, function (a) { return hit(q, [a.name, a.owner]); },
      function (a) { return { t: a.name, m: a.owner || "" }; },
      function (a) { return { h: "#/delivery/" + encodeURIComponent(a.name) }; });
    var reqs = Array.isArray(d.reqs) ? d.reqs : [];
    take("req", reqs.filter(function (r) { return r.type !== "缺陷"; }), function (r) { return hit(q, [r.name, r.id]); },
      function (r) { return { t: r.name, m: joinMeta([r.id, r.pri]) }; },
      function (r) { return { h: "#/requirements", f: { kind: "demo-req", q: r.name } }; });
    take("bug", reqs.filter(function (r) { return r.type === "缺陷"; }), function (r) { return hit(q, [r.name, r.id]); },
      function (r) { return { t: r.name, m: joinMeta([r.id, r.pri]) }; },
      function (r) { return { h: "#/requirements", f: { kind: "demo-req", q: r.name } }; });
    return out;
  }

  /* ---------------------------------------------------------------- 状态 */
  var S = { host: null, ready: false, q: "", items: [], sel: 0, selKey: "", seq: 0, ctl: null, timer: null, res: {}, errs: [], pending: false, appsCache: null, skipHash: "" };

  function cancelPending() {
    var t = timers();
    if (S.timer) { t.clear(S.timer); S.timer = null; }
    if (S.ctl) { try { S.ctl.abort(); } catch (e) {} S.ctl = null; }
    S.pending = false;
  }
  function isOpen() { return !!(S.host && S.host.el && !S.host.el.hidden); }

  // 输入变化：页面与操作立即更新，资源 300ms 后查询；旧请求取消，过期结果丢弃。
  function search(q) {
    S.q = q;
    S.seq += 1;
    cancelPending();
    S.res = {}; S.errs = [];
    var seq = S.seq;
    if (q) {
      if (live()) {
        var groups = liveGroups();
        if (groups.length) {
          S.pending = true;
          S.timer = timers().set(function () { S.timer = null; runLive(seq, q, groups); }, DEBOUNCE);
        }
      } else {
        S.res = demoResults(low(q));
      }
    }
    draw();
    return seq;
  }
  function runLive(seq, q, groups) {
    if (seq !== S.seq || !isOpen()) return Promise.resolve();
    var ctl = typeof root.AbortController === "function" ? new root.AbortController() : null;
    S.ctl = ctl;
    var sig = ctl ? ctl.signal : undefined;
    var jobs = groups.map(function (g) {
      var p;
      try { p = Promise.resolve(g.fetch(q, sig)); } catch (e) { p = Promise.reject(e); }
      return p.then(function (list) {
        if (seq !== S.seq || !isOpen()) return;
        var rows = list.slice(0, PER_GROUP).map(function (x) { var o = g.row(x); o.target = g.to(x); return o; }).filter(function (o) { return o.t; });
        if (rows.length) S.res[g.k] = rows;
        draw();
      }, function (err) {
        if (seq !== S.seq) return;
        var x = X();
        if (x && typeof x.isCancelled === "function" && x.isCancelled(err)) return;
        if (err && (err.name === "AbortError" || err.status === 403)) return;
        S.errs.push(g.t);
        draw();
      });
    });
    return Promise.all(jobs).then(function () {
      if (seq !== S.seq || !isOpen()) return;
      S.pending = false;
      if (S.ctl === ctl) S.ctl = null;
      draw();
    });
  }

  /* ---------------------------------------------------------------- 结果列表 */
  function build() {
    var q = low(S.q.trim()), out = [];
    function push(g, it) { it.g = g; it.key = it.key || g + "|" + (it.h || "") + "|" + it.t; out.push(it); }
    if (!q) {
      recentRead().forEach(function (r) { push("最近访问", { t: r.t, m: r.m, i: "clock", h: r.h, target: { h: r.h, f: r.f }, recent: true }); });
      actionsFor("").forEach(function (a) { push("常用操作", { t: a.t, m: a.m, i: a.i, h: a.h || "", action: a }); });
      if (!out.length) pagesAll().forEach(function (p) { push("页面", { t: p.t, m: p.m === p.t ? p.g : p.m, i: p.i, h: p.h, target: { h: p.h } }); });
      return out;
    }
    pagesAll().filter(function (p) { return hit(q, [p.t + " " + p.m + " " + p.g]); }).slice(0, MAX_PAGES)
      .forEach(function (p) { push("页面", { t: p.t, m: p.m === p.t ? p.g : p.m, i: p.i, h: p.h, target: { h: p.h } }); });
    actionsFor(q).forEach(function (a) { push("操作", { t: a.t, m: a.m, i: a.i, h: a.h || "", action: a }); });
    GROUPS.forEach(function (g) {
      (S.res[g.k] || []).forEach(function (r) { push(g.t, { t: r.t, m: r.m, i: g.i, dot: r.dot, h: r.target.h, target: r.target, kind: g.k }); });
    });
    return out;
  }

  function draw() {
    var host = S.host;
    S.items = build();
    var idx = -1;
    if (S.selKey) for (var i = 0; i < S.items.length; i++) if (S.items[i].key === S.selKey) { idx = i; break; }
    S.sel = idx >= 0 ? idx : Math.min(Math.max(0, S.sel), Math.max(0, S.items.length - 1));
    S.selKey = S.items[S.sel] ? S.items[S.sel].key : "";
    if (!host || !host.list) return S.items;
    var ic = typeof host.ic === "function" ? host.ic : function () { return ""; };
    var html = "", prev = null;
    S.items.forEach(function (it, i) {
      if (it.g !== prev) { html += '<li class="x-cmdg" role="presentation">' + esc(it.g) + (it.g === "最近访问" ? '<button type="button" class="xck-clear" data-xck-clear>清除</button>' : "") + "</li>"; prev = it.g; }
      var sel = i === S.sel;
      html += '<li><a href="' + esc(it.h || "#") + '" role="option" id="xck-' + i + '" data-ix="' + i + '" aria-selected="' + sel + '"' + (sel ? ' class="sel"' : "") + ">" +
        ic(it.i) + (it.dot ? '<i class="xck-dot" style="background:' + esc(it.dot) + '" aria-hidden="true"></i>' : "") +
        '<span class="xck-t">' + esc(it.t) + "</span>" + (it.m ? "<small>" + esc(it.m) + "</small>" : "") + "</a></li>";
    });
    var q = S.q.trim();
    if (q && S.pending) html += '<li class="xck-note" role="presentation">正在搜索资源…</li>';
    else if (q && !S.items.length) html += '<li class="x-subnone" role="presentation">没有匹配的页面或资源</li>';
    if (q && !S.pending && S.errs.length) html += '<li class="xck-note" role="presentation">' + esc(S.errs.join("、")) + "暂时无法搜索</li>";
    if (!q && !S.items.length) html = '<li class="x-subnone" role="presentation">暂无可用页面</li>';
    host.list.innerHTML = html;
    if (host.input && host.input.setAttribute) host.input.setAttribute("aria-activedescendant", S.items.length ? "xck-" + S.sel : "");
    var a = host.list.querySelector && host.list.querySelector(".sel");
    if (a && a.scrollIntoView) a.scrollIntoView({ block: "nearest" });
    return S.items;
  }

  function move(d) {
    if (!S.items.length) return;
    S.sel = (S.sel + d + S.items.length) % S.items.length;
    S.selKey = S.items[S.sel].key;
    draw();
  }

  /* ---------------------------------------------------------------- 打开结果 */
  function viewEl() { return root.document && root.document.getElementById ? root.document.getElementById("view") : null; }
  // 跳转后等页面（含异步加载）渲染出目标元素再执行；用户中途离开则放弃
  function follow(hash, find, act, onGiveUp) {
    // 首次检查也放到定时器里：hashchange 触发的重新渲染之后再找，避免命中旧页面上的元素
    var tries = 0, t = timers();
    function step() {
      if (baseHash(root.location && root.location.hash) !== baseHash(hash)) return;
      var v = viewEl(), el = v && find(v);
      if (el && el.isConnected !== false) { try { act(el, v); } catch (e) { if (root.console) root.console.warn("[XSCmdk]", e); } return; }
      if (++tries < FOLLOW_TRIES) t.set(step, FOLLOW_MS);
      else if (onGiveUp) onGiveUp();
    }
    t.set(step, FOLLOW_MS);
  }
  var FILTER_SEL = ["[data-live-q]", "[data-app-search]", "[data-wsp-search]", "[data-q]", "[data-bq]", "#q"];
  function findFilter(v) { for (var i = 0; i < FILTER_SEL.length; i++) { var el = v.querySelector(FILTER_SEL[i]); if (el) return el; } return null; }
  function applyFilter(input, q) {
    input.value = q;
    var E = root.Event, K = root.KeyboardEvent;
    try { input.dispatchEvent(new E("input", { bubbles: true })); } catch (e) {}
    // 研发中心列表按回车提交关键字
    if (input.hasAttribute && input.hasAttribute("data-q")) {
      try { input.dispatchEvent(K ? new K("keydown", { key: "Enter", bubbles: true }) : new E("keydown", { bubbles: true })); } catch (e) {}
    }
  }
  function afterNav(target) {
    var f = target.f;
    if (!f) return;
    var h = target.h;
    if (f.kind === "ws" && f.open) {
      return follow(h, function (v) { return v.querySelector('[data-wsp-click="open"][data-id="' + f.open.replace(/[^\w-]/g, "") + '"]'); }, function (b) { b.click(); });
    }
    if ((f.kind === "req" || f.kind === "bug") && f.open && root.XSDev && typeof root.XSDev.openDetail === "function") {
      // 等列表渲染完（render 会先关掉旧抽屉）再打开详情
      return follow(h, function (v) { return v.querySelector("[data-q]"); }, function () { root.XSDev.openDetail(f.kind === "bug" ? "bugs" : "requirements", f.open); });
    }
    if (f.q) follow(h, findFilter, function (input) { applyFilter(input, f.q); });
  }
  function navTo(target, skipRecent) {
    var h = safeHash(target && target.h);
    if (!h || !S.host) return;
    S.skipHash = skipRecent && baseHash(root.location && root.location.hash) !== baseHash(h) ? baseHash(h) : "";
    S.host.navGo(h);
    afterNav({ h: h, f: target.f });
  }
  function runAction(a) {
    if (a.run) { if (S.host && S.host.closeAll) S.host.closeAll(); return a.run(); }
    navTo({ h: a.h });
    follow(a.h, function (v) {
      for (var i = 0; i < a.clicks.length; i++) { var b = v.querySelector(a.clicks[i]); if (b && !b.disabled) return b; }
      return null;
    }, function (b) { b.click(); }, function () {
      var x = X(); if (x && typeof x.toast === "function") x.toast("未能自动打开「" + a.t + "」，请在页面上点击对应按钮");
    });
  }
  function choose(it) {
    if (!it) return;
    cancelPending();
    if (it.action) return runAction(it.action);
    var res = !!(it.kind || (it.recent && it.target.f));
    if (it.kind) recentPush({ h: it.target.h, t: (GROUP_NAME[it.kind] || "") + " " + it.t, m: it.m, f: it.target.f });
    else if (res) recentPush({ h: it.target.h, t: it.t, m: it.m, f: it.target.f });
    navTo(it.target, res);
  }
  var GROUP_NAME = {}; GROUPS.forEach(function (g) { GROUP_NAME[g.k] = g.t; });

  /* ---------------------------------------------------------------- 接线 */
  function css() {
    var d = root.document;
    if (!d || !d.getElementById || d.getElementById("xck-css")) return;
    var s = d.createElement("style");
    s.id = "xck-css";
    s.textContent = ".x-cmdl .xck-t{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}" +
      ".x-cmdl small{max-width:50%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}" +
      ".xck-dot{flex:none;width:8px;height:8px;border-radius:50%;margin-left:-4px}" +
      ".xck-note{padding:8px 10px;font-size:12px;color:var(--x-sub,#6b7280)}" +
      ".x-cmdg{display:flex;align-items:center}.xck-clear{margin-left:auto;border:0;background:none;padding:0 2px;font-size:12px;color:var(--x-sub,#6b7280);cursor:pointer}.xck-clear:hover{color:var(--x-ink,#1677ff)}";
    (d.head || d.body).appendChild(s);
  }
  function isMac() { var n = root.navigator || {}; return /Mac|iPhone|iPad/i.test(String(n.platform || n.userAgent || "")); }

  function onHash() {
    var h = baseHash(root.location && root.location.hash);
    if (!h || h === "#/" || /^#\/login\b/.test(h)) return;
    // 从资源结果跳转时已记下资源本身，不再重复记录它所在的列表页
    if (S.skipHash) { var skip = S.skipHash === h; S.skipHash = ""; if (skip) return; }
    var t = titleFor(h);
    if (t) recentPush({ h: h, t: t.t, m: t.m });
  }

  function attach(host) {
    if (!host || !host.el || !host.input || !host.list || typeof host.navGo !== "function") return false;
    S.host = host;
    css();
    var input = host.input, list = host.list;
    input.placeholder = "搜索页面、资源或操作…";
    input.setAttribute("aria-label", "全局搜索");
    input.setAttribute("autocomplete", "off");
    list.setAttribute("aria-label", "搜索结果");
    host.el.setAttribute("aria-label", "全局搜索");
    var kbtn = root.document && root.document.getElementById && root.document.getElementById("kbtn");
    if (kbtn) {
      var kbd = kbtn.querySelector("kbd"), label = isMac() ? "⌘K" : "Ctrl K";
      if (kbd) kbd.textContent = label;
      var span = kbtn.querySelector("span"); if (span) span.textContent = "搜索";
      kbtn.setAttribute("aria-label", "全局搜索（" + (isMac() ? "⌘K" : "Ctrl+K") + "）");
    }
    // 覆盖主脚本的属性处理器（不改其打开 / 关闭逻辑）
    input.oninput = function () { S.sel = 0; S.selKey = ""; search(String(input.value || "").trim()); };
    input.onkeydown = function (e) {
      if (e.isComposing) return;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") { e.preventDefault(); move(e.key === "ArrowDown" ? 1 : -1); }
      else if (e.key === "Enter") { e.preventDefault(); choose(S.items[S.sel]); }
      else if (e.key === "Escape") { cancelPending(); }
    };
    list.onclick = function (e) {
      var clr = e.target && e.target.closest && e.target.closest("[data-xck-clear]");
      if (clr) { e.preventDefault(); recentClear(); S.sel = 0; S.selKey = ""; draw(); input.focus(); return; }
      var a = e.target && e.target.closest && e.target.closest("a[data-ix]");
      if (!a) return;
      e.preventDefault();
      choose(S.items[+a.getAttribute("data-ix")]);
    };
    if (root.addEventListener) root.addEventListener("hashchange", onHash);
    // 面板被关闭（Esc、遮罩、跳转）时取消进行中的请求，迟到的结果直接丢弃
    if (typeof root.MutationObserver === "function") {
      try { new root.MutationObserver(function () { if (host.el.hidden) { cancelPending(); S.seq += 1; } }).observe(host.el, { attributes: true, attributeFilter: ["hidden"] }); } catch (e) {}
    }
    S.ready = true;
    XSCmdk.ready = true;
    onHash();
    return true;
  }

  // 主脚本 openCmdk() → drawCmdk() 时调用：重置状态并画空输入视图
  function onOpen() {
    cancelPending();
    S.seq += 1;
    S.q = ""; S.sel = 0; S.selKey = ""; S.res = {}; S.errs = []; S.appsCache = null;
    if (S.host && S.host.input) S.host.input.value = "";
    return draw();
  }

  var XSCmdk = {
    ready: false,
    attach: attach,
    onOpen: onOpen,
    // 测试与调试用
    _t: { S: S, search: search, runLive: runLive, build: build, draw: draw, move: move, choose: choose, recentRead: recentRead, recentPush: recentPush, recentClear: recentClear,
      actionsFor: actionsFor, titleFor: titleFor, demoResults: demoResults, onHash: onHash, statusDot: statusDot, safeHash: safeHash, applyFilter: applyFilter, ACTIONS: ACTIONS, GROUPS: GROUPS, isOpen: isOpen }
  };
  root.XSCmdk = XSCmdk;
  if (root.XSCmdkHost) attach(root.XSCmdkHost);
})(typeof window !== "undefined" ? window : this);
