/*
 * 星枢 Matrix 研发中心 · 交付效能（window.XSDora）
 *
 * XSDora.render(el)   在 el 内绘制「交付效能」页：部署频率、变更前置时间、变更失败率、故障恢复时间四张指标卡
 *                     （分级徽标 精英/高/中/低/数据不足 + 一句中文说明），按天的部署次数柱状图（内联 SVG），
 *                     筛选：时间窗 7/30/90 天、应用、环境。窗口内没有任何发布时显示空状态（XSEmpty）。
 * 数据只取研发中心交付效能接口与 CI/CD 应用列表；演示模式（XS.mode !== "live"）使用内置演示数据。
 * 纯函数（levelText / fmtHours / perText / chart / cards / demo）同时挂在 XSDora 上便于测试。所有外部文本均转义。
 */
(function (root) {
  "use strict";

  function E(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function xs() { return root.XS || {}; }
  function live() { return xs().mode === "live"; }
  function api(path, opts) {
    var X = xs();
    if (typeof X.api === "function") return X.api(path, opts || {});
    return Promise.reject(new Error("接口未就绪"));
  }
  function errText(e) {
    if (e && e.status === 403) return "没有权限查看交付效能，请联系管理员分配研发中心查看权限";
    if (e && e.status === 404) return "当前版本暂不支持交付效能统计";
    return (e && (e.message || e.error)) || String(e || "未知错误");
  }
  function arr(v) { return Array.isArray(v) ? v : []; }
  function num(v) { return typeof v === "number" && isFinite(v) ? v : null; }

  var LEVELS = {
    elite: { t: "精英", c: "#16a34a", bg: "#f0fdf4" },
    high: { t: "高", c: "#16a34a", bg: "#ffffff" },
    medium: { t: "中", c: "#d97706", bg: "#fffbeb" },
    low: { t: "低", c: "#dc2626", bg: "#fef2f2" },
    unknown: { t: "数据不足", c: "#6b7280", bg: "#f9fafb" }
  };
  function levelOf(l) { return LEVELS[l] ? l : "unknown"; }
  function levelText(l) { return LEVELS[levelOf(l)].t; }
  function badge(l) {
    var x = LEVELS[levelOf(l)];
    return '<span class="xdo-badge" data-level="' + levelOf(l) + '" style="color:' + x.c + ";border-color:" + x.c + ";background:" + x.bg + '">' + E(x.t) + "</span>";
  }

  // 小时数 → 易读时长
  function fmtHours(h) {
    h = num(h);
    if (h == null) return "—";
    if (h < 1) return Math.max(1, Math.round(h * 60)) + " 分钟";
    if (h < 48) return (Math.round(h * 10) / 10) + " 小时";
    return (Math.round(h / 24 * 10) / 10) + " 天";
  }
  // 每天部署次数 → 易读频率
  function perText(perDay, total) {
    perDay = num(perDay) || 0;
    if (!total) return "0 次";
    if (perDay >= 1) return "每天 " + (Math.round(perDay * 10) / 10) + " 次";
    if (perDay * 7 >= 1) return "每周 " + (Math.round(perDay * 7 * 10) / 10) + " 次";
    return "每月 " + (Math.round(perDay * 30 * 10) / 10) + " 次";
  }
  function pct(r) { r = num(r); return r == null ? "—" : (Math.round(r * 1000) / 10) + "%"; }

  var BASIS = { links: "按关联到需求 / 缺陷的最早提交时间计算", run: "按构建开始时间估算（尚无提交关联）", mixed: "部分按关联的提交时间，部分按构建开始时间估算", none: "" };

  // cards(data) → 四张指标卡 HTML
  function cards(d) {
    d = d || {};
    var df = d.deploy_frequency || {}, lt = d.lead_time || {}, cfr = d.change_failure_rate || {}, mt = d.mttr || {}, lv = d.level || {};
    var list = [
      { k: "deploy_frequency", t: "部署频率", v: perText(df.per_day, df.total), sub: "窗口内成功部署 " + (df.total || 0) + " 次",
        tip: "成功发布到环境的频率。小步快跑、频繁发布，单次风险更低。" },
      { k: "lead_time", t: "变更前置时间", v: fmtHours(lt.median_hours), sub: lt.samples ? "中位数，样本 " + lt.samples + " 次" + (num(lt.p90_hours) != null ? "，90% 在 " + fmtHours(lt.p90_hours) + " 内" : "") : "暂无已完成的发布",
        tip: "代码从提交到发布完成所需时间。" + (BASIS[lt.basis] ? BASIS[lt.basis] + "。" : "") },
      { k: "change_failure_rate", t: "变更失败率", v: pct(cfr.rate), sub: cfr.total ? "失败 " + (cfr.failed || 0) + " / 共 " + cfr.total + " 次发布" : "暂无已结束的发布",
        tip: "发布失败、24 小时内被回滚或灰度中止的发布占比。" },
      { k: "mttr", t: "故障恢复时间", v: fmtHours(mt.median_hours), sub: mt.available === false ? "未启用值班故障管理" : (mt.samples ? "中位数，已恢复故障 " + mt.samples + " 起" : "窗口内没有已恢复的故障"),
        tip: "线上故障从宣告到恢复的时间，体现团队的止血能力。" }
    ];
    return '<div class="xdo-cards">' + list.map(function (c) {
      return '<section class="xdo-card" data-metric="' + c.k + '"><div class="xdo-card-h"><span>' + E(c.t) + "</span>" + badge(lv[c.k]) + '</div><div class="xdo-val">' + E(c.v) + '</div><div class="xdo-sub">' + E(c.sub) + '</div><p class="xdo-tip">' + E(c.tip) + "</p></section>";
    }).join("") + "</div>";
  }

  // chart(series) → 按天部署次数柱状图（内联 SVG）；series = [[YYYY-MM-DD, count], ...]
  function chart(series) {
    series = arr(series).filter(function (p) { return Array.isArray(p) && p.length >= 2; });
    var W = 720, H = 180, L = 32, R = 8, T = 10, B = 26;
    var n = series.length;
    if (!n) return "";
    var max = 0;
    series.forEach(function (p) { max = Math.max(max, Number(p[1]) || 0); });
    var top = Math.max(1, Math.ceil(max));
    var pw = W - L - R, ph = H - T - B, step = pw / n, bw = Math.max(2, Math.min(28, step * 0.7));
    var out = '<svg class="xdo-chart" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="每日部署次数">';
    [0, 0.5, 1].forEach(function (f) {
      var y = T + ph - ph * f, v = Math.round(top * f * 10) / 10;
      out += '<line x1="' + L + '" x2="' + (W - R) + '" y1="' + y + '" y2="' + y + '" stroke="#e5e7eb"/>' +
        '<text x="' + (L - 6) + '" y="' + (y + 4) + '" text-anchor="end" font-size="10" fill="#6b7280">' + v + "</text>";
    });
    var every = Math.ceil(n / 10);
    series.forEach(function (p, i) {
      var c = Number(p[1]) || 0, h = ph * c / top, x = L + step * i + (step - bw) / 2, y = T + ph - h;
      out += '<rect data-day="' + E(p[0]) + '" x="' + x.toFixed(1) + '" y="' + y.toFixed(1) + '" width="' + bw.toFixed(1) + '" height="' + Math.max(c ? 1 : 0, h).toFixed(1) + '" rx="2" fill="#1677ff"><title>' + E(p[0]) + "：部署 " + c + " 次</title></rect>";
      if (i % every === 0 || i === n - 1) out += '<text x="' + (L + step * i + step / 2).toFixed(1) + '" y="' + (H - 8) + '" text-anchor="middle" font-size="10" fill="#6b7280">' + E(String(p[0]).slice(5)) + "</text>";
    });
    return out + "</svg>";
  }

  // 演示数据：确定性伪随机，不依赖真实接口。
  function demo(days, appId, env) {
    days = days || 30;
    var seed = (Number(appId) || 0) * 7 + (env === "prod" ? 3 : env ? 11 : 0) + days;
    function rnd() { seed = (seed * 9301 + 49297) % 233280; return seed / 233280; }
    var today = new Date(), series = [], total = 0;
    for (var i = days - 1; i >= 0; i--) {
      var d = new Date(today.getFullYear(), today.getMonth(), today.getDate() - i), wd = d.getDay();
      var c = wd === 0 || wd === 6 ? (rnd() < 0.2 ? 1 : 0) : Math.floor(rnd() * 4);
      total += c;
      series.push([d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0"), c]);
    }
    var failed = Math.round(total * 0.08);
    return {
      window_days: days,
      deploy_frequency: { per_day: Math.round(total / days * 1000) / 1000, total: total, series: series },
      lead_time: { median_hours: 9.5, p90_hours: 30.2, samples: total, basis: "links" },
      change_failure_rate: { rate: total ? Math.round(failed / total * 10000) / 10000 : null, failed: failed, total: total },
      mttr: { median_hours: 2.4, samples: 3, available: true },
      level: { deploy_frequency: total / days >= 1 ? "elite" : "high", lead_time: "elite", change_failure_rate: "high", mttr: "high" }
    };
  }
  var DEMO_APPS = [{ id: 1, name: "订单服务", envs: [{ name: "test" }, { name: "prod" }] }, { id: 2, name: "用户中心", envs: [{ name: "dev" }, { name: "prod" }] }];

  var ST = { days: 30, app_id: "", env: "", seq: 0 };

  (function ensureCss() {
    if (typeof document === "undefined" || !document.createElement || document.getElementById("xs-dora-css")) return;
    var s = document.createElement("style");
    s.id = "xs-dora-css";
    s.textContent = ".xdo-filters{display:flex;gap:8px;flex-wrap:wrap;align-items:center}" +
      ".xdo-cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin:12px 0}" +
      ".xdo-card{background:#fff;border:1px solid #e5e7eb;border-radius:6px;padding:14px 16px}" +
      ".xdo-card-h{display:flex;justify-content:space-between;align-items:center;color:#374151;font-size:13px}" +
      ".xdo-badge{display:inline-block;border:1px solid;border-radius:10px;padding:0 8px;font-size:12px;line-height:18px}" +
      ".xdo-val{font-size:24px;font-weight:600;color:#111827;margin:8px 0 2px}" +
      ".xdo-sub{font-size:12px;color:#6b7280}.xdo-tip{font-size:12px;color:#4b5563;margin:8px 0 0;line-height:1.6}" +
      ".xdo-panel{background:#fff;border:1px solid #e5e7eb;border-radius:6px;padding:14px 16px}" +
      ".xdo-panel h3{margin:0 0 8px;font-size:14px;color:#111827}.xdo-note{font-size:12px;color:#6b7280;margin:8px 0 0}";
    (document.head || document.body).appendChild(s);
  })();

  function filtersHtml(apps) {
    var envs = {};
    apps.forEach(function (a) {
      if (ST.app_id && String(a.id) !== String(ST.app_id)) return;
      arr(a.envs).forEach(function (e) { var n = typeof e === "string" ? e : e && e.name; if (n) envs[n] = 1; });
    });
    if (ST.env) envs[ST.env] = 1;
    return '<div class="xdo-filters">' +
      '<select class="x-input" data-dora="days" style="width:auto" aria-label="时间窗">' + [[7, "最近 7 天"], [30, "最近 30 天"], [90, "最近 90 天"]].map(function (o) { return '<option value="' + o[0] + '"' + (o[0] === ST.days ? " selected" : "") + ">" + o[1] + "</option>"; }).join("") + "</select>" +
      '<select class="x-input" data-dora="app" style="width:auto" aria-label="应用"><option value="">全部应用</option>' + apps.map(function (a) { return '<option value="' + E(a.id) + '"' + (String(a.id) === String(ST.app_id) ? " selected" : "") + ">" + E(a.name) + "</option>"; }).join("") + "</select>" +
      '<select class="x-input" data-dora="env" style="width:auto" aria-label="环境"><option value="">全部环境</option>' + Object.keys(envs).sort().map(function (n) { return '<option value="' + E(n) + '"' + (n === ST.env ? " selected" : "") + ">" + E(n) + "</option>"; }).join("") + "</select>" +
      '<button class="x-btn" data-dora-refresh>刷新</button></div>';
  }

  function emptyHtml() {
    var o = { kind: "release", title: "统计窗口内还没有发布记录", desc: "应用通过 CI/CD 发布到环境后，这里会自动计算部署频率、前置时间、失败率与恢复时间。", actions: [{ text: "前往应用发布", href: "#/delivery/apps", primary: true }] };
    var X = root.XSEmpty;
    if (X && typeof X.html === "function") return X.html(o);
    return '<p class="xb-empty">' + E(o.title) + "。" + E(o.desc) + "</p>";
  }

  function body(d) {
    var df = d.deploy_frequency || {}, cfr = d.change_failure_rate || {};
    if (!df.total && !cfr.total) return emptyHtml();
    return cards(d) + '<section class="xdo-panel"><h3>每日部署次数</h3>' + chart(df.series) +
      '<p class="xdo-note">分级参考 DORA 2023 行业基准：精英团队每天多次部署、前置时间不到 1 天、失败率不超过 5%、1 小时内恢复。</p></section>';
  }

  function load(el, apps) {
    var seq = ++ST.seq, box = el.querySelector("[data-dora-body]");
    if (box) box.innerHTML = '<p class="x-frac">正在计算…</p>';
    var q = { days: ST.days, tz_offset: -new Date().getTimezoneOffset() };
    if (ST.app_id) q.app_id = ST.app_id;
    if (ST.env) q.env = ST.env;
    var p = live() ? api("/devcenter/dora", { query: q }) : Promise.resolve(demo(ST.days, ST.app_id, ST.env));
    return p.then(function (d) {
      if (seq !== ST.seq) return;
      box = el.querySelector("[data-dora-body]");
      if (box) box.innerHTML = body(d || {});
    }, function (e) {
      if (seq !== ST.seq) return;
      box = el.querySelector("[data-dora-body]");
      if (box) box.innerHTML = '<p class="x-aerr">读取失败：' + E(errText(e)) + "</p>";
    });
  }

  function paint(el, apps) {
    el.innerHTML = '<div class="xb-head"><div><h2>交付效能</h2><p>用部署频率、变更前置时间、变更失败率和故障恢复时间四项指标衡量团队的交付速度与稳定性。</p></div><div class="xb-head-act">' + filtersHtml(apps) + "</div></div>" +
      (live() ? "" : '<p class="xdo-note">当前为演示数据。</p>') + "<div data-dora-body></div>";
    return load(el, apps);
  }

  function render(el) {
    if (!el) return Promise.resolve();
    var appsP = live() ? api("/cicd/applications", { query: { page_size: 200 } }).then(function (r) { return Array.isArray(r) ? r : arr(r && r.items); }, function () { return []; }) : Promise.resolve(DEMO_APPS);
    el.innerHTML = '<p class="x-frac">正在加载…</p>';
    return appsP.then(function (apps) {
      if (!el.__xsDora && typeof el.addEventListener === "function") {
        el.__xsDora = true;
        el.addEventListener("change", function (ev) {
          var t = ev && ev.target, k = t && t.getAttribute && t.getAttribute("data-dora");
          if (!k || !el.__xsDoraApps) return;
          if (k === "days") { var n = Number(t.value); ST.days = n === 7 || n === 90 ? n : 30; }
          if (k === "app") { ST.app_id = t.value || ""; ST.env = ""; }
          if (k === "env") ST.env = t.value || "";
          return paint(el, el.__xsDoraApps);
        });
        el.addEventListener("click", function (ev) {
          var t = ev && ev.target;
          if (t && t.closest && t.closest("[data-dora-refresh]") && el.__xsDoraApps) return load(el, el.__xsDoraApps);
        });
      }
      el.__xsDoraApps = apps;
      return paint(el, apps);
    });
  }

  root.XSDora = {
    render: render,
    levelText: levelText,
    fmtHours: fmtHours,
    perText: perText,
    cards: cards,
    chart: chart,
    demo: demo,
    _state: ST
  };
})(typeof window !== "undefined" ? window : this);
