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
| `alert:handle` | 认领 / 关闭告警 |
| `alert:silence` | 创建 / 删除静默 |
| `trace:read` | 链路查询 |
| `notify:manage` | 通知中心：渠道、消息模板、订阅规则、发送记录与重发、通知安全策略（全部接口同一权限） |

环境范围：每个数据源有 `env` 标签，所有按数据源的操作都校验 `Principal.CanEnv(env)`；告警列表按用户环境范围过滤。

## 数据表

- `obs_datasources`：类型 `prometheus|loki|elasticsearch|alertmanager|jaeger|tempo`、url、env、`auth_type none|basic|bearer`、username、`secret_enc`（AES-GCM 加密的密码或 Token）、`options`（JSON，见下）、`webhook_token_hash`（SHA-256，仅 alertmanager）。
- `obs_alerts`：按 `(datasource_id, fingerprint)` 唯一，`status firing|resolved`，`handle_status open|acked|closed`。
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
请求体可选 `{"comment":"已扩容"}`，响应 `{"id":10,"handle_status":"closed","closed_by":"alice"}`。关闭后若 Alertmanager 再次发送 resolved→firing，会自动重新打开。

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
