# 堡垒机模块（bastion）API

代码：`server/internal/modules/bastion/`。所有接口挂载在已认证的 `/api/v1` 组下（JWT + 强制改密 + 审计中间件已生效），统一响应 `{code,message,data,request_id}`。

## 权限

| 权限码 | 说明 |
|---|---|
| `bastion:read` | 查看资产、系统账号（密文脱敏）、授权规则、命令过滤规则、guacd 健康 |
| `bastion:manage` | 增删改资产 / 系统账号 / 授权规则 / 命令过滤规则 |
| `bastion:connect` | 查看"我的可连接资产"，发起 SSH Web 终端 |
| `bastion:audit` | 会话列表/详情、命令审计、录像下载 |
| `bastion:kill` | 强制断开在线会话 |

> 连接还需同时满足：①存在**有效的授权规则**（未禁用、未过期，主体为当前用户 ID 或其角色之一）；②`Principal.CanEnv(asset.env)` 通过。`bastion:manage` **不**隐含连接权，管理员也需要授权规则。

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `MATRIX_RECORD_DIR` | `./data/recordings` | asciicast v2 录像本地目录（Start 时自动创建） |
| `MATRIX_GUACD_ADDR` | `127.0.0.1:4822` | guacd 地址，仅用于健康检查 |

## 数据表（迁移 `0001_init.up.sql`、`0002_asset_options.up.sql`）

`bastion_assets`（含 `options JSONB`）、`bastion_accounts`（`secret_enc`/`passphrase_enc` 用 `d.Crypto` AES-256-GCM 加密）、`bastion_rules`、`bastion_filters`、`bastion_sessions`、`bastion_commands`。

`0002_asset_options.up.sql` 用 `ADD COLUMN IF NOT EXISTS` 增加 `options JSONB NOT NULL DEFAULT '{}'::jsonb`，对已有数据向后兼容：老行读出 `{}`，写回时整对象覆盖。

---

## 资产

### GET /bastion/assets — `bastion:read`
查询参数：`q`（名称/主机模糊）、`protocol`、`env`、`page`、`size`。
```json
{"items":[{"id":1,"name":"web-01","host":"10.0.0.11","port":22,"protocol":"ssh","env":"prod","description":"","host_key":"ssh-ed25519 AAAA...","options":{"group":"Web 集群","labels":"team=infra","source":"手动","sftp":true},"created_at":"...","updated_at":"..."}],"total":1,"page":1,"size":20}
```

### GET /bastion/assets/:id — `bastion:read`

### POST /bastion/assets — `bastion:manage`
```json
{"name":"web-01","host":"10.0.0.11","port":22,"protocol":"ssh","env":"prod","description":"Web 集群 · team=infra","host_key":"","options":{"group":"Web 集群","labels":"team=infra","source":"手动","gateway":"bastion.internal:22","sftp":true}}
```
`protocol` ∈ `ssh|rdp|vnc|mysql|postgres`；`env` 默认 `dev`。`host_key` 为 authorized_keys 格式；留空则**首次连接时信任并保存**（TOFU），之后不一致即拒绝连接（防中间人）。返回 201 + 资产对象。

`options` 规则（`assetIn.check`）：

| 规则 | 行为 |
|---|---|
| 缺省或 `null` | 归一化为 `{}`，不会写成 SQL NULL |
| 序列化后大于 4096 字节 | 400 `options is too large (max 4096 bytes)` |
| 键名（大小写不敏感）命中 `secret` / `password` / `passphrase` / `private_key` / `privatekey` / `token` / `kubeconfig` / `credential` | 400 `options must not contain credentials: <key>` |

常见协议字段（全部为非敏感配置）：`group`、`labels`、`source`、`gateway`、`sftp`、`domain`、`security`、`res`、`depth`、`readonly`、`database`、`tls`。凭据只能通过 `POST /bastion/accounts` 的 `secret` 字段提交，由后端加密写入 `secret_enc`。

### 资产来源（`options.source`）

| 来源 | 本轮状态 |
|---|---|
| `手动` | 前端登记表单真实调用 `POST /bastion/assets` + `POST /bastion/accounts` |
| `Agent 自动发现` | 需要 matrix-agent 上报接口，**本轮未联调**；登记时按手动写入并把来源记录在 `options.source` |
| `K8s 同步` | 需要 Kubernetes API 同步，**本轮未联调** |
| `云同步` | 需要云厂商 API，**本轮未联调** |
| `批量导入` | CSV 导入接口**本轮未实现** |

来源只影响登记入口与展示，不改变资产表结构：后端始终按手动路径写入 `bastion_assets`。

### PUT /bastion/assets/:id — `bastion:manage`（请求体同 POST，全量更新）
### DELETE /bastion/assets/:id — `bastion:manage`（级联删除其系统账号）

## 系统账号（凭据）

### GET /bastion/accounts?asset_id=1 — `bastion:read`
```json
[{"id":3,"asset_id":1,"name":"root-key","username":"root","auth_type":"private_key","secret":"****","created_at":"...","updated_at":"..."}]
```
`secret` 只返回 `crypto.Mask` 后的值，明文永不出接口。

### POST /bastion/accounts — `bastion:manage`
```json
{"asset_id":1,"name":"root-key","username":"root","auth_type":"private_key","secret":"-----BEGIN OPENSSH PRIVATE KEY-----\n...","passphrase":""}
```
`auth_type` ∈ `password|private_key`。私钥在入库前用 `ssh.ParsePrivateKey(WithPassphrase)` 校验。

### PUT /bastion/accounts/:id — `bastion:manage`
同上；`secret` 留空表示保留原凭据（及原 passphrase）。

### DELETE /bastion/accounts/:id — `bastion:manage`

## 授权规则

### GET /bastion/rules — `bastion:read`
### POST /bastion/rules — `bastion:manage`
```json
{"name":"ops 访问 prod web","subject_type":"role","subject":"ops","asset_ids":[1,2],"account_ids":[],"expires_at":"2026-12-31T23:59:59+08:00","enabled":true}
```
- `subject_type=user` 时 `subject` 为用户 ID（字符串）；`role` 时为角色名，与 `Principal.Roles` 中的值比对。
- `account_ids` 为空 = 这些资产下的全部账号。`expires_at` 为空 = 永久。
### PUT /bastion/rules/:id、DELETE /bastion/rules/:id — `bastion:manage`

### GET /bastion/my/assets — `bastion:connect`
返回当前用户经规则授权、且环境在其范围内的资产/账号组合：
```json
[{"asset_id":1,"asset_name":"web-01","host":"10.0.0.11","port":22,"protocol":"ssh","env":"prod","account_id":3,"account_name":"root-key","username":"root"}]
```

## 命令过滤规则

### GET /bastion/filters — `bastion:read`（按 priority 升序，首条命中生效）
### POST /bastion/filters — `bastion:manage`
```json
{"name":"禁止删根","pattern":"rm\\s+-rf\\s+/","action":"deny","priority":10,"enabled":true}
```
`action`：`deny` 直接拦截；`confirm` 需用户在终端输入 `yes` 确认后才执行。正则用 Go RE2 语法，保存前校验。
### PUT /bastion/filters/:id、DELETE /bastion/filters/:id — `bastion:manage`

---

## SSH Web 终端

### GET /bastion/sessions/ssh/ws?asset=1&account=3&cols=120&rows=32&access_token=<JWT> — `bastion:connect`

WebSocket（gorilla/websocket）。

**认证**：路由挂在已认证组上，核心 auth 中间件接受 `Authorization: Bearer <token>` 或查询参数 **`access_token=`**（浏览器 WebSocket 无法设置请求头，前端请用 `access_token`）。
注意：简报中设想的 `?token=` 以及 `Sec-WebSocket-Protocol` 传 token **未实现**——`d.Public` 上的中间件不会作用于已认证组的路由，且模块不可修改核心 auth；如需支持须在核心 `auth.Middleware` 增加读取逻辑。服务端声明子协议 `xterm`（客户端可选传）。

**消息协议（JSON 文本帧）**
```json
// 客户端 → 服务端
{"type":"input","data":"ls -l\r"}
{"type":"resize","cols":160,"rows":40}
// 服务端 → 客户端
{"type":"output","data":"total 8\r\n..."}
{"type":"closed","data":"session ended | killed | <错误>"}
```

**执行流程**
1. 校验资产存在且 `protocol=ssh`、环境范围、账号属于该资产、授权规则有效；解密凭据；加载启用的过滤规则。
2. 升级 WebSocket，写入 `bastion_sessions`（status=active）。
3. `golang.org/x/crypto/ssh` 拨号（10s 超时，host key TOFU 校验），申请 `xterm-256color` PTY 并启动 shell。
4. 录像：`$MATRIX_RECORD_DIR/<sessionID>-<时间>.cast`，asciicast v2（header + `[t,"o",data]` 输出事件 + `[t,"r","COLSxROWS"]` 尺寸事件；拦截提示也写入录像）。可用 asciinema-player 回放。
5. 命令过滤与审计：键入字符实时转发（保持远端回显/readline 体验），本地同步维护行缓冲；收到回车时对该行做正则匹配：
   - 未命中：转发回车，记录 `allow`；
   - `deny`：向远端发送 `Ctrl-U` 清空该行 + 回车（不执行），终端红字提示，记录 `deny`；
   - `confirm`：清空远端行，终端提示"执行? (yes/no)"，输入 `y/yes` 则重放命令并记录 `confirmed`，否则 `cancelled`。
   - 若该行经过方向键/历史/Tab/Ctrl 组合编辑，本地缓冲不可靠，则改用远端回显的当前屏幕行（去 ANSI，含提示符）做匹配与审计；此时 `confirm` 类规则无法安全重放，直接按拦截处理并提示重新完整输入。
   - 每条命令写入 `bastion_commands(session_id, command, action, rule_id)`。
6. 每 30s 发送 WebSocket ping；任一端断开或被强制断开后，更新会话 `status`（closed / killed / failed）、`ended_at`、`recording_path`。

## 会话审计

### GET /bastion/sessions — `bastion:audit`
参数：`status`（active|closed|killed|failed）、`username`、`asset_id`、分页。
```json
{"items":[{"id":12,"user_id":1,"username":"admin","asset_id":1,"asset_name":"web-01","account_id":3,"account_user":"root","protocol":"ssh","client_ip":"10.1.2.3","status":"closed","error":"","started_at":"...","ended_at":"...","has_recording":true}],"total":1,"page":1,"size":20}
```

### GET /bastion/sessions/:id — `bastion:audit`
```json
{"session":{...},"commands":[{"id":1,"session_id":12,"command":"ls -l","action":"allow","rule_id":null,"created_at":"..."}]}
```
（命令最多返回前 500 条，完整列表用下一个接口分页。）

### GET /bastion/sessions/:id/commands — `bastion:audit`（分页）

### GET /bastion/sessions/:id/recording — `bastion:audit`
下载 `.cast` 文件（`Content-Type: application/x-asciicast`，附件）。

### POST /bastion/sessions/:id/kill — `bastion:kill`
取消会话 context，关闭 SSH 与 WebSocket，终端提示"已被管理员强制断开"，状态记为 `killed`。
```json
{"id":12,"killed":true}
```
会话不在本实例内存中时返回 404。

## RDP / VNC

### GET /bastion/guacd/health — `bastion:read`
对 `MATRIX_GUACD_ADDR` 做 TCP 连通性探测（3s 超时）：
```json
{"addr":"127.0.0.1:4822","reachable":true,"latency_ms":1}
```

**guacd 集成计划（未实现）**：
1. 部署 `guacamole/guacd`（4822）。新增 `GET /bastion/sessions/guac/ws?asset=&account=`，鉴权/授权流程与 SSH 相同。
2. 服务端以 Guacamole 协议与 guacd 握手：`select,rdp|vnc` → 读取 `args` → 回传 `size/audio/video/image` 与 `connect`（hostname、port、username、password 由解密后的系统账号填入，凭据不下发浏览器）。
3. 握手完成后在 WebSocket（子协议 `guacamole`）与 guacd TCP 之间透明转发指令流，前端使用 `guacamole-common-js`。
4. 录像：在 connect 参数中设置 `recording-path`/`recording-name`（guacd 原生 `.guac` 录像，可用 `guacenc` 转 mp4）；session 表复用。
5. mysql/postgres 资产：计划通过协议代理或临时凭据下发实现，当前仅可登记资产。

## 未实现 / 限制

- RDP、VNC、MySQL、PostgreSQL 的实际连接代理（仅资产登记 + guacd 健康检查）。
- 录像上传 S3/MinIO：当前只写本地目录。计划：会话结束后用 minio-go `PutObject` 上传到 `recordings/<id>.cast`，`recording_path` 改存对象键，下载接口改为预签名 URL 重定向；多实例部署时必须启用，否则录像只存在于产生它的实例上。
- 强制断开、在线会话注册表为**单实例内存**；多实例需通过 Redis pub/sub 广播 kill。
- 命令识别基于输入行缓冲 + 回显屏幕行启发式：`vim` 等全屏程序内的按键、`sudo` 密码输入等也可能被当作"命令"记录；多行粘贴、shell 别名/变量展开、`bash -c` 嵌套可能绕过正则——过滤规则是辅助控制，不能替代最小权限账号。
- 生产环境连接未接入审批（`d.Approval.Gate`）；可后续增加"连接申请"工单类型。
- 未提供 SFTP 文件传输、会话实时监看（旁观）。
- `?token=` / `Sec-WebSocket-Protocol` 传 token 未实现，见上文，请用 `access_token`。

## 前端连接器状态（`public/index.html`）

访问工作台（`#/bastion/term` 无会话时）与资产页都按下列探测结果显示真实状态，未连接时不建立会话、不写审计：

| 探测 | 接口 | 状态判定 |
|---|---|---|
| SSH / SFTP 通道 | `GET /bastion/my/assets` | 200 → `可用`；401/403 → `未授权`；其他 → `未连接` |
| RDP / VNC | `GET /bastion/guacd/health` | `reachable:true` → `可用`；`false` → `未配置`；请求失败 → `无法确认` |
| SQL 代理 | 无 | 固定 `未配置`（未实现） |
| 静态预览（未接入 `/api/v1`） | 不探测 | 固定 `未连接` |

SSH 会话使用 `XS.ws("/bastion/sessions/ssh/ws", {asset, account, cols, rows})`，消息契约为：

| 方向 | 消息 |
|---|---|
| 浏览器 → 服务端 | `{"type":"input","data":"ls"}`、`{"type":"resize","cols":120,"rows":32}` |
| 服务端 → 浏览器 | `{"type":"output","data":"..."}`、`{"type":"error","data":"..."}`、`{"type":"closed","data":"killed\|session ended"}` |

前端用轻量屏幕模型渲染（剥离 ANSI、处理 `` 覆盖 / `` 退格 / 整屏清除），不引入 xterm 运行时依赖；颜色与光标寻址不还原。

## 本轮验证边界（重要）

以下项目**本轮未验证**，文档与界面都不得声称已联调：

- 真实 PostgreSQL 上的 `0002_asset_options` 迁移、资产/授权/审计 SQL 查询；
- 真实 SSH 目标主机的连接、TOFU 主机指纹校验与命令过滤；
- 真实 guacd 的 RDP / VNC 代理；
- Prometheus、日志、链路、Jenkins、Kafka 与真实 SSO。

已在本轮验证：`go test ./...`、`go vet ./...`、前端脚本解析、`git diff --check`、静态预览下的登录/改密/资产表单/终端空态交互。

界面约束：未接入后端时，资产与审计页显示"静态示例数据"提示；`live` 模式下未接入后端的子页（账号、授权策略、命令过滤、审计、脱敏、数据库）同样标注，避免把演示数据当成生产状态。
