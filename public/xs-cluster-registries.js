(function (root) {
  "use strict";
  var TYPE = "kubernetes.io/dockerconfigjson";
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function api(path, options) { return root.XS.api(path, options || {}); }
  function message(err) { return err && err.message || "请求失败"; }
  function notify(msg) { if (typeof root.toast === "function") root.toast(msg); else if (root.XS && root.XS.toast) root.XS.toast(msg); }
  function context() { var c = root.XSClusterContext; return c && typeof c.get === "function" ? c.get() : null; }
  function validEndpoint(text) {
    try { var u = new URL(text); return /^(http:|https:)$/.test(u.protocol) && !!u.hostname && !u.username && !u.password && !u.search && !u.hash && !/\s/.test(text); } catch (_) { return false; }
  }
  function render(el) {
    if (!el) return;
    var token = (el._xsRegistryToken || 0) + 1; el._xsRegistryToken = token;
    var selected = context(); var id = selected && selected.kind === "cluster" && selected.cluster_id;
    el.innerHTML = '<div class="xs-platform"><div class="xs-platform-head"><h2>集群镜像源与镜像仓库</h2><button class="x-btn" type="button" data-reg-refresh>刷新</button></div><section class="xs-platform-section"><h3>当前集群拉取凭据</h3><div data-reg-secrets role="status">' + (id ? "正在读取 Secret 元数据..." : "请选择单个目标集群") + '</div></section><section class="xs-platform-section"><h3>新 K3s 集群镜像加速模板</h3><div data-reg-mirror role="status">正在加载模板...</div></section></div>';
    el.querySelector("[data-reg-refresh]").onclick = function () { render(el); };
    var secretsBox = el.querySelector("[data-reg-secrets]"); var mirrorBox = el.querySelector("[data-reg-mirror]");
    function active() { return el._xsRegistryToken === token && (!id || (context() && context().kind === "cluster" && String(context().cluster_id) === String(id))); }
    if (id) api("/clusters/" + encodeURIComponent(id) + "/config/secrets", { scope: "cluster" }).then(function (data) {
      if (!active()) return;
      var rows = (Array.isArray(data) ? data : data && data.items || []).filter(function (s) { return s.type === TYPE; });
      var canWrite = typeof root.XS.can === "function" && root.XS.can("config:write");
      secretsBox.innerHTML = (rows.length ? '<div class="xs-platform-scroll"><table class="x-table"><thead><tr><th>名称</th><th>命名空间</th><th>键名（仅元数据）</th></tr></thead><tbody>' + rows.map(function (s) { return '<tr><td>' + esc(s.name) + '</td><td>' + esc(s.namespace) + '</td><td>' + esc((s.keys || []).join(", ")) + '</td></tr>'; }).join("") + '</tbody></table></div>' : '<p class="x-frac">当前集群未发现镜像拉取 Secret。</p>') +
        '<p class="x-frac">仅显示 Secret 名称、命名空间和键名；无法据此判断是否已被工作负载使用。</p>' +
        (canWrite ? '<button class="x-btn" type="button" data-reg-import>导入镜像仓库凭据</button><div data-reg-form></div>' : '<p class="x-frac">当前账号没有 config:write 权限，无法导入。</p>');
      var button = secretsBox.querySelector("[data-reg-import]");
      if (button) button.onclick = function () {
        var container = secretsBox.querySelector("[data-reg-form]");
        container.innerHTML = '<form class="x-form xs-platform-policy" data-reg-create><div class="xs-platform-grid"><label><span>命名空间</span><input class="x-input" name="namespace" required value="default" pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"></label><label><span>Secret 名称</span><input class="x-input" name="name" required pattern="[a-z0-9]([-a-z0-9.]*[a-z0-9])?"></label></div><label><span>Docker 配置文件 (.dockerconfigjson)</span><input class="x-input" type="file" name="file" accept=".json,application/json" required></label><button class="x-btn x-btn--pri" type="submit">导入</button></form>';
        var form = container.querySelector("form");
        form.onsubmit = async function (event) {
          event.preventDefault(); if (!form.reportValidity() || !active()) return;
          var file = form.elements.file.files[0]; if (!file || file.size > 900000) return notify("配置文件超过大小限制");
          var raw, parsed;
          try { raw = await file.text(); parsed = JSON.parse(raw); if (!parsed || typeof parsed.auths !== "object" || Array.isArray(parsed.auths) || !Object.keys(parsed.auths).length) throw new Error("缺少 auths"); }
          catch (_) { return notify("请选择有效的 Docker 配置文件"); }
          if (!active()) return;
          var submit = form.querySelector('[type="submit"]'); submit.disabled = true;
          api("/clusters/" + encodeURIComponent(id) + "/config/secrets", { method: "POST", scope: "cluster", body: { namespace: form.elements.namespace.value.trim(), name: form.elements.name.value.trim(), type: TYPE, data: { ".dockerconfigjson": raw } } }).then(function () { notify("镜像仓库 Secret 变更已提交"); render(el); }, function (err) { submit.disabled = false; notify("导入失败：" + message(err)); });
        };
      };
    }, function (err) { if (active()) secretsBox.innerHTML = '<p class="x-aerr" role="alert">拉取凭据不可用：' + esc(message(err)) + '</p>'; });
    api("/provision/registry-mirror").then(function (mirror) {
      if (el._xsRegistryToken !== token) return;
      var provider = mirror && mirror.provider || "dockerhub";
      mirrorBox.innerHTML = '<p class="x-frac">只影响之后由平台新建的 K3s 集群。现有集群镜像加速状态未知，本页不可更改现有节点配置。</p><form class="x-form" data-reg-mirror-form><div class="xs-platform-grid"><label><span>提供方</span><select class="x-select" name="provider">' + ["dockerhub", "aliyun", "daocloud", "custom"].map(function (p) { return '<option value="' + p + '"' + (provider === p ? " selected" : "") + '>' + ({ dockerhub: "Docker Hub 直连", aliyun: "阿里云", daocloud: "DaoCloud", custom: "自定义" }[p]) + '</option>'; }).join("") + '</select></label><label><span>加速地址</span><input class="x-input" name="endpoint" placeholder="https://registry.example.com" value="' + esc(mirror && mirror.endpoint || "") + '"></label></div><p class="x-frac" data-reg-hint>只进行 URL 格式校验，不验证实际连通性。</p><button class="x-btn x-btn--pri" type="submit">保存新建集群模板</button></form>';
      var form = mirrorBox.querySelector("form"); var endpoint = form.elements.endpoint; var providerInput = form.elements.provider;
      var lastProvider = providerInput.value;
      function sync() {
        endpoint.disabled = providerInput.value === "dockerhub";
        if (endpoint.disabled) endpoint.value = "";
        else if (providerInput.value === "daocloud" && (lastProvider !== "daocloud" || !endpoint.value)) endpoint.value = "https://m.daocloud.io";
        else if (providerInput.value === "aliyun" && lastProvider === "daocloud" && endpoint.value === "https://m.daocloud.io") endpoint.value = "";
        lastProvider = providerInput.value;
      }
      providerInput.onchange = sync; sync();
      form.onsubmit = function (event) {
        event.preventDefault(); var value = endpoint.disabled ? "" : endpoint.value.trim();
        if (!endpoint.disabled && !validEndpoint(value)) return notify("请输入不含认证信息、查询参数的 HTTP(S) 地址");
        var button = form.querySelector('[type="submit"]'); button.disabled = true;
        api("/provision/registry-mirror", { method: "PUT", body: { provider: providerInput.value, endpoint: value } }).then(function () { notify("新建集群模板已保存"); render(el); }, function (err) { button.disabled = false; notify("保存失败：" + message(err)); });
      };
    }, function (err) { if (el._xsRegistryToken === token) mirrorBox.innerHTML = '<p class="x-aerr" role="alert">镜像加速模板不可用：' + esc(message(err)) + '</p>'; });
  }
  root.XSClusterRegistries = { render: render, validEndpoint: validEndpoint };
})(window);
