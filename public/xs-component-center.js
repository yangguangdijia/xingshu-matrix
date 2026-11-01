(function (root) {
  "use strict";
  var mounts = new WeakMap();
  var states = { installed: "已安装", available: "已发现且就绪", partial: "已发现，尚未就绪", missing: "未安装", not_installed: "未安装", forbidden: "无权限", error: "发现失败", unavailable: "不可用", unknown: "状态未知", audit_pending: "安装已部署，审计待核对", pending_approval: "等待审批", awaiting_approval: "等待审批", pending: "等待审批", approved: "审批通过，等待执行", queued: "排队中", running: "安装中", succeeded: "安装完成", success: "安装完成", completed: "安装完成", failed: "安装失败", rejected: "审批拒绝", cancelled: "已取消", unsupported: "当前服务不支持安装" };
  // Curated chart value paths; only expose fields the component actually consumes.
  var profiles = {
    'ingress-nginx': { resources: 'controller.resources', exposure: 'controller.service.type' },
    'istiod': { resources: 'resources' }, 'istio-gateway': { resources: 'resources', exposure: 'service.type' },
    'cert-manager': { resources: 'resources' }, argocd: { resources: 'server.resources', exposure: 'server.service.type' },
    jenkins: { resources: 'controller.resources', storage: 'persistence', exposure: 'controller.serviceType' },
    prometheus: { resources: 'prometheus.prometheusSpec.resources' }, alertmanager: { resources: 'alertmanager.alertmanagerSpec.resources' },
    loki: { resources: 'singleBinary.resources', storage: 'singleBinary.persistence' },
    tempo: { resources: 'resources', storage: 'persistence', exposure: 'service.type' },
    jaeger: { resources: 'allInOne.resources' }, elasticsearch: { resources: 'resources', storage: 'volumeClaimTemplate' }
  };
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function val(v) { return esc(v == null || v === "" ? "—" : v); }
  function can(perm) { return !!(root.XS && root.XS.can && root.XS.can(perm)); }
  function live(s) { return mounts.get(s.el) === s && !s.controller.signal.aborted && s.context.isCurrent(s.snapshot) && s.el.isConnected !== false; }
  function items(v) { if (!v || !Array.isArray(v.items)) throw new Error("组件列表格式无效"); return v.items; }
  function key(r) { return String(r.id || r.component || r.key || r.name || ""); }
  function state(r) { return r.state || r.status || (r.installed === true ? "installed" : "unknown"); }
  function installed(r) { return r && (r.installed === true || /^(installed|available)$/.test(state(r))); }
  function pending(v) { return !!(v && (v.code === 40304 || v.approval || v.approval_required === true || /^(pending|pending_approval|awaiting_approval)$/.test(v.status || v.state) || root.XS.isApproval && root.XS.isApproval(v))); }
  function api(s, path, method, body, query) {
    return Promise.resolve().then(function () { if (!live(s)) throw new Error("集群上下文已变化"); return root.XS.api("/clusters/" + encodeURIComponent(s.snapshot.cluster_id) + path, { scope: "cluster", signal: s.controller.signal, method: method || "GET", body: body, query: query }); }).then(function (data) {
      if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error("响应集群不匹配"); return data;
    });
  }
  function settle(p) { return p.then(function (v) { return { ok: true, value: v }; }, function (e) { return { ok: false, error: e }; }); }
  function failure(err) { return err && err.status === 403 ? "无权限读取组件" : "组件接口失败：" + (err.message || "未知错误"); }
  function inventoryFor(s, item) { return s.inventory.find(function (row) { return key(row) === key(item); }); }
  function installable(s, item) {
    if (!item) return false;
    var inventory = inventoryFor(s, item), status = inventory ? state(inventory) : s.inventoryComplete ? 'missing' : 'unknown';
    return s.inventoryOK && /^(missing|not_installed)$/.test(status) && !installed(inventory) && (item.installable === true || item.supported === true) && can(item.install_permission || "workload:write");
  }
  function draw(s) {
    var inv = s.el.querySelector("[data-xcc-inventory]"), catalog = s.el.querySelector("[data-xcc-catalog]");
    inv.innerHTML = !s.inventoryOK ? '<p class="xcc-error" role="alert">' + esc(failure(s.inventoryError)) + '</p>' : !s.inventory.length ? '<p class="xcc-state">未发现组件库存</p>' : '<div class="xcc-scroll"><table class="x-table xcc-table"><thead><tr><th>组件</th><th>类型 / 安装来源</th><th>Namespace / Release</th><th>版本</th><th>发现状态</th><th>操作</th></tr></thead><tbody>' + s.inventory.filter(function (r) { return [key(r), r.name, r.kind, r.type, r.namespace].join(" ").toLowerCase().includes(s.search.toLowerCase()); }).map(function (r) {
      var index = s.inventory.indexOf(r);
      return '<tr><td><strong>' + val(r.name || key(r)) + '</strong></td><td>' + val(r.kind || r.type) + '<small>' + val(r.install_method || r.source) + '</small></td><td>' + val(r.namespace) + '<small>' + val(r.release) + '</small></td><td>' + val(r.version) + '</td><td><span data-state="' + esc(state(r)) + '">' + val(states[state(r)] || state(r)) + '</span>' + (r.error ? '<small class="xcc-error">' + esc(r.error) + '</small>' : '') + '</td><td><button class="x-btn" type="button" data-xcc-inspect="' + index + '">' + (installed(r) ? '配置 / 选择' : '详情') + '</button></td></tr>';
    }).join("") + '</tbody></table></div>';
    catalog.innerHTML = !s.catalogOK ? '<p class="xcc-error" role="alert">' + esc(failure(s.catalogError)) + '</p>' : !s.catalog.length ? '<p class="xcc-state">组件目录为空</p>' : '<div class="xcc-scroll"><table class="x-table xcc-table"><thead><tr><th>组件</th><th>类型</th><th>说明</th><th>版本</th><th>操作</th></tr></thead><tbody>' + s.catalog.filter(function (r) { return [key(r), r.name, r.type, r.description].join(" ").toLowerCase().includes(s.search.toLowerCase()); }).map(function (r) {
      var found = inventoryFor(s, r), available = installable(s, r), label = found && installed(found) ? '已安装' : !s.inventoryOK ? '发现失败' : !found && !s.inventoryComplete ? '库存未确认' : found && /^(forbidden|error|unavailable|unknown)$/.test(state(found)) ? states[state(found)] : r.installable !== true && r.supported !== true ? '不支持安装' : !can(r.install_permission || "workload:write") ? '无安装权限' : '安装';
      return '<tr><td><strong>' + val(r.name || key(r)) + '</strong></td><td>' + val(r.kind || r.category || r.type) + '</td><td>' + val(r.description || r.notes) + (r.kube_version ? '<small>Kubernetes ' + esc(r.kube_version) + '</small>' : '') + '</td><td>' + val((r.versions || []).map(function (v) { return typeof v === "object" ? v.version || v.name : v; }).join(' / ')) + '</td><td><button class="x-btn" type="button" data-xcc-install="' + s.catalog.indexOf(r) + '"' + (available ? '' : ' disabled') + '>' + esc(label) + '</button></td></tr>';
    }).join("") + '</tbody></table></div>';
  }
  function choices(rows, selected, empty) { return (empty ? '<option value="">' + esc(empty) + '</option>' : '') + rows.map(function (r) { var v = typeof r === "object" ? r.value || r.name || r.version : r; return '<option value="' + esc(v) + '"' + (String(v) === String(selected) ? ' selected' : '') + '>' + esc(typeof r === "object" ? r.label || r.name || v : v) + '</option>'; }).join(""); }
  function field(label, name, value, attrs) { return '<label>' + esc(label) + '<input class="x-input" name="' + name + '" value="' + esc(value) + '" ' + (attrs || '') + '></label>'; }
  function features(item) {
    if (Array.isArray(item.configuration_fields) && item.configuration_fields.length) return item.configuration_fields.map(function (v) { return typeof v === "string" ? v : v.key || v.name; });
    var f = item.fields || item.configuration_fields || [];
    var fields = Array.isArray(f) ? f.map(function (v) { return typeof v === "string" ? v : v.key || v.name; }) : Object.keys(f);
    return fields.length ? fields : Object.keys(profiles[key(item)] || {});
  }
  function profileFields(item) {
    if (Array.isArray(item.configuration_fields) && item.configuration_fields.length) return item.configuration_fields;
    return features(item).map(function (key) {
      var type = key === 'controller.replicaCount' ? 'integer' : /service\.type|serviceType$/.test(key) ? 'enum' : 'quantity';
      return { key: key, label: key, type: type, default: type === 'enum' ? 'ClusterIP' : '' , options: type === 'enum' ? ['ClusterIP', 'NodePort', 'LoadBalancer'] : [] };
    });
  }
  function profileControls(item, defaults, s) {
    return profileFields(item).map(function (f) {
      var key = f.key || f.name, value = defaults[key] == null ? f.default || '' : defaults[key], label = f.label || key;
      if (f.type === 'boolean') return '<label><input type="checkbox" name="' + esc(key) + '"' + (String(value) === 'true' ? ' checked' : '') + (/persistence\.enabled$/.test(key) && (!s.storageOK || !s.storageClasses.length) ? ' disabled' : '') + '> ' + esc(label) + '</label>';
      if (f.type === 'storageClass') return '<label>' + esc(label) + '<select class="x-select" name="' + esc(key) + '"' + (!s.storageOK || !s.storageClasses.length ? ' disabled' : '') + '>' + choices(s.storageClasses, value, '请选择已发现的存储类') + '</select></label>';
      if (f.type === 'enum') return '<label>' + esc(label) + '<select class="x-select" name="' + esc(key) + '">' + choices(f.options || [], value) + '</select></label>';
      return '<label>' + esc(label) + '<input class="x-input" name="' + esc(key) + '" value="' + esc(value) + '" type="' + (f.type === 'integer' ? 'number' : 'text') + '"' + (f.type === 'integer' ? ' min="1" max="10" step="1"' : '') + (f.required ? ' required' : '') + (f.placeholder ? ' placeholder="' + esc(f.placeholder) + '"' : '') + '></label>';
    }).join('');
  }
  function open(s, item) {
    if (!installable(s, item) || s.edit && (s.edit.busy || s.edit.locked)) return;
    var target = s.el.querySelector("[data-xcc-detail]"), defaults = item.defaults || {}, fs = features(item);
    s.edit = { item: item, version: 0, checked: "", preflight: null };
    target.hidden = false;
    target.innerHTML = '<div class="xcc-toolbar"><h3 tabindex="-1">安装 ' + val(item.name || key(item)) + '</h3><button class="x-btn" type="button" data-xcc-close>关闭</button></div><form class="xcc-form" data-xcc-form><fieldset data-xcc-editable>' +
      '<label class="xcc-wide">自然语言描述<input class="x-input" name="instruction" maxlength="2000"></label><button class="x-btn" type="button" data-xcc-ai' + (can("ai:chat") ? '' : ' disabled') + '>生成配置草稿</button>' + field('Namespace', 'namespace', defaults.namespace || '', 'required maxlength="63"') + field('Release', 'release', defaults.release || '', 'required maxlength="53"') +
      '<label>版本<select class="x-select" name="version" required>' + choices(item.versions || [], defaults.version, '请选择版本') + '</select></label>' +
      (fs.includes('storage') || fs.includes('storage_class') ? '<label>StorageClass<select class="x-select" name="storage_class"' + (s.storageOK ? '' : ' disabled') + '>' + choices(item.storage_classes || s.storageClasses || [], defaults.storage_class, '集群默认') + '</select></label>' + field('存储容量', 'storage_size', defaults.storage_size || '', 'placeholder="10Gi"') + (!s.storageOK ? '<p class="xcc-error">StorageClass 发现失败：' + esc(failure(s.storageError)) + '，已停止带存储的安装。</p>' : '') : '') +
      (Array.isArray(item.configuration_fields) && item.configuration_fields.length ? profileControls(item, defaults, s) : (fs.includes('resources') ? field('CPU request', 'cpu_request', defaults.cpu_request || '', 'placeholder="100m"') + field('CPU limit', 'cpu_limit', defaults.cpu_limit || '') + field('Memory request', 'memory_request', defaults.memory_request || '', 'placeholder="128Mi"') + field('Memory limit', 'memory_limit', defaults.memory_limit || '') : '')) +
      (fs.includes('exposure') ? '<label>暴露方式<select class="x-select" name="exposure">' + choices(item.exposure_types || ['ClusterIP', 'NodePort', 'LoadBalancer'], defaults.exposure || 'ClusterIP') + '</select></label>' : '') + field('变更原因', 'reason', '', 'required maxlength="500"') +
      '</fieldset><p class="xcc-error" data-xcc-error role="alert" hidden></p><p data-xcc-status role="status"></p><div data-xcc-preflight hidden></div><label class="xcc-confirm" data-xcc-confirm-wrap hidden><input type="checkbox" data-xcc-confirm>已核对预检结果和配置，确认安装</label><button class="x-btn x-btn--pri" type="submit" data-xcc-submit>预检配置</button></form><section class="xcc-operation" data-xcc-operation aria-live="polite"></section>';
    target.querySelector('h3').focus();
  }
  function configuration(form, item) {
    function get(k) { return form.elements[k] ? form.elements[k].value.trim() : ''; }
    function dns(v, max) { return v.length <= max && /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(v); }
    if (!dns(get('namespace'), 63) || !dns(get('release'), 53)) throw new Error('Namespace / Release 名称无效');
    var versions = (item.versions || []).map(function (v) { return String(typeof v === 'object' ? v.version || v.name : v); });
    if (!versions.includes(get('version'))) throw new Error('请选择目录提供的版本');
    if (!get('reason')) throw new Error('请填写变更原因');
    if (Array.from(get('reason')).length > 500) throw new Error('变更原因不能超过 500 个字符');
    var body = { namespace: get('namespace'), release_name: get('release'), version: get('version'), values: {} }, profile = profiles[key(item)] || {};
    var quantity = /^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+|[numkKMGTPE]|[KMGTPE]i)?$/;
    ['storage_size', 'cpu_request', 'cpu_limit', 'memory_request', 'memory_limit'].forEach(function (k) { if (get(k) && !quantity.test(get(k))) throw new Error('资源数量无效：' + k); });
    if (Array.isArray(item.configuration_fields) && item.configuration_fields.length) {
      profileFields(item).forEach(function (f) {
        var key = f.key || f.name, el = form.elements[key];
        if (!el) { if (f.required) throw new Error('缺少必填字段：' + key); return; }
        if (f.type === 'boolean') { body.values[key] = el.checked && !el.disabled ? 'true' : 'false'; return; }
        if (el.disabled || el.value.trim() === '') { if (f.required) throw new Error('请填写：' + key); return; }
        if (f.type === 'integer' && (!/^[0-9]+$/.test(el.value) || Number(el.value) < 1 || Number(el.value) > 10)) throw new Error('副本数无效：' + key);
        if (f.type === 'enum' && !(f.options || []).includes(el.value)) throw new Error('选项无效：' + key);
        if (f.type === 'quantity' && (!quantity.test(el.value) || parseFloat(el.value) <= 0)) throw new Error('资源数量无效：' + key);
        body.values[key] = el.value;
      });
    } else if (form.elements.storage_class) {
      if (!profile.storage) throw new Error('目录未提供存储字段映射');
      if (get('storage_class')) body.values[profile.storage + '.storageClass'] = get('storage_class');
      if (get('storage_size')) body.values[profile.storage + (profile.storage === 'volumeClaimTemplate' ? '.resources.requests.storage' : '.size')] = get('storage_size');
    }
    if (form.elements.cpu_request) {
      if (!profile.resources) throw new Error('目录未提供资源字段映射');
      ['cpu', 'memory'].forEach(function (k) { if (get(k + '_request')) body.values[profile.resources + '.requests.' + k] = get(k + '_request'); if (get(k + '_limit')) body.values[profile.resources + '.limits.' + k] = get(k + '_limit'); });
    }
    if (form.elements.exposure) {
      if (!(item.exposure_types || ['ClusterIP', 'NodePort', 'LoadBalancer']).includes(get('exposure'))) throw new Error('暴露方式无效');
      if (get('hostname') && !/^(?=.{1,253}$)[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(get('hostname'))) throw new Error('域名无效');
      if (!profile.exposure) throw new Error('目录未提供暴露字段映射');
      body.values[profile.exposure] = get('exposure');
      if (get('hostname')) throw new Error('该目录未提供域名映射，请先安装后在入口配置中设置域名');
    } return body;
  }
  function preflightOK(data) { return !!(data && (data.ok === true || data.ready === true) && data.supported !== false && data.allowed !== false && !(data.errors || []).length && !(data.conflicts || []).length && !(data.checks || []).some(function (c) { return c.ok === false || /^(failed|forbidden|error|missing|unavailable)$/.test(c.state || c.status); })); }
  function preflightHTML(data, body, item, reason) {
    var profile = profiles[key(item)] || {}, fields = [['组件', item.name || key(item)], ['环境', body.environment], ['Namespace', body.namespace], ['Release', body.release_name], ['版本', body.version]];
    function value(label, path) { if (body.values[path] != null) fields.push([label, body.values[path]]); }
    var structured = Array.isArray(item.configuration_fields) && item.configuration_fields.length;
    if (structured) profileFields(item).forEach(function (f) { value(f.label || f.key, f.key); });
    if (!structured && profile.storage) {
      fields.push(['StorageClass', body.values[profile.storage + '.storageClass'] || '集群默认']);
      value('存储容量', profile.storage + (profile.storage === 'volumeClaimTemplate' ? '.resources.requests.storage' : '.size'));
    }
    if (!structured && profile.resources) {
      value('CPU request', profile.resources + '.requests.cpu'); value('CPU limit', profile.resources + '.limits.cpu');
      value('Memory request', profile.resources + '.requests.memory'); value('Memory limit', profile.resources + '.limits.memory');
    }
    if (!structured && profile.exposure) value('暴露方式', profile.exposure);
    if (data.chart_digest) fields.push(['Chart SHA-256', data.chart_digest]);
    fields.push(['变更原因', reason]);
    return '<h4>' + (preflightOK(data) ? '预检通过' : '预检未通过，已停止安装') + '</h4>' + (data.checks || []).map(function (c) { return '<p>' + val(c.name || c.check) + ' · ' + val(c.ok === true ? c.message || '通过' : c.message || c.detail || c.state || c.status || '未确认') + '</p>'; }).join('') + (data.errors || []).concat(data.conflicts || []).map(function (e) { return '<p class="xcc-error">' + esc(typeof e === 'string' ? e : e.message) + '</p>'; }).join('') + '<h4>核对安装配置</h4><dl class="xcc-definition" aria-label="安装配置">' + fields.map(function (f) { return '<dt>' + esc(f[0]) + '</dt><dd>' + val(f[1]) + '</dd>'; }).join('') + '</dl>';
  }
  function show(s, key, message) { var n = s.el.querySelector('[data-xcc-' + key + ']'); if (n) { n.textContent = message; n.hidden = !message; } }
  function invalidate(s) { if (!s.edit || s.edit.locked) return; ++s.edit.version; s.edit.checked = ''; s.edit.preflight = null; ['preflight', 'confirm-wrap'].forEach(function (key) { var n = s.el.querySelector('[data-xcc-' + key + ']'); if (n) n.hidden = true; }); var confirm = s.el.querySelector('[data-xcc-confirm]'); if (confirm) confirm.checked = false; var b = s.el.querySelector('[data-xcc-submit]'); if (b) b.textContent = '预检配置'; }
  function operationHTML(op) {
    var status = op.status || op.state || 'unknown', approvalID = op.ticket_id || op.approval_id, logs = Array.isArray(op.logs) ? op.logs.map(function (r) { return typeof r === 'string' ? r : r.message || r.text || ''; }).join('\n') : typeof op.logs === 'string' ? op.logs : op.log_excerpt || '';
    return '<h4>安装操作 ' + val(op.id || op.operation_id) + '</h4><p data-state="' + esc(status) + '">' + val(states[status] || status) + (approvalID ? ' · 审批 #' + esc(approvalID) : '') + '</p>' + (op.message ? '<p>' + esc(op.message) + '</p>' : '') + (op.error ? '<p class="xcc-error">' + esc(op.error) + '</p>' : '') + (typeof op.progress === 'number' ? '<progress max="100" value="' + Math.max(0, Math.min(100, op.progress)) + '" aria-label="安装进度"></progress>' : '') + (logs ? '<pre class="xcc-logs">' + esc(logs) + '</pre>' : '<p class="xcc-state">暂未返回操作日志</p>');
  }
  function terminal(op) { return /^(succeeded|success|completed|failed|rejected|cancelled|unsupported|unknown|audit_pending)$/.test(op.status || op.state); }
  function accepted(s, e, result) {
    var data = result.details || result.approval || result, op = data.operation || data, waiting = pending(result) || pending(data) || pending(op);
    var id = data.operation_id || (data.operation ? data.operation.id : !waiting ? op.id : null);
    var approvalID = data.ticket_id || data.approval_id || data.ticket && data.ticket.id || op.approval_id;
    e.locked = true;
    s.el.querySelector('[data-xcc-operation]').innerHTML = operationHTML({ id: id, approval_id: approvalID, status: waiting ? 'pending_approval' : 'queued', message: '安装请求已受理，结果以操作状态回读为准。' });
    show(s, 'status', waiting ? '等待审批' + (approvalID ? ' #' + approvalID : '') + '，尚未安装。' + (!id ? '后端未提供可跟踪操作编号。' : '') : id ? '安装请求已受理，正在读取真实操作状态。' : '后端未返回操作编号，无法确认安装结果，请核对库存。');
    if (id) poll(s, id, e);
  }
  async function poll(s, id, e) {
    if (!live(s) || s.edit !== e) return;
    try {
      var data = await api(s, '/components/operations/' + encodeURIComponent(id)); if (!live(s) || s.edit !== e) return;
      var op = data.operation || data;
      if (String(op.id || op.operation_id || '') !== String(id)) throw new Error('操作响应编号不匹配');
      if (op.cluster_id != null && String(op.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error('响应集群不匹配');
      s.el.querySelector('[data-xcc-operation]').innerHTML = operationHTML(op);
      if (terminal(op)) { e.busy = false; show(s, 'status', '操作状态已回读：' + (states[op.status || op.state] || op.status || op.state)); await load(s); return; }
      s.timer = setTimeout(function () { poll(s, id, e); }, 2500);
    } catch (err) { if (live(s) && s.edit === e) { e.busy = false; show(s, 'status', '操作状态读取失败：' + err.message + '。请使用操作编号核对，勿重复提交。'); } }
  }
  async function submit(s, form) {
    var e = s.edit; if (!live(s) || !e || e.busy || e.locked || !installable(s, e.item)) return;
    var body, button = form.querySelector('[data-xcc-submit]'); show(s, 'error', '');
    try {
      if (!form.reportValidity()) return;
      if (form.elements.storage_class && !s.storageOK) throw new Error('StorageClass 发现未完成或失败，已停止安装');
      if (form.elements.storage_class && form.elements.storage_class.value && !(e.item.storage_classes || s.storageClasses || []).some(function (r) { return String(typeof r === 'object' ? r.value || r.name : r) === form.elements.storage_class.value; })) throw new Error('请选择已发现的 StorageClass');
      body = configuration(form, e.item);
      if (Array.isArray(e.item.configuration_fields)) profileFields(e.item).forEach(function (f) {
        if (f.type !== 'storageClass') return;
        var enabled = body.values[f.key.replace(/\.storageClass$/, '.enabled')] === 'true', selected = body.values[f.key];
        if (enabled && (!s.storageOK || !selected)) throw new Error('启用持久化前请选择已发现的 StorageClass');
        if (selected && !s.storageClasses.some(function (r) { return String(typeof r === 'object' ? r.name || r.value : r) === selected; })) throw new Error('请选择已发现的 StorageClass');
        if (!enabled) delete body.values[f.key];
      });
    } catch (err) { show(s, 'error', err.message); return; }
    body.environment = s.snapshot.env;
    var reason = form.elements.reason.value.trim(), fingerprint = JSON.stringify({ body: body, reason: reason }), isInstall = e.checked === fingerprint && preflightOK(e.preflight), version = e.version;
    if (isInstall && !form.querySelector('[data-xcc-confirm]').checked) { show(s, 'error', '请确认预检与最终配置'); return; }
    e.busy = true; button.disabled = true; form.querySelector('[data-xcc-editable]').disabled = true;
    if (isInstall) e.locked = true;
    try {
      var result = await api(s, '/components/' + encodeURIComponent(key(e.item)) + (isInstall ? '/install' : '/preflight'), 'POST', isInstall ? Object.assign({}, body, { confirm: true }, e.preflight.chart_digest ? { chart_digest: e.preflight.chart_digest } : {}) : body, { reason: reason });
      if (!live(s) || s.edit !== e || version !== e.version) return;
      if (!isInstall) {
        e.preflight = result; e.checked = fingerprint;
        form.querySelector('[data-xcc-confirm]').checked = false;
        var panel = form.querySelector('[data-xcc-preflight]'); panel.hidden = false; panel.innerHTML = preflightHTML(result, body, e.item, reason);
        form.querySelector('[data-xcc-confirm-wrap]').hidden = !preflightOK(result); button.textContent = preflightOK(result) ? '确认安装' : '重新预检';
      } else {
        accepted(s, e, result);
      }
    } catch (err) {
      if (!live(s) || s.edit !== e) return;
      if (isInstall && (pending(err) || err.operation_id || err.details && err.details.operation_id)) accepted(s, e, err);
      else { invalidate(s); show(s, 'error', (err.status === 403 ? '无安装权限 / 集群拒绝：' : '操作失败：') + err.message + (isInstall ? '；结果未确认，请刷新库存或核对操作状态，不要重复提交。' : '')); }
    } finally { if (live(s) && s.edit === e) { e.busy = false; button.disabled = !!e.locked; form.querySelector('[data-xcc-editable]').disabled = !!e.locked; } }
  }
  async function generate(s) {
    var e = s.edit, form = s.el.querySelector('[data-xcc-form]'); if (!e || e.busy || e.locked || !can('ai:chat') || !installable(s, e.item)) return;
    var instruction = form.elements.instruction.value.trim(), version = e.version; if (!instruction) { show(s, 'error', '请先描述安装配置'); return; }
    e.busy = true;
    try {
      var models = await root.XS.api('/ai/models/options', { scope: 'cluster', signal: s.controller.signal }); if (!live(s) || s.edit !== e || version !== e.version) return;
      if (!(Array.isArray(models) ? models : models.items || models.models || []).length) throw new Error('未配置可用 AI 模型');
      var draftKeys = ['namespace', 'release', 'version'].concat(Array.isArray(e.item.configuration_fields) && e.item.configuration_fields.length ? profileFields(e.item).map(function (f) { return f.key; }) : ['storage_class', 'storage_size', 'cpu_request', 'cpu_limit', 'memory_request', 'memory_limit', 'exposure', 'hostname']);
      var result = await root.XS.api('/ai/assist', { scope: 'cluster', signal: s.controller.signal, method: 'POST', body: { scene: 'k8s.component.install', instruction: instruction, schema: { configuration: '结构化字段对象，仅允许这些字面键：' + JSON.stringify(draftKeys) + '。字段和约束：' + JSON.stringify(e.item.configuration_fields || []) + '。版本候选：' + JSON.stringify(e.item.versions || []) } } });
      if (!live(s) || s.edit !== e || version !== e.version) return;
      var draft = result.fields && result.fields.configuration; draft = typeof draft === 'string' ? JSON.parse(draft) : draft;
      if (!draft || typeof draft !== 'object' || Array.isArray(draft)) throw new Error('AI 未返回结构化草稿');
      draftKeys.forEach(function (k) { var el = form.elements[k]; if (!el || el.disabled) return; if (el.type === 'checkbox') { if (draft[k] === true || draft[k] === 'true') el.checked = true; else if (draft[k] === false || draft[k] === 'false') el.checked = false; } else if (typeof draft[k] === 'string' || typeof draft[k] === 'number') el.value = String(draft[k]); });
      invalidate(s); show(s, 'status', '草稿已填入，请核对后预检。');
    } catch (err) { if (live(s) && s.edit === e) show(s, 'error', '草稿生成失败：' + err.message); }
    finally { e.busy = false; }
  }
  function inspect(s, row) {
    if (!row) return;
    var target = s.el.querySelector('[data-xcc-detail]'); if (s.edit && (s.edit.busy || s.edit.locked)) return;
    s.edit = null; s.inspected = row; target.hidden = false;
    var catalog = s.catalog.find(function (r) { return key(r) === key(row); }), route = row.configure_route || catalog && catalog.configure_route;
    var safeRoute = typeof route === 'string' && /^#\/(?:cluster|observe|traffic|storage|cicd|middleware)(?:\/[a-zA-Z0-9_/-]*)?$/.test(route);
    target.innerHTML = '<div class="xcc-toolbar"><h3 tabindex="-1">' + val(row.name || key(row)) + '</h3><button class="x-btn" type="button" data-xcc-close>关闭</button></div><dl class="xcc-definition">' + ['state', 'namespace', 'release', 'version', 'kind', 'install_method', 'controller', 'crd', 'storage_driver'].filter(function (k) { return row[k] != null; }).map(function (k) { return '<dt>' + esc(k) + '</dt><dd>' + val(row[k]) + '</dd>'; }).join('') + '</dl>' +
      (installed(row) ? '<button class="x-btn" type="button" data-xcc-select>选择此实例</button><p data-xcc-selected role="status"></p>' : '') +
      (installed(row) && safeRoute ? '<a class="x-btn" href="' + esc(route) + '">配置组件</a>' : installed(row) && /^(prometheus|loki|tempo|jaeger|alertmanager|elasticsearch)$/.test(key(row)) ? '<a class="x-btn" href="#/cluster/integrations">选择 / 配置数据源</a>' : '<p class="xcc-state">' + (installed(row) ? '组件已发现；目录未提供配置入口。' : '仅显示实际发现结果。') + '</p>');
    target.querySelector('h3').focus();
  }
  async function load(s) {
    var result = await Promise.all([settle(api(s, '/components')), settle(api(s, '/components/catalog')), settle(api(s, '/storage/classes'))]); if (!live(s)) return;
    try { s.inventory = result[0].ok ? items(result[0].value) : []; s.inventoryOK = result[0].ok; s.inventoryComplete = result[0].ok && result[0].value.truncated === false && result[0].value.complete !== false; s.inventoryError = result[0].error; } catch (err) { s.inventoryOK = false; s.inventoryComplete = false; s.inventoryError = err; }
    try { s.catalog = result[1].ok ? items(result[1].value) : []; s.catalogOK = result[1].ok; s.catalogError = result[1].error; } catch (err) { s.catalogOK = false; s.catalogError = err; }
    try { s.storageClasses = result[2].ok ? items(result[2].value) : []; s.storageOK = result[2].ok && result[2].value.truncated !== true && result[2].value.complete !== false; s.storageError = result[2].error; } catch (err) { s.storageOK = false; s.storageError = err; }
    draw(s);
  }
  function destroy(el) { var s = mounts.get(el); if (!s) return; mounts.delete(el); s.controller.abort(); clearTimeout(s.timer); if (s.unsubscribe) s.unsubscribe(); ['click', 'input', 'change', 'submit'].forEach(function (t) { el.removeEventListener(t, s[t]); }); el.innerHTML = ''; }
  function render(el, ctx) {
    destroy(el); var context = root.XSClusterContext;
    if (!context || !root.XS || !root.XS.api) { el.innerHTML = '<p class="xcc-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var s = { el: el, ctx: ctx || {}, context: context, snapshot: context.get(), controller: new AbortController(), inventory: [], catalog: [], search: '' }; mounts.set(el, s);
    s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (s.snapshot.kind !== 'cluster' || !s.snapshot.cluster_id) { el.innerHTML = '<p class="xcc-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xcc-root" aria-label="组件中心"><div class="xcc-toolbar"><h2>组件中心</h2><input class="x-input" type="search" data-xcc-search aria-label="搜索组件" placeholder="名称 / 类型"><button class="x-btn" type="button" data-xcc-refresh>刷新</button></div><section class="xcc-section"><h3>集群组件</h3><div data-xcc-inventory><p class="xcc-state" role="status">正在发现组件…</p></div></section><section class="xcc-section"><h3>组件目录</h3><div data-xcc-catalog><p class="xcc-state" role="status">正在读取目录…</p></div></section><section class="xcc-detail" data-xcc-detail hidden></section></section>';
    s.click = function (event) {
      var b = event.target.closest('button'); if (!b || !el.contains(b) || !live(s)) return;
      if (b.hasAttribute('data-xcc-refresh') && !(s.edit && s.edit.busy)) return render(el, ctx);
      if (b.hasAttribute('data-xcc-install')) return open(s, s.catalog[Number(b.getAttribute('data-xcc-install'))]);
      if (b.hasAttribute('data-xcc-inspect')) return inspect(s, s.inventory[Number(b.getAttribute('data-xcc-inspect'))]);
      if (b.hasAttribute('data-xcc-select') && s.inspected && installed(s.inspected)) {
        var selection = { cluster_id: s.snapshot.cluster_id, component: key(s.inspected), namespace: s.inspected.namespace || '', name: s.inspected.name || '', kind: s.inspected.kind || '' };
        if (typeof s.ctx.onSelectComponent === 'function') s.ctx.onSelectComponent(selection);
        if (typeof root.CustomEvent === 'function' && typeof el.dispatchEvent === 'function') el.dispatchEvent(new root.CustomEvent('xs:component-select', { detail: selection, bubbles: true }));
        show(s, 'selected', '已选择当前集群实例：' + (selection.namespace ? selection.namespace + '/' : '') + selection.name);
      }
      if (b.hasAttribute('data-xcc-close') && !(s.edit && s.edit.busy)) { clearTimeout(s.timer); s.edit = null; el.querySelector('[data-xcc-detail]').hidden = true; var trigger = el.querySelector('[data-xcc-install], [data-xcc-inspect]'); if (trigger) trigger.focus(); }
      if (b.hasAttribute('data-xcc-ai')) return generate(s);
    };
    s.input = function (event) { if (!live(s)) return; if (event.target.hasAttribute('data-xcc-search')) { s.search = event.target.value; draw(s); } else if (!event.target.hasAttribute('data-xcc-confirm')) invalidate(s); };
    s.change = s.input; s.submit = function (event) { if (event.target.hasAttribute('data-xcc-form')) { event.preventDefault(); return submit(s, event.target); } };
    ['click', 'input', 'change', 'submit'].forEach(function (t) { el.addEventListener(t, s[t]); });
    return load(s);
  }
  root.XSComponentCenter = { render: render, destroy: destroy, _internals: { configuration: configuration, preflightOK: preflightOK, installable: installable, operationHTML: operationHTML, pending: pending, terminal: terminal } };
})(window);
