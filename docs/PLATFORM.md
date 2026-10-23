# 星枢 Matrix 平台技术说明

> 本文回答：用什么语言开发、整体架构、项目背景、业务逻辑与接口如何实现、接口文档在哪、哪些已经实现、哪些还没实现以及怎么实现。
> 状态以仓库当前提交为准。**当前版本 v1.18（内测版）**：新增真实链路追踪、集群网络只读视图、应用运行清单与环境范围修正；本次代码尚未部署，真实外部依赖仍未完成生产级联调，**请勿直接用于生产**。
> 逐页接线矩阵与缺口清单见 [docs/PROGRESS.md](PROGRESS.md)；版本变更见 [CHANGELOG.md](../CHANGELOG.md)。

---

## 1. 项目背景

星枢 Matrix 起源于对一套大型「研发效能 + 云原生运维」平台的复刻与重构：原平台只有界面截图，没有代码。目标是做一个**可私有化部署**的一体化平台：

- **研发中心**：需求、开发、项目管理、团队协作、缺陷、测试。
- **运维中心**：集群与节点、工作负载、应用交付（CI/CD、发布单、Helm）、网络、可观测（日志/监控/告警/链路）、堡垒机与审计、值班故障、中间件。
- **AI 嵌入每个操作**：搜索框直接输入自然语言 → AI 给出「需求理解 / 字段选择 / 生成语句」→ 用户确认后执行；所有写操作必须经过权限校验、用户确认和生产审批。

设计原则：

1. 页面上的每个按钮都要对应**真实接口**和**真实执行链路**（K8s API、Agent、Prometheus、Loki/ES、Alertmanager、Kafka Admin……）。
2. 新部署的平台默认是空的，完成初始化、接入集群/数据源后才有数据；提供「演示模式」用于展示。
3. 所有写操作：RBAC → 环境策略（生产需审批）→ 执行 → 审计 → 结果回写。

## 2. 技术栈与开发语言

| 层 | 语言 / 技术 | 说明 |
|---|---|---|
| 后端 API | **Go 1.23**，Gin | `server/cmd/matrix-server`，单二进制；实测 Go 1.25 亦可构建 |
| 主机 Agent | **Go 1.23**，gorilla/websocket，gopsutil | `server/cmd/matrix-agent`，Linux 常驻进程 |
| 数据库 | PostgreSQL 14+（pgx/v5） | 元数据、权限、审批、审计、任务、日志片段；34 张表、13 个迁移 |
| 缓存 | Redis 7（go-redis v9，可选） | 健康检查已接入；多副本会话/锁待扩展 |
| 对象存储 | MinIO / S3（规划） | 堡垒机录像、制品；当前录像存本地目录 |
| K8s | client-go v0.31 | 集群管理、CI 任务（Job）、部署、`PrometheusRule` 读写 |
| 前端 | 原生 HTML/CSS/JavaScript 单页应用 | `public/index.html`（13 394 行），hash 路由，无构建依赖 |
| 前端适配层 | `public/xs-api.js` + `xs-live-core.js` + `xs-live-ops.js` | API 客户端与实时渲染接管，后端不可用时回退演示渲染 |
| 部署 | Docker 多阶段镜像、docker-compose、K8s 清单、Istio 网关、物理机脚本 | 见 `Dockerfile`、`docker-compose.yml`、`deploy/` |

规模基线（v1.18）：后端路由与权限沿用 v1.17 基线，并新增网络查询与应用 inventory 路由；前端 28 个路由键。精确历史统计见 v1.17 记录。

## 3. 总体架构

```
浏览器 (public/index.html SPA + xs-api.js / xs-live-core.js / xs-live-ops.js)
   │  HTTPS  /api/v1/*   (Bearer JWT；SSE 流式；WebSocket 终端)
   ▼
matrix-server (Go)
   ├─ 通用中间件：RequestID → Recover → CORS → 访问日志 → Prometheus 指标 → 审计
   ├─ 核心：auth / rbac / users（含部门、用户组）/ settings / platform / audit / approval / ai
   ├─ 业务模块（internal/modules/*，统一实现 app.Module 接口）
   │    cluster        ──► Kubernetes API Server（kubeconfig，client-go）
   │                   ──► PrometheusRule（告警规则读写）
   │    hosts          ◄── matrix-agent（WebSocket 长连接，JSON 帧）
   │    bastion        ──► 目标主机 SSH（x/crypto/ssh），guacd（RDP/VNC 规划）
   │    observability  ──► Prometheus / Loki / Elasticsearch / Jaeger / Tempo / Alertmanager
   │                   ◄── Alertmanager Webhook（公开接口 + Token）
   │    cicd           ──► K8s Job（git-clone + Kaniko / 测试 / Helm），Jenkins REST
   │                   ◄── GitLab / GitHub Webhook
   │    middleware     ──► Kafka Admin（franz-go）/ RabbitMQ Management API / RocketMQ Dashboard / MySQL / PostgreSQL / Redis
   │    automation     ──► 流程定义/版本/触发/运行记录（执行器待补）
   │    devcenter      ──► 需求 / 迭代 / 缺陷 / 测试（基础 CRUD）
   └─ PostgreSQL（每个模块独立迁移，表名带模块前缀）
```

### 3.1 一次写操作的完整链路（以「生产环境扩容 Deployment」为例）

1. 前端 `POST /api/v1/clusters/3/namespaces/prod/deployments/order/scale {replicas: 6}`。
2. `auth.Middleware` 校验 JWT → 加载用户角色/权限/环境范围（Principal，15 秒缓存）。
3. `rbac.RequirePerm("workload:write")` 校验权限；模块再校验用户是否有该集群环境（prod）的范围。
4. 模块调用 `Approval.Gate`：读取环境策略，prod `require_approval=true` → 创建审批单 `cluster.change`，返回 **HTTP 202 / code 40304**。
5. 审批人 `POST /api/v1/approvals/:id/approve` → 审批服务异步执行模块注册的 Handler：重新加载集群、复核申请人权限、调用 K8s Scale 子资源。
6. 审计中间件记录每一次写请求（用户、IP、路径、资源、结果、耗时）；审批单记录执行结果。

已注册的审批类型（`GET /api/v1/approval-types` 可查）：

| 类型 | 模块 | 触发场景 |
| --- | --- | --- |
| `generic` | core | 通用审批（无自动执行动作） |
| `cluster.change` | cluster | 扩缩容 / 重启 / 删除 / 节点维护 / YAML apply |
| `hosts.exec` | hosts | 在主机上执行命令 |
| `hosts.file_push` | hosts | 向主机推送文件 |
| `cicd.deploy` | cicd | 应用部署 / 回滚到受控环境 |
| `middleware.sql` | middleware | 生产库高危 SQL 执行 |
| `middleware.change` | middleware | Kafka Topic / RabbitMQ 队列创建删除 |

### 3.2 AI 调用链路

`POST /api/v1/ai/chat`（SSE）→ 读取用户选择的模型配置（OpenAI 兼容 base_url + AES-GCM 加密 API Key）→ 把**当前用户有权限**的工具（Function Calling）一并发给模型 → 模型请求调用工具：

- 只读工具（如 `promql_query`、`logs_query`、`k8s_list_pods`）直接执行，结果回填给模型；
- 写工具（`k8s_scale_deployment`、`hosts_exec`、`cicd_run_pipeline`）推送 `tool_confirmation` 事件，用户点「确认」后才执行，且生产环境仍走审批。

工具注册约束（`server/internal/ai/tools.go:54-74`）：名称必须匹配 `^[a-zA-Z0-9_-]{1,64}$`、必须声明权限、参数必须是合法 JSON Schema、禁止重名。

| 工具 | 权限 | 需确认 |
| --- | --- | --- |
| `search_audit_logs` | `audit:read` | 否 |
| `k8s_list_pods` | `workload:read` | 否 |
| `k8s_get_events` | `workload:read` | 否 |
| `k8s_scale_deployment` | `workload:write` | **是** |
| `hosts_list` | `host:read` | 否 |
| `hosts_exec` | `host:exec` | **是** |
| `promql_query` | `metrics:read` | 否 |
| `logs_query` | `logs:read` | 否 |
| `alerts_list` | `alert:read` | 否 |
| `cicd_list_runs` | `cicd:read` | 否 |
| `cicd_run_pipeline` | `cicd:run` | **是** |
| `kafka_consumer_lag` / `mq_overview` | `middleware:read` | 否 |

另有 `POST /api/v1/ai/assist`（场景化生成，供页面内「AI 生成」按钮使用，已覆盖告警规则、创建工作负载、创建用户、部门/用户组、接入集群等对话框）与 `POST /api/v1/ai/models/discover`、`POST /api/v1/ai/models/test`。

## 4. 各功能的数据来源与执行方式

| 功能 | 数据从哪来 | 页面操作如何下发执行 | 后端模块 |
|---|---|---|---|
| 登录 / 用户 / 角色 / 部门 / 用户组 | PostgreSQL | bcrypt 校验、JWT + 轮换刷新令牌、失败锁定、首次强制改密 | auth / users / rbac |
| 集群、节点、命名空间、工作负载、事件 | K8s API Server（实时调用）；节点用量来自 metrics-server | client-go：cordon / drain（Eviction API）/ 标签污点 patch / scale / rollout restart / server-side apply | cluster |
| 告警规则 | 集群内 `PrometheusRule` 自定义资源 | `GET/POST /clusters/:id/alert-rules`，直接读写 CR | cluster |
| 主机 | matrix-agent 上报的主机信息与心跳 | 服务端经 WebSocket 下发命令，Agent 用 `/bin/sh -c` 执行并回传输出；支持批量、文件下发 | hosts |
| 堡垒机 | 资产、账号（加密）、授权规则存 PostgreSQL | 浏览器 WebSocket ⇄ 服务端 SSH 代理 ⇄ 目标主机；逐行命令过滤（拒绝/确认）、asciicast 录像、命令审计 | bastion |
| 日志检索 | Loki（LogQL）或 Elasticsearch（KQL→query_string）；采集端用 Fluent Bit / Vector / Filebeat | 统一 `POST /observability/logs/query`，结果归一为 `{ts, level, service, host, message, labels}` | observability |
| 监控 | Prometheus HTTP API（node-exporter、kube-state-metrics、业务 exporter） | query / query_range / labels / label values / series / rules 代理与校验 | observability |
| 告警 | Alertmanager Webhook 推送到平台 | 认领、关闭；静默写回 Alertmanager `/api/v2/silences`；钉钉/飞书/企微/邮件通知 | observability |
| 链路追踪 | Jaeger `/api/traces` 或 Tempo `/api/search`（应用经 OpenTelemetry 上报） | 前端接入服务、检索、详情与资源标签；缺少共享集群标签时返回空结果 | observability |
| CI/CD | 应用、流水线、运行记录存 PostgreSQL | 每个阶段创建一个 K8s Job（git-clone 初始化容器 + Kaniko 构建 / 测试脚本 / Helm），日志实时写库并以 SSE 推送；部署阶段 patch Deployment 镜像并等待滚动完成；生产部署走审批；支持回滚、Jenkins 触发、GitLab/GitHub Webhook | cicd |
| 中间件 | Kafka Admin API、RabbitMQ Management API、RocketMQ Dashboard REST | Topic/队列创建删除（生产需审批）、消费组 Lag | middleware |
| 数据库控制台 | MySQL / PostgreSQL / Redis 直连 | SQL 风险识别（DROP/TRUNCATE/无 WHERE 的 DELETE/UPDATE 等）→ 生产走审批；每条语句写入审计表；Redis 高危命令拦截 | middleware |
| 审批 | PostgreSQL | 各模块注册审批类型，审批通过后自动执行 | approval |
| 审计 | 审计中间件自动记录全部写操作 | 查询、AI 检索 | audit |
| 平台设置 | PostgreSQL（API Key AES-GCM 加密） | 访问域名保存时自动创建/更新 Ingress；公告、运行环境信息 | settings / platform |
| AI 模型与提示词 | PostgreSQL | OpenAI 兼容接口代理 + 工具调用 | ai |
| 自动化编排 | PostgreSQL | 流程/版本/触发写库；`trigger` 只写 `queued`，**执行器待补** | automation |
| 研发中心 | PostgreSQL | 需求/迭代/缺陷/测试的列表、创建、更新；无状态机与关联 | devcenter |

## 5. 接口文档

统一前缀 `/api/v1`，统一响应：

```json
{ "code": 0, "message": "ok", "data": { }, "request_id": "..." }
```

- 认证：`Authorization: Bearer <access_token>`；WebSocket 可用 `?access_token=`。
- 分页：`page`、`page_size`，返回 `{items, total, page, page_size}`。
- 需要审批：HTTP 202，`code = 40304`，`data` 为审批单。
- 首次登录未改密：403 / `40301`，仅放行改密接口。

| 文档 | 内容 |
|---|---|
| [docs/api/core.md](api/core.md) | 认证、用户、角色与权限、环境策略、系统设置、审计、审批、AI、健康检查（含真实请求/响应示例） |
| [docs/api/cluster.md](api/cluster.md) | 集群、节点、命名空间、工作负载、日志、事件、YAML 应用、告警规则 |
| [docs/api/hosts.md](api/hosts.md) | 主机、注册令牌、Agent 协议、命令执行、批量执行、文件下发 |
| [docs/api/bastion.md](api/bastion.md) | 资产、账号、授权、命令过滤、SSH 终端 WebSocket、会话与录像 |
| [docs/api/observability.md](api/observability.md) | 数据源、Prometheus、日志查询、告警 Webhook、静默、链路、通知渠道 |
| [docs/api/cicd.md](api/cicd.md) | 运行目标、应用、流水线、运行、日志、部署、回滚、Webhook、Jenkins |
| [docs/api/middleware.md](api/middleware.md) | 中间件实例、Kafka、RabbitMQ、RocketMQ、SQL/Redis 控制台、SQL 审计 |
| [docs/api/automation-devcenter.md](api/automation-devcenter.md) | 自动化编排、研发中心 |
| [docs/dev-guide.md](dev-guide.md) | 新增模块、迁移、权限、审批、AI 工具的开发规范 |
| [docs/backend-architecture.md](backend-architecture.md) | 后端接入方案（初版设计说明） |
| [docs/PROGRESS.md](PROGRESS.md) | 进度报告：逐页接线矩阵、已完成/部分完成/未开始 |
| [docs/OUTSTANDING.md](OUTSTANDING.md) | 未完成 / 未落实清单：v1.18 发布状态、浏览器实测、缓存与功能缺口 |
| [docs/fix-stale-image-uncommitted-code.md](fix-stale-image-uncommitted-code.md) | 集群镜像混入未提交代码的排查与重建（v1.61 → v1.62） |
| [docs/bastion-product-patterns.md](bastion-product-patterns.md) | 堡垒机产品模式与实现边界（资产来源、协议字段、SSH 契约、未验证清单） |
| [docs/bastion-round-fix-notes.md](bastion-round-fix-notes.md) | 堡垒机与登录页 5 个问题的四段式复盘 |
| [docs/deploy-v1.63-bastion-workbench.md](deploy-v1.63-bastion-workbench.md) | v1.63 镜像构建、三节点分发、滚动与回滚记录 |
| [docs/deploy-192.168.6.3.md](deploy-192.168.6.3.md) | 真实集群部署与验收记录（7 个真实故障与解法） |

## 6. 已实现（截至 v1.18）

**后端（真实代码，可编译、单测通过）**

- 服务入口与路由装配、配置（YAML + 15 个 `MATRIX_*` 环境变量）、按模块版本化迁移、优雅退出、`/healthz`、`/api/v1/health`、`/metrics`、`/api/v1/version`。
- **237 条路由**，分布：cluster 27、hosts 16、bastion 25、observability 31、cicd 26、middleware 26、automation 8、devcenter 12、AI 19、auth 7、rbac 8、users 17（含部门 4、用户组 4）、settings 5、platform 6、audit 2、approval 7、server 4。
- 核心：认证（JWT + 刷新轮换 + 失败锁定 + 限流）、RBAC（50 个权限点 + 角色 + 环境策略）、用户 / 部门 / 用户组、系统设置、平台层（访问域名 + 运行环境 + 公告）、审计、审批（7 类，通过后异步执行）、AI（模型 / 提示词 / 工具 / 对话 / 场景化生成）。
- 8 个业务模块：cluster、hosts（含 matrix-agent）、bastion、observability、cicd、middleware、automation、devcenter。
- 数据库：**34 张表、13 个迁移文件**（core 5 个 + 8 个模块各 1 个）。

**前端**

- 登录页、真实 JWT 登录/刷新/改密/退出，独立 API 客户端 `public/xs-api.js`（base 探测、401 单飞刷新、SSE/WS）。
- v1.17 的 19/1/8 页面统计仍作为历史基线；v1.18 新增 trace 真实接线、网络 Service/NetworkPolicy 只读视图和 CICD 应用 inventory，traffic/netpol/bastion 仍是部分接线，oncall 后端仍缺失。
- 演示模式：`XS.mode === "live"` 才接管渲染器；后端不可用时保留原演示渲染器并显示「演示模式」徽标。

**已完成真实联调**

- 真实 PostgreSQL 16 启动：全部迁移执行成功；登录 → 强制改密 → 用户/角色/审计/审批/AI 接口返回正常。
- matrix-agent 真实注册上线，平台下发 `uname -a && uptime` 成功返回输出并入库。
- 真实 K8s 集群 `192.168.6.3`（三节点）部署：`xingshu-matrix 2/2 Running`，`https://xingshu-matrix.com` 入口，
  登录 / `/auth/me` / `/users` / `/roles` / `/clusters` / `/hosts` / `/approvals` / `/audit-logs` 全部 200。

## 7. 未实现 / 未验证，以及实现方法

| 项目 | 现状 | 实现方法 |
|---|---|---|
| oncall 值班 / 故障 / SLO 后端 | **前端已写完并调用 `/oncall/*`，后端无该模块（404）** | 新增 `internal/modules/oncall`：排班（分层轮值、代班）、故障单（声明/认领/升级/解决）、复盘、SLO 与错误预算；`approval` 注册 `oncall.change` |
| 堡垒机控制台前端接线 | 后端 25 条路由完整，前端仍部分接线 | 继续接入账号、授权、过滤、审计和协议代理 |
| 链路追踪前端接线 | v1.18 已接服务、检索、详情和资源标签 | 继续补共享数据源标签校验与多集群端到端验证 |
| 流量治理 / 网络策略 / 多集群 | traffic/netpol 仅部分真实只读；多集群无聚合接口 | 补 Istio/Cilium 写接口和多集群扇出 |
| 研发中心完整流程 | 基础 CRUD；`requirements`/`workbench`/`testing` 仍读前端本地数据 | 按资源定义状态机（需求：提出→受理→评审→开发→测试→验收→上线），补 `comments`、`links`（Git/流水线）、测试计划与用例表 |
| 自动化编排执行器 | `trigger` 只写 `queued`，无 worker | 新增 `automation_runner`：受控步骤（HTTP / 等待 / 主机命令 / K8s 操作 / 审批），失败重试与超时；或接 Tekton / Argo Workflows |
| 真实外部依赖联调 | 仅 httptest 假服务与 client-go fake | 备一套测试环境（kind/k3s + kube-prometheus-stack + Loki + Alertmanager + Jenkins + Kafka），按 `scripts/smoke-test.sh` 扩展端到端用例 |
| 集群内 Agent 隧道 / Web Shell / 端口转发 | 仅 kubeconfig 直连 | 增加 `matrix-cluster-agent`（集群内 Deployment，反向 WebSocket 隧道）；exec 用 `remotecommand.NewSPDYExecutor` 桥接 WebSocket；端口转发用 `portforward` |
| Service / Ingress / ConfigMap / Secret / PVC / HPA 专用接口 | 只能通过 YAML apply | 在 cluster 模块按资源类型补 list/get/patch，复用 dynamic client |
| 堡垒机 RDP / VNC / 数据库协议 | 仅资产登记与 guacd 健康检查 | 服务端实现 Guacamole 协议 WebSocket 隧道 → guacd；前端接 guacamole-common-js |
| 录像存 S3/MinIO、多副本会话 | 录像存本地，在线会话存内存 | 会话结束后上传 MinIO；会话踢出/列表用 Redis Pub/Sub 广播 |
| Agent 多副本路由 | Agent 连接只在单实例内存 | Redis 记录 host→实例映射，跨实例转发（或 NATS） |
| 告警认领/关闭回写 Alertmanager、通知重试与聚合 | 只在平台内记录，通知只发一次 | 认领时创建对应静默；通知加重试队列与按标签聚合 |
| CI/CD 断点续跑、阶段共享工作区、并行阶段 | 重启后运行标记失败；每阶段重新克隆 | PVC 共享工作区；阶段 DAG 调度；或接 Tekton / Argo Workflows |
| 单点登录（LDAP / OIDC） | 前端有「企业 SSO」入口，后端无实现 | auth 增加 OIDC（coreos/go-oidc）与 LDAP 绑定，首次登录自动建用户并按组映射角色 |
| 多副本权限缓存 | 每副本 15 秒缓存 | 角色变更时经 Redis Pub/Sub 广播失效 |
| Helm Chart / 高可用 | 只有 `helm-values.example.yaml` 示例；Ingress 单节点 hostNetwork；PostgreSQL 单副本 + `local-path` | 补 chart；Ingress 多副本 + PDB；PostgreSQL 主从或接托管数据库 |
| 压测、安全审计、备份恢复演练 | 未做 | k6 压测、govulncheck、PostgreSQL 定时备份与恢复演练 |
| core 包单测 | auth/rbac/users/settings/approval/ai/audit 无 `_test.go` | 按模块补 httptest 用例，覆盖 401/403/202/409 分支 |

## 8. 本地运行与部署

```bash
# 开发（Go 1.23+）
cd server && MATRIX_MODE=dev MATRIX_DB_DSN=postgres://matrix:matrix@127.0.0.1:5432/matrix?sslmode=disable \
  MATRIX_STATIC_DIR=../public go run ./cmd/matrix-server

# Docker Compose（server + PostgreSQL + Redis + MinIO + guacd）
cp .env.example .env   # 填写 MATRIX_JWT_SECRET、MATRIX_ENCRYPTION_KEY
docker compose up -d

# 主机接入：平台「主机 → 注册令牌」生成命令后在目标机执行
matrix-agent -server https://matrix.example.com -token mxe_xxx
```

`MATRIX_MODE=dev` 时缺失密钥会自动生成临时值；生产必须显式提供：

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `MATRIX_JWT_SECRET` | 是 | ≥32 字符 |
| `MATRIX_ENCRYPTION_KEY` | 是 | 32 字节 base64/hex，`openssl rand -base64 32` |
| `MATRIX_DB_DSN` | 是 | PostgreSQL 连接串 |
| `MATRIX_ADMIN_INITIAL_PASSWORD` | 否 | 默认 `Xingshu@2026`，首次登录强制改密 |
| `MATRIX_LISTEN` / `MATRIX_STATIC_DIR` / `MATRIX_MODE` | 否 | 默认 `:8080` / `/app/public` / `prod` |
| `MATRIX_REDIS_ADDR` / `MATRIX_REDIS_PASSWORD` / `MATRIX_REDIS_DB` | 否 | 留空禁用 Redis |
| `MATRIX_CORS_ORIGINS` / `MATRIX_TRUSTED_PROXIES` | 否 | 同域部署留空 |
| `MATRIX_DB_AUTO_MIGRATE` / `MATRIX_LOG_LEVEL` / `MATRIX_LOG_FORMAT` | 否 | 默认 `true` / `info` / `json` |

首次登录：`admin / Xingshu@2026`（或 `MATRIX_ADMIN_INITIAL_PASSWORD`），登录后强制修改密码。

### 8.1 版本与发布

```bash
# 1) 打标签（应用版本；发布时执行）
git tag -a v1.18 -m "v1.18"
git push origin v1.18

# 2) 构建镜像（版本号写入二进制，GET /api/v1/version 可见）
docker build --build-arg VERSION=v1.18 --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t xingshu/matrix-server:v1.18 .

# 3) 离线环境：导出 OCI tar
ctr -n k8s.io images export matrix-server-v1.18.tar.gz docker.io/xingshu/matrix-server:v1.18
```

镜像 tag 有多套历史口径（当前集群 `v1.63` / 脚本默认 `v1.3` / 离线包 `v40`），
v1.16 起约定以**应用版本**为镜像 tag，但现网仍用构建序号，差异与治理建议见 [PROGRESS.md §9.1](PROGRESS.md)。
历史对应关系：应用版本 `v1.17` ↔ 镜像 `v1.63` ↔ commit `f9cd41e`；v1.18 尚未构建或部署。

## 9. 质量门禁

```bash
cd server && go build ./... && go vet ./...      # 编译与静态检查
cd server && go test ./...                       # 单测：7 个包（含 cmd/matrix-agent）
node --test tests/frontend/xs-api.test.mjs tests/frontend/xs-observability.test.mjs # API 与可观测性 14 条
node public/xs-live-core.js                      # 核心适配层 48 条断言
node --check public/xs-live-ops.js               # 语法检查（ops 无 Node 自测，依赖 window）
```

CI 参数：`.github/workflows/docker.yml`，Go 1.23 + Node 内置 `node:test`（无第三方依赖），
push `main` / 打 `v*` / 手工触发；通过后推送 `ghcr.io/<owner>/xingshu-matrix:latest`。

## 10. 版本记录

| 版本 | 时间 | 主题 |
| --- | --- | --- |
| v1.18 | 2026-10-24 | 真实 trace 与环境范围、网络只读接口、应用运行清单；beta / 标签 / Release 已发布，未部署 |
| v1.17 | 2026-10-21 | 堡垒机访问工作台与真实 SSH 会话；资产协议参数持久化；登录可访问性；镜像 v1.63（历史部署） |
| v1.16 | 2026-10-20 | 文档与版本对齐；修复 CI 引用不存在脚本；新增 PROGRESS / CHANGELOG |
| v1.11–v1.15 | 2026-10-19/20 | IAM 部门与用户组、AI 对话框全覆盖、告警规则走 PrometheusRule、Istio 网关、镜像对齐 v1.34 |
| v1.4–v1.10 | 2026-10-18 | 可视化编辑器、堡垒机编辑类页面、集群/主机向导、登录页视觉收敛 |
| v1.1–v1.3 | 2026-10-17 | Go 后端落地、接口文档、三种部署方式、首次真实集群部署 |
| v1.0 | 2026-10-15 | 前端原型（单页 + 部署文件） |
