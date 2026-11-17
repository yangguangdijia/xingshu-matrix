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
    { k: "sso", t: "企业登录（SSO）" },
    { k: "ldap", t: "LDAP / AD" },
    { k: "appearance", t: "外观" },
    { k: "smtp", t: "系统邮件" },
    { k: "retention", t: "审计与保留" },
    { k: "env", t: "环境与审批" },
    { k: "integrations", t: "通知与集成" },
    { k: "backup", t: "平台备份" },
    { k: "jobs", t: "后台任务" },
    { k: "openapi", t: "开放接口" },
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
    if (!document.getElementById("xs-cfg-appearance-css")) {
      var link = document.createElement("link");
      link.id = "xs-cfg-appearance-css"; link.rel = "stylesheet"; link.href = "xs-platform-settings.css";
      document.head.appendChild(link);
    }
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
      "@media(max-width:960px){.xs-cfg{grid-template-columns:1fr}.xs-cfg-nav{position:static;display:flex;overflow:auto}.xs-cfg-nav button{flex:0 0 auto;width:auto;white-space:nowrap}.xs-cfg-row{grid-template-columns:1fr;gap:8px}.xs-cfg-links{grid-template-columns:1fr}}"
    ].join("\n");
    document.head.appendChild(s);
  }
  function row(label, control) { return '<div class="xs-cfg-row"><span>' + esc(label) + '</span><div>' + control + '</div></div>'; }
  function num(name, value, min, max, unit) {
    return '<div class="xs-cfg-unit"><input class="x-input" type="number" name="' + name + '" min="' + min + '" max="' + max + '" value="' + esc(value) + '" required><em>' + esc(unit) + '</em></div>';
  }
  function foot(label) { return '<div class="xs-cfg-foot"><button class="x-btn x-btn--pri" type="submit">' + esc(label || "保存") + '</button><button class="x-btn" type="reset">还原</button></div>'; }

  function appearance(body, initial, platformName, active) {
    var defaultColor = "#1677ff";
    var presets = [["#1677ff", "星枢蓝", "经典 · 清晰"], ["#0f7b6c", "松石绿", "沉稳 · 自然"], ["#7c3aed", "鸢尾紫", "鲜明 · 灵动"],
      ["#d4380d", "朱砂橙", "温暖 · 醒目"], ["#1d39c4", "靛青蓝", "理性 · 专注"], ["#08979c", "湖水青", "轻盈 · 通透"]];
    var saved = { logo: initial.logo || "", primary_color: /^#[0-9a-f]{6}$/i.test(initial.primary_color || "") ? initial.primary_color.toLowerCase() : defaultColor,
      login_title: initial.login_title || "", copyright: initial.copyright || "" };
    var logo = saved.logo, color = saved.primary_color, uploadId = 0, uploading = false, saving = false, view = "console";
    var canManage = !!(root.XS && root.XS.can && root.XS.can("setting:manage"));
    function safeLogo(value) {
      if (!value) return "";
      var match = /^data:image\/(png|jpeg|svg\+xml);base64,([A-Za-z0-9+/=]+)$/.exec(value);
      if (!match) throw new Error("仅支持 PNG、JPG 或 SVG 图片");
      var raw;
      try { raw = root.atob(match[2]); } catch (e) { throw new Error("图片内容无法读取，请重新选择"); }
      if (raw.length > 256 * 1024) throw new Error("图片超过 256 KiB，请压缩后再上传");
      if (match[1] === "svg+xml" && /<script|javascript:|\son[a-z]+\s*=/i.test(raw)) throw new Error("SVG 不能包含脚本或事件处理器");
      return value;
    }
    function mark(value) {
      var safe = "";
      try { safe = safeLogo(value); } catch (e) {}
      return safe ? '<img src="' + esc(safe) + '" alt="品牌 Logo">' : '<svg viewBox="0 0 32 32" aria-hidden="true"><use href="#mk"></use></svg>';
    }
    body.innerHTML = '<form class="xs-appearance" data-f aria-label="品牌与主题设置">' +
      '<header class="xs-appearance-head"><div><span class="xs-appearance-eyebrow">平台个性化</span><h2>品牌与主题</h2><p>让每一次登录，都带有你们的品牌印记。</p></div><span class="xs-appearance-local">仅在此页预览</span></header>' +
      '<div class="xs-appearance-layout"><div class="xs-appearance-editor">' +
      '<section class="xs-appearance-section"><div class="xs-appearance-sectionhead"><span>01</span><div><h3>品牌标识</h3><p>用于控制台导航与登录页面</p></div></div>' +
      '<div class="xs-appearance-upload"><div class="xs-appearance-logo" data-prev>' + mark(logo) + '</div><div><div class="xs-appearance-upload-actions"><button class="x-btn" type="button" data-upload>上传 Logo</button><button class="xs-appearance-textbtn" type="button" data-clear>恢复默认 Logo</button></div>' +
      '<input type="file" accept="image/png,image/jpeg,image/svg+xml" data-file hidden aria-label="选择品牌 Logo"><p id="xs-appearance-upload-hint">PNG / JPG / SVG · 最大 256 KiB<br>建议使用正方形、透明背景图片</p></div></div></section>' +
      '<section class="xs-appearance-section"><div class="xs-appearance-sectionhead"><span>02</span><div><h3>主题色</h3><p>一个主色，串联按钮、导航与选中状态</p></div></div>' +
      '<div class="xs-appearance-presets" role="group" aria-label="预设主题色">' + presets.map(function (preset) {
        return '<button type="button" class="xs-appearance-preset" data-preset="' + preset[0] + '" aria-pressed="false" aria-label="' + preset[1] + ' ' + preset[0] + '" style="--swatch:' + preset[0] + '">' +
          '<span class="xs-appearance-swatch" aria-hidden="true"><span class="xs-appearance-check">✓</span></span><span class="xs-appearance-preset-name">' + preset[1] + '<small>' + preset[2] + '</small></span></button>';
      }).join("") + '</div><div class="xs-appearance-custom"><label for="xs-appearance-hex">自定义主题色</label><div class="xs-appearance-colorinput"><input type="color" name="primary_color" value="' + color + '" aria-label="选择自定义主题色">' +
      '<input class="x-input" id="xs-appearance-hex" name="color_hex" value="' + color + '" maxlength="7" pattern="#[0-9a-fA-F]{6}" required spellcheck="false" autocomplete="off" aria-describedby="xs-appearance-color-hint"></div></div>' +
      '<p class="xs-appearance-hint" id="xs-appearance-color-hint">输入六位 HEX 色值，例如 #1677ff</p></section>' +
      '<section class="xs-appearance-section"><div class="xs-appearance-sectionhead"><span>03</span><div><h3>登录文案</h3><p>向团队传递统一的平台形象</p></div></div>' +
      '<label class="xs-appearance-field">登录页标题<span class="xs-appearance-field-meta">最多 40 字</span><input class="x-input" name="login_title" maxlength="40" value="' + esc(saved.login_title) + '" placeholder="研发与运维，一个平台"></label>' +
      '<label class="xs-appearance-field">版权信息<span class="xs-appearance-field-meta">最多 120 字</span><input class="x-input" name="copyright" maxlength="120" value="' + esc(saved.copyright) + '" placeholder="留空使用默认版权信息"></label>' +
      '<p class="xs-appearance-hint">留空使用默认文案。平台名称请在「基本信息」中修改。</p></section></div>' +
      '<aside class="xs-appearance-preview" aria-label="外观即时预览"><div class="xs-appearance-previewhead"><div><span class="xs-appearance-eyebrow">即时预览</span><h3>先看效果，再应用</h3></div><div class="xs-appearance-views" role="group" aria-label="预览场景">' +
      '<button type="button" data-view="console" aria-pressed="true">控制台</button><button type="button" data-view="login" aria-pressed="false">登录页</button></div></div>' +
      '<div class="xs-appearance-canvas" data-preview></div><div class="xs-appearance-previewnote"><span aria-hidden="true">◇</span><p>预览不会影响当前平台。<br>保存成功后，新外观将应用到整个团队。</p></div></aside></div>' +
      '<footer class="xs-appearance-footer"><div><p class="xs-appearance-status" data-status role="status" aria-live="polite"></p><p class="xs-appearance-error" data-error role="alert" hidden></p></div>' +
      '<div class="xs-appearance-actions"><button type="reset" class="x-btn" data-restore>还原更改</button><button type="submit" class="x-btn x-btn--pri" data-save>保存并应用</button></div></footer></form>';
    var form = body.querySelector("[data-f]"), preview = form.querySelector("[data-preview]"), error = form.querySelector("[data-error]");
    function alive() { return active() && body.isConnected !== false; }
    function values() { return { logo: logo, primary_color: form.elements.color_hex.value.trim().toLowerCase(), login_title: form.elements.login_title.value.trim(), copyright: form.elements.copyright.value.trim() }; }
    function dirty() { return JSON.stringify(values()) !== JSON.stringify(saved); }
    function showError(message) { error.textContent = message; error.hidden = !message; }
    function status() {
      var changed = dirty(), valid = /^#[0-9a-f]{6}$/i.test(form.elements.color_hex.value.trim());
      form.elements.color_hex.setAttribute("aria-invalid", valid ? "false" : "true");
      form.querySelector("[data-status]").textContent = !canManage ? "当前为只读模式，需要设置管理权限才能保存" : saving ? "正在保存，请稍候…" : uploading ? "正在读取 Logo…" : changed ? "有未保存的更改 · 仅在本页预览" : "已与保存的设置一致";
      form.querySelector("[data-status]").setAttribute("data-dirty", changed ? "true" : "false");
      [].forEach.call(form.querySelectorAll("input, button"), function (control) { control.disabled = saving || (!canManage && !control.hasAttribute("data-view")); });
      form.querySelector("[data-save]").disabled = !canManage || saving || uploading || !changed || !valid;
      form.querySelector("[data-restore]").disabled = !canManage || saving || (!changed && !uploading);
      form.setAttribute("aria-busy", saving || uploading ? "true" : "false");
    }
    function draw() {
      var data = values(), numeric = parseInt(color.slice(1), 16), channels = [numeric >> 16, (numeric >> 8) & 255, numeric & 255];
      var brightness = channels.map(function (channel) { channel /= 255; return channel <= 0.04045 ? channel / 12.92 : Math.pow((channel + 0.055) / 1.055, 2.4); });
      var ink = brightness[0] * 0.2126 + brightness[1] * 0.7152 + brightness[2] * 0.0722 > 0.179 ? "#101828" : "#ffffff";
      preview.setAttribute("style", "--preview-accent:" + color + ";--preview-rgb:" + channels.join(",") + ";--preview-on:" + ink);
      var brand = '<span class="xs-appearance-mini-logo">' + mark(logo) + '</span><strong>' + esc(platformName || "星枢 Matrix") + '</strong>';
      if (view === "console") {
        preview.innerHTML = '<div class="xs-appearance-console"><div class="xs-appearance-mini-top">' + brand + '<span class="xs-appearance-avatar" aria-hidden="true">M</span></div>' +
          '<div class="xs-appearance-mini-body"><div class="xs-appearance-mini-nav"><span>工作空间</span><b>应用服务</b><span>集群资源</span><span>系统设置</span></div>' +
          '<div class="xs-appearance-mini-content"><span class="xs-appearance-mini-breadcrumb">工作空间 / 应用服务</span><div class="xs-appearance-mini-title"><strong>应用服务</strong><span class="xs-appearance-mini-primary">创建应用</span></div>' +
          '<div class="xs-appearance-mini-list"><div><strong>团队应用</strong><span>运行状态</span></div><div><span><i aria-hidden="true"></i>业务服务</span><span class="xs-appearance-mini-state">运行中</span></div><div><span><i aria-hidden="true"></i>网关服务</span><span class="xs-appearance-mini-state">运行中</span></div></div>' +
          '<p class="xs-appearance-mini-caption">专注协作，有序交付。</p></div></div></div>';
      } else {
        preview.innerHTML = '<div class="xs-appearance-login"><div class="xs-appearance-login-brand">' + brand + '</div><h4>' + esc(data.login_title || "研发与运维，一个平台") + '</h4>' +
          '<div class="xs-appearance-login-form"><strong>欢迎登录</strong><span class="xs-appearance-mini-input">用户名</span><span class="xs-appearance-mini-input">密码</span><span class="xs-appearance-mini-primary">登 录</span></div>' +
          '<p class="xs-appearance-mini-copyright">' + esc(data.copyright || "© 星枢 Matrix 私有化部署版 v1.0") + '</p></div>';
      }
      form.querySelector("[data-prev]").innerHTML = mark(logo);
      [].forEach.call(form.querySelectorAll("[data-preset]"), function (button) { button.setAttribute("aria-pressed", button.getAttribute("data-preset") === data.primary_color ? "true" : "false"); });
      [].forEach.call(form.querySelectorAll("[data-view]"), function (button) { button.setAttribute("aria-pressed", button.getAttribute("data-view") === view ? "true" : "false"); });
      status();
    }
    function setColor(value) { color = value.toLowerCase(); form.elements.primary_color.value = color; form.elements.color_hex.value = color; showError(""); draw(); }
    form.elements.primary_color.oninput = function () { if (canManage && !saving) setColor(this.value); };
    form.elements.color_hex.oninput = function () {
      if (!canManage || saving) return;
      var value = this.value.trim();
      if (/^#[0-9a-f]{6}$/i.test(value)) { color = value.toLowerCase(); form.elements.primary_color.value = color; showError(""); }
      draw();
    };
    form.elements.login_title.oninput = form.elements.copyright.oninput = function () { if (!saving) draw(); };
    [].forEach.call(form.querySelectorAll("[data-preset]"), function (button) { button.onclick = function () { if (canManage && !saving) setColor(button.getAttribute("data-preset")); }; });
    [].forEach.call(form.querySelectorAll("[data-view]"), function (button) { button.onclick = function () { view = button.getAttribute("data-view"); draw(); }; });
    form.querySelector("[data-upload]").onclick = function () { if (canManage && !saving) form.querySelector("[data-file]").click(); };
    form.querySelector("[data-file]").onchange = function () {
      if (!canManage || saving) return;
      var file = this.files && this.files[0];
      if (!file) return;
      var request = ++uploadId;
      this.value = ""; uploading = false; showError("");
      if (!/^image\/(png|jpeg|svg\+xml)$/.test(file.type)) { showError("仅支持 PNG、JPG 或 SVG 图片"); status(); return; }
      if (file.size > 256 * 1024) { showError("图片超过 256 KiB，请压缩后再上传"); status(); return; }
      uploading = true; status();
      var reader = new root.FileReader();
      reader.onload = function () {
        if (!alive() || request !== uploadId) return;
        uploading = false;
        try { logo = safeLogo(String(reader.result || "")); showError(""); } catch (e) { showError(errMsg(e)); }
        draw();
      };
      reader.onerror = reader.onabort = function () { if (alive() && request === uploadId) { uploading = false; showError("读取图片失败，请重新选择"); status(); } };
      try { reader.readAsDataURL(file); } catch (e) { uploading = false; showError("读取图片失败，请重新选择"); status(); }
    };
    form.querySelector("[data-clear]").onclick = function () {
      if (!canManage || saving) return;
      uploadId++; uploading = false; logo = ""; showError(""); draw();
    };
    form.onreset = function (event) {
      event.preventDefault();
      if (!canManage || saving) return;
      uploadId++; uploading = false; logo = saved.logo; color = saved.primary_color;
      form.elements.primary_color.value = color; form.elements.color_hex.value = color;
      form.elements.login_title.value = saved.login_title; form.elements.copyright.value = saved.copyright;
      form.querySelector("[data-file]").value = ""; showError(""); draw();
    };
    form.onsubmit = function (event) {
      event.preventDefault();
      if (!canManage || saving || uploading || !dirty()) return;
      var data = values();
      if (!/^#[0-9a-f]{6}$/i.test(data.primary_color)) { showError("请输入完整的六位 HEX 色值，例如 #1677ff"); form.elements.color_hex.focus(); return; }
      if (Array.from(data.login_title).length > 40 || Array.from(data.copyright).length > 120) { showError("登录页标题最多 40 字，版权信息最多 120 字"); return; }
      try { safeLogo(data.logo); } catch (e) { showError(errMsg(e)); return; }
      saving = true; showError(""); status();
      return Promise.resolve().then(function () {
        return api("/settings/appearance", { method: "PUT", body: { logo: data.logo, primary_color: data.primary_color === defaultColor ? "" : data.primary_color, login_title: data.login_title, copyright: data.copyright } });
      }).then(function () {
        if (!alive()) return;
        saved = data; toast("外观已保存，正在应用…");
        location.reload();
      }, function (e) {
        if (!alive()) return;
        saving = false; showError("保存失败：" + errMsg(e)); status();
      });
    };
    draw();
    return function () { return !alive() || (!dirty() && !uploading && !saving) || root.confirm(saving ? "外观正在保存，确定离开此页面？" : "有未保存的外观更改，确定放弃？"); };
  }

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
    var leaveAppearance = function () { return true; };
    el.querySelector("[data-cfg-refresh]").onclick = function () { if (leaveAppearance()) render(el); };
    [].forEach.call(el.querySelectorAll("[data-cfg-tab]"), function (b) {
      b.onclick = function () { if (leaveAppearance()) { el._xsCfgTab = b.getAttribute("data-cfg-tab"); render(el); } };
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
    } else if (tab === "sso") {
      api("/settings/sso").then(function (r) {
        if (!active()) return;
        var c = (r && r.config) || {}, has = r && r.has_client_secret;
        var chk = function (n, v, t) { return '<label style="display:flex;align-items:center;gap:8px"><input type="checkbox" name="' + n + '"' + (v ? " checked" : "") + ' style="width:auto"> ' + esc(t) + "</label>"; };
        var inp = function (n, v, ph, type) { return '<input class="x-input" name="' + n + '" type="' + (type || "text") + '" value="' + esc(v || "") + '" placeholder="' + esc(ph || "") + '" autocomplete="off" style="width:420px">'; };
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>企业登录（OpenID Connect）</h3>' +
          '<p class="xs-cfg-tip" style="margin:0 0 16px;color:#6b7785;font-size:13px">对接 Keycloak、Authing、Okta、Azure AD、飞书 / 钉钉开放平台等支持 OIDC 的身份源。用户在登录页点「使用企业 SSO 登录」，在身份源认证后回到平台。</p>' +
          row("启用", chk("enabled", c.enabled, "在登录页显示企业 SSO 入口")) +
          row("按钮名称", inp("display_name", c.display_name, "企业 SSO")) +
          row("Issuer 地址", inp("issuer", c.issuer, "https://sso.example.com/realms/ops")) +
          row("Client ID", inp("client_id", c.client_id, "xingshu-matrix")) +
          row("Client Secret", inp("client_secret", "", has ? "已保存，留空不修改" : "身份源分配的密钥", "password")) +
          row("平台外部地址", inp("redirect_base", c.redirect_base || location.origin, "https://matrix.example.com")) +
          row("回调地址", '<code data-cb>' + esc(r.callback_url || ((c.redirect_base || location.origin) + "/api/v1/auth/sso/callback")) + '</code><div style="color:#8a94a3;font-size:12px;margin-top:4px">在身份源的应用里登记这个地址</div>') +
          '<h3 style="margin-top:28px">用户映射</h3>' +
          row("Scopes", inp("scopes", c.scopes, "openid profile email")) +
          row("用户名字段", inp("username_claim", c.username_claim, "preferred_username")) +
          row("显示名字段", inp("name_claim", c.name_claim, "name")) +
          row("邮箱字段", inp("email_claim", c.email_claim, "email")) +
          row("首次登录", chk("auto_create", c.auto_create, "身份源里存在、平台里没有的用户自动创建")) +
          row("默认角色", '<select class="x-input" name="default_role" style="width:220px">' + [["viewer", "只读用户"], ["operator", "运维工程师"], ["approver", "审批人"]].map(function (o) {
            return '<option value="' + o[0] + '"' + ((c.default_role || "viewer") === o[0] ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>") +
          '<div class="xs-cfg-foot"><button class="x-btn x-btn--pri" type="submit">保存</button><button class="x-btn" type="button" data-test>测试连接</button><span data-tres style="font-size:13px;margin-left:8px"></span></div></form>';
        var f = body.querySelector("[data-f]");
        var collect = function () {
          return { enabled: f.elements.enabled.checked, display_name: V(f, "display_name"), issuer: V(f, "issuer"), client_id: V(f, "client_id"),
            client_secret: f.elements.client_secret.value, redirect_base: V(f, "redirect_base"), scopes: V(f, "scopes"), username_claim: V(f, "username_claim"),
            name_claim: V(f, "name_claim"), email_claim: V(f, "email_claim"), auto_create: f.elements.auto_create.checked, default_role: f.elements.default_role.value };
        };
        f.elements.redirect_base.oninput = function () { f.querySelector("[data-cb]").textContent = V(f, "redirect_base").replace(/\/+$/, "") + "/api/v1/auth/sso/callback"; };
        f.querySelector("[data-test]").onclick = function () {
          var out = f.querySelector("[data-tres]"), v = collect();
          if (!v.issuer) { out.textContent = "先填写 Issuer 地址"; out.style.color = "#d43030"; return; }
          out.textContent = "正在读取发现文档…"; out.style.color = "#6b7785";
          api("/settings/sso/test", { method: "POST", body: v }).then(function (t) {
            if (t && t.ok) { out.textContent = "连接成功：已读取授权、令牌和用户信息端点"; out.style.color = "#1a9b55"; }
            else { out.textContent = "连接失败：" + ((t && t.error) || "未知错误"); out.style.color = "#d43030"; }
          }, function (e) { out.textContent = "连接失败：" + errMsg(e); out.style.color = "#d43030"; });
        };
        bindSave(f, function () { return api("/settings/sso", { method: "PUT", body: collect() }); });
      }, fail);
    } else if (tab === "appearance") {
      Promise.all([api("/settings/appearance"), getSetting("platform.name").catch(function () { return ""; })]).then(function (results) {
        if (!active()) return;
        leaveAppearance = appearance(body, results[0] || {}, typeof results[1] === "string" ? results[1] : "", active);
      }, fail);
    } else if (tab === "smtp") {
      api("/settings/smtp").then(function (r) {
        if (!active()) return;
        var c = (r && r.config) || {}, has = r && r.has_password;
        var inp = function (n, v, ph, type, w) { return '<input class="x-input" name="' + n + '" type="' + (type || "text") + '" value="' + esc(v == null ? "" : v) + '" placeholder="' + esc(ph || "") + '" autocomplete="off" style="width:' + (w || 420) + 'px">'; };
        var chk = function (n, v, t) { return '<label style="display:flex;align-items:center;gap:8px"><input type="checkbox" name="' + n + '"' + (v ? " checked" : "") + ' style="width:auto"> ' + esc(t) + "</label>"; };
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>系统邮件（SMTP）</h3>' +
          '<p style="margin:0 0 16px;color:#6b7785;font-size:13px">用于「忘记密码」等系统邮件。告警邮件在「可观测 → 通知渠道」里单独配置。</p>' +
          row("启用", chk("enabled", c.enabled, "启用系统邮件，登录页开放「忘记密码」")) +
          row("SMTP 服务器", inp("host", c.host, "smtp.example.com")) +
          row("端口", inp("port", c.port || 465, "465", "number", 120) + '<div style="color:#8a94a3;font-size:12px;margin-top:4px">465 为 SSL；587 / 25 在服务器支持时使用 STARTTLS</div>') +
          row("用户名", inp("username", c.username, "noreply@example.com")) +
          row("密码 / 授权码", inp("password", "", has ? "已保存，留空不修改" : "邮箱的 SMTP 授权码", "password")) +
          row("发件人", inp("from", c.from, "星枢 Matrix <noreply@example.com>")) +
          row("平台访问地址", inp("site_url", c.site_url || location.origin, "https://matrix.example.com") + '<div style="color:#8a94a3;font-size:12px;margin-top:4px">邮件里的重置链接指向这个地址</div>') +
          row("不加密连接", chk("insecure", c.insecure, "允许在不支持 TLS 的内网中继上发信（不推荐）")) +
          '<div class="xs-cfg-foot"><button class="x-btn x-btn--pri" type="submit">保存</button><input class="x-input" data-to placeholder="测试收件邮箱" style="width:220px"><button class="x-btn" type="button" data-test>发送测试邮件</button><span data-tres style="font-size:13px;margin-left:8px"></span></div></form>';
        var f = body.querySelector("[data-f]");
        var collect = function () {
          return { enabled: f.elements.enabled.checked, host: V(f, "host"), port: N(f, "port"), username: V(f, "username"), password: f.elements.password.value,
            from: V(f, "from"), site_url: V(f, "site_url"), insecure: f.elements.insecure.checked };
        };
        f.querySelector("[data-test]").onclick = function () {
          var out = f.querySelector("[data-tres]"), to = f.querySelector("[data-to]").value.trim();
          if (!to) { out.textContent = "先填写测试收件邮箱"; out.style.color = "#d43030"; return; }
          out.textContent = "正在发送…"; out.style.color = "#6b7785";
          api("/settings/smtp/test", { method: "POST", body: Object.assign(collect(), { to: to }) }).then(function (t) {
            if (t && t.ok) { out.textContent = "已发送，请到收件箱查收"; out.style.color = "#1a9b55"; }
            else { out.textContent = "发送失败：" + ((t && t.error) || "未知错误"); out.style.color = "#d43030"; }
          }, function (e) { out.textContent = "发送失败：" + errMsg(e); out.style.color = "#d43030"; });
        };
        bindSave(f, function () { return api("/settings/smtp", { method: "PUT", body: collect() }); });
      }, fail);
    } else if (tab === "ldap") {
      Promise.all([api("/settings/ldap"), api("/roles").catch(function () { return []; })]).then(function (rs) {
        if (!active()) return;
        var r = rs[0] || {}, c = r.config || {}, has = r.has_bind_password;
        var roles = (Array.isArray(rs[1]) ? rs[1] : (rs[1] && (rs[1].items || rs[1].roles)) || []).filter(function (x) { return x.code !== "admin"; });
        var inp = function (n, v, ph, type, w) { return '<input class="x-input" name="' + n + '" type="' + (type || "text") + '" value="' + esc(v == null ? "" : v) + '" placeholder="' + esc(ph || "") + '" autocomplete="off" style="width:' + (w || 420) + 'px">'; };
        var chk = function (n, v, t) { return '<label style="display:flex;align-items:center;gap:8px"><input type="checkbox" name="' + n + '"' + (v ? " checked" : "") + ' style="width:auto"> ' + esc(t) + "</label>"; };
        var gr = c.group_roles || {};
        var roleSel = '<select class="x-select" name="default_role" style="width:200px">' + (roles.length ? roles : [{ code: "viewer", name: "viewer" }]).map(function (x) {
          return '<option value="' + esc(x.code) + '"' + (x.code === (c.default_role || "viewer") ? " selected" : "") + ">" + esc(x.name || x.code) + "</option>"; }).join("") + "</select>";
        body.innerHTML = '<form class="xs-cfg-card" data-f><h3>LDAP / Active Directory</h3>' +
          '<p style="margin:0 0 16px;color:#6b7785;font-size:13px">启用后，登录页可直接使用目录账号登录。本地已有的同名账号仍用本地密码，不会被目录接管。</p>' +
          row("启用", chk("enabled", c.enabled, "允许使用 LDAP / AD 账号登录")) +
          row("服务器地址", inp("url", c.url, "ldap://ldap.example.com:389 或 ldaps://…:636")) +
          row("加密", chk("start_tls", c.start_tls, "使用 StartTLS（ldap:// 时）") + chk("insecure_skip_verify", c.insecure_skip_verify, "跳过证书校验（仅测试环境）")) +
          row("绑定 DN", inp("bind_dn", c.bind_dn, "cn=readonly,dc=example,dc=com")) +
          row("绑定密码", inp("bind_password", "", has ? "已保存，留空不修改" : "服务账号密码", "password")) +
          row("用户搜索 Base DN", inp("base_dn", c.base_dn, "ou=people,dc=example,dc=com")) +
          row("用户过滤器", inp("user_filter", c.user_filter || "(uid={username})", "(uid={username})") + '<div style="color:#8a94a3;font-size:12px;margin-top:4px">AD 常用 (sAMAccountName={username})</div>') +
          row("属性映射", inp("attr_username", c.attr_username || "uid", "用户名属性", "text", 136) + " " + inp("attr_display_name", c.attr_display_name || "cn", "显示名属性", "text", 136) + " " + inp("attr_email", c.attr_email || "mail", "邮箱属性", "text", 136)) +
          row("组属性", inp("attr_groups", c.attr_groups || "", "memberOf（留空不同步角色）", "text", 200)) +
          row("组 → 角色", '<textarea class="x-input" name="group_roles" rows="3" style="width:420px" placeholder="每行：组 DN 或组名=角色编码，例如 ops=operator">' + esc(Object.keys(gr).map(function (k) { return k + "=" + gr[k]; }).join("\n")) + "</textarea>") +
          row("自动创建", chk("auto_create", c.enabled ? c.auto_create : true, "首次登录时自动创建平台账号")) +
          row("默认角色", roleSel + '<span style="color:#8a94a3;font-size:12px;margin-left:8px">组映射未命中时使用</span>') +
          '<div class="xs-cfg-foot"><button class="x-btn x-btn--pri" type="submit">保存</button><input class="x-input" data-tu placeholder="测试用户名（可选）" style="width:160px"><input class="x-input" data-tp type="password" placeholder="密码（可选）" style="width:140px" autocomplete="new-password"><button class="x-btn" type="button" data-test>测试</button><span data-tres style="font-size:13px;margin-left:8px"></span></div></form>';
        var f = body.querySelector("[data-f]");
        var collect = function () {
          var map = {};
          String(f.elements.group_roles.value || "").split(/\n/).forEach(function (l) { var i = l.indexOf("="); if (i > 0) map[l.slice(0, i).trim()] = l.slice(i + 1).trim(); });
          return { enabled: f.elements.enabled.checked, url: V(f, "url"), start_tls: f.elements.start_tls.checked, insecure_skip_verify: f.elements.insecure_skip_verify.checked,
            bind_dn: V(f, "bind_dn"), bind_password: f.elements.bind_password.value, base_dn: V(f, "base_dn"), user_filter: V(f, "user_filter"),
            attr_username: V(f, "attr_username"), attr_display_name: V(f, "attr_display_name"), attr_email: V(f, "attr_email"), attr_groups: V(f, "attr_groups"),
            group_roles: map, auto_create: f.elements.auto_create.checked, default_role: f.elements.default_role.value };
        };
        f.querySelector("[data-test]").onclick = function () {
          var out = f.querySelector("[data-tres]");
          out.textContent = "正在测试…"; out.style.color = "#6b7785";
          var b = Object.assign(collect(), { username: f.querySelector("[data-tu]").value.trim(), password: f.querySelector("[data-tp]").value });
          api("/settings/ldap/test", { method: "POST", body: b }).then(function (t) {
            if (t && t.ok) {
              var id = t.identity;
              out.textContent = t.message + (id ? "：" + (id.display_name || id.username) + (id.email ? "（" + id.email + "）" : "") + (id.roles && id.roles.length ? "，角色 " + id.roles.join("、") : "") : "");
              out.style.color = "#1a9b55";
            } else { out.textContent = "失败：" + ((t && t.error) || "未知错误"); out.style.color = "#d43030"; }
          }, function (e) { out.textContent = "失败：" + errMsg(e); out.style.color = "#d43030"; });
        };
        bindSave(f, function () { return api("/settings/ldap", { method: "PUT", body: collect() }); });
      }, fail);
    } else if (tab === "backup") {
      if (root.XSPlatformBackup) root.XSPlatformBackup.render(body);
      else body.innerHTML = '<section class="xs-cfg-card"><p class="xs-cfg-err">平台备份组件未加载，请刷新页面。</p></section>';
    } else if (tab === "jobs") {
      if (root.XSJobs) root.XSJobs.render(body);
      else body.innerHTML = '<section class="xs-cfg-card"><p class="xs-cfg-err">后台任务组件未加载，请刷新页面。</p></section>';
    } else if (tab === "openapi") {
      body.innerHTML = '<section class="xs-cfg-card"><h3>开放接口</h3><p class="x-frac" style="margin:0 0 12px">第三方系统可用「个人设置 → 个人访问令牌」生成的令牌调用以下接口（请求头 Authorization: Bearer &lt;令牌&gt;），令牌权限不超过创建人本身。</p><div data-apidoc></div></section>';
      if (root.XSApiDoc) root.XSApiDoc.render(body.querySelector("[data-apidoc]"));
      else body.querySelector("[data-apidoc]").innerHTML = '<p class="xs-cfg-err">接口文档组件未加载，请刷新页面。</p>';
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
