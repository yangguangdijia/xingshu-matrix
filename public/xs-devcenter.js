/*
 * 星枢 Matrix 研发中心（实时模式）：工作台、需求管理与验收、迭代、看板、缺陷、测试用例、代码仓库。
 * 数据来自 /devcenter/*（需求 / 迭代 / 缺陷 / 测试四类对象，固定状态流转），代码仓库来自 /cicd/applications。
 * 新建与编辑统一使用 XSFormKit（可视化表单 + AI 填写 + YAML）。
 */
(function (root) {
  "use strict";
  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function errText(e) { return (e && (e.message || e.error)) || String(e || "未知错误"); }
  function items(r) { return Array.isArray(r) ? r : (r && r.items) || []; }
  function fmt(t) { if (!t) return "—"; var d = new Date(t); return isNaN(d) ? String(t).slice(0, 16) : (d.getMonth() + 1) + "-" + String(d.getDate()).padStart(2, "0") + " " + String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0"); }
  function me() { var u = xs().user || {}; return u.username || ""; }
  function data(x) { return (x && x.data && typeof x.data === "object") ? x.data : {}; }

  var KIND = {
    requirements: { name: "需求", st: [["todo", "待评审", "x-grey"], ["ready", "待开发", "x-blue"], ["doing", "开发中", "x-blue"], ["testing", "测试中", "x-orange"], ["accepting", "待验收", "x-orange"], ["done", "已完成", "x-green"], ["rejected", "已关闭", "x-grey"]] },
    iterations: { name: "迭代", st: [["todo", "规划中", "x-grey"], ["doing", "进行中", "x-blue"], ["done", "已结束", "x-green"]] },
    bugs: { name: "缺陷", st: [["todo", "新建", "x-red"], ["doing", "处理中", "x-blue"], ["fixed", "已修复", "x-orange"], ["verified", "已验证", "x-green"], ["closed", "已关闭", "x-grey"], ["reopened", "重新打开", "x-red"]] },
    tests: { name: "测试用例", st: [["todo", "未执行", "x-grey"], ["passed", "通过", "x-green"], ["failed", "失败", "x-red"], ["blocked", "阻塞", "x-orange"]] }
  };
  var PRI = [["P0", "紧急", "x-red"], ["P1", "高", "x-orange"], ["P2", "中", "x-blue"], ["P3", "低", "x-grey"]];
  function stOf(k, s) { return KIND[k].st.filter(function (x) { return x[0] === s; })[0] || [s, s, "x-grey"]; }
  function stTag(k, s) { var x = stOf(k, s); return '<span class="x-tag x-tag--s ' + x[2] + '">' + E(x[1]) + "</span>"; }
  function priTag(p) { var x = PRI.filter(function (y) { return y[0] === p; })[0] || [p, p, "x-grey"]; return '<span class="x-tag x-tag--s ' + x[2] + '">' + E(x[0]) + "</span>"; }
  var DONE = { requirements: ["done", "rejected"], iterations: ["done"], bugs: ["verified", "closed"], tests: ["passed"] };

  function head(t, sub, acts) {
    return '<div class="xb-head"><div><h2>' + E(t) + "</h2>" + (sub ? "<p>" + E(sub) + "</p>" : "") + '</div><div class="xb-head-act">' + (acts || "") + "</div></div>";
  }
  function table(cols, rows, empty) {
    if (!rows.length) return '<p class="xb-empty">' + E(empty || "暂无数据") + "</p>";
    return '<div class="xb-table"><table><thead><tr>' + cols.map(function (c) { return "<th>" + c + "</th>"; }).join("") + "</tr></thead><tbody>" + rows.map(function (r) { return "<tr>" + r.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>"; }).join("") + "</tbody></table></div>";
  }
  function confirmDel(k, x, done) {
    if (!root.XSBastion) { if (!confirm("确定删除？")) return; }
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true"><h3>删除' + KIND[k].name + '</h3><div class="xb-confirm-body">确定删除 <b>' + E(x.title) + "</b>？删除后不可恢复。</div><p class=\"fk-err\" hidden></p><div class=\"xb-confirm-foot\"><button class=\"x-btn\" data-no>取消</button><button class=\"x-btn x-btn--danger\" data-yes>删除</button></div></div>";
    document.body.appendChild(m);
    m.querySelector("[data-no]").onclick = function () { m.remove(); };
    m.querySelector("[data-yes]").onclick = function () {
      xs().del("/devcenter/" + k + "/" + x.id).then(function () { m.remove(); toast("已删除"); done(); })
        .catch(function (e) { var p = m.querySelector(".fk-err"); p.textContent = errText(e); p.hidden = false; });
    };
  }

  /* ---------------------------------------------------------------- 表单 */
  function itemForm(k, x, done, preset) {
    var editing = !!x;
    var iterLoad = function () { return xs().get("/devcenter/iterations", { page_size: 200 }).then(function (r) { return [{ v: "", t: "不关联迭代" }].concat(items(r).filter(function (i) { return i.status !== "done" || (x && x.iteration_id === i.id); }).map(function (i) { return { v: String(i.id), t: i.title }; })); }); };
    var userLoad = function () { return xs().get("/users", { page_size: 200 }).then(function (r) { return [{ v: "", t: "暂不指派" }].concat(items(r).map(function (u) { return { v: u.username, t: (u.display_name || u.username) + "（" + u.username + "）" }; })); }).catch(function () { return [{ v: "", t: "暂不指派" }, { v: me(), t: me() }]; }); };
    var f = [
      { k: "title", label: KIND[k].name + "标题", required: true, placeholder: { requirements: "登录页支持企业 SSO", iterations: "2026-11 第 1 迭代", bugs: "工作负载列表分页错乱", tests: "SSO 登录后跳回原页面" }[k] },
      { k: "status", label: "状态", type: "select", options: KIND[k].st.map(function (s) { return { v: s[0], t: s[1] }; }), def: KIND[k].st[0][0] }
    ];
    if (k !== "iterations") f.push({ k: "priority", label: "优先级", type: "radio", options: PRI.map(function (p) { return { v: p[0], t: p[0] + " " + p[1] }; }), def: "P2" });
    if (k !== "iterations") f.push({ k: "iteration_id", label: "所属迭代", type: "select", load: iterLoad, options: [{ v: "", t: "不关联迭代" }] });
    f.push({ k: "assignee", label: k === "iterations" ? "负责人" : "处理人", type: "select", load: userLoad, options: [{ v: "", t: "暂不指派" }] });
    if (k === "iterations") {
      f.push({ k: "start", label: "开始日期", type: "text", placeholder: "2026-11-04" }, { k: "end", label: "结束日期", type: "text", placeholder: "2026-11-18" }, { k: "goal", label: "迭代目标", type: "textarea" });
    } else if (k === "requirements") {
      f.push({ k: "description", label: "需求描述", type: "textarea", placeholder: "背景、用户故事、范围" }, { k: "acceptance", label: "验收标准", type: "textarea", placeholder: "每行一条，验收时逐条核对" });
    } else if (k === "bugs") {
      f.push({ k: "severity", label: "严重程度", type: "radio", options: [{ v: "critical", t: "致命" }, { v: "major", t: "严重" }, { v: "minor", t: "一般" }, { v: "trivial", t: "轻微" }], def: "major" },
        { k: "env", label: "发现环境", type: "select", options: [{ v: "", t: "—" }, { v: "prod", t: "生产" }, { v: "pre", t: "预发" }, { v: "test", t: "测试" }, { v: "dev", t: "开发" }] },
        { k: "description", label: "复现步骤与现象", type: "textarea", placeholder: "1. …\n2. …\n期望：…\n实际：…" });
    } else {
      f.push({ k: "requirement", label: "关联需求编号", type: "text", placeholder: "例如 12" }, { k: "steps", label: "测试步骤", type: "textarea" }, { k: "expected", label: "预期结果", type: "textarea" });
    }
    var d = x ? data(x) : {};
    var vals = x ? { title: x.title, status: x.status, priority: x.priority, iteration_id: x.iteration_id ? String(x.iteration_id) : "", assignee: x.assignee || "", description: x.description || "",
      start: d.start || "", end: d.end || "", goal: d.goal || "", acceptance: d.acceptance || "", severity: d.severity || "major", env: d.env || "", requirement: d.requirement || "", steps: d.steps || "", expected: d.expected || "" } : Object.assign({ assignee: k === "iterations" ? me() : "" }, preset || {});
    var toObj = function (v) {
      var o = { title: v.title, status: v.status, assignee: v.assignee || "" };
      if (k !== "iterations") { o.priority = v.priority || "P2"; o.iteration_id = v.iteration_id ? Number(v.iteration_id) : null; }
      var dd = Object.assign({}, d);
      if (k === "iterations") { dd.start = v.start || ""; dd.end = v.end || ""; dd.goal = v.goal || ""; }
      else if (k === "requirements") { o.description = v.description || ""; dd.acceptance = v.acceptance || ""; }
      else if (k === "bugs") { o.description = v.description || ""; dd.severity = v.severity || "major"; dd.env = v.env || ""; }
      else { dd.requirement = v.requirement || ""; dd.steps = v.steps || ""; dd.expected = v.expected || ""; }
      o.data = dd;
      return o;
    };
    root.XSFormKit.open({
      title: (editing ? "编辑" : "新建") + KIND[k].name + (editing ? " · #" + x.id : ""), okText: editing ? "保存" : "创建", scene: "devcenter." + k, width: "lg",
      aiExample: { requirements: "P1 需求：登录页支持企业 SSO，验收标准包括自动建号和失败提示", iterations: "11 月第一个迭代，11-04 到 11-18，目标上线企业 SSO", bugs: "生产环境工作负载列表翻页后数据重复，严重", tests: "验证 SSO 登录后回到原来的页面" }[k],
      sections: [{ fields: f }], values: vals, toObject: toObj,
      fromObject: function (o, v) { var dd = o.data || {}; return Object.assign({}, v, { title: o.title, status: o.status, priority: o.priority, assignee: o.assignee, description: o.description, iteration_id: o.iteration_id ? String(o.iteration_id) : "" }, dd); },
      validate: function (v) {
        if (!v.title) return "请填写标题";
        if (k === "iterations" && v.start && v.end && v.end < v.start) return "结束日期不能早于开始日期";
        return "";
      },
      submit: function (v, obj, ctx) {
        var p = editing ? xs().put("/devcenter/" + k + "/" + x.id, obj) : xs().post("/devcenter/" + k, obj);
        return p.then(function () { ctx.close(); toast(editing ? "已保存" : KIND[k].name + "已创建"); done(); });
      }
    });
  }

  function setStatus(k, x, s, done, extra) {
    var body = { status: s };
    if (extra) body.data = Object.assign({}, data(x), extra);
    return xs().patch("/devcenter/" + k + "/" + x.id, body).then(function () { toast(KIND[k].name + "已更新为「" + stOf(k, s)[1] + "」"); done(); }).catch(function (e) { toast("更新失败：" + errText(e)); });
  }

  /* ---------------------------------------------------------------- 列表页（需求 / 缺陷 / 测试） */
  var LS = { requirements: { tab: "open", q: "" }, bugs: { tab: "open", q: "" }, tests: { tab: "all", q: "" } };

  function listPage(el, k, opts) {
    opts = opts || {};
    var st = LS[k];
    if (opts.tab) st.tab = opts.tab;
    var tabs = k === "requirements" ? [["open", "进行中"], ["accepting", "待验收"], ["mine", "我负责"], ["done", "已完成"], ["all", "全部"]]
      : k === "bugs" ? [["open", "未解决"], ["mine", "我负责"], ["fixed", "待验证"], ["all", "全部"]]
        : [["all", "全部"], ["todo", "未执行"], ["failed", "失败"], ["mine", "我负责"]];
    var title = opts.title || { requirements: "需求管理", bugs: "缺陷总览", tests: "测试管理" }[k];
    var sub = { requirements: "需求从评审、开发、测试到验收的全过程；验收标准随需求一起保存。", bugs: "集中跟踪缺陷，修复后由提出人验证关闭。", tests: "维护测试用例并记录每次执行结果。" }[k];
    el.innerHTML = head(title, sub, '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 新建' + KIND[k].name + "</button>") +
      '<div class="xb-main"><div class="xb-toolbar"><div class="x-tabs" role="tablist">' + tabs.map(function (t) { return '<button type="button" data-tab="' + t[0] + '"' + (st.tab === t[0] ? ' class="on"' : "") + ">" + t[1] + "</button>"; }).join("") +
      '</div><input class="x-input" data-q placeholder="搜索标题或描述" value="' + E(st.q) + '"></div><div data-body><p class="x-frac">正在加载…</p></div></div>';
    var reload = function () { listPage(el, k, opts); };
    el.querySelector("[data-new]").onclick = function () { itemForm(k, null, reload); };
    el.querySelector("[data-refresh]").onclick = reload;
    [].forEach.call(el.querySelectorAll("[data-tab]"), function (b) { b.onclick = function () { st.tab = b.getAttribute("data-tab"); reload(); }; });
    var qi = el.querySelector("[data-q]");
    qi.onkeydown = function (e) { if (e.key === "Enter") { st.q = qi.value.trim(); reload(); } };
    var q = { page_size: 200, q: st.q };
    if (st.tab === "mine") q.assignee = "me";
    else if (st.tab === "open") q.status = KIND[k].st.map(function (s) { return s[0]; }).filter(function (s) { return DONE[k].indexOf(s) < 0; }).join(",");
    else if (st.tab === "done") q.status = DONE[k].join(",");
    else if (st.tab !== "all") q.status = st.tab;
    Promise.all([xs().get("/devcenter/" + k, q), xs().get("/devcenter/iterations", { page_size: 200 }).catch(function () { return { items: [] }; })]).then(function (r) {
      var list = items(r[0]), iters = {};
      items(r[1]).forEach(function (i) { iters[i.id] = i.title; });
      var rows = list.map(function (x) {
        var acts = '<button class="x-act" data-edit="' + x.id + '">编辑</button>';
        if (k === "requirements" && x.status === "accepting") acts = '<button class="x-btn x-btn--pri x-btn--s" data-accept="' + x.id + '">验收</button>' + acts;
        else if (k === "requirements") { var nx = { todo: "ready", ready: "doing", doing: "testing", testing: "accepting" }[x.status]; if (nx) acts = '<button class="x-act" data-next="' + x.id + '" data-to="' + nx + '">→ ' + stOf(k, nx)[1] + "</button>" + acts; }
        if (k === "bugs") { var bn = { todo: "doing", reopened: "doing", doing: "fixed", fixed: "verified" }[x.status]; if (bn) acts = '<button class="x-act" data-next="' + x.id + '" data-to="' + bn + '">→ ' + stOf(k, bn)[1] + "</button>" + acts; if (x.status === "fixed") acts += '<button class="x-act" data-next="' + x.id + '" data-to="reopened">重新打开</button>'; }
        if (k === "tests") acts = '<button class="x-act" data-next="' + x.id + '" data-to="passed">通过</button><button class="x-act" data-next="' + x.id + '" data-to="failed">失败</button>' + acts;
        acts += '<button class="x-act x-act--danger" data-del="' + x.id + '">删除</button>';
        var extra = k === "bugs" ? ({ critical: "致命", major: "严重", minor: "一般", trivial: "轻微" }[data(x).severity] || "—") : k === "tests" ? (data(x).requirement ? "#" + E(data(x).requirement) : "—") : (x.iteration_id ? E(iters[x.iteration_id] || "#" + x.iteration_id) : "—");
        return ['<span class="x-mono">#' + x.id + "</span>", '<b class="x-name" data-open="' + x.id + '" style="cursor:pointer">' + E(x.title) + "</b>", stTag(k, x.status), priTag(x.priority), extra, E(x.assignee || "—"), fmt(x.updated_at), '<span class="xb-ops">' + acts + "</span>"];
      });
      var extraCol = k === "bugs" ? "严重程度" : k === "tests" ? "关联需求" : "迭代";
      el.querySelector("[data-body]").innerHTML = table(["编号", "标题", "状态", "优先级", extraCol, "处理人", "更新时间", "操作"], rows,
        st.tab === "accepting" ? "没有待验收的需求。" : "还没有" + KIND[k].name + "，点右上角新建。") + '<p class="x-frac xb-note">共 ' + ((r[0] && r[0].total) != null ? r[0].total : list.length) + " 条</p>";
      var byId = function (id) { return list.filter(function (x) { return String(x.id) === String(id); })[0]; };
      var on = function (sel, fn) { [].forEach.call(el.querySelectorAll(sel), function (b) { b.onclick = function () { fn(b); }; }); };
      on("[data-edit]", function (b) { itemForm(k, byId(b.getAttribute("data-edit")), reload); });
      on("[data-open]", function (b) { itemForm(k, byId(b.getAttribute("data-open")), reload); });
      on("[data-del]", function (b) { confirmDel(k, byId(b.getAttribute("data-del")), reload); });
      on("[data-next]", function (b) { setStatus(k, byId(b.getAttribute("data-next")), b.getAttribute("data-to"), reload); });
      on("[data-accept]", function (b) { acceptDialog(byId(b.getAttribute("data-accept")), reload); });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
  }

  function acceptDialog(x, done) {
    var crit = String(data(x).acceptance || "").split(/\n+/).map(function (s) { return s.trim(); }).filter(Boolean);
    var m = document.createElement("div");
    m.className = "xb-confirm";
    m.innerHTML = '<div class="xb-confirm-card" role="dialog" aria-modal="true" style="width:min(560px,calc(100vw - 32px))"><h3>验收需求 · #' + x.id + " " + E(x.title) + '</h3><div class="xb-confirm-body">' +
      (crit.length ? '<p class="x-frac" style="margin:0 0 8px">逐条核对验收标准：</p>' + crit.map(function (c, i) { return '<label style="display:flex;gap:8px;align-items:flex-start;margin:6px 0"><input type="checkbox" data-c="' + i + '" style="width:auto;margin-top:4px"><span>' + E(c) + "</span></label>"; }).join("") : '<p class="x-frac">这条需求没有填写验收标准。</p>') +
      '<textarea class="x-input" data-note rows="3" placeholder="验收意见（驳回时必填）" style="width:100%;margin-top:10px"></textarea></div><p class="fk-err" hidden></p>' +
      '<div class="xb-confirm-foot"><button class="x-btn" data-no>取消</button><button class="x-btn" data-reject>驳回重做</button><button class="x-btn x-btn--pri" data-pass>验收通过</button></div></div>';
    document.body.appendChild(m);
    var err = m.querySelector(".fk-err"), note = m.querySelector("[data-note]");
    m.querySelector("[data-no]").onclick = function () { m.remove(); };
    var go = function (s) {
      var rec = { accepted_by: me(), accepted_at: new Date().toISOString(), acceptance_note: note.value.trim(), acceptance_result: s === "done" ? "pass" : "reject" };
      xs().patch("/devcenter/requirements/" + x.id, { status: s, data: Object.assign({}, data(x), rec) }).then(function () { m.remove(); toast(s === "done" ? "验收通过，需求已完成" : "已驳回，需求退回开发中"); done(); })
        .catch(function (e) { err.textContent = errText(e); err.hidden = false; });
    };
    m.querySelector("[data-pass]").onclick = function () {
      var boxes = m.querySelectorAll("[data-c]"), unchecked = [].filter.call(boxes, function (b) { return !b.checked; }).length;
      if (unchecked) { err.textContent = "还有 " + unchecked + " 条验收标准未勾选"; err.hidden = false; return; }
      go("done");
    };
    m.querySelector("[data-reject]").onclick = function () { if (!note.value.trim()) { err.textContent = "驳回时请填写验收意见"; err.hidden = false; return; } go("doing"); };
  }

  /* ---------------------------------------------------------------- 工作台 */
  function workbench(el) {
    var u = xs().user || {};
    var h = new Date().getHours(), hi = h < 11 ? "早上好" : h < 14 ? "中午好" : h < 18 ? "下午好" : "晚上好";
    el.innerHTML = head(hi + "，" + (u.display_name || u.username || ""), "这里汇总了指派给你的需求、缺陷和测试，以及团队最近的变化。", '<button class="x-btn x-btn--pri" data-newreq>+ 新建需求</button>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    el.querySelector("[data-newreq]").onclick = function () { itemForm("requirements", null, function () { workbench(el); }); };
    Promise.all([xs().get("/devcenter/summary"), xs().get("/devcenter/requirements", { assignee: "me", status: "todo,ready,doing,testing,accepting", page_size: 8 }),
      xs().get("/devcenter/bugs", { assignee: "me", status: "todo,doing,reopened,fixed", page_size: 8 }), xs().get("/devcenter/iterations", { status: "doing", page_size: 5 }),
      xs().get("/devcenter/requirements", { page_size: 6 })]).then(function (r) {
      var s = r[0] || {}, rq = s.requirements || { by_status: {} }, bg = s.bugs || { by_status: {} }, ts = s.tests || { by_status: {} };
      var sum = function (o, ks) { return ks.reduce(function (a, k) { return a + ((o.by_status || {})[k] || 0); }, 0); };
      var cards = [
        ["我负责的需求", rq.mine_open || 0, "#/requirements"],
        ["待验收", (rq.by_status || {}).accepting || 0, "#/requirements/accept"],
        ["未解决缺陷", sum(bg, ["todo", "doing", "reopened"]), "#/bugs"],
        ["测试通过率", ts.total ? Math.round(((ts.by_status || {}).passed || 0) * 100 / ts.total) + "%" : "—", "#/testing"]
      ];
      var line = function (k, x) { return '<li><a href="#/' + (k === "bugs" ? "bugs" : "requirements") + '" style="display:flex;gap:10px;align-items:center;text-decoration:none;color:inherit;padding:8px 0;border-top:1px solid #eef1f5"><span class="x-mono">#' + x.id + "</span>" + stTag(k, x.status) + '<span style="flex:1">' + E(x.title) + "</span>" + priTag(x.priority) + '<span class="x-frac">' + fmt(x.updated_at) + "</span></a></li>"; };
      var my = items(r[1]).map(function (x) { return line("requirements", x); }).concat(items(r[2]).map(function (x) { return line("bugs", x); }));
      var iters = items(r[3]);
      el.querySelector("[data-body]").innerHTML = '<div class="xb-stats">' + cards.map(function (c) { return '<a class="xb-stat" href="' + c[2] + '" style="text-decoration:none;color:inherit"><span>' + c[0] + "</span><b>" + E(c[1]) + "</b><em>查看 →</em></a>"; }).join("") + "</div>" +
        '<div class="xb-split" style="grid-template-columns:minmax(0,1.4fr) minmax(0,1fr)"><section class="xb-main"><h3 style="margin:0 0 6px;font-size:15px">待我处理</h3>' + (my.length ? '<ul style="list-style:none;margin:0;padding:0">' + my.join("") + "</ul>" : '<p class="xb-empty">没有指派给你的未完成事项。</p>') + "</section>" +
        '<section class="xb-main"><h3 style="margin:0 0 6px;font-size:15px">进行中的迭代</h3>' + (iters.length ? iters.map(function (i) { var d = data(i); return '<a href="#/project/board" style="display:block;padding:8px 0;border-top:1px solid #eef1f5;text-decoration:none;color:inherit"><b>' + E(i.title) + '</b><div class="x-frac">' + E((d.start || "") + (d.end ? " → " + d.end : "")) + (d.goal ? " · " + E(d.goal) : "") + "</div></a>"; }).join("") : '<p class="xb-empty">没有进行中的迭代。</p>') +
        '<h3 style="margin:16px 0 6px;font-size:15px">团队最近更新</h3>' + (items(r[4]).length ? '<ul style="list-style:none;margin:0;padding:0">' + items(r[4]).map(function (x) { return line("requirements", x); }).join("") + "</ul>" : '<p class="xb-empty">还没有需求。</p>') + "</section></div>";
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
  }

  /* ---------------------------------------------------------------- 迭代 & 看板 */
  function sprints(el) {
    el.innerHTML = head("迭代", "按迭代规划需求与缺陷，跟踪完成进度。", '<button class="x-btn" data-refresh>刷新</button><button class="x-btn x-btn--pri" data-new>+ 新建迭代</button>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { sprints(el); };
    el.querySelector("[data-new]").onclick = function () { itemForm("iterations", null, reload); };
    el.querySelector("[data-refresh]").onclick = reload;
    Promise.all([xs().get("/devcenter/iterations", { page_size: 200 }), xs().get("/devcenter/requirements", { page_size: 500 }), xs().get("/devcenter/bugs", { page_size: 500 })]).then(function (r) {
      var reqs = items(r[1]), bugs = items(r[2]);
      var rows = items(r[0]).map(function (i) {
        var rs = reqs.filter(function (x) { return x.iteration_id === i.id; }), bs = bugs.filter(function (x) { return x.iteration_id === i.id; });
        var done = rs.filter(function (x) { return x.status === "done"; }).length, pct = rs.length ? Math.round(done * 100 / rs.length) : 0, d = data(i);
        var acts = (i.status === "todo" ? '<button class="x-act" data-st="' + i.id + '" data-to="doing">开始</button>' : i.status === "doing" ? '<button class="x-act" data-st="' + i.id + '" data-to="done">结束</button>' : "") +
          '<button class="x-act" data-board="' + i.id + '">看板</button><button class="x-act" data-edit="' + i.id + '">编辑</button><button class="x-act x-act--danger" data-del="' + i.id + '">删除</button>';
        return ["<b>" + E(i.title) + "</b>" + (d.goal ? '<div class="x-frac">' + E(d.goal) + "</div>" : ""), stTag("iterations", i.status), E((d.start || "—") + " → " + (d.end || "—")), E(i.assignee || "—"),
          '<div style="display:flex;align-items:center;gap:8px;min-width:160px"><div style="flex:1;height:6px;border-radius:3px;background:#eef1f5"><div style="width:' + pct + '%;height:6px;border-radius:3px;background:#1664ff"></div></div><span class="x-frac">' + done + "/" + rs.length + "</span></div>",
          bs.length ? bs.filter(function (b) { return DONE.bugs.indexOf(b.status) < 0; }).length + " / " + bs.length : "0", '<span class="xb-ops">' + acts + "</span>"];
      });
      el.querySelector("[data-body]").innerHTML = '<div class="xb-main">' + table(["迭代", "状态", "周期", "负责人", "需求完成", "未解决缺陷", "操作"], rows, "还没有迭代，点右上角新建。") + "</div>";
      var list = items(r[0]), byId = function (id) { return list.filter(function (x) { return String(x.id) === String(id); })[0]; };
      var on = function (sel, fn) { [].forEach.call(el.querySelectorAll(sel), function (b) { b.onclick = function () { fn(b); }; }); };
      on("[data-edit]", function (b) { itemForm("iterations", byId(b.getAttribute("data-edit")), reload); });
      on("[data-del]", function (b) { confirmDel("iterations", byId(b.getAttribute("data-del")), reload); });
      on("[data-st]", function (b) { setStatus("iterations", byId(b.getAttribute("data-st")), b.getAttribute("data-to"), reload); });
      on("[data-board]", function (b) { BOARD.iter = b.getAttribute("data-board"); location.hash = "#/project/board"; });
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
  }

  var BOARD = { iter: "" };
  function board(el) {
    el.innerHTML = head("看板", "拖动卡片到其它列即可改变需求状态。", '<select class="x-input" data-iter style="width:220px"><option value="">全部需求</option></select><button class="x-btn x-btn--pri" data-new>+ 新建需求</button>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    var reload = function () { board(el); };
    el.querySelector("[data-new]").onclick = function () { itemForm("requirements", null, reload, BOARD.iter ? { iteration_id: BOARD.iter } : null); };
    Promise.all([xs().get("/devcenter/iterations", { page_size: 200 }), xs().get("/devcenter/requirements", BOARD.iter ? { iteration_id: BOARD.iter, page_size: 500 } : { page_size: 500 })]).then(function (r) {
      var sel = el.querySelector("[data-iter]");
      sel.innerHTML = '<option value="">全部需求</option>' + items(r[0]).map(function (i) { return '<option value="' + i.id + '"' + (String(i.id) === String(BOARD.iter) ? " selected" : "") + ">" + E(i.title) + "</option>"; }).join("");
      sel.onchange = function () { BOARD.iter = sel.value; reload(); };
      var reqs = items(r[1]), cols = KIND.requirements.st.filter(function (s) { return s[0] !== "rejected"; });
      el.querySelector("[data-body]").innerHTML = '<div class="xd-board">' + cols.map(function (c) {
        var cs = reqs.filter(function (x) { return x.status === c[0]; });
        return '<section class="xd-col" data-col="' + c[0] + '"><header><span class="x-tag x-tag--s ' + c[2] + '">' + c[1] + "</span><b>" + cs.length + "</b></header>" +
          cs.map(function (x) { return '<article class="xd-card" draggable="true" data-card="' + x.id + '"><div class="xd-card-t">' + E(x.title) + '</div><div class="xd-card-m"><span class="x-mono">#' + x.id + "</span>" + priTag(x.priority) + '<span class="x-frac">' + E(x.assignee || "未指派") + "</span></div></article>"; }).join("") + "</section>";
      }).join("") + "</div>";
      var byId = function (id) { return reqs.filter(function (x) { return String(x.id) === String(id); })[0]; };
      [].forEach.call(el.querySelectorAll("[data-card]"), function (c) {
        c.ondragstart = function (e) { e.dataTransfer.setData("text/plain", c.getAttribute("data-card")); c.classList.add("drag"); };
        c.ondragend = function () { c.classList.remove("drag"); };
        c.ondblclick = function () { itemForm("requirements", byId(c.getAttribute("data-card")), reload); };
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
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
  }

  /* ---------------------------------------------------------------- 代码仓库 / 合并请求 */
  function repos(el, sub) {
    if (sub === "mr") {
      el.innerHTML = head("合并请求", "") + '<div class="xb-guide"><h3>合并请求在代码托管平台完成</h3><p class="x-frac" style="line-height:1.8">平台不托管 Git 代码。合并请求、代码评审在 GitLab / GitHub / Gitea 中进行；把仓库的 Push / Merge Webhook 指向流水线后，合并即可自动触发构建与部署。</p><a class="x-btn x-btn--pri" href="#/cicd">前往流水线配置 Webhook</a></div>';
      return;
    }
    el.innerHTML = head("代码仓库", "已接入平台的代码仓库（来自 CI/CD 应用），可直接读取分支、配置凭证。", '<a class="x-btn x-btn--pri" href="#/cicd">+ 从代码仓库创建应用</a>') + '<div data-body><p class="x-frac">正在加载…</p></div>';
    xs().get("/cicd/applications", { page_size: 200 }).then(function (r) {
      var rows = items(r).map(function (a) { return ["<b>" + E(a.name) + "</b>", '<span class="x-mono">' + E(a.repo_url || a.git_url || a.repo || "—") + "</span>", E(a.default_branch || a.branch || "—"), E(a.env || "—"), fmt(a.updated_at || a.created_at)]; });
      el.querySelector("[data-body]").innerHTML = '<div class="xb-main">' + table(["应用", "仓库地址", "默认分支", "环境", "更新时间"], rows, "还没有接入代码仓库。到 CI/CD 用「从代码仓库创建应用」接入。") + "</div>";
    }).catch(function (e) { el.querySelector("[data-body]").innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>"; });
  }

  function render(el, key) {
    var parts = (location.hash || "").split("?")[0].split("/"), sub = parts[2] || "";
    if (key === "workbench") return workbench(el);
    if (key === "requirements") return sub === "accept" ? listPage(el, "requirements", { tab: "accepting", title: "需求验收" }) : listPage(el, "requirements");
    if (key === "bugs") return listPage(el, "bugs");
    if (key === "testing") return listPage(el, "tests");
    if (key === "project") return sub === "board" ? board(el) : sprints(el);
    if (key === "coding") return repos(el, sub);
  }
  root.XSDev = { render: render };
})(window);
