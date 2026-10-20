# 修复记录：集群镜像里混入了未提交代码（v1.61 → v1.62）

> 修复日期：2026-10-20
> 涉及对象：`192.168.6.3` 集群 `xingshu-system/xingshu-matrix` Deployment
> 修复前后：`docker.io/xingshu/matrix-server:v1.61` → `v1.62`
> 关联文档：[docs/deploy-192.168.6.3.md](deploy-192.168.6.3.md)、[docs/OUTSTANDING.md](OUTSTANDING.md)

---

## 一、报错问题

### 1.1 现象

用户反馈："怎么还是十点左右改的那一版啊"。第一次核对时，我根据 `/api/v1/version` 的返回判断集群已是当前代码：

```json
{"version":"v1.61","commit":"1c2f62b","build_time":"2026-10-20T14:05:00Z","go_version":"go1.25.7","platform":"linux/amd64"}
```

`1c2f62b` 是当时的 `beta` HEAD，看起来吻合。**但这个判断是错的**——镜像里实际包含的是工作区里一批从未提交、且已被用户判定为"改坏了"的代码（oncall 模块、SSO、automation runner 等）。

### 1.2 决定性证据：路由存在性探测

`version` 接口证明不了代码内容，**路由表可以**。用一个明确不存在的路径做对照：

```bash
B=http://192.168.6.3:30080/api/v1
curl -sk -o /dev/null -w "%{http_code}\n" $B/__nope          # 404 → 对照，说明不存在的路由确实 404
curl -sk -o /dev/null -w "%{http_code}\n" $B/oncall/overview # 401 → 路由存在（需认证）
curl -sk -o /dev/null -w "%{http_code}\n" $B/oncall/schedules # 401
curl -sk -o /dev/null -w "%{http_code}\n" $B/oncall/slos      # 401
curl -sk -o /dev/null -w "%{http_code}\n" $B/auth/sso/config  # 200
```

返回 `401`/`200` 而不是 `404`，说明 **oncall 与 SSO 的路由确实注册进了那个二进制**。

而 `beta` 分支 `1c2f62b` 里根本没有 `server/internal/modules/oncall` 与 `server/internal/auth/sso`——它们只存在于工作区未提交的文件中。

### 1.3 相关事实

| 项 | 值 |
| --- | --- |
| 坏镜像 tag | `docker.io/xingshu/matrix-server:v1.61` |
| 镜像 digest | `sha256:210bdd27ab0ad40989a5a22dda3986868b80a457ed0f25bfa01c659d790a1c8f` |
| Pod 创建时间 | `2026-10-20T14:01:34Z`（本地 22:01） |
| 未提交代码写入时间 | 本地 21:00–22:05（与镜像构建时间重叠） |
| 影响 | 集群暴露了本应丢弃的 oncall/SSO 接口；后续若有人在这些表/路由上做接入，会与仓库代码分叉 |

---

## 二、原因

1. **构建时没有从干净提交点构建**。镜像是在工作区里直接 `go build` 的，那批未提交文件（oncall、SSO、runner）被一起编译进了二进制。
2. **`version` 接口的 `commit` 不可信**。`Commit` 来自 `-ldflags -X ...version.Commit=1c2f62b`，是**构建脚本传进去的字符串**，与工作区实际内容无关。构建者传了 `1c2f62b`，即使代码已经漂移，接口也会显示 `1c2f62b`。
3. **没有对"镜像内容"做可验证断言**。之前的验收只看 `/healthz` 和页面 200，没有断言"某个已删除的路由必须 404"，因此脏代码可以顺利通过验收。
4. **构建链路不在 CI 里**。仓库的 `.github/workflows/docker.yml` 只推 `:latest`，集群实际镜像一直是人工离线构建，缺少强制 `git status --porcelain` 为空的前置检查。
5. **镜像 tag 语义混乱**（v1.34 / v1.57 / v1.61 三套并存），加剧了"哪一版是什么"的误判。

---

## 三、解决方案

### 3.1 用干净代码重建镜像

环境限制：本机无 Docker / Podman，WSL 内也无 docker，节点无 Go 与 Docker。因此采用**交叉编译 + 手工组装 OCI 镜像 + `ctr import`**。

```bash
# 1) 从干净提交点编译（Windows 交叉编译到 linux/amd64）
cd /d/EvoX/xingshu-matrix
export GOCACHE=/d/EvoX/xingshu-matrix/.cache/go-build
export GOTMPDIR=/d/EvoX/xingshu-matrix/.cache/tmp
cd server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Version=v1.16 \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.Commit=ac63d0f \
    -X github.com/yangguangdijia/xingshu-matrix/server/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ../.build/matrix-server ./cmd/matrix-server
# 产物：61 MB，ELF 64-bit LSB executable, x86-64, statically linked, stripped
```

### 3.2 组装 rootfs（镜像内只需要运行时必需文件）

| 路径 | 来源 | 说明 |
| --- | --- | --- |
| `/app/bin/matrix-server` | 上一步编译产物 | 静态二进制，uid/gid `10001` |
| `/app/public/*` | `public/`（12 个文件） | 前端 SPA，`MATRIX_STATIC_DIR=/app/public` |
| `/etc/matrix/config.yaml` | 手写最小配置 | `serve -config` 指定的文件**必须存在**，否则 `config.Load` 直接报错退出 |
| `/etc/ssl/certs/ca-certificates.crt` | 节点 `/etc/pki/tls/certs/ca-bundle.crt`（base64 传输） | 调用外部 HTTPS（AI 模型 API）需要 |
| `/usr/share/zoneinfo/Asia/Shanghai` | 节点同名文件（base64 传输，md5 校验一致） | `TZ=Asia/Shanghai` 需要 |
| `/data/recordings` | 空目录 | Deployment 挂 `emptyDir` 到 `/data` |

> 最小 `config.yaml` 内容：
> ```yaml
> server:
>   listen: ":8080"
>   mode: prod
>   static_dir: /app/public
> ```
> 其余配置全部走 Deployment 的 `MATRIX_*` 环境变量覆盖。

### 3.3 手工构造 OCI 镜像（关键：必须用 USTAR，不能用 PAX）

第一次尝试用 `docker save` 风格（`manifest.json` + `layer0/layer.tar`）并 gzip 外层，`ctr` 报：

```
ctr: archive/tar: invalid tar header
```

原因：Python `tarfile` 默认 `PAX_FORMAT`，会写入 `././@PaxHeader` 扩展头，节点的 containerd 解析 docker-archive 时不接受。

最终采用**标准 OCI layout**，且 tar 用 `USTAR_FORMAT`：

```
oci-v1.62/
├── oci-layout                      {"imageLayoutVersion":"1.0.0"}
├── index.json                      manifests[0].annotations["org.opencontainers.image.ref.name"]=tag
└── blobs/sha256/
    ├── <manifest digest>           mediaType=application/vnd.oci.image.manifest.v1+json
    ├── <config digest>             mediaType=application/vnd.oci.image.config.v1+json
    └── <layer digest>              mediaType=application/vnd.oci.image.layer.v1.tar+gzip
```

要点：

| 字段 | 值 / 规则 |
| --- | --- |
| `config.Entrypoint` | `["/app/bin/matrix-server"]` |
| `config.Cmd` | `["serve","-config","/etc/matrix/config.yaml"]` |
| `config.User` | `10001:10001`（对应 Deployment 的 `runAsUser: 10001`） |
| `config.Env` | `TZ=Asia/Shanghai`、`MATRIX_STATIC_DIR=/app/public`、`PATH=...` |
| `config.ExposedPorts` | `{"8080/tcp":{}}` |
| `config.Labels` | `org.opencontainers.image.version=v1.16`、`.revision=ac63d0f` |
| `rootfs.diff_ids` | **未压缩** layer.tar 的 sha256（本次 `sha256:dac0370558dc32e7a842e18a078012b873e4178b4d060087318eace4b6779020`） |
| manifest `layers[0].digest` | **gzip 后** blob 的 sha256（本次 `sha256:f0c6df809d59a586867ea1b01a954186a4f1c8a13193462a67f446e1d0dd6915`） |
| 打包格式 | `tarfile.USTAR_FORMAT`，路径用正斜杠，文件名均 <100 字符 |

生成脚本保存在工作区 `.build/mk_oci.py`（已被 `.gitignore` 排除，不提交）；产物 `oci-v1.62.tar` 19 435 520 字节。

### 3.4 分发到三个节点并导入

```bash
scp -F ~/.ssh/config oci-v1.62.tar k8s-master:/tmp/
ssh k8s-master 'ctr -n k8s.io images import --all-platforms /tmp/oci-v1.62.tar'
# master → node1/node2（节点间免密）
ssh k8s-master 'for h in 192.168.6.4 192.168.6.5; do
  scp -q /tmp/oci-v1.62.tar root@$h:/tmp/
  ssh root@$h "ctr -n k8s.io images import --all-platforms /tmp/oci-v1.62.tar"
done'
```

三节点导入后 digest 一致：`sha256:cc319c106484c9f6415d45da070e0d9c28027ff9785f2f1041427716dafea17a`。

> 注意 `-n k8s.io`：这是 containerd 的 kubelet 命名空间，不写会用 `default`，kubelet 看不到镜像。

### 3.5 切换 Deployment

```bash
ssh k8s-master 'export KUBECONFIG=/etc/kubernetes/admin.conf; \
  kubectl -n xingshu-system set image deploy/xingshu-matrix web=docker.io/xingshu/matrix-server:v1.62; \
  kubectl -n xingshu-system rollout status deploy/xingshu-matrix --timeout=180s'
# deployment.apps/xingshu-matrix image updated
# deployment "xingshu-matrix" successfully rolled out
```

镜像里 `imagePullPolicy: IfNotPresent`，节点已有本地镜像，不会去仓库拉取（集群无外网也无妨）。

### 3.6 验证（这次验的是"内容"，不只是"活着"）

```bash
B=http://192.168.6.3:30080
curl -sk $B/api/v1/version
# {"version":"v1.16","commit":"ac63d0f","build_time":"2026-10-20T15:12:15Z","go_version":"go1.25.7","platform":"linux/amd64"}

# 脏代码必须消失
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/oncall/overview    # 404 ✅
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/oncall/schedules   # 404 ✅
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/auth/sso/config    # 404 ✅

# 正常路由仍然存在
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/clusters           # 401 ✅
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/users              # 401 ✅
curl -sk -o /dev/null -w "%{http_code}\n" $B/api/v1/approvals          # 401 ✅

curl -sk $B/healthz                                                   # ok
curl -sk -o /dev/null -w "%{http_code}\n" $B/                       # 200
curl -sk -o /dev/null -w "%{http_code}\n" $B/xs-api.js               # 200
```

| 验收项 | 结果 |
| --- | --- |
| 运行 Pod | `xingshu-matrix-7db865b5b4-dcmnb`、`-gst42` 均 `1/1 Running`，`rollout status` 成功 |
| 版本 | `v1.16` / commit `ac63d0f` / 构建于 `2026-10-20T15:12:15Z` |
| 脏路由 | oncall ×3、sso ×1 全部 404 |
| 正常路由 | clusters / users / approvals 仍 401 |
| 健康与前端 | `/healthz` ok、`/` 200、`/xs-api.js` 200 |

---

## 四、如何避免

1. **构建镜像前强制检查工作区**
   ```bash
   test -z "$(git status --porcelain)" || { echo "工作区不干净，禁止构建"; exit 1; }
   git rev-parse --short HEAD   # 用这个哈希，而不是手写的字符串
   ```
   并且 `Commit` 必须由 `$(git rev-parse --short HEAD)` 注入，不能手填。

2. **不要把 `version` 接口当作内容证据**。它只能证明"构建脚本想标记成哪个提交"，不能证明"代码是什么"。
   内容验证要用**契约断言**：本次用"已删除的路由必须 404、保留的路由必须 401"。

3. **每次发布固定写三条断言**（建议进 `scripts/smoke-test.sh`）：
   - 一个只存在于目标版本的标识（如新增路由返回 401）
   - 一个只存在于旧版本、必须消失的标识（如 `oncall/*` 返回 404）
   - 对照路径必须 404（防止"全都 401/全都 404"的假通过）

4. **镜像加 OCI label**：`org.opencontainers.image.revision` 与 `.version`，用 `crictl inspect` / `ctr images info` 即可核对，不必进容器。

5. **构建只走 CI**：`.github/workflows/docker.yml` 里已经在跑 `go build/vet/test`，把镜像 tag 改成 `${GITHUB_REF_NAME}`，
   杜绝"人工离线构建 + 手填 commit"。

6. **镜像 tag 语义固定**：tag 用应用版本（`v1.16`），构建序号降级为 label；`v1.34/v1.57/v1.61/v1.62`
   这种编号只能当历史产物，不能再作为"当前版本"的口径。

7. **发布后对比 Pod 与清单**：`kubectl get deploy -o jsonpath` 的镜像必须等于清单里的镜像，
   否则说明有人手工改过，需要回写仓库（本次就是先发现清单 v1.34 与 live v1.61 不一致）。

8. **清理节点 `/tmp` 的镜像包**（19 MB × 3），避免长期堆积：
   ```bash
   ssh k8s-master 'rm -f /tmp/oci-v1.62.tar /tmp/image-v1.62.tar.gz'
   ```
