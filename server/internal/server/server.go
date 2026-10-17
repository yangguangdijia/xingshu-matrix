// Package server assembles the HTTP engine: global middleware, health and
// metrics endpoints, core routes, module routes and the SPA fallback.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/ai"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/auth"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/metrics"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/settings"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/users"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/version"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/web"
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
	d.Public = public

	// Authenticated routes.
	authed := api.Group("")
	authed.Use(c.Auth.Middleware(), auth.PasswordChangeGate())
	c.Auth.RegisterRoutes(authed)
	c.RBAC.RegisterRoutes(authed)
	c.Users.RegisterRoutes(authed)
	c.Settings.RegisterRoutes(authed)
	c.AuditDB.RegisterRoutes(authed)
	c.Approval.RegisterRoutes(authed)
	c.AI.RegisterRoutes(authed)
	for _, m := range mods {
		m.Register(authed.Group(""), d)
	}

	e.NoRoute(web.NoRoute(d.Cfg.Server.StaticDir))
	return e
}
