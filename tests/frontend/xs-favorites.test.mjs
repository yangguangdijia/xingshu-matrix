// node --test tests/frontend/xs-favorites.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { Element } from './xs-resource-pages.harness.mjs';

const SRC = fs.readFileSync(new URL('../../public/xs-favorites.js', import.meta.url), 'utf8');
const CMDK = fs.readFileSync(new URL('../../public/xs-cmdk.js', import.meta.url), 'utf8');

// Element with the few DOM extras the module needs (classList, insertBefore, firstChild)
class El extends Element {
  get classList() {
    const self = this;
    const get = () => (self.attrs.class || '').split(/\s+/).filter(Boolean);
    const set = (l) => { self.attrs.class = l.join(' '); };
    return {
      add: (c) => { const l = get(); if (!l.includes(c)) set([...l, c]); },
      remove: (c) => set(get().filter((x) => x !== c)),
      contains: (c) => get().includes(c),
      toggle: (c, on) => { const has = get().includes(c); const want = on === undefined ? !has : !!on; if (want && !has) set([...get(), c]); if (!want && has) set(get().filter((x) => x !== c)); return want; },
    };
  }
  set className(v) { this.attrs.class = v; }
  get firstChild() { return this.children[0] || null; }
  insertBefore(n, ref) { const i = ref ? this.children.indexOf(ref) : -1; if (i < 0) this.children.push(n); else this.children.splice(i, 0, n); n.parent = this; return n; }
  insertAdjacentHTML(pos, html) {
    const holder = new El(); holder.innerHTML = html;
    if (pos === 'afterend' && this.parent) { for (const n of [...holder.children]) { const p = this.parent; const i = p.children.indexOf(this); p.children.splice(i + 1, 0, n); n.parent = p; } return; }
    for (const n of [...holder.children]) this.appendChild(n);
  }
  set innerHTML(html) {
    super.innerHTML = html;
    // re-type parsed children so they get the extras too
    const fix = (node) => { for (const c of node.children) { Object.setPrototypeOf(c, El.prototype); fix(c); } };
    fix(this);
  }
  get innerHTML() { return super.innerHTML; }
}

function setup({ mode = 'live', user = { id: 7, username: 'li' }, store, hash = '#/cluster/list', crumb = '集群列表', cmdk = false } = {}) {
  const ls = store || new Map();
  const toasts = [], calls = [], listeners = { hashchange: [], storage: [] };
  const top = new El('header');
  const crumbs = new El('nav'); crumbs.attrs.id = 'crumbs'; top.appendChild(crumbs);
  crumbs.innerHTML = `<span>星枢</span><i>/</i><b aria-current="page">${crumb}</b>`;
  const nav = new El('aside'); nav.attrs.id = 'nav';
  nav.innerHTML = '<a class="x-sbrand" href="#/sre">星枢</a><nav class="x-nav" aria-label="运维中心"><a class="x-ni" href="#/cluster/list"><span>集群列表</span></a></nav>';
  const roots = [top, nav];
  const find = (id) => { for (const r of roots) { if (r.attrs.id === id) return r; const hit = r.querySelectorAll('*').find((n) => n.attrs.id === id); if (hit) return hit; } return null; };
  const window = {
    XS: { mode, user, toast: (m) => toasts.push(m), get: (...a) => { calls.push(a); return Promise.resolve({ items: [] }); }, can: () => true },
    localStorage: { getItem: (k) => (ls.has(k) ? ls.get(k) : null), setItem: (k, v) => ls.set(k, String(v)), removeItem: (k) => ls.delete(k) },
    location: { hash },
    addEventListener: (t, fn) => { (listeners[t] ||= []).push(fn); },
    console,
    document: { getElementById: find, createElement: (t) => new El(t), head: new El('head') },
  };
  const ctx = vm.createContext({ window, console, Promise, JSON, AbortController, encodeURIComponent, decodeURIComponent });
  if (cmdk) vm.runInContext(CMDK, ctx);
  vm.runInContext(SRC, ctx);
  const go = (h) => { window.location.hash = h; listeners.hashchange.forEach((f) => f()); };
  const grp = () => nav.querySelector('[data-xf-grp]');
  const star = () => find('xfStar');
  const fire = (type, target, extra = {}) => {
    const ev = { type, target, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() {}, ...extra };
    for (const fn of [...(grp().listeners[type] || [])]) fn(ev);
    return ev;
  };
  return { window, ls, toasts, calls, nav, crumbs, go, grp, star, fire, listeners, XSFav: window.XSFav };
}

// The harness has no '*' selector; add it for getElementById lookups.
const origMatches = Element.prototype.matches;
Element.prototype.matches = function (s) { return s === '*' ? true : origMatches.call(this, s); };

const titles = (fav) => Array.from(fav.list(), (r) => r.t);

test('keys per user like cmdk recent visits; dedupes, validates and caps at 20', () => {
  const s = setup();
  const F = s.XSFav;
  assert.equal(F._t.key(), 'xs-fav:7');
  assert.equal(F.add({ h: '#/cluster/hosts', t: '主机' }), 'added');
  assert.equal(F.add({ h: '#/cluster/hosts', t: '主机' }), 'exists');
  assert.equal(F.add({ h: 'javascript:alert(1)', t: 'x' }), 'invalid');
  assert.equal(F.add({ h: '#/login', t: '登录' }), 'invalid');
  assert.equal(F.add({ h: '#/x"onclick', t: 'x' }), 'invalid');
  assert.equal(F.add({ h: '#/a', t: '' }), 'invalid');
  for (let i = 0; i < 25; i++) F.add({ h: '#/p/' + i, t: '页' + i });
  assert.equal(F.list().length, 20);
  assert.equal(F.add({ h: '#/p/99', t: 'x' }), 'full');
  // corrupted / oversize storage is sanitised on read
  s.ls.set('xs-fav:7', JSON.stringify([{ h: '#/a', t: 'A' }, { h: '#/a', t: 'dup' }, { h: 'http://evil', t: 'bad' }, null, 'x', { h: '#/b', t: 'B', m: 5 }]));
  assert.deepEqual(JSON.parse(JSON.stringify(F.list())), [{ h: '#/a', t: 'A', m: '' }, { h: '#/b', t: 'B', m: '' }]);
  s.ls.set('xs-fav:7', '{not json');
  assert.deepEqual(F.list().length, 0);
});

test('user isolation and demo mode (no requests, local key)', () => {
  const store = new Map();
  const a = setup({ store, user: { id: 1 } });
  a.XSFav.add({ h: '#/sre', t: '总览' });
  const b = setup({ store, user: { username: 'wang' } });
  assert.equal(b.XSFav._t.key(), 'xs-fav:wang');
  assert.equal(b.XSFav.list().length, 0);
  const d = setup({ mode: 'demo', user: null });
  assert.equal(d.XSFav._t.key(), 'xs-fav:local');
  assert.equal(d.XSFav.toggle(), 'added');
  assert.deepEqual(titles(d.XSFav), ['集群列表']);
  assert.equal(d.calls.length, 0, 'demo mode must not call any API');
});

test('header star pins and unpins the current page and reflects state', () => {
  const s = setup();
  const btn = s.star();
  assert.ok(btn, 'star button inserted next to crumbs');
  assert.equal(s.crumbs.parent.children[s.crumbs.parent.children.indexOf(s.crumbs) + 1], btn);
  assert.equal(btn.getAttribute('aria-pressed'), 'false');
  btn.onclick({ preventDefault() {} });
  assert.deepEqual(titles(s.XSFav), ['集群列表']);
  assert.equal(btn.getAttribute('aria-pressed'), 'true');
  assert.equal(btn.getAttribute('aria-label'), '取消收藏当前页面');
  assert.match(s.toasts.at(-1), /已收藏「集群列表」/);
  // sidebar group lists it and marks it active
  const a = s.grp().querySelector('.xf-a');
  assert.equal(a.getAttribute('href'), '#/cluster/list');
  assert.equal(a.getAttribute('aria-current'), 'page');
  // navigating elsewhere: star off, item not active
  s.go('#/cluster/hosts');
  assert.equal(btn.getAttribute('aria-pressed'), 'false');
  assert.equal(s.grp().querySelector('.xf-a').getAttribute('aria-current'), null);
  s.go('#/cluster/list');
  btn.onclick({ preventDefault() {} });
  assert.equal(s.XSFav.list().length, 0);
  assert.match(s.toasts.at(-1), /已取消收藏/);
  assert.ok(s.grp().querySelector('.xf-hint'), 'empty state hint');
  // login page: star hidden
  s.go('#/login');
  assert.equal(btn.hidden, true);
});

test('star toast when full', () => {
  const s = setup({ hash: '#/new/page', crumb: '新页面' });
  for (let i = 0; i < 20; i++) s.XSFav.add({ h: '#/p/' + i, t: '页' + i });
  assert.equal(s.XSFav.toggle(), 'full');
  assert.match(s.toasts.at(-1), /最多收藏 20 个/);
});

test('uses XSCmdk titleFor for detail-page titles when available', () => {
  const s = setup({ hash: '#/delivery/my-app', crumb: '应用' });
  s.window.XSCmdk = { titleFor: (h) => (h === '#/delivery/my-app' ? { t: '应用 / my-app', m: '运维中心' } : null) };
  s.XSFav.toggle();
  assert.deepEqual(JSON.parse(JSON.stringify(s.XSFav.list())), [{ h: '#/delivery/my-app', t: '应用 / my-app', m: '运维中心' }]);
});

test('sidebar group: remove, arrow buttons, Alt+Arrow and drag reorder; titles escaped', () => {
  const s = setup();
  const F = s.XSFav;
  ['A', 'B', 'C'].forEach((t) => F.add({ h: '#/x/' + t, t }));
  F.add({ h: '#/x/evil', t: '<img src=x onerror=1>' });
  assert.ok(!s.grp().innerHTML.includes('<img'), 'title escaped');
  assert.ok(s.grp().querySelector('[data-xf-up="0"]').disabled);
  assert.ok(s.grp().querySelector('[data-xf-down="3"]').disabled);
  s.fire('click', s.grp().querySelector('[data-xf-down="0"]'));
  assert.deepEqual(titles(F).slice(0, 3), ['B', 'A', 'C']);
  s.fire('click', s.grp().querySelector('[data-xf-up="2"]'));
  assert.deepEqual(titles(F).slice(0, 3), ['B', 'C', 'A']);
  s.fire('click', s.grp().querySelector('[data-xf-rm="3"]'));
  assert.deepEqual(titles(F), ['B', 'C', 'A']);
  // keyboard
  const link = s.grp().querySelector('[data-xf-i="2"]').querySelector('.xf-a');
  const ev = s.fire('keydown', link, { altKey: true, key: 'ArrowUp' });
  assert.ok(ev.defaultPrevented);
  assert.deepEqual(titles(F), ['B', 'A', 'C']);
  s.fire('keydown', s.grp().querySelector('[data-xf-i="0"]').querySelector('.xf-a'), { altKey: false, key: 'ArrowDown' });
  assert.deepEqual(titles(F), ['B', 'A', 'C'], 'plain arrows do nothing');
  // drag first onto last
  const dt = { setData() {} };
  s.fire('dragstart', s.grp().querySelector('[data-xf-i="0"]'), { dataTransfer: dt });
  const over = s.fire('dragover', s.grp().querySelector('[data-xf-i="2"]'), { dataTransfer: dt });
  assert.ok(over.defaultPrevented);
  s.fire('drop', s.grp().querySelector('[data-xf-i="2"]'), { dataTransfer: dt });
  s.fire('dragend', s.grp());
  assert.deepEqual(titles(F), ['A', 'C', 'B']);
});

test('remounts after the sidebar is rebuilt and syncs across tabs', () => {
  const s = setup();
  s.XSFav.add({ h: '#/sre', t: '总览' });
  s.nav.innerHTML = '<nav class="x-nav"><a class="x-ni" href="#/sre"><span>总览</span></a></nav>';
  assert.equal(s.grp(), null);
  assert.equal(s.XSFav.mount(), true);
  assert.equal(s.nav.querySelector('.x-nav').children[0], s.grp(), 'group sits at the top of the menu');
  assert.equal(s.grp().querySelectorAll('.xf-it').length, 1);
  // another tab writes storage
  s.ls.set('xs-fav:7', JSON.stringify([{ h: '#/sre', t: '总览' }, { h: '#/cluster/list', t: '集群列表' }]));
  s.listeners.storage.forEach((f) => f({ key: 'xs-fav:7' }));
  assert.equal(s.grp().querySelectorAll('.xf-it').length, 2);
  assert.equal(s.star().getAttribute('aria-pressed'), 'true');
  let seen = null;
  s.XSFav.onChange((l) => { seen = l.length; });
  s.XSFav.remove('#/sre');
  assert.equal(seen, 1);
});

test('xs-cmdk shows favourites first in the empty-input state', (t) => {
  if (!CMDK.includes('XSFav')) { t.skip('xs-cmdk.js not yet patched (apply briefs/v132/p2-favs.patches.py)'); return; }
  const s = setup({ cmdk: true });
  const C = s.window.XSCmdk;
  assert.equal(typeof C.titleFor, 'function');
  s.XSFav.add({ h: '#/cluster/hosts', t: '主机' });
  C._t.recentPush({ h: '#/cluster/hosts', t: '主机' });
  C._t.recentPush({ h: '#/sre', t: '总览' });
  C._t.S.q = '';
  const items = C._t.build();
  assert.equal(items[0].g, '收藏');
  assert.equal(items[0].t, '主机');
  assert.ok(items[0].svg.includes('<svg'));
  const recent = Array.from(items.filter((i) => i.g === '最近访问'), (i) => i.t);
  assert.deepEqual(recent, ['总览'], 'favourite not duplicated under recent visits');
});
