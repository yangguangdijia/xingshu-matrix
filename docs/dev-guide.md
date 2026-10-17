# 星枢 Matrix 后端开发指南

## 环境

- Go 1.23（`server/go.mod`，模块名 `github.com/yangguangdijia/xingshu-matrix/server`）
- PostgreSQL 14+（必需）、Redis 7（可选）、MinIO/S3（录像/制品，可选）
- 本地运行：`cd server && MATRIX_MODE=dev MATRIX_DB_DSN=postgres://... go run ./cmd/matrix-server`
- 子命令：`matrix-server serve | migrate | version`，配置 `-config` 或 `MATRIX_CONFIG`

## 目录

```
server/
  cmd/matrix-server/        进程入口：配置、迁移、服务装配、优雅退出
  internal/server/          路由装配（中间件、/healthz、/metrics、核心路由、模块路由、SPA 回退）
  internal/config           YAML + MATRIX_* 环境变量
  internal/store            pgx 连接池、嵌入式迁移（按模块记录版本、advisory lock）
  internal/httpx            统一响应 {code,message,data,request_id}、错误码、分页
  internal/crypto           AES-256-GCM 加密（kubeconfig、主机密码、API Key）
  internal/auth             登录、JWT、刷新令牌轮换、锁定、强制改密
  internal/rbac             角色/权限（resource:action，支持通配）、环境策略
  internal/audit            写操作审计中间件 + 查询
  internal/approval         审批单 + Gate（生产环境写操作）
  internal/ai               OpenAI 兼容模型代理、提示词模板、Function Calling 工具注册
  internal/users, settings  用户管理、系统设置
  internal/app              模块契约（Deps / Module）
  internal/modules/<name>   业务模块（cluster、hosts、bastion、observability、cicd、middleware）
```

## 新增一个模块

1. 新建 `internal/modules/<name>/`，实现 `app.Module`：

```go
type Module struct{ /* 依赖 */ }
func New() *Module { return &Module{} }
func (m *Module) Name() string        { return "cluster" }          // 小写，永不改名（迁移历史按名记录）
func (m *Module) Migrations() fs.FS   { sub, _ := fs.Sub(migFS, "migrations"); return sub } // //go:embed migrations/*.sql
func (m *Module) Register(r *gin.RouterGroup, d *app.Deps) {
    g := r.Group("/clusters")
    g.GET("", rbac.RequirePerm("cluster:read"), m.list)
    g.POST("", rbac.RequirePerm("cluster:manage"), m.create)
}
```

2. 可选接口：
   - `Permissions() []rbac.Permission` —— 用 `rbac.P("cluster","cluster","read","查看集群")` 声明，角色编辑器可见。
   - `Start(ctx, *app.Deps) error` —— 后台任务（Informer、Agent Hub、调度器），ctx 在退出时取消。
   - `Stop(ctx) error` —— 清理。
3. 在 `internal/modules/registry.go` 的 `All()` 中加入 `<name>.New()`。
4. 迁移文件：`migrations/0001_init.up.sql`，表名以模块名为前缀（如 `cluster_clusters`）。

## 约定

- 响应：成功 `httpx.OK(c, data)` / `httpx.Created`；失败 `httpx.Fail(c, err)`，错误用 `httpx.BadRequest/NotFound/Forbidden/Conflict/Internal`。
- 请求体：`if !httpx.Bind(c, &in) { return }`；分页：`p := httpx.ParsePage(c)`，返回 `httpx.NewPageResult(items, total, p)`。
- 当前用户：`rbac.PrincipalFrom(c)`；环境范围 `p.CanEnv(env)`。
- 敏感字段：`d.Crypto` 加密后入库，接口只返回 `crypto.Mask` 后的值。
- 审计：写请求自动记录；用 `audit.SetResource(c, "cluster", id)`、`audit.SetDetail(c, v)` 补充。
- 生产审批：写操作前调用

```go
t, pending, err := d.Approval.Gate(ctx, p, approval.CreateInput{Type: "cluster.scale", Title: "...", Env: env, Payload: req})
if err != nil { httpx.Fail(c, err); return }
if pending { approval.RespondPending(c, t); return }   // HTTP 202，审批通过后由 TypeSpec.Handler 执行
```

  并在模块初始化时 `d.Approval.Register(approval.TypeSpec{Type: "cluster.scale", Module: "cluster", Handler: ...})`。
- AI 工具：`d.AITools.Register(ai.Tool{Name: "k8s_list_pods", Permission: "workload:read", Parameters: schema, Handler: fn})`；任何改变状态的工具设置 `RequiresConfirmation: true`，生产环境仍需在 Handler 内走 `Gate`。
- Webhook / Agent 注册等无登录接口挂到 `d.Public`，必须自行校验调用方（签名、Token）。

## 测试

- `make test`（`go test ./...`）；纯逻辑写单元测试，外部系统（K8s、Prometheus、Loki）用 httptest 假服务或 client-go fake clientset。
- 需要数据库的集成测试读取 `MATRIX_TEST_DSN`，未设置时跳过。
