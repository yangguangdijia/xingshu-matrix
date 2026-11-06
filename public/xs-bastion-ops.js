/*
 * 星枢 Matrix 堡垒机：工作台（我的资产、数据库控制台）、审计台（会话、录像回放、命令记录、登录日志）、工单（命令复核、SQL 工单）。
 * 由 xs-bastion.js 自动加载，通过 XSBastion.register 注册页面。
 */
(function (root) {
  "use strict";
  var B = root.XSBastion;
  if (!B) return;
  var U = B.util, E = U.E, xs = U.xs, toast = U.toast, errText = U.errText, list = U.list, soft = U.soft, fmtTime = U.fmtTime,
    tag = U.tag, head = U.head, table = U.table, on = U.on, byId = U.byId, go = U.go;
  var ACT_ZH = { connect: "连接", upload: "上传", download: "下载", copy: "复制", paste: "粘贴" };
  var LEVEL = { read: ["只读", "x-green"], write: ["变更", "x-orange"], high: ["高危", "x-red"], forbidden: ["禁止", "x-grey"] };
  function levelTag(l) { var x = LEVEL[l] || [l, "x-grey"]; return tag(x[0], x[1]); }
  function alive(el) { return el && el.isConnected; }
  function rangeFrom(k) {
    var d = new Date();
    if (k === "today") { d.setHours(0, 0, 0, 0); return d.toISOString(); }
    if (k === "7d") return new Date(Date.now() - 7 * 864e5).toISOString();
    if (k === "30d") return new Date(Date.now() - 30 * 864e5).toISOString();
    return "";
  }
  var RANGES = [{ v: "today", t: "今天" }, { v: "7d", t: "近 7 天" }, { v: "30d", t: "近 30 天" }, { v: "", t: "全部" }];

  /* ================================================================ 我的资产 */
  var MY = { q: "" };
  function renderMyAssets(el, hooks) {
    el.innerHTML = head("我的资产", "当前账号经授权可以连接的主机与数据库。", '<a class="x-btn" href="#/bastion/req">申请访问</a><button class="x-btn" data-refresh>刷新</button>') +
      '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    el.querySelector("[data-refresh]").onclick = function () { renderMyAssets(el, hooks); };
    Promise.all([list("/bastion/my/assets"), soft("/bastion/my/databases")]).then(function (r) {
      var hosts = r[0], dbs = r[1] || [];
      var draw = function () {
        var q = MY.q.toLowerCase(), f = function (g) { return !q || [g.asset_name, g.host, g.username, g.env].join(" ").toLowerCase().indexOf(q) >= 0; };
        var acts = function (g) { return (g.actions || []).map(function (a) { return tag(ACT_ZH[a] || a, a === "connect" ? "x-blue" : "x-grey"); }).join(" "); };
        var exp = function (g) { return g.expires_at ? E(fmtTime(g.expires_at)) + " 到期" : '<span class="x-frac">长期</span>'; };
        var h = hosts.filter(f).map(function (g, i) {
          return ["<b>" + E(g.asset_name) + '</b><div class="x-frac x-mono">' + E(g.host) + ":" + E(g.port) + "</div>", tag(U.envName(g.env), U.ENV_CLS[g.env]), '<span class="x-mono">' + E(g.username) + "</span>", acts(g), exp(g),
            '<button class="x-btn x-btn--pri x-btn--s" data-ssh="' + hosts.indexOf(g) + '">连接</button>'];
        });
        var d = dbs.filter(f).map(function (g) {
          return ["<b>" + E(g.asset_name) + '</b><div class="x-frac x-mono">' + E(g.host) + ":" + E(g.port) + "</div>", E(U.PROTO_NAME[g.protocol] || g.protocol), tag(U.envName(g.env), U.ENV_CLS[g.env]),
            '<span class="x-mono">' + E(g.username) + "</span>", exp(g), '<button class="x-btn x-btn--pri x-btn--s" data-db="' + dbs.indexOf(g) + '">打开控制台</button>'];
        });
        el.querySelector(".xb-body").innerHTML = '<div class="xb-toolbar"><input class="x-input" data-q placeholder="搜索资产、地址、账号" value="' + E(MY.q) + '"><span class="x-frac">主机 ' + hosts.length + " · 数据库 " + dbs.length + "</span></div>" +
          '<section class="xb-card"><h3>主机</h3>' + table(["资产", "环境", "账号", "允许的动作", "有效期", ""], h, hosts.length ? "没有符合条件的主机" : "还没有被授权的主机，可以点右上角「申请访问」") + "</section>" +
          '<section class="xb-card"><h3>数据库</h3>' + table(["资产", "类型", "环境", "账号", "有效期", ""], d, dbs.length ? "没有符合条件的数据库" : "还没有被授权的数据库") + "</section>";
        var qi = el.querySelector("[data-q]");
        qi.oninput = function () { MY.q = qi.value; clearTimeout(qi._t); qi._t = setTimeout(function () { draw(); var n = el.querySelector("[data-q]"); n.focus(); n.setSelectionRange(n.value.length, n.value.length); }, 250); };
        on(el, "data-ssh", function (i) {
          var g = hosts[+i];
          if (hooks && hooks.ssh) hooks.ssh(g);
          else if (hooks && hooks.connect) hooks.connect({ id: g.asset_id, name: g.asset_name, host: g.host });
          else toast("Web 终端不可用");
        });
        on(el, "data-db", function (i) { var g = dbs[+i]; U.handoff("dbc", { asset_id: g.asset_id, account_id: g.account_id }); go("dbc"); });
      };
      draw();
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }

  /* ================================================================ 数据库控制台 */
  var DBC = { key: "", database: "", sql: "", limit: 200, timeout: 15, conn: null, result: null };
  var READ_KW = { select: 1, show: 1, describe: 1, desc: 1, explain: 1, "with": 1, values: 1, table: 1 };
  // 仅用于界面提示（是否需要填写申请原因），最终分级以后端为准
  function guessRead(sql) {
    var s = String(sql || "").replace(/\/\*[\s\S]*?\*\//g, " ").replace(/--[^\n]*/g, " ").replace(/#[^\n]*/g, " ").trim().toLowerCase();
    var kw = s.split(/\s+/)[0] || "";
    if (!READ_KW[kw]) return false;
    if (/\bfor\s+(update|share)\b/.test(s) || (kw === "explain" && /\banalyze\b/.test(s))) return false;
    if (kw === "with" && /\b(insert|update|delete|merge)\b/.test(s)) return false;
    return true;
  }
  function renderDbc(el) {
    var pre = U.handoff("dbc");
    el.innerHTML = head("数据库控制台", "对已授权的 MySQL / PostgreSQL 资产执行 SQL：只读查询在只读事务中执行并脱敏；变更语句提交工单审批后执行；禁止语句直接拒绝。所有执行都记入审计。") +
      '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    list("/bastion/my/databases").then(function (grants) {
      if (!alive(el)) return;
      var key = function (g) { return g.asset_id + ":" + g.account_id; };
      if (pre) {
        var hit = grants.filter(function (g) { return g.asset_id === pre.asset_id && (!pre.account_id || g.account_id === pre.account_id); })[0];
        if (hit && key(hit) !== DBC.key) { DBC.key = key(hit); DBC.conn = null; DBC.database = ""; DBC.result = null; }
        else if (!hit) toast("你没有该数据库的生效授权");
      }
      if (!grants.length) {
        el.querySelector(".xb-body").innerHTML = '<div class="xb-guide"><h3>还没有被授权的数据库</h3><ol><li>管理员在「资产列表」添加 MySQL / PostgreSQL 资产并托管账号</li><li>在「资产授权」把资产授权给你</li></ol><a class="x-btn" href="#/bastion/req">申请访问</a></div>';
        return;
      }
      if (!grants.some(function (g) { return key(g) === DBC.key; })) { DBC.key = key(grants[0]); DBC.conn = null; }
      drawDbc(el, grants, key);
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function drawDbc(el, grants, key) {
    var g = grants.filter(function (x) { return key(x) === DBC.key; })[0], c = DBC.conn;
    var opt = function (arr, cur) { return arr.map(function (o) { return '<option value="' + E(o.v) + '"' + (String(o.v) === String(cur) ? " selected" : "") + ">" + E(o.t) + "</option>"; }).join(""); };
    var dbs = c ? (c.databases || []).map(function (d) { return { v: d, t: d }; }) : [];
    if (c && DBC.database && dbs.every(function (d) { return d.v !== DBC.database; })) dbs.unshift({ v: DBC.database, t: DBC.database });
    var body = el.querySelector(".xb-body");
    body.innerHTML = '<section class="xb-card"><div class="xb-dbbar">' +
      '<label><span>资产 / 账号</span><select class="x-select" data-g>' + opt(grants.map(function (x) { return { v: key(x), t: x.asset_name + " · " + x.username + "（" + (U.PROTO_NAME[x.protocol] || x.protocol) + " · " + U.envName(x.env) + "）" }; }), DBC.key) + "</select></label>" +
      '<label><span>数据库</span>' + (c ? '<select class="x-select" data-dbn><option value="">默认库</option>' + opt(dbs, DBC.database) + "</select>" : '<select class="x-select" disabled><option>连接后选择</option></select>') + "</label>" +
      '<button class="x-btn ' + (c ? "" : "x-btn--pri") + '" data-connect>' + (c ? "重新连接" : "连接") + "</button>" +
      (c ? '<span class="x-frac xb-dbver" title="' + E(c.version) + '">' + tag("已连接", "x-green") + " " + E(String(c.version || "").split(/[\s,]/).slice(0, 2).join(" ")) + "</span>" : '<span class="x-frac">连接时校验授权与登录 ACL，并记入登录日志</span>') +
      "</div></section>" +
      '<section class="xb-card"' + (c ? "" : " hidden") + '><textarea class="x-input xb-sql" data-sql rows="8" spellcheck="false" placeholder="SELECT * FROM users LIMIT 10&#10;&#10;Ctrl + Enter 执行">' + E(DBC.sql) + "</textarea>" +
      '<div class="xb-dbops"><label class="x-frac">最多返回 <select class="x-select" data-limit>' + opt([50, 100, 200, 500, 1000].map(function (n) { return { v: n, t: n + " 行" }; }), DBC.limit) + "</select></label>" +
      '<label class="x-frac">超时 <select class="x-select" data-timeout>' + opt([5, 15, 30, 60].map(function (n) { return { v: n, t: n + " 秒" }; }), DBC.timeout) + "</select></label>" +
      '<span class="x-frac" data-hint></span><button class="x-btn x-btn--pri" data-run style="margin-left:auto">执行</button></div></section>' +
      '<section class="xb-card" data-res' + (c ? "" : " hidden") + ">" + resultHTML(DBC.result) + "</section>" +
      '<section class="xb-card"><h3>我的执行记录 <button class="x-act" data-hist>刷新</button></h3><div data-histbox><p class="x-frac">正在读取…</p></div></section>';
    var redraw = function () { drawDbc(el, grants, key); };
    body.querySelector("[data-g]").onchange = function () { DBC.key = this.value; DBC.conn = null; DBC.database = ""; DBC.result = null; redraw(); };
    var dn = body.querySelector("[data-dbn]"); if (dn) dn.onchange = function () { DBC.database = this.value; };
    body.querySelector("[data-connect]").onclick = function () {
      var b = this; b.disabled = true; b.textContent = "连接中…";
      xs().post("/bastion/db/connect", { asset_id: g.asset_id, account_id: g.account_id, database: DBC.database }).then(function (r) {
        DBC.conn = r; if (!DBC.database) DBC.database = r.default_database || ""; redraw();
        var ta = el.querySelector("[data-sql]"); if (ta) ta.focus();
      }).catch(function (e) { b.disabled = false; b.textContent = "连接"; toast("连接失败：" + errText(e)); });
    };
    var ta = body.querySelector("[data-sql]"), hint = body.querySelector("[data-hint]");
    var showHint = function () {
      DBC.sql = ta.value;
      hint.textContent = !ta.value.trim() ? "" : guessRead(ta.value) ? "只读查询：在只读事务中直接执行" : "变更语句：需要填写原因并提交工单，审批通过后执行";
    };
    ta.oninput = showHint; showHint();
    ta.onkeydown = function (e) { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); run(); } };
    body.querySelector("[data-limit]").onchange = function () { DBC.limit = +this.value; };
    body.querySelector("[data-timeout]").onchange = function () { DBC.timeout = +this.value; };
    var send = function (reason) {
      var res = el.querySelector("[data-res]"), btn = el.querySelector("[data-run]");
      btn.disabled = true; res.innerHTML = '<p class="x-frac">正在执行…</p>';
      return xs().post("/bastion/db/query", { asset_id: g.asset_id, account_id: g.account_id, database: DBC.database, sql: ta.value, reason: reason || "", limit: DBC.limit, timeout_sec: DBC.timeout },
        { timeoutMs: (DBC.timeout + 15) * 1000 }).then(function (r) {
        DBC.result = r;
      }).catch(function (e) {
        var d = e && (e.approval || (e.details && e.details.details));
        DBC.result = d && d.status ? d : { status: "failed", error: errText(e) };
      }).then(function () {
        btn.disabled = false;
        if (!alive(el)) return;
        res.innerHTML = resultHTML(DBC.result);
        loadHist();
      });
    };
    var run = function () {
      if (!ta.value.trim()) { toast("请输入 SQL"); return; }
      if (guessRead(ta.value)) { send(""); return; }
      U.confirmBox("提交 SQL 变更工单", "这条语句会修改数据或结构，需要审批通过后才会执行。<br>资产：<b>" + E(g.asset_name) + "</b>　数据库：<b>" + E(DBC.database || "默认库") + "</b>",
        "提交审批", function (reason) { return send(reason); }, { primary: true, input: { label: "申请原因", required: true, placeholder: "例如：修复订单 #1234 状态，已在测试环境验证" } });
    };
    body.querySelector("[data-run]").onclick = run;
    var loadHist = function () {
      var box = el.querySelector("[data-histbox]"); if (!box) return;
      list("/bastion/my/db-queries", { size: 10 }).then(function (qs) {
        if (!alive(box)) return;
        box.innerHTML = table(["时间", "资产", "分级", "语句摘要", "结果", "工单"], qs.map(function (q) {
          return [E(fmtTime(q.created_at, true)), E(q.asset_name) + (q.database ? '<div class="x-frac">' + E(q.database) + "</div>" : ""), levelTag(q.level),
            '<code class="x-mono xb-ellipsis" title="' + E(q.summary) + '">' + E(q.summary) + "</code>", queryStatus(q), q.ticket_id ? '<a class="x-act" href="#/bastion/db">#' + E(q.ticket_id) + "</a>" : "—"];
        }), "还没有执行记录");
      }).catch(function (e) { box.innerHTML = U.failBox(e); });
    };
    body.querySelector("[data-hist]").onclick = loadHist;
    loadHist();
  }
  function queryStatus(q) {
    var m = { executed: ["已执行", "x-green"], failed: ["失败", "x-red"], rejected: ["已拒绝", "x-grey"], pending: ["待审批", "x-orange"], denied: ["无权限", "x-grey"] }[q.status] || [q.status, "x-grey"];
    return tag(m[0], m[1]) + (q.status === "executed" && q.level !== "read" ? '<div class="x-frac">影响 ' + E(q.rows_affected) + " 行</div>" : q.error ? '<div class="x-frac xb-wrap">' + E(q.error) + "</div>" : "");
  }
  function cell(v) {
    if (v === null || v === undefined) return '<span class="x-frac">NULL</span>';
    if (typeof v === "object") return E(JSON.stringify(v));
    return E(v);
  }
  function resultHTML(r) {
    if (!r) return '<p class="x-frac">执行结果会显示在这里。</p>';
    var meta = (r.level ? levelTag(r.level) + " " : "") + (r.risk ? '<span class="x-frac">' + E(r.risk) + "</span>" : "");
    if (r.status === "pending") return '<div class="xb-apv xb-apv--wait"><b>⏳</b><div><div>已提交 SQL 变更工单 #' + E(r.ticket_id) + "，审批通过后自动执行。</div><div class=\"x-frac\">" + meta + '</div></div><a class="x-act" href="#/bastion/db">查看工单</a></div>';
    if (r.status === "rejected") return '<div class="xb-apv xb-apv--bad"><b>✕</b><div><div>' + E(r.error || "已拒绝") + '</div><div class="x-frac">' + meta + "</div></div></div>";
    if (r.status === "failed") return '<div class="xb-apv xb-apv--bad"><b>✕</b><div><div>执行失败：' + E(r.error || "未知错误") + '</div><div class="x-frac">' + meta + "</div></div></div>";
    var res = r.result || {};
    var info = '<p class="x-frac xb-dbmeta">' + meta + " · " + (res.columns && res.columns.length ? "返回 " + E(res.row_count) + " 行" + (res.truncated ? "（已截断，只显示前 " + E(res.row_count) + " 行）" : "") : "影响 " + E(res.rows_affected || 0) + " 行") +
      " · 耗时 " + E(res.duration_ms) + " ms · 审计记录 #" + E(r.query_id) + "</p>";
    if (!res.columns || !res.columns.length) return info;
    return info + '<div class="xb-table xb-grid"><table><thead><tr><th>#</th>' + res.columns.map(function (c) { return "<th>" + E(c) + "</th>"; }).join("") + "</tr></thead><tbody>" +
      (res.rows || []).map(function (row, i) { return '<tr><td class="x-frac">' + (i + 1) + "</td>" + row.map(function (v) { return "<td>" + cell(v) + "</td>"; }).join("") + "</tr>"; }).join("") + "</tbody></table></div>";
  }

  /* ================================================================ 会话（在线 / 历史） */
  var SS = { tab: "active", status: "", user: "", page: 1, timer: null };
  function sessStatus(s) {
    var m = { active: ["在线", "x-green"], closed: ["已结束", "x-grey"], killed: ["已中断", "x-orange"], failed: ["异常", "x-red"] }[s.status] || [s.status, "x-grey"];
    return tag(m[0], m[1]) + (s.error ? '<div class="x-frac xb-wrap">' + E(s.error) + "</div>" : "");
  }
  function renderSess(el) {
    clearInterval(SS.timer);
    el.innerHTML = head("会话", "经堡垒机建立的 SSH 会话。在线会话可强制中断；结束的会话可回放录像、查看命令。", '<button class="x-btn" data-refresh>刷新</button>') +
      U.segs("stab", [{ v: "active", t: "在线会话" }, { v: "history", t: "历史会话" }], SS.tab) + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderSess(el); };
    el.querySelector("[data-refresh]").onclick = reload;
    on(el, "data-stab", function (k) { SS.tab = k; SS.page = 1; reload(); });
    var size = 20, q = { page: SS.page, size: size };
    if (SS.tab === "active") q.status = "active"; else if (SS.status) q.status = SS.status;
    if (SS.user) q.username = SS.user;
    xs().get("/bastion/sessions", q).then(function (p) {
      if (!alive(el)) return;
      var ss = U.items(p).filter(function (s) { return SS.tab === "active" || SS.status || s.status !== "active"; });
      var rows = ss.map(function (s) {
        var dur = (s.ended_at ? new Date(s.ended_at) : new Date()) - new Date(s.started_at);
        return [E(s.username), "<b>" + E(s.asset_name) + "</b>", '<span class="x-mono">' + E(s.account_user) + "</span>", E(s.protocol.toUpperCase()), '<span class="x-mono">' + E(s.client_ip || "—") + "</span>",
          E(fmtTime(s.started_at, true)), '<span data-dur="' + (s.ended_at ? "" : E(s.started_at)) + '">' + E(U.fmtDur(dur)) + "</span>", sessStatus(s),
          s.has_recording ? tag("可回放", "x-blue") : '<span class="x-frac">' + (s.status === "active" ? "录制中" : "无") + "</span>",
          '<span class="xb-ops">' + (s.status === "active" ? '<button class="x-act x-act--danger" data-kill="' + s.id + '">中断</button>' : "") +
          (s.has_recording ? '<button class="x-act" data-play="' + s.id + '">回放</button>' : "") + '<button class="x-act" data-cmds="' + s.id + '">命令</button></span>'];
      });
      var tools = SS.tab === "history" ? '<div class="xb-toolbar"><div class="xb-tools"><input class="x-input" data-user placeholder="按用户名筛选（精确）" value="' + E(SS.user) + '">' +
        '<select class="x-select" data-st aria-label="按状态筛选">' + [["", "全部状态"], ["closed", "已结束"], ["killed", "已中断"], ["failed", "异常"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === SS.status ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select></div></div>" : "";
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + tools + table(["用户", "资产", "账号", "协议", "来源 IP", "开始时间", "时长", "状态", "录像", "操作"], rows, SS.tab === "active" ? "当前没有在线会话" : "没有会话记录") +
        (SS.tab === "history" ? U.pager(p.total, SS.page, size) : '<p class="x-frac xb-note">每 10 秒自动刷新。中断会通知会话所在实例断开 SSH 并保存录像。</p>') + "</div>";
      var ui = el.querySelector("[data-user]");
      if (ui) ui.onkeydown = function (e) { if (e.key === "Enter") { SS.user = ui.value.trim(); SS.page = 1; reload(); } };
      var st = el.querySelector("[data-st]"); if (st) st.onchange = function () { SS.status = this.value; SS.page = 1; reload(); };
      on(el, "data-pg", function (n) { SS.page = Math.max(1, +n); reload(); });
      on(el, "data-kill", function (id) {
        var s = byId(ss, id);
        U.confirmBox("中断会话", "立即断开 <b>" + E(s.username) + "</b> 在 " + E(s.account_user + "@" + s.asset_name) + " 上的会话？正在执行的命令结果无法保证。", "中断会话", function () {
          return xs().post("/bastion/sessions/" + s.id + "/kill", {}).then(function (r) { toast(r && r.killed ? "会话已中断" : "已请求中断，等待会话所在实例确认"); reload(); });
        });
      });
      on(el, "data-play", function (id) { U.handoff("replay", +id); go("replay"); });
      on(el, "data-cmds", function (id) { showCommands(byId(ss, id)); });
      if (SS.tab === "active") SS.timer = setInterval(function () { if (!alive(el)) { clearInterval(SS.timer); return; } if (!document.querySelector(".xb-confirm")) reload(); }, 10000);
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function cmdAction(a) {
    var m = { allow: ["放行", "x-green"], deny: ["拦截", "x-red"], confirmed: ["确认后执行", "x-orange"], cancelled: ["已取消", "x-grey"], approved: ["审批通过", "x-blue"], rejected: ["审批未通过", "x-red"] }[a] || [a, "x-grey"];
    return tag(m[0], m[1]);
  }
  function showCommands(s) {
    var m = U.modal("会话命令 · " + s.username + " → " + s.account_user + "@" + s.asset_name, '<p class="x-frac">正在读取…</p>', true);
    xs().get("/bastion/sessions/" + s.id).then(function (d) {
      var cmds = d.commands || [];
      m.body.innerHTML = '<p class="x-frac">会话 #' + E(s.id) + " · 开始 " + E(fmtTime(s.started_at, true)) + " · 来源 " + E(s.client_ip || "—") + "</p>" +
        table(["时间", "命令", "结果"], cmds.map(function (c) { return [E(fmtTime(c.created_at, true)), '<code class="x-mono xb-wrap">' + E(c.command) + "</code>", cmdAction(c.action)]; }), "此会话没有记录到命令");
    }).catch(function (e) { m.body.innerHTML = U.failBox(e); });
  }

  /* ================================================================ 会话录像回放 */
  var RP = { player: null };
  function stripAnsi(t) {
    return t.replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "").replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "").replace(/\x1b[()][0-9A-Za-z]/g, "").replace(/\x1b[@-Z\\-_=>78]/g, "");
  }
  function Screen() { this.reset(); }
  Screen.prototype.reset = function () { this.lines = [[]]; this.cx = 0; this.cy = 0; };
  Screen.prototype.feed = function (chunk) {
    if (/\x1b\[[23]J/.test(chunk)) this.reset();
    var t = stripAnsi(chunk);
    for (var i = 0; i < t.length; i++) {
      var ch = t[i];
      if (ch === "\n") { this.cy++; this.cx = 0; if (!this.lines[this.cy]) this.lines[this.cy] = []; }
      else if (ch === "\r") this.cx = 0;
      else if (ch === "\b") this.cx = Math.max(0, this.cx - 1);
      else if (ch === "\t") this.cx += 8 - (this.cx % 8);
      else if (ch < " ") continue;
      else { var l = (this.lines[this.cy] = this.lines[this.cy] || []); while (l.length < this.cx) l.push(" "); l[this.cx] = ch; this.cx++; }
    }
    if (this.lines.length > 2000) { var cut = this.lines.length - 1500; this.lines = this.lines.slice(cut); this.cy -= cut; }
  };
  Screen.prototype.text = function () { return this.lines.map(function (l) { return (l || []).join("").replace(/\s+$/, ""); }).join("\n"); };

  function parseCast(text) {
    var lines = text.split("\n").filter(Boolean), hdr = {}, ev = [];
    try { hdr = JSON.parse(lines[0]); } catch (e) { throw new Error("录像格式不正确"); }
    for (var i = 1; i < lines.length; i++) { try { var x = JSON.parse(lines[i]); if (Array.isArray(x) && x[1] === "o") ev.push([+x[0], x[2]]); } catch (e) { /* 末行可能未写完 */ } }
    return { header: hdr, events: ev, duration: ev.length ? ev[ev.length - 1][0] : 0 };
  }
  function fetchCast(id) {
    var X = xs(), hdrs = X.token && X.token() ? { Authorization: "Bearer " + X.token() } : {};
    return fetch(X.base + "/bastion/sessions/" + encodeURIComponent(id) + "/recording", { headers: hdrs, credentials: "same-origin" }).then(function (r) {
      if (!r.ok) throw new Error(r.status === 409 ? "录像不可用或尚未写完" : r.status === 403 ? "没有查看录像的权限" : "录像读取失败（HTTP " + r.status + "）");
      return r.text();
    });
  }
  function renderReplay(el) {
    if (RP.player) RP.player.stop();
    var want = U.handoff("replay");
    el.innerHTML = head("会话录像", "回放 SSH 会话的完整输出（脱敏后），可按命令时间线跳转。", '<button class="x-btn" data-refresh>刷新列表</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    el.querySelector("[data-refresh]").onclick = function () { renderReplay(el); };
    xs().get("/bastion/sessions", { size: 50 }).then(function (p) {
      if (!alive(el)) return;
      var ss = U.items(p).filter(function (s) { return s.has_recording; });
      var cur = want ? byId(U.items(p), want) || { id: want } : ss[0];
      el.querySelector(".xb-body").innerHTML = '<div class="xb-split xb-split--wide"><aside class="xb-tree" aria-label="录像列表"><div class="xb-tree-h"><span>最近有录像的会话</span></div>' +
        (ss.length ? ss.map(function (s) {
          return '<button type="button" class="xb-node xb-node--2' + (cur && String(cur.id) === String(s.id) ? " on" : "") + '" data-rec="' + s.id + '"><span><b>' + E(s.asset_name) + "</b> · " + E(s.username) +
            '<small>' + E(fmtTime(s.started_at)) + "</small></span></button>";
        }).join("") : '<p class="x-frac" style="padding:8px">还没有可回放的录像</p>') + '</aside><section class="xb-main" data-player></section></div>';
      on(el, "data-rec", function (id) { var s = byId(ss, id); [].forEach.call(el.querySelectorAll("[data-rec]"), function (b) { b.classList.toggle("on", b === el.querySelector('[data-rec="' + id + '"]')); }); openPlayer(el.querySelector("[data-player]"), s); });
      if (cur) openPlayer(el.querySelector("[data-player]"), cur);
      else el.querySelector("[data-player]").innerHTML = '<p class="xb-empty">选择左侧的会话开始回放</p>';
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function openPlayer(box, s) {
    if (RP.player) RP.player.stop();
    box.innerHTML = '<p class="x-frac">正在读取录像…</p>';
    Promise.all([fetchCast(s.id), xs().get("/bastion/sessions/" + s.id).catch(function () { return null; })]).then(function (r) {
      if (!alive(box)) return;
      var cast = parseCast(r[0]), detail = r[1] || {}, sess = detail.session || s, cmds = detail.commands || [];
      var t0 = new Date(sess.started_at).getTime(), castStart = (cast.header.timestamp || 0) * 1000 || t0;
      var marks = cmds.map(function (c) { return { t: Math.max(0, (new Date(c.created_at).getTime() - castStart) / 1000), c: c }; });
      var dur = Math.max(cast.duration, marks.length ? marks[marks.length - 1].t : 0, 0.1);
      box.innerHTML = '<div class="xb-rp-head"><b>' + E(sess.username || "") + " → " + E((sess.account_user || "") + "@" + (sess.asset_name || "")) + '</b><span class="x-frac">会话 #' + E(sess.id) + " · " + E(fmtTime(sess.started_at, true)) +
        " · 时长 " + E(U.fmtDur(dur * 1000)) + '</span><a class="x-act" data-dl>下载录像</a></div>' +
        '<div class="xb-rp"><pre class="xb-rp-out" data-out tabindex="0" aria-label="录像回放画面"></pre><ol class="xb-rp-tl" aria-label="命令时间线">' +
        (marks.length ? marks.map(function (m, i) { return '<li><button type="button" data-seek="' + i + '"><span class="x-mono">' + E(mmss(m.t)) + "</span><code>" + E(m.c.command) + "</code>" + cmdAction(m.c.action) + "</button></li>"; }).join("") : '<li class="x-frac">没有命令记录</li>') +
        '</ol></div><div class="xb-rp-ctl"><button class="x-btn x-btn--pri" data-play>播放</button><input type="range" data-bar min="0" max="' + dur + '" step="0.1" value="0" aria-label="播放进度">' +
        '<span class="x-mono" data-time>00:00 / ' + mmss(dur) + '</span><label class="x-frac">倍速 <select class="x-select" data-spd>' + [0.5, 1, 2, 4, 8].map(function (v) { return '<option value="' + v + '"' + (v === 1 ? " selected" : "") + ">" + v + "x</option>"; }).join("") +
        '</select></label><label class="x-frac"><input type="checkbox" data-idle checked> 跳过空闲</label></div>';
      var out = box.querySelector("[data-out]"), bar = box.querySelector("[data-bar]"), tm = box.querySelector("[data-time]"), pb = box.querySelector("[data-play]");
      var scr = new Screen(), idx = 0, t = 0, speed = 1, timer = null, last = 0, skipIdle = true;
      var paint = function () {
        out.textContent = scr.text(); out.scrollTop = out.scrollHeight;
        bar.value = t; tm.textContent = mmss(t) + " / " + mmss(dur);
        var curMark = -1; marks.forEach(function (m, i) { if (m.t <= t) curMark = i; });
        [].forEach.call(box.querySelectorAll("[data-seek]"), function (b) { b.classList.toggle("on", +b.getAttribute("data-seek") === curMark); });
      };
      var advanceTo = function (nt) {
        if (nt < t) { scr.reset(); idx = 0; }
        t = nt;
        while (idx < cast.events.length && cast.events[idx][0] <= t) { scr.feed(cast.events[idx][1]); idx++; }
      };
      var stop = function () { if (timer) { clearInterval(timer); timer = null; } pb.textContent = "播放"; };
      var play = function () {
        if (t >= dur) advanceTo(0);
        pb.textContent = "暂停"; last = performance.now();
        timer = setInterval(function () {
          if (!alive(box)) { stop(); return; }
          var now = performance.now(), nt = t + ((now - last) / 1000) * speed; last = now;
          if (skipIdle && idx < cast.events.length && cast.events[idx][0] - t > 2 && cast.events[idx][0] > nt) nt = cast.events[idx][0] - 0.2;
          advanceTo(Math.min(dur, nt)); paint();
          if (t >= dur) stop();
        }, 60);
      };
      pb.onclick = function () { if (timer) stop(); else play(); };
      bar.oninput = function () { advanceTo(+bar.value); paint(); };
      box.querySelector("[data-spd]").onchange = function () { speed = +this.value; };
      box.querySelector("[data-idle]").onchange = function () { skipIdle = this.checked; };
      on(box, "data-seek", function (i) { advanceTo(marks[+i].t + 0.05); paint(); });
      box.querySelector("[data-dl]").onclick = function () {
        var a = document.createElement("a"), url = URL.createObjectURL(new Blob([r[0]], { type: "application/x-asciicast" }));
        a.href = url; a.download = "session-" + sess.id + ".cast"; a.click(); setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
      };
      RP.player = { stop: stop };
      paint();
    }).catch(function (e) { if (alive(box)) box.innerHTML = U.failBox(e, "录像读取失败"); });
  }
  function mmss(s) { s = Math.max(0, Math.floor(s)); var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60; return (h ? h + ":" : "") + (m < 10 ? "0" : "") + m + ":" + (x < 10 ? "0" : "") + x; }

  /* ================================================================ 命令记录 */
  var CL = { q: "", action: "", range: "7d", page: 1 };
  function renderCmdlog(el) {
    el.innerHTML = head("命令记录", "所有 SSH 会话中提交的命令及过滤结果，可按用户、资产、命令内容检索。", '<button class="x-btn" data-export>导出本页</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderCmdlog(el); }, size = 50;
    var q = { q: CL.q, action: CL.action, page: CL.page, size: size }, from = rangeFrom(CL.range);
    if (from) q.from = from;
    xs().get("/bastion/commands", q).then(function (p) {
      if (!alive(el)) return;
      var cs = U.items(p);
      var rows = cs.map(function (c) {
        return [E(fmtTime(c.created_at, true)), E(c.username), "<b>" + E(c.asset_name) + '</b><div class="x-frac x-mono">' + E(c.account_user) + "</div>", '<code class="x-mono xb-wrap">' + E(c.command) + "</code>",
          cmdAction(c.action), c.rule_name ? E(c.rule_name) : '<span class="x-frac">—</span>', '<button class="x-act" data-play="' + c.session_id + '">会话 #' + E(c.session_id) + "</button>"];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main"><div class="xb-toolbar"><div class="xb-tools"><input class="x-input" data-q placeholder="搜索命令、用户、资产，回车检索" value="' + E(CL.q) + '">' +
        '<select class="x-select" data-act aria-label="按结果筛选">' + [["", "全部结果"], ["allow", "放行"], ["deny", "拦截"], ["confirmed", "确认后执行"], ["approved", "审批通过"], ["rejected", "审批未通过"], ["cancelled", "已取消"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === CL.action ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>" +
        U.segs("rg", RANGES, CL.range) + "</div></div>" + table(["时间", "用户", "资产 / 账号", "命令", "结果", "命中规则", "会话"], rows, "没有符合条件的命令") + U.pager(p.total, CL.page, size) +
        '<p class="x-frac xb-note">命令按回车时提交的整行记录；经历史或补全编辑的行取屏幕回显内容。「放行」表示已发送到主机，不代表命令执行成功。</p></div>';
      var qi = el.querySelector("[data-q]");
      qi.onkeydown = function (e) { if (e.key === "Enter") { CL.q = qi.value.trim(); CL.page = 1; reload(); } };
      el.querySelector("[data-act]").onchange = function () { CL.action = this.value; CL.page = 1; reload(); };
      on(el, "data-rg", function (k) { CL.range = k; CL.page = 1; reload(); });
      on(el, "data-pg", function (n) { CL.page = Math.max(1, +n); reload(); });
      on(el, "data-play", function (id) { U.handoff("replay", +id); go("replay"); });
      el.querySelector("[data-export]").onclick = function () {
        csv("bastion-commands.csv", [["时间", "用户", "资产", "账号", "命令", "结果", "规则", "会话"]].concat(cs.map(function (c) { return [c.created_at, c.username, c.asset_name, c.account_user, c.command, c.action, c.rule_name || "", c.session_id]; })));
      };
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function csv(name, rows) {
    var text = rows.map(function (r) { return r.map(function (v) { return '"' + String(v == null ? "" : v).replace(/"/g, '""') + '"'; }).join(","); }).join("\r\n");
    var url = URL.createObjectURL(new Blob(["\ufeff" + text], { type: "text/csv;charset=utf-8" })), a = document.createElement("a");
    a.href = url; a.download = name; a.click(); setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  }

  /* ================================================================ 登录日志 */
  var LL = { q: "", result: "", range: "7d", page: 1 };
  function renderLoginlog(el) {
    el.innerHTML = head("登录日志", "用户经堡垒机登录资产的每次尝试：成功、被授权或登录 ACL 拒绝、连接失败。", '<button class="x-btn" data-export>导出本页</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderLoginlog(el); }, size = 50;
    var q = { q: LL.q, result: LL.result, page: LL.page, size: size }, from = rangeFrom(LL.range);
    if (from) q.from = from;
    xs().get("/bastion/login-logs", q).then(function (p) {
      if (!alive(el)) return;
      var ls = U.items(p), lr = U.loginResult || function (r) { return E(r); };
      var rows = ls.map(function (l) {
        return [E(fmtTime(l.created_at, true)), E(l.username), "<b>" + E(l.asset_name) + "</b>", '<span class="x-mono">' + E(l.account) + "</span>", E(U.PROTO_NAME[l.protocol] || l.protocol),
          '<span class="x-mono">' + E(l.client_ip || "—") + "</span>", lr(l.result), l.reason ? '<span class="xb-wrap">' + E(l.reason) + "</span>" : "—",
          l.session_id && l.result === "success" && l.protocol === "ssh" ? '<button class="x-act" data-play="' + l.session_id + '">会话</button>' : ""];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main"><div class="xb-toolbar"><div class="xb-tools"><input class="x-input" data-q placeholder="搜索用户、资产、来源 IP，回车检索" value="' + E(LL.q) + '">' +
        '<select class="x-select" data-res aria-label="按结果筛选">' + [["", "全部结果"], ["success", "成功"], ["denied", "拒绝"], ["failed", "失败"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === LL.result ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>" +
        U.segs("rg", RANGES, LL.range) + "</div></div>" + table(["时间", "用户", "资产", "账号", "协议", "来源 IP", "结果", "原因", ""], rows, "没有登录记录") + U.pager(p.total, LL.page, size) +
        '<p class="x-frac xb-note">平台账号本身的登录记录在「系统设置 → 登录日志」。</p></div>';
      var qi = el.querySelector("[data-q]");
      qi.onkeydown = function (e) { if (e.key === "Enter") { LL.q = qi.value.trim(); LL.page = 1; reload(); } };
      el.querySelector("[data-res]").onchange = function () { LL.result = this.value; LL.page = 1; reload(); };
      on(el, "data-rg", function (k) { LL.range = k; LL.page = 1; reload(); });
      on(el, "data-pg", function (n) { LL.page = Math.max(1, +n); reload(); });
      on(el, "data-play", function (id) { U.handoff("replay", +id); go("replay"); });
      el.querySelector("[data-export]").onclick = function () {
        csv("bastion-login-logs.csv", [["时间", "用户", "资产", "账号", "协议", "来源IP", "结果", "原因"]].concat(ls.map(function (l) { return [l.created_at, l.username, l.asset_name, l.account, l.protocol, l.client_ip, l.result, l.reason]; })));
      };
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }

  /* ================================================================ 工单：命令复核 / SQL 工单 */
  var TK = { cmd: { tab: "todo", timer: null }, sql: { tab: "todo", timer: null } };
  var TSTATUS = { pending: ["待审批", "x-orange"], approved: ["已通过，执行中", "x-blue"], executed: ["已通过", "x-green"], rejected: ["已拒绝", "x-red"], cancelled: ["已撤回", "x-grey"], failed: ["执行失败", "x-red"] };
  function tStatus(t) { var m = TSTATUS[t.status] || [t.status, "x-grey"]; return tag(m[0], m[1]) + (t.error ? '<div class="x-frac xb-wrap">' + E(t.error) + "</div>" : ""); }
  function payload(t) { var p = t.payload; if (typeof p === "string") { try { p = JSON.parse(p); } catch (e) { p = {}; } } return p || {}; }
  function renderTickets(el, kind) {
    var st = TK[kind], isCmd = kind === "cmd", type = isCmd ? "bastion.command" : "bastion.sql";
    clearInterval(st.timer);
    var tabs = [{ v: "todo", t: "待我审批" }, { v: "mine", t: "我发起的" }, { v: "all", t: "全部工单" }];
    if (!isCmd) tabs.push({ v: "records", t: "执行记录" });
    el.innerHTML = head(isCmd ? "命令复核" : "SQL 工单", isCmd ? "Web 终端中命中「审批后执行」规则的命令，审批通过后才会发送到主机；申请人在终端里实时看到结果。"
      : "数据库控制台提交的变更 SQL，审批通过后自动在事务中执行；工单只显示去掉字面量的语句摘要。", '<button class="x-btn" data-refresh>刷新</button>') +
      U.segs("tt", tabs, st.tab) + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderTickets(el, kind); };
    el.querySelector("[data-refresh]").onclick = reload;
    on(el, "data-tt", function (k) { st.tab = k; reload(); });
    if (st.tab === "records") return renderSqlRecords(el);
    xs().get("/approvals", { scope: st.tab, type: type, size: 100 }).then(function (p) {
      if (!alive(el)) return;
      var ts = U.items(p), me = (xs().user || {}).user_id || (xs().user || {}).id;
      var rows = ts.map(function (t) {
        var pl = payload(t), mine = String(t.applicant_id) === String(me);
        var ops = t.status === "pending" ? (st.tab !== "mine" && !mine ? '<button class="x-btn x-btn--pri x-btn--s" data-ok="' + t.id + '">批准</button><button class="x-btn x-btn--s" data-no="' + t.id + '">拒绝</button>' : "") +
          (mine ? '<button class="x-act" data-cancel="' + t.id + '">撤回</button>' : "") : "";
        var what = isCmd ? '<code class="x-mono xb-wrap">' + E(pl.command || t.reason) + '</code><div class="x-frac">规则「' + E(pl.rule || "—") + "」 · 会话 #" + E(pl.session_id || "—") + "</div>"
          : levelTag(pl.level) + ' <code class="x-mono xb-wrap">' + E(pl.summary || "") + '</code><div class="x-frac">' + E(pl.risk || "") + "</div>";
        return ["#" + E(t.id), E(t.applicant_name), "<b>" + E(pl.asset || "—") + "</b>" + (isCmd ? "" : '<div class="x-frac">' + E((pl.account || "") + (pl.database ? " · " + pl.database : "")) + "</div>"),
          what, isCmd ? tag(U.envName(t.env), U.ENV_CLS[t.env]) : '<span class="xb-wrap">' + E(t.reason || "—") + "</span>", E(fmtTime(t.created_at, true)), tStatus(t),
          t.approver_name ? E(t.approver_name) + (t.comment ? '<div class="x-frac xb-wrap">' + E(t.comment) + "</div>" : "") : "—", '<span class="xb-ops">' + ops + "</span>"];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + table(["单号", "申请人", "资产", isCmd ? "命令" : "语句", isCmd ? "环境" : "原因", "提交时间", "状态", "处理人 / 意见", "操作"], rows,
        st.tab === "todo" ? "没有待你审批的工单" : "没有工单") +
        '<p class="x-frac xb-note">' + (isCmd ? "命令审批单 10 分钟内未处理会自动撤回；不能审批自己发起的申请。待我审批每 5 秒自动刷新。" : "审批通过后由平台在事务中执行，执行前会重新校验申请人的授权与语句分级。") + "</p></div>";
      on(el, "data-ok", function (id) {
        var t = byId(ts, id), pl = payload(t);
        U.confirmBox("批准" + (isCmd ? "命令" : " SQL 变更"), (isCmd ? "批准后命令会立即在 <b>" + E(pl.asset) + "</b> 上执行：<br><code class=\"x-mono\">" + E(pl.command) + "</code>"
          : "批准后语句会立即在 <b>" + E(pl.asset) + "</b> 上执行（" + E(LEVEL[pl.level] ? LEVEL[pl.level][0] : pl.level) + "）：<br><code class=\"x-mono\">" + E(pl.summary) + "</code>"), "批准并执行", function (comment) {
          return xs().post("/approvals/" + t.id + "/approve", { comment: comment }).then(function () { toast("已批准 #" + t.id); reload(); });
        }, { primary: true, input: { label: "审批意见（选填）" } });
      });
      on(el, "data-no", function (id) {
        var t = byId(ts, id);
        U.confirmBox("拒绝工单 #" + t.id, "拒绝后" + (isCmd ? "命令会被取消，申请人在终端里看到拒绝原因。" : "语句不会执行。"), "拒绝", function (comment) {
          return xs().post("/approvals/" + t.id + "/reject", { comment: comment }).then(function () { toast("已拒绝 #" + t.id); reload(); });
        }, { input: { label: "拒绝原因", required: true } });
      });
      on(el, "data-cancel", function (id) {
        U.confirmBox("撤回工单 #" + id, "撤回后" + (isCmd ? "命令会被取消。" : "语句不会执行。"), "撤回", function () {
          return xs().post("/approvals/" + id + "/cancel", {}).then(function () { toast("已撤回 #" + id); reload(); });
        });
      });
      if (st.tab === "todo") st.timer = setInterval(function () { if (!alive(el)) { clearInterval(st.timer); return; } if (!document.querySelector(".xb-confirm")) reload(); }, 5000);
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function renderSqlRecords(el) {
    xs().get("/bastion/db/queries", { size: 100 }).catch(function (e) {
      if (e && e.status === 403) return xs().get("/bastion/my/db-queries", { size: 100 }).then(function (p) { p._mine = true; return p; });
      throw e;
    }).then(function (p) {
      if (!alive(el)) return;
      var qs = U.items(p);
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + (p._mine ? '<p class="x-frac">没有审计权限，只显示你自己的记录。</p>' : "") +
        table(["时间", "用户", "资产 / 库", "分级", "语句摘要", "结果", "行数", "耗时", "工单", "来源 IP"], qs.map(function (q) {
          return [E(fmtTime(q.created_at, true)), E(q.username), "<b>" + E(q.asset_name) + '</b><div class="x-frac">' + E(q.account_user + (q.database ? " · " + q.database : "")) + "</div>", levelTag(q.level),
            '<code class="x-mono xb-wrap">' + E(q.summary) + "</code>", queryStatus(q), E(q.level === "read" ? q.row_count : q.rows_affected), E(q.duration_ms) + " ms", q.ticket_id ? "#" + E(q.ticket_id) : "—", '<span class="x-mono">' + E(q.client_ip) + "</span>"];
        }), "还没有数据库操作记录") + '<p class="x-frac xb-note">每条 SQL 都有记录；原文加密保存，列表只显示去掉字面量的摘要。</p></div>';
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }

  B.register("myassets", renderMyAssets);
  B.register("dbc", renderDbc);
  B.register("sess", renderSess);
  B.register("replay", renderReplay);
  B.register("cmdlog", renderCmdlog);
  B.register("loginlog", renderLoginlog);
  B.register("cmdap", function (el) { renderTickets(el, "cmd"); });
  B.register("db", function (el) { renderTickets(el, "sql"); });
})(window);
