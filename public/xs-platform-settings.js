(function (root) {
  "use strict";
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function api(path, options) { return root.XS.api(path, options || {}); }
  function notice(msg) { if (typeof root.toast === "function") root.toast(msg); else if (root.XS && root.XS.toast) root.XS.toast(msg); }
  function list(data, key) { return Array.isArray(data) ? data : (data && Array.isArray(data[key]) ? data[key] : []); }
  function error(err) { return err && err.message || "服务暂不可用"; }
  function field(label, value, attr) { return '<label><span>' + label + '</span><input class="x-input" ' + attr + ' value="' + esc(value) + '"></label>'; }
  function render(el) {
    if (!el) return;
    var serial = (el._xsPlatformSerial || 0) + 1;
    el._xsPlatformSerial = serial;
    el.innerHTML = '<div class="xs-platform"><div class="xs-platform-head"><h2>系统设置</h2><button class="x-btn" type="button" data-ps-refresh>刷新</button></div><p role="status">正在加载平台设置...</p></div>';
    function active() { return el._xsPlatformSerial === serial; }
    Promise.allSettled([api("/settings"), api("/platform/domain"), api("/env-policies"), api("/platform/notices"), api("/version"), api("/auth/sso/config")]).then(function (results) {
      if (!active()) return;
      var settings = results[0].status === "fulfilled" ? list(results[0].value, "items") : [];
      var policies = results[2].status === "fulfilled" ? list(results[2].value, "items") : [];
      var domain = results[1].status === "fulfilled" ? results[1].value || {} : {};
      var notices = results[3].status === "fulfilled" ? list(results[3].value, "items") : [];
      var address = root.location && root.location.origin || "";
      function val(key) { var found = settings.find(function (s) { return s.key === key && !s.secret; }); return found ? (typeof found.value === "string" ? found.value : "") : ""; }
      function warning(index) { return results[index].status === "rejected" ? '<p class="x-aerr" role="alert">' + esc(error(results[index].reason)) + '</p>' : ""; }
      var policyRows = policies.map(function (p) { return '<tr><td>' + esc(p.env) + '</td><td>' + esc(p.name || p.env) + '</td><td>' + (p.require_approval ? "需要审批" : "无需审批") + '</td><td>' + esc(p.approver_permission || "-") + '</td><td><button class="x-btn" data-ps-env="' + esc(p.env) + '">编辑</button></td></tr>'; }).join("");
      el.innerHTML = '<div class="xs-platform"><div class="xs-platform-head"><h2>系统设置</h2><button class="x-btn" type="button" data-ps-refresh>刷新</button></div>' +
        '<section class="xs-platform-section"><h3>平台基础信息</h3>' + warning(0) + '<form data-ps-name>' + field("平台名称", val("platform.name"), 'name="name" maxlength="100" required') + '<button class="x-btn x-btn--pri" type="submit"' + (results[0].status === "rejected" ? " disabled" : "") + '>保存名称</button></form></section>' +
        '<section class="xs-platform-section"><h3>访问域名</h3>' + warning(1) + '<p class="x-frac">当前访问地址 <code>' + esc(address) + '</code>' + (domain.ingress_available ? ' · Ingress 可用' : ' · 当前使用节点端口访问') + '</p><form data-ps-domain>' + field("平台访问域名", domain.domain || "", 'name="domain" placeholder="matrix.example.com"') + '<button class="x-btn x-btn--pri" type="submit"' + (results[1].status === "rejected" ? " disabled" : "") + '>保存域名</button></form></section>' +
        '<section class="xs-platform-section"><h3>平台公告</h3>' + warning(3) + (notices.length ? '<div class="xs-platform-list">' + notices.map(function (n) { return '<p><strong>' + esc(n.title) + '</strong><br>' + esc(n.body) + '</p>'; }).join("") + '</div>' : '<p class="x-frac">' + (results[3].status === "rejected" ? "公告暂不可用" : "暂无公告") + '</p>') + '<a class="x-btn" href="#/sre">在总览管理公告</a></section>' +
        '<section class="xs-platform-section"><h3>平台管理员与认证</h3><div class="xs-platform-links"><a class="x-btn" href="#/iam/users">用户管理</a><a class="x-btn" href="#/iam/roles">角色权限</a><span class="x-frac">企业 SSO：' + (results[5].status === "fulfilled" && results[5].value && results[5].value.enabled ? "已启用" : "未配置，需先在后端配置 OIDC") + '</span></div></section>' +
        '<section class="xs-platform-section"><h3>版本与运行信息</h3>' + (results[4].status === "fulfilled" ? '<div class="xs-platform-links"><span>版本 <code>' + esc((results[4].value || {}).version) + '</code></span><span>运行平台 ' + esc((results[4].value || {}).platform) + '</span><span>Go ' + esc((results[4].value || {}).go_version) + '</span><span>提交 ' + esc((results[4].value || {}).commit) + '</span></div>' : '<p class="x-frac">版本信息暂不可用</p>') + '</section>' +
        '<section class="xs-platform-section"><h3>通知与集成</h3><div class="xs-platform-links"><a class="x-btn" href="#/observe/events">告警与通知渠道</a><a class="x-btn" href="#/cluster/integrations">数据源与集成</a><a class="x-btn" href="#/cluster/registries">镜像仓库</a><a class="x-btn" href="#/cluster/components">组件目录</a><span class="x-frac">企业 SSO：' + (results[5].status === "fulfilled" && results[5].value && results[5].value.enabled ? "已启用" : "未配置") + '</span></div></section>' +
        '<section class="xs-platform-section"><h3>平台能力</h3><div class="xs-platform-links"><a class="x-btn" href="#/iam/logs">登录与操作审计</a><a class="x-btn" href="#/cluster/backup">Kubernetes 备份恢复（Velero）</a></div></section>' +
        '<section class="xs-platform-section"><h3>环境审批策略</h3>' + warning(2) + (policyRows ? '<div class="xs-platform-scroll"><table class="x-table"><thead><tr><th>环境</th><th>名称</th><th>变更审批</th><th>审批权限</th><th>操作</th></tr></thead><tbody>' + policyRows + '</tbody></table></div>' : '<p class="x-frac">' + (results[2].status === "rejected" ? "环境策略不可用" : "暂无环境策略") + '</p>') + '</section></div>';
      el.querySelector("[data-ps-refresh]").onclick = function () { render(el); };
      function submit(form, path, body) {
        var button = form.querySelector('[type="submit"]'); button.disabled = true;
        api(path, { method: "PUT", body: body }).then(function () { notice("已保存"); render(el); }, function (err) { notice("保存失败：" + error(err)); button.disabled = false; });
      }
      el.querySelector("[data-ps-name]").onsubmit = function (event) { event.preventDefault(); submit(this, "/settings/platform.name", { value: this.elements.name.value.trim(), secret: false, description: "平台名称" }); };
      el.querySelector("[data-ps-domain]").onsubmit = function (event) { event.preventDefault(); submit(this, "/platform/domain", { domain: this.elements.domain.value.trim() }); };
      Array.prototype.forEach.call(el.querySelectorAll("[data-ps-env]"), function (button) {
        button.onclick = function () {
          var p = policies.find(function (item) { return String(item.env) === button.getAttribute("data-ps-env"); });
          if (!p) return;
          var panel = el.querySelector(".xs-platform-policy"); if (panel) panel.remove();
          panel = document.createElement("form"); panel.className = "xs-platform-policy x-form";
          panel.innerHTML = '<h4>编辑环境策略：' + esc(p.env) + '</h4>' + field("环境名称", p.name || p.env, 'name="name" required') + '<label><input name="approval" type="checkbox"' + (p.require_approval ? " checked" : "") + '> 变更需要审批</label>' + field("审批权限码", p.approver_permission || "approval:approve", 'name="permission" required') + '<button class="x-btn x-btn--pri" type="submit">保存策略</button>';
          button.closest("section").appendChild(panel);
          panel.onsubmit = function (event) { event.preventDefault(); submit(panel, "/env-policies/" + encodeURIComponent(p.env), { name: panel.elements.name.value.trim(), require_approval: panel.elements.approval.checked, approver_permission: panel.elements.permission.value.trim() }); };
        };
      });
    });
  }
  root.XSPlatformSettings = { render: render };
})(window);
