# 日志采集部署（Fluent Bit + Loki）

本文说明如何在业务集群部署日志采集，并让平台「日志」页查询到这些日志。清单位于 `deploy/k8s/logging/`：

| 文件 | 内容 |
| --- | --- |
| `fluent-bit.yaml` | 命名空间 `xingshu-logging`、ServiceAccount、ClusterRole/Binding（只读 pods、namespaces）、配置 ConfigMap、DaemonSet |
| `loki.yaml` | Loki 单副本 StatefulSet（单体模式、本地文件系统存储、20Gi PVC、保留 7 天）、Service `loki` 与无头 Service |

固定镜像版本：`fluent/fluent-bit:3.2.10`、`grafana/loki:3.4.2`。升级时修改清单中的镜像标签，先在测试集群验证。

## 为什么默认是 Loki

平台日志查询同时支持 Loki 与 Elasticsearch。日志页的默认查询是 `{job="fluent-bit"}`，多集群共用一个后端时按 `cluster=<平台集群名>` 标签区分集群（中央数据源的默认集群标签名就是 `cluster`）。本清单的 Loki 输出正好写入这两个标签，因此默认配置无需在平台侧额外调整即可对接。

每条日志在 Loki 中的标签：

| 标签 | 来源 |
| --- | --- |
| `job` | 固定 `fluent-bit` |
| `cluster` | DaemonSet 环境变量 `CLUSTER_NAME` |
| `node_name` | Pod 所在节点 |
| `namespace` / `pod` / `container` | kubernetes 过滤器元数据 |

日志正文只保留容器原始输出行；平台从正文里识别级别（如 `level=error`、JSON 中的 `level`），服务名取 `container` 标签，主机取 `pod` 标签。

## 部署步骤

1. **修改集群名**：编辑 `fluent-bit.yaml` 中 DaemonSet 的 `CLUSTER_NAME`，把 `change-me` 改成该集群在平台「集群」列表中显示的名称。每个集群各填自己的名称。
2. **确认存储**：`loki.yaml` 的 PVC 使用集群默认 StorageClass（20Gi）。没有默认 StorageClass 时取消注释 `storageClassName` 并填写（k3s 可用 `local-path`）。按日志量调整容量与 `retention_period`（默认 168h）。
3. **校验清单**（不改变集群）：
   ```bash
   kubectl apply --dry-run=server -f deploy/k8s/logging/fluent-bit.yaml
   kubectl apply --dry-run=server -f deploy/k8s/logging/loki.yaml
   ```
   命名空间尚不存在时，第二条的服务端校验会提示命名空间不存在，可先对 `loki.yaml` 用 `--dry-run=client`。
4. **部署**：先建命名空间与采集器，再部署 Loki。
   ```bash
   kubectl apply -f deploy/k8s/logging/fluent-bit.yaml
   kubectl apply -f deploy/k8s/logging/loki.yaml
   kubectl -n xingshu-logging rollout status statefulset/loki
   kubectl -n xingshu-logging rollout status daemonset/fluent-bit
   ```
   Loki 就绪前 Fluent Bit 会把数据缓存在节点磁盘并重试，不会丢弃。
5. **自检**：
   ```bash
   kubectl -n xingshu-logging logs ds/fluent-bit --tail=50     # 不应持续出现 [error]
   kubectl -n xingshu-logging port-forward svc/loki 3100:3100
   curl -s localhost:3100/loki/api/v1/labels                  # 应包含 job、cluster、namespace、pod
   ```

已有 Loki（例如通过平台「集群组件」安装的 Loki）时，可以只部署 `fluent-bit.yaml`，把 DaemonSet 的 `LOKI_HOST` / `LOKI_PORT` 改为现有地址。

## 与平台日志页对接

在平台可观测模块的数据源管理中新建数据源：

| 字段 | 单集群（每个集群一个 Loki） | 中央（多个集群共用一个 Loki） |
| --- | --- | --- |
| 类型 | Loki | Loki |
| 地址 | 平台可访问的 Loki 地址 | 同左 |
| 范围 | 集群，选择对应集群 | 中央 |
| 中央源集群标签名 | 不填 | `cluster` |
| 集群映射 | 不填 | 每个平台集群映射到它的 `CLUSTER_NAME`（采集端填的就是平台集群名时，与默认映射一致） |
| 日志筛选器 | 留空（默认 `{job="fluent-bit"}`） | 同左 |
| 组织 ID | 仅多租户 Loki 填写，与采集端 `tenant_id` 一致 | 同左 |

地址说明：`http://loki.xingshu-logging:3100` 只在该集群内部可解析。平台部署在同一集群时直接填写；平台在集群外时，需要通过内网负载均衡或网关暴露 Loki，并在网关处加认证（本清单的 Loki 未开启认证，不要直接暴露到公网）。

保存后在「日志」页选择该集群，默认显示最近日志；可在查询框输入 `{job="fluent-bit", namespace="default"} |= "error"` 之类的条件。

## 改用 Elasticsearch

1. 编辑 `fluent-bit.yaml` ConfigMap 中 `fluent-bit.conf` 末尾，注释 `@INCLUDE output-loki.conf`，取消注释 `@INCLUDE output-elasticsearch.conf`。
2. 修改 DaemonSet 的 `ES_HOST` / `ES_PORT`。需要认证时创建 Secret（口令只放在 Secret 中）：
   ```bash
   kubectl -n xingshu-logging create secret generic xingshu-logging-es \
     --from-literal=username=<用户名> --from-literal=password=<口令>
   ```
   然后取消注释 `output-elasticsearch.conf` 中的 `HTTP_User` / `HTTP_Passwd`；HTTPS 时同时打开 `tls`。
3. `kubectl apply -f deploy/k8s/logging/fluent-bit.yaml` 后执行 `kubectl -n xingshu-logging rollout restart ds/fluent-bit`（ConfigMap 变更不会自动重启 Pod）。
4. 不需要部署 `loki.yaml`。
5. 平台数据源：类型 Elasticsearch，索引 `logs-*`，时间字段 `@timestamp`，高级 Options 中 `message_field` 填 `log`。中央数据源的集群标签名建议填 `cluster.keyword`（动态映射下 `cluster` 是分词字段，精确匹配要用 keyword 子字段），或预先建立把 `cluster` 定义为 keyword 的索引模板后填 `cluster`。

ES 输出开启了 `Replace_Dots`，Pod 标签中的 `.` 会替换为 `_`；平台从 `kubernetes.labels.app`、`kubernetes.container_name`、`kubernetes.pod_name` 读取服务名与主机。

## 安全与资源

- 采集器以 root 读取节点日志（文件属主为 root），但不提权、去掉全部 Linux 能力、根文件系统只读；`/var/log` 与 `/var/lib/docker/containers` 只读挂载。唯一可写的是节点目录 `/var/lib/xingshu-fluent-bit`（读取位置记录与磁盘缓冲）。
- 容忍所有污点，控制面节点与专用节点同样采集。
- 资源：Fluent Bit requests 50m/64Mi、limits 500m/256Mi；Loki requests 100m/256Mi、limits 1 核/1Gi。日志量大的集群需按实际调高。
- 采集器排除自身日志，避免后端故障时日志自我放大。业务 Pod 可加注解 `fluentbit.io/exclude: "true"` 不采集，或 `fluentbit.io/parser: json` 指定解析器。
- 两个组件都暴露 Prometheus 指标（Fluent Bit 2020 端口 `/api/v2/metrics/prometheus`，Loki 3100 端口 `/metrics`），Pod 已带 `prometheus.io/*` 注解。

## 已验证

- 两个清单在 Kubernetes 1.25 与 1.30 的 OpenAPI 模式下严格校验通过（kubeconform `-strict`，10 个资源）。
- `fluent-bit:3.2.10`（arm64 镜像内的二进制）`--dry-run` 校验 Loki 与 Elasticsearch 两种输出配置均通过。
- `loki:3.4.2` 二进制 `-verify-config` 校验配置通过，并以该配置本地启动成功。
- 本地联调：Fluent Bit 以本清单的 Loki 输出写入本地 Loki，按 `{job="fluent-bit",cluster="c1"}` 查回，标签为 job/cluster/node_name/namespace/pod/container，正文为原始日志行；CRI 格式日志行解析正确；缺少 Kubernetes 元数据时相应标签自动省略，不报错。

## 未验证

- 未在真实集群执行 `kubectl apply --dry-run=server` 或实际部署（沙箱 k3s 当时未运行，未启动）；未验证 kubernetes 过滤器在真实集群取元数据、RBAC 权限是否足够、只读挂载下 Docker 运行时节点的软链接解析。
- 未在平台日志页端到端查询（数据源配置、中央数据源集群标签值映射）。
- Elasticsearch 输出只做了配置语法校验，未连接真实 ES；`cluster.keyword` 的建议基于 ES 默认动态映射，未实测。
- Loki 的保留期清理（compactor）、PVC 扩容、重启后数据保留未实测；单副本无高可用。
- 资源限制为经验值，未压测。
