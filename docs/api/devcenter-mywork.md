# 研发中心 · 我的交付

## GET /api/v1/devcenter/my/delivery

当前登录用户负责（`assignee` = 用户名）或创建（`created_by` = 用户 ID）的需求与缺陷，以及它们在交付价值流（代码 → 构建 → 制品 → 发布 → 环境）中走到的阶段。

- 权限：`devcenter:read`
- 范围：未结束的全部；已结束（需求 done/rejected、缺陷 verified/closed）只取近 30 天内更新过的。每类最多 200 条。
- 排序：受阻置顶，其余按更新时间倒序。

### 响应

```json
{
  "requirements": [MyItem],
  "bugs": [MyItem],
  "reviews": [],
  "counts": { "in_dev": 1, "in_build": 2, "released": 3, "blocked": 1 }
}
```

MyItem（契约字段 + 扩展字段）：

| 字段 | 说明 |
|---|---|
| id, title, status, priority, assignee | 条目基本信息 |
| stage | 走到的最远阶段：`code` `build` `artifact` `release` `env`，未进入价值流为 `none` |
| stage_state | 该阶段状态：`done` `running` `failed` `none` |
| env_latest | 最近一次成功发布的环境（无则空） |
| stages | 扩展：五个阶段各自的状态，供前端画阶段条 |
| blocked, blocked_reason | 扩展：是否受阻及原因 |
| bucket | 扩展：`dev` 开发中 / `build` 构建中 / `released` 已上线 / `blocked` 受阻 / `closed` 已关闭（不计数） |
| released_at | 扩展：上线时间（最近成功发布时间；按状态推断时取更新时间） |
| updated_at | 条目更新时间 |
| stage_source | 扩展：`links`（按价值流关联计算）或 `status`（按状态推断） |

`reviews`：待我评审的合并请求。当前没有评审人数据源，固定为空数组。

### 阶段计算

1. `devcenter_links` 为价值流版本（含 `item_type` 列，0004_value_stream 迁移后）且条目有关联时：
   - 提交 / 已合并的合并请求 → 代码完成；未合并的合并请求 → 代码进行中；
   - 最近一次运行 → 构建（成功 / 运行中 / 失败）；有任一成功运行 → 制品完成；
   - 最近一次发布 → 发布；有成功发布 → 环境完成，`env_latest` 取最近一次成功发布的环境；
   - 运行与发布状态优先读 `cicd_runs` / `cicd_releases` 的当前值（只读，表存在时），否则用关联里记录的状态；
   - 后面的阶段已到达时，前面没有记录的阶段视为完成。
2. 否则按条目状态推断（近似）：需求 doing → 代码进行中，testing/accepting → 发布进行中，done → 环境完成；缺陷 doing/reopened → 代码进行中，fixed → 代码完成，verified/closed → 环境完成。

受阻：状态为 `blocked`、`data.blocked=true`（原因取 `data.blocked_reason`），或未结束的条目最近一次运行 / 发布失败。

### 错误

- 401 未登录；403 无 `devcenter:read`；500 数据库错误。

### 未验证

- 未在真实 PostgreSQL 上跑过 SQL（单元测试只覆盖纯函数）。
- 「环境完成」只表示曾成功发布，不校验该版本当前是否仍在该环境运行。
