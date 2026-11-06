/* xs-kubectl.js：顶部栏 kubectl Web Shell。
 * 每个标签页绑定一个集群（可多开），命令发往 POST /clusters/:id/kubectl，由平台用该集群的凭据执行。
 * 只读命令直接返回结果；scale / rollout restart / delete / cordon 等变更走平台审批与审计；
 * 生产环境集群的变更命令在终端内还需输入集群名二次确认。
 * Pod 标签页（从工作负载「Shell」进入）每行以 exec <pod> -- sh -c "<命令>" 非交互执行。 */
(function () {
  "use strict";
  var dock = document.getElementById("kdock"), btn = document.getElementById("kcbtn");
  var body = document.getElementById("kbody"), inp = document.getElementById("kin"), ps = document.getElementById("kps");
  if (!dock || !btn || !body || !inp || !ps) return;
  var tabsEl = dock.querySelector("#ktabs"), addSel = dock.querySelector("#kadd"), grip = dock.querySelector("#kgrip"), tin = body.querySelector(".x-tin");
  var clusters = [], tabs = [], cur = null, seq = 0, loaded = false, loading = null, emptyEl = null;
  var RISKY = /^(?:kubectl\s+|k\s+)?(scale|delete|cordon|uncordon|drain|rollout\s+restart)\b/;

  function live() { return !!(window.XS && window.XS.mode === "live"); }
  function isProd(t) { return /^prod/i.test(t.env || "") || /生产/.test(t.env || ""); }
  function prompt(t) {
    t = t || cur; if (!t) return "";
    if (t.ask) return "生产集群变更，请输入集群名 " + t.name + " 确认:";
    if (t.pod) return t.pod + " (" + t.ns + ") #";
    return t.name + ":" + t.ns + " $";
  }
  function print(text, cls, t) {
    t = t || cur; if (!t) return;
    var d = document.createElement("div");
    if (cls) d.className = cls;
    d.textContent = text; t.out.appendChild(d);
    while (t.out.childNodes.length > 3000) t.out.removeChild(t.out.firstChild);
    t.out.scrollTop = t.out.scrollHeight;
  }
  function setPs() {
    ps.textContent = prompt();
    inp.disabled = !cur || !!(cur && cur.busy);
    inp.placeholder = !cur ? "选择集群后输入命令" : cur.pod ? "在容器内执行的命令，例如 ls -l /" : "例如 get pods，输入 help 查看支持的命令";
    inp.setAttribute("aria-label", cur && cur.pod ? "容器内命令" : "kubectl 命令");
  }
  function drawTabs() {
    tabsEl.textContent = "";
    tabs.forEach(function (t, i) {
      var el = document.createElement("div"), on = t === cur, name = t.pod ? t.pod : t.name;
      el.className = "x-ktab" + (on ? " on" : ""); el.id = "ktab-" + t.id;
      el.setAttribute("role", "tab"); el.setAttribute("aria-selected", String(on)); el.setAttribute("aria-controls", t.out.id); el.tabIndex = on ? 0 : -1;
      var s = document.createElement("span"); s.textContent = name; el.appendChild(s);
      if (isProd(t)) { var b = document.createElement("span"); b.className = "x-kprod"; b.textContent = "生产"; el.appendChild(b); }
      var x = document.createElement("button"); x.type = "button"; x.textContent = "✕"; x.tabIndex = -1; x.setAttribute("aria-label", "关闭标签页 " + name);
      x.addEventListener("click", function (e) { e.stopPropagation(); closeTab(t); });
      el.appendChild(x);
      el.addEventListener("click", function () { select(t); });
      el.addEventListener("keydown", function (e) {
        var j = e.key === "ArrowRight" ? i + 1 : e.key === "ArrowLeft" ? i - 1 : -2;
        if (j > -2) { e.preventDefault(); var n = tabs[(j + tabs.length) % tabs.length]; select(n, true); var f = document.getElementById("ktab-" + n.id); if (f) f.focus(); }
        else if (e.key === "Delete") { e.preventDefault(); closeTab(t); }
      });
      tabsEl.appendChild(el);
    });
  }
  function select(t, keepFocus) {
    cur = t;
    tabs.forEach(function (x) { x.out.hidden = x !== t; });
    if (emptyEl) emptyEl.hidden = !!t;
    drawTabs(); setPs();
    if (!keepFocus && !dock.hidden && !inp.disabled) inp.focus();
  }
  function addTab(cl, opts) {
    opts = opts || {};
    var t = { id: ++seq, cid: cl.id, name: cl.name, env: cl.env || "", ns: opts.ns || "default", pod: opts.pod || null,
      container: opts.container || "", hist: [], hi: 0, ask: null, busy: false, out: document.createElement("div") };
    t.out.className = "x-log"; t.out.id = "kout-" + t.id;
    t.out.setAttribute("role", "log"); t.out.setAttribute("aria-live", "polite");
    body.insertBefore(t.out, tin);
    tabs.push(t);
    if (t.pod) print("已连接容器 " + t.pod + (t.container ? "/" + t.container : "") + "（命名空间 " + t.ns + "，集群 " + t.name + "）。每行命令以非交互方式执行，不支持 vi、top 等全屏程序。", "d", t);
    else print("已连接集群 " + t.name + (t.env ? "（" + t.env + "）" : "") + "。输入 help 查看支持的命令；ns <命名空间> 切换默认命名空间。", "d", t);
    select(t);
    return t;
  }
  function closeTab(t) {
    var i = tabs.indexOf(t);
    if (i < 0) return;
    tabs.splice(i, 1); t.out.remove();
    if (!tabs.length) { cur = null; drawTabs(); setPs(); if (emptyEl) emptyEl.hidden = false; return; }
    if (t === cur) select(tabs[Math.min(i, tabs.length - 1)]); else drawTabs();
  }
  function currentClusterId() {
    try { var v = JSON.parse(localStorage.getItem("xs_cluster_context") || "null"); return v && v.kind === "cluster" ? String(v.cluster_id) : ""; } catch (e) { return ""; }
  }
  function errText(err) {
    if (!err) return "请求失败";
    if (err.status === 401) return "登录已过期，请重新登录";
    if (err.status === 403) return "当前账号没有该集群的终端权限";
    return err.message || ("请求失败（HTTP " + (err.status || "?") + "）");
  }
  function loadClusters() {
    if (loading) return loading;
    loading = window.XS.get("/clusters").then(function (data) {
      var list = Array.isArray(data) ? data : (data && (data.items || data.list)) || [];
      clusters = list.map(function (c) { return { id: c.id, name: c.name, env: c.env || "" }; });
      loaded = true;
      addSel.innerHTML = '<option value="">' + (clusters.length ? "＋ 打开集群终端" : "尚未接入集群") + "</option>" +
        clusters.map(function (c) { return '<option value="' + c.id + '">' + escapeHTML(c.name) + (c.env ? "（" + escapeHTML(c.env) + "）" : "") + "</option>"; }).join("");
      addSel.disabled = !clusters.length;
      emptyEl.textContent = clusters.length ? "从右上角选择一个集群打开终端。" : "尚未接入集群，请先在「集群管理」中添加集群。";
      return clusters;
    }, function (err) {
      loading = null;
      emptyEl.textContent = "无法读取集群列表：" + errText(err);
      throw err;
    });
    return loading;
  }
  function escapeHTML(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function ensureDefaultTab() {
    if (tabs.length || !clusters.length) return;
    var id = currentClusterId(), cl = null;
    clusters.forEach(function (c) { if (String(c.id) === id) cl = c; });
    addTab(cl || clusters[0]);
  }
  function open(show) {
    dock.hidden = !show; btn.setAttribute("aria-expanded", String(show));
    if (!show) return;
    dock.classList.remove("min");
    if (!live()) { emptyEl.textContent = "演示模式不连接真实集群，请登录后使用 Web Shell。"; addSel.disabled = true; setPs(); return; }
    loadClusters().then(function () { ensureDefaultTab(); if (cur && !inp.disabled) inp.focus(); }, function () {});
  }
  function shellQuote(s) { return '"' + String(s).replace(/(["\\])/g, "\\$1") + '"'; }

  function send(t, line, reason) {
    var cmd = line;
    if (t.pod) cmd = "exec " + t.pod + (t.container ? " -c " + t.container : "") + " -- sh -c " + shellQuote(line);
    t.busy = true; if (t === cur) setPs();
    return window.XS.post("/clusters/" + t.cid + "/kubectl", { command: cmd, namespace: t.ns, reason: reason || "" }, { timeoutMs: 45000 }).then(function (r) {
      r = r || {};
      if (r.output) print(r.output, r.exit_code ? "r" : (r.change ? "g" : ""), t);
      else if (!r.exit_code) print("（无输出）", "d", t);
      if (r.truncated) print("输出超过 1 MiB，已截断。", "d", t);
    }, function (err) { print("error: " + errText(err), "r", t); })
      .then(function () { t.busy = false; if (t === cur) { setPs(); inp.focus(); } });
  }
  function builtin(t, line) {
    var m = line.match(/^ns(?:\s+(\S+))?$/) || line.match(/^(?:kubectl\s+)?config\s+set-context\s+--current\s+--namespace[=\s](\S+)$/);
    if (m) {
      if (!m[1]) { print("当前命名空间：" + t.ns, "", t); return true; }
      t.ns = m[1]; print("默认命名空间已切换为 " + t.ns, "d", t); setPs(); return true;
    }
    if (line === "clear") { t.out.textContent = ""; return true; }
    if (line === "exit" && t.pod) { closeTab(t); return true; }
    return false;
  }

  btn.addEventListener("click", function () { open(dock.hidden); });
  var kc = dock.querySelector("#kclose"), km = dock.querySelector("#kmin");
  if (kc) kc.addEventListener("click", function () { open(false); btn.focus(); });
  if (km) km.addEventListener("click", function () { dock.classList.toggle("min"); if (!dock.classList.contains("min") && !inp.disabled) inp.focus(); });
  addSel.innerHTML = '<option value="">加载中…</option>';
  addSel.disabled = true;
  addSel.addEventListener("change", function () {
    var id = addSel.value; addSel.value = "";
    clusters.forEach(function (c) { if (String(c.id) === id) addTab(c); });
  });
  emptyEl = document.createElement("div");
  emptyEl.className = "x-log"; emptyEl.setAttribute("role", "status");
  emptyEl.textContent = "正在读取集群列表…";
  body.insertBefore(emptyEl, tin);
  setPs();

  inp.addEventListener("keydown", function (e) {
    var t = cur;
    if (!t || t.busy) return;
    if (e.key === "Enter") {
      e.preventDefault();
      var line = inp.value.trim(); inp.value = "";
      if (t.ask) {
        var pending = t.ask; t.ask = null;
        print(prompt(t) + " " + line, "d", t);
        if (line === t.name) send(t, pending, "Web Shell 生产变更（已输入集群名确认）");
        else print("集群名不匹配，已取消：" + pending, "r", t);
        setPs(); return;
      }
      print(prompt(t) + " " + line, "d", t);
      if (!line) return;
      t.hist.push(line); if (t.hist.length > 200) t.hist.shift(); t.hi = t.hist.length;
      if (builtin(t, line)) return;
      if (!t.pod && isProd(t) && RISKY.test(line)) { t.ask = line; setPs(); return; }
      send(t, line);
    } else if (e.key === "Escape" && t.ask) {
      e.preventDefault(); print("已取消：" + t.ask, "r", t); t.ask = null; inp.value = ""; setPs();
    } else if (e.key === "ArrowUp" && t.hist.length && !t.ask) {
      e.preventDefault(); t.hi = Math.max(0, t.hi - 1); inp.value = t.hist[t.hi];
    } else if (e.key === "ArrowDown" && t.hist.length && !t.ask) {
      e.preventDefault(); t.hi = Math.min(t.hist.length, t.hi + 1); inp.value = t.hist[t.hi] || "";
    } else if (e.key === "l" && e.ctrlKey) {
      e.preventDefault(); t.out.textContent = "";
    }
  });

  // 高度拖拽：160px ~ 80vh
  var maxH = function () { return Math.round(window.innerHeight * 0.8); };
  function setH(h) {
    h = Math.round(Math.max(160, Math.min(maxH(), h)));
    dock.style.setProperty("--kh", h + "px");
    if (grip) { grip.setAttribute("aria-valuenow", String(h)); grip.setAttribute("aria-valuemax", String(maxH())); }
  }
  if (grip) {
    grip.setAttribute("aria-valuemin", "160");
    grip.addEventListener("pointerdown", function (e) {
      e.preventDefault();
      var y0 = e.clientY, h0 = dock.getBoundingClientRect().height;
      try { grip.setPointerCapture(e.pointerId); } catch (x) {}
      function mv(ev) { setH(h0 + y0 - ev.clientY); }
      function up() { grip.removeEventListener("pointermove", mv); grip.removeEventListener("pointerup", up); grip.removeEventListener("pointercancel", up); }
      grip.addEventListener("pointermove", mv); grip.addEventListener("pointerup", up); grip.addEventListener("pointercancel", up);
    });
    grip.addEventListener("keydown", function (e) {
      if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
      e.preventDefault(); setH(dock.getBoundingClientRect().height + (e.key === "ArrowUp" ? 20 : -20));
    });
  }

  // 供工作负载 / Pod 页面调用：XSKubectl.openPod({cluster_id, namespace, pod, container})
  function openPod(o) {
    o = o || {};
    if (!o.pod) { open(true); return; }
    open(true);
    if (!live()) return;
    loadClusters().then(function () {
      var cid = String(o.cluster_id || currentClusterId()), cl = null;
      clusters.forEach(function (c) { if (String(c.id) === cid) cl = c; });
      if (!cl) { if (cur) print("未找到 Pod 所在集群", "r"); return; }
      addTab(cl, { ns: o.namespace || "default", pod: o.pod, container: o.container || "" });
    }, function () {});
  }
  window.XSKubectl = { open: function () { open(true); }, openPod: openPod };
  window.kcExecOpen = function (w) {
    if (w && w.pod) return openPod(w);
    open(true);
  };
})();
