# 未完成 / 未落实清单（v1.17）

> 生成时间：2026-10-20（基线 `18c3999`，tag `v1.16`）；v1.17 更新于 2026-10-21（基线 `f9cd41e`）
> 本文只记录**没做完的部分**：你明确要求但我没执行的、项目功能本身缺的、以及我提了但你没拍板的。
> 已完成项见 [PROGRESS.md](PROGRESS.md)；每页状态见 PROGRESS 的接线矩阵。
> 判定原则：每条都附**可复核的证据**（命令输出 / 文件行号 / 远端引用），不确定的标「未核实」。

---

## 0. v1.17 变更摘要

本清单的原始条目保留（作为历史记录），下面只写 v1.17 改变了的结论：

| 条目 | v1.16 | v1.17 | 证据 |
| --- | --- | --- | --- |
| B2 堡垒机 | 纯演示 | **部分接线**（资产 / Web 终端 / 访问工作台真实，其余子页标注静态示例） | `basWorkbench` / `basLiveSSH` / `basRenderLiveAssets` / `basStaticNote` |
| B8 SSH | 未接 | **已接真实 WebSocket**；RDP / VNC / SQL 仍未实现 | `basLiveSSH` 与 `ssh.go` 的 `wsMsg` |
| B7 堡垒机测试 | 5 个用例 | **12 个用例**（+7） | `bastion_test.go` |
| A2 浏览器实测 | 0 个视口 | 补测 1366×800 与 600×800；**1440×900 / 1680×900 / 390×844 仍未测** | 本轮浏览器会话记录 |
| 新增未验证项 | — | 真实 PostgreSQL 迁移、真实 SSH 目标机、guacd 代理 | `docs/deploy-v1.63-bastion-workbench.md` §4 |

---

## 0.1 先纠正上一版报告里的两处错误

写这份清单时复核数字，发现上一版报告有两处错误，已在 PROGRESS / README / PLATFORM 中同步修正：

| # | 上一版说法 | 实际 | 影响 |
| --- | --- | --- | --- |
| 1 | 前端「真实 20 / 演示 7 / 后端缺失 1」 | 实际 **真实可用 19 / 前端已接但后端缺失 1（oncall） / 纯演示 8** | 20+7+1=28 看似成立，但 oncall 被同时算进「真实 20」和「后端缺失 1」；真正的演示页有 8 个（多集群、流量治理、网络策略、链路追踪、堡垒机、工作台、需求管理、测试管理），漏算了 1 个 |
| 2 | 后端「6 个模块包有测试」 | 实际 **7 个包**：`cmd/matrix-agent` + bastion / cicd / cluster / hosts / middleware / observability | 上一轮用 C 盘缓存跑 `go test`，输出被 `tail` 截断，漏掉了 `cmd/matrix-agent` 那一行；用 D 盘缓存重跑后完整可见 |

修正后的口径：**页面 19 真实 / 1 前端就绪后端缺失 / 8 演示；后端 7 个包有测试。**

---

## A. 你明确要求过，但这一轮我没有执行

### A1. ~~没有推送到 GitHub 的 `beta` 分支~~ → 已推送（2026-10-20 补推完成）

| 项 | 事实 |
| --- | --- |
| 约定流程 | checkout `beta` → `git merge --ff-only test` → 打附注标签 → `git push origin HEAD:beta` → `git push origin <tag>` → 用 `git rev-list --left-right --count` 校验与 `origin/beta` 同步 |
| 当前状态 | **已推送成功（2026-10-20 补推）**：`36cca24..dac8dfc  HEAD -> beta`，标签 `v1.16` 已推送到远端 |
| 同步校验 | `git fetch origin && git rev-list --left-right --count origin/beta...HEAD` → `0  0`（远端与本地完全一致）；`origin/beta` = `dac8dfc` |
| 首次推送失败记录（保留） | 2026-10-20 首推 `git push origin HEAD:beta` 返回 128：`fatal: unable to access 'https://github.com/yangguangdijia/xingshu-matrix.git/': Failed to connect to github.com port 443 after 21123 ms: Could not connect to server`；网络恢复后重试即成功，与代码无关 |
| 仍然没做的 | `git merge --ff-only test` 未执行（`test` 分支停在 `36cca24`，与 `beta` 已分叉，需你确认是否合并）；标签推送本身已完成 |

**补齐方式**（已完成，仅保留供复核）

```bash
# 0) 先确认这台机器能连到 github.com:443
git ls-remote --heads origin beta     # 通则继续，超时先修网络/代理

# 1) 按约定快进合并 test（若 test 领先）
git checkout beta && git merge --ff-only test

# 2) 推送分支与标签
git push origin HEAD:beta
git push origin v1.16

# 3) 校验同步（左右两边都应为 0）
git fetch origin && git rev-list --left-right --count origin/beta...HEAD
```

> 结果：`HEAD:beta` 与 `v1.16` 均已推送（远端 tag `refs/tags/v1.16` → commit `dac8dfc`）。
> 剩余可选项：`git merge --ff-only test`——`test` 分支当前在 `36cca24`，`beta` 已领先 4 个提交，
> 直接 ff-only 会失败，需先决定是否把 `test` 快进到 `beta`（`git checkout test && git merge --ff-only beta && git push origin test`）。

### A2. 没有做多分辨率浏览器实测

| 项 | 事实 |
| --- | --- |
| 约定标准 | 改动以 1366×768、1440×900、1680×900、390×844 四个分辨率浏览器实测通过为完成标准 |
| 实际做了什么 | 只做了 Node 层（`node --test` 5 条、`xs-live-core.js` 自测 48 条、`node --check` 3 个文件）与 Go 层（build / vet / test）验证 |
| 实际没做什么 | 没有启动浏览器、没有截图、没有检查控制台错误、没有验证 4 个视口的布局 |
| 本轮文档改动的风险 | 文档改动本身不影响渲染，但**没有实测就等于没有验收**，不能算完成 |
| v1.17 补测 | 已实测 **1366×800**（登录、错误登录、首次改密、访问工作台）与 **600×800**（工作台窄屏），控制台无新增错误；**1440×900、1680×900、390×844 仍未测** |

**补齐方式**

```bash
# 方式一：项目自带的 CDP 冒烟（需要本机 Chrome）
node tests/frontend/mock-server.mjs &      # 端口 18080
node tests/frontend/smoke.mjs              # CDP 端口 19222，会输出并比对 smoke-login.png

# 方式二：连真实后端，逐视口检查
#   http://<服务器>/  →  DevTools 设备栏依次切 1366×768 / 1440×900 / 1680×900 / 390×844
#   检查项：登录页 → 工作台 → 集群 → 用户与权限 → 移动端表格是否横向滚动、控制台是否 error
```

### A3. Go 构建缓存仍然占着 C 盘

| 项 | 事实 |
| --- | --- |
| 你的要求 | Go 构建缓存、模块缓存、临时目录都放 `D:/EvoX/xingshu-matrix` 下，避免占用 C 盘 |
| 实际默认值 | `GOCACHE=C:\Users\jiangpeng\AppData\Local\go-build`（已 **3.3 GB**）、`GOMODCACHE=C:\Users\jiangpeng\go\pkg\mod`、`GOPATH=C:\Users\jiangpeng\go` |
| 这一轮的动作 | 第一轮 `go build` / `go vet` / `go test` 仍用默认 C 盘缓存，又写入了一部分 |
| 已经做的补救 | 建了 `D:/EvoX/xingshu-matrix/.cache/go-build`（**712 MB**）与 `.cache/tmp`，用 `GOCACHE` / `GOTMPDIR` 重跑 build + vet + test，**全部通过**；并把 `.cache/` 加进 `.gitignore` |
| 还没做 | 没有 `go env -w` 持久化（会影响你其它项目的构建缓存位置）、没有迁移 `GOMODCACHE` |

**补齐方式**（需要你点头，因为 `go env -w` 是全局配置）

```bash
# 持久化到 D 盘
go env -w GOCACHE=D:/EvoX/xingshu-matrix/.cache/go-build
go env -w GOMODCACHE=D:/EvoX/xingshu-matrix/.cache/go-mod
go env -w GOTMPDIR=D:/EvoX/xingshu-matrix/.cache/tmp

# 迁移已下载的模块（约 3.3 GB 的 build 缓存可直接删，可重建）
cp -r "C:/Users/jiangpeng/go/pkg/mod" "D:/EvoX/xingshu-matrix/.cache/go-mod"   # 首次需要网络校验时保留 -mod=mod
rm -rf "C:/Users/jiangpeng/AppData/Local/go-build"                             # 释放 3.3 GB
```

> 风险提示：`GOMODCACHE` 迁移后首次构建会做一次模块校验，离线环境下可能失败；建议在网络可用时执行。

### A4. 没有按约定走完整发布流程

| 步骤 | 约定 | 实际 |
| --- | --- | --- |
| 1 | 审查并测试改动 | 做了（Go + Node） |
| 2 | `merge --ff-only test` | **未做**（`test` 停在 `36cca24`，比 `beta` 落后 4 个提交；方向应是 `test` 快进到 `beta`） |
| 3 | 打附注标签 | 做了（`v1.16`） |
| 4 | 推送 `HEAD:beta` 与标签 | **已做**（`36cca24..dac8dfc` + `[new tag] v1.16`） |
| 5 | 用 `git rev-list` 校验同步 | **已完成**：`0  0` |

### A5. 演示数据清理的验收没有继续推进

| 项 | 事实 |
| --- | --- |
| 历史遗留 | 之前记录了一条未完成事项：在 `v1.56` 镜像上清空 localStorage 重新登录、确认进入工作台、验证网络策略页空态，然后继续清理剩余演示数据 |
| 这一轮做了什么 | 只在文档层量化了"还有 8 个页面是演示数据"，**没有改任何页面代码** |
| 为什么没做 | 这一轮的任务是文档与版本对齐，不是清演示数据；如果要做，属于功能改动，需要单独一轮 |
| 版本号提示 | 该条记录里的 `v1.56` 与当前 `v1.16` 不是同一口径（`v1.56` 是集群镜像构建序号），**是否仍适用未核实** |

---

## B. 项目功能本身没做完（代码层缺口）

### B1. 值班 / 故障 / SLO：前端已接，后端为零

| 项 | 事实 |
| --- | --- |
| 前端 | `public/xs-live-ops.js:1983` 起，四个子页调用 `/oncall/overview`、`/oncall/schedules`、`/oncall/incidents`、`/oncall/overrides`、`/oncall/slos` |
| 后端 | `server/internal/modules/` 只有 automation / bastion / cicd / cluster / devcenter / hosts / middleware / observability，**没有 oncall** |
| 用户可见后果 | 进入「值班故障」页会显示「值班概览（GET /oncall/overview）加载失败」横幅，排班列表为空 |

**补齐方式**：新增 `internal/modules/oncall`，表 `oncall_schedules` / `oncall_layers` / `oncall_overrides` / `oncall_incidents` / `oncall_timeline` / `oncall_slos`，权限 `oncall:read|manage`，注册审批类型 `oncall.change`。

### B2. 7 个页面仍是演示数据，另 1 个（堡垒机）部分接线

| 页面 | 路由 | 现状 | 后端是否就绪 |
| --- | --- | --- | --- |
| 多集群 | `#/ops/multicluster` | 演示（`renderMC`） | 无接口 |
| 流量治理 | `#/traffic` | 演示（`renderTraffic`） | 无接口 |
| 网络策略 | `#/netpol` | 演示（`renderNetpol`） | 无接口 |
| 链路追踪 | `#/trace` | 演示（`renderTrace`） | **有接口**（`/observability/traces/*`），前端未接 |
| 堡垒机 | `#/bastion/*` | **部分接线**：资产 / Web 终端 / 访问工作台接真实后端；账号、授权策略、命令过滤、审计、脱敏、数据库仍为静态示例（live 下 UI 标注） | **有 25 条路由**，已接资产与 SSH 会话 |
| 工作台 | `#/workbench` | 演示（本地 `state.reqs`） | 部分（devcenter） |
| 需求管理 | `#/requirements` | 演示（本地 `state.reqs`） | 部分（devcenter） |
| 测试管理 | `#/testing` | 演示（本地 `PLANS`） | 部分（devcenter） |

**补齐优先级建议**：链路追踪（后端现成，成本最低）→ 堡垒机（后端最完整，收益最大）→ 需求/测试（devcenter 扩字段）→ 流量/网络策略（需先写后端）。

### B3. 自动化编排没有执行器

`POST /automation/workflows/:id/trigger` 只把运行记录写成 `queued`（`server/internal/modules/automation/module.go:43`），没有任何 worker 消费队列，流程不会真正执行步骤。

### B4. 真实外部依赖没有生产级联调

Prometheus / Loki / Elasticsearch / Alertmanager / Jaeger / Tempo / Jenkins / Kafka / RabbitMQ / RocketMQ 目前只有 `httptest` 假服务与 client-go fake 测试；真实集群只验证了平台自身链路。

### B5. 没有单点登录

登录页有「企业 SSO」按钮并调用 `/auth/sso/config`，后端没有 OIDC / LDAP 实现，按钮只会提示"尚未配置"。

### B6. 没有 Helm Chart 与高可用

只有 `deploy/k8s/helm-values.example.yaml` 示例值；Ingress 控制器单节点 hostNetwork；PostgreSQL 单副本 + `local-path` PVC，Pod 重建后绑回原节点。

### B7. core 包没有后端单测

`auth` / `rbac` / `users` / `settings` / `approval` / `ai` / `audit` / `platform` 均无 `_test.go`；目前 7 个有测试的包全部集中在业务模块与 agent。

### B8. 堡垒机 RDP / VNC / 数据库协议代理未实现

SSH 已在 v1.17 接通真实 WebSocket（`GET /bastion/sessions/ssh/ws`，消息契约 `input/resize` ↔ `output/error/closed`）。
RDP / VNC 仍只有 guacd 健康检查（`GET /bastion/guacd/health`），没有 Guacamole 协议隧道；
MySQL / PostgreSQL 仍只能登记资产，没有协议代理。前端对这三类如实显示"未配置"，不会伪造会话。

### B9. 集群侧遗留问题（来自真实部署记录）

| 项 | 状态 | 到期/风险 |
| --- | --- | --- |
| `calico-token-refresh.sh` 自愈逻辑仍是坏的 | 本次手工修复 | CNI token 有效期到 **2026-11-17**，再次过期会导致新 Pod 卡 `ContainerCreating` |
| Rancher 主程序重启循环 | 未处理（缺镜像 `rancher/rancher:v2.14.2`） | 不影响平台，但 webhook 依赖它 |
| 应用详情抽屉 6 个面板占位 | 未做 | 服务列表 / 部署编排 / 监控告警 / 应用成员 / 操作记录 / 流程中心 |
| Ingress 控制器单节点 | 未做 | 无 HA，master 故障则入口不可用 |
| `node1` 曾失联约 6 分钟 | 未复现、未查 | 建议查 `dmesg` / `kubelet` 日志 |

---

## C. 我提了、但你没有拍板，所以没做

| # | 事项 | 现状 | 需要你决定什么 |
| --- | --- | --- | --- |
| C1 | 镜像 tag 统一为应用版本 | 四套并存：集群 `v1.62`、Argo/test1 的 `v1.57`、`image.sh` 默认 `v1.3`、离线包 `v40` | 是否改成 `v1.16`，并把构建序号降级为 label |
| C2 | 补 Helm Chart | 只有 values 示例 | 是否本轮就做，还是继续用原生清单 + Argo CD |
| C3 | Go 缓存持久化到 D 盘 | 只做了临时 env，未 `go env -w` | 是否允许改全局 Go 配置（会影响其它项目） |
| C4 | 下版本优先级 | 已给建议（oncall P0 → 堡垒机 P0 → 联调 P1） | 是否按这个顺序排 v1.17 |
| C5 | 是否下架 `/oncall` 菜单 | 现状是点进去报错 | 短期止血（隐藏菜单）还是直接补后端 |

---

## D. 这一轮实际完成的（对照，避免只看到缺口）

| 项 | 证据 |
| --- | --- |
| 修复 CI 引用不存在脚本 | `.github/workflows/docker.yml` 改为 `setup-go` + `go build/vet/test` + `node --test` |
| 新增进度报告 | `docs/PROGRESS.md`（接线矩阵 + 三张清单） |
| 重写技术说明 | `docs/PLATFORM.md` |
| 更新 README | 徽章 v1.16、当前状态、`[真]/[演]/[缺]` 标注、版本与镜像标签说明 |
| 新增 CHANGELOG | `CHANGELOG.md`（v1.0 → v1.16） |
| 新增 CI 修复记录 | `docs/fix-ci-smoke-script.md`（四段式） |
| 新增本清单 | `docs/OUTSTANDING.md` |
| Go 层验证（D 盘缓存） | `go build` ✅、`go vet` ✅、`go test` 7 个包 `ok` |
| Node 层验证 | `xs-api.test.mjs` 5 通过、`xs-live-core.js` 48 断言通过、3 个文件语法通过 |

---

## E. 一句话总结

**已经做完的是"文档与版本对齐 + 一处真实 CI 缺陷修复 + 推送到 beta 并打标签"；还没做完的是"四分辨率浏览器实测"（你定的完成标准里就差这一步）和"功能缺口"（oncall 后端、8 个演示页、执行器、真实联调）。**
下一轮如果要收口，最短路径是：先做四分辨率实测（把 v1.16 真正交付），再按 C4 的顺序补 `oncall` 与堡垒机接线。
