/* 平台管理 → 企业空间（KubeSphere 式：企业空间 → 项目 → 成员）。调用 v1.30 企业空间接口契约：
 *   空间        GET/POST /workspaces；GET/PUT/DELETE /workspaces/:id（删除 body {confirm_name}）
 *   成员        GET/POST /workspaces/:id/members {user_id|group_id, role}；PUT/DELETE /workspaces/:id/members/:mid
 *   项目        GET/POST /workspaces/:id/projects {cluster_id, namespace}；DELETE /workspaces/:id/projects/:pid
 *   角色绑定    GET/POST /role-bindings {role_id, user_id|group_id, scope, scope_value}；DELETE /role-bindings/:id
 *   引用数据    GET /users、GET /user-groups、GET /roles、GET /clusters、GET /clusters/:id/namespaces（均为现有接口）
 * 权限：读 workspace:read，写 workspace:manage（无写权限时隐藏全部写操作）。
 * 挂载：window.XSWorkspace.render(el)
 */
(function (root) {
  "use strict";
  // emptyHTML：统一空状态（XSEmpty）；未加载 xs-empty.js 时回退为原来的一行文字 fallbackHTML。
  function emptyHTML(o, fallbackHTML) {
    var X = root.XSEmpty;
    return typeof X !== "undefined" && X && typeof X.html === "function" ? X.html(o) : fallbackHTML;
  }
  var ROLES = [["owner", "所有者"], ["admin", "管理员"], ["member", "成员"], ["viewer", "只读"]];
  var ROLE_TEXT = { owner: "所有者", admin: "管理员", member: "成员", viewer: "只读" };
  var SCOPE_TEXT = { global: "全平台", env: "环境", cluster: "集群", workspace: "本空间", project: "项目" };
  var TABS = [["members", "成员"], ["projects", "项目"], ["bindings", "角色绑定"]];
  var NAME_MAX = 63, DESC_MAX = 256;
  var GIB = 1024 * 1024 * 1024;

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function api(path, options) { return X().api(path, options || {}); }
  function can(p) { var x = X(); return !!(x.can && x.can(p)); }
  function toast(msg) { var x = X(); if (x.toast) x.toast(msg); else if (typeof root.toast === "function") root.toast(msg); }
  function errMsg(e) {
    var s = e && Number(e.status);
    if (e && e.message && !/^HTTP \d+|^请求失败（HTTP|^Request failed|Failed to fetch/i.test(e.message)) return e.message;
    if (s === 403) return "没有权限执行此操作";
    if (s === 404) return "企业空间或记录不存在，可能已被删除";
    if (s === 409) return "名称已存在，或该命名空间已归属其他企业空间";
    if (s === 400) return "填写的内容不符合要求，请检查后重试";
    return "服务暂不可用，请稍后重试";
  }
  function listOf(d) { return Array.isArray(d) ? d : (d && Array.isArray(d.items) ? d.items : []); }
  function soft(p) { return p.then(function (d) { return { ok: true, items: listOf(d) }; }, function (e) { return { ok: false, items: [], error: errMsg(e) }; }); }
  function same(a, b) { return a != null && b != null && String(a) === String(b); }
  function pick(list, id) { for (var i = 0; i < list.length; i++) if (same(list[i].id, id)) return list[i]; return null; }
  function num(v) { var n = Number(v); return isFinite(n) ? n : 0; }

  function fmtCPU(milli) { milli = num(milli); if (milli <= 0) return ""; var c = milli / 1000; return (Math.round(c * 100) / 100) + " 核"; }
  function fmtMem(bytes) {
    bytes = num(bytes); if (bytes <= 0) return "";
    if (bytes >= GIB) return (Math.round(bytes / GIB * 100) / 100) + " GiB";
    return Math.round(bytes / 1024 / 1024) + " MiB";
  }
  function fmtQuota(w) {
    var c = fmtCPU(w.cpu_limit_milli), m = fmtMem(w.memory_limit_bytes);
    if (!c && !m) return "不限制";
    return "CPU " + (c || "不限") + " · 内存 " + (m || "不限");
  }
  function userLabel(u) { return u ? (u.display_name && u.display_name !== u.username ? u.display_name + "（" + u.username + "）" : (u.username || u.display_name || "")) : ""; }

  // 表单校验：返回 { errors:{field:msg}, body }。可单测。
  function validateWorkspace(f, items, editingId) {
    var errors = {}, name = String(f.name || "").trim(), desc = String(f.description || "").trim();
    if (!name) errors.name = "请输入空间名称";
    else if (name.length > NAME_MAX) errors.name = "名称不能超过 " + NAME_MAX + " 个字符";
    else if ((items || []).some(function (w) { return !same(w.id, editingId) && String(w.name || "").toLowerCase() === name.toLowerCase(); })) errors.name = "已存在同名企业空间";
    if (desc.length > DESC_MAX) errors.description = "描述不能超过 " + DESC_MAX + " 个字符";
    var cpu = String(f.cpu == null ? "" : f.cpu).trim(), mem = String(f.memory == null ? "" : f.memory).trim();
    var cpuN = cpu === "" ? 0 : Number(cpu), memN = mem === "" ? 0 : Number(mem);
    if (!(cpuN >= 0) || !isFinite(cpuN)) errors.cpu = "请输入不小于 0 的数字，留空表示不限制";
    else if (cpuN > 0 && Math.round(cpuN * 1000) < 1) errors.cpu = "CPU 最小为 0.001 核";
    if (!(memN >= 0) || !isFinite(memN)) errors.memory = "请输入不小于 0 的数字，留空表示不限制";
    var body = {
      name: name, description: desc,
      manager_id: f.manager_id ? Number(f.manager_id) : null,
      cpu_limit_milli: errors.cpu ? 0 : Math.round(cpuN * 1000),
      memory_limit_bytes: errors.memory ? 0 : Math.round(memN * GIB)
    };
    return { errors: errors, body: body };
  }

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-wsp-css")) return;
    var s = doc.createElement("style"); s.id = "xs-wsp-css";
    s.textContent = [
      ".xws-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:24px 28px;margin-bottom:20px}",
      ".xws-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:16px}.xws-head h3{margin:0;font-size:16px}",
      ".xws-acts{display:flex;gap:8px;align-items:center}.xws-acts .x-input{height:32px;width:220px}",
      ".xws-sub{font-size:13px;color:var(--x-sub,#6b7280);margin:4px 0 0}",
      ".xws-table{width:100%;border-collapse:collapse;font-size:14px}",
      ".xws-table th,.xws-table td{padding:11px 10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:middle}",
      ".xws-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xws-muted{color:var(--x-sub,#6b7280);font-size:13px}",
      ".xws-tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid #bfdbfe;color:#1d4ed8;background:#eff6ff}",
      ".xws-tag--grey{border-color:#e5e7eb;color:#4b5563;background:#f9fafb}.xws-tag--bad{border-color:#fecaca;color:#b91c1c;background:#fef2f2}",
      ".xws-err{color:#b91c1c;font-size:13px;margin:4px 0 0}.xws-empty{padding:28px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xws-link{border:0;background:none;padding:0 8px 0 0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:14px}",
      ".xws-link--danger{color:#b91c1c}.xws-link[disabled]{color:#9ca3af;cursor:not-allowed}",
      ".xws-tabs{display:flex;gap:4px;border-bottom:1px solid var(--x-line,#e6e9ed);margin-bottom:16px}",
      ".xws-tab{border:0;background:none;padding:10px 16px;font-size:14px;cursor:pointer;color:var(--x-sub,#4b5563);border-bottom:2px solid transparent;margin-bottom:-1px}",
      ".xws-tab--on{color:var(--x-blue,#2563eb);border-bottom-color:var(--x-blue,#2563eb);font-weight:500}",
      ".xws-meta{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px 24px;font-size:14px}.xws-meta span{display:block;color:var(--x-sub,#6b7280);font-size:12px;margin-bottom:2px}",
      ".xws-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:center;justify-content:center;z-index:1000}",
      ".xws-modal{background:#fff;border-radius:10px;width:520px;max-width:calc(100vw - 32px);max-height:calc(100vh - 48px);overflow:auto;padding:24px}",
      ".xws-modal h3{margin:0 0 14px;font-size:16px}",
      ".xws-warn{background:#fef2f2;border:1px solid #fecaca;color:#991b1b;border-radius:6px;padding:10px 12px;font-size:13px;line-height:1.6;margin-bottom:14px}",
      ".xws-field{margin-bottom:14px}.xws-field label{display:block;font-size:13px;color:var(--x-sub,#4b5563);margin-bottom:6px}",
      ".xws-field .x-input{width:100%;height:36px;box-sizing:border-box}.xws-field textarea.x-input{height:72px;padding-top:8px}",
      ".xws-two{display:grid;grid-template-columns:1fr 1fr;gap:12px}",
      ".xws-field--bad .x-input{border-color:#d43030}",
      ".xws-foot{display:flex;justify-content:flex-end;gap:10px;margin-top:18px}",
      ".xws .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}.xws .x-btn--danger:hover{background:#b92424}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  function render(el) {
    if (!el) return;
    css();
    var serial = (el._xsWspSerial || 0) + 1; el._xsWspSerial = serial;
    if (X().mode === "demo") {
      el.innerHTML = '<div class="xws" data-wsp><section class="xws-card"><div class="xws-head"><h3>企业空间</h3></div><p class="xws-empty" data-wsp-demo>演示模式下不提供企业空间，连接平台服务后即可使用。</p></section></div>';
      return Promise.resolve();
    }
    var canManage = can("workspace:manage");
    var st = {
      view: "list", q: "", items: [], loading: true, error: "",
      users: [], groups: [], roles: [], clusters: [], refErr: {},
      ws: null, tab: "members", detail: { loading: false, error: "", members: [], projects: [], bindings: [], bindErr: "" },
      modal: null, nsCache: {}
    };
    function alive() { return el._xsWspSerial === serial && el.isConnected !== false; }

    // ------------------------------------------------------------ data
    function loadRefs() {
      return Promise.all([
        soft(api("/users", { query: { page: 1, page_size: 500 } })),
        soft(api("/user-groups")),
        soft(api("/roles")),
        soft(api("/clusters", { query: { page_size: 200 } }))
      ]).then(function (r) {
        st.users = r[0].items; st.groups = r[1].items; st.roles = r[2].items; st.clusters = r[3].items;
        st.refErr = { users: r[0].error || "", groups: r[1].error || "", roles: r[2].error || "", clusters: r[3].error || "" };
      });
    }
    function loadList(quiet) {
      if (!quiet) { st.loading = true; paint(); }
      return api("/workspaces").then(function (d) {
        if (!alive()) return;
        st.items = listOf(d); st.error = ""; st.loading = false;
        if (st.ws) { var fresh = pick(st.items, st.ws.id); if (fresh) st.ws = fresh; }
        paint();
      }, function (e) {
        if (!alive()) return;
        st.loading = false; st.error = errMsg(e); paint();
      });
    }
    function loadDetail(quiet) {
      var ws = st.ws; if (!ws) return Promise.resolve();
      var D = st.detail;
      if (!quiet) { D.loading = true; paint(); }
      var base = "/workspaces/" + encodeURIComponent(ws.id);
      return Promise.all([
        api(base),
        api(base + "/members"),
        api(base + "/projects"),
        soft(api("/role-bindings?scope=workspace&scope_value=" + encodeURIComponent(ws.id) + "&page_size=200")),
        soft(api("/role-bindings?scope=project&page_size=200"))
      ]).then(function (r) {
        r[3] = { ok: r[3].ok && r[4].ok, error: r[3].error || r[4].error, items: (r[3].items || []).concat(r[4].items || []) };
        if (!alive() || !st.ws || !same(st.ws.id, ws.id)) return;
        if (r[0] && typeof r[0] === "object" && !Array.isArray(r[0]) && r[0].id != null) st.ws = r[0];
        D.members = listOf(r[1]); D.projects = listOf(r[2]);
        D.bindings = r[3].items.filter(function (b) { return bindingInWorkspace(b, ws.id, D.projects); });
        D.bindErr = r[3].ok ? "" : r[3].error;
        D.loading = false; D.error = ""; paint();
      }, function (e) {
        if (!alive()) return;
        D.loading = false; D.error = errMsg(e); paint();
      });
    }
    function loadNamespaces(clusterId) {
      if (!clusterId) return Promise.resolve();
      var c = st.nsCache[clusterId];
      if (c && (c.loading || c.ok)) return Promise.resolve();
      st.nsCache[clusterId] = { loading: true, items: [], error: "" }; paint();
      return api("/clusters/" + encodeURIComponent(clusterId) + "/namespaces").then(function (d) {
        st.nsCache[clusterId] = { ok: true, items: listOf(d).map(function (n) { return n.name || n; }), error: "" };
        if (alive()) paint();
      }, function (e) {
        st.nsCache[clusterId] = { ok: false, items: [], error: "命名空间加载失败：" + errMsg(e) };
        if (alive()) paint();
      });
    }

    // ------------------------------------------------------------ name lookups
    function userName(id, row) {
      if (row && (row.display_name || row.username)) return userLabel(row);
      var u = pick(st.users, id); return u ? userLabel(u) : "用户 #" + id;
    }
    function groupName(id, row) {
      if (row && row.group_name) return row.group_name;
      var g = pick(st.groups, id); return g ? g.name : "用户组 #" + id;
    }
    function subjectHTML(row) {
      if (row.user_id != null) return '<span class="xws-tag xws-tag--grey">用户</span> ' + esc(userName(row.user_id, row));
      return '<span class="xws-tag xws-tag--grey">用户组</span> ' + esc(groupName(row.group_id, row));
    }
    function clusterName(id, row) {
      if (row && row.cluster_name) return row.cluster_name;
      var c = pick(st.clusters, id); return c ? c.name : "";
    }
    function managerName(w) {
      if (w.manager_name) return w.manager_name;
      if (w.manager_id == null) return "";
      var u = pick(st.users, w.manager_id); return u ? userLabel(u) : "用户 #" + w.manager_id;
    }
    function roleName(id, row) {
      if (row && row.role_name) return row.role_name;
      var r = pick(st.roles, id); return r ? r.name : "角色 #" + id;
    }
    function scopeText(b) {
      if (b.scope === "workspace") return "本空间";
      if (b.scope === "project") {
        var parts = String(b.scope_value || "").split("/");
        var cn = clusterName(parts[0]);
        return "项目 " + (cn || "集群 #" + parts[0]) + " / " + (parts.slice(1).join("/") || "");
      }
      return SCOPE_TEXT[b.scope] || b.scope || "—";
    }

    // ------------------------------------------------------------ views
    function fieldHTML(key, label, control, err, hint) {
      return '<div class="xws-field' + (err ? " xws-field--bad" : "") + '" data-wsp-field="' + key + '"><label>' + esc(label) + "</label>" + control +
        (err ? '<p class="xws-err" data-wsp-ferr="' + key + '">' + esc(err) + "</p>" : (hint ? '<p class="xws-sub">' + esc(hint) + "</p>" : "")) + "</div>";
    }
    function opts(list, cur, empty) {
      return (empty != null ? '<option value="">' + esc(empty) + "</option>" : "") + list.map(function (o) {
        return '<option value="' + esc(o[0]) + '"' + (same(o[0], cur) ? " selected" : "") + (o[2] ? " disabled" : "") + ">" + esc(o[1]) + "</option>";
      }).join("");
    }

    function listView() {
      var head = '<div class="xws-head"><div><h3>企业空间</h3><p class="xws-sub">企业空间把多个集群中的命名空间作为项目统一管理，并为成员分配空间内的角色。</p></div><div class="xws-acts">' +
        '<input class="x-input" data-wsp-search placeholder="按名称或描述搜索" value="' + esc(st.q) + '">' +
        '<button type="button" class="x-btn" data-wsp-click="reload">刷新</button>' +
        (canManage ? '<button type="button" class="x-btn x-btn--pri" data-wsp-click="new">新建企业空间</button>' : "") + "</div></div>";
      if (st.loading && !st.items.length) return '<section class="xws-card">' + head + '<p class="xws-empty">正在加载…</p></section>';
      if (st.error) return '<section class="xws-card">' + head + '<p class="xws-err" data-wsp-error>' + esc(st.error) + "</p></section>";
      var q = st.q.trim().toLowerCase();
      var rows = st.items.filter(function (w) { return !q || String(w.name || "").toLowerCase().indexOf(q) >= 0 || String(w.description || "").toLowerCase().indexOf(q) >= 0; });
      if (!st.items.length) return '<section class="xws-card">' + head + emptyHTML({ kind: "workspace", title: "暂无企业空间",
        desc: canManage ? "企业空间把多个集群中的命名空间作为项目统一管理，先新建一个空间吧。" : "需要企业空间管理权限才能新建，请联系管理员。",
        actions: canManage ? [{ text: "新建企业空间", act: "new", primary: true, attrs: { "data-wsp-click": "new" } }] : [] },
        '<p class="xws-empty">暂无企业空间' + (canManage ? "，可点击「新建企业空间」创建" : "") + "</p>") + "</section>";
      if (!rows.length) return '<section class="xws-card">' + head + emptyHTML({ kind: "search", title: "没有匹配的企业空间", desc: "换个关键词，或清除搜索查看全部企业空间。",
        actions: [{ text: "清除搜索", act: "clearSearch", attrs: { "data-wsp-click": "clearSearch" } }] }, '<p class="xws-empty">没有匹配的企业空间</p>') + "</section>";
      return '<section class="xws-card">' + head +
        '<table class="xws-table"><thead><tr><th>名称</th><th>描述</th><th>管理员</th><th>成员数</th><th>项目数</th><th>配额</th><th style="width:150px">操作</th></tr></thead><tbody>' +
        rows.map(function (w) {
          var acts = '<button type="button" class="xws-link" data-wsp-click="open" data-id="' + esc(w.id) + '">详情</button>';
          if (canManage) acts += '<button type="button" class="xws-link" data-wsp-click="edit" data-id="' + esc(w.id) + '">编辑</button>' +
            '<button type="button" class="xws-link xws-link--danger" data-wsp-click="del" data-id="' + esc(w.id) + '">删除</button>';
          return '<tr data-wsp-row="' + esc(w.id) + '"><td><button type="button" class="xws-link" data-wsp-click="open" data-id="' + esc(w.id) + '"><b>' + esc(w.name) + "</b></button></td>" +
            '<td class="xws-muted">' + esc(w.description || "—") + "</td>" +
            "<td>" + esc(managerName(w) || "—") + "</td>" +
            "<td>" + esc(w.member_count != null ? w.member_count : "—") + "</td>" +
            "<td>" + esc(w.project_count != null ? w.project_count : "—") + "</td>" +
            "<td>" + esc(fmtQuota(w)) + "</td><td>" + acts + "</td></tr>";
        }).join("") + "</tbody></table>" +
        '<p class="xws-sub" style="text-align:right;margin-top:12px">共 ' + esc(rows.length) + " 个</p></section>";
    }

    function membersHTML() {
      var D = st.detail;
      var head = '<div class="xws-head"><h3>成员</h3>' + (canManage ? '<button type="button" class="x-btn x-btn--pri" data-wsp-click="addMember">添加成员</button>' : "") + "</div>";
      if (!D.members.length) return head + emptyHTML({ kind: "member", size: "s", title: "暂无成员", desc: canManage ? "添加用户或用户组，并分配空间内的角色。" : "",
        actions: canManage ? [{ text: "添加成员", act: "addMember", primary: true, attrs: { "data-wsp-click": "addMember" } }] : [] }, '<p class="xws-empty">暂无成员</p>');
      return head + '<table class="xws-table"><thead><tr><th>成员</th><th style="width:180px">空间角色</th><th>加入时间</th>' + (canManage ? '<th style="width:90px">操作</th>' : "") + "</tr></thead><tbody>" +
        D.members.map(function (m) {
          var role = canManage
            ? '<select class="x-input" style="height:30px" data-wsp-role="' + esc(m.id) + '">' + opts(ROLES, m.role) + "</select>"
            : esc(ROLE_TEXT[m.role] || m.role);
          return '<tr data-wsp-member="' + esc(m.id) + '"><td>' + subjectHTML(m) + "</td><td>" + role + '</td><td class="xws-muted">' + esc(fmtTime(m.created_at)) + "</td>" +
            (canManage ? '<td><button type="button" class="xws-link xws-link--danger" data-wsp-click="rmMember" data-id="' + esc(m.id) + '">移除</button></td>' : "") + "</tr>";
        }).join("") + "</tbody></table>";
    }
    function projectsHTML() {
      var D = st.detail;
      var head = '<div class="xws-head"><h3>项目</h3>' + (canManage ? '<button type="button" class="x-btn x-btn--pri" data-wsp-click="addProject">绑定项目</button>' : "") + "</div>";
      if (!D.projects.length) return head + emptyHTML({ kind: "cluster", size: "s", title: "暂无项目", desc: canManage ? "可把集群中的命名空间绑定到本空间。" : "",
        actions: canManage ? [{ text: "绑定项目", act: "addProject", primary: true, attrs: { "data-wsp-click": "addProject" } }] : [] },
        '<p class="xws-empty">暂无项目' + (canManage ? "，可把集群中的命名空间绑定到本空间" : "") + "</p>");
      return head + '<table class="xws-table"><thead><tr><th>集群</th><th>命名空间</th><th>绑定时间</th>' + (canManage ? '<th style="width:90px">操作</th>' : "") + "</tr></thead><tbody>" +
        D.projects.map(function (p) {
          var cn = clusterName(p.cluster_id, p);
          return '<tr data-wsp-project="' + esc(p.id) + '"><td>' + (cn ? esc(cn) : '<span class="xws-tag xws-tag--bad">集群不存在</span>') + "</td><td>" + esc(p.namespace) + '</td><td class="xws-muted">' + esc(fmtTime(p.created_at)) + "</td>" +
            (canManage ? '<td><button type="button" class="xws-link xws-link--danger" data-wsp-click="rmProject" data-id="' + esc(p.id) + '">解绑</button></td>' : "") + "</tr>";
        }).join("") + "</tbody></table>";
    }
    function bindingsHTML() {
      var D = st.detail;
      var head = '<div class="xws-head"><div><h3>角色绑定</h3><p class="xws-sub">把平台角色限定在本空间或空间内某个项目范围内授予用户或用户组。</p></div>' +
        (canManage && !D.bindErr ? '<button type="button" class="x-btn x-btn--pri" data-wsp-click="addBinding">添加绑定</button>' : "") + "</div>";
      if (D.bindErr) return head + '<p class="xws-err" data-wsp-binderr>' + esc(D.bindErr) + "</p>";
      if (!D.bindings.length) return head + emptyHTML({ kind: "member", size: "s", title: "暂无范围化角色绑定", desc: canManage ? "把平台角色限定在本空间或其中某个项目内授予用户或用户组。" : "",
        actions: canManage ? [{ text: "添加绑定", act: "addBinding", primary: true, attrs: { "data-wsp-click": "addBinding" } }] : [] }, '<p class="xws-empty">暂无范围化角色绑定</p>');
      return head + '<table class="xws-table"><thead><tr><th>授权对象</th><th>角色</th><th>范围</th><th>创建时间</th>' + (canManage ? '<th style="width:90px">操作</th>' : "") + "</tr></thead><tbody>" +
        D.bindings.map(function (b) {
          return '<tr data-wsp-binding="' + esc(b.id) + '"><td>' + subjectHTML(b) + "</td><td>" + esc(roleName(b.role_id, b)) + "</td><td>" + esc(scopeText(b)) + '</td><td class="xws-muted">' + esc(fmtTime(b.created_at)) + "</td>" +
            (canManage ? '<td><button type="button" class="xws-link xws-link--danger" data-wsp-click="rmBinding" data-id="' + esc(b.id) + '">删除</button></td>' : "") + "</tr>";
        }).join("") + "</tbody></table>";
    }

    function detailView() {
      var w = st.ws, D = st.detail;
      var head = '<section class="xws-card"><div class="xws-head"><div><button type="button" class="xws-link" data-wsp-click="back">← 返回企业空间列表</button>' +
        '<h3 style="margin-top:8px">' + esc(w.name) + "</h3>" + (w.description ? '<p class="xws-sub">' + esc(w.description) + "</p>" : "") + "</div>" +
        '<div class="xws-acts"><button type="button" class="x-btn" data-wsp-click="reloadDetail">刷新</button>' +
        (canManage ? '<button type="button" class="x-btn" data-wsp-click="edit" data-id="' + esc(w.id) + '">编辑</button><button type="button" class="x-btn x-btn--danger" data-wsp-click="del" data-id="' + esc(w.id) + '">删除</button>' : "") + "</div></div>" +
        '<div class="xws-meta"><div><span>管理员</span>' + esc(managerName(w) || "—") + "</div><div><span>成员数</span>" + esc(D.loading ? "…" : D.members.length) +
        "</div><div><span>项目数</span>" + esc(D.loading ? "…" : D.projects.length) + "</div><div><span>配额</span>" + esc(fmtQuota(w)) + "</div></div></section>";
      var body;
      if (D.loading && !D.members.length && !D.projects.length) body = '<p class="xws-empty">正在加载…</p>';
      else if (D.error) body = '<p class="xws-err" data-wsp-detail-error>' + esc(D.error) + "</p>";
      else body = st.tab === "projects" ? projectsHTML() : st.tab === "bindings" ? bindingsHTML() : membersHTML();
      var tabs = '<div class="xws-tabs">' + TABS.map(function (t) {
        return '<button type="button" class="xws-tab' + (st.tab === t[0] ? " xws-tab--on" : "") + '" data-wsp-click="tab" data-tab="' + t[0] + '">' + t[1] + "</button>";
      }).join("") + "</div>";
      return head + '<section class="xws-card">' + tabs + body + "</section>";
    }

    // ------------------------------------------------------------ modals
    function subjectFields(m) {
      var f = m.form, E = m.errors || {};
      var isGroup = f.kind === "group";
      var list = isGroup ? st.groups.map(function (g) { return [g.id, g.name]; }) : st.users.map(function (u) { return [u.id, userLabel(u)]; });
      var refErr = isGroup ? st.refErr.groups : st.refErr.users;
      return fieldHTML("kind", "授权对象类型", '<select class="x-input" data-wsf="kind">' + opts([["user", "用户"], ["group", "用户组"]], f.kind) + "</select>") +
        fieldHTML("subject", isGroup ? "用户组" : "用户", '<select class="x-input" data-wsf="subject">' + opts(list, f.subject, list.length ? "请选择" : "暂无可选项") + "</select>",
          E.subject || (refErr ? (isGroup ? "用户组" : "用户") + "列表加载失败：" + refErr : ""));
    }
    function modalHTML() {
      var m = st.modal; if (!m) return "";
      var f = m.form || {}, E = m.errors || {}, title = "", body = "", okText = "确定", danger = false;
      if (m.type === "ws") {
        title = m.id ? "编辑企业空间" : "新建企业空间"; okText = m.id ? "保存" : "创建";
        var users = st.users.map(function (u) { return [u.id, userLabel(u)]; });
        body = fieldHTML("name", "空间名称", '<input class="x-input" data-wsf="name" maxlength="' + NAME_MAX + '" autocomplete="off" value="' + esc(f.name) + '">', E.name) +
          fieldHTML("description", "描述", '<textarea class="x-input" data-wsf="description" maxlength="' + DESC_MAX + '">' + esc(f.description) + "</textarea>", E.description) +
          fieldHTML("manager_id", "空间管理员", '<select class="x-input" data-wsf="manager_id">' + opts(users, f.manager_id, "暂不指定") + "</select>",
            st.refErr.users ? "用户列表加载失败：" + st.refErr.users : "", "仅用于展示负责人；空间内的管理权限以成员角色为准") +
          '<div class="xws-two">' +
          fieldHTML("cpu", "CPU 上限（核）", '<input class="x-input" data-wsf="cpu" inputmode="decimal" placeholder="不限制" value="' + esc(f.cpu) + '">', E.cpu) +
          fieldHTML("memory", "内存上限（GiB）", '<input class="x-input" data-wsf="memory" inputmode="decimal" placeholder="不限制" value="' + esc(f.memory) + '">', E.memory) + "</div>" +
          '<p class="xws-sub">配额为整个空间所有项目的总上限，留空或填 0 表示不限制。</p>';
      } else if (m.type === "delWs") {
        title = "删除企业空间"; okText = "确认删除"; danger = true;
        body = '<div class="xws-warn">删除后，空间内的成员关系、项目绑定与范围化角色绑定将一并移除；集群中的命名空间与其中的资源不会被删除。此操作不可撤销。</div>' +
          fieldHTML("confirm", "请输入空间名称「" + m.name + "」以确认", '<input class="x-input" data-wsf="confirm" autocomplete="off" value="' + esc(f.confirm) + '">', E.confirm);
      } else if (m.type === "member") {
        title = "添加成员"; okText = "添加";
        body = subjectFields(m) + fieldHTML("role", "空间角色", '<select class="x-input" data-wsf="role">' + opts(ROLES, f.role) + "</select>", "",
          "所有者与管理员可管理空间；成员可在空间项目中工作；只读仅可查看");
      } else if (m.type === "project") {
        title = "绑定项目"; okText = "绑定";
        var cl = st.clusters.map(function (c) { return [c.id, c.name + (c.env ? "（" + c.env + "）" : "")]; });
        var ns = f.cluster_id ? st.nsCache[f.cluster_id] : null;
        var bound = st.detail.projects.filter(function (p) { return same(p.cluster_id, f.cluster_id); }).map(function (p) { return p.namespace; });
        var nsOpts = ns && ns.items ? ns.items.map(function (n) { var b = bound.indexOf(n) >= 0; return [n, n + (b ? "（已绑定）" : ""), b]; }) : [];
        var nsHint = !f.cluster_id ? "请先选择集群" : ns && ns.loading ? "正在加载命名空间…" : "";
        body = fieldHTML("cluster_id", "集群", '<select class="x-input" data-wsf="cluster_id">' + opts(cl, f.cluster_id, cl.length ? "请选择集群" : "暂无集群") + "</select>",
            E.cluster_id || (st.refErr.clusters ? "集群列表加载失败：" + st.refErr.clusters : "")) +
          fieldHTML("namespace", "命名空间", '<select class="x-input" data-wsf="namespace"' + (f.cluster_id && ns && ns.ok ? "" : " disabled") + ">" + opts(nsOpts, f.namespace, nsOpts.length ? "请选择命名空间" : (ns && ns.ok ? "该集群暂无命名空间" : "—")) + "</select>",
            E.namespace || (ns && ns.error) || "", nsHint || "一个命名空间只能归属一个企业空间");
      } else if (m.type === "binding") {
        title = "添加角色绑定"; okText = "添加";
        var roles = st.roles.map(function (r) { return [r.id, r.name]; });
        var scopes = [["workspace", "本空间"]].concat(st.detail.projects.length ? [["project", "空间内项目"]] : []);
        var projs = st.detail.projects.map(function (p) { return [p.cluster_id + "/" + p.namespace, (clusterName(p.cluster_id, p) || "集群 #" + p.cluster_id) + " / " + p.namespace]; });
        body = fieldHTML("role_id", "角色", '<select class="x-input" data-wsf="role_id">' + opts(roles, f.role_id, roles.length ? "请选择角色" : "暂无可选项") + "</select>",
            E.role_id || (st.refErr.roles ? "角色列表加载失败：" + st.refErr.roles : "")) +
          subjectFields(m) +
          fieldHTML("scope", "生效范围", '<select class="x-input" data-wsf="scope">' + opts(scopes, f.scope) + "</select>") +
          (f.scope === "project" ? fieldHTML("project", "项目", '<select class="x-input" data-wsf="project">' + opts(projs, f.project, "请选择项目") + "</select>", E.project) : "");
      } else if (m.type === "confirm") {
        title = m.title; okText = m.okText; danger = true;
        body = '<div class="xws-warn">' + m.html + "</div>";
      }
      return '<div class="xws-mask" data-wsp-mask><div class="xws-modal" role="dialog" aria-modal="true"><h3>' + esc(title) + "</h3>" + body +
        (m.error ? '<p class="xws-err" data-wsp-modal-error>' + esc(m.error) + "</p>" : "") +
        '<div class="xws-foot"><button type="button" class="x-btn" data-wsp-click="cancel"' + (m.busy ? " disabled" : "") + ">取消</button>" +
        '<button type="button" class="x-btn ' + (danger ? "x-btn--danger" : "x-btn--pri") + '" data-wsp-click="submit"' + (m.busy ? " disabled" : "") + ">" + (m.busy ? "正在提交…" : esc(okText)) + "</button></div></div></div>";
    }

    function paint() {
      if (!alive()) return;
      el.innerHTML = '<div class="xws" data-wsp>' + (st.view === "detail" && st.ws ? detailView() : listView()) + modalHTML() + "</div>";
    }

    // 读取弹窗中的当前输入（重绘前调用，避免丢失用户输入）
    function syncForm() {
      var m = st.modal; if (!m || !m.form) return;
      var nodes = el.querySelectorAll("[data-wsf]");
      for (var i = 0; i < nodes.length; i++) m.form[nodes[i].getAttribute("data-wsf")] = nodes[i].value == null ? "" : String(nodes[i].value);
    }
    function openModal(m) { m.form = m.form || {}; m.errors = {}; m.error = ""; m.busy = false; st.modal = m; paint(); }
    function fail(m, e) {
      if (!alive()) return;
      var x = X();
      if (x.isApproval && x.isApproval(e)) { st.modal = null; paint(); toast(errMsg(e)); return; }
      m.busy = false; m.error = errMsg(e);
      if (m.type === "ws" && e && Number(e.status) === 409) { m.errors = { name: m.error }; m.error = ""; }
      if (m.type === "project" && e && Number(e.status) === 409) { m.errors = { namespace: m.error }; m.error = ""; }
      paint();
    }
    function done(msg, after) {
      if (!alive()) return;
      st.modal = null; toast(msg);
      return after();
    }
    function wsBase() { return "/workspaces/" + encodeURIComponent(st.ws.id); }
    function refreshDetail() { return Promise.all([loadDetail(true), loadList(true)]); }

    var submitters = {
      ws: function (m) {
        var v = validateWorkspace(m.form, st.items, m.id);
        m.errors = v.errors;
        if (Object.keys(v.errors).length) { paint(); return; }
        m.busy = true; m.error = ""; paint();
        var p = m.id ? api("/workspaces/" + encodeURIComponent(m.id), { method: "PUT", body: v.body }) : api("/workspaces", { method: "POST", body: v.body });
        return p.then(function (res) {
          return done(m.id ? "已保存企业空间" : "已创建企业空间 " + v.body.name, function () {
            if (m.id && st.ws && same(st.ws.id, m.id)) st.ws = Object.assign({}, st.ws, v.body, res && res.id != null ? res : {});
            return loadList(true);
          });
        }, function (e) { fail(m, e); });
      },
      delWs: function (m) {
        var name = String(m.form.confirm || "").trim();
        if (name !== m.name) { m.errors = { confirm: name ? "输入的名称与空间名称不一致" : "请输入空间名称确认" }; paint(); return; }
        m.errors = {}; m.busy = true; m.error = ""; paint();
        return api("/workspaces/" + encodeURIComponent(m.id), { method: "DELETE", body: { confirm_name: name } }).then(function () {
          return done("已删除企业空间 " + name, function () {
            if (st.ws && same(st.ws.id, m.id)) { st.ws = null; st.view = "list"; }
            return loadList(true);
          });
        }, function (e) { fail(m, e); });
      },
      member: function (m) {
        var f = m.form, E = {};
        if (!f.subject) E.subject = f.kind === "group" ? "请选择用户组" : "请选择用户";
        else if (st.detail.members.some(function (x) { return f.kind === "group" ? same(x.group_id, f.subject) : same(x.user_id, f.subject); })) E.subject = "已是本空间成员";
        m.errors = E; if (E.subject) { paint(); return; }
        var body = { role: f.role || "member" }; body[f.kind === "group" ? "group_id" : "user_id"] = Number(f.subject);
        m.busy = true; m.error = ""; paint();
        return api(wsBase() + "/members", { method: "POST", body: body }).then(function () {
          return done("已添加成员", refreshDetail);
        }, function (e) { fail(m, e); });
      },
      project: function (m) {
        var f = m.form, E = {};
        if (!f.cluster_id) E.cluster_id = "请选择集群";
        else if (!f.namespace) E.namespace = "请选择命名空间";
        m.errors = E; if (E.cluster_id || E.namespace) { paint(); return; }
        m.busy = true; m.error = ""; paint();
        return api(wsBase() + "/projects", { method: "POST", body: { cluster_id: Number(f.cluster_id), namespace: f.namespace } }).then(function () {
          return done("已绑定项目 " + f.namespace, refreshDetail);
        }, function (e) { fail(m, e); });
      },
      binding: function (m) {
        var f = m.form, E = {};
        if (!f.role_id) E.role_id = "请选择角色";
        if (!f.subject) E.subject = f.kind === "group" ? "请选择用户组" : "请选择用户";
        if (f.scope === "project" && !f.project) E.project = "请选择项目";
        m.errors = E; if (Object.keys(E).length) { paint(); return; }
        var body = { role_id: Number(f.role_id), scope: f.scope === "project" ? "project" : "workspace", scope_value: f.scope === "project" ? f.project : String(st.ws.id) };
        body[f.kind === "group" ? "group_id" : "user_id"] = Number(f.subject);
        m.busy = true; m.error = ""; paint();
        return api("/role-bindings", { method: "POST", body: body }).then(function () {
          return done("已添加角色绑定", function () { return loadDetail(true); });
        }, function (e) { fail(m, e); });
      },
      confirm: function (m) {
        m.busy = true; m.error = ""; paint();
        return m.run().then(function () { return done(m.doneText, m.after); }, function (e) { fail(m, e); });
      }
    };

    function confirmModal(title, html, okText, run, doneText, after) {
      openModal({ type: "confirm", title: title, html: html, okText: okText, run: run, doneText: doneText, after: after || refreshDetail });
    }

    var actions = {
      reload: function () { return Promise.all([loadRefs(), loadList()]).then(paint); },
      reloadDetail: function () { return loadDetail(); },
      back: function () { st.view = "list"; st.ws = null; paint(); return loadList(true); },
      open: function (t) {
        var w = pick(st.items, t.getAttribute("data-id")); if (!w) return;
        st.ws = w; st.view = "detail"; st.tab = "members";
        st.detail = { loading: true, error: "", members: [], projects: [], bindings: [], bindErr: "" };
        return loadDetail();
      },
      tab: function (t) { st.tab = t.getAttribute("data-tab") || "members"; paint(); },
      clearSearch: function () { st.q = ""; paint(); },
      new: function () { if (canManage) openModal({ type: "ws", id: null, form: { name: "", description: "", manager_id: "", cpu: "", memory: "" } }); },
      edit: function (t) {
        if (!canManage) return;
        var id = t.getAttribute("data-id"), w = pick(st.items, id) || (st.ws && same(st.ws.id, id) ? st.ws : null); if (!w) return;
        openModal({ type: "ws", id: w.id, form: {
          name: w.name || "", description: w.description || "", manager_id: w.manager_id == null ? "" : String(w.manager_id),
          cpu: num(w.cpu_limit_milli) > 0 ? String(num(w.cpu_limit_milli) / 1000) : "",
          memory: num(w.memory_limit_bytes) > 0 ? String(Math.round(num(w.memory_limit_bytes) / GIB * 1000) / 1000) : ""
        } });
      },
      del: function (t) {
        if (!canManage) return;
        var id = t.getAttribute("data-id"), w = pick(st.items, id) || (st.ws && same(st.ws.id, id) ? st.ws : null); if (!w) return;
        openModal({ type: "delWs", id: w.id, name: w.name, form: { confirm: "" } });
      },
      addMember: function () { if (canManage) openModal({ type: "member", form: { kind: "user", subject: "", role: "member" } }); },
      rmMember: function (t) {
        var mb = pick(st.detail.members, t.getAttribute("data-id")); if (!mb || !canManage) return;
        var who = mb.user_id != null ? userName(mb.user_id, mb) : "用户组 " + groupName(mb.group_id, mb);
        confirmModal("移除成员", "确定将 <b>" + esc(who) + "</b> 移出企业空间「" + esc(st.ws.name) + "」？移除后其在本空间获得的访问权限将失效。", "确认移除",
          function () { return api(wsBase() + "/members/" + encodeURIComponent(mb.id), { method: "DELETE" }); }, "已移除成员");
      },
      addProject: function () { if (canManage) openModal({ type: "project", form: { cluster_id: "", namespace: "" } }); },
      rmProject: function (t) {
        var p = pick(st.detail.projects, t.getAttribute("data-id")); if (!p || !canManage) return;
        confirmModal("解绑项目", "确定将 <b>" + esc((clusterName(p.cluster_id, p) || "集群 #" + p.cluster_id) + " / " + p.namespace) + "</b> 从本空间解绑？命名空间与其中的资源不会被删除，但空间成员将不再经本空间访问它。", "确认解绑",
          function () { return api(wsBase() + "/projects/" + encodeURIComponent(p.id), { method: "DELETE" }); }, "已解绑项目 " + p.namespace);
      },
      addBinding: function () { if (canManage) openModal({ type: "binding", form: { role_id: "", kind: "user", subject: "", scope: "workspace", project: "" } }); },
      rmBinding: function (t) {
        var b = pick(st.detail.bindings, t.getAttribute("data-id")); if (!b || !canManage) return;
        var who = b.user_id != null ? userName(b.user_id, b) : "用户组 " + groupName(b.group_id, b);
        confirmModal("删除角色绑定", "确定删除 <b>" + esc(who) + "</b> 在「" + esc(scopeText(b)) + "」范围内的角色「" + esc(roleName(b.role_id, b)) + "」？", "确认删除",
          function () { return api("/role-bindings/" + encodeURIComponent(b.id), { method: "DELETE" }); }, "已删除角色绑定", function () { return loadDetail(true); });
      },
      cancel: function () { if (st.modal && !st.modal.busy) { st.modal = null; paint(); } },
      submit: function () {
        var m = st.modal; if (!m || m.busy) return;
        syncForm();
        var fn = submitters[m.type]; if (fn) return fn(m);
      }
    };

    function changeMemberRole(t) {
      var mb = pick(st.detail.members, t.getAttribute("data-wsp-role")), role = String(t.value || "");
      if (!mb || !canManage || !ROLE_TEXT[role] || role === mb.role) return;
      var prev = mb.role;
      mb.role = role;
      return api(wsBase() + "/members/" + encodeURIComponent(mb.id), { method: "PUT", body: { role: role } }).then(function () {
        if (!alive()) return; toast("已将角色改为「" + ROLE_TEXT[role] + "」"); paint();
      }, function (e) {
        if (!alive()) return; mb.role = prev; paint(); toast("修改角色失败：" + errMsg(e));
      });
    }

    if (el._xsWspClick) el.removeEventListener("click", el._xsWspClick);
    if (el._xsWspChange) el.removeEventListener("change", el._xsWspChange);
    if (el._xsWspInput) el.removeEventListener("input", el._xsWspInput);
    el._xsWspClick = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-wsp-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-wsp-click")];
      if (fn) return fn(t);
    };
    el._xsWspChange = function (ev) {
      if (!alive()) return;
      var t = ev.target; if (!t || !t.getAttribute) return;
      if (t.getAttribute("data-wsp-role") != null) return changeMemberRole(t);
      var k = t.getAttribute("data-wsf");
      if (!k || !st.modal) return;
      syncForm();
      var f = st.modal.form;
      if (k === "kind") { f.subject = ""; paint(); }
      else if (k === "scope") { paint(); }
      else if (k === "cluster_id") { f.namespace = ""; paint(); return loadNamespaces(f.cluster_id); }
    };
    el._xsWspInput = function (ev) {
      if (!alive()) return;
      var t = ev.target; if (!t || !t.getAttribute) return;
      if (t.getAttribute("data-wsp-search") != null) { st.q = String(t.value || ""); paint(); }
    };
    el.addEventListener("click", el._xsWspClick);
    el.addEventListener("change", el._xsWspChange);
    el.addEventListener("input", el._xsWspInput);

    paint();
    return Promise.all([loadRefs(), loadList(true)]).then(function () { if (alive()) { st.loading = false; paint(); } });
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(v) {
    if (!v) return "—";
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }
  // 绑定是否属于该空间：空间范围（scope_value=空间 ID）或空间内项目范围（"<集群ID>/<命名空间>"）。
  function bindingInWorkspace(b, wsId, projects) {
    if (!b) return false;
    if (b.scope === "workspace") return same(b.scope_value, wsId);
    if (b.scope === "project") return (projects || []).some(function (p) { return String(b.scope_value) === p.cluster_id + "/" + p.namespace; });
    return false;
  }

  root.XSWorkspace = { render: render, validate: validateWorkspace, fmtQuota: fmtQuota, bindingInWorkspace: bindingInWorkspace, ROLE_TEXT: ROLE_TEXT };
})(window);
