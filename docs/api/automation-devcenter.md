# Automation 与 Devcenter API

所有接口前缀为 `/api/v1`，必须通过平台认证、密码变更门禁、RBAC 和审计中间件。成功响应统一为 `{code:0,message:"ok",data,request_id}`；失败响应携带 HTTP 状态、业务 `code` 和 `request_id`。

## 前端调用约定

前端通过同源 `/api/v1` 调用；部署到独立前端域名时使用运行时 `window.XINGSHU_API_BASE` 覆盖前缀。请求必须携带 `credentials: include`，不能把数据库、Kubernetes、Jenkins 或 Kafka 凭据放进浏览器。401 由统一认证层处理，403 显示权限不足；列表接口支持 `page_size`（1–200，默认 50）和 `offset`。

页面上的“正在加载真实数据”表示正在请求后端；后端未配置或上游不可用时应展示错误状态，不能回退到静态演示数据。

## Automation

权限：`automation:read`、`automation:manage`、`automation:run`。

- `GET /automation/workflows`：分页查询流程定义。
- `POST /automation/workflows`：创建流程并写入第 1 个版本。请求包含 `name`、`description`、`trigger`、`steps`。`steps` 必须是 JSON，步骤类型由后续执行器白名单决定。
- `GET /automation/workflows/:id`：查询流程元数据。
- `POST /automation/workflows/:id/versions`：创建不可变版本，服务端递增版本号。
- `POST /automation/workflows/:id/trigger`：创建 `queued` 执行记录并固定当前版本。
- `GET /automation/runs`：查询执行历史。
- `POST /automation/runs/:id/retry`：仅允许 `failed` 执行重试并增加 `attempt`。
- `POST /automation/runs/:id/cancel`：仅允许 `queued` 或 `running` 执行取消。

执行器必须限制步骤类型、目标资源、超时和并发；当前 API 只创建受控执行记录，不提供任意 Shell 或脚本执行入口。

## Devcenter

权限：`devcenter:read`、`devcenter:manage`。资源为 `requirements`、`iterations`、`bugs`、`tests`。

v1.26 起各资源有固定状态集合，写入时校验（`GET /devcenter/meta` 返回）：

| 资源 | 状态（第一个为默认） |
|---|---|
| requirements | todo 待评审、ready 待开发、doing 开发中、testing 测试中、accepting 待验收、done 已完成、rejected 已关闭 |
| iterations | todo 规划中、doing 进行中、done 已结束 |
| bugs | todo 新建、doing 处理中、fixed 已修复、verified 已验证、closed 已关闭、reopened 重新打开 |
| tests | todo 未执行、passed 通过、failed 失败、blocked 阻塞 |

优先级 P0–P3。对象字段：`id,title,description,status,priority,iteration_id,assignee,data,created_by,updated_by,created_at,updated_at`；`data` 为 JSON 对象（≤16 KB），页面用它保存验收标准与验收记录（`acceptance`、`accepted_by`、`accepted_at`、`acceptance_result`）、迭代起止与目标（`start`、`end`、`goal`）、缺陷严重程度与环境（`severity`、`env`）、测试步骤（`steps`、`expected`、`requirement`）。

- `GET /devcenter/{resource}?status=a,b&assignee=me|<用户名>&iteration_id=&q=&page_size=&offset=`：列表，返回 `{"items":[...],"total":N}`，按更新时间倒序。
- `POST /devcenter/{resource}`：创建，`title` 必填。
- `GET /devcenter/{resource}/:id`
- `PUT /devcenter/{resource}/:id`：全量更新（`title` 必填）。
- `PATCH /devcenter/{resource}/:id`：只更新给出的字段，例如看板拖动 `{"status":"testing"}`；`iteration_id: null` 表示移出迭代。
- `DELETE /devcenter/{resource}/:id`：删除迭代时，关联需求 / 缺陷 / 用例的 `iteration_id` 置空。
- `GET /devcenter/summary`：各资源按状态计数与指派给当前用户的未完成数量，供工作台使用。

默认状态为 `todo`，默认优先级为 `P2`。所有修改由平台审计中间件记录，资源表保留创建人和更新时间。

## 数据库迁移

迁移文件分别为：

- `server/internal/modules/automation/migrations/0001_init.up.sql`
- `server/internal/modules/devcenter/migrations/0001_init.up.sql`
- `server/internal/modules/devcenter/migrations/0002_fields.up.sql`（处理人、data、最后修改人）

模块通过 `server/internal/modules/registry.go` 注册，启动时由现有迁移器执行。没有 PostgreSQL 连接时，服务会在健康接口中明确报告数据库不可用；本地前端不会伪造实时数据。
