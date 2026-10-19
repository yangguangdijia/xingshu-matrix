// 无第三方依赖的 Chrome CDP 冒烟测试：真实浏览器 + mock API。
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const CHROME = process.env.CHROME_PATH || "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
const BASE = process.env.SMOKE_URL || "http://127.0.0.1:18080/";
const PORT = Number(process.env.CDP_PORT || 19222);
const DEMO = process.env.SMOKE_DEMO === "1";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitHttp(url, timeout = 15000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    try { const r = await fetch(url); if (r.ok) return await r.json(); } catch {}
    await sleep(200);
  }
  throw new Error("timeout waiting " + url);
}

class CDP {
  constructor(ws) { this.ws = ws; this.id = 0; this.wait = new Map(); this.events = []; }
  static async connect(url) {
    const ws = new WebSocket(url);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    const c = new CDP(ws);
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id && c.wait.has(msg.id)) { const p = c.wait.get(msg.id); c.wait.delete(msg.id); msg.error ? p.rej(new Error(msg.error.message)) : p.res(msg.result); }
      else if (msg.method === "Runtime.exceptionThrown") c.events.push("exception: " + (msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text));
      else if (msg.method === "Log.entryAdded" && msg.params.entry.level === "error") c.events.push("console: " + msg.params.entry.text);
    };
    return c;
  }
  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((res, rej) => { this.wait.set(id, { res, rej }); this.ws.send(JSON.stringify({ id, method, params })); });
  }
  async eval(expression, awaitPromise = true) {
    let last;
    for (let i = 0; i < 2; i++) {
      try {
        const r = await this.send("Runtime.evaluate", { expression, awaitPromise, returnByValue: true, userGesture: true });
        if (r.exceptionDetails) throw new Error((r.exceptionDetails.exception && r.exceptionDetails.exception.description) || r.exceptionDetails.text);
        return r.result.value;
      } catch (e) {
        last = e;
        if (!/Execution context was destroyed|Cannot find context|Target closed/.test(e.message)) throw e;
        await sleep(600);
      }
    }
    throw last;
  }
}

async function main() {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), "xs-smoke-"));
  const chrome = spawn(CHROME, [
    "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
    "--remote-debugging-port=" + PORT, "--remote-allow-origins=*",
    "--user-data-dir=" + profile, "--window-size=1440,1000", "about:blank"
  ], { stdio: "ignore" });
  const cleanup = () => { try { chrome.kill(); } catch {} try { fs.rmSync(profile, { recursive: true, force: true }); } catch {} };
  try {
    await waitHttp("http://127.0.0.1:" + PORT + "/json/version");
    const page = await (await fetch("http://127.0.0.1:" + PORT + "/json/new?" + encodeURIComponent(BASE), { method: "PUT" })).json();
    const c = await CDP.connect(page.webSocketDebuggerUrl);
    await c.send("Runtime.enable");
    await c.send("Log.enable");
    await c.send("Page.enable");

    await c.eval("(async function(){ const end = Date.now() + 15000; while (Date.now() < end) { const h = document.querySelector('#lmode'); if (window.XS && XS.mode === " + JSON.stringify(DEMO ? "demo" : "live") + " && h && !h.hidden) return true; await new Promise(function(r){ setTimeout(r, 100); }); } return false; })()");
    const mode = await c.eval("({mode: XS.mode, base: XS.base, hint: (document.querySelector('#lmode')||{}).textContent || ''})");
    console.log("MODE", JSON.stringify(mode));

    const loginUser = process.env.SMOKE_USER || "admin";
    const loginPass = process.env.SMOKE_PASS || (DEMO ? "Xingshu@2026" : loginUser === "newadmin" ? "Temp1234!" : "Passw0rd!");
    await c.eval("(function(){ document.querySelector('#lf [name=u]').value = " + JSON.stringify(loginUser) + "; document.querySelector('#lf [name=p]').value = " + JSON.stringify(loginPass) + "; document.querySelector('#lf').requestSubmit(); return true; })()");

    const loggedIn = await c.eval("(async function(){ const end = Date.now() + 15000; while (Date.now() < end) { if (location.hash !== '#/login') return true; await new Promise(function(r){ setTimeout(r, 100); }); } return false; })()");
    let changed = false;
    if (process.env.SMOKE_CHANGE === "1") {
      const onPw = await c.eval("(async function(){ const end = Date.now() + 10000; while (Date.now() < end) { if (location.hash === '#/change-password' && document.querySelector('#cf')) return true; await new Promise(function(r){ setTimeout(r, 100); }); } return false; })()");
      if (!onPw) throw new Error("forced password-change page not shown");
      await c.eval("(function(){ const f = document.querySelector('#cf'); f.elements.cur.value = " + JSON.stringify(loginPass) + "; f.elements.np.value = 'NewPassw0rd!'; f.elements.cp.value = 'NewPassw0rd!'; f.requestSubmit(); return true; })()");
      changed = await c.eval("(async function(){ const end = Date.now() + 10000; while (Date.now() < end) { if (location.hash !== '#/change-password' && document.querySelector('#auth').hidden) return true; await new Promise(function(r){ setTimeout(r, 100); }); } return false; })()");
      console.log("PASSWORD_CHANGE", changed);
      if (!changed) process.exitCode = 1;
    }
    const state = await c.eval("({loggedIn: !!localStorage.getItem('xs_access_token'), hash: location.hash, authHidden: document.querySelector('#auth').hidden, who: Array.from(document.querySelectorAll('[data-who]')).map(function(x){return x.textContent;}), viewText: (document.querySelector('#view').innerText || '').slice(0, 240)})");
    console.log("LOGIN", JSON.stringify({ loggedIn: loggedIn, state: state }));

    async function visit(hash) {
      c.events = [];
      try { await c.eval("location.hash = " + JSON.stringify(hash) + "; true"); } catch (e) { console.log("NAVFAIL", hash, e.message); }
      await sleep(1200);
      let text = "";
      try { text = await c.eval("(document.querySelector('#view').innerText || '')"); } catch (e) { text = "EVALFAIL " + e.message; }
      const state = { hash, head: text.slice(0, 140), bad: /页面加载出错|Cannot read|undefined is not|加载失败|EVALFAIL/.test(text), errors: c.events.slice(0, 6) };
      console.log("PAGE", JSON.stringify(state));
      return state;
    }
    const pages = [];
    const routes = DEMO ? ["#/sre", "#/team"] : ["#/sre", "#/iam", "#/iam/roles", "#/iam/logs", "#/approvals", "#/cluster", "#/cluster/workloads", "#/cluster/hosts", "#/team", "#/observe", "#/logs", "#/cicd/pipeline", "#/delivery/apps", "#/release", "#/middleware", "#/oncall", "#/settings", "#/ai", "#/automation"];
    for (const h of routes) pages.push(await visit(h));
    const teamText = await c.eval("(document.querySelector('#view').innerText || '')");
    if (pages.some((p) => p.errors.length || p.bad)) process.exitCode = 1;

    const shot = await c.send("Page.captureScreenshot", { format: "png" });
    const out = path.resolve("tests/frontend/smoke-login.png");
    fs.writeFileSync(out, Buffer.from(shot.data, "base64"));
    console.log("SHOT", out);
    const ok = state.authHidden === true && state.hash !== "#/login";
    console.log(ok ? "SMOKE OK" : "SMOKE FAILED");
    if (!ok) process.exitCode = 1;
  } finally {
    cleanup();
  }
}
main().catch((e) => { console.error("SMOKE ERROR", e); process.exit(1); });
