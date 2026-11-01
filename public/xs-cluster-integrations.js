(function (root) {
  "use strict";
  var mounts = new WeakMap(), types = ['prometheus', 'loki', 'elasticsearch', 'alertmanager', 'jaeger', 'tempo'];
  var labels = { prometheus: 'Prometheus', loki: 'Loki', elasticsearch: 'Elasticsearch', alertmanager: 'Alertmanager', jaeger: 'Jaeger', tempo: 'Tempo' };
  var states = { available: '已发现', missing: '未发现', forbidden: '无权限', error: '发现失败', unavailable: '不可用' };
  function esc(v) { return String(v == null ? '' : v).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function val(v) { return esc(v == null || v === '' ? '—' : v); }
  function can(p) { return !!(root.XS && root.XS.can && root.XS.can(p)); }
  function live(s) { return mounts.get(s.el) === s && !s.controller.signal.aborted && s.context.isCurrent(s.snapshot) && s.el.isConnected !== false; }
  function api(s, path, method, body, query) {
    return Promise.resolve().then(function () { if (!live(s)) throw new Error('集群上下文已变化'); return root.XS.api(path, { scope: 'cluster', signal: s.controller.signal, method: method || 'GET', body: body, query: Object.assign({ cluster_id: s.snapshot.cluster_id }, query || {}) }); }).then(function (data) { if (data && data.cluster_id != null && String(data.cluster_id) !== String(s.snapshot.cluster_id)) throw new Error('响应集群不匹配'); return data; });
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
  function failure(e) { return Number(e && e.status) === 403 ? '无权限' : e && e.message || '接口不可用'; }
  function draw(s) {
    s.el.querySelector('[data-xi-list]').innerHTML = '<div class="xi-scroll"><table class="x-table xi-table"><thead><tr><th>数据源</th><th>当前绑定</th><th>发现候选</th><th>发现 / 连接</th><th>操作</th></tr></thead><tbody>' + s.entries.map(function (e, i) {
      var source = e.source, found = s.discovery.ok ? candidates(s.discovery.value, e.type) : null;
      var urls = found ? found.items.flatMap(function (r) { return Array.isArray(r.url_candidates) ? r.url_candidates : []; }) : [];
      return '<tr><td><strong>' + esc(labels[e.type]) + '</strong></td><td>' + (source ? val(source.ds.name) + '<small>' + val(source.ds.url) + '</small><small>' + (source.scope === 'central' ? '中央共享 · 编辑将创建集群专用绑定' : '集群专用') + '</small>' : e.error && e.error.status === 404 ? '未绑定' : '<span class="xi-error">' + esc(failure(e.error)) + '</span>') + '</td><td>' + (urls.length ? urls.map(function (u) { return '<small>' + esc(u) + '</small>'; }).join('') : '无候选地址') + '</td><td>' + (found ? val(states[found.state] || found.state) + (found.error ? '<small class="xi-error">' + esc(found.error) + '</small>' : '') + '<small>' + (urls.length ? '候选不代表可达，保存前必须测试' : '') + '</small>' : '<span class="xi-error">发现失败：' + esc(failure(s.discovery.error)) + '</span>') + '</td><td><button class="x-btn" type="button" data-xi-edit="' + i + '"' + (can('datasource:manage') && (source || e.error && e.error.status === 404) ? '' : ' disabled') + '>配置 / 选择</button>' + (source ? '<button class="x-btn" type="button" data-xi-test-bound="' + i + '"' + (can('datasource:read') ? '' : ' disabled') + '>测试连接</button>' : '') + '</td></tr>';
    }).join('') + '</tbody></table></div>';
    var components = s.el.querySelector('[data-xi-components]');
    components.innerHTML = '<div class="xi-toolbar"><h3>组件库存</h3><a class="x-btn" href="#/cluster/components">完整组件目录 / 安装</a></div>' + (!s.components.ok ? '<p class="xi-error" role="alert">组件发现失败：' + esc(failure(s.components.error)) + '</p>' : !Array.isArray(s.components.value.items) ? '<p class="xi-error">组件库存格式无效</p>' : !s.components.value.items.length ? '<p class="xi-state">未发现已安装组件</p>' : '<div class="xi-scroll"><table class="x-table xi-table"><thead><tr><th>组件</th><th>类型</th><th>Namespace</th><th>发现状态</th></tr></thead><tbody>' + s.components.value.items.map(function (r) { return '<tr><td>' + val(r.name || r.component || r.key) + '</td><td>' + val(r.kind || r.category || r.type) + '</td><td>' + val(r.namespace) + '</td><td>' + val(r.state || r.status || '未知') + '</td></tr>'; }).join('') + '</tbody></table></div>');
  }
  function field(label, name, value, attrs) { return '<label>' + esc(label) + '<input class="x-input" name="' + name + '" value="' + esc(value) + '" ' + (attrs || '') + '></label>'; }
  function open(s, index) {
    if (s.edit && s.edit.busy) return;
    var entry = s.entries[index]; if (!entry || !can('datasource:manage') || !entry.source && !(entry.error && entry.error.status === 404)) return;
    var old = entry.source && entry.source.ds || {}, opts = old.options || {}, found = s.discovery.ok ? candidates(s.discovery.value, entry.type) : { items: [] };
    s.edit = { entry: entry, version: 0, tested: '', confirmed: '' };
    var target = s.el.querySelector('[data-xi-detail]'); target.hidden = false;
    target.innerHTML = '<div class="xi-toolbar"><h3 tabindex="-1">' + esc(labels[entry.type]) + ' 集群绑定</h3><button class="x-btn" type="button" data-xi-close>关闭</button></div><form class="xi-form" data-xi-form><fieldset data-xi-editable>' +
      field('名称', 'name', old.name || s.snapshot.name + '-' + entry.type, 'required maxlength="64"') + '<label>发现候选<select class="x-select" data-xi-candidate><option value="">手动填写地址</option>' + found.items.map(function (r) { return (r.url_candidates || []).map(function (url) { return '<option value="' + esc(url) + '">' + esc((r.namespace || '') + '/' + (r.name || '') + ' · ' + url + (r.connection_verified === true && r.auth_type === 'none' ? ' · 已验证候选' : ' · 未验证候选')) + '</option>'; }).join(''); }).join('') + '</select></label>' +
      field('服务地址', 'url', old.url || '', 'type="url" required placeholder="https://service.example.com"') + '<label>认证方式<select class="x-select" name="auth_type">' + ['none', 'basic', 'bearer'].map(function (a) { return '<option' + (a === (old.auth_type || 'none') ? ' selected' : '') + '>' + a + '</option>'; }).join('') + '</select></label>' + field('用户名', 'username', old.username || '') + field('密码 / Token', 'secret', '', 'type="password" autocomplete="new-password" placeholder="' + (old.has_secret ? '留空保留已配置凭据' : '填写认证凭据') + '"') + field('超时（秒）', 'timeout_sec', old.timeout_sec || 30, 'type="number" min="1" max="300" required') + '<label class="xi-check"><input type="checkbox" name="tls_skip_verify"' + (old.tls_skip_verify ? ' checked' : '') + '>跳过 TLS 校验</label>' +
      (entry.type === 'loki' ? field('组织 ID', 'org_id', opts.org_id || '') + field('日志筛选器', 'log_selector', opts.log_selector || '') : entry.type === 'elasticsearch' ? field('索引', 'index', opts.index || '') + field('时间字段', 'time_field', opts.time_field || '') : '') +
      '</fieldset><p data-xi-status role="status"></p><p class="xi-error" data-xi-error role="alert" hidden></p><div class="xi-actions"><button class="x-btn" type="button" data-xi-test>测试当前配置</button><label class="xi-check"><input type="checkbox" data-xi-confirm disabled>确认绑定到当前集群</label><button class="x-btn x-btn--pri" type="submit" data-xi-save disabled>保存绑定</button></div></form>';
    target.querySelector('h3').focus();
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
      var result = unchanged(body, source) ? await api(s, '/observability/datasources/' + encodeURIComponent(source.ds.id) + '/test', 'POST') : await api(s, '/observability/datasources/test', 'POST', Object.assign({}, body, source && source.scope === 'cluster' ? { datasource_id: source.ds.id } : {}));
      if (!live(s) || s.edit !== e || version !== e.version) return;
      if (!result || result.ok !== true) throw new Error(result && (result.error || result.message) || '后端未确认连接成功');
      e.tested = JSON.stringify(body); form.querySelector('[data-xi-confirm]').disabled = false; show(s, 'status', '当前配置连接测试通过' + (result.latency_ms != null ? ' · ' + result.latency_ms + ' ms' : '') + '，请确认后保存。');
    } catch (err) { if (live(s) && s.edit === e) { invalidate(s); show(s, 'status', ''); show(s, 'error', [404, 405, 501].includes(err.status) ? '当前后端不支持保存前的草稿连接测试，已停止保存；不会先创建数据源来代替测试。' : '连接测试失败：' + failure(err)); } }
    finally { if (live(s) && s.edit === e) { e.busy = false; form.querySelector('[data-xi-test]').disabled = false; } }
  }
  async function save(s, form) {
    var e = s.edit; if (!e || e.busy || e.done || !can('datasource:manage') || !live(s)) return;
    var body; show(s, 'error', ''); try { body = bodyOf(s, form); if (e.tested !== JSON.stringify(body) || !form.querySelector('[data-xi-confirm]').checked) throw new Error('请测试当前配置并确认绑定'); } catch (err) { show(s, 'error', err.message); return; }
    e.busy = true; e.done = true; var button = form.querySelector('[data-xi-save]'); button.disabled = true; form.querySelector('[data-xi-editable]').disabled = true;
    var source = e.entry.source, update = source && source.scope === 'cluster';
    try {
      var result = await api(s, '/observability/datasources' + (update ? '/' + encodeURIComponent(source.ds.id) : ''), update ? 'PUT' : 'POST', body);
      if (!live(s) || s.edit !== e) return;
      e.done = true; e.tested = ''; form.elements.secret.value = '';
      if (result && (result.approval || result.code === 40304 || /^(pending|pending_approval)$/.test(result.status))) { show(s, 'status', '等待审批，尚未确认绑定生效。'); return; }
      show(s, 'status', '保存请求已处理，正在校验集群归属…');
      var resolved = await api(s, '/observability/datasources/resolve', 'GET', undefined, { type: e.entry.type });
      if (!live(s) || s.edit !== e) return;
      var verified = resolve(resolved, s, e.entry.type); if (!verified || verified.ds.url !== body.url || verified.scope !== 'cluster') throw new Error('保存后绑定与预期不匹配');
      show(s, 'status', '绑定已回读，当前集群使用 ' + verified.ds.url); await load(s);
    } catch (err) { if (!live(s) || s.edit !== e) return; if (err.code === 40304 || root.XS.isApproval && root.XS.isApproval(err)) { e.done = true; e.tested = ''; form.elements.secret.value = ''; show(s, 'status', '等待审批，尚未确认绑定生效。'); } else { e.tested = ''; form.elements.secret.value = ''; show(s, 'error', '保存 / 回读失败：' + failure(err) + '；结果未确认，请刷新绑定后再操作，勿重复提交。'); } }
    finally { if (live(s) && s.edit === e) { e.busy = false; form.querySelector('[data-xi-editable]').disabled = !!e.done; button.disabled = !!e.done || !e.tested; } }
  }
  async function load(s) {
    var results = await Promise.all([settle(api(s, base(s) + '/integration-discovery')), settle(api(s, base(s) + '/components')), Promise.all(types.map(function (type) { return settle(api(s, '/observability/datasources/resolve', 'GET', undefined, { type: type }).then(function (data) { return resolve(data, s, type); })); }))]);
    if (!live(s)) return;
    s.discovery = results[0]; s.components = results[1]; s.entries = types.map(function (type, i) { return { type: type, source: results[2][i].ok ? results[2][i].value : null, error: results[2][i].error }; }); draw(s);
  }
  function destroy(el) { var s = mounts.get(el); if (!s) return; mounts.delete(el); s.controller.abort(); if (s.unsubscribe) s.unsubscribe(); if (s.edit) s.edit.tested = ''; ['click', 'input', 'change', 'submit'].forEach(function (t) { el.removeEventListener(t, s[t]); }); el.innerHTML = ''; }
  function render(el, ctx) {
    destroy(el); var context = root.XSClusterContext;
    if (!context || !root.XS || !root.XS.api) { el.innerHTML = '<p class="xi-error" role="alert">集群接口尚未就绪</p>'; return Promise.resolve(); }
    var s = { el: el, context: context, snapshot: context.get(), controller: new AbortController(), entries: [] }; mounts.set(el, s);
    s.unsubscribe = context.subscribe(function () { if (mounts.get(el) === s) render(el, ctx); });
    if (s.snapshot.kind !== 'cluster' || !s.snapshot.cluster_id) { el.innerHTML = '<p class="xi-state">请选择单个集群</p>'; return Promise.resolve(); }
    el.innerHTML = '<section class="xi-root" aria-label="集群接入配置"><div class="xi-toolbar"><h2>接入配置</h2><button class="x-btn" type="button" data-xi-refresh>刷新</button><button class="x-btn" type="button" data-xi-discover' + (can('cluster:manage') || can('datasource:manage') ? '' : ' disabled') + '>刷新发现</button></div><p data-xi-notice role="status"></p><div data-xi-list><p class="xi-state" role="status">正在读取集群绑定与候选…</p></div><section class="xi-detail" data-xi-detail hidden></section><section class="xi-components" data-xi-components></section></section>';
    s.click = function (event) {
      var b = event.target.closest('button'); if (!b || !el.contains(b) || !live(s)) return;
      if (b.hasAttribute('data-xi-refresh') && !(s.edit && s.edit.busy)) return render(el, ctx);
      if (b.hasAttribute('data-xi-discover') && !s.discovering && (can('cluster:manage') || can('datasource:manage'))) { s.discovering = true; b.disabled = true; return api(s, base(s) + '/integration-discovery/refresh', 'POST', {}).then(function () { if (live(s)) { show(s, 'notice', '发现刷新已受理，结果以后台实际扫描为准。'); return load(s); } }).catch(function (err) { if (live(s)) show(s, 'notice', '发现刷新失败：' + failure(err)); }).finally(function () { if (live(s)) { s.discovering = false; b.disabled = false; } }); }
      if (b.hasAttribute('data-xi-edit')) return open(s, Number(b.getAttribute('data-xi-edit')));
      if (b.hasAttribute('data-xi-close') && !(s.edit && s.edit.busy)) { s.edit = null; el.querySelector('[data-xi-detail]').hidden = true; var trigger = el.querySelector('[data-xi-edit]'); if (trigger) trigger.focus(); }
      if (b.hasAttribute('data-xi-test')) return testDraft(s);
      if (b.hasAttribute('data-xi-test-bound') && can('datasource:read') && !s.testingBound) { var e = s.entries[Number(b.getAttribute('data-xi-test-bound'))]; if (!e || !e.source) return; s.testingBound = true; b.disabled = true; return api(s, '/observability/datasources/' + encodeURIComponent(e.source.ds.id) + '/test', 'POST').then(function (result) { if (live(s)) show(s, 'notice', labels[e.type] + '：' + (result && result.ok === true ? '连接测试通过' : '连接失败：' + (result && result.error || '未确认可达'))); }).catch(function (err) { if (live(s)) show(s, 'notice', '测试失败：' + failure(err)); }).finally(function () { if (live(s)) { s.testingBound = false; b.disabled = false; } }); }
    };
    s.input = function (event) { if (!live(s) || !s.edit) return; if (event.target.hasAttribute('data-xi-confirm')) { el.querySelector('[data-xi-save]').disabled = !event.target.checked || !s.edit.tested; return; } invalidate(s); };
    s.change = function (event) { if (event.target.hasAttribute('data-xi-candidate')) { var form = el.querySelector('[data-xi-form]'); if (event.target.value && form) form.elements.url.value = event.target.value; } s.input(event); };
    s.submit = function (event) { if (event.target.hasAttribute('data-xi-form')) { event.preventDefault(); return save(s, event.target); } };
    ['click', 'input', 'change', 'submit'].forEach(function (t) { el.addEventListener(t, s[t]); }); return load(s);
  }
  root.XSClusterIntegrations = { render: render, destroy: destroy, _internals: { candidates: candidates, endpoint: endpoint, bodyOf: bodyOf, unchanged: unchanged, types: types.slice() } };
})(window);
