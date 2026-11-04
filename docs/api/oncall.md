# 值班与故障 API（/oncall）

v1.26 新增。分层轮值排班、代班、当前值班计算、故障声明 / 认领 / 解决 / 时间线、SLO 与错误预算。
实现位于 `server/internal/modules/observability/oncall.go`，迁移 `observability/migrations/0005_oncall.up.sql`。

## 权限

| 权限 | 说明 |
|---|---|
| `oncall:read` | 查看排班、当前值班、故障、SLO 与错误预算 |
| `oncall:manage` | 管理排班 / 轮值 / 代班，声明与处理故障，维护 SLO 与手工 SLI |

带 `env` 的排班、故障与 SLO 受用户环境范围限制（`env` 为空表示全局）。

## 排班

### GET /oncall/schedules
`{"items":[{"id":1,"name":"core-api","description":"","env":"prod","timezone":"Asia/Shanghai","enabled":true,...}],"total":1}`

### POST /oncall/schedules、PUT /oncall/schedules/:id
```json
{"name":"core-api","description":"核心 API 值班","env":"prod","timezone":"Asia/Shanghai","enabled":true}
```
`name` 唯一；`timezone` 必须是合法 IANA 时区（仅用于展示）。

### GET /oncall/schedules/:id
`{"schedule":{...},"rotations":[...],"overrides":[...]}`（代班只返回近 7 天内仍相关的）

### DELETE /oncall/schedules/:id
级联删除轮值层与代班；关联故障的 `schedule_id` 置空。

### PUT /oncall/schedules/:id/rotations/:layer
同一层再次保存为整体替换。`layer` 1 = 一线，2–9 = 升级层。
```json
{"name":"一线","start_at":"2026-11-03T01:00:00Z","length_seconds":86400,"members":["alice","bob","carol"]}
```
`length_seconds` 至少 3600；`members` 顺序即轮值顺序。

### DELETE /oncall/schedules/:id/rotations/:layer

### POST /oncall/schedules/:id/overrides
```json
{"layer":1,"username":"dave","starts_at":"2026-11-05T01:00:00Z","ends_at":"2026-11-06T01:00:00Z","reason":"休假"}
```
区间左闭右开；同一层多条代班重叠时，最晚开始的生效（开始时间相同取 id 最大者）。

### DELETE /oncall/overrides/:id

## 当前值班

### GET /oncall/current?schedule_id=
计算规则：`k = floor((now - start_at) / length)`，值班人 = `members[k mod n]`，班次 `[start_at + k·length, +length)`；有生效代班时覆盖。
```json
{"items":[{"schedule_id":1,"schedule":"core-api","env":"prod","layer":1,"username":"zhang.san",
  "starts_at":"...","ends_at":"...","override":false,"next_username":"li.si","next_starts_at":"..."}]}
```

### GET /oncall/overview
`{"current":[...],"incidents":{"open":0,"acknowledged":1,"resolved":3},"recent_incidents":[最近 5 条],"slos":[{"id":1,"name":"...","env":"..."}]}`

## 故障

状态机：`declared`（已声明）→ `acknowledged`（处理中）→ `resolved`（已解决）；`declared` 也可直接解决。每次变更写入时间线。

### GET /oncall/incidents?status=&severity=&q=&page=1&page_size=50
`q` 匹配标题与服务。返回 `{"items":[...],"total":N,"page":1,"page_size":50}`。

### POST /oncall/incidents
```json
{"title":"订单服务 5xx 升高","description":"","severity":"P2","service":"order-api","env":"prod","schedule_id":1,"assigned_to":""}
```
`severity` 取 P1–P4（默认 P3）。给出 `schedule_id` 且未指定 `assigned_to` 时，自动指派给该排班当前层级最小（一线）的值班人。

### GET /oncall/incidents/:id
`{"incident":{...},"timeline":[{"kind":"declared|assigned|acknowledged|note|resolved","actor":"admin","message":"...","created_at":"..."}]}`

### POST /oncall/incidents/:id/ack —— `{"comment":"正在排查"}`
仅 `declared` 可认领，否则 409；未指派时认领人成为处理人。

### POST /oncall/incidents/:id/resolve —— `{"note":"扩容后恢复"}`
已解决再次提交返回 409。

### POST /oncall/incidents/:id/notes —— `{"message":"扩容连接池到 200"}`

## SLO

### GET /oncall/slos、GET /oncall/slos/:id
### POST /oncall/slos、PUT /oncall/slos/:id
```json
{"name":"订单 API 可用性","service":"order-api","env":"prod","description":"",
 "target":99.9,"window_days":30,"source":"manual","sli_type":"ratio",
 "datasource_id":null,"good_query":"","total_query":"","query":""}
```
- `target` ∈ (0, 100]，`window_days` 1–365。
- `source=manual`：通过手工 SLI 条目录入 good / total。
- `source=prometheus`：必须给出 Prometheus 类型的 `datasource_id`；`sli_type=ratio` 需 `good_query` 与 `total_query`，`sli_type=query` 需返回 0–1 比率的 `query`。查询中的 `$window` 会替换为窗口长度（如 `30d`），例如 `sum(increase(http_requests_total{code!~"5.."}[$window]))`。

### DELETE /oncall/slos/:id

### GET /oncall/slos/:id/entries、POST /oncall/slos/:id/entries、DELETE /oncall/slos/:id/entries/:entry
```json
{"period_start":"2026-11-01T00:00:00Z","period_end":"2026-11-02T00:00:00Z","good":99990,"total":100000,"note":""}
```
仅 `manual` 来源可录入；要求 `0 ≤ good ≤ total`、`total > 0`、结束晚于开始。

### GET /oncall/slos/:id/budget
```json
{"slo":{...},"source":"manual","from":"...","to":"...",
 "budget":{"sli":0.99955,"target":99.9,"remaining_pct":55.0,"burn_rate":4.5,"healthy":false,"has_data":true,"good":199910,"total":200000},
 "points":[{"ts":"...","remaining_pct":90.0}]}
```
- 允许失败率 = 1 − target/100；预算消耗 = 实际失败率 / 允许失败率；`remaining_pct` = (1 − 消耗) × 100，可为负（超支）。
- `burn_rate` = 消耗 ÷ 已经过的窗口比例（manual 以第一条条目开始计）。大于 1 表示按当前速度窗口结束前会耗尽预算。
- `healthy` = 剩余 > 0 且燃烧率 ≤ 1。窗口内没有数据时 `has_data=false`，预算视为未消耗。
- `points` 为 manual 来源按条目累计的剩余预算曲线；prometheus 来源为即时值，不返回曲线。
