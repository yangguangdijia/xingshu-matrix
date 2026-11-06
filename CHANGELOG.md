# 更新日志

本文件记录面向使用者与运维者的版本变更。平台架构与开发说明见 [docs/PLATFORM.md](docs/PLATFORM.md)。

版本规则：应用版本使用 `vMAJOR.MINOR` 格式，并可通过 Git tag 与 `--build-arg VERSION` 写入二进制；
部署镜像 tag 可按发布流程单独编号。

---

## v1.28 — 2026-11-06

主题：**堡垒机能力补全、通知中心、编译缓存与语言识别、研发协作与统计、Web kubectl 终端、企业身份与平台设置**。

### 新增

- **Web kubectl 终端**（顶部栏「kubectl」）：多标签页、每个标签绑定一个集群，命令由平台使用该集群凭据执行，直连与 Agent 接入的集群都可用。支持 `get`（与 kubectl 相同的列，`-o wide|yaml|json|name`、`-l`、`-A`、多资源）、`describe`、`logs`、`top`、`rollout status|history`、`api-resources`、`exec`；`scale`、`rollout restart`、`delete`、`cordon/uncordon` 转为平台变更单，按环境审批并记审计，生产集群还需在终端内输入集群名确认。工作负载页的「Shell」直接打开容器终端。接口 `POST /clusters/:id/kubectl`。
- **堡垒机**：资产树节点与平台（系统类型）管理；托管账号连通性验证；**改密计划**（定时 / 手动、执行记录）；**命令复核**（过滤规则动作为「审批」时命令暂存，审批通过后执行）；**登录 ACL** 与登录日志；**数据库资产与 Web SQL 控制台**（只读 / 读写策略、高危语句拦截、SQL 工单与查询记录）；控制台总览与全局命令记录；会话强制断开。
- **通知中心**：渠道、消息模板（变量渲染与预览）、订阅规则（按事件类型、级别、环境路由到渠道）、事件来源、发送记录与失败重发、出站地址安全策略（默认拒绝内网地址，可配置白名单）。
- **CI/CD**：从源码自动识别语言与构建方式（Java Maven/Gradle、Go、Node.js、Python，以及仓库自带的 Dockerfile）并生成 Dockerfile 模板；**编译缓存**（依赖缓存卷与 Kaniko 层缓存，可查看命中、清理），流水线运行详情展示缓存命中与耗时。
- **研发中心**：评论、变更动态、需求 / 缺陷关联代码仓库与流水线；**统计报表**（吞吐量、周期时间与前置时间 P50/P85、状态与严重程度分布、处理人负载）与迭代燃尽图；**AI 开发**：选择需求由大模型生成实施方案与子任务，确认后一键创建。
- **企业身份**：LDAP 登录（连接测试、用户字段映射、组到角色映射、首次登录自动建号）。
- **平台设置**：Logo 与主题色；系统邮件（SMTP）与自助找回密码；告警可升级为故障。

### 变更

- 工作台问候语按时间段显示；总览把节点状态与资源使用排行合并为一个卡片。
- 组件中心的安装改为右侧抽屉。
- 命名空间删除改为输入名称确认；`default`、`kube-system`、`kube-public`、`kube-node-lease`、`xingshu-system` 在前后端都禁止删除，新建时校验名称格式。
- 文档：README 重写；新增编码规范 `docs/dev-guidelines.md` 与机制规划 `docs/roadmap-gaps.md`；修正后端架构文档中与实现不一致的描述（任务队列、密钥存储）。

### 已验证（沙箱 k3s）

- Web kubectl：`get/describe/logs/top/rollout/exec` 输出正确；`scale` 经变更流程执行；管道、重定向、`-f` 跟踪与 `apply -f` 被拒绝。
- 各模块单元测试与 `go vet` 通过；全路由浏览器回归（含新增页面）。

### 已知限制

- 真实钉钉、飞书、企业微信、Slack 机器人，真实大模型，外部 MySQL / PostgreSQL 资产，多节点编译缓存调度，真实 LDAP 组到角色映射尚未在沙箱外联调。
- 主机 Agent 连接只在持有它的平台副本内可用（集群 Agent 已支持跨副本），多副本部署时主机命令可能需要重试；规划见 `docs/roadmap-gaps.md`。
- Web kubectl 的 `exec` 为非交互执行，不支持 vi、top 等全屏程序；不支持持续跟踪日志。
- 堡垒机文件传输尚未提供；复制粘贴限制只在浏览器端生效。

---

## v1.26 — 2026-11-04

主题：**对照 KubeSphere / Rainbond / JumpServer 整改：统一创建表单（可视化 / AI / YAML），CI/CD 凭证与从代码仓库创建应用，值班故障后端落地，企业 SSO，堡垒机资产树、连通性测试与数据脱敏**。

### 新增

- **统一创建表单**：新建类对话框共用一套组件，支持分步向导、一句话让 AI 填表（密码与密钥字段不会发给 AI）、YAML 双向同步与手动编辑、表单内测试按钮。旧的新建表单（告警规则、排班、故障、SLO、流程等）也补上了 AI 填写与 YAML 视图；认领、关闭这类动作对话框保持简洁。
- **工作负载创建向导**（KubeSphere 式）：基本信息 → 容器 → 存储 → 访问方式 → 高级，提交前 dry-run，再依次创建 PVC、工作负载、Service、Ingress。
- **CI/CD**：凭证管理（Git Token、账号密码、SSH 私钥、镜像仓库），被引用时禁止删除；「从代码仓库创建应用」三步向导，填写地址后可读取分支验证仓库与凭证；SSH 私钥克隆、镜像仓库凭证推送；流水线列表统计、横向阶段图、重新运行、可视化与 YAML 双向编排、模板与 AI 生成。
- **通知渠道**：新增 Slack、通用 Webhook（HMAC `X-Matrix-Signature` 签名）；钉钉 / 飞书加签；按告警级别与环境过滤；保存前「发送测试」。
- **值班与故障**（后端新落地，此前页面调用的接口不存在）：分层轮值排班与代班、当前值班计算；故障声明后按排班自动指派一线值班，认领、处理记录、解决全程时间线；SLO 支持手工录入与 Prometheus 查询，计算剩余错误预算与燃烧率。接口见 `docs/api/oncall.md`。
- **企业 SSO（OIDC）**：设置 → 企业登录（SSO）配置 Issuer、Client、用户字段映射与首次登录自动建号；授权码 + PKCE，地址栏只出现 60 秒一次性票据；保存前可「测试连接」读取发现文档。
- **堡垒机**：资产页改为「环境 → 资产组」资产树 + 统计 + 表格；三步添加资产（资产 → 托管账号 → 授权给我）；真实连通性测试（TCP、SSH 横幅、托管账号登录）；**数据脱敏**规则作用于 SSH 输出，发送到浏览器和写入录像前替换，保存前可预览；「登录安全」展示平台安全策略并跳转修改。
- **研发中心**（实时模式重做，此前工作台 / 需求页显示「尚未接入真实数据接口」，新建用浏览器 prompt）：工作台汇总待我处理与进行中迭代；需求管理按状态流转，验收时逐条核对验收标准、驳回需填写意见；迭代进度条；看板拖动改状态；缺陷与测试用例的处理按钮；代码仓库列出已接入的应用仓库。后端补充处理人、扩展数据、状态校验、全量更新与删除、汇总接口。
- **平台名称全局生效**：设置 → 基本信息里的平台名称用于浏览器标题、侧边栏与登录页（公开接口 `GET /branding`）。
- **单副本录像模式**：`MATRIX_BASTION_RECORDING_MODE=local` 允许单副本部署把录像放在本地卷（多副本仍要求共享 NFS）。

### 变更

- 总览、集群状态改为 KubeSphere 式统计卡片；监控面板四页；服务链路与集群列表的卡片式拓扑。
- 堡垒机实时模式下隐藏尚未实现的「数据库审查」「命令审批」「改密计划」，不再展示空壳页面；页面上不再出现接口路径等开发用语。
- 状态标签统一显示中文（已声明、处理中、已解决等）；后端常见英文错误改为中文说明，例如告警事件页「当前集群还没有接入告警来源」。

### 修复

- 新建通知渠道返回 500：数据库约束只允许 4 种类型，已放开 Slack / Webhook。
- 通知渠道旧表单退化为原生弹窗。
- 执行集群新建表单缺少 `cluster_id`。
- kube-prometheus-stack 安装预检报「chart resource targets a different namespace」；Helm 同名发布返回 409；PrometheusRule 自动匹配 ruleSelector 标签。

### 已验证（沙箱 k3s 实测）

- 值班：排班 + 两层轮值 + 代班计算、故障自动指派、认领重复提交 409、解决、时间线、SLO 手工条目与错误预算。
- SSO：浏览器端到端登录（最小 OIDC 身份源，校验 PKCE 与 client_secret）、自动建号为只读用户、伪造 state 与票据重放被拒、外部 redirect 被改回平台地址。
- 堡垒机：连通性测试（可达 / 端口拒绝 / 错误密码）、SSH 会话中手机号在终端与录像里均被脱敏。
- CI/CD：凭证 + Kaniko 构建 + 部署流水线跑通；工作负载向导创建 Deployment + PVC + Service + Ingress 全部就绪。
- 全路由浏览器回归结果见 `docs/qa/regression-v1.26.json`。

### 已知限制

- AI 填写 / 生成在沙箱未接入模型，只验证了未配置时的提示；需要在「设置 → AI 模型」接入后使用。
- SSO 尚未与 Keycloak、Azure AD 等真实身份源联调；当前以服务端直连 userinfo 的结果为准，不校验 ID Token 签名。
- 钉钉 / 飞书加签只验证了请求格式与错误回显，未用真实机器人验证。
- SSH 私钥克隆只有单元测试；从源码创建时的语言自动识别尚未实现。
- 堡垒机数据库代理、命令审批工单、改密计划尚未实现；数据脱敏在单个输出块内匹配，极少数被拆分的长串可能漏掉。
- 平台没有系统邮件（SMTP）与自助找回密码；邮件告警通过通知渠道里的邮件类型发送。
- 这是可部署的平台版本，但仍建议在预发环境完成联调与压测后再用于生产。

---

## v1.24 — 2026-11-03

主题：**登录页重做、总览跨集群汇总与数据原地刷新，组件目录一键安装，备份默认 Velero，监控大屏与系统设置补全**。

### 新增

- **登录页**：登录卡片、输入框、按钮和功能入口都是真实页面元素，背景使用 `xs-login-scene.jpg`。动效包含粒子漂浮、光轨流动、晶体旋转和鼠标视差；`xs-login-gl.js` 用 WebGL 绘制动效层，浏览器不支持 WebGL 或开启「减少动效」时回退为 CSS 动画，登录页隐藏后 WebGL 自动停止。
- **组件目录一键安装**：未安装的组件行直接显示「安装」按钮，沿用预检、确认、安装的流程；暂无经审核安装方案的组件会给出说明。
- **备份恢复**：默认打开 Velero 标签；集群未安装 Velero 时，显示未安装说明和前往组件目录安装的入口，不显示模拟数据。
- **系统设置**：新增版本与运行信息、企业 SSO 状态、通知与集成入口，以及 Kubernetes 备份恢复（Velero）入口。
- **指标监控大屏**：全屏时切换为深色大屏主题，数值与趋势图放大。

### 变更

- **总览**：「节点 Ready / 总数」「Pod 总数」汇总全部已接入集群，副标题列出各集群分项，读取失败的集群单独标注。
- **总览刷新**：每 15 秒只更新数据节点，不重绘页面，页面在后台时暂停；24 小时趋势图按容器宽度绘制，不再留白。CPU、内存卡片和趋势图仍只针对当前选中的集群。
- **镜像仓库**：页面标题改为「镜像仓库（当前集群）」，镜像加速模板标注为新建集群时选用。
- **危险操作**：删除命名空间、删除工作负载、滚动重启改用带影响说明的确认弹窗。
- **发布构建**：构建时写入版本号、提交号和构建时间，版本接口不再返回 `dev`。

### 已知限制

- 登录页底图分辨率为 1672×941，不是 4K；替换 `public/xs-login-scene.jpg` 即可，无需改代码。
- 登录页「忘记密码」目前只提示联系平台管理员，尚无自助找回流程。
- 总览的 CPU、内存指标不做跨集群汇总。

---

## v1.20 — 2026-11-01

主题：**多集群统一接入：新增 agent 反向隧道，CI/CD 与监控统一使用集群注册表，并完善总览、Helm、组件中心和中间件**。

### 新增

- **Agent 接入集群**：成员集群部署 `matrix-cluster-agent` 后主动连出到平台，平台通过 WebSocket + yamux 隧道访问 kube-apiserver，平台不需要访问成员集群的 API Server。exec、日志跟随和 watch 都可以穿过隧道。
- **接入清单**：平台生成包含 Namespace、ServiceAccount、RBAC、令牌 Secret 和 Deployment 的接入清单，并可选同时创建 CI/CD 部署账号 `matrix-cicd-deployer`。重新生成清单会使旧令牌失效。
- **多副本转发**：agent 连接的归属记录在 PostgreSQL 中，其他副本经 HMAC 签名的内部请求转发给持有连接的副本。
- **集群改名**：集群列表新增「编辑」，可修改名称、描述和 kubeconfig。改名与数据源中集群名的同步在同一个事务中完成。
- **Helm 应用市场**：管理无认证 HTTPS 仓库、搜索 Chart，并把选中的 Chart 带入应用创建流程。
- **指标监控模板**：内置 CPU、内存、磁盘、Pod、重启、网络和 API 错误 7 个 PromQL 模板。
- **Git 仓库**：应用列表新增「接入 Git 仓库」和「编辑」入口。编辑时 Git Token 不回显，留空表示保留原值。

### 变更

- **CI/CD 执行集群**：改为引用集群管理中已注册的集群（`cluster_id`），新建时不再接收 kubeconfig。已有执行集群可以继续使用，也可以通过「关联集群」改为引用已注册集群。
- **集群访问统一**：CI/CD、监控健康巡检和自动修复都通过平台集群注册表获取连接，不再各自读取 kubeconfig，因此 Agent 接入的集群同样可用。
- **集群总览**：CPU、内存和 24 小时曲线读取当前集群的真实 Prometheus 数据；节点 TOP5 按 CPU 排序，并显示每个节点的 Pod 数；告警显示标题、位置和详情入口。
- **组件中心**：优先展示集群组件库存，组件目录默认折叠。
- **中间件**：简化创建表单，高级设置默认折叠；新增编辑入口；按名称、类型、环境和地址搜索。
- **搜索**：覆盖节点、构建、发布和中间件；输入时焦点和光标位置保持不变。

### 修复

- 切换集群时，旧请求的返回结果不会再覆盖新页面。
- 集群列表的「改环境」按钮始终被后端拒绝，已替换为「编辑」；集群环境在接入后固定。
- 中央数据源的告警可以识别自定义集群标签；与集群映射冲突时拒绝推断所属集群。
- 修正必填项显示两个星号的问题；RabbitMQ 地址提示补充 HTTPS。
- 总览在手机端改为上下排列，表格在容器内横向滚动。

### 升级说明

- 启动时自动执行数据库迁移：cluster `0005_agent_connect` 和 cicd `0006_target_cluster`。升级前建议备份数据库。
- 平台多副本部署时，需要通过 Downward API 注入 `MATRIX_POD_IP`；`deploy/k8s/xingshu-matrix-dev.yaml` 已包含此配置。
- 成员集群需要能拉取 `matrix-cluster-agent` 镜像。接入清单默认引用与平台版本相同 tag 的 agent 镜像，可在生成清单时改为其他镜像地址。
- 引用 kubeconfig 的旧执行集群可以继续使用，建议逐个关联到已注册集群。

### 已知限制

- 集群采集器套件（日志、指标、链路推送到中心存储）尚未实现，见 README 路线图。
- Helm 市场暂不支持仓库认证、OCI 仓库和任意 Chart 直接安装。

---

## v1.19 — 2026-10-27

主题：**围绕 Kubernetes 日常运维补齐资源管理、环境审批与交付能力，简化常用操作**。

### 新增

- **存储管理**：管理 StorageClass、PersistentVolume、PersistentVolumeClaim，并在集群具备快照 API 与驱动时管理卷快照。
- **弹性伸缩与定时任务**：管理 autoscaling/v2 HPA 和 batch/v1 CronJob，支持查看 Job 运行历史与手动触发任务。
- **快照恢复**：在快照能力可用时查看 VolumeSnapshot 状态，并从已就绪快照创建新 PVC。
- **网络资源**：提供 Service、Ingress 与 NetworkPolicy 的表单管理；Service 支持 ClusterIP、NodePort、LoadBalancer 与 ExternalName。
- **对外服务与拓扑**：对外服务聚焦 Ingress 和已安装的 APISIX 路由；拓扑按层级展示调用关系，并区分实际 trace 与 NetworkPolicy 声明。
- **配置管理**：提供 ConfigMap 与 Secret 管理。Secret 仅展示元数据和键名，不在页面回显密钥值。
- **节点维护**：按节点标签进行分组，支持批量维护标签和污点。
- **集群接入与分发**：支持 kubeconfig 导入、集群信息对比及跨集群逐项分发资源清单。
- **集群安装**：支持面向 Linux 主机的 K3s 安装任务；高可用场景要求预先准备 API 负载均衡地址。
- **应用交付**：增加 Helm 仓库目录、Argo CD 应用读取与固定版本同步，以及 Jenkins 参数发现。
- **交付工作台**：恢复应用工作区布局，流水线列表可进入运行阶段、日志和操作详情。
- **自动化与诊断**：增加平台容器内的 Ansible 执行能力、集群健康巡检、告警诊断建议和经审批的工作负载修复入口。
- **自然语言草稿**：在 Service、Ingress、存储与配置创建表单中生成待确认草稿，由用户检查后再提交。

### 修复与优化

- 统一迁移校验中的 SQL 换行处理，兼容 Windows CRLF 与 Linux LF 构建，避免内容未变的已应用迁移被误判为修改。
- 生产环境未配置审批策略时拒绝写操作，避免生产变更绕过审批。
- Service 与存储入口归入“工作负载”导航；系统设置不再展示重复的 AI 功能原始键值和公告配置行，AI 场景默认启用。
- 清理缺少后端持久化的证书管理入口，避免将页面本地状态误认为集群证书变更。
- 更新面向用户的产品说明和平台技术文档。

### 使用说明

- Service 拓扑仅根据已检索到的链路数据展示观测到的调用关系；NetworkPolicy 图展示策略声明，不等同于 CNI 执行结果。
- 集群间资源分发逐集群执行，失败时可能出现部分成功；需人工核对各集群结果。
- 外部 Jenkins、Argo CD、Helm 仓库及 AI 服务需在平台中配置可用端点与凭据后使用。
- K3s 高可用安装依赖预先可用的负载均衡地址；平台不会自动创建云负载均衡或 VIP。

---

## v1.18 — 2026-10-24

主题：**真实环境筛选与可观测体验补强，补齐应用清单和集群网络只读接口**。

### 新增

- **集群网络资源接口**：新增 `GET /clusters/:id/network/services` 与
  `GET /clusters/:id/network/policies`，供实时网络视图读取 Kubernetes Service 和 NetworkPolicy。
- **应用运行清单接口**：新增 `GET /cicd/applications/:id/inventory`，汇总已配置环境中的工作负载、
  Pod、容器与匹配 Service，并返回采集警告。
- **可观测体验**：新增数据归属、指标查询、日志归一化、链路与告警资源定位工具；
  支持集群范围的链路/告警详情、通知渠道配置和应用级告警筛选。Jaeger trace 归一化纳入 process resource tags，
  并按 OpenTelemetry 错误状态识别错误 span。
- **拓扑数据**：流量拓扑基于当前集群 Service 与真实检索到的 trace 样本构建；
  网络策略视图读取 Kubernetes NetworkPolicy 声明。无观测数据时显示空态，不虚构调用关系。
- **环境与实时状态**：统一环境别名规范化；顶栏集群数量使用真实集群清单，区分加载中、失败与真实空列表；
  总览按所选环境加载集群、节点、Pod、告警及历史指标。

### 修复

- 修复共享 Jaeger/Tempo 数据源详情缺少 process 级 `k8s.cluster.name` 标签的问题，并补充 OTEL 错误状态识别。
- 修复环境菜单与总览将尚未加载的集群清单显示成零，以及环境筛选口径不一致的问题。
- live 模式明确标注未接后端或未接真实数据的页面，避免将本地样例显示成真实运行状态。

### 测试

- 新增 `tests/frontend/xs-observability.test.mjs`，覆盖数据源归属、查询转义、日志归一化、
  trace span 层级、告警资源定位与应用告警范围。
- 本轮执行的前端 API 与可观测 Node 测试共 14 条通过；三个新增/修改前端脚本语法检查通过。
- 使用工作区隔离的 Go 模块/构建缓存执行 `go build ./...`、`go vet ./...`、`go test ./...`，均通过；没有测试的包报告 `[no test files]`。

### 文档

- 更新 README、平台说明、逐页进度与未完成清单，记录 v1.18 的功能、验证结果和仍待处理事项。
- 补充应用清单、集群网络接口及可观测集群归属说明。
- 发布说明同步至 [GitHub Release v1.18](https://github.com/yangguangdijia/xingshu-matrix/releases/tag/v1.18)。

### 发布状态

- 代码已推送 GitHub `beta`，`v1.18` 标签及 GitHub Release 已发布并说明本次功能与验证结果。
- 本次发布不包含镜像构建、部署或真实集群验收；历史镜像和部署结果不代表此标签的运行状态。

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
