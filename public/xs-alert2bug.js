/* 告警 / 故障 →「转为缺陷」。读写真实接口（server/internal/modules/devcenter/alert_bug.go）：
 *   预览  POST /devcenter/bugs/from-alert {alert_id, dry_run:true} → {title, suspect_releases, existing, bug?, source}
 *   创建  POST /devcenter/bugs/from-alert {alert_id}              → 缺陷对象 + existing + suspect_releases（devcenter:manage）
 *   故障  GET  /oncall/incidents/:id/alerts                        → 取故障最早关联的告警
 * 实时模式按 XS.can('devcenter:manage') 显示按钮；演示模式（XS.mode !== "live"）使用演示数据，不发请求。
 * 挂载：
 *   XSAlert2Bug.buttonHTML(alert, {cls})      行内按钮 HTML（无权限 / 无平台记录时返回空串）
 *   XSAlert2Bug.bind(container, lookup)       绑定 container 内 [data-xa2b] 按钮；lookup(id) 返回告警对象（可选）
 *   XSAlert2Bug.mount(modal, alert)           在告警详情弹窗底部（.x-dfoot）加按钮
 *   XSAlert2Bug.mountIncident(slot, incId)    在故障详情的占位元素里加按钮（故障有关联告警时）
 *   XSAlert2Bug.open(alert)                   直接打开确认框
 */
(function (root) {
  "use strict";
  var PERM = "devcenter:manage";
  var PATH = "/devcenter/bugs/from-alert";
  var DEMO_BUGS = {}; // 演示模式：alert_id → 缺陷（模拟幂等）
  var demoSeq = 300;

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function live() { return X().mode === "live"; }
  function can() { var x = X(); return !!(x.can && x.can(PERM)); }
  function allowed() { return live() ? can() : true; }
  function doc() { return root.document || null; }
  function toast(msg) { var x = X(); if (x.toast) x.toast(msg); else if (typeof root.toast === "function") root.toast(msg); }
  function api(path, options) { return X().api(path, options || {}); }
  function hasCJK(s) { return /[\u4e00-\u9fff]/.test(String(s || "")); }
  function errMsg(e) {
    var m = (e && e.message) || "", s = e && e.status;
    if (s === 403) return hasCJK(m) ? m : "没有研发中心的管理权限";
    if (s === 404) return hasCJK(m) ? m : "告警不存在或已被清理";
    if (s === 400 || s === 422) return hasCJK(m) ? m : "请求参数无效";
    return hasCJK(m) ? m : "操作失败，请稍后重试";
  }
  function alertId(a) { return a == null ? "" : typeof a === "object" ? String(a.id == null ? "" : a.id) : String(a); }
  function bugKey(id) { return "BUG-" + id; }
  function ago(min) {
    min = +min || 0;
    if (min < 60) return min + " 分钟前";
    return Math.floor(min / 60) + " 小时" + (min % 60 ? " " + (min % 60) + " 分钟" : "") + "前";
  }

  /* ---------------------------------------------------------------- 演示数据 */
  function demoSource(a) {
    a = a || {};
    var lb = a.labels || {};
    return {
      alert_id: a.id, alertname: a.alertname || lb.alertname || a.rule || a.msg || "演示告警",
      cluster: lb.cluster || a.cluster || "prod-shanghai", namespace: lb.namespace || a.namespace || a.env || "prod",
      app: lb.app || lb.service || a.app || "order-service"
    };
  }
  function demoTitle(src) { return "[告警] " + src.alertname + " · " + src.cluster + "/" + src.namespace; }
  function demoSuspects(src) {
    var app = src.app, now = Date.now();
    return [
      { release_id: 1208, app_id: 3, app_name: app, env: "prod", image: "harbor.example.com/shop/" + app + ":v2.14.0", previous_image: "harbor.example.com/shop/" + app + ":v2.13.2",
        kind: "deploy", status: "succeeded", created_by: "zhangwei", created_at: new Date(now - 38 * 60000).toISOString(), minutes_before: 38, score: 6,
        reasons: ["应用名匹配（app=" + app + "）", "命名空间相同（" + src.namespace + "）", "同一集群（" + src.cluster + "）"] },
      { release_id: 1196, app_id: 5, app_name: "payment-gateway", env: "prod", image: "harbor.example.com/shop/payment-gateway:v1.8.3", kind: "deploy",
        status: "succeeded", created_by: "lina", created_at: new Date(now - 305 * 60000).toISOString(), minutes_before: 305, score: 3,
        reasons: ["命名空间相同（" + src.namespace + "）", "同一集群（" + src.cluster + "）"] }
    ];
  }
  function demoPreview(a) {
    var src = demoSource(a), ex = DEMO_BUGS[alertId(a)];
    return Promise.resolve({ preview: true, title: demoTitle(src), suspect_releases: demoSuspects(src), existing: !!ex, bug: ex || undefined, source: src });
  }
  function demoConvert(a) {
    var id = alertId(a), ex = DEMO_BUGS[id];
    if (ex) return Promise.resolve(Object.assign({}, ex, { existing: true }));
    var src = demoSource(a), sus = demoSuspects(src);
    var bug = { id: ++demoSeq, title: demoTitle(src), status: "todo", priority: "P1", data: { source: src, suspect_releases: sus }, suspect_releases: sus, existing: false };
    DEMO_BUGS[id] = bug;
    return Promise.resolve(bug);
  }

  /* ---------------------------------------------------------------- 接口 */
  function preview(a) {
    if (!live()) return demoPreview(a);
    return api(PATH, { method: "POST", body: { alert_id: +alertId(a), dry_run: true } });
  }
  function convert(a) {
    if (!live()) return demoConvert(a);
    return api(PATH, { method: "POST", body: { alert_id: +alertId(a) } });
  }

  /* ---------------------------------------------------------------- 打开缺陷 */
  var hostModal = null; // 当前告警详情弹窗（跳转前关闭）
  function openBug(id) {
    if (hostModal) {
      var close = hostModal.querySelector && hostModal.querySelector("[data-close]");
      if (close && typeof close.click === "function") close.click();
      hostModal = null;
    }
    var target = "#/bugs";
    var dev = function () { return root.XSDev && typeof root.XSDev.openDetail === "function" && live() ? root.XSDev : null; };
    var openNow = function () { var d = dev(); if (d) d.openDetail("bugs", id, function () {}); };
    if (!root.location) return;
    if (root.location.hash === target) { openNow(); return; }
    // 研发中心页面渲染时会关闭抽屉，等路由切换并渲染后再打开
    var done = false;
    var onHash = function () {
      if (done) return; done = true;
      if (root.removeEventListener) root.removeEventListener("hashchange", onHash);
      (root.setTimeout || setTimeout)(openNow, 400);
    };
    if (root.addEventListener) root.addEventListener("hashchange", onHash);
    root.location.hash = target;
  }

  /* ---------------------------------------------------------------- 提示 */
  function successToast(bug) {
    var d = doc();
    var text = (bug.existing ? "该告警已转过缺陷 " : "已创建缺陷 ") + bugKey(bug.id);
    if (!d || !d.body) { toast(text); return null; }
    var old = d.querySelector(".xa2b-toast"); if (old) old.remove();
    var el = d.createElement("div");
    el.setAttribute("class", "xa2b-toast");
    el.setAttribute("role", "status");
    el.setAttribute("style", "position:fixed;right:24px;bottom:24px;z-index:3000;background:#fff;border:1px solid #d9dee8;border-left:3px solid #16a34a;border-radius:4px;box-shadow:0 4px 12px rgba(15,23,42,.12);padding:10px 14px;font-size:13px;color:#1f2937;display:flex;gap:12px;align-items:center;max-width:420px");
    el.innerHTML = '<span>' + esc(text) + '</span><button class="x-tlink" type="button" data-xa2b-view style="white-space:nowrap">查看缺陷</button><button class="x-x" type="button" data-xa2b-x aria-label="关闭提示">×</button>';
    d.body.appendChild(el);
    var view = el.querySelector("[data-xa2b-view]"), x = el.querySelector("[data-xa2b-x]");
    if (view) view.onclick = function () { el.remove(); openBug(bug.id); };
    if (x) x.onclick = function () { el.remove(); };
    (root.setTimeout || setTimeout)(function () { if (el.isConnected !== false) el.remove(); }, 10000);
    return el;
  }

  /* ---------------------------------------------------------------- 确认框 */
  function suspectsHTML(list) {
    if (!list || !list.length) return '<p class="x-frac" style="margin:6px 0 0">告警前 24 小时内未发现同应用、同命名空间的发布记录。</p>';
    return '<div class="xb-table"><table><thead><tr><th>发布</th><th>应用 / 环境</th><th>镜像</th><th>时间</th><th>匹配依据</th></tr></thead><tbody>' +
      list.map(function (s) {
        return "<tr><td class=\"x-mono\">#" + esc(s.release_id) + "</td><td>" + esc(s.app_name) + " · " + esc(s.env) + "</td><td class=\"x-mono\" style=\"word-break:break-all\">" + esc(s.image) +
          "</td><td style=\"white-space:nowrap\">告警" + esc(ago(s.minutes_before)) + "</td><td>" + esc((s.reasons || []).join("、") || "—") + "</td></tr>";
      }).join("") + "</tbody></table></div>";
  }
  function modal(title, body, foot) {
    var d = doc();
    var m = d.createElement("div");
    m.setAttribute("class", "xb-confirm xa2b-confirm");
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true" style="width:min(720px,calc(100vw - 32px))"><h3>' + esc(title) +
      '</h3><div class="xb-confirm-body" data-xa2b-body>' + body + '</div><p class="fk-err xd-err" data-xa2b-err hidden></p><div class="xb-confirm-foot" data-xa2b-foot>' + foot + "</div></div>";
    d.body.appendChild(m);
    function esc2(e) { if (e.key === "Escape") api2.close(); }
    var api2 = {
      el: m,
      close: function () { m.remove(); if (d.removeEventListener) d.removeEventListener("keydown", esc2); },
      err: function (msg) { var p = m.querySelector("[data-xa2b-err]"); if (p) { p.textContent = msg || ""; p.hidden = !msg; } }
    };
    if (d.addEventListener) d.addEventListener("keydown", esc2);
    var no = m.querySelector("[data-xa2b-no]"); if (no) no.onclick = api2.close;
    return api2;
  }

  function open(a) {
    var id = alertId(a);
    if (!id || (live() && !/^\d+$/.test(id))) { toast("这条告警没有平台记录，无法转为缺陷"); return Promise.resolve(null); }
    if (!allowed()) { toast("没有研发中心的管理权限"); return Promise.resolve(null); }
    if (!doc() || !doc().body) return convert(a).then(function (bug) { successToast(bug); return bug; });
    var m = modal("告警转为缺陷", '<p class="x-frac">正在分析告警与最近的发布…</p>', '<button class="x-btn" type="button" data-xa2b-no>取消</button>');
    return preview(a).then(function (pv) {
      pv = pv || {};
      var body = m.el.querySelector("[data-xa2b-body]"), foot = m.el.querySelector("[data-xa2b-foot]");
      if (pv.existing && pv.bug) {
        body.innerHTML = '<p>该告警已转为缺陷 <b class="x-mono">' + esc(bugKey(pv.bug.id)) + "</b>：" + esc(pv.bug.title) + "</p>";
        foot.innerHTML = '<button class="x-btn" type="button" data-xa2b-no>关闭</button><button class="x-btn x-btn--pri" type="button" data-xa2b-view>查看缺陷</button>';
        foot.querySelector("[data-xa2b-no]").onclick = m.close;
        foot.querySelector("[data-xa2b-view]").onclick = function () { m.close(); openBug(pv.bug.id); };
        return null;
      }
      var sus = pv.suspect_releases || [];
      body.innerHTML = '<p style="margin:0 0 8px">将创建缺陷：<b>' + esc(pv.title || "") + "</b></p>" +
        '<p class="x-frac" style="margin:0 0 10px">缺陷描述包含告警摘要、标签与告警链接，处理人可在研发中心继续指派。</p>' +
        '<h4 style="margin:12px 0 6px;font-size:13px">可疑发布（告警前 24 小时，' + sus.length + " 条）</h4>" + suspectsHTML(sus) +
        (sus.length ? '<p class="x-frac" style="margin:8px 0 0">按应用名、命名空间、集群匹配，仅供排查参考。</p>' : "");
      foot.innerHTML = '<button class="x-btn" type="button" data-xa2b-no>取消</button><button class="x-btn x-btn--pri" type="button" data-xa2b-yes>确认转为缺陷</button>';
      foot.querySelector("[data-xa2b-no]").onclick = m.close;
      var yes = foot.querySelector("[data-xa2b-yes]");
      return new Promise(function (resolve) {
        yes.onclick = function () {
          if (yes.disabled) return;
          yes.disabled = true; yes.textContent = "正在创建…"; m.err("");
          convert(a).then(function (bug) {
            m.close();
            successToast(bug);
            resolve(bug);
          }).catch(function (e) { yes.disabled = false; yes.textContent = "确认转为缺陷"; m.err(errMsg(e)); });
        };
      });
    }).catch(function (e) {
      var body = m.el.querySelector("[data-xa2b-body]");
      if (body) body.innerHTML = '<p class="x-aerr" role="alert">' + esc(errMsg(e)) + "</p>";
      return null;
    });
  }

  /* ---------------------------------------------------------------- 挂载 */
  function buttonHTML(a, o) {
    o = o || {};
    var id = alertId(a);
    if (!id || !allowed() || (live() && !/^\d+$/.test(id))) return "";
    return '<button class="' + esc(o.cls || "x-btn") + '" type="button" data-xa2b="' + esc(id) + '">转为缺陷</button>';
  }
  function bind(container, lookup) {
    if (!container || !container.querySelectorAll) return;
    [].slice.call(container.querySelectorAll("[data-xa2b]")).forEach(function (b) {
      b.onclick = function (ev) {
        if (ev && ev.stopPropagation) ev.stopPropagation();
        var id = b.getAttribute("data-xa2b");
        var a = typeof lookup === "function" ? lookup(id) : null;
        open(a && alertId(a) ? a : { id: id });
      };
    });
  }
  function mount(modalEl, a) {
    if (!modalEl || !modalEl.querySelector || !alertId(a) || !allowed()) return null;
    if (modalEl.querySelector("[data-xa2b]")) return modalEl.querySelector("[data-xa2b]");
    var foot = modalEl.querySelector(".x-dfoot");
    if (!foot) return null;
    foot.insertAdjacentHTML("afterbegin", buttonHTML(a));
    var b = modalEl.querySelector("[data-xa2b]");
    if (b) b.onclick = function () { hostModal = modalEl; open(a); };
    return b;
  }
  function mountIncident(slot, incidentId) {
    if (!slot || !incidentId || !allowed()) return Promise.resolve(null);
    var load = live() ? api("/oncall/incidents/" + encodeURIComponent(incidentId) + "/alerts") :
      Promise.resolve({ items: [{ id: "demo-inc-" + incidentId, alertname: "故障关联告警", labels: { app: "order-service", namespace: "prod", cluster: "prod-shanghai" } }] });
    return load.then(function (d) {
      var items = (d && (Array.isArray(d) ? d : d.items)) || [];
      if (!items.length || slot.isConnected === false) return null;
      var a = items[0];
      slot.innerHTML = buttonHTML(a);
      var b = slot.querySelector("[data-xa2b]");
      if (b) b.onclick = function () { open(a); };
      return b;
    }).catch(function () { return null; }); // 读不到关联告警时不显示按钮
  }

  root.XSAlert2Bug = { buttonHTML: buttonHTML, bind: bind, mount: mount, mountIncident: mountIncident, open: open, preview: preview, convert: convert, openBug: openBug, allowed: allowed,
    _suspectsHTML: suspectsHTML, _reset: function () { DEMO_BUGS = {}; demoSeq = 300; hostModal = null; } };
})(window);
