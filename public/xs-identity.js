/* 星枢 Matrix — 身份安全：个人访问令牌 / 两步验证（TOTP）/ 登录 MFA 校验 / 管理员 MFA 管理
 * 依赖 window.XS（xs-api.js）。挂载到 window.XSIdentity。
 *
 * 后端接口（server/internal/auth/identity_http.go、server/internal/users/mfa_admin.go）：
 *   GET    /auth/tokens                    → APIToken[]（id,name,prefix,scopes,expires_at,last_used_at,last_used_ip,created_at,revoked_at,status）
 *   POST   /auth/tokens                    {name, scopes[], expires_in_days(0=永不)} → APIToken + token（明文，仅此一次）
 *   DELETE /auth/tokens/:id                → {id}
 *   GET    /auth/mfa                       → {enabled, enabled_at, recovery_codes_left, required}
 *   POST   /auth/mfa/setup                 → {secret, otpauth_uri}
 *   POST   /auth/mfa/activate              {code} → {enabled:true, recovery_codes[]}
 *   POST   /auth/mfa/disable               {code} → {enabled:false}
 *   POST   /auth/mfa/recovery-codes        {code} → {recovery_codes[]}
 *   POST   /auth/mfa/verify（免登录）       {mfa_ticket, code} → {token, user, recovery_codes?}
 *   GET    /users/mfa-flags?ids=1,2        → {"1":true,...}（单次最多 200 个）
 *   DELETE /users/:id/mfa                  → {id, mfa_enabled:false}
 */
(function (root) {
  "use strict";

  var EXPIRY = [[7, "7 天"], [30, "30 天"], [90, "90 天"], [365, "365 天"], [0, "永不过期"]];

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function XS() {
    if (!root.XS || typeof root.XS.api !== "function") throw new Error("XS API 客户端未加载");
    return root.XS;
  }
  function api(path, options) {
    try { return Promise.resolve(XS().api(path, options || {})); } catch (e) { return Promise.reject(e); }
  }
  function toast(m) { if (root.XS && typeof root.XS.toast === "function") root.XS.toast(m); }
  function errMsg(e) { return (e && e.message) || "操作失败，请稍后重试"; }
  function fmt(t) {
    if (!t) return "";
    var d = new Date(t);
    if (isNaN(d.getTime())) return String(t);
    function p(n) { return n < 10 ? "0" + n : "" + n; }
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
  }
  function list(d) { return Array.isArray(d) ? d : (d && Array.isArray(d.items) ? d.items : []); }

  // ---------------------------------------------------------------- 样式
  var CSS = [
    ".xid{font-size:14px;color:#1f2937}",
    ".xid-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:12px}",
    ".xid-head h3{margin:0 0 4px;font-size:16px;font-weight:600}.xid-sub{margin:0;color:#6b7280;font-size:13px}",
    ".xid-card{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:16px}",
    ".xid-table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #e5e7eb;border-radius:8px}",
    ".xid-table th,.xid-table td{padding:10px 12px;border-bottom:1px solid #f0f1f3;text-align:left;vertical-align:top}",
    ".xid-table th{background:#f8fafc;color:#4b5563;font-weight:500;font-size:13px}",
    ".xid-mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}",
    ".xid-muted{color:#9ca3af}.xid-err{color:#dc2626;font-size:13px;margin:6px 0 0}",
    ".xid-tag{display:inline-block;padding:0 6px;border-radius:4px;font-size:12px;line-height:20px;background:#f3f4f6;color:#4b5563;margin:0 4px 2px 0}",
    ".xid-tag--ok{background:#ecfdf5;color:#047857}.xid-tag--warn{background:#fff7ed;color:#c2410c}.xid-tag--off{background:#f3f4f6;color:#9ca3af}",
    ".xid-btn{height:32px;padding:0 14px;border:1px solid #d1d5db;border-radius:6px;background:#fff;color:#1f2937;cursor:pointer;font-size:13px}",
    ".xid-btn:hover{border-color:#2563eb;color:#2563eb}.xid-btn[disabled]{opacity:.5;cursor:not-allowed}",
    ".xid-btn--pri{background:#2563eb;border-color:#2563eb;color:#fff}.xid-btn--pri:hover{background:#1d4ed8;color:#fff}",
    ".xid-btn--danger{color:#dc2626;border-color:#fecaca}.xid-btn--danger:hover{background:#fef2f2;color:#dc2626;border-color:#dc2626}",
    ".xid-link{border:0;background:none;color:#2563eb;cursor:pointer;padding:0;font-size:13px}.xid-link--danger{color:#dc2626}.xid-link[disabled]{color:#9ca3af;cursor:not-allowed}",
    ".xid-input,.xid-select{width:100%;box-sizing:border-box;height:34px;padding:0 10px;border:1px solid #d1d5db;border-radius:6px;font-size:14px;background:#fff}",
    ".xid-input:focus,.xid-select:focus{outline:none;border-color:#2563eb;box-shadow:0 0 0 3px rgba(37,99,235,.12)}",
    ".xid-field{display:block;margin-bottom:14px}.xid-field>span{display:block;margin-bottom:6px;color:#374151;font-size:13px}",
    ".xid-scopes{max-height:180px;overflow:auto;border:1px solid #e5e7eb;border-radius:6px;padding:8px;display:grid;grid-template-columns:1fr 1fr;gap:4px 12px}",
    ".xid-mask{position:fixed;inset:0;background:rgba(15,23,42,.35);z-index:1000;display:flex;align-items:center;justify-content:center}",
    ".xid-dialog{background:#fff;border-radius:10px;width:min(480px,92vw);box-shadow:0 20px 40px rgba(15,23,42,.18);padding:20px 22px}",
    ".xid-dialog h4,.xid-drawer h4{margin:0 0 12px;font-size:16px}",
    ".xid-drawer{position:fixed;top:0;right:0;bottom:0;width:min(440px,96vw);background:#fff;box-shadow:-8px 0 24px rgba(15,23,42,.12);padding:20px 22px;display:flex;flex-direction:column;z-index:1001}",
    ".xid-drawer .xid-body{flex:1;overflow:auto}",
    ".xid-foot{display:flex;justify-content:flex-end;gap:8px;margin-top:16px}",
    ".xid-secret{display:flex;gap:8px;align-items:center;background:#f8fafc;border:1px solid #e5e7eb;border-radius:6px;padding:8px 10px;word-break:break-all}",
    ".xid-warn{background:#fff7ed;border:1px solid #fed7aa;color:#9a3412;border-radius:6px;padding:8px 10px;font-size:13px;margin:10px 0}",
    ".xid-codes{display:grid;grid-template-columns:1fr 1fr;gap:6px 16px;background:#f8fafc;border:1px solid #e5e7eb;border-radius:6px;padding:10px 14px}",
    ".xid-qr{width:180px;min-height:180px;display:flex;align-items:center;justify-content:center;border:1px solid #e5e7eb;border-radius:6px;background:#fff}",
    ".xid-setup{display:flex;gap:20px;flex-wrap:wrap}.xid-setup>div{flex:1;min-width:220px}",
    ".xid-otp{letter-spacing:6px;font-size:18px;text-align:center;max-width:200px}"
  ].join("\n");
  function injectCSS() {
    var doc = root.document;
    if (!doc || !doc.head || typeof doc.createElement !== "function") return;
    if (doc.getElementById && doc.getElementById("xid-style")) return;
    var s = doc.createElement("style");
    s.id = "xid-style";
    s.textContent = CSS;
    doc.head.appendChild(s);
  }

  // ---------------------------------------------------------------- 浮层（挂在宿主内部，随宿主销毁）
  function openLayer(host, html) {
    closeLayer(host);
    host.insertAdjacentHTML("beforeend", '<div class="xid-layer" data-xid-layer>' + html + "</div>");
    var layer = host.querySelector("[data-xid-layer]");
    var first = layer && layer.querySelector("input:not([readonly]), select, button");
    if (first && typeof first.focus === "function") first.focus();
    return layer;
  }
  function closeLayer(host) {
    var layer = host.querySelector("[data-xid-layer]");
    if (layer) layer.remove();
  }
  function dialog(title, body, foot) {
    return '<div class="xid-mask" data-xid-mask><div class="xid-dialog" role="dialog" aria-modal="true"><h4>' + esc(title) + "</h4>" +
      body + '<div class="xid-foot">' + foot + "</div></div></div>";
  }
  function copyText(text) {
    var nav = root.navigator;
    if (nav && nav.clipboard && typeof nav.clipboard.writeText === "function") {
      return Promise.resolve(nav.clipboard.writeText(text)).then(function () { toast("已复制到剪贴板"); }, function () { toast("复制失败，请手动选中复制"); });
    }
    toast("当前浏览器不支持自动复制，请手动选中复制");
    return Promise.resolve();
  }
  function onHost(host, key, handlers) {
    host["__xid_" + key] = handlers;
    if (host["__xid_bound_" + key]) return;
    host["__xid_bound_" + key] = true;
    ["click", "change", "keydown"].forEach(function (type) {
      host.addEventListener(type, function (e) {
        var h = host["__xid_" + key];
        if (!h || !e || !e.target || typeof e.target.closest !== "function") return;
        if (type === "keydown") {
          if (e.key !== "Enter") return;
          var f = e.target.closest("[data-xid-enter]");
          if (f && h[f.getAttribute("data-xid-enter")]) { if (e.preventDefault) e.preventDefault(); return h[f.getAttribute("data-xid-enter")](f, e); }
          return;
        }
        var t = e.target.closest("[data-xid-" + type + "]");
        if (t && host.contains(t)) {
          var act = t.getAttribute("data-xid-" + type);
          if (h[act]) return h[act](t, e);
        }
        if (type === "click" && e.target.hasAttribute && e.target.hasAttribute("data-xid-mask") && h.maskClose) return h.maskClose();
      });
    });
  }
  function val(scope, name) {
    var n = scope && scope.querySelector('[name="' + name + '"]');
    return n ? String(n.value == null ? "" : n.value) : "";
  }
  function setErr(scope, msg) {
    var n = scope && scope.querySelector("[data-xid-err]");
    if (!n) return;
    n.textContent = msg || "";
    n.hidden = !msg;
  }

  // ================================================================ 个人访问令牌
  function availablePerms() {
    var u = root.XS && root.XS.user;
    var p = (u && (u.permissions || u.perms)) || [];
    p = p.filter(function (x) { return typeof x === "string" && x && x !== "*"; });
    return p.filter(function (x, i) { return p.indexOf(x) === i; }).sort();
  }

  function tokenStatus(t) {
    if (t.status === "revoked" || t.revoked_at) return '<span class="xid-tag xid-tag--off">已吊销</span>';
    if (t.status === "expired") return '<span class="xid-tag xid-tag--warn">已过期</span>';
    return "";
  }

  function tokenRows(items) {
    if (!items.length) return '<tr><td colspan="5" class="xid-muted" style="text-align:center;padding:28px">暂无令牌，点击右上角「创建令牌」生成用于脚本 / CI 调用的访问令牌</td></tr>';
    return items.map(function (t) {
      var scopes = (t.scopes || []).length
        ? t.scopes.map(function (s) { return '<span class="xid-tag xid-mono">' + esc(s) + "</span>"; }).join("")
        : '<span class="xid-muted">全部权限（与账号一致）</span>';
      var active = !(t.status === "revoked" || t.status === "expired" || t.revoked_at);
      var used = t.last_used_at ? esc(fmt(t.last_used_at)) + (t.last_used_ip ? '<div class="xid-muted xid-mono">' + esc(t.last_used_ip) + "</div>" : "") : '<span class="xid-muted">从未使用</span>';
      return '<tr data-xid-token-row="' + esc(t.id) + '"><td><b>' + esc(t.name) + "</b> " + tokenStatus(t) +
        (t.prefix ? '<div class="xid-muted xid-mono">' + esc(t.prefix) + "…</div>" : "") + "</td>" +
        "<td>" + scopes + "</td>" +
        "<td>" + (t.expires_at ? esc(fmt(t.expires_at)) : "永不过期") + "</td>" +
        "<td>" + used + "</td>" +
        '<td><button type="button" class="xid-link xid-link--danger" data-xid-click="revoke" data-id="' + esc(t.id) + '"' + (active ? "" : " disabled") + ">吊销</button></td></tr>";
    }).join("");
  }

  function renderTokens(el) {
    if (!el) throw new Error("renderTokens 需要容器元素");
    injectCSS();
    var st = { items: [], loading: true, error: "", busy: false, seq: 0 };
    el.innerHTML = '<div class="xid" data-xid-tokens><div class="xid-head"><div><h3>个人访问令牌</h3>' +
      '<p class="xid-sub">用于脚本、CI 等以你的身份调用平台 API。令牌权限不会超过账号自身权限；请妥善保管，不用时及时吊销。</p></div>' +
      '<button type="button" class="xid-btn xid-btn--pri" data-xid-click="create">创建令牌</button></div><div data-xid-main></div></div>';

    function main() { return el.querySelector("[data-xid-main]"); }
    function draw() {
      var m = main();
      if (!m) return;
      if (st.loading) { m.innerHTML = '<div class="xid-card xid-muted">正在加载令牌…</div>'; return; }
      if (st.error) { m.innerHTML = '<div class="xid-card"><p class="xid-err">' + esc(st.error) + '</p><button type="button" class="xid-btn" data-xid-click="reload">重试</button></div>'; return; }
      m.innerHTML = '<table class="xid-table"><thead><tr><th>名称</th><th>权限范围</th><th>过期时间</th><th>最近使用</th><th style="width:72px">操作</th></tr></thead><tbody>' +
        tokenRows(st.items) + "</tbody></table>";
    }
    function load() {
      var my = ++st.seq;
      st.loading = true; st.error = ""; draw();
      return api("/auth/tokens").then(function (d) {
        if (my !== st.seq) return;
        st.items = list(d); st.loading = false; draw();
      }, function (e) {
        if (my !== st.seq) return;
        st.loading = false; st.error = "令牌加载失败：" + errMsg(e); draw();
      });
    }
    function find(id) { return st.items.filter(function (t) { return String(t.id) === String(id); })[0]; }

    function openCreate() {
      var perms = availablePerms();
      var scopeUI = perms.length
        ? '<div class="xid-scopes">' + perms.map(function (p) {
            return '<label><input type="checkbox" name="scope" value="' + esc(p) + '" /> <span class="xid-mono">' + esc(p) + "</span></label>";
          }).join("") + "</div>"
        : '<input class="xid-input xid-mono" name="scopes_text" placeholder="如 cluster:read, workload:read，多个用逗号分隔" />';
      openLayer(el, '<div class="xid-mask" data-xid-mask></div><aside class="xid-drawer" role="dialog" aria-modal="true" data-xid-drawer><h4>创建访问令牌</h4><div class="xid-body">' +
        '<label class="xid-field"><span>名称 <i style="color:#dc2626">*</i></span><input class="xid-input" name="name" maxlength="64" placeholder="如：CI 部署脚本" /></label>' +
        '<label class="xid-field"><span>有效期</span><select class="xid-select" name="expires">' +
        EXPIRY.map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === 90 ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") +
        "</select></label>" +
        '<div class="xid-field"><span>权限范围</span>' + scopeUI + '<p class="xid-sub" style="margin-top:6px">不选则继承账号全部权限；建议按最小权限勾选。</p></div>' +
        '<p class="xid-err" data-xid-err hidden></p></div>' +
        '<div class="xid-foot"><button type="button" class="xid-btn" data-xid-click="close">取消</button><button type="button" class="xid-btn xid-btn--pri" data-xid-click="submit">创建</button></div></aside>');
    }

    function submit(btn) {
      var layer = el.querySelector("[data-xid-layer]");
      if (!layer || st.busy) return;
      var name = val(layer, "name").trim();
      if (!name) return setErr(layer, "请填写令牌名称");
      if (name.length > 64) return setErr(layer, "名称最多 64 个字符");
      var days = parseInt(val(layer, "expires"), 10);
      if (!(days >= 0)) days = 90;
      var scopes = Array.prototype.filter.call(layer.querySelectorAll('[name="scope"]'), function (n) { return n.checked; })
        .map(function (n) { return n.value; });
      var text = val(layer, "scopes_text");
      if (text) scopes = scopes.concat(text.split(/[,，\s]+/).filter(Boolean));
      st.busy = true; setErr(layer, ""); if (btn) btn.disabled = true;
      return api("/auth/tokens", { method: "POST", body: { name: name, scopes: scopes, expires_in_days: days } }).then(function (t) {
        st.busy = false;
        showPlain(t || {});
        load();
      }, function (e) {
        st.busy = false; if (btn) btn.disabled = false;
        setErr(layer, "创建失败：" + errMsg(e));
      });
    }

    // 明文只在这一个弹窗里出现一次；关闭后从内存与 DOM 中清除。
    var plain = null;
    function showPlain(t) {
      plain = String(t.token || "");
      openLayer(el, dialog("令牌已创建",
        '<p style="margin:0 0 8px">令牌「' + esc(t.name) + '」已创建，请立即复制并妥善保存：</p>' +
        '<div class="xid-secret"><code class="xid-mono" data-xid-plain style="flex:1">' + esc(plain) + '</code>' +
        '<button type="button" class="xid-btn" data-xid-click="copyPlain">复制</button></div>' +
        '<div class="xid-warn">关闭后无法再次查看。如遗失，请吊销后重新创建。</div>',
        '<button type="button" class="xid-btn xid-btn--pri" data-xid-click="closePlain">我已保存，关闭</button>'));
    }
    function closePlain() { plain = null; closeLayer(el); }

    function askRevoke(btn) {
      var t = find(btn.getAttribute("data-id"));
      if (!t) return;
      openLayer(el, dialog("吊销令牌",
        '<p style="margin:0">确定吊销令牌「<b>' + esc(t.name) + '</b>」吗？</p><p class="xid-sub" style="margin-top:6px">吊销后使用该令牌的脚本将立即无法访问，且不可恢复。</p><p class="xid-err" data-xid-err hidden></p>',
        '<button type="button" class="xid-btn" data-xid-click="close">取消</button><button type="button" class="xid-btn xid-btn--danger" data-xid-click="confirmRevoke" data-id="' + esc(t.id) + '">确认吊销</button>'));
    }
    function confirmRevoke(btn) {
      if (st.busy) return;
      var id = btn.getAttribute("data-id"), t = find(id), layer = el.querySelector("[data-xid-layer]");
      st.busy = true; btn.disabled = true;
      return api("/auth/tokens/" + encodeURIComponent(id), { method: "DELETE" }).then(function () {
        st.busy = false; closeLayer(el);
        toast("已吊销令牌" + (t ? "「" + t.name + "」" : ""));
        return load();
      }, function (e) {
        st.busy = false; btn.disabled = false;
        setErr(layer, "吊销失败：" + errMsg(e));
      });
    }

    onHost(el, "tokens", {
      create: openCreate,
      submit: submit,
      close: function () { closeLayer(el); },
      maskClose: function () { if (plain === null) closeLayer(el); }, // 明文弹窗不允许点遮罩关闭，避免误关
      copyPlain: function () { if (plain) return copyText(plain); },
      closePlain: closePlain,
      revoke: askRevoke,
      confirmRevoke: confirmRevoke,
      reload: load
    });
    load();
    return { reload: load };
  }

  // ================================================================ 两步验证
  function qrInto(slot, uri) {
    if (!slot || !uri) return false;
    try {
      if (typeof root.qrcode === "function") { // qrcode-generator
        var q = root.qrcode(0, "M"); q.addData(uri); q.make();
        slot.innerHTML = q.createSvgTag ? q.createSvgTag({ cellSize: 4, margin: 2 }) : q.createImgTag(4, 8);
        return true;
      }
      if (typeof root.QRCode === "function") { // qrcodejs
        slot.innerHTML = "";
        new root.QRCode(slot, { text: uri, width: 176, height: 176 });
        return true;
      }
    } catch (e) { /* 回退到手动输入 */ }
    return false;
  }
  function groupSecret(s) { return String(s || "").replace(/\s+/g, "").replace(/(.{4})/g, "$1 ").trim(); }
  function codesHTML(codes) {
    return '<div class="xid-codes xid-mono" data-xid-codes>' + (codes || []).map(function (c) { return "<span>" + esc(c) + "</span>"; }).join("") + "</div>";
  }
  function validTOTP(code) { return /^\d{6}$/.test(code); }
  function validAnyCode(code) { return validTOTP(code) || /^[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}$/.test(code); }

  function renderMFA(el) {
    if (!el) throw new Error("renderMFA 需要容器元素");
    injectCSS();
    var st = { status: null, setup: null, loading: true, error: "", busy: false };
    var codes = null; // 恢复码：只在弹窗中出现一次
    el.innerHTML = '<div class="xid" data-xid-mfa><div class="xid-head"><div><h3>两步验证（MFA）</h3>' +
      '<p class="xid-sub">登录时除密码外，还需输入身份验证器 App（如 Google Authenticator、Microsoft Authenticator、阿里云 App）生成的 6 位动态码。</p></div></div>' +
      '<div data-xid-main></div></div>';
    function main() { return el.querySelector("[data-xid-main]"); }

    function draw() {
      var m = main(), s = st.status;
      if (!m) return;
      if (st.loading) { m.innerHTML = '<div class="xid-card xid-muted">正在加载…</div>'; return; }
      if (st.error && !s) { m.innerHTML = '<div class="xid-card"><p class="xid-err">' + esc(st.error) + '</p><button type="button" class="xid-btn" data-xid-click="reload">重试</button></div>'; return; }
      if (s && s.enabled) {
        m.innerHTML = '<div class="xid-card"><p style="margin:0 0 8px"><span class="xid-tag xid-tag--ok">已开启</span>' +
          (s.enabled_at ? '<span class="xid-muted">开启于 ' + esc(fmt(s.enabled_at)) + "</span>" : "") + "</p>" +
          '<p class="xid-sub">剩余恢复码 <b>' + esc(s.recovery_codes_left || 0) + "</b> 个" + ((s.recovery_codes_left || 0) <= 3 ? "，建议重新生成" : "") + "。</p>" +
          (s.required ? '<p class="xid-sub">管理员已要求你的账号开启两步验证，关闭后下次登录将要求重新绑定。</p>' : "") +
          '<div class="xid-foot" style="justify-content:flex-start"><button type="button" class="xid-btn" data-xid-click="askRegen">重新生成恢复码</button>' +
          '<button type="button" class="xid-btn xid-btn--danger" data-xid-click="askDisable">关闭两步验证</button></div></div>';
        return;
      }
      if (st.setup) {
        var uri = st.setup.otpauth_uri || "";
        m.innerHTML = '<div class="xid-card"><div class="xid-setup"><div style="flex:0 0 auto"><div class="xid-qr" data-xid-qr></div></div><div>' +
          '<p style="margin:0 0 8px"><b>1.</b> 用身份验证器 App 扫描左侧二维码；无法扫码时，手动添加以下密钥（基于时间）：</p>' +
          '<div class="xid-secret"><code class="xid-mono" style="flex:1" data-xid-secret>' + esc(groupSecret(st.setup.secret)) + '</code><button type="button" class="xid-btn" data-xid-click="copySecret">复制</button></div>' +
          '<details style="margin:8px 0"><summary class="xid-sub" style="cursor:pointer">查看 otpauth 链接</summary><code class="xid-mono" style="word-break:break-all" data-xid-uri>' + esc(uri) + "</code></details>" +
          '<p style="margin:12px 0 8px"><b>2.</b> 输入 App 中显示的 6 位验证码完成绑定：</p>' +
          '<div style="display:flex;gap:8px"><input class="xid-input xid-otp xid-mono" name="code" inputmode="numeric" maxlength="6" autocomplete="one-time-code" placeholder="000000" data-xid-enter="activate" />' +
          '<button type="button" class="xid-btn xid-btn--pri" data-xid-click="activate">确认绑定</button><button type="button" class="xid-btn" data-xid-click="cancelSetup">取消</button></div>' +
          '<p class="xid-err" data-xid-err' + (st.error ? "" : " hidden") + ">" + esc(st.error) + "</p></div></div></div>";
        var slot = m.querySelector("[data-xid-qr]");
        if (!qrInto(slot, uri) && slot) slot.innerHTML = '<span class="xid-sub" style="padding:12px;text-align:center">二维码组件未加载<br />请使用右侧密钥手动添加</span>';
        return;
      }
      m.innerHTML = '<div class="xid-card"><p style="margin:0 0 8px"><span class="xid-tag">未开启</span></p>' +
        (s && s.required ? '<div class="xid-warn">管理员已要求你的账号开启两步验证，请尽快完成绑定。</div>' : "") +
        '<p class="xid-sub">开启后可有效防止密码泄露导致的账号被盗。</p>' +
        '<p class="xid-err" data-xid-err' + (st.error ? "" : " hidden") + ">" + esc(st.error) + "</p>" +
        '<div class="xid-foot" style="justify-content:flex-start"><button type="button" class="xid-btn xid-btn--pri" data-xid-click="setup">开始绑定</button></div></div>';
    }

    function load() {
      st.loading = true; st.error = ""; draw();
      return api("/auth/mfa").then(function (s) { st.status = s || {}; st.loading = false; draw(); },
        function (e) { st.loading = false; st.status = null; st.error = "状态加载失败：" + errMsg(e); draw(); });
    }
    function setup() {
      if (st.busy) return;
      st.busy = true; st.error = "";
      return api("/auth/mfa/setup", { method: "POST" }).then(function (d) {
        st.busy = false; st.setup = d || {}; draw();
      }, function (e) { st.busy = false; st.error = "无法开始绑定：" + errMsg(e); draw(); });
    }
    function activate() {
      if (st.busy) return;
      var m = main(), code = val(m, "code").replace(/\s+/g, "");
      if (!validTOTP(code)) return setErr(m, "请输入 6 位数字验证码");
      st.busy = true; setErr(m, "");
      return api("/auth/mfa/activate", { method: "POST", body: { code: code } }).then(function (d) {
        st.busy = false; st.setup = null; st.error = "";
        st.status = { enabled: true, enabled_at: new Date().toISOString(), recovery_codes_left: ((d && d.recovery_codes) || []).length, required: st.status && st.status.required };
        draw();
        showCodes((d && d.recovery_codes) || [], "两步验证已开启");
        toast("两步验证已开启");
      }, function (e) { st.busy = false; setErr(m, "绑定失败：" + errMsg(e)); });
    }
    function showCodes(list, title) {
      codes = list.slice();
      openLayer(el, dialog(title,
        '<p style="margin:0 0 8px">以下恢复码可在手机丢失时代替动态码登录，每个只能使用一次：</p>' + codesHTML(codes) +
        '<div class="xid-warn">恢复码仅显示这一次，关闭后无法再次查看，请立即复制并离线保存。</div>',
        '<button type="button" class="xid-btn" data-xid-click="copyCodes">复制全部</button><button type="button" class="xid-btn xid-btn--pri" data-xid-click="closeCodes">我已保存，关闭</button>'));
    }
    function askCode(kind) {
      var disable = kind === "disable";
      openLayer(el, dialog(disable ? "关闭两步验证" : "重新生成恢复码",
        '<p class="xid-sub" style="margin:0 0 10px">' + (disable ? "关闭后登录将只需密码，账号安全性降低。" : "重新生成后旧的恢复码全部失效。") + "请输入身份验证器中的 6 位验证码（或一个恢复码）确认：</p>" +
        '<input class="xid-input xid-mono" name="code" maxlength="32" autocomplete="one-time-code" placeholder="6 位验证码 / 恢复码" data-xid-enter="' + kind + '" />' +
        '<p class="xid-err" data-xid-err hidden></p>',
        '<button type="button" class="xid-btn" data-xid-click="close">取消</button><button type="button" class="xid-btn ' + (disable ? "xid-btn--danger" : "xid-btn--pri") + '" data-xid-click="' + kind + '">' + (disable ? "确认关闭" : "确认生成") + "</button>"));
    }
    function submitCode(kind) {
      var layer = el.querySelector("[data-xid-layer]");
      if (!layer || st.busy) return;
      var code = val(layer, "code").trim();
      if (!validAnyCode(code.replace(/\s+/g, ""))) return setErr(layer, "请输入 6 位数字验证码或恢复码");
      st.busy = true; setErr(layer, "");
      var path = kind === "disable" ? "/auth/mfa/disable" : "/auth/mfa/recovery-codes";
      return api(path, { method: "POST", body: { code: code } }).then(function (d) {
        st.busy = false; closeLayer(el);
        if (kind === "disable") { toast("两步验证已关闭"); return load(); }
        var list = (d && d.recovery_codes) || [];
        if (st.status) st.status.recovery_codes_left = list.length;
        draw(); showCodes(list, "新的恢复码");
      }, function (e) { st.busy = false; setErr(layer, (kind === "disable" ? "关闭失败：" : "生成失败：") + errMsg(e)); });
    }

    onHost(el, "mfa", {
      reload: load,
      setup: setup,
      activate: activate,
      cancelSetup: function () { st.setup = null; st.error = ""; draw(); },
      copySecret: function () { if (st.setup) return copyText(String(st.setup.secret || "").replace(/\s+/g, "")); },
      askDisable: function () { askCode("disable"); },
      askRegen: function () { askCode("regen"); },
      disable: function () { return submitCode("disable"); },
      regen: function () { return submitCode("regen"); },
      copyCodes: function () { if (codes) return copyText(codes.join("\n")); },
      closeCodes: function () { codes = null; closeLayer(el); },
      close: function () { closeLayer(el); },
      maskClose: function () { if (codes === null) closeLayer(el); }
    });
    load();
    return { reload: load };
  }

  // ================================================================ 登录时二次校验
  // challenge 即 /auth/login 返回的 data：{ mfa_required, mfa_ticket, mfa_enroll, secret, otpauth_uri, expires_in }
  // 也接受 { ticket }。成功后写入会话并 resolve { pair, user, recovery_codes }；取消则 reject（code = XS_MFA_CANCELLED）。
  function mfaChallenge(challenge) {
    challenge = challenge || {};
    var ticket = challenge.ticket || challenge.mfa_ticket;
    var doc = root.document;
    if (!ticket) return Promise.reject(new Error("缺少 MFA 票据，请重新登录"));
    if (!doc || !doc.body || typeof doc.createElement !== "function") return Promise.reject(new Error("页面未就绪"));
    injectCSS();
    var enroll = !!(challenge.enroll || challenge.mfa_enroll);
    var uri = challenge.otpauth_uri || "", secret = challenge.secret || "";
    return new Promise(function (resolve, reject) {
      var box = doc.createElement("div");
      box.setAttribute("data-xid-challenge", "");
      box.className = "xid";
      var busy = false, done = null;
      box.innerHTML = dialog(enroll ? "绑定两步验证" : "两步验证",
        (enroll
          ? '<p class="xid-sub" style="margin:0 0 10px">管理员要求你的账号开启两步验证。请用身份验证器 App 扫码或手动添加密钥，然后输入 6 位验证码。</p>' +
            '<div style="display:flex;gap:14px;align-items:flex-start;margin-bottom:10px"><div class="xid-qr" data-xid-qr></div><div style="flex:1;min-width:0"><p class="xid-sub" style="margin:0 0 6px">密钥：</p><code class="xid-mono" style="word-break:break-all" data-xid-secret>' + esc(groupSecret(secret)) + '</code>' +
            '<details style="margin-top:8px"><summary class="xid-sub">otpauth 链接</summary><code class="xid-mono" style="word-break:break-all">' + esc(uri) + "</code></details></div></div>"
          : '<p class="xid-sub" style="margin:0 0 10px">请输入身份验证器 App 中的 6 位验证码；手机不在身边时可输入恢复码。</p>') +
        '<input class="xid-input xid-mono" name="code" maxlength="32" autocomplete="one-time-code" placeholder="' + (enroll ? "6 位验证码" : "6 位验证码 / 恢复码") + '" data-xid-enter="verify" />' +
        '<p class="xid-err" data-xid-err hidden></p>',
        '<button type="button" class="xid-btn" data-xid-click="cancel">取消</button><button type="button" class="xid-btn xid-btn--pri" data-xid-click="verify">验证并登录</button>');
      doc.body.appendChild(box);
      if (enroll) {
        var slot = box.querySelector("[data-xid-qr]");
        if (!qrInto(slot, uri) && slot) slot.innerHTML = '<span class="xid-sub" style="padding:10px;text-align:center">请手动添加密钥</span>';
      }
      function finish() { box.remove(); resolve(done); }
      function verify() {
        if (busy) return;
        var code = val(box, "code").trim();
        if (enroll ? !validTOTP(code) : !validAnyCode(code.replace(/\s+/g, ""))) return setErr(box, enroll ? "请输入 6 位数字验证码" : "请输入 6 位数字验证码或恢复码");
        busy = true; setErr(box, "");
        // _retried：401（验证码错误/票据过期）不得触发 refresh 流程
        return api("/auth/mfa/verify", { method: "POST", body: { mfa_ticket: ticket, code: code }, noAuth: true, _retried: true, timeoutMs: 20000 }).then(function (d) {
          busy = false; d = d || {};
          var user = d.user || null;
          if (root.XS && typeof root.XS.setSession === "function") root.XS.setSession(d.token, user);
          done = { pair: d.token, user: user, recovery_codes: d.recovery_codes || [] };
          if (d.recovery_codes && d.recovery_codes.length) {
            var m = box.querySelector(".xid-dialog");
            m.innerHTML = "<h4>保存恢复码</h4><p style=\"margin:0 0 8px\">两步验证已开启。以下恢复码可在手机丢失时代替动态码登录，每个只能使用一次：</p>" + codesHTML(d.recovery_codes) +
              '<div class="xid-warn">恢复码仅显示这一次，关闭后无法再次查看。</div><div class="xid-foot"><button type="button" class="xid-btn" data-xid-click="copyCodes">复制全部</button><button type="button" class="xid-btn xid-btn--pri" data-xid-click="finish">我已保存，进入平台</button></div>';
            return;
          }
          finish();
        }, function (e) {
          busy = false;
          setErr(box, errMsg(e));
          var inp = box.querySelector('[name="code"]');
          if (inp) { inp.value = ""; if (inp.focus) inp.focus(); }
        });
      }
      var H = {
        verify: verify,
        finish: finish,
        copyCodes: function () { if (done && done.recovery_codes) return copyText(done.recovery_codes.join("\n")); },
        cancel: function () { box.remove(); var e = new Error("已取消两步验证"); e.code = "XS_MFA_CANCELLED"; reject(e); }
      };
      ["click", "keydown"].forEach(function (type) {
        box.addEventListener(type, function (e) {
          if (!e || !e.target || typeof e.target.closest !== "function") return;
          var t = type === "click" ? e.target.closest("[data-xid-click]") : (e.key === "Enter" ? e.target.closest("[data-xid-enter]") : null);
          if (!t) return;
          var act = t.getAttribute(type === "click" ? "data-xid-click" : "data-xid-enter");
          if (H[act]) { if (type === "keydown" && e.preventDefault) e.preventDefault(); return H[act](t, e); }
        });
      });
      var first = box.querySelector('[name="code"]');
      if (first && first.focus) first.focus();
    });
  }

  // ================================================================ 管理员
  function userMfaBadges(ids) {
    var uniq = [];
    (ids || []).forEach(function (id) {
      var n = Number(id);
      if (n > 0 && Math.floor(n) === n && uniq.indexOf(n) < 0) uniq.push(n);
    });
    if (!uniq.length) return Promise.resolve({});
    var chunks = [];
    for (var i = 0; i < uniq.length; i += 200) chunks.push(uniq.slice(i, i + 200));
    return Promise.all(chunks.map(function (c) {
      return api("/users/mfa-flags", { query: { ids: c.join(",") } });
    })).then(function (parts) {
      var out = {};
      uniq.forEach(function (id) { out[id] = false; });
      parts.forEach(function (p) { Object.keys(p || {}).forEach(function (k) { out[k] = !!p[k]; }); });
      return out;
    });
  }

  function resetUserMFA(id) {
    var n = Number(id);
    if (!(n > 0) || Math.floor(n) !== n) return Promise.reject(new Error("用户 ID 不正确"));
    return api("/users/" + n + "/mfa", { method: "DELETE" });
  }

  root.XSIdentity = {
    renderTokens: renderTokens,
    renderMFA: renderMFA,
    mfaChallenge: mfaChallenge,
    userMfaBadges: userMfaBadges,
    resetUserMFA: resetUserMFA,
    _esc: esc
  };
})(typeof window !== "undefined" ? window : this);
