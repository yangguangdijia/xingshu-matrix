/* 星枢 Matrix   实时核心页适配器（xs-live-core.js）
 * ---------------------------------------------------------------------------
 * 作用：把「用户与权限 / 变更审批 / 主机纳管 / 集群状态 / 总览 / 团队协作」
 *       这些核心页面接到真实 matrix-server API。后端不可用时完全不覆盖原有
 *       演示渲染器，只在右下角显示「演示模式」徽标。
 *
 * 依赖：public/xs-api.js（window.XS）与 index.html 主脚本的全局变量/函数
 *       （routes、CONSOLES、nav、render、phead、esc、toast、navGo、dlg、
 *        closeAll、LEAVES、$、XS）。本文件不接管登录：登录/改密/会话恢复由
 *       index.html 主脚本负责，这里只在 401 时调用 XS.auth.logout 送回登录页。
 *
 * 引入方式（由 Lead 接线，本文件不改 index.html）：
 *   必须在 index.html 的**主脚本之后**加载，例如放在 </body> 之前：
 *     <script src="xs-api.js"></script>            <!-- 已在 head 区域 -->
 *     <script src="xs-live-core.js"></script>      <!-- 新增：主脚本 </script> 之后 -->
 *   也可与 public/xs-live-ops.js 共存（路由键不重叠），两者加载顺序任意：
 *   各自只注册自己的路由键，并通过 window.XS_LIVE 复用徽标/注册表。
 *
 * 覆盖的路由（仅在 XS.ready 解析为 live 后）：
 *   #/sre               总览：/clusters、/hosts、/observability/alerts 汇总
 *   #/iam/<sub>         用户 / 角色+环境策略 / 按角色聚合的用户组+权限目录 /
 *                       登录日志+会话（users、roles、permissions、env-policies、
 *                       audit-logs、auth/sessions）
 *   #/approvals         审批列表（todo/mine/all）、通过、驳回、取消
 *   #/cluster/<sub>     list / nodes / namespaces / workloads / events / logs，
 *                       支持集群选择；#/cluster/hosts 为主机页
 *   #/team              新增页面：成员(/users) + 活动(/audit-logs) +
 *                       研发负载(/devcenter/requirements)，按人聚合
 *
 * 演示模式（无后端）：routes.*.render 一律不覆盖（除本文件新增的 #/team 只在
 * live 模式注册），只显示 #xs-live-badge「演示模式」。
 *
 * 自测（内联纯函数断言，不引依赖）：
 *   node public/xs-live-core.js          —— 运行 30+ 条断言，失败时退出码 1
 *   浏览器打开 ?xs_live_selftest=1        —— 控制台运行同一套断言
 *   node --check public/xs-live-core.js  —— 语法检查
 *
 * 说明：live 模式下本文件会把左侧菜单中「总览 / 变更审批 / 节点 / 工作负载」
 * 这几个受 opsOn() 门禁的叶子标记为 free，否则新部署（未走完演示向导）会被
 * 主脚本的 renderNoCluster 拦截，无法进入实时页面；demo 模式不做该调整。
 * ------------------------------------------------------------------------- */
(function () {
  "use strict";

  var VERSION = "1.0.0";
  var GLOBAL = typeof globalThis !== "undefined" ? globalThis : (typeof window !== "undefined" ? window : this);
  var IS_NODE = typeof process !== "undefined" && !!process.versions && !!process.versions.node && typeof window === "undefined";

  /* ================================================================
   * 1. 纯函数（可单测；不触碰 DOM/window）
   * ================================================================ */

  function escLocal(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function decodePart(s) {
    try { return decodeURIComponent(s); } catch (e) { return String(s == null ? "" : s); }
  }

  // "#/cluster/nodes/3?x" -> { key:"cluster", sub:"nodes", sel:"3" }
  function parseHash(hash) {
    var h = String(hash == null ? "" : hash).replace(/^#/, "");
    if (h.charAt(0) === "/") h = h.slice(1);
    if (!h) return { key: "sre", sub: "", sel: "" };
    var segs = h.split("?")[0].split("/");
    return {
      key: segs[0] || "sre",
      sub: segs[1] ? decodePart(segs[1]) : "",
      sel: segs[2] ? decodePart(segs[2]) : ""
    };
  }

  function clusterResourceHash(routeKey, routeSub, clusterId) {
    var key = String(routeKey || "cluster").replace(/[^a-z0-9-]/gi, "");
    var sub = String(routeSub || "").replace(/[^a-z0-9-]/gi, "");
    var hash = "#/" + (key || "cluster");
    if (sub) hash += "/" + sub;
    if (clusterId != null && String(clusterId)) hash += "/" + encodeURIComponent(String(clusterId));
    return hash;
  }

  function networkResourcePlan(view) {
    if (view === "expose") return { title: "对外服务", endpoint: "ingresses", resource: "Ingress" };
    return { title: "Service", endpoint: "services", resource: "Service" };
  }

  // 兼容数组 / {items:[]} / {data:[]} 三种列表返回
  function itemsOf(data) {
    if (!data) return [];
    if (Array.isArray(data)) return data;
    if (Array.isArray(data.items)) return data.items;
    if (Array.isArray(data.data)) return data.data;
    return [];
  }

  function totalOf(data, items) {
    if (data && typeof data.total === "number" && isFinite(data.total)) return data.total;
    return Array.isArray(items) ? items.length : 0;
  }

  function pad2(n) { return (n < 10 ? "0" : "") + n; }

  function toDate(v) {
    if (v == null || v === "") return null;
    if (v instanceof Date) return isNaN(v.getTime()) ? null : v;
    if (typeof v === "number") {
      var d = new Date(v < 1e12 ? v * 1000 : v);
      return isNaN(d.getTime()) ? null : d;
    }
    var d2 = new Date(String(v));
    return isNaN(d2.getTime()) ? null : d2;
  }

  function fmtTime(v) {
    var d = toDate(v);
    if (!d) return "—";
    return d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + " " +
      pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + ":" + pad2(d.getSeconds());
  }

  function fmtAgo(v) {
    var d = toDate(v);
    if (!d) return "—";
    var s = Math.round((Date.now() - d.getTime()) / 1000);
    if (s < 0) s = 0;
    if (s < 60) return "刚刚";
    if (s < 3600) return Math.floor(s / 60) + " 分钟前";
    if (s < 86400) return Math.floor(s / 3600) + " 小时前";
    return Math.floor(s / 86400) + " 天前";
  }

  function fmtBytes(n) {
    n = Number(n);
    if (!isFinite(n) || n < 0) return "—";
    var units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"], i = 0;
    while (n >= 1024 && i < units.length - 1) { n = n / 1024; i++; }
    return (i === 0 ? String(Math.round(n)) : n.toFixed(1)) + " " + units[i];
  }

  function pctOf(used, total) {
    var u = Number(used), t = Number(total);
    if (!isFinite(u) || !isFinite(t) || t <= 0) return null;
    return Math.max(0, Math.min(100, Math.round((u / t) * 100)));
  }

  function truncate(s, n) {
    var v = String(s == null ? "" : s);
    return v.length > n ? v.slice(0, Math.max(0, n - 1)) + "…" : v;
  }

  function prettyJSON(v) {
    try { return JSON.stringify(v, null, 2); } catch (e) { return String(v); }
  }

  function envNormLocal(e) { return e === "dev" ? "开发" : (e == null ? "" : String(e)); }

  function envMatchLocal(env, navEnv) {
    if (!navEnv || navEnv === "全部") return true;
    return envNormLocal(env) === envNormLocal(navEnv);
  }

  // 集群选择：数字 = id，其它 = name
  function resolveCluster(list, sel) {
    if (!sel) return null;
    var s = String(sel), n = /^\d+$/.test(s) ? Number(s) : NaN, i, c;
    for (i = 0; i < (list || []).length; i++) {
      c = list[i] || {};
      if (!isNaN(n) && Number(c.id) === n) return c;
      if (c.name && String(c.name) === s) return c;
    }
    return null;
  }

  function ticketIdOf(err) {
    if (!err) return 0;
    var p = err.approval || (err.code === 40304 ? err.details : null);
    var t = p && p.ticket ? p.ticket : (p && p.id ? p : null);
    return t && t.id ? Number(t.id) : 0;
  }

  var TICKET_STATUS = {
    pending: ["待审批", "warn"],
    approved: ["已通过", "info"],
    executed: ["已执行", "ok"],
    rejected: ["已驳回", "bad"],
    cancelled: ["已取消", "muted"],
    failed: ["执行失败", "bad"]
  };
  function ticketStatus(v) { return TICKET_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  var SEVERITY_TONE = {
    critical: "bad", fatal: "bad", p0: "bad", severe: "bad", error: "bad",
    warning: "warn", warn: "warn", p1: "warn",
    info: "info", notice: "info", p2: "info",
    p3: "muted", debug: "muted"
  };
  function severityTone(v) { return SEVERITY_TONE[String(v == null ? "" : v).toLowerCase()] || "muted"; }

  var ALERT_STATUS = {
    firing: ["触发中", "bad"],
    resolved: ["已恢复", "ok"],
    suppressed: ["已抑制", "muted"],
    pending: ["待触发", "warn"],
    acked: ["已认领", "warn"],
    open: ["待处理", "warn"],
    closed: ["已关闭", "ok"]
  };
  function alertStatus(v) { return ALERT_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  var USER_STATUS = {
    active: ["正常", "ok"],
    disabled: ["已禁用", "muted"],
    locked: ["已锁定", "warn"]
  };
  function userStatus(v) { return USER_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  var CLUSTER_STATUS = {
    connected: ["已连接", "ok"],
    error: ["连接异常", "bad"],
    unknown: ["未知", "muted"]
  };
  function clusterStatus(v) { return CLUSTER_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  var RESULT_STATUS = {
    success: ["成功", "ok"],
    failure: ["失败", "bad"]
  };
  function resultStatus(v) { return RESULT_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  var HOST_TASK_STATUS = {
    running: ["执行中", "warn"],
    success: ["成功", "ok"],
    failed: ["失败", "bad"],
    timeout: ["超时", "bad"]
  };
  function hostTaskStatus(v) { return HOST_TASK_STATUS[String(v == null ? "" : v).toLowerCase()] || [String(v == null || v === "" ? "—" : v), "muted"]; }

  function toneClass(t) {
    return "x-" + ({ ok: "green", warn: "orange", bad: "red", info: "blue", muted: "grey" }[t] || "grey");
  }

  // 团队协作页：成员 + 审计活动 + 研发需求 按人聚合
  function aggregateTeam(members, audits, reqs) {
    var rows = [], byId = {}, byName = {};
    (members || []).forEach(function (u) {
      u = u || {};
      var row = {
        id: u.id,
        username: u.username || "",
        name: u.display_name || u.username || "",
        status: u.status || "",
        roles: (u.roles || []).map(function (r) { return (r && (r.name || r.code)) || ""; }).filter(Boolean),
        last_login: u.last_login_at || null,
        last_login_ip: u.last_login_ip || "",
        activity: 0,
        last_activity: null,
        reqs: 0,
        open_reqs: 0
      };
      rows.push(row);
      if (row.id != null) byId[String(row.id)] = row;
      if (row.username) byName[row.username.toLowerCase()] = row;
    });
    (audits || []).forEach(function (a) {
      a = a || {};
      var key = String(a.username || "").toLowerCase(), row = key ? byName[key] : null;
      if (!row) return;
      row.activity += 1;
      var t = toDate(a.created_at);
      if (t && (!row.last_activity || t.getTime() > row.last_activity.getTime())) row.last_activity = t;
    });
    (reqs || []).forEach(function (r) {
      r = r || {};
      var row = r.created_by == null ? null : byId[String(r.created_by)];
      if (!row) return;
      row.reqs += 1;
      var st = String(r.status || "").toLowerCase();
      if (st !== "done" && st !== "closed" && st !== "cancelled" && st !== "已完成" && st !== "已关闭" && st !== "已取消") row.open_reqs += 1;
    });
    rows.sort(function (a, b) { return b.activity - a.activity || String(a.username).localeCompare(String(b.username)); });
    return rows;
  }

  function summarizeAlerts(alerts) {
    var out = { total: 0, firing: 0, bySeverity: {} };
    (alerts || []).forEach(function (a) {
      a = a || {};
      out.total += 1;
      if (String(a.status || "").toLowerCase() === "firing") out.firing += 1;
      var s = String(a.severity || (a.labels && a.labels.severity) || "unknown").toLowerCase();
      out.bySeverity[s] = (out.bySeverity[s] || 0) + 1;
    });
    return out;
  }

  function roleMembers(roles, users) {
    return (roles || []).map(function (r) {
      var members = (users || []).filter(function (u) {
        return (u.roles || []).some(function (x) { return x && (Number(x.id) === Number(r.id) || String(x.code || "") === String(r.code || "")); });
      }).map(function (u) { return u.username; });
      return { role: r, members: members };
    });
  }

  function groupPermissions(perms) {
    var groups = {};
    (perms || []).forEach(function (p) {
      var mod = (p && p.module) || String((p && p.code) || "").split(":")[0] || "other";
      (groups[mod] = groups[mod] || []).push(p);
    });
    return groups;
  }

  var AI_SENSITIVE_KEY = /(?:password|passwd|token|credential|private[_-]?key|api[_-]?key|authorization|secret[_-]?(?:value|data|token))/i;

  function aiPromptContainsSensitiveValue(text, strictNoValues) {
    var s = String(text || "");
    if (/(?:password|passwd|token|credential|private[_ -]?key|api[_ -]?key|authorization|密码|口令|令牌|凭据|私钥|密钥)\s*["']?\s*(?:=|:|：|为|是)\s*["']?\S{4,}/i.test(s) ||
        /(?:bearer)\s+[A-Za-z0-9._~+/-]{12,}/i.test(s)) return true;
    return !!strictNoValues && /(?:value|值)\s*["']?\s*(?:=|:|：|为|是)\s*["']?\S{4,}/i.test(s);
  }

  function aiDraftTree(value, depth) {
    if (depth > 12) return undefined;
    if (value === null || typeof value === "boolean" || typeof value === "number") {
      return typeof value === "number" && !isFinite(value) ? undefined : value;
    }
    if (typeof value === "string") return value.length <= 4000 ? value : undefined;
    if (Array.isArray(value)) {
      if (value.length > 100) return undefined;
      var list = [];
      for (var i = 0; i < value.length; i++) {
        var next = aiDraftTree(value[i], depth + 1);
        if (next === undefined) return undefined;
        list.push(next);
      }
      return list;
    }
    if (!value || typeof value !== "object") return undefined;
    var out = {};
    var keys = Object.keys(value);
    if (keys.length > 100) return undefined;
    for (var j = 0; j < keys.length; j++) {
      var key = keys[j];
      if (AI_SENSITIVE_KEY.test(key) || key === "__proto__" || key === "constructor" || key === "prototype") continue;
      var child = aiDraftTree(value[key], depth + 1);
      if (child !== undefined) Object.defineProperty(out, key, { value: child, enumerable: true, configurable: true, writable: true });
    }
    return out;
  }

  function sanitizeAIDraft(fields, allowlist) {
    if (!fields || typeof fields !== "object" || Array.isArray(fields) || !allowlist || typeof allowlist !== "object") return {};
    var out = {};
    Object.keys(allowlist).forEach(function (key) {
      var rule = allowlist[key] || {};
      if (rule.sensitive || AI_SENSITIVE_KEY.test(key) || !Object.prototype.hasOwnProperty.call(fields, key)) return;
      var value = fields[key], next;
      if (rule.type === "string" || rule.type === "enum") {
        if (typeof value !== "string") return;
        next = value.trim();
        if (rule.maxLength && next.length > rule.maxLength) return;
        if (!next && !rule.allowEmpty) return;
        if (rule.type === "enum" && (rule.values || []).indexOf(next) < 0) return;
      } else if (rule.type === "boolean") {
        if (typeof value !== "boolean") return;
        next = value;
      } else if (rule.type === "array") {
        if (!Array.isArray(value)) return;
        next = aiDraftTree(value, 0);
        if (!Array.isArray(next)) return;
        if (rule.values) {
          next = next.filter(function (item, index, list) {
            return typeof item === "string" && rule.values.indexOf(item) >= 0 && list.indexOf(item) === index;
          });
        }
        if (!next.length && !rule.allowEmpty) return;
      } else if (rule.type === "string_map") {
        if (!value || typeof value !== "object" || Array.isArray(value)) return;
        next = {};
        Object.keys(value).forEach(function (mapKey) {
          var mapValue = value[mapKey];
          if (!AI_SENSITIVE_KEY.test(mapKey) && mapKey !== "__proto__" && mapKey !== "constructor" &&
              mapKey !== "prototype" && typeof mapValue === "string" && mapValue.length <= 4000) {
            Object.defineProperty(next, mapKey, { value: mapValue, enumerable: true, configurable: true, writable: true });
          }
        });
        if (!Object.keys(next).length && !rule.allowEmpty) return;
      } else if (rule.type === "object") {
        if (!value || typeof value !== "object" || Array.isArray(value)) return;
        next = aiDraftTree(value, 0);
        if (!next || Array.isArray(next)) return;
        if (!Object.keys(next).length && !rule.allowEmpty) return;
      } else return;
      Object.defineProperty(out, key, { value: next, enumerable: true, configurable: true, writable: true });
    });
    return out;
  }

  function approvalScopeDefault(canReadAll) { return canReadAll ? "all" : "todo"; }

  var PURE = {
    esc: escLocal,
    parseHash: parseHash,
    clusterResourceHash: clusterResourceHash,
    networkResourcePlan: networkResourcePlan,
    itemsOf: itemsOf,
    totalOf: totalOf,
    fmtTime: fmtTime,
    fmtAgo: fmtAgo,
    fmtBytes: fmtBytes,
    pctOf: pctOf,
    truncate: truncate,
    toDate: toDate,
    resolveCluster: resolveCluster,
    ticketIdOf: ticketIdOf,
    ticketStatus: ticketStatus,
    severityTone: severityTone,
    alertStatus: alertStatus,
    userStatus: userStatus,
    clusterStatus: clusterStatus,
    resultStatus: resultStatus,
    hostTaskStatus: hostTaskStatus,
    toneClass: toneClass,
    aggregateTeam: aggregateTeam,
    summarizeAlerts: summarizeAlerts,
    roleMembers: roleMembers,
    groupPermissions: groupPermissions,
    aiPromptContainsSensitiveValue: aiPromptContainsSensitiveValue,
    sanitizeAIDraft: sanitizeAIDraft,
    envMatchLocal: envMatchLocal,
    approvalScopeDefault: approvalScopeDefault,
    prettyJSON: prettyJSON
  };

  /* ================================================================
   * 2. 内联纯函数自测（node public/xs-live-core.js）
   * ================================================================ */

  function selfTest(verbose) {
    var passed = 0, failed = [];
    function ok(name, cond, got) {
      if (cond) { passed += 1; if (verbose) console.log("  + " + name); }
      else { failed.push(name); if (verbose) console.log("  - FAIL " + name + " (got: " + String(got) + ")"); }
    }
    function eq(name, got, want) {
      var g = JSON.stringify(got), w = JSON.stringify(want);
      ok(name + " == " + w, g === w, got);
    }

    eq("parseHash empty", parseHash(""), { key: "sre", sub: "", sel: "" });
    eq("parseHash sre", parseHash("#/sre"), { key: "sre", sub: "", sel: "" });
    eq("parseHash cluster nodes", parseHash("#/cluster/nodes/3"), { key: "cluster", sub: "nodes", sel: "3" });
    eq("parseHash iam roles", parseHash("#/iam/roles"), { key: "iam", sub: "roles", sel: "" });
    eq("parseHash cluster hosts", parseHash("#/cluster/hosts"), { key: "cluster", sub: "hosts", sel: "" });
    eq("parseHash encoded", parseHash("#/cluster/namespaces/a%20b"), { key: "cluster", sub: "namespaces", sel: "a b" });

    eq("itemsOf page", itemsOf({ items: [1, 2], total: 2 }), [1, 2]);
    eq("itemsOf array", itemsOf([1]), [1]);
    eq("itemsOf null", itemsOf(null), []);
    eq("totalOf", totalOf({ items: [1, 2], total: 5 }, [1, 2]), 5);
    eq("totalOf fallback", totalOf({ items: [1, 2] }, [1, 2]), 2);

    eq("resolveCluster id", (resolveCluster([{ id: 3, name: "prod" }], "3") || {}).name, "prod");
    eq("resolveCluster name", (resolveCluster([{ id: 3, name: "prod" }], "prod") || {}).id, 3);
    eq("resolveCluster miss", resolveCluster([{ id: 3, name: "prod" }], "9"), null);

    eq("fmtTime local", fmtTime(new Date(2026, 9, 18, 9, 5, 7)), "2026-10-18 09:05:07");
    eq("fmtTime empty", fmtTime(null), "—");
    eq("fmtBytes 1536", fmtBytes(1536), "1.5 KiB");
    eq("fmtBytes 0", fmtBytes(0), "0 B");
    eq("pctOf", pctOf(1, 3), 33);
    eq("pctOf zero", pctOf(1, 0), null);
    eq("truncate", truncate("abcdef", 4), "abc…");

    eq("ticketStatus pending", ticketStatus("pending"), ["待审批", "warn"]);
    eq("ticketStatus unknown", ticketStatus("weird"), ["weird", "muted"]);
    eq("severityTone P0", severityTone("P0"), "bad");
    eq("severityTone critical", severityTone("critical"), "bad");
    eq("severityTone info", severityTone("info"), "info");
    eq("alertStatus firing", alertStatus("firing"), ["触发中", "bad"]);
    eq("userStatus disabled", userStatus("disabled"), ["已禁用", "muted"]);
    eq("clusterStatus error", clusterStatus("error"), ["连接异常", "bad"]);
    eq("hostTaskStatus timeout", hostTaskStatus("timeout"), ["超时", "bad"]);
    eq("toneClass", toneClass("warn"), "x-orange");

    eq("ticketIdOf pending", ticketIdOf({ code: 40304, approval: { ticket: { id: 7 } } }), 7);
    eq("ticketIdOf none", ticketIdOf(new Error("x")), 0);

    var team = aggregateTeam(
      [
        { id: 1, username: "alice", display_name: "Alice", status: "active", roles: [{ id: 1, code: "admin", name: "管理员" }] },
        { id: 2, username: "bob", display_name: "Bob", status: "active", roles: [] },
        { id: 3, username: "carol", display_name: "Carol", status: "disabled", roles: [] }
      ],
      [
        { username: "alice", created_at: "2026-10-18T09:00:00Z" },
        { username: "alice", created_at: "2026-10-18T10:00:00Z" },
        { username: "bob", created_at: "2026-10-18T08:00:00Z" },
        { username: "ghost", created_at: "2026-10-18T08:00:00Z" }
      ],
      [
        { id: 1, status: "todo", created_by: 1 },
        { id: 2, status: "done", created_by: 1 },
        { id: 3, status: "todo", created_by: 2 },
        { id: 4, status: "todo", created_by: 99 }
      ]
    );
    eq("team rows", team.length, 3);
    eq("team alice activity", team[0].username + ":" + team[0].activity, "alice:2");
    eq("team alice reqs/open", team[0].reqs + "/" + team[0].open_reqs, "2/1");
    eq("team bob activity", team[1].username + ":" + team[1].activity, "bob:1");
    eq("team carol empty", team[2].activity + "/" + team[2].reqs, "0/0");

    var sum = summarizeAlerts([
      { status: "firing", severity: "critical" },
      { status: "firing", severity: "warning" },
      { status: "resolved", severity: "critical" }
    ]);
    eq("alerts total/firing", sum.total + "/" + sum.firing, "3/2");
    eq("alerts by severity critical", sum.bySeverity.critical, 2);

    var roleAgg = roleMembers(
      [{ id: 1, code: "admin" }, { id: 2, code: "ops" }],
      [{ username: "alice", roles: [{ id: 1, code: "admin" }] }, { username: "bob", roles: [{ id: 2, code: "ops" }, { id: 1, code: "admin" }] }]
    );
    eq("roleMembers admin", roleAgg[0].members, ["alice", "bob"]);
    eq("roleMembers ops", roleAgg[1].members, ["bob"]);

    var permGroups = groupPermissions([{ module: "cluster", code: "cluster:read" }, { module: "cluster", code: "node:read" }, { module: "hosts", code: "host:read" }]);
    eq("groupPermissions modules", Object.keys(permGroups).sort(), ["cluster", "hosts"]);
    eq("groupPermissions cluster", permGroups.cluster.length, 2);

    eq("envMatchLocal all", envMatchLocal("prod", "全部"), true);
    eq("envMatchLocal dev", envMatchLocal("dev", "开发"), true);
    eq("envMatchLocal mismatch", envMatchLocal("prod", "测试"), false);
    eq("approvalScopeDefault", approvalScopeDefault(false), "todo");

    var line = "[xs-live-core] 自测：" + passed + " 通过" + (failed.length ? "，" + failed.length + " 失败" : "，全部通过");
    console.log(line);
    if (failed.length && verbose) console.log("[xs-live-core] 失败项：" + failed.join("、"));
    return { passed: passed, failed: failed };
  }

  var CORE = { version: VERSION, pure: PURE, selfTest: selfTest };
  GLOBAL.XS_LIVE_CORE = CORE;

  if (IS_NODE) {
    var result = selfTest(true);
    if (result.failed.length) process.exitCode = 1;
    try { if (typeof module !== "undefined" && module.exports) module.exports = CORE; } catch (e) {}
    return;
  }

  /* ================================================================
   * 3. 浏览器运行时：共享命名空间 / 徽标 / DOM 与 API 辅助
   * ================================================================ */

  var LIVE = GLOBAL.XS_LIVE;
  if (!LIVE || typeof LIVE !== "object") LIVE = GLOBAL.XS_LIVE = {};
  LIVE.version = LIVE.version || VERSION;
  LIVE.files = LIVE.files || {};
  LIVE.files["xs-live-core.js"] = VERSION;
  LIVE.pure = LIVE.pure || PURE;
  if (typeof LIVE.onLive !== "function") LIVE.onLive = function (cb) { onLive(cb); };
  if (typeof LIVE.repaint !== "function") LIVE.repaint = function () { repaint(); };

  var SEQ = 0;
  var IAM_USERS = [], IAM_ROLES = [];
  var WAITERS = [];
  var REFRESHING = false;

  function bump() { SEQ += 1; return SEQ; }
  function alive(seq, el) {
    var d = document;
    return seq === SEQ && !!el && d.getElementById("view") === el;
  }

  function docEl() { return typeof document !== "undefined" ? document : null; }

  function xs() { return typeof window !== "undefined" ? window.XS : null; }
  function isLive() { var x = xs(); return !!(x && x.mode === "live"); }
  function apiBase() { var x = xs(); return (x && x.base) || "/api/v1"; }

  function $id(id) {
    try { if (typeof $ === "function") return $(id); } catch (e) {}
    var d = docEl();
    return d ? d.getElementById(id) : null;
  }

  function gEsc(s) {
    try { if (typeof esc === "function") return esc(s); } catch (e) {}
    return escLocal(s);
  }

  function pheadSafe(title, desc, right) {
    try { if (typeof phead === "function") return phead(title, desc, right || ""); } catch (e) {}
    return '<div class="x-sethead"><div><h1 class="x-h1">' + gEsc(title) + '</h1><p class="x-desc">' + (desc || "") +
      '</p></div><div class="x-phacts">' + (right || "") + '</div></div>';
  }

  function toastMsg(m) {
    try { if (typeof toast === "function") { toast(m); return; } } catch (e) {}
    try { console.log("[xs-live-core] " + m); } catch (e2) {}
  }

  function goHash(h) {
    try { if (typeof navGo === "function") { navGo(h); return; } } catch (e) {}
    if (typeof location !== "undefined") location.hash = h;
  }

  function repaint() {
    try { if (typeof render === "function") { render(); return; } } catch (e) { console.error("[xs-live-core] render", e); }
  }

  function closeModal() {
    try { if (typeof closeAll === "function") { closeAll(); return; } } catch (e) {}
    var d = docEl();
    var m = d && d.getElementById("modal");
    if (m) m.hidden = true;
    var mask = d && d.getElementById("mask");
    if (mask) mask.hidden = true;
  }

  function openModal(title, body, foot, dark) {
    try {
      if (typeof dlg === "function") { dlg(title, body, foot, dark); return $id("modal"); }
    } catch (e) { console.error("[xs-live-core] dlg", e); }
    return null;
  }

  function tagHtml(text, tone) {
    return '<span class="x-tag x-dot x-pill ' + toneClass(tone) + '">' + gEsc(text) + '</span>';
  }

  function statusTag(pair) { return tagHtml(pair[0], pair[1]); }

  function mono(s) { return '<span class="x-mono">' + gEsc(s) + '</span>'; }

  function refreshButton(label) {
    return '<button type="button" class="x-btn" data-live-refresh>' + gEsc(label || "刷新") + '</button>';
  }

  function shell(title, desc, actions) {
    return pheadSafe(title, desc, (actions || "") + refreshButton());
  }

  function loadingPanel(text) {
    return '<section class="x-panel" aria-busy="true"><p class="x-frac" style="margin:0">' + gEsc(text || "正在加载…") + '</p></section>';
  }

  function errorPanel(err, retryLabel) {
    var msg = errText(err);
    return '<section class="x-panel"><p class="x-aerr" role="alert" style="margin:0 0 12px">' + gEsc(msg) + '</p>' +
      '<button type="button" class="x-btn" data-live-retry>' + gEsc(retryLabel || "重试") + '</button></section>';
  }

  function emptyPanel(title, desc) {
    return '<section class="x-panel x-nodata"><strong>' + gEsc(title) + '</strong><p>' + gEsc(desc || "") + '</p></section>';
  }

  function panelHtml(title, meta, body, tools) {
    return '<section class="x-panel"><div class="x-iamhd"><h2 class="x-h2">' + gEsc(title) + '</h2>' +
      '<span class="x-frac">' + gEsc(meta || "") + '</span>' + (tools || "") + '</div>' + body + '</section>';
  }

  function toolbarHtml(inner) { return '<div class="x-tbar">' + (inner || "") + '</div>'; }

  function searchInput(ph) {
    return '<input class="x-input x-search" type="search" data-live-q placeholder="' + gEsc(ph || "搜索") +
      '" aria-label="' + gEsc(ph || "搜索") + '" />';
  }

  // items: [{v,t}] 或 ["a","b"]
  function selectHtml(items, cur, attrs) {
    var html = '<select class="x-select" style="width:auto"' + (attrs ? " " + attrs : "") + ">";
    (items || []).forEach(function (o) {
      var v = (o && typeof o === "object" && "v" in o) ? o.v : o;
      var t = (o && typeof o === "object" && "t" in o) ? o.t : o;
      var sel = String(v) === String(cur == null ? "" : cur) ? " selected" : "";
      html += '<option value="' + gEsc(v) + '"' + sel + ">" + gEsc(t) + "</option>";
    });
    return html + "</select>";
  }

  function tableHtml(cols, rows) {
    var head = (cols || []).map(function (c) { return "<th scope=\"col\">" + c + "</th>"; }).join("");
    var body = (rows || []).map(function (cells) {
      return "<tr>" + (cells || []).map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>";
    }).join("");
    return '<div style="overflow-x:auto" data-live-tablewrap><table class="x-table x-table--plain"><thead><tr>' +
      head + "</tr></thead><tbody>" + body + "</tbody></table></div>";
  }

  function statCards(cards) {
    var html = '<div class="x-cards x-cards4">';
    (cards || []).forEach(function (c) {
      var inner = "<p>" + gEsc(c.label) + "</p><strong>" + gEsc(c.value) + "</strong>" +
        '<small class="x-frac">' + gEsc(c.sub || "") + "</small>";
      if (c.href) html += '<a class="x-card" href="' + gEsc(c.href) + '" style="text-decoration:none;color:inherit">' + inner + "</a>";
      else html += '<div class="x-card">' + inner + "</div>";
    });
    return html + "</div>";
  }

  function barHtml(pct, text, neutral) {
    var p = pct == null ? null : pct;
    if (p == null) return '<span class="x-frac">—</span>';
    var hot = neutral ? "" : p >= 85 ? " x-hot" : p >= 75 ? " x-low" : "";
    return '<div class="x-res" style="min-width:110px"><span class="x-prog' + hot + '" role="img" aria-label="' + gEsc(text || (p + "%")) +
      '"><i style="width:' + p + '%"></i></span><span class="x-frac">' + gEsc(text || (p + "%")) + "</span></div>";
  }

  function actBtn(label, act, id, opts) {
    opts = opts || {};
    var dis = opts.disabled ? " disabled" : "";
    var title = opts.title ? ' title="' + gEsc(opts.title) + '"' : "";
    return '<button type="button" class="x-act" data-act="' + gEsc(act) + '" data-id="' + gEsc(id == null ? "" : id) + '"' + dis + title + ">" + gEsc(label) + "</button>";
  }

  function bindCommon(el) {
    if (!el) return;
    Array.prototype.forEach.call(el.querySelectorAll("[data-live-refresh],[data-live-retry]"), function (b) {
      b.onclick = function (e) { e.preventDefault(); repaint(); };
    });
  }

  function bindActs(el, handler) {
    Array.prototype.forEach.call(el.querySelectorAll("[data-act]"), function (b) {
      b.onclick = function (e) {
        e.preventDefault();
        e.stopPropagation();
        try { handler(b.getAttribute("data-act"), b.getAttribute("data-id"), b); }
        catch (err) { console.error("[xs-live-core] action", err); toastMsg("操作失败：" + ((err && err.message) || err)); }
      };
    });
  }

  function bindClusterSelect(el) {
    Array.prototype.forEach.call(el.querySelectorAll("[data-live-cluster-select]"), function (s) {
      s.onchange = function () {
        var routeKey = s.getAttribute("data-route-key") || "cluster";
        var sub = s.getAttribute("data-sub") || "nodes";
        goHash(clusterResourceHash(routeKey, sub, s.value));
      };
    });
  }

  function mount(el, html) {
    el.innerHTML = html;
    bindCommon(el);
    filterTable(el);
  }

  // 客户端筛选：data-live-q 搜索 + select[data-live-filter] 精确匹配行 data-f-<attr>
  function filterTable(el) {
    var wrap = el.querySelector("[data-live-tablewrap]");
    if (!wrap) return;
    var input = el.querySelector("[data-live-q]");
    var none = el.querySelector("[data-live-none]");
    var rows = Array.prototype.slice.call(wrap.querySelectorAll("tbody tr"));
    var selects = Array.prototype.slice.call(el.querySelectorAll("select[data-live-filter]"));
    var run = function () {
      var q = input ? String(input.value || "").trim().toLowerCase() : "";
      var n = 0;
      rows.forEach(function (tr) {
        var ok = !q || String(tr.textContent || "").toLowerCase().indexOf(q) >= 0;
        if (!ok) { tr.hidden = true; return; }
        selects.forEach(function (s) {
          if (!s.value) return;
          var attr = s.getAttribute("data-attr");
          var want = s.value;
          var got = attr ? tr.getAttribute("data-f-" + attr) : null;
          if (got == null) { var col = +(s.getAttribute("data-col") || -1); got = col >= 0 && tr.cells[col] ? tr.cells[col].textContent.trim() : ""; }
          if (String(got) !== String(want)) ok = false;
        });
        tr.hidden = !ok;
        if (ok) n += 1;
      });
      if (none) none.hidden = n > 0;
    };
    if (input) input.oninput = run;
    selects.forEach(function (s) { s.onchange = run; });
    run();
  }

  function envOptions(list) {
    return [{ v: "", t: "全部环境" }].concat((list || []).map(function (e) { return { v: e, t: envNormLocal(e) }; }));
  }

  /* ---------------- API / 错误处理 ---------------- */

  function isAuthErr(err) {
    var x = xs();
    return !!(x && typeof x.isAuth === "function" && x.isAuth(err));
  }

  function pendingInfo(err) {
    if (!err || err.code !== 40304) return null;
    return err.approval || err.details || { ticket: null };
  }

  function errText(err) {
    if (!err) return "请求失败";
    if (isAuthErr(err)) return "登录已过期，请重新登录";
    var message = String(err.message || "");
    if (/\/(?:api\/)?v\d+(?:\/|$)|\b(?:GET|POST|PUT|PATCH|DELETE|HEAD)\s+\/\S+/i.test(message)) {
      return "管理服务暂时无法完成请求" + (err.status ? "（HTTP " + err.status + "）" : "") + "，请稍后重试";
    }
    return message || ("请求失败（HTTP " + (err.status || "?") + "）");
  }

  var KICKING = false;
  function kickLogin() {
    if (KICKING) return;
    KICKING = true;
    toastMsg("登录已过期，请重新登录");
    var done = function () {
      KICKING = false;
      try { if (typeof location !== "undefined") location.hash = "#/login"; } catch (e) {}
    };
    try {
      var x = xs();
      if (x && x.auth && typeof x.auth.logout === "function") { x.auth.logout().then(done, done); return; }
      if (x && typeof x.clearSession === "function") x.clearSession();
    } catch (e) {}
    done();
  }

  // 统一请求：返回 data；401 -> 回登录；202 审批 -> 抛 XS ApiError（阻止渲染）
  function apiGet(path, query) {
    var x = xs();
    if (!x) return Promise.reject(new Error("xs-api.js 未加载，无法访问后端"));
    return x.get(path, query);
  }
  function apiPost(path, body) {
    var x = xs();
    if (!x) return Promise.reject(new Error("xs-api.js 未加载，无法访问后端"));
    return x.post(path, body === undefined ? null : body);
  }
  function apiPut(path, body) {
    var x = xs();
    if (!x) return Promise.reject(new Error("xs-api.js 未加载，无法访问后端"));
    return x.put(path, body === undefined ? null : body);
  }
  function apiDel(path, body) {
    var x = xs();
    if (!x) return Promise.reject(new Error("xs-api.js 未加载，无法访问后端"));
    return x.del(path, body === undefined ? undefined : { body: body });
  }

  // 写操作：202 -> 「已提交审批」，401 -> 回登录，其它错误 toast；成功回调
  function doAction(promise, okMsg, after) {
    return promise.then(function (data) {
      if (okMsg) toastMsg(okMsg);
      if (after) after(data);
      return data;
    }, function (err) {
      if (isAuthErr(err)) { kickLogin(); return null; }
      var pending = pendingInfo(err);
      if (pending) {
        var id = ticketIdOf(err);
        toastMsg("已提交审批" + (id ? "（审批单 #" + id + "）" : ""));
        if (after) after(null);
        return null;
      }
      toastMsg(errText(err));
      return null;
    });
  }

  // 异步页公共收尾：校验 seq，401 回登录，其余错误渲染错误面板
  function loadFail(el, seq, err, retryLabel) {
    if (!alive(seq, el)) return;
    if (isAuthErr(err)) { kickLogin(); return; }
    mount(el, shell("加载失败", "", "") + errorPanel(err, retryLabel));
  }

  function safe(fn) {
    try { return fn(); } catch (e) { console.error("[xs-live-core]", e); return undefined; }
  }

  function onLive(cb) {
    if (typeof cb !== "function") return;
    if (isLive()) { safe(cb); return; }
    WAITERS.push(cb);
  }
  function flushLive() {
    var list = WAITERS.slice();
    WAITERS.length = 0;
    list.forEach(function (cb) { safe(cb); });
  }

  /* ---------------- 弹窗 ---------------- */

  function confirmDialog(title, msgHtml, okLabel, onOk) {
    var m = openModal(title || "确认操作",
      '<div class="x-form"><p style="margin:0">' + msgHtml + "</p></div>",
      '<button class="x-btn" data-close>取消</button><button class="x-btn x-btn--pri" id="liveDlgOk">' + gEsc(okLabel || "确定") + "</button>");
    if (!m) {
      if (typeof window !== "undefined" && typeof window.confirm === "function") {
        if (window.confirm(String(msgHtml).replace(/<[^>]*>/g, ""))) onOk();
      }
      return;
    }
    var ok = $id("liveDlgOk");
    if (ok) ok.onclick = function () { closeModal(); onOk(); };
  }

  function formDialog(title, bodyHtml, okLabel, onSubmit, aiDraft) {
    var m = openModal(title,
      '<form class="x-form" id="liveForm" novalidate>' + bodyHtml +
      (aiDraft ? aiDraftHtml(aiDraft) : "") +
      '<span class="x-err" id="liveErr" role="alert" hidden></span></form>',
      '<button class="x-btn" data-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="liveForm">' + gEsc(okLabel || "确定") + "</button>");
    if (!m) return null;
    var f = $id("liveForm");
    if (!f) return null;
    var bad = function (msg, inp) {
      var e = $id("liveErr");
      if (e) { e.textContent = msg; e.hidden = false; }
      if (inp) inp.focus();
    };
    f.onsubmit = function (e) {
      e.preventDefault();
      try { onSubmit(f, bad); } catch (err) { console.error("[xs-live-core] form", err); bad(errText(err)); }
    };
    if (aiDraft) bindAIDraft(f, aiDraft);
    var first = f.querySelector("input,textarea,select");
    if (first) { try { first.focus(); } catch (e) {} }
    return f;
  }

  function aiDraftHtml(config) {
    var x = xs();
    var allowed = !!(x && typeof x.can === "function" && x.can("ai:chat"));
    var note = allowed
      ? "AI 只会收到这句话和表单允许字段，不会读取当前表单内容；生成后请检查，再单独提交。"
      : "需要 ai:chat 权限才能使用，仍可手动填写。";
    if (config.noSecrets) note += " 不要在描述中输入密码、令牌或其他密钥值。";
    return '<div class="x-ai-formdraft" data-live-ai-draft style="border-top:1px solid var(--x-line);margin-top:4px;padding-top:12px">' +
      '<div class="x-row2" style="align-items:end"><label><span>AI 帮填草稿</span><input class="x-input" type="text" data-live-ai-instruction maxlength="2000" autocomplete="off" placeholder="' +
      gEsc(config.placeholder || config.example || "用一句话描述要创建的资源") + '"' + (allowed ? "" : " disabled") + " /></label>" +
      '<div><button class="x-btn" type="button" data-live-ai-generate' + (allowed ? "" : ' disabled title="需要 ai:chat 权限"') + '>生成草稿</button></div></div>' +
      '<p class="x-frac" style="margin:6px 0 0">' + gEsc(note + (config.example ? " 示例：" + config.example : "")) + '</p>' +
      '<p class="x-frac" data-live-ai-status role="status" aria-live="polite" style="margin:4px 0 0"></p></div>';
  }

  function aiDraftSchema(fields) {
    var schema = {};
    Object.keys(fields || {}).forEach(function (key) {
      var rule = fields[key] || {};
      if (rule.sensitive || AI_SENSITIVE_KEY.test(key) || !rule.description) return;
      var description = rule.description;
      if (rule.type === "enum" && rule.values) description += "；只能取：" + rule.values.join("、");
      if (rule.type === "array" && rule.values) description += "；可选值：" + rule.values.join("、");
      Object.defineProperty(schema, key, { value: rule.type + "，" + description, enumerable: true, configurable: true, writable: true });
    });
    return schema;
  }

  function aiField(type, description, options) {
    var field = { type: type, description: description };
    Object.keys(options || {}).forEach(function (key) { field[key] = options[key]; });
    return field;
  }

  function aiDraftConfig(scene, example, fields, noSecrets, strictNoValues) {
    return {
      scene: scene,
      example: example,
      fields: fields,
      noSecrets: !!noSecrets,
      strictNoValues: !!strictNoValues
    };
  }

  function setAIDraftField(form, name, value, rule) {
    if (rule.ui === "checks") {
      var selected = Array.isArray(value) ? value : [];
      Array.prototype.forEach.call(form.querySelectorAll('input[name="' + name + '"]'), function (input) {
        input.checked = selected.indexOf(input.value) >= 0;
      });
      return true;
    }
    var control = form.elements[name];
    if (!control) return false;
    if (rule.type === "boolean") {
      control.checked = value;
      return true;
    }
    if (rule.ui === "json") control.value = JSON.stringify(value, null, 2);
    else if (rule.ui === "lines") control.value = Array.isArray(value) ? value.join("\n") : String(value);
    else control.value = value;
    if (control.tagName === "SELECT" && typeof control.dispatchEvent === "function") {
      try { control.dispatchEvent(new Event("change", { bubbles: true })); } catch (e) {}
    }
    return true;
  }

  function bindAIDraft(form, config) {
    var root = form.querySelector("[data-live-ai-draft]");
    var input = root && root.querySelector("[data-live-ai-instruction]");
    var button = root && root.querySelector("[data-live-ai-generate]");
    var status = root && root.querySelector("[data-live-ai-status]");
    if (!root || !input || !button || !status) return;
    var setStatus = function (message, error) {
      status.textContent = message;
      status.setAttribute("role", error ? "alert" : "status");
    };
    var generate = async function () {
      var instruction = String(input.value || "").trim();
      if (!instruction) { setStatus("先用一句话描述资源用途和关键要求。", true); input.focus(); return; }
      if (config.noSecrets && aiPromptContainsSensitiveValue(instruction, config.strictNoValues)) {
        setStatus("描述中疑似包含密钥值。请删除具体值，只描述资源用途和名称。", true);
        input.focus();
        return;
      }
      var api = xs();
      if (!api || typeof api.get !== "function" || typeof api.post !== "function") {
        setStatus("AI 接口未连接，仍可手动填写。", true);
        return;
      }
      if (typeof api.can !== "function" || !api.can("ai:chat")) {
        setStatus("当前账号没有 ai:chat 权限，仍可手动填写。", true);
        return;
      }
      button.disabled = true;
      button.textContent = "检查模型...";
      setStatus("正在检查已启用的 AI 模型...");
      try {
        var models = itemsOf(await apiGet("/ai/models/options"));
        if (!models.length) throw new Error("尚未配置已启用的 AI 模型，请先到 AI 设置中接入模型。");
        button.textContent = "生成中...";
        setStatus("正在生成草稿...");
        var response = await api.post("/ai/assist", {
          scene: config.scene,
          instruction: instruction,
          schema: aiDraftSchema(config.fields)
        });
        var draft = sanitizeAIDraft(response && response.fields, config.fields);
        var keys = Object.keys(draft);
        if (!keys.length) throw new Error("没有生成可用字段，请补充资源名称、用途或约束后重试。");
        keys.forEach(function (key) { setAIDraftField(form, key, draft[key], config.fields[key]); });
        setStatus("已填入 " + keys.length + " 个字段草稿，请检查后再单独提交。");
      } catch (err) {
        if (isAuthErr(err)) kickLogin();
        setStatus(errText(err) || "生成失败，请重试或手动填写。", true);
      } finally {
        button.disabled = false;
        button.textContent = "生成草稿";
      }
    };
    button.onclick = generate;
    input.onkeydown = function (event) {
      if (event.key === "Enter" && !event.isComposing) {
        event.preventDefault();
        generate();
      }
    };
  }

  function resultDialog(title, bodyHtml) {
    openModal(title, '<div class="x-form">' + bodyHtml + "</div>", '<button class="x-btn" data-close>关闭</button>');
  }

  function showJson(title, obj) {
    resultDialog(title, '<pre class="x-log" style="max-height:56vh;overflow:auto;margin:0" aria-label="JSON">' + gEsc(prettyJSON(obj)) + "</pre>");
  }

  function copyText(text) {
    var done = function () { toastMsg("已复制到剪贴板"); };
    var fail = function () { resultDialog("手动复制", '<textarea class="x-input" rows="4" readonly>' + gEsc(text) + "</textarea>"); };
    try {
      if (typeof navigator !== "undefined" && navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, fail);
        return;
      }
    } catch (e) {}
    fail();
  }


  /* ================================================================
   * 4. 通用：路由解析 / Promise 包装 / 按 id 查找
   * ================================================================ */

  function routeOf() {
    if (typeof location === "undefined") return { key: "sre", sub: "", sel: "" };
    return parseHash(location.hash);
  }

  function wrap(promise) {
    return promise.then(function (data) { return { ok: true, data: data, err: null }; },
      function (err) { return { ok: false, data: null, err: err }; });
  }

  function byId(list, id) {
    for (var i = 0; i < (list || []).length; i++) {
      if (String((list[i] || {}).id) === String(id)) return list[i];
    }
    return null;
  }

  function sectionPanel(title, meta, res, emptyTitle, emptyDesc, bodyFn) {
    if (!res || !res.ok) {
      return panelHtml(title, "", '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(res && res.err)) + "</p>");
    }
    var items = itemsOf(res.data);
    var total = totalOf(res.data, items);
    if (!items.length) {
      return panelHtml(title, "", '<p class="x-frac" style="margin:0">' + gEsc(emptyTitle || "暂无数据") +
        (emptyDesc ? "；" + gEsc(emptyDesc) : "") + "</p>");
    }
    return panelHtml(title, meta ? meta(items, total) : "", bodyFn(items, total));
  }

  /* ================================================================
   * 5. 总览 #/sre —— /clusters、/hosts、/observability/alerts 汇总
   * ================================================================ */

  // 总览：沿用旧版布局（5 张 KPI + 24h 曲线 + 节点 TOP5 + 平台公告 + 变更/告警 + 环境筛选），
  // 数据全部取自后端；取不到的区块保留完整卡片与操作入口，只标注"未接入"。
  function renderSreLive(el) {
    var seq = bump();
    mount(el, shell("总览", "集群容量、节点负载、变更与告警", refreshButton("刷新")) + loadingPanel("正在加载总览…"));
    Promise.all([
      wrap(apiGet("/clusters", { page_size: 200 }).then(function (d) { window.XS_CLUSTERS = itemsOf(d); return d; })),
      wrap(apiGet("/hosts", { page_size: 200 })),
      wrap(apiGet("/observability/alerts", { page_size: 200, status: "firing" })),
      wrap(apiGet("/platform/env")),
      wrap(apiGet("/platform/notices"))
    ]).then(function (rs) {
      if (!alive(seq, el)) return;
      if (rs.some(function (r) { return r.err && isAuthErr(r.err); })) { kickLogin(); return; }
      var clusters = rs[0].ok ? itemsOf(rs[0].data) : [];
      var hosts = rs[1].ok ? itemsOf(rs[1].data) : [];
      var alerts = rs[2].ok ? itemsOf(rs[2].data) : [];
      var envInfo = rs[3].ok ? (rs[3].data || {}) : {};
      var notices = rs[4].ok ? itemsOf(rs[4].data) : [];
      var metricsOK = !!(envInfo && envInfo.metrics_available) || (envInfo && envInfo.cpu ? false : false);

      // 逐集群取节点与 Pod 总数（集群不多，并发即可）
      var perCluster = clusters.map(function (c) {
        return Promise.all([
          wrap(apiGet("/clusters/" + c.id + "/nodes")),
          wrap(apiGet("/clusters/" + c.id + "/workloads/pods", { page_size: 1 }))
        ]).then(function (r2) { return { c: c, nodes: r2[0], pods: r2[1] }; });
      });
      Promise.all(perCluster).then(function (list) {
        if (!alive(seq, el)) return;
        var nodesAll = [], podsRun = 0, podsTotal = 0, podsKnown = false;
        list.forEach(function (x) {
          if (x.nodes.ok) itemsOf(x.nodes.data).forEach(function (n) { nodesAll.push({ node: n, cluster: x.c }); });
          if (x.pods.ok) { podsKnown = true; var d = x.pods.data || {}; podsTotal += (d.total || 0); }
        });
        var ready = nodesAll.filter(function (x) { return x.node.ready; }).length;

        var kpi = function (label, value, sub, go) {
          return '<div class="x-card x-kpi x-go" data-go="' + go + '" tabindex="0" role="link" aria-label="' + gEsc(label + " " + value) + '">' +
            "<p>" + gEsc(label) + "</p><strong>" + gEsc(value) + "</strong>" +
            '<span class="x-frac">' + gEsc(sub) + "</span></div>";
        };
        var html = shell("总览", "集群容量、节点负载、变更与告警", refreshButton("刷新"));
        html += '<div class="x-cards x-cards5">' +
          kpi("集群数", String(clusters.length), clusters.length ? clusters.map(function (c) { return c.env || "—"; }).join("   ") : "尚未接入", "#/cluster/list") +
          kpi("节点 Ready / 总数", nodesAll.length ? ready + " / " + nodesAll.length : "—", nodesAll.length ? (nodesAll.length - ready) + " 个 NotReady" : "未接入集群", "#/cluster/nodes") +
          kpi("Pod 运行 / 总数", podsKnown ? String(podsTotal) : "—", podsKnown ? "已统计 Pod 清单" : "未接入", "#/cluster/workloads") +
          kpi("CPU 使用率", "—", metricsOK ? "metrics-server 已就绪" : "metrics-server 未安装", "#/observe") +
          kpi("内存使用率", "—", metricsOK ? "metrics-server 已就绪" : "metrics-server 未安装", "#/observe") +
          "</div>";

        // 24h 曲线：没有 Prometheus 时保留卡片与图例，内部显示未接入
        html += '<section class="x-panel x-setform"><div class="x-sethead"><h2 class="x-h2">集群资源使用   24h</h2>' +
          '<span class="x-frac">CPU / 内存   全部集群均值</span></div>' +
          '<div class="x-tbar" style="margin:0 0 8px"><span class="x-tag x-blue">CPU</span><span class="x-tag x-green">内存</span>' +
          '<span class="x-grow"></span><button class="x-btn" type="button" data-goto="#/settings">配置数据源</button></div>' +
          '<div class="x-ochart" style="display:grid;place-items:center">' +
          '<div style="text-align:center"><strong>尚未接入 Prometheus</strong><p class="x-frac" style="margin:6px 0 0">在「系统设置」配置 Prometheus 地址后，这里显示 24h CPU/内存曲线。</p></div>' +
          "</div></section>";

        var topRows = nodesAll.slice(0, 5).map(function (x) {
          var n = x.node;
          return [
            '<span class="x-name">' + gEsc(n.name) + "</span>",
            '<span class="x-tag x-grey">' + gEsc(x.cluster.name) + "</span>",
            '<span class="x-frac">—</span>', '<span class="x-frac">—</span>',
            gEsc(n.pods || "—"),
            n.ready ? tagHtml("Ready", "ok") : tagHtml(n.status || "NotReady", "bad")
          ];
        });
        var noticeRows = notices.map(function (n) {
          return '<div class="x-listitem" style="padding:8px 0;border-bottom:1px solid var(--x-line)">' +
            '<div style="display:flex;gap:8px;align-items:center">' + (n.pinned ? '<span class="x-tag x-blue">置顶</span>' : "") +
            '<strong>' + gEsc(n.title) + "</strong><span class=\"x-grow\"></span>" +
            '<span class="x-frac">' + gEsc((n.created_by || "") + "   " + fmtTime(n.created_at)) + "</span>" +
            '<button class="x-btn" type="button" data-ndel="' + gEsc(n.id) + '">删除</button></div>' +
            (n.body ? '<p class="x-frac" style="margin:4px 0 0">' + gEsc(n.body) + "</p>" : "") + "</div>";
        }).join("");

        html += '<div class="x-wide" style="display:grid;grid-template-columns:minmax(0,1.15fr) minmax(0,1fr);gap:16px">' +
          '<section class="x-panel"><div class="x-sethead"><h2 class="x-h2">节点 TOP5 负载</h2><span class="x-frac">' +
          (metricsOK ? "按 CPU 排序" : "CPU/内存未上报（metrics-server 未安装）") + "</span></div>" +
          (topRows.length ? tableHtml(["节点", "集群", "CPU", "内存", "Pods", "状态"], topRows) : '<p class="x-empty">尚无节点数据</p>') + "</section>" +
          '<section class="x-panel"><div class="x-sethead"><h2 class="x-h2">平台公告</h2><button class="x-btn" type="button" data-nadd>+ 发布公告</button></div>' +
          (noticeRows || '<p class="x-empty">暂无公告</p>') + "</section></div>";

        // 环境筛选 + 变更/告警
        var ENVS = ["全部", "生产", "预发", "测试", "开发"];
        html += '<div class="x-tbar" role="group" aria-label="按环境筛选">' +
          ENVS.map(function (e) { return '<button class="x-seg" type="button" data-senv="' + e + '">' + e + "</button>"; }).join("") + "</div>";

        var alertRows = alerts.slice(0, 6).map(function (a) {
          return '<li><span class="x-tag x-red">' + gEsc(a.severity || "P?") + "</span><span class=\"x-name\">" + gEsc(a.summary || a.name || a.alertname || "告警") +
            '</span><span class="x-frac">' + gEsc(a.status || "firing") + "   " + gEsc(fmtAgo(a.starts_at || a.created_at)) + "</span></li>";
        }).join("");
        html += '<div class="x-two" style="display:grid;grid-template-columns:1fr 1fr;gap:16px">' +
          '<section class="x-panel"><div class="x-sethead"><h2 class="x-h2">最近变更</h2>' +
          '<a class="x-btn" href="#/release">查看发布单</a></div>' +
          '<p class="x-empty">尚未接入发布记录：在「应用交付 › 发布单」创建发布后显示。</p></section>' +
          '<section class="x-panel"><div class="x-sethead"><h2 class="x-h2">活跃告警 <span class="x-frac">' + alerts.length + " 条</span></h2>" +
          '<a class="x-btn" href="#/observe">查看监控告警</a></div>' +
          (alertRows ? '<ul class="x-list">' + alertRows + "</ul>" : '<p class="x-empty">当前没有触发中的告警</p>') + "</section></div>";

        mount(el, html);
        // 交互
        [].slice.call(el.querySelectorAll(".x-go")).forEach(function (t) {
          t.onclick = function (e) { if (e.target.closest("a,button")) return; var h = t.getAttribute("data-go"); if (h) goHash(h); };
        });
        [].slice.call(el.querySelectorAll("[data-goto]")).forEach(function (b) { b.onclick = function () { goHash(b.getAttribute("data-goto")); }; });
        [].slice.call(el.querySelectorAll("[data-senv]")).forEach(function (b) {
          b.onclick = function () { goHash("#/cluster/list"); };
        });
        var add = el.querySelector("[data-nadd]");
        if (add) add.onclick = function () { noticeDialog(renderSreLive.bind(null, el)); };
        [].slice.call(el.querySelectorAll("[data-ndel]")).forEach(function (b) {
          b.onclick = function () {
            if (!confirm("删除这条公告？")) return;
            doAction(apiDel("/platform/notices/" + encodeURIComponent(b.getAttribute("data-ndel"))), "已删除公告", function () { renderSreLive(el); });
          };
        });
      });
    }, function (err) {
      if (!alive(seq, el)) return;
      if (isAuthErr(err)) return kickLogin();
      mount(el, shell("总览", "", refreshButton("刷新")) + errorPanel(err));
    });
  }

  // 发布公告弹窗（真实写库：POST /platform/notices）
  function noticeDialog(after) {
    var body = '<form class="x-form" id="ntf" novalidate>' +
      '<label><span>标题</span><input class="x-input" name="title" maxlength="200" placeholder="如：周六 02:00-04:00 集群升级" required /></label>' +
      '<label><span>内容</span><textarea class="x-input" name="body" rows="4" maxlength="4000" placeholder="补充说明（可选）"></textarea></label>' +
      '<label class="x-check"><input type="checkbox" name="pinned" /> 置顶</label></form>';
    var foot = '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="button" id="ntok">发布</button>';
    if (typeof dlg === "function") dlg("发布公告", body, foot); else openModal("发布公告", body, foot);
    var ok = $id("ntok");
    if (!ok) return;
    ok.onclick = function () {
      var f = $id("ntf"), t = f.querySelector("[name=title]").value.trim();
      if (!t) return;
      doAction(apiPost("/platform/notices", { title: t, body: f.querySelector("[name=body]").value.trim(), pinned: f.querySelector("[name=pinned]").checked }),
        "公告已发布", function () { try { if (typeof closeAll === "function") closeAll(); } catch (e) {} if (after) after(); });
    };
  }

  function renderIamLive(el) {
    var sub = routeOf().sub || "users";
    if (sub === "groups") return iamGroupsLive(el);
    if (sub === "roles") return iamRolesLive(el);
    if (sub === "logs") return iamLogsLive(el);
    return iamUsersLive(el);
  }

  function iamUsersLive(el) {
    var seq = bump();
    mount(el, shell("用户管理", "平台账号、角色与登录状态", "") + loadingPanel("正在加载用户…"));

    Promise.all([wrap(apiGet("/users", { page_size: 200 })), wrap(apiGet("/roles"))]).then(function (rs) {
      if (!alive(seq, el)) return;
      var uR = rs[0], rR = rs[1];
      if (!uR.ok) {
        if (isAuthErr(uR.err)) return kickLogin();
        mount(el, shell("用户管理", "平台账号、角色与登录状态", "") + errorPanel(uR.err));
        return;
      }
      var users = itemsOf(uR.data), total = totalOf(uR.data, users);
      IAM_USERS = users;
      IAM_ROLES = rR.ok ? itemsOf(rR.data) : [];
      var roleNames = IAM_ROLES.map(function (r) { return r.name; }).filter(Boolean);

      var table = users.length ? tableHtml(
        ["用户", "姓名", "邮箱", "角色", "状态", "最后登录", "操作"],
        users.map(function (u) {
          var roles = (u.roles || []).map(function (r) { return r.name || r.code; }).join("、");
          var ops = actBtn("解锁", "iam-unlock", u.id, { disabled: !u.locked, title: u.locked ? "清除登录锁定" : "账号未锁定" }) +
            actBtn(u.status === "disabled" ? "启用" : "禁用", "iam-toggle", u.id, { title: "启用/禁用账号" }) +
            actBtn("重置密码", "iam-reset", u.id) +
            actBtn("删除", "iam-del", u.id, { disabled: !!u.is_builtin, title: u.is_builtin ? "内置用户不可删除" : "" });
          return [
            "<b>" + gEsc(u.username) + "</b>" +
              (u.is_builtin ? ' <span class="x-tag x-blue">内置</span>' : "") +
              (u.must_change_password ? ' <span class="x-tag x-orange">待改密</span>' : ""),
            gEsc(u.display_name || "—"),
            gEsc(u.email || "—"),
            gEsc(roles || "—"),
            statusTag(userStatus(u.status)) + (u.locked ? " " + tagHtml("登录锁定", "warn") : ""),
            '<span class="x-mono">' + gEsc(fmtTime(u.last_login_at)) + "</span>" +
              (u.last_login_ip ? '<br /><span class="x-frac">' + gEsc(u.last_login_ip) + "</span>" : ""),
            '<span class="x-kacts">' + ops + "</span>"
          ];
        })
      ) : emptyPanel("暂无用户数据", "当前没有可展示的用户。");

      var html = shell("用户管理", "平台账号、角色与登录状态", "") +
        statCards([
          { label: "用户总数", value: String(total), sub: "含内置账号" },
          { label: "正常账号", value: String(users.filter(function (u) { return u.status === "active"; }).length), sub: "status=active" },
          { label: "锁定 / 禁用", value: String(users.filter(function (u) { return u.locked || u.status === "disabled"; }).length), sub: "需处理" }
        ]) +
        toolbarHtml(searchInput("搜索用户名 / 姓名 / 邮箱") +
          selectHtml([{ v: "", t: "全部状态" }, { v: "active", t: "正常" }, { v: "disabled", t: "已禁用" }], "", 'data-live-filter data-attr="status" aria-label="按状态筛选"') +
          '<span class="x-frac" data-live-none hidden>无匹配用户</span>' +
          '<button type="button" class="x-btn x-btn--pri" style="margin-left:auto" data-act="iam-new" data-id="">+ 新建用户</button>') +
        (rR.ok ? "" : '<p class="x-aerr" role="alert" style="margin:0 0 12px">角色列表加载失败：' + gEsc(errText(rR.err)) + "；新建用户时将无法选择角色。</p>") +
        table;

      mount(el, html);
      // 行级筛选需要 data-f-status
      Array.prototype.forEach.call(el.querySelectorAll("[data-live-tablewrap] tbody tr"), function (tr, i) {
        var u = users[i];
        if (u) tr.setAttribute("data-f-status", String(u.status || ""));
      });
      filterTable(el);

      bindActs(el, function (act, id) {
        var u = byId(IAM_USERS, id);
        if (act === "iam-new") return newUserDialog();
        if (!u) return;
        if (act === "iam-unlock") {
          doAction(apiPost("/users/" + u.id + "/unlock", null), "已解锁 " + u.username, repaint);
        } else if (act === "iam-toggle") {
          var next = u.status === "disabled" ? "active" : "disabled";
          if (u.is_builtin && next === "disabled") return toastMsg("内置用户不可禁用");
          confirmDialog("修改账号状态", (next === "disabled" ? "禁用" : "启用") + "用户 <b>" + gEsc(u.username) + "</b>？" +
            (next === "disabled" ? "禁用后该用户所有会话会被撤销。" : ""), next === "disabled" ? "禁用" : "启用", function () {
            doAction(apiPut("/users/" + u.id + "/status", { status: next }), "已" + (next === "disabled" ? "禁用" : "启用") + " " + u.username, repaint);
          });
        } else if (act === "iam-reset") {
          confirmDialog("重置密码", "为 <b>" + gEsc(u.username) +
            "</b> 生成新的临时密码？该用户所有会话会被撤销，下次登录需修改密码。", "重置", function () {
            doAction(apiPost("/users/" + u.id + "/reset-password", {}), null, function (data) {
              if (data && data.password) {
                resultDialog("临时密码", '<p style="margin:0">用户 <b>' + gEsc(u.username) + '</b> 的临时密码：</p><p class="x-mono" style="margin:8px 0;font-size:16px">' +
                  gEsc(data.password) + '</p><p class="x-frac" style="margin:0">此密码只显示一次，请通过安全渠道告知用户。</p>');
              } else {
                toastMsg("已重置 " + u.username + " 的密码");
              }
              repaint();
            });
          });
        } else if (act === "iam-del") {
          confirmDialog("删除用户", "确认删除 <b>" + gEsc(u.username) + "</b>？该操作不可恢复。", "删除", function () {
            doAction(apiDel("/users/" + u.id), "已删除用户 " + u.username, repaint);
          });
        }
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function newUserDialog() {
    var roles = IAM_ROLES || [];
    var checks = roles.map(function (r) {
      return '<label><input type="checkbox" name="role" value="' + gEsc(r.id) + '" /> ' + gEsc(r.name) +
        ' <span class="x-frac">(' + gEsc(r.code) + ")</span></label>";
    }).join("");
    formDialog("新建用户",
      '<label><span class="x-req">用户名</span><input class="x-input" name="username" placeholder="字母开头，3-64 位字母/数字/._-" autocomplete="off" /></label>' +
      '<label><span>姓名</span><input class="x-input" name="display_name" /></label>' +
      '<label><span>邮箱</span><input class="x-input" name="email" type="email" /></label>' +
      '<fieldset class="x-radios" style="display:grid;gap:6px"><legend>角色</legend>' +
      (checks || '<span class="x-frac">没有可分配的角色，请先创建角色。</span>') + "</fieldset>",
      "创建", function (f, bad) {
        var name = String(f.elements.username.value || "").trim();
        if (!/^[A-Za-z][A-Za-z0-9_.\-]{2,63}$/.test(name)) return bad("用户名需以字母开头，3-64 位字母/数字/._-", f.elements.username);
        var ids = Array.prototype.slice.call(f.querySelectorAll('input[name="role"]:checked')).map(function (c) { return Number(c.value); });
        doAction(apiPost("/users", {
          username: name,
          display_name: String(f.elements.display_name.value || "").trim(),
          email: String(f.elements.email.value || "").trim(),
          role_ids: ids
        }), null, function (data) {
          closeModal();
          repaint();
          if (data && data.initial_password) {
            resultDialog("初始密码", '<p style="margin:0">用户 <b>' + gEsc(name) + '</b> 的初始密码：</p><p class="x-mono" style="margin:8px 0;font-size:16px">' +
              gEsc(data.initial_password) + '</p><p class="x-frac" style="margin:0">请通过安全渠道告知用户，首次登录需修改密码。</p>');
          } else {
            toastMsg("已创建用户 " + name);
          }
        });
      });
  }

  function iamGroupsLive(el) {
    var seq = bump();
    mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录", "") +
      loadingPanel("正在加载角色与权限…"));

    Promise.all([
      wrap(apiGet("/users", { page_size: 200 })),
      wrap(apiGet("/roles")),
      wrap(apiGet("/permissions"))
    ]).then(function (rs) {
      if (!alive(seq, el)) return;
      var uR = rs[0], rR = rs[1], pR = rs[2];
      if (!uR.ok) {
        if (isAuthErr(uR.err)) return kickLogin();
        mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录", "") + errorPanel(uR.err));
        return;
      }
      var users = itemsOf(uR.data);
      var roles = rR.ok ? itemsOf(rR.data) : [];
      var perms = pR.ok ? itemsOf(pR.data) : [];

      var agg = roleMembers(roles, users);
      var roleBody;
      if (!rR.ok) {
        roleBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(rR.err)) + "</p>";
      } else if (!agg.length) {
        roleBody = '<p class="x-frac" style="margin:0">还没有角色，无法按角色聚合成员。</p>';
      } else {
        roleBody = tableHtml(["用户组（角色）", "编码", "环境范围", "成员数", "成员", "类型"], agg.map(function (x) {
          var r = x.role || {};
          return [
            "<b>" + gEsc(r.name || "—") + "</b>",
            mono(r.code || "—"),
            gEsc((r.env_scope || []).join("、") || "全部环境"),
            String(x.members.length),
            '<span class="x-frac">' + gEsc(x.members.slice(0, 20).join("、") || "—") + (x.members.length > 20 ? " 等" : "") + "</span>",
            r.builtin ? tagHtml("内置", "info") : tagHtml("自定义", "muted")
          ];
        }));
      }

      var permBody;
      if (!pR.ok) {
        permBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(pR.err)) + "</p>";
      } else if (!perms.length) {
        permBody = '<p class="x-frac" style="margin:0">/permissions 返回空目录。</p>';
      } else {
        var sorted = perms.slice().sort(function (a, b) {
          return String(a.module || "").localeCompare(String(b.module || "")) || String(a.code || "").localeCompare(String(b.code || ""));
        });
        permBody = tableHtml(["模块", "权限码", "说明"], sorted.map(function (p) {
          return [gEsc(p.module || "—"), mono(p.code || "—"), '<span class="x-frac">' + gEsc(p.description || "") + "</span>"];
        }));
      }

      mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录", "") +
        '<p class="x-frac" style="margin:0 0 12px">当前按成员角色聚合展示，并列出可授予的权限目录。</p>' +
        panelHtml("按角色聚合的用户组", roles.length + " 个角色", roleBody) +
        panelHtml("权限目录", perms.length + " 项权限", permBody));
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function iamRolesLive(el) {
    var seq = bump();
    mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略", "") +
      loadingPanel("正在加载角色与环境策略…"));

    Promise.all([wrap(apiGet("/roles")), wrap(apiGet("/env-policies"))]).then(function (rs) {
      if (!alive(seq, el)) return;
      var rR = rs[0], eR = rs[1];
      if (!rR.ok) {
        if (isAuthErr(rR.err)) return kickLogin();
        mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略", "") + errorPanel(rR.err));
        return;
      }
      IAM_ROLES = itemsOf(rR.data);
      var policies = eR.ok ? itemsOf(eR.data) : [];

      var roleBody = IAM_ROLES.length ? tableHtml(
        ["角色", "编码", "环境范围", "用户数", "权限数", "更新时间", "操作"],
        IAM_ROLES.map(function (r) {
          return [
            "<b>" + gEsc(r.name) + "</b>" + (r.builtin ? ' <span class="x-tag x-blue">内置</span>' : ""),
            mono(r.code || "—"),
            gEsc((r.env_scope || []).join("、") || "全部环境"),
            String(r.user_count == null ? "—" : r.user_count),
            '<a href="#" class="x-tlink" data-act="iam-perms" data-id="' + gEsc(r.id) + '">' + (r.permissions || []).length + " 项</a>",
            '<span class="x-frac">' + gEsc(fmtTime(r.updated_at)) + "</span>",
            actBtn("查看权限", "iam-perms", r.id)
          ];
        })
      ) : '<p class="x-frac" style="margin:0">还没有角色。</p>';

      var polBody;
      if (!eR.ok) {
        polBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(eR.err)) + "</p>";
      } else if (!policies.length) {
        polBody = '<p class="x-frac" style="margin:0">尚未配置环境变更策略；未配置时写操作不触发审批，建议为生产环境开启。</p>';
      } else {
        polBody = tableHtml(["环境", "名称", "变更需审批", "审批权限", "更新时间"], policies.map(function (p) {
          return [
            "<b>" + gEsc(p.env || "—") + "</b>",
            gEsc(p.name || "—"),
            p.require_approval ? tagHtml("需要审批", "warn") : tagHtml("免审批", "muted"),
            mono(p.approver_permission || "approval:approve"),
            '<span class="x-frac">' + gEsc(fmtTime(p.updated_at)) + "</span>"
          ];
        }));
      }

      mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略", "") +
        panelHtml("角色", IAM_ROLES.length + " 个", roleBody) +
        panelHtml("环境变更策略（生产审批）", policies.length + " 条", polBody));

      bindActs(el, function (act, id) {
        if (act !== "iam-perms") return;
        var role = byId(IAM_ROLES, id);
        if (!role) return;
        var groups = groupPermissions((role.permissions || []).map(function (code) { return { code: code, module: String(code).split(":")[0] }; }));
        var body = Object.keys(groups).sort().map(function (m) {
          return '<p style="margin:0 0 6px"><b>' + gEsc(m) + '</b> <span class="x-frac">' + groups[m].length + " 项</span></p>" +
            '<p style="margin:0 0 14px">' + groups[m].map(function (p) {
              return '<span class="x-tag x-grey x-tag--s">' + gEsc(p.code) + "</span>";
            }).join(" ") + "</p>";
        }).join("");
        resultDialog("角色权限   " + (role.name || role.code || id), body || '<p class="x-frac" style="margin:0">该角色没有任何权限。</p>');
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function iamLogsLive(el) {
    var seq = bump();
    mount(el, shell("登录日志", "登录审计与活跃会话", "") + loadingPanel("正在加载登录日志与会话…"));

    Promise.all([
      wrap(apiGet("/audit-logs", { path: "/api/v1/auth/login", page_size: 100 })),
      wrap(apiGet("/auth/sessions"))
    ]).then(function (rs) {
      if (!alive(seq, el)) return;
      var lR = rs[0], sR = rs[1];
      if ((lR.err && isAuthErr(lR.err)) || (sR.err && isAuthErr(sR.err))) return kickLogin();
      var logs = lR.ok ? itemsOf(lR.data) : [];
      var sessions = sR.ok ? itemsOf(sR.data) : [];

      var logBody;
      if (!lR.ok) {
        logBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(lR.err)) + "（查看登录日志需要 audit:read 权限）</p>";
      } else if (!logs.length) {
        logBody = '<p class="x-frac" style="margin:0">还没有登录记录。</p>';
      } else {
        logBody = tableHtml(["时间", "用户", "IP", "结果", "方法", "User-Agent"], logs.map(function (l) {
          return [
            '<span class="x-mono">' + gEsc(fmtTime(l.created_at)) + "</span>",
            "<b>" + gEsc(l.username || "—") + "</b>",
            mono(l.ip || "—"),
            statusTag(resultStatus(l.result)),
            gEsc(l.method || "—"),
            '<span class="x-frac" title="' + gEsc(l.user_agent || "") + '">' + gEsc(truncate(l.user_agent || "—", 48)) + "</span>"
          ];
        }));
      }

      var sessBody;
      if (!sR.ok) {
        sessBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(sR.err)) + "</p>";
      } else if (!sessions.length) {
        sessBody = '<p class="x-frac" style="margin:0">没有活跃会话。</p>';
      } else {
        sessBody = tableHtml(["会话", "当前", "IP", "客户端", "创建时间", "最近使用", "操作"], sessions.map(function (s) {
          return [
            '<span class="x-mono" title="' + gEsc(s.id || "") + '">' + gEsc(truncate(s.id || "—", 12)) + "</span>",
            s.current ? tagHtml("当前会话", "ok") : '<span class="x-frac">—</span>',
            mono(s.ip || "—"),
            '<span class="x-frac" title="' + gEsc(s.user_agent || "") + '">' + gEsc(truncate(s.user_agent || "—", 36)) + "</span>",
            '<span class="x-frac">' + gEsc(fmtTime(s.created_at)) + "</span>",
            '<span class="x-frac">' + gEsc(fmtAgo(s.last_used_at)) + "</span>",
            s.current ? '<span class="x-frac">—</span>' : actBtn("下线", "iam-revoke", s.id)
          ];
        }));
      }

      mount(el, shell("登录日志", "登录审计与活跃会话", "") +
        toolbarHtml(searchInput("搜索用户 / IP / 结果") +
          selectHtml([{ v: "", t: "全部结果" }, { v: "success", t: "成功" }, { v: "failure", t: "失败" }], "", 'data-live-filter data-attr="result" aria-label="按结果筛选"') +
          '<span class="x-frac" data-live-none hidden>无匹配记录</span>') +
        panelHtml("登录日志", (lR.ok ? logs.length + " 条" : ""), logBody) +
        panelHtml("活跃会话", (sR.ok ? sessions.length + " 个" : ""), sessBody));

      Array.prototype.forEach.call(el.querySelectorAll("[data-live-tablewrap] tbody tr"), function (tr, i) {
        var l = logs[i];
        if (l) tr.setAttribute("data-f-result", String(l.result || ""));
      });
      filterTable(el);

      bindActs(el, function (act, id) {
        if (act !== "iam-revoke") return;
        confirmDialog("下线会话", "确认撤销该会话？对应客户端会立即退出登录。", "下线", function () {
          doAction(apiDel("/auth/sessions/" + encodeURIComponent(id)), "已下线该会话", repaint);
        });
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  /* ================================================================
   * 7. 变更审批 #/approvals
   * ================================================================ */

  var APPROVALS = { scope: null, status: "" };
  var APPROVAL_ITEMS = [];

  function renderApprovalsLive(el) {
    var seq = bump();
    var canAll = !!(xs() && typeof xs().can === "function" && xs().can("approval:read"));
    if (!APPROVALS.scope) APPROVALS.scope = approvalScopeDefault(canAll);
    var scope = APPROVALS.scope;
    if (scope === "todo" && !APPROVALS.status) APPROVALS.status = "pending";
    var status = APPROVALS.status;

    var base = shell("变更审批", "审批单列表与处理；生产变更按环境审批策略执行", "");
    mount(el, base + loadingPanel("正在加载审批单…"));

    var query = { scope: scope, page_size: 100 };
    if (status) query.status = status;

    apiGet("/approvals", query).then(function (data) {
      if (!alive(seq, el)) return;
      var items = itemsOf(data), total = totalOf(data, items);
      APPROVAL_ITEMS = items;

      var tabs = [["todo", "待我审批"], ["mine", "我的申请"], ["all", "全部"]];
      var tabsHtml = '<div class="x-tabs" role="tablist">' + tabs.map(function (t) {
        return '<button type="button" role="tab" class="' + (scope === t[0] ? "on" : "") + '" data-approval-scope="' + t[0] +
          '" aria-selected="' + (scope === t[0]) + '">' + t[1] + "</button>";
      }).join("") + "</div>";

      var statusItems = [{ v: "", t: "全部状态" }, { v: "pending", t: "待审批" }, { v: "approved", t: "已通过" },
        { v: "executed", t: "已执行" }, { v: "rejected", t: "已驳回" }, { v: "cancelled", t: "已取消" }, { v: "failed", t: "执行失败" }];
      var statusSel = selectHtml(statusItems, status,
        'data-approval-status aria-label="按状态筛选"' + (scope === "todo" ? " disabled title=\"待我审批仅显示待审批\"" : ""));

      var body = items.length ? tableHtml(
        ["#", "类型", "标题", "环境", "申请人", "状态", "创建时间", "操作"],
        items.map(function (t) {
          var st = ticketStatus(t.status);
          var ops = actBtn("详情", "approval-detail", t.id);
          if (String(t.status) === "pending") {
            if (scope === "mine") ops += actBtn("取消", "approval-cancel", t.id);
            else ops = actBtn("通过", "approval-approve", t.id) + actBtn("驳回", "approval-reject", t.id) + ops;
          }
          return [
            mono("#" + t.id),
            gEsc(t.type || "—"),
            '<span class="x-name" title="' + gEsc(t.title || "") + '">' + gEsc(truncate(t.title || "—", 56)) + "</span>" +
              (t.reason ? '<br /><span class="x-frac">' + gEsc(truncate(t.reason, 60)) + "</span>" : ""),
            t.env ? '<span class="x-tag x-env">' + gEsc(t.env) + "</span>" : '<span class="x-frac">—</span>',
            gEsc(t.applicant_name || "—"),
            statusTag(st),
            '<span class="x-frac">' + gEsc(fmtTime(t.created_at)) + "</span>",
            '<span class="x-kacts">' + ops + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">' + gEsc(scope === "todo"
        ? "没有待你审批的审批单。其他人发起、且所在环境要求审批的变更会出现在这里。"
        : scope === "mine"
          ? "你还没有发起过审批单。生产环境变更被拦截时会自动创建审批单。"
          : "没有符合条件的审批单。") + "</p>";

      mount(el, shell("变更审批", "审批单列表与处理；生产变更按环境审批策略执行", "") +
        toolbarHtml(tabsHtml + statusSel + '<span class="x-frac">共 ' + total + " 条</span>") +
        panelHtml("审批单", scope === "todo" ? "待我审批" : scope === "mine" ? "我的申请" : "全部", body));

      Array.prototype.forEach.call(el.querySelectorAll("[data-approval-scope]"), function (b) {
        b.onclick = function (e) {
          e.preventDefault();
          APPROVALS.scope = b.getAttribute("data-approval-scope");
          APPROVALS.status = APPROVALS.scope === "todo" ? "pending" : "";
          repaint();
        };
      });
      var st = el.querySelector("[data-approval-status]");
      if (st) st.onchange = function () { APPROVALS.status = st.value; repaint(); };

      bindActs(el, function (act, id) {
        var t = byId(APPROVAL_ITEMS, id);
        if (!t) return;
        if (act === "approval-detail") return showJson("审批单 #" + t.id, t);
        if (act === "approval-approve") {
          return confirmDialog("通过审批单 #" + t.id,
            "确认通过 <b>" + gEsc(t.title || "") + "</b>？通过后将执行对应变更（生产环境请先确认影响范围）。", "通过", function () {
              doAction(apiPost("/approvals/" + t.id + "/approve", { comment: "" }), "已通过审批单 #" + t.id, repaint);
            });
        }
        if (act === "approval-reject") {
          return formDialog("驳回审批单 #" + t.id,
            '<p class="x-frac" style="margin:0 0 8px">' + gEsc(t.title || "") + "</p>" +
            '<label><span>驳回原因</span><textarea class="x-input" name="comment" rows="3" maxlength="2000" placeholder="选填"></textarea></label>',
            "驳回", function (f) {
              doAction(apiPost("/approvals/" + t.id + "/reject", { comment: String(f.elements.comment.value || "").trim() }),
                "已驳回审批单 #" + t.id, function () { closeModal(); repaint(); });
            });
        }
        if (act === "approval-cancel") {
          return confirmDialog("取消审批单 #" + t.id, "确认取消 <b>" + gEsc(t.title || "") + "</b>？", "取消审批单", function () {
            doAction(apiPost("/approvals/" + t.id + "/cancel", { comment: "" }), "已取消审批单 #" + t.id, repaint);
          });
        }
      });
    }, function (err) {
      if (!alive(seq, el)) return;
      if (isAuthErr(err)) return kickLogin();
      if (scope === "all" && err && (err.status === 403 || err.code === 40300)) {
        APPROVALS.scope = "mine";
        toastMsg("没有「查看全部审批」权限，已切换到「我的申请」");
        return repaint();
      }
      mount(el, base + errorPanel(err));
    });
  }


  /* ================================================================
   * 8. 主机纳管 #/cluster/hosts —— /hosts、/hosts/enroll-tokens、/hosts/tasks
   * ================================================================ */

  var HOSTS = [];
  var HOST_TOKENS = [];

  function renderHostsLive(el) {
    var seq = bump();
    mount(el, shell("主机纳管", "主机清单、注册令牌与批量任务", "") + loadingPanel("正在加载主机…"));

    Promise.all([
      wrap(apiGet("/hosts", { page_size: 200 })),
      wrap(apiGet("/hosts/enroll-tokens"))
    ]).then(function (rs) {
      if (!alive(seq, el)) return;
      var hR = rs[0], tR = rs[1];
      if (hR.err && isAuthErr(hR.err)) return kickLogin();
      var hosts = hR.ok ? itemsOf(hR.data) : [];
      HOSTS = hosts;
      HOST_TOKENS = tR.ok ? itemsOf(tR.data) : [];
      var online = hosts.filter(function (h) { return h.online; }).length;
      var envs = [];
      hosts.forEach(function (h) { if (h.env && envs.indexOf(h.env) < 0) envs.push(h.env); });

      var hostBody;
      if (!hR.ok) {
        hostBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(hR.err)) + "</p>";
      } else if (!hosts.length) {
        hostBody = '<p class="x-frac" style="margin:0">还没有纳管主机。先在下方创建注册令牌，再在目标主机上安装 matrix-agent。</p>';
      } else {
        hostBody = tableHtml(["主机", "环境", "系统", "规格", "Agent", "状态", "最后心跳", "操作"], hosts.map(function (h) {
          var f = h.facts || {};
          return [
            '<span class="x-name">' + gEsc(h.name || "—") + "</span>" +
              (h.enrolled ? "" : ' <span class="x-tag x-grey">未注册</span>') +
              '<br /><span class="x-mono">' + gEsc(h.ip || "—") + "</span>",
            gEsc(h.env || "—"),
            gEsc(f.os || f.platform || "—") + (f.arch ? " / " + gEsc(f.arch) : ""),
            gEsc((f.cpu_cores || "?") + "C") + "   " + gEsc(fmtBytes(f.mem_total)),
            mono(h.agent_version || "—"),
            h.online ? tagHtml("在线", "ok") : tagHtml("离线", "muted"),
            '<span class="x-frac">' + gEsc(fmtAgo(h.last_seen_at)) + "</span>",
            '<span class="x-kacts">' + actBtn("执行命令", "host-exec", h.id) + actBtn("任务", "host-tasks", h.id) + actBtn("删除", "host-del", h.id) + "</span>"
          ];
        }));
      }

      var tokenBody;
      if (!tR.ok) {
        tokenBody = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(tR.err)) + "（管理注册令牌需要 host:write 权限）</p>";
      } else if (!HOST_TOKENS.length) {
        tokenBody = '<p class="x-frac" style="margin:0">还没有注册令牌。创建后把安装命令复制到目标主机执行即可纳管。</p>';
      } else {
        tokenBody = tableHtml(["名称", "环境", "创建人", "过期时间", "状态", "操作"], HOST_TOKENS.map(function (t) {
          var expired = toDate(t.expires_at) && toDate(t.expires_at).getTime() < Date.now();
          return [
            "<b>" + gEsc(t.name || "—") + "</b>",
            gEsc(t.env || "—"),
            gEsc(t.created_by || "—"),
            '<span class="x-frac">' + gEsc(fmtTime(t.expires_at)) + "</span>",
            t.used_at ? tagHtml("已使用", "muted") : expired ? tagHtml("已过期", "bad") : tagHtml("可用", "ok"),
            '<span class="x-kacts">' + actBtn("删除", "token-del", t.id) + "</span>"
          ];
        }));
      }

      mount(el, shell("主机纳管", "主机清单、注册令牌与批量任务", "") +
        statCards([
          { label: "主机总数", value: hR.ok ? String(hosts.length) : "—", sub: "host:read" },
          { label: "在线主机", value: hR.ok ? String(online) : "—", sub: "agent 心跳 90s 内" },
          { label: "注册令牌", value: tR.ok ? String(HOST_TOKENS.length) : "—", sub: "host:write" },
          { label: "离线主机", value: hR.ok ? String(hosts.length - online) : "—", sub: "待排查" }
        ]) +
        toolbarHtml(
          searchInput("搜索主机 / IP / 环境") +
          selectHtml(envOptions(envs), "", 'data-live-filter data-attr="env" aria-label="按环境筛选"') +
          '<span class="x-frac" data-live-none hidden>无匹配主机</span>' +
          '<button type="button" class="x-btn x-btn--pri" style="margin-left:auto" data-act="host-batch" data-id="">批量执行</button>'
        ) +
        panelHtml("主机", hR.ok ? hosts.length + " 台" : "", hostBody) +
        panelHtml("注册令牌", tR.ok ? HOST_TOKENS.length + " 个" : "",
          '<div class="x-iamhd"><h2 class="x-h2">注册令牌</h2><span class="x-frac">matrix-agent 接入凭证</span>' +
          '<button type="button" class="x-btn x-btn--pri" style="margin-left:auto" data-act="token-new" data-id="">+ 新建令牌</button></div>' + tokenBody));

      Array.prototype.forEach.call(el.querySelectorAll("[data-live-tablewrap] tbody tr"), function (tr, i) {
        var h = hosts[i];
        if (h) tr.setAttribute("data-f-env", String(h.env || ""));
      });
      filterTable(el);
      bindActs(el, function (act, id) {
        if (act === "host-batch") return batchExecDialog(HOSTS);
        if (act === "token-new") return newTokenDialog();
        if (act === "token-del") {
          var tk = byId(HOST_TOKENS, id);
          if (!tk) return;
          return confirmDialog("删除注册令牌", "确认删除令牌 <b>" + gEsc(tk.name || tk.id) + "</b>？已使用该令牌注册的 Agent 不受影响。", "删除", function () {
            doAction(apiDel("/hosts/enroll-tokens/" + tk.id), "已删除注册令牌", repaint);
          });
        }
        var h = byId(HOSTS, id);
        if (!h) return;
        if (act === "host-exec") return hostExecDialog(h);
        if (act === "host-tasks") return hostTasksDialog(h);
        if (act === "host-del") {
          return confirmDialog("移除主机", "确认移除 <b>" + gEsc(h.name || h.ip) + "</b>？已安装的 Agent 下次心跳时停止。", "移除", function () {
            doAction(apiDel("/hosts/" + h.id), "已移除主机 " + (h.name || h.ip), repaint);
          });
        }
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function hostExecDialog(h) {
    formDialog("在主机执行命令   " + (h.name || h.ip),
      '<p class="x-frac" style="margin:0 0 8px">目标：' + gEsc(h.name || "") + "（" + gEsc(h.ip || "—") + "）  环境 " + gEsc(h.env || "—") +
      "；需要 host:exec 权限，生产环境执行可能转为审批。</p>" +
      '<label><span class="x-req">命令</span><textarea class="x-input" name="command" rows="4" placeholder="如 uptime / systemctl status nginx" spellcheck="false"></textarea></label>' +
      '<label><span>超时（秒）</span><input class="x-input" name="timeout" type="number" min="1" max="3600" value="60" /></label>',
      "执行", function (f, bad) {
        var cmd = String(f.elements.command.value || "").trim();
        if (!cmd) return bad("请输入要执行的命令", f.elements.command);
        var timeout = Math.max(1, Math.min(3600, Number(f.elements.timeout.value) || 60));
        closeModal();
        doAction(apiPost("/hosts/" + h.id + "/exec", { command: cmd, timeout_sec: timeout }), null, function (task) {
          if (task) taskResultDialog(h, task);
        });
      });
  }

  function taskResultDialog(h, task) {
    var st = hostTaskStatus(task.status);
    resultDialog("命令结果   " + (h.name || h.ip),
      '<p style="margin:0 0 8px">状态：' + statusTag(st) + "   退出码：" + mono(task.exit_code == null ? "—" : task.exit_code) +
      "   任务 #" + gEsc(task.id) + "</p>" +
      '<pre class="x-log" style="max-height:46vh;overflow:auto;margin:0" aria-label="命令输出">' +
      gEsc(task.output || task.error || "（无输出）") + "</pre>");
  }

  function hostTasksDialog(h) {
    var m = openModal("任务记录   " + (h.name || h.ip), '<div class="x-form"><p class="x-frac" style="margin:0">正在加载任务记录…</p></div>',
      '<button class="x-btn" data-close>关闭</button>');
    if (!m) return;
    apiGet("/hosts/" + h.id + "/tasks", { page_size: 20 }).then(function (data) {
      if (!m || m.hidden) return;
      var items = itemsOf(data);
      var body = items.length ? tableHtml(["#", "类型", "命令", "状态", "退出码", "开始时间", "耗时"], items.map(function (t) {
        var dur = "";
        var a = toDate(t.started_at), b = toDate(t.finished_at);
        if (a && b) dur = Math.max(0, Math.round((b.getTime() - a.getTime()) / 1000)) + "s";
        return [
          mono("#" + t.id),
          gEsc(t.kind || "—"),
          '<span class="x-mono" title="' + gEsc(t.command || "") + '">' + gEsc(truncate(t.command || "—", 40)) + "</span>",
          statusTag(hostTaskStatus(t.status)),
          gEsc(t.exit_code == null ? "—" : t.exit_code),
          '<span class="x-frac">' + gEsc(fmtTime(t.started_at)) + "</span>",
          '<span class="x-frac">' + gEsc(dur || "—") + "</span>"
        ];
      })) : '<p class="x-frac" style="margin:0">该主机还没有执行记录。</p>';
      var w = m.querySelector(".x-wbody");
      if (w) w.innerHTML = body;
    }, function (err) {
      if (isAuthErr(err)) return kickLogin();
      var w = m.querySelector(".x-wbody");
      if (w) w.innerHTML = '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(err)) + "</p>";
    });
  }

  function newTokenDialog() {
    formDialog("新建注册令牌",
      '<label><span>名称</span><input class="x-input" name="name" placeholder="如 prod-web" /></label>' +
      '<label><span class="x-req">环境</span><input class="x-input" name="env" placeholder="如 生产 / 测试" /></label>' +
      '<label><span>有效期（小时）</span><input class="x-input" name="ttl" type="number" min="1" max="720" value="24" /></label>',
      "创建", function (f, bad) {
        var env = String(f.elements.env.value || "").trim();
        if (!env) return bad("请填写环境（决定令牌注册主机的权限范围）", f.elements.env);
        var ttl = Math.max(1, Math.min(720, Number(f.elements.ttl.value) || 24));
        closeModal();
        doAction(apiPost("/hosts/enroll-tokens", {
          name: String(f.elements.name.value || "").trim(),
          env: env,
          ttl_hours: ttl
        }), null, function (data) {
          repaint();
          if (!data) return;
          var tok = data.token || {};
          var cmd = data.install_command || "";
          resultDialog("注册令牌已创建",
            '<p style="margin:0 0 6px">名称：<b>' + gEsc(tok.name || "—") + "</b>   环境 " + gEsc(tok.env || "—") + "</p>" +
            '<p style="margin:0 0 6px">令牌（只显示一次）：</p><p class="x-mono" style="margin:0 0 12px;font-size:15px">' + gEsc(tok.token || "—") + "</p>" +
            (cmd ? '<p style="margin:0 0 6px">在目标主机执行：</p><pre class="x-log" style="margin:0 0 12px;overflow:auto">' + gEsc(cmd) + "</pre>" : "") +
            '<button type="button" class="x-btn" id="liveCopyTok">复制安装命令</button>');
          var b = $id("liveCopyTok");
          if (b) b.onclick = function () { copyText(cmd || tok.token || ""); };
        });
      });
  }

  function batchExecDialog(hosts) {
    if (!hosts || !hosts.length) { toastMsg("没有可执行的主机"); return; }
    var list = hosts.map(function (h) {
      return '<label style="display:block"><input type="checkbox" name="host" value="' + gEsc(h.id) + '"' + (h.online ? "" : " disabled") + " /> " +
        gEsc(h.name || h.ip) + ' <span class="x-frac">' + gEsc(h.ip || "") + "   " + gEsc(h.env || "") + (h.online ? "" : "   离线") + "</span></label>";
    }).join("");
    formDialog("批量执行命令",
      '<p class="x-frac" style="margin:0 0 8px">选择目标主机（仅在线主机可执行；生产环境可能需要审批）：</p>' +
      '<div style="max-height:180px;overflow:auto;display:grid;gap:4px;padding:4px 0">' + list + "</div>" +
      '<label><span class="x-req">命令</span><textarea class="x-input" name="command" rows="3" spellcheck="false"></textarea></label>' +
      '<label><span>超时（秒）</span><input class="x-input" name="timeout" type="number" min="1" max="3600" value="60" /></label>',
      "执行", function (f, bad) {
        var ids = Array.prototype.slice.call(f.querySelectorAll('input[name="host"]:checked')).map(function (c) { return Number(c.value); });
        var cmd = String(f.elements.command.value || "").trim();
        if (!ids.length) return bad("请至少选择一台主机");
        if (!cmd) return bad("请输入要执行的命令", f.elements.command);
        var timeout = Math.max(1, Math.min(3600, Number(f.elements.timeout.value) || 60));
        closeModal();
        apiPost("/hosts/batch-exec", { host_ids: ids, command: cmd, timeout_sec: timeout }).then(function (res) {
          var pending = (res && res.pending_approvals) || [];
          var tasks = (res && res.tasks) || [];
          if (!tasks.length && pending.length) {
            toastMsg("已提交审批（" + pending.length + " 个审批单）");
            return;
          }
          toastMsg("已提交批量任务" + (res && res.batch_id ? "：批次 " + res.batch_id : ""));
          batchResultDialog(res && res.batch_id, tasks);
        }, function (err) {
          if (isAuthErr(err)) return kickLogin();
          var p = pendingInfo(err);
          if (p) { toastMsg("已提交审批" + (ticketIdOf(err) ? "（审批单 #" + ticketIdOf(err) + "）" : "")); return; }
          toastMsg(errText(err));
        });
      });
  }

  function batchResultDialog(batchId, tasks) {
    var m = openModal("批量任务结果", '<div class="x-form" id="liveBatchBody"></div>', '<button class="x-btn" data-close>关闭</button>');
    if (!m) return;
    function body(items) {
      return (items.length ? tableHtml(["主机 ID", "命令", "状态", "退出码", "输出"], items.map(function (t) {
        return [
          mono("#" + t.host_id),
          '<span class="x-mono" title="' + gEsc(t.command || "") + '">' + gEsc(truncate(t.command || "—", 36)) + "</span>",
          statusTag(hostTaskStatus(t.status)),
          gEsc(t.exit_code == null ? "—" : t.exit_code),
          '<span class="x-frac" title="' + gEsc(t.output || t.error || "") + '">' + gEsc(truncate(t.output || t.error || "—", 60)) + "</span>"
        ];
      })) : '<p class="x-frac" style="margin:0">暂无任务。</p>') +
        '<p class="x-frac" style="margin:8px 0 0">批次：<span class="x-mono">' + gEsc(batchId || "—") + "</span></p>";
    }
    var box = m.querySelector("#liveBatchBody");
    if (box) box.innerHTML = body(tasks || []);
    if (!batchId) return;
    var tries = 0;
    function tick() {
      tries += 1;
      if (!m || m.hidden || tries > 40) return;
      apiGet("/hosts/tasks", { batch_id: batchId, page_size: 200 }).then(function (data) {
        if (!m || m.hidden) return;
        var items = itemsOf(data);
        var b = m.querySelector("#liveBatchBody");
        if (b) b.innerHTML = body(items);
        if (items.some(function (t) { return String(t.status) === "running"; })) setTimeout(tick, 1500);
      }, function () {});
    }
    setTimeout(tick, 1200);
  }

  /* ================================================================
   * 9. 集群状态 #/cluster/<sub> —— live Kubernetes inventory and operations
   * ================================================================ */

  var CLUSTERS = [];
  var CL_SELECTED = null;
  var CLSTATE = { kind: "deployments", ns: "", evtype: "", logNs: "", logPod: "", logTail: "500" };

  var CLUSTER_TITLES = {
    list: "集群列表",
    nodes: "节点",
    nodepools: "节点分组",
    namespaces: "命名空间",
    workloads: "工作负载",
    storage: "存储",
    hpa: "弹性伸缩",
    cron: "定时任务",
    backup: "备份恢复",
    network: "Service",
    expose: "对外服务",
    config: "配置与密钥",
    events: "事件",
    logs: "Pod 日志",
    hosts: "主机纳管"
  };

  function clusterSelHtml(clusters, cluster, sub, routeKey) {
    if (!clusters || !clusters.length) return "";
    return selectHtml(clusters.map(function (c) {
      return { v: c.id, t: (c.name || "#" + c.id) + (c.env ? "   " + c.env : "") };
    }), cluster ? cluster.id : "", 'data-live-cluster-select data-route-key="' + gEsc(routeKey || "cluster") +
      '" data-sub="' + gEsc(sub || "nodes") + '" aria-label="选择集群"');
  }

  function renderClusterLive(el) {
    var r = routeOf();
    var sub = r.sub || "nodes";
    if (sub === "list") return clListPage(el);
    if (sub === "hosts") return renderHostsLive(el);
    return clDataPage(el, sub, r.sel);
  }

  function renderTrafficLiveRoute(el) {
    var r = routeOf();
    var R = routesRef(), route = R && R.traffic;
    return route && typeof route.__xsLiveOrig === "function" ? route.__xsLiveOrig(el) : undefined;
  }

  function clListPage(el) {
    var seq = bump();
    mount(el, shell("集群列表", "已接入的 Kubernetes 集群", "") + loadingPanel("正在加载集群列表…"));
    apiGet("/clusters", { page_size: 200 }).then(function (data) {
      if (!alive(seq, el)) return;
      CLUSTERS = itemsOf(data);
      window.XS_CLUSTERS = CLUSTERS; // 供主脚本的环境计数/筛选用
      var html = shell("集群列表", "已接入的 Kubernetes 集群", '<button class="x-btn" type="button" data-cljobs>部署任务</button><button class="x-btn" type="button" data-clprovision>部署新集群</button><button class="x-btn x-btn--pri" type="button" data-climport>接入集群</button>');
      if (!CLUSTERS.length) {
        mount(el, html + emptyPanel("尚未接入集群", "使用右上方“接入集群”添加 kubeconfig，接入后可查看节点、命名空间、工作负载与事件。"));
        return;
      }
      var rows = CLUSTERS.map(function (c) {
        return [
          '<span class="x-name">' + gEsc(c.name) + '</span><br /><span class="x-mono">' + gEsc(c.api_server || "—") + "</span>",
          gEsc(c.env || "—"),
          mono(c.version || "—"),
          statusTag(clusterStatus(c.status)) +
            (c.last_error ? '<br /><span class="x-frac" title="' + gEsc(c.last_error) + '">' + gEsc(truncate(c.last_error, 40)) + "</span>" : ""),
          '<span class="x-frac">' + gEsc(fmtTime(c.checked_at)) + "</span>",
          gEsc(c.created_by || "—"),
          '<span class="x-kacts">' + actBtn("节点", "cl-enter", c.id) + actBtn("改环境", "cl-env", c.id, { title: "调整集群的环境标签" }) +
            actBtn("测试连接", "cl-test", c.id) + actBtn("删除", "cl-del", c.id) + "</span>"
        ];
      });
      html += toolbarHtml(searchInput("搜索集群 / 环境 / API") + '<span class="x-frac" data-live-none hidden>无匹配集群</span>') +
        panelHtml("集群", CLUSTERS.length + " 个", tableHtml(["集群", "环境", "版本", "状态", "最近检查", "创建人", "操作"], rows));
      mount(el, html);
      bindActs(el, function (act, id) {
        var c = byId(CLUSTERS, id);
        if (!c) return;
        if (act === "cl-env") {
          var tags = (typeof ENV_TAGS !== "undefined" ? ENV_TAGS : [["生产"], ["预发"], ["测试"], ["开发"]]).map(function (t) { return t[0]; });
          var curEnv = c.env === "dev" ? "开发" : (c.env || "");
          var body = '<form class="x-form" id="cef" novalidate><label><span>环境标签</span><select class="x-select" name="env">' +
            tags.map(function (t) { return '<option value="' + gEsc(t) + '"' + (t === curEnv ? " selected" : "") + ">" + gEsc(t) + "</option>"; }).join("") +
            '</select></label><p class="x-frac" style="margin:8px 0 0">环境决定权限范围与生产审批策略（生产环境默认需要审批）。</p></form>';
          var foot = '<button class="x-btn" data-close type="button">取消</button><button class="x-btn x-btn--pri" id="ceok" type="button">保存</button>';
          if (typeof dlg === "function") dlg("调整集群环境   " + gEsc(c.name), body, foot);
          else openModal("调整集群环境   " + gEsc(c.name), body, foot);
          var ok = $id("ceok");
          if (ok) ok.onclick = function () {
            var sel = $id("cef").querySelector("select[name=env]");
            var next = sel ? sel.value : "";
            if (!next) return;
            doAction(apiPut("/clusters/" + c.id, { name: c.name, env: next }), "已将 " + c.name + " 的环境调整为「" + next + "」", function () {
              try { if (typeof closeAll === "function") closeAll(); } catch (e) {}
              repaint();
            });
          };
          return;
        }
        if (act === "cl-enter") return goHash("#/cluster/nodes/" + c.id);
        if (act === "cl-test") {
          return doAction(apiPost("/clusters/" + c.id + "/test", null), null, function (res) {
            if (!res) return;
            if (res.ok) toastMsg("连接成功   Kubernetes " + (res.version || ""));
            else toastMsg("连接失败：" + (res.error || "未知错误"));
          });
        }
        if (act === "cl-del") {
          return confirmDialog("移除集群", "确认移除 <b>" + gEsc(c.name) +
            "</b>？平台将不再采集该集群的节点与工作负载，集群本身不受影响。", "移除", function () {
              doAction(apiDel("/clusters/" + c.id), "已移除集群 " + c.name, repaint);
            });
        }
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function clDataPage(el, sub, sel, pageRoute) {
    var routeKey = pageRoute && pageRoute.routeKey || "cluster";
    var routeSub = pageRoute && pageRoute.routeSub || sub;
    var bodySub = pageRoute && pageRoute.bodySub || sub;
    var seq = bump();
    var title = pageRoute && pageRoute.title || CLUSTER_TITLES[sub] || "集群状态";
    mount(el, shell(title, "查看所选集群中的 Kubernetes 资源", "") + loadingPanel("正在加载集群列表…"));
    apiGet("/clusters", { page_size: 200 }).then(function (data) {
      if (!alive(seq, el)) return;
      CLUSTERS = itemsOf(data);
      window.XS_CLUSTERS = CLUSTERS; // 供主脚本的环境计数/筛选用
      if (!CLUSTERS.length) {
        mount(el, shell(title, "查看所选集群中的 Kubernetes 资源", "") +
          emptyPanel("尚未接入集群", "接入集群后，这里会显示对应的 Kubernetes 资源。"));
        return;
      }
      var cur = resolveCluster(CLUSTERS, sel) || CLUSTERS[0];
      CL_SELECTED = cur;
      if (String(sel || "") !== String(cur.id)) {
        try { history.replaceState(null, "", clusterResourceHash(routeKey, routeSub, cur.id)); } catch (e) {}
      }
      var summary = "集群 " + (cur.name || "#" + cur.id);
      if (sub === "storage") {
        summary += storageCanWrite()
          ? "   支持创建/修改/删除，写入统一经过集群变更审批网关"
          : "   只读清单，当前账号没有 storage:write 权限";
      } else if (sub === "config" || sub === "nodepools") {
        summary += "   只读清单，当前页面未开放写操作";
      }
      mount(el, shell(title, summary, "") +
        '<div class="x-tbar">' + clusterSelHtml(CLUSTERS, cur, routeSub, routeKey) + "</div>" +
        '<div id="liveClusterBody">' + loadingPanel("正在加载…") + "</div>");
      bindClusterSelect(el);
      var box = $id("liveClusterBody");
      var renderBody = CLUSTER_BODY[bodySub];
      if (!renderBody) {
        if (box) box.innerHTML = emptyPanel("页面暂不可用", "请刷新页面后重试，或联系平台管理员。");
        return;
      }
      renderBody(el, seq, cur, box);
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  // 节点详情：live 模式下的"点开查看"。数据来自 GET /clusters/:id/nodes/:name，
  // 不再依赖演示数据；监控/Pod/事件在没有对应数据源时显示"未上报"而不是伪造数字。
  function clNodeDetail(cur, name) {
    var host = $id("drawer");
    if (!host) return;
    var requestKey = String(cur.id) + "/" + String(name);
    host.classList.add("x-drawer--wide");
    host.setAttribute("role", "dialog");
    host.setAttribute("aria-modal", "true");
    host.setAttribute("aria-label", "节点 " + name + " 详情");
    host.setAttribute("data-live-node", requestKey);
    host.innerHTML =
      '<div class="x-dhead"><span class="x-tag x-blue">Node</span><span>' + gEsc(name) + '</span><button class="x-x" type="button" data-close aria-label="关闭">×</button></div>' +
      '<div class="x-dbody" aria-live="polite">' + loadingPanel("正在加载节点详情…") + "</div>" +
      '<div class="x-dfoot"><button class="x-btn" type="button" data-close>关闭</button></div>';
    var close = function () {
      if (typeof closeAll === "function") closeAll();
      else host.classList.remove("open");
    };
    Array.prototype.forEach.call(host.querySelectorAll("[data-close]"), function (button) { button.onclick = close; });
    host.classList.add("open");
    var detailBody = host.querySelector(".x-dbody");
    var isCurrent = function () {
      return host.classList.contains("open") && host.getAttribute("data-live-node") === requestKey;
    };
    apiGet("/clusters/" + cur.id + "/nodes/" + encodeURIComponent(name)).then(function (res) {
      if (!isCurrent()) return;
      // 后端返回 {node:{…}, metrics_available:bool}
      var n = (res && res.node) || res;
      var metricsOk = !!(res && res.metrics_available);
      if (!n || !n.name) { detailBody.innerHTML = emptyPanel("节点不存在", name); return; }
      var cap = n.capacity || {}, alloc = n.allocatable || {};
      var kv = function (k) { return (alloc[k] || cap[k] || "—"); };
      var labels = Object.keys(n.labels || {}).sort();
      var taints = n.taints || [];
      var conds = n.conditions || [];
      var usage = n.usage || null;
      var row = function (k, v) { return "<dt>" + gEsc(k) + "</dt><dd>" + v + "</dd>"; };
      var detail = (n.ready ? tagHtml("Ready", "ok") : tagHtml(n.status || "NotReady", "bad")) +
        (n.unschedulable ? tagHtml("已封锁", "warn") : "") +
        '<dl class="x-sum">' +
        row("角色", gEsc((n.roles || []).join("、") || "worker")) +
        row("内网 IP", mono(n.internal_ip || "—")) +
        row("kubelet", mono(n.kubelet_version || "—")) +
        row("操作系统", gEsc(n.os_image || "—")) +
        row("内核", mono(n.kernel_version || "—")) +
        row("容器运行时", mono(n.container_runtime || "—")) +
        row("架构", gEsc(n.architecture || "—")) +
        row("创建时间", gEsc(fmtTime(n.created_at))) +
        row("CPU", mono(kv("cpu")) + (usage ? ' <span class="x-frac">已用 ' + usage.cpu_percent + '%</span>' : "")) +
        row("内存", mono(kv("memory")) + (usage ? ' <span class="x-frac">已用 ' + usage.memory_percent + '%</span>' : "")) +
        row("Pod 容量", mono(kv("pods"))) +
        "</dl>" +
        '<h3 class="x-h2" style="margin:16px 0 8px">条件</h3>' +
        (conds.length ? tableHtml(["类型", "状态", "原因", "说明", "最近变更"], conds.map(function (c) {
          return [gEsc(c.type), c.status === "True" ? tagHtml("True", "ok") : tagHtml(c.status || "—", "bad"),
            gEsc(c.reason || "—"), gEsc(c.message || "—"), gEsc(fmtTime(c.lastTransitionTime))];
        })) : '<p class="x-frac" style="margin:0">无</p>') +
        '<h3 class="x-h2" style="margin:16px 0 8px">标签 <span class="x-frac">' + labels.length + '</span></h3>' +
        (labels.length ? '<div class="x-lbls">' + labels.map(function (k) {
          return '<span class="x-tag x-grey">' + gEsc(k) + "=" + gEsc(n.labels[k]) + "</span>";
        }).join("") + "</div>" : '<p class="x-frac" style="margin:0">无</p>') +
        '<h3 class="x-h2" style="margin:16px 0 8px">污点 <span class="x-frac">' + taints.length + '</span></h3>' +
        (taints.length ? '<div class="x-lbls">' + taints.map(function (t) {
          return '<span class="x-tag x-orange">' + gEsc(t.key) + (t.value ? "=" + gEsc(t.value) : "") + ":" + gEsc(t.effect) + "</span>";
        }).join("") + "</div>" : '<p class="x-frac" style="margin:0">无</p>') +
        (!metricsOk ? '<p class="x-frac" style="margin:16px 0 0">CPU/内存使用率未上报：集群未安装 metrics-server。</p>' : "");
      detailBody.innerHTML = detail;
      detailBody.scrollTo(0, 0);
    }, function (err) {
      if (isCurrent()) detailBody.innerHTML = errorPanel(err);
    });
  }

  function clNodesPage(el, seq, cur, box) {
    apiGet("/clusters/" + cur.id + "/nodes").then(function (data) {
      if (!alive(seq, el) || !box) return;
      var items = itemsOf(data);
      var metricsOk = !!(data && data.metrics_available);
      var body = items.length ? tableHtml(
        ["节点", "状态", "角色", "IP", "版本", "CPU", "内存", "操作"],
        items.map(function (n) {
          var usage = n.usage || null;
          return [
            '<a class="x-name" href="javascript:void(0)" data-node="' + gEsc(n.name) + '" title="查看节点详情">' + gEsc(n.name) + "</a>",
            n.ready ? tagHtml("Ready", "ok") : tagHtml(n.status || "NotReady", "bad"),
            gEsc((n.roles || []).join("、") || "worker"),
            mono(n.internal_ip || "—"),
            mono(n.kubelet_version || "—"),
            barHtml(usage ? usage.cpu_percent : null),
            barHtml(usage ? usage.memory_percent : null),
            '<span class="x-kacts">' +
              actBtn("详情", "node-detail", n.name, { title: "查看节点详情" }) +
              (n.unschedulable ? actBtn("解除封锁", "node-uncordon", n.name) : actBtn("封锁", "node-cordon", n.name)) +
              actBtn("驱逐", "node-drain", n.name) + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">该集群没有节点数据。</p>';
      box.innerHTML = panelHtml("节点", items.length + " 个" + (metricsOk ? "" : "   metrics-server 不可用（无 CPU/内存使用率）"), body);
      [].slice.call(el.querySelectorAll("[data-node]")).forEach(function (a) {
        a.onclick = function () { clNodeDetail(cur, a.getAttribute("data-node")); };
      });
      bindActs(el, function (act, id) {
        var basePath = "/clusters/" + cur.id + "/nodes/" + encodeURIComponent(id);
        if (act === "node-detail") clNodeDetail(cur, id);
        else if (act === "node-cordon") doAction(apiPost(basePath + "/cordon", null), "已封锁节点 " + id, repaint);
        else if (act === "node-uncordon") doAction(apiPost(basePath + "/uncordon", null), "已解除封锁 " + id, repaint);
        else if (act === "node-drain") {
          confirmDialog("驱逐节点", "确认驱逐节点 <b>" + gEsc(id) + "</b>？节点会先被封锁，DaemonSet 之外的 Pod 将被迁移。", "驱逐", function () {
            doAction(apiPost(basePath + "/drain", null), "已提交驱逐节点 " + id, repaint);
          });
        }
      });
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  function clNamespacesPage(el, seq, cur, box) {
    apiGet("/clusters/" + cur.id + "/namespaces").then(function (data) {
      if (!alive(seq, el) || !box) return;
      var items = itemsOf(data);
      var body = items.length ? tableHtml(
        ["命名空间", "状态", "标签", "创建时间", "操作"],
        items.map(function (ns) {
          var labels = Object.keys(ns.labels || {});
          return [
            '<span class="x-name">' + gEsc(ns.name) + "</span>",
            tagHtml(ns.status || "—", String(ns.status || "").toLowerCase() === "active" ? "ok" : "muted"),
            '<span class="x-tags">' + (labels.slice(0, 5).map(function (k) {
              return '<span class="x-tag x-grey x-tag--s">' + gEsc(k) + "</span>";
            }).join(" ") || "—") + "</span>",
            '<span class="x-frac">' + gEsc(fmtTime(ns.created_at)) + "</span>",
            '<span class="x-kacts">' + actBtn("详情", "ns-detail", ns.name) + actBtn("查看工作负载", "ns-workloads", ns.name) +
              actBtn("删除", "ns-del", ns.name) + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">该集群没有命名空间数据。</p>';
      var tools = '<button class="x-btn x-btn--pri" type="button" data-nsnew>+ 新建命名空间</button>';
      box.innerHTML = panelHtml("命名空间", items.length + " 个", body, tools);
      var mk = box.querySelector("[data-nsnew]");
      if (mk) mk.onclick = function () {
        var b = '<form class="x-form" id="nsf" novalidate><label><span>命名空间</span><input class="x-input x-mono" name="name" placeholder="如 team-a" required /></label>' +
          '<p class="x-frac" style="margin:8px 0 0">名称需符合 DNS-1123：小写字母、数字和 -。</p></form>';
        var f = '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="button" id="nsok">创建</button>';
        if (typeof dlg === "function") dlg("新建命名空间", b, f); else openModal("新建命名空间", b, f);
        var ok = $id("nsok");
        if (ok) ok.onclick = function () {
          var n = ($id("nsf").querySelector("[name=name]").value || "").trim();
          if (!n) return;
          doAction(apiPost("/clusters/" + cur.id + "/namespaces", { name: n }), "已创建命名空间 " + n, function () {
            try { if (typeof closeAll === "function") closeAll(); } catch (e) {}
            repaint();
          });
        };
      };
      bindActs(el, function (act, id) {
        if (act === "ns-workloads") { CLSTATE.ns = id; return goHash("#/cluster/workloads/" + cur.id); }
        var item = null;
        items.forEach(function (x) { if (x.name === id) item = x; });
        if (act === "ns-detail" && item) {
          var labels = Object.keys(item.labels || {}).sort().map(function (k) { return '<span class="x-tag x-grey">' + gEsc(k) + "=" + gEsc(item.labels[k]) + "</span>"; }).join(" ");
          var b2 = '<dl class="x-sum"><dt>名称</dt><dd>' + mono(item.name) + "</dd><dt>状态</dt><dd>" + tagHtml(item.status || "—", "ok") +
            "</dd><dt>创建时间</dt><dd>" + gEsc(fmtTime(item.created_at)) + "</dd><dt>标签</dt><dd>" + (labels || '<span class="x-frac">无</span>') + "</dd></dl>";
          var f2 = '<button class="x-btn" type="button" data-close>关闭</button>';
          if (typeof dlg === "function") dlg("命名空间   " + gEsc(item.name), b2, f2); else openModal("命名空间", b2, f2);
          return;
        }
        if (act === "ns-del") {
          if (!confirm("删除命名空间 " + id + "？其中的所有资源都会被删除。")) return;
          return doAction(apiDel("/clusters/" + cur.id + "/namespaces/" + encodeURIComponent(id)), "已删除命名空间 " + id, function () { repaint(); });
        }
      });
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  var WORKLOAD_KINDS = [
    { v: "deployments", t: "Deployment" },
    { v: "statefulsets", t: "StatefulSet" },
    { v: "daemonsets", t: "DaemonSet" },
    { v: "jobs", t: "Job" },
    { v: "cronjobs", t: "CronJob" },
    { v: "pods", t: "Pod" }
  ];

  function clWorkloadsPage(el, seq, cur, box) {
    var kind = CLSTATE.kind || "deployments";
    var ns = CLSTATE.ns || "";
    var wlQuery = ns ? { namespace: ns } : {};
    Promise.all([
      wrap(apiGet("/clusters/" + cur.id + "/namespaces")),
      wrap(apiGet("/clusters/" + cur.id + "/workloads/" + kind, wlQuery))
    ]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      var nsR = rs[0], wR = rs[1];
      if (wR.err && isAuthErr(wR.err)) return kickLogin();
      var namespaces = nsR.ok ? itemsOf(nsR.data) : [];
      var items = wR.ok ? itemsOf(wR.data) : [];
      var cols, rows;
      if (kind === "pods") {
        cols = ["Pod", "命名空间", "状态", "就绪", "重启", "节点", "镜像", "操作"];
        rows = items.map(function (p) {
          return [
            '<span class="x-name">' + gEsc(p.name) + "</span>",
            gEsc(p.namespace || "—"),
            tagHtml(p.status || p.phase || "—", String(p.phase || "") === "Running" ? "ok" : "warn"),
            gEsc(p.ready || "—"),
            gEsc(p.restarts == null ? "—" : p.restarts),
            gEsc(p.node || "—"),
            '<span class="x-mono" title="' + gEsc((p.images || []).join(", ")) + '">' + gEsc(truncate((p.images || []).join(", "), 42)) + "</span>",
            '<span class="x-kacts">' + actBtn("详情", "wl-yaml", p.name + "|" + (p.namespace || "")) +
              actBtn("日志", "wl-logs", p.name + "|" + (p.namespace || "")) + "</span>"
          ];
        });
      } else {
        cols = ["名称", "类型", "命名空间", "就绪", "状态", "镜像", "创建时间", "操作"];
        rows = items.map(function (w) {
          var st = String(w.status || "");
          return [
            '<span class="x-name">' + gEsc(w.name) + "</span>",
            gEsc(w.kind || kind),
            gEsc(w.namespace || "—"),
            gEsc(w.ready || "—"),
            tagHtml(w.status || "—", (st === "Available" || st === "Ready" || st === "Running" || st === "Complete") ? "ok" : "warn"),
            '<span class="x-mono" title="' + gEsc((w.images || []).join(", ")) + '">' + gEsc(truncate((w.images || []).join(", "), 42)) + "</span>",
            '<span class="x-frac">' + gEsc(fmtTime(w.created_at)) + "</span>",
            '<span class="x-kacts">' + actBtn("详情", "wl-yaml", w.name + "|" + (w.namespace || "")) +
              (kind === "deployments" || kind === "statefulsets" ? actBtn("扩缩容", "wl-scale", w.name + "|" + (w.namespace || "")) : "") +
              (kind === "deployments" || kind === "statefulsets" || kind === "daemonsets" ? actBtn("重启", "wl-restart", w.name + "|" + (w.namespace || "")) : "") +
              actBtn("删除", "wl-del", w.name + "|" + (w.namespace || "")) + "</span>"
          ];
        });
      }
      var kindSel = selectHtml(WORKLOAD_KINDS, kind, 'data-live-kind aria-label="工作负载类型"');
      var nsSel = selectHtml([{ v: "", t: "全部命名空间" }].concat(namespaces.map(function (n) { return { v: n.name, t: n.name }; })),
        ns, 'data-live-ns aria-label="命名空间"');
      var emptyText = wR.ok
        ? ((ns ? "命名空间 " + ns + " 中" : "该集群中") + "没有 " + (WORKLOAD_KINDS.filter(function (k) { return k.v === kind; })[0] || {}).t + " 资源。")
        : errText(wR.err);
      box.innerHTML = panelHtml("工作负载", wR.ok ? items.length + " 个" : "",
        toolbarHtml(kindSel + nsSel + searchInput("搜索名称 / 镜像 / 状态") +
          '<span class="x-frac" data-live-none hidden>无匹配工作负载</span>') +
        (items.length ? tableHtml(cols, rows) : '<p class="x-frac" style="margin:0">' + gEsc(emptyText) + "</p>"));
      var ks = el.querySelector("[data-live-kind]");
      if (ks) ks.onchange = function () { CLSTATE.kind = ks.value; repaint(); };
      var nsEl = el.querySelector("[data-live-ns]");
      if (nsEl) nsEl.onchange = function () { CLSTATE.ns = nsEl.value; repaint(); };
      filterTable(el);
      bindActs(el, function (act, id) {
        var parts = String(id).split("|"), name = parts[0], wns = parts[1] || ns, base = "/clusters/" + cur.id + "/namespaces/" + wns + "/" + kind + "/" + encodeURIComponent(name);
        if (act === "wl-logs") {
          CLSTATE.logPod = name; CLSTATE.logNs = wns;
          return goHash("#/cluster/logs/" + cur.id);
        }
        if (act === "wl-yaml") {
          apiGet(base).then(function (d) {
            var y = (d && d.yaml) || "# 无内容";
            var body = '<pre class="x-mono" style="max-height:64vh;overflow:auto;white-space:pre-wrap;margin:0">' + gEsc(y) + "</pre>";
            var foot = '<button class="x-btn" type="button" data-close>关闭</button>';
            if (typeof dlg === "function") dlg(kind + "/" + gEsc(name) + "   YAML", body, foot);
            else openModal("YAML", body, foot);
          }, function (err) { toast(errText(err)); });
          return;
        }
        if (act === "wl-scale") {
          var sbody = '<form class="x-form" id="scf" novalidate><label><span>副本数</span><input class="x-input" type="number" min="0" step="1" name="replicas" value="1" required /></label>' +
            '<p class="x-frac" style="margin:8px 0 0">将调整 ' + gEsc(kind + "/" + name) + " 的期望副本数。</p></form>";
          var sfoot = '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="button" id="scok">保存</button>';
          if (typeof dlg === "function") dlg("扩缩容   " + gEsc(name), sbody, sfoot); else openModal("扩缩容", sbody, sfoot);
          var sok = $id("scok");
          if (sok) sok.onclick = function () {
            var v = parseInt($id("scf").querySelector("[name=replicas]").value, 10);
            if (isNaN(v) || v < 0) return;
            doAction(apiPost(base + "/scale", { replicas: v }), "已调整 " + name + " 的副本数为 " + v, function () {
              try { if (typeof closeAll === "function") closeAll(); } catch (e) {}
              repaint();
            });
          };
          return;
        }
        if (act === "wl-restart") {
          if (!confirm("滚动重启 " + kind + "/" + name + "？")) return;
          return doAction(apiPost(base + "/restart", null), "已触发 " + name + " 滚动重启", function () { repaint(); });
        }
        if (act === "wl-del") {
          if (!confirm("删除 " + kind + "/" + name + "？此操作不可撤销。")) return;
          return doAction(apiDel(base), "已删除 " + name, function () { repaint(); });
        }
      });
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  function clEventsPage(el, seq, cur, box) {
    var type = CLSTATE.evtype || "";
    var query = { limit: 200 };
    if (type) query.type = type;
    apiGet("/clusters/" + cur.id + "/events", query).then(function (data) {
      if (!alive(seq, el) || !box) return;
      var items = itemsOf(data);
      var typeSel = selectHtml([{ v: "", t: "全部类型" }, { v: "Normal", t: "Normal" }, { v: "Warning", t: "Warning" }],
        type, 'data-live-evtype aria-label="事件类型"');
      var body = items.length ? tableHtml(
        ["时间", "类型", "原因", "命名空间", "对象", "次数", "消息"],
        items.map(function (e) {
          return [
            '<span class="x-frac">' + gEsc(fmtAgo(e.last_seen)) + "</span>",
            tagHtml(e.type || "—", String(e.type || "") === "Warning" ? "warn" : "info"),
            gEsc(e.reason || "—"),
            gEsc(e.namespace || "—"),
            gEsc((e.object_kind || "") + "/" + (e.object_name || "")),
            gEsc(e.count == null ? "—" : e.count),
            '<span class="x-frac" title="' + gEsc(e.message || "") + '">' + gEsc(truncate(e.message || "—", 90)) + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">最近没有事件。</p>';
      box.innerHTML = panelHtml("事件", items.length + " 条",
        toolbarHtml(typeSel + searchInput("搜索原因 / 对象 / 消息") + '<span class="x-frac" data-live-none hidden>无匹配事件</span>') + body);
      var ts = el.querySelector("[data-live-evtype]");
      if (ts) ts.onchange = function () { CLSTATE.evtype = ts.value; repaint(); };
      filterTable(el);
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  function clLogsPage(el, seq, cur, box) {
    var ns = CLSTATE.logNs || "";
    Promise.all([
      wrap(apiGet("/clusters/" + cur.id + "/namespaces")),
      wrap(apiGet("/clusters/" + cur.id + "/workloads/pods", ns ? { namespace: ns } : {}))
    ]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      var nsR = rs[0], wR = rs[1];
      if (wR.err && isAuthErr(wR.err)) return kickLogin();
      var namespaces = nsR.ok ? itemsOf(nsR.data) : [];
      var pods = wR.ok ? itemsOf(wR.data) : [];
      var nsSel = selectHtml([{ v: "", t: "选择命名空间" }].concat(namespaces.map(function (n) { return { v: n.name, t: n.name }; })),
        ns, 'data-live-logns aria-label="命名空间"');
      var tailSel = selectHtml([{ v: "200", t: "200 行" }, { v: "500", t: "500 行" }, { v: "1000", t: "1000 行" }, { v: "2000", t: "2000 行" }],
        CLSTATE.logTail || "500", 'data-live-logtail aria-label="日志行数"');
      var podTable = wR.ok && pods.length ? tableHtml(
        ["Pod", "状态", "就绪", "重启", "节点", "操作"],
        pods.map(function (p) {
          return [
            '<span class="x-name">' + gEsc(p.name) + "</span>",
            tagHtml(p.status || p.phase || "—", String(p.phase || "") === "Running" ? "ok" : "warn"),
            gEsc(p.ready || "—"),
            gEsc(p.restarts == null ? "—" : p.restarts),
            gEsc(p.node || "—"),
            '<span class="x-kacts">' + actBtn("查看日志", "pod-logs", p.name) + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">' + gEsc(wR.ok ? (ns ? "该命名空间没有 Pod。" : "先选择命名空间，再选择 Pod 查看日志。") : errText(wR.err)) + "</p>";

      box.innerHTML = panelHtml("Pod 日志", ns ? "命名空间 " + ns : "选择命名空间",
        toolbarHtml(nsSel + tailSel + '<span class="x-frac">' + gEsc(wR.ok ? pods.length + " 个 Pod" : "") + "</span>") +
        podTable +
        '<div id="liveLogBox" style="margin-top:12px">' +
        (CLSTATE.logPod && ns ? loadingPanel("正在加载 " + CLSTATE.logPod + " 的日志…") : "") + "</div>");

      var nsEl = el.querySelector("[data-live-logns]");
      if (nsEl) nsEl.onchange = function () { CLSTATE.logNs = nsEl.value; CLSTATE.logPod = ""; repaint(); };
      var tailEl = el.querySelector("[data-live-logtail]");
      if (tailEl) tailEl.onchange = function () {
        CLSTATE.logTail = tailEl.value;
        if (CLSTATE.logPod && ns) loadPodLogs(seq, el, cur, ns, CLSTATE.logPod);
      };
      bindActs(el, function (act, id) {
        if (act !== "pod-logs") return;
        CLSTATE.logPod = id;
        loadPodLogs(seq, el, cur, ns, id);
      });
      if (CLSTATE.logPod && ns) loadPodLogs(seq, el, cur, ns, CLSTATE.logPod);
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  function loadPodLogs(seq, el, cur, ns, pod) {
    var box = $id("liveLogBox");
    if (!box) return;
    box.innerHTML = loadingPanel("正在加载 " + pod + " 的日志…");
    apiGet("/clusters/" + cur.id + "/namespaces/" + encodeURIComponent(ns) + "/pods/" + encodeURIComponent(pod) + "/logs",
      { tail_lines: Number(CLSTATE.logTail) || 500 }).then(function (data) {
      if (!alive(seq, el)) return;
      var b = $id("liveLogBox");
      if (!b) return;
      var text = (data && (data.logs || data.log)) || "";
      var lines = text ? text.split("\n").length : 0;
      b.innerHTML = '<div class="x-iamhd"><h2 class="x-h2">' + gEsc(ns + "/" + pod) + '</h2><span class="x-frac">' +
        gEsc(lines ? lines + " 行" : "无输出") + '</span><button type="button" class="x-btn" data-live-refreshlogs>刷新</button></div>' +
        '<pre class="x-log" style="max-height:46vh;overflow:auto;margin:0" aria-label="Pod 日志">' + gEsc(text || "（无日志输出）") + "</pre>";
      var rb = b.querySelector("[data-live-refreshlogs]");
      if (rb) rb.onclick = function () { loadPodLogs(seq, el, cur, ns, pod); };
    }, function (err) {
      if (!alive(seq, el)) return;
      if (isAuthErr(err)) return kickLogin();
      var b2 = $id("liveLogBox");
      if (b2) b2.innerHTML = errorPanel(err);
    });
  }

  var STORAGE_ACCESS_MODES = [
    { v: "ReadWriteOnce", t: "ReadWriteOnce（单节点读写）" },
    { v: "ReadOnlyMany", t: "ReadOnlyMany（多节点只读）" },
    { v: "ReadWriteMany", t: "ReadWriteMany（多节点读写）" },
    { v: "ReadWriteOncePod", t: "ReadWriteOncePod（单 Pod 读写）" }
  ];

  function storageCanWrite() {
    var x = xs();
    return !!(x && typeof x.can === "function" && x.can("storage:write"));
  }

  function storageAccessChecks(selected) {
    var values = selected && selected.length ? selected : ["ReadWriteOnce"];
    return '<fieldset class="x-radios" style="display:grid;gap:6px"><legend>访问模式</legend>' +
      STORAGE_ACCESS_MODES.map(function (m) {
        return '<label><input type="checkbox" name="access_modes" value="' + gEsc(m.v) + '"' +
          (values.indexOf(m.v) >= 0 ? " checked" : "") + " /> " + gEsc(m.t) + "</label>";
      }).join("") + "</fieldset>";
  }

  function storageSelectedModes(f, bad) {
    var modes = Array.prototype.slice.call(f.querySelectorAll('input[name="access_modes"]:checked')).map(function (el) { return el.value; });
    if (!modes.length) { bad("至少选择一个访问模式"); return null; }
    return modes;
  }

  function storageLabelsField(labels) {
    return '<label><span>标签 JSON</span><textarea class="x-input x-mono" name="labels" rows="3" spellcheck="false">' +
      gEsc(JSON.stringify(labels || {}, null, 2)) + "</textarea></label>";
  }

  function storageJSONObject(f, field, label, bad) {
    var input = f.elements[field];
    var raw = String(input && input.value || "").trim();
    if (!raw) return {};
    var value;
    try { value = JSON.parse(raw); } catch (e) {
      bad(label + " 必须是有效 JSON", input);
      return null;
    }
    if (!value || Array.isArray(value) || typeof value !== "object") {
      bad(label + " 必须是 JSON 对象", input);
      return null;
    }
    var badKey = Object.keys(value).filter(function (k) { return typeof value[k] !== "string"; })[0];
    if (badKey !== undefined) {
      bad(label + " 的值必须是字符串（" + badKey + "）", input);
      return null;
    }
    return value;
  }

  function storageLines(text) {
    return String(text || "").split(/[\n,]+/).map(function (s) { return s.trim(); }).filter(Boolean);
  }

  function storageClassSelect(classes, value, emptyLabel, attrs) {
    var items = [{ v: "", t: emptyLabel || "不指定" }].concat((classes || []).map(function (c) { return { v: c.name, t: c.name }; }));
    if (value && !(classes || []).some(function (c) { return c.name === value; })) items.push({ v: value, t: value });
    return selectHtml(items, value || "", (attrs ? attrs + " " : "") + 'name="storage_class"');
  }

  function storageSubmit(promise, okMsg) {
    return doAction(promise, null, function (data) {
      closeModal();
      repaint();
      if (data) toastMsg(okMsg);
    });
  }

  function storageMissingIdentity(item) {
    if (!item || !item.uid || !item.resource_version) {
      toastMsg("缺少 UID 或 resource_version，请刷新后重试");
      return true;
    }
    return false;
  }

  function storageClassDialog(item, ctx) {
    var editing = !!item;
    if (editing && !item.resource_version) return toastMsg("缺少 resource_version，请刷新后重试");
    var name = editing ? item.name : "";
    var provisioner = editing ? item.provisioner : "";
    var reclaim = editing ? item.reclaim_policy : "Delete";
    var binding = editing ? item.volume_binding_mode : "Immediate";
    var allow = editing ? !!item.allow_volume_expansion : true;
    var isDefault = editing ? !!item.default : false;
    var parameters = JSON.stringify((editing && item.parameters) || {}, null, 2);
    var mounts = ((editing && item.mount_options) || []).join("\n");
    var body =
      '<div class="x-row2">' +
      '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" pattern="[a-z0-9]([-a-z0-9.]*[a-z0-9])?" value="' +
      gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label>" +
      '<label><span class="x-req">Provisioner</span><input class="x-input x-mono" name="provisioner" required maxlength="253" value="' +
      gEsc(provisioner) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
      '<div class="x-row2">' +
      '<label><span>回收策略</span>' + selectHtml([{ v: "Delete", t: "Delete（删除后端存储）" }, { v: "Retain", t: "Retain（保留）" }], reclaim, (editing ? 'disabled title="StorageClass 创建后不可修改"' : "") + ' name="reclaim_policy"') + "</label>" +
      '<label><span>绑定模式</span>' + selectHtml([{ v: "Immediate", t: "Immediate" }, { v: "WaitForFirstConsumer", t: "WaitForFirstConsumer" }], binding, editing ? "disabled name=\"volume_binding_mode\"" : 'name="volume_binding_mode"') + "</label></div>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="allow_volume_expansion"' + (allow ? " checked" : "") + " />允许卷扩容</label>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="is_default"' + (isDefault ? " checked" : "") + " />设为默认 StorageClass</label>" +
      '<label><span>参数 JSON</span><textarea class="x-input x-mono" name="parameters" rows="5" spellcheck="false"' + (editing ? " readonly" : "") + ">" + gEsc(parameters) + "</textarea></label>" +
      '<label><span>挂载选项</span><textarea class="x-input x-mono" name="mount_options" rows="2" spellcheck="false" placeholder="每行一个">' + gEsc(mounts) + "</textarea></label>" +
      '<p class="x-frac">' + (editing ? "StorageClass 创建后 Provisioner、回收策略、绑定模式和参数不可修改；如需变更请新建 StorageClass。" : "仅写入 Kubernetes 原生 StorageClass。") + "生产环境由集群变更审批策略决定是否生成审批单。</p>";
    formDialog(editing ? "编辑 StorageClass   " + name : "创建 StorageClass", body, editing ? "保存变更" : "创建", function (f, bad) {
      if (!f.reportValidity()) return;
      var parsed = storageJSONObject(f, "parameters", "参数", bad);
      if (parsed === null) return;
      var nextName = String(f.elements.name.value || "").trim();
      var nextProvisioner = String(f.elements.provisioner.value || "").trim();
      if (!nextName) return bad("名称不能为空", f.elements.name);
      if (!nextProvisioner) return bad("Provisioner 不能为空", f.elements.provisioner);
      var payload = {
        name: nextName,
        provisioner: nextProvisioner,
        reclaim_policy: f.elements.reclaim_policy.value,
        volume_binding_mode: f.elements.volume_binding_mode.value,
        allow_volume_expansion: !!f.elements.allow_volume_expansion.checked,
        is_default: !!f.elements.is_default.checked,
        parameters: parsed,
        mount_options: storageLines(f.elements.mount_options.value)
      };
      if (editing) payload.resource_version = item.resource_version;
      var url = ctx.base + "classes" + (editing ? "/" + encodeURIComponent(item.name) : "");
      storageSubmit(editing ? apiPut(url, payload) : apiPost(url, payload),
        editing ? "已更新 StorageClass " + nextName : "已创建 StorageClass " + nextName);
    }, !editing ? aiDraftConfig("k8s.storageclass.create", "创建一个默认 StorageClass，使用 CSI 提供器，延迟绑定并保留卷。", {
      name: aiField("string", "StorageClass 名称，使用小写字母和短横线", { maxLength: 253 }),
      provisioner: aiField("string", "已安装 CSI 驱动提供的 Provisioner 标识", { maxLength: 253 }),
      reclaim_policy: aiField("enum", "卷删除后的回收策略", { values: ["Delete", "Retain"] }),
      volume_binding_mode: aiField("enum", "卷绑定时机", { values: ["Immediate", "WaitForFirstConsumer"] }),
      allow_volume_expansion: aiField("boolean", "是否允许扩容"),
      is_default: aiField("boolean", "是否设为集群默认 StorageClass"),
      parameters: aiField("string_map", "驱动支持的非敏感参数键值对象", { ui: "json" }),
      mount_options: aiField("array", "每项一个挂载参数")
    }, true) : null);
  }

  function storageClassDelete(item, ctx) {
    if (storageMissingIdentity(item)) return;
    var body = '<p>确认删除 StorageClass <b>' + gEsc(item.name) + "</b>？该操作不可恢复。</p>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 StorageClass</label>' +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="force" />即使仍被 PV/PVC 引用也强制删除（可能导致存储不可用）</label>' +
      '<p class="x-frac">默认在检测到引用时拒绝删除；force 只应在确认无数据依赖后使用。</p>';
    formDialog("删除 StorageClass   " + item.name, body, "删除", function (f) {
      if (!f.reportValidity()) return;
      storageSubmit(apiDel(ctx.base + "classes/" + encodeURIComponent(item.name), {
        uid: item.uid,
        resource_version: item.resource_version,
        force: !!f.elements.force.checked
      }),
        "已删除 StorageClass " + item.name);
    });
  }

  function storagePVCDialog(item, ctx) {
    var editing = !!item;
    if (editing && !item.resource_version) return toastMsg("缺少 resource_version，请刷新后重试");
    var namespace = editing ? item.namespace : "";
    var name = editing ? item.name : "";
    var size = editing ? item.requested : "";
    var body =
      '<div class="x-row2">' +
      '<label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" value="' +
      gEsc(namespace) + '"' + (editing ? " readonly" : "") + " /></label>" +
      '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" value="' +
      gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
      '<div class="x-row2">' +
      '<label><span class="x-req">申请容量</span><input class="x-input x-mono" name="size" required placeholder="例如 10Gi" value="' + gEsc(size) + '" /></label>' +
      '<label><span>StorageClass</span>' + storageClassSelect(ctx.classes, editing ? item.storage_class : "", "使用默认 StorageClass", editing ? "disabled" : "") + "</label></div>" +
      (editing
        ? '<p class="x-frac">PVC 仅支持扩容与标签修改；访问模式、StorageClass 与绑定 PV 不能在此修改。</p>'
        : storageAccessChecks(item && item.access_modes) +
          '<div class="x-row2"><label><span>卷模式</span>' + selectHtml([{ v: "Filesystem", t: "Filesystem" }, { v: "Block", t: "Block" }], "Filesystem", 'name="volume_mode"') + "</label>" +
          '<label><span>绑定已有 PV（可选）</span><input class="x-input x-mono" name="volume_name" placeholder="留空由调度器绑定" /></label></div>') +
      storageLabelsField(editing ? item.labels : {}) +
      '<p class="x-frac">创建与更新均调用现有 PVC 接口；生产环境写入会进入审批网关。</p>';
    formDialog(editing ? "编辑 PVC   " + namespace + "/" + name : "创建 PersistentVolumeClaim", body, editing ? "保存变更" : "创建", function (f, bad) {
      if (!f.reportValidity()) return;
      var labels = storageJSONObject(f, "labels", "标签", bad);
      if (labels === null) return;
      var nextSize = String(f.elements.size.value || "").trim();
      if (!nextSize) return bad("容量不能为空", f.elements.size);
      if (editing) {
        storageSubmit(apiPut(ctx.base + "claims/" + encodeURIComponent(namespace) + "/" + encodeURIComponent(name), {
          size: nextSize,
          labels: labels,
          resource_version: item.resource_version
        }), "已更新 PVC " + namespace + "/" + name);
        return;
      }
      var modes = storageSelectedModes(f, bad);
      if (!modes) return;
      var nextNamespace = String(f.elements.namespace.value || "").trim();
      var nextName = String(f.elements.name.value || "").trim();
      if (!nextNamespace) return bad("命名空间不能为空", f.elements.namespace);
      if (!nextName) return bad("名称不能为空", f.elements.name);
      storageSubmit(apiPost(ctx.base + "claims", {
        name: nextName,
        namespace: nextNamespace,
        storage_class: f.elements.storage_class.value,
        access_modes: modes,
        size: nextSize,
        volume_mode: f.elements.volume_mode.value,
        volume_name: String(f.elements.volume_name.value || "").trim(),
        labels: labels
      }), "已创建 PVC " + nextNamespace + "/" + nextName);
    }, !editing ? aiDraftConfig("k8s.pvc.create", "在 prod 命名空间为 web 创建 10Gi 的块存储 PVC，使用 fast-ssd，单节点读写。", {
      namespace: aiField("string", "目标 Kubernetes 命名空间", { maxLength: 63 }),
      name: aiField("string", "PVC 名称，使用小写字母和短横线", { maxLength: 253 }),
      size: aiField("string", "申请容量，例如 10Gi"),
      storage_class: aiField("enum", "现有 StorageClass；空字符串表示使用默认类", {
        values: [""].concat((ctx.classes || []).map(function (c) { return c.name; }))
      }),
      access_modes: aiField("array", "访问模式，可选择一个或多个", {
        values: STORAGE_ACCESS_MODES.map(function (mode) { return mode.v; }),
        ui: "checks"
      }),
      volume_mode: aiField("enum", "卷模式", { values: ["Filesystem", "Block"] }),
      volume_name: aiField("string", "可选的预绑定 PV 名称；未指定时留空"),
      labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" })
    }, true) : null);
  }

  function storagePVCDelete(item, ctx) {
    if (storageMissingIdentity(item)) return;
    var key = item.namespace + "/" + item.name;
    var body = '<p>确认删除 PVC <b>' + gEsc(key) + "</b>？删除后工作负载可能立即失去存储。</p>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 PVC</label>' +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="allow_reclaim_delete" />若绑定 PV 的回收策略为 Delete，允许连带删除卷（数据将丢失）</label>' +
      '<p class="x-frac">默认拒绝连带删除回收策略为 Delete 的卷；只有确认数据可丢弃时才勾选第二项。</p>';
    formDialog("删除 PVC   " + key, body, "删除", function (f) {
      if (!f.reportValidity()) return;
      storageSubmit(apiDel(ctx.base + "claims/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name), {
        uid: item.uid,
        resource_version: item.resource_version,
        confirm: key,
        allow_reclaim_delete: !!f.elements.allow_reclaim_delete.checked
      }), "已删除 PVC " + key);
    });
  }

  function storagePVDialog(item, ctx) {
    var editing = !!item;
    if (editing && (!item.uid || !item.resource_version)) return toastMsg("缺少 UID 或 resource_version，请刷新后重试");
    if (editing && !item.host_path && !item.nfs) return toastMsg("该 PV 的卷来源不是 HostPath/NFS，暂不支持编辑");
    var sourceSummary = editing
      ? (item.host_path ? "HostPath " + item.host_path : "NFS " + item.nfs.server + ":" + item.nfs.path + (item.nfs.read_only ? "（只读）" : ""))
      : "";
    var body =
      '<div class="x-row2">' +
      '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" value="' +
      gEsc(editing ? item.name : "") + '"' + (editing ? " readonly" : "") + " /></label>" +
      '<label><span class="x-req">容量</span><input class="x-input x-mono" name="capacity" required placeholder="例如 20Gi" value="' +
      gEsc(editing ? item.capacity : "") + '" /></label></div>' +
      storageAccessChecks(editing ? item.access_modes : []) +
      '<div class="x-row2">' +
      '<label><span>回收策略</span>' + selectHtml([{ v: "Retain", t: "Retain（保留）" }, { v: "Delete", t: "Delete（删除后端存储）" }], editing ? item.reclaim_policy : "Retain", 'name="reclaim_policy"') + "</label>" +
      '<label><span>StorageClass</span>' + storageClassSelect(ctx.classes, editing ? item.storage_class : "", "不指定（静态 PV）") + "</label></div>" +
      '<div class="x-row2">' +
      '<label><span>卷模式</span>' + selectHtml([{ v: "Filesystem", t: "Filesystem" }, { v: "Block", t: "Block" }], editing ? item.volume_mode : "Filesystem", 'name="volume_mode"') + "</label>" +
      (editing
        ? '<label><span>卷来源（不可修改）</span><input class="x-input x-mono" value="' + gEsc(sourceSummary) + '" readonly /></label>'
        : '<label><span>卷来源</span>' + selectHtml([{ v: "host_path", t: "HostPath" }, { v: "nfs", t: "NFS" }], "host_path", 'name="source_type"') + "</label>") +
      "</div>" +
      (editing
        ? '<p class="x-frac">PV 来源不可修改；仅可调整容量、访问模式、回收策略、StorageClass、卷模式与标签。</p>'
        : '<div data-storage-src="host_path"><label><span class="x-req">HostPath 路径</span><input class="x-input x-mono" name="host_path" placeholder="/data/volume" /></label></div>' +
          '<div data-storage-src="nfs" hidden><div class="x-row2"><label><span class="x-req">NFS Server</span><input class="x-input x-mono" name="nfs_server" placeholder="nfs.example" /></label>' +
          '<label><span class="x-req">NFS Path</span><input class="x-input x-mono" name="nfs_path" placeholder="/export/data" /></label></div>' +
          '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="nfs_read_only" />只读挂载</label></div>') +
      storageLabelsField(editing ? item.labels : {}) +
      '<p class="x-frac">静态 PV 只会以 HostPath 或 NFS 创建；不会自动绑定或伪造 Claim 状态。</p>';
    var form = formDialog(editing ? "编辑 PV   " + item.name : "创建 PersistentVolume", body, editing ? "保存变更" : "创建", function (f, bad) {
      if (!f.reportValidity()) return;
      var labels = storageJSONObject(f, "labels", "标签", bad);
      if (labels === null) return;
      var modes = storageSelectedModes(f, bad);
      if (!modes) return;
      var nextName = String(f.elements.name.value || "").trim();
      var capacity = String(f.elements.capacity.value || "").trim();
      if (!nextName) return bad("名称不能为空", f.elements.name);
      if (!capacity) return bad("容量不能为空", f.elements.capacity);
      var payload = {
        name: nextName,
        capacity: capacity,
        access_modes: modes,
        reclaim_policy: f.elements.reclaim_policy.value,
        storage_class: f.elements.storage_class.value,
        volume_mode: f.elements.volume_mode.value,
        labels: labels
      };
      if (editing) {
        payload.uid = item.uid;
        payload.resource_version = item.resource_version;
        if (item.host_path) payload.host_path = item.host_path;
        if (item.nfs) payload.nfs = { server: item.nfs.server, path: item.nfs.path, read_only: !!item.nfs.read_only };
        storageSubmit(apiPut(ctx.base + "volumes/" + encodeURIComponent(item.name), payload), "已更新 PV " + nextName);
        return;
      }
      if (f.elements.source_type.value === "nfs") {
        var server = String(f.elements.nfs_server.value || "").trim();
        var nfsPath = String(f.elements.nfs_path.value || "").trim();
        if (!server || !nfsPath) return bad("NFS 需要填写 server 与 path", !server ? f.elements.nfs_server : f.elements.nfs_path);
        payload.nfs = { server: server, path: nfsPath, read_only: !!f.elements.nfs_read_only.checked };
      } else {
        var hostPath = String(f.elements.host_path.value || "").trim();
        if (!hostPath) return bad("HostPath 路径不能为空", f.elements.host_path);
        payload.host_path = hostPath;
      }
      storageSubmit(apiPost(ctx.base + "volumes", payload), "已创建 PV " + nextName);
    }, !editing ? aiDraftConfig("k8s.pv.create", "创建一个 20Gi 的静态 NFS PV，地址 nfs.example:/export/data，只读并保留卷。", {
      name: aiField("string", "PV 名称，使用小写字母和短横线", { maxLength: 253 }),
      capacity: aiField("string", "PV 容量，例如 20Gi"),
      access_modes: aiField("array", "访问模式，可选择一个或多个", {
        values: STORAGE_ACCESS_MODES.map(function (mode) { return mode.v; }),
        ui: "checks"
      }),
      reclaim_policy: aiField("enum", "卷删除后的回收策略", { values: ["Retain", "Delete"] }),
      storage_class: aiField("enum", "现有 StorageClass；空字符串表示静态卷不指定", {
        values: [""].concat((ctx.classes || []).map(function (c) { return c.name; }))
      }),
      volume_mode: aiField("enum", "卷模式", { values: ["Filesystem", "Block"] }),
      source_type: aiField("enum", "卷来源", { values: ["host_path", "nfs"] }),
      host_path: aiField("string", "仅 HostPath 来源使用的节点路径"),
      nfs_server: aiField("string", "仅 NFS 来源使用的 NFS 主机名或 IP"),
      nfs_path: aiField("string", "仅 NFS 来源使用的导出路径"),
      nfs_read_only: aiField("boolean", "NFS 是否只读挂载"),
      labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" })
    }, true) : null);
    if (form && form.elements.source_type) {
      var syncSource = function () {
        var nfs = form.elements.source_type.value === "nfs";
        var hp = form.querySelector('[data-storage-src="host_path"]');
        var nf = form.querySelector('[data-storage-src="nfs"]');
        if (hp) hp.hidden = nfs;
        if (nf) nf.hidden = !nfs;
      };
      form.elements.source_type.onchange = syncSource;
      syncSource();
    }
  }

  function storagePVDelete(item, ctx) {
    if (storageMissingIdentity(item)) return;
    var body = '<p>确认删除 PV <b>' + gEsc(item.name) + "</b>？回收策略为 <b>" + gEsc(item.reclaim_policy || "—") +
      "</b>；删除后可能清理后端数据。</p>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 PersistentVolume</label>' +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="force" />PV 仍绑定 Claim 时强制删除（仅在确认 Claim 已移除时使用）</label>' +
      '<p class="x-frac">默认拒绝删除仍绑定 Claim 的 PV。</p>';
    formDialog("删除 PV   " + item.name, body, "删除", function (f) {
      if (!f.reportValidity()) return;
      storageSubmit(apiDel(ctx.base + "volumes/" + encodeURIComponent(item.name), {
        uid: item.uid,
        resource_version: item.resource_version,
        confirm: item.name,
        force: !!f.elements.force.checked
      }), "已删除 PV " + item.name);
    });
  }

  function storageSnapshotDialog(ctx) {
    if (!ctx.snapshotCap || !ctx.snapshotCap.supported) return toastMsg("当前集群未确认 VolumeSnapshot 能力");
    var sourceOptions = (ctx.claims || []).map(function (c) { return { v: c.namespace + "/" + c.name, t: c.namespace + "/" + c.name }; });
    var classOptions = (ctx.snapshotCap.classes || []).map(function (c) { return { v: c, t: c }; });
    var body =
      '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" pattern="[a-z0-9]([-a-z0-9.]*[a-z0-9])?" /></label>' +
      '<label><span class="x-req">源 PVC</span>' + selectHtml(sourceOptions, "", 'name="source_pvc"') + "</label>" +
      (classOptions.length
        ? '<label><span>VolumeSnapshotClass</span>' + selectHtml([{ v: "", t: "不指定（使用默认类）" }].concat(classOptions), "", 'name="class_name"') + "</label>"
        : "") +
      storageLabelsField({}) +
      '<p class="x-frac">仅提交 VolumeSnapshot 创建请求；就绪状态由 snapshot controller 回报，本页不会伪造。</p>';
    formDialog("创建 VolumeSnapshot", body, "创建", function (f, bad) {
      if (!f.reportValidity()) return;
      var source = String(f.elements.source_pvc.value || "");
      var parts = source.split("/");
      if (parts.length !== 2 || !parts[0] || !parts[1]) return bad("请选择源 PVC", f.elements.source_pvc);
      var labels = storageJSONObject(f, "labels", "标签", bad);
      if (labels === null) return;
      var nextName = String(f.elements.name.value || "").trim();
      if (!nextName) return bad("名称不能为空", f.elements.name);
      var payload = { namespace: parts[0], name: nextName, source_pvc: parts[1], labels: labels };
      if (f.elements.class_name) payload.class_name = f.elements.class_name.value;
      storageSubmit(apiPost(ctx.base + "snapshots", payload), "已创建 VolumeSnapshot " + parts[0] + "/" + nextName);
    }, aiDraftConfig("k8s.snapshot.create", "为 prod/data 创建一个快照，名称 data-before-upgrade。", {
      name: aiField("string", "快照名称，使用小写字母和短横线", { maxLength: 253 }),
      source_pvc: aiField("enum", "要快照的现有 PVC，格式为 namespace/name", {
        values: sourceOptions.map(function (option) { return option.v; })
      }),
      class_name: aiField("enum", "现有 VolumeSnapshotClass；空字符串表示使用默认类", {
        values: [""].concat(classOptions)
      }),
      labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" })
    }, true));
  }

  function storageSnapshotDelete(item, ctx) {
    if (storageMissingIdentity(item)) return;
    var key = item.namespace + "/" + item.name;
    var body = '<p>确认删除 VolumeSnapshot <b>' + gEsc(key) + "</b>？恢复点将不可用。</p>" +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 VolumeSnapshot</label>';
    formDialog("删除 VolumeSnapshot   " + key, body, "删除", function (f) {
      if (!f.reportValidity()) return;
      storageSubmit(apiDel(ctx.base + "snapshots/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name), {
        uid: item.uid,
        resource_version: item.resource_version
      }), "已删除 VolumeSnapshot " + key);
    });
  }

  function clStoragePage(el, seq, cur, box) {
    var base = "/clusters/" + cur.id + "/storage/";
    Promise.all([
      wrap(apiGet(base + "capabilities")),
      wrap(apiGet(base + "classes")),
      wrap(apiGet(base + "volumes")),
      wrap(apiGet(base + "claims")),
      wrap(apiGet(base + "snapshots"))
    ]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      for (var i = 0; i < rs.length; i++) if (rs[i].err && isAuthErr(rs[i].err)) return kickLogin();
      var capabilities = rs[0].ok ? (rs[0].data || {}) : {};
      var classes = rs[1].ok ? itemsOf(rs[1].data) : [];
      var volumes = rs[2].ok ? itemsOf(rs[2].data) : [];
      var claims = rs[3].ok ? itemsOf(rs[3].data) : [];
      var snapshots = rs[4].ok ? itemsOf(rs[4].data) : [];
      var snapshotCap = capabilities.snapshots || (rs[4].data && rs[4].data.capability) || {};
      var canWrite = storageCanWrite();
      var ctx = { base: base, classes: classes, volumes: volumes, claims: claims, snapshotCap: snapshotCap };
      var actionHead = canWrite ? ["操作"] : [];
      var actCell = function (html) { return '<span class="x-kacts">' + html + "</span>"; };
      var section = function (title, result, items, cols, rows, empty, tools) {
        if (!result.ok) return panelHtml(title, "", '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(result.err)) + "</p>", "");
        return panelHtml(title, items.length + " 个", items.length ? tableHtml(cols, rows) : '<p class="x-frac" style="margin:0">' + gEsc(empty) + "</p>", tools || "");
      };
      var classRows = classes.map(function (x) {
        var cells = [
          '<span class="x-name">' + gEsc(x.name) + "</span>" + (x.default ? ' <span class="x-tag x-green x-tag--s">默认</span>' : ""),
          mono(x.provisioner || "—"), gEsc(x.reclaim_policy || "—"), gEsc(x.volume_binding_mode || "—"),
          x.allow_volume_expansion ? "支持" : "不支持", gEsc(fmtTime(x.created_at))
        ];
        if (canWrite) cells.push(actCell(
          actBtn("编辑", "storage-sc-edit", x.name, { disabled: !x.resource_version, title: x.resource_version ? "" : "缺少 resource_version，请刷新" }) +
          actBtn("删除", "storage-sc-del", x.name, { disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新" })));
        return cells;
      });
      var pvRows = volumes.map(function (x) {
        var editable = !!(x.host_path || x.nfs);
        var cells = [
          '<span class="x-name">' + gEsc(x.name) + "</span>", tagHtml(x.phase || "—", x.phase === "Bound" ? "ok" : "muted"),
          gEsc(x.capacity || "—"), gEsc((x.access_modes || []).join(", ") || "—"), gEsc(x.storage_class || "—"),
          gEsc(x.claim_ref || "—"), gEsc(x.reclaim_policy || "—")
        ];
        if (canWrite) cells.push(actCell(
          actBtn("编辑", "storage-pv-edit", x.name, { disabled: !editable || !x.resource_version, title: editable ? (x.resource_version ? "" : "缺少 resource_version，请刷新") : "仅支持 HostPath/NFS PV 编辑" }) +
          actBtn("删除", "storage-pv-del", x.name, { disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新" })));
        return cells;
      });
      var pvcRows = claims.map(function (x) {
        var cells = [
          '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>",
          tagHtml(x.phase || "—", x.phase === "Bound" ? "ok" : "warn"), gEsc(x.requested || "—"),
          gEsc(x.capacity || "—"), gEsc((x.access_modes || []).join(", ") || "—"),
          gEsc(x.storage_class || "—"), gEsc(x.volume_name || "—")
        ];
        if (canWrite) cells.push(actCell(
          actBtn("编辑", "storage-pvc-edit", x.namespace + "/" + x.name, { disabled: !x.resource_version, title: x.resource_version ? "" : "缺少 resource_version，请刷新" }) +
          actBtn("删除", "storage-pvc-del", x.namespace + "/" + x.name, { disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新" })));
        return cells;
      });
      var snapshotRows = snapshots.map(function (x) {
        var cells = [
          '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>",
          x.ready ? tagHtml("Ready", "ok") : tagHtml(x.error || "Pending", "warn"),
          gEsc(x.source_pvc || "—"), gEsc(x.class || "—"), gEsc(x.restore_size || "—"), gEsc(fmtTime(x.created_at))
        ];
        if (canWrite && snapshotCap.supported) cells.push(actCell(
          actBtn("删除", "storage-snapshot-del", x.namespace + "/" + x.name, { disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新" })));
        return cells;
      });
      var snapshotNote = rs[4].ok
        ? (snapshotCap.supported ? "快照 API 与存储驱动已发现。" : (snapshotCap.reason || "当前集群未确认快照能力，不能创建快照。"))
        : errText(rs[4].err);
      var accessNote = canWrite
        ? '<p class="x-frac" style="margin:0 0 12px">创建、修改与删除均调用现有集群变更接口；生产环境按审批策略返回待审批。</p>'
        : '<p class="x-frac" style="margin:0 0 12px">当前账号没有 storage:write 权限，以下为只读清单。</p>';
      var newClass = canWrite ? '<button class="x-btn x-btn--pri" type="button" data-storage-new="class">+ 新建 StorageClass</button>' : "";
      var newPVC = canWrite ? '<button class="x-btn x-btn--pri" type="button" data-storage-new="pvc">+ 新建 PVC</button>' : "";
      var newPV = canWrite ? '<button class="x-btn x-btn--pri" type="button" data-storage-new="pv">+ 新建 PV</button>' : "";
      var newSnapshot = canWrite && snapshotCap.supported && claims.length
        ? '<button class="x-btn x-btn--pri" type="button" data-storage-new="snapshot">+ 新建快照</button>'
        : "";
      box.innerHTML = accessNote +
        panelHtml("存储能力", snapshotCap.supported ? "VolumeSnapshot 可用" : "快照未接入",
          '<p class="x-frac" style="margin:0">' + gEsc(snapshotNote) + "</p>") +
        section("StorageClass", rs[1], classes, ["名称", "Provisioner", "回收策略", "绑定模式", "扩容", "创建时间"].concat(actionHead), classRows, "集群没有 StorageClass。", newClass) +
        section("PersistentVolume", rs[2], volumes, ["名称", "状态", "容量", "访问模式", "StorageClass", "Claim", "回收策略"].concat(actionHead), pvRows, "集群没有 PV。", newPV) +
        section("PersistentVolumeClaim", rs[3], claims, ["命名空间 / 名称", "状态", "申请容量", "已绑定容量", "访问模式", "StorageClass", "PV"].concat(actionHead), pvcRows, "集群没有 PVC。", newPVC) +
        section("VolumeSnapshot", rs[4], snapshots, ["命名空间 / 名称", "状态", "源 PVC", "快照类", "恢复容量", "创建时间"].concat(canWrite && snapshotCap.supported ? actionHead : []), snapshotRows, snapshotNote, newSnapshot);

      var findClass = function (name) { var found = null; classes.forEach(function (x) { if (x.name === name) found = x; }); return found; };
      var findPV = function (name) { var found = null; volumes.forEach(function (x) { if (x.name === name) found = x; }); return found; };
      var findPVC = function (key) { var found = null; claims.forEach(function (x) { if (x.namespace + "/" + x.name === key) found = x; }); return found; };
      var findSnapshot = function (key) { var found = null; snapshots.forEach(function (x) { if (x.namespace + "/" + x.name === key) found = x; }); return found; };

      bindActs(el, function (act, id) {
        if (!canWrite) return;
        var item;
        if (act === "storage-sc-edit") { item = findClass(id); if (item) storageClassDialog(item, ctx); return; }
        if (act === "storage-sc-del") { item = findClass(id); if (item) storageClassDelete(item, ctx); return; }
        if (act === "storage-pv-edit") { item = findPV(id); if (item) storagePVDialog(item, ctx); return; }
        if (act === "storage-pv-del") { item = findPV(id); if (item) storagePVDelete(item, ctx); return; }
        if (act === "storage-pvc-edit") { item = findPVC(id); if (item) storagePVCDialog(item, ctx); return; }
        if (act === "storage-pvc-del") { item = findPVC(id); if (item) storagePVCDelete(item, ctx); return; }
        if (act === "storage-snapshot-del") { item = findSnapshot(id); if (item) storageSnapshotDelete(item, ctx); }
      });
      Array.prototype.forEach.call(el.querySelectorAll("[data-storage-new]"), function (btn) {
        btn.onclick = function () {
          if (!storageCanWrite()) return toastMsg("没有 storage:write 权限");
          var kind = btn.getAttribute("data-storage-new");
          if (kind === "class") storageClassDialog(null, ctx);
          else if (kind === "pvc") storagePVCDialog(null, ctx);
          else if (kind === "pv") storagePVDialog(null, ctx);
          else if (kind === "snapshot") storageSnapshotDialog(ctx);
        };
      });
      filterTable(box);
    });
  }

  function clNetworkPage(el, seq, cur, box, view) {
    var base = "/clusters/" + cur.id + "/network/";
    var plan = networkResourcePlan(view);
    var isService = plan.endpoint === "services";
    Promise.all([wrap(apiGet(base + plan.endpoint))]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      if (rs[0].err && isAuthErr(rs[0].err)) return kickLogin();
      var result = rs[0];
      var services = isService && result.ok ? itemsOf(result.data) : [];
      var ingresses = !isService && result.ok ? itemsOf(result.data) : [];
      var api = xs();
      var canWrite = !!(api && typeof api.can === "function" && api.can("network:write"));
      var section = function (title, result, items, cols, rows, empty, tools) {
        if (!result.ok) return panelHtml(title, "", '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(result.err)) + "</p>");
        return panelHtml(title, items.length + " 个", items.length ? tableHtml(cols, rows) : '<p class="x-frac" style="margin:0">' + gEsc(empty) + "</p>", tools || "");
      };
      var serviceRows = services.map(function (x) {
        var lb = (x.load_balancer || []).map(function (a) { return a.ip || a.hostname; }).filter(Boolean).join(", ");
        var ports = (x.ports || []).map(function (p) { return (p.port || "") + "/" + (p.protocol || "TCP") + (p.nodePort ? "   node " + p.nodePort : ""); }).join(", ");
        var cells = [
          '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>", gEsc(x.type || "—"),
          mono(x.cluster_ip || x.external_name || "—"), gEsc(ports || "—"),
          gEsc(x.address_status === "pending" ? "等待负载均衡地址" : (lb || (x.external_ips || []).join(", ") || "—")),
          gEsc(fmtTime(x.created_at))
        ];
        if (canWrite) cells.push('<span class="x-kacts">' +
          actBtn("编辑", "network-service-edit", x.namespace + "/" + x.name, {
            disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
          }) +
          actBtn("删除", "network-service-del", x.namespace + "/" + x.name, {
            disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
          }) + "</span>");
        return cells;
      });
      var ingressRows = ingresses.map(function (x) {
        var address = (x.load_balancer || []).map(function (a) { return a.ip || a.hostname; }).filter(Boolean).join(", ");
        var cells = [
          '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>",
          gEsc((x.hosts || []).join(", ") || "*"), gEsc(x.class_name || "—"),
          x.address_status === "ready" ? tagHtml(address || "Ready", "ok") : tagHtml("Pending", "warn"),
          gEsc((x.tls || []).length + " 个 TLS"), gEsc(fmtTime(x.created_at))
        ];
        if (canWrite) cells.push('<span class="x-kacts">' +
          actBtn("编辑", "network-ingress-edit", x.namespace + "/" + x.name, {
            disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
          }) +
          actBtn("删除", "network-ingress-del", x.namespace + "/" + x.name, {
            disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
          }) + "</span>");
        return cells;
      });
      var writeNote = canWrite
        ? isService
          ? '<p class="x-frac" style="margin:0 0 12px">Service 提供服务发现与四层转发；外部流量策略只控制节点间转发，不等同于 NetworkPolicy 的 Pod 通信规则。写操作按集群审批策略提交。</p>'
          : '<p class="x-frac" style="margin:0 0 12px">此处配置域名与 HTTP/HTTPS 路由。NetworkPolicy 是作用于 Pod 的 L3/L4 访问控制，请在网络策略页面管理。</p>'
        : '<p class="x-frac" style="margin:0 0 12px">当前账号没有 network:write 权限，页面为只读。</p>';
      box.innerHTML = writeNote + (isService
        ? section(plan.resource, result, services, ["命名空间 / 名称", "类型", "集群地址", "端口", "外部地址", "创建时间"].concat(canWrite ? ["操作"] : []),
          serviceRows, "集群没有 Service。", canWrite ? '<button class="x-btn x-btn--pri" type="button" data-network-new="service">+ 新建 Service</button>' : "")
        : section(plan.resource, result, ingresses, ["命名空间 / 名称", "主机", "IngressClass", "负载均衡状态", "TLS", "创建时间"].concat(canWrite ? ["操作"] : []),
          ingressRows, "集群没有 Ingress。", canWrite ? '<button class="x-btn x-btn--pri" type="button" data-network-new="ingress">+ 新建 Ingress</button>' : ""));
      var formError = function (bad, message, field) { bad(message, field); };
      var parseArray = function (form, field, label, bad) {
        var input = form.elements[field], value;
        try { value = JSON.parse(String(input && input.value || "[]")); }
        catch (e) { bad(label + " 必须是有效 JSON 数组", input); return null; }
        if (!Array.isArray(value)) { bad(label + " 必须是 JSON 数组", input); return null; }
        return value;
      };
      var networkDialog = function (kind, item) {
        var editing = !!item, isService = kind === "service";
        if (editing && (!item.uid || !item.resource_version)) return toastMsg("缺少 UID 或 resource_version，请刷新后重试");
        var title = isService ? (editing ? "编辑 Service" : "创建 Service") : (editing ? "编辑 Ingress" : "创建 Ingress");
        var namespace = editing ? item.namespace : "default", name = editing ? item.name : "";
        var body;
        if (isService) {
          var serviceType = editing ? item.type : "ClusterIP";
          var ports = (editing && item.ports) || [{ name: "", port: 80, targetPort: 80, protocol: "TCP" }];
          body = '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" value="' + gEsc(namespace) + '"' + (editing ? " readonly" : "") + " /></label>" +
            '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="63" pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?" value="' + gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
            '<div class="x-row2"><label><span>类型</span>' + selectHtml([{ v: "ClusterIP", t: "ClusterIP" }, { v: "NodePort", t: "NodePort" }, { v: "LoadBalancer", t: "LoadBalancer" }, { v: "ExternalName", t: "ExternalName" }], serviceType, 'name="type"') + "</label>" +
            '<label><span>ExternalName（ExternalName 类型）</span><input class="x-input x-mono" name="external_name" value="' + gEsc(item && item.external_name || "") + '" /></label></div>' +
            '<label><span>端口 JSON</span><textarea class="x-input x-mono" name="ports" rows="4" spellcheck="false">' + gEsc(JSON.stringify(ports, null, 2)) + "</textarea></label>" +
            '<label><span>Pod 选择器 JSON</span><textarea class="x-input x-mono" name="selector" rows="3" spellcheck="false">' + gEsc(JSON.stringify(item && item.selector || {}, null, 2)) + "</textarea></label>" +
            '<div class="x-row2"><label><span>外部 IP（逗号分隔）</span><input class="x-input x-mono" name="external_ips" value="' + gEsc((item && item.external_ips || []).join(", ")) + '" /></label>' +
            '<label><span>LoadBalancer IP</span><input class="x-input x-mono" name="load_balancer_ip" value="' + gEsc(item && item.load_balancer_ip || "") + '" /></label></div>' +
            '<div class="x-row2"><label><span>LoadBalancer Class</span><input class="x-input x-mono" name="load_balancer_class" value="' + gEsc(item && item.load_balancer_class || "") + '" /></label>' +
            '<label><span>外部流量策略</span>' + selectHtml([{ v: "", t: "默认" }, { v: "Cluster", t: "Cluster" }, { v: "Local", t: "Local" }], item && item.external_traffic_policy || (serviceType === "NodePort" || serviceType === "LoadBalancer" ? "Cluster" : ""), 'name="external_traffic_policy"') + "</label></div>" +
            '<label><span>Session Affinity</span>' + selectHtml([{ v: "None", t: "None" }, { v: "ClientIP", t: "ClientIP" }], item && item.session_affinity || "None", 'name="session_affinity"') + "</label>" +
            '<div class="x-row2">' + storageLabelsField(item && item.labels || {}) + '<label><span>Annotations JSON</span><textarea class="x-input x-mono" name="annotations" rows="3">' + gEsc(JSON.stringify(item && item.annotations || {}, null, 2)) + "</textarea></label></div>";
        } else {
          var rules = (editing && item.rules) || [{
            host: "",
            http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "", port: { number: 80 } } } }] }
          }];
          body = '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" value="' + gEsc(namespace) + '"' + (editing ? " readonly" : "") + " /></label>" +
            '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" value="' + gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
            '<label><span>IngressClass</span><input class="x-input x-mono" name="class_name" value="' + gEsc(item && item.class_name || "") + '" /></label>' +
            '<label><span>路由规则 JSON</span><textarea class="x-input x-mono" name="rules" rows="9" spellcheck="false">' + gEsc(JSON.stringify(rules, null, 2)) + "</textarea></label>" +
            '<label><span>TLS JSON</span><textarea class="x-input x-mono" name="tls" rows="4" spellcheck="false">' + gEsc(JSON.stringify(item && item.tls || [], null, 2)) + "</textarea></label>" +
            '<div class="x-row2">' + storageLabelsField(item && item.labels || {}) + '<label><span>Annotations JSON</span><textarea class="x-input x-mono" name="annotations" rows="3">' + gEsc(JSON.stringify(item && item.annotations || {}, null, 2)) + "</textarea></label></div>";
        }
        body += '<label><span class="x-req">变更原因</span><input class="x-input" name="reason" required maxlength="500" /></label>';
        formDialog(title + (editing ? "   " + namespace + "/" + name : ""), body, editing ? "保存变更" : "创建", function (f, bad) {
          if (!f.reportValidity()) return;
          var labels = storageJSONObject(f, "labels", "标签", bad);
          var annotations = storageJSONObject(f, "annotations", "Annotations", bad);
          if (labels === null || annotations === null) return;
          var nextName = String(f.elements.name.value || "").trim();
          var nextNamespace = String(f.elements.namespace.value || "").trim();
          if (!nextName || !nextNamespace) return bad("名称与命名空间不能为空");
          var payload, path, method;
          if (isService) {
            var selector = storageJSONObject(f, "selector", "Pod 选择器", bad);
            if (selector === null) return;
            var servicePorts = parseArray(f, "ports", "端口", bad);
            if (servicePorts === null) return;
            var nextType = f.elements.type.value;
            if (nextType === "ExternalName") {
              servicePorts = [];
              selector = {};
            }
            payload = {
              name: nextName, namespace: nextNamespace, type: nextType,
              ports: servicePorts, selector: selector, external_ips: nextType === "ExternalName" ? [] : storageLines(f.elements.external_ips.value),
              external_name: String(f.elements.external_name.value || "").trim(),
              load_balancer_ip: String(f.elements.load_balancer_ip.value || "").trim(),
              external_traffic_policy: nextType === "NodePort" || nextType === "LoadBalancer" ? f.elements.external_traffic_policy.value : "",
              session_affinity: f.elements.session_affinity.value, labels: labels, annotations: annotations
            };
            if (nextType === "ExternalName") {
              payload.load_balancer_ip = "";
              payload.external_traffic_policy = "";
            }
            var lbClass = String(f.elements.load_balancer_class.value || "").trim();
            if (lbClass) payload.load_balancer_class = lbClass;
            if (editing) {
              payload.uid = item.uid; payload.resource_version = item.resource_version;
              path = base + "services/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name);
              method = apiPut;
            } else {
              path = base + "services"; method = apiPost;
            }
          } else {
            var ingressRules = parseArray(f, "rules", "Ingress 规则", bad);
            var tls = parseArray(f, "tls", "TLS", bad);
            if (ingressRules === null || tls === null) return;
            payload = { name: nextName, namespace: nextNamespace, rules: ingressRules, tls: tls, labels: labels, annotations: annotations };
            var ingressClass = String(f.elements.class_name.value || "").trim();
            if (ingressClass) payload.class_name = ingressClass;
            if (editing) {
              payload.uid = item.uid; payload.resource_version = item.resource_version;
              path = base + "ingresses/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name);
              method = apiPut;
            } else {
              path = base + "ingresses"; method = apiPost;
            }
          }
          storageSubmit(method(path + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), payload),
            (editing ? "已提交更新 " : "已提交创建 ") + nextNamespace + "/" + nextName);
        }, !editing ? (isService
          ? aiDraftConfig("k8s.service.create", "在 prod 创建 api Service，选择器 app=api，端口 80 转发到 8080，类型 ClusterIP。", {
            namespace: aiField("string", "目标 Kubernetes 命名空间", { maxLength: 63 }),
            name: aiField("string", "Service 名称，使用小写字母和短横线", { maxLength: 63 }),
            type: aiField("enum", "Service 类型", { values: ["ClusterIP", "NodePort", "LoadBalancer", "ExternalName"] }),
            external_name: aiField("string", "仅 ExternalName 使用的外部 DNS 名称"),
            ports: aiField("array", "Kubernetes Service 端口对象数组，字段包含 port、targetPort、protocol、name；ExternalName 留空", { ui: "json" }),
            selector: aiField("string_map", "匹配后端 Pod 的标签选择器；ExternalName 留空", { ui: "json" }),
            external_ips: aiField("array", "可选的外部 IP 地址列表", { ui: "lines" }),
            load_balancer_ip: aiField("string", "LoadBalancer 服务申请的外部 IP"),
            load_balancer_class: aiField("string", "集群中已安装的负载均衡器 class"),
            external_traffic_policy: aiField("enum", "外部流量转发策略", { values: ["", "Cluster", "Local"] }),
            session_affinity: aiField("enum", "客户端会话亲和性", { values: ["None", "ClientIP"] }),
            labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" }),
            annotations: aiField("string_map", "Kubernetes 注解键值对象", { ui: "json" })
          }, true)
          : aiDraftConfig("k8s.ingress.create", "发布 api.example.com 的 /v1 路径到 prod/api 服务 8080 端口，并启用 api-tls。", {
            namespace: aiField("string", "目标 Kubernetes 命名空间", { maxLength: 63 }),
            name: aiField("string", "Ingress 名称，使用小写字母和短横线", { maxLength: 253 }),
            class_name: aiField("string", "集群中已安装并配置的 IngressClass 名称"),
            rules: aiField("array", "networking.k8s.io/v1 规则数组，后端仅引用已存在的 Service", { ui: "json" }),
            tls: aiField("array", "TLS 配置数组，只引用已存在的 TLS Secret 名称，不包含证书内容", { ui: "json" }),
            labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" }),
            annotations: aiField("string_map", "Ingress 控制器支持的注解键值对象", { ui: "json" })
          }, true)) : null);
      };
      var networkDelete = function (kind, item) {
        if (!item || !item.uid || !item.resource_version) return toastMsg("缺少 UID 或 resource_version，请刷新后重试");
        var key = item.namespace + "/" + item.name, isService = kind === "service";
        var body = '<p>确认删除 ' + (isService ? "Service" : "Ingress") + ' <b>' + gEsc(key) + "</b>？</p>" +
          '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该资源</label>';
        formDialog("删除 " + (isService ? "Service" : "Ingress") + "   " + key, body, "删除", function (f) {
          if (!f.reportValidity()) return;
          storageSubmit(apiDel(base + (isService ? "services/" : "ingresses/") +
            encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name), {
              uid: item.uid, resource_version: item.resource_version
            }), "已提交删除 " + key);
        });
      };
      bindActs(box, function (act, id) {
        var rows = act.indexOf("service") >= 0 ? services : ingresses;
        var item = rows.find(function (x) { return x.namespace + "/" + x.name === id; });
        if (!item) return;
        if (act === "network-service-edit") networkDialog("service", item);
        else if (act === "network-service-del") networkDelete("service", item);
        else if (act === "network-ingress-edit") networkDialog("ingress", item);
        else if (act === "network-ingress-del") networkDelete("ingress", item);
      });
      Array.prototype.forEach.call(box.querySelectorAll("[data-network-new]"), function (button) {
        button.onclick = function () {
          if (!canWrite) return toastMsg("没有 network:write 权限");
          var kind = button.getAttribute("data-network-new");
          networkDialog(kind, null);
        };
      });
      filterTable(box);
    });
  }

  function clConfigPage(el, seq, cur, box) {
    var base = "/clusters/" + cur.id + "/config/";
    Promise.all([
      wrap(apiGet(base + "configmaps")),
      wrap(apiGet(base + "secrets"))
    ]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      for (var i = 0; i < rs.length; i++) if (rs[i].err && isAuthErr(rs[i].err)) return kickLogin();
      var configMaps = rs[0].ok ? itemsOf(rs[0].data) : [];
      var secrets = rs[1].ok ? itemsOf(rs[1].data) : [];
      var api = xs();
      var canWrite = !!(api && typeof api.can === "function" && api.can("config:write"));
      var configRows = configMaps.map(function (x, i) { return [
        '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>",
        gEsc((x.keys || []).join(", ") || "无键"), gEsc((x.keys || []).length + " 个"),
        x.immutable ? "不可变" : "可编辑", gEsc(fmtTime(x.created_at)),
        '<span class="x-kacts">' + actBtn("查看数据", "cm-detail", String(i)) +
          (canWrite ? actBtn("编辑", "cm-edit", String(i), {
            disabled: !x.uid || !x.resource_version,
            title: x.uid && x.resource_version ? (x.immutable ? "不可变数据只读，可维护标签和注解" : "") : "缺少 UID/resource_version，请刷新"
          }) + actBtn("删除", "cm-del", String(i), {
            disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
          }) : "") + "</span>"
      ]; });
      var secretRows = secrets.map(function (x) { return [
        '<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>",
        gEsc(x.type || "Opaque"), gEsc((x.keys || []).join(", ") || "无键"),
        gEsc((x.keys || []).length + " 个"), x.immutable ? "不可变" : "可编辑", gEsc(fmtTime(x.created_at)),
        canWrite ? '<span class="x-kacts">' + actBtn("编辑", "secret-edit", String(x.namespace + "/" + x.name), {
          disabled: !x.uid || !x.resource_version,
          title: x.uid && x.resource_version ? (x.immutable ? "不可变数据只读，可维护标签和注解" : "") : "缺少 UID/resource_version，请刷新"
        }) + actBtn("删除", "secret-del", String(x.namespace + "/" + x.name), {
          disabled: !x.uid || !x.resource_version, title: x.uid && x.resource_version ? "" : "缺少 UID/resource_version，请刷新"
        }) + "</span>" : ""
      ]; });
      var section = function (title, result, items, cols, rows, empty, tools) {
        if (!result.ok) return panelHtml(title, "", '<p class="x-aerr" role="alert" style="margin:0">' + gEsc(errText(result.err)) + "</p>");
        return panelHtml(title, items.length + " 个", items.length ? tableHtml(cols, rows) : '<p class="x-frac" style="margin:0">' + gEsc(empty) + "</p>", tools || "");
      };
      var writeNote = canWrite
        ? '<p class="x-frac" style="margin:0 0 12px">ConfigMap 与 Secret 变更经集群审批网关提交。Secret API 只返回键名等元数据，现有值不会回显。</p>'
        : '<p class="x-frac" style="margin:0 0 12px">当前账号没有 config:write 权限，页面为只读。</p>';
      box.innerHTML = writeNote +
        section("ConfigMap", rs[0], configMaps,
          ["命名空间 / 名称", "键", "数量", "状态", "创建时间", "操作"], configRows,
          "集群没有 ConfigMap。", canWrite ? '<button class="x-btn x-btn--pri" type="button" data-config-new="configmap">+ 新建 ConfigMap</button>' : "") +
        section("Secret", rs[1], secrets,
          ["命名空间 / 名称", "类型", "键名（仅元数据）", "数量", "状态", "创建时间"].concat(canWrite ? ["操作"] : []), secretRows,
          "集群没有 Secret。", canWrite ? '<button class="x-btn x-btn--pri" type="button" data-config-new="secret">+ 新建 Secret</button>' : "");
      var parseData = function (form, field, label, bad) {
        return storageJSONObject(form, field, label, bad);
      };
      var configMapDialog = function (item) {
        var editing = !!item;
        if (editing && (!item.uid || !item.resource_version)) return toastMsg("缺少 UID 或资源版本，请刷新后重试");
        var namespace = editing ? item.namespace : "default";
        var name = editing ? item.name : "";
        var dataReadonly = editing && item.immutable ? " readonly" : "";
        var body =
          '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" value="' + gEsc(namespace) + '"' + (editing ? " readonly" : "") + " /></label>" +
          '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" value="' + gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
          '<label><span>Data JSON</span><textarea class="x-input x-mono" name="data" rows="6" spellcheck="false"' + dataReadonly + ">" + gEsc(JSON.stringify(item && item.data || {}, null, 2)) + "</textarea></label>" +
          '<label><span>BinaryData JSON（Base64）</span><textarea class="x-input x-mono" name="binary_data" rows="3" spellcheck="false"' + dataReadonly + ">" + gEsc(JSON.stringify(item && item.binary_data || {}, null, 2)) + "</textarea></label>" +
          '<div class="x-row2">' + storageLabelsField(item && item.labels || {}) +
          '<label><span>Annotations JSON</span><textarea class="x-input x-mono" name="annotations" rows="3" spellcheck="false">' + gEsc(JSON.stringify(item && item.annotations || {}, null, 2)) + "</textarea></label></div>" +
          '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="immutable"' + (item && item.immutable ? " checked disabled" : "") + " />创建为不可变（不可变后不能修改数据）</label>" +
          (item && item.immutable ? '<p class="x-frac">不可变 ConfigMap 仅允许维护标签与注解，数据字段只读。</p>' : "") +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>';
        formDialog(editing ? "编辑 ConfigMap   " + namespace + "/" + name : "创建 ConfigMap", body, editing ? "保存变更" : "创建", function (f, bad) {
          if (!f.reportValidity()) return;
          var data = parseData(f, "data", "Data", bad);
          var binaryData = parseData(f, "binary_data", "BinaryData", bad);
          var labels = parseData(f, "labels", "标签", bad);
          var annotations = parseData(f, "annotations", "Annotations", bad);
          if (data === null || binaryData === null || labels === null || annotations === null) return;
          var namespace = String(f.elements.namespace.value || "").trim();
          var name = String(f.elements.name.value || "").trim();
          if (!namespace || !name) return bad("名称与命名空间不能为空");
          Object.keys(binaryData).forEach(function (key) {
            if (Object.prototype.hasOwnProperty.call(data, key)) data = null;
          });
          if (data === null) return bad("同一键不能同时出现在 Data 与 BinaryData");
          var payload = {
            namespace: namespace, name: name, data: data, binary_data: binaryData,
            labels: labels, annotations: annotations, immutable: !!f.elements.immutable.checked
          };
          var path = base + "configmaps" + (editing ? "/" + encodeURIComponent(namespace) + "/" + encodeURIComponent(name) : "");
          if (editing) {
            payload.uid = item.uid;
            payload.resource_version = item.resource_version;
          }
          var method = editing ? apiPut : apiPost;
          storageSubmit(method(path + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), payload),
            (editing ? "已提交更新 " : "已提交创建 ") + namespace + "/" + name);
        }, !editing ? aiDraftConfig("k8s.configmap.create", "在 prod 创建 web-settings ConfigMap，设置 LOG_LEVEL=info 和 APP_PORT=8080，并添加 team=web 标签。", {
          namespace: aiField("string", "目标 Kubernetes 命名空间", { maxLength: 63 }),
          name: aiField("string", "ConfigMap 名称，使用小写字母和短横线", { maxLength: 253 }),
          data: aiField("string_map", "非敏感的 ConfigMap 文本配置键值对象；不要放密码、令牌或凭据", { ui: "json" }),
          labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" }),
          annotations: aiField("string_map", "Kubernetes 注解键值对象", { ui: "json" }),
          immutable: aiField("boolean", "是否创建为不可变 ConfigMap")
        }, true) : null);
      };
      var deleteConfigMap = function (item) {
        if (!item.uid || !item.resource_version) return toastMsg("缺少 UID 或 resource_version，请刷新后重试");
        var key = item.namespace + "/" + item.name;
        formDialog("删除 ConfigMap   " + key,
          '<p>确认删除 <b>' + gEsc(key) + "</b>？引用此配置的工作负载可能受到影响。</p>" +
          '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 ConfigMap</label>' +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>', "删除", function (f) {
            if (!f.reportValidity()) return;
            storageSubmit(apiDel(base + "configmaps/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name) +
              (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), {
                uid: item.uid, resource_version: item.resource_version
              }), "已提交删除 " + key);
          });
      };
      var secretDialog = function (item) {
        var editing = !!item;
        if (editing && (!item.uid || !item.resource_version)) return toastMsg("缺少 UID 或资源版本，请刷新后重试");
        var namespace = editing ? item.namespace : "default";
        var name = editing ? item.name : "";
        var secretDataReadonly = editing && item.immutable ? " readonly" : "";
        var body =
          '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" value="' + gEsc(namespace) + '"' + (editing ? " readonly" : "") + " /></label>" +
          '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="253" value="' + gEsc(name) + '"' + (editing ? " readonly" : "") + " /></label></div>" +
          '<label><span>类型</span>' + (editing
            ? '<input class="x-input x-mono" name="type" value="' + gEsc(item.type || "Opaque") + '" readonly />'
            : '<input class="x-input x-mono" name="type" value="Opaque" maxlength="253" />') + "</label>" +
          '<label><span>' + (editing ? "新增或替换键值 JSON" : "键值 JSON") + '</span><textarea class="x-input x-mono" name="data" rows="5" spellcheck="false"' + secretDataReadonly + '>{}</textarea></label>' +
          (editing ? '<label><span>删除键（逗号或换行分隔）</span><textarea class="x-input x-mono" name="delete_keys" rows="2" spellcheck="false"' + secretDataReadonly + "></textarea></label>" : "") +
          '<div class="x-row2">' + storageLabelsField(item && item.labels || {}) +
          '<label><span>Annotations JSON</span><textarea class="x-input x-mono" name="annotations" rows="3" spellcheck="false">' + gEsc(JSON.stringify(item && item.annotations || {}, null, 2)) + "</textarea></label></div>" +
          (editing ? '<p class="x-frac">' + (item.immutable ? "此 Secret 不可变，仅可维护标签与注解。" : "Secret 旧值不会回显；未填写的键保持原值。仅填写本次新增/替换的键，并单独指定需要删除的键。") + "</p>" :
            '<p class="x-frac">Secret 值仅在本次提交中输入，接口只返回键名等元数据。</p>') +
          (editing ? '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm_patch" required />我确认按上述增量修改 Secret</label>' : "") +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>';
        formDialog(editing ? "编辑 Secret   " + namespace + "/" + name : "创建 Secret", body, editing ? "提交修改" : "创建", function (f, bad) {
          if (!f.reportValidity()) return;
          var data = parseData(f, "data", "Secret 数据", bad);
          var labels = parseData(f, "labels", "标签", bad);
          var annotations = parseData(f, "annotations", "Annotations", bad);
          if (data === null || labels === null || annotations === null) return;
          var namespace = String(f.elements.namespace.value || "").trim();
          var name = String(f.elements.name.value || "").trim();
          var type = String(f.elements.type.value || "Opaque").trim();
          if (!namespace || !name || !type) return bad("名称、命名空间和类型不能为空");
          var deleteKeys = editing ? storageLines(f.elements.delete_keys.value) : [];
          if (editing) {
            for (var i = 0; i < deleteKeys.length; i++) {
              if (Object.prototype.hasOwnProperty.call(data, deleteKeys[i])) return bad("同一 Secret 键不能同时替换和删除");
            }
          }
          var payload = { namespace: namespace, name: name, type: type, data: data, labels: labels, annotations: annotations };
          if (editing) {
            payload.uid = item.uid;
            payload.resource_version = item.resource_version;
            payload.merge = true;
            payload.delete_keys = deleteKeys;
          }
          var path = base + "secrets" + (editing ? "/" + encodeURIComponent(namespace) + "/" + encodeURIComponent(name) : "");
          storageSubmit((editing ? apiPut : apiPost)(path +
            (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), payload),
          (editing ? "已提交更新 " : "已提交创建 ") + namespace + "/" + name);
        }, !editing ? aiDraftConfig("k8s.secret.create", "创建名为 payment-auth 的 Opaque Secret，供 prod 中的 payment 应用引用。不要填写密钥值。", {
          namespace: aiField("string", "目标 Kubernetes 命名空间", { maxLength: 63 }),
          name: aiField("string", "Secret 名称，使用小写字母和短横线", { maxLength: 253 }),
          type: aiField("string", "Secret 类型，如 Opaque、kubernetes.io/tls 或 kubernetes.io/dockerconfigjson", { maxLength: 253 }),
          labels: aiField("string_map", "Kubernetes 标签键值对象", { ui: "json" })
        }, true, true) : null);
      };
      var deleteSecret = function (item) {
        if (!item.uid || !item.resource_version) return toastMsg("缺少 UID 或 resource_version，请刷新后重试");
        var key = item.namespace + "/" + item.name;
        formDialog("删除 Secret   " + key,
          '<p>确认删除 <b>' + gEsc(key) + "</b>？引用此 Secret 的应用可能无法启动或访问凭据。</p>" +
          '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required />我确认删除该 Secret</label>' +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>', "删除", function (f) {
            if (!f.reportValidity()) return;
            storageSubmit(apiDel(base + "secrets/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name) +
              (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), {
                uid: item.uid, resource_version: item.resource_version
              }), "已提交删除 " + key);
          });
      };
      bindActs(box, function (act, id) {
        if (act.indexOf("cm-") === 0) {
          var item = configMaps[Number(id)];
          if (!item) return;
          if (act === "cm-detail") {
            var body = '<pre class="x-mono" style="max-height:64vh;overflow:auto;white-space:pre-wrap;margin:0">' +
              gEsc(prettyJSON({ data: item.data || {}, binary_data_base64: item.binary_data || {} })) + "</pre>";
            var foot = '<button class="x-btn" type="button" data-close>关闭</button>';
            if (typeof dlg === "function") dlg("ConfigMap   " + gEsc(item.namespace + "/" + item.name), body, foot);
            else openModal("ConfigMap", body, foot);
          } else if (act === "cm-edit") configMapDialog(item);
          else if (act === "cm-del") deleteConfigMap(item);
          return;
        }
        var secret = secrets.find(function (x) { return x.namespace + "/" + x.name === id; });
        if (!secret) return;
        if (act === "secret-edit") secretDialog(secret);
        else if (act === "secret-del") deleteSecret(secret);
      });
      Array.prototype.forEach.call(box.querySelectorAll("[data-config-new]"), function (button) {
        button.onclick = function () {
          if (!canWrite) return toastMsg("没有 config:write 权限");
          if (button.getAttribute("data-config-new") === "secret") secretDialog(null);
          else configMapDialog(null);
        };
      });
      filterTable(box);
    });
  }

  function clNodeGroupsPage(el, seq, cur, box) {
    apiGet("/clusters/" + cur.id + "/node-groups").then(function (data) {
      if (!alive(seq, el) || !box) return;
      var items = itemsOf(data);
      var api = xs();
      var canWrite = !!(api && typeof api.can === "function" && api.can("node:write"));
      var rows = items.map(function (x) {
        var cells = [
        '<span class="x-name">' + gEsc(x.name || "未设置") + "</span>",
        gEsc(x.label_value || "—"), gEsc((x.nodes || []).length + " 个"),
        gEsc(x.ready + " Ready"), gEsc(x.unschedulable + " 已封锁"),
        gEsc((x.roles || []).join(", ") || "—"), gEsc((x.taints || []).join(", ") || "—")
        ];
        if (canWrite) cells.push('<span class="x-kacts">' +
          actBtn("批量标签", "nodegroup-label", x.name, { disabled: !(x.nodes || []).length }) +
          actBtn("新增污点", "nodegroup-taint-add", x.name, { disabled: !(x.nodes || []).length }) +
          actBtn("移除污点", "nodegroup-taint-remove", x.name, { disabled: !(x.nodes || []).length }) + "</span>");
        return cells;
      });
      box.innerHTML = panelHtml("节点分组", gEsc(data && data.label || "node.xingshu.com/pool"),
        (canWrite
          ? '<p class="x-frac" style="margin:0 0 12px">支持按当前分组批量维护节点标签与污点；写操作经集群审批网关提交。' + gEsc(data && data.note || "") + "</p>"
          : '<p class="x-frac" style="margin:0 0 12px">当前账号没有 node:write 权限。' + gEsc(data && data.note || "") + "</p>") +
        (items.length ? tableHtml(["分组", "标签值", "节点数", "就绪", "调度状态", "角色", "污点"].concat(canWrite ? ["操作"] : []), rows) :
          emptyPanel("没有节点", "节点接入后按标签汇总。")));
      var selectedGroup = function (name) {
        return items.find(function (x) { return x.name === name; }) || null;
      };
      var batchDialog = function (action, group) {
        var nodes = (group && group.nodes || []).slice();
        if (!nodes.length) return toastMsg("该分组没有可操作节点");
        var isLabels = action === "labels";
        var isAdd = action === "taint-add";
        var op = isLabels ? "labels" : "taints";
        var form = isLabels
          ? '<label><span class="x-req">标签 JSON</span><textarea class="x-input x-mono" name="labels" rows="4" spellcheck="false">{"' + gEsc(data.label || "node.xingshu.com/pool") + '":"' + gEsc(group.label_value || "") + '"}</textarea></label>' +
            '<p class="x-frac">将标签合并到所选节点；将某个值设置为 null 可移除该标签。</p>'
          : '<div class="x-row2"><label><span class="x-req">污点键</span><input class="x-input x-mono" name="key" required maxlength="253" /></label>' +
            '<label><span>污点值</span><input class="x-input x-mono" name="value" maxlength="63" /></label></div>' +
            '<label><span class="x-req">Effect</span>' + selectHtml([
              { v: "NoSchedule", t: "NoSchedule" }, { v: "PreferNoSchedule", t: "PreferNoSchedule" }, { v: "NoExecute", t: "NoExecute" }
            ], "NoSchedule", 'name="effect"') + "</label>" +
            '<p class="x-frac">' + (isAdd
              ? "新增污点会保留节点上的其他污点；如同键与 Effect 已存在，则更新其值。"
              : "仅移除键、值与 Effect 完全匹配的污点，其他污点保持不变。") + "</p>";
        formDialog((isLabels ? "批量设置节点标签" : (isAdd ? "批量新增节点污点" : "批量移除节点污点")) + "   " + gEsc(group.name),
          '<p>所选节点：' + gEsc(nodes.join(", ")) + "</p>" + form +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>',
          "提交", function (f, bad) {
            if (!f.reportValidity()) return;
            var path = "/clusters/" + cur.id + "/node-batch-" + op;
            if (isLabels) {
              var input = f.elements.labels, labels;
              try { labels = JSON.parse(String(input.value || "")); }
              catch (e) { return bad("标签必须是有效 JSON", input); }
              if (!labels || Array.isArray(labels) || typeof labels !== "object" || !Object.keys(labels).length) {
                return bad("标签必须是非空 JSON 对象", input);
              }
              var invalid = Object.keys(labels).find(function (key) {
                return labels[key] !== null && typeof labels[key] !== "string";
              });
              if (invalid !== undefined) return bad("标签值必须是字符串或 null", input);
              storageSubmit(apiPost(path +
                (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), {
                  nodes: nodes, labels: labels
                }), "已提交 " + nodes.length + " 个节点的标签变更");
              return;
            }
            var taint = {
              key: String(f.elements.key.value || "").trim(),
              value: String(f.elements.value.value || "").trim(),
              effect: f.elements.effect.value
            };
            if (!taint.key) return bad("污点键不能为空", f.elements.key);
            var items = nodes.map(function (name) {
              return { name: name, add: isAdd ? [taint] : [], remove: isAdd ? [] : [taint] };
            });
            storageSubmit(apiPut(path +
              (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), {
                items: items
              }), "已提交 " + nodes.length + " 个节点的污点变更");
          });
      };
      bindActs(box, function (act, name) {
        if (!canWrite) return toastMsg("没有 node:write 权限");
        var group = selectedGroup(name);
        if (!group) return;
        if (act === "nodegroup-label") batchDialog("labels", group);
        else if (act === "nodegroup-taint-add") batchDialog("taint-add", group);
        else if (act === "nodegroup-taint-remove") batchDialog("taint-remove", group);
      });
      filterTable(box);
    }, function (err) {
      if (!alive(seq, el) || !box) return;
      if (isAuthErr(err)) return kickLogin();
      box.innerHTML = errorPanel(err);
    });
  }

  function clHpaPage(el, seq, cur, box) {
    var base = "/clusters/" + cur.id + "/hpa";
    Promise.all([wrap(apiGet(base)), wrap(apiGet("/clusters/" + cur.id + "/namespaces"))]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      var result = rs[0], items = result.ok ? itemsOf(result.data) : [];
      if (result.err && isAuthErr(result.err)) return kickLogin();
      var api = xs(), canWrite = !!(api && typeof api.can === "function" && api.can("workload:write"));
      var rows = items.map(function (x) {
        var min = x.spec && x.spec.minReplicas != null ? x.spec.minReplicas : "—";
        var max = x.spec && x.spec.maxReplicas != null ? x.spec.maxReplicas : "—";
        var current = x.status && x.status.currentReplicas != null ? x.status.currentReplicas : "—";
        var desired = x.status && x.status.desiredReplicas != null ? x.status.desiredReplicas : "—";
        var target = x.spec && x.spec.scaleTargetRef ? (x.spec.scaleTargetRef.kind + "/" + x.spec.scaleTargetRef.name) : "—";
        var cells = ['<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>", gEsc(target),
          gEsc(min + " - " + max), gEsc(current + " / " + desired), gEsc((x.spec && x.spec.metrics || []).length + " 项"),
          gEsc(fmtTime(x.created_at))];
        if (canWrite) cells.push('<span class="x-kacts">' + actBtn("编辑", "hpa-edit", x.namespace + "/" + x.name,
          { disabled: !x.uid || !x.resource_version }) + actBtn("删除", "hpa-del", x.namespace + "/" + x.name,
          { disabled: !x.uid || !x.resource_version }) + "</span>");
        return cells;
      });
      var note = canWrite ? "HPA 使用 autoscaling/v2，写操作经过集群审批网关。" : "当前账号没有 workload:write 权限，页面为只读。";
      box.innerHTML = '<p class="x-frac" style="margin:0 0 12px">' + gEsc(note) + "</p>" +
        panelHtml("HorizontalPodAutoscaler", result.ok ? items.length + " 个" : "",
          result.ok ? (items.length ? tableHtml(["命名空间 / 名称", "目标", "副本范围", "当前 / 期望", "指标", "创建时间"].concat(canWrite ? ["操作"] : []), rows) :
            '<p class="x-frac" style="margin:0">集群没有 HPA。</p>') : '<p class="x-aerr" role="alert">' + gEsc(errText(result.err)) + "</p>",
          canWrite ? '<button class="x-btn x-btn--pri" type="button" data-hpa-new>+ 新建 HPA</button>' : "");
      var find = function (key) { return items.find(function (x) { return x.namespace + "/" + x.name === key; }); };
      var hpaDialog = function (item) {
        var edit = !!item, spec = item && item.spec || { scaleTargetRef: { apiVersion: "apps/v1", kind: "Deployment", name: "" }, minReplicas: 1, maxReplicas: 3, metrics: [] };
        var body = '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required value="' + gEsc(edit ? item.namespace : "default") + '"' + (edit ? " readonly" : "") + ' /></label>' +
          '<label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required value="' + gEsc(edit ? item.name : "") + '"' + (edit ? " readonly" : "") + " /></label></div>" +
          '<label><span class="x-req">spec JSON（autoscaling/v2）</span><textarea class="x-input x-mono" name="spec" rows="12" required spellcheck="false">' + gEsc(JSON.stringify(spec, null, 2)) + "</textarea></label>" +
          '<label><span>标签 JSON</span><textarea class="x-input x-mono" name="labels" rows="3" spellcheck="false">' + gEsc(JSON.stringify(item && item.labels || {}, null, 2)) + "</textarea></label>" +
          '<label><span>变更原因</span><input class="x-input" name="reason" maxlength="500" /></label>' +
          '<p class="x-frac">示例指标：Resource CPU/Memory 或 Pods；只提交 Kubernetes 原生 HPA，不生成 VPA 推荐。</p>';
        formDialog(edit ? "编辑 HPA   " + item.namespace + "/" + item.name : "创建 HPA", body, edit ? "保存" : "创建", function (f, bad) {
          if (!f.reportValidity()) return;
          var parsed, labels;
          try { parsed = JSON.parse(f.elements.spec.value); labels = JSON.parse(f.elements.labels.value || "{}"); } catch (e) { return bad("spec 与标签必须是有效 JSON"); }
          if (!parsed || !parsed.scaleTargetRef || !parsed.metrics || !parsed.metrics.length) return bad("spec.scaleTargetRef 与至少一个 metrics 必填");
          var payload = { namespace: f.elements.namespace.value.trim(), name: f.elements.name.value.trim(), spec: parsed, labels: labels };
          if (edit) { payload.uid = item.uid; payload.resource_version = item.resource_version; }
          var path = base + (edit ? "/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name) : "");
          storageSubmit(edit ? apiPut(path + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), payload) :
            apiPost(path + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), payload),
            edit ? "已提交更新 HPA" : "已提交创建 HPA");
        }, !edit ? aiDraftConfig("k8s.hpa.create", "在 prod 创建 web-hpa，目标 Deployment/web，最小 2 最大 6，按 CPU 70% 扩缩容。", {
          namespace: aiField("string", "目标 Kubernetes 命名空间"), name: aiField("string", "HPA 名称"),
          spec: aiField("object", "autoscaling/v2 HPA spec，必须包含 scaleTargetRef、minReplicas、maxReplicas、metrics", { ui: "json" }),
          labels: aiField("string_map", "Kubernetes 标签", { ui: "json" })
        }, true) : null);
      };
      bindActs(box, function (act, id) {
        var item = find(id);
        if (!item || !canWrite) return;
        if (act === "hpa-edit") return hpaDialog(item);
        if (act === "hpa-del") formDialog("删除 HPA   " + id, '<p>确认删除该 HPA？工作负载不会被删除。</p><label><input type="checkbox" name="confirm" required />我确认删除</label><label><span>变更原因</span><input class="x-input" name="reason" /></label>', "删除", function (f) {
          if (!f.reportValidity()) return;
          storageSubmit(apiDel(base + "/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name) + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), { uid: item.uid, resource_version: item.resource_version }), "已提交删除 HPA");
        });
      });
      var newBtn = box.querySelector("[data-hpa-new]"); if (newBtn) newBtn.onclick = function () { hpaDialog(null); };
      filterTable(box);
    }, function (err) { if (alive(seq, el) && box) box.innerHTML = errorPanel(err); });
  }

  function clCronPage(el, seq, cur, box) {
    var base = "/clusters/" + cur.id + "/cronjobs";
    Promise.all([wrap(apiGet(base)), wrap(apiGet("/clusters/" + cur.id + "/namespaces"))]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      var result = rs[0], items = result.ok ? itemsOf(result.data) : [];
      if (result.err && isAuthErr(result.err)) return kickLogin();
      var api = xs(), canWrite = !!(api && typeof api.can === "function" && api.can("workload:write"));
      var rows = items.map(function (x) {
        var spec = x.spec || {}, status = x.status || {}, suspended = spec.suspend ? "已暂停" : "运行中";
        var cells = ['<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>", mono(spec.schedule || "—"), gEsc(suspended),
          gEsc((status.active || []).length + " 个运行中"), gEsc(status.lastScheduleTime ? fmtTime(status.lastScheduleTime) : "—"), gEsc(fmtTime(x.created_at))];
        cells.push('<span class="x-kacts">' + actBtn("运行历史", "cron-history", x.namespace + "/" + x.name) +
          (canWrite ? actBtn("编辑", "cron-edit", x.namespace + "/" + x.name, { disabled: !x.uid || !x.resource_version }) +
            actBtn("立即运行", "cron-run", x.namespace + "/" + x.name, { disabled: !x.uid || !x.resource_version }) +
            actBtn("删除", "cron-del", x.namespace + "/" + x.name, { disabled: !x.uid || !x.resource_version }) : "") + "</span>");
        return cells;
      });
      box.innerHTML = '<p class="x-frac" style="margin:0 0 12px">' + gEsc(canWrite ? "CronJob 使用 batch/v1；暂停、立即运行与删除均经过集群审批网关。" : "当前账号没有 workload:write 权限，页面为只读。") + "</p>" +
        panelHtml("CronJob", result.ok ? items.length + " 个" : "", result.ok ? (items.length ? tableHtml(["命名空间 / 名称", "调度", "状态", "运行中", "上次调度", "创建时间", "操作"], rows) : '<p class="x-frac" style="margin:0">集群没有 CronJob。</p>') : '<p class="x-aerr">' + gEsc(errText(result.err)) + "</p>", canWrite ? '<button class="x-btn x-btn--pri" type="button" data-cron-new>+ 新建 CronJob</button>' : "");
      var find = function (key) { return items.find(function (x) { return x.namespace + "/" + x.name === key; }); };
      var cronDialog = function (item) {
        var edit = !!item, spec = item && item.spec || { schedule: "*/5 * * * *", suspend: false, jobTemplate: { spec: { template: { spec: { restartPolicy: "Never", containers: [] } } } } };
        var body = '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required value="' + gEsc(edit ? item.namespace : "default") + '"' + (edit ? " readonly" : "") + ' /></label><label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required value="' + gEsc(edit ? item.name : "") + '"' + (edit ? " readonly" : "") + ' /></label></div>' +
          '<label><span class="x-req">spec JSON（batch/v1）</span><textarea class="x-input x-mono" name="spec" rows="14" required spellcheck="false">' + gEsc(JSON.stringify(spec, null, 2)) + "</textarea></label>" +
          '<label><span>标签 JSON</span><textarea class="x-input x-mono" name="labels" rows="3">' + gEsc(JSON.stringify(item && item.labels || {}, null, 2)) + "</textarea></label>" +
          '<label><span>变更原因</span><input class="x-input" name="reason" /></label><p class="x-frac">CronJob spec 使用标准 Kubernetes batch/v1 结构。containers 必须至少包含一个容器。</p>';
        formDialog(edit ? "编辑 CronJob   " + item.namespace + "/" + item.name : "创建 CronJob", body, edit ? "保存" : "创建", function (f, bad) {
          if (!f.reportValidity()) return;
          var parsed, labels; try { parsed = JSON.parse(f.elements.spec.value); labels = JSON.parse(f.elements.labels.value || "{}"); } catch (e) { return bad("spec 与标签必须是有效 JSON"); }
          var payload = { namespace: f.elements.namespace.value.trim(), name: f.elements.name.value.trim(), spec: parsed, labels: labels }; if (edit) { payload.uid = item.uid; payload.resource_version = item.resource_version; }
          var path = base + (edit ? "/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name) : ""), q = f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : "";
          storageSubmit(edit ? apiPut(path + q, payload) : apiPost(path + q, payload), edit ? "已提交更新 CronJob" : "已提交创建 CronJob");
        }, !edit ? aiDraftConfig("k8s.cronjob.create", "在 prod 创建 nightly-job，每天凌晨 2 点运行 busybox echo backup，保留最近 3 次成功记录。", {
          namespace: aiField("string", "目标 Kubernetes 命名空间"), name: aiField("string", "CronJob 名称"), spec: aiField("object", "batch/v1 CronJob spec，包含 schedule、suspend、jobTemplate", { ui: "json" }), labels: aiField("string_map", "Kubernetes 标签", { ui: "json" })
        }, true) : null);
      };
      bindActs(box, function (act, id) {
        var item = find(id); if (!item) return;
        var path = base + "/" + encodeURIComponent(item.namespace) + "/" + encodeURIComponent(item.name);
        if (act === "cron-history") {
          apiGet(path + "/jobs").then(function (data) {
            var jobs = itemsOf(data);
            var historyRows = jobs.map(function (job) {
              return [mono(job.name || "—"), tagHtml(job.status || "—", job.status === "Succeeded" ? "ok" : job.status === "Failed" ? "warn" : "muted"),
                gEsc(job.active == null ? "—" : job.active), gEsc(job.succeeded == null ? "—" : job.succeeded),
                gEsc(job.failed == null ? "—" : job.failed), gEsc(fmtTime(job.start_time || job.created_at)),
                gEsc(fmtTime(job.completion_time))];
            });
            var historyBody = jobs.length
              ? tableHtml(["Job", "状态", "运行中", "成功", "失败", "开始时间", "完成时间"], historyRows)
              : '<p class="x-frac" style="margin:0">该 CronJob 暂无运行记录。</p>';
            var historyFoot = '<button class="x-btn" type="button" data-close>关闭</button>';
            if (typeof dlg === "function") dlg("CronJob 运行历史   " + gEsc(id), historyBody, historyFoot);
            else openModal("CronJob 运行历史", historyBody, historyFoot);
          }, function (err) { toastMsg(errText(err)); });
          return;
        }
        if (!canWrite) return;
        if (act === "cron-edit") return cronDialog(item);
        if (act === "cron-run") return formDialog("立即运行 CronJob   " + id, '<p>平台会基于当前 batch/v1 CronJob 模板创建一个一次性 Job，不修改调度。</p><label><input type="checkbox" name="confirm" required />我确认立即运行</label><label><span>变更原因</span><input class="x-input" name="reason" /></label>', "运行", function (f) { if (!f.reportValidity()) return; storageSubmit(apiPost(path + "/run" + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), { uid: item.uid, resource_version: item.resource_version }), "已提交 CronJob 立即运行"); });
        if (act === "cron-del") return formDialog("删除 CronJob   " + id, '<p>确认删除 CronJob 及其控制的 Job？</p><label><input type="checkbox" name="confirm" required />我确认删除</label><label><span>变更原因</span><input class="x-input" name="reason" /></label>', "删除", function (f) { if (!f.reportValidity()) return; storageSubmit(apiDel(path + (f.elements.reason.value.trim() ? "?reason=" + encodeURIComponent(f.elements.reason.value.trim()) : ""), { uid: item.uid, resource_version: item.resource_version }), "已提交删除 CronJob"); });
      });
      var newBtn = box.querySelector("[data-cron-new]"); if (newBtn) newBtn.onclick = function () { cronDialog(null); };
      filterTable(box);
    }, function (err) { if (alive(seq, el) && box) box.innerHTML = errorPanel(err); });
  }

  function clBackupPage(el, seq, cur, box) {
    var base = "/clusters/" + cur.id + "/storage/";
    Promise.all([wrap(apiGet(base + "capabilities")), wrap(apiGet(base + "snapshots")), wrap(apiGet(base + "claims"))]).then(function (rs) {
      if (!alive(seq, el) || !box) return;
      var cap = rs[0].ok && rs[0].data ? rs[0].data.snapshots || {} : {}, snaps = rs[1].ok ? itemsOf(rs[1].data) : [], claims = rs[2].ok ? itemsOf(rs[2].data) : [];
      if (rs[0].err && isAuthErr(rs[0].err)) return kickLogin();
      var api = xs(), canWrite = !!(api && typeof api.can === "function" && api.can("storage:write"));
      var note = cap.supported ? "当前页面提供 Kubernetes VolumeSnapshot 恢复点；就绪状态以 snapshot controller 为准。" : (cap.reason || "集群未确认 VolumeSnapshot 能力，创建和恢复已禁用。");
      var rows = snaps.map(function (x) { var cells = ['<span class="x-name">' + gEsc(x.namespace + "/" + x.name) + "</span>", x.ready ? tagHtml("Ready", "ok") : tagHtml(x.error || "Pending", "warn"), gEsc(x.source_pvc || "—"), gEsc(x.restore_size || "—"), gEsc(fmtTime(x.created_at))]; if (canWrite && cap.supported && x.ready) cells.push('<span class="x-kacts">' + actBtn("从快照创建 PVC", "snapshot-restore", x.namespace + "/" + x.name) + "</span>"); return cells; });
      box.innerHTML = '<p class="x-frac" style="margin:0 0 12px">' + gEsc(note) + "</p>" + panelHtml("VolumeSnapshot 恢复点", snaps.length + " 个", snaps.length ? tableHtml(["命名空间 / 名称", "状态", "源 PVC", "恢复容量", "创建时间"].concat(canWrite && cap.supported ? ["操作"] : []), rows) : '<p class="x-frac" style="margin:0">当前集群没有可用的 VolumeSnapshot。备份恢复页不提供虚构的 Velero 全量备份。</p>');
      var find = function (key) { return snaps.find(function (x) { return x.namespace + "/" + x.name === key; }); };
      bindActs(box, function (act, id) {
        if (act !== "snapshot-restore") return;
        var snap = find(id); if (!snap || !snap.ready || !canWrite) return;
        var body = '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required value="' + gEsc(snap.namespace) + '" /></label><label><span class="x-req">新 PVC 名称</span><input class="x-input x-mono" name="name" required /></label></div><label><span class="x-req">容量</span><input class="x-input x-mono" name="size" required value="' + gEsc(snap.restore_size || "10Gi") + '" /></label><label><span>StorageClass</span><input class="x-input x-mono" name="storage_class" /></label><label><span>访问模式</span>' + selectHtml([{ v: "ReadWriteOnce", t: "ReadWriteOnce" }, { v: "ReadWriteMany", t: "ReadWriteMany" }], "ReadWriteOnce", 'name="access_mode"') + "</label><label><input type=\"checkbox\" name=\"confirm\" required />我确认这是基于 VolumeSnapshot 的新 PVC 恢复</label><p class=\"x-frac\">不会覆盖原 PVC，不会声称完成全量备份恢复；创建结果以 Kubernetes API 返回为准。</p>";
        formDialog("从快照创建 PVC   " + id, body, "创建 PVC", function (f, bad) { if (!f.reportValidity()) return; var ns = f.elements.namespace.value.trim(), name = f.elements.name.value.trim(); if (!ns || !name) return bad("命名空间与 PVC 名称不能为空"); storageSubmit(apiPost(base + "claims", { namespace: ns, name: name, size: f.elements.size.value.trim(), storage_class: f.elements.storage_class.value.trim(), access_modes: [f.elements.access_mode.value], volume_mode: "Filesystem", volume_name: "", snapshot_name: snap.name, labels: { "xingshu.com/restore-source": snap.namespace + "/" + snap.name } }), "已提交从快照创建 PVC"); }, aiDraftConfig("k8s.snapshot.restore", "从 prod/data-before-upgrade 创建一个名为 data-restored 的 20Gi PVC，不覆盖原 PVC。", { namespace: aiField("string", "目标命名空间"), name: aiField("string", "新 PVC 名称"), size: aiField("string", "PVC 容量，例如 20Gi"), storage_class: aiField("string", "可选 StorageClass"), access_mode: aiField("enum", "访问模式", { values: ["ReadWriteOnce", "ReadWriteMany"] }) }, true));
      });
    }, function (err) { if (alive(seq, el) && box) box.innerHTML = errorPanel(err); });
  }

  var CLUSTER_BODY = {
    nodes: clNodesPage,
    nodepools: clNodeGroupsPage,
    namespaces: clNamespacesPage,
    workloads: clWorkloadsPage,
    storage: clStoragePage,
    hpa: clHpaPage,
    cron: clCronPage,
    backup: clBackupPage,
    network: clNetworkPage,
    expose: function (el, seq, cur, box) { clNetworkPage(el, seq, cur, box, "expose"); },
    config: clConfigPage,
    helm: function (el, seq, cur, box) {
      if (typeof window !== "undefined" && typeof window.XSLiveHelmPage === "function") {
        return window.XSLiveHelmPage(el, seq, cur, box);
      }
      if (alive(seq, el) && box) box.innerHTML = emptyPanel("Helm 应用视图未加载", "请刷新页面后重试，或联系平台管理员。");
    },
    events: clEventsPage,
    logs: clLogsPage
  };

  /* ================================================================
   * 10. 团队协作 #/team —— /users + /audit-logs + /devcenter/requirements
   * ================================================================ */

  function renderTeamLive(el) {
    var seq = bump();
    mount(el, shell("团队协作", "成员、最近活动与研发负载", "") +
      loadingPanel("正在加载团队数据…"));

    Promise.all([
      wrap(apiGet("/users", { page_size: 200 })),
      wrap(apiGet("/audit-logs", { page_size: 100 })),
      wrap(apiGet("/devcenter/requirements", { page_size: 200 }))
    ]).then(function (rs) {
      if (!alive(seq, el)) return;
      var uR = rs[0], aR = rs[1], rR = rs[2];
      if (uR.err && isAuthErr(uR.err)) return kickLogin();
      if (aR.err && isAuthErr(aR.err)) return kickLogin();
      if (rR.err && isAuthErr(rR.err)) return kickLogin();
      if (!uR.ok) {
        mount(el, shell("团队协作", "成员、最近活动与研发负载", "") + errorPanel(uR.err));
        return;
      }
      var users = itemsOf(uR.data);
      var audits = aR.ok ? itemsOf(aR.data) : [];
      var reqs = rR.ok ? itemsOf(rR.data) : [];
      var rows = aggregateTeam(users, audits, reqs);
      var active = rows.filter(function (r) { return r.activity > 0; }).length;
      var openReqs = rows.reduce(function (n, r) { return n + r.open_reqs; }, 0);
      var maxAct = rows.reduce(function (n, r) { return Math.max(n, r.activity); }, 0);
      var nmap = {};
      rows.forEach(function (r) { if (r.id != null) nmap[String(r.id)] = r; });

      var memberBody = rows.length ? tableHtml(
        ["成员", "角色", "状态", "活动", "最近活动", "研发负载", "最近登录"],
        rows.map(function (r) {
          var pct = maxAct > 0 ? Math.round((r.activity / maxAct) * 100) : 0;
          return [
            "<b>" + gEsc(r.username) + "</b>" + (r.name && r.name !== r.username ? '<br /><span class="x-frac">' + gEsc(r.name) + "</span>" : ""),
            gEsc(r.roles.join("、") || "—"),
            statusTag(userStatus(r.status)),
            barHtml(pct, String(r.activity) + " 条", true),
            '<span class="x-frac">' + gEsc(r.last_activity ? fmtAgo(r.last_activity) : "—") + "</span>",
            String(r.reqs) + ' 个需求' + (r.open_reqs ? '   <span class="x-tag x-orange x-tag--s">' + r.open_reqs + " 未完成</span>" : ""),
            '<span class="x-frac">' + gEsc(fmtTime(r.last_login)) + "</span>" +
              (r.last_login_ip ? '<br /><span class="x-mono">' + gEsc(r.last_login_ip) + "</span>" : "")
          ];
        })
      ) : emptyPanel("暂无成员", "当前没有可展示的成员。");

      var feedBody = audits.length ? tableHtml(
        ["时间", "成员", "动作", "资源", "结果"],
        audits.slice(0, 20).map(function (a) {
          return [
            '<span class="x-frac">' + gEsc(fmtTime(a.created_at)) + "</span>",
            "<b>" + gEsc(a.username || "—") + "</b>",
            gEsc(a.action || a.method || "—"),
            '<span class="x-mono" title="' + gEsc(a.path || "") + '">' + gEsc(truncate(a.resource || a.path || "—", 48)) +
              (a.resource_id ? "/" + gEsc(a.resource_id) : "") + "</span>",
            statusTag(resultStatus(a.result))
          ];
        })
      ) : '<p class="x-frac" style="margin:0">' + gEsc(aR.ok ? "最近没有审计活动。" : errText(aR.err) + "（需要 audit:read 权限）") + "</p>";

      var reqBody = reqs.length ? tableHtml(
        ["需求", "状态", "优先级", "负责人", "更新时间"],
        reqs.slice(0, 20).map(function (q) {
          var owner = q.created_by == null ? null : nmap[String(q.created_by)];
          return [
            '<span class="x-name" title="' + gEsc(q.title || "") + '">' + gEsc(truncate(q.title || "—", 64)) + "</span>",
            gEsc(q.status || "—"),
            gEsc(q.priority || "—"),
            gEsc(owner ? (owner.name || owner.username) : ("用户 #" + q.created_by)),
            '<span class="x-frac">' + gEsc(fmtAgo(q.updated_at || q.created_at)) + "</span>"
          ];
        })
      ) : '<p class="x-frac" style="margin:0">' + gEsc(rR.ok ? "研发中心还没有需求。" : errText(rR.err) + "（需要 devcenter:read 权限）") + "</p>";

      mount(el, shell("团队协作", "成员、最近活动与研发负载", "") +
        statCards([
          { label: "团队成员", value: String(users.length), sub: "来自 /users" },
          { label: "活跃成员", value: String(active), sub: "最近 100 条审计内" },
          { label: "未完成需求", value: String(openReqs), sub: "按创建人聚合" },
          { label: "最近活动", value: aR.ok ? String(audits.length) : "—", sub: aR.ok ? "条审计记录" : "无 audit:read 权限" }
        ]) +
        panelHtml("成员与负载", rows.length + " 人", memberBody) +
        '<div class="x-cards" style="grid-template-columns:1fr">' +
        panelHtml("最近活动", aR.ok ? audits.length + " 条" : "", feedBody) +
        panelHtml("研发需求", rR.ok ? reqs.length + " 个" : "", reqBody) +
        "</div>");
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  /* ================================================================
   * 11. 安装：仅 live 模式覆盖 routes.<key>.render；demo 不覆盖
   * ================================================================ */

  var OWN_KEYS = { iam: 1, approvals: 1, cluster: 1, traffic: 1, team: 1 };
  var INSTALLED = false;

  function routesRef() {
    try {
      if (typeof routes !== "undefined" && routes && typeof routes === "object") return routes;
    } catch (e) {}
    return null;
  }

  function patchRoute(R, key, fn, clearSoon) {
    var r = R[key];
    if (!r || typeof r !== "object") return false;
    if (r.__xsLivePatched) return false;
    r.__xsLivePatched = true;
    r.__xsLiveOrig = typeof r.render === "function" ? r.render : null;
    r.render = function (viewEl) {
      if (!isLive()) return r.__xsLiveOrig ? r.__xsLiveOrig(viewEl) : undefined;
      return fn(viewEl);
    };
    if (clearSoon) {
      try { delete r.soon; } catch (e) { r.soon = undefined; }
    }
    return true;
  }

  // live 模式放开受 opsOn() 门禁的叶子，否则新部署会被 renderNoCluster 拦截
  function enableLiveLeaves() {
    var want = { "#/sre": 1, "#/approvals": 1, "#/cluster": 1, "#/cluster/workloads": 1, "#/iam": 1 };
    function mark(list) {
      (list || []).forEach(function (l) { if (l && l.h && want[l.h]) l.free = true; });
    }
    try { if (typeof LEAVES !== "undefined" && LEAVES) mark(LEAVES); } catch (e) {}
    try {
      if (typeof CONSOLES !== "undefined" && CONSOLES) {
        CONSOLES.forEach(function (w) { (w.items || []).forEach(function (e) { mark(e.kids || [e]); }); });
      }
    } catch (e) {}
    try {
      if (typeof ENTRIES !== "undefined" && ENTRIES) mark(ENTRIES);
    } catch (e) {}
  }

  function registerRoute(keys, fn) {
    var list = Object.prototype.toString.call(keys) === "[object Array]" ? keys : [keys];
    LIVE.routes = LIVE.routes || {};
    list.forEach(function (k) { if (k && typeof fn === "function") LIVE.routes[k] = fn; });
    if (isLive()) {
      var R = routesRef();
      if (R) list.forEach(function (k) { if (LIVE.routes[k]) patchRoute(R, k, LIVE.routes[k], false); });
    }
  }

  function installLive() {
    if (INSTALLED || !isLive()) return;
    var R = routesRef();
    if (!R) return;
    INSTALLED = true;
    LIVE.routes = LIVE.routes || {};
    patchRoute(R, "iam", renderIamLive, false);
    patchRoute(R, "approvals", renderApprovalsLive, false);
    patchRoute(R, "cluster", renderClusterLive, true);
    patchRoute(R, "traffic", renderTrafficLiveRoute, false);
    if (!R.team) R.team = { label: "团队协作", icon: "users" };
    patchRoute(R, "team", renderTeamLive, true);
    LIVE.routes.iam = renderIamLive;
    LIVE.routes.approvals = renderApprovalsLive;
    LIVE.routes.cluster = renderClusterLive;
    LIVE.routes.traffic = renderTrafficLiveRoute;
    LIVE.routes.team = renderTeamLive;
    enableLiveLeaves();
    updateBadge("live");
    flushLive();
    try {
      var v = $id("view"), a = $id("auth");
      if (v && v.childNodes.length && a && a.hidden && OWN_KEYS[routeOf().key]) {
        setTimeout(function () { if (isLive() && OWN_KEYS[routeOf().key]) repaint(); }, 30);
      }
    } catch (e) {}
  }

  /* ---------------- 右下角模式徽标（幂等，可被 xs-live-ops.js 复用） ---------------- */

  function ensureBadge() {
    var d = document;
    var el = d.getElementById("xs-live-badge");
    if (el) return el;
    var style = d.getElementById("xs-live-style");
    if (!style) {
      style = d.createElement("style");
      style.id = "xs-live-style";
      style.textContent = "#xs-live-badge{position:fixed;right:14px;bottom:14px;z-index:90;display:inline-flex;align-items:center;gap:6px;padding:5px 11px;border-radius:999px;font:12px/1.5 system-ui,-apple-system,'Segoe UI',sans-serif;background:rgba(255,255,255,.95);border:1px solid rgba(15,23,42,.1);box-shadow:0 4px 14px rgba(15,23,42,.14);color:#8c6d1f;pointer-events:none;user-select:none}#xs-live-badge i{width:7px;height:7px;border-radius:50%;background:currentColor;display:inline-block}#xs-live-badge.xs-live-ok{color:#237804}#xs-live-badge.xs-live-demo{color:#B54708}#xs-live-badge.xs-live-boot{color:#64748b}";
      (d.head || d.documentElement).appendChild(style);
    }
    el = d.createElement("div");
    el.id = "xs-live-badge";
    el.setAttribute("role", "status");
    el.innerHTML = '<i aria-hidden="true"></i><span></span>';
    (d.body || d.documentElement).appendChild(el);
    return el;
  }

  function updateBadge(state, note) {
    try {
      var el = ensureBadge();
      el.className = state === "live" ? "xs-live-ok" : state === "demo" ? "xs-live-demo" : "xs-live-boot";
      var span = el.querySelector("span");
      if (span) span.textContent = state === "live" ? "已连接" : state === "demo" ? "服务暂不可用" : "正在检查连接…";
      el.title = note || (state === "live"
        ? "管理服务已连接"
        : state === "demo"
          ? "暂时无法连接管理服务，请检查网络或稍后重试"
          : "正在检查管理服务连接…");
      return el;
    } catch (e) {
      return null;
    }
  }

  /* ---------------- boot ---------------- */

  var BOOTED = false;

  function bootOnce() {
    updateBadge("boot");
    var x = xs();
    if (!x) {
      updateBadge("demo", "平台连接暂不可用，请检查网络或稍后重试");
      return;
    }
    function onMode(mode) {
      mode = mode || x.mode || "demo";
      updateBadge(mode === "live" ? "live" : "demo");
      if (mode === "live") installLive();
      LIVE.mode = mode;
    }
    function start() {
      if (BOOTED) return;
      BOOTED = true;
      if (x.ready && typeof x.ready.then === "function") {
        x.ready.then(function (mode) { onMode(mode); }, function () { onMode(x.mode); });
      } else {
        onMode(x.mode);
      }
      if (typeof x.on === "function") x.on("mode", function (mode) { onMode(mode); });
      if (typeof x.on === "function") x.on("auth", function (payload) {
        if (payload === null) updateBadge(isLive() ? "live" : "demo");
      });
    }
    start();
  }

  function boot() {
    if (BOOTED || typeof document === "undefined") return;
    if (!routesRef()) {
      if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", bootOnce);
        return;
      }
      setTimeout(bootOnce, 60);
      return;
    }
    bootOnce();
  }

  // 共享命名空间对外能力（供 xs-live-ops.js 复用；全部幂等）
  LIVE.pure = LIVE.pure || PURE;
  LIVE.isLive = isLive;
  LIVE.repaint = repaint;
  LIVE.updateBadge = updateBadge;
  if (typeof LIVE.badge !== "function") LIVE.badge = updateBadge;
  if (typeof LIVE.registerRoute !== "function") LIVE.registerRoute = registerRoute;
  LIVE.core = {
    version: VERSION,
    routes: ["sre", "iam", "approvals", "cluster", "traffic", "team"],
    install: installLive,
    isLive: isLive,
    repaint: repaint
  };

  boot();
})();
