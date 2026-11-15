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
| GET | `/auth/sso/config` | 公开 | 企业 SSO 是否启用及按钮名称（v1.26） |
| GET | `/auth/sso/start?redirect=` | 公开 | 跳转到 OIDC 身份源（授权码 + PKCE） |
| GET | `/auth/sso/callback` | 公开 | 身份源回调；带一次性 `sso_ticket` 跳回页面 |
| POST | `/auth/sso/token` | 公开 | 用一次性票据换取平台会话 |
| GET | `/branding` | 公开 | 平台名称（登录前展示用） |
| GET / PUT | `/settings/sso` | `setting:read` / `setting:manage` | 读取 / 保存 OIDC 配置（密钥不返回） |
| POST | `/settings/sso/test` | `setting:manage` | 读取身份源发现文档，验证配置 |

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

### 企业 SSO（OIDC，v1.26）

实现：`server/internal/sso`，迁移 `store/migrations/0006_sso.up.sql`（`sso_states`、`sso_tickets`）。

流程：登录页「使用企业 SSO 登录」→ `GET /auth/sso/start` 生成 state、nonce、PKCE verifier（存数据库，10 分钟有效）并跳转身份源 → 用户认证 → `GET /auth/sso/callback?code&state`：校验并删除 state，用授权码（`client_secret_post` + `code_verifier`）换 access token，读取 userinfo → 按用户名字段映射本地用户，不存在且开启「首次登录自动创建」时以默认角色创建（本地密码不可用，只能走 SSO；默认角色不允许为 admin）→ 生成 60 秒一次性票据，跳回 `redirect?sso_ticket=...` → 页面 `POST /auth/sso/token {"ticket":"..."}` 得到与密码登录相同的 `{"token":{...},"user":{...}}`。

- 地址栏里只出现一次性票据，平台 token 不进 URL；state 与票据都只存 SHA-256 摘要，用后即删，多副本可用。
- `redirect` 只允许平台外部地址开头，其它一律回到首页，防开放重定向。
- 失败时跳回 `?sso_error=invalid_state | invalid_identity | server_error | provider_<身份源错误>`，登录页显示中文提示。
- 账号被禁用或锁定时仍拒绝登录；会话有效期遵循安全策略。

`PUT /settings/sso` 请求体（`client_secret` 留空表示保留原值）：
```json
{"enabled":true,"display_name":"企业 SSO","issuer":"https://sso.example.com/realms/ops","client_id":"xingshu-matrix",
 "client_secret":"...","redirect_base":"https://matrix.example.com","scopes":"openid profile email",
 "username_claim":"preferred_username","name_claim":"name","email_claim":"email","auto_create":true,"default_role":"viewer"}
```
需要在身份源登记的回调地址：`{redirect_base}/api/v1/auth/sso/callback`（`GET /settings/sso` 的 `callback_url` 字段）。

已验证：沙箱内用最小 OIDC 身份源（授权码 + PKCE 校验）完成浏览器端到端登录、自动建号、伪造 state 与票据重放被拒。尚未与 Keycloak / Azure AD 等真实身份源联调；当前不校验 ID Token 签名，身份以服务端直连 userinfo 端点的结果为准。

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

### 审计完整性

审计记录写入时按 id 顺序串成哈希链：`hash = SHA-256("xsaudit/v1\n" + 规范化 JSON 数组[id, prev_hash, created_at, user_id, username, ip, method, path, resource, resource_id, action, status_code, result, duration_ms, request_id, user_agent, detail])`，规范化细节见 `server/internal/audit/chain.go` 的 `CanonicalHash`（detail 对象键排序、created_at 为 UTC 微秒 `2006-01-02T15:04:05.000000Z`）。

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/audit/chain/verify` | `audit:read` | 在线校验链（`from_id`、`limit` 1–50000，默认 10000） |
| GET | `/audit/export` | `audit:export` | 按时间段流式导出，含 `prev_hash`/`hash`，可离线校验 |
| POST | `/audit/archive` | `audit:manage` | 把时间段导出为 `jsonl.gz` 写入对象存储（未配置时写本地归档目录） |

`audit:export`、`audit:manage` 需登记在核心权限目录（`rbac.CorePermissions`）后才能授予普通角色；超级管理员（`*`）始终可用。

#### GET /audit/chain/verify

返回 `{checked, ok, break_at, kind, head_id, head_hash, last_id}`。`kind`：`hash_mismatch`（记录内容被改）、`prev_mismatch`（前一条缺失/被删/顺序被改）、`unchained`（记录尚未入链）。`checked == limit` 且 `last_id < head_id` 时，以 `from_id=last_id+1` 继续。只校验 live 段（`start_id` 之前的历史记录不校验）。

#### GET /audit/export

参数：

| 参数 | 必填 | 说明 |
|---|---|---|
| `from` | 是 | 开始时间（含），RFC 3339 或 Unix 秒 |
| `to` | 是 | 结束时间（不含），须晚于 `from`，跨度 ≤ 366 天 |
| `format` | 否 | `jsonl`（默认）或 `csv` |

- 范围语义：取 `created_at ∈ [from, to)` 的最小与最大 id，导出这两个 id 之间的**连续 id 区间**（按 id 升序），保证区间内链条不断开；因 `created_at` 取请求开始时间，区间可能带有少量时间略在边界外的记录。
- 响应为文件流（非 JSON 信封）：`Content-Type: application/x-ndjson` 或 `text/csv`，`Content-Disposition: attachment; filename="audit-<from>-<to>.<format>"`，`Cache-Control: no-store`。区间内无记录时返回 200 与空内容（CSV 仅表头）。
- JSONL 每行一条：`id, created_at, user_id, username, ip, method, path, resource, resource_id, action, status_code, result, duration_ms, request_id, user_agent, detail, prev_hash, hash`。字段值原样输出（不做 HTML 转义），可直接重算哈希。
- CSV：UTF-8 带 BOM，表头同上；`detail` 为紧凑 JSON 文本；以 `= + - @ TAB CR` 开头的文本单元格前加 `'` 防止表格公式注入，因此**离线校验请使用 JSONL**。
- 流式输出中途出错时（响应头已发出），JSONL 末尾追加一行 `{"error":"export interrupted"}`，CSV 末尾追加一行 `#export interrupted`；拿到这行即说明文件不完整。
- 导出本身写一条审计（GET 不经中间件，由接口显式写入）：`resource=audit, resource_id=export, action=export`，`detail.data` 含 `from/to/format/rows/first_id/last_id/complete`，中断时 `result=failure`。
- 错误：`40001` 参数缺失 / 时间格式错误 / `to` 不晚于 `from` / 跨度超 366 天 / `format` 非法；`40100`、`40300`。

离线校验：逐行解析（Go 可用 `audit.ParseExportRow`），以第一行的 `prev_hash` 为锚点依次检查 `prev_hash == 上一行 hash` 且 `CanonicalHash(prev_hash, 行) == hash`（即 `audit.VerifyRows`）。区间内若含未入链（`hash` 为空）的历史记录，会在该行报 `unchained`。

#### POST /audit/archive

请求：`{"from": "2026-10-01T00:00:00+08:00", "to": "2026-11-01T00:00:00+08:00"}`（规则同导出）。

响应 `data`：

```json
{
  "file": "audit-20260930T160000Z-20261031T160000Z-1201-58830-1763100000.jsonl.gz",
  "storage": "local",
  "location": "/var/lib/matrix/archive/audit/audit-20260930T160000Z-20261031T160000Z-1201-58830-1763100000.jsonl.gz",
  "rows": 57630,
  "first_id": 1201,
  "last_id": 58830,
  "last_hash": "9f2c…",
  "size": 1843321,
  "sha256": "5be1…",
  "from": "2026-09-30T16:00:00Z",
  "to": "2026-10-31T16:00:00Z"
}
```

- 内容与 JSONL 导出一致，gzip 压缩；`sha256`/`size` 针对 `.jsonl.gz` 文件本身。
- 存放：配置了对象存储（`object_store`，与平台备份共用）时写入 `<prefix>/audit/<file>`，`storage=s3`；否则写入 `archive.dir`（默认 `/var/lib/matrix/archive`）下 `audit/<file>`，`storage=local`。
- 归档**不删除**数据库中的记录。同一时刻只允许一个归档任务。
- 审计：由审计中间件记录，`resource=audit, resource_id=archive, action=archive`，成功时 `detail.data` 为上面的结果。
- 错误：`40001`（参数 / 时间段内无记录）、`40900`（已有归档在进行）、`50000`（读取或写入存储失败，未写入任何对象）。

未验证项：大时间段（百万级记录）的导出/归档耗时与反向代理超时；对象存储真实写入（单测仅用内存与本地目录）；归档在请求内同步执行，客户端断开会中止归档。

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
| `matrix_jobs_processed_total` | counter | `type`、`result`（见第 12 节） |
| `matrix_jobs_duration_seconds` | histogram | `type` |
| `matrix_jobs_by_status` | gauge | `status` |

```text
matrix_build_info{commit="unknown",version="dev"} 1
matrix_http_requests_total{method="DELETE",route="/api/v1/roles/:id",status="200"} 1
matrix_http_request_duration_seconds_bucket{method="DELETE",route="/api/v1/ai/models/:id",le="0.005"} 1
```

## 11. 平台备份

实现：`server/internal/backup`（`api.go`、`runner.go`、`premigrate.go`、`policy.go`）。备份为 PostgreSQL `pg_dump --format=custom` 导出，记录在 `platform_backups` 表（迁移 `core/0011_audit_chain`）。前端：系统设置 →「平台备份」（`public/xs-platform-backup.js`，`window.XSPlatformBackup.render(el)`）。

> 本节示例为按代码整理的字段说明，未对运行实例实测（见末尾「未验证项」）。

### 备份记录字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 记录 ID |
| `kind` | string | `manual`（手动）、`scheduled`（定时）、`pre_migrate`（升级前自动） |
| `status` | string | `running`、`succeeded`、`failed` |
| `file` | string | 备份文件名 `xs-<UTC 时间>.dump`，仅成功时有值 |
| `size` | int64 | 字节数 |
| `sha256` | string | 文件 SHA-256（十六进制） |
| `schedule_slot` | string\|null | 定时备份的计划时刻（UTC 分钟），其余为 null |
| `error` | string | 失败原因（≤2000 字，已脱敏数据库密码） |
| `created_by` | int64\|null | 发起用户 ID；定时 / 升级前为 null |
| `created_at` / `finished_at` | RFC 3339 | 开始 / 结束时间 |

### GET /platform/backups

权限 `setting:read`。分页参数 `page`、`page_size`，按 `created_at` 倒序。返回 `{items, total, page, page_size}`。

### POST /platform/backups

权限 `setting:manage`，无请求体。异步开始一次手动备份，HTTP **202**，`message: "备份已开始"`，`data` 为刚插入的 `running` 记录。写审计（resource `platform_backup`，action `create`）。

| HTTP / code | 场景 |
|---|---|
| 409 / 40900 | 本实例已有备份在进行（`已有平台备份正在进行，请稍后再试`） |
| 503 / 50300 | 服务器缺少 `pg_dump`、数据库连接串无效或登记记录失败（中文原因见 `message`） |

### GET /platform/backups/:id/download

权限 `setting:manage`。成功时直接返回文件流：`Content-Type: application/octet-stream`、`Content-Disposition: attachment; filename="<file>"`、`X-Content-SHA256`、`Cache-Control: no-store`。无论成败都写审计（action `download`，detail 含 file / sha256 / size / 实际发送字节数）。前端以 `Authorization` 头下载（不把令牌放进 URL），≤512 MiB 的文件在浏览器内复核 SHA-256。

| HTTP / code | 场景 |
|---|---|
| 400 / 40000 | id 非法 |
| 404 / 40400 | 记录不存在（`备份不存在`）或文件已被清理（`备份文件已不存在`） |
| 409 / 40900 | 备份未成功完成 |

### GET /platform/backup-policy

权限 `setting:read`。只读返回当前生效的备份策略（来自部署配置 `backup.*` / `MATRIX_BACKUP_*`，运行时不可修改）：

| 字段 | 说明 |
|---|---|
| `schedule_enabled` | 是否开启定时备份（`backup.enabled`） |
| `schedule` | 5 段 cron（服务器时区），未开启时为空串 |
| `keep` | 保留最近 N 份**成功的定时备份**；手动与升级前备份不自动清理 |
| `before_migrate` | 是否在数据库迁移前自动备份 |
| `before_migrate_on_failure` | `abort`（默认，备份失败中止迁移）或 `warn`（仅告警继续迁移） |
| `storage` | `local`（服务器备份目录）或 `s3`（对象存储） |
| `online_restore` | 是否支持在线恢复；当前恒为 `false` |

### 恢复

当前只支持离线恢复：停止平台服务后在服务器上执行 `matrix-server restore --file <备份文件> --yes`（会先打印 SHA-256 供核对，再以 `pg_restore --clean --if-exists --no-owner` 覆盖数据库）。控制台在 `online_restore=false` 时只显示离线恢复说明，不显示「恢复」按钮。

前端已预留在线恢复流程（`online_restore=true` 时显示）：输入「恢复」二字二次确认后调用 `POST /platform/backups/:id/restore`，请求体 `{"confirm":"恢复"}`。**该接口后端尚未实现**。

### 迁移前自动备份（pre_migrate）

`store.MigrateWith(ctx, pool, log, store.MigrateOptions{BeforeApply: backup.PreMigrateHook(runner)}, sources...)`：迁移器在持有迁移锁、校验完全部已执行迁移的校验和之后，若存在待执行迁移，先调用一次 `BeforeApply(pending)`（`pending` 形如 `core/0012_jobs`），再逐个执行。`store.Migrate` 签名与行为不变（不带回调）。

| 情况 | 行为 |
|---|---|
| `MATRIX_BACKUP_BEFORE_MIGRATE=false`（`backup.before_migrate: false`） | 跳过 |
| 无待执行迁移 | 不调用回调，不备份 |
| 全新数据库（`schema_migrations` 无记录） | 跳过 |
| 已有 `platform_backups` 表 | `Runner.Run(pre_migrate)`，记录可在列表中看到 |
| 升级自 0011 之前（表不存在） | 仍执行 pg_dump 并保存文件（本地目录 / 对象存储），但不登记记录 |
| 备份失败（默认） | 中止迁移，返回中文错误：`迁移前自动备份失败，已中止数据库迁移（待执行 N 项迁移：…）：<原因>。…` |
| 备份失败且 `MATRIX_BACKUP_BEFORE_MIGRATE_ON_FAILURE=warn` | 记录告警日志后继续迁移 |

### 未验证项

- 未在运行实例上实测上述接口（`/platform/backup-policy` 为本轮新增，需主代理按补丁接线后部署）。
- 迁移前备份未在真实 PostgreSQL 上跑通（单测覆盖决策逻辑；`pg_dump` 调用沿用 Runner 已有实现）。
- 浏览器下载大文件时整份读入内存（Blob）；超大备份建议在服务器上直接取文件。

## 12. 后台任务

实现：`server/internal/jobs`（`jobs.go` 状态机与退避、`store.go` PostgreSQL 存储、`worker.go` 执行器与回收器、`api.go` 管理接口、`metrics.go` 指标）。任务存放在 `platform_jobs` 表（迁移 `core/0012_jobs`）。管理接口挂在 `/api/v1` 认证分组下；写操作由审计中间件记录（resource `platform_job`）。

> 本节为按代码整理的说明，未对运行实例实测（见末尾「未验证项」）。控制台入口：系统设置 → 后台任务。

### 权限

与平台备份一致，核心权限目录中没有独立的 `platform:*` 权限，沿用系统设置权限：

| 操作 | 权限 |
|---|---|
| 列表、详情 | `setting:read` |
| 重试、删除 | `setting:manage` |

### 任务字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 任务 ID |
| `type` | string | 任务类型（≤128 字符，对应注册的处理器） |
| `payload` | object | 任务参数。**接口返回前脱敏**：键名含 `password`、`passwd`、`secret`、`token`、`private_key`、`access_key`、`api_key`、`credential`、`kubeconfig` 等（不区分大小写、任意层级）的值替换为 `"***"`；无法解析时整体返回 `"***"` |
| `status` | string | `queued`、`running`、`succeeded`、`failed`、`dead`（见状态机） |
| `priority` | int | 优先级，越大越先被认领 |
| `attempts` / `max_attempts` | int | 已执行次数（认领时 +1）/ 最大执行次数（默认 5，上限 100） |
| `run_at` | RFC 3339 | 最早可执行时间（失败重试时为下次执行时间） |
| `locked_by` | string | 持有租约的执行节点标识，未执行时为空串 |
| `locked_until` | RFC 3339\|null | 租约到期时间 |
| `last_error` | string | 最近一次失败原因（≤4000 字节，超出截断并加 `…`） |
| `dedupe_key` | string\|null | 去重键：同一去重键同时只能存在一个未完成任务 |
| `created_by` | int64\|null | 创建者用户 ID；系统任务为 null |
| `created_at` / `updated_at` | RFC 3339 | 创建 / 更新时间 |
| `started_at` / `finished_at` | RFC 3339\|null | 最近一次开始 / 结束（成功或死信）时间 |

### GET /jobs

权限 `setting:read`。按 `id` 倒序分页，返回 `{items, total, page, page_size}`。

| 参数 | 说明 |
|---|---|
| `status` | 可选，逗号分隔的多个状态，如 `status=failed,dead`；空项与重复项忽略；出现未知状态返回 400 |
| `type` | 可选，按任务类型精确匹配（去首尾空白，≤128 字符） |
| `page` / `page_size` | 同 1.4（默认 1 / 20，`page_size` 最大 200） |

未知状态的错误响应：`code=40001`，`message: "任务状态无效：<值>"`，`details.allowed` 为 `["queued","running","failed","dead","succeeded"]`。

### GET /jobs/:id

权限 `setting:read`。返回单个任务（payload 已脱敏）。

### POST /jobs/:id/retry

权限 `setting:manage`，无请求体。**仅 `failed` 或 `dead` 任务可重试**：状态改为 `queued`、`run_at=now()`，清空租约与 `finished_at`；`dead` 任务的 `attempts` 重置为 0（重新获得完整次数），`failed` 任务保留已执行次数、仅跳过剩余退避等待。`last_error` 保留到下次执行。成功后唤醒本实例执行器立即认领。返回更新后的任务（payload 已脱敏）。审计 action `retry`，detail 含 `type`、`max_attempts`。

### DELETE /jobs/:id

权限 `setting:manage`。**仅 `dead` 或 `succeeded` 任务可删除**（排队、执行中、等待重试的任务不可删）。返回 `{"id": <id>, "deleted": true}`。审计 action `delete`，detail 含 `type`、`status`、`attempts`。

### 错误码

| HTTP / code | 场景 | message |
|---|---|---|
| 400 / 40000 | `:id` 不是正整数 | `任务编号无效` |
| 400 / 40001 | `status` 含未知状态；`type` 过长 | `任务状态无效：…` / `任务类型过长` |
| 403 / 40300 | 缺少 `setting:read` / `setting:manage` | — |
| 404 / 40400 | 任务不存在 | `任务不存在` |
| 409 / 40900 | 重试非 failed/dead 任务、删除非 dead/succeeded 任务 | `当前状态不允许该操作：仅失败或死信任务可重试，仅死信或已完成任务可删除` |
| 409 / 40900 | 重试时已有相同去重键的未完成任务 | `已有相同去重键的未完成任务，无法重试` |

### 状态机

```text
queued  --认领-->                         running
failed  --认领（run_at 已到）-->           running
running --成功-->                         succeeded
running --失败，attempts < max_attempts--> failed（run_at = now + 退避）
running --失败且次数用尽 / 永久错误-->      dead（死信）
running --租约过期（回收器）-->            failed | dead
failed | dead --手动重试-->                queued
succeeded | dead --删除-->                 （移除）
```

- **认领**：执行节点以 `FOR UPDATE SKIP LOCKED` 按 `priority DESC, run_at, id` 认领一条 `queued`/`failed` 且 `run_at <= now()` 且类型已注册的任务，`attempts+1`，写入 `locked_by` 与租约 `locked_until`（默认 1 分钟，最小 3 秒）；处理期间每 1/3 租约心跳续期。只有仍持有租约时才记录结果，租约已丢失的结果被丢弃。
- **指数退避**：第 n 次失败后等待 `base × 2^(n-1)`（默认 base 10 秒，上限 1 小时），带随机抖动，结果不超过上限、不低于 base/2。处理器可按类型配置 base / 上限 / 超时（默认超时 10 分钟）。
- **最大次数**：`attempts >= max_attempts` 或处理器返回永久错误（`jobs.Permanent`，如参数非法、目标已删除）时直接进入 `dead`；未注册的任务类型按永久错误处理。处理器 panic 视为普通失败，会重试。
- **租约回收**：回收器默认每 30 秒把租约过期的 `running` 任务（执行节点崩溃）改为 `failed`（次数用尽则 `dead`），`last_error` 为 `执行租约超时（执行节点可能已退出），已由回收器回收`，`run_at=now()`，**立即可被再次认领**（过期租约本身已起到延迟作用）。
- **停机中断**：执行节点停止时被中断的任务（非永久错误）释放为 `failed`，`last_error` 为 `执行节点停止，任务已释放等待重新执行`，按正常退避等待其他节点重新执行，不会因停机直接进死信。

### 指标

后台任务指标随 `/metrics` 输出（见第 10 节指标表）：

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `matrix_jobs_processed_total` | counter | `type`、`result` | 每次执行的结果：`succeeded`、`failed`（将重试）、`dead`（进入死信）、`reaped`（租约过期被回收；回收后次数用尽时另计一次 `dead`） |
| `matrix_jobs_duration_seconds` | histogram | `type` | 单次执行耗时（桶 0.05s～3600s；回收的任务不计） |
| `matrix_jobs_by_status` | gauge | `status` | 各状态当前任务数，随回收器周期（默认 30 秒）刷新；`dead` 即死信积压 |

### 未验证项

- 未在真实 PostgreSQL 上实测接口与认领/回收 SQL（`store_pg_test.go` 需数据库环境变量开启，本轮未运行）。
- 多副本并发认领（`SKIP LOCKED`）、租约丢失与回收器竞争未在多实例环境验证。
- 未运行 `go test -race`。
- 控制台页面（系统设置 → 后台任务）只在模拟环境测试；接口未在运行实例上调用。

## 13. 企业空间

实现：`server/internal/workspace`（`service.go` 校验与业务规则、`store.go` PostgreSQL 存储、`api.go` 路由）。表来自迁移 `core/0009_workspace_scope`（`workspaces`、`workspace_members`、`workspace_projects`、`role_bindings`），本节不新增迁移。数据范围解析（`rbac.Principal.Scopes`）读同一组表；成员、项目、角色绑定的增删改以及删除空间后，本实例的范围缓存立即失效（`rbac.Service.InvalidateScopes`），其他副本最长 15 秒后生效。

> 本节为按代码整理的说明，未对运行实例实测（见末尾「未验证项」）。

### 权限

| 操作 | 权限 |
|---|---|
| 列表、详情、成员列表、项目列表、角色绑定列表 | `workspace:read` |
| 新建/修改/删除空间；增改删成员；增删项目；新建/删除角色绑定 | `workspace:manage` |
| 新建/删除 `global` 或 `env` 范围的角色绑定 | 另需 `user:manage`（这类绑定扩大平台级权限，等同分配全局角色） |

- 数据范围：持有 `workspace:manage`，或持有 `workspace:read` 且不受范围限制（有任何全局角色）的用户可见全部空间；仅靠空间成员/范围化绑定获得权限的用户（受限用户）只看到自己所属的空间，访问其他空间的详情/成员/项目返回 403 `该企业空间不在你的数据范围内`。受限用户的 `GET /role-bindings` 只返回其所属空间的 `workspace` 绑定与其可见项目的 `project` 绑定。
- 空间成员角色本身授予的权限见 `rbac.WorkspaceRolePermissions`（viewer/member/admin/owner 都含 `workspace:read`，都不含 `workspace:manage`）；本轮空间 owner/admin **不能**通过本接口管理成员，写操作统一要求平台权限 `workspace:manage`。
- 防越权：新建角色绑定时，角色的每个权限都必须为操作者自身所持有（`*` 需操作者持有 `*`），否则 403 `不能授予超出你自身权限的角色`，`details.missing` 列出缺少的权限。
- 这些路由在 `principal.Scopes` 为空时自行解析数据范围，因此不依赖全局挂载 `ScopeMiddleware`。

### 字段

空间（Workspace）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 空间 ID |
| `name` | string | 名称，去首尾空白后 1–63 个字符（按字符计，可中文），不含控制字符；不区分大小写唯一 |
| `description` | string | 描述，≤512 字符 |
| `manager_id` | int64\|null | 空间管理员用户 ID（展示用；管理权限以成员角色为准） |
| `manager_name` | string | 管理员用户名，无则空串 |
| `cpu_limit_milli` | int64 | CPU 总配额（毫核），0 表示不限制，不能为负 |
| `memory_limit_bytes` | int64 | 内存总配额（字节），0 表示不限制，不能为负 |
| `member_count` / `project_count` | int64 | 成员数 / 项目数 |
| `created_by` | string | 创建者用户名 |
| `created_at` / `updated_at` | RFC 3339 | 创建 / 更新时间 |

成员（Member）：`id`、`workspace_id`、`kind`（`user`|`group`）、`user_id`|`group_id`（另一个为 null）、`name`（用户名或组名）、`display_name`（用户显示名，组为空串）、`username`（用户成员才有）、`group_name`（组成员才有）、`role`（`owner`|`admin`|`member`|`viewer`）、`created_by`、`created_at`。列表按 owner→admin→member→viewer、再按 id 排序。

项目（Project）：`id`、`workspace_id`、`cluster_id`、`cluster_name`、`cluster_env`、`cluster_exists`（集群已删除或集群模块未安装时为 false，名称为空串，前端显示「集群不存在」）、`namespace`、`created_by`、`created_at`。

角色绑定（RoleBinding）：`id`、`role_id`、`role_code`、`role_name`、`kind`（`user`|`group`）、`user_id`|`group_id`、`subject_name`（用户名或组名）、`scope`、`scope_value`、`created_by`、`created_at`。

`scope` / `scope_value` 规则（与表 CHECK 及 rbac 解析一致）：

| scope | scope_value |
|---|---|
| `global`（默认） | 必须为空串 |
| `env` | 环境编码，`^[a-z0-9][a-z0-9_-]{0,63}$`（如 `prod`） |
| `cluster` | 集群 ID（正整数，无前导 0）；集群模块已安装时须存在 |
| `workspace` | 空间 ID（正整数）；须存在 |
| `project` | `<集群ID>/<命名空间>`，命名空间为 DNS-1123 标签；集群须存在 |

请求中 `scope_value` 可传字符串或数字（如 `"scope_value": 5`）；`scope` 不区分大小写。

### GET /workspaces

权限 `workspace:read`。参数 `keyword`（按名称/描述模糊匹配，≤64 字符）、`page`、`page_size`（同 1.4）。按名称排序，返回 `{items, total, page, page_size}`。

### POST /workspaces

权限 `workspace:manage`。请求体：

```json
{ "name": "研发中心", "description": "研发部门", "manager_id": 12, "cpu_limit_milli": 8000, "memory_limit_bytes": 17179869184 }
```

`manager_id` 可省略或为 null/0。创建与首个 owner 成员在同一事务：指定了 `manager_id` 时该用户成为 owner，否则创建者成为 owner。返回 201 与空间对象。审计 resource `workspace`，action `create`，detail 含 `name`、`manager_id`、配额。

### GET /workspaces/:id

权限 `workspace:read`，且空间在调用者数据范围内。返回空间对象。

### PUT /workspaces/:id

权限 `workspace:manage`。请求体字段同创建，**均可省略**（省略即不变）；`manager_id: 0` 清除管理员。返回更新后的空间。审计 action `update`，detail 含 `name` 与 `changed`（修改的字段名列表）。

### DELETE /workspaces/:id

权限 `workspace:manage`。请求体 `{"confirm_name": "<空间名称>"}`，必须与名称完全一致（去首尾空白后，区分大小写），缺失或不一致返回 400。删除会级联删除成员、项目（只解除归属，不删除集群中的命名空间资源）以及 `scope=workspace` 指向该空间的角色绑定。返回 `{"id": <id>, "deleted": true}`。审计 action `delete`，detail 含 `name`、`member_count`、`project_count`。

### GET /workspaces/:id/members

权限 `workspace:read` + 数据范围。返回 `{items, total}`（不分页）。

### POST /workspaces/:id/members

权限 `workspace:manage`。请求体 `{"user_id": 3, "role": "member"}` 或 `{"group_id": 2, "role": "viewer"}`（二者必须且只能给一个；`role` 不区分大小写，白名单 `owner|admin|member|viewer`）。返回 201 与成员对象。审计 resource `workspace_member`（resource_id 为空间 ID），action `create`，detail 含 `member_id`、`kind`、`user_id`/`group_id`、`name`、`role`。

### PUT /workspaces/:id/members/:mid

权限 `workspace:manage`。请求体 `{"role": "admin"}`。把最后一个 owner 改成其他角色返回 409。审计 action `update`。

### DELETE /workspaces/:id/members/:mid

权限 `workspace:manage`。删除最后一个 owner 返回 409。返回 `{"id": <mid>, "deleted": true}`。审计 action `delete`。

「最后一个 owner」按该空间 `role=owner` 的成员行计数（用户组成员也计入），检查与修改在同一事务内并锁定空间行，避免并发把 owner 全部移除。没有任何 owner 的空间（如由无用户 ID 的调用方创建）不受此限制。

### GET /workspaces/:id/projects

权限 `workspace:read` + 数据范围。返回 `{items, total}`（不分页），按集群、命名空间排序。

### POST /workspaces/:id/projects

权限 `workspace:manage`。请求体 `{"cluster_id": 1, "namespace": "team-a"}`。命名空间须为 DNS-1123 标签（小写字母、数字、连字符，首尾为字母或数字，≤63）。集群模块已安装时集群须存在（否则 404）。同一集群命名空间只能归属一个空间：已归属任何空间（包括本空间）返回 409 `该命名空间已归属企业空间「<名称>」`，`details.workspace` 为该空间名称。本接口不检查命名空间在集群中是否真实存在。返回 201 与项目对象。审计 resource `workspace_project`，detail 含 `project_id`、`cluster_id`、`namespace`。

### DELETE /workspaces/:id/projects/:pid

权限 `workspace:manage`。只解除归属，不删除命名空间。返回 `{"id": <pid>, "deleted": true}`。

### GET /role-bindings

权限 `workspace:read`。可选过滤参数：`scope`、`scope_value`、`role_id`、`user_id`、`group_id`；`page` / `page_size`（默认 20，最大 200）。按 `id` 倒序，返回 `{items, total, page, page_size}`。前端查看某空间的绑定时建议传 `scope=workspace&scope_value=<空间ID>`（项目绑定传 `scope=project`），避免只拿到第一页。

### POST /role-bindings

权限 `workspace:manage`（`global`/`env` 另需 `user:manage`）。请求体：

```json
{ "role_id": 4, "user_id": 3, "scope": "project", "scope_value": "1/team-a" }
```

返回 201 与绑定对象。审计 resource `role_binding`，action `create`，detail 含角色、对象与范围。

### DELETE /role-bindings/:id

权限 `workspace:manage`（删除 `global`/`env` 绑定另需 `user:manage`）。返回 `{"id": <id>, "deleted": true}`。

### 错误码

| HTTP / code | 场景 | message 示例 |
|---|---|---|
| 400 / 40000 | 路径 ID 非正整数 | `企业空间编号无效` / `成员编号无效` / `项目编号无效` / `角色绑定编号无效` |
| 400 / 40001 | 请求体无法解析；名称为空或超过 63 字符；描述超长；配额为负；成员同时/都未给 user_id、group_id；角色不在白名单；命名空间不合法；scope/scope_value 不合法；删除确认名称缺失或不一致；过滤参数非法 | `请填写企业空间名称`、`空间角色无效，可选：owner、admin、member、viewer`、`输入的名称与企业空间名称不一致` 等 |
| 403 / 40300 | 缺少 `workspace:read` / `workspace:manage`；空间不在数据范围；global/env 绑定缺 `user:manage`；授予超出自身权限的角色 | `该企业空间不在你的数据范围内`、`不能授予超出你自身权限的角色` |
| 404 / 40400 | 空间、成员、项目、绑定、用户、用户组、角色、集群不存在 | `企业空间不存在`、`用户不存在`、`集群不存在` 等 |
| 409 / 40900 | 名称重复（不区分大小写）；成员已存在；命名空间已归属其他空间；相同绑定已存在；移除/降级最后一个 owner | `企业空间名称已存在`、`该命名空间已归属企业空间「研发中心」`、`不能移除或降级企业空间的最后一个所有者` |

### 未验证项

- 存储层 SQL 在本地临时 PostgreSQL（应用核心迁移 + 集群模块 0001）上通过 `TestPgStore`；未在运行实例上调用这些接口。
- 未在多副本下验证范围缓存失效时延（其他副本依赖 15 秒 TTL）。
- 空间配额（`cpu_limit_milli`、`memory_limit_bytes`）只存储，不对项目 ResourceQuota 做汇总校验或下发。
- 堡垒机资产节点绑定（`workspace_asset_nodes`）无接口。
