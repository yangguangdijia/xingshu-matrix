/* 系统设置 → 后台任务。读写真实接口（server/internal/jobs/api.go）：
 *   列表   GET    /jobs?status=a,b&type=&page=&page_size=   （setting:read）
 *   详情   GET    /jobs/:id                                  （setting:read，参数已由后端脱敏）
 *   重试   POST   /jobs/:id/retry                            （setting:manage，仅失败 / 死信）
 *   删除   DELETE /jobs/:id                                  （setting:manage，仅死信 / 成功，需输入任务编号确认）
 * 挂载：window.XSJobs.render(el)
 */
(function (root) {
  "use strict";
  var PAGE_SIZE = 20;
  var POLL_MS = 5000;
  var STATUS = {
    queued: ["排队中", "run"],
    running: ["运行中", "run"],
    failed: ["失败", "warn"],
    dead: ["死信", "bad"],
    succeeded: ["成功", "ok"]
  };
  var STATUS_ORDER = ["queued", "running", "failed", "dead", "succeeded"];
  var FILTERS = [["", "全部状态"], ["failed,dead", "需处理（失败与死信）"]].concat(STATUS_ORDER.map(function (s) { return [s, STATUS[s][0]]; }));
  var SECRET_KEY = /password|passwd|secret|token|private_key|privatekey|access_key|accesskey|api_key|apikey|credential|kubeconfig/i;

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function api(path, options) { return X().api(path, options || {}); }
  function can(p) { var x = X(); return !!(x.can && x.can(p)); }
  function toast(msg) { var x = X(); if (x.toast) x.toast(msg); else if (typeof root.toast === "function") root.toast(msg); }
  function errMsg(e) { return (e && e.message) || "服务暂不可用，请稍后重试"; }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(v) {
    if (!v) return "—";
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }
  function short(s, n) { s = String(s || ""); return s.length > n ? s.slice(0, n) + "…" : s; }
  function canRetry(j) { return j && (j.status === "failed" || j.status === "dead"); }
  function canDelete(j) { return j && (j.status === "dead" || j.status === "succeeded"); }
  // 后端已脱敏；前端再兜底一次，避免展示任何疑似凭据。
  function redact(v) {
    if (Array.isArray(v)) return v.map(redact);
    if (v && typeof v === "object") {
      var out = {};
      Object.keys(v).forEach(function (k) { out[k] = SECRET_KEY.test(k) ? "***" : redact(v[k]); });
      return out;
    }
    return v;
  }
  function payloadText(p) {
    if (p == null || p === "") return "（无参数）";
    if (typeof p === "string") { try { p = JSON.parse(p); } catch (e) { return p === "***" ? "（参数无法解析，已隐藏）" : "（参数已隐藏）"; } }
    if (p === "***") return "（参数无法解析，已隐藏）";
    if (p && typeof p === "object" && !Array.isArray(p) && !Object.keys(p).length) return "（无参数）";
    return JSON.stringify(redact(p), null, 2);
  }
  // 下次执行：只有排队中 / 失败（等待重试）的任务有意义。
  function nextRun(j) { return j.status === "queued" || j.status === "failed" ? fmtTime(j.run_at) : "—"; }

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-jobs-css")) return;
    var s = doc.createElement("style"); s.id = "xs-jobs-css";
    s.textContent = [
      ".xjb-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:24px 28px;margin-bottom:20px}",
      ".xjb-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:16px}.xjb-head h3{margin:0;font-size:16px}",
      ".xjb-filters{display:flex;flex-wrap:wrap;align-items:center;gap:10px;margin-bottom:14px;font-size:13px;color:var(--x-sub,#4b5563)}",
      ".xjb-filters .x-select,.xjb-filters .x-input{height:32px}.xjb-filters .x-input{width:220px}",
      ".xjb-table{width:100%;border-collapse:collapse;font-size:14px}",
      ".xjb-table th,.xjb-table td{padding:11px 10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:middle}",
      ".xjb-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xjb-mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}",
      ".xjb-muted{color:var(--x-sub,#6b7280)}.xjb-errtxt{color:#b91c1c;font-size:13px;max-width:280px;word-break:break-all}",
      ".xjb-tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid;white-space:nowrap}",
      ".xjb-tag--ok{color:#15803d;background:#f0fdf4;border-color:#bbf7d0}.xjb-tag--bad{color:#b91c1c;background:#fef2f2;border-color:#fecaca}",
      ".xjb-tag--warn{color:#b45309;background:#fffbeb;border-color:#fde68a}.xjb-tag--run{color:#1d4ed8;background:#eff6ff;border-color:#bfdbfe}",
      ".xjb-err{color:#b91c1c;font-size:13px;margin:4px 0 0}.xjb-empty{padding:28px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xjb-link{border:0;background:none;padding:0 8px 0 0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:14px;white-space:nowrap}",
      ".xjb-link[disabled]{color:#9ca3af;cursor:not-allowed}.xjb-link--danger{color:#b91c1c}",
      ".xjb-pager{display:flex;align-items:center;justify-content:flex-end;gap:10px;margin-top:12px;font-size:13px;color:var(--x-sub,#6b7280)}",
      ".xjb-drawer-mask{position:fixed;inset:0;background:rgba(15,23,42,.35);z-index:1000}",
      ".xjb-drawer{position:fixed;top:0;right:0;bottom:0;width:560px;max-width:100vw;background:#fff;box-shadow:-4px 0 16px rgba(15,23,42,.12);z-index:1001;display:flex;flex-direction:column}",
      ".xjb-drawer header{display:flex;align-items:center;justify-content:space-between;padding:18px 24px;border-bottom:1px solid var(--x-line,#e6e9ed)}",
      ".xjb-drawer header h3{margin:0;font-size:16px}.xjb-drawer .xjb-body{padding:16px 24px;overflow:auto;flex:1}",
      ".xjb-drawer footer{display:flex;justify-content:flex-end;gap:10px;padding:14px 24px;border-top:1px solid var(--x-line,#e6e9ed)}",
      ".xjb-row{display:grid;grid-template-columns:110px minmax(0,1fr);gap:12px;padding:8px 0;border-bottom:1px solid var(--x-line,#f0f2f5);font-size:14px}",
      ".xjb-row>span{color:var(--x-sub,#4b5563)}",
      ".xjb-sub{margin:18px 0 8px;font-size:14px;font-weight:600}",
      ".xjb-pre{background:#f8fafc;border:1px solid var(--x-line,#e6e9ed);border-radius:6px;padding:10px 12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;white-space:pre-wrap;word-break:break-all;max-height:320px;overflow:auto;margin:0}",
      ".xjb-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:center;justify-content:center;z-index:1100}",
      ".xjb-modal{background:#fff;border-radius:10px;width:460px;max-width:calc(100vw - 32px);padding:24px}",
      ".xjb-modal h3{margin:0 0 12px;font-size:16px}.xjb-warn{background:#fef2f2;border:1px solid #fecaca;color:#991b1b;border-radius:6px;padding:10px 12px;font-size:13px;line-height:1.6;margin-bottom:14px}",
      ".xjb-modal label{display:block;font-size:13px;color:var(--x-sub,#4b5563);margin-bottom:6px}.xjb-modal .x-input{width:100%;height:36px}",
      ".xjb-foot{display:flex;justify-content:flex-end;gap:10px;margin-top:18px}",
      ".xjb .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}.xjb .x-btn--danger:hover{background:#b92424}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  function statusTag(j) {
    var s = STATUS[j.status] || [j.status || "未知", "run"];
    var title = j.status === "failed" ? "执行失败，将在下次执行时间自动重试" : j.status === "dead" ? "重试次数用尽或无法重试，已停止执行" : "";
    return '<span class="xjb-tag xjb-tag--' + s[1] + '"' + (title ? ' title="' + esc(title) + '"' : "") + ">" + esc(s[0]) + "</span>";
  }

  function render(el) {
    if (!el) return;
    css();
    var serial = (el._xsJobsSerial || 0) + 1; el._xsJobsSerial = serial;
    var st = { page: 1, status: "", type: "", items: [], total: 0, loading: true, error: "", busy: {}, drawer: null, del: null };
    var pollTimer = null;
    var canManage = can("setting:manage");
    function alive() { return el._xsJobsSerial === serial && el.isConnected !== false; }

    function load(quiet) {
      if (!quiet) { st.loading = true; paint(); }
      var q = { page: st.page, page_size: PAGE_SIZE };
      if (st.status) q.status = st.status;
      if (st.type) q.type = st.type;
      return api("/jobs", { query: q }).then(function (d) {
        if (!alive()) return;
        d = d || {};
        st.items = Array.isArray(d) ? d : (Array.isArray(d.items) ? d.items : []);
        st.total = Number(d.total) || st.items.length;
        if (!st.items.length && st.page > 1 && st.total > 0) { st.page = Math.max(1, Math.ceil(st.total / PAGE_SIZE)); return load(quiet); }
        st.error = ""; st.loading = false;
        paint(); schedulePoll();
      }, function (e) {
        if (!alive()) return;
        st.loading = false; st.error = errMsg(e); paint();
      });
    }
    function schedulePoll() {
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
      if (!st.items.some(function (j) { return j.status === "queued" || j.status === "running"; })) return;
      pollTimer = setTimeout(function () { pollTimer = null; if (alive() && !st.del) load(true); else if (alive()) schedulePoll(); }, POLL_MS);
    }

    function filtersHTML() {
      return '<div class="xjb-filters">' +
        '<label>状态 <select class="x-select" data-jobs-status aria-label="按状态筛选">' + FILTERS.map(function (f) {
          return '<option value="' + esc(f[0]) + '"' + (f[0] === st.status ? " selected" : "") + ">" + esc(f[1]) + "</option>";
        }).join("") + "</select></label>" +
        '<label>类型 <input class="x-input" data-jobs-type maxlength="128" placeholder="输入任务类型精确查找" value="' + esc(st.type) + '"></label>' +
        '<button type="button" class="x-btn" data-jobs-click="search">查询</button>' +
        (st.status || st.type ? '<button type="button" class="x-btn" data-jobs-click="clear">清除筛选</button>' : "") +
        "</div>";
    }
    function rowHTML(j) {
      var acts = '<button type="button" class="xjb-link" data-jobs-click="open" data-id="' + esc(j.id) + '">详情</button>';
      if (canManage && canRetry(j)) acts += '<button type="button" class="xjb-link" data-jobs-click="retry" data-id="' + esc(j.id) + '"' + (st.busy[j.id] ? " disabled" : "") + ">" + (st.busy[j.id] ? "提交中…" : "重试") + "</button>";
      if (canManage && canDelete(j)) acts += '<button type="button" class="xjb-link xjb-link--danger" data-jobs-click="del" data-id="' + esc(j.id) + '">删除</button>';
      return '<tr data-jobs-row="' + esc(j.id) + '">' +
        '<td class="xjb-mono">#' + esc(j.id) + "</td>" +
        '<td class="xjb-mono">' + esc(j.type || "—") + "</td>" +
        "<td>" + statusTag(j) + "</td>" +
        "<td>" + esc((Number(j.attempts) || 0) + " / " + (Number(j.max_attempts) || 0)) + "</td>" +
        "<td>" + esc(nextRun(j)) + "</td>" +
        "<td>" + (j.last_error ? '<span class="xjb-errtxt" title="' + esc(short(j.last_error, 500)) + '">' + esc(short(j.last_error, 60)) + "</span>" : '<span class="xjb-muted">—</span>') + "</td>" +
        "<td>" + esc(fmtTime(j.created_at)) + "</td>" +
        "<td>" + acts + "</td></tr>";
    }
    function listHTML() {
      var head = '<div class="xjb-head"><h3>后台任务</h3><button type="button" class="x-btn" data-jobs-click="reload">刷新</button></div>' +
        '<p class="xjb-muted" style="margin:-8px 0 14px;font-size:13px">平台在后台异步执行的任务。失败的任务会按间隔自动重试，重试次数用尽后进入死信，需人工处理。</p>' + filtersHTML();
      if (st.loading && !st.items.length) return '<section class="xjb-card">' + head + '<p class="xjb-empty">正在加载…</p></section>';
      if (st.error) return '<section class="xjb-card">' + head + '<p class="xjb-err" data-jobs-error>' + esc(st.error) + "</p></section>";
      if (!st.items.length) return '<section class="xjb-card">' + head + '<p class="xjb-empty">' + (st.status || st.type ? "没有符合筛选条件的任务" : "暂无后台任务") + "</p></section>";
      var pages = Math.max(1, Math.ceil(st.total / PAGE_SIZE));
      return '<section class="xjb-card">' + head +
        '<table class="xjb-table"><thead><tr><th>编号</th><th>类型</th><th>状态</th><th>尝试次数</th><th>下次执行</th><th>最近错误</th><th>创建时间</th><th style="width:140px">操作</th></tr></thead><tbody>' +
        st.items.map(rowHTML).join("") + "</tbody></table>" +
        '<div class="xjb-pager"><span>共 ' + esc(st.total) + " 条</span>" +
        '<button type="button" class="x-btn" data-jobs-click="prev"' + (st.page <= 1 ? " disabled" : "") + ">上一页</button>" +
        "<span>" + st.page + " / " + pages + "</span>" +
        '<button type="button" class="x-btn" data-jobs-click="next"' + (st.page >= pages ? " disabled" : "") + ">下一页</button></div></section>";
    }
    function kv(k, v) { return '<div class="xjb-row"><span>' + esc(k) + "</span><div>" + v + "</div></div>"; }
    function drawerHTML() {
      var d = st.drawer;
      if (!d) return "";
      var j = d.job, body;
      if (d.loading && !j) body = '<p class="xjb-empty">正在加载…</p>';
      else if (d.error && !j) body = '<p class="xjb-err" data-jobs-drawer-error>' + esc(d.error) + "</p>";
      else {
        body = kv("编号", '<span class="xjb-mono">#' + esc(j.id) + "</span>") +
          kv("类型", '<span class="xjb-mono">' + esc(j.type || "—") + "</span>") +
          kv("状态", statusTag(j)) +
          kv("尝试次数", esc((Number(j.attempts) || 0) + " / " + (Number(j.max_attempts) || 0))) +
          kv("优先级", esc(j.priority == null ? "—" : j.priority)) +
          kv("下次执行", esc(nextRun(j))) +
          kv("执行节点", j.locked_by ? '<span class="xjb-mono">' + esc(j.locked_by) + "</span>" + (j.locked_until ? '<span class="xjb-muted">（占用至 ' + esc(fmtTime(j.locked_until)) + "）</span>" : "") : '<span class="xjb-muted">—</span>') +
          kv("发起人", j.created_by ? "用户 #" + esc(j.created_by) : "系统") +
          kv("创建时间", esc(fmtTime(j.created_at))) +
          kv("最近开始", esc(fmtTime(j.started_at))) +
          kv("最近结束", esc(fmtTime(j.finished_at))) +
          kv("更新时间", esc(fmtTime(j.updated_at))) +
          '<p class="xjb-sub">任务参数</p><p class="xjb-muted" style="font-size:12px;margin:0 0 6px">密码、令牌、密钥等敏感内容已隐藏。</p>' +
          '<pre class="xjb-pre" data-jobs-payload>' + esc(payloadText(j.payload)) + "</pre>" +
          '<p class="xjb-sub">最近错误</p>' + (j.last_error ? '<pre class="xjb-pre" data-jobs-lasterror>' + esc(j.last_error) + "</pre>" : '<p class="xjb-muted">无</p>') +
          (d.error ? '<p class="xjb-err">' + esc(d.error) + "</p>" : "");
      }
      var foot = "";
      if (j && canManage) {
        if (canRetry(j)) foot += '<button type="button" class="x-btn x-btn--pri" data-jobs-click="retry" data-id="' + esc(j.id) + '"' + (st.busy[j.id] ? " disabled" : "") + ">" + (st.busy[j.id] ? "提交中…" : "重试") + "</button>";
        if (canDelete(j)) foot += '<button type="button" class="x-btn x-btn--danger" data-jobs-click="del" data-id="' + esc(j.id) + '">删除</button>';
      }
      return '<div class="xjb-drawer-mask" data-jobs-click="closeDrawer"></div><aside class="xjb-drawer" role="dialog" aria-modal="true" aria-label="任务详情" data-jobs-drawer>' +
        '<header><h3>任务详情</h3><button type="button" class="x-btn" data-jobs-click="closeDrawer">关闭</button></header>' +
        '<div class="xjb-body">' + body + "</div>" + (foot ? "<footer>" + foot + "</footer>" : "") + "</aside>";
    }
    function deleteHTML() {
      var m = st.del;
      if (!m) return "";
      var j = m.job;
      return '<div class="xjb-mask" data-jobs-mask><div class="xjb-modal" role="dialog" aria-modal="true"><h3>删除后台任务</h3>' +
        '<div class="xjb-warn">删除后该任务的记录（参数、错误信息、执行次数）将无法恢复' + (j.status === "dead" ? "，任务也不会再执行" : "") + "。</div>" +
        kv("任务", '<span class="xjb-mono">#' + esc(j.id) + " · " + esc(j.type || "—") + "</span>") +
        kv("状态", statusTag(j)) +
        '<label for="xjb-confirm" style="margin-top:14px">请输入任务编号 <b>' + esc(j.id) + "</b> 以确认删除</label>" +
        '<input id="xjb-confirm" class="x-input" name="confirm" autocomplete="off" data-jobs-confirm value="' + esc(m.text || "") + '">' +
        (m.error ? '<p class="xjb-err" data-jobs-del-error>' + esc(m.error) + "</p>" : "") +
        '<div class="xjb-foot"><button type="button" class="x-btn" data-jobs-click="cancelDel"' + (m.busy ? " disabled" : "") + ">取消</button>" +
        '<button type="button" class="x-btn x-btn--danger" data-jobs-click="confirmDel"' + (m.busy ? " disabled" : "") + ">" + (m.busy ? "正在删除…" : "确认删除") + "</button></div></div></div>";
    }

    function paint() {
      if (!alive()) return;
      el.innerHTML = '<div class="xjb" data-jobs>' + listHTML() + drawerHTML() + deleteHTML() + "</div>";
    }
    function find(id) {
      var hit = st.items.filter(function (j) { return String(j.id) === String(id); })[0];
      if (!hit && st.drawer && st.drawer.job && String(st.drawer.job.id) === String(id)) hit = st.drawer.job;
      return hit;
    }
    function replace(job) {
      if (!job) return;
      st.items = st.items.map(function (j) { return String(j.id) === String(job.id) ? job : j; });
      if (st.drawer && st.drawer.job && String(st.drawer.job.id) === String(job.id)) st.drawer.job = job;
    }
    function readFilters() {
      var sel = el.querySelector("[data-jobs-status]"), inp = el.querySelector("[data-jobs-type]");
      if (sel) st.status = String(sel.value || "");
      if (inp) st.type = String(inp.value || "").trim();
    }

    var actions = {
      reload: function () { return load(); },
      search: function () { readFilters(); st.page = 1; return load(); },
      clear: function () { st.status = ""; st.type = ""; st.page = 1; return load(); },
      prev: function () { if (st.page > 1) { st.page--; return load(); } },
      next: function () { st.page++; return load(); },
      open: function (t) {
        var id = t.getAttribute("data-id");
        st.drawer = { id: id, job: null, loading: true, error: "" }; paint();
        return api("/jobs/" + encodeURIComponent(id)).then(function (j) {
          if (!alive() || !st.drawer || st.drawer.id !== id) return;
          st.drawer.job = j || null; st.drawer.loading = false; if (j) replace(j); paint();
        }, function (e) {
          if (!alive() || !st.drawer || st.drawer.id !== id) return;
          st.drawer.loading = false; st.drawer.error = "读取任务详情失败：" + errMsg(e); paint();
        });
      },
      closeDrawer: function () { if (st.drawer) { st.drawer = null; paint(); } },
      retry: function (t) {
        var j = find(t.getAttribute("data-id"));
        if (!canManage || !canRetry(j) || st.busy[j.id]) return;
        st.busy[j.id] = true; paint();
        return api("/jobs/" + encodeURIComponent(j.id) + "/retry", { method: "POST" }).then(function (nj) {
          delete st.busy[j.id];
          if (!alive()) return;
          replace(nj); toast("任务 #" + j.id + " 已重新排队");
          return load(true);
        }, function (e) {
          delete st.busy[j.id];
          if (!alive()) return;
          paint(); toast("重试失败：" + errMsg(e));
        });
      },
      del: function (t) {
        var j = find(t.getAttribute("data-id"));
        if (!canManage || !canDelete(j)) return;
        st.del = { job: j, text: "", error: "", busy: false }; paint();
      },
      cancelDel: function () { if (st.del && !st.del.busy) { st.del = null; paint(); } },
      confirmDel: function () {
        var m = st.del;
        if (!m || m.busy) return;
        var input = el.querySelector("[data-jobs-confirm]");
        m.text = input ? String(input.value || "").trim().replace(/^#/, "") : "";
        if (m.text !== String(m.job.id)) { m.error = "请输入任务编号「" + m.job.id + "」确认删除"; paint(); return; }
        m.busy = true; m.error = ""; paint();
        return api("/jobs/" + encodeURIComponent(m.job.id), { method: "DELETE" }).then(function () {
          if (!alive()) return;
          var id = m.job.id;
          st.del = null;
          if (st.drawer && st.drawer.job && String(st.drawer.job.id) === String(id)) st.drawer = null;
          toast("任务 #" + id + " 已删除");
          return load(true);
        }, function (e) {
          if (!alive()) return;
          m.busy = false; m.error = "删除失败：" + errMsg(e); paint();
        });
      }
    };

    if (el._xsJobsClick) el.removeEventListener("click", el._xsJobsClick);
    if (el._xsJobsChange) el.removeEventListener("change", el._xsJobsChange);
    if (el._xsJobsKey) el.removeEventListener("keydown", el._xsJobsKey);
    el._xsJobsClick = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-jobs-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-jobs-click")];
      if (fn) return fn(t);
    };
    el._xsJobsChange = function (ev) {
      if (!alive()) return;
      var t = ev.target;
      if (t && t.matches && t.matches("[data-jobs-status]")) return actions.search();
    };
    el._xsJobsKey = function (ev) {
      if (!alive()) return;
      var t = ev.target;
      if (ev.key === "Enter" && t && t.matches && t.matches("[data-jobs-type]")) { if (ev.preventDefault) ev.preventDefault(); return actions.search(); }
      if (ev.key === "Escape" && st.drawer && !st.del) return actions.closeDrawer();
    };
    el.addEventListener("click", el._xsJobsClick);
    el.addEventListener("change", el._xsJobsChange);
    el.addEventListener("keydown", el._xsJobsKey);
    load();
  }

  root.XSJobs = { render: render, payloadText: payloadText, STATUS: STATUS };
})(window);
