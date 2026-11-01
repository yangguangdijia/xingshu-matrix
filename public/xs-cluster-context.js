(function (root) {
  "use strict";

  var STORAGE_KEY = "xs_cluster_context";
  var scopes = { platform: true, environment: true, cluster: true, aggregate: true };
  var state = { kind: "unselected", cluster_id: "", name: "", env: "", version: 0 };
  var epoch = 0;
  var listeners = [];
  var controllers = new Set();
  var pageScopes = Object.create(null);
  var activePage = "";
  var cache = new Map();

  function storageRead() {
    try {
      var value = root.localStorage.getItem(STORAGE_KEY);
      return value ? JSON.parse(value) : null;
    } catch (_) { return null; }
  }
  var savedAtBoot = storageRead();
  if (savedAtBoot && Number.isSafeInteger(savedAtBoot.version) && savedAtBoot.version >= 0 &&
    savedAtBoot.version < Number.MAX_SAFE_INTEGER - 1) {
    epoch = savedAtBoot.version;
    state.version = epoch;
  }

  function persist() {
    try {
      if (state.kind === "unselected") root.localStorage.removeItem(STORAGE_KEY);
      else root.localStorage.setItem(STORAGE_KEY, JSON.stringify({
        kind: state.kind, cluster_id: state.cluster_id, name: state.name,
        env: state.env, version: state.version
      }));
    } catch (_) {}
  }

  function snapshot() { return Object.assign({ state: state.kind }, state); }
  function emit(previous, reason) {
    var detail = { current: snapshot(), previous: previous, epoch: epoch, reason: reason || "set" };
    listeners.slice().forEach(function (fn) {
      try { fn(detail); } catch (err) { if (root.console) root.console.error("[XSClusterContext]", err); }
    });
    if (typeof root.dispatchEvent === "function" && typeof root.CustomEvent === "function") {
      root.dispatchEvent(new root.CustomEvent("xs:cluster-context", { detail: detail }));
    }
  }
  function cancelControllers(reason, previous, next) {
    controllers.forEach(function (entry) {
      if (entry.scope === "platform" || (entry.scope === "environment" && previous.env === next.env)) return;
      if (!entry.controller.signal.aborted) {
        entry.reason = reason || "context-changed";
        entry.controller.abort();
      }
      controllers.delete(entry);
    });
  }
  function normalize(selection) {
    var kind = selection && (selection.kind || selection.state);
    if (!selection || ["unselected", "cluster", "all"].indexOf(kind) < 0 ||
      (selection.kind && selection.state && selection.kind !== selection.state)) {
      throw new TypeError("Invalid cluster context kind");
    }
    if (kind === "cluster" && (selection.cluster_id === undefined || selection.cluster_id === null || String(selection.cluster_id).trim() === "")) {
      throw new TypeError("cluster_id is required");
    }
    if (kind !== "cluster" && selection.cluster_id) throw new TypeError("Only a cluster context can contain cluster_id");
    if (kind === "cluster" && !String(selection.name || "").trim()) throw new TypeError("Cluster name is required");
    if (kind === "cluster" && !String(selection.env || "").trim()) throw new TypeError("Cluster env is required");
    return {
      kind: kind,
      cluster_id: kind === "cluster" ? String(selection.cluster_id) : "",
      name: kind === "cluster" ? String(selection.name || "") : "",
      env: String(selection.env || ""),
      version: Number(selection.version) || 0
    };
  }
  function clearScopedCache(previous, next) {
    cache.forEach(function (_, key) {
      if (key.indexOf("cluster:") === 0 || key.indexOf("aggregate:") === 0 ||
        (previous.env !== next.env && key.indexOf("environment:") === 0)) cache.delete(key);
    });
  }
  function apply(selection, reason) {
    var next = normalize(selection);
    var previous = snapshot();
    if (next.kind === state.kind && next.cluster_id === state.cluster_id && next.name === state.name && next.env === state.env &&
      !(reason === "clear" && storageRead())) {
      next.version = state.version;
      return snapshot();
    }
    epoch += 1;
    next.version = epoch;
    cancelControllers("context-changed", previous, next);
    clearScopedCache(previous, next);
    state = next;
    persist();
    emit(previous, reason);
    return snapshot();
  }

  var api = {
    get: snapshot,
    epoch: function () { return epoch; },
    set: function (selection) { return apply(selection, "set"); },
    clear: function () { return apply({ kind: "unselected" }, "clear"); },
    subscribe: function (fn) {
      if (typeof fn !== "function") return function () {};
      listeners.push(fn);
      return function () { listeners = listeners.filter(function (item) { return item !== fn; }); };
    },
    isCurrent: function (version) {
      if (version && typeof version === "object") {
        return version.version === epoch && (version.kind || version.state) === state.kind &&
          version.cluster_id === state.cluster_id && version.env === state.env;
      }
      return version === epoch;
    },
    assertCurrent: function (version) {
      if (!api.isCurrent(version)) {
        var err = new Error("集群上下文已变化，请重新加载");
        err.name = "XS.ContextChangedError";
        err.code = "XS_CONTEXT_CHANGED";
        throw err;
      }
      return true;
    },
    restoreCandidate: function () {
      var candidate = storageRead();
      if (!candidate) return null;
      try { return normalize(candidate); } catch (_) { return null; }
    },
    validate: function (candidate, clusters, env) {
      try {
        var selected = normalize(candidate);
        if (!env || !Array.isArray(clusters)) return false;
        if (selected.kind === "all") return selected.env === String(env);
        if (selected.kind !== "cluster") return false;
        var match = clusters.find(function (cluster) { return String(cluster.id) === selected.cluster_id; });
        return !!match && String(match.env || "") === String(env) && selected.env === String(env);
      } catch (_) { return false; }
    },
    activate: function (candidate, options) {
      options = options || {};
      var selected = normalize(candidate);
      if (!api.validate(selected, options.clusters, options.env)) {
        throw new Error("集群选择不存在或与当前环境不匹配");
      }
      if (selected.kind === "cluster") {
        var matched = options.clusters.find(function (cluster) { return String(cluster.id) === selected.cluster_id; });
        return apply({
          kind: "cluster", cluster_id: String(matched.id),
          name: matched.name || matched.cluster_name || selected.name,
          env: matched.env || selected.env
        }, "activate");
      }
      if (options.allowAll !== true) throw new Error("必须显式允许全部集群视图");
      return apply({ kind: selected.kind, env: options.env }, "activate");
    },
    registerPageScope: function (name, scope) {
      if (!name) throw new TypeError("Page name is required");
      scope = scope || "platform";
      if (!scopes[scope]) throw new TypeError("Unknown page scope: " + scope);
      pageScopes[String(name)] = scope;
      return scope;
    },
    pageScope: function (name) { return pageScopes[String(name)] || null; },
    enterPage: function (name) {
      if (!api.pageScope(name)) throw new TypeError("Page scope is not registered: " + name);
      if (activePage !== name) {
        controllers.forEach(function (entry) {
          if (entry.page === activePage && !entry.controller.signal.aborted) {
            entry.reason = "page-changed";
            entry.controller.abort();
            controllers.delete(entry);
          }
        });
      }
      activePage = String(name);
      return api.pageScope(name);
    },
    invalidate: function (reason) {
      var previous = snapshot();
      epoch += 1;
      state.version = epoch;
      cancelControllers(reason || "invalidated", previous, state);
      clearScopedCache(previous, state);
      persist();
      emit(previous, reason || "invalidated");
      return epoch;
    },
    createRequestScope: function (options) {
      options = options || {};
      var controller = new AbortController();
      var requestPage = options.page || activePage;
      var scope = options.scope || api.pageScope(requestPage) || "cluster";
      if (!scopes[scope]) throw new TypeError("Unknown request scope: " + scope);
      var entry = { controller: controller, epoch: epoch, env: state.env,
        page: requestPage, scope: scope, reason: "" };
      controllers.add(entry);
      function release() { controllers.delete(entry); }
      controller.signal.addEventListener("abort", release, { once: true });
      return {
        signal: controller.signal,
        epoch: entry.epoch,
        cancel: function () {
          entry.reason = "cancelled";
          controller.abort();
        },
        reason: function () { return entry.reason; },
        isCurrent: function () {
          return !controller.signal.aborted && (scope === "platform" ||
            (scope === "environment" ? entry.env === state.env : entry.epoch === epoch));
        },
        assertCurrent: function () {
          if (controller.signal.aborted) {
            var err = new Error("请求已取消");
            err.name = "XS.RequestCancelledError";
            err.code = "XS_REQUEST_CANCELLED";
            throw err;
          }
          if (!this.isCurrent()) api.assertCurrent(entry.epoch);
          return true;
        },
        release: release
      };
    },
    cache: {
      key: function (key, scope) {
        scope = scope || "cluster";
        if (!scopes[scope]) throw new TypeError("Unknown cache scope: " + scope);
        if (scope === "cluster" && state.kind !== "cluster") throw new Error("未选择集群");
        if (scope === "aggregate" && state.kind !== "all") throw new Error("未选择全部集群视图");
        if (scope === "environment" && !state.env) throw new Error("未选择环境");
        var context = scope === "platform" ? "" : scope === "environment" ? state.env :
          scope === "aggregate" ? state.env + ":" + epoch : state.cluster_id + ":" + epoch;
        return scope + ":" + context + ":" + String(key);
      },
      get: function (key, scope) { return cache.get(api.cache.key(key, scope)); },
      set: function (key, value, scope) { cache.set(api.cache.key(key, scope), value); return value; },
      has: function (key, scope) { return cache.has(api.cache.key(key, scope)); },
      clear: function (scope) {
        if (!scope) return cache.clear();
        if (!scopes[scope]) throw new TypeError("Unknown cache scope: " + scope);
        cache.forEach(function (_, key) { if (key.indexOf(scope + ":") === 0) cache.delete(key); });
      }
    },
    scopes: Object.keys(scopes)
  };

  root.XSClusterContext = api;
})(typeof window !== "undefined" ? window : globalThis);
