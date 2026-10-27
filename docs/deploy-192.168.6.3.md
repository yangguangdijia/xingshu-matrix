# 星枢 Matrix 在 192.168.6.3 集群的部署与验收记录

> 目标集群：`192.168.6.3`（k8s-master，同集群还有 k8s-node1 / k8s-node2）
> 部署时间：2026-10-19
> 镜像：`docker.io/xingshu/matrix-server:v8`（离线自建，见 §3.2）
> **最新镜像（v1.17 / 2026-10-21）**：`docker.io/xingshu/matrix-server:v1.63`（commit `f9cd41e`），
> 构建、三节点分发、滚动与回滚记录见 [deploy-v1.63-bastion-workbench.md](deploy-v1.63-bastion-workbench.md)。
> 本文其余章节保留 2026-10-19 首次部署的历史记录，其中的 v8 镜像与故障已不代表当前环境。
> 访问入口：**`https://xingshu-matrix.com`**（标准 443，Ingress + 自签证书；需在客户端 hosts 里加 `192.168.6.3 xingshu-matrix.com`）
> 备用入口：`http://192.168.6.3:30080`（NodePort，无域名/无 Ingress 时使用）
> 说明：**代码未提交 git**（按用户要求），改动都在工作区。

---

## 一、报错问题

部署过程中共有 5 个问题，按出现顺序记录。

### 问题 1：所有新 Pod 卡在 `ContainerCreating`，Calico CNI 报 `Unauthorized`

```
Failed to create pod sandbox: plugin type="calico" failed (add):
error getting ClusterInformation: connection is unauthorized: Unauthorized
```

影响面：整个集群所有新 Pod（包括 Rancher 自己的 `rancher` / `rancher-webhook`，它们已经卡了 5 天）。
`kubectl create ns` 也因此失败——Rancher 的命名空间校验 webhook 没有 endpoints。

### 问题 2：命名空间创建被 Rancher webhook 拦住

```
failed calling webhook "rancher.cattle.io.namespaces.create-non-kubesystem":
no endpoints available for service "rancher-webhook"
```

### 问题 3：集群没有外网，镜像拉不下来

```
failed to pull and unpack image "docker.m.daocloud.io/library/postgres:16-alpine": 502 Bad Gateway
failed to resolve reference "docker.io/xingshu/matrix-server:v1": ...
```

`registry-1.docker.io` / `ghcr.io` / `quay.io` / `mirror.gcr.io` 全部不通（`curl` 返回 000），只有
`docker.m.daocloud.io` 与 `registry.cn-hangzhou.aliyuncs.com` 可达（401 = 仓库在线）。

### 问题 4：升级镜像后页面仍跑旧前端

`xs-live-ops.js` 明明已经打进新镜像，浏览器里 `routes.audit.__xlopsPatched` 仍是旧值、新函数不存在。

根因在 `server/internal/web/spa.go`：静态资源被设成 `Cache-Control: public, max-age=3600`，
且镜像内文件 mtime 被固定成常量，`Last-Modified` 永远不变 —— 即使设了 `no-cache`，浏览器拿
`If-Modified-Since` 去问也永远得到 304，等于永久缓存旧文件。

### 问题 5：移动端（390px）KPI 卡片被压成 88px 一列

`grid-template-columns:repeat(4,minmax(0,1fr))` 写死 4 列，窄屏下每列 79.5px，
"最近一条 10-19 00:28" 竖排溢出、被裁切。

### 问题 6：升级后打开页面一直是"正在连接平台"，进不去

首屏渲染时认证还没恢复完，先绘制了 boot 遮罩；恢复成功后只渲染了应用，**没有把遮罩收掉**，
于是应用在下面、遮罩在上面，看起来像"卡住"。冷启动慢（Pod 正在滚动）时必现。

### 问题 7：master 与两台 worker 互相失联（节点 NotReady、Pod 被驱逐）

现象：`kubectl get nodes` 三个里两个 `NotReady`；master 上 `ping 192.168.6.4/5` 100% 丢包，
`ip neigh` 显示 `INCOMPLETE`；但**用户主机能 SSH 到两台节点**，节点上的 Pod 还在正常返回 200。
节点侧 `kubectl` 也连不上 master（6443 不通）。

抓包（node1 上 tcpdump）看到决定性证据：

```
ARP, Reply 192.168.6.3 is-at 00:0c:29:5c:0f:ae   ← node1 的 MAC 在应答 master 的 IP
ARP, Reply 192.168.6.3 is-at 00:0c:29:6d:0c:f6   ← master 自己
ARP, Reply 192.168.6.3 is-at 00:0c:29:88:36:73   ← node2 的 MAC 也在应答
```

即三台机器的 MAC 同时宣称持有 `192.168.6.3`。

---

## 二、原因

| # | 直接原因 | 更深一层 |
| --- | --- | --- |
| 1 | `/etc/cni/net.d/calico-kubeconfig` 里内嵌的 SA token **2026-09-22 就过期了**（当前 10-19，过期 27 天）。三个节点全部过期（node2 是 10-14 过期） | 该 token 由 `install-cni` 在 Pod 启动时现铸，默认带过期时间；集群里有一个 `calico-token-refresh.sh` 自愈脚本挂在 cron 上，但它**从 10-14 起就没再成功过** |
| 1b | 自愈脚本写 token 的函数用 `ssh 'bash -s' <<'EOS'`（heredoc 占用了 stdin），调用方又用 `printf token \| write_remote_token` 想从 stdin 传 token —— **两个写入源抢同一个 stdin**，`read -r NEWTOK` 读到的是脚本自己的下一行，`sed` 因此永远失败（日志里的 `FAIL sed 失败`） | 这不是 RBAC 问题，也不是证书过期；`getenforce` = Disabled、`sed` 手工执行正常，排除了 SELinux 与权限 |
| 2 | webhook 的 Pod 因为问题 1 一直起不来（`ContainerCreating` 5 天），Service 没有 endpoints，webhook 又配置成 `failurePolicy: Fail` | Rancher 的 webhook 拦截了 namespace 创建；先修 CNI 才能修 webhook |
| 3 | 集群没有外网出口；`mirror.gcr.io` 的 IP 被拒绝（connection refused）；daocloud 的 502 是镜像站瞬时错误 | 平台自身的镜像（Go 静态二进制）根本没在任何仓库里，只能自建并以 OCI tar 直接导入 containerd |
| 4 | `spa.go` 对所有静态文件统一 `max-age=3600`；镜像构建脚本把所有文件 mtime 固定为 `1700000000`（2023-11-14） | 前端没有构建步骤、文件名不带内容哈希，这两条一起等于"发版不生效" |
| 5 | KPI 卡片内联样式写死 4 列 | 没有走响应式网格 |
| 6 | `gate()` 里 `session && !session.must` 分支直接 `return false`，只在 `#/login`、`#/change-password` 时调用 `enterApp()`（它负责 `$("auth").hidden = true`） | 首屏那条 `showAuth("boot")` 路径没人收尾；恢复越快越不容易触发，所以长期潜伏 |
| 7 | `kube-ipvs0` 上挂着**别人的 IP** —— node1 有 `.3/.5`，node2 有 `.3/.4`。这些 /32 地址让每个节点都用自己的 MAC 应答 `192.168.6.3`，形成 ARP 冲突，双向流量随机打到错误主机 | 根因是我给 ingress Service 加的 `externalIPs: [192.168.6.3,.4,.5]`。**IPVS 模式下 kube-proxy 会把 externalIP 绑到节点的 `kube-ipvs0`**；我随后删掉 Service 上的 externalIPs，但节点上的 kube-proxy 因为"连不上 API Server"无法同步，残留地址把网络锁死（死锁：要清地址就得连 API，要连 API 就得先清地址） |

---

## 三、解决方案

### 3.1 修复 Calico CNI token（问题 1 / 2）

三步，全部在 `k8s-master` 上执行：

1. 铸新 token（有效期 720h，与既有脚本一致的时长）：

```bash
TOK=$(kubectl -n kube-system create token calico-cni-plugin --duration=720h)
```

2. 逐节点替换 kubeconfig 里的 token（**先备份**，用 `awk` + `cat >` 覆盖，不做 `sed -i`，
   避免改 inode / SELinux label；token 通过**环境变量**传给远端，不再走 stdin）：

```bash
for n in k8s-master k8s-node1 k8s-node2; do
  ssh root@$n "NEWTOK='$TOK' bash -s" <<'EOS'
set -e
f=/etc/cni/net.d/calico-kubeconfig
cp -a "$f" "$f.bak-$(date +%Y%m%d%H%M%S)"
awk -v t="$NEWTOK" '{ if ($1=="token:") print "    token: " t; else print }' "$f" > /tmp/ck.new
cat /tmp/ck.new > "$f"; rm -f /tmp/ck.new
grep -qF "$NEWTOK" "$f" && echo "$(hostname): OK"
EOS
done
```

3. 校验与善后：

```bash
cd /root/calico-upgrade && ./calico-token-refresh.sh --status   # 应显示"全部节点健康"
kubectl create ns xingshu-system                                # 能建 = CNI + webhook 都通了
```

> 自愈脚本的 `write_remote_token` 仍是坏的（heredoc 抢 stdin）。本次是手工修复；
> 要彻底自愈，得把 token 改成 `ssh "NEWTOK='...' bash -s"` 传参（见 §7.4）。

另外：`rancher-webhook` 的 Pod 因为 CNI 修复后仍带着 5 天前的失败退避，删掉让它重建：

```bash
kubectl -n cattle-system delete pod -l app=rancher-webhook --wait=false
# 之后 1/1 Running，namespace 创建恢复
```

> 用户要求"Rancher 的问题先不解决"，所以 `rancher` 主程序（缺镜像）保持原状，只修了挡住部署的 webhook。

### 3.2 离线镜像：自建 OCI tar 直接导入 containerd（问题 3）

集群没有 registry 可写、也没有 docker/buildah/nerdctl，所以走"本地交叉编译 + 手搓 OCI 镜像 + `ctr import`"：

1. 交叉编译（Windows 本机，Go 1.25，静态链接）：

```bash
cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X .../internal/version.Version=v1.0-preview" \
  -o matrix-server ./cmd/matrix-server        # 61 MB，ELF 64-bit statically linked
```

2. 组装 rootfs：`/app/bin/matrix-server`、`/app/public/**`、`/etc/matrix/config.yaml`、
   `/etc/ssl/certs/ca-certificates.crt`（从节点拷贝）、`/usr/share/zoneinfo/Asia/Shanghai`、
   最小 `/etc/passwd`（uid 10001 `matrix`）、`/tmp` 1777。

3. 用 Python 写 OCI layout（`oci-layout` + `index.json` + `blobs/sha256/*`），单层 tar.gz，
   `diff_ids` / `manifest` / `config` 的 digest 全部按内容计算。关键参数：

   | 项 | 值 |
   | --- | --- |
   | `User` | `10001:10001` |
   | `WorkingDir` | `/app` |
   | `Env` | `PATH` / `TZ=Asia/Shanghai` / `MATRIX_STATIC_DIR=/app/public` / `MATRIX_CONFIG=/etc/matrix/config.yaml` |
   | `Entrypoint` | `/app/bin/matrix-server` |
   | `Cmd` | `serve -config /etc/matrix/config.yaml` |
   | `ExposedPorts` | `8080/tcp` |
   | 层大小 | raw 66 MB → gz 19.4 MB |

4. 分发与导入（三节点都要有，否则调度到没镜像的节点会 `ErrImagePull`）：

```bash
ctr -n k8s.io images import /tmp/matrix-server-image.tar
ctr -n k8s.io images tag xingshu/matrix-server:v8 docker.io/xingshu/matrix-server:v8
ctr -n k8s.io images export /tmp/app-v8.tar docker.io/xingshu/matrix-server:v8
scp /tmp/app-v8.tar k8s-node1:/tmp/ && ssh k8s-node1 'ctr -n k8s.io images import /tmp/app-v8.tar'
```

> 坑：`ctr images import` 会把 ref 归一化成 `xingshu/matrix-server:v8`（不带 `docker.io/`），
> 而 Pod spec 写的是 `docker.io/xingshu/matrix-server:v8`，两者对不上就 `ErrImagePull`。
> **必须显式 `ctr images tag` 成 Pod 里用的全名。**

依赖镜像（PG/Redis）从可达的镜像站直接拉：

```bash
ctr -n k8s.io images pull --snapshotter overlayfs docker.m.daocloud.io/library/postgres:16-alpine
ctr -n k8s.io images pull --snapshotter overlayfs docker.m.daocloud.io/library/redis:7-alpine
```

> daocloud 偶发 502，重试即可（本次 postgres 第一次 502，第二次 20s 成功）。

### 3.3 部署清单（`deploy/k8s/xingshu-matrix-dev.yaml`，未提交）

| 对象 | 关键参数 |
| --- | --- |
| Namespace | `xingshu-system` |
| Secret `matrix-secrets` | `POSTGRES_PASSWORD` / `MATRIX_JWT_SECRET`（40 字符随机）/ `MATRIX_ENCRYPTION_KEY`（32 字节 base64）/ `MATRIX_ADMIN_INITIAL_PASSWORD=Xingshu@2026` |
| StatefulSet `postgres` | `postgres:16-alpine`，`PGDATA=/var/lib/postgresql/data/pgdata`，`local-path` 5Gi PVC，探针 `pg_isready -U matrix -d matrix`，requests 100m/256Mi |
| Deployment `redis` | `redis:7-alpine`，`--appendonly yes`，emptyDir，requests 20m/64Mi |
| Deployment `xingshu-matrix` | `replicas: 2`，`RollingUpdate maxSurge=1 maxUnavailable=0`，`runAsUser/Group 10001`，探针 `/healthz`，requests 100m/128Mi、limits 500m/384Mi |
| 环境变量 | `MATRIX_MODE=prod`、`MATRIX_LISTEN=:8080`、`MATRIX_DB_DSN=postgres://matrix:$(POSTGRES_PASSWORD)@postgres:5432/matrix?sslmode=disable`、`MATRIX_REDIS_ADDR=redis:6379`、`MATRIX_RECORD_DIR=/data/recordings`（挂 emptyDir，否则以 10001 身份 mkdir `/app/data` 会 permission denied） |
| Service | ClusterIP `xingshu-matrix:80 → 8080`；NodePort `xingshu-matrix-nodeport:30080` |
| Ingress | `matrix.192.168.6.3.nip.io`，`nginx.ingress.kubernetes.io/proxy-read-timeout: 3600`（SSE/WebSocket） |

> `$(POSTGRES_PASSWORD)` 的环境变量展开**要求被引用变量在本列表中排在前**，否则会原样输出字面量。

### 3.4 修静态资源缓存（问题 4）

```go
// server/internal/web/spa.go
if fi, err := os.Stat(full); err == nil && !fi.IsDir() && clean != "/index.html" {
    c.Header("Cache-Control", "no-cache")   // 原来是 public, max-age=3600
    c.File(full)
    return
}
```

镜像构建侧同时把 mtime 改为**构建时间**（原来固定 `1700000000`），这样 `Last-Modified` 每次发版都变，
`no-cache` 的协商才能拿到新内容：

```python
BUILD_TS = int(os.environ.get("BUILD_TS") or time.time())
ti.mtime = BUILD_TS
```

验证：

```bash
curl -sI http://192.168.6.3:30080/xs-live-ops.js | grep -i "cache-control\|last-modified"
# Cache-Control: no-cache
# Last-Modified: Sun, 18 Oct 2026 16:29:27 GMT   ← 随构建变化
```

### 3.5 首屏 boot 遮罩（问题 6）

```js
if (session && !session.must) {
  if (location.hash !== "#/login" && location.hash !== "#/change-password") {
    // 首屏渲染时认证尚未恢复完会先 showAuth("boot")；恢复成功后这里必须把遮罩收掉
    if (!$("auth").hidden) { $("auth").hidden = true; $("top").inert = false; authView = ""; }
    return false;
  }
  nextHash = ""; enterApp(); return true;
}
```

### 3.6 解除 ARP 死锁（问题 7）

在**两台节点**上手工摘掉不属于本机的 /32（不要删自己那条）：

```bash
# node1 (.4)
ip addr del 192.168.6.3/32 dev kube-ipvs0
ip addr del 192.168.6.5/32 dev kube-ipvs0
# node2 (.5)
ip addr del 192.168.6.3/32 dev kube-ipvs0
ip addr del 192.168.6.4/32 dev kube-ipvs0
```

执行后立刻 `ping` 恢复 0% 丢包，kubelet 重新上报，三节点回到 `Ready`，Pod 自动重建。

**教训**：这个集群用的是 IPVS 代理模式，**不要用 `externalIPs` 做入口**；要么 hostNetwork（见 §8.2），
要么 MetalLB。另外以后任何"删配置"的动作，都要确认下发它的组件（kube-proxy）真的能同步。

### 3.7 移动端 KPI 卡片（问题 5）

`repeat(4,minmax(0,1fr))` → `repeat(auto-fit,minmax(150px,1fr))`，1280 下仍是 4 列，390 下自动 2×2。

另外团队协作表格加 `min-width:760px;white-space:nowrap`，窄屏改为横向滚动，不再把"李明"竖排。

---

## 四、如何避免

1. **"Unauthorized"的 CNI 错误先查 token 过期，不要怀疑 RBAC/证书**。判定方法：解出 kubeconfig 里
   JWT 的 `exp` 与 `sub`（本次 `system:serviceaccount:kube-system:calico-cni-plugin`，过期 27 天）。
   一行 Python 就能验，比翻 ACL 快得多。
2. **自动化脚本不要既用 heredoc 传脚本、又用管道传数据**。`ssh 'bash -s' <<'EOS'` 已经占用了 stdin，
   任何 `read` 都会读到脚本自己；要用参数或环境变量传数据。写完必须跑一次真实验证
   （本次脚本"看起来在跑"，日志里每小时一条 `FAIL sed 失败`，五天没人发现）。
3. **自愈脚本要留可观测性**：日志文件 mtime 停更就是最好的告警信号；建议把"上次成功时间"写进文件，
   并给 cron 加 `MAILTO` 或落到 `journalctl`，而不是静默重试。
4. **改 kubeconfig 这类敏感文件**：先 `cp -a` 备份、用 `awk`/`cat >` 原地覆写（保 inode 与 label），
   改完 `grep -qF` 校验，最后跑只读巡检确认。
5. **无外网集群的发布流程要固化**：构建 → 打 OCI tar → 全节点 `ctr import` → **用 Pod spec 里那个全名
   `ctr tag`** → `kubectl apply`。少一步就会 `ErrImagePull`，且报错只说"pull failed"，不告诉你名字不一致。
6. **前端有缓存就必须有版本机制**：要么文件名带内容哈希，要么 `no-cache` + 每次构建刷新 mtime。
   只改 `Cache-Control` 而 mtime 固定，等于永久缓存——本次就是被这个组合坑了。
7. **验证要换一个全新 origin**。浏览器对同一 origin 的旧响应可能不复用协商结果，测新版本时用
   `192.168.6.4:30080` 这类等价入口，能立刻分辨"服务端没更新"还是"浏览器缓存"。
8. **移动端必须真机宽度过一遍**：写死的列数（`repeat(4,…)`）在 390px 下必然溢出；
   表格在窄屏优先 `min-width + overflow-x`，不要让它压缩换行。

---

## 五、验收结果

### 5.1 集群与工作负载

| 检查项 | 结果 |
| --- | --- |
| 节点 | master / node1 / node2 全部 `Ready`（node1 在部署中途短暂 `NotReady`，自行恢复） |
| Pod | `postgres-0 1/1 Running`、`redis 1/1 Running`、`xingshu-matrix 2/2 Running`（分布 node1/node2） |
| 镜像 | 三节点均持有 `docker.io/xingshu/matrix-server:v8`、`postgres:16-alpine`、`redis:7-alpine` |
| 存储 | `data-postgres-0` PVC 由 `local-path` 供给，绑定 node2 |
| 健康检查 | `GET /healthz` → 200；`GET /api/v1/health` → `{"database":"ok","redis":"ok","status":"ok"}` |

### 5.2 后端接口链路（真实 HTTP，非 mock）

| 步骤 | 结果 |
| --- | --- |
| `POST /auth/login`（admin / 初始密码） | 200，`must_change_password=true` |
| 改密闸门 `GET /clusters` | 403 / `40301`（符合设计） |
| `PUT /auth/password` | 200 |
| 重新登录 | 200，`must_change_password=false` |
| `/auth/me`、`/users`、`/roles`、`/settings/public`、`/clusters`、`/hosts`、`/approvals` | 全部 200 |
| `/audit-logs?page=1&page_size=5` | 200，有真实记录（登录、改密） |

> 管理员密码保持默认 **`Xingshu@2026`**（来自 Secret `matrix-secrets` 的 `MATRIX_ADMIN_INITIAL_PASSWORD`）。
> 冒烟测试时曾临时改成 `Matrix@2026ok` 验证改密链路，**已还原**，并清掉了 admin 的
> `must_change_password` 与失败锁定，可直接用默认密码登录。
>
> 如需重置任意账号密码（示例，bcrypt cost=12）：
> ```bash
> HASH=$(go run ./tools/bcrypt -pw 'Xingshu@2026')   # 或任意 bcrypt 工具
> kubectl -n xingshu-system exec -i postgres-0 -- psql -U matrix -d matrix <<SQL
> update users set password_hash='${HASH}', must_change_password=false,
>   failed_attempts=0, locked_until=null, password_changed_at=now() where username='admin';
> SQL
> ```
> 注意：SQL 里的 `$2a$12$...` 含 `$`，**不要**写在双引号 shell 里直接拼接，否则会被变量展开截断（本次踩过）。

### 5.3 前端（浏览器实测）

| 页面 | 视口 | 结果 |
| --- | --- | --- |
| 登录 | 390×844 / 1280×720 / 1366×768 | 布局正常，登录成功进入工作台 |
| 工作台 | 1280×720 | 正常，"实时数据"徽标 |
| 运维审计（本次新增 live 页） | 1366×768 | 真实数据 6 条，筛选/分页/详情弹窗/导出可用 |
| 运维审计 | 390×844 | KPI 2×2、表格横向滚动、筛选纵向堆叠 |
| 团队协作（本次新增 demo 页） | 1366×768 / 390×844 | 10 成员、4 团队、知识库 42 篇、周报；详情面板可用；窄屏表格横向滚动 |

控制台：全程无 error。

---

## 六、本次代码改动（未提交）

| 文件 | 改动 |
| --- | --- |
| `public/index.html` | 新增 `renderTeam`（团队协作页，原为"页面建设中"占位）：KPI 4 卡、成员表（搜索/部门筛选/邀请/移除/详情面板）、团队分组、知识库、周报；路由 `team` 由占位改为 `render: renderTeam` |
| `public/xs-live-ops.js` | 新增 `renderAuditLive`（运维审计实时页，数据源 `GET /audit-logs`）：资源/操作人/结果筛选、KPI、分页、详情弹窗、CSV 导出；注册到 `ROUTE_KEYS` + `RENDERERS` |
| `server/internal/web/spa.go` | 静态资源缓存 `max-age=3600` → `no-cache`（发版不生效的根因） |
| `deploy/k8s/xingshu-matrix-dev.yaml` | 本集群完整部署清单：PG/Redis/平台/Service/NodePort/**HTTPS Ingress**/**RBAC + ServiceAccount** |
| `deploy/scripts/gen-tls-cert.sh` | 新增：自签证书生成（CN=xingshu matrix、10 年、SAN）并写入 TLS Secret |
| `server/internal/platform/domain.go` | 新增：`GET/PUT /platform/domain`，保存域名时自动创建/更新 Ingress；无 Ingress 时返回 skipped |
| `server/internal/server/server.go` | 挂载 `platform.Register(authed, c.Settings)` |
| `public/xs-live-ops.js` | 设置页新增「访问域名」卡片（输入框 + 保存/清除 + 提示 + Ingress 状态） |
| `public/index.html` | 修首屏 boot 遮罩不消失的问题；删除登录页底部两行提示（「首次部署默认账号 admin…」「已连接 matrix-server…/演示模式」） |

## 七、已知限制 / 未完成

1. ~~Ingress 不可用~~：`ingress-nginx-controller` 原本 `ImagePullBackOff`，在修好 CNI 后自行恢复为
   `1/1 Running`；现在 HTTPS 入口可用（`:30855`/`:32533` 两个 NodePort 由控制器监听）。
2. **Rancher 主程序仍在重启循环**（缺镜像 `rancher/rancher:v2.14.2`），按用户要求未处理；
   webhook 已恢复，不影响平台。
3. **`calico-token-refresh.sh` 的自愈逻辑仍是坏的**，本次为手工修复。彻底方案：
   `write_remote_token` 改为 `ssh "NEWTOK='$newtok' bash -s"`（token 走环境变量），并给脚本加
   "上次成功时间"落盘。token 当前有效期到 2026-11-17。
4. **单副本 PostgreSQL + `local-path` PVC**：Pod 重建后仍绑定 node2，跨节点不可迁移（本地卷语义）。
5. **应用详情抽屉仍有 6 个面板是占位**（服务列表 / 部署编排 / 监控告警 / 应用成员 / 操作记录 / 流程中心），
   本轮的 t8 未完成。
6. **`node1` 中途失联过约 6 分钟**（kubelet 停止上报、SSH 无响应），随后自行恢复；原因未复现，
   建议后续查 `dmesg`/`kubelet` 日志。

---

## 八、HTTPS、自签证书与平台域名

### 8.1 证书

| 项 | 值 |
| --- | --- |
| 主题（Subject） | `CN=xingshu matrix`，`O=Xingshu Matrix`，`OU=Platform` |
| 有效期 | 3650 天（2026-10-18 → 2036-10-15） |
| 密钥 | RSA 4096 / SHA-256 |
| SAN | `xingshu-matrix.com`、`*.xingshu-matrix.com`、`localhost`、`192.168.6.3`、`127.0.0.1` |
| K8s Secret | `xingshu-system/xingshu-matrix-tls`（type `kubernetes.io/tls`） |

生成脚本：`deploy/scripts/gen-tls-cert.sh`（兼容 CentOS 7 自带的 OpenSSL 1.0.2，用配置文件声明 SAN，
不用 `-addext`）。参数全部可覆盖：

```bash
DOMAIN=xingshu-matrix.com CN="xingshu matrix" DAYS=3650 \
EXTRA_IPS=192.168.6.3,192.168.6.4,192.168.6.5,127.0.0.1 \
NAMESPACE=xingshu-system SECRET=xingshu-matrix-tls \
bash deploy/scripts/gen-tls-cert.sh
```

脚本会：生成 `./certs/tls.key`（600）、`./certs/tls.crt` → 打印 subject/dates/SAN → 用
`kubectl create secret tls --dry-run=client -o yaml | kubectl apply -f -` 写入/轮换 Secret。

续期：证书到期前重跑一次脚本即可（Ingress 无需改动，Secret 同名覆盖）。

### 8.2 Ingress

```yaml
spec:
  ingressClassName: nginx
  tls:
    - hosts: ["xingshu-matrix.com"]
      secretName: xingshu-matrix-tls
  rules:
    - host: xingshu-matrix.com
      http:
        paths:
          - { path: /, pathType: Prefix, backend: { service: { name: xingshu-matrix, port: { number: 80 } } } }
```

注解：`ssl-redirect=true`、`force-ssl-redirect=true`（HTTP 308 跳 HTTPS）、
`proxy-body-size=50m`、`proxy-read-timeout=3600`（SSE / WebSocket 长连接）。

**让 `https://xingshu-matrix.com` 走标准 443（不带端口）**：Ingress 控制器必须占用宿主机的 80/443。
本集群没有 MetalLB（Service 的 EXTERNAL-IP 一直 `<pending>`），且 kube-proxy 是 IPVS 模式
（`externalIPs` 会导致 ARP 冲突，见问题 7），因此采用 hostNetwork 并固定到 master：

```bash
# 1) 先把控制器镜像放到 master（该节点原先没有，会 ImagePullBackOff）
ssh k8s-node2 "ctr -n k8s.io images export /tmp/ing.tar registry.k8s.io/ingress-nginx/controller:v1.15.1"
scp k8s-node2:/tmp/ing.tar /tmp/ && ctr -n k8s.io images import /tmp/ing.tar

# 2) 让控制器绑宿主端口并固定到 master（master 有 control-plane 污点，需要 toleration）
kubectl -n ingress-nginx patch deploy ingress-nginx-controller --type strategic -p '{"spec":{"template":{"spec":{
  "hostNetwork":true,"dnsPolicy":"ClusterFirstWithHostNet",
  "nodeSelector":{"kubernetes.io/hostname":"k8s-master"},
  "tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}],
  "containers":[{"name":"controller","ports":[{"name":"http","containerPort":80,"hostPort":80},{"name":"https","containerPort":443,"hostPort":443}]}]
}}}}'
```

代价：控制器只跑在 master 单节点上（无 HA），NodePort `30855/32533` 作为备用入口仍然保留。

验证：

```bash
curl -sk --resolve xingshu-matrix.com:443:192.168.6.3 https://xingshu-matrix.com/api/v1/health
curl -s -o /dev/null -w "%{http_code} -> %{redirect_url}\n" --resolve xingshu-matrix.com:80:192.168.6.3 http://xingshu-matrix.com/
# 200 / 308 -> https://xingshu-matrix.com/
```

客户端访问方式（任选其一）：

1. 本机 hosts：`192.168.6.3 xingshu-matrix.com` → `https://xingshu-matrix.com:30855`
2. 内网 DNS 把 `xingshu-matrix.com` 解析到 `192.168.6.3`（推荐）
3. 直接用 IP：`https://192.168.6.3:30855`（证书 SAN 含该 IP，浏览器仍会提示自签不受信）

> 自签证书需要在客户端信任（把 `tls.crt` 导入系统/浏览器信任库），否则会有安全警告——这是自签的固有行为。

### 8.3 平台域名：设置页配置 + 自动维护 Ingress

**位置**：运维中心 › 设置 › 系统设置 › 第一块「平台入口」。

页面提示（写死在 UI 里，避免误操作）：

- 域名需要集群内存在 Ingress 控制器才能生效；
- 保存时平台会自动创建/更新 Ingress（host=该域名，TLS 复用 `xingshu-matrix-tls`）；
- 集群没有 Ingress 时保存依旧成功，但域名不生效，请用 `http(s)://节点IP:30080`；
- 清空并保存会删除平台自己的 Ingress，回退到 NodePort。

**接口**：

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/v1/platform/domain` | `setting:read` | 返回 `{domain, ingress_available, ingress_class, namespace, tls_secret, status, message, nodeport}` |
| PUT | `/api/v1/platform/domain` | `setting:manage` | body `{"domain":"xingshu-matrix.com"}`；保存 `platform.domain` 并收敛 Ingress |

PUT 的行为矩阵：

| 输入 | 结果 |
| --- | --- |
| 合法域名 + 有 IngressClass | `status=created/updated`，Ingress 立即生效 |
| 合法域名 + 无 IngressClass / 不在集群内 | `status=skipped`，设置仍保存，`message` 说明原因 |
| 空字符串 | 删除平台 Ingress，`status=removed`，回退 NodePort |
| 非法域名 | 400 `40000`「域名格式不合法」 |
| IP 地址 | 400 `40000`「请填写域名而不是 IP」 |

实现：`server/internal/platform/domain.go`（in-cluster config + client-go，6 秒超时探测）。
鉴权靠 Job 里的 RBAC：

```yaml
Role xingshu-matrix-platform       # networking.k8s.io/ingresses: get/list/watch/create/update/patch/delete
ClusterRole xingshu-matrix-ingressclass-reader   # networking.k8s.io/ingressclasses: get/list（只读）
```

Deployment 使用独立 ServiceAccount `xingshu-matrix`，并注入
`POD_NAMESPACE`（fieldRef）、`MATRIX_NODEPORT=30080`、`MATRIX_TLS_SECRET=xingshu-matrix-tls`。

### 8.4 本地 hosts（客户端）

```
192.168.6.3  xingshu-matrix.com
```

Windows：`C:\Windows\System32\drivers\etc\hosts`；Linux/macOS：`/etc/hosts`。

### 8.5 验证记录

| 检查 | 结果 |
| --- | --- |
| `curl -k https://xingshu-matrix.com:30855/api/v1/health` | `{"database":"ok","redis":"ok","status":"ok"}` |
| `openssl s_client -connect 192.168.6.3:30855 -servername xingshu-matrix.com` | subject `CN=xingshu matrix`，notAfter 2036-10-15 |
| HTTP `:32533` 带 Host 头 | `308 → https://xingshu-matrix.com` |
| 设置页保存同名域名 | toast「已更新 Ingress 域名」，Ingress 保持 |
| 设置页「清除域名」 | Ingress 被删除，`domain=""`，提示回退 NodePort |
| 通过 API 重新设置 | `status=created`，Ingress 重建，HTTPS 恢复 |
| 非法域名 / IP | 400 + 对应中文提示 |
