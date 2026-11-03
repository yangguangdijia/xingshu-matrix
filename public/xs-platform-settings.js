/* 系统设置 —— 配置中心（左侧分类 + 右侧表单）。所有表单读写真实接口：
 *   基本信息   GET/PUT /settings/platform.name、GET/PUT /platform/domain
 *   安全策略   GET/PUT /settings/security.policy   （后端登录 / 改密 / 会话即时生效）
 *   审计与保留 GET/PUT /settings/audit.retention   （后端每 6 小时按天数清理）
 *   环境与审批 GET /env-policies、PUT /env-policies/:env
 *   关于       GET /version
 */
(function (root) {
  "use strict";
  var TABS = [
    { k: "basic", t: "基本信息" },
    { k: "security", t: "安全策略" },
    { k: "retention", t: "审计与保留" },
    { k: "env", t: "环境与审批" },
    { k: "integrations", t: "通知与集成" },
    { k: "about", t: "关于平台" }
  ];
  var SEC_DEFAULT = { password_min_length: 8, password_min_classes: 3, max_login_failures: 5, lockout_minutes: 15, session_hours: 168 };
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function api(path, options) { return root.XS.api(path, options || {}); }
  function toast(msg) { if (typeof root.toast === "function") root.toast(msg); else if (root.XS && root.XS.toast) root.XS.toast(msg); }
  function list(data, key) { return Array.isArray(data) ? data : (data && Array.isArray(data[key]) ? data[key] : []); }
  function errMsg(err) { return err && err.message || "服务暂不可用"; }
  function getSetting(key) {
    return api("/settings/" + key).then(function (s) { return s && s.value != null ? s.value : null; }, function (e) {
      if (e && Number(e.status) === 404) return null; throw e;
    });
  }
  function css() {
    if (document.getElementById("xs-cfg-css")) return;
    var s = document.createElement("style"); s.id = "xs-cfg-css";
    s.textContent = [
      ".xs-cfg{display:grid;grid-template-columns:200px minmax(0,1fr);gap:20px;align-items:start}",
      ".xs-cfg-nav{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:8px;position:sticky;top:12px}",
      ".xs-cfg-nav button{display:block;width:100%;text-align:left;border:0;background:none;padding:10px 14px;border-radius:7px;font-size:14px;color:var(--x-text,#1d2129);cursor:pointer}",
      ".xs-cfg-nav button:hover{background:var(--x-hover,#f3f5f8)}.xs-cfg-nav button.on{background:#eaf1ff;color:var(--x-blue,#2563eb);font-weight:600}",
      ".xs-cfg-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:24px 28px;margin-bottom:20px}",
      ".xs-cfg-card h3{margin:0 0 20px;font-size:16px}",
      ".xs-cfg-row{display:grid;grid-template-columns:180px minmax(0,420px);gap:20px;align-items:center;padding:12px 0;border-bottom:1px solid var(--x-line,#f0f2f5)}",
      ".xs-cfg-row:last-of-type{border-bottom:0}.xs-cfg-row>span{font-size:14px;color:var(--x-sub,#4b5563)}",
      ".xs-cfg-row .x-input{width:100%;height:36px}.xs-cfg-row code{font-size:13px}",
      ".xs-cfg-unit{display:flex;align-items:center;gap:10px}.xs-cfg-unit .x-input{width:140px}.xs-cfg-unit em{font-style:normal;color:var(--x-sub,#6b7280);font-size:13px}",
      ".xs-cfg-foot{display:flex;gap:10px;margin-top:20px;padding-top:20px;border-top:1px solid var(--x-line,#f0f2f5)}",
      ".xs-cfg-links{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:14px}",
      ".xs-cfg-links a{display:block;padding:16px 18px;border:1px solid var(--x-line,#e6e9ed);border-radius:8px;text-decoration:none;color:inherit}",
      ".xs-cfg-links a:hover{border-color:var(--x-blue,#2563eb)}.xs-cfg-links strong{display:block;font-size:14px}",
      ".xs-cfg-table{width:100%;border-collapse:collapse;font-size:14px}.xs-cfg-table th,.xs-cfg-table td{padding:12px 10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left}",
      ".xs-cfg-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xs-cfg-switch{display:inline-flex;align-items:center;gap:8px;cursor:pointer}",
      ".xs-cfg-err{color:#b91c1c;font-size:13px;margin:0 0 12px}",
      "@media(max-width:960px){.xs-cfg{grid-template-columns:1fr}.xs-cfg-nav{position:static;display:flex;overflow:auto}.xs-cfg-row{grid-template-columns:1fr;gap:8px}.xs-cfg-links{grid-template-columns:1fr}}"
    ].join("\n");
    document.head.appendChild(s);
  }
  function row(label, control) { return '<div class="xs-cfg-row"><span>' + esc(label) + '</span><div>' + control + '</div></div>'; }
  function num(name, value, min, max, unit) {
    return '<div class="xs-cfg-unit"><input class="x-input" type="number" name="' + name + '" min="' + min + '" max="' + max + '" value="' + esc(value) + '" required><em>' + esc(unit) + '</em></div>';
  }
  function foot(label) { return '<div class="xs-cfg-foot"><button class="x-btn x-btn--pri" type="submit">' + esc(label || "保存") + '</button><button class="x-btn" type="reset">还原</button></div>'; }

  function render(el) {
    if (!el) return;
    css();
    var tab = (el._xsCfgTab) || (location.hash.split("/")[2] || "basic");
    if (!TABS.some(function (t) { return t.k === tab; })) tab = "basic";
    el._xsCfgTab = tab;
    var serial = (el._xsCfgSerial || 0) + 1; el._xsCfgSerial = serial;
    function active() { return el._xsCfgSerial === serial; }
    el.innerHTML = '<div class="x-sethead"><div><h1 class="x-h1">系统设置</h1></div><div class="x-phacts"><button class="x-btn" type="button" data-cfg-refresh>刷新</button></div></div>' +
      '<div class="xs-cfg"><nav class="xs-cfg-nav">' + TABS.map(function (t) {
        return '<button type="button" data-cfg-tab="' + t.k + '" class="' + (t.k === tab ? "on" : "") + '">' + t.t + '</button>';
      }).join("") + '</nav><div data-cfg-body><section class="xs-cfg-card"><p>正在加载…</p></section></div></div>';
    el.querySelector("[data-cfg-refresh]").onclick = function () { render(el); };
    [].forEach.call(el.querySelectorAll("[data-cfg-tab]"), function (b) {
      b.onclick = function () { el._xsCfgTab = b.getAttribute("data-cfg-tab"); render(el); };
    });
    var body = el.querySelector("[data-cfg-body]");
    function fail(e) { if (active()) body.innerHTML = '<section class="xs-cfg-card"><p class="xs-cfg-err">' + esc(errMsg(e)) + '</p></section>'; }
    function bindSave(form, fn) {
      form.onsubmit = function (ev) {
        ev.preventDefault();
        var btn = form.querySelector("[type=submit]"); btn.disabled = true;
        Promise.resolve().then(fn).then(function () { toast("已保存"); render(el); }, function (e) { toast("保存失败：" + errMsg(e)); btn.disabled = false; });
      };
    }
    var V = function (f, n) { return f.elements[n].value.trim(); };
    var N = function (f, n) { return parseInt(f.elements[n].value, 10) || 0; };

    if (tab === "basic") {
      Promise.all([getSetting("platform.name"), api("/platform/domain").catch(function () { return {}; })]).then(function (r) {
        if (!active()) return;
        var d = r[1] || {};
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>基本信息</h3>' +
          row("平台名称", '<input class="x-input" name="name" maxlength="100" value="' + esc(typeof r[0] === "string" ? r[0] : "星枢 Matrix") + '" required>') +
          row("平台访问域名", '<input class="x-input" name="domain" placeholder="matrix.example.com" value="' + esc(d.domain || "") + '">') +
          row("当前访问地址", '<code>' + esc(location.origin) + '</code>') +
          row("Ingress", d.ingress_available ? "可用" : "未检测到 Ingress 控制器") + foot() + '</form>';
        var f = body.querySelector("[data-f]");
        bindSave(f, function () {
          return Promise.all([
            api("/settings/platform.name", { method: "PUT", body: { value: V(f, "name"), secret: false, description: "平台名称" } }),
            V(f, "domain") !== (d.domain || "") ? api("/platform/domain", { method: "PUT", body: { domain: V(f, "domain") } }) : null
          ]);
        });
      }, fail);
    } else if (tab === "security") {
      getSetting("security.policy").then(function (v) {
        if (!active()) return;
        var p = Object.assign({}, SEC_DEFAULT, v || {});
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>密码策略</h3>' +
          row("最小长度", num("password_min_length", p.password_min_length, 8, 64, "位（8-64）")) +
          row("字符类别", '<select class="x-input" name="password_min_classes" style="width:280px">' + [2, 3, 4].map(function (n) {
            return '<option value="' + n + '"' + (n === p.password_min_classes ? " selected" : "") + '>大小写字母、数字、符号中至少 ' + n + ' 类</option>'; }).join("") + '</select>') +
          '<h3 style="margin-top:28px">登录与会话</h3>' +
          row("连续失败锁定", num("max_login_failures", p.max_login_failures, 3, 20, "次")) +
          row("锁定时长", num("lockout_minutes", p.lockout_minutes, 5, 1440, "分钟")) +
          row("会话有效期", num("session_hours", p.session_hours, 1, 720, "小时")) + foot() + '</form>';
        var f = body.querySelector("[data-f]");
        bindSave(f, function () {
          return api("/settings/security.policy", { method: "PUT", body: { secret: false, description: "安全策略", value: {
            password_min_length: N(f, "password_min_length"), password_min_classes: N(f, "password_min_classes"),
            max_login_failures: N(f, "max_login_failures"), lockout_minutes: N(f, "lockout_minutes"), session_hours: N(f, "session_hours") } } });
        });
      }, fail);
    } else if (tab === "retention") {
      getSetting("audit.retention").then(function (v) {
        if (!active()) return;
        var p = Object.assign({ audit_days: 0, command_days: 0 }, v || {});
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>数据保留</h3>' +
          row("审计日志", num("audit_days", p.audit_days, 0, 3650, "天，0 为永久")) +
          row("堡垒机命令记录", num("command_days", p.command_days, 0, 3650, "天，0 为永久")) +
          row("查看", '<a class="x-btn" href="#/iam/logs">登录与操作审计</a>') + foot() + '</form>';
        var f = body.querySelector("[data-f]");
        bindSave(f, function () {
          return api("/settings/audit.retention", { method: "PUT", body: { secret: false, description: "审计与记录保留天数", value: { audit_days: N(f, "audit_days"), command_days: N(f, "command_days") } } });
        });
      }, fail);
    } else if (tab === "env") {
      api("/env-policies").then(function (d) {
        if (!active()) return;
        var ps = list(d, "items");
        body.innerHTML = '<section class="xs-cfg-card"><h3>环境与变更审批</h3><table class="xs-cfg-table"><thead><tr><th>环境</th><th>名称</th><th>变更审批</th><th>审批权限</th><th style="width:90px">操作</th></tr></thead><tbody>' +
          ps.map(function (p) {
            return '<tr><td><code>' + esc(p.env) + '</code></td><td>' + esc(p.name || p.env) + '</td><td>' + (p.require_approval ? '<span class="x-tag x-orange">需要审批</span>' : '<span class="x-tag x-grey">无需审批</span>') +
              '</td><td><code>' + esc(p.approver_permission || "-") + '</code></td><td><button class="x-btn" type="button" data-env="' + esc(p.env) + '">编辑</button></td></tr>';
          }).join("") + '</tbody></table><div data-env-form></div></section>';
        [].forEach.call(body.querySelectorAll("[data-env]"), function (b) {
          b.onclick = function () {
            var p = ps.filter(function (x) { return String(x.env) === b.getAttribute("data-env"); })[0]; if (!p) return;
            var box = body.querySelector("[data-env-form]");
            box.innerHTML = '<form data-ef style="margin-top:24px"><h3>编辑环境：' + esc(p.env) + '</h3>' +
              row("环境名称", '<input class="x-input" name="name" value="' + esc(p.name || p.env) + '" required>') +
              row("变更审批", '<label class="xs-cfg-switch"><input type="checkbox" name="approval"' + (p.require_approval ? " checked" : "") + '> 部署与变更需审批通过后执行</label>') +
              row("审批权限码", '<input class="x-input" name="permission" value="' + esc(p.approver_permission || "approval:approve") + '" required>') + foot("保存策略") + '</form>';
            var f = box.querySelector("[data-ef]");
            bindSave(f, function () {
              return api("/env-policies/" + encodeURIComponent(p.env), { method: "PUT", body: { name: V(f, "name"), require_approval: f.elements.approval.checked, approver_permission: V(f, "permission") } });
            });
          };
        });
      }, fail);
    } else if (tab === "integrations") {
      var links = [
        ["告警与通知渠道", "#/observe/events"], ["数据源绑定", "#/cluster/integrations"], ["镜像仓库", "#/cluster/registries"],
        ["组件目录", "#/cluster/components"], ["AI 模型与提示词", "#/ai/config"], ["备份恢复", "#/cluster/backup"],
        ["用户管理", "#/iam/users"], ["角色权限", "#/iam/roles"], ["登录与操作审计", "#/iam/logs"]
      ];
      body.innerHTML = '<section class="xs-cfg-card"><h3>通知与集成</h3><div class="xs-cfg-links">' + links.map(function (l) {
        return '<a href="' + l[1] + '"><strong>' + esc(l[0]) + '</strong></a>'; }).join("") + '</div></section>';
    } else if (tab === "about") {
      api("/version").then(function (v) {
        if (!active()) return;
        v = v || {};
        body.innerHTML = '<section class="xs-cfg-card"><h3>关于平台</h3>' +
          row("版本", '<code>' + esc(v.version) + '</code>') + row("提交", '<code>' + esc(v.commit) + '</code>') +
          row("构建时间", esc(v.build_time)) + row("运行平台", esc(v.platform)) + row("Go 版本", esc(v.go_version)) + '</section>';
      }, fail);
    }
  }
  root.XSPlatformSettings = { render: render };
})(window);
