# hosts 模块 API（主机管理 + matrix-agent）

模块名 `hosts`，代码 `server/internal/modules/hosts/`，Agent 二进制 `server/cmd/matrix-agent/`。
所有业务接口挂在 `/api/v1`（需登录），统一响应 `{code,message,data,request_id}`。

## 权限

| 权限 | 说明 |
|---|---|
| `host:read` | 查看主机、事实信息、执行记录 |
| `host:write` | 主机增删改、注册令牌管理 |
| `host:exec` | 执行命令、批量执行、推送文件 |

写/执行类接口额外校验环境范围 `p.CanEnv(host.env)`。

## 数据表（迁移 `0001_init.up.sql`）

- `hosts_hosts`：主机（name、ip、env、labels JSONB、facts JSONB、agent_version、secret_hash、online、last_seen_at …）
- `hosts_enroll_tokens`：一次性注册令牌（仅存 SHA-256 哈希）
- `hosts_tasks`：执行记录（kind=exec|file_push、status=running|success|failed|timeout、exit_code、output ≤1MB）

## Agent 通信协议（WebSocket + JSON 帧）

选型：**WebSocket（gorilla/websocket）+ JSON 帧**，而非 gRPC——单端口复用 HTTPS（可过 Nginx/Ingress），无需 protobuf 代码生成，Agent→Server 单向建连，内网主机无需开放端口。协议定义见 `internal/modules/hosts/proto/proto.go`。

每个 WebSocket 文本帧是一个信封：`{"type":"<类型>","data":{...}}`。

| 方向 | type | data |
|---|---|---|
| A→S | `hello` | `{agent_version, facts}`，建连后第一帧（15 秒内） |
| A→S | `heartbeat` | `{facts}`，每 30 秒 |
| A→S | `exec_output` | `{task_id, stream:"stdout"\|"stderr", data}` |
| A→S | `exec_result` | `{task_id, exit_code, timed_out, error, duration_ms}` |
| A→S | `file_result` | `{task_id, bytes, error}` |
| S→A | `welcome` | `{host_id, secret?}`（secret 仅注册时下发一次） |
| S→A | `exec` | `{task_id, command, timeout_sec}` |
| S→A | `file_push` | `{task_id, path, content_base64, mode}` |
| S→A | `error` | `{message}` |

facts 字段：`hostname, os, platform, platform_version, kernel, arch, cpu_model, cpu_cores, cpu_percent, load1, mem_total, mem_used, mem_percent, disks[{mount,fstype,total,used,percent}], uptime, boot_time, ips[]`（Agent 端 gopsutil 采集）。

### 注册与重连流程

1. 管理员 `POST /api/v1/hosts/enroll-tokens` 得到明文令牌（只返回一次，库中存哈希，默认 24h 过期）。
2. Agent 首次：`GET /api/v1/agent/ws?token=<令牌>` → 发送 `hello` → 服务端在事务中**原子消费令牌**并创建主机、生成每主机密钥（库中存 SHA-256）→ 回 `welcome{host_id, secret}` → 服务端关闭连接。
3. Agent 把 `host_id/secret` 写入 `-state` 文件（0600），随即用密钥重连：`GET /api/v1/agent/ws`，请求头 `Authorization: Bearer <secret>`、`X-Matrix-Host-ID: <id>`。
4. 鉴权失败在升级前直接返回 HTTP 401。同一主机新连接会顶掉旧连接。
5. 服务端每 30s 发 ping，90s 无任何帧判定掉线 → `online=false`，该主机所有执行中任务标记 `failed: agent disconnected`。
6. 在线判定：`online=true 且 last_seen_at 在 90 秒内`。

### 公共接口

#### GET /api/v1/agent/ws
- 无登录（挂 `d.Public`），自行校验令牌或主机密钥；见上。

## 主机 CRUD

#### GET /api/v1/hosts　`host:read`
查询参数：`page,size,env,keyword(名称/IP/hostname 模糊),online=true`
```json
{"code":0,"data":{"items":[{"id":7,"name":"web-1","ip":"10.0.0.5","env":"prod","labels":{"role":"web"},
 "facts":{"hostname":"web-1","platform":"ubuntu","platform_version":"22.04","kernel":"5.15.0-105-generic","arch":"x86_64",
 "cpu_cores":8,"cpu_percent":12.5,"mem_total":16777216000,"mem_percent":41.2,"disks":[{"mount":"/","fstype":"ext4","total":107374182400,"used":32212254720,"percent":30}],"uptime":864000,"ips":["10.0.0.5"]},
 "agent_version":"0.1.0","enrolled":true,"online":true,"last_seen_at":"2026-10-17T04:00:00Z","registered_at":"2026-10-10T02:00:00Z",
 "created_by":"admin","created_at":"...","updated_at":"..."}],"total":1,"page":1,"size":20}}
```

#### GET /api/v1/hosts/:id　`host:read` → 单个主机（同上）
#### POST /api/v1/hosts　`host:write`
手工登记主机（未安装 Agent，`enrolled=false`，不能执行命令）。
```json
{"name":"db-1","ip":"10.0.1.3","env":"prod","labels":{"role":"db"}}
```
#### PUT /api/v1/hosts/:id　`host:write`　请求体同上，返回更新后主机。
#### DELETE /api/v1/hosts/:id　`host:write`
删除主机（级联删除执行记录），并断开其 Agent；密钥随之失效，Agent 无法再连接，需重新注册。

## 注册令牌

#### POST /api/v1/hosts/enroll-tokens　`host:write`
```json
{"name":"","env":"prod","labels":{"role":"web"},"ttl_hours":24}
```
`name` 为空时使用 Agent 上报的 hostname；`env/labels` 会写入新主机。响应（token 仅此一次返回）：
```json
{"code":0,"data":{"token":{"id":3,"name":"","env":"prod","labels":{"role":"web"},"expires_at":"2026-10-18T04:00:00Z","used_at":null,
 "used_host_id":null,"created_by":"admin","created_at":"...","token":"mxe_4f1c..."},
 "install_command":"matrix-agent -server https://matrix.example.com -token mxe_4f1c..."}}
```
#### GET /api/v1/hosts/enroll-tokens　`host:write` → 令牌列表（不含明文）
#### DELETE /api/v1/hosts/enroll-tokens/:id　`host:write`

## 命令执行

Agent 端以 `/bin/sh -c <command>` 执行，独立进程组，超时 SIGKILL 整组；单任务输出上限 1MB（超出截断）；Agent 默认最多 8 个并发命令。超时默认 60s、最大 3600s；服务端在超时 + 30s 仍未收到结果时把任务置为 failed。

**生产审批**：执行前按主机环境调用 `d.Approval.Gate`（类型 `hosts.exec`，payload `{host_ids,command,timeout_sec}`）。环境策略要求审批时返回 HTTP 202 + 审批单；审批通过后由 TypeSpec Handler 执行并把每台主机的结果（输出尾部 4KB）写入审批单 result，完整输出在 `hosts_tasks`（`ticket_id` 关联，`batch_id=ticket-<id>`）。

#### POST /api/v1/hosts/:id/exec　`host:exec`
```json
{"command":"df -h /","timeout_sec":30}
```
- 默认同步：等待完成后返回任务：
```json
{"code":0,"data":{"id":42,"host_id":7,"kind":"exec","batch_id":"","command":"df -h /","timeout_sec":30,"status":"success",
 "exit_code":0,"output":"Filesystem Size Used Avail Use% Mounted on\n/dev/sda1 100G 30G 70G 30% /\n","error":"",
 "triggered_by":"admin","ticket_id":null,"started_at":"...","finished_at":"..."}}
```
- `?stream=true` 或 `Accept: text/event-stream`：SSE 流
```
event: task
data: {"id":42,"status":"running",...}

event: output
data: {"stream":"stdout","data":"Filesystem ..."}

event: done
data: {"id":42,"status":"success","exit_code":0,"output":"...",...}
```
客户端断开不影响任务继续执行并入库。
- 需审批：HTTP 202 `{"code":<CodeApprovalRequired>,"data":{"approval_required":true,"ticket":{...}}}`。
- 主机离线：任务立即 `status=failed, error="agent offline"`。

#### POST /api/v1/hosts/batch-exec　`host:exec`
```json
{"host_ids":[7,8,9],"command":"uptime","timeout_sec":30}
```
异步：按环境分组过审批闸门，可执行的部分立即下发，返回：
```json
{"code":0,"data":{"batch_id":"bm1x2y3z","tasks":[{"id":43,"host_id":7,"status":"running",...}],
 "pending_approvals":[{"id":12,"type":"hosts.exec","env":"prod",...}]}}
```
全部待审批时 HTTP 202。最多 200 台。

#### GET /api/v1/hosts/tasks?batch_id=…　`host:read` → 批次任务分页
#### GET /api/v1/hosts/tasks/:id　`host:read` → 单任务
#### GET /api/v1/hosts/tasks/:id/stream　`host:read` → SSE（运行中先回放已有输出再实时推送；已结束直接 `done`）
#### GET /api/v1/hosts/:id/tasks　`host:read` → 该主机执行历史分页

## 文件推送

#### POST /api/v1/hosts/:id/files　`host:exec`
```json
{"path":"/etc/myapp/app.conf","content_base64":"a2V5PXZhbHVlCg==","mode":"0644"}
```
- 路径须为绝对、规范路径；解码后 ≤ 1MB；Agent 写临时文件后 rename（原子替换），自动创建父目录。
- 生产环境走 `d.Approval.Gate`（类型 `hosts.file_push`）。
- 同步返回 `kind=file_push` 的任务，`output` 如 `wrote 10 bytes`。

## AI 工具

| 名称 | 权限 | 确认 | 说明 |
|---|---|---|---|
| `hosts_list` | `host:read` | 否 | 参数 `{env?, keyword?, online_only?}`，返回最多 100 台主机摘要（OS、内核、CPU/内存/磁盘） |
| `hosts_exec` | `host:exec` | **是** | 参数 `{host_ids, command, timeout_sec≤300}`；Handler 内对每个环境调用 `Gate`，生产需审批时返回 `pending_approvals`，否则等待完成返回各主机结果（输出尾部 8KB） |

## Agent 使用

```
matrix-agent -server https://matrix.example.com -token mxe_xxx [-state /var/lib/matrix-agent/state.json] [-insecure] [-max-concurrency 8]
```
- 也可用环境变量 `MATRIX_SERVER`、`MATRIX_ENROLL_TOKEN`。
- 首次注册后令牌不再需要；之后以 state 文件中的密钥连接。
- 断线指数退避重连（1s→60s，带抖动；稳定连接 >1 分钟后重置）。
- 交叉编译：`GOOS=linux GOARCH=amd64 go build -o matrix-agent ./cmd/matrix-agent`。需以有权限的用户（通常 root，systemd 服务）运行。

## 未实现 / 限制

- **单实例 Hub**：Agent 连接保存在进程内存。多副本部署时，命令只能下发到 Agent 所连实例（需 Redis pub/sub 路由或会话亲和），当前未实现；服务启动时会把所有主机置为离线、执行中任务置为失败。
- 不支持交互式终端/PTY（Web Shell 属 bastion 模块）、不支持 stdin、不支持大文件/分片传输与文件拉取。
- 无密钥轮换接口（删除主机后重新注册即可）；令牌和密钥为 SHA-256 哈希（高熵随机值，无需慢哈希）。
- SSE 订阅者消费过慢时会丢弃中间输出帧（完整输出仍入库，`done` 事件包含全部输出）。
- 命令黑名单/危险命令识别未实现，依赖 RBAC + 审批 + 审计。
- 文件推送审批单的 payload 中包含文件 base64 内容（≤1MB）。
- Agent 仅支持 Linux/类 Unix（`/bin/sh`、进程组）。
- 需在 `internal/modules/registry.go` 中启用 `hosts.New()`（本模块未修改该文件）。
