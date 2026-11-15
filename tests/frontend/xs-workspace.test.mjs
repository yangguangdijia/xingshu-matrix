import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { harness, tick } from "./xs-resource-pages.harness.mjs";

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const text = (node) => node.textContent + node.children.map(text).join("");
const XSS = `<img src=x onerror=alert(1)>"'&`;
const GIB = 1024 * 1024 * 1024;
const err = (status, message) => Object.assign(new Error(message || `请求失败（HTTP ${status}）`), { status });

const USERS = [{ id: 1, username: "admin", display_name: "系统管理员" }, { id: 2, username: "alice", display_name: "Alice" }, { id: 3, username: "bob", display_name: "bob" }];
const GROUPS = [{ id: 11, name: "数据库组" }];
const ROLES = [{ id: 21, name: "运维工程师" }, { id: 22, name: "只读访客" }];
const CLUSTERS = [{ id: 7, name: "prod-a", env: "生产" }, { id: 8, name: "test-b", env: "测试" }];

function db() {
  return {
    workspaces: [
      { id: 1, name: "支付中心", description: XSS, manager_id: 2, member_count: 2, project_count: 1, cpu_limit_milli: 8000, memory_limit_bytes: 16 * GIB },
      { id: 2, name: "数据平台", description: "", manager_id: null, member_count: 0, project_count: 0, cpu_limit_milli: 0, memory_limit_bytes: 0 }
    ],
    members: [{ id: 101, user_id: 2, role: "owner", created_at: "2026-11-15T08:00:00Z" }, { id: 102, group_id: 11, role: "viewer", created_at: "2026-11-15T08:00:00Z" }],
    projects: [{ id: 201, cluster_id: 7, namespace: "pay" }, { id: 202, cluster_id: 99, namespace: "gone" }],
    bindings: [
      { id: 301, role_id: 21, user_id: 3, scope: "workspace", scope_value: "1" },
      { id: 302, role_id: 22, group_id: 11, scope: "project", scope_value: "7/pay" },
      { id: 303, role_id: 22, user_id: 3, scope: "workspace", scope_value: "2" },
      { id: 304, role_id: 22, user_id: 3, scope: "global", scope_value: "" }
    ]
  };
}

function setup({ perms = ["workspace:read", "workspace:manage"], data = db(), extra } = {}) {
  const h = harness("xs-workspace.js", {
    permissions: perms,
    api: async (path, o = {}) => {
      if (extra) { const r = await extra(path, o); if (r !== undefined) return r; }
      const m = o.method || "GET";
      if (path === "/users") return { items: USERS, total: USERS.length };
      if (path === "/user-groups") return { items: GROUPS, total: 1 };
      if (path === "/roles") return ROLES; // 现有接口直接返回数组
      if (path === "/clusters") return { items: CLUSTERS, total: 2 };
      if (path === "/clusters/7/namespaces") return { items: [{ name: "pay" }, { name: "web" }], total: 2 };
      if (path === "/workspaces" && m === "GET") return { items: data.workspaces, total: data.workspaces.length };
      if (path === "/workspaces" && m === "POST") return { id: 3, ...o.body };
      if (/^\/workspaces\/\d+$/.test(path) && m === "GET") return data.workspaces.find((w) => "/workspaces/" + w.id === path);
      if (/^\/workspaces\/\d+$/.test(path)) return {};
      if (/\/members$/.test(path) && m === "GET") return { items: data.members, total: data.members.length };
      if (/\/projects$/.test(path) && m === "GET") return { items: data.projects, total: data.projects.length };
      if (path.startsWith("/role-bindings?") && m === "GET") {
        const q = new URLSearchParams(path.split("?")[1]);
        const items = data.bindings.filter((b) => b.scope === q.get("scope") && (!q.get("scope_value") || String(b.scope_value) === q.get("scope_value")));
        return { items, total: items.length };
      }
      if (m !== "GET") return {};
      throw new Error("unexpected " + path);
    }
  });
  const toasts = [];
  h.window.XS.toast = (msg) => toasts.push(msg);
  return { ...h, toasts };
}
async function mount(h) { await h.window.XSWorkspace.render(h.el); await tick(); }
async function openDetail(h, id = "1") { await h.emit("click", h.el.querySelector('[data-wsp-row="' + id + '"]').querySelector('[data-wsp-click="open"]')); await tick(); }
const calls = (h, method, path) => h.calls.filter((c) => (c.options.method || "GET") === method && (path instanceof RegExp ? path.test(c.path) : c.path === path));

test("uses existing reference endpoints and the workspace contract paths", () => {
  const src = read("../../public/xs-workspace.js");
  const users = read("../../server/internal/users/users.go"), groups = read("../../server/internal/users/groups.go");
  const rbac = read("../../server/internal/rbac/handler.go"), cl = read("../../server/internal/modules/cluster/module.go");
  assert.ok(users.includes('r.GET("/users"')); assert.ok(groups.includes('r.GET("/user-groups"'));
  assert.ok(rbac.includes('r.GET("/roles"')); assert.ok(cl.includes('g.GET("/:id/namespaces"'));
  for (const p of ['"/users"', '"/user-groups"', '"/roles"', '"/clusters"', '"/namespaces"', '"/workspaces"', '"/members"', '"/projects"', '"/role-bindings"']) assert.ok(src.includes(p), p);
  assert.ok(src.includes('can("workspace:manage")'));
});

test("list shows name, description, manager, counts and quota; escapes input", async () => {
  const h = setup();
  await mount(h);
  const t = text(h.el);
  for (const s of ["支付中心", "数据平台", "Alice（alice）", "CPU 8 核 · 内存 16 GiB", "不限制", "共 2 个", "新建企业空间"]) assert.ok(t.includes(s), s);
  assert.ok(!h.el.innerHTML.includes("<img src=x"));
  assert.ok(h.el.innerHTML.includes("&lt;img src=x"));
  assert.ok(!/\/workspaces|api\/v1|owner|viewer|namespace/i.test(t), "no developer jargon");
  await h.emit("input", "[data-wsp-search]", "数据");
  assert.ok(!text(h.el).includes("支付中心"));
  assert.ok(text(h.el).includes("数据平台"));
});

test("read-only users see no write actions anywhere", async () => {
  const h = setup({ perms: ["workspace:read"] });
  await mount(h);
  for (const a of ["new", "edit", "del"]) assert.equal(h.el.querySelector(`[data-wsp-click="${a}"]`), null, a);
  await openDetail(h);
  assert.equal(h.el.querySelector('[data-wsp-click="addMember"]'), null);
  assert.equal(h.el.querySelector("[data-wsp-role]"), null);
  assert.ok(text(h.el).includes("所有者") && text(h.el).includes("只读"));
  await h.emit("click", '[data-tab="projects"]');
  assert.equal(h.el.querySelector('[data-wsp-click="addProject"]'), null);
  assert.equal(h.el.querySelector('[data-wsp-click="rmProject"]'), null);
  await h.emit("click", '[data-tab="bindings"]');
  assert.equal(h.el.querySelector('[data-wsp-click="addBinding"]'), null);
  assert.equal(h.el.querySelector('[data-wsp-click="rmBinding"]'), null);
});

test("create validates inline, then posts converted quota", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", '[data-wsp-click="new"]');
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="name"]')).includes("请输入空间名称"));
  h.el.querySelector('[data-wsf="name"]').value = "支付中心";
  h.el.querySelector('[data-wsf="cpu"]').value = "-1";
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="name"]')).includes("已存在同名"));
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="cpu"]')).includes("不小于 0"));
  assert.equal(calls(h, "POST", "/workspaces").length, 0);
  h.el.querySelector('[data-wsf="name"]').value = " 交易中台 ";
  h.el.querySelector('[data-wsf="cpu"]').value = "2.5";
  h.el.querySelector('[data-wsf="memory"]').value = "4";
  h.el.querySelector('[data-wsf="manager_id"]').value = "3";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  const post = calls(h, "POST", "/workspaces");
  assert.equal(post.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(post[0].options.body)), { name: "交易中台", description: "", manager_id: 3, cpu_limit_milli: 2500, memory_limit_bytes: 4 * GIB });
  assert.equal(h.el.querySelector("[data-wsp-mask]"), null);
  assert.ok(h.toasts.includes("已创建企业空间 交易中台"));
});

test("server 409 on save is shown next to the name field", async () => {
  const h = setup({ extra: async (p, o) => { if (p === "/workspaces/2" && o.method === "PUT") throw err(409, "企业空间名称已存在"); } });
  await mount(h);
  await h.emit("click", h.el.querySelector('[data-wsp-row="2"]').querySelector('[data-wsp-click="edit"]'));
  assert.equal(h.el.querySelector('[data-wsf="name"]').value, "数据平台");
  h.el.querySelector('[data-wsf="name"]').value = "其他";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="name"]')).includes("企业空间名称已存在"));
  assert.ok(h.el.querySelector("[data-wsp-mask]"));
});

test("delete requires typing the exact workspace name and sends confirm_name", async () => {
  const h = setup();
  await mount(h);
  await h.emit("click", h.el.querySelector('[data-wsp-row="1"]').querySelector('[data-wsp-click="del"]'));
  assert.ok(text(h.el).includes("不可撤销"));
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="confirm"]')).includes("请输入空间名称"));
  h.el.querySelector('[data-wsf="confirm"]').value = "支付";
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="confirm"]')).includes("不一致"));
  assert.equal(calls(h, "DELETE", /^\/workspaces\//).length, 0);
  h.el.querySelector('[data-wsf="confirm"]').value = "支付中心";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  const del = calls(h, "DELETE", "/workspaces/1");
  assert.equal(del.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(del[0].options.body)), { confirm_name: "支付中心" });
});

test("members: list with Chinese roles, add, change role, remove with confirmation", async () => {
  const h = setup();
  await mount(h);
  await openDetail(h);
  const t = text(h.el);
  assert.ok(t.includes("Alice（alice）") && t.includes("数据库组") && t.includes("成员数"));
  const sel = h.el.querySelector('[data-wsp-role="101"]');
  assert.deepEqual(sel.querySelectorAll("option").map((o) => o.textContent), ["所有者", "管理员", "成员", "只读"]);
  assert.equal(sel.value, "owner");

  await h.emit("change", '[data-wsp-role="102"]', "member"); await tick();
  const put = calls(h, "PUT", "/workspaces/1/members/102");
  assert.equal(put.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(put[0].options.body)), { role: "member" });

  await h.emit("click", '[data-wsp-click="addMember"]');
  h.el.querySelector('[data-wsf="subject"]').value = "2";
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="subject"]')).includes("已是本空间成员"));
  await h.emit("change", '[data-wsf="kind"]', "group");
  assert.ok(text(h.el.querySelector('[data-wsf="subject"]')).includes("数据库组"));
  await h.emit("change", '[data-wsf="kind"]', "user");
  h.el.querySelector('[data-wsf="subject"]').value = "3";
  h.el.querySelector('[data-wsf="role"]').value = "admin";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  const post = calls(h, "POST", "/workspaces/1/members");
  assert.deepEqual(JSON.parse(JSON.stringify(post[0].options.body)), { role: "admin", user_id: 3 });

  await h.emit("click", h.el.querySelector('[data-wsp-member="101"]').querySelector('[data-wsp-click="rmMember"]'));
  assert.ok(text(h.el.querySelector("[data-wsp-mask]")).includes("移出企业空间"));
  assert.equal(calls(h, "DELETE", /members/).length, 0);
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  assert.equal(calls(h, "DELETE", "/workspaces/1/members/101").length, 1);
});

test("role change failure reverts and reports in Chinese", async () => {
  const h = setup({ extra: async (p, o) => { if (o.method === "PUT") throw err(409, "空间至少需要保留一名所有者"); } });
  await mount(h);
  await openDetail(h);
  await h.emit("change", '[data-wsp-role="101"]', "viewer"); await tick();
  assert.ok(h.toasts.includes("修改角色失败：空间至少需要保留一名所有者"));
  assert.equal(h.el.querySelector('[data-wsp-role="101"]').value, "owner");
});

test("projects: cluster then namespace select, bound namespaces disabled, 409 inline, unbind confirm", async () => {
  let fail = true;
  const h = setup({ extra: async (p, o) => { if (p === "/workspaces/1/projects" && o.method === "POST" && fail) { fail = false; throw err(409, "该命名空间已归属其他企业空间"); } } });
  await mount(h);
  await openDetail(h);
  await h.emit("click", '[data-tab="projects"]');
  const t = text(h.el);
  assert.ok(t.includes("prod-a") && t.includes("pay") && t.includes("集群不存在"));
  await h.emit("click", '[data-wsp-click="addProject"]');
  assert.ok(h.el.querySelector('[data-wsf="namespace"]').disabled);
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="cluster_id"]')).includes("请选择集群"));
  await h.emit("change", '[data-wsf="cluster_id"]', "7"); await tick();
  assert.ok(calls(h, "GET", "/clusters/7/namespaces").length === 1);
  const ns = h.el.querySelector('[data-wsf="namespace"]');
  const opt = ns.querySelectorAll("option");
  assert.ok(opt.find((o) => o.getAttribute("value") === "pay").disabled, "already bound namespace disabled");
  ns.value = "web";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="namespace"]')).includes("已归属其他企业空间"));
  h.el.querySelector('[data-wsf="namespace"]').value = "web";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  const posts = calls(h, "POST", "/workspaces/1/projects");
  assert.deepEqual(JSON.parse(JSON.stringify(posts.at(-1).options.body)), { cluster_id: 7, namespace: "web" });
  assert.equal(h.el.querySelector("[data-wsp-mask]"), null);

  await h.emit("click", h.el.querySelector('[data-wsp-project="201"]').querySelector('[data-wsp-click="rmProject"]'));
  assert.ok(text(h.el.querySelector("[data-wsp-mask]")).includes("不会被删除"));
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  assert.equal(calls(h, "DELETE", "/workspaces/1/projects/201").length, 1);
});

test("role bindings: only this workspace's scoped bindings; add workspace and project scope; delete confirm", async () => {
  const h = setup();
  await mount(h);
  await openDetail(h);
  await h.emit("click", '[data-tab="bindings"]');
  assert.ok(h.el.querySelector('[data-wsp-binding="301"]'));
  assert.ok(h.el.querySelector('[data-wsp-binding="302"]'));
  assert.equal(h.el.querySelector('[data-wsp-binding="303"]'), null, "other workspace hidden");
  assert.equal(h.el.querySelector('[data-wsp-binding="304"]'), null, "global binding hidden");
  const t = text(h.el);
  assert.ok(t.includes("本空间") && t.includes("项目 prod-a / pay") && t.includes("运维工程师"));

  await h.emit("click", '[data-wsp-click="addBinding"]');
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="role_id"]')).includes("请选择角色"));
  h.el.querySelector('[data-wsf="role_id"]').value = "22";
  h.el.querySelector('[data-wsf="subject"]').value = "2";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  let post = calls(h, "POST", "/role-bindings");
  assert.deepEqual(JSON.parse(JSON.stringify(post[0].options.body)), { role_id: 22, scope: "workspace", scope_value: "1", user_id: 2 });

  await h.emit("click", '[data-wsp-click="addBinding"]');
  h.el.querySelector('[data-wsf="role_id"]').value = "21";
  await h.emit("change", '[data-wsf="kind"]', "group");
  h.el.querySelector('[data-wsf="subject"]').value = "11";
  await h.emit("change", '[data-wsf="scope"]', "project");
  assert.equal(h.el.querySelector('[data-wsf="role_id"]').value, "21", "input kept across repaint");
  await h.emit("click", '[data-wsp-click="submit"]');
  assert.ok(text(h.el.querySelector('[data-wsp-ferr="project"]')).includes("请选择项目"));
  h.el.querySelector('[data-wsf="project"]').value = "7/pay";
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  post = calls(h, "POST", "/role-bindings");
  assert.deepEqual(JSON.parse(JSON.stringify(post[1].options.body)), { role_id: 21, scope: "project", scope_value: "7/pay", group_id: 11 });

  await h.emit("click", h.el.querySelector('[data-wsp-binding="301"]').querySelector('[data-wsp-click="rmBinding"]'));
  assert.equal(calls(h, "DELETE", /role-bindings/).length, 0);
  await h.emit("click", '[data-wsp-click="submit"]'); await tick();
  assert.equal(calls(h, "DELETE", "/role-bindings/301").length, 1);
});

test("load failure and forbidden reference lists degrade gracefully in Chinese", async () => {
  const h = setup({ extra: async (p) => { if (p === "/workspaces") throw err(403); } });
  await mount(h);
  assert.ok(text(h.el.querySelector("[data-wsp-error]")).includes("没有权限"));

  const h2 = setup({ extra: async (p) => { if (p === "/users" || p === "/roles") throw err(403); } });
  await mount(h2);
  assert.ok(text(h2.el).includes("用户 #2"), "manager falls back to id");
  await openDetail(h2);
  await h2.emit("click", '[data-wsp-click="addMember"]');
  assert.ok(text(h2.el.querySelector('[data-wsp-ferr="subject"]')).includes("用户列表加载失败"));
});

test("validate helper and binding scope filter", () => {
  const h = setup();
  const W = h.window.XSWorkspace;
  assert.deepEqual(Object.keys(W.validate({ name: "a".repeat(65) }, [], null).errors), ["name"]);
  assert.equal(W.validate({ name: "x", cpu: "", memory: "0" }, [], null).body.cpu_limit_milli, 0);
  assert.deepEqual(Object.keys(W.validate({ name: "X", cpu: "abc" }, [{ id: 5, name: "x" }], 5).errors), ["cpu"]);
  assert.equal(W.bindingInWorkspace({ scope: "project", scope_value: "7/pay" }, 1, [{ cluster_id: 7, namespace: "pay" }]), true);
  assert.equal(W.bindingInWorkspace({ scope: "cluster", scope_value: "7" }, 1, []), false);
  assert.equal(W.fmtQuota({ cpu_limit_milli: 500, memory_limit_bytes: 512 * 1024 * 1024 }), "CPU 0.5 核 · 内存 512 MiB");
});

test("demo mode shows a notice and makes no requests", async () => {
  const h = setup();
  h.window.XS.mode = "demo";
  await mount(h);
  assert.ok(h.el.querySelector("[data-wsp-demo]"));
  assert.equal(h.calls.length, 0);
});
