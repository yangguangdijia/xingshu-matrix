import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { harness, deferred, tick } from './xs-resource-pages.harness.mjs';

const sources = ['xs-helm-apps.js', 'xs-crd-page.js', 'xs-project-quota.js', 'xs-component-center.js', 'xs-cluster-integrations.js'];
const globals = ['XSHelmApps', 'XSCRDPage', 'XSProjectQuota', 'XSComponentCenter', 'XSClusterIntegrations'];
for (const [i, file] of sources.entries()) {
  test(`${file} exports a disposable page and blocks unselected cluster requests`, async () => {
    const h = harness(file, { selected: false }); await h.window[globals[i]].render(h.el, {});
    assert.equal(h.calls.length, 0); assert.match(h.el.innerHTML, /请选择单个集群/);
    h.window[globals[i]].destroy(h.el); assert.equal(h.listeners.size, 0); assert.equal(h.el.innerHTML, '');
  });
  test(`${file} aborts and discards responses from the previous cluster`, async () => {
    const a = deferred(); const h = harness(file, { api: (path, opts) => path.includes('/clusters/7/') || opts.query?.cluster_id === '7' ? a.promise : Promise.resolve({ items: [], truncated: false }) });
    const render = h.window[globals[i]].render(h.el, {}); await tick(); const signal = h.calls[0].options.signal;
    await h.switchCluster(); assert.equal(signal.aborted, true); a.resolve({ cluster_id: 7, items: [{ name: 'STALE-CLUSTER-DATA', type: 'prometheus' }], truncated: false }); await render; await tick();
    assert.doesNotMatch(h.el.innerHTML, /STALE-CLUSTER-DATA/); for (const n of h.el.querySelectorAll('div')) assert.doesNotMatch(n.innerHTML, /STALE-CLUSTER-DATA/);
    h.window[globals[i]].destroy(h.el);
  });
}

test('Helm renders real releases and requests encoded namespace/release history', async () => {
  const h = harness('xs-helm-apps.js', { api: async (path) => ({ cluster_id: 7, items: [{ name: 'a/<x>', namespace: 'n/s', chart: '<script>', chart_version: '1.2', revision: 3, status: 'deployed', history_count: 2 }], truncated: false }) });
  await h.window.XSHelmApps.render(h.el); assert.match(h.el.querySelector('[data-xh-list]').innerHTML, /&lt;script&gt;/);
  await h.emit('click', '[data-xh-view]'); assert.match(h.calls.at(-1).path, /\/helm\/releases\/n%2Fs\/a%2F%3Cx%3E\/history$/);
  assert.doesNotMatch(h.calls.map((c) => c.path).join(''), /cicd/);
});

test('CRD detail supports projected item envelope, schema and partial-information warning', async () => {
  const item = { name: 'widgets.example.test', group: 'example.test', kind: 'Widget', scope: 'Namespaced', versions: [{ name: 'v1', served: true, storage: true, schema_truncated: true, schema: { type: 'object', required: ['spec'], properties: { spec: { type: 'object', properties: { title: { type: 'string', description: '<unsafe>' } } } } } }], schema_projection: 'structural', conditions: [{ type: 'Established', status: 'True' }] };
  const h = harness('xs-crd-page.js', { api: async (path) => path.endsWith('/crds') ? { items: [item] } : { item } });
  await h.window.XSCRDPage.render(h.el); await h.emit('click', '[data-xc-detail]');
  assert.match(h.el.querySelector('[data-xc-panel]').innerHTML, /Widget/); assert.match(h.el.querySelector('[data-xc-panel]').innerHTML, /&lt;unsafe&gt;/);
  assert.match(h.el.querySelector('[data-xc-panel]').innerHTML, /已截断/); assert.match(h.el.querySelector('[data-xc-panel]').innerHTML, /必填/);
});

test('CRD schema rendering bounds deeply nested schemas and escapes constraints', () => {
  const h = harness('xs-crd-page.js'); let schema = { type: 'string', pattern: '<img>' }; for (let i = 0; i < 25; i++) schema = { properties: { nested: schema } };
  assert.match(h.window.XSCRDPage._internals.schemaHTML(schema), /截断/);
  assert.match(h.window.XSCRDPage._internals.schemaHTML({ type: 'string', pattern: '<img>' }), /&lt;img&gt;/);
});

const project = { name: 'apps', phase: 'Active', resource_quotas: [{ name: 'budget', uid: 'quota-uid', resource_version: '12', hard: { 'requests.cpu': '2' }, used: { 'requests.cpu': '1' }, scopes: ['NotBestEffort'] }], limit_ranges: [] };
async function quotaForm(h) { await h.window.XSProjectQuota.render(h.el); await h.emit('click', '[data-xq-resource="0"]'); const form = h.el.querySelector('[data-xq-form]'); form.elements.reason.value = 'Capacity review'; return form; }
test('quota edit uses namespace permission, exact PATCH/spec contract and explicit confirmation', async () => {
  const h = harness('xs-project-quota.js', { api: async (_path, opts) => opts.method === 'PATCH' ? { status: 'applied' } : { items: [project] } }); const form = await quotaForm(h);
  await h.emit('submit', form); assert.equal(h.calls.filter((c) => c.options.method === 'PATCH').length, 0);
  form.querySelector('[data-xq-confirm]').checked = true; await h.emit('submit', form);
  const call = h.calls.find((c) => c.options.method === 'PATCH'); assert.equal(call.path, '/clusters/7/quotas/apps/budget'); assert.equal(call.options.body.resource_version, '12'); assert.equal(call.options.body.uid, 'quota-uid'); assert.equal(call.options.body.spec.hard['requests.cpu'], '2'); assert.equal(call.options.body.spec.scopes[0], 'NotBestEffort'); assert.equal(call.options.query.reason, 'Capacity review'); assert.equal(call.options.body.reason, undefined);
});
test('quota denies guessed config:write and retains pending approval without success', async () => {
  const read = harness('xs-project-quota.js', { permissions: ['config:write'], api: async () => ({ items: [project] }) }); await read.window.XSProjectQuota.render(read.el); assert.equal(read.el.querySelector('[data-xq-create]'), null);
  const h = harness('xs-project-quota.js', { api: async (_path, opts) => { if (opts.method === 'PATCH') throw { code: 40304, details: { ticket_id: 51 } }; return { items: [project] }; } }); const form = await quotaForm(h); await h.emit('submit', form); form.querySelector('[data-xq-confirm]').checked = true; await h.emit('submit', form);
  assert.match(h.el.querySelector('[data-xq-status]').textContent, /等待审批 51，尚未应用/); assert.equal(form.querySelector('[data-xq-submit]').disabled, true);
});
test('quota form rejects duplicate/invalid quantities and missing edit preconditions', async () => {
  const h = harness('xs-project-quota.js', { api: async () => ({ items: [project] }) }); const form = await quotaForm(h);
  form.querySelector('[data-xq-value]').value = '-1'; await h.emit('submit', form); assert.match(h.el.querySelector('[data-xq-error]').textContent, /非负/);
  form.querySelector('[data-xq-value]').value = '2'; await h.emit('click', '[data-xq-add]'); const rows = form.querySelectorAll('[data-xq-pair]'); rows[1].querySelector('[data-xq-key]').value = 'requests.cpu'; rows[1].querySelector('[data-xq-value]').value = '1'; await h.emit('submit', form); assert.match(h.el.querySelector('[data-xq-error]').textContent, /重复/);
});
test('partial quota permission is not represented as an empty configuration', async () => {
  const h = harness('xs-project-quota.js', { api: async () => ({ items: [{ ...project, resource_quotas: [], quota_read: { state: 'forbidden', complete: false } }] }) }); await h.window.XSProjectQuota.render(h.el);
  assert.match(h.el.querySelector('[data-xq-list]').innerHTML, /无权限读取/); assert.equal(h.el.querySelector('[data-xq-kind="quota"]').disabled, true);
});
test('project creation submits labels as fields and reason as audit query', async () => {
  const h = harness('xs-project-quota.js', { api: async (_p, opts) => opts.method === 'POST' ? { status: 'applied' } : { items: [] } });
  await h.window.XSProjectQuota.render(h.el); await h.emit('click', '[data-xq-create]'); const form = h.el.querySelector('[data-xq-form]'); form.elements.resource_name.value = 'team-a'; form.elements.reason.value = 'New team';
  await h.emit('click', '[data-xq-add]'); form.querySelector('[data-xq-key]').value = 'team'; form.querySelector('[data-xq-value]').value = 'a';
  await h.emit('submit', form); form.querySelector('[data-xq-confirm]').checked = true; await h.emit('submit', form);
  const call = h.calls.find((c) => c.options.method === 'POST'); assert.equal(call.path, '/clusters/7/projects'); assert.equal(call.options.body.labels.team, 'a'); assert.equal(call.options.body.name, 'team-a'); assert.equal(call.options.query.reason, 'New team');
});
test('LimitRange supports multiple editable entries and exact PATCH spec', async () => {
  const limit = { name: 'defaults', uid: 'limit-uid', resource_version: '9', limits: [{ type: 'Container', defaultRequest: { cpu: '100m' }, max: { cpu: '2' } }, { type: 'Pod', max: { memory: '4Gi' } }] };
  const h = harness('xs-project-quota.js', { api: async (_p, opts) => opts.method === 'PATCH' ? { status: 'applied' } : { items: [{ ...project, resource_quotas: [], limit_ranges: [limit] }] } }); await h.window.XSProjectQuota.render(h.el); await h.emit('click', '[data-xq-resource="0"]');
  const form = h.el.querySelector('[data-xq-form]'); form.elements.reason.value = 'Preserve all entries'; assert.equal(form.querySelectorAll('[data-xq-limit]').length, 2); await h.emit('submit', form); form.querySelector('[data-xq-confirm]').checked = true; await h.emit('submit', form);
  const call = h.calls.find((c) => c.options.method === 'PATCH'); assert.equal(call.path, '/clusters/7/limitranges/apps/defaults'); assert.equal(call.options.body.spec.limits.length, 2); assert.equal(call.options.body.spec.limits[1].max.memory, '4Gi'); assert.equal(call.options.body.spec.limits[0].defaultRequest.cpu, '100m');
});
test('quota edits require concurrency preconditions and AI fails closed without a model', async () => {
  const invalid = { ...project, resource_quotas: [{ ...project.resource_quotas[0], uid: '', resource_version: '' }] };
  const h = harness('xs-project-quota.js', { permissions: ['namespace:write', 'ai:chat'], api: async (p) => p === '/ai/models/options' ? { items: [] } : { items: [invalid] } }); const form = await quotaForm(h); await h.emit('submit', form); assert.match(h.el.querySelector('[data-xq-error]').textContent, /缺少资源版本或 UID/);
  form.elements.instruction.value = 'Set CPU to 2'; await h.emit('click', '[data-xq-ai]'); assert.match(h.el.querySelector('[data-xq-ai-status]').textContent, /未配置可用 AI 模型/); assert.equal(h.calls.some((c) => c.path === '/ai/assist'), false);
});

const catalog = { key: 'tempo', name: 'Tempo', category: 'observability', supported: true, versions: ['1.10.3'] };
function componentAPI(path, opts) { if (path.endsWith('/storage/classes')) return { items: [{ name: 'fast-csi' }] }; if (path.endsWith('/components/catalog')) return { items: [catalog] }; if (path.endsWith('/components')) return { items: [], truncated: false }; if (path.endsWith('/preflight')) return { ready: true, supported: true, checks: [{ name: 'RBAC', ok: true }] }; if (path.endsWith('/install')) return { operation: { id: 'op1', status: 'queued' } }; if (path.endsWith('/operations/op1')) return { id: 'op1', status: 'running', logs: ['<run>'], progress: 20 }; throw new Error(path); }
async function componentForm(h) { await h.window.XSComponentCenter.render(h.el); await h.emit('click', '[data-xcc-install]'); const form = h.el.querySelector('[data-xcc-form]'); form.elements.namespace.value = 'observability'; form.elements.release.value = 'tempo'; form.elements.version.value = '1.10.3'; form.elements.reason.value = 'Enable traces'; return form; }
test('component install follows real catalog, preflight and confirmed operation contract', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => componentAPI(p, o) }); const form = await componentForm(h); assert.ok(form);
  form.elements.storage_class.value = 'fast-csi'; form.elements.storage_size.value = '12Gi';
  form.elements.cpu_request.value = '200m'; form.elements.cpu_limit.value = '1';
  form.elements.memory_request.value = '128Mi'; form.elements.memory_limit.value = '1Gi';
  form.elements.reason.value = '  Enable <traces> & audit  ';
  await h.emit('submit', form); assert.equal(h.calls.filter((c) => c.path.endsWith('/install')).length, 0); assert.match(form.querySelector('[data-xcc-preflight]').innerHTML, /预检通过/);
  const preflight = h.calls.find((c) => c.path.endsWith('/preflight'));
  assert.equal(preflight.options.query.reason, 'Enable <traces> & audit');
  assert.equal(preflight.options.body.reason, undefined);
  const review = form.querySelector('[data-xcc-preflight]');
  assert.deepEqual(review.querySelectorAll('dt').map((n) => n.textContent), ['组件', '环境', 'Namespace', 'Release', '版本', 'StorageClass', '存储容量', 'CPU request', 'CPU limit', 'Memory request', 'Memory limit', '暴露方式', '变更原因']);
  assert.deepEqual(review.querySelectorAll('dd').map((n) => n.textContent), ['Tempo', 'test', 'observability', 'tempo', '1.10.3', 'fast-csi', '12Gi', '200m', '1', '128Mi', '1Gi', 'ClusterIP', 'Enable <traces> & audit']);
  assert.match(review.innerHTML, /Enable &lt;traces&gt; &amp; audit/);
  assert.equal(review.querySelector('pre'), null);
  assert.equal(form.querySelector('textarea'), null);
  assert.doesNotMatch(review.innerHTML, /"release_name"|"values"|"environment"|persistence\.size/);
  form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
  const call = h.calls.find((c) => c.path.endsWith('/install')); assert.equal(call.options.body.confirm, true); assert.equal(call.options.body.environment, 'test'); assert.equal(call.options.body.release_name, 'tempo'); assert.ok(call.options.body.values);
  assert.equal(call.options.query.reason, preflight.options.query.reason); assert.equal(call.options.body.reason, undefined);
  assert.deepEqual(JSON.parse(JSON.stringify(call.options.body.values)), JSON.parse(JSON.stringify(preflight.options.body.values)));
  assert.equal(call.options.body.values['resources.requests.cpu'], '200m');
  assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装中/); assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /&lt;run&gt;/); assert.doesNotMatch(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/);
  h.window.XSComponentCenter.destroy(h.el); assert.equal(h.timers.size, 0);
});
test('component preflight failure and changed fields block installation', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/preflight') ? { ready: false, conflicts: ['RBAC denied'], checks: [{ name: 'RBAC', ok: false }] } : componentAPI(p, o) }); const form = await componentForm(h); await h.emit('submit', form); assert.match(form.querySelector('[data-xcc-preflight]').innerHTML, /RBAC denied/); assert.equal(form.querySelector('[data-xcc-confirm-wrap]').hidden, true); assert.equal(h.calls.some((c) => c.path.endsWith('/install')), false);
  const h2 = harness('xs-component-center.js', { api: async (p, o) => componentAPI(p, o) }); const f2 = await componentForm(h2); await h2.emit('submit', f2); await h2.emit('input', f2.elements.release, 'tempo-new'); await h2.emit('submit', f2); assert.equal(h2.calls.filter((c) => c.path.endsWith('/preflight')).length, 2); assert.equal(h2.calls.some((c) => c.path.endsWith('/install')), false);
});
test('component approval acceptance is not an installed result', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => { if (p.endsWith('/install')) throw { code: 40304, details: { ticket_id: 90 } }; return componentAPI(p, o); } }); const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); assert.match(h.el.querySelector('[data-xcc-status]').textContent, /等待审批 #90，尚未安装/); assert.equal(form.querySelector('[data-xcc-submit]').disabled, true);
});
test('component reason changes invalidate preflight and require a fresh explicit confirmation', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => componentAPI(p, o) }); const form = await componentForm(h);
  await h.emit('submit', form); const confirm = form.querySelector('[data-xcc-confirm]'); confirm.checked = true;
  await h.emit('input', form.elements.reason, 'Capacity & traces / 复核');
  assert.equal(confirm.checked, false); assert.equal(form.querySelector('[data-xcc-preflight]').hidden, true);
  await h.emit('submit', form); assert.equal(h.calls.filter((c) => c.path.endsWith('/preflight')).length, 2);
  assert.equal(confirm.checked, false); assert.equal(h.calls.some((c) => c.path.endsWith('/install')), false);
  await h.emit('submit', form); assert.match(h.el.querySelector('[data-xcc-error]').textContent, /请确认/);
  confirm.checked = true; await h.emit('submit', form);
  assert.equal(h.calls.find((c) => c.path.endsWith('/install')).options.query.reason, 'Capacity & traces / 复核');
  h.window.XSComponentCenter.destroy(h.el);
});
test('component reason length is checked before preflight', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => componentAPI(p, o) }); const form = await componentForm(h);
  form.elements.reason.value = 'x'.repeat(501); await h.emit('submit', form);
  assert.match(h.el.querySelector('[data-xcc-error]').textContent, /不能超过 500/);
  assert.equal(h.calls.some((c) => c.path.endsWith('/preflight')), false);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component reason is URL-encoded by the real XS API client', async () => {
  const apiSource = fs.readFileSync(new URL('../../public/xs-api.js', import.meta.url), 'utf8');
  const storage = { getItem: () => null, setItem() {}, removeItem() {} }, requests = [];
  const window = { XSClusterContext: { get: () => ({ kind: 'cluster', cluster_id: '7', version: 1 }), isCurrent: () => true, subscribe: () => () => {} } };
  const context = vm.createContext({
    window, localStorage: storage, navigator: { onLine: true },
    location: { protocol: 'https:', hostname: 'matrix.example.test', port: '', href: 'https://matrix.example.test/' },
    URLSearchParams, AbortController, Response, Headers, Request, FormData, TextDecoder, console,
    fetch: async (url) => { requests.push(String(url)); return new Response(JSON.stringify({ code: 0, data: { ready: true } }), { status: 200, headers: { 'content-type': 'application/json' } }); },
    setTimeout, clearTimeout
  });
  context.globalThis = context;
  vm.runInContext(apiSource, context, { filename: 'xs-api.js' });
  await window.XS.ready;
  const reason = 'Capacity & traces / 复核';
  await window.XS.api('/clusters/7/components/tempo/preflight', { scope: 'cluster', method: 'POST', body: { namespace: 'observability' }, query: { reason } });
  const url = requests.find((entry) => entry.includes('/components/tempo/preflight'));
  assert.ok(url);
  assert.equal(new URL(url, 'https://matrix.example.test').searchParams.get('reason'), reason);
  assert.equal(new URL(url, 'https://matrix.example.test').searchParams.get('cluster_id'), '7');
});
test('component approval envelopes show ticket identity without polling the ticket as an operation', async () => {
  for (const throws of [false, true]) {
    const data = { approval_required: true, ticket: { id: 90, status: 'pending' } };
    const h = harness('xs-component-center.js', { api: async (p, o) => {
      if (p.endsWith('/install')) { if (throws) throw { code: 40304, details: data, approval: data }; return data; }
      return componentAPI(p, o);
    } });
    const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form);
    assert.match(h.el.querySelector('[data-xcc-status]').textContent, /等待审批 #90，尚未安装/);
    assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /审批 #90/);
    assert.equal(h.calls.some((c) => c.path.includes('/operations/')), false); assert.equal(h.timers.size, 0);
    assert.equal(form.querySelector('[data-xcc-submit]').disabled, true);
    await h.emit('submit', form); assert.equal(h.calls.filter((c) => c.path.endsWith('/install')).length, 1);
    h.window.XSComponentCenter.destroy(h.el);
  }
});
test('component approval polls the operation through approval and execution to read-back success', async () => {
  for (const throws of [false, true]) {
    const sequence = ['pending_approval', 'approved', 'running', 'succeeded'];
    const data = { operation_id: 'op1', approval_required: true, ticket: { id: 90, status: 'pending' } };
    const h = harness('xs-component-center.js', { api: async (p, o) => {
      if (p.endsWith('/install')) { if (throws) throw { code: 40304, details: data, approval: data }; return data; }
      if (p.endsWith('/operations/op1')) return { id: 'op1', cluster_id: 7, status: sequence.shift(), approval_id: 90 };
      return componentAPI(p, o);
    } });
    const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
    assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /等待审批/);
    await h.runTimer(); assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /审批通过，等待执行/);
    await h.runTimer(); assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装中/);
    assert.doesNotMatch(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/);
    await h.runTimer(); await tick(); assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/);
    assert.equal(h.timers.size, 0); assert.equal(form.querySelector('[data-xcc-submit]').disabled, true);
    assert.equal(h.calls.filter((c) => c.path.endsWith('/components')).length, 2, 'terminal state refreshes inventory');
    assert.equal(h.calls.some((c) => c.path.endsWith('/operations/90')), false);
    h.window.XSComponentCenter.destroy(h.el);
  }
});
test('component rejected/cancelled/failed operations terminate polling without installation success', async () => {
  for (const [status, label] of [['rejected', '审批拒绝'], ['cancelled', '已取消'], ['failed', '安装失败']]) {
    const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/operations/op1') ? { id: 'op1', status, message: '<review>' } : componentAPI(p, o) });
    const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
    const panel = h.el.querySelector('[data-xcc-operation]'); assert.ok(panel.innerHTML.includes(label)); assert.match(panel.innerHTML, /&lt;review&gt;/);
    assert.doesNotMatch(panel.innerHTML, /安装完成/); assert.equal(h.timers.size, 0);
    assert.equal(form.querySelector('[data-xcc-submit]').disabled, true); h.window.XSComponentCenter.destroy(h.el);
  }
});
test('component audit-pending readback stops polling with its truthful deployed state', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/operations/op1') ? { id: 'op1', status: 'audit_pending' } : componentAPI(p, o) });
  const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
  assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装已部署，审计待核对/);
  assert.equal(h.timers.size, 0); assert.equal(form.querySelector('[data-xcc-submit]').disabled, true);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component operation read failure or missing ID remains unconfirmed and prevents duplicate submission', async () => {
  for (const missing of [false, true]) {
    const h = harness('xs-component-center.js', { api: async (p, o) => {
      if (p.endsWith('/install') && missing) return { status: 'accepted' };
      if (p.endsWith('/operations/op1')) throw { status: 503, message: 'temporarily unavailable' };
      return componentAPI(p, o);
    } });
    const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
    assert.match(h.el.querySelector('[data-xcc-status]').textContent, missing ? /未返回操作编号/ : /操作状态读取失败/);
    assert.doesNotMatch(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/);
    assert.equal(form.querySelector('[data-xcc-submit]').disabled, true); assert.equal(h.timers.size, 0);
    await h.emit('submit', form); assert.equal(h.calls.filter((c) => c.path.endsWith('/install')).length, 1);
    h.window.XSComponentCenter.destroy(h.el);
  }
});
test('component operation response is discarded after switching cluster', async () => {
  const waiting = deferred();
  const h = harness('xs-component-center.js', { api: async (p, o) => p === '/clusters/7/components/operations/op1' ? waiting.promise : componentAPI(p, o) });
  const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form);
  const request = h.calls.find((c) => c.path.endsWith('/operations/op1')); await h.switchCluster(); assert.equal(request.options.signal.aborted, true);
  waiting.resolve({ id: 'op1', status: 'succeeded' }); await tick(); await tick();
  assert.equal(h.el.querySelector('[data-xcc-operation]'), null); assert.equal(h.timers.size, 0);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component operation identity and cluster must match before a successful state is shown', async () => {
  for (const operation of [{ id: 'wrong-operation', status: 'succeeded' }, { id: 'op1', cluster_id: 8, status: 'succeeded' }]) {
    const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/operations/op1') ? { operation } : componentAPI(p, o) });
    const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form); await tick();
    assert.match(h.el.querySelector('[data-xcc-status]').textContent, /不匹配/);
    assert.doesNotMatch(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/);
    assert.equal(h.timers.size, 0); assert.equal(form.querySelector('[data-xcc-submit]').disabled, true);
    h.window.XSComponentCenter.destroy(h.el);
  }
});
test('incomplete or forbidden component inventory never unlocks install', async () => {
  for (const inventory of [{ items: [], truncated: true }, { items: [{ component: 'tempo', state: 'forbidden' }], truncated: false }]) {
    const h = harness('xs-component-center.js', { api: async (p) => p.endsWith('/catalog') ? { items: [catalog] } : inventory }); await h.window.XSComponentCenter.render(h.el); assert.equal(h.el.querySelector('[data-xcc-install]').disabled, true);
  }
});
test('component install status must be read back, not trusted from accepted response', async () => {
  const waiting = deferred(); const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/install') ? { operation: { id: 'op1', status: 'succeeded' } } : p.endsWith('/operations/op1') ? waiting.promise : componentAPI(p, o) }); const form = await componentForm(h); await h.emit('submit', form); form.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', form);
  assert.doesNotMatch(h.el.querySelector('[data-xcc-operation]').innerHTML, /安装完成/); waiting.resolve({ id: 'op1', status: 'unsupported', message: 'No executor configured' }); await tick(); await tick(); assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /当前服务不支持安装/); assert.equal(h.timers.size, 0);
});

const typedTempoCatalog = {
  key: 'tempo', name: 'Tempo', supported: true, installable: true, versions: ['1.24.4'],
  configuration_fields: [
    { key: 'replicas', label: '副本数', type: 'integer', default: '1' },
    { key: 'service.type', label: '暴露方式', type: 'enum', default: 'ClusterIP', options: ['ClusterIP', 'NodePort', 'LoadBalancer'] },
    { key: 'resources.requests.cpu', label: 'CPU request', type: 'quantity', default: '100m' },
    { key: 'persistence.enabled', label: '启用持久化', type: 'boolean', default: 'false' },
    { key: 'persistence.storageClass', label: 'StorageClass', type: 'storageClass' }
  ]
};
async function typedComponentForm(h) {
  await h.window.XSComponentCenter.render(h.el); await h.emit('click', '[data-xcc-install]');
  const form = h.el.querySelector('[data-xcc-form]');
  form.elements.namespace.value = 'obs'; form.elements.release.value = 'tempo'; form.elements.version.value = '1.24.4'; form.elements.reason.value = 'Typed profile';
  return form;
}
function typedComponentAPI(p, o) {
  if (p.endsWith('/components/catalog')) return { items: [typedTempoCatalog] };
  if (p.endsWith('/preflight')) return { ready: true, chart_digest: 'a'.repeat(64) };
  return componentAPI(p, o);
}
test('component literal dotted fields submit every typed setting and bind the reviewed digest', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => typedComponentAPI(p, o) });
  const f = await typedComponentForm(h);
  assert.equal(f.elements['persistence.enabled'].type, 'checkbox');
  assert.equal(f.elements['persistence.enabled'].checked, false);
  assert.equal(f.elements['service.type'].tag, 'select');
  assert.equal(f.elements.replicas.type, 'number');
  f.elements.replicas.value = '2'; f.elements['service.type'].value = 'NodePort'; f.elements['resources.requests.cpu'].value = '200m';
  f.elements['persistence.enabled'].checked = true; f.elements['persistence.storageClass'].value = 'fast-csi';
  await h.emit('submit', f);
  const before = h.calls.find((c) => c.path.endsWith('/preflight'));
  assert.deepEqual(JSON.parse(JSON.stringify(before.options.body.values)), { replicas: '2', 'service.type': 'NodePort', 'resources.requests.cpu': '200m', 'persistence.enabled': 'true', 'persistence.storageClass': 'fast-csi' });
  const review = f.querySelector('[data-xcc-preflight]');
  assert.deepEqual(review.querySelectorAll('dt').map((n) => n.textContent), ['组件', '环境', 'Namespace', 'Release', '版本', '副本数', '暴露方式', 'CPU request', '启用持久化', 'StorageClass', 'Chart SHA-256', '变更原因']);
  assert.ok(review.querySelectorAll('dd').some((n) => n.textContent === 'fast-csi'));
  f.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', f);
  const install = h.calls.find((c) => c.path.endsWith('/install'));
  assert.equal(install.options.body.chart_digest, 'a'.repeat(64));
  assert.deepEqual(JSON.parse(JSON.stringify(install.options.body.values)), JSON.parse(JSON.stringify(before.options.body.values)));
  h.window.XSComponentCenter.destroy(h.el);
});
test('component structured persistence requires complete discovered storage classes', async () => {
  for (const storage of [{ items: [] }, { items: [{ name: 'fast-csi' }], truncated: true }]) {
    const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/storage/classes') ? storage : typedComponentAPI(p, o) });
    const f = await typedComponentForm(h);
    assert.equal(f.elements['persistence.enabled'].disabled, true);
    assert.equal(f.elements['persistence.storageClass'].disabled, true);
    await h.emit('submit', f);
    assert.equal(h.calls.find((c) => c.path.endsWith('/preflight')).options.body.values['persistence.enabled'], 'false');
    h.window.XSComponentCenter.destroy(h.el);
  }
  const h = harness('xs-component-center.js', { api: async (p, o) => typedComponentAPI(p, o) });
  const f = await typedComponentForm(h); f.elements['persistence.enabled'].checked = true;
  await h.emit('submit', f); assert.match(f.querySelector('[data-xcc-error]').textContent, /StorageClass/);
  assert.equal(h.calls.some((c) => c.path.endsWith('/preflight')), false);
  f.elements['persistence.storageClass'].value = 'guessed-class'; await h.emit('submit', f);
  assert.equal(h.calls.some((c) => c.path.endsWith('/preflight')), false);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component structured invalid values and changes invalidate confirmation', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => typedComponentAPI(p, o) });
  const f = await typedComponentForm(h);
  for (const [key, value] of [['replicas', '0'], ['service.type', 'ExternalName'], ['resources.requests.cpu', '0']]) {
    const original = f.elements[key].value; f.elements[key].value = value; await h.emit('submit', f);
    assert.equal(h.calls.some((c) => c.path.endsWith('/preflight')), false);
    f.elements[key].value = original;
  }
  await h.emit('submit', f); f.querySelector('[data-xcc-confirm]').checked = true;
  await h.emit('change', f.elements['service.type'], 'NodePort');
  assert.equal(f.querySelector('[data-xcc-confirm]').checked, false);
  await h.emit('submit', f);
  assert.equal(h.calls.filter((c) => c.path.endsWith('/preflight')).length, 2);
  assert.equal(h.calls.some((c) => c.path.endsWith('/install')), false);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component unknown operation retains identity and stops polling without unlocking resubmission', async () => {
  const h = harness('xs-component-center.js', { api: async (p, o) => p.endsWith('/operations/op1') ? { operation_id: 'op1', status: 'unknown', log_excerpt: 'Inspect Helm release' } : typedComponentAPI(p, o) });
  const f = await typedComponentForm(h); await h.emit('submit', f); f.querySelector('[data-xcc-confirm]').checked = true; await h.emit('submit', f); await tick();
  assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /状态未知/);
  assert.match(h.el.querySelector('[data-xcc-operation]').innerHTML, /Inspect Helm release/);
  assert.equal(h.timers.size, 0); assert.equal(f.querySelector('[data-xcc-submit]').disabled, true);
  h.window.XSComponentCenter.destroy(h.el);
});
test('component AI draft uses exact profile field keys and retains typed booleans', async () => {
  const h = harness('xs-component-center.js', { permissions: ['workload:write', 'ai:chat'], api: async (p, o) => {
    if (p === '/ai/models/options') return { items: [{ id: 1 }] };
    if (p === '/ai/assist') return { fields: { configuration: { replicas: 2, 'service.type': 'NodePort', 'persistence.enabled': true, 'persistence.storageClass': 'fast-csi' } } };
    return typedComponentAPI(p, o);
  } });
  const f = await typedComponentForm(h); f.elements.instruction.value = 'Two replicas with storage';
  await h.emit('click', '[data-xcc-ai]');
  assert.equal(f.elements.replicas.value, '2'); assert.equal(f.elements['persistence.enabled'].checked, true);
  assert.match(h.calls.find((c) => c.path === '/ai/assist').options.body.schema.configuration, /service\.type/);
  await h.emit('submit', f);
  assert.equal(h.calls.find((c) => c.path.endsWith('/preflight')).options.body.values['persistence.enabled'], 'true');
  h.window.XSComponentCenter.destroy(h.el);
});
test('component selection emits a scoped instance and storage fields use discovered classes', async () => {
  const installed = { component: 'tempo', name: 'tempo-0', namespace: 'obs', state: 'installed', kind: 'StatefulSet' }; let selection;
  const h = harness('xs-component-center.js', { api: async (p) => p.endsWith('/components') ? { items: [installed], truncated: false } : componentAPI(p) }); await h.window.XSComponentCenter.render(h.el, { onSelectComponent: (s) => { selection = s; } }); await h.emit('click', '[data-xcc-inspect]'); await h.emit('click', '[data-xcc-select]'); assert.equal(selection.cluster_id, '7'); assert.equal(selection.namespace, 'obs');
  const h2 = harness('xs-component-center.js', { api: async (p) => p.endsWith('/components/catalog') ? { items: [{ key: 'jenkins', supported: true, versions: ['5.7.0'] }] } : p.endsWith('/storage/classes') ? { items: [{ name: 'fast-csi' }] } : { items: [], truncated: false } }); await h2.window.XSComponentCenter.render(h2.el); await h2.emit('click', '[data-xcc-install]'); assert.match(h2.el.querySelector('[data-xcc-detail]').innerHTML, /fast-csi/);
});

const observe = { resolvedDatasource: (data) => { if (!data.datasource || data.datasource.options.cluster_id !== '7') throw new Error('ownership mismatch'); return { ds: data.datasource, scope: 'cluster' }; } };
function integrationAPI(path, opts) { if (path.endsWith('/integration-discovery')) return { result: { items: [{ type: 'loki', name: '<candidate>', namespace: 'obs', url_candidates: ['http://loki.obs.svc:3100'], connection_verified: false }], components: { loki: { state: 'available' } } } }; if (path.endsWith('/components')) return { items: [{ component: 'ingress-nginx', kind: 'Deployment', name: 'ingress-nginx', state: 'installed' }] }; if (path.endsWith('/resolve')) throw { status: 404, message: 'not bound' }; if (path.endsWith('/datasources/test')) return { ok: true, latency_ms: 12 }; if (path.endsWith('/datasources') && opts.method === 'POST') return { id: 10 }; throw new Error(path); }
async function integrationForm(h) { await h.window.XSClusterIntegrations.render(h.el); await h.emit('click', h.el.querySelectorAll('[data-xi-edit]')[1]); const form = h.el.querySelector('[data-xi-form]'); form.elements.url.value = 'http://loki.obs.svc:3100'; return form; }
test('integrations preserve discovered candidate vs reachability and full component entry', async () => {
  const h = harness('xs-cluster-integrations.js', { observe, api: async (p, o) => integrationAPI(p, o) }); const form = await integrationForm(h);
  assert.match(h.el.querySelector('[data-xi-detail]').innerHTML, /未验证候选/); assert.match(h.el.querySelector('[data-xi-components]').innerHTML, /ingress-nginx/); assert.match(h.el.querySelector('[data-xi-components]').innerHTML, /#\/cluster\/components/); assert.doesNotMatch(h.el.innerHTML, /#\/cluster\/helm/);
  assert.equal(form.querySelector('[data-xi-save]').disabled, true); await h.emit('click', '[data-xi-test]'); assert.match(h.el.querySelector('[data-xi-status]').textContent, /测试通过/); assert.equal(form.querySelector('[data-xi-confirm]').disabled, false);
  await h.emit('input', form.elements.url, 'http://changed:3100'); assert.equal(form.querySelector('[data-xi-confirm]').disabled, true); assert.equal(form.querySelector('[data-xi-save]').disabled, true);
});
test('missing draft-test backend fails closed without creating a datasource', async () => {
  const h = harness('xs-cluster-integrations.js', { observe, api: async (p, o) => { if (p.endsWith('/datasources/test')) throw { status: 404 }; return integrationAPI(p, o); } }); const form = await integrationForm(h); await h.emit('click', '[data-xi-test]'); assert.match(h.el.querySelector('[data-xi-error]').textContent, /不支持保存前/); await h.emit('submit', form); assert.equal(h.calls.some((c) => c.path.endsWith('/datasources') && c.options.method === 'POST'), false);
});
test('integration datasource names use the backend 64-character limit before draft testing', async () => {
  const h = harness('xs-cluster-integrations.js', { observe, api: async (p, o) => integrationAPI(p, o) }); const form = await integrationForm(h);
  assert.equal(form.elements.name.getAttribute('maxlength'), '64');
  form.elements.name.value = 'n'.repeat(65); await h.emit('click', '[data-xi-test]');
  assert.match(h.el.querySelector('[data-xi-error]').textContent, /不能超过 64/);
  assert.equal(h.calls.some((c) => c.path.endsWith('/datasources/test')), false);
  form.elements.name.value = 'n'.repeat(64); await h.emit('click', '[data-xi-test]');
  assert.equal(h.calls.find((c) => c.path.endsWith('/datasources/test')).options.body.name.length, 64);
  h.window.XSClusterIntegrations.destroy(h.el);
});
test('integration permission and endpoint guards reject unsafe or cross-cluster sources', async () => {
  const h = harness('xs-cluster-integrations.js', { permissions: ['datasource:read'], observe, api: async (p, o) => integrationAPI(p, o) }); await h.window.XSClusterIntegrations.render(h.el); assert.equal(h.el.querySelector('[data-xi-edit]').disabled, true);
  const valid = h.window.XSClusterIntegrations._internals.endpoint; assert.equal(valid('https://loki.example.com'), true); for (const url of ['javascript:alert(1)', 'http://user:pass@host', 'https://host?a=1', 'https://host/#token']) assert.equal(valid(url), false);
  const h2 = harness('xs-cluster-integrations.js', { observe, api: async (p, o) => p.endsWith('/resolve') ? { datasource: { options: { cluster_id: '8' } } } : integrationAPI(p, o) }); await h2.window.XSClusterIntegrations.render(h2.el); assert.match(h2.el.querySelector('[data-xi-list]').innerHTML, /ownership mismatch/); assert.equal(h2.el.querySelector('[data-xi-edit]').disabled, true);
});
test('integration saves only a tested confirmed configuration and rechecks ownership', async () => {
  let saved; const h = harness('xs-cluster-integrations.js', { observe, api: async (p, o) => { if (p.endsWith('/datasources') && o.method === 'POST') { saved = o.body; return { id: 10 }; } if (p.endsWith('/resolve') && saved && o.query.type === 'loki') return { datasource: { ...saved, id: 10 } }; return integrationAPI(p, o); } }); const form = await integrationForm(h); await h.emit('click', '[data-xi-test]'); form.querySelector('[data-xi-confirm]').checked = true; await h.emit('change', '[data-xi-confirm]'); await h.emit('submit', form); assert.equal(saved.options.cluster_id, '7'); assert.match(h.el.querySelector('[data-xi-status]').textContent, /绑定已回读/);
});
test('all new pages avoid horizontal page tabs, legacy CICD Helm and raw input editors', () => {
  for (const file of sources) { const source = fs.readFileSync(new URL(`../../public/${file}`, import.meta.url), 'utf8'); assert.doesNotMatch(source, /role="tablist"|\/cicd\/helm|config:write|<textarea/); }
});
