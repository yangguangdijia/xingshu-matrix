(function (root) {
  "use strict";
  var mounts = new WeakMap(), unsafe = ["__proto__", "constructor", "prototype"];
  var quantity = /^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+|[numkKMGTPE]|[KMGTPE]i)?$/;
  var groups = ["min", "max", "default", "defaultRequest", "maxLimitRequestRatio"];
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function val(v) { return esc(v == null || v === "" ? "—" : v); }
  function object(v) { return v && typeof v === "object" && !Array.isArray(v) ? v : {}; }
  function can(perm) { return !!(root.XS && root.XS.can && root.XS.can(perm)); }
  function live(s) { return mounts.get(s.el) === s && !s.controller.signal.aborted && s.context.isCurrent(s.snapshot) && s.el.isConnected !== false; }
  function approval(v) { return !!(v && (v.code === 40304 || v.approval || /^(pending|pending_approval|awaiting_approval)$/.test(v.status) || root.XS.isApproval && root.XS.isApproval(v))); }
  function ticket(v) { var d = v && (v.details || v.approval || v); return d && (d.ticket_id || d.ticket && d.ticket.id || d.id) || ""; }
  function api(s, path, method, body, reason) {
    return Promise.resolve().then(function () {
      if (!live(s)) throw new Error("集群上下文已变化");
      return root.XS.api("/clusters/" + encodeURIComponent(s.snapshot.cluster_id) + path, { scope: "cluster", signal: s.controller.signal, method: method || "GET", body: body, query: reason ? { reason: reason } : undefined });
    }).then(function (data) { if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error("响应集群不匹配"); return data; });
  }
  function normalize(row) { return { name: row.name || row.namespace || row.metadata && row.metadata.name || "", phase: row.phase || row.status && row.status.phase,
    labels: row.labels || row.metadata && row.metadata.labels || {}, quotaRead: row.quota_read, limitRead: row.limit_read,
    quotas: Array.isArray(row.resource_quotas) ? row.resource_quotas : [], limits: Array.isArray(row.limit_ranges) ? row.limit_ranges : [] }; }
  function readable(status) { return !status || status.complete === true; }
  function readStatus(status) { return !status || status.complete === true ? '' : '<p class="xq-warning" role="status">' + esc(status.state === 'forbidden' ? '无权限读取' : status.truncated ? '资源列表已截断' : '资源读取失败') + '</p>'; }
  function nameOf(r) { return r.name || r.metadata && r.metadata.name || ""; }
  function limits(r) { return r.limits || r.spec && r.spec.limits || []; }
  function metric(hard, used) {
    return Array.from(new Set(Object.keys(object(hard)).concat(Object.keys(object(used))))).map(function (k) { return '<dt>' + esc(k) + '</dt><dd>' + val(hard && hard[k]) + ' / ' + val(used && used[k]) + '</dd>'; }).join("");
  }
  function draw(s) {
    var rows = s.rows.map(function (r, i) { return { r: r, i: i }; }).filter(function (e) { return e.r.name.toLowerCase().includes(s.search.toLowerCase()); });
    s.el.querySelector("[data-xq-list]").innerHTML = (s.truncated ? '<p class="xq-warning">项目列表已截断</p>' : '') + (rows.length ? '<div class="xq-scroll"><table class="x-table xq-table"><thead><tr><th>Namespace</th><th>Phase</th><th>ResourceQuota hard / used</th><th>LimitRange</th><th>操作</th></tr></thead><tbody>' + rows.map(function (e) {
      return '<tr><td><strong>' + val(e.r.name) + '</strong></td><td>' + val(e.r.phase) + '</td><td>' + readStatus(e.r.quotaRead) + (e.r.quotas.map(function (q, i) { return '<div><strong>' + val(nameOf(q)) + '</strong><dl class="xq-metrics">' + metric(q.hard || q.spec && q.spec.hard, q.used || q.status && q.status.used) + '</dl><button class="x-btn" type="button" data-xq-edit="' + e.i + '" data-xq-kind="quota" data-xq-resource="' + i + '">' + (can("namespace:write") ? '编辑' : '查看') + '</button></div>'; }).join("") || (readable(e.r.quotaRead) ? '未配置' : '')) + '</td><td>' + readStatus(e.r.limitRead) + (e.r.limits.map(function (l, i) {
        return '<div><strong>' + val(nameOf(l)) + '</strong>' + limits(l).map(function (item) { return '<div class="xq-limit-values">' + val(item.type) + '<dl class="xq-metrics">' + groups.map(function (k) { return Object.keys(object(item[k])).length ? '<dt>' + esc(k) + '</dt><dd>' + Object.keys(item[k]).map(function (resource) { return esc(resource) + ': ' + val(item[k][resource]); }).join(' · ') + '</dd>' : ''; }).join("") + '</dl></div>'; }).join("") + '<button class="x-btn" type="button" data-xq-edit="' + e.i + '" data-xq-kind="limit" data-xq-resource="' + i + '">' + (can("namespace:write") ? '编辑' : '查看') + '</button></div>';
      }).join("") || (readable(e.r.limitRead) ? '未配置' : '')) + '</td><td>' + (can("namespace:write") ? '<button class="x-btn" type="button" data-xq-edit="' + e.i + '" data-xq-kind="quota"' + (readable(e.r.quotaRead) ? '' : ' disabled') + '>添加配额</button><button class="x-btn" type="button" data-xq-edit="' + e.i + '" data-xq-kind="limit"' + (readable(e.r.limitRead) ? '' : ' disabled') + '>添加限制</button>' : '只读') + '</td></tr>';
    }).join("") + '</tbody></table></div>' : '<p class="xq-state">' + (s.rows.length ? '没有匹配的 Namespace' : '未发现 Namespace') + '</p>');
  }
  function pair(key, value) { return '<div class="xq-pair" data-xq-pair><label>资源键<input class="x-input" data-xq-key value="' + esc(key) + '"></label><label>值<input class="x-input" data-xq-value value="' + esc(value) + '"></label><button class="x-btn" type="button" data-xq-remove aria-label="删除字段">×</button></div>'; }
  function fields(title, key, values) { return '<fieldset class="xq-fields"><legend>' + esc(title) + '</legend><div data-xq-fields="' + key + '">' + Object.keys(object(values)).map(function (k) { return pair(k, values[k]); }).join("") + '</div><button class="x-btn" type="button" data-xq-add="' + key + '">添加字段</button></fieldset>'; }
  function limitHTML(item) { return '<fieldset class="xq-limit-entry" data-xq-limit><legend>限制条目</legend><label>类型<select class="x-select" name="limit_type">' + ["Container", "Pod", "PersistentVolumeClaim"].map(function (t) { return '<option' + (item.type === t ? ' selected' : '') + '>' + t + '</option>'; }).join("") + '</select></label>' + groups.map(function (k) { return fields(k, k, item[k]); }).join("") + '<button class="x-btn" type="button" data-xq-remove-limit>删除条目</button></fieldset>'; }
  function open(s, kind, row, resource) {
    if (s.edit && (s.edit.busy || s.edit.aiBusy)) return;
    if (row && !resource && !readable(kind === 'quota' ? row.quotaRead : row.limitRead)) return;
    s.edit = { kind: kind, row: row || {}, original: resource || null, version: 0 };
    editor(s, resource || {});
  }
  function editor(s, resource) {
    var e = s.edit, kind = e.kind, target = s.el.querySelector("[data-xq-detail]"), isProject = kind === "project";
    target.hidden = false;
    target.innerHTML = '<div class="xq-toolbar"><h3 tabindex="-1">' + (isProject ? '创建项目 / Namespace' : val(e.row.name) + ' · ' + (kind === 'quota' ? 'ResourceQuota' : 'LimitRange')) + '</h3><button class="x-btn" type="button" data-xq-close>关闭</button></div><form class="xq-form" data-xq-form><fieldset data-xq-editable' + (can("namespace:write") ? '' : ' disabled') + '><label>自然语言描述<input class="x-input" name="instruction" maxlength="2000" placeholder="描述名称、资源数量和限制"></label><button class="x-btn" type="button" data-xq-ai' + (can("ai:chat") ? '' : ' disabled') + '>生成结构化草稿</button><p data-xq-ai-status role="status"></p><label>名称<input class="x-input" name="resource_name" required value="' + esc(nameOf(resource)) + '"' + (e.original ? ' readonly' : '') + ' maxlength="253"></label>' +
      (isProject ? fields("Labels", "labels", resource.labels || resource.metadata && resource.metadata.labels) : kind === "quota" ? fields("Hard", "hard", resource.hard || resource.spec && resource.spec.hard) : '<div data-xq-limits>' + (limits(resource).length ? limits(resource) : [{ type: "Container" }]).map(limitHTML).join("") + '</div><button class="x-btn" type="button" data-xq-add-limit>添加限制条目</button>') +
      '<label>变更原因<input class="x-input" name="reason" required maxlength="500"></label></fieldset><p class="xq-error" data-xq-error role="alert" hidden></p><p data-xq-status role="status"></p><div data-xq-preview hidden><h4>待提交配置</h4><pre></pre><label><input type="checkbox" data-xq-confirm>已核对目标集群和结构化配置</label></div><button class="x-btn x-btn--pri" type="submit" data-xq-submit' + (can("namespace:write") ? '' : ' disabled') + '>验证配置</button></form>';
    target.querySelector("h3").focus();
  }
  function resourceName(v, namespace) { if (!v || v.length > (namespace ? 63 : 253) || !/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(v) || v.split('.').some(function (p) { return !p || p.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(p); }) || namespace && v.includes('.')) throw new Error("资源名称无效"); return v; }
  function collect(form, group, labels) {
    var result = {}, box = form.querySelector('[data-xq-fields="' + group + '"]');
    box.querySelectorAll("[data-xq-pair]").forEach(function (row) {
      var k = row.querySelector("[data-xq-key]").value.trim(), v = row.querySelector("[data-xq-value]").value.trim();
      if (!k && !v) return;
      if (unsafe.includes(k) || Object.prototype.hasOwnProperty.call(result, k) || !/^[A-Za-z0-9][A-Za-z0-9._/\-]*$/.test(k)) throw new Error("资源键无效或重复");
      if (!labels && !quantity.test(v)) throw new Error("请输入有效的非负 Kubernetes 数量：" + k);
      if (labels && (v.length > 63 || v && !/^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$/.test(v))) throw new Error("Label 值无效");
      result[k] = v;
    }); return result;
  }
  function payload(s, form) {
    var e = s.edit, old = e.original || {}, meta = old.metadata || old, name = resourceName(form.elements.resource_name.value.trim(), e.kind === "project");
    var reason = form.elements.reason.value.trim(); if (!reason) throw new Error("请填写变更原因");
    var body = { name: name };
    if (e.kind === "project") { body.labels = collect(form, "labels", true); return body; }
    body.namespace = e.row.name;
    if (e.original) { if (!(meta.resourceVersion || meta.resource_version) || !meta.uid) throw new Error("缺少资源版本或 UID，请刷新后重试"); body.uid = meta.uid; body.resource_version = meta.resourceVersion || meta.resource_version; }
    if (e.kind === "quota") {
      body.spec = { hard: collect(form, "hard") }; if (!Object.keys(body.spec.hard).length) throw new Error("Quota hard 不能为空");
      if (old.scopes || old.spec && old.spec.scopes) body.spec.scopes = old.scopes || old.spec.scopes;
      if (old.scope_selector || old.spec && old.spec.scopeSelector) body.spec.scopeSelector = old.scope_selector || old.spec.scopeSelector;
    } else {
      body.spec = { limits: Array.from(form.querySelectorAll("[data-xq-limit]")).map(function (entry) {
        var item = { type: entry.querySelector('[name="limit_type"]').value };
        if (!["Container", "Pod", "PersistentVolumeClaim"].includes(item.type)) throw new Error("LimitRange 类型无效");
        groups.forEach(function (key) { var values = collect(entry, key); if (Object.keys(values).length) item[key] = values; });
        if (Object.keys(item).length < 2) throw new Error("每条限制至少需要一个资源数量");
        if (item.type !== "Container" && (item.default || item.defaultRequest || item.maxLimitRequestRatio)) throw new Error("默认值和 limit/request 比例只适用于 Container");
        return item;
      }) }; if (!body.spec.limits.length) throw new Error("至少添加一个限制条目");
    } return body;
  }
  function route(e, body) { var path = e.kind === "project" ? "/projects" : e.kind === "quota" ? "/quotas" : "/limitranges"; return { path: path + (e.original ? '/' + encodeURIComponent(e.row.name) + '/' + encodeURIComponent(body.name) : ''), method: e.original ? 'PATCH' : 'POST' }; }
  function show(s, key, text) { var n = s.el.querySelector('[data-xq-' + key + ']'); if (n) { n.textContent = text; n.hidden = !text; } }
  function invalidate(s) { if (!s.edit) return; ++s.edit.version; s.edit.checked = ""; var preview = s.el.querySelector("[data-xq-preview]"); if (preview) preview.hidden = true; var b = s.el.querySelector("[data-xq-submit]"); if (b) b.textContent = "验证配置"; }
  function reload(s) { return api(s, "/projects").then(function (data) { if (!live(s)) return; if (!data || !Array.isArray(data.items)) throw new Error("项目列表格式无效"); s.rows = data.items.map(normalize); s.truncated = data.truncated; draw(s); }); }
  function save(s, form) {
    var e = s.edit; if (!live(s) || !e || e.busy || e.aiBusy || e.done || !can("namespace:write")) return Promise.resolve();
    var body; show(s, "error", "");
    try { if (!form.reportValidity()) return Promise.resolve(); body = payload(s, form); } catch (err) { show(s, "error", err.message); return Promise.resolve(); }
    var reason = form.elements.reason.value.trim(), fingerprint = JSON.stringify({ body: body, reason: reason }), preview = form.querySelector("[data-xq-preview]"), button = form.querySelector("[data-xq-submit]");
    if (e.checked !== fingerprint) { e.checked = fingerprint; preview.hidden = false; preview.querySelector("pre").textContent = JSON.stringify({ cluster_id: s.snapshot.cluster_id, configuration: body }, null, 2); preview.querySelector("input").checked = false; button.textContent = "确认提交"; return Promise.resolve(); }
    if (!preview.querySelector("input").checked) { show(s, "error", "请先核对并确认配置"); return Promise.resolve(); }
    var endpoint = route(e, body); e.busy = true; button.disabled = true; form.querySelector("[data-xq-editable]").disabled = true; show(s, "status", "正在提交…");
    return api(s, endpoint.path, endpoint.method, body, reason).then(function (data) {
      if (!live(s) || s.edit !== e) return;
      if (approval(data)) { e.done = true; show(s, "status", "等待审批 " + ticket(data) + "，尚未应用"); return; }
      if (!data || data.success === false || data.failed > 0 || /^(failed|error|rejected)$/.test(data.status)) throw new Error("后端未确认变更成功");
      e.done = true; show(s, "status", "请求已处理，正在回读资源…");
      return reload(s).then(function () { if (live(s)) show(s, "status", "列表已回读，请核对实际资源状态。"); }, function (err) { if (live(s)) show(s, "error", "请求已处理，但资源回读失败：" + err.message); });
    }).catch(function (err) { if (!live(s) || s.edit !== e) return; if (approval(err)) { e.done = true; show(s, "status", "等待审批 " + ticket(err) + "，尚未应用"); } else { show(s, "status", ""); show(s, "error", err.status === 403 ? "无 namespace:write 权限或集群写入被拒绝" : "提交失败：" + err.message); } }).finally(function () { if (!live(s) || s.edit !== e) return; e.busy = false; button.disabled = !!e.done; form.querySelector("[data-xq-editable]").disabled = !!e.done; });
  }
  async function generate(s) {
    var e = s.edit, form = s.el.querySelector("[data-xq-form]"); if (!e || e.busy || e.aiBusy || !can("ai:chat") || !can("namespace:write")) return;
    var instruction = form.elements.instruction.value.trim(); if (!instruction) { show(s, "ai-status", "请先描述配置要求"); return; }
    var version = e.version, applied = false; e.aiBusy = true;
    try {
      show(s, "ai-status", "正在检查已配置模型…");
      var models = await root.XS.api("/ai/models/options", { scope: "cluster", signal: s.controller.signal });
      if (!live(s) || s.edit !== e || e.version !== version) return;
      if (!(Array.isArray(models) ? models : models.items || models.models || []).length) throw new Error("未配置可用 AI 模型");
      var response = await root.XS.api("/ai/assist", { scope: "cluster", signal: s.controller.signal, method: "POST", body: { scene: "k8s.project." + e.kind, instruction: instruction, schema: { configuration: e.kind === "project" ? "对象：name 为 Namespace DNS 标签，labels 为键值对象" : e.kind === "quota" ? "对象：name 和 hard 资源键值对象，数量为 Kubernetes quantity 字符串" : "对象：name 和 limits 数组，每项为 type、min、max、default、defaultRequest、maxLimitRequestRatio 资源键值对象" } } });
      if (!live(s) || s.edit !== e || e.version !== version) return;
      var generated = response && response.fields && response.fields.configuration;
      generated = typeof generated === "string" ? JSON.parse(generated) : generated;
      if (!generated || typeof generated !== "object" || Array.isArray(generated)) throw new Error("AI 未返回结构化配置");
      var originalName = e.original && nameOf(e.original); resourceName(originalName || generated.name, e.kind === "project");
      if (e.kind === "quota" && !Object.keys(object(generated.hard)).length || e.kind === "limit" && (!Array.isArray(generated.limits) || !generated.limits.length)) throw new Error("AI 草稿缺少资源配置");
      var reason = form.elements.reason.value; generated.name = originalName || generated.name;
      editor(s, generated); var next = s.el.querySelector("[data-xq-form]"); next.elements.reason.value = reason; next.elements.instruction.value = instruction; invalidate(s); applied = true; show(s, "ai-status", "草稿已填入，请核对并验证。" );
    } catch (err) { if (live(s) && s.edit === e) show(s, "ai-status", "生成失败：" + err.message); }
    finally { e.aiBusy = false; if (live(s) && s.edit === e && e.version !== version && !applied) show(s, "ai-status", "配置已改变，已丢弃旧草稿"); }
  }
  function destroy(el) { var s = mounts.get(el); if (!s) return; mounts.delete(el); s.controller.abort(); if (s.unsubscribe) s.unsubscribe(); ["click", "input", "change", "submit"].forEach(function (t) { el.removeEventListener(t, s[t]); }); el.innerHTML = ""; }
  function render(el, ctx) {
    destroy(el); var context = root.XSClusterContext;
    if (!context || !root.XS || !root.XS.api) { el.innerHTML = '<p class="xq-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var s = { el: el, context: context, snapshot: context.get(), controller: new AbortController(), rows: [], search: "" }; mounts.set(el, s);
    s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (s.snapshot.kind !== "cluster" || !s.snapshot.cluster_id) { el.innerHTML = '<p class="xq-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xq-root" aria-label="项目配额"><div class="xq-toolbar"><h2>项目配额</h2><input class="x-input" type="search" data-xq-search aria-label="搜索 Namespace" placeholder="Namespace"><button class="x-btn" type="button" data-xq-refresh>刷新</button>' + (can("namespace:write") ? '<button class="x-btn x-btn--pri" type="button" data-xq-create>创建项目</button>' : '') + '</div><div data-xq-list aria-live="polite"><p class="xq-state" role="status">正在读取 Namespace 与配额…</p></div><section class="xq-detail" data-xq-detail hidden></section></section>';
    s.click = function (event) {
      var b = event.target.closest("button"); if (!b || !el.contains(b) || !live(s) || s.edit && (s.edit.busy || s.edit.aiBusy)) return;
      if (b.hasAttribute("data-xq-refresh")) return render(el, ctx);
      if (b.hasAttribute("data-xq-create") && can("namespace:write")) return open(s, "project");
      if (b.hasAttribute("data-xq-edit")) { var row = s.rows[Number(b.getAttribute("data-xq-edit"))]; if (!row) return; var kind = b.getAttribute("data-xq-kind"); return open(s, kind, row, b.hasAttribute("data-xq-resource") ? (kind === "quota" ? row.quotas : row.limits)[Number(b.getAttribute("data-xq-resource"))] : null); }
      if (b.hasAttribute("data-xq-close")) { s.edit = null; el.querySelector("[data-xq-detail]").hidden = true; var trigger = el.querySelector("[data-xq-edit], [data-xq-create]"); if (trigger) trigger.focus(); return; }
      if (!s.edit || s.edit.done || !can("namespace:write")) return;
      if (b.hasAttribute("data-xq-ai")) return generate(s);
      if (b.hasAttribute("data-xq-add")) b.parentNode.querySelector('[data-xq-fields="' + b.getAttribute("data-xq-add") + '"]').insertAdjacentHTML("beforeend", pair("", ""));
      if (b.hasAttribute("data-xq-remove")) b.closest("[data-xq-pair]").remove();
      if (b.hasAttribute("data-xq-add-limit")) el.querySelector("[data-xq-limits]").insertAdjacentHTML("beforeend", limitHTML({ type: "Container" }));
      if (b.hasAttribute("data-xq-remove-limit")) b.closest("[data-xq-limit]").remove();
      invalidate(s);
    };
    s.input = function (event) { if (!live(s)) return; if (event.target.hasAttribute("data-xq-search")) { s.search = event.target.value; draw(s); } else if (s.edit && !event.target.hasAttribute("data-xq-confirm")) invalidate(s); };
    s.change = s.input;
    s.submit = function (event) { if (event.target.hasAttribute("data-xq-form")) { event.preventDefault(); return save(s, event.target); } };
    ["click", "input", "change", "submit"].forEach(function (t) { el.addEventListener(t, s[t]); });
    return reload(s).catch(function (err) { if (live(s)) el.querySelector("[data-xq-list]").innerHTML = '<p class="xq-error" role="alert">' + esc(err.status === 403 ? "无权限读取 Namespace、ResourceQuota 或 LimitRange" : "读取项目失败：" + err.message) + '</p>'; });
  }
  root.XSProjectQuota = { render: render, destroy: destroy, _internals: { normalize: normalize, resourceName: resourceName, collect: collect, payload: payload, route: route, approval: approval } };
})(window);
