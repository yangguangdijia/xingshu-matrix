# 星枢 Matrix 核心 API 参考

本文覆盖核心平台（`server/internal/{auth,rbac,users,settings,audit,approval,ai}` 与 `server/internal/server`）的全部 HTTP 接口。业务模块（集群、主机、堡垒机等）的接口见各自文档。

> 示例说明：所有响应示例均为对运行中的服务（PostgreSQL 后端）实际 `curl` 得到的结果；令牌、密码已替换为 `<redacted>`，数组截取前 1–2 项，超长字符串截断。时间字段为 RFC 3339 UTC（如 `2026-10-17T04:01:32.011689Z`）。示例中的 `doc_demo` 用户/角色为编写文档时创建的演示数据。AI 示例中的上游模型是一个 OpenAI 兼容的本地 mock，`data:` 块内容即上游原样转发的数据。

## 1. 通用约定

### 1.1 基础路径与认证

- 基础路径：`/api/v1`（`/healthz`、`/metrics` 除外，挂在根路径）。
- 除 `POST /auth/login`、`POST /auth/refresh`、`GET /health`、`GET /version` 外，所有接口都需要认证：
  - 请求头 `Authorization: Bearer <access_token>`；
  - 无法设置请求头的客户端（EventSource / WebSocket）可用查询参数 `?access_token=<access_token>`。
- 认证后还要经过**改密闸门**：用户 `must_change_password=true`（首次登录或被管理员重置密码）时，只允许访问 `GET /auth/me`、`PUT /auth/password`、`POST /auth/logout`、`GET /auth/sessions`、`GET /settings/public`，其余接口返回 HTTP 403 / `40301`。
- 权限：下文"权限"列为路由上 `RequirePerm` 要求的权限码（`资源:动作`）；`*` 表示拥有全部权限，`*:read` 之类通配由角色授予。"登录即可"表示只需有效令牌。
- 请求 ID：每个响应带 `X-Request-ID` 头，同值出现在响应体 `request_id`；客户端可自带合法的 `X-Request-ID` 透传。
- 请求体均为 `application/json`。JSON 绑定失败统一返回 400 / `40001`，`data.details` 为校验信息。

### 1.2 统一响应结构

```json
{ "code": 0, "message": "ok", "data": { }, "request_id": "065451660a812007e8d6e4be" }
```

- 成功：HTTP 200（`message: "ok"`）或 201（创建，`message: "created"`），`code=0`。
- 失败：HTTP 状态与 `code` 前三位一致，`data` 为 `null`，或在有附加信息时为 `{"details": ...}`。

失败示例（权限不足，`GET /settings` 由无 `setting:read` 的用户调用）：

```json
{
  "code": 40300,
  "message": "permission denied: setting:read",
  "data": {
    "details": {
      "required": "setting:read"
    }
  },
  "request_id": "3ce1befec9b4f9a1928fb629"
}
```

### 1.3 错误码表（`server/internal/httpx/response.go`）

| code | HTTP | 常量 | 含义 |
|---|---|---|---|
| 0 | 200/201 | CodeOK | 成功 |
| 40000 | 400 | CodeBadRequest | 请求错误 |
| 40001 | 400 | CodeValidation | 参数校验失败（含 JSON 绑定失败） |
| 40100 | 401 | CodeUnauthorized | 未认证 / 令牌无效 / 会话失效 |
| 40101 | 401 | CodeTokenExpired | 令牌过期 |
| 40102 | 401 | CodeInvalidCredentials | 用户名或密码错误 |
| 40300 | 403 | CodeForbidden | 无权限 |
| 40301 | 403 | CodePasswordChangeRequired | 需先修改密码 |
| 40302 | 403 | CodeAccountLocked | 账号已锁定 |
| 40303 | 403 | CodeAccountDisabled | 账号已禁用 |
| 40304 | **202** | CodeApprovalRequired | 变更需审批，已创建审批单（见 1.5） |
| 40400 | 404 | CodeNotFound | 资源 / 路由不存在 |
| 40900 | 409 | CodeConflict | 冲突（重复、状态不允许） |
| 42900 | 429 | CodeTooManyRequests | 请求过于频繁（登录限流，带 `Retry-After`） |
| 50000 | 500 | CodeInternal | 服务器内部错误 |
| 50200 | 502/504 | CodeUpstream | 上游（如 AI 模型接口）错误或超时 |
| 50300 | 503 | CodeUnavailable | 服务不可用 |

### 1.4 分页

列表接口（`/users`、`/audit-logs`、`/approvals`）使用查询参数 `page`（从 1 开始，默认 1）与 `page_size`（默认 20，最大 200，别名 `size`）。返回：

```json
{ "items": [ ], "total": 10, "page": 1, "page_size": 20 }
```

其余列表（角色、权限、设置、模型、提示词等）直接返回数组，不分页。

### 1.5 需要审批的变更（HTTP 202 / 40304）

当业务模块对某环境执行写操作、且该环境的环境策略 `require_approval=true`（如 `prod`）时，接口不会立即执行，而是创建审批单并返回 **HTTP 202**（格式取自 `approval.RespondPending`，核心模块自身不产生此响应，故此处为结构说明而非抓包）：

```json
{
  "code": 40304,
  "message": "该环境的变更需要审批，已创建审批单",
  "data": { "approval_required": true, "ticket": { "id": 12, "type": "...", "status": "pending", "...": "..." } },
  "request_id": "..."
}
```

客户端应提示用户并跳转到审批单；审批通过后由平台异步执行，审批单状态变为 `executed` / `failed`。

---

## 2. 认证

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/auth/login` | 公开（按 IP 限流） | 登录 |
| POST | `/auth/refresh` | 公开 | 用 refresh token 换新令牌 |
| POST | `/auth/logout` | 登录即可 | 注销当前会话 |
| GET | `/auth/me` | 登录即可 | 当前用户、角色、权限、环境范围 |
| PUT | `/auth/password` | 登录即可 | 修改自己的密码 |
| GET | `/auth/sessions` | 登录即可 | 我的会话列表 |
| DELETE | `/auth/sessions/:sid` | 登录即可 | 撤销自己的某个会话 |

### POST /auth/login

请求：`{"username": "string(必填,≤64)", "password": "string(必填,≤128)"}`

响应：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "token": {
      "access_token": "<redacted>",
      "refresh_token": "<redacted>",
      "token_type": "Bearer",
      "expires_in": 899,
      "expires_at": "2026-10-17T04:16:33.182121648Z",
      "must_change_password": true
    },
    "user": {
      "display_name": "文档演示用户",
      "id": 3,
      "must_change_password": true,
      "username": "doc_demo"
    }
  },
  "request_id": "3f90269d1d59358f920478d1"
}
```

密码错误（`data.details.remaining_attempts` 为锁定前剩余次数；超过次数后返回 `40302`）：

```json
{
  "code": 40102,
  "message": "用户名或密码错误",
  "data": {
    "details": {
      "remaining_attempts": 4
    }
  },
  "request_id": "773771ab5fb5bc6ca5b9b801"
}
```

`must_change_password=true` 时，除改密闸门白名单外的接口都会返回：

```json
{
  "code": 40301,
  "message": "首次登录或密码已被重置，请先修改密码",
  "data": null,
  "request_id": "14f2c0454a44fba0700c5719"
}
```

### POST /auth/refresh

请求：`{"refresh_token": "string(必填)"}`。响应 `data` 为新的令牌对（结构同登录响应的 `data.token`）：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "access_token": "<redacted>",
    "refresh_token": "<redacted>",
    "token_type": "Bearer",
    "expires_in": 899,
    "expires_at": "2026-10-17T04:16:33.773022135Z",
    "must_change_password": false
  },
  "request_id": "5f8d837c179daac10b9a31a3"
}
```

### POST /auth/logout

无请求体。注销后该会话的 access token 立即失效。

```json
{
  "code": 0,
  "message": "ok",
  "data": null,
  "request_id": "6ab63fcfecd2e6153a1de66b"
}
```

注销后再用原令牌访问：

```json
{
  "code": 40100,
  "message": "会话已失效，请重新登录",
  "data": null,
  "request_id": "e49ec307b37994f1e1f97a6a"
}
```

### GET /auth/me

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "user_id": 1,
    "username": "admin",
    "display_name": "系统管理员",
    "roles": [
      "admin"
    ],
    "permissions": [
      "*"
    ],
    "env_scope": null,
    "must_change_password": false
  },
  "request_id": "1b8793589d6a3ce1799b5fc6"
}
```

`permissions` 含 `*` 表示超级管理员；`env_scope` 为 `null`/空表示不限环境。

### PUT /auth/password

请求：`{"old_password": "string(必填,≤128)", "new_password": "string(必填,≤128)"}`

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "must_change_password": false
  },
  "request_id": "297cfc8d7e8869a0827af16a"
}
```

### GET /auth/sessions

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "id": "tEjnyz2qL_-24iffCh_3kgit",
      "user_id": 1,
      "user_agent": "curl/8.14.1",
      "ip": "::1",
      "created_at": "2026-10-17T03:59:55.932455Z",
      "last_used_at": "2026-10-17T03:59:55.932455Z",
      "expires_at": "2026-10-24T03:59:55.64798Z",
      "current": true
    },
    {
      "id": "tnZkn7RPR5caCcFg-wn560a6",
      "user_id": 1,
      "user_agent": "curl/8.14.1",
      "ip": "::1",
      "created_at": "2026-10-17T03:59:55.0081Z",
      "last_used_at": "2026-10-17T03:59:55.0081Z",
      "expires_at": "2026-10-24T03:59:54.714064Z",
      "current": false
    }
  ],
  "request_id": "4811b0a5443cae5d2362daab"
}
```

### DELETE /auth/sessions/:sid

只能撤销自己的会话。

```json
{
  "code": 0,
  "message": "ok",
  "data": null,
  "request_id": "f574fe08fea7be0de45d8c32"
}
```

未携带令牌 / 令牌无效：

```json
{
  "code": 40100,
  "message": "authentication required",
  "data": null,
  "request_id": "729928579f79ca4e1bc52e3f"
}
```

---

## 3. 用户

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/users` | `user:read` | 分页列表；筛选 `q`（关键字）、`status`（active/disabled）、`role_id` |
| GET | `/users/:id` | `user:read` | 详情 |
| POST | `/users` | `user:manage` | 创建 |
| PUT | `/users/:id` | `user:manage` | 修改资料（可选同时改角色） |
| PUT | `/users/:id/roles` | `user:manage` | 设置角色 |
| PUT | `/users/:id/status` | `user:manage` | 启用 / 禁用 |
| POST | `/users/:id/reset-password` | `user:manage` | 重置密码 |
| POST | `/users/:id/unlock` | `user:manage` | 解除登录锁定 |
| DELETE | `/users/:id` | `user:manage` | 删除（不能删自己、内置用户，且须保留至少一个管理员） |

### GET /users

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "items": [
      {
        "id": 1,
        "username": "admin",
        "display_name": "系统管理员",
        "email": "",
        "phone": "",
        "status": "active",
        "is_builtin": true,
        "must_change_password": false,
        "locked": false,
        "locked_until": null,
        "last_login_at": "2026-10-17T03:59:55.932055Z",
        "last_login_ip": "::1",
        "password_changed_at": "2026-10-17T03:59:55.635727Z",
        "roles": [
          {
            "id": 1,
            "code": "admin",
            "name": "超级管理员"
          }
        ],
        "created_at": "2026-10-17T03:59:34.290235Z",
        "updated_at": "2026-10-17T03:59:55.635727Z"
      },
      {
        "id": 2,
        "username": "dev01",
        "display_name": "开发一",
        "email": "dev01@x.com",
        "phone": "",
        "status": "active",
        "is_builtin": false,
        "must_change_password": true,
        "locked": false,
        "locked_until": null,
        "last_login_at": null,
        "last_login_ip": "",
        "password_changed_at": null,
        "roles": [],
        "created_at": "2026-10-17T03:59:56.432558Z",
        "updated_at": "2026-10-17T03:59:56.432558Z"
      }
    ],
    "total": 3,
    "page": 1,
    "page_size": 2
  },
  "request_id": "95a728f60115495e0b74b0f4"
}
```

### GET /users/:id

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 3,
    "username": "doc_demo",
    "display_name": "文档演示",
    "email": "doc_demo@example.com",
    "phone": "",
    "status": "active",
    "is_builtin": false,
    "must_change_password": true,
    "locked": false,
    "locked_until": null,
    "last_login_at": null,
    "last_login_ip": "",
    "password_changed_at": null,
    "roles": [
      {
        "id": 6,
        "code": "doc_demo",
        "name": "文档演示角色"
      }
    ],
    "created_at": "2026-10-17T04:01:32.305392Z",
    "updated_at": "2026-10-17T04:01:32.305392Z"
  },
  "request_id": "7e4663e5c0dd3f5a1873e389"
}
```

### POST /users

请求（`CreateInput`）：

```json
{
  "username": "string(必填)",
  "display_name": "string(≤64)",
  "email": "string(≤128，需为合法邮箱)",
  "phone": "string(≤32)",
  "password": "string(≤72，留空则自动生成)",
  "role_ids": [6],
  "must_change_password": true
}
```

响应（自动生成密码时返回一次性 `initial_password`）：

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "initial_password": "<redacted>",
    "user": {
      "id": 3,
      "username": "doc_demo",
      "display_name": "文档演示",
      "email": "doc_demo@example.com",
      "phone": "",
      "status": "active",
      "is_builtin": false,
      "must_change_password": true,
      "locked": false,
      "locked_until": null,
      "last_login_at": null,
      "last_login_ip": "",
      "password_changed_at": null,
      "roles": [
        {
          "id": 6,
          "code": "doc_demo",
          "name": "文档演示角色"
        }
      ],
      "created_at": "2026-10-17T04:01:32.305392Z",
      "updated_at": "2026-10-17T04:01:32.305392Z"
    }
  },
  "request_id": "bc5f4e982b549a04fa528fa9"
}
```

缺少必填字段：

```json
{
  "code": 40001,
  "message": "invalid request body",
  "data": {
    "details": "Key: 'CreateInput.Username' Error:Field validation for 'Username' failed on the 'required' tag"
  },
  "request_id": "ed7d2a4a76087c934c1dfb0e"
}
```

### PUT /users/:id

请求（`UpdateInput`）：`{"display_name": "...", "email": "...", "phone": "...", "role_ids": [6]}`；`role_ids` 省略或为 `null` 时不改角色。响应为用户对象：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 3,
    "username": "doc_demo",
    "display_name": "文档演示用户",
    "email": "doc_demo@example.com",
    "phone": "13800000000",
    "status": "active",
    "is_builtin": false,
    "must_change_password": true,
    "locked": false,
    "locked_until": null,
    "last_login_at": null,
    "last_login_ip": "",
    "password_changed_at": null,
    "roles": [
      {
        "id": 6,
        "code": "doc_demo",
        "name": "文档演示角色"
      }
    ],
    "created_at": "2026-10-17T04:01:32.305392Z",
    "updated_at": "2026-10-17T04:01:32.310446Z"
  },
  "request_id": "04b37ea62bf6e2d2e309ed21"
}
```

### PUT /users/:id/roles

请求：`{"role_ids": [6]}`（空数组清空角色）。响应为用户对象（同上）。

### PUT /users/:id/status

请求：`{"status": "active" | "disabled"}`（必填；不能禁用自己）。响应为用户对象，`status` 已更新：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 3,
    "username": "doc_demo",
    "display_name": "文档演示用户",
    "email": "doc_demo@example.com",
    "phone": "13800000000",
    "status": "disabled",
    "is_builtin": false,
    "must_change_password": true,
    "locked": false,
    "locked_until": null,
    "last_login_at": null,
    "last_login_ip": "",
    "password_changed_at": null,
    "roles": [
      {
        "id": 6,
        "code": "doc_demo",
        "name": "文档演示角色"
      }
    ],
    "created_at": "2026-10-17T04:01:32.305392Z",
    "updated_at": "2026-10-17T04:01:32.314913Z"
  },
  "request_id": "d18d7c9b73b70d0eb7d279ca"
}
```

### POST /users/:id/reset-password

请求体可省略；可选 `{"password": "string(≤72)"}`。指定密码时响应不回显密码；未指定则自动生成并在 `data.password` 中返回一次。重置后用户必须改密：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "must_change_password": true
  },
  "request_id": "ee3e1c4f91675a976f13e4ab"
}
```

### POST /users/:id/unlock

无请求体，响应为用户对象：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 3,
    "username": "doc_demo",
    "display_name": "文档演示用户",
    "email": "doc_demo@example.com",
    "phone": "13800000000",
    "status": "active",
    "is_builtin": false,
    "must_change_password": true,
    "locked": false,
    "locked_until": null,
    "last_login_at": null,
    "last_login_ip": "",
    "password_changed_at": "2026-10-17T04:01:32.600415Z",
    "roles": [
      {
        "id": 6,
        "code": "doc_demo",
        "name": "文档演示角色"
      }
    ],
    "created_at": "2026-10-17T04:01:32.305392Z",
    "updated_at": "2026-10-17T04:01:32.601883Z"
  },
  "request_id": "288932bea48859aa918424cf"
}
```

### DELETE /users/:id

成功返回 `{"id": <id>}`。错误示例：

```json
{
  "code": 40300,
  "message": "不能删除自己",
  "data": null,
  "request_id": "918458f0b860eb0869206b1c"
}
```

```json
{
  "code": 40400,
  "message": "user not found",
  "data": null,
  "request_id": "6d1f553ccbc4ec0d7a52fe1b"
}
```

---

## 4. 角色与权限

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/permissions` | `role:read` | 权限目录（核心 + 已加载模块） |
| GET | `/roles` | `role:read` | 角色列表（含 `user_count`） |
| GET | `/roles/:id` | `role:read` | 角色详情 |
| POST | `/roles` | `role:manage` | 创建角色 |
| PUT | `/roles/:id` | `role:manage` | 修改角色 |
| DELETE | `/roles/:id` | `role:manage` | 删除角色（内置角色不可删） |

### GET /permissions

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "code": "ai:chat",
      "resource": "ai",
      "action": "chat",
      "module": "core",
      "description": "使用 AI 助手"
    },
    {
      "code": "ai:manage",
      "resource": "ai",
      "action": "manage",
      "module": "core",
      "description": "管理 AI 模型与提示词模板"
    }
  ],
  "request_id": "2719b7851ccad05f48fb91e9"
}
```

### GET /roles

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "id": 1,
      "code": "admin",
      "name": "超级管理员",
      "description": "拥有全部权限",
      "builtin": true,
      "env_scope": [],
      "permissions": [
        "*"
      ],
      "user_count": 1,
      "created_at": "2026-10-17T03:59:33.918269Z",
      "updated_at": "2026-10-17T03:59:33.918269Z"
    },
    {
      "id": 2,
      "code": "operator",
      "name": "运维工程师",
      "description": "读取全部资源，执行非用户/角色管理类操作",
      "builtin": true,
      "env_scope": [],
      "permissions": [
        "*:exec",
        "*:read"
      ],
      "user_count": 0,
      "created_at": "2026-10-17T03:59:33.918269Z",
      "updated_at": "2026-10-17T03:59:33.918269Z"
    }
  ],
  "request_id": "0b3a1d6761dc2f5ba456597e"
}
```

### GET /roles/:id

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 1,
    "code": "admin",
    "name": "超级管理员",
    "description": "拥有全部权限",
    "builtin": true,
    "env_scope": [],
    "permissions": [
      "*"
    ],
    "user_count": 1,
    "created_at": "2026-10-17T03:59:33.918269Z",
    "updated_at": "2026-10-17T03:59:33.918269Z"
  },
  "request_id": "4dc25cbe6ba84fcfc06edd35"
}
```

### POST /roles

请求（`RoleInput`）：

```json
{
  "code": "string(角色编码，唯一)",
  "name": "string(必填,≤64)",
  "description": "string(≤512)",
  "env_scope": ["dev", "test"],
  "permissions": ["approval:create", "ai:chat", "user:read"]
}
```

`env_scope` 为空表示不限环境；`permissions` 支持通配（`*`、`*:read`）。

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 6,
    "code": "doc_demo",
    "name": "文档演示角色",
    "description": "API 文档示例",
    "builtin": false,
    "env_scope": [
      "dev",
      "test"
    ],
    "permissions": [
      "ai:chat",
      "approval:create"
    ],
    "user_count": 0,
    "created_at": "2026-10-17T04:01:32.011689Z",
    "updated_at": "2026-10-17T04:01:32.011689Z"
  },
  "request_id": "163ad871f427e583615abba3"
}
```

编码重复：

```json
{
  "code": 40900,
  "message": "role code already exists",
  "data": null,
  "request_id": "b27828093faad96d0242676e"
}
```

### PUT /roles/:id

请求同 `RoleInput`，响应为角色对象：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 6,
    "code": "doc_demo",
    "name": "文档演示角色",
    "description": "API 文档示例（已更新）",
    "builtin": false,
    "env_scope": [
      "dev",
      "test"
    ],
    "permissions": [
      "ai:chat",
      "approval:create"
    ],
    "user_count": 0,
    "created_at": "2026-10-17T04:01:32.011689Z",
    "updated_at": "2026-10-17T04:01:32.013806Z"
  },
  "request_id": "ea8ad05fbe59a69130d8bb68"
}
```

### DELETE /roles/:id

成功：`{"code":0,"message":"ok","data":{"id":5},...}`。删除内置角色：

```json
{
  "code": 40300,
  "message": "built-in roles cannot be deleted",
  "data": null,
  "request_id": "d39104337652e1077e2d3ba8"
}
```

---

## 5. 环境策略

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/env-policies` | `role:read` | 各环境的变更策略 |
| PUT | `/env-policies/:env` | `role:manage` | 新建或更新某环境策略（upsert） |

### GET /env-policies

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "env": "dev",
      "name": "开发",
      "require_approval": false,
      "approver_permission": "approval:approve",
      "updated_at": "2026-10-17T03:59:33.918269Z"
    },
    {
      "env": "prod",
      "name": "生产",
      "require_approval": true,
      "approver_permission": "approval:approve",
      "updated_at": "2026-10-17T03:59:33.918269Z"
    }
  ],
  "request_id": "a7388956a5408c46805c6e40"
}
```

### PUT /env-policies/:env

请求：

```json
{ "name": "测试", "require_approval": false, "approver_permission": "approval:approve" }
```

`require_approval=true` 时，该环境上的模块写操作会走审批（见 1.5）；`approver_permission` 为可审批该环境审批单所需的权限。

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "env": "test",
    "name": "测试",
    "require_approval": false,
    "approver_permission": "approval:approve",
    "updated_at": "2026-10-17T04:01:32.020639Z"
  },
  "request_id": "c55b50d489ab75034b18f59d"
}
```

---

## 6. 系统设置

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/settings/public` | 登录即可（改密闸门放行） | `public.` 前缀的非密文设置，`key → value` 映射 |
| GET | `/settings` | `setting:read` | 列表，可用 `?prefix=` 过滤 |
| GET | `/settings/:key` | `setting:read` | 单项（密文显示为 `******`） |
| PUT | `/settings/:key` | `setting:manage` | 新建或更新 |
| DELETE | `/settings/:key` | `setting:manage` | 删除 |

### PUT /settings/:key

请求：

```json
{ "value": "任意 JSON(必填)", "secret": false, "description": "string(≤512)" }
```

`secret=true` 时值加密存储，读取时一律返回 `"******"`，且不写入审计详情。

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "key": "public.site_name",
    "value": "星枢 Matrix",
    "secret": false,
    "has_value": true,
    "description": "站点名称",
    "updated_by": 1,
    "updated_at": "2026-10-17T04:01:35.088956Z"
  },
  "request_id": "c2e0b508faaac248d5be88fc"
}
```

密文设置：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "key": "doc.demo_secret",
    "value": "******",
    "secret": true,
    "has_value": true,
    "description": "示例密钥",
    "updated_by": 1,
    "updated_at": "2026-10-17T04:01:35.090406Z"
  },
  "request_id": "de1a56016f0d3d2e91b37268"
}
```

### GET /settings

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "key": "doc.demo_secret",
      "value": "******",
      "secret": true,
      "has_value": true,
      "description": "示例密钥",
      "updated_by": 1,
      "updated_at": "2026-10-17T04:01:35.090406Z"
    },
    {
      "key": "public.site_name",
      "value": "星枢 Matrix",
      "secret": false,
      "has_value": true,
      "description": "站点名称",
      "updated_by": 1,
      "updated_at": "2026-10-17T04:01:35.088956Z"
    }
  ],
  "request_id": "c1cd0c9405ba604ff9858586"
}
```

### GET /settings/:key

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "key": "doc.demo_secret",
    "value": "******",
    "secret": true,
    "has_value": true,
    "description": "示例密钥",
    "updated_by": 1,
    "updated_at": "2026-10-17T04:01:35.090406Z"
  },
  "request_id": "ae88a78b0da93f5a2519a51f"
}
```

不存在：

```json
{
  "code": 40400,
  "message": "resource not found",
  "data": null,
  "request_id": "c9e651ebd708fad3079c407e"
}
```

### GET /settings/public

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "public.site_name": "星枢 Matrix"
  },
  "request_id": "57613471578d458c25b74911"
}
```

### DELETE /settings/:key

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "key": "doc.demo_secret"
  },
  "request_id": "603071ded0716985f6d8021b"
}
```

---

## 7. 审计日志

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/audit-logs` | `audit:read` | 分页查询 |
| GET | `/audit-logs/:id` | `audit:read` | 详情 |

`/api/v1` 下所有非读请求（POST/PUT/PATCH/DELETE，含登录、失败请求）由审计中间件自动记录，GET 不记录；`POST /ai/chat` 显式跳过（工具确认接口单独记录）。

查询参数：`username`、`user_id`、`resource`、`action`、`method`、`result`（`success`/`failure`）、`path`、`ip`、`q`（关键字）、`from`/`to`（RFC 3339 或 Unix 秒），以及 `page`/`page_size`。结果按时间倒序。

### GET /audit-logs

`GET /audit-logs?resource=approvals&page_size=1`：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "items": [
      {
        "id": 40,
        "user_id": 1,
        "username": "admin",
        "ip": "::1",
        "method": "POST",
        "path": "/api/v1/approvals/3/approve",
        "resource": "approvals",
        "resource_id": "3",
        "action": "approve",
        "status_code": 409,
        "result": "failure",
        "duration_ms": 0,
        "request_id": "a3dbfd0360f7b64c699c5347",
        "user_agent": "Python-urllib/3.12",
        "created_at": "2026-10-17T04:01:34.085365Z"
      }
    ],
    "total": 9,
    "page": 1,
    "page_size": 1
  },
  "request_id": "446c7cc7b5e703a56903a20e"
}
```

### GET /audit-logs/:id

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 40,
    "user_id": 1,
    "username": "admin",
    "ip": "::1",
    "method": "POST",
    "path": "/api/v1/approvals/3/approve",
    "resource": "approvals",
    "resource_id": "3",
    "action": "approve",
    "status_code": 409,
    "result": "failure",
    "duration_ms": 0,
    "request_id": "a3dbfd0360f7b64c699c5347",
    "user_agent": "Python-urllib/3.12",
    "created_at": "2026-10-17T04:01:34.085365Z"
  },
  "request_id": "8053fa539a872483ee3158a4"
}
```

---

## 8. 审批

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/approval-types` | 登录即可 | 已注册的审批类型 |
| GET | `/approvals` | 登录即可；`scope=all` 需 `approval:read` | 列表 |
| GET | `/approvals/:id` | 登录即可（申请人、有该单审批权限者或 `approval:read`，否则 404） | 详情 |
| POST | `/approvals` | 由审批类型的 `create_permission` 决定（`generic` 为 `approval:create`） | 发起审批 |
| POST | `/approvals/:id/approve` | 审批类型 / 环境策略的审批权限（默认 `approval:approve`） | 通过 |
| POST | `/approvals/:id/reject` | 同上 | 驳回 |
| POST | `/approvals/:id/cancel` | 申请人 | 撤回 |

状态：`pending` → `approved`（处理器执行中）→ `executed` / `failed`；或 `rejected`、`cancelled`。规则：不能审批自己发起的单；带 `env` 的单要求操作者的 `env_scope` 包含该环境；非 `pending` 状态不能再决策（409）。

### GET /approval-types

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "type": "generic",
      "module": "core",
      "description": "通用审批（无自动执行动作）",
      "create_permission": "approval:create",
      "approver_permission": ""
    }
  ],
  "request_id": "2bd29384200038f14c285ca2"
}
```

### GET /approvals

查询参数：`scope`（`mine` 我发起的，默认；`todo` 待我审批；`all` 全部，需 `approval:read`）、`status`、`type`、`env`、`page`、`page_size`。

`GET /approvals?scope=todo&page_size=1`：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "items": [
      {
        "id": 4,
        "type": "generic",
        "title": "撤回示例",
        "env": "dev",
        "reason": "",
        "payload": {},
        "status": "pending",
        "applicant_id": 3,
        "applicant_name": "doc_demo",
        "approver_id": null,
        "approver_name": "",
        "comment": "",
        "result": null,
        "error": "",
        "created_at": "2026-10-17T04:01:34.071931Z",
        "updated_at": "2026-10-17T04:01:34.071931Z",
        "decided_at": null,
        "executed_at": null
      }
    ],
    "total": 3,
    "page": 1,
    "page_size": 1
  },
  "request_id": "8c5c662cce00ae718c70febf"
}
```

无 `approval:read` 时使用 `scope=all`：

```json
{
  "code": 40300,
  "message": "permission denied: approval:read",
  "data": null,
  "request_id": "941f055a05647ead9ed251e4"
}
```

### POST /approvals

请求（`CreateInput`）：

```json
{
  "type": "generic(必填,≤64)",
  "title": "string(必填,≤256)",
  "env": "string(≤64，可选，须在申请人 env_scope 内)",
  "reason": "string(≤2000)",
  "payload": { "任意": "JSON" }
}
```

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 2,
    "type": "generic",
    "title": "开放 test 环境数据库只读账号",
    "env": "test",
    "reason": "排查问题",
    "payload": {
      "db": "orders"
    },
    "status": "pending",
    "applicant_id": 3,
    "applicant_name": "doc_demo",
    "approver_id": null,
    "approver_name": "",
    "comment": "",
    "result": null,
    "error": "",
    "created_at": "2026-10-17T04:01:34.070433Z",
    "updated_at": "2026-10-17T04:01:34.070433Z",
    "decided_at": null,
    "executed_at": null
  },
  "request_id": "bb924dae23e52265dea14a94"
}
```

### GET /approvals/:id

审批通过、处理器执行完成后：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 2,
    "type": "generic",
    "title": "开放 test 环境数据库只读账号",
    "env": "test",
    "reason": "排查问题",
    "payload": {
      "db": "orders"
    },
    "status": "executed",
    "applicant_id": 3,
    "applicant_name": "doc_demo",
    "approver_id": 1,
    "approver_name": "admin",
    "comment": "同意",
    "result": null,
    "error": "",
    "created_at": "2026-10-17T04:01:34.070433Z",
    "updated_at": "2026-10-17T04:01:34.077652Z",
    "decided_at": "2026-10-17T04:01:34.075799Z",
    "executed_at": "2026-10-17T04:01:34.077652Z"
  },
  "request_id": "85eac5fd0d25b7c2fafac372"
}
```

### POST /approvals/:id/approve · /reject · /cancel

请求体可省略；approve / reject 可带 `{"comment": "string(≤2000)"}`，cancel 忽略请求体。响应为更新后的审批单。

通过（随后异步执行，状态变为 `executed`/`failed`）：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 2,
    "type": "generic",
    "title": "开放 test 环境数据库只读账号",
    "env": "test",
    "reason": "排查问题",
    "payload": {
      "db": "orders"
    },
    "status": "approved",
    "applicant_id": 3,
    "applicant_name": "doc_demo",
    "approver_id": 1,
    "approver_name": "admin",
    "comment": "同意",
    "result": null,
    "error": "",
    "created_at": "2026-10-17T04:01:34.070433Z",
    "updated_at": "2026-10-17T04:01:34.075799Z",
    "decided_at": "2026-10-17T04:01:34.075799Z",
    "executed_at": null
  },
  "request_id": "ba54041ebce92c67329c8160"
}
```

驳回：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 3,
    "type": "generic",
    "title": "临时扩容",
    "env": "dev",
    "reason": "压测",
    "payload": {},
    "status": "rejected",
    "applicant_id": 3,
    "applicant_name": "doc_demo",
    "approver_id": 1,
    "approver_name": "admin",
    "comment": "压测窗口未确认",
    "result": null,
    "error": "",
    "created_at": "2026-10-17T04:01:34.07124Z",
    "updated_at": "2026-10-17T04:01:34.083045Z",
    "decided_at": "2026-10-17T04:01:34.083045Z",
    "executed_at": null
  },
  "request_id": "7799da36f30df20860886066"
}
```

对非 pending 单再次决策：

```json
{
  "code": 40900,
  "message": "ticket is not pending (status: rejected)",
  "data": null,
  "request_id": "a3dbfd0360f7b64c699c5347"
}
```

---

## 9. AI

所有 AI 接口位于 `/api/v1/ai` 下。

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/ai/models` | `ai:read` | 模型列表（含 base_url、脱敏 key） |
| GET | `/ai/models/options` | `ai:chat` | 精简的已启用模型列表（供聊天选择） |
| GET | `/ai/models/:id` | `ai:read` | 模型详情 |
| POST | `/ai/models` | `ai:manage` | 新增模型 |
| PUT | `/ai/models/:id` | `ai:manage` | 修改模型 |
| DELETE | `/ai/models/:id` | `ai:manage` | 删除模型 |
| POST | `/ai/models/:id/test` | `ai:manage` | 连通性测试（发送 ping） |
| GET | `/ai/prompts` | `ai:read` 或 `ai:chat` | 提示词模板列表，可 `?scene=` 过滤 |
| GET | `/ai/prompts/:id` | `ai:read` 或 `ai:chat` | 模板详情 |
| POST | `/ai/prompts` | `ai:manage` | 新增模板 |
| PUT | `/ai/prompts/:id` | `ai:manage` | 修改模板 |
| DELETE | `/ai/prompts/:id` | `ai:manage` | 删除模板 |
| GET | `/ai/tools` | `ai:chat` | 当前用户可用的工具 |
| POST | `/ai/tool-calls/:id/confirm` | `ai:chat` | 确认执行待确认的工具调用 |
| POST | `/ai/tool-calls/:id/reject` | `ai:chat` | 拒绝待确认的工具调用 |
| POST | `/ai/chat` | `ai:chat` | 对话（默认 SSE 流式） |

### 9.1 模型

请求（`ModelInput`，POST 与 PUT 相同）：

```json
{
  "name": "string(必填,≤64)",
  "provider": "string(≤64)",
  "base_url": "string(必填,≤512；可为 https://x、https://x/v1 或完整 .../chat/completions)",
  "api_key": "string|null（PUT 时 null/省略表示保留原 key）",
  "model": "string(必填,≤128)",
  "temperature": 0.3,
  "max_tokens": 1024,
  "extra_headers": { "X-Header": "value" },
  "is_default": true,
  "enabled": true
}
```

上游需兼容 OpenAI Chat Completions（流式）。API Key 加密存储，响应只返回 `api_key_masked` 与 `has_api_key`。

POST /ai/models：

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 1,
    "name": "文档演示模型",
    "provider": "openai",
    "base_url": "http://127.0.0.1:18999/v1",
    "api_key_masked": "sk-d****cdef",
    "has_api_key": true,
    "model": "mock-gpt",
    "temperature": 0.3,
    "max_tokens": 1024,
    "extra_headers": {},
    "is_default": true,
    "enabled": true,
    "created_at": "2026-10-17T04:01:35.095697Z",
    "updated_at": "2026-10-17T04:01:35.095697Z"
  },
  "request_id": "da5a06aa833ee331b62503ed"
}
```

GET /ai/models：

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "id": 1,
      "name": "文档演示模型",
      "provider": "openai",
      "base_url": "http://127.0.0.1:18999/v1",
      "api_key_masked": "sk-d****cdef",
      "has_api_key": true,
      "model": "mock-gpt",
      "temperature": 0.2,
      "max_tokens": 2048,
      "extra_headers": {},
      "is_default": true,
      "enabled": true,
      "created_at": "2026-10-17T04:01:35.095697Z",
      "updated_at": "2026-10-17T04:01:35.097588Z"
    }
  ],
  "request_id": "aa05ff9fb7c5abd2fcbeaa19"
}
```

GET /ai/models/options：

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "id": 1,
      "is_default": true,
      "model": "mock-gpt",
      "name": "文档演示模型"
    }
  ],
  "request_id": "8953029ba9a7d3342f67e2c5"
}
```

PUT /ai/models/:id、GET /ai/models/:id 返回模型对象（同上）。DELETE 返回：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 2
  },
  "request_id": "2f06ebe8bf4bcd0dd158ad77"
}
```

POST /ai/models/:id/test：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "ok": true,
    "reply": "pong"
  },
  "request_id": "f770cc2c55a2e66d2196f05f"
}
```

上游不可达：

```json
{
  "code": 50200,
  "message": "AI 模型接口调用失败",
  "data": null,
  "request_id": "404d5e50d6760d97c215d321"
}
```

### 9.2 提示词模板

请求（`PromptInput`）：

```json
{
  "scene": "string(必填,≤64)",
  "name": "string(必填,≤128)",
  "description": "string(≤512)",
  "content": "string(必填,≤65536；Go text/template，变量写作 {{.username}}，缺失变量渲染为空)",
  "is_default": true,
  "enabled": true
}
```

每个 `scene` 的默认模板会在聊天请求只传 `scene` 时作为 system prompt。

POST /ai/prompts：

```json
{
  "code": 0,
  "message": "created",
  "data": {
    "id": 1,
    "scene": "doc_demo",
    "name": "运维助手",
    "description": "示例模板",
    "content": "你是星枢 Matrix 的运维助手，当前用户：{{.username}}。",
    "is_default": true,
    "enabled": true,
    "created_at": "2026-10-17T04:01:35.103315Z",
    "updated_at": "2026-10-17T04:01:35.103315Z"
  },
  "request_id": "bd8f5b6202f95488b5499bd1"
}
```

GET /ai/prompts?scene=doc_demo：

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "id": 1,
      "scene": "doc_demo",
      "name": "运维助手",
      "description": "示例模板（已更新）",
      "content": "你是星枢 Matrix 的运维助手，当前用户：{{.username}}。回答保持简洁。",
      "is_default": true,
      "enabled": true,
      "created_at": "2026-10-17T04:01:35.103315Z",
      "updated_at": "2026-10-17T04:01:35.105127Z"
    }
  ],
  "request_id": "9609ab88d98cd8b1ed9e2627"
}
```

PUT 返回模板对象，DELETE 返回 `{"id": <id>}`：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 2
  },
  "request_id": "1279c89fdf288470748afb93"
}
```

### 9.3 工具

GET /ai/tools 只返回当前用户拥有对应 `permission` 的工具；`requires_confirmation=true` 的工具在对话中需用户确认后才执行。

```json
{
  "code": 0,
  "message": "ok",
  "data": [
    {
      "name": "search_audit_logs",
      "module": "core",
      "description": "按用户、资源、动作、结果和时间范围检索平台审计日志（只读）。",
      "parameters": {
        "type": "object",
        "properties": {
          "username": {
            "type": "string",
            "description": "操作人用户名"
          },
          "resource": {
            "type": "string",
            "description": "资源类型，如 user、cluster、pipeline"
          },
          "action": {
            "type": "string",
            "description": "动作，如 create、update、delete"
          },
          "since": {
            "type": "string",
            "description": "起始时间 RFC3339"
          },
          "until": {
            "type": "string",
            "description": "结束时间 RFC3339"
          },
          "limit": {
            "type": "integer",
            "minimum": 1,
            "maximum": 100
          }
        }
      },
      "permission": "audit:read",
      "requires_confirmation": false
    }
  ],
  "request_id": "58bef2270c7964e8a127e5b7"
}
```

### 9.4 对话 POST /ai/chat

请求（`ChatRequest`）：

```json
{
  "model_id": 1,
  "scene": "doc_demo",
  "template_id": 0,
  "variables": { "username": "admin" },
  "messages": [ { "role": "user", "content": "你好" } ],
  "tools": true,
  "stream": true,
  "temperature": 0.2,
  "max_tokens": 1024
}
```

- `messages`：必填，1–200 条，`role` ∈ `system|user|assistant|tool`；`tool` 消息必须带 `tool_call_id`；`content` 原样透传（支持多模态数组）。
- `model_id`：省略/0 时使用默认模型。
- system prompt：`template_id` > 0 时用该模板（禁用则 400），否则 `scene` 非空时用该场景默认模板；均用 `variables` 渲染。
- `tools`：`true` 提供该用户可用的全部工具；字符串数组则限定为指定工具名；省略/`false` 不用工具。
- `stream`：省略或 `true` 为 SSE；`false` 返回一次性 JSON。
- `temperature` / `max_tokens`：覆盖模型默认值。
- 工具调用最多进行 `MaxToolRounds` 轮，超出时发出 `error` 事件。

#### SSE 事件格式（`server/internal/ai/chat.go`、`http.go`）

响应头：`Content-Type: text/event-stream; charset=utf-8`、`Cache-Control: no-cache`、`X-Accel-Buffering: no`。流中有两类帧：

| 帧 | 格式 | 说明 |
|---|---|---|
| 上游增量 | `data: <chat.completion.chunk JSON>` | 上游 OpenAI 兼容 chunk 原样转发；文本在 `choices[0].delta.content` |
| `tool_call` | `event: tool_call` + `data: {"tool_call_id","tool","arguments"}` | 平台即将执行一个无需确认的工具 |
| `tool_result` | `event: tool_result` + `data: {"tool_call_id","tool","content"}` | 工具执行结果（`content` 为 JSON 字符串，超 16KB 截断） |
| `tool_confirmation` | `event: tool_confirmation` + `data: {"pending":[...],"messages":[...]}` | 存在需确认的工具，本轮对话暂停（见 9.5） |
| `error` | `event: error` + `data: {"code","message","details"}` 或 `{"message"}` | 流已开始后发生的错误 |
| 结束 | `data: [DONE]` | 始终最后发送 |

流开始前出错（参数、模型/模板不存在、上游连接失败）时不进入 SSE，直接返回普通 JSON 错误，例如：

```json
{"code":40400,"message":"prompt template not found","data":null,"request_id":"2a6df6b6d8a0d852267b00ee"}
```

纯文本对话（`scene=doc_demo`）：

```text
data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"content": "你好！"}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}

data: [DONE]
```

带工具（`"tools": true`，模型调用 `search_audit_logs`，平台执行后把结果交回模型继续生成）：

```text
data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"role": "assistant", "tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "search_audit_logs", "arguments": ""}}]}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"arguments": "{\"username\":\"admin\",\"limit\":1}"}}]}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]}

event: tool_call
data: {"arguments":{"username":"admin","limit":1},"tool":"search_audit_logs","tool_call_id":"call_1"}

event: tool_result
data: {"content":"{\"items\":[{\"id\":50,\"user_id\":1,\"username\":\"admin\",\"ip\":\"::1\",\"method\":\"POST\",\"pat…(截断)","tool":"search_audit_logs","tool_call_id":"call_1"}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {"content": "最近一条审计为 admin 的操作。"}, "finish_reason": null}]}

data: {"id": "chatcmpl-mock1", "object": "chat.completion.chunk", "created": 1760673600, "model": "mock-gpt", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}

data: [DONE]
```

#### 非流式（`"stream": false`）

`data` 为 `{"content": 汇总文本, "finish_reason": "...", "events": [{"event": 名称, "data": ...}]}`，`events` 中依次是上表中的平台事件：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "content": "最近一条审计为 admin 的操作。",
    "events": [
      {
        "data": {
          "arguments": {
            "username": "admin",
            "limit": 1
          },
          "tool": "search_audit_logs",
          "tool_call_id": "call_1"
        },
        "event": "tool_call"
      },
      {
        "data": {
          "content": "{\"items\":[{\"id\":40,\"user_id\":1,\"username\":\"admin\",\"ip\":\"::1\",\"method\":\"POST\",\"path\":\"/api/v1/approvals/3/approve\",\"resou…(截断)",
          "tool": "search_audit_logs",
          "tool_call_id": "call_1"
        },
        "event": "tool_result"
      }
    ],
    "finish_reason": "stop"
  },
  "request_id": "b08d48eef6c92f78a703c6ae"
}
```

参数校验失败：

```json
{
  "code": 40001,
  "message": "invalid request body",
  "data": {
    "details": "Key: 'ChatRequest.Messages' Error:Field validation for 'Messages' failed on the 'min' tag"
  },
  "request_id": "3343706aba340501e340dcb2"
}
```

### 9.5 工具调用确认

当模型请求 `requires_confirmation=true` 的工具时，平台不执行，而是保存待确认调用并发出 `tool_confirmation` 事件后结束本轮（以下结构取自 `chat.go`；当前核心内置工具均无需确认，故未抓包）：

```text
event: tool_confirmation
data: {"pending":[{"confirmation_id":"tc_<32位hex>","tool_call_id":"call_1","tool":"<工具名>","description":"...","arguments":{...},"expires_at":"<RFC3339>"}],"messages":[<assistant 消息(含 tool_calls)>, <已直接执行的 tool 消息>...]}
```

客户端流程：

1. 把 `messages` 追加到本地对话历史；
2. 对每个 `pending` 项，调用 `POST /ai/tool-calls/{confirmation_id}/confirm` 或 `/reject`（无请求体），在 `expires_at` 前完成，且只能由发起对话的用户操作；
3. 响应为 `{"call": PendingCall, "message": tool 消息}`，把 `message`（`role: "tool"`）追加到历史；
4. 全部处理后再次调用 `POST /ai/chat`，模型据此继续。

`PendingCall` 字段：`id`、`user_id`、`tool`、`tool_call_id`、`arguments`、`status`（`executed`/`failed`/`rejected`）、`result`、`error`、`created_at`。拒绝时返回的 tool 消息内容为 `{"error":"用户拒绝执行该操作"}`。确认时会重新检查工具权限（失去权限返回 403）。

调用不存在、已决策或已过期：

```json
{
  "code": 40400,
  "message": "tool call not found, already decided or expired",
  "data": null,
  "request_id": "d5f65c45ca57089e49fd9298"
}
```

---

## 10. 健康检查与指标

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 存活探针，纯文本 `ok` |
| GET | `/api/v1/health` | 无 | 就绪探针，检查数据库与 Redis |
| GET | `/api/v1/version` | 无 | 版本信息 |
| GET | `/metrics` | 无 | Prometheus 指标 |

### GET /healthz

```text
ok
```

### GET /api/v1/health

全部正常时 HTTP 200、`status: "ok"`；数据库或已配置的 Redis 不可用时 HTTP 503、`status: "degraded"`（`code` 仍为 0，不含 `request_id`）。未配置 Redis 时 `redis: "disabled"`。下例为抓包时的实际状态（Redis 未启动）：

```json
{
  "code": 0,
  "data": {
    "database": "ok",
    "redis": "dial tcp 127.0.0.1:6379: connect: connection refused",
    "status": "degraded"
  },
  "message": "degraded"
}
```

### GET /api/v1/version

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "version": "dev",
    "commit": "unknown",
    "build_time": "unknown",
    "go_version": "go1.23.4",
    "platform": "linux/arm64"
  },
  "request_id": "e5cdd81c9df2bcdd1ec759f0"
}
```

### GET /metrics

Prometheus 文本格式，包含 Go/进程默认指标与平台指标：

| 指标 | 类型 | 标签 |
|---|---|---|
| `matrix_build_info` | gauge | `version`、`commit` |
| `matrix_http_requests_total` | counter | `method`、`route`、`status` |
| `matrix_http_request_duration_seconds` | histogram | `method`、`route` |
| `matrix_http_requests_in_flight` | gauge | — |

```text
matrix_build_info{commit="unknown",version="dev"} 1
matrix_http_requests_total{method="DELETE",route="/api/v1/roles/:id",status="200"} 1
matrix_http_request_duration_seconds_bucket{method="DELETE",route="/api/v1/ai/models/:id",le="0.005"} 1
```
