(function (root) {
  "use strict";
  var mounts = new WeakMap(), types = ['prometheus', 'loki', 'elasticsearch', 'alertmanager', 'jaeger', 'tempo'];
  var labels = { prometheus: 'Prometheus', loki: 'Loki', elasticsearch: 'Elasticsearch', alertmanager: 'Alertmanager', jaeger: 'Jaeger', tempo: 'Tempo' };
  var states = { available: '已发现', missing: '未发现', forbidden: '无权限', error: '发现失败', unavailable: '不可用' };
  var capabilities = { prometheus: 'metrics', loki: 'logs', elasticsearch: 'logs', alertmanager: 'alerts', jaeger: 'traces', tempo: 'traces' };
  var capabilityLabels = { metrics: '指标', logs: '日志', alerts: '告警', traces: '链路' }, drawerSequence = 0;
  function esc(v) { return String(v == null ? '' : v).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function val(v) { return esc(v == null || v === '' ? '—' : v); }
  function can(p) { return !!(root.XS && root.XS.can && root.XS.can(p)); }
  function live(s) { return mounts.get(s.el) === s && !s.controller.signal.aborted && s.context.isCurrent(s.snapshot) && s.el.isConnected !== false; }
  function api(s, path, method, body, query, edit) {
    return Promise.resolve().then(function () { if (!live(s) || edit && (s.edit !== edit || edit.controller.signal.aborted)) throw new Error('集群上下文或配置抽屉已变化'); return root.XS.api(path, { scope: 'cluster', signal: edit ? edit.controller.signal : s.controller.signal, method: method || 'GET', body: body, query: Object.assign({ cluster_id: s.snapshot.cluster_id }, query || {}) }); }).then(function (data) { if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error('响应集群不匹配'); return data; });
  }
  function base(s) { return '/clusters/' + encodeURIComponent(s.snapshot.cluster_id); }
  function settle(p) { return p.then(function (v) { return { ok: true, value: v }; }, function (e) { return { ok: false, error: e }; }); }
  function candidates(data, type) {
    var outer = data && data.data || data || {}, result = outer.result || outer, rows = Array.isArray(result.items) ? result.items.filter(function (r) { return r.type === type; }) : [];
    return { items: rows, state: result.components && result.components[type] && result.components[type].state || (rows.length ? 'available' : 'missing'), error: result.components && result.components[type] && result.components[type].error || '', status: outer.status || '' };
  }
  function resolve(data, s, type) {
    if (!root.XSObserve || !root.XSObserve.resolvedDatasource) throw new Error('数据源归属校验器不可用，已停止绑定');
    var cluster = { id: s.snapshot.cluster_id, name: s.snapshot.name, env: s.snapshot.env };
    return root.XSObserve.resolvedDatasource(data, [cluster], cluster, type);
  }
  function failure(e) {
    var message = String(typeof e === 'string' ? e : e && e.message || ''), status = Number(e && e.status);
    var translations = {
      'cluster client unavailable': '集群客户端不可用，请检查集群连接',
      'no matching component evidence in the readable inventory': '已读取资源中未发现匹配组件，可手动配置地址',
      'component discovery is incomplete: a resource cannot be read': '部分资源无读取权限，发现结果不完整',
      'component discovery failed': '组件发现失败，请刷新重试',
      'component discovery is incomplete: a resource API is unavailable': '部分资源接口不可用，发现结果不完整',
      'component discovery inventory was truncated': '组件发现结果已截断，候选可能不完整',
      'component evidence found without a query endpoint': '已发现组件，但没有可用的查询地址',
      'cluster discovery unavailable': '集群服务发现暂不可用',
      'discovery inventory truncated': '发现结果已截断，候选可能不完整',
      'discovery inventory incomplete': '部分资源无法读取，候选可能不完整',
      'discovery result could not be stored': '发现结果保存失败',
      'ownership mismatch': '数据源不属于当前集群，已停止绑定',
      'not bound': '尚未绑定数据源'
    };
    if (translations[message]) return translations[message];
    if (/[㐀-鿿]/.test(message)) return message;
    var summary = status === 403 || /forbidden|permission denied/i.test(message) ? '无权限，请检查资源访问授权' : status === 401 || /unauthorized/i.test(message) ? '认证失败，请检查登录状态或数据源凭据' : /timeout|timed out|deadline exceeded/i.test(message) ? '请求超时，请检查网络与服务状态' : /connection refused/i.test(message) ? '连接被拒绝，请检查服务地址与端口' : /no such host|ENOTFOUND/i.test(message) ? '地址解析失败，请检查域名' : /certificate|x509/i.test(message) ? '证书校验失败，请检查服务证书' : '接口请求失败';
    return summary + (status ? '（HTTP ' + status + '）' : '') + (message ? '；原始错误：' + message : '');
  }
  function foundFor(s, type) { return s.discovery && s.discovery.ok ? candidates(s.discovery.value, type) : { items: [] }; }
  function urlsOf(found) { return found.items.flatMap(function (row) { return Array.isArray(row.url_candidates) ? row.url_candidates : []; }); }
  function draw(s) {
    var count = 0, query = (s.search || '').trim().toLowerCase();
    var rows = s.entries.map(function (entry, index) {
      var source = entry.source, found = foundFor(s, entry.type), urls = urlsOf(found);
      var searchable = [labels[entry.type], capabilityLabels[capabilities[entry.type]], source && source.ds.name, source && source.ds.url].concat(found.items.map(function (row) { return [row.namespace, row.name].join('/'); }), urls).join(' ').toLowerCase();
      if (s.capability && capabilities[entry.type] !== s.capability || query && !searchable.includes(query)) return '';
      ++count;
      return '<tr><td><strong>' + esc(labels[entry.type]) + '</strong><small class="xi-muted">' + esc(capabilityLabels[capabilities[entry.type]]) + '</small></td><td>' + (source ? '<span class="xi-ellipsis" title="' + esc(source.ds.name) + '">' + val(source.ds.name) + '</span><small class="xi-ellipsis" title="' + esc(source.ds.url) + '">' + val(source.ds.url) + '</small><small class="xi-muted">' + (source.scope === 'central' ? '中央共享 · 编辑将创建集群专用绑定' : '集群专用') + '</small>' : entry.error && entry.error.status === 404 ? '未绑定' : '<span class="xi-error">' + esc(failure(entry.error)) + '</span>') + '</td><td>' + (found.items.length ? '<strong>' + found.items.length + ' 个候选 · ' + urls.length + ' 个地址</strong><small class="xi-muted">在配置抽屉中查看完整候选</small>' : '无候选地址') + '</td><td>' + (s.discovery.ok ? esc(states[found.state] || '状态未知') + (found.error ? '<small class="' + (found.state === 'missing' ? 'xi-muted' : 'xi-error') + '">' + esc(failure(found.error)) + '</small>' : '') : '<span class="xi-error">发现失败：' + esc(failure(s.discovery.error)) + '</span>') + '</td><td><button class="x-btn" type="button" data-xi-edit="' + index + '" aria-haspopup="dialog" aria-label="配置 ' + esc(labels[entry.type]) + ' 集群绑定"' + (can('datasource:manage') && (source || entry.error && entry.error.status === 404) ? '' : ' disabled') + '>配置 / 选择</button>' + (source ? '<button class="x-btn" type="button" data-xi-test-bound="' + index + '"' + (can('datasource:read') ? '' : ' disabled') + '>测试连接</button>' : '') + '</td></tr>';
    }).join('');
    s.el.querySelector('[data-xi-list]').innerHTML = rows ? '<div class="xi-scroll"><table class="x-table xi-table"><thead><tr><th>数据源 / 能力</th><th>当前绑定</th><th>发现候选</th><th>发现状态</th><th>操作</th></tr></thead><tbody>' + rows + '</tbody></table></div>' : '<p class="xi-state">没有匹配的数据源，请调整搜索或能力筛选。</p>';
    show(s, 'count', '显示 ' + count + ' / ' + types.length + ' 种数据源');
  }
  function field(label, name, value, attrs) { return '<label>' + esc(label) + '<input class="x-input" name="' + name + '" value="' + esc(value) + '" ' + (attrs || '') + '></label>'; }
  function close(s, restore) {
    var edit = s.edit; if (!edit) return;
    s.edit = null; edit.tested = ''; edit.controller.abort();
    var target = s.el.querySelector('[data-xi-detail]');
    if (s.doc) { s.doc.removeEventListener('keydown', edit.keydown, true); s.doc.removeEventListener('focusin', edit.focusin, true); }
    if (target) {
      target.removeEventListener('cancel', edit.cancel); target.removeEventListener('close', edit.closed);
      var secret = target.querySelector('[name="secret"]'); if (secret) secret.value = '';
      if (typeof target.close === 'function' && target.open) target.close();
      target.hidden = true; target.innerHTML = '';
    }
    if (edit.done && edit.busy && restore) show(s, 'notice', '保存请求已发出，关闭抽屉不代表撤销。请刷新绑定确认结果，勿重复提交。');
    if (restore && live(s)) {
      var trigger = edit.trigger && s.el.contains(edit.trigger) ? edit.trigger : s.el.querySelector('[data-xi-edit="' + edit.index + '"]') || s.el.querySelector('[data-xi-search]');
      if (trigger) trigger.focus();
    }
  }
  function focusDrawer(s, target) {
    var edit = s.edit, panel = target.querySelector('[data-xi-panel]');
    function focusables() { return Array.from(panel.querySelectorAll('button, input, select, summary, [tabindex]')).filter(function (node) { var fieldset = node.closest('fieldset'); return !node.disabled && !node.hidden && node.getAttribute('tabindex') !== '-1' && !(fieldset && fieldset.disabled); }); }
    edit.cancel = function (event) { event.preventDefault(); close(s, true); };
    edit.closed = function () { if (s.edit === edit && !target.open) close(s, true); };
    edit.keydown = function (event) {
      if (!live(s) || s.edit !== edit) return;
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(s, true); return; }
      if (event.key !== 'Tab') return;
      var nodes = focusables(), current = s.doc && s.doc.activeElement, position = nodes.indexOf(current);
      if (!nodes.length) { event.preventDefault(); panel.focus(); return; }
      if (event.shiftKey && position <= 0 || !event.shiftKey && (position === -1 || position === nodes.length - 1)) { event.preventDefault(); nodes[event.shiftKey ? nodes.length - 1 : 0].focus(); }
    };
    edit.focusin = function (event) { if (live(s) && s.edit === edit && !panel.contains(event.target)) panel.querySelector('[data-xi-close]').focus(); };
    target.addEventListener('cancel', edit.cancel); target.addEventListener('close', edit.closed);
    if (s.doc) { s.doc.addEventListener('keydown', edit.keydown, true); s.doc.addEventListener('focusin', edit.focusin, true); }
    if (typeof target.showModal === 'function') target.showModal();
    target.querySelector('h3').focus();
  }
  function candidateDetails(found) {
    return '<details class="xi-candidates"><summary>完整发现候选（' + found.items.length + '）</summary><p class="xi-muted">发现不代表连接成功或归属正确；选择地址后仍需测试当前配置。</p>' + found.items.map(function (row) {
      return '<article><strong>' + esc((row.namespace || '未指定命名空间') + ' / ' + (row.name || '未命名候选')) + '</strong><small>' + (row.connection_verified === true && row.auth_type === 'none' ? '已验证候选' : '未验证候选') + '</small>' + (row.url_candidates || []).map(function (url) { return '<code>' + esc(url) + '</code>'; }).join('') + '</article>';
    }).join('') + '</details>';
  }
  function open(s, index, trigger) {
    if (s.saving) { show(s, 'notice', '保存请求仍在处理中，请稍后刷新绑定确认结果。'); return; }
    close(s, false);
    var entry = s.entries[index]; if (!entry || !can('datasource:manage') || !entry.source && !(entry.error && entry.error.status === 404)) return;
    var old = entry.source && entry.source.ds || {}, opts = old.options || {}, found = s.discovery.ok ? candidates(s.discovery.value, entry.type) : { items: [] };
    s.edit = { entry: entry, index: index, trigger: trigger, controller: new AbortController(), version: 0, tested: '' };
    var target = s.el.querySelector('[data-xi-detail]'), titleId = 'xi-drawer-title-' + (++drawerSequence); target.hidden = false;
    target.setAttribute('aria-labelledby', titleId);
    target.innerHTML = '<section class="xi-panel" data-xi-panel tabindex="-1"><header class="xi-drawer-head"><div><span class="xi-eyebrow">' + esc(capabilityLabels[capabilities[entry.type]]) + ' · 集群专用绑定</span><h3 id="' + titleId + '" tabindex="-1">' + esc(labels[entry.type]) + ' 数据源配置</h3></div><button class="x-btn" type="button" data-xi-close aria-label="关闭数据源配置抽屉">关闭</button></header><div class="xi-drawer-context"><strong>当前集群：' + esc(s.snapshot.name) + '</strong><span>集群编号：' + esc(s.snapshot.cluster_id) + ' · 数据源：' + esc(old.name || labels[entry.type] + '（新绑定）') + '</span></div><form class="xi-form" data-xi-form><div class="xi-drawer-body"><p class="xi-muted">先填写配置并测试连接，再确认绑定。关闭抽屉将丢弃未保存的配置。</p>' + (entry.source && entry.source.scope === 'central' ? '<p class="xi-hint">当前使用中央共享数据源；保存将创建集群专用绑定，不修改共享配置。凭据不会从共享数据源复制。</p>' : '') + (entry.type === 'prometheus' ? '<p class="xi-hint">Prometheus 兼容后端仅在真实支持所需 HTTP API（含连接测试与查询接口）时可用；不保证所有兼容实现均可接入。</p>' : '') + candidateDetails(found) + '<fieldset data-xi-editable>' +
      field('名称', 'name', old.name || s.snapshot.name + '-' + entry.type, 'required maxlength="64"') + '<label>发现候选<select class="x-select" data-xi-candidate><option value="">手动填写地址</option>' + found.items.map(function (r) { return (r.url_candidates || []).map(function (url) { return '<option value="' + esc(url) + '">' + esc((r.namespace || '') + '/' + (r.name || '') + ' · ' + url + (r.connection_verified === true && r.auth_type === 'none' ? ' · 已验证候选' : ' · 未验证候选')) + '</option>'; }).join(''); }).join('') + '</select></label>' +
      field('服务地址', 'url', old.url || '', 'type="url" required placeholder="https://service.example.com"') + '<label>认证方式<select class="x-select" name="auth_type">' + ['none', 'basic', 'bearer'].map(function (a) { return '<option value="' + a + '"' + (a === (old.auth_type || 'none') ? ' selected' : '') + '>' + ({ none: '无认证', basic: '用户名和密码', bearer: '令牌认证' })[a] + '</option>'; }).join('') + '</select></label>' + field('用户名', 'username', old.username || '') + field('密码 / 令牌', 'secret', '', 'type="password" autocomplete="new-password" placeholder="' + (old.has_secret && entry.source.scope === 'cluster' ? '留空保留已配置凭据' : '填写认证凭据') + '"') + field('超时（秒）', 'timeout_sec', old.timeout_sec || 30, 'type="number" min="1" max="300" required') + '<label class="xi-check"><input type="checkbox" name="tls_skip_verify"' + (old.tls_skip_verify ? ' checked' : '') + '>跳过 TLS 校验</label>' +
      (entry.type === 'loki' ? field('组织 ID', 'org_id', opts.org_id || '') + field('日志筛选器', 'log_selector', opts.log_selector || '') : entry.type === 'elasticsearch' ? field('索引', 'index', opts.index || '') + field('时间字段', 'time_field', opts.time_field || '') : '') +
      '</fieldset><p data-xi-status role="status"></p><p class="xi-error" data-xi-error role="alert" hidden></p></div><footer class="xi-actions"><button class="x-btn" type="button" data-xi-test>测试当前配置</button><label class="xi-check"><input type="checkbox" data-xi-confirm disabled>确认绑定到 ' + esc(s.snapshot.name) + '</label><button class="x-btn x-btn--pri" type="submit" data-xi-save disabled>保存绑定</button></footer></form></section>';
    focusDrawer(s, target);
  }
  function endpoint(value) { try { var u = new URL(value); return /^https?:$/.test(u.protocol) && !u.username && !u.password && !u.hash && !u.search; } catch (_) { return false; } }
  function bodyOf(s, form) {
    var e = s.edit, old = e.entry.source && e.entry.source.ds || {}, oldOpts = old.options || {};
    function get(k) { return form.elements[k] ? form.elements[k].value.trim() : ''; }
    if (!get('name') || !endpoint(get('url'))) throw new Error('请输入名称和不含凭据、查询或片段的 HTTP(S) 地址');
    if (Array.from(get('name')).length > 64) throw new Error('名称不能超过 64 个字符');
    if (!['none', 'basic', 'bearer'].includes(get('auth_type'))) throw new Error('认证方式无效');
    if (get('auth_type') === 'basic' && !get('username')) throw new Error('Basic 认证需要用户名');
    var editingOwned = e.entry.source && e.entry.source.scope === 'cluster';
    if (get('auth_type') !== 'none' && !get('secret') && (!editingOwned || !old.has_secret || old.auth_type !== get('auth_type'))) throw new Error('请填写认证凭据');
    var timeout = Number(get('timeout_sec')); if (!Number.isInteger(timeout) || timeout < 1 || timeout > 300) throw new Error('超时必须为 1–300 秒');
    var options = Object.assign({}, oldOpts, { scope: 'cluster', cluster_id: String(s.snapshot.cluster_id), cluster_name: s.snapshot.name }); delete options.cluster_values; delete options.cluster_label;
    ['org_id', 'log_selector', 'index', 'time_field'].forEach(function (k) { if (form.elements[k]) { if (get(k)) options[k] = get(k); else delete options[k]; } });
    var body = { name: get('name'), type: e.entry.type, url: get('url'), env: s.snapshot.env, auth_type: get('auth_type'), username: get('username'), timeout_sec: timeout, tls_skip_verify: form.elements.tls_skip_verify.checked, options: options };
    if (get('secret')) body.secret = get('secret'); return body;
  }
  function unchanged(body, source) {
    if (!source || source.scope !== 'cluster' || body.secret) return false;
    var ds = source.ds;
    return ['name', 'type', 'url', 'env', 'auth_type', 'username'].every(function (k) { return String(body[k] || '') === String(ds[k] || ''); }) && body.timeout_sec === Number(ds.timeout_sec || 30) && body.tls_skip_verify === !!ds.tls_skip_verify && JSON.stringify(body.options) === JSON.stringify(ds.options);
  }
  function show(s, key, text) { var n = s.el.querySelector('[data-xi-' + key + ']'); if (n) { n.textContent = text; n.hidden = !text; } }
  function invalidate(s) { var e = s.edit; if (!e || e.done) return; ++e.version; e.tested = ''; var form = s.el.querySelector('[data-xi-form]'); if (form) { form.querySelector('[data-xi-save]').disabled = true; var confirm = form.querySelector('[data-xi-confirm]'); confirm.checked = false; confirm.disabled = true; } }
  async function testDraft(s) {
    var e = s.edit, form = s.el.querySelector('[data-xi-form]'); if (!e || e.busy || e.done || !can('datasource:manage')) return;
    var body; show(s, 'error', ''); try { if (!form.reportValidity()) return; body = bodyOf(s, form); } catch (err) { show(s, 'error', err.message); return; }
    var version = e.version; e.busy = true; form.querySelector('[data-xi-test]').disabled = true; show(s, 'status', '正在测试当前配置…');
    try {
      var source = e.entry.source;
      var result = unchanged(body, source) ? await api(s, '/observability/datasources/' + encodeURIComponent(source.ds.id) + '/test', 'POST', undefined, undefined, e) : await api(s, '/observability/datasources/test', 'POST', Object.assign({}, body, source && source.scope === 'cluster' ? { datasource_id: source.ds.id } : {}), undefined, e);
      if (!live(s) || s.edit !== e || version !== e.version) return;
      if (!result || result.ok !== true) throw new Error(result && (result.error || result.message) || '后端未确认连接成功');
      e.tested = JSON.stringify(body); form.querySelector('[data-xi-confirm]').disabled = false; show(s, 'status', '当前配置连接测试通过' + (result.latency_ms != null ? ' · ' + result.latency_ms + ' ms' : '') + '，请确认后保存。');
    } catch (err) { if (live(s) && s.edit === e && version === e.version) { invalidate(s); show(s, 'status', ''); show(s, 'error', [404, 405, 501].includes(err.status) ? '当前后端不支持保存前的草稿连接测试，已停止保存；不会先创建数据源来代替测试。' : '连接测试失败：' + failure(err)); } }
    finally { if (live(s) && s.edit === e) { e.busy = false; form.querySelector('[data-xi-test]').disabled = false; } }
  }
  async function save(s, form) {
    var e = s.edit; if (!e || e.busy || e.done || !can('datasource:manage') || !live(s)) return;
    var body; show(s, 'error', ''); try { body = bodyOf(s, form); if (e.tested !== JSON.stringify(body) || !form.querySelector('[data-xi-confirm]').checked) throw new Error('请测试当前配置并确认绑定'); } catch (err) { show(s, 'error', err.message); return; }
    e.busy = true; e.done = true; s.saving = true; var button = form.querySelector('[data-xi-save]'); button.disabled = true; form.querySelector('[data-xi-test]').disabled = true; form.querySelector('[data-xi-confirm]').disabled = true; form.querySelector('[data-xi-editable]').disabled = true;
    var source = e.entry.source, update = source && source.scope === 'cluster';
    try {
      var result = await api(s, '/observability/datasources' + (update ? '/' + encodeURIComponent(source.ds.id) : ''), update ? 'PUT' : 'POST', body, undefined, e);
      if (!live(s) || s.edit !== e) return;
      e.done = true; e.tested = ''; form.elements.secret.value = '';
      if (result && (result.approval || result.code === 40304 || /^(pending|pending_approval)$/.test(result.status))) { show(s, 'status', '等待审批，尚未确认绑定生效。'); return; }
      show(s, 'status', '保存请求已处理，正在校验集群归属…');
      var resolved = await api(s, '/observability/datasources/resolve', 'GET', undefined, { type: e.entry.type }, e);
      if (!live(s) || s.edit !== e) return;
      var verified = resolve(resolved, s, e.entry.type); if (!verified || verified.ds.url !== body.url || verified.scope !== 'cluster') throw new Error('保存后绑定与预期不匹配');
      show(s, 'status', '绑定已回读，当前集群使用 ' + verified.ds.url); await load(s);
    } catch (err) { if (!live(s) || s.edit !== e) return; if (err.code === 40304 || root.XS.isApproval && root.XS.isApproval(err)) { e.done = true; e.tested = ''; form.elements.secret.value = ''; show(s, 'status', '等待审批，尚未确认绑定生效。'); } else { e.tested = ''; form.elements.secret.value = ''; show(s, 'error', '保存 / 回读失败：' + failure(err) + '；结果未确认，请刷新绑定后再操作，勿重复提交。'); } }
    finally { s.saving = false; if (live(s) && s.edit === e) { e.busy = false; form.querySelector('[data-xi-editable]').disabled = !!e.done; button.disabled = !!e.done || !e.tested; } }
  }
  async function load(s) {
    var results = await Promise.all([settle(api(s, base(s) + '/integration-discovery')), Promise.all(types.map(function (type) { return settle(api(s, '/observability/datasources/resolve', 'GET', undefined, { type: type }).then(function (data) { return resolve(data, s, type); })); }))]);
    if (!live(s)) return;
    s.discovery = results[0]; s.entries = types.map(function (type, i) { return { type: type, source: results[1][i].ok ? results[1][i].value : null, error: results[1][i].error }; }); draw(s);
    var discovery = s.discovery.ok && (s.discovery.value && s.discovery.value.data || s.discovery.value) || {};
    var discoveryStates = { pending: '服务发现等待执行', running: '服务发现正在执行', partial: '服务发现结果不完整', error: '服务发现失败' };
    show(s, 'discovery-status', (discoveryStates[discovery.status] || '') + (discovery.last_error ? '：' + failure(discovery.last_error) : ''));
  }
  function destroy(el) {
    var s = mounts.get(el); if (!s) return;
    close(s, false); mounts.delete(el); s.controller.abort();
    if (s.unsubscribe) s.unsubscribe(); if (s.observer) s.observer.disconnect();
    if (root.removeEventListener) root.removeEventListener('pagehide', s.pagehide);
    ['click', 'input', 'change', 'submit'].forEach(function (type) { el.removeEventListener(type, s[type]); }); el.innerHTML = '';
  }
  function render(el, ctx) {
    destroy(el); var context = root.XSClusterContext;
    if (!context || !root.XS || !root.XS.api) { el.innerHTML = '<p class="xi-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var s = { el: el, context: context, snapshot: context.get(), controller: new AbortController(), entries: [], search: '', capability: '', doc: el.ownerDocument || root.document }; mounts.set(el, s);
    s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    s.pagehide = function () { destroy(el); }; if (root.addEventListener) root.addEventListener('pagehide', s.pagehide);
    if (s.doc && s.doc.body && root.MutationObserver) { s.observer = new root.MutationObserver(function () { if (!el.isConnected) destroy(el); }); s.observer.observe(s.doc.body, { childList: true, subtree: true }); }
    if (s.snapshot.kind !== 'cluster' || !s.snapshot.cluster_id) { el.innerHTML = '<p class="xi-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xi-root" aria-label="可观测数据源绑定"><div class="xi-toolbar"><h2>可观测数据源</h2><button class="x-btn" type="button" data-xi-refresh>刷新</button><button class="x-btn" type="button" data-xi-discover' + (can('cluster:manage') || can('datasource:manage') ? '' : ' disabled') + '>刷新服务发现</button></div><p class="xi-muted">当前集群：' + esc(s.snapshot.name) + ' · 支持 Prometheus、Loki、Elasticsearch、Alertmanager、Jaeger、Tempo 六种数据源。</p><p class="xi-hint">按能力查找，打开抽屉完成配置。发现候选不代表连接成功，保存前必须测试。Prometheus 兼容后端须真实支持所需 HTTP API，不代表支持其他数据源类型。</p><div class="xi-filters"><label>搜索数据源<input class="x-input" type="search" data-xi-search placeholder="名称、类型、命名空间或地址"></label><label>能力筛选<select class="x-select" data-xi-capability><option value="">全部能力</option>' + Object.keys(capabilityLabels).map(function (key) { return '<option value="' + key + '">' + capabilityLabels[key] + '</option>'; }).join('') + '</select></label><span class="xi-muted" data-xi-count role="status"></span></div><p data-xi-notice role="status"></p><p class="xi-muted" data-xi-discovery-status role="status"></p><div data-xi-list><p class="xi-state" role="status">正在读取集群绑定与候选…</p></div><dialog class="xi-detail" data-xi-detail aria-modal="true" hidden></dialog></section>';
    s.click = function (event) {
      if (!live(s)) return;
      if (event.target.hasAttribute('data-xi-detail')) return close(s, true);
      var b = event.target.closest('button'); if (!b || !el.contains(b)) return;
      if (b.hasAttribute('data-xi-refresh') && !(s.edit && s.edit.busy)) return render(el, ctx);
      if (b.hasAttribute('data-xi-discover') && !s.discovering && (can('cluster:manage') || can('datasource:manage'))) { s.discovering = true; b.disabled = true; return api(s, base(s) + '/integration-discovery/refresh', 'POST', {}).then(function () { if (live(s)) { show(s, 'notice', '发现刷新已受理，结果以后台实际扫描为准。'); return load(s); } }).catch(function (err) { if (live(s)) show(s, 'notice', '发现刷新失败：' + failure(err)); }).finally(function () { if (live(s)) { s.discovering = false; b.disabled = false; } }); }
      if (b.hasAttribute('data-xi-edit')) return open(s, Number(b.getAttribute('data-xi-edit')), b);
      if (b.hasAttribute('data-xi-close')) return close(s, true);
      if (b.hasAttribute('data-xi-test')) return testDraft(s);
      if (b.hasAttribute('data-xi-test-bound') && can('datasource:read') && !s.testingBound) { var e = s.entries[Number(b.getAttribute('data-xi-test-bound'))]; if (!e || !e.source) return; s.testingBound = true; b.disabled = true; return api(s, '/observability/datasources/' + encodeURIComponent(e.source.ds.id) + '/test', 'POST').then(function (result) { if (live(s)) show(s, 'notice', labels[e.type] + '：' + (result && result.ok === true ? '连接测试通过' : '连接失败：' + failure(result && (result.error || result.message) || '未确认可达'))); }).catch(function (err) { if (live(s)) show(s, 'notice', '测试失败：' + failure(err)); }).finally(function () { if (live(s)) { s.testingBound = false; b.disabled = false; } }); }
    };
    s.input = function (event) {
      if (!live(s)) return;
      if (event.target.hasAttribute('data-xi-search')) { s.search = event.target.value; draw(s); return; }
      if (event.target.hasAttribute('data-xi-capability')) { s.capability = event.target.value; draw(s); return; }
      if (!s.edit || !event.target.closest('[data-xi-form]')) return;
      if (event.target.hasAttribute('data-xi-confirm')) { el.querySelector('[data-xi-save]').disabled = !!s.edit.done || !!s.edit.busy || !event.target.checked || !s.edit.tested; return; }
      invalidate(s); show(s, 'status', '配置已修改，请重新测试后确认绑定。');
    };
    s.change = function (event) { if (event.target.hasAttribute('data-xi-candidate')) { var form = el.querySelector('[data-xi-form]'); if (event.target.value && form) form.elements.url.value = event.target.value; } s.input(event); };
    s.submit = function (event) { if (event.target.hasAttribute('data-xi-form')) { event.preventDefault(); return save(s, event.target); } };
    ['click', 'input', 'change', 'submit'].forEach(function (t) { el.addEventListener(t, s[t]); }); return load(s);
  }
  root.XSClusterIntegrations = { render: render, destroy: destroy, _internals: { candidates: candidates, endpoint: endpoint, bodyOf: bodyOf, unchanged: unchanged, types: types.slice() } };
})(window);
