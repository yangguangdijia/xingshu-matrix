/* 星枢 Matrix — API 文档页
 * 读取 GET /api/v1/openapi.json?envelope=1（XS.api 解包后得到 OpenAPI 3 文档），
 * 按模块分组展示，支持路径/摘要搜索与下载 openapi.json。挂载到 window.XSApiDoc。
 */
(function () {
  "use strict";

  var MODULE_NAMES = {
    cluster: "集群", cicd: "持续交付", bastion: "堡垒机", observability: "可观测",
    hosts: "主机", automation: "自动化", users: "用户", roles: "角色",
    audit: "审计", settings: "系统设置", approvals: "审批"
  };
  var METHODS = ["get", "post", "put", "patch", "delete", "head", "options"];
  var METHOD_COLORS = {
    GET: "#1677ff", POST: "#389e0d", PUT: "#d48806", PATCH: "#08979c",
    DELETE: "#cf1322", HEAD: "#595959", OPTIONS: "#595959"
  };
  var STYLE = [
    ".xs-apidoc{font-size:13px;color:#1f2329;background:#fff}",
    ".xs-apidoc-bar{display:flex;gap:12px;align-items:center;padding:12px 0;border-bottom:1px solid #e5e6eb;margin-bottom:12px}",
    ".xs-apidoc-bar h2{margin:0;font-size:16px;font-weight:600}",
    ".xs-apidoc-meta{color:#86909c;font-size:12px}",
    ".xs-apidoc-search{flex:1;max-width:420px;height:32px;padding:0 10px;border:1px solid #d0d3d9;border-radius:6px;outline:none}",
    ".xs-apidoc-search:focus{border-color:#1677ff}",
    ".xs-apidoc-btn{height:32px;padding:0 14px;border:1px solid #1677ff;border-radius:6px;background:#1677ff;color:#fff;cursor:pointer}",
    ".xs-apidoc-group{border:1px solid #e5e6eb;border-radius:8px;margin-bottom:12px;overflow:hidden}",
    ".xs-apidoc-group>h3{margin:0;padding:10px 14px;font-size:14px;background:#f7f8fa;border-bottom:1px solid #e5e6eb}",
    ".xs-apidoc-group>h3 small{color:#86909c;font-weight:400;margin-left:6px}",
    ".xs-apidoc-row{display:grid;grid-template-columns:72px minmax(220px,2fr) 2fr 2fr 80px;gap:10px;align-items:center;padding:8px 14px;border-top:1px solid #f0f1f3}",
    ".xs-apidoc-row:first-of-type{border-top:0}",
    ".xs-apidoc-method{display:inline-block;min-width:56px;text-align:center;border-radius:4px;color:#fff;font-size:11px;font-weight:600;padding:2px 0}",
    ".xs-apidoc-path{font-family:Menlo,Consolas,monospace;word-break:break-all}",
    ".xs-apidoc-summary{color:#4e5969}",
    ".xs-apidoc-perm code{background:#f2f3f5;border-radius:3px;padding:1px 4px;margin:0 4px 2px 0;display:inline-block;font-size:12px}",
    ".xs-apidoc-tag{display:inline-block;border-radius:10px;padding:1px 8px;font-size:12px}",
    ".xs-apidoc-tag.pub{background:#e8ffea;color:#00a870}",
    ".xs-apidoc-tag.auth{background:#e8f3ff;color:#1677ff}",
    ".xs-apidoc-empty,.xs-apidoc-error{padding:32px;text-align:center;color:#86909c}",
    ".xs-apidoc-error{color:#cf1322}"
  ].join("\n");

  function esc(v) {
    return String(v === undefined || v === null ? "" : v)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function moduleName(key) { return MODULE_NAMES[key] || key; }

  function operations(spec) {
    var out = [];
    var paths = (spec && spec.paths) || {};
    Object.keys(paths).sort().forEach(function (path) {
      var item = paths[path] || {};
      METHODS.forEach(function (m) {
        var op = item[m];
        if (!op || typeof op !== "object") return;
        var access = op["x-xs-access"] || {};
        out.push({
          method: m.toUpperCase(),
          path: path,
          summary: op.summary || op.description || "",
          module: (op.tags && op.tags[0]) || "other",
          isPublic: !!access.public,
          permissions: Array.isArray(access.permissions) ? access.permissions : [],
          anyOf: Array.isArray(access.any_of) ? access.any_of : [],
          envScoped: !!access.env_scoped
        });
      });
    });
    return out;
  }

  function filter(ops, q) {
    q = String(q || "").trim().toLowerCase();
    if (!q) return ops;
    return ops.filter(function (op) {
      return op.path.toLowerCase().indexOf(q) >= 0 || String(op.summary).toLowerCase().indexOf(q) >= 0;
    });
  }

  function group(ops) {
    var map = {}, order = [];
    ops.forEach(function (op) {
      if (!map[op.module]) { map[op.module] = []; order.push(op.module); }
      map[op.module].push(op);
    });
    return order.map(function (k) { return { key: k, name: moduleName(k), ops: map[k] }; });
  }

  function permHtml(op) {
    if (op.isPublic) return "<span class=\"xs-apidoc-meta\">无需权限</span>";
    var parts = [];
    if (op.permissions.length) parts.push(op.permissions.map(function (p) { return "<code>" + esc(p) + "</code>"; }).join(""));
    if (op.anyOf.length) parts.push("<span class=\"xs-apidoc-meta\">任一：</span>" + op.anyOf.map(function (p) { return "<code>" + esc(p) + "</code>"; }).join(""));
    if (op.envScoped) parts.push("<span class=\"xs-apidoc-meta\">（按环境授权）</span>");
    return parts.length ? parts.join(" ") : "<span class=\"xs-apidoc-meta\">登录即可</span>";
  }

  function listHtml(ops) {
    var groups = group(ops);
    if (!groups.length) return "<div class=\"xs-apidoc-empty\">没有匹配的接口</div>";
    return groups.map(function (g) {
      return "<section class=\"xs-apidoc-group\" data-apidoc-module=\"" + esc(g.key) + "\">" +
        "<h3>" + esc(g.name) + "<small>" + g.ops.length + " 个接口</small></h3>" +
        g.ops.map(function (op) {
          var color = METHOD_COLORS[op.method] || "#595959";
          return "<div class=\"xs-apidoc-row\">" +
            "<span><span class=\"xs-apidoc-method\" style=\"background:" + color + "\">" + esc(op.method) + "</span></span>" +
            "<span class=\"xs-apidoc-path\">" + esc(op.path) + "</span>" +
            "<span class=\"xs-apidoc-summary\">" + esc(op.summary) + "</span>" +
            "<span class=\"xs-apidoc-perm\">" + permHtml(op) + "</span>" +
            "<span>" + (op.isPublic
              ? "<span class=\"xs-apidoc-tag pub\">公开</span>"
              : "<span class=\"xs-apidoc-tag auth\">需登录</span>") + "</span>" +
            "</div>";
        }).join("") +
        "</section>";
    }).join("");
  }

  function download(spec) {
    var doc = window.document;
    var blob = new Blob([JSON.stringify(spec, null, 2)], { type: "application/json" });
    var href = URL.createObjectURL(blob);
    var a = doc.createElement("a");
    a.href = href;
    a.download = "openapi.json";
    (doc.body || doc.documentElement).appendChild(a);
    a.click();
    if (a.parentNode) a.parentNode.removeChild(a);
    setTimeout(function () { URL.revokeObjectURL(href); }, 0);
  }

  function render(el) {
    if (!el) return Promise.resolve();
    el.innerHTML = "<div class=\"xs-apidoc\"><div class=\"xs-apidoc-empty\">正在加载接口文档…</div></div>";
    var XS = window.XS;
    if (!XS || typeof XS.api !== "function") {
      el.innerHTML = "<div class=\"xs-apidoc\"><div class=\"xs-apidoc-error\">API 客户端未加载</div></div>";
      return Promise.resolve();
    }
    return XS.api("/openapi.json?envelope=1").then(function (spec) {
      var ops = operations(spec);
      var info = (spec && spec.info) || {};
      el.innerHTML =
        "<style>" + STYLE + "</style>" +
        "<div class=\"xs-apidoc\">" +
        "<div class=\"xs-apidoc-bar\">" +
        "<h2>API 文档</h2>" +
        "<span class=\"xs-apidoc-meta\">" + esc(info.title || "") + (info.version ? " v" + esc(info.version) : "") + " · 共 " + ops.length + " 个接口</span>" +
        "<input class=\"xs-apidoc-search\" data-apidoc-search type=\"search\" placeholder=\"按路径或摘要搜索\">" +
        "<button class=\"xs-apidoc-btn\" data-apidoc-download type=\"button\">下载 openapi.json</button>" +
        "</div>" +
        "<div data-apidoc-list>" + listHtml(ops) + "</div>" +
        "</div>";
      var input = el.querySelector("[data-apidoc-search]");
      var list = el.querySelector("[data-apidoc-list]");
      var btn = el.querySelector("[data-apidoc-download]");
      if (input && list) input.addEventListener("input", function () { list.innerHTML = listHtml(filter(ops, input.value)); });
      if (btn) btn.addEventListener("click", function () { download(spec); });
    }).catch(function (err) {
      el.innerHTML = "<div class=\"xs-apidoc\"><div class=\"xs-apidoc-error\">接口文档加载失败：" + esc((err && err.message) || "未知错误") + "</div></div>";
    });
  }

  window.XSApiDoc = {
    render: render,
    _internal: { esc: esc, operations: operations, filter: filter, group: group, listHtml: listHtml, moduleName: moduleName }
  };
})();
