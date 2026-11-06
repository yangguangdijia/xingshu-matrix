/*
 * 星枢 Matrix 研发中心（实时模式）
 * 页面：工作台、需求 / 任务（列表 + 详情抽屉：状态流转、指派、评论、变更记录、关联代码仓库与流水线、拆分任务）、
 *       需求验收、迭代（燃尽图）、看板、缺陷、测试用例、代码仓库关联、需求 → AI 开发（实施方案与任务拆解）、统计报表。
 * 数据全部来自研发中心与 CI/CD 的真实接口；没有数据时显示空状态，不使用演示数据。
 * 新建与编辑统一使用 XSFormKit；删除均需二次确认，删除迭代需输入迭代名称。
 */
(function (root) {
  "use strict";
  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function errText(e) {
    if (e && e.status === 403) return "没有权限执行此操作，请联系管理员分配研发中心权限";
    return (e && (e.message || e.error)) || String(e || "未知错误");
  }
  function items(r) { return Array.isArray(r) ? r : (r && r.items) || []; }
  function pad(n) { return String(n).padStart(2, "0"); }
  function fmt(t) { if (!t) return "—"; var d = new Date(t); return isNaN(d) ? String(t).slice(0, 16) : (d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()); }
  function ago(t) {
    if (!t) return "—";
    var s = (Date.now() - new Date(t).getTime()) / 1000;
    if (isNaN(s)) return "—";
    if (s < 60) return "刚刚";
    if (s < 3600) return Math.floor(s / 60) + " 分钟前";
    if (s < 86400) return Math.floor(s / 3600) + " 小时前";
    if (s < 86400 * 30) return Math.floor(s / 86400) + " 天前";
    return fmt(t);
  }
  function hrs(h) { if (h == null || isNaN(h)) return "—"; if (h < 1) return "不到 1 小时"; if (h < 48) return Math.round(h) + " 小时"; return (h / 24).toFixed(1) + " 天"; }
  function me() { var u = (xs() && xs().user) || {}; return u.username || ""; }
  function data(x) { return (x && x.data && typeof x.data === "object") ? x.data : {}; }
  function on(el, sel, fn) { [].forEach.call(el.querySelectorAll(sel), function (b) { b.onclick = function (ev) { ev.stopPropagation(); fn(b, ev); }; }); }
  function tzq(q) { q = q || {}; q.tz_offset = -new Date().getTimezoneOffset(); return q; }
  function stillOn(hash) { return (location.hash || "").split("?")[0] === hash; }

  (function ensureCss() {
    if (typeof document === "undefined" || document.getElementById("xs-devcenter-css")) return;
    var l = document.createElement("link");
    l.id = "xs-devcenter-css"; l.rel = "stylesheet"; l.href = "xs-devcenter.css";
    document.head.appendChild(l);
  })();

  var KIND = {
    requirements: { name: "需求", st: [["todo", "待评审", "x-grey"], ["ready", "待开发", "x-blue"], ["doing", "开发中", "x-blue"], ["testing", "测试中", "x-orange"], ["accepting", "待验收", "x-orange"], ["done", "已完成", "x-green"], ["rejected", "已关闭", "x-grey"]] },
    iterations: { name: "迭代", st: [["todo", "规划中", "x-grey"], ["doing", "进行中", "x-blue"], ["done", "已结束", "x-green"]] },
    bugs: { name: "缺陷", st: [["todo", "新建", "x-red"], ["doing", "处理中", "x-blue"], ["fixed", "已修复", "x-orange"], ["verified", "已验证", "x-green"], ["closed", "已关闭", "x-grey"], ["reopened", "重新打开", "x-red"]] },
    tests: { name: "测试用例", st: [["todo", "未执行", "x-grey"], ["passed", "通过", "x-green"], ["failed", "失败", "x-red"], ["blocked", "阻塞", "x-orange"]] }
  };
  var PRI = [["P0", "紧急", "x-red"], ["P1", "高", "x-orange"], ["P2", "中", "x-blue"], ["P3", "低", "x-grey"]];
  var SEV = { critical: "致命", major: "严重", minor: "一般", trivial: "轻微", unknown: "未填写" };
  var DONE = { requirements: ["done", "rejected"], iterations: ["done"], bugs: ["verified", "closed"], tests: ["passed"] };
  var RUN_ST = { queued: ["排队中", "x-grey"], running: ["运行中", "x-blue"], cancelling: ["取消中", "x-orange"], succeeded: ["成功", "x-green"], failed: ["失败", "x-red"], cancelled: ["已取消", "x-grey"] };
  function stOf(k, s) { return KIND[k].st.filter(function (x) { return x[0] === s; })[0] || [s, s, "x-grey"]; }
  function stTag(k, s) { var x = stOf(k, s); return '<span class="x-tag x-tag--s ' + x[2] + '">' + E(x[1]) + "</span>"; }
  function priTag(p) { var x = PRI.filter(function (y) { return y[0] === p; })[0] || [p, p, "x-grey"]; return '<span class="x-tag x-tag--s ' + x[2] + '">' + E(x[0]) + "</span>"; }
  function runTag(s) { var x = RUN_ST[s] || [s || "—", "x-grey"]; return '<span class="x-tag x-tag--s ' + x[1] + '">' + E(x[0]) + "</span>"; }
  function isTask(x) { return data(x).type === "task"; }
  function typeTag(x) { return isTask(x) ? '<span class="xd-type xd-type--task">任务</span>' : ""; }
  function openStatuses(k) { return KIND[k].st.map(function (s) { return s[0]; }).filter(function (s) { return DONE[k].indexOf(s) < 0; }); }

  function head(t, sub, acts) {
    return '<div class="xb-head"><div><h2>' + E(t) + "</h2>" + (sub ? "<p>" + E(sub) + "</p>" : "") + '</div><div class="xb-head-act">' + (acts || "") + "</div></div>";
  }
  function table(cols, rows, empty) {
    if (!rows.length) return '<p class="xb-empty">' + E(empty || "暂无数据") + "</p>";
    return '<div class="xb-table"><table><thead><tr>' + cols.map(function (c) { return "<th>" + c + "</th>"; }).join("") + "</tr></thead><tbody>" + rows.map(function (r) { return "<tr>" + r.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>"; }).join("") + "</tbody></table></div>";
  }
  function failBox(e) { return '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; }

  /* ---------------------------------------------------------------- 通用弹窗 / 确认 */
  function modal(title, body, foot, width) {
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true"' + (width ? ' style="width:min(' + width + 'px,calc(100vw - 32px))"' : "") + "><h3>" + E(title) + '</h3><div class="xb-confirm-body">' + body + '</div><p class="fk-err xd-err" hidden></p><div class="xb-confirm-foot">' + foot + "</div></div>";
    document.body.appendChild(m);
    var api = {
      el: m,
      close: function () { m.remove(); document.removeEventListener("keydown", esc); },
      err: function (msg) { var p = m.querySelector(".fk-err"); p.textContent = msg || ""; p.hidden = !msg; }
    };
    function esc(e) { if (e.key === "Escape") api.close(); }
    document.addEventListener("keydown", esc);
    var no = m.querySelector("[data-no]");
    if (no) no.onclick = api.close;
    return api;
  }

  // confirmDanger：二次确认；typeName 不为空时需要输入完全一致的名称才能继续。
  function confirmDanger(o) {
    var body = o.body + (o.typeName ? '<p style="margin:12px 0 6px">请输入 <b>' + E(o.typeName) + '</b> 确认：</p><input class="x-input" data-name style="width:100%" autocomplete="off">' : "");
    var m = modal(o.title, body, '<button class="x-btn" data-no>取消</button><button class="x-btn x-btn--danger" data-yes>' + E(o.okText || "删除") + "</button>");
    var yes = m.el.querySelector("[data-yes]"), inp = m.el.querySelector("[data-name]");
    if (inp) { yes.disabled = true; inp.oninput = function () { yes.disabled = inp.value.trim() !== o.typeName; }; setTimeout(function () { inp.focus(); }, 0); }
    yes.onclick = function () {
      if (inp && inp.value.trim() !== o.typeName) { m.err("名称不一致"); return; }
      yes.disabled = true;
      Promise.resolve(o.run()).then(function () { m.close(); }).catch(function (e) { yes.disabled = false; m.err(errText(e)); });
    };
  }

  function confirmDel(k, x, done) {
    var heavy = k === "iterations";
    confirmDanger({
      title: "删除" + KIND[k].name,
      body: "确定删除 <b>#" + x.id + " " + E(x.title) + "</b>？" + (heavy ? "迭代内的需求、缺陷和用例会移出迭代（不会被删除）。" : "评论、变更记录和关联会一并删除。") + "删除后不可恢复。",
      typeName: heavy ? x.title : "",
      run: function () {
        return xs().del("/devcenter/" + k + "/" + x.id, heavy ? { query: { confirm: x.title } } : undefined).then(function () { toast("已删除"); done(); });
      }
    });
  }

  /* ---------------------------------------------------------------- 用户 / 迭代选项 */
  var USERS = null, USERS_AT = 0;
  function loadUsers() {
    if (USERS && Date.now() - USERS_AT < 60000) return USERS;
    USERS_AT = Date.now();
    USERS = xs().get("/users", { page_size: 200 }).then(function (r) { return items(r).filter(function (u) { return u.username; }); }).catch(function () { return []; });
    return USERS;
  }
  function userOptions(list, cur, emptyText) {
    var seen = {}, out = '<option value="">' + E(emptyText || "未指派") + "</option>";
    list.forEach(function (u) { seen[u.username] = 1; out += '<option value="' + E(u.username) + '"' + (u.username === cur ? " selected" : "") + ">" + E((u.display_name || u.username) + (u.display_name && u.display_name !== u.username ? "（" + u.username + "）" : "")) + "</option>"; });
    [cur, me()].forEach(function (n) { if (n && !seen[n]) { seen[n] = 1; out += '<option value="' + E(n) + '"' + (n === cur ? " selected" : "") + ">" + E(n) + "</option>"; } });
    return out;
  }
  function loadIters() { return xs().get("/devcenter/iterations", { page_size: 200 }).then(items).catch(function () { return []; }); }

  /* ---------------------------------------------------------------- 表单 */
  // preset: { values: {...}, data: {...} }，用于从迭代 / 看板 / 父需求新建。
  function itemForm(k, x, done, preset) {
    preset = preset || {};
    var editing = !!x;
    var iterLoad = function () { return loadIters().then(function (list) { return [{ v: "", t: "不关联迭代" }].concat(list.filter(function (i) { return i.status !== "done" || (x && x.iteration_id === i.id); }).map(function (i) { return { v: String(i.id), t: i.title }; })); }); };
    var userLoad = function () { return loadUsers().then(function (list) { var o = [{ v: "", t: "暂不指派" }].concat(list.map(function (u) { return { v: u.username, t: (u.display_name || u.username) + "（" + u.username + "）" }; })); if (!list.length && me()) o.push({ v: me(), t: me() }); return o; }); };
    var f = [];
    if (k === "requirements") f.push({ k: "type", label: "类型", type: "radio", options: [{ v: "story", t: "需求" }, { v: "task", t: "任务" }], def: "story", help: "任务用于把需求拆成可指派的开发工作" });
    f.push({ k: "title", label: (k === "requirements" ? "" : KIND[k].name) + "标题", required: true, placeholder: { requirements: "登录页支持企业 SSO", iterations: "2026-11 第 1 迭代", bugs: "工作负载列表分页错乱", tests: "SSO 登录后跳回原页面" }[k] },
      { k: "status", label: "状态", type: "select", options: KIND[k].st.map(function (s) { return { v: s[0], t: s[1] }; }), def: KIND[k].st[0][0] });
    if (k !== "iterations") f.push({ k: "priority", label: "优先级", type: "radio", options: PRI.map(function (p) { return { v: p[0], t: p[0] + " " + p[1] }; }), def: "P2" });
    if (k !== "iterations") f.push({ k: "iteration_id", label: "所属迭代", type: "select", load: iterLoad, options: [{ v: "", t: "不关联迭代" }] });
    f.push({ k: "assignee", label: k === "iterations" ? "负责人" : "处理人", type: "select", load: userLoad, options: [{ v: "", t: "暂不指派" }] });
    if (k === "iterations") {
      f.push({ k: "start", label: "开始日期", type: "text", placeholder: "2026-11-04", help: "格式 YYYY-MM-DD，用于燃尽图" }, { k: "end", label: "结束日期", type: "text", placeholder: "2026-11-18" }, { k: "goal", label: "迭代目标", type: "textarea" });
    } else if (k === "requirements") {
      f.push({ k: "description", label: "描述", type: "textarea", placeholder: "背景、用户故事、范围" }, { k: "acceptance", label: "验收标准", type: "textarea", placeholder: "每行一条，验收时逐条核对" }, { k: "estimate", label: "预估工作量", type: "text", placeholder: "例如 2 天" });
    } else if (k === "bugs") {
      f.push({ k: "severity", label: "严重程度", type: "radio", options: [{ v: "critical", t: "致命" }, { v: "major", t: "严重" }, { v: "minor", t: "一般" }, { v: "trivial", t: "轻微" }], def: "major" },
        { k: "env", label: "发现环境", type: "select", options: [{ v: "", t: "—" }, { v: "prod", t: "生产" }, { v: "pre", t: "预发" }, { v: "test", t: "测试" }, { v: "dev", t: "开发" }] },
        { k: "description", label: "复现步骤与现象", type: "textarea", placeholder: "1. …\n2. …\n期望：…\n实际：…" });
    } else {
      f.push({ k: "requirement", label: "关联需求编号", type: "text", placeholder: "例如 12" }, { k: "steps", label: "测试步骤", type: "textarea" }, { k: "expected", label: "预期结果", type: "textarea" });
    }
    var d = x ? data(x) : Object.assign({}, preset.data || {});
    var vals = x ? { type: d.type || "story", title: x.title, status: x.status, priority: x.priority, iteration_id: x.iteration_id ? String(x.iteration_id) : "", assignee: x.assignee || "", description: x.description || "",
      start: d.start || "", end: d.end || "", goal: d.goal || "", acceptance: d.acceptance || "", estimate: d.estimate || "", severity: d.severity || "major", env: d.env || "", requirement: d.requirement || "", steps: d.steps || "", expected: d.expected || "" }
      : Object.assign({ assignee: k === "iterations" ? me() : "" }, preset.values || {});
    var toObj = function (v) {
      var o = { title: v.title, status: v.status, assignee: v.assignee || "" };
      if (k !== "iterations") { o.priority = v.priority || "P2"; o.iteration_id = v.iteration_id ? Number(v.iteration_id) : null; }
      var dd = Object.assign({}, d);
      if (k === "iterations") { dd.start = v.start || ""; dd.end = v.end || ""; dd.goal = v.goal || ""; }
      else if (k === "requirements") { o.description = v.description || ""; dd.acceptance = v.acceptance || ""; dd.estimate = v.estimate || ""; dd.type = v.type || "story"; }
      else if (k === "bugs") { o.description = v.description || ""; dd.severity = v.severity || "major"; dd.env = v.env || ""; }
      else { dd.requirement = v.requirement || ""; dd.steps = v.steps || ""; dd.expected = v.expected || ""; }
      o.data = dd;
      return o;
    };
    var title = (editing ? "编辑" : "新建") + (k === "requirements" ? (d.type === "task" || (preset.values && preset.values.type === "task") ? "任务" : "需求") : KIND[k].name) + (editing ? " · #" + x.id : "");
    root.XSFormKit.open({
      title: title, okText: editing ? "保存" : "创建", scene: "devcenter." + k, width: "lg",
      aiExample: { requirements: "P1 需求：登录页支持企业 SSO，验收标准包括自动建号和失败提示", iterations: "11 月第一个迭代，11-04 到 11-18，目标上线企业 SSO", bugs: "生产环境工作负载列表翻页后数据重复，严重", tests: "验证 SSO 登录后回到原来的页面" }[k],
      sections: [{ fields: f }], values: vals, toObject: toObj,
      fromObject: function (o, v) { var dd = o.data || {}; return Object.assign({}, v, { title: o.title, status: o.status, priority: o.priority, assignee: o.assignee, description: o.description, iteration_id: o.iteration_id ? String(o.iteration_id) : "" }, dd); },
      validate: function (v) {
        if (!v.title || !String(v.title).trim()) return "请填写标题";
        if (String(v.title).trim().length > 200) return "标题不能超过 200 个字";
        if (k === "iterations") {
          var re = /^\d{4}-\d{2}-\d{2}$/;
          if (v.start && !re.test(v.start)) return "开始日期格式应为 YYYY-MM-DD";
          if (v.end && !re.test(v.end)) return "结束日期格式应为 YYYY-MM-DD";
          if (v.start && v.end && v.end < v.start) return "结束日期不能早于开始日期";
        }
        if (k === "tests" && v.requirement && !/^\d+$/.test(String(v.requirement).trim())) return "关联需求编号应为数字";
        return "";
      },
      submit: function (v, obj, ctx) {
        var p = editing ? xs().put("/devcenter/" + k + "/" + x.id, obj) : xs().post("/devcenter/" + k, obj);
        return p.then(function (res) { ctx.close(); toast(editing ? "已保存" : "已创建"); done(res); });
      }
    });
  }

  function setStatus(k, x, s, done, extra) {
    var body = { status: s };
    if (extra) body.data = Object.assign({}, data(x), extra);
    return xs().patch("/devcenter/" + k + "/" + x.id, body).then(function () { toast("已更新为「" + stOf(k, s)[1] + "」"); done(); }).catch(function (e) { toast("更新失败：" + errText(e)); });
  }

  function acceptDialog(x, done) {
    var crit = String(data(x).acceptance || "").split(/\n+/).map(function (s) { return s.trim(); }).filter(Boolean);
    var m = modal("验收需求 · #" + x.id + " " + x.title,
      (crit.length ? '<p class="x-frac" style="margin:0 0 8px">逐条核对验收标准：</p>' + crit.map(function (c, i) { return '<label style="display:flex;gap:8px;align-items:flex-start;margin:6px 0"><input type="checkbox" data-c="' + i + '" style="width:auto;margin-top:4px"><span>' + E(c) + "</span></label>"; }).join("") : '<p class="x-frac">这条需求没有填写验收标准。</p>') +
      '<textarea class="x-input" data-note rows="3" placeholder="验收意见（驳回时必填）" style="width:100%;margin-top:10px"></textarea>',
      '<button class="x-btn" data-no>取消</button><button class="x-btn" data-reject>驳回重做</button><button class="x-btn x-btn--pri" data-pass>验收通过</button>', 560);
    var note = m.el.querySelector("[data-note]");
    var go = function (s) {
      var rec = { accepted_by: me(), accepted_at: new Date().toISOString(), acceptance_note: note.value.trim(), acceptance_result: s === "done" ? "pass" : "reject" };
      xs().patch("/devcenter/requirements/" + x.id, { status: s, data: Object.assign({}, data(x), rec) }).then(function () { m.close(); toast(s === "done" ? "验收通过，需求已完成" : "已驳回，需求退回开发中"); done(); })
        .catch(function (e) { m.err(errText(e)); });
    };
    m.el.querySelector("[data-pass]").onclick = function () {
      var unchecked = [].filter.call(m.el.querySelectorAll("[data-c]"), function (b) { return !b.checked; }).length;
      if (unchecked) { m.err("还有 " + unchecked + " 条验收标准未勾选"); return; }
      go("done");
    };
    m.el.querySelector("[data-reject]").onclick = function () { if (!note.value.trim()) { m.err("驳回时请填写验收意见"); return; } go("doing"); };
  }

  /* ---------------------------------------------------------------- 详情抽屉 */
  var DR = null; // { k, id, tab, onChange, el }
  function closeDrawer() {
    if (!DR) return;
    DR.mask.remove(); DR.el.remove();
    document.removeEventListener("keydown", DR.esc);
    DR = null;
  }
  function openDetail(k, id, onChange) {
    closeDrawer();
    var mask = document.createElement("div"), el = document.createElement("aside");
    mask.className = "xd-mask"; el.className = "xd-drawer"; el.setAttribute("role", "dialog"); el.setAttribute("aria-modal", "true");
    document.body.appendChild(mask); document.body.appendChild(el);
    DR = { k: k, id: id, tab: "comments", onChange: onChange || function () {}, el: el, mask: mask, esc: function (e) { if (e.key === "Escape" && !document.querySelector(".xb-confirm, .fk-mask, .fk-modal")) closeDrawer(); } };
    mask.onclick = closeDrawer;
    document.addEventListener("keydown", DR.esc);
    drawDetail();
  }
  function changed() { if (!DR) return; var cb = DR.onChange; drawDetail(); cb(); }

  function drawDetail() {
    if (!DR) return;
    var k = DR.k, id = DR.id, el = DR.el, my = DR;
    el.innerHTML = '<div class="xd-db"><p class="x-frac">正在加载…</p></div>';
    var linkable = k === "requirements" || k === "bugs";
    Promise.all([
      xs().get("/devcenter/" + k + "/" + id),
      xs().get("/devcenter/" + k + "/" + id + "/comments").catch(function () { return { items: [] }; }),
      xs().get("/devcenter/" + k + "/" + id + "/activities").catch(function () { return { items: [] }; }),
      linkable ? xs().get("/devcenter/" + k + "/" + id + "/links").catch(function () { return { items: [] }; }) : Promise.resolve({ items: [] }),
      k === "requirements" ? xs().get("/devcenter/requirements", { parent_id: id, page_size: 100 }).catch(function () { return { items: [] }; }) : Promise.resolve({ items: [] }),
      k === "iterations" ? Promise.resolve([]) : loadIters(),
      loadUsers()
    ]).then(function (r) {
      if (DR !== my) return;
      var x = r[0], comments = items(r[1]), acts = items(r[2]), links = items(r[3]), kids = items(r[4]), iters = r[5], users = r[6];
      var d = data(x), iterName = {};
      iters.forEach(function (i) { iterName[i.id] = i.title; });
      var kindName = k === "requirements" ? (isTask(x) ? "任务" : "需求") : KIND[k].name;
      var acts1 = '<button class="x-btn" data-edit>编辑</button>' + (k === "requirements" ? '<button class="x-btn" data-ai>AI 开发</button>' : "") + '<button class="x-btn xd-btn-del" data-del>删除</button><button class="x-btn" data-close aria-label="关闭">关闭</button>';
      var idx = KIND[k].st.map(function (s) { return s[0]; }).indexOf(x.status);
      var flow = '<div class="xd-flow" role="group" aria-label="状态流转">' + KIND[k].st.map(function (s, i) {
        var cls = s[0] === x.status ? "on" : (i < idx && x.status !== "rejected" && s[0] !== "rejected" ? "past" : "");
        return '<button type="button" data-st="' + s[0] + '" class="' + cls + '"' + (s[0] === x.status ? " disabled" : "") + ">" + E(s[1]) + "</button>";
      }).join("") + "</div>";
      var meta = '<div class="xd-meta">';
      if (k !== "iterations") meta += '<label>优先级<select class="x-input" data-f="priority">' + PRI.map(function (p) { return '<option value="' + p[0] + '"' + (p[0] === x.priority ? " selected" : "") + ">" + p[0] + " " + p[1] + "</option>"; }).join("") + "</select></label>";
      meta += "<label>" + (k === "iterations" ? "负责人" : "处理人") + '<select class="x-input" data-f="assignee">' + userOptions(users, x.assignee || "") + "</select></label>";
      if (k !== "iterations") meta += '<label>所属迭代<select class="x-input" data-f="iteration_id"><option value="">不关联迭代</option>' + iters.filter(function (i) { return i.status !== "done" || i.id === x.iteration_id; }).map(function (i) { return '<option value="' + i.id + '"' + (i.id === x.iteration_id ? " selected" : "") + ">" + E(i.title) + "</option>"; }).join("") + "</select></label>";
      if (k === "iterations") meta += '<label>周期<span class="xd-ro">' + E((d.start || "—") + " → " + (d.end || "—")) + "</span></label>";
      meta += '<label>最后修改<span class="xd-ro">' + E((x.updated_by || "—") + " · " + fmt(x.updated_at)) + '</span></label><label>创建时间<span class="xd-ro">' + fmt(x.created_at) + "</span></label>";
      if (k === "requirements" && d.estimate) meta += '<label>预估工作量<span class="xd-ro">' + E(d.estimate) + "</span></label>";
      if (k === "bugs") meta += '<label>严重程度 / 环境<span class="xd-ro">' + E((SEV[d.severity] || "—") + " · " + ({ prod: "生产", pre: "预发", test: "测试", dev: "开发" }[d.env] || "—")) + "</span></label>";
      if (k === "requirements" && isTask(x) && d.parent_id) meta += '<label>所属需求<span class="xd-ro"><a class="xd-link" data-parent="' + E(d.parent_id) + '">#' + E(d.parent_id) + " 查看</a></span></label>";
      meta += "</div><p class=\"xd-err\" data-merr hidden></p>";

      var body = "";
      if (k === "iterations") {
        if (d.goal) body += '<div class="xd-sec"><h4>迭代目标</h4><p class="xd-text">' + E(d.goal) + "</p></div>";
        body += '<div class="xd-sec"><h4>燃尽图</h4><div data-burn><p class="x-frac">正在计算…</p></div></div><div class="xd-sec"><h4>迭代内需求</h4><div data-iteritems><p class="x-frac">正在加载…</p></div></div>';
      } else {
        if (x.description) body += '<div class="xd-sec"><h4>' + (k === "bugs" ? "复现步骤与现象" : "描述") + '</h4><p class="xd-text">' + E(x.description) + "</p></div>";
        if (k === "requirements" && d.acceptance) body += '<div class="xd-sec"><h4>验收标准</h4><ul class="xd-list">' + String(d.acceptance).split(/\n+/).filter(function (s) { return s.trim(); }).map(function (s) { return "<li>" + E(s.trim()) + "</li>"; }).join("") + "</ul>" +
          (d.acceptance_result ? '<p class="x-frac" style="margin:6px 0 0">' + (d.acceptance_result === "pass" ? "验收通过" : "验收驳回") + " · " + E(d.accepted_by || "") + " · " + fmt(d.accepted_at) + (d.acceptance_note ? " · " + E(d.acceptance_note) : "") + "</p>" : "") + "</div>";
        if (k === "tests") body += '<div class="xd-sec"><h4>测试步骤</h4><p class="xd-text">' + E(d.steps || "未填写") + '</p></div><div class="xd-sec"><h4>预期结果</h4><p class="xd-text">' + E(d.expected || "未填写") + "</p></div>" + (d.requirement ? '<p class="x-frac">关联需求 <a class="xd-link" data-parent="' + E(d.requirement) + '">#' + E(d.requirement) + "</a></p>" : "");
        if (k === "requirements" && !isTask(x)) {
          var doneKids = kids.filter(function (c) { return DONE.requirements.indexOf(c.status) >= 0; }).length;
          body += '<div class="xd-sec"><h4>拆分任务 <span class="x-frac">' + doneKids + "/" + kids.length + '</span><button class="x-btn x-btn--s" data-addtask>+ 拆分任务</button></h4>' +
            (kids.length ? '<ul class="xd-list">' + kids.map(function (c) { return '<li><span class="x-mono">#' + c.id + "</span>" + stTag("requirements", c.status) + '<a class="xd-link xd-grow" data-kid="' + c.id + '">' + E(c.title) + '</a><span class="x-frac">' + E(c.assignee || "未指派") + "</span></li>"; }).join("") + "</ul>" : '<p class="x-frac" style="margin:0">还没有拆分任务。可以手动拆分，或用「AI 开发」生成任务拆解。</p>') + "</div>";
        }
        if (linkable) body += '<div class="xd-sec"><h4>代码仓库与流水线<button class="x-btn x-btn--s" data-addlink>+ 关联</button></h4><div data-links>' + (links.length ? '<p class="x-frac">正在读取构建状态…</p>' : '<p class="x-frac" style="margin:0">还没有关联。关联后可在这里看到最近一次构建结果。</p>') + "</div></div>";
      }
      body += '<div class="xd-sec"><div class="xd-tabs"><button type="button" data-tab="comments"' + (DR.tab === "comments" ? ' class="on"' : "") + ">评论 " + comments.length + '</button><button type="button" data-tab="acts"' + (DR.tab === "acts" ? ' class="on"' : "") + ">变更记录 " + acts.length + "</button></div>" +
        (DR.tab === "comments" ? (comments.length ? comments.map(function (c) { return '<div class="xd-cm"><div class="xd-cm-h"><b>' + E(c.author || "—") + "</b><span>" + E(ago(c.created_at)) + "</span>" + (c.mine ? '<button class="x-act x-act--danger" data-delcm="' + c.id + '">删除</button>' : "") + '</div><div class="xd-cm-b">' + E(c.body) + "</div></div>"; }).join("") : '<p class="x-frac">还没有评论。</p>') +
          '<div class="xd-cbox"><textarea class="x-input" data-cm placeholder="写下评论，最多 4000 字" maxlength="4000"></textarea><p class="xd-err" data-cmerr hidden></p><div class="xd-cbox-f"><button class="x-btn x-btn--pri" data-sendcm>发表评论</button></div></div>'
          : (acts.length ? acts.map(function (a) { return '<div class="xd-act"><time>' + fmt(a.created_at) + "</time><span><b>" + E(a.actor || "系统") + "</b> " + actText(k, a, iterName) + "</span></div>"; }).join("") : '<p class="x-frac">暂无变更记录。</p>')) + "</div>";

      el.innerHTML = '<div class="xd-dh"><div class="xd-dh-main"><span class="x-mono x-frac">' + E(kindName) + " #" + x.id + "</span> " + stTag(k, x.status) + (k !== "iterations" ? " " + priTag(x.priority) : "") + "<h3>" + E(x.title) + '</h3></div><div class="xd-dh-act">' + acts1 + '</div></div><div class="xd-db">' + flow + meta + body + "</div>";

      var redraw = function () { changed(); };
      el.querySelector("[data-close]").onclick = closeDrawer;
      el.querySelector("[data-edit]").onclick = function () { itemForm(k, x, redraw); };
      el.querySelector("[data-del]").onclick = function () { confirmDel(k, x, function () { var cb = DR && DR.onChange; closeDrawer(); if (cb) cb(); }); };
      var aiB = el.querySelector("[data-ai]");
      if (aiB) aiB.onclick = function () { aiLaunch(x); };
      on(el, "[data-st]", function (b) {
        var s = b.getAttribute("data-st");
        if (k === "requirements" && s === "done" && x.status === "accepting") return acceptDialog(x, redraw);
        setStatus(k, x, s, redraw);
      });
      [].forEach.call(el.querySelectorAll("[data-f]"), function (s) {
        s.onchange = function () {
          var f = s.getAttribute("data-f"), body = {};
          body[f] = f === "iteration_id" ? (s.value ? Number(s.value) : null) : s.value;
          xs().patch("/devcenter/" + k + "/" + x.id, body).then(function () { toast(f === "assignee" ? (s.value ? "已指派给 " + s.value : "已取消指派") : "已更新"); redraw(); })
            .catch(function (e) { var p = el.querySelector("[data-merr]"); p.textContent = "更新失败：" + errText(e); p.hidden = false; });
        };
      });
      on(el, "[data-parent]", function (b) { openDetail("requirements", b.getAttribute("data-parent"), DR.onChange); });
      on(el, "[data-kid]", function (b) { openDetail("requirements", b.getAttribute("data-kid"), DR.onChange); });
      var addTask = el.querySelector("[data-addtask]");
      if (addTask) addTask.onclick = function () { itemForm("requirements", null, redraw, { values: { type: "task", iteration_id: x.iteration_id ? String(x.iteration_id) : "", priority: x.priority }, data: { type: "task", parent_id: x.id } }); };
      on(el, "[data-tab]", function (b) { DR.tab = b.getAttribute("data-tab"); drawDetail(); });
      var send = el.querySelector("[data-sendcm]");
      if (send) send.onclick = function () {
        var ta = el.querySelector("[data-cm]"), er = el.querySelector("[data-cmerr]"), v = ta.value.trim();
        if (!v) { er.textContent = "请输入评论内容"; er.hidden = false; return; }
        send.disabled = true;
        xs().post("/devcenter/" + k + "/" + x.id + "/comments", { body: v }).then(function () { toast("评论已发表"); drawDetail(); })
          .catch(function (e) { send.disabled = false; er.textContent = errText(e); er.hidden = false; });
      };
      on(el, "[data-delcm]", function (b) {
        confirmDanger({ title: "删除评论", body: "确定删除这条评论？删除后不可恢复。", run: function () { return xs().del("/devcenter/comments/" + b.getAttribute("data-delcm")).then(function () { toast("评论已删除"); drawDetail(); }); } });
      });
      var addLink = el.querySelector("[data-addlink]");
      if (addLink) addLink.onclick = function () { linkDialog(k, x, redraw); };
      if (linkable && links.length) drawLinks(el.querySelector("[data-links]"), links, redraw);
      if (k === "iterations") { drawBurndown(el.querySelector("[data-burn]"), x.id); drawIterItems(el.querySelector("[data-iteritems]"), x.id); }
    }).catch(function (e) { if (DR === my) el.innerHTML = '<div class="xd-dh"><div class="xd-dh-main"><h3>' + E(KIND[k].name) + " #" + E(id) + '</h3></div><div class="xd-dh-act"><button class="x-btn" data-close>关闭</button></div></div><div class="xd-db">' + failBox(e) + "</div>"; var c = el.querySelector("[data-close]"); if (c) c.onclick = closeDrawer; });
  }

  function actText(k, a, iterName) {
    if (a.action === "create") return "创建，初始状态「" + E(stOf(k, a.to)[1]) + "」";
    if (a.action === "status") return "状态 " + E(stOf(k, a.from)[1]) + " → <b>" + E(stOf(k, a.to)[1]) + "</b>";
    if (a.action === "assign") return a.to ? "指派给 <b>" + E(a.to) + "</b>" + (a.from ? "（原 " + E(a.from) + "）" : "") : "取消指派（原 " + E(a.from) + "）";
    if (a.action === "iteration") { var n = function (v) { return v ? (iterName[v] || "#" + v) : "无"; }; return "迭代 " + E(n(a.from)) + " → <b>" + E(n(a.to)) + "</b>"; }
    return E(a.action);
  }

  // 读取流水线列表（含最近一次运行）用于展示关联的构建状态；没有 CI/CD 权限时返回 null。
  function loadPipelines() { return xs().get("/cicd/pipelines", { page_size: 200 }).then(items).catch(function () { return null; }); }
  function lastRunOf(link, pipes) {
    if (!pipes) return null;
    var cands = pipes.filter(function (p) { return link.ref_type === "pipeline" ? p.id === link.ref_id : p.app_id === link.ref_id; });
    var best = null;
    cands.forEach(function (p) { var lr = p.stats && p.stats.last_run; if (lr && (!best || new Date(lr.created_at) > new Date(best.created_at))) best = Object.assign({ pipeline_id: p.id }, lr); });
    return best;
  }
  function drawLinks(box, links, redraw) {
    loadPipelines().then(function (pipes) {
      box.innerHTML = '<ul class="xd-list">' + links.map(function (l) {
        var lr = lastRunOf(l, pipes);
        var href = l.ref_type === "pipeline" ? "#/cicd/pipeline/" + encodeURIComponent(l.ref_id) : (lr ? "#/cicd/pipeline/" + encodeURIComponent(lr.pipeline_id) : "#/coding/repos");
        return '<li><span class="x-tag x-tag--s x-grey">' + (l.ref_type === "pipeline" ? "流水线" : "代码仓库") + '</span><a class="xd-link xd-grow" href="' + href + '">' + E(l.ref_name) + "</a>" +
          (lr ? runTag(lr.status) + '<span class="x-frac">#' + E(lr.id) + " · " + E(lr.branch || "") + " · " + E(ago(lr.created_at)) + "</span>" : '<span class="x-frac">' + (pipes ? "暂无构建" : "无权查看构建") + "</span>") +
          '<button class="x-act x-act--danger" data-unlink="' + l.id + '" data-name="' + E(l.ref_name) + '">解除</button></li>';
      }).join("") + "</ul>";
      on(box, "[data-unlink]", function (b) {
        confirmDanger({ title: "解除关联", okText: "解除", body: "确定解除与 <b>" + E(b.getAttribute("data-name")) + "</b> 的关联？代码仓库和流水线本身不受影响。", run: function () { return xs().del("/devcenter/links/" + b.getAttribute("data-unlink")).then(function () { toast("已解除关联"); redraw(); }); } });
      });
    });
  }

  function linkDialog(k, x, done) {
    var m = modal("关联代码仓库或流水线 · #" + x.id, '<div class="xd-form"><label>类型<select class="x-input" data-t><option value="app">代码仓库（CI/CD 应用）</option><option value="pipeline">流水线</option></select></label><label>选择<select class="x-input" data-r><option value="">正在加载…</option></select></label></div>',
      '<button class="x-btn" data-no>取消</button><button class="x-btn x-btn--pri" data-ok disabled>关联</button>', 480);
    var tSel = m.el.querySelector("[data-t]"), rSel = m.el.querySelector("[data-r]"), ok = m.el.querySelector("[data-ok]");
    Promise.all([xs().get("/cicd/applications", { page_size: 200 }).then(items), loadPipelines()]).then(function (r) {
      var apps = r[0], pipes = r[1] || [], appName = {};
      apps.forEach(function (a) { appName[a.id] = a.name; });
      var fill = function () {
        var list = tSel.value === "app" ? apps.map(function (a) { return { id: a.id, name: a.name, sub: a.repo_url || "" }; }) : pipes.map(function (p) { return { id: p.id, name: (appName[p.app_id] ? appName[p.app_id] + " / " : "") + p.name, sub: "" }; });
        rSel.innerHTML = list.length ? '<option value="">请选择</option>' + list.map(function (o) { return '<option value="' + o.id + '" data-n="' + E(o.name) + '">' + E(o.name + (o.sub ? "（" + o.sub + "）" : "")) + "</option>"; }).join("") : '<option value="">' + (tSel.value === "app" ? "还没有接入代码仓库，请先在 CI/CD 创建应用" : "还没有流水线") + "</option>";
        ok.disabled = true;
      };
      tSel.onchange = fill; rSel.onchange = function () { ok.disabled = !rSel.value; m.err(""); };
      fill();
    }).catch(function (e) { rSel.innerHTML = '<option value="">读取失败</option>'; m.err(errText(e)); });
    ok.onclick = function () {
      if (!rSel.value) { m.err("请选择要关联的对象"); return; }
      var opt = rSel.options[rSel.selectedIndex];
      ok.disabled = true;
      xs().post("/devcenter/" + k + "/" + x.id + "/links", { ref_type: tSel.value, ref_id: Number(rSel.value), ref_name: opt.getAttribute("data-n") }).then(function () { m.close(); toast("已关联"); done(); })
        .catch(function (e) { ok.disabled = false; m.err(errText(e)); });
    };
  }

  /* ---------------------------------------------------------------- 燃尽图 */
  function burnSvg(r) {
    var pts = r.points || [];
    if (!pts.length) return '<p class="x-frac">迭代没有可计算的日期。</p>';
    var W = 640, H = 200, L = 32, B = 22, T = 10, max = 1;
    pts.forEach(function (p) { max = Math.max(max, p.scope || 0, p.ideal || 0); });
    var x = function (i) { return L + (pts.length === 1 ? 0 : i * (W - L - 8) / (pts.length - 1)); }, y = function (v) { return T + (H - T - B) * (1 - v / max); };
    var ideal = pts.map(function (p, i) { return p.ideal == null ? "" : x(i) + "," + y(p.ideal); }).filter(Boolean).join(" ");
    var today = new Date(); today = today.getFullYear() + "-" + pad(today.getMonth() + 1) + "-" + pad(today.getDate());
    var actual = pts.filter(function (p) { return p.date <= today; }).map(function (p, i) { return x(i) + "," + y(p.remaining); }).join(" ");
    var ticks = "";
    var step = Math.max(1, Math.ceil(pts.length / 8));
    pts.forEach(function (p, i) { if (i % step === 0 || i === pts.length - 1) ticks += '<text x="' + x(i) + '" y="' + (H - 6) + '" text-anchor="middle">' + E(p.date.slice(5)) + "</text>"; });
    ticks += '<text x="4" y="' + (y(max) + 4) + '">' + max + '</text><text x="4" y="' + (y(0) + 4) + '">0</text>';
    return '<svg class="xd-burn" viewBox="0 0 ' + W + " " + H + '" role="img" aria-label="燃尽图"><line x1="' + L + '" y1="' + y(0) + '" x2="' + (W - 8) + '" y2="' + y(0) + '" stroke="#e5e9f0"/>' +
      '<polyline points="' + ideal + '" fill="none" stroke="#b5c7e8" stroke-dasharray="4 4" stroke-width="1.5"/><polyline points="' + actual + '" fill="none" stroke="#1664ff" stroke-width="2"/>' + ticks + "</svg>" +
      '<div class="xd-legend"><span><i class="xd-c1"></i>剩余需求</span><span><i class="xd-c3"></i>理想线</span>' + (r.dates_from_iteration ? "" : '<span class="x-frac">迭代未填写起止日期，按创建日起 14 天估算</span>') + "</div>";
  }
  function drawBurndown(box, id) {
    xs().get("/devcenter/iterations/" + id + "/burndown", tzq()).then(function (r) { box.innerHTML = burnSvg(r); }).catch(function (e) { box.innerHTML = failBox(e); });
  }
  function drawIterItems(box, id) {
    Promise.all([xs().get("/devcenter/requirements", { iteration_id: id, page_size: 500 }), xs().get("/devcenter/bugs", { iteration_id: id, page_size: 500 })]).then(function (r) {
      var list = items(r[0]).map(function (x) { return ["requirements", x]; }).concat(items(r[1]).map(function (x) { return ["bugs", x]; }));
      box.innerHTML = list.length ? '<ul class="xd-list">' + list.map(function (p) { var x = p[1]; return '<li><span class="x-mono">' + (p[0] === "bugs" ? "缺陷" : "") + "#" + x.id + "</span>" + stTag(p[0], x.status) + typeTag(x) + '<a class="xd-link xd-grow" data-k="' + p[0] + '" data-id="' + x.id + '">' + E(x.title) + '</a><span class="x-frac">' + E(x.assignee || "未指派") + "</span></li>"; }).join("") + "</ul>" : '<p class="x-frac">迭代内还没有需求或缺陷。</p>';
      on(box, "[data-k]", function (b) { openDetail(b.getAttribute("data-k"), b.getAttribute("data-id"), DR && DR.onChange); });
    }).catch(function (e) { box.innerHTML = failBox(e); });
  }

  /* ---------------------------------------------------------------- 列表页（需求 / 缺陷 / 测试） */
  var LS = { requirements: { tab: "open", q: "", pri: "", type: "", iter: "", off: 0 }, bugs: { tab: "open", q: "", pri: "", iter: "", off: 0 }, tests: { tab: "all", q: "", pri: "", iter: "", off: 0 } };
  var PAGE = 50;

  function listPage(el, k, opts) {
    opts = opts || {};
    var st = LS[k];
    if (opts.tab && !st._forced) { st.tab = opts.tab; st._forced = true; }
    var tabs = k === "requirements" ? [["open", "进行中"], ["accepting", "待验收"], ["mine", "我负责"], ["done", "已完成"], ["all", "全部"]]
      : k === "bugs" ? [["open", "未解决"], ["mine", "我负责"], ["fixed", "待验证"], ["all", "全部"]]
        : [["all", "全部"], ["todo", "未执行"], ["failed", "失败"], ["mine", "我负责"]];
    var title = opts.title || { requirements: "需求管理", bugs: "缺陷总览", tests: "测试管理" }[k];
    var sub = { requirements: "需求与任务从评审、开发、测试到验收的全过程；点击标题查看详情、评论和关联的代码仓库。", bugs: "集中跟踪缺陷，修复后由提出人验证关闭。", tests: "维护测试用例并记录每次执行结果。" }[k];
    var acts = '<button class="x-btn" data-refresh>刷新</button>' + (k === "requirements" ? '<a class="x-btn" href="#/project/board">看板视图</a>' : "") + '<button class="x-btn x-btn--pri" data-new>+ 新建' + KIND[k].name + "</button>";
    el.innerHTML = head(title, sub, acts) +
      '<div class="xb-main"><div class="xb-toolbar"><div class="x-tabs" role="tablist">' + tabs.map(function (t) { return '<button type="button" data-tab="' + t[0] + '"' + (st.tab === t[0] ? ' class="on"' : "") + ">" + t[1] + "</button>"; }).join("") +
      '</div><input class="x-input" data-q placeholder="搜索编号、标题或描述，回车确认" value="' + E(st.q) + '"></div>' +
      '<div class="xd-filters">' + (k === "requirements" ? '<select class="x-input" data-fl="type" aria-label="类型"><option value="">全部类型</option><option value="story"' + (st.type === "story" ? " selected" : "") + '>需求</option><option value="task"' + (st.type === "task" ? " selected" : "") + ">任务</option></select>" : "") +
      '<select class="x-input" data-fl="pri" aria-label="优先级"><option value="">全部优先级</option>' + PRI.map(function (p) { return '<option value="' + p[0] + '"' + (st.pri === p[0] ? " selected" : "") + ">" + p[0] + " " + p[1] + "</option>"; }).join("") + "</select>" +
      '<select class="x-input" data-fl="iter" aria-label="迭代"><option value="">全部迭代</option></select></div>' +
      '<div data-body><p class="x-frac">正在加载…</p></div></div>';
    var reload = function () { listPage(el, k, opts); };
    el.querySelector("[data-new]").onclick = function () { itemForm(k, null, function (res) { reload(); if (res && res.id) openDetail(k, res.id, reload); }, st.iter ? { values: { iteration_id: st.iter } } : null); };
    el.querySelector("[data-refresh]").onclick = reload;
    [].forEach.call(el.querySelectorAll("[data-tab]"), function (b) { b.onclick = function () { st.tab = b.getAttribute("data-tab"); st.off = 0; reload(); }; });
    [].forEach.call(el.querySelectorAll("[data-fl]"), function (s) { s.onchange = function () { st[s.getAttribute("data-fl")] = s.value; st.off = 0; reload(); }; });
    var qi = el.querySelector("[data-q]");
    qi.onkeydown = function (e) { if (e.key === "Enter") { st.q = qi.value.trim(); st.off = 0; reload(); } };
    var q = { page_size: PAGE, offset: st.off, q: st.q, priority: st.pri, iteration_id: st.iter, type: k === "requirements" ? st.type : "" };
    if (st.tab === "mine") { q.assignee = "me"; q.status = openStatuses(k).join(","); }
    else if (st.tab === "open") q.status = openStatuses(k).join(",");
    else if (st.tab === "done") q.status = DONE[k].join(",");
    else if (st.tab !== "all") q.status = st.tab;
    Promise.all([xs().get("/devcenter/" + k, q), loadIters()]).then(function (r) {
      var list = items(r[0]), iters = {}, total = (r[0] && r[0].total) != null ? r[0].total : list.length;
      r[1].forEach(function (i) { iters[i.id] = i.title; });
      var isel = el.querySelector('[data-fl="iter"]');
      isel.innerHTML = '<option value="">全部迭代</option>' + r[1].map(function (i) { return '<option value="' + i.id + '"' + (String(i.id) === String(st.iter) ? " selected" : "") + ">" + E(i.title) + "</option>"; }).join("");
      var rows = list.map(function (x) {
        var acts = "";
        if (k === "requirements" && x.status === "accepting") acts = '<button class="x-btn x-btn--pri x-btn--s" data-accept="' + x.id + '">验收</button>';
        else if (k === "requirements") { var nx = { todo: "ready", ready: "doing", doing: "testing", testing: "accepting" }[x.status]; if (nx) acts = '<button class="x-act" data-next="' + x.id + '" data-to="' + nx + '">→ ' + stOf(k, nx)[1] + "</button>"; }
        if (k === "bugs") { var bn = { todo: "doing", reopened: "doing", doing: "fixed", fixed: "verified" }[x.status]; if (bn) acts = '<button class="x-act" data-next="' + x.id + '" data-to="' + bn + '">→ ' + stOf(k, bn)[1] + "</button>"; if (x.status === "fixed") acts += '<button class="x-act" data-next="' + x.id + '" data-to="reopened">重新打开</button>'; }
        if (k === "tests") acts = '<button class="x-act" data-next="' + x.id + '" data-to="passed">通过</button><button class="x-act" data-next="' + x.id + '" data-to="failed">失败</button>';
        acts += '<button class="x-act" data-edit="' + x.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + x.id + '">删除</button>';
        var extra = k === "bugs" ? E(SEV[data(x).severity] || "—") : k === "tests" ? (data(x).requirement ? "#" + E(data(x).requirement) : "—") : (x.iteration_id ? E(iters[x.iteration_id] || "#" + x.iteration_id) : "—");
        return ['<span class="x-mono">#' + x.id + "</span>", typeTag(x) + '<span class="xd-name" data-open="' + x.id + '" role="button" tabindex="0">' + E(x.title) + "</span>", stTag(k, x.status), priTag(x.priority), extra, E(x.assignee || "—"), fmt(x.updated_at), '<span class="xb-ops">' + acts + "</span>"];
      });
      var extraCol = k === "bugs" ? "严重程度" : k === "tests" ? "关联需求" : "迭代";
      var filtered = st.q || st.pri || st.iter || st.type;
      el.querySelector("[data-body]").innerHTML = table(["编号", "标题", "状态", "优先级", extraCol, "处理人", "更新时间", "操作"], rows,
        filtered ? "没有符合筛选条件的" + KIND[k].name + "。" : st.tab === "accepting" ? "没有待验收的需求。" : st.tab === "mine" ? "没有指派给你的未完成" + KIND[k].name + "。" : "还没有" + KIND[k].name + "，点右上角新建。") +
        '<div class="xd-pager"><span class="x-frac">共 ' + total + " 条" + (total > PAGE ? "，第 " + (Math.floor(st.off / PAGE) + 1) + " / " + Math.ceil(total / PAGE) + " 页" : "") + "</span>" +
        (total > PAGE ? '<span><button class="x-btn x-btn--s" data-pg="-1"' + (st.off <= 0 ? " disabled" : "") + '>上一页</button> <button class="x-btn x-btn--s" data-pg="1"' + (st.off + PAGE >= total ? " disabled" : "") + ">下一页</button></span>" : "") + "</div>";
      var byId = function (id) { return list.filter(function (x) { return String(x.id) === String(id); })[0]; };
      on(el, "[data-edit]", function (b) { itemForm(k, byId(b.getAttribute("data-edit")), reload); });
      on(el, "[data-open]", function (b) { openDetail(k, b.getAttribute("data-open"), reload); });
      [].forEach.call(el.querySelectorAll("[data-open]"), function (b) { b.onkeydown = function (e) { if (e.key === "Enter") openDetail(k, b.getAttribute("data-open"), reload); }; });
      on(el, "[data-del]", function (b) { confirmDel(k, byId(b.getAttribute("data-del")), reload); });
      on(el, "[data-next]", function (b) { setStatus(k, byId(b.getAttribute("data-next")), b.getAttribute("data-to"), reload); });
      on(el, "[data-accept]", function (b) { acceptDialog(byId(b.getAttribute("data-accept")), reload); });
      on(el, "[data-pg]", function (b) { st.off = Math.max(0, st.off + Number(b.getAttribute("data-pg")) * PAGE); reload(); });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  /* ---------------------------------------------------------------- 工作台 */
  function greeting(h) {
    if (h < 5) return "夜深了";
    if (h < 9) return "早上好";
    if (h < 12) return "上午好";
    if (h < 14) return "中午好";
    if (h < 18) return "下午好";
    return "晚上好";
  }
  var WB = { tab: "todo" };
  function workbench(el) {
    var u = xs().user || {};
    el.innerHTML = head(greeting(new Date().getHours()) + "，" + (u.display_name || u.username || ""), "指派给你的需求、缺陷和测试，你创建和参与的事项，以及相关的最近构建。", '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-newreq>+ 新建需求</button>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { workbench(el); };
    el.querySelector("[data-refresh]").onclick = reload;
    el.querySelector("[data-newreq]").onclick = function () { itemForm("requirements", null, function (res) { reload(); if (res && res.id) openDetail("requirements", res.id, reload); }); };
    var tq = { todo: null, created: null, involved: null };
    Promise.all([
      xs().get("/devcenter/summary"),
      xs().get("/devcenter/requirements", { assignee: "me", status: openStatuses("requirements").join(","), page_size: 50 }),
      xs().get("/devcenter/bugs", { assignee: "me", status: openStatuses("bugs").join(","), page_size: 50 }),
      xs().get("/devcenter/tests", { assignee: "me", status: "todo,failed,blocked", page_size: 50 }),
      xs().get("/devcenter/iterations", { status: "doing", page_size: 10 }),
      xs().get("/devcenter/requirements", { created_by: "me", page_size: 20 }),
      xs().get("/devcenter/requirements", { involved: "me", page_size: 20 })
    ]).then(function (r) {
      var s = r[0] || {}, rq = s.requirements || { by_status: {} }, bg = s.bugs || { by_status: {} }, ts = s.tests || { by_status: {} };
      tq.todo = items(r[1]).map(function (x) { return ["requirements", x]; }).concat(items(r[2]).map(function (x) { return ["bugs", x]; }), items(r[3]).map(function (x) { return ["tests", x]; }));
      tq.todo.sort(function (a, b) { return a[1].priority === b[1].priority ? (new Date(b[1].updated_at) - new Date(a[1].updated_at)) : (a[1].priority < b[1].priority ? -1 : 1); });
      tq.created = items(r[5]).map(function (x) { return ["requirements", x]; });
      tq.involved = items(r[6]).map(function (x) { return ["requirements", x]; });
      var sum = function (o, ks) { return ks.reduce(function (a, k) { return a + ((o.by_status || {})[k] || 0); }, 0); };
      var cards = [
        ["我的待办", tq.todo.length, "#/requirements"],
        ["团队待验收", (rq.by_status || {}).accepting || 0, "#/requirements/accept"],
        ["团队未解决缺陷", sum(bg, ["todo", "doing", "reopened"]), "#/bugs"],
        ["测试通过率", ts.total ? Math.round(((ts.by_status || {}).passed || 0) * 100 / ts.total) + "%" : "—", "#/testing"]
      ];
      var iters = items(r[4]);
      el.querySelector("[data-body]").innerHTML = '<div class="xb-stats">' + cards.map(function (c) { return '<a class="xb-stat" href="' + c[2] + '" style="text-decoration:none;color:inherit"><span>' + c[0] + "</span><b>" + E(c[1]) + "</b><em>查看 →</em></a>"; }).join("") + "</div>" +
        '<div class="xd-wb"><div><section class="xb-main"><div class="x-tabs" role="tablist" style="margin-bottom:8px">' +
        [["todo", "待办 " + tq.todo.length], ["created", "我创建的"], ["involved", "我参与的"]].map(function (t) { return '<button type="button" data-wt="' + t[0] + '"' + (WB.tab === t[0] ? ' class="on"' : "") + ">" + E(t[1]) + "</button>"; }).join("") +
        '</div><div data-wlist></div></section></div><div><section class="xb-main"><h3>进行中的迭代<a class="xd-link" href="#/project/sprint">全部迭代</a></h3><div data-iters>' + (iters.length ? '<p class="x-frac">正在计算进度…</p>' : '<p class="xb-empty" style="padding:16px 0">没有进行中的迭代。</p>') + '</div></section>' +
        '<section class="xb-main"><h3>最近构建<a class="xd-link" href="#/cicd/pipeline">流水线</a></h3><div data-builds><p class="x-frac">正在加载…</p></div></section></div></div>';
      var drawList = function () {
        var list = tq[WB.tab] || [];
        var box = el.querySelector("[data-wlist]");
        box.innerHTML = list.length ? list.map(function (p) { var k = p[0], x = p[1]; return '<div class="xd-row" data-k="' + k + '" data-id="' + x.id + '"><span class="x-mono">' + ({ bugs: "缺陷", tests: "用例" }[k] || "") + "#" + x.id + "</span>" + stTag(k, x.status) + typeTag(x) + '<span class="xd-grow">' + E(x.title) + "</span>" + priTag(x.priority) + '<span class="x-frac">' + E(ago(x.updated_at)) + "</span></div>"; }).join("")
          : '<p class="xb-empty">' + { todo: "没有指派给你的未完成事项。", created: "你还没有创建需求。", involved: "还没有你参与（处理、评论或变更过）的需求。" }[WB.tab] + "</p>";
        [].forEach.call(box.querySelectorAll("[data-k]"), function (row) { row.onclick = function () { openDetail(row.getAttribute("data-k"), row.getAttribute("data-id"), reload); }; });
      };
      drawList();
      [].forEach.call(el.querySelectorAll("[data-wt]"), function (b) { b.onclick = function () { WB.tab = b.getAttribute("data-wt"); [].forEach.call(el.querySelectorAll("[data-wt]"), function (x) { x.classList.toggle("on", x === b); }); drawList(); }; });
      if (iters.length) Promise.all(iters.map(function (i) { return xs().get("/devcenter/requirements", { iteration_id: i.id, page_size: 500 }).then(items).catch(function () { return []; }); })).then(function (lists) {
        el.querySelector("[data-iters]").innerHTML = iters.map(function (i, n) {
          var rs = lists[n].filter(function (x) { return x.status !== "rejected"; }), done = rs.filter(function (x) { return x.status === "done"; }).length, pct = rs.length ? Math.round(done * 100 / rs.length) : 0, d = data(i);
          return '<div class="xd-row" data-it="' + i.id + '"><span class="xd-grow"><b>' + E(i.title) + '</b><br><span class="x-frac">' + E((d.start || "—") + " → " + (d.end || "—")) + '</span></span><span class="xd-prog" style="max-width:120px"><i style="width:' + pct + '%"></i></span><span class="x-frac">' + done + "/" + rs.length + "</span></div>";
        }).join("");
        [].forEach.call(el.querySelectorAll("[data-it]"), function (row) { row.onclick = function () { openDetail("iterations", row.getAttribute("data-it"), reload); }; });
      });
      drawBuilds(el.querySelector("[data-builds]"), tq.todo.concat(tq.involved));
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  // 最近构建：优先显示与我相关的（我触发的，或我负责 / 参与事项关联的代码仓库、流水线的构建），没有时显示团队最近构建。
  function drawBuilds(box, mine) {
    Promise.all([xs().get("/cicd/runs", { page_size: 50 }), xs().get("/devcenter/links").catch(function () { return { items: [] }; }), xs().get("/cicd/pipelines", { page_size: 200 }).then(items).catch(function () { return []; })]).then(function (r) {
      var runs = items(r[0]), keys = {}, apps = {}, pipes = {}, pname = {};
      mine.forEach(function (p) { keys[p[0] + "#" + p[1].id] = 1; });
      items(r[1]).forEach(function (l) { if (keys[l.kind + "#" + l.item_id]) { if (l.ref_type === "app") apps[l.ref_id] = 1; else pipes[l.ref_id] = 1; } });
      r[2].forEach(function (p) { pname[p.id] = p.name; });
      var related = runs.filter(function (x) { return x.triggered_by === me() || apps[x.app_id] || pipes[x.pipeline_id]; });
      var list = (related.length ? related : runs).slice(0, 6);
      box.innerHTML = (list.length ? (related.length ? "" : '<p class="x-frac" style="margin:0 0 4px">暂无与你相关的构建，以下为团队最近构建。</p>') + list.map(function (x) {
        return '<a class="xd-row" style="text-decoration:none;color:inherit" href="#/cicd/pipeline/' + encodeURIComponent(x.pipeline_id) + '">' + runTag(x.status) + '<span class="xd-grow">' + E(pname[x.pipeline_id] || "流水线 #" + x.pipeline_id) + ' <span class="x-frac">#' + E(x.id) + " · " + E(x.branch || "—") + "</span></span><span class=\"x-frac\">" + E(ago(x.created_at)) + "</span></a>";
      }).join("") : '<p class="xb-empty" style="padding:16px 0">还没有构建记录。</p>');
    }).catch(function (e) { box.innerHTML = '<p class="x-frac">' + (e && e.status === 403 ? "没有查看构建记录的权限。" : "读取构建记录失败：" + E(errText(e))) + "</p>"; });
  }

  /* ---------------------------------------------------------------- 迭代 & 看板 */
  function sprints(el) {
    el.innerHTML = head("迭代", "按迭代规划需求与缺陷，跟踪完成进度和燃尽情况。", '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 新建迭代</button>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { sprints(el); };
    el.querySelector("[data-new]").onclick = function () { itemForm("iterations", null, reload); };
    el.querySelector("[data-refresh]").onclick = reload;
    Promise.all([xs().get("/devcenter/iterations", { page_size: 200 }), xs().get("/devcenter/requirements", { page_size: 500 }), xs().get("/devcenter/bugs", { page_size: 500 })]).then(function (r) {
      var reqs = items(r[1]), bugs = items(r[2]), list = items(r[0]);
      var rows = list.map(function (i) {
        var rs = reqs.filter(function (x) { return x.iteration_id === i.id && x.status !== "rejected"; }), bs = bugs.filter(function (x) { return x.iteration_id === i.id; });
        var done = rs.filter(function (x) { return x.status === "done"; }).length, pct = rs.length ? Math.round(done * 100 / rs.length) : 0, d = data(i);
        var acts = (i.status === "todo" ? '<button class="x-act" data-st="' + i.id + '" data-to="doing">开始</button>' : i.status === "doing" ? '<button class="x-act" data-st="' + i.id + '" data-to="done">结束</button>' : "") +
          '<button class="x-act" data-open="' + i.id + '">燃尽图</button><button class="x-act" data-board="' + i.id + '">看板</button><button class="x-act" data-edit="' + i.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + i.id + '">删除</button>';
        return ['<span class="xd-name" data-open="' + i.id + '">' + E(i.title) + "</span>" + (d.goal ? '<div class="x-frac">' + E(d.goal) + "</div>" : ""), stTag("iterations", i.status), E((d.start || "—") + " → " + (d.end || "—")), E(i.assignee || "—"),
          '<div style="display:flex;align-items:center;gap:8px;min-width:160px"><span class="xd-prog"><i style="width:' + pct + '%"></i></span><span class="x-frac">' + done + "/" + rs.length + "</span></div>",
          bs.length ? bs.filter(function (b) { return DONE.bugs.indexOf(b.status) < 0; }).length + " / " + bs.length : "0", '<span class="xb-ops">' + acts + "</span>"];
      });
      el.querySelector("[data-body]").innerHTML = '<div class="xb-main">' + table(["迭代", "状态", "周期", "负责人", "需求完成", "未解决缺陷", "操作"], rows, "还没有迭代，点右上角新建。") + "</div>";
      var byId = function (id) { return list.filter(function (x) { return String(x.id) === String(id); })[0]; };
      on(el, "[data-edit]", function (b) { itemForm("iterations", byId(b.getAttribute("data-edit")), reload); });
      on(el, "[data-open]", function (b) { openDetail("iterations", b.getAttribute("data-open"), reload); });
      on(el, "[data-del]", function (b) { confirmDel("iterations", byId(b.getAttribute("data-del")), reload); });
      on(el, "[data-st]", function (b) { setStatus("iterations", byId(b.getAttribute("data-st")), b.getAttribute("data-to"), reload); });
      on(el, "[data-board]", function (b) { BOARD.iter = b.getAttribute("data-board"); location.hash = "#/project/board"; });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  var BOARD = { iter: "", mine: false, type: "" };
  function board(el) {
    el.innerHTML = head("看板", "拖动卡片到其它列即可改变状态，点击卡片查看详情。", '<a class="x-btn" href="#/requirements">列表视图</a><button class="x-btn x-btn--pri" data-new>+ 新建需求</button>') +
      '<div class="xd-filters"><select class="x-input" data-iter aria-label="迭代"><option value="">全部迭代</option></select><select class="x-input" data-type aria-label="类型"><option value="">需求和任务</option><option value="story"' + (BOARD.type === "story" ? " selected" : "") + '>只看需求</option><option value="task"' + (BOARD.type === "task" ? " selected" : "") + '>只看任务</option></select><label class="x-frac" style="display:flex;gap:6px;align-items:center"><input type="checkbox" data-mine style="width:auto"' + (BOARD.mine ? " checked" : "") + ">只看我负责的</label></div>" +
      '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { board(el); };
    el.querySelector("[data-new]").onclick = function () { itemForm("requirements", null, reload, BOARD.iter ? { values: { iteration_id: BOARD.iter } } : null); };
    el.querySelector("[data-type]").onchange = function (e) { BOARD.type = e.target.value; reload(); };
    el.querySelector("[data-mine]").onchange = function (e) { BOARD.mine = e.target.checked; reload(); };
    var q = { page_size: 500, iteration_id: BOARD.iter, type: BOARD.type, assignee: BOARD.mine ? "me" : "" };
    Promise.all([loadIters(), xs().get("/devcenter/requirements", q)]).then(function (r) {
      var sel = el.querySelector("[data-iter]");
      sel.innerHTML = '<option value="">全部迭代</option>' + r[0].map(function (i) { return '<option value="' + i.id + '"' + (String(i.id) === String(BOARD.iter) ? " selected" : "") + ">" + E(i.title) + "</option>"; }).join("");
      sel.onchange = function () { BOARD.iter = sel.value; reload(); };
      var reqs = items(r[1]), cols = KIND.requirements.st.filter(function (s) { return s[0] !== "rejected"; });
      el.querySelector("[data-body]").innerHTML = '<div class="xd-board">' + cols.map(function (c) {
        var cs = reqs.filter(function (x) { return x.status === c[0]; });
        return '<section class="xd-col" data-col="' + c[0] + '"><header><span class="x-tag x-tag--s ' + c[2] + '">' + c[1] + "</span><b>" + cs.length + "</b></header>" +
          cs.map(function (x) { return '<article class="xd-card" draggable="true" tabindex="0" data-card="' + x.id + '"><div class="xd-card-t">' + typeTag(x) + E(x.title) + '</div><div class="xd-card-m"><span class="x-mono">#' + x.id + "</span>" + priTag(x.priority) + '<span class="x-frac">' + E(x.assignee || "未指派") + "</span></div></article>"; }).join("") + "</section>";
      }).join("") + "</div>" + (reqs.length ? "" : '<p class="xb-empty">没有符合条件的需求。</p>');
      var byId = function (id) { return reqs.filter(function (x) { return String(x.id) === String(id); })[0]; };
      [].forEach.call(el.querySelectorAll("[data-card]"), function (c) {
        c.ondragstart = function (e) { e.dataTransfer.setData("text/plain", c.getAttribute("data-card")); c.classList.add("drag"); };
        c.ondragend = function () { c.classList.remove("drag"); };
        c.onclick = function () { openDetail("requirements", c.getAttribute("data-card"), reload); };
        c.onkeydown = function (e) { if (e.key === "Enter") openDetail("requirements", c.getAttribute("data-card"), reload); };
      });
      [].forEach.call(el.querySelectorAll("[data-col]"), function (col) {
        col.ondragover = function (e) { e.preventDefault(); col.classList.add("over"); };
        col.ondragleave = function () { col.classList.remove("over"); };
        col.ondrop = function (e) {
          e.preventDefault(); col.classList.remove("over");
          var x = byId(e.dataTransfer.getData("text/plain")), to = col.getAttribute("data-col");
          if (x && x.status !== to) { if (to === "done" && x.status === "accepting") acceptDialog(x, reload); else setStatus("requirements", x, to, reload); }
        };
      });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  /* ---------------------------------------------------------------- 统计报表 */
  var STATS = { days: 30, iter: "" };
  function kpi(label, val, sub) { return '<div class="xb-stat"><span>' + E(label) + "</span><b>" + E(val) + "</b><em>" + E(sub || "") + "</em></div>"; }
  function stats(el) {
    el.innerHTML = head("统计报表", "基于需求、缺陷和测试用例的真实数据与状态变更记录计算。", '<select class="x-input" data-days style="width:auto">' + [[7, "最近 7 天"], [30, "最近 30 天"], [90, "最近 90 天"], [180, "最近 180 天"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === STATS.days ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") +
      '</select><select class="x-input" data-iter style="width:auto"><option value="">全部迭代</option></select>') + '<div data-body><p class="x-frac">正在计算…</p></div>';
    el.querySelector("[data-days]").onchange = function (e) { STATS.days = Number(e.target.value); stats(el); };
    Promise.all([xs().get("/devcenter/stats", tzq({ days: STATS.days, iteration_id: STATS.iter })), loadIters()]).then(function (res) {
      var isel = el.querySelector("[data-iter]");
      isel.innerHTML = '<option value="">全部迭代</option>' + res[1].map(function (i) { return '<option value="' + i.id + '"' + (String(i.id) === String(STATS.iter) ? " selected" : "") + ">" + E(i.title) + "</option>"; }).join("");
      isel.onchange = function () { STATS.iter = isel.value; stats(el); };
      var r = res[0].report || {}, tests = res[0].tests_by_status || {}, ct = r.cycle_time || {}, lt = r.lead_time || {}, th = r.throughput || {}, cr = r.created || {};
      var ttotal = Object.keys(tests).reduce(function (a, k) { return a + tests[k]; }, 0);
      var kp = '<div class="xd-kpis">' +
        kpi("需求吞吐", (th.requirements || 0) + " 个", "区间内完成，新增 " + (cr.requirements || 0) + " 个") +
        kpi("缺陷关闭", (th.bugs || 0) + " 个", "区间内验证或关闭，新增 " + (cr.bugs || 0) + " 个") +
        kpi("在手事项", (r.open_total || 0) + " 个", "未完成的需求与缺陷") +
        kpi("需求周期时长（中位数）", hrs((ct.requirements || {}).p50_hours), (ct.requirements || {}).count ? "开始开发到完成，平均 " + hrs(ct.requirements.avg_hours) + "，85% 在 " + hrs(ct.requirements.p85_hours) + " 内" : "区间内没有可计算的需求") +
        kpi("需求交付时长（中位数）", hrs((lt.requirements || {}).p50_hours), (lt.requirements || {}).count ? "创建到完成，平均 " + hrs(lt.requirements.avg_hours) : "区间内没有完成的需求") +
        kpi("缺陷修复时长（中位数）", hrs((ct.bugs || {}).p50_hours), (ct.bugs || {}).count ? "开始处理到关闭，平均 " + hrs(ct.bugs.avg_hours) : "区间内没有可计算的缺陷") + "</div>";
      var weeks = r.weeks || [], max = 1;
      weeks.forEach(function (w) { max = Math.max(max, w.requirements_done, w.bugs_closed, w.requirements_created); });
      var hgt = function (v) { return Math.round(v * 150 / max); };
      var chart = '<div class="xb-main" style="margin-bottom:16px"><h3 style="margin:0 0 10px;font-size:15px">每周吞吐</h3>' + (weeks.length ? '<div class="xd-bars">' + weeks.map(function (w) {
        return '<div class="xd-bar" title="' + E(w.week) + " 起：完成需求 " + w.requirements_done + "，关闭缺陷 " + w.bugs_closed + "，新增需求 " + w.requirements_created + '"><div class="xd-bar-g"><i class="xd-c1" style="height:' + hgt(w.requirements_done) + 'px"></i><i class="xd-c2" style="height:' + hgt(w.bugs_closed) + 'px"></i><i class="xd-c3" style="height:' + hgt(w.requirements_created) + 'px"></i></div><span>' + E(w.week.slice(5)) + "</span></div>";
      }).join("") + '</div><div class="xd-legend"><span><i class="xd-c1"></i>完成需求</span><span><i class="xd-c2"></i>关闭缺陷</span><span><i class="xd-c3"></i>新增需求</span><span class="x-frac">按周一起算</span></div>' : '<p class="x-frac">没有数据。</p>') + "</div>";
      var hbars = function (obj, k, labels) {
        var keys = labels ? labels.map(function (l) { return l[0]; }).filter(function (s) { return obj[s]; }) : Object.keys(obj), mx = 1;
        keys.forEach(function (s) { mx = Math.max(mx, obj[s]); });
        if (!keys.length) return '<p class="x-frac">没有数据。</p>';
        return keys.map(function (s) { var name = k ? stOf(k, s)[1] : (SEV[s] || s); return '<div class="xd-hbar"><span>' + E(name) + '</span><div><i style="width:' + Math.round(obj[s] * 100 / mx) + '%"></i></div><b>' + obj[s] + "</b></div>"; }).join("");
      };
      var dist = '<div class="xd-grid2"><div class="xb-main"><h3>需求状态分布</h3>' + hbars(r.requirements_by_status || {}, "requirements", KIND.requirements.st) + '</div><div class="xb-main"><h3>缺陷状态分布</h3>' + hbars(r.bugs_by_status || {}, "bugs", KIND.bugs.st) +
        '<h3 style="margin-top:16px">未解决缺陷严重程度</h3>' + hbars(r.open_bugs_by_severity || {}, "", null) + "</div></div>";
      var who = '<div class="xd-grid2"><div class="xb-main"><h3>成员负载</h3>' + table(["处理人", "在手需求", "在手缺陷", "区间内完成"], (r.assignees || []).map(function (a) { return [E(a.assignee), a.open_requirements, a.open_bugs, a.done_in_range]; }), "没有数据") + "</div>" +
        '<div class="xb-main"><h3>测试执行</h3>' + (ttotal ? '<p style="margin:0 0 8px;font-size:13px">通过率 <b style="font-size:20px">' + Math.round((tests.passed || 0) * 100 / ttotal) + "%</b>（" + (tests.passed || 0) + "/" + ttotal + "）</p>" + hbars(tests, "tests", KIND.tests.st) : '<p class="x-frac">还没有测试用例。</p>') + "</div></div>";
      el.querySelector("[data-body]").innerHTML = kp + chart + dist + who +
        '<p class="x-frac xb-note">口径说明：吞吐为区间内进入「已完成」（需求）或「已验证 / 已关闭」（缺陷）的数量，已关闭的需求不计入；周期时长从首次进入「开发中 / 处理中」算起；交付时长从创建算起；被重新打开的事项不计为完成。启用变更记录之前的历史数据以最后更新时间近似。</p>';
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  /* ---------------------------------------------------------------- 需求 → AI 开发 */
  var AI_TIMER = 0;
  function stopAITimer() { if (AI_TIMER) { clearTimeout(AI_TIMER); AI_TIMER = 0; } }
  function aiStatus() { return xs().get("/devcenter/ai/status").catch(function (e) { return { available: false, reason: errText(e), models: [] }; }); }
  function aiNotice(s) {
    if (s.available) return "";
    var noModel = !(s.models || []).length;
    return '<div class="xd-hint xd-hint--warn"><b>AI 开发暂不可用：</b>' + E(s.reason || "未知原因") + (noModel ? '。配置完成后回到这里即可使用。 <a class="xd-link" href="#/ai/config">前往 AI 设置</a>' : "") + "</div>";
  }
  var AI_ST = { running: ["生成中", "x-blue"], succeeded: ["已生成", "x-green"], failed: ["失败", "x-red"] };
  function aiTag(s) { var x = AI_ST[s] || [s, "x-grey"]; return '<span class="x-tag x-tag--s ' + x[1] + '">' + E(x[0]) + "</span>"; }

  // aiLaunch：选择需求（可预选）、模型和补充说明，发起生成。
  function aiLaunch(req) {
    aiStatus().then(function (s) {
      if (!s.available) {
        var m0 = modal("AI 开发", aiNotice(s), '<button class="x-btn" data-no>知道了</button>', 480);
        return m0;
      }
      var models = s.models || [], def = models.filter(function (x) { return x.is_default; })[0] || models[0];
      var m = modal("AI 开发" + (req ? " · #" + req.id + " " + req.title : ""), '<div class="xd-form">' +
        (req ? "" : '<label>需求<select class="x-input" data-req><option value="">正在加载…</option></select></label>') +
        '<label>模型<select class="x-input" data-model>' + models.map(function (x) { return '<option value="' + x.id + '"' + (x === def ? " selected" : "") + ">" + E(x.name + (x.model && x.model !== x.name ? " · " + x.model : "")) + "</option>"; }).join("") + "</select></label>" +
        '<label>补充说明（可选）<textarea class="x-input" data-note rows="3" maxlength="2000" placeholder="技术约束、希望采用的方案、不要改动的模块等"></textarea></label>' +
        '<p class="x-frac" style="margin:0">AI 会根据需求描述、验收标准和关联的代码仓库生成实施方案、任务拆解、风险与测试要点。平台不会自动修改代码或提交合并请求；拆解出的任务需你确认后才会创建。</p></div>',
        '<button class="x-btn" data-no>取消</button><button class="x-btn x-btn--pri" data-ok>开始生成</button>', 560);
      var rs = m.el.querySelector("[data-req]");
      if (rs) xs().get("/devcenter/requirements", { type: "story", status: openStatuses("requirements").join(","), page_size: 200 }).then(function (r) {
        var l = items(r);
        rs.innerHTML = l.length ? '<option value="">请选择需求</option>' + l.map(function (x) { return '<option value="' + x.id + '">#' + x.id + " " + E(x.title) + "</option>"; }).join("") : '<option value="">没有未完成的需求</option>';
      }).catch(function (e) { m.err(errText(e)); });
      var ok = m.el.querySelector("[data-ok]");
      ok.onclick = function () {
        var rid = req ? req.id : (rs && rs.value);
        if (!rid) { m.err("请选择需求"); return; }
        ok.disabled = true;
        xs().post("/devcenter/requirements/" + rid + "/ai-tasks", { model_id: Number(m.el.querySelector("[data-model]").value) || 0, note: m.el.querySelector("[data-note]").value.trim() })
          .then(function (t) { m.close(); closeDrawer(); toast("已开始生成，通常需要几十秒"); location.hash = "#/coding/ai/" + t.id; })
          .catch(function (e) { ok.disabled = false; m.err(errText(e)); });
      };
    });
  }

  function aiPage(el, base) {
    stopAITimer();
    el.innerHTML = head("AI 开发", "选择一条需求，由已配置的大模型生成实施方案和任务拆解，确认后一键创建为任务。", '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 发起 AI 开发</button>') + '<div data-note></div><div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { aiPage(el, base); };
    el.querySelector("[data-refresh]").onclick = reload;
    el.querySelector("[data-new]").onclick = function () { aiLaunch(null); };
    Promise.all([aiStatus(), xs().get("/devcenter/ai-tasks", { page_size: 100 })]).then(function (r) {
      el.querySelector("[data-note]").innerHTML = aiNotice(r[0]);
      var list = items(r[1]);
      el.querySelector("[data-body]").innerHTML = '<div class="xb-main">' + table(["编号", "需求", "状态", "模型", "发起人", "发起时间", "操作"], list.map(function (t) {
        return ['<span class="x-mono">#' + t.id + "</span>", t.requirement_title ? '<a class="xd-link" data-req="' + t.requirement_id + '">#' + t.requirement_id + " " + E(t.requirement_title) + "</a>" : '<span class="x-frac">需求已删除</span>', aiTag(t.status), E(t.model_name || "—"), E(t.created_by || "—"), fmt(t.created_at), '<a class="x-act" href="' + base + "/" + t.id + '">查看</a>'];
      }), "还没有 AI 开发记录。点右上角「发起 AI 开发」，或在需求详情中点「AI 开发」。") + "</div>";
      on(el, "[data-req]", function (b) { openDetail("requirements", b.getAttribute("data-req"), reload); });
      if (list.some(function (t) { return t.status === "running"; })) { var h = (location.hash || "").split("?")[0]; AI_TIMER = setTimeout(function () { if (stillOn(h)) reload(); }, 4000); }
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = failBox(e); });
  }

  function aiTaskView(el, id, base) {
    stopAITimer();
    var h = (location.hash || "").split("?")[0];
    xs().get("/devcenter/ai-tasks/" + id).then(function (t) {
      if (!stillOn(h)) return;
      var res = t.result || {}, applied = t.applied || [];
      var crumb = '<p class="x-frac" style="margin:0 0 8px"><a class="xd-link" href="' + base + '">AI 开发</a> / #' + t.id + "</p>";
      var acts = '<a class="x-btn" href="' + base + '">返回列表</a>' + (t.status !== "running" && t.requirement_title ? '<button class="x-btn" data-again>重新生成</button>' : "");
      var body = '<div class="xb-main xd-ai"><p style="margin:0 0 12px;font-size:13px">' + aiTag(t.status) + " 模型 <b>" + E(t.model_name || "—") + "</b> · 发起人 " + E(t.created_by || "—") + " · " + fmt(t.created_at) + (t.finished_at ? " · 完成于 " + fmt(t.finished_at) : "") + "</p>" +
        (t.note ? '<p class="x-frac">补充说明：' + E(t.note) + "</p>" : "");
      if (t.status === "running") body += '<p><span class="xd-spin"></span>正在生成实施方案，通常需要几十秒，页面会自动刷新…</p>';
      else if (t.status === "failed") body += '<div class="xd-hint xd-hint--warn">' + E(t.error || "生成失败") + "</div>";
      else {
        body += (res.summary ? '<div class="xd-sec"><h4>概述</h4><p class="xd-text">' + E(res.summary) + "</p></div>" : "") +
          ((res.steps || []).length ? '<div class="xd-sec"><h4>实施步骤</h4><ol>' + res.steps.map(function (s) { return "<li>" + E(s) + "</li>"; }).join("") + "</ol></div>" : "") +
          '<div class="xd-sec"><h4>任务拆解<span class="x-frac">勾选后创建为挂在需求下的任务</span></h4>' + ((res.subtasks || []).length ? res.subtasks.map(function (s, i) {
            var did = applied.indexOf(i) >= 0;
            return '<div class="xd-sub"><input type="checkbox" data-si="' + i + '"' + (did ? " checked disabled" : " checked") + ' aria-label="选择任务 ' + (i + 1) + '"><div class="xd-grow"><b>' + E(s.title) + "</b> " + priTag(s.priority) + (s.estimate ? ' <span class="x-frac">' + E(s.estimate) + "</span>" : "") + (did ? ' <span class="x-tag x-tag--s x-green">已创建</span>' : "") + (s.description ? "<p>" + E(s.description) + "</p>" : "") + "</div></div>";
          }).join("") + '<div class="xd-filters" style="margin-top:12px"><select class="x-input" data-assignee aria-label="处理人"><option value="">暂不指派</option></select><button class="x-btn x-btn--pri" data-apply>创建选中的任务</button><span class="xd-err" data-aerr hidden></span></div>' : '<p class="x-frac">模型没有给出任务拆解。</p>') + "</div>" +
          ((res.risks || []).length ? '<div class="xd-sec"><h4>风险与依赖</h4><ul>' + res.risks.map(function (s) { return "<li>" + E(s) + "</li>"; }).join("") + "</ul></div>" : "") +
          ((res.test_points || []).length ? '<div class="xd-sec"><h4>测试要点</h4><ul>' + res.test_points.map(function (s) { return "<li>" + E(s) + "</li>"; }).join("") + "</ul></div>" : "") +
          '<p class="x-frac">方案由 AI 生成，仅供参考，请结合实际代码评审后执行。</p>';
      }
      body += "</div>";
      el.innerHTML = crumb + head("AI 开发 · " + (t.requirement_title ? "#" + t.requirement_id + " " + t.requirement_title : "需求已删除"), "", acts) + body;
      var again = el.querySelector("[data-again]");
      if (again) again.onclick = function () { xs().get("/devcenter/requirements/" + t.requirement_id).then(aiLaunch).catch(function (e) { toast(errText(e)); }); };
      var asel = el.querySelector("[data-assignee]");
      if (asel) loadUsers().then(function (u) { asel.innerHTML = userOptions(u, "", "暂不指派"); });
      var ap = el.querySelector("[data-apply]");
      if (ap) ap.onclick = function () {
        var idx = [].filter.call(el.querySelectorAll("[data-si]"), function (c) { return c.checked && !c.disabled; }).map(function (c) { return Number(c.getAttribute("data-si")); });
        var er = el.querySelector("[data-aerr]");
        if (!idx.length) { er.textContent = "请至少勾选一个尚未创建的任务"; er.hidden = false; return; }
        ap.disabled = true;
        xs().post("/devcenter/ai-tasks/" + t.id + "/apply", { indexes: idx, assignee: asel ? asel.value : "" }).then(function (r) { toast("已创建 " + items(r).length + " 个任务"); aiTaskView(el, id, base); })
          .catch(function (e) { ap.disabled = false; er.textContent = errText(e); er.hidden = false; });
      };
      if (t.status === "running") AI_TIMER = setTimeout(function () { if (stillOn(h)) aiTaskView(el, id, base); }, 3000);
    }).catch(function (e) { el.innerHTML = head("AI 开发", "", '<a class="x-btn" href="' + base + '">返回列表</a>') + failBox(e); });
  }

  /* ---------------------------------------------------------------- 代码仓库 / 合并请求 */
  function repos(el, sub) {
    if (sub === "mr") {
      el.innerHTML = head("合并请求", "") + '<div class="xb-guide xb-main"><h3>合并请求在代码托管平台完成</h3><p class="x-frac" style="line-height:1.8">平台不托管 Git 代码。合并请求与代码评审在 GitLab / GitHub / Gitea 中进行；把仓库的推送或合并事件指向流水线后，合并即可自动触发构建与部署。需求与代码的对应关系可在需求详情中「关联代码仓库或流水线」。</p><a class="x-btn x-btn--pri" href="#/cicd/pipeline">前往流水线</a></div>';
      return;
    }
    el.innerHTML = head("代码仓库", "已接入平台的代码仓库（来自 CI/CD 应用），以及关联到这些仓库的需求、缺陷和最近一次构建。", '<button class="x-btn" data-refresh>刷新</button><a class="x-btn x-btn--pri" href="#/delivery/apps">+ 接入代码仓库</a>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { repos(el, sub); };
    el.querySelector("[data-refresh]").onclick = reload;
    Promise.all([xs().get("/cicd/applications", { page_size: 200 }), loadPipelines(), xs().get("/devcenter/links").catch(function () { return { items: [] }; })]).then(function (r) {
      var apps = items(r[0]), pipes = r[1] || [], links = items(r[2]);
      var rows = apps.map(function (a) {
        var ps = pipes.filter(function (p) { return p.app_id === a.id; }), pids = {};
        ps.forEach(function (p) { pids[p.id] = 1; });
        var lr = lastRunOf({ ref_type: "app", ref_id: a.id }, pipes);
        var ls = links.filter(function (l) { return (l.ref_type === "app" && l.ref_id === a.id) || (l.ref_type === "pipeline" && pids[l.ref_id]); });
        var open = ls.filter(function (l) { return l.kind === "requirements" ? DONE.requirements.indexOf(l.item_status) < 0 : DONE.bugs.indexOf(l.item_status) < 0; }).length;
        return ['<b>' + E(a.name) + "</b>", '<span class="x-mono">' + E(a.repo_url || "—") + "</span>", E(a.branch || "—"), ps.length ? ps.length + " 条" : "—",
          lr ? '<a class="xd-link" href="#/cicd/pipeline/' + encodeURIComponent(lr.pipeline_id) + '">' + runTag(lr.status) + "</a> <span class=\"x-frac\">" + E(ago(lr.created_at)) + "</span>" : '<span class="x-frac">暂无</span>',
          ls.length ? '<a class="xd-link" data-app="' + a.id + '">' + ls.length + " 项（未完成 " + open + "）</a>" : '<span class="x-frac">未关联</span>'];
      });
      el.querySelector("[data-body]").innerHTML = '<div class="xb-main">' + table(["应用", "仓库地址", "默认分支", "流水线", "最近构建", "关联需求 / 缺陷"], rows, "还没有接入代码仓库。到 CI/CD 应用中接入后，这里会显示仓库与需求的关联。") + '</div><div data-rel style="margin-top:16px"></div>';
      on(el, "[data-app]", function (b) {
        var id = Number(b.getAttribute("data-app")), ps = {};
        pipes.forEach(function (p) { if (p.app_id === id) ps[p.id] = 1; });
        var ls = links.filter(function (l) { return (l.ref_type === "app" && l.ref_id === id) || (l.ref_type === "pipeline" && ps[l.ref_id]); });
        var app = apps.filter(function (a) { return a.id === id; })[0] || {};
        var box = el.querySelector("[data-rel]");
        box.innerHTML = '<div class="xb-main"><h3 style="margin:0 0 8px;font-size:15px">' + E(app.name) + ' 关联的需求与缺陷</h3><ul class="xd-list">' + ls.map(function (l) {
          return '<li><span class="x-mono">' + (l.kind === "bugs" ? "缺陷" : "") + "#" + l.item_id + "</span>" + stTag(l.kind, l.item_status) + '<a class="xd-link xd-grow" data-k="' + l.kind + '" data-id="' + l.item_id + '">' + E(l.item_title || "（已删除）") + '</a><span class="x-frac">' + (l.ref_type === "pipeline" ? "经流水线 " : "") + E(l.ref_name) + "</span></li>";
        }).join("") + "</ul></div>";
        on(box, "[data-k]", function (x) { openDetail(x.getAttribute("data-k"), x.getAttribute("data-id"), reload); });
      });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + (e && e.status === 403 ? "" : "（代码仓库数据来自 CI/CD）") + "</p>"; });
  }

  function render(el, key) {
    stopAITimer();
    closeDrawer();
    var parts = (location.hash || "").split("?")[0].split("/"), sub = parts[2] || "", third = parts[3] || "";
    if (key === "workbench") return workbench(el);
    if (key === "requirements") {
      if (sub === "ai") return third ? aiTaskView(el, third, "#/requirements/ai") : aiPage(el, "#/requirements/ai");
      var rs = LS.requirements;
      rs._forced = false;
      if (sub === "accept") { rs._acc = true; return listPage(el, "requirements", { tab: "accepting", title: "需求验收" }); }
      if (rs._acc) { rs._acc = false; rs.tab = "open"; }
      return listPage(el, "requirements");
    }
    if (key === "bugs") return listPage(el, "bugs");
    if (key === "testing") return listPage(el, "tests");
    if (key === "project") return sub === "board" ? board(el) : sub === "stats" ? stats(el) : sprints(el);
    if (key === "coding") {
      if (sub === "ai") return third ? aiTaskView(el, third, "#/coding/ai") : aiPage(el, "#/coding/ai");
      return repos(el, sub);
    }
  }
  root.XSDev = { render: render, openDetail: openDetail, _greeting: greeting };
})(window);
