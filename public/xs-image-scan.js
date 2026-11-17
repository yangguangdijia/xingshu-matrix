/* 应用工作台 → 镜像扫描。只读真实接口（server/internal/modules/cicd/scan.go、runs.go）：
 *   后端没有独立的「扫描记录列表 / 触发扫描」接口：扫描由流水线中的镜像扫描阶段执行，
 *   结果随运行详情返回（GET /cicd/runs/:id 的 scans 字段，仅含扫描阶段的运行才有）。
 *   因此本页：
 *     1. GET /cicd/pipelines?app_id=           找出含镜像扫描阶段（type=scan）的流水线
 *     2. GET /cicd/runs?app_id=&page=&page_size= 分页读取运行记录
 *     3. 对属于扫描流水线的运行 GET /cicd/runs/:id，取 scans 汇总成扫描记录
 *   触发扫描：后端没有单独触发接口，不提供按钮（扫描随流水线运行执行）。
 * 挂载：window.XSImageScan.render(el, { appId })
 */
(function (root) {
  "use strict";
  // emptyHTML：统一空状态（XSEmpty）；未加载 xs-empty.js 时回退为原来的一行文字 fallbackHTML。
  function emptyHTML(o, fallbackHTML) {
    var X = root.XSEmpty;
    return typeof X !== "undefined" && X && typeof X.html === "function" ? X.html(o) : fallbackHTML;
  }
  var PAGE_SIZE = 10; // 每页运行记录数（每条需单独读取详情，保持较小）
  var POLL_MS = 8000;
  var SEV = {
    CRITICAL: ["严重", "crit"],
    HIGH: ["高危", "high"],
    MEDIUM: ["中危", "med"],
    LOW: ["低危", "low"],
    UNKNOWN: ["未知", "unk"]
  };
  var SEV_ORDER = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"];
  var SEV_KEY = { CRITICAL: "critical", HIGH: "high", MEDIUM: "medium", LOW: "low", UNKNOWN: "unknown" };
  var STATUS = { passed: ["通过", "ok"], blocked: ["已阻断", "bad"], error: ["扫描失败", "warn"] };
  var SOURCE = { trivy: "Trivy", harbor: "Harbor 镜像仓库" };
  var RUN_ACTIVE = { queued: 1, running: 1, cancelling: 1, pending: 1, waiting_approval: 1 };

  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function X() { return root.XS || {}; }
  function api(path, options) { return X().api(path, options || {}); }
  function errMsg(e) { return (e && e.message) || "服务暂不可用，请稍后重试"; }
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function fmtTime(v) {
    if (!v) return "—";
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }
  function list(d, key) {
    if (Array.isArray(d)) return d;
    if (d && Array.isArray(d.items)) return d.items;
    return d && key && Array.isArray(d[key]) ? d[key] : [];
  }
  function sevOf(s) { s = String(s || "").toUpperCase(); return SEV[s] ? s : "UNKNOWN"; }
  function sevTag(s) { s = sevOf(s); return '<span class="xis-sev xis-sev--' + SEV[s][1] + '">' + esc(SEV[s][0]) + "</span>"; }
  function statusTag(s) { var x = STATUS[s] || [s || "未知", "warn"]; return '<span class="xis-tag xis-tag--' + x[1] + '">' + esc(x[0]) + "</span>"; }
  function safeURL(u) { u = String(u || ""); return /^https?:\/\//i.test(u) ? u : ""; }
  function hasScanStage(pl) { return (pl && Array.isArray(pl.stages) ? pl.stages : []).some(function (s) { return s && s.type === "scan"; }); }

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-image-scan-css")) return;
    var s = doc.createElement("style"); s.id = "xs-image-scan-css";
    s.textContent = [
      ".xis-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin:0 0 12px}.xis-head h2{margin:0;font-size:16px}",
      ".xis-note{font-size:13px;color:var(--x-sub,#6b7280);margin:0 0 14px;line-height:1.6}",
      ".xis-cards{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-bottom:16px}",
      ".xis-card{border:1px solid var(--x-line,#e6e9ed);border-radius:8px;padding:12px 16px;background:#fff}",
      ".xis-card b{display:block;font-size:22px;line-height:30px}.xis-card span{font-size:13px;color:var(--x-sub,#6b7280)}",
      ".xis-card--crit b{color:#b91c1c}.xis-card--high b{color:#c2410c}.xis-card--med b{color:#b45309}.xis-card--low b{color:#1d4ed8}",
      ".xis-table{width:100%;border-collapse:collapse;font-size:14px}",
      ".xis-table th,.xis-table td{padding:10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:middle}",
      ".xis-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xis-mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;word-break:break-all}",
      ".xis-muted{color:var(--x-sub,#6b7280)}",
      ".xis-tag,.xis-sev{display:inline-block;padding:1px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid;white-space:nowrap}",
      ".xis-tag--ok{color:#15803d;background:#f0fdf4;border-color:#bbf7d0}.xis-tag--bad{color:#b91c1c;background:#fef2f2;border-color:#fecaca}.xis-tag--warn{color:#b45309;background:#fffbeb;border-color:#fde68a}",
      ".xis-sev--crit{color:#fff;background:#b91c1c;border-color:#b91c1c}.xis-sev--high{color:#c2410c;background:#fff7ed;border-color:#fed7aa}",
      ".xis-sev--med{color:#b45309;background:#fffbeb;border-color:#fde68a}.xis-sev--low{color:#1d4ed8;background:#eff6ff;border-color:#bfdbfe}.xis-sev--unk{color:#4b5563;background:#f3f4f6;border-color:#e5e7eb}",
      ".xis-counts{display:flex;gap:6px;flex-wrap:wrap}.xis-counts .xis-sev{min-width:44px;text-align:center}",
      ".xis-link{border:0;background:none;padding:0 8px 0 0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:14px;white-space:nowrap}",
      ".xis-err{color:#b91c1c;font-size:13px;margin:4px 0 0;word-break:break-all}.xis-empty{padding:28px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xis-pager{display:flex;align-items:center;justify-content:flex-end;gap:10px;margin-top:12px;font-size:13px;color:var(--x-sub,#6b7280)}",
      ".xis-detail{border:1px solid var(--x-line,#e6e9ed);border-radius:8px;padding:16px 20px;margin-top:16px;background:#fff}",
      ".xis-detail h3{margin:0;font-size:15px}.xis-kv{display:grid;grid-template-columns:110px minmax(0,1fr);gap:10px;padding:7px 0;border-bottom:1px solid var(--x-line,#f0f2f5);font-size:14px}",
      ".xis-kv>span{color:var(--x-sub,#4b5563)}.xis-filter{display:flex;gap:6px;flex-wrap:wrap;margin:14px 0 10px}",
      ".xis-filter button{border:1px solid var(--x-line,#d1d5db);background:#fff;border-radius:4px;padding:3px 10px;font-size:13px;cursor:pointer}",
      ".xis-filter button.on{border-color:var(--x-blue,#2563eb);color:var(--x-blue,#2563eb);background:#eff6ff}",
      ".xis-flag{font-size:12px;margin-left:6px;color:#b91c1c}.xis-flag--ign{color:#6b7280}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  function countsHTML(sum) {
    sum = sum || {};
    return '<span class="xis-counts">' + ["CRITICAL", "HIGH", "MEDIUM", "LOW"].map(function (s) {
      var n = Number(sum[SEV_KEY[s]]) || 0;
      return '<span class="xis-sev xis-sev--' + (n ? SEV[s][1] : "unk") + '" title="' + esc(SEV[s][0]) + '">' + esc(SEV[s][0].slice(0, 1) + " " + n) + "</span>";
    }).join("") + "</span>";
  }
  function policyText(p) {
    p = p || {};
    var parts = [];
    if (p.report_only || !(p.block_severities || []).length) parts.push("仅记录结果，不阻断发布");
    else parts.push("出现" + p.block_severities.map(function (s) { return SEV[sevOf(s)][0]; }).join("、") + "漏洞时阻断发布");
    if (p.ignore_unfixed) parts.push("暂无修复版本的漏洞不阻断");
    if ((p.ignore_cves || []).length) parts.push("已设置 " + p.ignore_cves.length + " 个忽略的漏洞编号");
    return parts.join("；");
  }

  function render(el, opts) {
    if (!el) return;
    css();
    opts = opts || {};
    var appId = opts.appId;
    var serial = (el._xsIscSerial || 0) + 1; el._xsIscSerial = serial;
    var st = { page: 1, totalRuns: 0, pipes: [], scanPipeIds: {}, scans: [], loading: true, error: "", partial: "", hasActive: false, open: null, sev: "" };
    var pollTimer = null;
    function alive() { return el._xsIscSerial === serial && el.isConnected !== false; }

    function load(quiet) {
      if (appId == null || appId === "") { st.loading = false; st.error = "未指定应用"; paint(); return Promise.resolve(); }
      if (!quiet) { st.loading = true; paint(); }
      return api("/cicd/pipelines", { query: { app_id: appId } }).then(function (pd) {
        var pipes = list(pd, "pipelines");
        st.pipes = pipes; st.scanPipeIds = {};
        pipes.forEach(function (pl) { if (hasScanStage(pl)) st.scanPipeIds[String(pl.id)] = pl; });
        if (!Object.keys(st.scanPipeIds).length) return { runs: [], total: 0, details: [] };
        return api("/cicd/runs", { query: { app_id: appId, page: st.page, page_size: PAGE_SIZE } }).then(function (rd) {
          var runs = list(rd, "runs").filter(function (r) { return st.scanPipeIds[String(r.pipeline_id)]; });
          var total = Number(rd && rd.total) || list(rd, "runs").length;
          return Promise.all(runs.map(function (r) {
            return api("/cicd/runs/" + encodeURIComponent(r.id)).then(function (d) { return { ok: true, run: d || r }; }, function (e) { return { ok: false, run: r, error: e }; });
          })).then(function (details) { return { runs: runs, total: total, details: details }; });
        });
      }).then(function (res) {
        if (!alive()) return;
        var scans = [], failed = 0, active = false;
        res.details.forEach(function (d) {
          var run = d.run || {};
          if (RUN_ACTIVE[run.status]) active = true;
          if (!d.ok) { failed++; return; }
          (Array.isArray(run.scans) ? run.scans : []).forEach(function (s) { scans.push({ scan: s, run: run }); });
        });
        scans.sort(function (a, b) { return (Number(b.run.id) || 0) - (Number(a.run.id) || 0) || (Number(b.scan.stage_idx) || 0) - (Number(a.scan.stage_idx) || 0); });
        st.scans = scans; st.totalRuns = res.total; st.hasActive = active;
        st.partial = failed ? failed + " 次运行的扫描结果读取失败" : "";
        st.error = ""; st.loading = false;
        if (st.open && !scans.some(function (x) { return String(x.scan.id) === String(st.open); })) st.open = null;
        paint(); schedulePoll();
      }, function (e) {
        if (!alive()) return;
        st.loading = false; st.error = "读取镜像扫描结果失败：" + errMsg(e); paint();
      });
    }
    function schedulePoll() {
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
      if (!st.hasActive) return;
      pollTimer = setTimeout(function () { pollTimer = null; if (alive()) load(true); }, POLL_MS);
    }

    function pipeName(run) { var pl = st.scanPipeIds[String(run.pipeline_id)]; return pl ? (pl.name || "流水线 #" + pl.id) : "流水线 #" + run.pipeline_id; }
    function stageName(x) {
      var pl = st.scanPipeIds[String(x.run.pipeline_id)], stg = pl && pl.stages ? pl.stages[x.scan.stage_idx] : null;
      return stg && stg.name ? stg.name : "第 " + ((Number(x.scan.stage_idx) || 0) + 1) + " 阶段";
    }

    function cardsHTML() {
      var latest = st.scans.filter(function (x) { return x.scan.status !== "error"; })[0];
      if (!latest) return "";
      var sum = latest.scan.summary || {};
      return '<p class="xis-note" style="margin-bottom:8px">最近一次扫描（运行 #' + esc(latest.run.id) + "，" + esc(fmtTime(latest.scan.scanned_at)) + "）漏洞数量：</p>" +
        '<div class="xis-cards" data-isc-summary>' + ["CRITICAL", "HIGH", "MEDIUM", "LOW"].map(function (s) {
          return '<div class="xis-card xis-card--' + SEV[s][1] + '"><b>' + esc(Number(sum[SEV_KEY[s]]) || 0) + "</b><span>" + esc(SEV[s][0]) + "</span></div>";
        }).join("") + "</div>";
    }
    function rowHTML(x) {
      var s = x.scan, sum = s.summary || {};
      return '<tr data-isc-row="' + esc(s.id) + '">' +
        "<td>#" + esc(x.run.id) + '<div class="xis-muted" style="font-size:12px">' + esc(pipeName(x.run)) + "</div></td>" +
        '<td><span class="xis-mono">' + esc(s.image || "—") + "</span></td>" +
        "<td>" + esc(SOURCE[s.source] || s.source || "—") + "</td>" +
        "<td>" + statusTag(s.status) + "</td>" +
        "<td>" + (s.status === "error" ? '<span class="xis-muted">—</span>' : countsHTML(sum)) + "</td>" +
        "<td>" + (s.status === "error" ? '<span class="xis-muted">—</span>' : esc(Number(sum.blocking) || 0)) + "</td>" +
        "<td>" + esc(fmtTime(s.scanned_at)) + "</td>" +
        '<td><button type="button" class="xis-link" data-isc-click="open" data-id="' + esc(s.id) + '">' + (String(st.open) === String(s.id) ? "收起" : "查看漏洞") + "</button></td></tr>";
    }
    function detailHTML() {
      if (st.open == null) return "";
      var x = st.scans.filter(function (y) { return String(y.scan.id) === String(st.open); })[0];
      if (!x) return "";
      var s = x.scan, sum = s.summary || {}, p = s.policy || {};
      var vulns = Array.isArray(s.vulnerabilities) ? s.vulnerabilities : [];
      var shown = st.sev ? vulns.filter(function (v) { return sevOf(v.severity) === st.sev; }) : vulns;
      var head = '<div class="xis-head"><h3>扫描详情 · 运行 #' + esc(x.run.id) + "</h3>" +
        '<button type="button" class="x-btn" data-isc-click="close">关闭</button></div>' +
        '<div class="xis-kv"><span>镜像</span><div class="xis-mono">' + esc(s.image || "—") + "</div></div>" +
        '<div class="xis-kv"><span>扫描阶段</span><div>' + esc(pipeName(x.run)) + " · " + esc(stageName(x)) + "</div></div>" +
        '<div class="xis-kv"><span>扫描方式</span><div>' + esc(SOURCE[s.source] || s.source || "—") + "</div></div>" +
        '<div class="xis-kv"><span>结果</span><div>' + statusTag(s.status) + "</div></div>" +
        '<div class="xis-kv"><span>门禁策略</span><div data-isc-policy>' + esc(policyText(p)) + "</div></div>";
      if (s.status === "error") {
        return '<section class="xis-detail" data-isc-detail>' + head + '<p class="xis-err" data-isc-scan-error>扫描未完成：' + esc(s.error || "原因未知") + "</p></section>";
      }
      head += '<div class="xis-kv"><span>漏洞统计</span><div>共 ' + esc(Number(sum.total) || 0) + " 个，阻断 " + esc(Number(sum.blocking) || 0) + " 个，已忽略 " + esc(Number(sum.ignored) || 0) + " 个" +
        (Number(sum.unknown) ? "，未知级别 " + esc(sum.unknown) + " 个" : "") + "</div></div>";
      var filter = '<div class="xis-filter">' + [["", "全部"]].concat(SEV_ORDER.map(function (k) { return [k, SEV[k][0]]; })).map(function (f) {
        var n = f[0] ? vulns.filter(function (v) { return sevOf(v.severity) === f[0]; }).length : vulns.length;
        if (f[0] && !n) return "";
        return '<button type="button" data-isc-click="sev" data-sev="' + esc(f[0]) + '"' + (st.sev === f[0] ? ' class="on"' : "") + ">" + esc(f[1]) + "（" + n + "）</button>";
      }).join("") + "</div>";
      var table = shown.length ? '<table class="xis-table" data-isc-vulns><thead><tr><th>漏洞编号</th><th>级别</th><th>组件</th><th>当前版本</th><th>修复版本</th><th>说明</th></tr></thead><tbody>' +
        shown.map(function (v) {
          var url = safeURL(v.url);
          var id = url ? '<a class="xis-mono" href="' + esc(url) + '" target="_blank" rel="noopener noreferrer">' + esc(v.id) + "</a>" : '<span class="xis-mono">' + esc(v.id) + "</span>";
          return "<tr><td>" + id + (v.blocking ? '<span class="xis-flag">阻断</span>' : "") + (v.ignored ? '<span class="xis-flag xis-flag--ign">已忽略</span>' : "") + "</td>" +
            "<td>" + sevTag(v.severity) + "</td>" +
            '<td><span class="xis-mono">' + esc(v.package || "—") + "</span>" + (v.target ? '<div class="xis-muted" style="font-size:12px">' + esc(v.target) + "</div>" : "") + "</td>" +
            '<td><span class="xis-mono">' + esc(v.installed || "—") + "</span></td>" +
            "<td>" + (v.fixed ? '<span class="xis-mono">' + esc(v.fixed) + "</span>" : '<span class="xis-muted">暂无修复</span>') + "</td>" +
            "<td>" + esc(v.title || "—") + "</td></tr>";
        }).join("") + "</tbody></table>" : '<p class="xis-empty">' + (vulns.length ? "该级别没有漏洞" : "未发现漏洞") + "</p>";
      var more = Number(sum.total) > vulns.length ? '<p class="xis-note" style="margin-top:8px">共发现 ' + esc(sum.total) + " 个漏洞，仅保存最需关注的前 " + esc(vulns.length) + " 个（阻断优先、按级别排序）。</p>" : "";
      return '<section class="xis-detail" data-isc-detail>' + head + filter + table + more + "</section>";
    }
    function paint() {
      if (!alive()) return;
      var head = '<div class="xis-head"><h2>镜像扫描</h2><button type="button" class="x-btn" data-isc-click="reload">刷新</button></div>' +
        '<p class="xis-note">镜像扫描在流水线的「镜像扫描」阶段自动执行，检查构建出的镜像中的已知安全漏洞，并按门禁策略决定是否允许继续发布。运行包含扫描阶段的流水线即可产生新的扫描结果。</p>';
      var body;
      if (st.loading && !st.scans.length) body = '<p class="xis-empty">正在加载…</p>';
      else if (st.error) body = '<p class="xis-err" data-isc-error>' + esc(st.error) + "</p>";
      else if (!Object.keys(st.scanPipeIds).length) {
        var firstPipe = st.pipes && st.pipes[0];
        body = emptyHTML({ kind: "scan", title: "该应用的流水线尚未配置镜像扫描阶段", desc: "可在流水线编辑中添加「镜像扫描」阶段后运行，构建出的镜像会自动检查已知漏洞。", attrs: { "data-isc-noscan": "" },
          actions: [{ text: firstPipe ? "前往流水线添加扫描阶段" : "前往流水线", href: firstPipe ? "#/cicd/pipeline/" + encodeURIComponent(firstPipe.id) : "#/cicd/pipeline", primary: true }] },
          '<p class="xis-empty" data-isc-noscan>该应用的流水线尚未配置镜像扫描阶段。可在流水线编辑中添加「镜像扫描」阶段后运行。</p>');
      }
      else {
        var pages = Math.max(1, Math.ceil(st.totalRuns / PAGE_SIZE));
        body = cardsHTML() + (st.partial ? '<p class="xis-err" role="alert">' + esc(st.partial) + "</p>" : "") +
          (st.scans.length ? '<table class="xis-table"><thead><tr><th>运行</th><th>镜像</th><th>扫描方式</th><th>结果</th><th>严重 / 高 / 中 / 低</th><th>阻断</th><th>扫描时间</th><th>操作</th></tr></thead><tbody>' +
            st.scans.map(rowHTML).join("") + "</tbody></table>" : emptyHTML({ kind: "scan", size: "s", title: "本页的运行记录中没有扫描结果", desc: st.hasActive ? "扫描进行中的运行完成后会自动刷新。" : "运行包含扫描阶段的流水线即可产生新的扫描结果。", actions: [{ text: "刷新", act: "reload", attrs: { "data-isc-click": "reload" } }] },
              '<p class="xis-empty">本页的运行记录中没有扫描结果' + (st.hasActive ? "，扫描进行中的运行完成后会自动刷新" : "") + "</p>")) +
          '<div class="xis-pager"><span>按运行记录分页，共 ' + esc(st.totalRuns) + " 次运行</span>" +
          '<button type="button" class="x-btn" data-isc-click="prev"' + (st.page <= 1 ? " disabled" : "") + ">上一页</button>" +
          "<span>" + st.page + " / " + pages + "</span>" +
          '<button type="button" class="x-btn" data-isc-click="next"' + (st.page >= pages ? " disabled" : "") + ">下一页</button></div>" +
          detailHTML();
      }
      el.innerHTML = '<div class="xis" data-isc>' + head + body + "</div>";
    }

    var actions = {
      reload: function () { return load(); },
      prev: function () { if (st.page > 1) { st.page--; st.open = null; return load(); } },
      next: function () { st.page++; st.open = null; return load(); },
      open: function (t) { var id = t.getAttribute("data-id"); st.open = String(st.open) === String(id) ? null : id; st.sev = ""; paint(); },
      close: function () { st.open = null; paint(); },
      sev: function (t) { st.sev = t.getAttribute("data-sev") || ""; paint(); }
    };
    if (el._xsIscClick) el.removeEventListener("click", el._xsIscClick);
    el._xsIscClick = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-isc-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-isc-click")];
      if (fn) return fn(t);
    };
    el.addEventListener("click", el._xsIscClick);
    load();
  }

  root.XSImageScan = { render: render, policyText: policyText, SEV: SEV };
})(window);
