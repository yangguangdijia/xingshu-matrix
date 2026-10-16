# 星枢 Matrix 后端接入架构（真实数据从哪来、操作怎么下发）

## 总体
- 前端：现有页面，所有数据改为调用 `/api/v1/*`；保留「演示模式」开关（mock）。
- 平台后端 `matrix-server`（Go）：API 网关、认证 RBAC、审计、任务调度、各数据源适配器。
- 存储：PostgreSQL（元数据/权限/审计）、Redis（会话/缓存/锁）、MinIO/S3（录像、制品、备份）、NATS（任务队列/事件）。
- 执行面：
  - K8s 集群：直连 kubeconfig（client-go）或在集群内装 `matrix-cluster-agent`，通过反向 WebSocket 隧道连回平台（适合内网/NAT 集群，Rancher 同思路）。
  - 物理机/虚拟机：`matrix-agent`（Go 单文件，systemd 运行），一行命令安装，长连接 gRPC 回平台；负责心跳、上报主机信息、执行命令/脚本、文件分发、日志采集配置。无 Agent 时走堡垒机 SSH。
- 所有写操作：RBAC 校验 → 生产环境走审批 → 下发 → 记录审计 → 结果回写。

## 各功能的数据来源与下发方式
| 功能 | 读数据 | 写/下发 |
| --- | --- | --- |
| 集群/节点/工作负载 | K8s API（Informer 缓存）；节点 OS/内核/运行时取自 `node.status.nodeInfo`；CPU/内存用量取 metrics-server 或 Prometheus | client-go apply/patch/delete；kubectl Web Shell 走 exec 子资源 |
| 主机 | matrix-agent 上报（CPU 型号、核数、内存、磁盘、OS、内核、负载） | agent 执行命令/脚本，或 SSH |
| Helm 应用商店 | 仓库 `index.yaml`（含每个 Chart 的 `icon` 官方图标地址）/ Artifact Hub API | Helm Go SDK install/upgrade/rollback |
| 日志检索 | 采集：K8s 内 Fluent Bit/Vector DaemonSet，主机由 agent 或 Filebeat；存储：Elasticsearch/OpenSearch 或 Loki。查询：后端把 KQL 转 ES DSL 或 LogQL 调其 HTTP API | — |
| 监控 | Prometheus HTTP API（query_range）；node-exporter、kube-state-metrics、各 exporter | 告警规则写入 PrometheusRule / Alertmanager 配置 |
| 告警 | Alertmanager webhook 推到平台 → 去重、分派值班、通知（钉钉/飞书/企微/邮件） | 静默、认领、关闭回写 Alertmanager |
| 链路追踪 | Jaeger / Tempo / SkyWalking 查询 API；应用接 OpenTelemetry | — |
| CI/CD | 内置执行：每个流水线阶段生成 K8s Job（或 Tekton TaskRun）在构建集群里跑；也可对接 Jenkins/GitLab CI API 触发并拉取日志 | 部署阶段：K8s → 更新 Deployment 镜像/Helm upgrade/Argo CD sync；主机 → agent 拉制品并按批次执行部署脚本，健康检查后继续 |
| 发布单/变更审批 | PostgreSQL | 审批通过后由调度器按步骤调用上面的部署接口，失败自动/手动回滚 |
| 堡垒机 | 资产/账号存 PostgreSQL（密码 KMS 加密） | SSH 代理（Go x/crypto/ssh + WebSocket + xterm.js）；RDP/VNC 走 guacd；MySQL/PostgreSQL/Redis 走协议代理 + Web SQL 客户端；录像存 MinIO（asciicast），命令/SQL 实时过滤 |
| 中间件 MQ | Kafka Admin API（Topic、分区、消费组 Lag）、RabbitMQ Management HTTP API、RocketMQ Admin；指标走 kafka-exporter/rabbitmq exporter | 建/删 Topic、队列；堆积/宕机告警走 Prometheus 规则 |
| 自动化编排 | PostgreSQL 模板 | 按 环境/主机组/集群 解析目标 → agent/Ansible/K8s Job 执行 |
| 研发：代码/MR | GitLab/Gitea/GitHub API、Webhook | 创建分支、MR、评论 |
| AI | 用户配置的 OpenAI 兼容接口 | Function Calling：模型只能调用平台暴露的工具（查日志、PromQL、kubectl get、生成 YAML），执行类工具一律走 RBAC + 用户确认 + 生产审批 |

## 部署
- K8s：Helm Chart（server、agent 镜像、PostgreSQL/Redis/MinIO 可选内置或外接）。
- Docker：docker-compose（server + postgres + redis + minio + guacd）。
- 物理机：二进制 + systemd + 安装脚本；离线包。

## 实施顺序建议
1. 后端骨架：认证、RBAC、审计、API 规范；前端接 API + 演示模式开关。
2. K8s 集群接入（kubeconfig/agent）、节点/工作负载真实数据、Web Shell。
3. matrix-agent + 主机纳管 + 堡垒机 SSH 与录像。
4. Prometheus/Alertmanager、日志（ES/Loki）、链路（Jaeger/Tempo）。
5. CI/CD 执行器与发布、Helm。
6. MQ、数据库代理、自动化编排、AI 工具调用。
