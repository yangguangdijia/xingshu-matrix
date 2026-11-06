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
- Webhook 只处理 push 事件；不支持 Tag / Merge Request 事件、Gitee/Gitea 签名。
- 运行日志无自动清理策略。
- 本模块未在 `internal/modules/registry.go` 中注册（按规则由集成方添加 `cicd.New()`）。
