(function (root) {
  "use strict";
  /*
   * 通知渠道新建 / 编辑：基于 XSFormKit（类型卡片 → 表单 → 发送测试 → 保存）。
   * 修复：旧版依赖不存在的全局 dlg() 导致退化为无样式原生对话框；钉钉/飞书缺少「加签密钥」；
   * 没有级别 / 环境过滤；只能保存后才能测试；测试失败提示文案错误（"测试消息已发送失败"）。
   */
  var TYPES = [
    { v: "feishu", t: "飞书", d: "群机器人 Webhook，可选签名校验", icon: "/assets/channels/feishu.svg" },
    { v: "dingtalk", t: "钉钉", d: "群机器人 Webhook，推荐加签", icon: "/assets/channels/dingtalk.svg" },
    { v: "wecom", t: "企业微信", d: "群机器人 Webhook", icon: "/assets/channels/wecom.png" },
    { v: "email", t: "邮件", d: "SMTP 发信，支持 SSL / STARTTLS" },
    { v: "slack", t: "Slack", d: "Incoming Webhook" },
    { v: "webhook", t: "通用 Webhook", d: "POST JSON 到任意系统，可选 HMAC 签名" }
  ];
  var NAMES = {};
  TYPES.forEach(function (x) { NAMES[x.v] = x.t; });
  var SEVERITIES = [{ v: "critical", t: "严重" }, { v: "warning", t: "警告" }, { v: "info", t: "提示" }];
  var robot = function (v) { return ["feishu", "dingtalk", "wecom", "slack", "webhook"].indexOf(v.type) >= 0; };
  var signed = function (v) { return ["feishu", "dingtalk", "webhook"].indexOf(v.type) >= 0; };
  var mail = function (v) { return v.type === "email"; };

  function payload(v, existing) {
    var cfg = {};
    if (v.type === "email") {
      cfg = { host: v.host, port: Number(v.port) || 25, username: v.username || "", from: v.from, to: v.to || [] };
      if (v.password) cfg.password = v.password;
    } else {
      if (v.webhook) cfg.webhook = v.webhook;
      if (v.secret) cfg.secret = v.secret;
    }
    return { name: v.name, type: v.type, config: cfg, enabled: v.enabled !== false, severities: v.severities || [], envs: v.envs || [] };
  }

  function check(v, existing) {
    if (robot(v)) {
      if (!v.webhook && !existing) return "请填写 Webhook 地址";
      if (v.webhook && !/^https:\/\//i.test(v.webhook) && !(v.type === "webhook" && /^http:\/\//i.test(v.webhook))) return v.type === "webhook" ? "Webhook 必须以 http:// 或 https:// 开头" : "Webhook 必须是 https 地址";
      if (v.type === "dingtalk" && v.webhook && !/oapi\.dingtalk\.com\/robot\/send\?access_token=/.test(v.webhook)) return "钉钉 Webhook 形如 https://oapi.dingtalk.com/robot/send?access_token=…";
      if (v.type === "feishu" && v.webhook && !/open\.(feishu\.cn|larksuite\.com)\/open-apis\/bot\/v2\/hook\//.test(v.webhook)) return "飞书 Webhook 形如 https://open.feishu.cn/open-apis/bot/v2/hook/…";
      if (v.type === "wecom" && v.webhook && !/qyapi\.weixin\.qq\.com\/cgi-bin\/webhook\/send\?key=/.test(v.webhook)) return "企业微信 Webhook 形如 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…";
    }
    if (mail(v)) {
      var port = Number(v.port);
      if (!Number.isInteger(port) || port < 1 || port > 65535) return "SMTP 端口无效";
      var bad = [v.from].concat(v.to || []).filter(function (a) { return !/^[^\s@]+@[^\s@]+$/.test(a || ""); });
      if (bad.length) return "邮箱地址无效：" + bad.join(", ");
    }
    return "";
  }

  function open(existing, done) {
    if (!root.XSFormKit) { if (root.XS && root.XS.toast) root.XS.toast("表单组件未加载，请刷新页面"); return null; }
    existing = existing || null;
    var c = existing && existing.config || {};
    var values = existing ? {
      __existing: true, type: existing.type, name: existing.name, enabled: !!existing.enabled,
      severities: existing.severities || [], envs: existing.envs || [],
      webhook: "", secret: "", host: c.host || "", port: c.port || 465, username: c.username || "", password: "", from: c.from || "", to: c.to || []
    } : { type: "feishu", enabled: true, port: 465, severities: [], envs: [] };
    var envOptions = [];
    return root.XSFormKit.open({
      title: existing ? "编辑通知渠道" : "新建通知渠道",
      subtitle: existing ? NAMES[existing.type] + " · 已保存的 Webhook 与密钥不会回显，留空即沿用" : "告警、审批、流水线结果会按级别和环境推送到这里",
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
          { k: "name", label: "渠道名称", required: true, placeholder: "prod-oncall", help: "在告警规则、流水线通知里按名称选择", check: function (s) { return s.length > 64 ? "不超过 64 个字符" : ""; } },
          { k: "enabled", label: "状态", type: "switch", text: "启用" },
          { k: "webhook", label: "Webhook 地址", full: true, mono: true, show: robot, required: !existing, secret: true,
            placeholder: existing ? "留空保持原地址" : "https://…", help: "地址里包含 token，保存后加密存储且不再明文显示" },
          { k: "secret", label: "签名密钥（可选）", type: "password", full: true, show: signed,
            help: "钉钉「加签」/ 飞书「签名校验」的密钥；通用 Webhook 用它计算 X-Matrix-Signature: sha256=HMAC(body)" },
          { k: "host", label: "SMTP 服务器", show: mail, required: true, placeholder: "smtp.exmail.qq.com", mono: true },
          { k: "port", label: "端口", type: "number", show: mail, required: true, min: 1, max: 65535, help: "465 为 SSL；25/587 自动 STARTTLS" },
          { k: "username", label: "SMTP 账号", show: mail, placeholder: "alert@example.com" },
          { k: "password", label: "SMTP 密码 / 授权码", type: "password", show: mail },
          { k: "from", label: "发件人", show: mail, required: true, placeholder: "alert@example.com" },
          { k: "to", label: "收件人", type: "tags", show: mail, required: true, full: true, placeholder: "ops@example.com, sre@example.com", help: "多个地址用逗号分隔" }
        ] },
        { title: "接收范围", desc: "不选表示全部接收。", fields: [
          { k: "severities", label: "告警级别", type: "checks", options: SEVERITIES },
          { k: "envs", label: "环境", type: "checks", options: envOptions, load: loadEnvs }
        ] }
      ],
      toObject: function (v) {
        var p = payload(v, existing);
        return { apiVersion: "notification.xingshu.io/v1", kind: "Channel", metadata: { name: p.name }, spec: { type: p.type, enabled: p.enabled, config: p.config, filter: { severities: p.severities, envs: p.envs } } };
      },
      fromObject: function (o, v) {
        var spec = o.spec || {}, cfg = spec.config || {}, f = spec.filter || {};
        if (existing && spec.type && spec.type !== existing.type) throw new Error("编辑时不能修改渠道类型");
        return {
          name: o.metadata && o.metadata.name || "", type: spec.type || v.type, enabled: spec.enabled !== false,
          webhook: cfg.webhook || "", secret: cfg.secret || "", host: cfg.host || "", port: cfg.port || v.port, username: cfg.username || "",
          password: cfg.password || "", from: cfg.from || "", to: cfg.to || [], severities: f.severities || [], envs: f.envs || []
        };
      },
      validate: function (v) { return check(v, existing); },
      actions: [{ label: "发送测试", run: function (v, _o, ctx) {
        var body = payload(v, existing);
        if (existing) body.id = existing.id;
        ctx.status("正在发送测试消息…", true);
        return root.XS.api("/observability/channels/test", { method: "POST", body: body }).then(function (r) {
          if (r && r.ok === false) ctx.status("测试失败：" + (r.error || "对方未接受消息"));
          else ctx.status("测试消息已送达，请到" + NAMES[v.type] + "确认。", true);
        });
      } }],
      submit: function (v, _o, ctx) {
        var body = payload(v, existing);
        return root.XS.api("/observability/channels" + (existing ? "/" + encodeURIComponent(existing.id) : ""), { method: existing ? "PUT" : "POST", body: body }).then(function (result) {
          ctx.close();
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

  root.XSChannelForm = { open: open, payload: payload, check: check };
})(window);
