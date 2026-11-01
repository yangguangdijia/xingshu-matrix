import test from 'node:test';
import assert from 'node:assert/strict';
import { harness, deferred, tick } from './xs-resource-pages.harness.mjs';

const repo = { id: 10, name: 'charts-internal', url: 'https://charts.example.test/stable', env: 'dev', last_sync_status: 'succeeded' };
const chart = { repo_id: 10, repo_name: 'charts-internal', env: 'dev/east', chart: { name: 'api service', version: '1.2.0+build', app_version: '1.0.0', description: '<internal> API' } };

function marketAPI(path) {
  if (path === '/clusters/7/helm/releases') return { cluster_id: 7, items: [], truncated: false };
  if (path === '/cicd/helm/repos') return [repo];
  if (path === '/cicd/helm/charts') return { items: [chart], install_supported: false };
  throw new Error(path);
}

test('Helm market reads repositories and searchable charts, then routes selection to application creation', async () => {
  const h = harness('xs-helm-apps.js', { permissions: ['cicd:manage'], api: async (path) => marketAPI(path) });
  await h.window.XSHelmApps.render(h.el);
  await h.emit('click', '[data-xh-tab="catalog"]');

  assert.ok(h.calls.some((c) => c.path === '/cicd/helm/repos'));
  const initial = h.calls.find((c) => c.path === '/cicd/helm/charts');
  assert.equal(initial.options.query.q, '');
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /charts-internal/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /dev\/east/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /api service/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /1\.2\.0\+build/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /1\.0\.0/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /&lt;internal&gt; API/);
  assert.match(h.el.querySelector('[data-xh-charts]').innerHTML, /install_supported:false/);
  assert.match(h.el.innerHTML, /无认证 HTTPS index 仓库/);
  assert.match(h.el.innerHTML, /默认仅适配 image\.repository \/ image\.tag/);
  assert.match(h.el.innerHTML, /不保证任意 Chart 可部署/);
  assert.doesNotMatch(h.calls.map((c) => c.path).join('\n'), /\/install(?:\?|$)/);

  const chartCalls = () => h.calls.filter((c) => c.path === '/cicd/helm/charts');
  const before = chartCalls().length;
  await h.emit('input', '[data-xh-market-search]', 'api');
  await h.emit('input', '[data-xh-market-search]', 'api & service');
  assert.equal(chartCalls().length, before, 'chart search is debounced while typing');
  assert.equal(h.timers.size, 1, 'only the latest keystroke keeps a pending search');
  await h.runTimer();
  assert.equal(chartCalls().length, before + 1);
  assert.equal(chartCalls().at(-1).options.query.q, 'api & service');
  await h.emit('click', '[data-xh-use-chart]');
  assert.equal(h.window.location.hash, '#/delivery/apps?helm_chart=api%20service&helm_repo=https%3A%2F%2Fcharts.example.test%2Fstable&helm_version=1.2.0%2Bbuild&helm_env=dev%2Feast');
  h.window.XSHelmApps.destroy(h.el);
});

test('Helm repository writes require cicd:manage and approval never masquerades as success', async () => {
  const writes = [];
  const h = harness('xs-helm-apps.js', { permissions: ['cicd:manage'], api: async (path, options) => {
    if (options.method) {
      writes.push({ path, options });
      if (path.endsWith('/sync')) return { approval_required: true, ticket: { id: 44 }, status: 'pending' };
      return { id: 10 };
    }
    return marketAPI(path);
  } });
  await h.window.XSHelmApps.render(h.el); await h.emit('click', '[data-xh-tab="catalog"]');

  await h.emit('click', '[data-xh-repo-new]');
  const create = h.el.querySelector('[data-xh-repo-form]');
  create.elements.name.value = 'team'; create.elements.url.value = 'http://charts.example.test'; create.elements.env.value = 'prod';
  await h.emit('submit', create);
  assert.equal(writes.length, 0);
  assert.match(h.el.querySelector('[data-xh-market-status]').textContent, /HTTPS/);
  create.elements.url.value = 'https://charts.example.test/team';
  await h.emit('submit', create);
  assert.equal(writes[0].path, '/cicd/helm/repos'); assert.equal(writes[0].options.method, 'POST');
  assert.deepEqual(JSON.parse(JSON.stringify(writes[0].options.body)), { name: 'team', url: 'https://charts.example.test/team', env: 'prod' });

  await h.emit('click', '[data-xh-repo-edit]');
  const edit = h.el.querySelector('[data-xh-repo-form]'); edit.elements.name.value = 'charts-renamed';
  await h.emit('submit', edit);
  assert.equal(writes.at(-1).path, '/cicd/helm/repos/10'); assert.equal(writes.at(-1).options.method, 'PUT');

  await h.emit('click', '[data-xh-repo-delete]'); await h.emit('click', '[data-xh-repo-delete-confirm]');
  assert.equal(writes.at(-1).path, '/cicd/helm/repos/10'); assert.equal(writes.at(-1).options.method, 'DELETE');

  await h.emit('click', '[data-xh-repo-sync]');
  assert.equal(writes.at(-1).path, '/cicd/helm/repos/10/sync'); assert.equal(writes.at(-1).options.method, 'POST');
  assert.match(h.el.querySelector('[data-xh-market-status]').textContent, /等待审批 #44；本页写操作已锁定。审批后请刷新/);
  assert.doesNotMatch(h.el.querySelector('[data-xh-market-status]').textContent, /已同步|成功/);
  assert.equal(h.el.querySelector('[data-xh-repo-sync]').disabled, true);
  const lockedWrites = writes.length;
  await h.emit('click', '[data-xh-repo-sync]');
  assert.equal(writes.length, lockedWrites);
  await h.emit('click', '[data-xh-refresh]');
  assert.equal(h.el.querySelector('[data-xh-repo-sync]').disabled, false);
  await h.emit('click', '[data-xh-repo-sync]');
  assert.equal(writes.length, lockedWrites + 1);

  const denied = harness('xs-helm-apps.js', { permissions: [], api: async (path, options) => { if (options.method) throw new Error('write must not happen'); return marketAPI(path); } });
  await denied.window.XSHelmApps.render(denied.el); await denied.emit('click', '[data-xh-tab="catalog"]');
  assert.equal(denied.el.querySelector('[data-xh-repo-new]').disabled, true);
  assert.equal(denied.el.querySelector('[data-xh-repo-sync]').disabled, true);
  await denied.emit('click', '[data-xh-repo-sync]');
  assert.equal(denied.calls.some((c) => c.options.method), false);
});

test('Helm discards responses superseded by refresh, tab changes, unmount, or cluster changes', async () => {
  const first = deferred(), second = deferred(); let releases = 0;
  const h = harness('xs-helm-apps.js', { api: (path) => {
    if (path === '/clusters/7/helm/releases') return (++releases === 1 ? first : second).promise;
    return marketAPI(path);
  } });
  const rendering = h.window.XSHelmApps.render(h.el); await tick();
  const refreshing = h.emit('click', '[data-xh-refresh]'); second.resolve({ cluster_id: 7, items: [{ name: 'FRESH', namespace: 'n' }] }); await refreshing;
  first.resolve({ cluster_id: 7, items: [{ name: 'STALE-REFRESH', namespace: 'n' }] }); await rendering; await tick();
  assert.match(h.el.querySelector('[data-xh-list]').innerHTML, /FRESH/); assert.doesNotMatch(h.el.innerHTML, /STALE-REFRESH/);

  const charts = deferred();
  const h2 = harness('xs-helm-apps.js', { api: async (path) => {
    if (path === '/clusters/7/helm/releases') return { cluster_id: 7, items: [] };
    if (path === '/cicd/helm/repos') return [repo];
    if (path === '/cicd/helm/charts') return charts.promise;
    throw new Error(path);
  } });
  await h2.window.XSHelmApps.render(h2.el); const opening = h2.emit('click', '[data-xh-tab="catalog"]'); await tick();
  await h2.emit('click', '[data-xh-tab="installed"]'); charts.resolve({ items: [{ ...chart, chart: { ...chart.chart, name: 'STALE-TAB' } }] }); await opening; await tick();
  assert.doesNotMatch(h2.el.innerHTML, /STALE-TAB/);

  const unmounted = deferred();
  const h3 = harness('xs-helm-apps.js', { api: async (path) => path === '/clusters/7/helm/releases' ? { cluster_id: 7, items: [] } : path === '/cicd/helm/repos' ? [repo] : unmounted.promise });
  await h3.window.XSHelmApps.render(h3.el); const pending = h3.emit('click', '[data-xh-tab="catalog"]'); await tick(); h3.window.XSHelmApps.destroy(h3.el);
  unmounted.resolve({ items: [{ ...chart, chart: { ...chart.chart, name: 'STALE-UNMOUNT' } }] }); await pending; await tick();
  assert.equal(h3.el.innerHTML, '');
});

test('closing Helm revision history returns focus to the history button that opened it', async () => {
  const releases = { cluster_id: 7, truncated: false, items: [
    { namespace: 'apps', name: 'web', chart: 'web', chart_version: '1.0.0', status: 'deployed' },
    { namespace: 'apps', name: 'api', chart: 'api', chart_version: '2.0.0', status: 'deployed' }
  ] };
  const h = harness('xs-helm-apps.js', { api: async (path) => {
    if (path === '/clusters/7/helm/releases') return releases;
    if (path.endsWith('/history')) return { items: [{ revision: 1, status: 'deployed', chart: 'api', chart_version: '2.0.0' }] };
    throw new Error(path);
  } });
  await h.window.XSHelmApps.render(h.el);
  await h.emit('click', '[data-xh-view="1"]');
  await h.emit('click', '[data-xh-close]');
  assert.equal(h.el.querySelector('[data-xh-detail]').hidden, true);
  assert.equal(h.el.querySelector('[data-xh-view="1"]').focused, true);
  assert.notEqual(h.el.querySelector('[data-xh-view="0"]').focused, true);
  h.window.XSHelmApps.destroy(h.el);
});
