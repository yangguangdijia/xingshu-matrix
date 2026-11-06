(function (root) {
  "use strict";
  /*
   * 通知渠道（通知中心）：渠道 / 订阅规则 / 消息模板 / 发送记录 / 通知安全。
   * 对标夜莺、KubeSphere 的通知管理。实时模式下由「监控告警 → 通知渠道」页面调用：
   *   window.XSNotifyCenter.render(el)
   * 依赖：root.XS.api（接口）、XSFormKit（表单）、XSChannelForm（渠道表单）、全局 dlg / closeAll / k8Confirm（对话框）。
   */
  var TABS = [
    { k: "channels", t: "通知渠道" },
    { k: "subs", t: "订阅规则" },
    { k: "templates", t: "消息模板" },
    { k: "records", t: "发送记录" },
    { k: "security", t: "通知安全" }
  ];
  var KIND = { alert: "告警", incident: "故障", pipeline: "流水线", approval: "审批待办", test: "测试" };
  var STATUS = {
    firing: "触发", resolved: "已恢复", declared: "已声明", assigned: "已指派", acknowledged: "已认领",
    succeeded: "成功", failed: "失败", cancelled: "已取消", pending: "待审批", test: "测试", aggregated: "汇总"
  };
  var STATUS_OPTS = [
    ["alert", "firing"], ["alert", "resolved"], ["incident", "declared"], ["incident", "assigned"], ["incident", "acknowledged"], ["incident", "resolved"],
    ["pipeline", "succeeded"], ["pipeline", "failed"], ["pipeline", "cancelled"], ["approval", "pending"]
  ];
  var SEV = { critical: "严重", warning: "警告", info: "提示" };
  var TYPE_NAME = { feishu: "飞书", dingtalk: "钉钉", wecom: "企业微信", email: "邮件", slack: "Slack", webhook: "通用 Webhook" };
  var LOGO = { feishu: "/assets/channels/feishu.svg", dingtalk: "/assets/channels/dingtalk.svg", wecom: "/assets/channels/wecom.png" };
  var REC_ST = { success: ["成功", "x-green"], failed: ["失败", "x-red"], suppressed: ["已抑制", "x-grey"] };
  var WEEK = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
  var RANGES = [{ v: "1h", t: "近 1 小时", s: 3600 }, { v: "24h", t: "近 24 小时", s: 86400 }, { v: "7d", t: "近 7 天", s: 604800 }, { v: "30d", t: "近 30 天", s: 2592000 }];

  var S = { tab: "channels", el: null, seq: 0, channels: [], subs: [], templates: [], defaults: [], rec: { page: 1, size: 20, range: "24h", channel_id: "", event_kind: "", status: "", q: "" } };

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; });
  }
  function api(path, opts) { return root.XS.api(path, opts || {}); }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function errMsg(e) {
    var m = e && e.message || "";
    if (/route not found|404/i.test(m)) return "该功能需要更新服务端后才能使用";
    if (/forbidden|403/i.test(m) && !/[\u4e00-\u9fa5]/.test(m)) return "没有权限执行该操作";
    return m || "请求失败";
  }
  function tag(t, cls) { return '<span class="x-tag ' + (cls || "x-grey") + ' x-tag--s">' + esc(t) + "</span>"; }
  function fmt(t) {
    if (!t) return "—";
    var d = new Date(t);
    if (isNaN(d)) return "—";
    var p = function (n) { return String(n).padStart(2, "0"); };
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }
  function modal(title, body, foot) {
    if (typeof dlg === "function") { dlg(title, body, foot); return document.getElementById("modal"); } // eslint-disable-line no-undef
    toast("对话框组件未加载，请刷新页面");
    return null;
  }
  function closeModal() { if (typeof closeAll === "function") closeAll(); } // eslint-disable-line no-undef
  function confirmDo(title, msg, ok, fn) {
    if (typeof root.k8Confirm === "function") return root.k8Confirm(esc(title), esc(msg), esc(ok), fn);
    if (root.confirm(title + "\n" + msg)) fn();
  }
  function table(head, rows, empty) {
    if (!rows.length) return '<p class="x-empty">' + esc(empty || "暂无数据") + "</p>";
    return '<div style="overflow-x:auto"><table class="x-table"><thead><tr>' + head.map(function (h) { return "<th>" + esc(h) + "</th>"; }).join("") +
      "</tr></thead><tbody>" + rows.map(function (r) { return "<tr>" + r.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>"; }).join("") + "</tbody></table></div>";
  }
  function btn(label, attrs, pri) { return '<button class="x-btn' + (pri ? " x-btn--pri" : "") + '" type="button" ' + attrs + ">" + esc(label) + "</button>"; }
  function list(v) { return Array.isArray(v) ? v : v && Array.isArray(v.items) ? v.items : []; }
  function chName(id) { for (var i = 0; i < S.channels.length; i++) if (S.channels[i].id === id) return S.channels[i].name; return "#" + id; }

  // 删除类高危操作：输入名称确认
  function confirmName(title, name, desc, okText, fn) {
    var m = modal(esc(title), '<form class="x-form" id="nc-del" novalidate><p class="x-desc" style="margin:0">' + desc + '</p>' +
      '<label><span>请输入名称 <b class="x-mono">' + esc(name) + '</b> 以确认</span><input class="x-input" name="confirm" autocomplete="off" spellcheck="false" /></label>' +
      '<span class="x-err" id="nc-del-err" role="alert" hidden></span></form>',
      '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="nc-del" style="background:#f5222d;border-color:#f5222d">' + esc(okText || "删除") + "</button>");
    if (!m) return;
    var f = document.getElementById("nc-del"), e = document.getElementById("nc-del-err");
    f.elements.confirm.focus();
    f.onsubmit = function (ev) {
      ev.preventDefault();
      if (f.elements.confirm.value.trim() !== name) { e.textContent = "名称不一致"; e.hidden = false; f.elements.confirm.focus(); return; }
      var b = document.querySelector('[form="nc-del"][type="submit"]'); if (b) b.disabled = true;
      Promise.resolve(fn()).then(function () { closeModal(); }).catch(function (err) { e.textContent = errMsg(err); e.hidden = false; if (b) b.disabled = false; });
    };
  }

  /* -------------------------------------------------------------- 页面骨架 */

  function render(el) {
    S.el = el;
    el.innerHTML = '<div id="nc"><div class="x-sethead"><div><h1 class="x-h1">通知渠道</h1></div><div class="x-phacts" data-nc-tools></div></div>' +
      '<div class="x-bar"><div class="x-tabs" role="tablist">' + TABS.map(function (t) {
        return '<button type="button" role="tab" data-nc-tab="' + t.k + '" aria-selected="' + (t.k === S.tab) + '" class="' + (t.k === S.tab ? "on" : "") + '">' + esc(t.t) + "</button>";
      }).join("") + '</div></div><div data-nc-body style="margin-top:16px"><section class="x-panel"><p class="x-empty">正在加载…</p></section></div></div>';
    [].forEach.call(el.querySelectorAll("[data-nc-tab]"), function (b) {
      b.onclick = function () { S.tab = b.getAttribute("data-nc-tab"); render(el); };
    });
    draw();
  }
  function body() { return S.el && S.el.querySelector("[data-nc-body]"); }
  function tools(html) { var t = S.el && S.el.querySelector("[data-nc-tools]"); if (t) t.innerHTML = html; return t; }
  function draw() {
    var seq = ++S.seq;
    var fn = { channels: drawChannels, subs: drawSubs, templates: drawTemplates, records: drawRecords, security: drawSecurity }[S.tab];
    fn(seq);
  }
  function alive(seq) { return seq === S.seq && S.el && document.body.contains(S.el); }
  function fail(seq, err) {
    if (!alive(seq)) return;
    body().innerHTML = '<section class="x-panel"><p class="x-aerr" role="alert">' + esc(errMsg(err)) + '</p><p style="margin:12px 0 0">' + btn("重试", "data-nc-retry") + "</p></section>";
    body().querySelector("[data-nc-retry]").onclick = draw;
  }
  function loadChannels() { return api("/observability/channels").then(function (r) { S.channels = list(r); return S.channels; }); }
  function loadTemplates() {
    return api("/observability/notify/templates").then(function (r) { S.templates = r && r.items || []; S.defaults = r && r.defaults || []; return S.templates; });
  }

  /* -------------------------------------------------------------- 渠道 */

  function chIcon(type) {
    return LOGO[type] ? '<img src="' + LOGO[type] + '" alt="" aria-hidden="true" style="width:22px;height:22px;object-fit:contain;vertical-align:middle;margin-right:8px" />' : "";
  }
  function drawChannels(seq) {
    var t = tools(btn("刷新", "data-nc-refresh") + btn("+ 新建渠道", "data-nc-new", true));
    t.querySelector("[data-nc-refresh]").onclick = draw;
    t.querySelector("[data-nc-new]").onclick = function () { openChannel(null); };
    Promise.all([loadChannels(), api("/observability/notify/records", { query: { page_size: 1 } }).catch(function () { return null; })]).then(function (res) {
      if (!alive(seq)) return;
      var st = res[1] && res[1].last_24h;
      var stats = st ? '<div class="x-tbar" style="gap:24px;margin:0 0 12px"><span>近 24 小时发送：成功 <b>' + esc(st.success) + "</b></span><span>失败 <b" + (st.failed ? ' style="color:#f5222d"' : "") + ">" + esc(st.failed) +
        "</b></span><span>已抑制 <b>" + esc(st.suppressed) + "</b></span>" + (st.failed ? btn("查看失败记录", "data-nc-failed") : "") + "</div>" : "";
      var rows = S.channels.map(function (c) {
        var subs = c.subscriptions || [];
        var last = c.last_sent_at ? fmt(c.last_sent_at) + " " + (c.last_status === "failed" ? tag("失败", "x-red") : tag("成功", "x-green")) : '<span class="x-frac">未发送</span>';
        return [
          chIcon(c.type) + "<b>" + esc(c.name) + "</b>" + (c.description ? '<div class="x-frac" style="font-size:12px">' + esc(c.description) + "</div>" : ""),
          tag(TYPE_NAME[c.type] || c.type, "x-blue"),
          c.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
          subs.length ? esc(subs.map(function (s) { return s.name; }).join("、")) : '<span class="x-frac">未被订阅</span>',
          last,
          '<span class="ops-nowrap" style="display:inline-flex;gap:4px">' + btn("编辑", 'data-ch-edit="' + c.id + '"') + btn("测试", 'data-ch-test="' + c.id + '"') +
            btn(c.enabled ? "停用" : "启用", 'data-ch-toggle="' + c.id + '"') + btn("发送记录", 'data-ch-rec="' + c.id + '"') + btn("删除", 'data-ch-del="' + c.id + '"') + "</span>"
        ];
      });
      body().innerHTML = '<section class="x-panel">' + stats + table(["名称", "类型", "状态", "订阅规则", "最近发送", "操作"], rows,
        "尚未配置通知渠道。支持钉钉、飞书、企业微信、邮件、Slack 与通用 Webhook。") +
        '<p class="x-frac" style="margin:12px 0 0;font-size:12px">渠道只负责「发到哪里」；告警、故障、流水线结果与审批待办发给谁，由「订阅规则」决定。</p></section>';
      bindChannels();
    }).catch(function (e) { fail(seq, e); });
  }
  function byId(id) { for (var i = 0; i < S.channels.length; i++) if (String(S.channels[i].id) === String(id)) return S.channels[i]; return null; }
  function openChannel(existing) {
    if (!root.XSChannelForm) return toast("渠道表单未加载，请刷新页面");
    if (root.XSChannelForm.resetTemplates) root.XSChannelForm.resetTemplates();
    root.XSChannelForm.open(existing, function () { draw(); });
  }
  function bindChannels() {
    var b = body();
    var f = b.querySelector("[data-nc-failed]");
    if (f) f.onclick = function () { S.rec.status = "failed"; S.rec.page = 1; S.tab = "records"; render(S.el); };
    [].forEach.call(b.querySelectorAll("[data-ch-edit]"), function (x) { x.onclick = function () { openChannel(byId(x.getAttribute("data-ch-edit"))); }; });
    [].forEach.call(b.querySelectorAll("[data-ch-rec]"), function (x) {
      x.onclick = function () { S.rec = { page: 1, size: 20, range: "7d", channel_id: x.getAttribute("data-ch-rec"), event_kind: "", status: "", q: "" }; S.tab = "records"; render(S.el); };
    });
    [].forEach.call(b.querySelectorAll("[data-ch-test]"), function (x) {
      x.onclick = function () {
        x.disabled = true; x.textContent = "发送中…";
        api("/observability/channels/" + encodeURIComponent(x.getAttribute("data-ch-test")) + "/test", { method: "POST", body: {} }).then(function (r) {
          toast(r && r.ok === false ? "测试失败：" + (r.error || "对方未接受消息") : "测试消息已发送（" + (r && r.duration_ms || 0) + " ms），请到对应渠道确认");
          draw();
        }).catch(function (e) { toast(errMsg(e)); x.disabled = false; x.textContent = "测试"; });
      };
    });
    [].forEach.call(b.querySelectorAll("[data-ch-toggle]"), function (x) {
      x.onclick = function () {
        var c = byId(x.getAttribute("data-ch-toggle")); if (!c) return;
        var go = function () {
          api("/observability/channels/" + c.id + "/enabled", { method: "PUT", body: { enabled: !c.enabled } })
            .then(function () { toast(c.enabled ? "渠道已停用" : "渠道已启用"); draw(); }).catch(function (e) { toast(errMsg(e)); });
        };
        if (c.enabled && (c.subscriptions || []).length) confirmDo("停用通知渠道", "停用后，" + c.subscriptions.length + " 条订阅规则不会再发送到「" + c.name + "」。", "停用", go);
        else go();
      };
    });
    [].forEach.call(b.querySelectorAll("[data-ch-del]"), function (x) {
      x.onclick = function () {
        var c = byId(x.getAttribute("data-ch-del")); if (!c) return;
        api("/observability/channels/" + c.id + "/references").then(function (r) {
          var subs = r && r.subscriptions || [];
          var desc = subs.length
            ? "该渠道正被 <b>" + subs.length + "</b> 条订阅规则引用：" + esc(subs.map(function (s) { return s.name; }).join("、")) + "。删除后会从这些规则中移除，没有其他渠道的规则将被停用。"
            : "删除后不可恢复，历史发送记录会保留。";
          confirmName("删除通知渠道", c.name, desc, "删除", function () {
            return api("/observability/channels/" + c.id, { method: "DELETE", query: subs.length ? { force: "true" } : {} }).then(function () { toast("渠道已删除"); draw(); });
          });
        }).catch(function (e) { toast(errMsg(e)); });
      };
    });
  }

  /* -------------------------------------------------------------- 订阅规则 */

  function subSummary(s) {
    var parts = [];
    if ((s.statuses || []).length) parts.push("状态：" + s.statuses.map(function (x) { return STATUS[x] || x; }).join("/"));
    if ((s.severities || []).length) parts.push("级别：" + s.severities.map(function (x) { return SEV[x] || x; }).join("/"));
    if ((s.envs || []).length) parts.push("环境：" + s.envs.join("/"));
    if ((s.clusters || []).length) parts.push("集群：" + s.clusters.join("/"));
    var ls = Object.keys(s.match_labels || {});
    if (ls.length) parts.push("标签：" + ls.map(function (k) { return k + "=" + s.match_labels[k]; }).join(", "));
    return parts.length ? esc(parts.join("；")) : '<span class="x-frac">全部</span>';
  }
  function subPolicy(s) {
    var p = [];
    if (s.quiet && s.quiet.enabled) p.push("静默 " + s.quiet.start + "-" + s.quiet.end + (s.quiet.allow_critical ? "（严重除外）" : ""));
    if (s.dedup_minutes) p.push(s.dedup_minutes + " 分钟内不重复");
    if (s.aggregate) p.push("聚合发送");
    return p.length ? esc(p.join("；")) : '<span class="x-frac">—</span>';
  }
  function drawSubs(seq) {
    var t = tools(btn("刷新", "data-nc-refresh") + btn("+ 新建订阅", "data-nc-new", true));
    t.querySelector("[data-nc-refresh]").onclick = draw;
    t.querySelector("[data-nc-new]").onclick = function () { openSub(null); };
    Promise.all([loadChannels(), api("/observability/notify/subscriptions")]).then(function (r) {
      if (!alive(seq)) return;
      S.subs = list(r[1]);
      var rows = S.subs.map(function (s) {
        var chs = (s.channel_ids || []).map(chName);
        return [
          "<b>" + esc(s.name) + "</b>" + (s.description ? '<div class="x-frac" style="font-size:12px">' + esc(s.description) + "</div>" : ""),
          esc((s.event_kinds || []).map(function (k) { return KIND[k] || k; }).join("、")),
          subSummary(s),
          chs.length ? esc(chs.join("、")) : tag("无渠道", "x-orange"),
          subPolicy(s),
          s.enabled ? tag("启用", "x-green") : tag("停用", "x-grey"),
          '<span class="ops-nowrap" style="display:inline-flex;gap:4px">' + btn("编辑", 'data-sub-edit="' + s.id + '"') + btn(s.enabled ? "停用" : "启用", 'data-sub-toggle="' + s.id + '"') + btn("删除", 'data-sub-del="' + s.id + '"') + "</span>"
        ];
      });
      body().innerHTML = '<section class="x-panel">' + table(["名称", "事件", "匹配条件", "通知渠道", "发送策略", "状态", "操作"], rows,
        "还没有订阅规则。订阅规则决定哪些告警、故障、流水线结果和审批待办发送到哪些渠道。") +
        '<p class="x-frac" style="margin:12px 0 0;font-size:12px">同一事件命中多条规则时，每个渠道只发送一次。</p></section>';
      var b = body();
      var find = function (id) { return S.subs.filter(function (s) { return String(s.id) === String(id); })[0]; };
      [].forEach.call(b.querySelectorAll("[data-sub-edit]"), function (x) { x.onclick = function () { openSub(find(x.getAttribute("data-sub-edit"))); }; });
      [].forEach.call(b.querySelectorAll("[data-sub-toggle]"), function (x) {
        x.onclick = function () {
          var s = find(x.getAttribute("data-sub-toggle"));
          api("/observability/notify/subscriptions/" + s.id + "/enabled", { method: "PUT", body: { enabled: !s.enabled } })
            .then(function () { toast(s.enabled ? "订阅已停用" : "订阅已启用"); draw(); }).catch(function (e) { toast(errMsg(e)); });
        };
      });
      [].forEach.call(b.querySelectorAll("[data-sub-del]"), function (x) {
        x.onclick = function () {
          var s = find(x.getAttribute("data-sub-del"));
          confirmName("删除订阅规则", s.name, "删除后匹配的事件将不再通过该规则发送。", "删除", function () {
            return api("/observability/notify/subscriptions/" + s.id, { method: "DELETE" }).then(function () { toast("订阅已删除"); draw(); });
          });
        };
      });
    }).catch(function (e) { fail(seq, e); });
  }
  function loadEnvOptions() {
    var base = ["prod", "staging", "test", "dev"];
    return api("/clusters").then(function (r) { return list(r).map(function (c) { return c.env; }); }).catch(function () { return []; }).then(function (envs) {
      var seen = {}, out = [];
      envs.concat(base).forEach(function (n) { if (n && !seen[n]) { seen[n] = 1; out.push({ v: n, t: n }); } });
      return out;
    });
  }
  function loadClusterOptions() {
    return api("/clusters").then(function (r) { return list(r).map(function (c) { return { v: c.name, t: c.name + (c.env ? "（" + c.env + "）" : "") }; }); }).catch(function () { return []; });
  }
  function openSub(existing) {
    if (!root.XSFormKit) return toast("表单组件未加载，请刷新页面");
    var s = existing || { enabled: true, event_kinds: ["alert"], statuses: [], severities: [], envs: [], clusters: [], match_labels: {}, channel_ids: [], quiet: {}, dedup_minutes: 0, aggregate: false };
    var q = s.quiet || {};
    var values = {
      name: s.name || "", description: s.description || "", enabled: s.enabled !== false, event_kinds: s.event_kinds || [], statuses: s.statuses || [],
      severities: s.severities || [], envs: s.envs || [], clusters: s.clusters || [],
      match_labels: Object.keys(s.match_labels || {}).map(function (k) { return { k: k, v: s.match_labels[k] }; }),
      channel_ids: (s.channel_ids || []).map(String), quiet_enabled: !!q.enabled, quiet_start: q.start || "22:00", quiet_end: q.end || "08:00",
      quiet_weekdays: (q.weekdays || []).map(String), quiet_allow_critical: !!q.allow_critical,
      dedup_minutes: s.dedup_minutes || 0, aggregate: !!s.aggregate
    };
    var quietOn = function (v) { return !!v.quiet_enabled; };
    var toBody = function (v) {
      var labels = {};
      (v.match_labels || []).forEach(function (r) { if (r.k) labels[r.k] = r.v || ""; });
      var kinds = v.event_kinds || [];
      return {
        name: v.name, description: v.description || "", enabled: v.enabled !== false, event_kinds: kinds,
        statuses: (v.statuses || []).filter(function (st) { return STATUS_OPTS.some(function (o) { return o[1] === st && kinds.indexOf(o[0]) >= 0; }); }),
        severities: v.severities || [], envs: v.envs || [], clusters: v.clusters || [], match_labels: labels,
        channel_ids: (v.channel_ids || []).map(Number),
        quiet: { enabled: !!v.quiet_enabled, start: v.quiet_start || "", end: v.quiet_end || "", weekdays: (v.quiet_weekdays || []).map(Number), timezone: "Asia/Shanghai", allow_critical: !!v.quiet_allow_critical },
        dedup_minutes: Number(v.dedup_minutes) || 0, aggregate: !!v.aggregate
      };
    };
    root.XSFormKit.open({
      title: existing ? "编辑订阅规则" : "新建订阅规则",
      subtitle: "按事件类型、级别、环境、集群和标签把事件路由到通知渠道",
      scene: "observability.notify.subscription." + (existing ? "update" : "create"),
      aiExample: "生产环境的严重告警和 P1 故障发到值班飞书群，夜里 22 点到 8 点只发严重的",
      width: "lg", okText: existing ? "保存" : "创建", values: values,
      sections: [
        { title: "基本信息", fields: [
          { k: "name", label: "规则名称", required: true, placeholder: "生产严重告警 → 值班群", check: function (x) { return x.length > 64 ? "不超过 64 个字符" : ""; } },
          { k: "enabled", label: "状态", type: "switch", text: "启用" },
          { k: "description", label: "说明", full: true, placeholder: "可选" }
        ] },
        { title: "匹配条件", desc: "各条件之间为「且」关系；不选表示不限。", fields: [
          { k: "event_kinds", label: "事件类型", type: "checks", required: true, options: [{ v: "alert", t: "告警" }, { v: "incident", t: "故障（声明/指派/认领/解决）" }, { v: "pipeline", t: "流水线结果" }, { v: "approval", t: "审批待办" }] },
          { k: "statuses", label: "事件状态", type: "checks", options: STATUS_OPTS.map(function (o) { return { v: o[1], t: KIND[o[0]] + "·" + STATUS[o[1]] }; }), help: "只保留与所选事件类型对应的状态；不选表示全部状态" },
          { k: "severities", label: "级别", type: "checks", options: [{ v: "critical", t: "严重（含 P1/P2 故障）" }, { v: "warning", t: "警告（含 P3、失败的流水线）" }, { v: "info", t: "提示" }] },
          { k: "envs", label: "环境", type: "checks", options: [], load: loadEnvOptions },
          { k: "clusters", label: "集群", type: "checks", options: [], load: loadClusterOptions },
          { k: "match_labels", label: "标签匹配", type: "kv", keyPh: "namespace", valPh: "payment", addText: "添加标签",
            help: "值以 ~ 开头为正则（如 ~pay-.*），以 ! 开头为不等于；也可匹配 cluster、namespace、env、alertname" }
        ] },
        { title: "通知渠道", fields: [
          { k: "channel_ids", label: "发送到", type: "checks", required: true, options: [], load: function () {
            return loadChannels().then(function (cs) { return cs.map(function (c) { return { v: String(c.id), t: c.name + "（" + (TYPE_NAME[c.type] || c.type) + (c.enabled ? "" : "，已停用") + "）" }; }); });
          } }
        ] },
        { title: "发送策略", fields: [
          { k: "dedup_minutes", label: "限频（分钟）", type: "number", min: 0, max: 1440, help: "同一事件在该时间内只发送一次；0 表示不限" },
          { k: "aggregate", label: "聚合", type: "switch", text: "同一批到达的多条事件合并为一条消息" },
          { k: "quiet_enabled", label: "静默时段", type: "switch", text: "在指定时段内不发送" },
          { k: "quiet_start", label: "开始", show: quietOn, placeholder: "22:00" },
          { k: "quiet_end", label: "结束", show: quietOn, placeholder: "08:00", help: "结束早于开始表示跨天" },
          { k: "quiet_weekdays", label: "生效日", type: "checks", show: quietOn, options: WEEK.map(function (w, i) { return { v: String(i), t: w }; }), help: "不选表示每天；跨天时段按开始那天计算" },
          { k: "quiet_allow_critical", label: "例外", type: "switch", show: quietOn, text: "严重事件不受静默影响" }
        ] }
      ],
      toObject: function (v) { var b = toBody(v); return { apiVersion: "notification.xingshu.io/v1", kind: "Subscription", metadata: { name: b.name }, spec: b }; },
      validate: function (v) {
        if (!(v.event_kinds || []).length) return "至少选择一种事件类型";
        if (!(v.channel_ids || []).length) return "至少选择一个通知渠道";
        var n = Number(v.dedup_minutes);
        if (!(n >= 0 && n <= 1440)) return "限频取值 0~1440 分钟";
        if (v.quiet_enabled) {
          var hm = /^([01]\d|2[0-3]):[0-5]\d$/;
          if (!hm.test(v.quiet_start || "") || !hm.test(v.quiet_end || "")) return "静默时段格式为 HH:MM，例如 22:00";
          if (v.quiet_start === v.quiet_end) return "静默开始与结束不能相同";
        }
        var badRe = (v.match_labels || []).filter(function (r) { if (!r.v || r.v.charAt(0) !== "~") return false; try { new RegExp(r.v.slice(1)); return false; } catch (e) { return true; } });
        if (badRe.length) return "标签正则不正确：" + badRe.map(function (r) { return r.k; }).join(", ");
        return "";
      },
      submit: function (v, _o, ctx) {
        var b = toBody(v);
        return api("/observability/notify/subscriptions" + (existing ? "/" + existing.id : ""), { method: existing ? "PUT" : "POST", body: b }).then(function () {
          ctx.close(); toast(existing ? "订阅已更新" : "订阅已创建"); draw();
        });
      }
    });
  }

  /* -------------------------------------------------------------- 消息模板 */

  function drawTemplates(seq) {
    var t = tools(btn("刷新", "data-nc-refresh") + btn("+ 新建模板", "data-nc-new", true));
    t.querySelector("[data-nc-refresh]").onclick = draw;
    t.querySelector("[data-nc-new]").onclick = function () { openTemplate(null, null); };
    Promise.all([loadTemplates(), loadChannels()]).then(function () {
      if (!alive(seq)) return;
      var used = {};
      S.channels.forEach(function (c) { Object.keys(c.templates || {}).forEach(function (k) { var id = c.templates[k]; (used[id] = used[id] || []).push(c.name); }); });
      var rows = S.templates.map(function (x) {
        return ["<b>" + esc(x.name) + "</b>" + (x.description ? '<div class="x-frac" style="font-size:12px">' + esc(x.description) + "</div>" : ""),
          esc(KIND[x.event_kind] || x.event_kind), esc(x.channel_type ? TYPE_NAME[x.channel_type] || x.channel_type : "通用"),
          used[x.id] ? esc(used[x.id].join("、")) : '<span class="x-frac">未使用</span>', fmt(x.updated_at),
          '<span class="ops-nowrap" style="display:inline-flex;gap:4px">' + btn("编辑", 'data-tpl-edit="' + x.id + '"') + btn("删除", 'data-tpl-del="' + x.id + '"') + "</span>"];
      });
      var defs = S.defaults.map(function (d) {
        return ["<b>" + esc(d.name) + "</b>", esc(KIND[d.event_kind] || d.event_kind), '<span class="x-mono" style="font-size:12px">' + esc(d.title_tpl) + "</span>",
          btn("预览", 'data-def-view="' + d.event_kind + '"') + " " + btn("以此新建", 'data-def-copy="' + d.event_kind + '"')];
      });
      body().innerHTML = '<section class="x-panel"><div class="x-tbar"><h2 class="x-h2">自定义模板</h2></div>' +
        table(["名称", "事件类型", "适用渠道", "使用中的渠道", "更新时间", "操作"], rows, "暂无自定义模板，渠道将使用下方的默认模板。") + "</section>" +
        '<section class="x-panel" style="margin-top:16px"><div class="x-tbar"><h2 class="x-h2">默认模板</h2></div>' + table(["名称", "事件类型", "标题", "操作"], defs) +
        '<p class="x-frac" style="margin:12px 0 0;font-size:12px">模板使用 Go text/template 语法，例如 {{.AlertName}}、{{label "pod" .Labels}}。渠道在编辑时按事件类型选择模板。</p></section>';
      var b = body();
      var find = function (id) { return S.templates.filter(function (x) { return String(x.id) === String(id); })[0]; };
      var def = function (k) { return S.defaults.filter(function (d) { return d.event_kind === k; })[0]; };
      [].forEach.call(b.querySelectorAll("[data-tpl-edit]"), function (x) { x.onclick = function () { openTemplate(find(x.getAttribute("data-tpl-edit")), null); }; });
      [].forEach.call(b.querySelectorAll("[data-def-copy]"), function (x) { x.onclick = function () { openTemplate(null, def(x.getAttribute("data-def-copy"))); }; });
      [].forEach.call(b.querySelectorAll("[data-def-view]"), function (x) { x.onclick = function () { openTemplate(null, def(x.getAttribute("data-def-view")), true); }; });
      [].forEach.call(b.querySelectorAll("[data-tpl-del]"), function (x) {
        x.onclick = function () {
          var tp = find(x.getAttribute("data-tpl-del"));
          var users = used[tp.id] || [];
          confirmName("删除消息模板", tp.name, users.length ? "以下渠道正在使用该模板，删除后改用默认模板：" + esc(users.join("、")) + "。" : "删除后不可恢复。", "删除", function () {
            return api("/observability/notify/templates/" + tp.id, { method: "DELETE", query: users.length ? { force: "true" } : {} }).then(function () {
              if (root.XSChannelForm && root.XSChannelForm.resetTemplates) root.XSChannelForm.resetTemplates();
              toast("模板已删除"); draw();
            });
          });
        };
      });
    }).catch(function (e) { fail(seq, e); });
  }
  function openTemplate(existing, from, readonly) {
    var x = existing || { name: from && !readonly ? from.name + "（副本）" : "", description: "", event_kind: from ? from.event_kind : "alert", channel_type: "", title_tpl: from ? from.title_tpl : "", body_tpl: from ? from.body_tpl : "" };
    var kinds = ["alert", "incident", "pipeline", "approval", "test"];
    var sel = function (name, opts, cur, dis) {
      return '<select class="x-select" name="' + name + '"' + (dis ? " disabled" : "") + ">" + opts.map(function (o) { return '<option value="' + esc(o[0]) + '"' + (o[0] === cur ? " selected" : "") + ">" + esc(o[1]) + "</option>"; }).join("") + "</select>";
    };
    var ro = readonly ? " readonly" : "";
    var html = '<form class="x-form" id="nc-tpl" novalidate>' +
      (readonly ? "" : '<div class="x-row2"><label><span class="x-req">模板名称</span><input class="x-input" name="name" maxlength="64" value="' + esc(x.name) + '" autocomplete="off" /></label>' +
        '<label><span>说明</span><input class="x-input" name="description" maxlength="256" value="' + esc(x.description) + '" autocomplete="off" /></label></div>') +
      '<div class="x-row2"><label><span>事件类型</span>' + sel("event_kind", kinds.map(function (k) { return [k, KIND[k]]; }), x.event_kind, readonly) + "</label>" +
      '<label><span>适用渠道（用于预览）</span>' + sel("channel_type", [["", "通用"]].concat(Object.keys(TYPE_NAME).map(function (k) { return [k, TYPE_NAME[k]]; })), x.channel_type) + "</label></div>" +
      '<label><span class="x-req">标题模板</span><input class="x-input x-mono" name="title_tpl" value="' + esc(x.title_tpl) + '" spellcheck="false"' + ro + " /></label>" +
      '<label><span class="x-req">正文模板</span><textarea class="x-input x-mono" name="body_tpl" rows="10" spellcheck="false" style="min-height:180px"' + ro + ">" + esc(x.body_tpl) + "</textarea></label>" +
      '<details><summary style="cursor:pointer">可用变量</summary><div data-tpl-vars class="x-frac" style="font-size:12px;line-height:1.8;margin-top:6px">预览后显示</div></details>' +
      '<div class="x-tbar" style="margin:0">' + btn("预览", "data-tpl-preview") + '<span class="x-frac" data-tpl-msg style="font-size:12px"></span></div>' +
      '<div data-tpl-out hidden><p style="margin:0 0 6px"><b data-tpl-title></b></p><pre class="x-log" data-tpl-body style="white-space:pre-wrap;max-height:240px;overflow:auto;margin:0"></pre>' +
      '<details style="margin-top:8px"><summary style="cursor:pointer">渠道实际收到的内容</summary><pre class="x-log" data-tpl-payload style="white-space:pre-wrap;max-height:200px;overflow:auto;margin:6px 0 0"></pre></details></div>' +
      '<span class="x-err" id="nc-tpl-err" role="alert" hidden></span></form>';
    var foot = readonly ? '<button class="x-btn" type="button" data-close>关闭</button>'
      : '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="nc-tpl">' + (existing ? "保存" : "创建") + "</button>";
    var m = modal(readonly ? "默认模板 · " + esc(KIND[x.event_kind]) : existing ? "编辑消息模板" : "新建消息模板", html, foot);
    if (!m) return;
    var f = document.getElementById("nc-tpl"), err = document.getElementById("nc-tpl-err");
    var val = function () {
      var e = f.elements;
      return { name: e.name ? e.name.value.trim() : "", description: e.description ? e.description.value.trim() : "", event_kind: e.event_kind.value, channel_type: e.channel_type.value, title_tpl: e.title_tpl.value, body_tpl: e.body_tpl.value };
    };
    var showErr = function (msg, el) { err.textContent = msg; err.hidden = !msg; if (msg && el) el.focus(); };
    var preview = function () {
      var v = val(), msg = f.querySelector("[data-tpl-msg]");
      msg.textContent = "正在渲染示例数据…";
      return api("/observability/notify/templates/preview", { method: "POST", body: { event_kind: v.event_kind, channel_type: v.channel_type || "feishu", title_tpl: v.title_tpl, body_tpl: v.body_tpl } }).then(function (r) {
        msg.textContent = "已用示例数据渲染";
        showErr("");
        f.querySelector("[data-tpl-out]").hidden = false;
        f.querySelector("[data-tpl-title]").textContent = r.title || "";
        f.querySelector("[data-tpl-body]").textContent = r.body || "";
        f.querySelector("[data-tpl-payload]").textContent = r.payload ? JSON.stringify(r.payload, null, 2) : "";
        f.querySelector("[data-tpl-vars]").innerHTML = (r.variables || []).map(function (x2) { return '<code class="x-mono">' + esc(x2.name) + "</code>　" + esc(x2.desc); }).join("<br>");
      }).catch(function (e) { msg.textContent = ""; showErr(errMsg(e)); });
    };
    f.querySelector("[data-tpl-preview]").onclick = preview;
    if (readonly) { preview(); return; }
    f.onsubmit = function (ev) {
      ev.preventDefault();
      var v = val(), e = f.elements;
      if (!v.name) return showErr("请填写模板名称", e.name);
      if (!v.title_tpl.trim()) return showErr("请填写标题模板", e.title_tpl);
      if (!v.body_tpl.trim()) return showErr("请填写正文模板", e.body_tpl);
      var sb = document.querySelector('[form="nc-tpl"][type="submit"]'); if (sb) sb.disabled = true;
      api("/observability/notify/templates" + (existing ? "/" + existing.id : ""), { method: existing ? "PUT" : "POST", body: v }).then(function () {
        if (root.XSChannelForm && root.XSChannelForm.resetTemplates) root.XSChannelForm.resetTemplates();
        closeModal(); toast(existing ? "模板已保存" : "模板已创建"); draw();
      }).catch(function (e2) { showErr(errMsg(e2)); if (sb) sb.disabled = false; });
    };
  }

  /* -------------------------------------------------------------- 发送记录 */

  function drawRecords(seq) {
    var t = tools(btn("刷新", "data-nc-refresh"));
    t.querySelector("[data-nc-refresh]").onclick = draw;
    var r = S.rec, range = RANGES.filter(function (x) { return x.v === r.range; })[0] || RANGES[1];
    var query = { page: r.page, page_size: r.size, since: Math.floor(Date.now() / 1000) - range.s };
    ["channel_id", "event_kind", "status", "q"].forEach(function (k) { if (r[k]) query[k] = r[k]; });
    Promise.all([S.channels.length ? Promise.resolve(S.channels) : loadChannels(), api("/observability/notify/records", { query: query })]).then(function (res) {
      if (!alive(seq)) return;
      var data = res[1] || {}, items = data.items || [], total = data.total || 0;
      var opt = function (cur, pairs) { return pairs.map(function (p) { return '<option value="' + esc(p[0]) + '"' + (String(p[0]) === String(cur) ? " selected" : "") + ">" + esc(p[1]) + "</option>"; }).join(""); };
      var filters = '<form class="x-tbar" data-rec-filter style="gap:8px">' +
        '<select class="x-select" name="range" aria-label="时间范围">' + opt(r.range, RANGES.map(function (x) { return [x.v, x.t]; })) + "</select>" +
        '<select class="x-select" name="channel_id" aria-label="渠道">' + opt(r.channel_id, [["", "全部渠道"]].concat(S.channels.map(function (c) { return [c.id, c.name]; }))) + "</select>" +
        '<select class="x-select" name="event_kind" aria-label="事件类型">' + opt(r.event_kind, [["", "全部事件"]].concat(Object.keys(KIND).map(function (k) { return [k, KIND[k]]; }))) + "</select>" +
        '<select class="x-select" name="status" aria-label="结果">' + opt(r.status, [["", "全部结果"], ["success", "成功"], ["failed", "失败"], ["suppressed", "已抑制"]]) + "</select>" +
        '<input class="x-input" name="q" value="' + esc(r.q) + '" placeholder="标题 / 渠道 / 原因" style="width:200px" autocomplete="off" />' +
        '<button class="x-btn" type="submit">查询</button></form>';
      var rows = items.map(function (x) {
        var st = REC_ST[x.status] || [x.status, "x-grey"];
        var ops = btn("详情", 'data-rec-view="' + x.id + '"') + (x.status !== "success" ? " " + btn("重发", 'data-rec-resend="' + x.id + '"') : "");
        return [fmt(x.created_at), esc(x.channel_name) + '<div class="x-frac" style="font-size:12px">' + esc(TYPE_NAME[x.channel_type] || x.channel_type) + "</div>",
          esc(KIND[x.event_kind] || x.event_kind) + (x.event_status ? " · " + esc(STATUS[x.event_status] || x.event_status) : ""),
          '<span title="' + esc(x.title) + '">' + esc(x.title.length > 48 ? x.title.slice(0, 48) + "…" : x.title) + "</span>",
          esc(x.subscription_name || (x.event_kind === "test" ? "手动测试" : "—")) + (x.resend_of ? '<div class="x-frac" style="font-size:12px">重发自 #' + esc(x.resend_of) + "</div>" : ""),
          tag(st[0], st[1]) + (x.reason ? '<div class="x-frac" style="font-size:12px;max-width:260px;overflow-wrap:anywhere">' + esc(x.reason.length > 80 ? x.reason.slice(0, 80) + "…" : x.reason) + "</div>" : ""),
          x.status === "suppressed" ? "—" : esc(x.duration_ms) + " ms", '<span class="ops-nowrap">' + ops + "</span>"];
      });
      var pages = Math.max(1, Math.ceil(total / r.size));
      var pager = '<div class="x-tbar" style="justify-content:flex-end;margin:12px 0 0"><span class="x-frac">共 ' + total + " 条，第 " + r.page + " / " + pages + " 页</span>" +
        btn("上一页", "data-rec-prev" + (r.page <= 1 ? " disabled" : "")) + btn("下一页", "data-rec-next" + (r.page >= pages ? " disabled" : "")) + "</div>";
      body().innerHTML = '<section class="x-panel">' + filters + table(["时间", "渠道", "事件", "标题", "订阅规则", "结果", "耗时", "操作"], rows, "该条件下没有发送记录。") + pager +
        '<p class="x-frac" style="margin:8px 0 0;font-size:12px">发送记录保留 30 天。「已抑制」表示命中静默时段或限频，未实际发送。</p></section>';
      var b = body(), form = b.querySelector("[data-rec-filter]");
      form.onsubmit = function (ev) {
        ev.preventDefault();
        var e = form.elements;
        S.rec = { page: 1, size: r.size, range: e.range.value, channel_id: e.channel_id.value, event_kind: e.event_kind.value, status: e.status.value, q: e.q.value.trim() };
        draw();
      };
      form.onchange = function (ev) { if (ev.target.tagName === "SELECT") form.onsubmit(ev); };
      b.querySelector("[data-rec-prev]").onclick = function () { S.rec.page = Math.max(1, r.page - 1); draw(); };
      b.querySelector("[data-rec-next]").onclick = function () { S.rec.page = r.page + 1; draw(); };
      [].forEach.call(b.querySelectorAll("[data-rec-view]"), function (x) { x.onclick = function () { viewRecord(x.getAttribute("data-rec-view")); }; });
      [].forEach.call(b.querySelectorAll("[data-rec-resend]"), function (x) { x.onclick = function () { resend(x.getAttribute("data-rec-resend"), x); }; });
    }).catch(function (e) { fail(seq, e); });
  }
  function resend(id, b) {
    confirmDo("重发通知", "将按渠道当前配置重新发送记录 #" + id + " 的内容。", "重发", function () {
      if (b) b.disabled = true;
      api("/observability/notify/records/" + encodeURIComponent(id) + "/resend", { method: "POST", body: {} }).then(function (r) {
        toast(r && r.ok ? "已重新发送" : "重发失败：" + (r && r.error || "对方未接受消息"));
        closeModal(); draw();
      }).catch(function (e) { toast(errMsg(e)); if (b) b.disabled = false; });
    });
  }
  function viewRecord(id) {
    api("/observability/notify/records/" + encodeURIComponent(id)).then(function (x) {
      var st = REC_ST[x.status] || [x.status, "x-grey"];
      var row = function (k, v) { return "<dt>" + esc(k) + "</dt><dd>" + v + "</dd>"; };
      var html = '<div class="x-form"><dl style="display:grid;grid-template-columns:90px minmax(0,1fr);gap:8px 12px;margin:0">' +
        row("时间", esc(fmt(x.created_at))) + row("渠道", esc(x.channel_name) + "（" + esc(TYPE_NAME[x.channel_type] || x.channel_type) + "）") +
        row("事件", esc(KIND[x.event_kind] || x.event_kind) + (x.event_status ? " · " + esc(STATUS[x.event_status] || x.event_status) : "")) +
        row("订阅规则", esc(x.subscription_name || "—")) + row("级别 / 环境", esc([SEV[x.severity] || x.severity, x.env, x.cluster].filter(Boolean).join(" / ") || "—")) +
        row("结果", tag(st[0], st[1]) + (x.status === "suppressed" ? "" : " " + esc(x.duration_ms) + " ms")) +
        (x.reason ? row(x.status === "failed" ? "错误" : "原因", '<span style="overflow-wrap:anywhere">' + esc(x.reason) + "</span>") : "") +
        row("触发", esc(x.triggered_by || "—") + (x.resend_of ? "，重发自 #" + esc(x.resend_of) : "")) +
        "</dl>" + (x.title ? '<p style="margin:12px 0 6px"><b>' + esc(x.title) + "</b></p>" : "") +
        (x.body ? '<pre class="x-log" style="white-space:pre-wrap;max-height:300px;overflow:auto;margin:0">' + esc(x.body) + "</pre>" : "") + "</div>";
      var foot = '<button class="x-btn" type="button" data-close>关闭</button>' + (x.status !== "success" ? '<button class="x-btn x-btn--pri" type="button" data-rec-again>重发</button>' : "");
      var m = modal("发送记录 #" + esc(x.id), html, foot);
      var again = m && m.querySelector("[data-rec-again]");
      if (again) again.onclick = function () { resend(x.id, again); };
    }).catch(function (e) { toast(errMsg(e)); });
  }

  /* -------------------------------------------------------------- 通知安全 */

  function drawSecurity(seq) {
    tools("");
    api("/observability/notify/security").then(function (p) {
      if (!alive(seq)) return;
      body().innerHTML = '<section class="x-panel"><form class="x-form" id="nc-sec" novalidate style="max-width:720px">' +
        '<p class="x-desc" style="margin:0">通知会从平台服务器向外部地址发起请求。为防止被利用访问内网服务，默认禁止向内网、本机和云元数据地址发送；需要使用内网 Webhook 或收件桩时，把它加入白名单。</p>' +
        '<label class="x-channel-enabled" style="display:flex;justify-content:space-between;align-items:center;padding:14px 16px;border-radius:8px;background:#f7f9fc"><span><strong style="font-weight:500">禁止内网地址</strong><small style="display:block;color:var(--x-sub)">拦截 10.0.0.0/8、172.16.0.0/12、192.168.0.0/16、127.0.0.0/8、169.254.0.0/16、100.64.0.0/10 等地址</small></span>' +
        '<input type="checkbox" role="switch" name="block_private"' + (p.block_private ? " checked" : "") + ' style="width:18px;height:18px" /></label>' +
        '<label><span>白名单</span><textarea class="x-input x-mono" name="allow_hosts" rows="6" spellcheck="false" placeholder="每行一项，例如：\n127.0.0.1:2525\nhooks.intranet.example.com\n10.10.0.0/16">' + esc((p.allow_hosts || []).join("\n")) + "</textarea>" +
        '<small class="x-frac">支持主机名、IP、主机:端口、CIDR。白名单内的地址可使用 http 机器人地址，且不受「禁止内网地址」限制。</small></label>' +
        '<span class="x-err" id="nc-sec-err" role="alert" hidden></span><div><button class="x-btn x-btn--pri" type="submit">保存</button></div></form></section>';
      var f = document.getElementById("nc-sec"), e = document.getElementById("nc-sec-err");
      f.onsubmit = function (ev) {
        ev.preventDefault();
        var hosts = f.elements.allow_hosts.value.split(/[\n,，]+/).map(function (s) { return s.trim(); }).filter(Boolean);
        var bad = hosts.filter(function (h) { return /[\s?#@\\]/.test(h) || /^[a-z]+:\/\//i.test(h); });
        if (bad.length) { e.textContent = "白名单项不要包含协议或路径：" + bad.join(", "); e.hidden = false; f.elements.allow_hosts.focus(); return; }
        var go = function () {
          api("/observability/notify/security", { method: "PUT", body: { block_private: f.elements.block_private.checked, allow_hosts: hosts } }).then(function () {
            e.hidden = true; toast("通知安全设置已保存"); draw();
          }).catch(function (err) { e.textContent = errMsg(err); e.hidden = false; });
        };
        if (!f.elements.block_private.checked && p.block_private) confirmDo("关闭内网地址拦截", "关闭后，任何有通知管理权限的人都可以让平台向内网地址发送请求。建议改用白名单。", "仍然关闭", go);
        else go();
      };
    }).catch(function (e) { fail(seq, e); });
  }

  root.XSNotifyCenter = { render: render };
})(window);
