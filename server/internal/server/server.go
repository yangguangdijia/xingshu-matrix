// Package server assembles the HTTP engine: global middleware, health and
// metrics endpoints, core routes, module routes and the SPA fallback.
package server

import (
	"context"
	"encoding/json"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/ldapauth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/recovery"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/sso"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/jobs"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/mcp"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/metrics"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/platform"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/users"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/web"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/workspace"
)

// Core bundles the core services mounted by the router.
type Core struct {
	Auth     *auth.Service
	Limiter  *auth.RateLimiter
	RBAC     *rbac.Service
	Users    *users.Service
	Settings *settings.Service
	AuditDB  *audit.PgStore
	Audit    *audit.Recorder
	Approval *approval.Service
	AI       *ai.Service
	Metrics  *metrics.Metrics
	// Backup is the platform backup runner; nil disables /platform/backups.
	Backup    *backup.Runner
	Jobs      *jobs.PgStore
	JobWorker *jobs.Worker
	// Workspace is the 企业空间 store; nil disables /workspaces and /role-bindings.
	Workspace *workspace.PgStore
}

// New builds the gin engine. mods are mounted on the authenticated group.
func New(d *app.Deps, c Core, mods []app.Module) *gin.Engine {
	if d.Cfg.Server.Mode != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}
	e := gin.New()
	if len(d.Cfg.Server.TrustedProxies) > 0 {
		_ = e.SetTrustedProxies(d.Cfg.Server.TrustedProxies)
	}
	e.Use(httpx.RequestID(), httpx.Recover(d.Logger), httpx.CORS(d.Cfg.Server.CORSOrigins), httpx.AccessLog(d.Logger), c.Metrics.Middleware())

	e.GET("/healthz", func(ctx *gin.Context) { ctx.String(http.StatusOK, "ok") })
	e.GET("/metrics", c.Metrics.Handler())

	api := e.Group("/api/v1")
	api.Use(c.Audit.Middleware())
	api.GET("/health", func(ctx *gin.Context) {
		audit.Skip(ctx)
		cctx, cancel := context.WithTimeout(ctx.Request.Context(), 2*time.Second)
		defer cancel()
		status, code := "ok", http.StatusOK
		dbStatus := "ok"
		if err := d.DB.Ping(cctx); err != nil {
			dbStatus, status, code = err.Error(), "degraded", http.StatusServiceUnavailable
		}
		redisStatus := "disabled"
		if d.Redis != nil {
			redisStatus = "ok"
			if err := d.Redis.Ping(cctx).Err(); err != nil {
				redisStatus, status, code = err.Error(), "degraded", http.StatusServiceUnavailable
			}
		}
		ctx.JSON(code, gin.H{"code": 0, "message": status, "data": gin.H{"status": status, "database": dbStatus, "redis": redisStatus}})
	})
	api.GET("/version", func(ctx *gin.Context) { httpx.OK(ctx, version.Get()) })

	// Public (unauthenticated) routes.
	public := api.Group("")
	c.Auth.RegisterPublic(public, c.Limiter)
	c.Settings.RegisterPublic(public)
	d.Public = public

	// Authenticated routes.
	authed := api.Group("")
	authed.Use(c.Auth.Middleware(), auth.PasswordChangeGate())
	c.Auth.RegisterRoutes(authed)
	c.RBAC.RegisterRoutes(authed)
	c.Users.RegisterRoutes(authed)
	c.Settings.RegisterRoutes(authed)
	platform.Register(authed, c.Settings)
	(&sso.Service{DB: d.DB, Settings: c.Settings, Auth: c.Auth}).Register(public, authed)
	ldapSvc := &ldapauth.Service{DB: d.DB, Settings: c.Settings}
	c.Auth.SetExternal(ldapSvc)
	ldapSvc.Register(authed)
	(&recovery.Service{DB: d.DB, Settings: c.Settings, Auth: c.Auth, Limiter: c.Limiter}).Register(public, authed)
	c.AuditDB.RegisterRoutes(authed)
	c.Approval.RegisterRoutes(authed)
	c.AI.RegisterRoutes(authed)

	// MCP（Streamable HTTP）：挂在 engine 上、不经 auth.Middleware，自行只接受 API Token。
	// 用 Any 让 GET 等方法由 mcp.Server 返回 405（gin 默认未注册方法会 404）。
	mcpSrv := mcp.NewWithOptions(c.AI.Tools, func(ctx context.Context, tok, ip string) (*rbac.Principal, error) {
		if !auth.IsAPIToken(tok) {
			return nil, httpx.Unauthorized("MCP 仅接受 API Token")
		}
		p, err := c.Auth.AuthenticateAPIToken(ctx, tok, ip)
		if err != nil {
			return nil, err
		}
		if p.MustChangePassword {
			return nil, httpx.Unauthorized("需要先修改密码")
		}
		return p, nil
	}, mcp.Options{
		Version: version.Version,
		Pending: func(ctx context.Context, p *rbac.Principal, tool string, args json.RawMessage) (string, error) {
			pc := &ai.PendingCall{ID: uuid.NewString(), UserID: p.UserID, Tool: tool, ToolCallID: "mcp-" + uuid.NewString(), Arguments: args}
			if err := c.AI.Store.InsertPendingCall(ctx, pc); err != nil {
				return "", err
			}
			return "/api/v1/ai/tool-calls/" + pc.ID + "/confirm", nil
		},
		Audit: func(ctx context.Context, r mcp.AuditRecord) {
			uid := r.UserID
			res := audit.ResultSuccess
			if r.Outcome != "ok" && r.Outcome != "pending_confirmation" {
				res = audit.ResultFailure
			}
			detail, _ := json.Marshal(map[string]any{"tool": r.Tool, "args": r.Args, "token_id": r.TokenID, "outcome": r.Outcome, "error": r.Error})
			c.Audit.Record(ctx, audit.Entry{UserID: &uid, Username: r.Username, IP: r.IP, Method: http.MethodPost, Path: "/mcp",
				Resource: "mcp_tool", ResourceID: r.Tool, Action: "mcp.tools.call", StatusCode: http.StatusOK, Result: res,
				DurationMs: int(r.Duration.Milliseconds()), UserAgent: r.UserAgent, Detail: detail, CreatedAt: time.Now()})
		},
	})
	e.Any("/mcp", gin.WrapH(mcpSrv))
	if c.Backup != nil {
		backup.RegisterRoutes(authed, backup.APIDeps{Runner: c.Backup, Audit: c.Audit})
	}
	if c.Jobs != nil {
		jobs.RegisterRoutes(authed, jobs.APIDeps{Store: c.Jobs, Worker: c.JobWorker})
	}
	if c.Workspace != nil {
		workspace.RegisterRoutes(authed, workspace.Deps{Store: c.Workspace, Scopes: c.RBAC})
	}
	for _, m := range mods {
		m.Register(authed.Group(""), d)
	}

	e.NoRoute(web.NoRoute(d.Cfg.Server.StaticDir))
	return e
}
