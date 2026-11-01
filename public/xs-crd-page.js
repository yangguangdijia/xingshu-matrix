(function (root) {
  "use strict";
  var mounts = new WeakMap();
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function val(v) { return esc(v == null || v === "" ? "—" : v); }
  function list(v) { return Array.isArray(v) ? v : []; }
  function normalize(r) {
    var spec = r.spec || r, names = spec.names || {}, status = r.status || {};
    return { name: r.name || r.metadata && r.metadata.name, group: spec.group, kind: r.kind && r.kind !== "CustomResourceDefinition" ? r.kind : names.kind,
      scope: spec.scope, plural: names.plural || r.plural, versions: list(spec.versions), conditions: list(r.conditions || status.conditions), raw: r };
  }
  function live(s) { return mounts.get(s.el) === s && !s.controller.signal.aborted && s.context.isCurrent(s.snapshot) && s.el.isConnected !== false; }
  function api(s, name) {
    return Promise.resolve().then(function () {
      if (!live(s)) throw new Error("集群上下文已变化");
      return root.XS.api("/clusters/" + encodeURIComponent(s.snapshot.cluster_id) + "/crds" + (name ? "/" + encodeURIComponent(name) : ""), { scope: "cluster", signal: s.controller.signal });
    }).then(function (data) {
      if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error("响应集群不匹配");
      return data;
    });
  }
  function conditions(rows) { return rows.length ? rows.map(function (c) { return '<div><strong>' + val(c.type) + ': ' + val(c.status) + '</strong>' + (c.reason ? ' · ' + esc(c.reason) : '') + (c.message ? '<small>' + esc(c.message) + '</small>' : '') + '</div>'; }).join("") : "未返回条件"; }
  function schemaHTML(schema) {
    var count = 0;
    function node(key, v, required, depth) {
      if (!v || typeof v !== "object") return '<li>' + esc(key) + '：未提供 Schema</li>';
      if (++count > 2000 || depth > 15) return '<li class="xc-warning">Schema 展示已截断</li>';
      var constraints = ["format", "minimum", "maximum", "minLength", "maxLength", "pattern", "minItems", "maxItems", "nullable", "default", "enum", "x-kubernetes-list-type", "x-kubernetes-preserve-unknown-fields"].filter(function (k) { return v[k] !== undefined; }).map(function (k) { return k + '=' + (typeof v[k] === "object" ? JSON.stringify(v[k]) : v[k]); });
      var children = Object.keys(v.properties || {}).map(function (k) { return node(k, v.properties[k], list(v.required).indexOf(k) >= 0, depth + 1); });
      if (v.items) children.push(node("items[]", v.items, false, depth + 1));
      if (v.additionalProperties) children.push(node("additionalProperties", v.additionalProperties === true ? { description: "允许任意属性" } : v.additionalProperties, false, depth + 1));
      ["oneOf", "anyOf", "allOf"].forEach(function (k) { list(v[k]).forEach(function (child, i) { children.push(node(k + '[' + i + ']', child, false, depth + 1)); }); });
      var label = '<code>' + esc(key) + '</code> <span>' + val(v.type || (v.properties ? "object" : "未声明类型")) + (required ? ' · 必填' : '') + '</span>';
      var body = (v.description ? '<p>' + esc(v.description) + '</p>' : '') + (constraints.length ? '<small>' + esc(constraints.join(" · ")) + '</small>' : '');
      return '<li>' + (children.length ? '<details' + (depth < 2 ? ' open' : '') + '><summary>' + label + '</summary>' + body + '<ul>' + children.join("") + '</ul></details>' : label + body) + '</li>';
    }
    return '<ul class="xc-schema">' + node("root", schema, false, 0) + '</ul>';
  }
  function draw(s) {
    var visible = s.rows.map(function (row, index) { return { row: row, index: index }; }).filter(function (e) { return [e.row.name, e.row.group, e.row.kind, e.row.scope].join(" ").toLowerCase().includes(s.search.toLowerCase()); });
    s.el.querySelector("[data-xc-list]").innerHTML = (s.truncated ? '<p class="xc-warning">CRD 列表已截断</p>' : '') + (visible.length ? '<div class="xc-scroll"><table class="x-table xc-table"><thead><tr><th>名称 / Group</th><th>Kind</th><th>Scope</th><th>版本</th><th>条件</th><th>操作</th></tr></thead><tbody>' + visible.map(function (e) {
      var r = e.row;
      return '<tr><td><strong>' + val(r.name) + '</strong><small>' + val(r.group) + '</small></td><td>' + val(r.kind) + '</td><td>' + val(r.scope) + '</td><td>' + r.versions.map(function (v) { return '<div>' + val(typeof v === "string" ? v : v.name) + (v.served === true ? ' · served' : '') + (v.storage === true ? ' · storage' : '') + '</div>'; }).join("") + '</td><td>' + conditions(r.conditions) + '</td><td><button class="x-btn" data-xc-detail="' + e.index + '" type="button">Schema 详情</button></td></tr>';
    }).join("") + '</tbody></table></div>' : '<p class="xc-state">' + (s.rows.length ? '没有匹配的 CRD' : '未发现 CRD') + '</p>');
  }
  function detail(s, index) {
    var row = s.rows[index]; if (!row) return;
    var seq = ++s.detailSeq, target = s.el.querySelector("[data-xc-panel]");
    target.hidden = false; target.innerHTML = '<p role="status">正在读取 CRD Schema…</p>';
    return api(s, row.name).then(function (data) {
      if (!live(s) || seq !== s.detailSeq) return;
      var raw = data.item || data.object || data.crd || data, r = normalize(raw);
      target.innerHTML = '<div class="xc-toolbar"><h3 tabindex="-1">' + val(r.name || row.name) + '</h3><button class="x-btn" type="button" data-xc-close>关闭</button></div><dl class="xc-definition"><dt>Group / Kind</dt><dd>' + val(r.group) + ' / ' + val(r.kind) + '</dd><dt>Scope / Plural</dt><dd>' + val(r.scope) + ' / ' + val(r.plural) + '</dd><dt>Conditions</dt><dd>' + conditions(r.conditions) + '</dd></dl>' +
        (raw.schema_projection ? '<p class="xc-warning">Schema 为后端结构投影，敏感默认值、示例及校验表达式可能省略。</p>' : '') +
        (r.versions.length ? r.versions.map(function (v) { return '<section class="xc-version"><h4>' + val(v.name || v) + (v.served === true ? ' · served' : '') + (v.storage === true ? ' · storage' : '') + (v.deprecated === true ? ' · deprecated' : '') + '</h4>' + (v.schema_truncated ? '<p class="xc-warning">该版本 Schema 已截断</p>' : '') + (v.schema ? schemaHTML(v.schema.openAPIV3Schema || v.schema) : '<p class="xc-state">该版本未提供 Schema</p>') + '</section>'; }).join("") : '<p class="xc-state">未返回版本 Schema</p>');
      var h = target.querySelector("h3"); if (h) h.focus();
    }).catch(function (err) { if (live(s) && seq === s.detailSeq) target.innerHTML = '<button class="x-btn" type="button" data-xc-close>关闭</button><p class="xc-error" role="alert">' + esc(err.status === 403 ? "无权限读取 CRD 详情" : "读取 Schema 失败：" + err.message) + '</p>'; });
  }
  function destroy(el) { var s = mounts.get(el); if (!s) return; mounts.delete(el); s.controller.abort(); if (s.unsubscribe) s.unsubscribe(); el.removeEventListener("click", s.click); el.removeEventListener("input", s.input); el.innerHTML = ""; }
  function render(el, ctx) {
    destroy(el); var context = root.XSClusterContext;
    if (!context || !root.XS || !root.XS.api) { el.innerHTML = '<p class="xc-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var s = { el: el, context: context, snapshot: context.get(), controller: new AbortController(), rows: [], search: "", detailSeq: 0 };
    mounts.set(el, s); s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (s.snapshot.kind !== "cluster" || !s.snapshot.cluster_id) { el.innerHTML = '<p class="xc-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xc-root" aria-label="CRD"><div class="xc-toolbar"><h2>CRD</h2><input class="x-input" type="search" data-xc-search aria-label="搜索 CRD" placeholder="名称 / Group / Kind"><button class="x-btn" type="button" data-xc-refresh>刷新</button></div><div data-xc-list aria-live="polite"><p class="xc-state" role="status">正在读取 CRD…</p></div><section class="xc-detail" data-xc-panel hidden></section></section>';
    s.click = function (event) { var b = event.target.closest("button"); if (!b || !el.contains(b) || !live(s)) return; if (b.hasAttribute("data-xc-refresh")) return render(el, ctx); if (b.hasAttribute("data-xc-detail")) return detail(s, Number(b.getAttribute("data-xc-detail"))); if (b.hasAttribute("data-xc-close")) { ++s.detailSeq; el.querySelector("[data-xc-panel]").hidden = true; var trigger = el.querySelector("[data-xc-detail]"); if (trigger) trigger.focus(); } };
    s.input = function (event) { if (live(s) && event.target.hasAttribute("data-xc-search")) { s.search = event.target.value; draw(s); } };
    el.addEventListener("click", s.click); el.addEventListener("input", s.input);
    return api(s).then(function (data) { if (!live(s)) return; if (!data || !Array.isArray(data.items)) throw new Error("CRD 列表格式无效"); s.rows = data.items.map(normalize); s.truncated = data.truncated; draw(s); }).catch(function (err) { if (live(s)) el.querySelector("[data-xc-list]").innerHTML = '<p class="xc-error" role="alert">' + esc(err.status === 403 ? "无权限读取 CRD" : "读取 CRD 失败：" + err.message) + '</p>'; });
  }
  root.XSCRDPage = { render: render, destroy: destroy, _internals: { normalize: normalize, schemaHTML: schemaHTML } };
})(window);
