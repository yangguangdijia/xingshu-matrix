# CI/CD 模块（`cicd`）

代码：`server/internal/modules/cicd/`（module.go / runs.go / k8s.go / deploy.go / hooks.go / cache.go（编译缓存）/ detect.go（语言识别）/ migrations/）。
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

## 凭证（credentials）

统一保存 Git 与镜像仓库凭证，密钥用平台密钥加密（`cicd_credentials.secret_enc`），任何接口都不返回明文。

- GET `/cicd/credentials[?type=git|git_token|git_basic|ssh_key|registry]`（`cicd:read`）：返回 `{items,total}`，每项含 `has_secret`、`used_by`（被应用与流水线引用的次数）。
- POST `/cicd/credentials`（`cicd:manage`）：
```json
{"name":"gitlab-ci","type":"git_token","username":"","secret":"glpat-xxx","description":"只读"}
{"name":"git-bot","type":"git_basic","username":"bot","secret":"***"}
{"name":"deploy-key","type":"ssh_key","secret":"-----BEGIN OPENSSH PRIVATE KEY-----\n..."}
{"name":"harbor-ci","type":"registry","host":"harbor.example.com","username":"robot$ci","secret":"***"}
```
  `name` 为 DNS label；`git_basic`/`registry` 必须有用户名；`registry` 的 `host` 只填 `主机[:端口]`；`ssh_key` 必须是 PEM/OpenSSH 私钥。
- PUT `/cicd/credentials/:id`：`type` 不可改；`secret` 省略或为空表示保留原值。
- DELETE `/cicd/credentials/:id`：仍被应用（`credential_id`）或流水线构建阶段（`registry_credential_id`）引用时返回 409 `credential is in use`。

引用方式：
- 应用 `credential_id`（Git 类凭证）：`git_token`/`git_basic` 需要 http(s) 仓库，运行时把账号密码写入本次运行的临时 Secret 中的克隆地址；`ssh_key` 仓库地址可写 `git@host:org/repo.git` 或 `ssh://`，私钥以 0400 挂载到克隆容器并设置 `GIT_SSH_COMMAND`（`StrictHostKeyChecking=accept-new`）。旧版应用内嵌 `git_token` 仍可用，`credential_id` 优先。
- 构建阶段 `registry_credential_id`：运行时生成 `.dockerconfigjson` 写入本次运行 Secret（键 `dockerconfig-<阶段序号>`），挂载到 Kaniko 的 `/kaniko/.docker/config.json`；优先于 `registry_secret`。
- 运行 Secret `cicd-run-<id>` 在运行结束后删除；使用了凭证的阶段不回显克隆输出。

## 代码仓库探测

- POST `/cicd/repos/probe`（`cicd:read`）：`{"repo_url":"…","credential_id":0,"app_id":0}` → `{"branches":["main","develop"],"default_branch":"main","head":"<sha>"}`。
  - https/http 走 Git smart HTTP（`info/refs?service=git-upload-pack`，10 秒超时，凭证不会被转发到跳转后的其他主机）；`git://` 走 git daemon 协议（9418）。
  - 401/403 返回 `authentication failed: check the credential`；SSH 仓库不在服务端探测（返回 400，运行时用私钥克隆）。
  - `app_id` 且仓库地址未变时，可沿用该应用已保存的凭证或旧版 Token。

## 从源码识别语言

### POST /cicd/repos/detect — `cicd:manage`
```json
{"repo_url":"git://git-server.cicd-demo.svc:9418/hello-web.git","branch":"main","credential_id":0,"app_id":0,
 "runner_target_id":1,"context_dir":".","dockerfile":"Dockerfile"}
```
- 在 `runner_target_id` 指定的执行集群（省略或 0 时取 id 最小的执行集群）里创建一次性 Job `cicd-detect-<随机>`：**复用流水线的 git-clone 初始化容器与凭证注入**（临时 Secret `cicd-detect-<随机>` 中的带凭据克隆地址，或 SSH 私钥 0400 挂载），浅克隆所选分支，再用 `alpine/git` 列出深度 ≤4 的文件（最多 3000 个）并读取 `context_dir` 下少量标志文件的前 64 KiB（go.mod、package.json、pom.xml、build.gradle(.kts)、requirements.txt、pyproject.toml、.nvmrc、.node-version、.python-version、runtime.txt）。Job 与 Secret 结束即删除；Job 超时 110 秒，接口总超时约 130 秒。
- 调用者须在执行集群所属环境范围内（`env_scope`），否则 403。`app_id` 且仓库地址未变时沿用该应用已保存的凭证 / 旧版 Token。
- 识别规则（`DetectLanguage`，按优先级）：`go.mod`→Go；`package.json`→Node（`pnpm-lock.yaml`→pnpm、`yarn.lock`→yarn、`package-lock.json`/`npm-shrinkwrap.json`→npm，无锁文件时看 `packageManager`，否则 `npm install`）；`pom.xml`→Maven；`build.gradle(.kts)`→Gradle；`requirements.txt`/`pyproject.toml`→Python（含 `[tool.poetry]` 为 Poetry）。同时出现多个时取优先级最高者，其余写入 `others`。版本：go.mod 的 `toolchain`/`go`、`.nvmrc`/`.node-version`/`engines.node`（钳制到 18–24）、pom/gradle 的 Java 版本（映射到 8/11/17/21）、`.python-version`/`runtime.txt`/`requires-python`（3.8–3.13）。
- Dockerfile：先查请求里的 `dockerfile`，再查 `<context_dir>/Dockerfile|dockerfile|Containerfile`；存在则 `build_mode=dockerfile`（优先用 Dockerfile 构建），否则有语言时 `build_mode=template`（构建阶段带 `dockerfile_template`），都没有为 `none`。
- 响应：
```json
{"language":"go","build_tool":"go","version":"1.22","has_dockerfile":false,"dockerfile":"Dockerfile","context_dir":".",
 "build_mode":"template","evidence":["go.mod"],"others":[],"notes":["仓库中没有 Dockerfile，构建阶段将使用内置模板生成…"],
 "branch":"main","commit":"<sha>",
 "template":{"stages":[
   {"name":"test","type":"test","image":"golang:1.22","commands":["go mod download","go vet ./...","go test ./..."]},
   {"name":"build","type":"build","dockerfile_template":"go:1.22"}],
  "cache":{"enabled":true,"language":"go","key":"{{branch}}","paths":["/go/pkg/mod","/root/.cache/go-build"],"storage":"pvc","size":"5Gi","access_mode":"ReadWriteOnce","layer_cache":true}}}
```
- 错误：400 `cannot clone the repository: check the URL, branch and credential (…)`（使用凭证时不附带克隆输出）、400 `the execution cluster cannot pull the Git image …`、400 `invalid branch name` / `context_dir and dockerfile must be relative paths…` / `unknown runner target`、409 `no execution cluster is configured`、403 环境范围。
- 前端：「从代码仓库创建应用」向导「构建」一步的「识别语言」按钮，回填 Dockerfile 路径与识别结果；创建应用时可勾选「同时生成流水线」，按模板创建 `<应用名>-ci` 流水线（测试 → 镜像构建 → 部署到第一个环境，带缓存配置，可选镜像推送凭证）。

### 构建阶段 `dockerfile_template`
取值 `go:<版本>`、`node:<版本>[:npm|npm-install|yarn|pnpm]`、`maven:<JDK>`、`gradle:<JDK>`、`python:<版本>[:requirements|pyproject]`。运行时在 git-clone 之后追加初始化容器 `dockerfile-template`：仓库中 `<应用 dockerfile 路径>` 已存在则直接使用，否则写入内置多阶段 Dockerfile（Go 静态编译到 alpine；Node `npm start`；Maven/Gradle 取第一个可执行 jar 跑在 `eclipse-temurin:<JDK>-jre`；Python 依次尝试 manage.py/main.py/app.py）。非法取值在保存流水线时 400。

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

## 金丝雀发布

在 Deployment 方式部署的应用环境上，把新镜像先以独立的金丝雀实例上线，通过 ingress-nginx 的 canary 注解按步骤放量，确认后全量替换主版本。实现见 `server/internal/modules/cicd/canary.go`、`canary_store.go`，迁移 `0009_delivery_safety`（建表）与 `0010_canary`（执行字段）。前端：`public/xs-release-canary.js`（`window.XSReleaseCanary.render(el,{appId,env})`、`openCreate(appId,env)`）。

工作方式：发起后平台在主工作负载所在命名空间创建 `<deployment>-canary`（克隆主 Deployment，只换镜像，选择器标签加 `-canary` 后缀避免被主服务选中）、`<service>-canary`（只选金丝雀实例）、`<ingress>-canary`（复制主 Ingress 中指向该服务的规则，注解 `nginx.ingress.kubernetes.io/canary: "true"` 与 `canary-weight`）。全量时把主 Deployment 镜像切到新镜像并等待滚动完成，再删除三个金丝雀资源；回滚时先把权重置 0 再删除资源。平台只操作自己创建（带 `app.kubernetes.io/managed-by=matrix-cicd` 与 `matrix.cicd/canary-id` 标签）的同名对象，同名的外部对象会返回 409。

### GET /cicd/applications/:id/canaries?env=prod — `cicd:read`
按 id 倒序最多 100 条；`env` 可选。返回 `Canary` 数组（不含 `events`）。

### POST /cicd/applications/:id/canaries — `cicd:deploy`
```json
{"env":"prod","image":"harbor.example.com/team/web:v2","service":"web","ingress":"web",
 "steps":[{"weight":10,"pause_sec":300,"manual":false},{"weight":30,"pause_sec":300,"manual":false},
          {"weight":50,"pause_sec":0,"manual":true},{"weight":100,"pause_sec":0,"manual":true}],
 "replicas":1,"auto_promote":false,"reason":"新版本灰度"}
```
| 字段 | 说明 |
|---|---|
| `env` | 必填，应用已配置的环境；须在调用者的环境范围内（否则 403） |
| `image` | 必填，≤512 字符，不含空白 |
| `service` | 必填，主服务名（DNS-1035，≤56 字符，以便生成 `-canary` 名称） |
| `ingress` | 必填，主路由名（DNS-1123 子域名）；不能本身是金丝雀路由 |
| `steps` | 可省略（默认 10/30/50/100，前两步各观察 300 秒，后两步人工确认）；最多 20 步；`weight` 1–100 且严格递增；`pause_sec` 0–86400；`manual=true` 的步骤生效后停在 `awaiting_confirm` |
| `replicas` | 金丝雀实例数，默认 1，范围 1–50 |
| `auto_promote` | 最后一步（非人工）观察结束后自动全量；为 false 时进入 `awaiting_confirm` 等待全量 |
| `reason` | ≤2000 字符，写入审批单与时间线 |

- 不需审批：200，返回 `Canary`（`status: starting`），后台创建资源、等待实例就绪后进入第 1 步。
- 需审批：HTTP 202（`code: 40304`，`data: {"approval_required":true,"ticket":{...}}`），金丝雀记录为 `awaiting_approval`，审批通过后自动启动；审批拒绝 → `rejected`。

### GET /cicd/canaries/:id — `cicd:read`
返回 `Canary`，附 `events` 时间线：`[{"id":1,"kind":"operator","message":"发起金丝雀发布","data":{...},"actor":"alice","ts":"…"}]`，`kind` 取 `info | step | operator | error`。

`Canary` 字段：`id, app_id, env, target_id, release_id, image, stable_image（启动时读取的主版本镜像）, namespace, deployment, container, service, ingress, canary_deployment, canary_service, canary_ingress, plan{steps,replicas,auto_promote}, status, current_step（从 0 计，未开始为 -1）, weight（当前金丝雀流量百分比）, step_started_at, next_at（progressing 时为自动进入下一阶段的时间）, attempts, message, approval_id, pending_action（start|advance|promote，等待审批中的动作）, reason, created_by, created_at, updated_at, finished_at`。

### POST /cicd/canaries/:id/{advance|promote|pause|resume|rollback} — `cicd:deploy`
请求体可选 `{"reason":"…"}`（≤2000 字符）。

| 动作 | 允许的状态 | 说明 |
|---|---|---|
| `advance` | progressing / awaiting_confirm / paused | 进入下一步并调整权重；已是最后一步返回 409「请执行全量发布」；走审批 |
| `promote` | progressing / awaiting_confirm / paused | 主版本切到新镜像并清理金丝雀资源，异步执行（`promoting`），生成一条 `kind: canary` 的发布记录；走审批；失败时回到 `awaiting_confirm` 可重试或回滚 |
| `pause` | progressing / awaiting_confirm | 停止自动推进，权重保持不变；不走审批 |
| `resume` | paused | 重新进入当前步骤（重新计时）；不走审批 |
| `rollback` | pending / awaiting_approval / progressing / awaiting_confirm / paused / rolling_back | 权重置 0 并删除金丝雀资源（异步，`rolling_back`，失败自动重试，5 次后 `failed`）；尚未创建资源（pending / awaiting_approval）时直接 `cancelled`；不走审批 |

已有待审批动作时，除 `pause`、`rollback` 外的动作返回 409。审批类型 `cicd.canary`（发起 / 推进 / 全量），审批单超时 30 分钟，审批通过时会重新校验金丝雀状态、步骤与部署目标，不一致则审批单失效。

### 状态
| 值 | 含义（界面文字） |
|---|---|
| `pending` | 待启动 |
| `awaiting_approval` | 等待审批（仅发起阶段） |
| `starting` | 启动中：创建金丝雀实例并等待就绪 |
| `progressing` | 观察中：当前步骤计时，到 `next_at` 自动进入下一步 |
| `awaiting_confirm` | 等待确认：人工确认步骤已生效，或全部步骤结束且未开启自动全量，或全量失败待重试 |
| `paused` | 已暂停 |
| `promoting` | 全量发布中 |
| `rolling_back` | 回滚中 |
| `succeeded` | 已全量（结束） |
| `rolled_back` | 已回滚（结束） |
| `failed` | 失败（结束；启动失败会尝试清理金丝雀资源） |
| `rejected` | 审批未通过（结束） |
| `cancelled` | 已取消（结束） |

服务重启时，`starting` 中的金丝雀会被清理并标记 `failed`（需重新发起）；`promoting` / `rolling_back` 会由后台任务继续执行。

### 错误码
| HTTP | code | 场景 |
|---|---|---|
| 400 | 40001 | 字段缺失或不合法；步骤权重不在 1–100 或未严格递增；观察时长越界；副本数越界；名称过长无法生成 `-canary` 名称；环境不存在 |
| 403 | 40300 | 缺少 `cicd:deploy` / `cicd:read`，或环境不在调用者的环境范围内 |
| 404 | 40400 | 应用或金丝雀不存在 |
| 409 | 40900 | **Helm 部署的环境不支持金丝雀发布**；环境未配置工作负载/容器/命名空间；当前状态不允许该操作；已有待审批动作；已是最后一步；正在执行其他操作；部署目标已变更；同名资源已存在且非本平台创建；所选路由本身是金丝雀路由或没有指向该服务的规则；主服务没有标签选择器 |
| 202 | 40304 | 需要审批，已创建审批单（不是错误，前端提示「已提交审批」） |
| 503 | — | 审批服务不可用，金丝雀操作被拒绝 |

### 未验证项
- **未在真实 ingress-nginx 上验证**：canary 注解（`canary` / `canary-weight`）的实际分流比例、`kubernetes.io/ingress.class` 注解与 `ingressClassName` 的兼容、同一主机多个金丝雀路由的行为，均只有单元测试（fake clientset）。其他 Ingress 控制器（Traefik、APISIX 等）不识别这些注解，金丝雀路由会被当作普通路由，**不能使用**。
- 金丝雀实例只复制主 Deployment 的 Pod 模板；HPA、PDB、ServiceMonitor 等附属对象不会复制。
- 未接入指标自动判定（错误率 / 延迟），步骤推进只按时间或人工确认。
- 前端（`xs-release-canary.js`）只在测试替身上运行过（`tests/frontend/xs-release-canary.test.mjs`），未在浏览器中对接真实后端验证。

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

### 编译缓存

流水线新增字段 `cache`（`POST/PUT /cicd/pipelines` 可带；PUT 省略 `cache` 表示保持不变）。也可单独读写：

#### GET /cicd/pipelines/:id/cache — `cicd:read`
```json
{"configured":true,
 "config":{"enabled":true,"language":"go","key":"{{branch}}","paths":["/go/pkg/mod","/root/.cache/go-build"],
           "storage":"pvc","storage_class":"","size":"5Gi","access_mode":"ReadWriteOnce",
           "layer_cache":true,"layer_cache_repo":"","layer_cache_ttl_hours":0},
 "resolved_paths":["/go/pkg/mod","/root/.cache/go-build"],
 "storage":{"type":"pvc","name":"cicd-cache-pipeline-5","exists":true,"phase":"Bound","capacity":"5Gi","storage_class":"local-path"},
 "usage_bytes":123731968,"usage_at":"…",
 "last_run":{"run_id":91,"at":"…","result":"hit","key":"main"},
 "last_hit":{"run_id":91,"at":"…","result":"hit","key":"main"},
 "last_layer_hit":{"run_id":91,"at":"…","result":"hit","key":"","hits":4},
 "layer_cache_repo":"10.42.0.1:5000/demo/hello-web/cache"}
```
- `storage` 为实时读取执行集群上的 PVC（8 秒超时），读不到时 `exists=null` 并给出 `error`；hostPath 方式不在服务端探测。
- `usage_bytes` 来自最近一次运行中 cache-prepare 容器 `du -sk`（20 秒超时）的结果，**拿不到就是 null，不做估算**；`last_*` 取自最近 50 次带缓存记录的运行。

#### PUT /cicd/pipelines/:id/cache — `cicd:manage`
请求体即 `config`。字段与校验：
| 字段 | 说明 |
|---|---|
| `enabled` | 依赖缓存开关（只作用于 `test`/`script` 阶段；`build` 阶段用镜像层缓存） |
| `language` | `go|node|npm|yarn|pnpm|maven|gradle|python|custom`，`paths` 为空时用默认路径：Go `/go/pkg/mod`+`/root/.cache/go-build`，npm `~/.npm`，Yarn `/usr/local/share/.cache/yarn`，pnpm `~/.local/share/pnpm/store`，Maven `~/.m2/repository`，Gradle `~/.gradle/caches`+`~/.gradle/wrapper`，Python `~/.cache/pip` |
| `paths` | ≤10 个；绝对路径、`~/`（按 `/root` 解析）或相对源码目录（如 `node_modules`）；禁止 `/`、`/etc`、`/usr`、`/root`、`/tmp`、`/workspace`、`/workspace/src`、`/proc`、`/sys`、`/dev`、`/kaniko`、`/root/.ssh` 等，禁止 `..` |
| `key` | 缓存键模板，默认 `{{branch}}`；变量 `{{branch}}`/`{{app}}`/`{{pipeline}}`/`{{hash:文件}}`（`checksum:` 同义，文件为相对源码目录的路径，取 sha256 前 16 位，文件不存在记为 `nofile`）；字面量只允许 `[A-Za-z0-9._-]`，≤200 字符 |
| `storage` | `pvc`（默认）或 `hostpath`（**仅沙箱**，需显式选择；目录 `/var/lib/matrix-cicd/cache/pipeline-<id>`，`DirectoryOrCreate`，多节点时缓存不共享，受 PodSecurity 限制） |
| `storage_class` / `size` / `access_mode` | 仅 PVC：存储类（空 = 集群默认）、容量（默认 5Gi，≤1Ti）、`ReadWriteOnce`（默认）/`ReadWriteMany` |
| `layer_cache` | Kaniko 镜像层缓存：`--cache=true --cache-repo=<layer_cache_repo 或 镜像仓库/cache>`，`layer_cache_ttl_hours>0` 时加 `--cache-ttl=<n>h`；阶段 `extra_args` 里已有 `--cache`/`--cache-repo` 时以阶段为准 |
| `layer_cache_repo` | `host[:port]/path`，不带 scheme 与 tag |

走 CICD 配置审批（环境 = 执行集群所属环境，类型 `cicd.config`，变更类型 `pipeline.cache`，乐观锁基于流水线 `updated_at`）；需审批时返回 202 + 审批单。成功返回同 GET。错误：400 校验（如 `cache path … cannot be used`、`unknown cache key variable …`、`cache size must be a quantity such as 5Gi`），409 `pipeline changed, reload and retry`。

#### POST /cicd/pipelines/:id/cache/clear — `cicd:manage`
```json
{"confirm":"<流水线名称>"}
```
- `confirm` 必须与流水线名称一致（400 `confirmation does not match the pipeline name`）；流水线有排队/运行中的运行时 409；没有缓存配置 409。
- 同样走 `cicd.config` 审批（变更类型 `pipeline.cache.clear`）。执行：PVC 方式删除 `cicd-cache-pipeline-<id>`（下次运行按当前配置重建，可借此修改容量/存储类）；hostPath 方式运行 Job `cicd-cache-clean-<id>` 清空目录。镜像层缓存在镜像仓库中，不清理。
- 成功：`{"cleared":5}`；审计记录 `cicd_pipeline` + `{"action":"clear_cache"}`。

#### 运行时行为
- 运行开始（有 test/script 阶段且开启依赖缓存）时确保 PVC 存在（不存在则按配置创建，标签 `matrix.cicd/pipeline=<id>`、`matrix.cicd/purpose=build-cache`）；创建失败时本次运行不用依赖缓存并在运行日志中说明，不让运行失败。
- test/script 阶段 Pod：git-clone 之后追加初始化容器 `cache-prepare`（`alpine/git`），挂载缓存卷到 `/xs-cache`，计算缓存键、判断各路径目录是否非空（命中/未命中）、统计占用，输出一行 `XS_CACHE result=hit|miss key=… usage_kb=…`（日志里改写为中文提示）。
  - 缓存键不含文件摘要：步骤容器把 `/xs-cache` 的子目录 `<键>/<路径标识>` 以 subPath 直接挂载到各缓存路径。
  - 缓存键含 `{{hash:…}}`：步骤容器挂载 `/xs-cache`，命令前插入符号链接（`ln -s /xs-cache/$KEY/<标识> <路径>`）；注意 `npm ci` 会删除 `node_modules`，相对路径 `node_modules` 不适合这种模式，Node 默认缓存 `~/.npm`。
  - 步骤容器注入 `CI_CACHE=true`。
- build 阶段：按配置追加 Kaniko 缓存参数；日志中 `Using caching version of cmd`/`No cached layer found for cmd` 计为层缓存命中/未命中（使用凭证、日志被隐藏时仍会计数）。
- 每个阶段结束后把观测写入 `cicd_runs.cache`，运行详情 `GET /cicd/runs/:id` 返回：
```json
"cache":{"deps":{"storage":"pvc","key":"main","result":"hit|miss|partial|unknown","usage_bytes":123731968,
                 "stages":[{"idx":0,"name":"test","key":"main","result":"hit"}]},
         "layer":{"repo":"10.42.0.1:5000/demo/hello-web/cache","hits":4,"misses":1,"observed":true}}
```
  未使用缓存的运行 `cache` 为 `null`。

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
- Gitee：校验 `X-Gitee-Token`，事件 `X-Gitee-Event: Push Hook`。两种模式：
  - 密码模式（无 `X-Gitee-Timestamp`）：`X-Gitee-Token` 与 `webhook_secret` 常量时间比较。
  - 签名模式（带 `X-Gitee-Timestamp`，毫秒）：`X-Gitee-Token = base64(HMAC-SHA256(webhook_secret, timestamp + "\n" + webhook_secret))`，可为 URL 编码；时间戳与服务器时钟相差超过 1 小时（过期或超前）即拒绝。带时间戳时不再接受明文密码。
- Gitea / Gogs：按 GitHub 兼容的 `X-Hub-Signature-256` 校验。
- 签名失败 401。成功：`{"runs":[89,90]}`。

#### 投递去重
- 签名校验通过后按提供方投递 id 去重：Gitee `X-Gitee-Delivery`、Gitea `X-Gitea-Delivery`（优先于其兼容 GitHub 头）、Gogs `X-Gogs-Delivery`、GitHub `X-GitHub-Delivery`、GitLab `Idempotency-Key`（GitLab 17.4+，重试不变）否则 `X-Gitlab-Event-UUID`。id 须为 1–128 个可见 ASCII 字符，否则视为无 id。
- 每次投递写入 `cicd_webhook_deliveries`（唯一键 `app_id, provider, delivery_id`）。同一 id 再次到达（重试/重新投递）时不触发运行，`duplicates+1`、记录 `last_duplicate_at`，直接返回 200：`{"duplicate":true,"delivery_id":"…","runs":[]}`。
- 例外：上一次处理结果为 `error`（且未启动任何运行）的投递会重新处理，`attempts+1`。没有 id 的投递不去重，每次都处理。
- 投递状态：`processing | triggered | ignored | error`；记录保留 7 天。

### GET /cicd/pipelines/:id/webhook-deliveries — `cicd:read`
分页（`page`、`page_size`）列出该流水线所属应用的投递记录（新→旧）：
```json
{"items":[{"id":7,"app_id":3,"provider":"gitee","delivery_id":"abc-123","event":"Push Hook","ref":"refs/heads/main",
  "commit":"3f2a9c1…","status":"triggered","detail":"","runs":[{"pipeline_id":5,"run_id":90}],"run_id":90,
  "duplicates":1,"attempts":1,"received_at":"…","last_duplicate_at":"…"}],
 "total":1,"page":1,"page_size":20,"retention_days":7,"on_push":true}
```
`run_id` 为本流水线由该投递启动的运行（无则 null）。

## 镜像扫描门禁（阶段 `type: "scan"`）
扫描前序构建阶段推送的镜像（阶段自身 `image` 优先，否则取最近的 build 阶段镜像或 `image_repo:image_tag`；镜像仓库凭证同理继承）。阶段字段 `scan`：

| 字段 | 说明 |
|---|---|
| `source` | `trivy`（默认，在执行集群以 Job 运行 `aquasec/trivy:0.57.1`）或 `harbor`（读取 Harbor 已有报告） |
| `block_severities` | 阻断级别，默认 `["CRITICAL","HIGH"]`；可选 CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN |
| `report_only` | 只记录，从不阻断 |
| `ignore_cves` | 不计入门禁的漏洞 id（≤500 条，不区分大小写） |
| `ignore_unfixed` | 无修复版本的漏洞不计入门禁 |
| `top_n` | 保存的漏洞条数，默认 50，最大 200 |
| `insecure` | HTTP / 自签名仓库 |
| `harbor_trigger` | Harbor 无报告时触发扫描并等待 |

- 门禁：漏洞按 id+包+版本去重后，属于阻断级别且未被忽略的计为 `blocking`；`blocking > 0`（即超过阈值 0）运行失败，消息如 `镜像扫描未通过：2 个阻断级别漏洞（CRITICAL 1，HIGH 1；…）`；`blocking = 0` 放行。
- 扫描器不可用时阶段失败（不放行）：trivy 无可用执行集群、harbor 无仓库凭证（保存流水线时 400 `harbor scan needs a registry credential`）、Harbor 无报告且未开 `harbor_trigger`、扫描超时（默认 15 分钟）。如需容忍扫描失败，应在阶段层面处理，`report_only` 只影响门禁判定，不屏蔽扫描器错误。
- 结果写入 `cicd_image_scans`（状态 `passed | blocked | error`），在 `GET /cicd/runs/:id` 的 `scans` 字段返回（含 `summary`、`policy`、`vulnerabilities`）。

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

- 迁移 0009 `delivery_safety`：`cicd_image_scans`、`cicd_webhook_deliveries`、`cicd_canaries`、`cicd_canary_events`；回滚脚本 `0009_delivery_safety.down.sql`（按逆序删除这四张表及索引，数据不可恢复；迁移器只执行 `.up.sql`，回滚需手动执行）。
- 迁移 0008 `build_cache`：`cicd_pipelines.cache JSONB`（缓存配置，NULL = 未配置）、`cicd_runs.cache JSONB`（本次运行的缓存使用与观测）。
- `cicd_credentials`（迁移 0007）：`name UNIQUE, type, username, secret_enc, host, description, created_by`；`cicd_applications.credential_id` 引用它（无级联）。


`cicd_targets`、`cicd_jenkins`、`cicd_applications`、`cicd_pipelines`、`cicd_runs`、`cicd_run_stages`、`cicd_run_logs`、`cicd_releases`。

## 未实现 / 限制

- 未实现 Tekton 执行器、阶段并行/矩阵、手动重试单个阶段；每个 K8s 阶段各自重新克隆代码（不共享工作区）。依赖缓存只覆盖 test/script 阶段，不在阶段间传递构建产物。
- 编译缓存未验证项（需在沙箱实测）：k3s `local-path`（WaitForFirstConsumer）下 PVC 首次绑定与 subPath 自动建目录；同一流水线并发运行时 RWO 卷跨节点调度会等待；Kaniko `--insecure` 是否同样作用于 HTTP 缓存仓库（沙箱 registry 为 http，必要时在阶段 `extra_args` 补 `--insecure-registry=10.42.0.1:5000`）；`du` 在超大缓存上可能超时导致占用为空；缓存键含文件摘要时各容器以 root 运行的假设（`~` 按 `/root` 解析）。
- 语言识别未验证项：需要执行集群能拉取 `alpine/git:2.45.2`；大仓库浅克隆可能超过 110 秒；内置 Dockerfile 模板（尤其 Maven 多模块、Gradle `-plain.jar` 过滤、Python 入口猜测、`gradle:8.10-jdk8` 等镜像标签）未逐一实测；`xs-live-ops.js` 里旧的流水线弹窗保存时会丢弃 `dockerfile_template` 等它不认识的阶段字段（缓存配置不受影响；新工作台 `xs-cicd-pipeline.js` 按序号保留原字段）。
- 进程重启时正在执行的运行直接标记失败，不做断点续跑（Job 仍会在集群中跑完并由 TTL 清理）。
- Kaniko 推送凭据需预先在 runner 命名空间创建 dockerconfigjson Secret，平台不托管镜像仓库凭据；SSH 方式克隆需自行在镜像中提供密钥（仅 https Token 由平台注入）。
- Helm 回滚通过重新 `upgrade` 旧镜像实现，未调用 `helm rollback`；Helm 部署的 `previous_image` 取自平台发布记录而非集群实际状态。
- 直接部署（非流水线）的过程日志只写服务端日志，接口通过发布记录的 `status/message` 反映结果。
- Webhook 只处理 push 事件；不支持 Tag / Merge Request 事件。
- 运行日志无自动清理策略。
- 本模块未在 `internal/modules/registry.go` 中注册（按规则由集成方添加 `cicd.New()`）。
