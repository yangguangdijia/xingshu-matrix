// node --test tests/frontend/xs-cmdk.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { Element, tick } from './xs-resource-pages.harness.mjs';

const SRC = fs.readFileSync(new URL('../../public/xs-cmdk.js', import.meta.url), 'utf8');

const PAGES = [
  { t: '集群列表', m: '集群', g: '运维中心', i: 'cube', h: '#/cluster/list' },
  { t: '主机', m: '集群', g: '运维中心', i: 'term', h: '#/cluster/hosts' },
  { t: '应用', m: 'CI/CD', g: '运维中心', i: 'apps', h: '#/delivery/apps' },
  { t: '需求管理', m: '需求管理', g: '研发中心', i: 'list', h: '#/requirements' },
  { t: '登录', m: '登录', g: '运维中心', i: 'x', h: '#/login' },
];

function setup({ mode = 'live', perms = ['cluster:read', 'host:read', 'cicd:read', 'workspace:read', 'devcenter:read', 'cicd:manage', 'host:write', 'workload:read'], api, demo, user = { id: 7, username: 'li' }, store } = {}) {
  const timers = new Map(); let seq = 0;
  const calls = [], navs = [], events = [];
  const ls = store || new Map();
  const hashListeners = [];
  const view = new Element('div');
  view.attrs.id = 'view';
  const window = {
    XS: {
      mode, user,
      can: (p) => perms.includes(p),
      isCancelled: (e) => e?.code === 'XS_REQUEST_CANCELLED',
      get: (path, query, opts) => { calls.push({ path, query, opts }); return (api || (async () => ({ items: [] })))(path, query, opts); },
    },
    localStorage: { getItem: (k) => (ls.has(k) ? ls.get(k) : null), setItem: (k, v) => ls.set(k, String(v)), removeItem: (k) => ls.delete(k) },
    location: { hash: '#/sre' },
    navigator: { platform: 'Win32' },
    setTimeout: (fn, ms) => { const id = ++seq; timers.set(id, { fn, ms }); return id; },
    clearTimeout: (id) => timers.delete(id),
    AbortController,
    Event: class { constructor(type, o = {}) { this.type = type; this.bubbles = !!o.bubbles; } },
    KeyboardEvent: class { constructor(type, o = {}) { this.type = type; this.key = o.key; } },
    addEventListener: (t, fn) => { if (t === 'hashchange') hashListeners.push(fn); },
    console,
    XSKubectl: { open: () => events.push('kubectl') },
    document: {
      getElementById: (id) => (id === 'view' ? view : null),
      createElement: (t) => new Element(t),
      head: new Element('head'),
      dispatchEvent: (e) => events.push(e.type),
    },
  };
  const ctx = vm.createContext({ window, console, Promise, JSON, AbortController, encodeURIComponent, decodeURIComponent });
  vm.runInContext(SRC, ctx);
  const el = new Element('div'), input = new Element('input'), list = new Element('ul');
  el.hidden = false;
  const host = {
    el, input, list,
    pages: () => PAGES,
    navGo: (h) => { navs.push(h); window.location.hash = h; },
    ic: (k) => `<svg data-i="${k}"></svg>`,
    esc: (s) => s,
    closeAll: () => { el.hidden = true; },
    demo: demo || (() => ({})),
  };
  assert.equal(window.XSCmdk.attach(host), true);
  const T = window.XSCmdk._t;
  const flush = async () => { for (let i = 0; i < 5; i++) await tick(); };
  async function runTimers(ms) {
    for (const [id, t] of [...timers]) { if (ms == null || t.ms === ms) { timers.delete(id); await t.fn(); } }
    await flush();
  }
  async function type(v) { input.value = v; input.oninput(); await flush(); }
  function key(k) { let prevented = false; input.onkeydown({ key: k, preventDefault() { prevented = true; } }); return prevented; }
  const texts = () => list.querySelectorAll('a').map((a) => a.querySelector('.xck-t')?.textContent || '');
  const groups = () => list.querySelectorAll('.x-cmdg').map((g) => g.textContent.replace('清除', ''));
  return { window, T, host, el, input, list, calls, navs, events, ls, view, timers, runTimers, type, key, texts, groups, flush, fireHash: () => hashListeners.forEach((f) => f()) };
}

test('空输入：最近访问 + 按权限过滤的常用操作', async () => {
  const s = setup();
  s.window.XSCmdk.onOpen();
  assert.deepEqual(s.groups(), ['常用操作']);
  const t = s.texts();
  assert.ok(t.includes('新建应用') && t.includes('新建流水线') && t.includes('添加主机') && t.includes('打开 kubectl 终端'));
  assert.ok(!t.includes('接入集群'), '无 cluster:manage 不显示接入集群');
  assert.ok(!t.includes('新建企业空间'), '无 workspace:manage 不显示');

  s.window.location.hash = '#/cluster/hosts'; s.fireHash();
  s.window.location.hash = '#/delivery/demo-app'; s.fireHash();
  s.window.XSCmdk.onOpen();
  assert.deepEqual(s.groups(), ['最近访问', '常用操作']);
  assert.deepEqual(s.texts().slice(0, 2), ['应用 / demo-app', '主机']);
});

test('无权限时常用操作为空，退回显示全部页面', () => {
  const s = setup({ perms: [] });
  s.window.XSCmdk.onOpen();
  assert.deepEqual(s.groups(), ['页面']);
});

test('最近访问：去重、最多 8 条、按用户隔离、拒绝非法路由', () => {
  const store = new Map();
  const s = setup({ store });
  for (let i = 0; i < 12; i++) s.T.recentPush({ h: '#/p/' + i, t: '页' + i });
  s.T.recentPush({ h: '#/p/3', t: '页3' });
  s.T.recentPush({ h: 'javascript:alert(1)', t: 'x' });
  s.T.recentPush({ h: '#/login', t: '登录' });
  const r = s.T.recentRead();
  assert.equal(r.length, 8);
  assert.equal(r[0].h, '#/p/3');
  assert.equal(r.filter((x) => x.h === '#/p/3').length, 1);
  assert.ok(r.every((x) => x.h.startsWith('#/p/')));
  const other = setup({ store, user: { id: 9 } });
  assert.equal(other.T.recentRead().length, 0);
  store.set('xs-cmdk-recent:7', '[{"h":"javascript:x","t":"a"},{"h":"#/ok","t":"<b>"}]');
  assert.deepEqual(s.T.recentRead().map((x) => x.h), ['#/ok']);
});

test('输入时 300ms 防抖，只发最后一次；分组、每组最多 5 条、参数正确', async () => {
  const many = Array.from({ length: 8 }, (_, i) => ({ id: i + 1, name: 'web-' + i, env: '生产', status: 'ready' }));
  const s = setup({
    api: async (path) => {
      if (path === '/clusters') return { items: many, total: 8 };
      if (path === '/hosts') return { items: [{ id: 1, name: 'web-host', ip: '10.0.0.1', online: true, enrolled: true }] };
      if (path === '/cicd/applications') return { items: [{ id: 1, name: 'web-shop', envs: [{ name: 'test' }] }, { id: 2, name: 'billing' }] };
      if (path === '/workspaces') return { items: [{ id: 3, name: 'web 团队' }] };
      if (path === '/devcenter/requirements') return { items: [{ id: 11, title: 'web 需求', status: 'doing', priority: 'P1' }] };
      if (path === '/devcenter/bugs') return { items: [] };
      return { items: [] };
    },
  });
  s.window.XSCmdk.onOpen();
  await s.type('w'); await s.type('we'); await s.type('web');
  assert.equal(s.calls.length, 0, '防抖期内不请求');
  assert.equal([...s.timers.values()].filter((t) => t.ms === 300).length, 1, '只保留最后一个防抖定时器');
  assert.ok(s.list.innerHTML.includes('正在搜索资源'));
  await s.runTimers(300);
  const paths = s.calls.map((c) => c.path).sort();
  assert.deepEqual(paths, ['/cicd/applications', '/clusters', '/devcenter/bugs', '/devcenter/requirements', '/hosts', '/workspaces']);
  assert.deepEqual({ ...s.calls.find((c) => c.path === '/clusters').query }, { keyword: 'web', page_size: 5 });
  assert.deepEqual({ ...s.calls.find((c) => c.path === '/hosts').query }, { keyword: 'web', page_size: 5 });
  assert.equal(s.calls.find((c) => c.path === '/devcenter/requirements').query.q, 'web');
  assert.ok(s.calls.find((c) => c.path === '/clusters').opts.signal, '带取消信号');
  assert.deepEqual(s.groups(), ['操作', '集群', '主机', '应用', '企业空间', '需求'], '「Web Shell」命中 kubectl 操作');
  assert.equal(s.T.S.items.filter((i) => i.g === '集群').length, 5);
  assert.deepEqual([...s.T.S.items.filter((i) => i.g === '应用').map((i) => i.t)], ['web-shop'], '应用前端过滤');
  assert.ok(!s.list.innerHTML.includes('正在搜索资源'));
  assert.ok(s.list.innerHTML.includes('#16a34a'), '状态色点');
  // 同一次打开内应用列表只拉一次
  await s.type('webs'); await s.runTimers(300);
  assert.equal(s.calls.filter((c) => c.path === '/cicd/applications').length, 1);
});

test('按权限只搜有权读的资源', async () => {
  const s = setup({ perms: ['host:read'] });
  s.window.XSCmdk.onOpen();
  await s.type('a'); await s.runTimers(300);
  assert.deepEqual(s.calls.map((c) => c.path), ['/hosts']);
});

test('新输入取消旧请求并丢弃过期结果', async () => {
  const pending = [];
  const s = setup({ perms: ['cluster:read'], api: (path, q, opts) => new Promise((resolve) => pending.push({ q, opts, resolve })) });
  s.window.XSCmdk.onOpen();
  await s.type('old'); await s.runTimers(300);
  assert.equal(pending.length, 1);
  await s.type('new');
  assert.equal(pending[0].opts.signal.aborted, true, '旧请求已取消');
  pending[0].resolve({ items: [{ id: 1, name: 'old-cluster' }] });
  await s.flush();
  assert.ok(!s.texts().includes('old-cluster'), '过期结果被丢弃');
  await s.runTimers(300);
  pending[1].resolve({ items: [{ id: 2, name: 'new-cluster' }] });
  await s.flush();
  assert.ok(s.texts().includes('new-cluster'));
});

test('面板关闭后迟到的结果不再渲染', async () => {
  const pending = [];
  const s = setup({ perms: ['cluster:read'], api: () => new Promise((r) => pending.push(r)) });
  s.window.XSCmdk.onOpen();
  await s.type('x'); await s.runTimers(300);
  s.el.hidden = true;
  const before = s.list.innerHTML;
  pending[0]({ items: [{ id: 1, name: 'late' }] });
  await s.flush();
  assert.equal(s.list.innerHTML, before);
});

test('接口失败降级为一行提示，403 静默隐藏', async () => {
  const s = setup({
    perms: ['cluster:read', 'host:read'],
    api: async (path) => { if (path === '/hosts') { const e = new Error('x'); e.status = 500; throw e; } const e = new Error('no'); e.status = 403; throw e; },
  });
  s.window.XSCmdk.onOpen();
  await s.type('zzz'); await s.runTimers(300);
  assert.ok(s.list.innerHTML.includes('主机暂时无法搜索'));
  assert.ok(!s.list.innerHTML.includes('集群暂时无法搜索'));
  assert.ok(s.list.innerHTML.includes('没有匹配的页面或资源'));
});

test('结果中的用户数据被转义', async () => {
  const s = setup({ perms: ['cluster:read'], api: async () => ({ items: [{ id: 1, name: '<img src=x onerror=alert(1)>', env: '"><b>' }] }) });
  s.window.XSCmdk.onOpen();
  await s.type('i'); await s.runTimers(300);
  assert.ok(!s.list.innerHTML.includes('<img'));
  assert.ok(s.list.innerHTML.includes('&lt;img'));
});

test('键盘：上下选择、回车打开资源并记入最近访问、跳列表后自动填筛选', async () => {
  const s = setup({ perms: ['host:read'], api: async () => ({ items: [{ id: 5, name: 'db-01', ip: '10.1.1.5', online: false, enrolled: true }] }) });
  s.window.XSCmdk.onOpen();
  await s.type('主机'); await s.runTimers(300);
  assert.deepEqual(s.groups(), ['页面', '主机']);
  assert.equal(s.T.S.sel, 0);
  assert.equal(s.key('ArrowDown'), true);
  assert.equal(s.T.S.items[s.T.S.sel].t, 'db-01');
  assert.ok(s.list.querySelector('.sel').querySelector('.xck-t').textContent === 'db-01');
  s.key('ArrowDown');
  assert.equal(s.T.S.sel, 0, '循环');
  s.key('ArrowUp');
  s.key('Enter');
  assert.deepEqual(s.navs, ['#/cluster/hosts']);
  // 跳转后页面渲染出搜索框，面板把名称填进去
  const inputEl = new Element('input'); inputEl.setAttribute('data-live-q', ''); const fired = [];
  inputEl.dispatchEvent = (e) => fired.push(e.type);
  s.view.appendChild(inputEl);
  await s.runTimers(100);
  assert.equal(inputEl.value, 'db-01');
  assert.deepEqual(fired, ['input']);
  // 列表页本身不重复记录
  s.fireHash();
  const r = s.T.recentRead();
  assert.equal(r.length, 1);
  assert.equal(r[0].t, '主机 db-01');
  assert.equal(r[0].f.q, 'db-01');
});

test('需求结果打开详情抽屉；研发中心搜索框用回车提交', async () => {
  const s = setup({ perms: ['devcenter:read'], api: async (p) => (p === '/devcenter/requirements' ? { items: [{ id: 42, title: '登录优化', status: 'todo' }] } : { items: [] }) });
  const opened = [];
  s.window.XSDev = { openDetail: (k, id) => opened.push([k, id]) };
  s.window.XSCmdk.onOpen();
  await s.type('登录优化'); await s.runTimers(300);
  const ix = s.T.S.items.findIndex((i) => i.kind === 'req');
  s.T.choose(s.T.S.items[ix]);
  assert.deepEqual(s.navs, ['#/requirements']);
  const q = new Element('input'); q.setAttribute('data-q', ''); s.view.appendChild(q);
  await s.runTimers(100);
  assert.deepEqual(opened, [['requirements', '42']]);
  const fired = []; const inp = new Element('input'); inp.setAttribute('data-q', ''); inp.dispatchEvent = (e) => fired.push(e.type + ':' + (e.key || ''));
  s.T.applyFilter(inp, 'abc');
  assert.deepEqual(fired, ['input:', 'keydown:Enter']);
});

test('用户离开目标页后不再执行后续动作', async () => {
  const s = setup({ perms: ['host:write'] });
  s.window.XSCmdk.onOpen();
  const add = s.T.S.items.find((i) => i.t === '添加主机');
  s.T.choose(add);
  assert.deepEqual(s.navs, ['#/cluster/hosts']);
  s.window.location.hash = '#/sre';
  let clicked = 0; const b = new Element('button'); b.setAttribute('data-act', 'host-add'); b.click = () => clicked++; s.view.appendChild(b);
  await s.runTimers(100);
  assert.equal(clicked, 0);
  assert.equal(s.timers.size, 0, '不再轮询');
});

test('常用操作：跳页并点击已有按钮；kubectl 直接打开', async () => {
  const s = setup();
  s.window.XSCmdk.onOpen();
  s.T.choose(s.T.S.items.find((i) => i.t === '新建应用'));
  let clicked = 0; const b = new Element('button'); b.setAttribute('data-newapp', ''); b.click = () => clicked++; s.view.appendChild(b);
  await s.runTimers(100);
  assert.equal(clicked, 1);
  s.el.hidden = false;
  s.T.choose(s.T.S.items.find((i) => i.t === '打开 kubectl 终端'));
  assert.deepEqual(s.events, ['kubectl']);
  assert.equal(s.el.hidden, true);
});

test('演示模式：不发请求，只搜页面与演示数据', async () => {
  const s = setup({
    mode: 'demo', user: null, perms: [],
    demo: () => ({ clusters: [{ n: 'prod-sh', env: '生产' }], hosts: [{ n: 'prod-db', ip: '1.1.1.1' }], apps: [], reqs: [{ id: 'R1', name: 'prod 缺陷', type: '缺陷' }, { id: 'R2', name: 'prod 需求', type: '需求' }] }),
  });
  s.window.XSCmdk.onOpen();
  const t0 = s.texts();
  assert.deepEqual([...t0], ['接入集群'], '演示模式只给演示可用的操作');
  await s.type('prod');
  assert.equal(s.calls.length, 0);
  assert.equal(s.timers.size, 0);
  assert.deepEqual(s.groups(), ['集群', '主机', '需求', '缺陷']);
  s.T.choose(s.T.S.items.find((i) => i.t === 'prod-sh'));
  assert.deepEqual(s.navs, ['#/cluster/list/prod-sh']);
});

test('没有匹配时显示简洁提示；路由标题解析', async () => {
  const s = setup({ mode: 'demo', user: null, perms: [] });
  s.window.XSCmdk.onOpen();
  await s.type('不存在的东西');
  assert.ok(s.list.innerHTML.includes('没有匹配的页面或资源'));
  assert.deepEqual({ ...s.T.titleFor('#/cluster/hosts') }, { t: '主机', m: '集群' });
  assert.equal(s.T.titleFor('#/delivery/%E5%95%86%E5%9F%8E').t, '应用 / 商城');
  assert.equal(s.T.titleFor('#/nowhere'), null);
  s.window.location.hash = '#/login'; s.fireHash();
  assert.equal(s.T.recentRead().length, 0);
});

test('顶栏按钮按平台显示快捷键', () => {
  const s = setup();
  assert.equal(s.input.placeholder, '搜索页面、资源或操作…');
  assert.equal(s.input.getAttribute('aria-label'), '全局搜索');
});

test('入口始终没出现时给出简短提示并停止轮询', async () => {
  const s = setup({ perms: ['cicd:manage'] });
  const toasts = []; s.window.XS.toast = (m) => toasts.push(m);
  s.window.XSCmdk.onOpen();
  s.T.choose(s.T.S.items.find((i) => i.t === '新建流水线'));
  for (let i = 0; i < 45 && s.timers.size; i++) await s.runTimers(100);
  assert.equal(s.timers.size, 0);
  assert.deepEqual(toasts, ['未能自动打开「新建流水线」，请在页面上点击对应按钮']);
});
