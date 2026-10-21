# 堡垒机产品模式与实现边界

本文记录星枢 Matrix 堡垒机本轮（登录页 + 访问工作台 + 协议感知资产）采用的产品模式、公开参考做法，以及与后端能力之间的真实边界。**任何界面文案都不得越过这些边界宣告"生产可用"。**

## 1. 目标与范围

本轮目标是把堡垒机从"演示页面"改造为"访问工作台"：

- 没有会话时，用户能直接看到**哪些协议真的能连**、**哪些资产已授权**、**最近发生了什么会话**；
- 资产登记按协议展示对应字段，凭据只提交后端，不进入浏览器持久化；
- 未接入后端（静态预览）或缺少连接器时，界面如实显示"未连接 / 未配置 / 未授权"，不渲染虚假成功会话。

非目标：RDP/VNC 真实代理、SFTP 文件传输、SQL 协议代理、多实例 kill 广播、真实 SSO 与外部环境联调。

## 2. 资产来源模式

| 来源 | 触发方式 | 本轮落地 |
|---|---|---|
| 手动 | 登记表单 | 真实写入 `POST /bastion/assets` + `POST /bastion/accounts` |
| Agent 自动发现 | matrix-agent 上报 | 未联调，登记时按手动写入并记录 `options.source` |
| K8s 同步 | Kubernetes API | 未联调 |
| 云同步 | 云厂商 API | 未联调 |
| 批量导入 | CSV | 未实现 |

模式要点：

1. **来源是标签，不是第二条写路径**。所有资产最终都落到 `bastion_assets`，避免多套写入逻辑产生字段漂移。
2. **来源不影响授权模型**。授权规则只看资产 ID / 账号 ID / 环境范围，不看来源。
3. **外部来源必须在 UI 上标注未联调**，不能让用户以为"K8s 同步"已经在工作。

公开产品对照：JumpServer 把资产来源拆为"手动 / 自动发现 / 云同步 / 网域"并统一进入资产库；Teleport 用 `discovery` 服务把云与 K8s 资源注册为统一 resource。二者都遵循"统一资源模型 + 独立发现器"的模式。

## 3. 协议感知表单

同一份"登记资产"表单按协议切换字段，宿主字段保持稳定：

| 协议 | 默认端口 | 专属字段 |
|---|---|---|
| SSH | 22 | 主机指纹（TOFU）、跳转网关、SFTP 开关 |
| RDP | 3389 | 域、安全层（NLA/TLS/RDP）、分辨率 |
| VNC | 5900 | 色彩深度、只读查看 |
| MySQL | 3306 | 数据库、TLS、只读会话 |
| PostgreSQL | 5432 | 数据库、TLS、只读会话 |

宿主字段：名称、主机 / IP、端口、环境、资产组、标签、来源、登录用户名、认证方式、凭据。

专属字段写入 `bastion_assets.options`（JSONB），键名禁止命中凭据关键字（`secret` / `password` / `passphrase` / `private_key` / `token` / `kubeconfig` / `credential`），由 `assetIn.check` 在服务端拒绝。

公开产品对照：Guacamole 的 connection 参数按协议分组（`rdp-*` / `vnc-*` / `ssh-*`）；JumpServer 的资产平台类型决定"协议 → 端口 → 凭据"三元组。两者都是"宿主字段固定 + 协议参数可扩展"。

## 4. 授权与凭据边界

- **权限码**：`bastion:read` / `manage` / `connect` / `audit` / `kill`，互不隐含。`bastion:manage` 不隐含连接权。
- **连接三重条件**：有效授权规则（未禁用/未过期，主体为用户或其角色）+ `CanEnv(asset.env)` + 账号属于该资产。
- **凭据流向**：浏览器 → `POST /bastion/accounts` 的 `secret` → 后端 AES-256-GCM 加密进 `secret_enc` → SSH 客户端解密使用。列表接口只返回 `crypto.Mask` 结果。
- **浏览器禁止持久化**：密码、私钥、passphrase、kubeconfig 不写 `localStorage` / `sessionStorage`；`options` 只存非敏感配置。
- **无授权即不建会话**：前端在 `GET /bastion/my/assets` 里找不到授权条目时直接提示，不发起 WebSocket，因此不会产生 `bastion_sessions` 记录。

公开产品对照：Teleport 用 RBAC + 短期证书签发实现"授权即凭据"，凭据不长期落地；JumpServer 用"系统用户 + 授权规则"分离凭据与访问权。本项目采用后者（长期托管凭据 + 规则授权），因此**凭据托管侧必须依赖后端加密与审计**。

## 5. SSH：已实现

| 能力 | 状态 | 证据 |
|---|---|---|
| WebSocket 终端 | 已实现 | `GET /bastion/sessions/ssh/ws`，JSON `input/resize` ↔ `output/error/closed` |
| PTY 与终端尺寸 | 已实现 | `RequestPty("xterm-256color", rows, cols)` + `resize` 调 `WindowChange` |
| 主机指纹 TOFU | 已实现 | `hostKeyCallback` 首次写入 `host_key`，之后不一致拒绝 |
| 命令过滤 | 已实现 | `InputProcessor` 产出 `deny/confirm`，写入 `bastion_commands` |
| 录像 | 已实现 | asciicast v2 写入 `MATRIX_RECORD_DIR`，`has_recording` 可下载 |
| 强制断开 | 已实现（单实例） | `POST /sessions/:id/kill` → `cancel()`，内存注册表 |
| 前端终端 | 已实现（轻量） | 屏幕模型 + 逐键发送，不引入 xterm 依赖 |

**未验证**：真实 SSH 目标机、真实 PostgreSQL、真实命令过滤规则命中、多实例 kill。

## 6. RDP / VNC：未实现

后端只有 `GET /bastion/guacd/health` 的 TCP 探活，没有 `guac/ws` 转发端点。计划中的做法：

1. 部署 `guacamole/guacd`（4822），新增 `GET /bastion/sessions/guac/ws?asset=&account=`，鉴权与 SSH 相同；
2. 服务端以 Guacamole 协议握手：`select,rdp|vnc` → 读 `args` → 回传 `size/audio/video/image` 与 `connect`；
3. 握手后在 WebSocket（子协议 `guacamole`）与 guacd TCP 之间透明转发；
4. 录像使用 guacd 原生 `recording-path`，用 `guacenc` 转 mp4。

在这些完成之前，前端 `BAS_CONN()` 对 RDP/VNC 返回 `guacd.reachable`，且 `basNoConnector()` 明确写出"需要 guacd 协议代理"与后端路径。**不得**用前端 mock 桌面冒充远程会话。

## 7. 连接器未配置状态

状态机（`BAS_CONN_STATE`）：

| 状态 | 含义 | 是否允许建会话 |
|---|---|---|
| 可用 | 后端接口 200 且通道就绪 | 是 |
| 未配置 | 组件未部署（guacd 不可达、SQL 代理未实现） | 否 |
| 未授权 | 401/403，缺少权限或授权规则 | 否 |
| 未连接 | 后端不可达、静态预览 | 否 |
| 无法确认 | 探测请求本身失败 | 否 |

界面表现：访问工作台的连接器卡片、资产页底部状态行、`basNoConnector()` 弹窗三处共用同一份状态，避免出现"某处说已连接、某处说未配置"的矛盾。

## 8. 设计检查清单

新增堡垒机页面或协议时逐项确认：

- [ ] 状态来自真实接口，而不是常量或演示数组；
- [ ] 未连接时不创建会话、不写审计、不显示"成功"；
- [ ] 凭据字段不出现在 `options`、URL、`localStorage`、日志；
- [ ] 新协议字段先加 `options` 白名单说明，再加 UI；
- [ ] 无权限路径走 403 + 明确文案，而不是静默空表；
- [ ] 前端静态预览与 live 模式视觉上可区分（标签 / 提示条）；
- [ ] 文档同步更新"未实现 / 未验证"清单。

## 9. 参考产品（公开信息）

| 产品 | 可借鉴的模式 |
|---|---|
| JumpServer | 资产库 + 系统用户 + 授权规则三层分离；资产来源作为发现器标签 |
| Teleport | 统一资源模型（server / k8s / db / app）+ 短时凭据 + 强审计 |
| Apache Guacamole | 无客户端远程桌面网关；按协议分组的连接参数与 `guacd` 握手 |
| 通用堡垒机 | 会话录像、命令过滤（deny/confirm）、强制断开、会话审计 |

以上只作为模式参考，不代表本项目已实现同等能力。

## 10. 本轮未验证清单

- 真实 PostgreSQL：`0002_asset_options.up.sql` 迁移、资产/账号/授权/审计 SQL；
- 真实 SSH：拨号、TOFU、PTY 尺寸、命令过滤命中、录像落盘；
- 真实 guacd：RDP / VNC 代理；
- Kubernetes、Prometheus、日志、链路、Jenkins、Kafka、真实 SSO；
- 多实例部署下的强制断开与录像下载。
