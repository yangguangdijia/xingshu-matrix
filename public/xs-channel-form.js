(function (root) {
  "use strict";
  /*
   * 通知渠道新建 / 编辑：基于 XSFormKit（类型卡片 → 表单 → 发送测试 → 保存）。
   * 修复：旧版依赖不存在的全局 dlg() 导致退化为无样式原生对话框；钉钉/飞书缺少「加签密钥」；
   * 没有级别 / 环境过滤；只能保存后才能测试；测试失败提示文案错误（"测试消息已发送失败"）。
   * 通知中心：渠道说明、通用 Webhook 自定义请求头、邮件可复用平台系统邮件、按事件类型选择消息模板、
   * 新建时可一并生成「全部告警」订阅；内网收件桩（白名单内）允许 http 地址，最终由服务端按通知安全策略校验。
   */
  var TYPES = [
    { v: "feishu", t: "飞书", d: "群机器人 Webhook，可选签名校验", icon: "/assets/channels/feishu.svg" },
    { v: "dingtalk", t: "钉钉", d: "群机器人 Webhook，推荐加签", icon: "/assets/channels/dingtalk.svg" },
    { v: "wecom", t: "企业微信", d: "群机器人 Webhook", icon: "/assets/channels/wecom.png" },
    { v: "email", t: "邮件", d: "平台系统邮件或自定义 SMTP" },
    { v: "slack", t: "Slack", d: "Incoming Webhook" },
    { v: "webhook", t: "通用 Webhook", d: "POST JSON，自定义请求头，可选 HMAC 签名" }
  ];
  var NAMES = {};
  TYPES.forEach(function (x) { NAMES[x.v] = x.t; });
  var SEVERITIES = [{ v: "critical", t: "严重" }, { v: "warning", t: "警告" }, { v: "info", t: "提示" }];
  var robot = function (v) { return ["feishu", "dingtalk", "wecom", "slack", "webhook"].indexOf(v.type) >= 0; };
  var signed = function (v) { return ["feishu", "dingtalk", "webhook"].indexOf(v.type) >= 0; };
  var mail = function (v) { return v.type === "email"; };
  var smtp = function (v) { return v.type === "email" && !v.use_system; };
  var hook = function (v) { return v.type === "webhook"; };
  var KINDS = [{ v: "alert", t: "告警" }, { v: "incident", t: "故障" }, { v: "pipeline", t: "流水线" }, { v: "approval", t: "审批待办" }, { v: "test", t: "测试消息" }];
  var tplCache = null;
  function loadTemplates() {
    if (tplCache) return tplCache;
    tplCache = root.XS && root.XS.api ? root.XS.api("/observability/notify/templates").then(function (r) { return (r && r.items) || []; }).catch(function () { tplCache = null; return []; }) : Promise.resolve([]);
    return tplCache;
  }
  function tplOptions(kind) {
    return function () {
      return loadTemplates().then(function (list) {
        return list.filter(function (t) { return t.event_kind === kind; }).map(function (t) { return { v: String(t.id), t: t.name + (t.channel_type ? "（" + (NAMES[t.channel_type] || t.channel_type) + "）" : "") }; });
      });
    };
  }
  function localHost(u) {
    try { var h = new URL(u).hostname; return h === "localhost" || /^\d+\.\d+\.\d+\.\d+$/.test(h) || h.indexOf(":") >= 0; } catch (e) { return false; }
  }

  function payload(v, existing) {
    var cfg = {};
    if (v.type === "email") {
      cfg = { use_system: !!v.use_system, to: v.to || [] };
      if (!v.use_system) {
        cfg.host = v.host; cfg.port = Number(v.port) || 25; cfg.username = v.username || ""; cfg.from = v.from;
        if (v.password) cfg.password = v.password;
      }
    } else {
      if (v.webhook) cfg.webhook = v.webhook;
      if (v.secret) cfg.secret = v.secret;
      if (v.type === "webhook") {
        cfg.headers = {};
        (v.headers || []).forEach(function (r) { if (r && r.k) cfg.headers[r.k] = r.v || ""; });
      }
    }
    var templates = {};
    KINDS.forEach(function (k) { var id = Number(v["tpl_" + k.v] || 0); if (id > 0) templates[k.v] = id; });
    var out = { name: v.name, type: v.type, description: v.description || "", config: cfg, enabled: v.enabled !== false, severities: v.severities || [], envs: v.envs || [], templates: templates };
    if (!existing) out.subscribe_alerts = v.subscribe_alerts !== false;
    return out;
  }

  function check(v, existing) {
    if (robot(v)) {
      if (!v.webhook && !existing) return "请填写 Webhook 地址";
      if (v.webhook && !/^https?:\/\//i.test(v.webhook)) return "Webhook 必须以 https:// 开头（内网收件桩可用 http://）";
      var real = v.webhook && /^https:\/\//i.test(v.webhook) && !localHost(v.webhook);
      if (real && v.type === "dingtalk" && !/oapi\.dingtalk\.com\/robot\/send\?access_token=/.test(v.webhook)) return "钉钉 Webhook 形如 https://oapi.dingtalk.com/robot/send?access_token=…";
      if (real && v.type === "feishu" && !/open\.(feishu\.cn|larksuite\.com)\/open-apis\/bot\/v2\/hook\//.test(v.webhook)) return "飞书 Webhook 形如 https://open.feishu.cn/open-apis/bot/v2/hook/…";
      if (real && v.type === "wecom" && !/qyapi\.weixin\.qq\.com\/cgi-bin\/webhook\/send\?key=/.test(v.webhook)) return "企业微信 Webhook 形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…";
      if (hook(v)) {
        var bad = (v.headers || []).filter(function (r) { return r.k && !/^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$/.test(r.k); });
        if (bad.length) return "请求头名称不合法：" + bad.map(function (r) { return r.k; }).join(", ");
        var reserved = (v.headers || []).filter(function (r) { return /^(host|content-length|content-type|transfer-encoding|connection|x-matrix-signature(-v2)?|x-matrix-timestamp)$/i.test(r.k || ""); });
        if (reserved.length) return "不能自定义保留请求头：" + reserved.map(function (r) { return r.k; }).join(", ");
      }
    }
    if (mail(v)) {
      if (!(v.to || []).length) return "请填写收件人";
      var addrs = (v.to || []).slice();
      if (smtp(v)) {
        var port = Number(v.port);
        if (!v.host) return "请填写 SMTP 服务器";
        if (/[\s/:]/.test(v.host)) return "SMTP 服务器只填主机名或 IP，不含协议和端口";
        if (!Number.isInteger(port) || port < 1 || port > 65535) return "SMTP 端口无效";
        addrs.push(v.from);
      }
      var badMail = addrs.filter(function (a) { return !/^[^\s@<>,;]+@[^\s@<>,;]+$/.test(a || ""); });
      if (badMail.length) return "邮箱地址无效：" + badMail.join(", ");
    }
    return "";
  }

  function open(existing, done) {
    if (!root.XSFormKit) { if (root.XS && root.XS.toast) root.XS.toast("表单组件未加载，请刷新页面"); return null; }
    existing = existing || null;
    var c = existing && existing.config || {};
    var tm = existing && existing.templates || {};
    var values = existing ? {
      __existing: true, type: existing.type, name: existing.name, description: existing.description || "", enabled: !!existing.enabled,
      severities: existing.severities || [], envs: existing.envs || [],
      webhook: "", secret: "", host: c.host || "", port: c.port || 465, username: c.username || "", password: "", from: c.from || "", to: c.to || [],
      use_system: !!c.use_system,
      headers: Object.keys(c.headers || {}).map(function (k) { return { k: k, v: c.headers[k] }; })
    } : { type: "feishu", enabled: true, port: 465, severities: [], envs: [], use_system: true, headers: [], subscribe_alerts: true };
    KINDS.forEach(function (k) { values["tpl_" + k.v] = tm[k.v] ? String(tm[k.v]) : ""; });
    var envOptions = [];
    return root.XSFormKit.open({
      title: existing ? "编辑通知渠道" : "新建通知渠道",
      subtitle: existing ? NAMES[existing.type] + " · 已保存的 Webhook、密钥与请求头只显示掩码，保持不变即沿用" : "告警、故障、流水线结果与审批待办按订阅规则推送到这里",
      scene: "observability.channel." + (existing ? "update" : "create"),
      aiExample: "建一个飞书渠道叫 prod-oncall，只接收生产环境的严重告警",
      width: "md",
      okText: existing ? "保存" : "创建",
      values: values,
      sections: [
        { title: "渠道类型", fields: [
          existing ? { k: "type", label: "类型", type: "select", required: true, options: TYPES.filter(function (x) { return x.v === existing.type; }) }
            : { k: "type", label: "", type: "radio", options: TYPES, required: true }
        ] },
        { title: "连接信息", fields: [
          { k: "name", label: "渠道名称", required: true, placeholder: "prod-oncall", help: "在订阅规则里按名称选择", check: function (s) { return s.length > 64 ? "不超过 64 个字符" : ""; } },
          { k: "enabled", label: "状态", type: "switch", text: "启用" },
          { k: "description", label: "说明", full: true, placeholder: "例如：生产值班群，夜间只收严重告警", check: function (s) { return s.length > 256 ? "不超过 256 个字符" : ""; } },
          { k: "webhook", label: "Webhook 地址", full: true, mono: true, show: robot, required: !existing, secret: true,
            placeholder: existing ? "留空保持原地址" : "https://…", help: "地址里包含 token，保存后加密存储且不再明文显示；内网地址需管理员在「通知安全」中加入白名单" },
          { k: "secret", label: "签名密钥（可选）", type: "password", full: true, show: signed,
            help: "钉钉「加签」/ 飞书「签名校验」的密钥；通用 Webhook 用它计算 X-Matrix-Signature 与带时间戳的 X-Matrix-Signature-V2" },
          { k: "headers", label: "自定义请求头", type: "kv", show: hook, noAI: true, keyPh: "Authorization", valPh: "Bearer …", addText: "添加请求头",
            help: "请求头的值按密钥加密保存，回显为掩码，保持掩码即沿用原值" },
          { k: "use_system", label: "发信方式", type: "switch", show: mail, text: "使用平台系统邮件（在「平台设置 → 系统邮件」中配置）" },
          { k: "host", label: "SMTP 服务器", show: smtp, required: true, placeholder: "smtp.exmail.qq.com", mono: true },
          { k: "port", label: "端口", type: "number", show: smtp, required: true, min: 1, max: 65535, help: "465 为 SSL；25/587 自动 STARTTLS" },
          { k: "username", label: "SMTP 账号", show: smtp, placeholder: "alert@example.com" },
          { k: "password", label: "SMTP 密码 / 授权码", type: "password", show: smtp },
          { k: "from", label: "发件人", show: smtp, required: true, placeholder: "alert@example.com" },
          { k: "to", label: "收件人", type: "tags", show: mail, required: true, full: true, placeholder: "ops@example.com, sre@example.com", help: "多个地址用逗号分隔" }
        ] },
        { title: "告警接收范围", desc: "渠道级兜底过滤，只作用于告警；不选表示全部接收。故障、流水线、审批由订阅规则决定。", fields: [
          { k: "severities", label: "告警级别", type: "checks", options: SEVERITIES },
          { k: "envs", label: "环境", type: "checks", options: envOptions, load: loadEnvs }
        ].concat(existing ? [] : [{ k: "subscribe_alerts", label: "订阅", type: "switch", text: "同时创建订阅：按上面的范围接收全部告警" }]) },
        { title: "消息模板", desc: "不选择时使用系统默认模板。模板在「通知渠道 → 消息模板」中维护。", fields: KINDS.map(function (k) {
          return { k: "tpl_" + k.v, label: k.t, type: "select", empty: "默认模板", options: [], load: tplOptions(k.v) };
        }) }
      ],
      toObject: function (v) {
        var p = payload(v, existing);
        return { apiVersion: "notification.xingshu.io/v1", kind: "Channel", metadata: { name: p.name, annotations: { description: p.description } }, spec: { type: p.type, enabled: p.enabled, config: p.config, filter: { severities: p.severities, envs: p.envs }, templates: p.templates } };
      },
      fromObject: function (o, v) {
        var spec = o.spec || {}, cfg = spec.config || {}, f = spec.filter || {}, meta = o.metadata || {}, tpl = spec.templates || {};
        if (existing && spec.type && spec.type !== existing.type) throw new Error("编辑时不能修改渠道类型");
        var out = {
          name: meta.name || "", description: meta.annotations && meta.annotations.description || "", type: spec.type || v.type, enabled: spec.enabled !== false,
          webhook: cfg.webhook || "", secret: cfg.secret || "", host: cfg.host || "", port: cfg.port || v.port, username: cfg.username || "",
          password: cfg.password || "", from: cfg.from || "", to: cfg.to || [], severities: f.severities || [], envs: f.envs || [],
          use_system: !!cfg.use_system, headers: Object.keys(cfg.headers || {}).map(function (k) { return { k: k, v: cfg.headers[k] }; }),
          subscribe_alerts: v.subscribe_alerts
        };
        KINDS.forEach(function (k) { out["tpl_" + k.v] = tpl[k.v] ? String(tpl[k.v]) : ""; });
        return out;
      },
      validate: function (v) { return check(v, existing); },
      actions: [{ label: "发送测试", run: function (v, _o, ctx) {
        var body = payload(v, existing);
        if (existing) body.id = existing.id;
        ctx.status("正在发送测试消息…", true);
        return root.XS.api("/observability/channels/test", { method: "POST", body: body }).then(function (r) {
          if (r && r.ok === false) ctx.status("测试失败：" + (r.error || "对方未接受消息"));
          else ctx.status("测试消息已送达，请到" + NAMES[v.type] + "确认。", true);
        }).catch(function (err) { ctx.status("测试失败：" + (err && err.message || "请求失败")); });
      } }],
      submit: function (v, _o, ctx) {
        var body = payload(v, existing);
        return root.XS.api("/observability/channels" + (existing ? "/" + encodeURIComponent(existing.id) : ""), { method: existing ? "PUT" : "POST", body: body }).then(function (result) {
          ctx.close();
          tplCache = null;
          if (root.XS && root.XS.toast) root.XS.toast(existing ? "渠道已更新" : "渠道已创建");
          if (typeof done === "function") done(result);
        });
      }
    });
  }

  // 环境来自已接入集群的环境标签（与 RBAC 环境范围一致），再补上常用的 dev/test/staging/prod。
  function loadEnvs() {
    var base = ["dev", "test", "staging", "prod"];
    var done = function (names) {
      var seen = {}, out = [];
      names.concat(base).forEach(function (n) { if (n && !seen[n]) { seen[n] = 1; out.push({ v: n, t: n }); } });
      return out;
    };
    if (!root.XS || !root.XS.api) return Promise.resolve(done([]));
    return root.XS.api("/clusters").then(function (r) {
      var list = Array.isArray(r) ? r : r && r.items || [];
      return done(list.map(function (c) { return c.env; }));
    }).catch(function () { return done([]); });
  }

  root.XSChannelForm = { open: open, payload: payload, check: check, types: TYPES, names: NAMES, resetTemplates: function () { tplCache = null; } };
})(window);
