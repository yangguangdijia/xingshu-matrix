import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { Element, harness, deferred, tick } from './xs-resource-pages.harness.mjs';

const originalFocus = Element.prototype.focus;
Element.prototype.focus = function () {
  originalFocus.call(this);
  let node = this;
  while (node && !node.ownerDocument) node = node.parentNode;
  if (node) node.ownerDocument.activeElement = this;
};
Element.prototype.showModal = function () { this.open = true; };
Element.prototype.close = function () { this.open = false; };

const providers = ['prometheus', 'loki', 'elasticsearch', 'alertmanager', 'jaeger', 'tempo'];
const observe = { resolvedDatasource(data, clusters, cluster, type) {
  const datasource = data.datasource;
  if (datasource.type !== type || datasource.options.scope !== 'central' && datasource.options.cluster_id !== cluster.id) throw new Error('ownership mismatch');
  return { ds: datasource, scope: datasource.options.scope };
} };
function api(path, options) {
  if (path.endsWith('/integration-discovery')) return { result: { items: [{ type: 'loki', name: '<日志候选>', namespace: 'monitoring', url_candidates: Array.from({ length: 25 }, (_, index) => `http://loki-${index}.svc:3100`), connection_verified: false }], components: { loki: { state: 'available' } } } };
  if (path.endsWith('/resolve')) throw { status: 404, message: 'not bound' };
  if (path.endsWith('/datasources/test') || /\/datasources\/\d+\/test$/.test(path)) return { ok: true };
  if (path.endsWith('/integration-discovery/refresh')) return {};
  throw new Error(`Unexpected API: ${path} ${options.method}`);
}
function setup(options = {}) {
  const h = harness('xs-cluster-integrations.js', { api, observe, ...options });
  const doc = new Element('document'); doc.body = h.el; h.el.ownerDocument = doc;
  h.window.addEventListener = (type, handler) => doc.addEventListener('window:' + type, handler);
  h.window.removeEventListener = (type, handler) => doc.removeEventListener('window:' + type, handler);
  h.window.MutationObserver = class {
    constructor(callback) { h.removed = callback; }
    observe() { h.observing = true; }
    disconnect() { h.observing = false; }
  };
  h.doc = doc;
  h.key = (key, shiftKey = false) => {
    const event = { key, shiftKey, preventDefault() { this.prevented = true; }, stopPropagation() {} };
    for (const handler of [...(doc.listeners.keydown || [])]) handler(event);
    return event;
  };
  return h;
}
async function open(h, index = 1) {
  await h.window.XSClusterIntegrations.render(h.el);
  const trigger = h.el.querySelector(`[data-xi-edit="${index}"]`);
  trigger.focus(); await h.emit('click', trigger);
  const form = h.el.querySelector('[data-xi-form]');
  if (form && !form.elements.url.value) form.elements.url.value = 'http://loki.svc:3100';
  return { form, trigger, drawer: h.el.querySelector('[data-xi-detail]') };
}
async function confirm(h, form) {
  await h.emit('click', '[data-xi-test]');
  form.querySelector('[data-xi-confirm]').checked = true;
  await h.emit('change', '[data-xi-confirm]');
}

test('binding drawer uses a labelled modal, cluster context and exact trigger focus restoration', async () => {
  const h = setup(), { drawer, trigger } = await open(h, 5);
  assert.equal(drawer.tag, 'dialog'); assert.equal(drawer.open, true);
  assert.equal(drawer.getAttribute('aria-modal'), 'true');
  assert.equal(drawer.getAttribute('aria-labelledby'), drawer.querySelector('h3').getAttribute('id'));
  assert.match(drawer.innerHTML, /当前集群：cluster-a/); assert.match(drawer.innerHTML, /Tempo 数据源配置/);
  assert.equal(h.doc.activeElement, drawer.querySelector('h3'));
  await h.emit('click', '[data-xi-close]');
  assert.equal(drawer.hidden, true); assert.equal(drawer.innerHTML, ''); assert.equal(drawer.open, false);
  assert.equal(h.doc.activeElement, trigger); assert.equal(h.doc.listeners.keydown.size, 0);
});

test('Escape, backdrop and native cancellation discard drafts and remove focus handlers', async () => {
  for (const method of ['escape', 'backdrop', 'cancel']) {
    const h = setup(), { drawer, form, trigger } = await open(h);
    form.elements.secret.value = 'draft-secret';
    if (method === 'escape') assert.equal(h.key('Escape').prevented, true);
    if (method === 'backdrop') await h.emit('click', drawer);
    if (method === 'cancel') for (const handler of drawer.listeners.cancel) handler({ preventDefault() {} });
    assert.equal(drawer.innerHTML, ''); assert.equal(form.elements.secret.value, '');
    assert.equal(h.doc.activeElement, trigger); assert.equal(h.doc.listeners.focusin.size, 0);
    assert.equal(h.calls.some(call => /\/datasources$/.test(call.path)), false);
  }
});

test('drawer traps Tab and reverse Tab and excludes controls inside a disabled fieldset', async () => {
  const h = setup(), { drawer, form } = await open(h);
  const close = drawer.querySelector('[data-xi-close]'), testButton = form.querySelector('[data-xi-test]');
  h.key('Tab'); assert.equal(h.doc.activeElement, close);
  h.key('Tab', true); assert.equal(h.doc.activeElement, testButton);
  h.key('Tab'); assert.equal(h.doc.activeElement, close);
  for (const handler of h.doc.listeners.focusin) handler({ target: h.el.querySelector('[data-xi-search]') });
  assert.equal(h.doc.activeElement, close);
  form.querySelector('[data-xi-editable]').disabled = true;
  testButton.disabled = true;
  close.focus(); h.key('Tab', true); assert.equal(h.doc.activeElement, drawer.querySelector('summary'));
});

test('native close event from an earlier drawer cannot close a newly opened modal', async () => {
  const h = setup(); await open(h);
  await h.emit('click', '[data-xi-close]'); await h.emit('click', '[data-xi-edit="5"]');
  const drawer = h.el.querySelector('[data-xi-detail]');
  for (const handler of drawer.listeners.close) handler({});
  assert.equal(drawer.open, true); assert.match(drawer.innerHTML, /Tempo 数据源配置/);
});

test('candidate list is compact while all addresses and escaped names remain in drawer', async () => {
  const h = setup(), { drawer } = await open(h);
  const list = h.el.querySelector('[data-xi-list]').innerHTML;
  assert.match(list, /1 个候选 · 25 个地址/); assert.doesNotMatch(list, /loki-24/);
  assert.match(drawer.innerHTML, /http:\/\/loki-24.svc:3100/);
  assert.match(drawer.innerHTML, /&lt;日志候选&gt;/); assert.match(drawer.innerHTML, /未验证候选/);
  assert.equal(drawer.querySelector('[data-xi-candidate]').value, '');
});

test('search and capability filters preserve stable provider identity and support no results', async () => {
  const h = setup(); await h.window.XSClusterIntegrations.render(h.el);
  assert.deepEqual(Array.from(h.window.XSClusterIntegrations._internals.types), providers);
  for (const [capability, indexes] of [['metrics', ['0']], ['logs', ['1', '2']], ['alerts', ['3']], ['traces', ['4', '5']]]) {
    await h.emit('change', '[data-xi-capability]', capability);
    assert.deepEqual(h.el.querySelectorAll('[data-xi-edit]').map(node => node.getAttribute('data-xi-edit')), indexes);
  }
  await h.emit('change', '[data-xi-capability]', '');
  await h.emit('input', '[data-xi-search]', '  LOKI-24  ');
  assert.deepEqual(h.el.querySelectorAll('[data-xi-edit]').map(node => node.getAttribute('data-xi-edit')), ['1']);
  await h.emit('click', '[data-xi-edit="1"]'); assert.match(h.el.querySelector('[data-xi-detail]').innerHTML, /Loki 数据源配置/);
  await h.emit('click', '[data-xi-close]');
  await h.emit('input', '[data-xi-search]', 'monitoring'); assert.equal(h.el.querySelectorAll('[data-xi-edit]').length, 1);
  await h.emit('input', '[data-xi-search]', 'OpenSearch');
  assert.match(h.el.querySelector('[data-xi-list]').innerHTML, /没有匹配/);
  assert.equal(h.el.querySelector('[data-xi-count]').textContent, '显示 0 / 6 种数据源');
});

test('only real provider options are offered and Prometheus API compatibility is conditional', async () => {
  const h = setup(), { drawer } = await open(h, 0);
  assert.match(drawer.innerHTML, /真实支持所需 HTTP API/);
  assert.match(drawer.innerHTML, /不保证所有兼容实现/);
  assert.doesNotMatch(h.el.innerHTML, /OpenSearch|role="tablist"/);
});

test('closing a pending draft aborts it and late success or failure cannot change another drawer', async () => {
  for (const failure of [false, true]) {
    const pending = deferred(); const h = setup({ api: (path, options) => path.endsWith('/datasources/test') ? pending.promise : api(path, options) });
    await open(h); const testing = h.emit('click', '[data-xi-test]'); await tick();
    const request = h.calls.find(call => call.path.endsWith('/datasources/test'));
    await h.emit('click', '[data-xi-close]'); assert.equal(request.options.signal.aborted, true);
    await h.emit('click', '[data-xi-edit="5"]');
    if (failure) pending.reject(new Error('old failure')); else pending.resolve({ ok: true });
    await testing;
    assert.equal(h.el.querySelector('[data-xi-confirm]').disabled, true);
    assert.equal(h.el.querySelector('[data-xi-status]').textContent, '');
    assert.equal(h.el.querySelector('[data-xi-error]').hidden, true);
    assert.match(h.el.querySelector('[data-xi-detail]').innerHTML, /Tempo 数据源配置/);
  }
});

test('field changes during an async test invalidate both late success and late error', async () => {
  for (const failure of [false, true]) {
    const pending = deferred(), h = setup({ api: (path, options) => path.endsWith('/datasources/test') ? pending.promise : api(path, options) });
    const { form } = await open(h); const testing = h.emit('click', '[data-xi-test]'); await tick();
    await h.emit('input', form.elements.url, 'http://new.svc:3100');
    if (failure) pending.reject(new Error('old failure')); else pending.resolve({ ok: true });
    await testing;
    assert.equal(form.querySelector('[data-xi-save]').disabled, true);
    assert.equal(form.querySelector('[data-xi-confirm]').disabled, true);
    assert.match(h.el.querySelector('[data-xi-status]').textContent, /请重新测试/);
    assert.equal(h.el.querySelector('[data-xi-error]').hidden, true);
  }
});

test('cluster change, destroy, DOM removal and pagehide dispose drawer and in-flight work', async () => {
  for (const action of ['cluster', 'destroy', 'removed', 'pagehide']) {
    const pending = deferred(), h = setup({ api: (path, options) => path.endsWith('/datasources/test') ? pending.promise : api(path, options) });
    const { drawer } = await open(h); const testing = h.emit('click', '[data-xi-test]'); await tick();
    if (action === 'cluster') await h.switchCluster();
    if (action === 'destroy') h.window.XSClusterIntegrations.destroy(h.el);
    if (action === 'removed') { h.el.isConnected = false; h.removed(); }
    if (action === 'pagehide') for (const handler of [...h.doc.listeners['window:pagehide']]) handler();
    assert.equal(drawer.hidden, true); assert.equal(drawer.innerHTML, '');
    assert.equal(h.doc.listeners.keydown.size, 0); assert.equal(h.doc.listeners.focusin.size, 0);
    assert.equal(h.calls.find(call => call.path.endsWith('/datasources/test')).options.signal.aborted, true);
    pending.resolve({ ok: true }); await testing;
    assert.equal(h.el.querySelector('[data-xi-form]'), null);
    if (action !== 'cluster') { assert.equal(h.listeners.size, 0); assert.equal(h.observing, false); }
  }
});

test('save requires test plus confirmation and candidate selection invalidates both', async () => {
  const h = setup(), { form } = await open(h);
  await h.emit('submit', form); assert.match(h.el.querySelector('[data-xi-error]').textContent, /请测试当前配置并确认绑定/);
  await confirm(h, form); assert.equal(form.querySelector('[data-xi-save]').disabled, false);
  await h.emit('change', '[data-xi-candidate]', 'http://loki-24.svc:3100');
  assert.equal(form.elements.url.value, 'http://loki-24.svc:3100');
  assert.equal(form.querySelector('[data-xi-confirm]').disabled, true);
  assert.equal(form.querySelector('[data-xi-save]').disabled, true);
  await h.emit('submit', form); assert.equal(h.calls.some(call => /\/datasources$/.test(call.path)), false);
});

test('existing cluster secrets stay omitted; shared sources never reuse private credentials', async () => {
  for (const scope of ['cluster', 'central']) {
    const datasource = { id: 11, name: '已有日志', type: 'loki', url: 'http://loki.svc:3100', auth_type: 'bearer', has_secret: true, env: 'test', timeout_sec: 30, options: { scope, cluster_id: '7', cluster_name: 'cluster-a' } };
    const h = setup({ api: (path, options) => path.endsWith('/resolve') && options.query.type === 'loki' ? { datasource } : api(path, options) });
    const { form, drawer } = await open(h); form.elements.url.value = 'http://changed.svc:3100';
    await h.emit('click', '[data-xi-test]');
    const request = h.calls.find(call => call.path.endsWith('/datasources/test'));
    if (scope === 'cluster') { assert.equal(request.options.body.datasource_id, 11); assert.equal('secret' in request.options.body, false); assert.match(form.elements.secret.getAttribute('placeholder'), /留空保留/); }
    else { assert.equal(request, undefined); assert.match(h.el.querySelector('[data-xi-error]').textContent, /请填写认证凭据/); assert.doesNotMatch(form.elements.secret.getAttribute('placeholder'), /留空保留/); assert.match(drawer.innerHTML, /不修改共享配置/); }
  }
});

test('pending approvals never claim success or enable repeat save', async () => {
  for (const rejected of [false, true]) {
    const h = setup({ api: (path, options) => {
      if (path.endsWith('/datasources') && options.method === 'POST') { if (rejected) throw { code: 40304 }; return { status: 'pending_approval' }; }
      return api(path, options);
    } });
    const { form } = await open(h); await confirm(h, form); await h.emit('submit', form);
    assert.match(h.el.querySelector('[data-xi-status]').textContent, /等待审批/);
    assert.equal(form.querySelector('[data-xi-save]').disabled, true);
    assert.equal(form.querySelector('[data-xi-confirm]').disabled, true);
    await h.emit('submit', form);
    assert.equal(h.calls.filter(call => call.path.endsWith('/datasources') && call.options.method === 'POST').length, 1);
  }
});

test('closing during save shows uncertainty and late completion cannot write the next drawer', async () => {
  const pending = deferred(), h = setup({ api: (path, options) => path.endsWith('/datasources') && options.method === 'POST' ? pending.promise : api(path, options) });
  const { form } = await open(h); await confirm(h, form);
  const saving = h.emit('submit', form); await tick();
  await h.emit('click', '[data-xi-close]'); assert.match(h.el.querySelector('[data-xi-notice]').textContent, /不代表撤销/);
  await h.emit('click', '[data-xi-edit="5"]'); assert.equal(h.el.querySelector('[data-xi-form]'), null);
  pending.resolve({ id: 12 }); await saving;
  await h.emit('click', '[data-xi-edit="5"]'); assert.match(h.el.querySelector('[data-xi-detail]').innerHTML, /Tempo 数据源配置/);
  assert.equal(h.el.querySelector('[data-xi-status]').textContent, '');
});

test('discovery errors are explained in Chinese without dropping unknown diagnostics', async () => {
  for (const [message, expected] of [['cluster client unavailable', /集群客户端不可用/], ['no matching component evidence in the readable inventory', /未发现匹配组件/], ['component discovery is incomplete: a resource cannot be read', /部分资源无读取权限/], ['component evidence found without a query endpoint', /没有可用的查询地址/], ['services is forbidden: no permission', /无权限.*原始错误：services is forbidden/], ['unexpected upstream detail', /接口请求失败；原始错误：unexpected upstream detail/]]) {
    const h = setup({ api: (path, options) => path.endsWith('/integration-discovery') ? { status: 'partial', last_error: 'discovery inventory incomplete', result: { components: { loki: { state: 'error', error: message } } } } : api(path, options) });
    await h.window.XSClusterIntegrations.render(h.el);
    assert.match(h.el.querySelector('[data-xi-list]').innerHTML, expected);
    assert.match(h.el.querySelector('[data-xi-discovery-status]').textContent, /部分资源无法读取/);
  }
});

test('drawer CSS uses viewport sizing, internal scroll and full-width mobile layout', () => {
  const css = fs.readFileSync(new URL('../../public/xs-cluster-integrations.css', import.meta.url), 'utf8');
  assert.match(css, /\.xi-detail \{[^}]*position: fixed;[^}]*100dvh/);
  assert.match(css, /\.xi-detail::backdrop/);
  assert.match(css, /\.xi-panel \{[^}]*width: min\(680px, 100%\)/);
  assert.match(css, /\.xi-drawer-body \{[^}]*overflow-y: auto/);
  assert.match(css, /@media \(max-width: 640px\)[\s\S]*\.xi-panel \{ width: 100%/);
});
