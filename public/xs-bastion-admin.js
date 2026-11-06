/*
 * 星枢 Matrix 堡垒机：控制台总览、平台、账号管理、改密计划、资产授权、命令过滤、登录 ACL。
 * 由 xs-bastion.js 自动加载，通过 XSBastion.register 注册页面。
 */
(function (root) {
  "use strict";
  var B = root.XSBastion;
  if (!B) return;
  var U = B.util, E = U.E, xs = U.xs, toast = U.toast, errText = U.errText, list = U.list, soft = U.soft, fmtTime = U.fmtTime,
    tag = U.tag, head = U.head, table = U.table, on = U.on, byId = U.byId, go = U.go;
  var rethrow = function (e) { throw new Error(errText(e)); };
  var WEEK = ["", "周一", "周二", "周三", "周四", "周五", "周六", "周日"];

  /* ================================================================ 控制台总览 */
  function renderDash(el) {
    el.innerHTML = head("控制台总览", "资产、会话、审批与改密的整体状态。", '<button class="x-btn" data-refresh>刷新</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    el.querySelector("[data-refresh]").onclick = function () { renderDash(el); };
    Promise.all([xs().get("/bastion/overview"), soft("/bastion/sessions", { status: "active", size: 8 }), soft("/bastion/login-logs", { size: 8 })]).then(function (r) {
      var o = r[0], online = r[1], logs = r[2];
      var card = function (label, val, sub, link, warn) {
        return '<button type="button" class="xb-stat xb-stat--link' + (warn ? " xb-stat--warn" : "") + '"' + (link ? ' data-go="' + link + '"' : "") + "><span>" + E(label) + "</span><b>" + E(val) + "</b><em>" + E(sub) + "</em></button>";
      };
      var ap = o.approvals || {}, rot = o.rotation || {}, lg = o.logins || {}, as = o.assets || {}, ss = o.sessions || {};
      var cards = [
        card("资产", as.total, as.hosts + " 台主机 · " + as.databases + " 个数据库 · " + as.nodes + " 个节点", "assets"),
        card("托管账号", as.accounts, (rot.verify_failed ? rot.verify_failed + " 个连通性验证失败" : "连通性正常"), "accounts", rot.verify_failed > 0),
        card("在线会话", ss.online, "今日 " + ss.today + " 次会话 · " + ss.today_users + " 位用户", "sess"),
        card("今日登录", lg.success, "拒绝 " + lg.denied + " · 失败 " + lg.failed, "loginlog", lg.denied + lg.failed > 0),
        card("待审批命令", ap.command, "命令复核工单", "cmdap", ap.command > 0),
        card("待审批 SQL", ap.sql, "数据库变更工单", "db", ap.sql > 0),
        card("改密失败（7 天）", rot.failed_7d, "成功 " + rot.succeeded_7d + " · 启用计划 " + rot.enabled_plans, "pwd", rot.failed_7d > 0),
        card("今日拦截命令", (o.commands || {}).blocked_today, "数据库查询 " + o.db_queries_today + " 条", "cmdlog")
      ];
      var trend = o.trend || [], max = Math.max.apply(null, [1].concat(trend.map(function (t) { return t.sessions; })));
      var bars = trend.map(function (t) {
        return '<div class="xb-bar"><i style="height:' + Math.round((t.sessions / max) * 100) + '%" title="' + E(t.date + "：" + t.sessions + " 次") + '"></i><b>' + E(t.sessions) + "</b><span>" + E(t.date.slice(5)) + "</span></div>";
      }).join("");
      var top = (o.top_assets || []).map(function (t) { return [E(t.asset), E(t.sessions)]; });
      var onl = online === null ? '<p class="x-frac">需要审计权限才能查看会话。</p>' : table(["用户", "资产", "账号", "开始时间"], online.map(function (s) {
        return [E(s.username), "<b>" + E(s.asset_name) + "</b>", E(s.account_user), E(fmtTime(s.started_at))];
      }), "当前没有在线会话");
      var lgs = logs === null ? '<p class="x-frac">需要审计权限才能查看登录日志。</p>' : table(["时间", "用户", "资产", "结果"], logs.map(function (l) {
        return [E(fmtTime(l.created_at)), E(l.username), E(l.asset_name), loginResult(l.result) + (l.reason ? '<div class="x-frac">' + E(l.reason) + "</div>" : "")];
      }), "还没有登录记录");
      el.querySelector(".xb-body").innerHTML = '<div class="xb-stats">' + cards.join("") + "</div>" +
        '<div class="xb-grid2"><section class="xb-card"><h3>近 7 天会话</h3><div class="xb-bars">' + (bars || '<p class="x-frac">暂无数据</p>') + "</div></section>" +
        '<section class="xb-card"><h3>近 7 天最常访问的资产</h3>' + table(["资产", "会话数"], top, "暂无会话") + "</section>" +
        '<section class="xb-card"><h3>在线会话</h3>' + onl + "</section>" +
        '<section class="xb-card"><h3>最近登录</h3>' + lgs + "</section></div>" +
        '<p class="x-frac xb-note">统计截至 ' + E(fmtTime(o.as_of, true)) + "；「今日」按平台所在时区计算。</p>";
      on(el, "data-go", function (k) { go(k); });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function loginResult(r) { return r === "success" ? tag("成功", "x-green") : r === "denied" ? tag("拒绝", "x-orange") : tag("失败", "x-red"); }

  /* ================================================================ 平台 */
  var PTYPES = [{ v: "linux", t: "Linux" }, { v: "unix", t: "Unix" }, { v: "windows", t: "Windows" }, { v: "mysql", t: "MySQL" }, { v: "postgresql", t: "PostgreSQL" }];
  function renderPlatforms(el) {
    el.innerHTML = head("平台", "资产的系统或数据库类型，决定连接协议与默认端口。内置平台不可删除。", '<button class="x-btn x-btn--pri" data-new>+ 新建平台</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderPlatforms(el); };
    el.querySelector("[data-new]").onclick = function () { platformForm(null, reload); };
    list("/bastion/platforms").then(function (ps) {
      var rows = ps.map(function (p) {
        return ["<b>" + E(p.name) + "</b>" + (p.builtin ? " " + tag("内置", "x-blue") : ""), p.category === "database" ? "数据库" : "主机", E(U.PROTO_NAME[p.protocol] || p.protocol), E(p.port), E(p.charset),
          p.connectable ? tag("可连接", "x-green") : tag("仅登记", "x-grey"), E(p.asset_count), '<span class="x-frac">' + E(p.description || "—") + "</span>",
          '<span class="xb-ops"><button class="x-act" data-edit="' + p.id + '">编辑</button>' + (p.builtin ? "" : '<button class="x-act x-act--danger" data-del="' + p.id + '">删除</button>') + "</span>"];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + table(["平台", "类别", "协议", "默认端口", "字符集", "连接器", "资产数", "说明", "操作"], rows) +
        '<p class="x-frac xb-note">SSH 平台通过 Web 终端连接，MySQL / PostgreSQL 通过数据库控制台连接；远程桌面平台暂无连接器，只能登记。</p></div>';
      on(el, "data-edit", function (id) { platformForm(byId(ps, id), reload); });
      on(el, "data-del", function (id) {
        var p = byId(ps, id);
        if (p.asset_count > 0) { toast("还有 " + p.asset_count + " 个资产使用平台「" + p.name + "」，请先调整资产"); return; }
        U.confirmDelete("平台", p.name, "", function () { return xs().del("/bastion/platforms/" + p.id).then(function () { toast("已删除平台 " + p.name); reload(); }); });
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function platformForm(p, done) {
    var editing = !!p, builtin = editing && p.builtin;
    var defPort = { linux: 22, unix: 22, windows: 3389, mysql: 3306, postgresql: 5432 };
    U.form({
      title: editing ? "编辑平台 · " + p.name : "新建平台", okText: editing ? "保存" : "创建", scene: "bastion.platform", width: "md",
      sections: [{ fields: [
        { k: "name", label: "平台名称", required: true, placeholder: "CentOS 7", readonly: builtin },
        { k: "type", label: "类型", type: builtin ? "text" : "radio", readonly: builtin, options: PTYPES, def: "linux", onChange: function (v, api) { api.set({ port: defPort[v.type] || 22 }); } },
        { k: "port", label: "默认端口", type: "number", required: true, def: 22 },
        { k: "charset", label: "字符集", type: "select", required: true, options: ["utf-8", "gbk"], def: "utf-8" },
        { k: "description", label: "说明", type: "textarea" }
      ] }],
      values: editing ? { name: p.name, type: p.type, port: p.port, charset: p.charset, description: p.description } : {},
      toObject: function (v) { return { name: (v.name || "").trim(), type: v.type, port: Number(v.port), charset: v.charset, description: v.description || "" }; },
      validate: function (v) { var n = Number(v.port); return !(n >= 1 && n <= 65535) ? "端口需在 1–65535" : ""; },
      submit: function (v, obj, ctx) {
        var r = editing ? xs().put("/bastion/platforms/" + p.id, obj) : xs().post("/bastion/platforms", obj);
        return r.then(function () { ctx.close(); toast("平台已保存"); done(); }).catch(rethrow);
      }
    });
  }

  /* ================================================================ 账号管理 */
  var AC = { asset: "", q: "", verify: "" };
  function verifyTag(a) {
    if (a.verify_status === "ok") return tag("可登录", "x-green") + '<div class="x-frac">' + E(fmtTime(a.verified_at)) + "</div>";
    if (a.verify_status === "failed") return tag("验证失败", "x-red") + '<div class="x-frac" title="' + E(a.verify_message) + '">' + E(a.verify_message) + "</div>";
    return '<span class="x-frac">未验证</span>';
  }
  function renderAccounts(el) {
    el.innerHTML = head("账号管理", "资产上的托管账号。密码与私钥只在提交时发送，后端加密保存，任何页面都不显示明文。",
      '<button class="x-btn" data-verify-all>批量验证</button><button class="x-btn x-btn--pri" data-new>+ 添加账号</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderAccounts(el); };
    Promise.all([list("/bastion/assets", { size: 200 }), list("/bastion/accounts"), soft("/bastion/rotation-plans")]).then(function (r) {
      var assets = r[0], accounts = r[1], plans = r[2] || [], planBy = {};
      plans.forEach(function (p) { (p.account_ids || []).forEach(function (id) { (planBy[id] = planBy[id] || []).push(p.name); }); });
      var asset = function (id) { return byId(assets, id); };
      el.querySelector("[data-new]").onclick = function () { accountForm(null, assets, reload); };
      var draw = function () {
        var q = AC.q.toLowerCase();
        var shown = accounts.filter(function (a) {
          var as = asset(a.asset_id) || {};
          if (AC.asset && String(a.asset_id) !== AC.asset) return false;
          if (AC.verify && (a.verify_status || "unknown") !== AC.verify) return false;
          return !q || [a.name, a.username, as.name, as.host].join(" ").toLowerCase().indexOf(q) >= 0;
        });
        var rows = shown.map(function (a) {
          var as = asset(a.asset_id) || {}, canRotate = as.protocol === "ssh" && a.auth_type === "password";
          return ['<b class="x-mono">' + E(a.username) + '</b><div class="x-frac">' + E(a.name) + "</div>",
            "<b>" + E(as.name || "#" + a.asset_id) + '</b><div class="x-frac">' + E(U.PROTO_NAME[as.protocol] || "") + " · " + E(U.envName(as.env)) + "</div>",
            a.auth_type === "private_key" ? "SSH 私钥" : "密码", a.secret ? tag("已托管", "x-green") : tag("未设置", "x-orange"), verifyTag(a),
            a.password_changed_at ? E(fmtTime(a.password_changed_at)) : '<span class="x-frac">' + (canRotate ? "未改过" : "不适用") + "</span>",
            planBy[a.id] ? planBy[a.id].map(function (n) { return tag(n, "x-blue"); }).join(" ") : '<span class="x-frac">—</span>',
            '<span class="xb-ops"><button class="x-act" data-ver="' + a.id + '">验证</button>' + (canRotate ? '<button class="x-act" data-rot="' + a.id + '">立即改密</button>' : "") +
            '<button class="x-act" data-edit="' + a.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + a.id + '">删除</button></span>'];
        });
        el.querySelector(".xb-body").innerHTML = '<div class="xb-main"><div class="xb-toolbar"><div class="xb-tools"><input class="x-input" data-q placeholder="搜索账号、资产" value="' + E(AC.q) + '">' +
          '<select class="x-select" data-asset aria-label="按资产筛选"><option value="">全部资产</option>' + assets.map(function (a) { return '<option value="' + a.id + '"' + (String(a.id) === AC.asset ? " selected" : "") + ">" + E(a.name) + "</option>"; }).join("") + "</select>" +
          '<select class="x-select" data-vf aria-label="按连通性筛选">' + [["", "全部状态"], ["ok", "可登录"], ["failed", "验证失败"], ["unknown", "未验证"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === AC.verify ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select></div>" +
          '<span class="x-frac">共 ' + shown.length + " 个</span></div>" +
          table(["账号", "资产", "认证", "凭据", "连通性", "最近改密", "改密计划", "操作"], rows, accounts.length ? "没有符合条件的账号" : "还没有托管账号") +
          '<p class="x-frac xb-note">「验证」用托管凭据真实登录一次（SSH 不打开 Shell，数据库执行 SELECT 1）。「立即改密」只适用于 SSH 密码账号，周期改密在「改密计划」配置。</p></div>';
        var qi = el.querySelector("[data-q]");
        qi.oninput = function () { AC.q = qi.value; clearTimeout(qi._t); qi._t = setTimeout(function () { draw(); var n = el.querySelector("[data-q]"); n.focus(); n.setSelectionRange(n.value.length, n.value.length); }, 250); };
        el.querySelector("[data-asset]").onchange = function () { AC.asset = this.value; draw(); };
        el.querySelector("[data-vf]").onchange = function () { AC.verify = this.value; draw(); };
        on(el, "data-ver", function (id, b) {
          b.disabled = true; b.textContent = "验证中…";
          xs().post("/bastion/accounts/" + id + "/verify", {}).then(function (v) {
            var a = byId(accounts, id); a.verify_status = v.status; a.verify_message = v.message; a.verified_at = new Date().toISOString();
            toast(v.status === "ok" ? "登录成功（" + v.latency_ms + " ms）" : "验证失败：" + v.message); draw();
          }).catch(function (e) { toast("验证失败：" + errText(e)); b.disabled = false; b.textContent = "验证"; });
        });
        on(el, "data-rot", function (id) {
          var a = byId(accounts, id), as = asset(a.asset_id) || {};
          U.confirmBox("立即改密", "将为 <b>" + E(a.username + "@" + (as.name || "")) + "</b> 生成新的随机密码并在主机上修改，然后用新密码验证登录。<br>失败时会自动判断主机上的实际密码并保留可用的那一个。", "开始改密", function () {
            return xs().post("/bastion/accounts/" + a.id + "/rotate", {}).then(function (res) {
              toast(res.status === "succeeded" ? "改密成功：" + res.message : res.status === "skipped" ? "已跳过：" + res.message : "改密失败：" + res.message); reload();
            });
          }, { primary: true });
        });
        on(el, "data-edit", function (id) { accountForm(byId(accounts, id), assets, reload); });
        on(el, "data-del", function (id) {
          var a = byId(accounts, id);
          U.confirmDelete("账号", a.username, "使用该账号的授权将不再可用，改密计划会跳过它。", function () {
            return xs().del("/bastion/accounts/" + a.id).then(function () { toast("已删除账号 " + a.username); reload(); });
          });
        });
      };
      el.querySelector("[data-verify-all]").onclick = function () {
        var targets = accounts.filter(function (a) { return !AC.asset || String(a.asset_id) === AC.asset; });
        if (!targets.length) { toast("没有可验证的账号"); return; }
        U.confirmBox("批量验证", "将依次用托管凭据登录 " + targets.length + " 个账号（当前筛选范围）。", "开始验证", function () {
          var okN = 0, badN = 0;
          return targets.reduce(function (p, a) {
            return p.then(function () { return xs().post("/bastion/accounts/" + a.id + "/verify", {}).then(function (v) { if (v.status === "ok") okN++; else badN++; }, function () { badN++; }); });
          }, Promise.resolve()).then(function () { toast("验证完成：成功 " + okN + "，失败 " + badN); reload(); });
        }, { primary: true });
      };
      draw();
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function accountForm(a, assets, done) {
    var editing = !!a;
    var assetOf = function (v) { return byId(assets, v.asset_id) || {}; };
    var isDB = function (v) { return assetOf(v).protocol && assetOf(v).protocol !== "ssh"; };
    U.form({
      title: editing ? "编辑账号 · " + a.username : "添加账号", okText: editing ? "保存" : "添加", scene: "bastion.account", width: "md",
      sections: [{ fields: [
        { k: "asset_id", label: "资产", type: "select", required: true, options: assets.map(function (x) { return { v: String(x.id), t: U.assetLabel(x) }; }) },
        { k: "username", label: "登录用户名", required: true, placeholder: "ops" },
        { k: "name", label: "显示名称", placeholder: "留空则为 用户名@资产" },
        { k: "auth_type", label: "认证方式", type: "radio", options: [{ v: "password", t: "密码" }, { v: "private_key", t: "SSH 私钥" }], def: "password", show: function (v) { return !isDB(v); } },
        { k: "password", label: "密码", type: "password", secret: true, show: function (v) { return isDB(v) || v.auth_type !== "private_key"; } },
        { k: "private_key", label: "私钥（PEM）", type: "code", secret: true, placeholder: editing ? "留空表示保持不变" : "-----BEGIN OPENSSH PRIVATE KEY-----", show: function (v) { return !isDB(v) && v.auth_type === "private_key"; } },
        { k: "passphrase", label: "私钥口令", type: "password", secret: true, show: function (v) { return !isDB(v) && v.auth_type === "private_key"; } }
      ] }],
      values: editing ? { __existing: true, asset_id: String(a.asset_id), username: a.username, name: a.name, auth_type: a.auth_type } : { asset_id: assets[0] ? String(assets[0].id) : "" },
      toObject: function (v) {
        var pk = !isDB(v) && v.auth_type === "private_key", as = assetOf(v);
        return { asset_id: Number(v.asset_id), username: (v.username || "").trim(), name: (v.name || "").trim() || (v.username || "").trim() + "@" + (as.name || ""),
          auth_type: pk ? "private_key" : "password", secret: pk ? (v.private_key || "") : (v.password || ""), passphrase: pk ? (v.passphrase || "") : "" };
      },
      validate: function (v) {
        if (!v.asset_id) return "请选择资产";
        if (!/^[A-Za-z0-9._@-]{1,64}$/.test(v.username || "")) return "用户名只能包含字母、数字、. _ @ -";
        var pk = !isDB(v) && v.auth_type === "private_key";
        if (!editing && !pk && !v.password) return "请填写密码";
        if (pk && (!editing || v.private_key) && !/-----BEGIN/.test(v.private_key || "")) return "请粘贴 PEM 格式的私钥";
        if (editing && pk && a.auth_type !== "private_key" && !v.private_key) return "改为私钥认证时需要填写私钥";
        if (editing && !pk && a.auth_type === "private_key" && !v.password) return "改为密码认证时需要填写密码";
        return "";
      },
      submit: function (v, obj, ctx) {
        var r = editing ? xs().put("/bastion/accounts/" + a.id, obj) : xs().post("/bastion/accounts", obj);
        return r.then(function () { ctx.close(); toast(editing ? "账号已更新" : "账号已加密保存"); done(); }).catch(rethrow);
      }
    });
  }

  /* ================================================================ 改密计划 */
  var PW = { tab: "plans", plan: "" };
  function runStatus(s) { return s === "succeeded" ? tag("成功", "x-green") : s === "skipped" ? tag("跳过", "x-grey") : tag("失败", "x-red"); }
  function renderPwd(el) {
    el.innerHTML = head("改密计划", "定期为 SSH 密码账号生成强随机密码并在主机上修改，改后用新密码验证登录再保存。",
      '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 新建计划</button>') +
      U.segs("pwtab", [{ v: "plans", t: "改密计划" }, { v: "runs", t: "执行记录" }], PW.tab) + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderPwd(el); };
    el.querySelector("[data-refresh]").onclick = reload;
    on(el, "data-pwtab", function (k) { PW.tab = k; reload(); });
    Promise.all([list("/bastion/rotation-plans"), list("/bastion/accounts"), list("/bastion/assets", { size: 200 }), PW.tab === "runs" ? list("/bastion/rotation-runs", PW.plan ? { plan_id: PW.plan } : {}) : null]).then(function (r) {
      var plans = r[0], accounts = r[1], assets = r[2], runs = r[3];
      var label = function (id) { var a = byId(accounts, id); if (!a) return "已删除账号"; var as = byId(assets, a.asset_id) || {}; return a.username + "@" + (as.name || "?"); };
      var eligible = accounts.filter(function (a) { var as = byId(assets, a.asset_id) || {}; return as.protocol === "ssh" && a.auth_type === "password"; });
      el.querySelector("[data-new]").onclick = function () { planForm(null, eligible, label, reload); };
      var body = el.querySelector(".xb-body");
      if (PW.tab === "runs") {
        var rows = runs.map(function (x) {
          var dur = x.finished_at && x.started_at ? new Date(x.finished_at) - new Date(x.started_at) : null;
          return [E(fmtTime(x.started_at, true)), E(x.plan_name), "<b>" + E(x.asset) + '</b><div class="x-frac x-mono">' + E(x.username) + "</div>", runStatus(x.status),
            '<span class="xb-wrap">' + E(x.message) + "</span>", E(x.triggered_by || "—"), E(U.fmtDur(dur))];
        });
        body.innerHTML = '<div class="xb-main"><div class="xb-toolbar"><select class="x-select" data-plan aria-label="按计划筛选"><option value="">全部计划（含手动）</option>' +
          plans.map(function (p) { return '<option value="' + p.id + '"' + (String(p.id) === PW.plan ? " selected" : "") + ">" + E(p.name) + "</option>"; }).join("") + '</select><span class="x-frac">最近 100 条</span></div>' +
          table(["时间", "计划", "账号", "结果", "说明", "触发人", "耗时"], rows, "还没有执行记录") + "</div>";
        body.querySelector("[data-plan]").onchange = function () { PW.plan = this.value; reload(); };
        return;
      }
      var prow = plans.map(function (p) {
        var ids = p.account_ids || [];
        return ["<b>" + E(p.name) + '</b><div class="x-frac">创建人 ' + E(p.created_by || "—") + "</div>",
          '<span title="' + E(ids.map(label).join("、")) + '">' + ids.length + " 个</span>", "每 " + E(p.interval_days) + " 天", E(p.length) + " 位",
          '<label class="xb-switch"><input type="checkbox" data-toggle="' + p.id + '"' + (p.enabled ? " checked" : "") + "><span></span>" + (p.enabled ? "启用" : "停用") + "</label>",
          p.enabled ? E(fmtTime(p.next_run_at)) : '<span class="x-frac">已停用</span>',
          p.last_run_at ? E(fmtTime(p.last_run_at)) + '<div class="x-frac">' + E(p.last_result) + "</div>" : '<span class="x-frac">未执行</span>',
          '<span class="xb-ops"><button class="x-act" data-run="' + p.id + '">立即执行</button><button class="x-act" data-runs="' + p.id + '">记录</button><button class="x-act" data-edit="' + p.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + p.id + '">删除</button></span>'];
      });
      body.innerHTML = '<div class="xb-main">' + (plans.length ? table(["计划", "账号", "周期", "密码长度", "状态", "下次执行", "上次执行", "操作"], prow)
        : '<div class="xb-guide"><h3>还没有改密计划</h3><ol><li>选择要定期改密的 SSH 密码账号（root 用 chpasswd，普通用户用 passwd）</li><li>设置周期与密码长度</li><li>后台每分钟检查到期计划，结果记入「执行记录」</li></ol></div>') +
        '<p class="x-frac xb-note">可选账号 ' + eligible.length + " 个（私钥账号与数据库账号不参与改密）。执行中途失败时，系统会用新旧密码分别登录判断主机实际状态，并保留可用的那一个。</p></div>";
      [].forEach.call(body.querySelectorAll("[data-toggle]"), function (b) {
        b.onchange = function () {
          var p = byId(plans, b.getAttribute("data-toggle"));
          xs().put("/bastion/rotation-plans/" + p.id, { name: p.name, account_ids: p.account_ids, interval_days: p.interval_days, length: p.length, enabled: b.checked })
            .then(function () { toast((b.checked ? "已启用 " : "已停用 ") + p.name); reload(); }).catch(function (e) { toast("操作失败：" + errText(e)); reload(); });
        };
      });
      on(body, "data-run", function (id) {
        var p = byId(plans, id);
        U.confirmBox("立即执行改密", "将为计划 <b>" + E(p.name) + "</b> 的 " + (p.account_ids || []).length + " 个账号生成新密码并在主机上修改。<br>执行可能需要几分钟，请勿重复点击。", "立即执行", function () {
          toast("正在执行改密计划 " + p.name + "…");
          return xs().post("/bastion/rotation-plans/" + p.id + "/run", {}, { timeoutMs: 300000 }).then(function (res) {
            toast("执行完成：成功 " + res.succeeded + "，失败 " + res.failed); PW.tab = "runs"; PW.plan = String(p.id); reload();
          });
        }, { primary: true });
      });
      on(body, "data-runs", function (id) { PW.tab = "runs"; PW.plan = id; reload(); });
      on(body, "data-edit", function (id) { planForm(byId(plans, id), eligible, label, reload); });
      on(body, "data-del", function (id) {
        var p = byId(plans, id);
        U.confirmDelete("改密计划", p.name, "计划的执行记录会一并删除，已改的密码不受影响。", function () { return xs().del("/bastion/rotation-plans/" + p.id).then(function () { toast("已删除计划 " + p.name); reload(); }); });
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function planForm(p, eligible, label, done) {
    var editing = !!p;
    var opts = eligible.map(function (a) { return { v: String(a.id), t: label(a.id) }; });
    (editing ? p.account_ids || [] : []).forEach(function (id) { if (!opts.some(function (o) { return o.v === String(id); })) opts.push({ v: String(id), t: label(id) + "（不可改密）" }); });
    U.form({
      title: editing ? "编辑改密计划 · " + p.name : "新建改密计划", okText: editing ? "保存" : "创建", scene: "bastion.rotation", width: "md",
      sections: [{ fields: [
        { k: "name", label: "计划名称", required: true, placeholder: "生产主机月度改密" },
        { k: "account_ids", label: "账号", type: "checks", required: true, options: opts, help: opts.length ? "只列出 SSH 密码账号" : "没有可改密的账号：需要 SSH 资产上的密码账号" },
        { k: "interval_days", label: "周期（天）", type: "number", required: true, def: 30, min: 1, max: 365 },
        { k: "length", label: "密码长度", type: "number", required: true, def: 20, min: 12, max: 64, help: "包含大小写字母、数字与符号" },
        { k: "next_run_at", label: "首次执行时间", placeholder: "2026-12-01 02:00", help: editing ? "留空保持当前计划时间" : "留空则从现在起一个周期后执行" },
        { k: "enabled", label: "启用", type: "switch", def: true }
      ] }],
      values: editing ? { name: p.name, account_ids: (p.account_ids || []).map(String), interval_days: p.interval_days, length: p.length, enabled: p.enabled } : {},
      toObject: function (v) {
        var o = { name: (v.name || "").trim(), account_ids: (v.account_ids || []).map(Number), interval_days: Number(v.interval_days), length: Number(v.length), enabled: v.enabled !== false };
        var t = U.parseLocal(v.next_run_at); if (t) o.next_run_at = t;
        return o;
      },
      validate: function (v) {
        var d = Number(v.interval_days), l = Number(v.length);
        if (!(d >= 1 && d <= 365)) return "周期需在 1–365 天";
        if (!(l >= 12 && l <= 64)) return "密码长度需在 12–64 位";
        if (U.parseLocal(v.next_run_at) === undefined) return "首次执行时间格式为 2026-12-01 02:00";
        return "";
      },
      submit: function (v, obj, ctx) {
        var r = editing ? xs().put("/bastion/rotation-plans/" + p.id, obj) : xs().post("/bastion/rotation-plans", obj);
        return r.then(function () { ctx.close(); toast("改密计划已保存"); done(); }).catch(rethrow);
      }
    });
  }

  /* ================================================================ 资产授权 */
  var ACTS = [{ v: "connect", t: "连接" }, { v: "upload", t: "上传" }, { v: "download", t: "下载" }, { v: "copy", t: "复制" }, { v: "paste", t: "粘贴" }];
  var ACT_ZH = { connect: "连接", upload: "上传", download: "下载", copy: "复制", paste: "粘贴" };
  // 授权主体选项：用户 / 角色 / 用户组（读取失败时退回手动填写）
  function loadSubjects() {
    return Promise.all([soft("/users", { size: 200 }), soft("/roles"), soft("/user-groups")]).then(function (r) {
      return {
        user: r[0] && r[0].map(function (u) { return { v: String(u.id), t: (u.display_name || u.username) + "（" + u.username + "）" }; }),
        role: r[1] && r[1].map(function (x) { return { v: x.code, t: x.name + "（" + x.code + "）" }; }),
        group: r[2] && r[2].map(function (g) { return { v: String(g.id), t: g.name }; })
      };
    });
  }
  function subjectText(type, val, subs) {
    var o = (subs[type] || []).filter(function (x) { return x.v === String(val); })[0];
    return ({ user: "用户", role: "角色", group: "用户组", all: "所有人" }[type] || type) + (type === "all" ? "" : " · " + (o ? o.t : val));
  }
  function subjectFields(subs, withAll) {
    var types = [{ v: "user", t: "用户" }, { v: "group", t: "用户组" }, { v: "role", t: "角色" }];
    if (withAll) types.unshift({ v: "all", t: "所有人" });
    var f = [{ k: "subject_type", label: "适用对象", type: "radio", options: types, def: withAll ? "all" : "user" }];
    ["user", "group", "role"].forEach(function (t) {
      var name = { user: "用户", group: "用户组", role: "角色" }[t];
      f.push(subs[t] && subs[t].length
        ? { k: "subject_" + t, label: name, type: "select", required: true, options: subs[t], show: function (v) { return v.subject_type === t; } }
        : { k: "subject_" + t, label: name + (t === "user" ? " ID" : t === "group" ? " ID" : "编码"), required: true, placeholder: "没有读取" + name + "列表的权限，请手动填写", show: function (v) { return v.subject_type === t; } });
    });
    return f;
  }
  function renderPolicy(el) {
    el.innerHTML = head("资产授权", "用户 / 用户组 / 角色 → 资产或节点 → 账号 → 动作，可设置生效与失效时间。", '<button class="x-btn x-btn--pri" data-new>+ 新建授权</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderPolicy(el); };
    Promise.all([list("/bastion/rules"), list("/bastion/assets", { size: 200 }), list("/bastion/accounts"), soft("/bastion/nodes"), loadSubjects()]).then(function (r) {
      var rules = r[0], assets = r[1], accounts = r[2], nodes = r[3] || [], subs = r[4], ix = U.nodeIndex(nodes), now = Date.now();
      var ctx = { assets: assets, accounts: accounts, nodes: nodes, subs: subs };
      el.querySelector("[data-new]").onclick = function () { ruleForm(null, ctx, reload); };
      var state = function (x) {
        if (!x.enabled) return tag("停用", "x-grey");
        if (x.expires_at && new Date(x.expires_at) < now) return tag("已过期", "x-grey");
        if (x.starts_at && new Date(x.starts_at) > now) return tag("未生效", "x-orange");
        return tag("生效中", "x-green");
      };
      var rows = rules.map(function (x) {
        var tgt = (x.asset_ids || []).map(function (id) { var a = byId(assets, id); return E(a ? a.name : "#" + id); })
          .concat((x.node_ids || []).map(function (id) { return tag("节点 " + (ix.byId[id] ? ix.path(id) : "#" + id), "x-blue"); }));
        var accs = (x.account_ids || []).length ? x.account_ids.map(function (id) { var a = byId(accounts, id); return E(a ? a.username : "#" + id); }).join("、") : '<span class="x-frac">全部账号</span>';
        return ["<b>" + E(x.name) + "</b>", E(subjectText(x.subject_type, x.subject, subs)), tgt.join("、") || "—", accs,
          (x.actions || []).map(function (a) { return tag(ACT_ZH[a] || a, a === "connect" ? "x-blue" : "x-grey"); }).join(" "),
          (x.starts_at ? E(fmtTime(x.starts_at)) : "立即") + " ~ " + (x.expires_at ? E(fmtTime(x.expires_at)) : "长期"), state(x),
          '<span class="xb-ops"><button class="x-act" data-edit="' + x.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + x.id + '">删除</button></span>'];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + table(["名称", "授权对象", "资产 / 节点", "账号", "动作", "有效期", "状态", "操作"], rows, "还没有资产授权") +
        '<p class="x-frac xb-note">「连接」由后端强制校验；「复制 / 粘贴」由 Web 终端执行；「上传 / 下载」需要文件传输通道，当前版本未提供，仅登记。按节点授权包含其全部子节点下的资产。</p></div>';
      on(el, "data-edit", function (id) { ruleForm(byId(rules, id), ctx, reload); });
      on(el, "data-del", function (id) {
        var x = byId(rules, id);
        U.confirmDelete("授权", x.name, "授权对象将立即失去这条授权提供的访问权限。", function () { return xs().del("/bastion/rules/" + x.id).then(function () { toast("已删除授权 " + x.name); reload(); }); });
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function ruleForm(x, ctx, done) {
    var editing = !!x, subs = ctx.subs;
    var vals = editing ? { name: x.name, subject_type: x.subject_type, asset_ids: (x.asset_ids || []).map(String), node_ids: (x.node_ids || []).map(String), account_ids: (x.account_ids || []).map(String),
      actions: x.actions || [], starts_at: U.toLocal(x.starts_at), expires_at: U.toLocal(x.expires_at), enabled: x.enabled } : { actions: ["connect", "copy", "paste"] };
    if (editing) vals["subject_" + x.subject_type] = x.subject;
    var accLabel = function (a) { var as = byId(ctx.assets, a.asset_id); return a.username + " @ " + (as ? as.name : "?"); };
    U.form({
      title: editing ? "编辑授权 · " + x.name : "新建资产授权", okText: editing ? "保存" : "创建", scene: "bastion.grant", width: "lg", wizard: !editing,
      sections: [
        { title: "授权对象", fields: [{ k: "name", label: "授权名称", required: true, placeholder: "运维组-生产主机" }].concat(subjectFields(subs, false)) },
        { title: "资产与账号", desc: "可以直接选资产，也可以选节点（包含子节点下所有资产）。账号不选表示所选资产的全部账号。", fields: [
          { k: "node_ids", label: "节点", type: "checks", options: U.nodeOptions(ctx.nodes) },
          { k: "asset_ids", label: "资产", type: "checks", options: ctx.assets.map(function (a) { return { v: String(a.id), t: U.assetLabel(a) }; }) },
          { k: "account_ids", label: "限定账号", type: "checks", options: ctx.accounts.map(function (a) { return { v: String(a.id), t: accLabel(a) }; }) }
        ], validate: function (v) { return (v.asset_ids || []).length + (v.node_ids || []).length ? "" : "请至少选择一个资产或节点"; } },
        { title: "动作与有效期", fields: [
          { k: "actions", label: "允许的动作", type: "checks", required: true, options: ACTS, help: "上传 / 下载需要文件传输通道（未提供），勾选后仅登记" },
          { k: "starts_at", label: "生效时间", placeholder: "留空立即生效，如 2026-11-06 09:00" },
          { k: "expires_at", label: "失效时间", placeholder: "留空长期有效，如 2026-12-31 18:00" },
          { k: "enabled", label: "启用", type: "switch", def: true }
        ] }
      ],
      values: vals,
      toObject: function (v) {
        return { name: (v.name || "").trim(), subject_type: v.subject_type, subject: String(v["subject_" + v.subject_type] || "").trim(),
          asset_ids: (v.asset_ids || []).map(Number), node_ids: (v.node_ids || []).map(Number), account_ids: (v.account_ids || []).map(Number),
          actions: v.actions || [], starts_at: U.parseLocal(v.starts_at) || null, expires_at: U.parseLocal(v.expires_at) || null, enabled: v.enabled !== false };
      },
      validate: function (v) {
        if (!(v.asset_ids || []).length && !(v.node_ids || []).length) return "请至少选择一个资产或节点";
        if ((v.actions || []).indexOf("connect") < 0) return "授权必须包含「连接」动作";
        var s = U.parseLocal(v.starts_at), e = U.parseLocal(v.expires_at);
        if (s === undefined || e === undefined) return "时间格式为 2026-12-31 18:00";
        if (s && e && e <= s) return "失效时间必须晚于生效时间";
        var nodeSet = {}, ix = U.nodeIndex(ctx.nodes);
        (v.node_ids || []).forEach(function (n) { ix.desc(+n).forEach(function (d) { nodeSet[d] = 1; }); });
        var bad = (v.account_ids || []).some(function (id) {
          var a = byId(ctx.accounts, id), as = a && byId(ctx.assets, a.asset_id);
          return !a || ((v.asset_ids || []).indexOf(String(a.asset_id)) < 0 && !(as && nodeSet[as.node_id]));
        });
        return bad ? "限定账号必须属于所选资产或节点下的资产" : "";
      },
      submit: function (v, obj, fctx) {
        var r = editing ? xs().put("/bastion/rules/" + x.id, obj) : xs().post("/bastion/rules", obj);
        return r.then(function () { fctx.close(); toast("资产授权已保存"); done(); }).catch(rethrow);
      }
    });
  }

  /* ================================================================ 命令过滤 */
  var FACT = {
    deny: ["拒绝", "x-red", "直接拦截，命令不会发送到主机"],
    confirm: ["确认", "x-orange", "提示执行人输入 yes 确认后执行"],
    approve: ["审批后执行", "x-blue", "命令暂存，提交审批单，审批人通过后才发送到主机"],
    allow: ["允许", "x-green", "白名单：命中即放行，不再匹配后续规则"]
  };
  function renderCmd(el) {
    el.innerHTML = head("命令过滤", "SSH 会话中的每条命令按优先级从小到大匹配规则，第一条命中的规则决定动作。",
      '<button class="x-btn" data-test>测试命令</button><button class="x-btn x-btn--pri" data-new>+ 新建规则</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderCmd(el); };
    Promise.all([list("/bastion/filters"), list("/bastion/assets", { size: 200 })]).then(function (r) {
      var fs = r[0], assets = r[1], groups = {};
      fs.forEach(function (f) { (groups[f.group_name || "默认规则组"] = groups[f.group_name || "默认规则组"] || []).push(f); });
      el.querySelector("[data-new]").onclick = function () { filterForm(null, assets, Object.keys(groups), reload); };
      el.querySelector("[data-test]").onclick = function () { testCommand(fs, assets); };
      var names = Object.keys(groups).sort();
      var html = names.map(function (g) {
        return '<section class="xb-card"><h3>' + E(g) + ' <span class="x-frac">' + groups[g].length + " 条规则</span></h3>" + table(["优先级", "规则", "表达式", "动作", "作用资产", "状态", "操作"], groups[g].map(function (f) {
          var a = FACT[f.action] || [f.action, "x-grey"];
          return [E(f.priority), "<b>" + E(f.name) + "</b>", '<code class="x-mono xb-ellipsis" title="' + E(f.pattern) + '">' + E(f.pattern) + "</code>", tag(a[0], a[1]),
            (f.asset_ids || []).length ? f.asset_ids.map(function (id) { var x = byId(assets, id); return E(x ? x.name : "#" + id); }).join("、") : '<span class="x-frac">全部资产</span>',
            '<label class="xb-switch"><input type="checkbox" data-toggle="' + f.id + '"' + (f.enabled ? " checked" : "") + "><span></span>" + (f.enabled ? "启用" : "停用") + "</label>",
            '<span class="xb-ops"><button class="x-act" data-edit="' + f.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + f.id + '">删除</button></span>'];
        })) + "</section>";
      }).join("");
      el.querySelector(".xb-body").innerHTML = (html || '<div class="xb-guide"><h3>还没有命令过滤规则</h3><p class="x-frac">建议至少拦截 rm -rf /、mkfs、dd 写盘等危险命令；重启、停服务类命令可设为「审批后执行」。</p></div>') +
        '<div class="xb-legend">' + Object.keys(FACT).map(function (k) { return tag(FACT[k][0], FACT[k][1]) + ' <span class="x-frac">' + FACT[k][2] + "</span>"; }).join("") + "</div>" +
        '<p class="x-frac xb-note">规则在新建立的会话中生效。表达式由后端 Go RE2 引擎执行（不支持回溯与前瞻）。规则组用于归类，匹配顺序只看优先级。</p>';
      [].forEach.call(el.querySelectorAll("[data-toggle]"), function (b) {
        b.onchange = function () {
          var f = byId(fs, b.getAttribute("data-toggle"));
          xs().put("/bastion/filters/" + f.id, Object.assign({}, f, { enabled: b.checked })).then(function () { toast((b.checked ? "已启用 " : "已停用 ") + f.name); reload(); })
            .catch(function (e) { toast("操作失败：" + errText(e)); reload(); });
        };
      });
      on(el, "data-edit", function (id) { filterForm(byId(fs, id), assets, names, reload); });
      on(el, "data-del", function (id) {
        var f = byId(fs, id);
        U.confirmDelete("命令过滤规则", f.name, "删除后新会话中的此类命令将不再受该规则约束。", function () { return xs().del("/bastion/filters/" + f.id).then(function () { toast("已删除规则 " + f.name); reload(); }); });
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function jsRe(p) { try { return new RegExp(p.replace(/^\(\?i\)/, ""), /^\(\?i\)/.test(p) ? "i" : ""); } catch (e) { return null; } }
  function testCommand(fs, assets) {
    var m = U.modal("测试命令", '<div class="x-form"><label><span>资产</span><select class="x-select" data-ta><option value="">任意资产</option>' + assets.filter(function (a) { return a.protocol === "ssh"; }).map(function (a) { return '<option value="' + a.id + '">' + E(a.name) + "</option>"; }).join("") +
      '</select></label><label><span>命令</span><input class="x-input x-mono" data-tc placeholder="systemctl restart nginx" autocomplete="off" spellcheck="false"></label><div data-tr class="xb-note"></div><p class="x-frac">在浏览器中用 JavaScript 正则模拟匹配，个别 RE2 专属语法可能与后端结果不同。</p></div>');
    var run = function () {
      var cmd = m.body.querySelector("[data-tc]").value.trim(), aid = m.body.querySelector("[data-ta]").value, out = m.body.querySelector("[data-tr]");
      if (!cmd) { out.innerHTML = ""; return; }
      var hit = fs.filter(function (f) { return f.enabled && (!aid || !(f.asset_ids || []).length || f.asset_ids.map(String).indexOf(aid) >= 0); })
        .sort(function (a, b) { return a.priority - b.priority || a.id - b.id; }).filter(function (f) { var re = jsRe(f.pattern); return re && re.test(cmd); })[0];
      out.innerHTML = hit ? "命中规则 <b>" + E(hit.name) + "</b>（" + E(hit.group_name) + "，优先级 " + E(hit.priority) + "）：" + tag(FACT[hit.action][0], FACT[hit.action][1]) + ' <span class="x-frac">' + FACT[hit.action][2] + "</span>"
        : tag("放行", "x-green") + ' <span class="x-frac">没有命中任何规则</span>';
    };
    m.body.querySelector("[data-tc]").oninput = run;
    m.body.querySelector("[data-ta]").onchange = run;
    setTimeout(function () { m.body.querySelector("[data-tc]").focus(); }, 0);
  }
  function filterForm(f, assets, groups, done) {
    var editing = !!f;
    U.form({
      title: editing ? "编辑命令规则 · " + f.name : "新建命令规则", okText: editing ? "保存" : "创建", scene: "bastion.filter", width: "md",
      sections: [{ fields: [
        { k: "group_name", label: "规则组", required: true, options: groups.map(function (g) { return { v: g, t: g }; }), placeholder: "默认规则组", def: "默认规则组" },
        { k: "name", label: "规则名称", required: true, placeholder: "禁止删除根目录" },
        { k: "pattern", label: "匹配表达式（RE2 正则）", required: true, mono: true, placeholder: "^\\s*rm\\s+-rf\\s+/(\\s|$)", check: function (v) { return jsRe(v) ? "" : "正则语法错误"; } },
        { k: "action", label: "动作", type: "radio", options: Object.keys(FACT).map(function (k) { return { v: k, t: FACT[k][0], d: FACT[k][2] }; }), def: "deny" },
        { k: "priority", label: "优先级", type: "number", required: true, def: 100, min: 1, max: 100000, help: "数字越小越先匹配" },
        { k: "asset_ids", label: "作用资产", type: "checks", options: assets.filter(function (a) { return a.protocol === "ssh"; }).map(function (a) { return { v: String(a.id), t: U.assetLabel(a) }; }), help: "不选表示全部资产" },
        { k: "enabled", label: "启用", type: "switch", def: true }
      ] }],
      values: editing ? { group_name: f.group_name, name: f.name, pattern: f.pattern, action: f.action, priority: f.priority, asset_ids: (f.asset_ids || []).map(String), enabled: f.enabled } : {},
      toObject: function (v) { return { group_name: (v.group_name || "").trim() || "默认规则组", name: (v.name || "").trim(), pattern: v.pattern, action: v.action, priority: Number(v.priority), asset_ids: (v.asset_ids || []).map(Number), enabled: v.enabled !== false }; },
      validate: function (v) { var p = Number(v.priority); if (!(p >= 1 && p <= 100000)) return "优先级需为 1–100000"; var re = jsRe(v.pattern || ""); return re && re.test("") ? "表达式不能匹配空命令" : ""; },
      submit: function (v, obj, ctx) {
        var r = editing ? xs().put("/bastion/filters/" + f.id, obj) : xs().post("/bastion/filters", obj);
        return r.then(function () { ctx.close(); toast("命令规则已保存，新会话生效"); done(); }).catch(rethrow);
      }
    });
  }

  /* ================================================================ 登录 ACL */
  function renderAcl(el) {
    el.innerHTML = head("登录 ACL", "按来源 IP、星期与时段控制谁能通过堡垒机连接哪些资产（Web 终端与数据库控制台）。",
      '<button class="x-btn x-btn--pri" data-new>+ 新建规则</button>') + '<div class="xb-body"><p class="x-frac">正在读取…</p></div>';
    var reload = function () { renderAcl(el); };
    Promise.all([list("/bastion/login-acls"), list("/bastion/assets", { size: 200 }), loadSubjects()]).then(function (r) {
      var acls = r[0], assets = r[1], subs = r[2];
      el.querySelector("[data-new]").onclick = function () { aclForm(null, assets, subs, reload); };
      var when = function (x) {
        var d = (x.weekdays || []).length ? x.weekdays.slice().sort().map(function (n) { return WEEK[n]; }).join("、") : "每天";
        return d + (x.time_from ? " " + E(x.time_from) + "–" + E(x.time_to) + (x.time_from > x.time_to ? "（跨天）" : "") : " 全天");
      };
      var rows = acls.map(function (x) {
        return [E(x.priority), "<b>" + E(x.name) + "</b>" + (x.description ? '<div class="x-frac">' + E(x.description) + "</div>" : ""), E(subjectText(x.subject_type, x.subject, subs)),
          (x.asset_ids || []).length ? x.asset_ids.map(function (id) { var a = byId(assets, id); return E(a ? a.name : "#" + id); }).join("、") : '<span class="x-frac">全部资产</span>',
          (x.ip_ranges || []).length ? '<span class="x-mono">' + x.ip_ranges.map(E).join("<br>") + "</span>" : '<span class="x-frac">任意来源</span>', when(x),
          x.action === "deny" ? tag("拒绝", "x-red") : tag("允许", "x-green"),
          '<label class="xb-switch"><input type="checkbox" data-toggle="' + x.id + '"' + (x.enabled ? " checked" : "") + "><span></span>" + (x.enabled ? "启用" : "停用") + "</label>",
          '<span class="xb-ops"><button class="x-act" data-edit="' + x.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + x.id + '">删除</button></span>'];
      });
      el.querySelector(".xb-body").innerHTML = '<div class="xb-main">' + table(["优先级", "规则", "适用对象", "资产", "来源 IP", "时段", "动作", "状态", "操作"], rows, "还没有登录 ACL，所有已授权的连接都会放行") +
        '<p class="x-frac xb-note">规则按优先级从小到大匹配，第一条命中的规则决定允许或拒绝；没有命中时放行（授权仍是必要条件）。常见做法：先放行办公网段，再用一条「所有人 · 任意来源 · 拒绝」兜底。时段按平台时区（默认北京时间）判断。被拒绝的尝试记入「登录日志」。</p></div>';
      [].forEach.call(el.querySelectorAll("[data-toggle]"), function (b) {
        b.onchange = function () {
          var x = byId(acls, b.getAttribute("data-toggle"));
          xs().put("/bastion/login-acls/" + x.id, Object.assign({}, x, { enabled: b.checked })).then(function () { toast((b.checked ? "已启用 " : "已停用 ") + x.name); reload(); })
            .catch(function (e) { toast("操作失败：" + errText(e)); reload(); });
        };
      });
      on(el, "data-edit", function (id) { aclForm(byId(acls, id), assets, subs, reload); });
      on(el, "data-del", function (id) {
        var x = byId(acls, id);
        U.confirmDelete("登录 ACL", x.name, "", function () { return xs().del("/bastion/login-acls/" + x.id).then(function () { toast("已删除规则 " + x.name); reload(); }); });
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = U.failBox(e); });
  }
  function aclForm(x, assets, subs, done) {
    var editing = !!x;
    var vals = editing ? { name: x.name, priority: x.priority, action: x.action, subject_type: x.subject_type, asset_ids: (x.asset_ids || []).map(String), ip_ranges: (x.ip_ranges || []).join("\n"),
      weekdays: (x.weekdays || []).map(String), time_from: x.time_from, time_to: x.time_to, description: x.description, enabled: x.enabled } : {};
    if (editing && x.subject_type !== "all") vals["subject_" + x.subject_type] = x.subject;
    var clock = /^([01]\d|2[0-3]):[0-5]\d$/;
    U.form({
      title: editing ? "编辑登录 ACL · " + x.name : "新建登录 ACL", okText: editing ? "保存" : "创建", scene: "bastion.acl", width: "lg",
      sections: [{ fields: [
        { k: "name", label: "规则名称", required: true, placeholder: "仅允许办公网工作时间" },
        { k: "priority", label: "优先级", type: "number", required: true, def: 100, min: 1, max: 100000, help: "数字越小越先匹配" },
        { k: "action", label: "动作", type: "radio", options: [{ v: "allow", t: "允许", d: "命中即放行" }, { v: "deny", t: "拒绝", d: "命中即拒绝连接" }], def: "deny" }
      ].concat(subjectFields(subs, true)).concat([
        { k: "asset_ids", label: "资产", type: "checks", options: assets.map(function (a) { return { v: String(a.id), t: U.assetLabel(a) }; }), help: "不选表示全部资产" },
        { k: "ip_ranges", label: "来源 IP", type: "textarea", rows: 3, placeholder: "每行一个：10.0.0.0/8、192.168.1.10、10.1.1.1-10.1.1.50 或 *", help: "留空表示任意来源" },
        { k: "weekdays", label: "星期", type: "checks", options: [1, 2, 3, 4, 5, 6, 7].map(function (n) { return { v: String(n), t: WEEK[n] }; }), help: "不选表示每天" },
        { k: "time_from", label: "开始时间", placeholder: "09:00", check: function (v) { return clock.test(v) ? "" : "格式为 HH:MM"; } },
        { k: "time_to", label: "结束时间", placeholder: "18:00", help: "结束早于开始表示跨天，如 22:00–06:00", check: function (v) { return clock.test(v) ? "" : "格式为 HH:MM"; } },
        { k: "description", label: "说明", type: "textarea", rows: 2 },
        { k: "enabled", label: "启用", type: "switch", def: true }
      ]) }],
      values: vals,
      toObject: function (v) {
        return { name: (v.name || "").trim(), priority: Number(v.priority), action: v.action, subject_type: v.subject_type, subject: v.subject_type === "all" ? "" : String(v["subject_" + v.subject_type] || "").trim(),
          asset_ids: (v.asset_ids || []).map(Number), ip_ranges: String(v.ip_ranges || "").split(/[\n,，;]+/).map(function (s) { return s.trim(); }).filter(Boolean),
          weekdays: (v.weekdays || []).map(Number), time_from: (v.time_from || "").trim(), time_to: (v.time_to || "").trim(), description: v.description || "", enabled: v.enabled !== false };
      },
      validate: function (v) {
        var p = Number(v.priority); if (!(p >= 1 && p <= 100000)) return "优先级需为 1–100000";
        if (!!v.time_from !== !!v.time_to) return "开始与结束时间需同时填写";
        if (v.time_from && v.time_from === v.time_to) return "开始与结束时间不能相同";
        var bad = String(v.ip_ranges || "").split(/[\n,，;]+/).map(function (s) { return s.trim(); }).filter(Boolean).filter(function (s) {
          return !(s === "*" || /^[0-9a-fA-F:.]+(\/\d{1,3})?$/.test(s) || /^[0-9a-fA-F:.]+\s*-\s*[0-9a-fA-F:.]+$/.test(s));
        });
        return bad.length ? "来源 IP 格式不正确：" + bad[0] : "";
      },
      submit: function (v, obj, ctx) {
        var r = editing ? xs().put("/bastion/login-acls/" + x.id, obj) : xs().post("/bastion/login-acls", obj);
        return r.then(function () { ctx.close(); toast("登录 ACL 已保存，立即生效"); done(); }).catch(rethrow);
      }
    });
  }

  B.register("dash", renderDash);
  B.register("platforms", renderPlatforms);
  B.register("accounts", renderAccounts);
  B.register("pwd", renderPwd);
  B.register("policy", renderPolicy);
  B.register("cmd", renderCmd);
  B.register("acl", renderAcl);
  B.util.loginResult = loginResult;
})(window);
