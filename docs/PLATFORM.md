# 星枢 Matrix 平台技术说明

> 本文介绍平台架构、组件边界、资源接口与开发入口。当前文档对应 v1.19，逐版本变化见 [CHANGELOG.md](../CHANGELOG.md)，接口索引见 [docs/api/](api/)。

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
| 数据库 | PostgreSQL 14+（pgx/v5） | 元数据、权限、审批、审计与任务记录；迁移位于各模块目录 |
| 缓存 | Redis 7（go-redis v9，可选） | 健康检查已接入；多副本会话/锁待扩展 |
| 对象存储 | MinIO / S3（规划） | 堡垒机录像、制品；当前录像存本地目录 |
| K8s | client-go v0.31 | 集群管理、CI 任务（Job）、部署、`PrometheusRule` 读写 |
| 前端 | 原生 HTML/CSS/JavaScript 单页应用 | `public/index.html`，hash 路由，无构建依赖 |
| 前端适配层 | `public/xs-api.js` + `xs-live-core.js` + `xs-live-ops.js` | API 客户端与实时渲染接管，后端不可用时回退演示渲染 |
| 部署 | Docker 多阶段镜像、docker-compose、K8s 清单、Istio 网关、物理机脚本 | 见 `Dockerfile`、`docker-compose.yml`、`deploy/` |

Kubernetes 资源 API 覆盖集群、节点、工作负载、autoscaling/v2 HPA、batch/v1 CronJob、Service、Ingress、NetworkPolicy、存储卷、VolumeSnapshot、ConfigMap 与 Secret；写操作由环境权限、审批策略和审计链路统一约束。平台还包含 kubeconfig 接入、K3s 主机安装任务、多集群资源分发、Helm/Argo CD/Jenkins 集成与受限的 Ansible 自动化。能力与边界以各接口文档和 [v1.19 更新记录](../CHANGELOG.md#v119--2026-10-27) 为准。

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
   │    automation     ──► 流程定义/版本/运行记录与受控 Ansible worker
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
| 自动化编排 | PostgreSQL、平台内嵌 Ansible | 流程版本、运行记录和受控主机任务；生产任务按环境策略审批后执行 | automation |
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
| [docs/PROGRESS.md](PROGRESS.md) | v1.18 历史页面接线快照 |
| [docs/OUTSTANDING.md](OUTSTANDING.md) | v1.18 历史问题与发布记录 |
| [docs/fix-stale-image-uncommitted-code.md](fix-stale-image-uncommitted-code.md) | 集群镜像混入未提交代码的排查与重建（v1.61 → v1.62） |
| [docs/bastion-product-patterns.md](bastion-product-patterns.md) | 堡垒机产品模式与实现边界（资产来源、协议字段、SSH 契约、未验证清单） |
| [docs/bastion-round-fix-notes.md](bastion-round-fix-notes.md) | 堡垒机与登录页 5 个问题的四段式复盘 |
| [docs/deploy-v1.63-bastion-workbench.md](deploy-v1.63-bastion-workbench.md) | v1.63 镜像构建、三节点分发、滚动与回滚记录 |
| [docs/deploy-192.168.6.3.md](deploy-192.168.6.3.md) | 真实集群部署与验收记录（7 个真实故障与解法） |

## 6. Kubernetes 运维能力

平台以集群、环境和命名空间组织 Kubernetes 日常操作，当前覆盖：

- 集群接入、集群与节点查看、节点标签分组及批量标签和污点维护。
- Deployment、StatefulSet、DaemonSet、Job、CronJob、Pod 等工作负载；HPA 状态与配置。
- Service、Ingress、NetworkPolicy、ConfigMap、Secret、PersistentVolume、PersistentVolumeClaim 和 StorageClass。
- 集群支持卷快照 API 与快照驱动时，可查看 VolumeSnapshot 并从就绪快照创建新 PVC。
- 面向多个集群逐项分发资源清单，并分别返回各集群结果。
- Helm、Argo CD 与 Jenkins 应用交付接入；Linux 主机上的 K3s 安装任务。
- 告警、日志、指标和链路视图，以及自然语言配置草稿和告警诊断建议。

所有集群写操作都经过资源权限、环境策略、审批和审计流程。Secret 页面只展示元数据与键名，不回显值。

## 7. 使用边界

- VolumeSnapshot 能力取决于目标集群是否安装对应 CRD 与可用的 CSI 快照驱动；不具备条件时平台会说明原因，不生成模拟快照。
- NetworkPolicy 页面描述 Kubernetes 策略声明；具体流量是否被允许还取决于集群 CNI 的策略执行能力。
- 拓扑中的调用连线来自已检索到的 trace 数据，不代表未观测到的流量不存在。
- APISIX 路由视图只在集群已安装并接入相应 API 时提供；外部入口还取决于 Ingress Controller 或网关配置。
- Jenkins、Argo CD、Helm 仓库、Prometheus、Loki、Alertmanager 和 AI 模型需要配置可访问的服务地址及凭据。
- K3s 高可用安装要求预先准备 API 负载均衡地址；平台不代替云服务创建负载均衡器或 VIP。
- 多集群资源分发按集群逐项执行；发生失败时，已成功的集群不会自动回滚，需核对结果后处理。
- AI 输出用于配置草稿或处置建议。对资源的实际变更仍由工程师确认，并遵循相应环境审批策略。

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
# 按当前发布版本构建镜像
docker build --build-arg VERSION=v1.19 --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t xingshu/matrix-server:v1.19 .

# 离线环境：导出 OCI tar
ctr -n k8s.io images export matrix-server-v1.19.tar.gz docker.io/xingshu/matrix-server:v1.19
```

应用版本与部署镜像 tag 可由发布流程分别管理。构建时传入 `VERSION` 后，`GET /api/v1/version` 会返回二进制记录的应用版本。

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
| v1.19 | 2026-10-27 | Kubernetes 存储、网络、配置、节点、伸缩和定时任务；多集群、应用交付与诊断体验完善 |
| v1.18 | 2026-10-24 | 真实 trace 与环境范围、网络只读接口、应用运行清单；beta / 标签 / Release 已发布，未部署 |
| v1.17 | 2026-10-21 | 堡垒机访问工作台与真实 SSH 会话；资产协议参数持久化；登录可访问性；镜像 v1.63（历史部署） |
| v1.16 | 2026-10-20 | 文档与版本对齐；修复 CI 引用不存在脚本；新增 PROGRESS / CHANGELOG |
| v1.11–v1.15 | 2026-10-19/20 | IAM 部门与用户组、AI 对话框全覆盖、告警规则走 PrometheusRule、Istio 网关、镜像对齐 v1.34 |
| v1.4–v1.10 | 2026-10-18 | 可视化编辑器、堡垒机编辑类页面、集群/主机向导、登录页视觉收敛 |
| v1.1–v1.3 | 2026-10-17 | Go 后端落地、接口文档、三种部署方式、首次真实集群部署 |
| v1.0 | 2026-10-15 | 前端原型（单页 + 部署文件） |
