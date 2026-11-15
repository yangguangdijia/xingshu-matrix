/* 星枢 Matrix — 审计日志：完整性校验 / 导出 / 归档
 * 依赖 window.XS（xs-api.js）。挂载到 window.XSAuditChain。
 *
 * 后端接口（server/internal/audit/verify_api.go、export.go）：
 *   GET  /audit/chain/verify?from_id=&limit=   → {checked, ok, break_at, kind, head_id, head_hash, last_id}   权限 audit:read
 *   GET  /audit/export?from=&to=&format=jsonl|csv → 文件流（含 prev_hash/hash，可离线校验）            权限 audit:export
 *   POST /audit/archive {from, to}              → {file, storage, location, rows, first_id, last_id, size, sha256}  权限 audit:manage
 *
 * 用法：
 *   XSAuditChain.render(el)  在 el 内渲染「审计完整性」工具条（按权限显示按钮）
 *   XSAuditChain.attach()    实时模式下自动挂到「运维审计」页（#/audit）的记录列表上方
 */
(function (root) {
  "use strict";

  var VERIFY_LIMIT = 50000;
  var VERIFY_MAX_ROUNDS = 40; // 最多 200 万条，超出后提示继续
  var MAX_DAYS = 366;
  var INTERRUPTED = "export interrupted";

  var KIND_TEXT = {
    hash_mismatch: "这条记录的内容与写入时不一致，可能被修改过",
    prev_mismatch: "这条记录之前的记录缺失、被删除或顺序被改动",
    unchained: "这条记录尚未纳入防篡改保护"
  };
  var SERVER_MSG = [
    [/another audit archive is running/i, "已有归档任务在进行，请稍后再试"],
    [/no audit records in this time range/i, "该时间段内没有审计记录"],
    [/range must not exceed/i, "时间段不能超过 " + MAX_DAYS + " 天"],
    [/to must be after from/i, "结束时间需晚于开始时间"],
    [/(from|to) is required|invalid time/i, "请填写正确的开始与结束时间"],
    [/format must be/i, "请选择导出格式"],
    [/permission denied/i, "没有权限执行此操作，请联系管理员授权"],
    [/authentication required/i, "登录已失效，请重新登录"]
  ];

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function XS() {
    if (!root.XS || typeof root.XS.api !== "function") throw new Error("XS API 客户端未加载");
    return root.XS;
  }
  function api(path, options) {
    try { return Promise.resolve(XS().api(path, options || {})); } catch (e) { return Promise.reject(e); }
  }
  function can(perm) {
    var x = root.XS;
    if (!x || typeof x.can !== "function") return false;
    try { return !!x.can(perm); } catch (e) { return false; }
  }
  function toast(m) { if (root.XS && typeof root.XS.toast === "function") root.XS.toast(m); }
  function errMsg(e, fallback) {
    var m = (e && e.message) || "";
    for (var i = 0; i < SERVER_MSG.length; i++) if (SERVER_MSG[i][0].test(m)) return SERVER_MSG[i][1];
    if (e && (e.status === 403 || e.code === 40300)) return "没有权限执行此操作，请联系管理员授权";
    if (/[\u4e00-\u9fa5]/.test(m)) return m;
    return fallback || "操作失败，请稍后重试";
  }
  function pad(n) { return n < 10 ? "0" + n : "" + n; }
  function localInput(d) {
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + "T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }
  function fmtDate(d) {
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }
  function num(n) { return String(n == null ? 0 : n).replace(/\B(?=(\d{3})+(?!\d))/g, ","); }
  function size(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + " B";
    if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
    return (n / 1048576).toFixed(1) + " MB";
  }

  // ---------------------------------------------------------------- 时间段
  function defaultRange(kind, now) {
    now = now || new Date();
    if (kind === "archive") { // 上一个自然月
      var s = new Date(now.getFullYear(), now.getMonth() - 1, 1, 0, 0);
      var e = new Date(now.getFullYear(), now.getMonth(), 1, 0, 0);
      return { from: localInput(s), to: localInput(e) };
    }
    var end = new Date(now.getTime());
    end.setSeconds(0, 0);
    end = new Date(end.getTime() + 60000);
    return { from: localInput(new Date(end.getTime() - 7 * 86400000)), to: localInput(end) };
  }
  // 校验表单里的时间段；返回 {from, to, fromDate, toDate} 或 {error, field}
  function parseRange(fromV, toV) {
    fromV = String(fromV || "").trim(); toV = String(toV || "").trim();
    if (!fromV) return { error: "请选择开始时间", field: "from" };
    if (!toV) return { error: "请选择结束时间", field: "to" };
    var f = new Date(fromV), t = new Date(toV);
    if (isNaN(f.getTime())) return { error: "开始时间格式不正确", field: "from" };
    if (isNaN(t.getTime())) return { error: "结束时间格式不正确", field: "to" };
    if (t.getTime() <= f.getTime()) return { error: "结束时间需晚于开始时间", field: "to" };
    if (t.getTime() - f.getTime() > MAX_DAYS * 86400000) return { error: "时间段不能超过 " + MAX_DAYS + " 天", field: "to" };
    return { from: f.toISOString(), to: t.toISOString(), fromDate: f, toDate: t };
  }

  // ---------------------------------------------------------------- 完整性校验
  // 分批调用校验接口，直到到达最新记录、发现断点或达到轮数上限。
  function verifyAll(onProgress) {
    var total = 0, rounds = 0;
    function step(fromID) {
      var q = { limit: VERIFY_LIMIT };
      if (fromID) q.from_id = fromID;
      return api("/audit/chain/verify", { query: q, timeoutMs: 120000 }).then(function (r) {
        r = r || {};
        rounds++;
        total += Number(r.checked) || 0;
        var res = { ok: r.ok !== false, checked: total, breakAt: r.break_at || 0, kind: r.kind || "", headID: r.head_id || 0, lastID: r.last_id || 0, partial: false };
        if (!res.ok) return res;
        var more = (Number(r.checked) || 0) >= VERIFY_LIMIT && res.lastID && res.lastID < res.headID;
        if (!more) return res;
        if (rounds >= VERIFY_MAX_ROUNDS) { res.partial = true; return res; }
        if (typeof onProgress === "function") onProgress(total);
        return step(res.lastID + 1);
      });
    }
    return step(0);
  }

  function verifyHTML(r) {
    if (!r.ok) {
      return '<div class="xac-res xac-res--bad" data-xac-verdict="broken"><b>发现断点</b>' +
        "<p>记录编号 <b>#" + esc(r.breakAt) + "</b>：" + esc(KIND_TEXT[r.kind] || "校验未通过") + "。</p>" +
        "<p class=\"xac-muted\">在此之前已有 " + esc(num(r.checked)) + " 条记录校验通过。请保留现场并联系安全管理员核查。</p></div>";
    }
    if (!r.checked && !r.headID) {
      return '<div class="xac-res" data-xac-verdict="empty"><b>暂无可校验的记录</b><p class="xac-muted">开启防篡改保护后新产生的审计记录会纳入校验。</p></div>';
    }
    var tail = r.partial
      ? "<p class=\"xac-muted\">记录较多，本次已检查到 #" + esc(r.lastID) + "，再次点击「完整性校验」不会从断点继续，可在空闲时段重试完整校验。</p>"
      : "<p class=\"xac-muted\">已检查到最新记录 #" + esc(r.headID) + "。</p>";
    return '<div class="xac-res xac-res--ok" data-xac-verdict="' + (r.partial ? "partial" : "ok") + '"><b>' + (r.partial ? "已校验部分记录，未发现篡改" : "校验通过") + "</b>" +
      "<p>共检查 " + esc(num(r.checked)) + " 条审计记录，内容与顺序均未被改动。</p>" + tail + "</div>";
  }

  // ---------------------------------------------------------------- 导出下载
  var IO = {
    fetch: function (url, init) { return root.fetch(url, init); },
    save: function (blob, filename) {
      var doc = root.document;
      if (!doc || !root.URL || typeof root.URL.createObjectURL !== "function") throw new Error("当前浏览器不支持下载");
      var a = doc.createElement("a");
      var href = root.URL.createObjectURL(blob);
      a.href = href; a.download = filename;
      doc.body.appendChild(a); a.click(); a.remove();
      setTimeout(function () { try { root.URL.revokeObjectURL(href); } catch (e) {} }, 30000);
    }
  };

  function exportURL(q) {
    var base = (root.XS && root.XS.base) || "/api/v1";
    var qs = Object.keys(q).map(function (k) { return encodeURIComponent(k) + "=" + encodeURIComponent(q[k]); }).join("&");
    return base + "/audit/export?" + qs;
  }
  function fileName(res, q) {
    var cd = res.headers && typeof res.headers.get === "function" ? res.headers.get("content-disposition") || "" : "";
    var m = /filename="?([^";]+)"?/.exec(cd);
    return m ? m[1] : "audit-export." + q.format;
  }
  function readError(res) {
    return Promise.resolve(res.json ? res.json().catch(function () { return null; }) : null).then(function (p) {
      var e = new Error((p && p.message) || ("HTTP " + res.status));
      e.status = res.status; e.code = p && p.code;
      throw e;
    });
  }
  // 下载导出文件；返回 {filename, bytes, complete}
  function downloadExport(q) {
    function once(retried) {
      var headers = { "Accept": "*/*" };
      var tok = root.XS && typeof root.XS.token === "function" ? root.XS.token() : "";
      if (tok) headers.Authorization = "Bearer " + tok;
      return Promise.resolve(IO.fetch(exportURL(q), { headers: headers, credentials: "same-origin" })).then(function (res) {
        if (res.status === 401 && !retried && root.XS && typeof root.XS.refresh === "function") {
          return root.XS.refresh().then(function () { return once(true); });
        }
        if (!res.ok) return readError(res);
        return res.blob().then(function (blob) {
          var name = fileName(res, q);
          var tailBlob = blob.size > 256 ? blob.slice(blob.size - 256) : blob;
          return Promise.resolve(typeof tailBlob.text === "function" ? tailBlob.text() : "").then(function (tail) {
            IO.save(blob, name);
            return { filename: name, bytes: blob.size, complete: String(tail).indexOf(INTERRUPTED) < 0 };
          });
        });
      }, function (err) {
        var e = new Error("无法连接服务，请检查网络后重试");
        e.cause = err;
        throw e;
      });
    }
    return once(false);
  }

  // ---------------------------------------------------------------- 样式
  var CSS = [
    ".xac{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:12px 16px;margin-bottom:16px;font-size:14px;color:#1f2937}",
    ".xac-bar{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.xac-bar>b{font-weight:600;margin-right:4px}",
    ".xac-bar .xac-muted{margin-right:auto}",
    ".xac-muted{color:#6b7280;font-size:13px;margin:4px 0 0}",
    ".xac-btn{height:32px;padding:0 14px;border:1px solid #d1d5db;border-radius:6px;background:#fff;color:#1f2937;cursor:pointer;font-size:13px}",
    ".xac-btn:hover{border-color:#2563eb;color:#2563eb}.xac-btn[disabled]{opacity:.5;cursor:not-allowed}",
    ".xac-btn--pri{background:#2563eb;border-color:#2563eb;color:#fff}.xac-btn--pri:hover{background:#1d4ed8;color:#fff}",
    ".xac-form{margin-top:12px;padding-top:12px;border-top:1px solid #f0f1f3;display:flex;flex-wrap:wrap;gap:12px;align-items:flex-end}",
    ".xac-form label{display:flex;flex-direction:column;gap:6px;font-size:13px;color:#374151}",
    ".xac-input{height:32px;padding:0 10px;border:1px solid #d1d5db;border-radius:6px;font-size:14px;background:#fff}",
    ".xac-input:focus{outline:none;border-color:#2563eb;box-shadow:0 0 0 3px rgba(37,99,235,.12)}.xac-input--bad{border-color:#dc2626}",
    ".xac-err{color:#dc2626;font-size:13px;margin:6px 0 0;flex-basis:100%}",
    ".xac-res{margin-top:12px;padding:10px 12px;border-radius:6px;border:1px solid #e5e7eb;background:#f8fafc}.xac-res p{margin:4px 0 0}",
    ".xac-res--ok{border-color:#bbf7d0;background:#f0fdf4}.xac-res--ok>b{color:#15803d}",
    ".xac-res--bad{border-color:#fecaca;background:#fef2f2}.xac-res--bad>b{color:#b91c1c}",
    ".xac-confirm{flex-basis:100%;background:#fff7ed;border:1px solid #fed7aa;color:#9a3412;border-radius:6px;padding:8px 10px;font-size:13px}",
    ".xac-mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;word-break:break-all}"
  ].join("\n");
  function injectCSS() {
    var doc = root.document;
    if (!doc || !doc.head || doc.getElementById("xs-audit-chain-css")) return;
    var s = doc.createElement("style");
    s.id = "xs-audit-chain-css";
    s.textContent = CSS;
    doc.head.appendChild(s);
  }

  // ---------------------------------------------------------------- 渲染
  function perms() {
    return { verify: can("audit:read"), export: can("audit:export"), archive: can("audit:manage") };
  }

  function rangeFields(kind, r) {
    return '<label>开始时间<input class="xac-input" type="datetime-local" name="from" value="' + esc(r.from) + '" /></label>' +
      '<label>结束时间<input class="xac-input" type="datetime-local" name="to" value="' + esc(r.to) + '" /></label>';
  }
  function exportFormHTML(r) {
    return '<form class="xac-form" data-xac-form="export" novalidate>' + rangeFields("export", r) +
      '<label>文件格式<select class="xac-input" name="format">' +
      '<option value="jsonl">逐行数据文件 .jsonl（可离线校验）</option>' +
      '<option value="csv">表格文件 .csv（便于查看）</option></select></label>' +
      '<button class="xac-btn xac-btn--pri" type="button" data-xac="export-submit">开始导出</button>' +
      '<button class="xac-btn" type="button" data-xac="cancel">取消</button>' +
      '<p class="xac-err" data-xac-err hidden></p>' +
      '<p class="xac-muted" style="flex-basis:100%">导出内容包含每条记录的防篡改校验信息；表格文件中以 = + - @ 开头的内容会加单引号，需离线核对时请选逐行数据文件。</p></form>';
  }
  function archiveFormHTML(r) {
    return '<form class="xac-form" data-xac-form="archive" novalidate>' + rangeFields("archive", r) +
      '<button class="xac-btn xac-btn--pri" type="button" data-xac="archive-submit">归档</button>' +
      '<button class="xac-btn" type="button" data-xac="cancel">取消</button>' +
      '<p class="xac-err" data-xac-err hidden></p>' +
      '<p class="xac-muted" style="flex-basis:100%">把所选时间段的审计记录压缩打包，保存到平台的归档存储（已配置对象存储时存入对象存储，否则保存在服务器归档目录）。归档不会删除现有记录。</p></form>';
  }

  function render(host) {
    if (!host) return null;
    var p = perms();
    if (!p.verify && !p.export && !p.archive) { host.innerHTML = ""; return null; }
    injectCSS();
    var st = { busy: "", form: "" };
    host.innerHTML = '<section class="xac" data-xac-root>' +
      '<div class="xac-bar"><b>审计完整性</b><span class="xac-muted">审计记录写入后即被串联保护，可随时核对是否被改动或删除</span>' +
      (p.verify ? '<button class="xac-btn" type="button" data-xac="verify">完整性校验</button>' : "") +
      (p.export ? '<button class="xac-btn" type="button" data-xac="export">导出</button>' : "") +
      (p.archive ? '<button class="xac-btn" type="button" data-xac="archive">归档</button>' : "") +
      '</div><div data-xac-panel></div><div data-xac-result></div></section>';
    var panel = host.querySelector("[data-xac-panel]");
    var result = host.querySelector("[data-xac-result]");

    function btn(name) { return host.querySelector('[data-xac="' + name + '"]'); }
    function setBusy(name, label) {
      st.busy = name;
      ["verify", "export", "archive", "export-submit", "archive-submit"].forEach(function (n) { var b = btn(n); if (b) b.disabled = !!name; });
      var b = btn(name);
      if (b && label) { b.setAttribute("data-label", b.textContent); b.textContent = label; }
    }
    function clearBusy() {
      var b = st.busy && btn(st.busy);
      if (b && b.getAttribute("data-label")) { b.textContent = b.getAttribute("data-label"); }
      st.busy = "";
      ["verify", "export", "archive", "export-submit", "archive-submit"].forEach(function (n) { var x = btn(n); if (x) x.disabled = false; });
    }
    function form() { return panel.querySelector("[data-xac-form]"); }
    function fieldErr(msg, field) {
      var f = form(); if (!f) return;
      var e = f.querySelector("[data-xac-err]");
      f.querySelectorAll("input").forEach(function (i) { i.className = "xac-input"; });
      if (e) { e.textContent = msg || ""; e.hidden = !msg; if (msg) e.removeAttribute("hidden"); else e.setAttribute("hidden", ""); }
      if (field) { var i = f.querySelector('[name="' + field + '"]'); if (i) { i.className = "xac-input xac-input--bad"; if (i.focus) i.focus(); } }
    }
    function readRange() {
      var f = form();
      var get = function (n) { var i = f && f.querySelector('[name="' + n + '"]'); return i ? i.value : ""; };
      return { range: parseRange(get("from"), get("to")), format: get("format") || "jsonl" };
    }
    function openForm(kind) {
      if (st.form === kind) { st.form = ""; panel.innerHTML = ""; return; }
      st.form = kind;
      var r = defaultRange(kind);
      panel.innerHTML = kind === "export" ? exportFormHTML(r) : archiveFormHTML(r);
    }
    function closeForm() { st.form = ""; panel.innerHTML = ""; }

    function doVerify() {
      if (st.busy) return Promise.resolve();
      setBusy("verify", "校验中…");
      result.innerHTML = '<div class="xac-res" data-xac-verdict="running"><p>正在校验审计记录，记录较多时需要一些时间…</p></div>';
      return verifyAll(function (n) {
        result.innerHTML = '<div class="xac-res" data-xac-verdict="running"><p>正在校验，已检查 ' + esc(num(n)) + " 条…</p></div>";
      }).then(function (r) {
        result.innerHTML = verifyHTML(r);
      }).catch(function (e) {
        result.innerHTML = '<div class="xac-res xac-res--bad" data-xac-verdict="error"><b>校验未完成</b><p>' + esc(errMsg(e, "校验失败，请稍后重试")) + "</p></div>";
      }).then(clearBusy);
    }

    function doExport() {
      if (st.busy) return Promise.resolve();
      var x = readRange();
      if (x.range.error) { fieldErr(x.range.error, x.range.field); return Promise.resolve(); }
      fieldErr("");
      var fmt = x.format === "csv" ? "csv" : "jsonl";
      setBusy("export-submit", "导出中…");
      return downloadExport({ from: x.range.from, to: x.range.to, format: fmt }).then(function (r) {
        var text = "已导出 " + fmtDate(x.range.fromDate) + " 至 " + fmtDate(x.range.toDate) + " 的审计记录（" + size(r.bytes) + "）";
        if (!r.complete) {
          result.innerHTML = '<div class="xac-res xac-res--bad" data-xac-verdict="export-incomplete"><b>导出不完整</b><p>文件 ' + esc(r.filename) + " 在导出过程中中断，内容不完整，请重新导出。</p></div>";
        } else {
          result.innerHTML = '<div class="xac-res xac-res--ok" data-xac-verdict="exported"><b>导出完成</b><p>' + esc(text) + "，文件名 " + esc(r.filename) + "。</p>" +
            (r.bytes ? "" : '<p class="xac-muted">该时间段内没有审计记录。</p>') + "</div>";
          closeForm();
        }
        toast(r.complete ? "导出完成" : "导出不完整，请重试");
      }).catch(function (e) {
        fieldErr(errMsg(e, "导出失败，请稍后重试"));
      }).then(clearBusy);
    }

    function askArchive() {
      if (st.busy) return;
      var x = readRange();
      if (x.range.error) { fieldErr(x.range.error, x.range.field); return; }
      fieldErr("");
      var f = form(); if (!f) return;
      var old = f.querySelector("[data-xac-confirm]"); if (old) old.remove();
      f.insertAdjacentHTML("beforeend", '<div class="xac-confirm" data-xac-confirm>将把 <b>' + esc(fmtDate(x.range.fromDate)) + "</b> 至 <b>" + esc(fmtDate(x.range.toDate)) +
        "</b> 的审计记录打包保存到归档存储，现有记录不会被删除。确认归档？ " +
        '<button class="xac-btn xac-btn--pri" type="button" data-xac="archive-confirm">确认归档</button> ' +
        '<button class="xac-btn" type="button" data-xac="archive-back">再想想</button></div>');
    }
    function doArchive() {
      if (st.busy) return Promise.resolve();
      var x = readRange();
      if (x.range.error) { fieldErr(x.range.error, x.range.field); return Promise.resolve(); }
      setBusy("archive-confirm", "归档中…");
      return api("/audit/archive", { method: "POST", body: { from: x.range.from, to: x.range.to }, timeoutMs: 600000 }).then(function (r) {
        r = r || {};
        var where = r.storage === "s3" ? "对象存储" : "服务器归档目录";
        result.innerHTML = '<div class="xac-res xac-res--ok" data-xac-verdict="archived"><b>归档完成</b>' +
          "<p>已归档 " + esc(num(r.rows)) + " 条记录（编号 #" + esc(r.first_id) + " – #" + esc(r.last_id) + "，" + esc(size(r.size)) + "），保存在" + esc(where) + "。</p>" +
          '<p class="xac-muted">文件名：<span class="xac-mono">' + esc(r.file) + "</span></p>" +
          (r.sha256 ? '<p class="xac-muted">文件指纹：<span class="xac-mono">' + esc(r.sha256) + "</span></p>" : "") + "</div>";
        closeForm();
        toast("归档完成");
      }).catch(function (e) {
        if (root.XS && typeof root.XS.isApproval === "function" && root.XS.isApproval(e)) {
          result.innerHTML = '<div class="xac-res" data-xac-verdict="approval"><b>已提交审批</b><p>审批通过后将自动执行归档。</p></div>';
          closeForm();
          return;
        }
        var c = panel.querySelector("[data-xac-confirm]"); if (c) c.remove();
        fieldErr(errMsg(e, "归档失败，请稍后重试"));
      }).then(clearBusy);
    }

    function onClick(ev) {
      var t = ev && ev.target;
      var b = t && typeof t.closest === "function" ? t.closest("[data-xac]") : null;
      if (!b || !host.contains(b) || b.disabled) return;
      var a = b.getAttribute("data-xac");
      if (ev.preventDefault) ev.preventDefault();
      switch (a) {
        case "verify": return doVerify();
        case "export": case "archive": return openForm(a);
        case "cancel": return closeForm();
        case "export-submit": return doExport();
        case "archive-submit": return askArchive();
        case "archive-back": { var c = panel.querySelector("[data-xac-confirm]"); if (c) c.remove(); return; }
        case "archive-confirm": return doArchive();
      }
    }
    if (host.__xacClick) host.removeEventListener("click", host.__xacClick);
    host.__xacClick = onClick;
    host.addEventListener("click", onClick);
    return { verify: doVerify, exportNow: doExport, archiveNow: doArchive };
  }

  // ---------------------------------------------------------------- 自动挂到实时审计页
  var attached = false;
  function isLive() { return !!(root.XS && root.XS.mode === "live"); }
  function routeKey() {
    var h = (root.location && root.location.hash) || "";
    return h.replace(/^#\/?/, "").split(/[/?]/)[0];
  }
  function inject() {
    var doc = root.document;
    if (!doc || !isLive() || routeKey() !== "audit") return false;
    var body = doc.querySelector("[data-ops-body]");
    var page = body && body.parentNode;
    if (!page || !page.querySelector("[data-export]") || page.querySelector("[data-xac-host]")) return false;
    var p = perms();
    if (!p.verify && !p.export && !p.archive) return false;
    var host = doc.createElement("div");
    host.setAttribute("data-xac-host", "");
    page.insertBefore(host, body);
    render(host);
    return true;
  }
  function attach() {
    var doc = root.document;
    if (attached || !doc) return;
    attached = true;
    var pending = false;
    function schedule() {
      if (pending) return;
      pending = true;
      setTimeout(function () { pending = false; try { inject(); } catch (e) {} }, 50);
    }
    if (typeof root.MutationObserver === "function") {
      try { new root.MutationObserver(schedule).observe(doc.body || doc.documentElement, { childList: true, subtree: true }); } catch (e) {}
    }
    if (typeof root.addEventListener === "function") root.addEventListener("hashchange", schedule);
    schedule();
  }

  root.XSAuditChain = {
    render: render,
    attach: attach,
    inject: inject,
    verifyAll: verifyAll,
    verifyHTML: verifyHTML,
    parseRange: parseRange,
    defaultRange: defaultRange,
    downloadExport: downloadExport,
    _io: IO
  };

  if (root.document) {
    if (root.document.readyState === "loading" && typeof root.addEventListener === "function") root.addEventListener("DOMContentLoaded", attach);
    else attach();
  }
})(window);
