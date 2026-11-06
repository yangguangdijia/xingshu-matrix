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

> 连接（SSH 终端与数据库控制台）还需同时满足：①存在**有效的资产授权**（未禁用、已到生效时间、未过期，动作含 `connect`，主体为当前用户 ID、其角色编码之一或其所在用户组；资产直接授权或位于所授权节点及其子节点下）；②`Principal.CanEnv(asset.env)` 通过；③**登录 ACL** 未拒绝。`bastion:manage` **不**隐含连接权，管理员也需要授权。
>
> 审批：命令复核（`bastion.command`）与 SQL 工单（`bastion.sql`）走平台审批流 `/approvals`，审批人需要 `approval:approve`，不能审批自己发起的单。

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `MATRIX_RECORD_DIR` | `./data/recordings` | asciicast v2 录像本地目录（Start 时自动创建） |
| `MATRIX_GUACD_ADDR` | `127.0.0.1:4822` | guacd 地址，仅用于健康检查 |
| `MATRIX_BASTION_RECORDING_STORAGE_ID` | （空） | 录像卷标识，必须与录像目录下 `.matrix-bastion-storage-id` 文件内容一致；未配置时拒绝 SSH 连接（不录像就不允许连接） |
| `MATRIX_BASTION_RECORDING_MODE` | （空） | 设为 `local` 时允许录像目录不在 NFS 上，**仅限单副本部署**；多副本必须使用共享 NFS，否则会话被其它副本接管后录像不完整。目录不能是组 / 全局可写 |

## 数据表（迁移 `0001_init.up.sql` … `0006_jumpserver_layout.up.sql`）

`bastion_assets`（含 `options JSONB`）、`bastion_accounts`（`secret_enc`/`passphrase_enc` 用 `d.Crypto` AES-256-GCM 加密）、`bastion_rules`、`bastion_filters`、`bastion_sessions`、`bastion_commands`。

`0005_approve_and_rotation.up.sql`：命令过滤动作增加 `approve`；`bastion_accounts.pending_secret_enc / password_changed_at`；`bastion_rotation_plans`、`bastion_rotation_runs`。

`0006_jumpserver_layout.up.sql`（全部增量）：`bastion_nodes`（资产树）、`bastion_platforms`（内置 Linux / Unix / Windows / MySQL / PostgreSQL）、`bastion_assets.node_id / platform_id`（已有 SSH 资产回填为 Linux 平台）、`bastion_accounts.verify_status / verify_message / verified_at`、`bastion_rules.node_ids / actions / starts_at` 与主体类型 `group`、`bastion_filters.group_name / asset_ids` 与动作 `allow`、`bastion_login_acls`、`bastion_login_logs`、`bastion_db_queries`。

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
`protocol` ∈ `ssh|mysql|postgres`（其余协议没有连接器，创建时 400）；`env` 默认 `dev`。可选 `node_id`（资产树节点）、`platform_id`（平台）。数据库资产的 `options` 可含 `database`（默认库）、`sslmode`（PostgreSQL：`disable|prefer|require|verify-full`）、`tls`（MySQL：`preferred|true|skip-verify`）。列表接口支持 `node_id`（包含其全部子节点）。`host_key` 为 authorized_keys 格式；留空则**首次连接时信任并保存**（TOFU），之后不一致即拒绝连接（防中间人）。返回 201 + 资产对象。

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

### POST /bastion/assets/:id/test — `bastion:manage`（v1.26）
从平台所在网络做真实连通性探测：TCP 建连（6 秒超时）→ 读取 SSH 横幅 → 给出 `account_id` 时用托管凭据做一次登录验证（不打开 Shell，首次连接按 TOFU 记录主机指纹）。
```json
// 请求（可选）
{"account_id": 3}
// 响应
{"asset_id":1,"address":"10.0.1.11:22","reachable":true,"latency_ms":2,"banner":"SSH-2.0-OpenSSH_9.6","auth":true}
// 失败示例
{"asset_id":2,"address":"10.0.1.12:22","reachable":false,"error":"端口拒绝连接：服务未启动或端口错误"}
```
`error` 为中文原因：连接超时、端口拒绝、域名无法解析、登录失败、主机指纹不一致、凭据无法解密。
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

v1.27 起（资产授权，对应 JumpServer「资产授权」）请求体增加字段：

| 字段 | 说明 |
|---|---|
| `subject_type` | `user`（用户 ID）/ `role`（角色编码）/ `group`（用户组 ID，按 `user_group_members` 匹配） |
| `asset_ids` / `node_ids` | 至少一项非空；节点授权包含其全部子节点下的资产 |
| `actions` | `connect` `upload` `download` `copy` `paste` 的子集，缺省为全部。`connect` 由后端强制；`copy/paste` 由 Web 终端前端执行（服务端无法区分粘贴与键入）；`upload/download` 需要文件传输通道，未实现，仅登记 |
| `starts_at` / `expires_at` | 生效 / 失效时间（RFC3339，可空）；失效必须晚于生效 |

错误：`select at least one asset or node`、`unknown action: x`、`expires_at must be later than starts_at`（均为 400 / 40001）。

### GET /bastion/my/assets — `bastion:connect`
返回当前用户可连接的 **SSH** 资产账号（多条授权覆盖同一资产账号时合并动作、取最晚到期，只返回含 `connect` 的组合）：
```json
[{"asset_id":1,"asset_name":"web-01","host":"10.0.0.11","port":22,"protocol":"ssh","env":"prod","account_id":3,"account_name":"root-key","username":"root","actions":["connect","copy","paste"],"expires_at":null}]
```

### GET /bastion/my/databases — `bastion:connect`
同上，返回 MySQL / PostgreSQL 资产账号。

## 命令过滤规则

### GET /bastion/filters — `bastion:read`（按 priority 升序，首条命中生效）
### POST /bastion/filters — `bastion:manage`
```json
{"name":"禁止删根","pattern":"rm\\s+-rf\\s+/","action":"deny","priority":10,"enabled":true}
```
`action`：`deny` 直接拦截；`confirm` 需用户在终端输入 `yes` 确认后才执行；`approve` 命令暂存并提交审批单（见「命令复核」），审批通过后发送到主机；`allow` 白名单，命中即放行且不再匹配后续规则。正则用 Go RE2 语法，保存前校验，不能匹配空命令。

可选字段：`group_name`（规则组，默认「默认规则组」，只用于归类，匹配顺序只看 `priority`）、`asset_ids`（作用资产，空 = 全部资产；会话建立时按资产加载规则）。`priority` 取值 1–100000。
### PUT /bastion/filters/:id、DELETE /bastion/filters/:id — `bastion:manage`

## 数据脱敏规则（v1.26）

SSH 会话输出在**发送到浏览器、写入录像之前**按规则做正则替换，运维人员与录像回放都看不到原文。规则在新建立的会话中生效；任一启用的规则表达式非法时拒绝新连接（与命令过滤一致）。

### GET /bastion/mask-rules — `bastion:read`
### POST /bastion/mask-rules — `bastion:manage`
```json
{"name":"手机号脱敏","pattern":"1[3-9]\\d{9}","replacement":"1**********","enabled":true}
```
`pattern` 为 Go RE2 正则，不能匹配空字符串，最长 512；`replacement` 默认 `******`，最长 64，按字面替换。
### PUT /bastion/mask-rules/:id、DELETE /bastion/mask-rules/:id — `bastion:manage`
### POST /bastion/mask-rules/preview — `bastion:read`
```json
// 用给定规则预览；不带 pattern 时用全部已启用规则
{"sample":"call 13812345678 now","pattern":"1[3-9]\\d{9}","replacement":"1**********"}
// → {"output":"call 1********** now"}
```
限制：匹配在单个输出块（最大 16 KB）内进行，不缓冲输出以免拖慢交互回显；极少数被网络拆分到两个块之间的长串可能漏掉。命令审计记录的是用户输入原文，不做脱敏。

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
{"type":"session","data":"42"}
{"type":"output","data":"total 8\r\n..."}
{"type":"approval","data":"{\"state\":\"pending\",\"ticket_id\":17,\"command\":\"systemctl restart nginx\",\"rule\":\"重启服务需审批\",\"deadline\":\"2026-11-05T14:10:00Z\"}"}
{"type":"closed","data":"session ended | killed | <错误>"}
```

`approval` 消息（v1.27）：命令命中 `approve` 规则时推送审批状态，`state` ∈ `submitting|pending|approved|rejected|cancelled|timeout|error`，同时以终端黄/红字输出同样的提示（旧前端也能看到）。等待期间终端只接受 Ctrl+C（放弃并撤回审批单）；超时（默认 10 分钟）自动撤回。

建立连接前还会校验登录 ACL；授权缺失、ACL 拒绝、凭据不可用、网络或 SSH 握手失败、成功连接都写入 `bastion_login_logs`。

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

## 资产树节点（v1.27）

### GET /bastion/nodes — `bastion:read`
`[{"id":1,"parent_id":null,"name":"生产","created_at":"...","asset_count":3}]`（`asset_count` 为直接挂在该节点下的资产数）。

### POST /bastion/nodes、PUT /bastion/nodes/:id — `bastion:manage`
`{"name":"支付","parent_id":1}`。名称 1–64 字符、不含 `/`，同一父节点下唯一（409）；不能移动到自身或子孙节点下（400）。

### DELETE /bastion/nodes/:id — `bastion:manage`
节点下还有子节点或资产时 409，`details` 给出 `children` / `assets` 数量。

## 平台（v1.27）

### GET /bastion/platforms — `bastion:read`
`[{"id":1,"name":"Linux","category":"host","type":"linux","protocol":"ssh","port":22,"charset":"utf-8","description":"...","builtin":true,"asset_count":3,"connectable":true}]`。`connectable=false`（如 Windows / RDP）表示没有连接器，只能登记。

### POST /bastion/platforms、PUT /bastion/platforms/:id — `bastion:manage`
`{"name":"CentOS 7","type":"linux|unix|windows|mysql|postgresql","port":22,"charset":"utf-8","description":""}`，`category`/`protocol` 由 `type` 推出。内置平台只能改端口、字符集、说明。

### DELETE /bastion/platforms/:id — `bastion:manage`
内置平台、仍被资产使用的平台返回 409。

## 账号连通性验证（v1.27）

### POST /bastion/accounts/:id/verify — `bastion:manage`
用托管凭据真实登录一次：SSH 完成握手与认证后立即断开（不开 Shell，按 TOFU 校验主机指纹）；数据库执行 `SELECT 1`。结果写回账号 `verify_status`（`ok|failed|unknown`）/ `verify_message` / `verified_at`，并返回 `{"id":3,"status":"ok","message":"登录成功","latency_ms":38}`。受环境范围限制（403）。

## 改密计划（v1.27，`rotation.go`）

| 接口 | 权限 | 说明 |
|---|---|---|
| `GET /bastion/rotation-plans` | read | `{"items":[{id,name,account_ids,interval_days,length,enabled,next_run_at,last_run_at,last_result,created_by,...}]}` |
| `POST /bastion/rotation-plans` | manage | `{"name","account_ids":[1],"interval_days":30,"length":20,"enabled":true,"next_run_at":"RFC3339 可选"}`；`length` 12–64，`interval_days` 1–365，名称重复 409 |
| `PUT /bastion/rotation-plans/:id` | manage | 同上；`next_run_at` 为空时保持原计划时间 |
| `DELETE /bastion/rotation-plans/:id` | manage | 级联删除执行记录 |
| `POST /bastion/rotation-plans/:id/run` | manage | 立即同步执行（最长 5 分钟），返回 `{"succeeded":1,"failed":0}` |
| `POST /bastion/accounts/:id/rotate` | manage | 单账号立即改密，`{"length":20}` 可选，返回 `{"status":"succeeded|failed|skipped","message":"..."}` |
| `GET /bastion/rotation-runs?plan_id=` | read | 最近 100 条执行记录（含 `plan_name`，手动执行为「手动」） |

单账号流程（`rotation.Rotate`，主机与存储通过接口注入）：生成密码 → 加密写入 `pending_secret_enc` → 用当前密码登录改密（root 用 `chpasswd`，其他用户在 PTY 中运行 `passwd`）→ 用新密码登录验证 → 提交为正式凭据。失败时：改密前就登录失败 → 主机未改，丢弃新密码；旧密码仍可用 → 丢弃新密码；旧密码不可用而新密码可用 → 视为成功；两者都不可用 → 保存新密码并标记失败、提示人工核对。私钥账号与非 SSH 资产 `skipped`。后台每分钟检查到期计划（PostgreSQL advisory lock 保证多副本只执行一份）。

## 登录 ACL 与登录日志（v1.27）

### GET /bastion/login-acls — `bastion:read`；POST、PUT /:id、DELETE /:id — `bastion:manage`
```json
{"name":"仅办公网","priority":10,"subject_type":"all|user|role|group","subject":"","asset_ids":[],"ip_ranges":["10.0.0.0/8","192.168.1.10","10.1.1.1-10.1.1.50","*"],
 "weekdays":[1,2,3,4,5],"time_from":"09:00","time_to":"18:00","action":"allow|deny","enabled":true,"description":""}
```
按 `priority` 升序取第一条命中的启用规则；`deny` 拒绝连接（403，原因写入登录日志），`allow` 放行；没有命中时放行（授权仍是必要条件）。`weekdays` 1=周一…7=周日，空 = 每天；`time_from > time_to` 表示跨午夜。时段按 `MATRIX_TZ`（默认 `Asia/Shanghai`，二进制内置时区库）判断。作用于 SSH Web 终端与数据库控制台（`/db/connect`、`/db/query`）。校验错误：`invalid ip range`、`time must be HH:MM`、`weekdays must be 1 (Mon) - 7 (Sun)`、`subject is required unless subject_type is all`。

### GET /bastion/login-logs — `bastion:audit`
分页；参数 `q`（用户/资产/IP/账号模糊）、`result`（`success|denied|failed`）、`protocol`、`from`、`to`（RFC3339）。项：`{id,user_id,username,asset_id,asset_name,account,protocol,client_ip,result,reason,session_id,created_at}`。

## 控制台总览与命令记录（v1.27）

### GET /bastion/overview — `bastion:read`
`assets{total,hosts,databases,accounts,nodes}`、`sessions{online,today,today_users}`、`logins{success,denied,failed}`（今日）、`approvals{command,sql,access}`（待审批，直接统计 `approval_tickets`）、`rotation{failed_7d,succeeded_7d,enabled_plans,verify_failed}`、`commands{blocked_today}`、`db_queries_today`、`trend[{date,sessions}]`（近 7 天）、`top_assets[{asset,sessions}]`。「今日」按 `MATRIX_TZ` 计算。

### GET /bastion/commands — `bastion:audit`
跨会话命令检索，分页；参数 `q`（命令/用户/资产模糊）、`action`、`username`、`asset_id`、`from`、`to`。项含 `rule_name`、`session_started_at`、会话的用户 / 资产 / 账号。

## 命令复核（审批单类型 `bastion.command`）

由 Web 终端在命令命中 `approve` 规则时以会话用户身份创建（`CreateOwned`，需要 `bastion:connect`），`payload`：`{session_id, asset_id, asset, command, rule_id, rule}`。列表 / 处理使用平台接口：`GET /approvals?scope=todo|mine|all&type=bastion.command`、`POST /approvals/:id/approve|reject|cancel`。终端每 2 秒查询一次状态，通过后把原命令发送到主机，拒绝 / 撤回 / 超时则取消；结果写入命令审计（`approved` / `rejected` / `cancelled`）。

## 数据库控制台与 SQL 工单（v1.27，`dbconsole.go`）

驱动：`github.com/go-sql-driver/mysql` 与 `github.com/jackc/pgx/v5/stdlib`（均已在 go.mod 中，无需新增依赖）。

### POST /bastion/db/connect — `bastion:connect`
`{"asset_id":5,"account_id":9,"database":""}`。校验授权、环境、登录 ACL 后真实连接一次并记录登录日志；返回 `{version, databases[], default_database, protocol, limits{max_rows:1000, default_rows:200, max_timeout_sec:60}}`。

### POST /bastion/db/query — `bastion:connect`
`{"asset_id":5,"account_id":9,"database":"app","sql":"SELECT ...","reason":"变更时必填","limit":200,"timeout_sec":15}`。按 `ClassifySQL` 路由：

| 分级 | 处理 | 响应 |
|---|---|---|
| `read` | 只读事务（`BEGIN READ ONLY` / `START TRANSACTION READ ONLY`）内执行，PostgreSQL `SET LOCAL statement_timeout`、MySQL `max_execution_time`，最多 `limit` 行（≤1000），文本单元格按启用的脱敏规则替换，事务回滚 | 200 `{query_id, level, risk, status:"executed", result{columns,rows,row_count,truncated,duration_ms}}`；数据库报错 400 `SQL 执行失败：...`，`details` 同结构 |
| `write` / `high` | 必须填写 `reason`，创建审批单 `bastion.sql`（不连库） | 202 / code 40304 `{query_id, status:"pending", ticket_id}` |
| `forbidden` | 直接拒绝（不连库） | 400 / 40001，`details.status="rejected"` |

每条语句先写入 `bastion_db_queries`（SQL 原文以 `d.Crypto` 加密并绑定记录 ID 作为附加数据，列表只返回去掉注释与字面量的 `summary`），执行后更新 `status`（`executed|failed|rejected|pending`）、行数、耗时。

审批通过后处理器：重新读取申请人的角色 / 权限 / 环境范围并校验 `bastion:connect`、资产授权仍有效、资产环境未变 → 解密 SQL → 重新分级（变严格则失败）→ 普通事务执行并提交（超时 5 分钟）→ 更新记录；审批单结果 `{query_id, rows_affected, duration_ms}`。拒绝 / 撤回时记录置为 `rejected`。审批单 `payload` 对外去掉加密 SQL，保留 `asset, account, database, level, risk, summary`。

### GET /bastion/my/db-queries — `bastion:connect`；GET /bastion/db/queries — `bastion:audit`
分页；参数 `q`、`level`、`status`、`asset_id`。不返回 SQL 原文。

## 未实现 / 限制

- RDP、VNC 的实际连接代理（仅资产登记 + guacd 健康检查）；MySQL / PostgreSQL 只提供 Web 数据库控制台（HTTP 查询），不提供原生协议代理（客户端工具直连）。
- 文件传输（SFTP / 上传下载）未实现，因此没有「文件传输记录」；授权里的 `upload` / `download` 动作仅登记。
- MySQL 的 `max_execution_time` 只约束 SELECT；变更语句的超时依赖连接上下文取消（驱动断开连接），服务器端语句可能在断开后继续执行到结束。
- 数据库控制台结果脱敏是对每个文本单元格做正则替换，不识别列语义；二进制列按字符串处理。
- 登录 ACL 不支持「需要审核后登录」动作，只有允许 / 拒绝。
- 录像上传 S3/MinIO：当前只写本地目录。计划：会话结束后用 minio-go `PutObject` 上传到 `recordings/<id>.cast`，`recording_path` 改存对象键，下载接口改为预签名 URL 重定向；多实例部署时必须启用，否则录像只存在于产生它的实例上。
- 强制断开、在线会话注册表为**单实例内存**；多实例需通过 Redis pub/sub 广播 kill。
- 命令识别基于输入行缓冲 + 回显屏幕行启发式：`vim` 等全屏程序内的按键、`sudo` 密码输入等也可能被当作"命令"记录；多行粘贴、shell 别名/变量展开、`bash -c` 嵌套可能绕过正则——过滤规则是辅助控制，不能替代最小权限账号。
- 连接本身不走审批（临时访问用 `bastion.access_request`）；命令级审批见「命令复核」。
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
| 浏览器 → 服务端 | `{"type":"input","data":"ls
"}`、`{"type":"resize","cols":120,"rows":32}` |
| 服务端 → 浏览器 | `{"type":"output","data":"..."}`、`{"type":"error","data":"..."}`、`{"type":"closed","data":"killed\|session ended"}` |

前端用轻量屏幕模型渲染（剥离 ANSI、处理 `
` 覆盖 / `` 退格 / 整屏清除），不引入 xterm 运行时依赖；颜色与光标寻址不还原。

## 本轮验证边界（重要）

以下项目**本轮未验证**，文档与界面都不得声称已联调：

- 真实 PostgreSQL 上的 `0002_asset_options` 迁移、资产/授权/审计 SQL 查询；
- 真实 SSH 目标主机的连接、TOFU 主机指纹校验与命令过滤；
- 真实 guacd 的 RDP / VNC 代理；
- Prometheus、日志、链路、Jenkins、Kafka 与真实 SSO。

已在本轮验证：`go test ./...`、`go vet ./...`、前端脚本解析、`git diff --check`、静态预览下的登录/改密/资产表单/终端空态交互。

界面约束：未接入后端时，资产与审计页显示"静态示例数据"提示；`live` 模式下未接入后端的子页（账号、授权策略、命令过滤、审计、脱敏、数据库）同样标注，避免把演示数据当成生产状态。

### v1.27（JumpServer 结构补全）验证边界

- 已验证：`go test ./internal/modules/bastion/`（改密流程用假 SSH 主机 / 假存储覆盖成功、旧密码可用回滚、新密码已生效、新旧都失败、主机不可达、保存失败、提交失败、跳过；SQL 执行器分级路由、禁止语句不执行、无审批服务拒绝、只读结果脱敏与行数上限；登录 ACL 匹配与输入校验；授权合并、过滤规则 allow 短路）、`go vet`、`gofmt -l`、前端 `node --check`。
- **未验证**：0006 迁移在真实 PostgreSQL 上执行；节点递归授权 SQL、总览统计 SQL、用户组主体匹配；真实 MySQL / PostgreSQL 的只读事务、超时与审批后执行；真实主机上的 `passwd` / `chpasswd` 改密；Web 终端 `approval` 消息在浏览器中的展示（需要应用 index.html 补丁）；跨实例的命令审批等待。
