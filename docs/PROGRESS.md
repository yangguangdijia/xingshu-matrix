# 星枢 Matrix 项目进度报告（v1.18）

> 报告生成时间：2026-10-24（v1.18 增量）；本文后续未更新的历史统计仍以各节注明的旧基线为准。
> 代码基线：beta 分支当前工作树；应用版本 **v1.18**。本次尚未部署，集群历史镜像记录不能代表 v1.18。
> 判定原则：**只写能在仓库里找到证据的状态**。每条结论附文件路径或命令输出；无法验证的标「未核实」。

---

## 0. v1.18 增量（2026-10-24）

| 项 | v1.17 | v1.18 | 证据 |
| --- | --- | --- | --- |
| 链路追踪 | 后端已有、页面演示 | 前端接入 Jaeger/Tempo 服务、检索、详情与资源标签归一化 | `public/xs-observability.js`、`public/index.html` |
| 集群网络视图 | Service / NetworkPolicy 页面为演示 | 新增只读查询接口和真实数据视图；不提供策略写入或流量治理配置下发 | `cluster/network.go`、`docs/api/cluster.md` |
| 应用运行清单 | 无专用接口 | 新增 CICD 应用 inventory 汇总接口 | `cicd/inventory.go`、`docs/api/cicd.md` |
| 环境与告警 | 环境维度及资源定位存在边界 | 统一环境筛选、集群加载状态、告警资源归属与通知配置展示 | `docs/observability-data-scope.md`、`docs/fix-environment-metrics-notification.md` |
| 验证 | v1.17 历史结果 | Node 前端 API/可观测性测试 14 项及 JS 语法检查通过；Go 测试结果见 CHANGELOG | `CHANGELOG.md` |

v1.17 的历史记录保留如下；逐页矩阵与未完成清单在 v1.18 有局部更新，未覆盖的统计仍是 v1.17 基线，不能视为本版重新盘点。

## 0.1 v1.17 增量（2026-10-21）

这一版只动堡垒机与登录页，没有改集群模块、监控、CI/CD 或外部集成。逐页矩阵里的其它统计仍是 2026-10-20 的口径。

| 项 | v1.16 | v1.17 | 证据 |
| --- | --- | --- | --- |
| 堡垒机前端 | 纯演示（`SEC` 内置数据） | **部分接线**：资产、Web 终端、访问工作台接真实后端 | `public/index.html` 的 `basProbeConnectors` / `basLiveSSH` / `basWorkbench` / `basRenderLiveAssets` |
| 堡垒机 SSH | 无 | **真实 WebSocket**（`/bastion/sessions/ssh/ws`） | `basLiveSSH` 的消息契约与 `ssh.go` 的 `wsMsg` 一致 |
| 堡垒机协议参数 | 只在前端内存 | **持久化到 `bastion_assets.options`** | `0002_asset_options.up.sql`、`assetIn.check` |
| 堡垒机仍为演示的子页 | 全部 | 账号、授权策略、命令过滤、审计、脱敏、数据库（live 下 UI 标注"静态示例数据"） | `BAS_UNWIRED` 与 `basStaticNote` |
| 登录/改密可访问性 | 隐式 label、无 `aria-invalid` | `for`/`id`、`aria-live`、`aria-invalid`、移动端 44px | `public/index.html` 的 `#lu` / `#lp` / `#cu` / `#cn` / `#cc` / `#cerr` |
| 集群内镜像 | `v1.62`（`ac63d0f`） | `v1.63`（`f9cd41e`，`version=v1.17`） | `deploy/k8s/xingshu-matrix-dev.yaml`、`GET /api/v1/version` |
| 堡垒机测试 | 5 个用例 | **12 个用例**（新增 7 个） | `server/internal/modules/bastion/bastion_test.go` |

页面计数口径为 v1.17 历史统计：堡垒机不再计入"纯演示 8 个"，改为"部分接线 1 个"；完全真实的页面仍是 19 个。

---

## 1. 一句话结论

平台从「前端原型 + 预览版后端」推进到了**可在真实 K8s 集群部署、核心运维链路真实可用的内测版**：
后端 8 个业务模块 + 核心 10 个包全部可编译、单测通过（`go build`/`go vet`/`go test` 实测全绿），
v1.17 历史统计为前端 28 个页面中 **19 个真实可用、1 个前端已接但后端缺失、8 个演示**；
v1.18 已改进其中的 trace、network 视图与应用 inventory 接线，但没有重新计算整页百分比。
已有真实集群部署（192.168.6.3 三节点）与真实 HTTP 验收记录，该记录不覆盖本版。

**距离「生产可用」仍有多项工作**：值班/SLO 后端、堡垒机其余子页、网络写入能力，以及真实外部依赖的端到端联调。

---

## 2. 完成度总览

| 维度 | 完成度 | 判定依据 |
| --- | --- | --- |
| 后端 API 与持久化 | **90%** | 237 条路由、34 张表、13 个迁移文件、50 个权限点、7 类审批；全部文件编译通过 |
| 后端单元测试 | **60%** | 7 个包有测试（`cmd/matrix-agent` + bastion/cicd/cluster/hosts/middleware/observability），core 包（auth/rbac/users/settings/approval/ai/audit）无测试 |
| 前端页面接线 | **68%（19/28）** | 28 个路由键中 19 个真实可用；另 1 个（oncall）已由 `xs-live-ops.js` 接管但后端缺失 |
| 前端自动化测试 | **40%** | `xs-api.test.mjs` 5 条用例通过、`xs-live-core.js` 自测 48 条通过；`xs-live-ops.js` 无 Node 自测；浏览器冒烟需手工跑 |
| 部署交付物 | **85%** | Dockerfile / compose / K8s 清单 / Istio 网关 / 物理机脚本 / 离线镜像包齐备；缺 Helm Chart、缺高可用 |
| 真实环境联调 | **45%** | 平台自身链路（登录/RBAC/审计/集群/主机）已在 192.168.6.3 验证；Prometheus/Loki/ES/Jenkins/Kafka 只用假服务测过 |
| CI | **可运行（本轮修复）** | 原工作流引用不存在的 `scripts/smoke-test.mjs`，本轮改为 `go build/vet/test` + `node --test tests/frontend/xs-api.test.mjs` |

---

## 3. 已完成（可以直接对外说「有」）

### 3.1 后端核心（`server/internal/`）

| 能力 | 证据 |
| --- | --- |
| JWT 登录 / 刷新令牌轮换 / 退出 / 改密 / 在线会话列表与踢出 | `server/internal/auth/http.go:73-150` |
| 登录失败锁定（5 次 / 15 分钟）、登录限流（20 次/分钟） | `server/config.example.yaml` → `auth.max_login_failures`、`lockout_duration`、`login_rate_per_minute` |
| RBAC 权限目录、角色 CRUD、环境策略（生产是否强制审批） | `server/internal/rbac/handler.go:30-100` |
| 用户管理（含启用/禁用、重置密码、解锁、角色绑定） | `server/internal/users/users.go:407-492` |
| 部门（含上级部门、改名、备注） | `server/internal/users/departments.go:146-187` |
| 用户组（成员 + 角色聚合） | `server/internal/users/groups.go:140-178`；表 `user_groups` / `user_group_members` / `user_group_roles` |
| 系统设置（键值 + 公开项） | `server/internal/settings/settings.go:156-206` |
| 平台层：访问域名（保存时自动创建/更新 Ingress）、运行环境信息、公告 | `server/internal/platform/domain.go`、`env.go`、`notices.go` |
| 审计日志查询与详情 | `server/internal/audit/store.go:174-188` |
| 审批：类型注册、开单、通过/驳回/取消、通过后异步执行 | `server/internal/approval/http.go:15-80` |
| AI：模型 CRUD / 连通性测试 / 模型发现、提示词 CRUD、工具注册与确认、SSE 对话、`/ai/assist` 场景化生成 | `server/internal/ai/http.go:87-274`、`assist.go:37`、`discover.go:32-100` |
| 健康检查 `/healthz`、`/api/v1/health`（含 DB/Redis 状态）、`/metrics`、`/api/v1/version` | `server/internal/server/server.go:57-110` |
| 按模块版本化迁移、启动自动迁移、独立 `migrate` 子命令 | `server/internal/store/migrate.go`、`server/cmd/matrix-server/main.go` |

### 3.2 业务模块（8 个，全部有真实执行链路）

| 模块 | 路由数 | 关键能力 | 证据 |
| --- | --- | --- | --- |
| `cluster` | 27 | 集群 CRUD/连通性测试、节点 cordon/uncordon/drain/标签/污点、命名空间增删、工作负载列表/详情/日志/事件、scale/restart/删除/YAML apply、告警规则读写（走集群内 `PrometheusRule`） | `modules/cluster/module.go:92-120`、`alertrules.go:179-180` |
| `hosts` | 16 | 主机 CRUD、注册令牌、Agent WebSocket 接入、单机/批量执行、任务流 SSE、文件下发、生产执行走审批 | `modules/hosts/module.go:86-102` |
| `bastion` | 25 | 资产/账号（加密）/授权规则/命令过滤 CRUD、我的资产、SSH 终端 WebSocket、会话列表/详情/命令审计/录像下载/强制断开、guacd 健康检查 | `modules/bastion/module.go:78-109` |
| `observability` | 31 | 数据源 CRUD 与测试、Webhook Token 轮换、Prometheus query/query_range/labels/series/rules、日志查询（Loki/ES）、告警列表/详情/认领/关闭、静默增删、链路服务/检索/详情、通知渠道 CRUD 与测试 | `modules/observability/module.go:78-122` |
| `cicd` | 26 | 执行目标、Jenkins 接入、应用 CRUD/Webhook 密钥轮换/发布记录、部署/回滚（走审批）、流水线 CRUD 与运行、运行详情/SSE 日志/取消、GitLab-GitHub Webhook | `modules/cicd/module.go:112-143` |
| `middleware` | 26 | 实例 CRUD 与测试、概览、Kafka Broker/Topic/消费组、RabbitMQ 概览/节点/队列/交换机/连接、RocketMQ 集群与 Topic、SQL 控制台、Redis 控制台、SQL 审计 | `modules/middleware/module.go:72-104` |
| `automation` | 8 | 流程 CRUD、版本、触发、运行记录、重试、取消 | `modules/automation/module.go:39-46` |
| `devcenter` | 12 | 需求 / 迭代 / 缺陷 / 测试四类对象的列表、创建、更新（`PATCH`） | `modules/devcenter/module.go:30-35` |

### 3.3 AI 能力（嵌入式，非独立聊天框）

- 已注册 **12 个 Function Calling 工具**，其中 **3 个写工具必须用户二次确认**：

| 工具 | 权限 | 是否需确认 |
| --- | --- | --- |
| `search_audit_logs` | `audit:read` | 否 |
| `k8s_list_pods` / `k8s_get_events` | `workload:read` | 否 |
| `k8s_scale_deployment` | `workload:write` | **是** |
| `hosts_list` | `host:read` | 否 |
| `hosts_exec` | `host:exec` | **是** |
| `promql_query` / `logs_query` / `alerts_list` | `metrics:read` / `logs:read` / `alert:read` | 否 |
| `cicd_list_runs` | `cicd:read` | 否 |
| `cicd_run_pipeline` | `cicd:run` | **是** |
| `kafka_consumer_lag` / `mq_overview` | `middleware:read` | 否 |

- 证据：`server/cmd/matrix-server/main.go:279`、`modules/cluster/ai.go:47-110`、`modules/hosts/module.go:283-310`、`modules/observability/handlers.go:491-557`、`modules/cicd/module.go:762-793`、`modules/middleware/module.go:635-663`。
- 安全约束：工具名必须匹配 `^[a-zA-Z0-9_-]{1,64}$`，必须声明权限，参数必须是合法 JSON Schema，禁止重名（`ai/tools.go:54-74`）。
- 页面覆盖：告警规则、创建工作负载、创建用户、部门/用户组、接入集群等对话框均已接 `/ai/assist` 生成（git log `feat(ai)` 系列共 8 条提交，至 `d706c50` 完成"全部对话框覆盖"）。

### 3.4 前端（`public/`，无构建依赖的 SPA）

- `public/index.html`：13 394 行，28 个路由键；`public/xs-api.js`：统一 API 客户端（base 探测、JWT 存取、401 单飞刷新、SSE/WS）。
- 实时适配层：`public/xs-live-core.js`（2 643 行，接管 `sre`/`iam`/`approvals`/`cluster`/`team`）、`public/xs-live-ops.js`（2 718 行，接管 `observe`/`logs`/`cicd`/`delivery`/`release`/`middleware`/`settings`/`ai`/`audit`/`appstatus`/`oncall` + `automation` 渲染器）。
- 演示模式机制：`XS.mode` 由 `/health` 探测决定；后端不可用时**完全不覆盖**原渲染器，只显示「演示模式」徽标（`xs-api.js:210-221`、`xs-live-core.js:2463`）。

### 3.5 部署与交付

| 交付物 | 内容 | 证据 |
| --- | --- | --- |
| Dockerfile | 多阶段（golang:1.23-alpine → alpine:3.20）、`-ldflags` 注入 `Version/Commit/BuildTime`、非 root（uid 10001）、`HEALTHCHECK`、同镜像构建 `matrix-server` + `matrix-agent` | `Dockerfile:1-31` |
| docker-compose | server + PostgreSQL + Redis + MinIO + guacd | `docker-compose.yml` |
| K8s 清单（开发集群完整版） | Namespace/Secret/StatefulSet(PG)/Deployment(Redis)/SA/Role/RoleBinding/ClusterRole/ClusterRoleBinding/Deployment(平台 2 副本)/双 Service/Ingress | `deploy/k8s/xingshu-matrix-dev.yaml` |
| Istio 入口 | `Gateway` + `VirtualService`（`matrix-gw` → host `xingshu-matrix`） | `deploy/k8s/istio-gateway.yaml` |
| 物理机安装 | CentOS/RHEL/Rocky/openEuler/麒麟/Ubuntu/Debian；Nginx、防火墙、SELinux、离线包 | `deploy/baremetal/install.sh`、`uninstall.sh` |
| 镜像后勤 | `image.sh` 的 `ensure/has/load/save/list` 五个动作，决策顺序：本地 → 离线包 → 拉取 → 本地构建 → 报错 | `deploy/scripts/image.sh` |
| 离线镜像包 | `deploy/images/matrix-server-v40.tar.gz`（19 226 445 字节，OCI tar + gzip） | `deploy/images/` |
| 自签证书 | `gen-tls-cert.sh` 生成并写入 TLS Secret | `deploy/scripts/gen-tls-cert.sh` |
| 冒烟脚本 | `scripts/smoke-test.sh`：`/healthz`、首页含「星枢」、`hark.css`/`hark.js`/`chart.js` | `scripts/smoke-test.sh` |

### 3.6 真实环境（历史部署事实；非 v1.18 验收）

| 项 | 事实 | 证据 |
| --- | --- | --- |
| 集群 | `192.168.6.3`（master）+ `k8s-node1` + `k8s-node2`，Rancher 管理 | `docs/deploy-192.168.6.3.md:1-9` |
| 工作负载 | `postgres-0 1/1`、`redis 1/1`、`xingshu-matrix 2/2` | 同上 §5.1 |
| 入口 | `https://xingshu-matrix.com`（自签 TLS，需 hosts）；备用 `http://192.168.6.3:30080` | 同上 §1、§8 |
| 验收 | 登录 → 强制改密 → `/auth/me`、`/users`、`/roles`、`/clusters`、`/hosts`、`/approvals`、`/audit-logs` 全部 200 | 同上 §5.2 |
| 镜像 | 集群内为 `docker.io/xingshu/matrix-server:v1.61`，`GET /api/v1/version` 返回 `commit=1c2f62b`、`build_time=2026-10-20T14:05:00Z` | `deploy/k8s/xingshu-matrix-dev.yaml:196`；version 接口实测 |

---

## 4. 部分完成（有基础，但明确缺东西）

| 项 | 已完成的部分 | 缺什么 | 证据 |
| --- | --- | --- | --- |
| **值班故障 / SLO** | 前端四个子页（排班 / 故障 / 复盘 / SLO）已按真实接口写完，调用 `/oncall/overview`、`/oncall/schedules`、`/oncall/incidents`、`/oncall/overrides`、`/oncall/slos` | **后端没有 oncall 模块**，这些请求会 404；页面会显示「值班概览加载失败」横幅 | `public/xs-live-ops.js:1983-2060`；`server/internal/modules/` 下无 oncall |
| **堡垒机** | 后端完整（资产、账号加密、授权、命令过滤、SSH 终端、录像、命令审计、强制断开） | v1.17 起部分接入；账号、授权、过滤、审计等子页仍未接真实接口 | `public/index.html`、`public/xs-live-ops.js` |
| **链路追踪** | Jaeger/Tempo 后端归一化 | v1.18 前端已接服务、检索、详情与资源标签 | `public/xs-observability.js` |
| **流量治理 / 网络策略** | 页面与可视化编辑器已存在 | v1.18 拓扑和策略视图部分接入真实 Service、NetworkPolicy 与 trace；配置写入未实现 | `public/index.html`、`cluster/network.go` |
| **多集群** | 前端 `#/ops/multicluster` 页面存在 | 无后端聚合接口，演示数据 | `public/index.html:12654` |
| **研发中心** | `coding`/`project`/`bugs` 三页已接 `/devcenter/*`（真实 POST/PATCH） | `requirements`（需求管理）与 `workbench`（工作台）仍读前端本地 `state.reqs`；`testing`（测试管理）读本地 `PLANS`；devcenter 无状态机、无评论、无 Git/流水线关联 | `public/index.html:2215-2245`、`5663-5700`；`modules/devcenter/module.go:30-35` |
| **自动化编排** | 流程/版本/触发/运行记录全链路落库，页面可触发运行 | `trigger` 只写 `queued`，**没有执行器 worker**，流程不会真的跑步骤 | `modules/automation/module.go:39-46` |
| **告警闭环** | 认领、关闭、静默增删、通知渠道增删与测试 | 通知只发一次、无重试队列与聚合；认领不回写 Alertmanager 静默 | `modules/observability/module.go:97-118` |
| **CI/CD 执行** | 每阶段一个 K8s Job（git-clone initContainer + Kaniko 构建 / 测试 / Helm），日志 SSE，部署/回滚走审批 | 无断点续跑、无阶段共享工作区、无并行阶段 DAG | `modules/cicd/runs.go`、`k8s.go` |
| **集群接入方式** | kubeconfig 直连（client-go v0.31） | 无集群内 Agent 反向隧道；无 Web Shell（exec）；无端口转发 | `modules/cluster/client.go` |
| **资源专用接口** | 工作负载四类 + 命名空间 + 节点 + 告警规则 | Service / Ingress / ConfigMap / Secret / PVC / HPA 专用接口缺失，只能走 YAML apply | `modules/cluster/module.go:92-120` |
| **单点登录** | 登录页有「企业 SSO」入口，调用 `/auth/sso/config` | 后端无 OIDC / LDAP | `public/index.html`（SSO 按钮）；`server/internal/auth/` 无 oidc/ldap |
| **多副本** | 平台 Deployment 2 副本可跑（K8s 清单） | 权限缓存按副本 15 秒，无失效广播；Agent 连接只在单实例内存；在线会话存内存 | `rbac/principal.go`、`modules/hosts/hub.go` |
| **测试覆盖** | v1.18 `go test ./...` 有测试的包通过（见 CHANGELOG）；新增前端 API 与 observability 测试 | auth/rbac/users/settings/approval/ai/audit 后端无单测；`xs-live-ops.js` 无 Node 自测 | `go test ./...`、`tests/frontend/xs-observability.test.mjs` |

---

## 5. 未开始

| 项 | 说明 |
| --- | --- |
| Helm Chart | 只有 `deploy/k8s/helm-values.example.yaml` 示例值，无 chart 本体；当前用原生清单 + Argo CD |
| 高可用 | Ingress 控制器单节点 hostNetwork；PostgreSQL 单副本 + `local-path` PVC 不可跨节点迁移 |
| 压测 / 安全审计 / 备份恢复演练 | 无 k6、无 govulncheck、无定时备份 |
| 网关与网络策略后端 | 证书管理、Cilium 网络策略可视化下发均无接口 |
| 值班通知联动 | 故障单 → 值班人 → 电话/短信升级链路未实现 |
| 移动端专项 | 仅做过 390px 视口的页面适配，无独立移动端 |

---

## 6. 页面级接线矩阵（28 个路由）

数据来源：`public/index.html` 的 `routes` 对象与 inline renderer、`xs-live-core.js`、`xs-live-ops.js`、`xs-observability.js` 的实时接管清单。以下为 v1.18 定性状态，不再使用 v1.17 的 19/1/8 汇总数。

| # | 路由键 | 页面 | 数据来源 | 后端支撑 |
| --- | --- | --- | --- | --- |
| 1 | `sre` | 总览 | **真实** `/clusters`、`/hosts`、`/observability/alerts` | 有 |
| 2 | `delivery` | 应用 | **真实** `/cicd/applications` | 有 |
| 3 | `cluster` | 集群状态 | **真实**（列表/节点/命名空间/工作负载/事件/日志） | 有 |
| 4 | `ops` | 多集群 | 演示 | 无 |
| 5 | `appstatus` | 应用状态 | **真实** | 有 |
| 6 | `cicd` | 流水线 | **真实**（含 SSE 日志） | 有 |
| 7 | `automation` | 自动化编排 | **真实**（但只有触发，无执行器） | 部分 |
| 8 | `release` | 发布单 | **真实** | 有 |
| 9 | `traffic` | 流量治理 | **部分真实**：拓扑子页读取集群 Service 与检索到的 trace 样本；配置页未接入 | 部分 |
| 10 | `netpol` | 网络策略 | **只读真实数据**：Service / NetworkPolicy 列表；策略编辑、下发未实现 | 部分 |
| 11 | `observe` | 监控告警 | **真实**（PromQL/规则/认领/静默/渠道） | 有 |
| 12 | `middleware` | 中间件 | **真实**（Kafka/RabbitMQ/RocketMQ/SQL/Redis） | 有 |
| 13 | `trace` | 链路追踪 | **真实**：服务、检索、详情与资源标签 | 有 |
| 14 | `logs` | 日志检索 | **真实**（Loki/ES） | 有 |
| 15 | `approvals` | 变更审批 | **真实**（待办/我的/全部，通过/驳回/取消） | 有 |
| 16 | `oncall` | 值班故障 | **真实接口但后端缺失** | **无（404）** |
| 17 | `audit` | 运维审计 | **真实** `/audit-logs`（筛选/分页/详情/CSV） | 有 |
| 18 | `bastion` | 堡垒机运维 | **部分接线**，沿用 v1.17 状态 | 部分 |
| 19 | `workbench` | 工作台 | 演示模式有本地样例；live 模式无数据渲染器时显示未接入空态 | 部分 |
| 20 | `requirements` | 需求管理 | 演示模式有本地样例；live 模式无数据渲染器时显示未接入空态 | 部分 |
| 21 | `coding` | Coding | **真实** `/devcenter/requirements` | 有 |
| 22 | `project` | 项目管理 | **真实** `/devcenter/iterations` | 有 |
| 23 | `team` | 团队协作 | **真实** `/users` + `/audit-logs` + `/devcenter/requirements` | 有 |
| 24 | `bugs` | 缺陷总览 | **真实** `/devcenter/bugs` | 有 |
| 25 | `testing` | 测试管理 | 演示模式有本地样例；live 模式无数据渲染器时显示未接入空态 | 部分 |
| 26 | `ai` | 智能助手 | **真实**（模型/提示词/对话） | 有 |
| 27 | `iam` | 用户与权限 | **真实**（用户/角色/部门/用户组/登录日志/会话） | 有 |
| 28 | `settings` | 设置 | **真实**（含访问域名卡片） | 有 |

**统计：19 / 1 / 8 是 v1.17 历史统计，不再代表 v1.18 的接线状态。** v1.18 新增真实 trace 与网络只读视图；traffic 与 netpol 仍是部分接线，oncall 后端仍缺失。

> 修正记录：v1.16 首次发布时此处误写为「真实 20 / 演示 7 / 后端缺失 1」——把 `oncall` 同时算进真实与缺失，
> 且漏算 1 个演示页。当前口径已按 28 个路由键逐条复核，详见 [OUTSTANDING.md](OUTSTANDING.md) §0。

---

## 7. 如何自己复核（v1.18 验证范围）

```bash
# 1) 后端编译与静态检查
cd server && go build ./... && go vet ./...

# 2) 后端单元测试（v1.18 使用隔离工作区缓存；结果见 CHANGELOG）
cd server && go test ./...

# 3) 前端 API 与可观测性单测（14 项通过）
node --test tests/frontend/xs-api.test.mjs tests/frontend/xs-observability.test.mjs

# 4) 前端核心适配层纯函数自测（历史覆盖：48 条断言）
node public/xs-live-core.js

# 5) 语法检查
node --check public/xs-api.js
node --check public/xs-live-core.js
node --check public/xs-live-ops.js
node --check public/xs-observability.js

# 6) 浏览器冒烟（需本机 Chrome；实测未在本轮运行）
node tests/frontend/mock-server.mjs &            # 默认端口 18080
node tests/frontend/smoke.mjs                    # 默认 CDP 端口 19222

# 7) 部署后冒烟
bash scripts/smoke-test.sh http://<服务器IP>:8080
```

> Windows 下 `node public/xxx.js` 若报 `lstat 'D:'`，是 Git Bash 的路径转换问题，
> 用 `node "$(cygpath -m "$PWD")/public/xs-live-core.js"` 传绝对路径即可。

---

## 8. v1.16 历史代码改动

| 文件 | 改动 | 原因 |
| --- | --- | --- |
| `.github/workflows/docker.yml` | 删除 `cd scripts && npm i jsdom@24 --no-save && node smoke-test.mjs`（目标文件不存在）；改为 `setup-go@v5` + `go build/vet/test` + `node --test tests/frontend/xs-api.test.mjs` | CI 从第一次提交起就是坏的，任何 push 都会在 Smoke test 步骤失败 |
| `README.md` | 版本号 v1.0-preview → v1.16，重写「当前状态」、功能接线标注、目录结构、路线图 | 与实际代码脱节 8 个版本 |
| `docs/PLATFORM.md` | 版本对齐 v1.16，重写已实现/未实现清单，补 IAM 部门与用户组、platform 层、PrometheusRule 告警规则、AI 工具表 | 原文档停留在预览版 |
| `docs/PROGRESS.md` | 新增本报告 | 需要一份可复核的进度口径 |
| `CHANGELOG.md` | 新增，倒序记录 v1.0 → v1.16 | 之前只有 git log，没有面向使用者的版本说明 |
| `docs/fix-ci-smoke-script.md` | 新增，按「报错问题 / 原因 / 解决方案 / 如何避免」记录 CI 修复 | 项目既有文档惯例 |
| `docs/OUTSTANDING.md` | 新增，未完成 / 未落实清单 | 需要一份"欠账"口径 |
| `docs/fix-stale-image-uncommitted-code.md` | 新增，四段式记录集群镜像混入未提交代码的排查与重建（v1.61→v1.62） | 镜像内容无法用 version 接口证明 |

---

## 9. 已知风险与待治理

### 9.1 镜像 tag 三套口径并存（容易发布错版本）

| 位置 | tag | 说明 |
| --- | --- | --- |
| `deploy/k8s/xingshu-matrix-dev.yaml:198` | `docker.io/xingshu/matrix-server:v1.63` | 与集群实际运行一致（commit `f9cd41e`，干净构建，`version=v1.17`） |
| Argo CD（application `xingshu-matrix`，source.path `deploy/k8s`，target `beta`） | 跟随清单 `v1.63` | v1.17 起 Argo 与 beta 同源；此前文档误写为 `test1`，已按 `kubectl get application` 实测更正 |
| `deploy/scripts/image.sh` 默认 `IMAGE` | `docker.io/xingshu/matrix-server:v1.3` | 脚本默认值滞后 |
| `deploy/images/README.md` | 离线包内 tag `v40`，标注「对应代码版本 v1.3」 | 离线包与文档不同步 |
| `.github/workflows/docker.yml` | 只推 `:latest` | 无版本 tag，无法回滚定位 |

**建议**：统一以应用版本（`v1.17`）作为镜像 tag，构建序号另存为 label，例如
`matrix-server:v1.17` + `org.opencontainers.image.revision=f9cd41e`。
在改口径之前，每次发布必须把「应用版本 ↔ 镜像 tag ↔ commit」写在同一处（当前：`v1.17 ↔ v1.63 ↔ f9cd41e`）。

### 9.2 前端超前于后端

`#/oncall` 五个接口在后端不存在。要么本版本下架该页菜单，要么补 oncall 模块（建议下一版优先）。

### 9.3 集群侧遗留（来自部署记录）

- `calico-token-refresh.sh` 自愈逻辑仍是坏的（本次手工修复），token 有效期到 **2026-11-17**。
- Rancher 主程序仍在重启循环（缺镜像 `rancher/rancher:v2.14.2`），不影响平台。
- 应用详情抽屉仍有 6 个面板是占位（服务列表 / 部署编排 / 监控告警 / 应用成员 / 操作记录 / 流程中心）。
- Ingress 控制器单节点 hostNetwork，无 HA。

---

## 10. 后续版本建议优先级

| 优先级 | 事项 | 理由 |
| --- | --- | --- |
| P0 | 新增 `oncall` 后端模块（排班/轮值/代班/故障/复盘/SLO） | 前端已就绪，只差后端，收益最大 |
| P0 | 堡垒机控制台接 `/bastion/*`（资产/账号/授权/过滤/会话/终端） | 后端最完整却完全没露出 |
| P1 | 真实外部依赖联调：Prometheus + Loki + Alertmanager + Jenkins + Kafka 各一套 | 目前只有 fake 测试，生产可信度缺口 |
| P1 | automation 执行器 worker（HTTP/等待/主机/K8s/审批五类步骤） | 编排目前只会排队 |
| P2 | 集群内 Agent 隧道 + Web Shell（`remotecommand.NewSPDYExecutor`）+ 端口转发 | 解决"必须能直连 API Server"的限制 |
| P2 | Service/Ingress/ConfigMap/Secret/PVC/HPA 专用接口 | 减少对 YAML apply 的依赖 |
| P2 | 镜像 tag 统一 + Helm Chart | 发布可回滚、可重复 |
| P3 | core 包补单测（auth/rbac/approval/ai）、`xs-live-ops.js` 补 Node 自测 | 补齐测试短板 |
| P3 | OIDC/LDAP 单点登录 | 企业接入前提 |
