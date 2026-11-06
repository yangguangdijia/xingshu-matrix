/*
 * 星枢 Matrix 堡垒机（按 JumpServer 信息架构组织）。
 *
 * 本文件：公共工具、页面路由、资产列表（资产树 + 平台）、数据脱敏、登录安全、Web 终端命令审批横幅。
 * 其余页面在同目录的两个文件中，本文件加载时自动引入：
 *   xs-bastion-admin.js  控制台总览、平台、账号管理、改密计划、资产授权、命令过滤、登录 ACL
 *   xs-bastion-ops.js    我的资产、数据库控制台、在线 / 历史会话、会话录像、命令记录、登录日志、命令复核、SQL 工单
 *
 * index.html 在实时模式下调用：
 *   XSBastion.has(key)                   该页面是否由本模块实现
 *   XSBastion.render(key, el, hooks)     渲染页面；hooks.connect(asset) 从资产打开 SSH，hooks.ssh(grant) 按授权打开 SSH
 *   XSBastion.approvalBanner(state) / XSBastion.paintApproval(session)  Web 终端命令审批等待提示
 */
(function (root) {
  "use strict";
  var ENV = [{ v: "prod", t: "生产" }, { v: "pre", t: "预发" }, { v: "test", t: "测试" }, { v: "dev", t: "开发" }];
  var envName = function (v) { var e = ENV.filter(function (x) { return x.v === v; })[0]; return e ? e.t : (v || "—"); };
  var ENV_CLS = { prod: "x-red", pre: "x-orange", test: "x-blue", dev: "x-green" };
  var PROTO_NAME = { ssh: "SSH", mysql: "MySQL", postgres: "PostgreSQL", rdp: "RDP" };
  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }

  // 后端错误信息多为英文，这里统一翻译成中文提示
  var ERR_ZH = [
    [/no active authorization rule/i, "没有生效的资产授权"],
    [/not in your scope/i, "该环境不在你的权限范围内"],
    [/permission denied/i, "没有执行该操作的权限"],
    [/already exists|duplicate key/i, "名称已存在"],
    [/^not found$/i, "记录不存在或已被删除"],
    [/invalid regex/i, "正则表达式语法错误"],
    [/pattern must not match an empty/i, "表达式不能匹配空内容"],
    [/select at least one asset or node/i, "请至少选择一个资产或节点"],
    [/expires_at must be later/i, "失效时间必须晚于生效时间"],
    [/unknown action/i, "包含未知的授权动作"],
    [/account_ids contains unknown/i, "所选账号中有不存在的账号"],
    [/length must be 12-64/i, "密码长度需在 12–64 位"],
    [/invalid ip range/i, "来源 IP 格式不正确"],
    [/time must be HH:MM/i, "时间需为 HH:MM 格式"],
    [/time_from and time_to/i, "开始与结束时间需同时填写且不能相同"],
    [/weekdays must be/i, "星期取值不正确"],
    [/subject is required/i, "请选择适用对象"],
    [/priority must be/i, "优先级需为 1–100000"],
    [/node name must be/i, "节点名称为 1–64 个字符，不能包含 /"],
    [/own parent|own descendant/i, "不能把节点移动到它自己或其子节点下"],
    [/protocol must be ssh/i, "该协议没有可用连接器，只能登记 SSH、MySQL、PostgreSQL 资产"],
    [/options must not contain credentials/i, "附加参数里不能包含密码、密钥等凭据"],
    [/invalid private key/i, "私钥格式不正确或口令错误"],
    [/secret is required/i, "请填写凭据"],
    [/account does not belong to asset/i, "账号不属于该资产"],
    [/asset is not a MySQL/i, "该资产不是数据库资产"],
    [/invalid database name/i, "数据库名称不合法"],
    [/session is not active/i, "会话已结束或所在实例不可用"],
    [/recording unavailable/i, "录像不可用或尚未写完"],
    [/scope must be|approval:read/i, "没有查看全部审批单的权限"],
    [/不能审批自己/i, "不能审批自己发起的申请"],
    [/ticket is not pending|decided concurrently/i, "该审批单已被处理"],
    [/invalid request body/i, "提交内容格式不正确"],
    [/environment is required/i, "资产缺少环境标记，无法提交审批"],
    [/^请求失败（HTTP 403）$/, "没有权限"]
  ];
  function errText(e) {
    var m = (e && (e.message || e.error)) || String(e || "未知错误");
    for (var i = 0; i < ERR_ZH.length; i++) if (ERR_ZH[i][0].test(m)) return ERR_ZH[i][1];
    if (e && e.status === 403 && !/[\u4e00-\u9fa5]/.test(m)) return "没有权限";
    return m;
  }
  function items(r) { return Array.isArray(r) ? r : (r && (r.items || r.data)) || []; }
  function list(p, q) { return xs().get(p, q).then(items); }
  function soft(p, q) { return list(p, q).catch(function () { return null; }); }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(t, withSec) {
    if (!t) return "—";
    var d = new Date(t);
    if (isNaN(d)) return String(t);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + (withSec ? ":" + pad(d.getSeconds()) : "");
  }
  function fmtDur(ms) {
    if (ms == null || isNaN(ms)) return "—";
    var s = Math.max(0, Math.round(ms / 1000)), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
    return h ? h + " 小时 " + m + " 分" : m ? m + " 分 " + (s % 60) + " 秒" : (s % 60) + " 秒";
  }
  function group(a) { return (a.options && a.options.group) || "未分组"; }
  function tags(a) { var t = a.options && a.options.tags; return Array.isArray(t) ? t : (t ? String(t).split(",") : []); }
  function tag(text, cls) { return '<span class="x-tag x-tag--s ' + (cls || "x-grey") + '">' + E(text) + "</span>"; }
  function head(title, desc, acts) {
    return '<div class="xb-head"><div><h2>' + E(title) + "</h2>" + (desc ? "<p>" + E(desc) + "</p>" : "") + "</div>" +
      (acts ? '<div class="xb-head-act">' + acts + "</div>" : "") + "</div>";
  }
  function table(cols, rows, empty) {
    if (!rows.length) return '<p class="xb-empty">' + E(empty || "暂无数据") + "</p>";
    return '<div class="xb-table"><table><thead><tr>' + cols.map(function (c) { return "<th>" + E(c) + "</th>"; }).join("") + "</tr></thead><tbody>" +
      rows.map(function (r) { return "<tr>" + r.map(function (c) { return "<td>" + (c == null ? "—" : c) + "</td>"; }).join("") + "</tr>"; }).join("") + "</tbody></table></div>";
  }
  function on(el, attr, fn) {
    [].forEach.call(el.querySelectorAll("[" + attr + "]"), function (b) { b.onclick = function (ev) { fn(b.getAttribute(attr), b, ev); }; });
  }
  function byId(arr, id) { return (arr || []).filter(function (x) { return String(x.id) === String(id); })[0]; }
  function segs(name, opts, cur) {
    return '<div class="xb-segs" role="tablist">' + opts.map(function (o) {
      return '<button type="button" class="x-seg" data-' + name + '="' + E(o.v) + '" aria-pressed="' + (String(o.v) === String(cur)) + '">' + E(o.t) + (o.n ? " <b>" + E(o.n) + "</b>" : "") + "</button>";
    }).join("") + "</div>";
  }
  function pager(total, page, size) {
    var pages = Math.max(1, Math.ceil((total || 0) / size));
    return '<div class="xb-pager"><span class="x-frac">共 ' + (total || 0) + " 条</span>" +
      '<button class="x-btn x-btn--s" data-pg="' + (page - 1) + '"' + (page <= 1 ? " disabled" : "") + ">上一页</button>" +
      '<span class="x-frac">' + page + " / " + pages + "</span>" +
      '<button class="x-btn x-btn--s" data-pg="' + (page + 1) + '"' + (page >= pages ? " disabled" : "") + ">下一页</button></div>";
  }
  function loading(el, title, desc) { el.innerHTML = head(title, desc) + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>'; }
  function failBox(e, what) { return '<p class="x-aerr" role="alert">' + E((what || "读取失败") + "：" + errText(e)) + "</p>"; }
  function go(k) { location.hash = "#/bastion/" + k; }
  // 页面间传递一次性参数（例如从资产打开数据库控制台、从会话跳到录像）
  var hand = {};
  function handoff(k, v) { if (arguments.length > 1) { hand[k] = v; return v; } var x = hand[k]; delete hand[k]; return x; }

  /*
   * 二次确认。opts.typeName：高危删除需要输入该名称才能确认；opts.input：额外的文本输入（如审批意见），值传给 run。
   */
  function confirmBox(title, html, okLabel, run, opts) {
    opts = opts || {};
    var m = document.createElement("div");
    m.className = "xb-confirm";
    var typed = opts.typeName != null ? '<label class="xb-typed"><span>请输入 <b>' + E(opts.typeName) + "</b> 以确认</span>" +
      '<input class="x-input" data-typed autocomplete="off" spellcheck="false"></label>' : "";
    var extra = opts.input ? '<label class="xb-typed"><span>' + E(opts.input.label) + "</span>" +
      '<textarea class="x-input" data-extra rows="3" maxlength="' + (opts.input.max || 2000) + '" placeholder="' + E(opts.input.placeholder || "") + '"></textarea></label>' : "";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true" aria-label="' + E(title) + '"><h3>' + E(title) + '</h3><div class="xb-confirm-body">' + html + typed + extra +
      '</div><p class="x-aerr" data-err hidden></p><div class="xb-confirm-foot"><button class="x-btn" data-no>取消</button><button class="x-btn ' +
      (opts.primary ? "x-btn--pri" : "x-btn--danger") + '" data-yes' + (opts.typeName != null ? " disabled" : "") + ">" + E(okLabel) + "</button></div></div>";
    document.body.appendChild(m);
    var close = function () { m.remove(); document.removeEventListener("keydown", esc); };
    var esc = function (e) { if (e.key === "Escape") close(); };
    document.addEventListener("keydown", esc);
    var yes = m.querySelector("[data-yes]"), ti = m.querySelector("[data-typed]");
    if (ti) { ti.oninput = function () { yes.disabled = ti.value.trim() !== String(opts.typeName); }; setTimeout(function () { ti.focus(); }, 0); }
    m.querySelector("[data-no]").onclick = close;
    m.onclick = function (e) { if (e.target === m) close(); };
    yes.onclick = function () {
      var ex = m.querySelector("[data-extra]"), val = ex ? ex.value.trim() : "";
      var p = m.querySelector("[data-err]");
      if (opts.input && opts.input.required && !val) { p.textContent = "请填写" + opts.input.label; p.hidden = false; return; }
      yes.disabled = true;
      Promise.resolve(run(val)).then(close).catch(function (e) { yes.disabled = false; p.textContent = errText(e); p.hidden = false; });
    };
  }
  // 删除统一入口：输入名称确认
  function confirmDelete(what, name, note, run) {
    confirmBox("删除" + what, "确定删除" + what + " <b>" + E(name) + "</b>？" + (note ? "<br>" + note : "") + "<br>此操作不可恢复。", "删除", run, { typeName: name });
  }
  // 只读详情弹窗
  function modal(title, bodyHTML, wide) {
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card' + (wide ? " xb-wide" : "") + '" role="dialog" aria-modal="true" aria-label="' + E(title) + '"><h3>' + E(title) +
      '</h3><div class="xb-confirm-body" data-body>' + bodyHTML + '</div><div class="xb-confirm-foot"><button class="x-btn" data-no>关闭</button></div></div>';
    document.body.appendChild(m);
    var close = function () { m.remove(); if (m.onclose) m.onclose(); };
    m.querySelector("[data-no]").onclick = close;
    m.onclick = function (e) { if (e.target === m) close(); };
    m.body = m.querySelector("[data-body]");
    m.close = close;
    return m;
  }
  function form(spec) {
    if (!root.XSFormKit) { toast("表单组件未加载"); return; }
    root.XSFormKit.open(spec);
  }
  // "2026-12-31 18:00" ⇄ ISO
  function parseLocal(s) {
    s = String(s || "").trim();
    if (!s) return null;
    var m = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2}))?$/.exec(s);
    if (!m) return undefined;
    var d = new Date(+m[1], +m[2] - 1, +m[3], +(m[4] || 0), +(m[5] || 0));
    return isNaN(d) ? undefined : d.toISOString();
  }
  function toLocal(iso) { return iso ? fmtTime(iso) : ""; }
  // 资产树节点：返回 { byId, kids, path(id) }
  function nodeIndex(nodes) {
    var by = {}, kids = {};
    (nodes || []).forEach(function (n) { by[n.id] = n; var p = n.parent_id || 0; (kids[p] = kids[p] || []).push(n); });
    var path = function (id) { var out = [], n = by[id], guard = 0; while (n && guard++ < 50) { out.unshift(n.name); n = by[n.parent_id]; } return out.join(" / "); };
    var desc = function (id) { var out = [id]; (kids[id] || []).forEach(function (c) { out = out.concat(desc(c.id)); }); return out; };
    return { byId: by, kids: kids, path: path, desc: desc };
  }
  function nodeOptions(nodes) {
    var ix = nodeIndex(nodes);
    return (nodes || []).map(function (n) { return { v: String(n.id), t: ix.path(n.id) }; }).sort(function (a, b) { return a.t < b.t ? -1 : 1; });
  }
  function assetLabel(a) { return a ? a.name + "（" + a.host + "）" : "已删除的资产"; }

  var U = {
    E: E, xs: xs, toast: toast, errText: errText, items: items, list: list, soft: soft, fmtTime: fmtTime, fmtDur: fmtDur, tag: tag, head: head, table: table,
    on: on, byId: byId, segs: segs, pager: pager, loading: loading, failBox: failBox, go: go, handoff: handoff, confirmBox: confirmBox, confirmDelete: confirmDelete,
    modal: modal, form: form, parseLocal: parseLocal, toLocal: toLocal, nodeIndex: nodeIndex, nodeOptions: nodeOptions, assetLabel: assetLabel,
    ENV: ENV, envName: envName, ENV_CLS: ENV_CLS, PROTO_NAME: PROTO_NAME
  };

  /* ------------------------------------------------------------------ 资产列表（资产树 + 平台） */
  var A = { node: "all", env: "", q: "" };

  function renderAssets(el, hooks) {
    hooks = hooks || {};
    el.innerHTML = head("资产列表", "按资产树节点组织服务器与数据库；登录凭据由后端加密托管，页面不保存明文。",
      '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 添加资产</button>') +
      '<div class="xb-body"><p class="x-frac">正在读取资产…</p></div>';
    var reload = function () { renderAssets(el, hooks); };
    el.querySelector("[data-refresh]").onclick = reload;
    Promise.all([list("/bastion/assets", { size: 200 }), soft("/bastion/accounts"), soft("/bastion/sessions", { size: 200 }), soft("/bastion/nodes"), soft("/bastion/platforms")]).then(function (r) {
      var ctx = { assets: r[0], accounts: r[1] || [], sessions: r[2], nodes: r[3] || [], platforms: r[4] || [], hooks: hooks, reload: reload };
      el.querySelector("[data-new]").onclick = function () { assetWizard(null, ctx); };
      drawAssets(el, ctx);
    }).catch(function (e) {
      el.querySelector(".xb-body").innerHTML = failBox(e, "资产读取失败");
    });
  }

  function drawAssets(el, ctx) {
    var assets = ctx.assets, accBy = {}, lastBy = {}, ix = nodeIndex(ctx.nodes), plat = {};
    ctx.platforms.forEach(function (p) { plat[p.id] = p; });
    ctx.accounts.forEach(function (a) { (accBy[a.asset_id] = accBy[a.asset_id] || []).push(a); });
    (ctx.sessions || []).forEach(function (s) { if (!lastBy[s.asset_id] || s.started_at > lastBy[s.asset_id]) lastBy[s.asset_id] = s.started_at; });
    var dbN = assets.filter(function (a) { return a.protocol !== "ssh"; }).length;
    var active = (ctx.sessions || []).filter(function (s) { return s.status === "active"; }).length;
    var stats = [
      ["资产总数", assets.length, (assets.length - dbN) + " 台主机 · " + dbN + " 个数据库"],
      ["托管账号", ctx.accounts.length, "凭据加密保存"],
      ["资产节点", ctx.nodes.length, "资产树层级"],
      ["在线会话", ctx.sessions ? active : "—", ctx.sessions ? "正在进行的 SSH 会话" : "需要审计权限"]
    ];
    // 节点计数包含子节点
    var countIn = function (id) { var set = {}; ix.desc(id).forEach(function (x) { set[x] = 1; }); return assets.filter(function (a) { return a.node_id && set[a.node_id]; }).length; };
    var nodeBtn = function (key, label, n, depth, nodeId) {
      return '<div class="xb-node-row"><button type="button" class="xb-node' + (A.node === key ? " on" : "") + '" data-node="' + E(key) + '" style="padding-left:' + (10 + depth * 16) + 'px">' +
        "<span>" + E(label) + "</span><b>" + n + "</b></button>" +
        (nodeId ? '<span class="xb-node-ops"><button type="button" class="x-act" data-nadd="' + nodeId + '" title="新建子节点" aria-label="在 ' + E(label) + ' 下新建子节点">+</button>' +
          '<button type="button" class="x-act" data-nedit="' + nodeId + '" title="重命名" aria-label="重命名 ' + E(label) + '">✎</button>' +
          '<button type="button" class="x-act x-act--danger" data-ndel="' + nodeId + '" title="删除" aria-label="删除 ' + E(label) + '">×</button></span>' : "") + "</div>";
    };
    var walk = function (pid, depth) {
      return (ix.kids[pid] || []).slice().sort(function (a, b) { return a.name < b.name ? -1 : 1; }).map(function (n) {
        return nodeBtn("n:" + n.id, n.name, countIn(n.id), depth, n.id) + walk(n.id, depth + 1);
      }).join("");
    };
    var unassigned = assets.filter(function (a) { return !a.node_id || !ix.byId[a.node_id]; }).length;
    var treeHTML = nodeBtn("all", "全部资产", assets.length, 0) + walk(0, 0) + (unassigned ? nodeBtn("none", "未分配节点", unassigned, 0) : "");
    var inNode = {};
    if (A.node.indexOf("n:") === 0) ix.desc(+A.node.slice(2)).forEach(function (x) { inNode[x] = 1; });
    var q = A.q.toLowerCase();
    var shown = assets.filter(function (a) {
      if (A.node === "none" && a.node_id && ix.byId[a.node_id]) return false;
      if (A.node.indexOf("n:") === 0 && !inNode[a.node_id]) return false;
      if (A.env && a.env !== A.env) return false;
      return !q || [a.name, a.host, a.description, group(a), ix.path(a.node_id)].concat(tags(a)).join(" ").toLowerCase().indexOf(q) >= 0;
    });
    var rows = shown.map(function (a) {
      var acc = accBy[a.id] || [], p = plat[a.platform_id], isDB = a.protocol === "mysql" || a.protocol === "postgres";
      var conn = isDB
        ? '<button class="x-btn x-btn--pri x-btn--s" data-dbc="' + a.id + '"' + (acc.length ? "" : ' disabled title="先托管一个数据库账号"') + ">控制台</button>"
        : '<button class="x-btn x-btn--pri x-btn--s" data-conn="' + a.id + '"' + (acc.length ? "" : ' disabled title="先托管一个登录账号"') + ">连接</button>";
      return ['<b class="x-name">' + E(a.name) + '</b><div class="x-frac x-mono">' + E(a.host) + ":" + E(a.port) + "</div>",
        E(p ? p.name : PROTO_NAME[a.protocol] || a.protocol) + '<div class="x-frac">' + E(PROTO_NAME[a.protocol] || a.protocol) + "</div>",
        a.node_id && ix.byId[a.node_id] ? E(ix.path(a.node_id)) : '<span class="x-frac">未分配</span>',
        tag(envName(a.env), ENV_CLS[a.env]) + (tags(a).length ? '<div class="x-tags">' + tags(a).map(function (t) { return tag(t); }).join("") + "</div>" : ""),
        acc.length ? acc.map(function (x) { return E(x.username); }).join("、") : '<span class="x-frac">未托管</span>',
        a.protocol !== "ssh" ? '<span class="x-frac">—</span>' : a.host_key ? tag("已记录", "x-green") : '<span class="x-frac">首次连接时记录</span>',
        E(fmtTime(lastBy[a.id])),
        '<span class="xb-ops">' + conn + '<button class="x-act" data-test="' + a.id + '">测试</button><button class="x-act" data-edit="' + a.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + a.id + '">删除</button></span>'];
    });
    var envOpts = [{ v: "", t: "全部环境" }].concat(ENV);
    var body = el.querySelector(".xb-body");
    body.innerHTML = '<div class="xb-stats">' + stats.map(function (s) { return '<div class="xb-stat"><span>' + s[0] + "</span><b>" + E(s[1]) + "</b><em>" + E(s[2]) + "</em></div>"; }).join("") + "</div>" +
      '<div class="xb-split"><aside class="xb-tree" aria-label="资产树"><div class="xb-tree-h"><span>资产树</span><button type="button" class="x-act" data-nadd="0">+ 节点</button></div>' + treeHTML + "</aside>" +
      '<section class="xb-main"><div class="xb-toolbar"><div class="xb-tools"><input class="x-input" data-q placeholder="搜索名称、地址、节点、标签" value="' + E(A.q) + '">' +
      '<select class="x-select" data-env aria-label="按环境筛选">' + envOpts.map(function (o) { return '<option value="' + o.v + '"' + (o.v === A.env ? " selected" : "") + ">" + o.t + "</option>"; }).join("") + "</select></div>" +
      '<span class="x-frac">共 ' + shown.length + " 个</span></div>" +
      (assets.length ? table(["资产", "平台", "节点", "环境 / 标签", "托管账号", "主机指纹", "最近连接", "操作"], rows, "没有符合条件的资产。")
        : '<div class="xb-guide"><h3>还没有纳管任何资产</h3><ol><li>在左侧资产树建立节点（如「生产 / 支付」）</li><li>添加资产：选择平台（Linux、MySQL、PostgreSQL），填写地址与端口</li><li>托管账号并授权，即可在「工作台」连接</li></ol><button class="x-btn x-btn--pri" data-new2>+ 添加第一个资产</button></div>') +
      '<p class="x-frac xb-note">SSH 资产通过 Web 终端连接；MySQL / PostgreSQL 资产通过数据库控制台查询。Windows 等远程桌面平台只登记，暂无连接器。</p></section></div>';
    var redraw = function () { drawAssets(el, ctx); };
    var n2 = body.querySelector("[data-new2]"); if (n2) n2.onclick = function () { assetWizard(null, ctx); };
    on(body, "data-node", function (k) { A.node = k; redraw(); });
    on(body, "data-nadd", function (id) { nodeForm(null, +id || null, ctx); });
    on(body, "data-nedit", function (id) { nodeForm(byId(ctx.nodes, id), null, ctx); });
    on(body, "data-ndel", function (id) {
      var n = byId(ctx.nodes, id);
      if ((ix.kids[n.id] || []).length || countIn(n.id)) { toast("节点「" + n.name + "」下还有子节点或资产，请先移走"); return; }
      confirmDelete("节点", n.name, "", function () { return xs().del("/bastion/nodes/" + n.id).then(function () { toast("已删除节点 " + n.name); if (A.node === "n:" + n.id) A.node = "all"; ctx.reload(); }); });
    });
    var qi = body.querySelector("[data-q]");
    qi.oninput = function () { A.q = qi.value; clearTimeout(qi._t); qi._t = setTimeout(function () { redraw(); var n = el.querySelector("[data-q]"); n.focus(); n.setSelectionRange(n.value.length, n.value.length); }, 250); };
    body.querySelector("[data-env]").onchange = function () { A.env = this.value; redraw(); };
    var find = function (id) { return byId(assets, id); };
    on(body, "data-conn", function (id) { var a = find(id); if (a && ctx.hooks.connect) ctx.hooks.connect(a); else go("myassets"); });
    on(body, "data-dbc", function (id) { handoff("dbc", { asset_id: +id }); go("dbc"); });
    on(body, "data-test", function (id) { testAsset(find(id), accBy); });
    on(body, "data-edit", function (id) { assetWizard(find(id), ctx); });
    on(body, "data-del", function (id) {
      var a = find(id);
      confirmDelete("资产", a.name, "它的托管账号会一并删除；引用它的授权、改密计划需要单独调整。", function () {
        return xs().del("/bastion/assets/" + a.id).then(function () { toast("已删除资产 " + a.name); ctx.reload(); });
      });
    });
  }

  function nodeForm(n, parentId, ctx) {
    var editing = !!n;
    form({
      title: editing ? "编辑节点 · " + n.name : "新建节点", okText: editing ? "保存" : "创建", scene: "bastion.node", width: "sm",
      sections: [{ fields: [
        { k: "name", label: "节点名称", required: true, placeholder: "支付集群", check: function (v) { return /\//.test(v) || v.length > 64 ? "不能包含 /，最多 64 个字符" : ""; } },
        { k: "parent_id", label: "上级节点", type: "select", empty: "根节点", options: nodeOptions(ctx.nodes).filter(function (o) { return !editing || ctx.nodes && nodeIndex(ctx.nodes).desc(n.id).indexOf(+o.v) < 0; }) }
      ] }],
      values: editing ? { name: n.name, parent_id: n.parent_id ? String(n.parent_id) : "" } : { parent_id: parentId ? String(parentId) : "" },
      toObject: function (v) { return { name: (v.name || "").trim(), parent_id: v.parent_id ? Number(v.parent_id) : null }; },
      submit: function (v, obj, fctx) {
        var p = editing ? xs().put("/bastion/nodes/" + n.id, obj) : xs().post("/bastion/nodes", obj);
        return p.then(function () { fctx.close(); toast(editing ? "节点已更新" : "节点已创建"); ctx.reload(); }).catch(function (e) { throw new Error(errText(e)); });
      }
    });
  }

  function testAsset(a, accBy) {
    var acc = (accBy[a.id] || [])[0];
    var m = modal("连通性测试 · " + a.name, '<p class="x-frac">正在从平台连接 ' + E(a.host) + ":" + E(a.port) + "…</p>");
    var line = function (ok, t, d) { return '<li class="' + (ok ? "ok" : ok === false ? "bad" : "") + '"><b>' + (ok ? "✓" : ok === false ? "✕" : "·") + "</b><span>" + E(t) + (d ? "<em>" + E(d) + "</em>" : "") + "</span></li>"; };
    var isDB = a.protocol !== "ssh";
    xs().post("/bastion/assets/" + a.id + "/test", acc && !isDB ? { account_id: acc.id } : {}).then(function (r) {
      var it = [line(r.reachable, "网络连通", r.reachable ? "延迟 " + r.latency_ms + " ms" : errText(r.error))];
      if (r.reachable && !isDB) it.push(line(!!r.banner, "SSH 服务", r.banner || "没有读到 SSH 横幅，端口上可能不是 SSH 服务"));
      if (r.reachable && !isDB && acc) it.push(line(r.auth, "账号登录（" + acc.username + "）", r.auth ? "登录成功，未打开 Shell" : r.error));
      m.body.innerHTML = '<ul class="xb-check">' + it.join("") + "</ul>";
      if (r.reachable && isDB && acc) {
        m.body.querySelector(".xb-check").insertAdjacentHTML("beforeend", line(null, "数据库登录（" + acc.username + "）", "正在验证…"));
        return xs().post("/bastion/accounts/" + acc.id + "/verify", {}).then(function (v) {
          m.body.querySelector(".xb-check li:last-child").outerHTML = line(v.status === "ok", "数据库登录（" + acc.username + "）", v.message);
        });
      }
      if (r.reachable && !acc) m.body.querySelector(".xb-check").insertAdjacentHTML("beforeend", line(null, "账号登录", "还没有托管账号，跳过"));
    }).catch(function (e) { m.body.innerHTML = failBox(e, "测试失败"); });
  }

  function assetWizard(a, ctx) {
    var editing = !!a, u = xs().user || {}, me = u.username || "", meId = u.user_id != null ? String(u.user_id) : (u.id != null ? String(u.id) : "");
    var plats = ctx.platforms.filter(function (p) { return p.connectable; });
    if (!plats.length) plats = [{ id: 0, name: "Linux", protocol: "ssh", port: 22, category: "host" }];
    var platBy = function (id) { return plats.filter(function (p) { return String(p.id) === String(id); })[0] || plats[0]; };
    var isDB = function (v) { var p = platBy(v.platform_id); return p.category === "database"; };
    var sections = [{
      title: "资产信息", desc: "平台从自身所在网络连接这台资产，请确认地址可达。",
      fields: [
        { k: "platform_id", label: "平台", type: "radio", options: plats.map(function (p) { return { v: String(p.id), t: p.name, d: (PROTO_NAME[p.protocol] || p.protocol) + " · 默认端口 " + p.port }; }), def: String(plats[0].id),
          onChange: function (v, api) { var p = platBy(v.platform_id); api.set({ port: p.port }); } },
        { k: "name", label: "资产名称", required: true, placeholder: "web-01", help: "小写字母、数字、- _ .，全局唯一" },
        { k: "host", label: "地址", required: true, placeholder: "10.0.1.11 或 web-01.internal" },
        { k: "port", label: "端口", type: "number", required: true, def: plats[0].port },
        { k: "node_id", label: "所属节点", type: "select", empty: "未分配", options: nodeOptions(ctx.nodes), help: "资产树与按节点授权使用" },
        { k: "env", label: "环境", type: "radio", options: ENV, def: "dev" },
        { k: "database", label: "默认数据库", placeholder: "app_db", help: "数据库控制台默认连接的库，可留空", show: isDB },
        { k: "sslmode", label: "加密连接", type: "select", options: [{ v: "", t: "默认" }, { v: "disable", t: "不加密" }, { v: "require", t: "要求加密（不校验证书）" }, { v: "verify-full", t: "加密并校验证书" }], required: true, def: "",
          show: function (v) { return isDB(v) && platBy(v.platform_id).protocol === "postgres"; } },
        { k: "tls", label: "TLS", type: "select", options: [{ v: "", t: "不使用" }, { v: "preferred", t: "优先使用" }, { v: "true", t: "强制并校验证书" }, { v: "skip-verify", t: "强制但不校验证书" }], required: true, def: "",
          show: function (v) { return isDB(v) && platBy(v.platform_id).protocol === "mysql"; } },
        { k: "tags", label: "标签", type: "tags", placeholder: "输入后回车，例如 nginx" },
        { k: "description", label: "说明", type: "textarea", placeholder: "用途、负责人等" }
      ]
    }];
    if (!editing) {
      sections.push({
        title: "托管账号", desc: "登录凭据加密保存在后端，任何页面和接口都不会返回明文。可以跳过，稍后在「账号管理」添加。",
        fields: [
          { k: "with_account", label: "现在添加登录账号", type: "switch", def: true },
          { k: "username", label: "登录用户名", placeholder: "ops / readonly", show: function (v) { return v.with_account; } },
          { k: "auth_type", label: "认证方式", type: "radio", options: [{ v: "password", t: "密码" }, { v: "private_key", t: "SSH 私钥" }], def: "password", show: function (v) { return v.with_account && !isDB(v); } },
          { k: "password", label: "密码", type: "password", secret: true, show: function (v) { return v.with_account && (isDB(v) || v.auth_type === "password"); } },
          { k: "private_key", label: "私钥（PEM）", type: "code", secret: true, placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----", show: function (v) { return v.with_account && !isDB(v) && v.auth_type === "private_key"; } },
          { k: "passphrase", label: "私钥口令", type: "password", secret: true, help: "私钥没有口令就留空", show: function (v) { return v.with_account && !isDB(v) && v.auth_type === "private_key"; } }
        ]
      }, {
        title: "授权", desc: "授权后才能在「工作台 → 我的资产」看到并连接。更细的授权在「权限管理 → 资产授权」。",
        fields: [
          { k: "grant_me", label: "授权给我（" + (me || "当前用户") + "）", type: "switch", def: true },
          { k: "expire", label: "有效期", type: "radio", options: [{ v: "0", t: "长期" }, { v: "7", t: "7 天" }, { v: "30", t: "30 天" }], def: "0", show: function (v) { return v.grant_me; } }
        ]
      });
    }
    var o = editing ? a.options || {} : {};
    var vals = editing ? { platform_id: String(a.platform_id || (plats.filter(function (p) { return p.protocol === a.protocol; })[0] || plats[0]).id), name: a.name, host: a.host, port: a.port, env: a.env,
      node_id: a.node_id ? String(a.node_id) : "", tags: tags(a), description: a.description || "", database: o.database || "", sslmode: o.sslmode || "", tls: o.tls || "" } : {};
    if (!editing && A.node.indexOf("n:") === 0) vals.node_id = A.node.slice(2);
    var toObj = function (v) {
      var p = platBy(v.platform_id), opts = Object.assign({}, editing ? a.options : {}, { tags: v.tags || [] });
      ["database", "sslmode", "tls"].forEach(function (k) { if (v[k] && p.category === "database") opts[k] = v[k]; else delete opts[k]; });
      return { name: v.name, host: v.host, port: Number(v.port) || p.port, protocol: p.protocol, platform_id: p.id || null, node_id: v.node_id ? Number(v.node_id) : null,
        env: v.env || "dev", description: v.description || "", options: opts };
    };
    form({
      title: editing ? "编辑资产 · " + a.name : "添加资产", okText: editing ? "保存" : "添加", scene: "bastion.asset", wizard: !editing, width: "lg",
      aiExample: "生产环境的支付网关 pay-gw-01，地址 10.20.1.15，端口 22",
      sections: sections, values: vals, toObject: toObj,
      fromObject: function (ob, v) { return Object.assign({}, v, { name: ob.name, host: ob.host, port: ob.port, env: ob.env, description: ob.description, tags: (ob.options || {}).tags || [] }); },
      validate: function (v) {
        if (!/^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/.test(v.name || "")) return "资产名称只能用小写字母、数字、- _ .";
        if (!/^[A-Za-z0-9.:_-]+$/.test(v.host || "")) return "请填写合法的 IP 或域名";
        var p = Number(v.port); if (!(p >= 1 && p <= 65535)) return "端口需在 1–65535";
        if (v.database && !/^[A-Za-z0-9_$-]{1,64}$/.test(v.database)) return "默认数据库名称不合法";
        if (!editing && v.with_account) {
          if (!v.username) return "请填写登录用户名，或关闭「现在添加登录账号」";
          var pw = isDB(v) || v.auth_type === "password";
          if (pw && !v.password) return "请填写密码";
          if (!pw && !/-----BEGIN/.test(v.private_key || "")) return "请粘贴 PEM 格式的私钥";
        }
        return "";
      },
      submit: function (v, obj, fctx) {
        if (editing) return xs().put("/bastion/assets/" + a.id, Object.assign({ host_key: a.host_key || "" }, obj)).then(function () { fctx.close(); toast("资产已更新"); ctx.reload(); }).catch(function (e) { throw new Error(errText(e)); });
        var asset, account, db = isDB(v);
        fctx.status("正在登记资产…");
        return xs().post("/bastion/assets", obj).then(function (r) {
          asset = r;
          if (!v.with_account) return null;
          fctx.status("正在加密保存账号…");
          var pk = !db && v.auth_type === "private_key";
          return xs().post("/bastion/accounts", { asset_id: asset.id, name: v.username + "@" + asset.name, username: v.username, auth_type: pk ? "private_key" : "password",
            secret: pk ? v.private_key : v.password, passphrase: pk ? (v.passphrase || "") : "" });
        }).then(function (acc) {
          account = acc;
          if (!v.grant_me || !meId) return null;
          fctx.status("正在创建授权…");
          var body = { name: "授权-" + asset.name + "-" + me, subject_type: "user", subject: meId, asset_ids: [asset.id], account_ids: account ? [account.id] : [], actions: ["connect", "upload", "download", "copy", "paste"] };
          if (v.expire && v.expire !== "0") body.expires_at = new Date(Date.now() + Number(v.expire) * 864e5).toISOString();
          return xs().post("/bastion/rules", body);
        }).then(function () {
          fctx.close(); toast("资产 " + asset.name + " 已添加" + (account ? "，可以点「测试」验证登录" : "")); ctx.reload();
        }).catch(function (e) {
          if (asset) { fctx.close(); toast("资产已登记，但后续步骤失败：" + errText(e)); ctx.reload(); return; }
          throw new Error(errText(e));
        });
      }
    });
  }
  /* ------------------------------------------------------------------ 数据脱敏 */
  var PRESETS = [
    { v: "phone", t: "手机号", p: "1[3-9]\\d{9}", r: "1**********" },
    { v: "idcard", t: "身份证号", p: "\\b\\d{6}(19|20)\\d{2}(0[1-9]|1[0-2])\\d{2}\\d{3}[\\dXx]\\b", r: "******************" },
    { v: "bank", t: "银行卡号", p: "\\b\\d{16,19}\\b", r: "****************" },
    { v: "email", t: "邮箱", p: "[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,}", r: "***@***" },
    { v: "aksk", t: "AccessKey / 密钥", p: "(?i)(access[_-]?key|secret[_-]?key|password|passwd)\\s*[=:]\\s*\\S+", r: "$1=******" },
    { v: "custom", t: "自定义正则", p: "", r: "******" }
  ];

  function renderMask(el) {
    el.innerHTML = '<div class="xb-head"><div><h2>数据脱敏</h2><p>SSH 会话输出在发到浏览器、写入录像之前按规则替换；数据库控制台的查询结果同样脱敏。</p></div>' +
      '<div class="xb-head-act"><button class="x-btn x-btn--pri" data-new>+ 新建规则</button></div></div><div class="xb-body"><p class="x-frac">正在读取规则…</p></div>';
    var reload = function () { renderMask(el); };
    el.querySelector("[data-new]").onclick = function () { maskForm(null, reload); };
    list("/bastion/mask-rules").then(function (rs) {
      var body = el.querySelector(".xb-body");
      if (!rs.length) {
        body.innerHTML = '<div class="xb-guide"><h3>还没有脱敏规则</h3><p class="x-frac">常用的手机号、身份证、银行卡、密钥都有预设，选一个就能用；保存前可以用示例文本预览效果。</p><button class="x-btn x-btn--pri" data-new2>+ 新建规则</button></div>';
        body.querySelector("[data-new2]").onclick = function () { maskForm(null, reload); };
        return;
      }
      body.innerHTML = '<div class="xb-table"><table><thead><tr><th>规则</th><th>匹配表达式</th><th>替换为</th><th>状态</th><th>创建时间</th><th>操作</th></tr></thead><tbody>' + rs.map(function (r) {
        return "<tr><td><b>" + E(r.name) + '</b></td><td class="x-mono xb-ellipsis" title="' + E(r.pattern) + '">' + E(r.pattern) + '</td><td class="x-mono">' + E(r.replacement) + "</td>" +
          '<td><label class="xb-switch"><input type="checkbox" data-toggle="' + r.id + '"' + (r.enabled ? " checked" : "") + "><span></span>" + (r.enabled ? "启用" : "停用") + "</label></td><td>" + E(fmtTime(r.created_at)) + "</td>" +
          '<td class="xb-ops"><button class="x-act" data-edit="' + r.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + r.id + '">删除</button></td></tr>';
      }).join("") + '</tbody></table></div><p class="x-frac xb-note">规则在新建立的会话中生效；匹配在单个输出块内进行，极少数被网络拆分的长串可能漏掉，高敏感场景请配合命令过滤使用。</p>';
      var byId = function (id) { return rs.filter(function (r) { return String(r.id) === String(id); })[0]; };
      [].forEach.call(body.querySelectorAll("[data-edit]"), function (b) { b.onclick = function () { maskForm(byId(b.getAttribute("data-edit")), reload); }; });
      [].forEach.call(body.querySelectorAll("[data-toggle]"), function (b) {
        b.onchange = function () {
          var r = byId(b.getAttribute("data-toggle"));
          xs().put("/bastion/mask-rules/" + r.id, { name: r.name, pattern: r.pattern, replacement: r.replacement, enabled: b.checked })
            .then(function () { toast((b.checked ? "已启用 " : "已停用 ") + r.name); reload(); }).catch(function (e) { toast("操作失败：" + errText(e)); reload(); });
        };
      });
      [].forEach.call(body.querySelectorAll("[data-del]"), function (b) {
        b.onclick = function () {
          var r = byId(b.getAttribute("data-del"));
          confirmDelete("脱敏规则", r.name, "之后的新会话与数据库查询结果将不再脱敏这类内容。", function () {
            return xs().del("/bastion/mask-rules/" + r.id).then(function () { toast("已删除规则 " + r.name); reload(); });
          });
        };
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = failBox(e); });
  }

  function maskForm(r, done) {
    var editing = !!r;
    var guess = editing ? (PRESETS.filter(function (p) { return p.p === r.pattern; })[0] || PRESETS[PRESETS.length - 1]).v : "phone";
    root.XSFormKit.open({
      title: editing ? "编辑脱敏规则 · " + r.name : "新建脱敏规则", okText: editing ? "保存" : "创建", scene: "bastion.mask", width: "md",
      aiExample: "把输出里的 IPv4 地址都替换成 x.x.x.x",
      sections: [{ fields: [
        { k: "preset", label: "敏感数据类型", type: "select", options: PRESETS.map(function (p) { return { v: p.v, t: p.t }; }), def: guess, help: "选择预设会自动填入表达式",
          onChange: function (v, api) {
            var p = PRESETS.filter(function (x) { return x.v === v.preset; })[0];
            if (p && p.v !== "custom") api.set({ pattern: p.p, replacement: p.r, name: v.name && !/脱敏$/.test(v.name) ? v.name : p.t + "脱敏" });
          } },
        { k: "name", label: "规则名称", required: true, placeholder: "手机号脱敏" },
        { k: "pattern", label: "匹配表达式（正则）", required: true, type: "text", placeholder: "1[3-9]\\d{9}", help: "Go RE2 语法；不能匹配空字符串" },
        { k: "replacement", label: "替换为", placeholder: "******", help: "可用 $1 引用分组" },
        { k: "enabled", label: "启用", type: "switch", def: true },
        { k: "sample", label: "示例文本（只用于预览，不保存）", type: "textarea", placeholder: "联系人 13812345678，password=Abc123" }
      ] }],
      values: editing ? { name: r.name, pattern: r.pattern, replacement: r.replacement, enabled: r.enabled, preset: guess } : { preset: "phone", name: "手机号脱敏", pattern: PRESETS[0].p, replacement: PRESETS[0].r, enabled: true },
      toObject: function (v) { return { name: v.name, pattern: v.pattern, replacement: v.replacement || "******", enabled: v.enabled !== false }; },
      fromObject: function (o, v) { return Object.assign({}, v, o); },
      validate: function (v) { return !v.name ? "请填写规则名称" : !v.pattern ? "请填写匹配表达式" : ""; },
      actions: [{ label: "预览效果", run: function (v, obj, ctx) {
        if (!v.sample) { ctx.status("先在「示例文本」里填一段内容"); return Promise.resolve(); }
        return xs().post("/bastion/mask-rules/preview", { sample: v.sample, pattern: v.pattern, replacement: v.replacement }).then(function (res) { ctx.status("预览：" + res.output); })
          .catch(function (e) { ctx.status("预览失败：" + errText(e)); });
      } }],
      submit: function (v, obj, ctx) {
        var p = editing ? xs().put("/bastion/mask-rules/" + r.id, obj) : xs().post("/bastion/mask-rules", obj);
        return p.then(function () { ctx.close(); toast(editing ? "规则已更新" : "规则已创建，新会话立即生效"); done(); }).catch(function (e) { throw new Error(errText(e)); });
      }
    });
  }

  /* ------------------------------------------------------------------ 登录安全 */
  function renderLsec(el) {
    el.innerHTML = '<div class="xb-head"><div><h2>登录安全</h2><p>堡垒机与平台共用一套登录规则，在「设置 → 安全策略」统一修改，保存后立即生效。</p></div>' +
      '<div class="xb-head-act"><button class="x-btn x-btn--pri" data-go>前往修改</button></div></div><div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    el.querySelector("[data-go]").onclick = function () {
      location.hash = "#/settings";
      setTimeout(function () { var t = document.querySelector('[data-cfg-tab="security"]'); if (t) t.click(); }, 400);
    };
    var paint = function (s) {
      var body = el.querySelector(".xb-body");
      if (!body) return;
      var v = s && (s.value !== undefined ? s.value : s);
      var p = Object.assign({ password_min_length: 8, password_min_classes: 3, max_login_failures: 5, lockout_minutes: 15, session_hours: 168 }, v && typeof v === "object" ? v : {});
      var items = [
        ["密码最小长度", p.password_min_length + " 位"],
        ["密码复杂度", "大小写字母、数字、符号中至少 " + p.password_min_classes + " 类"],
        ["连续失败锁定", p.max_login_failures + " 次失败后锁定 " + p.lockout_minutes + " 分钟"],
        ["登录会话有效期", p.session_hours + " 小时"],
        ["堡垒机连接条件", "连接权限 + 生效的资产授权 + 环境范围允许 + 登录 ACL 放行"],
        ["会话录像", "所有 SSH 会话强制录像，录像存储不可用时拒绝连接"]
      ];
      body.innerHTML = (s ? "" : '<p class="x-frac" style="margin:0 0 10px">尚未自定义，当前为平台默认值。</p>') +
        '<div class="xb-kv">' + items.map(function (i) { return "<div><span>" + E(i[0]) + "</span><b>" + E(i[1]) + "</b></div>"; }).join("") + "</div>";
    };
    xs().get("/settings/security.policy").then(paint, function () { paint(null); });
  }


  /* ------------------------------------------------------------------ Web 终端：命令审批等待提示 */
  // state 来自后端 WebSocket 消息 {type:"approval", data: JSON}：submitting | pending | approved | rejected | cancelled | timeout | error
  function parseApproval(data) {
    if (!data) return null;
    if (typeof data === "object") return data;
    try { return JSON.parse(data); } catch (e) { return null; }
  }
  function approvalBanner(st) {
    if (!st || !st.state) return "";
    var cmd = st.command ? '<code class="x-mono">' + E(st.command.length > 120 ? st.command.slice(0, 120) + "…" : st.command) + "</code>" : "";
    var no = st.ticket_id ? "审批单 #" + E(st.ticket_id) : "";
    var map = {
      submitting: ["xb-apv--wait", "⏳", "命令命中「" + E(st.rule || "需审批") + "」规则，正在提交审批…"],
      pending: ["xb-apv--wait", "⏳", "命令等待审批（" + no + (st.deadline ? "，" + E(fmtTime(st.deadline)) + " 前未处理将自动撤回" : "") + "）。审批人可在「工单 → 命令复核」处理，按 Ctrl+C 放弃。"],
      approved: ["xb-apv--ok", "✓", E(st.note || "审批已通过，命令已发送到主机")],
      rejected: ["xb-apv--bad", "✕", E(st.note || "审批未通过，命令已取消")],
      timeout: ["xb-apv--bad", "✕", E(st.note || "审批超时，命令已取消")],
      cancelled: ["xb-apv--muted", "·", E(st.note || "已放弃，审批单已撤回")],
      error: ["xb-apv--bad", "✕", E(st.note || "提交审批失败，命令已取消")]
    }[st.state];
    if (!map) return "";
    return '<div class="xb-apv ' + map[0] + '" role="status" aria-live="polite"><b>' + map[1] + "</b><div><div>" + map[2] + "</div>" + (cmd ? '<div class="xb-apv-cmd">' + cmd + "</div>" : "") + "</div>" +
      (st.state === "pending" ? '<a class="x-act" href="#/bastion/cmdap">查看工单</a>' : "") + "</div>";
  }
  // 在终端页里就地刷新提示（不重绘终端，避免丢失输入焦点）；结束态 8 秒后自动收起
  function paintApproval(s) {
    if (!s) return;
    var box = document.querySelector('[data-lapv="' + s.id + '"]');
    if (box) box.innerHTML = approvalBanner(s.approval);
    var st = s.approval && s.approval.state;
    if (st && st !== "pending" && st !== "submitting") {
      var mark = s.approval;
      setTimeout(function () { if (s.approval === mark) { s.approval = null; var b = document.querySelector('[data-lapv="' + s.id + '"]'); if (b) b.innerHTML = ""; } }, 8000);
    }
  }

  /* ------------------------------------------------------------------ 路由与分文件加载 */
  var PAGES = { assets: renderAssets, mask: renderMask, lsec: renderLsec };
  // 由本模块实现的页面（含另外两个文件中的页面）
  var KEYS = ["dash", "assets", "platforms", "accounts", "pwd", "policy", "cmd", "acl", "mask", "lsec",
    "myassets", "dbc", "sess", "replay", "cmdlog", "loginlog", "cmdap", "db"];
  var me = document.currentScript, base = me && me.src ? me.src.replace(/[^/?#]*([?#].*)?$/, "") : "";
  var parts = null;
  function loadParts() {
    if (parts) return parts;
    parts = Promise.all(["xs-bastion-admin.js", "xs-bastion-ops.js"].map(function (f) {
      return new Promise(function (resolve, reject) {
        var s = document.createElement("script");
        s.src = base + f;
        s.onload = resolve;
        s.onerror = function () { reject(new Error("页面脚本 " + f + " 加载失败")); };
        document.head.appendChild(s);
      });
    }));
    parts.catch(function () { parts = null; });
    return parts;
  }
  function register(k, fn) { PAGES[k] = fn; }
  function has(k) { return KEYS.indexOf(k) >= 0; }
  function render(k, el, hooks) {
    if (PAGES[k]) return PAGES[k](el, hooks || {});
    el.innerHTML = '<p class="x-frac">正在加载…</p>';
    loadParts().then(function () {
      if (!el.isConnected) return;
      if (PAGES[k]) PAGES[k](el, hooks || {});
      else el.innerHTML = '<p class="x-aerr" role="alert">该页面暂不可用。</p>';
    }).catch(function (e) { if (el.isConnected) el.innerHTML = failBox(e, "加载失败"); });
  }
  if (root.document && root.document.readyState !== "loading") setTimeout(loadParts, 0);
  else if (root.document) root.document.addEventListener("DOMContentLoaded", function () { loadParts(); });

  root.XSBastion = {
    has: has, render: render, register: register, util: U,
    renderAssets: renderAssets, renderMask: renderMask, renderLsec: renderLsec,
    parseApproval: parseApproval, approvalBanner: approvalBanner, paintApproval: paintApproval
  };
})(window);
