/* 星枢 Matrix — 浏览器 API 客户端
 * 统一处理：API base 探测、JWT 存取、401 单飞刷新、统一错误、SSE/WS。
 * 设计为独立脚本，不依赖主 SPA 的私有变量；挂载到 window.XS。
 */
(function () {
  "use strict";

  var TOKEN_KEY = "xs_access_token";
  var REFRESH_KEY = "xs_refresh_token";
  var USER_KEY = "xs_user";
  var BASE_KEY = "xs_api_base";

  function read(key) { try { return localStorage.getItem(key) || ""; } catch (e) { return ""; } }
  function write(key, v) { try { v ? localStorage.setItem(key, v) : localStorage.removeItem(key); } catch (e) {} }
  function parseJSON(s, d) { try { return JSON.parse(s); } catch (e) { return d; } }

  function normalizeBase(b) {
    b = String(b || "").trim();
    if (!b) return "/api/v1";
    b = b.replace(/\/+$/, "");
    if (!/\/api\/v\d+$/.test(b)) b += "/api/v1";
    return b;
  }

  function candidateBases() {
    var out = [];
    var configured = window.XINGSHU_API_BASE;
    if (configured) out.push(normalizeBase(configured));
    var stored = read(BASE_KEY);
    if (stored) out.push(normalizeBase(stored));
    out.push("/api/v1");
    // 本地开发：静态页可能由 5500/3000 等端口提供，而后端在 8080。
    if (/^(localhost|127\.0\.0\.1)$/.test(location.hostname) && location.port !== "8080") {
      out.push("http://127.0.0.1:8080/api/v1");
    }
    return out.filter(function (v, i) { return v && out.indexOf(v) === i; });
  }

  var XS = {
    version: "1.1.0",
    base: "/api/v1",
    mode: "unknown",
    user: parseJSON(read(USER_KEY), null),
    ready: null,
    _listeners: {},
    _refreshing: null,
    _probing: null
  };

  function emit(name, payload) {
    (XS._listeners[name] || []).slice().forEach(function (fn) {
      try { fn(payload); } catch (e) { console.error("[XS] listener", name, e); }
    });
  }
  XS.on = function (name, fn) {
    if (typeof fn !== "function") return function () {};
    (XS._listeners[name] = XS._listeners[name] || []).push(fn);
    return function () { XS._listeners[name] = (XS._listeners[name] || []).filter(function (x) { return x !== fn; }); };
  };

  function ApiError(message, status, code, details) {
    this.name = "XS.ApiError";
    this.message = message || "请求失败";
    this.status = status || 0;
    this.code = code || 0;
    this.details = details || null;
  }
  ApiError.prototype = Object.create(Error.prototype);
  ApiError.prototype.constructor = ApiError;
  XS.ApiError = ApiError;

  XS.isApproval = function (err) { return !!(err && err.code === 40304); };
  XS.isAuth = function (err) { return !!(err && (err.status === 401 || err.code === 40100 || err.code === 40101)); };
  XS.isStale = function (err) { return !!(err && err.code === "XS_CONTEXT_CHANGED"); };
  XS.isCancelled = function (err) { return !!(err && (err.code === "XS_REQUEST_CANCELLED" || XS.isStale(err))); };

  XS.toast = function (msg) {
    if (typeof window.toast === "function") { try { window.toast(msg); return; } catch (e) {} }
    console.info("[XS]", msg);
  };

  XS.token = function () { return read(TOKEN_KEY); };
  XS.refreshToken = function () { return read(REFRESH_KEY); };
  XS.clearSession = function () {
    write(TOKEN_KEY, ""); write(REFRESH_KEY, ""); write(USER_KEY, "");
    XS.user = null;
    emit("auth", null);
  };
  XS.setSession = function (pair, user) {
    if (pair && pair.access_token) write(TOKEN_KEY, pair.access_token);
    if (pair && pair.refresh_token) write(REFRESH_KEY, pair.refresh_token);
    if (user) { write(USER_KEY, JSON.stringify(user)); XS.user = user; }
    emit("auth", { user: XS.user, token: XS.token() });
  };

  function setBase(b) {
    XS.base = normalizeBase(b);
    write(BASE_KEY, XS.base);
  }

  function url(path, query) {
    var p = path.charAt(0) === "/" ? path : "/" + path;
    var u = XS.base + p;
    if (query && typeof query === "object") {
      var qs = [];
      Object.keys(query).forEach(function (k) {
        if (query[k] === undefined || query[k] === null || query[k] === "") return;
        qs.push(encodeURIComponent(k) + "=" + encodeURIComponent(query[k]));
      });
      if (qs.length) u += (u.indexOf("?") >= 0 ? "&" : "?") + qs.join("&");
    }
    return u;
  }

  function parseBody(res) {
    var ct = (res.headers.get("content-type") || "").toLowerCase();
    if (ct.indexOf("json") >= 0) return res.json().catch(function () { return null; });
    return res.text().then(function (text) { return text ? { message: text } : null; });
  }

  function contextChanged() { return new ApiError("集群上下文已变化，请重新加载", 0, "XS_CONTEXT_CHANGED"); }
  function requestCancelled() { return new ApiError("请求已取消", 0, "XS_REQUEST_CANCELLED"); }
  function assertRequestScope(options) {
    if (options._clusterScope && !options._clusterScope.context.isCurrent(options._clusterScope.snapshot)) throw contextChanged();
    if (options.signal && options.signal.aborted) throw requestCancelled();
  }

  function prepareScope(path, options) {
    if (!options.scope || options.scope === "platform" || options.scope === "environment") return options;
    if (options.scope !== "cluster") throw new ApiError("此请求范围尚不支持，请逐集群查询", 0, "XS_SCOPE_UNSUPPORTED");
    var context = window.XSClusterContext;
    if (!context || typeof context.get !== "function") throw new ApiError("集群上下文尚未加载", 0, "XS_CLUSTER_REQUIRED");
    var snapshot = context.get();
    if (snapshot.kind !== "cluster" || !snapshot.cluster_id) throw new ApiError("请先选择集群", 0, "XS_CLUSTER_REQUIRED");
    var query = Object.assign({}, options.query || {});
    var inlineQuery = String(path).split("?")[1] || "";
    var inlineIds = new URLSearchParams(inlineQuery).getAll("cluster_id");
    var suppliedIds = inlineIds.slice();
    if (query.cluster_id !== undefined && query.cluster_id !== null) suppliedIds.push(String(query.cluster_id));
    if (suppliedIds.some(function (id) { return id !== String(snapshot.cluster_id); })) {
      throw new ApiError("请求集群与当前集群不一致", 0, "XS_CLUSTER_MISMATCH");
    }
    if (!suppliedIds.length) query.cluster_id = snapshot.cluster_id;
    else if (inlineIds.length) delete query.cluster_id;
    return Object.assign({}, options, {
      query: query,
      _clusterScope: { context: context, snapshot: snapshot }
    });
  }

  function request(path, options) {
    options = options || {};
    try { assertRequestScope(options); } catch (err) { return Promise.reject(err); }
    var headers = Object.assign({ "Accept": "application/json" }, options.headers || {});
    var body = options.body;
    if (body !== undefined && body !== null && !(body instanceof FormData) && typeof body !== "string") {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    } else if (typeof body === "string" && body && !headers["Content-Type"] && !headers["content-type"]) {
      headers["Content-Type"] = "application/json";
    }
    var tok = XS.token();
    if (tok && !options.noAuth) headers.Authorization = "Bearer " + tok;
    var ctl = typeof AbortController === "undefined" ? null : new AbortController();
    var timer = null;
    var timedOut = false;
    var externalSignal = options.signal;
    function abort() { if (ctl) ctl.abort(); }
    if (externalSignal) {
      if (externalSignal.aborted) return Promise.reject(requestCancelled());
      if (ctl) externalSignal.addEventListener("abort", abort, { once: true });
    }
    if (ctl && options.timeoutMs !== 0) {
      timer = setTimeout(function () { timedOut = true; ctl.abort(); }, options.timeoutMs || 30000);
    }
    var unsubscribe = options._clusterScope && options._clusterScope.context.subscribe(function () { abort(); });
    function cleanup() {
      if (timer) clearTimeout(timer);
      if (unsubscribe) unsubscribe();
      if (externalSignal && ctl) externalSignal.removeEventListener("abort", abort);
    }
    var init = {
      method: options.method || "GET",
      headers: headers,
      body: body,
      credentials: options.credentials || "same-origin",
      signal: ctl ? ctl.signal : externalSignal
    };
    var response;
    try { response = fetch(url(path, options.query), init); }
    catch (err) { response = Promise.reject(err); }
    return Promise.resolve(response).then(function (res) {
      assertRequestScope(options);
      return parseBody(res).then(function (payload) {
        assertRequestScope(options);
        var code = payload && typeof payload.code === "number" ? payload.code : 0;
        if (res.status === 202 && code === 40304) {
          var pending = new ApiError((payload && payload.message) || "已提交审批", res.status, code, payload && payload.data);
          pending.approval = payload && payload.data;
          throw pending;
        }
        if (!res.ok || code !== 0) {
          throw new ApiError((payload && payload.message) || ("请求失败（HTTP " + res.status + "）"), res.status, code, payload && payload.data);
        }
        if (!payload || !("data" in payload)) {
          throw new ApiError("接口返回格式不正确（缺少 data）", res.status, 0, payload);
        }
        return payload.data;
      });
    }, function (err) {
      assertRequestScope(options);
      if (err && err.name === "AbortError") {
        throw timedOut ? new ApiError("请求超时", 0, 0, null) : requestCancelled();
      }
      throw new ApiError("无法连接后端：" + (err && err.message ? err.message : "网络错误"), 0, 0, null);
    }).finally(cleanup);
  }

  XS.refresh = function () {
    if (XS._refreshing) return XS._refreshing;
    var rt = XS.refreshToken();
    if (!rt) return Promise.reject(new ApiError("会话已失效，请重新登录", 401, 40100));
    XS._refreshing = request("/auth/refresh", { method: "POST", body: { refresh_token: rt }, noAuth: true, timeoutMs: 15000 })
      .then(function (pair) {
        XS.setSession(pair, XS.user);
        return pair;
      })
      .catch(function (err) {
        XS.clearSession();
        throw err;
      })
      .then(function (v) { XS._refreshing = null; return v; }, function (e) { XS._refreshing = null; throw e; });
    return XS._refreshing;
  };

  XS.api = function (path, options) {
    options = options || {};
    var prepared;
    try { prepared = prepareScope(path, options); } catch (err) { return Promise.reject(err); }
    return request(path, prepared).catch(function (err) {
      assertRequestScope(prepared);
      if (XS.isAuth(err) && !prepared._retried && XS.refreshToken()) {
        return XS.refresh().then(function () {
          assertRequestScope(prepared);
          var retry = Object.assign({}, prepared, { _retried: true });
          return request(path, retry);
        });
      }
      throw err;
    });
  };

  XS.get = function (path, query, opts) { return XS.api(path, Object.assign({ query: query }, opts || {})); };
  XS.post = function (path, body, opts) { return XS.api(path, Object.assign({ method: "POST", body: body }, opts || {})); };
  XS.put = function (path, body, opts) { return XS.api(path, Object.assign({ method: "PUT", body: body }, opts || {})); };
  XS.patch = function (path, body, opts) { return XS.api(path, Object.assign({ method: "PATCH", body: body }, opts || {})); };
  XS.del = function (path, opts) { return XS.api(path, Object.assign({ method: "DELETE" }, opts || {})); };
  // Page callers pass { scope: "cluster", signal: requestScope.signal } to scoped requests.
  XS.createRequestScope = function (options) {
    var context = window.XSClusterContext;
    if (context && typeof context.createRequestScope === "function") return context.createRequestScope(options);
    var controller = new AbortController();
    return { signal: controller.signal, cancel: function () { controller.abort(); } };
  };

  XS.probe = function () {
    if (XS._probing) return XS._probing;
    var bases = candidateBases();
    var i = 0;
    function next() {
      if (i >= bases.length) {
        XS.mode = "demo";
        XS.base = normalizeBase(bases[0] || "/api/v1");
        emit("mode", "demo");
        return "demo";
      }
      var b = bases[i++];
      return fetch(b.replace(/\/+$/, "") + "/health", { method: "GET", credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json().catch(function () { return {}; }) : Promise.reject(r.status); })
        .then(function () { setBase(b); XS.mode = "live"; emit("mode", "live"); return "live"; })
        .catch(function () { return next(); });
    }
    XS._probing = next().then(function (m) { XS._probing = null; return m; }, function (e) { XS._probing = null; XS.mode = "demo"; return "demo"; });
    return XS._probing;
  };

  XS.auth = {
    login: function (username, password) {
      return request("/auth/login", { method: "POST", body: { username: username, password: password }, noAuth: true, timeoutMs: 20000 })
        .then(function (data) {
          var user = data.user || (data.token && data.token.user) || null;
          XS.setSession(data.token || data, user);
          return { pair: data.token || data, user: user };
        });
    },
    me: function () { return XS.get("/auth/me").then(function (p) { XS.user = p; write(USER_KEY, JSON.stringify(p)); return p; }); },
    restore: function () {
      if (!XS.token() && !XS.refreshToken()) return Promise.resolve(null);
      return (XS.ready || XS.probe()).then(function (mode) {
        if (mode !== "live") return null;
        return XS.api("/auth/me", { timeoutMs: 12000 }).then(function (p) {
          XS.user = p; write(USER_KEY, JSON.stringify(p)); XS.mode = "live"; return p;
        }).catch(function (err) {
          if (XS.isAuth(err)) { XS.clearSession(); return null; }
          throw err;
        });
      });
    },
    logout: function () {
      var done = function () { XS.clearSession(); };
      if (XS.mode !== "live" || !XS.token()) { done(); return Promise.resolve(); }
      return XS.post("/auth/logout", null, { timeoutMs: 8000 }).then(done, done);
    },
    changePassword: function (oldPassword, newPassword) {
      return XS.put("/auth/password", { old_password: oldPassword, new_password: newPassword });
    },
    sessions: function () { return XS.get("/auth/sessions"); },
    revokeSession: function (sid) { return XS.del("/auth/sessions/" + encodeURIComponent(sid)); }
  };

  XS.can = function (perm) {
    var u = XS.user;
    if (!u) return false;
    if (u.roles && u.roles.indexOf("超级管理员") >= 0) return true;
    var perms = u.permissions || u.perms || [];
    return perms.indexOf(perm) >= 0 || perms.indexOf("*") >= 0;
  };

  // SSE：返回 abort 函数；用于运行日志/告警流。
  XS.stream = function (path, handlers, query, options) {
    handlers = handlers || {};
    var prepared;
    try { prepared = prepareScope(path, Object.assign({}, options || {}, { query: query })); assertRequestScope(prepared); }
    catch (err) {
      if (handlers.error) handlers.error(err);
      return function () {};
    }
    var ctl = new AbortController();
    var headers = { "Accept": "text/event-stream" };
    if (XS.token()) headers.Authorization = "Bearer " + XS.token();
    var externalSignal = prepared.signal;
    function abort() { ctl.abort(); }
    if (externalSignal) externalSignal.addEventListener("abort", abort, { once: true });
    var unsubscribe = prepared._clusterScope && prepared._clusterScope.context.subscribe(abort);
    function cleanup() {
      if (unsubscribe) unsubscribe();
      if (externalSignal) externalSignal.removeEventListener("abort", abort);
    }
    function assertStreamScope() {
      assertRequestScope(prepared);
      if (ctl.signal.aborted) throw requestCancelled();
    }
    var response;
    try { response = fetch(url(path, prepared.query), { headers: headers, signal: ctl.signal, credentials: "same-origin" }); }
    catch (err) { response = Promise.reject(err); }
    Promise.resolve(response).then(function (res) {
      assertStreamScope();
      if (!res.ok) throw new ApiError("日志流连接失败（HTTP " + res.status + "）", res.status);
      var reader = res.body.getReader();
      var dec = new TextDecoder();
      var buf = "";
      function pump() {
        return reader.read().then(function (r) {
          assertStreamScope();
          if (r.done) { if (handlers.end) handlers.end(); return; }
          buf += dec.decode(r.value, { stream: true });
          var idx;
          while ((idx = buf.indexOf("\n\n")) >= 0) {
            var chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
            var event = "message", data = "";
            chunk.split(/\r?\n/).forEach(function (line) {
              if (line.indexOf("event:") === 0) event = line.slice(6).trim();
              else if (line.indexOf("data:") === 0) data += line.slice(5).trim();
            });
            assertStreamScope();
            if (handlers[event]) handlers[event](data);
            assertStreamScope();
            if (handlers.message) handlers.message(event, data);
          }
          return pump();
        });
      }
      return pump();
    }).catch(function (err) {
      if (ctl.signal.aborted || (err && err.name === "AbortError") || XS.isCancelled(err)) return;
      if (handlers.error) handlers.error(err);
    }).finally(cleanup);
    return function () { try { ctl.abort(); } catch (e) {} };
  };

  XS.ws = function (path, query, options) {
    var prepared = prepareScope(path, Object.assign({}, options || {}, { query: query }));
    assertRequestScope(prepared);
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var q = Object.assign({}, prepared.query || {});
    if (XS.token()) q.access_token = XS.token();
    var qs = Object.keys(q).filter(function (k) { return q[k] !== undefined && q[k] !== null && q[k] !== ""; })
      .map(function (k) { return encodeURIComponent(k) + "=" + encodeURIComponent(q[k]); }).join("&");
    var b = XS.base.replace(/^https?:/, proto);
    var socket = new WebSocket(b + (path.charAt(0) === "/" ? path : "/" + path) + (qs ? (path.indexOf("?") >= 0 ? "&" : "?") + qs : ""));
    var externalSignal = prepared.signal;
    function close() { socket.close(); }
    var unsubscribe = prepared._clusterScope && prepared._clusterScope.context.subscribe(close);
    if (externalSignal) externalSignal.addEventListener("abort", close, { once: true });
    function cleanup() {
      if (unsubscribe) unsubscribe();
      if (externalSignal) externalSignal.removeEventListener("abort", close);
    }
    if (typeof socket.addEventListener === "function") {
      socket.addEventListener("close", cleanup, { once: true });
      if (prepared._clusterScope) socket.addEventListener("message", function (event) {
        if (prepared._clusterScope.context.isCurrent(prepared._clusterScope.snapshot) &&
          !(externalSignal && externalSignal.aborted)) return;
        if (typeof event.stopImmediatePropagation === "function") event.stopImmediatePropagation();
        close();
      }, { capture: true });
    }
    return socket;
  };

  XS.online = function () { return navigator.onLine !== false; };

  // Session restoration belongs to the application bootstrap. Keeping ready to
  // API discovery prevents duplicate /auth/me calls and stale-token races.
  XS.ready = XS.probe();

  window.XS = XS;
})();
