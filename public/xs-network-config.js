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
  function clone(value) { return JSON.parse(JSON.stringify(value)); }
  function objectKey(object) {
    var meta = object.metadata || {};
    return JSON.stringify([object.apiVersion, object.kind, meta.namespace || "", meta.name]);
  }
  function snapshotResources(data) {
    return (data.resources || []).map(function (resource) {
      var copy = clone(resource);
      copy.items = (copy.items || []).map(function (object) {
        object.kind = object.kind || copy.kind;
        object.apiVersion = object.apiVersion || copy.api_version;
        object.metadata = object.metadata || {};
        if (!copy.cluster_scoped && !object.metadata.namespace) object.metadata.namespace = "default";
        return object;
      });
      return copy;
    });
  }
  function view(object) {
    dlg(E(object.kind + "   " + object.metadata.name), '<pre class="x-mono" style="max-height:60vh;overflow:auto;white-space:pre-wrap;word-break:break-word">' +
      E(JSON.stringify(object, null, 2)) + '</pre>', '<button class="x-btn" data-close type="button">关闭</button>');
  }
  function ingressTemplate(className) {
    return {
      apiVersion: "networking.k8s.io/v1", kind: "Ingress",
      metadata: { name: "", namespace: "default" },
      spec: {
        ingressClassName: className || "",
        rules: [{ host: "", http: { paths: [{ path: "/", pathType: "Prefix", backend: { service: { name: "", port: { number: 80 } } } }] } }]
      }
    };
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
  function tlsSecret(spec, host) {
    var tls = (spec.tls || []).find(function (entry) {
      return host ? (entry.hosts || []).indexOf(host) >= 0 : !(entry.hosts || []).length;
    });
    return tls && tls.secretName || "";
  }
  function pathRows(object) {
    var spec = object.spec || {}, rows = [];
    if (object.kind === "Ingress") {
      (spec.rules || []).forEach(function (rule, ruleIndex) {
        ((rule.http || {}).paths || []).forEach(function (path, pathIndex) {
          var service = (path.backend || {}).service || {};
          rows.push({
            rule: ruleIndex, pathIndex: pathIndex, source: clone(path),
            host: rule.host || "", path: path.path || "/", pathType: path.pathType || "Prefix",
            tls: tlsSecret(spec, rule.host || ""),
            resource: (path.backend || {}).resource,
            backends: [{ service: service.name || "", port: String((service.port || {}).number || (service.port || {}).name || ""), source: clone(service) }]
          });
        });
      });
    } else {
      (spec.http || []).forEach(function (route, ruleIndex) {
        var match = route.match || {};
        (match.paths && match.paths.length ? match.paths : ["/*"]).forEach(function (path, pathIndex) {
          rows.push({
            rule: ruleIndex, pathIndex: pathIndex, host: (match.hosts || []).join(", "), path: path,
            pathType: "ImplementationSpecific", tls: "",
            backends: (route.backends || []).map(function (backend) {
              return { service: backend.serviceName || "", port: String(backend.servicePort || ""), source: clone(backend) };
            })
          });
        });
      });
    }
    return rows;
  }
  function validDNS(value, max) {
    return value.length <= max && value.split(".").every(function (part) {
      return part.length > 0 && part.length <= 63 && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(part);
    });
  }
  function servicePort(value) {
    if (/^\d+$/.test(value)) {
      var number = Number(value);
      if (number >= 1 && number <= 65535) return number;
    } else if (value.length <= 15 && /[a-z]/.test(value) && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(value)) return value;
    throw new Error("服务端口必须是 1–65535 的整数或有效端口名。");
  }
  function hostList(value, kind) {
    var hosts = value ? value.split(",").map(function (host) { return host.trim(); }) : [];
    if (kind === "Ingress" && hosts.length > 1) throw new Error("Ingress 每行只能填写一个域名。");
    hosts.forEach(function (host) {
      if (!validDNS(host.replace(/^\*\./, ""), 253) || /^\d+\.\d+\.\d+\.\d+$/.test(host)) throw new Error("域名格式无效：" + host);
    });
    return hosts;
  }
  function ingressTLS(base, originalRows, rows) {
    if (rows.length === originalRows.length && rows.every(function (row, i) {
      return row.host === originalRows[i].host && row.tls === originalRows[i].tls;
    })) return base.spec.tls;
    var oldHosts = new Set(originalRows.map(function (row) { return row.host; }));
    var entries = clone(base.spec.tls || []).filter(function (entry) {
      if (!(entry.hosts || []).length) return !oldHosts.has("");
      entry.hosts = entry.hosts.filter(function (host) { return !oldHosts.has(host); });
      return entry.hosts.length > 0;
    });
    var hosts = new Map();
    rows.forEach(function (row) {
      if (hosts.has(row.host) && hosts.get(row.host) !== row.tls) throw new Error("同一域名的 TLS Secret 必须一致。");
      hosts.set(row.host, row.tls);
    });
    hosts.forEach(function (secret, host) {
      if (!secret) return;
      var original = (base.spec.tls || []).find(function (entry) { return (entry.hosts || []).indexOf(host) >= 0 || (!host && !(entry.hosts || []).length); });
      var entry = clone(original || {});
      entry.secretName = secret;
      entry.hosts = host ? [host] : [];
      entries.push(entry);
    });
    return entries.length ? entries : undefined;
  }
  function buildObject(base, originalRows, rows, fields) {
    var result = clone(base), spec = result.spec;
    if (!validDNS(fields.name, 253)) throw new Error("资源名称必须是有效的 DNS 名称。");
    if (!validDNS(fields.namespace, 63) || fields.namespace.indexOf(".") >= 0) throw new Error("命名空间格式无效。");
    if (fields.className && !validDNS(fields.className, 253)) throw new Error("IngressClass 名称格式无效。");
    result.metadata.name = fields.name;
    result.metadata.namespace = fields.namespace;
    delete result.metadata.managedFields;
    delete result.status;
    if (fields.className) spec.ingressClassName = fields.className;
    else delete spec.ingressClassName;
    if (!rows.length && !(result.kind === "Ingress" && spec.defaultBackend)) throw new Error("至少添加一条路径。");
    var groups = new Map();
    rows.forEach(function (row, rowIndex) {
      var hosts = hostList(row.host, result.kind);
      if (!row.path || (result.kind === "Ingress" ? row.path[0] !== "/" : !/^(\/|~)/.test(row.path)) || /[\r\n]/.test(row.path)) throw new Error("第 " + (rowIndex + 1) + " 行路径格式无效。");
      if (row.tls && (!validDNS(row.tls, 253))) throw new Error("TLS Secret 名称格式无效。");
      if (result.kind === "Ingress" && ["Prefix", "Exact", "ImplementationSpecific"].indexOf(row.pathType) < 0) throw new Error("请选择有效的路径类型。");
      var backends = row.resource ? [] : row.backends.map(function (backend) {
        if (!validDNS(backend.service, 63) || backend.service.indexOf(".") >= 0) throw new Error("第 " + (rowIndex + 1) + " 行后端服务名称无效。");
        var port = servicePort(backend.port), value = clone(backend.source || {});
        if (result.kind === "Ingress") {
          value.name = backend.service;
          value.port = typeof port === "number" ? { number: port } : { name: port };
        } else {
          value.serviceName = backend.service;
          value.servicePort = port;
        }
        return value;
      });
      if (!row.resource && !backends.length) throw new Error("第 " + (rowIndex + 1) + " 行至少需要一个后端服务。");
      var groupKey = JSON.stringify([row.rule == null ? "new" : row.rule, hosts, result.kind === "Ingress" ? null : backends]);
      var group = groups.get(groupKey);
      if (!group) {
        if (result.kind === "Ingress") {
          group = clone((base.spec.rules || [])[row.rule] || {});
          if (hosts.length) group.host = hosts[0]; else delete group.host;
          group.http = group.http || {};
          group.http.paths = [];
        } else {
          group = clone((base.spec.http || [])[row.rule] || {});
          group.match = group.match || {};
          if (hosts.length) group.match.hosts = hosts; else delete group.match.hosts;
          group.match.paths = [];
          group.backends = backends;
        }
        groups.set(groupKey, group);
      }
      if (result.kind === "Ingress") {
        var path = clone(row.source || {});
        path.path = row.path; path.pathType = row.pathType;
        if (!row.resource) { path.backend = clone(path.backend || {}); path.backend.service = backends[0]; delete path.backend.resource; }
        group.http.paths.push(path);
      } else group.match.paths.push(row.path);
    });
    if (result.kind === "Ingress") {
      if (groups.size) spec.rules = Array.from(groups.values()); else delete spec.rules;
      var tls = ingressTLS(base, originalRows, rows);
      if (tls) spec.tls = tls; else delete spec.tls;
    } else {
      var names = new Set();
      spec.http = Array.from(groups.values()).map(function (route, i) {
        var name = route.name || "route-" + (i + 1), suffix = 1, unique = name;
        while (names.has(unique)) unique = name + "-" + (++suffix);
        route.name = unique; names.add(unique);
        return route;
      });
    }
    return result;
  }
  function approvalPending(result) {
    return result && (Number(result.code) === 40304 || result.approval_required || result.approval && result.approval.approval_required ||
      result.data && result.data.approval_required);
  }
  function editor(ctx, object, done, available, ingressClasses, apisixClasses, classDiscovery, isCurrent) {
    var initialKind = object ? object.kind : available.Ingress ? "Ingress" : "ApisixRoute";
    var base = object ? clone(object) : initialKind === "Ingress" ? ingressTemplate("") : apisixTemplate(available.ApisixRoute, "");
    var originalRows = pathRows(base), rows = clone(originalRows), saving = false, draftRev = 0;
    function classOptions(kind) {
      return (kind === "ApisixRoute" ? apisixClasses : ingressClasses).map(function (name) { return '<option value="' + E(name) + '"></option>'; }).join("");
    }
    function input(name, value, extra) {
      return '<input class="x-input" name="' + name + '" value="' + E(value) + '"' + (extra || "") + '>';
    }
    dlg(object ? "编辑流量配置" : "创建流量配置", '<form id="netConfigForm" class="x-form x-exposure-editor">' +
      '<p class="x-desc">' + E(ctx.cluster.name) + '   ' + E(ctx.cluster.env || "") + '</p>' +
      '<div class="x-net-fields"><label><span>资源类型</span><select class="x-select" name="routeKind"' + (object ? " disabled" : "") + '>' +
      '<option value="Ingress"' + (base.kind === "Ingress" ? " selected" : "") + (!available.Ingress && base.kind !== "Ingress" ? " disabled" : "") + '>Ingress</option>' +
      '<option value="ApisixRoute"' + (base.kind === "ApisixRoute" ? " selected" : "") + (!available.ApisixRoute && base.kind !== "ApisixRoute" ? " disabled" : "") + '>ApisixRoute</option></select></label>' +
      '<label><span>名称</span>' + input("resourceName", base.metadata.name, ' required maxlength="253"' + (object ? " readonly" : "")) + '</label>' +
      '<label><span>命名空间</span>' + input("namespace", base.metadata.namespace, ' required maxlength="63"' + (object ? " readonly" : "")) + '</label>' +
      '<label><span>IngressClass</span>' + input("className", base.spec.ingressClassName || "", ' list="netIngressClasses" placeholder="集群默认" maxlength="253"') +
      '<datalist id="netIngressClasses">' + classOptions(base.kind) + '</datalist></label></div>' +
      (classDiscovery && !classDiscovery.available ? '<span class="x-net-status" role="status" data-class-discovery>' + E(unavailableMessage(classDiscovery)) + '</span>' : "") +
      '<label><span>自然语言生成</span><input class="x-input" name="instruction" data-net-ai-prompt maxlength="2000" placeholder="域名、路径、后端服务和端口"></label>' +
      '<div class="x-exposure-ai"><button class="x-btn" type="button" data-net-ai>生成草稿</button><span data-net-ai-status role="status" aria-live="polite"></span></div>' +
      '<div class="x-net-paths"><table><thead><tr><th>域名</th><th>路径</th><th>路径类型</th><th>后端服务 / 端口</th><th>TLS Secret</th><th></th></tr></thead><tbody data-net-paths></tbody></table></div>' +
      '<div><button class="x-btn" type="button" data-net-add-path>+ 添加路径</button></div>' +
      '<label><span>变更原因</span><input class="x-input" name="reason" required maxlength="500"></label>' +
      '<label class="x-net-confirm"><input type="checkbox" name="confirm" required>已核对目标集群、命名空间、路由和业务影响</label>' +
      '<p class="x-aerr" data-net-error role="alert" hidden></p></form>',
      '<button class="x-btn" data-close type="button">取消</button><button class="x-btn x-btn--pri" type="submit" form="netConfigForm">保存</button>');
    var form = document.getElementById("netConfigForm");
    var routeKind = form.elements.routeKind, error = form.querySelector("[data-net-error]");
    var button = document.querySelector('[form="netConfigForm"]');
    function active() { return isCurrent() && document.getElementById("netConfigForm") === form && form.isConnected !== false; }
    function showError(message) { error.textContent = message; error.hidden = false; }
    function readRows() {
      form.querySelectorAll("[data-net-path-row]").forEach(function (tr, i) {
        var row = rows[i];
        ["host", "path", "pathType", "tls"].forEach(function (field) { row[field] = tr.querySelector('[data-net-field="' + field + '"]').value.trim(); });
        tr.querySelectorAll("[data-net-backend]").forEach(function (node, j) {
          row.backends[j].service = node.querySelector("[data-net-service]").value.trim();
          row.backends[j].port = node.querySelector("[data-net-port]").value.trim();
        });
      });
    }
    function changed() { draftRev++; form.elements.confirm.checked = false; }
    function drawRows() {
      form.querySelector("[data-net-paths]").innerHTML = rows.map(function (row, i) {
        var apisix = base.kind === "ApisixRoute";
        return '<tr data-net-path-row><td>' + input("host-" + i, row.host, ' data-net-field="host" aria-label="域名"' + (apisix ? ' placeholder="可用逗号分隔多个域名"' : ' placeholder="全部域名"')) + '</td>' +
          '<td>' + input("path-" + i, row.path, ' data-net-field="path" aria-label="路径" required') + '</td>' +
          '<td><select class="x-select" data-net-field="pathType" aria-label="路径类型"' + (apisix ? " disabled" : "") + '>' +
          ["Prefix", "Exact", "ImplementationSpecific"].map(function (type) { return '<option value="' + type + '"' + (row.pathType === type ? " selected" : "") + '>' + (apisix && type === "ImplementationSpecific" ? "APISIX" : type) + '</option>'; }).join("") + '</select></td>' +
          '<td><div class="x-net-backends">' + row.backends.map(function (backend, j) {
            return '<div class="x-net-backend" data-net-backend>' +
              input("service-" + i + "-" + j, row.resource ? row.resource.kind + "/" + row.resource.name : backend.service, ' data-net-service aria-label="后端服务"' + (row.resource ? " disabled" : " required")) +
              input("port-" + i + "-" + j, backend.port, ' data-net-port aria-label="服务端口"' + (row.resource ? " disabled" : " required")) +
              (apisix ? '<button type="button" class="x-btn" data-net-remove-backend="' + i + ':' + j + '" aria-label="删除后端" title="删除后端">×</button>' : "") + '</div>';
          }).join("") + (apisix ? '<button class="x-btn" type="button" data-net-add-backend="' + i + '">+ 后端</button>' : "") + '</div></td>' +
          '<td>' + input("tls-" + i, row.tls, ' data-net-field="tls" aria-label="TLS Secret"' + (apisix ? ' disabled placeholder="网关管理"' : ' placeholder="可选"')) + '</td>' +
          '<td><button class="x-btn" type="button" data-net-remove-path="' + i + '" aria-label="删除路径" title="删除路径">×</button></td></tr>';
      }).join("");
      form.querySelectorAll("[data-net-remove-path]").forEach(function (control) { control.onclick = function () { readRows(); rows.splice(Number(control.dataset.netRemovePath), 1); changed(); drawRows(); }; });
      form.querySelectorAll("[data-net-add-backend]").forEach(function (control) { control.onclick = function () {
        readRows(); rows[Number(control.dataset.netAddBackend)].backends.push({ service: "", port: "80", source: {} }); changed(); drawRows();
      }; });
      form.querySelectorAll("[data-net-remove-backend]").forEach(function (control) { control.onclick = function () {
        readRows(); var indices = control.dataset.netRemoveBackend.split(":").map(Number); rows[indices[0]].backends.splice(indices[1], 1); changed(); drawRows();
      }; });
    }
    drawRows();
    form.oninput = function (event) { if (event.target !== form.elements.confirm && event.target !== form.elements.reason && event.target !== form.elements.instruction) changed(); };
    form.querySelector("[data-net-add-path]").onclick = function () {
      readRows(); rows.push({ host: "", path: base.kind === "Ingress" ? "/" : "/*", pathType: base.kind === "Ingress" ? "Prefix" : "ImplementationSpecific", tls: "", backends: [{ service: "", port: "80", source: {} }] });
      changed(); drawRows();
    };
    routeKind.onchange = function () {
      if (object || !available[routeKind.value]) { routeKind.value = base.kind; return; }
      var meta = { name: form.elements.resourceName.value, namespace: form.elements.namespace.value };
      base = routeKind.value === "ApisixRoute" ? apisixTemplate(available.ApisixRoute, form.elements.className.value) : ingressTemplate(form.elements.className.value);
      base.metadata = meta;
      originalRows = pathRows(base); rows = clone(originalRows);
      form.querySelector("#netIngressClasses").innerHTML = classOptions(base.kind);
      changed(); drawRows();
    };
    var aiButton = form.querySelector("[data-net-ai]");
    var aiPrompt = form.querySelector("[data-net-ai-prompt]");
    var aiStatus = form.querySelector("[data-net-ai-status]");
    if (!window.XS || typeof window.XS.can !== "function" || !window.XS.can("ai:chat")) {
      aiButton.disabled = true;
      aiButton.title = "需要 ai:chat 权限";
      aiStatus.textContent = "需要 ai:chat 权限";
    }
    aiButton.onclick = async function () {
      var instruction = String(aiPrompt.value || "").trim();
      if (!instruction) { aiStatus.textContent = "先描述域名、路径、后端服务和端口。"; aiPrompt.focus(); return; }
      var api = window.XS;
      if (!api || typeof api.post !== "function" || typeof api.get !== "function" || !api.can("ai:chat")) {
        aiStatus.textContent = "AI 接口或 ai:chat 权限不可用";
        return;
      }
      aiButton.disabled = true;
      aiStatus.textContent = "正在检查已启用模型…";
      var requestRev = draftRev, selectedKind = base.kind;
      try {
        var models = await api.get("/ai/models/options");
        if (!active() || requestRev !== draftRev) return;
        models = Array.isArray(models) ? models : (models.items || models.models || []);
        if (!models.length) throw new Error("尚未配置已启用的 AI 模型。");
        aiStatus.textContent = "正在生成草稿…";
        var response = await api.post("/ai/assist", {
          scene: selectedKind === "ApisixRoute" ? "k8s.apisixroute.create" : "k8s.ingress.create",
          instruction: instruction,
          schema: { manifest: "完整 Kubernetes JSON 资源清单字符串；kind 必须是 " + selectedKind + "，不要输出 Secret 内容。" }
        });
        var generated = response && response.fields && response.fields.manifest;
        if (!active() || requestRev !== draftRev) return;
        var parsed = generated && typeof generated === "object" ? generated : JSON.parse(String(generated || ""));
        if (!parsed || parsed.kind !== selectedKind || !parsed.metadata || !parsed.spec) {
          throw new Error("生成结果不是完整的 " + selectedKind + " 清单，请补充要求后重试。");
        }
        delete parsed.status;
        if (parsed.metadata) delete parsed.metadata.managedFields;
        var generatedRows = pathRows(parsed);
        if (!generatedRows.length) throw new Error("草稿没有可用的 HTTP 路径。");
        if (!object) {
          base = parsed;
          base.apiVersion = selectedKind === "Ingress" ? "networking.k8s.io/v1" : available.ApisixRoute;
          originalRows = pathRows(base);
          form.elements.resourceName.value = parsed.metadata.name || "";
          form.elements.namespace.value = parsed.metadata.namespace || "default";
          form.elements.className.value = parsed.spec.ingressClassName || form.elements.className.value;
        } else {
          // AI may propose visible fields; existing plugins, annotations and route policy stay authoritative.
          generatedRows.forEach(function (row, index) {
            var original = originalRows[index];
            if (original) {
              row.rule = original.rule; row.pathIndex = original.pathIndex; row.source = original.source;
              row.backends.forEach(function (backend, i) { backend.source = original.backends[i] && original.backends[i].source || {}; });
            } else { delete row.rule; delete row.pathIndex; delete row.source; }
          });
        }
        rows = generatedRows;
        changed(); drawRows();
        aiStatus.textContent = "草稿已填入";
      } catch (err) {
        if (active()) aiStatus.textContent = err.message || "生成失败";
      } finally {
        if (active()) {
          aiButton.disabled = false;
          if (requestRev !== draftRev && !aiStatus.textContent.includes("草稿已填入")) aiStatus.textContent = "配置已变更，请重新生成";
        }
      }
    };
    form.onsubmit = async function (event) {
      event.preventDefault();
      if (saving) return;
      if (!active()) { showError("页面或目标集群已变更，请重新打开配置。"); return; }
      if (!XS.can || !XS.can("network:write")) { showError("需要 network:write 权限。"); return; }
      if (!form.reportValidity()) return;
      error.hidden = true;
      try {
        readRows();
        if (!form.elements.confirm.checked || !form.elements.reason.value.trim()) throw new Error("请填写变更原因并确认配置。");
        var manifest = buildObject(base, originalRows, rows, {
          name: form.elements.resourceName.value.trim(), namespace: form.elements.namespace.value.trim(), className: form.elements.className.value.trim()
        });
        if (object && objectKey(manifest) !== objectKey(object)) throw new Error("不能修改现有资源的身份。");
        saving = true; button.disabled = true;
        var result = await XS.post("/clusters/" + encodeURIComponent(ctx.cluster.id) + "/network/configuration/apply?reason=" + encodeURIComponent(form.elements.reason.value.trim()),
          { yaml: JSON.stringify(manifest), namespace: manifest.metadata.namespace, force: false });
        if (!active()) return;
        if (approvalPending(result)) { closeAll(); toast("变更已提交环境审批"); done(); return; }
        var failed = result && (result.objects || []).filter(function (o) { return o.status === "failed" || o.error; }) || [];
        if (!result || Number(result.failed) > 0 || failed.length || result.status === "failed") throw new Error(failed.map(function (o) { return (o.name || "资源") + ": " + (o.error || "保存失败"); }).join("; ") || result && result.error || "资源保存失败");
        closeAll(); toast("配置已保存"); done();
      } catch (err) {
        if (!active()) return;
        if (approvalPending(err)) { closeAll(); toast("变更已提交环境审批"); done(); }
        else showError(err.message || "保存失败");
      } finally {
        saving = false;
        if (active()) button.disabled = false;
      }
    };
  }
  function routeStatus(object) {
    var status = object.status || {}, conditions = status.conditions || [];
    var failed = conditions.find(function (condition) {
      return condition.status === "False" && /Accepted|Ready|Programmed|LoadBalancer/i.test(condition.type || "");
    });
    if (failed) return { label: failed.reason || "存在问题", key: "issue" };
    if (status.loadBalancer && (status.loadBalancer.ingress || []).length) return { label: "已分配入口", key: "ready" };
    if (conditions.some(function (condition) { return condition.status === "True" && /Accepted|Ready|Programmed/i.test(condition.type || ""); })) return { label: "已就绪", key: "ready" };
    return { label: "待控制器处理", key: "pending" };
  }
  function exposureRows(resources) {
    var rows = [];
    var classFor = function (object) {
      return (object.spec || {}).ingressClassName || "集群默认";
    };
    resources.forEach(function (resource) {
      if (!resource.available) return;
      (resource.items || []).forEach(function (object) {
        var meta = object.metadata || {}, spec = object.spec || {}, namespace = meta.namespace || "default";
        var state = routeStatus(object), className = classFor(object), kind = object.kind;
        if (kind === "Ingress") {
          var tlsByHost = new Map();
          (spec.tls || []).forEach(function (tls) {
            (tls.hosts || []).forEach(function (host) { tlsByHost.set(host, tls.secretName || "未指定 Secret"); });
          });
          (spec.rules || []).forEach(function (rule) {
            var host = rule.host || "*";
            var paths = (rule.http && rule.http.paths || []);
            (paths.length ? paths : [{}]).forEach(function (path) {
              var service = (((path.backend || {}).service) || {});
              rows.push({
                kind: "Ingress", namespace: namespace, className: className, state: state,
                host: host, path: path.path || "/", backend: service.name ? service.name + ":" + ((service.port || {}).number || (service.port || {}).name || "—") : "未配置后端",
                tls: tlsByHost.has(host) ? "TLS · " + tlsByHost.get(host) : "未启用",
                object: object
              });
            });
          });
        } else if (kind === "ApisixRoute") {
          (spec.http || []).forEach(function (route) {
            var match = route.match || {}, backends = route.backends || [];
            rows.push({
              kind: "ApisixRoute", namespace: namespace, className: className,
              state: state, host: (match.hosts || ["*"]).join(", "),
              path: (match.paths || ["/*"]).join(", "),
              backend: backends.map(function (backend) { return (backend.serviceName || "未配置服务") + ":" + (backend.servicePort || "—"); }).join(", ") || "未配置后端",
              tls: "由 APISIX 网关配置", object: object
            });
          });
        }
        if (!rows.some(function (row) { return row.object === object; })) {
          rows.push({ kind: kind, namespace: namespace, className: className, state: state, host: meta.name || "未命名", path: "—", backend: summary(object), tls: "—", object: object });
        }
      });
    });
    return rows;
  }
  function render(el, pg, page, ctx) {
    var mode = pg.k;
    var root = "/clusters/" + encodeURIComponent(ctx.cluster.id) + "/network/";
    var clusterId = String(ctx.cluster.id), revision = page.rev, request = {};
    var operation = 0;
    el._xsNetworkConfigRequest = request;
    function currentPage() {
      return el._xsNetworkConfigRequest === request && page.rev === revision && revision === pageRenderRev &&
        current() === "traffic" && String(ctx.cluster.id) === clusterId && el.isConnected !== false;
    }
    if (document.head && !document.getElementById("xs-network-config-forms-css")) {
      var css = document.createElement("link");
      css.id = "xs-network-config-forms-css"; css.rel = "stylesheet"; css.href = "/xs-network-config-forms.css";
      document.head.appendChild(css);
    }
    try {
      return XS.get(root + "configuration?view=" + encodeURIComponent(mode)).then(function (data) {
      if (!currentPage()) return;
      var uniqueResources = Array.from(new Map(snapshotResources(data).map(function (resource) { return [resource.kind, resource]; })).values());
      var objects = new Map();
      var resources = pg.k === "expose" ? uniqueResources.filter(function (r) {
        return r.kind === "Ingress" || r.kind === "ApisixRoute";
      }) : uniqueResources;
      resources.forEach(function (resource) { (resource.items || []).forEach(function (object) { objects.set(objectKey(object), object); }); });
      var available = {};
      resources.forEach(function (resource) { if (resource.available) available[resource.kind] = resource.api_version; });
      var writable = typeof XS.can === "function" && XS.can("network:write");
      var apisixResource = resources.find(function (r) { return r.kind === "ApisixRoute" && r.available; });
      var ingressClassResource = uniqueResources.find(function (r) { return r.kind === "IngressClass"; });
      var classItems = ingressClassResource && ingressClassResource.available ? ingressClassResource.items || [] : [];
      var ingressClasses = classItems.map(function (item) { return item.metadata && item.metadata.name; }).filter(Boolean).sort();
      var apisixClasses = classItems.filter(function (item) {
        return (((item.spec || {}).controller || "").indexOf("apisix.apache.org/") === 0);
      }).map(function (item) { return item.metadata && item.metadata.name; }).filter(Boolean).sort();
      var rows = exposureRows(resources);
      var namespaceNames = Array.from(new Set(rows.map(function (row) { return row.namespace; }))).sort();
      var classNames = Array.from(new Set(rows.map(function (row) { return row.className; }))).sort();
      function options(values) { return '<option value="">全部</option>' + values.map(function (value) { return '<option value="' + E(value) + '">' + E(value) + '</option>'; }).join(""); }
      function rowHtml(row) {
        var key = objectKey(row.object);
        return '<div class="x-exposure-row" role="row" data-exposure-row data-kind="' + E(row.kind) + '" data-namespace="' + E(row.namespace) + '" data-class="' + E(row.className) + '" data-status="' + E(row.state.key) + '" data-search="' + E([row.host, row.path, row.backend, row.namespace, row.className].join(" ").toLowerCase()) + '">' +
          '<span role="cell">' + E(row.kind === "ApisixRoute" ? "APISIX" : row.kind) + '</span>' +
          '<span class="x-exposure-host" role="cell">' + E(row.host) + '<small>' + E(row.namespace + "/" + (row.object.metadata || {}).name) + '</small></span>' +
          '<span class="x-exposure-route" role="cell"><strong>' + E(row.path) + '</strong><small>→ ' + E(row.backend) + '</small></span>' +
          '<span role="cell">' + E(row.tls) + '</span><span role="cell">' + E(row.className) + '</span>' +
          '<span role="cell"><i class="x-exposure-state x-exposure-state--' + E(row.state.key) + '"></i>' + E(row.state.label) + '</span>' +
          '<span class="x-exposure-actions" role="cell"><button class="x-btn" data-net-view="' + E(key) + '" type="button" title="查看配置">查看</button>' +
          (writable && (row.object.kind === "Ingress" || row.object.kind === "ApisixRoute") ? ' <button class="x-btn" data-net-edit="' + E(key) + '" type="button" title="编辑配置">编辑</button>' : "") + '</span></div>';
      }
      var ingressCount = (resources.find(function (r) { return r.kind === "Ingress"; }) || {}).items || [];
      var apisixCount = (apisixResource || {}).items || [];
      var tlsCount = ingressCount.reduce(function (count, object) { return count + ((object.spec || {}).tls || []).length; }, 0);
      var issueCount = rows.filter(function (row) { return row.state.key !== "ready"; }).length;
      function capabilityBadge(resource) {
        var label = resource.available ? "可读取" : unavailableMessage(resource);
        return '<span class="x-net-status" data-net-capability="' + E(resource.state || (resource.available ? "available" : "unavailable")) +
          '" title="' + E(resourceName(resource)) + '">' + E(resource.kind + "：" + label) + '</span>';
      }
      var body = '<div class="x-tbar"><span class="x-frac">' + E(ctx.cluster.name) + '   ' +
        (pg.k === "expose" ? "集群入口资源" : "已安装组件的实时资源") + '</span>' +
        '<button class="x-btn" type="button" data-net-refresh style="margin-left:auto">刷新</button>' +
        (writable ? '<button class="x-btn x-btn--pri" type="button" data-net-create' + (!available.Ingress && !available.ApisixRoute ? ' disabled title="入口资源 API 不可用"' : "") + '>+ 创建入口</button>' : "") + '</div>' +
        '<span class="x-net-status" data-net-action-status role="status" aria-live="polite" hidden></span><p class="x-aerr" data-net-action-error role="alert" hidden></p>';
      if (pg.k === "expose") {
        body += '<div class="x-exposure"><div class="x-exposure-stats"><div><strong>' + ingressCount.length + '</strong><span>Ingress</span></div><div><strong>' + apisixCount.length + '</strong><span>APISIX 路由</span></div><div><strong>' + tlsCount + '</strong><span>TLS 引用</span></div><div><strong>' + issueCount + '</strong><span>待处理项</span></div></div>' +
          '<div class="x-exposure-capabilities">' + resources.map(capabilityBadge).join("") + (ingressClassResource ? capabilityBadge(ingressClassResource) : "") + '</div>' +
          '<div class="x-exposure-filters"><input class="x-input" type="search" data-exposure-search placeholder="搜索域名、路径、后端"><select class="x-select" data-exposure-kind aria-label="资源类型"><option value="">全部类型</option><option value="Ingress">Ingress</option><option value="ApisixRoute">APISIX</option></select><select class="x-select" data-exposure-namespace aria-label="命名空间">' + options(namespaceNames) + '</select><select class="x-select" data-exposure-class aria-label="入口 Class">' + options(classNames) + '</select><select class="x-select" data-exposure-status aria-label="状态"><option value="">全部状态</option><option value="ready">已就绪</option><option value="pending">待处理</option><option value="issue">存在问题</option></select></div>' +
          '<div class="x-exposure-table" role="table"><div class="x-exposure-heading" role="row"><span>类型</span><span>域名 / 命名空间</span><span>路径 → 后端</span><span>TLS</span><span>Class</span><span>状态</span><span>操作</span></div>' +
          rows.map(rowHtml).join("") +
          '</div>' +
          '<p class="x-empty" data-exposure-empty>当前筛选条件下没有可展示的入口资源。</p></div>';
      } else {
        resources.forEach(function (r) {
          body += '<h2 class="x-h2">' + E(resourceName(r)) + '</h2>';
          if (!r.available) { body += '<p class="x-aerr">' + E(unavailableMessage(r)) + '</p>'; return; }
          var resourceRows = (r.items || []).map(function (o) {
            return [E(o.metadata.namespace || "集群级"), E(o.metadata.name), E(summary(o)),
              '<button class="x-btn" data-net-view="' + E(objectKey(o)) + '" type="button">查看</button>'];
          });
          body += resourceRows.length ? tbl(["命名空间", "名称", "配置", "操作"], resourceRows) : '<p class="x-empty">该 API 可用，暂无资源。</p>';
        });
      }
      page.draw(ctx, body);
      if (!currentPage()) return;
      el.querySelector("[data-net-refresh]").onclick = function () { operation++; renderTrafficLive(el); };
      function actionError(message) {
        var error = el.querySelector("[data-net-action-error]");
        error.textContent = message; error.hidden = false;
      }
      function latestObject(key) {
        var previous = objects.get(key);
        if (!previous) return Promise.reject(new Error("找不到该资源，请刷新列表。"));
        return XS.get(root + "configuration?view=" + encodeURIComponent(mode)).then(function (latest) {
          var group = snapshotResources(latest).find(function (resource) {
            return resource.kind === previous.kind && resource.api_version === previous.apiVersion;
          });
          if (!group || !group.available) throw new Error(group ? unavailableMessage(group) : "资源 API 不可用。");
          var object = group.items.find(function (item) { return objectKey(item) === key; });
          if (!object || !object.metadata.name || !object.spec && (object.kind === "Ingress" || object.kind === "ApisixRoute")) throw new Error("资源不存在或未返回完整配置，请刷新后重试。");
          if (previous.metadata.uid && object.metadata.uid && previous.metadata.uid !== object.metadata.uid) throw new Error("资源已被重新创建，请刷新后重试。");
          return object;
        });
      }
      var openEditor = function (key) {
        if (!writable || !currentPage()) return;
        var sequence = ++operation;
        function actionCurrent() { return currentPage() && sequence === operation; }
        var status = el.querySelector("[data-net-action-status]");
        status.textContent = "正在读取配置…"; status.hidden = false;
        el.querySelector("[data-net-action-error]").hidden = true;
        var discovery = XS.get(root + "configuration?view=ingress").catch(function (error) {
          return { resources: [{ kind: "IngressClass", available: false, state: "unavailable", error: error.message || "IngressClass 发现失败" }] };
        });
        return Promise.all([key ? latestObject(key) : Promise.resolve(null), discovery]).then(function (values) {
          if (!actionCurrent()) return;
          var object = values[0], discovery = values[1];
          var discovered = (discovery.resources || []).find(function (resource) { return resource.kind === "IngressClass"; });
          classItems = discovered && discovered.available ? discovered.items || [] : [];
          ingressClasses = classItems.map(function (item) { return item.metadata && item.metadata.name; }).filter(Boolean).sort();
          apisixClasses = classItems.filter(function (item) {
            return (((item.spec || {}).controller || "").indexOf("apisix.apache.org/") === 0);
          }).map(function (item) { return item.metadata && item.metadata.name; }).filter(Boolean).sort();
          ingressClassResource = discovered || { available: false, state: "unavailable", error: "未返回 IngressClass 发现结果" };
          // The initial list is only a snapshot; create capabilities also follow the fresh discovery result.
          var ingressAPI = (discovery.resources || []).find(function (resource) { return resource.kind === "Ingress"; });
          var editorAvailable = Object.assign({}, available);
          if (ingressAPI) {
            if (ingressAPI.available) editorAvailable.Ingress = ingressAPI.api_version;
            else delete editorAvailable.Ingress;
          }
          if (!object && !editorAvailable.Ingress && !editorAvailable.ApisixRoute) throw new Error("入口资源 API 不可用。");
          editor(ctx, object, function () { if (currentPage()) renderTrafficLive(el); },
            editorAvailable, ingressClasses, apisixClasses, ingressClassResource, actionCurrent);
        }).catch(function (error) {
          if (actionCurrent()) actionError(error.message || "读取配置失败");
        }).finally(function () {
          if (actionCurrent()) status.hidden = true;
        });
      };
      var createButton = el.querySelector("[data-net-create]");
      if (createButton) createButton.onclick = function () { return openEditor(null); };
      if (pg.k === "expose") {
        var applyFilters = function () {
          var query = (el.querySelector("[data-exposure-search]").value || "").trim().toLowerCase();
          var kind = el.querySelector("[data-exposure-kind]").value;
          var namespace = el.querySelector("[data-exposure-namespace]").value;
          var className = el.querySelector("[data-exposure-class]").value;
          var status = el.querySelector("[data-exposure-status]").value;
          var shown = 0;
          el.querySelectorAll("[data-exposure-row]").forEach(function (row) {
            var match = (!kind || row.dataset.kind === kind) && (!query || row.dataset.search.indexOf(query) >= 0) &&
              (!namespace || row.dataset.namespace === namespace) && (!className || row.dataset.class === className) &&
              (!status || row.dataset.status === status);
            row.hidden = !match;
            if (match) shown++;
          });
          var empty = el.querySelector("[data-exposure-empty]");
          if (empty) empty.hidden = shown > 0;
        };
        ["[data-exposure-search]", "[data-exposure-kind]", "[data-exposure-namespace]", "[data-exposure-class]", "[data-exposure-status]"].forEach(function (selector) {
          var control = el.querySelector(selector);
          if (control) control.oninput = control.onchange = applyFilters;
        });
        applyFilters();
      }
      el.querySelectorAll("[data-net-view]").forEach(function (b) { b.onclick = function () {
        if (!currentPage()) return;
        var sequence = ++operation;
        return latestObject(b.dataset.netView).then(function (object) {
          if (currentPage() && sequence === operation) view(object);
        }).catch(function (error) { if (currentPage() && sequence === operation) actionError(error.message || "读取配置失败"); });
      }; });
      el.querySelectorAll("[data-net-edit]").forEach(function (b) { b.onclick = function () { return openEditor(b.dataset.netEdit); }; });
      }).catch(function (err) {
        if (currentPage()) page.draw(ctx, '<p class="x-aerr" role="alert">读取流量配置失败：' + E(err.message || "未知错误") + '</p>');
      });
    } catch (err) {
      if (currentPage()) page.draw(ctx, '<p class="x-aerr" role="alert">读取流量配置失败：' + E(err.message || "未知错误") + '</p>');
    }
  }
  window.XSNetworkConfig = { render: render, summary: summary };
})();
