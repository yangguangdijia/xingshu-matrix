# 星枢 Matrix

> 统一的云原生运维与研发效能平台 —— 一个入口管好集群、主机、应用交付、可观测、堡垒机与研发协作，并在每个功能里直接调用 AI。

![status](https://img.shields.io/badge/version-v1.17-2f6bff) ![deploy](https://img.shields.io/badge/deploy-K8s%20%7C%20Docker%20%7C%20物理机-0b1a33) ![api](https://img.shields.io/badge/API-237%20routes-0b1a33) ![pages](https://img.shields.io/badge/pages-19%2F28%20live%20%C2%B7%201%20%E7%BC%BA%E5%90%8E%E7%AB%AF-3F8F1F)

> **技术说明文档**：开发语言、架构、背景、接口逻辑、接口文档索引、已实现与未实现清单见 [docs/PLATFORM.md](docs/PLATFORM.md)。后端为 Go 1.23（`server/`），接口文档在 `docs/api/`。
>
> **进度与版本**：逐页接线矩阵、已完成/部分完成/未开始清单见 [docs/PROGRESS.md](docs/PROGRESS.md)；未完成与未落实清单见 [docs/OUTSTANDING.md](docs/OUTSTANDING.md)；版本变更见 [CHANGELOG.md](CHANGELOG.md)。

---

## 目录

- [它是什么](#它是什么)
- [适合谁](#适合谁)
- [适用场景](#适用场景)
- [为什么选择星枢](#为什么选择星枢)
- [功能一览](#功能一览)
- [AI 能力](#ai-能力)
- [当前状态（务必阅读）](#当前状态务必阅读)
- [部署方式](#部署方式)
  - [方式一：Kubernetes](#方式一kubernetes)
  - [方式二：Docker / Docker Compose](#方式二docker--docker-compose)
  - [方式三：物理机 / 虚拟机](#方式三物理机--虚拟机)
- [首次使用](#首次使用)
- [目录结构](#目录结构)
- [路线图](#路线图)

---

## 它是什么

星枢 Matrix 是一个可私有化部署的 **运维 + 研发效能一体化平台**，分为两个工作台：

| 工作台 | 解决的问题 |
| --- | --- |
| **运维中心** | 多集群 Kubernetes 与物理机/虚拟机统一纳管、应用交付与发布、网络与流量治理、监控告警/日志/链路追踪、堡垒机与运维审计、值班与故障管理、用户与权限 |
| **研发中心** | 需求、开发（代码仓库/合并请求）、迭代与看板、团队协作、缺陷与测试管理 |

两边共享同一套用户、权限、环境标签（生产/预发/测试/开发）和 AI 模型配置，需求可以一路走到构建、发布、监控和故障复盘。

## 适合谁

- **运维 / SRE / 平台工程团队**：同时管理多个 K8s 集群和一批物理机，需要统一入口、统一权限、统一审计。
- **研发团队负责人与开发者**：希望需求、代码、发布、线上状态在一处可见，少在多套系统之间切换。
- **安全与合规岗位**：需要堡垒机录像、命令审计、高危命令拦截、三员分立和访问审批。
- **中小型企业 / 私有化场景的 IT 部门**：没有精力自研平台，又不想把 Rancher、JumpServer、Kibana、Grafana、Jira 各部署一套再自己打通。

## 适用场景

1. **多集群、多环境运维**：生产/预发/测试/开发多个集群，需要对比配置差异、一键把资源分发到多个集群并可回滚。
2. **混合基础设施**：K8s 与物理机、虚拟机并存，主机批量纳管、分组、Agent 安装、批量命令。
3. **规范化发布**：滚动/灰度/蓝绿发布、发布单、生产变更审批、发布记录可追溯。
4. **合规审计**：等保、内审要求的运维操作留痕，堡垒机会话录像回放、命令审计。
5. **故障响应**：告警 → 值班通知 → 故障单 → 链路/日志定位 → 复盘与 SLO 跟踪。
6. **AI 辅助运维与研发**：用自然语言生成编排、分析告警与日志、根据需求生成代码与合并请求。

## 为什么选择星枢

- **一个平台替代多套工具**：覆盖 Rancher（集群管理）、JumpServer（堡垒机）、Kibana（日志检索）、Grafana/Prometheus（监控）、Jira（需求缺陷）中最常用的部分，并且相互打通。
- **按环境治理**：所有资源都带环境标签，权限可以细到「某环境 / 某集群 / 某命名空间 / 某主机组」，生产操作可强制审批。
- **AI 内嵌在每个功能里**：不是一个孤立的聊天框，告警、日志、部署、编排、链路、需求页里都有针对该场景的「AI 分析 / 生成」入口；模型只需填 OpenAI 兼容接口即可接入私有模型。
- **私有化友好**：纯静态前端，无外部依赖，支持离线包；可部署在 K8s、Docker 或任意 Linux 物理机。
- **国产化友好**：安装脚本支持 CentOS/Rocky/openEuler/麒麟、Ubuntu/Debian。

## 功能一览

> 标注说明：**[真]** 页面已接真实接口；**[演]** 仍为内置演示数据；**[缺]** 前端就绪、后端未实现。

**运维中心**
- 总览：集群、主机、应用、告警健康度一屏可见 **[真]**
- 集群：接入（kubeconfig）、节点 cordon/drain/标签/污点、命名空间、工作负载、事件、Pod 日志、YAML apply、告警规则（集群内 `PrometheusRule`）**[真]**
- 多集群对比与资源分发 **[演]**
- 应用交付：应用、流水线、运行与 SSE 日志、部署/回滚、发布单、Jenkins、GitLab/GitHub Webhook **[真]**
- 自动化编排：流程、版本、触发、运行记录 **[真，无执行器]**
- 网络：流量治理、网关、网络策略、证书 **[演]**
- 可观测：监控与告警（PromQL、认领/关闭、静默、通知渠道）、日志检索（Loki/ES）**[真]**；链路追踪 **[演]**（后端已有接口）
- 堡垒机：Web SSH / 会话 / 命令审计 / 录像回放 **[演]**（后端完整，前端未接）；RDP / VNC **[未实现]**
- 值班与故障：排班、故障、复盘、SLO **[缺]**
- 设置：用户、角色、权限矩阵、部门、用户组、环境策略、登录日志、访问域名、AI 模型与提示词 **[真]**；SSO/LDAP **[未实现]**

**研发中心**
- Coding（需求）、项目管理（迭代）、缺陷总览、团队协作 **[真]**
- 工作台、需求管理、测试管理 **[演]**
- 状态机、评论、Git/流水线关联 **[未实现]**

## AI 能力

- **模型配置**：设置 › AI 模型，自行添加任意 OpenAI 兼容模型（Base URL、API Key、模型 ID、温度、上下文长度），可测试连接、设为默认，可按场景指定模型。
- **场景化提示词**：每个场景（告警分析、日志分析、部署检查、编排生成、链路诊断、需求开发等）都有可编辑的提示词模板。
- **功能内入口**：在各功能页直接点「AI 分析 / AI 生成」，带上当前页面上下文（如所选告警、日志查询、集群与命名空间）。

## 当前状态（务必阅读）

当前版本 **v1.17**，定位是**可在真实集群部署的内测版**，不是生产版。

**已经做到的**

- **后端可编译、可测试**：237 条路由、34 张表、13 个迁移、50 个权限点、7 类审批；`go build ./...`、`go vet ./...` 全部通过，`go test ./...` 中 bastion / cicd / cluster / hosts / middleware / observability 六个模块测试通过。
- **8 个业务模块全部有真实执行链路**：cluster（client-go 直连）、hosts（WebSocket Agent）、bastion（SSH 代理 + 录像 + 命令审计）、observability（Prometheus/Loki/ES/Jaeger/Tempo/Alertmanager）、cicd（K8s Job + Kaniko + Helm）、middleware（Kafka/RabbitMQ/RocketMQ/SQL/Redis）、automation、devcenter。
- **AI 嵌到操作里**：12 个 Function Calling 工具，3 个写工具（`k8s_scale_deployment`、`hosts_exec`、`cicd_run_pipeline`）必须用户二次确认，生产环境继续走审批；告警规则、创建工作负载、创建用户、部门/用户组、接入集群等对话框均可 AI 生成。
- **前端 28 个页面中 19 个真实可用**，另有 1 个（值班故障）前端已接但后端缺失（明细见 [docs/PROGRESS.md](docs/PROGRESS.md) §6）。
- **已在真实集群验证**：`192.168.6.3` 三节点，`https://xingshu-matrix.com` 入口，登录 → 强制改密 → 用户/角色/集群/主机/审批/审计接口全部 200（记录见 [docs/deploy-192.168.6.3.md](docs/deploy-192.168.6.3.md)）。

**还没做到的**

- **值班故障 / SLO 只有前端**：页面会调 `/oncall/*`，但后端没有 oncall 模块，请求返回 404，页面顶部会显示加载失败横幅。
- **8 个页面仍是演示数据**：多集群、流量治理、网络策略、链路追踪（后端有接口、前端未接）、堡垒机控制台（后端最完整却未接）、工作台、需求管理、测试管理。
- **自动化编排没有执行器**：触发只写 `queued`，流程不会真的执行步骤。
- **真实外部依赖未联调**：Prometheus / Loki / Elasticsearch / Alertmanager / Jenkins / Kafka 目前只有 httptest 与 client-go fake 测试。
- **单集群、单副本 PostgreSQL、单节点 Ingress**：无高可用，PostgreSQL 用 `local-path` PVC，Pod 重建后绑回原节点。
- **无 SSO**：登录页有「企业 SSO」入口，但后端没有 OIDC/LDAP。

因此 **请勿直接作为生产系统使用**；可以先在测试集群里跑通集群接入、主机纳管、审计与审批链路。

## 部署方式

三种方式部署的是同一份前端资源；Docker / K8s 运行 `matrix-server`（内置 SPA），物理机脚本可用 Nginx 单独托管静态前端。

### 环境要求

| 项 | 最低 |
| --- | --- |
| CPU / 内存 | 1 核 / 512 MB |
| 端口 | 默认 8080（Docker/物理机）或 Ingress 80/443（K8s） |
| 浏览器 | Chrome / Edge / Firefox 最近两个大版本 |

### 版本与镜像标签

| 概念 | 当前值 | 说明 |
| --- | --- | --- |
| 应用版本 | `v1.17` | 取自 git tag / `-ldflags`，写入二进制 `Version` 字段，`GET /api/v1/version` 可见 |
| 集群内镜像 tag | `docker.io/xingshu/matrix-server:v1.63` | 构建序号，与上面**不是同一个号**；对应 commit `f9cd41e`（`GET /api/v1/version` 返回 `version=v1.17`） |
| 离线包 | `deploy/images/matrix-server-v40.tar.gz` | 包里 tag 为 `v40`，`image.sh` 会自动补目标 tag 别名 |
| CI 产物 | `ghcr.io/<owner>/xingshu-matrix:latest` | 目前只推 `latest`，无版本 tag（待治理，见 docs/PROGRESS.md §9.1） |

构建时务必带上版本号，否则 `/api/v1/version` 只会显示 `dev`：

```bash
docker build --build-arg VERSION=v1.17 --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t <你的仓库>/xingshu-matrix:v1.17 .
```

### 方式一：Kubernetes

```bash
# 1. 构建并推送镜像（或使用你自己的镜像仓库）
docker build --build-arg VERSION=v1.17 -t <你的仓库>/xingshu-matrix:v1.17 .
docker push <你的仓库>/xingshu-matrix:v1.17

# 2. 修改 deploy/k8s/xingshu-matrix.yaml 中的 image 与 Ingress host
kubectl apply -f deploy/k8s/xingshu-matrix.yaml

# 3. 检查
kubectl -n xingshu-system get pods,svc,ingress
```

默认 2 副本，带就绪/存活探针（`/healthz`）与资源限制。

### 方式二：Docker / Docker Compose

```bash
# Compose（推荐）
docker compose up -d --build
# 访问 http://<服务器IP>:8080

# 或纯 Docker
docker build -t xingshu-matrix:latest .
docker run -d --name xingshu-matrix -p 8080:8080 --restart unless-stopped xingshu-matrix:latest
```

### 方式三：物理机 / 虚拟机

适用于 CentOS / RHEL / Rocky / openEuler / 麒麟 / Ubuntu / Debian，脚本会安装 Nginx、拷贝文件、写配置、放行防火墙端口并处理 SELinux。

```bash
git clone https://github.com/yangguangdijia/xingshu-matrix.git
cd xingshu-matrix
sudo bash deploy/baremetal/install.sh --port 8080 --dir /opt/xingshu-matrix
```

**离线环境**：在有网机器上打包，拷贝到目标机安装：

```bash
bash scripts/package.sh v1.17                  # 生成 dist/xingshu-matrix-v1.17.tar.gz
sudo bash deploy/baremetal/install.sh --offline xingshu-matrix-v1.17.tar.gz
```

卸载：`sudo bash deploy/baremetal/uninstall.sh /opt/xingshu-matrix`

### 部署后验证

```bash
bash scripts/smoke-test.sh http://<服务器IP>:8080
```

## 首次使用

1. 打开平台，使用内置账号 `admin` 登录，首次登录会 **强制修改密码**。
2. 进入初始化向导：环境检测 → 基础信息 → 代码仓库 / 镜像仓库 / 可观测性 → 接入 K8s 集群 → 消息通知。
3. 在「设置 › 用户与权限」创建用户、角色与资源范围；如需 SSO，在同一处配置。
4. 在「设置 › AI 模型」添加 OpenAI 兼容模型，即可在各功能页使用 AI。

## 目录结构

```
server/                  Go 后端（cmd/matrix-server、cmd/matrix-agent、internal/*）
public/                  前端静态资源（index.html 为入口，xs-api.js / xs-live-core.js / xs-live-ops.js 为实时适配层）
docs/                    技术说明（PLATFORM.md）、进度报告（PROGRESS.md）、接口文档（api/）、真实部署记录
tests/frontend/          前端单测与浏览器冒烟（xs-api.test.mjs、smoke.mjs、mock-server.mjs）
deploy/nginx.conf        Nginx 配置（含 /healthz 与 SPA 路由回退）
deploy/k8s/              Kubernetes 清单（开发集群完整版 + Istio 网关 + Helm 值示例）
deploy/baremetal/        物理机 / 虚拟机安装与卸载脚本
deploy/scripts/          镜像后勤（image.sh）与自签证书（gen-tls-cert.sh）
deploy/images/           离线镜像包（OCI tar + gzip）
Dockerfile               多阶段镜像（构建 matrix-server + matrix-agent）
docker-compose.yml       Compose 部署（server + PostgreSQL + Redis + MinIO + guacd）
scripts/package.sh       打前端离线包
scripts/smoke-test.sh    部署后冒烟测试
Makefile                 build / test / vet / run / docker / package
```

## 质量门禁

本地可复现的全部检查（v1.17 实测结果）：

```bash
cd server && go build ./... && go vet ./...   # 通过
cd server && go test ./...                    # 7 个包 ok（bastion 12 个用例，v1.17 新增 7 个）
node --test tests/frontend/xs-api.test.mjs    # 5 通过
node public/xs-live-core.js                   # 48 条断言全部通过
```

CI（`.github/workflows/docker.yml`）在 `push` 到 `main`、打 `v*` 标签或手工触发时执行同一组检查并推送镜像。
浏览器级冒烟需要本机 Chrome，按 `tests/frontend/` 下的说明手工运行。

## 路线图

- [x] 运维中心全模块前端（集群、工作负载、应用交付、网络、可观测、堡垒机、值班、设置）
- [x] 研发中心前端
- [x] 场景化 AI 入口与模型配置（12 个工具 + 3 个写操作确认）
- [x] 核心后端 API 与持久化（认证、RBAC、审计、审批、AI）
- [x] automation 与研发中心 API、迁移和前端真实接口
- [x] 真实集群部署与平台自身链路验收（192.168.6.3）
- [x] 用户组 / 部门 / 登录日志 / 访问域名 / 告警规则 PrometheusRule
- [x] CI 修复（v1.16）
- [ ] oncall 后端模块（排班 / 故障 / 复盘 / SLO）
- [ ] 堡垒机控制台、链路追踪前端接真实接口
- [ ] automation 执行器 worker
- [ ] 真实 Kubernetes / Prometheus / Loki / ES / Jenkins / Kafka 生产级联调
- [ ] Helm Chart 与高可用部署方案
- [ ] OIDC / LDAP 单点登录

## License

私有仓库，版权所有 © 星枢 Matrix。
