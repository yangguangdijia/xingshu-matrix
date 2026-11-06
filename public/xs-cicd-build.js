/*
 * CI/CD 构建辅助：
 *   XSCICDDetect —— 「从代码仓库创建应用」里的「识别语言」：调用后端在执行集群里浅克隆仓库、
 *                    按标志文件识别语言与构建工具，返回建议模板；可按模板一键生成流水线。
 *   XSCICDCache  —— 流水线详情里的「编译缓存」区块（依赖缓存 / 镜像层缓存设置、占用、最近命中、清理），
 *                    以及运行摘要里的「本次缓存」字段。拿不到的数据显示「暂无数据」，不做估算。
 * 原生 JS IIFE，所有输出经 esc() 转义；清理缓存需输入流水线名称二次确认。
 */
(function (root) {
  "use strict";

  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function msg(err) { return root.XSFormKit && root.XSFormKit.errText ? root.XSFormKit.errText(err) : (err && err.message) || "请求失败"; }
  function approvalPending(err) { return err && (err.status === 202 || err.code === 40304 || err.approval); }
  function clone(v) { return v == null ? v : JSON.parse(JSON.stringify(v)); }
  function fmtTime(v) {
    if (!v) return "—";
    var d = new Date(v);
    return isNaN(d.getTime()) ? esc(v) : esc(d.toLocaleString("zh-CN", { hour12: false }));
  }
  function fmtBytes(n) {
    if (n == null || !isFinite(n)) return "";
    var u = ["B", "KiB", "MiB", "GiB", "TiB"], i = 0;
    n = Number(n);
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i ? n.toFixed(n >= 100 ? 0 : 1) : String(n)) + " " + u[i];
  }
  function fact(label, value) { return '<div class="x-cicd-fact"><dt>' + esc(label) + "</dt><dd>" + value + "</dd></div>"; }

  /* ------------------------------------------------------------ 识别语言 */

  var LANG = { go: "Go", node: "Node.js", java: "Java", python: "Python", dockerfile: "仅 Dockerfile", unknown: "未识别" };
  var TOOL = { go: "Go Modules", npm: "npm", yarn: "Yarn", pnpm: "pnpm", maven: "Maven", gradle: "Gradle", pip: "pip", poetry: "Poetry", dockerfile: "Dockerfile" };
  var MODE = { dockerfile: "使用仓库中的 Dockerfile 构建", template: "使用内置模板构建（仓库中没有 Dockerfile）", none: "无法构建：缺少 Dockerfile" };

  function detect(body) {
    return xs().post("/cicd/repos/detect", body, { timeoutMs: 150000 });
  }
  function summary(r) {
    if (!r) return "";
    var s = LANG[r.language] || r.language || "未识别";
    if (r.version && r.language !== "dockerfile") s += " " + r.version;
    if (r.build_tool && r.build_tool !== "dockerfile" && TOOL[r.build_tool]) s += " · " + TOOL[r.build_tool];
    return s + " · " + (MODE[r.build_mode] || "");
  }
  function statusText(r) {
    var parts = ["识别完成：" + summary(r)];
    if (r.evidence && r.evidence.length) parts.push("依据 " + r.evidence.join("、"));
    if (r.others && r.others.length) parts.push("仓库中还包含 " + r.others.join("、"));
    if (r.template && r.template.stages) parts.push("建议阶段：" + r.template.stages.map(function (s) { return s.type === "build" ? "镜像构建" : s.type === "test" ? "测试（" + (s.image || "") + "）" : s.name; }).join(" → "));
    if (r.notes && r.notes.length) parts.push(r.notes.join("；"));
    return parts.join("。");
  }
  function errorText(err) {
    var m = msg(err);
    if (/cannot clone/i.test(m)) return "无法克隆仓库：请检查仓库地址、分支和凭证" + (m.indexOf("(") > 0 ? "（" + m.slice(m.indexOf("(") + 1).replace(/\)$/, "") + "）" : "");
    if (/no execution cluster/i.test(m)) return "还没有执行集群：请先在「流水线 → 执行集群」接入一个已注册集群";
    if (/cannot pull the Git image/i.test(m)) return "执行集群无法拉取 Git 镜像，请检查集群的镜像拉取配置";
    if (/not in your scope/i.test(m)) return "无权使用该执行集群所属的环境";
    if (/credential/i.test(m)) return "凭证不可用：" + m;
    if (/unknown runner target/i.test(m)) return "执行集群不存在或已删除";
    if (/branch/i.test(m)) return "分支名不合法";
    if (/approval/i.test(m) || /审批/.test(m)) return "该环境的配置变更需要审批";
    return "识别失败：" + m;
  }

  /* 按识别结果生成流水线：测试 → 镜像构建 → 部署到第一个环境，并带上缓存配置。 */
  function createPipeline(app, v, detected) {
    var tpl = detected && detected.template;
    if (!tpl || !tpl.stages || !tpl.stages.length) return Promise.resolve(null);
    var envs = v.envs || [], env0 = envs[0];
    if (!env0 || !env0.target_id) return Promise.reject(new Error("缺少部署环境或执行集群"));
    var stages = clone(tpl.stages).map(function (s) {
      if (s.type === "build" && v.registry_credential_id) s.registry_credential_id = Number(v.registry_credential_id);
      return s;
    });
    stages.push({ name: ("deploy-" + env0.name).slice(0, 40).replace(/-+$/, ""), type: "deploy", deploy_env: env0.name });
    var name = (String(app.name || v.name) + "-ci").slice(0, 64);
    var body = { app_id: app.id, name: name, runner_target_id: Number(env0.target_id), stages: stages, on_push: false };
    if (tpl.cache) body.cache = clone(tpl.cache);
    return xs().post("/cicd/pipelines", body).then(function (p) { return p || { name: name }; }, function (err) {
      if (approvalPending(err)) return { pending: true, name: name };
      throw err;
    });
  }

  /* ------------------------------------------------------------ 编译缓存 */

  var LANGS = [
    { v: "go", t: "Go（模块与编译缓存）" }, { v: "npm", t: "Node.js · npm" }, { v: "yarn", t: "Node.js · Yarn" }, { v: "pnpm", t: "Node.js · pnpm" },
    { v: "maven", t: "Java · Maven" }, { v: "gradle", t: "Java · Gradle" }, { v: "python", t: "Python · pip" }, { v: "custom", t: "自定义路径" }
  ];
  var DEFAULT_PATHS = {
    go: ["/go/pkg/mod", "/root/.cache/go-build"], node: ["~/.npm"], npm: ["~/.npm"], yarn: ["/usr/local/share/.cache/yarn"],
    pnpm: ["~/.local/share/pnpm/store"], maven: ["~/.m2/repository"], gradle: ["~/.gradle/caches", "~/.gradle/wrapper"], python: ["~/.cache/pip"]
  };
  var RESULT = { hit: "命中", miss: "未命中", partial: "部分命中", unknown: "未获取到结果" };
  var PHASE = { Bound: "已绑定", Pending: "等待绑定（首次运行时创建）", Lost: "已丢失" };
  var states = {}; // pipelineId → { data, error, loading, at }

  function load(id, repaint, force) {
    var st = states[id] || (states[id] = {});
    if (st.loading || (!force && st.at && Date.now() - st.at < 60000)) return;
    st.loading = true;
    xs().get("/cicd/pipelines/" + encodeURIComponent(id) + "/cache").then(function (d) {
      st.data = d; st.error = "";
    }, function (err) { st.error = msg(err); }).then(function () {
      st.loading = false; st.at = Date.now();
      if (typeof repaint === "function") repaint();
    });
  }

  function storageText(d) {
    var c = d.config || {}, s = d.storage;
    if (c.storage === "hostpath") return "节点本地目录（仅沙箱）" + (s && s.name ? " <code>" + esc(s.name) + "</code>" : "");
    var t = "持久卷";
    if (s && s.name) t += " <code>" + esc(s.name) + "</code>";
    var extra = [];
    if (s && s.error) extra.push(esc(s.error));
    else if (s && s.exists === false) extra.push("尚未创建，首次运行时自动创建");
    else if (s && s.exists) {
      if (s.phase) extra.push(esc(PHASE[s.phase] || s.phase));
      if (s.capacity) extra.push("容量 " + esc(s.capacity));
      if (s.storage_class) extra.push("存储类 " + esc(s.storage_class));
    }
    if (!s || !s.exists) extra.push("申请 " + esc(c.size || "5Gi") + (c.storage_class ? "，存储类 " + esc(c.storage_class) : "，默认存储类"));
    return t + (extra.length ? "（" + extra.join("，") + "）" : "");
  }

  function sectionHTML(pl) {
    if (!pl || pl.id == null) return "";
    var st = states[pl.id] || {}, d = st.data;
    var head = '<div class="x-cicd-section-head"><h2>编译缓存</h2><span class="x-cicd-actions">' +
      '<button class="x-btn" type="button" data-xs-cache="edit" data-id="' + esc(pl.id) + '">缓存设置</button>' +
      (d && d.configured && d.config && d.config.enabled ? '<button class="x-btn" type="button" data-xs-cache="clear" data-id="' + esc(pl.id) + '">清理缓存</button>' : "") +
      "</span></div>";
    if (!d) {
      return '<section class="x-cicd-section" aria-label="编译缓存">' + head +
        (st.error ? '<p class="x-cicd-alert" role="status">缓存信息加载失败：' + esc(st.error) + ' <button class="x-btn" type="button" data-xs-cache="reload" data-id="' + esc(pl.id) + '">重试</button></p>'
          : '<p class="x-cicd-empty">正在读取缓存信息…</p>') + "</section>";
    }
    var c = d.config || {};
    if (!d.configured || (!c.enabled && !c.layer_cache)) {
      return '<section class="x-cicd-section" aria-label="编译缓存">' + head +
        '<p class="x-cicd-empty">未开启编译缓存。开启后，测试与脚本阶段的依赖目录（如 Go 模块、npm、Maven 仓库）会保存在持久卷中供后续运行复用，镜像构建阶段可复用已推送的镜像层。</p></section>';
    }
    var rows = [];
    rows.push(fact("依赖缓存", c.enabled ? "已开启" : "未开启"));
    if (c.enabled) {
      rows.push(fact("缓存路径", (d.resolved_paths || c.paths || []).map(function (p) { return "<code>" + esc(p) + "</code>"; }).join(" ") || "—"));
      rows.push(fact("缓存键", "<code>" + esc(c.key || "{{branch}}") + "</code>"));
      rows.push(fact("存储", storageText(d)));
      rows.push(fact("占用", d.usage_bytes != null ? esc(fmtBytes(d.usage_bytes)) + '<span class="x-cicd-muted">（' + fmtTime(d.usage_at) + " 运行时统计）</span>" : "暂无数据"));
      rows.push(fact("最近一次", d.last_run ? "运行 #" + esc(d.last_run.run_id) + " · " + esc(RESULT[d.last_run.result] || d.last_run.result) + " · " + fmtTime(d.last_run.at) : "尚无使用缓存的运行"));
      rows.push(fact("最近命中", d.last_hit ? "运行 #" + esc(d.last_hit.run_id) + " · 缓存键 <code>" + esc(d.last_hit.key) + "</code> · " + fmtTime(d.last_hit.at) : "尚无命中"));
    }
    rows.push(fact("镜像层缓存", c.layer_cache ? "已开启" + (d.layer_cache_repo ? "，缓存仓库 <code>" + esc(d.layer_cache_repo) + "</code>" : "") : "未开启"));
    if (c.layer_cache) rows.push(fact("层缓存最近命中", d.last_layer_hit ? "运行 #" + esc(d.last_layer_hit.run_id) + " · 复用 " + esc(d.last_layer_hit.hits) + " 层 · " + fmtTime(d.last_layer_hit.at) : "尚无命中"));
    return '<section class="x-cicd-section" aria-label="编译缓存">' + head + '<dl class="x-cicd-grid">' + rows.join("") + "</dl>" +
      (c.layer_cache ? '<p class="x-cicd-note">镜像层缓存保存在镜像仓库中，「清理缓存」只清理依赖缓存；镜像层缓存请在镜像仓库中删除缓存仓库。</p>' : "") + "</section>";
  }

  function runFactHTML(run) {
    var rc = run && run.cache;
    if (!rc || (!rc.deps && !rc.layer)) return fact("编译缓存", "本次未使用");
    var parts = [];
    if (rc.deps) {
      var t = "依赖缓存：" + esc(RESULT[rc.deps.result] || "未获取到结果");
      if (rc.deps.key) t += "（缓存键 <code>" + esc(rc.deps.key) + "</code>）";
      parts.push(t);
    }
    if (rc.layer) parts.push("镜像层缓存：" + (rc.layer.observed ? "复用 " + esc(rc.layer.hits) + " 层、新建 " + esc(rc.layer.misses) + " 层" : "已开启（未获取到命中统计）"));
    return fact("编译缓存", parts.join("；"));
  }

  function cacheForm(pl, current, done) {
    var c = current && current.config || {};
    var values = {
      enabled: current && current.configured ? !!c.enabled : true, language: c.language || "go", paths: (c.paths || []).join("\n"),
      key: c.key || "{{branch}}", storage: c.storage || "pvc", storage_class: c.storage_class || "", size: c.size || "5Gi",
      access_mode: c.access_mode || "ReadWriteOnce", layer_cache: current && current.configured ? !!c.layer_cache : true,
      layer_cache_repo: c.layer_cache_repo || "", layer_cache_ttl_hours: c.layer_cache_ttl_hours || null
    };
    return root.XSFormKit.open({
      title: "编译缓存设置", subtitle: "流水线「" + (pl.name || pl.id) + "」", width: "md", okText: "保存", ai: false,
      scene: "cicd.pipeline.cache", values: values,
      sections: [
        { title: "依赖缓存", desc: "测试与脚本阶段的依赖目录挂载到本流水线专用的持久卷，多次运行复用。镜像构建阶段不使用依赖缓存。", fields: [
          { k: "enabled", type: "switch", text: "开启依赖缓存" },
          { k: "language", label: "语言 / 构建工具", type: "select", options: LANGS, show: function (v) { return v.enabled; },
            help: "决定默认缓存路径；下方填写路径后以填写的为准" },
          { k: "paths", label: "缓存路径（每行一个）", type: "textarea", rows: 3, full: true, mono: true, show: function (v) { return v.enabled; },
            placeholder: "留空使用默认路径，例如 /go/pkg/mod", help: "支持绝对路径、~/ 开头（按 /root）或相对源码目录的路径" },
          { k: "key", label: "缓存键", mono: true, required: true, show: function (v) { return v.enabled; },
            help: "可用 {{branch}}（分支）、{{app}}、{{pipeline}}、{{hash:go.sum}}（文件内容摘要，锁文件变化即换新缓存）",
            check: function (s) { return /^([A-Za-z0-9._-]|\{\{\s*(branch|app|pipeline|hash:[A-Za-z0-9._\/@+-]+|checksum:[A-Za-z0-9._\/@+-]+)\s*\}\})+$/.test(s) ? "" : "只能包含字母、数字、. _ - 和 {{…}} 变量"; } },
          { k: "storage", label: "存储方式", type: "select", show: function (v) { return v.enabled; },
            options: [{ v: "pvc", t: "持久卷（推荐）" }, { v: "hostpath", t: "节点本地目录（仅沙箱，无存储类时使用）" }] },
          { k: "storage_class", label: "存储类", mono: true, placeholder: "留空使用集群默认存储类", show: function (v) { return v.enabled && v.storage === "pvc"; } },
          { k: "size", label: "容量", mono: true, required: true, show: function (v) { return v.enabled && v.storage === "pvc"; },
            check: function (s) { return /^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti)$/.test(s) ? "" : "格式如 5Gi"; }, help: "修改容量需清理缓存后在下次运行时重建" },
          { k: "access_mode", label: "访问模式", type: "select", show: function (v) { return v.enabled && v.storage === "pvc"; },
            options: [{ v: "ReadWriteOnce", t: "单节点读写（默认）" }, { v: "ReadWriteMany", t: "多节点读写（需存储支持）" }] }
        ] },
        { title: "镜像层缓存", desc: "镜像构建阶段把中间层推送到缓存仓库，下次构建命中时直接复用。推送凭证与构建阶段相同。", fields: [
          { k: "layer_cache", type: "switch", text: "开启镜像层缓存" },
          { k: "layer_cache_repo", label: "缓存仓库", mono: true, full: true, show: function (v) { return v.layer_cache; },
            placeholder: "留空使用 <镜像仓库>/cache", check: function (s) { return !s || /^[A-Za-z0-9.-]+(:[0-9]{1,5})?(\/[a-z0-9._\/-]+)+$/.test(s) ? "" : "格式如 harbor.example.com/team/app/cache，不带 http(s):// 与 tag"; } },
          { k: "layer_cache_ttl_hours", label: "缓存有效期（小时）", type: "number", min: 0, max: 8760, show: function (v) { return v.layer_cache; }, help: "留空使用默认值" }
        ] }
      ],
      submit: function (v, _o, ctx) {
        var paths = String(v.paths || "").split(/\r?\n/).map(function (s) { return s.trim(); }).filter(Boolean);
        var body = {
          enabled: !!v.enabled, language: v.language === "custom" ? "custom" : v.language, paths: paths, key: v.key || "{{branch}}",
          storage: v.storage, storage_class: v.storage === "pvc" ? v.storage_class || "" : "", size: v.size || "5Gi",
          access_mode: v.storage === "pvc" ? v.access_mode : "", layer_cache: !!v.layer_cache, layer_cache_repo: v.layer_cache_repo || "",
          layer_cache_ttl_hours: v.layer_cache_ttl_hours ? Number(v.layer_cache_ttl_hours) : 0
        };
        if (body.enabled && body.language === "custom" && !paths.length) { ctx.status("自定义路径时至少填写一个缓存路径"); return Promise.resolve(); }
        return xs().put("/cicd/pipelines/" + encodeURIComponent(pl.id) + "/cache", body).then(function () {
          ctx.close(); toast("缓存设置已保存，下次运行生效"); if (done) done();
        }, function (err) {
          if (approvalPending(err)) { ctx.close(); toast("已提交审批，审批通过后生效"); if (done) done(); return; }
          throw new Error(cacheErr(err));
        });
      }
    });
  }

  function cacheErr(err) {
    var m = msg(err);
    if (/cache path/i.test(m)) return "缓存路径不合法或不允许使用：" + m.replace(/^cache path\s*/i, "").replace(/\s*(is invalid|cannot be used)$/i, "");
    if (/cache key|variable|\{\{hash/i.test(m)) return "缓存键格式不正确";
    if (/storage class/i.test(m)) return "存储类名称不合法";
    if (/cache size/i.test(m)) return "容量格式不正确（例如 5Gi，最大 1Ti）";
    if (/layer cache repository/i.test(m)) return "缓存仓库格式不正确";
    if (/active runs/i.test(m)) return "流水线正在运行，请等运行结束后再清理缓存";
    if (/confirmation/i.test(m)) return "输入的名称与流水线名称不一致";
    if (/changed/i.test(m)) return "流水线已被修改，请刷新后重试";
    if (/no dependency cache/i.test(m)) return "该流水线没有依赖缓存";
    return m;
  }

  function clearForm(pl, done) {
    return root.XSFormKit.open({
      title: "清理编译缓存", width: "md", okText: "清理", ai: false, scene: "cicd.pipeline.cache.clear",
      values: { confirm: "" },
      sections: [{ desc: "将删除流水线「" + pl.name + "」的依赖缓存（持久卷或节点目录中的全部内容），下次运行会重新下载依赖。镜像层缓存不受影响。此操作不可撤销。", fields: [
        { k: "confirm", label: "请输入流水线名称以确认", required: true, mono: true, placeholder: pl.name,
          check: function (s) { return s === pl.name ? "" : "名称不一致"; } }
      ] }],
      submit: function (v, _o, ctx) {
        if (v.confirm !== pl.name) { ctx.status("名称不一致"); return Promise.resolve(); }
        ctx.status("正在清理…", true);
        return xs().post("/cicd/pipelines/" + encodeURIComponent(pl.id) + "/cache/clear", { confirm: v.confirm }, { timeoutMs: 360000 }).then(function () {
          ctx.close(); toast("依赖缓存已清理"); if (done) done();
        }, function (err) {
          if (approvalPending(err)) { ctx.close(); toast("已提交审批，审批通过后执行清理"); if (done) done(); return; }
          throw new Error(cacheErr(err));
        });
      }
    });
  }

  var bound = typeof WeakSet === "function" ? new WeakSet() : null;
  var lastRepaint = {};
  /* 绑定事件（委托，重绘后依然有效）并在需要时加载缓存信息；repaint 由调用方提供。 */
  function bind(el, pl, repaint) {
    if (!el || !pl || pl.id == null) return;
    lastRepaint[pl.id] = repaint;
    load(pl.id, repaint, false);
    if (bound && bound.has(el)) return;
    if (bound) bound.add(el);
    el.addEventListener("click", function (ev) {
      var b = ev.target && ev.target.closest ? ev.target.closest("[data-xs-cache]") : null;
      if (!b) return;
      var id = b.getAttribute("data-id"), action = b.getAttribute("data-xs-cache"), st = states[id] || {};
      var cur = el.__xsCachePipeline && String(el.__xsCachePipeline.id) === String(id) ? el.__xsCachePipeline : { id: id, name: "" };
      var refresh = function () { load(id, lastRepaint[id], true); };
      if (action === "reload") refresh();
      else if (action === "edit") { if (root.XSFormKit) cacheForm(cur, st.data, refresh); }
      else if (action === "clear") { if (root.XSFormKit) clearForm(cur, refresh); }
    });
  }
  function track(el, pl) { if (el) el.__xsCachePipeline = pl; }

  root.XSCICDDetect = { detect: detect, summary: summary, statusText: statusText, errorText: errorText, createPipeline: createPipeline, LANG: LANG, TOOL: TOOL };
  root.XSCICDCache = {
    sectionHTML: sectionHTML, runFactHTML: runFactHTML,
    bind: function (el, pl, repaint) { track(el, pl); bind(el, pl, repaint); },
    refresh: function (id, repaint) { load(id, repaint || lastRepaint[id], true); },
    DEFAULT_PATHS: DEFAULT_PATHS
  };
})(window);
