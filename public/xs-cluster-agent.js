// Agent-mode cluster onboarding: create the registry entry, then show the
// generated manifest the user applies to the member cluster.
(function (root) {
  "use strict";
  var NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function api() {
    if (!root.XS || typeof root.XS.api !== "function") throw new Error("平台接口尚未就绪");
    return root.XS;
  }
  function isApproval(err) {
    return !!(err && (Number(err.code) === 40304 || (root.XS && typeof root.XS.isApproval === "function" && root.XS.isApproval(err))));
  }
  function ticketOf(err) {
    var d = err && (err.approval || err.details || err.data) || {};
    return d.ticket_id || d.approval_id || (d.ticket && d.ticket.id) || "";
  }

  // validateCreate mirrors the server rules so mistakes surface before submission.
  function validateCreate(input) {
    var name = String(input && input.name || "").trim(), env = String(input && input.env || "").trim();
    if (!NAME.test(name) || name.length > 63) throw new Error("集群名称只能包含小写字母、数字和中划线，且不超过 63 个字符");
    if (!env || env === "+") throw new Error("请选择环境");
    return { name: name, env: env, description: String(input && input.description || "").trim() };
  }

  // manifestRequest validates the platform address the agent will dial.
  function manifestRequest(values) {
    var raw = String(values && values.server_url || "").trim().replace(/\/+$/, ""), parsed;
    try { parsed = new URL(raw); } catch (e) { throw new Error("平台地址无效，例如 https://matrix.example.com"); }
    var insecure = !!(values && values.allow_insecure_http);
    if (parsed.username || parsed.password || parsed.search || parsed.hash || (parsed.pathname && parsed.pathname !== "/")) {
      throw new Error("平台地址不能包含路径、凭据、查询或片段");
    }
    if (parsed.protocol !== "https:" && !(parsed.protocol === "http:" && insecure)) {
      throw new Error("平台地址应使用 HTTPS；仅内网测试可勾选允许 HTTP");
    }
    return {
      server_url: parsed.protocol + "//" + parsed.host,
      image: String(values && values.image || "").trim(),
      namespace: String(values && values.namespace || "").trim(),
      enable_cicd: !!(values && values.enable_cicd),
      allow_insecure_http: parsed.protocol === "http:" && insecure
    };
  }

  function create(input) {
    var body;
    try { body = validateCreate(input); } catch (err) { return Promise.reject(err); }
    return api().api("/clusters/agent", { method: "POST", body: body }).then(function (cluster) {
      return { cluster: cluster, pending: false };
    }, function (err) {
      if (isApproval(err)) return { cluster: null, pending: true, ticket: ticketOf(err) };
      throw err;
    });
  }

  function showDialog(title, body, foot) {
    if (typeof root.dlg === "function") return root.dlg(title, body, foot);
    throw new Error("对话框组件不可用");
  }

  function targetContext(value) {
    var context = String(value || "").trim();
    if (!context || /[\u0000-\u001f\u007f]/.test(context)) throw new Error("请填写已核对的目标集群 context，不能包含控制字符");
    return context;
  }

  function shellArg(value) {
    return "'" + String(value).replace(/'/g, "'\\''") + "'";
  }

  function manifestFile(cluster) {
    if (!/^\d+$/.test(String(cluster.id))) throw new Error("集群登记编号无效，请刷新后重试");
    return "matrix-cluster-agent-" + cluster.id + ".yaml";
  }

  function installCommands(cluster, context, namespace) {
    var kubectl = "kubectl --context=" + shellArg(targetContext(context));
    return kubectl + " cluster-info\n" +
      kubectl + " get namespace kube-system -o jsonpath='{.metadata.uid}'\n" +
      kubectl + " apply -f " + shellArg(manifestFile(cluster)) + "\n" +
      kubectl + " -n " + shellArg(namespace || "matrix-system") + " rollout status deployment/matrix-cluster-agent";
  }

  function clusterIdentity(data) {
    if (!data || data.exit_code !== 0 || data.truncated) throw new Error("未取得完整的集群身份信息");
    var namespace;
    try { namespace = JSON.parse(data.output); } catch (err) { throw new Error("集群身份响应不是有效的资源数据"); }
    if (!namespace || namespace.kind !== "Namespace" || !namespace.metadata || namespace.metadata.name !== "kube-system" ||
        typeof namespace.metadata.uid !== "string" || !namespace.metadata.uid.trim()) throw new Error("未取得 kube-system 命名空间 UID");
    return namespace.metadata.uid;
  }

  // openManifest shows the form; generating rotates the join token, so it is explicit.
  function openManifest(cluster, options) {
    options = options || {};
    var origin = root.location && root.location.origin || "";
    var rotateNote = '<p class="x-warn x-full" data-xca-rotation style="margin:0"' + (cluster.agent_bound || cluster.agent_online ? '' : ' hidden') +
      '>重新生成会使旧令牌失效并断开现有代理连接，需在同一目标集群重新应用清单。</p>';
    var body = '<form class="x-form" id="xca-form" novalidate>' + rotateNote +
      '<p class="x-frac x-full" style="margin:0">平台登记 #' + esc(cluster.id) + ' · ' + esc(cluster.name || "") + '。名称只是登记标签，不会自动发现或选择目标集群。</p>' +
      '<label class="x-full"><span class="x-req">1 · 目标集群 context</span><input class="x-input x-mono" name="target_context" autocomplete="off" spellcheck="false" placeholder="填写已核对的 kubectl context" /></label>' +
      '<details class="x-full"><summary>如何确认安装目标</summary><pre class="x-log" style="overflow:auto">kubectl config get-contexts\nkubectl config current-context</pre>' +
      '<p class="x-frac">代理程序使用集群内 ServiceAccount（InClusterConfig），只能代理它所在的集群。必须在目标集群的 context 下安装，不能仅在平台所在集群运行。</p></details>' +
      '<label class="x-full"><span class="x-req">2 · 平台地址（代理回连地址）</span><input class="x-input" name="server_url" value="' + esc(origin) + '" autocomplete="off" spellcheck="false" /></label>' +
      '<p class="x-frac x-full" style="margin:0">填写目标集群可访问的平台地址，不是 Kubernetes API 地址。请保持 TLS 证书校验。</p>' +
      '<label><span>代理镜像（留空使用默认）</span><input class="x-input x-mono" name="image" autocomplete="off" spellcheck="false" placeholder="由平台提供默认镜像" /></label>' +
      '<label><span>代理命名空间</span><input class="x-input" name="namespace" value="matrix-system" autocomplete="off" /></label>' +
      '<label class="x-full"><span><input type="checkbox" name="enable_cicd" /> 同时创建 CI/CD 部署账号（matrix-cicd-deployer）</span></label>' +
      '<label class="x-full"><span><input type="checkbox" name="allow_insecure_http" /> 允许 HTTP 平台地址（仅限内网测试）</span></label>' +
      '<div class="x-full" data-xca-result aria-live="polite"></div>' +
      '<p class="x-aerr x-full" role="alert" data-xca-err hidden></p></form>';
    var foot = '<button class="x-btn" data-close type="button">关闭</button><button class="x-btn x-btn--pri" type="submit" form="xca-form" data-xca-gen>生成接入清单</button>';
    showDialog("Agent 接入   " + esc(cluster.name || ""), body, foot);
    var doc = root.document, form = doc && doc.getElementById("xca-form");
    if (!form) return;
    var errBox = form.querySelector("[data-xca-err]"), result = form.querySelector("[data-xca-result]");
    var generate = doc.querySelector("[data-xca-gen]"), busy = false, generation = 0;
    function fail(message) { errBox.textContent = message; errBox.hidden = false; }
    function active() { return doc.getElementById("xca-form") === form; }
    form.onsubmit = function (event) {
      event.preventDefault();
      if (busy) return;
      var payload, context, filename;
      try {
        context = targetContext(form.elements.target_context.value);
        filename = manifestFile(cluster);
        payload = manifestRequest({
          server_url: form.elements.server_url.value, image: form.elements.image.value, namespace: form.elements.namespace.value,
          enable_cicd: form.elements.enable_cicd.checked, allow_insecure_http: form.elements.allow_insecure_http.checked
        });
      } catch (err) { return fail(err.message); }
      errBox.hidden = true;
      busy = true;
      generation += 1;
      result.innerHTML = "";
      if (generate) { generate.disabled = true; generate.textContent = "生成中…"; }
      api().api("/clusters/" + encodeURIComponent(cluster.id) + "/agent/manifest", { method: "POST", body: payload }).then(function (data) {
        if (!active()) return;
        if (!data || typeof data.manifest !== "string" || !data.manifest.trim()) throw new Error("接口未返回接入清单");
        if (data.cluster_id != null && String(data.cluster_id) !== String(cluster.id)) throw new Error("清单所属集群与登记编号不一致");
        var commands = installCommands(cluster, context, data.namespace || payload.namespace);
        result.innerHTML = '<strong>3 · 下载清单，在目标集群安装</strong>' +
          '<p class="x-frac">本次目标 context：<b>' + esc(context) + '</b>。平台回连地址：' + esc(data.server_url || payload.server_url) + '</p>' +
          '<p class="x-frac">清单含 Secret 接入令牌，禁止提交到 Git 或共享仓库。当前清单会授予代理 cluster-admin 权限，请核对安装目标与授权。</p>' +
          '<div class="x-tbar"><button class="x-btn x-btn--pri" type="button" data-xca-download>下载接入清单</button>' +
          '<button class="x-btn" type="button" data-xca-copy>复制清单</button><span class="x-frac">' + esc(filename) + '</span></div>' +
          '<details><summary>查看清单内容（含凭据）</summary><textarea class="x-input x-mono" rows="8" readonly spellcheck="false" data-xca-yaml>' + esc(data.manifest) + '</textarea></details>' +
          '<p class="x-frac">在下载目录执行以下命令（Bash）；先核对集群信息和 UID，再执行安装。</p>' +
          '<pre class="x-log" style="overflow:auto" data-xca-commands>' + esc(commands) + '</pre>' +
          '<div class="x-tbar"><strong>4 · 核对连接与集群身份</strong><button class="x-btn" type="button" data-xca-check>检查连接状态</button></div>' +
          '<p class="x-frac" data-xca-status role="status">清单已生成，尚未确认代理连接。</p>' +
          '<p class="x-frac" data-xca-identity role="status">接入后读取目标集群的 kube-system UID，与终端输出逐字核对；名称不能作为身份依据。</p>';
        bindResult(cluster, data, filename, generation);
        form.querySelector("[data-xca-rotation]").hidden = false;
        if (generate) generate.textContent = "重新生成（旧令牌失效）";
        if (options.onGenerated) options.onGenerated(data);
      }).catch(function (err) {
        if (active()) fail(isApproval(err) ? "该操作需要审批，审批通过后再生成清单" : "生成失败：" + (err && err.message || "接口不可用"));
      }).then(function () {
        busy = false;
        if (generate) { generate.disabled = false; if (!result.innerHTML) generate.textContent = "重新生成接入清单"; }
      });
    };
    function bindResult(cl, data, filename, revision) {
      var area = result.querySelector("[data-xca-yaml]"), status = result.querySelector("[data-xca-status]");
      var identity = result.querySelector("[data-xca-identity]"), check = result.querySelector("[data-xca-check]");
      function current() { return active() && revision === generation; }
      function manualCopy() { area.closest("details").open = true; area.select(); status.textContent = "请手动复制清单"; }
      result.querySelector("[data-xca-copy]").onclick = function () {
        var nav = root.navigator;
        if (nav && nav.clipboard && nav.clipboard.writeText) {
          nav.clipboard.writeText(data.manifest).then(function () { if (current()) status.textContent = "清单已复制，仍需安装并检查连接"; }, function () { if (current()) manualCopy(); });
        } else { manualCopy(); }
      };
      result.querySelector("[data-xca-download]").onclick = function () {
        var blob = new root.Blob([data.manifest], { type: "application/yaml" });
        var link = doc.createElement("a");
        link.href = root.URL.createObjectURL(blob);
        link.download = filename;
        doc.body.appendChild(link); link.click(); link.remove();
        root.setTimeout(function () { root.URL.revokeObjectURL(link.href); }, 1000);
      };
      check.onclick = function () {
        if (check.disabled) return;
        check.disabled = true;
        status.textContent = "检查中…";
        identity.textContent = "尚未核对集群身份。";
        api().api("/clusters/" + encodeURIComponent(cl.id)).then(function (fresh) {
          if (!current()) return;
          if (!fresh || String(fresh.id) !== String(cl.id) || fresh.connect_mode !== "agent") throw new Error("集群登记信息不一致，请刷新后重试");
          status.textContent = (fresh.agent_online === true ? "代理已连接" : "代理尚未连接") +
            " · 登记 #" + fresh.id + " · " + (fresh.agent_bound ? "已绑定集群指纹" : "尚未绑定指纹") +
            " · 代理版本：" + (fresh.agent_version || "未上报") + " · 最近心跳：" + (fresh.agent_seen_at || "未上报");
          if (fresh.agent_online !== true) {
            identity.textContent = "等待代理连接，暂不能确认目标集群身份。";
            return;
          }
          if (typeof api().can !== "function" || !api().can("workload:read")) {
            identity.textContent = "连接已建立，但读取 kube-system UID 需要工作负载只读权限；请联系管理员核对目标集群，不能依据名称确认身份。";
            return;
          }
          identity.textContent = "正在通过代理读取目标集群身份…";
          return api().api("/clusters/" + encodeURIComponent(cl.id) + "/kubectl", {
            method: "POST", body: { command: "get namespace kube-system -o json" }
          }).then(function (response) {
            if (current()) identity.textContent = "实际 kube-system UID：" + clusterIdentity(response) + "。请与目标 context 的终端输出核对，一致后再确认接入。";
          }).catch(function (err) {
            if (current()) identity.textContent = "代理已连接，但集群身份核对失败：" + (err && err.message || "接口不可用") + "。请勿仅凭名称确认接入。";
          });
        }).catch(function (err) {
          if (current()) status.textContent = "读取状态失败：" + (err && err.message || "接口不可用");
        }).then(function () { if (current()) check.disabled = false; });
      };
    }
  }

  root.XSClusterAgent = { create: create, openManifest: openManifest, _internals: {
    validateCreate: validateCreate, manifestRequest: manifestRequest, targetContext: targetContext,
    manifestFile: manifestFile, installCommands: installCommands, clusterIdentity: clusterIdentity
  } };
})(window);
