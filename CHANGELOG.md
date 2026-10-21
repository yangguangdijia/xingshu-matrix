# 更新日志

本文件记录面向使用者与运维者的版本变更。更细的逐页状态见 [docs/PROGRESS.md](docs/PROGRESS.md)。

版本规则：应用版本取 `vMAJOR.MINOR`（如 `v1.16`），通过 `git tag` 与 `--build-arg VERSION` 写入二进制；
集群内镜像 tag 使用构建序号（当前 `v1.63`，commit `f9cd41e`），两者不通用。

---

## v1.17 — 2026-10-21

主题：**堡垒机从演示页变成访问工作台，SSH 接真实 WebSocket，登录页补齐可访问性**。

### 新增

- **堡垒机访问工作台**（`#/bastion/term` 无会话时）：连接器状态卡片（SSH / RDP / VNC / SFTP / SQL）、
  可访问资产表（含授权账号）、最近会话表。状态只认后端探测：`可用 / 未配置 / 未授权 / 未连接 / 无法确认`。
- **真实 SSH 会话**：live 模式通过 `GET /bastion/sessions/ssh/ws` 建立 WebSocket，
  消息契约 `{type:input,data}` / `{type:resize,cols,rows}` ↔ `{type:output}` / `{type:error}` / `{type:closed}`。
  前端用轻量屏幕模型渲染（剥离 ANSI、处理 `\r` 覆盖与 `\b` 退格、`\x1b[2J` 清屏），不引入 xterm 运行时依赖。
- **资产协议参数持久化**：迁移 `0002_asset_options.up.sql` 新增 `options JSONB NOT NULL DEFAULT '{}'::jsonb`，
  保存资产组、标签、来源、跳板、域、TLS、库名、只读、分辨率等非敏感配置。
- **资产来源选择**：手动 / Agent 自动发现 / K8s 同步 / 云同步 / 批量导入；只有"手动"本轮真实写入后端，其余在 UI 上标注未联调。

### 修复

- **`BAS_CONN()` 硬编码全 `false`**：导致即使后端可用，任何协议都被判"连接器未配置"，SSH 永远走不到 WebSocket。
  现改为 `basProbeConnectors()` 真实探测 `/bastion/my/assets` 与 `/bastion/guacd/health`。
- **堡垒机被"未接集群"网关拦住**：`#/bastion/term` 在未接集群时只显示 `renderNoCluster`，
  访问工作台不可达。现对 `current() === "bastion"` 豁免，集群页逻辑未改。
- **资产页 live 模式仍读演示数组**：现读 `GET /bastion/assets?size=200`，状态列显示「已登记」而不是伪造的「在线」。
- **登录/改密可访问性**：账号/密码/三个改密输入框补 `for`/`id`，`#cerr` 补 `aria-live="polite"`，
  错误时设置 `aria-invalid` 并在输入时清除，移动端可点区域提升到 44px。

### 安全

- `assetIn.check` 拒绝 `options` 中出现 `secret` / `password` / `passphrase` / `private_key` / `privatekey` /
  `token` / `kubeconfig` / `credential`（大小写不敏感子串），上限 4096 字节；凭据只能走 `POST /bastion/accounts` 的 `secret`。
- live 会话的审计与录像由后端 `bastion_sessions` / `bastion_commands` 记录，前端不再补写演示审计行。

### 测试

- 堡垒机新增 7 个用例：`assetIn` 校验与凭据拒绝、`options` 大小限制、guacd 不可达、401/403 权限门、
  凭据脱敏、会话强制断开、env scope 过滤。`go test ./...` 与 `go vet ./...` 通过。

### 文档

- 更新 `docs/api/bastion.md`（options、资产来源、连接器状态、SSH 消息契约、验证边界）。
- 新增 `docs/bastion-product-patterns.md`（产品模式与实现边界）。
- 新增 `docs/bastion-round-fix-notes.md`（本轮 5 个问题的四段式复盘）。
- 新增 `docs/deploy-v1.63-bastion-workbench.md`（v1.63 镜像构建、分发、滚动与回滚记录）。

### 部署

- 镜像 `docker.io/xingshu/matrix-server:v1.63`（commit `f9cd41e`，`GET /api/v1/version` → `version=v1.17`）。
- 构建方式：`.build/mk_oci_v163.py` 复用 v1.62 基础层 + 追加应用层（USTAR），产物 `oci-v1.63.tar` 38,748,160 字节。
- 三节点 `ctr -n k8s.io images import` 后 digest 一致：`sha256:ec081a8d7b009353f14232129e2f19cce9344deee18044f759a0905f34e31422`。
- 清单 `deploy/k8s/xingshu-matrix-dev.yaml` 镜像指向 `v1.63`，由 Argo CD（Application `xingshu-matrix`，path `deploy/k8s`，targetRevision `beta`，automated sync）同步；
  推送后手动 refresh 一次以立即生效。
- 滚动结果：2 个 Pod 均为 `v1.63` 且 `READY`；`GET /api/v1/version` → `version=v1.17`；首页 HTML sha256 与本地 `public/index.html` 一致（`9aa0a2aa…`）；
  `bastion_assets.options` 已为 `jsonb NOT NULL DEFAULT '{}'`。完整参数见 `docs/deploy-v1.63-bastion-workbench.md`。

### 未验证

- 真实 PostgreSQL 上的 `0002_asset_options` 迁移与资产/授权/审计 SQL；
- 真实 SSH 目标机（TOFU、PTY 尺寸、命令过滤命中、录像落盘）；
- guacd 的 RDP / VNC 代理（本轮仍未实现）；
- Kubernetes、Prometheus、日志、链路、Jenkins、Kafka、真实 SSO。

---

## v1.16 — 2026-10-20

主题：**文档与版本对齐，修复从第一次提交就坏掉的 CI**。

### 修复

- **CI 工作流引用不存在的脚本**：`.github/workflows/docker.yml` 原来的步骤是
  `cd scripts && npm i jsdom@24 --no-save && node smoke-test.mjs`，而 `scripts/` 下只有 `package.sh` 与
  `smoke-test.sh`，`smoke-test.mjs` 从未存在，`jsdom` 也不被任何测试使用。
  任何一次 push 或打 tag 都会在 Smoke test 步骤失败，镜像从未被自动构建。
  现改为：`actions/setup-go@v5`（Go 1.23）+ `go build ./...` + `go vet ./...` + `go test ./...`
  + `node --test tests/frontend/xs-api.test.mjs`。详见 [docs/fix-ci-smoke-script.md](docs/fix-ci-smoke-script.md)。

### 文档

- **新增 `docs/PROGRESS.md`**：28 个页面逐页接线矩阵、已完成 / 部分完成 / 未开始三张清单、
  自复核命令与实测输出、已知风险、下一版优先级。
- **重写 `docs/PLATFORM.md`**：版本对齐 v1.16；补 IAM 部门与用户组、`platform` 层（访问域名 / 运行环境 / 公告）、
  告警规则走集群内 `PrometheusRule`、AI 工具表与写操作确认规则；未实现清单逐条复核后重写。
- **更新 `README.md`**：版本徽章 v1.16；重写「当前状态」（明确 19/28 页面真实可用、值班页后端缺失、
  真实集群已验证）；功能一览加 `[真]/[演]/[缺]` 标注；补目录结构、质量门禁、版本与镜像标签说明、路线图。
- **新增 `docs/OUTSTANDING.md`**：未完成 / 未落实清单（未做四分辨率浏览器实测、Go 缓存仍在 C 盘、
  oncall 后端缺失、8 个演示页、执行器等），并纠正首版报告里"20/7/1"的页面计数错误与"6 个测试包"的漏计。
- **新增本文件**。

### 交付

- 推送到 GitHub `beta` 分支：`36cca24..dac8dfc  HEAD -> beta`；附注标签 `v1.16` 已推送（远端 `refs/tags/v1.16` → `dac8dfc`）。
- 同步校验：`git rev-list --left-right --count origin/beta...HEAD` → `0  0`。
- 仍未做：1366×768 / 1440×900 / 1680×900 / 390×844 四分辨率浏览器实测（见 `docs/OUTSTANDING.md` §A2）。

### 集群核对（192.168.6.3）

| 检查项 | 结果 |
| --- | --- |
| 集群 | 三节点 `Ready`（k8s-master / node1 / node2，v1.29.9） |
| 工作负载 | `postgres-0 1/1`、`redis 1/1`、`xingshu-matrix 2/2 Running`，`rollout status` 成功 |
| 运行版本 | `GET /api/v1/version` → `version=v1.61`、**`commit=1c2f62b`**、`build_time=2026-10-20T14:05:00Z`、`go1.25.7 linux/amd64` |
| 代码一致性 | `1c2f62b` 即当前 `beta` HEAD → **集群跑的就是当前代码**，无需重新部署 |
| 前端资源 | `/`、`/xs-api.js`、`/xs-live-core.js`、`/xs-live-ops.js`、`/chart.js` 全部返回 200（非空壳镜像） |
| 清单一致性 | 修正 `deploy/k8s/xingshu-matrix-dev.yaml` 镜像 → `v1.62`，与集群实际对齐（v1.61 含未提交代码，已重建） |
| Argo CD | application `xingshu-matrix` 跟踪 `deploy/k8s`、target 分支 `test1`（`e6e0c31`、镜像 `v1.57`），状态 `OutOfSync` + `Progressing`；`syncPolicy.automated` 为空，**未开启自动同步，不会自动回滚到 v1.57** |
| 遗留 | Argo 的 source 分支是 `test1` 而不是 `beta`，与仓库主线不同源；是否切换需另行确认 |
| **脏镜像修复** | v1.61 的 `version` 显示 `commit=1c2f62b`，但 `/api/v1/oncall/*` 返回 **401** 证明二进制里混入了未提交代码；已用干净代码重建 **v1.62**（`version=v1.16`、`commit=ac63d0f`），切换后 oncall/sso 全部 404，见 [docs/fix-stale-image-uncommitted-code.md](docs/fix-stale-image-uncommitted-code.md) |

### 校验（实测）

| 检查 | 结果 |
| --- | --- |
| `cd server && go build ./...` | 通过，无输出 |
| `cd server && go vet ./...` | 通过，无输出 |
| `cd server && go test ./...` | 7 个包 `ok`（`cmd/matrix-agent` + bastion / cicd / cluster / hosts / middleware / observability） |
| `node --test tests/frontend/xs-api.test.mjs` | 5 通过 / 0 失败 |
| `node public/xs-live-core.js` | 48 条断言全部通过 |
| `node --check public/xs-{api,live-core,live-ops}.js` | 三个文件语法均通过 |

---

## v1.11 — v1.15（IAM、AI 全覆盖、集群真实字段）

- **IAM**：部门后端 CRUD（`GET/POST/PUT/DELETE /departments`，支持上级部门与排序）、部门页接真实接口并修空态崩溃；
  用户组模块（建表 `user_groups` / `user_group_members` / `user_group_roles` + CRUD + 页面接线）；
  用户新建/编辑接后端并支持归属部门；用户管理页接 `/users`（列表 + 启用/禁用）；登录日志页接 `/audit-logs`。
- **AI**：告警规则对话框、创建工作负载、新建用户、部门与用户组、接入集群对话框全部接入 `/ai/assist`，
  完成"全部对话框覆盖"；模型用途标签与备注落库（`0005_ai_model_meta.up.sql`）；
  修复创建模型时 `tags` 为 `nil` 导致 500、测试连接未触发请求、`tfErr` 空元素崩溃。
- **可观测**：告警规则页改接集群内 `PrometheusRule`（后端 `GET/POST /clusters/:id/alert-rules`）。
- **集群**：节点详情改为真实字段，不再用哈希编造。
- **部署**：新增 Istio 入口网关与路由（`deploy/k8s/istio-gateway.yaml`）；清单镜像最初对齐 `v1.34`（v1.16 已改为与集群一致的 `v1.62`）供 Argo CD 接管；
  `image.sh` 修三处真实缺陷；`.gitattributes` 强制 LF；新增离线镜像包与本地优先的镜像后勤脚本。

## v1.4 — v1.10（前端可用性建设）

- 可视化编辑器：工作负载、ConfigMap/Secret、资源创建、日志查询构建器、告警规则构建器、编排步骤参数。
- 堡垒机相关可视化：资产详情抽屉、协议感知的资产注册（含 AI 填充）、授权策略、命令过滤（正则可校验 + 命中测试）、
  数据脱敏规则（实时预览）、数据库变更单（SQL 风险分级）、访问申请（审批链预览）。
- 集群与主机：集群接入预检清单、节点池扩容向导（含驱逐估算）、应用实例详情抽屉、应用接入表单、
  主机分组改名/合并/删除（含迁移）、主机导入预检、批量主机操作、CronJob 下次运行预览。
- 登录页与整体视觉：按参考稿重建登录页、品牌星形几何、右上角标语、右侧竖排说明、底部版权位置，多轮收敛到 15px。
- 环境切换修复：切换环境后中间件与发布单列表按环境过滤；堡垒机资产/账号/策略改为按连接器真实数据重建。
- 移除页头、页头与创建对话框里的独立 AI 入口，改为场景化内嵌。

## v1.1 — v1.3（后端落地与首次部署）

- Go 后端：核心（auth / rbac / users / settings / audit / approval / ai）+ 8 个业务模块 + `matrix-agent`，
  按模块版本化迁移、`/healthz`、`/api/v1/health`、`/metrics`、优雅退出。
- 接口文档 8 份（`docs/api/`）与开发规范（`docs/dev-guide.md`）。
- 部署：Dockerfile 多阶段、`docker-compose.yml`、K8s 清单、物理机安装脚本、`scripts/package.sh`、`scripts/smoke-test.sh`。
- 真实环境首次部署到 `192.168.6.3`，记录 Calico CNI token 过期、命名空间 webhook 拦截、离线镜像导入、
  静态资源缓存不生效、IPVS `externalIPs` 造成 ARP 冲突等 7 个真实故障与解法（`docs/deploy-192.168.6.3.md`）。

## v1.0 — 2026-10-15（前端原型）

- 初始提交：`public/` 单页原型（`index.html` + `hark*.css/js` + `chart.js`）、`Dockerfile`、`docker-compose.yml`、
  `deploy/nginx.conf`、K8s 清单、`.github/workflows/docker.yml`、`README.md`。
- 后续提交补齐中间件（Kafka / RabbitMQ / RocketMQ）与应用商店官方图标。

---

> 说明：除 `v1.3`、`v1.15`、`v1.16` 外，历史版本号按提交主题归纳，用于阅读，不与某个具体 commit 一一对应；
> 精确变更请用 `git log --oneline`。
