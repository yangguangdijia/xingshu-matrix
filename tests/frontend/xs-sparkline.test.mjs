import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { Element, tick } from './xs-resource-pages.harness.mjs';

const source = fs.readFileSync(new URL('../../public/xs-sparkline.js', import.meta.url), 'utf8');

function load({ mode = 'live', api, IntersectionObserver } = {}) {
  const calls = [];
  const window = {
    XS: { mode, api: async (path, options = {}) => { calls.push({ path, options }); return api ? api(path, options) : { source: 'none', items: [] }; } },
    IntersectionObserver
  };
  const ctx = vm.createContext({ window, console, Date, JSON, Math, Promise });
  vm.runInContext(source, ctx);
  return { S: window.XSSpark, window, calls };
}

function table(spark, keys, extraCol = true) {
  const el = new Element('div');
  el.innerHTML = '<table><thead><tr><th>名称</th>' + (extraCol ? '<th>近 30 分钟</th>' : '') + '</tr></thead><tbody>' +
    keys.map((k) => '<tr><td>' + k.name + '</td><td>' + spark.placeholder(k.name, { ip: k.ip }) + '</td></tr>').join('') + '</tbody></table>';
  return el;
}

const series = (vals, t0 = 1700000000) => vals.map((v, i) => [t0 + i * 60, v]);

test('svg draws a polyline sized 80x24 by default and escapes the color', () => {
  const { S } = load();
  const out = S.svg(series([1, 3, 2]), { color: '#2563eb' });
  assert.match(out, /^<svg[^>]+width="80" height="24"/);
  assert.match(out, /<path d="M[\d.]+,[\d.]+L[\d.]+,[\d.]+L[\d.]+,[\d.]+"/);
  assert.match(out, /stroke:#2563eb/);
  assert.doesNotMatch(S.svg(series([1, 2]), { color: '"><script>' }), /<script>/);
});

test('svg returns a dash for empty or invalid data', () => {
  const { S } = load();
  for (const empty of [[], null, undefined, [[1, null], [2, 'NaN']]]) {
    const out = S.svg(empty);
    assert.match(out, /—/);
    assert.doesNotMatch(out, /<svg/);
  }
});

test('svg breaks the line on gaps and handles a single point / flat data', () => {
  const { S } = load();
  const gap = S.svg(series([1, null, 3, 4]));
  assert.equal((gap.match(/M/g) || []).length, 2);
  assert.match(S.svg([[1, 5]]), /<path d="M2,[\d.]+L78,[\d.]+"/);
  assert.match(S.svg(series([0, 0, 0])), /<path d="M/);
  assert.match(S.svg([4, 5, 6], { w: 40, h: 10 }), /width="40" height="10"/);
});

test('format renders percent, cores and bytes in Chinese-friendly units', () => {
  const { S } = load();
  assert.equal(S.format(12.34, 'percent'), '12.3%');
  assert.equal(S.format(0.25, 'cores'), '250m');
  assert.equal(S.format(1.5, 'cores'), '1.5 核');
  assert.equal(S.format(1048576 * 300, 'bytes'), '300 Mi');
  assert.equal(S.format(null, 'bytes'), '—');
});

test('cell colors percent lines by threshold and shows dash when no samples', () => {
  const { S } = load();
  const hot = S.cell({ cpu: series([50, 90]), mem: series([10, 20]) }, { cpu: 'percent', mem: 'percent' });
  assert.match(hot, /stroke:#dc2626/);
  assert.match(hot, /CPU 近 30 分钟趋势，当前 90%/);
  assert.match(hot, /内存/);
  assert.match(S.cell({ cpu: [], mem: [] }, {}), /—/);
  assert.match(S.cell(null, {}), /—/);
});

test('placeholder escapes keys', () => {
  const { S } = load();
  assert.match(S.placeholder('ns/a"b', { ip: '10.0.0.1' }), /data-spark="ns\/a&quot;b" data-spark-ip="10.0.0.1"/);
});

test('attach batches one request for all rows and fills cells', async () => {
  const { S, calls } = load({
    api: async (path, options) => ({
      source: 'prometheus', unit: { cpu: 'cores', mem: 'bytes' },
      items: options.query.names.split(',').map((name) => ({ name, cpu: series([0.1, 0.2]), mem: series([1024, 2048]) }))
    })
  });
  const el = table(S, [{ name: 'default/web-0' }, { name: 'default/web-1' }]);
  const r = await S.attach(el, { clusterId: 7, kind: 'pod', apiOptions: { scope: 'cluster' } });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, '/observability/metrics/sparklines');
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0].options.query)), { cluster_id: 7, kind: 'pod', names: 'default/web-0,default/web-1' });
  assert.equal(calls[0].options.scope, 'cluster');
  assert.equal(r.filled, 2);
  for (const c of el.querySelectorAll('[data-spark]')) assert.match(c.innerHTML, /<svg/);
});

test('attach splits more than 50 rows into batches and sends workload kind / node ips', async () => {
  const { S, calls } = load({ api: async () => ({ source: 'prometheus', unit: {}, items: [] }) });
  const keys = Array.from({ length: 120 }, (_, i) => ({ name: 'w' + i }));
  await S.attach(table(S, keys), { clusterId: 1, kind: 'workload', workloadKind: 'statefulsets', namespace: 'prod', lazy: false });
  assert.equal(calls.length, 3);
  assert.equal(calls[0].options.query.names.split(',').length, 50);
  assert.equal(calls[0].options.query.workload_kind, 'statefulsets');
  assert.equal(calls[0].options.query.namespace, 'prod');
  const nodes = load({ api: async () => ({ source: 'prometheus', unit: {}, items: [] }) });
  await nodes.S.attach(table(nodes.S, [{ name: 'n1', ip: '10.0.0.1' }, { name: 'n2' }]), { clusterId: 1, kind: 'node' });
  assert.equal(nodes.calls[0].options.query.ips, '10.0.0.1,');
});

test('attach caches results so a 15s table refresh does not refetch', async () => {
  const { S, calls } = load({ api: async (p, o) => ({ source: 'prometheus', unit: { cpu: 'percent', mem: 'percent' }, items: [{ name: 'n1', cpu: series([10, 20]), mem: series([30, 40]) }] }) });
  await S.attach(table(S, [{ name: 'n1' }]), { clusterId: 1, kind: 'node' });
  const again = table(S, [{ name: 'n1' }]);
  await S.attach(again, { clusterId: 1, kind: 'node' });
  assert.equal(calls.length, 1);
  assert.match(again.querySelector('[data-spark]').innerHTML, /<svg/);
});

test('attach degrades silently on API failure', async () => {
  const { S } = load({ api: async () => { throw Object.assign(new Error('boom'), { status: 502 }); } });
  const el = table(S, [{ name: 'a' }]);
  const r = await S.attach(el, { clusterId: 1, kind: 'node' });
  assert.equal(r.filled, 0);
  assert.match(el.querySelector('[data-spark]').innerHTML, /—/);
  assert.doesNotMatch(el.querySelector('[data-spark]').innerHTML, /boom/);
});

test('attach hides the column when the cluster has no monitoring source', async () => {
  const { S } = load({ api: async (p, o) => ({ source: 'none', unit: {}, items: [{ name: 'a', cpu: [], mem: [] }] }) });
  const el = table(S, [{ name: 'a' }]);
  await S.attach(el, { clusterId: 1, kind: 'pod', namespace: 'default' });
  assert.equal(el.querySelectorAll('th')[1].hidden, true);
  assert.equal(el.querySelectorAll('td')[1].hidden, true);
  assert.equal(el.querySelectorAll('th')[0].hidden, false);
});

test('attach does not call the API outside live mode', async () => {
  const { S, calls } = load({ mode: 'demo' });
  const el = table(S, [{ name: 'a' }]);
  const r = await S.attach(el, { clusterId: 1, kind: 'pod', namespace: 'x' });
  assert.equal(calls.length, 0);
  assert.equal(r.skipped, true);
});

test('attach is lazy when IntersectionObserver exists', async () => {
  let observer;
  class IO { constructor(cb) { this.cb = cb; observer = this; } observe() {} disconnect() { this.off = true; } }
  const { S, calls } = load({ IntersectionObserver: IO, api: async () => ({ source: 'prometheus', unit: {}, items: [] }) });
  const p = S.attach(table(S, [{ name: 'a' }]), { clusterId: 1, kind: 'node' });
  await tick();
  assert.equal(calls.length, 0);
  observer.cb([{ isIntersecting: true }]);
  await p;
  assert.equal(calls.length, 1);
  assert.equal(observer.off, true);
});
