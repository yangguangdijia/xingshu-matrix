# v1.63 镜像构建与集群部署记录（堡垒机访问工作台）

> 目标集群：`192.168.6.3`（k8s-master，另含 k8s-node1 `192.168.6.4`、k8s-node2 `192.168.6.5`）
> 应用版本：`v1.17`；镜像 tag：`docker.io/xingshu/matrix-server:v1.63`
> 代码基线：`f9cd41e`（`feat(bastion): 访问工作台与真实 SSH 会话，登录可访问性补齐`）
> 部署时间：2026-10-21 16:39–16:45 UTC（本地 00:39–00:45）
> 访问入口：`http://192.168.6.3:30080`（NodePort）、`https://xingshu-matrix.com`（Ingress + 自签证书）
> 接管方式：Argo CD Application `xingshu-matrix`（namespace `argocd`，source.path `deploy/k8s`，targetRevision `beta`，automated sync）

---

## 一、报错问题

### 问题 1：集群仍跑 v1.62，`/api/v1/version` 落后代码 25 小时

```
GET /api/v1/version
{"version":"v1.16","commit":"ac63d0f","build_time":"2026-10-20T15:12:15Z"}
```

而本轮代码是 `f9cd41e`（`2026-10-21T16:34:11Z`）。集群首页 HTML 也证明没有本轮内容：

| 标记 | 集群 HTML |
|---|---|
| `basProbeConnectors` | 0 |
| `basLiveSSH` | 0 |
| `basWorkbench` | 0 |
| `id="lu"` | 0 |
| `aria-invalid` | 0 |

### 问题 2：Argo CD 显示 `Synced`，但部署的仍是旧镜像

```
sync=Synced  rev=f9cd41e  health=Progressing
Deployment image = docker.io/xingshu/matrix-server:v1.62
```

`Synced` 只表示"集群资源与 Git 清单一致"，**不代表"清单里写的是最新镜像"**。清单里 `image: v1.62` 没跟着代码更新，同步得再勤也只是把旧 tag 再 apply 一遍。

### 问题 3：默认 kubeconfig 连不上集群

```
Unable to connect to the server: tls: failed to verify certificate:
x509: certificate signed by unknown authority (possibly because of
"crypto/rsa: verification error" while trying to verify candidate authority certificate "kubernetes")
```

`~/.kube/config` 里的 CA 与当前集群不匹配，但仓库内 `.build/admin.conf` 可用。

### 问题 4：容器里没有 `sh` 和 `curl`，无法 `exec` 进容器核对文件

```
OCI runtime exec failed: exec failed: unable to start container process:
exec: "sh": executable file not found in $PATH: unknown
exec: "curl": executable file not found in $PATH: unknown
```

镜像是 distroless/scratch 风格，只有静态二进制，没有 shell。

### 问题 5：Deployment 的 `last-applied-configuration` 与 spec 不一致

```
metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"] → image: v1.57
spec.template.spec.containers[0].image                          → image: v1.62
```

说明现网不是靠 `kubectl apply -f deploy/k8s/xingshu-matrix-dev.yaml` 维护的：可能是 Argo 接管前手动 `set image` 过，或清单更新后没有 apply。**直接 `kubectl apply` 会把镜像退回 v1.57。**

---

## 二、原因

1. **只提交代码，没有构建与分发**：上一轮 `f9cd41e` 的 8 个文件里没有 `deploy/`，也没有执行镜像构建脚本，集群自然停留在上一次构建的产物。
2. **GitOps 的"同步"被误读成"更新"**：Argo 的 `Synced` 是"集群 = Git 清单"，镜像 tag 写在清单里，清单不改就不会有变化。
3. **kubeconfig 与集群证书不匹配**：本机 `~/.kube/config` 是旧集群或旧 CA 留下的，仓库内的 `.build/admin.conf` 才是当前集群的 admin 凭据。
4. **镜像采用 scratch/静态二进制**：为减小体积与攻击面，镜像内不放 shell 与 curl，所以 `kubectl exec` 的传统排查手段不可用，必须改用 Service proxy 或 NodePort 访问。
5. **Deployment 曾被 `kubectl set image` 直接改过**：`last-applied-configuration` 停留在 v1.57，而 spec 是 v1.62，两个"真相源"分叉。

---

## 三、解决方案

### 3.1 构建 linux/amd64 二进制

```bash
cd /d/EvoX/xingshu-matrix/server
export GOCACHE=$(cygpath -w ../.evox-cache/go-build)
export GOTMPDIR=$(cygpath -w ../.evox-cache/go-tmp)

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Version=v1.17 \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Commit=f9cd41e \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.BuildTime=2026-10-21T16:39:05Z" \
  -o ../.build/matrix-server-v1.63 ./cmd/matrix-server
```

| 参数 | 值 | 说明 |
|---|---|---|
| `GOOS/GOARCH` | `linux/amd64` | 集群节点架构 |
| `CGO_ENABLED` | `0` | 静态链接，镜像内无 libc 也能跑 |
| `-trimpath` | — | 去掉本机路径，构建可复现 |
| `-ldflags -s -w` | — | 去符号表，二进制 63.9 MB |
| `Version` | `v1.17` | `GET /api/v1/version` 的 `version` |
| `Commit` | `f9cd41e` | 便于核对镜像与代码 |
| `BuildTime` | `2026-10-21T16:39:05Z` | 实际构建时刻 |

产物：`.build/matrix-server-v1.63`，63,877,304 字节，`ELF 64-bit LSB executable, x86-64, statically linked, stripped`。

### 3.2 组装 OCI 镜像（复用 v1.62 基础层）

用 `.build/mk_oci_v163.py`：

```bash
cd /d/EvoX/xingshu-matrix
python .build/mk_oci_v163.py
```

脚本行为：

| 步骤 | 参数 / 规则 |
|---|---|
| 基础层 | 直接复用 `.build/oci-v1.62` 的 layer blob：`sha256:f0c6df80…`，diff_id `sha256:dac03705…` |
| 应用层 | 只含 `app/bin/matrix-server`（0755）、`app/public/*`（0644）、`app`/`app/bin`/`app/public` 目录项 |
| 打包格式 | `tarfile.USTAR_FORMAT`（**不能用 PAX**，否则节点 `ctr` 报 `archive/tar: invalid tar header`） |
| 层压缩 | gzip level 6 |
| `rootfs.diff_ids` | `[基础层 diff_id, 新层未压缩 sha256]` |
| `config.Entrypoint` | `["/app/bin/matrix-server"]` |
| `config.Cmd` | `["serve","-config","/etc/matrix/config.yaml"]` |
| `config.User` | `10001:10001` |
| `config.Env` | `TZ=Asia/Shanghai`、`MATRIX_STATIC_DIR=/app/public`、`PATH=…` |
| `config.Labels` | `version=v1.17`、`revision=f9cd41e` |
| `index.json` 注解 | `org.opencontainers.image.ref.name=docker.io/xingshu/matrix-server:v1.63` |

实测输出：

```
base layer    sha256:f0c6df809d59a586867ea1b01a954186a4f1c8a13193462a67f446e1d0dd6915 19424236
app layer     sha256:32e8a72005a34edac226cb10aa4bf61918319f863c689f99b06e85889227e37a 19308169
app diff_id   sha256:c11aafc07de0f60188b4daa7117712d24ee0b788ec361b0b9726f0d358559f3b
config        sha256:f8f13e12e1f0c6cbef8c7be0f26ede480095d60f93710c501875f606534684d9
manifest      sha256:ec081a8d7b009353f14232129e2f19cce9344deee18044f759a0905f34e31422
oci tar       .build/oci-v1.63.tar 38748160 bytes
```

tar 结构核对（`tar -tf .build/oci-v1.63.tar`）：

```
oci-layout
index.json
blobs/
blobs/sha256/
blobs/sha256/32e8a720…   (应用层)
blobs/sha256/ec081a8d…   (manifest)
blobs/sha256/f0c6df80…   (基础层)
blobs/sha256/f8f13e12…   (config)
```

### 3.3 分发到三个节点并导入

```bash
scp -F ~/.ssh/config .build/oci-v1.63.tar k8s-master:/tmp/oci-v1.63.tar
ssh -F ~/.ssh/config k8s-master \
  'ctr -n k8s.io images import --all-platforms /tmp/oci-v1.63.tar'

ssh -F ~/.ssh/config k8s-master 'for h in 192.168.6.4 192.168.6.5; do
  scp -q -o StrictHostKeyChecking=no /tmp/oci-v1.63.tar root@$h:/tmp/oci-v1.63.tar
  ssh -o StrictHostKeyChecking=no root@$h \
    "ctr -n k8s.io images import --all-platforms /tmp/oci-v1.63.tar"
done'
```

要点：

| 项 | 值 | 说明 |
|---|---|---|
| `-n k8s.io` | 必须 | kubelet 只认这个命名空间，`default` 里导入等于没导 |
| `--all-platforms` | 建议 | 避免平台索引缺失 |
| 三节点 | master + node1 + node2 | `imagePullPolicy: IfNotPresent`，节点没有才会去拉；集群无外网，必须三节点都导入 |
| digest 一致性 | `sha256:ec081a8d…` | 三节点导入后 manifest digest 相同 |

### 3.4 更新清单（而不是 `kubectl set image`）

Argo CD 会持续把集群拉回 Git 里的状态。手动 `set image` 在开启 self-heal 时会被立刻改回去，即使没有 self-heal，下一次 sync 也会覆盖。所以改清单：

```yaml
# deploy/k8s/xingshu-matrix-dev.yaml
image: docker.io/xingshu/matrix-server:v1.63
```

同时更新上方注释（v1.63 / `f9cd41e` / `version=v1.17` 与构建脚本路径），避免下一个人再被"构建序号 ≠ 应用版本"绕进去。

### 3.5 触发 Argo 同步

```bash
git add deploy/k8s/xingshu-matrix-dev.yaml
git commit -m "chore(deploy): 镜像切换到 v1.63（f9cd41e / version=v1.17）"
git push origin HEAD:beta

# 自动同步有轮询间隔；需要立即生效时手动刷新一次
kubectl --kubeconfig .build/admin.conf -n argocd patch application xingshu-matrix \
  --type merge -p '{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"normal"}}}'
```

同步后观察：

```bash
kubectl --kubeconfig .build/admin.conf -n xingshu-system rollout status deploy/xingshu-matrix --timeout=180s
```

滚动策略是 `maxSurge: 1`、`maxUnavailable: 0`，两个副本逐个替换，**滚动期间至少一个 Pod 可用**。

### 3.6 验证（内容级，不只看"活着"）

| 检查 | 命令 | 实测结果 |
|---|---|---|
| 版本 | `curl http://192.168.6.3:30080/api/v1/version` | `{"version":"v1.17","commit":"f9cd41e","build_time":"2026-10-21T16:39:05Z"}` |
| 首页哈希 | `curl -s http://192.168.6.3:30080/ \| sha256sum` | `9aa0a2aa95549013ce85553171f7b870a8352257ebcb9c9717a5ab56d5a02af4`（与本地 `public/index.html` 完全一致） |
| 首页大小 | `wc -c` | 1,348,179 字节 |
| 本轮标记 | `grep -c` | `basProbeConnectors=4`、`basLiveSSH=2`、`basWorkbench=4`、`id="lu"=1`、`aria-invalid=4` |
| Pod | `kubectl get pods -l app=xingshu-matrix` | 2 个 `v1.63` 全部 `READY=true` |
| 健康检查 | `curl /healthz` | 200 |
| 登录页 | `curl /` | 200 |
| 数据库迁移 | `psql -c "\d bastion_assets"` | `options \| jsonb \| nullable=NO \| default='{}'::jsonb` |
| Deployment | `kubectl get deploy` | `replicas=2 ready=2 available=2 updated=2` |
| Argo | `kubectl get application` | `Synced`，rev `1f80ef1` |

Argo 的 `health=Progressing` 来自 `Ingress/xingshu-matrix`（nginx class，无 `status.loadBalancer.ingress`），
Deployment / Service / StatefulSet 全部 `Healthy`，与本次部署无关，是既有状态。

### 3.7 回滚

```bash
# 方式一：改回清单让 Argo 回滚（推荐，保持 GitOps 一致）
git revert <本次部署提交> && git push origin HEAD:beta

# 方式二：临时手动回退（下一次 sync 前有效）
kubectl --kubeconfig .build/admin.conf -n xingshu-system \
  set image deploy/xingshu-matrix web=docker.io/xingshu/matrix-server:v1.62
kubectl --kubeconfig .build/admin.conf -n xingshu-system rollout undo deploy/xingshu-matrix
```

回滚后复查 `/api/v1/version` 应为 `{"version":"v1.16","commit":"ac63d0f"}`。

---

## 四、如何避免

1. **"代码已推送"不等于"平台已更新"**。发布完成的标准是三条同时成立：
   `GET /api/v1/version` 的 commit 等于目标提交；首页 HTML sha256 等于仓库版本；Pod 镜像 tag 等于新 tag。
2. **GitOps 下永远改清单，不直接 `set image`**。`Synced` 只证明集群=清单；清单里的 tag 才是真正的版本开关。
   改完清单要提交推送，再确认 Argo 的 `status.sync.revision` 已经指向新 commit。
3. **`last-applied-configuration` 要定期核对**。它若与 `spec` 不一致，说明有人绕过清单改过现网，
   下次 `kubectl apply` 会静默回退。发现不一致时先用 Argo/清单收敛，再 apply。
4. **镜像导入必须三节点齐全且 digest 一致**。`imagePullPolicy: IfNotPresent` + 集群无外网，
   少导一个节点就会出现 `ErrImagePull` 或调度到旧版本。
5. **OCI 包必须 USTAR**。PAX 扩展头会让 containerd 报 `archive/tar: invalid tar header`；
   自检命令：`tar -tf oci-vX.tar` 出现 `././@PaxHeader` 就是错的。
6. **scratch 镜像的排查手段**：不要依赖 `kubectl exec sh/curl`，改用
   `kubectl get --raw "/api/v1/namespaces/<ns>/services/http:<svc>:80/proxy/<path>"` 或 NodePort 直连。
7. **每次发布把"镜像 tag ↔ commit ↔ 应用版本"三者写进同一张表**（本文件 §3.6），
   避免再出现 v1.34 / v1.57 / v1.61 / v1.62 四套 tag 并存时的误判。
8. **部署后再查一次迁移**。`MATRIX_DB_AUTO_MIGRATE=true` 只保证"启动时尝试迁移"，
   是否真的落地要查 `information_schema.columns`。

---

## 五、本次交付清单

| 项 | 值 |
|---|---|
| 代码提交 | `f9cd41e`（功能）、`1f80ef1`（清单） |
| 镜像 | `docker.io/xingshu/matrix-server:v1.63` |
| manifest digest | `sha256:ec081a8d7b009353f14232129e2f19cce9344deee18044f759a0905f34e31422` |
| 应用版本 | `v1.17` |
| 构建脚本 | `.build/mk_oci_v163.py`（不提交，`.gitignore` 排除 `.build/`） |
| 镜像包 | `.build/oci-v1.63.tar`，38,748,160 字节 |
| Pod | `xingshu-matrix-666bf495d7-26mgc`、`xingshu-matrix-666bf495d7-lv4jl` |
| 数据库迁移 | `0002_asset_options.up.sql` → `bastion_assets.options jsonb NOT NULL DEFAULT '{}'` |
| 未验证 | 真实 SSH 目标机、guacd RDP/VNC、Prometheus/Loki/ES/Jenkins/Kafka、真实 SSO |
