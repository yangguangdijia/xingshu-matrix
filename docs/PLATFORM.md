# 星枢 Matrix 平台技术说明

> 本文回答：用什么语言开发、整体架构、项目背景、业务逻辑与接口如何实现、接口文档在哪、哪些已经实现、哪些还没实现以及怎么实现。
> 状态以 GitHub 仓库当前提交为准。**当前版本为预览版，尚未在真实生产环境完成联调与压测，请勿直接用于生产。**

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
| 后端 API | **Go 1.23**，Gin | `server/cmd/matrix-server`，单二进制 |
| 主机 Agent | **Go 1.23**，gorilla/websocket，gopsutil | `server/cmd/matrix-agent`，Linux 常驻进程 |
| 数据库 | PostgreSQL 14+（pgx/v5） | 元数据、权限、审批、审计、任务、日志片段 |
| 缓存 | Redis 7（go-redis v9，可选） | 健康检查已接入；多副本会话/锁待扩展 |
| 对象存储 | MinIO / S3（规划） | 堡垒机录像、制品；当前录像存本地目录 |
| K8s | client-go v0.31 | 集群管理、CI 任务（Job）、部署 |
| 前端 | 原生 HTML/CSS/JavaScript 单页应用 | `public/index.html`，hash 路由，无构建依赖 |
| 部署 | Docker 多阶段镜像、docker-compose、K8s 清单、物理机脚本 | 见 `Dockerfile`、`docker-compose.yml`、`deploy/` |

## 3. 总体架构

```
浏览器 (public/index.html SPA)
   │  HTTPS  /api/v1/*   (Bearer JWT；SSE 流式；WebSocket 终端)
   ▼
matrix-server (Go)
   ├─ 通用中间件：RequestID → Recover → CORS → 访问日志 → Prometheus 指标 → 审计
   ├─ 核心：auth / rbac / users / settings / audit / approval / ai
   ├─ 业务模块（internal/modules/*，统一实现 app.Module 接口）
   │    cluster        ──► Kubernetes API Server（kubeconfig，client-go）
   │    hosts          ◄── matrix-agent（WebSocket 长连接，JSON 帧）
   │    bastion        ──► 目标主机 SSH（x/crypto/ssh），guacd（RDP/VNC 规划）
   │    observability  ──► Prometheus / Loki / Elasticsearch / Jaeger / Tempo / Alertmanager
   │                   ◄── Alertmanager Webhook（公开接口 + Token）
   │    cicd           ──► K8s Job（git-clone + Kaniko / 测试 / Helm），Jenkins REST
   │                   ◄── GitLab / GitHub Webhook
   │    middleware     ──► Kafka Admin（franz-go）/ RabbitMQ Management API / RocketMQ Dashboard / MySQL / PostgreSQL / Redis
   └─ PostgreSQL（每个模块独立迁移，表名带模块前缀）
```

### 3.1 一次写操作的完整链路（以「生产环境扩容 Deployment」为例）

1. 前端 `POST /api/v1/clusters/3/namespaces/prod/deployments/order/scale {replicas: 6}`。
2. `auth.Middleware` 校验 JWT → 加载用户角色/权限/环境范围（Principal，15 秒缓存）。
3. `rbac.RequirePerm("workload:write")` 校验权限；模块再校验用户是否有该集群环境（prod）的范围。
4. 模块调用 `Approval.Gate`：读取环境策略，prod `require_approval=true` → 创建审批单 `cluster.change`，返回 **HTTP 202 / code 40304**。
5. 审批人 `POST /api/v1/approvals/:id/approve` → 审批服务异步执行模块注册的 Handler：重新加载集群、复核申请人权限、调用 K8s Scale 子资源。
6. 审计中间件记录每一次写请求（用户、IP、路径、资源、结果、耗时）；审批单记录执行结果。

### 3.2 AI 调用链路

`POST /api/v1/ai/chat`（SSE）→ 读取用户选择的模型配置（OpenAI 兼容 base_url + 加密 API Key）→ 把**当前用户有权限**的工具（Function Calling）一并发给模型 → 模型请求调用工具：
- 只读工具（如 `promql_query`、`logs_query`、`k8s_list_pods`）直接执行，结果回填给模型；
- 写工具（如 `k8s_scale_deployment`、`hosts_exec`、`cicd_run_pipeline`）推送 `tool_confirmation` 事件，用户点「确认」后才执行，且生产环境仍走审批。

已注册工具：`search_audit_logs`、`k8s_list_pods`、`k8s_get_events`、`k8s_scale_deployment`、`hosts_list`、`hosts_exec`、`promql_query`、`logs_query`、`alerts_list`、`cicd_list_runs`、`cicd_run_pipeline`、`kafka_consumer_lag`、`mq_overview`。

## 4. 各功能的数据来源与执行方式

| 功能 | 数据从哪来 | 页面操作如何下发执行 | 后端模块 |
|---|---|---|---|
| 登录 / 用户 / 角色 | PostgreSQL | bcrypt 校验、JWT + 轮换刷新令牌、失败锁定、首次强制改密 | auth / users / rbac |
| 集群、节点、命名空间、工作负载、事件 | K8s API Server（实时调用）；节点用量来自 metrics-server | client-go：cordon / drain（Eviction API）/ 标签污点 patch / scale / rollout restart / server-side apply | cluster |
| 主机 | matrix-agent 上报的主机信息与心跳 | 服务端经 WebSocket 下发命令，Agent 用 `/bin/sh -c` 执行并回传输出；支持批量、文件下发 | hosts |
| 堡垒机 | 资产、账号（加密）、授权规则存 PostgreSQL | 浏览器 WebSocket ⇄ 服务端 SSH 代理 ⇄ 目标主机；逐行命令过滤（拒绝/确认）、asciicast 录像、命令审计 | bastion |
| 日志检索 | Loki（LogQL）或 Elasticsearch（KQL→query_string）；采集端用 Fluent Bit / Vector / Filebeat | 统一 `POST /observability/logs/query`，结果归一为 `{ts, level, service, host, message, labels}` | observability |
| 监控 | Prometheus HTTP API（node-exporter、kube-state-metrics、业务 exporter） | query / query_range / labels / series / rules 代理与校验 | observability |
| 告警 | Alertmanager Webhook 推送到平台 | 认领、关闭；静默写回 Alertmanager `/api/v2/silences`；钉钉/飞书/企微/邮件通知 | observability |
| 链路追踪 | Jaeger `/api/traces` 或 Tempo `/api/search`（应用经 OpenTelemetry 上报） | 检索与详情归一化 | observability |
| CI/CD | 应用、流水线、运行记录存 PostgreSQL | 每个阶段创建一个 K8s Job（git-clone 初始化容器 + Kaniko 构建 / 测试脚本 / Helm），日志实时写库并以 SSE 推送；部署阶段 patch Deployment 镜像并等待滚动完成；生产部署走审批；支持回滚、Jenkins 触发、GitLab/GitHub Webhook | cicd |
| 中间件 | Kafka Admin API、RabbitMQ Management API、RocketMQ Dashboard REST | Topic/队列创建删除（生产需审批）、消费组 Lag | middleware |
| 数据库控制台 | MySQL / PostgreSQL / Redis 直连 | SQL 风险识别（DROP/TRUNCATE/无 WHERE 的 DELETE/UPDATE 等）→ 生产走审批；每条语句写入审计表；Redis 高危命令拦截 | middleware |
| 审批 | PostgreSQL | 各模块注册审批类型，审批通过后自动执行 | approval |
| 审计 | 审计中间件自动记录全部写操作 | 查询、AI 检索 | audit |
| AI 模型与提示词 | PostgreSQL（API Key AES-GCM 加密） | OpenAI 兼容接口代理 + 工具调用 | ai |

## 5. 接口文档

统一前缀 `/api/v1`，统一响应：

```json
{ "code": 0, "message": "ok", "data": { }, "request_id": "..." }
```

- 认证：`Authorization: Bearer <access_token>`；WebSocket 可用 `?access_token=`。
- 分页：`page`、`page_size`，返回 `{items, total, page, page_size}`。
- 需要审批：HTTP 202，`code = 40304`，`data` 为审批单。

| 文档 | 内容 |
|---|---|
| [docs/api/core.md](api/core.md) | 认证、用户、角色与权限、环境策略、系统设置、审计、审批、AI、健康检查（含真实请求/响应示例） |
| [docs/api/cluster.md](api/cluster.md) | 集群、节点、命名空间、工作负载、日志、事件、YAML 应用 |
| [docs/api/hosts.md](api/hosts.md) | 主机、注册令牌、Agent 协议、命令执行、批量执行、文件下发 |
| [docs/api/bastion.md](api/bastion.md) | 资产、账号、授权、命令过滤、SSH 终端 WebSocket、会话与录像 |
| [docs/api/observability.md](api/observability.md) | 数据源、Prometheus、日志查询、告警 Webhook、静默、链路、通知渠道 |
| [docs/api/cicd.md](api/cicd.md) | 运行目标、应用、流水线、运行、日志、部署、回滚、Webhook、Jenkins |
| [docs/api/middleware.md](api/middleware.md) | 中间件实例、Kafka、RabbitMQ、RocketMQ、SQL/Redis 控制台、SQL 审计 |
| [docs/dev-guide.md](dev-guide.md) | 新增模块、迁移、权限、审批、AI 工具的开发规范 |
| [docs/backend-architecture.md](backend-architecture.md) | 后端接入方案（初版设计说明） |

## 6. 已实现（截至本次提交）

**后端（真实代码，可编译、单元测试通过）**
- 服务入口与路由装配、配置（YAML + `MATRIX_*` 环境变量）、数据库迁移（按模块版本化）、优雅退出、`/healthz`、`/api/v1/health`、`/metrics`。
- 核心：认证、RBAC、用户、角色、环境策略、系统设置、审计、审批、AI 模型/提示词/对话/工具调用。
- 8 个业务模块：cluster、hosts（含 matrix-agent）、bastion、observability、cicd、middleware、automation、devcenter。

**已做过的真实联调**
- 在真实 PostgreSQL 16 上启动 matrix-server：全部迁移执行成功，登录 → 强制改密 → 用户/角色/审计/审批/AI 接口全部返回正常；6 个模块的列表与创建接口可用。
- matrix-agent 真实注册到服务端并上线，平台下发 `uname -a && uptime` 成功返回输出并入库。

**前端**
- 登录页、真实 JWT 登录/刷新/改密/退出，以及独立 API 客户端 `public/xs-api.js`。
- `public/xs-live-core.js` 与 `public/xs-live-ops.js` 在 `XS.mode === "live"` 时按路由接管真实接口：总览、用户权限、审批、集群/节点/工作负载/日志、主机、团队协作、可观测/日志、CI/CD、中间件、值班/SLO、设置、AI、自动化编排。
- 后端不可用时保留原演示渲染器；流量治理、网络策略、堡垒机控制台、部分集群高级子页仍使用演示数据。

## 7. 未实现 / 未验证，以及实现方法

| 项目 | 现状 | 实现方法 |
|---|---|---|
| 前端对接真实接口 | 登录与 18 个核心/运维页已接；流量治理、网络策略、堡垒机控制台、部分集群高级子页仍为演示数据 | 继续按路由补适配器，保持原渲染器在演示模式下不变 |
| 与真实 K8s / Prometheus / Loki / ES / Alertmanager / Jenkins / Kafka 联调 | 仅用 httptest 假服务与 client-go fake 测试 | 准备一套测试集群（kind/k3s + kube-prometheus-stack + Loki + Kafka），按 `scripts/smoke-test.sh` 扩展端到端用例 |
| K8s 集群 Agent 隧道、Web Shell（exec）、端口转发 | 仅支持 kubeconfig 直连 | 增加 `matrix-cluster-agent`（集群内 Deployment，反向 WebSocket 隧道）；exec 用 `remotecommand.NewSPDYExecutor` 桥接 WebSocket |
| Service / Ingress / ConfigMap / Secret / PVC / HPA 专用接口 | 只能通过 YAML apply | 在 cluster 模块按资源类型补 list/get/patch，复用 dynamic client |
| 堡垒机 RDP / VNC / 数据库协议代理 | 仅资产登记与 guacd 健康检查 | 服务端实现 Guacamole 协议 WebSocket 隧道 → guacd；前端接 guacamole-common-js |
| 录像存 S3/MinIO、多副本会话 | 录像存本地，在线会话存内存 | 会话结束后上传 MinIO；会话踢出/列表用 Redis Pub/Sub 广播 |
| Agent 多副本路由 | Agent 连接只在单个实例内存 | 用 Redis 记录 host→实例映射，跨实例转发命令（或 NATS） |
| 告警认领/关闭回写 Alertmanager、通知重试与聚合 | 只在平台内记录 | 认领时创建对应静默；通知加重试队列与按标签聚合 |
| CI/CD 断点续跑、阶段共享工作区、并行阶段 | 重启后运行标记失败；每阶段重新克隆 | 用 PVC 共享工作区；阶段 DAG 调度；或接入 Tekton / Argo Workflows |
| 自动化编排执行器 | automation 模块已有流程/版本/触发/运行记录；trigger 只写 queued，没有 worker | 增加受控步骤执行器（HTTP/等待/主机/K8s/审批），或接 Tekton/Argo |
| 值班排班、SLO 后端 | 前端页面已按 API 预留，但后端暂无 oncall 模块 | 新增 oncall：排班/轮值/代班、故障时间线、SLO 错误预算 |
| 研发中心状态流转与关联 | devcenter 已有基础 CRUD；状态机、评论、Git/流水线关联未实现 | 按资源定义状态机，增加 comments/links 表与接口 |
| 单点登录（LDAP / OIDC） | 仅本地账号 | auth 增加 OIDC（coreos/go-oidc）与 LDAP 绑定，首次登录自动建用户并按组映射角色 |
| 多副本权限缓存 | 每副本 15 秒缓存 | 角色变更时经 Redis Pub/Sub 广播失效 |
| 压测、安全审计、备份恢复演练 | 未做 | 上线前必做：k6 压测、依赖漏洞扫描（govulncheck）、PostgreSQL 定时备份 |

## 8. 本地运行与部署

```bash
# 开发
cd server && MATRIX_MODE=dev MATRIX_DB_DSN=postgres://matrix:matrix@127.0.0.1:5432/matrix?sslmode=disable \
  MATRIX_STATIC_DIR=../public go run ./cmd/matrix-server

# Docker Compose（server + PostgreSQL + Redis + MinIO + guacd）
cp .env.example .env   # 填写 MATRIX_JWT_SECRET、MATRIX_ENCRYPTION_KEY
docker compose up -d

# 主机接入：平台「主机 → 注册令牌」生成命令后在目标机执行
matrix-agent -server https://matrix.example.com -token mxe_xxx
```

首次登录：`admin / Xingshu@2026`（或 `MATRIX_ADMIN_INITIAL_PASSWORD`），登录后强制修改密码。
