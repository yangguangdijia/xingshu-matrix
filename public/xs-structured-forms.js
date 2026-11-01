(function (root) {
  "use strict";

  var sequence = 0;
  var states = new WeakMap();
  var usedIds = new Set();
  var forbiddenKeys = new Set(["__proto__", "prototype", "constructor"]);
  var kinds = ["keyValue", "selector", "servicePorts", "ingressRules", "hpa", "cronJob", "configMap", "secret", "componentEndpoint", "objectFields"];
  var own = function (object, key) { return Object.prototype.hasOwnProperty.call(object, key); };
  var copy = function (value) {
    return parseJSON(JSON.stringify(value == null ? {} : value, function (_key, entry) {
      if (typeof entry === "number" && !Number.isFinite(entry)) throw new Error("配置包含无效数值");
      return entry;
    }));
  };

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (character) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[character];
    });
  }

  function parseJSON(value) {
    if (typeof value !== "string" || value.length > 2000000) throw new Error("配置数据无效或过大");
    var result = JSON.parse(value, function (key, entry) {
      if (forbiddenKeys.has(key)) throw new Error("配置包含不安全的键");
      return entry;
    });
    function checkDepth(entry, depth) {
      if (depth > 64) throw new Error("配置嵌套层数过多");
      if (entry && typeof entry === "object") Object.keys(entry).forEach(function (key) { checkDepth(entry[key], depth + 1); });
    }
    checkDepth(result, 0);
    return result;
  }

  function uid(preferred) {
    var base = String(preferred || "x-sf").replace(/[^a-zA-Z0-9_-]/g, "-").slice(0, 90) || "x-sf";
    var id = preferred ? base : base + "-" + (++sequence);
    while (usedIds.has(id) || (root.document && root.document.getElementById(id))) id = base + "-" + (++sequence);
    usedIds.add(id);
    return id;
  }

  function list(value) { return Array.isArray(value) ? value : []; }
  function normalizeValue(kind, value) {
    if (value == null) value = kind === "servicePorts" ? [] : {};
    if (typeof value === "string") {
      if (!value.trim()) value = kind === "servicePorts" ? [] : {};
      else value = parseJSON(value);
    }
    value = copy(value);
    if (kind === "servicePorts" && !Array.isArray(value)) throw new Error("服务端口必须为数组");
    if (kind !== "servicePorts" && kind !== "ingressRules" &&
      (!value || typeof value !== "object" || Array.isArray(value))) {
      throw new Error("表单配置必须为对象");
    }
    if (!value || typeof value !== "object") throw new Error("表单配置必须为对象或数组");
    return value;
  }
  function resourceEnvelope(value) {
    return !!(value && typeof value === "object" &&
      (own(value, "apiVersion") || own(value, "kind") || own(value, "metadata")));
  }
  function specOf(value) {
    if (value && own(value, "spec")) return value.spec || {};
    return resourceEnvelope(value) ? {} : value || {};
  }
  function withSpec(value, spec) {
    var result = copy(value);
    if (value && (own(value, "spec") || resourceEnvelope(value))) result.spec = spec;
    else result = spec;
    return result;
  }
  function validKey(key) {
    if (!key || forbiddenKeys.has(key)) throw new Error("请输入有效且安全的键名");
    return key;
  }
  function integer(value, title, min, max, optional) {
    var text = String(value == null ? "" : value).trim();
    if (!text && optional) return undefined;
    if (!/^\d+$/.test(text)) throw new Error(title + "必须为整数");
    var number = Number(text);
    if (!Number.isSafeInteger(number) || number < min || number > max) throw new Error(title + "超出允许范围");
    return number;
  }
  function portValue(value, title, optional) {
    var text = String(value == null ? "" : value).trim();
    if (!text && optional) return undefined;
    if (/^\d+$/.test(text)) return integer(text, title, 1, 65535);
    if (/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(text) && text.length <= 15) return text;
    throw new Error(title + "必须为端口号或有效端口名");
  }
  function enumValue(value, allowed, title) {
    if (allowed.indexOf(value) < 0) throw new Error(title + "无效");
    return value;
  }

  function secretKeys(value) {
    var keys = Array.isArray(value) ? value : list(value && value.keys);
    if (value && !Array.isArray(value)) {
      keys = keys.concat(list(value.key_names), Object.keys(value.data || {}), Object.keys(value.stringData || {}));
    }
    return Array.from(new Set(keys.map(function (key) { return validKey(String(key)); })));
  }

  function normalizeCandidates(candidates) {
    var results = [];
    var seen = new Set();
    list(candidates).forEach(function (candidate) {
      var entry = typeof candidate === "string" ? { url: candidate } : candidate || {};
      var addresses = entry.url_candidates || entry.urls || (entry.url ? [entry.url] : []);
      if (!Array.isArray(addresses)) addresses = [addresses];
      if (!addresses.length) addresses = [""];
      addresses.forEach(function (address) {
        var url = String(typeof address === "string" ? address : address && (address.url || address.address) || "");
        var state = String(entry.state || (entry.available === false ? "unavailable" : "available"));
        var usable = entry.available !== false && ["available", "installed", "readable"].indexOf(state) >= 0;
        var safeURL = false;
        try { safeURL = /^https?:$/.test(new URL(url).protocol); } catch (_) { /* Unusable candidates remain visible. */ }
        var label = String(entry.label || [entry.type, entry.namespace, entry.service || entry.name].filter(Boolean).join(" / ") || url || state);
        var key = label + "\n" + url + "\n" + state;
        if (seen.has(key)) return;
        seen.add(key);
        results.push({ url: url, label: label, state: state, disabled: !usable || !safeURL });
      });
    });
    return results;
  }

  function ingressToRows(value) {
    if (Array.isArray(value)) return copy(value);
    var spec = specOf(value);
    var rows = [];
    list(spec.rules).forEach(function (rule, ruleIndex) {
      list(rule.http && rule.http.paths).forEach(function (path) {
        // Preserve resource backends and unknown rule/path fields even when the user edits another row.
        var service = path.backend && path.backend.service || {};
        var tls = list(spec.tls).find(function (entry) { return list(entry.hosts).indexOf(rule.host) >= 0; });
        rows.push({
          host: rule.host || "", path: path.path == null ? "/" : path.path, pathType: path.pathType || "Prefix",
          serviceName: service.name || "", servicePort: service.port && (service.port.number || service.port.name) || "",
          tlsSecretName: tls && tls.secretName || "", _ruleIndex: ruleIndex, _rule: copy(rule), _path: copy(path)
        });
      });
    });
    return rows;
  }

  function rowsToIngress(rows, value) {
    var spec = Array.isArray(value) ? {} : copy(specOf(value));
    var rules = [];
    var groups = new Map();
    var tlsBySecret = new Map();
    var editedHosts = new Set();
    rows.forEach(function (row) {
      var host = String(row.host || "").trim();
      var groupKey = host + "\n" + (row._ruleIndex == null ? "new" : row._ruleIndex);
      var rule = groups.get(groupKey);
      if (!rule) {
        rule = copy(row._rule || {});
        if (host) rule.host = host;
        else delete rule.host;
        rule.http = Object.assign({}, rule.http || {}, { paths: [] });
        rules.push(rule);
        groups.set(groupKey, rule);
      }
      var path = copy(row._path || {});
      path.path = String(row.path || "/");
      path.pathType = enumValue(row.pathType || "Prefix", ["Prefix", "Exact", "ImplementationSpecific"], "路径类型");
      var resourceBackend = path.backend && path.backend.resource && !row.serviceName;
      if (!resourceBackend) {
        var serviceName = String(row.serviceName || "").trim();
        if (!serviceName) throw new Error("请输入后端 Service 名称");
        var servicePort = portValue(row.servicePort, "Service 端口");
        path.backend = Object.assign({}, path.backend || {}, {
          service: { name: serviceName, port: typeof servicePort === "number" ? { number: servicePort } : { name: servicePort } }
        });
        delete path.backend.resource;
      }
      rule.http.paths.push(path);
      if (host) editedHosts.add(host);
      if (row.tlsSecretName) {
        if (!host) throw new Error("TLS 规则需要域名");
        var secret = String(row.tlsSecretName).trim();
        var hosts = tlsBySecret.get(secret) || new Set();
        hosts.add(host);
        tlsBySecret.set(secret, hosts);
      }
    });
    var originalHosts = new Set(ingressToRows(value).map(function (row) { return row.host; }).filter(Boolean));
    var tls = [];
    var emittedSecrets = new Set();
    list(spec.tls).forEach(function (entry) {
      var original = copy(entry);
      var secret = String(entry.secretName || "");
      if (tlsBySecret.has(secret)) {
        tls.push(Object.assign({}, original, { secretName: secret, hosts: Array.from(tlsBySecret.get(secret)) }));
        emittedSecrets.add(secret);
        return;
      }
      if (!list(entry.hosts).length) { tls.push(original); return; }
      original.hosts = entry.hosts.filter(function (host) { return !originalHosts.has(host) && !editedHosts.has(host); });
      if (original.hosts.length) tls.push(original);
    });
    tlsBySecret.forEach(function (hosts, secret) {
      if (emittedSecrets.has(secret)) return;
      var original = list(spec.tls).find(function (entry) { return entry.secretName === secret; });
      tls.push(Object.assign({}, copy(original || {}), { secretName: secret, hosts: Array.from(hosts) }));
    });
    // Rules without HTTP paths cannot be represented as editable rows and must survive.
    list(spec.rules).filter(function (rule) { return !list(rule.http && rule.http.paths).length; }).forEach(function (rule) { rules.push(copy(rule)); });
    spec.rules = rules;
    if (tls.length || own(spec, "tls")) spec.tls = tls;
    return Array.isArray(value) ? spec : withSpec(value, spec);
  }

  function optionsHTML(options, value) {
    return options.map(function (entry) {
      var item = typeof entry === "string" ? { value: entry, label: entry } : entry;
      return '<option value="' + esc(item.value) + '"' + (String(item.value) === String(value) ? " selected" : "") +
        (item.disabled ? " disabled" : "") + ">" + esc(item.label) + "</option>";
    }).join("");
  }

  function input(field, value, config) {
    var id = uid(config.id + "-" + field.key);
    var attributes = ' id="' + id + '" data-xsf-field="' + esc(field.key) + '" aria-label="' + esc(field.label) + '"';
    if (field.required) attributes += ' required aria-required="true"';
    if (field.readonly) attributes += " readonly";
    if (field.placeholder) attributes += ' placeholder="' + esc(field.placeholder) + '"';
    if (field.type === "boolean") return '<input class="x-sf-check" type="checkbox"' + attributes + (value ? " checked" : "") + ">";
    if (field.type === "select") return '<select class="x-input"' + attributes + ">" + optionsHTML(field.options, value) + "</select>";
    if (field.type === "textarea") return '<textarea class="x-input" rows="3"' + attributes + ">" + esc(value) + "</textarea>";
    return '<input class="x-input" type="' + esc(field.type || "text") + '"' + attributes +
      (field.type === "number" ? ' step="any"' : "") + (field.type === "password" ? ' autocomplete="new-password"' : "") +
      ' value="' + esc(value) + '">';
  }

  function fieldsHTML(fields, values, config) {
    return '<div class="x-sf-fields">' + fields.map(function (field) {
      return '<label class="x-sf-field' + (field.type === "boolean" ? " x-sf-field--check" : "") + '">' +
        "<span>" + esc(field.label) + "</span>" + input(field, values[field.key], config) + "</label>";
    }).join("") + "</div>";
  }

  function columns(kind, row, options) {
    if (kind === "servicePorts") return [
      { key: "name", label: "名称" }, { key: "protocol", label: "协议", type: "select", options: ["TCP", "UDP", "SCTP"] },
      { key: "port", label: "服务端口", type: "number" }, { key: "targetPort", label: "目标端口" }, { key: "nodePort", label: "节点端口", type: "number" }
    ];
    if (kind === "ingressRules") return [
      { key: "host", label: "域名" }, { key: "path", label: "路径" },
      { key: "pathType", label: "路径类型", type: "select", options: ["Prefix", "Exact", "ImplementationSpecific"] },
      { key: "serviceName", label: "后端 Service" }, { key: "servicePort", label: "Service 端口" }, { key: "tlsSecretName", label: "TLS Secret" }
    ];
    if (kind === "secret") return [
      { key: "key", label: "键", readonly: !!row.configured },
      { key: "value", label: "替换值", type: "password", placeholder: row.configured ? "已配置" : "" },
      { key: "replace", label: "替换", type: "boolean" }
    ];
    var fields = [{ key: "key", label: "键" }, { key: "value", label: "值", type: kind === "configMap" ? "textarea" : row.type === "boolean" ? "boolean" : row.type === "number" ? "number" : "text" }];
    if (kind === "keyValue" && options.typed !== false) fields.push({ key: "type", label: "类型", type: "select", options: [
      { value: "text", label: "文本" }, { value: "number", label: "数值" }, { value: "boolean", label: "布尔" }
    ] });
    return fields;
  }

  function rowsFor(config) {
    var value = config.value;
    if (config.kind === "servicePorts") return list(value).map(function (row) { return Object.assign({ protocol: "TCP", targetPort: row.port }, row); });
    if (config.kind === "ingressRules") return ingressToRows(value);
    if (config.kind === "secret") return secretKeys(value).map(function (key) { return { key: key, value: "", configured: true, replace: false }; });
    return Object.keys(value || {}).map(function (key) {
      var entry = value[key];
      if (entry != null && typeof entry === "object") throw new Error("键值表仅接受文本、数值和布尔值");
      var type = config.kind === "keyValue" && config.options.typed !== false && typeof entry === "number" ? "number" :
        config.kind === "keyValue" && config.options.typed !== false && typeof entry === "boolean" ? "boolean" : "text";
      return { key: key, value: entry == null ? "" : type === "text" ? String(entry) : entry, type: type };
    });
  }

  function rowHTML(config, row) {
    var fields = columns(config.kind, row, config.options);
    var original = copy(row);
    if (config.kind === "secret") { original.value = ""; original.replace = false; }
    return '<tr data-xsf-row data-xsf-original="' + esc(JSON.stringify(original)) + '">' + fields.map(function (field) {
      return '<td data-label="' + esc(field.label) + '">' + input(field, row[field.key], config) + "</td>";
    }).join("") + '<td class="x-sf-row-action"><button class="x-sf-icon" type="button" data-xsf-action="remove" aria-label="删除行" title="删除行"><span aria-hidden="true">&times;</span></button></td></tr>';
  }

  function tableHTML(config) {
    var rows = rowsFor(config);
    var fields = columns(config.kind, {}, config.options);
    return '<div class="x-sf-table-wrap"><table class="x-table x-sf-table"><thead><tr>' + fields.map(function (field) {
      return "<th>" + esc(field.label) + "</th>";
    }).join("") + '<th class="x-sf-row-action">操作</th></tr></thead><tbody data-xsf-rows>' +
      rows.map(function (row) { return rowHTML(config, row); }).join("") +
      '</tbody></table></div><div class="x-sf-actions"><button class="x-btn x-sf-add" type="button" data-xsf-action="add" title="新增行"><span aria-hidden="true">+</span> 新增行</button></div>';
  }

  function hpaHTML(config) {
    var spec = specOf(config.value);
    var target = spec.scaleTargetRef || {};
    var metric = list(spec.metrics).find(function (entry) { return entry.type === "Resource" && entry.resource && entry.resource.name === "cpu"; });
    var utilization = metric && metric.resource.target && metric.resource.target.averageUtilization;
    return fieldsHTML([
      { key: "minReplicas", label: "最小副本数", type: "number" }, { key: "maxReplicas", label: "最大副本数", type: "number" },
      { key: "targetKind", label: "目标类型", type: "select", options: Array.from(new Set(["Deployment", "StatefulSet", "ReplicaSet"].concat(target.kind || []))) },
      { key: "targetName", label: "目标名称" }, { key: "targetAPIVersion", label: "目标 API 版本" },
      { key: "utilization", label: "CPU 利用率 (%)", type: "number" }
    ], {
      minReplicas: spec.minReplicas == null ? "" : spec.minReplicas, maxReplicas: spec.maxReplicas == null ? 10 : spec.maxReplicas,
      targetKind: target.kind || "Deployment", targetName: target.name || "", targetAPIVersion: target.apiVersion || "apps/v1",
      utilization: utilization == null ? (own(spec, "metrics") ? "" : 70) : utilization
    }, config);
  }

  function cronJobHTML(config) {
    var spec = specOf(config.value);
    var pod = spec.jobTemplate && spec.jobTemplate.spec && spec.jobTemplate.spec.template && spec.jobTemplate.spec.template.spec || {};
    var container = list(pod.containers)[config.options.containerIndex || 0] || {};
    return fieldsHTML([
      { key: "schedule", label: "Cron 表达式" }, { key: "timeZone", label: "时区" },
      { key: "suspend", label: "暂停", type: "boolean" },
      { key: "concurrencyPolicy", label: "并发策略", type: "select", options: ["Allow", "Forbid", "Replace"] },
      { key: "containerName", label: "容器名称" }, { key: "image", label: "镜像" },
      { key: "command", label: "命令参数", type: "textarea" }
    ], {
      schedule: spec.schedule || "", timeZone: spec.timeZone || spec.timezone || "", suspend: !!spec.suspend,
      concurrencyPolicy: spec.concurrencyPolicy || "Allow", containerName: container.name || "main", image: container.image || "",
      command: list(container.command).join("\n")
    }, config);
  }

  function candidateFields(config) {
    var value = config.value || {};
    var candidates = normalizeCandidates(config.options.candidates || value.candidates || []);
    var names = { forbidden: "无读取权限", not_installed: "未安装", missing: "未发现", unavailable: "不可用", error: "发现失败", discovery_failed: "发现失败" };
    var selected = "";
    var choices = [{ value: "", label: "手动填写地址" }];
    candidates.forEach(function (entry, index) {
      if (!entry.disabled && entry.url === value.url && !selected) selected = String(index);
      choices.push({ value: String(index), label: entry.label + (entry.disabled ? " · " + (names[entry.state] || "不可用") : " · " + entry.url), disabled: entry.disabled });
    });
    return fieldsHTML([
      { key: "candidate", label: "集群内组件", type: "select", options: choices },
      { key: "url", label: "连接地址", type: "url" }
    ], { candidate: selected, url: value.url || "" }, config);
  }

  function objectFieldsHTML(config) {
    var fields = list(config.options.fields);
    if (!fields.length) throw new Error("请配置对象字段");
    var normalized = fields.map(function (field) {
      if (!field || !field.key) throw new Error("对象字段缺少键名");
      var options = list(field.options);
      if (field.type === "select" && !field.required &&
        !options.some(function (entry) { return String(typeof entry === "object" ? entry.value : entry) === ""; })) {
        options = [{ value: "", label: "请选择" }].concat(options);
      }
      return {
        key: validKey(String(field.key)), label: field.label || field.key, type: field.type || "text",
        options: options, placeholder: field.placeholder || "", readonly: !!field.readonly, required: !!field.required
      };
    });
    var values = {};
    fields.forEach(function (field) {
      if (!field || !field.key) return;
      var value = own(config.value, field.key) ? config.value[field.key] :
        own(field, "defaultValue") ? field.defaultValue : "";
      values[field.key] = field.type === "boolean" ? value === true || value === "true" : value;
    });
    return fieldsHTML(normalized, values, config);
  }

  function bodyHTML(config) {
    if (config.kind === "hpa") return hpaHTML(config);
    if (config.kind === "cronJob") return cronJobHTML(config);
    if (config.kind === "componentEndpoint") return candidateFields(config);
    if (config.kind === "objectFields") return objectFieldsHTML(config);
    return tableHTML(config);
  }

  function makeConfig(kind, value, options) {
    if (kinds.indexOf(kind) < 0) throw new Error("未知的结构化表单类型: " + kind);
    var config = { kind: kind, value: normalizeValue(kind, value), options: copy(options || {}) };
    if (kind === "secret") config.value = { keys: secretKeys(config.value) };
    config.id = uid(config.options.id);
    return config;
  }

  function render(kind, value, options) {
    var config = makeConfig(kind, value, options);
    return '<section class="x-sf x-sf--' + kind + '" id="' + config.id + '" data-xsf-kind="' + kind +
      '" data-xsf-name="' + esc(config.options.name || "") + '" data-xsf-state="' + esc(JSON.stringify(config)) + '">' +
      bodyHTML(config) + "</section>";
  }

  function resolve(scope, key) {
    if (!scope) throw new Error("缺少表单容器");
    var forms = [];
    if (scope.matches && scope.matches("[data-xsf-kind]")) forms.push(scope);
    if (scope.querySelectorAll) forms = forms.concat(Array.from(scope.querySelectorAll("[data-xsf-kind]")));
    if (key != null) forms = forms.filter(function (element) { return element.id === key || element.getAttribute("data-xsf-name") === key; });
    if (forms.length !== 1) throw new Error(forms.length ? "请指定表单 id 或 name" : "未找到结构化表单");
    return forms[0];
  }

  function state(element) {
    var stored = states.get(element);
    if (stored) return stored;
    var config = parseJSON(element.getAttribute("data-xsf-state") || "");
    if (kinds.indexOf(config.kind) < 0) throw new Error("表单类型无效");
    config.value = normalizeValue(config.kind, config.value);
    if (config.kind === "secret") config.value = { keys: secretKeys(config.value || {}) };
    stored = { config: config, bound: false };
    states.set(element, stored);
    element.removeAttribute("data-xsf-state");
    return stored;
  }

  function fieldsOf(element) {
    var values = {};
    Array.from(element.querySelectorAll("[data-xsf-field]")).forEach(function (field) {
      var key = validKey(field.getAttribute("data-xsf-field"));
      values[key] = field.type === "checkbox" ? !!field.checked : field.value;
    });
    return values;
  }

  function readRows(element, key) {
    element = resolve(element, key);
    var config = state(element).config;
    return Array.from(element.querySelectorAll("[data-xsf-row]")).map(function (row) {
      var original = parseJSON(row.getAttribute("data-xsf-original") || "{}");
      var values = fieldsOf(row);
      if (config.kind === "secret") return {
        key: original.configured ? original.key : String(values.key || "").trim(), value: String(values.value || ""),
        replace: !!values.replace, configured: !!original.configured, deleted: row.getAttribute("data-xsf-deleted") === "true"
      };
      var result = Object.assign({}, original, values);
      if (config.kind === "servicePorts") {
        result.name = String(result.name || "").trim();
        result.protocol = enumValue(result.protocol, ["TCP", "UDP", "SCTP"], "协议");
        result.port = integer(result.port, "服务端口", 1, 65535);
        result.targetPort = portValue(result.targetPort, "目标端口", true);
        result.nodePort = integer(result.nodePort, "节点端口", 1, 65535, true);
        if (!result.name) delete result.name;
        if (result.targetPort === undefined) delete result.targetPort;
        if (result.nodePort === undefined) delete result.nodePort;
      }
      return result;
    });
  }

  function readKeyValues(rows, config) {
    var result = {};
    rows.forEach(function (row) {
      var key = String(row.key || "").trim();
      if (!key && String(row.value || "") === "") return;
      validKey(key);
      if (own(result, key)) throw new Error("键名重复: " + key);
      var type = config.kind === "keyValue" && config.options.typed !== false ? row.type : "text";
      if (type === "number") {
        if (!String(row.value).trim() || !Number.isFinite(Number(row.value))) throw new Error("键 " + key + " 的数值无效");
        result[key] = Number(row.value);
      } else if (type === "boolean") {
        if (typeof row.value !== "boolean") throw new Error("键 " + key + " 的布尔值无效");
        result[key] = row.value;
      } else if (type === "text") result[key] = String(row.value);
      else throw new Error("值类型无效");
    });
    return result;
  }

  function readHPA(config, values) {
    var spec = copy(specOf(config.value));
    var min = integer(values.minReplicas, "最小副本数", 0, 2147483647, true);
    var max = integer(values.maxReplicas, "最大副本数", 1, 2147483647);
    if (min != null && min > max) throw new Error("最小副本数不能超过最大副本数");
    if (min != null) spec.minReplicas = min;
    else delete spec.minReplicas;
    spec.maxReplicas = max;
    if (!String(values.targetName).trim()) throw new Error("请输入目标名称");
    spec.scaleTargetRef = Object.assign({}, spec.scaleTargetRef || {}, {
      kind: enumValue(values.targetKind, ["Deployment", "StatefulSet", "ReplicaSet"].concat(spec.scaleTargetRef && spec.scaleTargetRef.kind || []), "目标类型"),
      name: String(values.targetName).trim(), apiVersion: String(values.targetAPIVersion).trim() || "apps/v1"
    });
    var utilization = integer(values.utilization, "CPU 利用率", 1, 2147483647, true);
    var metrics = list(spec.metrics);
    var cpu = metrics.find(function (metric) { return metric.type === "Resource" && metric.resource && metric.resource.name === "cpu"; });
    if (utilization != null) {
      var metric = copy(cpu || { type: "Resource", resource: { name: "cpu" } });
      metric.resource.target = Object.assign({}, metric.resource.target || {}, { type: "Utilization", averageUtilization: utilization });
      delete metric.resource.target.averageValue;
      delete metric.resource.target.value;
      spec.metrics = cpu ? metrics.map(function (item) { return item === cpu ? metric : item; }) : metrics.concat([metric]);
    } else if (cpu && cpu.resource.target && cpu.resource.target.type === "Utilization") {
      spec.metrics = metrics.filter(function (item) { return item !== cpu; });
    }
    return withSpec(config.value, spec);
  }

  function readCronJob(config, values) {
    var spec = copy(specOf(config.value));
    var schedule = String(values.schedule || "").trim();
    if (schedule.split(/\s+/).length !== 5 && !/^@(annually|yearly|monthly|weekly|daily|midnight|hourly)$/.test(schedule)) throw new Error("Cron 表达式格式无效");
    spec.schedule = schedule;
    spec.suspend = !!values.suspend;
    spec.concurrencyPolicy = enumValue(values.concurrencyPolicy, ["Allow", "Forbid", "Replace"], "并发策略");
    if (String(values.timeZone).trim()) spec.timeZone = String(values.timeZone).trim();
    else delete spec.timeZone;
    delete spec.timezone;
    spec.jobTemplate = spec.jobTemplate || {};
    spec.jobTemplate.spec = spec.jobTemplate.spec || {};
    spec.jobTemplate.spec.template = spec.jobTemplate.spec.template || {};
    var pod = spec.jobTemplate.spec.template.spec = spec.jobTemplate.spec.template.spec || {};
    if (!String(values.containerName).trim() || !String(values.image).trim()) throw new Error("请输入容器名称和镜像");
    pod.containers = list(pod.containers);
    var index = config.options.containerIndex || 0;
    if (index < 0 || index > pod.containers.length || !Number.isInteger(index)) throw new Error("容器索引无效");
    var container = Object.assign({}, pod.containers[index] || {}, { name: String(values.containerName).trim(), image: String(values.image).trim() });
    var command = String(values.command || "").replace(/\r\n/g, "\n");
    if (command !== "") container.command = list(container.command).join("\n") === command ? container.command : command.split("\n");
    else delete container.command;
    pod.containers[index] = container;
    if (!pod.restartPolicy) pod.restartPolicy = "OnFailure";
    return withSpec(config.value, spec);
  }

  function readObjectFields(config, values) {
    var result = copy(config.value);
    list(config.options.fields).forEach(function (field) {
      var key = validKey(String(field.key));
      var raw = values[key];
      if (field.type === "boolean") {
        result[key] = raw === true || raw === "true";
        return;
      }
      var text = String(raw == null ? "" : raw).trim();
      if (!text) {
        if (field.required) throw new Error("请填写：" + (field.label || key));
        delete result[key];
        return;
      }
      if (field.type === "number") {
        var number = Number(text);
        if (!Number.isFinite(number) ||
          field.min != null && number < Number(field.min) ||
          field.max != null && number > Number(field.max)) {
          throw new Error((field.label || key) + "数值无效");
        }
        result[key] = number;
      } else if (field.type === "select") {
        var option = list(field.options).map(function (entry) {
          return typeof entry === "object" ? entry : { value: entry };
        }).find(function (entry) { return String(entry.value) === text; });
        if (!option) throw new Error((field.label || key) + "选项无效");
        result[key] = option.value;
      } else result[key] = String(raw);
    });
    return result;
  }

  function read(scope, key) {
    var element = resolve(scope, key);
    var config = state(element).config;
    var values = fieldsOf(element);
    if (config.kind === "objectFields") return readObjectFields(config, values);
    if (config.kind === "hpa") return readHPA(config, values);
    if (config.kind === "cronJob") return readCronJob(config, values);
    if (config.kind === "componentEndpoint") {
      var result = copy(config.value);
      result.url = String(values.url || "").trim();
      if (result.url || config.options.required) {
        var url;
        try { url = new URL(result.url); } catch (_) { throw new Error("请输入有效的 HTTP(S) 地址"); }
        if (!/^https?:$/.test(url.protocol) || url.username || url.password) throw new Error("请输入不含凭据的 HTTP(S) 地址");
      }
      return result;
    }
    var rows = readRows(element);
    if (config.kind === "servicePorts") {
      var names = new Set();
      rows.forEach(function (row) {
        if (row.name && (!/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(row.name) || row.name.length > 15)) throw new Error("端口名称无效");
        if (rows.length > 1 && !row.name) throw new Error("多个端口需要分别填写名称");
        if (row.name && names.has(row.name)) throw new Error("端口名称重复");
        names.add(row.name);
      });
      return rows;
    }
    if (config.kind === "ingressRules") return rowsToIngress(rows, config.value);
    if (config.kind === "secret") {
      var patch = { set: {}, remove: [] };
      var keys = new Set();
      rows.forEach(function (row) {
        if (!row.key && !row.value && !row.replace) return;
        var keyName = validKey(row.key);
        if (keys.has(keyName)) throw new Error("键名重复: " + keyName);
        keys.add(keyName);
        if (row.deleted) { if (row.configured) patch.remove.push(keyName); }
        else if (row.replace || row.value !== "") patch.set[keyName] = row.value;
      });
      return patch;
    }
    return readKeyValues(rows, config);
  }

  function emit(element, name) {
    if (root.CustomEvent && element.dispatchEvent) element.dispatchEvent(new root.CustomEvent("xsforms:" + name, { bubbles: true, detail: { id: element.id } }));
  }

  function addRow(scope, value, key) {
    var element = resolve(scope, key);
    var config = state(element).config;
    var tbody = element.querySelector("[data-xsf-rows]");
    if (!tbody) throw new Error("当前控件不支持动态行");
    var defaults = config.kind === "servicePorts" ? { protocol: "TCP", port: "", targetPort: "", nodePort: "" } :
      config.kind === "ingressRules" ? { host: "", path: "/", pathType: "Prefix", serviceName: "", servicePort: "", tlsSecretName: "" } :
        { key: "", value: "", type: "text" };
    var row = Object.assign(defaults, copy(value || {}));
    if (config.kind === "secret") { row.value = ""; row.configured = false; row.replace = false; }
    tbody.insertAdjacentHTML("beforeend", rowHTML(config, row));
    var last = tbody.lastElementChild;
    var first = last && last.querySelector("[data-xsf-field]");
    if (first && first.focus) first.focus();
    emit(element, "change");
    return last;
  }

  function removeRow(element, row) {
    var config = state(element).config;
    if (!row || !element.contains(row)) throw new Error("配置行不属于此表单");
    var original = parseJSON(row.getAttribute("data-xsf-original") || "{}");
    if (config.kind === "secret" && original.configured) {
      var deleted = row.getAttribute("data-xsf-deleted") !== "true";
      row.setAttribute("data-xsf-deleted", String(deleted));
      var button = row.querySelector('[data-xsf-action="remove"]');
      button.setAttribute("aria-label", deleted ? "撤销删除" : "删除行");
      button.setAttribute("title", deleted ? "撤销删除" : "删除行");
      Array.from(row.querySelectorAll("[data-xsf-field]")).forEach(function (field) { field.disabled = deleted; });
    } else row.remove();
    emit(element, "change");
  }

  function changeType(element, field) {
    var row = field.closest("[data-xsf-row]");
    var valueInput = row.querySelector('[data-xsf-field="value"]');
    var value = valueInput.type === "checkbox" ? valueInput.checked : valueInput.value;
    var type = enumValue(field.value, ["text", "number", "boolean"], "值类型");
    valueInput.type = type === "boolean" ? "checkbox" : type;
    valueInput.className = type === "boolean" ? "x-sf-check" : "x-input";
    if (type === "boolean") valueInput.checked = value === true || value === "true";
    else valueInput.value = typeof value === "boolean" ? String(value) : value;
  }

  function controller(element) {
    return {
      element: element, id: element.id,
      read: function () { return read(element); }, readRows: function () { return readRows(element); },
      fill: function (value) { return fill(element, value); },
      addRow: function (value) { return addRow(element, value); },
      removeRow: function (row) { return removeRow(element, row); },
      updateCandidates: function (candidates) { return updateCandidates(element, candidates); },
      destroy: function () {
        var stored = state(element);
        if (stored.bound) ["click", "input", "change"].forEach(function (name) { element.removeEventListener(name, stored.handler); });
        stored.bound = false;
      }
    };
  }

  function bind(scope) {
    var elements = [];
    if (scope.matches && scope.matches("[data-xsf-kind]")) elements.push(scope);
    elements = elements.concat(Array.from(scope.querySelectorAll("[data-xsf-kind]")));
    return elements.map(function (element) {
      var stored = state(element);
      if (!stored.bound) {
        stored.handler = function (event) {
          var target = event.target;
          if (event.type === "click") {
            var action = target.closest("[data-xsf-action]");
            if (!action || !element.contains(action)) return;
            event.preventDefault();
            if (action.getAttribute("data-xsf-action") === "add") addRow(element);
            else removeRow(element, action.closest("[data-xsf-row]"));
            return;
          }
          if (!target.getAttribute) return;
          var field = target.getAttribute("data-xsf-field");
          var config = stored.config;
          if (field === "type" && event.type === "change") changeType(element, target);
          if (config.kind === "secret" && field === "value" && event.type === "input") {
            target.closest("[data-xsf-row]").querySelector('[data-xsf-field="replace"]').checked = true;
          }
          if (config.kind === "componentEndpoint") {
            if (field === "candidate" && event.type === "change" && target.value !== "") {
              var candidates = normalizeCandidates(config.options.candidates || config.value.candidates || []);
              var candidate = candidates[Number(target.value)];
              if (candidate && !candidate.disabled) element.querySelector('[data-xsf-field="url"]').value = candidate.url;
            } else if (field === "url" && event.type === "input") element.querySelector('[data-xsf-field="candidate"]').value = "";
          }
          emit(element, "change");
        };
        ["click", "input", "change"].forEach(function (name) { element.addEventListener(name, stored.handler); });
        stored.bound = true;
      }
      return controller(element);
    });
  }

  function fill(scope, value, key) {
    var element = resolve(scope, key);
    var stored = state(element);
    stored.config.value = normalizeValue(stored.config.kind, value);
    if (stored.config.kind === "secret") stored.config.value = { keys: secretKeys(stored.config.value) };
    element.innerHTML = bodyHTML(stored.config);
    return bind(element)[0];
  }

  function updateCandidates(scope, candidates, key) {
    var element = resolve(scope, key);
    var stored = state(element);
    if (stored.config.kind !== "componentEndpoint") throw new Error("当前控件不是组件地址选择器");
    var url = element.querySelector('[data-xsf-field="url"]').value;
    stored.config.options.candidates = copy(candidates);
    stored.config.value.url = url;
    element.innerHTML = bodyHTML(stored.config);
    return controller(element);
  }

  function mount(container, kind, value, options) {
    if (!container) throw new Error("缺少表单容器");
    container.innerHTML = render(kind, value, options);
    return bind(container)[0];
  }

  var api = {
    version: "1.1.0", render: render, mount: mount, bind: bind, read: read, readRows: readRows,
    fill: fill, addRow: addRow, updateCandidates: updateCandidates,
    escapeHTML: esc, parseJSON: parseJSON, clone: copy, uniqueId: uid,
    ingressToRows: ingressToRows, rowsToIngress: rowsToIngress, normalizeCandidates: normalizeCandidates, secretKeys: secretKeys
  };
  kinds.forEach(function (kind) { api[kind] = function (value, options) { return render(kind, value, options); }; });
  api.keyValues = api.keyValue;
  api.serviceSelector = api.selector;
  api.endpoint = api.componentEndpoint;
  api.fields = api.objectFields;
  root.XSStructuredForms = api;
})(typeof window !== "undefined" ? window : globalThis);
