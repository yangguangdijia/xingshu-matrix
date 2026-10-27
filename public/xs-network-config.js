(function () {
  "use strict";
  function E(v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; });
  }
  function summary(object) {
    var spec = object.spec || {}, meta = object.metadata || {};
    if (object.kind === "Ingress") return (spec.rules || []).map(function (r) {
      return (r.host || "*") + ": " + ((r.http || {}).paths || []).map(function (p) {
        var service = (p.backend || {}).service || {};
        return p.path + " → " + (service.name || "Resource backend") + ":" + ((service.port || {}).number || (service.port || {}).name || "");
      }).join(", ");
    }).join("; ");
    if (object.kind === "ApisixRoute") return (spec.http || []).map(function (route) {
      var match = route.match || {}, backends = route.backends || [];
      var hosts = match.hosts || [], paths = match.paths || [];
      return (hosts.length ? hosts.join(", ") : "*") + " " + (paths.length ? paths.join(", ") : "/*") +
        (backends.length ? " → " + backends.map(function (backend) {
          return (backend.serviceName || "Service") + ":" + (backend.servicePort || "");
        }).join(", ") : "");
    }).join("; ") || "未配置 HTTP 路由";
    if (object.kind === "Namespace") {
      var labels = meta.labels || {};
      return "Istio injection: " + (labels["istio-injection"] || "未设置") + "   revision: " + (labels["istio.io/rev"] || "未设置");
    }
    return (spec.hostnames || spec.hosts || []).join(", ") || spec.controllerName || spec.controller ||
      (spec.mtls && spec.mtls.mode) || JSON.stringify(spec).slice(0, 180);
  }
  function resourceName(resource) {
    return resource.kind === "ApisixRoute" ? "APISIX 路由 CRD（ApisixRoute）" : resource.kind;
  }
  function unavailableMessage(resource) {
    var states = {
      not_installed: "集群未安装或未提供该版本 API",
      forbidden: "集群凭据无权读取该资源",
      unavailable: "资源 API 读取失败"
    };
    var label = states[resource.state] || "资源不可用";
    return label + (resource.error ? "：" + resource.error : "");
  }
  function view(object) {
    dlg(E(object.kind + "   " + object.metadata.name), '<pre class="x-mono" style="max-height:60vh;overflow:auto;white-space:pre-wrap;word-break:break-word">' +
      E(JSON.stringify(object, null, 2)) + '</pre>', '<button class="x-btn" data-close type="button">关闭</button>');
  }
  function apisixTemplate(apiVersion, className) {
    return {
      apiVersion: apiVersion,
      kind: "ApisixRoute",
      metadata: { name: "", namespace: "default" },
      spec: {
        ingressClassName: className || "",
        http: [{
          name: "route-1",
          match: { hosts: ["example.com"], paths: ["/*"] },
          backends: [{ serviceName: "", servicePort: 80 }]
        }]
      }
    };
  }
  function editor(ctx, object, done, apisixApiVersion, apisixClassName) {
    var copy = object ? JSON.parse(JSON.stringify(object)) : {
      apiVersion: "networking.k8s.io/v1", kind: "Ingress", metadata: { name: "", namespace: "default" },
      spec: { ingressClassName: "", rules: [{ host: "", http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "", port: { number: 80 } } } }] } }] }
    };
    delete copy.status;
    if (copy.metadata) delete copy.metadata.managedFields;
    var kindField = !object && apisixApiVersion
      ? '<label><span>入口类型</span><select class="x-select" name="routeKind"><option value="ingress">Kubernetes Ingress</option><option value="apisix">APISIX ApisixRoute</option></select></label>'
      : "";
    dlg(object ? "编辑流量配置" : "创建流量配置", '<form id="netConfigForm" class="x-form">' +
      '<p class="x-desc">' + E(ctx.cluster.name) + '   ' + E(ctx.cluster.env || "") + '</p>' +
      kindField +
      '<label><span>YAML / JSON</span><textarea class="x-input x-mono" name="manifest" rows="18" required style="resize:vertical">' +
      E(JSON.stringify(copy, null, 2)) + '</textarea></label>' +
      '<label><span>变更原因</span><input class="x-input" name="reason" required maxlength="500"></label>' +
      '<label style="display:flex;gap:8px;align-items:flex-start"><input type="checkbox" name="confirm" required>已核对目标集群、命名空间、路由和业务影响</label>' +
      '<p class="x-frac">APISIX 路由按集群实际发现的 ApisixRoute API 提交；CRD 可用不代表控制器或网关已就绪。变更遵守环境审批，平台不会自动安装网关或签发证书。</p>' +
      '<p class="x-aerr" data-net-error role="alert" hidden></p></form>',
      '<button class="x-btn" data-close type="button">取消</button><button class="x-btn x-btn--pri" type="submit" form="netConfigForm">保存</button>');
    var form = document.getElementById("netConfigForm");
    var routeKind = form.elements.routeKind;
    if (routeKind) routeKind.onchange = function () {
      var value = routeKind.value === "apisix" ? apisixTemplate(apisixApiVersion, apisixClassName) : {
        apiVersion: "networking.k8s.io/v1", kind: "Ingress", metadata: { name: "", namespace: "default" },
        spec: { ingressClassName: "", rules: [{ host: "", http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "", port: { number: 80 } } } }] } }] }
      };
      form.elements.manifest.value = JSON.stringify(value, null, 2);
    };
    form.onsubmit = async function (event) {
      event.preventDefault();
      if (!form.reportValidity()) return;
      var button = document.querySelector('[form="netConfigForm"]'), error = form.querySelector("[data-net-error]");
      button.disabled = true; error.hidden = true;
      try {
        var result = await XS.post("/clusters/" + encodeURIComponent(ctx.cluster.id) + "/apply?reason=" + encodeURIComponent(form.elements.reason.value.trim()),
          { yaml: form.elements.manifest.value, namespace: object && object.metadata.namespace || "default", force: false });
        if (result && result.failed) throw new Error((result.objects || []).filter(function (o) { return o.status === "failed"; }).map(function (o) { return o.name + ": " + o.error; }).join("; "));
        closeAll(); toast("配置已保存"); done();
      } catch (err) {
        if (err.code === 40304) { closeAll(); toast("变更已提交环境审批"); done(); }
        else { error.textContent = err.message || "保存失败"; error.hidden = false; button.disabled = false; }
      }
    };
  }
  async function render(el, pg, page, ctx) {
    var mode = pg.k;
    var root = "/clusters/" + encodeURIComponent(ctx.cluster.id) + "/network/";
    try {
      var data = await XS.get(root + "configuration?view=" + encodeURIComponent(mode));
      var allResources = data.resources || [], objects = [];
      var resources = pg.k === "expose" ? allResources.filter(function (r) {
        return r.kind === "Ingress" || r.kind === "ApisixRoute";
      }) : allResources;
      var apisixResource = resources.find(function (r) { return r.kind === "ApisixRoute" && r.available; });
      var apisixClass = allResources.find(function (r) { return r.kind === "IngressClass" && r.available; });
      var apisixClassName = apisixClass && (apisixClass.items || []).find(function (item) {
        return (((item.spec || {}).controller || "").indexOf("apisix.apache.org/") === 0);
      });
      var body = '<div class="x-tbar"><span class="x-frac">' + E(ctx.cluster.name) + '   ' +
        (pg.k === "expose" ? "Ingress 与 APISIX 域名入口" : "已安装组件的实时资源") + '</span>' +
        '<button class="x-btn" type="button" data-net-refresh style="margin-left:auto">刷新</button>' +
        '<button class="x-btn x-btn--pri" type="button" data-net-create>' +
        (pg.k === "expose" ? "+ 创建入口" : "+ 创建配置") + '</button></div>';
      if (pg.k === "expose") {
        body += '<p class="x-frac" style="margin:8px 0 12px">此处仅管理 Ingress 和集群已安装的 APISIX ApisixRoute 域名入口，不列出 Service 清单。APISIX 是 API 网关及 Ingress Controller；NetworkPolicy 定义 Pod 的 L3/L4 入站、出站规则，不负责域名发布，实际拦截效果取决于集群 CNI。ApisixRoute CRD 可用也不代表控制器已就绪。</p>';
      }
      resources.forEach(function (r) {
        body += '<h2 class="x-h2" style="margin-top:16px">' + E(resourceName(r)) + '</h2>';
        if (!r.available) {
          body += '<p class="' + (r.state === "not_installed" ? "x-frac" : "x-aerr") + '">' + E(unavailableMessage(r)) + '</p>';
          return;
        }
        var rows = (r.items || []).map(function (o) {
          var index = objects.push(o) - 1, readOnly = r.cluster_scoped;
          return [E(o.metadata.namespace || "集群级"), E(o.metadata.name), E(summary(o)),
            '<button class="x-btn" data-net-view="' + index + '" type="button">查看</button>' +
            (readOnly ? "" : ' <button class="x-btn" data-net-edit="' + index + '" type="button">编辑</button>')];
        });
        body += rows.length ? tbl(["命名空间", "名称", "配置", "操作"], rows) : '<p class="x-empty">该 API 可用，暂无资源。</p>';
        if (r.truncated) body += '<p class="x-frac">仅显示前 1000 条，请使用命名空间筛选 API 进一步查询。</p>';
      });
      body += '<p class="x-frac" style="margin-top:12px">仅展示实际配置，不据此推断真实流量、策略最终效果、控制器已就绪或证书已签发。</p>';
      page.draw(ctx, body);
      if (page.rev !== pageRenderRev || current() !== "traffic") return;
      el.querySelector("[data-net-refresh]").onclick = function () { renderTrafficLive(el); };
      el.querySelector("[data-net-create]").onclick = function () {
        editor(ctx, null, function () { renderTrafficLive(el); },
          apisixResource && apisixResource.api_version,
          apisixClassName && apisixClassName.metadata.name);
      };
      el.querySelectorAll("[data-net-view]").forEach(function (b) { b.onclick = function () { view(objects[Number(b.dataset.netView)]); }; });
      el.querySelectorAll("[data-net-edit]").forEach(function (b) { b.onclick = function () { editor(ctx, objects[Number(b.dataset.netEdit)], function () { renderTrafficLive(el); }); }; });
    } catch (err) {
      page.draw(ctx, '<p class="x-aerr" role="alert">读取流量配置失败：' + E(err.message || "未知错误") + '</p>');
    }
  }
  window.XSNetworkConfig = { render: render, summary: summary };
})();
