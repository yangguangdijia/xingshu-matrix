(function (root) {
  "use strict";
  var TYPES = { feishu: "飞书", dingtalk: "钉钉", wecom: "企业微信", email: "邮件" };
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function field(label, name, value, required, kind) { return '<label><span>' + label + '</span><input class="x-input" name="' + name + '" type="' + (kind || "text") + '" value="' + esc(value || "") + '"' + (required ? " required" : "") + ' autocomplete="off"></label>'; }
  function fields(type, cfg, editing) {
    if (type === "email") return field("SMTP 主机", "host", cfg.host, true) + field("SMTP 端口", "port", cfg.port || 25, true, "number") + field("用户名", "username", cfg.username) + field("密码", "password", "", false, "password") + field("发件人", "from", cfg.from, true, "email") + field("收件人（逗号分隔）", "to", (cfg.to || []).join(", "), true);
    return field("Webhook 地址", "webhook", editing ? "" : cfg.webhook, !editing, "url");
  }
  function build(type, form, existing) {
    var cfg = {}, data = new FormData(form);
    if (type === "email") {
      cfg = { host: String(data.get("host") || "").trim(), port: Number(data.get("port")), username: String(data.get("username") || "").trim(), from: String(data.get("from") || "").trim(), to: String(data.get("to") || "").split(/[,;\n]/).map(function (s) { return s.trim(); }).filter(Boolean) };
      if (data.get("password")) cfg.password = String(data.get("password"));
      if (!Number.isInteger(cfg.port) || cfg.port < 1 || cfg.port > 65535 || !cfg.to.length) throw new Error("请填写有效的 SMTP 端口和收件人");
    } else {
      cfg = { webhook: String(data.get("webhook") || "").trim() };
      if (!cfg.webhook && existing) delete cfg.webhook;
      if (!existing && !/^https:\/\//i.test(cfg.webhook)) throw new Error("Webhook 必须是 HTTPS 地址");
    }
    return { name: String(data.get("name") || "").trim(), type: type, config: cfg, enabled: existing ? !!existing.enabled : true, severities: existing && existing.severities || [], envs: existing && existing.envs || [] };
  }
  function open(existing, done) {
    existing = existing || null;
    var type = existing && existing.type && TYPES[existing.type] ? existing.type : "";
    var body = '<form class="xs-channel-form" data-channel-form><div data-channel-step></div><div class="xs-channel-actions"><button class="x-btn" type="button" data-channel-back hidden>返回类型</button><button class="x-btn x-btn--pri" type="submit" data-channel-save hidden>' + (existing ? "保存" : "创建") + '</button></div></form>';
    var wrap, fallback = false;
    if (typeof root.dlg === "function" && typeof root.closeAll === "function") {
      root.dlg(existing ? "编辑通知渠道" : "新建通知渠道", body, "");
      wrap = document.querySelector("#modal");
    }
    if (!wrap) {
      fallback = true;
      wrap = document.createElement("dialog"); wrap.className = "xs-channel-dialog";
      wrap.innerHTML = '<header><strong>' + (existing ? "编辑通知渠道" : "新建通知渠道") + '</strong><button class="x-btn" type="button" data-channel-close>关闭</button></header>' + body;
      document.body.appendChild(wrap);
      if (wrap.showModal) wrap.showModal(); else wrap.setAttribute("open", "");
      wrap.querySelector("[data-channel-close]").onclick = function () { wrap.remove(); };
    }
    var form = wrap.querySelector("[data-channel-form]");
    if (!form) return null;
    var step = form.querySelector("[data-channel-step]"); var back = form.querySelector("[data-channel-back]"); var save = form.querySelector("[data-channel-save]");
    function paint() {
      back.hidden = !type || !!existing; save.hidden = !type;
      if (!type) {
        step.innerHTML = '<label><span>选择渠道类型</span><select class="x-select" data-channel-type><option value="">请选择</option>' + Object.keys(TYPES).map(function (t) { return '<option value="' + t + '">' + TYPES[t] + '</option>'; }).join("") + '</select></label>';
        step.querySelector("select").onchange = function () { type = this.value; paint(); };
      } else {
        step.innerHTML = '<h4>' + esc(TYPES[type]) + '</h4>' + field("渠道名称", "name", existing && existing.name, true) + fields(type, existing && existing.config || {}, !!existing) + (existing ? '<p class="x-frac">留空的密钥与 Webhook 保留原值，现有凭据不会回显。</p>' : "");
      }
    }
    back.onclick = function () { type = ""; paint(); }; paint();
    form.onsubmit = function (event) {
      event.preventDefault(); if (!type || !form.reportValidity()) return;
      var payload;
      try { payload = build(type, form, existing); } catch (err) { if (root.XS && root.XS.toast) root.XS.toast(err.message); return; }
      if (!payload.name) return;
      save.disabled = true;
      root.XS.api("/observability/channels" + (existing ? "/" + encodeURIComponent(existing.id) : ""), { method: existing ? "PUT" : "POST", body: payload }).then(function (result) {
        if (fallback) wrap.remove(); else root.closeAll();
        if (root.XS && root.XS.toast) root.XS.toast(existing ? "渠道已更新" : "渠道已创建");
        if (typeof done === "function") done(result);
      }, function (err) { save.disabled = false; if (root.XS && root.XS.toast) root.XS.toast("保存失败：" + (err && err.message || "请求失败")); });
    };
    return wrap;
  }
  root.XSChannelForm = { open: open, build: build };
})(window);
