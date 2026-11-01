(function (root) {
  "use strict";
  var mounts = new WeakMap();
  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function items(data) {
    if (!data || !Array.isArray(data.items)) throw new Error("接口列表格式无效");
    return data.items;
  }
  function value(v) { return esc(v == null || v === "" ? "—" : v); }
  function failure(err) { return err && Number(err.status) === 403 ? "无权限读取 Helm Secret / release" : "读取 Helm 应用失败：" + (err.message || "接口不可用"); }
  function current(s) {
    return mounts.get(s.el) === s && !s.controller.signal.aborted &&
      s.context.isCurrent(s.snapshot) && s.el.isConnected !== false;
  }
  // Keep endpoint contracts in this helper for the route integration owner.
  function api(s, release) {
    var path = "/clusters/" + encodeURIComponent(s.snapshot.cluster_id) + "/helm/releases";
    if (release) path += "/" + encodeURIComponent(release.namespace) + "/" + encodeURIComponent(release.name || release.release) + "/history";
    return Promise.resolve().then(function () {
      if (!current(s)) throw new Error("集群上下文已变化");
      return root.XS.api(path, { scope: "cluster", signal: s.controller.signal, query: { cluster_id: s.snapshot.cluster_id } });
    }).then(function (data) {
      if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error("响应集群不匹配");
      return data;
    });
  }
  function warning(data) {
    return data.truncated ? '<p class="xh-warning" role="status">结果已截断，当前列表和历史数量可能不完整。</p>' : "";
  }
  function errors(row) {
    var bad = Array.isArray(row.corrupt_revisions) ? row.corrupt_revisions : [];
    return (row.parse_error ? '<span class="xh-error">' + esc(row.parse_error) + '</span>' : "") +
      bad.filter(function (r) { return r.parse_error && !(r.revision === row.revision && r.parse_error === row.parse_error); }).map(function (r) {
        return '<span class="xh-error">revision ' + value(r.revision) + ' · ' + esc(r.parse_error) + '</span>';
      }).join("");
  }
  function table(s) {
    var q = s.search.toLowerCase();
    var visible = s.rows.map(function (r, i) { return { row: r, index: i }; }).filter(function (entry) {
      var r = entry.row;
      return [r.namespace, r.name || r.release, r.chart, r.chart_version, r.status].join(" ").toLowerCase().indexOf(q) >= 0;
    });
    var target = s.el.querySelector("[data-xh-list]");
    if (!target) return;
    target.innerHTML = warning(s.data) + (visible.length ? '<div class="xh-scroll"><table class="x-table xh-table"><thead><tr>' +
      '<th>Namespace</th><th>Release</th><th>Chart</th><th>Chart 版本</th><th>状态</th><th>更新时间</th><th>历史</th><th>操作</th></tr></thead><tbody>' +
      visible.map(function (entry) {
        var r = entry.row;
        return '<tr><td>' + value(r.namespace) + '</td><td><strong>' + value(r.name || r.release) + '</strong></td><td>' + value(r.chart) + '</td><td>' +
          value(r.chart_version) + '</td><td>' + value(r.status) + errors(r) + '</td><td>' + value(r.updated) + '</td><td>' +
          value(r.history_count) + (r.history_complete === false ? '（不完整）' : '') + '</td><td><button class="x-btn" type="button" data-xh-view="' +
          entry.index + '">详情</button></td></tr>';
      }).join("") + '</tbody></table></div>' : '<p class="xh-state">' + (s.rows.length ? '没有匹配的 release' : '未发现 Helm release') + '</p>');
    var count = s.el.querySelector("[data-xh-count]");
    if (count) count.textContent = String(s.rows.length) + " releases";
  }
  function history(s, index) {
    var release = s.rows[index];
    if (!release) return Promise.resolve();
    var sequence = ++s.detailRevision;
    var target = s.el.querySelector("[data-xh-detail]");
    target.hidden = false;
    target.innerHTML = '<h3 tabindex="-1">' + esc(release.namespace + "/" + (release.name || release.release)) + '</h3>' +
      '<button class="x-btn" type="button" data-xh-close aria-label="关闭 release 详情">关闭</button><p role="status">正在读取 revision 历史…</p>';
    var heading = target.querySelector("h3");
    if (heading) heading.focus();
    return api(s, release).then(function (data) {
      if (!current(s) || sequence !== s.detailRevision) return;
      var rows = items(data).slice().sort(function (a, b) { return Number(b.revision) - Number(a.revision); });
      target.innerHTML = '<div class="xh-toolbar"><h3>' + esc(release.namespace + "/" + (release.name || release.release)) + '</h3>' +
        '<button class="x-btn" type="button" data-xh-close aria-label="关闭 release 详情">关闭</button></div>' + warning(data) +
        (rows.length ? '<ol class="xh-timeline">' + rows.map(function (r) {
          return '<li><strong>revision ' + value(r.revision) + '</strong><time>' + value(r.updated) + '</time><span>' + value(r.status) +
            '</span><span>' + value(r.chart) + ' / ' + value(r.chart_version) + '</span>' + errors(r) + '</li>';
        }).join("") + '</ol>' : '<p class="xh-state">未发现 revision 历史</p>');
    }).catch(function (err) {
      if (current(s) && sequence === s.detailRevision) target.innerHTML = '<button class="x-btn" type="button" data-xh-close>关闭</button><p class="xh-error" role="alert">读取历史失败：' + esc(err.message) + '</p>';
    });
  }
  function destroy(el) {
    var s = mounts.get(el);
    if (!s) return;
    mounts.delete(el);
    s.controller.abort();
    if (s.unsubscribe) s.unsubscribe();
    el.removeEventListener("click", s.click);
    el.removeEventListener("input", s.input);
    el.innerHTML = "";
  }
  function render(el, ctx) {
    destroy(el);
    var context = root.XSClusterContext;
    if (!context || !root.XS || typeof root.XS.api !== "function") {
      el.innerHTML = '<p class="xh-error" role="alert">集群接口尚未就绪</p>';
      return Promise.resolve();
    }
    var snapshot = context.get();
    var s = { el: el, ctx: ctx, context: context, snapshot: snapshot, controller: new AbortController(), rows: [], data: {}, search: "", detailRevision: 0 };
    mounts.set(el, s);
    s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (snapshot.kind !== "cluster" || !snapshot.cluster_id) {
      el.innerHTML = '<p class="xh-state">请选择单个集群</p>';
      return Promise.resolve();
    }
    el.innerHTML = '<section class="xh-root" aria-label="Helm 应用"><div class="xh-toolbar"><h2>Helm 应用</h2><span class="x-frac" data-xh-count></span>' +
      '<input class="x-input" type="search" data-xh-search aria-label="搜索 Helm release" placeholder="Namespace / Release / Chart">' +
      '<button class="x-btn" type="button" data-xh-refresh aria-label="刷新 Helm 应用">刷新</button></div>' +
      '<div data-xh-list aria-live="polite"><p class="xh-state" role="status">正在读取 Helm releases…</p></div><section class="xh-detail" data-xh-detail aria-label="Release revision 历史" hidden></section></section>';
    s.click = function (event) {
      var button = event.target.closest("button");
      if (!button || !el.contains(button) || !current(s)) return;
      if (button.hasAttribute("data-xh-refresh")) return render(el, ctx);
      if (button.hasAttribute("data-xh-view")) return history(s, Number(button.getAttribute("data-xh-view")));
      if (button.hasAttribute("data-xh-close")) {
        s.detailRevision++;
        el.querySelector("[data-xh-detail]").hidden = true;
        var trigger = el.querySelector("[data-xh-view]");
        if (trigger) trigger.focus();
      }
    };
    s.input = function (event) { if (event.target.hasAttribute("data-xh-search") && current(s)) { s.search = event.target.value; table(s); } };
    el.addEventListener("click", s.click);
    el.addEventListener("input", s.input);
    return api(s).then(function (data) {
      if (!current(s)) return;
      s.rows = items(data); s.data = data; table(s);
    }).catch(function (err) {
      if (current(s)) el.querySelector("[data-xh-list]").innerHTML = '<p class="xh-error" role="alert">' + esc(failure(err)) + '</p>';
    });
  }
  root.XSHelmApps = { render: render, destroy: destroy };
})(window);
