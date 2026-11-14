/*
 * 星枢 Matrix 堡垒机：AI 辅助起草命令过滤规则（对齐 JumpServer 5.1）。
 * 用法：window.XSBastionAIDraft.open({ onApply: function (draft) { ... } })
 * 抽屉表单填写描述与「应拦截 / 应放行」样例，调用 POST /bastion/command-filters/ai-draft，
 * 展示后端校验结果与样例预览；点「采用」把草稿交给调用方（通常回填新建规则表单），本组件不落库。
 */
(function () {
  "use strict";
  var API = "/bastion/command-filters/ai-draft";
  var ACTIONS = { deny: "拒绝", confirm: "确认", approve: "审批后执行", allow: "允许" };
  var STYLE_ID = "xs-bastion-aidraft-style";
  var CSS = [
    ".xsad-mask{position:fixed;inset:0;background:rgba(15,23,42,.18);z-index:1000;display:flex;justify-content:flex-end}",
    ".xsad-drawer{width:560px;max-width:100%;height:100%;background:#fff;color:#1f2937;box-shadow:-8px 0 24px rgba(15,23,42,.12);display:flex;flex-direction:column;font-size:14px}",
    ".xsad-head{display:flex;align-items:center;justify-content:space-between;padding:16px 20px;border-bottom:1px solid #e5e7eb}",
    ".xsad-head h3{margin:0;font-size:16px;font-weight:600}",
    ".xsad-body{flex:1;overflow:auto;padding:16px 20px}",
    ".xsad-body label{display:block;margin-bottom:12px}",
    ".xsad-body label span{display:block;margin-bottom:4px;color:#4b5563}",
    ".xsad-body textarea{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:6px;padding:8px;font:inherit;background:#fff;color:inherit;resize:vertical}",
    ".xsad-mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}",
    ".xsad-foot{display:flex;gap:8px;justify-content:flex-end;padding:12px 20px;border-top:1px solid #e5e7eb;background:#f9fafb}",
    ".xsad-btn{border:1px solid #d1d5db;background:#fff;color:#1f2937;border-radius:6px;padding:6px 14px;cursor:pointer}",
    ".xsad-btn--pri{background:#2563eb;border-color:#2563eb;color:#fff}",
    ".xsad-btn[disabled]{opacity:.55;cursor:not-allowed}",
    ".xsad-status{min-height:20px;color:#6b7280;margin:4px 0 8px}",
    ".xsad-status.is-err{color:#dc2626}",
    ".xsad-rule{border:1px solid #e5e7eb;border-radius:8px;padding:12px;background:#f9fafb;margin-bottom:12px}",
    ".xsad-rule dl{display:grid;grid-template-columns:72px 1fr;gap:6px 8px;margin:0}",
    ".xsad-rule dt{color:#6b7280}.xsad-rule dd{margin:0;word-break:break-all}",
    ".xsad-errs{color:#dc2626;margin:8px 0 0;padding-left:18px}",
    ".xsad-ok{color:#16a34a}.xsad-bad{color:#dc2626}",
    ".xsad-table{width:100%;border-collapse:collapse}",
    ".xsad-table th,.xsad-table td{border-bottom:1px solid #e5e7eb;padding:6px 8px;text-align:left;vertical-align:top}",
    ".xsad-table th{color:#6b7280;font-weight:500;background:#f9fafb}"
  ].join("\n");

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function lines(v) {
    return String(v || "").split(/\r?\n/).map(function (s) { return s.trim(); }).filter(Boolean);
  }
  function errText(e) {
    if (!e) return "未知错误";
    if (e.status === 503 || e.code === 50300) return e.message || "未配置大模型";
    return e.message || String(e);
  }
  function ensureStyle(doc) {
    if (!doc.head || (doc.getElementById && doc.getElementById(STYLE_ID))) return;
    var s = doc.createElement("style");
    s.id = STYLE_ID;
    s.textContent = CSS;
    doc.head.appendChild(s);
  }

  function ruleHTML(res) {
    var d = res.draft || {};
    var head = res.valid
      ? '<b class="xsad-ok">✓ 草稿校验通过</b>'
      : '<b class="xsad-bad">✗ 草稿未通过校验</b>';
    var errs = (res.errors || []).length
      ? '<ul class="xsad-errs" data-xsad-errors>' + res.errors.map(function (e) { return "<li>" + esc(e) + "</li>"; }).join("") + "</ul>"
      : "";
    return '<div class="xsad-rule" data-xsad-rule>' + head +
      "<dl><dt>名称</dt><dd>" + esc(d.name) + "</dd>" +
      "<dt>匹配方式</dt><dd>" + esc(d.match_type === "regex" ? "正则（RE2）" : d.match_type) + "</dd>" +
      '<dt>表达式</dt><dd><code class="xsad-mono">' + esc(d.pattern) + "</code></dd>" +
      "<dt>动作</dt><dd>" + esc(ACTIONS[d.action] || d.action) + "</dd>" +
      "<dt>说明</dt><dd>" + esc(d.reason) + "</dd></dl>" + errs + "</div>";
  }
  function previewHTML(res) {
    var rows = res.preview || [];
    if (!rows.length) return '<p class="xsad-status">没有可预览的样例。</p>';
    return '<table class="xsad-table" data-xsad-preview><thead><tr><th>样例命令</th><th>预期</th><th>是否命中</th><th>结果</th></tr></thead><tbody>' +
      rows.map(function (p) {
        var pass = p.pass !== undefined ? p.pass : (p.expected === "block") === !!p.matched;
        return '<tr data-xsad-row="' + (pass ? "pass" : "fail") + '"><td><code class="xsad-mono">' + esc(p.command) + "</code></td>" +
          "<td>" + (p.expected === "block" ? "应拦截" : "应放行") + "</td>" +
          "<td>" + (p.matched ? "命中" : "未命中") + "</td>" +
          "<td>" + (pass ? '<span class="xsad-ok">✓</span>' : '<span class="xsad-bad">✗</span>') + "</td></tr>";
      }).join("") + "</tbody></table>";
  }

  function open(opts) {
    opts = opts || {};
    var doc = window.document;
    var XS = window.XS;
    ensureStyle(doc);
    var root = doc.createElement("div");
    root.className = "xsad-mask";
    root.innerHTML =
      '<aside class="xsad-drawer" role="dialog" aria-label="AI 起草命令过滤规则">' +
      '<div class="xsad-head"><h3>AI 起草命令过滤规则</h3><button type="button" class="xsad-btn" data-xsad-close>关闭</button></div>' +
      '<div class="xsad-body">' +
      '<label><span>规则描述</span><textarea data-xsad-desc rows="3" maxlength="2000" placeholder="例如：禁止删除根目录或整盘格式化"></textarea></label>' +
      '<label><span>应拦截的命令样例（每行一条）</span><textarea class="xsad-mono" data-xsad-block rows="4" placeholder="rm -rf /"></textarea></label>' +
      '<label><span>应放行的命令样例（每行一条）</span><textarea class="xsad-mono" data-xsad-allow rows="4" placeholder="rm -rf ./tmp"></textarea></label>' +
      '<div class="xsad-status" data-xsad-status role="status"></div>' +
      '<div data-xsad-result></div>' +
      "</div>" +
      '<div class="xsad-foot"><button type="button" class="xsad-btn" data-xsad-cancel>取消</button>' +
      '<button type="button" class="xsad-btn" data-xsad-generate>生成草稿</button>' +
      '<button type="button" class="xsad-btn xsad-btn--pri" data-xsad-apply disabled>采用</button></div>' +
      "</aside>";
    doc.body.appendChild(root);

    var q = function (sel) { return root.querySelector(sel); };
    var desc = q("[data-xsad-desc]"), block = q("[data-xsad-block]"), allow = q("[data-xsad-allow]");
    var status = q("[data-xsad-status]"), result = q("[data-xsad-result]");
    var gen = q("[data-xsad-generate]"), apply = q("[data-xsad-apply]");
    var current = null, closed = false, seq = 0;

    function setStatus(text, isErr) {
      status.textContent = text || "";
      status.className = "xsad-status" + (isErr ? " is-err" : "");
    }
    function close() {
      if (closed) return;
      closed = true;
      seq++;
      if (doc.removeEventListener) doc.removeEventListener("keydown", onKey);
      root.remove();
    }
    function onKey(e) { if (e && e.key === "Escape") close(); }

    function generate() {
      var description = String(desc.value || "").trim();
      if (!description) { setStatus("请先填写规则描述", true); desc.focus(); return Promise.resolve(); }
      var body = { description: description, samples_block: lines(block.value), samples_allow: lines(allow.value) };
      var my = ++seq;
      current = null;
      apply.disabled = true;
      gen.disabled = true;
      gen.textContent = "生成中…";
      result.innerHTML = "";
      setStatus("正在调用大模型生成草稿…");
      return XS.api(API, { method: "POST", body: body, timeoutMs: 60000 }).then(function (res) {
        if (my !== seq) return;
        res = res || {};
        current = res;
        result.innerHTML = ruleHTML(res) + previewHTML(res);
        apply.disabled = !res.draft;
        setStatus(res.valid ? "草稿可用，请检查后点「采用」回填表单。" : "草稿存在问题，可调整描述或样例后重新生成，也可采用后手动修改。", !res.valid);
      }).catch(function (e) {
        if (my !== seq) return;
        setStatus("生成失败：" + errText(e), true);
      }).then(function () {
        if (my !== seq) return;
        gen.disabled = false;
        gen.textContent = "生成草稿";
      });
    }
    function doApply() {
      if (!current || !current.draft) return;
      var d = current.draft;
      if (typeof opts.onApply === "function") {
        opts.onApply({ name: d.name, match_type: d.match_type, pattern: d.pattern, action: d.action, reason: d.reason, valid: !!current.valid });
      }
      close();
    }

    root.addEventListener("click", function (e) {
      var t = e && e.target;
      if (!t) return;
      if (t === root) { close(); return; }
      var hit = function (sel) { return t.closest ? t.closest(sel) : (t.matches && t.matches(sel) ? t : null); };
      if (hit("[data-xsad-close]") || hit("[data-xsad-cancel]")) close();
      else if (hit("[data-xsad-generate]")) { if (!gen.disabled) return generate(); }
      else if (hit("[data-xsad-apply]")) { if (!apply.disabled) doApply(); }
    });
    if (doc.addEventListener) doc.addEventListener("keydown", onKey);
    setTimeout(function () { if (!closed) desc.focus(); }, 0);
    return { close: close, generate: generate, root: root };
  }

  window.XSBastionAIDraft = { open: open, _esc: esc, _lines: lines };
})();
