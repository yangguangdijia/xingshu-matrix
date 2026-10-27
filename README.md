# 星枢 Matrix

![version](https://img.shields.io/badge/version-v1.19-2f6bff) ![deploy](https://img.shields.io/badge/deploy-Kubernetes%20%7C%20Docker%20%7C%20Linux-0b1a33)

星枢 Matrix 是面向运维与开发工程师的 Kubernetes 管理平台。它把多集群资源管理、应用交付、网络与存储、告警诊断和操作审计放在一个工作台中，减少日常工作在不同工具之间切换的成本。

平台围绕集群、环境和命名空间组织资源。常用操作通过表单完成，复杂配置可查看或编辑资源清单；AI 可根据自然语言生成操作草稿和诊断建议，提交前由工程师检查确认。

## 功能概览

### 集群与工作负载

- 管理多个 Kubernetes 集群，并按环境查看集群、节点、命名空间和资源状态。
- 查看节点角色、容量、调度状态、标签与污点。节点表示集群中的单台机器；节点池用于把配置或用途相近的节点归组，便于集中查看和进行批量运维。
- 管理 Deployment、StatefulSet、DaemonSet、Job、CronJob、Pod 和自定义资源；查看 HPA 副本与指标状态。
- 管理基于 `autoscaling/v2` 的 HPA，以及 CronJob 调度、手动触发和 Job 执行历史。
- 使用 ConfigMap 管理应用配置；Secret 操作默认隐藏敏感值。
- 管理 Kubernetes Service，覆盖 ClusterIP、NodePort、LoadBalancer 和 ExternalName 等类型。
- 管理 PersistentVolume、PersistentVolumeClaim、StorageClass；集群安装卷快照 API 与驱动后，可管理 VolumeSnapshot 并从就绪快照创建新的 PVC。

### 网络与访问

- 从 Service 页面管理集群内部服务、端口和选择器。
- 从对外服务页面维护域名入口、路径路由、后端服务和入口流量配置。
- 通过网络策略查看工作负载间的访问规则，并结合服务拓扑了解已观测到的调用方向。
- 根据集群安装的入口控制器、网关、CNI 和服务网格组件使用相应能力。

### 应用交付

- 按应用管理代码仓库、镜像、部署环境和目标工作负载。
- 创建包含构建、测试、部署与通知阶段的流水线，并维护 Jenkins 或 Argo CD 连接参数。
- 浏览 Helm 仓库与 Chart，按表单填写安装参数并管理 Release。
- 通过发布单和环境审批约束生产变更，查看执行进度与操作记录。
- 使用内置 Ansible 执行自动化任务，无需在目标主机单独安装 Ansible。

### 可观测与智能运维

- 汇总集群与应用状态、事件、告警、日志、指标和链路信息。
- 配置告警规则、静默和通知渠道，并查看告警关联资源与处理记录。
- 使用 AI 分析告警和日志，生成 Kubernetes 配置草稿，辅助判断影响范围并提供处置建议。
- 自动化修复需要经过权限校验和必要的审批；AI 建议不会替代工程师对变更的确认。

### 权限与审计

- 通过用户、角色、环境范围和资源权限控制操作范围。
- 按环境配置审批策略，对生产变更执行审批约束。
- 记录重要的登录、资源变更、审批和自动化操作，便于检索与追溯。

## 部署

平台支持 Kubernetes、Docker Compose 和 Linux 主机部署。部署前准备 PostgreSQL；Redis 可用于会话、缓存与任务协调。各部署方式的配置模板位于 `deploy/` 和仓库根目录。

### Kubernetes

```bash
docker build --build-arg VERSION=v1.19 \
  -t <你的镜像仓库>/xingshu-matrix:v1.19 .
docker push <你的镜像仓库>/xingshu-matrix:v1.19
```

将 `deploy/k8s/xingshu-matrix.yaml` 中的镜像地址和 Ingress 域名改为当前环境配置，然后应用资源：

```bash
kubectl apply -f deploy/k8s/xingshu-matrix.yaml
kubectl -n xingshu-system get pods,svc,ingress
```

集群应为数据库和平台配置持久化存储，并按实际访问方式配置 Ingress、TLS 与网络策略。

### Docker Compose

设置数据库密码、平台加密密钥和初始管理员密码后启动：

```bash
docker compose up -d --build
docker compose ps
```

浏览器访问 `http://<服务器地址>:8080`。首次登录后应立即修改管理员密码，并按需配置 AI 模型、集群凭据和外部服务端点。

### Linux 主机

适用于 Ubuntu、Debian、RHEL、Rocky Linux、openEuler 等 Linux 发行版：

```bash
sudo bash deploy/baremetal/install.sh --port 8080 --dir /opt/xingshu-matrix
```

卸载：

```bash
sudo bash deploy/baremetal/uninstall.sh /opt/xingshu-matrix
```

## 首次使用

1. 使用管理员账号登录，并先修改初始密码。
2. 在系统设置中填写平台入口、镜像源和所需集成参数。
3. 接入 Kubernetes 集群，选择环境范围并检查集群连接状态。
4. 配置用户、角色和环境审批策略，再按需接入镜像仓库、代码仓库与可观测组件。
5. 在 AI 模型设置中添加兼容 OpenAI Chat Completions 的服务。模型接入后，可在告警、日志、工作负载、网络和自动化页面使用场景化 AI。

## 目录

| 路径 | 内容 |
| --- | --- |
| `server/` | Go API 服务与主机 Agent |
| `public/` | Web 管理界面 |
| `deploy/` | Kubernetes、Docker、Nginx 与 Linux 部署文件 |
| `docs/` | 平台架构、配置说明和接口文档 |
| `tests/` | 前端、后端与浏览器测试 |
| `CHANGELOG.md` | 版本更新说明 |

## 文档

- [平台架构与技术说明](docs/PLATFORM.md)
- [后端开发指南](docs/dev-guide.md)
- [接口文档索引](docs/api/)
- [部署与配置文件](deploy/)
- [更新日志](CHANGELOG.md)

## 版本构建

```bash
docker build --build-arg VERSION=v1.19 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  -t xingshu-matrix:v1.19 .
```

本地运行检查：

```bash
cd server && go test ./...
node --test tests/frontend/*.test.mjs
```

## License

私有仓库，版权所有 © 星枢 Matrix。
