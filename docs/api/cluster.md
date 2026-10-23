# 集群模块（cluster）API

代码：`server/internal/modules/cluster`（module.go / client.go / read.go / ops.go / ai.go / migrations）
外部系统：Kubernetes API Server（`k8s.io/client-go v0.31.4`：typed clientset、dynamic client、discovery RESTMapper），可选 metrics-server（`metrics.k8s.io/v1beta1`，`k8s.io/metrics v0.31.4`）。
所有接口挂在已鉴权的 `/api/v1` 下，统一响应 `{code,message,data,request_id}`；以下示例只列 `data`。

## 数据与执行方式

- 表 `cluster_clusters`：name（唯一）、env（环境标签）、description、api_server、kubeconfig（`d.Crypto` AES-256-GCM 加密，接口只返回 `crypto.Mask` 结果）、version/status/last_error/checked_at（最近一次连通性测试）。
- 客户端缓存：按集群 ID 缓存 clientset/dynamic/metrics/RESTMapper，以 `updated_at` 作为版本键；编辑或删除集群时失效。请求超时 30s，QPS 50/Burst 100，UA `xingshu-matrix`。
- 环境范围：所有 `/clusters/:id/...` 接口都会校验 `Principal.CanEnv(cluster.env)`；列表按 `EnvScope` 过滤。
- **写操作审批**：节点/命名空间/工作负载/apply 等写操作统一走 `d.Approval.Gate`，审批类型 `cluster.change`。若该环境策略要求审批，返回 HTTP 202 + 审批单（`approval.RespondPending`）；审批通过后 `TypeSpec.Handler` 读取票据中的 payload（`{cluster_id, action, namespace, kind, name, params}`）重新加载集群并执行同一动作，结果写入票据 result。参数在开票前已校验，非法请求不会生成审批单。集群自身的增删改（平台配置）不走审批。
- 可选查询参数 `?reason=` 作为审批单理由。
- K8s 错误映射：NotFound→404、AlreadyExists/Conflict→409、Forbidden/Unauthorized→403、Invalid→400、429→429、其他→502(50200)。

## 权限

| 权限 | 说明 |
|---|---|
| cluster:read / cluster:manage | 查看 / 新增编辑删除集群 |
| node:read / node:write | 查看节点 / 封锁、驱逐、标签、污点 |
| namespace:read / namespace:write | 查看 / 创建删除命名空间 |
| workload:read / workload:write | 工作负载、日志、事件查看 / 扩缩容、重启、删除、YAML 应用 |

## 集群管理

### GET /clusters  `cluster:read`
查询：`page, page_size, env, keyword`
```json
{"items":[{"id":1,"name":"prod-sh","env":"prod","description":"","api_server":"https://10.0.0.1:6443","kubeconfig":"apiV***","version":"v1.30.4","status":"connected","last_error":"","checked_at":"2026-10-17T03:00:00Z","created_by":"admin","created_at":"...","updated_at":"..."}],"total":1,"page":1,"page_size":20}
```

### POST /clusters  `cluster:manage`
```json
{"name":"prod-sh","env":"prod","description":"上海生产","kubeconfig":"apiVersion: v1\nkind: Config\n...","context":"prod-admin","api_server":""}
```
校验 kubeconfig（clientcmd），`context` 可选（写入 current-context），`api_server` 为空时取 kubeconfig 中的地址；保存后立即探测 `/version` 并写入状态。返回 201 + 集群对象。名称重复 409。

### GET /clusters/:id  `cluster:read`
### PUT /clusters/:id  `cluster:manage`
请求同创建；`kubeconfig` 为空或包含掩码 `***` 时保留原值。修改后客户端缓存失效。
### DELETE /clusters/:id  `cluster:manage` → `{"id":1}`

### POST /clusters/:id/test  `cluster:read`
调用 discovery `/version`（15s 超时），更新状态。
```json
{"ok":true,"version":"v1.30.4"}   // 失败：{"ok":false,"error":"..."}
```
### POST /clusters/test  `cluster:manage`
保存前测试：`{"kubeconfig":"...","context":""}` → `{"ok":true,"version":"v1.31.4","api_server":"https://..."}`

## 节点

### GET /clusters/:id/nodes  `node:read`
节点列表 + metrics-server 用量（不可用时 `usage:null`、`metrics_available:false`）。
```json
{"items":[{"name":"node-1","ready":true,"status":"Ready","roles":["worker"],"unschedulable":false,"internal_ip":"10.0.0.11",
 "kubelet_version":"v1.30.4","os_image":"Ubuntu 22.04","kernel_version":"5.15","container_runtime":"containerd://1.7.2","architecture":"amd64",
 "capacity":{"cpu":"8","memory":"32Gi","pods":"110"},"allocatable":{"cpu":"7800m","memory":"30Gi","pods":"110"},
 "conditions":[{"type":"Ready","status":"True","reason":"KubeletReady"}],"labels":{},"taints":[],
 "usage":{"cpu_millicores":1200,"memory_bytes":8589934592,"cpu_percent":15.4,"memory_percent":26.7,"timestamp":"..."},"created_at":"..."}],
 "total":1,"metrics_available":true}
```
### GET /clusters/:id/nodes/:node  `node:read`
`{"node":{...同上},"metrics_available":true,"pods":[PodRow...]}`

### POST /clusters/:id/nodes/:node/cordon | /uncordon  `node:write`（审批）
StrategicMerge patch `spec.unschedulable`。返回 `null`。

### POST /clusters/:id/nodes/:node/drain  `node:write`（审批）
```json
{"grace_period_seconds":30,"ignore_daemonsets":true,"delete_emptydir_data":false,"force":false}
```
先 cordon，再对节点上 Pod 调用 Eviction API（policy/v1，遵守 PDB）。跳过：终止中、静态(mirror) Pod、已完成 Pod、DaemonSet Pod（ignore_daemonsets 默认 true）。无控制器 Pod 需 `force`，使用 emptyDir 需 `delete_emptydir_data`。
```json
{"node":"node-1","cordoned":true,"evicted":5,"failed":1,"pods":[{"namespace":"default","name":"web-1","status":"evicted"},{"namespace":"db","name":"pg-0","status":"failed","reason":"blocked by PodDisruptionBudget: ..."}]}
```

### PATCH /clusters/:id/nodes/:node/labels  `node:write`（审批）
`{"labels":{"zone":"a","old-key":null}}`（null 删除）→ `{"name":"node-1","labels":{...}}`
### PUT /clusters/:id/nodes/:node/taints  `node:write`（审批）
整表替换：`{"taints":[{"key":"dedicated","value":"gpu","effect":"NoSchedule"}]}` → `{"name":"node-1","taints":[...]}`

## 命名空间

- `GET /clusters/:id/namespaces?label_selector=`  `namespace:read` → `{"items":[{"name":"default","status":"Active","labels":{},"created_at":"..."}],"total":1}`
- `POST /clusters/:id/namespaces`  `namespace:write`（审批）`{"name":"team-a","labels":{"owner":"a"}}`
- `DELETE /clusters/:id/namespaces/:ns`  `namespace:write`（审批）→ `{"name":"team-a"}`

## 网络只读查询

网络视图使用集群当前 Kubernetes 对象，不生成虚拟拓扑关系。两个接口都校验集群环境范围与 `workload:read` 权限，支持 `namespace` 和 `label_selector` 查询参数。

### GET /clusters/:id/network/services

返回 Service 的命名空间、类型、ClusterIP、外部 IP、端口、selector、标签和创建时间：

```json
{"items":[
  {"namespace":"web-prod","name":"web","type":"ClusterIP","cluster_ip":"10.96.0.10",
   "external_ips":[],"ports":[],"selector":{"app":"web"},"labels":{},"created_at":"..."}
],"total":1}
```

### GET /clusters/:id/network/policies

返回 NetworkPolicy 的 selector、策略类型、Ingress/Egress 声明、标签和创建时间：

```json
{"items":[
  {"namespace":"web-prod","name":"web-default","labels":{},
   "pod_selector":{"matchLabels":{"app":"web"}},"policy_types":["Ingress"],
   "ingress":[],"egress":[],"created_at":"..."}
],"total":1}
```

当前版本只提供真实只读数据；NetworkPolicy 创建/修改、Istio 流量治理和跨集群聚合仍未实现。

## 工作负载

`:kind` 取值 `deployments|statefulsets|daemonsets|jobs|cronjobs|pods`（也接受 deploy/sts/ds/job/cj/pod 等别名）。

### GET /clusters/:id/workloads/:kind  `workload:read`
查询：`namespace`（空=全部）、`label_selector`、`field_selector`
```json
{"items":[{"kind":"Deployment","namespace":"default","name":"web","ready":"3/3","replicas":3,"status":"Available","images":["nginx:1.27"],"labels":{},"extra":{"updated":3,"available":3},"created_at":"..."}],"total":1}
```
pods 返回 PodRow：`{"namespace","name","phase","status"(CrashLoopBackOff 等),"ready":"1/1","restarts":0,"node","pod_ip","containers","images","owner":"ReplicaSet/web-7d9f","labels","created_at"}`。

### GET /clusters/:id/namespaces/:ns/:kind/:name  `workload:read`
dynamic client 获取对象，去掉 managedFields：`{"yaml":"apiVersion: apps/v1\n..."}`；`?format=json` 返回 `{"object":{...}}`。

### POST .../:kind/:name/scale  `workload:write`（审批）
仅 deployments/statefulsets，scale 子资源：`{"replicas":5}` → `{"kind":"deployments","namespace":"default","name":"web","from":3,"replicas":5}`
### POST .../:kind/:name/restart  `workload:write`（审批）
deployments/statefulsets/daemonsets；merge patch `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]`（与 kubectl rollout restart 相同）→ `{"restarted_at":"2026-10-17T04:00:00Z"}`
### DELETE .../:kind/:name  `workload:write`（审批）
可选 body `{"grace_period_seconds":0}`；Background 级联删除 → `{"deleted":"web"}`

### GET /clusters/:id/namespaces/:ns/pods/:name/logs  `workload:read`
查询：`container, tail_lines(默认500), since_seconds, previous, timestamps, follow`
- `follow` 非 true：`{"logs":"..."}`（上限 5MiB）
- `follow=true`：SSE（`text/event-stream`），每行一条 `event: log`，流结束 `event: end`，每 15s 发送 `: ping` 心跳；客户端断开即关闭上游流。

### GET /clusters/:id/events  `workload:read`
查询：`namespace, kind, name, type(Normal|Warning), limit(默认200,最大1000)`；按最近时间倒序。
```json
{"items":[{"namespace":"default","type":"Warning","reason":"BackOff","message":"Back-off restarting failed container","object_kind":"Pod","object_name":"web-1","source":"kubelet","count":12,"first_seen":"...","last_seen":"..."}],"total":1}
```

### POST /clusters/:id/apply  `workload:write`（审批）
```json
{"yaml":"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: demo}\ndata: {a: b}\n---\n...","namespace":"default","dry_run":false,"force":true}
```
多文档 YAML/JSON 逐个 Server-Side Apply（fieldManager `xingshu-matrix`，默认 force conflicts），通过 discovery RESTMapper 解析 GVR，命名空间对象缺省使用 `namespace`（再缺省 `default`），集群级对象清空 namespace。单个文档失败不影响其它文档。
```json
{"dry_run":false,"failed":0,"objects":[{"api_version":"v1","kind":"ConfigMap","namespace":"default","name":"demo","status":"applied"}]}
```

## AI 工具（d.AITools）

| 名称 | 权限 | 确认 | 参数 |
|---|---|---|---|
| k8s_list_pods | workload:read | 否 | cluster（ID 或名称）, namespace, label_selector, limit(默认100) |
| k8s_get_events | workload:read | 否 | cluster, namespace, kind, name, type, limit(默认50) |
| k8s_scale_deployment | workload:write | **是** | cluster, namespace, name, replicas |

工具内校验 `rbac.FromContext(ctx).CanEnv(env)`；扩缩容走与 HTTP 相同的 `Gate`，需审批时返回 `{"pending_approval":true,"ticket_id":12}`。

## 未实现 / 限制

- 仅支持 kubeconfig 接入；未实现 matrix-cluster-agent 反向隧道、Web Shell（exec）、端口转发。
- drain 不等待 Pod 终止完成、不重试 PDB 阻塞的驱逐（结果中标记 failed，可再次调用）。
- 无 Informer/Watch 缓存，列表每次直连 API Server；未分页（大集群 pods 全量返回，AI 工具有 limit 截断）。
- 未提供 Service/Ingress/ConfigMap/Secret/PVC 等资源的专用接口（可用 apply 写入）；Pod 级 metrics 未接入。
- 回滚到指定 ReplicaSet 版本、HPA 管理、YAML diff 预览未实现（apply 可用 `dry_run`）。
- 新模块需在 `internal/modules/registry.go` 中加入 `cluster.New()`（本模块未改动 registry.go）。
