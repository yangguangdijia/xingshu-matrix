/*
 * 星枢 Matrix 统一创建表单（XSFormKit）
 *
 * 所有「新建 / 创建 / 编辑」对话框共用：
 *   - 可视化表单（分组或分步向导，字段按条件显示，带说明）
 *   - AI 一句话生成：调用 POST /ai/assist，把字段清单作为 schema 传给已配置的模型，回填表单
 *     （密码 / 密钥类字段永远不会发送给 AI，也不会由 AI 填写）
 *   - YAML 预览与手动编辑：表单 → YAML，YAML 修改后切回表单会解析并回填；解析失败就地提示
 *   - 可选的「测试」按钮（例如通知渠道发送测试消息、代码仓库读取分支）
 *
 * 用法：
 *   XSFormKit.open({
 *     title, scene, aiExample, wizard: false, width: "lg",
 *     sections: [{ title, desc, fields: [{ k, label, type, required, options, placeholder, help, secret, show(v), def }] }],
 *     values: {...},                         // 初始值
 *     toObject(v) -> object,                 // 表单值 → 资源对象（YAML 里展示的就是它）
 *     fromObject(obj, v) -> values,          // YAML 对象 → 表单值（缺省则 YAML 只读）
 *     validate(v) -> "错误信息" | "",
 *     submit(v, obj, ctx) -> Promise,        // ctx.status(msg) / ctx.close()
 *     actions: [{ label, run(v, obj, ctx) }] // 额外按钮，例如「发送测试」
 *   })
 *
 * 字段类型：text number password textarea select radio switch tags kv list checks code
 *   select/radio/checks 的 options：[{v, t, d?}] 或字符串数组；可传 load() 异步取选项
 *   kv：[{k,v}]，list：子字段数组（item: [...fields]）
 */
(function (root) {
  "use strict";

  var SECRET_MASK = "******";
  var seq = 0;

  function esc(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function clone(v) { return v == null ? v : JSON.parse(JSON.stringify(v)); }
  function opts(list) {
    return (list || []).map(function (o) { return typeof o === "object" ? o : { v: o, t: o }; });
  }
  function toast(msg) {
    if (root.XS && typeof root.XS.toast === "function") root.XS.toast(msg);
  }
  function api() { return root.XS && typeof root.XS.api === "function" ? root.XS : null; }
  function yamlLib() { return root.jsyaml || null; }

  function dump(obj) {
    var y = yamlLib();
    if (!y) return JSON.stringify(obj, null, 2);
    return y.dump(obj, { noRefs: true, lineWidth: 120, sortKeys: false });
  }
  function load(text) {
    var y = yamlLib();
    var obj = y ? y.load(text) : JSON.parse(text);
    if (obj == null || typeof obj !== "object" || Array.isArray(obj)) throw new Error("YAML 顶层必须是对象");
    return obj;
  }

  function allFields(spec) {
    var out = [];
    (spec.sections || []).forEach(function (s) { (s.fields || []).forEach(function (f) { out.push(f); }); });
    return out;
  }
  function visible(f, v) { return typeof f.show !== "function" || !!f.show(v); }

  /* ---------------------------------------------------------- rendering */

  function fieldHTML(f, v, id) {
    var val = v[f.k];
    var name = ' data-fk="' + esc(f.k) + '"';
    var ph = f.placeholder ? ' placeholder="' + esc(f.placeholder) + '"' : "";
    var req = f.required ? '<i class="fk-req" aria-hidden="true">*</i>' : "";
    var label = f.label ? '<span class="fk-label">' + esc(f.label) + req + "</span>" : "";
    var help = f.help ? '<span class="fk-help">' + esc(f.help) + "</span>" : "";
    var ctl = "";
    switch (f.type) {
      case "textarea":
      case "code":
        ctl = '<textarea class="x-input fk-ta' + (f.type === "code" ? " fk-code" : "") + '" rows="' + (f.rows || 4) + '"' + name + ph + ">" + esc(val == null ? "" : Array.isArray(val) ? val.join("\n") : val) + "</textarea>";
        break;
      case "select":
        ctl = '<select class="x-select"' + name + ">" + (f.required ? "" : '<option value="">' + esc(f.empty || "不选择") + "</option>") +
          opts(f.options).map(function (o) { return '<option value="' + esc(o.v) + '"' + (String(o.v) === String(val == null ? "" : val) ? " selected" : "") + ">" + esc(o.t) + "</option>"; }).join("") + "</select>";
        break;
      case "radio":
        ctl = '<div class="fk-cards" role="radiogroup">' + opts(f.options).map(function (o) {
          var on = String(o.v) === String(val);
          return '<button type="button" class="fk-card' + (on ? " on" : "") + '" role="radio" aria-checked="' + on + '" data-fk-radio="' + esc(f.k) + '" data-v="' + esc(o.v) + '">' +
            (o.icon ? '<img src="' + esc(o.icon) + '" alt="" aria-hidden="true">' : "") + "<b>" + esc(o.t) + "</b>" + (o.d ? "<small>" + esc(o.d) + "</small>" : "") + "</button>";
        }).join("") + "</div>";
        break;
      case "checks":
        var sel = Array.isArray(val) ? val.map(String) : [];
        ctl = '<div class="fk-checks">' + opts(f.options).map(function (o) {
          return '<label class="fk-check"><input type="checkbox" data-fk-check="' + esc(f.k) + '" value="' + esc(o.v) + '"' + (sel.indexOf(String(o.v)) >= 0 ? " checked" : "") + "> " + esc(o.t) + "</label>";
        }).join("") + "</div>";
        break;
      case "switch":
        ctl = '<label class="fk-switch"><input type="checkbox"' + name + (val ? " checked" : "") + '><span></span>' + esc(f.text || "") + "</label>";
        break;
      case "tags":
        ctl = '<input class="x-input" type="text"' + name + ph + ' value="' + esc(Array.isArray(val) ? val.join(", ") : val || "") + '">';
        break;
      case "kv":
        var rows = Array.isArray(val) ? val : [];
        ctl = '<div class="fk-kv" data-fk-kv="' + esc(f.k) + '">' + rows.map(function (r) { return kvRow(r, f); }).join("") +
          '<button type="button" class="x-btn fk-add" data-fk-kvadd="' + esc(f.k) + '">+ ' + esc(f.addText || "添加") + "</button></div>";
        break;
      case "list":
        var items = Array.isArray(val) ? val : [];
        ctl = '<div class="fk-list" data-fk-list="' + esc(f.k) + '">' + items.map(function (it, i) { return listItem(f, it, i); }).join("") +
          '<button type="button" class="x-btn fk-add" data-fk-listadd="' + esc(f.k) + '">+ ' + esc(f.addText || "添加") + "</button></div>";
        break;
      case "password":
        ctl = '<input class="x-input" type="password" autocomplete="new-password"' + name + ' placeholder="' + esc(f.placeholder || (v.__existing ? "留空表示保持不变" : "")) + '" value="">';
        break;
      default:
        ctl = '<input class="x-input' + (f.mono ? " x-mono" : "") + '" type="' + (f.type === "number" ? "number" : "text") + '"' + name + ph +
          (f.min != null ? ' min="' + f.min + '"' : "") + (f.max != null ? ' max="' + f.max + '"' : "") +
          ' value="' + esc(val == null ? "" : val) + '"' + (f.readonly ? " readonly" : "") + (f.options ? ' list="' + id + "-dl-" + esc(f.k) + '"' : "") + ">" +
          (f.options ? '<datalist id="' + id + "-dl-" + esc(f.k) + '">' + opts(f.options).map(function (o) { return '<option value="' + esc(o.v) + '">' + esc(o.t) + "</option>"; }).join("") + "</datalist>" : "");
    }
    var extra = f.button ? '<button type="button" class="x-btn fk-inline-btn" data-fk-btn="' + esc(f.k) + '">' + esc(f.button.label) + "</button>" : "";
    return '<div class="fk-field' + (f.full || ["textarea", "code", "radio", "kv", "list", "checks"].indexOf(f.type) >= 0 ? " fk-full" : "") + '" data-fk-wrap="' + esc(f.k) + '"' + (visible(f, v) ? "" : " hidden") + ">" +
      label + (extra ? '<div class="fk-with-btn">' + ctl + extra + "</div>" : ctl) + help + '<span class="fk-err" data-fk-err="' + esc(f.k) + '" hidden></span></div>';
  }

  function kvRow(r, f) {
    return '<div class="fk-kvrow"><input class="x-input x-mono" data-kv="k" placeholder="' + esc(f.keyPh || "键") + '" value="' + esc(r && r.k || "") + '">' +
      '<input class="x-input x-mono" data-kv="v" placeholder="' + esc(f.valPh || "值") + '" value="' + esc(r && r.v || "") + '">' +
      '<button type="button" class="x-btn fk-del" data-fk-del aria-label="删除">×</button></div>';
  }

  function listItem(f, it, i) {
    it = it || {};
    return '<div class="fk-item" data-fk-item="' + i + '"><div class="fk-item-head"><b>' + esc((f.itemTitle || "项") + " " + (i + 1)) + "</b>" +
      '<button type="button" class="x-btn fk-del" data-fk-del aria-label="删除">移除</button></div><div class="fk-grid">' +
      (f.item || []).map(function (sub) { return fieldHTML(sub, it, "li" + i); }).join("") + "</div></div>";
  }

  /* ---------------------------------------------------------- reading */

  function readScope(scope, fields, base) {
    var v = base || {};
    fields.forEach(function (f) {
      var wrap = null;
      [].some.call(scope.querySelectorAll("[data-fk-wrap]"), function (w) {
        if (w.getAttribute("data-fk-wrap") === f.k && closestScope(w) === scope) { wrap = w; return true; }
        return false;
      });
      if (!wrap) return;
      var el = wrap.querySelector('[data-fk="' + f.k + '"]');
      switch (f.type) {
        case "number": v[f.k] = el.value === "" ? null : Number(el.value); break;
        case "switch": v[f.k] = !!el.checked; break;
        case "tags": v[f.k] = String(el.value || "").split(/[,，;\s]+/).map(function (s) { return s.trim(); }).filter(Boolean); break;
        case "textarea": case "code": v[f.k] = f.lines ? String(el.value || "").split("\n").map(function (s) { return s.replace(/\s+$/, ""); }).filter(function (s) { return s.trim(); }) : el.value; break;
        case "radio":
          var on = wrap.querySelector(".fk-card.on"); v[f.k] = on ? on.getAttribute("data-v") : ""; break;
        case "checks":
          v[f.k] = [].filter.call(wrap.querySelectorAll("[data-fk-check]"), function (c) { return c.checked; }).map(function (c) { return c.value; }); break;
        case "kv":
          v[f.k] = [].map.call(wrap.querySelectorAll(".fk-kvrow"), function (r) {
            return { k: r.querySelector('[data-kv="k"]').value.trim(), v: r.querySelector('[data-kv="v"]').value };
          }).filter(function (r) { return r.k; });
          break;
        case "list":
          v[f.k] = [].map.call(wrap.querySelectorAll(":scope > .fk-list > .fk-item"), function (item) { return readScope(item, f.item || [], {}); });
          break;
        case "password":
          if (el.value) v[f.k] = el.value; else if (v[f.k] === SECRET_MASK || v.__existing) v[f.k] = v[f.k] || ""; else v[f.k] = "";
          break;
        default: v[f.k] = el ? el.value.trim() : "";
      }
    });
    return v;
  }
  function closestScope(el) {
    var n = el.parentNode;
    while (n && !(n.hasAttribute && (n.hasAttribute("data-fk-item") || n.hasAttribute("data-fk-root")))) n = n.parentNode;
    return n;
  }

  /* ---------------------------------------------------------- AI */

  function aiSchema(spec) {
    var schema = {};
    function describe(f) {
      var d = (f.label || f.k) + (f.help ? "（" + f.help + "）" : "");
      if (f.type === "number") return d + "：数字";
      if (f.type === "switch") return d + "：布尔";
      if (f.type === "tags" || f.type === "checks") return d + "：字符串数组" + (f.options ? "，可选值 " + JSON.stringify(opts(f.options).map(function (o) { return o.v; })) : "");
      if (f.type === "kv") return d + "：数组，每项 {k, v}";
      if (f.type === "list") {
        var sub = {};
        (f.item || []).forEach(function (s) { if (!s.secret && s.type !== "password") sub[s.k] = describe(s); });
        return d + "：数组，每项对象字段 " + JSON.stringify(sub);
      }
      if (f.options) return d + "：枚举，只能取 " + JSON.stringify(opts(f.options).map(function (o) { return o.v; }));
      return d + "：字符串";
    }
    allFields(spec).forEach(function (f) {
      if (f.secret || f.type === "password" || f.noAI) return;
      schema[f.k] = describe(f);
    });
    return schema;
  }

  /* ---------------------------------------------------------- open */

  function open(spec) {
    var id = "fk" + (++seq);
    var values = Object.assign({}, clone(spec.values || {}));
    allFields(spec).forEach(function (f) { if (values[f.k] === undefined && f.def !== undefined) values[f.k] = clone(f.def); });
    var wizard = !!spec.wizard && (spec.sections || []).length > 1;
    var stepIdx = 0, mode = "form", busy = false;
    var canAI = spec.scene && spec.ai !== false;

    var mask = document.createElement("div");
    mask.className = "fk-mask";
    var box = document.createElement("div");
    box.className = "fk-modal fk-" + (spec.width || "lg");
    box.setAttribute("role", "dialog");
    box.setAttribute("aria-modal", "true");
    box.setAttribute("aria-labelledby", id + "t");
    box.innerHTML =
      '<header class="fk-head"><div><h3 id="' + id + 't">' + esc(spec.title) + "</h3>" + (spec.subtitle ? "<p>" + esc(spec.subtitle) + "</p>" : "") + "</div>" +
      '<button type="button" class="fk-x" data-fk-close aria-label="关闭">×</button></header>' +
      (canAI ? '<div class="fk-ai"><span class="fk-ai-tag">AI</span><input class="x-input" data-fk-ai placeholder="' + esc(spec.aiExample ? "用一句话描述，例如：" + spec.aiExample : "用一句话描述要创建的内容") + '">' +
        '<button type="button" class="x-btn" data-fk-aigo>生成</button></div><p class="fk-ai-status" data-fk-aistatus role="status"></p>' : "") +
      (spec.toObject ? '<div class="fk-tabs" role="tablist"><button type="button" role="tab" class="on" data-fk-mode="form">可视化表单</button>' +
        '<button type="button" role="tab" data-fk-mode="yaml">YAML' + (spec.fromObject ? "（可编辑）" : "（预览）") + "</button></div>" : '<div class="fk-tabs fk-tabs--none"></div>') +
      '<div class="fk-body"><div class="fk-form" data-fk-root></div><div class="fk-yaml" hidden><textarea class="x-input fk-code" spellcheck="false" data-fk-yaml' + (spec.fromObject ? "" : " readonly") + '></textarea>' +
      '<p class="fk-help">' + (spec.fromObject ? "可直接修改 YAML，切回表单或提交时会解析校验。密码类字段以 ****** 显示，保持不变即沿用表单中的值。" : "由表单生成，仅供预览。") + "</p></div></div>" +
      '<p class="fk-status" data-fk-status role="alert" hidden></p>' +
      '<footer class="fk-foot"><span class="fk-foot-l">' + (spec.actions || []).map(function (a, i) { return '<button type="button" class="x-btn" data-fk-act="' + i + '">' + esc(a.label) + "</button>"; }).join("") + "</span>" +
      (spec.noCancel ? "" : '<button type="button" class="x-btn" data-fk-close>取消</button>') +
      (wizard ? '<button type="button" class="x-btn" data-fk-prev hidden>上一步</button><button type="button" class="x-btn x-btn--pri" data-fk-next>下一步</button>' : "") +
      '<button type="button" class="x-btn x-btn--pri" data-fk-submit' + (wizard ? " hidden" : "") + ">" + esc(spec.okText || "创建") + "</button></footer>";
    document.body.appendChild(mask);
    document.body.appendChild(box);
    var form = box.querySelector("[data-fk-root]");
    var yamlBox = box.querySelector(".fk-yaml");
    var yamlTA = box.querySelector("[data-fk-yaml]");
    var statusEl = box.querySelector("[data-fk-status]");
    var prevFocus = document.activeElement;

    function status(msg, ok) {
      statusEl.hidden = !msg;
      statusEl.textContent = msg || "";
      statusEl.className = "fk-status" + (ok ? " fk-ok" : "");
    }
    function close() {
      document.removeEventListener("keydown", onKey, true);
      root.removeEventListener("hashchange", onHash);
      mask.remove(); box.remove();
      if (prevFocus && prevFocus.focus) try { prevFocus.focus(); } catch (e) {}
    }
    function onKey(e) { if (e.key === "Escape" && !busy) { e.stopPropagation(); close(); } }
    document.addEventListener("keydown", onKey, true);
    function onHash() { if (!busy) close(); }
    root.addEventListener("hashchange", onHash);

    function render() {
      var secs = spec.sections || [];
      lastSecKey = visibleSteps().join(",");
      var html = "";
      if (wizard) {
        var vis = visibleSteps();
        if (vis.indexOf(stepIdx) < 0) stepIdx = vis.filter(function (i) { return i < stepIdx; }).pop() || vis[0];
        html += '<ol class="fk-steps">' + vis.map(function (i, n) {
          var s = secs[i];
          return '<li class="' + (i === stepIdx ? "on" : i < stepIdx ? "done" : "") + '"><button type="button" data-fk-step="' + i + '"><i>' + (i < stepIdx ? "✓" : n + 1) + "</i>" + esc(s.title) + "</button></li>";
        }).join("") + "</ol>";
      }
      html += secs.map(function (s, i) {
        if (wizard && i !== stepIdx) return "";
        if (typeof s.show === "function" && !s.show(values)) return "";
        return '<section class="fk-sec">' + (s.title && !wizard ? "<h4>" + esc(s.title) + "</h4>" : "") + (s.desc ? '<p class="fk-desc">' + esc(s.desc) + "</p>" : "") +
          '<div class="fk-grid">' + (s.fields || []).map(function (f) { return fieldHTML(f, values, id); }).join("") + "</div></section>";
      }).join("");
      form.innerHTML = html;
      if (wizard) {
        var vs = visibleSteps(), last = vs[vs.length - 1];
        box.querySelector("[data-fk-prev]").hidden = stepIdx === vs[0];
        box.querySelector("[data-fk-next]").hidden = stepIdx === last;
        box.querySelector("[data-fk-submit]").hidden = stepIdx !== last;
      }
      bind();
      (spec.sections || []).forEach(function (s) {
        (s.fields || []).forEach(function (f) { if (f.load && !f._loaded) loadOptions(f); });
      });
    }

    function visibleSteps() {
      var out = [];
      (spec.sections || []).forEach(function (s, i) { if (typeof s.show !== "function" || s.show(values)) out.push(i); });
      return out;
    }
    function stepMove(d) {
      var vs = visibleSteps(), at = vs.indexOf(stepIdx);
      stepIdx = vs[Math.max(0, Math.min(vs.length - 1, at + d))];
    }
    function loadOptions(f) {
      f._loaded = true;
      Promise.resolve().then(function () { return f.load(values); }).then(function (list) {
        f.options = list || [];
        sync(); render();
      }).catch(function (err) { f._loaded = false; status((f.label || f.k) + "选项加载失败：" + (err && err.message || err)); });
    }

    function currentFields() {
      var secs = spec.sections || [];
      var out = [];
      secs.forEach(function (s, i) { if (!wizard || i === stepIdx) (s.fields || []).forEach(function (f) { out.push(f); }); });
      return out;
    }
    function sync() {
      if (mode === "form") readScope(form, currentFields(), values);
      return values;
    }
    function refreshVisibility() {
      sync();
      allFields(spec).forEach(function (f) {
        var w = form.querySelector('[data-fk-wrap="' + f.k + '"]');
        if (w) w.hidden = !visible(f, values);
      });
      var key = visibleSteps().join(",");
      if (key !== lastSecKey) render();
    }
    var lastSecKey = "";

    function bind() {
      [].forEach.call(form.querySelectorAll("input,select,textarea"), function (el) {
        el.addEventListener("change", function () {
          var k = el.getAttribute("data-fk");
          if (el.closest(".fk-item") && el.tagName === "SELECT") { sync(); render(); return; }
          var f = k && allFields(spec).filter(function (x) { return x.k === k; })[0];
          refreshVisibility();
          if (f && typeof f.onChange === "function") { f.onChange(values, api_); }
        });
      });
      [].forEach.call(form.querySelectorAll("[data-fk-radio]"), function (b) {
        b.onclick = function () {
          var k = b.getAttribute("data-fk-radio");
          [].forEach.call(b.parentNode.querySelectorAll(".fk-card"), function (x) { x.classList.remove("on"); x.setAttribute("aria-checked", "false"); });
          b.classList.add("on"); b.setAttribute("aria-checked", "true");
          sync();
          var f = allFields(spec).filter(function (x) { return x.k === k; })[0];
          if (f && typeof f.onChange === "function") f.onChange(values, api_);
          render();
        };
      });
      [].forEach.call(form.querySelectorAll("[data-fk-kvadd]"), function (b) {
        b.onclick = function () {
          var f = findField(b.getAttribute("data-fk-kvadd"));
          b.insertAdjacentHTML("beforebegin", kvRow({}, f || {}));
          bindDel();
        };
      });
      [].forEach.call(form.querySelectorAll("[data-fk-listadd]"), function (b) {
        b.onclick = function () {
          sync();
          var k = b.getAttribute("data-fk-listadd"), f = findField(k);
          values[k] = (values[k] || []).concat([clone(f && f.newItem || {})]);
          render();
        };
      });
      [].forEach.call(form.querySelectorAll("[data-fk-btn]"), function (b) {
        b.onclick = function () {
          var f = findField(b.getAttribute("data-fk-btn"));
          sync();
          if (f && f.button && f.button.run) {
            b.disabled = true;
            Promise.resolve(f.button.run(values, api_)).catch(function (err) { status(errText(err)); }).finally(function () { b.disabled = false; });
          }
        };
      });
      [].forEach.call(form.querySelectorAll("[data-fk-step]"), function (b) {
        b.onclick = function () {
          var to = +b.getAttribute("data-fk-step");
          if (to > stepIdx && !checkStep()) return;
          sync(); stepIdx = to; render();
        };
      });
      bindDel();
    }
    function bindDel() {
      [].forEach.call(form.querySelectorAll("[data-fk-del]"), function (b) {
        b.onclick = function () {
          var item = b.closest(".fk-item");
          if (item) {
            sync();
            var listWrap = item.closest("[data-fk-list]");
            var k = listWrap.getAttribute("data-fk-list"), idx = +item.getAttribute("data-fk-item");
            values[k].splice(idx, 1);
            render();
          } else b.closest(".fk-kvrow").remove();
        };
      });
    }
    function findField(k) {
      var hit = null;
      (function walk(fields) {
        fields.forEach(function (f) { if (f.k === k && !hit) hit = f; if (f.item) walk(f.item); });
      })(allFields(spec));
      return hit;
    }

    function fieldErr(k, msg) {
      var e = form.querySelector('[data-fk-err="' + k + '"]');
      if (e) { e.hidden = !msg; e.textContent = msg || ""; }
    }
    function checkFields(fields) {
      var bad = "";
      fields.forEach(function (f) {
        fieldErr(f.k, "");
        if (!visible(f, values)) return;
        var v = values[f.k];
        var empty = v == null || v === "" || (Array.isArray(v) && !v.length);
        if (f.type === "password" && values.__existing) empty = false;
        if (f.required && empty) { fieldErr(f.k, "必填"); bad = bad || (f.label || f.k) + "为必填项"; return; }
        if (!empty && f.pattern && !new RegExp(f.pattern).test(String(v))) { fieldErr(f.k, f.patternMsg || "格式不正确"); bad = bad || (f.label || f.k) + "：" + (f.patternMsg || "格式不正确"); }
        if (!empty && typeof f.check === "function") {
          var m = f.check(v, values);
          if (m) { fieldErr(f.k, m); bad = bad || (f.label || f.k) + "：" + m; }
        }
      });
      return bad;
    }
    function checkStep() {
      sync();
      var bad = checkFields(currentFields());
      if (bad) { status(bad); return false; }
      if (wizard && typeof spec.sections[stepIdx].validate === "function") {
        var m = spec.sections[stepIdx].validate(values);
        if (m) { status(m); return false; }
      }
      status("");
      return true;
    }

    function maskSecrets(v) {
      var copy = clone(v);
      allFields(spec).forEach(function (f) { if ((f.type === "password" || f.secret) && copy[f.k]) copy[f.k] = SECRET_MASK; });
      return copy;
    }
    function unmaskSecrets(v) {
      allFields(spec).forEach(function (f) { if ((f.type === "password" || f.secret) && v[f.k] === SECRET_MASK) v[f.k] = values[f.k]; });
      return v;
    }
    function toYaml() {
      var obj = spec.toObject(maskSecrets(values));
      return dump(obj);
    }
    function fromYaml() {
      var obj = load(yamlTA.value);
      var next = spec.fromObject(obj, clone(values));
      values = unmaskSecrets(Object.assign({}, values, next));
    }

    function setMode(m) {
      if (m === mode) return true;
      if (m === "yaml") {
        sync();
        try { yamlTA.value = toYaml(); } catch (e) { status("生成 YAML 失败：" + errText(e)); return false; }
      } else if (spec.fromObject) {
        try { fromYaml(); } catch (e) { status("YAML 解析失败：" + errText(e)); return false; }
      }
      mode = m;
      form.hidden = m !== "form";
      yamlBox.hidden = m !== "yaml";
      [].forEach.call(box.querySelectorAll("[data-fk-mode]"), function (b) { b.classList.toggle("on", b.getAttribute("data-fk-mode") === m); });
      if (m === "form") render();
      status("");
      return true;
    }
    [].forEach.call(box.querySelectorAll("[data-fk-mode]"), function (b) { b.onclick = function () { setMode(b.getAttribute("data-fk-mode")); }; });

    function collect() {
      if (mode === "yaml" && spec.fromObject) fromYaml(); else sync();
      var bad = checkFields(allFields(spec));
      if (bad) return { err: bad };
      if (typeof spec.validate === "function") {
        var m = spec.validate(values);
        if (m) return { err: m };
      }
      var obj = spec.toObject ? spec.toObject(values) : null;
      return { v: values, obj: obj };
    }

    var api_ = {
      values: function () { return sync(); },
      set: function (patch) { sync(); Object.assign(values, patch); if (mode === "form") render(); else yamlTA.value = toYaml(); },
      setOptions: function (k, list) { var f = findField(k); if (f) { f.options = list; f._loaded = true; sync(); render(); } },
      status: status, close: close, el: box
    };

    function run(fn, btn) {
      if (busy) return;
      var c;
      try { c = collect(); } catch (e) { status("YAML 解析失败：" + errText(e)); return; }
      if (c.err) { status(c.err); if (mode === "yaml") return; return; }
      busy = true; btn.disabled = true; status("");
      Promise.resolve().then(function () { return fn(c.v, c.obj, api_); }).catch(function (err) { status(errText(err)); })
        .finally(function () { busy = false; btn.disabled = false; });
    }

    box.querySelector("[data-fk-submit]").onclick = function () { run(spec.submit, this); };
    (spec.actions || []).forEach(function (a, i) { box.querySelector('[data-fk-act="' + i + '"]').onclick = function () { run(a.run, this); }; });
    [].forEach.call(box.querySelectorAll("[data-fk-close]"), function (b) { b.onclick = function () { if (!busy) close(); }; });
    if (wizard) {
      box.querySelector("[data-fk-next]").onclick = function () { if (checkStep()) { stepMove(1); render(); } };
      box.querySelector("[data-fk-prev]").onclick = function () { sync(); stepMove(-1); render(); };
    }

    if (canAI) {
      var aiIn = box.querySelector("[data-fk-ai]"), aiBtn = box.querySelector("[data-fk-aigo]"), aiSt = box.querySelector("[data-fk-aistatus]");
      var gen = function () {
        var text = String(aiIn.value || "").trim();
        if (!text) { aiSt.textContent = "先用一句话描述你要创建什么。"; aiIn.focus(); return; }
        if (/(password|passwd|token|secret|密码|口令|私钥)\s*[:=：]\s*\S+/i.test(text) || /-----BEGIN/.test(text)) { aiSt.textContent = "描述里疑似包含密码或密钥，请删掉具体值；密钥类字段请手动填写。"; return; }
        var x = api();
        if (!x) { aiSt.textContent = "AI 接口未连接，可以手动填写。"; return; }
        aiBtn.disabled = true; aiBtn.textContent = "生成中…"; aiSt.textContent = "正在调用已配置的模型…";
        sync();
        var context = typeof spec.aiContext === "function" ? spec.aiContext(values) : "";
        x.api("/ai/assist", { method: "POST", body: { scene: spec.scene, instruction: text + (context ? "\n\n当前上下文：" + context : ""), schema: aiSchema(spec) } }).then(function (r) {
          var draft = r && (r.fields || r.data || r) || {};
          var n = 0;
          Object.keys(draft).forEach(function (k) {
            var f = findField(k);
            if (!f || f.secret || f.type === "password") return;
            var val = draft[k];
            if (val == null || val === "" || (Array.isArray(val) && !val.length)) return;
            if (f.options && f.type !== "checks" && f.type !== "tags" && !f.load && opts(f.options).every(function (o) { return String(o.v) !== String(val); })) return;
            if (f.type === "number") val = Number(val);
            if (f.type === "kv" && val && !Array.isArray(val) && typeof val === "object") val = Object.keys(val).map(function (kk) { return { k: kk, v: String(val[kk]) }; });
            values[k] = val; n++;
          });
          if (typeof spec.afterAI === "function") spec.afterAI(values);
          if (mode === "form") render(); else yamlTA.value = toYaml();
          aiSt.textContent = n ? "已填入 " + n + " 个字段，请检查后再提交" + (wizard ? "（可逐步查看）" : "") + "。" : "模型没有给出可用字段，请补充名称、用途等关键信息再试。";
        }).catch(function (err) {
          aiSt.textContent = /model|模型/i.test(errText(err)) ? "尚未配置可用的 AI 模型，请到「设置 → AI 模型」接入；也可以手动填写。" : "生成失败：" + errText(err);
        }).finally(function () { aiBtn.disabled = false; aiBtn.textContent = "生成"; });
      };
      aiBtn.onclick = gen;
      aiIn.onkeydown = function (e) { if (e.key === "Enter" && !e.isComposing) { e.preventDefault(); gen(); } };
    }

    render();
    var first = box.querySelector(canAI ? "[data-fk-ai]" : "input,select,textarea");
    if (first) try { first.focus(); } catch (e) {}
    return api_;
  }

  function errText(err) {
    if (!err) return "请求失败";
    if (typeof err === "string") return err;
    return err.message || err.msg || err.error || "请求失败";
  }

  /* helpers for callers */
  function kvToMap(list) {
    var m = {};
    (list || []).forEach(function (r) { if (r && r.k) m[r.k] = r.v == null ? "" : String(r.v); });
    return m;
  }
  function mapToKv(map) {
    return Object.keys(map || {}).map(function (k) { return { k: k, v: String(map[k]) }; });
  }


  /*
   * enhance(form, opts)：给旧版对话框表单（formModal 等）补上 AI 一句话填写和 YAML 视图，
   * 不改变原有提交逻辑。只处理普通字段（text/number/select/textarea/checkbox）；密码字段既不发给 AI
   * 也不在 YAML 里显示明文；结构化子表单在 YAML 里只读展示。
   */
  function enhance(form, o) {
    if (!form || form.getAttribute("data-fk-enhanced")) return;
    form.setAttribute("data-fk-enhanced", "1");
    o = o || {};
    var fields = (o.fields || []).filter(function (f) { return f && f.k; });
    var simple = fields.filter(function (f) { return !f.kind || ["text", "number", "select", "textarea", "checkbox", "url", "email"].indexOf(f.kind) >= 0; });
    if (!simple.length) return;
    function node(k) { return form.elements ? form.elements[k] : null; }
    function readAll() {
      var out = {};
      fields.forEach(function (f) {
        var n = node(f.k);
        if (f.kind === "password") { if (n && n.value) out[f.k] = SECRET_MASK; return; }
        if (!n || n.length > 1 && !n.tagName) return;
        if (f.kind === "checkbox") out[f.k] = !!n.checked;
        else if (f.kind === "number") out[f.k] = n.value === "" ? null : Number(n.value);
        else if (n.value !== undefined) out[f.k] = n.value;
      });
      return out;
    }
    function writeAll(vals, fromAI) {
      var n_ = 0;
      simple.forEach(function (f) {
        if (!(f.k in vals)) return;
        var n = node(f.k), v = vals[f.k];
        if (!n || v == null || (fromAI && v === "")) return;
        if (f.kind === "checkbox") { n.checked = !!v; n_++; }
        else if (f.kind === "select") {
          var ok = [].some.call(n.options || [], function (op) { return op.value === String(v); });
          if (ok) { n.value = String(v); n_++; }
        } else { n.value = Array.isArray(v) ? v.join(f.kind === "textarea" ? "\n" : ",") : String(v); n_++; }
        try { n.dispatchEvent(new Event("change", { bubbles: true })); } catch (e) {}
      });
      return n_;
    }
    var wrap = document.createElement("div");
    wrap.className = "fk-enh";
    var canAI = o.ai !== false;
    wrap.innerHTML = (canAI ? '<div class="fk-ai"><span class="fk-ai-tag">AI</span><input class="x-input" data-fk-ai placeholder="用一句话描述要填写的内容，AI 帮你填表"><button type="button" class="x-btn" data-fk-aigo>生成</button></div><p class="fk-ai-status" data-fk-aistatus role="status"></p>' : "") +
      '<div class="fk-tabs" role="tablist"><button type="button" role="tab" class="on" data-fk-mode="form">可视化表单</button><button type="button" role="tab" data-fk-mode="yaml">YAML</button></div>' +
      '<div class="fk-yaml" hidden><textarea class="x-input fk-code" spellcheck="false" data-fk-yaml></textarea><p class="fk-help">修改后切回表单会回填普通字段；密码以 ****** 显示且不会被覆盖。</p><p class="fk-err" data-fk-yerr hidden></p></div>';
    form.insertBefore(wrap, form.firstChild);
    var yamlBox = wrap.querySelector(".fk-yaml"), ta = wrap.querySelector("[data-fk-yaml]"), yerr = wrap.querySelector("[data-fk-yerr]");
    var mode = "form";
    function others() { return [].filter.call(form.children, function (c) { return c !== wrap; }); }
    function setMode(m) {
      if (m === mode) return;
      if (m === "yaml") {
        var obj = readAll();
        if (typeof o.extra === "function") { try { Object.assign(obj, o.extra()); } catch (e) {} }
        ta.value = dump(obj);
      } else {
        try {
          var parsed = load(ta.value);
          Object.keys(parsed).forEach(function (k) { if (parsed[k] === SECRET_MASK) delete parsed[k]; });
          writeAll(parsed, false);
          yerr.hidden = true;
        } catch (e) { yerr.textContent = "YAML 解析失败：" + errText(e); yerr.hidden = false; return; }
      }
      mode = m;
      yamlBox.hidden = m !== "yaml";
      others().forEach(function (c) { if (!c.classList.contains("x-err") || m === "form") c.style.display = m === "yaml" ? "none" : ""; });
      [].forEach.call(wrap.querySelectorAll("[data-fk-mode]"), function (b) { b.classList.toggle("on", b.getAttribute("data-fk-mode") === m); });
    }
    [].forEach.call(wrap.querySelectorAll("[data-fk-mode]"), function (b) { b.onclick = function () { setMode(b.getAttribute("data-fk-mode")); }; });
    form.addEventListener("submit", function () { if (mode === "yaml") setMode("form"); }, true);
    if (!canAI) return;
    var aiIn = wrap.querySelector("[data-fk-ai]"), aiBtn = wrap.querySelector("[data-fk-aigo]"), st = wrap.querySelector("[data-fk-aistatus]");
    function gen() {
      var text = String(aiIn.value || "").trim();
      if (!text) { st.textContent = "先用一句话描述要填写的内容。"; return; }
      if (/(password|passwd|token|secret|密码|口令|私钥)\s*[:=：]\s*\S+/i.test(text) || /-----BEGIN/.test(text)) { st.textContent = "描述里疑似包含密码或密钥，请删掉具体值；密钥类字段请手动填写。"; return; }
      var x = api();
      if (!x) { st.textContent = "AI 接口未连接，可以手动填写。"; return; }
      var schema = {};
      simple.forEach(function (f) {
        var d = (f.label || f.k) + (f.placeholder ? "（例如 " + f.placeholder + "）" : "");
        if (f.kind === "select") d += "：只能取 " + JSON.stringify((f.options || []).map(function (op) { return typeof op === "object" ? op.v : op; }));
        else if (f.kind === "checkbox") d += "：布尔";
        else if (f.kind === "number") d += "：数字";
        schema[f.k] = d;
      });
      aiBtn.disabled = true; aiBtn.textContent = "生成中…"; st.textContent = "正在调用已配置的模型…";
      x.api("/ai/assist", { method: "POST", body: { scene: o.scene || "ops.form", instruction: text, schema: schema } }).then(function (r) {
        var n = writeAll(r && r.fields || {}, true);
        st.textContent = n ? "已填入 " + n + " 个字段，请检查后再提交。" : "模型没有给出可用字段，请补充关键信息再试。";
      }).catch(function (err) {
        st.textContent = /model|模型/i.test(errText(err)) ? "尚未配置可用的 AI 模型，请到「设置 → AI 模型」接入；也可以手动填写。" : "生成失败：" + errText(err);
      }).finally(function () { aiBtn.disabled = false; aiBtn.textContent = "生成"; });
    }
    aiBtn.onclick = gen;
    aiIn.onkeydown = function (e) { if (e.key === "Enter" && !e.isComposing) { e.preventDefault(); gen(); } };
  }

  root.XSFormKit = { open: open, enhance: enhance, dump: dump, load: load, kvToMap: kvToMap, mapToKv: mapToKv, errText: errText, SECRET_MASK: SECRET_MASK };
})(window);
