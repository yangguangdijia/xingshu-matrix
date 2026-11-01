import fs from 'node:fs';
import vm from 'node:vm';

const decode = (v) => String(v).replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
const voidTags = /^(input|br|hr|img|meta|link|wbr)$/;
export class Element {
  constructor(tag = 'div') { this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.value = ''; this.textContent = ''; this.hidden = false; this.disabled = false; this.checked = false; this.isConnected = true; }
  get parentNode() { return this.parent; }
  get type() { return this.attrs.type || ''; }
  setAttribute(k, v) { this.attrs[k] = decode(v); if (k === 'value') this.value = decode(v); if (k === 'hidden') this.hidden = true; if (k === 'disabled') this.disabled = true; if (k === 'checked') this.checked = true; }
  getAttribute(k) { return Object.hasOwn(this.attrs, k) ? this.attrs[k] : null; }
  hasAttribute(k) { return Object.hasOwn(this.attrs, k); }
  appendChild(n) { this.children.push(n); n.parent = this; return n; }
  set innerHTML(html) {
    this.children = []; this.textContent = ''; this.raw = String(html); const stack = [this];
    for (const token of String(html).match(/<!--[^]*?-->|<[^>]+>|[^<]+/g) || []) {
      if (token.startsWith('<!--')) continue;
      if (token.startsWith('</')) { if (stack.length > 1) stack.pop(); continue; }
      if (!token.startsWith('<')) { stack.at(-1).textContent += decode(token); continue; }
      const m = token.match(/^<([\w-]+)([^]*?)\/?\s*>$/); if (!m) continue;
      const n = new Element(m[1]);
      for (const a of m[2].matchAll(/([:\w-]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g)) n.setAttribute(a[1], a[2] ?? a[3] ?? a[4] ?? '');
      stack.at(-1).appendChild(n); if (!voidTags.test(n.tag) && !token.endsWith('/>')) stack.push(n);
    }
    for (const n of this.querySelectorAll('option')) { if (!n.hasAttribute('value')) n.value = n.textContent; }
    for (const n of this.querySelectorAll('select')) { const options = n.querySelectorAll('option'); n.value = (options.find((o) => o.hasAttribute('selected')) || options[0] || {}).value || ''; }
  }
  get innerHTML() { return this.raw || ''; }
  matches(selector) {
    return selector.split(',').some((s) => { s = s.trim(); const m = s.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/); return m ? this.hasAttribute(m[1]) && (m[2] == null || this.getAttribute(m[1]) === m[2]) : s.startsWith('.') ? (this.attrs.class || '').split(/\s+/).includes(s.slice(1)) : this.tag === s; });
  }
  querySelectorAll(selector) { const all = []; const visit = (node) => { for (const n of node.children) { if (n.matches(selector)) all.push(n); visit(n); } }; visit(this); return all; }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { let n = this; while (n) { if (n.matches(selector)) return n; n = n.parent; } return null; }
  contains(node) { let n = node; while (n) { if (n === this) return true; n = n.parent; } return false; }
  get elements() { return Object.fromEntries(this.querySelectorAll('input, select, textarea').filter((n) => n.attrs.name).map((n) => [n.attrs.name, n])); }
  reportValidity() { return this.querySelectorAll('input, select, textarea').every((n) => !n.hasAttribute('required') || !!n.value.trim()); }
  addEventListener(t, fn) { (this.listeners[t] ||= new Set()).add(fn); }
  removeEventListener(t, fn) { this.listeners[t]?.delete(fn); }
  focus() { this.focused = true; }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((n) => n !== this); this.isConnected = false; }
  insertAdjacentHTML(_position, html) { const holder = new Element(); holder.innerHTML = html; for (const n of [...holder.children]) this.appendChild(n); }
}

export const tick = () => new Promise((r) => setImmediate(r));
export function deferred() { let resolve, reject; const promise = new Promise((r, j) => { resolve = r; reject = j; }); return { promise, resolve, reject }; }
export function harness(file, { api = async () => ({ items: [] }), permissions = ['namespace:write', 'workload:write', 'datasource:read', 'datasource:manage'], selected = true, observe } = {}) {
  let snapshot = { kind: selected ? 'cluster' : 'unselected', cluster_id: selected ? '7' : '', name: 'cluster-a', env: 'test', version: 1 };
  const listeners = new Set(), calls = [], timers = new Map(); let timerSeq = 0;
  const window = {
    XS: { can: (p) => permissions.includes(p), isApproval: (e) => e?.code === 40304, api: async (path, options = {}) => { calls.push({ path, options }); return api(path, options); } },
    XSClusterContext: { get: () => ({ ...snapshot }), isCurrent: (s) => s.version === snapshot.version && s.cluster_id === snapshot.cluster_id, subscribe: (fn) => { listeners.add(fn); return () => listeners.delete(fn); } },
    XSObserve: observe
  };
  const context = vm.createContext({ window, AbortController, URL, console, setTimeout: (fn) => { const id = ++timerSeq; timers.set(id, fn); return id; }, clearTimeout: (id) => timers.delete(id) });
  vm.runInContext(fs.readFileSync(new URL(`../../public/${file}`, import.meta.url), 'utf8'), context);
  const el = new Element('main');
  async function emit(type, selector, value) { const target = typeof selector === 'string' ? el.querySelector(selector) : selector; if (!target) throw new Error(`missing target ${selector}`); if (value !== undefined) target.value = value; const event = { target, preventDefault() {} }; for (const fn of [...(el.listeners[type] || [])]) await fn(event); await tick(); return target; }
  return { window, el, calls, timers, emit, async switchCluster(id = '8') { snapshot = { ...snapshot, cluster_id: id, name: 'cluster-' + id, version: snapshot.version + 1 }; for (const fn of [...listeners]) fn(); await tick(); }, async runTimer() { const entry = timers.entries().next().value; if (entry) { timers.delete(entry[0]); await entry[1](); await tick(); } }, listeners };
}
