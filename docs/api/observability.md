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
| `notify:manage` | 通知渠道管理与测试 |

环境范围：每个数据源有 `env` 标签，所有按数据源的操作都校验 `Principal.CanEnv(env)`；告警列表按用户环境范围过滤。

## 数据表

- `obs_datasources`：类型 `prometheus|loki|elasticsearch|alertmanager|jaeger|tempo`、url、env、`auth_type none|basic|bearer`、username、`secret_enc`（AES-GCM 加密的密码或 Token）、`options`（JSON，见下）、`webhook_token_hash`（SHA-256，仅 alertmanager）。
- `obs_alerts`：按 `(datasource_id, fingerprint)` 唯一，`status firing|resolved`，`handle_status open|acked|closed`。
- `obs_channels`：`type dingtalk|feishu|wecom|email`，`config_enc` 整体加密，`severities`/`envs` 过滤（空=全部）。

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
处理：按 `fingerprint` upsert（缺失时用排序后标签的 SHA-256 前 16 位）；`resolved→firing` 重新打开（handle_status=open、fire_count+1、清空认领人）；**首次出现的 firing 或 resolved→firing** 异步推送到所有匹配的启用通知渠道；重复的 firing 不重复推送。
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

## 6. 通知渠道　（全部 `notify:manage`）

- GET `/observability/channels`、GET `/observability/channels/:id`（敏感信息掩码）
- POST `/observability/channels`
```json
{"name":"运维钉钉群","type":"dingtalk","config":{"webhook":"https://oapi.dingtalk.com/robot/send?access_token=xxx","secret":"SECxxx"},"enabled":true,"severities":["critical","warning"],"envs":["prod"]}
{"name":"飞书","type":"feishu","config":{"webhook":"https://open.feishu.cn/open-apis/bot/v2/hook/xxx","secret":"可选"}}
{"name":"企业微信","type":"wecom","config":{"webhook":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"}}
{"name":"邮件","type":"email","config":{"host":"smtp.example.com","port":465,"username":"alert@example.com","password":"***","from":"alert@example.com","to":["ops@example.com"]}}
```
- PUT `/observability/channels/:id`：`type` 不可改；`config` 省略=保留；`secret/password/webhook` 为空或等于掩码值时保留原值。
- DELETE `/observability/channels/:id`
- POST `/observability/channels/:id/test`：发送测试消息，返回 `{"ok":true}` 或 `{"ok":false,"error":"robot error 310000: sign not match"}`。

发送实现：
- 钉钉：markdown 消息；有 secret 时加签 `timestamp`+`sign=Base64(HMAC-SHA256(secret, ts+"\n"+secret))`；校验 `errcode`。
- 飞书：text 消息；有 secret 时 `sign=Base64(HMAC-SHA256(key=ts+"\n"+secret, ""))`；校验 `code`。
- 企业微信：markdown 消息；校验 `errcode`。
- 邮件：`net/smtp`，465 端口隐式 TLS，其他端口在服务器支持时 STARTTLS，有用户名时 PLAIN 认证；UTF-8 base64 正文。
- 触发：Webhook 收到新 firing 告警后后台 goroutine 发送（2 分钟超时），按渠道 `severities`、`envs` 过滤；失败写 warn 日志。

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
- 认领/关闭仅在平台侧记录，不回写 Alertmanager；未实现告警升级、通知重试、限流、聚合合并及恢复（resolved）通知。
- ES 查询为 `query_string` 近似 KQL，不支持 KQL 的嵌套字段语法 `a:{b:c}`、范围 `>`/`<` 等需写成 Lucene `field:[a TO b]`；不支持 ES 的 API Key 认证（可用 bearer）。
- Loki metric 类 LogQL 不支持（拒绝）；不支持 live tail。
- Tempo 搜索摘要不含 span 数和错误标记；未支持 Zipkin、SkyWalking。
- 数据源无“保存前测试”接口（需先创建再 `/test`）。
- 数据库相关（webhook upsert、CRUD）无集成测试；单元测试覆盖 Prometheus 校验/代理、Loki/ES 归一化与 KQL 转换、Tempo OTLP 解析、钉钉加签与错误码。
