# CI/CD 模块（`cicd`）

代码：`server/internal/modules/cicd/`（module.go / runs.go / k8s.go / deploy.go / hooks.go / migrations/0001_init.up.sql）。
所有接口前缀 `/api/v1/cicd`，响应为统一信封 `{code,message,data,request_id}`（下文示例只写 `data`）。

## 数据源 / 外部系统

| 系统 | 用途 | 方式 |
|---|---|---|
| Kubernetes（执行集群 = runner target） | 每个构建/测试阶段一个 `batch/v1 Job`，git-clone initContainer + 步骤容器；读取 Pod 日志 | client-go v0.31，kubeconfig 由 `d.Crypto` 加密存 `cicd_targets`，**不依赖 cluster 模块** |
| Kubernetes（部署目标） | `apps/v1 Deployment` 策略合并补丁改镜像并等待 rollout；或运行 `helm upgrade --install` Job | 同上（环境里的 `target_id` 指向一个 runner target） |
| 镜像仓库 | Kaniko `gcr.io/kaniko-project/executor:v1.23.2` 构建并推送 | 推送凭据为 runner 命名空间内已存在的 `kubernetes.io/dockerconfigjson` Secret（阶段字段 `registry_secret`） |
| Git（GitLab / GitHub / 任意 https/ssh） | `alpine/git:2.45.2` 浅克隆；GitLab `X-Gitlab-Token`、GitHub `X-Hub-Signature-256` Webhook | Git Token 加密入库，运行时仅写入单次运行的 K8s Secret `cicd-run-<id>`，运行结束删除 |
| Jenkins（可选） | 阶段类型 `jenkins`：取 crumb → `POST /job/<a>/job/<b>/buildWithParameters` → 轮询队列 → 跟随 `logText/progressiveText` → 读取 `result` | Basic Auth（用户名 + API Token，Token 加密存 `cicd_jenkins`），Cookie Jar 保持 crumb 会话 |
| 审批 | 部署/回滚走 `d.Approval.Gate`，类型 `cicd.deploy` | 环境策略要求审批时开单，审批通过后执行 |

## 权限

| 权限 | 说明 |
|---|---|
| `cicd:read` | 查看应用、流水线、运行、日志、发布记录 |
| `cicd:manage` | 管理执行集群、Jenkins、应用、流水线、Webhook 密钥 |
| `cicd:run` | 运行 / 取消流水线 |
| `cicd:deploy` | 直接部署 / 回滚（并受用户环境范围 `env_scope` 限制） |

## 执行集群（runner targets）

### GET /cicd/targets — `cicd:read`
```json
[{"id":1,"name":"ci-cluster","namespace":"matrix-cicd","kubeconfig":"********","created_at":"2026-10-17T04:00:00Z"}]
```
### POST /cicd/targets — `cicd:manage`
```json
{"name":"ci-cluster","kubeconfig":"apiVersion: v1\nkind: Config\n...","namespace":"matrix-cicd"}
```
→ 201 `{"id":1,"name":"ci-cluster","namespace":"matrix-cicd"}`。kubeconfig 先用 `clientcmd` 解析校验，再加密入库；`namespace` 为 Job 运行命名空间（需预先创建，且执行身份需能 create/get/delete Jobs、Secrets，list/get Pods、pods/log）。
### DELETE /cicd/targets/:id — `cicd:manage`（被流水线引用时 409）

## Jenkins 服务器

- `GET /cicd/jenkins` — `cicd:read`，token 掩码。
- `POST /cicd/jenkins` — `cicd:manage`：`{"name":"jk","url":"https://jenkins.example.com","username":"bot","token":"11ab..."}`
- `DELETE /cicd/jenkins/:id` — `cicd:manage`

## 应用

### POST /cicd/applications — `cicd:manage`
```json
{
  "name": "web",
  "repo_url": "https://gitlab.example.com/team/web.git",
  "branch": "main",
  "dockerfile": "Dockerfile",
  "context_dir": ".",
  "image_repo": "harbor.example.com/team/web",
  "git_username": "oauth2",
  "git_token": "glpat-xxxx",
  "envs": [
    {"name":"test","target_id":1,"namespace":"web-test","deployment":"web","container":"app","rollout_timeout_sec":600},
    {"name":"prod","target_id":2,"namespace":"web","helm":{"release":"web","chart":"oci://harbor.example.com/charts/web","version":"1.4.0",
      "image_repo_key":"image.repository","image_tag_key":"image.tag","set":["replicaCount=3"]}}
  ]
}
```
→ 201，返回应用（`git_token` 掩码为 `********`），并**仅此一次**返回 `webhook_secret`。
环境 `name` 即平台环境名（与 RBAC 环境范围、环境审批策略对应）；每个环境要么 `deployment+container`，要么 `helm`。

- `GET /cicd/applications?page=1&size=20` — `cicd:read`，分页 `{items,total,page,size}`
- `GET /cicd/applications/:id` — `cicd:read`
- `GET /cicd/applications/:id/inventory?env=prod` — `cicd:read`，读取应用环境在目标集群中的实时运行清单。返回 Deployment、匹配 Pod、容器状态与匹配 Service；`env` 为空时使用应用配置的第一个环境。

```json
{
  "application": "web",
  "environment": "prod",
  "target_id": 2,
  "namespace": "web-prod",
  "observed_at": "2026-10-24T09:00:00Z",
  "workloads": [
    {"kind":"Deployment","name":"web","namespace":"web-prod","desired":3,"ready":3,"available":3,
     "images":["harbor.example.com/team/web:3f2a9c1"],"selector":{"app":"web"},
     "created_at":"...","observed_generation":12}
  ],
  "pods": [
    {"name":"web-7d9f-abc","namespace":"web-prod","phase":"Running","status":"Running",
     "ready":"1/1","restarts":0,"node":"node-1","pod_ip":"10.0.0.10",
     "owner":"ReplicaSet/web-7d9f","created_at":"...","containers":[]}
  ],
  "services": [
    {"name":"web","namespace":"web-prod","type":"ClusterIP","cluster_ip":"10.96.0.10",
     "external_ips":[],"ports":[],"selector":{"app":"web"}}
  ],
  "warnings": []
}
```

该接口只读取应用已配置的 Deployment 或 Helm release 目标，不扫描或猜测其它应用的资源；无目标资源时通过 `warnings` 返回可读提示。
- `PUT /cicd/applications/:id` — `cicd:manage`，请求体同创建；`git_token` 为空或 `********` 时保留原值
- `DELETE /cicd/applications/:id` — `cicd:manage`（级联删除流水线、运行、发布记录）
- `POST /cicd/applications/:id/webhook-secret` — `cicd:manage`，轮换密钥：`{"webhook_secret":"…","url":"/api/v1/cicd/webhooks/3"}`

### GET /cicd/applications/:id/releases?env=prod — `cicd:read`
```json
[{"id":12,"app_id":3,"env":"prod","run_id":88,"image":"harbor.example.com/team/web:3f2a9c1d0b7e","previous_image":"harbor.example.com/team/web:a1b2c3d4e5f6",
  "kind":"deploy","status":"succeeded","message":"rolled out","created_by":"alice","created_at":"…","finished_at":"…"}]
```

### POST /cicd/applications/:id/deploy — `cicd:deploy`
```json
{"env":"prod","image":"harbor.example.com/team/web:v1.2.0","reason":"hotfix"}
```
- 环境需审批：HTTP 202（`approval.RespondPending`），审批通过后由 `cicd.deploy` 处理器同步执行部署。
- 不需审批：立即创建发布记录（`status: running`）并在后台执行，返回该记录，前端轮询 releases 查看结果。

### POST /cicd/applications/:id/rollback — `cicd:deploy`
```json
{"env":"prod"}                     // 回滚到最近一次成功发布所替换的镜像（previous_image）
{"env":"prod","release_id":10}     // 重新部署指定发布记录的镜像
```
审批与执行方式同 deploy，发布记录 `kind: rollback`。

## 流水线

### POST /cicd/pipelines — `cicd:manage`
```json
{
  "app_id": 3,
  "name": "main",
  "runner_target_id": 1,
  "on_push": true,
  "stages": [
    {"name":"unit-test","type":"test","image":"golang:1.23","commands":["go test ./..."],"env":{"GOFLAGS":"-mod=mod"},"timeout_sec":1800},
    {"name":"build","type":"build","registry_secret":"harbor-push","extra_args":["--cache=true"]},
    {"name":"deploy-test","type":"deploy","deploy_env":"test"},
    {"name":"e2e","type":"jenkins","jenkins_id":1,"jenkins_job":"qa/web-e2e","params":{"IMAGE":"${IMAGE}","BRANCH":"${BRANCH}"}},
    {"name":"deploy-prod","type":"deploy","deploy_env":"prod"}
  ]
}
```
阶段类型：
- `build`：Kaniko，`--context=dir:///workspace/src/<context_dir> --dockerfile=… --destination=<image_repo>:<image_tag>`（`image` 字段可覆盖目标镜像）。构建+推送在同一 Job 内完成。
- `test` / `script`：在 `image` 中 `sh -c "set -e; <commands>"`，工作目录为克隆后的源码。注入 `CI_RUN_ID / CI_BRANCH / CI_COMMIT / CI_IMAGE_TAG`。
- `deploy`：部署 `<image_repo>:<image_tag>`（或 `image`）到 `deploy_env`，在平台进程内用 client-go 执行（Helm 环境则起 Job）。
- `jenkins`：触发 Jenkins Job；参数支持 `${BRANCH} ${COMMIT} ${IMAGE_TAG} ${IMAGE} ${RUN_ID}` 占位符；结果非 `SUCCESS` 即失败。

阶段名需为 DNS label（≤40）。其他：`GET /cicd/pipelines?app_id=3`、`GET/PUT/DELETE /cicd/pipelines/:id`。

### POST /cicd/pipelines/:id/run — `cicd:run`
```json
{"branch":"main","commit":"3f2a9c1d0b7e5a..."}   // 均可省略：默认应用分支、分支 HEAD
```
→ 201
```json
{"id":88,"pipeline_id":5,"app_id":3,"status":"queued","trigger":"manual","triggered_by_id":7,"triggered_by":"alice",
 "branch":"main","commit":"3f2a9c1d0b7e5a...","image_tag":"3f2a9c1d0b7e","error":"","created_at":"…","started_at":null,"finished_at":null}
```
`image_tag` = commit 前 12 位，未指定 commit 时为 `run-<id>`。包含部署阶段时，调用者须在相应环境范围内。

## 运行

- `GET /cicd/runs?pipeline_id=&app_id=&status=&page=&size=` — `cicd:read`
- `GET /cicd/runs/:id` — `cicd:read`，含 `stages`：
```json
{"id":88,"status":"running","stages":[
  {"idx":0,"name":"unit-test","type":"test","status":"succeeded","job_name":"cicd-88-0-unit-test","approval_id":null,"error":"","started_at":"…","finished_at":"…"},
  {"idx":4,"name":"deploy-prod","type":"deploy","status":"awaiting_approval","job_name":"","approval_id":41,"error":""}]}
```
运行状态：`queued → running → succeeded | failed | cancelled`（取消中为 `cancelling`）。
阶段状态：`pending / running / awaiting_approval / approved / rejected / succeeded / failed / skipped / cancelled`。

### GET /cicd/runs/:id/logs — `cicd:read`
- 普通请求：JSON 数组（最多 5000 行，`?after=<id>` 翻页）：`[{"id":1001,"stage":0,"line":"ok  ./...","ts":"…"}]`（`stage=-1` 为运行级消息）。
- `Accept: text/event-stream` 或 `?follow=1`：SSE。每行一个 `event: log`（`id:` 为日志行 id，支持 `Last-Event-ID` 断点续传），运行结束时发送 `event: end`，`data: {"status":"succeeded"}`；每 15s 发送注释心跳。

### POST /cicd/runs/:id/cancel — `cicd:run`
排队中直接置为 `cancelled`；运行中置为 `cancelling` 并取消执行上下文：当前 Job 以 Background 级联删除，Jenkins 构建调用 `/stop`，等待审批的阶段停止等待。其他副本在 3s 内通过轮询数据库感知取消。
```json
{"id":88,"status":"cancelling"}
```

## Webhook（无需登录，挂在 `d.Public`）

### POST /api/v1/cicd/webhooks/:appId
- GitLab：在项目 Webhook 中填写 Secret Token = 应用的 `webhook_secret`，校验 `X-Gitlab-Token`（常量时间比较），事件 `X-Gitlab-Event: Push Hook`。
- GitHub：Secret = `webhook_secret`，校验 `X-Hub-Signature-256`（HMAC-SHA256 原始请求体），事件 `X-GitHub-Event: push`（`ping` 等其他事件返回 `{"ignored":…}`）。
- 只处理推送到应用 `branch` 的事件（忽略删除分支），对该应用所有 `on_push=true` 的流水线各创建一次运行（commit 取 `checkout_sha`/`after`，触发方式 `webhook:gitlab|github`，触发人 `webhook`）。
- 签名失败 401。成功：`{"runs":[89,90]}`。

## 执行过程

1. `Start()` 启动调度器：每 3s（或新建运行时立即）用 `FOR UPDATE SKIP LOCKED` 认领 `queued` 运行，单进程最多并发 4 个；可多副本部署。启动时把遗留的 `running/cancelling` 运行标记为失败（进程重启无法续跑）。
2. 运行开始时在 runner 命名空间创建 Secret `cicd-run-<id>`（含带凭据的克隆 URL），运行结束删除。
3. 构建/测试阶段：创建 Job `cicd-<run>-<idx>-<stage>`（`backoffLimit=0`，`activeDeadlineSeconds=timeout_sec`（默认 3600），`ttlSecondsAfterFinished=3600`）。Pod：initContainer `git-clone` 浅克隆到 emptyDir `/workspace/src`（输出中的凭据被脱敏），主容器 `step`。每 2s 轮询 Job 状态；按顺序 Follow 各容器日志，批量（COPY）写入 `cicd_run_logs`（每 500ms 或 200 行刷新）。容器处于 `ErrImagePull/ImagePullBackOff/CreateContainerConfigError` 等不可恢复状态时立即失败并删除 Job。
4. 部署阶段：调用 `Approval.Gate`（`cicd.deploy`，env = 环境名，申请人 = 运行触发人）。需审批则阶段置 `awaiting_approval` 并记录 `approval_id`，每 3s 轮询；审批通过处理器把阶段置为 `approved` 后继续，驳回/撤销则阶段 `rejected`、运行失败。随后：
   - Deployment：读取当前容器镜像作为 `previous_image`，策略合并补丁（同时写注解 `matrix.cicd/deployed-at`），按 `kubectl rollout status` 规则等待（observedGeneration、updated/available replicas、`ProgressDeadlineExceeded`），超时默认 10 分钟。
   - Helm：在目标集群的 target 命名空间创建 kubeconfig Secret，运行 `alpine/helm:3.16.3` Job：`helm upgrade --install <release> <chart> -n <ns> --wait --timeout … --set image.repository=… --set image.tag=…`；`previous_image` 取该环境上一次成功发布的镜像。
   - 每次部署写一条 `cicd_releases`，回滚基于其中的 `previous_image`。
5. 任一阶段失败，其余阶段标记 `skipped`，运行 `failed`。

## AI 工具

| 工具 | 权限 | 说明 |
|---|---|---|
| `cicd_list_runs` | `cicd:read` | 参数 `{pipeline_id?, app_id?, status?, limit?≤50}`，返回运行列表 |
| `cicd_run_pipeline` | `cicd:run`，`RequiresConfirmation: true` | 参数 `{pipeline_id, branch?, commit?}`；生产部署阶段仍会走审批 |

## 数据表

`cicd_targets`、`cicd_jenkins`、`cicd_applications`、`cicd_pipelines`、`cicd_runs`、`cicd_run_stages`、`cicd_run_logs`、`cicd_releases`。

## 未实现 / 限制

- 未实现 Tekton 执行器、阶段并行/矩阵、制品缓存卷、手动重试单个阶段；每个 K8s 阶段各自重新克隆代码（不共享工作区）。
- 进程重启时正在执行的运行直接标记失败，不做断点续跑（Job 仍会在集群中跑完并由 TTL 清理）。
- Kaniko 推送凭据需预先在 runner 命名空间创建 dockerconfigjson Secret，平台不托管镜像仓库凭据；SSH 方式克隆需自行在镜像中提供密钥（仅 https Token 由平台注入）。
- Helm 回滚通过重新 `upgrade` 旧镜像实现，未调用 `helm rollback`；Helm 部署的 `previous_image` 取自平台发布记录而非集群实际状态。
- 直接部署（非流水线）的过程日志只写服务端日志，接口通过发布记录的 `status/message` 反映结果。
- Webhook 只处理 push 事件；不支持 Tag / Merge Request 事件、Gitee/Gitea 签名。
- 运行日志无自动清理策略。
- 本模块未在 `internal/modules/registry.go` 中注册（按规则由集成方添加 `cicd.New()`）。
