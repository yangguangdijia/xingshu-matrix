/* Standalone, API-backed CI/CD pipeline workbench. The shell owns loading this file. */
(function () {
  "use strict";

  var root = window;
  var instances = new WeakMap();
  var FIELD_FOR = { image: "build test script", registry_secret: "build", deploy_env: "deploy", jenkins_id: "jenkins", jenkins_job: "jenkins",
    commands: "test script", env: "build test script", params: "jenkins" };
  var ACTIVE = { queued: true, pending: true, running: true, cancelling: true };
  var LABEL = {
    queued: "排队中", pending: "等待中", running: "运行中", cancelling: "取消中",
    succeeded: "成功", failed: "失败", cancelled: "已取消", skipped: "已跳过",
    awaiting_approval: "等待审批", approved: "已审批", rejected: "已拒绝"
  };

  function escapeHTML(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function array(data, keys) {
    if (Array.isArray(data)) return data;
    for (var i = 0; data && i < keys.length; i++) if (Array.isArray(data[keys[i]])) return data[keys[i]];
    return [];
  }
  function val(value) { return value == null || value === "" ? "—" : escapeHTML(value); }
  function date(value) {
    if (!value) return "—";
    var d = new Date(value);
    return isNaN(d.getTime()) ? val(value) : escapeHTML(d.toLocaleString("zh-CN", { hour12: false }));
  }
  function duration(start, end, now) {
    if (!start) return "—";
    var a = new Date(start).getTime(), b = end ? new Date(end).getTime() : (now == null ? Date.now() : now);
    if (!isFinite(a) || !isFinite(b) || b < a) return "—";
    var s = Math.floor((b - a) / 1000);
    return s >= 3600 ? Math.floor(s / 3600) + "小时 " + Math.floor(s % 3600 / 60) + "分" :
      s >= 60 ? Math.floor(s / 60) + "分 " + s % 60 + "秒" : s + "秒";
  }
  function status(value) {
    var raw = String(value || "unknown");
    var kind = /^(succeeded|approved)$/.test(raw) ? "ok" :
      /^(failed|rejected)$/.test(raw) ? "bad" :
      /^(queued|pending|running|cancelling|awaiting_approval)$/.test(raw) ? "busy" : "neutral";
    return '<span class="x-cicd-state x-cicd-state--' + kind + '">' + escapeHTML(LABEL[raw] || raw) + "</span>";
  }
  function route(hash) {
    var match = /^#\/cicd\/pipeline(?:\/([^/?#]+))?(?:\?.*)?$/.exec(String(hash || ""));
    if (!match) return null;
    try { return { id: match[1] ? decodeURIComponent(match[1]) : "" }; }
    catch (_) { return null; }
  }
  function message(err) { return err && err.message ? String(err.message) : "请求失败，请稍后重试"; }
  function record(label, value, cls) {
    return '<div class="x-cicd-fact"><dt>' + escapeHTML(label) + '</dt><dd' + (cls ? ' class="' + cls + '"' : "") + '>' + value + "</dd></div>";
  }
  function itemById(list, id) { return list.find(function (item) { return String(item.id) === String(id); }); }
  function nameOf(list, id) { var item = itemById(list, id); return item ? val(item.name) : (id ? "#" + escapeHTML(id) : "—"); }
  function stageName(stage, index) { return stage.name || "阶段 " + (index + 1); }
  function fields(map) {
    if (!map || typeof map !== "object") return "—";
    var names = Object.keys(map);
    return names.length ? names.map(function (key) {
      return '<span class="x-cicd-pair"><code>' + escapeHTML(key) + '</code> = <code>' + escapeHTML(map[key]) + "</code></span>";
    }).join("") : "—";
  }
  function currentConfig(pipeline) {
    var stages = Array.isArray(pipeline.stages) ? pipeline.stages : [];
    return '<section class="x-cicd-section" aria-label="当前流水线配置"><h2>当前流水线配置</h2>' +
      '<p class="x-cicd-note">以下是当前流水线定义。历史运行未保存阶段配置快照，不能据此判断历史运行使用的镜像、命令或参数。</p>' +
      (stages.length ? '<div class="x-cicd-config-list">' + stages.map(function (s, index) {
        return '<details class="x-cicd-config"><summary><span class="x-cicd-mono">' + (index + 1) + '</span> ' + escapeHTML(stageName(s, index)) +
          ' <span class="x-cicd-muted">' + val(s.type) + '</span></summary><dl class="x-cicd-grid">' +
          record("类型", val(s.type)) + record("镜像", val(s.image), "x-cicd-break") +
          record("命令", Array.isArray(s.commands) && s.commands.length ? '<pre class="x-cicd-code">' + escapeHTML(s.commands.join("\n")) + "</pre>" : "—") +
          record("环境变量", fields(s.env)) + record("镜像仓库 Secret", val(s.registry_secret)) +
          record("部署环境", val(s.deploy_env)) + record("Jenkins ID", val(s.jenkins_id)) +
          record("Jenkins Job", val(s.jenkins_job), "x-cicd-break") + record("Jenkins 参数", fields(s.params)) +
          record("超时", s.timeout_sec ? escapeHTML(s.timeout_sec) + " 秒" : "—") +
          record("构建参数", Array.isArray(s.extra_args) && s.extra_args.length ? escapeHTML(s.extra_args.join(" · ")) : "—") +
          "</dl></details>";
      }).join("") + "</div>" : '<p class="x-cicd-empty">尚未配置阶段。</p>') + "</section>";
  }
  function runSummary(run) {
    var act = ACTIVE[run.status] ? '<button class="x-btn" type="button" data-action="cancel-run" data-id="' + escapeHTML(run.id) + '">取消运行</button>' :
      '<button class="x-btn" type="button" data-action="rerun" data-id="' + escapeHTML(run.id) + '">重新运行</button>';
    return '<section class="x-cicd-section" aria-label="运行摘要"><div class="x-cicd-section-head"><h2>运行 #' + escapeHTML(run.id) + " " + status(run.status) + '</h2><span class="x-cicd-actions">' + act + '</span></div><dl class="x-cicd-grid x-cicd-grid--summary">' +
      record("运行 ID", '<span class="x-cicd-mono">#' + escapeHTML(run.id) + "</span>") + record("状态", status(run.status)) +
      record("触发方式", triggerText(run.trigger)) + record("触发人", val(run.triggered_by || run.triggered_by_id)) +
      record("分支", val(run.branch), "x-cicd-break") + record("完整 Commit", val(run.commit), "x-cicd-mono x-cicd-break") +
      record("镜像标签", val(run.image_tag), "x-cicd-mono x-cicd-break") + record("创建时间", date(run.created_at)) +
      record("开始时间", date(run.started_at)) + record("结束时间", date(run.finished_at)) +
      record("运行耗时", run.started_at ? escapeHTML(duration(run.started_at, run.finished_at)) : "—") +
      (run.error ? record("运行错误", val(run.error), "x-cicd-error x-cicd-break") : "") +
      "</dl></section>";
  }
  function runStages(run, current) {
    var stages = Array.isArray(run.stages) ? run.stages : [];
    return '<section class="x-cicd-section" aria-label="执行阶段"><h2>执行阶段</h2>' + stageGraph(run, current) +
      (stages.length ? '<details class="x-cicd-stage-more"><summary>阶段明细（Job、审批单、起止时间）</summary><div class="x-cicd-stages">' + stages.map(function (s, i) {
        return '<article class="x-cicd-stage"><header><span class="x-cicd-mono">' + (Number.isInteger(+s.idx) ? +s.idx + 1 : i + 1) +
          "</span><strong>" + escapeHTML(stageName(s, i)) + "</strong>" + status(s.status) + '</header><dl class="x-cicd-grid">' +
          record("类型", val(s.type)) + record("Job", val(s.job_name), "x-cicd-break") +
          record("审批单", s.approval_id ? "#" + escapeHTML(s.approval_id) : "—") +
          record("开始", date(s.started_at)) + record("结束", date(s.finished_at)) +
          record("耗时", s.started_at ? escapeHTML(duration(s.started_at, s.finished_at)) : "—") +
          (s.error ? record("错误", val(s.error), "x-cicd-error x-cicd-break") : "") +
          "</dl></article>";
      }).join("") + "</div></details>" : "") + "</section>";
  }
  var TYPE_LABEL = { build: "镜像构建", test: "测试", script: "脚本", deploy: "部署", jenkins: "Jenkins" };
  function secs(value) {
    if (value == null || !isFinite(value)) return "—";
    var s = Math.round(value);
    return s >= 3600 ? Math.floor(s / 3600) + "小时" + Math.floor(s % 3600 / 60) + "分" : s >= 60 ? Math.floor(s / 60) + "分" + (s % 60) + "秒" : s + "秒";
  }
  function ago(value) {
    if (!value) return "—";
    var t = new Date(value).getTime(); if (!isFinite(t)) return "—";
    var d = Math.max(0, Math.floor((Date.now() - t) / 1000));
    return d < 60 ? "刚刚" : d < 3600 ? Math.floor(d / 60) + " 分钟前" : d < 86400 ? Math.floor(d / 3600) + " 小时前" : Math.floor(d / 86400) + " 天前";
  }
  function kindOf(raw) {
    return /^(succeeded|approved)$/.test(raw) ? "ok" : /^(failed|rejected)$/.test(raw) ? "bad" :
      /^(queued|pending|running|cancelling|awaiting_approval)$/.test(raw) ? "busy" : "neutral";
  }
  function triggerText(t) {
    t = String(t || "");
    if (t === "manual") return "手动";
    if (/^retry:/.test(t)) return "重试 #" + escapeHTML(t.slice(6));
    if (/^webhook:/.test(t)) return "代码推送";
    return val(t);
  }
  function recentStrip(list) {
    var items = (list || []).slice(0, 10).reverse();
    var pad = 10 - items.length, html = "";
    for (var i = 0; i < pad; i++) html += '<i class="x-cicd-dot"></i>';
    return '<span class="x-cicd-strip" aria-label="最近 10 次运行">' + html + items.map(function (st) {
      return '<i class="x-cicd-dot x-cicd-dot--' + kindOf(st) + '" title="' + escapeHTML(LABEL[st] || st) + '"></i>';
    }).join("") + "</span>";
  }
  function rateCell(stats) {
    if (!stats || stats.success_rate == null) return '<span class="x-cicd-muted">—</span>';
    var pct = Math.round(stats.success_rate * 100), cls = pct >= 90 ? "ok" : pct >= 60 ? "warn" : "bad";
    return '<span class="x-cicd-rate x-cicd-rate--' + cls + '"><b style="width:' + pct + '%"></b></span><span class="x-cicd-numeric">' + pct +
      '%</span><span class="x-cicd-muted"> ' + stats.succeeded + "/" + stats.finished + "</span>";
  }
  function stageGraph(run, current) {
    var stages = Array.isArray(run.stages) ? run.stages : [];
    if (!stages.length) return '<p class="x-cicd-empty">该运行暂无阶段执行记录。</p>';
    return '<ol class="x-cicd-graph">' + stages.map(function (s, i) {
      var idx = s.idx == null ? i : s.idx, k = kindOf(String(s.status || ""));
      return '<li class="x-cicd-node x-cicd-node--' + k + (String(idx) === String(current) ? " is-active" : "") + (s.status === "running" ? " is-running" : "") +
        '"><button type="button" data-action="stage-log" data-id="' + escapeHTML(idx) + '" title="查看该阶段日志">' +
        '<span class="x-cicd-node-type">' + escapeHTML(TYPE_LABEL[s.type] || s.type || "") + '</span>' +
        '<strong>' + escapeHTML(stageName(s, i)) + "</strong>" +
        '<span class="x-cicd-node-meta">' + status(s.status) + '<em>' + (s.started_at ? escapeHTML(duration(s.started_at, s.finished_at)) : "—") + "</em></span>" +
        (s.error ? '<span class="x-cicd-node-err" title="' + escapeHTML(s.error) + '">' + escapeHTML(s.error) + "</span>" : "") +
        "</button></li>";
    }).join("") + "</ol>";
  }
  function formatLog(line, names) {
    var stage = Number(line.stage), label = isFinite(stage) && stage >= 0 ? (names[stage] || "阶段 " + (stage + 1)) : "系统";
    return "[" + (line.ts ? date(line.ts) : "—") + "] [" + label + "] " + String(line.line || "").replace(/\u001b\[[0-9;?]*[A-Za-z]/g, "");
  }

  function createView(el) {
    var xs = window.XS;
    if (!xs || typeof xs.get !== "function") {
      el.innerHTML = '<section class="x-cicd-page"><p class="x-cicd-alert" role="alert">平台 API 不可用，请重新连接后刷新。</p></section>';
      return null;
    }
    var state = {
      el: el, xs: xs, alive: true, seq: 0, runSeq: 0, logSeq: 0, controller: new AbortController(),
      pipelines: [], apps: [], targets: [], jenkins: [], argos: [], selected: null, runs: [], run: null, runError: "", relatedErrors: {},
      log: { rows: [], after: 0, more: false, busy: false, error: "", stage: "", search: "", follow: true },
      timer: null, modal: null, targetId: ""
    };
    function active(token) { return state.alive && state.el.isConnected !== false && (token == null || token === state.seq); }
    function get(path, query, signal) { return xs.get(path, query, { signal: signal || state.controller.signal }); }
    function requestError(err) { return !xs.isCancelled || !xs.isCancelled(err); }
    function disposeLog() { state.logSeq++; if (state.timer) clearTimeout(state.timer); state.timer = null; }
    function reset() {
      disposeLog(); state.seq++; state.runSeq++;
      state.controller.abort(); state.controller = new AbortController();
      state.run = null; state.runError = ""; state.runs = [];
      state.log = { rows: [], after: 0, more: false, busy: false, error: "", stage: "", search: "", follow: true };
    }
    function header(title, subtitle) {
      return '<header class="x-cicd-head"><div><h1>' + escapeHTML(title) + '</h1><p>' + subtitle +
        '</p></div><div class="x-cicd-actions"><button class="x-btn" type="button" data-action="refresh">刷新</button>' +
        (state.selected ? '<a class="x-btn" href="#/cicd/pipeline">返回列表</a>' : "") +
        (state.selected ? '<button class="x-btn x-btn--pri" type="button" data-action="run" data-id="' + escapeHTML(state.selected.id) + '">运行</button>' :
          '<button class="x-btn x-btn--pri" type="button" data-action="create">新建流水线</button>') +
        "</div></header>";
    }
    function connectionPanel(title, action, body, error) {
      var actionAttr = action === "new-argo" ? "data-newargo" : 'data-action="' + action + '"';
      return '<section class="x-cicd-section x-cicd-connection"><div class="x-cicd-section-head"><h2>' + title +
        '</h2><button class="x-btn" type="button" ' + actionAttr + '>新建</button></div>' +
        (error ? '<p class="x-cicd-alert" role="status">' + escapeHTML(error) + '</p>' : "") + body + '</section>';
    }
    function smallTable(columns, rows, empty) {
      if (!rows.length) return '<p class="x-cicd-empty">' + escapeHTML(empty) + '</p>';
      return '<div class="x-cicd-table-wrap"><table class="x-table x-cicd-table x-cicd-table--compact"><thead><tr>' +
        columns.map(function (column) { return '<th>' + escapeHTML(column) + '</th>'; }).join("") +
        '</tr></thead><tbody>' + rows.join("") + '</tbody></table></div>';
    }
    function integrationsHTML() {
      var targetRows = state.targets.map(function (item) {
        return '<tr><td class="x-cicd-title">' + val(item.name) + '</td><td>' + val(item.namespace) +
          '</td><td>' + val(item.env) + '</td><td class="x-cicd-date">' + date(item.created_at) +
          '</td><td class="x-cicd-row-actions"><button class="x-btn" type="button" data-action="delete-target" data-id="' +
          escapeHTML(item.id) + '">移除</button></td></tr>';
      });
      var jenkinsRows = state.jenkins.map(function (item) {
        return '<tr><td class="x-cicd-title">' + val(item.name) + '</td><td class="x-cicd-break">' + val(item.url) +
          '</td><td>' + val(item.username) + '</td><td class="x-cicd-date">' + date(item.created_at) +
          '</td><td class="x-cicd-row-actions"><button class="x-btn" type="button" data-action="delete-jenkins" data-id="' +
          escapeHTML(item.id) + '">移除</button></td></tr>';
      });
      var argoBody = window.XSLiveOpsCICD && typeof window.XSLiveOpsCICD.argoConnectionsHTML === "function"
        ? window.XSLiveOpsCICD.argoConnectionsHTML(state.argos)
        : '<p class="x-cicd-empty">Argo CD 接入模块不可用。</p>';
      return '<div class="x-cicd-connections">' +
        connectionPanel("执行集群", "new-target",
          smallTable(["名称", "命名空间", "环境", "创建时间", "操作"], targetRows, "暂无执行集群。"), state.relatedErrors.targets) +
        connectionPanel("Jenkins", "new-jenkins",
          smallTable(["名称", "地址", "用户名", "创建时间", "操作"], jenkinsRows, "暂无 Jenkins 接入。"), state.relatedErrors.jenkins) +
        connectionPanel("Argo CD", "new-argo", argoBody, state.relatedErrors.argos) +
        '</div>';
    }
    function paintList() {
      var kpi = { total: state.pipelines.length, running: 0, fin: 0, ok: 0, failedLast: 0 };
      state.pipelines.forEach(function (pl) {
        var st = pl.stats || {}, last = st.last_run;
        kpi.fin += st.finished || 0; kpi.ok += st.succeeded || 0;
        if (last && ACTIVE[last.status]) kpi.running++;
        if (last && last.status === "failed") kpi.failedLast++;
      });
      var kpiHTML = '<div class="x-cicd-kpis">' +
        '<div><span>流水线</span><b>' + kpi.total + '</b></div>' +
        '<div><span>运行中</span><b class="x-cicd-kpi--busy">' + kpi.running + '</b></div>' +
        '<div><span>最近一次失败</span><b class="x-cicd-kpi--bad">' + kpi.failedLast + '</b></div>' +
        '<div><span>近期成功率</span><b>' + (kpi.fin ? Math.round(kpi.ok / kpi.fin * 100) + "%" : "—") + '</b><em>' + (kpi.fin ? kpi.ok + "/" + kpi.fin + " 次" : "暂无完成运行") + "</em></div></div>";
      var rows = state.pipelines.map(function (pl) {
        var st = pl.stats || {}, last = st.last_run;
        var stageTypes = (Array.isArray(pl.stages) ? pl.stages : []).map(function (s) { return escapeHTML(s.name || s.type); }).join('<i class="x-cicd-arrow">›</i>');
        return '<tr><td class="x-cicd-title"><a href="#/cicd/pipeline/' + encodeURIComponent(pl.id) + '">' + val(pl.name) +
          '</a><span class="x-cicd-sub">' + nameOf(state.apps, pl.app_id) + ' · ' + (pl.on_push ? "代码推送触发" : "手动触发") + '</span><span class="x-cicd-sub x-cicd-stagepath">' + (stageTypes || "无阶段") +
          '</span></td><td>' + (last ? status(last.status) + '<span class="x-cicd-sub"><a href="#/cicd/pipeline/' + encodeURIComponent(pl.id) + '">#' + escapeHTML(last.id) + "</a> · " + val(last.branch) + " · " + escapeHTML(ago(last.created_at)) + "</span>" : '<span class="x-cicd-muted">未运行</span>') +
          '</td><td>' + (last ? triggerText(last.trigger) + '<span class="x-cicd-sub">' + val(last.triggered_by) + "</span>" : "—") +
          '</td><td>' + recentStrip(st.recent) +
          '</td><td>' + rateCell(st) +
          '</td><td class="x-cicd-numeric">' + secs(st.avg_duration_sec) +
          '</td><td class="x-cicd-row-actions"><a class="x-btn" href="#/cicd/pipeline/' + encodeURIComponent(pl.id) +
          '">详情</a><button class="x-btn" type="button" data-action="edit" data-id="' + escapeHTML(pl.id) +
          '">编辑</button><button class="x-btn" type="button" data-action="run" data-id="' + escapeHTML(pl.id) + '">运行</button></td></tr>';
      }).join("");
      el.innerHTML = '<main class="x-cicd-page">' + header("流水线", "管理构建、测试和部署的执行定义") + kpiHTML +
        (state.relatedErrors.apps ? '<p class="x-cicd-alert" role="status">应用信息暂不可用：' + escapeHTML(state.relatedErrors.apps) + '</p>' : "") +
        '<section class="x-cicd-section"><div class="x-cicd-section-head"><h2>流水线定义</h2><span>' + state.pipelines.length +
        ' 条</span></div><div class="x-cicd-table-wrap"><table class="x-table x-cicd-table x-cicd-table--list">' +
        '<thead><tr><th>流水线</th><th>最近运行</th><th>触发</th><th>最近 10 次</th><th>成功率（近 20 次）</th><th>平均耗时</th><th class="x-cicd-row-actions">操作</th></tr></thead><tbody>' +
        (rows || '<tr><td colspan="7" class="x-cicd-empty">暂无流水线。新建后会显示在这里。</td></tr>') +
        '</tbody></table></div></section>' + integrationsHTML() + modalHTML() + '</main>';
      var argoHost = el.querySelector(".x-cicd-connections .x-cicd-connection:last-child");
      if (argoHost && window.XSLiveOpsCICD && typeof window.XSLiveOpsCICD.bindArgoConnections === "function") {
        window.XSLiveOpsCICD.bindArgoConnections(argoHost, state.argos, load);
      }
    }
    function history() {
      return '<aside class="x-cicd-history"><div class="x-cicd-section-head"><h2>运行历史</h2><span>' + state.runs.length + ' 条已加载</span></div>' +
        (state.runs.length ? '<ol>' + state.runs.map(function (r) {
          return '<li><button type="button" data-action="select-run" data-id="' + escapeHTML(r.id) + '"' +
            (state.run && String(state.run.id) === String(r.id) ? ' aria-current="true"' : "") +
            '><span class="x-cicd-history-top"><strong>#' + escapeHTML(r.id) + '</strong>' + status(r.status) +
            '</span><span>' + date(r.started_at || r.created_at) + '</span><span>' + val(r.branch) +
            ' · ' + (r.started_at ? escapeHTML(duration(r.started_at, r.finished_at)) : "—") + ' · ' + triggerText(r.trigger) + '</span></button></li>';
        }).join("") + '</ol>' : '<p class="x-cicd-empty">还没有运行记录。运行流水线后可查看阶段和日志。</p>') +
        '<p class="x-cicd-muted">仅显示最近 50 次运行。</p></aside>';
    }
    function logPanel() {
      var lines = state.log.rows.filter(function (line) {
        return (!state.log.stage || String(line.stage) === state.log.stage) &&
          (!state.log.search || formatLog(line, []).toLowerCase().includes(state.log.search.toLowerCase()));
      });
      var names = (state.run && state.run.stages || []).map(function (s) { return s.name; });
      return '<section class="x-cicd-section x-cicd-log"><div class="x-cicd-section-head"><h2>运行日志</h2><span data-log-count>' +
        state.log.rows.length + ' 条已加载' + (state.log.more ? "，还有后续日志" : "") + '</span></div>' +
        '<div class="x-cicd-log-tools"><label>阶段<select class="x-select" data-log-stage><option value="">全部阶段</option>' +
        (state.run.stages || []).map(function (s, index) {
          var idx = s.idx == null ? index : s.idx;
          return '<option value="' + idx + '"' + (String(idx) === state.log.stage ? " selected" : "") + '>' + escapeHTML(s.name || "阶段 " + (index + 1)) + '</option>';
        }).join("") + '</select></label><label>搜索<input class="x-input" type="search" data-log-search value="' + escapeHTML(state.log.search) + '" placeholder="搜索已加载日志" /></label>' +
        '<label class="x-cicd-follow"><input type="checkbox" data-log-follow' + (state.log.follow ? " checked" : "") + ' /> 自动跟随</label>' +
        '<button class="x-btn" type="button" data-action="download-log">下载完整日志</button></div>' +
        (state.log.error ? '<p class="x-cicd-alert" role="alert">日志加载失败：' + escapeHTML(state.log.error) +
          ' <button class="x-btn" type="button" data-action="retry-log">重试</button></p>' : "") +
        '<pre class="x-cicd-log-body" tabindex="0" aria-label="运行日志" data-log-body>' +
        (lines.length ? lines.map(function (line) { return escapeHTML(formatLog(line, names)); }).join("\n") :
          state.log.busy ? "正在读取日志…" : "当前筛选没有日志") + '</pre>' +
        (state.log.more ? '<button class="x-btn" type="button" data-action="more-log"' + (state.log.busy ? " disabled" : "") + '>加载后续日志</button>' : "") +
        '</section>';
    }
    function paintDetail() {
      var pl = state.selected, app = itemById(state.apps, pl.app_id), target = itemById(state.targets, pl.runner_target_id);
      var subtitle = '应用 ' + nameOf(state.apps, pl.app_id) + ' · 默认分支 ' + val(app && app.branch) +
        ' · 执行集群 ' + nameOf(state.targets, pl.runner_target_id) + ' · ' + (pl.on_push ? "代码推送触发" : "手动触发");
      var info = '<section class="x-cicd-section"><h2>流水线信息</h2><dl class="x-cicd-grid">' +
        record("应用", nameOf(state.apps, pl.app_id)) + record("代码仓库", app ? val(app.repo_url) : "应用信息未加载", "x-cicd-break") +
        record("默认分支", val(app && app.branch)) + record("执行集群", target ? val(target.name) : "#" + escapeHTML(pl.runner_target_id)) +
        record("命名空间", val(target && target.namespace)) + record("触发方式", pl.on_push ? "代码推送" : "手动") +
        record("更新时间", date(pl.updated_at)) + '</dl></section>';
      var runContent = state.run ? runSummary(state.run) + runStages(state.run, state.log.stage) + logPanel() :
        state.runError ? '<p class="x-cicd-alert" role="alert">运行详情加载失败：' + escapeHTML(state.runError) +
          ' <button class="x-btn" type="button" data-action="retry-run">重试</button></p>' :
          state.runs.length ? '<p class="x-cicd-empty">正在加载运行详情…</p>' : '<p class="x-cicd-empty">暂无运行记录。</p>';
      el.innerHTML = '<main class="x-cicd-page">' + header(pl.name || "流水线 #" + pl.id, subtitle) +
        (state.relatedErrors.apps ? '<p class="x-cicd-alert" role="status">应用信息暂不可用：' + escapeHTML(state.relatedErrors.apps) + '</p>' : "") +
        (state.relatedErrors.targets ? '<p class="x-cicd-alert" role="status">执行集群信息暂不可用：' + escapeHTML(state.relatedErrors.targets) + '</p>' : "") +
        info + '<div class="x-cicd-detail-grid">' + history() + '<div class="x-cicd-detail-main">' + runContent +
        currentConfig(pl) + '</div></div>' + modalHTML() + '</main>';
      if (state.log.follow) { var box = el.querySelector("[data-log-body]"); if (box) box.scrollTop = box.scrollHeight; }
    }
    function paint() { if (!active()) return; state.selected ? paintDetail() : paintList(); }
    function fail(text, retry) {
      if (!active()) return;
      el.innerHTML = '<main class="x-cicd-page">' + header("流水线", "") + '<p class="x-cicd-alert" role="alert">' + escapeHTML(text) +
        ' <button class="x-btn" type="button" data-action="' + escapeHTML(retry || "refresh") + '">重试</button></p></main>';
    }
    function fetchRelated(pl, token) {
      function load(key, path, keys, query) {
        return get(path, query).then(function (data) {
          if (!active(token)) return;
          state[key] = pl && key === "apps" ? [data] : array(data, ["items", key].concat(keys));
          delete state.relatedErrors[key];
          paint();
        }).catch(function (err) {
          if (active(token) && requestError(err)) {
            state.relatedErrors[key] = message(err);
            paint();
          }
        });
      }
      if (pl) {
        load("apps", "/cicd/applications/" + encodeURIComponent(pl.app_id), ["applications"]);
        load("targets", "/cicd/targets", ["targets"]);
        return;
      }
      load("apps", "/cicd/applications", ["applications"], { page: 1, page_size: 200 });
      load("targets", "/cicd/targets", ["targets"]);
      load("jenkins", "/cicd/jenkins", ["jenkins"]);
      load("argos", "/cicd/argocd", ["argocd"]);
    }
    function fetchRuns(token) {
      get("/cicd/runs", { pipeline_id: state.selected.id, page: 1, page_size: 50 }).then(function (data) {
        if (!active(token)) return;
        state.runs = array(data, ["items", "runs"]);
        paint();
        if (state.runs.length) selectRun(state.runs[0].id);
      }).catch(function (err) { if (active(token) && requestError(err)) { state.runError = message(err); paint(); } });
    }
    function load() {
      reset();
      var token = state.seq, targetRoute = route(location.hash);
      if (!targetRoute) { fail("不是流水线路由。", "refresh"); return; }
      state.targetId = targetRoute.id;
      el.innerHTML = '<main class="x-cicd-page"><header class="x-cicd-head"><h1>流水线</h1></header><p class="x-cicd-empty" role="status">正在加载流水线…</p></main>';
      get("/cicd/pipelines").then(function (data) {
        if (!active(token)) return;
        state.pipelines = array(data, ["items", "pipelines"]);
        state.selected = targetRoute.id ? itemById(state.pipelines, targetRoute.id) : null;
        if (targetRoute.id && !state.selected) { fail("流水线不存在或无权查看。", "refresh"); return; }
        paint();
        fetchRelated(state.selected, token);
        if (state.selected) fetchRuns(token);
        else get("/cicd/targets").then(function (data) {
          if (!active(token)) return;
          state.targets = array(data, ["items", "targets"]);
        }).catch(function () {});
      }).catch(function (err) { if (active(token) && requestError(err)) fail(message(err), "refresh"); });
    }
    function selectRun(id) {
      var picked = itemById(state.runs, id);
      if (!picked) return;
      disposeLog(); state.runSeq++;
      var token = state.runSeq;
      state.run = null; state.runError = "";
      state.log = { rows: [], after: 0, more: false, busy: false, error: "", stage: "", search: "", follow: true };
      paint();
      get("/cicd/runs/" + encodeURIComponent(id)).then(function (run) {
        if (!active() || token !== state.runSeq) return;
        state.run = run; paint(); loadLogs();
      }).catch(function (err) { if (active() && token === state.runSeq && requestError(err)) { state.runError = message(err); paint(); } });
    }
    function appendLogs(data) {
      var rows = array(data, ["items", "logs"]), ids = new Set(state.log.rows.map(function (row) { return Number(row.id); }));
      rows.forEach(function (row) {
        var id = Number(row.id);
        if (!isFinite(id) || ids.has(id)) return;
        ids.add(id); state.log.rows.push(row); state.log.after = Math.max(state.log.after, id);
      });
      state.log.more = rows.length === 5000;
    }
    function scheduleLogs(token) {
      if (state.timer) clearTimeout(state.timer);
      if (state.log.more || !state.run || !ACTIVE[state.run.status] || !active()) return;
      state.timer = setTimeout(function () {
        if (token === state.logSeq) loadLogs();
      }, 2000);
    }
    function loadLogs() {
      if (!state.run || state.log.busy) return;
      if (state.timer) clearTimeout(state.timer);
      var runID = state.run.id, token = state.logSeq;
      state.log.busy = true; state.log.error = ""; paint();
      get("/cicd/runs/" + encodeURIComponent(runID) + "/logs", { after: state.log.after }).then(function (data) {
        if (!active() || token !== state.logSeq || !state.run || String(state.run.id) !== String(runID)) return;
        appendLogs(data); state.log.busy = false; paint(); scheduleLogs(token);
      }).catch(function (err) {
        if (!active() || token !== state.logSeq || !requestError(err)) return;
        state.log.busy = false; state.log.error = message(err); paint();
      });
    }
    function downloadLogs() {
      if (!state.run || state.log.busy) return;
      var id = state.run.id, after = 0, lines = [], token = state.logSeq;
      state.log.busy = true; paint();
      function next() {
        return get("/cicd/runs/" + encodeURIComponent(id) + "/logs", { after: after }).then(function (data) {
          if (!active() || token !== state.logSeq) return;
          var rows = array(data, ["items", "logs"]), names = (state.run.stages || []).map(function (s) { return s.name; });
          rows.forEach(function (row) { lines.push(formatLog(row, names)); after = Math.max(after, Number(row.id) || 0); });
          if (rows.length === 5000) {
            if (!after) throw new Error("日志游标无效，无法继续下载");
            return next();
          }
          var blob = new Blob([lines.join("\n") + (lines.length ? "\n" : "")], { type: "text/plain;charset=utf-8" });
          var url = URL.createObjectURL(blob), a = document.createElement("a");
          a.href = url; a.download = "pipeline-run-" + id + ".log"; a.style.display = "none";
          document.body.appendChild(a); a.click(); a.remove(); setTimeout(function () { URL.revokeObjectURL(url); }, 0);
        });
      }
      next().catch(function (err) { if (active() && token === state.logSeq) state.log.error = message(err); })
        .finally(function () { if (active() && token === state.logSeq) { state.log.busy = false; paint(); } });
    }
    function modalHTML() { return state.modal || ""; }
    function showModal(html) { state.modal = html; paint(); }
    function closeModal() { state.modal = null; paint(); }
    function runModal(id) {
      var pl = itemById(state.pipelines, id), app = pl && itemById(state.apps, pl.app_id);
      if (!pl) return;
      showModal('<div class="x-cicd-dialog-backdrop" data-action="close-modal"><section class="x-cicd-dialog" role="dialog" aria-modal="true" aria-label="运行流水线">' +
        '<h2>运行 ' + escapeHTML(pl.name) + '</h2><form data-cicd-form="run" data-id="' + escapeHTML(id) + '"><label>分支<input class="x-input" name="branch" value="' + escapeHTML(app && app.branch || "") +
        '" placeholder="留空使用应用默认分支" /></label><label>Commit SHA（可选）<input class="x-input" name="commit" /></label>' +
        '<p class="x-cicd-alert" data-form-error role="alert" hidden></p><footer><button class="x-btn" type="button" data-action="close-modal">取消</button>' +
        '<button class="x-btn x-btn--pri" type="submit">提交运行</button></footer></form></section></div>');
    }
    function pipelineEditor(existing) {
      if (!state.apps.length || !state.targets.length) {
        showModal('<div class="x-cicd-dialog-backdrop" data-action="close-modal"><section class="x-cicd-dialog" role="dialog" aria-modal="true" aria-label="缺少配置"><h2>无法编辑流水线</h2><p>需要先加载可用应用和执行集群，请刷新后重试。</p><button class="x-btn" type="button" data-action="close-modal">关闭</button></section></div>');
        return;
      }
      var stages = existing && Array.isArray(existing.stages) ? existing.stages : [{ name: "build", type: "build" }];
      var opts = function (items, selected) { return items.map(function (item) {
        return '<option value="' + escapeHTML(item.id) + '"' + (String(item.id) === String(selected) ? " selected" : "") + '>' + val(item.name) + '</option>';
      }).join(""); };
      var area = function (title, name, value, t) {
        var f = FIELD_FOR[name];
        return '<label class="x-cicd-wide" data-for="' + f + '"' + (f.split(" ").indexOf(t) < 0 ? " hidden" : "") + '>' + title +
          '<textarea class="x-input" name="' + name + '" rows="3" spellcheck="false">' + escapeHTML(value) + '</textarea></label>';
      };
      var stageFields = stages.map(function (s, i) {
        var t = s.type || "build";
        return '<fieldset data-editor-stage><legend><span class="x-cicd-mono" data-stage-no>' + (i + 1) + '</span> <span data-stage-title>' + escapeHTML(s.name || "新阶段") + '</span>' +
          '<span class="x-cicd-legend-tools"><button class="x-btn x-btn--sm" type="button" data-action="move-stage" data-id="-1" title="上移">↑</button>' +
          '<button class="x-btn x-btn--sm" type="button" data-action="move-stage" data-id="1" title="下移">↓</button>' +
          '<button class="x-btn x-btn--sm" type="button" data-action="remove-stage">移除</button></span></legend><div class="x-cicd-editor-grid">' +
          editorField("名称", "name", s.name, true) + '<label>类型<select class="x-select" name="type">' +
          ["build", "test", "script", "deploy", "jenkins"].map(function (type) {
            return '<option value="' + type + '"' + (t === type ? " selected" : "") + '>' + TYPE_LABEL[type] + '（' + type + '）</option>';
          }).join("") + '</select></label>' + editorField("镜像", "image", s.image, false, "text", t) +
          editorField("镜像仓库 Secret", "registry_secret", s.registry_secret, false, "text", t) + editorField("部署环境", "deploy_env", s.deploy_env, false, "text", t) +
          editorField("Jenkins ID", "jenkins_id", s.jenkins_id, false, "number", t) + editorField("Jenkins Job", "jenkins_job", s.jenkins_job, false, "text", t) +
          editorField("超时（秒）", "timeout_sec", s.timeout_sec, false, "number") +
          area("命令（每行一条）", "commands", (s.commands || []).join("\n"), t) +
          area("环境变量（每行 KEY=VALUE，敏感值用 secret://名称/键）", "env", mapText(s.env), t) +
          area("Jenkins 参数（每行 KEY=VALUE）", "params", mapText(s.params), t) +
          '<div class="x-cicd-wide x-cicd-jenkins-tools" data-jenkins-tools' + (s.type === "jenkins" ? "" : " hidden") + '><button class="x-btn" type="button" data-action="discover-jenkins">检测 Jenkins 参数</button><span class="x-cicd-muted" data-jenkins-status>从 Jenkins Job 读取公开参数，敏感值不会回填。</span></div></div>' +
          '</fieldset>';
      }).join("");
      showModal('<div class="x-cicd-dialog-backdrop" data-action="close-modal"><section class="x-cicd-dialog x-cicd-dialog--wide" role="dialog" aria-modal="true" aria-label="' +
        (existing ? "编辑" : "新建") + '流水线"><h2>' + (existing ? "编辑" : "新建") +
        '流水线</h2><form data-cicd-form="pipeline" data-mode="visual" data-id="' + escapeHTML(existing && existing.id || "") + '">' +
        '<div class="x-cicd-modebar" role="tablist"><button type="button" class="is-on" data-action="editor-mode" data-id="visual">可视化编排</button><button type="button" data-action="editor-mode" data-id="yaml">YAML</button></div>' +
        '<div data-visual>' +
        editorField("流水线名称", "pipeline_name", existing && existing.name, true) + '<div class="x-cicd-editor-grid"><label>应用<select class="x-select" name="app_id">' +
        opts(state.apps, existing && existing.app_id) + '</select></label><label>执行集群<select class="x-select" name="runner_target_id">' +
        opts(state.targets, existing && existing.runner_target_id) + '</select></label></div><label class="x-cicd-check"><input type="checkbox" name="on_push"' +
        (existing && existing.on_push ? " checked" : "") + ' /> 代码推送时自动触发</label><div data-stage-editor>' + stageFields +
        '</div><button class="x-btn" type="button" data-action="add-stage">添加阶段</button></div>' +
        '<div data-yaml hidden><textarea class="x-input x-cicd-yaml" name="yaml" rows="22" spellcheck="false"></textarea></div><p class="x-cicd-alert" data-form-error role="alert" hidden></p>' +
        '<footer><button class="x-btn" type="button" data-action="close-modal">取消</button><button class="x-btn x-btn--pri" type="submit">' +
        (existing ? "保存" : "创建") + '</button></footer></form></section></div>');
    }
    function editorField(title, name, value, required, type, stageType) {
      var forTypes = stageType != null && FIELD_FOR[name];
      return '<label' + (forTypes ? ' data-for="' + forTypes + '"' + (forTypes.split(" ").indexOf(stageType) < 0 ? " hidden" : "") : "") + '>' + title + '<input class="x-input" name="' + name + '" type="' + (type || "text") + '" value="' + escapeHTML(value || "") + '"' + (required ? " required" : "") + ' /></label>';
    }
    function connectionEditor(kind) {
      var title = kind === "target" ? "接入执行集群" : "接入 Jenkins";
      var body = kind === "target"
        ? '<div class="x-cicd-editor-grid">' + editorField("名称", "name", "", true) +
          editorField("环境", "env", "", false) + editorField("命名空间", "namespace", "matrix-cicd", true) +
          '<label class="x-cicd-wide">Kubeconfig<textarea class="x-input" name="kubeconfig" rows="9" required spellcheck="false" placeholder="粘贴目标集群 kubeconfig；凭据不会回显"></textarea></label></div>'
        : '<div class="x-cicd-editor-grid">' + editorField("名称", "name", "", true) +
          editorField("地址", "url", "", true) + editorField("用户名", "username", "", false) +
          editorField("API Token", "token", "", false, "password") + '</div>';
      showModal('<div class="x-cicd-dialog-backdrop" data-action="close-modal"><section class="x-cicd-dialog" role="dialog" aria-modal="true" aria-label="' +
        title + '"><h2>' + title + '</h2><form data-cicd-form="' + kind + '">' + body +
        '<p class="x-cicd-alert" data-form-error role="alert" hidden></p><footer><button class="x-btn" type="button" data-action="close-modal">取消</button>' +
        '<button class="x-btn x-btn--pri" type="submit">接入</button></footer></form></section></div>');
    }
    function removeConnection(kind, id) {
      var item = itemById(kind === "target" ? state.targets : state.jenkins, id);
      if (!item) return;
      var label = kind === "target" ? "执行集群" : "Jenkins 接入";
      if (typeof root.confirm === "function" && !root.confirm("确定移除" + label + "「" + item.name + "」？")) return;
      xs.del("/cicd/" + (kind === "target" ? "targets" : "jenkins") + "/" + encodeURIComponent(id)).then(function () {
        if (root.XS && root.XS.toast) root.XS.toast(label + "已移除");
        load();
      }).catch(function (err) { if (root.XS && root.XS.toast) root.XS.toast("移除失败：" + message(err)); });
    }
    function discoverJenkins(button) {
      var stage = button.closest("[data-editor-stage]");
      if (!stage) return;
      var id = stage.querySelector('[name="jenkins_id"]').value.trim();
      var job = stage.querySelector('[name="jenkins_job"]').value.trim();
      var status = stage.querySelector("[data-jenkins-status]");
      if (!id || !/^\d+$/.test(id)) { formError(stage.closest("form"), new Error("请先填写有效的 Jenkins ID")); return; }
      if (!job) { formError(stage.closest("form"), new Error("请先填写 Jenkins Job 路径")); return; }
      button.disabled = true;
      if (status) status.textContent = "正在读取 Jenkins Job 参数…";
      get("/cicd/jenkins/" + encodeURIComponent(id) + "/job-parameters", { job: job }).then(function (data) {
        if (!active()) return;
        var parameters = array(data, ["parameters"]);
        var lines = parameters.filter(function (parameter) { return !parameter.sensitive; }).map(function (parameter) {
          var value = parameter.default_value;
          if (value == null) value = Array.isArray(parameter.choices) && parameter.choices.length ? parameter.choices[0] : "";
          return String(parameter.name || "").trim() + "=" + String(value == null ? "" : value);
        }).filter(function (line) { return !/^=/.test(line); });
        stage.querySelector('[name="params"]').value = lines.join("\n");
        if (status) status.textContent = "已发现 " + parameters.length + " 个参数；敏感默认值未读取。";
      }).catch(function (err) {
        if (active()) {
          if (status) status.textContent = "参数读取失败";
          formError(stage.closest("form"), err);
        }
      }).finally(function () { button.disabled = false; });
    }
    function mapText(data) { return Object.keys(data || {}).map(function (k) { return k + "=" + data[k]; }).join("\n"); }
    function parseMap(text) {
      var result = {};
      String(text || "").split(/\r?\n/).filter(function (row) { return row.trim(); }).forEach(function (row) {
        var at = row.indexOf("="), key = row.slice(0, at).trim();
        if (at < 1 || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key) || Object.hasOwn(result, key)) throw new Error("环境变量或参数须为不重复的 KEY=VALUE");
        result[key] = row.slice(at + 1);
      });
      return result;
    }
    function editorStages(form, existing) {
      var rows = Array.from(form.querySelectorAll("[data-editor-stage]"));
      if (!rows.length) throw new Error("至少配置一个阶段");
      return rows.map(function (row, index) {
        var source = existing && existing.stages && existing.stages[index] || {};
        var s = Object.assign({}, source), pick = function (name) { return row.elements.namedItem(name).value.trim(); };
        s.name = pick("name"); s.type = pick("type");
        if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(s.name)) throw new Error("阶段名称需为小写字母、数字或连字符");
        ["image", "registry_secret", "deploy_env", "jenkins_job"].forEach(function (key) { var v = pick(key); if (v) s[key] = v; else delete s[key]; });
        s.commands = pick("commands").split(/\r?\n/).map(function (v) { return v.trim(); }).filter(Boolean);
        s.env = parseMap(pick("env")); s.params = parseMap(pick("params"));
        var jid = pick("jenkins_id"), timeout = pick("timeout_sec");
        if (jid && (!Number.isSafeInteger(+jid) || +jid < 1)) throw new Error("Jenkins ID 无效");
        if (timeout && (!Number.isSafeInteger(+timeout) || +timeout < 0 || +timeout > 86400)) throw new Error("超时范围应为 0 至 86400 秒");
        if (jid) s.jenkins_id = +jid; else delete s.jenkins_id;
        if (timeout) s.timeout_sec = +timeout; else delete s.timeout_sec;
        if ((s.type === "test" || s.type === "script") && (!s.image || !s.commands.length)) throw new Error("测试/脚本阶段需要镜像和执行命令");
        if (s.type === "deploy" && !s.deploy_env) throw new Error("部署阶段需要部署环境");
        if (s.type === "jenkins" && (!s.jenkins_id || !s.jenkins_job)) throw new Error("Jenkins 阶段需要服务器 ID 和 Job 名称");
        return s;
      });
    }
    function renumber() {
      el.querySelectorAll("[data-stage-editor] [data-editor-stage]").forEach(function (row, i) {
        var no = row.querySelector("[data-stage-no]"); if (no) no.textContent = i + 1;
        var title = row.querySelector("[data-stage-title]"), name = row.elements.namedItem("name");
        if (title && name) title.textContent = name.value.trim() || "新阶段";
      });
    }
    function formPayload(form, existing) {
      return {
        name: form.elements.namedItem("pipeline_name").value.trim(), app_id: +form.elements.namedItem("app_id").value,
        runner_target_id: +form.elements.namedItem("runner_target_id").value,
        on_push: form.elements.namedItem("on_push").checked, stages: editorStages(form, existing)
      };
    }
    function switchMode(form, mode) {
      if (!form || form.getAttribute("data-mode") === mode) return;
      var id = form.getAttribute("data-id"), existing = itemById(state.pipelines, id), err = form.querySelector("[data-form-error]");
      if (err) err.hidden = true;
      var task;
      try {
        task = mode === "yaml" ? xs.post("/cicd/pipelines/yaml/render", formPayload(form, existing)) :
          xs.post("/cicd/pipelines/yaml/parse", { yaml: form.elements.namedItem("yaml").value });
      } catch (e) { formError(form, e); return; }
      task.then(function (data) {
        if (!active()) return;
        if (mode === "yaml") {
          form.elements.namedItem("yaml").value = data && data.yaml || "";
          form.querySelector("[data-visual]").hidden = true; form.querySelector("[data-yaml]").hidden = false;
          form.setAttribute("data-mode", "yaml");
          form.querySelectorAll("[data-action=editor-mode]").forEach(function (b) { b.classList.toggle("is-on", b.getAttribute("data-id") === "yaml"); });
        } else {
          pipelineEditor(Object.assign({}, existing || {}, data, { id: existing && existing.id }));
        }
      }).catch(function (e) { if (active()) formError(form, e); });
    }
    function formError(form, err) { var node = form.querySelector("[data-form-error]"); if (node) { node.hidden = false; node.textContent = message(err); } }
    function submit(form) {
      var type = form.getAttribute("data-cicd-form"), id = form.getAttribute("data-id");
      var button = form.querySelector('[type="submit"]'); if (button) button.disabled = true;
      var task;
      try {
        if (type === "target") {
          var kubeconfig = form.elements.namedItem("kubeconfig").value.trim();
          if (!kubeconfig) throw new Error("请填写 Kubeconfig");
          task = xs.post("/cicd/targets", {
            name: form.elements.namedItem("name").value.trim(),
            namespace: form.elements.namedItem("namespace").value.trim() || "matrix-cicd",
            env: form.elements.namedItem("env").value.trim(),
            kubeconfig: kubeconfig
          });
        } else if (type === "jenkins") {
          task = xs.post("/cicd/jenkins", {
            name: form.elements.namedItem("name").value.trim(),
            url: form.elements.namedItem("url").value.trim(),
            username: form.elements.namedItem("username").value.trim(),
            token: form.elements.namedItem("token").value
          });
        } else if (type === "run") task = xs.post("/cicd/pipelines/" + encodeURIComponent(id) + "/run", {
          branch: form.elements.namedItem("branch").value.trim(), commit: form.elements.namedItem("commit").value.trim()
        });
        else {
          var existing = itemById(state.pipelines, id);
          var save = function (payload) {
            if (!payload.name || !payload.app_id || !payload.runner_target_id) throw new Error("请填写名称、应用和执行集群");
            return existing ? xs.put("/cicd/pipelines/" + encodeURIComponent(id), payload) : xs.post("/cicd/pipelines", payload);
          };
          task = form.getAttribute("data-mode") === "yaml"
            ? xs.post("/cicd/pipelines/yaml/parse", { yaml: form.elements.namedItem("yaml").value }).then(save)
            : save(formPayload(form, existing));
        }
      } catch (err) { formError(form, err); if (button) button.disabled = false; return; }
      task.then(function () { if (active()) { closeModal(); load(); } }).catch(function (err) {
        if (active()) formError(form, err);
      }).finally(function () { if (button) button.disabled = false; });
    }
    function click(event) {
      var button = event.target.closest("[data-action]"); if (!button || !el.contains(button)) return;
      var action = button.getAttribute("data-action"), id = button.getAttribute("data-id");
      if (action === "close-modal") { if (button === event.target || button.tagName === "BUTTON") closeModal(); return; }
      if (action === "refresh") load();
      if (action === "select-run") selectRun(id);
      if (action === "run") runModal(id);
      if (action === "create" || action === "edit") pipelineEditor(action === "edit" ? itemById(state.pipelines, id) : null);
      if (action === "new-target") {
        // 执行集群只能引用「集群管理」中已注册的集群（后端不再接受 kubeconfig）
        if (window.XSLiveOpsCICD && typeof window.XSLiveOpsCICD.targetModal === "function") window.XSLiveOpsCICD.targetModal(load);
        else if (root.XS && root.XS.toast) root.XS.toast("执行集群接入模块不可用");
      }
      if (action === "new-jenkins") connectionEditor("jenkins");
      if (action === "delete-target") removeConnection("target", id);
      if (action === "delete-jenkins") removeConnection("jenkins", id);
      if (action === "discover-jenkins") discoverJenkins(button);
      if (action === "retry-run" && state.runs.length) selectRun(state.runs[0].id);
      if (action === "more-log" || action === "retry-log") loadLogs();
      if (action === "download-log") downloadLogs();
      if (action === "stage-log") { state.log.stage = state.log.stage === id ? "" : id; paint(); }
      if (action === "cancel-run" || action === "rerun") {
        if (action === "cancel-run" && typeof root.confirm === "function" && !root.confirm("确定取消运行 #" + id + "？")) return;
        button.disabled = true;
        xs.post("/cicd/runs/" + encodeURIComponent(id) + (action === "rerun" ? "/retry" : "/cancel"), {}).then(function (r) {
          if (root.XS && root.XS.toast) root.XS.toast(action === "rerun" ? "已提交重新运行 #" + (r && r.id || "") : "已请求取消");
          if (!active()) return;
          if (action === "rerun" && r && r.id) { fetchRuns(state.seq); selectRun(r.id); } else selectRun(id);
        }).catch(function (err) { button.disabled = false; if (root.XS && root.XS.toast) root.XS.toast((action === "rerun" ? "重新运行" : "取消") + "失败：" + message(err)); });
      }
      if (action === "remove-stage") { var row = button.closest("[data-editor-stage]"); if (row) row.remove(); renumber(); }
      if (action === "move-stage") {
        var cur = button.closest("[data-editor-stage]");
        if (cur) {
          if (id === "-1" && cur.previousElementSibling) cur.parentNode.insertBefore(cur, cur.previousElementSibling);
          if (id === "1" && cur.nextElementSibling) cur.parentNode.insertBefore(cur.nextElementSibling, cur);
          renumber();
        }
      }
      if (action === "editor-mode") switchMode(button.closest("form"), id);
      if (action === "add-stage") {
        var list = el.querySelector("[data-stage-editor]");
        if (list) {
          var row = list.querySelector("[data-editor-stage]");
          if (row) {
            var clone = row.cloneNode(true);
            clone.querySelectorAll("input,textarea").forEach(function (input) { input.value = ""; });
            list.appendChild(clone); renumber();
          }
        }
      }
    }
    function change(event) {
      var node = event.target;
      if (node.matches("[data-log-stage]")) { state.log.stage = node.value; paint(); }
      if (node.matches("[data-log-follow]")) { state.log.follow = node.checked; paint(); }
      if (node.matches("[data-editor-stage] [name=type]")) {
        var stage = node.closest("[data-editor-stage]");
        var tools = stage && stage.querySelector("[data-jenkins-tools]");
        if (tools) tools.hidden = node.value !== "jenkins";
        if (stage) stage.querySelectorAll("[data-for]").forEach(function (f) { f.hidden = f.getAttribute("data-for").split(" ").indexOf(node.value) < 0; });
      }
    }
    function input(event) {
      if (event.target.matches("[data-editor-stage] [name=name]")) renumber();
      if (event.target.matches("[data-log-search]")) {
        var node = event.target, start = node.selectionStart;
        state.log.search = node.value; paint();
        var replacement = el.querySelector("[data-log-search]");
        if (replacement) { replacement.focus(); replacement.setSelectionRange(start, start); }
      }
    }
    function formSubmit(event) {
      var form = event.target.closest("[data-cicd-form]");
      if (form && el.contains(form)) { event.preventDefault(); submit(form); }
    }
    function hashChange() {
      if (!state.alive) return;
      if (route(location.hash)) load(); else dispose();
    }
    function dispose() {
      state.alive = false; reset();
      el.removeEventListener("click", click); el.removeEventListener("change", change);
      el.removeEventListener("input", input); el.removeEventListener("submit", formSubmit);
      window.removeEventListener("hashchange", hashChange); instances.delete(el);
    }
    el.addEventListener("click", click); el.addEventListener("change", change);
    el.addEventListener("input", input); el.addEventListener("submit", formSubmit);
    window.addEventListener("hashchange", hashChange);
    state.dispose = dispose; state.load = load; state.selectRun = selectRun; state.loadLogs = loadLogs;
    load();
    return state;
  }

  window.XSCICDPipelineView = {
    render: function (el) {
      if (!el) throw new TypeError("render(el) requires a container");
      var previous = instances.get(el); if (previous) previous.dispose();
      var view = createView(el); if (view) instances.set(el, view);
      return view && view.dispose;
    },
    _internals: { route: route, array: array, duration: duration, formatLog: formatLog,
      runSummary: runSummary, runStages: runStages, currentConfig: currentConfig, escapeHTML: escapeHTML }
  };
})();
