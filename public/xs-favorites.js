/*
 * 星枢 Matrix · 收藏 / 快速访问  window.XSFav
 *
 *   - 顶栏面包屑右侧的星标按钮：收藏 / 取消收藏当前页面（路由 + 标题）。
 *   - 侧边菜单顶部「收藏」分组：点击打开，× 取消收藏，拖拽或 ↑↓ 按钮（以及 Alt+↑/↓）调整顺序。
 *   - 最多 20 个；保存在 localStorage，按用户隔离（与 xs-cmdk 最近访问同一套键规则：用户 id → 用户名 → local）。
 *     后端暂无个人偏好接口，因此只存本地；同一浏览器的其他标签页通过 storage 事件同步。
 *   - 演示模式同样可用（不发任何请求）。
 *   - xs-cmdk 空输入时调用 XSFav.list() 把收藏排在最前。
 *
 * 接线：index.html 加载本文件与 xs-favorites.css；drawSide() 重建侧栏后调用 XSFav.mount()，drawTop() 末尾调用
 * XSFav.syncStar()（见 /workspace/work/briefs/v132/p2-favs.patches.py）。未接这两个钩子时，模块用
 * MutationObserver 监听 #nav / #crumbs 自行补挂。
 */
(function (root) {
  "use strict";

  var MAX = 20, PREFIX = "xs-fav:", MAX_T = 120, MAX_M = 60, MAX_H = 300;
  var STAR = '<svg class="xf-star" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3.6l2.6 5.3 5.8.8-4.2 4.1 1 5.8L12 16.9l-5.2 2.7 1-5.8-4.2-4.1 5.8-.8z"/></svg>';
  var I_UP = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 14l5-5 5 5"/></svg>';
  var I_DN = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 10l5 5 5-5"/></svg>';
  var I_RM = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 7l10 10M17 7L7 17"/></svg>';

  function X() { return root.XS || null; }
  function doc() { return root.document || null; }
  function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function safeHash(h) { h = String(h || ""); return h.length <= MAX_H && /^#\/[^\s"'<>`]*$/.test(h) ? h : ""; }
  function pinnable(h) { h = safeHash(h); return !!h && h !== "#/" && !/^#\/login\b/.test(h); }
  function toast(msg) { var x = X(); if (x && typeof x.toast === "function") x.toast(msg); }

  /* ---------------------------------------------------------------- 存储 */
  function storage() { try { return root.localStorage || null; } catch (e) { return null; } }
  function userKey() {
    var u = (X() && X().user) || null, id = u ? (u.id != null ? u.id : u.username) : null;
    return id != null && String(id) !== "" ? String(id) : "local";
  }
  function key() { return PREFIX + userKey(); }
  function norm(r) {
    if (!r || typeof r !== "object") return null;
    var h = safeHash(r.h), t = typeof r.t === "string" ? r.t.trim().slice(0, MAX_T) : "";
    if (!h || !t) return null;
    return { h: h, t: t, m: typeof r.m === "string" ? r.m.slice(0, MAX_M) : "" };
  }
  function list() {
    var ls = storage(), raw = null, arr;
    try { raw = ls && ls.getItem(key()); } catch (e) { raw = null; }
    try { arr = JSON.parse(raw || "[]"); } catch (e) { arr = []; }
    if (!Array.isArray(arr)) return [];
    var seen = {}, out = [];
    arr.forEach(function (r) { var o = norm(r); if (o && !seen[o.h] && out.length < MAX) { seen[o.h] = 1; out.push(o); } });
    return out;
  }
  function save(arr) {
    var ls = storage();
    try { if (ls) ls.setItem(key(), JSON.stringify(arr.slice(0, MAX))); } catch (e) { toast("收藏保存失败：浏览器存储不可用"); return false; }
    changed();
    return true;
  }
  function indexOf(h) { var a = list(); for (var i = 0; i < a.length; i++) if (a[i].h === h) return i; return -1; }
  function has(h) { h = safeHash(h); return !!h && indexOf(h) >= 0; }

  // 返回 "added" | "exists" | "full" | "invalid"
  function add(entry) {
    var o = norm(entry);
    if (!o || !pinnable(o.h)) return "invalid";
    var a = list();
    for (var i = 0; i < a.length; i++) if (a[i].h === o.h) return "exists";
    if (a.length >= MAX) return "full";
    a.push(o);
    return save(a) ? "added" : "invalid";
  }
  function remove(h) {
    var a = list(), n = a.length;
    a = a.filter(function (r) { return r.h !== h; });
    return a.length !== n ? save(a) : false;
  }
  function moveTo(from, to) {
    var a = list();
    if (from < 0 || from >= a.length) return false;
    to = Math.max(0, Math.min(a.length - 1, to));
    if (to === from) return false;
    var it = a.splice(from, 1)[0];
    a.splice(to, 0, it);
    return save(a);
  }
  function move(h, delta) { var i = indexOf(h); return i >= 0 ? moveTo(i, i + delta) : false; }

  /* ---------------------------------------------------------------- 当前页面 */
  function curHash() { return safeHash(root.location && root.location.hash); }
  function crumbTitle() {
    var d = doc(), c = d && d.getElementById && d.getElementById("crumbs");
    var b = c && c.querySelector && c.querySelector("b");
    return b ? String(b.textContent || "").trim() : "";
  }
  function current() {
    var h = curHash();
    if (!pinnable(h)) return null;
    var base = h.split("?")[0], t = null, cm = root.XSCmdk;
    try {
      var fn = cm && (typeof cm.titleFor === "function" ? cm.titleFor : cm._t && cm._t.titleFor);
      t = fn ? fn(base) : null;
    } catch (e) { t = null; }
    var title = (t && t.t) || crumbTitle();
    if (!title) return null;
    return { h: h, t: title, m: (t && t.m) || "" };
  }
  function toggle() {
    var c = current();
    if (!c) { toast("当前页面不支持收藏"); return "invalid"; }
    if (has(c.h)) { remove(c.h); toast("已取消收藏「" + c.t + "」"); return "removed"; }
    var r = add(c);
    if (r === "full") toast("最多收藏 " + MAX + " 个页面，请先移除不常用的收藏");
    else if (r === "added") toast("已收藏「" + c.t + "」");
    return r;
  }

  /* ---------------------------------------------------------------- 订阅 */
  var subs = [];
  function onChange(fn) { if (typeof fn === "function") subs.push(fn); return function () { subs = subs.filter(function (f) { return f !== fn; }); }; }
  function changed() {
    drawGroup(); syncStar();
    subs.slice().forEach(function (fn) { try { fn(list()); } catch (e) { if (root.console) root.console.warn("[XSFav]", e); } });
  }

  /* ---------------------------------------------------------------- 顶栏星标 */
  function starBtn() {
    var d = doc();
    if (!d || !d.getElementById) return null;
    var b = d.getElementById("xfStar");
    if (b) return b;
    var c = d.getElementById("crumbs");
    if (!c || !c.insertAdjacentHTML) return null;
    c.insertAdjacentHTML("afterend", '<button type="button" class="x-ibtn xf-starbtn x-devo" id="xfStar" aria-pressed="false" aria-label="收藏当前页面" title="收藏当前页面">' + STAR + "</button>");
    b = d.getElementById("xfStar");
    if (b) b.onclick = function (e) { if (e && e.preventDefault) e.preventDefault(); toggle(); };
    return b;
  }
  function syncStar() {
    var b = starBtn();
    if (!b) return;
    var h = curHash(), ok = pinnable(h), on = ok && has(h);
    b.hidden = !ok;
    b.setAttribute("aria-pressed", String(on));
    var label = on ? "取消收藏当前页面" : "收藏当前页面";
    b.setAttribute("aria-label", label);
    b.setAttribute("title", label);
    if (b.classList) b.classList.toggle("on", on);
  }

  /* ---------------------------------------------------------------- 侧边分组 */
  function groupHTML() {
    var a = list(), h = curHash(), n = a.length;
    var head = '<div class="xf-h"><span>收藏</span>' + (n ? "<small>" + n + "/" + MAX + "</small>" : "") + "</div>";
    if (!n) return head + '<p class="xf-hint">点击顶栏标题旁的 ☆ 收藏常用页面</p>';
    return head + '<ul class="xf-list" role="list">' + a.map(function (r, i) {
      var on = r.h === h;
      return '<li class="xf-it" draggable="true" data-xf-i="' + i + '">' +
        '<a class="x-si xf-a' + (on ? " on" : "") + '" href="' + esc(r.h) + '" title="' + esc(r.m ? r.t + " · " + r.m : r.t) + '"' + (on ? ' aria-current="page"' : "") + ">" + STAR + "<span>" + esc(r.t) + "</span></a>" +
        '<span class="xf-ops">' +
        '<button type="button" class="xf-op" data-xf-up="' + i + '"' + (i === 0 ? " disabled" : "") + ' aria-label="上移「' + esc(r.t) + '」">' + I_UP + "</button>" +
        '<button type="button" class="xf-op" data-xf-down="' + i + '"' + (i === n - 1 ? " disabled" : "") + ' aria-label="下移「' + esc(r.t) + '」">' + I_DN + "</button>" +
        '<button type="button" class="xf-op" data-xf-rm="' + i + '" aria-label="取消收藏「' + esc(r.t) + '」">' + I_RM + "</button>" +
        "</span></li>";
    }).join("") + "</ul>";
  }
  function groupEl() {
    var d = doc(), nav = d && d.getElementById && d.getElementById("nav");
    var box = nav && nav.querySelector && nav.querySelector(".x-nav");
    return box ? box.querySelector("[data-xf-grp]") : null;
  }
  var focusAfter = null;
  function drawGroup() {
    var g = groupEl();
    if (!g) return;
    g.innerHTML = groupHTML();
    if (focusAfter) {
      var f = g.querySelector(focusAfter);
      if (f && !f.disabled && f.focus) f.focus();
      else { var a = g.querySelector(".xf-a"); if (a && a.focus && /xf-(up|down|rm)/.test(focusAfter)) a.focus(); }
      focusAfter = null;
    }
  }
  function itemsAt(g) { return g && g.querySelectorAll ? g.querySelectorAll("[data-xf-i]") : []; }
  function bindGroup(g) {
    var dragFrom = -1;
    g.addEventListener("click", function (e) {
      var t = e.target, b = t && t.closest && t.closest("[data-xf-up], [data-xf-down], [data-xf-rm]");
      if (!b) return;
      e.preventDefault();
      if (e.stopPropagation) e.stopPropagation();
      var a = list(), i;
      if (b.hasAttribute("data-xf-rm")) {
        i = +b.getAttribute("data-xf-rm");
        if (!a[i]) return;
        focusAfter = '[data-xf-rm="' + Math.min(i, a.length - 2) + '"]';
        remove(a[i].h);
        toast("已取消收藏「" + a[i].t + "」");
        return;
      }
      var up = b.hasAttribute("data-xf-up");
      i = +b.getAttribute(up ? "data-xf-up" : "data-xf-down");
      var to = i + (up ? -1 : 1);
      focusAfter = (up ? '[data-xf-up="' : '[data-xf-down="') + to + '"]';
      moveTo(i, to);
    });
    // 键盘：在收藏链接上 Alt+↑/↓ 调整顺序
    g.addEventListener("keydown", function (e) {
      if (!e.altKey || (e.key !== "ArrowUp" && e.key !== "ArrowDown")) return;
      var li = e.target && e.target.closest && e.target.closest("[data-xf-i]");
      if (!li) return;
      e.preventDefault();
      var i = +li.getAttribute("data-xf-i"), to = i + (e.key === "ArrowUp" ? -1 : 1);
      focusAfter = '[data-xf-i="' + to + '"] .xf-a';
      if (!moveTo(i, to)) focusAfter = null;
    });
    g.addEventListener("dragstart", function (e) {
      var li = e.target && e.target.closest && e.target.closest("[data-xf-i]");
      if (!li) return;
      dragFrom = +li.getAttribute("data-xf-i");
      if (li.classList) li.classList.add("xf-drag");
      if (e.dataTransfer) { e.dataTransfer.effectAllowed = "move"; try { e.dataTransfer.setData("text/plain", String(dragFrom)); } catch (x) {} }
    });
    g.addEventListener("dragover", function (e) {
      if (dragFrom < 0) return;
      var li = e.target && e.target.closest && e.target.closest("[data-xf-i]");
      if (!li) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = "move";
      [].forEach.call(itemsAt(g), function (n) { if (n.classList) n.classList.toggle("xf-over", n === li); });
    });
    g.addEventListener("drop", function (e) {
      var li = e.target && e.target.closest && e.target.closest("[data-xf-i]");
      if (dragFrom < 0 || !li) return;
      e.preventDefault();
      var from = dragFrom; dragFrom = -1;
      if (!moveTo(from, +li.getAttribute("data-xf-i"))) drawGroup();
    });
    g.addEventListener("dragend", function () {
      dragFrom = -1;
      [].forEach.call(itemsAt(g), function (n) { if (n.classList) { n.classList.remove("xf-over"); n.classList.remove("xf-drag"); } });
    });
  }
  // 侧栏被 drawSide() 整体重建后调用：在 .x-nav 顶部挂上「收藏」分组（已挂则只刷新）
  function mount() {
    var d = doc(), nav = d && d.getElementById && d.getElementById("nav");
    var box = nav && nav.querySelector && nav.querySelector(".x-nav");
    if (!box) return false;
    var g = box.querySelector("[data-xf-grp]");
    if (!g) {
      g = d.createElement("div");
      g.className = "xf-grp";
      g.setAttribute("data-xf-grp", "");
      g.setAttribute("role", "group");
      g.setAttribute("aria-label", "收藏");
      if (box.insertBefore && box.firstChild !== undefined) box.insertBefore(g, box.firstChild || null);
      else box.appendChild(g);
      bindGroup(g);
    }
    drawGroup();
    return true;
  }

  /* ---------------------------------------------------------------- 接线 */
  function onHash() { syncStar(); drawGroup(); }
  function observe(id, fn) {
    var d = doc(), el = d && d.getElementById && d.getElementById(id);
    if (!el || typeof root.MutationObserver !== "function") return;
    try { new root.MutationObserver(fn).observe(el, { childList: true }); } catch (e) {}
  }
  function init() {
    if (root.addEventListener) {
      root.addEventListener("hashchange", onHash);
      root.addEventListener("storage", function (e) { if (!e || e.key == null || e.key === key()) changed(); });
    }
    var x = X();
    if (x && typeof x.on === "function") { try { x.on("auth", function () { changed(); }); } catch (e) {} }
    observe("nav", function () { if (!groupEl()) mount(); });
    observe("crumbs", syncStar);
    mount();
    syncStar();
  }

  var XSFav = {
    MAX: MAX,
    STAR_SVG: STAR,
    list: list, has: has, add: add, remove: remove, move: move, moveTo: moveTo,
    toggle: toggle, current: current, onChange: onChange,
    mount: mount, syncStar: syncStar,
    _t: { key: key, groupHTML: groupHTML, safeHash: safeHash, pinnable: pinnable }
  };
  root.XSFav = XSFav;
  init();
})(typeof window !== "undefined" ? window : this);
