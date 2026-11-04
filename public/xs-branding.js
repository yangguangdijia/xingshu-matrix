/*
 * 外观（设置 → 外观）全局生效：平台名称、Logo、主题色、登录页标题、版权信息。
 * 数据来自公开接口 GET /branding（登录前可读）。
 * 主题色：重写已加载样式表里平台默认的几种蓝色（按钮、选中态、链接、浅色底），再覆盖 --x-blue 系列变量。
 */
(function (root) {
  "use strict";
  var DEFAULT = "星枢 Matrix";
  var B = { name: "", logo: "", primary_color: "", login_title: "", copyright: "" };
  function split(n) { var i = n.lastIndexOf(" "); return i > 0 ? [n.slice(0, i), n.slice(i + 1)] : [n, ""]; }
  function esc(s) { return String(s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }

  /* ---------- 颜色工具 ---------- */
  function hex2rgb(h) { h = h.replace("#", ""); return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)]; }
  function rgb2hex(c) { return "#" + c.map(function (v) { v = Math.max(0, Math.min(255, Math.round(v))); return (v < 16 ? "0" : "") + v.toString(16); }).join(""); }
  function mix(a, b, t) { var x = hex2rgb(a), y = hex2rgb(b); return rgb2hex([0, 1, 2].map(function (i) { return x[i] + (y[i] - x[i]) * t; })); }
  // 平台默认蓝色 → 由主题色派生（强调色 / 悬停 / 深色 / 浅底 / 更浅底）
  function palette(p) {
    return {
      main: p, hover: mix(p, "#ffffff", 0.2), dark: mix(p, "#000000", 0.15), bg: mix(p, "#ffffff", 0.9), bg2: mix(p, "#ffffff", 0.93), line: mix(p, "#ffffff", 0.55)
    };
  }
  var MAP = { "#1677ff": "main", "#1668dc": "main", "#1664ff": "main", "#2563eb": "main", "#2f6bff": "main", "#3b82f6": "hover", "#4096ff": "hover",
    "#0958d9": "dark", "#e6f4ff": "bg", "#e8f1ff": "bg", "#eff6ff": "bg2", "#f0f6ff": "bg2", "#94b8ff": "line", "#91caff": "line", "#bccfe6": "line" };
  var themed = false;
  function applyTheme() {
    if (!/^#[0-9a-fA-F]{6}$/.test(B.primary_color || "") || B.primary_color.toLowerCase() === "#1677ff") return;
    var pal = palette(B.primary_color.toLowerCase());
    var re = new RegExp("(" + Object.keys(MAP).join("|") + ")", "gi");
    [].forEach.call(document.styleSheets, function (sh) {
      var rules; try { rules = sh.cssRules; } catch (e) { return; }
      walk(rules);
    });
    function walk(rules) {
      [].forEach.call(rules || [], function (r) {
        if (r.cssRules) walk(r.cssRules);
        if (!r.style || r.__xsThemed) return;
        for (var i = 0; i < r.style.length; i++) {
          var prop = r.style[i], val = r.style.getPropertyValue(prop);
          if (val && re.test(val)) { re.lastIndex = 0; r.style.setProperty(prop, val.replace(re, function (m) { return pal[MAP[m.toLowerCase()]]; }), r.style.getPropertyPriority(prop)); }
          re.lastIndex = 0;
        }
        r.__xsThemed = true;
      });
    }
    var st = document.getElementById("xs-theme-vars") || document.createElement("style");
    st.id = "xs-theme-vars";
    st.textContent = ":root{--x-blue:" + pal.main + ";--x-blue-h:" + pal.hover + ";--x-ink:" + pal.dark + ";--x-blue-bg:" + pal.bg + "}";
    document.head.appendChild(st);
    themed = true;
  }

  function apply() {
    var name = B.name && B.name !== DEFAULT ? B.name : "";
    if (name) {
      var p = split(name);
      if (document.title.indexOf(DEFAULT) >= 0) document.title = document.title.replace(DEFAULT, name);
      [].forEach.call(document.querySelectorAll(".x-wm"), function (el) {
        if (el.getAttribute("data-brand") === name) return;
        el.innerHTML = "<b>" + esc(p[0]) + "</b>" + (p[1] ? "<i>" + esc(p[1]) + "</i>" : "");
        el.setAttribute("data-brand", name);
      });
      [].forEach.call(document.querySelectorAll(".x-bname, .x-ahead b"), function (el) { if (el.textContent === DEFAULT) el.textContent = name; });
      [].forEach.call(document.querySelectorAll(".x-sbrand[aria-label]"), function (el) { el.setAttribute("aria-label", name + " 首页"); });
    }
    if (B.logo) {
      [].forEach.call(document.querySelectorAll("svg.x-mark, svg.x-bmark, .x-ahead > svg"), function (svg) {
        var img = document.createElement("img");
        img.src = B.logo; img.alt = ""; img.className = (svg.getAttribute("class") || "") + " xs-logo-img";
        img.setAttribute("aria-hidden", "true");
        img.style.width = (svg.getAttribute("width") || 32) + "px"; img.style.height = (svg.getAttribute("height") || 32) + "px"; img.style.objectFit = "contain";
        svg.replaceWith(img);
      });
    }
    if (B.login_title) {
      var h = document.querySelector(".x-bhead");
      if (h && h.getAttribute("data-brand") !== B.login_title) { h.textContent = B.login_title; h.setAttribute("data-brand", B.login_title); }
    }
    if (B.copyright) {
      var cp = document.querySelector(".x-bcopy");
      if (cp && cp.textContent !== B.copyright) cp.textContent = B.copyright;
    }
  }
  var pending = false, observer = null;
  function schedule() { if (pending) return; pending = true; requestAnimationFrame(function () { pending = false; apply(); }); }
  function load() {
    var base = (root.XS && root.XS.base) || "/api/v1";
    return fetch(base + "/branding", { credentials: "omit" }).then(function (r) { return r.ok ? r.json() : null; }).then(function (j) {
      var d = (j && j.data) || {};
      Object.keys(B).forEach(function (k) { B[k] = d[k] || ""; });
      var custom = (B.name && B.name !== DEFAULT) || B.logo || B.primary_color || B.login_title || B.copyright;
      if (!custom) return;
      if (!themed) applyTheme();
      apply();
      if (!observer) { observer = new MutationObserver(schedule); observer.observe(document.body, { childList: true, subtree: true }); }
    }).catch(function () {});
  }
  root.XSBranding = { reload: function () { themed = false; return load().then(function () { location.reload(); }); }, palette: palette };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", load); else load();
})(window);
