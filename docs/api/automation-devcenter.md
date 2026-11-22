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

### 协作：评论、动态与关联（v1.28）

- `GET /devcenter/{resource}/:id/comments`：评论列表，`{id,body,author,created_at,mine}`。
- `POST /devcenter/{resource}/:id/comments`：`{"body":"..."}`，发表评论；`DELETE /devcenter/comments/:id` 只能删除自己的评论（管理员除外）。
- `GET /devcenter/{resource}/:id/activities`：状态、处理人、优先级等字段变更记录，`{id,actor,action,from,to,created_at}`，由写接口自动生成。
- `GET|POST /devcenter/requirements/:id/links`、`/devcenter/bugs/:id/links`：关联应用代码仓库或流水线，请求体 `{"ref_type":"app|pipeline","ref_id":1,"ref_name":"order-service"}`。
- `GET /devcenter/links?ref_type=app|pipeline&ref_id=`：反查某个应用或流水线关联了哪些需求 / 缺陷；`DELETE /devcenter/links/:id` 解除关联。

### 统计报表（v1.28）

- `GET /devcenter/stats?days=30`：区间内按周的需求完成 / 新建、缺陷关闭 / 新建，吞吐量，周期时间与前置时间（平均、P50、P85，单位小时），需求与缺陷状态分布，未关闭缺陷按严重程度分布，处理人负载，完成率。`days` 取 7–365。
- `GET /devcenter/iterations/:id/burndown`：迭代燃尽数据，按日 `{date,scope,done,remaining,ideal}`；迭代需在 `data.start` / `data.end` 中设置起止日期。

### AI 开发（v1.28）

- `GET /devcenter/ai/status`：是否已配置可用模型。
- `POST /devcenter/requirements/:id/ai-tasks`：`{"model_id":0,"note":"补充说明"}`，`model_id` 省略时使用默认模型。接口立即返回 `201` 和任务（`status=running`），后台调用模型生成实施方案；同一需求同时只能有一个运行中的任务（否则 `409`），超时任务会被标记为失败。未启用 AI 或未配置模型时返回校验错误。
- `GET /devcenter/ai-tasks?requirement_id=`、`GET /devcenter/ai-tasks/:id`：任务列表与详情。`result` 为 `{summary,steps[],subtasks[{title,description,priority,estimate}],risks[],test_points[]}`。
- `POST /devcenter/ai-tasks/:id/apply`：`{"indexes":[0,2],"assignee":"alice"}`，把选中的子任务创建为需求（挂在原需求的迭代下），返回创建结果并写入 `applied`，同一子任务不会重复创建。
- AI 调用以发起人身份执行，只授予 `ai:chat`，不携带工具调用能力；需求中的密钥类内容不会被单独提取。

### 交付效能 DORA（v1.32）

`GET /devcenter/dora?days=30&app_id=&env=&tz_offset=480`，权限 `devcenter:read`。只读计算，不落库、无迁移。

参数：`days` 1–365（页面提供 7/30/90，缺省 30，窗口为含今天在内的最近 N 个自然日）；`app_id` 可选（CI/CD 应用 ID）；`env` 可选（环境名，≤63 字符）；`tz_offset` 分钟，缺省东八区。参数非法返回 400（`days`、`app_id` 校验失败）。

返回：
```json
{"window_days":30,"from":"2026-10-24","to":"2026-11-22",
 "deploy_frequency":{"per_day":0.4,"total":12,"series":[["2026-10-24",0],["2026-10-25",2]]},
 "lead_time":{"median_hours":9.5,"p90_hours":30.2,"samples":12,"basis":"links|run|mixed|none"},
 "change_failure_rate":{"rate":0.0833,"failed":1,"total":12},
 "mttr":{"median_hours":2.4,"samples":3,"available":true},
 "level":{"deploy_frequency":"high","lead_time":"elite","change_failure_rate":"high","mttr":"high"}}
```

口径：
- 部署频率：窗口内 `cicd_releases` 中 `kind=deploy`、`status=succeeded` 的发布数（按完成时间落天），`per_day = total / days`。
- 变更前置时间：每次成功部署的起点 → `finished_at`，取中位数与 P90。起点优先取 `devcenter_links`（价值流结构：同一需求 / 缺陷上 `kind=release, ref=<release_id>` 或 `kind=run, ref=<该发布的 run_id>` 的关联所在条目，其 `commit/mr/run` 关联的最早 `occurred_at`）；没有关联时退化为该发布对应构建（`cicd_runs.created_at`）。`basis` 标明来源：`links` 全部来自关联、`run` 全部来自构建、`mixed` 两者都有、`none` 无样本。关联表不是价值流结构时只用构建时间。
- 变更失败率：分母为窗口内已结束（成功或失败）的部署；失败 = 发布失败，或其灰度（`cicd_canaries.release_id`）状态为 `rolled_back/failed`，或完成后 24 小时内同应用同环境出现 `kind=rollback`（回滚只归到它之前最近一次成功部署）。无部署时 `rate=null`。
- 故障恢复时间：窗口内宣告（`declared_at`）且已解决的 `oncall_incidents`，宣告 → `resolved_at` 中位数。按应用过滤时以应用名匹配故障的 `service`，按环境过滤匹配 `env`。值班表不存在时 `available=false`。
- 分级（DORA 2023 逐项阈值）：部署频率 精英 ≥1 次/天、高 ≥1 次/周、中 ≥1 次/月、低 更少；前置时间（中位） 精英 <1 天、高 <1 周、中 <30 天、低 ≥30 天；失败率 精英 ≤5%、高 ≤10%、中 ≤15%、低 >15%；恢复时间（中位） 精英 <1 小时、高 <1 天、中 <1 周、低 ≥1 周。无样本为 `unknown`；窗口内没有任何部署记录时部署频率为 `unknown`，有记录但无成功部署为 `low`。
- 依赖表（`cicd_releases`、`cicd_canaries`、`oncall_incidents`）先判断存在，不存在时对应指标为空，不报错。

前端：`public/xs-dora.js`（`window.XSDora.render(el)`），路由 `#/project/dora`「交付效能」。

未验证：未在真实 PostgreSQL 上跑过 SQL（纯计算函数有单测）；`devcenter_links` 价值流结构由 vs-trace 的 0004 迁移提供，联调前 `basis` 只会是 `run`/`none`；故障按应用名匹配 `service` 为近似。

### 交付价值流：追踪、关联与发布说明（v1.32）

把需求 / 缺陷串到 代码 → 构建 → 制品 → 发布 → 线上环境。关联编号：需求 `REQ-<id>`、缺陷 `BUG-<id>`（id 为主键，大小写不敏感，也接受 `#REQ-12`、`[BUG-3]`、`feature/REQ-12-login`；编号前紧挨字母数字、编号后紧挨 ASCII 字母数字或下划线的不算）。解析函数 `devcenter.ParseItemKeys(text) []ItemKey`（`ItemKey{Type:"requirement"|"bug", ID}`），各模块共用。

**关联表** `devcenter_links`（迁移 `0004_value_stream`）：`id, item_type(requirement|bug), item_id, kind(commit|mr|run|release|alert), ref, app_id, title, url, author, env, status, occurred_at, source(auto|manual), created_at`，唯一键 `(item_type,item_id,kind,ref)`。`ref`：提交为小写 sha（≥7 位）、合并请求为编号、构建为 `cicd_runs.id`、发布为 `cicd_releases.id`、告警为告警 id。v1.28 的“代码仓库 / 流水线关联”表在此迁移中更名为 `devcenter_repo_links`（数据保留），两表共用 id 序列，id 不会重复。

**自动关联**（`source=auto`，幂等 upsert，只读 cicd 表）：
- 构建分支名中的编号（`cicd_runs.branch`）→ 该构建（run）与其提交（commit）；
- Git 推送记录（`cicd_webhook_deliveries.ref / commit_sha`，含未触发构建的特性分支推送，平台保留 7 天）→ 提交；
- 提交说明中的编号：需要 cicd 保存提交说明（`commits` 列，见“未验证 / 依赖”），有该列时自动启用；
- 传递：提交 → 相同 sha（前缀匹配）的构建；构建 → 由该构建发布（`run_id`）或镜像标签一致的发布（带环境）；构建 / 发布关联的状态随 cicd 刷新。
- 触发：查看追踪时按条目惰性同步（同一条目 60 秒内最多一次，最长 3 秒，失败不影响查询）；后台每 5 分钟扫描最近 90 天（服务关闭时随之停止）。写错的编号（条目不存在）会被忽略。

接口：

- `GET /devcenter/{requirements|bugs}/:id/trace`（`devcenter:read`）：
  ```json
  {"item":{"type":"requirement","id":12,"key":"REQ-12","title":"单点登录","status":"doing"},
   "stages":[{"key":"code","state":"done","at":"...","items":[{"id":31,"source":"auto","kind":"commit","ref":"aaaa1111","title":"feat: sso REQ-12","url":"","author":"alice","env":"","status":"","occurred_at":"...","app_id":1}]},
             {"key":"build","state":"failed|running|done|none", "at":null, "items":[]},
             {"key":"artifact", ...}, {"key":"release", ...}, {"key":"env", ...}],
   "envs":[{"env":"prod","app_id":1,"app_name":"web","release_id":9,"image":"reg/web:v2","at":"..."}]}
  ```
  阶段：`code` = commit/mr 关联（有即 done）；`build` = run 关联，按最新一条状态（succeeded→done，queued/running/awaiting_approval→running，failed/cancelled→failed）；`artifact` = 关联构建中成功且有镜像标签的镜像（推导记录，无 id）；`release` = release 关联，按最新一条状态；`env` = 当前在线且包含本条目改动的环境（推导记录 kind=env），关联的告警（kind=alert）也列在此阶段，告警仍在触发时为 failed。阶段记录按时间倒序。`envs` 判断：某应用某环境当前在线的发布（最新成功发布）就是关联的发布，或是在最早一次关联的成功发布之后的常规部署（非回滚）。关联表未就绪时返回空阶段。错误：400 id 非法，404 条目不存在。
- `POST /devcenter/{requirements|bugs}/:id/links`（`devcenter:manage`）：`{"kind":"commit|mr|run|release|alert","ref":"...","app_id":1,"title":"","url":""}`，`source=manual`，返回 201 和关联行；构建 / 发布会自动补全应用、环境、状态与时间。校验：提交 7–64 位十六进制，合并请求 `!12`/`#12`/`12`，构建 / 发布为正整数，`url` 必须 http(s)，`title` ≤200 字。错误：400 校验失败，404 条目不存在，409 已存在相同关联或关联表未就绪。**兼容**：请求体含 `ref_type` 时仍按 v1.28 的代码仓库 / 流水线关联处理。
- `DELETE /devcenter/links/:id`（`devcenter:manage`）：解除价值流手工关联；自动关联返回 409（需修改提交说明或分支名）；id 不在价值流表时按 v1.28 关联处理；都不存在返回 404。
- `GET /devcenter/releases/:release_id/items`（`devcenter:read`，`release_id` = `cicd_releases.id`）：
  ```json
  {"release":{"id":9,"app_id":1,"app_name":"web","env":"prod","image":"reg/web:v2","status":"succeeded","kind":"deploy","created_at":"...","previous_release_id":8},
   "requirements":[{"id":12,"title":"单点登录","status":"done"}],"bugs":[{"id":3,"title":"空指针","status":"fixed"}],
   "commits":[{"sha":"aaaa1111","title":"feat: sso REQ-12","author":"alice"}],
   "notes_md":"## web prod 发布说明\n..."}
  ```
  范围：同应用同环境上一次成功发布（不含，`previous_release_id`）到本次（含）之间的构建（按构建 id，发布无 `run_id` 时按镜像标签找构建；首次发布取本次构建及之前最多 200 个）；条目 = 这些构建的 run 关联 + 其提交的 commit 关联 + 直接关联本次发布的条目 + 提交说明中直接写了编号的条目。`notes_md` 为中文 Markdown，分组「新功能」（需求）/「缺陷修复」（缺陷）/「其他提交」（未关联任何条目的提交），空分组省略；时间按东八区显示。前端渲染时需转义。回滚发布没有构建范围，只列直接关联的条目。错误：400 id 非法，404 发布不存在（或 cicd 模块未启用）。

审计：手工关联的新增 / 解除由审计中间件记录，资源 `devcenter_link`，详情含 `item_type,item_id,kind,ref,source`。

未验证 / 依赖：
- 基线 cicd 不保存提交说明与合并请求标题（只有分支名、sha）。建议补丁 `briefs/v132/vs-trace.cicd-commits.patch`（未应用）：cicd 迁移新增 `cicd_runs.commits`、`cicd_webhook_deliveries.commits`（JSONB，最近 20 条 `{sha,message,author,url}`），推送 webhook 写入；本模块检测到该列即自动使用。合并请求事件目前不处理，`kind=mr` 只能手工关联。
- 推送记录只保留 7 天：后台扫描每 5 分钟一次，建立的关联长期保留；服务停机超过 7 天期间的特性分支推送会丢失。
- 环境“已上线”按发布时间线性推进近似判断，不比对 git 祖先关系。
- SQL 已在本地临时 PostgreSQL 上跑过集成测试（`TestTracePostgres`，cicd 表为最小化建表）；未在真实集群数据上验证。

## 数据库迁移

迁移文件分别为：

- `server/internal/modules/automation/migrations/0001_init.up.sql`
- `server/internal/modules/devcenter/migrations/0001_init.up.sql`
- `server/internal/modules/devcenter/migrations/0002_fields.up.sql`（处理人、data、最后修改人）
- `server/internal/modules/devcenter/migrations/0003_collab.up.sql`（评论、动态、关联、AI 开发任务）
- `server/internal/modules/devcenter/migrations/0004_value_stream.up.sql` / `.down.sql`（v1.32 价值流关联表 `devcenter_links`；旧关联表更名为 `devcenter_repo_links`）

模块通过 `server/internal/modules/registry.go` 注册，启动时由现有迁移器执行。没有 PostgreSQL 连接时，服务会在健康接口中明确报告数据库不可用；本地前端不会伪造实时数据。
