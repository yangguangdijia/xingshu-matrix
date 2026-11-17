/* 星枢 Matrix · 列表行内迷你趋势图（近 30 分钟 CPU / 内存）
 *
 * window.XSSpark
 *   svg(points, { w: 80, h: 24, color, min, max, label })  纯 SVG 折线；无有效数据返回「—」
 *   placeholder(key, { ip })                                 渲染单元格占位（行内 HTML）
 *   cell(series, unit, opts)                                 由一条 {cpu, mem} 生成单元格 HTML
 *   attach(tableEl, opts)                                    批量拉数据并填充占位单元格
 *       opts: { clusterId, kind: "pod"|"node"|"workload", workloadKind, namespace,
 *               apiOptions, lazy = true, hideColumnWhenNone = true, ttlMs = 60000 }
 *
 * 只在实时模式请求真实接口；演示模式与任何失败都静默降级为「—」，不弹错误。
 * 不依赖第三方库。
 */
(function (root) {
  "use strict";

  var ENDPOINT = "/observability/metrics/sparklines";
  var BATCH = 50;
  var COLORS = { cpu: "#2563eb", mem: "#0891b2", warn: "#d97706", bad: "#dc2626", none: "#6b7280" };
  var cache = {};      // key → { at, item, unit, source }
  var inflight = {};   // request key → Promise

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function emptyMark(title) {
    return '<span class="xs-spark-empty" style="color:' + COLORS.none + '"' + (title ? ' title="' + esc(title) + '"' : "") + ">—</span>";
  }

  function normalize(points) {
    if (!Array.isArray(points)) return [];
    var out = [];
    points.forEach(function (p, i) {
      var t, v;
      if (Array.isArray(p)) { t = Number(p[0]); v = p[1]; } else { t = i; v = p; }
      v = v == null || v === "" ? NaN : Number(v);
      out.push([Number.isFinite(t) ? t : i, Number.isFinite(v) ? v : null]);
    });
    return out;
  }

  function fmtNum(n) {
    var r = Math.round(n * 1000) / 1000;
    return String(r);
  }

  /* 纯 SVG 折线。points 为 [[ts, v], ...] 或数值数组；null 值断开折线。 */
  function svg(points, opts) {
    opts = opts || {};
    var w = Number(opts.w) > 0 ? Number(opts.w) : 80;
    var h = Number(opts.h) > 0 ? Number(opts.h) : 24;
    var color = opts.color || COLORS.cpu;
    var pts = normalize(points);
    var valid = pts.filter(function (p) { return p[1] != null; });
    if (!valid.length) return emptyMark(opts.emptyTitle || "暂无趋势数据");
    var values = valid.map(function (p) { return p[1]; });
    var times = pts.map(function (p) { return p[0]; });
    var lo = opts.min != null ? Number(opts.min) : Math.min.apply(null, values);
    var hi = opts.max != null ? Number(opts.max) : Math.max.apply(null, values);
    if (!(hi > lo)) { var mid = lo, span = Math.abs(mid) || 1; lo = mid - span; hi = mid + span; } // 平直数据画在中线
    var t0 = Math.min.apply(null, times), t1 = Math.max.apply(null, times);
    var pad = 2;
    function x(t) { return t1 > t0 ? pad + (t - t0) / (t1 - t0) * (w - pad * 2) : w / 2; }
    function y(v) {
      var b = Math.max(lo, Math.min(hi, v));
      return pad + (hi - b) / (hi - lo) * (h - pad * 2);
    }
    var d = "", started = false, last = null;
    pts.forEach(function (p) {
      if (p[1] == null) { started = false; return; }
      d += (started ? "L" : "M") + fmtNum(x(p[0])) + "," + fmtNum(y(p[1]));
      started = true;
      last = p;
    });
    var label = opts.label || "近 30 分钟趋势";
    var dot = last ? '<circle cx="' + fmtNum(x(last[0])) + '" cy="' + fmtNum(y(last[1])) + '" r="1.6" style="fill:' + esc(color) + '"></circle>' : "";
    // 只有一个有效点时画一条水平短线，避免空图。
    if (valid.length === 1) d = "M" + pad + "," + fmtNum(y(valid[0][1])) + "L" + (w - pad) + "," + fmtNum(y(valid[0][1]));
    return '<svg class="xs-spark-svg" width="' + w + '" height="' + h + '" viewBox="0 0 ' + w + " " + h + '" role="img" aria-label="' + esc(label) +
      '" focusable="false" style="display:inline-block;vertical-align:middle;overflow:visible">' +
      '<path d="' + d + '" style="fill:none;stroke:' + esc(color) + ';stroke-width:1.5;stroke-linejoin:round;stroke-linecap:round"></path>' + dot + "</svg>";
  }

  function lastValue(points) {
    var pts = normalize(points);
    for (var i = pts.length - 1; i >= 0; i--) if (pts[i][1] != null) return pts[i][1];
    return null;
  }

  function fmtValue(v, unit) {
    if (v == null) return "—";
    if (unit === "percent") return (Math.round(v * 10) / 10) + "%";
    if (unit === "cores") return v < 1 ? Math.round(v * 1000) + "m" : (Math.round(v * 100) / 100) + " 核";
    if (unit === "bytes") {
      var units = ["B", "Ki", "Mi", "Gi", "Ti"], i = 0;
      while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
      return (i === 0 ? Math.round(v) : Math.round(v * 10) / 10) + " " + units[i];
    }
    return String(Math.round(v * 100) / 100);
  }

  function colorFor(kind, value, unit) {
    if (unit === "percent" && value != null) {
      if (value >= 85) return COLORS.bad;
      if (value >= 75) return COLORS.warn;
    }
    return COLORS[kind];
  }

  function row(kind, points, unit) {
    var name = kind === "cpu" ? "CPU" : "内存";
    var v = lastValue(points);
    var text = fmtValue(v, unit);
    var graph = svg(points, {
      w: 80, h: 16, color: colorFor(kind, v, unit),
      min: unit === "percent" ? 0 : undefined, max: unit === "percent" ? 100 : undefined,
      label: name + " 近 30 分钟趋势，当前 " + text, emptyTitle: name + "暂无数据"
    });
    return '<span class="xs-spark-row" style="display:flex;align-items:center;gap:6px;white-space:nowrap;line-height:16px">' +
      '<span style="width:28px;color:' + COLORS.none + ';font-size:12px">' + name + "</span>" + graph +
      '<span class="x-frac" style="font-size:12px;min-width:44px">' + esc(text) + "</span></span>";
  }

  /* 一个对象的单元格内容：上 CPU、下内存。 */
  function cell(series, unit, opts) {
    opts = opts || {};
    unit = unit || {};
    if (!series) return emptyMark(opts.emptyTitle || "暂无趋势数据");
    var hasAny = lastValue(series.cpu) != null || lastValue(series.mem) != null;
    if (!hasAny) return emptyMark(opts.emptyTitle || "近 30 分钟无采样");
    return '<span class="xs-spark-cell" style="display:inline-flex;flex-direction:column;gap:2px">' +
      row("cpu", series.cpu, unit.cpu) + row("mem", series.mem, unit.mem) + "</span>";
  }

  /* 列表行占位：attach 会按 data-spark 找到并填充。 */
  function placeholder(key, opts) {
    opts = opts || {};
    return '<span class="xs-spark" data-spark="' + esc(key) + '"' + (opts.ip ? ' data-spark-ip="' + esc(opts.ip) + '"' : "") +
      ' style="display:inline-block;min-width:80px;color:' + COLORS.none + '">—</span>';
  }

  function xs() { return root.XS || {}; }

  function cacheKey(opts, name) {
    return [opts.clusterId, opts.kind, opts.workloadKind || "", opts.namespace || "", name].join("|");
  }

  function requestBatch(opts, names, ips) {
    var query = { cluster_id: opts.clusterId, kind: opts.kind, names: names.join(",") };
    if (opts.kind === "workload") query.workload_kind = opts.workloadKind || "deployments";
    if (opts.namespace) query.namespace = opts.namespace;
    if (opts.kind === "node" && ips.some(Boolean)) query.ips = ips.join(",");
    var key = JSON.stringify(query);
    if (inflight[key]) return inflight[key];
    var api = xs().api;
    var p = Promise.resolve().then(function () {
      return api(ENDPOINT, Object.assign({ query: query }, opts.apiOptions || {}));
    }).then(function (data) {
      delete inflight[key];
      return data;
    }, function (err) {
      delete inflight[key];
      throw err;
    });
    inflight[key] = p;
    return p;
  }

  function cellsOf(tableEl) {
    if (!tableEl || typeof tableEl.querySelectorAll !== "function") return [];
    return [].slice.call(tableEl.querySelectorAll("[data-spark]"));
  }

  function fill(el, html) {
    if (el && el.isConnected !== false) el.innerHTML = html;
  }

  function hideColumn(tableEl, cells) {
    // 集群未接入监控时，整列只会是「—」：按实时模式「未实现即隐藏」约定隐藏该列。
    cells.forEach(function (el) {
      var td = typeof el.closest === "function" ? el.closest("td") : null;
      if (!td) return;
      td.hidden = true;
      if (td.style) td.style.display = "none";
      var tr = td.parentNode, idx = tr && tr.children ? [].indexOf.call(tr.children, td) : -1;
      var table = typeof td.closest === "function" ? td.closest("table") : null;
      var th = idx >= 0 && table ? table.querySelectorAll("th")[idx] : null;
      if (th) { th.hidden = true; if (th.style) th.style.display = "none"; }
    });
  }

  function load(tableEl, opts) {
    var cells = cellsOf(tableEl);
    if (!cells.length) return Promise.resolve({ filled: 0 });
    var live = xs().mode === "live" && typeof xs().api === "function";
    if (!live || !opts.clusterId || !opts.kind) {
      cells.forEach(function (el) { fill(el, emptyMark("暂无趋势数据")); });
      return Promise.resolve({ filled: 0, skipped: true });
    }
    var ttl = opts.ttlMs == null ? 60000 : Number(opts.ttlMs);
    var now = Date.now();
    var pending = [], seen = {};
    cells.forEach(function (el) {
      var name = el.getAttribute("data-spark");
      var hit = cache[cacheKey(opts, name)];
      if (hit && now - hit.at < ttl) {
        fill(el, cell(hit.item, hit.unit, { emptyTitle: hit.source === "none" ? "集群未接入监控数据源" : "" }));
      } else if (name && !seen[name]) {
        seen[name] = true;
        pending.push({ name: name, ip: el.getAttribute("data-spark-ip") || "" });
      }
    });
    var allCachedNone = !pending.length && cells.every(function (el) {
      var hit = cache[cacheKey(opts, el.getAttribute("data-spark"))];
      return hit && hit.source === "none";
    });
    if (allCachedNone && opts.hideColumnWhenNone !== false) hideColumn(tableEl, cells);
    if (!pending.length) return Promise.resolve({ filled: cells.length });
    var batches = [];
    for (var i = 0; i < pending.length; i += BATCH) batches.push(pending.slice(i, i + BATCH));
    var sources = [];
    return Promise.all(batches.map(function (batch) {
      return requestBatch(opts, batch.map(function (b) { return b.name; }), batch.map(function (b) { return b.ip; })).then(function (data) {
        data = data || {};
        sources.push(data.source || "");
        var byName = {};
        (Array.isArray(data.items) ? data.items : []).forEach(function (it) { if (it && it.name != null) byName[it.name] = it; });
        batch.forEach(function (b) {
          cache[cacheKey(opts, b.name)] = { at: Date.now(), item: byName[b.name] || null, unit: data.unit || {}, source: data.source || "" };
        });
        return { data: data, byName: byName };
      }, function () {
        sources.push("error");
        return null; // 失败静默降级
      });
    })).then(function () {
      var filled = 0;
      cellsOf(tableEl).forEach(function (el) {
        var hit = cache[cacheKey(opts, el.getAttribute("data-spark"))];
        if (hit) { filled++; fill(el, cell(hit.item, hit.unit, { emptyTitle: hit.source === "none" ? "集群未接入监控数据源" : "" })); }
        else fill(el, emptyMark("趋势暂不可用"));
      });
      if (opts.hideColumnWhenNone !== false && sources.length && sources.every(function (s) { return s === "none"; })) {
        hideColumn(tableEl, cellsOf(tableEl));
      }
      return { filled: filled, sources: sources };
    });
  }

  /* 批量填充：默认懒加载（表格进入视口才请求），不支持 IntersectionObserver 时立即请求。 */
  function attach(tableEl, opts) {
    opts = opts || {};
    if (!tableEl) return Promise.resolve({ filled: 0 });
    var IO = root.IntersectionObserver;
    if (opts.lazy === false || typeof IO !== "function") return load(tableEl, opts).catch(function () { return { filled: 0 }; });
    return new Promise(function (resolve) {
      var done = false;
      var io = new IO(function (entries) {
        if (done || !entries.some(function (e) { return e.isIntersecting; })) return;
        done = true;
        io.disconnect();
        load(tableEl, opts).then(resolve, function () { resolve({ filled: 0 }); });
      }, { rootMargin: "120px" });
      io.observe(tableEl);
    });
  }

  function clearCache() { cache = {}; inflight = {}; }

  var api = { svg: svg, cell: cell, placeholder: placeholder, attach: attach, format: fmtValue, clearCache: clearCache, endpoint: ENDPOINT, batchSize: BATCH };
  root.XSSpark = api;
  if (typeof module !== "undefined" && module.exports) module.exports = api;
})(typeof window === "undefined" ? globalThis : window);
