/*
 * 星枢 Matrix 堡垒机（对标 JumpServer）：资产、数据脱敏、登录安全三页的实时版本。
 *   资产       GET/POST/PUT/DELETE /bastion/assets，POST /bastion/assets/:id/test（真实 TCP + SSH 横幅 + 可选登录验证）
 *              左侧按「环境 → 资产组」的资产树，顶部统计，添加资产为三步向导（资产 → 托管账号 → 授权）
 *   数据脱敏   GET/POST/PUT/DELETE /bastion/mask-rules，POST /bastion/mask-rules/preview
 *              规则作用于 SSH 会话输出，在下发浏览器与写入录像之前替换
 *   登录安全   读取平台安全策略（/settings/security.policy），堡垒机与平台共用一套登录规则
 * index.html 在实时模式下调用 XSBastion.renderAssets / renderMask / renderLsec。
 */
(function (root) {
  "use strict";
  var ENV = [{ v: "prod", t: "生产" }, { v: "pre", t: "预发" }, { v: "test", t: "测试" }, { v: "dev", t: "开发" }];
  var envName = function (v) { var e = ENV.filter(function (x) { return x.v === v; })[0]; return e ? e.t : (v || "—"); };
  var ENV_CLS = { prod: "x-red", pre: "x-orange", test: "x-blue", dev: "x-green" };
  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function errText(e) { return (e && (e.message || e.error)) || String(e || "未知错误"); }
  function list(p) { return xs().get(p).then(function (r) { return Array.isArray(r) ? r : (r && (r.items || r.data)) || []; }); }
  function soft(p) { return list(p).catch(function () { return null; }); }
  function fmtTime(t) {
    if (!t) return "—";
    var s = String(t);
    if (/^\d{4}-\d{2}-\d{2}T/.test(s)) { var d = new Date(s); if (!isNaN(d)) return d.toLocaleString("zh-CN", { hour12: false, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).replace(/\//g, "-"); }
    return s.replace(/\s*\(UTC[^)]*\)$/, "").replace(/^[A-Z][a-z]{2} /, "");
  }
  function group(a) { return (a.options && a.options.group) || "未分组"; }
  function tags(a) { var t = a.options && a.options.tags; return Array.isArray(t) ? t : (t ? String(t).split(",") : []); }

  function confirmBox(title, html, okLabel, run) {
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true"><h3>' + E(title) + '</h3><div class="xb-confirm-body">' + html +
      '</div><p class="fk-err" hidden></p><div class="xb-confirm-foot"><button class="x-btn" data-no>取消</button><button class="x-btn x-btn--danger" data-yes>' + E(okLabel) + "</button></div></div>";
    document.body.appendChild(m);
    var close = function () { m.remove(); };
    m.querySelector("[data-no]").onclick = close;
    m.onclick = function (e) { if (e.target === m) close(); };
    m.querySelector("[data-yes]").onclick = function () {
      var b = this; b.disabled = true;
      Promise.resolve(run()).then(close).catch(function (e) { b.disabled = false; var p = m.querySelector(".fk-err"); p.textContent = errText(e); p.hidden = false; });
    };
  }

  /* ------------------------------------------------------------------ 资产 */
  var A = { tree: "all", q: "" };

  function renderAssets(el, hooks) {
    hooks = hooks || {};
    el.innerHTML = '<div class="xb-head"><div><h2>资产</h2><p>纳管服务器，按环境与资产组分级；登录凭据由后端加密托管，页面不保存明文。</p></div>' +
      '<div class="xb-head-act"><button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 添加资产</button></div></div>' +
      '<div class="xb-body"><p class="x-frac">正在读取资产…</p></div>';
    var reload = function () { renderAssets(el, hooks); };
    el.querySelector("[data-new]").onclick = function () { assetWizard(null, hooks, reload); };
    el.querySelector("[data-refresh]").onclick = reload;
    Promise.all([list("/bastion/assets"), soft("/bastion/accounts"), soft("/bastion/sessions?limit=200"), soft("/bastion/rules")]).then(function (r) {
      drawAssets(el, hooks, r[0], r[1] || [], r[2], r[3] || [], reload);
    }).catch(function (e) {
      el.querySelector(".xb-body").innerHTML = '<p class="x-aerr" role="alert">资产读取失败：' + E(errText(e)) + "</p>";
    });
  }

  function drawAssets(el, hooks, assets, accounts, sessions, rules, reload) {
    var accBy = {}, lastBy = {};
    accounts.forEach(function (a) { (accBy[a.asset_id] = accBy[a.asset_id] || []).push(a); });
    (sessions || []).forEach(function (s) { if (!lastBy[s.asset_id] || s.started_at > lastBy[s.asset_id]) lastBy[s.asset_id] = s.started_at; });
    var today = new Date().toISOString().slice(0, 10);
    var active = (sessions || []).filter(function (s) { return s.status === "active"; }).length;
    var todayN = (sessions || []).filter(function (s) { return String(s.started_at || "").slice(0, 10) === today; }).length;
    var stats = [
      ["资产总数", assets.length, "台服务器"],
      ["托管账号", accounts.length, "由后端加密保存"],
      ["授权规则", rules.length, "生效中的访问授权"],
      ["会话", sessions ? active + " 在线" : "—", sessions ? "今日 " + todayN + " 次连接" : "需要审计权限"]
    ];
    // 资产树：全部 → 环境 → 资产组
    var tree = {};
    assets.forEach(function (a) { var e = a.env || "dev"; tree[e] = tree[e] || {}; tree[e][group(a)] = (tree[e][group(a)] || 0) + 1; });
    var node = function (key, label, n, depth) {
      return '<button type="button" class="xb-node' + (A.tree === key ? " on" : "") + '" data-node="' + E(key) + '" style="padding-left:' + (10 + depth * 16) + 'px">' +
        '<span>' + E(label) + "</span><b>" + n + "</b></button>";
    };
    var treeHTML = node("all", "全部资产", assets.length, 0) + ENV.filter(function (e) { return tree[e.v]; }).map(function (e) {
      var groups = tree[e.v], total = Object.keys(groups).reduce(function (s, g) { return s + groups[g]; }, 0);
      return node("env:" + e.v, e.t, total, 0) + Object.keys(groups).sort().map(function (g) { return node("grp:" + e.v + ":" + g, g, groups[g], 1); }).join("");
    }).join("");
    var q = A.q.toLowerCase();
    var shown = assets.filter(function (a) {
      if (A.tree.indexOf("env:") === 0 && a.env !== A.tree.slice(4)) return false;
      if (A.tree.indexOf("grp:") === 0) { var p = A.tree.split(":"); if (a.env !== p[1] || group(a) !== p.slice(2).join(":")) return false; }
      return !q || [a.name, a.host, a.description, group(a)].concat(tags(a)).join(" ").toLowerCase().indexOf(q) >= 0;
    });
    var rows = shown.map(function (a) {
      var acc = accBy[a.id] || [];
      return "<tr><td><b class=\"x-name\">" + E(a.name) + '</b><div class="x-frac x-mono">' + E(a.host) + ":" + E(a.port) + "</div></td>" +
        '<td><span class="x-tag x-tag--s ' + (ENV_CLS[a.env] || "x-grey") + '">' + E(envName(a.env)) + "</span></td>" +
        "<td>" + E(group(a)) + (tags(a).length ? '<div class="x-tags">' + tags(a).map(function (t) { return '<span class="x-tag x-grey x-tag--s">' + E(t) + "</span>"; }).join("") + "</div>" : "") + "</td>" +
        '<td><span class="x-tag x-grey x-tag--s">SSH</span></td>' +
        "<td>" + (acc.length ? acc.map(function (x) { return E(x.username); }).join("、") : '<span class="x-frac">未托管</span>') + "</td>" +
        "<td>" + (a.host_key ? '<span class="x-tag x-green x-tag--s">已记录</span>' : '<span class="x-frac">首次连接时记录</span>') + "</td>" +
        "<td>" + E(fmtTime(lastBy[a.id])) + "</td>" +
        '<td class="xb-ops"><button class="x-btn x-btn--pri x-btn--s" data-conn="' + a.id + '"' + (acc.length ? "" : ' disabled title="先托管一个登录账号"') + '>连接</button>' +
        '<button class="x-act" data-test="' + a.id + '">测试</button><button class="x-act" data-edit="' + a.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + a.id + '">删除</button></td></tr>';
    }).join("");
    var body = el.querySelector(".xb-body");
    body.innerHTML = '<div class="xb-stats">' + stats.map(function (s) { return '<div class="xb-stat"><span>' + s[0] + "</span><b>" + E(s[1]) + "</b><em>" + E(s[2]) + "</em></div>"; }).join("") + "</div>" +
      '<div class="xb-split"><aside class="xb-tree" aria-label="资产树"><div class="xb-tree-h">资产树</div>' + treeHTML + "</aside>" +
      '<section class="xb-main"><div class="xb-toolbar"><input class="x-input" data-q placeholder="搜索名称、地址、资产组、标签" value="' + E(A.q) + '"><span class="x-frac">共 ' + shown.length + " 台</span></div>" +
      (assets.length ? (shown.length ? '<div class="xb-table"><table><thead><tr><th>资产</th><th>环境</th><th>资产组 / 标签</th><th>协议</th><th>托管账号</th><th>主机指纹</th><th>最近连接</th><th>操作</th></tr></thead><tbody>' + rows + "</tbody></table></div>"
        : '<p class="xb-empty">没有符合条件的资产。</p>')
        : '<div class="xb-guide"><h3>还没有纳管任何服务器</h3><ol><li>添加资产：填写地址和端口，选择环境与资产组</li><li>托管账号：登录用户名和密码或私钥，后端加密保存</li><li>授权：把资产授权给自己或角色，即可在「Web 终端」连接</li></ol><button class="x-btn x-btn--pri" data-new2>+ 添加第一台资产</button></div>') +
      '<p class="x-frac xb-note">目前支持 SSH；RDP / VNC 需要部署 guacd 连接器，数据库代理尚未提供。</p></section></div>';
    var n2 = body.querySelector("[data-new2]"); if (n2) n2.onclick = function () { assetWizard(null, hooks, reload); };
    [].forEach.call(body.querySelectorAll("[data-node]"), function (b) { b.onclick = function () { A.tree = b.getAttribute("data-node"); drawAssets(el, hooks, assets, accounts, sessions, rules, reload); }; });
    var qi = body.querySelector("[data-q]");
    qi.oninput = function () { A.q = qi.value; clearTimeout(qi._t); qi._t = setTimeout(function () { drawAssets(el, hooks, assets, accounts, sessions, rules, reload); var n = el.querySelector("[data-q]"); n.focus(); n.setSelectionRange(n.value.length, n.value.length); }, 250); };
    var byId = function (id) { return assets.filter(function (a) { return String(a.id) === String(id); })[0]; };
    [].forEach.call(body.querySelectorAll("[data-conn]"), function (b) { b.onclick = function () { var a = byId(b.getAttribute("data-conn")); if (a && hooks.connect) hooks.connect(a); }; });
    [].forEach.call(body.querySelectorAll("[data-test]"), function (b) { b.onclick = function () { testAsset(byId(b.getAttribute("data-test")), accBy); }; });
    [].forEach.call(body.querySelectorAll("[data-edit]"), function (b) { b.onclick = function () { assetWizard(byId(b.getAttribute("data-edit")), hooks, reload); }; });
    [].forEach.call(body.querySelectorAll("[data-del]"), function (b) {
      b.onclick = function () {
        var a = byId(b.getAttribute("data-del"));
        confirmBox("删除资产", "确定删除 <b>" + E(a.name) + "</b>（" + E(a.host) + "）？它的托管账号会一并删除，引用它的授权规则需要单独调整。", "删除", function () {
          return xs().del("/bastion/assets/" + a.id).then(function () { toast("已删除资产 " + a.name); reload(); });
        });
      };
    });
  }

  function testAsset(a, accBy) {
    var acc = (accBy[a.id] || [])[0];
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true"><h3>连通性测试 · ' + E(a.name) + '</h3><div class="xb-confirm-body"><p class="x-frac">正在从平台连接 ' + E(a.host) + ":" + E(a.port) + "…</p></div>" +
      '<div class="xb-confirm-foot"><button class="x-btn" data-no>关闭</button></div></div>';
    document.body.appendChild(m);
    m.querySelector("[data-no]").onclick = function () { m.remove(); };
    xs().post("/bastion/assets/" + a.id + "/test", acc ? { account_id: acc.id } : {}).then(function (r) {
      var line = function (ok, t, d) { return '<li class="' + (ok ? "ok" : ok === false ? "bad" : "") + '"><b>' + (ok ? "✓" : ok === false ? "✕" : "·") + "</b><span>" + E(t) + (d ? '<em>' + E(d) + "</em>" : "") + "</span></li>"; };
      var items = [line(r.reachable, "网络连通", r.reachable ? "延迟 " + r.latency_ms + " ms" : r.error)];
      if (r.reachable) items.push(line(!!r.banner, "SSH 服务", r.banner || "没有读到 SSH 横幅，端口上可能不是 SSH 服务"));
      if (r.reachable && acc) items.push(line(r.auth, "账号登录（" + acc.username + "）", r.auth ? "登录成功，未打开 Shell" : r.error));
      if (r.reachable && !acc) items.push(line(null, "账号登录", "还没有托管账号，跳过"));
      m.querySelector(".xb-confirm-body").innerHTML = '<ul class="xb-check">' + items.join("") + "</ul>";
    }).catch(function (e) { m.querySelector(".xb-confirm-body").innerHTML = '<p class="x-aerr">测试失败：' + E(errText(e)) + "</p>"; });
  }

  function assetWizard(a, hooks, done) {
    var editing = !!a, u = xs().user || {}, me = u.username || "", meId = u.user_id != null ? String(u.user_id) : (u.id != null ? String(u.id) : "");
    var sections = [{
      title: "资产信息", desc: "平台从自身所在网络连接这台服务器，请确认地址可达。",
      fields: [
        { k: "name", label: "资产名称", required: true, placeholder: "web-01", help: "小写字母、数字、- _ .，全局唯一" },
        { k: "host", label: "地址", required: true, placeholder: "10.0.1.11 或 web-01.internal" },
        { k: "port", label: "SSH 端口", type: "number", required: true, def: 22 },
        { k: "env", label: "环境", type: "radio", options: ENV, def: "dev" },
        { k: "group", label: "资产组", placeholder: "Web 集群", help: "资产树按「环境 → 资产组」展示" },
        { k: "tags", label: "标签", type: "tags", placeholder: "输入后回车，例如 nginx" },
        { k: "description", label: "说明", type: "textarea", placeholder: "用途、负责人等" }
      ]
    }];
    if (!editing) {
      sections.push({
        title: "托管账号", desc: "登录凭据加密保存在后端，任何页面和 API 都不会返回明文。可以跳过，稍后在「账号」页添加。",
        fields: [
          { k: "with_account", label: "现在添加登录账号", type: "switch", def: true },
          { k: "username", label: "登录用户名", placeholder: "root 或 ops", show: function (v) { return v.with_account; } },
          { k: "auth_type", label: "认证方式", type: "radio", options: [{ v: "password", t: "密码" }, { v: "private_key", t: "SSH 私钥" }], def: "password", show: function (v) { return v.with_account; } },
          { k: "password", label: "密码", type: "password", secret: true, show: function (v) { return v.with_account && v.auth_type === "password"; } },
          { k: "private_key", label: "私钥（PEM）", type: "code", secret: true, placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----", show: function (v) { return v.with_account && v.auth_type === "private_key"; } },
          { k: "passphrase", label: "私钥口令", type: "password", secret: true, help: "私钥没有口令就留空", show: function (v) { return v.with_account && v.auth_type === "private_key"; } }
        ]
      }, {
        title: "授权", desc: "授权后才能在「Web 终端」里看到并连接这台资产。更细的授权在「访问策略 → 授权策略」里管理。",
        fields: [
          { k: "grant_me", label: "授权给我（" + (me || "当前用户") + "）", type: "switch", def: true },
          { k: "expire", label: "有效期", type: "radio", options: [{ v: "0", t: "长期" }, { v: "7", t: "7 天" }, { v: "30", t: "30 天" }], def: "0", show: function (v) { return v.grant_me; } }
        ]
      });
    }
    var vals = editing ? { name: a.name, host: a.host, port: a.port, env: a.env, group: (a.options || {}).group || "", tags: tags(a), description: a.description || "" } : {};
    var toObj = function (v) {
      var o = { name: v.name, host: v.host, port: Number(v.port) || 22, protocol: "ssh", env: v.env || "dev", description: v.description || "", options: Object.assign({}, editing ? a.options : {}, { group: v.group || "", tags: v.tags || [] }) };
      if (!v.group) delete o.options.group;
      return o;
    };
    root.XSFormKit.open({
      title: editing ? "编辑资产 · " + a.name : "添加资产", okText: editing ? "保存" : "添加", scene: "bastion.asset", wizard: !editing, width: "lg",
      aiExample: "生产环境的支付网关 pay-gw-01，地址 10.20.1.15，端口 22，资产组 支付",
      sections: sections, values: vals, toObject: toObj,
      fromObject: function (o, v) { return Object.assign({}, v, { name: o.name, host: o.host, port: o.port, env: o.env, description: o.description, group: (o.options || {}).group || "", tags: (o.options || {}).tags || [] }); },
      validate: function (v) {
        if (!/^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/.test(v.name || "")) return "资产名称只能用小写字母、数字、- _ .";
        if (!/^[A-Za-z0-9.:_-]+$/.test(v.host || "")) return "请填写合法的 IP 或域名";
        var p = Number(v.port); if (!(p >= 1 && p <= 65535)) return "端口需在 1–65535";
        if (!editing && v.with_account) {
          if (!v.username) return "请填写登录用户名，或关闭「现在添加登录账号」";
          if (v.auth_type === "password" && !v.password) return "请填写密码";
          if (v.auth_type === "private_key" && !/-----BEGIN/.test(v.private_key || "")) return "请粘贴 PEM 格式的私钥";
        }
        return "";
      },
      submit: function (v, obj, ctx) {
        if (editing) return xs().put("/bastion/assets/" + a.id, Object.assign({ host_key: a.host_key || "" }, obj)).then(function () { ctx.close(); toast("资产已更新"); done(); });
        var asset, account;
        ctx.status("正在登记资产…");
        return xs().post("/bastion/assets", obj).then(function (r) {
          asset = r;
          if (!v.with_account) return null;
          ctx.status("正在加密保存账号…");
          return xs().post("/bastion/accounts", { asset_id: asset.id, name: v.username + "@" + asset.name, username: v.username, auth_type: v.auth_type,
            secret: v.auth_type === "password" ? v.password : v.private_key, passphrase: v.auth_type === "private_key" ? (v.passphrase || "") : "" });
        }).then(function (acc) {
          account = acc;
          if (!v.grant_me || !meId) return null;
          ctx.status("正在创建授权…");
          var body = { name: "授权-" + asset.name + "-" + me, subject_type: "user", subject: meId, asset_ids: [asset.id], account_ids: account ? [account.id] : [] };
          if (v.expire && v.expire !== "0") body.expires_at = new Date(Date.now() + Number(v.expire) * 864e5).toISOString();
          return xs().post("/bastion/rules", body);
        }).then(function () {
          ctx.close(); toast("资产 " + asset.name + " 已添加" + (account ? "，可以点「测试」验证登录" : "")); done();
        }).catch(function (e) {
          if (asset) { ctx.close(); toast("资产已登记，但后续步骤失败：" + errText(e)); done(); return; }
          throw e;
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
    el.innerHTML = '<div class="xb-head"><div><h2>数据脱敏</h2><p>SSH 会话里的输出在发到浏览器、写入录像之前按规则替换，运维人员与回放都看不到原文。</p></div>' +
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
          confirmBox("删除脱敏规则", "确定删除 <b>" + E(r.name) + "</b>？之后的新会话将不再脱敏这类内容。", "删除", function () {
            return xs().del("/bastion/mask-rules/" + r.id).then(function () { toast("已删除规则 " + r.name); reload(); });
          });
        };
      });
    }).catch(function (e) { el.querySelector(".xb-body").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
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
        return p.then(function () { ctx.close(); toast(editing ? "规则已更新" : "规则已创建，新会话立即生效"); done(); });
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
        ["堡垒机连接条件", "连接权限 + 生效的授权规则 + 环境范围允许"],
        ["会话录像", "所有 SSH 会话强制录像，录像存储不可用时拒绝连接"]
      ];
      body.innerHTML = (s ? "" : '<p class="x-frac" style="margin:0 0 10px">尚未自定义，当前为平台默认值。</p>') +
        '<div class="xb-kv">' + items.map(function (i) { return "<div><span>" + E(i[0]) + "</span><b>" + E(i[1]) + "</b></div>"; }).join("") + "</div>";
    };
    xs().get("/settings/security.policy").then(paint, function () { paint(null); });
  }

  root.XSBastion = { renderAssets: renderAssets, renderMask: renderMask, renderLsec: renderLsec };
})(window);
