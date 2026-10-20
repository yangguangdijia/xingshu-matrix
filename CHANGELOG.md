# 更新日志

本文件记录面向使用者与运维者的版本变更。更细的逐页状态见 [docs/PROGRESS.md](docs/PROGRESS.md)。

版本规则：应用版本取 `vMAJOR.MINOR`（如 `v1.16`），通过 `git tag` 与 `--build-arg VERSION` 写入二进制；
集群内镜像 tag 使用构建序号（如 `v1.34`），两者不通用。

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
- **更新 `README.md`**：版本徽章 v1.16；重写「当前状态」（明确 20/28 页面已接真实接口、值班页后端缺失、
  真实集群已验证）；功能一览加 `[真]/[演]/[缺]` 标注；补目录结构、质量门禁、版本与镜像标签说明、路线图。
- **新增本文件**。

### 校验（实测）

| 检查 | 结果 |
| --- | --- |
| `cd server && go build ./...` | 通过，无输出 |
| `cd server && go vet ./...` | 通过，无输出 |
| `cd server && go test ./...` | bastion / cicd / cluster / hosts / middleware / observability 六个包 `ok` |
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
- **部署**：新增 Istio 入口网关与路由（`deploy/k8s/istio-gateway.yaml`）；清单镜像对齐 `v1.34` 供 Argo CD 接管；
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
