// 前端自测用的最小 mock API + 静态服务器。仅使用 Node 内置模块。
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../public");
const port = Number(process.env.MOCK_PORT || 18080);
const issued = new Map();
let seq = 0;
const now = () => new Date().toISOString();

const permissions = ["user:read","user:manage","role:read","role:manage","setting:read","setting:manage","audit:read","approval:read","approval:approve","ai:read","ai:manage","ai:chat","cluster:read","node:read","node:write","namespace:read","namespace:write","workload:read","workload:write","host:read","host:write","host:exec","devcenter:read","devcenter:manage","automation:read","automation:manage","automation:run","alert:read","alert:silence","oncall:read","oncall:manage"];
const roleDefs = [
  { id: 1, name: "超级管理员", code: "admin", description: "全部权限", permissions: ["*"], builtin: true },
  { id: 2, name: "运维工程师", code: "ops-engineer", description: "运维中心读写", permissions: ["cluster:read","workload:read","host:read","host:exec","alert:read"], builtin: false },
  { id: 3, name: "研发工程师", code: "dev-engineer", description: "研发中心读写", permissions: ["devcenter:read","devcenter:manage"], builtin: false },
];
const roleRef = (name) => { const d = roleDefs.find((x) => x.name === name); return { id: d.id, name: d.name, code: d.code }; };
const users = [
  { id: 1, username: "admin", display_name: "管理员", email: "admin@xingshu.local", status: "active", roles: [roleRef("超级管理员")], created_at: now(), last_login_at: now() },
  { id: 2, username: "liming", display_name: "李明", email: "liming@xingshu.local", status: "active", roles: [roleRef("运维工程师")], created_at: now(), last_login_at: now() },
  { id: 3, username: "wangfang", display_name: "王芳", email: "wangfang@xingshu.local", status: "active", roles: [roleRef("研发工程师")], created_at: now(), last_login_at: now() },
];
const accounts = {
  // 本地预览只使用原平台默认账号；这里没有、也不应该放真实密码。
  admin: { id: 1, username: "admin", display_name: "管理员", password: "Xingshu@2026", must: false },
};
const roles = roleDefs;
const clusters = [{ id: 1, name: "demo-prod", env: "生产", description: "端到端自测集群", api_server: "https://k8s.example.com", version: "v1.31.2", status: "connected", last_error: "", created_at: now(), updated_at: now() }];
const hosts = [{ id: 1, name: "web-01", ip: "10.0.0.11", port: 22, os: "Linux", arch: "amd64", agent_version: "1.0.0", status: "online", online: true, env: "生产", group: "web", last_seen_at: now(), created_at: now() }];
const requirements = [
  { id: 1, title: "支持多集群资源对比", status: "开发中", priority: "P1", created_at: now(), updated_at: now() },
  { id: 2, title: "审计日志导出 CSV", status: "待验收", priority: "P2", created_at: now(), updated_at: now() },
];
const alerts = [{ fingerprint: "fp-1", status: "firing", labels: { alertname: "HighCPU", severity: "warning", instance: "demo-prod" }, annotations: { summary: "CPU 使用率超过 85%" }, starts_at: now(), updated_at: now(), claimed_by: "", silence_id: "" }];
const schedules = [{ id: 1, name: "默认值班", timezone: "Asia/Shanghai", members: ["李明", "王芳"], rotation_hours: 12, current: "李明", starts_at: now(), enabled: true }];
const incidents = [{ id: 1, title: "演示故障：网关 5xx 升高", severity: "P1", status: "open", assignee: "李明", created_at: now(), timeline: [{ at: now(), actor: "系统", action: "created" }] }];
const slos = [{ id: 1, name: "API 可用性", service: "api-gateway", target: 99.9, window_days: 30, budget_remaining: 0.87, status: "healthy" }];

function body(req) {
  return new Promise((resolve) => {
    let b = "";
    req.on("data", (c) => (b += c));
    req.on("end", () => { try { resolve(b ? JSON.parse(b) : {}); } catch { resolve({}); } });
  });
}
function ok(res, data, status = 200) {
  res.writeHead(status, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
  res.end(JSON.stringify({ code: 0, message: "ok", data, request_id: "mock-" + (++seq) }));
}
function fail(res, status, code, message) {
  res.writeHead(status, { "Content-Type": "application/json; charset=utf-8" });
  res.end(JSON.stringify({ code, message, data: null }));
}
function auth(req) {
  const h = req.headers.authorization || "";
  const token = h.startsWith("Bearer ") ? h.slice(7) : new URL(req.url, "http://x").searchParams.get("access_token");
  return token && issued.has(token) ? issued.get(token) : null;
}
const mime = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".woff2": "font/woff2" };

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://x");
  const p = u.pathname;
  if (p.startsWith("/api/v1/")) {
    if (p === "/api/v1/health") return ok(res, { status: "ok", version: "test", modules: 8, database: "ok", redis: "ok" });
    if (p === "/api/v1/auth/login" && req.method === "POST") {
      const b = await body(req);
      const acct = accounts[b.username];
      if (!acct || b.password !== acct.password) return fail(res, 401, 40102, "用户名或密码错误");
      const token = "access-" + (++seq), refresh = "refresh-" + seq;
      issued.set(token, { id: acct.id, username: acct.username, display_name: acct.display_name, must_change_password: acct.must, roles: ["超级管理员"], permissions, session_id: "s-" + acct.id });
      return ok(res, { token: { access_token: token, refresh_token: refresh, token_type: "Bearer", expires_in: 3600, expires_at: new Date(Date.now() + 3600e3).toISOString(), must_change_password: acct.must }, user: { id: acct.id, username: acct.username, display_name: acct.display_name, must_change_password: acct.must } });
    }
    if (p === "/api/v1/auth/me") {
      const a = auth(req); if (!a) return fail(res, 401, 40100, "authentication required");
      return ok(res, a);
    }
    if (p === "/api/v1/auth/logout") { const a = auth(req); if (a) for (const [k, v] of issued) if (v.session_id === a.session_id) issued.delete(k); return ok(res, null); }
    if (p === "/api/v1/auth/password") {
      const a = auth(req); if (!a) return fail(res, 401, 40100, "authentication required");
      const b = await body(req); const acct = accounts[a.username];
      if (!acct || b.old_password !== acct.password) return fail(res, 400, 40102, "原密码错误");
      acct.password = b.new_password; acct.must = false; a.must_change_password = false;
      return ok(res, { must_change_password: false });
    }
    if (p === "/api/v1/auth/sessions") return ok(res, []);
    if (p === "/api/v1/auth/sso/config") return ok(res, { enabled: false });
    if (!auth(req)) return fail(res, 401, 40100, "authentication required");
    if (p === "/api/v1/users") return ok(res, { items: users, total: users.length, page: 1, page_size: 50 });
    if (p === "/api/v1/roles") return ok(res, roles);
    if (p === "/api/v1/permissions") return ok(res, permissions.map((x) => ({ code: x, description: x })));
    if (p === "/api/v1/env-policies") return ok(res, [{ env: "生产", name: "生产", require_approval: true, approver_permission: "approval:approve" }]);
    if (p === "/api/v1/audit-logs") return ok(res, { items: [{ id: 1, username: "admin", action: "login", resource: "auth", result: "success", ip: "127.0.0.1", created_at: now() }, { id: 2, username: "liming", action: "scale", resource: "deployment/api-gateway", result: "success", ip: "10.0.0.2", created_at: now() }], total: 2, page: 1, page_size: 50 });
    if (p === "/api/v1/approvals") return ok(res, { items: [{ id: 1, type: "cluster.change", title: "[demo-prod] scale deployments/api-gateway", env: "生产", status: "pending", applicant: "liming", created_at: now() }], total: 1, page: 1, page_size: 50 });
    if (p === "/api/v1/clusters") return ok(res, { items: clusters, total: clusters.length, page: 1, page_size: 50 });
    if (p === "/api/v1/clusters/1") return ok(res, clusters[0]);
    if (p === "/api/v1/clusters/1/nodes") return ok(res, { items: [{ name: "node-1", ready: true, status: "Ready", roles: ["worker"], cpu_percent: 32, mem_percent: 54, pods: 18, created_at: now() }], total: 1 });
    if (p === "/api/v1/clusters/1/namespaces") return ok(res, { items: [{ name: "prod", status: "Active", created_at: now() }], total: 1 });
    if (p === "/api/v1/hosts") return ok(res, { items: hosts, total: hosts.length, page: 1, page_size: 50 });
    if (p === "/api/v1/devcenter/requirements") return ok(res, { items: requirements, total: requirements.length, page: 1, page_size: 50 });
    if (p === "/api/v1/devcenter/bugs") return ok(res, { items: [], total: 0, page: 1, page_size: 50 });
    if (p === "/api/v1/devcenter/iterations") return ok(res, { items: [], total: 0, page: 1, page_size: 50 });
    if (p === "/api/v1/observability/alerts") return ok(res, { items: alerts, total: alerts.length });
    if (p === "/api/v1/observability/data-sources") return ok(res, { items: [{ id: 1, name: "prometheus", type: "prometheus", url: "http://prometheus:9090", status: "ok" }] });
    if (p === "/api/v1/oncall/schedules") return ok(res, { items: schedules, total: schedules.length });
    if (p === "/api/v1/oncall/incidents") return ok(res, { items: incidents, total: incidents.length });
    if (p === "/api/v1/oncall/slos") return ok(res, { items: slos, total: slos.length });
    if (p === "/api/v1/settings/public") return ok(res, { platform_name: "星枢 Matrix", version: "test" });
    if (p === "/api/v1/settings") return ok(res, { items: [{ key: "platform.name", value: "星枢 Matrix" }] });
    return ok(res, { items: [], total: 0 });
  }
  let file = path.join(root, decodeURIComponent(p === "/" ? "/index.html" : p));
  if (!file.startsWith(root)) { res.writeHead(403); return res.end("forbidden"); }
  try {
    if (fs.statSync(file).isDirectory()) file = path.join(file, "index.html");
    const data = fs.readFileSync(file);
    res.writeHead(200, { "Content-Type": mime[path.extname(file)] || "application/octet-stream", "Cache-Control": "no-store" });
    res.end(data);
  } catch { res.writeHead(404); res.end("not found"); }
});

server.listen(port, "127.0.0.1", () => console.log("mock server http://127.0.0.1:" + port));
