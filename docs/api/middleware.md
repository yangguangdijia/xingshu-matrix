# 中间件模块（middleware）API

模块名 `middleware`，代码 `server/internal/modules/middleware/`。所有接口挂在已鉴权的 `/api/v1/middleware` 下，统一响应 `{code,message,data,request_id}`。写请求自动进入平台审计；控制台语句另行写入 `middleware_sql_audit`。

## 权限

| 权限 | 说明 |
|---|---|
| `middleware:read` | 查看实例、测试连接、Kafka/RabbitMQ/RocketMQ 只读接口 |
| `middleware:manage` | 实例增删改、Topic / 队列创建删除 |
| `middleware:console` | SQL 控制台、Redis 控制台 |
| `middleware:audit` | 查询控制台审计 |

所有带 `:id` 的接口还会校验实例 `env` 是否在用户环境范围内（`EnvScope`），并校验实例类型。

## 数据表

- `middleware_instances`：实例（`auth_enc` 为 AES-256-GCM 加密的认证 JSON，接口只返回掩码后的密码）。
- `middleware_sql_audit`：SQL / Redis 控制台每条语句（执行成功 `ok`、失败 `error`、被拦截 `blocked`、进入审批 `pending`）。
  > 注：需求中写的是 `mw_sql_audit`，按平台约定表名必须以模块名为前缀，故实际为 `middleware_sql_audit`。

## 审批类型

| Type | 用途 | 发起权限 |
|---|---|---|
| `middleware.sql` | 高危 SQL 在需审批的环境执行，审批通过后由 Handler 以申请人身份执行并写审计，结果存入审批单 result | `middleware:console` |
| `middleware.change` | Kafka Topic 创建/删除、RabbitMQ 队列创建/删除（环境策略要求审批时） | `middleware:manage` |

## AI 工具（只读，权限 `middleware:read`）

- `kafka_consumer_lag` `{instance_id, group?}`：返回消费组状态、成员数、总 lag、按 Topic lag。
- `mq_overview` `{instance_id}`：Kafka（broker/topic/分区/欠副本/消费组/总 lag/Top10 堆积组）、RabbitMQ（`/api/overview` + 节点内存/磁盘告警）、RocketMQ（dashboard 集群信息）。

---

## 1. 实例

实例字段：

```json
{
  "name": "kafka-prod-01",
  "type": "kafka",                     // kafka|rabbitmq|rocketmq|mysql|postgres|redis
  "env": "prod",
  "endpoints": ["10.0.0.1:9092","10.0.0.2:9092"],
  "auth": {"username":"admin","password":"***","tls":false,"tls_skip_verify":false,"sasl_mechanism":"SCRAM-SHA-512"},
  "options": {"database":"app"},
  "description": "核心消息集群"
}
```

endpoints 含义：
- kafka：bootstrap broker `host:port`（多个）。`sasl_mechanism` 支持 `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`。
- rabbitmq：Management 插件地址 `http(s)://host:15672`（取第一个），使用 Basic Auth。
- rocketmq：rocketmq-dashboard 地址 `http(s)://host:8080`；若填写 username 则先调用 `/login/login.do` 登录并复用 session cookie。
- mysql / postgres：`host:port`（取第一个）。options：`database` 默认库，postgres 可设 `sslmode`（默认 disable，tls=true 时 require）。
- redis：一个地址 = 单机（options `db` 默认库）；多个 = Cluster；options `master_name` = Sentinel。

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/middleware/instances?type=&env=&keyword=&page=&page_size=` | read | 分页列表（按 EnvScope 过滤） |
| POST | `/middleware/instances` | manage | 创建，返回 201 |
| POST | `/middleware/instances/test` | manage | 用请求体（未保存的实例）测试连接 |
| GET | `/middleware/instances/:id` | read | 详情（密码掩码） |
| PUT | `/middleware/instances/:id` | manage | 更新；`auth` 省略=保留原凭据，`password` 为空或含 `****`=保留原密码 |
| DELETE | `/middleware/instances/:id` | manage | 删除 |
| POST | `/middleware/instances/:id/test` | read | 测试连接 |
| GET | `/middleware/instances/:id/overview` | read | MQ 概览（同 AI 工具 mq_overview） |

测试连接响应：

```json
{"code":0,"data":{"ok":true,"latency_ms":12,"version":"RabbitMQ 3.13.1","message":"连接成功"}}
```

测试方式：Kafka `ListBrokers`；RabbitMQ `GET /api/overview`；RocketMQ `GET /cluster/list.query`；MySQL/PG `SELECT version()`；Redis `INFO server`。超时 10s。

## 2. Kafka（franz-go `kadm` v1.14.0 / `kgo` v1.18.0，直连 broker 协议）

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/middleware/instances/:id/kafka/brokers` | read | `[{node_id,host,port,rack}]` |
| GET | `/middleware/instances/:id/kafka/topics?internal=true&keyword=` | read | `[{name,id,internal,partitions,replication_factor,under_replicated}]` |
| GET | `/middleware/instances/:id/kafka/topics/:topic` | read | 含 `partition_details`（leader/replicas/isr/start_offset/end_offset）与 `configs`（敏感值不返回） |
| POST | `/middleware/instances/:id/kafka/topics` | manage | 创建 Topic（走 `middleware.change` Gate） |
| DELETE | `/middleware/instances/:id/kafka/topics/:topic` | manage | 删除 Topic（走 Gate） |
| GET | `/middleware/instances/:id/kafka/groups?group=&detail=true` | read | 消费组 + lag（kadm.Lag：DescribeGroups + OffsetFetch + ListOffsets） |

创建 Topic 请求（partitions / replication_factor 不填或 ≤0 时用 broker 默认值，需 Kafka 2.4+）：

```json
{"name":"orders","partitions":6,"replication_factor":3,"configs":{"retention.ms":"604800000","cleanup.policy":"delete"}}
```

响应：`{"topic":"orders","partitions":6,"replication_factor":3}`；若环境要求审批则 HTTP 202：

```json
{"code":<CodeApprovalRequired>,"message":"该环境的变更需要审批，已创建审批单","data":{"approval_required":true,"ticket":{"id":12,"type":"middleware.change",...}}}
```

消费组响应：

```json
[{"group":"order-svc","state":"Stable","protocol_type":"consumer","protocol":"range","coordinator":1,"members":3,
  "total_lag":1520,"topic_lag":{"orders":1520},
  "partitions":[{"topic":"orders","partition":0,"committed":10230,"end_offset":10500,"lag":270,"member":"consumer-1-xxx","client_id":"consumer-1","host":"/10.0.0.8"}]}]
```

`partitions` 仅在 `detail=true` 或指定 `group` 时返回。

## 3. RabbitMQ（Management HTTP API 透传）

| 方法 | 路径 | 权限 | 上游 |
|---|---|---|---|
| GET | `/middleware/instances/:id/rabbitmq/overview` | read | `GET /api/overview` |
| GET | `/middleware/instances/:id/rabbitmq/nodes` | read | `GET /api/nodes` |
| GET | `/middleware/instances/:id/rabbitmq/queues?vhost=&page=&page_size=&name=&use_regex=` | read | `GET /api/queues[/{vhost}]`（默认 page=1,page_size=100，返回上游分页结构） |
| GET | `/middleware/instances/:id/rabbitmq/exchanges?vhost=` | read | `GET /api/exchanges[/{vhost}]` |
| GET | `/middleware/instances/:id/rabbitmq/connections` | read | `GET /api/connections` |
| POST | `/middleware/instances/:id/rabbitmq/queues` | manage | `PUT /api/queues/{vhost}/{name}`（走 Gate） |
| DELETE | `/middleware/instances/:id/rabbitmq/queues/:vhost/:name` | manage | `DELETE /api/queues/{vhost}/{name}`（vhost `/` 传 `%2F`，走 Gate） |

只读接口的 `data` 为上游 JSON 原样返回。创建队列请求：

```json
{"vhost":"/","name":"orders","durable":true,"auto_delete":false,"arguments":{"x-queue-type":"quorum"}}
```

响应：`{"vhost":"/","queue":"orders","created":true}`。

## 4. RocketMQ（rocketmq-dashboard REST）

RocketMQ 原生管理需要 Remoting 协议 / mqadmin，较重；本模块改为调用部署好的 **rocketmq-dashboard** 的 REST 接口，解析其 `{"status":0,"data":...,"errMsg":...}` 外层后返回 `data`。

| 方法 | 路径 | 权限 | 上游 |
|---|---|---|---|
| GET | `/middleware/instances/:id/rocketmq/clusters` | read | `GET /cluster/list.query`（clusterInfo.brokerAddrTable / clusterAddrTable、brokerServer 运行统计） |
| GET | `/middleware/instances/:id/rocketmq/topics` | read | `GET /topic/list.query`（`{"topicList":[...],"brokerAddr":...}`） |

dashboard 开启登录（`rocketmq.config.loginRequired=true`）时，实例填写 username/password 即可。

## 5. SQL 控制台（MySQL go-sql-driver v1.8.1 / PostgreSQL pgx v5 stdlib）

`POST /middleware/instances/:id/sql`，权限 `middleware:console`，实例类型 mysql|postgres。

请求：

```json
{"sql":"SELECT id,name FROM users WHERE id > 10","database":"app","timeout_sec":30,"confirm":false,"reason":"修复数据"}
```

执行流程：
1. **风险分析**（去注释、屏蔽字符串字面量后判断）：每次仅允许 **一条** 语句（多语句直接拒绝；MySQL 驱动 `multiStatements=false`）。
   - `read`：SELECT / SHOW / DESC / EXPLAIN / VALUES / TABLE / 纯查询 WITH
   - `dangerous`：DROP、TRUNCATE、ALTER、RENAME、GRANT、REVOKE、无 WHERE 的 DELETE / UPDATE（含 CTE 中的）、SELECT INTO OUTFILE/DUMPFILE
   - 其余为 `write`（INSERT、带 WHERE 的 UPDATE/DELETE、CREATE 等）
2. **高危语句**：
   - 环境策略要求审批（`RBAC.RequiresApproval(env)`）→ `Approval.Gate(Type "middleware.sql")`，返回 HTTP 202 + 审批单，审计状态 `pending`；审批通过后 Handler 执行并写审计（带 `approval_id`），结果写入审批单 result。
   - 环境为生产（env 为 prod/prd/production、prod-*、*-prod）但未配置审批策略 → 403 拒绝（审计 `blocked`）。
   - 非生产环境 → 需 `confirm=true`，否则 400（审计 `blocked`）。
3. **执行**：独立连接；`timeout_sec` 默认 30、最大 300，客户端 context 超时 + 服务端超时（MySQL `max_execution_time`，PG `statement_timeout`）。查询最多返回 **500 行**（`truncated=true` 表示被截断）。
4. 每条语句写入 `middleware_sql_audit`。

响应：

```json
{"code":0,"data":{"risk":{"level":"read","keyword":"SELECT"},"columns":["id","name"],"rows":[[11,"alice"]],
 "row_count":1,"truncated":false,"rows_affected":0,"duration_ms":4,"audit_id":981}}
```

高危拦截示例（400）：`{"code":<CodeValidation>,"message":"高危 SQL（DELETE 未带 WHERE 条件（全表）），请确认后以 confirm=true 重新提交","details":{"level":"dangerous",...}}`

## 6. Redis 控制台（go-redis v9）

`POST /middleware/instances/:id/redis`，权限 `middleware:console`。

```json
{"command":"HGETALL \"user:1001\"","db":0}
```
或 `{"args":["HGETALL","user:1001"]}`。

- **禁止命令**（403，审计 `blocked`）：FLUSHALL、FLUSHDB、KEYS（任意模式，请用 SCAN）、CONFIG SET/REWRITE/RESETSTAT、SHUTDOWN、DEBUG、MONITOR、(P/S)SUBSCRIBE、SYNC/PSYNC、REPLICAOF/SLAVEOF、SWAPDB、MODULE、SCRIPT FLUSH、FUNCTION FLUSH、CLUSTER RESET/FAILOVER/FORGET、ACL SETUSER/DELUSER、CLIENT KILL、BGREWRITEAOF、BGSAVE、SAVE。
- 风险等级：常见读命令为 `read`，其余 `write`；超时 30s；数组结果最多 500 个元素。

响应：`{"code":0,"data":{"result":{"name":"alice"},"risk":"read","duration_ms":1,"audit_id":982}}`（RESP2 下 HGETALL 返回扁平数组）。

## 7. 控制台审计

`GET /middleware/sql-audit?instance_id=&username=&status=&risk=&type=&page=&page_size=`，权限 `middleware:audit`，按 EnvScope 过滤。

```json
{"code":0,"data":{"items":[{"id":982,"instance_id":3,"instance_name":"mysql-prod","instance_type":"mysql","env":"prod","database":"app",
 "statement":"DELETE FROM t","risk":"dangerous","status":"pending","error":"","rows":0,"duration_ms":0,"approval_id":15,
 "user_id":2,"username":"zhangsan","created_at":"2026-10-17T12:00:00Z"}],"total":1,"page":1,"page_size":20}}
```

## 未实现 / 限制

- RocketMQ 仅只读（集群、Topic 列表），依赖 rocketmq-dashboard；未实现 Topic 创建、消费组进度、消息查询（可扩展 dashboard 的 `/topic/createOrUpdate.do`、`/consumer/groupList.query`）。
- Kafka：未实现 Topic 配置修改、分区扩容、消费位点重置、ACL；SASL 仅 PLAIN/SCRAM，不支持 OAUTHBEARER/Kerberos。
- RabbitMQ：只实现队列增删；exchange/binding/用户/策略管理未实现。
- SQL 风险判断为词法级（非完整 SQL 解析）；存储过程内部语句、`CALL`、动态 SQL 不做深度识别。MySQL `max_execution_time` 只作用于 SELECT，其他语句超时依赖客户端断开连接。
- Redis 控制台不支持 Pipeline/事务（MULTI 需在一次连接内，单命令模式下无意义）；危险命令无审批通道（直接禁止）。
- 连接测试与 SQL 执行每次新建连接，无连接池复用。
- 单元测试覆盖 SQL 风险分析、Redis 命令拦截、RabbitMQ/RocketMQ HTTP 调用（httptest）；Kafka 与数据库未做集成测试（需真实环境）。
