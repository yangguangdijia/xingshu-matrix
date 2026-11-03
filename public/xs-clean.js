/* 隐藏段落级说明文字：p.x-frac 长句且非状态/空态/错误 */
(function () {
  var KEEP = /^(正在|暂无|没有|无|加载|未接入|未配置|未安装|请选择|请先|失败|共 ?\d)/;
  function scan(root) {
    [].forEach.call((root || document).querySelectorAll("p.x-frac:not([role]):not([data-xs-hint]),div.x-frac:not([role]):not([data-xs-hint])"), function (p) {
      if (p.closest("td,th,li,.x-empty,.x-nodata,.x-aerr,dialog,.x-modal,[role=dialog]")) return;
      var t = (p.textContent || "").trim();
      if (t.length > 14 && !KEEP.test(t) && /[，。；：（]/.test(t)) p.setAttribute("data-xs-hint", "");
    });
  }
  var pending = false;
  new MutationObserver(function () {
    if (pending) return; pending = true;
    requestAnimationFrame(function () { pending = false; scan(document.getElementById("view") || document); });
  }).observe(document.documentElement, { childList: true, subtree: true });
  document.addEventListener("DOMContentLoaded", function () { scan(); });
})();
/* 未选择集群时默认选中第一个已接入集群（与 KubeSphere 默认进入集群一致），避免每页都提示「请选择集群」 */
(function () {
  var tried = false;
  function pick() {
    var ctx = window.XSClusterContext, XS = window.XS;
    if (tried || !ctx || !XS || XS.mode !== "live" || typeof XS.get !== "function") return;
    var st = ctx.get && ctx.get();
    if (!st || st.kind !== "unselected") { tried = true; return; }
    tried = true;
    XS.get("/clusters", { page_size: 50 }).then(function (d) {
      var items = (d && (d.items || d)) || [];
      if (!items.length || (ctx.get().kind !== "unselected")) return;
      var c = items[0];
      ctx.set({ kind: "cluster", cluster_id: c.id, name: c.name || ("#" + c.id), env: c.env || "dev" });
    }).catch(function () { tried = false; });
  }
  setInterval(pick, 1500);
})();
/* 顶栏集群下拉与当前集群上下文保持一致 */
(function () {
  function sync() {
    var sel = document.getElementById("clctx"), ctx = window.XSClusterContext;
    if (!sel || !ctx || !ctx.get) return;
    var st = ctx.get();
    if (st.kind !== "cluster") return;
    var id = String(st.cluster_id);
    if (![].some.call(sel.options, function (o) { return o.value === id; })) {
      var o = document.createElement("option"); o.value = id; o.textContent = st.name + "   " + (st.env || ""); sel.appendChild(o);
    }
    if (sel.value !== id) sel.value = id;
    sel.disabled = false;
  }
  window.addEventListener("xs:cluster-context", function () { setTimeout(sync, 0); });
  setInterval(sync, 2000);
})();
