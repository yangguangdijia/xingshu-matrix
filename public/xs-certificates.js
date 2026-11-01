(function () {
  "use strict";

  function E(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (char) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[char];
    });
  }

  function list(value) {
    return Array.isArray(value) ? value : [];
  }

  function closeDrawer() {
    var drawer = document.getElementById("drawer");
    var mask = document.getElementById("mask");
    if (drawer) drawer.classList.remove("open");
    if (mask) mask.hidden = true;
  }

  function drawer(title, body, footer) {
    var target = document.getElementById("drawer");
    var mask = document.getElementById("mask");
    if (!target) return null;
    target.classList.add("x-drawer--wide");
    target.setAttribute("aria-label", title);
    target.innerHTML = '<div class="x-dhead"><span>' + E(title) + '</span><button class="x-x" type="button" data-cert-close aria-label="关闭">×</button></div>' +
      '<div class="x-dbody">' + body + '</div><div class="x-dfoot">' + footer + "</div>";
    if (mask) mask.hidden = false;
    target.querySelectorAll("[data-cert-close]").forEach(function (button) { button.onclick = closeDrawer; });
    requestAnimationFrame(function () { target.classList.add("open"); });
    return target;
  }

  function openImport(ctx, reload) {
    var canWrite = typeof XS.can === "function" && XS.can("config:write");
    var formRoot = drawer("导入证书", '<form id="certificateForm" class="x-cert-form" novalidate>' +
      '<div data-cert-import-fields>' +
      '<div class="x-row2"><label><span class="x-req">命名空间</span><input class="x-input x-mono" name="namespace" required maxlength="63" autocomplete="off"></label>' +
      '<label><span class="x-req">Secret 名称</span><input class="x-input x-mono" name="name" required maxlength="253" autocomplete="off"></label></div>' +
      '<label><span>用途</span><input class="x-input" name="purpose" maxlength="160" placeholder="例如：api.example.com HTTPS"></label>' +
      '<div class="x-row2"><label><span>证书文件</span><input class="x-input" type="file" data-cert-file="certificate" accept=".crt,.pem,application/x-pem-file"></label>' +
      '<label><span>私钥文件</span><input class="x-input" type="file" data-cert-file="private_key" accept=".key,.pem,application/x-pem-file"></label></div>' +
      '<label><span class="x-req">证书 PEM</span><textarea class="x-input x-cert-pem" name="certificate" required spellcheck="false" autocomplete="off"></textarea></label>' +
      '<label><span class="x-req">私钥 PEM</span><textarea class="x-input x-cert-pem" name="private_key" required spellcheck="false" autocomplete="off"></textarea></label>' +
      '</div><p class="x-aerr" data-cert-form-error role="alert" hidden></p>' +
      '<p class="x-frac" data-cert-form-status role="status" aria-live="polite"></p></form>',
      '<button class="x-btn" type="button" data-cert-close>取消</button><button class="x-btn x-btn--pri" type="submit" form="certificateForm" data-cert-submit' + (canWrite ? "" : " disabled") + '>导入证书</button>');
    if (!formRoot) return;

    var form = formRoot.querySelector("#certificateForm");
    var submit = formRoot.querySelector("[data-cert-submit]");
    var status = formRoot.querySelector("[data-cert-form-status]");
    var error = formRoot.querySelector("[data-cert-form-error]");
    if (!form || !submit) return;

    formRoot.querySelectorAll("[data-cert-file]").forEach(function (input) {
      input.onchange = function () {
        var file = input.files && input.files[0];
        if (!file) return;
        file.text().then(function (text) {
          if (form.elements[input.dataset.certFile]) form.elements[input.dataset.certFile].value = text;
        }).catch(function () {
          error.textContent = "无法读取所选文件。";
          error.hidden = false;
        });
      };
    });

    form.onsubmit = function (event) {
      event.preventDefault();
      if (!canWrite || submit.disabled || !form.reportValidity()) return;
      submit.disabled = true;
      status.textContent = "正在提交…";
      error.hidden = true;
      XS.post("/clusters/" + encodeURIComponent(ctx.cluster.id) + "/certificates/import", {
        namespace: form.elements.namespace.value.trim(),
        name: form.elements.name.value.trim(),
        purpose: form.elements.purpose.value.trim(),
        certificate: form.elements.certificate.value,
        private_key: form.elements.private_key.value
      }, { scope: "cluster" }).then(function () {
        closeDrawer();
        if (typeof XS.toast === "function") XS.toast("证书导入已提交");
        reload();
      }).catch(function (err) {
        if (typeof XS.isApproval === "function" && XS.isApproval(err)) {
          form.elements.certificate.value = "";
          form.elements.private_key.value = "";
          closeDrawer();
          if (typeof XS.toast === "function") XS.toast("证书变更已提交审批");
          return;
        }
        status.textContent = "";
        error.textContent = err && err.message || "证书导入失败";
        error.hidden = false;
        submit.disabled = false;
      });
    };
  }

  function detailRows(row) {
    var refs = list(row.ingress_refs).join(", ");
    var sans = list(row.dns_names).concat(list(row.ip_addresses)).join(", ");
    return [
      ["命名空间", row.namespace],
      ["用途", row.purpose || (refs ? "Ingress TLS" : "TLS Secret")],
      ["签发者", row.issuer],
      ["SAN / Common Name", sans || row.common_name],
      ["生效时间", row.not_before],
      ["到期时间", row.not_after],
      ["来源", row.source],
      ["Ingress 引用", refs],
      ["状态", row.parse_error || row.status]
    ].map(function (item) {
      return "<dt>" + E(item[0]) + "</dt><dd>" + E(item[1] || "—") + "</dd>";
    }).join("");
  }

  function openDetails(row) {
    drawer(row.namespace + "/" + row.name,
      '<dl class="x-cert-details">' + detailRows(row) + "</dl>",
      '<button class="x-btn" type="button" data-cert-close>关闭</button>');
  }

  function render(el, page, ctx) {
    var clusterId = String(ctx.cluster.id);
    var revision = page.rev;
    var requestToken = {};
    el._xsCertificateRequest = requestToken;
    function active() {
      return el._xsCertificateRequest === requestToken && page.rev === revision &&
        String(ctx.cluster.id) === clusterId && el.isConnected !== false;
    }
    var canWrite = typeof XS.can === "function" && XS.can("config:write");
    var root = "/clusters/" + encodeURIComponent(clusterId) + "/certificates";
    page.draw(ctx, '<div class="x-cert-toolbar"><span class="x-frac">' + E(ctx.cluster.name) + '</span>' +
      '<button class="x-btn" type="button" data-cert-refresh title="刷新证书">刷新</button>' +
      '<button class="x-btn x-btn--pri" type="button" data-cert-create' + (canWrite ? "" : ' disabled title="需要 config:write 权限"') + '>+ 导入证书</button></div>' +
      '<div data-cert-content class="x-cert-state" aria-live="polite">正在读取集群证书…</div>');
    return XS.get(root, null, { scope: "cluster" }).then(function (data) {
      if (!active()) return;
      var rows = list(data.items);
      var body = "";
      if (rows.length) {
        body += '<div class="x-cert-table-wrap"><table class="x-table x-table--plain x-cert-table"><thead><tr><th>名称 / 用途</th><th>签发者 / SAN</th><th>到期时间</th><th>来源</th><th>Ingress 引用</th><th>状态</th><th>操作</th></tr></thead><tbody>' +
          rows.map(function (row, index) {
            var identity = list(row.dns_names).join(", ") || row.common_name || list(row.ip_addresses).join(", ") || "—";
            var purpose = row.purpose || (list(row.ingress_refs).length ? "Ingress TLS" : "TLS Secret");
            return '<tr><td><strong>' + E(row.namespace + "/" + row.name) + '</strong><small>' + E(purpose) + "</small></td>" +
              '<td class="x-cert-wrap">' + E(row.issuer || "—") + "<small>" + E(identity) + "</small></td>" +
              "<td>" + E(row.not_after || "—") + "</td><td>" + E(row.source || "Kubernetes TLS Secret") + "</td>" +
              "<td>" + E(list(row.ingress_refs).join(", ") || "—") + "</td>" +
              "<td>" + E(row.parse_error || row.status || "—") + "</td>" +
              '<td><button class="x-btn" type="button" data-cert-view="' + index + '">查看</button></td></tr>';
          }).join("") + "</tbody></table></div>";
      } else {
        body += '<p class="x-empty">当前集群未发现 kubernetes.io/tls Secret。</p>';
      }
      var content = el.querySelector("[data-cert-content]");
      if (content) content.innerHTML = body;
      var refresh = el.querySelector("[data-cert-refresh]");
      if (refresh) refresh.onclick = function () { render(el, page, ctx); };
      var create = el.querySelector("[data-cert-create]");
      if (create) create.onclick = function () { if (active() && canWrite) openImport(ctx, function () { render(el, page, ctx); }); };
      el.querySelectorAll("[data-cert-view]").forEach(function (button) {
        button.onclick = function () {
          var row = rows[Number(button.dataset.certView)];
          if (active() && row) openDetails(row);
        };
      });
    }).catch(function (err) {
      if (!active()) return;
      var content = el.querySelector("[data-cert-content]");
      if (content) {
        content.className = "x-cert-error";
        content.setAttribute("role", "alert");
        content.textContent = "读取当前集群证书失败：" + (err && err.message || "未知错误");
      }
    });
  }

  window.XSCertificates = { render: render };
})();
