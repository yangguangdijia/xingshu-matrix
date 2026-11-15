/* 系统设置 → 平台备份。读写真实接口（server/internal/backup/api.go）：
 *   列表        GET  /platform/backups?page=&page_size=      （setting:read）
 *   立即备份    POST /platform/backups                         （setting:manage，202 + 记录）
 *   下载        GET  /platform/backups/:id/download            （setting:manage，二进制，带 X-Content-SHA256）
 *   备份策略    GET  /platform/backup-policy                   （setting:read，只读：来自部署配置）
 * 恢复：后端目前只提供离线恢复（停服后在服务器上执行），policy.online_restore=false 时不显示「恢复」按钮，
 *       只展示离线恢复说明；后端开放在线恢复（online_restore=true）后，按钮走
 *       POST /platform/backups/:id/restore {confirm:"恢复"}，需输入「恢复」二字二次确认。
 * 挂载：window.XSPlatformBackup.render(el)
 */
(function (root) {
  "use strict";
  var PAGE_SIZE = 20;
  var POLL_MS = 5000;
  var VERIFY_MAX = 512 * 1024 * 1024; // 超过此大小不在浏览器内计算 SHA-256
  var KIND = { manual: "手动备份", scheduled: "定时备份", pre_migrate: "升级前备份" };
  var STATUS = { running: ["进行中", "run"], succeeded: ["成功", "ok"], failed: ["失败", "bad"] };
  var WEEK = ["周日", "周一", "周二", "周三", "周四", "周五", "周六", "周日"];

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
  function fmtSize(n) {
    n = Number(n) || 0;
    if (n <= 0) return "—";
    var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)) + " " + u[i];
  }
  function fmtDuration(a, b) {
    if (!a || !b) return "—";
    var s = Math.round((new Date(b) - new Date(a)) / 1000);
    if (!(s >= 0)) return "—";
    if (s < 60) return s + " 秒";
    if (s < 3600) return Math.floor(s / 60) + " 分 " + (s % 60) + " 秒";
    return Math.floor(s / 3600) + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
  }
  // 常见 5 段计划的中文描述；无法描述时原样给出。
  function describeCron(expr) {
    var f = String(expr || "").trim().split(/\s+/);
    if (f.length !== 5) return expr ? "按计划「" + expr + "」" : "—";
    var isNum = function (s) { return /^\d+$/.test(s); };
    var hm = isNum(f[0]) && isNum(f[1]) ? pad(+f[1]) + ":" + pad(+f[0]) : "";
    if (hm && f[2] === "*" && f[3] === "*" && f[4] === "*") return "每天 " + hm;
    if (hm && f[2] === "*" && f[3] === "*" && isNum(f[4]) && +f[4] <= 7) return "每" + WEEK[+f[4]] + " " + hm;
    if (hm && isNum(f[2]) && f[3] === "*" && f[4] === "*") return "每月 " + (+f[2]) + " 日 " + hm;
    if (isNum(f[0]) && f[1] === "*" && f[2] === "*" && f[3] === "*" && f[4] === "*") return "每小时第 " + (+f[0]) + " 分";
    return "按计划「" + expr + "」";
  }

  function css() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-pbk-css")) return;
    var s = doc.createElement("style"); s.id = "xs-pbk-css";
    s.textContent = [
      ".xpb-card{background:#fff;border:1px solid var(--x-line,#e6e9ed);border-radius:10px;padding:24px 28px;margin-bottom:20px}",
      ".xpb-card h3{margin:0 0 16px;font-size:16px}",
      ".xpb-head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:16px}.xpb-head h3{margin:0}",
      ".xpb-acts{display:flex;gap:8px}",
      ".xpb-row{display:grid;grid-template-columns:160px minmax(0,1fr);gap:16px;padding:10px 0;border-bottom:1px solid var(--x-line,#f0f2f5);font-size:14px}",
      ".xpb-row:last-of-type{border-bottom:0}.xpb-row>span{color:var(--x-sub,#4b5563)}",
      ".xpb-note{font-size:13px;color:var(--x-sub,#6b7280);margin:12px 0 0}",
      ".xpb-table{width:100%;border-collapse:collapse;font-size:14px}",
      ".xpb-table th,.xpb-table td{padding:11px 10px;border-bottom:1px solid var(--x-line,#f0f2f5);text-align:left;vertical-align:middle}",
      ".xpb-table th{font-weight:500;color:var(--x-sub,#6b7280);font-size:13px;background:#fafbfc}",
      ".xpb-mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:var(--x-sub,#6b7280)}",
      ".xpb-tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:12px;line-height:18px;border:1px solid}",
      ".xpb-tag--ok{color:#15803d;background:#f0fdf4;border-color:#bbf7d0}.xpb-tag--bad{color:#b91c1c;background:#fef2f2;border-color:#fecaca}",
      ".xpb-tag--run{color:#1d4ed8;background:#eff6ff;border-color:#bfdbfe}",
      ".xpb-err{color:#b91c1c;font-size:13px;margin:4px 0 0}.xpb-empty{padding:28px 0;text-align:center;color:var(--x-sub,#6b7280)}",
      ".xpb-link{border:0;background:none;padding:0 8px 0 0;color:var(--x-blue,#2563eb);cursor:pointer;font-size:14px}",
      ".xpb-link[disabled]{color:#9ca3af;cursor:not-allowed}.xpb-link--danger{color:#b91c1c}",
      ".xpb-pager{display:flex;align-items:center;justify-content:flex-end;gap:10px;margin-top:12px;font-size:13px;color:var(--x-sub,#6b7280)}",
      ".xpb-mask{position:fixed;inset:0;background:rgba(15,23,42,.45);display:flex;align-items:center;justify-content:center;z-index:1000}",
      ".xpb-modal{background:#fff;border-radius:10px;width:480px;max-width:calc(100vw - 32px);padding:24px}",
      ".xpb-modal h3{margin:0 0 12px;font-size:16px}.xpb-warn{background:#fef2f2;border:1px solid #fecaca;color:#991b1b;border-radius:6px;padding:10px 12px;font-size:13px;line-height:1.6;margin-bottom:14px}",
      ".xpb-modal label{display:block;font-size:13px;color:var(--x-sub,#4b5563);margin-bottom:6px}.xpb-modal .x-input{width:100%;height:36px}",
      ".xpb-foot{display:flex;justify-content:flex-end;gap:10px;margin-top:18px}",
      ".xpb .x-btn--danger{background:#d43030;border-color:#d43030;color:#fff}.xpb .x-btn--danger:hover{background:#b92424}"
    ].join("\n");
    doc.head.appendChild(s);
  }

  function statusTag(r) {
    var s = STATUS[r.status] || [r.status || "未知", "run"];
    return '<span class="xpb-tag xpb-tag--' + s[1] + '"' + (r.error ? ' title="' + esc(r.error) + '"' : "") + ">" + esc(s[0]) + "</span>" +
      (r.status === "failed" && r.error ? '<div class="xpb-err">' + esc(r.error.length > 120 ? r.error.slice(0, 120) + "…" : r.error) + "</div>" : "");
  }

  // 浏览器内保存文件（测试可通过 XSPlatformBackup._save 替换）。
  function saveBlob(blob, name) {
    var doc = root.document, U = root.URL;
    if (!doc || !U || !U.createObjectURL) throw new Error("当前浏览器不支持文件下载");
    var href = U.createObjectURL(blob), a = doc.createElement("a");
    a.href = href; a.download = name || "platform-backup.dump"; a.style.display = "none";
    (doc.body || doc.documentElement).appendChild(a); a.click(); a.remove();
    setTimeout(function () { U.revokeObjectURL(href); }, 1000);
  }
  function hex(buf) {
    var b = new Uint8Array(buf), out = "";
    for (var i = 0; i < b.length; i++) out += (b[i] < 16 ? "0" : "") + b[i].toString(16);
    return out;
  }
  // 返回 "ok" | "skip" ；不一致时抛错。
  function verify(blob, expect) {
    var subtle = root.crypto && root.crypto.subtle;
    if (!expect || !subtle || !blob.arrayBuffer || blob.size > VERIFY_MAX) return Promise.resolve("skip");
    return blob.arrayBuffer().then(function (buf) { return subtle.digest("SHA-256", buf); }).then(function (d) {
      if (hex(d) !== String(expect).toLowerCase()) throw new Error("下载的文件校验值与备份记录不一致，文件可能已损坏，请重新下载");
      return "ok";
    });
  }
  function fetchDownload(id, retried) {
    var x = X(), headers = {};
    var tok = x.token && x.token();
    if (tok) headers.Authorization = "Bearer " + tok;
    var url = (x.base || "/api/v1") + "/platform/backups/" + encodeURIComponent(id) + "/download";
    if (typeof root.fetch !== "function") return Promise.reject(new Error("当前浏览器不支持文件下载"));
    return root.fetch(url, { headers: headers, credentials: "same-origin" }).then(function (res) {
      if (res.status === 401 && !retried && x.refresh && x.refreshToken && x.refreshToken()) {
        return x.refresh().then(function () { return fetchDownload(id, true); });
      }
      if (!res.ok) {
        return res.json().catch(function () { return null; }).then(function (p) {
          throw new Error((p && p.message) || ("下载失败（HTTP " + res.status + "）"));
        });
      }
      return res;
    }, function () { throw new Error("无法连接服务器，下载失败"); });
  }
  function download(rec) {
    return fetchDownload(rec.id).then(function (res) {
      var expect = (res.headers && res.headers.get && res.headers.get("X-Content-SHA256")) || rec.sha256 || "";
      return res.blob().then(function (blob) {
        return verify(blob, expect).then(function (v) {
          (root.XSPlatformBackup._save || saveBlob)(blob, rec.file);
          return v;
        });
      });
    });
  }

  function render(el) {
    if (!el) return;
    css();
    var serial = (el._xsPbkSerial || 0) + 1; el._xsPbkSerial = serial;
    var st = { page: 1, items: [], total: 0, policy: null, policyMissing: false, loading: true, error: "", creating: false, downloading: {}, restore: null };
    var pollTimer = null;
    var canManage = can("setting:manage");
    function alive() { return el._xsPbkSerial === serial && el.isConnected !== false; }

    function load(quiet) {
      if (!quiet) { st.loading = true; paint(); }
      var list = api("/platform/backups", { query: { page: st.page, page_size: PAGE_SIZE } });
      var pol = st.policy || st.policyMissing ? Promise.resolve(st.policy) : api("/platform/backup-policy").catch(function (e) {
        if (e && Number(e.status) === 404) { st.policyMissing = true; return null; }
        throw e;
      });
      return Promise.all([list, pol]).then(function (r) {
        if (!alive()) return;
        var d = r[0] || {};
        st.items = Array.isArray(d) ? d : (Array.isArray(d.items) ? d.items : []);
        st.total = Number(d.total) || st.items.length;
        st.policy = r[1] || null;
        st.error = ""; st.loading = false;
        paint(); schedulePoll();
      }, function (e) {
        if (!alive()) return;
        st.loading = false; st.error = errMsg(e); paint();
      });
    }
    function schedulePoll() {
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
      if (!st.items.some(function (r) { return r.status === "running"; })) return;
      pollTimer = setTimeout(function () { pollTimer = null; if (alive()) load(true); }, POLL_MS);
    }

    function policyHTML() {
      var p = st.policy;
      if (!p) return "";
      var restoreRow = p.online_restore
        ? "在线恢复：在下方列表中选择成功的备份执行恢复"
        : "离线恢复：恢复会覆盖平台全部数据，需由运维停止平台服务后在服务器上执行，执行前请核对备份的 SHA-256 校验值";
      return '<section class="xpb-card"><h3>备份策略</h3>' +
        '<div class="xpb-row"><span>定时备份</span><div>' + (p.schedule_enabled ? "已开启 · " + esc(describeCron(p.schedule)) : "未开启") + "</div></div>" +
        '<div class="xpb-row"><span>保留策略</span><div>' + (Number(p.keep) > 0 ? "保留最近 " + esc(p.keep) + " 份成功的定时备份" : "不自动清理") + "；手动备份与升级前备份不会自动清理</div></div>" +
        '<div class="xpb-row"><span>升级前备份</span><div>' + (p.before_migrate ? "已开启 · 数据库升级前自动备份，备份失败时" + (p.before_migrate_on_failure === "warn" ? "仅告警并继续升级" : "中止升级") : "未开启") + "</div></div>" +
        '<div class="xpb-row"><span>存储位置</span><div>' + (p.storage === "local" ? "服务器本地备份目录" : "对象存储") + "</div></div>" +
        '<div class="xpb-row"><span>恢复方式</span><div>' + esc(restoreRow) + "</div></div>" +
        '<p class="xpb-note">以上设置由部署配置决定，如需调整请修改部署配置后重启平台服务。</p></section>';
    }

    function rowHTML(r) {
      var ok = r.status === "succeeded" && r.file;
      var acts = "";
      if (canManage && ok) {
        acts += '<button type="button" class="xpb-link" data-pbk-click="download" data-id="' + esc(r.id) + '"' + (st.downloading[r.id] ? " disabled" : "") + ">" + (st.downloading[r.id] ? "下载中…" : "下载") + "</button>";
        if (st.policy && st.policy.online_restore) acts += '<button type="button" class="xpb-link xpb-link--danger" data-pbk-click="restore" data-id="' + esc(r.id) + '">恢复</button>';
      }
      return '<tr data-pbk-row="' + esc(r.id) + '"><td>' + esc(KIND[r.kind] || r.kind) + "</td>" +
        "<td>" + esc(fmtTime(r.created_at)) + "</td>" +
        "<td>" + esc(r.status === "running" ? "—" : fmtDuration(r.created_at, r.finished_at)) + "</td>" +
        "<td>" + esc(fmtSize(r.size)) + "</td>" +
        "<td>" + statusTag(r) + "</td>" +
        '<td><span class="xpb-mono" title="' + esc(r.sha256 || "") + '">' + esc(r.sha256 ? r.sha256.slice(0, 12) + "…" : "—") + "</span></td>" +
        "<td>" + (acts || '<span class="xpb-mono">—</span>') + "</td></tr>";
    }

    function listHTML() {
      var head = '<div class="xpb-head"><h3>备份记录</h3><div class="xpb-acts">' +
        '<button type="button" class="x-btn" data-pbk-click="reload">刷新</button>' +
        (canManage ? '<button type="button" class="x-btn x-btn--pri" data-pbk-click="create"' + (st.creating ? " disabled" : "") + ">" + (st.creating ? "正在发起…" : "立即备份") + "</button>" : "") +
        "</div></div>";
      if (st.loading && !st.items.length) return '<section class="xpb-card">' + head + '<p class="xpb-empty">正在加载…</p></section>';
      if (st.error) return '<section class="xpb-card">' + head + '<p class="xpb-err" data-pbk-error>' + esc(st.error) + "</p></section>";
      if (!st.items.length) return '<section class="xpb-card">' + head + '<p class="xpb-empty">暂无备份记录' + (canManage ? "，可点击「立即备份」创建第一份备份" : "") + "</p></section>";
      var pages = Math.max(1, Math.ceil(st.total / PAGE_SIZE));
      return '<section class="xpb-card">' + head +
        '<table class="xpb-table"><thead><tr><th>类型</th><th>开始时间</th><th>耗时</th><th>大小</th><th>状态</th><th>SHA-256</th><th style="width:110px">操作</th></tr></thead><tbody>' +
        st.items.map(rowHTML).join("") + "</tbody></table>" +
        '<div class="xpb-pager"><span>共 ' + esc(st.total) + " 条</span>" +
        '<button type="button" class="x-btn" data-pbk-click="prev"' + (st.page <= 1 ? " disabled" : "") + ">上一页</button>" +
        "<span>" + st.page + " / " + pages + "</span>" +
        '<button type="button" class="x-btn" data-pbk-click="next"' + (st.page >= pages ? " disabled" : "") + ">下一页</button></div></section>";
    }

    function restoreHTML() {
      var m = st.restore;
      if (!m) return "";
      var r = m.rec;
      return '<div class="xpb-mask" data-pbk-mask><div class="xpb-modal" role="dialog" aria-modal="true"><h3>从备份恢复平台数据</h3>' +
        '<div class="xpb-warn">恢复会用这份备份<strong>覆盖平台当前的全部数据</strong>（用户、权限、配置、审计记录等），备份之后产生的数据将丢失，恢复期间平台不可用，此操作不可撤销。</div>' +
        '<div class="xpb-row"><span>备份</span><div>' + esc(KIND[r.kind] || r.kind) + " · " + esc(fmtTime(r.created_at)) + " · " + esc(fmtSize(r.size)) + "</div></div>" +
        '<label for="xpb-confirm">请输入「恢复」二字以确认</label><input id="xpb-confirm" class="x-input" name="confirm" autocomplete="off" data-pbk-confirm value="' + esc(m.text || "") + '">' +
        (m.error ? '<p class="xpb-err" data-pbk-restore-error>' + esc(m.error) + "</p>" : "") +
        '<div class="xpb-foot"><button type="button" class="x-btn" data-pbk-click="cancelRestore"' + (m.busy ? " disabled" : "") + '>取消</button>' +
        '<button type="button" class="x-btn x-btn--danger" data-pbk-click="confirmRestore"' + (m.busy ? " disabled" : "") + ">" + (m.busy ? "正在提交…" : "确认恢复") + "</button></div></div></div>";
    }

    function paint() {
      if (!alive()) return;
      el.innerHTML = '<div class="xpb" data-pbk>' + policyHTML() + listHTML() + restoreHTML() + "</div>";
    }

    function find(id) { return st.items.filter(function (r) { return String(r.id) === String(id); })[0]; }

    var actions = {
      reload: function () { st.policy = null; st.policyMissing = false; return load(); },
      prev: function () { if (st.page > 1) { st.page--; return load(); } },
      next: function () { st.page++; return load(); },
      create: function () {
        if (!canManage || st.creating) return;
        st.creating = true; paint();
        return api("/platform/backups", { method: "POST" }).then(function () {
          if (!alive()) return;
          st.creating = false; st.page = 1; toast("备份已开始，完成后可在列表中下载");
          return load(true);
        }, function (e) {
          if (!alive()) return;
          st.creating = false; paint(); toast("发起备份失败：" + errMsg(e));
        });
      },
      download: function (t) {
        var rec = find(t.getAttribute("data-id"));
        if (!rec || st.downloading[rec.id]) return;
        st.downloading[rec.id] = true; paint();
        return download(rec).then(function (v) {
          toast(v === "ok" ? "下载完成，文件校验通过" : "下载完成");
        }, function (e) { toast(errMsg(e)); }).then(function () {
          delete st.downloading[rec.id]; paint();
        });
      },
      restore: function (t) {
        if (!(st.policy && st.policy.online_restore) || !canManage) return;
        var rec = find(t.getAttribute("data-id"));
        if (!rec || rec.status !== "succeeded") return;
        st.restore = { rec: rec, text: "", error: "", busy: false }; paint();
      },
      cancelRestore: function () { if (st.restore && !st.restore.busy) { st.restore = null; paint(); } },
      confirmRestore: function () {
        var m = st.restore;
        if (!m || m.busy) return;
        var input = el.querySelector("[data-pbk-confirm]");
        m.text = input ? String(input.value || "").trim() : "";
        if (m.text !== "恢复") { m.error = "请输入「恢复」二字确认"; paint(); return; }
        m.busy = true; m.error = ""; paint();
        return api("/platform/backups/" + encodeURIComponent(m.rec.id) + "/restore", { method: "POST", body: { confirm: "恢复" } }).then(function () {
          if (!alive()) return;
          st.restore = null; toast("恢复任务已提交，平台将在恢复完成后重新可用"); return load(true);
        }, function (e) {
          if (!alive()) return;
          m.busy = false; m.error = "恢复失败：" + errMsg(e); paint();
        });
      }
    };

    if (el._xsPbkClick) el.removeEventListener("click", el._xsPbkClick);
    el._xsPbkClick = function (ev) {
      if (!alive()) return;
      var t = ev.target && ev.target.closest ? ev.target.closest("[data-pbk-click]") : null;
      if (!t || t.disabled) return;
      var fn = actions[t.getAttribute("data-pbk-click")];
      if (fn) return fn(t);
    };
    el.addEventListener("click", el._xsPbkClick);
    load();
  }

  root.XSPlatformBackup = { render: render, describeCron: describeCron, fmtSize: fmtSize, _save: null };
})(window);
