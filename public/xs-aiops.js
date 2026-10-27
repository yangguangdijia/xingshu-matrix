(function () {
  "use strict";
  var E = function (v) {
    return String(v == null ? "" : v).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; });
  };
  var states = { not_analyzed: "尚未分析", queued: "等待分析", analyzing: "正在分析",
    ready: "待运维决策", manual: "人工处理", awaiting_approval: "等待环境审批", approved: "等待执行",
    executing: "正在处置并验证", succeeded: "处置已验证，请等待新一轮告警采样", failed: "未完成，请人工核查" };
  var active = ["queued", "analyzing", "approved", "executing"];
  function mount(modal, alert) {
    var state = modal.querySelector("[data-alert-ai-state]"), result = modal.querySelector("[data-alert-ai-result]");
    var analyze = modal.querySelector("[data-alert-ai]"), execute = modal.querySelector("[data-alert-remediate]");
    var manual = modal.querySelector("[data-alert-manual]"), current, timer;
    if (!state || !result || !analyze || !execute || !manual) return;
    var root = "/observability/alerts/" + encodeURIComponent(alert.id);
    function connected() { return modal.isConnected && modal.contains(state) && !modal.hidden; }
    function error(err) { state.textContent = "请求失败：" + (err.message || "未知错误"); }
    function paint(d) {
      current = d;
      var report = d.analysis || {}, target = d.target || {};
      state.textContent = (states[d.status] || d.status) + (d.error ? "   " + d.error : "") + (d.decision_name ? "   决策人：" + d.decision_name : "");
      if (d.approval_id) state.textContent += "   审批单 #" + d.approval_id;
      result.innerHTML = report.impact_summary ? '<dl class="x-alert-facts">' + [
        ["影响范围", report.impact_scope || "unknown"], ["影响判断", report.impact_summary], ["可能原因", report.probable_causes],
        ["解决方案", report.recommendation], ["操作风险", report.risk], ["验证步骤", report.verification_steps],
        ["置信度", report.confidence || "low"], ["受控处置目标", target.uid ? target.cluster_name + "   " + target.namespace + "/" + target.name + "（就绪 " + target.ready + "/" + target.replicas + "）" : "未核验到可执行的唯一控制器，仅支持人工处理"]
      ].map(function (row) { return "<dt>" + E(row[0]) + "</dt><dd style=\"white-space:pre-wrap\">" + E(row[1] || "未提供") + "</dd>"; }).join("") + "</dl>" : "";
      var firing = alert.status === "firing" && alert.handle_status !== "closed";
      execute.hidden = !(firing && d.status === "ready" && target.uid && report.proposed_action === "restart_workload");
      execute.disabled = false;
      execute.textContent = "确认 AI 处理   重启 " + (target.name || "工作负载");
      manual.hidden = !firing || !d.id || !["queued", "analyzing", "ready", "failed"].includes(d.status);
      manual.disabled = false;
      analyze.disabled = active.includes(d.status) || ["manual", "awaiting_approval", "succeeded"].includes(d.status) || !!d.decision_by;
      analyze.textContent = d.id ? "重新分析" : "分析告警";
      if (active.includes(d.status) || d.status === "awaiting_approval") timer = setTimeout(load, 5000);
    }
    async function load() {
      clearTimeout(timer);
      if (!connected()) return;
      try { var d = await XS.get(root + "/diagnosis"); if (connected()) paint(d); }
      catch (err) { if (connected()) error(err); }
    }
    analyze.onclick = async function () {
      if (analyze.disabled) return;
      analyze.disabled = true; state.textContent = "正在提交分析任务…";
      try { await XS.post(root + "/diagnose", {}); if (connected()) load(); }
      catch (err) { if (connected()) { error(err); analyze.disabled = false; } }
    };
    async function decide(decision) {
      if (!current || !current.id) return;
      manual.disabled = execute.disabled = true;
      try {
        await XS.post(root + "/decision", { diagnosis_id: current.id, decision: decision });
        if (connected()) load();
        else if (window.XSOpenAlertDetail) window.XSOpenAlertDetail(alert.id);
      } catch (err) {
        if (err.code === 40304) {
          toast("AI 处置已提交环境审批");
          if (connected()) load(); else if (window.XSOpenAlertDetail) window.XSOpenAlertDetail(alert.id);
        } else if (connected()) { error(err); manual.disabled = execute.disabled = false; }
        else toast(err.message || "处置请求失败");
      }
    }
    manual.onclick = function () { decide("manual"); };
    execute.onclick = function () {
      if (!current || current.status !== "ready") return;
      var report = current.analysis || {}, target = current.target || {};
      k8Confirm("确认 AI 处置", "<b>" + E(target.cluster_name + "   " + target.namespace + "/" + target.name) +
        "</b><p>" + E(report.impact_summary) + "</p><p>风险：" + E(report.risk || "重建 Pod 可能影响应用可用性，请核对副本和业务状态") +
        "</p><p>仅执行该控制器的滚动重启；环境审批和执行前资源身份检查仍生效。</p>", "确认并提交", function () { decide("ai"); });
    };
    load();
  }
  window.XSAIOps = { mount: mount };
})();
