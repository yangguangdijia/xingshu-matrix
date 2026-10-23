# 环境筛选、指标监控与通知渠道修复记录

## 报错问题

本轮发现三类用户可见问题：

1. 顶栏切换环境后，环境菜单显示的集群数量为 0，或总览仍显示其他环境的集群、节点、Pod 和告警。
2. 点击“指标监控”后出现“页面加载出错，请刷新重试”，或者页面显示空白。
3. “消息渠道/通知渠道”页面缺少明确内容，空数据、接口异常和操作状态不可区分。

实际后端数据并非为空：测试集群数据库中存在 `dev` 环境集群和 Prometheus 数据源，Prometheus NodePort 能对 `up`、CPU、内存、网络查询返回成功结果。

## 原因

### 1. 环境数据源不统一

顶栏计数依赖 `window.XS_CLUSTERS` 和 `clAll()`，实时总览只有在进入总览后才请求 `/clusters` 并更新缓存；用户从其他页面直接打开环境菜单时，缓存还没准备好，旧逻辑会把“未加载”当成 0。

同时旧逻辑会把 localStorage 中的集群、演示集群和实时 `/clusters` 结果混在一起，`dev`、`开发`、`prod`、`生产` 等标签又没有统一归一化，导致计数和筛选口径不一致。

### 2. 指标页存在两套渲染路径

`public/index.html` 内有早期的内置指标演示渲染器，`public/xs-live-ops.js` 又有 `/observability/datasources` 与 PromQL 的实时适配器。实时模式初始化、脚本加载顺序和空数据结构没有完全收敛时，异常会冒泡到 `index.html` 的 `safeRender()`，最终只显示通用错误“页面加载出错，请刷新重试”，用户看不到真实接口错误。

### 3. 通知渠道缺少完整空态/异常态验证

通知渠道 API 已存在，但页面没有把“没有配置渠道”“读取失败”“测试失败”“删除或启停失败”分别呈现；mock 测试服务也没有覆盖通知渠道的空数组响应，因此前端空白问题没有被回归测试提前发现。

### 4. 建设中页面的边界没有和后端对齐

`#/trace`、`#/bastion`、`#/testing`、`#/requirements` 的后端接口（`/observability/traces/*`、`/bastion/*`、`/devcenter/*`）早就存在，但前端一直停留在演示渲染器，用户看到的是假数据；而 `#/traffic`、`#/netpol`、`#/ops/multicluster` 在后端没有任何模块，却同样有演示业务数据和成功态，导致“看起来已上线”。两类问题被同一种表象（页面能点开）掩盖，必须按 route 逐个核对后端合同。

## 解决方案

### 1. 环境和总览

- 在 `public/index.html` 统一环境别名：
  - `dev/development` → `开发`
  - `prod/prd/production` → `生产`
  - `pre/preview/staging/uat` → `预发`
  - `test/testing/qa` → `测试`
- 使用 `window.XS_ENV_FILTER` 作为环境桥，提供 `get/set/matches/syncClusters/refreshClusters`。
- live 模式只使用后端 `/clusters`，不再合并 localStorage 或演示集群。
- 首次打开环境菜单时，如果集群数据还没加载，显示“读取中…”；请求失败显示“读取失败”，允许重新打开菜单重试。
- 总览先根据当前环境过滤集群，再请求节点和 Pod；节点、Pod、告警、Prometheus 数据源和 KPI 都使用当前环境的数据。
- 切换环境时同步 `nav.env`、`sre.env`、顶栏计数、总览和实时运维页面的重绘。

### 2. 指标监控

- `public/xs-live-ops.js` 的指标页兼容 datasource 返回数组、`items` 和 `datasources` 三种结构。
- 只选择当前环境可用的 Prometheus 数据源；空数据源显示配置引导。
- PromQL 查询对空查询、无 datasource、401/403/404/5xx、超时、Prometheus 非 success、缺少 `data/result/resultType` 和空结果统一显示页面内错误/空态与重试入口。
- 无演示指标数据时不再对空对象做属性解引用，显示明确的“暂无指标数据”状态。

### 3. 通知渠道

- 页面通过 `/observability/channels` 读取列表。
- 空列表显示“尚未配置通知渠道”，而不是空白页。
- 保留创建、启用/停用、删除和测试发送操作。
- 单个操作失败只显示操作错误，不让整个页面进入通用加载错误。
- mock server 增加空渠道响应，浏览器测试覆盖空态。

### 4. 建设中页面：有后端的接真实数据，没后端的只声明缺口

按 `server/internal/modules/*` 与 `docs/api/*` 逐个路由核对后端合同，结果分两类处理：

| 路由 | 后端合同 | 处理 |
| --- | --- | --- |
| `#/trace` | `GET /observability/traces/services`、`/search`、`/:traceId` | 新增实时渲染器：数据源（jaeger/tempo）→ 服务列表 → 检索 → 明细，全部走真实接口 |
| `#/bastion` | `GET /bastion/assets`、`/bastion/sessions` | 新增实时渲染器：主机资产 + 会话审计双区块，单区块失败只在本区块显示错误与重试 |
| `#/testing` | `/devcenter/tests` 的 list / create / patch | 新增实时渲染器：列表 + 新建 + 行内状态变更 |
| `#/requirements` | `/devcenter/requirements` 的 list / create / patch | 同上，与测试页共用一套渲染器 |
| `#/traffic` | 无（没有 mesh / gateway / 限流接口） | 删除演示数据，只显示「当前版本未提供后端能力」与可返回的支持页面链接 |
| `#/netpol` | 无（没有 NetworkPolicy / 连通性探测接口） | 同上 |
| `#/ops/multicluster` | 无（没有跨集群对比 / 资源分发接口） | 同上 |

这三条缺口路由不再渲染任何业务假数据和成功态，也不新增后端接口。

### 5. 验证结果

本地（mock 服务 + 一次浏览器 run，桌面与 390px 窄屏各一次菜单检查）：

- `node --check`：`public/xs-live-core.js`、`public/xs-live-ops.js`、`tests/frontend/mock-server.mjs`、`tests/frontend/smoke.mjs` 以及从 `public/index.html` 抽取的 4 段内联脚本全部通过。
- `node --test tests/frontend/xs-api.test.mjs`：5 passed；`node public/xs-live-core.js`：48 断言通过。
- `go build ./...`、`go vet ./...`、`go test ./...`：通过（缓存目录 `D:/EvoX/xingshu-matrix/.cache/{go-build,tmp}`）。
- 浏览器 run（`SMOKE OK`）：环境菜单 全部 2 / 生产 1 / 开发 1；切生产只显示 `demo-prod` 与 `prod-node-1`，切开发只显示 `demo-dev` 与 `dev-node-1`；`#/observe` 显示 `prometheus-dev`，PromQL `up` 返回矩阵、`simulate_error` 显示 502 + 重试、`empty_result` 显示空态；通知渠道依次验证 空态 → 500 错误 + 重试 → 列表 → 测试发送；`#/trace`、`#/bastion`、`#/testing` 显示真实列表，`#/traffic`、`#/netpol`、`#/ops/multicluster` 显示能力缺口；控制台无 uncaught error。

测试集群（`xingshu-system/xingshu-matrix`，NodePort 30080）：

- 部署前镜像 `docker.io/xingshu/matrix-server:v1.16-local`（revision 105）；本次镜像 `docker.io/xingshu/matrix-server:v1.16-local-r2`，manifest digest `sha256:4ec63b3a1788c038236cfbcc467c7d8d61286033ab24c07b49b51e7f2c1569fc`，三节点 `ctr -n k8s.io images ls` 中该 tag 的 digest 一致。
- `rollout status` 成功，两个 Pod 均 `Running / Ready`、0 重启，`imageID` 均为 `sha256:2e406cb2fdaa67f412f72317883e6c796736e3200e9fcce9a8218bb30936aeff`。
- `/healthz` 200；`/api/v1/version` 返回 `version=v1.16-local-r2`、`commit=f1b4b7a-dirty`；`/`、`/xs-api.js` 200，`/xs-live-ops.js` 已含新的缺口页逻辑。
- 真实集群浏览器验收：环境菜单 全部 1 / 开发 1（其余 0，且无「读取失败」）；切到开发后总览只显示 `k8s-192.168.6.3`，节点 3/3、Pod 56/56、CPU 29% 取自 metrics-server；`#/observe` 列出 `prometheus-cluster`（env=dev），PromQL `up` 与 `sum(up) by (job)` 均返回矩阵；`#/observe/channels` 显示「尚未配置通知渠道」空态；390px 窄屏下环境菜单不溢出；控制台无错误。

> 首次 rollout 出现过一次 CrashLoopBackOff：`exec: "/app/bin/matrix-server": permission denied`。原因是手工 OCI 打包时 layer tar 内的二进制没有可执行位（Windows 文件系统上 `chmod` 无效）。改为按文件名显式写入 `0755` 重打 layer、重新导入后 rollout 通过。失败期间线上旧 Pod 未中断，旧镜像 `v1.16-local` 始终可回滚。

回滚方式：`kubectl -n xingshu-system set image deploy/xingshu-matrix web=docker.io/xingshu/matrix-server:v1.16-local`，再 `kubectl -n xingshu-system rollout status deploy/xingshu-matrix --timeout=180s`。

## 如何避免

1. 环境菜单打开时必须区分“未加载”“加载失败”“真实数量为 0”，不能把三种状态都渲染成 `0 个集群`。
2. live 模式不能合并 localStorage/demo 集群；演示数据和真实数据必须由 `XS.mode` 明确隔离。
3. 所有环境值进入筛选前必须经过同一个归一化函数，不能在不同页面分别判断 `dev`、`开发`、`prod` 等字符串。
4. 前端数据请求必须在页面级错误边界内处理；普通接口错误显示在当前页面，不允许冒泡到全局 `safeRender()`。
5. 监控页必须覆盖 datasource 空数组、Prometheus 非 success、空 result、超时和权限错误测试。
6. 通知渠道必须有空列表、读取失败、创建失败、测试失败、启停失败和删除失败状态，不能只测试成功路径。
7. 部署前必须使用真实 API 做一次验证：读取集群环境、读取 datasource、执行 `up` PromQL，并核对页面静态资源和浏览器 console。
8. 不使用浏览器截图循环代替诊断；先读页面文本、网络响应和 console 错误，再做一次针对性 UI 验证。
9. 手工打 OCI 镜像时必须显式写入文件模式：Windows 上 `chmod +x` 对 NTFS 不生效，layer tar 里的二进制会变成 `0644`，表现为 `exec: permission denied`。打包后应 `tar -tvf` 核对入口文件是 `-rwxr-xr-x`。
10. 页面要么接真实接口，要么明确标注“当前版本未提供后端能力”；不能保留演示业务数据与假成功态，否则会把“没有后端”伪装成“已上线”。
