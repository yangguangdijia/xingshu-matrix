import test from 'node:test';
import assert from 'node:assert/strict';
import { harness } from './xs-resource-pages.harness.mjs';

const component = { component: 'tempo', name: 'tempo', namespace: 'observability', state: 'installed' };
const catalog = { key: 'loki', name: 'Loki', description: 'Log search', supported: true, versions: ['1.0.0'] };

function apiFor(inventory) {
  return async (path) => {
    if (path.endsWith('/components/catalog')) return { items: [catalog] };
    if (path.endsWith('/storage/classes')) return { items: [] };
    if (path.endsWith('/components')) {
      if (inventory instanceof Error) throw inventory;
      return { items: inventory, truncated: false };
    }
    throw new Error(path);
  };
}

test('component inventory is primary and a non-empty successful inventory collapses installable components', async () => {
  const h = harness('xs-component-center.js', { api: apiFor([component]) });
  await h.window.XSComponentCenter.render(h.el);
  assert.match(h.el.innerHTML, /集群组件库存/);
  assert.match(h.el.innerHTML, /<details[^>]*data-xcc-available/);
  assert.match(h.el.innerHTML, /<summary>可安装组件<\/summary>/);
  assert.equal(h.el.querySelector('[data-xcc-available]').open, false);
});

test('component catalog opens only for a confirmed empty inventory, not failed or forbidden inventory', async () => {
  const empty = harness('xs-component-center.js', { api: apiFor([]) });
  await empty.window.XSComponentCenter.render(empty.el);
  assert.equal(empty.el.querySelector('[data-xcc-available]').open, true);
  assert.match(empty.el.querySelector('[data-xcc-inventory]').innerHTML, /未发现组件库存/);

  for (const error of [Object.assign(new Error('offline'), { status: 503 }), Object.assign(new Error('forbidden'), { status: 403 })]) {
    const failed = harness('xs-component-center.js', { api: apiFor(error) });
    await failed.window.XSComponentCenter.render(failed.el);
    assert.equal(failed.el.querySelector('[data-xcc-available]').open, false);
    assert.doesNotMatch(failed.el.querySelector('[data-xcc-inventory]').innerHTML, /未发现组件库存/);
    assert.match(failed.el.querySelector('[data-xcc-inventory]').innerHTML, error.status === 403 ? /无权限读取组件/ : /组件接口失败/);
  }
});

test('component search expands the catalog and clearing search restores the inventory-derived default', async () => {
  for (const [inventory, defaultOpen] of [[[component], false], [[], true]]) {
    const h = harness('xs-component-center.js', { api: apiFor(inventory) });
    await h.window.XSComponentCenter.render(h.el);
    const detail = h.el.querySelector('[data-xcc-available]');
    assert.equal(detail.open, defaultOpen);
    await h.emit('input', '[data-xcc-search]', 'loki');
    assert.equal(detail.open, true);
    assert.match(h.el.querySelector('[data-xcc-catalog]').innerHTML, /Loki/);
    await h.emit('input', '[data-xcc-search]', '');
    assert.equal(detail.open, defaultOpen);
  }
});
