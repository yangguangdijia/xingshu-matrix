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

  // openManifest shows the form; generating rotates the join token, so it is explicit.
  function openManifest(cluster, options) {
    options = options || {};
    var origin = root.location && root.location.origin || "";
    var rotateNote = cluster.agent_bound || cluster.agent_online
      ? '<p class="x-warn" style="display:block;margin:0">重新生成会使当前令牌失效，正在运行的 agent 会断开，需在目标集群重新 apply 新清单。</p>' : "";
    var body = '<form class="x-form" id="xca-form" novalidate>' + rotateNote +
      '<label class="x-full"><span class="x-req">平台地址（agent 回连地址）</span><input class="x-input" name="server_url" value="' + esc(origin) + '" autocomplete="off" spellcheck="false" /></label>' +
      '<label><span>agent 镜像（留空使用默认）</span><input class="x-input x-mono" name="image" autocomplete="off" spellcheck="false" placeholder="docker.io/xingshu/matrix-cluster-agent:版本" /></label>' +
      '<label><span>agent 命名空间</span><input class="x-input" name="namespace" value="matrix-system" autocomplete="off" /></label>' +
      '<label class="x-full"><span><input type="checkbox" name="enable_cicd" /> 同时创建 CI/CD 部署账号（matrix-cicd-deployer）</span></label>' +
      '<label class="x-full"><span><input type="checkbox" name="allow_insecure_http" /> 允许 HTTP 平台地址（仅限内网测试）</span></label>' +
      '<div class="x-full" data-xca-result aria-live="polite"></div>' +
      '<p class="x-aerr x-full" role="alert" data-xca-err hidden></p></form>';
    var foot = '<button class="x-btn" data-close type="button">关闭</button><button class="x-btn x-btn--pri" type="submit" form="xca-form" data-xca-gen>生成接入清单</button>';
    showDialog("Agent 接入   " + esc(cluster.name || ""), body, foot);
    var doc = root.document, form = doc && doc.getElementById("xca-form");
    if (!form) return;
    var errBox = form.querySelector("[data-xca-err]"), result = form.querySelector("[data-xca-result]");
    var generate = doc.querySelector("[data-xca-gen]"), busy = false;
    function fail(message) { errBox.textContent = message; errBox.hidden = false; }
    form.onsubmit = function (event) {
      event.preventDefault();
      if (busy) return;
      var payload;
      try {
        payload = manifestRequest({
          server_url: form.elements.server_url.value, image: form.elements.image.value, namespace: form.elements.namespace.value,
          enable_cicd: form.elements.enable_cicd.checked, allow_insecure_http: form.elements.allow_insecure_http.checked
        });
      } catch (err) { return fail(err.message); }
      errBox.hidden = true;
      busy = true;
      if (generate) { generate.disabled = true; generate.textContent = "生成中…"; }
      api().api("/clusters/" + encodeURIComponent(cluster.id) + "/agent/manifest", { method: "POST", body: payload }).then(function (data) {
        if (!data || typeof data.manifest !== "string") throw new Error("接口未返回接入清单");
        result.innerHTML = '<p class="x-frac" style="margin:0 0 6px">在目标集群执行：<code class="x-mono">' + esc(data.apply_command || "kubectl apply -f <文件>") + '</code>。清单含一次性令牌，只显示这一次，请勿提交到代码仓库。</p>' +
          '<textarea class="x-input x-mono" rows="12" readonly spellcheck="false" data-xca-yaml>' + esc(data.manifest) + '</textarea>' +
          '<div class="x-tbar" style="margin-top:6px"><button class="x-btn" type="button" data-xca-copy>复制清单</button>' +
          '<button class="x-btn" type="button" data-xca-download>下载 YAML</button><span class="x-grow"></span>' +
          '<button class="x-btn" type="button" data-xca-check>检查连接状态</button><span class="x-frac" data-xca-status role="status"></span></div>';
        bindResult(cluster, data);
        if (generate) generate.textContent = "重新生成";
        if (options.onGenerated) options.onGenerated(data);
      }).catch(function (err) {
        fail(isApproval(err) ? "该操作需要审批，审批通过后再生成清单" : "生成失败：" + (err && err.message || "接口不可用"));
      }).then(function () {
        busy = false;
        if (generate) generate.disabled = false;
      });
    };
    function bindResult(cl, data) {
      var area = result.querySelector("[data-xca-yaml]"), status = result.querySelector("[data-xca-status]");
      result.querySelector("[data-xca-copy]").onclick = function () {
        var nav = root.navigator;
        if (nav && nav.clipboard && nav.clipboard.writeText) {
          nav.clipboard.writeText(data.manifest).then(function () { status.textContent = "已复制"; }, function () { area.select(); status.textContent = "请手动复制"; });
        } else { area.select(); status.textContent = "请手动复制"; }
      };
      result.querySelector("[data-xca-download]").onclick = function () {
        var blob = new root.Blob([data.manifest], { type: "application/yaml" });
        var link = doc.createElement("a");
        link.href = root.URL.createObjectURL(blob);
        link.download = "matrix-cluster-agent-" + cl.id + ".yaml";
        doc.body.appendChild(link); link.click(); link.remove();
        root.setTimeout(function () { root.URL.revokeObjectURL(link.href); }, 1000);
      };
      result.querySelector("[data-xca-check]").onclick = function () {
        status.textContent = "检查中…";
        api().api("/clusters/" + encodeURIComponent(cl.id)).then(function (fresh) {
          status.textContent = fresh && fresh.agent_online ? "agent 已连接" + (fresh.agent_version ? "（" + fresh.agent_version + "）" : "") : "agent 尚未连接";
        }, function (err) { status.textContent = "读取状态失败：" + (err && err.message || "接口不可用"); });
      };
    }
  }

  root.XSClusterAgent = { create: create, openManifest: openManifest, _internals: { validateCreate: validateCreate, manifestRequest: manifestRequest } };
})(window);
