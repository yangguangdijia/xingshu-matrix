(function () {
  "use strict";
  var E = function (value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  };
  var states = { awaiting_approval: "等待审批", queued: "排队中", running: "安装中", cancelling: "正在取消", succeeded: "已完成", failed: "失败", cancelled: "已取消" };
  function request(path, method, body, timeout) {
    return window.XS.api(path, { method: method || "GET", body: body, timeoutMs: timeout || 30000 });
  }
  function message(error) { return error && error.message || "请求失败"; }
  function modal(title, body, footer) { dlg(E(title), body, footer || '<button class="x-btn" data-close type="button">关闭</button>'); }
  function field(name, label, placeholder, value, optional) {
    return '<label><span>' + label + '</span><input class="x-input" name="' + name + '" placeholder="' + E(placeholder || "") + '" value="' + E(value || "") + '"' + (optional ? "" : " required") + ' autocomplete="off"></label>';
  }
  function hostRow() {
    return '<tr data-host><td><input class="x-input" data-host-key="ip" aria-label="目标主机 IP" placeholder="IP" required></td>' +
      '<td><input class="x-input" data-host-key="username" aria-label="SSH 用户名" value="root" required></td>' +
      '<td><input class="x-input" data-host-key="port" aria-label="SSH 端口" type="number" min="1" max="65535" value="22" required style="width:80px"></td>' +
      '<td><input class="x-input" data-host-key="password" aria-label="SSH 密码" type="password" autocomplete="new-password" required></td>' +
      '<td><button class="x-btn" data-remove-host type="button" aria-label="移除主机" title="移除主机">×</button></td></tr>';
  }
  function readRequest(form) {
    var value = function (name) { return form.elements[name].value.trim(); };
    return {
      name: value("name"), env: value("env"), version: value("version"), mode: value("mode"),
      endpoint: value("endpoint"), cni: value("cni"),
      components: {
        traefik: form.elements.traefik.checked, servicelb: form.elements.servicelb.checked,
        local_storage: form.elements.local_storage.checked, metrics_server: form.elements.metrics_server.checked
      },
      hosts: Array.from(form.querySelectorAll("[data-host]"), function (row) {
        var h = {};
        row.querySelectorAll("[data-host-key]").forEach(function (input) { h[input.dataset.hostKey] = input.dataset.hostKey === "port" ? Number(input.value) : input.value; });
        return h;
      })
    };
  }
  function open() {
    if (!window.XS || window.XS.mode !== "live") return toast("请先连接平台后端");
    modal("部署 Kubernetes 集群", '<form id="provisionForm" class="x-form" novalidate>' +
      '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px">' +
      field("name", "集群名称", "小写字母、数字、连字符") + field("env", "环境", "与平台环境策略名称一致") +
      field("version", "K3s 固定版本", "v1.x.y+k3sN") +
      '<label><span>控制平面</span><select class="x-select" name="mode"><option value="auto">自动：1–2 台单控制平面，3 台起 HA</option><option value="single">单控制平面</option><option value="ha">三控制平面 HA</option></select></label>' +
      field("endpoint", "API 地址 / VIP（HA 必填）", "IP 或域名，不含协议和端口", "", true) +
      '<label><span>网络插件</span><select class="x-select" name="cni"><option value="vxlan">Flannel VXLAN</option><option value="wireguard-native">Flannel WireGuard</option></select></label></div>' +
      '<fieldset style="border:0;padding:0;margin:4px 0"><legend>可选组件</legend><div style="display:flex;gap:16px;flex-wrap:wrap">' +
      [["traefik", "Traefik Ingress"], ["servicelb", "ServiceLB"], ["local_storage", "本地存储"], ["metrics_server", "Metrics Server"]].map(function (c) {
        return '<label style="display:flex;align-items:center;gap:6px"><input type="checkbox" name="' + c[0] + '" checked>' + c[1] + '</label>';
      }).join("") + '</div></fieldset><div style="overflow:auto"><table class="x-table"><thead><tr><th>IP</th><th>SSH 用户</th><th>端口</th><th>密码</th><th></th></tr></thead><tbody id="provisionHosts">' + hostRow() +
      '</tbody></table></div><button class="x-btn" type="button" data-add-host>+ 添加主机</button>' +
      '<p class="x-frac">安装方式：K3s + containerd。按表格顺序分配控制平面；单机和三控制平面节点均允许运行业务。已有 Kubernetes 请用 kubeconfig 导入。需要全新 Linux/systemd 主机及 root 或免密 sudo。</p>' +
      '<p class="x-frac">HA 需提前配置负载均衡/VIP 的 6443 端口；取消不会卸载已安装的节点。</p>' +
      '<div data-plan></div><p class="x-aerr" data-error role="alert" hidden></p></form>',
      '<button class="x-btn" data-close type="button">取消</button><button class="x-btn" type="button" id="provisionPreflight">连接预检</button><button class="x-btn x-btn--pri" type="submit" form="provisionForm" id="provisionSubmit" disabled>确认安装</button>');
    var form = document.getElementById("provisionForm"), hosts = document.getElementById("provisionHosts");
    var check = document.getElementById("provisionPreflight"), submit = document.getElementById("provisionSubmit"), plan, serial = 0;
    var error = function (text) { var box = form.querySelector("[data-error]"); box.textContent = text; box.hidden = false; };
    function invalidate() {
      serial++; plan = null; submit.disabled = true; form.querySelector("[data-plan]").innerHTML = "";
    }
    form.addEventListener("input", function (e) { if (!e.target.matches("[data-confirm-plan]")) invalidate(); });
    form.addEventListener("change", function (e) { if (!e.target.matches("[data-confirm-plan]")) invalidate(); });
    form.querySelector("[data-add-host]").onclick = function () {
      if (hosts.children.length >= 30) return error("最多添加 30 台主机");
      hosts.insertAdjacentHTML("beforeend", hostRow()); invalidate();
    };
    hosts.onclick = function (e) {
      var b = e.target.closest("[data-remove-host]");
      if (b && hosts.children.length > 1) { b.closest("tr").remove(); invalidate(); }
    };
    check.onclick = async function () {
      if (!form.reportValidity()) return;
      var version = ++serial;
      check.disabled = true; submit.disabled = true; plan = null;
      form.querySelector("[data-plan]").innerHTML = '<p class="x-frac">正在检查 SSH、系统和资源…</p>';
      form.querySelector("[data-error]").hidden = true;
      try {
        var result = await request("/provision/preflight", "POST", readRequest(form), 190000);
        if (!form.isConnected || version !== serial) return;
        plan = result;
        var mirror = result.registry_mirror || { provider: "dockerhub", endpoint: "" };
        var mirrorName = { dockerhub: "Docker Hub 直连", aliyun: "阿里云加速", daocloud: "DaoCloud", custom: "自定义镜像源" }[mirror.provider] || mirror.provider;
        form.querySelector("[data-plan]").innerHTML = '<h3>部署规划   ' + result.masters + ' 控制平面 / ' + result.workers + ' 工作节点   ' + (result.ha ? "HA" : "单控制平面") + '</h3>' +
          '<p class="x-mono">API：' + E(result.endpoint) + '</p><p class="x-frac">Docker Hub 镜像源：' + E(mirrorName) + (mirror.endpoint ? '   <span class="x-mono">' + E(mirror.endpoint) + "</span>" : "") + '</p><div style="overflow:auto"><table class="x-table"><thead><tr><th>主机</th><th>角色</th><th>SSH 主机指纹</th></tr></thead><tbody>' +
          result.hosts.map(function (h) { return '<tr><td>' + E(h.ip) + '</td><td>' + E(h.role) + '</td><td class="x-mono">' + E(h.fingerprint) + '</td></tr>'; }).join("") +
          '</tbody></table></div><ul class="x-frac">' + result.warnings.map(function (w) { return '<li>' + E(w) + '</li>'; }).join("") +
          '</ul><label style="display:flex;align-items:center;gap:8px"><input type="checkbox" data-confirm-plan>已核对主机指纹及角色，确认在这些主机安装新集群</label>';
        form.querySelector("[data-confirm-plan]").onchange = function (e) { submit.disabled = !e.target.checked; };
      } catch (err) { if (form.isConnected && version === serial) error(message(err)); }
      finally { if (form.isConnected) check.disabled = false; }
    };
    form.onsubmit = async function (e) {
      e.preventDefault();
      if (!plan || submit.disabled) return;
      var req = readRequest(form);
      req.confirmed = true;
      req.registry_mirror = plan.registry_mirror || { provider: "dockerhub", endpoint: "" };
      req.hosts.forEach(function (h, i) { h.fingerprint = plan.hosts[i].fingerprint; });
      submit.disabled = check.disabled = true;
      try {
        var job = await request("/provision/jobs", "POST", req);
        form.querySelectorAll('input[type="password"]').forEach(function (input) { input.value = ""; });
        plan = null; req = null; closeAll(); detail(job.id);
      } catch (err) {
        if (err.code === 40304) {
          form.querySelectorAll('input[type="password"]').forEach(function (input) { input.value = ""; });
          plan = null; req = null; closeAll(); toast("已提交环境审批，凭据加密保留至任务结束或超时"); jobs();
        } else { error(message(err)); submit.disabled = false; }
      } finally { if (form.isConnected) check.disabled = false; }
    };
  }
  function detail(id) {
    modal("Kubernetes 部署任务 #" + id, '<div id="provisionJob">正在加载…</div>');
    var box = document.getElementById("provisionJob");
    async function load() {
      try {
        var j = await request("/provision/jobs/" + encodeURIComponent(id));
        if (!box.isConnected) return;
        var active = ["awaiting_approval", "queued", "running", "cancelling"].includes(j.status);
        box.innerHTML = '<p><b>' + E(j.name) + '</b>   ' + E(j.env) + '   ' + E(states[j.status] || j.status) + '</p>' +
          (j.error ? '<p class="x-aerr" role="alert">' + E(j.error) + '</p>' : "") +
          '<pre style="max-height:360px;overflow:auto;white-space:pre-wrap">' + E((j.logs || []).join("\n") || "等待调度或环境审批") + '</pre>' +
          (j.cluster_id ? '<a class="x-btn" data-close href="#/cluster/nodes/' + E(j.cluster_id) + '">查看集群</a>' : "") +
          (active && j.status !== "cancelling" ? '<button class="x-btn" type="button" data-cancel-job>取消任务</button>' : "");
        var cancel = box.querySelector("[data-cancel-job]");
        if (cancel) cancel.onclick = async function () {
          cancel.disabled = true;
          try { await request("/provision/jobs/" + encodeURIComponent(id) + "/cancel", "POST", {}); load(); }
          catch (err) { toast(message(err)); cancel.disabled = false; }
        };
        var link = box.querySelector("[data-close]");
        if (link) link.onclick = closeAll;
        if (active) setTimeout(function () { if (box.isConnected) load(); }, 5000);
      } catch (err) { if (box.isConnected) box.textContent = message(err); }
    }
    load();
  }
  async function jobs() {
    modal("Kubernetes 部署任务", '<div id="provisionJobs">正在加载…</div>');
    var box = document.getElementById("provisionJobs");
    try {
      var result = await request("/provision/jobs");
      if (!box.isConnected) return;
      box.innerHTML = result.items.length ? '<div style="overflow:auto"><table class="x-table"><thead><tr><th>集群</th><th>环境</th><th>状态</th><th>发起人</th><th></th></tr></thead><tbody>' +
        result.items.map(function (j) { return '<tr><td>' + E(j.name) + '</td><td>' + E(j.env) + '</td><td>' + E(states[j.status] || j.status) + '</td><td>' + E(j.created_name) + '</td><td><button class="x-btn" type="button" data-job-id="' + E(j.id) + '">详情</button></td></tr>'; }).join("") +
        '</tbody></table></div>' : '<p class="x-empty">暂无部署任务</p>';
      box.querySelectorAll("[data-job-id]").forEach(function (b) { b.onclick = function () { detail(b.dataset.jobId); }; });
    } catch (err) { if (box.isConnected) box.textContent = message(err); }
  }
  document.addEventListener("click", function (e) {
    var button = e.target.closest("[data-clprovision],[data-cljobs],[data-climport]");
    if (!button) return;
    if (button.hasAttribute("data-clprovision")) open();
    else if (button.hasAttribute("data-cljobs")) jobs();
    else document.dispatchEvent(new Event("xingshu:cluster-import"));
  });
  window.XSProvision = { open: open, jobs: jobs, detail: detail, readRequest: readRequest };
})();
