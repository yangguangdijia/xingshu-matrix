/*
 * CI/CD 表单（基于 XSFormKit）：凭证、从代码仓库创建应用（分步向导）、Jenkins 接入。
 * 对标 KubeSphere「DevOps 凭证」与 Rainbond「从源码创建」：
 *   凭证统一保存（Git Token / 账号密码 / SSH 私钥 / 镜像仓库），应用与构建阶段按名称引用；
 *   填写仓库地址后「读取分支」直接列出远端分支并验证凭证是否可用。
 */
(function (root) {
  "use strict";

  var CRED_TYPES = [
    { v: "git_token", t: "Git 访问令牌", d: "GitHub / GitLab / Gitee 的 Personal Access Token" },
    { v: "git_basic", t: "Git 账号密码", d: "HTTP(S) 克隆用的用户名与密码" },
    { v: "ssh_key", t: "SSH 私钥", d: "git@host:org/repo.git 形式的仓库" },
    { v: "registry", t: "镜像仓库", d: "Harbor / ACR / Docker Hub 等推送凭证" }
  ];
  var CRED_LABEL = {};
  CRED_TYPES.forEach(function (x) { CRED_LABEL[x.v] = x.t; });
  var DNS = "^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$";

  function xs() { return root.XS; }
  function toast(m) { if (root.XS && root.XS.toast) root.XS.toast(m); }
  function items(r, keys) {
    if (Array.isArray(r)) return r;
    for (var i = 0; r && i < keys.length; i++) if (Array.isArray(r[keys[i]])) return r[keys[i]];
    return [];
  }
  function esc(v) { return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
  function approvalPending(err) { return err && (err.status === 202 || err.code === 40304 || err.approval); }
  function errText(err) { return root.XSFormKit ? root.XSFormKit.errText(err) : (err && err.message) || "请求失败"; }

  /* ------------------------------------------------------------ 凭证 */

  function credentialForm(existing, done, presetType) {
    var t = existing ? existing.type : presetType || "git_token";
    return root.XSFormKit.open({
      title: existing ? "编辑凭证" : "新建凭证",
      subtitle: "密钥加密保存，任何接口都不会再返回明文；流水线运行时临时注入，运行结束即删除",
      scene: "cicd.credential." + (existing ? "update" : "create"),
      aiExample: "建一个 harbor 推送凭证叫 harbor-ci，仓库地址 harbor.example.com，用户 robot$ci",
      width: "md", okText: existing ? "保存" : "创建",
      values: existing ? { __existing: true, name: existing.name, type: existing.type, username: existing.username, host: existing.host, description: existing.description } : { type: t },
      sections: [
        { title: "类型", fields: [existing ? { k: "type", label: "类型", type: "select", required: true, options: CRED_TYPES.filter(function (x) { return x.v === t; }) }
          : { k: "type", type: "radio", options: CRED_TYPES, required: true }] },
        { title: "凭证信息", fields: [
          { k: "name", label: "名称", required: true, pattern: DNS, patternMsg: "小写字母、数字、中划线", placeholder: "gitlab-ci" },
          { k: "host", label: "镜像仓库地址", mono: true, placeholder: "harbor.example.com 或 10.0.0.5:5000", required: true,
            show: function (v) { return v.type === "registry"; }, pattern: "^[A-Za-z0-9.-]+(:[0-9]{1,5})?$", patternMsg: "只填主机名[:端口]，不带 https:// 与路径" },
          { k: "username", label: "用户名", mono: true, show: function (v) { return v.type !== "ssh_key"; },
            help: "Token 类可留空（默认 oauth2）；镜像仓库填推送账号" },
          { k: "secret", label: "密码 / Token", type: "password", show: function (v) { return v.type !== "ssh_key"; }, required: !existing,
            placeholder: existing ? "留空表示不修改" : "" },
          { k: "ssh", label: "SSH 私钥", type: "code", rows: 7, full: true, secret: true, show: function (v) { return v.type === "ssh_key"; },
            placeholder: existing ? "留空表示不修改\n-----BEGIN OPENSSH PRIVATE KEY-----" : "-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----",
            help: "建议使用只读部署密钥（Deploy Key）" },
          { k: "description", label: "描述", full: true, placeholder: "用途、负责人" }
        ] }
      ],
      toObject: function (v) {
        var o = { apiVersion: "cicd.xingshu.io/v1", kind: "Credential", metadata: { name: v.name || "" }, type: v.type, description: v.description || "" };
        if (v.type === "registry") o.host = v.host || "";
        if (v.type !== "ssh_key") { o.username = v.username || ""; o.secret = v.secret || ""; } else o.privateKey = v.ssh || "";
        return o;
      },
      fromObject: function (o) {
        if (existing && o.type && o.type !== existing.type) throw new Error("凭证类型不能修改");
        return { name: o.metadata && o.metadata.name || "", type: o.type, description: o.description || "", host: o.host || "", username: o.username || "", secret: o.secret || "", ssh: o.privateKey || "" };
      },
      validate: function (v) {
        if (v.type === "git_basic" && !v.username) return "账号密码类型需要填写用户名";
        if (v.type === "registry" && !v.username) return "镜像仓库凭证需要填写用户名";
        var secret = v.type === "ssh_key" ? v.ssh : v.secret;
        if (!existing && !secret) return v.type === "ssh_key" ? "请粘贴 SSH 私钥" : "请填写密码 / Token";
        if (v.type === "ssh_key" && secret && secret.indexOf("-----BEGIN") < 0) return "私钥格式不正确，应以 -----BEGIN 开头";
        return "";
      },
      submit: function (v, _o, ctx) {
        var body = { name: v.name, type: v.type, username: v.type === "ssh_key" ? "" : v.username || "", host: v.type === "registry" ? v.host : "", description: v.description || "" };
        var secret = v.type === "ssh_key" ? v.ssh : v.secret;
        if (secret) body.secret = secret;
        var req = existing ? xs().put("/cicd/credentials/" + encodeURIComponent(existing.id), body) : xs().post("/cicd/credentials", body);
        return req.then(function (r) {
          ctx.close(); toast(existing ? "凭证已更新" : "凭证已创建");
          if (done) done(r);
        });
      }
    });
  }
  // 「密码 / Token」标签随类型变化
  var _open = credentialForm;

  function credentialsPanelHTML(list, error) {
    var rows = (list || []).map(function (c) {
      return "<tr><td class=\"x-cicd-title\">" + esc(c.name) + "</td><td>" + esc(CRED_LABEL[c.type] || c.type) + "</td><td class=\"x-cicd-break\">" +
        esc(c.host || c.username || "—") + "</td><td>" + (c.used_by ? esc(c.used_by) + " 处" : "未使用") + "</td><td class=\"x-cicd-row-actions\">" +
        '<button class="x-btn" type="button" data-cred-edit="' + esc(c.id) + '">编辑</button><button class="x-btn" type="button" data-cred-del="' + esc(c.id) + '"' +
        (c.used_by ? ' disabled title="仍被应用或流水线引用"' : "") + ">删除</button></td></tr>";
    });
    return '<section class="x-cicd-section x-cicd-connection"><div class="x-cicd-section-head"><h2>凭证</h2><button class="x-btn" type="button" data-cred-new>新建</button></div>' +
      (error ? '<p class="x-cicd-alert" role="status">' + esc(error) + "</p>" : "") +
      (rows.length ? '<div class="x-cicd-table-wrap"><table class="x-table x-cicd-table x-cicd-table--compact"><thead><tr><th>名称</th><th>类型</th><th>仓库 / 用户</th><th>引用</th><th>操作</th></tr></thead><tbody>' + rows.join("") + "</tbody></table></div>"
        : '<p class="x-cicd-empty">还没有凭证。私有代码仓库和镜像仓库需要先在这里保存凭证。</p>') + "</section>";
  }
  function bindCredentials(box, list, reload) {
    var byId = function (id) { return (list || []).filter(function (c) { return String(c.id) === String(id); })[0]; };
    var n = box.querySelector("[data-cred-new]");
    if (n) n.onclick = function () { credentialForm(null, reload); };
    [].forEach.call(box.querySelectorAll("[data-cred-edit]"), function (b) { b.onclick = function () { credentialForm(byId(b.getAttribute("data-cred-edit")), reload); }; });
    [].forEach.call(box.querySelectorAll("[data-cred-del]"), function (b) {
      b.onclick = function () {
        var c = byId(b.getAttribute("data-cred-del"));
        if (!c || (root.confirm && !root.confirm("确定删除凭证「" + c.name + "」？"))) return;
        xs().del("/cicd/credentials/" + encodeURIComponent(c.id)).then(function () { toast("凭证已删除"); reload(); })
          .catch(function (err) { toast("删除失败：" + (/in use/.test(errText(err)) ? "仍被应用或流水线引用" : errText(err))); });
      };
    });
  }
  function loadCredentials(type) {
    return xs().get("/cicd/credentials", type ? { type: type } : undefined).then(function (r) { return items(r, ["items"]); });
  }

  /* ------------------------------------------------------------ 应用（从代码仓库创建） */

  function credOptions(list) {
    return list.map(function (c) { return { v: String(c.id), t: c.name + " · " + (CRED_LABEL[c.type] || c.type) }; });
  }

  function appForm(app, done, initial) {
    var editing = !!(app && app.id != null);
    var src = Object.assign({}, app || {}, initial || {});
    var envs = (initial && initial.envs || src.envs || []).map(function (e) {
      return { name: e.name || "", target_id: e.target_id != null ? String(e.target_id) : "", namespace: e.namespace || "", mode: e.helm && e.helm.chart ? "helm" : "workload",
        deployment: e.deployment || "", container: e.container || "", chart: e.helm && e.helm.chart || "", release: e.helm && e.helm.release || "",
        repo: e.helm && e.helm.repo || "", version: e.helm && e.helm.version || "" };
    });
    if (!envs.length) envs = [{ name: "test", mode: "workload", namespace: "", deployment: "", container: "app" }];
    var values = {
      name: src.name || "", repo_url: src.repo_url || "", credential_id: src.credential_id ? String(src.credential_id) : "",
      branch: src.branch || "main", dockerfile: src.dockerfile || "Dockerfile", context_dir: src.context_dir || ".", image_repo: src.image_repo || "", envs: envs
    };
    var legacyToken = editing && app.git_token;
    var targets = [], envField = null;
    var kit = root.XSFormKit.open({
      title: editing ? "编辑应用" : "从代码仓库创建应用",
      subtitle: "代码源 → 构建 → 部署环境，完成后可一键生成流水线",
      scene: "cicd.application." + (editing ? "update" : "create"),
      aiExample: "接入 https://git.example.com/shop/web.git 的 develop 分支，镜像推到 harbor.example.com/shop/web，部署到 test 环境 shop-test 命名空间的 web Deployment",
      aiContext: function () { return "可选执行集群：" + JSON.stringify(targets.map(function (t) { return { id: t.id, name: t.name, env: t.env }; })); },
      wizard: true, width: "lg", okText: editing ? "保存" : "创建",
      values: values,
      sections: [
        { title: "代码源", desc: "支持 https:// 、git:// 和 git@host:org/repo.git（SSH 私钥凭证）。私有仓库先选凭证，再读取分支验证连通性。", fields: [
          { k: "name", label: "应用名称", required: true, pattern: DNS, patternMsg: "小写字母、数字、中划线，≤63", placeholder: "shop-web", readonly: editing },
          { k: "credential_id", label: "Git 凭证", type: "select", empty: "公开仓库（无需凭证）", options: [], load: function () { return loadCredentials("git").then(credOptions); },
            help: legacyToken ? "当前使用旧版内嵌 Token；选择凭证后以凭证为准" : "私有仓库需要凭证，可在右侧直接新建", button: { label: "新建凭证", run: function (v, api) {
              credentialForm(null, function (c) { loadCredentials("git").then(function (l) { api.setOptions("credential_id", credOptions(l)); if (c && c.id) api.set({ credential_id: String(c.id) }); }); }, "git_token");
            } } },
          { k: "repo_url", label: "仓库地址", required: true, full: true, mono: true, placeholder: "https://gitlab.example.com/team/web.git",
            check: function (s) { return /^(https?:\/\/|git:\/\/|ssh:\/\/|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:)/.test(s) ? (/\/\/[^/]*@/.test(s) ? "地址里不要带账号密码，请使用凭证" : "") : "需以 https://、git:// 或 git@ 开头"; } },
          { k: "branch", label: "默认分支", required: true, mono: true, options: [],
            button: { label: "读取分支", run: function (v, api) { return probe(v, api); } }, help: "可直接输入，或点「读取分支」从远端加载并验证凭证" }
        ] },
        { title: "构建", desc: "流水线的「镜像构建」阶段用 Kaniko 在集群内构建，无需 Docker 守护进程。", fields: [
          { k: "image_repo", label: "镜像仓库", required: true, full: true, mono: true, placeholder: "harbor.example.com/shop/web",
            help: "不含 tag；推送凭证在流水线构建阶段选择", check: function (s) { return /^https?:\/\//.test(s) ? "不要带 http(s)://" : /:[^/]*$/.test(s.split("/").pop()) ? "不要带 tag" : ""; } },
          { k: "dockerfile", label: "Dockerfile 路径", mono: true, required: true },
          { k: "context_dir", label: "构建上下文目录", mono: true, required: true }
        ] },
        { title: "部署环境", desc: "每个环境对应一个执行集群里的命名空间；部署方式二选一：更新已有 Deployment 的镜像，或升级 Helm Release。", fields: [
          envField = { k: "envs", type: "list", itemTitle: "环境", addText: "添加环境", newItem: { mode: "workload", container: "app" }, item: [
            { k: "name", label: "环境名", type: "select", required: true, options: ["dev", "test", "staging", "prod"].map(function (x) { return { v: x, t: x }; }) },
            { k: "target_id", label: "执行集群", type: "select", required: true, options: [] },
            { k: "namespace", label: "命名空间", required: true, mono: true, placeholder: "shop-test" },
            { k: "mode", label: "部署方式", type: "select", required: true, options: [{ v: "workload", t: "更新 Deployment 镜像" }, { v: "helm", t: "Helm Release" }] },
            { k: "deployment", label: "Deployment", mono: true, placeholder: "web", show: function (e) { return e.mode !== "helm"; } },
            { k: "container", label: "容器名", mono: true, placeholder: "app", show: function (e) { return e.mode !== "helm"; } },
            { k: "chart", label: "Helm Chart", mono: true, placeholder: "bitnami/nginx", show: function (e) { return e.mode === "helm"; } },
            { k: "release", label: "Release", mono: true, show: function (e) { return e.mode === "helm"; } },
            { k: "repo", label: "Chart 仓库", mono: true, placeholder: "https://charts.example.com", show: function (e) { return e.mode === "helm"; } },
            { k: "version", label: "Chart 版本", mono: true, show: function (e) { return e.mode === "helm"; } }
          ] }
        ], validate: function (v) {
          if (!v.envs || !v.envs.length) return "至少添加一个部署环境";
          for (var i = 0; i < v.envs.length; i++) {
            var e = v.envs[i], n = "环境 " + (i + 1) + "：";
            if (!e.name || !e.target_id || !e.namespace) return n + "环境名、执行集群、命名空间必填";
            if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(e.namespace)) return n + "命名空间格式不正确";
            var tg = targets.filter(function (t) { return String(t.id) === String(e.target_id); })[0];
            if (tg && tg.env && tg.env !== e.name) return n + "执行集群「" + tg.name + "」属于 " + tg.env + " 环境，环境名需一致（或换一个执行集群）";
            if (e.mode === "helm" ? !(e.chart && e.release) : !(e.deployment && e.container)) return n + (e.mode === "helm" ? "Helm 方式需要 Chart 与 Release" : "需要 Deployment 与容器名");
          }
          return "";
        } }
      ],
      toObject: function (v) {
        return { apiVersion: "cicd.xingshu.io/v1", kind: "Application", metadata: { name: v.name },
          spec: { source: { repoURL: v.repo_url, branch: v.branch, credential: credName(v.credential_id) },
            build: { image: v.image_repo, dockerfile: v.dockerfile, context: v.context_dir },
            environments: (v.envs || []).map(function (e) {
              var o = { name: e.name, target: targetName(e.target_id), namespace: e.namespace };
              if (e.mode === "helm") o.helm = { chart: e.chart, release: e.release, repo: e.repo || undefined, version: e.version || undefined };
              else { o.deployment = e.deployment; o.container = e.container; }
              return JSON.parse(JSON.stringify(o));
            }) } };
      },
      fromObject: function (o, v) {
        var s = o.spec || {}, so = s.source || {}, b = s.build || {};
        return { name: o.metadata && o.metadata.name || v.name, repo_url: so.repoURL || "", branch: so.branch || "main", credential_id: credId(so.credential),
          image_repo: b.image || "", dockerfile: b.dockerfile || "Dockerfile", context_dir: b.context || ".",
          envs: (s.environments || []).map(function (e) {
            var h = e.helm || {};
            return { name: e.name || "", target_id: targetId(e.target), namespace: e.namespace || "", mode: e.helm ? "helm" : "workload", deployment: e.deployment || "", container: e.container || "",
              chart: h.chart || "", release: h.release || "", repo: h.repo || "", version: h.version || "" };
          }) };
      },
      submit: function (v, _o, ctx) {
        var body = {
          name: v.name, repo_url: v.repo_url, branch: v.branch, dockerfile: v.dockerfile, context_dir: v.context_dir, image_repo: v.image_repo,
          git_username: editing ? app.git_username || "" : "", credential_id: v.credential_id ? Number(v.credential_id) : 0,
          envs: v.envs.map(function (e) {
            var o = { name: e.name, target_id: Number(e.target_id), namespace: e.namespace };
            if (e.mode === "helm") { o.helm = { chart: e.chart, release: e.release }; if (e.repo) o.helm.repo = e.repo; if (e.version) o.helm.version = e.version; }
            else { o.deployment = e.deployment; o.container = e.container; }
            return o;
          })
        };
        var req = editing ? xs().put("/cicd/applications/" + encodeURIComponent(app.id), body) : xs().post("/cicd/applications", body);
        return req.then(function (r) {
          ctx.close();
          toast(editing ? "应用已更新" : "应用已创建");
          if (!editing && r && r.webhook_secret && root.XSFormKit) showWebhook(r);
          if (done) done(r);
        }, function (err) {
          if (approvalPending(err)) { ctx.close(); toast("已提交审批，审批通过后生效"); if (done) done(null); return; }
          throw err;
        });
      }
    });
    var credCache = [];
    loadCredentials("git").then(function (l) { credCache = l; }).catch(function () {});
    function credName(id) { var c = credCache.filter(function (x) { return String(x.id) === String(id); })[0]; return c ? c.name : id ? "#" + id : ""; }
    function credId(name) { if (!name) return ""; var c = credCache.filter(function (x) { return x.name === name || "#" + x.id === name; })[0]; if (!c) throw new Error("找不到凭证「" + name + "」"); return String(c.id); }
    function targetName(id) { var t = targets.filter(function (x) { return String(x.id) === String(id); })[0]; return t ? t.name : id ? "#" + id : ""; }
    function targetId(name) { if (!name) return ""; var t = targets.filter(function (x) { return x.name === name || "#" + x.id === name; })[0]; if (!t) throw new Error("找不到执行集群「" + name + "」"); return String(t.id); }

    xs().get("/cicd/targets").then(function (r) {
      targets = items(r, ["items", "targets"]);
      var envs = [];
      targets.forEach(function (t) { if (t.env && envs.indexOf(t.env) < 0) envs.push(t.env); });
      envField.item.forEach(function (sub) {
        if (sub.k === "target_id") sub.options = targets.map(function (t) { return { v: String(t.id), t: t.name + (t.env ? " (" + t.env + ")" : "") }; });
        if (sub.k === "name" && envs.length) sub.options = envs.map(function (x) { return { v: x, t: x }; });
      });
      if (targets[0]) {
        envField.newItem = Object.assign({}, envField.newItem, { name: targets[0].env, target_id: String(targets[0].id) });
        var cur = kit.values();
        (cur.envs || []).forEach(function (e) { if (!editing && (!e.target_id || e.name === "test") && !e.namespace) { e.target_id = String(targets[0].id); e.name = targets[0].env || e.name; } });
      }
      if (!targets.length) kit.status("还没有执行集群：请先在「流水线 → 执行集群」接入一个已注册集群。");
      kit.set({});
    }).catch(function (err) { kit.status("执行集群加载失败：" + errText(err)); });
    return kit;

    function probe(v, api) {
      if (!v.repo_url) { api.status("先填写仓库地址"); return Promise.resolve(); }
      api.status("正在读取远端分支…", true);
      return xs().post("/cicd/repos/probe", { repo_url: v.repo_url, credential_id: v.credential_id ? Number(v.credential_id) : 0, app_id: editing ? app.id : 0 }).then(function (r) {
        var list = r && r.branches || [];
        if (!list.length) { api.status("仓库可访问，但还没有任何分支"); return; }
        var cur = list.indexOf(v.branch) >= 0 ? v.branch : r.default_branch || list[0];
        api.setOptions("branch", list.map(function (b) { return { v: b, t: b === r.default_branch ? "默认分支" : "" }; }));
        api.set({ branch: cur });
        api.status("连接成功：" + list.length + " 个分支，默认 " + (r.default_branch || "—") + (r.head ? "，最新提交 " + r.head.slice(0, 8) : ""), true);
      }).catch(function (err) {
        var m = errText(err);
        api.status(/authentication/i.test(m) ? "认证失败：请检查所选凭证是否有该仓库的读取权限" : /SSH/.test(m) ? "SSH 仓库无法在服务端预读分支，运行时会用私钥克隆；请手动填写分支名" : "读取失败：" + m);
      });
    }
  }

  function showWebhook(r) {
    root.XSFormKit.open({
      title: "Webhook 密钥（仅显示一次）", width: "md", ai: false, okText: "我已保存", noCancel: true,
      sections: [{ desc: "在代码仓库的 Webhook 设置里填写以下地址与密钥，推送代码即可触发开启了「代码推送触发」的流水线。", fields: [
        { k: "url", label: "Webhook 地址", full: true, mono: true, readonly: true },
        { k: "secret", label: "Secret", full: true, mono: true, readonly: true }
      ] }],
      values: { url: location.origin + (r.webhook_url || "/api/v1/cicd/webhooks/" + r.id), secret: r.webhook_secret },
      submit: function (_v, _o, ctx) { ctx.close(); }
    });
  }

  root.XSCICDForms = {
    credentialForm: _open, credentialsPanelHTML: credentialsPanelHTML, bindCredentials: bindCredentials, loadCredentials: loadCredentials,
    appForm: appForm, CRED_LABEL: CRED_LABEL
  };
})(window);
