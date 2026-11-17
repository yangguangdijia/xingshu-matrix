import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import { Element, deferred, tick } from "./xs-resource-pages.harness.mjs";

const source = fs.readFileSync(new URL("../../public/xs-platform-settings.js", import.meta.url), "utf8");
const stylesheet = fs.readFileSync(new URL("../../public/xs-platform-settings.css", import.meta.url), "utf8");
const dataURL = (raw, type = "image/svg+xml") => `data:${type};base64,${Buffer.from(raw).toString("base64")}`;
const LOGO = dataURL('<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h10v10H0z"/></svg>');
const INITIAL = { logo: LOGO, primary_color: "#0f7b6c", login_title: "团队研发平台", copyright: "© 团队" };
const plain = (value) => JSON.parse(JSON.stringify(value));
const event = () => ({ preventDefault() {} });

function setup({ initial = INITIAL, manage = true, api, name = "示例平台", confirm = true } = {}) {
  const calls = [], toasts = [], readers = [], confirmations = [], globalCalls = [];
  const head = new Element("head"), el = new Element("main");
  let reloads = 0;
  const location = { hash: "#/platform/appearance", origin: "https://example.test", reload: () => reloads++ };
  const document = {
    head,
    createElement: (tag) => new Element(tag),
    getElementById: (id) => head.children.find((child) => child.id === id) || null
  };
  class Reader {
    constructor() { readers.push(this); }
    readAsDataURL(file) { this.file = file; }
    finish(result) { this.result = result; this.onload(); }
  }
  const window = {
    XS: {
      can: (permission) => manage && permission === "setting:manage",
      api: async (path, options = {}) => {
        calls.push({ path, options });
        if (api) { const result = await api(path, options); if (result !== undefined) return result; }
        if (path === "/settings/appearance") return initial;
        if (path === "/settings/platform.name") return { value: name };
        throw new Error("unexpected " + path);
      },
      toast: (message) => toasts.push(message)
    },
    XSBranding: { reload: () => globalCalls.push("reload"), palette: () => globalCalls.push("palette") },
    atob,
    FileReader: Reader,
    confirm: (message) => { confirmations.push(message); return confirm; }
  };
  vm.runInNewContext(source, { window, document, location, console });
  const query = (selector) => el.querySelector(selector);
  const click = (selector) => query(selector).onclick(event());
  const input = (field, value) => { const control = query("[data-f]").elements[field]; control.value = value; control.oninput.call(control); };
  const upload = (type = "image/svg+xml", size = 100) => { const control = query("[data-file]"); control.files = [{ type, size }]; control.onchange.call(control); };
  return { window, document, el, calls, toasts, readers, confirmations, globalCalls, query, click, input, upload,
    get reloads() { return reloads; },
    async mount() { window.XSPlatformSettings.render(el); await tick(); await tick(); },
    submit() { return query("[data-f]").onsubmit(event()); },
    reset() { return query("[data-f]").onreset(event()); }
  };
}

test("appearance loads read-only data, named theme cards and one local stylesheet without global branding writes", async () => {
  const page = setup();
  await page.mount();
  assert.deepEqual(page.calls.map((call) => call.path).sort(), ["/settings/appearance", "/settings/platform.name"]);
  assert.ok(page.calls.every((call) => !call.options.method));
  assert.equal(page.el.querySelectorAll("[data-preset]").length, 6);
  assert.equal(page.query('[data-preset="#0f7b6c"]').getAttribute("aria-pressed"), "true");
  assert.match(page.query('[data-preset="#1677ff"]').getAttribute("aria-label"), /星枢蓝/);
  assert.equal(page.query("[data-save]").disabled, true);
  assert.equal(page.query("[data-restore]").disabled, true);
  assert.equal(page.query('[role="tablist"]'), null);
  assert.equal(page.query("[data-f]").getAttribute("aria-label"), "品牌与主题设置");
  assert.ok(page.query("[data-preview]").innerHTML.includes("示例平台"));
  const link = page.document.getElementById("xs-cfg-appearance-css");
  assert.equal(link.rel, "stylesheet");
  assert.equal(link.href, "xs-platform-settings.css");
  await page.mount();
  assert.equal(page.document.head.children.filter((child) => child.id === link.id).length, 1);
  assert.equal(page.reloads, 0);
  assert.deepEqual(page.globalCalls, []);
});

test("preset, picker and hex stay synchronized; custom colors change only the local preview", async () => {
  const page = setup();
  await page.mount();
  page.click('[data-preset="#1d39c4"]');
  assert.equal(page.query("[data-f]").elements.primary_color.value, "#1d39c4");
  assert.equal(page.query("[data-f]").elements.color_hex.value, "#1d39c4");
  assert.equal(page.query('[data-preset="#1d39c4"]').getAttribute("aria-pressed"), "true");
  page.input("color_hex", "#AaBbCc");
  assert.equal(page.query("[data-f]").elements.primary_color.value, "#aabbcc");
  assert.ok(page.query("[data-preview]").getAttribute("style").includes("--preview-accent:#aabbcc"));
  assert.ok(page.el.querySelectorAll("[data-preset]").every((button) => button.getAttribute("aria-pressed") === "false"));
  page.input("primary_color", "#ffffff");
  assert.equal(page.query("[data-f]").elements.color_hex.value, "#ffffff");
  assert.match(page.query("[data-preview]").getAttribute("style"), /--preview-on:#101828/);
  assert.match(page.query("[data-status]").textContent, /未保存/);
  assert.equal(page.query("[data-save]").disabled, false);
  assert.equal(page.reloads, 0);
  assert.deepEqual(page.globalCalls, []);
  assert.ok(page.calls.every((call) => !call.options.method));
});

test("invalid hex never reaches inline CSS or the API and remains correctable", async () => {
  const page = setup();
  await page.mount();
  page.input("color_hex", '#fff;url("bad")');
  assert.equal(page.query("[data-save]").disabled, true);
  assert.equal(page.query("[data-f]").elements.color_hex.getAttribute("aria-invalid"), "true");
  assert.ok(!page.query("[data-preview]").getAttribute("style").includes("bad"));
  await page.submit();
  assert.match(page.query("[data-error]").textContent, /六位 HEX/);
  assert.ok(page.calls.every((call) => !call.options.method));
  page.input("color_hex", "#ABCDEF");
  assert.equal(page.query("[data-save]").disabled, false);
  assert.equal(page.query("[data-error]").hidden, true);
});

test("login preview escapes user-supplied text and never uses global branding selectors", async () => {
  const injection = '<img src=x onerror="alert(1)">';
  const page = setup({ name: injection, initial: { ...INITIAL, login_title: injection, copyright: injection } });
  await page.mount();
  page.click('[data-view="login"]');
  const preview = page.query("[data-preview]").innerHTML;
  assert.ok(preview.includes("&lt;img"));
  assert.ok(!preview.includes("<img src=x"));
  assert.equal(page.query('[data-view="login"]').getAttribute("aria-pressed"), "true");
  assert.equal(page.query('[data-view="console"]').getAttribute("aria-pressed"), "false");
  for (const selector of [".x-mark", ".x-bmark", ".x-wm", ".x-bhead", ".x-bcopy"]) assert.equal(page.query(selector), null);
  page.input("login_title", "新的登录标题");
  assert.ok(page.query("[data-preview]").innerHTML.includes("新的登录标题"));
  assert.equal(page.reloads, 0);
});

test("reset restores saved logo, color and copy locally; default logo does not reset color", async () => {
  const page = setup();
  await page.mount();
  page.click("[data-clear]");
  assert.equal(page.query("[data-f]").elements.primary_color.value, INITIAL.primary_color);
  assert.ok(!page.query("[data-prev]").innerHTML.includes(LOGO));
  page.click('[data-preset="#7c3aed"]');
  page.input("login_title", "changed");
  page.input("copyright", "changed");
  page.reset();
  assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
  for (const field of ["primary_color", "login_title", "copyright"]) assert.equal(page.query("[data-f]").elements[field].value, INITIAL[field]);
  assert.equal(page.query("[data-save]").disabled, true);
  assert.equal(page.query("[data-status]").getAttribute("data-dirty"), "false");
  assert.ok(page.calls.every((call) => !call.options.method));
});

test("PUT preserves API defaults and only a successful save reloads global branding", async () => {
  const saving = deferred();
  const page = setup({ api: (path, options) => options.method === "PUT" ? saving.promise : undefined });
  await page.mount();
  page.click('[data-preset="#1677ff"]');
  page.click("[data-clear]");
  page.input("login_title", " 新标题 ");
  page.input("copyright", " ");
  const result = page.submit();
  await tick();
  assert.deepEqual(plain(page.calls.find((call) => call.options.method === "PUT")), {
    path: "/settings/appearance", options: { method: "PUT", body: { logo: "", primary_color: "", login_title: "新标题", copyright: "" } }
  });
  assert.equal(page.reloads, 0);
  assert.equal(page.query("[data-save]").disabled, true);
  assert.equal(page.query("[data-f]").elements.login_title.disabled, true);
  await page.submit();
  assert.equal(page.calls.filter((call) => call.options.method === "PUT").length, 1);
  saving.resolve({});
  await result;
  assert.equal(page.reloads, 1);
});

test("failed save retains editable draft without reload or another GET", async () => {
  const page = setup({ api: (path, options) => { if (options.method === "PUT") throw new Error("没有管理权限"); } });
  await page.mount();
  page.input("login_title", "保留我的草稿");
  await page.submit();
  assert.match(page.query("[data-error]").textContent, /保存失败：没有管理权限/);
  assert.equal(page.query("[data-f]").elements.login_title.value, "保留我的草稿");
  assert.equal(page.query("[data-save]").disabled, false);
  assert.equal(page.query("[data-f]").elements.login_title.disabled, false);
  assert.equal(page.reloads, 0);
  assert.equal(page.calls.length, 3);
});

test("read-only settings users cannot mutate through any handler but can switch previews", async () => {
  const page = setup({ manage: false });
  await page.mount();
  assert.match(page.query("[data-status]").textContent, /只读模式/);
  assert.equal(page.query("[data-upload]").disabled, true);
  assert.equal(page.query('[data-preset="#1677ff"]').disabled, true);
  page.click("[data-clear]");
  assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
  page.upload();
  assert.equal(page.readers.length, 0);
  page.click('[data-view="login"]');
  assert.equal(page.query('[data-view="login"]').disabled, false);
  assert.ok(page.query("[data-preview]").innerHTML.includes(INITIAL.login_title));
  await page.submit();
  assert.ok(page.calls.every((call) => !call.options.method));
});

test("uploads enforce MIME type, byte limit and SVG script/event protections", async () => {
  const page = setup();
  await page.mount();
  for (const type of ["text/html", "image/gif", "", "image/webp"]) {
    page.upload(type);
    assert.match(page.query("[data-error]").textContent, /仅支持/);
  }
  page.upload("image/png", 256 * 1024 + 1);
  assert.match(page.query("[data-error]").textContent, /256 KiB/);
  assert.equal(page.readers.length, 0);
  for (const raw of ['<svg><script>alert(1)</script></svg>', '<svg onload = "alert(1)"/>', '<svg><a href="javascript:alert(1)"/></svg>']) {
    page.upload();
    page.readers.at(-1).finish(dataURL(raw));
    assert.match(page.query("[data-error]").textContent, /脚本或事件/);
    assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
  }
  for (const value of ["javascript:alert(1)", "data:image/png;base64,abcde", dataURL("x".repeat(256 * 1024 + 1), "image/png")]) {
    page.upload("image/png");
    page.readers.at(-1).finish(value);
    assert.equal(page.query("[data-error]").hidden, false);
    assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
  }
  assert.ok(page.calls.every((call) => !call.options.method));
});

test("valid uploads are pending until read completes; exact 256 KiB is accepted", async () => {
  const page = setup();
  await page.mount();
  page.input("login_title", "changed");
  page.upload("image/png", 256 * 1024);
  assert.equal(page.query("[data-save]").disabled, true);
  await page.submit();
  assert.ok(page.calls.every((call) => !call.options.method));
  const image = dataURL("x".repeat(256 * 1024), "image/png");
  page.readers.at(-1).finish(image);
  assert.equal(page.query("[data-save]").disabled, false);
  assert.ok(page.query("[data-prev]").innerHTML.includes(image));
  await page.submit();
  assert.equal(page.calls.at(-1).options.body.logo, image);
  assert.equal(page.reloads, 1);
});

test("late FileReader completions cannot undo clear, reset or a newer upload", async () => {
  const page = setup();
  await page.mount();
  page.upload();
  const cleared = page.readers.at(-1);
  page.click("[data-clear]");
  cleared.finish(LOGO);
  assert.ok(!page.query("[data-prev]").innerHTML.includes(LOGO));
  page.upload();
  const reset = page.readers.at(-1);
  page.reset();
  reset.finish(dataURL("changed", "image/png"));
  assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
  page.upload();
  const older = page.readers.at(-1);
  page.upload("image/jpeg");
  const latest = dataURL("latest", "image/jpeg");
  page.readers.at(-1).finish(latest);
  older.finish(LOGO);
  assert.ok(page.query("[data-prev]").innerHTML.includes(latest));
});

test("read failures clear busy state and leave the previous logo intact", async () => {
  const page = setup();
  await page.mount();
  page.input("login_title", "changed");
  page.upload();
  page.readers.at(-1).onerror();
  assert.match(page.query("[data-error]").textContent, /读取图片失败/);
  assert.equal(page.query("[data-save]").disabled, false);
  assert.ok(page.query("[data-prev]").innerHTML.includes(LOGO));
});

test("refresh warns about drafts and does not discard when cancelled", async () => {
  const page = setup({ confirm: false });
  await page.mount();
  page.input("copyright", "changed");
  page.click("[data-cfg-refresh]");
  page.click('[data-cfg-tab="basic"]');
  assert.equal(page.confirmations.length, 2);
  assert.equal(page.query("[data-f]").elements.copyright.value, "changed");
  assert.equal(page.calls.length, 2);
});

test("late save and upload completion cannot replace a newer settings page", async () => {
  const saving = deferred();
  const page = setup({ api: (path, options) => options.method === "PUT" ? saving.promise : undefined });
  await page.mount();
  page.input("login_title", "changed");
  const result = page.submit();
  await tick();
  page.click('[data-cfg-tab="integrations"]');
  saving.resolve({});
  await result;
  assert.equal(page.reloads, 0);
  assert.equal(page.query("[data-f]"), null);
  page.el._xsCfgTab = "appearance";
  await page.mount();
  page.upload();
  const oldReader = page.readers.at(-1);
  const oldPreview = page.query("[data-prev]");
  page.click('[data-cfg-tab="integrations"]');
  oldReader.finish(dataURL("stale", "image/png"));
  assert.ok(oldPreview.innerHTML.includes(LOGO));
});

test("stale initial loads cannot render over another settings category", async () => {
  const loading = deferred();
  const page = setup({ api: (path) => path === "/settings/appearance" ? loading.promise : undefined });
  page.window.XSPlatformSettings.render(page.el);
  page.click('[data-cfg-tab="integrations"]');
  loading.resolve(INITIAL);
  await tick(); await tick();
  assert.equal(page.query("[data-f]"), null);
});

test("permission load errors do not offer a blank writable form; unsafe saved logos never render", async () => {
  const denied = setup({ api: (path) => { if (path === "/settings/appearance") throw new Error("没有访问权限"); } });
  await denied.mount();
  assert.equal(denied.query("[data-f]"), null);
  assert.match(denied.query(".xs-cfg-err").textContent, /没有访问权限/);
  const invalid = setup({ initial: { ...INITIAL, logo: 'https://example.test/tracker.png" onerror="alert(1)' } });
  await invalid.mount();
  assert.ok(!invalid.query("[data-prev]").innerHTML.includes("tracker"));
  invalid.input("login_title", "changed");
  await invalid.submit();
  assert.ok(invalid.calls.every((call) => !call.options.method));
});

test("appearance CSS is scoped, supports narrow layouts and visible keyboard focus", () => {
  assert.match(stylesheet, /\.xs-appearance-layout\{display:grid/);
  assert.match(stylesheet, /@media\(max-width:1180px\)\{\.xs-appearance-layout\{grid-template-columns:1fr/);
  assert.match(stylesheet, /@media\(max-width:640px\)/);
  assert.match(stylesheet, /:focus-visible/);
  assert.match(stylesheet, /prefers-reduced-motion/);
  assert.ok(!stylesheet.includes("@import"));
  assert.ok(!stylesheet.includes("fonts.googleapis"));
});
