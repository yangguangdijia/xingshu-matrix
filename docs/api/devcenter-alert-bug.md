# 研发中心 · 告警转缺陷（v1.32）

> 实现：server/internal/modules/devcenter/alert_bug.go；前端 public/xs-alert2bug.js（window.XSAlert2Bug）。
> 本节可由主代理合并进 automation-devcenter.md 的研发中心部分。

## POST /api/v1/devcenter/bugs/from-alert

权限：`devcenter:manage`。写审计（resource=devcenter_bugs，detail 含 from_alert / alertname / cluster / namespace / suspect_releases 数量）。

请求：

| 字段 | 类型 | 说明 |
|---|---|---|
| alert_id | int64 | 必填（或给 incident_id）。obs_alerts.id |
| incident_id | int64 | 可选扩展：未给 alert_id 时取该故障最早关联的告警（oncall_alert_links） |
| dry_run | bool | 可选扩展：只预览，不创建，不写审计 |

返回（创建 201 / 已存在 200）：缺陷对象（与 GET /devcenter/bugs/:id 字段相同）外加
`existing`（bool，告警此前已转过缺陷时为 true，返回已有缺陷）与 `suspect_releases`（数组）。

`dry_run:true` 返回 200：`{preview:true, title, suspect_releases, existing, bug?, source}`。

缺陷内容：
- 标题：`[告警] <alertname> · <集群>/<命名空间>`（缺失时为「未知集群」「-」，最多 200 字）；
- 描述（Markdown 文本）：告警摘要 / 说明、告警信息（级别、状态、首次触发、集群、命名空间、应用、指纹）、告警详情链接 `#/observe/events/<id>`、https 处理手册、全部标签、可疑发布；
- 优先级：critical→P1，warning/error→P2，info→P3；data.severity：critical / major / minor；data.env 由告警 env 标签或数据源环境映射为 prod/pre/test/dev（无法映射时不填）；
- data.source：`{type:"alert", alert_id, alertname, cluster, cluster_id, namespace, app, severity, fingerprint, fired_at, alert_url}`；
- data.suspect_releases：`[{release_id, app_id, app_name, env, image, previous_image, kind, status, created_by, created_at, minutes_before, score, reasons[]}]`，最多 10 条。

可疑发布启发式（cicd_releases 中 `首次触发-24h ≤ created_at ≤ 首次触发`，最多扫描 500 条）：
- 告警应用名候选：标签 app、app_kubernetes_io_name、app_kubernetes_io_instance、k8s_app、service、deployment、statefulset、daemonset、workload、container，以及 pod 名去掉 Deployment / StatefulSet 后缀；job 标签不使用（多为采集任务名）；
- 应用名（cicd_applications.name、该环境 deployment 或 helm release）命中候选 +3；该发布环境命名空间 = 告警 namespace +2；执行目标集群（cicd_targets.cluster_id → cluster_clusters.name）= 告警集群 +1；
- 两边都已知但命名空间或集群不同则排除；至少命中应用名或命名空间之一；按得分、时间倒序。
- 告警集群取 labels.cluster → labels.cluster_id / 数据源 options.cluster_id（查 cluster_clusters 名称）→ 数据源 options.cluster_name。

幂等：按 `devcenter_bugs.data->'source'->>'alert_id'` 查找；同一告警的并发请求以 PostgreSQL 会话级咨询锁串行化。

关联：devcenter_links 已是价值流结构（含 item_type/ref/occurred_at/source 等列，vs-trace 的 0004 迁移）时，写入一条
`item_type='bug', kind='alert', ref=<alert_id>, url=#/observe/events/<id>, source='auto'`，ON CONFLICT DO NOTHING；失败只记日志，不影响创建。

错误：400/422 请求格式或 alert_id 缺失（「请指定要转为缺陷的告警」）；403 无权限；404 告警不存在（「告警不存在或已被清理」）/ 故障无关联告警；500 其他。

未验证：真实 PostgreSQL 下的整体流程（无测试库）、与 vs-trace 0004 迁移的列与 item_type 取值对齐。
