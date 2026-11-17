# 可观测模块 API（observability）

代码：`server/internal/modules/observability`　模块名：`observability`　
所有接口前缀 `/api/v1`，统一响应 `{code,message,data,request_id}`。除 Webhook 外均需登录。
上游错误：上游返回 400/422 → 本接口 400（`message` 带上游原因）；网络错误/其他非 2xx → 502（`code=50200`）。

## 权限

| 权限码 | 说明 |
|---|---|
| `datasource:read` / `datasource:manage` | 查看 / 管理数据源 |
| `metrics:read` | PromQL 查询、标签、序列 |
| `logs:read` | 日志查询 |
| `alert:read` | 告警列表、告警规则、静默列表 |
| `alert:handle` | 认领 / 关闭告警、告警转故障（另需 `oncall:manage`） |
| `alert:silence` | 创建 / 删除静默 |
| `trace:read` | 链路查询 |
| `notify:manage` | 通知中心：渠道、消息模板、订阅规则、发送记录与重发、通知安全策略（全部接口同一权限） |

环境范围：每个数据源有 `env` 标签，所有按数据源的操作都校验 `Principal.CanEnv(env)`；告警列表按用户环境范围过滤。

## 数据表

- `obs_datasources`：类型 `prometheus|loki|elasticsearch|alertmanager|jaeger|tempo`、url、env、`auth_type none|basic|bearer`、username、`secret_enc`（AES-GCM 加密的密码或 Token）、`options`（JSON，见下）、`webhook_token_hash`（SHA-256，仅 alertmanager）。
- `obs_alerts`：按 `(datasource_id, fingerprint)` 唯一，`status firing|resolved`，`handle_status open|acked|closed`；`claim_comment`/`claim_silence_id`/`claim_silence_until`（迁移 0009，认领备注与认领静默）。
- `obs_channels`：`type dingtalk|feishu|wecom|slack|webhook|email`（迁移 0004 放开 slack/webhook，0007 重申），`config_enc` 整体加密（含 Webhook、签名密钥、请求头、SMTP 密码），`severities`/`envs` 为渠道级告警兜底过滤（空=全部），`description`、`templates`（事件类型→模板 id）、`last_sent_at`/`last_status`（迁移 0007）。
- `obs_notify_templates`、`obs_notify_subscriptions`、`obs_notify_records`、`obs_notify_cursors`：通知中心（迁移 0007，见第 6 节）。

`options` 字段：
- elasticsearch：`index`（必填，可被请求覆盖，如 `logs-*`）、`time_field`（默认 `@timestamp`）、`message_field`
- loki / tempo：`org_id` → 请求头 `X-Scope-OrgID`

---

## 1. 数据源

### GET /observability/datasources?type=prometheus　`datasource:read`
返回当前用户可见环境内的数据源数组（密钥已 `Mask`）。
```json
[{"id":1,"name":"prom-prod","type":"prometheus","url":"http://prometheus:9090","env":"prod","auth_type":"bearer","username":"","secret":"ab****yz","tls_skip_verify":false,"timeout_sec":30,"options":{},"has_webhook_token":false,"created_by":"admin","created_at":"...","updated_at":"..."}]
```

### POST /observability/datasources　`datasource:manage`
```json
{"name":"am-prod","type":"alertmanager","url":"http://alertmanager:9093","env":"prod","auth_type":"basic","username":"admin","secret":"pass","tls_skip_verify":false,"timeout_sec":30,"options":{}}
```
响应 201；alertmanager 类型额外**仅此一次**返回 Webhook Token：
```json
{"datasource":{...},"webhook_token":"9f3c...","webhook_path":"/api/v1/alerts/webhook?token=9f3c..."}
```

### GET /observability/datasources/:id　`datasource:read`
### PUT /observability/datasources/:id　`datasource:manage`
请求体同创建；`type` 不可修改；`secret` 省略 = 保留原值，`""` = 清空。
### DELETE /observability/datasources/:id　`datasource:manage`（级联删除该源告警）
### POST /observability/datasources/:id/webhook-token　`datasource:manage`
仅 alertmanager，轮换 Token，返回 `{"webhook_token":"...","webhook_path":"..."}`，旧 Token 立即失效。

### POST /observability/datasources/:id/test　`datasource:read`
按类型调用：Prometheus `/api/v1/status/buildinfo`、Loki `/loki/api/v1/labels`、ES `GET /`、Alertmanager `/api/v2/status`、Jaeger `/api/services`、Tempo `/api/echo`。
```json
{"ok":true,"latency_ms":23,"info":{...上游返回...}}
{"ok":false,"latency_ms":5001,"info":null,"error":"prometheus /api/v1/status/buildinfo: ... timeout"}
```

---

## 2. Prometheus（代理 + 校验）

路径中 `:id` 为 prometheus 数据源 id。时间参数支持 RFC3339、Unix 秒（可带小数）、`now`、`now-1h`/`now-7d`。
返回体为 Prometheus 原始 `{status,data,warnings}`（`status != success` 时转为 400）。

| 方法 路径 | 权限 | 参数 | 上游 |
|---|---|---|---|
| GET `/observability/prometheus/:id/query` | metrics:read | `query`(必填,≤8192), `time` | `/api/v1/query` |
| GET `/observability/prometheus/:id/query_range` | metrics:read | `query`, `start`(默认 end-1h), `end`(默认 now), `step`(`30s`/`5m`/秒数，默认 范围/250) | `/api/v1/query_range` |
| GET `/observability/prometheus/:id/labels` | metrics:read | `match[]`(可多值), `start`, `end` | `/api/v1/labels` |
| GET `/observability/prometheus/:id/label/:name/values` | metrics:read | 同上，`name` 需合法标签名 | `/api/v1/label/<name>/values` |
| GET `/observability/prometheus/:id/series` | metrics:read | `match[]`(**必填**), `start`, `end` | `/api/v1/series` |
| GET `/observability/prometheus/:id/rules` | alert:read | `type=alert|record` | `/api/v1/rules`（告警规则读取） |

校验：start < end、step > 0、点数 (end-start)/step ≤ 11000。
示例：`GET /api/v1/observability/prometheus/1/query_range?query=rate(http_requests_total[5m])&start=now-6h&step=1m`
```json
{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"job":"api"},"values":[[1700000000,"12.3"]]}]}}
```

---

## 3. 日志（统一查询）

### POST /observability/logs/query　`logs:read`
```json
{"datasource":2,"query":"{app=\"api\"} |= \"error\"","from":"now-1h","to":"now","limit":200,"index":"可选，仅ES"}
```
- Loki：`GET /loki/api/v1/query_range`（`start/end` 纳秒、`limit`、`direction=backward`），query 为 LogQL，必须是日志流查询（metric 查询返回 400）。
- Elasticsearch：`POST /<index>/_search`，`query` 支持 Lucene / 类 KQL `field:value and field2:"a b"`：小写 `and/or/not` 转大写、去除冒号两侧空格（引号内不变），空查询 = `*`；生成 `bool.must[query_string{default_operator:AND, analyze_wildcard, lenient}] + filter[range <time_field>]`，按时间倒序，`track_total_hits`。
- `limit` 默认 100，最大 5000；`from` 默认 `to-1h`。

响应（统一行结构）：
```json
{"datasource":"loki-prod","engine":"loki","query":"{app=\"api\"} |= \"error\"","total":2,
 "rows":[{"ts":"2026-10-17T03:00:01Z","level":"error","service":"api","host":"api-7d9-1","message":"...","labels":{"app":"api","pod":"api-7d9-1"}}]}
```
字段归一：`level` 取标签 `level/detected_level/severity/log.level`，否则从消息前 200 字符识别（warning→warn，err→error，critical/panic→fatal）；`service` 取 `service_name/service/service.name/app/container/job...`；`host` 取 `host/hostname/host.name/instance/pod/kubernetes.pod_name...`。ES 的 `_source` 扁平化为点分键放入 `labels`（附 `_index`、`_id`），`total` 为 ES 命中总数。

---

## 4. 告警

### POST /alerts/webhook?token=xxx　（公开，挂在 `d.Public`）
Alertmanager receiver 配置：
```yaml
receivers:
- name: matrix
  webhook_configs:
  - url: https://matrix.example.com/api/v1/alerts/webhook?token=<webhook_token>
    send_resolved: true
```
也接受 `Authorization: Bearer <token>`。Token 以 SHA-256 比对确定所属 alertmanager 数据源（及其 env）。请求体为 Alertmanager webhook v4 标准格式。
处理：按 `fingerprint` upsert（缺失时用排序后标签的 SHA-256 前 16 位）；`resolved→firing` 重新打开（handle_status=open、fire_count+1、清空认领人）；**首次出现的 firing 或 resolved→firing** 异步交给通知中心按订阅规则推送（见第 6 节）；重复的 firing 不重复推送。
```json
{"received":3,"notified":1}
```
错误：无 token/无效 token → 401。

### GET /observability/alerts　`alert:read`
参数：`status=firing|resolved`、`handle_status=open|acked|closed`、`severity`、`datasource`、`q`（alertname/annotations 模糊）、`page`、`page_size`。排序：firing 优先，再按最近接收时间。
```json
{"items":[{"id":10,"datasource_id":3,"datasource":"am-prod","env":"prod","fingerprint":"a1b2...","alertname":"HighCPU","severity":"critical","status":"firing","handle_status":"open","labels":{"instance":"n1"},"annotations":{"summary":"CPU>90%"},"generator_url":"http://prom/graph?...","starts_at":"...","ends_at":null,"fire_count":1,"acked_by":"","acked_at":null,"closed_by":"","closed_at":null,"close_comment":"","last_received_at":"...","created_at":"..."}],"total":1,"page":1,"page_size":20}
```
### GET /observability/alerts/:id　`alert:read`
### POST /observability/alerts/:id/ack　`alert:handle`
认领：仅 `open` 可认领，已被他人认领/已关闭返回 409。响应 `{"id":10,"handle_status":"acked","acked_by":"alice"}`。
### POST /observability/alerts/:id/close　`alert:handle`
`:id` 可为告警 id 或 fingerprint（同认领接口，需 `cluster_id`）。请求体可选 `{"comment":"已扩容","force":false}`。

认领时若在 Alertmanager 创建了静默且未到期，关闭前先删除该静默（静默已不存在视为成功）：
- 删除失败且 `force=false`：返回 502（code 50200），告警状态不变；
- `force=true`：照常关闭，保留上游静默至到期，关闭说明追加失败记录（时间线可见）。

响应 `{"id":10,"handle_status":"closed","closed_by":"alice","silence_id":"…","silence_deleted":true}`；`silence_id`/`silence_deleted` 仅在存在需清理的静默时返回。已关闭再关闭返回 409。关闭后若 Alertmanager 再次发送 resolved→firing，会自动重新打开。

> 以下三个接口的 `:id` 可为告警数字 id 或 fingerprint；fingerprint 命中多个 Alertmanager 数据源时返回 409，需改用 id 或带 `datasource_id`（body）/`datasource`（query）。均按 `?cluster_id=` 校验集群归属（同 GET /alerts/:id）。

### POST /observability/alerts/:id/claim?cluster_id=1　`alert:handle`（`silence=true` 时另需 `alert:silence`）
认领并（可选）在告警来源 Alertmanager 上创建静默。
```json
{"comment":"已介入处理","duration":"2h","datasource_id":3,"silence":true}
```
- `duration` 5m–72h，默认 `2h`；`silence` 省略视为 `true`。静默匹配器取告警的 alertname/cluster/namespace/pod… 前 4 个已有标签（同 `DefaultSilenceMatchers`），中心化数据源自动补集群标签。
- 认领人/时间写入 `acked_by/acked_at`（响应同时给出 `claimed_by/claimed_at`），备注与静默写入 `claim_comment/claim_silence_id/claim_silence_until`（迁移 0009）；告警 resolved→firing 重新打开时一并清空。
- 顺序：先建静默再认领；静默失败（上游 502）则不认领；认领因并发落空时撤销刚建的静默。
```json
{"id":10,"handle_status":"acked","claimed_by":"alice","claimed_at":"...","acked_by":"alice","acked_at":"...","silenced":true,"silence_id":"6f1c...","silence_until":"..."}
```
本人重复认领幂等返回当前状态（附 `"already_claimed":true`）。
错误：已被他人认领 → 409「告警已被 bob 认领，如需接手请先与其确认」；已关闭 → 409「告警已关闭，无法认领」；无可用 Alertmanager 数据源 / 该环境静默需审批 / 告警无可用标签 → 412（前端据此提示「只认领」并以 `silence:false` 重试）；无 `alert:silence` → 403；上游失败 → 502。

### GET /observability/alerts/:id/timeline?cluster_id=1　`alert:read`
按时间升序（同一时刻按 触发→通知→认领→静默→转故障→恢复→故障解决→关闭）返回：
```json
{"alert_id":10,"items":[
 {"created_at":"...","kind":"triggered","actor":"Alertmanager","message":"告警触发：HighCPU（级别 critical），CPU>90%"},
 {"created_at":"...","kind":"notified","actor":"通知中心","message":"触发通知已发送至「值班群」","ref":"88"},
 {"created_at":"...","kind":"claimed","actor":"alice","message":"认领告警：已介入处理"},
 {"created_at":"...","kind":"silenced","actor":"alice","message":"认领时在 Alertmanager 创建静默 6f1c...，至 ...","ref":"6f1c..."},
 {"created_at":"...","kind":"incident","actor":"alice","message":"手动转为故障 INC-5（P1）：[告警] CPU>90%","ref":"5"},
 {"created_at":"...","kind":"resolved","actor":"Alertmanager","message":"告警已恢复"}]}
```
`kind`：`triggered|notified|notify_failed|notify_suppressed|claimed|silenced|incident|resolved|incident_resolved|closed`。数据来源：`obs_alerts`（触发取 `starts_at`，多次触发只保留最近一次并注明累计次数；恢复取 `ends_at`；认领/静默/关闭）、`obs_notify_records`（`event_kind='alert'` 且 `event_key` 含 `alert:<ds>:<fingerprint>:`，含聚合发送）、`oncall_alert_links`+`oncall_incidents`。

### POST /observability/alerts/:id/incident?cluster_id=1　`alert:handle` + `oncall:manage`
告警转故障，复用值班模块 `declareFromAlert`（同 `POST /oncall/incidents/from-alert/:alert_id`：写 `oncall_incidents`、`oncall_incident_events`、`oncall_alert_links`，按排班指派一线值班）。
```json
{"severity":"P1","schedule_id":2}
```
`severity` 省略时按告警级别推导（critical/P0/P1→P1，info→P3，其余 P2）。幂等：告警已关联**未解决**故障时直接返回该故障（`created:false`）；同一告警并发转故障以事务级 advisory lock 串行化。
```json
{"alert_id":10,"incident_id":5,"created":true}
```

### 静默（代理 Alertmanager `/api/v2/silences`）
- GET `/observability/silences?datasource=3&filter=alertname="HighCPU"`　`alert:read` → 返回 Alertmanager 原始静默数组。
- POST `/observability/silences`　`alert:silence`
```json
{"datasource":3,"matchers":[{"name":"alertname","value":"HighCPU","isRegex":false,"isEqual":true}],"startsAt":"now","duration":"2h","comment":"变更窗口"}
```
（`endsAt` 与 `duration` 二选一，默认 2h；`createdBy` 自动填当前用户名）→ 201 `{"silenceID":"6f1c..."}`
- DELETE `/observability/silences/:sid?datasource=3`　`alert:silence` → 上游 `DELETE /api/v2/silence/:sid`。

---

## 5. 链路追踪

`datasource` 为 jaeger 或 tempo 数据源 id。
- GET `/observability/traces/services?datasource=4`　`trace:read`：Jaeger `/api/services`；Tempo `/api/v2/search/tag/resource.service.name/values`。
- GET `/observability/traces/search?datasource=4&service=api&operation=GET%20/&tags={"http.status_code":"500"}&minDuration=100ms&start=now-1h&end=now&limit=20`　`trace:read`
  - Jaeger：`/api/traces`（start/end 微秒，`service` 必填）。
  - Tempo：`/api/search`（start/end 秒；`q` 传 TraceQL 时直接使用，否则由 service/operation/tags 组装 `tags=service.name=api name=...`）。
```json
[{"trace_id":"4bf92f3577b34da6","root_service":"api","root_operation":"GET /","start":"...","duration_ms":123.4,"span_count":12,"services":["api","db"],"error":false}]
```
（Tempo 搜索结果不含 span_count/services/error。）
- GET `/observability/traces/:traceId?datasource=4`　`trace:read`：Jaeger `/api/traces/:id`；Tempo `/api/traces/:id`（OTLP JSON，兼容 `batches/resourceSpans/trace.*`、`scopeSpans/instrumentationLibrarySpans`，base64 ID 转 hex）。
```json
{"trace_id":"4bf92f3577b34da6","spans":[{"span_id":"00f067aa0ba902b7","parent_id":"","service":"api","operation":"GET /","start":"...","duration_us":1200,"error":false,"tags":{"http.status_code":"200"}}]}
```

---

## 6. 通知中心　（全部 `notify:manage`）

对标夜莺 / KubeSphere 通知管理：**渠道**负责「发到哪里」，**订阅规则**负责「什么事件发给哪些渠道」，**消息模板**负责「发什么内容」，**发送记录**可筛选并对失败记录重发，**通知安全**控制出站地址。

### 6.1 渠道

- GET `/observability/channels`、GET `/observability/channels/:id`：返回渠道（敏感字段掩码：Webhook 地址 token 部分、`secret`、`password`、`headers` 的值）及 `subscriptions:[{id,name,enabled}]`（引用它的订阅）。
- POST `/observability/channels`

| 字段 | 说明 |
|---|---|
| `name` | 必填，≤64，唯一 |
| `type` | `dingtalk｜feishu｜wecom｜email｜slack｜webhook`，创建后不可改 |
| `description` | 可选，≤256 |
| `config` | 见下表 |
| `enabled` | 默认 true |
| `severities` / `envs` | 渠道级告警兜底过滤（只作用于告警；空=全部） |
| `templates` | `{"alert":3,"incident":5}`，事件类型→模板 id；缺省用默认模板；模板事件类型必须一致 |
| `subscribe_alerts` | 仅创建时：同时生成订阅「告警 → 渠道名」，按 `severities/envs` 接收全部告警 |

`config`：

| 类型 | 字段 |
|---|---|
| 钉钉 / 飞书 / 企业微信 / Slack | `webhook`（https；白名单内地址可用 http）、`secret`（钉钉加签 / 飞书签名，可选） |
| 通用 Webhook | `webhook`（http/https，不得含账号密码）、`secret`（HMAC，可选）、`headers`（自定义请求头，≤20 个；禁止 Host、Content-Length、Content-Type、Transfer-Encoding、Connection 与 X-Matrix-* 签名头） |
| 邮件 | `use_system:true` 复用平台系统邮件（设置项 `mail.smtp`，只需 `to`）；或自定义 SMTP：`host`（不含协议/端口）、`port`（465 隐式 TLS，其他端口支持时 STARTTLS）、`username`、`password`、`from`、`to`（≤50） |

```json
{"name":"值班群","type":"feishu","config":{"webhook":"https://open.feishu.cn/open-apis/bot/v2/hook/xxx","secret":"可选"},"subscribe_alerts":true}
{"name":"工单","type":"webhook","config":{"webhook":"https://itsm.example.com/hooks/matrix","secret":"可选","headers":{"Authorization":"Bearer ..."}},"templates":{"alert":3}}
{"name":"值班邮箱","type":"email","config":{"use_system":true,"to":["oncall@example.com"]}}
```

- PUT `/observability/channels/:id`：`type` 不可改；`config` 省略=保留；`secret/password/webhook` 及同名请求头的值为空或等于掩码时保留原值；`templates` 省略=保留；只改启停时 `description` 留空即保留。修改了地址 / SMTP 主机时重新做出站地址校验。
- PUT `/observability/channels/:id/enabled`　`{"enabled":false}`：启用 / 停用。
- GET `/observability/channels/:id/references`：`{"subscriptions":[...]}`。
- DELETE `/observability/channels/:id`：被订阅引用时返回 409（`details.subscriptions`）；带 `?force=true` 时同一事务内从这些订阅移除该渠道，移除后没有渠道的订阅自动停用。历史发送记录保留。
- POST `/observability/channels/test`：用**未保存**的配置发测试消息（表单的「发送测试」）；带 `id` 时空/掩码密钥沿用已保存值。草稿测试不写发送记录。
- POST `/observability/channels/:id/test`：发测试消息并写发送记录（`event_kind=test`）。

两个测试接口返回 `{"ok":true,"duration_ms":120}` 或 `{"ok":false,"error":"robot error 310000: sign not match"}`（HTTP 200）；出站地址被策略拦截时返回 400。

发送实现：
- 钉钉：markdown；有 secret 时 URL 加 `timestamp` 与 `sign=Base64(HMAC-SHA256(secret, ts+"\n"+secret))`；校验 `errcode`。
- 飞书：text；有 secret 时 body 带 `timestamp` 与 `sign=Base64(HMAC-SHA256(key=ts+"\n"+secret, ""))`；校验 `code`。
- 企业微信：markdown，内容截断到 4000 字节；校验 `errcode`。
- Slack：Incoming Webhook `{"text":"*标题*\n正文"}`。
- 通用 Webhook：POST `{"source":"xingshu-matrix","channel","title","text","sent_at","event":{kind,status,key,alert_name,severity,env,cluster,namespace,resource,summary,starts_at,link,labels,id}}`；有 secret 时带 `X-Matrix-Signature: sha256=hex(HMAC-SHA256(secret, body))`（与旧版兼容）、`X-Matrix-Timestamp`（Unix 秒）、`X-Matrix-Signature-V2: sha256=hex(HMAC-SHA256(secret, timestamp+"."+body))`（防重放，建议接收方校验时间差 ≤5 分钟）。
- 邮件：纯文本 UTF-8 base64，主题 B 编码，头部去除 CR/LF 防注入；系统邮件走 `internal/mail.Send`（需启用且 TLS，内网中继需在系统邮件勾选「允许不加密连接」）。
- 所有 HTTP 发送 15 秒超时、**不跟随重定向**、不使用环境代理；错误信息中的 Webhook 地址会被掩码。

### 6.2 消息模板

Go `text/template`。变量：`.AlertName` `.Severity`（critical/warning/info）`.SeverityText` `.SeverityRaw`（故障 P1~P4）`.Status` `.StatusText` `.Kind` `.Env` `.EnvText` `.Cluster` `.Namespace` `.Resource` `.Summary` `.Description` `.StartsAt`（北京时间字符串）`.StartsAtT` `.Link` `.Actor` `.Labels` `.LabelsText` `.Count`；函数 `default upper lower trunc join date label`（如 `{{label "pod" .Labels}}`、`{{.Summary | trunc 100}}`）。模板 ≤16KB，输出 ≤32KB；保存时用示例数据执行校验。自定义模板渲染失败时自动退回默认模板并在正文末尾注明。

- GET `/observability/notify/templates?event_kind=`：`{"items":[...],"defaults":[{event_kind,name,title_tpl,body_tpl}],"statuses":{"alert":["firing","resolved"],...}}`。
- POST `/observability/notify/templates`：`{name, description, event_kind: alert|incident|pipeline|approval|test, channel_type: ""|类型, title_tpl, body_tpl}`。
- GET / PUT / DELETE `/observability/notify/templates/:id`：被渠道使用时不能改事件类型（409）；删除被引用模板返回 409（`details.channels`），`?force=true` 删除并让这些渠道改用默认模板。
- POST `/observability/notify/templates/preview`：`{event_kind, status?, channel_type?, template_id? | title_tpl+body_tpl, event?}` → `{title, body, payload(按渠道类型实际发送的 JSON，不含签名), variables}`；不传模板时预览默认模板，不传 `event` 时用示例数据。

### 6.3 订阅规则（路由）

- GET / POST `/observability/notify/subscriptions`，GET / PUT / DELETE `/observability/notify/subscriptions/:id`，PUT `/:id/enabled`。

| 字段 | 说明 |
|---|---|
| `name` | 必填，唯一 |
| `event_kinds` | 必填：`alert`、`incident`、`pipeline`、`approval` |
| `statuses` | 可选，须属于所选类型：告警 `firing resolved`；故障 `declared assigned acknowledged resolved`；流水线 `succeeded failed cancelled`；审批 `pending` |
| `severities` | `critical warning info`；故障 P1/P2→critical、P3→warning、P4→info；流水线失败→warning、其余→info；生产审批→warning |
| `envs` / `clusters` | 不区分大小写；告警集群取标签 `cluster`/`cluster_name`/`k8s_cluster`，流水线取执行集群 |
| `match_labels` | `{"namespace":"payment","pod":"~pay-.*","team":"!infra"}`：精确 / `~` 正则（全匹配）/ `!` 不等于；也可匹配 `cluster namespace env alertname severity` |
| `channel_ids` | 必填，渠道须存在 |
| `quiet` | `{enabled,start:"22:00",end:"08:00",weekdays:[1..5],timezone:"Asia/Shanghai",allow_critical}`；结束早于开始为跨天，跨天时段按开始那天判断星期 |
| `dedup_minutes` | 0~1440：同一事件（`key`）在该时间内对同一渠道只发一次（进程内 + 发送记录双重判断，跨副本有效） |
| `aggregate` | 同一批到达、命中同一规则同一渠道的多条事件合并为一条（最多展开 10 条） |

- POST `/observability/notify/subscriptions/match`：请求体为事件 `{kind,status,severity,env,cluster,namespace,labels,...}`，返回会命中的启用订阅及当前是否处于静默。

路由规则：同一事件命中多条订阅时每个渠道只发一次（按订阅 id 顺序，先命中者的静默/限频策略生效）；渠道停用即不发送；告警还要通过渠道级 `severities/envs`。被静默或限频的发送写一条 `suppressed` 记录。

**升级兼容**：迁移 0007 为每个已有渠道生成订阅「告警 → 渠道名」（复制原 `severities/envs`），升级后告警推送行为不变。

### 6.4 事件来源

| 事件 | 来源 | 说明 |
|---|---|---|
| 告警 | `notifyAlerts`（Alertmanager Webhook 新触发、健康检测、AIOps） | 保留原有自动升级故障与 AI 诊断摘要追加；`key=alert:<数据源>:<fingerprint>:<status>`；链接用 AIOps 配置的平台访问地址 |
| 故障 | 轮询 `oncall_incident_events`（declared/assigned/acknowledged/resolved） | 值班模块无需改动；note 不通知 |
| 审批待办 | 轮询 `approval_tickets` 新建且仍为 pending 的工单 | |
| 流水线结果 | 轮询 `cicd_runs` 已结束（succeeded/failed/cancelled） | 只读访问 CI/CD 表，表不存在时跳过 |

轮询间隔 15 秒，游标存 `obs_notify_cursors`，首次启动从当前位置开始（不补发历史），多副本通过比较并设置游标保证只有一个副本发送。发送记录保留 30 天（每小时清理）。

### 6.5 发送记录

- GET `/observability/notify/records`：筛选 `channel_id subscription_id resend_of event_kind status(success|failed|suppressed) env cluster severity channel_type event_status q(标题/渠道/原因/事件键) since until`（RFC3339 或 Unix 秒）、`page page_size`；返回 `{items,total,page,page_size,last_24h:{success,failed,suppressed}}`。列表不含正文。按调用者环境范围过滤（无环境的记录所有人可见）。
- GET `/observability/notify/records/:id`：含 `body`、`event`。
- POST `/observability/notify/records/:id/resend`：用渠道**当前**配置重发记录的标题/正文（被抑制的记录按事件重新渲染）；成功记录需 `?force=true`；渠道已删除/停用返回 409。返回 `{ok, record_id, duration_ms, error?}`，并写一条 `resend_of` 指向原记录的新记录。

### 6.6 通知安全（出站地址策略）

- GET / PUT `/observability/notify/security`：`{"block_private":true,"allow_hosts":["127.0.0.1:2525","10.0.0.0/16","hooks.intranet"]}`。存设置项 `notify.security`；PUT 需要不受限的环境范围（403）。未配置时默认 `block_private=true`、白名单为空。
- `block_private` 拦截回环、私有、链路本地、CGNAT(100.64/10)、未指定、组播、IPv6 ULA 等地址；云元数据地址 169.254.169.254 即使关闭开关也拦截，除非显式列入白名单。
- 白名单项：主机名、IP、`主机:端口`、CIDR（≤64 项）；白名单内地址允许 http 机器人地址。
- 校验分两层：保存/测试时解析并拒绝（400）；**发送时在拨号阶段对解析出的 IP 再校验并直连该 IP**（防 DNS 重绑定），自定义 SMTP 同样适用；系统邮件不受此策略限制。
- 沙箱联调：把本地收件桩加入白名单，例如 `PUT /observability/notify/security {"block_private":true,"allow_hosts":["127.0.0.1:2525","127.0.0.1:<HTTP 桩端口>"]}`。

### 错误码

| HTTP | code | 场景 |
|---|---|---|
| 400 | 40001 | 字段校验失败、模板语法/渲染错误、出站地址被拦截、正则不合法 |
| 403 | 40300 | 无 `notify:manage`；修改通知安全需要不受限环境；查看/重发不在环境范围内的记录 |
| 404 | 40400 | 渠道 / 模板 / 订阅 / 记录不存在 |
| 409 | 40900 | 名称重复；删除被引用的渠道/模板未带 `force`；重发到已删除/停用渠道；成功记录重发未带 `force` |

### 未验证 / 限制

- 未在真实 PostgreSQL 上运行：`TestNotifyCenterPostgres` 需设置 `MATRIX_TEST_DATABASE_URL`；钉钉/飞书/企业微信/Slack 真实机器人未联调（单元测试用本地 HTTP 桩）。
- 告警**恢复**通知需要告警入口把已恢复告警也传入 `notifyAlerts`（当前 Webhook 只传新触发告警），模板与路由已支持 `resolved`。
- 故障 / 流水线 / 审批为轮询接入（最长约 15 秒延迟）；流水线读取 CI/CD 模块表结构，CI/CD 表结构变化时需同步调整。更理想的挂钩点见交付报告。
- 聚合只合并同一批到达的事件，不做跨时间窗口的延迟聚合；没有自动重试（失败可手动重发）。

---

## 7. AI 工具（只读）

| 名称 | 权限 | 参数 | 说明 |
|---|---|---|---|
| `promql_query` | metrics:read | `datasource, query, start?, end?, step?` | 有 start 为区间查询，否则即时查询 |
| `logs_query` | logs:read | `datasource, query, from?, to?, limit?(≤200,默认50)` | 输出去掉 labels、消息截断 1000 字符 |
| `alerts_list` | alert:read | `status?, handle_status?, severity?, q?, limit?` | 按调用者环境范围过滤 |

工具处理器从 `rbac.FromContext(ctx)` 取调用者并做环境校验。

---

## 未实现 / 限制

- 未在 `registry.go` 注册（按规范由集成方添加 `observability.New()`）。
- 告警规则只读（不支持通过平台写 Prometheus 规则文件 / PrometheusRule CRD）。
- 静默/告警处理未走生产审批 Gate（视为运维操作，仅 RBAC + 审计）。
- 认领/关闭仅在平台侧记录，不回写 Alertmanager。通知中心的限制见第 6 节末尾。
- ES 查询为 `query_string` 近似 KQL，不支持 KQL 的嵌套字段语法 `a:{b:c}`、范围 `>`/`<` 等需写成 Lucene `field:[a TO b]`；不支持 ES 的 API Key 认证（可用 bearer）。
- Loki metric 类 LogQL 不支持（拒绝）；不支持 live tail。
- Tempo 搜索摘要不含 span 数和错误标记；未支持 Zipkin、SkyWalking。
- 数据源无“保存前测试”接口（需先创建再 `/test`）。
- 数据库相关（webhook upsert、CRUD）无集成测试；单元测试覆盖 Prometheus 校验/代理、Loki/ES 归一化与 KQL 转换、Tempo OTLP 解析、钉钉加签与错误码。

---

## 8. 证书到期提醒（v1.29 g-cert / h-cert-fe）

后端：`server/internal/modules/observability/cert_watch.go`；迁移 `0010_cert_watch`（`obs_cert_domains`、`obs_cert_status`、`obs_cert_watch_config`），提醒去重表 `obs_cert_notices` 来自 `0008_alerting_logs`。
前端：`public/xs-cert-watch.js`（`window.XSCertWatch.render(el)`），页面入口「可观测 → 证书到期」（`#/observe/certificates`，与通知里的「查看」链接一致）。

扫描对象：
- 所有已登记集群中 `type=kubernetes.io/tls` 的 Secret，只解析 `tls.crt` 的叶子证书；`tls.key` 不读取、不落库、不出现在接口和日志中。单集群最多 5000 个，单集群超时 45 秒；某集群读取失败时保留该集群上次快照，并在 `last_result.cluster_errors` 记录原因。
- 用户配置的域名：TLS 握手读取对端证书（跳过校验，以便读取已过期/自签证书），握手后立即断开，不发送应用数据；单个探测超时 8 秒，并发 8，最多 500 个域名。

### 接口（均在 `/api/v1/observability` 下）

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/certificates?source=&status=&q=` | `alert:read` | 证书列表；返回 `{items, counts, settings}` |
| POST | `/certificates/scan` | `notify:manage` | 立即同步扫描（最长约 2 分钟），返回扫描结果；有扫描进行中返回 409 |
| GET | `/certificates/settings` | `alert:read` | 扫描开关、周期、阈值、上次扫描结果 |
| PUT | `/certificates/settings` | `notify:manage` + 全部环境权限 | 修改设置；受限环境用户返回 403 |
| GET | `/certificates/domains` | `alert:read` | 监测域名列表（按环境范围过滤，未指定环境的域名所有人可见） |
| POST | `/certificates/domains` | `notify:manage` | 添加域名并立即探测一次；返回 `{domain, probe}`（201） |
| DELETE | `/certificates/domains/:id` | `notify:manage` | 删除域名，同时删除该域名的证书快照 |

查询参数（列表）：`source`=`secret`|`domain`；`status`=`ok`|`warning`|`critical`|`expired`|`error`；`q` 不区分大小写匹配 名称/命名空间/集群名/域名。`counts` 在关键字与来源过滤之后、状态过滤之前统计（用于状态计数标签）。列表按环境范围过滤（`env` 为空的域名证书对所有有读权限的人可见），最多 20000 行，按到期时间升序（未知到期时间在前）。

证书条目字段（`items[]`）：

| 字段 | 说明 |
|---|---|
| `source` | `secret`（集群 TLS Secret）/ `domain`（域名探测） |
| `cluster_id` / `cluster_name` / `env` | 集群信息；域名来源 `cluster_id=0`、`cluster_name` 为空，`env` 为域名配置的环境 |
| `namespace` / `name` | Secret 的命名空间与名称；域名来源 `namespace` 为空，`name` 为 `host:port` |
| `subject` / `issuer` / `dns_names` | 证书主题、签发者、SAN 域名（逗号分隔，最多 20 个） |
| `not_after` | 到期时间（UTC，RFC3339）；读取失败时为 null |
| `days_left` | 剩余整天数（向下取整，过期为负数）；读取失败时为 null |
| `status` | `expired`（<0 天）、`critical`（≤7 天）、`warning`（≤最大阈值）、`ok`、`error`（读取/探测失败） |
| `error` | 失败原因（中文，如「域名无法解析」「连接超时」「TLS 握手失败」「未找到 PEM 格式证书」） |
| `scanned_at` | 本条快照写入时间 |

设置字段：`enabled`（bool，默认 true）、`interval_hours`（1–168，默认 6）、`thresholds`（1–10 个，每个 1–365 天，去重后降序，默认 `[30,14,7,1]`）、`last_scan_at`、`last_result`（`trigger` schedule|manual、`started_at`、`duration_ms`、`clusters`、`cluster_errors`、`secrets`、`domains`、`domain_errors`、`notices`、`notice_error`）、`updated_by`、`updated_at`。PUT 只接收 `enabled`、`interval_hours`、`thresholds`。

域名输入：`host`（必填；去掉 `http(s)://`、路径、末尾点，转小写；可写成 `host:port`；支持 IP；不支持通配符 `*.`）、`port`（默认 443，1–65535，与 host 中端口不一致时报错）、`env`（≤32 字符；为空需要全部环境权限，否则需有该环境权限）、`remark`（≤128 字符）。`(host, port)` 唯一。

手动扫描结果仅对全部环境权限用户返回 `cluster_errors`（受限用户看不到无权集群的错误）。

### 定时扫描与提醒规则

- 每分钟检查一次；`enabled=true` 且距上次扫描超过 `interval_hours` 时，通过对 `last_scan_at` 的条件更新抢占，多副本只有一个执行；同一进程内扫描互斥。
- 每张证书以（集群、来源、命名空间、名称、`not_after`）为身份。扫描后取证书已跨过的**最小**阈值作为本次提醒阈值（已过期为 0），在 `obs_cert_notices` 插入成功才发送；同时把已跨过的较大阈值一并记为已发送，因此每张证书每个阈值最多提醒一次，且不会在跨过 7 天后再补发 30 天提醒。证书续期后 `not_after` 变化，重新计数。
- 提醒通过通知中心发送，事件类型 `certificate`，状态 `expiring` / `expired`；剩余 ≤7 天或已过期为 `critical`，否则 `warning`。默认模板「默认证书到期模板」，标签含 `source`、`days_left`、`not_after`、`threshold`、`dns_names`。配置了平台访问地址时附带链接 `<平台地址>/#/observe/certificates`。需要在订阅规则中勾选「证书」事件才会投递到渠道。
- 修改阈值不会删除已有去重记录：例如原阈值 30 已发送，新增 20 后，对同一证书仍会在跨过 20 天时再提醒一次（20 < 30 的新最小阈值）。

### 审计

`POST /scan`（资源 `obs_cert_watch/scan`，记录集群/证书/域名/提醒数）、`PUT /settings`（`obs_cert_watch/settings`）、`POST /domains`、`DELETE /domains/:id`（`obs_cert_domain/<id>`）。

### 错误码

| HTTP | code | 场景 |
|---|---|---|
| 400 | 40001 | 域名/端口/环境/备注校验失败；扫描周期或阈值越界 |
| 403 | 40300 | 无 `alert:read` / `notify:manage`；受限环境用户修改设置、添加未指定环境的域名、操作无权环境的域名 |
| 404 | 40400 | 删除的域名不存在 |
| 409 | 40900 | 已有扫描进行中；域名重复；超过 500 个域名 |

### 前端（`xs-cert-watch.js`）

- 证书列表：名称（来源、证书主题）、命名空间 / 域名（含 SAN）、集群（域名显示「外部域名」，含环境）、到期时间（含签发方）、剩余天数、状态中文标签（已过期 / 紧急 / 即将到期 / 正常 / 读取失败）；按来源、状态、关键字筛选；状态计数标签可点击筛选；显示上次扫描摘要与失败集群。
- 「立即扫描」（`notify:manage`，请求超时放宽到 150 秒）；添加域名表单就地校验（与后端规则一致）；删除域名需输入域名二次确认；提醒设置（开关、周期、阈值）就地校验。只读用户只看列表、域名与设置。
- 测试：`node --test tests/frontend/xs-cert-watch.test.mjs`。

### 未验证 / 限制

- 未在真实 PostgreSQL 与真实集群上联调（前端只用桩接口测试；本次未连到运行中的实例）。
- 手动扫描为同步请求，集群多、域名多时可能接近 2 分钟上限；网关/反向代理超时需大于 150 秒，否则前端会看到超时但扫描仍在后台完成。
- 只读取 Secret 中的叶子证书，不检查中间证书到期；不扫描 cert-manager `Certificate` 资源状态；不支持 SNI 与连接地址不同的探测；通配符域名不能直接探测（需填写具体子域名）。
- 列表过滤在服务端内存中完成（最多 20000 行），无分页。
- 删除集群后，该集群证书快照在下次扫描时清理；去重记录 `obs_cert_notices` 不自动清理。

## 9. 列表行内迷你趋势图（v1.31 u-spark）

### GET /observability/metrics/sparklines　`workload:read`（`kind=node` 另需 `node:read`）
批量返回近 30 分钟 CPU / 内存曲线，供工作负载、Pod、节点列表的「近 30 分钟」列使用。代码：`sparklines.go`。

| 参数 | 必填 | 说明 |
|---|---|---|
| `cluster_id` | 是 | 集群 ID（正整数） |
| `kind` | 是 | `pod` \| `node` \| `workload` |
| `workload_kind` | `kind=workload` 时 | `deployments`（默认）\| `statefulsets` \| `daemonsets` \| `jobs` \| `cronjobs` |
| `namespace` | pod/workload 时二选一 | 默认命名空间；`names` 中也可写 `命名空间/名称`（跨命名空间列表） |
| `names` | 是 | 逗号分隔，去重后 ≤ 50 个；名称按 DNS-1123 校验（节点允许大写与下划线），不接受正则 |
| `ips` | 否 | 仅 `kind=node`：与 `names` 等长的节点 IP 列表（可留空项），用于匹配 node_exporter 的 `instance` |

固定窗口：结束时间按分钟取整，`start = end - 30m`，步长 60 秒（最多 31 点）。整次查询超时 10 秒（另受数据源 `timeout_sec` 限制）。

数据源解析与总览 24 小时趋势一致（`selectSource`：集群专属源优先，其次唯一的中央源；中央源自动为每个选择器追加精确集群标签）。PromQL 不使用 `by()/on()`，原始序列取回后在服务端聚合：

| kind | CPU | 内存 | 单位 `unit` |
|---|---|---|---|
| `pod` | `rate(container_cpu_usage_seconds_total{namespace,pod,container!="",container!="POD"}[5m])` 按 Pod 求和 | `container_memory_working_set_bytes{…}` 按 Pod 求和 | `cores` / `bytes` |
| `workload` | 同上，`pod=~"(?:名称…)<控制器后缀>"`，再按 Pod 名映射回工作负载（名称互为前缀时取最长匹配） | 同上 | `cores` / `bytes` |
| `node` | `rate(node_cpu_seconds_total{mode="idle"}[5m])` 各核均值换算为使用率 | `100 * (1 - MemAvailable / MemTotal)` | `percent` / `percent` |

节点映射顺序：`ips` 与 `instance` 主机部分 → 序列上的 `node`/`nodename`/`kubernetes_node` 标签 → `node_uname_info{nodename}`。

响应：
```json
{"source":"prometheus","kind":"workload","workload_kind":"deployments","start":1763346600,"end":1763348400,"step":60,
 "unit":{"cpu":"cores","mem":"bytes"},
 "items":[{"name":"api","cpu":[[1763346600,0.12],[1763346660,0.13]],"mem":[[1763346600,73400320]]},
          {"name":"web","cpu":[],"mem":[]}]}
```
- `items` 与请求名称一一对应、顺序一致，`name` 原样回显（含 `命名空间/名称` 写法）；无采样的对象返回空数组。
- 集群未绑定 Prometheus：HTTP 200，`source:"none"`，全部空数组，`note:"集群未接入监控数据源"`。

错误码：400（参数无效、超过 50 个、缺命名空间、IP 数量不一致）；401；403（无集群/命名空间权限、节点趋势缺 `node:read`、环境范围不符）；404（集群不存在）；409（集群绑定了多个同级数据源）；502（Prometheus 不可达或 CPU、内存查询均失败）。只有一条查询失败时返回成功的那条，另一条为空数组。

未验证 / 限制：
- 未连真实 Prometheus 联调（单测用假 Prometheus 服务）；cAdvisor / node_exporter 标签按 kube-prometheus-stack 默认假设。
- 工作负载按 Pod 命名规则匹配，未读取 ownerReferences；自定义 Pod 名（如裸 Pod 或改写名称的控制器）不会计入。
- 同一容器被多个抓取任务重复采集时，Pod 级求和会偏大（节点内存已取均值规避）。
- 不读取审计（只读查询）；平台没有历史采样存储，无 Prometheus 时不回退 metrics-server。
