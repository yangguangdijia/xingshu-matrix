/*
 * 平台名称（设置 → 基本信息 → 平台名称）全局生效：浏览器标题、侧边栏品牌、登录页。
 * 名称来自公开接口 GET /branding（登录前可读），例如「星枢 Matrix」或「某某云运维平台」。
 */
(function (root) {
  "use strict";
  var DEFAULT = "星枢 Matrix", name = "";
  function split(n) { var i = n.lastIndexOf(" "); return i > 0 ? [n.slice(0, i), n.slice(i + 1)] : [n, ""]; }
  function esc(s) { return String(s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function apply() {
    if (!name || name === DEFAULT) return;
    var p = split(name);
    if (document.title.indexOf(DEFAULT) >= 0) document.title = document.title.replace(DEFAULT, name);
    else if (document.title !== name && !/ - /.test(document.title)) document.title = name;
    [].forEach.call(document.querySelectorAll(".x-wm"), function (el) {
      if (el.getAttribute("data-brand") === name) return;
      el.innerHTML = "<b>" + esc(p[0]) + "</b>" + (p[1] ? "<i>" + esc(p[1]) + "</i>" : "");
      el.setAttribute("data-brand", name);
    });
    [].forEach.call(document.querySelectorAll(".x-bname, .x-ahead b"), function (el) { if (el.textContent === DEFAULT) el.textContent = name; });
    [].forEach.call(document.querySelectorAll(".x-sbrand[aria-label]"), function (el) { el.setAttribute("aria-label", name + " 首页"); });
  }
  var pending = false;
  function schedule() { if (pending) return; pending = true; requestAnimationFrame(function () { pending = false; apply(); }); }
  function load() {
    var base = (root.XS && root.XS.base) || "/api/v1";
    fetch(base + "/branding", { credentials: "omit" }).then(function (r) { return r.ok ? r.json() : null; }).then(function (j) {
      name = (j && j.data && j.data.name) || "";
      if (!name || name === DEFAULT) return;
      apply();
      new MutationObserver(schedule).observe(document.body, { childList: true, subtree: true });
    }).catch(function () {});
  }
  root.XSBranding = { reload: load };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", load); else load();
})(window);
