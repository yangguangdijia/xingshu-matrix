# 星枢 Matrix

> 统一的云原生运维与研发效能平台 —— 一个入口管好集群、主机、应用交付、可观测、堡垒机与研发协作，并在每个功能里直接调用 AI。

![status](https://img.shields.io/badge/version-v1.0--preview-2f6bff) ![deploy](https://img.shields.io/badge/deploy-K8s%20%7C%20Docker%20%7C%20物理机-0b1a33)

> **技术说明文档**：开发语言、架构、背景、接口逻辑、接口文档索引、已实现与未实现清单见 [docs/PLATFORM.md](docs/PLATFORM.md)。后端为 Go 1.23（`server/`），接口文档在 `docs/api/`。

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

**运维中心**
- 总览：集群、主机、应用、告警健康度一屏可见
- 集群：集群接入（kubeconfig / Agent / 托管云 ACK、TKE、EKS）、节点、节点池与升级、主机纳管、多集群对比与资源分发
- 工作负载：Deployment/StatefulSet/DaemonSet/Pod、配置与密钥、CRD、项目与配额、Helm 应用商店、HPA/VPA、CronJob、备份恢复（Velero）、kubectl Web Shell
- 应用交付：应用、构建、制品、流水线、部署编排（滚动/灰度/蓝绿）、发布单、变更审批
- 网络：服务网格与流量治理、网关、网络策略（Cilium）、证书
- 可观测：监控与告警、日志检索、链路追踪
- 堡垒机：Web SSH、RDP/VNC、SFTP、批量命令、会话监控与录像回放、命令审计与高危拦截、访问申请
- 值班与故障：排班、故障单、SLO
- 设置：用户与权限（组织/部门/用户组/角色/RBAC 权限矩阵/SSO/LDAP/API Token）、系统接入、AI 模型配置

**研发中心**
- 工作台、需求管理、代码仓库与合并请求、迭代与看板、团队协作、缺陷总览、测试管理

## AI 能力

- **模型配置**：设置 › AI 模型，自行添加任意 OpenAI 兼容模型（Base URL、API Key、模型 ID、温度、上下文长度），可测试连接、设为默认，可按场景指定模型。
- **场景化提示词**：每个场景（告警分析、日志分析、部署检查、编排生成、链路诊断、需求开发等）都有可编辑的提示词模板。
- **功能内入口**：在各功能页直接点「AI 分析 / AI 生成」，带上当前页面上下文（如所选告警、日志查询、集群与命名空间）。

## 当前状态（务必阅读）

当前 **v1.0-preview** 为 **可联调的预览版**：

- **前端已接入真实接口**：登录/会话/改密、总览、用户与权限、审批、集群/节点/工作负载/日志、主机、团队协作、可观测/日志、CI/CD、中间件、值班/SLO、设置、AI、自动化编排。未接入的页面（流量治理、网络策略、堡垒机控制台、部分集群高级子页）保留内置演示数据。
- 无法连接后端时自动进入**演示模式**，登录页会明确提示，不会把演示数据伪装成实时数据。
- **后端已有**：认证、RBAC、审计、审批、AI，以及 cluster / hosts / bastion / observability / cicd / middleware / automation / devcenter 模块；SSO、oncall/SLO、告警静默回写、S3 录像、多副本路由等仍待补。
- 本轮尚未在真实 PostgreSQL / K8s / Prometheus / Loki / ES / Jenkins / Kafka 上做端到端联调；接口未配置时返回可观察错误。

因此 **请勿直接作为生产系统使用**。

## 部署方式

三种方式部署的是同一份前端资源；Docker / K8s 运行 `matrix-server`（内置 SPA），物理机脚本可用 Nginx 单独托管静态前端。

### 环境要求

| 项 | 最低 |
| --- | --- |
| CPU / 内存 | 1 核 / 512 MB |
| 端口 | 默认 8080（Docker/物理机）或 Ingress 80/443（K8s） |
| 浏览器 | Chrome / Edge / Firefox 最近两个大版本 |

### 方式一：Kubernetes

```bash
# 1. 构建并推送镜像（或使用你自己的镜像仓库）
docker build -t <你的仓库>/xingshu-matrix:v1.0 .
docker push <你的仓库>/xingshu-matrix:v1.0

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
bash scripts/package.sh v1.0                   # 生成 dist/xingshu-matrix-v1.0.tar.gz
sudo bash deploy/baremetal/install.sh --offline xingshu-matrix-v1.0.tar.gz
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
public/                 前端静态资源（index.html 为平台入口）
deploy/nginx.conf       Nginx 配置（含 /healthz 与 SPA 路由回退）
deploy/k8s/             Kubernetes 清单
deploy/baremetal/       物理机 / 虚拟机安装与卸载脚本
Dockerfile              Nginx 镜像
docker-compose.yml      Compose 部署
scripts/package.sh      打离线包
scripts/smoke-test.sh   部署后冒烟测试
```

## 路线图

- [x] 运维中心全模块前端（集群、工作负载、应用交付、网络、可观测、堡垒机、值班、设置）
- [x] 研发中心前端
- [x] 场景化 AI 入口与模型配置
- [x] 核心后端 API 与持久化（认证、RBAC、审计）
- [x] automation 与研发中心 API、迁移和前端真实接口
- [ ] 真实 Kubernetes / SSH / 指标与日志数据源生产环境联调
- [ ] Helm Chart 与高可用部署方案

## License

私有仓库，版权所有 © 星枢 Matrix。
