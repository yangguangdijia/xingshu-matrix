/* 星枢 Matrix · 实时核心页适配器（xs-live-core.js）
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

  function approvalScopeDefault(canReadAll) { return canReadAll ? "all" : "todo"; }

  var PURE = {
    esc: escLocal,
    parseHash: parseHash,
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

  function selfTag() {
    return '<span class="x-tag x-green x-pill" title="数据来自 matrix-server 实时接口">实时数据</span>';
  }

  function refreshButton(label) {
    return '<button type="button" class="x-btn" data-live-refresh>' + gEsc(label || "刷新") + '</button>';
  }

  function refreshBase() {
    return '<span class="x-frac" style="margin:0 8px 0 auto;max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' +
      gEsc("API base：" + apiBase()) + '">' + gEsc(apiBase()) + '</span>';
  }

  function shell(title, desc, actions) {
    return pheadSafe(title, desc, selfTag() + refreshBase() + (actions || "") + refreshButton());
  }

  function loadingPanel(text) {
    return '<section class="x-panel" aria-busy="true"><p class="x-frac" style="margin:0">' + gEsc(text || "正在从 matrix-server 加载…") + '</p></section>';
  }

  function errorPanel(err, retryLabel) {
    var msg = (err && err.message) || String(err || "请求失败");
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

  function bindTabs(el) {
    Array.prototype.forEach.call(el.querySelectorAll("[data-live-tab]"), function (b) {
      b.onclick = function (e) { e.preventDefault(); goHash(b.getAttribute("data-live-tab")); };
    });
    Array.prototype.forEach.call(el.querySelectorAll("[data-live-cluster-select]"), function (s) {
      s.onchange = function () {
        var sub = s.getAttribute("data-sub") || "nodes";
        goHash("#/cluster/" + sub + "/" + encodeURIComponent(s.value));
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
    return err.message || ("请求失败（HTTP " + (err.status || "?") + "）");
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
  function apiDel(path) {
    var x = xs();
    if (!x) return Promise.reject(new Error("xs-api.js 未加载，无法访问后端"));
    return x.del(path);
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

  function formDialog(title, bodyHtml, okLabel, onSubmit) {
    var m = openModal(title,
      '<form class="x-form" id="liveForm" novalidate>' + bodyHtml +
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
    var first = f.querySelector("input,textarea,select");
    if (first) { try { first.focus(); } catch (e) {} }
    return f;
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
          kpi("集群数", String(clusters.length), clusters.length ? clusters.map(function (c) { return c.env || "—"; }).join(" · ") : "尚未接入", "#/cluster/list") +
          kpi("节点 Ready / 总数", nodesAll.length ? ready + " / " + nodesAll.length : "—", nodesAll.length ? (nodesAll.length - ready) + " 个 NotReady" : "未接入集群", "#/cluster/nodes") +
          kpi("Pod 运行 / 总数", podsKnown ? String(podsTotal) : "—", podsKnown ? "来自 /workloads/pods" : "未接入", "#/cluster/workloads") +
          kpi("CPU 使用率", "—", metricsOK ? "metrics-server 已就绪" : "metrics-server 未安装", "#/observe") +
          kpi("内存使用率", "—", metricsOK ? "metrics-server 已就绪" : "metrics-server 未安装", "#/observe") +
          "</div>";

        // 24h 曲线：没有 Prometheus 时保留卡片与图例，内部显示未接入
        html += '<section class="x-panel x-setform"><div class="x-sethead"><h2 class="x-h2">集群资源使用 · 24h</h2>' +
          '<span class="x-frac">CPU / 内存 · 全部集群均值</span></div>' +
          '<div class="x-tbar" style="margin:0 0 8px"><span class="x-tag x-blue">CPU</span><span class="x-tag x-green">内存</span>' +
          '<span class="x-grow"></span><button class="x-btn" type="button" data-goto="#/settings">配置数据源</button></div>' +
          '<div class="x-ochart" style="display:grid;place-items:center">' +
          '<div style="text-align:center"><strong>尚未接入 Prometheus</strong><p class="x-frac" style="margin:6px 0 0">在「设置 › 系统接入」配置 Prometheus 地址后，这里显示 24h CPU/内存曲线。</p></div>' +
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
            '<span class="x-frac">' + gEsc((n.created_by || "") + " · " + fmtTime(n.created_at)) + "</span>" +
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
            '</span><span class="x-frac">' + gEsc(a.status || "firing") + " · " + gEsc(fmtAgo(a.starts_at || a.created_at)) + "</span></li>";
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
    mount(el, shell("用户管理", "平台账号、角色与登录状态（/users · /roles）", "") + loadingPanel("正在加载用户…"));

    Promise.all([wrap(apiGet("/users", { page_size: 200 })), wrap(apiGet("/roles"))]).then(function (rs) {
      if (!alive(seq, el)) return;
      var uR = rs[0], rR = rs[1];
      if (!uR.ok) {
        if (isAuthErr(uR.err)) return kickLogin();
        mount(el, shell("用户管理", "平台账号、角色与登录状态（/users · /roles）", "") + errorPanel(uR.err));
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
      ) : emptyPanel("暂无用户数据", "/users 返回空列表");

      var html = shell("用户管理", "平台账号、角色与登录状态（/users · /roles）", "") +
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
      (checks || '<span class="x-frac">没有可分配的角色（请先在 #/iam/roles 创建）</span>') + "</fieldset>",
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
    mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录（/users · /roles · /permissions）", "") +
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
        mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录（/users · /roles · /permissions）", "") + errorPanel(uR.err));
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

      mount(el, shell("部门与用户组", "按角色聚合的成员与可授予权限目录（/users · /roles · /permissions）", "") +
        '<p class="x-frac" style="margin:0 0 12px">后端暂未提供部门/用户组 API；本页按角色聚合成员（/users 的角色字段）并展示可授予权限目录（/permissions）。</p>' +
        panelHtml("按角色聚合的用户组", roles.length + " 个角色", roleBody) +
        panelHtml("权限目录", perms.length + " 项权限", permBody));
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function iamRolesLive(el) {
    var seq = bump();
    mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略（/roles · /env-policies）", "") +
      loadingPanel("正在加载角色与环境策略…"));

    Promise.all([wrap(apiGet("/roles")), wrap(apiGet("/env-policies"))]).then(function (rs) {
      if (!alive(seq, el)) return;
      var rR = rs[0], eR = rs[1];
      if (!rR.ok) {
        if (isAuthErr(rR.err)) return kickLogin();
        mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略（/roles · /env-policies）", "") + errorPanel(rR.err));
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

      mount(el, shell("角色权限", "角色、环境范围与环境变更审批策略（/roles · /env-policies）", "") +
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
        resultDialog("角色权限 · " + (role.name || role.code || id), body || '<p class="x-frac" style="margin:0">该角色没有任何权限。</p>');
      });
    }, function (err) {
      loadFail(el, seq, err);
      if (isAuthErr(err)) kickLogin();
    });
  }

  function iamLogsLive(el) {
    var seq = bump();
    mount(el, shell("登录日志", "登录审计与活跃会话（/audit-logs · /auth/sessions）", "") + loadingPanel("正在加载登录日志与会话…"));

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

      mount(el, shell("登录日志", "登录审计与活跃会话（/audit-logs · /auth/sessions）", "") +
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

    var base = shell("变更审批", "审批单列表与处理（/approvals）；生产变更返回 202 时会自动创建审批单", "");
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

      mount(el, shell("变更审批", "审批单列表与处理（/approvals）；生产变更返回 202 时会自动创建审批单", "") +
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
    mount(el, shell("主机纳管", "主机清单、注册令牌与批量任务（/hosts）", "") + loadingPanel("正在加载主机…"));

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
            gEsc((f.cpu_cores || "?") + "C") + " · " + gEsc(fmtBytes(f.mem_total)),
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

      mount(el, shell("主机纳管", "主机清单、注册令牌与批量任务（/hosts）", "") +
        statCards([
          { label: "主机总数", value: hR.ok ? String(hosts.length) : "—", sub: "host:read" },
          { label: "在线主机", value: hR.ok ? String(online) : "—", sub: "agent 心跳 90s 内" },
          { label: "注册令牌", value: tR.ok ? String(HOST_TOKENS.length) : "—", sub: "host:write" },
          { label: "离线主机", value: hR.ok ? String(hosts.length - online) : "—", sub: "待排查" }
        ]) +
        '<div class="x-tbar">' + clusterTabsHtml("hosts", null) + "</div>" +
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
      bindTabs(el);

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
    formDialog("在主机执行命令 · " + (h.name || h.ip),
      '<p class="x-frac" style="margin:0 0 8px">目标：' + gEsc(h.name || "") + "（" + gEsc(h.ip || "—") + "）· 环境 " + gEsc(h.env || "—") +
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
    resultDialog("命令结果 · " + (h.name || h.ip),
      '<p style="margin:0 0 8px">状态：' + statusTag(st) + " · 退出码：" + mono(task.exit_code == null ? "—" : task.exit_code) +
      " · 任务 #" + gEsc(task.id) + "</p>" +
      '<pre class="x-log" style="max-height:46vh;overflow:auto;margin:0" aria-label="命令输出">' +
      gEsc(task.output || task.error || "（无输出）") + "</pre>");
  }

  function hostTasksDialog(h) {
    var m = openModal("任务记录 · " + (h.name || h.ip), '<div class="x-form"><p class="x-frac" style="margin:0">正在加载任务记录…</p></div>',
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
            '<p style="margin:0 0 6px">名称：<b>' + gEsc(tok.name || "—") + "</b> · 环境 " + gEsc(tok.env || "—") + "</p>" +
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
        gEsc(h.name || h.ip) + ' <span class="x-frac">' + gEsc(h.ip || "") + " · " + gEsc(h.env || "") + (h.online ? "" : " · 离线") + "</span></label>";
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
   * 9. 集群状态 #/cluster/<sub> —— /clusters/:id/{nodes,namespaces,workloads,events}
   * ================================================================ */

  var CLUSTERS = [];
  var CL_SELECTED = null;
  var CLSTATE = { kind: "deployments", ns: "", evtype: "", logNs: "", logPod: "", logTail: "500" };

  var CLUSTER_TITLES = {
    list: "集群列表",
    nodes: "节点",
    namespaces: "命名空间",
    workloads: "工作负载",
    events: "事件",
    logs: "Pod 日志",
    hosts: "主机纳管"
  };

  var CLUSTER_TABS = [
    ["list", "集群列表", "#/cluster/list"],
    ["nodes", "节点", "#/cluster/nodes"],
    ["namespaces", "命名空间", "#/cluster/namespaces"],
    ["workloads", "工作负载", "#/cluster/workloads"],
    ["events", "事件", "#/cluster/events"],
    ["logs", "日志", "#/cluster/logs"],
    ["hosts", "主机", "#/cluster/hosts"]
  ];

  function clusterTabsHtml(active, cluster) {
    return '<div class="x-tabs" role="tablist">' + CLUSTER_TABS.map(function (t) {
      var href = t[2];
      if (cluster && t[0] !== "list" && t[0] !== "hosts") href += "/" + cluster.id;
      return '<button type="button" role="tab" class="' + (active === t[0] ? "on" : "") + '" data-live-tab="' + href +
        '" aria-selected="' + (active === t[0]) + '">' + t[1] + "</button>";
    }).join("") + "</div>";
  }

  function clusterSelHtml(clusters, cluster, sub) {
    if (!clusters || !clusters.length) return "";
    return selectHtml(clusters.map(function (c) {
      return { v: c.id, t: (c.name || "#" + c.id) + (c.env ? " · " + c.env : "") };
    }), cluster ? cluster.id : "", 'data-live-cluster-select data-sub="' + sub + '" aria-label="选择集群"');
  }

  function renderClusterLive(el) {
    var r = routeOf();
    var sub = r.sub || "nodes";
    if (sub === "list") return clListPage(el);
    if (sub === "hosts") return renderHostsLive(el);
    return clDataPage(el, sub, r.sel);
  }

  function clListPage(el) {
    var seq = bump();
    mount(el, shell("集群列表", "已接入的 Kubernetes 集群（/clusters）", "") + loadingPanel("正在加载集群列表…"));
    apiGet("/clusters", { page_size: 200 }).then(function (data) {
      if (!alive(seq, el)) return;
      CLUSTERS = itemsOf(data);
      window.XS_CLUSTERS = CLUSTERS; // 供主脚本的环境计数/筛选用
      var html = shell("集群列表", "已接入的 Kubernetes 集群（/clusters）；点「节点」查看实时资源", "") +
        '<div class="x-tbar">' + clusterTabsHtml("list", null) + "</div>";
      if (!CLUSTERS.length) {
        mount(el, html + emptyPanel("尚未接入集群", "通过 POST /api/v1/clusters 提交 kubeconfig 接入集群；接入后这里会显示节点、命名空间、工作负载与事件。"));
        bindTabs(el);
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
      bindTabs(el);
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
          if (typeof dlg === "function") dlg("调整集群环境 · " + gEsc(c.name), body, foot);
          else openModal("调整集群环境 · " + gEsc(c.name), body, foot);
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
            if (res.ok) toastMsg("连接成功 · Kubernetes " + (res.version || ""));
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

  function clDataPage(el, sub, sel) {
    var seq = bump();
    var title = CLUSTER_TITLES[sub] || "集群状态";
    mount(el, shell(title, "集群实时数据（/clusters/:id/…）", "") + loadingPanel("正在加载集群列表…"));
    apiGet("/clusters", { page_size: 200 }).then(function (data) {
      if (!alive(seq, el)) return;
      CLUSTERS = itemsOf(data);
      window.XS_CLUSTERS = CLUSTERS; // 供主脚本的环境计数/筛选用
      if (!CLUSTERS.length) {
        mount(el, shell(title, "集群实时数据（/clusters/:id/…）", "") + '<div class="x-tbar">' + clusterTabsHtml(sub, null) + "</div>" +
          emptyPanel("尚未接入集群", "集群接入后，节点、命名空间、工作负载、事件与 Pod 日志会显示在这里。"));
        bindTabs(el);
        return;
      }
      var cur = resolveCluster(CLUSTERS, sel) || CLUSTERS[0];
      CL_SELECTED = cur;
      if (String(sel || "") !== String(cur.id)) {
        try { history.replaceState(null, "", "#/cluster/" + sub + "/" + cur.id); } catch (e) {}
      }
      mount(el, shell(title, "集群 " + (cur.name || "#" + cur.id) + " 的实时数据（/clusters/:id/…）", "") +
        '<div class="x-tbar">' + clusterSelHtml(CLUSTERS, cur, sub) + clusterTabsHtml(sub, cur) + "</div>" +
        '<div id="liveClusterBody">' + loadingPanel("正在加载…") + "</div>");
      bindTabs(el);
      var box = $id("liveClusterBody");
      var renderBody = CLUSTER_BODY[sub];
      if (!renderBody) {
        if (box) box.innerHTML = emptyPanel("该子页面尚未接入实时数据", "当前实时适配覆盖：集群列表、节点、命名空间、工作负载、事件、Pod 日志与主机。");
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
  function clNodeDetail(el, cur, box, name, nodeRowData) {
    var host = $id("liveNodeDetail");
    if (!host) {
      host = document.createElement("div");
      host.id = "liveNodeDetail";
      box.parentNode.insertBefore(host, box.nextSibling);
    }
    host.innerHTML = loadingPanel("正在加载节点 " + gEsc(name) + " …");
    apiGet("/clusters/" + cur.id + "/nodes/" + encodeURIComponent(name)).then(function (res) {
      // 后端返回 {node:{…}, metrics_available:bool}
      var n = (res && res.node) || res;
      var metricsOk = !!(res && res.metrics_available);
      if (!n || !n.name) { host.innerHTML = emptyPanel("节点不存在", name); return; }
      var cap = n.capacity || {}, alloc = n.allocatable || {};
      var kv = function (k) { return (alloc[k] || cap[k] || "—"); };
      var labels = Object.keys(n.labels || {}).sort();
      var taints = n.taints || [];
      var conds = n.conditions || [];
      var usage = n.usage || null;
      var row = function (k, v) { return "<dt>" + gEsc(k) + "</dt><dd>" + v + "</dd>"; };
      var detail =
        '<div class="x-tbar" style="margin:0 0 12px"><h2 class="x-h2" style="margin:0">节点 ' + gEsc(n.name) + '</h2>' +
        (n.ready ? tagHtml("Ready", "ok") : tagHtml(n.status || "NotReady", "bad")) +
        (n.unschedulable ? tagHtml("已封锁", "warn") : "") +
        '<span class="x-grow"></span><button class="x-btn" type="button" data-ndclose>关闭</button></div>' +
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
      host.innerHTML = '<section class="x-panel">' + detail + "</section>";
      var close = host.querySelector("[data-ndclose]");
      if (close) close.onclick = function () { host.remove(); };
      host.scrollIntoView({ behavior: "smooth", block: "nearest" });
    }, function (err) {
      host.innerHTML = errorPanel(err);
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
      box.innerHTML = panelHtml("节点", items.length + " 个" + (metricsOk ? "" : " · metrics-server 不可用（无 CPU/内存使用率）"), body);
      [].slice.call(el.querySelectorAll("[data-node]")).forEach(function (a) {
        a.onclick = function () { clNodeDetail(el, cur, box, a.getAttribute("data-node")); };
      });
      bindActs(el, function (act, id) {
        var basePath = "/clusters/" + cur.id + "/nodes/" + encodeURIComponent(id);
        if (act === "node-detail") clNodeDetail(el, cur, box, id);
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
          if (typeof dlg === "function") dlg("命名空间 · " + gEsc(item.name), b2, f2); else openModal("命名空间", b2, f2);
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
            if (typeof dlg === "function") dlg(kind + "/" + gEsc(name) + " · YAML", body, foot);
            else openModal("YAML", body, foot);
          }, function (err) { toast(errText(err)); });
          return;
        }
        if (act === "wl-scale") {
          var sbody = '<form class="x-form" id="scf" novalidate><label><span>副本数</span><input class="x-input" type="number" min="0" step="1" name="replicas" value="1" required /></label>' +
            '<p class="x-frac" style="margin:8px 0 0">将调整 ' + gEsc(kind + "/" + name) + " 的期望副本数。</p></form>";
          var sfoot = '<button class="x-btn" type="button" data-close>取消</button><button class="x-btn x-btn--pri" type="button" id="scok">保存</button>';
          if (typeof dlg === "function") dlg("扩缩容 · " + gEsc(name), sbody, sfoot); else openModal("扩缩容", sbody, sfoot);
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

  var CLUSTER_BODY = {
    nodes: clNodesPage,
    namespaces: clNamespacesPage,
    workloads: clWorkloadsPage,
    events: clEventsPage,
    logs: clLogsPage
  };

  /* ================================================================
   * 10. 团队协作 #/team —— /users + /audit-logs + /devcenter/requirements
   * ================================================================ */

  function renderTeamLive(el) {
    var seq = bump();
    mount(el, shell("团队协作", "成员、最近活动与研发负载（/users · /audit-logs · /devcenter/requirements）", "") +
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
        mount(el, shell("团队协作", "成员、最近活动与研发负载（/users · /audit-logs · /devcenter/requirements）", "") + errorPanel(uR.err));
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
            String(r.reqs) + ' 个需求' + (r.open_reqs ? ' · <span class="x-tag x-orange x-tag--s">' + r.open_reqs + " 未完成</span>" : ""),
            '<span class="x-frac">' + gEsc(fmtTime(r.last_login)) + "</span>" +
              (r.last_login_ip ? '<br /><span class="x-mono">' + gEsc(r.last_login_ip) + "</span>" : "")
          ];
        })
      ) : emptyPanel("暂无成员", "/users 返回空列表");

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

      mount(el, shell("团队协作", "成员、最近活动与研发负载（/users · /audit-logs · /devcenter/requirements）", "") +
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

  var OWN_KEYS = { sre: 1, iam: 1, approvals: 1, cluster: 1, team: 1 };
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
    patchRoute(R, "sre", renderSreLive, false);
    patchRoute(R, "iam", renderIamLive, false);
    patchRoute(R, "approvals", renderApprovalsLive, false);
    patchRoute(R, "cluster", renderClusterLive, false);
    if (!R.team) R.team = { label: "团队协作", icon: "users" };
    patchRoute(R, "team", renderTeamLive, true);
    LIVE.routes.sre = renderSreLive;
    LIVE.routes.iam = renderIamLive;
    LIVE.routes.approvals = renderApprovalsLive;
    LIVE.routes.cluster = renderClusterLive;
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
      if (span) span.textContent = state === "live" ? "实时数据" : state === "demo" ? "后端未连接" : "连接检测中…";
      el.title = note || (state === "live"
        ? "已连接 matrix-server：" + apiBase()
        : state === "demo"
          ? "未检测到 matrix-server，页面无法读取实时数据"
          : "正在探测 matrix-server…");
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
      updateBadge("demo", "xs-api.js 未加载，无法访问后端；实时数据不可用");
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
    routes: ["sre", "iam", "approvals", "cluster", "team"],
    install: installLive,
    isLive: isLive,
    repaint: repaint
  };

  boot();
})();
