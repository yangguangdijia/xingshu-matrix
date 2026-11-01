(function (root) {
  "use strict";
  var mounts = new WeakMap();
  var HELM = "/cicd/" + "helm";

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function value(v) { return esc(v == null || v === "" ? "—" : v); }
  function list(data, label) {
    if (Array.isArray(data)) return data;
    if (data && Array.isArray(data.items)) return data.items;
    throw new Error((label || "接口") + "列表格式无效");
  }
  function canManage() { return !!(root.XS && root.XS.can && root.XS.can("cicd:manage")); }
  function current(s) {
    return mounts.get(s.el) === s && !s.controller.signal.aborted &&
      s.context.isCurrent(s.snapshot) && s.el.isConnected !== false;
  }
  function pending(v) {
    return !!(v && (Number(v.code) === 40304 || v.approval_required === true || v.approval ||
      /^(pending|pending_approval|awaiting_approval)$/.test(v.status || v.state) ||
      root.XS.isApproval && root.XS.isApproval(v)));
  }
  function ticket(v) {
    var d = v && (v.details || v.approval || v) || {};
    return d.ticket_id || d.approval_id || d.ticket && d.ticket.id || "";
  }
  function clusterApi(s, release) {
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
  function marketApi(s, path, options) {
    return Promise.resolve().then(function () {
      if (!current(s)) throw new Error("集群上下文已变化");
      return root.XS.api(HELM + path, Object.assign({ scope: "platform", signal: s.controller.signal }, options || {}));
    });
  }
  function warning(data) {
    return data && data.truncated ? '<p class="xh-warning" role="status">结果已截断，当前列表和历史数量可能不完整。</p>' : "";
  }
  function errors(row) {
    var bad = Array.isArray(row.corrupt_revisions) ? row.corrupt_revisions : [];
    return (row.parse_error ? '<span class="xh-error">' + esc(row.parse_error) + '</span>' : "") +
      bad.filter(function (r) { return r.parse_error && !(r.revision === row.revision && r.parse_error === row.parse_error); }).map(function (r) {
        return '<span class="xh-error">revision ' + value(r.revision) + ' · ' + esc(r.parse_error) + '</span>';
      }).join("");
  }
  function installedFailure(err) {
    return err && Number(err.status) === 403 ? "无权限读取 Helm Secret / release" : "读取 Helm 应用失败：" + (err.message || "接口不可用");
  }
  function marketFailure(label, err) {
    return (err && Number(err.status) === 403 ? "无权限读取" : "读取失败") + " " + label + "：" + (err && err.message || "接口不可用");
  }
  function drawInstalled(s) {
    var q = s.installedSearch.toLowerCase();
    var visible = s.rows.map(function (r, i) { return { row: r, index: i }; }).filter(function (entry) {
      var r = entry.row;
      return [r.namespace, r.name || r.release, r.chart, r.chart_version, r.status].join(" ").toLowerCase().indexOf(q) >= 0;
    });
    var target = s.el.querySelector("[data-xh-list]");
    if (!target) return;
    target.innerHTML = warning(s.releaseData) + (visible.length ? '<div class="xh-scroll"><table class="x-table xh-table"><thead><tr>' +
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
  function loadInstalled(s) {
    var revision = ++s.installedRevision;
    var target = s.el.querySelector("[data-xh-list]");
    if (target) target.innerHTML = '<p class="xh-state" role="status">正在读取 Helm releases…</p>';
    return clusterApi(s).then(function (data) {
      if (!current(s) || s.tab !== "installed" || revision !== s.installedRevision) return;
      s.rows = list(data, "Helm release"); s.releaseData = data; drawInstalled(s);
    }).catch(function (err) {
      if (current(s) && s.tab === "installed" && revision === s.installedRevision && target) target.innerHTML = '<p class="xh-error" role="alert">' + esc(installedFailure(err)) + '</p>';
    });
  }
  function history(s, index) {
    var release = s.rows[index];
    if (!release || s.tab !== "installed") return Promise.resolve();
    var revision = ++s.detailRevision;
    s.detailIndex = index;
    var target = s.el.querySelector("[data-xh-detail]");
    target.hidden = false;
    target.innerHTML = '<h3 tabindex="-1">' + esc(release.namespace + "/" + (release.name || release.release)) + '</h3>' +
      '<button class="x-btn" type="button" data-xh-close>关闭</button><p role="status">正在读取 revision 历史…</p>';
    var heading = target.querySelector("h3"); if (heading) heading.focus();
    return clusterApi(s, release).then(function (data) {
      if (!current(s) || s.tab !== "installed" || revision !== s.detailRevision) return;
      var rows = list(data, "Helm revision").slice().sort(function (a, b) { return Number(b.revision) - Number(a.revision); });
      var doc = root.document, active = doc && doc.activeElement, keepFocus = !active || active === doc.body || target.contains(active);
      target.innerHTML = '<div class="xh-toolbar"><h3 tabindex="-1">' + esc(release.namespace + "/" + (release.name || release.release)) + '</h3><button class="x-btn" type="button" data-xh-close>关闭</button></div>' + warning(data) +
        (rows.length ? '<ol class="xh-timeline">' + rows.map(function (r) {
          return '<li><strong>revision ' + value(r.revision) + '</strong><time>' + value(r.updated) + '</time><span>' + value(r.status) + '</span><span>' + value(r.chart) + ' / ' + value(r.chart_version) + '</span>' + errors(r) + '</li>';
        }).join("") + '</ol>' : '<p class="xh-state">未发现 revision 历史</p>');
      var loadedHeading = keepFocus && target.querySelector("h3"); if (loadedHeading) loadedHeading.focus();
    }).catch(function (err) {
      if (current(s) && s.tab === "installed" && revision === s.detailRevision) target.innerHTML = '<button class="x-btn" type="button" data-xh-close>关闭</button><p class="xh-error" role="alert">读取历史失败：' + esc(err.message) + '</p>';
    });
  }
  function repoURL(s, chart) {
    if (chart.repo_url) return chart.repo_url;
    var repo = s.repos.find(function (r) { return String(r.id) === String(chart.repo_id); });
    return repo && repo.url || "";
  }
  function chartRoute(s, row) {
    var chart = row.chart || row, url = repoURL(s, row);
    return "#/delivery/apps?helm_chart=" + encodeURIComponent(chart.name || "") +
      "&helm_repo=" + encodeURIComponent(url) + "&helm_version=" + encodeURIComponent(chart.version || "") +
      "&helm_env=" + encodeURIComponent(row.env || "");
  }
  function drawRepos(s) {
    var target = s.el.querySelector("[data-xh-repos]"); if (!target) return;
    var manage = canManage() && !s.approvalLocked;
    target.innerHTML = s.repos.length ? '<div class="xh-scroll"><table class="x-table xh-table"><thead><tr><th>仓库</th><th>环境</th><th>HTTPS URL</th><th>同步状态</th><th>操作</th></tr></thead><tbody>' + s.repos.map(function (r, index) {
      return '<tr><td><strong>' + value(r.name) + '</strong></td><td>' + value(r.env) + '</td><td>' + value(r.url) + '</td><td>' + value(r.last_sync_status) + (r.last_sync_error ? '<small class="xh-error">' + esc(r.last_sync_error) + '</small>' : '') + '</td><td>' +
        '<button class="x-btn" type="button" data-xh-repo-edit="' + index + '"' + (manage ? '' : ' disabled') + '>编辑</button> ' +
        '<button class="x-btn" type="button" data-xh-repo-sync="' + index + '"' + (manage ? '' : ' disabled') + '>同步</button> ' +
        '<button class="x-btn" type="button" data-xh-repo-delete="' + index + '"' + (manage ? '' : ' disabled') + '>删除</button></td></tr>';
    }).join("") + '</tbody></table></div>' : '<p class="xh-state">尚未配置 Helm 仓库</p>';
  }
  function drawCharts(s) {
    var target = s.el.querySelector("[data-xh-charts]"); if (!target) return;
    var capability = s.chartMeta && s.chartMeta.install_supported === false
      ? '<p class="xh-state">目录 install_supported:false；此处不提供直接安装接口。</p>' : "";
    target.innerHTML = capability + (s.charts.length ? '<div class="xh-scroll"><table class="x-table xh-table"><thead><tr><th>仓库</th><th>环境</th><th>Chart</th><th>版本</th><th>应用版本</th><th>说明</th><th>操作</th></tr></thead><tbody>' + s.charts.map(function (row, index) {
      var chart = row.chart || row, usable = !!(chart.name && chart.version && row.env && repoURL(s, row));
      return '<tr><td>' + value(row.repo_name) + '</td><td>' + value(row.env) + '</td><td><strong>' + value(chart.name) + '</strong></td><td>' + value(chart.version) + '</td><td>' + value(chart.app_version) + '</td><td>' + value(chart.description) + '</td><td><button class="x-btn x-btn--pri" type="button" data-xh-use-chart="' + index + '"' + (usable ? '' : ' disabled') + '>用于新建应用</button></td></tr>';
    }).join("") + '</tbody></table></div>' : '<p class="xh-state">没有匹配的 Chart</p>');
    var count = s.el.querySelector("[data-xh-chart-count]"); if (count) count.textContent = String(s.charts.length) + " charts";
  }
  function loadRepos(s) {
    var revision = ++s.repoRevision, target = s.el.querySelector("[data-xh-repos]");
    if (target) target.innerHTML = '<p class="xh-state" role="status">正在读取 Helm 仓库…</p>';
    return marketApi(s, "/repos").then(function (data) {
      if (!current(s) || s.tab !== "catalog" || revision !== s.repoRevision) return false;
      s.repos = list(data, "Helm 仓库"); drawRepos(s); return true;
    }).catch(function (err) {
      if (current(s) && s.tab === "catalog" && revision === s.repoRevision && target) target.innerHTML = '<p class="xh-error" role="alert">' + esc(marketFailure("Helm 仓库", err)) + '</p>';
      return false;
    });
  }
  function loadCharts(s) {
    var revision = ++s.chartRevision, target = s.el.querySelector("[data-xh-charts]");
    if (target) target.innerHTML = '<p class="xh-state" role="status">正在搜索 Chart…</p>';
    return marketApi(s, "/charts", { query: { q: s.marketSearch } }).then(function (data) {
      if (!current(s) || s.tab !== "catalog" || revision !== s.chartRevision) return;
      s.charts = list(data, "Helm Chart"); s.chartMeta = data || {}; drawCharts(s);
    }).catch(function (err) {
      if (current(s) && s.tab === "catalog" && revision === s.chartRevision && target) target.innerHTML = '<p class="xh-error" role="alert">' + esc(marketFailure("Helm Chart", err)) + '</p>';
    });
  }
  function loadMarket(s) {
    return loadRepos(s).then(function () { if (current(s) && s.tab === "catalog") return loadCharts(s); });
  }
  function setStatus(s, message, error) {
    var n = s.el.querySelector("[data-xh-market-status]"); if (!n) return;
    n.textContent = message || ""; n.hidden = !message; n.className = error ? "xh-error" : "";
  }
  function setRepoWriteLocked(s, locked) {
    s.approvalLocked = !!locked;
    s.el.querySelectorAll("[data-xh-repo-new], [data-xh-repo-edit], [data-xh-repo-sync], [data-xh-repo-delete], [data-xh-repo-save], [data-xh-repo-delete-confirm]").forEach(function (button) {
      button.disabled = s.approvalLocked || !canManage();
    });
  }
  function approvalStatus(s, result) {
    setRepoWriteLocked(s, true);
    setStatus(s, "等待审批" + (ticket(result) ? " #" + ticket(result) : "") + "；本页写操作已锁定。审批后请刷新，以读取真实状态。", false);
  }
  function validRepo(form) {
    var name = form.elements.name.value.trim(), raw = form.elements.url.value.trim(), env = form.elements.env.value.trim(), parsed;
    if (!name || Array.from(name).length > 64) throw new Error("仓库名称必填且不能超过 64 个字符");
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(env)) throw new Error("环境只能使用字母、数字、点、横线或下划线");
    try { parsed = new URL(raw); } catch (_) { throw new Error("仓库 URL 无效"); }
    if (parsed.protocol !== "https:" || !parsed.hostname || parsed.username || parsed.password || parsed.search || parsed.hash) throw new Error("仓库必须使用不含凭据、查询或片段的 HTTPS URL");
    return { name: name, url: raw.replace(/\/+$/, ""), env: env };
  }
  function openRepoForm(s, repo) {
    if (s.approvalLocked) { setStatus(s, "本页写操作已锁定。审批后请刷新，以读取真实状态。", false); return; }
    if (!canManage()) { setStatus(s, "缺少 cicd:manage 权限，不能管理 Helm 仓库。", true); return; }
    var target = s.el.querySelector("[data-xh-repo-editor]"), editing = !!repo;
    s.repoEdit = { repo: repo || null };
    target.hidden = false;
    target.innerHTML = '<form data-xh-repo-form><h4>' + (editing ? '编辑 Helm 仓库' : '新增 Helm 仓库') + '</h4>' +
      '<label>名称<input class="x-input" name="name" maxlength="64" required value="' + esc(repo && repo.name || '') + '"></label>' +
      '<label>HTTPS index URL<input class="x-input" name="url" type="url" required value="' + esc(repo && repo.url || '') + '"></label>' +
      '<p class="xh-state">当前仅支持无认证 HTTPS index 仓库；URL 不能包含凭据、查询或片段。</p>' +
      '<label>环境<input class="x-input" name="env" maxlength="64" required value="' + esc(repo && repo.env || s.snapshot.env || '') + '"></label>' +
      '<button class="x-btn x-btn--pri" type="submit" data-xh-repo-save>' + (editing ? '保存' : '新增') + '</button> <button class="x-btn" type="button" data-xh-repo-cancel>取消</button></form>';
  }
  function confirmDelete(s, repo) {
    if (s.approvalLocked) { setStatus(s, "本页写操作已锁定。审批后请刷新，以读取真实状态。", false); return; }
    var target = s.el.querySelector("[data-xh-repo-editor]"); s.repoEdit = { deleteRepo: repo };
    target.hidden = false;
    target.innerHTML = '<p>确认删除 Helm 仓库 <strong>' + value(repo.name) + '</strong>？</p><button class="x-btn x-btn--pri" type="button" data-xh-repo-delete-confirm>确认删除</button> <button class="x-btn" type="button" data-xh-repo-cancel>取消</button>';
  }
  function writeRepo(s, path, method, body, success) {
    if (s.approvalLocked) { setStatus(s, "本页写操作已锁定。审批后请刷新，以读取真实状态。", false); return Promise.resolve(); }
    if (!canManage() || s.tab !== "catalog") { setStatus(s, "缺少 cicd:manage 权限，操作已停止。", true); return Promise.resolve(); }
    var revision = ++s.writeRevision; setStatus(s, "正在提交…", false);
    return marketApi(s, path, { method: method, body: body }).then(function (data) {
      if (!current(s) || s.tab !== "catalog" || revision !== s.writeRevision) return;
      if (pending(data)) { approvalStatus(s, data); return; }
      setStatus(s, success, false); s.el.querySelector("[data-xh-repo-editor]").hidden = true; s.repoEdit = null;
      return loadMarket(s);
    }).catch(function (err) {
      if (!current(s) || s.tab !== "catalog" || revision !== s.writeRevision) return;
      if (pending(err)) approvalStatus(s, err);
      else setStatus(s, "操作失败：" + (err.message || "接口不可用"), true);
    });
  }
  function submitRepo(s, form) {
    if (!canManage()) return writeRepo(s, "", "POST");
    var body;
    try { if (!form.reportValidity()) return Promise.resolve(); body = validRepo(form); }
    catch (err) { setStatus(s, err.message, true); return Promise.resolve(); }
    var repo = s.repoEdit && s.repoEdit.repo;
    return writeRepo(s, repo ? "/repos/" + encodeURIComponent(repo.id) : "/repos", repo ? "PUT" : "POST", body, repo ? "仓库已更新。" : "仓库已新增。");
  }
  function selectTab(s, tab) {
    if (!current(s) || (tab !== "installed" && tab !== "catalog")) return Promise.resolve();
    s.tab = tab; ++s.installedRevision; ++s.repoRevision; ++s.chartRevision; ++s.detailRevision; ++s.writeRevision;
    var installed = s.el.querySelector('[data-xh-content="installed"]'), catalog = s.el.querySelector('[data-xh-content="catalog"]');
    installed.hidden = tab !== "installed"; catalog.hidden = tab !== "catalog";
    s.el.querySelectorAll("[data-xh-tab]").forEach(function (b) { var on = b.getAttribute("data-xh-tab") === tab; b.setAttribute("aria-selected", on ? "true" : "false"); b.className = on ? "on" : ""; });
    s.el.querySelector("[data-xh-detail]").hidden = true;
    return tab === "installed" ? loadInstalled(s) : loadMarket(s);
  }
  function refresh(s) {
    ++s.detailRevision;
    var detail = s.el.querySelector("[data-xh-detail]"); if (detail) detail.hidden = true;
    if (s.tab === "catalog") {
      ++s.writeRevision;
      s.repoEdit = null;
      s.approvalLocked = false;
      var editor = s.el.querySelector("[data-xh-repo-editor]"); if (editor) editor.hidden = true;
      setStatus(s, "", false);
      setRepoWriteLocked(s, false);
    }
    return s.tab === "installed" ? loadInstalled(s) : loadMarket(s);
  }
  function destroy(el) {
    var s = mounts.get(el); if (!s) return;
    mounts.delete(el); s.controller.abort(); if (s.unsubscribe) s.unsubscribe();
    clearTimeout(s.searchTimer);
    ["click", "input", "submit"].forEach(function (type) { el.removeEventListener(type, s[type]); }); el.innerHTML = "";
  }
  function render(el, ctx) {
    destroy(el);
    var context = root.XSClusterContext;
    if (!context || !root.XS || typeof root.XS.api !== "function") { el.innerHTML = '<p class="xh-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var snapshot = context.get();
    var s = { el: el, ctx: ctx || {}, context: context, snapshot: snapshot, controller: new AbortController(), tab: "installed", rows: [], releaseData: {}, installedSearch: "", marketSearch: "", repos: [], charts: [], installedRevision: 0, repoRevision: 0, chartRevision: 0, detailRevision: 0, writeRevision: 0, approvalLocked: false };
    mounts.set(el, s); s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (snapshot.kind !== "cluster" || !snapshot.cluster_id) { el.innerHTML = '<p class="xh-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xh-root" aria-label="Helm 应用"><div class="xh-toolbar"><h2>Helm 应用</h2><span class="x-frac" data-xh-count></span><button class="x-btn" type="button" data-xh-refresh>刷新</button></div>' +
      '<div class="x-tabs" aria-label="Helm 页面"><button class="on" type="button" role="tab" aria-selected="true" data-xh-tab="installed">已安装</button><button type="button" role="tab" aria-selected="false" data-xh-tab="catalog">应用市场</button></div>' +
      '<div data-xh-content="installed"><input class="x-input" type="search" data-xh-installed-search aria-label="搜索 Helm release" placeholder="Namespace / Release / Chart"><div data-xh-list aria-live="polite"></div></div>' +
      '<div data-xh-content="catalog" hidden><p class="xh-state">当前仅支持无认证 HTTPS index 仓库。“用于新建应用”只进入现有 CI/CD 受控部署流程，默认仅适配 image.repository / image.tag，不保证任意 Chart 可部署。</p><div class="xh-toolbar"><input class="x-input" type="search" data-xh-market-search aria-label="搜索 Helm Chart" placeholder="Chart 名称 / 说明"><span class="x-frac" data-xh-chart-count></span><button class="x-btn" type="button" data-xh-repo-new' + (canManage() ? '' : ' disabled') + '>新增仓库</button></div><p data-xh-market-status role="status" hidden></p><section><h3>Helm 仓库</h3><div data-xh-repos></div><div data-xh-repo-editor hidden></div></section><section><h3>Chart</h3><div data-xh-charts></div></section></div>' +
      '<section class="xh-detail" data-xh-detail aria-label="Release revision 历史" hidden></section></section>';
    s.click = function (event) {
      var b = event.target.closest("button"); if (!b || !el.contains(b) || !current(s)) return;
      if (b.hasAttribute("data-xh-tab")) return selectTab(s, b.getAttribute("data-xh-tab"));
      if (b.hasAttribute("data-xh-refresh")) return refresh(s);
      if (b.hasAttribute("data-xh-view")) return history(s, Number(b.getAttribute("data-xh-view")));
      if (b.hasAttribute("data-xh-close")) {
        ++s.detailRevision; el.querySelector("[data-xh-detail]").hidden = true;
        var trigger = s.detailIndex != null && el.querySelector('[data-xh-view="' + s.detailIndex + '"]'); if (trigger) trigger.focus();
        return;
      }
      if (b.hasAttribute("data-xh-repo-new")) return openRepoForm(s);
      if (b.hasAttribute("data-xh-repo-edit")) return openRepoForm(s, s.repos[Number(b.getAttribute("data-xh-repo-edit"))]);
      if (b.hasAttribute("data-xh-repo-sync")) { var sync = s.repos[Number(b.getAttribute("data-xh-repo-sync"))]; if (sync) return writeRepo(s, "/repos/" + encodeURIComponent(sync.id) + "/sync", "POST", undefined, "仓库已同步。"); }
      if (b.hasAttribute("data-xh-repo-delete")) { var remove = s.repos[Number(b.getAttribute("data-xh-repo-delete"))]; if (remove && canManage()) return confirmDelete(s, remove); }
      if (b.hasAttribute("data-xh-repo-delete-confirm") && s.repoEdit && s.repoEdit.deleteRepo) return writeRepo(s, "/repos/" + encodeURIComponent(s.repoEdit.deleteRepo.id), "DELETE", undefined, "仓库已删除。");
      if (b.hasAttribute("data-xh-repo-cancel")) { s.repoEdit = null; el.querySelector("[data-xh-repo-editor]").hidden = true; setStatus(s, "", false); return; }
      if (b.hasAttribute("data-xh-use-chart")) { var row = s.charts[Number(b.getAttribute("data-xh-use-chart"))]; if (row && repoURL(s, row)) { if (!root.location) root.location = {}; root.location.hash = chartRoute(s, row); } }
    };
    s.input = function (event) {
      if (!current(s)) return;
      if (event.target.hasAttribute("data-xh-installed-search")) { s.installedSearch = event.target.value; drawInstalled(s); }
      if (event.target.hasAttribute("data-xh-market-search")) {
        s.marketSearch = event.target.value;
        clearTimeout(s.searchTimer);
        s.searchTimer = setTimeout(function () { if (current(s) && s.tab === "catalog") loadCharts(s); }, 250);
      }
    };
    s.submit = function (event) { if (event.target.hasAttribute("data-xh-repo-form")) { event.preventDefault(); return submitRepo(s, event.target); } };
    ["click", "input", "submit"].forEach(function (type) { el.addEventListener(type, s[type]); });
    return loadInstalled(s);
  }
  root.XSHelmApps = { render: render, destroy: destroy, _internals: { chartRoute: chartRoute, pending: pending, validRepo: validRepo } };
})(window);
