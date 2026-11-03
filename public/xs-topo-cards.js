/* 卡片式拓扑（参照 Rainbond / ACK One 拓扑）：点阵画布 + 卡片节点 + 从左到右分层 + 贝塞尔曲线连线，异常链路标红。
 *   XSTopoCanvas.render(host, graph, opts)   通用渲染器（tree / dag 两种布局，拖拽平移、滚轮缩放、适应画布）
 *   XSAppTopology.render(el, page, ctx)      网络 › 服务链路：入口 → 组件（工作负载 + Service）→ 依赖组件
 *       依赖来源：OpenTelemetry trace 调用边（有数据源时）+ 工作负载注解 xingshu.io/depends-on（声明式依赖）
 *   XSResourceTopology.render(host, clusters) 集群 › 拓扑：平台 → 环境 → 集群 → 已接入组件 / 数据源 / 镜像仓库
 * 数据全部来自真实接口，不做推测连线。
 */
(function (root) {
  "use strict";
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function items(d) { return Array.isArray(d) ? d : d && Array.isArray(d.items) ? d.items : []; }
  function get(path, q) { return root.XS.get(path, q); }
  function soft(p, fb) { return p.then(function (v) { return v; }, function () { return fb; }); }

  var ICON = {
    platform: '<svg viewBox="0 0 24 24"><path d="M12 2.5 20.5 7v10L12 21.5 3.5 17V7z" fill="#e8f0ff" stroke="#2563eb" stroke-width="1.6"/><circle cx="12" cy="12" r="3.2" fill="#2563eb"/></svg>',
    env: '<svg viewBox="0 0 24 24"><rect x="5" y="3.5" width="14" height="17" rx="1.5" fill="none" stroke="#0f9f7a" stroke-width="1.6"/><path d="M9 8h2M13 8h2M9 12h2M13 12h2M10.5 20.5v-4h3v4" stroke="#0f9f7a" stroke-width="1.6" fill="none"/></svg>',
    k8s: '<svg viewBox="0 0 24 24"><path d="M12 1.8 21 6.2l2 9.7-6.3 7.8H7.3L1 15.9 3 6.2z" fill="#326ce5"/><circle cx="12" cy="12.5" r="4.6" fill="none" stroke="#fff" stroke-width="1.5"/><path d="M12 5.5v4.4M12 15.1v4.4M5.6 9.8l4 1.6M14.4 13.6l4 1.6M5.6 15.2l4-1.6M14.4 11.4l4-1.6" stroke="#fff" stroke-width="1.4"/></svg>',
    ingress: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="#eef4ff" stroke="#2563eb" stroke-width="1.6"/><path d="M3 12h18M12 3c3 3.2 3 14.8 0 18M12 3c-3 3.2-3 14.8 0 18" stroke="#2563eb" stroke-width="1.3" fill="none"/></svg>',
    deploy: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#eaf1ff"/><path d="M7 8.5 12 6l5 2.5v6L12 17l-5-2.5z" fill="none" stroke="#2563eb" stroke-width="1.6"/><path d="M7 8.5 12 11l5-2.5M12 11v6" stroke="#2563eb" stroke-width="1.4" fill="none"/></svg>',
    sts: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#f3ecff"/><ellipse cx="12" cy="8" rx="5" ry="2" fill="none" stroke="#7c3aed" stroke-width="1.5"/><path d="M7 8v8c0 1.1 2.2 2 5 2s5-.9 5-2V8M7 12c0 1.1 2.2 2 5 2s5-.9 5-2" fill="none" stroke="#7c3aed" stroke-width="1.5"/></svg>',
    ds: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#e9f8f2"/><circle cx="8.5" cy="8.5" r="2" fill="#0f9f7a"/><circle cx="15.5" cy="8.5" r="2" fill="#0f9f7a"/><circle cx="8.5" cy="15.5" r="2" fill="#0f9f7a"/><circle cx="15.5" cy="15.5" r="2" fill="#0f9f7a"/></svg>',
    trace: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#fff4e5"/><path d="M7 16 11 9l3 4 3-6" fill="none" stroke="#d97706" stroke-width="1.7"/></svg>',
    prometheus: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#e6522c"/><path d="M12 4.5c1.4 2.6-1.2 3.5.4 6 1-1 1.1-1.9 1-2.8 2 1.6 2.7 3.6 2.4 5.6H8.2C7.6 10.4 11 8.8 12 4.5zM8 15h8v1.6H8zM9.3 17.6h5.4c0 1-1.2 1.9-2.7 1.9s-2.7-.9-2.7-1.9z" fill="#fff"/></svg>',
    loki: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#fff1e6"/><path d="M7 7h2v10H7zM11 9h2v8h-2zM15 11h2v6h-2z" fill="#f46800"/></svg>',
    alertmanager: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#fdecec"/><path d="M12 6a4 4 0 0 0-4 4v3l-1.5 2h11L16 13v-3a4 4 0 0 0-4-4zM10.5 17a1.5 1.5 0 0 0 3 0" fill="none" stroke="#dc2626" stroke-width="1.6"/></svg>',
    registry: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#e7f6f8"/><path d="M6.5 9.5 12 7l5.5 2.5v5L12 17l-5.5-2.5z" fill="none" stroke="#0891b2" stroke-width="1.6"/><path d="M6.5 9.5 12 12l5.5-2.5M12 12v5" stroke="#0891b2" stroke-width="1.3" fill="none"/></svg>',
    component: '<svg viewBox="0 0 24 24"><rect x="3" y="3" width="18" height="18" rx="4" fill="#f1f5f9"/><path d="M8 8h3v3H8zM13 8h3v3h-3zM8 13h3v3H8zM13 13h3v3h-3z" fill="#475569"/></svg>',
    jenkins: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#fdf2e9"/><circle cx="12" cy="10" r="4.5" fill="#f0d6b7" stroke="#335061" stroke-width="1.2"/><path d="M7 19c1-3 3-4 5-4s4 1 5 4" fill="#d33833"/></svg>',
    argocd: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#fdecec"/><circle cx="12" cy="10.5" r="5" fill="#fff" stroke="#ef7b4d" stroke-width="1.6"/><circle cx="12" cy="10.5" r="1.8" fill="#ef7b4d"/><path d="M9 16l-1 4M15 16l1 4" stroke="#ef7b4d" stroke-width="1.6"/></svg>'
  };
  var TONE = { ok: ["正常", "#e8f7ef", "#0f8a5f"], warn: ["部分就绪", "#fff6e5", "#b25e00"], bad: ["异常", "#fdecec", "#c42b1c"], idle: ["未检测", "#f1f3f6", "#6b7280"], info: ["", "#eaf1ff", "#2563eb"], env: ["环境", "#e8f7ef", "#0f8a5f"] };

  function css() {
    if (document.getElementById("xs-topo-css")) return;
    var s = document.createElement("style"); s.id = "xs-topo-css";
    s.textContent = [
      ".xt-wrap{position:relative;height:640px;border:1px solid #e6e9ed;border-radius:10px;overflow:hidden;background:#f7f8fa radial-gradient(circle,#d5d9e0 1px,transparent 1.2px) 0 0/18px 18px;cursor:grab;user-select:none}",
      ".xt-wrap.drag{cursor:grabbing}.xt-stage{position:absolute;left:0;top:0;transform-origin:0 0}",
      ".xt-stage svg{position:absolute;left:0;top:0;overflow:visible;pointer-events:none}",
      ".xt-edge{fill:none;stroke:#c9ced8;stroke-width:1.6}.xt-edge.bad{stroke:#e5484d;stroke-width:2}.xt-edge.dash{stroke-dasharray:5 5}",
      ".xt-edge.hi{stroke:#2563eb;stroke-width:2.2}.xt-edge.bad.hi{stroke:#c42b1c}.xt-elabel{font-size:11px;fill:#6b7280;paint-order:stroke;stroke:#f7f8fa;stroke-width:4px}.xt-elabel.bad{fill:#c42b1c}",
      ".xt-node{position:absolute;box-sizing:border-box;background:#fff;border:1px solid #e3e6eb;border-radius:8px;padding:12px 14px 11px;box-shadow:0 1px 2px rgba(16,24,40,.05);cursor:pointer;transition:box-shadow .15s,border-color .15s}",
      ".xt-node:before{content:'';position:absolute;left:12px;top:-1px;width:28px;height:3px;border-radius:0 0 3px 3px;background:var(--xt-c,#2563eb)}",
      ".xt-node:hover,.xt-node.hi{border-color:#9db8f5;box-shadow:0 4px 14px rgba(37,99,235,.12)}.xt-node.dim{opacity:.45}",
      ".xt-node.bad{border-color:#f3b7b2}.xt-head{display:flex;align-items:flex-start;gap:10px}",
      ".xt-ic{width:26px;height:26px;flex:none}.xt-ic svg{width:26px!important;height:26px!important;display:block;max-width:none}",
      ".xt-tt{flex:1;min-width:0}.xt-tt b{display:block;font-size:14px;font-weight:600;color:#1d2129;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;line-height:18px}",
      ".xt-tt span{display:block;font-size:12px;color:#86909c;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;margin-top:2px}",
      ".xt-badge{flex:none;font-size:12px;line-height:20px;padding:0 8px;border-radius:10px;white-space:nowrap}",
      ".xt-meta{margin-top:9px;font-size:12.5px;color:#4e5969;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.xt-meta i{font-style:normal;color:#c9cdd4;margin:0 6px}",
      ".xt-tags{margin-top:8px;display:flex;gap:6px;flex-wrap:nowrap;overflow:hidden}.xt-tag{font-size:11.5px;line-height:18px;padding:0 7px;border-radius:9px;white-space:nowrap;background:#f2f3f5;color:#4e5969}",
      ".xt-tag:before{content:'';display:inline-block;width:5px;height:5px;border-radius:50%;background:currentColor;margin-right:5px;vertical-align:1px;opacity:.8}",
      ".xt-tool{position:absolute;right:12px;top:12px;display:flex;gap:6px;z-index:2}.xt-tool button{width:30px;height:30px;border:1px solid #e3e6eb;background:#fff;border-radius:6px;cursor:pointer;font-size:15px;color:#4e5969}",
      ".xt-tool button:hover{background:#f2f3f5}.xt-legend{position:absolute;left:12px;bottom:10px;display:flex;gap:14px;font-size:12px;color:#86909c;z-index:2;background:rgba(247,248,250,.9);padding:4px 8px;border-radius:6px}",
      ".xt-legend i{display:inline-block;width:18px;height:0;border-top:2px solid #c9ced8;vertical-align:3px;margin-right:6px}.xt-legend i.bad{border-color:#e5484d}.xt-legend i.dash{border-top-style:dashed}",
      ".xt-bar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:12px}.xt-bar .xt-sp{flex:1}.xt-bar select{width:auto}.xt-sum{font-size:13px;color:#6b7280}",
      ".xt-empty{height:320px;display:flex;align-items:center;justify-content:center;color:#86909c;font-size:13px;border:1px dashed #dfe3e8;border-radius:10px;background:#fafbfc}"
    ].join("");
    document.head.appendChild(s);
  }

  /* ---------- 布局 ---------- */
  var W = 264, H = 96, HT = 120, COL = 340, ROW = 26;
  function nodeH(n) { return n.tags && n.tags.length ? HT : H; }
  function layoutTree(nodes, edges) {
    var by = {}, kids = {}, hasParent = {};
    nodes.forEach(function (n) { by[n.id] = n; kids[n.id] = []; });
    edges.forEach(function (e) { if (by[e.from] && by[e.to] && !hasParent[e.to]) { kids[e.from].push(e.to); hasParent[e.to] = true; } });
    var y = 0, pos = {};
    function place(id, depth) {
      var n = by[id], ch = kids[id];
      if (!ch.length) { pos[id] = { x: depth * COL, y: y }; y += nodeH(n) + ROW; return pos[id].y + nodeH(n) / 2; }
      var mids = ch.map(function (c) { return place(c, depth + 1); });
      var mid = (mids[0] + mids[mids.length - 1]) / 2;
      pos[id] = { x: depth * COL, y: mid - nodeH(n) / 2 };
      if (pos[id].y + nodeH(n) + ROW > y) y = pos[id].y + nodeH(n) + ROW;
      return mid;
    }
    nodes.filter(function (n) { return !hasParent[n.id]; }).forEach(function (n) { place(n.id, 0); y += ROW; });
    return pos;
  }
  function layoutDag(nodes, edges) {
    var by = {}, ins = {}, outs = {}, layer = {};
    nodes.forEach(function (n) { by[n.id] = n; ins[n.id] = []; outs[n.id] = []; });
    edges.forEach(function (e) { if (by[e.from] && by[e.to] && e.from !== e.to) { outs[e.from].push(e.to); ins[e.to].push(e.from); } });
    // 最长路径分层（有环时用访问标记截断）
    function depth(id, seen) {
      if (layer[id] != null) return layer[id];
      if (seen[id]) return 0;
      seen[id] = true;
      var d = by[id].layer != null ? by[id].layer : 0;
      ins[id].forEach(function (p) { d = Math.max(d, depth(p, seen) + 1); });
      seen[id] = false;
      return (layer[id] = d);
    }
    nodes.forEach(function (n) { depth(n.id, {}); });
    // 没有上游、也不是入口的节点（如 worker、loadgen）靠近它依赖的下游放置，避免长连线横穿
    nodes.forEach(function (n) {
      if (ins[n.id].length || by[n.id].layer === 0 || !outs[n.id].length) return;
      var m = Math.min.apply(null, outs[n.id].map(function (t) { return layer[t]; }));
      if (m - 1 > layer[n.id]) layer[n.id] = m - 1;
    });
    var cols = [];
    nodes.forEach(function (n) { (cols[layer[n.id]] = cols[layer[n.id]] || []).push(n); });
    var order = {};
    cols.forEach(function (col, ci) {
      if (!col) return;
      if (ci > 0) col.sort(function (a, b) { return bary(a) - bary(b); });
      col.forEach(function (n, i) { order[n.id] = i; });
    });
    function bary(n) { var p = ins[n.id]; if (!p.length) return 1e6 + (order[n.id] || 0); return p.reduce(function (s, id) { return s + (order[id] || 0); }, 0) / p.length; }
    var heights = cols.map(function (col) { return (col || []).reduce(function (s, n) { return s + nodeH(n) + ROW; }, -ROW); });
    var maxH = Math.max.apply(null, heights.concat([0])), pos = {};
    cols.forEach(function (col, ci) {
      if (!col) return;
      var y = (maxH - heights[ci]) / 2;
      col.forEach(function (n) { pos[n.id] = { x: ci * COL, y: y }; y += nodeH(n) + ROW; });
    });
    return pos;
  }

  /* ---------- 渲染器 ---------- */
  function card(n, p) {
    var t = TONE[n.tone] || null, badge = n.badge != null ? n.badge : t ? t[0] : "";
    var bt = TONE[n.badgeTone || n.tone] || TONE.info;
    return '<div class="xt-node' + (n.tone === "bad" ? " bad" : "") + '" data-xt-id="' + esc(n.id) + '" style="left:' + p.x + "px;top:" + p.y + "px;width:" + W + "px;height:" + nodeH(n) + "px;--xt-c:" + (n.color || "#2563eb") + '"' +
      (n.title ? ' title="' + esc(n.hint || n.title) + '"' : "") + ' tabindex="0" role="button">' +
      '<div class="xt-head"><span class="xt-ic">' + (ICON[n.icon] || ICON.component).replace('<svg ', '<svg width="26" height="26" style="width:26px;height:26px;min-width:26px" ') + '</span><span class="xt-tt"><b>' + esc(n.title) + "</b><span>" + esc(n.sub || "") + "</span></span>" +
      (badge ? '<span class="xt-badge" style="background:' + bt[1] + ";color:" + bt[2] + '">' + esc(badge) + "</span>" : "") + "</div>" +
      '<div class="xt-meta">' + (n.meta || []).filter(function (x) { return x !== "" && x != null; }).map(esc).join("<i>•</i>") + "</div>" +
      (n.tags && n.tags.length ? '<div class="xt-tags">' + n.tags.map(function (g) { return '<span class="xt-tag" style="background:' + (g.bg || "#f2f3f5") + ";color:" + (g.fg || "#4e5969") + '">' + esc(g.t) + "</span>"; }).join("") + "</div>" : "") +
      "</div>";
  }
  function edgePath(a, b, ha, hb) {
    var x1 = a.x + W, y1 = a.y + ha / 2, x2 = b.x, y2 = b.y + hb / 2;
    if (x2 <= x1) { x2 = b.x + W / 2; y2 = b.y + (y2 > y1 ? 0 : hb); }
    var dx = Math.max(60, (x2 - x1) * 0.5);
    return { d: "M" + x1 + "," + y1 + " C" + (x1 + dx) + "," + y1 + " " + (x2 - dx) + "," + y2 + " " + x2 + "," + y2, mx: (x1 + x2) / 2, my: (y1 + y2) / 2 };
  }
  function render(host, graph, opts) {
    css(); opts = opts || {};
    var nodes = graph.nodes, edges = graph.edges.filter(function (e) { return e.from !== e.to; });
    if (!nodes.length) { host.innerHTML = '<div class="xt-empty">' + esc(opts.empty || "没有可展示的资源") + "</div>"; return; }
    var pos = opts.layout === "tree" ? layoutTree(nodes, edges) : layoutDag(nodes, edges);
    var byId = {}; nodes.forEach(function (n) { byId[n.id] = n; });
    var maxX = 0, maxY = 0;
    nodes.forEach(function (n) { var p = pos[n.id]; maxX = Math.max(maxX, p.x + W); maxY = Math.max(maxY, p.y + nodeH(n)); });
    var paths = edges.filter(function (e) { return pos[e.from] && pos[e.to]; }).map(function (e, i) {
      var g = edgePath(pos[e.from], pos[e.to], nodeH(byId[e.from]), nodeH(byId[e.to]));
      return '<path class="xt-edge' + (e.tone === "bad" ? " bad" : "") + (e.dash ? " dash" : "") + '" data-xt-e="' + i + '" data-from="' + esc(e.from) + '" data-to="' + esc(e.to) + '" d="' + g.d + '"/>' +
        (e.label ? '<text class="xt-elabel' + (e.tone === "bad" ? " bad" : "") + '" x="' + g.mx + '" y="' + (g.my - 6) + '" text-anchor="middle">' + esc(e.label) + "</text>" : "");
    }).join("");
    host.innerHTML = '<div class="xt-wrap" style="height:' + Math.max(420, Math.min(680, maxY + 120)) + 'px"><div class="xt-tool"><button type="button" data-xt-z="1" title="放大" aria-label="放大">+</button><button type="button" data-xt-z="-1" title="缩小" aria-label="缩小">−</button><button type="button" data-xt-fit title="适应画布" aria-label="适应画布">⤢</button></div>' +
      '<div class="xt-stage" style="width:' + maxX + "px;height:" + maxY + 'px"><svg width="' + maxX + '" height="' + maxY + '">' + paths + "</svg>" +
      nodes.map(function (n) { return card(n, pos[n.id]); }).join("") + "</div>" +
      (opts.legend ? '<div class="xt-legend">' + opts.legend + "</div>" : "") + "</div>";
    var wrap = host.querySelector(".xt-wrap"), stage = host.querySelector(".xt-stage");
    var view = { s: 1, x: 0, y: 0 };
    function apply() { stage.style.transform = "translate(" + view.x + "px," + view.y + "px) scale(" + view.s + ")"; }
    function fit() {
      var w = wrap.clientWidth - 60, h = wrap.clientHeight - 70;
      view.s = Math.max(0.35, Math.min(1, w / maxX, h / maxY));
      view.x = (wrap.clientWidth - maxX * view.s) / 2; view.y = Math.max(24, (wrap.clientHeight - maxY * view.s) / 2);
      apply();
    }
    function zoom(f, cx, cy) {
      var ns = Math.max(0.3, Math.min(2, view.s * f));
      if (cx == null) { cx = wrap.clientWidth / 2; cy = wrap.clientHeight / 2; }
      view.x = cx - (cx - view.x) * ns / view.s; view.y = cy - (cy - view.y) * ns / view.s; view.s = ns; apply();
    }
    fit();
    [].forEach.call(host.querySelectorAll("[data-xt-z]"), function (b) { b.onclick = function () { zoom(b.getAttribute("data-xt-z") === "1" ? 1.2 : 1 / 1.2); }; });
    host.querySelector("[data-xt-fit]").onclick = fit;
    wrap.addEventListener("wheel", function (e) { e.preventDefault(); var r = wrap.getBoundingClientRect(); zoom(e.deltaY < 0 ? 1.1 : 1 / 1.1, e.clientX - r.left, e.clientY - r.top); }, { passive: false });
    var drag = null;
    wrap.addEventListener("mousedown", function (e) { if (e.target.closest(".xt-tool")) return; drag = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y, moved: false }; wrap.classList.add("drag"); });
    window.addEventListener("mousemove", function (e) { if (!drag) return; var dx = e.clientX - drag.x, dy = e.clientY - drag.y; if (Math.abs(dx) + Math.abs(dy) > 3) drag.moved = true; view.x = drag.vx + dx; view.y = drag.vy + dy; apply(); });
    window.addEventListener("mouseup", function () { if (drag) { wrap.classList.remove("drag"); setTimeout(function () { drag = null; }, 0); } });
    function highlight(id) {
      var linked = {}; linked[id] = true;
      [].forEach.call(host.querySelectorAll(".xt-edge"), function (p) {
        var on = id && (p.getAttribute("data-from") === id || p.getAttribute("data-to") === id);
        p.classList.toggle("hi", !!on);
        if (on) { linked[p.getAttribute("data-from")] = true; linked[p.getAttribute("data-to")] = true; }
      });
      [].forEach.call(host.querySelectorAll(".xt-node"), function (n) { var nid = n.getAttribute("data-xt-id"); n.classList.toggle("hi", !!id && nid === id); n.classList.toggle("dim", !!id && !linked[nid]); });
    }
    [].forEach.call(host.querySelectorAll(".xt-node"), function (el) {
      var n = byId[el.getAttribute("data-xt-id")];
      el.onmouseenter = function () { highlight(n.id); };
      el.onmouseleave = function () { highlight(null); };
      el.onclick = function () { if (drag && drag.moved) return; if (n.href) location.hash = n.href; else if (opts.onClick) opts.onClick(n); };
      el.onkeydown = function (e) { if (e.key === "Enter") el.onclick(); };
    });
  }
  root.XSTopoCanvas = { render: render, icons: ICON };

  /* ---------- 应用拓扑 ---------- */
  var KIND = { Deployment: ["无状态", "deploy", "#2563eb", "deployments"], StatefulSet: ["有状态", "sts", "#7c3aed", "statefulsets"], DaemonSet: ["守护进程", "ds", "#0f9f7a", "daemonsets"] };
  var APP = { ns: null };
  function ownerOf(pod) {
    var o = String(pod.owner || ""), i = o.indexOf("/");
    if (i < 0) return null;
    var kind = o.slice(0, i), name = o.slice(i + 1);
    if (kind === "ReplicaSet") { var h = pod.labels && pod.labels["pod-template-hash"]; kind = "Deployment"; name = h && name.slice(-h.length - 1) === "-" + h ? name.slice(0, -h.length - 1) : name.replace(/-[a-z0-9]{6,10}$/, ""); }
    return kind + "/" + name;
  }
  function annotationDeps(yaml) {
    var text = yaml && typeof yaml === "object" ? yaml.yaml || "" : String(yaml || "");
    var m = /^\s+xingshu\.io\/depends-on:\s*["']?([^"'\n]+)["']?\s*$/m.exec(text);
    return m ? m[1].split(",").map(function (s) { return s.trim(); }).filter(Boolean) : [];
  }
  function appRender(el, page, ctx) {
    var cid = encodeURIComponent(ctx.cluster.id), base = "/clusters/" + cid;
    page.draw(ctx, '<p class="x-empty">正在读取应用拓扑…</p>');
    return get(base + "/namespaces").then(function (nsData) {
      var nss = items(nsData).map(function (n) { return n.name || n.metadata && n.metadata.name; }).filter(Boolean)
        .filter(function (n) { return !/^(kube-|xingshu-system$|local-path|monitoring$)/.test(n) || n === APP.ns; });
      return Promise.all(["deployments", "statefulsets", "daemonsets"].map(function (k) { return soft(get(base + "/workloads/" + k, { page_size: 500 }), { items: [] }); })).then(function (w) {
        var counts = {};
        w.forEach(function (d) { items(d).forEach(function (x) { counts[x.namespace] = (counts[x.namespace] || 0) + 1; }); });
        nss = nss.filter(function (n) { return counts[n]; }).sort(function (a, b) { return (counts[b] || 0) - (counts[a] || 0); });
        if (!APP.ns || nss.indexOf(APP.ns) < 0) APP.ns = nss[0] || "default";
        return loadApp(el, page, ctx, base, nss, w);
      });
    }).catch(function (err) { page.draw(ctx, '<p class="x-aerr" role="alert">读取应用拓扑失败：' + esc(err.message || err) + "</p>"); });
  }
  function loadApp(el, page, ctx, base, nss, w) {
    var ns = APP.ns, q = { namespace: ns, page_size: 1000 };
    var workloads = [].concat.apply([], w.map(items)).filter(function (x) { return x.namespace === ns; });
    return Promise.all([
      soft(get(base + "/workloads/pods", q), { items: [] }), soft(get(base + "/network/services", q), { items: [] }),
      soft(get(base + "/network/endpoints", q), { items: [] }), soft(get(base + "/network/ingresses", q), { items: [] }),
      soft(get(base + "/hpa", q), { items: [] }), soft(get(base + "/network/policies", q), { items: [] }),
      root.XSTrafficTrace ? soft(root.XSTrafficTrace(ctx), null) : Promise.resolve(null)
    ].concat(workloads.map(function (x) { return soft(get(base + "/namespaces/" + encodeURIComponent(ns) + "/" + KIND[x.kind][3] + "/" + encodeURIComponent(x.name)), ""); })))
      .then(function (r) {
        var pods = items(r[0]).filter(function (p) { return p.namespace === ns; }), svcs = items(r[1]).filter(function (s) { return s.namespace === ns; });
        var eps = items(r[2]), ings = items(r[3]).filter(function (i) { return !i.namespace || i.namespace === ns; }), hpas = items(r[4]), pols = items(r[5]), traces = r[6];
        var yamls = r.slice(7);
        var nodes = [], edges = [], byKey = {}, svcOwner = {};
        // Service → 工作负载：EndpointSlice 中的 Pod 归属
        var podOwner = {}; pods.forEach(function (p) { podOwner[p.name] = ownerOf(p); });
        eps.forEach(function (e) { (e.endpoints || []).forEach(function (pt) { var o = pt.pod && podOwner[pt.pod.name]; if (o && e.service) svcOwner[e.service] = o; }); });
        svcs.forEach(function (s) {
          if (svcOwner[s.name] || !s.selector) return;
          var sel = s.selector; pods.forEach(function (p) { if (Object.keys(sel).every(function (k) { return p.labels && p.labels[k] === sel[k]; })) svcOwner[s.name] = podOwner[p.name]; });
        });
        var svcByOwner = {}; Object.keys(svcOwner).forEach(function (s) { (svcByOwner[svcOwner[s]] = svcByOwner[svcOwner[s]] || []).push(svcs.filter(function (x) { return x.name === s; })[0]); });
        var guarded = pols.length > 0;
        workloads.forEach(function (x, i) {
          var key = x.kind + "/" + x.name, meta = KIND[x.kind], my = pods.filter(function (p) { return podOwner[p.name] === key; });
          var parts = String(x.ready || "0/0").split("/"), ready = Number(parts[0]) || 0, want = Number(parts[1]) || x.replicas || 0;
          var restarts = my.reduce(function (s, p) { return s + (Number(p.restarts) || 0); }, 0);
          var bad = my.filter(function (p) { return p.status && !/^(Running|Completed|Succeeded)$/.test(p.status); });
          var tone = want && ready === 0 ? "bad" : bad.length || ready < want ? "warn" : "ok";
          var ss = svcByOwner[key] || [], port = ss.map(function (s) { return (s.ports || []).map(function (p) { return ":" + p.port; }).join(" "); }).join(" ");
          var img = (x.images || [])[0] || "", tag = img.split("/").pop(); if (tag.length > 17) tag = tag.slice(0, 16) + "…";
          var hpa = hpas.filter(function (h) { var t = h.spec && h.spec.scaleTargetRef || h.scaleTargetRef || {}; return t.kind === x.kind && t.name === x.name; })[0];
          var tags = [{ t: tag, bg: "#f2f3f5" }];
          if (hpa) {
            var hs = hpa.spec || {}, cm = (hpa.status && hpa.status.currentMetrics || []).filter(function (m) { return m.resource && m.resource.name === "cpu"; })[0];
            var cpu = cm && cm.resource.current && cm.resource.current.averageUtilization;
            tags.push({ t: "HPA " + hs.minReplicas + "–" + hs.maxReplicas + (cpu != null ? " · " + cpu + "%" : ""), bg: "#eaf1ff", fg: "#2563eb" });
          }
          if (x.kind === "StatefulSet") tags.push({ t: "持久卷", bg: "#f3ecff", fg: "#7c3aed" });
          if (guarded) tags.push({ t: "网络策略", bg: "#e8f7ef", fg: "#0f8a5f" });
          if (restarts) tags.push({ t: "重启 " + restarts, bg: "#fdecec", fg: "#c42b1c" });
          var n = { id: key, title: x.name, sub: meta[0] + " · " + (ss.length ? ss.map(function (s) { return s.name; }).join(", ") : "无 Service"), icon: meta[1], color: meta[2], tone: tone,
            badge: TONE[tone][0], meta: [ready + "/" + want + " 副本", port || "不对外", my.length + " 容器组"], tags: tags.slice(0, 3),
            href: "#/cluster/" + KIND[x.kind][3] + "/" + ctx.cluster.id + "?ns=" + encodeURIComponent(ns), deps: annotationDeps(yamls[i]) };
          nodes.push(n); byKey[key] = n;
        });
        var svcNode = function (name) { return svcOwner[name] && byKey[svcOwner[name]] ? svcOwner[name] : null; };
        // 入口
        ings.forEach(function (ing) {
          var rules = ing.rules || ing.hosts || [], hosts = (ing.hosts || rules.map(function (r) { return r.host; })).filter(Boolean);
          var id = "ing/" + ing.name, back = [];
          (ing.rules || []).forEach(function (rule) { (rule.paths || (rule.http && rule.http.paths) || []).forEach(function (p) { var s = p.service || p.backend && (p.backend.service ? p.backend.service.name : p.backend.serviceName); if (s) back.push(s); }); });
          (ing.backends || []).forEach(function (b) { if (b.service || b.name) back.push(b.service || b.name); });
          if (ing.default_backend) back.push(ing.default_backend.service || ing.default_backend);
          nodes.push({ id: id, title: ing.name, sub: "Ingress · " + (hosts.length ? hosts.slice().sort()[hosts.length - 1] + (hosts.length > 1 ? " 等" : "") : "无域名"), icon: "ingress", color: "#2563eb", tone: "info", badge: ing.class_name || ing.ingress_class || "入口", badgeTone: "info",
            meta: [(ing.tls && ing.tls.length ? "HTTPS" : "HTTP"), hosts.length + " 个域名", (ing.address || ing.addresses || "未分配地址").toString()], layer: 0, href: "#/traffic/expose" });
          back.forEach(function (s) { var t = svcNode(s); if (t) edges.push({ from: id, to: t, tone: byKey[t].tone === "bad" ? "bad" : "" }); });
        });
        svcs.filter(function (s) { return s.type === "NodePort" || s.type === "LoadBalancer"; }).forEach(function (s) {
          var t = svcNode(s.name); if (!t) return;
          var id = "svc/" + s.name;
          nodes.push({ id: id, title: s.name, sub: s.type + " · Service", icon: "ingress", color: "#2563eb", tone: "info", badge: "外部访问", badgeTone: "info",
            meta: (s.ports || []).map(function (p) { return (p.nodePort || p.node_port ? "节点端口 " + (p.nodePort || p.node_port) : ":" + p.port); }), layer: 0 });
          edges.push({ from: id, to: t });
        });
        // 声明式依赖：annotation xingshu.io/depends-on，值可为 Service 名或工作负载名
        nodes.forEach(function (n) {
          (n.deps || []).forEach(function (d) {
            var t = svcNode(d) || (byKey["Deployment/" + d] ? "Deployment/" + d : byKey["StatefulSet/" + d] ? "StatefulSet/" + d : null);
            if (t && t !== n.id) edges.push({ from: n.id, to: t, dash: true, tone: byKey[t].tone === "bad" ? "bad" : "" });
          });
        });
        // 观测到的调用：trace 边（服务名 = Service 名）
        var traceCount = 0;
        if (traces && traces.edges) traces.edges.forEach(function (e) {
          var a = svcNode(e.from), b = svcNode(e.to); if (!a || !b || a === b) return;
          traceCount++;
          edges = edges.filter(function (x) { return !(x.from === a && x.to === b); });
          edges.push({ from: a, to: b, label: e.count + " 次 · " + e.errorPct + "% 错", tone: e.errorPct >= 1 ? "bad" : "" });
        });
        var unhealthy = nodes.filter(function (n) { return n.tone === "bad" || n.tone === "warn"; }).length;
        var sum = workloads.length + " 个组件 · " + ings.length + " 个入口 · " + edges.length + " 条连线" + (unhealthy ? " · " + unhealthy + " 个异常" : "") + (traces && traces.source ? " · 链路数据：" + (traces.source.ds && traces.source.ds.name || "已接入") : " · 未接入链路追踪");
        page.draw(ctx, '<div class="xt-bar"><select class="x-select" data-xt-ns aria-label="命名空间">' + nss.map(function (n) { return "<option" + (n === ns ? " selected" : "") + ">" + esc(n) + "</option>"; }).join("") +
          '</select><span class="xt-sum">' + esc(sum) + '</span><span class="xt-sp"></span><button class="x-btn" type="button" data-xt-reload>刷新</button></div><div data-xt-host></div>');
        render(el.querySelector("[data-xt-host]"), { nodes: nodes, edges: edges }, {
          layout: "dag", empty: "该命名空间没有工作负载",
          legend: '<span><i></i>入口 / 观测调用</span><span><i class="dash"></i>声明依赖</span><span><i class="bad"></i>指向异常组件</span>'
        });
        el.querySelector("[data-xt-ns]").onchange = function (e) { APP.ns = e.target.value; appRender(el, page, ctx); };
        el.querySelector("[data-xt-reload]").onclick = function () { appRender(el, page, ctx); };
      });
  }
  root.XSAppTopology = { render: appRender };

  /* ---------- 资源拓扑（集群视角） ---------- */
  var ENV_CN = { prod: "生产", production: "生产", staging: "预发", pre: "预发", test: "测试", qa: "测试", dev: "开发", development: "开发" };
  var DS_ICON = { prometheus: "prometheus", loki: "loki", alertmanager: "alertmanager", jaeger: "trace", tempo: "trace", elasticsearch: "loki" };
  function clusterTone(c) {
    if (c.connect_mode === "agent") return c.agent_online ? "ok" : "bad";
    return { ok: "ok", healthy: "ok", ready: "ok", connected: "ok", error: "bad", failed: "bad", unreachable: "bad", offline: "bad" }[String(c.status || "").toLowerCase()] || "idle";
  }
  function resourceRender(host, clusters, platformName) {
    css();
    host.innerHTML = '<div class="xt-empty">正在读取集群与集成…</div>';
    var ids = clusters.map(function (c) { return c.id; });
    return Promise.all([
      soft(get("/observability/datasources"), { items: [] }), soft(get("/cicd/jenkins"), { items: [] }), soft(get("/cicd/argocd"), { items: [] })
    ].concat(ids.map(function (id) { return soft(get("/clusters/" + id + "/nodes", { page_size: 500 }), null); }))
      .concat(ids.map(function (id) { return soft(get("/clusters/" + id + "/components"), null); }))
      .concat(ids.map(function (id) { return soft(get("/clusters/" + id + "/config/secrets", { page_size: 500 }), null); })))
      .then(function (r) {
        var ds = items(r[0]), jenkins = items(r[1]), argo = items(r[2]), n = ids.length;
        var nodesR = r.slice(3, 3 + n), compR = r.slice(3 + n, 3 + 2 * n), secR = r.slice(3 + 2 * n);
        var nodes = [], edges = [], envs = {};
        nodes.push({ id: "root", title: platformName || "星枢 Matrix", sub: "平台", icon: "platform", color: "#2563eb", tone: "info", badge: clusters.length + " 集群", badgeTone: "ok",
          meta: [Object.keys(clusters.reduce(function (m, c) { m[c.env || "未标记"] = 1; return m; }, {})).length + " 个环境", clusters.length + " 集群", ds.length + " 数据源"] });
        clusters.forEach(function (c) { var e = c.env || "未标记"; (envs[e] = envs[e] || []).push(c); });
        Object.keys(envs).sort().forEach(function (e) {
          var id = "env/" + e, list = envs[e], bad = list.some(function (c) { return clusterTone(c) === "bad"; });
          nodes.push({ id: id, title: ENV_CN[e.toLowerCase()] ? ENV_CN[e.toLowerCase()] + "环境" : e, sub: list.length + " 个集群", icon: "env", color: "#0f9f7a", tone: "env", badge: "环境", meta: [e, list.length + " 集群"] });
          edges.push({ from: "root", to: id, tone: bad ? "bad" : "" });
          list.forEach(function (c) {
            var i = ids.indexOf(c.id), cid = "cl/" + c.id, tone = clusterTone(c);
            var nodeList = nodesR[i] ? items(nodesR[i]) : null;
            var readyNodes = nodeList ? nodeList.filter(function (x) { return x.ready === true || x.status === "Ready" || x.ready === "True"; }).length : null;
            var comps = compR[i] ? items(compR[i]).filter(function (x) { return x.state && x.state !== "missing"; }) : [];
            var regs = secR[i] ? items(secR[i]).filter(function (s) { return /dockerconfigjson|dockercfg/.test(s.type || ""); }) : [];
            var cds = ds.filter(function (d) { return String(d.cluster_id || "") === String(c.id) || (d.cluster_ids || []).map(String).indexOf(String(c.id)) >= 0; });
            var tags = [];
            if (cds.length) tags.push({ t: "数据源 " + cds.length, bg: "#fff1e6", fg: "#c2410c" });
            if (comps.length) tags.push({ t: "组件 " + comps.length, bg: "#eaf1ff", fg: "#2563eb" });
            if (regs.length) tags.push({ t: "镜像仓库 " + regs.length, bg: "#e7f6f8", fg: "#0e7490" });
            nodes.push({ id: cid, title: c.name, sub: c.connect_mode === "agent" ? "Agent 接入" : (c.api_server || "kubeconfig"), icon: "k8s", color: "#326ce5", tone: tone,
              badge: { ok: "正常", bad: "异常", idle: "未检测", warn: "告警" }[tone], meta: [c.version || "版本未知", nodeList ? (readyNodes + "/" + nodeList.length + " 节点就绪") : "节点未知", c.env || "未标记"],
              tags: tags, href: "#/cluster/list/" + encodeURIComponent(c.name), hint: c.last_error || c.name });
            edges.push({ from: id, to: cid, tone: tone === "bad" ? "bad" : "" });
            cds.forEach(function (d) {
              var did = "ds/" + d.id + "/" + c.id;
              nodes.push({ id: did, title: d.name, sub: (d.type || "").toUpperCase(), icon: DS_ICON[d.type] || "component", color: "#e6522c", tone: d.last_error ? "bad" : "info", badge: d.type, badgeTone: "info", meta: [String(d.url || "").replace(/^https?:\/\//, "").slice(0, 40)], href: "#/cluster/integrations" });
              edges.push({ from: cid, to: did, tone: d.last_error ? "bad" : "" });
            });
            comps.slice(0, 6).forEach(function (x) {
              var xid = "cp/" + c.id + "/" + (x.key || x.component || x.name);
              var st = String(x.state || "").toLowerCase(), t = /fail|error|degrad|unhealthy/.test(st) ? "bad" : st === "registered" || x.error ? "warn" : "ok";
              var w0 = (x.items || []).filter(function (it) { return it.source !== "api"; })[0] || (x.items || [])[0] || {};
              nodes.push({ id: xid, title: x.name || x.key, sub: "组件 · " + (w0.namespace || x.namespace || "集群级"), icon: DS_ICON[x.key] || "component", color: "#475569", tone: t,
                badge: { ok: "运行中", warn: "仅注册 API", bad: "异常" }[t], badgeTone: t, meta: [x.version || w0.version || "", (x.items || []).length + " 个资源"], href: "#/cluster/components", hint: x.error || "" });
              edges.push({ from: cid, to: xid, tone: t === "bad" ? "bad" : "" });
            });
            regs.slice(0, 4).forEach(function (s) {
              var rid = "rg/" + c.id + "/" + s.namespace + "/" + s.name;
              nodes.push({ id: rid, title: s.name, sub: "镜像仓库凭证 · " + s.namespace, icon: "registry", color: "#0891b2", tone: "info", badge: "Registry", badgeTone: "info", meta: [s.namespace], href: "#/cluster/registries" });
              edges.push({ from: cid, to: rid });
            });
          });
        });
        if (jenkins.length || argo.length) {
          nodes.push({ id: "cicd", title: "CI/CD 集成", sub: "平台级", icon: "component", color: "#d33833", tone: "info", badge: (jenkins.length + argo.length) + " 个", badgeTone: "info", meta: [jenkins.length + " Jenkins", argo.length + " Argo CD"] });
          edges.push({ from: "root", to: "cicd" });
          jenkins.forEach(function (j) { nodes.push({ id: "jk/" + j.id, title: j.name, sub: String(j.url || "").replace(/^https?:\/\//, ""), icon: "jenkins", color: "#d33833", tone: "info", badge: "Jenkins", badgeTone: "info", meta: [j.username || "", j.env || ""], href: "#/cicd/pipeline" }); edges.push({ from: "cicd", to: "jk/" + j.id }); });
          argo.forEach(function (a) { nodes.push({ id: "ar/" + a.id, title: a.name, sub: String(a.url || a.server || "").replace(/^https?:\/\//, ""), icon: "argocd", color: "#ef7b4d", tone: "info", badge: "Argo CD", badgeTone: "info", meta: [a.env || ""], href: "#/cicd/pipeline" }); edges.push({ from: "cicd", to: "ar/" + a.id }); });
        }
        render(host, { nodes: nodes, edges: edges }, { layout: "tree", empty: "尚未接入集群", legend: '<span><i></i>归属关系</span><span><i class="bad"></i>链路上有异常</span>' });
      });
  }
  root.XSResourceTopology = { render: resourceRender };
})(window);
