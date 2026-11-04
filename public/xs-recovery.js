/*
 * 忘记密码 / 重置密码（登录页）：
 *   登录页「忘记密码」→ 输入用户名或邮箱 → POST /auth/password/forgot（无论账号是否存在都返回同样提示）
 *   邮件链接 #/reset-password?token=... → 设置新密码 → POST /auth/password/reset
 * 本脚本在 <head> 里尽早加载，先把令牌从地址栏取出并移除，避免登录跳转时丢失或留在历史记录里。
 */
(function (root) {
  "use strict";
  var KEY = "xs.reset.token", token = "";
  try {
    var m = /^#\/reset-password\?token=([A-Za-z0-9_-]{20,})/.exec(location.hash || "");
    if (m) { token = m[1]; sessionStorage.setItem(KEY, token); history.replaceState(null, "", location.pathname + location.search + "#/login"); }
    else token = sessionStorage.getItem(KEY) || "";
  } catch (e) {}
  function base() { return (root.XS && root.XS.base) || "/api/v1"; }
  function call(method, path, body) {
    return fetch(base() + path, { method: method, credentials: "omit", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined })
      .then(function (r) { return r.json().catch(function () { return {}; }); })
      .then(function (j) { if (j && j.code) { var e = new Error(j.message || "请求失败"); e.code = j.code; throw e; } return (j && j.data) || {}; });
  }
  function esc(s) { return String(s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function card() { return document.getElementById("acard"); }
  function backToLogin() { location.hash = "#/login"; location.reload(); }
  var CSS = ".xr-form{display:flex;flex-direction:column;gap:14px}.xr-form input{height:44px;border:1px solid #d9dee8;border-radius:10px;padding:0 14px;font-size:14px}.xr-form button.xr-pri{height:46px;border:0;border-radius:10px;background:#1668dc;color:#fff;font-size:15px;font-weight:600;cursor:pointer}.xr-form button.xr-pri:disabled{opacity:.6}.xr-msg{font-size:13px;line-height:1.7;margin:0}.xr-msg.err{color:#d43030}.xr-msg.ok{color:#1a9b55}.xr-back{font-size:13px;color:#6b7785;cursor:pointer;background:none;border:0;padding:0;text-align:left}";
  function style() { if (document.getElementById("xr-css")) return; var s = document.createElement("style"); s.id = "xr-css"; s.textContent = CSS; document.head.appendChild(s); }

  function forgotView() {
    var c = card(); if (!c) return;
    style();
    c.innerHTML = '<div class="x-ahead"><b>找回密码</b></div><h1>找回密码</h1><p>输入用户名或绑定的邮箱，我们会把重置链接发到账号绑定的邮箱。</p>' +
      '<form class="xr-form" novalidate><input name="a" autocomplete="username" placeholder="用户名或邮箱" maxlength="128"><p class="xr-msg" role="status"></p>' +
      '<button class="xr-pri" type="submit">发送重置邮件</button><button class="xr-back" type="button">← 返回登录</button></form>';
    var f = c.querySelector("form"), msg = f.querySelector(".xr-msg"), btn = f.querySelector(".xr-pri");
    f.querySelector(".xr-back").onclick = backToLogin;
    f.elements.a.focus();
    f.onsubmit = function (e) {
      e.preventDefault();
      var a = f.elements.a.value.trim();
      if (!a) { msg.className = "xr-msg err"; msg.textContent = "请输入用户名或邮箱"; return; }
      btn.disabled = true; msg.className = "xr-msg"; msg.textContent = "正在发送…";
      call("POST", "/auth/password/forgot", { account: a }).then(function (r) {
        msg.className = "xr-msg ok"; msg.textContent = r.message || "如果该账号存在并绑定了邮箱，重置链接已发送。"; btn.textContent = "已发送";
      }).catch(function (err) { btn.disabled = false; msg.className = "xr-msg err"; msg.textContent = err.message; });
    };
  }

  function resetView() {
    var c = card(); if (!c || c.getAttribute("data-xr") === "reset") return;
    style();
    c.setAttribute("data-xr", "reset");
    c.innerHTML = '<div class="x-ahead"><b>重置密码</b></div><h1>设置新密码</h1><p>新密码需满足平台密码策略。设置后，该账号在其它设备上的登录会全部失效。</p>' +
      '<form class="xr-form" novalidate><input name="p1" type="password" autocomplete="new-password" placeholder="新密码"><input name="p2" type="password" autocomplete="new-password" placeholder="再次输入新密码">' +
      '<p class="xr-msg" role="status"></p><button class="xr-pri" type="submit">确认重置</button><button class="xr-back" type="button">← 返回登录</button></form>';
    var f = c.querySelector("form"), msg = f.querySelector(".xr-msg"), btn = f.querySelector(".xr-pri");
    f.querySelector(".xr-back").onclick = function () { try { sessionStorage.removeItem(KEY); } catch (e) {} backToLogin(); };
    f.elements.p1.focus();
    f.onsubmit = function (e) {
      e.preventDefault();
      var a = f.elements.p1.value, b = f.elements.p2.value;
      if (!a) { msg.className = "xr-msg err"; msg.textContent = "请输入新密码"; return; }
      if (a !== b) { msg.className = "xr-msg err"; msg.textContent = "两次输入的密码不一致"; return; }
      btn.disabled = true; msg.className = "xr-msg"; msg.textContent = "正在提交…";
      call("POST", "/auth/password/reset", { token: token, password: a }).then(function (r) {
        try { sessionStorage.removeItem(KEY); } catch (e2) {}
        token = "";
        msg.className = "xr-msg ok"; msg.textContent = (r.message || "密码已重置") + "，3 秒后回到登录页。";
        setTimeout(backToLogin, 3000);
      }).catch(function (err) { btn.disabled = false; msg.className = "xr-msg err"; msg.textContent = err.message; });
    };
  }

  function boot() {
    document.addEventListener("click", function (e) {
      var a = e.target.closest && e.target.closest("#lforgot");
      if (!a) return;
      e.preventDefault(); e.stopImmediatePropagation();
      call("GET", "/auth/password/forgot").then(function (r) {
        if (r.available) forgotView();
        else if (root.XS && root.XS.toast) root.XS.toast("系统邮件未配置，请联系平台管理员重置密码");
      }).catch(function () { if (root.XS && root.XS.toast) root.XS.toast("请联系平台管理员重置密码"); });
    }, true);
    if (!token) return;
    // 登录卡片渲染出来后替换为重置表单
    var tryShow = function () { var c = card(), auth = document.getElementById("auth"); if (c && auth && !auth.hidden && c.querySelector("form")) resetView(); };
    new MutationObserver(function () { if (token) tryShow(); }).observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ["hidden"] });
    tryShow();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot); else boot();
})(window);
