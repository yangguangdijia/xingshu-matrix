/* Themes Chart.js with the Hark chart palette and adds Hark.wash, Hark.labels and Hark.data. Load it after chart.js and datalabels.js. style-gates: data */
(function () {
  "use strict";
  var Chart = window.Chart;
  if (!Chart) throw new Error("hark-charts.js needs chart.js loaded before it");
  var doc = document, root = doc.documentElement;
  var TOKEN = /^var\(--[\w-]+\)$/;
  var PER_POINT = { pie: true, doughnut: true, polarArea: true };
  var LABEL_GAP_PX = 14; // the least vertical distance between two names drawn at line ends
  var VALUE_LABELS_MAX = 6; // value axis labels, when the chart sets no step or limit of its own

  // A canvas cannot read var(), so each token is read from the value a styled element gets.
  var cache = {};
  function computed(prop, value) {
    var probe = doc.createElement("span");
    probe.style[prop] = value;
    (doc.body || root).appendChild(probe);
    var out = getComputedStyle(probe)[prop];
    probe.remove();
    return out;
  }
  function resolve(token) {
    if (!(token in cache)) cache[token] = computed("color", token);
    return cache[token];
  }
  function live(token) { return function () { return resolve(token); }; }
  function isToken(v) { return typeof v === "string" && TOKEN.test(v.trim()); }
  function sameColor(a, b) { return typeof a === "string" && a.replace(/\s/g, "") === String(b).replace(/\s/g, ""); }
  function seriesTokens() {
    var tokens = [];
    for (var i = 1; getComputedStyle(root).getPropertyValue("--chart-" + i).trim(); i++) tokens.push("var(--chart-" + i + ")");
    return tokens;
  }

  function cssVar(name) { return getComputedStyle(root).getPropertyValue(name).trim(); }
  function applyDefaults() {
    var d = Chart.defaults, tip = d.plugins.tooltip, legend = d.plugins.legend.labels;
    var sans = cssVar("--sans"), mono = cssVar("--chart-mono");
    d.font.family = sans; d.font.size = 12; d.font.weight = 400;
    d.color = resolve("var(--chart-ink-2)");
    d.borderColor = resolve("var(--chart-grid)");
    // A chart copies its axis defaults when it is built, so axis colors are functions that read the theme at each draw.
    d.scale.ticks.color = d.scale.title.color = live("var(--chart-ink-2)");
    d.scale.grid.color = live("var(--chart-grid)");
    d.scale.grid.drawTicks = false;
    d.scale.ticks.padding = 8;
    d.scale.ticks.maxRotation = 0; // labels that do not fit are thinned out, never tilted
    d.scale.border.display = false; // the baseline is drawn by the plugin, at zero when the axis crosses it
    d.scales.category.grid = Object.assign(d.scales.category.grid || {}, { display: false });
    d.scales.category.ticks = Object.assign(d.scales.category.ticks || {}, { font: categoryFont(sans, mono) });
    d.scales.linear.ticks = Object.assign(d.scales.linear.ticks || {}, { font: { family: mono, size: 12 } });
    if (d.scales.radialLinear) {
      d.scales.radialLinear.ticks = Object.assign(d.scales.radialLinear.ticks || {}, { font: { family: mono, size: 12 }, showLabelBackdrop: false }); // no white boxes on the grey card
      d.scales.radialLinear.pointLabels = Object.assign(d.scales.radialLinear.pointLabels || {}, { font: { family: sans, size: 14 } });
    }
    d.maintainAspectRatio = false;
    if (d.plugins.colors) d.plugins.colors.enabled = false;
    legend.usePointStyle = true; legend.boxWidth = 8; legend.boxHeight = 8;
    legend.generateLabels = solidSwatches;
    legend.filter = function (item, data) { return !isReference(data.datasets[item.datasetIndex]); };
    tip.backgroundColor = resolve("var(--fill-primary)");
    tip.titleColor = tip.bodyColor = resolve("var(--text-primary-on)");
    tip.titleFont = { family: sans, size: 12, weight: 500 }; // Hark's weights stop at medium for labels
    tip.bodyFont = { family: mono, size: 12, weight: 400 };
    tip.cornerRadius = 8; tip.padding = 10; tip.usePointStyle = true; tip.boxPadding = 4;
    tip.callbacks.labelColor = outlinedSwatch; // a black series' dot would vanish on the dark tooltip
    d.elements.line.borderWidth = 1.5;
    d.elements.line.borderCapStyle = "round"; d.elements.line.borderJoinStyle = "round";
    d.datasets.line.cubicInterpolationMode = "monotone"; // smooth, but never past a real peak or dip
    d.elements.point.hoverRadius = 4; d.elements.point.hitRadius = 8;
    d.datasets.line.pointRadius = 0; // a line is the shape; a scatter keeps its points
    d.elements.bar.borderRadius = 3;
    applyLabelDefaults(sans, mono);
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) d.animation = false;
  }

  function cartesian(chart) { var t = chart.config.type; return !PER_POINT[t] && t !== "radar"; }
  function indexAxis(chart) { return chart.config.options.indexAxis === "y" ? "y" : "x"; }
  function valueAxis(chart) { return indexAxis(chart) === "y" ? "x" : "y"; }

  // Chart.js has already copied its defaults into each axis by now, so a setting still equal to the
  // default counts as unset; one the chart changed stays.
  function applyChrome(chart) {
    if (!cartesian(chart)) return;
    var opts = chart.config.options, scales = opts.scales = opts.scales || {};
    var base = indexAxis(chart), value = valueAxis(chart);
    [base, value].forEach(function (id) {
      var s = scales[id] = scales[id] || {};
      s.grid = s.grid || {}; s.ticks = s.ticks || {};
    });
    if (scales[base].grid.display !== false) scales[base].grid.display = false;
    Object.keys(scales).forEach(function (id) {
      var t = scales[id] && scales[id].ticks;
      if (scales[id].type !== "linear" || !t) return;
      if (t.stepSize === undefined && t.maxTicksLimit === undefined) t.maxTicksLimit = VALUE_LABELS_MAX;
      var ownCallback = t.callback && t.callback !== Chart.Ticks.formatters.numeric; // the default counts as unset
      if (!ownCallback && id === "x" && yearAxis(chart)) t.callback = plainYear;
    });
    if (base === "y" && scales.y.type !== "linear" && scales.y.ticks.autoSkip !== false) scales.y.ticks.autoSkip = false;
    var washed = chart.config.data.datasets.some(function (ds) {
      return (ds.type || chart.config.type) === "line" && (ds.fill === true || ds.fill === "origin" || ds.fill === "start");
    });
    var v = scales[value];
    if (washed && v.min === undefined && v.suggestedMin === undefined && !v.beginAtZero) v.beginAtZero = true;
  }

  // yearAxis reports an x axis whose values are all whole years, which a linear axis would write as "2,025".
  var FIRST_YEAR = 1000, LAST_YEAR = 2999;
  function isYear(v) { return typeof v === "number" && Number.isInteger(v) && v >= FIRST_YEAR && v <= LAST_YEAR; }
  function yearAxis(chart) {
    var xs = (chart.config.data.labels || []).slice();
    chart.config.data.datasets.forEach(function (ds) {
      (ds.data || []).forEach(function (d) { if (d && typeof d === "object" && "x" in d) xs.push(d.x); });
    });
    return xs.length > 0 && xs.every(isYear);
  }
  function plainYear(value, index, ticks) {
    return isYear(value) ? String(value) : Chart.Ticks.formatters.numeric.call(this, value, index, ticks);
  }

  // Headroom is the label's own size in pixels against the axis length. A bound the chart set stays.
  var LABEL_PAD_PX = 10, LABEL_HEIGHT_PX = 18, MAX_ROOM = 0.6;
  function roomAbove(chart, scale) {
    if (!chart.$harkRoom || scale.id !== valueAxis(chart)) return;
    var set = chart.config.options.scales[scale.id] || {}, range = (scale.max - scale.min) || Math.abs(scale.max) || 1;
    var length = scale.isHorizontal() ? scale.width : scale.height;
    if (!length) return;
    var room = function (v) {
      var px = scale.isHorizontal() ? labelWidth(chart, v) + LABEL_PAD_PX : LABEL_HEIGHT_PX + LABEL_PAD_PX;
      var f = Math.min(MAX_ROOM, px / length);
      return range * f / (1 - f);
    };
    if (set.max === undefined && scale.max > 0) scale.max += room(scale.max);
    if (set.min === undefined && scale.min < 0) scale.min -= room(scale.min);
  }
  function labelWidth(chart, v) {
    var dl = chart.config.options.plugins.datalabels, widest = 0;
    chart.config.data.datasets.forEach(function (ds, i) {
      (ds.data || []).forEach(function (d, j) {
        var n = d !== null && typeof d === "object" ? d.x : d;
        if (typeof n !== "number" || (v > 0 ? n <= 0 : n >= 0)) return;
        var text = dl.formatter ? dl.formatter(d, { chart: chart, dataset: ds, datasetIndex: i, dataIndex: j }) : formatNumber(d);
        widest = Math.max(widest, textWidth(String(text), "12px " + cssVar("--chart-mono")));
      });
    });
    return widest;
  }

  function drawBaseline(chart) {
    if (!cartesian(chart)) return;
    var scale = chart.scales[valueAxis(chart)], area = chart.chartArea;
    if (!scale || scale.type !== "linear" || !area) return;
    var horizontal = scale.isHorizontal(), zero = scale.min <= 0 && scale.max >= 0;
    var at = zero ? scale.getPixelForValue(0) : (horizontal ? area.left : area.bottom);
    var c = chart.ctx;
    c.save();
    c.strokeStyle = resolve("var(--chart-reference)"); c.lineWidth = 1;
    c.beginPath();
    if (horizontal) { c.moveTo(Math.round(at) + 0.5, area.top); c.lineTo(Math.round(at) + 0.5, area.bottom); }
    else { c.moveTo(area.left, Math.round(at) + 0.5); c.lineTo(area.right, Math.round(at) + 0.5); }
    c.stroke();
    c.restore();
  }

  var chartLabels = Chart.defaults.plugins.legend.labels.generateLabels;
  function solidSwatches(chart) {
    return chartLabels(chart).map(function (item) {
      var ds = chart.data.datasets[item.datasetIndex];
      var type = ds && (ds.type || chart.config.type);
      if (type === "line" || type === "radar") item.fillStyle = item.strokeStyle = chart.getDatasetMeta(item.datasetIndex).controller.getStyle(undefined, false).borderColor;
      else item.strokeStyle = item.fillStyle;
      item.lineWidth = 0;
      if (ds && dashed(ds)) { item.pointStyle = "line"; item.lineWidth = 2; }
      return item;
    });
  }

  // The wash fades toward the zero line, so an area below zero is washed like one above it.
  function wash(token, opacity) {
    var edge = token ? withOpacity(token, opacity === undefined ? 1 : opacity) : function () { return resolve("var(--chart-muted)"); };
    return function (ctx) {
      var area = ctx.chart.chartArea, meta = ctx.chart.getDatasetMeta(ctx.datasetIndex);
      if (!area || !meta.yScale) return "transparent";
      var zero = Math.min(1, Math.max(0, (meta.yScale.getPixelForValue(0) - area.top) / (area.bottom - area.top)));
      var g = ctx.chart.ctx.createLinearGradient(0, area.top, 0, area.bottom), color = edge();
      g.addColorStop(0, zero > 0 ? color : "transparent");
      g.addColorStop(zero, "transparent");
      g.addColorStop(1, zero < 1 ? color : "transparent");
      return g;
    };
  }

  var OVERLAPPING = { scatter: true, bubble: true, radar: true };
  function palette(n, overlapping) {
    if (n <= 1) return ["var(--chart-ink)"];
    if (n === 2 && !overlapping) return ["var(--chart-ink)", "var(--chart-ink-2)"];
    return seriesTokens();
  }
  function filled(ds) { return ds.fill !== undefined && ds.fill !== false; }
  function dashed(ds) { return Array.isArray(ds.borderDash) && ds.borderDash.length > 0; }
  var REFERENCE_TOKENS = ["var(--chart-reference)", "var(--chart-accent)"];
  function isReference(ds) {
    if (!ds || !dashed(ds)) return false;
    return REFERENCE_TOKENS.some(function (t) { return ds.borderColor === t || sameColor(ds.borderColor, resolve(t)); });
  }
  function colored(ds) { return !!ds.borderColor || (!!ds.backgroundColor && !filled(ds)); }
  function stacked(chart) {
    var scales = chart.config.options.scales || {};
    return Object.keys(scales).some(function (id) { return scales[id] && scales[id].stacked; });
  }
  // Area washes read lighter on a dark page, so they start stronger there.
  var FILL_OPACITY = { bubble: 0.35, radar: 0.2, area: function () { return darkTheme() ? 0.45 : 0.2; } };
  function darkTheme() {
    var theme = root.getAttribute("data-theme");
    return theme === "dark" || (theme !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }
  function withOpacity(token, opacity) {
    return function () {
      var c = resolve(token).match(/[\d.]+/g), a = typeof opacity === "function" ? opacity() : opacity;
      return "rgba(" + c[0] + ", " + c[1] + ", " + c[2] + ", " + (c.length > 3 ? c[3] * a : a) + ")";
    };
  }
  // It runs on every update, so datasets swapped in after the chart was built are colored too.
  function assignSeries(chart) {
    // Only series left to the defaults are counted: a dashed target or a hand-colored dataset is not one.
    var datasets = chart.config.data.datasets, open = datasets.filter(function (ds) { return !colored(ds) && !ds.$harkColor; });
    var areas = open.filter(filled).length, next = 0, solidAreas = stacked(chart);
    var lines = palette(open.length, OVERLAPPING[chart.config.type] || areas > 0 || solidAreas);
    datasets.forEach(function (ds) {
      var type = ds.type || chart.config.type;
      if (PER_POINT[type]) {
        var slices = palette((ds.data || []).length, true);
        if (!ds.backgroundColor) ds.backgroundColor = (ds.data || []).map(function (_, j) { return slices[j % slices.length]; });
        if (!ds.borderColor) ds.borderColor = "var(--fill-tertiary)"; // the chart card's tint, so slices read as separate
        return;
      }
      if (isReference(ds) && ds.order === undefined) ds.order = -1; // drawn over bars, not hidden behind them
      if (ds.pointBackgroundColor && !ds.pointBorderColor) ds.pointBorderColor = ds.pointBackgroundColor;
      if (colored(ds) || ds.$harkColor) {
        if (!ds.borderColor) ds.borderColor = ds.backgroundColor; // marks colored by their fill: the outline and dots match
        if (!ds.backgroundColor && !filled(ds)) ds.backgroundColor = ds.borderColor;
      } else {
        var color = lines[next++ % lines.length];
        ds.borderColor = color; ds.$harkColor = true;
        if (!ds.backgroundColor) ds.backgroundColor = areaColor(ds, type, color, areas, solidAreas);
      }
      if (!ds.pointBackgroundColor) ds.pointBackgroundColor = ds.borderColor;
    });
  }
  function areaColor(ds, type, color, areas, solidAreas) {
    if (filled(ds) && solidAreas) return color;
    if (filled(ds) && areas === 1) return wash();
    if (filled(ds)) return wash(color, FILL_OPACITY.area);
    if (FILL_OPACITY[type]) return withOpacity(color, FILL_OPACITY[type]);
    return color;
  }
  // A dataset's data is values, not colors, so it is never scanned for tokens.
  function tokenSlots(node, slots) {
    var proto = Object.getPrototypeOf(node); // Chart.js builds merged axis options with a null prototype
    if (!Array.isArray(node) && proto !== Object.prototype && proto !== null) return slots;
    Object.keys(node).forEach(function (key) {
      var v = node[key];
      if (isToken(v)) slots.push({ obj: node, key: key, token: v.trim() });
      else if (typeof v === "function" && /color$/i.test(key) && !v.$hark) node[key] = resolvingFunction(v);
      else if (v && typeof v === "object" && !(key === "data" && Array.isArray(v))) tokenSlots(v, slots);
    });
    return slots;
  }
  function resolvingFunction(fn) {
    var out = function () {
      var v = fn.apply(this, arguments);
      return isToken(v) ? resolve(v.trim()) : v;
    };
    out.$hark = true;
    return out;
  }
  function paint(slots) {
    slots.forEach(function (s) { s.obj[s.key] = resolve(s.token); });
  }

  Chart.register({
    id: "harkTheme",
    beforeInit: function (chart) {
      chart.$harkSlots = [];
      applyChrome(chart);
      roomForLabels(chart);
    },
    beforeUpdate: function (chart) {
      assignSeries(chart);
      var fresh = tokenSlots(chart.config.options, tokenSlots(chart.config.data.datasets, []));
      paint(fresh);
      chart.$harkSlots = chart.$harkSlots.concat(fresh);
    },
    afterDataLimits: function (chart, args) { roomAbove(chart, args.scale); },
    beforeDatasetsDraw: function (chart) { drawBaseline(chart); },
    afterDatasetsDraw: function (chart) { drawReferenceNames(chart); drawEndNames(chart); }
  });

  var chartSwatch = Chart.defaults.plugins.tooltip.callbacks.labelColor;
  function outlinedSwatch(ctx) {
    var swatch = chartSwatch.call(this, ctx);
    return { backgroundColor: swatch.backgroundColor, borderColor: resolve("var(--text-primary-on)"), borderWidth: 1 };
  }

  function lastIndex(data) {
    for (var i = data.length - 1; i >= 0; i--) if (data[i] !== null && data[i] !== undefined) return i;
    return -1;
  }
  function textWidth(text, font) {
    var c = textWidth.ctx || (textWidth.ctx = doc.createElement("canvas").getContext("2d"));
    c.font = font;
    return c.measureText(text).width;
  }
  function sansFont() { return "400 12px " + cssVar("--sans"); }
  function drawReferenceNames(chart) {
    var area = chart.chartArea, names = [];
    chart.data.datasets.forEach(function (ds, i) {
      var meta = chart.getDatasetMeta(i), last = lastIndex(ds.data || []);
      if (!isReference(ds) || !ds.label || !chart.isDatasetVisible(i) || last < 0 || !meta.data[last]) return;
      names.push({ text: ds.label, x: area.right - 2, y: meta.data[last].y, color: resolve("var(--chart-ink-2)") });
    });
    drawNames(chart, names);
  }
  function drawNames(chart, names) {
    spread(names, chart.chartArea);
    var c = chart.ctx;
    c.save();
    c.font = sansFont(); c.textAlign = "left"; c.textBaseline = "middle";
    names.forEach(function (n) { c.fillStyle = n.color; c.fillText(n.text, n.x + 8, n.y); });
    c.restore();
  }
  function drawEndNames(chart) {
    var dl = chart.config.options.plugins && chart.config.options.plugins.datalabels;
    if (!dl || !dl.harkEnds) return;
    var names = [];
    chart.data.datasets.forEach(function (ds, i) {
      var meta = chart.getDatasetMeta(i), last = lastIndex(ds.data || []);
      if (isReference(ds) || !chart.isDatasetVisible(i) || last < 0 || !meta.data[last]) return;
      var ctx = { chart: chart, dataset: ds, datasetIndex: i, dataIndex: last };
      var text = dl.formatter ? dl.formatter(ds.data[last], ctx) : ds.label;
      names.push({ text: String(text), x: meta.data[last].x, y: meta.data[last].y, color: meta.controller.getStyle(undefined, false).borderColor });
    });
    drawNames(chart, names);
  }
  function spread(names, area) {
    names.sort(function (a, b) { return a.y - b.y; });
    for (var i = 1; i < names.length; i++) names[i].y = Math.max(names[i].y, names[i - 1].y + LABEL_GAP_PX);
    var over = names.length ? names[names.length - 1].y - area.bottom : 0;
    for (var j = names.length - 1; over > 0 && j >= 0; j--) {
      names[j].y -= over;
      over = j > 0 ? names[j - 1].y + LABEL_GAP_PX - names[j].y : 0;
    }
  }

  var DataLabels = window.ChartDataLabels;
  if (DataLabels) Chart.register(DataLabels);
  function applyLabelDefaults(sans, mono) {
    if (!DataLabels) return;
    Object.assign(Chart.defaults.plugins.datalabels, {
      display: false, clip: false, font: { family: mono, size: 12 }, color: live("var(--chart-ink)"), formatter: formatNumber
    });
  }
  function formatNumber(v) {
    var n = v !== null && typeof v === "object" ? v.y : v;
    return typeof n === "number" ? n.toLocaleString(undefined, { maximumFractionDigits: 2 }) : n;
  }
  function seriesColor(ctx) {
    return ctx.chart.getDatasetMeta(ctx.datasetIndex).controller.getStyle(undefined, false).borderColor;
  }
  function valueOf(ctx) {
    var v = ctx.dataset.data[ctx.dataIndex];
    return v !== null && typeof v === "object" ? (ctx.chart.config.options.indexAxis === "y" ? v.x : v.y) : v;
  }
  function horizontalBars(ctx) { return ctx.chart.config.options.indexAxis === "y"; }
  function dense(test) {
    return function (ctx) { return !isReference(ctx.dataset) && test(ctx) ? "auto" : false; };
  }
  function always(test) {
    return function (ctx) { return !isReference(ctx.dataset) && test(ctx); };
  }
  function onColour(ctx) {
    var fill = ctx.chart.getDatasetMeta(ctx.datasetIndex).controller.getStyle(ctx.dataIndex, false).backgroundColor;
    var c = String(fill).match(/[\d.]+/g) || [];
    var light = c.length >= 3 && (0.299 * c[0] + 0.587 * c[1] + 0.114 * c[2]) > 140;
    return light ? resolve("var(--chart-on-colour)") : resolve("var(--fill-primary-on)");
  }
  function fits(ctx) {
    var el = ctx.chart.getDatasetMeta(ctx.datasetIndex).data[ctx.dataIndex];
    if (!el) return false;
    var p = el.getProps(["x", "y", "base", "width", "height"], true);
    var along = horizontalBars(ctx) ? Math.abs(p.x - p.base) : Math.abs(p.y - p.base);
    var across = horizontalBars(ctx) ? p.height : p.width;
    var text = String(formatNumber(valueOf(ctx)));
    var need = horizontalBars(ctx) ? textWidth(text, "12px " + cssVar("--chart-mono")) + 8 : 16;
    var room = horizontalBars(ctx) ? along : Math.min(along, across);
    return room >= need;
  }
  function everyPoint() { return true; }
  function percentOfTotal(value, ctx) {
    var total = ctx.dataset.data.reduce(function (a, b) { return a + b; }, 0), share = (value / total) * 100;
    return (share < 10 ? share.toFixed(1) : Math.round(share)) + "%";
  }
  var LABELS = {
    values: function () {
      return {
        display: dense(everyPoint), offset: 2, harkRoom: true,
        anchor: function (ctx) { return valueOf(ctx) < 0 ? "start" : "end"; },
        align: function (ctx) {
          var negative = valueOf(ctx) < 0;
          if (horizontalBars(ctx)) return negative ? "left" : "right";
          return negative ? "bottom" : "top";
        }
      };
    },
    inside: function () { return { display: always(fits), anchor: "center", align: "center", color: onColour }; },
    points: function () { return { display: dense(everyPoint), anchor: "center", align: "top", offset: 6, harkRoom: true, harkRight: true }; },
    percent: function () { return { display: dense(everyPoint), formatter: percentOfTotal, color: onColour }; },
    ends: function () { return { display: false, harkEnds: true }; },
    names: function () {
      return {
        display: always(everyPoint), anchor: "center", align: "right", offset: 4, color: live("var(--chart-ink-2)"),
        font: { family: cssVar("--sans"), size: 12 },
        formatter: function (v, ctx) { return (v && v.label) || (ctx.chart.data.labels || [])[ctx.dataIndex] || ""; }
      };
    }
  };
  var labels = {};
  Object.keys(LABELS).forEach(function (name) { Object.defineProperty(labels, name, { get: LABELS[name], enumerable: true }); });
  // A chart that sets its own padding keeps it.
  var LAST_VALUE_ROOM_PX = 24;
  function roomForLabels(chart) {
    var opts = chart.config.options, dl = (opts.plugins && opts.plugins.datalabels) || {};
    chart.$harkRoom = !!dl.harkRoom;
    var names = marginNames(chart, dl);
    var right = names.reduce(function (w, t) { return Math.max(w, textWidth(t, sansFont()) + 12); }, dl.harkRight ? LAST_VALUE_ROOM_PX : 0);
    var layout = opts.layout = opts.layout || {};
    if (!right || (layout.padding !== undefined && typeof layout.padding !== "object")) return;
    var padding = layout.padding = layout.padding || {};
    if (!padding.right) padding.right = right;
  }
  function marginNames(chart, dl) {
    if (!cartesian(chart)) return [];
    return chart.config.data.datasets.map(function (ds, i) {
      if (isReference(ds)) return ds.label || "";
      if (!dl.harkEnds) return "";
      var last = lastIndex(ds.data || []);
      return String(dl.formatter ? dl.formatter(ds.data[last], { chart: chart, dataset: ds, datasetIndex: i, dataIndex: last }) : ds.label || "");
    }).filter(Boolean);
  }

  function categoryFont(sans, mono) {
    return function (ctx) {
      var label = ctx.tick ? String(ctx.tick.label) : "";
      return /^[\d$€£+\-−]/.test(label.trim()) ? { family: mono, size: 12 } : { family: sans, size: 14 };
    };
  }
  // A canvas never asks the browser for a web font, so the chart fonts are loaded here and every chart is
  // redrawn once they arrive; if they cannot load, the charts keep the fallback fonts they already drew in.
  function loadFonts() {
    if (!doc.fonts || !doc.fonts.load) return;
    var sans = cssVar("--sans"), mono = cssVar("--chart-mono");
    var faces = ["400 12px " + sans, "500 12px " + sans, "400 14px " + sans, "400 12px " + mono];
    var redraw = function () { Object.keys(Chart.instances).forEach(function (id) { Chart.instances[id].update("none"); }); };
    Promise.all(faces.map(function (f) { return doc.fonts.load(f); })).then(redraw, redraw);
  }

  function retheme() {
    cache = {};
    applyDefaults();
    Object.keys(Chart.instances).forEach(function (id) {
      var chart = Chart.instances[id];
      paint(chart.$harkSlots || []);
      chart.update("none");
    });
  }
  new MutationObserver(retheme).observe(root, { attributes: true, attributeFilter: ["data-theme"] });
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", retheme);
  applyDefaults();
  loadFonts();

  function parseCSV(text) {
    var rows = [], row = [], field = "", quoted = false;
    for (var i = 0; i < text.length; i++) {
      var ch = text[i];
      if (quoted) {
        if (ch === '"' && text[i + 1] === '"') { field += '"'; i++; }
        else if (ch === '"') quoted = false;
        else field += ch;
      } else if (ch === '"') quoted = true;
      else if (ch === ",") { row.push(field); field = ""; }
      else if (ch === "\n") { row.push(field); rows.push(row); row = []; field = ""; }
      else if (ch !== "\r") field += ch;
    }
    if (field || row.length) { row.push(field); rows.push(row); }
    var head = rows.shift() || [];
    return rows.map(function (r) {
      var o = {};
      head.forEach(function (name, j) { o[name.trim()] = typed(r[j]); });
      return o;
    });
  }
  function typed(cell) {
    var s = (cell == null ? "" : cell).trim();
    if (s === "") return null;
    return isNaN(s) ? s : Number(s);
  }
  function readData(id) {
    var el = doc.getElementById(id);
    if (!el) throw new Error("Hark.data: no element with id " + id);
    return el.type === "text/csv" ? parseCSV(el.textContent.trim()) : JSON.parse(el.textContent);
  }

  window.Hark = Object.assign(window.Hark || {}, { data: readData, color: resolve, wash: wash, labels: labels });
})();
