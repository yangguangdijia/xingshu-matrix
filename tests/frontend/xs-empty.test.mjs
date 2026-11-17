import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { harness, tick, Element } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;

function loadEmpty() {
  const window = {};
  vm.runInContext(read("../../public/xs-empty.js"), vm.createContext({ window }));
  return window.XSEmpty;
}
const XE = loadEmpty();
const frag = (html) => { const e = new Element("div"); e.innerHTML = html; return e; };

/* ------------------------------------------------------------ 组件本身 */

test("provides every required illustration kind as an inline 120×90 line drawing", () => {
  for (const k of ["pipeline", "app", "release", "canary", "requirement", "bug", "test", "host", "cluster", "search", "error", "generic"]) {
    assert.ok(XE.kinds.includes(k), k);
    const e = frag(XE.html({ kind: k, title: "t" }));
    const svg = e.querySelector("svg");
    assert.ok(svg, k);
    assert.equal(svg.getAttribute("viewBox"), "0 0 120 90");
    assert.equal(svg.getAttribute("width"), "120"); assert.equal(svg.getAttribute("height"), "90");
    assert.equal(svg.getAttribute("fill"), "none");
    assert.equal(svg.getAttribute("aria-hidden"), "true");
    assert.equal(e.querySelector("[data-xse]").getAttribute("data-xse"), k);
  }
  // 互不相同（不是同一张图换名字）
  const arts = XE.kinds.map((k) => XE.art(k));
  assert.equal(new Set(arts).size, arts.length);
  // 不引用外部图片 / 渐变 / 滤镜
  const src = read("../../public/xs-empty.js");
  assert.ok(!/<image|url\(|Gradient|<filter|https?:\/\/(?!www\.w3\.org)/.test(src.replace(/\/\*[\s\S]*?\*\//, "")));
});

test("unknown kind falls back to generic; small size is 80×60", () => {
  const e = frag(XE.html({ kind: "nope", title: "x" }));
  assert.equal(e.querySelector("[data-xse]").getAttribute("data-xse"), "generic");
  const s = frag(XE.html({ kind: "host", title: "x", size: "s" }));
  assert.ok(s.querySelector(".xse--s"));
  assert.equal(s.querySelector("svg").getAttribute("width"), "80");
});

test("escapes title, desc, action text and attributes", () => {
  const html = XE.html({ kind: "app", title: XSS, desc: XSS, actions: [{ text: XSS, act: XSS, attrs: { "data-x": XSS } }] });
  assert.ok(!html.includes("<img"));
  const e = frag(html);
  assert.ok(text(e.querySelector(".xse-title")).includes("<img src=x"));
  assert.ok(text(e.querySelector(".xse-desc")).includes("<img src=x"));
  assert.equal(e.querySelector("button").getAttribute("data-x"), XSS);
});

test("actions: act → button, href → link, primary class, unsafe href dropped, attrs filtered", () => {
  const e = frag(XE.html({ kind: "pipeline", title: "还没有流水线", actions: [
    { text: "从模板创建", act: "new", primary: true, attrs: { "data-jobs-click": "reload", onclick: "alert(1)", "data-xs-hint": "1", style: "x" } },
    { text: "文档", href: "#/cicd/pipeline" },
    { text: "坏链接", href: "javascript:alert(1)" },
    { text: "坏链接2", href: " java\tscript:alert(1)" },
    { text: "外链", href: "//evil.example" }
  ] }));
  const b = e.querySelector('[data-xse-act="new"]');
  assert.equal(b.tag, "button"); assert.equal(b.getAttribute("type"), "button");
  assert.ok(b.getAttribute("class").includes("x-btn--pri"));
  assert.equal(b.getAttribute("data-jobs-click"), "reload");
  assert.ok(!b.hasAttribute("onclick") && !b.hasAttribute("style") && !b.hasAttribute("data-xs-hint"));
  const links = e.querySelectorAll("a");
  assert.equal(links.length, 1); assert.equal(links[0].getAttribute("href"), "#/cicd/pipeline");
  assert.ok(!e.raw.includes("javascript") && !e.raw.includes("evil.example"));
});

test("empty state never carries data-xs-hint (globally hidden attribute)", () => {
  for (const k of XE.kinds) {
    const h = XE.html({ kind: k, title: "t", desc: "d", attrs: { "data-xs-hint": "" }, actions: [{ text: "a", act: "a", attrs: { "data-xs-hint": "" } }] });
    assert.ok(!/data-xs-hint/.test(h), k);
  }
  assert.ok(!/data-xs-hint/.test(read("../../public/xs-empty.css")));
});

test("actionsHTML appends a module's own button after the generated actions", () => {
  const e = frag(XE.html({ kind: "host", title: "添加第一台主机", actions: [{ text: "文档", href: "#/hosts" }], actionsHTML: '<button class="x-btn" data-host-add>添加主机</button>' }));
  const acts = e.querySelector(".xse-acts");
  assert.ok(acts.querySelector("[data-host-add]"));
  assert.equal(acts.children.at(-1).getAttribute("data-host-add"), "");
});

test("bind wires [data-xse-act] buttons to handlers", () => {
  const e = frag(XE.html({ kind: "app", title: "t", actions: [{ text: "新建", act: "new" }, { text: "其它", act: "other" }] }));
  let hit = 0;
  XE.bind(e, { new: () => { hit++; } });
  e.querySelector('[data-xse-act="new"]').onclick({ stopPropagation() {} });
  assert.equal(hit, 1);
  assert.equal(e.querySelector('[data-xse-act="other"]').onclick, undefined);
});

test("tag renders the unified four-colour status labels", () => {
  const cases = { ok: ["正常", "running", "Succeeded", "healthy", true], warn: ["警告", "pending", "Degraded", "paused"], bad: ["异常", "failed", "CrashLoopBackOff", "dead", false], unknown: ["未知", "停用", "disabled", "", null, "whatever"] };
  for (const [k, list] of Object.entries(cases)) for (const s of list) assert.equal(XE.state(s), k, String(s));
  const e = frag(XE.tag("running"));
  const t = e.querySelector("span");
  assert.ok(t.getAttribute("class").includes("xse-tag--ok"));
  assert.equal(t.getAttribute("data-xse-state"), "ok");
  assert.equal(text(t), "正常");
  assert.equal(text(frag(XE.tag("failed", "构建失败")).querySelector("span")), "构建失败");
  assert.ok(!XE.tag("warn", XSS).includes("<img"));
  assert.deepEqual({ ...XE.COLORS }, { ok: "#16a34a", warn: "#d97706", bad: "#dc2626", unknown: "#6b7280" });
  const css = read("../../public/xs-empty.css");
  for (const c of ["#16a34a", "#d97706", "#dc2626", "#6b7280"]) assert.ok(css.includes(c), c);
  assert.ok(!/gradient|box-shadow|filter:|glow/i.test(css));
});

/* ------------------------------------------------------------ 模块接入：有 XSEmpty / 无 XSEmpty 回退 */

function jobsHarness(items = [], withEmpty = true) {
  const h = harness("xs-jobs.js", { permissions: ["setting:read", "setting:manage"], api: async (path, o = {}) => path === "/jobs" ? { items, total: items.length, page: 1, page_size: 20 } : {} });
  if (withEmpty) h.window.XSEmpty = XE;
  return h;
}
test("jobs: empty list uses XSEmpty with a refresh step; filtered list offers clear-filter; falls back without XSEmpty", async () => {
  let h = jobsHarness();
  h.window.XSJobs.render(h.el); await tick(); await tick();
  assert.ok(h.el.querySelector('[data-xse="job"]'));
  assert.ok(text(h.el).includes("暂无后台任务"));
  assert.equal(h.el.querySelector(".xse").querySelector("button").getAttribute("data-jobs-click"), "reload");

  h = jobsHarness([], false);
  h.window.XSJobs.render(h.el); await tick(); await tick();
  assert.ok(!h.el.querySelector(".xse"));
  assert.equal(text(h.el.querySelector(".xjb-empty")), "暂无后台任务");
});

test("jobs: filter with no match → search illustration whose button clears the filter", async () => {
  const h = jobsHarness();
  h.window.XSJobs.render(h.el); await tick(); await tick();
  await h.emit("change", "[data-jobs-status]", "dead"); await tick();
  await h.emit("click", '[data-jobs-click="search"]'); await tick();
  assert.ok(h.calls.some((c) => c.options.query && c.options.query.status === "dead"));
  const btn = h.el.querySelector(".xse") && h.el.querySelector(".xse").querySelector('[data-jobs-click="clear"]');
  assert.ok(h.el.querySelector('[data-xse="search"]'));
  assert.ok(btn);
  const before = h.calls.length;
  await h.emit("click", btn); await tick();
  assert.ok(h.calls.length > before);
  assert.ok(!(h.calls.at(-1).options.query || {}).status);
});

test("workspace: empty list → workspace illustration with create step; no match → clear search; detail tabs get add actions", async () => {
  const data = { workspaces: [], members: [], projects: [], bindings: [] };
  const mk = (withEmpty, ws = []) => {
    const h = harness("xs-workspace.js", {
      permissions: ["workspace:read", "workspace:manage"],
      api: async (path, o = {}) => {
        if (path === "/workspaces") return { items: ws, total: ws.length };
        if (/^\/workspaces\/\d+$/.test(path)) return ws[0];
        if (/\/(members|projects)$/.test(path)) return { items: [], total: 0 };
        if (path.startsWith("/role-bindings")) return { items: [], total: 0 };
        if (path === "/roles") return [];
        return { items: [], total: 0 };
      }
    });
    if (withEmpty) h.window.XSEmpty = XE;
    return h;
  };
  let h = mk(true);
  await h.window.XSWorkspace.render(h.el); await tick();
  assert.ok(h.el.querySelector('[data-xse="workspace"]'));
  assert.ok(h.el.querySelector(".xse").querySelector('[data-wsp-click="new"]'));

  h = mk(false);
  await h.window.XSWorkspace.render(h.el); await tick();
  assert.ok(!h.el.querySelector(".xse"));
  assert.ok(text(h.el.querySelector(".xws-empty")).includes("暂无企业空间"));

  h = mk(true, [{ id: 1, name: "支付中心", description: "", member_count: 0, project_count: 0 }]);
  await h.window.XSWorkspace.render(h.el); await tick();
  await h.emit("input", "[data-wsp-search]", "不存在"); await tick();
  assert.ok(h.el.querySelector('[data-xse="search"]'));
  await h.emit("click", h.el.querySelector(".xse").querySelector('[data-wsp-click="clearSearch"]'));
  assert.ok(h.el.querySelector('[data-wsp-row="1"]'));

  await h.emit("click", h.el.querySelector('[data-wsp-row="1"]').querySelector('[data-wsp-click="open"]')); await tick(); await tick();
  const addMember = h.el.querySelectorAll('[data-wsp-click="addMember"]');
  assert.ok(addMember.some((b) => b.closest(".xse")), "成员空状态带「添加成员」");
  assert.ok(h.el.querySelector('[data-xse="member"]'));
});

test("release canary: empty list shows canary illustration with 发起金丝雀 wired to the module action", async () => {
  const mk = (withEmpty) => {
    const h = harness("xs-release-canary.js", {
      permissions: ["cicd:read", "cicd:deploy"],
      api: async (path) => {
        if (path === "/cicd/applications/3") return { id: 3, name: "web", envs: [{ name: "prod", deploy_mode: "manifest" }] };
        if (path === "/cicd/applications/3/canaries") return [];
        return {};
      }
    });
    if (withEmpty) h.window.XSEmpty = XE;
    return h;
  };
  let h = mk(true);
  h.window.XSReleaseCanary.render(h.el, { appId: 3, env: "prod" }); await tick(); await tick();
  const box = h.el.querySelector('[data-xse="canary"]');
  assert.ok(box);
  const b = box.querySelector('[data-xrc-click="create"]');
  assert.ok(b && text(b) === "发起金丝雀");
  await h.emit("click", b); await tick();
  assert.equal(h.el.querySelector("[data-xrc-view]").getAttribute("data-xrc-view"), "create");

  h = mk(false);
  h.window.XSReleaseCanary.render(h.el, { appId: 3, env: "prod" }); await tick(); await tick();
  assert.equal(text(h.el.querySelector(".xrc-empty")), "还没有金丝雀发布记录。");
});

test("image scan: no scan stage keeps data-isc-noscan and links to the pipeline; fallback keeps the old text", async () => {
  const mk = (withEmpty) => {
    const h = harness("xs-image-scan.js", { permissions: ["cicd:read"], api: async (path) => path === "/cicd/pipelines" ? [{ id: 12, app_id: 3, name: "仅部署", stages: [{ name: "部署", type: "deploy" }] }] : { items: [] } });
    if (withEmpty) h.window.XSEmpty = XE;
    return h;
  };
  let h = mk(true);
  h.window.XSImageScan.render(h.el, { appId: 3 }); for (let i = 0; i < 4; i++) await tick();
  const n = h.el.querySelector("[data-isc-noscan]");
  assert.ok(n && n.getAttribute("data-xse") === "scan");
  assert.equal(n.querySelector("a").getAttribute("href"), "#/cicd/pipeline/12");
  h = mk(false);
  h.window.XSImageScan.render(h.el, { appId: 3 }); for (let i = 0; i < 4; i++) await tick();
  assert.equal(h.el.querySelector("[data-isc-noscan]").tag, "p");
});

test("cicd forms: credential empty state uses XSEmpty and its button opens the credential form", () => {
  const mk = (withEmpty) => {
    const opened = [];
    const window = { XS: {}, XSFormKit: { open: (o) => { opened.push(o); return {}; }, errText: (e) => String(e) } };
    if (withEmpty) window.XSEmpty = XE;
    vm.runInContext(read("../../public/xs-cicd-forms.js"), vm.createContext({ window, console }));
    return { F: window.XSCICDForms, opened };
  };
  let { F, opened } = mk(true);
  const e = frag(F.credentialsPanelHTML([], ""));
  assert.ok(e.querySelector('[data-xse="credential"]'));
  const btns = e.querySelectorAll("[data-cred-new]");
  assert.equal(btns.length, 2);
  F.bindCredentials(e, [], () => {});
  btns[1].onclick();
  assert.equal(opened.length, 1);
  ({ F } = mk(false));
  const f = frag(F.credentialsPanelHTML([], ""));
  assert.ok(text(f.querySelector(".x-cicd-empty")).includes("还没有凭证"));
});

function devHarness(withEmpty, hash = "#/requirements") {
  const opened = [], calls = [];
  const window = {
    XS: { user: { username: "alice" }, get: async (p, q) => { calls.push([p, q]); return { items: [], total: 0 }; }, post: async () => ({}), del: async () => ({}), toast() {} },
    XSFormKit: { open: (o) => { opened.push(o); return {}; } }
  };
  window.window = window;
  if (withEmpty) window.XSEmpty = XE;
  const location = { hash };
  vm.runInContext(read("../../public/xs-devcenter.js"), vm.createContext({ window, location, console, setTimeout, clearTimeout }));
  return { X: window.XSDev, el: new Element("main"), opened, calls, location };
}
const settle = async () => { for (let i = 0; i < 6; i++) await tick(); };

test("devcenter: empty requirement list → illustration + 新建需求 opens the create form", async () => {
  const d = devHarness(true);
  d.X.render(d.el, "requirements"); await settle();
  const box = d.el.querySelector('[data-xse="requirement"]');
  assert.ok(box);
  assert.ok(text(box).includes("还没有需求"));
  box.querySelector('[data-xse-act="new"]').onclick({ stopPropagation() {} });
  assert.equal(d.opened.length, 1);
});

test("devcenter: filtered list offers 清除筛选; sprints / AI / repos get their own next step; fallback keeps text", async () => {
  let d = devHarness(true);
  d.X.render(d.el, "bugs"); await settle();
  assert.ok(d.el.querySelector('[data-xse="bug"]'));
  const q = d.el.querySelector("[data-q]"); q.value = "zzz"; q.onkeydown({ key: "Enter" }); await settle();
  assert.ok(d.el.querySelector('[data-xse="search"]'));
  d.el.querySelector('[data-xse-act="clear"]').onclick({ stopPropagation() {} }); await settle();
  assert.ok(d.el.querySelector('[data-xse="bug"]'));
  assert.equal(d.calls.filter((c) => c[0] === "/devcenter/bugs").at(-1)[1].q, "");

  d = devHarness(true, "#/project/sprint");
  d.X.render(d.el, "project"); await settle();
  d.el.querySelector('[data-xse-act="new"]').onclick({ stopPropagation() {} });
  assert.equal(d.opened.length, 1);

  d = devHarness(true, "#/coding/repos");
  d.X.render(d.el, "coding"); await settle();
  const a = d.el.querySelector('[data-xse="app"]').querySelector("a");
  assert.equal(a.getAttribute("href"), "#/delivery/apps");

  d = devHarness(false, "#/testing");
  d.X.render(d.el, "testing"); await settle();
  assert.ok(!d.el.querySelector(".xse"));
  assert.ok(text(d.el.querySelector(".xb-empty")).includes("还没有测试用例，点右上角新建。"));
});

test("modules guard XSEmpty with typeof and do not touch files owned by others", () => {
  for (const f of ["xs-devcenter.js", "xs-cicd-forms.js", "xs-release-canary.js", "xs-jobs.js", "xs-image-scan.js", "xs-workspace.js"]) {
    const s = read("../../public/" + f);
    assert.ok(/typeof X (?:!==|===) "undefined"/.test(s), f);
    assert.ok(!/data-xs-hint/.test(s), f);
  }
});
