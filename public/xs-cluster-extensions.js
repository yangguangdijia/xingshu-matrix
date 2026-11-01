(function (root) {
  "use strict";

  var runtime = null;
  var componentCache = {};
  var RBAC_KINDS = [
    { kind: "ServiceAccount", path: "serviceaccounts", label: "ServiceAccount", namespaced: true },
    { kind: "Role", path: "roles", label: "Role", namespaced: true },
    { kind: "ClusterRole", path: "clusterroles", label: "ClusterRole", namespaced: false },
    { kind: "RoleBinding", path: "rolebindings", label: "RoleBinding", namespaced: true },
    { kind: "ClusterRoleBinding", path: "clusterrolebindings", label: "ClusterRoleBinding", namespaced: false }
  ];
  var WORKLOAD_KINDS = [
    { v: "Deployment", t: "Deployment" },
    { v: "StatefulSet", t: "StatefulSet" },
    { v: "DaemonSet", t: "DaemonSet" },
    { v: "Job", t: "Job" },
    { v: "CronJob", t: "CronJob" },
    { v: "Pod", t: "Pod" }
  ];

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (ch) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch];
    });
  }

  function list(data) {
    if (runtime && typeof runtime.itemsOf === "function") return runtime.itemsOf(data);
    if (Array.isArray(data)) return data;
    return data && (data.items || data.data || data.results) || [];
  }

  function pathFor(clusterId, suffix) {
    return "/clusters/" + encodeURIComponent(clusterId) + suffix;
  }

  function request(promise) {
    return Promise.resolve(promise).then(function (data) { return { ok: true, data: data }; }, function (err) {
      return { ok: false, error: err };
    });
  }

  function errorText(err) {
    return runtime && runtime.errText ? runtime.errText(err) : String(err && err.message || err || "请求失败");
  }

  function componentOptions(clusterId, key) {
    var entry = componentCache[String(clusterId) + ":" + key];
    return entry && entry.items || [];
  }

  function componentState(clusterId, key) {
    return componentCache[String(clusterId) + ":" + key] || { items: [], error: null };
  }

  function ensureComponents(clusterId) {
    var id = String(clusterId);
    if (componentCache[id + ":loading"]) return componentCache[id + ":loading"];
    var base = pathFor(id, "");
    var jobs = [
      request(runtime.apiGet(base + "/storage/classes")),
      request(runtime.apiGet(base + "/storage/capabilities")),
      request(runtime.apiGet(base + "/network/configuration", { view: "ingress" })),
      request(runtime.apiGet(base + "/backups"))
    ];
    var loading = Promise.all(jobs).then(function (results) {
      var classes = results[0].ok ? list(results[0].data).map(function (x) { return x.name || x.metadata && x.metadata.name; }).filter(Boolean) : [];
      var snapshots = results[1].ok && results[1].data && results[1].data.snapshots || {};
      var snapshotClasses = snapshots.classes || [];
      var network = results[2].ok ? results[2].data : {};
      var ingress = (network.resources || []).filter(function (r) {
        return /ingressclass/i.test(String(r.kind || r.resource || r.name || ""));
      }).reduce(function (all, r) { return all.concat(list(r.items).map(function (x) { return x.name || x.metadata && x.metadata.name; })); }, []);
      var backup = results[3].ok ? results[3].data || {} : {};
      var backupResources = backup.resources || {};
      var locationCapability = backupResources.velero_storage_locations || {};
      var locations = backup.storage_locations || backup.storageLocations || locationCapability.items ||
        backup.velero && (backup.velero.storage_locations || backup.velero.storageLocations) || [];
      if (!Array.isArray(locations)) locations = list(locations);
      locations = locations.map(function (x) { return typeof x === "string" ? x : x.name || x.metadata && x.metadata.name; }).filter(Boolean);
      componentCache[id + ":storageClasses"] = { items: classes, error: results[0].error };
      componentCache[id + ":volumeSnapshotClasses"] = { items: snapshotClasses.map(function (x) { return typeof x === "string" ? x : x.name || x.metadata && x.metadata.name; }).filter(Boolean), error: results[1].error };
      componentCache[id + ":ingressClasses"] = { items: ingress, error: results[2].error };
      componentCache[id + ":veleroStorageLocations"] = { items: locations, error: results[3].error };
      return componentCache;
    }).finally(function () { delete componentCache[id + ":loading"]; });
    componentCache[id + ":loading"] = loading;
    return loading;
  }

  function panel(title, content, extra) {
    return '<section class="x-panel" style="margin-bottom:12px"><header class="x-ph"><strong>' + esc(title) +
      '</strong><span class="x-frac">' + esc(extra || "") + '</span></header><div class="x-pb">' + content + "</div></section>";
  }

  function empty(title, detail) {
    return '<div class="x-frac" style="padding:14px 0">' + esc(title) + (detail ? " · " + esc(detail) : "") + "</div>";
  }

  function table(headers, rows) {
    return '<div class="x-table-wrap"><table class="x-table"><thead><tr>' + headers.map(function (h) {
      return "<th>" + esc(h) + "</th>";
    }).join("") + "</tr></thead><tbody>" + (rows.length ? rows.map(function (r) {
      return "<tr>" + r.map(function (c) { return "<td>" + c + "</td>"; }).join("") + "</tr>";
    }).join("") : '<tr><td colspan="' + headers.length + '">' + empty("暂无资源") + "</td></tr>") + "</tbody></table></div>";
  }

  function optionsHtml(items, value, name, firstLabel) {
    var out = '<select class="x-input" name="' + esc(name) + '">';
    if (firstLabel != null) out += '<option value="">' + esc(firstLabel) + "</option>";
    items.forEach(function (item) {
      var v = typeof item === "string" ? item : item.v;
      var label = typeof item === "string" ? item : item.t;
      out += '<option value="' + esc(v) + '"' + (String(v) === String(value) ? " selected" : "") + ">" + esc(label) + "</option>";
    });
    return out + "</select>";
  }

  function labelsOf(node) {
    return node && (node.labels || node.metadata && node.metadata.labels) || {};
  }

  function nodeNames(group) {
    var nodes = group && group.nodes;
    if (!Array.isArray(nodes)) return [];
    return nodes.map(function (node) { return typeof node === "string" ? node : node.name || node.metadata && node.metadata.name; }).filter(Boolean);
  }

  function renderNodeGroups(el, seq, cluster, box) {
    var selectedKey = runtime.getNodeGroupLabel ? runtime.getNodeGroupLabel() : "";
    var base = pathFor(cluster.id, "");
    Promise.all([
      request(runtime.apiGet(base + "/node-groups", selectedKey ? { label: selectedKey } : {})),
      request(runtime.apiGet(base + "/nodes"))
    ]).then(function (results) {
      if (!runtime.alive(seq, el) || !box) return;
      if (!results[0].ok) {
        box.innerHTML = runtime.errorPanel(results[0].error);
        return;
      }
      var groups = list(results[0].data);
      var nodes = results[1].ok ? list(results[1].data) : [];
      var keys = {};
      nodes.forEach(function (node) {
        Object.keys(labelsOf(node)).forEach(function (key) { keys[key] = true; });
      });
      var labelKeys = Object.keys(keys).sort();
      if (selectedKey && labelKeys.indexOf(selectedKey) < 0) labelKeys.unshift(selectedKey);
      var selected = selectedKey || results[0].data && results[0].data.label || "";
      if (selected && labelKeys.indexOf(selected) < 0) labelKeys.unshift(selected);
      var api = runtime.xs();
      var canWrite = !!(api && api.can && api.can("node:write"));
      var rows = groups.map(function (group) {
        var names = nodeNames(group);
        var actions = canWrite
          ? '<button class="x-btn" type="button" data-rename-group="' + esc(group.name || group.label_value || "") + '">设置/重命名分组</button>'
          : '<span class="x-frac">需要 node:write 权限</span>';
        return [
          esc(group.name || group.label_value || "未设置"),
          esc(group.label_value || "—"),
          esc(group.count == null ? names.length || group.node_count || 0 : group.count),
          esc(group.ready == null ? "—" : group.ready),
          esc(group.schedulable == null ? "—" : group.schedulable),
          esc((group.roles || []).join(", ") || "—"),
          esc((group.taints || []).map(function (t) { return typeof t === "string" ? t : t.key + ":" + t.value + ":" + t.effect; }).join(", ") || "—"),
          actions
        ];
      });
      var select = optionsHtml(labelKeys, selected, "node_label_key", "选择节点标签");
      box.innerHTML = panel("节点分组", '<div class="x-toolbar" style="margin-bottom:10px">' +
        '<label style="min-width:240px"><span>按标签分组</span>' + select + "</label>" +
        (canWrite ? '<button class="x-btn x-btn--pri" type="button" data-rename-group="__unset">设置/重命名分组</button>' : "") +
        "</div>" + (!labelKeys.length ? empty("未发现节点标签", "需要先在节点上配置可作为分组依据的标签") : "") +
        table(["分组", "标签值", "节点数", "就绪", "可调度", "角色", "污点", "操作"], rows) +
        (results[1].ok ? "" : '<p class="x-frac">' + esc("节点标签读取失败：" + errorText(results[1].error)) + "</p>"),
      selected ? "标签 " + selected : "请选择用于聚合的标签");
      var keySelect = box.querySelector('[name="node_label_key"]');
      if (keySelect) keySelect.onchange = function () {
        runtime.setNodeGroupLabel(keySelect.value);
        runtime.repaint();
      };
      Array.prototype.forEach.call(box.querySelectorAll("[data-rename-group]"), function (button) {
        button.onclick = function () {
          var target = button.getAttribute("data-rename-group");
          var group = groups.filter(function (entry) {
            return String(entry.name || entry.label_value || "") === target;
          })[0];
          var candidates = group ? nodeNames(group) : [];
          if (target === "__unset" && !candidates.length) {
            candidates = nodes.filter(function (node) { return !labelsOf(node)[selected]; }).map(function (node) {
              return node.name || node.metadata && node.metadata.name;
            }).filter(Boolean);
          }
          if (!candidates.length) return runtime.toast("当前分组没有可更新的节点");
          var body = '<div class="x-row2"><label><span class="x-req">节点标签 Key</span>' +
            optionsHtml(labelKeys, selected, "label_key", "选择已有标签") +
            '</label><label><span class="x-req">分组名称</span><input class="x-input x-mono" name="group_name" required maxlength="63" value="' +
            esc(target === "__unset" ? "" : group && group.label_value || "") + '" placeholder="例如 worker-prod" /></label></div>' +
            '<p class="x-frac">将对当前 ' + candidates.length + " 个节点批量设置所选标签。节点分组仍按标签值聚合。</p>";
          runtime.formDialog("设置/重命名节点分组", body, "保存标签", function (form, bad) {
            var key = form.elements.label_key.value.trim();
            var name = form.elements.group_name.value.trim();
            if (!key) return bad("请选择节点标签 Key", form.elements.label_key);
            if (!/^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$/.test(name) || name.length > 63) return bad("分组名称需符合 Kubernetes 标签值格式");
            runtime.apiPost(base + "/node-batch-labels", { nodes: candidates, labels: (function () {
              var value = {}; value[key] = name; return value;
            })() }).then(function () {
              runtime.closeModal();
              runtime.toast("节点分组标签已更新");
              runtime.setNodeGroupLabel(key);
              runtime.repaint();
            }).catch(function (error) { bad(errorText(error)); });
          });
        };
      });
    }).catch(function (err) {
      if (runtime.alive(seq, el) && box) box.innerHTML = runtime.errorPanel(err);
    });
  }

  function kindInfo(kind) {
    return WORKLOAD_KINDS.filter(function (item) { return item.v === kind; })[0] || WORKLOAD_KINDS[0];
  }

  function quoteYaml(value) {
    return JSON.stringify(value);
  }

  function yamlValue(value, depth) {
    var indent = new Array(depth + 1).join("  ");
    if (Array.isArray(value)) {
      if (!value.length) return indent + "[]";
      return value.map(function (item) {
        if (item && typeof item === "object") {
          var nested = yamlValue(item, depth + 1).split("\n");
          return indent + "- " + nested[0].trimStart() + (nested.length > 1 ? "\n" + nested.slice(1).join("\n") : "");
        }
        return indent + "- " + scalar(item);
      }).join("\n");
    }
    if (value && typeof value === "object") {
      var keys = Object.keys(value);
      if (!keys.length) return indent + "{}";
      return keys.map(function (key) {
        var item = value[key];
        var head = indent + quoteYaml(key) + ":";
        if (item && typeof item === "object") {
          var nested = yamlValue(item, depth + 1);
          return head + "\n" + nested;
        }
        return head + " " + scalar(item);
      }).join("\n");
    }
    return indent + scalar(value);
  }

  function scalar(value) {
    if (value === null) return "null";
    if (typeof value === "number" || typeof value === "boolean") return String(value);
    return quoteYaml(String(value));
  }

  function valueOf(form, name, fallback) {
    var control = form.elements && form.elements[name] || form.querySelector && form.querySelector('[name="' + name + '"]');
    return control ? String(control.value == null ? "" : control.value).trim() : fallback == null ? "" : String(fallback);
  }

  function clone(value) { return JSON.parse(JSON.stringify(value)); }

  function can(permission) {
    var xs = runtime && runtime.xs && runtime.xs();
    return !!(xs && typeof xs.can === "function" && xs.can(permission));
  }

  function requireName(value, label, max) {
    if (!value || value.length > (max || 63) || !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(value)) {
      throw new Error(label + "必须是小写字母、数字或连字符组成的有效名称");
    }
    return value;
  }

  function requireResourceName(value) {
    if (!value || value.length > 253 || value.split(".").some(function (part) {
      return !part || part.length > 63 || !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(part);
    })) throw new Error("资源名称格式无效");
    return value;
  }

  function integer(value, label, min, max) {
    if (!/^\d+$/.test(String(value)) || Number(value) < min || Number(value) > max) throw new Error(label + "超出有效整数范围");
    return Number(value);
  }

  function cron(value) {
    if (!value || !(/^(?:@\w+)$/.test(value) || value.split(/\s+/).length === 5)) throw new Error("请填写五段 Cron 表达式或有效计划别名");
    return value;
  }

  function workloadManifest(form) {
    var kind = valueOf(form, "kind");
    if (!WORKLOAD_KINDS.some(function (item) { return item.v === kind; })) throw new Error("不支持的工作负载类型");
    var ns = requireName(valueOf(form, "namespace"), "命名空间");
    var name = requireName(valueOf(form, "name"), "工作负载名称");
    var image = valueOf(form, "image");
    if (!image || /\s/.test(image)) throw new Error("请填写有效的容器镜像");
    var rawPort = valueOf(form, "container_port");
    var port = rawPort ? integer(rawPort, "容器端口", 1, 65535) : 0;
    var container = { name: name, image: image };
    if (port) container.ports = [{ containerPort: port }];
    var labels = { app: name };
    var podSpec = { containers: [container] };
    var manifest = { apiVersion: "apps/v1", kind: kind, metadata: { name: name, namespace: ns, labels: labels } };
    if (kind === "Pod") {
      manifest.apiVersion = "v1";
      manifest.spec = podSpec;
    } else if (kind === "Job" || kind === "CronJob") {
      podSpec.restartPolicy = valueOf(form, "restart_policy", "Never") || "Never";
      if (["Never", "OnFailure"].indexOf(podSpec.restartPolicy) < 0) throw new Error("任务重启策略必须是 Never 或 OnFailure");
      manifest.apiVersion = "batch/v1";
      if (kind === "CronJob") {
        manifest.spec = { schedule: cron(valueOf(form, "schedule")), jobTemplate: { spec: { template: { metadata: { labels: labels }, spec: podSpec } } } };
        var timezone = valueOf(form, "timezone");
        if (timezone) {
          try { new Intl.DateTimeFormat("en", { timeZone: timezone }); } catch (err) { throw new Error("CronJob 时区无效"); }
          manifest.spec.timeZone = timezone;
        }
      } else {
        manifest.spec = { template: { metadata: { labels: labels }, spec: podSpec } };
      }
    } else {
      manifest.spec = { selector: { matchLabels: labels }, template: { metadata: { labels: labels }, spec: podSpec } };
      if (kind !== "DaemonSet") manifest.spec.replicas = integer(valueOf(form, "replicas", "1"), "副本数", 0, 2147483647);
      if (kind === "StatefulSet") manifest.spec.serviceName = requireName(valueOf(form, "service_name", name) || name, "StatefulSet Service 名称");
    }
    return manifest;
  }

  function nameSpaceOptions(namespaces, selected) {
    var values = namespaces.map(function (ns) { return { v: ns.name || ns.metadata && ns.metadata.name, t: ns.name || ns.metadata && ns.metadata.name }; }).filter(function (item) { return item.v; });
    if (!values.length) return '<input class="x-input x-mono" name="namespace" required value="' + esc(selected) + '" placeholder="命名空间读取不可用，可手动填写" />';
    if (selected && !values.some(function (item) { return item.v === selected; })) values.unshift({ v: selected, t: selected });
    return optionsHtml(values, selected, "namespace", "选择命名空间");
  }

  function approvalText(result) {
    if (!result) return "";
    var data = result.approval || result.details || result.data || result;
    if (!(result.status === 202 || result.code === 40304 || data.approval_required)) return "";
    var ticket = data.ticket || result.ticket || {};
    return "已提交审批" + (ticket.id ? "（审批单 #" + ticket.id + "）" : "");
  }

  function applyIssue(result) {
    var pending = approvalText(result);
    if (pending) return pending;
    if (!result || !Array.isArray(result.objects) || !result.objects.length) return "服务端未返回资源执行结果";
    var errors = result.objects.filter(function (object) {
      return object.error || object.failed || /^(failed|error|rejected)$/i.test(String(object.status || ""));
    });
    if (Number(result.failed) > 0 || errors.length) return errors.map(function (object) {
      return (object.name ? object.name + "：" : "") + (object.error || object.message || object.status || "操作失败");
    }).join("；") || "部分资源操作失败";
    if (result.objects.some(function (object) { return !/^(applied|created|configured|unchanged|validated)$/i.test(String(object.status || "")); })) {
      return "资源执行状态未确认";
    }
    return "";
  }

  function failMessage(error) { return approvalText(error) || errorText(error); }

  function ensureWorkloadAbsent(clusterId, manifest) {
    var kind = manifest.kind.toLowerCase() + "s";
    return runtime.apiGet(pathFor(clusterId, "/namespaces/" + encodeURIComponent(manifest.metadata.namespace) + "/" +
      kind + "/" + encodeURIComponent(manifest.metadata.name))).then(function () {
      throw new Error("同名 " + manifest.kind + " 已存在，请使用其他名称；创建不会覆盖已有资源");
    }, function (error) {
      // An HTTP 404 can also mean the Matrix route is absent. Confirm via the live list.
      if (!error || error.status !== 404) throw error;
      return runtime.apiGet(pathFor(clusterId, "/workloads/" + kind), { namespace: manifest.metadata.namespace }).then(function (data) {
        if (!data || !Array.isArray(data.items)) throw new Error("无法确认同名资源是否存在，已停止创建");
        if (data.items.some(function (item) { return item.name === manifest.metadata.name; })) throw new Error("同名资源已存在，请使用其他名称");
      });
    });
  }

  function decorateWorkloads(args) {
    var toolbar = args.box && args.box.querySelector(".x-tbar");
    if (!can("workload:write")) {
      var old = toolbar && toolbar.querySelector("[data-wl-create]");
      if (old) old.remove();
      return;
    }
    if (!toolbar || toolbar.querySelector("[data-wl-create]")) return;
    var button = document.createElement("button");
    button.type = "button";
    button.className = "x-btn x-btn--pri";
    button.setAttribute("data-wl-create", "1");
    button.textContent = "创建工作负载";
    button.onclick = function () { openWorkloadForm(args); };
    toolbar.appendChild(button);
  }

  function openWorkloadForm(args) {
    if (!can("workload:write")) return runtime.toast("需要 workload:write 权限");
    var namespaces = args.namespaces || [];
    var kind = args.kind === "deployments" ? "Deployment" : args.kind === "statefulsets" ? "StatefulSet" :
      args.kind === "daemonsets" ? "DaemonSet" : args.kind === "jobs" ? "Job" : args.kind === "cronjobs" ? "CronJob" : "Pod";
    var ns = args.namespace || namespaces[0] && namespaces[0].name || "";
    var body = '<div class="x-row2"><label><span class="x-req">资源类型</span>' + optionsHtml(WORKLOAD_KINDS, kind, "kind") +
      '</label><label><span class="x-req">命名空间</span>' + nameSpaceOptions(namespaces, ns) + '</label></div>' +
      '<div class="x-row2"><label><span class="x-req">名称</span><input class="x-input x-mono" name="name" required maxlength="63" value="new-workload" /></label>' +
      '<label><span>镜像</span><input class="x-input x-mono" name="image" value="nginx:stable" /></label></div>' +
      '<div class="x-row2"><label><span>副本数</span><input class="x-input" type="number" min="0" name="replicas" value="1" /></label>' +
      '<label><span>容器端口</span><input class="x-input" type="number" min="0" max="65535" name="container_port" value="8080" /></label></div>' +
      '<label data-service-field hidden><span class="x-req">StatefulSet Service</span><input class="x-input x-mono" name="service_name" value="new-workload" /></label>' +
      '<div data-schedule-field hidden class="x-row2"><label><span class="x-req">CronJob 计划</span><input class="x-input x-mono" name="schedule" value="0 * * * *" /></label>' +
      '<label><span>时区</span><input class="x-input x-mono" name="timezone" placeholder="Asia/Shanghai" /></label></div>' +
      '<label data-restart-field hidden><span>任务重启策略</span>' + optionsHtml(["Never", "OnFailure"], "Never", "restart_policy") + "</label>" +
      '<button class="x-btn" type="button" data-check-yaml>服务端校验</button>' +
      '<details class="x-adv"><summary>配置预览</summary><pre class="x-log" data-manifest-preview style="max-height:320px;overflow:auto;white-space:pre-wrap"></pre></details>' +
      '<p class="x-frac" data-form-status role="status"></p>';
    var ai = runtime.aiDraftConfig("k8s.workload.create", "在 prod 创建一个 2 副本的 api Deployment，镜像 nginx:stable，容器端口 8080。", {
      kind: runtime.aiField("enum", "Kubernetes 工作负载 Kind", { values: WORKLOAD_KINDS.map(function (x) { return x.v; }) }),
      namespace: runtime.aiField("enum", "目标命名空间", { values: namespaces.map(function (x) { return x.name || x.metadata && x.metadata.name; }).filter(Boolean) }),
      name: runtime.aiField("string", "工作负载名称，使用 DNS 标签格式", { maxLength: 63 }),
      image: runtime.aiField("string", "容器镜像与标签", { maxLength: 512 }),
      replicas: runtime.aiField("number", "副本数"),
      container_port: runtime.aiField("number", "容器端口"),
      schedule: runtime.aiField("string", "CronJob cron 表达式", { maxLength: 100 }),
      timezone: runtime.aiField("string", "CronJob IANA 时区"),
      service_name: runtime.aiField("string", "StatefulSet 对应的 Service 名称"),
      restart_policy: runtime.aiField("enum", "Job/CronJob 重启策略", { values: ["Never", "OnFailure"] })
    }, true);
    var form = runtime.formDialog("创建工作负载", body, "创建", function (f, bad) {
      if (!can("workload:write")) return bad("需要 workload:write 权限");
      if (f._xsBusy || !active()) return;
      var manifest = workloadManifest(f), yaml = objectYaml(manifest);
      if (f._xsValidatedYaml !== yaml) return bad("请先运行服务端校验，再提交创建");
      f._xsBusy = true;
      ensureWorkloadAbsent(args.cluster.id, manifest).then(function () {
        if (!active()) throw new Error("页面或集群已切换，请重新打开表单");
        return runtime.apiPost(pathFor(args.cluster.id, "/apply"), { yaml: yaml, namespace: manifest.metadata.namespace, force: false });
      }).then(function (result) {
        var issue = applyIssue(result);
        if (issue) throw new Error(issue);
        if (!active()) return;
        runtime.closeModal();
        runtime.toast("工作负载已创建");
        runtime.repaint();
      }).catch(function (err) { if (active()) bad(failMessage(err)); }).finally(function () { f._xsBusy = false; });
    }, ai);
    if (!form) return;
    function active() {
      return form.isConnected !== false && (!runtime.alive || runtime.alive(args.seq, args.el));
    }
    var status = form.querySelector("[data-form-status]");
    var update = function () {
      form._xsValidatedYaml = "";
      var selected = form.elements.kind.value;
      form.querySelector("[data-schedule-field]").hidden = selected !== "CronJob";
      form.querySelector("[data-service-field]").hidden = selected !== "StatefulSet";
      form.querySelector("[data-restart-field]").hidden = ["Job", "CronJob"].indexOf(selected) < 0;
      form.elements.replicas.disabled = ["Deployment", "StatefulSet"].indexOf(selected) < 0;
      try {
        form.querySelector("[data-manifest-preview]").textContent = objectYaml(workloadManifest(form));
        if (status) status.textContent = "";
      } catch (e) {
        form.querySelector("[data-manifest-preview]").textContent = "";
        if (status) status.textContent = errorText(e);
      }
    };
    form.addEventListener("input", update);
    form.addEventListener("change", update);
    form.querySelector("[data-check-yaml]").onclick = function () {
      if (!can("workload:write") || form._xsBusy || !active()) return;
      var manifest, yaml;
      form._xsValidatedYaml = "";
      try { manifest = workloadManifest(form); yaml = objectYaml(manifest); } catch (err) {
        if (status) status.textContent = errorText(err);
        return;
      }
      form._xsBusy = true;
      if (status) status.textContent = "正在执行服务端校验";
      ensureWorkloadAbsent(args.cluster.id, manifest).then(function () {
        if (!active()) throw new Error("页面或集群已切换");
        return runtime.apiPost(pathFor(args.cluster.id, "/apply"), { yaml: yaml, namespace: manifest.metadata.namespace, dry_run: true, force: false });
      }).then(function (result) {
          var issue = applyIssue(result);
          if (issue) throw new Error(issue);
          if (!active()) return;
          if (objectYaml(workloadManifest(form)) !== yaml) throw new Error("配置已变更，请重新校验");
          form._xsValidatedYaml = yaml;
          if (status) status.textContent = "校验通过";
        }).catch(function (err) {
          form._xsValidatedYaml = "";
          if (active() && status) status.textContent = failMessage(err);
        }).finally(function () { form._xsBusy = false; });
    };
    update();
  }

  function rbacKindInfo(kind) {
    var value = String(kind || "").toLowerCase().replace(/[^a-z]/g, "");
    return RBAC_KINDS.filter(function (entry) {
      return entry.path === value || entry.kind.toLowerCase() === value ||
        entry.kind.toLowerCase() + "s" === value;
    })[0];
  }

  function rbacRows(data) {
    var source = data && (data.resources || data.items || data);
    var result = {};
    RBAC_KINDS.forEach(function (entry) { result[entry.kind] = []; });
    if (Array.isArray(source)) {
      source.forEach(function (item) {
        var info = rbacKindInfo(item.kind || item.type || item.resource);
        if (info) result[info.kind].push(item);
      });
    } else if (source && typeof source === "object") {
      RBAC_KINDS.forEach(function (entry) {
        var raw = source[entry.kind] || source[entry.path] || source[entry.kind[0].toLowerCase() + entry.kind.slice(1)];
        if (Array.isArray(raw)) result[entry.kind] = raw;
        else if (raw && Array.isArray(raw.items)) result[entry.kind] = raw.items;
      });
    }
    return result;
  }

  function rbacCapability(data, entry) {
    return data && data.resources && data.resources[entry.path] || { available: true, state: "available" };
  }

  function rbacPrecondition(yaml, key) {
    var match = String(yaml || "").match(new RegExp("(?:^|\\n)\\s+" + key + "\\s*:\\s*[\\\"']?([^\\\"'\\n#]+)", "i"));
    return match ? match[1].trim() : "";
  }

  function objectYaml(obj) {
    if (typeof obj === "string") return obj;
    return yamlValue(obj || {}, 0) + "\n";
  }

  function resourceObject(detail) {
    var object = detail && (detail.object || detail.manifest || (detail.metadata && detail));
    var raw = typeof object === "string" ? object : detail && detail.yaml;
    if ((!object || typeof object === "string") && raw) {
      var parse = runtime && runtime.parseYaml || root.jsyaml && root.jsyaml.load || root.YAML && root.YAML.parse || parseYaml;
      if (!parse) throw new Error("资源详情未返回完整对象，无法安全回填；请更新详情接口或加载 YAML 解析器");
      object = parse(raw);
    }
    if (!object || typeof object !== "object" || Array.isArray(object) || !object.kind || !object.metadata || !object.metadata.name) {
      throw new Error("资源详情不完整，已停止编辑");
    }
    return clone(object);
  }

  function input(label, name, value, extra) {
    return '<label><span>' + esc(label) + '</span><input class="x-input x-mono" name="' + esc(name) + '" value="' +
      esc(value == null ? "" : value) + '" ' + (extra || "") + " /></label>";
  }

  function splitValues(value) {
    return String(value || "").split(/[,\n]/).map(function (item) { return item.trim(); }).filter(Boolean);
  }

  function rowHtml(columns, values) {
    return '<tr data-form-row data-original="' + esc(JSON.stringify(values || {})) + '">' + columns.map(function (column) {
      var value = values && values[column.key];
      if (Array.isArray(value)) value = value.join(", ");
      var control = column.options ? optionsHtml(column.options, value == null ? column.defaultValue : value, column.key) :
        '<input class="x-input x-mono" name="' + esc(column.key) + '" aria-label="' + esc(column.label) + '" value="' + esc(value == null ? "" : value) + '" />';
      return '<td style="min-width:130px">' + control + "</td>";
    }).join("") + '<td><button class="x-btn" type="button" data-remove-row aria-label="删除行" title="删除行">×</button></td></tr>';
  }

  function rowsHtml(key, label, columns, values) {
    return '<fieldset data-rowset="' + key + '" style="min-width:0"><legend>' + esc(label) + '</legend><div class="x-table-wrap"><table class="x-table"><thead><tr>' +
      columns.map(function (column) { return "<th>" + esc(column.label) + "</th>"; }).join("") + '<th></th></tr></thead><tbody>' +
      (values || []).map(function (value) { return rowHtml(columns, value); }).join("") +
      '</tbody></table></div><button class="x-btn" type="button" data-add-row>添加</button></fieldset>';
  }

  function bindRows(form, key, columns, changed) {
    var set = form.querySelector('[data-rowset="' + key + '"]');
    if (!set) return;
    set.addEventListener("click", function (event) {
      var remove = event.target.closest("[data-remove-row]");
      if (remove) { remove.closest("tr").remove(); if (changed) changed(); }
    });
    set.querySelector("[data-add-row]").onclick = function () {
      set.querySelector("tbody").insertAdjacentHTML("beforeend", rowHtml(columns, {}));
      var rows = set.querySelectorAll("[data-form-row]");
      var first = rows[rows.length - 1].querySelector("input,select");
      if (first) first.focus();
      if (changed) changed();
    };
  }

  function readRows(form, key, columns) {
    return Array.prototype.map.call(form.querySelectorAll('[data-rowset="' + key + '"] [data-form-row]'), function (row) {
      var result = JSON.parse(row.getAttribute("data-original") || "{}");
      columns.forEach(function (column) {
        var value = row.querySelector('[name="' + column.key + '"]').value.trim();
        if (column.array) result[column.key] = splitValues(value);
        else result[column.key] = value;
      });
      return result;
    });
  }

  // Keep unrepresented fields as typed rows, including nested arrays and empty containers.
  function advancedLeaves(value, path, output) {
    if (value && typeof value === "object" && Object.keys(value).length) {
      Object.keys(value).forEach(function (key) { advancedLeaves(value[key], path.concat(key), output); });
    } else if (path.length) {
      output.push({ path: "/" + path.map(function (key) { return key.replace(/~/g, "~0").replace(/\//g, "~1"); }).join("/"),
        type: value === null ? "null" : Array.isArray(value) ? "array" : typeof value,
        value: value && typeof value === "object" || value === null ? "" : String(value) });
    }
    return output;
  }

  var ADVANCED_COLUMNS = [
    { key: "path", label: "字段路径" },
    { key: "type", label: "类型", options: ["string", "number", "boolean", "null", "object", "array"], defaultValue: "string" },
    { key: "value", label: "值" }
  ];
  var RULE_COLUMNS = [
    { key: "apiGroups", label: "API 组（逗号分隔）", array: true },
    { key: "resources", label: "资源（逗号分隔）", array: true },
    { key: "verbs", label: "操作（逗号分隔）", array: true },
    { key: "resourceNames", label: "资源名称（逗号分隔）", array: true },
    { key: "nonResourceURLs", label: "非资源 URL（逗号分隔）", array: true }
  ];
  var SUBJECT_COLUMNS = [
    { key: "kind", label: "主体类型", options: ["ServiceAccount", "User", "Group"], defaultValue: "ServiceAccount" },
    { key: "name", label: "名称" }, { key: "namespace", label: "命名空间" }, { key: "apiGroup", label: "API 组" }
  ];

  function advancedHtml(object) {
    var rows = Object.keys(object).length ? advancedLeaves(object, [], []) : [];
    return '<details class="x-adv"><summary>高级字段</summary>' + rowsHtml("advanced", "配置字段", ADVANCED_COLUMNS, rows) + "</details>";
  }

  function extrasOf(object, excluded) {
    var extra = clone(object || {});
    excluded.concat(["status", "apiVersion", "kind"]).forEach(function (key) { delete extra[key]; });
    if (extra.metadata) {
      ["name", "namespace", "managedFields", "creationTimestamp", "uid", "resourceVersion"].forEach(function (key) { delete extra.metadata[key]; });
      if (!Object.keys(extra.metadata).length) delete extra.metadata;
    }
    return extra;
  }

  function advancedObject(rows) {
    var output = {};
    rows.forEach(function (row) {
      if (!row.path || row.path.charAt(0) !== "/") throw new Error("高级字段路径必须以 / 开头");
      var keys = row.path.slice(1).split("/").map(function (key) {
        if (/~(?![01])/.test(key)) throw new Error("高级字段路径转义无效");
        return key.replace(/~1/g, "/").replace(/~0/g, "~");
      });
      if (keys.some(function (key) { return !key || ["__proto__", "constructor", "prototype"].indexOf(key) >= 0; })) throw new Error("高级字段路径无效");
      if (["apiVersion", "kind", "status", "rules", "subjects", "roleRef"].indexOf(keys[0]) >= 0 ||
          keys[0] === "metadata" && ["name", "namespace", "uid", "resourceVersion", "managedFields"].indexOf(keys[1]) >= 0) {
        throw new Error("此字段由主表单或资源身份管理");
      }
      var value = row.value;
      if (row.type === "number") {
        if (!String(value).trim() || !Number.isFinite(Number(value))) throw new Error("高级数字字段无效");
        value = Number(value);
      } else if (row.type === "boolean") {
        if (value !== "true" && value !== "false") throw new Error("布尔字段只能填写 true 或 false");
        value = value === "true";
      } else if (row.type === "null") value = null;
      else if (row.type === "array") value = [];
      else if (row.type === "object") value = {};
      else if (row.type !== "string") throw new Error("高级字段类型无效");
      var target = output;
      keys.slice(0, -1).forEach(function (key, index) {
        if (!Object.prototype.hasOwnProperty.call(target, key)) target[key] = /^\d+$/.test(keys[index + 1]) ? [] : {};
        if (!target[key] || typeof target[key] !== "object") throw new Error("高级字段路径相互冲突");
        target = target[key];
      });
      var last = keys[keys.length - 1];
      if (Object.prototype.hasOwnProperty.call(target, last)) throw new Error("高级字段路径重复");
      if (Array.isArray(target) && (!/^(0|[1-9]\d*)$/.test(last) || Number(last) > 10000)) throw new Error("高级数组下标无效");
      target[last] = value;
    });
    function dense(value) {
      if (!value || typeof value !== "object") return;
      if (Array.isArray(value) && Object.keys(value).length !== value.length) throw new Error("高级数组下标必须连续");
      Object.keys(value).forEach(function (key) { dense(value[key]); });
    }
    dense(output);
    return output;
  }

  function formExtras(form) { return advancedObject(readRows(form, "advanced", ADVANCED_COLUMNS)); }

  function yamlScalar(raw) {
    var value = String(raw || "").trim();
    if (!value || value === "~" || value === "null") return null;
    if (value === "{}") return {};
    if (value === "[]") return [];
    if (value === "true" || value === "false") return value === "true";
    if (/^-?(?:\d+|\d*\.\d+)$/.test(value)) return Number(value);
    if ((value.charAt(0) === '"' && value.charAt(value.length - 1) === '"') ||
        (value.charAt(0) === "'" && value.charAt(value.length - 1) === "'")) {
      try { return JSON.parse(value); } catch (err) { return value.slice(1, -1).replace(/''/g, "'"); }
    }
    if (value.charAt(0) === "[" || value.charAt(0) === "{") {
      try { return JSON.parse(value.replace(/'/g, '"')); } catch (err) {}
    }
    return value.replace(/\s+#.*$/, "").trim();
  }

  // Parses the server's ordinary Kubernetes YAML subset for safe edit backfill.
  function parseYaml(raw) {
    var lines = String(raw || "").replace(/\r/g, "").split("\n").map(function (line, index) {
      var text = line.replace(/\s+#.*$/, "");
      return { n: index + 1, indent: (text.match(/^\s*/) || [""])[0].length, text: text.trim() };
    }).filter(function (line) { return line.text && line.text !== "---" && line.text !== "..."; });
    if (!lines.length) throw new Error("资源详情为空");
    function keyValue(text) {
      var match = text.match(/^([^:]+):(?:\s+(.*))?$/);
      if (!match) throw new Error("无法解析 YAML 第 " + lines[0].n + " 行");
      return { key: match[1].trim().replace(/^['"]|['"]$/g, ""), value: match[2] == null ? null : yamlScalar(match[2]) };
    }
    function parseBlock(position, indent) {
      var array = lines[position] && lines[position].indent === indent && lines[position].text.indexOf("- ") === 0;
      var target = array ? [] : {};
      while (position < lines.length && lines[position].indent === indent) {
        var line = lines[position].text;
        if (array) {
          if (line.indexOf("- ") !== 0) break;
          var itemText = line.slice(2).trim();
          if (!itemText) {
            var nested = parseBlock(position + 1, lines[position + 1] && lines[position + 1].indent || indent + 2);
            target.push(nested.value); position = nested.position; continue;
          }
          if (itemText.indexOf(":") >= 0 && !/^['"].*['"]$/.test(itemText)) {
            var first = keyValue(itemText), item = {};
            if (first.value !== null) item[first.key] = first.value;
            var next = lines[position + 1];
            if (first.value === null && next && next.indent > indent) {
              var nestedValue = parseBlock(position + 1, next.indent); item[first.key] = nestedValue.value; position = nestedValue.position;
            } else position++;
            while (position < lines.length && lines[position].indent > indent) {
              var extra = keyValue(lines[position].text), nextLine = lines[position + 1];
              if (extra.value === null && nextLine && nextLine.indent > lines[position].indent) {
                var child = parseBlock(position + 1, nextLine.indent); item[extra.key] = child.value; position = child.position;
              } else { item[extra.key] = extra.value; position++; }
            }
            target.push(item); continue;
          }
          target.push(yamlScalar(itemText)); position++; continue;
        }
        if (line.indexOf("- ") === 0) break;
        var entry = keyValue(line), childLine = lines[position + 1];
        if (entry.value === null && childLine && childLine.indent > indent) {
          var childBlock = parseBlock(position + 1, childLine.indent); target[entry.key] = childBlock.value; position = childBlock.position;
        } else { target[entry.key] = entry.value; position++; }
      }
      return { value: target, position: position };
    }
    var parsed = parseBlock(0, lines[0].indent).value;
    if (!parsed || !parsed.kind || !parsed.metadata || !parsed.metadata.name) throw new Error("资源详情缺少 kind 或 metadata.name");
    return parsed;
  }

  function rbacManifest(values, base) {
    var info = rbacKindInfo(values.kind);
    if (!info) throw new Error("不支持的 RBAC 类型");
    var original = clone(base || {}), object = clone(values.extra || {});
    object.apiVersion = info.kind === "ServiceAccount" ? "v1" : "rbac.authorization.k8s.io/v1";
    object.kind = info.kind;
    object.metadata = Object.assign({}, object.metadata, { name: requireResourceName(values.name) });
    if (info.namespaced) object.metadata.namespace = requireName(values.namespace, "命名空间");
    else delete object.metadata.namespace;
    if (original.metadata) ["uid", "resourceVersion"].forEach(function (key) {
      if (original.metadata[key]) object.metadata[key] = original.metadata[key];
    });
    if (/Binding$/.test(info.kind)) {
      if (["Role", "ClusterRole"].indexOf(values.role_kind) < 0 || info.kind === "ClusterRoleBinding" && values.role_kind !== "ClusterRole") throw new Error("绑定的角色类型无效");
      object.roleRef = Object.assign({}, original.roleRef, { apiGroup: "rbac.authorization.k8s.io", kind: values.role_kind, name: requireResourceName(values.role_name) });
      if (!values.subjects || !values.subjects.length) throw new Error("请添加至少一个绑定主体");
      object.subjects = values.subjects.map(function (subject) {
        var item = clone(subject);
        if (["ServiceAccount", "User", "Group"].indexOf(item.kind) < 0 || !item.name) throw new Error("请填写有效的主体类型和名称");
        if (item.kind === "ServiceAccount") {
          item.namespace = requireName(item.namespace || values.namespace, "ServiceAccount 命名空间");
          item.apiGroup = "";
        } else {
          delete item.namespace;
          item.apiGroup = "rbac.authorization.k8s.io";
        }
        return item;
      });
    } else if (/Role$/.test(info.kind)) {
      object.rules = (values.rules || []).map(function (rule) {
        var item = clone(rule);
        if (!item.verbs || !item.verbs.length) throw new Error("规则操作不能为空");
        if (item.resources && item.resources.length) {
          if (item.nonResourceURLs && item.nonResourceURLs.length) throw new Error("资源规则和非资源 URL 需分行填写");
          if (!item.apiGroups || !item.apiGroups.length) item.apiGroups = [""];
          delete item.nonResourceURLs;
        } else {
          if (info.kind !== "ClusterRole" || !item.nonResourceURLs || !item.nonResourceURLs.length) throw new Error("请填写资源；非资源 URL 仅限 ClusterRole");
          delete item.apiGroups; delete item.resources; delete item.resourceNames;
        }
        if (item.resourceNames && !item.resourceNames.length) delete item.resourceNames;
        return item;
      });
    } else object.automountServiceAccountToken = !!values.automount;
    return object;
  }

  function previewForm(form, build, columns) {
    var update = function () {
      var preview = form.querySelector("[data-manifest-preview]"), status = form.querySelector("[data-form-status]");
      try { preview.textContent = objectYaml(build(form)); if (status) status.textContent = ""; }
      catch (err) { preview.textContent = ""; if (status) status.textContent = errorText(err); }
    };
    Object.keys(columns || {}).forEach(function (key) { bindRows(form, key, columns[key], update); });
    bindRows(form, "advanced", ADVANCED_COLUMNS, update);
    form.addEventListener("input", update);
    form.addEventListener("change", update);
    update();
    return update;
  }

  function configurationPreview() {
    return '<details class="x-adv"><summary>配置预览</summary><pre class="x-log" data-manifest-preview style="max-height:320px;overflow:auto;white-space:pre-wrap"></pre></details>' +
      '<p class="x-frac" role="status" data-form-status></p>';
  }

  function submitGoverned(form, cluster, suffix, manifest, bad, message, beforeSubmit) {
    if (form._xsBusy || form.isConnected === false) return;
    var permission = suffix.indexOf("/rbac") === 0 ? "rbac:write" : "storage:write";
    if (!can(permission)) return bad("需要 " + permission + " 权限");
    form._xsBusy = true;
    Promise.resolve(beforeSubmit ? beforeSubmit() : null).then(function () {
      if (!can(permission)) throw new Error("需要 " + permission + " 权限");
      return runtime.apiPost(pathFor(cluster.id, suffix), { yaml: objectYaml(manifest), namespace: manifest.metadata.namespace || "" });
    }).then(function (result) {
      var issue = applyIssue(result);
      if (issue) throw new Error(issue);
      if (form.isConnected === false) return;
      runtime.closeModal(); runtime.toast(message); runtime.repaint();
    }).catch(function (err) { if (form.isConnected !== false) bad(failMessage(err)); }).finally(function () { form._xsBusy = false; });
  }

  function openRbacForm(cluster, namespaces, resource, existingResources) {
    if (!can("rbac:write")) return runtime.toast("需要 rbac:write 权限");
    var initial;
    try { initial = resource ? resourceObject(resource) : {}; } catch (err) { return runtime.toast(errorText(err)); }
    var info = rbacKindInfo(initial.kind) || RBAC_KINDS[1];
    var meta = initial.metadata || {};
    var namespace = meta.namespace || namespaces[0] && (namespaces[0].name || namespaces[0].metadata && namespaces[0].metadata.name) || "";
    function ensureNewName(manifest) {
      if (resource) return Promise.resolve();
      var capability = existingResources && existingResources[rbacKindInfo(manifest.kind).path];
      if (!capability || !capability.available) return Promise.reject(new Error("无法确认目标类型的现有资源，已停止创建"));
      var collision = list(capability.items).some(function (item) {
        var itemMeta = item.metadata || {};
        return (item.name || itemMeta.name) === manifest.metadata.name &&
          (!manifest.metadata.namespace || (item.namespace || itemMeta.namespace) === manifest.metadata.namespace);
      });
      return collision ? Promise.reject(new Error("同名 RBAC 资源已存在；编辑现有资源或使用其他名称")) : Promise.resolve();
    }
    var body = '<div class="x-row2"><label><span class="x-req">类型</span>' +
      optionsHtml(RBAC_KINDS.map(function (x) { return { v: x.kind, t: x.label }; }), info.kind, "kind") + "</label>" +
      '<label><span>命名空间</span>' + nameSpaceOptions(namespaces, namespace) + "</label></div>" +
      input("名称", "name", meta.name || "new-role", "required") +
      '<div data-rbac-rules>' + rowsHtml("rules", "权限规则", RULE_COLUMNS, initial.rules || []) + "</div>" +
      '<div data-rbac-binding><div class="x-row2"><label><span>角色类型</span>' + optionsHtml(["Role", "ClusterRole"], initial.roleRef && initial.roleRef.kind || "Role", "role_kind") + "</label>" +
      input("角色名称", "role_name", initial.roleRef && initial.roleRef.name || "") + "</div>" +
      rowsHtml("subjects", "绑定主体", SUBJECT_COLUMNS, initial.subjects || []) + "</div>" +
      '<label data-rbac-account><input type="checkbox" name="automount"' + (initial.automountServiceAccountToken !== false ? " checked" : "") + " />自动挂载 ServiceAccount Token</label>" +
      advancedHtml(extrasOf(initial, ["rules", "subjects", "roleRef", "automountServiceAccountToken"])) + configurationPreview();
    function build(form) {
      return rbacManifest({ kind: valueOf(form, "kind"), name: valueOf(form, "name"), namespace: valueOf(form, "namespace"),
        role_kind: valueOf(form, "role_kind"), role_name: valueOf(form, "role_name"),
        subjects: readRows(form, "subjects", SUBJECT_COLUMNS), rules: readRows(form, "rules", RULE_COLUMNS),
        automount: form.elements.automount.checked, extra: formExtras(form) }, initial);
    }
    var form = runtime.formDialog(resource ? "编辑集群 RBAC" : "新建集群 RBAC", body, "保存", function (f, bad) {
      if (!can("rbac:write")) return bad("需要 rbac:write 权限");
      var manifest;
      try { manifest = build(f); } catch (err) { return bad(errorText(err)); }
      submitGoverned(f, cluster, "/rbac/apply", manifest, bad, "RBAC 配置已提交", function () { return ensureNewName(manifest); });
    }, runtime.aiDraftConfig("k8s.rbac", "创建 team-a 命名空间的只读 Pod Role。", {
      kind: runtime.aiField("enum", "RBAC 类型", { values: RBAC_KINDS.map(function (item) { return item.kind; }) }),
      namespace: runtime.aiField("string", "命名空间"), name: runtime.aiField("string", "资源名称"),
      role_kind: runtime.aiField("enum", "绑定角色类型", { values: ["Role", "ClusterRole"] }),
      role_name: runtime.aiField("string", "绑定角色名称")
    }, true));
    if (!form) return;
    if (resource) {
      form.elements.name.readOnly = true;
      form.elements.kind.disabled = true;
      form.elements.namespace.disabled = true;
      form.elements.role_kind.disabled = true;
      form.elements.role_name.readOnly = true;
    }
    function updateKind() {
      var selected = rbacKindInfo(valueOf(form, "kind"));
      form.querySelector("[data-rbac-rules]").hidden = !/Role$/.test(selected.kind);
      form.querySelector("[data-rbac-binding]").hidden = !/Binding$/.test(selected.kind);
      form.querySelector("[data-rbac-account]").hidden = selected.kind !== "ServiceAccount";
      form.elements.namespace.disabled = !!resource || !selected.namespaced;
      if (selected.kind === "ClusterRoleBinding") form.elements.role_kind.value = "ClusterRole";
    }
    form.addEventListener("change", updateKind);
    updateKind();
    previewForm(form, build, { rules: RULE_COLUMNS, subjects: SUBJECT_COLUMNS });
  }

  function renderRBAC(el, seq, cluster, box) {
    var base = pathFor(cluster.id, "");
    Promise.all([
      request(runtime.apiGet(base + "/rbac")),
      request(runtime.apiGet(base + "/namespaces"))
    ]).then(function (results) {
      if (!runtime.alive(seq, el) || !box) return;
      var rbac = results[0];
      var writable = can("rbac:write");
      if (!rbac.ok) {
        var detail = errorText(rbac.error);
        var missing = rbac.error && (rbac.error.status === 404 || /not found|404/i.test(String(rbac.error.message || "")));
        box.innerHTML = panel("集群 RBAC", '<div role="alert" style="margin-top:10px">' + esc(detail) + "</div>" +
          (missing ? empty("当前服务端未提供集群 RBAC 读取接口", "需要部署 GET /clusters/:id/rbac 与 RBAC apply/delete 接口") :
            empty("无法读取 RBAC 资源", "请检查当前用户的集群读取权限及 Kubernetes RBAC 权限")));
        var create = box.querySelector("[data-rbac-create]");
        if (create) create.onclick = function () {
          openRbacForm(cluster, results[1].ok ? list(results[1].data) : [], null, {});
        };
        return;
      }
      var groups = rbacRows(rbac.data);
      var namespaces = results[1].ok ? list(results[1].data) : [];
      var sections = RBAC_KINDS.map(function (entry) {
        var capability = rbacCapability(rbac.data, entry);
        var rows = (groups[entry.kind] || []).map(function (resource) {
          var metadata = resource.metadata || resource.object && resource.object.metadata || {};
          var name = resource.name || metadata.name || "—";
          var ns = resource.namespace || metadata.namespace || "—";
          return [
            '<span class="x-name">' + esc(name) + "</span>",
            esc(ns),
            esc(resource.subjects && resource.subjects.length || resource.rules && resource.rules.length || "—"),
            writable ? '<span class="x-kacts"><button class="x-btn" type="button" data-rbac-edit="' + esc(entry.path) + '" data-name="' + esc(name) + '" data-ns="' + esc(ns === "—" ? "-" : ns) + '">编辑</button> ' +
              '<button class="x-btn" type="button" data-rbac-delete="' + esc(entry.path) + '" data-name="' + esc(name) + '" data-ns="' + esc(ns === "—" ? "-" : ns) + '">删除</button></span>' : '<span class="x-frac">只读</span>'
          ];
        });
        var status = capability.available ? rows.length + " 项" :
          (capability.state || "不可用") + (capability.error ? " · " + capability.error : "");
        return panel(entry.label, table(["名称", "命名空间", "规则/主体", "操作"], rows), status);
      }).join("");
      box.innerHTML = panel("集群 RBAC", (writable ? '<button class="x-btn x-btn--pri" type="button" data-rbac-create>新建 RBAC</button>' : "") +
        (results[1].ok ? "" : '<span class="x-frac" style="margin-left:10px">命名空间读取失败：' + esc(errorText(results[1].error)) + "</span>")) +
        sections;
      var createRbac = box.querySelector("[data-rbac-create]");
      if (createRbac) createRbac.onclick = function () { openRbacForm(cluster, namespaces, null, rbac.data.resources || {}); };
      Array.prototype.forEach.call(box.querySelectorAll("[data-rbac-edit]"), function (button) {
        button.onclick = function () {
          var kind = button.getAttribute("data-rbac-edit");
          var name = button.getAttribute("data-name");
          var ns = button.getAttribute("data-ns") || "-";
          var suffix = "/rbac/" + encodeURIComponent(kind) + "/" + encodeURIComponent(ns) + "/" + encodeURIComponent(name);
          runtime.apiGet(pathFor(cluster.id, suffix)).then(function (detail) {
            if (!runtime.alive(seq, el)) return;
            openRbacForm(cluster, namespaces, detail);
          }).catch(function (err) { runtime.toast("读取 RBAC YAML 失败：" + errorText(err)); });
        };
      });
      Array.prototype.forEach.call(box.querySelectorAll("[data-rbac-delete]"), function (button) {
        button.onclick = function () {
          if (!can("rbac:write")) return runtime.toast("需要 rbac:write 权限");
          var kind = button.getAttribute("data-rbac-delete");
          var name = button.getAttribute("data-name");
          var ns = button.getAttribute("data-ns") || "-";
          if (!root.confirm || !root.confirm("确认删除 " + kind + "/" + name + "？")) return;
          var suffix = "/rbac/" + encodeURIComponent(kind) + "/" + encodeURIComponent(ns) + "/" + encodeURIComponent(name);
          runtime.apiGet(pathFor(cluster.id, suffix)).then(function (detail) {
            var uid = rbacPrecondition(detail.yaml, "uid");
            var version = rbacPrecondition(detail.yaml, "resourceVersion");
            if (!uid || !version) throw new Error("读取不到 UID/resourceVersion，已停止删除");
            return runtime.apiDel(pathFor(cluster.id, suffix), { uid: uid, resource_version: version });
          }).then(function () {
            runtime.toast("RBAC 资源已删除");
            runtime.repaint();
          }).catch(function (err) { runtime.toast("删除失败：" + errorText(err)); });
        };
      });
    });
  }

  function backupItems(data) {
    var items = data && (data.items || data.backups || data.velero && data.velero.items) || [];
    return Array.isArray(items) ? items : list(items);
  }

  function scheduleYaml(form, cluster) {
    var name = requireName(valueOf(form, "name"), "Schedule 名称");
    var ns = requireName(valueOf(form, "namespace", "velero"), "命名空间");
    var location = valueOf(form, "storage_location");
    var schedule = cron(valueOf(form, "schedule"));
    var ttl = valueOf(form, "ttl", "720h");
    if (!/^\d+(?:\.\d+)?[smhd]$/.test(ttl)) throw new Error("保留时长需使用例如 720h 的格式");
    var include = splitValues(valueOf(form, "included_namespace"));
    var exclude = splitValues(valueOf(form, "excluded_namespace"));
    if (!include.length) include = ["*"];
    var manifest = {
      apiVersion: "velero.io/v1",
      kind: "Schedule",
      metadata: { name: name, namespace: ns },
      spec: {
        schedule: schedule,
        template: {
          includedNamespaces: include,
          ttl: ttl
        }
      }
    };
    if (location) manifest.spec.template.storageLocation = location;
    if (exclude.length) manifest.spec.template.excludedNamespaces = exclude;
    var labels = {};
    Array.prototype.forEach.call(form.querySelectorAll('[data-rowset="labels"] [data-form-row]'), function (row) {
      var key = valueOf(row, "key"), value = valueOf(row, "value");
      if (key) labels[key] = value;
    });
    if (Object.keys(labels).length) manifest.spec.template.labels = labels;
    return manifest;
  }

  function openScheduleForm(cluster, namespaces, locations, resource) {
    var meta = resource && (resource.metadata || {}) || {};
    var spec = resource && resource.spec || {};
    var template = spec.template || {};
    var included = template.includedNamespaces || [];
    var excluded = template.excludedNamespaces || [];
    var labels = Object.keys(template.labels || {}).map(function (key) { return { key: key, value: template.labels[key] }; });
    var body = '<div class="x-row2"><label><span class="x-req">Schedule 名称</span><input class="x-input x-mono" name="name" required value="' + esc(meta.name || "scheduled-backup") + '" /></label>' +
      '<label><span class="x-req">命名空间</span>' + nameSpaceOptions(namespaces, meta.namespace || "velero") + "</label></div>" +
      '<div class="x-row2"><label><span class="x-req">Cron 表达式</span><input class="x-input x-mono" name="schedule" required value="' + esc(spec.schedule || "0 2 * * *") + '" /></label>' +
      '<label><span>保留时长</span><input class="x-input x-mono" name="ttl" value="' + esc(template.ttl || "720h") + '" /></label></div>' +
      '<div class="x-row2"><label><span>Velero StorageLocation</span>' + optionsHtml(locations, template.storageLocation || "", "storage_location", "选择已发现的存储位置") + "</label>" +
      '<label><span>包含命名空间（* 表示全部）</span><input class="x-input x-mono" name="included_namespace" value="' + esc(included.join(",") || "*") + '" /></label></div>' +
      '<label><span>排除命名空间</span><input class="x-input x-mono" name="excluded_namespace" value="' + esc(excluded.join(", ")) + '" /></label>' +
      rowsHtml("labels", "备份标签", [{ key: "key", label: "标签 Key" }, { key: "value", label: "标签值" }], labels) +
      configurationPreview();
    var form = runtime.formDialog(resource ? "编辑 Velero 定时备份" : "创建 Velero 定时备份", body, "保存 Schedule", function (f, bad) {
      try { submitGoverned(f, cluster, "/backups/apply", scheduleYaml(f, cluster), bad, "Velero Schedule 已提交"); }
      catch (err) { bad(errorText(err)); }
    }, runtime.aiDraftConfig("k8s.velero.schedule", "每天凌晨 2 点备份 prod 命名空间，保留 30 天，使用已发现的主存储位置。", {
      name: runtime.aiField("string", "Schedule 名称"),
      schedule: runtime.aiField("string", "五段 Cron 表达式"),
      ttl: runtime.aiField("string", "Velero 保留时长，例如 720h"),
      included_namespace: runtime.aiField("string", "包含的命名空间，逗号分隔"),
      excluded_namespace: runtime.aiField("string", "排除的命名空间，逗号分隔")
    }, true));
    if (!form) return;
    previewForm(form, scheduleYaml, { labels: [{ key: "key", label: "标签 Key" }, { key: "value", label: "标签值" }] });
  }

  function renderBackup(el, seq, cluster, box) {
    var base = pathFor(cluster.id, "");
    Promise.all([
      request(runtime.apiGet(base + "/backups")),
      request(runtime.apiGet(base + "/storage/capabilities")),
      request(runtime.apiGet(base + "/storage/snapshots")),
      request(runtime.apiGet(base + "/storage/claims")),
      request(runtime.apiGet(base + "/namespaces"))
    ]).then(function (results) {
      if (!runtime.alive(seq, el) || !box) return;
      var backup = results[0].ok ? results[0].data || {} : {};
      var snapshots = results[2].ok ? list(results[2].data) : [];
      var capabilities = results[1].ok ? results[1].data && results[1].data.snapshots || {} : {};
      var snapshotClasses = capabilities.classes || [];
      var namespaces = results[4].ok ? list(results[4].data) : [];
      var locations = backup.storage_locations || backup.storageLocations || backup.velero && (backup.velero.storage_locations || backup.velero.storageLocations) || [];
      if (!Array.isArray(locations)) locations = list(locations);
      locations = locations.map(function (x) { return typeof x === "string" ? x : x.name || x.metadata && x.metadata.name; }).filter(Boolean);
      var backups = backupItems(backup);
      var schedules = backup.schedules || backup.velero && backup.velero.schedules || [];
      if (!Array.isArray(schedules)) schedules = list(schedules);
      var providers = backup.providers || backup.velero && backup.velero.providers || [];
      if (!Array.isArray(providers)) providers = list(providers);
      var activeTab = "snapshots";
      function draw() {
        var tabs = '<div class="x-toolbar" style="margin-bottom:10px"><button class="x-btn' + (activeTab === "snapshots" ? " x-btn--pri" : "") + '" type="button" data-backup-tab="snapshots">VolumeSnapshot</button>' +
          '<button class="x-btn' + (activeTab === "velero" ? " x-btn--pri" : "") + '" type="button" data-backup-tab="velero">Velero</button></div>';
        if (activeTab === "snapshots") {
          var snapshotReady = results[1].ok && capabilities.supported;
          var snapshotRows = snapshots.map(function (item) {
            var meta = item.metadata || {};
            return [esc(item.namespace || meta.namespace || "—"), '<span class="x-name">' + esc(item.name || meta.name || "—") + "</span>",
              esc(item.ready_to_use == null ? item.readyToUse == null ? "—" : item.readyToUse : item.ready_to_use),
              esc(item.source_pvc || item.sourcePVC || "—"), esc(item.class_name || item.volume_snapshot_class_name || "—")];
          });
          var tools = snapshotReady ? '<button class="x-btn x-btn--pri" type="button" data-snapshot-create>创建 VolumeSnapshot</button>' : "";
          var info = !results[1].ok
            ? '<p class="x-frac">快照能力读取失败：' + esc(errorText(results[1].error)) + "</p>"
            : (capabilities.supported ? "" : '<p class="x-frac">' + esc(capabilities.reason || "当前集群未确认 VolumeSnapshot 能力，创建入口已关闭。") + "</p>");
          box.innerHTML = panel("备份恢复", tabs + tools + info + table(["命名空间", "快照", "可恢复", "源 PVC", "SnapshotClass"], snapshotRows) +
            (!snapshotClasses.length ? '<p class="x-frac">未发现 VolumeSnapshotClass；仍可尝试使用集群默认类。</p>' : ""), snapshots.length + " 个快照");
          var snapshotCreate = box.querySelector("[data-snapshot-create]");
          if (snapshotCreate) snapshotCreate.onclick = function () {
            runtime.formDialog("创建 VolumeSnapshot", '<div class="x-row2"><label><span class="x-req">命名空间</span>' + nameSpaceOptions(namespaces, "") + '</label>' +
              '<label><span class="x-req">快照名称</span><input class="x-input x-mono" name="name" required /></label></div>' +
              '<label><span class="x-req">源 PVC</span><input class="x-input x-mono" name="source_pvc" required /></label>' +
              '<label><span>VolumeSnapshotClass</span>' + optionsHtml(snapshotClasses, "", "class_name", "使用集群默认类") + "</label>" +
              '<p class="x-frac">快照就绪状态由集群 CSI snapshot controller 返回。</p>', "创建", function (form, bad) {
              var payload = { namespace: form.elements.namespace.value, name: form.elements.name.value.trim(), source_pvc: form.elements.source_pvc.value.trim() };
              if (!payload.namespace || !payload.name || !payload.source_pvc) return bad("请填写命名空间、快照名和源 PVC");
              if (form.elements.class_name.value) payload.class_name = form.elements.class_name.value;
              runtime.apiPost(pathFor(cluster.id, "/storage/snapshots"), payload).then(function () {
                runtime.closeModal(); runtime.toast("VolumeSnapshot 已提交"); runtime.repaint();
              }).catch(function (err) { bad(errorText(err)); });
            }, runtime.aiDraftConfig("k8s.snapshot.create", "为 prod/data 创建名为 data-before-upgrade 的 VolumeSnapshot。", {
              namespace: runtime.aiField("enum", "命名空间", { values: namespaces.map(function (x) { return x.name || x.metadata && x.metadata.name; }).filter(Boolean) }),
              name: runtime.aiField("string", "VolumeSnapshot 名称", { maxLength: 63 }),
              source_pvc: runtime.aiField("string", "已存在的源 PVC 名称", { maxLength: 253 }),
              class_name: runtime.aiField("enum", "已发现的 VolumeSnapshotClass", { values: [""].concat(snapshotClasses.map(function (x) { return typeof x === "string" ? x : x.name; })) })
            }, true));
          };
        } else {
          var backupRows = backups.map(function (item) {
            var meta = item.metadata || {};
            return [esc(item.kind || "Backup"), esc(meta.namespace || item.namespace || "—"), esc(item.name || meta.name || "—"),
              esc(item.phase || item.status && item.status.phase || item.status || "—"), esc(item.created_at || meta.creationTimestamp || "—")];
          });
          var scheduleRows = schedules.map(function (item) {
            var meta = item.metadata || {};
            return [esc(meta.namespace || item.namespace || "—"), esc(meta.name || item.name || "—"),
              esc(item.schedule || item.spec && item.spec.schedule || "—"),
              '<button class="x-btn" type="button" data-schedule-edit="' + esc(meta.name || item.name || "") + '">查看/编辑</button>'];
          });
          var providerNames = providers.map(function (x) { return typeof x === "string" ? x : x.name || x.kind; }).filter(Boolean);
          var unavailable = results[0].ok ? "" : '<div role="alert">' + esc(errorText(results[0].error)) + "</div>" +
            empty("Velero 备份接口不可用", "需要 GET /clusters/:id/backups 与 POST /clusters/:id/backups/apply；不会显示模拟数据。");
          box.innerHTML = panel("备份恢复", tabs + unavailable +
            (results[0].ok ? '<div class="x-toolbar"><button class="x-btn x-btn--pri" type="button" data-schedule-create>新建定时备份</button><span class="x-frac">Provider：' + esc(providerNames.join(", ") || "未发现") + " · StorageLocation：" + esc(locations.join(", ") || "未发现") + "</span></div>" +
              panel("Velero Schedule", table(["命名空间", "名称", "计划", "操作"], scheduleRows)) +
              panel("备份记录", table(["类型", "命名空间", "名称", "状态", "创建时间"], backupRows)) : ""));
          var create = box.querySelector("[data-schedule-create]");
          if (create) create.onclick = function () { openScheduleForm(cluster, namespaces, locations, null); };
          Array.prototype.forEach.call(box.querySelectorAll("[data-schedule-edit]"), function (button) {
            button.onclick = function () {
              var name = button.getAttribute("data-schedule-edit");
              var item = schedules.filter(function (entry) {
                var meta = entry.metadata || {};
                return (meta.name || entry.name) === name;
              })[0];
              if (item) openScheduleForm(cluster, namespaces, locations, item);
            };
          });
        }
        Array.prototype.forEach.call(box.querySelectorAll("[data-backup-tab]"), function (button) {
          button.onclick = function () { activeTab = button.getAttribute("data-backup-tab"); draw(); };
        });
      }
      draw();
    });
  }

  function register(ctx) {
    if (!ctx || !ctx.body || typeof ctx.apiGet !== "function") return false;
    runtime = ctx;
    ctx.body.rbac = renderRBAC;
    ctx.body.backup = renderBackup;
    return true;
  }

  var api = root.XSClusterExtensions || {};
  api.register = register;
  api.decorateWorkloads = decorateWorkloads;
  api.ensureComponents = ensureComponents;
  api.componentOptions = componentOptions;
  api.componentState = componentState;
  api.workloadKinds = WORKLOAD_KINDS.slice();
  api.buildWorkloadManifest = workloadManifest;
  api.serializeManifest = objectYaml;
  api.checkApplyResult = applyIssue;
  api.checkCron = cron;
  root.XSClusterExtensions = api;
})(typeof window !== "undefined" ? window : globalThis);
